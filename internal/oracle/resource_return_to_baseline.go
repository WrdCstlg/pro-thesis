package oracle

import (
	"context"
	"fmt"
	"strings"

	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// ResourceReturnToBaseline is the `resource_return_to_baseline` built-in: after
// the workload has stopped and the convergence grace window has elapsed, RSS,
// file descriptors and goroutine/thread counts are back within a band around
// their pre-DRIVE baseline.
//
// The band is deliberately wide (DefaultResourceTolerancePct, plus an absolute
// slack per metric) and BOTH must be exceeded. This is a smoke-level leak
// detector reading a handful of samples from a process that has just been
// through fault injection; a tight bound would fire on ordinary allocator
// behaviour, and a resource oracle that fires on healthy runs teaches its
// operator to ignore it, which is worse than not shipping one.
//
// Two things it will NOT do:
//
//   - It does not fabricate a baseline. No pre-DRIVE value means INCONCLUSIVE.
//   - It does not accept a page-cache-inclusive memory figure as RSS. See
//     MetricRSSBytes: a cgroup's memory.current grows with log volume and is
//     reclaimed on demand, so feeding it in would make every log-heavy run look
//     like a leak.
type ResourceReturnToBaseline struct {
	builtinDecl
	opts ResourceOptions
}

// NewResourceReturnToBaseline builds the oracle.
func NewResourceReturnToBaseline(opts ResourceOptions) *ResourceReturnToBaseline {
	return &ResourceReturnToBaseline{
		builtinDecl: builtinDecl{id: schema.BuiltinResourceReturnToBaseline},
		opts:        opts.withDefaults(),
	}
}

// resourceFinding is one node/metric that did not come back.
type resourceFinding struct {
	node     string
	metric   string
	baseline int64
	post     int64
	postTMS  int64
	allowed  int64
}

func (f resourceFinding) String() string {
	return fmt.Sprintf("%s %s went from %d to %d at t+%dms, %+d over a baseline of %d (allowed %+d)",
		f.node, f.metric, f.baseline, f.post, f.postTMS, f.post-f.baseline, f.baseline, f.allowed)
}

// Evaluate implements Oracle.
func (o *ResourceReturnToBaseline) Evaluate(_ context.Context, _ schema.Phase, in *Input) (Result, error) {
	if in == nil || len(in.Nodes) == 0 {
		return Inconclusive("no node telemetry was collected, so no resource could be compared against " +
			"its pre-DRIVE baseline"), nil
	}
	quiesce, ok := in.Window(schema.PhaseQuiesce)
	if !ok {
		return Inconclusive("the run recorded no QUIESCE window, so there is no post-convergence point " +
			"at which a resource could be expected to have returned to baseline"), nil
	}

	var (
		findings []resourceFinding
		gaps     []string
		// partial names, per checked node, the configured metrics that could
		// NOT be evaluated. They are carried into the OK text: a node checked
		// on goroutines alone did not have its RSS judged, and "every resource
		// metric returned to baseline" would assert a measurement never made.
		partial []string
		checked int
	)

	for _, n := range in.Nodes {
		evaluated := 0
		var missing []string
		for _, metric := range o.opts.Metrics {
			series, has := n.Series(metric)
			if !has || len(series.Points) == 0 {
				missing = append(missing, metric+": no samples")
				continue
			}
			baseline, hasBase := n.BaselineFor(metric)
			if !hasBase {
				missing = append(missing, metric+": no pre-DRIVE baseline")
				continue
			}
			post, hasPost := series.LastFrom(quiesce.StartMS)
			if !hasPost {
				missing = append(missing, metric+": no sample at or after QUIESCE start")
				continue
			}
			evaluated++
			allowed := o.allowance(metric, baseline)
			if post.Value-baseline > allowed {
				findings = append(findings, resourceFinding{
					node:     n.NodeID,
					metric:   metric,
					baseline: baseline,
					post:     post.Value,
					postTMS:  post.TMS,
					allowed:  allowed,
				})
			}
		}
		if evaluated == 0 {
			gaps = append(gaps, fmt.Sprintf("%s: %s", n.NodeID, strings.Join(missing, ", ")))
			continue
		}
		checked++
		if len(missing) > 0 {
			partial = append(partial, fmt.Sprintf("%s: %s", n.NodeID, strings.Join(missing, ", ")))
		}
	}

	if len(findings) > 0 {
		f := findings[0]
		details := make([]string, 0, len(findings))
		for _, x := range findings {
			details = append(details, x.String())
		}
		w := NewWitness().
			Set("node", f.node).
			Set("metric", f.metric).
			Set("baseline", f.baseline).
			Set("observed", f.post).
			Set("observed_at_ms", f.postTMS).
			Set("allowed_excess", f.allowed).
			Set("tolerance_pct", o.opts.TolerancePct).
			Set("findings", details)
		return Violated(f.postTMS, w.Build(),
			"%d resource(s) did not return to within %d%% of their pre-DRIVE baseline after QUIESCE: %s",
			len(findings), o.opts.TolerancePct, strings.Join(details, "; ")), nil
	}

	if checked == 0 {
		return Inconclusive("no resource metric could be compared against a baseline: %s",
			strings.Join(sortedStrings(gaps), "; ")), nil
	}
	if len(gaps) > 0 {
		return Inconclusive("%d node(s) were checked, but no metric was evaluable on %s",
			checked, strings.Join(sortedStrings(gaps), "; ")), nil
	}
	if len(partial) > 0 {
		return OK("%d node(s) returned to within %d%% of their pre-DRIVE baseline on every metric that "+
			"could be evaluated; NOT evaluated: %s",
			checked, o.opts.TolerancePct, strings.Join(sortedStrings(partial), "; ")), nil
	}
	return OK("every resource metric on %d node(s) returned to within %d%% of its pre-DRIVE baseline",
		checked, o.opts.TolerancePct), nil
}

// allowance is the excess a metric may show over its baseline: the larger of
// the percentage band and the absolute slack.
//
// Both matter. The percentage alone is meaningless at small magnitudes (50% of
// a 4-goroutine baseline is 2) and the absolute slack alone would let a
// gigabyte process leak 64 MiB unnoticed while flagging nothing at small sizes.
func (o *ResourceReturnToBaseline) allowance(metric string, baseline int64) int64 {
	pct := baseline / 100 * int64(o.opts.TolerancePct)
	if baseline < 0 {
		pct = 0
	}
	slack := o.opts.slackFor(metric)
	if slack > pct {
		return slack
	}
	return pct
}
