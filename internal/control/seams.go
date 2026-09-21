package control

import (
	"context"
	"time"

	"github.com/WrdCstlg/pro-thesis/internal/oracle"
	"github.com/WrdCstlg/pro-thesis/internal/recorder"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// ---------------------------------------------------------------------------
// Driver: the workload generator (directive 4.1 step 3, 4.2 `driver:`)
// ---------------------------------------------------------------------------

// DriverRequest is everything the driver supervisor needs to launch one world's
// workload.
//
// The driver is an EXTERNAL, UNMODIFIED process (invariant I1). It cannot know
// the harness's virtual clock origin, so it emits Unix epoch nanoseconds and the
// recorder converts (PHASE0_BUILD_BRIEF D-C). It also cannot be assumed to parse
// prothesis.yaml, which is why Profile carries the profile NAME and PlanPath
// exists as the additive channel for its contents (DECISIONS.md D-021, OQ-012).
type DriverRequest struct {
	// Config is the validated prothesis.yaml.
	Config *schema.Config
	// ProjectDir is the directory prothesis.yaml lives in. Relative paths in
	// driver.cmd resolve against it.
	ProjectDir string
	// RunID and WorldOrdinal identify the execution.
	RunID        string
	WorldOrdinal int
	// WorldDir is the world's artifact directory.
	WorldDir string
	// HistoryPath is the {history_path} substitution: where the driver appends
	// its operation records. NOTHING ELSE WRITES TO THIS FILE.
	HistoryPath string
	// PlanPath is the {plan_path} substitution and the PROTHESIS_PLAN_PATH
	// value. Empty in Phase 1: op-plan shrinking is Phase 5.
	PlanPath string
	// Seed is the {seed} substitution: this world's seed, not the run's.
	Seed uint64
	// Profile is the {profile} substitution: the DRIVER profile name.
	Profile string
	// Topology is the live binding, so a driver that needs host ports can find
	// them without re-deriving them.
	Topology *recorder.Topology

	// StdinControl asks the supervisor to give the driver a stdin control
	// channel, so QUIESCE can DRAIN it rather than only kill it (see drain.go
	// and OQ-033). A driver that ignores the channel is unaffected; the drain
	// then hits its deadline and says so.
	//
	// ADDITIVE, and off by default at this seam so a Driver implementation
	// written against Phase 1 keeps its exact behaviour.
	StdinControl bool

	// Env is extra environment for the driver process, appended to the
	// harness's own. Phase 4's parallel executor uses it to point each
	// concurrent world's driver at that world's own published host ports.
	//
	// ADDITIVE.
	Env []string
}

// DriverResult is a workload's outcome.
type DriverResult struct {
	// Started reports whether the workload was launched at all.
	Started bool
	// ExitCode is the process exit status. Meaningless unless Started.
	ExitCode int
	// Stopped reports whether the workload was terminated by QUIESCE rather
	// than exiting on its own. A non-zero ExitCode on a stopped driver is
	// expected, not a failure.
	Stopped bool
	// Err is non-nil when the workload could not be run, or exited in a way
	// that makes its history untrustworthy. It is an ENVIRONMENT failure
	// (exit 2), never an oracle violation.
	Err error
	// OpsWritten is the number of history records observed, when the supervisor
	// counts them. Zero means "not counted", not "none".
	OpsWritten int64

	// Drain records whether the workload was asked to finish its outstanding
	// operations before being stopped, and what it did (see drain.go).
	//
	// It is on DriverResult, and therefore on EvalRequest.Driver, because an
	// oracle reasoning about in-flight operations is entitled to know whether
	// the harness gave them a chance to retire. It is a fact about the run, not
	// a licence to assume a benign reading: OQ-033 rejects that move by name.
	Drain DrainReport
}

// DriverHandle is a running workload.
type DriverHandle interface {
	// Done is closed when the workload exits of its own accord.
	Done() <-chan struct{}

	// Drain asks the workload to stop ISSUING new operations while completing
	// the ones it already holds, and waits up to deadline for it to exit.
	//
	// It never returns an error: every way it can go wrong is a fact about the
	// run that has to be RECORDED rather than propagated, because none of them
	// stops the world from proceeding. The caller stops the driver afterwards
	// regardless: a drain that succeeded makes Stop a no-op, and a drain that
	// did not still has to be followed by a kill.
	Drain(ctx context.Context, deadline time.Duration) DrainReport

	// Stop terminates the workload and waits for it. It is idempotent and safe
	// after the workload has already exited, because QUIESCE and TEARDOWN both
	// call it and either may be first.
	Stop(ctx context.Context) error

	// Result reports the outcome. Valid once Done is closed or Stop returns.
	Result() DriverResult
}

// Driver launches the workload for one world.
type Driver interface {
	Start(ctx context.Context, req DriverRequest) (DriverHandle, error)
}

// ---------------------------------------------------------------------------
// Telemetry: the sampled system observations oracles read
// ---------------------------------------------------------------------------

// TelemetryRequest configures one world's sampler.
type TelemetryRequest struct {
	Config   *schema.Config
	Topology *recorder.Topology
	// OutputPath is the artifact prothesis.oracle_input/v1's telemetry_path
	// will point at.
	OutputPath string
	// Clock and Timeline are the recorder's, so telemetry timestamps share one
	// frame with the phase markers and the history.
	Clock    recorder.Clock
	Timeline *recorder.Timeline
	// Interval is the sampling period. Zero means the sampler's own default.
	Interval time.Duration
}

// TelemetryHandle is a running sampler.
type TelemetryHandle interface {
	// Stop halts sampling and flushes. It is idempotent.
	Stop(ctx context.Context) error
	// Path is the artifact written.
	Path() string
}

// Telemetry samples system observations across a world.
//
// It starts at the BEGINNING OF SEED rather than at DRIVE, because
// resource_return_to_baseline compares post-QUIESCE resource usage against a
// BASELINE, and the only place a baseline can honestly be measured is the
// steady, unloaded system that SEED establishes. Samples taken before DRIVE
// have no virtual time; they carry epoch t_ns like the history does, and the
// oracle converts through the phase windows.
type Telemetry interface {
	Start(ctx context.Context, req TelemetryRequest) (TelemetryHandle, error)
}

// ---------------------------------------------------------------------------
// Oracle engine: invariants I3 and I5
// ---------------------------------------------------------------------------

// EvalRequest is the evidence ASSERT hands the oracle engine.
type EvalRequest struct {
	Config     *schema.Config
	ProjectDir string
	RunID      string
	// WorldOrdinal and World identify what was executed.
	WorldOrdinal int
	World        *schema.World
	Topology     *recorder.Topology
	// Paths are the world's artifact locations.
	Paths WorldPaths
	// Input is the ready-made prothesis.oracle_input/v1 document, so an
	// external oracle runner (Phase 3) has nothing to reconstruct and a built-in
	// oracle reads the same phase windows an external one would.
	Input schema.OracleInput
	// Phases is Input.Phases, surfaced separately because every built-in oracle
	// needs it and none of them should have to reach through the document.
	Phases schema.PhaseTimings
	// Clock and Timeline convert between the epoch frame the history uses and
	// the millisecond frame the phase windows use.
	Clock    recorder.Clock
	Timeline *recorder.Timeline
	// Driver is the workload's outcome, so an oracle can distinguish "no
	// operations because the system was wedged" from "no operations because the
	// driver never started".
	Driver DriverResult
	// Logs are the node logs collected at TEARDOWN, when collection ran before
	// evaluation. Empty is normal in Phase 1: logs are collected in TEARDOWN,
	// which follows ASSERT.
	Logs []CollectedLog
	// Probes are health probe observations collected during QUIESCE.
	Probes []oracle.ProbeObservation

	// Realized is what the perturber ACTUALLY injected: the concrete node ids a
	// target resolved to, and the measured window between injection and
	// withdrawal. It is what oracle.Input.PlannedFaults is built from.
	//
	// ADDITIVE (OQ-038). It is here because the field it feeds was hardcoded to
	// an EMPTY LIST, which made no_crash's entire "outside a planned fault
	// window" clause dead code in production. Measured: a world whose only fault
	// was `proc.kill(kv-n1, signal=SIGTERM)@12600..14600` reported
	//
	//	no_crash VIOLATED: "1 process exited outside any planned fault window:
	//	                    kv-n1 at t+12713ms (exit code 0)"
	//
	// against a window that started 113 ms earlier and named that very node. The
	// oracle's logic was right; it was handed nothing to reason with.
	//
	// The consequence is a Phase 4 concern, not a cosmetic one: with proc.kill
	// in perturber.allow, a guided search reaches its FIRST kill world, is handed
	// a fabricated crash violation, and stops; having "found" a bug that is the
	// harness reporting its own fault injection. That is the ranked #1 failure
	// mode of this phase, arriving through an unwired field rather than through a
	// weakened assertion.
	//
	// REALIZED rather than PLANNED, deliberately. A planned `role:leader` binds
	// to a concrete node only at INJECTION time (D-012), so the planned schedule
	// cannot say which node's exit to excuse; the realized record can, and
	// oracle.FaultWindow documents that an empty node list excuses nothing.
	Realized []schema.RealizedFault
}

// OracleResult is one oracle's finding, plus the two facts the verdict needs
// that prothesis.oracle_output/v1 does not carry.
type OracleResult struct {
	// Output is the oracle's own document.
	Output schema.OracleOutput

	// ObservedPhase is the lifecycle phase in which the finding was OBSERVED.
	//
	// It is a DIFFERENT field from Output.ValidPhases, which declares where
	// evaluating the oracle is meaningful (invariant I5). The frozen examples
	// invite conflating them (OQ-004). The empty phase means the finding is not
	// scoped to one.
	ObservedPhase schema.Phase

	// FirstSeenMS is the virtual-clock millisecond offset of the earliest
	// evidence for the finding, in the same frame as the phase windows.
	FirstSeenMS int64

	// Err is set when this oracle could not be evaluated at all. Such a result
	// is INCONCLUSIVE (exit 2), never a pass: "an oracle that could not check"
	// is the directive's own example of exit 2.
	Err error
}

// Violated reports whether this result is a violation.
func (r OracleResult) Violated() bool {
	return r.Err == nil && r.Output.Status == schema.StatusViolated
}

// Outcome classifies one oracle result.
//
// The default arm is load-bearing: an unrecognised status is treated as
// inconclusive, never as ok. Reading a status nobody defined as a pass is the
// cheapest gate-weakening vector there is (invariant I6).
func (r OracleResult) Outcome() Outcome {
	if r.Err != nil {
		return OutcomeInconclusive
	}
	switch r.Output.Status {
	case schema.StatusOK:
		return OutcomePass
	case schema.StatusViolated:
		return OutcomeViolation
	case schema.StatusInconclusive:
		return OutcomeInconclusive
	default:
		return OutcomeInconclusive
	}
}

// OracleEngine evaluates the configured oracles against one world's evidence.
//
// An engine-level error means ASSERT could not be carried out, which is
// INCONCLUSIVE. Returning no results and no error for a config that lists
// built-in oracles is treated by the runner as inconclusive too: see
// worldRun.assertPhase. A vacuous pass over a system nobody checked is the most
// dangerous output this tool can produce.
type OracleEngine interface {
	Evaluate(ctx context.Context, req EvalRequest) ([]OracleResult, error)
}

// ---------------------------------------------------------------------------
// Steady state: directive 4.1 step 2
// ---------------------------------------------------------------------------

// SteadyStateRequest is the input to the SEED probe.
type SteadyStateRequest struct {
	Config     *schema.Config
	ProjectDir string
	Topology   *recorder.Topology
	Probe      string
	Timeout    time.Duration
}

// SteadyStateProber runs harness.steady_state.probe and reports whether the
// system reached steady state.
//
// A failure here is an ENVIRONMENT failure (exit 2). The config was valid; the
// system did not settle.
type SteadyStateProber interface {
	Probe(ctx context.Context, req SteadyStateRequest) error
}

// ---------------------------------------------------------------------------
// Log collection: directive 4.1 step 8
// ---------------------------------------------------------------------------

// LogRequest is the input to TEARDOWN's log collection.
type LogRequest struct {
	Topology *recorder.Topology
	// Dir is the directory to write per-node log files into.
	Dir string
	// Timeline converts a log line's timestamp into the virtual frame. It may
	// have no origin, in which case timestamps are left unconverted.
	Timeline *recorder.Timeline
}

// CollectedLog is one node's collected output.
type CollectedLog struct {
	// Node is the logical node id.
	Node string
	// Path is the file the log was written to.
	Path string
	// Lines are the parsed, timestamped lines, in file order. It may be empty
	// even when Path exists: collection writes the raw file first and parses
	// second, so a log with no parseable timestamps still reaches disk.
	Lines []LogLine
	// Err records a per-node collection failure. Log collection NEVER fails a
	// world: a missing log makes a causal timeline thinner, not wrong.
	Err error
}

// LogLine is one timestamped line of node output.
type LogLine struct {
	// TNS is Unix epoch nanoseconds, from the container runtime's own stamp.
	TNS int64
	// TMS is the virtual-clock millisecond offset, valid only when HasVirtual.
	TMS int64
	// HasVirtual reports whether TMS could be computed, i.e. whether DRIVE had
	// started by the time this run's timeline was consulted.
	HasVirtual bool
	// Text is the line with its timestamp prefix removed.
	Text string
}

// LogCollector collects node logs at TEARDOWN, before the topology is
// destroyed.
type LogCollector interface {
	Collect(ctx context.Context, req LogRequest) ([]CollectedLog, error)
}

// ---------------------------------------------------------------------------
// SUT image resolution: OQ-055's provenance, behind a seam (D-073)
// ---------------------------------------------------------------------------

// ImageResolver names the images the bound containers are actually running,
// for world.thesis's sut.images.
//
// It is a seam for the same reason Driver and LogCollector are: the production
// implementation asks the Docker daemon, and a unit test that drives a stub
// backend with invented container ids has no daemon to ask and must not find
// out whether one is healthy. Before this seam existed, those tests did, and
// on the day the daemon was sick, two of them failed BUDGET_EXHAUSTED for a
// lookup they never wanted (OQ-067). A test whose result depends on the health
// of a daemon it does not use is not a unit test.
//
// Resolution is fail-soft by contract: an error costs the world its provenance
// and never its execution. The runner records [] and prints a warning that
// names the cause.
type ImageResolver interface {
	Resolve(ctx context.Context, nodes []recorder.NodeBinding, cfg []schema.NodeConfig) ([]schema.SUTImage, error)
}
