package replay

import (
	"context"
	"strings"
	"testing"

	"github.com/WrdCstlg/pro-thesis/internal/control"
	"github.com/WrdCstlg/pro-thesis/internal/corpus"
	"github.com/WrdCstlg/pro-thesis/internal/driver"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// `thesis regress` replays the committed corpus. A corpus world that no longer
// reproduces used to be a PASS, but the harness cannot tell "the defect is
// fixed" from "this world no longer reaches the defect", so it is
// NON_REPRODUCING and the gate is inconclusive (exit 2), never green.

type fixedExec struct {
	exec Execution
	reqs []ExecRequest
}

func (f *fixedExec) Execute(_ context.Context, req ExecRequest) Execution {
	f.reqs = append(f.reqs, req)
	return f.exec
}

var (
	cleanRun    = Execution{Outcome: control.OutcomePass, Findings: []control.OracleResult{{Output: schema.OracleOutput{Oracle: "linearizable.kv", Status: schema.StatusOK}}}}
	violatedRun = Execution{Outcome: control.OutcomeViolation, Findings: []control.OracleResult{{Output: schema.OracleOutput{Oracle: "linearizable.kv", Class: schema.ClassConsistency, Status: schema.StatusViolated}}}}
)

func corpusWith(t *testing.T, plan *driver.Plan) *corpus.Corpus {
	t.Helper()
	c := corpus.OpenDir(t.TempDir())
	w := schema.NewWorld(7, "default", "linear")
	w.FaultSchedule.Planned = []string{"net.partition(kv-n1)@3000..7500"}
	w.FaultSchedule.Realized = []schema.RealizedFault{}
	var raw []byte
	if plan != nil {
		d, err := plan.WorkloadDigest()
		if err != nil {
			t.Fatal(err)
		}
		w.PlanDigest = d
		if raw, err = plan.Marshal(); err != nil {
			t.Fatal(err)
		}
	}
	w = w.Normalized()
	if _, err := c.AddWithPlan(&w, corpus.Confirmation{Attempts: 3, Reproduced: 3}, raw); err != nil {
		t.Fatalf("AddWithPlan: %v", err)
	}
	return c
}

func TestARegressionThatNoLongerReproducesIsNotAPass(t *testing.T) {
	rep, err := Regress(context.Background(), RegressOptions{Corpus: corpusWith(t, nil), Exec: &fixedExec{exec: cleanRun}})
	if err != nil {
		t.Fatalf("Regress: %v", err)
	}
	if len(rep.Worlds) != 1 || rep.Worlds[0].Status != StatusNonReproducing {
		t.Fatalf("worlds %+v, want one NON_REPRODUCING", rep.Worlds)
	}
	if rep.NonReproducing != 1 || rep.ExitCode() != schema.ExitInconclusive {
		t.Fatalf("non_reproducing %d, exit %d, want 1 and 2", rep.NonReproducing, rep.ExitCode())
	}
	if !strings.Contains(rep.Summary(), "non-reproducing") {
		t.Fatalf("summary %q does not say so", rep.Summary())
	}
}

func TestARegressionThatReproducesFails(t *testing.T) {
	rep, err := Regress(context.Background(), RegressOptions{Corpus: corpusWith(t, nil), Exec: &fixedExec{exec: violatedRun}})
	if err != nil {
		t.Fatalf("Regress: %v", err)
	}
	if rep.Worlds[0].Status != StatusFail || rep.ExitCode() != schema.ExitFail {
		t.Fatalf("status %s, exit %d, want FAIL and 1", rep.Worlds[0].Status, rep.ExitCode())
	}
}

func TestAnEmptyCorpusSaysSoAndExitsZero(t *testing.T) {
	rep, err := Regress(context.Background(), RegressOptions{Corpus: corpus.OpenDir(t.TempDir()), Exec: &fixedExec{exec: cleanRun}})
	if err != nil {
		t.Fatalf("Regress: %v", err)
	}
	if !rep.Empty || rep.ExitCode() != schema.ExitPass || !strings.Contains(rep.Summary(), "no regressions") {
		t.Fatalf("empty %v, exit %d, summary %q", rep.Empty, rep.ExitCode(), rep.Summary())
	}
}

// A corpus entry that names its workload is replayed with the plan committed
// beside it, not with one regenerated from the current config.
func TestRegressDrivesTheCommittedPlan(t *testing.T) {
	plan := &driver.Plan{Schema: driver.PlanSchema, Profile: "linear", Seed: 7,
		Operations: []driver.PlanOp{{OpID: 4, Process: 0, F: "write", Key: "k/0"}}}
	exec := &fixedExec{exec: violatedRun}
	if _, err := Regress(context.Background(), RegressOptions{Corpus: corpusWith(t, plan), Exec: exec}); err != nil {
		t.Fatalf("Regress: %v", err)
	}
	if len(exec.reqs) != 1 || exec.reqs[0].Plan == nil || len(exec.reqs[0].Plan.Operations) != 1 {
		t.Fatalf("the committed plan was not driven: %+v", exec.reqs)
	}
}
