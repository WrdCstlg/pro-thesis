package recorder

import (
	"context"
	"errors"
	"runtime"
	"sort"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Time frames
// ---------------------------------------------------------------------------

// RTime is nanoseconds since Clock construction. It is a MONOTONIC reading:
// unaffected by NTP steps, manual clock changes, DST, or Hyper-V save/restore.
// Everything the recorder stamps is stamped in RTime, so nothing ever needs
// rebasing when the virtual-clock origin is finally established.
type RTime int64

// Duration converts an RTime interval to a time.Duration.
func (r RTime) Duration() time.Duration { return time.Duration(r) }

// VTime is nanoseconds relative to the DRIVE origin (t=0). This is the frame
// the fault grammar ("@8200..15100") and prothesis.oracle_input/v1's `phases`
// array both use: directive 4.3 says fault windows are "relative to DRIVE start
// on virtual clock" and 4.5 shows `{"phase":"DRIVE","start_ms":0}`.
//
// VTime is NEGATIVE for anything that happened during BOOT or SEED. It does not
// exist at all until DRIVE starts: see Timeline.
type VTime int64

// MS converts to milliseconds, rounding toward negative infinity so that a
// point 0.5 ms before DRIVE start reports -1 rather than 0. Truncation toward
// zero would place pre-DRIVE events inside DRIVE, which is exactly the class of
// off-by-one that makes I5 phase-aware assertion unsound.
func (v VTime) MS() int64 { return floorDiv(int64(v), int64(time.Millisecond)) }

// VMS constructs a VTime from milliseconds.
func VMS(ms int64) VTime { return VTime(ms * int64(time.Millisecond)) }

func floorDiv(a, b int64) int64 {
	q := a / b
	if (a%b != 0) && ((a < 0) != (b < 0)) {
		q--
	}
	return q
}

// ---------------------------------------------------------------------------
// Timeline: the one conversion constant, and the anchor
// ---------------------------------------------------------------------------

// ErrNoOrigin is returned by every Timeline query made before DRIVE has
// started.
//
// The virtual clock's origin is DRIVE start. During BOOT and SEED it does not
// exist. Returning a plausible-looking number in that window (which is what a
// zero origin would do: V(r) would silently equal r) is worse than returning an
// error, because a raw monotonic reading masquerading as virtual time is
// indistinguishable from a real one downstream. `thesis up` and `thesis down`
// have no DRIVE phase at all, so this is the normal state for them, not an edge
// case.
var ErrNoOrigin = errors.New("recorder: virtual time does not exist before DRIVE start " +
	"(call Timeline.StartDrive first; BOOT/SEED events carry rt_ns only)")

// ErrOriginSet is returned when StartDrive is called a second time. The origin
// is immutable: rebasing it would retroactively move every fault window.
var ErrOriginSet = errors.New("recorder: DRIVE origin is already set and is immutable")

// ErrClockClosed is returned by SleepUntilR when the clock has been closed. A
// long-lived process runs many runs; a sleeper left blocking on a retired
// clock would outlive the run that created it.
var ErrClockClosed = errors.New("recorder: clock is closed")

// Timeline holds the single RTime→VTime conversion constant and the single wall
// anchor.
//
// drive_origin_wall_ns is captured EXACTLY ONCE, at DRIVE start, and is derived
// from Clock.NowWall rather than a fresh time.Now: see NewRealClock. It is the
// anchor that converts the external driver's absolute Unix-epoch t_ns
// (PHASE0_BUILD_BRIEF D-C) into the start_ms/end_ms frame of
// prothesis.oracle_input/v1. Without it, I5 phase-aware assertion is not
// evaluable at all: an oracle cannot decide whether op 8891 fell inside DRIVE
// or HEAL.
type Timeline struct {
	mu           sync.RWMutex
	started      bool
	originRT     RTime
	originWallNS int64
}

// StartDrive stamps the origin from c. It may be called once.
//
// The wall instant is taken from c.NowWall(), which is DERIVED from the
// monotonic reading, so the anchor can never disagree with any other timestamp
// the recorder produces.
func (t *Timeline) StartDrive(c Clock) error {
	if c == nil {
		return errors.New("recorder: StartDrive needs a Clock")
	}
	return t.StartDriveAt(c.NowR(), c.NowWall().UnixNano())
}

// StartDriveAt stamps the origin explicitly. Used by tests and by replay, where
// the origin is reconstructed from a recorded run rather than observed.
func (t *Timeline) StartDriveAt(rt RTime, wallUnixNS int64) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.started {
		return ErrOriginSet
	}
	t.started = true
	t.originRT = rt
	t.originWallNS = wallUnixNS
	return nil
}

