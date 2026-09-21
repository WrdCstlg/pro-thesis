package oracle

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// TestBuiltinsMatchSchemaDeclarations verifies that every built-in oracle's
// Class and ValidPhases exactly match the frozen schema.BuiltinOracle declarations.
func TestBuiltinsMatchSchemaDeclarations(t *testing.T) {
	opts := DefaultOptions()
	for _, b := range schema.AllBuiltinOracles {
		t.Run(string(b), func(t *testing.T) {
			o, err := NewBuiltin(b, opts)
			if err != nil {
				t.Fatalf("NewBuiltin(%q) error: %v", b, err)
			}
			if got, want := o.Name(), string(b); got != want {
				t.Errorf("Name() = %q, want %q", got, want)
			}
			if got, want := o.Class(), b.Class(); got != want {
				t.Errorf("Class() = %q, want %q", got, want)
			}
			gotPhases := o.ValidPhases()
			wantPhases := b.ValidPhases()
			if len(gotPhases) != len(wantPhases) {
				t.Fatalf("ValidPhases() length = %d, want %d", len(gotPhases), len(wantPhases))
			}
			for i := range gotPhases {
				if gotPhases[i] != wantPhases[i] {
					t.Errorf("ValidPhases()[%d] = %s, want %s", i, gotPhases[i], wantPhases[i])
				}
			}
		})
	}
}

func TestNoCrashOracle(t *testing.T) {
	ctx := context.Background()
	opts := DefaultNoCrashOptions()
	oracle := NewNoCrash(opts)

	t.Run("clean running nodes pass", func(t *testing.T) {
		in := &Input{
			Nodes: []NodeObservation{
				{NodeID: "n1", StateObserved: true, Running: true},
				{NodeID: "n2", StateObserved: true, Running: true},
			},
		}
		res, err := oracle.Evaluate(ctx, schema.PhaseAssert, in)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res.Status != schema.StatusOK {
			t.Errorf("expected OK, got %v: %s", res.Status, res.Explanation)
		}
	})

	t.Run("unplanned exit fails", func(t *testing.T) {
		at := int64(1500)
		in := &Input{
			Nodes: []NodeObservation{
				{NodeID: "n1", StateObserved: true, Running: true},
				{
					NodeID:        "n2",
					StateObserved: true,
					Running:       false,
					Exit: &ProcessExit{
						Code:   137,
						Signal: "SIGKILL",
						AtMS:   &at,
					},
				},
			},
		}
		res, err := oracle.Evaluate(ctx, schema.PhaseAssert, in)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res.Status != schema.StatusViolated {
			t.Errorf("expected Violated, got %v: %s", res.Status, res.Explanation)
		}
		if res.FirstSeenMS != 1500 {
			t.Errorf("expected FirstSeenMS 1500, got %d", res.FirstSeenMS)
		}
	})

	t.Run("planned exit is excused", func(t *testing.T) {
		at := int64(2500)
		in := &Input{
			Nodes: []NodeObservation{
				{
					NodeID:        "n1",
					StateObserved: true,
					Running:       false,
					Exit: &ProcessExit{
						Code:   137,
						Signal: "SIGKILL",
						AtMS:   &at,
					},
				},
			},
			PlannedFaults: []FaultWindow{
				{
					Kind:    "proc.kill",
					Nodes:   []string{"n1"},
					StartMS: 2000,
					EndMS:   3000,
				},
			},
		}
		res, err := oracle.Evaluate(ctx, schema.PhaseAssert, in)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res.Status != schema.StatusOK {
			t.Errorf("expected OK, got %v: %s", res.Status, res.Explanation)
		}
	})

	t.Run("unobserved node state is inconclusive", func(t *testing.T) {
		in := &Input{
			Nodes: []NodeObservation{
				{NodeID: "n1", StateObserved: false},
			},
		}
		res, err := oracle.Evaluate(ctx, schema.PhaseAssert, in)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res.Status != schema.StatusInconclusive {
			t.Errorf("expected Inconclusive, got %v: %s", res.Status, res.Explanation)
		}
	})
}

