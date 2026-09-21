package recorder

import (
	"context"
	"errors"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// The origin does not exist before DRIVE
// ---------------------------------------------------------------------------

// TestVirtualTimeDoesNotExistBeforeDrive is the typed-impossibility check.
//
// The virtual clock's origin is DRIVE start. If a query before that returned a
// number, it would return the raw monotonic reading: a value indistinguishable
// from a real virtual timestamp and wrong by however long BOOT took. `thesis up`
// and `thesis down` have no DRIVE phase at all, so this is their permanent
// state, not an edge case.
func TestVirtualTimeDoesNotExistBeforeDrive(t *testing.T) {
	var tl Timeline

	if tl.Started() {
		t.Fatal("a fresh Timeline reports a started origin")
	}
	for name, call := range map[string]func() error{
		"V":                 func() error { _, err := tl.V(12345); return err },
		"R":                 func() error { _, err := tl.R(12345); return err },
		"DriveOriginWallNS": func() error { _, err := tl.DriveOriginWallNS(); return err },
		"DriveOriginRT":     func() error { _, err := tl.DriveOriginRT(); return err },
		"VMSFromEpochNS":    func() error { _, err := tl.VMSFromEpochNS(1); return err },
		"EpochNSFromVMS":    func() error { _, err := tl.EpochNSFromVMS(1); return err },
	} {
		if err := call(); !errors.Is(err, ErrNoOrigin) {
			t.Errorf("%s before DRIVE returned %v, want ErrNoOrigin", name, err)
		}
	}

	c := NewSimClock(time.Unix(1700000000, 0).UTC())
	c.Advance(3 * time.Second)
	if err := tl.StartDrive(c); err != nil {
		t.Fatal(err)
	}
	v, err := tl.V(c.NowR())
	if err != nil {
		t.Fatal(err)
	}
	if v != 0 {
		t.Fatalf("virtual time at the origin is %d, want 0", v)
	}
}

// TestTimelineOriginIsImmutable: rebasing the origin would retroactively move
// every fault window in the schedule.
func TestTimelineOriginIsImmutable(t *testing.T) {
	var tl Timeline
	if err := tl.StartDriveAt(100, 1700000000000000000); err != nil {
		t.Fatal(err)
	}
	if err := tl.StartDriveAt(200, 1700000001000000000); !errors.Is(err, ErrOriginSet) {
		t.Fatalf("second StartDrive returned %v, want ErrOriginSet", err)
	}
	if rt, _ := tl.DriveOriginRT(); rt != 100 {
		t.Fatalf("origin moved to %d", rt)
	}
}

// TestVirtualTimeIsNegativeBeforeTheOrigin: BOOT and SEED precede DRIVE, so
// their virtual bounds are negative. Truncating toward zero would place a point
// half a millisecond before DRIVE inside DRIVE.
func TestVirtualTimeIsNegativeBeforeTheOrigin(t *testing.T) {
	var tl Timeline
	origin := RTime(42 * int64(time.Second))
	if err := tl.StartDriveAt(origin, 0); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		at   RTime
		want int64
	}{
		{origin, 0},
		{origin + RTime(1500*int64(time.Millisecond)), 1500},
		{origin - RTime(500*int64(time.Microsecond)), -1},
		{origin - RTime(1500*int64(time.Millisecond)), -1500},
		{0, -42000},
	}
	for _, tc := range cases {
		v, err := tl.V(tc.at)
		if err != nil {
			t.Fatal(err)
		}
		if got := v.MS(); got != tc.want {
			t.Errorf("V(%d).MS() = %d, want %d", tc.at, got, tc.want)
		}
	}
}

// TestEpochAnchorConversion checks the anchor described in the build brief's
// D-C: the driver is an external process emitting absolute Unix-epoch
// nanoseconds, and this is the only thing that turns those into the virtual
// milliseconds prothesis.oracle_input/v1's phases array uses.
func TestEpochAnchorConversion(t *testing.T) {
	var tl Timeline
	const origin = int64(1788451200000000000) // ~2026-09-03, well above 2^53
	if err := tl.StartDriveAt(1000, origin); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		tNS  int64
		want int64
	}{
		{origin, 0},
		{origin + 410*int64(time.Millisecond), 410},
		{origin + 42_000*int64(time.Millisecond), 42000},
		{origin - 1, -1}, // a nanosecond before DRIVE
		{origin - 500*int64(time.Microsecond), -1}, // half a millisecond before
		{origin - 30_000*int64(time.Millisecond), -30000},
	}
	for _, tc := range cases {
		got, err := tl.VMSFromEpochNS(tc.tNS)
		if err != nil {
			t.Fatal(err)
		}
		if got != tc.want {
			t.Errorf("VMSFromEpochNS(%d) = %d, want %d", tc.tNS, got, tc.want)
		}
	}

	// The anchor must survive a value larger than 2^53 without loss: absolute
	// Unix nanoseconds are ~1.8e18 and a float64 round trip would corrupt them.
	if got, _ := tl.DriveOriginWallNS(); got != origin {
		t.Fatalf("the anchor lost precision: %d != %d", got, origin)
	}
	back, err := tl.EpochNSFromVMS(42000)
	if err != nil {
		t.Fatal(err)
	}
	if back != origin+42_000*int64(time.Millisecond) {
		t.Fatalf("EpochNSFromVMS round trip: got %d", back)
	}
}

