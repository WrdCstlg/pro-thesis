package saboteur

import (
	"errors"
	"strings"
	"testing"

	"github.com/WrdCstlg/pro-thesis/internal/perturber"
	"github.com/WrdCstlg/pro-thesis/internal/perturber/faults"
	"github.com/WrdCstlg/pro-thesis/internal/recorder"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// ---------------------------------------------------------------------------
// fixtures
// ---------------------------------------------------------------------------

// ladderConfig is the kv fixture's own perturber block: the allow list it
// actually ships with, including io.latency, which is allowed by config and
// NOT implementable on this platform (D-033b, OQ-021).
func ladderConfig(t *testing.T, allow []schema.FaultKind, deny []schema.FaultKind, constraints ...string) *schema.Config {
	t.Helper()
	cfg := schema.DefaultConfig()
	cfg.Perturber.Allow = allow
	cfg.Perturber.Deny = deny
	cfg.Perturber.Constraints = constraints
	return &cfg
}

func fixtureAllow() []schema.FaultKind {
	return []schema.FaultKind{
		schema.FaultNetPartition, schema.FaultNetLatency, schema.FaultNetLoss,
		schema.FaultProcKill, schema.FaultProcPause, schema.FaultIOLatency,
	}
}

func ladderTopology(t *testing.T) *perturber.Topology {
	t.Helper()
	top, err := perturber.NewTopologyFromNodes([]perturber.Node{
		{ID: "kv-n1", Service: "kv", ComposeService: "kv-n1", ContainerID: "c1"},
		{ID: "kv-n2", Service: "kv", ComposeService: "kv-n2", ContainerID: "c2"},
		{ID: "kv-n3", Service: "kv", ComposeService: "kv-n3", ContainerID: "c3"},
	})
	if err != nil {
		t.Fatalf("topology: %v", err)
	}
	return top
}

// allSupported is the platform predicate for tests that want the full ladder
// regardless of what Docker Desktop can deliver.
func allSupported(schema.FaultKind) error { return nil }

func ladderOpts(t *testing.T, cfg *schema.Config) LadderOptions {
	t.Helper()
	return LadderOptions{
		Config:        cfg,
		Topology:      ladderTopology(t),
		Seed:          recorder.Seed(0xC0FFEE),
		AtMS:          8200,
		KindSupported: allSupported,
	}
}

func mustGenerate(t *testing.T, opts LadderOptions) LadderPlan {
	t.Helper()
	p, err := Generate(opts)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	return p
}

// ---------------------------------------------------------------------------
// every action must be executable
// ---------------------------------------------------------------------------

// The ladder's whole value is that the search never spends a ~30 s world on a
// fault string that cannot run. Every generated fault must parse under the
// frozen grammar AND survive perturber.Compile against the same config and
// topology the runner will use.
func TestEveryGeneratedFaultParsesAndCompiles(t *testing.T) {
	cfg := ladderConfig(t, fixtureAllow(), nil)
	opts := ladderOpts(t, cfg)
	plan := mustGenerate(t, opts)

	if len(plan.Actions) == 0 {
		t.Fatal("the fixture's allow list must produce actions; an empty ladder is a broken ladder")
	}
	for _, a := range plan.Actions {
		for i, f := range a.Faults {
			spec, err := schema.ParseFault(f)
			if err != nil {
				t.Fatalf("%s: %q does not parse: %v", a.Rung, f, err)
			}
			if spec.Kind != a.Kinds[i] {
				t.Fatalf("%s: Kinds[%d] = %s but the fault is %s", a.Rung, i, a.Kinds[i], spec.Kind)
			}
			// Canonical form: what goes into a world's fault_schedule.planned.
			canon, err := schema.CanonicalFault(f)
			if err != nil || canon != f {
				t.Fatalf("%s: %q is not canonical (canonical form is %q, err %v)", a.Rung, f, canon, err)
			}
		}
		if _, err := perturber.Compile(perturber.CompileOptions{
			Config:   cfg,
			Topology: opts.Topology,
			Planned:  a.Faults,
		}); err != nil {
			t.Fatalf("%s: %v does not compile: %v", a.Rung, a.Faults, err)
		}
	}
}

func TestActionsComeOutInRungOrder(t *testing.T) {
	plan := mustGenerate(t, ladderOpts(t, ladderConfig(t, fixtureAllow(), nil)))
	last := Rung(-1)
	for _, a := range plan.Actions {
		if a.Rung < last {
			t.Fatalf("actions are out of ladder order: %s came after %s", a.Rung, last)
		}
		last = a.Rung
	}
	if last < RungCompound {
		t.Fatalf("the fixture's allow list reaches the compound rung; highest rung reached was %s", last)
	}
}

// ---------------------------------------------------------------------------
// allow / deny / platform
// ---------------------------------------------------------------------------

// A rung whose kind is not allowed yields NOTHING, and says why. It is never an
// error: a search that aborts because rung 5 is denied is worse than one that
// climbs the five rungs it has.
func TestADisallowedKindYieldsNothingRatherThanAnError(t *testing.T) {
	cfg := ladderConfig(t, []schema.FaultKind{schema.FaultProcPause}, nil)
	plan := mustGenerate(t, ladderOpts(t, cfg))

	for _, a := range plan.Actions {
		for _, k := range a.Kinds {
			if k != schema.FaultProcPause {
				t.Fatalf("%s generated %s, which is not in perturber.allow", a.Rung, k)
			}
		}
	}
	if len(plan.ActionsFor(RungGrayFail)) == 0 {
		t.Fatal("proc.pause is allowed, so the gray-failure rung must still produce actions")
	}
	if len(plan.ActionsFor(RungPartition)) != 0 {
		t.Fatal("net.partition is not allowed and the partition rung must be empty")
	}
	// Silence is not acceptable: an empty rung must be distinguishable from a
	// rung that was tried and found nothing.
	//
	// The skip must name the BARE KIND, which is what the pre-check emits. The
	// allow list is enforced twice (here and again inside perturber.Compile)
	// and asserting on the bare kind is what distinguishes "refused before a
	// fault string was built" from "built, then refused by the compiler". Both
	// are safe; only one gives a diagnostic an operator can act on.
	found := false
	for _, s := range plan.SkipsFor(RungPartition) {
		if s.What == string(schema.FaultNetPartition) && strings.Contains(s.Reason, "perturber.allow") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the partition rung must record WHY it is empty, naming the kind; skips were %+v",
			plan.SkipsFor(RungPartition))
	}
}