// Started reports whether DRIVE has begun.
func (t *Timeline) Started() bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.started
}

// V converts a monotonic reading to virtual time.
func (t *Timeline) V(r RTime) (VTime, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if !t.started {
		return 0, ErrNoOrigin
	}
	return VTime(r - t.originRT), nil
}

// R converts virtual time back to a monotonic reading.
func (t *Timeline) R(v VTime) (RTime, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if !t.started {
		return 0, ErrNoOrigin
	}
	return RTime(int64(v) + int64(t.originRT)), nil
}

// DriveOriginWallNS returns the wall anchor: Unix epoch nanoseconds at DRIVE
// start. This is the `drive_origin_wall_ns` of PHASE0_BUILD_BRIEF D-C.
func (t *Timeline) DriveOriginWallNS() (int64, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if !t.started {
		return 0, ErrNoOrigin
	}
	return t.originWallNS, nil
}

// DriveOriginRT returns the monotonic reading at DRIVE start.
func (t *Timeline) DriveOriginRT() (RTime, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if !t.started {
		return 0, ErrNoOrigin
	}
	return t.originRT, nil
}

// VMSFromEpochNS converts an absolute Unix-epoch nanosecond timestamp (the
// normative `t_ns` field of the history log (directive 4.4, read per D-C as
// true nanoseconds)) into the virtual millisecond frame that
// prothesis.oracle_input/v1's `phases` array uses.
//
// This is the whole reason the wall anchor exists. The driver is an external,
// unmodified process; it cannot know the harness's virtual-clock origin, so it
// emits wall time and the recorder does the conversion.
func (t *Timeline) VMSFromEpochNS(tNS int64) (int64, error) {
	origin, err := t.DriveOriginWallNS()
	if err != nil {
		return 0, err
	}
	return floorDiv(tNS-origin, int64(time.Millisecond)), nil
}

// EpochNSFromVMS is the inverse of VMSFromEpochNS, exact at millisecond
// granularity.
func (t *Timeline) EpochNSFromVMS(ms int64) (int64, error) {
	origin, err := t.DriveOriginWallNS()
	if err != nil {
		return 0, err
	}
	return origin + ms*int64(time.Millisecond), nil
}

// ---------------------------------------------------------------------------
// Clock
// ---------------------------------------------------------------------------

// Clock is the recorder's time source.
//
// Two implementations ship in Phase 0: RealClock, anchored to the host
// monotonic clock (QueryPerformanceCounter on Windows), and SimClock, a
// deterministic manual clock for tests and the seam a full simulation backend
// would plug into later. The interface is identical for both, which is what
// keeps that option open.
type Clock interface {
	// NowR is the monotonic reading. It never decreases.
	NowR() RTime

	// NowWall is DERIVED from NowR plus a wall base captured once at
	// construction. It is not a fresh wall read.
	//
	// Deriving it guarantees non-decreasing timestamps in everything the
	// recorder itself writes. A fresh time.Now() can step backwards on NTP
	// correction, manual clock change, or VM save/restore: all realistic on a
	// Windows host running Docker Desktop. True wall divergence is sampled
	// separately and reported by Health.
	NowWall() time.Time

	// SleepUntilR blocks until NowR() >= at, ctx is done, or the clock is
	// closed. It returns the actual wake time, so lateness is always measured
	// rather than assumed.
	SleepUntilR(ctx context.Context, at RTime) (woke RTime, err error)

	// Health reports clock observations: calibration, wall divergence, and any
	// anomalies seen so far.
	Health() ClockHealth

	// Close releases the clock's resources. It is idempotent.
	Close() error
}

