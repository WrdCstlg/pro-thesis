package saboteur

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/WrdCstlg/pro-thesis/internal/perturber"
	"github.com/WrdCstlg/pro-thesis/internal/recorder"
	"github.com/WrdCstlg/pro-thesis/internal/search"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// ---------------------------------------------------------------------------
// The Saboteur as a search.Strategy
//
// This is the only file in the package that imports internal/search, and it is
// the integration seam: everything else here is a total function of its
// arguments and can be tested without a corpus, a coverage extractor or a
// container runtime.
//
// It runs A.2's three sub-modules as three STAGES over the world budget:
//
//	PROBE     A.3's sweep, control world FIRST, one micro-probe per (kind,
//	          target). Cheap because a world costs what its schedule spans.
//	ESCALATE  A.5's MCTS trees, rooted at the top-ranked REINFORCE signals
//	          OBSERVE classified, descending the A.7 ladder.
//	MUTATE    A.5's own fallback: "If no REINFORCE signal is detected during
//	          probing, fall back to the base spec's stochastic corpus mutation
//	          for the remaining budget. The Saboteur is an accelerant, not a
//	          replacement for baseline coverage." It is also where an exhausted
//	          tree lands, for the same reason.
//
// The stage is a function of what has been OBSERVED, never of wall-clock luck,
// so the same seed against the same fixture walks the same stages.
// ---------------------------------------------------------------------------

// StageName is which of A.2's sub-modules is currently choosing.
type StageName string

const (
	StageProbe    StageName = "probe"
	StageEscalate StageName = "escalate"
	StageMutate   StageName = "mutate"
)

// Options builds a Saboteur.
type Options struct {
	// Params is the shared search substrate. Both arms of D-029's comparison
	// are built from the same type for the same reason: the benchmark must
	// compare strategies, not budgets.
	Params search.Params
	// Topology is the compile-time topology the ladder and the sweep draw
	// targets from. Required.
	Topology *perturber.Topology

	// Search is the resolved `search:` block. Its defaults are already applied
	// by schema.Config.
	Search schema.SearchConfig

	// MaxProbeWorlds truncates the Tier 1 sweep to A.5's probe budget. Zero or
	// negative means the whole sweep.
	MaxProbeWorlds int

	// ProbeAtMS, ProbeWindowMS and EscalateAtMS place the generated windows.
	// Zero means the documented defaults.
	ProbeAtMS     int64
	ProbeWindowMS int64
	EscalateAtMS  int64

	// LeaseMS and Timing are the ladder's window arithmetic.
	LeaseMS int64
	Timing  LadderTiming

	// MaxTrees bounds the escalation. Zero means DefaultMaxTrees.
	MaxTrees int

	// KindSupported is the platform capability check. nil means
	// faults.PlatformCapability.
	KindSupported func(schema.FaultKind) error

	// Log receives one line per decision, or nil. It is how a human sees which
	// stage chose a world and why.
	Log func(format string, args ...any)
}

// Saboteur is Addendum A's adversarial agent, as a search.Strategy.
type Saboteur struct {
	opts    Options
	weights Weights
	observe ObserveOptions
	mutator *search.Mutator

	// Tier 1
	plan      SweepPlan
	probeNext int
	probeDone bool
	outcomes  []ProbeOutcome
	// baselines are the control world's per-metric medians, which every
	// comparative criterion in A.3 needs and which A.3 itself omits.
	baselines []Baseline
	controlOK bool

	// Tier 2
	esc      *Escalation
	escBuilt bool
	escErr   error
	// pending maps a proposed world's hash to the tree node it came from, so a
	// batch of concurrent rollouts backpropagates to the right places.
	pending map[string]pendingRollout

	// bookkeeping
	stage    StageName
	proposed int
	observed int
	// signals is A.3's ranked output, computed once when probing ends.
	signals []ProbeSignal
	// treeStats snapshots each tree at the end, so the D-015 measurement
	// survives the trees being retired.
	treeStats []TreeStats
}

type pendingRollout struct {
	tree *Tree
	node *Node
}