func TestADeniedKindIsNeverGenerated(t *testing.T) {
	cfg := ladderConfig(t, nil, []schema.FaultKind{schema.FaultProcPause, schema.FaultNetPartition})
	plan := mustGenerate(t, ladderOpts(t, cfg))
	for _, a := range plan.Actions {
		for _, k := range a.Kinds {
			if k == schema.FaultProcPause || k == schema.FaultNetPartition {
				t.Fatalf("%s generated denied kind %s", a.Rung, k)
			}
		}
	}
	if len(plan.ActionsFor(RungCompound)) != 0 {
		t.Fatal("both compound kinds are denied, so the compound rung must be empty")
	}
}

// The default platform predicate is faults.PlatformCapability, and it matters:
// the fixture's own allow list contains io.latency, which the frozen registry
// declares and Docker Desktop cannot deliver. Generating it would burn a whole
// world to reach ErrUnsupported mid-DRIVE.
func TestAnUnimplementableKindIsNotGeneratedByDefault(t *testing.T) {
	opts := ladderOpts(t, ladderConfig(t, fixtureAllow(), nil))
	opts.KindSupported = nil // fall back to the real platform predicate
	plan := mustGenerate(t, opts)

	for _, a := range plan.Actions {
		for _, k := range a.Kinds {
			if k == schema.FaultIOLatency {
				t.Fatalf("%s generated io.latency, which %v", a.Rung, faults.PlatformCapability(k))
			}
		}
	}
	explained := false
	for _, s := range plan.Skipped {
		if s.What == string(schema.FaultIOLatency) && strings.Contains(s.Reason, "io.latency") {
			explained = true
		}
	}
	if !explained {
		t.Fatal("io.latency must be skipped WITH the measured reason, not silently omitted")
	}

	// And with the predicate relaxed it comes back, so the test above is about
	// the platform and not about a typo in the kind list.
	opts.KindSupported = allSupported
	relaxed := mustGenerate(t, opts)
	saw := false
	for _, a := range relaxed.Actions {
		for _, k := range a.Kinds {
			if k == schema.FaultIOLatency {
				saw = true
			}
		}
	}
	if !saw {
		t.Fatal("with the platform predicate relaxed, rung 1 must generate io.latency")
	}
}