// ClockAnchor records how virtual time relates to the outside world. It is an
// artifact record; the monotonic base it derives from is never serialized,
// because Go silently strips the monotonic reading on Round(0), MarshalJSON,
// MarshalText and gob.
type ClockAnchor struct {
	// BaseWallUnixNS is the wall instant at clock construction.
	BaseWallUnixNS int64 `json:"base_wall_unix_ns"`
	// Kind is "real" or "sim".
	Kind string `json:"kind"`
	// SleepGranularityNS is the measured sleep overshoot (see calibrateSleep).
	SleepGranularityNS int64 `json:"sleep_granularity_ns"`
}

// ClockHealth is the observation record. Nothing in it changes how a world
// executes: a host that sleeps coarsely must not silently alter fault windows,
// or the same `.thesis` would behave differently on a fast host and a slow one,
// which would make the world tuple stop determining the run (invariant I2).
type ClockHealth struct {
	Anchor ClockAnchor `json:"anchor"`
	// Anomalies counts observed wall-clock STEPS (see driftMonitor).
	Anomalies int64 `json:"anomalies"`
	// MaxStepNS is the largest single-interval wall-vs-monotonic discrepancy
	// observed, in nanoseconds, signed.
	MaxStepNS int64 `json:"max_step_ns"`
	// CumulativeSlewNS is total wall-vs-monotonic divergence since
	// construction. It is recorded as an OBSERVATION ONLY and must never be
	// used to fail a run: an ordinary 500 ppm NTP slew accumulates 14 seconds
	// over an 8-hour soak without a single discontinuity ever occurring.
	CumulativeSlewNS int64 `json:"cumulative_slew_ns"`
	// Samples is the number of drift samples taken.
	Samples int64 `json:"samples"`
}

// ---------------------------------------------------------------------------
// Drift detection
// ---------------------------------------------------------------------------

// DefaultDriftStepThreshold is the per-interval discrepancy above which a wall
// sample is treated as a clock STEP (NTP step, manual change, VM resume) rather
// than slew.
const DefaultDriftStepThreshold = 500 * time.Millisecond

// driftMonitor detects wall-clock DISCONTINUITIES.
//
// It deliberately compares consecutive INTERVALS, not totals. Comparing
// wall-since-start against mono-since-start measures accumulated slew, which
// grows without bound on a perfectly healthy host and would mark every long run
// INCONCLUSIVE: precisely where a spurious INCONCLUSIVE is most expensive.
type driftMonitor struct {
	threshold time.Duration

	have     bool
	prevWall time.Time
	prevR    RTime

	baseWall time.Time
	baseR    RTime

	samples   int64
	anomalies int64
	maxStepNS int64
	cumSlewNS int64
}

func newDriftMonitor(threshold time.Duration, baseWall time.Time, baseR RTime) *driftMonitor {
	return &driftMonitor{threshold: threshold, baseWall: baseWall, baseR: baseR}
}

// sample records one (wall, monotonic) observation and reports the
// interval-local discrepancy plus whether it crossed the step threshold.
func (d *driftMonitor) sample(wall time.Time, r RTime) (step time.Duration, anomaly bool) {
	d.samples++
	d.cumSlewNS = wall.Sub(d.baseWall).Nanoseconds() - int64(r-d.baseR)
	if !d.have {
		d.have = true
		d.prevWall, d.prevR = wall, r
		return 0, false
	}
	wallDelta := wall.Sub(d.prevWall)
	monoDelta := time.Duration(r - d.prevR)
	step = wallDelta - monoDelta
	d.prevWall, d.prevR = wall, r
	if abs64(step.Nanoseconds()) > abs64(d.maxStepNS) {
		d.maxStepNS = step.Nanoseconds()
	}
	if step > d.threshold || step < -d.threshold {
		d.anomalies++
		return step, true
	}
	return step, false
}

// cumulativeSlew is the total wall-vs-monotonic divergence since construction.
// It is an observation. It must never be compared against a threshold that
// changes a verdict: see the field comment on ClockHealth.CumulativeSlewNS.
func (d *driftMonitor) cumulativeSlew() time.Duration { return time.Duration(d.cumSlewNS) }

func abs64(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}

// ---------------------------------------------------------------------------
// RealClock
// ---------------------------------------------------------------------------

// DefaultDriftInterval is how often RealClock samples the true wall clock.
const DefaultDriftInterval = 5 * time.Second

