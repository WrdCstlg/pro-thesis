package schema

import (
	"math"
	"strings"
	"testing"
)

func TestClassifyIsTheTwoByTwoAndNothingElse(t *testing.T) {
	cases := []struct {
		expected, observed VerdictResult
		want               CalibrationClass
	}{
		{VerdictPass, VerdictPass, ClassTruePass},
		{VerdictPass, VerdictFail, ClassFalseRejection},
		{VerdictFail, VerdictFail, ClassTrueReject},
		{VerdictFail, VerdictPass, ClassEscape},
		{VerdictPass, VerdictInconclusive, ClassUndecided},
		{VerdictFail, VerdictBudgetExhausted, ClassUndecided},
		{VerdictFail, VerdictOracleDrift, ClassUndecided},
		{VerdictInconclusive, VerdictPass, ClassUndecided},
	}
	for _, c := range cases {
		if got := Classify(c.expected, c.observed); got != c.want {
			t.Errorf("Classify(expected %s, observed %s) = %s, want %s", c.expected, c.observed, got, c.want)
		}
	}
}

// A negative-pole arm names the oracle that must catch the planted defect. A
// FAIL raised only by some other oracle did not show that oracle catching it,
// so it is an escape of the target, not a true rejection.
func TestATargetOracleMustBeTheOneThatRejects(t *testing.T) {
	v := Verdict{Verdict: VerdictFail, Violations: []Violation{{ID: "v1", Oracle: "no_stuck_op"}}}
	if got := ClassifyVerdict(VerdictFail, &v, "linearizable.kv"); got != ClassEscape {
		t.Fatalf("FAIL by another oracle = %s, want %s", got, ClassEscape)
	}
	v.Violations = append(v.Violations, Violation{ID: "v2", Oracle: "linearizable.kv"})
	if got := ClassifyVerdict(VerdictFail, &v, "linearizable.kv"); got != ClassTrueReject {
		t.Fatalf("FAIL by the target oracle = %s, want %s", got, ClassTrueReject)
	}
	if got := ClassifyVerdict(VerdictFail, &v, ""); got != ClassTrueReject {
		t.Fatalf("with no target any violation counts: %s", got)
	}
}

func approx(t *testing.T, name string, got *float64, want float64) {
	t.Helper()
	if got == nil {
		t.Fatalf("%s = nil, want %.4f", name, want)
	}
	if math.Abs(*got-want) > 1e-9 {
		t.Fatalf("%s = %.6f, want %.6f", name, *got, want)
	}
}

func TestRatesAndPPVFromCounts(t *testing.T) {
	c := CalibrationCounts{TrueReject: 9, Escape: 1, TruePass: 19, FalseRejection: 1, Undecided: 4}
	r := c.Rates()
	approx(t, "sensitivity", r.Sensitivity, 0.9)
	approx(t, "specificity", r.Specificity, 0.95)
	approx(t, "false_rejection_rate", r.FalseRejectionRate, 0.05)

	ppv := PPVByBaseRate(r, DefaultBaseRates)
	if len(ppv) != 4 {
		t.Fatalf("%d PPV points, want one for each of 0.20, 0.10, 0.042, 0.01", len(ppv))
	}
	// Se*b / (Se*b + (1-Sp)*(1-b)), worked by hand: at b=0.10, 0.09 / (0.09 + 0.045).
	approx(t, "ppv@0.10", ppv[1].PPV, 0.09/0.135)
	approx(t, "ppv@0.042", ppv[2].PPV, 0.9*0.042/(0.9*0.042+0.05*0.958))
}

// No denominator, no number: a rate over zero runs is not zero, it is unknown.
func TestARateWithoutADenominatorIsNull(t *testing.T) {
	r := CalibrationCounts{TruePass: 3, Undecided: 2}.Rates()
	if r.Sensitivity != nil {
		t.Fatalf("sensitivity from no negative-pole runs = %v, want null", *r.Sensitivity)
	}
	if r.Specificity == nil {
		t.Fatal("specificity from three true passes is computable")
	}
	for _, p := range PPVByBaseRate(r, DefaultBaseRates) {
		if p.PPV != nil {
			t.Fatalf("PPV at %.3f computed without a sensitivity", p.BaseRate)
		}
	}
}