// New builds the Saboteur.
func New(o Options) (*Saboteur, error) {
	if err := o.Params.Validate(); err != nil {
		return nil, err
	}
	if o.Topology == nil {
		return nil, fmt.Errorf("%w: needs a topology", ErrEscalate)
	}
	w, err := WeightsFrom(o.Search.Utility)
	if err != nil {
		// A utility block that cannot rank is refused rather than defaulted:
		// silently substituting A.6's literals for a block the user wrote would
		// make the search score worlds by weights nobody chose.
		return nil, err
	}
	plan, err := Sweep(SweepOptions{
		Config:        o.Params.Config,
		Topology:      o.Topology,
		Seed:          o.Params.Streams.Seed(),
		AtMS:          o.ProbeAtMS,
		WindowMS:      o.ProbeWindowMS,
		KindSupported: o.KindSupported,
	})
	if err != nil {
		return nil, err
	}
	if o.MaxProbeWorlds > 0 && len(plan.Probes) > o.MaxProbeWorlds {
		plan.Probes = plan.Probes[:o.MaxProbeWorlds]
	}
	mut, err := search.NewMutator(o.Params.Config, o.Params.Space, o.Params.Validator)
	if err != nil {
		return nil, err
	}
	return &Saboteur{
		opts:    o,
		weights: w,
		observe: ObserveOptionsFrom(o.Search.Observe),
		mutator: mut,
		plan:    plan,
		pending: map[string]pendingRollout{},
		stage:   StageProbe,
	}, nil
}

// Name implements search.Strategy.
func (s *Saboteur) Name() schema.SearchStrategy { return schema.StrategySaboteur }

// Stage reports which sub-module is currently choosing.
func (s *Saboteur) Stage() StageName { return s.stage }

// Plan returns the Tier 1 sweep, including what it refused to generate.
func (s *Saboteur) Plan() SweepPlan { return s.plan }

// Signals returns A.3's ranked output once probing has ended.
func (s *Saboteur) Signals() []ProbeSignal { return append([]ProbeSignal(nil), s.signals...) }

// ProbeOutcomes returns the classified probe worlds.
func (s *Saboteur) ProbeOutcomes() []ProbeOutcome { return append([]ProbeOutcome(nil), s.outcomes...) }

// Baselines returns the control world's per-metric medians, or nil when the
// control world could not be evaluated. A nil result is a real answer: every
// comparative criterion downstream then reports Known=false rather than
// comparing against a number nobody measured.
func (s *Saboteur) Baselines() []Baseline { return append([]Baseline(nil), s.baselines...) }

// ControlEvaluable reports whether the no-fault control world produced a usable
// verdict.
//
// It is worth surfacing rather than keeping private: when it is FALSE every
// comparative criterion in A.3 reports Known=false, the classifier runs on the
// two non-comparative criteria alone, and it will under-report REINFORCE. A
// reader looking at a sweep full of DAMPEN needs to be able to tell "the system
// absorbed everything" from "the baseline was never measured".
func (s *Saboteur) ControlEvaluable() bool { return s.controlOK }

// EscalationError reports why Tier 2 was not activated, or nil.
//
// ErrNoReinforcingSignal is the ordinary case and is not a failure: A.5
// specifies falling back to stochastic corpus mutation for exactly that.
func (s *Saboteur) EscalationError() error { return s.escErr }

// EndProbeStage stops Tier 1 early, which is what A.5's "or first 20% of
// wall-clock budget, whichever is reached first" needs at run time.
func (s *Saboteur) EndProbeStage(reason string) {
	if s.probeDone {
		return
	}
	s.probeDone = true
	s.logf("saboteur: Tier 1 probe stage ended after %d of %d planned probes: %s",
		s.probeNext, len(s.plan.Probes), reason)
}

// TreeStats returns each escalation tree's measurement of itself, including the
// D-015 count of selections that backpropagation could actually have decided.
func (s *Saboteur) TreeStats() []TreeStats {
	out := append([]TreeStats(nil), s.treeStats...)
	if s.esc != nil {
		for _, t := range s.esc.Trees() {
			out = append(out, t.Stats())
		}
	}
	return out
}

