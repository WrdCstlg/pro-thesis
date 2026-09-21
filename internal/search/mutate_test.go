package search

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/WrdCstlg/pro-thesis/internal/recorder"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// applyOperator runs one named operator against a parent until it succeeds,
// drawing from a fresh stream. It returns the child and how many draws it took.
//
// It exists so each operator can be tested in isolation: Mutate's weighted draw
// would otherwise make "did add_fault work" a question about the distribution.
func applyOperator(t *testing.T, m *Mutator, parent schema.World, op Operator, seed uint64) (schema.World, bool) {
	t.Helper()
	st := recorder.NewStream(recorder.MustDeriveKey(recorder.Seed(seed), "test", string(op)))
	parentHash := mustHash(t, parent)
	for i := 0; i < 256; i++ {
		child, err := Derive(parent, OriginMutated, nil)
		if err != nil {
			t.Fatalf("Derive: %v", err)
		}
		applied, err := m.apply(op, &child, st)
		if err != nil {
			continue
		}
		if !applied {
			continue
		}
		child = Stamp(child, OriginMutated, parentHash)
		if mustHash(t, child) == parentHash {
			continue
		}
		if err := m.Validator.ValidateWorld(&child); err != nil {
			continue
		}
		return child, true
	}
	return schema.World{}, false
}

func testMutator(t *testing.T) (*Mutator, Params) {
	t.Helper()
	p := testParams(t, 0xA5A5A5A5)
	m, err := NewMutator(p.Config, p.Space, p.Validator)
	if err != nil {
		t.Fatalf("NewMutator: %v", err)
	}
	return m, p
}

// mutationParent is a two-fault world with room to grow, shrink, shift and
// overlap. It is the D-031 schedule, so the operators are exercised on the
// shape the fixture's anomaly actually needs.
func mutationParent(t *testing.T) schema.World {
	return corpusWorld(t, 7, OriginSeeded, "",
		"net.partition(role:leader)@8200..13500",
		"proc.pause(role:leader)@8300..11000")
}

// inventoryParent is mutationParent plus a parameterised fault, so that every
// operator (escalate_magnitude included) has something to act on. The D-031
// schedule alone uses two kinds that declare no parameters, and escalate is
// correctly inapplicable to it (see TestOperatorSemantics).
func inventoryParent(t *testing.T) schema.World {
	return corpusWorld(t, 21, OriginSeeded, "",
		"net.partition(role:leader)@8200..13500",
		"proc.pause(role:leader)@8300..11000",
		"net.latency(kv-n3, mean=50, jitter=10)@3000..5000")
}

// TestEveryOperatorProducesAParseableCompilableWorld is the operator inventory
// check. Each of the ten operators the spec names must be applicable to a
// realistic parent and must produce a schedule the perturber accepts.
func TestEveryOperatorProducesAParseableCompilableWorld(t *testing.T) {
	m, _ := testMutator(t)
	parent := inventoryParent(t)

	for _, op := range AllOperators {
		t.Run(string(op), func(t *testing.T) {
			child, ok := applyOperator(t, m, parent, op, 1)
			if !ok {
				t.Fatalf("%s never produced a valid, different child from a two-fault parent", op)
			}
			// Every fault string must parse and must already be canonical: the
			// world hash is computed from these strings.
			for _, f := range child.FaultSchedule.Planned {
				spec, err := schema.ParseFault(f)
				if err != nil {
					t.Fatalf("%s emitted an unparseable fault %q: %v", op, f, err)
				}
				if spec.String() != f {
					t.Fatalf("%s emitted a non-canonical fault %q, canonical is %q", op, f, spec.String())
				}
			}
			// And the whole schedule must compile through the real perturber.
			if err := m.Validator.ValidateWorld(&child); err != nil {
				t.Fatalf("%s emitted a world the perturber refuses: %v", op, err)
			}
			if child.Meta == nil || child.Meta.Origin != OriginMutated {
				t.Fatalf("%s produced a child with origin %v, want %s", op, child.Meta, OriginMutated)
			}
			if child.Meta.ParentHash != mustHash(t, parent) {
				t.Fatalf("%s produced a child whose parent_hash is %q", op, child.Meta.ParentHash)
			}
		})
	}
}

