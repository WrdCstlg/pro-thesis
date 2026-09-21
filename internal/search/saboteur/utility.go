package saboteur

import (
	"errors"
	"fmt"
	"math"

	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// ---------------------------------------------------------------------------
// A.6: the payoff function
// ---------------------------------------------------------------------------
//
// Addendum A.6 is labelled normative: "must not be modified by the implementing
// agent without recording the change in DECISIONS.md". D-028 is that record.
// Three things are done differently from the literal text and each is recorded
// there:
//
//	1. A `default` branch scoring the lowest named tier (20.0). A.6's switch
//	   covers six of the eight normative OracleClass values; `differential` and
//	   `metamorphic` fall through and contribute exactly 0.0, so a violation of
//	   either is INVISIBLE to the search. The Saboteur cannot see a bug it just
//	   found, which cannot be the intent. (D-028 item 2.)
//
//	2. The weights are configurable from `search.utility` and DEFAULT to A.6's
//	   literals. A.10 defines the block; A.6 writes the numbers. See Weights.
//
//	3. Nothing else. In particular the RATIO between novelty and parsimony is
//	   left exactly as specified even though A.6's prose draws a false
//	   conclusion from it — see the "Parsimony" section below.

// Class ratios.
//
// A.6 writes the violation rewards as absolute numbers (100 consistency,
// 80 crash, 60 safety, 40 resource, 20 liveness/convergence) while A.10 exposes
// a single `utility.violation_weight` documented as the "base reward for oracle
// violation" with default 100. The only reading that makes both true is that the
// class table is a RATIO of that base. At the default base of 100 these
// reproduce A.6's literals exactly.
const (
	RatioConsistency = 1.0
	RatioCrash       = 0.8
	RatioSafety      = 0.6
	RatioResource    = 0.4

	// RatioLowest is A.6's 20.0 tier: liveness, convergence, AND the default
	// branch D-028 item 2 adds. An unrecognised or absent class is scored here
	// rather than at zero.
	RatioLowest = 0.2
)

// stateNoveltyRatio is A.6's 5-per-novel-state against its 10-per-novel-template.
//
// A.10 gives `novelty_weight` for templates and no key at all for states, so the
// state reward is expressed as a ratio of the template reward. Hardcoding 5.0
// instead was rejected: a user who sets `novelty_weight: 0` to switch novelty
// off would otherwise still be paying 5 points per novel state, which is the
// opposite of what they asked for.
const stateNoveltyRatio = 0.5

// Weights are A.6's payoff coefficients.
//
// Build one with DefaultWeights (A.6's literals) or WeightsFrom (the user's
// `search.utility` block). The zero value is deliberately NOT usable: see
// WeightsFrom.
type Weights struct {
	// ViolationBase is the reward for a consistency violation at severity
	// multiplier 1.0. Every other class is a ratio of it.
	ViolationBase float64
	// NoveltyTemplate is the reward per novel log template.
	NoveltyTemplate float64
	// NoveltyState is the reward per novel state abstraction.
	NoveltyState float64
	// FaultPenalty is the cost per fault injected.
	FaultPenalty float64
	// DurationPenalty is the cost per second of world execution.
	DurationPenalty float64
}

// DefaultWeights returns A.6's literals: 100 / 10 / 5 / 3 / 0.1.
func DefaultWeights() Weights {
	return Weights{
		ViolationBase:   float64(schema.DefaultViolationWeight),
		NoveltyTemplate: float64(schema.DefaultNoveltyWeight),
		NoveltyState:    float64(schema.DefaultNoveltyWeight) * stateNoveltyRatio,
		FaultPenalty:    float64(schema.DefaultFaultPenalty),
		DurationPenalty: schema.DefaultDurationPenalty,
	}
}

// ErrUnusableWeights reports a `search.utility` block the Saboteur cannot search
// with.
var ErrUnusableWeights = errors.New("saboteur: unusable utility weights")

// WeightsFrom builds the coefficients from `search.utility`.
//
// It REFUSES a non-positive violation weight. schema.DecodeConfig always decodes
// over DefaultConfig so a real prothesis.yaml cannot produce one, but a caller
// that hand-builds a schema.Config gets the zero value, and a Saboteur whose
// violation reward is zero scores a world that found a consistency bug exactly
// equal to one that found nothing. That is the ranked #1 failure mode for this
// phase (a search that learns from noise) arriving through a struct literal, so
// it fails loudly here rather than silently ranking garbage for ten minutes.
//
// A zero novelty or parsimony weight is accepted: switching either term off is a
// coherent thing to ask for.
func WeightsFrom(u schema.UtilityConfig) (Weights, error) {
	if u.ViolationWeight <= 0 {
		return Weights{}, fmt.Errorf("%w: search.utility.violation_weight is %d; a Saboteur that "+
			"scores every violation at zero ranks a world that found a bug equal to one that found "+
			"nothing (A.10's default is %d)",
			ErrUnusableWeights, u.ViolationWeight, schema.DefaultViolationWeight)
	}
	if u.NoveltyWeight < 0 || u.FaultPenalty < 0 || u.DurationPenalty < 0 {
		return Weights{}, fmt.Errorf("%w: novelty_weight=%d fault_penalty=%d duration_penalty=%v; "+
			"none may be negative (a negative penalty pays the Saboteur to be wasteful)",
			ErrUnusableWeights, u.NoveltyWeight, u.FaultPenalty, u.DurationPenalty)
	}
	return Weights{
		ViolationBase:   float64(u.ViolationWeight),
		NoveltyTemplate: float64(u.NoveltyWeight),
		NoveltyState:    float64(u.NoveltyWeight) * stateNoveltyRatio,
		FaultPenalty:    float64(u.FaultPenalty),
		DurationPenalty: u.DurationPenalty,
	}, nil
}

// WorldResult is A.6's `result`: everything the payoff function reads about one
// executed world.
//
// It reuses the frozen verdict types rather than redeclaring them, so a class or
// severity the engine produced is the exact value scored here.
type WorldResult struct {
	// Violations is what the oracle engine reported for this world.
	Violations []schema.Violation
	// Coverage carries the novelty deltas. Only NewTemplates and NewStates are
	// read; the cumulative figures are not part of the payoff.
	Coverage schema.Coverage
	// FaultCount is the number of faults the world actually injected.
	FaultCount int
	// DurationSeconds is wall-clock execution time.
	DurationSeconds float64
}

// ClassRatio is the multiple of ViolationBase a violation of this class earns.
//
// The `default` return is D-028 item 2: `differential` and `metamorphic` are
// normative OracleClass values that A.6's switch omits, and an unrecognised
// class scoring zero would make a real finding invisible to the search. They are
// scored at the lowest named tier, not at zero.
func ClassRatio(c schema.OracleClass) float64 {
	switch c {
	case schema.ClassConsistency:
		return RatioConsistency
	case schema.ClassCrash:
		return RatioCrash
	case schema.ClassSafety:
		return RatioSafety
	case schema.ClassResource:
		return RatioResource
	case schema.ClassLiveness, schema.ClassConvergence:
		return RatioLowest
	default:
		return RatioLowest
	}
}

// classIsNamed reports whether A.6's switch names this class explicitly. Used
// only to mark a scored violation as having fallen to the default branch, so the
// fact is visible in output instead of being inferred.
func classIsNamed(c schema.OracleClass) bool {
	switch c {
	case schema.ClassConsistency, schema.ClassCrash, schema.ClassSafety,
		schema.ClassResource, schema.ClassLiveness, schema.ClassConvergence:
		return true
	}
	return false
}

// SeverityMultiplier is A.6's severityMultiplier, transcribed exactly: high 1.5,
// medium 1.0, low 0.5, default 1.0.
//
// NOTE, and it is a real one: schema.SeverityCritical therefore takes the
// default and scores 1.0, LESS than SeverityHigh's 1.5. The v1 class-to-severity
// table (schema.DefaultSeverity) never returns `critical`, so this is
// unreachable from any built-in oracle; it is reachable from a third-party
// oracle that overrides severity in its output. A.6's table is normative and is
// transcribed as written rather than quietly extended: the anomaly is pinned by
// TestSeverityCriticalScoresBelowHigh and reported as an open question.
func SeverityMultiplier(s schema.Severity) float64 {
	switch s {
	case schema.SeverityHigh:
		return 1.5
	case schema.SeverityMedium:
		return 1.0
	case schema.SeverityLow:
		return 0.5
	default:
		return 1.0
	}
}

// ViolationScore is one violation's contribution, itemised.
type ViolationScore struct {
	Oracle     string
	Class      schema.OracleClass
	Severity   schema.Severity
	Ratio      float64
	Multiplier float64
	Points     float64
	// Defaulted is true when Class fell to ClassRatio's default branch. It
	// exists so D-028 item 2 is VISIBLE in an explanation rather than being a
	// silent behaviour of the switch.
	Defaulted bool
}

// Breakdown is Utility's arithmetic, itemised, so a ranking decision can be
// explained without re-deriving it.
type Breakdown struct {
	Violation    float64
	Novelty      float64
	Parsimony    float64 // always <= 0
	Total        float64
	PerViolation []ViolationScore
}

// Utility is A.6's payoff function.
//
//	U = sum(R_class * severity) + 10*new_templates + 5*new_states
//	    - 3*fault_count - 0.1*duration_seconds
//
// # The parsimony claim in A.6's prose is FALSE, and the weights are still
// # exactly as specified
//
// A.6's "Key property" paragraph argues that the Saboteur "will always prefer
// the minimal path". It does not, and the counterexample is small: a novel log
// template is worth 10 and a fault costs 3, so a 12-fault world that turned up
// 5 novel templates scores
//
//	150 + 50 - 36 - 12 = 152
//
// against a 2-fault world finding the same violation:
//
//	150 + 0 - 6 - 3 = 141
//
// Novelty outweighs parsimony at 3.33 faults per novel template. The arithmetic
// inside A.6's own paragraph is right for the two worlds it compares; the
// general claim it draws from them is wrong.
//
// The weights are NOT changed here. They are normative, and the behaviour may
// well be intended: novelty is what drives coverage, and a search that never
// paid for exploration would stop finding new failure surface. What is corrected
// is the CLAIM. If minimality is genuinely required, `fault_penalty` must exceed
// `novelty_weight`, and that is a change only the specification's author should
// make (D-028 item 3, OQ-014 item 2, awaiting a human ruling).
//
// TestParsimonyClaimIsFalse pins the 152-vs-141 numbers precisely so that a
// later reader who believes the prose cannot "fix" this function without a test
// failing in their face.
func Utility(r WorldResult, w Weights) float64 {
	return Score(r, w).Total
}

// Score is Utility with the arithmetic itemised.
func Score(r WorldResult, w Weights) Breakdown {
	var b Breakdown
	b.PerViolation = make([]ViolationScore, 0, len(r.Violations))

	for _, v := range r.Violations {
		ratio := ClassRatio(v.Class)
		mult := SeverityMultiplier(v.Severity)
		pts := w.ViolationBase * ratio * mult
		b.Violation += pts
		b.PerViolation = append(b.PerViolation, ViolationScore{
			Oracle:     v.Oracle,
			Class:      v.Class,
			Severity:   v.Severity,
			Ratio:      ratio,
			Multiplier: mult,
			Points:     pts,
			Defaulted:  !classIsNamed(v.Class),
		})
	}

	b.Novelty = w.NoveltyTemplate*float64(r.Coverage.NewTemplates) +
		w.NoveltyState*float64(r.Coverage.NewStates)

	b.Parsimony = -(w.FaultPenalty*float64(r.FaultCount) + w.DurationPenalty*r.DurationSeconds)

	b.Total = b.Violation + b.Novelty + b.Parsimony
	return b
}

// FaultsPerNovelTemplate is the exchange rate between the two terms A.6's prose
// claims are ordered: how many extra faults one novel log template pays for.
//
// It exists so the tension is a computed number in the codebase rather than a
// claim in a comment. At A.6's literals it is 10/3 = 3.33. A value <= 1 would
// mean parsimony genuinely dominates and A.6's "Key property" paragraph would be
// true as written.
func FaultsPerNovelTemplate(w Weights) float64 {
	if w.FaultPenalty == 0 {
		// No parsimony pressure at all: one template pays for unboundedly many
		// faults. Reported as +Inf rather than dividing by zero silently.
		if w.NoveltyTemplate == 0 {
			return 0
		}
		return math.Inf(1)
	}
	return w.NoveltyTemplate / w.FaultPenalty
}
