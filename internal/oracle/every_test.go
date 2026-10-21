package oracle

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// A world's oracles are evaluated once, at ASSERT. An oracle registered for a
// set that does not include ASSERT used to be dropped by selection and recorded
// nowhere, so the world could PASS over a property nobody checked. Every
// registered oracle must come back with a finding, and one that was not
// evaluated must come back inconclusive, never ok.
func TestEvaluateEveryFailsClosedOnAnOracleThatCannotRunInThePhase(t *testing.T) {
	assertOnly := helperDef(t, helperCmd(t, "emit", "0", docFile(t, okDoc())))
	healOnly := helperDef(t, helperCmd(t, "emit", "0", docFile(t, okDoc())), func(d *Definition) {
		d.Name = "heal.only"
		d.ValidPhases = []schema.Phase{schema.PhaseHeal}
	})
	e := NewEngine()
	for _, d := range []Definition{assertOnly, healOnly} {
		o, err := NewProcessOracle(d, ExternalOptions{})
		if err != nil {
			t.Fatalf("NewProcessOracle(%s): %v", d.Name, err)
		}
		if err := e.Register(o); err != nil {
			t.Fatalf("Register(%s): %v", d.Name, err)
		}
	}

	if got := e.Unevaluable(schema.PhaseAssert); len(got) != 1 || got[0] != "heal.only" {
		t.Fatalf("Unevaluable(ASSERT) = %v, want [heal.only]", got)
	}

	fs, err := e.EvaluateEvery(context.Background(), schema.PhaseAssert, testInput(t))
	if err != nil {
		t.Fatalf("EvaluateEvery: %v", err)
	}
	if len(fs) != 2 {
		t.Fatalf("got %d findings (%v), want one for each of the 2 registered oracles", len(fs), fs.Names())
	}
	for _, f := range fs {
		switch f.Oracle {
		case testOracleName:
			if f.Result.Status != schema.StatusOK {
				t.Fatalf("the ASSERT oracle: %q (%s), want ok", f.Result.Status, f.Result.Explanation)
			}
		case "heal.only":
			if f.Result.Status != schema.StatusInconclusive {
				t.Fatalf("an oracle that never ran reported %q; only inconclusive is honest", f.Result.Status)
			}
			if !errors.Is(f.Err, ErrNotEvaluated) {
				t.Fatalf("the finding's error is %v, want ErrNotEvaluated", f.Err)
			}
			if !strings.Contains(f.Result.Explanation, "HEAL") || !strings.Contains(f.Result.Explanation, "ASSERT") {
				t.Fatalf("the explanation does not say what was declared and where evaluation happens: %s",
					f.Result.Explanation)
			}
		default:
			t.Fatalf("unexpected finding for %q", f.Oracle)
		}
	}
	if got := fs.ExitCode(); got != schema.ExitInconclusive {
		t.Fatalf("exit code = %d, want 2: a property that was never checked cannot support a PASS", got)
	}
}

// With every oracle valid in the phase, EvaluateEvery is EvaluateAt: same
// findings, same order. The fail-closed rule adds findings and changes nothing
// else.
func TestEvaluateEveryMatchesEvaluateAtWhenNothingIsSkipped(t *testing.T) {
	e := NewEngine()
	o, err := NewProcessOracle(helperDef(t, helperCmd(t, "emit", "0", docFile(t, okDoc()))), ExternalOptions{})
	if err != nil {
		t.Fatalf("NewProcessOracle: %v", err)
	}
	if err := e.Register(o); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if got := e.Unevaluable(schema.PhaseAssert); len(got) != 0 {
		t.Fatalf("Unevaluable(ASSERT) = %v, want none", got)
	}
	every, err := e.EvaluateEvery(context.Background(), schema.PhaseAssert, testInput(t))
	if err != nil {
		t.Fatalf("EvaluateEvery: %v", err)
	}
	at, err := e.EvaluateAt(context.Background(), schema.PhaseAssert, testInput(t))
	if err != nil {
		t.Fatalf("EvaluateAt: %v", err)
	}
	if len(every) != len(at) || every[0].Oracle != at[0].Oracle || every[0].Result.Status != at[0].Result.Status {
		t.Fatalf("EvaluateEvery %v differs from EvaluateAt %v", every.Names(), at.Names())
	}
}