// Propose implements search.Strategy.
func (s *Saboteur) Propose(ctx context.Context, ordinal int) (schema.World, error) {
	if err := ctx.Err(); err != nil {
		return schema.World{}, err
	}
	if ordinal < 0 {
		return schema.World{}, fmt.Errorf("saboteur: negative world ordinal %d", ordinal)
	}
	s.proposed++

	if !s.probeDone && s.probeNext < len(s.plan.Probes) {
		p := s.plan.Probes[s.probeNext]
		s.probeNext++
		if s.probeNext >= len(s.plan.Probes) {
			s.probeDone = true
		}
		w, err := s.world(search.OriginSaboteurProbe, p.Faults, ordinal)
		if err != nil {
			return schema.World{}, err
		}
		s.logf("saboteur: [probe %d/%d] %s", p.Index+1, len(s.plan.Probes), probeLabel(p))
		return w, nil
	}
	s.probeDone = true

	if w, ok, err := s.proposeEscalation(ordinal); err != nil {
		return schema.World{}, err
	} else if ok {
		return w, nil
	}
	return s.proposeMutation(ordinal)
}

// proposeEscalation hands out the next tree rollout, building the escalation on
// first use.
func (s *Saboteur) proposeEscalation(ordinal int) (schema.World, bool, error) {
	if !s.escBuilt {
		s.escBuilt = true
		s.signals = Rank(s.outcomes)
		s.reportSignals()
		esc, err := NewEscalation(EscalateOptions{
			Tree:     s.treeOptions(),
			Signals:  s.signals,
			MaxTrees: s.opts.MaxTrees,
		})
		if err != nil {
			s.escErr = err
			s.stage = StageMutate
			s.logf("saboteur: Tier 2 not activated: %v — falling back to corpus mutation, "+
				"which A.5 specifies for exactly this case", err)
			return schema.World{}, false, nil
		}
		s.esc = esc
		s.stage = StageEscalate
		for i, r := range esc.Roots() {
			s.logf("saboteur: Tier 2 tree %d rooted at %s(%s) [%s, rank %.1f] on node %s",
				i+1, r.Kind, r.Target, r.Class, r.Rank, r.Node)
		}
	}
	if s.esc == nil {
		return schema.World{}, false, nil
	}
	tree, node, err := s.esc.Next()
	if errors.Is(err, ErrExhaustedTree) {
		s.retireTrees()
		s.stage = StageMutate
		s.logf("saboteur: every Tier 2 tree is exhausted — falling back to corpus mutation")
		return schema.World{}, false, nil
	}
	if err != nil {
		return schema.World{}, false, err
	}
	w, err := s.world(search.OriginSaboteurMCTS, node.Faults, ordinal)
	if err != nil {
		node.Pending = false
		return schema.World{}, false, err
	}
	hash, err := w.Hash()
	if err != nil {
		return schema.World{}, false, err
	}
	s.pending[hash] = pendingRollout{tree: tree, node: node}
	s.stage = StageEscalate
	s.logf("saboteur: [mcts depth %d, %d fault(s)] %v", node.Depth, len(node.Faults), node.Faults)
	return w, true, nil
}

// proposeMutation is A.5's fallback and the terminal stage.
func (s *Saboteur) proposeMutation(ordinal int) (schema.World, error) {
	s.stage = StageMutate
	st := s.opts.Params.Streams.World(search.PathMutateDraw, ordinal)
	entry, err := s.opts.Params.Corpus.Select(s.opts.Params.Streams.World(search.PathCorpusSelect, ordinal))
	if err != nil {
		// An empty corpus means nothing observed has been admitted. Generating
		// uniformly is the honest continuation: it keeps the run producing
		// coverage rather than stalling, and it is exactly what the base spec's
		// own loop does before its corpus fills.
		planned, gerr := search.RandomSchedule(s.opts.Params.Space, s.opts.Params.Validator, st)
		if gerr != nil {
			return schema.World{}, gerr
		}
		return s.world(search.OriginSaboteurProbe, planned, ordinal)
	}
	child, op, err := s.mutator.Mutate(entry.World, st)
	if err != nil {
		return schema.World{}, err
	}
	child.Seed = WorldSeedFor(s.opts.Params.Streams.Seed(), ordinal)
	child = child.Normalized()
	if v := s.opts.Params.Validator; v != nil {
		if err := v.ValidateWorld(&child); err != nil {
			return schema.World{}, err
		}
	}
	s.logf("saboteur: [mutate %s] %v", op, child.FaultSchedule.Planned)
	return child, nil
}

