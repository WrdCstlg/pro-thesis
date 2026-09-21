package control

import (
	"errors"
	"fmt"
	"sync"

	"github.com/WrdCstlg/pro-thesis/internal/recorder"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// ErrPhaseOutOfOrder reports an attempt to move through the lifecycle
// backwards, or to leave a phase that is still nested inside another.
//
// Directive 4.1 says the run "must transition LINEARLY through these
// virtual-clock phases". Linearity is what makes invariant I5 mean anything: an
// oracle that declares itself valid only in ASSERT is relying on ASSERT being a
// well-defined, once-only interval. A machine that could re-enter DRIVE would
// make phase_timings ambiguous and every phase-scoped assertion unsound.
var ErrPhaseOutOfOrder = errors.New("control: lifecycle phases advance in order")

// MarkerSink is where phase markers are appended. *recorder.Appender satisfies
// it.
//
// It takes a pre-encoded line rather than a value because the history log is
// NOT canonical-JSON territory: schema.HistoryEntry carries a
// json.RawMessage value field, which internal/recorder/cjson rejects by design
// (it implements json.Marshaler, and cjson never consults that interface). The
// history's own encoder (schema.HistoryEntry.MarshalLine) is the correct one,
// and routing through it keeps exactly one encoder responsible for the history
// wire format.
type MarkerSink interface {
	AppendLine(line []byte) error
}

// phaseWindow is one recorded lifecycle interval.
type phaseWindow struct {
	phase  schema.Phase
	startR recorder.RTime
	endR   recorder.RTime
	closed bool
	nested bool
}

// PhaseLog is the lifecycle state machine and the phase-marker writer.
//
// # Two frames, and why nothing is stamped in virtual time
//
// Every boundary is recorded as an RTime (a monotonic reading taken from the
// recorder's clock) and converted to virtual time only at the end, by
// Timings. That ordering is forced by the clock's design: the virtual origin IS
// DRIVE start, so during BOOT and SEED virtual time does not exist and
// recorder.Timeline returns ErrNoOrigin rather than a plausible-looking zero.
// Recording RTime throughout means nothing has to be rebased and nothing has to
// pretend.
//
// # Who writes what (PHASE0_BUILD_BRIEF D-D)
//
// The driver writes ONLY operation records, to its own history path. This type
// writes ONLY phase markers, to phases.jsonl. The recorder merges the two by
// timestamp. Two writers appending to one file across the Docker Desktop VM /
// Windows bind-mount boundary produce torn lines, and one torn line makes an
// entire history unparseable: surfacing as a spurious INCONCLUSIVE that reads
// like an environment flake rather than a bug.
type PhaseLog struct {
	clock recorder.Clock
	tl    *recorder.Timeline
	sink  MarkerSink

	mu      sync.Mutex
	windows []phaseWindow
	openIdx int // index of the open primary window, -1 for none
	nestIdx int // index of the open nested window, -1 for none
	maxOrd  int // highest phase ordinal opened so far, -1 for none
	reached []schema.Phase
}

// NewPhaseLog returns a phase log writing markers to sink. sink may be nil, in
// which case transitions are recorded in memory but no marker file is written.
func NewPhaseLog(clock recorder.Clock, tl *recorder.Timeline, sink MarkerSink) (*PhaseLog, error) {
	if clock == nil {
		return nil, errors.New("control: NewPhaseLog needs a clock")
	}
	if tl == nil {
		return nil, errors.New("control: NewPhaseLog needs a timeline")
	}
	return &PhaseLog{clock: clock, tl: tl, sink: sink, openIdx: -1, nestIdx: -1, maxOrd: -1}, nil
}

// Advance closes the open primary phase and opens p, at ONE clock reading.
//
// Using a single reading for both is what makes the windows exactly contiguous:
// directive 4.5's own example has DRIVE end at 42000 and HEAL begin at 42000.
// Two separate readings would leave a sub-millisecond hole that an oracle
// mapping a history record onto a phase could fall into.
//
// Skipping forward is legal and is the normal failure path: a world that dies in
// SEED goes BOOT -> SEED -> HEAL -> TEARDOWN, because HEAL and TEARDOWN run on
// every path. Going backwards is not legal.
func (l *PhaseLog) Advance(p schema.Phase) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if !p.Valid() {
		return fmt.Errorf("control: %q is not a lifecycle phase (want one of %v)", p, schema.AllPhases)
	}
	if l.nestIdx >= 0 {
		return fmt.Errorf("control: cannot advance to %s while %s is still open: %w",
			p, l.windows[l.nestIdx].phase, ErrPhaseOutOfOrder)
	}
	ord := p.Ordinal()
	if ord <= l.maxOrd {
		return fmt.Errorf("control: %s does not follow %s: %w", p, schema.AllPhases[l.maxOrd], ErrPhaseOutOfOrder)
	}

	now := l.clock.NowR()
	if p == schema.PhaseDrive {
		// DRIVE start IS the virtual clock origin, so the origin is stamped
		// first and then read back, and the phase boundary uses the origin
		// itself. Taking a fresh reading here instead would put DRIVE's start a
		// few hundred nanoseconds before or after t=0; before is worse, because
		// VTime.MS floors toward negative infinity and DRIVE would report
		// start_ms = -1, which schema.PhaseTimings.Validate rejects outright.
		if err := l.tl.StartDrive(l.clock); err != nil {
			return fmt.Errorf("control: stamp DRIVE origin: %w", err)
		}
		o, err := l.tl.DriveOriginRT()
		if err != nil {
			return fmt.Errorf("control: read DRIVE origin: %w", err)
		}
		now = o
	}

	if l.openIdx >= 0 {
		l.windows[l.openIdx].endR = now
		l.windows[l.openIdx].closed = true
	}
	l.windows = append(l.windows, phaseWindow{phase: p, startR: now})
	l.openIdx = len(l.windows) - 1
	l.maxOrd = ord
	l.reached = append(l.reached, p)
	return l.mark(p, now)
}

