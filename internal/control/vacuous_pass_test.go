package control

import (
	"context"
	"testing"

	"github.com/WrdCstlg/pro-thesis/internal/perturber"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// ---------------------------------------------------------------------------
// A world that no oracle judged cannot pass (OQ-075, D-089)
//
// The world outcome starts at PASS and only a finding can raise it. An engine
// that returned no findings and no error therefore left the world at PASS, and
// a run in which nothing was checked exited 0. seams.go promised the opposite,
// citing a worldRun.assertPhase that has never existed in any revision. In
// production the empty set is reached with `oracles.builtin: []` and no oracles
// directory, which config validation accepts.
// ---------------------------------------------------------------------------

// noFindings is an engine that judged nothing and reported no error.
type noFindings struct{}

func (noFindings) Evaluate(context.Context, EvalRequest) ([]OracleResult, error) { return nil, nil }

func TestAWorldNoOracleJudgedIsInconclusiveNotPass(t *testing.T) {
	r, _ := newPerturbTestRunner(t, perturbTestConfig(t), nil)
	r.oracleEngine = noFindings{}

	verdict, exit, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if exit != schema.ExitInconclusive {
		t.Fatalf("exit = %d (verdict %s), want 2: no oracle judged the world, so nothing was "+
			"checked, and PASS would describe a system nobody looked at", exit, verdict.Verdict)
	}

	// The same world judged by one oracle that says ok must still pass, so the
	// assertion above is about the empty finding set and nothing else in this setup.
	control, _ := newPerturbTestRunner(t, perturbTestConfig(t), nil)
	cverdict, cexit, err := control.Run(context.Background())
	if err != nil {
		t.Fatalf("control Run: %v", err)
	}
	if cexit != schema.ExitPass {
		t.Fatalf("control world with one ok finding: exit = %d (verdict %s), want 0; without a "+
			"passing control the assertion above could hold for an unrelated reason",
			cexit, cverdict.Verdict)
	}
}

// A world whose HEAL left a fault behind is a harness error. Finding afterwards
// that no oracle judged it must not overwrite that cause with a lesser one:
// both exit 2, but result.json is what `thesis diagnose` attributes, and
// "inconclusive" there would hide that the environment failed.
func TestAnUnjudgedWorldKeepsTheWorseCauseItAlreadyHad(t *testing.T) {
	inj := newControlFakeInjector()
	inj.residues = []perturber.Residue{{
		NodeID: "kv-n1", Mechanism: "iptables", FaultID: "f001",
		Detail: `-A INPUT -j DROP -m comment --comment "thesis:r:f001"`,
	}}
	r, _ := newPerturbTestRunner(t, perturbTestConfig(t), []string{"proc.pause(kv-n1)@10..40"}, inj)
	r.oracleEngine = noFindings{}

	_, exit, err := r.Run(context.Background())
	if err != nil || exit != schema.ExitInconclusive {
		t.Fatalf("Run: exit %d, err %v; want exit 2", exit, err)
	}
	if got := readResultDoc(t, r, 1)["outcome"]; got != string(OutcomeHarnessError) {
		t.Fatalf("result.json outcome = %v, want %q: the HEAL failure was overwritten by the "+
			"lesser finding that no oracle judged the world", got, OutcomeHarnessError)
	}
}
