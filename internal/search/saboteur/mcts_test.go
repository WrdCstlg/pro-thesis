package saboteur

import (
	"errors"
	"strings"
	"testing"

	"github.com/WrdCstlg/pro-thesis/internal/perturber"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

func treeOpts(t *testing.T, root ...string) TreeOptions {
	t.Helper()
	return TreeOptions{
		Config:        ladderConfig(t, fixtureAllow(), nil, "never partition more than minority of kv"),
		Topology:      ladderTopology(t),
		Seed:          0xC0FFEE,
		Root:          root,
		AtMS:          3000,
		MaxDepth:      DefaultMaxDepth,
		MaxFaults:     5,
		KindSupported: allSupported,
	}
}

func mustTree(t *testing.T, opts TreeOptions) *Tree {
	t.Helper()
	tr, err := NewTree(opts)
	if err != nil {
		t.Fatalf("NewTree: %v", err)
	}
	return tr
}

// ---------------------------------------------------------------------------
// Every node must be executable
// ---------------------------------------------------------------------------

// A tree that expands into schedules the executor would refuse spends a real
// ~30-second world, two bridge networks and a compose project to discover what
// perturber.Compile knows for free.
func TestEveryTreeNodeCompiles(t *testing.T) {
	tr := mustTree(t, treeOpts(t, "proc.pause(role:leader)@3000..5700"))
	cfg := tr.opts.Config
	top := tr.opts.Topology

	seen := 0
	for i := 0; i < 40; i++ {
		n, err := tr.Select()
		if errors.Is(err, ErrExhaustedTree) {
			break
		}
		if err != nil {
			t.Fatalf("Select: %v", err)
		}
		if len(n.Faults) == 0 {
			t.Fatal("a selected node has no faults; a rollout would perturb nothing while " +
				"reporting as a rollout")
		}
		if _, err := perturber.Compile(perturber.CompileOptions{
			Config: cfg, Topology: top, Planned: n.Faults,
		}); err != nil {
			t.Fatalf("node %v does not compile: %v", n.Faults, err)
		}
		seen++
		tr.Backpropagate(n, RolloutResult{Utility: 1, Evaluable: true})
	}
	if seen < 5 {
		t.Fatalf("the tree produced only %d executable nodes; it cannot search", seen)
	}
}

// The depth cap is A.5's "4 additional faults beyond the triggering probe
// fault", and the fault ceiling is the space's. Neither may be exceeded: a
// deeper branch costs a world AND makes A.9 #3's "<= 4 faults" unreachable.
func TestTheTreeRespectsItsDepthAndFaultCeilings(t *testing.T) {
	opts := treeOpts(t, "proc.pause(role:leader)@3000..5700")
	opts.MaxDepth = 2
	opts.MaxFaults = 3
	tr := mustTree(t, opts)

	for i := 0; i < 200; i++ {
		n, err := tr.Select()
		if errors.Is(err, ErrExhaustedTree) {
			break
		}
		if err != nil {
			t.Fatalf("Select: %v", err)
		}
		if n.Depth > opts.MaxDepth {
			t.Fatalf("node at depth %d exceeds MaxDepth %d", n.Depth, opts.MaxDepth)
		}
		if len(n.Faults) > opts.MaxFaults {
			t.Fatalf("node with %d faults exceeds MaxFaults %d: %v",
				len(n.Faults), opts.MaxFaults, n.Faults)
		}
		tr.Backpropagate(n, RolloutResult{Utility: 1, Evaluable: true})
	}
}

// ---------------------------------------------------------------------------
// The expansion order IS the search (D-015)
// ---------------------------------------------------------------------------

// A.7 Rung 4 states the bias in normative text: "Bias toward overlapping
// proc.pause with net.partition; this is the canonical Raft lease bug trigger."
// Without it, strict rung order spends the whole budget on RECON and STRESS
// before reaching a single PARTITION, and at ~62 rollouts the compound
// interaction is never reached at all.
func TestAnOverlappingActionOnTheSameTargetIsTriedFirst(t *testing.T) {
	prefix := []string{"proc.pause(role:leader)@3000..5700"}
	plan := mustGenerate(t, LadderOptions{
		Config:        ladderConfig(t, fixtureAllow(), nil, "never partition more than minority of kv"),
		Topology:      ladderTopology(t),
		Seed:          0xC0FFEE,
		AtMS:          2900,
		Prefix:        prefix,
		KindSupported: allSupported,
	})
	ordered := OrderActions(plan.Actions, prefix, roleLeader)
	if len(ordered) == 0 {
		t.Fatal("the ladder generated nothing to order")
	}

	// Find where the first partition on the SAME target lands, in ladder order
	// and in expansion order.
	idxIn := func(as []Action) int {
		for i, a := range as {
			if a.Target != roleLeader {
				continue
			}
			for _, k := range a.Kinds {
				if k == schema.FaultNetPartition {
					return i
				}
			}
		}
		return -1
	}
	ladderPos := idxIn(plan.Actions)
	orderedPos := idxIn(ordered)
	if ladderPos < 0 || orderedPos < 0 {
		t.Skip("this config generates no partition on role:leader; the bias cannot be observed")
	}
	if orderedPos >= ladderPos {
		t.Fatalf("the overlapping partition on the reinforced target is at position %d after "+
			"ordering, no earlier than its raw ladder position %d. Strict rung order reaches "+
			"PARTITION only after every RECON and STRESS child, which at a ~62-rollout budget "+
			"means never.", orderedPos, ladderPos)
	}
}

// The prior must come from the MEASURED signal, not from a constant. Changing
// the reinforced target must change the order.
func TestTheReinforcedTargetOrdersTheExpansion(t *testing.T) {
	plan := mustGenerate(t, ladderOpts(t,
		ladderConfig(t, fixtureAllow(), nil, "never partition more than minority of kv")))

	first := func(target string) string {
		ord := OrderActions(plan.Actions, nil, target)
		if len(ord) == 0 {
			t.Fatal("nothing to order")
		}
		return ord[0].Target
	}
	if got := first("kv-n3"); got != "kv-n3" {
		t.Fatalf("with kv-n3 reinforced the first action targets %q; the probe's measured signal "+
			"is not steering the expansion", got)
	}
	if got := first(roleLeader); got != roleLeader {
		t.Fatalf("with role:leader reinforced the first action targets %q", got)
	}
}

// Ordering must be a permutation: a reorder that dropped or duplicated an action
// would silently shrink the space.
func TestOrderingIsAPermutation(t *testing.T) {
	plan := mustGenerate(t, ladderOpts(t, ladderConfig(t, fixtureAllow(), nil)))
	ordered := OrderActions(plan.Actions, []string{"proc.pause(kv-n1)@3000..5700"}, "kv-n1")
	if len(ordered) != len(plan.Actions) {
		t.Fatalf("ordering changed the action count: %d -> %d", len(plan.Actions), len(ordered))
	}
	count := map[string]int{}
	for _, a := range plan.Actions {
		count[strings.Join(a.Faults, "|")]++
	}
	for _, a := range ordered {
		count[strings.Join(a.Faults, "|")]--
	}
	for k, v := range count {
		if v != 0 {
			t.Fatalf("action %q appears %d more/fewer times after ordering", k, -v)
		}
	}
}

// ---------------------------------------------------------------------------
// The geometry that makes the fixture's anomaly reachable
// ---------------------------------------------------------------------------

// D-031 established that the outer fault must OPEN BEFORE and OUTLAST the inner
// one, or the displaced leader learns it was displaced and steps down before
// serving anything. The tree expresses that as arithmetic over the branch rather
// than as a constant, and this test checks the ARITHMETIC: at an offset that is
// deliberately NOT D-031's 8200, because a search with the answer baked in would
// satisfy A.9 #2 by construction.
func TestAnAddedFaultOpensBeforeAndOutlastsWhatIsAlreadyCommitted(t *testing.T) {
	const at = 3000
	opts := treeOpts(t)
	opts.AtMS = at
	tr := mustTree(t, opts)

	pause := "proc.pause(role:leader)@3000..5700"
	root := &Node{Depth: 1, Faults: []string{pause}}
	if got := tr.expandAt(root); got != at-DefaultTiming().CompoundLeadInMS {
		t.Fatalf("expandAt = %d, want %d (CompoundLeadInMS before the earliest committed fault)",
			got, at-DefaultTiming().CompoundLeadInMS)
	}

	plan := mustGenerate(t, LadderOptions{
		Config:        opts.Config,
		Topology:      opts.Topology,
		Seed:          opts.Seed,
		AtMS:          tr.expandAt(root),
		Prefix:        root.Faults,
		KindSupported: allSupported,
	})
	inner, err := schema.ParseFault(pause)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	found := false
	for _, a := range plan.Actions {
		if a.Target != roleLeader || len(a.Faults) != 1 {
			continue
		}
		outer, err := schema.ParseFault(a.Faults[0])
		if err != nil || outer.Kind != schema.FaultNetPartition {
			continue
		}
		found = true
		if outer.StartMS > inner.StartMS {
			t.Fatalf("the added partition %s opens AFTER the committed pause %s; the leader is "+
				"paused before it is isolated and the mechanism does not fire (D-031)",
				a.Faults[0], pause)
		}
		if outer.EndMS <= inner.EndMS {
			t.Fatalf("the added partition %s does not OUTLAST the committed pause %s. D-031 "+
				"measured that a displaced leader which can learn it was displaced steps down "+
				"before serving anything, so the containing fault must survive the contained one.",
				a.Faults[0], pause)
		}
	}
	if !found {
		t.Fatal("no partition on role:leader was generated around the committed pause; the " +
			"canonical two-fault shape is unreachable from this root")
	}
}

// ---------------------------------------------------------------------------
// Parallel rollouts
// ---------------------------------------------------------------------------

// The executor runs four worlds at once, so Select is called four times before
// any result arrives. Without the pending marker it would hand back the same
// unvisited node four times: the batch would execute ONE schedule four times
// while reporting four rollouts, and the tree would record four visits of
// evidence it never gathered.
func TestSelectDoesNotHandOutTheSameNodeTwiceBeforeAResultArrives(t *testing.T) {
	tr := mustTree(t, treeOpts(t, "proc.pause(role:leader)@3000..5700"))
	seen := map[string]bool{}
	var batch []*Node
	for i := 0; i < 4; i++ {
		n, err := tr.Select()
		if err != nil {
			t.Fatalf("Select %d: %v", i, err)
		}
		key := strings.Join(n.Faults, "|")
		if seen[key] {
			t.Fatalf("Select handed out the schedule %q twice with no result in between; a "+
				"4-wide batch would execute one world four times and record four rollouts", key)
		}
		seen[key] = true
		batch = append(batch, n)
	}
	// Backpropagating releases them.
	for _, n := range batch {
		tr.Backpropagate(n, RolloutResult{Utility: 1, Evaluable: true})
		if n.Pending {
			t.Fatal("a node stayed pending after its result was backpropagated")
		}
	}
}

// ---------------------------------------------------------------------------
// Backpropagation and pruning
// ---------------------------------------------------------------------------

func TestBackpropagationReachesTheRoot(t *testing.T) {
	tr := mustTree(t, treeOpts(t, "proc.pause(role:leader)@3000..5700"))
	n, err := tr.Select()
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	tr.Backpropagate(n, RolloutResult{Utility: 150, NewTemplates: 2, Violations: 1, Evaluable: true})

	if tr.Root().Visits != 1 {
		t.Fatalf("root visits = %d, want 1", tr.Root().Visits)
	}
	if tr.Root().TotalUtility != 150 {
		t.Fatalf("root utility = %v, want 150", tr.Root().TotalUtility)
	}
	if tr.Root().ViolationTotal != 1 {
		t.Fatalf("the violation did not reach the root")
	}
	if got := tr.Root().AvgUtility(); got != 150 {
		t.Fatalf("root U_avg = %v, want 150", got)
	}
}

// A.5's pruning rule, applied. It is UNREACHABLE at Addendum A's own budgets and
// that is documented rather than hidden, so the test drives it directly.
func TestAProvenUninterestingNodeIsPruned(t *testing.T) {
	tr := mustTree(t, treeOpts(t, "proc.pause(role:leader)@3000..5700"))
	n, err := tr.Select()
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	for i := 0; i < PruneVisits; i++ {
		tr.Backpropagate(n, RolloutResult{Utility: -1, Evaluable: true})
	}
	if !n.Pruned {
		t.Fatalf("a node visited %d times with zero coverage delta and zero violations was not "+
			"pruned (visits=%d novel=%d violations=%d)",
			PruneVisits, n.Visits, n.NovelTotal, n.ViolationTotal)
	}
	if n.PruneWhy == "" {
		t.Fatal("the node was pruned with no recorded reason")
	}
}

// An UNEVALUABLE world established nothing. Letting it prune a branch would let
// harness trouble quietly delete the part of the space the search was about to
// reach: the ranked #1 failure mode wearing a different hat.
func TestAnUnevaluableRolloutNeverPrunes(t *testing.T) {
	tr := mustTree(t, treeOpts(t, "proc.pause(role:leader)@3000..5700"))
	n, err := tr.Select()
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	for i := 0; i < PruneVisits+3; i++ {
		tr.Backpropagate(n, RolloutResult{Utility: 0, Evaluable: false})
	}
	if n.Pruned {
		t.Fatal("a node was pruned on the strength of rollouts that could not be judged. " +
			"\"We could not tell\" is not \"proven uninteresting\".")
	}
}

// Pruning the root would end the search while claiming the branch was explored.
func TestTheRootIsNeverPruned(t *testing.T) {
	tr := mustTree(t, treeOpts(t))
	for i := 0; i < 10; i++ {
		tr.Backpropagate(tr.Root(), RolloutResult{Utility: 0, Evaluable: true})
	}
	if tr.Root().Pruned {
		t.Fatal("the root was pruned; the search would end while reporting the branch explored")
	}
}

// ---------------------------------------------------------------------------
// The D-015 measurement
// ---------------------------------------------------------------------------

// This is the honest instrument, not a feature. At Addendum A's budgets the
// exploration term is infinite at zero visits, so every selection is FORCED and
// backpropagation never decides anything. The tree must SAY so rather than imply
// otherwise, and a run that reports zero informed selections is a finding.
func TestAtLowRolloutCountsEverySelectionIsForced(t *testing.T) {
	tr := mustTree(t, treeOpts(t, "proc.pause(role:leader)@3000..5700"))
	for i := 0; i < 16; i++ { // OQ-013's measured serial rollout budget
		n, err := tr.Select()
		if errors.Is(err, ErrExhaustedTree) {
			break
		}
		if err != nil {
			t.Fatalf("Select: %v", err)
		}
		tr.Backpropagate(n, RolloutResult{Utility: float64(i), Evaluable: true})
	}
	st := tr.Stats()
	if st.ForcedSelections == 0 {
		t.Fatal("no selection was recorded at all; the measurement is not wired up")
	}
	if st.InformedSelections != 0 {
		t.Fatalf("%d selection(s) were INFORMED at a 16-rollout budget against a branching "+
			"factor in the dozens. D-015 and OQ-013 say this is impossible: an unvisited child "+
			"always scores +Inf. If this is genuinely true the decision record needs updating, "+
			"not this test.", st.InformedSelections)
	}
	if st.TreeContributed() {
		t.Fatal("TreeStats claims backpropagation influenced a decision when no selection was informed")
	}
	if st.MaxVisits > 1 && st.RevisitedNodes == 0 {
		t.Fatal("TreeStats is internally inconsistent about revisits")
	}
}

// The same seed must expand the same tree (A.8).
func TestTheSameSeedExpandsTheSameTree(t *testing.T) {
	walk := func() []string {
		tr := mustTree(t, treeOpts(t, "proc.pause(role:leader)@3000..5700"))
		var out []string
		for i := 0; i < 12; i++ {
			n, err := tr.Select()
			if errors.Is(err, ErrExhaustedTree) {
				break
			}
			if err != nil {
				t.Fatalf("Select: %v", err)
			}
			out = append(out, strings.Join(n.Faults, "|"))
			tr.Backpropagate(n, RolloutResult{Utility: float64(i), Evaluable: true})
		}
		return out
	}
	a, b := walk(), walk()
	if len(a) == 0 {
		t.Fatal("the tree produced no nodes")
	}
	if len(a) != len(b) {
		t.Fatalf("the same seed produced %d and %d nodes", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("rollout %d differs between two runs of the same seed:\n  %s\n  %s", i, a[i], b[i])
		}
	}
}

func TestNewTreeNeedsAConfigAndATopology(t *testing.T) {
	if _, err := NewTree(TreeOptions{Topology: ladderTopology(t)}); err == nil {
		t.Fatal("NewTree accepted a nil config")
	}
	if _, err := NewTree(TreeOptions{Config: ladderConfig(t, fixtureAllow(), nil)}); err == nil {
		t.Fatal("NewTree accepted a nil topology; every node's targets would go unchecked")
	}
}