// Nest opens p INSIDE the currently open primary phase, leaving that phase
// open.
//
// PERTURB is the only phase this applies to in v1: directive 4.1 step 4 says it
// "overlaps DRIVE". Callers must not assume phase_timings partitions the
// timeline.
func (l *PhaseLog) Nest(p schema.Phase) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if !p.Valid() {
		return fmt.Errorf("control: %q is not a lifecycle phase (want one of %v)", p, schema.AllPhases)
	}
	if l.openIdx < 0 {
		return fmt.Errorf("control: %s must nest inside a phase, but none is open: %w", p, ErrPhaseOutOfOrder)
	}
	if l.nestIdx >= 0 {
		return fmt.Errorf("control: %s cannot nest inside %s, which is itself nested: %w",
			p, l.windows[l.nestIdx].phase, ErrPhaseOutOfOrder)
	}
	ord := p.Ordinal()
	if ord <= l.maxOrd {
		return fmt.Errorf("control: %s does not follow %s: %w", p, schema.AllPhases[l.maxOrd], ErrPhaseOutOfOrder)
	}

	now := l.clock.NowR()
	l.windows = append(l.windows, phaseWindow{phase: p, startR: now, nested: true})
	l.nestIdx = len(l.windows) - 1
	l.maxOrd = ord
	l.reached = append(l.reached, p)
	return l.mark(p, now)
}

// Unnest closes the open nested phase. It is idempotent and returns nil when
// nothing is nested, so a cleanup path can call it blind before advancing to
// HEAL: which is exactly what happens when a world is cancelled mid-PERTURB.
func (l *PhaseLog) Unnest() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.unnestLocked()
	return nil
}

func (l *PhaseLog) unnestLocked() {
	if l.nestIdx < 0 {
		return
	}
	now := l.clock.NowR()
	l.windows[l.nestIdx].endR = now
	l.windows[l.nestIdx].closed = true
	l.nestIdx = -1
}