// WorldSeedFor derives world `ordinal`'s seed from the run seed.
//
// It is recorder.WorldSeed and nothing else, which is the SAME derivation
// control.WorldSeedFor uses for the world it actually executes: with control's
// 1-based ordinal against this package's 0-based one. They must agree: the world
// the strategy hashes and the world the executor runs have to be the same
// document, or the corpus would archive a world that was never executed and the
// tree would backpropagate to a node whose hash nobody produced.
//
// The derivation is PATH-KEYED (D-013), so a world's seed is a function of
// (run seed, ordinal) alone, not of how many worlds ran before it and not of
// which worker picked it up, which is what makes a parallel search replay.
func WorldSeedFor(runSeed recorder.Seed, ordinal int) uint64 {
	if ordinal < 0 {
		ordinal = 0
	}
	return uint64(recorder.WorldSeed(runSeed, uint64(ordinal)))
}

// world builds a proposed world with provenance and a per-ordinal seed.
func (s *Saboteur) world(origin string, planned []string, ordinal int) (schema.World, error) {
	w := s.opts.Params.Base
	w.Seed = WorldSeedFor(s.opts.Params.Streams.Seed(), ordinal)
	if planned == nil {
		planned = []string{}
	}
	w.FaultSchedule.Planned = append([]string(nil), planned...)
	w.FaultSchedule.Realized = nil
	w.PhaseTimings = schema.PhaseTimings{}
	w.Meta = &schema.WorldMeta{Origin: origin}
	w = w.Normalized()
	if v := s.opts.Params.Validator; v != nil {
		if err := v.ValidateWorld(&w); err != nil {
			return schema.World{}, fmt.Errorf("saboteur: proposed world failed validation: %w", err)
		}
	}
	return w, nil
}

func (s *Saboteur) treeOptions() TreeOptions {
	return TreeOptions{
		Config:        s.opts.Params.Config,
		Topology:      s.opts.Topology,
		Seed:          s.opts.Params.Streams.Seed(),
		AtMS:          s.opts.EscalateAtMS,
		MaxDepth:      s.opts.Search.MaxMCTSDepth,
		MaxFaults:     s.opts.Params.Space.MaxFaults,
		Exploration:   s.opts.Search.ExplorationConstant,
		LeaseMS:       s.opts.LeaseMS,
		Timing:        s.opts.Timing,
		KindSupported: s.opts.KindSupported,
	}
}

func (s *Saboteur) retireTrees() {
	if s.esc == nil {
		return
	}
	for _, t := range s.esc.Trees() {
		s.treeStats = append(s.treeStats, t.Stats())
	}
	s.esc = nil
}

// Observe implements search.Strategy.
//
// It delegates to ObserveWorld with NO telemetry. That is not a shortcut: a
// caller that supplies no trajectory has given the classifier nothing to
// classify, and the honest result is INSUFFICIENT_DATA rather than DAMPEN.
func (s *Saboteur) Observe(ctx context.Context, out search.Outcome) error {
	return s.ObserveWorld(ctx, out, nil)
}