// ---------------------------------------------------------------------------
// Drift
// ---------------------------------------------------------------------------

// TestDriftMonitorIgnoresSlewAndCatchesSteps is the regression test for a
// specific defect: computing wall-vs-monotonic divergence CUMULATIVELY rather
// than per interval.
//
// A host slewing at a routine 500 ppm accumulates over fourteen seconds across
// an eight-hour soak without a single discontinuity ever occurring. Under a
// cumulative rule that run is marked INCONCLUSIVE and eight hours of search is
// thrown away because the NTP daemon was doing its job.
func TestDriftMonitorIgnoresSlewAndCatchesSteps(t *testing.T) {
	base := time.Unix(1700000000, 0).UTC()
	d := newDriftMonitor(DefaultDriftStepThreshold, base, 0)

	const (
		interval = 5 * time.Second
		samples  = 8 * 60 * 60 / 5 // an eight-hour soak
		ppm      = 500
	)
	wall := base
	var r RTime
	for i := 0; i < samples; i++ {
		r += RTime(interval)
		// The wall clock runs 500 ppm fast: no step, just slew.
		wall = wall.Add(interval + interval*ppm/1_000_000)
		if _, anomaly := d.sample(wall, r); anomaly {
			t.Fatalf("sample %d: routine slew was reported as a clock anomaly", i)
		}
	}
	if d.anomalies != 0 {
		t.Fatalf("%d anomalies raised by pure slew", d.anomalies)
	}
	if d.cumulativeSlew() < 10*time.Second {
		t.Fatalf("the test did not actually accumulate slew: %v", d.cumulativeSlew())
	}

	// Now a genuine step: NTP correction or a VM resume.
	r += RTime(interval)
	wall = wall.Add(interval + 3*time.Second)
	step, anomaly := d.sample(wall, r)
	if !anomaly {
		t.Fatalf("a 3 s wall-clock step was not reported (step measured as %v)", step)
	}
	if d.anomalies != 1 {
		t.Fatalf("anomalies = %d, want 1", d.anomalies)
	}

	// And the run recovers: the interval after the step is normal again.
	r += RTime(interval)
	wall = wall.Add(interval)
	if _, anomaly := d.sample(wall, r); anomaly {
		t.Fatal("the interval after a step was itself reported as a step")
	}
}

func TestDriftMonitorCatchesBackwardsSteps(t *testing.T) {
	base := time.Unix(1700000000, 0).UTC()
	d := newDriftMonitor(DefaultDriftStepThreshold, base, 0)
	d.sample(base, 0)
	// The wall clock jumps backwards two seconds while monotonic time advances.
	if _, anomaly := d.sample(base.Add(3*time.Second), RTime(5*time.Second)); !anomaly {
		t.Fatal("a backwards wall step was not reported")
	}
}

// ---------------------------------------------------------------------------
// Clocks
// ---------------------------------------------------------------------------

func TestRealClockIsMonotonicAndWallIsDerived(t *testing.T) {
	c := NewRealClock(RealClockOptions{SkipCalibration: true, DriftInterval: -1})
	defer c.Close()

	prevR := c.NowR()
	prevW := c.NowWall()
	for i := 0; i < 20000; i++ {
		r := c.NowR()
		w := c.NowWall()
		if r < prevR {
			t.Fatalf("NowR went backwards: %d -> %d", prevR, r)
		}
		if w.Before(prevW) {
			t.Fatalf("NowWall went backwards: %v -> %v", prevW, w)
		}
		prevR, prevW = r, w
	}

	// NowWall must be base + NowR, not a fresh sample.
	h := c.Health()
	r := c.NowR()
	w := c.NowWall()
	drift := w.UnixNano() - (h.Anchor.BaseWallUnixNS + int64(r))
	if drift < -int64(time.Millisecond) || drift > int64(time.Millisecond) {
		t.Fatalf("NowWall is not derived from NowR: off by %d ns", drift)
	}
	if h.Anchor.Kind != "real" {
		t.Fatalf("anchor kind is %q", h.Anchor.Kind)
	}
}