// ---------------------------------------------------------------------------
// Rung 4: the D-031 shape
// ---------------------------------------------------------------------------

// The canonical Raft lease-bug trigger, byte for byte. D-031 established this
// schedule by MEASUREMENT after the directive's own pairing was found incapable
// of firing for three independent reasons.
func TestCompoundRungLeadsWithTheD031Schedule(t *testing.T) {
	plan := mustGenerate(t, ladderOpts(t, ladderConfig(t, fixtureAllow(), nil)))
	compound := plan.ActionsFor(RungCompound)
	if len(compound) == 0 {
		t.Fatal("no compound actions")
	}
	first := compound[0]
	want := []string{
		"net.partition(role:leader)@8200..13500",
		"proc.pause(role:leader)@8300..11000",
	}
	if len(first.Faults) != 2 || first.Faults[0] != want[0] || first.Faults[1] != want[1] {
		t.Fatalf("the compound rung must lead with D-031's proven schedule\n got %v\nwant %v",
			first.Faults, want)
	}
}

// Three structural properties, each of which the directive's own schedule
// violates, and each of which D-031 proved is load-bearing:
//
//  1. both faults name the SAME target (the directive paused the leader and
//     partitioned an arbitrary minority, so quorum was lost and no election
//     completed)
//  2. the outer window strictly CONTAINS the inner one and OUTLASTS it (a
//     displaced leader that learns it was displaced steps down before serving
//     anything)
//  3. a contained pause is shorter than the lease (a pause that outlives the
//     lease lets the deadline expire while the process is stopped)
func TestEveryCompoundActionHoldsTheD031Shape(t *testing.T) {
	opts := ladderOpts(t, ladderConfig(t, fixtureAllow(), nil))
	plan := mustGenerate(t, opts)
	compound := plan.ActionsFor(RungCompound)
	if len(compound) < 2 {
		t.Fatalf("expected several compound pairs, got %d", len(compound))
	}
	for _, a := range compound {
		if len(a.Faults) != 2 {
			t.Fatalf("a compound action overlaps exactly two faults, got %v", a.Faults)
		}
		outer, err := schema.ParseFault(a.Faults[0])
		if err != nil {
			t.Fatalf("parse %q: %v", a.Faults[0], err)
		}
		inner, err := schema.ParseFault(a.Faults[1])
		if err != nil {
			t.Fatalf("parse %q: %v", a.Faults[1], err)
		}
		if outer.Target.String() != inner.Target.String() {
			t.Fatalf("compound faults must name the same target, got %s and %s: pausing one node "+
				"while partitioning another is how the directive's schedule lost quorum (D-031)",
				outer.Target, inner.Target)
		}
		if outer.Target.Kind == schema.TargetQuorum {
			t.Fatalf("a compound action must never use a quorum target (%s): it binds to an "+
				"ARBITRARY minority at injection time, which is precisely how the directive's "+
				"schedule became incapable of firing", outer.Target)
		}
		if !(outer.StartMS < inner.StartMS && inner.EndMS < outer.EndMS) {
			t.Fatalf("the outer fault must strictly contain and outlast the inner one: %s vs %s",
				a.Faults[0], a.Faults[1])
		}
		if inner.Kind == schema.FaultProcPause && inner.DurationMS() >= DefaultLeaseMS {
			t.Fatalf("a contained pause of %dms is not shorter than the %dms lease: %s",
				inner.DurationMS(), DefaultLeaseMS, a.Faults[1])
		}
	}
}

