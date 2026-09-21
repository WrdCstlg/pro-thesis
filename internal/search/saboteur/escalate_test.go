package saboteur

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// ---------------------------------------------------------------------------
// Early termination: OQ-004 / D-022
// ---------------------------------------------------------------------------

// A.5: "If an oracle violation is detected during DRIVE or PERTURB, terminate
// the rollout immediately." OQ-004's resolution restricts that to PREFIX-CLOSED
// classes. crash and safety are prefix-closed; consistency and convergence are
// claims about the history AS A WHOLE and are ASSERT-only.
func TestOnlyCrashAndSafetyArePrefixClosed(t *testing.T) {
	want := map[schema.OracleClass]bool{
		schema.ClassCrash:  true,
		schema.ClassSafety: true,
	}
	for _, c := range schema.AllOracleClasses {
		if PrefixClosed(c) != want[c] {
			t.Fatalf("PrefixClosed(%s) = %v, want %v. Only crash and safety survive being "+
				"observed on a prefix of the history (OQ-004).", c, PrefixClosed(c), want[c])
		}
	}
}

// This is the bite OQ-004 names: consistency carries the HIGHEST reward in A.6
// and is the class the tool exists to find; the fixture's stale read is one. It
// is also precisely the class that cannot be soundly judged mid-partition. A
// rollout hunting it MUST run to ASSERT.
func TestAConsistencyFindingInDriveDoesNotTerminateTheSearch(t *testing.T) {
	got := MayTerminateEarly(TerminationInput{
		Oracle: "linearizable.kv", Class: schema.ClassConsistency,
		Definite: true, Violated: true, ObservedPhase: schema.PhaseDrive,
	})
	if got.Terminates() {
		t.Fatal("a consistency violation observed in DRIVE terminated the search. Invariant I5: " +
			"checking consistency during an active partition generates false positives, and a " +
			"linearizability claim is not prefix-closed. The rollout must run to ASSERT.")
	}
	if !strings.Contains(got.Reason, "prefix-closed") {
		t.Fatalf("the refusal does not say why: %q", got.Reason)
	}
}

func TestACrashFindingInDriveTerminatesTheSearch(t *testing.T) {
	got := MayTerminateEarly(TerminationInput{
		Oracle: "no_crash", Class: schema.ClassCrash,
		Definite: true, Violated: true, ObservedPhase: schema.PhaseDrive,
	})
	if !got.Terminates() {
		t.Fatalf("a crash observed in DRIVE did not terminate: %q. A crash is prefix-closed — "+
			"no continuation of the history un-crashes a process.", got.Reason)
	}
}

// A world that RAN TO COMPLETION and produced a consistency violation is a
// finished finding. Refusing to stop there would make the restriction above
// meaningless in the direction that matters: the search would never stop on the
// bug it exists to find.
func TestAConsistencyFindingInAssertTerminatesTheSearch(t *testing.T) {
	got := MayTerminateEarly(TerminationInput{
		Oracle: "linearizable.kv", Class: schema.ClassConsistency,
		Definite: true, Violated: true, ObservedPhase: schema.PhaseAssert,
	})
	if !got.Terminates() {
		t.Fatalf("a completed consistency violation did not terminate the search: %q", got.Reason)
	}
}

// "Could not check" is not "found a bug". An indefinite finding must never end
// a search, whatever class it carries.
func TestAnIndefiniteFindingNeverTerminates(t *testing.T) {
	for _, c := range schema.AllOracleClasses {
		got := MayTerminateEarly(TerminationInput{
			Oracle: "x", Class: c, Definite: false, Violated: true, ObservedPhase: schema.PhaseAssert,
		})
		if got.Terminates() {
			t.Fatalf("an oracle that could not answer terminated the search for class %s", c)
		}
	}
	// And an ok result is not a violation either.
	if MayTerminateEarly(TerminationInput{
		Oracle: "x", Class: schema.ClassCrash, Definite: true, Violated: false,
		ObservedPhase: schema.PhaseAssert,
	}).Terminates() {
		t.Fatal("a satisfied oracle terminated the search")
	}
}

