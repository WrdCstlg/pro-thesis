package saboteur

import (
	"errors"
	"fmt"
	"sort"

	"github.com/WrdCstlg/pro-thesis/internal/perturber"
	"github.com/WrdCstlg/pro-thesis/internal/perturber/faults"
	"github.com/WrdCstlg/pro-thesis/internal/recorder"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// ---------------------------------------------------------------------------
// A.3: Tier 1, cheap reconnaissance
//
// The sweep characterises the failure surface with single-fault, short-window,
// minimal-magnitude worlds, one per (kind, target), and hands OBSERVE the
// trajectory each produced. Its output is A.3's ranked list of
// (fault_kind, target, observed_signal, classification).
//
// # The no-fault control is part of the sweep, and its absence is a defect
//
// A.3 lists only fault probes. Every one of its four DAMPEN/REINFORCE criteria
// is COMPARATIVE ("metrics returned to baseline", "retry count exceeded 2x
// baseline", "error rate increased after fault withdrawal") so a sweep with no
// control has no baseline and every criterion is unevaluable. DECISIONS.md
// D-028 item 6 records the reading: the control is restored, and it is FIRST, so
// a run that exhausts its budget early has still measured it. CRUCIBLE §G's seed
// corpus says the same thing independently.
//
// # Why probes are genuinely cheaper here, measured rather than assumed
//
// PERTURB blocks until the last withdrawal, so a world costs roughly what its
// schedule SPANS plus the fixed lifecycle. A 2000 ms probe window therefore
// costs materially less than a 5300 ms escalation window, which is the only
// sense in which A.3's "cost: ~5-15s per probe" is achievable on this stack.
// OQ-013 measured a probe world at ~19.4 s against that claim; the saving is
// real but it is not the order of magnitude A.3 implies, and nothing here
// pretends otherwise.
//
// # Why the probe window is 2000 ms and not A.3's lower bound
//
// A.3 offers 500-2000 ms. 500 ms is SHORTER THAN A RAFT ELECTION on this
// fixture (D-031 measured the election completing ~1.2 s into a pause) so a
// 500 ms gray-failure probe cannot produce the role change, the queue growth or
// the post-withdrawal error rate the sweep exists to detect. It would classify
// DAMPEN on a system that reinforces, which is the ranked #1 failure mode
// arriving through a default. The upper bound is used, and the reason is
// recorded here rather than left as a number.
// ---------------------------------------------------------------------------

// ErrSweep reports a sweep that cannot be planned.
var ErrSweep = errors.New("saboteur: probe sweep")

const (
	// DefaultProbeAtMS is where a probe's window opens, DRIVE-relative. Late
	// enough that SEED has proved steady state and the driver has ramped, early
	// enough that the world stays cheap.
	DefaultProbeAtMS int64 = 3000
	// DefaultProbeWindowMS is A.3's upper bound. See the note above.
	DefaultProbeWindowMS int64 = 2000
)

// ProbeControlLabel names the no-fault control world.
const ProbeControlLabel = "control"

// Probe is one micro-probe world.
type Probe struct {
	// Index is the probe's position in the sweep. 0 is always the control.
	Index int
	// Control marks the no-fault world.
	Control bool
	// Kind and Target are the (fault_kind, target) half of A.3's output tuple.
	// Both are empty on the control.
	Kind   schema.FaultKind
	Target string
	// Faults is the world's planned schedule: exactly one fault, or none.
	Faults []string
	// Label is a short human tag carried into the world outcome.
	Label string
}

// SweepPlan is the planned Tier 1 sweep.
type SweepPlan struct {
	Probes []Probe
	// Skipped records every (kind, target) the sweep did NOT generate and why.
	// A silently short sweep looks exactly like a sweep that found nothing, and
	// those are different facts.
	Skipped []Skip
}

// Fault probes are the plan minus the control.
func (p SweepPlan) FaultProbes() []Probe {
	out := make([]Probe, 0, len(p.Probes))
	for _, pr := range p.Probes {
		if !pr.Control {
			out = append(out, pr)
		}
	}
	return out
}

// SweepOptions is the input to Sweep.
type SweepOptions struct {
	// Config and Topology carry the same meaning as in LadderOptions.
	Config   *schema.Config
	Topology *perturber.Topology
	// Seed is the RUN seed. Target order within a kind is drawn from it by
	// path, exactly as the ladder does, so a ten-trial benchmark does not
	// systematically probe kv-n1 first.
	Seed recorder.Seed
	// AtMS and WindowMS bound every probe window. Zero means the defaults.
	AtMS     int64
	WindowMS int64
	// KindSupported is the platform capability check. nil means
	// faults.PlatformCapability.
	KindSupported func(schema.FaultKind) error
	// SkipRoleTargets drops `role:` targets from the sweep.
	//
	// They are INCLUDED by default, which is an addition to A.3's literal "on
	// each node individually". The reason is A.9 #1: the definition of done asks
	// the sweep to classify `proc.pause` ON THE LEADER, and `role:leader` is the
	// only form in the frozen grammar that names the leader; it binds to a
	// concrete node at INJECTION time from live state (D-012). Probing node ids
	// alone would hit the leader only by luck, and would attribute the signal to
	// whichever id happened to hold the role in that world.
	SkipRoleTargets bool
}

func (o *SweepOptions) fill() {
	if o.AtMS <= 0 {
		o.AtMS = DefaultProbeAtMS
	}
	if o.WindowMS <= 0 {
		o.WindowMS = DefaultProbeWindowMS
	}
	if o.KindSupported == nil {
		o.KindSupported = faults.PlatformCapability
	}
}

// minimalMagnitude is A.3's "minimal magnitude" per kind, as the rendered
// parameter suffix.
//
// The values are rung 0 / rung 1 of the escalation ladder, so a probe and the
// escalation that follows it speak the same magnitudes and a REINFORCE signal
// measured at a probe magnitude is not attributed to a fault ten times larger.
// A kind absent from the table has no tunable magnitude and probes at its only
// one.
func minimalMagnitude(k schema.FaultKind) string {
	switch k {
	case schema.FaultNetLatency:
		return ", mean=50, jitter=10"
	case schema.FaultNetLoss:
		return ", pct=1"
	case schema.FaultNetReorder, schema.FaultNetDuplicate:
		return ", pct=1"
	case schema.FaultIOLatency:
		return ", ms=50"
	case schema.FaultProcKill:
		// SIGTERM rather than SIGKILL: a probe is reconnaissance, and the
		// graceful signal is the smaller of the two magnitudes the ladder
		// carries for this kind.
		return ", signal=SIGTERM"
	case schema.FaultProcSlow:
		return ", cpu_pct=50"
	case schema.FaultMemPressure:
		return ", pct=10"
	case schema.FaultNetBandwidth:
		return ", bps=10000000"
	case schema.FaultIOFill:
		return ", pct=10"
	default:
		return ""
	}
}

// probeStreamPath is the sweep's own path-keyed randomness root. It is distinct
// from the ladder's, so a draw here can never shift a ladder ordering (D-013).
const probeStreamPath = "search.saboteur.probe"

// ProbeStream returns the sweep's tie-break stream for one fault kind.
func ProbeStream(seed recorder.Seed, k schema.FaultKind) *recorder.Stream {
	return recorder.NewStream(recorder.MustDeriveKey(seed, probeStreamPath, string(k)))
}

// Sweep plans the Tier 1 probe sweep.
//
// Every generated fault is parsed by pkg/schema AND compiled by
// perturber.Compile against the run's allow list, deny list, budget,
// constraints and topology: the same function the executor runs. A probe the
// executor would refuse is recorded in Skipped rather than emitted, because a
// refused probe costs a whole world to discover mid-DRIVE and teaches nothing.
func Sweep(opts SweepOptions) (SweepPlan, error) {
	if opts.Config == nil {
		return SweepPlan{}, fmt.Errorf("%w: needs a config", ErrSweep)
	}
	if opts.Topology == nil {
		return SweepPlan{}, fmt.Errorf("%w: needs a topology; a probe whose target matches no node "+
			"injects nothing while still reporting as perturbed", ErrSweep)
	}
	opts.fill()

	plan := SweepPlan{}
	plan.Probes = append(plan.Probes, Probe{Index: 0, Control: true, Label: ProbeControlLabel})

	kinds := opts.Config.Perturber.EffectiveFaultKinds()
	sorted := append([]schema.FaultKind(nil), kinds...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	end := opts.AtMS + opts.WindowMS
	idx := 1
	for _, k := range sorted {
		if err := opts.KindSupported(k); err != nil {
			plan.Skipped = append(plan.Skipped, Skip{Rung: RungRecon, What: string(k), Reason: err.Error()})
			continue
		}
		for _, target := range probeTargets(opts, k) {
			text := fmt.Sprintf("%s(%s%s)@%d..%d", k, target, minimalMagnitude(k), opts.AtMS, end)
			canon, err := schema.CanonicalFault(text)
			if err != nil {
				plan.Skipped = append(plan.Skipped, Skip{
					Rung: RungRecon, What: text, Reason: err.Error(),
				})
				continue
			}
			if _, err := perturber.Compile(perturber.CompileOptions{
				Config:   opts.Config,
				Topology: opts.Topology,
				Planned:  []string{canon},
			}); err != nil {
				plan.Skipped = append(plan.Skipped, Skip{
					Rung: RungRecon, What: canon, Reason: err.Error(),
				})
				continue
			}
			plan.Probes = append(plan.Probes, Probe{
				Index:  idx,
				Kind:   k,
				Target: target,
				Faults: []string{canon},
				Label:  fmt.Sprintf("probe/%s/%s", k, target),
			})
			idx++
		}
	}
	if len(plan.Probes) == 1 {
		return plan, fmt.Errorf("%w: no permitted fault kind produced a schedulable probe; the sweep "+
			"would be the control world alone and could classify nothing", ErrSweep)
	}
	return plan, nil
}

// probeTargets is the target order for one kind: role:leader first, then the
// node ids in an order drawn from this kind's own path-keyed stream.
func probeTargets(opts SweepOptions, k schema.FaultKind) []string {
	nodes := opts.Topology.Nodes()
	ids := make([]string, 0, len(nodes))
	for _, n := range nodes {
		ids = append(ids, n.ID)
	}
	sort.Strings(ids)
	st := ProbeStream(opts.Seed, k)
	st.Shuffle(len(ids), func(i, j int) { ids[i], ids[j] = ids[j], ids[i] })
	if opts.SkipRoleTargets {
		return ids
	}
	return append([]string{roleLeader}, ids...)
}

// ---------------------------------------------------------------------------
// classification and ranking
// ---------------------------------------------------------------------------

// ProbeOutcome is one executed probe world, classified.
type ProbeOutcome struct {
	Probe Probe
	// Nodes is OBSERVE's answer per node, in node order.
	Nodes []Result
	// Utility is A.6's payoff for the world, so the ranking and the tree agree
	// about what a world was worth.
	Utility float64
	// Evaluable reports whether the world produced a usable verdict. An
	// unevaluable world is NOT evidence that the fault was absorbed.
	Evaluable bool
	// Why explains the evaluability decision in one line.
	Why string
}

// Class is the strongest classification any node reported.
//
// Strongest, not consensus: A.4 says "a REINFORCE_ACCELERATING classification on
// ANY metric triggers immediate escalation", and the same reading applies across
// nodes. A cluster in which one node is spiralling is a spiralling cluster.
func (o ProbeOutcome) Class() SpiralClass {
	best := SpiralInsufficientData
	seen := false
	for _, r := range o.Nodes {
		if !seen || r.Class > best {
			best, seen = r.Class, true
		}
	}
	if !seen {
		return SpiralInsufficientData
	}
	return best
}

// Escalate reports A.4's Tier 2 trigger for this probe.
func (o ProbeOutcome) Escalate() bool {
	for _, r := range o.Nodes {
		if r.Escalate() {
			return true
		}
	}
	return false
}

// Rank is the reinforcing signal strength A.3 orders its output by. It is the
// strongest per-node rank, so a cluster is ranked by its worst node.
func (o ProbeOutcome) Rank() float64 {
	best := 0.0
	for i, r := range o.Nodes {
		if i == 0 || r.Rank > best {
			best = r.Rank
		}
	}
	return best
}

// SignalNode is the node that produced the outcome's strongest signal, or "".
func (o ProbeOutcome) SignalNode() string {
	best := -1
	for i, r := range o.Nodes {
		if best < 0 || r.Rank > o.Nodes[best].Rank {
			best = i
		}
	}
	if best < 0 {
		return ""
	}
	return o.Nodes[best].Node
}

// ProbeSignal is one entry of A.3's ranked output:
// (fault_kind, target, observed_signal, classification).
type ProbeSignal struct {
	Kind   schema.FaultKind
	Target string
	// Node is the node whose telemetry carried the strongest signal.
	Node string
	// Class is the classification. INSUFFICIENT_DATA is a real value here and
	// is never folded into DAMPEN.
	Class SpiralClass
	// Rank is the reinforcing signal strength.
	Rank float64
	// Utility is A.6's payoff for the probe world.
	Utility float64
	// Evaluable is false when the world could not be judged at all.
	Evaluable bool
	// Reason is the classifier's own explanation.
	Reason string
	// Escalate is A.4's Tier 2 trigger.
	Escalate bool
}

// Rank orders probe outcomes by reinforcing signal strength, strongest first.
//
// The ordering is total and deterministic: class rank, then signal rank, then
// utility, then the probe's own sweep index. Nothing here reads a map and
// nothing depends on the order the worlds happened to finish in: a parallel
// executor completes them out of order and a ranking that inherited that would
// not replay (A.8).
//
// UNEVALUABLE probes are sorted to the BOTTOM regardless of any number they
// carry, and they keep their INSUFFICIENT_DATA label. A world whose oracles
// could not answer has told the search nothing, and letting it rank as though
// it had is the ranked #1 way this phase fails.
func Rank(outs []ProbeOutcome) []ProbeSignal {
	type keyed struct {
		s   ProbeSignal
		idx int
	}
	ks := make([]keyed, 0, len(outs))
	for _, o := range outs {
		if o.Probe.Control {
			continue
		}
		node := o.SignalNode()
		reason := ""
		for _, r := range o.Nodes {
			if r.Node == node {
				reason = r.Reason
				break
			}
		}
		class := o.Class()
		rank := o.Rank()
		if !o.Evaluable {
			class = SpiralInsufficientData
			rank = 0
			if o.Why != "" {
				reason = o.Why
			}
		}
		ks = append(ks, keyed{
			s: ProbeSignal{
				Kind:      o.Probe.Kind,
				Target:    o.Probe.Target,
				Node:      node,
				Class:     class,
				Rank:      rank,
				Utility:   o.Utility,
				Evaluable: o.Evaluable,
				Reason:    reason,
				Escalate:  o.Evaluable && o.Escalate(),
			},
			idx: o.Probe.Index,
		})
	}
	sort.SliceStable(ks, func(i, j int) bool {
		a, b := ks[i].s, ks[j].s
		if a.Evaluable != b.Evaluable {
			return a.Evaluable
		}
		if a.Class != b.Class {
			return a.Class > b.Class
		}
		if a.Rank != b.Rank {
			return a.Rank > b.Rank
		}
		if a.Utility != b.Utility {
			return a.Utility > b.Utility
		}
		return ks[i].idx < ks[j].idx
	})
	out := make([]ProbeSignal, 0, len(ks))
	for _, k := range ks {
		out = append(out, k.s)
	}
	return out
}

// TopReinforcing returns the ranked signals that actually reinforce, which is
// the set A.5 roots its trees at. A DAMPEN signal is never escalated: A.5 says
// Tier 2 is "activated only when OBSERVE classifies REINFORCE".
func TopReinforcing(sigs []ProbeSignal, n int) []ProbeSignal {
	out := make([]ProbeSignal, 0, len(sigs))
	for _, s := range sigs {
		if !s.Evaluable || !s.Class.Reinforcing() {
			continue
		}
		out = append(out, s)
		if n > 0 && len(out) >= n {
			break
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// baselines
// ---------------------------------------------------------------------------

// BaselineFrom summarises a control-world series as a comparative baseline.
//
// The statistic is the MEDIAN, not the mean: a single scheduling hiccup in a
// control world would drag a mean upward and make every comparative criterion
// harder to satisfy, which biases the classifier toward DAMPEN; the direction
// that loses signal.
//
// A series with no observed points yields Known=false. It never yields 0, which
// would make "we never measured this" indistinguishable from "it was empty" and
// would turn every comparison against it into a claim nobody made.
func BaselineFrom(s Series) Baseline {
	if len(s.Points) == 0 {
		return Baseline{Metric: s.Metric, Known: false}
	}
	vals := make([]float64, 0, len(s.Points))
	for _, p := range s.Points {
		vals = append(vals, p.Value)
	}
	sort.Float64s(vals)
	mid := len(vals) / 2
	v := vals[mid]
	if len(vals)%2 == 0 {
		v = (vals[mid-1] + vals[mid]) / 2
	}
	return Baseline{Metric: s.Metric, Value: v, Known: true}
}

// BaselinesFrom summarises several control series, one baseline per metric,
// ordered by metric name so the result does not depend on collection order.
func BaselinesFrom(series []Series) []Baseline {
	out := make([]Baseline, 0, len(series))
	for _, s := range series {
		out = append(out, BaselineFrom(s))
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Metric < out[j].Metric })
	return out
}