func TestEveryPauseAnywhereIsShorterThanTheLease(t *testing.T) {
	opts := ladderOpts(t, ladderConfig(t, fixtureAllow(), nil))
	plan := mustGenerate(t, opts)
	seen := 0
	for _, a := range plan.Actions {
		for _, f := range a.Faults {
			spec, err := schema.ParseFault(f)
			if err != nil {
				t.Fatalf("parse %q: %v", f, err)
			}
			if spec.Kind != schema.FaultProcPause {
				continue
			}
			seen++
			if spec.DurationMS() >= DefaultLeaseMS {
				t.Fatalf("%s: a %dms pause against a %dms lease lets the deadline expire while the "+
					"process is stopped, so the resumed node correctly refuses the stale read "+
					"(D-019, OQ-010)", f, spec.DurationMS(), DefaultLeaseMS)
			}
		}
	}
	if seen == 0 {
		t.Fatal("no pause was generated at all, so this test proved nothing")
	}
}

func TestGenerateRefusesAPauseAtOrBeyondTheLease(t *testing.T) {
	opts := ladderOpts(t, ladderConfig(t, fixtureAllow(), nil))
	opts.Timing = DefaultTiming()
	opts.Timing.PauseMS = 6900 // the directive's own 6.9s window (OQ-010)
	if _, err := Generate(opts); !errors.Is(err, ErrLadder) {
		t.Fatalf("a pause longer than the lease must be refused rather than generated; err=%v", err)
	}
}

// ---------------------------------------------------------------------------
// Rung 5 and the clock's platform limits
// ---------------------------------------------------------------------------

// OQ-019: a time namespace moves CLOCK_MONOTONIC/BOOTTIME only.
// OQ-020: the offset is fixed at namespace creation, so this is a restart into
// a skewed namespace, not a live shift.
// OQ-023: util-linux parses whole seconds only.
func TestTemporalRungRespectsThePlatformLimits(t *testing.T) {
	cfg := ladderConfig(t, []schema.FaultKind{schema.FaultClockSkew, schema.FaultClockJump}, nil)
	plan := mustGenerate(t, ladderOpts(t, cfg))

	temporal := plan.ActionsFor(RungTemporal)
	if len(temporal) == 0 {
		t.Fatal("with both clock kinds allowed, rung 5 must produce actions")
	}
	for _, a := range temporal {
		spec, err := schema.ParseFault(a.Faults[0])
		if err != nil {
			t.Fatalf("parse %q: %v", a.Faults[0], err)
		}
		ms, ok := spec.Param("ms")
		if !ok {
			t.Fatalf("%s has no ms parameter", a.Faults[0])
		}
		if !strings.HasSuffix(ms, "000") {
			t.Fatalf("%s: an offset that is not whole seconds truncates toward zero and the fault "+
				"never fires (OQ-023)", a.Faults[0])
		}
		if !strings.Contains(a.Note, "monotonic") || !strings.Contains(a.Note, "RESTART") {
			t.Fatalf("a clock action must carry the monotonic-only and restart caveats; note was %q", a.Note)
		}
	}
}

// A.7's rung 0 names clock.skew(100ms). It truncates to zero whole seconds and
// is refused by the injector, so it is NOT generated, and it is NOT quietly
// rounded up to 1000 ms either, because a 10x magnitude change is a different
// experiment.
func TestReconRungDoesNotGenerateTheUnimplementableClockSkew(t *testing.T) {
	cfg := ladderConfig(t, []schema.FaultKind{schema.FaultClockSkew, schema.FaultNetLatency}, nil)
	plan := mustGenerate(t, ladderOpts(t, cfg))

	for _, a := range plan.ActionsFor(RungRecon) {
		for _, k := range a.Kinds {
			if k == schema.FaultClockSkew {
				t.Fatalf("rung 0 generated %v; a 100ms offset truncates to 0 whole seconds", a.Faults)
			}
		}
	}
	explained := false
	for _, s := range plan.SkipsFor(RungRecon) {
		if strings.Contains(s.What, "clock.skew(100ms)") && strings.Contains(s.Reason, "whole seconds") {
			explained = true
		}
	}
	if !explained {
		t.Fatalf("rung 0 must SAY why it drops A.7's clock action; skips were %+v", plan.SkipsFor(RungRecon))
	}
	if len(plan.ActionsFor(RungRecon)) == 0 {
		t.Fatal("rung 0 must still generate its net.latency actions")
	}
}