// The decision must not depend on the order the engine happened to collect the
// findings in: a parallel executor completes worlds out of order.
func TestFirstTerminationIsOrderIndependent(t *testing.T) {
	ins := []TerminationInput{
		{Oracle: "zzz", Class: schema.ClassConsistency, Definite: true, Violated: true, ObservedPhase: schema.PhaseDrive},
		{Oracle: "aaa", Class: schema.ClassCrash, Definite: true, Violated: true, ObservedPhase: schema.PhaseDrive},
	}
	a := FirstTermination(ins)
	b := FirstTermination([]TerminationInput{ins[1], ins[0]})
	if a.Oracle != b.Oracle || a.Scope != b.Scope {
		t.Fatalf("FirstTermination depends on input order: %+v vs %+v", a, b)
	}
	if !a.Terminates() || a.Oracle != "aaa" {
		t.Fatalf("the prefix-closed crash finding did not win: %+v", a)
	}
}

func TestNoFindingsMeansNoTermination(t *testing.T) {
	got := FirstTermination(nil)
	if got.Terminates() {
		t.Fatal("an empty finding set terminated the search")
	}
	if got.Reason == "" {
		t.Fatal("the decision carries no reason")
	}
}

// ---------------------------------------------------------------------------
// A.5's budget split
// ---------------------------------------------------------------------------

func TestSplitBudgetIsTwentyPercentOfBoth(t *testing.T) {
	s := SplitBudget(30*time.Minute, 100, 20)
	if s.ProbeWall != 6*time.Minute {
		t.Fatalf("ProbeWall = %s, want 6m (20%% of 30m)", s.ProbeWall)
	}
	if s.ProbeWorlds != 20 {
		t.Fatalf("ProbeWorlds = %d, want 20", s.ProbeWorlds)
	}
	if !strings.Contains(s.Explain(), "whichever is reached first") {
		t.Fatalf("Explain does not state A.5's rule: %q", s.Explain())
	}
}

func TestSplitBudgetHandlesUnboundedWorlds(t *testing.T) {
	s := SplitBudget(10*time.Minute, -1, 20)
	if s.ProbeWorlds != -1 {
		t.Fatalf("ProbeWorlds = %d for an unbounded run, want -1", s.ProbeWorlds)
	}
	if s.ProbeWall != 2*time.Minute {
		t.Fatalf("ProbeWall = %s, want 2m", s.ProbeWall)
	}
}

func TestProbeStageExpiresOnWhicheverComesFirst(t *testing.T) {
	s := SplitBudget(time.Hour, 10, 20) // 2 probe worlds, 12m
	if done, _ := ProbeStageExpired(time.Now(), s, 1); done {
		t.Fatal("the probe stage expired after 1 of 2 worlds with 12 minutes left")
	}
	done, why := ProbeStageExpired(time.Now(), s, 2)
	if !done {
		t.Fatal("the probe stage did not expire when its world budget was spent")
	}
	if !strings.Contains(why, "world budget") {
		t.Fatalf("the reason does not say which budget expired: %q", why)
	}
	// And the wall arm, with the world budget untouched.
	s2 := SplitBudget(time.Nanosecond, 1000, 100)
	if done, why := ProbeStageExpired(time.Now().Add(-time.Second), s2, 0); !done ||
		!strings.Contains(why, "wall") {
		t.Fatalf("the wall arm did not fire: done=%v why=%q", done, why)
	}
}

// ---------------------------------------------------------------------------
// The escalation itself
// ---------------------------------------------------------------------------

func escalateOpts(t *testing.T, sigs ...ProbeSignal) EscalateOptions {
	t.Helper()
	return EscalateOptions{
		Tree:    treeOpts(t),
		Signals: sigs,
	}
}