// RealClock is the host-anchored clock.
type RealClock struct {
	// monoBase CARRIES a monotonic reading. It is private, is never serialized,
	// is never Round(0)'d, and is never handed to encoding/json. Go strips the
	// monotonic reading on any of those, and a monotonic base that has silently
	// degraded into a wall clock is the most common way a Go program loses
	// monotonicity.
	monoBase time.Time
	// wallBase has the monotonic reading deliberately STRIPPED. Artifact use
	// only; NowWall derives from it.
	wallBase time.Time

	spinNS int64

	mu     sync.Mutex
	drift  *driftMonitor
	closed bool
	done   chan struct{}
	wg     sync.WaitGroup
}

// RealClockOptions tunes RealClock. The zero value is the production default.
type RealClockOptions struct {
	// DriftInterval is the wall-sampling period. Zero means
	// DefaultDriftInterval. Negative disables the watcher entirely.
	DriftInterval time.Duration
	// DriftStepThreshold overrides DefaultDriftStepThreshold.
	DriftStepThreshold time.Duration
	// SkipCalibration skips the startup sleep calibration (20 x 1 ms). Tests
	// that do not care about sleep granularity set it to stay fast.
	SkipCalibration bool
}

// NewRealClock constructs a host-anchored clock and starts its drift watcher.
//
// The watcher writes into the clock's own health record and nowhere else. It
// deliberately does not hold a reference to an artifact bundle: the clock is
// constructed before the run directory exists, so an emit target would be nil
// for the first interval with no way to attach one later.
func NewRealClock(opts RealClockOptions) *RealClock {
	now := time.Now()
	c := &RealClock{
		monoBase: now,
		wallBase: now.Round(0).UTC(),
		done:     make(chan struct{}),
	}
	if !opts.SkipCalibration {
		c.spinNS = calibrateSleep()
	}
	th := opts.DriftStepThreshold
	if th <= 0 {
		th = DefaultDriftStepThreshold
	}
	c.drift = newDriftMonitor(th, c.wallBase, 0)

	iv := opts.DriftInterval
	if iv == 0 {
		iv = DefaultDriftInterval
	}
	if iv > 0 {
		c.wg.Add(1)
		go c.watchDrift(iv)
	}
	return c
}

// NowR returns the monotonic reading. time.Since uses the monotonic reading of
// monoBase, so this is a pure QPC subtraction on Windows.
func (c *RealClock) NowR() RTime { return RTime(time.Since(c.monoBase)) }

// NowWall returns wallBase advanced by the monotonic reading. Derived, not
// sampled: see Clock.NowWall.
func (c *RealClock) NowWall() time.Time {
	return c.wallBase.Add(time.Duration(c.NowR()))
}

// SleepUntilR sleeps to within the calibrated spin threshold of the deadline,
// then busy-yields.
//
// Windows' default timer granularity is ~15.6 ms. Go 1.16+ uses
// CREATE_WAITABLE_TIMER_HIGH_RESOLUTION on Win10 1803+ for roughly 1 ms, but
// that is a runtime implementation detail rather than a contract, so sleeping
// the entire remaining duration can overshoot a deadline by a whole tick. The
// spin tail costs at most spinNS of one core per call.
//
// timeBeginPeriod is deliberately NOT called: it is process-global, perturbs
// system-wide power management, and would need golang.org/x/sys/windows.
func (c *RealClock) SleepUntilR(ctx context.Context, at RTime) (RTime, error) {
	spin := c.spinNS
	for {
		now := c.NowR()
		if now >= at {
			return now, ctx.Err()
		}
		if err := ctx.Err(); err != nil {
			return now, err
		}
		select {
		case <-c.done:
			return now, ErrClockClosed
		default:
		}
		d := time.Duration(at - now)
		if d > time.Duration(spin) {
			t := time.NewTimer(d - time.Duration(spin))
			select {
			case <-t.C:
			case <-ctx.Done():
				t.Stop()
				return c.NowR(), ctx.Err()
			case <-c.done:
				t.Stop()
				return c.NowR(), ErrClockClosed
			}
			continue
		}
		for c.NowR() < at {
			if err := ctx.Err(); err != nil {
				return c.NowR(), err
			}
			runtime.Gosched()
		}
		return c.NowR(), nil
	}
}

