package replay

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/WrdCstlg/pro-thesis/internal/control"
	"github.com/WrdCstlg/pro-thesis/internal/driver"
	"github.com/WrdCstlg/pro-thesis/internal/recorder"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// Replay used to regenerate the driver workload from the CURRENT config and
// never looked at the plan the world recorded, so an edited driver profile
// silently changed what a replay drove. A v2 world names its workload by
// digest; replay drives the recorded plan.json and refuses one that does not
// match.

func recordedWorld(t *testing.T, dir, worldFile string, plan *driver.Plan, planFile string) (*schema.World, string) {
	t.Helper()
	w := schema.NewWorld(99, "default", "smoke")
	w.SUT.Images = []schema.SUTImage{{Service: "kv", Image: "kv:fixed", Digest: "sha256:aaaa"}}
	if plan != nil {
		d, err := plan.WorkloadDigest()
		if err != nil {
			t.Fatal(err)
		}
		w.PlanDigest = d
		if planFile != "" {
			if err := driver.WritePlan(filepath.Join(dir, planFile), plan); err != nil {
				t.Fatal(err)
			}
		}
	}
	w = w.Normalized()
	p := filepath.Join(dir, worldFile)
	if err := recorder.StoreWorld(p, &w); err != nil {
		t.Fatal(err)
	}
	return &w, p
}

func testPlan() *driver.Plan {
	return &driver.Plan{Schema: driver.PlanSchema, Profile: "smoke", Seed: 99, Clients: 1, Ops: 7,
		HistoryPath: "/elsewhere/history.jsonl", Mix: []driver.MixEntry{{Op: "read", WeightPPM: 1000000}}}
}

func TestTheRecordedPlanIsFoundAndChecked(t *testing.T) {
	t.Run("beside a run's world.thesis", func(t *testing.T) {
		dir := t.TempDir()
		w, p := recordedWorld(t, dir, "world.thesis", testPlan(), driver.PlanFileName)
		got, note, err := LoadRecordedPlan(p, w)
		if err != nil || got == nil || got.Ops != 7 {
			t.Fatalf("plan %+v, note %q, err %v", got, note, err)
		}
	})
	t.Run("beside a corpus entry", func(t *testing.T) {
		dir := t.TempDir()
		w, p := recordedWorld(t, dir, "w_abcd.thesis", testPlan(), "w_abcd.plan.json")
		if got, _, err := LoadRecordedPlan(p, w); err != nil || got == nil {
			t.Fatalf("plan %+v, err %v", got, err)
		}
	})
	t.Run("a plan that is not the recorded workload", func(t *testing.T) {
		dir := t.TempDir()
		edited := testPlan()
		edited.Ops = 5
		w, p := recordedWorld(t, dir, "world.thesis", testPlan(), "")
		if err := driver.WritePlan(filepath.Join(dir, driver.PlanFileName), edited); err != nil {
			t.Fatal(err)
		}
		if _, _, err := LoadRecordedPlan(p, w); err == nil || !strings.Contains(err.Error(), "plan_digest") {
			t.Fatalf("an edited plan.json was accepted (err %v)", err)
		}
	})
	t.Run("a v2 world whose plan is missing", func(t *testing.T) {
		dir := t.TempDir()
		w, p := recordedWorld(t, dir, "world.thesis", testPlan(), "")
		if _, _, err := LoadRecordedPlan(p, w); err == nil {
			t.Fatal("a world that names its workload replayed without it")
		}
	})
	t.Run("a v1 world regenerates, and says so", func(t *testing.T) {
		dir := t.TempDir()
		w := schema.NewWorld(1, "default", "smoke")
		w.Schema = schema.WorldSchemaV1
		w = w.Normalized()
		p := filepath.Join(dir, "world.thesis")
		if err := recorder.StoreWorld(p, &w); err != nil {
			t.Fatal(err)
		}
		got, note, err := LoadRecordedPlan(p, &w)
		if err != nil || got != nil || !strings.Contains(note, "regenerated") {
			t.Fatalf("plan %+v, note %q, err %v", got, note, err)
		}
	})
}

type capturingExec struct{ reqs []ExecRequest }

func (c *capturingExec) Execute(_ context.Context, req ExecRequest) Execution {
	c.reqs = append(c.reqs, req)
	return Execution{Outcome: control.OutcomePass}
}

func TestReplayHandsTheRecordedPlanAndImagesToEveryAttempt(t *testing.T) {
	dir := t.TempDir()
	plan := testPlan()
	w, p := recordedWorld(t, dir, "world.thesis", plan, driver.PlanFileName)
	exec := &capturingExec{}
	if _, err := Run(context.Background(), Options{World: w, WorldPath: p, Exec: exec, Plan: plan, Attempts: 2}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(exec.reqs) != 2 {
		t.Fatalf("%d attempts, want 2", len(exec.reqs))
	}
	for _, r := range exec.reqs {
		if r.Plan == nil || r.Plan.Ops != 7 {
			t.Fatalf("an attempt was not handed the recorded plan: %+v", r.Plan)
		}
		if len(r.ExpectImages) != 1 || r.ExpectImages[0].Digest != "sha256:aaaa" {
			t.Fatalf("an attempt was not told the recorded images: %+v", r.ExpectImages)
		}
	}
}

// `thesis replay -k` writes a machine-readable verdict: the trial count and how
// many reproduced, beside the usual fields.
func TestAReplayVerdictCountsItsTrials(t *testing.T) {
	rep := Report{
		WorldHash: "sha256:" + strings.Repeat("ab", 32), Planned: 3,
		Attempts: []Attempt{
			{N: 1, Signal: SignalReproduced}, {N: 2, Signal: SignalNotReproduced}, {N: 3, Signal: SignalUnknown},
		},
	}
	rep.Confirmation = tally(rep.Attempts)
	v := rep.Verdict(VerdictMeta{RunID: "r_2026_10_21_0001", Profile: "linear", Commit: "abc1234", Artifacts: ".prothesis/runs/r_2026_10_21_0001/"})
	if v.Trials == nil || v.Trials.Attempts != 3 || v.Trials.Reproduced != 1 || v.Trials.Inconclusive != 1 {
		t.Fatalf("trials = %+v, want 3 attempts, 1 reproduced, 1 inconclusive", v.Trials)
	}
	if v.Trials.World != rep.WorldHash {
		t.Fatalf("the verdict does not name the replayed world: %q", v.Trials.World)
	}
	if v.Verdict != schema.VerdictFail || v.Verdict.ExitCode() != rep.ExitCode() {
		t.Fatalf("verdict %s (exit %d) disagrees with the report's exit %d", v.Verdict, v.Verdict.ExitCode(), rep.ExitCode())
	}
	if err := v.Validate(); err != nil {
		t.Fatalf("the replay verdict does not validate: %v", err)
	}
}
