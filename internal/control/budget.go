package control

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/WrdCstlg/pro-thesis/internal/recorder"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// ErrBudgetExhausted is the cancellation CAUSE the runner attaches when the
// wall-clock or world budget expires.
//
// It exists so that budget expiry stays distinguishable from Ctrl-C after the
// context has been cancelled. Both look identical to a cancelled operation
// (context.Canceled, nothing else) and they map to different exit codes (3
// versus 2). Without a cause, every budget expiry on a busy run would be
// reported as an interrupt, or every interrupt as a soft warning. Recover it
// with context.Cause.
var ErrBudgetExhausted = errors.New("control: budget exhausted")

// Budget is invariant I7's run budget: "Every run adheres to a strict
// wall-clock time or world-count budget."
type Budget struct {
	// Wall is the wall-clock budget. Zero or negative means unbounded.
	Wall time.Duration
	// Worlds is the planned world count. schema.WorldsUnbounded (-1) means
	// "run until the wall budget expires". Zero is rejected by the config
	// validator and is treated here as unbounded-by-count.
	Worlds int

	// RequiredWall and RequiredWorlds are the PROFILE's own numbers, captured
	// before `--budget` / `--worlds` are applied. Narrowed says argv reduced at
	// least one of them.
	//
	// Wall and Worlds above are what this run will actually enforce; these are
	// what the committed, lock-covered profile asks for. Keeping both is what
	// lets the verdict state the gap instead of silently reporting the smaller
	// number as though it were the requirement. See D-059.
	RequiredWall   time.Duration
	RequiredWorlds int
	Narrowed       bool
}

// ApplyCLIOverrides returns b narrowed by `--budget` and `--worlds`, recording
// the profile's own numbers and whether anything moved.
//
// The overrides may only ever NARROW. Widening from argv would let an agent
// grant itself a longer budget without touching prothesis.yaml, so the lock
// would guard a file nobody edits (CRUCIBLE E.2). A zero or negative override
// means "not supplied" and is ignored.
//
// Narrowing is permitted but is NOT safe, which is the correction D-059 makes
// to D-033: for a search over a fault space, fewer worlds is fewer chances to
// find the defect. The flag survives because a narrowed run is useful while
// developing; what changes is that the narrowing is recorded here and the
// verdict is forbidden from calling such a run a PASS.
func ApplyCLIOverrides(b Budget, cliWall time.Duration, cliWorlds int) Budget {
	b.RequiredWall, b.RequiredWorlds = b.Wall, b.Worlds
	if cliWall > 0 && (b.Wall <= 0 || cliWall < b.Wall) {
		b.Wall = cliWall
		b.Narrowed = true
	}
	if cliWorlds > 0 && (b.Worlds < 0 || cliWorlds < b.Worlds) {
		b.Worlds = cliWorlds
		b.Narrowed = true
	}
	return b
}

// describeWorlds renders a world count for an operator-facing message. An
// unbounded ceiling is spelled, not printed as -1: the message exists to be read
// under time pressure and "-1 -> 1" reads like a smaller number, not like a
// ceiling being introduced where there was none.
func describeWorlds(n int) string {
	if n <= 0 {
		return "unbounded"
	}
	return fmt.Sprintf("%d", n)
}

// describeWall renders a wall budget, spelling an unbounded one for the same
// reason.
func describeWall(d time.Duration) string {
	if d <= 0 {
		return "unbounded"
	}
	return d.String()
}

// WallUnbounded reports whether the wall-clock budget is unlimited.
func (b Budget) WallUnbounded() bool { return b.Wall <= 0 }

// WorldsUnbounded reports whether the world count is unlimited.
func (b Budget) WorldsUnbounded() bool { return b.Worlds <= 0 }

// Validate rejects a budget that can never terminate.
//
// An unbounded wall clock AND an unbounded world count is a run with no
// stopping condition at all, which invariant I7 forbids outright. Reporting it
// up front is much better than discovering it eight hours later.
func (b Budget) Validate() error {
	if b.WallUnbounded() && b.WorldsUnbounded() {
		return errors.New("control: budget has neither a wall-clock limit nor a world count; " +
			"invariant I7 requires every run to be bounded by one or the other")
	}
	if b.Worlds < schema.WorldsUnbounded {
		return fmt.Errorf("control: worlds %d is not a world count (want a positive number or %d for unbounded)",
			b.Worlds, schema.WorldsUnbounded)
	}
	return nil
}

// BudgetTracker measures a run against its budget.
//
// Elapsed time is taken from recorder.Clock.NowR, which is a MONOTONIC reading.
// That is not a detail: an 8-hour soak sampled against the wall clock would have
// its budget silently moved by any NTP step, a manual clock change, or a
// Hyper-V save/restore; all realistic on a Windows host running Docker
// Desktop. A budget that moves is not a budget.
type BudgetTracker struct {
	budget Budget
	clock  recorder.Clock
	startR recorder.RTime

	mu        sync.Mutex
	started   int
	completed int
}