func TestAvailabilityAfterHealOracle(t *testing.T) {
	ctx := context.Background()
	opts := DefaultAvailabilityOptions()
	oracle := NewAvailabilityAfterHeal(opts)

	t.Run("all nodes healthy after heal passes", func(t *testing.T) {
		in := &Input{
			Nodes: []NodeObservation{
				{NodeID: "n1", StateObserved: true, Running: true},
				{NodeID: "n2", StateObserved: true, Running: true},
			},
			Phases: schema.PhaseTimings{
				{Phase: schema.PhaseHeal, StartMS: 5000, EndMS: 6000},
				{Phase: schema.PhaseQuiesce, StartMS: 6000, EndMS: 8000},
			},
			Probes: []ProbeObservation{
				{NodeID: "n1", TMS: 6500, OK: true, Target: "http://n1:8080/healthz"},
				{NodeID: "n2", TMS: 6500, OK: true, Target: "http://n2:8080/healthz"},
			},
		}
		res, err := oracle.Evaluate(ctx, schema.PhaseAssert, in)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res.Status != schema.StatusOK {
			t.Errorf("expected OK, got %v: %s", res.Status, res.Explanation)
		}
	})

	t.Run("failed probe in quiesce fails", func(t *testing.T) {
		in := &Input{
			Nodes: []NodeObservation{
				{NodeID: "n1", StateObserved: true, Running: true},
				{NodeID: "n2", StateObserved: true, Running: true},
			},
			Phases: schema.PhaseTimings{
				{Phase: schema.PhaseHeal, StartMS: 5000, EndMS: 6000},
				{Phase: schema.PhaseQuiesce, StartMS: 6000, EndMS: 8000},
			},
			Probes: []ProbeObservation{
				{NodeID: "n1", TMS: 6500, OK: true, Target: "http://n1:8080/healthz"},
				{NodeID: "n2", TMS: 6500, OK: false, Target: "http://n2:8080/healthz", Err: "connection refused"},
			},
		}
		res, err := oracle.Evaluate(ctx, schema.PhaseAssert, in)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res.Status != schema.StatusViolated {
			t.Errorf("expected Violated, got %v: %s", res.Status, res.Explanation)
		}
		if !strings.Contains(res.Explanation, "n2") {
			t.Errorf("expected explanation to mention n2, got %s", res.Explanation)
		}
	})

	t.Run("unprobed node is inconclusive", func(t *testing.T) {
		in := &Input{
			Nodes: []NodeObservation{
				{NodeID: "n1", StateObserved: true, Running: true},
				{NodeID: "n2", StateObserved: true, Running: true},
			},
			Phases: schema.PhaseTimings{
				{Phase: schema.PhaseHeal, StartMS: 5000, EndMS: 6000},
				{Phase: schema.PhaseQuiesce, StartMS: 6000, EndMS: 8000},
			},
			Probes: []ProbeObservation{
				{NodeID: "n1", TMS: 6500, OK: true, Target: "http://n1:8080/healthz"},
			},
		}
		res, err := oracle.Evaluate(ctx, schema.PhaseAssert, in)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res.Status != schema.StatusInconclusive {
			t.Errorf("expected Inconclusive, got %v: %s", res.Status, res.Explanation)
		}
	})
}

func TestNoStuckOpOracle(t *testing.T) {
	ctx := context.Background()
	opts := DefaultNoStuckOpOptions()
	oracle := NewNoStuckOp(opts)

	originNS := int64(1700000000000000000)
	nsAtMS := func(ms int64) int64 {
		return originNS + ms*int64(time.Millisecond)
	}

	opID1 := int64(1)
	opID2 := int64(2)

	t.Run("all ops completed within SLO passes", func(t *testing.T) {
		in := &Input{
			DriveOriginWallNS: originNS,
			Phases: schema.PhaseTimings{
				{Phase: schema.PhaseDrive, StartMS: 0, EndMS: 10000},
				{Phase: schema.PhaseHeal, StartMS: 10000, EndMS: 12000},
				{Phase: schema.PhaseQuiesce, StartMS: 12000, EndMS: 15000},
				{Phase: schema.PhaseAssert, StartMS: 15000, EndMS: 15500},
			},
			History: &History{
				Entries: []schema.HistoryEntry{
					{TNS: nsAtMS(1000), Type: schema.HistoryInvoke, F: "write", OpID: &opID1},
					{TNS: nsAtMS(1050), Type: schema.HistoryOK, F: "write", OpID: &opID1},
					{TNS: nsAtMS(12100), Type: schema.HistoryInvoke, F: "read", OpID: &opID2},
					{TNS: nsAtMS(12200), Type: schema.HistoryOK, F: "read", OpID: &opID2},
				},
			},
		}
		res, err := oracle.Evaluate(ctx, schema.PhaseAssert, in)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res.Status != schema.StatusOK {
			t.Errorf("expected OK, got %v: %s", res.Status, res.Explanation)
		}
	})

	t.Run("uncompleted op past HEAL + SLO fails", func(t *testing.T) {
		in := &Input{
			DriveOriginWallNS: originNS,
			Phases: schema.PhaseTimings{
				{Phase: schema.PhaseDrive, StartMS: 0, EndMS: 10000},
				{Phase: schema.PhaseHeal, StartMS: 10000, EndMS: 12000},
				{Phase: schema.PhaseQuiesce, StartMS: 12000, EndMS: 20000},
				{Phase: schema.PhaseAssert, StartMS: 20000, EndMS: 20500},
			},
			History: &History{
				Entries: []schema.HistoryEntry{
					{TNS: nsAtMS(5000), Type: schema.HistoryInvoke, F: "write", OpID: &opID1},
					// Op 1 was never completed. Heal end is 12000, SLO ceiling is 5000.
					// At quiesce end (20000), 20000 >= 12000 + 5000 = 17000. It is stuck!
				},
			},
		}
		res, err := oracle.Evaluate(ctx, schema.PhaseAssert, in)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res.Status != schema.StatusViolated {
			t.Errorf("expected Violated, got %v: %s", res.Status, res.Explanation)
		}
	})

	t.Run("missing origin is inconclusive", func(t *testing.T) {
		in := &Input{
			DriveOriginWallNS: 0,
			Phases: schema.PhaseTimings{
				{Phase: schema.PhaseDrive, StartMS: 0, EndMS: 10000},
			},
			History: &History{
				Entries: []schema.HistoryEntry{
					{TNS: 12345, Type: schema.HistoryInvoke, F: "write", OpID: &opID1},
				},
			},
		}
		res, err := oracle.Evaluate(ctx, schema.PhaseAssert, in)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res.Status != schema.StatusInconclusive {
			t.Errorf("expected Inconclusive, got %v: %s", res.Status, res.Explanation)
		}
	})
}
