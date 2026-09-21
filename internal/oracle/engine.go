package oracle

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"sort"

	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// ErrPhaseInvalid is the sentinel behind every phase-validity refusal, so a
// caller can errors.Is it apart from an ordinary evaluation error.
var ErrPhaseInvalid = errors.New("oracle: evaluated outside its declared valid phases")

// PhaseError reports an attempt to evaluate an oracle in a phase it did not
// declare (invariant I5).
//
// This is an ENGINE BUG, never a finding. Checking consistency during an active
// partition manufactures false positives, so the engine refuses loudly instead
// of returning a result nobody should trust: a silently skipped evaluation
// would be just as bad, because the run would then report PASS over a property
// that was never checked.
type PhaseError struct {
	Oracle string
	Phase  schema.Phase
	Valid  []schema.Phase
}

func (e *PhaseError) Error() string {
	return fmt.Sprintf("oracle %q was asked to evaluate in phase %s but declares %s; "+
		"this is a control-plane bug, not a violation", e.Oracle, e.Phase, PhaseList(e.Valid))
}

func (e *PhaseError) Unwrap() error { return ErrPhaseInvalid }

// ---------------------------------------------------------------------------
// engine
// ---------------------------------------------------------------------------

// Engine holds the registered oracles and evaluates them under the phase
// validity rule.
//
// It is not safe for concurrent registration; evaluation of a registered set is
// read-only and may be shared.
type Engine struct {
	oracles []Oracle
	index   map[string]int
}

// NewEngine returns an empty engine.
func NewEngine() *Engine { return &Engine{index: map[string]int{}} }

// Register adds an oracle.
//
// Registration is strict, because every rejection here is a defect that would
// otherwise surface as a malformed verdict: a nameless oracle, a duplicate
// name, a class outside the eight, or (most importantly) an empty or invalid
// phase declaration. An oracle valid nowhere can never fire and would sit in
// the config looking like coverage.
func (e *Engine) Register(o Oracle) error {
	if o == nil {
		return errors.New("oracle: cannot register a nil oracle")
	}
	name := o.Name()
	if name == "" {
		return errors.New("oracle: cannot register an oracle with no name")
	}
	if _, dup := e.index[name]; dup {
		return fmt.Errorf("oracle: %q is already registered", name)
	}
	if !o.Class().Valid() {
		return fmt.Errorf("oracle %q: class %q is not one of %v", name, o.Class(), schema.AllOracleClasses)
	}
	phases := o.ValidPhases()
	if len(phases) == 0 {
		return fmt.Errorf("oracle %q: declares no valid phases, so it could never be evaluated "+
			"(invariant I5 requires an explicit declaration)", name)
	}
	seen := map[schema.Phase]bool{}
	for _, p := range phases {
		if !p.Valid() {
			return fmt.Errorf("oracle %q: %q is not one of the eight lifecycle phases %v", name, p, schema.AllPhases)
		}
		if seen[p] {
			return fmt.Errorf("oracle %q: phase %s is declared twice", name, p)
		}
		seen[p] = true
	}
	if e.index == nil {
		e.index = map[string]int{}
	}
	e.index[name] = len(e.oracles)
	e.oracles = append(e.oracles, o)
	return nil
}

// RegisterAll registers a list, stopping at the first rejection.
func (e *Engine) RegisterAll(os []Oracle) error {
	for _, o := range os {
		if err := e.Register(o); err != nil {
			return err
		}
	}
	return nil
}

// Len is the number of registered oracles.
func (e *Engine) Len() int { return len(e.oracles) }

// Oracles returns the registered oracles in registration order.
func (e *Engine) Oracles() []Oracle { return append([]Oracle(nil), e.oracles...) }

// Lookup returns a registered oracle by name.
func (e *Engine) Lookup(name string) (Oracle, bool) {
	i, ok := e.index[name]
	if !ok {
		return nil, false
	}
	return e.oracles[i], true
}

// Names returns the registered names, sorted.
func (e *Engine) Names() []string {
	out := make([]string, 0, len(e.oracles))
	for _, o := range e.oracles {
		out = append(out, o.Name())
	}
	sort.Strings(out)
	return out
}

// SelectFor partitions the registered oracles by whether they are valid in p.
//
// SELECTING is not ASKING. Skipping an oracle that does not apply to a phase is
// correct behaviour and is how a phase-aware run works; evaluating one that
// does not apply is the bug PhaseError reports. The skipped list is returned so
// the control plane can record what was not checked rather than let it vanish.
func (e *Engine) SelectFor(p schema.Phase) (run, skipped []Oracle) {
	for _, o := range e.oracles {
		if ValidIn(o, p) {
			run = append(run, o)
		} else {
			skipped = append(skipped, o)
		}
	}
	return run, skipped
}

