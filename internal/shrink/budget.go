package shrink

import (
	"fmt"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// RULE 3; the budget is explicit and expiry is REPORTED
//
// D-029, in full: "Naive ddmin over 14 faults, then 20,000 ops, then binary
// search window narrowing, then 3x confirmation, is an estimated 9-52 hours at a
// measured ~29s per world -- which contradicts I7's 'every invocation takes a
// budget and returns a verdict'. Shrinking takes an explicit budget like every
// other operation and emits a PARTIAL shrink with attempted: true and the honest
// before/after counts when it expires."
//
// So there is no unbounded path through this package. Every world costs a grant
// from the Ledger, every grant is refused once a ceiling is reached, and the
// refusal carries the reason all the way into the Result.
// ---------------------------------------------------------------------------

// Default budget values. They are deliberately small relative to a search run:
// D-029 makes shrinking a FRACTION of the run budget, and a shrink that eats the
// budget of the thing that found the bug is not an improvement on not shrinking.
const (
	// DefaultWallBudget is 20 minutes: ~40 worlds at the measured ~30s each,
	// which is enough for a 14-fault ddmin plus a confirmation gate and is far
	// short of the 9-52 hours the naive pipeline would take.
	DefaultWallBudget = 20 * time.Minute
	// DefaultWorldBudget caps the world count independently of the clock, so a
	// host that is unexpectedly fast cannot turn a wall budget into an unbounded
	// number of executions.
	DefaultWorldBudget = 60
)

// StopReason says why the pipeline stopped where it did. The empty value means
// it ran to completion.
type StopReason string

const (
	// StopComplete is the zero value: nothing cut the pipeline short.
	StopComplete StopReason = ""
	// StopWallBudget: the wall ceiling elapsed.
	StopWallBudget StopReason = "wall_budget_expired"
	// StopWorldBudget: the world ceiling was reached.
	StopWorldBudget StopReason = "world_budget_exhausted"
	// StopCanceled: the caller's context was cancelled.
	StopCanceled StopReason = "canceled"
	// StopExecutorError: the executor itself failed, as distinct from a world
	// failing. Retrying candidates against a broken executor spends budget to
	// learn nothing.
	StopExecutorError StopReason = "executor_error"
	// StopBaselineNotReproduced: the UNREDUCED world did not reproduce the
	// violation. Every subsequent rejection would be meaningless, so the
	// pipeline refuses to shrink rather than spend a budget producing a
	// confident-looking "nothing could be removed".
	StopBaselineNotReproduced StopReason = "baseline_did_not_reproduce"
	// StopBaselineUnjudgeable: the unreduced world could not be judged at all;
	// it never reached a state its oracles could speak about. Distinct from
	// StopBaselineNotReproduced on purpose: "it did not reproduce" is a claim
	// about the bug, "it could not be judged" is a claim about the harness, and
	// reporting the second as the first is the dishonesty this package exists to
	// avoid.
	StopBaselineUnjudgeable StopReason = "baseline_could_not_be_judged"
)

// Truncated reports whether this reason means the result is PARTIAL.
func (s StopReason) Truncated() bool { return s != StopComplete }

// Describe explains the reason and what to do about it.
func (s StopReason) Describe() string {
	switch s {
	case StopComplete:
		return "the pipeline ran to completion"
	case StopWallBudget:
		return "the shrink wall budget elapsed; the reduction below is partial"
	case StopWorldBudget:
		return "the shrink world budget was exhausted; the reduction below is partial"
	case StopCanceled:
		return "interrupted before the pipeline finished"
	case StopExecutorError:
		return "the executor failed; no further candidate could be judged"
	case StopBaselineNotReproduced:
		return "the unreduced world did not reproduce the violation, so no reduction " +
			"could be judged against it"
	case StopBaselineUnjudgeable:
		return "the unreduced world could not be judged, so no reduction could be " +
			"judged against it"
	}
	return string(s)
}

// Budget is the explicit ceiling on a shrink.
type Budget struct {
	// Wall is the wall-clock ceiling. Zero means DefaultWallBudget. A negative
	// value means UNLIMITED, which is a deliberate act a caller must spell.
	Wall time.Duration
	// Worlds is the ceiling on world executions. Zero means
	// DefaultWorldBudget; negative means unlimited.
	Worlds int
	// ConfirmReserve is how many worlds are held back from the stages so the
	// confirmation gate can always run. Zero means Policy.ConfirmK.
	//
	// Reserving matters: without it the pipeline can spend its last world on a
	// narrowing probe and then be unable to measure the `reproduced` number it
	// is about to publish, and a shrink that cannot measure its own repro is
	// exactly failure mode #3.
	ConfirmReserve int

	// ConfirmWallGrace extends the WALL ceiling when the confirmation gate
	// opens. Zero (the default) means the wall budget is HARD: when it expires
	// there is no confirmation, Confirmation.Ran() is false, and
	// Result.MinimalRepro is nil rather than a number nobody measured.
	//
	// It is a separate knob from ConfirmReserve because the two protect against
	// different things. ConfirmReserve costs nothing to set correctly, since
	// worlds are countable in advance. Wall time is not: granting a grace by
	// default would mean a shrink can overrun the budget its caller stated,
	// which is the one thing I7 does not allow it to do silently. A caller who
	// would rather overrun than lose the measurement sets it explicitly.
	ConfirmWallGrace time.Duration
}

func (b Budget) withDefaults(confirmK int) Budget {
	out := b
	if out.Wall == 0 {
		out.Wall = DefaultWallBudget
	}
	if out.Worlds == 0 {
		out.Worlds = DefaultWorldBudget
	}
	if out.ConfirmReserve == 0 {
		out.ConfirmReserve = confirmK
	}
	if out.ConfirmReserve < 0 {
		out.ConfirmReserve = 0
	}
	return out
}

// Describe renders the ceilings for a report.
func (b Budget) Describe() string {
	wall := b.Wall.String()
	if b.Wall < 0 {
		wall = "unlimited"
	}
	worlds := fmt.Sprintf("%d", b.Worlds)
	if b.Worlds < 0 {
		worlds = "unlimited"
	}
	return fmt.Sprintf("wall %s, worlds %s (%d reserved for confirmation)", wall, worlds, b.ConfirmReserve)
}

// ledger meters world executions against a Budget.
//
// It is safe for concurrent use because a batch of candidates is executed
// concurrently and each one charges its own world.
type ledger struct {
	mu      sync.Mutex
	budget  Budget
	start   time.Time
	now     func() time.Time
	spent   int
	reserve int // worlds withheld from the stages
}

func newLedger(b Budget, now func() time.Time) *ledger {
	if now == nil {
		now = time.Now
	}
	return &ledger{budget: b, start: now(), now: now, reserve: b.ConfirmReserve}
}

// acquire asks for permission to run n worlds.
//
// It returns how many were granted, which may be fewer than asked and may be
// zero, and the reason when it granted fewer. Partial grants are the point: a
// round of four ddmin candidates with two worlds left runs the FIRST two, in
// index order, which keeps the answer deterministic even at the ceiling.
func (l *ledger) acquire(n int) (int, StopReason) {
	if n <= 0 {
		return 0, StopComplete
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.budget.Wall >= 0 && l.now().Sub(l.start) >= l.budget.Wall {
		return 0, StopWallBudget
	}
	if l.budget.Worlds < 0 {
		l.spent += n
		return n, StopComplete
	}
	avail := l.budget.Worlds - l.reserve - l.spent
	if avail <= 0 {
		return 0, StopWorldBudget
	}
	if avail < n {
		l.spent += avail
		return avail, StopWorldBudget
	}
	l.spent += n
	return n, StopComplete
}

// openReserve releases the confirmation reserve, and the wall grace with it. It
// is called exactly once, when the stages are done and the gate is about to run.
func (l *ledger) openReserve() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.reserve = 0
	if l.budget.Wall >= 0 && l.budget.ConfirmWallGrace > 0 {
		l.budget.Wall += l.budget.ConfirmWallGrace
	}
}

// wallLeft reports the remaining wall budget, or a negative duration when the
// budget is unlimited.
func (l *ledger) wallLeft() time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.budget.Wall < 0 {
		return -1
	}
	return l.budget.Wall - l.now().Sub(l.start)
}

// spentWorlds reports how many worlds have been granted.
func (l *ledger) spentWorlds() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.spent
}

// elapsed reports the wall time consumed.
func (l *ledger) elapsed() time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.now().Sub(l.start)
}
