package driver

import (
	"path/filepath"
	"strings"
	"testing"
)

// A plan supplied from outside a world bundle (a trace an external generator
// emitted, or one op-level ddmin reduced) is driven exactly. It must therefore
// be an operation trace, for the world's driver profile, and it runs under the
// world's seed, so its workload digest is the workload that will actually run.
func TestASuppliedPlanIsAnOperationTraceForTheWorld(t *testing.T) {
	trace, err := extractSample(t, ExtractOptions{}).Plan(nil)
	if err != nil {
		t.Fatal(err)
	}
	write := func(p *Plan) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "generated.plan.json")
		if err := WritePlan(path, p); err != nil {
			t.Fatal(err)
		}
		return path
	}

	trace.HistoryPath = "/generator/scratch/history.jsonl"
	got, err := LoadSuppliedPlan(write(trace), "linear", 42)
	if err != nil {
		t.Fatalf("LoadSuppliedPlan: %v", err)
	}
	if got.Profile != "linear" || got.Seed != 42 || got.HistoryPath != "" {
		t.Fatalf("profile %q seed %d history %q, want the world's profile and seed and no foreign history path",
			got.Profile, got.Seed, got.HistoryPath)
	}
	if ids := got.OpIDs(); len(ids) != 4 || ids[0] != 11402 || ids[3] != 11999 {
		t.Fatalf("op ids %v, want the supplied trace in its own order", ids)
	}

	other := trace.Clone()
	other.Profile = "soak"
	if _, err := LoadSuppliedPlan(write(other), "linear", 42); err == nil || !strings.Contains(err.Error(), "soak") {
		t.Fatalf("a plan for another driver profile was accepted (err %v)", err)
	}

	profile, err := NewPlan(nil, "linear", "", 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSuppliedPlan(write(profile), "linear", 42); err == nil ||
		!strings.Contains(err.Error(), "no operation trace") {
		t.Fatalf("a profile plan, which the driver regenerates from, was accepted (err %v)", err)
	}

	if _, err := LoadSuppliedPlan(filepath.Join(t.TempDir(), "absent.plan.json"), "linear", 42); err == nil {
		t.Fatal("a missing plan file was accepted")
	}
}