// NewBudgetTracker starts tracking b against clock, from now.
func NewBudgetTracker(b Budget, clock recorder.Clock) *BudgetTracker {
	return &BudgetTracker{budget: b, clock: clock, startR: clock.NowR()}
}

// Budget returns the budget being tracked.
func (t *BudgetTracker) Budget() Budget { return t.budget }

// Elapsed is the monotonic time since the tracker was created.
func (t *BudgetTracker) Elapsed() time.Duration {
	return time.Duration(t.clock.NowR() - t.startR)
}

// Remaining is the wall budget left. It is never negative, and it is
// math.MaxInt64 nanoseconds for an unbounded budget: callers should test
// WallUnbounded rather than compare against that value.
func (t *BudgetTracker) Remaining() time.Duration {
	if t.budget.WallUnbounded() {
		return time.Duration(1<<63 - 1)
	}
	d := t.budget.Wall - t.Elapsed()
	if d < 0 {
		return 0
	}
	return d
}

// WallExpired reports whether the wall-clock budget is used up.
func (t *BudgetTracker) WallExpired() bool {
	return !t.budget.WallUnbounded() && t.Elapsed() >= t.budget.Wall
}

// NoteWorldStarted records that a world's execution has begun.
//
// A world counts as RUN the moment it is entered, not when it succeeds. A world
// cut short by budget expiry consumed real time and left real artifacts;
// reporting worlds_run as if it never happened would understate what the run
// actually did.
func (t *BudgetTracker) NoteWorldStarted() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.started++
	return t.started
}

// NoteWorldFinished records that a world's execution completed its lifecycle.
func (t *BudgetTracker) NoteWorldFinished() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.completed++
}

// WorldsStarted is how many worlds were entered.
func (t *BudgetTracker) WorldsStarted() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.started
}

// WorldsFinished is how many worlds completed their lifecycle.
func (t *BudgetTracker) WorldsFinished() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.completed
}

// WorldsExhausted reports whether the planned world count has been reached.
func (t *BudgetTracker) WorldsExhausted() bool {
	if t.budget.WorldsUnbounded() {
		return false
	}
	return t.WorldsStarted() >= t.budget.Worlds
}

// MayStartWorld reports whether another world may be entered, and why not.
//
// The reason string is carried into the verdict rather than discarded, because
// "stopped after 22 of 30 worlds" and "ran all 30" are different facts and the
// agent reading the verdict has to be able to tell them apart.
func (t *BudgetTracker) MayStartWorld() (bool, string) {
	if t.WorldsExhausted() {
		return false, fmt.Sprintf("planned world count reached (%d)", t.budget.Worlds)
	}
	if t.WallExpired() {
		return false, fmt.Sprintf("wall-clock budget of %s used up", t.budget.Wall)
	}
	return true, ""
}

// Snapshot renders the verdict's `budget` object (directive 4.6).
//
// worlds_planned is emitted as schema.WorldsUnbounded (-1) for an unbounded
// profile, which is the config's own spelling for the same idea, rather than as
// a fabricated finite number.
func (t *BudgetTracker) Snapshot() schema.Budget {
	planned := t.budget.Worlds
	if t.budget.WorldsUnbounded() {
		planned = schema.WorldsUnbounded
	}
	wall := int64(0)
	if !t.budget.WallUnbounded() {
		wall = int64(t.budget.Wall.Round(time.Second).Seconds())
	}
	out := schema.Budget{
		WallS:         wall,
		UsedS:         int64(t.Elapsed().Round(time.Second).Seconds()),
		WorldsPlanned: planned,
		WorldsRun:     t.WorldsStarted(),
	}
	if !t.budget.Narrowed {
		return out
	}
	// Both required values are emitted whenever anything narrowed, not only the
	// one that moved: a reader comparing the gate that ran against the gate the
	// profile specifies needs the whole pair, and half of it invites the guess
	// that the other half was untouched.
	out.Narrowed = true
	out.WorldsRequired = t.budget.RequiredWorlds
	if t.budget.RequiredWorlds <= 0 {
		out.WorldsRequired = schema.WorldsUnbounded
	}
	if t.budget.RequiredWall > 0 {
		out.WallSRequired = int64(t.budget.RequiredWall.Round(time.Second).Seconds())
	}
	return out
}

// BudgetOutcome decides what a budget expiry means.
//
// Directive 4.7 does not say "the budget expired"; it says exit 3 is
// "BUDGET_EXHAUSTED: Budget expired WITHOUT COVERAGE PROGRESS". The
// qualifier is normative and easy to lose: a soak that expires having found no
// violation but having discovered new log templates is a PASS, not a soft
// warning.
//
// Coverage is a Phase 4 signal and does not exist yet, so coverageProgress is
// false on every Phase 1 call site and expiry always yields exit 3. The
// parameter exists so that Phase 4 wires a value in rather than discovering
// that the rule was quietly dropped.
func BudgetOutcome(coverageProgress bool) Outcome {
	if coverageProgress {
		return OutcomePass
	}
	return OutcomeBudgetExhausted
}
