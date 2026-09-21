package schema

import "fmt"

// ExitCode is the normative CLI exit code space (directive 4.7).
//
// It is a DIFFERENT space from OracleExitCode: 1 means FAIL here and VIOLATED
// there, 2 means INCONCLUSIVE here and INCONCLUSIVE there but for a different
// reason.
type ExitCode int

const (
	// ExitPass: all oracles satisfied across all worlds.
	ExitPass ExitCode = 0
	// ExitFail: an oracle violation was found; inspect minimal_repro.
	ExitFail ExitCode = 1
	// ExitInconclusive: harness or environment error (docker daemon dead, a
	// host port held by a foreign process, no usable execution path). Retry
	// once, then escalate. NOT for an invalid config: that is ExitConfigError.
	ExitInconclusive ExitCode = 2
	// ExitBudgetExhausted: the budget expired WITHOUT COVERAGE PROGRESS.
	//
	// The coverage qualifier is normative and is easy to lose: a soak run that
	// expires having found no violation but having added new log templates is a
	// PASS, not a soft warning. This code encodes the decision; it does not make
	// it. The control plane decides, and it must not map every budget expiry
	// here. Coverage itself is a Phase 4 signal.
	ExitBudgetExhausted ExitCode = 3
	// ExitOracleDrift: .prothesis/lock mismatch or a gate-weakening attempt.
	// Fail closed; never auto-resolve.
	ExitOracleDrift ExitCode = 4
	// ExitConfigError: invalid prothesis.yaml or invalid CLI arguments.
	ExitConfigError ExitCode = 5
)

// Valid reports whether c is one of the six defined codes.
func (c ExitCode) Valid() bool { return c >= ExitPass && c <= ExitConfigError }

func (c ExitCode) String() string { return string(c.Verdict()) }

// Verdict is the encoding-level bijection between exit code and verdict string.
//
// It is a pure encoding, not a decision procedure: see the note on
// ExitBudgetExhausted.
func (c ExitCode) Verdict() VerdictResult {
	switch c {
	case ExitPass:
		return VerdictPass
	case ExitFail:
		return VerdictFail
	case ExitInconclusive:
		return VerdictInconclusive
	case ExitBudgetExhausted:
		return VerdictBudgetExhausted
	case ExitOracleDrift:
		return VerdictOracleDrift
	case ExitConfigError:
		return VerdictConfigError
	}
	return VerdictResult(fmt.Sprintf("UNKNOWN(%d)", int(c)))
}

// AgentAction is the agent-loop contract: what an autonomous consumer should do
// with each exit code. Surfaced so a loop does not hardcode the table.
type AgentAction string

const (
	ActionProceed   AgentAction = "proceed"
	ActionFixBug    AgentAction = "fix_bug_using_minimal_repro"
	ActionRetryOnce AgentAction = "retry_once_then_escalate"
	ActionSoftWarn  AgentAction = "soft_warning_do_not_merge_blind"
	ActionEscalate  AgentAction = "stop_escalate_to_human_never_auto_resolve"
	ActionFixConfig AgentAction = "fix_prothesis_yaml"
)

// AgentAction maps an exit code to the action an agent loop should take.
func (c ExitCode) AgentAction() AgentAction {
	switch c {
	case ExitPass:
		return ActionProceed
	case ExitFail:
		return ActionFixBug
	case ExitInconclusive:
		return ActionRetryOnce
	case ExitBudgetExhausted:
		return ActionSoftWarn
	case ExitOracleDrift:
		return ActionEscalate
	default:
		return ActionFixConfig
	}
}

// AllExitCodes is every defined code, in numeric order.
var AllExitCodes = [...]ExitCode{
	ExitPass, ExitFail, ExitInconclusive,
	ExitBudgetExhausted, ExitOracleDrift, ExitConfigError,
}
