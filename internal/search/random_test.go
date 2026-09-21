package search

import (
	"context"
	"strings"
	"testing"

	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// TestRandomBaselineRespectsAllowDenyAndBudget is the fairness floor: the
// baseline must be a legal searcher, not a strawman and not a cheat. Every
// world it proposes has to survive the same compiler the Saboteur's worlds do.
func TestRandomBaselineRespectsAllowDenyAndBudget(t *testing.T) {
	p := testParams(t, 0x5EED)
	r, err := NewRandom(p)
	if err != nil {
		t.Fatalf("NewRandom: %v", err)
	}
	allowed := map[schema.FaultKind]bool{}
	for _, k := range p.Config.Perturber.EffectiveFaultKinds() {
		allowed[k] = true
	}
	denied := map[schema.FaultKind]bool{}
	for _, k := range p.Config.Perturber.Deny {
		denied[k] = true
	}

	ctx := context.Background()
	seen := map[schema.FaultKind]int{}
	for i := 0; i < 300; i++ {
		w, err := r.Propose(ctx, i)
		if err != nil {
			t.Fatalf("Propose(%d): %v", i, err)
		}
		if err := p.Validator.ValidateWorld(&w); err != nil {
			t.Fatalf("world %d does not compile: %v\n  %v", i, err, w.FaultSchedule.Planned)
		}
		if n := len(w.FaultSchedule.Planned); n > p.Space.MaxFaults {
			t.Fatalf("world %d has %d faults, over the shared ceiling of %d", i, n, p.Space.MaxFaults)
		}
		for _, f := range w.FaultSchedule.Planned {
			spec, err := schema.ParseFault(f)
			if err != nil {
				t.Fatalf("world %d emitted an unparseable fault %q: %v", i, f, err)
			}
			if denied[spec.Kind] {
				t.Fatalf("world %d used %s, which is in perturber.deny", i, spec.Kind)
			}
			if !allowed[spec.Kind] {
				t.Fatalf("world %d used %s, which is outside perturber.allow", i, spec.Kind)
			}
			seen[spec.Kind]++
		}
	}
	// A uniform baseline must actually reach the whole allowed space, or it is
	// a strawman in a different disguise.
	for k := range allowed {
		if seen[k] == 0 {
			t.Errorf("300 random worlds never used %s; the baseline does not cover the allowed space", k)
		}
	}
}

// TestRandomBaselineNeverPartitionsAMajority: the fixture's own safety
// constraint. A search that emitted it would manufacture a false violation by
// destroying quorum, which is the constraint's whole purpose.
func TestRandomBaselineNeverPartitionsAMajority(t *testing.T) {
	p := testParams(t, 0xC0FFEE)
	r, err := NewRandom(p)
	if err != nil {
		t.Fatalf("NewRandom: %v", err)
	}
	ctx := context.Background()
	for i := 0; i < 300; i++ {
		w, err := r.Propose(ctx, i)
		if err != nil {
			t.Fatalf("Propose(%d): %v", i, err)
		}
		for _, f := range w.FaultSchedule.Planned {
			spec, err := schema.ParseFault(f)
			if err != nil {
				t.Fatalf("parse %q: %v", f, err)
			}
			if spec.Kind != schema.FaultNetPartition {
				continue
			}
			switch spec.Target.Kind {
			case schema.TargetWildcard:
				t.Fatalf("world %d partitions a whole service: %s", i, f)
			case schema.TargetQuorum:
				if spec.Target.Func == schema.QuorumMajority {
					t.Fatalf("world %d partitions a majority: %s", i, f)
				}
			}
		}
	}
}

// TestRandomBaselineStartsWithTheSeedSweepAndTheControl.
//
// The no-fault CONTROL is not optional. Every DAMPEN/REINFORCE criterion is
// comparative and every coverage delta is measured against a baseline; without
// a control the templates a healthy system emits are indistinguishable from the
// ones a fault caused (D-028 item 6). It must also come FIRST, so a run that
// exhausts its budget early has still measured it.
func TestRandomBaselineStartsWithTheSeedSweepAndTheControl(t *testing.T) {
	p := testParams(t, 0xABCD)
	r, err := NewRandom(p)
	if err != nil {
		t.Fatalf("NewRandom: %v", err)
	}
	seeds := r.Seeds()
	if len(seeds) == 0 {
		t.Fatal("the baseline has no seed corpus")
	}
	if n := len(seeds[0].FaultSchedule.Planned); n != 0 {
		t.Fatalf("the first seed world has %d faults; the no-fault control must come first", n)
	}

	kindsSeeded := map[schema.FaultKind]bool{}
	for _, w := range seeds[1:] {
		if len(w.FaultSchedule.Planned) != 1 {
			t.Fatalf("a seed world has %d faults, want exactly 1: %v",
				len(w.FaultSchedule.Planned), w.FaultSchedule.Planned)
		}
		spec, err := schema.ParseFault(w.FaultSchedule.Planned[0])
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		kindsSeeded[spec.Kind] = true
	}
	for _, k := range p.Config.Perturber.EffectiveFaultKinds() {
		if !kindsSeeded[k] {
			t.Errorf("no seed world exercises %s", k)
		}
	}

	ctx := context.Background()
	for i := range seeds {
		w, err := r.Propose(ctx, i)
		if err != nil {
			t.Fatalf("Propose(%d): %v", i, err)
		}
		if mustHash(t, w) != mustHash(t, seeds[i]) {
			t.Fatalf("Propose(%d) did not hand out seed world %d", i, i)
		}
	}
}

// TestRandomBaselineIsReproducible: same seed, same worlds, in the same order.
// A.8 requires it, and D-029's benchmark needs a trial to be re-runnable.
func TestRandomBaselineIsReproducible(t *testing.T) {
	run := func(seed uint64) []string {
		p := testParams(t, seed)
		r, err := NewRandom(p)
		if err != nil {
			t.Fatalf("NewRandom: %v", err)
		}
		out := make([]string, 0, 40)
		for i := 0; i < 40; i++ {
			w, err := r.Propose(context.Background(), i)
			if err != nil {
				t.Fatalf("Propose: %v", err)
			}
			out = append(out, mustHash(t, w))
		}
		return out
	}
	first := strings.Join(run(0x1234), ",")
	for i := 0; i < 3; i++ {
		if got := strings.Join(run(0x1234), ","); got != first {
			t.Fatalf("the baseline is not reproducible on repeat %d", i)
		}
	}
	if strings.Join(run(0x4321), ",") == first {
		t.Fatal("two different seeds produced identical worlds; the seed is not reaching generation")
	}
}

// TestWorldNIsIndependentOfWorldsBeforeIt.
//
// The generation stream is keyed by ORDINAL, so a run that retried, skipped, or
// spent extra draws on an earlier world still produces the same world N. A
// search whose world N depended on how many values worlds 1..N-1 happened to
// consume would not replay, which is what A.8 forbids.
func TestWorldNIsIndependentOfWorldsBeforeIt(t *testing.T) {
	p := testParams(t, 0x9999)
	r, err := NewRandom(p)
	if err != nil {
		t.Fatalf("NewRandom: %v", err)
	}
	ctx := context.Background()
	n := len(r.Seeds()) + 11

	// In order.
	var inOrder schema.World
	for i := 0; i <= n; i++ {
		w, err := r.Propose(ctx, i)
		if err != nil {
			t.Fatalf("Propose: %v", err)
		}
		if i == n {
			inOrder = w
		}
	}

	// Straight to n, on a fresh strategy with the same seed.
	p2 := testParams(t, 0x9999)
	r2, err := NewRandom(p2)
	if err != nil {
		t.Fatalf("NewRandom: %v", err)
	}
	direct, err := r2.Propose(ctx, n)
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if mustHash(t, inOrder) != mustHash(t, direct) {
		t.Fatalf("world %d depends on the worlds before it:\n  in order: %v\n  direct:   %v",
			n, inOrder.FaultSchedule.Planned, direct.FaultSchedule.Planned)
	}
}

// TestRandomBaselineIgnoresFeedback is the definition of the baseline. A
// "random" strategy that quietly reweighted itself from coverage would make
// D-029's comparison a contest between two adaptive searchers while calling one
// of them uniform random.
func TestRandomBaselineIgnoresFeedback(t *testing.T) {
	ctx := context.Background()
	worlds := func(feed bool) []string {
		p := testParams(t, 0x777)
		r, err := NewRandom(p)
		if err != nil {
			t.Fatalf("NewRandom: %v", err)
		}
		out := make([]string, 0, 30)
		for i := 0; i < 30; i++ {
			w, err := r.Propose(ctx, i)
			if err != nil {
				t.Fatalf("Propose: %v", err)
			}
			out = append(out, mustHash(t, w))
			if feed {
				// A violation with a large coverage delta: the strongest possible
				// signal. It must change nothing.
				if err := r.Observe(ctx, Outcome{
					WorldHash: out[len(out)-1],
					Observed:  true,
					Findings: []Finding{
						violatedFinding("linearizable.kv", schema.ClassConsistency, schema.SeverityHigh),
					},
					Coverage:   Delta{NewTemplates: 99, NewStates: 99},
					DurationMS: 30000,
				}); err != nil {
					t.Fatalf("Observe: %v", err)
				}
			}
		}
		return out
	}
	blind := strings.Join(worlds(false), ",")
	fed := strings.Join(worlds(true), ",")
	if blind != fed {
		t.Fatal("the random baseline changed its proposals after being told a world found a violation; " +
			"it is not a uniform-random baseline")
	}
}

// TestBothStrategiesShareTheSameSpaceAndCeiling.
//
// This is the structural half of D-029's fairness requirement. Params is the
// only construction path, so any strategy built from the same Params draws from
// the same space with the same ceiling. A test that asserted the two arms'
// numbers agreed would prove nothing; this asserts they cannot disagree.
func TestBothStrategiesShareTheSameSpaceAndCeiling(t *testing.T) {
	p := testParams(t, 1)
	if p.Space.MaxFaults > MaxFaultsPerWorld {
		t.Fatalf("Space.MaxFaults = %d exceeds the shared ceiling %d", p.Space.MaxFaults, MaxFaultsPerWorld)
	}
	if p.Space.MaxFaults > p.Config.Perturber.Budget.MaxFaultsPerWorld {
		t.Fatalf("Space.MaxFaults = %d exceeds perturber.budget.max_faults_per_world = %d",
			p.Space.MaxFaults, p.Config.Perturber.Budget.MaxFaultsPerWorld)
	}
	for _, k := range p.Space.Kinds {
		if len(p.Space.Targets[k]) == 0 {
			continue
		}
		if len(p.Space.Rungs(k)) == 0 {
			t.Fatalf("%s has no magnitude rung; it would be unselectable", k)
		}
	}
}

// TestSpaceHonoursDeny: a denied kind must not appear in the enumerated space
// at all, so no strategy can reach it by accident.
func TestSpaceHonoursDeny(t *testing.T) {
	p := testParams(t, 1)
	for _, k := range p.Space.Kinds {
		for _, denied := range p.Config.Perturber.Deny {
			if k == denied {
				t.Fatalf("the space enumerates %s, which is in perturber.deny", k)
			}
		}
	}
	if len(p.Space.Kinds) != len(p.Config.Perturber.EffectiveFaultKinds()) {
		t.Fatalf("the space has %d kinds, perturber permits %d",
			len(p.Space.Kinds), len(p.Config.Perturber.EffectiveFaultKinds()))
	}
}

// TestAnEdgeTargetOnlyReachesTheNetworkFamily: the grammar refuses an edge
// target for a process fault, so the space must not offer one.
func TestAnEdgeTargetOnlyReachesTheNetworkFamily(t *testing.T) {
	p := testParams(t, 1)
	for _, k := range p.Space.Kinds {
		decl, ok := schema.LookupFaultKind(k)
		if !ok {
			t.Fatalf("unknown kind in space: %s", k)
		}
		for _, tgt := range p.Space.Targets[k] {
			if tgt.Kind == schema.TargetEdge && !decl.AllowEdge {
				t.Fatalf("the space offers edge target %s to %s, which acts on a node", tgt, k)
			}
		}
	}
}

// TestGenerationFailsLoudlyRatherThanTruncating: "never emit a world the
// compiler will reject" is only true if the failure to build one is an error.
// A silently truncated schedule would run a world with fewer faults than the
// search believed it injected.
func TestGenerationFailsLoudlyRatherThanTruncating(t *testing.T) {
	cfg := testConfig()
	// A constraint no partition can satisfy, combined with an allow list of one
	// partitioning kind: nothing is schedulable.
	cfg.Perturber.Allow = []schema.FaultKind{schema.FaultNetPartition}
	cfg.Perturber.Deny = nil
	cfg.Perturber.Constraints = []string{"never partition more than 0 of kv"}

	top := testTopology(t)
	sp, err := NewSpace(cfg, top, DefaultWindowPolicy())
	if err != nil {
		t.Fatalf("NewSpace: %v", err)
	}
	v, err := NewValidator(cfg, top, nil)
	if err != nil {
		t.Fatalf("NewValidator: %v", err)
	}
	st := NewStreams(1).Get(PathRandomWorld)

	// role: targets bind at injection time and are not refused at compile time,
	// so some schedules still pass. What must never happen is a silent partial
	// schedule; force the failure by asking for a schedule over a space whose
	// only static targets are all refused.
	sawFailure := false
	for i := 0; i < 200; i++ {
		planned, err := RandomSchedule(sp, v, st)
		if err != nil {
			if !strings.Contains(err.Error(), ErrNoValidCandidate.Error()) {
				t.Fatalf("unexpected error: %v", err)
			}
			sawFailure = true
			continue
		}
		if len(planned) == 0 {
			t.Fatal("RandomSchedule returned an empty schedule instead of an error")
		}
		if err := v.Validate(planned); err != nil {
			t.Fatalf("RandomSchedule returned a schedule the compiler refuses: %v\n  %v", err, planned)
		}
	}
	_ = sawFailure // A space that happens to stay schedulable is fine; a silent truncation is not.
}
