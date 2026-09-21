package saboteur

import (
	"strings"
	"testing"

	"github.com/WrdCstlg/pro-thesis/internal/perturber"
	"github.com/WrdCstlg/pro-thesis/internal/recorder"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

func sweepOpts(t *testing.T, allow []schema.FaultKind, constraints ...string) SweepOptions {
	t.Helper()
	return SweepOptions{
		Config:        ladderConfig(t, allow, nil, constraints...),
		Topology:      ladderTopology(t),
		Seed:          0xC0FFEE,
		KindSupported: allSupported,
	}
}

func mustSweep(t *testing.T, opts SweepOptions) SweepPlan {
	t.Helper()
	p, err := Sweep(opts)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	return p
}

// ---------------------------------------------------------------------------
// The control world A.3 drops
// ---------------------------------------------------------------------------

// D-028 item 6. A.3 lists only fault probes, and every one of its four
// DAMPEN/REINFORCE criteria is COMPARATIVE: "returned to baseline", "exceeded
// 2x baseline", "got worse after the fault ended". Without a control the
// classifier has no baseline and every comparative criterion is unevaluable.
func TestTheSweepIncludesTheNoFaultControlFirst(t *testing.T) {
	plan := mustSweep(t, sweepOpts(t, fixtureAllow()))
	if len(plan.Probes) == 0 {
		t.Fatal("the sweep is empty")
	}
	c := plan.Probes[0]
	if !c.Control {
		t.Fatal("the first probe is not the control. A.3 drops the no-fault control world " +
			"entirely (D-028 item 6); it is restored here, and it is FIRST so a run that " +
			"exhausts its budget early has still measured it.")
	}
	if len(c.Faults) != 0 {
		t.Fatalf("the control world carries %d fault(s): %v", len(c.Faults), c.Faults)
	}
	controls := 0
	for _, p := range plan.Probes {
		if p.Control {
			controls++
		}
	}
	if controls != 1 {
		t.Fatalf("%d control worlds; exactly one baseline is wanted", controls)
	}
}

// A truncated probe budget must not be able to drop the control: SplitBudget
// floors the probe world count at 1 whenever any probing was asked for.
func TestATinyProbeBudgetStillBuysTheControl(t *testing.T) {
	s := SplitBudget(0, 4, 20) // 20% of 4 worlds rounds to 0
	if s.ProbeWorlds < 1 {
		t.Fatalf("ProbeWorlds = %d; a probe budget that rounds to zero drops the control world "+
			"and with it every comparative criterion in A.3", s.ProbeWorlds)
	}
	// But an explicit zero percent means no probing at all, which is a
	// different statement and must be honoured.
	if got := SplitBudget(0, 100, 0).ProbeWorlds; got != 0 {
		t.Fatalf("probe_budget_pct 0 gave %d probe worlds, want 0", got)
	}
}

// ---------------------------------------------------------------------------
// Every probe must be executable
// ---------------------------------------------------------------------------

func TestEveryProbeParsesAndCompiles(t *testing.T) {
	opts := sweepOpts(t, fixtureAllow(), "never partition more than minority of kv")
	plan := mustSweep(t, opts)
	for _, p := range plan.FaultProbes() {
		if len(p.Faults) != 1 {
			t.Fatalf("probe %s has %d faults; A.3 says a micro-probe is a SINGLE fault",
				p.Label, len(p.Faults))
		}
		canon, err := schema.CanonicalFault(p.Faults[0])
		if err != nil {
			t.Fatalf("probe %q does not parse: %v", p.Faults[0], err)
		}
		if canon != p.Faults[0] {
			t.Fatalf("probe %q is not in canonical form (want %q)", p.Faults[0], canon)
		}
		if _, err := perturber.Compile(perturber.CompileOptions{
			Config: opts.Config, Topology: opts.Topology, Planned: p.Faults,
		}); err != nil {
			t.Fatalf("probe %q does not compile: %v", p.Faults[0], err)
		}
	}
}

// A kind this platform cannot deliver must be SKIPPED WITH A REASON, not
// generated. Generating it spends a whole ~30 s world, a compose project and two
// of the host's ~24 bridge networks to reach ErrUnsupported mid-DRIVE.
func TestAnUndeliverableKindIsSkippedWithItsReason(t *testing.T) {
	opts := sweepOpts(t, fixtureAllow())
	opts.KindSupported = func(k schema.FaultKind) error {
		if k == schema.FaultIOLatency {
			return errNotHere
		}
		return nil
	}
	plan := mustSweep(t, opts)
	for _, p := range plan.FaultProbes() {
		if p.Kind == schema.FaultIOLatency {
			t.Fatalf("a probe was generated for a kind this platform cannot deliver: %v", p.Faults)
		}
	}
	found := false
	for _, s := range plan.Skipped {
		if s.What == string(schema.FaultIOLatency) {
			found = true
			if s.Reason == "" {
				t.Fatal("the skip carries no reason; a silently short sweep looks exactly like " +
					"a sweep that was run and found nothing")
			}
		}
	}
	if !found {
		t.Fatal("the undeliverable kind was dropped with no Skip recorded")
	}
}

var errNotHere = errNotHereType{}

type errNotHereType struct{}

func (errNotHereType) Error() string { return "not deliverable on this platform" }

// A.9 #1 asks the sweep to classify `proc.pause` ON THE LEADER. `role:leader` is
// the only target form in the frozen grammar that names the leader (it binds to
// a concrete node at INJECTION time (D-012)) so probing node ids alone would hit
// the leader only by luck and would attribute the signal to whichever id held
// the role in that world.
func TestTheSweepProbesTheLeaderRoleAndEveryNode(t *testing.T) {
	plan := mustSweep(t, sweepOpts(t, []schema.FaultKind{schema.FaultProcPause}))
	targets := map[string]bool{}
	for _, p := range plan.FaultProbes() {
		targets[p.Target] = true
	}
	for _, want := range []string{roleLeader, "kv-n1", "kv-n2", "kv-n3"} {
		if !targets[want] {
			t.Fatalf("the sweep never probes %q; targets were %v", want, targets)
		}
	}
	// The opt-out exists and works, for a caller whose system has no roles.
	opts := sweepOpts(t, []schema.FaultKind{schema.FaultProcPause})
	opts.SkipRoleTargets = true
	for _, p := range mustSweep(t, opts).FaultProbes() {
		if strings.HasPrefix(p.Target, "role:") {
			t.Fatalf("SkipRoleTargets did not suppress %q", p.Target)
		}
	}
}

// A 500 ms gray-failure probe is SHORTER THAN A RAFT ELECTION on the reference
// fixture (D-031 measured the election completing ~1.2 s into a pause) so it
// could not produce the role change, the queue growth or the post-withdrawal
// error rate the sweep exists to detect. A.3's upper bound is used, and the
// reason is a test rather than a comment.
func TestTheProbeWindowOutlastsAnElection(t *testing.T) {
	if DefaultProbeWindowMS < 1200 {
		t.Fatalf("DefaultProbeWindowMS = %d, shorter than the ~1200ms D-031 measured an election "+
			"taking on this fixture. A probe that ends before the cluster reacts classifies "+
			"DAMPEN on a system that reinforces.", DefaultProbeWindowMS)
	}
	if DefaultProbeWindowMS > 2000 {
		t.Fatalf("DefaultProbeWindowMS = %d exceeds A.3's stated 500-2000ms range", DefaultProbeWindowMS)
	}
}

// ---------------------------------------------------------------------------
// Ranking
// ---------------------------------------------------------------------------

func probeOutcome(idx int, kind schema.FaultKind, target string, class SpiralClass, rank float64, evaluable bool) ProbeOutcome {
	return ProbeOutcome{
		Probe:     Probe{Index: idx, Kind: kind, Target: target},
		Nodes:     []Result{{Node: "kv-n1", Class: class, Rank: rank}},
		Evaluable: evaluable,
		Why:       "test",
	}
}

// An UNEVALUABLE probe told the search nothing. Ranking it as though it had is
// the ranked #1 failure mode of this phase.
func TestAnUnevaluableProbeRanksBelowEveryEvaluableOne(t *testing.T) {
	outs := []ProbeOutcome{
		probeOutcome(1, schema.FaultProcPause, "kv-n1", SpiralReinforceAccelerating, 999, false),
		probeOutcome(2, schema.FaultNetLoss, "kv-n2", SpiralDampen, 1, true),
	}
	got := Rank(outs)
	if len(got) != 2 {
		t.Fatalf("Rank returned %d signals", len(got))
	}
	if got[0].Evaluable != true {
		t.Fatal("an unevaluable probe outranked an evaluable one. A world whose oracles could " +
			"not answer is not evidence about the system, however dramatic its telemetry looked.")
	}
	if got[1].Class != SpiralInsufficientData {
		t.Fatalf("the unevaluable probe kept the class %s; it must report INSUFFICIENT_DATA, "+
			"which is not DAMPEN and not REINFORCE", got[1].Class)
	}
	if got[1].Rank != 0 {
		t.Fatalf("the unevaluable probe kept rank %v", got[1].Rank)
	}

	// And the ordering must hold when the CLASS RESET alone cannot decide it.
	// Both of these end up INSUFFICIENT_DATA at rank 0, so only the evaluability
	// key separates them, and the unevaluable one carries the higher utility,
	// which is what it would win on without that key.
	tie := Rank([]ProbeOutcome{
		{
			Probe:     Probe{Index: 1, Kind: schema.FaultProcPause, Target: "unevaluable"},
			Nodes:     []Result{{Node: "kv-n1", Class: SpiralReinforceAccelerating, Rank: 900}},
			Evaluable: false, Utility: 500,
		},
		{
			Probe:     Probe{Index: 2, Kind: schema.FaultNetLoss, Target: "evaluable"},
			Nodes:     []Result{{Node: "kv-n1", Class: SpiralInsufficientData, Rank: 0}},
			Evaluable: true, Utility: 1,
		},
	})
	if tie[0].Target != "evaluable" {
		t.Fatalf("an UNEVALUABLE probe outranked an evaluable one on utility once their classes "+
			"tied. A world whose oracles could not answer must sort below every world that was "+
			"actually judged, whatever numbers it happens to carry. got %q first", tie[0].Target)
	}
}

func TestRankOrdersByClassThenSignalStrength(t *testing.T) {
	outs := []ProbeOutcome{
		probeOutcome(1, schema.FaultNetLoss, "a", SpiralDampen, 500, true),
		probeOutcome(2, schema.FaultProcPause, "b", SpiralReinforceLinear, 10, true),
		probeOutcome(3, schema.FaultProcPause, "c", SpiralReinforceAccelerating, 5, true),
		probeOutcome(4, schema.FaultProcPause, "d", SpiralReinforceAccelerating, 50, true),
	}
	got := Rank(outs)
	want := []string{"d", "c", "b", "a"}
	for i, w := range want {
		if got[i].Target != w {
			t.Fatalf("rank %d is %q, want %q (class dominates, then signal strength)",
				i, got[i].Target, w)
		}
	}
}

// The control is not a signal and must never appear in A.3's ranked output.
func TestTheControlIsNotRanked(t *testing.T) {
	outs := []ProbeOutcome{
		{Probe: Probe{Index: 0, Control: true}, Evaluable: true},
		probeOutcome(1, schema.FaultProcPause, "x", SpiralDampen, 1, true),
	}
	got := Rank(outs)
	if len(got) != 1 {
		t.Fatalf("Rank returned %d signals; the control world is a baseline, not a probe result", len(got))
	}
}

// A.5: Tier 2 is "activated only when OBSERVE classifies REINFORCE". A DAMPEN or
// an INSUFFICIENT_DATA must never become a tree root.
func TestOnlyReinforcingEvaluableSignalsAreEscalated(t *testing.T) {
	sigs := []ProbeSignal{
		{Target: "a", Class: SpiralReinforceAccelerating, Evaluable: true},
		{Target: "b", Class: SpiralDampen, Evaluable: true},
		{Target: "c", Class: SpiralReinforceLinear, Evaluable: false},
		{Target: "d", Class: SpiralInsufficientData, Evaluable: true},
		{Target: "e", Class: SpiralReinforceLinear, Evaluable: true},
	}
	got := TopReinforcing(sigs, 0)
	if len(got) != 2 {
		t.Fatalf("TopReinforcing returned %d signals, want 2 (a and e)", len(got))
	}
	for _, s := range got {
		if !s.Class.Reinforcing() || !s.Evaluable {
			t.Fatalf("%q was escalated with class %s evaluable=%v", s.Target, s.Class, s.Evaluable)
		}
	}
	if n := len(TopReinforcing(sigs, 1)); n != 1 {
		t.Fatalf("the cap was not applied: got %d", n)
	}
}

// ---------------------------------------------------------------------------
// Baselines
// ---------------------------------------------------------------------------

// A series with no observed points must report Known=false, never 0. A zero
// would make "we never measured this" indistinguishable from "it was empty" and
// would turn every comparison against it into a claim nobody made.
func TestAnUnobservedBaselineIsUnknownNotZero(t *testing.T) {
	b := BaselineFrom(Series{Metric: MetricQueueDepth, Absent: []Absence{{TMS: 1, Reason: "paused"}}})
	if b.Known {
		t.Fatal("a series with no observed points produced a KNOWN baseline")
	}
	if b.Value != 0 {
		t.Fatalf("an unknown baseline carries the value %v; it must be inert", b.Value)
	}
}

// The median, not the mean: one scheduling hiccup in a control world would drag
// a mean upward and make every comparative criterion harder to satisfy, biasing
// the classifier toward DAMPEN; the direction that loses signal.
func TestTheBaselineIsAMedianNotAMean(t *testing.T) {
	// The outlier sits in the MIDDLE of the slice on purpose. With it at the
	// end, taking the middle element of an unsorted slice happens to give the
	// right answer, and the test would pass with the sort removed.
	s := Series{Metric: MetricQueueDepth, Points: []Point{
		{TMS: 0, Value: 1}, {TMS: 1, Value: 1}, {TMS: 2, Value: 1000},
		{TMS: 3, Value: 1}, {TMS: 4, Value: 1},
	}}
	b := BaselineFrom(s)
	if !b.Known {
		t.Fatal("the baseline is not known for a series with five observed points")
	}
	if b.Value != 1 {
		t.Fatalf("baseline = %v, want the median 1. The mean is 200.8 and the middle UNSORTED "+
			"element is 1000; either would let a control world's single hiccup raise every "+
			"comparative threshold and bias the classifier toward DAMPEN.", b.Value)
	}
}

func TestBaselinesAreOrderedByMetric(t *testing.T) {
	got := BaselinesFrom([]Series{
		{Metric: "z", Points: []Point{{Value: 1}}},
		{Metric: "a", Points: []Point{{Value: 2}}},
	})
	if len(got) != 2 || got[0].Metric != "a" || got[1].Metric != "z" {
		t.Fatalf("baselines are not metric-ordered: %+v", got)
	}
}

// ---------------------------------------------------------------------------
// Determinism (A.8)
// ---------------------------------------------------------------------------

func TestTheSameSeedPlansTheSameSweep(t *testing.T) {
	a := mustSweep(t, sweepOpts(t, fixtureAllow()))
	b := mustSweep(t, sweepOpts(t, fixtureAllow()))
	if len(a.Probes) != len(b.Probes) {
		t.Fatalf("two sweeps at the same seed planned %d and %d probes", len(a.Probes), len(b.Probes))
	}
	for i := range a.Probes {
		if strings.Join(a.Probes[i].Faults, "|") != strings.Join(b.Probes[i].Faults, "|") {
			t.Fatalf("probe %d differs: %v vs %v", i, a.Probes[i].Faults, b.Probes[i].Faults)
		}
	}
}

// role:leader is pinned first because it is not a tie; D-031 proved that
// targeting anything else makes the canonical bug unreachable. The NODE ids that
// follow are genuinely equal-prior, so a fixed alphabetical order would make
// every seed in a 10-trial benchmark probe kv-n1 first.
func TestDifferentSeedsReorderEqualPriorProbeTargets(t *testing.T) {
	order := func(seed uint64) []string {
		o := sweepOpts(t, []schema.FaultKind{schema.FaultProcPause})
		o.Seed = recorder.Seed(seed)
		var out []string
		for _, p := range mustSweep(t, o).FaultProbes() {
			out = append(out, p.Target)
		}
		return out
	}
	base := order(1)
	if base[0] != roleLeader {
		t.Fatalf("the first probe target is %q, not role:leader", base[0])
	}
	differs := false
	for s := uint64(2); s < 40 && !differs; s++ {
		other := order(s)
		for i := range base {
			if base[i] != other[i] {
				differs = true
				break
			}
		}
	}
	if !differs {
		t.Fatal("no seed in 40 reordered the equal-prior node targets; a 10-trial benchmark " +
			"would systematically probe the same node first and the tie-break would be a bias")
	}
}