// ---------------------------------------------------------------------------
// determinism
// ---------------------------------------------------------------------------

func actionText(p LadderPlan) []string {
	out := make([]string, 0, len(p.Actions))
	for _, a := range p.Actions {
		out = append(out, a.String())
	}
	return out
}

func TestTheSameSeedGeneratesTheSameLadder(t *testing.T) {
	cfg := ladderConfig(t, fixtureAllow(), nil)
	a := actionText(mustGenerate(t, ladderOpts(t, cfg)))
	b := actionText(mustGenerate(t, ladderOpts(t, cfg)))
	if len(a) != len(b) {
		t.Fatalf("same seed produced %d then %d actions", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("action %d differs across two runs at the same seed:\n %s\n %s", i, a[i], b[i])
		}
	}
}

// Tie-breaks must actually come from the PRNG. If node ordering were fixed
// alphabetically, every seed of a ten-trial benchmark would try kv-n1 first and
// the "tie-break" would be a systematic bias.
func TestDifferentSeedsReorderEqualPriorTargets(t *testing.T) {
	cfg := ladderConfig(t, fixtureAllow(), nil)
	seen := map[string]bool{}
	for s := 0; s < 12; s++ {
		opts := ladderOpts(t, cfg)
		opts.Seed = recorder.Seed(s)
		plan := mustGenerate(t, opts)
		gray := plan.ActionsFor(RungGrayFail)
		if len(gray) == 0 {
			t.Fatal("no gray-failure actions")
		}
		order := make([]string, 0, len(gray))
		for _, a := range gray {
			order = append(order, a.Target)
		}
		if order[0] != roleLeader {
			t.Fatalf("role:leader is pinned first and is not a tie to be broken; got %v", order)
		}
		seen[strings.Join(order, ",")] = true
	}
	if len(seen) < 2 {
		t.Fatalf("twelve seeds produced one target order (%v): the tie-break is not drawing from "+
			"the PRNG at all", seen)
	}
}

// D-013's property, applied to this file: a stream's key is a function of its
// PATH ALONE, so draws taken inside one rung cannot shift another rung's
// ordering, and adding a rung later cannot perturb any existing stream.
func TestRungStreamsAreIndependent(t *testing.T) {
	const seed = recorder.Seed(42)

	want := make([]uint64, 4)
	st := LadderStream(seed, RungCompound)
	for i := range want {
		want[i] = st.Uint64()
	}

	// Burn an arbitrary number of values from another rung's stream first.
	other := LadderStream(seed, RungRecon)
	for i := 0; i < 97; i++ {
		_ = other.Uint64()
	}

	got := LadderStream(seed, RungCompound)
	for i := range want {
		if v := got.Uint64(); v != want[i] {
			t.Fatalf("draw %d from the compound stream changed after drawing from the recon "+
				"stream: %d != %d", i, v, want[i])
		}
	}
}

// The same rung under two different seeds must not share a keystream.
func TestRungStreamsAreSeedSeparated(t *testing.T) {
	a := LadderStream(recorder.Seed(1), RungCompound).Uint64()
	b := LadderStream(recorder.Seed(2), RungCompound).Uint64()
	if a == b {
		t.Fatal("two seeds produced the same first draw for the same rung")
	}
}

// ---------------------------------------------------------------------------
// budget, constraints and prefix
// ---------------------------------------------------------------------------

// Budget is a property of the whole schedule. A candidate is compiled together
// with the branch's committed faults, so a branch that has already spent the
// budget produces no actions, and no error.
func TestABranchThatHasSpentItsBudgetGeneratesNothing(t *testing.T) {
	cfg := ladderConfig(t, fixtureAllow(), nil)
	cfg.Perturber.Budget.MaxFaultsPerWorld = 2
	opts := ladderOpts(t, cfg)
	opts.Prefix = []string{
		"proc.pause(kv-n1)@100..200",
		"proc.pause(kv-n2)@100..200",
	}
	plan, err := Generate(opts)
	if err != nil {
		t.Fatalf("a spent budget is not an error: %v", err)
	}
	if len(plan.Actions) != 0 {
		t.Fatalf("max_faults_per_world is 2 and the prefix uses both; got %d actions", len(plan.Actions))
	}
	if len(plan.Skipped) == 0 {
		t.Fatal("the exhausted budget must be recorded, not silently swallowed")
	}
}

