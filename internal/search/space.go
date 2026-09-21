package search

import (
	"errors"
	"fmt"
	"sort"
	"strconv"

	"github.com/WrdCstlg/pro-thesis/internal/perturber"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// ---------------------------------------------------------------------------
// The fault space
//
// One enumeration of what may be injected, shared by every strategy. It is
// shared on purpose: D-029 pre-registers a Fisher's exact comparison of the
// Saboteur against uniform random, and that comparison is only evidence if both
// arms draw from the same fault space with the same budget and the same world
// cost. A baseline restricted to a smaller space would be a strawman, and a
// benchmark against a strawman is the ranked #3 way this phase fails.
//
// Everything here produces CANDIDATES. Nothing here is emitted without passing
// through Validator, which is internal/perturber's own Compile (the same
// function the executor runs) so a search can never emit a world the compiler
// will reject mid-run.
// ---------------------------------------------------------------------------

// WindowPolicy bounds where in DRIVE a generated fault window may sit.
//
// The defaults are chosen against the fixture's measured geometry, not
// invented: the anomaly-producing schedule is net.partition@8200..13500 with
// proc.pause@8300..11000 (D-031), so the earliest start must be well below 8200
// and the latest end well above 13500, or the search could not express the
// schedule it exists to find.
type WindowPolicy struct {
	// EarliestMS is the earliest a window may start, DRIVE-relative.
	EarliestMS int64
	// LatestMS is the latest a window may end.
	LatestMS int64
	// MinDurationMS and MaxDurationMS bound a durative fault's length.
	MinDurationMS int64
	MaxDurationMS int64
	// GranularityMS quantises generated offsets. Coarse offsets make two
	// mutations of the same schedule likely to be comparable, and keep the
	// canonical fault string short.
	GranularityMS int64
}

// DefaultWindowPolicy is the compiled-in policy.
func DefaultWindowPolicy() WindowPolicy {
	return WindowPolicy{
		EarliestMS:    2000,
		LatestMS:      20000,
		MinDurationMS: 500,
		MaxDurationMS: 8000,
		GranularityMS: 100,
	}
}

func (p WindowPolicy) normalize() WindowPolicy {
	if p.GranularityMS <= 0 {
		p.GranularityMS = 1
	}
	if p.MinDurationMS < 0 {
		p.MinDurationMS = 0
	}
	if p.MaxDurationMS < p.MinDurationMS {
		p.MaxDurationMS = p.MinDurationMS
	}
	if p.EarliestMS < 0 {
		p.EarliestMS = 0
	}
	if p.LatestMS < p.EarliestMS+p.MinDurationMS {
		p.LatestMS = p.EarliestMS + p.MinDurationMS
	}
	return p
}

// quantize rounds v down to the policy's granularity.
func (p WindowPolicy) quantize(v int64) int64 {
	g := p.GranularityMS
	if g <= 1 {
		return v
	}
	return (v / g) * g
}

// magnitudeLadder is the ordered parameter ladder for one fault kind.
//
// Ordered ASCENDING in aggression, which is what makes "escalate magnitude" a
// well-defined mutation (mutate.go) and what gives the Saboteur's escalation
// ladder (A.7) a per-kind notion of a rung. Every value is written as the
// canonical SOURCE TEXT the fault grammar stores, never re-derived from a
// parsed number: schema.Param's contract is that the text is what is
// re-emitted, so 0.05 must never come back as 0.05000000000000000277.
type magnitudeLadder struct {
	// Rungs[i] is one complete parameter binding, in the kind's declared order.
	Rungs [][]string
}

// ladders is the compiled-in magnitude table.
//
// A kind with no declared parameters has a single empty rung: it has exactly
// one magnitude, and representing that as "no rungs" would make it
// unselectable.
var ladders = map[schema.FaultKind]magnitudeLadder{
	schema.FaultNetPartition: {Rungs: [][]string{{}}},
	schema.FaultNetLatency: {Rungs: [][]string{
		{"50", "10"}, {"100", "20"}, {"200", "50"}, {"500", "100"}, {"1000", "200"},
	}},
	schema.FaultNetLoss:      {Rungs: [][]string{{"1"}, {"5"}, {"15"}, {"30"}, {"60"}}},
	schema.FaultNetReorder:   {Rungs: [][]string{{"1"}, {"5"}, {"15"}, {"30"}}},
	schema.FaultNetDuplicate: {Rungs: [][]string{{"1"}, {"5"}, {"15"}, {"30"}}},
	schema.FaultNetBandwidth: {Rungs: [][]string{
		{"10000000"}, {"1000000"}, {"100000"}, {"10000"},
	}},
	schema.FaultProcKill:    {Rungs: [][]string{{"SIGTERM"}, {"SIGKILL"}}},
	schema.FaultProcPause:   {Rungs: [][]string{{}}},
	schema.FaultProcRestart: {Rungs: [][]string{{}}},
	schema.FaultProcSlow:    {Rungs: [][]string{{"50"}, {"20"}, {"10"}, {"5"}}},
	schema.FaultClockSkew: {Rungs: [][]string{
		{"100"}, {"-1000"}, {"1000"}, {"3000"}, {"5000"},
	}},
	schema.FaultClockJump: {Rungs: [][]string{
		{"100"}, {"-1000"}, {"1000"}, {"3000"}, {"5000"},
	}},
	schema.FaultIOLatency:   {Rungs: [][]string{{"20"}, {"50"}, {"200"}, {"1000"}}},
	schema.FaultIOError:     {Rungs: [][]string{{"0.01"}, {"0.05"}, {"0.2"}, {"0.5"}}},
	schema.FaultIOFill:      {Rungs: [][]string{{"50"}, {"80"}, {"95"}}},
	schema.FaultMemPressure: {Rungs: [][]string{{"25"}, {"50"}, {"75"}, {"90"}}},
	schema.FaultFDExhaust:   {Rungs: [][]string{{}}},
}

// Roles a role: target may name. The fixture reports role=leader/follower and
// the frozen grammar's own example is role:leader.
var searchRoles = []string{"follower", "leader"}

// Space is the enumerated action space for one config and topology.
type Space struct {
	// Kinds are the permitted fault kinds, sorted. Exactly
	// cfg.Perturber.EffectiveFaultKinds(), so allow/deny is honoured by
	// construction rather than by a second implementation of the same rule.
	Kinds []schema.FaultKind
	// Targets maps a kind to the targets legal for it, sorted by canonical
	// string. Edge targets appear only for the network family, because the
	// grammar refuses them elsewhere.
	Targets map[schema.FaultKind][]schema.Target
	// Window is the window policy.
	Window WindowPolicy
	// MaxFaults is the per-world fault ceiling every strategy must respect.
	//
	// It is min(perturber.budget.max_faults_per_world, MaxFaultsPerWorld). The
	// cap matters for the benchmark: A.5 caps the MCTS tree at 4 additional
	// faults, so letting the random arm draw 24-fault worlds would compare two
	// arms with different budgets and D-029's test would measure the budget
	// rather than the strategy.
	MaxFaults int
	// MaxConcurrent is perturber.budget.max_concurrent_faults.
	MaxConcurrent int
}

// MaxFaultsPerWorld is the search-side ceiling on a generated schedule.
//
// FIVE: A.5 caps the MCTS tree at "4 additional faults beyond the triggering
// probe fault", which is a five-fault world. Both strategies use this number,
// which is what makes D-029's pre-registered comparison a comparison of
// strategies rather than of budgets.
//
// It is also the ceiling A.9 #3 asks the DISCOVERED violation to respect
// ("<= 4 faults"); a search that could only ever emit four-fault worlds would
// satisfy that criterion by construction rather than by parsimony, so the
// generator is allowed one more than the target and the parsimony incentive is
// left to do its job.
const MaxFaultsPerWorld = 5

// ErrEmptySpace reports a config whose fault space is empty.
var ErrEmptySpace = errors.New("search: the fault space is empty; perturber.allow/deny permits no kind, " +
	"or no target resolves — a search over an empty space would report worlds it never perturbed")

// NewSpace enumerates the action space.
func NewSpace(cfg *schema.Config, top *perturber.Topology, win WindowPolicy) (*Space, error) {
	if cfg == nil {
		return nil, errors.New("search: NewSpace needs a config")
	}
	if top == nil {
		return nil, errors.New("search: NewSpace needs a topology; a space enumerated without one " +
			"would emit targets that resolve to no node")
	}
	kinds := cfg.Perturber.EffectiveFaultKinds()
	if len(kinds) == 0 {
		return nil, ErrEmptySpace
	}
	sorted := append([]schema.FaultKind(nil), kinds...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	targets := enumerateTargets(top)
	if len(targets) == 0 {
		return nil, ErrEmptySpace
	}

	s := &Space{
		Kinds:         sorted,
		Targets:       map[schema.FaultKind][]schema.Target{},
		Window:        win.normalize(),
		MaxConcurrent: cfg.Perturber.Budget.MaxConcurrentFaults,
	}

	maxFaults := cfg.Perturber.Budget.MaxFaultsPerWorld
	if maxFaults <= 0 || maxFaults > MaxFaultsPerWorld {
		maxFaults = MaxFaultsPerWorld
	}
	s.MaxFaults = maxFaults
	if s.MaxConcurrent <= 0 {
		s.MaxConcurrent = 1
	}

	usable := 0
	for _, k := range sorted {
		decl, ok := schema.LookupFaultKind(k)
		if !ok {
			continue
		}
		legal := make([]schema.Target, 0, len(targets))
		for _, t := range targets {
			if t.Kind == schema.TargetEdge && !decl.AllowEdge {
				continue
			}
			legal = append(legal, t)
		}
		sort.Slice(legal, func(i, j int) bool { return legal[i].String() < legal[j].String() })
		s.Targets[k] = legal
		if len(legal) > 0 {
			usable++
		}
	}
	if usable == 0 {
		return nil, ErrEmptySpace
	}
	return s, nil
}

// enumerateTargets builds every target form the frozen grammar offers, from the
// live topology.
//
// Sorted by canonical string throughout, so the enumeration does not depend on
// the order harness.nodes happens to be written in: reordering a config must
// not change which fault a seed produces.
func enumerateTargets(top *perturber.Topology) []schema.Target {
	nodes := top.Nodes()
	out := make([]schema.Target, 0, 32)

	for _, n := range nodes {
		out = append(out, schema.Target{Kind: schema.TargetNode, Node: n.ID})
	}

	services := top.Services()
	for _, svc := range services {
		group := top.Service(svc)
		out = append(out, schema.Target{Kind: schema.TargetWildcard, Service: svc})
		if len(group) >= 2 {
			out = append(out,
				schema.Target{Kind: schema.TargetQuorum, Func: schema.QuorumMinority, Scope: svc},
				schema.Target{Kind: schema.TargetQuorum, Func: schema.QuorumMajority, Scope: svc},
				schema.Target{Kind: schema.TargetQuorum, Func: schema.QuorumAny, Scope: svc, N: 1},
			)
		}
	}

	for _, r := range searchRoles {
		out = append(out, schema.Target{Kind: schema.TargetRole, Role: r})
	}

	// Edges within a logical group. Cross-group edges are omitted: a partition
	// between a kv node and a postgres node is expressible but is not a quorum
	// event, and enumerating it multiplies the space without adding a class of
	// failure the fixture can exhibit.
	for _, svc := range services {
		group := top.Service(svc)
		for i := 0; i < len(group); i++ {
			for j := i + 1; j < len(group); j++ {
				a, b := group[i].ID, group[j].ID
				if a > b {
					a, b = b, a
				}
				out = append(out, schema.Target{Kind: schema.TargetEdge, A: a, B: b})
			}
		}
	}

	// Drop anything that does not validate as a target, and de-duplicate by
	// canonical string.
	seen := map[string]bool{}
	keep := out[:0]
	for _, t := range out {
		if err := t.Validate(); err != nil {
			continue
		}
		s := t.String()
		if seen[s] {
			continue
		}
		seen[s] = true
		keep = append(keep, t)
	}
	sort.Slice(keep, func(i, j int) bool { return keep[i].String() < keep[j].String() })
	return keep
}

// Rungs returns the magnitude ladder for a kind. A kind with no declared
// parameters has exactly one rung, the empty binding.
func (s *Space) Rungs(k schema.FaultKind) [][]string {
	l, ok := ladders[k]
	if !ok || len(l.Rungs) == 0 {
		return [][]string{{}}
	}
	return l.Rungs
}

// RungOf reports which rung a fault's parameters sit on, and whether they were
// found on the ladder at all. A hand-authored fault whose parameters are not on
// the ladder returns (-1, false), and "escalate magnitude" then starts it at
// rung 0 rather than guessing.
func (s *Space) RungOf(f schema.FaultSpec) (int, bool) {
	rungs := s.Rungs(f.Kind)
	for i, r := range rungs {
		if len(r) != len(f.Params) {
			continue
		}
		match := true
		for j := range r {
			if f.Params[j].Value != r[j] {
				match = false
				break
			}
		}
		if match {
			return i, true
		}
	}
	return -1, false
}

// BuildFault assembles a canonical FaultSpec.
//
// It routes through schema.ParseFault rather than constructing a FaultSpec
// literal, so the result is exactly what the parser accepts and every declared
// parameter is materialized with its default. Building the struct by hand would
// let this package and the parser disagree about the canonical form, and the
// world hash is computed from that string.
func BuildFault(kind schema.FaultKind, target schema.Target, params []string, startMS, endMS int64) (schema.FaultSpec, error) {
	if endMS < startMS {
		return schema.FaultSpec{}, fmt.Errorf("search: window %d..%d ends before it starts", startMS, endMS)
	}
	expr := string(kind) + "(" + target.String()
	for _, p := range params {
		expr += ", " + p
	}
	expr += ")@" + strconv.FormatInt(startMS, 10) + ".." + strconv.FormatInt(endMS, 10)
	return schema.ParseFault(expr)
}

// ---------------------------------------------------------------------------
// Validator
// ---------------------------------------------------------------------------

// Validator refuses any schedule the executor would refuse.
//
// It is internal/perturber.Compile and nothing else. Re-implementing the
// allow-list, the budget, the constraint check or the zero-match target rule
// here would give the tree two definitions of "legal schedule", and the search
// would eventually emit a world that compiles in one and not the other: mid
// run, after the world had already cost thirty seconds.
type Validator struct {
	cfg *schema.Config
	top *perturber.Topology
	reg *perturber.Registry
}

// NewValidator builds a validator. reg may be nil: without it a kind with no
// injection mechanism on this host is not refused until the executor reaches
// it, which is exactly what perturber.Compile documents.
func NewValidator(cfg *schema.Config, top *perturber.Topology, reg *perturber.Registry) (*Validator, error) {
	if cfg == nil || top == nil {
		return nil, errors.New("search: NewValidator needs a config and a topology")
	}
	return &Validator{cfg: cfg, top: top, reg: reg}, nil
}

// Validate compiles a planned schedule and returns the compiler's own error.
func (v *Validator) Validate(planned []string) error {
	_, err := perturber.Compile(perturber.CompileOptions{
		Config:   v.cfg,
		Topology: v.top,
		Planned:  planned,
		Registry: v.reg,
	})
	return err
}

// ValidateWorld validates a world's planned schedule.
func (v *Validator) ValidateWorld(w *schema.World) error {
	if w == nil {
		return errors.New("search: ValidateWorld(nil)")
	}
	if err := w.Validate(); err != nil {
		return err
	}
	return v.Validate(w.FaultSchedule.Planned)
}