// EvaluateOracleAt evaluates one oracle in one phase.
//
// It REFUSES with a *PhaseError when the oracle does not declare p. That
// refusal is the enforcement point for invariant I5 and it is intentionally
// impossible to ignore: no Finding is returned with it.
func (e *Engine) EvaluateOracleAt(ctx context.Context, o Oracle, p schema.Phase, in *Input) (Finding, error) {
	if o == nil {
		return Finding{}, errors.New("oracle: nil oracle")
	}
	if !p.Valid() {
		return Finding{}, fmt.Errorf("oracle: %q is not one of the eight lifecycle phases %v", p, schema.AllPhases)
	}
	if !ValidIn(o, p) {
		return Finding{}, &PhaseError{Oracle: o.Name(), Phase: p, Valid: o.ValidPhases()}
	}
	if err := ctx.Err(); err != nil {
		return Finding{}, err
	}

	res, err := safeEvaluate(ctx, o, p, in)
	f := Finding{
		Oracle:      o.Name(),
		Class:       o.Class(),
		ValidPhases: append([]schema.Phase(nil), o.ValidPhases()...),
		EvaluatedIn: p,
		Err:         err,
	}
	f.Result = normalizeResult(o, p, in, res, err)
	return f, nil
}

// EvaluateAt evaluates every registered oracle that is valid in p.
//
// The returned Findings are in registration order. An error is returned only
// for a condition that invalidates the whole evaluation: a cancelled context,
// or a phase outside the eight. A single oracle malfunctioning does not abort
// the set; it becomes an inconclusive finding, because losing four good
// oracles' results to one bad one helps nobody.
func (e *Engine) EvaluateAt(ctx context.Context, p schema.Phase, in *Input) (Findings, error) {
	if !p.Valid() {
		return nil, fmt.Errorf("oracle: %q is not one of the eight lifecycle phases %v", p, schema.AllPhases)
	}
	run, _ := e.SelectFor(p)
	out := make(Findings, 0, len(run))
	for _, o := range run {
		f, err := e.EvaluateOracleAt(ctx, o, p, in)
		if err != nil {
			// Only a context error or an engine bug reaches here; SelectFor has
			// already established phase validity.
			return out, err
		}
		out = append(out, f)
	}
	return out, nil
}

// safeEvaluate runs an oracle and converts a panic into an error.
//
// A built-in that panics is a defect in this package, but it must not take the
// run down: the other oracles' findings are still worth having, and the panic
// itself must reach the verdict as INCONCLUSIVE with its stack, never as a
// silent pass.
func safeEvaluate(ctx context.Context, o Oracle, p schema.Phase, in *Input) (res Result, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("oracle %q panicked in phase %s: %v\n%s", o.Name(), p, r, debug.Stack())
			res = Result{}
		}
	}()
	return o.Evaluate(ctx, p, in)
}

// normalizeResult applies the fail-closed presentation rules.
//
// It never downgrades a finding: a violation stays a violation even when the
// oracle also erred or forgot its explanation. Everything else that is not a
// clean ok becomes inconclusive with the reason attached.
func normalizeResult(o Oracle, p schema.Phase, in *Input, res Result, err error) Result {
	if err != nil && res.Status != schema.StatusViolated {
		res.Status = schema.StatusInconclusive
		res.Explanation = joinExplanation(res.Explanation, err.Error())
	}
	if !res.Status.Valid() {
		res.Explanation = joinExplanation(res.Explanation,
			fmt.Sprintf("oracle %q returned status %q, which is not one of %v; "+
				"recorded as inconclusive rather than ok", o.Name(), res.Status, schema.AllOracleStatuses))
		res.Status = schema.StatusInconclusive
	}
	switch res.Status {
	case schema.StatusViolated:
		if res.Explanation == "" {
			res.Explanation = fmt.Sprintf("oracle %q reported a violation without an explanation", o.Name())
		}
	case schema.StatusInconclusive:
		if res.Explanation == "" {
			res.Explanation = fmt.Sprintf("oracle %q reported inconclusive without a reason", o.Name())
		}
	}
	if res.Phase == "" {
		res.Phase = observedPhase(in, res, p)
	}
	return res
}

// observedPhase places a finding on the lifecycle.
//
// Directive 4.6's own example carries "phase": "DRIVE" with first_seen_ms
// 14320 on a violation reported from a completed run, so the field means "the
// phase the evidence falls in", not "the phase the oracle ran in". When several
// windows cover the moment (PERTURB is contained within DRIVE) the earliest
// in lifecycle order wins, which is what that example does. With no timings to
// resolve against, the evaluation phase is the honest answer.
func observedPhase(in *Input, res Result, evaluatedIn schema.Phase) schema.Phase {
	if in == nil || res.Status == schema.StatusOK {
		return evaluatedIn
	}
	candidates := in.Phases.At(res.FirstSeenMS)
	if len(candidates) == 0 {
		return evaluatedIn
	}
	best := candidates[0]
	for _, c := range candidates[1:] {
		if c.Ordinal() < best.Ordinal() {
			best = c
		}
	}
	return best
}

func joinExplanation(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	default:
		return a + "; " + b
	}
}
