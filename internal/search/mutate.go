package search

import (
	"errors"
	"fmt"
	"sort"

	"github.com/WrdCstlg/pro-thesis/internal/recorder"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// ---------------------------------------------------------------------------
// Mutation operators
//
// Every operator produces a fault string pkg/schema can parse and
// internal/perturber can compile against the allow-list, the budget and the
// safety constraints. The order is GENERATE, then VALIDATE, then emit, never
// emit and discover. A schedule refused at t+8200ms has already perturbed the
// system it was about to declare untested (perturber/compile.go says exactly
// this, and this file is the search-side half of that rule).
//
// The mutator is BIASED TOWARD OVERLAP, deliberately and by a wide margin. The
// base spec says most real distributed bugs need two concurrent faults; A.7
// Rung 4 says the same; and this repo has the measurement; D-031's schedule is
// net.partition(role:leader)@8200..13500 overlapping proc.pause(role:leader)@
// 8300..11000, and BOTH the overlap and the fact that the partition OUTLASTS
// the pause are load-bearing. A mutator that placed windows independently would
// reach that configuration only by accident.
// ---------------------------------------------------------------------------

// Operator names one mutation.
type Operator string

const (
	// OpAddFault appends a freshly generated fault.
	OpAddFault Operator = "add_fault"
	// OpRemoveFault drops one fault. It never empties a schedule: a zero-fault
	// world is the CONTROL, which the seed corpus already carries, and reaching
	// it by mutation would spend a world re-measuring the baseline.
	OpRemoveFault Operator = "remove_fault"
	// OpShiftWindow moves one fault's window without changing its length.
	OpShiftWindow Operator = "shift_window"
	// OpWidenWindow lengthens one fault's window.
	OpWidenWindow Operator = "widen_window"
	// OpNarrowWindow shortens one fault's window.
	OpNarrowWindow Operator = "narrow_window"
	// OpRetarget moves one fault to a related target: node to group, follower to
	// leader, edge to endpoint.
	OpRetarget Operator = "retarget"
	// OpEscalate moves one fault up its magnitude ladder.
	OpEscalate Operator = "escalate_magnitude"
	// OpSwapProfile changes the driver profile.
	OpSwapProfile Operator = "swap_driver_profile"
	// OpChangeSeed changes only the world seed. It is the VARIANCE PROBE: the
	// same schedule under different scheduling noise, which is how a search
	// distinguishes a reproducible finding from a flake without re-running the
	// identical world.
	OpChangeSeed Operator = "change_seed"
	// OpForceOverlap rewrites two faults so their windows overlap in time.
	OpForceOverlap Operator = "force_overlap"
)

// AllOperators is every operator, in a FIXED order.
//
// The order is part of the reproducibility contract: operator selection is a
// WeightedPPM draw over this slice, so reordering it would change every search
// that has ever been recorded against a seed.
var AllOperators = []Operator{
	OpAddFault,
	OpRemoveFault,
	OpShiftWindow,
	OpWidenWindow,
	OpNarrowWindow,
	OpRetarget,
	OpEscalate,
	OpSwapProfile,
	OpChangeSeed,
	OpForceOverlap,
}

// DefaultOperatorWeights is the compiled-in operator distribution.
//
// force_overlap carries roughly a quarter of the total mass: more than any
// other operator and more than three times add_fault. That is the overlap bias
// stated as a number rather than as an intention.
func DefaultOperatorWeights() map[Operator]uint32 {
	return map[Operator]uint32{
		OpAddFault:     10,
		OpRemoveFault:  6,
		OpShiftWindow:  10,
		OpWidenWindow:  8,
		OpNarrowWindow: 8,
		OpRetarget:     12,
		OpEscalate:     10,
		OpSwapProfile:  3,
		OpChangeSeed:   5,
		OpForceOverlap: 28,
	}
}

// ErrNoMutation reports that no operator produced a valid, DIFFERENT child
// within the attempt budget.
var ErrNoMutation = errors.New("search: no mutation produced a schedule the perturber accepts")

// Mutator applies operators to a parent world.
type Mutator struct {
	Space     *Space
	Validator *Validator
	// Profiles are the driver profile names available to OpSwapProfile, sorted.
	Profiles []string
	// Weights is the operator distribution. Missing entries weigh zero.
	Weights map[Operator]uint32
	// Attempts bounds how many operator draws one Mutate call makes.
	Attempts int
}

// NewMutator builds a mutator from the run's config.
func NewMutator(cfg *schema.Config, sp *Space, v *Validator) (*Mutator, error) {
	if cfg == nil || sp == nil {
		return nil, errors.New("search: NewMutator needs a config and a space")
	}
	profiles := make([]string, 0, len(cfg.Driver.Profiles))
	for name := range cfg.Driver.Profiles {
		profiles = append(profiles, name)
	}
	// Sorted, because a Go map's iteration order is randomized and a mutator
	// that picked a profile by map order would not replay.
	sort.Strings(profiles)
	return &Mutator{
		Space:     sp,
		Validator: v,
		Profiles:  profiles,
		Weights:   DefaultOperatorWeights(),
		Attempts:  GenerateAttempts,
	}, nil
}

// Mutate derives a child of parent.
//
// It returns the child, the operator that produced it, and an error. The child
// carries meta.origin=mutated and meta.parent_hash set to the parent's hash.
//
// A child identical to its parent is REJECTED and the attempt is retried. An
// operator that silently no-ops would spend a world re-running the parent while
// the corpus recorded it as a new lineage.
func (m *Mutator) Mutate(parent schema.World, st *recorder.Stream) (schema.World, Operator, error) {
	if m.Space == nil {
		return schema.World{}, "", errors.New("search: Mutator has no space")
	}
	parentHash, err := parent.Hash()
	if err != nil {
		return schema.World{}, "", fmt.Errorf("search: hash parent: %w", err)
	}

	weights := m.weightVector()
	attempts := m.Attempts
	if attempts <= 0 {
		attempts = GenerateAttempts
	}

	var lastErr error
	for i := 0; i < attempts; i++ {
		op := AllOperators[st.WeightedPPM(weights)]
		child, err := Derive(parent, OriginMutated, nil)
		if err != nil {
			return schema.World{}, "", err
		}
		applied, err := m.apply(op, &child, st)
		if err != nil {
			lastErr = err
			continue
		}
		if !applied {
			lastErr = fmt.Errorf("%s was not applicable", op)
			continue
		}
		child = Stamp(child, OriginMutated, parentHash)
		h, err := child.Hash()
		if err != nil {
			lastErr = err
			continue
		}
		if h == parentHash {
			lastErr = fmt.Errorf("%s produced the parent unchanged", op)
			continue
		}
		if m.Validator != nil {
			if err := m.Validator.ValidateWorld(&child); err != nil {
				lastErr = err
				continue
			}
		}
		return child, op, nil
	}
	if lastErr == nil {
		lastErr = errors.New("no operator was applicable")
	}
	return schema.World{}, "", fmt.Errorf("%w after %d attempts: %v", ErrNoMutation, attempts, lastErr)
}

// weightVector projects Weights onto AllOperators' fixed order.
func (m *Mutator) weightVector() []uint32 {
	out := make([]uint32, len(AllOperators))
	var total uint64
	for i, op := range AllOperators {
		out[i] = m.Weights[op]
		total += uint64(out[i])
	}
	if total == 0 {
		// A zero total would panic inside WeightedPPM. A mutator configured with
		// no weights falls back to uniform rather than crashing a search that is
		// six worlds from a finding.
		for i := range out {
			out[i] = 1
		}
	}
	return out
}

// apply runs one operator against the child in place.
//
// It returns (false, nil) when the operator has nothing to act on: a remove on
// a one-fault schedule, an escalate on a kind with one rung. That is not an
// error: it is a legitimate outcome that the retry loop absorbs.
func (m *Mutator) apply(op Operator, w *schema.World, st *recorder.Stream) (bool, error) {
	specs, err := schema.ParseFaults(w.FaultSchedule.Planned)
	if err != nil {
		return false, err
	}

	switch op {
	case OpAddFault:
		if len(specs) >= m.Space.MaxFaults {
			return false, nil
		}
		f, err := RandomFault(m.Space, st)
		if err != nil {
			return false, err
		}
		specs = append(specs, f)

	case OpRemoveFault:
		if len(specs) <= 1 {
			return false, nil
		}
		i := int(st.Uint64n(uint64(len(specs))))
		specs = append(specs[:i:i], specs[i+1:]...)

	case OpShiftWindow, OpWidenWindow, OpNarrowWindow:
		if len(specs) == 0 {
			return false, nil
		}
		i := int(st.Uint64n(uint64(len(specs))))
		if !m.reshape(op, &specs[i], st) {
			return false, nil
		}

	case OpRetarget:
		if len(specs) == 0 {
			return false, nil
		}
		i := int(st.Uint64n(uint64(len(specs))))
		if !m.retarget(&specs[i], st) {
			return false, nil
		}

	case OpEscalate:
		if len(specs) == 0 {
			return false, nil
		}
		i := int(st.Uint64n(uint64(len(specs))))
		next, ok := m.escalated(specs[i], st)
		if !ok {
			return false, nil
		}
		specs[i] = next

	case OpSwapProfile:
		if len(m.Profiles) < 2 {
			return false, nil
		}
		for tries := 0; tries < 4; tries++ {
			p := pick(st, m.Profiles)
			if p != w.DriverProfile {
				w.DriverProfile = p
				return true, nil
			}
		}
		return false, nil

	case OpChangeSeed:
		w.Seed = st.Uint64()
		return true, nil

	case OpForceOverlap:
		var err error
		specs, err = m.forceOverlap(specs, st)
		if err != nil {
			return false, err
		}
		if specs == nil {
			return false, nil
		}

	default:
		return false, fmt.Errorf("search: unknown mutation operator %q", op)
	}

	planned := make([]string, 0, len(specs))
	for _, s := range specs {
		planned = append(planned, s.String())
	}
	w.FaultSchedule.Planned = schema.SortFaultStrings(planned)
	return true, nil
}

// reshape moves or resizes one window inside the policy.
func (m *Mutator) reshape(op Operator, f *schema.FaultSpec, st *recorder.Stream) bool {
	p := m.Space.Window
	dur := f.EndMS - f.StartMS

	switch op {
	case OpShiftWindow:
		latest := p.LatestMS - dur
		if latest < p.EarliestMS {
			return false
		}
		span := latest - p.EarliestMS + 1
		start := p.quantize(p.EarliestMS + int64(st.Uint64n(uint64(span))))
		if start < p.EarliestMS {
			start = p.EarliestMS
		}
		if start == f.StartMS {
			return false
		}
		f.StartMS, f.EndMS = start, start+dur

	case OpWidenWindow:
		room := p.MaxDurationMS - dur
		if avail := p.LatestMS - f.StartMS - dur; avail < room {
			room = avail
		}
		if room <= 0 {
			return false
		}
		grow := p.quantize(1 + int64(st.Uint64n(uint64(room))))
		if grow <= 0 {
			grow = p.GranularityMS
		}
		if f.EndMS+grow > p.LatestMS {
			return false
		}
		f.EndMS += grow

	case OpNarrowWindow:
		room := dur - p.MinDurationMS
		if room <= 0 {
			return false
		}
		shrink := p.quantize(1 + int64(st.Uint64n(uint64(room))))
		if shrink <= 0 {
			shrink = p.GranularityMS
		}
		if dur-shrink < p.MinDurationMS {
			return false
		}
		f.EndMS -= shrink
	}
	return f.EndMS >= f.StartMS
}

// retarget moves a fault to a RELATED target rather than a uniformly random
// one.
//
// The relations are the three the spec names (node to group, follower to
// leader, edge to node) and they are directional because that is where the
// signal is: `role:leader` is the target that produces an election, and a
// mutator that replaced a leader target with a uniformly drawn node would
// discard the one piece of structure it had.
func (m *Mutator) retarget(f *schema.FaultSpec, st *recorder.Stream) bool {
	legal := m.Space.Targets[f.Kind]
	if len(legal) == 0 {
		return false
	}
	cands := m.relatedTargets(f.Kind, f.Target)
	if len(cands) == 0 {
		cands = legal
	}
	cur := f.Target.String()
	for tries := 0; tries < 8; tries++ {
		t := pick(st, cands)
		if t.String() == cur {
			continue
		}
		f.Target = t
		return true
	}
	return false
}

// relatedTargets returns the escalation-adjacent targets of t, restricted to
// those legal for the kind.
func (m *Mutator) relatedTargets(kind schema.FaultKind, t schema.Target) []schema.Target {
	legal := m.Space.Targets[kind]
	allowed := map[string]bool{}
	for _, x := range legal {
		allowed[x.String()] = true
	}
	keep := func(cands []schema.Target) []schema.Target {
		out := make([]schema.Target, 0, len(cands))
		for _, c := range cands {
			if err := c.Validate(); err != nil {
				continue
			}
			if allowed[c.String()] {
				out = append(out, c)
			}
		}
		sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
		return out
	}

	switch t.Kind {
	case schema.TargetNode:
		// node -> the groups and roles that contain it.
		var out []schema.Target
		for _, x := range legal {
			switch x.Kind {
			case schema.TargetWildcard, schema.TargetQuorum, schema.TargetRole:
				out = append(out, x)
			}
		}
		return keep(out)

	case schema.TargetRole:
		// follower -> leader, and leader -> follower. Both directions, because a
		// search that could only ever move toward the leader could never walk
		// back out of a dead end.
		var out []schema.Target
		for _, r := range searchRoles {
			if r != t.Role {
				out = append(out, schema.Target{Kind: schema.TargetRole, Role: r})
			}
		}
		return keep(out)

	case schema.TargetEdge:
		// edge -> either endpoint as a node.
		return keep([]schema.Target{
			{Kind: schema.TargetNode, Node: t.A},
			{Kind: schema.TargetNode, Node: t.B},
		})

	case schema.TargetWildcard, schema.TargetQuorum:
		// group -> a narrower selection of the same scope, or a role.
		scope := t.Service
		if t.Kind == schema.TargetQuorum {
			scope = t.Scope
		}
		var out []schema.Target
		for _, x := range legal {
			switch x.Kind {
			case schema.TargetQuorum:
				if x.Scope == scope {
					out = append(out, x)
				}
			case schema.TargetRole:
				out = append(out, x)
			}
		}
		return keep(out)
	}
	return nil
}

// escalated returns f one rung up its magnitude ladder.
//
// A fault whose parameters are not on the ladder (a hand-authored world, or
// one from an older ladder) restarts at rung 0 rather than being guessed at.
// Guessing a rung from a parsed number would mean re-deriving the parameter
// TEXT, and schema.Param's whole contract is that the text is never re-derived.
func (m *Mutator) escalated(f schema.FaultSpec, st *recorder.Stream) (schema.FaultSpec, bool) {
	rungs := m.Space.Rungs(f.Kind)
	if len(rungs) < 2 {
		return f, false
	}
	cur, ok := m.Space.RungOf(f)
	next := 0
	if ok {
		next = cur + 1
		if next >= len(rungs) {
			// Already at the top. Wrap to a lower rung so the operator stays
			// applicable rather than becoming dead weight on a saturated fault.
			next = int(st.Uint64n(uint64(len(rungs) - 1)))
		}
	}
	out, err := BuildFault(f.Kind, f.Target, rungs[next], f.StartMS, f.EndMS)
	if err != nil {
		return f, false
	}
	if out.String() == f.String() {
		return f, false
	}
	return out, true
}

// forceOverlap rewrites the schedule so two faults are concurrent.
//
// Two shapes, drawn evenly, and both are needed:
//
//   - CONTAINED: B sits strictly inside A. This is a fault that fires and is
//     withdrawn while another is still in force.
//   - STRADDLE: A sits strictly inside B, i.e. B OUTLASTS A. This is the shape
//     that reproduces the fixture: the partition must outlast the pause, or the
//     displaced leader learns it was displaced the instant it unfreezes and
//     steps down before serving anything (D-031). A mutator that only produced
//     containment could reach the anomaly only by relabelling which fault was
//     which.
//
// With a single fault it ADDS one and overlaps it, so the operator is
// applicable to every non-empty schedule.
func (m *Mutator) forceOverlap(specs []schema.FaultSpec, st *recorder.Stream) ([]schema.FaultSpec, error) {
	if len(specs) == 0 {
		return nil, nil
	}
	if len(specs) == 1 {
		if len(specs) >= m.Space.MaxFaults {
			return nil, nil
		}
		f, err := RandomFault(m.Space, st)
		if err != nil {
			return nil, err
		}
		specs = append(specs, f)
	}

	i := int(st.Uint64n(uint64(len(specs))))
	j := int(st.Uint64n(uint64(len(specs) - 1)))
	if j >= i {
		j++
	}

	p := m.Space.Window
	g := p.GranularityMS
	if g <= 0 {
		g = 1
	}
	straddle := st.Uint64n(2) == 1

	a, b := specs[i], specs[j]
	if straddle {
		// b OUTLASTS a on both edges.
		start := a.StartMS - g
		if start < p.EarliestMS {
			start = p.EarliestMS
		}
		end := a.EndMS + g
		if end > p.LatestMS {
			end = p.LatestMS
		}
		if end <= start {
			return nil, nil
		}
		b.StartMS, b.EndMS = start, end
	} else {
		// b sits inside a.
		if a.EndMS-a.StartMS <= 2*g {
			return nil, nil
		}
		start := a.StartMS + g
		end := a.EndMS - g
		if end <= start {
			return nil, nil
		}
		b.StartMS, b.EndMS = start, end
	}
	if b.EndMS < b.StartMS {
		return nil, nil
	}
	// Rebuild through the parser so the result is canonical rather than a
	// hand-edited struct.
	rebuilt, err := schema.ParseFault(b.String())
	if err != nil {
		return nil, err
	}
	specs[j] = rebuilt
	return specs, nil
}