func TestMaxConcurrentFaultsIsHonoured(t *testing.T) {
	cfg := ladderConfig(t, fixtureAllow(), nil)
	cfg.Perturber.Budget.MaxConcurrentFaults = 1
	plan := mustGenerate(t, ladderOpts(t, cfg))
	if len(plan.ActionsFor(RungCompound)) != 0 {
		t.Fatal("a compound action holds two faults at once and cannot fit max_concurrent_faults=1")
	}
	if len(plan.ActionsFor(RungGrayFail)) == 0 {
		t.Fatal("single-fault rungs are unaffected and must still generate")
	}
}

// A safety constraint must be enforced on generated actions, not merely on
// authored ones. A search that could route around perturber.constraints would
// be able to manufacture a false violation by partitioning a majority.
func TestSafetyConstraintsAreEnforcedOnGeneratedActions(t *testing.T) {
	cfg := ladderConfig(t, fixtureAllow(), nil, "kv-n1 must be reachable during PERTURB")
	plan := mustGenerate(t, ladderOpts(t, cfg))

	for _, a := range plan.Actions {
		for _, f := range a.Faults {
			spec, err := schema.ParseFault(f)
			if err != nil {
				t.Fatalf("parse %q: %v", f, err)
			}
			if spec.Target.Kind == schema.TargetNode && spec.Target.Node == "kv-n1" &&
				(spec.Kind == schema.FaultProcPause || spec.Kind == schema.FaultNetPartition) {
				t.Fatalf("%q isolates kv-n1, which the constraint forbids during PERTURB", f)
			}
		}
	}
	// The other nodes are untouched by the constraint, so the ladder must not
	// have collapsed to nothing.
	if len(plan.ActionsFor(RungGrayFail)) == 0 {
		t.Fatal("only kv-n1 is constrained; the gray-failure rung must still generate for others")
	}
}

// ---------------------------------------------------------------------------
// input validation
// ---------------------------------------------------------------------------

func TestGenerateRefusesMissingInputs(t *testing.T) {
	top := ladderTopology(t)
	cfg := ladderConfig(t, fixtureAllow(), nil)

	if _, err := Generate(LadderOptions{Topology: top}); !errors.Is(err, ErrLadder) {
		t.Fatal("a nil config must be refused: allow, deny, budget and constraints all come from it")
	}
	if _, err := Generate(LadderOptions{Config: cfg}); !errors.Is(err, ErrLadder) {
		t.Fatal("a nil topology must be refused: a target that matches nothing injects nothing")
	}
	if _, err := Generate(LadderOptions{Config: cfg, Topology: top, AtMS: -1}); !errors.Is(err, ErrLadder) {
		t.Fatal("a window cannot open before the virtual clock exists")
	}
}

func TestRungNamesAreTheLaddersOwn(t *testing.T) {
	want := []string{"RECON", "STRESS", "GRAY_FAIL", "PARTITION", "COMPOUND", "TEMPORAL"}
	for i, r := range AllRungs {
		if r.String() != want[i] {
			t.Fatalf("rung %d is %q, want %q", i, r.String(), want[i])
		}
	}
}

// The partition rung reports the capability gap rather than claiming A.7's
// preference was honoured (D-028 item 4).
func TestPartitionRungSaysItIsSymmetric(t *testing.T) {
	plan := mustGenerate(t, ladderOpts(t, ladderConfig(t, fixtureAllow(), nil)))
	acts := plan.ActionsFor(RungPartition)
	if len(acts) == 0 {
		t.Fatal("no partition actions")
	}
	if !strings.Contains(acts[0].Note, "asymmetric") {
		t.Fatalf("A.7 prefers an asymmetric partition the frozen grammar cannot express; the "+
			"action must say so. Note was %q", acts[0].Note)
	}
}