func TestRealClockCalibrationIsRecordedNotApplied(t *testing.T) {
	c := NewRealClock(RealClockOptions{DriftInterval: -1})
	defer c.Close()
	h := c.Health()
	if h.Anchor.SleepGranularityNS <= 0 {
		t.Fatalf("sleep granularity was not measured: %d", h.Anchor.SleepGranularityNS)
	}
	// The measurement is clamped into a sane band so a pathological host cannot
	// turn the spin tail into a busy-wait of arbitrary length.
	if h.Anchor.SleepGranularityNS > int64(4*time.Millisecond) {
		t.Fatalf("sleep granularity %d ns exceeds the clamp", h.Anchor.SleepGranularityNS)
	}
}

func TestRealClockSleepUntilR(t *testing.T) {
	c := NewRealClock(RealClockOptions{DriftInterval: -1})
	defer c.Close()

	target := c.NowR() + RTime(20*time.Millisecond)
	woke, err := c.SleepUntilR(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	if woke < target {
		t.Fatalf("woke %d ns early", int64(target-woke))
	}

	// A deadline already in the past returns immediately.
	if _, err := c.SleepUntilR(context.Background(), 0); err != nil {
		t.Fatalf("a past deadline returned %v", err)
	}

	// Cancellation is honoured rather than slept through.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.SleepUntilR(ctx, c.NowR()+RTime(time.Hour)); !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled context returned %v, want context.Canceled", err)
	}
}

func TestRealClockCloseIsIdempotentAndReleasesSleepers(t *testing.T) {
	c := NewRealClock(RealClockOptions{SkipCalibration: true, DriftInterval: 5 * time.Millisecond})

	// A sleeper waiting on a long deadline must be released by Close rather
	// than outliving the run that created the clock.
	done := make(chan error, 1)
	go func() {
		_, err := c.SleepUntilR(context.Background(), c.NowR()+RTime(time.Hour))
		done <- err
	}()

	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, ErrClockClosed) {
			t.Fatalf("the sleeper returned %v, want ErrClockClosed", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not release a sleeper")
	}

	if err := c.Close(); err != nil {
		t.Fatalf("second Close returned %v", err)
	}
	if _, err := c.SleepUntilR(context.Background(), c.NowR()+RTime(time.Hour)); !errors.Is(err, ErrClockClosed) {
		t.Fatalf("sleeping on a closed clock returned %v, want ErrClockClosed", err)
	}
}

func TestSimClockSleepAfterCloseFails(t *testing.T) {
	c := NewSimClock(time.Unix(0, 0))
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.SleepUntilR(context.Background(), RTime(time.Second)); !errors.Is(err, ErrClockClosed) {
		t.Fatalf("got %v, want ErrClockClosed", err)
	}
}

func TestSimClockIsDeterministic(t *testing.T) {
	base := time.Unix(1700000000, 0).UTC()
	c := NewSimClock(base)
	if c.NowR() != 0 {
		t.Fatalf("a fresh SimClock reads %d", c.NowR())
	}
	c.Advance(1500 * time.Millisecond)
	if got := c.NowR(); got != RTime(1500*time.Millisecond) {
		t.Fatalf("NowR = %d after advancing 1.5 s", got)
	}
	if got := c.NowWall(); !got.Equal(base.Add(1500 * time.Millisecond)) {
		t.Fatalf("NowWall = %v", got)
	}

	woke, err := c.SleepUntilR(context.Background(), RTime(9*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if woke != RTime(9*time.Second) || c.NowR() != RTime(9*time.Second) {
		t.Fatalf("SleepUntilR did not advance the clock: woke %d, now %d", woke, c.NowR())
	}
	// Sleeping to a past deadline must not move time backwards.
	if _, err := c.SleepUntilR(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	if c.NowR() != RTime(9*time.Second) {
		t.Fatalf("SleepUntilR moved the clock backwards to %d", c.NowR())
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestSimClockRejectsNegativeAdvance(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("Advance with a negative duration must panic")
		}
	}()
	NewSimClock(time.Unix(0, 0)).Advance(-time.Second)
}

func TestFloorDiv(t *testing.T) {
	cases := []struct{ a, b, want int64 }{
		{7, 2, 3}, {-7, 2, -4}, {0, 2, 0}, {-1, 1000000, -1}, {1999999, 1000000, 1},
	}
	for _, tc := range cases {
		if got := floorDiv(tc.a, tc.b); got != tc.want {
			t.Errorf("floorDiv(%d, %d) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}
