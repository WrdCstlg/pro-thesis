package oracle

import (
	"context"
	"fmt"
	"strings"

	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// MaxWitnessSamples bounds how many telemetry points a witness quotes.
const MaxWitnessSamples = 32

// NoUnboundedQueue is the `no_unbounded_queue` built-in: no queue depth grows
// monotonically across QUIESCE.
//
// QUIESCE is where this is meaningful and nowhere else. The workload has
// stopped, the faults have been withdrawn, and the system is being given its
// convergence grace window, so a queue that is still climbing is not busy, it
// is not draining. schema.BuiltinOracle declares the oracle valid in QUIESCE
// and ASSERT for exactly that reason; during DRIVE a rising queue is just load.
//
// "Monotonically increasing" is read STRICTLY: a run of consecutive samples
// each greater than the one before, at least MinSamples long. A flat queue is
// not growing, and a queue that ticks up once inside a flat run is noise. The
// strict reading is what keeps this oracle from crying wolf, and an oracle that
// cries wolf is one an operator learns to ignore.
type NoUnboundedQueue struct {
	builtinDecl
	opts NoUnboundedQueueOptions
}

// NewNoUnboundedQueue builds the oracle.
func NewNoUnboundedQueue(opts NoUnboundedQueueOptions) *NoUnboundedQueue {
	return &NoUnboundedQueue{
		builtinDecl: builtinDecl{id: schema.BuiltinNoUnboundedQueue},
		opts:        opts.withDefaults(),
	}
}

// samplePoint is a witness-shaped telemetry point.
type samplePoint struct {
	TMS   int64 `json:"t_ms"`
	Value int64 `json:"value"`
}

// Evaluate implements Oracle.
func (o *NoUnboundedQueue) Evaluate(_ context.Context, _ schema.Phase, in *Input) (Result, error) {
	if in == nil || len(in.Nodes) == 0 {
		return Inconclusive("no node telemetry was collected, so no queue depth could be checked"), nil
	}
	quiesce, ok := in.Window(schema.PhaseQuiesce)
	if !ok {
		return Inconclusive("the run recorded no QUIESCE window, so there is no interval across which " +
			"a queue could be shown to grow (queue depth during DRIVE is load, not a leak)"), nil
	}
	if quiesce.DurationMS() <= 0 {
		return Inconclusive("the QUIESCE window is %dms long, which cannot hold the %d samples this "+
			"oracle needs", quiesce.DurationMS(), o.opts.MinSamples), nil
	}

	type finding struct {
		node    string
		metric  string
		run     []Sample
		samples []Sample
	}
	var (
		findings []finding
		gaps     []string
		checked  int
	)

	for _, n := range in.Nodes {
		metric, series, has := o.pick(n)
		if !has {
			gaps = append(gaps, fmt.Sprintf("%s carries none of the queue-depth metrics %s",
				n.NodeID, strings.Join(o.opts.Metrics, ", ")))
			continue
		}
		samples := series.InWindow(quiesce)
		if len(samples) < o.opts.MinSamples {
			gaps = append(gaps, fmt.Sprintf("%s has %d %s sample(s) inside QUIESCE, fewer than the %d "+
				"needed to call a rise monotonic", n.NodeID, len(samples), metric, o.opts.MinSamples))
			continue
		}
		checked++
		if run := longestStrictlyRisingRun(samples); len(run) >= o.opts.MinSamples {
			findings = append(findings, finding{node: n.NodeID, metric: metric, run: run, samples: samples})
		}
	}

	if len(findings) > 0 {
		f := findings[0]
		details := make([]string, 0, len(findings))
		nodes := make([]string, 0, len(findings))
		for _, x := range findings {
			nodes = append(nodes, x.node)
			details = append(details, fmt.Sprintf("%s %s rose %d -> %d over %d consecutive samples "+
				"(t+%dms..t+%dms)", x.node, x.metric,
				x.run[0].Value, x.run[len(x.run)-1].Value, len(x.run),
				x.run[0].TMS, x.run[len(x.run)-1].TMS))
		}
		w := NewWitness().
			Set("node", f.node).
			Set("metric", f.metric).
			Set("rising_samples", witnessSamples(f.run)).
			Set("quiesce_samples", witnessSamples(f.samples)).
			Set("quiesce_window_ms", []int64{quiesce.StartMS, quiesce.EndMS}).
			Set("nodes", sortedStrings(nodes))
		return Violated(f.run[0].TMS, w.Build(),
			"queue depth grew monotonically across QUIESCE, after the workload stopped and all faults "+
				"were withdrawn: %s", strings.Join(details, "; ")), nil
	}

	if checked == 0 {
		return Inconclusive("no queue-depth metric could be evaluated across QUIESCE: %s",
			strings.Join(sortedStrings(gaps), "; ")), nil
	}
	if len(gaps) > 0 {
		return Inconclusive("%d node(s) were checked, but %s", checked, strings.Join(sortedStrings(gaps), "; ")), nil
	}
	return OK("queue depth did not rise monotonically across QUIESCE on any of %d node(s)", checked), nil
}

// pick returns the first configured queue metric the node actually carries.
func (o *NoUnboundedQueue) pick(n NodeObservation) (string, Series, bool) {
	for _, m := range o.opts.Metrics {
		if s, ok := n.Series(m); ok && len(s.Points) > 0 {
			return m, s, true
		}
	}
	return "", Series{}, false
}

// longestStrictlyRisingRun returns the longest run of consecutive samples in
// which each value is strictly greater than its predecessor.
func longestStrictlyRisingRun(s []Sample) []Sample {
	var best, cur []Sample
	for i, p := range s {
		if i == 0 || p.Value <= s[i-1].Value {
			cur = []Sample{p}
		} else {
			cur = append(cur, p)
		}
		if len(cur) > len(best) {
			best = append([]Sample(nil), cur...)
		}
	}
	return best
}

// witnessSamples renders samples for a witness, clipped to MaxWitnessSamples.
func witnessSamples(s []Sample) []samplePoint {
	out := make([]samplePoint, 0, len(s))
	for i, p := range s {
		if i >= MaxWitnessSamples {
			break
		}
		out = append(out, samplePoint{TMS: p.TMS, Value: p.Value})
	}
	return out
}