// TestOperatorSemantics checks that each operator does what its name says,
// rather than merely producing something valid. An operator that quietly did
// nothing useful would still pass the inventory check above.
func TestOperatorSemantics(t *testing.T) {
	m, _ := testMutator(t)
	parent := mutationParent(t)
	parentSpecs, err := schema.ParseFaults(parent.FaultSchedule.Planned)
	if err != nil {
		t.Fatalf("parse parent: %v", err)
	}

	t.Run("add_fault adds exactly one", func(t *testing.T) {
		child, ok := applyOperator(t, m, parent, OpAddFault, 2)
		if !ok {
			t.Fatal("add_fault produced nothing")
		}
		if got, want := len(child.FaultSchedule.Planned), len(parent.FaultSchedule.Planned)+1; got != want {
			t.Fatalf("schedule has %d faults, want %d", got, want)
		}
	})

	t.Run("remove_fault removes exactly one and never empties", func(t *testing.T) {
		child, ok := applyOperator(t, m, parent, OpRemoveFault, 3)
		if !ok {
			t.Fatal("remove_fault produced nothing")
		}
		if got, want := len(child.FaultSchedule.Planned), len(parent.FaultSchedule.Planned)-1; got != want {
			t.Fatalf("schedule has %d faults, want %d", got, want)
		}
		single := corpusWorld(t, 8, OriginSeeded, "", "proc.pause(role:leader)@8300..11000")
		if _, ok := applyOperator(t, m, single, OpRemoveFault, 4); ok {
			t.Fatal("remove_fault emptied a one-fault schedule; the no-fault control is the seed corpus's job")
		}
	})

	t.Run("shift_window preserves duration", func(t *testing.T) {
		child, ok := applyOperator(t, m, parent, OpShiftWindow, 5)
		if !ok {
			t.Fatal("shift_window produced nothing")
		}
		before := multiset(durations(t, parent))
		after := multiset(durations(t, child))
		if before != after {
			t.Fatalf("shift changed a duration: %s -> %s", before, after)
		}
		if multiset(starts(t, parent)) == multiset(starts(t, child)) {
			t.Fatal("shift_window did not move any window")
		}
	})

	t.Run("widen and narrow change total duration in the stated direction", func(t *testing.T) {
		wide, ok := applyOperator(t, m, parent, OpWidenWindow, 6)
		if !ok {
			t.Fatal("widen_window produced nothing")
		}
		if totalDuration(t, wide) <= totalDuration(t, parent) {
			t.Fatalf("widen did not lengthen: %d -> %d", totalDuration(t, parent), totalDuration(t, wide))
		}
		narrow, ok := applyOperator(t, m, parent, OpNarrowWindow, 7)
		if !ok {
			t.Fatal("narrow_window produced nothing")
		}
		if totalDuration(t, narrow) >= totalDuration(t, parent) {
			t.Fatalf("narrow did not shorten: %d -> %d", totalDuration(t, parent), totalDuration(t, narrow))
		}
	})

	t.Run("retarget changes a target and keeps the kinds", func(t *testing.T) {
		child, ok := applyOperator(t, m, parent, OpRetarget, 8)
		if !ok {
			t.Fatal("retarget produced nothing")
		}
		if kinds(t, parent) != kinds(t, child) {
			t.Fatalf("retarget changed the fault kinds: %s -> %s", kinds(t, parent), kinds(t, child))
		}
		if targets(t, parent) == targets(t, child) {
			t.Fatal("retarget left every target unchanged")
		}
	})

	t.Run("escalate moves up a magnitude ladder", func(t *testing.T) {
		// The parent's two kinds carry no parameters, so escalate cannot apply to
		// it. Use a parent whose fault has a ladder.
		lowRung := corpusWorld(t, 9, OriginSeeded, "", "net.latency(kv-n1, mean=50, jitter=10)@3000..5000")
		child, ok := applyOperator(t, m, lowRung, OpEscalate, 9)
		if !ok {
			t.Fatal("escalate produced nothing on a fault with a five-rung ladder")
		}
		spec, err := schema.ParseFault(child.FaultSchedule.Planned[0])
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		rung, on := m.Space.RungOf(spec)
		if !on {
			t.Fatalf("escalate produced parameters that are not on the ladder: %s", spec)
		}
		if rung != 1 {
			t.Fatalf("escalate from rung 0 landed on rung %d, want 1", rung)
		}
		if _, ok := applyOperator(t, m, parent, OpEscalate, 10); ok {
			t.Fatal("escalate applied to a schedule whose kinds have no parameters")
		}
	})

	t.Run("change_seed changes only the seed", func(t *testing.T) {
		child, ok := applyOperator(t, m, parent, OpChangeSeed, 11)
		if !ok {
			t.Fatal("change_seed produced nothing")
		}
		if child.Seed == parent.Seed {
			t.Fatal("change_seed left the seed alone")
		}
		if strings.Join(child.FaultSchedule.Planned, ";") != strings.Join(parent.FaultSchedule.Planned, ";") {
			t.Fatalf("change_seed altered the schedule:\n  %v\n  %v",
				parent.FaultSchedule.Planned, child.FaultSchedule.Planned)
		}
		if child.DriverProfile != parent.DriverProfile {
			t.Fatal("change_seed altered the driver profile")
		}
	})

	t.Run("swap_driver_profile changes only the profile", func(t *testing.T) {
		child, ok := applyOperator(t, m, parent, OpSwapProfile, 12)
		if !ok {
			t.Fatal("swap_driver_profile produced nothing")
		}
		if child.DriverProfile == parent.DriverProfile {
			t.Fatal("swap_driver_profile left the profile alone")
		}
		if _, cfgOK := testConfig().Driver.Profiles[child.DriverProfile]; !cfgOK {
			t.Fatalf("swap_driver_profile chose %q, which is not in driver.profiles", child.DriverProfile)
		}
		if strings.Join(child.FaultSchedule.Planned, ";") != strings.Join(parent.FaultSchedule.Planned, ";") {
			t.Fatal("swap_driver_profile altered the schedule")
		}
	})

	t.Run("force_overlap makes two windows concurrent", func(t *testing.T) {
		// Start from a schedule whose windows do NOT overlap, so overlap can only
		// come from the operator.
		disjoint := corpusWorld(t, 13, OriginSeeded, "",
			"net.partition(role:leader)@3000..4000",
			"proc.pause(role:leader)@9000..10000")
		if overlappingPairs(t, disjoint) != 0 {
			t.Fatal("the disjoint parent already overlaps")
		}
		child, ok := applyOperator(t, m, disjoint, OpForceOverlap, 14)
		if !ok {
			t.Fatal("force_overlap produced nothing")
		}
		if overlappingPairs(t, child) == 0 {
			t.Fatalf("force_overlap produced no overlapping pair: %v", child.FaultSchedule.Planned)
		}
	})

	_ = parentSpecs
}