const testManifest = `schema: eval_manifest/v1
standard_fault: net.partition(role:leader)@3000..7500
arms:
  - id: HC-01
    target: testdata/kvfixture
    control: harness
    profile: linear
    expected: 1
    n: {P2: 10}
  - id: HC-02
    target: testdata/kvfixture
    control: harness
    profile: linear
    expected: 0
  - id: T2-X
    target: undecided
    control: positive
`

func TestTheManifestYieldsPolesAndSaysWhatItCannotCalibrate(t *testing.T) {
	m, err := ParseEvalManifest([]byte(testManifest))
	if err != nil {
		t.Fatalf("ParseEvalManifest: %v", err)
	}
	if len(m.Arms) != 3 {
		t.Fatalf("%d arms, want 3", len(m.Arms))
	}
	hc1, _ := m.Arm("HC-01")
	if exp, ok := hc1.ExpectedVerdict(); !ok || exp != VerdictFail || ControlTypeFor(exp) != ControlBuggyNegative {
		t.Fatalf("HC-01 expected %v (%v), want FAIL / buggy_negative", exp, ok)
	}
	hc2, _ := m.Arm("HC-02")
	if exp, ok := hc2.ExpectedVerdict(); !ok || exp != VerdictPass || ControlTypeFor(exp) != ControlGoldPositive {
		t.Fatalf("HC-02 expected %v (%v), want PASS / gold_positive", exp, ok)
	}
	if t2, _ := m.Arm("T2-X"); func() bool { _, ok := t2.ExpectedVerdict(); return ok }() {
		t.Fatal("an arm with no expected outcome was given one")
	}
	if _, err := ParseEvalManifest([]byte("schema: something_else/v1\narms: []\n")); err == nil {
		t.Fatal("a document that is not an evaluation manifest was accepted")
	}
}

func TestTheReportJoinsArmsWithTheirVerdicts(t *testing.T) {
	m, err := ParseEvalManifest([]byte(testManifest))
	if err != nil {
		t.Fatal(err)
	}
	v := func(arm string, res VerdictResult) LocatedVerdict {
		return LocatedVerdict{Path: arm + "/verdict.json", Verdict: Verdict{RunID: "r_2026_10_21_0001", Arm: arm, Verdict: res}}
	}
	rep := BuildCalibrationReport("eval/manifest.yaml", m, []LocatedVerdict{
		v("HC-01", VerdictFail), v("HC-01", VerdictFail), v("HC-01", VerdictPass),
		v("HC-02", VerdictPass), v("HC-02", VerdictInconclusive),
		v("NOT-AN-ARM", VerdictPass),
		{Path: "plain/verdict.json", Verdict: Verdict{RunID: "r_2026_10_21_0009", Verdict: VerdictPass}},
	}, DefaultBaseRates)
	if rep.Schema != CalibrationReportSchema {
		t.Fatalf("schema %q", rep.Schema)
	}
	a := rep.Aggregate
	if a.TrueReject != 2 || a.Escape != 1 || a.TruePass != 1 || a.Undecided != 1 || a.Runs != 5 {
		t.Fatalf("aggregate %+v, want 2 true rejects, 1 escape, 1 true pass, 1 undecided over 5 runs", a)
	}
	approx(t, "aggregate sensitivity", rep.Rates.Sensitivity, 2.0/3.0)
	if len(rep.Uncalibrated) != 1 || !strings.HasPrefix(rep.Uncalibrated[0], "T2-X") {
		t.Fatalf("uncalibrated = %v, want T2-X named with its reason", rep.Uncalibrated)
	}
	if len(rep.Unmatched) != 1 || rep.Unmatched[0] != "NOT-AN-ARM/verdict.json" {
		t.Fatalf("unmatched = %v: a verdict naming an arm the manifest lacks must be reported", rep.Unmatched)
	}
	if !rep.HasEscape() {
		t.Fatal("the report does not see its escape")
	}
}
