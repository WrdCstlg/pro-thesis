package oracle

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// Oracle is one property check.
//
// The interface is shaped so that a Phase 3 external-process oracle implements
// it without the engine changing: Name, Class and ValidPhases are exactly the
// three fields prothesis.oracle_output/v1 requires an oracle to declare, and
// Evaluate returns the same three-valued status that contract defines.
//
// Implementations must treat Input as read-only.
type Oracle interface {
	// Name is the oracle's identity, e.g. "no_crash". It appears verbatim in
	// violation.oracle and is what .prothesis/lock will address in Phase 3, so
	// it is stable across versions.
	Name() string

	// Class is one of the eight schema.OracleClass values. It determines
	// severity through schema.DefaultSeverity, so it is declared by the oracle
	// and never chosen per finding.
	Class() schema.OracleClass

	// ValidPhases is the invariant I5 declaration: the lifecycle phases in
	// which evaluating this oracle is meaningful. The engine refuses to
	// evaluate outside them.
	//
	// It must not be empty. An oracle valid nowhere can never fire, and an
	// oracle that cannot fire is worse than none: it manufactures the
	// appearance of coverage.
	ValidPhases() []schema.Phase

	// Evaluate examines in and returns a Result.
	//
	// phase is the lifecycle phase the evaluation is being made in; it is
	// always one of ValidPhases, because the engine checks first.
	//
	// The error return is for the oracle MALFUNCTIONING, not for a property
	// being violated and not for evidence being absent. A violated property is
	// a Result, absent evidence is an inconclusive Result, and a malfunction is
	// an error that the engine records as inconclusive with the reason attached.
	Evaluate(ctx context.Context, phase schema.Phase, in *Input) (Result, error)
}

// ValidIn reports whether o declares itself meaningful in phase p.
func ValidIn(o Oracle, p schema.Phase) bool {
	for _, q := range o.ValidPhases() {
		if q == p {
			return true
		}
	}
	return false
}

// PhaseList renders an oracle's declared phases for a message.
func PhaseList(phases []schema.Phase) string {
	s := make([]string, 0, len(phases))
	for _, p := range phases {
		s = append(s, string(p))
	}
	return "[" + strings.Join(s, " ") + "]"
}

// ---------------------------------------------------------------------------
// findings
// ---------------------------------------------------------------------------

// Finding is one oracle's evaluation, with the declaration it was made under.
//
// The declaration is carried alongside the result so a verdict reader can see
// not just what was concluded but under what phase validity it was concluded,
// which is the difference between a finding and an anecdote.
type Finding struct {
	Oracle      string
	Class       schema.OracleClass
	ValidPhases []schema.Phase
	// EvaluatedIn is the phase the engine evaluated the oracle in.
	EvaluatedIn schema.Phase
	Result      Result
	// Err is set when the oracle malfunctioned. Result.Status is then
	// inconclusive; it is never ok.
	Err error
}

// Output renders the finding as prothesis.oracle_output/v1: the same document
// an external oracle writes on stdout. Built-in and external findings are
// therefore archivable in one format.
func (f Finding) Output() schema.OracleOutput {
	return schema.OracleOutput{
		Schema:      schema.OracleOutputSchema,
		Oracle:      f.Oracle,
		Class:       f.Class,
		ValidPhases: append([]schema.Phase(nil), f.ValidPhases...),
		Status:      f.Result.Status,
		Witness:     f.Result.Witness,
		Explanation: f.Result.Explanation,
	}
}

// ToViolation converts a violated finding into a verdict violation.
//
// Severity is assigned by schema.DefaultSeverity from the class: the single
// choke point, so consistency keeps its normative "high" and no oracle can
// grade itself down.
func (f Finding) ToViolation(id string) schema.Violation {
	out := f.Output()
	return out.ToViolation(id, f.Result.Phase, f.Result.FirstSeenMS)
}

// Findings is the set of evaluations made for one world.
type Findings []Finding

// Violations returns the findings that are violations, in evaluation order.
func (fs Findings) Violations() Findings {
	out := make(Findings, 0, len(fs))
	for _, f := range fs {
		if f.Result.IsViolation() {
			out = append(out, f)
		}
	}
	return out
}

// Inconclusive returns the findings that could not be evaluated.
func (fs Findings) Inconclusive() Findings {
	out := make(Findings, 0, len(fs))
	for _, f := range fs {
		if f.Result.IsInconclusive() {
			out = append(out, f)
		}
	}
	return out
}

// Worst is the most severe status in the set, ordered ok < inconclusive <
// violated.
//
// An EMPTY set is INCONCLUSIVE, not ok. A world in which no oracle ran has not
// been checked, and reporting PASS over it would be a claim about a property
// nobody evaluated.
func (fs Findings) Worst() schema.OracleStatus {
	if len(fs) == 0 {
		return schema.StatusInconclusive
	}
	worst := schema.StatusOK
	for _, f := range fs {
		worst = worst.Worse(f.Result.Status)
	}
	return worst
}

// ExitCode maps the set onto the normative CLI exit space (directive 4.7).
//
//	violated      -> 1 FAIL
//	inconclusive  -> 2 INCONCLUSIVE
//	all ok        -> 0 PASS
//	nothing ran   -> 2 INCONCLUSIVE
//
// It never returns 3, 4 or 5: budget, lock drift and config errors are the
// control plane's decisions, not the oracle engine's.
func (fs Findings) ExitCode() schema.ExitCode {
	switch fs.Worst() {
	case schema.StatusViolated:
		return schema.ExitFail
	case schema.StatusInconclusive:
		return schema.ExitInconclusive
	default:
		return schema.ExitPass
	}
}

// Verdict is the verdict string for the set, bijective with ExitCode.
func (fs Findings) Verdict() schema.VerdictResult { return fs.ExitCode().Verdict() }

// ToViolations renders every violated finding as a verdict violation, numbered
// v1, v2, ... in evaluation order.
func (fs Findings) ToViolations() []schema.Violation {
	vs := fs.Violations()
	out := make([]schema.Violation, 0, len(vs))
	for i, f := range vs {
		out = append(out, f.ToViolation(fmt.Sprintf("v%d", i+1)))
	}
	return out
}

// Summary is a one-line human account of the set, including the inconclusive
// count: which is the number a reader must never miss.
func (fs Findings) Summary() string {
	if len(fs) == 0 {
		return "no oracle was evaluated (INCONCLUSIVE: nothing was checked)"
	}
	var ok, viol, inc int
	for _, f := range fs {
		switch f.Result.Status {
		case schema.StatusViolated:
			viol++
		case schema.StatusInconclusive:
			inc++
		default:
			ok++
		}
	}
	return fmt.Sprintf("%d oracle(s): %d ok, %d violated, %d inconclusive -> %s",
		len(fs), ok, viol, inc, fs.Verdict())
}

// Names returns the evaluated oracle names, sorted.
func (fs Findings) Names() []string {
	out := make([]string, 0, len(fs))
	for _, f := range fs {
		out = append(out, f.Oracle)
	}
	sort.Strings(out)
	return out
}