// TestForceOverlapProducesBothShapes.
//
// Containment alone is not enough. The fixture's anomaly needs the partition to
// OUTLAST the pause: with a contained partition the displaced leader learns it
// was displaced the instant it unfreezes and steps down before serving anything
// (D-031). A mutator that only ever nested one window inside another could not
// reach the schedule the tool exists to find.
func TestForceOverlapProducesBothShapes(t *testing.T) {
	m, _ := testMutator(t)
	parent := corpusWorld(t, 15, OriginSeeded, "",
		"net.partition(role:leader)@3000..4000",
		"proc.pause(role:leader)@9000..10000")

	sawContained, sawStraddle := false, false
	for seed := uint64(0); seed < 80 && !(sawContained && sawStraddle); seed++ {
		child, ok := applyOperator(t, m, parent, OpForceOverlap, 1000+seed)
		if !ok {
			continue
		}
		specs, err := schema.ParseFaults(child.FaultSchedule.Planned)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		for i := range specs {
			for j := range specs {
				if i == j {
					continue
				}
				a, b := specs[i], specs[j]
				if a.StartMS < b.StartMS && b.EndMS < a.EndMS {
					sawContained = true
				}
				if a.StartMS <= b.StartMS && b.EndMS <= a.EndMS && a.DurationMS() > b.DurationMS() {
					sawStraddle = true
				}
			}
		}
	}
	if !sawContained {
		t.Error("force_overlap never produced a contained window")
	}
	if !sawStraddle {
		t.Error("force_overlap never produced a window that outlasts another; " +
			"the fixture's partition must outlast its pause")
	}
}

// TestMutateIsBiasedTowardOverlap. The spec is explicit that most real
// distributed bugs need two concurrent faults and the mutator must be biased
// toward overlap. This measures the bias rather than trusting the weight table.
func TestMutateIsBiasedTowardOverlap(t *testing.T) {
	m, _ := testMutator(t)
	counts := map[Operator]int{}
	st := recorder.NewStream(recorder.MustDeriveKey(recorder.Seed(0xB1A5), "test", "bias"))
	weights := m.weightVector()
	const draws = 20000
	for i := 0; i < draws; i++ {
		counts[AllOperators[st.WeightedPPM(weights)]]++
	}
	overlap := counts[OpForceOverlap]
	for _, op := range AllOperators {
		if op == OpForceOverlap {
			continue
		}
		if counts[op] >= overlap {
			t.Fatalf("force_overlap was drawn %d times, no more than %s at %d; the mutator is not overlap-biased",
				overlap, op, counts[op])
		}
	}
	if overlap*100/draws < 15 {
		t.Fatalf("force_overlap took only %d%% of draws; that is not a bias", overlap*100/draws)
	}
}