// ObserveWorld is the wide seam: it takes the per-node telemetry that
// search.Outcome does not carry, which is what A.4's classifier consumes.
//
// The engine calls this; Observe exists so the Saboteur still satisfies the
// narrow search.Strategy interface that D-029's benchmark is written against.
func (s *Saboteur) ObserveWorld(ctx context.Context, out search.Outcome, obs []Observation) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.observed++

	// Tier 2 backpropagation, if this world came from a tree.
	if pr, ok := s.pending[out.WorldHash]; ok {
		delete(s.pending, out.WorldHash)
		pr.tree.Backpropagate(pr.node, s.rolloutResult(out))
	}

	// Tier 1 classification, if this world was a probe.
	idx := s.probeIndexFor(out)
	if idx < 0 {
		return nil
	}
	p := s.plan.Probes[idx]
	evaluable := out.Signal() != search.SignalUnknown

	if p.Control {
		s.controlOK = evaluable
		if evaluable {
			s.baselines = baselinesFromObservations(obs)
			s.logf("saboteur: control world evaluable (%s); %d baseline metric(s) recorded",
				out.Why(), len(s.baselines))
		} else {
			// Stated rather than hidden. Without a baseline every comparative
			// criterion in A.3 reports Known=false, so the classifier runs on
			// the two non-comparative criteria alone and will under-report
			// REINFORCE. That is a weaker sweep, not a wrong one.
			s.logf("saboteur: control world NOT evaluable (%s) — every comparative A.3 criterion "+
				"will report unknown rather than false for the rest of this sweep", out.Why())
		}
		return nil
	}

	po := ProbeOutcome{
		Probe:     p,
		Utility:   Utility(worldResultOf(out), s.weights),
		Evaluable: evaluable,
		Why:       out.Why(),
	}
	for _, o := range obs {
		if len(s.baselines) > 0 && len(o.Baselines) == 0 {
			o.Baselines = s.baselines
		}
		po.Nodes = append(po.Nodes, Classify(o, s.probeOptionsFor(o)))
	}
	sort.SliceStable(po.Nodes, func(i, j int) bool { return po.Nodes[i].Node < po.Nodes[j].Node })
	s.outcomes = append(s.outcomes, po)
	s.logf("saboteur: probe %s -> %s (rank %.1f, utility %.1f)%s",
		probeLabel(p), po.Class(), po.Rank(), po.Utility, evaluableNote(evaluable, out))
	return nil
}

// probeOptionsFor is the classifier's tuning for one observation.
//
// A.3 asks for a 200 ms trajectory during a probe world, and ForProbe encodes
// that. But the classifier's window-stretch guard compares a window's REAL span
// against `SampleIntervalMS * (W-1)`, so telling it 200 when the harness sampled
// at 500 makes every seven-sample window look like it straddles a blackout. The
// OBSERVED rate therefore wins over the requested one whenever the caller knows
// it. A.3's request is a statement about what the harness SHOULD do; this is a
// measurement of what it DID.
func (s *Saboteur) probeOptionsFor(o Observation) ObserveOptions {
	return ProbeOptionsFor(s.observe, o)
}

// ProbeOptionsFor applies an observation's MEASURED sampling rate over a base
// tuning's requested one.
//
// Exported because it is the whole of D-047 and a test that could not reach it
// would be testing the arithmetic instead of the premise.
func ProbeOptionsFor(base ObserveOptions, o Observation) ObserveOptions {
	opt := base.ForProbe()
	if o.SampleIntervalMS > 0 {
		opt.SampleIntervalMS = int(o.SampleIntervalMS)
	}
	return opt
}

func evaluableNote(ok bool, out search.Outcome) string {
	if ok {
		return ""
	}
	return " [UNEVALUABLE: " + out.Why() + "]"
}

// probeIndexFor matches an outcome back to the probe that produced it, by
// planned schedule.
//
// By SCHEDULE rather than by arrival order: the executor completes worlds out of
// order, and matching by position would attribute one probe's telemetry to
// another's fault; the quietest possible way to corrupt a ranking.
func (s *Saboteur) probeIndexFor(out search.Outcome) int {
	for i, p := range s.plan.Probes {
		if i >= s.probeNext {
			break
		}
		if sameSchedule(p.Faults, out.Faults) {
			return i
		}
	}
	return -1
}

func sameSchedule(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	x := append([]string(nil), a...)
	y := append([]string(nil), b...)
	sort.Strings(x)
	sort.Strings(y)
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}

