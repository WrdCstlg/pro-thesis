package replay

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/WrdCstlg/pro-thesis/internal/control"
	"github.com/WrdCstlg/pro-thesis/internal/driver"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// `thesis replay --plan` drives a supplied operation trace exactly, in place of
// the workload the world recorded, and says when that is not the recorded
// workload: the trials are then about the world's schedule under another
// workload, which the verdict names.

func opTrace() *driver.Plan {
	return &driver.Plan{Schema: driver.PlanSchema, Profile: "smoke", Seed: 99, Operations: []driver.PlanOp{
		{OpID: 1, Process: 0, F: "write", Key: "k/0"},
		{OpID: 2, Process: 1, F: "read", Key: "k/0"},
	}}
}

func writePlanFile(t *testing.T, p *driver.Plan) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "generated.plan.json")
	if err := driver.WritePlan(path, p); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestASuppliedPlanIsChosenOverTheRecordedOne(t *testing.T) {
	dir := t.TempDir()
	w, p := recordedWorld(t, dir, "world.thesis", testPlan(), driver.PlanFileName)

	rec, err := ChoosePlan(p, w, "")
	if err != nil || rec.Source != PlanRecorded || rec.Path != filepath.Join(dir, driver.PlanFileName) || rec.Workload != "" {
		t.Fatalf("without --plan: %+v, err %v; want the recorded plan beside the world", rec, err)
	}

	gen := opTrace()
	gen.Seed = 5 // a generator's own seed; the world's is what runs
	sup, err := ChoosePlan(p, w, writePlanFile(t, gen))
	if err != nil {
		t.Fatalf("ChoosePlan: %v", err)
	}
	if sup.Source != PlanSupplied || sup.Plan == nil || len(sup.Plan.Operations) != 2 || sup.Plan.Seed != w.Seed {
		t.Fatalf("supplied: %+v; want the supplied trace under the world's seed", sup)
	}
	if sup.Workload == "" || sup.Workload == w.PlanDigest || !strings.Contains(sup.Note, "not the recorded world") {
		t.Fatalf("a supplied workload that is not the recorded one was not named: workload %q, note %q",
			sup.Workload, sup.Note)
	}

	// The supplied trace IS the recorded workload: nothing to qualify.
	same := opTrace()
	w2, p2 := recordedWorld(t, t.TempDir(), "world.thesis", same, driver.PlanFileName)
	if c, err := ChoosePlan(p2, w2, writePlanFile(t, same)); err != nil || c.Workload != "" {
		t.Fatalf("the recorded workload supplied again: workload %q, err %v; want none", c.Workload, err)
	}

	// Whose error it is decides the exit code: a bad --plan is the caller's.
	profile := testPlan()
	var supErr *SuppliedPlanError
	if _, err := ChoosePlan(p, w, writePlanFile(t, profile)); !errors.As(err, &supErr) {
		t.Fatalf("a profile plan supplied with --plan: err %v, want a SuppliedPlanError", err)
	}
	_, lost := recordedWorld(t, t.TempDir(), "world.thesis", testPlan(), "")
	if _, err := ChoosePlan(lost, w, ""); err == nil || errors.As(err, &supErr) {
		t.Fatalf("a world whose recorded plan is missing: err %v, want the world's error", err)
	}
}

// historyExec runs every attempt to a violation and writes the history the
// driver would have: the given op ids invoked in order.
type historyExec struct {
	t   *testing.T
	ids []int64
}

func (h historyExec) Execute(_ context.Context, req ExecRequest) Execution {
	var b strings.Builder
	for i, id := range h.ids {
		fmt.Fprintf(&b, `{"t_ns":%d,"process":%d,"type":"invoke","f":"read","key":"k/0","op_id":%d}`+"\n", 1000+i, i, id)
		fmt.Fprintf(&b, `{"t_ns":%d,"process":%d,"type":"ok","f":"read","key":"k/0","value":1,"op_id":%d}`+"\n", 1500+i, i, id)
	}
	hist := filepath.Join(h.t.TempDir(), "history.jsonl")
	if err := os.WriteFile(hist, []byte(b.String()), 0o644); err != nil {
		h.t.Fatal(err)
	}
	out := violatedRun
	out.Paths = control.WorldPaths{History: hist}
	return out
}

func TestAnAttemptWhoseDriverIgnoredTheOperationPlanIsInconclusive(t *testing.T) {
	dir := t.TempDir()
	w, p := recordedWorld(t, dir, "world.thesis", testPlan(), driver.PlanFileName)
	plan := opTrace()

	honoured, err := Run(context.Background(), Options{World: w, WorldPath: p, Plan: plan, Attempts: 1,
		Exec: historyExec{t: t, ids: []int64{1, 2}}, Workload: "sha256:" + strings.Repeat("cd", 32)})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if honoured.Attempts[0].Signal != SignalReproduced || honoured.ExitCode() != schema.ExitFail {
		t.Fatalf("a driver that executed the plan: %s (%s)", honoured.Attempts[0].Signal, honoured.Attempts[0].Reason)
	}
	v := honoured.Verdict(VerdictMeta{RunID: "r_2026_10_21_0003", Profile: "smoke", Commit: "abc1234",
		Artifacts: ".prothesis/runs/r_2026_10_21_0003/"})
	if v.Trials == nil || v.Trials.Workload != honoured.Workload || v.Trials.World != honoured.WorldHash {
		t.Fatalf("trials %+v do not name the supplied workload beside the world", v.Trials)
	}
	if err := v.Validate(); err != nil {
		t.Fatalf("verdict: %v", err)
	}

	ignored, err := Run(context.Background(), Options{World: w, WorldPath: p, Plan: plan, Attempts: 1,
		Exec: historyExec{t: t, ids: []int64{77}}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	a := ignored.Attempts[0]
	if a.Signal != SignalUnknown || ignored.ExitCode() != schema.ExitInconclusive || a.Reason == "" {
		t.Fatalf("a driver that generated its own workload: %s, exit %d, reason %q; want inconclusive",
			a.Signal, ignored.ExitCode(), a.Reason)
	}
	iv := ignored.Verdict(VerdictMeta{RunID: "r_2026_10_21_0005", Profile: "smoke", Commit: "abc1234",
		Artifacts: ".prothesis/runs/r_2026_10_21_0005/"})
	if iv.Verdict != schema.VerdictInconclusive || !strings.Contains(iv.Error, a.Reason) {
		t.Fatalf("the INCONCLUSIVE replay verdict's error %q does not carry the attempt's reason", iv.Error)
	}
}