// A.5: "If no REINFORCE signal is detected during probing, fall back to the base
// spec's stochastic corpus mutation for the remaining budget. The Saboteur is an
// accelerant, not a replacement for baseline coverage."
func TestNoReinforcingSignalIsNotAnError(t *testing.T) {
	_, err := NewEscalation(escalateOpts(t,
		ProbeSignal{Kind: schema.FaultProcPause, Target: roleLeader,
			Class: SpiralDampen, Evaluable: true}))
	if !errors.Is(err, ErrNoReinforcingSignal) {
		t.Fatalf("err = %v, want ErrNoReinforcingSignal so the caller can fall back to corpus "+
			"mutation rather than treating a quiet system as a failure", err)
	}
}

// Tier 2's root re-issues the reinforcing (kind, target) at the LADDER's
// magnitude, not the probe's. The probe deliberately ran at minimal magnitude
// and a 2000 ms window because that is what makes a probe cheap; carrying that
// into every branch below the root would defeat A.5's whole purpose.
func TestTheTreeRootUsesTheLaddersMagnitudeNotTheProbes(t *testing.T) {
	opts := treeOpts(t)
	opts.AtMS = 3000
	root, err := LadderRootFor(ProbeSignal{
		Kind: schema.FaultProcPause, Target: roleLeader,
		Class: SpiralReinforceAccelerating, Evaluable: true,
	}, opts)
	if err != nil {
		t.Fatalf("LadderRootFor: %v", err)
	}
	spec, err := schema.ParseFault(root[0])
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	dur := spec.EndMS - spec.StartMS
	if dur == DefaultProbeWindowMS {
		t.Fatalf("the tree root reuses the PROBE window of %dms. Tier 2 exists to convert a "+
			"detected spiral into a violation, and a minimal-magnitude root cannot.", dur)
	}
	if dur != DefaultTiming().PauseMS {
		t.Fatalf("root window is %dms, want the ladder's PauseMS of %dms",
			dur, DefaultTiming().PauseMS)
	}
	// And the pause must stay strictly under the lease, or the deadline expires
	// while the process is stopped and the resumed node correctly refuses the
	// stale read (D-019, OQ-010).
	if dur >= DefaultLeaseMS {
		t.Fatalf("the root pause of %dms is not shorter than the %dms lease", dur, DefaultLeaseMS)
	}
	// The TARGET is carried over verbatim: it is what the probe MEASURED.
	if spec.Target.String() != roleLeader {
		t.Fatalf("the root target is %q; the measured signal's target must be preserved",
			spec.Target.String())
	}
}

// The escalation visits its trees round-robin, so one tree cannot consume the
// whole budget while a differently-rooted branch goes untried.
func TestEscalationVisitsItsTreesRoundRobin(t *testing.T) {
	esc, err := NewEscalation(escalateOpts(t,
		ProbeSignal{Kind: schema.FaultProcPause, Target: roleLeader,
			Class: SpiralReinforceAccelerating, Evaluable: true},
		ProbeSignal{Kind: schema.FaultNetPartition, Target: "kv-n1",
			Class: SpiralReinforceLinear, Evaluable: true},
	))
	if err != nil {
		t.Fatalf("NewEscalation: %v", err)
	}
	if len(esc.Trees()) != 2 {
		t.Fatalf("built %d trees, want 2", len(esc.Trees()))
	}
	seen := map[*Tree]int{}
	for i := 0; i < 6; i++ {
		tr, n, err := esc.Next()
		if err != nil {
			t.Fatalf("Next %d: %v", i, err)
		}
		seen[tr]++
		tr.Backpropagate(n, RolloutResult{Utility: 1, Evaluable: true})
	}
	if len(seen) != 2 {
		t.Fatalf("only %d of 2 trees were visited in six rollouts", len(seen))
	}
	for tr, n := range seen {
		if n < 2 {
			t.Fatalf("tree %p got only %d of six rollouts", tr, n)
		}
	}
}

