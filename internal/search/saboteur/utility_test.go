package saboteur

import (
	"errors"
	"math"
	"testing"

	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

func closeTo(t *testing.T, got, want float64, what string) {
	t.Helper()
	if math.Abs(got-want) > 1e-9 {
		t.Fatalf("%s = %v, want %v", what, got, want)
	}
}

func violation(class schema.OracleClass, sev schema.Severity) schema.Violation {
	return schema.Violation{Oracle: "test." + string(class), Class: class, Severity: sev}
}

// A.6's own "Key property" paragraph, transcribed as a test. Both numbers come
// from the specification text, not from running this code:
//
//	"A world that finds a consistency violation using 2 faults in 30 seconds
//	 scores 100x1.5 + 0 - 6 - 3 = 141. A world that finds the same violation
//	 using 12 faults in 120 seconds scores 100x1.5 + 0 - 36 - 12 = 102."
func TestUtilityMatchesA6WorkedExample(t *testing.T) {
	w := DefaultWeights()
	viol := []schema.Violation{violation(schema.ClassConsistency, schema.SeverityHigh)}

	minimal := WorldResult{Violations: viol, FaultCount: 2, DurationSeconds: 30}
	closeTo(t, Utility(minimal, w), 141, "A.6's minimal world")

	wasteful := WorldResult{Violations: viol, FaultCount: 12, DurationSeconds: 120}
	closeTo(t, Utility(wasteful, w), 102, "A.6's wasteful world")
}

// D-028 item 3. A.6's prose claims "The Saboteur will always prefer the minimal
// path". It does not. This test pins the counterexample with the exact numbers
// from the decision record, so that a later reader who believes the prose cannot
// "fix" Utility without this failing in their face.
//
// The weights are NORMATIVE and are deliberately left alone. If minimality is
// genuinely wanted, fault_penalty must exceed novelty_weight: a change only the
// specification's author should make (OQ-014 item 2).
func TestParsimonyClaimIsFalse(t *testing.T) {
	w := DefaultWeights()
	viol := []schema.Violation{violation(schema.ClassConsistency, schema.SeverityHigh)}

	minimal := WorldResult{Violations: viol, FaultCount: 2, DurationSeconds: 30}
	novel := WorldResult{
		Violations:      viol,
		Coverage:        schema.Coverage{NewTemplates: 5},
		FaultCount:      12,
		DurationSeconds: 120,
	}

	// 150 + 0 - 6 - 3
	closeTo(t, Utility(minimal, w), 141, "minimal world")
	// 150 + 50 - 36 - 12
	closeTo(t, Utility(novel, w), 152, "12-fault world with 5 novel templates")

	if Utility(novel, w) <= Utility(minimal, w) {
		t.Fatalf("this test exists BECAUSE the wasteful world outscores the minimal one; "+
			"if that has stopped being true the weights were changed: %v vs %v",
			Utility(novel, w), Utility(minimal, w))
	}

	// The exchange rate behind it, as a number rather than a claim.
	closeTo(t, FaultsPerNovelTemplate(w), 10.0/3.0, "faults bought per novel template")
	if FaultsPerNovelTemplate(w) <= 1 {
		t.Fatalf("a rate <= 1 would mean parsimony dominates and A.6's prose is true; got %v",
			FaultsPerNovelTemplate(w))
	}
}

// D-028 item 2. `differential` and `metamorphic` are normative OracleClass
// values that A.6's switch omits, so under the literal text they contribute
// exactly 0.0 and a violation of either is invisible to the search: the
// Saboteur cannot see a bug it just found. The added default scores them at the
// lowest named tier.
func TestDefaultBranchScoresTheTwoOmittedClasses(t *testing.T) {
	w := DefaultWeights()
	for _, class := range []schema.OracleClass{schema.ClassDifferential, schema.ClassMetamorphic} {
		r := WorldResult{Violations: []schema.Violation{violation(class, schema.SeverityMedium)}}
		got := Utility(r, w)
		if got == 0 {
			t.Fatalf("%s scored 0: A.6's switch has no default and the violation is invisible "+
				"to the search", class)
		}
		closeTo(t, got, 20.0, string(class)+" at severity medium")

		b := Score(r, w)
		if len(b.PerViolation) != 1 || !b.PerViolation[0].Defaulted {
			t.Fatalf("%s must be marked as having fallen to the default branch, so D-028 item 2 "+
				"is visible in output: %+v", class, b.PerViolation)
		}
	}
}

// An entirely unknown class (a third-party oracle emitting something outside
// the frozen vocabulary) must also be visible rather than free.
func TestAnUnknownClassIsScoredAtTheLowestTierNotZero(t *testing.T) {
	w := DefaultWeights()
	r := WorldResult{Violations: []schema.Violation{violation(schema.OracleClass("wat"), schema.SeverityMedium)}}
	closeTo(t, Utility(r, w), 20.0, "unknown class")
}

func TestClassRatiosReproduceA6Literals(t *testing.T) {
	w := DefaultWeights()
	for _, tc := range []struct {
		class schema.OracleClass
		want  float64
	}{
		{schema.ClassConsistency, 100},
		{schema.ClassCrash, 80},
		{schema.ClassSafety, 60},
		{schema.ClassResource, 40},
		{schema.ClassLiveness, 20},
		{schema.ClassConvergence, 20},
	} {
		r := WorldResult{Violations: []schema.Violation{violation(tc.class, schema.SeverityMedium)}}
		closeTo(t, Utility(r, w), tc.want, string(tc.class)+" at multiplier 1.0")
	}
}

func TestSeverityMultipliersAreA6sTable(t *testing.T) {
	for _, tc := range []struct {
		sev  schema.Severity
		want float64
	}{
		{schema.SeverityHigh, 1.5},
		{schema.SeverityMedium, 1.0},
		{schema.SeverityLow, 0.5},
		{schema.Severity(""), 1.0},
		{schema.Severity("nonsense"), 1.0},
	} {
		closeTo(t, SeverityMultiplier(tc.sev), tc.want, "severityMultiplier("+string(tc.sev)+")")
	}
}

// A.6's severityMultiplier has no `critical` case, so critical takes the default
// of 1.0 and a CRITICAL violation scores LESS than a HIGH one. The v1
// class-to-severity table never emits critical, so this is unreachable from any
// built-in oracle, but a third-party oracle overriding severity can reach it.
//
// Transcribed as specified rather than quietly extended; pinned here so the
// anomaly is a recorded fact rather than a surprise, and reported as an open
// question.
func TestSeverityCriticalScoresBelowHigh(t *testing.T) {
	w := DefaultWeights()
	crit := WorldResult{Violations: []schema.Violation{violation(schema.ClassConsistency, schema.SeverityCritical)}}
	high := WorldResult{Violations: []schema.Violation{violation(schema.ClassConsistency, schema.SeverityHigh)}}
	closeTo(t, Utility(crit, w), 100, "critical consistency violation")
	closeTo(t, Utility(high, w), 150, "high consistency violation")
	if Utility(crit, w) >= Utility(high, w) {
		t.Fatalf("A.6's table gives critical the default multiplier of 1.0; if this now passes, " +
			"the table was extended and DECISIONS.md must say so")
	}
}

func TestNoveltyRewardsAreTenPerTemplateAndFivePerState(t *testing.T) {
	w := DefaultWeights()
	r := WorldResult{Coverage: schema.Coverage{NewTemplates: 3, NewStates: 4}}
	// 3*10 + 4*5
	closeTo(t, Utility(r, w), 50, "novelty only")
}

func TestParsimonyPenaltiesAreThreePerFaultAndATenthPerSecond(t *testing.T) {
	w := DefaultWeights()
	r := WorldResult{FaultCount: 4, DurationSeconds: 30}
	// -(4*3) - (30*0.1)
	closeTo(t, Utility(r, w), -15, "parsimony only")
}

// The weights must come from `search.utility`, and the shipped defaults must
// reproduce A.6's literals exactly. If schema's defaults ever move, this fails.
func TestWeightsFromShippedDefaultsAreA6Literals(t *testing.T) {
	got, err := WeightsFrom(schema.DefaultConfig().Search.Utility)
	if err != nil {
		t.Fatalf("WeightsFrom(defaults): %v", err)
	}
	if got != DefaultWeights() {
		t.Fatalf("WeightsFrom(shipped defaults) = %+v, want %+v", got, DefaultWeights())
	}
	closeTo(t, got.ViolationBase, 100, "violation base")
	closeTo(t, got.NoveltyTemplate, 10, "novelty per template")
	closeTo(t, got.NoveltyState, 5, "novelty per state")
	closeTo(t, got.FaultPenalty, 3, "fault penalty")
	closeTo(t, got.DurationPenalty, 0.1, "duration penalty")
}

func TestWeightsAreConfigurable(t *testing.T) {
	got, err := WeightsFrom(schema.UtilityConfig{
		ViolationWeight: 200, NoveltyWeight: 4, FaultPenalty: 7, DurationPenalty: 0.5,
	})
	if err != nil {
		t.Fatalf("WeightsFrom: %v", err)
	}
	r := WorldResult{
		Violations:      []schema.Violation{violation(schema.ClassCrash, schema.SeverityLow)},
		Coverage:        schema.Coverage{NewTemplates: 1, NewStates: 1},
		FaultCount:      2,
		DurationSeconds: 10,
	}
	// 200*0.8*0.5 + 4 + 2 - 14 - 5
	closeTo(t, Utility(r, got), 80+4+2-14-5, "configured weights")
}

// A hand-built schema.Config has a zero UtilityConfig, and a Saboteur whose
// violation reward is zero ranks a world that found a consistency bug exactly
// equal to one that found nothing. That is the #1 failure mode for this phase
// arriving through a struct literal, so it is refused loudly.
func TestWeightsFromRefusesAZeroViolationWeight(t *testing.T) {
	if _, err := WeightsFrom(schema.UtilityConfig{}); !errors.Is(err, ErrUnusableWeights) {
		t.Fatalf("a zero utility block must be refused, got err=%v", err)
	}
	if _, err := WeightsFrom(schema.UtilityConfig{ViolationWeight: 100, FaultPenalty: -1}); !errors.Is(err, ErrUnusableWeights) {
		t.Fatalf("a negative penalty pays the Saboteur to be wasteful and must be refused, got err=%v", err)
	}
	// Switching novelty off entirely is a coherent request and is accepted.
	if _, err := WeightsFrom(schema.UtilityConfig{ViolationWeight: 100, NoveltyWeight: 0}); err != nil {
		t.Fatalf("novelty_weight: 0 is a legitimate setting: %v", err)
	}
}

// Turning novelty off must turn it off for STATES too. That is the whole reason
// the state reward is a ratio of the template reward rather than a hardcoded 5.
func TestZeroNoveltyWeightAlsoZeroesStateNovelty(t *testing.T) {
	w, err := WeightsFrom(schema.UtilityConfig{
		ViolationWeight: 100, NoveltyWeight: 0, FaultPenalty: 3, DurationPenalty: 0.1,
	})
	if err != nil {
		t.Fatalf("WeightsFrom: %v", err)
	}
	r := WorldResult{Coverage: schema.Coverage{NewTemplates: 9, NewStates: 9}}
	closeTo(t, Utility(r, w), 0, "novelty switched off")
}

func TestScoreItemisesTheArithmetic(t *testing.T) {
	w := DefaultWeights()
	b := Score(WorldResult{
		Violations: []schema.Violation{
			violation(schema.ClassConsistency, schema.SeverityHigh),
			violation(schema.ClassLiveness, schema.SeverityLow),
		},
		Coverage:        schema.Coverage{NewTemplates: 2, NewStates: 1},
		FaultCount:      3,
		DurationSeconds: 20,
	}, w)

	closeTo(t, b.Violation, 150+10, "violation term")
	closeTo(t, b.Novelty, 25, "novelty term")
	closeTo(t, b.Parsimony, -11, "parsimony term")
	closeTo(t, b.Total, 150+10+25-11, "total")
	if b.Total != Utility(WorldResult{
		Violations: []schema.Violation{
			violation(schema.ClassConsistency, schema.SeverityHigh),
			violation(schema.ClassLiveness, schema.SeverityLow),
		},
		Coverage:        schema.Coverage{NewTemplates: 2, NewStates: 1},
		FaultCount:      3,
		DurationSeconds: 20,
	}, w) {
		t.Fatal("Score().Total and Utility() must agree")
	}
	if len(b.PerViolation) != 2 {
		t.Fatalf("PerViolation = %d entries, want 2", len(b.PerViolation))
	}
	for _, v := range b.PerViolation {
		if v.Defaulted {
			t.Fatalf("%s is named explicitly in A.6's switch and must not be marked defaulted", v.Class)
		}
	}
}

// A world with nothing to report scores nothing, not a small positive number.
func TestAnEmptyWorldScoresZero(t *testing.T) {
	closeTo(t, Utility(WorldResult{}, DefaultWeights()), 0, "empty world")
}
