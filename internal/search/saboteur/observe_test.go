package saboteur

import (
	"math"
	"testing"

	"github.com/WrdCstlg/pro-thesis/internal/recorder"
	"github.com/WrdCstlg/pro-thesis/internal/telemetry"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// ---------------------------------------------------------------------------
// synthetic curves
// ---------------------------------------------------------------------------

const probeIntervalMS = ProbeSampleIntervalMS

// curve builds a series sampled at the probe trajectory rate (A.3's 200 ms).
func curve(metric string, n int, f func(i int) float64) Series {
	s := Series{Metric: metric, Node: "kv-n2"}
	for i := 0; i < n; i++ {
		s.Points = append(s.Points, Point{TMS: int64(i * probeIntervalMS), Value: f(i)})
	}
	return s
}

func probeOpts() ObserveOptions { return DefaultObserveOptions().ForProbe() }

func classify(t *testing.T, s Series) Classification {
	t.Helper()
	return ClassifySignal(s, probeOpts())
}

func wantClass(t *testing.T, s Series, want SpiralClass) Classification {
	t.Helper()
	got := classify(t, s)
	if got.Class != want {
		t.Fatalf("%s: class = %s, want %s (trend %+.4g/s SE %.4g, accel %+.4g/s^2 SE %.4g): %s",
			s.Metric, got.Class, want, got.Trend, got.TrendSE, got.Accel, got.AccelSE, got.Reason)
	}
	return got
}

// A perfectly flat metric is the healthiest shape there is.
func TestFlatSeriesIsDampen(t *testing.T) {
	wantClass(t, curve("flat", 20, func(int) float64 { return 100 }), SpiralDampen)
}

// A steady rise is REINFORCE_LINEAR, never ACCELERATING: A.4 reserves the
// stronger label (and Tier 2's expensive escalation) for a rise that is
// itself speeding up.
func TestLinearRiseIsReinforceLinear(t *testing.T) {
	c := wantClass(t, curve("linear", 20, func(i int) float64 { return 100 + 10*float64(i) }),
		SpiralReinforceLinear)
	// 10 units per 200 ms sample is 50 units per second.
	if math.Abs(c.Trend-50) > 1e-6 {
		t.Fatalf("trend = %v/s, want 50/s: the regression's x-axis must be TIME, not the sample index", c.Trend)
	}
	if c.Class.Reinforcing() != true {
		t.Fatal("REINFORCE_LINEAR must report as reinforcing")
	}
}

// A.9's first definition of done requires this label to be reachable from probe
// data, which is why A.3's boolean scheme could not be the classifier.
func TestAcceleratingRiseIsReinforceAccelerating(t *testing.T) {
	c := wantClass(t, curve("accel", 20, func(i int) float64 { return float64(i) * float64(i) }),
		SpiralReinforceAccelerating)
	if c.Accel <= 0 {
		t.Fatalf("acceleration = %v, want positive", c.Accel)
	}
	r := Result{Class: c.Class}
	if !r.Escalate() {
		t.Fatal("A.4: a REINFORCE_ACCELERATING classification triggers immediate escalation to Tier 2")
	}
}

// A metric returning to baseline is the DAMPEN case A.3 describes.
func TestDecayingSeriesIsDampen(t *testing.T) {
	wantClass(t, curve("decay", 20, func(i int) float64 {
		return 100 * math.Exp(-float64(i)/4)
	}), SpiralDampen)
}

func TestSlowlyFallingSeriesIsDampen(t *testing.T) {
	wantClass(t, curve("falling", 20, func(i int) float64 { return 500 - 3*float64(i) }), SpiralDampen)
}

// ---------------------------------------------------------------------------
// the noise guard: this is the one that keeps the search off ghosts
// ---------------------------------------------------------------------------

// noisyFlat is a flat metric plus deterministic sensor noise, drawn from a
// path-keyed stream so the test is reproducible.
func noisyFlat(seed uint64, n int) Series {
	st := recorder.NewStream(recorder.MustDeriveKey(recorder.Seed(seed), "test.observe.noise"))
	return curve("noisy_flat", n, func(int) float64 {
		return 100 + (st.Float64()-0.5)*8 // +/- 4 units around a flat 100
	})
}

// A.4's literal rule is `if trend > 0 { REINFORCE }`. On a flat, noisy metric
// that is true about half the time, so the unguarded classifier reports the
// system is deteriorating on a coin flip, and every world it then escalates
// costs ~30 s of wall clock chasing nothing.
//
// The bounds are derived from the tests' own alphas, not from the measurement:
//
//   - REINFORCE at all requires the trend to clear 2 standard errors. The
//     trailing window is 5 samples, so the slope has 3 residual degrees of
//     freedom and P(t_3 > 2) = 0.070 one-sided. Bounded at 15%.
//   - ACCELERATING additionally requires the quadratic's curvature to clear the
//     same bar over 7 samples, df 4, P(t_4 > 2) = 0.058. Bounded at 10%.
//   - The unguarded rule admits any positive slope: 50%. Bounded below at 35%.
//
// A first version of this test bounded ACCELERATING at 3% on the reasoning that
// two ~6% tests would compose to ~0.4%. That reasoning is WRONG and the
// measurement said so: the two fits share five of their seven samples, so they
// are strongly dependent; 8 of the 13 series that cleared the trend bar also
// cleared the acceleration bar. The bound was corrected to the acceleration
// test's own alpha rather than the estimator being tuned until it matched a
// mistaken prediction.
//
// The residual rate is real and is not hidden: at the default 2-sigma bar about
// one flat, noisy series in 25 is reported as a spiral. An escalation policy
// that cannot afford that should require corroboration across metrics or nodes
// before spending Tier 2's budget, which is escalate.go's decision, not this
// classifier's.
func TestNoisyFlatSeriesIsDampenAndTheGuardIsWhy(t *testing.T) {
	const trials = 200
	opt := probeOpts()
	unguarded := opt
	unguarded.NoiseSigma = 0 // A.4 as written

	var reinforce, accelerating, unguardedReinforce int
	for s := uint64(0); s < trials; s++ {
		series := noisyFlat(s, 20)
		switch ClassifySignal(series, opt).Class {
		case SpiralReinforceAccelerating:
			accelerating++
			reinforce++
		case SpiralReinforceLinear:
			reinforce++
		}
		if ClassifySignal(series, unguarded).Class.Reinforcing() {
			unguardedReinforce++
		}
	}
	t.Logf("over %d noise realizations: guarded REINFORCE %d (%.1f%%), of which ACCELERATING %d; "+
		"unguarded REINFORCE %d (%.1f%%)",
		trials, reinforce, 100*float64(reinforce)/trials, accelerating,
		unguardedReinforce, 100*float64(unguardedReinforce)/trials)

	if unguardedReinforce < trials*35/100 {
		t.Fatalf("the unguarded rule fired on only %d/%d flat noise series; this test's premise is "+
			"that A.4 as written fires about half the time, so something else changed",
			unguardedReinforce, trials)
	}
	if reinforce > trials*15/100 {
		t.Fatalf("the noise guard let %d/%d flat noise series through as REINFORCE; the search "+
			"would spend those rollouts chasing sampling noise", reinforce, trials)
	}
	if accelerating > trials*10/100 {
		t.Fatalf("%d/%d flat noise series triggered a Tier 2 escalation; the acceleration test's "+
			"own one-sided alpha at 2 standard errors with 4 degrees of freedom is 5.8%%",
			accelerating, trials)
	}
}

// A genuinely LINEAR rise must not be escalated as an accelerating one. This is
// the failure the quadratic fit was introduced to remove: A.4's slope-of-slopes
// over three overlapping windows called a noisy straight line ACCELERATING.
func TestANoisyLinearRiseIsNotEscalatedAsAccelerating(t *testing.T) {
	const trials = 200
	opt := probeOpts()
	var reinforcing, accelerating int
	for s := uint64(0); s < trials; s++ {
		st := recorder.NewStream(recorder.MustDeriveKey(recorder.Seed(s), "test.observe.noise"))
		series := curve("noisy_rise", 20, func(i int) float64 {
			return 100 + 10*float64(i) + (st.Float64()-0.5)*8
		})
		c := ClassifySignal(series, opt)
		if c.Class.Reinforcing() {
			reinforcing++
		}
		if c.Class == SpiralReinforceAccelerating {
			accelerating++
		}
	}
	t.Logf("over %d noisy linear rises: REINFORCE %d, of which ACCELERATING %d (%.1f%%)",
		trials, reinforcing, accelerating, 100*float64(accelerating)/trials)

	if reinforcing != trials {
		t.Fatalf("a 50/s rise through +/-4 units of noise is unmistakable; only %d/%d were "+
			"reported as REINFORCE", reinforcing, trials)
	}
	// The curvature of a straight line is zero, so this is the acceleration
	// test's own alpha again: 5.8% one-sided, bounded at 10%.
	if accelerating > trials*10/100 {
		t.Fatalf("%d/%d straight lines were called ACCELERATING", accelerating, trials)
	}
}

// A real rise must still be seen through the same amount of noise. A guard that
// suppressed genuine signal would be worse than no guard at all.
func TestTheNoiseGuardDoesNotSuppressARealRise(t *testing.T) {
	st := recorder.NewStream(recorder.MustDeriveKey(recorder.Seed(7), "test.observe.noise"))
	s := curve("noisy_rise", 20, func(i int) float64 {
		return 100 + 10*float64(i) + (st.Float64()-0.5)*8
	})
	c := classify(t, s)
	if !c.Class.Reinforcing() {
		t.Fatalf("class = %s (%s): a 50/s rise through +/-4 units of noise must still be seen; "+
			"a guard that suppressed genuine signal would be worse than no guard", c.Class, c.Reason)
	}
	if !c.NoiseChecked {
		t.Fatal("the noise guard must have been applied")
	}
}

// ---------------------------------------------------------------------------
// insufficient data is NOT dampen
// ---------------------------------------------------------------------------

// Collapsing "the system recovered" into "there was no data" makes a clean world
// and an unevaluable one indistinguishable, and every utility ranking built on
// top of that is meaningless. It is the ranked #1 failure mode for this phase.
func TestTooFewSamplesIsInsufficientDataNotDampen(t *testing.T) {
	c := wantClass(t, curve("short", 3, func(i int) float64 { return float64(i) }), SpiralInsufficientData)
	if c.Class.Evaluable() {
		t.Fatal("INSUFFICIENT_DATA is not a claim about the system")
	}
	if c.Class.Reinforcing() {
		t.Fatal("INSUFFICIENT_DATA must never contribute a reinforcing signal")
	}
	if c.Reason == "" {
		t.Fatal("an unevaluable series must say why")
	}
}

func TestAnEmptySeriesIsInsufficientData(t *testing.T) {
	wantClass(t, Series{Metric: "nothing", Node: "kv-n1"}, SpiralInsufficientData)
}

// A series with exactly the window size is the boundary and must be classifiable.
func TestExactlyWindowSizeSamplesIsClassifiable(t *testing.T) {
	opt := probeOpts()
	s := curve("boundary", opt.SpiralWindow, func(i int) float64 { return 100 + float64(i) })
	c := ClassifySignal(s, opt)
	if !c.Class.Evaluable() {
		t.Fatalf("%d samples is exactly the window and must be classifiable: %s", opt.SpiralWindow, c.Reason)
	}
	if c.AccelKnown {
		t.Fatal("one trend window cannot yield an acceleration")
	}
	if c.Class != SpiralReinforceLinear {
		t.Fatalf("class = %s: with no acceleration available the WEAKER reinforce label is correct", c.Class)
	}
}

// ---------------------------------------------------------------------------
// absent is not zero
// ---------------------------------------------------------------------------

// The single most important test in this file.
//
// internal/telemetry deliberately keeps an unobservable metric ABSENT: a paused
// container answers nothing, and reporting 0 would make a healthy system look
// like it fell off a cliff. A consumer can undo that in one careless line.
//
// The series below is a metric that sat at 100, went unobservable while the
// container was paused, and came back at 100: a system that was fine
// throughout. Read correctly it is INSUFFICIENT_DATA over the gap. Read with
// absence as zero it is a textbook REINFORCE.
func TestAbsenceIsNotZeroAndTheDifferenceIsAFalseReinforce(t *testing.T) {
	opt := probeOpts()

	honest := Series{Metric: MetricQueueDepth, Node: "kv-n2"}
	asZero := Series{Metric: MetricQueueDepth, Node: "kv-n2"}
	for i := 0; i < 18; i++ {
		tms := int64(i * probeIntervalMS)
		observable := i < 5 || i >= 15
		if observable {
			honest.Points = append(honest.Points, Point{TMS: tms, Value: 100})
		} else {
			honest.Absent = append(honest.Absent, Absence{TMS: tms, Reason: "container is paused"})
		}
		v := 100.0
		if !observable {
			v = 0 // the careless line
		}
		asZero.Points = append(asZero.Points, Point{TMS: tms, Value: v})
	}

	got := ClassifySignal(honest, opt)
	if got.Class != SpiralInsufficientData {
		t.Fatalf("class = %s, want INSUFFICIENT_DATA: the trailing window straddles a %dms gap in "+
			"observation and fitting a line across it invents a trend. %s",
			got.Class, got.WindowSpanMS, got.Reason)
	}
	if got.AbsentCount != 10 {
		t.Fatalf("AbsentCount = %d, want 10", got.AbsentCount)
	}

	bad := ClassifySignal(asZero, opt)
	if !bad.Class.Reinforcing() {
		t.Fatalf("this test's premise is that absence-read-as-zero manufactures a REINFORCE; the "+
			"zeroed series classified as %s (%s), so the premise no longer holds", bad.Class, bad.Reason)
	}
}

// Absence that does not disturb the trailing window must not block
// classification: the guard is about gaps, not about absence as such.
func TestAbsenceOutsideTheTrailingWindowDoesNotBlockClassification(t *testing.T) {
	s := Series{Metric: "queue", Node: "kv-n1"}
	s.Absent = append(s.Absent, Absence{TMS: 0, Reason: "container is paused"})
	s.Absent = append(s.Absent, Absence{TMS: 200, Reason: "container is paused"})
	for i := 2; i < 20; i++ {
		s.Points = append(s.Points, Point{TMS: int64(i * probeIntervalMS), Value: 100 + 10*float64(i)})
	}
	c := wantClass(t, s, SpiralReinforceLinear)
	if c.AbsentCount != 2 {
		t.Fatalf("AbsentCount = %d, want 2: absence is still reported even when it did not block", c.AbsentCount)
	}
}

// A series whose samples all carry one timestamp has an undefined slope. Zero
// would read as "flat", which is a claim the data cannot make.
func TestSamplesSharingATimestampAreInsufficientData(t *testing.T) {
	s := Series{Metric: "stuck", Node: "kv-n1"}
	for i := 0; i < 10; i++ {
		s.Points = append(s.Points, Point{TMS: 500, Value: float64(i)})
	}
	wantClass(t, s, SpiralInsufficientData)
}

func TestOutOfOrderPointsAreSortedNotRejected(t *testing.T) {
	s := curve("rise", 20, func(i int) float64 { return 100 + 10*float64(i) })
	// Reverse the input.
	for i, j := 0, len(s.Points)-1; i < j; i, j = i+1, j-1 {
		s.Points[i], s.Points[j] = s.Points[j], s.Points[i]
	}
	wantClass(t, s, SpiralReinforceLinear)
}

// ---------------------------------------------------------------------------
// A.3's criteria as evidence
// ---------------------------------------------------------------------------

func criterion(cs []Criterion, name CriterionName) Criterion {
	for _, c := range cs {
		if c.Name == name {
			return c
		}
	}
	return Criterion{Name: name}
}

// OQ-006 item 4. Pausing a Raft leader always causes an election, in a correct
// implementation as much as a buggy one, so the criterion carries no
// information. It is kept as a place to look and scored at zero.
func TestARoleChangeCarriesNoWeight(t *testing.T) {
	obs := Observation{
		Node:          "kv-n2",
		Fault:         Window{StartMS: 8200, EndMS: 11000},
		RolesObserved: true,
		RoleChanges:   []RoleChange{{TMS: 8600, From: "leader", To: "follower"}},
	}
	c := criterion(EvaluateCriteria(obs, probeOpts()), CritRoleChange)
	if !c.Known || !c.Met {
		t.Fatalf("the role change is inside the 1s window and must be detected: %+v", c)
	}
	if c.Weighted {
		t.Fatal("a criterion that fires identically on healthy and unhealthy systems must carry " +
			"no weight (OQ-006 item 4)")
	}
}

// The evidence layer must never promote a DAMPEN. A.4's classifier alone decides
// the label; A.3's criteria only order the ranked list.
//
// The observation below is built so that a WEIGHTED criterion is genuinely
// satisfied: otherwise the test would pass without ever exercising the path it
// is about, which is exactly what an earlier version of it did.
func TestEvidenceNeverPromotesADampen(t *testing.T) {
	obs := Observation{
		Node:  "kv-n2",
		Fault: Window{StartMS: 400, EndMS: 800},
		Metrics: []Series{
			// Recovering: the classifier's answer is DAMPEN.
			curve(MetricQueueDepth, 20, func(i int) float64 { return 500 - 10*float64(i) }),
			// But the error rate did rise after withdrawal, and the leader did
			// change inside the 1s window.
			{
				Metric: MetricErrorRate, Node: "kv-n2",
				Points: []Point{{TMS: 500, Value: 1}, {TMS: 700, Value: 1}, {TMS: 1200, Value: 9}},
			},
		},
		Baselines:     []Baseline{{Metric: MetricRetryCount, Value: 1, Known: true}},
		RolesObserved: true,
		RoleChanges:   []RoleChange{{TMS: 600, From: "leader", To: "follower"}},
	}
	r := Classify(obs, probeOpts())

	met := 0
	for _, c := range r.Criteria {
		if c.Weighted && c.Known && c.Met {
			met++
		}
	}
	if met == 0 {
		t.Fatal("setup: no weighted criterion is satisfied, so this test would pass without " +
			"exercising the promotion path at all")
	}
	if r.Class != SpiralDampen {
		t.Fatalf("class = %s with %d weighted criteria met, want DAMPEN: a boolean criterion must "+
			"not talk the classifier into a REINFORCE. %s", r.Class, met, r.Reason)
	}
	if r.Escalate() {
		t.Fatal("a DAMPEN must never escalate")
	}
}

// A DAMPEN satisfying every criterion still ranks below a REINFORCE_LINEAR
// satisfying none: the class dominates by two orders of magnitude.
func TestClassDominatesTheRanking(t *testing.T) {
	opt := probeOpts()

	rising := Classify(Observation{
		Node:    "kv-n1",
		Fault:   Window{StartMS: 0, EndMS: 200},
		Metrics: []Series{curve(MetricQueueDepth, 20, func(i int) float64 { return 100 + 10*float64(i) })},
	}, opt)
	if rising.Class != SpiralReinforceLinear {
		t.Fatalf("setup: class = %s", rising.Class)
	}

	// A flat queue that nevertheless satisfies the queue-monotonic criterion is
	// impossible, so build the DAMPEN out of a falling metric and give it the
	// error-rate criterion instead.
	damp := Classify(Observation{
		Node:  "kv-n2",
		Fault: Window{StartMS: 0, EndMS: 1000},
		Metrics: []Series{
			curve(MetricQueueDepth, 20, func(i int) float64 { return 500 - 10*float64(i) }),
			{
				Metric: MetricErrorRate, Node: "kv-n2",
				Points: []Point{{TMS: 200, Value: 1}, {TMS: 600, Value: 1}, {TMS: 1400, Value: 9}},
			},
		},
	}, opt)
	if damp.Class != SpiralDampen {
		t.Fatalf("setup: class = %s (%s)", damp.Class, damp.Reason)
	}
	if got := criterion(damp.Criteria, CritErrorRateAfter); !got.Known || !got.Met {
		t.Fatalf("setup: the error-rate criterion should be met: %+v", got)
	}
	if damp.Rank >= rising.Rank {
		t.Fatalf("a DAMPEN with evidence (%v) outranked a REINFORCE_LINEAR without (%v)",
			damp.Rank, rising.Rank)
	}
}

// "Nothing could measure it" and "it did not hold" are different facts, and only
// one of them supports a DAMPEN. internal/telemetry v1 collects no retry counter
// and no driver error rate, so those two criteria are UNEVALUABLE on this stack.
func TestAnUnevaluableCriterionIsUnknownNotFalse(t *testing.T) {
	cs := EvaluateCriteria(Observation{Node: "kv-n1", Fault: Window{StartMS: 0, EndMS: 100}}, probeOpts())
	for _, name := range []CriterionName{CritRetryOverBaseline, CritErrorRateAfter, CritQueueMonotonic} {
		c := criterion(cs, name)
		if c.Known {
			t.Fatalf("%s reported as evaluable with no data at all: %+v", name, c)
		}
		if c.Met {
			t.Fatalf("%s must not be Met when it is not Known", name)
		}
		if c.Detail == "" {
			t.Fatalf("%s must say why it could not be evaluated", name)
		}
	}
	// Role: not observable is not the same as "no role change".
	c := criterion(cs, CritRoleChange)
	if c.Known {
		t.Fatalf("with RolesObserved false, a role change can be neither confirmed nor ruled out: %+v", c)
	}
}

// D-028 item 6: every A.3 criterion is comparative, so without the no-fault
// control world's baseline there is nothing to compare against.
func TestARatioCriterionWithoutABaselineIsUnknown(t *testing.T) {
	obs := Observation{
		Node:  "kv-n1",
		Fault: Window{StartMS: 0, EndMS: 400},
		Metrics: []Series{{
			Metric: MetricRetryCount, Node: "kv-n1",
			Points: []Point{{TMS: 200, Value: 50}, {TMS: 600, Value: 90}},
		}},
	}
	c := criterion(EvaluateCriteria(obs, probeOpts()), CritRetryOverBaseline)
	if c.Known {
		t.Fatalf("no baseline was supplied, so the criterion is unevaluable: %+v", c)
	}

	obs.Baselines = []Baseline{{Metric: MetricRetryCount, Value: 10, Known: true}}
	c = criterion(EvaluateCriteria(obs, probeOpts()), CritRetryOverBaseline)
	if !c.Known || !c.Met {
		t.Fatalf("a peak of 90 against a baseline of 10 exceeds 2x: %+v", c)
	}
}

// A.3: "queue depth monotonically increased for >= 3 consecutive samples after
// fault withdrawal".
func TestQueueMonotonicCriterion(t *testing.T) {
	base := Observation{Node: "kv-n1", Fault: Window{StartMS: 0, EndMS: 1000}}

	rising := base
	rising.Metrics = []Series{{
		Metric: MetricQueueDepth, Node: "kv-n1",
		Points: []Point{
			{TMS: 1200, Value: 1}, {TMS: 1400, Value: 2},
			{TMS: 1600, Value: 3}, {TMS: 1800, Value: 4},
		},
	}}
	if c := criterion(EvaluateCriteria(rising, probeOpts()), CritQueueMonotonic); !c.Known || !c.Met {
		t.Fatalf("three consecutive increases after withdrawal must satisfy the criterion: %+v", c)
	}

	bumpy := base
	bumpy.Metrics = []Series{{
		Metric: MetricQueueDepth, Node: "kv-n1",
		Points: []Point{
			{TMS: 1200, Value: 1}, {TMS: 1400, Value: 2},
			{TMS: 1600, Value: 1}, {TMS: 1800, Value: 2},
		},
	}}
	if c := criterion(EvaluateCriteria(bumpy, probeOpts()), CritQueueMonotonic); !c.Known || c.Met {
		t.Fatalf("two runs of one increase is not a run of three: %+v", c)
	}

	// Samples DURING the fault window do not count: A.3 is explicit that this
	// criterion is about what happens after withdrawal.
	during := base
	during.Metrics = []Series{{
		Metric: MetricQueueDepth, Node: "kv-n1",
		Points: []Point{
			{TMS: 200, Value: 1}, {TMS: 400, Value: 2},
			{TMS: 600, Value: 3}, {TMS: 800, Value: 4},
		},
	}}
	if c := criterion(EvaluateCriteria(during, probeOpts()), CritQueueMonotonic); c.Known {
		t.Fatalf("no samples after withdrawal, so the criterion is unevaluable: %+v", c)
	}
}

// "Consecutive" means consecutive in TIME. Absent samples are not in Points, so
// readings either side of a blackout sit adjacent in the slice; counting them as
// a run would build the evidence out of a gap in observation.
func TestAMonotonicRunIsNotAssembledAcrossAGapInObservation(t *testing.T) {
	obs := Observation{Node: "kv-n1", Fault: Window{StartMS: 0, EndMS: 1000}}
	obs.Metrics = []Series{{
		Metric: MetricQueueDepth, Node: "kv-n1",
		// Four rising values, but each pair is seconds apart because the metric
		// was unobservable in between.
		Points: []Point{
			{TMS: 1200, Value: 1}, {TMS: 5200, Value: 2},
			{TMS: 9200, Value: 3}, {TMS: 13200, Value: 4},
		},
		Absent: []Absence{{TMS: 1400, Reason: "container is paused"}},
	}}
	c := criterion(EvaluateCriteria(obs, probeOpts()), CritQueueMonotonic)
	if c.Met {
		t.Fatalf("four readings 4s apart at a 200ms sampling rate are not consecutive samples: %+v", c)
	}

	// The same four values sampled consecutively DO satisfy it, so the test
	// above is about the gap and not about the values.
	obs.Metrics[0].Points = []Point{
		{TMS: 1200, Value: 1}, {TMS: 1400, Value: 2},
		{TMS: 1600, Value: 3}, {TMS: 1800, Value: 4},
	}
	if c := criterion(EvaluateCriteria(obs, probeOpts()), CritQueueMonotonic); !c.Met {
		t.Fatalf("consecutively sampled increases must satisfy the criterion: %+v", c)
	}
}

// ---------------------------------------------------------------------------
// Classify over several metrics
// ---------------------------------------------------------------------------

func TestClassifyTakesTheWorstEvaluableMetric(t *testing.T) {
	obs := Observation{
		Node:  "kv-n2",
		Fault: Window{StartMS: 0, EndMS: 200},
		Metrics: []Series{
			curve("calm", 20, func(int) float64 { return 100 }),
			curve("spiralling", 20, func(i int) float64 { return float64(i) * float64(i) }),
			curve("also_calm", 20, func(i int) float64 { return 100 - float64(i) }),
		},
	}
	r := Classify(obs, probeOpts())
	if r.Class != SpiralReinforceAccelerating {
		t.Fatalf("class = %s, want REINFORCE_ACCELERATING: A.4 escalates on ANY metric", r.Class)
	}
	if !r.Escalate() {
		t.Fatal("A.4: a REINFORCE_ACCELERATING on any metric triggers escalation")
	}
	if len(r.Metrics) != 3 {
		t.Fatalf("every metric's classification must be reported, got %d", len(r.Metrics))
	}
}

// A world where nothing could be measured is not a clean world.
func TestClassifyWithNothingMeasurableIsInsufficientNotDampen(t *testing.T) {
	obs := Observation{
		Node:  "kv-n3",
		Fault: Window{StartMS: 0, EndMS: 200},
		Metrics: []Series{
			{Metric: "queue", Node: "kv-n3", Absent: []Absence{{TMS: 0, Reason: "container is paused"}}},
			{Metric: "rss", Node: "kv-n3"},
		},
	}
	r := Classify(obs, probeOpts())
	if r.Class != SpiralInsufficientData {
		t.Fatalf("class = %s, want INSUFFICIENT_DATA", r.Class)
	}
	if r.Class == SpiralDampen {
		t.Fatal("an unevaluable world must never be scored as a clean one")
	}
	if r.Reason == "" {
		t.Fatal("the result must say that nothing could be classified")
	}
}

// ---------------------------------------------------------------------------
// options
// ---------------------------------------------------------------------------

// OQ-006 item 2: 500 ms is the configurable general default,
// 200 ms is the probe-specific trajectory rate. They describe different things.
func TestSamplingIntervalsAreTheTwoDocumentedRates(t *testing.T) {
	if schema.DefaultSampleIntervalMS != 500 {
		t.Fatalf("A.4 and A.10 both say 500ms; schema's default is %d", schema.DefaultSampleIntervalMS)
	}
	def := DefaultObserveOptions()
	if def.SampleIntervalMS != schema.DefaultSampleIntervalMS {
		t.Fatalf("default sample interval = %d, want %d", def.SampleIntervalMS, schema.DefaultSampleIntervalMS)
	}
	if def.ForProbe().SampleIntervalMS != 200 {
		t.Fatalf("probe sample interval = %d, want 200", def.ForProbe().SampleIntervalMS)
	}
	if def.SpiralWindow != 5 || def.AccelWindow != 3 {
		t.Fatalf("A.4's windows are 5 and 3, got %d and %d", def.SpiralWindow, def.AccelWindow)
	}
}

func TestObserveOptionsComeFromTheConfigBlock(t *testing.T) {
	cfg := schema.DefaultConfig()
	cfg.Search.Observe.SpiralWindow = 9
	cfg.Search.Observe.SampleIntervalMS = 250
	cfg.Search.Observe.ReinforceThreshold = 2.5
	got := ObserveOptionsFrom(cfg.Search.Observe)
	if got.SpiralWindow != 9 || got.SampleIntervalMS != 250 || got.ReinforceThreshold != 2.5 {
		t.Fatalf("options = %+v", got)
	}
	if got.NoiseSigma != DefaultNoiseSigma {
		t.Fatalf("the additive knobs must keep their defaults, got NoiseSigma %v", got.NoiseSigma)
	}
}

// The threshold is a floor on the slope, in units per second.
func TestReinforceThresholdSuppressesASmallRise(t *testing.T) {
	s := curve("creep", 20, func(i int) float64 { return 100 + 0.02*float64(i) })
	opt := probeOpts()
	if c := ClassifySignal(s, opt); !c.Class.Reinforcing() {
		t.Fatalf("at threshold 0 a clean small rise is a REINFORCE: %s (%s)", c.Class, c.Reason)
	}
	opt.ReinforceThreshold = 1.0 // units per second
	if c := ClassifySignal(s, opt); c.Class != SpiralDampen {
		t.Fatalf("a 0.1/s rise is below a 1.0/s threshold: %s (%s)", c.Class, c.Reason)
	}
}

// ---------------------------------------------------------------------------
// building a series from telemetry
// ---------------------------------------------------------------------------

func sampleAt(node string, vms int64, queue *int64, absentReason string) *telemetry.Sample {
	s := &telemetry.Sample{Schema: telemetry.SampleSchema, Node: node, VMS: &vms}
	if queue != nil {
		s.Status = &telemetry.StatusMetrics{QueueDepth: queue}
	} else {
		s.MarkAbsent(telemetry.MetricStatusQueue, "%s", absentReason)
	}
	return s
}

// The projection must preserve internal/telemetry's central property: a metric
// that was not observed does not become a number.
func TestSeriesFromTelemetryKeepsAbsenceAbsent(t *testing.T) {
	q := func(v int64) *int64 { return &v }
	samples := []*telemetry.Sample{
		sampleAt("kv-n1", 0, q(4), ""),
		sampleAt("kv-n1", 200, nil, "container is paused"),
		sampleAt("kv-n1", 400, q(6), ""),
		sampleAt("kv-n2", 400, q(999), ""), // another node: not ours
	}
	s := QueueDepthSeries(samples, "kv-n1")

	if len(s.Points) != 2 {
		t.Fatalf("points = %+v, want the two observed samples only", s.Points)
	}
	for _, p := range s.Points {
		if p.Value == 0 {
			t.Fatal("an absent sample became a zero point")
		}
	}
	if len(s.Absent) != 1 || s.Absent[0].TMS != 200 {
		t.Fatalf("absent = %+v, want one entry at t+200ms", s.Absent)
	}
	if s.Absent[0].Reason != "container is paused" {
		t.Fatalf("the reason telemetry recorded must survive, got %q", s.Absent[0].Reason)
	}
	// MetricQueueDepth is BOUND to telemetry's own constant so the two cannot
	// drift; this checks the projection carries it through.
	if s.Metric != telemetry.MetricStatusQueue {
		t.Fatalf("metric name %q must be telemetry's own constant %q", s.Metric, telemetry.MetricStatusQueue)
	}
}

func TestSeriesFromSkipsSamplesWithNoVirtualTime(t *testing.T) {
	q := int64(3)
	noVMS := &telemetry.Sample{
		Schema: telemetry.SampleSchema, Node: "kv-n1",
		Status: &telemetry.StatusMetrics{QueueDepth: &q},
	}
	s := QueueDepthSeries([]*telemetry.Sample{noVMS}, "kv-n1")
	if len(s.Points) != 0 {
		t.Fatal("a sample with no virtual timestamp cannot be placed on the fault-window axis")
	}
	if len(s.Absent) != 1 {
		t.Fatal("and it must be recorded as an absence rather than dropped silently")
	}
}

func TestSpiralClassNames(t *testing.T) {
	want := map[SpiralClass]string{
		SpiralInsufficientData:      "INSUFFICIENT_DATA",
		SpiralDampen:                "DAMPEN",
		SpiralReinforceLinear:       "REINFORCE_LINEAR",
		SpiralReinforceAccelerating: "REINFORCE_ACCELERATING",
	}
	for c, name := range want {
		if c.String() != name {
			t.Fatalf("%d.String() = %q, want %q", int(c), c.String(), name)
		}
	}
	if SpiralDampen.Reinforcing() || !SpiralDampen.Evaluable() {
		t.Fatal("DAMPEN is an evaluable, non-reinforcing claim")
	}
}
