package oracle

import (
	"context"
	"strings"
	"testing"

	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// A node that supplies ONE of the four configured metrics (goroutines from a
// declared metrics endpoint, say, on an image with no shell to read RSS
// through) is checked on that metric only. The OK text must say so; "every
// resource metric returned to baseline" would assert three measurements that
// were never made.
func TestResourceOKTextNamesTheMetricsItCouldNotEvaluate(t *testing.T) {
	o := NewResourceReturnToBaseline(DefaultResourceOptions())
	in := &Input{
		Phases: schema.PhaseTimings{
			{Phase: schema.PhaseDrive, StartMS: 0, EndMS: 5000},
			{Phase: schema.PhaseQuiesce, StartMS: 5000, EndMS: 7000},
		},
		Nodes: []NodeObservation{{
			NodeID:   "n1",
			Metrics:  []Series{{Metric: "goroutines", Points: []Sample{{TMS: -500, Value: 10}, {TMS: 6500, Value: 11}}}},
			Baseline: []Metric{{Name: "goroutines", Value: 10}},
		}},
	}
	res, err := o.Evaluate(context.Background(), schema.PhaseAssert, in)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != schema.StatusOK {
		t.Fatalf("want ok, got %s: %s", res.Status, res.Explanation)
	}
	if strings.Contains(res.Explanation, "every resource metric") {
		t.Fatalf("the OK text asserts metrics that were never evaluated: %q", res.Explanation)
	}
	for _, m := range []string{"rss_bytes", "fd_count", "threads"} {
		if !strings.Contains(res.Explanation, m) {
			t.Fatalf("the OK text must name unevaluated metric %s: %q", m, res.Explanation)
		}
	}
}

// When every configured metric was evaluated the unqualified text stands.
func TestResourceOKTextIsUnqualifiedWhenEveryMetricWasEvaluated(t *testing.T) {
	o := NewResourceReturnToBaseline(DefaultResourceOptions())
	var series []Series
	var base []Metric
	for _, m := range o.opts.Metrics {
		series = append(series, Series{Metric: m, Points: []Sample{{TMS: -500, Value: 100}, {TMS: 6500, Value: 100}}})
		base = append(base, Metric{Name: m, Value: 100})
	}
	in := &Input{
		Phases: schema.PhaseTimings{
			{Phase: schema.PhaseDrive, StartMS: 0, EndMS: 5000},
			{Phase: schema.PhaseQuiesce, StartMS: 5000, EndMS: 7000},
		},
		Nodes: []NodeObservation{{NodeID: "n1", Metrics: series, Baseline: base}},
	}
	res, err := o.Evaluate(context.Background(), schema.PhaseAssert, in)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != schema.StatusOK || !strings.Contains(res.Explanation, "every resource metric") {
		t.Fatalf("got %s: %s", res.Status, res.Explanation)
	}
}
