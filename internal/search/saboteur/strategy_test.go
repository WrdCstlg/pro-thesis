package saboteur

import (
	"testing"
)

// ---------------------------------------------------------------------------
// D-047: the classifier must be told the rate the samples were ACTUALLY taken
// at, not the rate A.3 asked for
//
// A.3 wants a 200 ms trajectory during a probe world and ForProbe honours that.
// The harness samples at search.observe.sample_interval_ms, which defaults to
// 500. The window-stretch guard then compares a real seven-sample span of
// ~3500 ms against a nominal 1200 ms, concludes the samples straddle a
// blackout, and returns INSUFFICIENT_DATA.
//
// Measured, before this override existed: 21 of 21 probes in run
// r_2026_09_08_e363 classified INSUFFICIENT_DATA, nothing reinforced, and Tier 2
// was never activated; on a fixture that does reinforce.
// ---------------------------------------------------------------------------

func TestTheObservedSamplingRateOverridesTheRequestedOne(t *testing.T) {
	base := DefaultObserveOptions()
	if got := ProbeOptionsFor(base, Observation{}).SampleIntervalMS; got != ProbeSampleIntervalMS {
		t.Fatalf("with no measured rate the options report %dms, want A.3's requested %dms",
			got, ProbeSampleIntervalMS)
	}
	got := ProbeOptionsFor(base, Observation{SampleIntervalMS: 500}).SampleIntervalMS
	if got != 500 {
		t.Fatalf("SampleIntervalMS = %d, want the MEASURED 500. A.3's 200ms is a statement about "+
			"what the harness should do; telemetry.Document.interval_ms is a measurement of what "+
			"it did, and the measurement has to win or the stretch guard refuses every window.",
			got)
	}
}

// The regression this closes, end to end through the classifier: a real
// 500 ms series must be classifiable, and telling the classifier it is a 200 ms
// series must be what breaks it. The test asserts BOTH halves so it cannot pass
// by the series being unclassifiable for some other reason.
func TestA500msSeriesIsClassifiableOnlyWhenTheRateIsKnown(t *testing.T) {
	// A clean, strongly rising 500 ms series: nine samples, +10 per sample.
	s := Series{Metric: MetricQueueDepth, Node: "kv-n1"}
	for i := 0; i < 9; i++ {
		s.Points = append(s.Points, Point{TMS: int64(i) * 500, Value: float64(10 * i)})
	}

	told := ProbeOptionsFor(DefaultObserveOptions(), Observation{SampleIntervalMS: 500})
	honest := ClassifySignal(s, told)
	if honest.Class == SpiralInsufficientData {
		t.Fatalf("a clean nine-sample 500ms rise was INSUFFICIENT_DATA when the rate was known: %s",
			honest.Reason)
	}
	if !honest.Class.Reinforcing() {
		t.Fatalf("a monotone +10-per-sample rise classified %s: %s", honest.Class, honest.Reason)
	}

	// The premise: with A.3's assumed 200 ms the SAME series is refused. If this
	// ever stops holding, the test above is no longer evidence of anything.
	assumed := DefaultObserveOptions().ForProbe()
	if got := ClassifySignal(s, assumed); got.Class != SpiralInsufficientData {
		t.Fatalf("with the rate ASSUMED at %dms the same series classified %s rather than "+
			"INSUFFICIENT_DATA, so this test no longer demonstrates the defect it was written for",
			ProbeSampleIntervalMS, got.Class)
	}
}

// A zero measured rate must change nothing: a caller that does not know the rate
// keeps whatever the options say.
func TestAnUnknownSamplingRateChangesNothing(t *testing.T) {
	base := DefaultObserveOptions()
	base.SampleIntervalMS = 750
	if got := ProbeOptionsFor(base, Observation{SampleIntervalMS: 0}).SampleIntervalMS; got != ProbeSampleIntervalMS {
		t.Fatalf("SampleIntervalMS = %d; with no measurement ForProbe's request stands", got)
	}
}
