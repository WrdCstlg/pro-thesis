package oracle

import (
	"context"
	"strings"
	"testing"

	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

func declaredInput(healthProbed []string) *Input {
	return &Input{
		Nodes: []NodeObservation{
			{NodeID: "n1", StateObserved: true, Running: true},
			{NodeID: "n2", StateObserved: true, Running: true},
			{NodeID: "pg", StateObserved: true, Running: true},
		},
		Phases: schema.PhaseTimings{
			{Phase: schema.PhaseHeal, StartMS: 5000, EndMS: 6000},
			{Phase: schema.PhaseQuiesce, StartMS: 6000, EndMS: 8000},
		},
		Probes: []ProbeObservation{
			{NodeID: "n1", TMS: 6500, OK: true, Target: "http://127.0.0.1:1/health"},
			{NodeID: "n2", TMS: 6500, OK: true, Target: "http://127.0.0.1:2/health"},
		},
		HealthProbed: healthProbed,
	}
}

// D-066. A node harness.health declares no probe for is NOT JUDGED: it is
// named in the OK text rather than turning the world INCONCLUSIVE as "never
// probed". Mutation: ignoring HealthProbed returns INCONCLUSIVE for pg.
func TestAvailabilityDoesNotJudgeANodeWithoutADeclaredProbe(t *testing.T) {
	o := NewAvailabilityAfterHeal(DefaultAvailabilityOptions())
	res, err := o.Evaluate(context.Background(), schema.PhaseAssert, declaredInput([]string{"n1", "n2"}))
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != schema.StatusOK {
		t.Fatalf("want ok, got %s: %s", res.Status, res.Explanation)
	}
	if !strings.Contains(res.Explanation, "not judged") || !strings.Contains(res.Explanation, "pg") {
		t.Fatalf("the OK text must name the node it did not judge: %q", res.Explanation)
	}
	if !strings.Contains(res.Explanation, "all 2 node(s)") {
		t.Fatalf("the OK text must count only the judged nodes: %q", res.Explanation)
	}
}

// A declared node that was never probed is still the harness's failure to
// produce evidence, and stays INCONCLUSIVE.
func TestAvailabilityStillRefusesADeclaredNodeThatWasNeverProbed(t *testing.T) {
	o := NewAvailabilityAfterHeal(DefaultAvailabilityOptions())
	res, err := o.Evaluate(context.Background(), schema.PhaseAssert, declaredInput([]string{"n1", "n2", "pg"}))
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != schema.StatusInconclusive || !strings.Contains(res.Explanation, "pg") {
		t.Fatalf("want inconclusive naming pg, got %s: %s", res.Status, res.Explanation)
	}
}

// Nothing declared means nothing to judge against: INCONCLUSIVE with the
// reason, not a vacuous pass.
func TestAvailabilityWithNoDeclaredProbeIsInconclusive(t *testing.T) {
	o := NewAvailabilityAfterHeal(DefaultAvailabilityOptions())
	res, err := o.Evaluate(context.Background(), schema.PhaseAssert, declaredInput([]string{}))
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != schema.StatusInconclusive || !strings.Contains(res.Explanation, "harness.health") {
		t.Fatalf("got %s: %s", res.Status, res.Explanation)
	}
}

// A nil declaration is a caller that did not record one; every node is judged
// as before D-066.
func TestAvailabilityNilDeclarationJudgesEveryNode(t *testing.T) {
	o := NewAvailabilityAfterHeal(DefaultAvailabilityOptions())
	res, err := o.Evaluate(context.Background(), schema.PhaseAssert, declaredInput(nil))
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != schema.StatusInconclusive || !strings.Contains(res.Explanation, "pg") {
		t.Fatalf("want the pre-D-066 'never probed' inconclusive for pg, got %s: %s", res.Status, res.Explanation)
	}
}
