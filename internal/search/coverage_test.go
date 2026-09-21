package search

import (
	"fmt"
	"testing"

	"github.com/WrdCstlg/pro-thesis/internal/telemetry"
)

// TestObserveBuildsBothSignalsFromWhatAWorldProduced is the end-to-end shape a
// caller in internal/control uses: log lines from Phase 1's collector, the
// telemetry document Phase 1 already materialised, nothing re-collected.
func TestObserveBuildsBothSignalsFromWhatAWorldProduced(t *testing.T) {
	samplesA := make([]telemetry.Sample, 0, 10)
	samplesB := make([]telemetry.Sample, 0, 10)
	for i := int64(0); i < 10; i++ {
		role := "leader"
		term := int64(5)
		if i >= 5 {
			// A leader change halfway through: a second global state.
			role = "follower"
			term = 6
		}
		samplesA = append(samplesA, sample("kv-n1", i, role, term, 0, 4, 2, true))
		samplesB = append(samplesB, sample("kv-n2", i, "follower", term, 0, 4, 2, true))
	}

	obs := WorldObservation{
		Logs: []NodeLog{
			{Node: "kv-n1", Lines: fixtureLines},
			{Node: "kv-n2", Lines: fixtureLines},
		},
		Telemetry: doc(
			telemetry.NodeSeries{Node: "kv-n1", Samples: samplesA},
			telemetry.NodeSeries{Node: "kv-n2", Samples: samplesB},
		),
	}
	cov := Observe(obs)

	if got, want := cov.Templates.Len(), len(fixtureLines); got != want {
		t.Fatalf("templates = %d, want %d (two nodes emitting the same lines is not twice the coverage)", got, want)
	}
	if got := cov.Templates.Lines(); got != int64(2*len(fixtureLines)) {
		t.Fatalf("lines = %d, want %d", got, 2*len(fixtureLines))
	}
	if got := cov.States.Len(); got != 2 {
		t.Fatalf("states = %d, want 2:\n%v", got, cov.States.IDs())
	}
	if got := cov.States.Rounds(); got != 10 {
		t.Fatalf("rounds = %d, want 10", got)
	}
}

// TestObserveWithNoTelemetryStillCollectsTemplates: a world whose telemetry
// document is missing must not lose its log coverage, and must not fabricate a
// state.
func TestObserveWithNoTelemetryStillCollectsTemplates(t *testing.T) {
	cov := Observe(WorldObservation{Logs: []NodeLog{{Node: "kv-n1", Lines: fixtureLines}}})
	if cov.Templates.Len() != len(fixtureLines) {
		t.Fatalf("templates = %d, want %d", cov.Templates.Len(), len(fixtureLines))
	}
	if cov.States.Len() != 0 {
		t.Fatalf("states = %d with no telemetry; a state was fabricated", cov.States.Len())
	}
}

// TestDeltaIsMeasuredBeforeTheMerge, at the Coverage level.
func TestDeltaIsMeasuredBeforeTheMerge(t *testing.T) {
	global := NewCoverage()
	first := Observe(WorldObservation{Logs: []NodeLog{{Node: "kv-n1", Lines: fixtureLines[:5]}}})
	d := first.Against(global)
	if d.NewTemplates != 5 {
		t.Fatalf("first world delta = %d, want 5", d.NewTemplates)
	}
	if !d.Positive() {
		t.Fatal("a five-template delta was not positive")
	}
	global.Merge(first)

	same := Observe(WorldObservation{Logs: []NodeLog{{Node: "kv-n2", Lines: fixtureLines[:5]}}})
	if d := same.Against(global); d.Positive() {
		t.Fatalf("a world that added nothing reported a positive delta: %+v", d)
	}

	more := Observe(WorldObservation{Logs: []NodeLog{{Node: "kv-n2", Lines: fixtureLines}}})
	if d := more.Against(global); d.NewTemplates != int64(len(fixtureLines)-5) {
		t.Fatalf("delta = %d, want %d", d.NewTemplates, len(fixtureLines)-5)
	}
}

// TestCoverageCountsRenderIntoTheFrozenVerdictShape.
func TestCoverageCountsRenderIntoTheFrozenVerdictShape(t *testing.T) {
	global := NewCoverage()
	global.Merge(Observe(WorldObservation{Logs: []NodeLog{{Node: "kv-n1", Lines: fixtureLines}}}))
	c := global.Counts()
	if c.CumTemplates != int64(len(fixtureLines)) {
		t.Fatalf("cum_templates = %d, want %d", c.CumTemplates, len(fixtureLines))
	}
	if c.NewTemplates != 0 {
		t.Fatalf("new_templates = %d; Counts must not invent a per-run figure only the caller knows",
			c.NewTemplates)
	}
}

// TestCoverageDoesNotGrowWithLogVolume is the whole point of the signal, stated
// as an aggregate. Ten thousand lines of the same ten code paths is ten
// templates.
func TestCoverageDoesNotGrowWithLogVolume(t *testing.T) {
	lines := make([]string, 0, 10000)
	for i := 0; i < 1000; i++ {
		for _, base := range fixtureLines {
			lines = append(lines, fmt.Sprintf("%s iter=%d", base, i))
		}
	}
	cov := Observe(WorldObservation{Logs: []NodeLog{{Node: "kv-n1", Lines: lines}}})
	if cov.Templates.Len() != len(fixtureLines) {
		t.Fatalf("%d lines produced %d templates, want %d", len(lines), cov.Templates.Len(), len(fixtureLines))
	}
}
