package control

import (
	"fmt"

	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// Outcome classifies one execution: a single world, or a whole run once the
// worlds have been folded together.
//
// It is a SEPARATE vocabulary from schema.VerdictResult and schema.ExitCode on
// purpose. Those two are a bijection with each other and are a pure encoding
// (see schema.ExitCode.Verdict); this type is the DECISION, and several distinct
// decisions legitimately land on the same code. Collapsing them early would
// throw away the reason:
//
//	OutcomeInconclusive   an oracle could not evaluate       -> 2
//	OutcomeHarnessError   the environment failed us          -> 2
//	OutcomeCanceled       a human interrupted the run        -> 2
//
// All three are exit 2, and all three need different words in the log.
type Outcome string

const (
	// OutcomePass: every oracle that ran was satisfied.
	OutcomePass Outcome = "pass"

	// OutcomeViolation: an oracle reported a violation. Directive 4.7 exit 1.
	OutcomeViolation Outcome = "violation"

	// OutcomeInconclusive: an oracle COULD NOT CHECK; it errored, it returned
	// an unrecognised status, or the engine produced no result for a configured
	// oracle. This is the half of exit 2 that is about the assertion, not the
	// environment, and it is the reason a vacuous PASS is impossible here: an
	// oracle set that produces nothing is inconclusive, never satisfied.
	OutcomeInconclusive Outcome = "inconclusive"

	// OutcomeHarnessError: the topology, the driver, the steady-state probe or
	// the daemon failed. Directive 4.7 calls this out by name: "harness/
	// environment error (e.g. docker daemon dead)". Exit 2.
	OutcomeHarnessError Outcome = "harness_error"

	// OutcomeCanceled: the caller's context was cancelled by something other
	// than budget expiry, i.e. Ctrl-C. The run did not reach a verdict, so it
	// cannot claim PASS; exit 2 is the only honest code of the six.
	OutcomeCanceled Outcome = "canceled"

	// OutcomeBudgetExhausted: invariant I7. The budget expired before the
	// planned worlds were done. Exit 3.
	OutcomeBudgetExhausted Outcome = "budget_exhausted"

	// OutcomeConfigError: prothesis.yaml or the CLI arguments are wrong. This
	// can only be decided BEFORE any world runs, so it never competes with a
	// world's outcome. Exit 5.
	OutcomeConfigError Outcome = "config_error"
)

// AllOutcomes is the complete closed set, weakest first. Tests iterate it to
// keep the exit-code mapping exhaustive.
var AllOutcomes = [...]Outcome{
	OutcomePass,
	OutcomeBudgetExhausted,
	OutcomeCanceled,
	OutcomeInconclusive,
	OutcomeHarnessError,
	OutcomeViolation,
	OutcomeConfigError,
}

// Valid reports whether o is one of the defined outcomes.
func (o Outcome) Valid() bool {
	for _, x := range AllOutcomes {
		if o == x {
			return true
		}
	}
	return false
}

// Rank orders outcomes for run-level folding, weakest first. An undefined
// outcome ranks ABOVE violation rather than below pass: an outcome nobody
// declared must never be able to make a run greener than it is.
//
// The ordering, and why:
//
//	pass              < everything
//	budget_exhausted  a soft warning; the run simply ran out of time
//	canceled          a human stopped it; less informative than a real failure
//	inconclusive      an oracle could not check — "retry once, then escalate"
//	harness_error     the environment failed — same action, different cause
//	violation         the actionable result: there is a bug, work from it
//	config_error      pre-run; cannot co-occur with any of the above
//
// The one line that needs defending is inconclusive ABOVE budget_exhausted.
// Both mean "we do not know". They differ in what the agent should do:
// budget expiry is a soft warning the directive tells the loop not to merge
// blind on, while an oracle that could not evaluate is a broken measurement
// and gets retry-once-then-escalate. When both are true the stronger claim on
// the agent's attention wins.
func (o Outcome) Rank() int {
	switch o {
	case OutcomePass:
		return 0
	case OutcomeBudgetExhausted:
		return 1
	case OutcomeCanceled:
		return 2
	case OutcomeInconclusive:
		return 3
	case OutcomeHarnessError:
		return 4
	case OutcomeViolation:
		return 5
	case OutcomeConfigError:
		return 6
	}
	return 7
}

// Worse returns the more severe of two outcomes, so a run folds its worlds by
// repeated application.
func (o Outcome) Worse(p Outcome) Outcome {
	if p.Rank() > o.Rank() {
		return p
	}
	return o
}

// VerdictResult maps an outcome to the `verdict` field of
// prothesis.verdict/v1.
//
// ORACLE_DRIFT is deliberately unreachable. The lock manifest is a Phase 3
// deliverable, and exit 4 is the one code the agent-loop contract must never
// auto-resolve; emitting it before the mechanism that justifies it exists would
// be a lie with the highest possible cost. ExitOracleDriftIsPhase3 records that
// as a compile-time visible fact.
func (o Outcome) VerdictResult() schema.VerdictResult {
	switch o {
	case OutcomePass:
		return schema.VerdictPass
	case OutcomeViolation:
		return schema.VerdictFail
	case OutcomeInconclusive, OutcomeHarnessError, OutcomeCanceled:
		return schema.VerdictInconclusive
	case OutcomeBudgetExhausted:
		return schema.VerdictBudgetExhausted
	case OutcomeConfigError:
		return schema.VerdictConfigError
	}
	// An outcome nobody declared is not a pass. Fail closed.
	return schema.VerdictInconclusive
}

// ExitCode maps an outcome to the normative CLI exit code (directive 4.7).
func (o Outcome) ExitCode() schema.ExitCode { return o.VerdictResult().ExitCode() }

// ExitOracleDriftIsPhase3 is the exit code this package never emits. It exists
// so the claim is greppable and so a test can assert it rather than trusting a
// comment.
const ExitOracleDriftIsPhase3 = schema.ExitOracleDrift

// String renders the outcome.
func (o Outcome) String() string { return string(o) }

// Describe returns a one-line human explanation of what the outcome means and
// what to do about it, for the CLI's non-JSON output.
func (o Outcome) Describe() string {
	switch o {
	case OutcomePass:
		return "all oracles satisfied"
	case OutcomeViolation:
		return "oracle violation; work from the violation's witness"
	case OutcomeInconclusive:
		return "an oracle could not evaluate; retry once, then escalate"
	case OutcomeHarnessError:
		return "harness or environment failure; retry once, then escalate"
	case OutcomeCanceled:
		return "interrupted before a verdict was reached"
	case OutcomeBudgetExhausted:
		return "budget expired before the planned worlds finished"
	case OutcomeConfigError:
		return "invalid configuration or arguments"
	}
	return fmt.Sprintf("unrecognised outcome %q, treated as inconclusive", string(o))
}