// rolloutResult converts an executed world into the tree's evidence.
func (s *Saboteur) rolloutResult(out search.Outcome) RolloutResult {
	return RolloutResult{
		Utility:      Utility(worldResultOf(out), s.weights),
		NewTemplates: out.Coverage.NewTemplates,
		NewStates:    out.Coverage.NewStates,
		Violations:   len(out.Violations()),
		Evaluable:    out.Signal() != search.SignalUnknown,
	}
}

// worldResultOf projects a search.Outcome onto A.6's inputs.
//
// An INDEFINITE finding contributes nothing: it is neither a violation to
// reward nor a clean result to trust. The coverage and parsimony terms still
// apply, because they are measurements of the world rather than claims about
// the system.
func worldResultOf(out search.Outcome) WorldResult {
	r := WorldResult{
		Coverage: schema.Coverage{
			NewTemplates: out.Coverage.NewTemplates,
			NewStates:    out.Coverage.NewStates,
		},
		FaultCount:      out.FaultCount(),
		DurationSeconds: out.DurationSeconds(),
	}
	for _, f := range out.Violations() {
		r.Violations = append(r.Violations, schema.Violation{
			Oracle:   f.Oracle,
			Class:    f.Class,
			Severity: f.Severity,
			Phase:    f.Phase,
		})
	}
	return r
}

// baselinesFromObservations folds every node's control series into one baseline
// per metric. Metrics are keyed by name and the result is metric-ordered, so it
// does not depend on the order the nodes were collected in.
func baselinesFromObservations(obs []Observation) []Baseline {
	byMetric := map[string][]Point{}
	metrics := []string{}
	for _, o := range obs {
		for _, s := range o.Metrics {
			if _, seen := byMetric[s.Metric]; !seen {
				metrics = append(metrics, s.Metric)
			}
			byMetric[s.Metric] = append(byMetric[s.Metric], s.Points...)
		}
	}
	sort.Strings(metrics)
	out := make([]Baseline, 0, len(metrics))
	for _, m := range metrics {
		out = append(out, BaselineFrom(Series{Metric: m, Points: byMetric[m]}))
	}
	return out
}

func (s *Saboteur) reportSignals() {
	s.logf("saboteur: Tier 1 complete: %d probe(s) classified, %d skipped",
		len(s.outcomes), len(s.plan.Skipped))
	for i, sig := range s.signals {
		if i >= 8 {
			s.logf("saboteur:   ... and %d more", len(s.signals)-i)
			break
		}
		s.logf("saboteur:   %d. %s(%s) node=%s %s rank=%.1f%s",
			i+1, sig.Kind, sig.Target, sig.Node, sig.Class, sig.Rank,
			map[bool]string{true: "", false: " [unevaluable]"}[sig.Evaluable])
	}
}

func probeLabel(p Probe) string {
	if p.Control {
		return "control (no faults)"
	}
	return fmt.Sprintf("%s(%s)", p.Kind, p.Target)
}

func (s *Saboteur) logf(format string, args ...any) {
	if s.opts.Log != nil {
		s.opts.Log(format, args...)
	}
}

// Proposed and Observations are diagnostics, matching the random baseline's.
func (s *Saboteur) Proposed() int     { return s.proposed }
func (s *Saboteur) Observations() int { return s.observed }

// Deadline is a convenience for a caller enforcing A.5's wall-clock probe
// budget: it reports whether the probe stage should end now.
func ProbeStageExpired(started time.Time, split BudgetSplit, worldsRun int) (bool, string) {
	if split.ProbeWorlds >= 0 && worldsRun >= split.ProbeWorlds {
		return true, fmt.Sprintf("the Tier 1 world budget (%d) is spent", split.ProbeWorlds)
	}
	if split.ProbeWall > 0 && time.Since(started) >= split.ProbeWall {
		return true, fmt.Sprintf("the Tier 1 wall budget (%s) is spent", split.ProbeWall.Round(time.Second))
	}
	return false, ""
}

var _ search.Strategy = (*Saboteur)(nil)
