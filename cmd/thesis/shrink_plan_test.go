package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/WrdCstlg/pro-thesis/internal/driver"
	"github.com/WrdCstlg/pro-thesis/internal/shrink"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// A shrink used to drive a workload regenerated from the current config for
// every candidate that kept all operations, and built its operation plan on the
// config's profile, whatever the world had recorded. Every candidate now drives
// the chosen workload (a supplied --plan, or the recorded plan) or a subset of
// it, exactly.
func TestShrinkCandidatesDriveTheChosenWorkload(t *testing.T) {
	world := schema.NewWorld(7, "default", "linear")
	worldPath := filepath.Join(t.TempDir(), "world.thesis")

	trace := &driver.Plan{Schema: driver.PlanSchema, Profile: "linear", Seed: 7, Operations: []driver.PlanOp{
		{OpID: 1, Process: 0, F: "write", Key: "k/0"},
		{OpID: 2, Process: 1, F: "read", Key: "k/0"},
		{OpID: 3, Process: 0, F: "read", Key: "k/0"},
	}}

	t.Run("an operation plan is reduced as it stands", func(t *testing.T) {
		full, ops := shrinkWorkload(nil, &world, worldPath, trace, false)
		if full != trace || len(ops) != 3 || ops[0] != 1 || ops[2] != 3 {
			t.Fatalf("full %p ops %v, want the supplied trace and its op ids", full, ops)
		}
		e := &shrinkRunnerExecutor{wholePlan: trace, fullPlan: full}
		if p, err := e.planFor(shrink.Candidate{}); err != nil || p != trace {
			t.Fatalf("a candidate keeping every operation drove %p (err %v), want the whole trace", p, err)
		}
		p, err := e.planFor(shrink.Candidate{Ops: []int64{1, 3}})
		if err != nil || len(p.Operations) != 2 || p.Operations[1].OpID != 3 {
			t.Fatalf("a reduced candidate drove %+v (err %v), want ops 1 and 3", p, err)
		}
	})

	t.Run("--skip-ops still drives the whole plan", func(t *testing.T) {
		full, ops := shrinkWorkload(nil, &world, worldPath, trace, true)
		if full != nil || ops != nil {
			t.Fatalf("stage 2 was given work under --skip-ops: %v", ops)
		}
		e := &shrinkRunnerExecutor{wholePlan: trace}
		if p, _ := e.planFor(shrink.Candidate{}); p != trace {
			t.Fatal("--skip-ops dropped the supplied workload")
		}
	})

	t.Run("a recorded profile plan is the base the history's trace is attached to", func(t *testing.T) {
		dir := t.TempDir()
		hist := `{"t_ns":1000,"process":0,"type":"invoke","f":"write","key":"k/0","value":1,"op_id":11}
{"t_ns":1100,"process":0,"type":"ok","f":"write","key":"k/0","value":1,"op_id":11}
{"t_ns":1200,"process":1,"type":"invoke","f":"read","key":"k/0","op_id":12}
`
		if err := os.WriteFile(filepath.Join(dir, "history.jsonl"), []byte(hist), 0o644); err != nil {
			t.Fatal(err)
		}
		recorded := &driver.Plan{Schema: driver.PlanSchema, Profile: "linear", Seed: 7, Clients: 3, Ops: 900,
			Mix: []driver.MixEntry{{Op: "read", WeightPPM: 1000000}}}
		full, ops := shrinkWorkload(nil, &world, filepath.Join(dir, "world.thesis"), recorded, false)
		if full == nil || len(ops) != 2 || full.Clients != 3 || full.Ops != 900 {
			t.Fatalf("full %+v ops %v, want the history's two ops on the recorded profile", full, ops)
		}
		e := &shrinkRunnerExecutor{wholePlan: recorded, fullPlan: full}
		if p, _ := e.planFor(shrink.Candidate{}); p != recorded {
			t.Fatal("a candidate keeping every operation did not drive the recorded plan")
		}
	})

	t.Run("a world that recorded no plan regenerates, as it always did", func(t *testing.T) {
		full, ops := shrinkWorkload(nil, &world, worldPath, nil, false)
		if full != nil || ops != nil {
			t.Fatalf("an operation plan appeared from nowhere: %v", ops)
		}
		if p, _ := (&shrinkRunnerExecutor{}).planFor(shrink.Candidate{}); p != nil {
			t.Fatal("a world with no recorded plan was handed one")
		}
	})
}
