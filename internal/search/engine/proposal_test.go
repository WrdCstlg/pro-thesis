package engine

import (
	"context"
	"testing"

	"github.com/WrdCstlg/pro-thesis/internal/control"
	"github.com/WrdCstlg/pro-thesis/internal/recorder"
	"github.com/WrdCstlg/pro-thesis/internal/search"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// The engine forwarded only a proposal's ordinal, faults and label: a strategy
// proposing its own seed or another driver profile had them silently replaced,
// and the outcome was then hashed from the proposal, naming a world that never
// ran. The proposal is now executed as proposed, and the outcome names the
// world that actually ran.

type proposingStrategy struct {
	search.Strategy
	world schema.World
}

func (p proposingStrategy) Propose(context.Context, int) (schema.World, error) { return p.world, nil }

func proposed(seed uint64, profile string) schema.World {
	w := schema.NewWorld(seed, "default", profile)
	w.FaultSchedule.Planned = []string{"net.partition(kv-n1)@3000..7500"}
	w.Meta = &schema.WorldMeta{Origin: "test"}
	return w.Normalized()
}

func TestTheEngineExecutesTheSeedAndProfileItProposes(t *testing.T) {
	cfg := engineTestConfig(t, true)
	e, err := New(Options{Runner: newEngineRunner(t, cfg), Quiet: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	e.strategy = proposingStrategy{world: proposed(777, "linear")}
	ordinal := 0
	reqs, _, err := e.proposeBatch(context.Background(), 1, &ordinal)
	if err != nil || len(reqs) != 1 {
		t.Fatalf("proposeBatch: %d request(s), err %v", len(reqs), err)
	}
	if reqs[0].Seed == nil || *reqs[0].Seed != 777 {
		t.Fatalf("the proposed seed 777 was not forwarded: %v", reqs[0].Seed)
	}
	if reqs[0].DriverProfile != "linear" {
		t.Fatalf("the proposed driver profile was not forwarded: %q", reqs[0].DriverProfile)
	}
	if e.workloadNarrowed {
		t.Fatal("a driver profile a run profile names narrowed the run")
	}
}

// A driver profile no run profile names is not covered by the lock, so a world
// that drives it is not the gate's workload and the run cannot PASS.
func TestADriverProfileOutsideTheLockNarrowsTheSearch(t *testing.T) {
	cfg := engineTestConfig(t, true)
	cfg.Driver.Profiles["unlocked"] = schema.DriverProfile{Clients: 1, Ops: 1}
	e, err := New(Options{Runner: newEngineRunner(t, cfg), Quiet: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	e.strategy = proposingStrategy{world: proposed(1, "unlocked")}
	ordinal := 0
	if _, _, err := e.proposeBatch(context.Background(), 1, &ordinal); err != nil {
		t.Fatal(err)
	}
	if !e.workloadNarrowed {
		t.Fatal("a world driving an unlocked driver profile did not narrow the search")
	}
	res, _ := e.finish(context.Background(), Result{
		Worlds: []control.WorldOutcome{{Ordinal: 1, Outcome: control.OutcomePass}},
	}, "test", nil)
	if !res.Verdict.Budget.Narrowed || res.ExitCode == schema.ExitPass {
		t.Fatalf("narrowed %v, exit %d: a search off the locked workload reported PASS",
			res.Verdict.Budget.Narrowed, res.ExitCode)
	}
}

func TestTheOutcomeNamesTheWorldThatRan(t *testing.T) {
	dir := t.TempDir()
	paths := control.NewWorldPaths(dir)
	executed := proposed(777, "linear")
	executed.SUT.Images = []schema.SUTImage{{Service: "kv", Image: "kv:1", Digest: "sha256:aa"}}
	executed.PlanDigest = "sha256:bb"
	if err := recorder.StoreWorld(paths.World, &executed); err != nil {
		t.Fatal(err)
	}
	want, _ := executed.Hash()
	proposal := proposed(777, "linear")
	out := OutcomeOf(control.WorldOutcome{Ordinal: 1, Paths: paths}, proposal)
	if out.ExecutedHash != want {
		t.Fatalf("ExecutedHash = %q, want the executed world's %q", out.ExecutedHash, want)
	}
	if ph, _ := proposal.Hash(); out.WorldHash != ph {
		t.Fatalf("WorldHash = %q, want the proposal's %q (the strategies key their bookkeeping on it)", out.WorldHash, ph)
	}
}