// Finish closes every open window. Call it once the lifecycle is over, before
// the world file is written.
func (l *PhaseLog) Finish() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.unnestLocked()
	if l.openIdx >= 0 {
		l.windows[l.openIdx].endR = l.clock.NowR()
		l.windows[l.openIdx].closed = true
		l.openIdx = -1
	}
}

// Reached returns the phases entered so far, in the order they were entered.
func (l *PhaseLog) Reached() []schema.Phase {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]schema.Phase(nil), l.reached...)
}

// Entered reports whether p was entered.
func (l *PhaseLog) Entered(p schema.Phase) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, q := range l.reached {
		if q == p {
			return true
		}
	}
	return false
}

// Current returns the innermost open phase, or "" when none is open.
func (l *PhaseLog) Current() schema.Phase {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.nestIdx >= 0 {
		return l.windows[l.nestIdx].phase
	}
	if l.openIdx >= 0 {
		return l.windows[l.openIdx].phase
	}
	return ""
}

// Timings converts the recorded windows into schema.PhaseTimings:
// milliseconds relative to DRIVE start.
//
// It returns recorder.ErrNoOrigin when DRIVE was never entered. That is the
// honest answer, not a failure to handle: a world that died in BOOT has no
// virtual clock, so it has no phase timings, and manufacturing a zero origin
// would place every BOOT event inside DRIVE.
//
// A window that is still open is reported as ending NOW. That is how ASSERT
// looks to the oracle engine, which runs inside it; the world file written at
// TEARDOWN carries the closed windows.
func (l *PhaseLog) Timings() (schema.PhaseTimings, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if !l.tl.Started() {
		return schema.PhaseTimings{}, recorder.ErrNoOrigin
	}
	now := l.clock.NowR()
	out := make(schema.PhaseTimings, 0, len(l.windows))
	for _, w := range l.windows {
		end := w.endR
		if !w.closed {
			end = now
		}
		sv, err := l.tl.V(w.startR)
		if err != nil {
			return nil, err
		}
		ev, err := l.tl.V(end)
		if err != nil {
			return nil, err
		}
		out = append(out, schema.PhaseWindow{Phase: w.phase, StartMS: sv.MS(), EndMS: ev.MS()})
	}
	return out.Normalize(), nil
}

// TimingsOrEmpty is Timings with the no-origin case folded into an empty list,
// for the callers that must produce a document either way.
func (l *PhaseLog) TimingsOrEmpty() schema.PhaseTimings {
	t, err := l.Timings()
	if err != nil {
		return schema.PhaseTimings{}
	}
	return t
}

// mark appends the normative phase marker (directive 4.4):
//
//	{"t_ns":...,"type":"info","event":"phase","phase":"HEAL"}
//
// The caller holds l.mu.
func (l *PhaseLog) mark(p schema.Phase, r recorder.RTime) error {
	if l.sink == nil {
		return nil
	}
	e := schema.PhaseMarker(l.wallAt(r), p)
	if err := e.Validate(); err != nil {
		return fmt.Errorf("control: phase marker for %s: %w", p, err)
	}
	line, err := e.MarshalLine()
	if err != nil {
		return fmt.Errorf("control: encode phase marker for %s: %w", p, err)
	}
	if err := l.sink.AppendLine(line); err != nil {
		return fmt.Errorf("control: write phase marker for %s: %w", p, err)
	}
	return nil
}

// wallAt converts a monotonic reading to Unix epoch nanoseconds: the normative
// t_ns frame (PHASE0_BUILD_BRIEF D-C).
//
// Once the origin exists the conversion is derived from the recorded wall
// anchor, so a marker's t_ns and its virtual offset can never disagree. Before
// the origin exists there is no anchor, so the clock's own derived wall reading
// is used; it is derived from the same monotonic base, so it still cannot step
// backwards.
func (l *PhaseLog) wallAt(r recorder.RTime) int64 {
	originWall, err := l.tl.EpochNSFromVMS(0)
	if err != nil {
		return l.clock.NowWall().UnixNano()
	}
	v, err := l.tl.V(r)
	if err != nil {
		return l.clock.NowWall().UnixNano()
	}
	return originWall + int64(v)
}
