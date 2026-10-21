package control

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/WrdCstlg/pro-thesis/internal/driver"
	"github.com/WrdCstlg/pro-thesis/internal/recorder"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// topology_variant was hard-coded to "default", so a world could not say which
// build of the system under test it ran: the fixture's planted-bug build and
// its fixed build recorded the same identity. A target names the compose
// variable that selects its build (harness.variant_env); a profile may pin its
// value (profiles.<p>.variant); and the world records what was in force.

func loadFirstWorld(t *testing.T, r *Runner) *schema.World {
	t.Helper()
	w, err := recorder.LoadWorld(NewWorldPaths(joinRunWorld(r, 1)).World)
	if err != nil {
		t.Fatalf("load world.thesis: %v", err)
	}
	return w
}

func envHas(env []string, kv string) bool {
	for _, e := range env {
		if e == kv {
			return true
		}
	}
	return false
}

func TestAProfileVariantSelectsTheBuildAndIsRecorded(t *testing.T) {
	cfg := perturbTestConfig(t)
	cfg.Harness.VariantEnv = "KV_VARIANT"
	p := cfg.Profiles["smoke"]
	p.Variant = "kvfixed"
	cfg.Profiles["smoke"] = p
	r, backend := newPerturbTestRunner(t, cfg, nil)
	if _, exit, err := r.Run(context.Background()); err != nil || exit != schema.ExitPass {
		t.Fatalf("Run: exit %d, err %v", exit, err)
	}
	if len(backend.upEnvs) == 0 || !envHas(backend.upEnvs[0], "KV_VARIANT=kvfixed") {
		t.Fatalf("compose was not told to build the variant: env %v", backend.upEnvs)
	}
	if got := loadFirstWorld(t, r).TopologyVariant; got != "kvfixed" {
		t.Fatalf("world records topology_variant %q, want kvfixed", got)
	}
}

func TestAnInheritedVariantIsRecordedAsItWas(t *testing.T) {
	t.Setenv("KV_VARIANT", "kvfixed")
	cfg := perturbTestConfig(t)
	cfg.Harness.VariantEnv = "KV_VARIANT"
	r, _ := newPerturbTestRunner(t, cfg, nil)
	if _, _, err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := loadFirstWorld(t, r).TopologyVariant; got != "kvfixed" {
		t.Fatalf("world records topology_variant %q, want the inherited kvfixed", got)
	}
}

func TestWithNoVariantEnvTheWorldSaysDefault(t *testing.T) {
	r, _ := newPerturbTestRunner(t, perturbTestConfig(t), nil)
	if _, _, err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := loadFirstWorld(t, r).TopologyVariant; got != "default" {
		t.Fatalf("topology_variant = %q, want default", got)
	}
}

// A command-line variant other than the profile's runs a different system from
// the one the lock-covered profile names, so the run cannot PASS.
func TestACommandLineVariantOtherThanTheProfileNarrows(t *testing.T) {
	cfg := perturbTestConfig(t)
	cfg.Harness.VariantEnv = "KV_VARIANT"
	p := cfg.Profiles["smoke"]
	p.Variant = "buggy"
	cfg.Profiles["smoke"] = p
	r, backend := newPerturbTestRunner(t, cfg, nil)
	r.variant, r.variantPin, r.variantNarrowed = resolveVariant(cfg, p, "kvfixed")
	v, exit, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if exit != schema.ExitInconclusive || !v.Budget.Narrowed {
		t.Fatalf("exit %d, narrowed %v: a run on another build than the profile's cannot pass", exit, v.Budget.Narrowed)
	}
	if !envHas(backend.upEnvs[0], "KV_VARIANT=kvfixed") || loadFirstWorld(t, r).TopologyVariant != "kvfixed" {
		t.Fatal("the command-line variant was not the one built and recorded")
	}
}

// The world names the workload its driver ran: plan_digest is the recorded
// plan.json's workload digest.
func TestTheWorldRecordsItsPlanDigest(t *testing.T) {
	r, _ := newPerturbTestRunner(t, perturbTestConfig(t), nil)
	if _, _, err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	w := loadFirstWorld(t, r)
	if w.Schema != schema.WorldSchema {
		t.Fatalf("a new world is %q, want %q", w.Schema, schema.WorldSchema)
	}
	raw, err := os.ReadFile(filepath.Join(joinRunWorld(r, 1), driver.PlanFileName))
	if err != nil {
		t.Fatalf("read plan.json: %v", err)
	}
	plan, err := driver.ParsePlan(raw)
	if err != nil {
		t.Fatalf("parse plan.json: %v", err)
	}
	want, err := plan.WorkloadDigest()
	if err != nil {
		t.Fatal(err)
	}
	if w.PlanDigest == "" || w.PlanDigest != want {
		t.Fatalf("plan_digest = %q, want %q (the recorded plan.json's workload)", w.PlanDigest, want)
	}
}