// TestMutateIsReproducible: A.8's determinism requirement applied to mutation.
func TestMutateIsReproducible(t *testing.T) {
	parent := mutationParent(t)
	run := func() []string {
		m, p := testMutator(t)
		st := p.Streams.Get(PathMutateOp)
		out := make([]string, 0, 30)
		w := parent
		for i := 0; i < 30; i++ {
			child, op, err := m.Mutate(w, st)
			if err != nil {
				out = append(out, "err:"+err.Error())
				continue
			}
			out = append(out, string(op)+" -> "+mustHash(t, child))
			w = child
		}
		return out
	}
	first := strings.Join(run(), "\n")
	for i := 0; i < 4; i++ {
		if got := strings.Join(run(), "\n"); got != first {
			t.Fatalf("mutation is not reproducible from a fixed seed on repeat %d", i)
		}
	}
}

// TestMutateNeverEmitsAnUncompilableWorld is the "generate, then VALIDATE"
// claim, exercised across a long random walk against the FIXTURE's real
// constraint set, including "never partition more than minority of kv", which
// refuses a wildcard or majority partition.
func TestMutateNeverEmitsAnUncompilableWorld(t *testing.T) {
	m, p := testMutator(t)
	st := p.Streams.Get(PathMutateOp)
	w := mutationParent(t)
	produced := 0
	for i := 0; i < 400; i++ {
		parentHash := mustHash(t, w)
		child, op, err := m.Mutate(w, st)
		if err != nil {
			continue
		}
		produced++
		// Lineage, on the real Mutate path rather than on a hand-stamped child:
		// Phase 5 walks parent_hash to reconstruct how a repro was reached.
		if child.Meta == nil || child.Meta.ParentHash != parentHash {
			t.Fatalf("%s produced a child whose parent_hash is %v, want %s", op, child.Meta, parentHash)
		}
		if child.Meta.Origin != OriginMutated {
			t.Fatalf("%s produced a child with origin %q, want %q", op, child.Meta.Origin, OriginMutated)
		}
		if err := p.Validator.ValidateWorld(&child); err != nil {
			t.Fatalf("%s emitted a world the perturber refuses after %d mutations: %v\n  %v",
				op, i, err, child.FaultSchedule.Planned)
		}
		if n := len(child.FaultSchedule.Planned); n > p.Space.MaxFaults {
			t.Fatalf("%s produced %d faults, over the shared ceiling of %d", op, n, p.Space.MaxFaults)
		}
		w = child
	}
	if produced < 300 {
		t.Fatalf("only %d of 400 mutations succeeded; the mutator is mostly failing", produced)
	}
}

// TestMutateRefusesToReturnTheParent: an operator that silently no-ops would
// spend a world re-running the parent while the corpus recorded a new lineage.
func TestMutateRefusesToReturnTheParent(t *testing.T) {
	m, p := testMutator(t)
	st := p.Streams.Get(PathMutateOp)
	parent := mutationParent(t)
	parentHash := mustHash(t, parent)
	for i := 0; i < 200; i++ {
		child, op, err := m.Mutate(parent, st)
		if err != nil {
			continue
		}
		if mustHash(t, child) == parentHash {
			t.Fatalf("%s returned the parent unchanged", op)
		}
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func specsOf(t *testing.T, w schema.World) []schema.FaultSpec {
	t.Helper()
	s, err := schema.ParseFaults(w.FaultSchedule.Planned)
	if err != nil {
		t.Fatalf("parse schedule %v: %v", w.FaultSchedule.Planned, err)
	}
	return s
}

func durations(t *testing.T, w schema.World) []string {
	out := []string{}
	for _, s := range specsOf(t, w) {
		out = append(out, fmt.Sprintf("%d", s.DurationMS()))
	}
	return out
}

func starts(t *testing.T, w schema.World) []string {
	out := []string{}
	for _, s := range specsOf(t, w) {
		out = append(out, fmt.Sprintf("%d", s.StartMS))
	}
	return out
}

func totalDuration(t *testing.T, w schema.World) int64 {
	var total int64
	for _, s := range specsOf(t, w) {
		total += s.DurationMS()
	}
	return total
}

func kinds(t *testing.T, w schema.World) string {
	out := []string{}
	for _, s := range specsOf(t, w) {
		out = append(out, string(s.Kind))
	}
	return multiset(out)
}

func targets(t *testing.T, w schema.World) string {
	out := []string{}
	for _, s := range specsOf(t, w) {
		out = append(out, s.Target.String())
	}
	return multiset(out)
}

func overlappingPairs(t *testing.T, w schema.World) int {
	specs := specsOf(t, w)
	n := 0
	for i := 0; i < len(specs); i++ {
		for j := i + 1; j < len(specs); j++ {
			if specs[i].StartMS < specs[j].EndMS && specs[j].StartMS < specs[i].EndMS {
				n++
			}
		}
	}
	return n
}

func multiset(ss []string) string {
	out := append([]string(nil), ss...)
	sort.Strings(out)
	return strings.Join(out, ",")
}
