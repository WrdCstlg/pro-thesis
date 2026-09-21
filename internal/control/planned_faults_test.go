package control

import (
	"context"
	"testing"

	"github.com/WrdCstlg/pro-thesis/internal/oracle"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// ---------------------------------------------------------------------------
// no_crash must be told what the harness itself did
//
// oracle.Input.PlannedFaults was hardcoded to an EMPTY LIST, so no_crash's
// entire "outside a planned fault window" clause was dead code in production.
// Measured on run r_2026_09_08_5c6b world 3, whose only fault was
// `proc.kill(kv-n1, signal=SIGTERM)@12600..14600`:
//
//	no_crash VIOLATED — "1 process exited outside any planned fault window:
//	                     kv-n1 at t+12713ms (exit code 0)"
//
// against a window that opened 113 ms earlier and named that very node. The
// oracle was right about its own logic and was handed nothing to reason with.
//
// Why it matters more in Phase 4 than it did in Phase 2: a guided search over a
// config whose `perturber.allow` contains proc.kill or proc.restart reaches its
// first kill world, is handed a fabricated crash violation, and STOPS; having
// "found" a bug that is the harness reporting its own fault injection. That is
// the ranked #1 failure mode of this phase (a search learning from noise),
// arriving through an unwired field.
// ---------------------------------------------------------------------------

func TestPlannedFaultWindowsCarryTheResolvedNodes(t *testing.T) {
	windows := plannedFaultWindows([]schema.RealizedFault{{
		Fault:    "proc.pause(role:leader)@8300..11000",
		Resolved: "proc.pause(kv-n2)@8300..11000",
		Nodes:    []string{"kv-n2"},
		StartMS:  8300,
		EndMS:    11000,
	}})
	if len(windows) != 1 {
		t.Fatalf("got %d windows, want 1", len(windows))
	}
	w := windows[0]
	if w.Kind != "proc.pause" {
		t.Errorf("Kind = %q, want proc.pause", w.Kind)
	}
	// The window must name the node the ROLE resolved to. A planned
	// `role:leader` binds to a concrete node only at injection time (D-012), so
	// a window built from the planned string alone could excuse nothing.
	if !w.Names("kv-n2") {
		t.Fatalf("the window does not name kv-n2, which the role resolved to; nodes = %v", w.Nodes)
	}
	if w.Names("kv-n1") {
		t.Fatalf("the window names kv-n1, which the fault never touched; it would excuse an "+
			"unrelated crash. nodes = %v", w.Nodes)
	}
	if !w.Covers("kv-n2", 8500, 0) {
		t.Errorf("the window does not cover an exit inside it")
	}
	if w.Covers("kv-n2", 12000, 0) {
		t.Errorf("the window covers an exit a full second after it closed with no grace")
	}
}

// A realized fault whose binding was NOT recorded must excuse nothing. Treating
// "unknown" as "planned" would let one unrecorded fault silence every crash in
// its time range.
func TestAFaultWithNoRecordedBindingExcusesNothing(t *testing.T) {
	windows := plannedFaultWindows([]schema.RealizedFault{{
		Fault: "proc.kill(role:leader)@1000..2000", StartMS: 1000, EndMS: 2000,
	}})
	if len(windows) != 1 {
		t.Fatalf("got %d windows, want 1", len(windows))
	}
	if windows[0].Covers("kv-n1", 1500, 0) {
		t.Fatal("a window with no recorded nodes excused a crash; an unrecorded binding must " +
			"excuse nothing, or one unrecorded fault silences every crash in its range")
	}
}

// capturingOracles records the EvalRequest so a test can assert the wiring
// rather than the arithmetic.
type capturingOracles struct{ last EvalRequest }

func (c *capturingOracles) Evaluate(_ context.Context, req EvalRequest) ([]OracleResult, error) {
	c.last = req
	return nil, nil
}

// The wiring itself: a world that injected a fault must hand the oracle engine
// what it injected.
func TestTheOracleEngineIsToldWhatThePerturberInjected(t *testing.T) {
	cfg := perturbTestConfig(t)
	cap := &capturingOracles{}
	inj := newControlFakeInjector()
	r, _ := newPerturbTestRunner(t, cfg, []string{"proc.pause(kv-n1)@10..20"}, inj)
	r.oracleEngine = cap

	if _, _, err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(cap.last.Realized) == 0 {
		t.Fatal("EvalRequest.Realized was empty for a world that injected a fault.\n" +
			"oracle.Input.PlannedFaults is built from it, and an empty list makes no_crash's " +
			"\"outside a planned fault window\" clause dead code: every harness-scheduled kill " +
			"is then reported as a crash the system caused.")
	}
	// And, crucially, the ASSEMBLY STEP must carry it into the document the
	// oracles actually read. Asserting only on EvalRequest.Realized would pass
	// with the field still hardcoded to an empty list at the point of use, which
	// is exactly how this survived three phases.
	in, err := buildOracleInput(context.Background(), cap.last)
	if err != nil {
		t.Fatalf("buildOracleInput: %v", err)
	}
	if len(in.PlannedFaults) == 0 {
		t.Fatal("oracle.Input.PlannedFaults is EMPTY for a world that injected a fault. " +
			"no_crash then has nothing to excuse and reports the harness's own scheduled kill " +
			"as a crash the system caused.")
	}
	if !in.PlannedFaults[0].Names("kv-n1") {
		t.Fatalf("the assembled window does not name kv-n1: %+v", in.PlannedFaults[0])
	}
}

// The end-to-end claim, through the REAL no_crash oracle: an exit inside a
// window the harness itself scheduled is not a crash.
func TestNoCrashExcusesAnExitInsideAWindowTheHarnessScheduled(t *testing.T) {
	in := &oracle.Input{
		Nodes: []oracle.NodeObservation{{
			NodeID:        "kv-n1",
			StateObserved: true,
			Running:       false,
			Exit:          &oracle.ProcessExit{Code: 0, AtMS: int64Ptr(12713)},
		}},
		PlannedFaults: plannedFaultWindows([]schema.RealizedFault{{
			Fault:   "proc.kill(kv-n1, signal=SIGTERM)@12600..14600",
			Nodes:   []string{"kv-n1"},
			StartMS: 12600,
			EndMS:   14600,
		}}),
	}
	res, err := oracle.NewNoCrash(oracle.NoCrashOptions{}).
		Evaluate(context.Background(), schema.PhaseAssert, in)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if res.Status == schema.StatusViolated {
		t.Fatalf("no_crash reported a violation for an exit INSIDE the window the harness itself "+
			"scheduled: %s", res.Explanation)
	}

	// And the assertion is not merely absent: the same exit with NO window is
	// still a violation, so this test cannot pass by weakening the oracle.
	in.PlannedFaults = nil
	res, err = oracle.NewNoCrash(oracle.NoCrashOptions{}).
		Evaluate(context.Background(), schema.PhaseAssert, in)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if res.Status != schema.StatusViolated {
		t.Fatalf("no_crash did NOT report an unexplained exit as a violation (status %s); the "+
			"previous assertion would then be vacuous", res.Status)
	}
}

func int64Ptr(v int64) *int64 { return &v }
