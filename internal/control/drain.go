package control

import (
	"fmt"
	"time"
)

// ---------------------------------------------------------------------------
// DRAIN: the first act of QUIESCE
//
// This closes OQ-033. The symptom was that a CLEAN world on a long profile could
// never report PASS: `no_stuck_op` said, correctly,
//
//	"16 operation(s) were still in flight when observation ended at t+19002ms
//	 before the 5000ms SLO ceiling elapsed"
//
// and returned INCONCLUSIVE. The oracle is right and was not touched. The
// LIFECYCLE was wrong: QUIESCE killed the driver outright, so a profile long
// enough to span a fault window, which is what D-040 built `linear` to be, was
// ALWAYS killed with operations outstanding, and an operation with no completion
// record is indistinguishable from a wedged one. Every clean long world was
// inconclusive by construction rather than by evidence.
//
// Why that is fatal for Phase 4 specifically, and not merely untidy: the
// Saboteur's utility function scores oracle findings. If a clean world and an
// unevaluable one produce the same document, the search cannot tell "nothing is
// wrong here" from "I could not tell", and it ranks worlds on noise. That is
// failure mode #1 for this phase.
//
// # Why this is not a ninth phase
//
// Directive 4.1 freezes eight phase names and invariant I5 is built on them. The
// drain therefore adds NO name: it is the first act of QUIESCE, whose normative
// job is "stop the workload and wait for convergence". Stopping a workload by
// asking it to finish is a better rendering of that sentence than killing it
// mid-operation, so this is arguably what QUIESCE always meant. Nothing new
// appears in schema.Phase, in phases.jsonl, or in oracle_input.phases.
//
// # Why it is bounded, and what happens at the bound
//
// A driver that does not implement the control channel, or that is genuinely
// wedged, will not drain. The deadline is what makes that observable rather than
// fatal: at the bound the driver is KILLED exactly as before and the fact is
// recorded in result.json, on stderr, and on DriverResult, where an oracle can
// see it. A hung driver must not hang the run.
//
// # Why this cannot manufacture a pass
//
// Draining does not hide a stuck operation, it EXPOSES one. `no_stuck_op` still
// violates on any operation that took longer than the SLO ceiling to retire
// after HEAL, and an operation that would previously have been in flight at
// observation end (inconclusive, unjudged) now either completes and is judged,
// or is still open with the ceiling long elapsed and is reported STUCK. The
// deadline is therefore deliberately longer than the SLO ceiling: a shorter one
// would stop observing before the oracle's own threshold was reachable, which is
// the defect being fixed, not a fix for it.
// ---------------------------------------------------------------------------

// DefaultDrainDeadline is how long a driver is given to finish its outstanding
// operations after being asked to stop issuing.
//
// It exceeds oracle.DefaultStuckOpSLO (5s) on purpose: see the note above. It
// is not a guess about how long the fixture takes: loadgen's own per-operation
// deadlines are 500ms for a read, 1s for a write and 1.5s for a transaction,
// over at most three leader-hint attempts, so a healthy drain retires in well
// under half of this and the remainder is the margin in which a SICK one becomes
// visible to the oracle instead of invisible to it.
const DefaultDrainDeadline = 10 * time.Second

// DrainOutcome is what happened when the harness asked the workload to stop
// issuing new operations and complete the ones it had.
//
// The outcomes are kept distinct because they license different readings of the
// world's verdict. Collapsing "we never asked" into "it refused" would make a
// misconfiguration look like a wedged system under test, and collapsing
// "it refused" into "it finished" would be the vacuous pass this whole file
// exists to avoid.
type DrainOutcome string

const (
	// DrainNotAttempted: draining was disabled, or the driver never started.
	DrainNotAttempted DrainOutcome = "not_attempted"

	// DrainUnsupported: this driver was launched without a control channel, so
	// there was no way to ask.
	DrainUnsupported DrainOutcome = "unsupported"

	// DrainCompleted: the driver exited of its own accord inside the deadline.
	// Its history has a completion record for every invoke it issued.
	DrainCompleted DrainOutcome = "completed"

	// DrainDeadlineExceeded: the driver was asked and did not finish in time. It
	// is killed exactly as it was before, and operations may be left in flight.
	DrainDeadlineExceeded DrainOutcome = "deadline_exceeded"

	// DrainCanceled: the run was cancelled while draining.
	DrainCanceled DrainOutcome = "canceled"

	// DrainFailed: the control channel itself broke.
	DrainFailed DrainOutcome = "failed"
)

// AllDrainOutcomes is the closed set, so a test can keep the reporting
// exhaustive.
var AllDrainOutcomes = [...]DrainOutcome{
	DrainNotAttempted,
	DrainUnsupported,
	DrainCompleted,
	DrainDeadlineExceeded,
	DrainCanceled,
	DrainFailed,
}

// DrainReport records one world's drain, for result.json and for the oracles.
type DrainReport struct {
	// Outcome is what happened.
	Outcome DrainOutcome
	// Deadline is the bound that was offered. Zero when none was.
	Deadline time.Duration
	// Waited is how long the drain actually took.
	Waited time.Duration
	// Err carries the detail when Outcome is not DrainCompleted.
	Err error
}

// Drained reports the one outcome that means every issued operation has a
// completion record.
func (d DrainReport) Drained() bool { return d.Outcome == DrainCompleted }

// Describe renders the report for a human reading stderr.
func (d DrainReport) Describe() string {
	switch d.Outcome {
	case DrainCompleted:
		return fmt.Sprintf("the driver drained in %s; every issued operation has a completion record",
			d.Waited.Round(time.Millisecond))
	case DrainDeadlineExceeded:
		return fmt.Sprintf("the driver did not drain within %s and was stopped; "+
			"operations left in flight cannot be judged by no_stuck_op",
			d.Deadline.Round(time.Millisecond))
	case DrainUnsupported:
		return "this driver has no stdin control channel, so it could not be asked to drain"
	case DrainCanceled:
		return "the run was cancelled while the driver was draining"
	case DrainFailed:
		return fmt.Sprintf("the drain control channel failed: %v", d.Err)
	case DrainNotAttempted:
		return "no drain was attempted"
	}
	return fmt.Sprintf("unrecognised drain outcome %q, treated as not drained", string(d.Outcome))
}

// Doc renders the report for result.json.
//
// It always carries `outcome` and `drained`, so a reader never has to infer the
// second from the first, and it carries `error` only when there is one. The
// artifact must be able to say "asked and refused"; that is the fact OQ-033's
// fix would otherwise hide.
func (d DrainReport) Doc() map[string]any {
	out := map[string]any{
		"outcome":     string(d.Outcome),
		"drained":     d.Drained(),
		"waited_ms":   d.Waited.Milliseconds(),
		"deadline_ms": d.Deadline.Milliseconds(),
	}
	if d.Err != nil {
		out["error"] = d.Err.Error()
	}
	return out
}