// Health returns the observation record.
func (c *RealClock) Health() ClockHealth {
	c.mu.Lock()
	defer c.mu.Unlock()
	return ClockHealth{
		Anchor: ClockAnchor{
			BaseWallUnixNS:     c.wallBase.UnixNano(),
			Kind:               "real",
			SleepGranularityNS: c.spinNS,
		},
		Anomalies:        c.drift.anomalies,
		MaxStepNS:        c.drift.maxStepNS,
		CumulativeSlewNS: c.drift.cumSlewNS,
		Samples:          c.drift.samples,
	}
}

// Close stops the drift watcher. Idempotent, so a long-lived process running
// many runs does not leak a goroutine and a ticker per run.
func (c *RealClock) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	close(c.done)
	c.mu.Unlock()
	c.wg.Wait()
	return nil
}

func (c *RealClock) watchDrift(interval time.Duration) {
	defer c.wg.Done()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-c.done:
			return
		case <-t.C:
			wall := time.Now().Round(0).UTC()
			r := c.NowR()
			c.mu.Lock()
			c.drift.sample(wall, r)
			c.mu.Unlock()
		}
	}
}

// calibrateSleep measures this host's sleep overshoot: 20 sleeps of 1 ms, of
// which the median overshoot is taken. The result is recorded in ClockHealth
// and used only to size the spin tail. It NEVER alters how a world executes.
func calibrateSleep() int64 {
	const n = 20
	over := make([]int64, 0, n)
	for i := 0; i < n; i++ {
		start := time.Now()
		time.Sleep(time.Millisecond)
		d := time.Since(start) - time.Millisecond
		if d < 0 {
			d = 0
		}
		over = append(over, int64(d))
	}
	sort.Slice(over, func(i, j int) bool { return over[i] < over[j] })
	med := over[n/2]
	const (
		minSpin = int64(200 * time.Microsecond)
		maxSpin = int64(4 * time.Millisecond)
	)
	if med < minSpin {
		med = minSpin
	}
	if med > maxSpin {
		med = maxSpin
	}
	return med
}

// ---------------------------------------------------------------------------
// SimClock
// ---------------------------------------------------------------------------

// SimClock is a deterministic clock. Time advances only when Advance is called
// or when SleepUntilR jumps to a deadline, so a test's timing is a function of
// the test rather than of the host's scheduling mood.
type SimClock struct {
	mu       sync.Mutex
	now      RTime
	wallBase time.Time
	closed   bool
}

// NewSimClock returns a SimClock whose wall base is wallBase (any instant; it
// is used only to derive NowWall) and whose monotonic reading starts at zero.
func NewSimClock(wallBase time.Time) *SimClock {
	return &SimClock{wallBase: wallBase.Round(0).UTC()}
}

// NowR returns the current simulated monotonic reading.
func (c *SimClock) NowR() RTime {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// NowWall derives wall time from the simulated monotonic reading.
func (c *SimClock) NowWall() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.wallBase.Add(time.Duration(c.now))
}

// Advance moves simulated time forward. Advancing by a negative amount panics:
// virtual time never goes backwards.
func (c *SimClock) Advance(d time.Duration) {
	if d < 0 {
		panic("recorder: SimClock.Advance with a negative duration")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now += RTime(d)
}

// SleepUntilR jumps simulated time to at (never backwards) and returns.
func (c *SimClock) SleepUntilR(ctx context.Context, at RTime) (RTime, error) {
	if err := ctx.Err(); err != nil {
		return c.NowR(), err
	}
	c.mu.Lock()
	if c.closed {
		now := c.now
		c.mu.Unlock()
		return now, ErrClockClosed
	}
	if at > c.now {
		c.now = at
	}
	now := c.now
	c.mu.Unlock()
	return now, nil
}

// Health reports a sim clock's anchor. A simulated clock has no drift by
// construction.
func (c *SimClock) Health() ClockHealth {
	c.mu.Lock()
	defer c.mu.Unlock()
	return ClockHealth{
		Anchor: ClockAnchor{
			BaseWallUnixNS:     c.wallBase.UnixNano(),
			Kind:               "sim",
			SleepGranularityNS: 0,
		},
	}
}

// Close marks the clock closed. It has nothing to release.
func (c *SimClock) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	return nil
}

var (
	_ Clock = (*RealClock)(nil)
	_ Clock = (*SimClock)(nil)
)