func TestEscalationRespectsTheTreeCap(t *testing.T) {
	sigs := []ProbeSignal{}
	for _, tgt := range []string{roleLeader, "kv-n1", "kv-n2", "kv-n3"} {
		sigs = append(sigs, ProbeSignal{
			Kind: schema.FaultProcPause, Target: tgt,
			Class: SpiralReinforceAccelerating, Evaluable: true,
		})
	}
	o := escalateOpts(t, sigs...)
	o.MaxTrees = 2
	esc, err := NewEscalation(o)
	if err != nil {
		t.Fatalf("NewEscalation: %v", err)
	}
	if len(esc.Trees()) != 2 {
		t.Fatalf("built %d trees against MaxTrees 2", len(esc.Trees()))
	}
}

// Every rollout the escalation hands out must be executable. A branch that
// cannot compile costs a real world to discover.
func TestEveryEscalationRolloutCompiles(t *testing.T) {
	o := escalateOpts(t, ProbeSignal{
		Kind: schema.FaultProcPause, Target: roleLeader,
		Class: SpiralReinforceAccelerating, Evaluable: true,
	})
	esc, err := NewEscalation(o)
	if err != nil {
		t.Fatalf("NewEscalation: %v", err)
	}
	for i := 0; i < 25; i++ {
		tr, n, err := esc.Next()
		if errors.Is(err, ErrExhaustedTree) {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if len(n.Faults) == 0 {
			t.Fatal("an escalation rollout has no faults")
		}
		if len(n.Faults) > o.Tree.MaxFaults {
			t.Fatalf("rollout exceeds the fault ceiling: %v", n.Faults)
		}
		tr.Backpropagate(n, RolloutResult{Utility: float64(i), Evaluable: true})
	}
}

// The canonical two-fault shape must be REACHABLE from a leader-pause root
// within a handful of rollouts, or A.9 #2's ten-minute budget cannot be met.
// This asserts the SHAPE (an outer partition containing and outlasting an inner
// pause on the same target) never D-031's literal milliseconds, which would be
// the hardcoded schedule A.9 #2 forbids.
func TestTheCanonicalTwoFaultShapeIsReachedEarly(t *testing.T) {
	o := escalateOpts(t, ProbeSignal{
		Kind: schema.FaultProcPause, Target: roleLeader,
		Class: SpiralReinforceAccelerating, Evaluable: true,
	})
	esc, err := NewEscalation(o)
	if err != nil {
		t.Fatalf("NewEscalation: %v", err)
	}
	const budget = 10
	for i := 0; i < budget; i++ {
		tr, n, err := esc.Next()
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if len(n.Faults) == 2 && isContainedPausePartition(t, n.Faults) {
			return
		}
		tr.Backpropagate(n, RolloutResult{Utility: float64(i), Evaluable: true})
	}
	t.Fatalf("the canonical pause-inside-partition shape was not reached in %d rollouts. "+
		"At ~35s per world that is over five minutes, and A.9 #2 allows ten for the whole "+
		"cold start.", budget)
}

// isContainedPausePartition reports D-031's shape: a partition and a pause on
// the SAME target, where the partition opens no later than the pause and closes
// strictly after it, and the pause is shorter than the lease.
func isContainedPausePartition(t *testing.T, faults []string) bool {
	t.Helper()
	var pause, part *schema.FaultSpec
	for i := range faults {
		spec, err := schema.ParseFault(faults[i])
		if err != nil {
			return false
		}
		switch spec.Kind {
		case schema.FaultProcPause:
			s := spec
			pause = &s
		case schema.FaultNetPartition:
			s := spec
			part = &s
		}
	}
	if pause == nil || part == nil {
		return false
	}
	if pause.Target.String() != part.Target.String() {
		return false
	}
	if pause.EndMS-pause.StartMS >= DefaultLeaseMS {
		return false
	}
	return part.StartMS <= pause.StartMS && part.EndMS > pause.EndMS
}
