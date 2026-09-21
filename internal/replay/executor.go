package replay

import (
	"context"
	"time"

	"github.com/WrdCstlg/pro-thesis/internal/control"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// ---------------------------------------------------------------------------
// The execution seam
//
// This package decides WHAT to execute and WHETHER it reproduced. It does not
// own a lifecycle, a compose project or a bridge network. Execution goes through
// this interface, implemented by internal/control for a real replay and by a
// pure function in the tests, which is what lets the realized-over-planned
// rule, the k/n arithmetic, the divergence rule and the budget all be pinned
// WITHOUT Docker, on a host where a world costs thirty seconds.
// ---------------------------------------------------------------------------

// ExecRequest names one execution.
type ExecRequest struct {
	// Ordinal is the world's 1-based identity within the run bundle. Each
	// attempt gets its own, so k confirmation replays leave k inspectable world
	// directories rather than overwriting one.
	Ordinal int
	// Seed is the world's OWN seed, taken from the `.thesis` file. It is pinned,
	// not derived: see control.WorldRequest.Seed.
	Seed uint64
	// Faults is the schedule ChooseSchedule selected.
	Faults []string
	// Label is carried through for reporting.
	Label string
	// KeepUp asks the executor to leave the topology standing and persist the
	// up -> down handoff. Only the LAST attempt of a replay ever sets it.
	KeepUp bool
}

// Execution is one execution's result, reduced to what a replay judges.
type Execution struct {
	Outcome control.Outcome
	// Findings is EVERY oracle's result, not only the violations. "Checked and
	// satisfied" and "never ran" are different facts (D-034) and a replay that
	// could not tell them apart would report a vacuous non-reproduction.
	Findings []control.OracleResult
	Realized []schema.RealizedFault
	Phases   schema.PhaseTimings
	Paths    control.WorldPaths
	Project  string
	// HostPorts is where each node was observed, so --keep-up can print
	// something a human can actually connect to.
	HostPorts map[string]int64
	Duration  time.Duration
	KeptUp    bool
	// Err is the execution's own error. It never carries the outcome.
	Err error
}

// Executor executes one world.
type Executor interface {
	Execute(ctx context.Context, req ExecRequest) Execution
}

// RunnerExecutor is the real executor: internal/control's serial single-world
// path.
type RunnerExecutor struct {
	Runner *control.Runner
}

// NewRunnerExecutor wraps a runner.
func NewRunnerExecutor(r *control.Runner) *RunnerExecutor { return &RunnerExecutor{Runner: r} }

// Execute runs one world through the eight-phase lifecycle.
func (e *RunnerExecutor) Execute(ctx context.Context, req ExecRequest) Execution {
	seed := req.Seed
	wr := control.WorldRequest{
		Ordinal: req.Ordinal,
		Faults:  req.Faults,
		Label:   req.Label,
		Seed:    &seed,
	}
	var out control.WorldOutcome
	if req.KeepUp {
		out = e.Runner.RunWorldKeepUp(ctx, wr)
	} else {
		out = e.Runner.RunWorld(ctx, wr)
	}
	return Execution{
		Outcome:   out.Outcome,
		Findings:  out.Findings,
		Realized:  out.Realized,
		Phases:    out.Phases,
		Paths:     out.Paths,
		Project:   out.Project,
		HostPorts: out.HostPorts,
		Duration:  out.Duration,
		KeptUp:    out.KeptUp,
		Err:       out.Err,
	}
}
