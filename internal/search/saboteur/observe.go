package saboteur

import (
	"fmt"
	"math"
	"sort"

	"github.com/WrdCstlg/pro-thesis/internal/telemetry"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// ---------------------------------------------------------------------------
// A.4: the telemetry spiral detector
// ---------------------------------------------------------------------------
//
// # Reconciling A.3 and A.4 (OQ-006 item 3)
//
// Addendum A defines two things that share names and do not agree. A.3
// classifies a probe DAMPEN or REINFORCE from four boolean event criteria; A.4
// classifies a metric series DAMPEN / REINFORCE_LINEAR / REINFORCE_ACCELERATING
// from the trend and acceleration of a regression. A.9's first definition of
// done requires the label REINFORCE_ACCELERATING from a PROBE, which A.3's
// scheme cannot produce at all.
//
// The reading taken, and recorded in OQ-006: A.4's trend/acceleration
// classifier IS the classifier (it alone decides the label) and A.3's four
// criteria are EVIDENCE it is reported alongside. Evidence orders A.3's "ranked
// list ... ordered by reinforcing signal strength"; it never promotes a DAMPEN
// into a REINFORCE. That direction matters: the criteria are cheap boolean
// events that noise satisfies regularly, and a classifier that could be talked
// into REINFORCE by one of them would be exactly the search-that-learns-from-
// noise this phase is most at risk of.
//
// # Three departures from A.4's pseudocode, all in the conservative direction
//
//  1. There is a FOURTH label. A.4's function returns DAMPEN when it cannot
//     conclude REINFORCE, which collapses "the system recovered" and "there was
//     no data" into one answer. A clean world and an unevaluable one would then
//     be indistinguishable, and every utility ranking built on top would be
//     meaningless. INSUFFICIENT_DATA is not DAMPEN and never contributes a
//     reinforcing signal.
//
//  2. ABSENT IS NOT ZERO. internal/telemetry deliberately keeps an unobservable
//     metric absent rather than reporting 0. An absent point is not in Points
//     at all, and a window whose observed points are stretched across a long
//     absence is INSUFFICIENT_DATA rather than a cliff.
//
//  3. A SLOPE MUST BEAT ITS OWN NOISE. A.4 tests `trend > 0`. On a flat, noisy
//     series that is true about half the time, so the unguarded rule reports
//     REINFORCE on a healthy system at coin-flip odds. The fitted slope must
//     additionally exceed NoiseSigma standard errors of itself.
//     TestNoisyFlatSeriesIsDampenAndTheGuardIsWhy pins a series on which the
//     unguarded rule WOULD have fired, so the guard cannot be removed quietly.
//
// The regression's x-axis is TIME IN SECONDS, not the sample index. Index-based
// regression across a gap in the samples fabricates steepness: two readings ten
// seconds apart are treated as adjacent, and a metric that drifted slowly while
// nobody was looking arrives as a cliff.

// SpiralClass is A.4's classification, plus the fourth label described above.
type SpiralClass int

const (
	// SpiralInsufficientData means the series could not be fitted: too few
	// observed points, or the observed points do not cover the window. It is
	// NOT DAMPEN and it is not a claim about the system.
	SpiralInsufficientData SpiralClass = iota
	// SpiralDampen means the system absorbed the fault: no significant upward
	// trend.
	SpiralDampen
	// SpiralReinforceLinear means the system is getting worse at a steady rate.
	SpiralReinforceLinear
	// SpiralReinforceAccelerating means the system is getting worse, faster.
	// A.4: this triggers immediate escalation to Tier 2.
	SpiralReinforceAccelerating
)

var spiralNames = [...]string{"INSUFFICIENT_DATA", "DAMPEN", "REINFORCE_LINEAR", "REINFORCE_ACCELERATING"}

func (c SpiralClass) String() string {
	if c < 0 || int(c) >= len(spiralNames) {
		return fmt.Sprintf("SpiralClass(%d)", int(c))
	}
	return spiralNames[c]
}

// Reinforcing reports whether the class is one of the two REINFORCE labels.
func (c SpiralClass) Reinforcing() bool {
	return c == SpiralReinforceLinear || c == SpiralReinforceAccelerating
}

// Evaluable reports whether the class is a claim about the system at all.
// SpiralInsufficientData is not.
func (c SpiralClass) Evaluable() bool { return c != SpiralInsufficientData }

// Metric names.
//
// MetricQueueDepth is bound to telemetry's own constant so the two cannot drift.
// The other two are NAMED BUT NOT COLLECTED: internal/telemetry v1 exposes no
// retry counter and no driver error rate, so A.3's second and fourth criteria
// are UNEVALUABLE on this stack. They are reported as unknown, never as false:
// "the criterion did not hold" and "nothing could measure it" are different
// facts, and only one of them supports a DAMPEN.
const (
	MetricQueueDepth = telemetry.MetricStatusQueue
	MetricRetryCount = "driver.retry_count"
	MetricErrorRate  = "driver.error_rate"
)

// Point is one observed sample of one metric.
type Point struct {
	// TMS is the VIRTUAL millisecond the sample was taken at.
	TMS int64
	// Value is the observed value. A Point exists only when the metric was
	// actually observed; see Absence for the other case.
	Value float64
}

// Absence is a sampling slot at which the metric was NOT observed, and why.
//
// It exists so absence can be counted and explained without ever being turned
// into a number.
type Absence struct {
	TMS    int64
	Reason string
}

// Series is one metric on one node over time.
type Series struct {
	Metric string
	Node   string
	// Points are the OBSERVED samples. Absent samples are not here.
	Points []Point
	// Absent are the sampling slots at which the metric was unobservable.
	Absent []Absence
}

// ObserveOptions is the classifier's tuning, from `search.observe` plus one
// additive knob.
type ObserveOptions struct {
	// SpiralWindow is A.4's windowSize=5: the number of samples in the trend
	// regression window. From search.observe.spiral_window.
	SpiralWindow int
	// AccelWindow is A.4's windowSize=3 for the second derivative. A.10 exposes
	// no key for it, so it is not configurable from prothesis.yaml.
	AccelWindow int
	// ReinforceThreshold is the minimum trend slope, IN UNITS PER SECOND, that
	// can be called REINFORCE. From search.observe.reinforce_threshold; A.10's
	// default is 0.0, so any significant positive slope qualifies.
	ReinforceThreshold float64
	// SampleIntervalMS is the expected spacing between samples: 500 ms by
	// default (search.observe.sample_interval_ms), 200 ms for the probe-specific
	// trajectory rate A.3 asks for. It is used only to detect a window stretched
	// across an absence; 0 disables that check.
	SampleIntervalMS int
	// MaxWindowStretch bounds how much wider than SampleIntervalMS*(W-1) a
	// trend window's time span may be before the window is treated as
	// INSUFFICIENT_DATA. ADDITIVE: A.10 has no key for it. Without it, a metric
	// that was unobservable for ten seconds and came back higher fits a
	// perfectly respectable upward line through two clusters of points and the
	// search chases a ghost.
	MaxWindowStretch float64
	// NoiseSigma is how many standard errors of itself a fitted slope must
	// exceed to count as a signal. ADDITIVE: A.10 has no key for it. 0 disables
	// the guard and restores A.4's literal `trend > 0`.
	//
	// Measured on 200 realizations of a flat metric plus +/-4 units of noise,
	// sampled 20 times at the probe rate:
	//
	//	unguarded (NoiseSigma 0)   REINFORCE on 103/200  (51.5%)
	//	guarded   (NoiseSigma 2)   REINFORCE on  13/200  ( 6.5%)
	//	                           of which ACCELERATING  8/200  ( 4.0%)
	//
	// The residual 4% is the acceleration test's own one-sided alpha and is not
	// hidden: an escalation policy that cannot afford one spurious spiral in 25
	// should require corroboration across metrics or nodes before spending Tier
	// 2's budget. That is escalate.go's decision, not this classifier's.
	NoiseSigma float64
}

// Probe sampling rate. A.3 asks for 200 ms during DRIVE for probe worlds;
// A.4 and A.10 say 500 ms for general telemetry. They describe different things
// (OQ-006 item 2), so both exist.
const ProbeSampleIntervalMS = 200

// Additive defaults, chosen and justified rather than tuned.
const (
	// DefaultAccelWindow is A.4's literal windowSize=3.
	DefaultAccelWindow = 3
	// DefaultNoiseSigma is two standard errors: the conventional one-sided ~95%
	// bar. High enough that sensor noise does not launch a Tier 2 escalation,
	// low enough that a genuine spiral clears it immediately.
	DefaultNoiseSigma = 2.0
	// DefaultMaxWindowStretch allows a window to be twice its nominal span
	// before it is refused, which tolerates one missed sample and refuses a gap.
	DefaultMaxWindowStretch = 2.0
)

// DefaultObserveOptions returns A.10's defaults plus the additive knobs.
func DefaultObserveOptions() ObserveOptions {
	return ObserveOptions{
		SpiralWindow:       schema.DefaultSpiralWindow,
		AccelWindow:        DefaultAccelWindow,
		ReinforceThreshold: schema.DefaultReinforceThreshold,
		SampleIntervalMS:   schema.DefaultSampleIntervalMS,
		MaxWindowStretch:   DefaultMaxWindowStretch,
		NoiseSigma:         DefaultNoiseSigma,
	}
}

// ObserveOptionsFrom builds the classifier's tuning from `search.observe`,
// filling the additive knobs with their defaults.
func ObserveOptionsFrom(o schema.ObserveConfig) ObserveOptions {
	out := DefaultObserveOptions()
	if o.SpiralWindow > 0 {
		out.SpiralWindow = o.SpiralWindow
	}
	if o.SampleIntervalMS > 0 {
		out.SampleIntervalMS = o.SampleIntervalMS
	}
	out.ReinforceThreshold = o.ReinforceThreshold
	return out
}

// ForProbe returns the options with A.3's 200 ms trajectory rate.
func (o ObserveOptions) ForProbe() ObserveOptions {
	o.SampleIntervalMS = ProbeSampleIntervalMS
	return o
}

func (o ObserveOptions) filled() ObserveOptions {
	if o.SpiralWindow < 2 {
		o.SpiralWindow = schema.DefaultSpiralWindow
	}
	if o.AccelWindow < 2 {
		o.AccelWindow = DefaultAccelWindow
	}
	if o.MaxWindowStretch <= 0 {
		o.MaxWindowStretch = DefaultMaxWindowStretch
	}
	if o.NoiseSigma < 0 {
		o.NoiseSigma = 0
	}
	return o
}

// Classification is one metric series' verdict, with the arithmetic that
// produced it.
type Classification struct {
	Metric string
	Node   string
	Class  SpiralClass
	Reason string

	// Observed and AbsentCount describe the input. AbsentCount > 0 with a low
	// Observed is what distinguishes "the system was quiet" from "nothing could
	// be measured".
	Observed    int
	AbsentCount int

	// Trend is the fitted slope of the most recent window, in units per SECOND.
	Trend      float64
	TrendSE    float64
	TrendT     float64 // |Trend| / TrendSE: dimensionless, comparable across metrics
	TrendKnown bool

	// Accel is the second derivative, in units per second squared, from the
	// quadratic fit described on ClassifySignal.
	Accel      float64
	AccelSE    float64
	AccelT     float64
	AccelKnown bool
	// AccelSpanMS is the time the acceleration fit covers.
	AccelSpanMS int64

	// NoiseChecked is false when the window was too small to estimate a
	// standard error (a regression over 2 points has zero residual degrees of
	// freedom). The classification then rests on the slope alone, which is
	// A.4's literal behaviour, and saying so is the point of the field.
	NoiseChecked bool

	// WindowSpanMS is the time the trailing trend window actually covers.
	WindowSpanMS int64
}

// ClassifySignal is A.4's ClassifySignal, with the three departures documented
// at the top of this file and one more described here.
//
// # Acceleration: one quadratic fit, not a slope of slopes
//
// A.4 computes the second derivative as `linearRegression(trends,
// windowSize=3)`; a line through the slopes of three sliding windows. Those
// three windows share three of their five samples with each other, so their
// slopes are strongly correlated, their residuals are not a noise estimate, and
// a line through three points has ONE residual degree of freedom. The
// construction cannot separate curvature from noise.
//
// That is not a theoretical objection. Measured on this implementation before
// the change: of 200 flat, pure-noise series, 13 were classified
// REINFORCE_ACCELERATING (every one of the false positives went straight to
// the label that triggers a Tier 2 escalation) and a genuinely LINEAR noisy
// rise was also called ACCELERATING (accel 14.4 +/- 5.8).
//
// So the acceleration is obtained from a single least-squares QUADRATIC fit
// over the same span the two-stage version covers, SpiralWindow + AccelWindow -
// 1 samples, and reported as its second derivative 2c. Same quantity, same
// span, honest degrees of freedom (n-3), and a standard error that means what
// it says. AccelWindow keeps its A.4 meaning (the number of trend windows the
// acceleration looks back over) it just sets the span rather than a second
// regression's point count.
func ClassifySignal(s Series, opt ObserveOptions) Classification {
	opt = opt.filled()
	c := Classification{
		Metric:      s.Metric,
		Node:        s.Node,
		Observed:    len(s.Points),
		AbsentCount: len(s.Absent),
	}

	pts := make([]Point, len(s.Points))
	copy(pts, s.Points)
	sort.SliceStable(pts, func(i, j int) bool { return pts[i].TMS < pts[j].TMS })

	w := opt.SpiralWindow
	if len(pts) < w {
		c.Class = SpiralInsufficientData
		c.Reason = fmt.Sprintf("%d observed sample(s), and a trend regression needs %d; "+
			"%d sampling slot(s) were absent%s. This is NOT a DAMPEN: nothing was measured",
			len(pts), w, len(s.Absent), absenceReasonSuffix(s.Absent))
		return c
	}

	// The trailing window must be a contiguous stretch of observation, not two
	// clusters straddling an absence.
	tail := pts[len(pts)-w:]
	c.WindowSpanMS = tail[len(tail)-1].TMS - tail[0].TMS
	if opt.SampleIntervalMS > 0 {
		nominal := float64(opt.SampleIntervalMS) * float64(w-1)
		if nominal > 0 && float64(c.WindowSpanMS) > opt.MaxWindowStretch*nominal {
			c.Class = SpiralInsufficientData
			c.Reason = fmt.Sprintf("the trailing %d-sample window spans %dms against a nominal "+
				"%.0fms at a %dms sampling interval, so the samples straddle a gap in observation; "+
				"fitting a line across it would invent a trend%s",
				w, c.WindowSpanMS, nominal, opt.SampleIntervalMS, absenceReasonSuffix(s.Absent))
			return c
		}
	}

	// First derivative: A.4's linear regression over the trailing window.
	last := regress(tail)
	if !last.ok {
		c.Class = SpiralInsufficientData
		c.Reason = "the trailing window's samples share a single timestamp, so the slope is " +
			"undefined; zero would read as `flat`, which is a claim the data cannot make"
		return c
	}

	c.Trend = last.slope
	c.TrendSE = last.se
	c.TrendKnown = true
	c.NoiseChecked = last.seKnown
	if last.seKnown && last.se > 0 {
		c.TrendT = math.Abs(last.slope) / last.se
	}

	// Second derivative. See the "acceleration" note on ClassifySignal for why
	// this is one quadratic fit rather than A.4's slope-of-slopes.
	span := w + opt.AccelWindow - 1
	if len(pts) >= span {
		atail := pts[len(pts)-span:]
		c.AccelSpanMS = atail[len(atail)-1].TMS - atail[0].TMS
		stretched := false
		if opt.SampleIntervalMS > 0 {
			nominal := float64(opt.SampleIntervalMS) * float64(span-1)
			stretched = nominal > 0 && float64(c.AccelSpanMS) > opt.MaxWindowStretch*nominal
		}
		if !stretched {
			if af := quadFit(atail); af.ok {
				c.Accel = af.slope
				c.AccelSE = af.se
				c.AccelKnown = true
				if af.seKnown && af.se > 0 {
					c.AccelT = math.Abs(af.slope) / af.se
				}
				c.Class, c.Reason = decide(c, opt, af)
				return c
			}
		}
	}

	c.Class, c.Reason = decide(c, opt, fit{})
	return c
}

// decide applies A.4's two comparisons, with the noise guard.
func decide(c Classification, opt ObserveOptions, af fit) (SpiralClass, string) {
	rising := c.Trend > opt.ReinforceThreshold
	if !rising {
		return SpiralDampen, fmt.Sprintf("trend %+.4g/s does not exceed the reinforce threshold "+
			"%+.4g/s: the system is stable or recovering", c.Trend, opt.ReinforceThreshold)
	}
	if !significant(c.Trend, fit{slope: c.Trend, se: c.TrendSE, seKnown: c.NoiseChecked, ok: true}, opt.NoiseSigma) {
		return SpiralDampen, fmt.Sprintf("trend %+.4g/s is positive but within %.3g standard "+
			"errors of zero (SE %.4g): indistinguishable from sampling noise, so it is not a signal",
			c.Trend, opt.NoiseSigma, c.TrendSE)
	}
	if !c.AccelKnown {
		return SpiralReinforceLinear, fmt.Sprintf("trend %+.4g/s is a significant rise; there were "+
			"too few trend windows to fit an acceleration, so the weaker of the two REINFORCE "+
			"labels is reported rather than the stronger one", c.Trend)
	}
	if af.slope > 0 && significant(af.slope, af, opt.NoiseSigma) {
		return SpiralReinforceAccelerating, fmt.Sprintf("trend %+.4g/s and acceleration %+.4g/s^2 "+
			"are both significant rises: the system is getting worse, faster", c.Trend, c.Accel)
	}
	return SpiralReinforceLinear, fmt.Sprintf("trend %+.4g/s is a significant rise; acceleration "+
		"%+.4g/s^2 is not: the system is getting worse at a steady rate", c.Trend, c.Accel)
}

// significant reports whether a fitted slope beats its own noise.
//
// When the standard error could not be estimated (a regression over two points
// has zero residual degrees of freedom) there is nothing to test against and
// the slope stands on its own, which is A.4's literal behaviour. That case is
// surfaced as Classification.NoiseChecked = false rather than being hidden.
func significant(v float64, f fit, sigma float64) bool {
	if sigma <= 0 || !f.seKnown {
		return v != 0
	}
	if f.se == 0 {
		// A perfect fit: every residual is zero, so the slope is exactly what
		// the data say and there is no noise to clear.
		return v != 0
	}
	return math.Abs(v) > sigma*f.se
}

func absenceReasonSuffix(a []Absence) string {
	if len(a) == 0 {
		return ""
	}
	// The first recorded reason, which is the one an operator needs; the rest
	// are almost always the same reason repeated.
	return fmt.Sprintf(" (first reason: %s)", a[0].Reason)
}

// ---------------------------------------------------------------------------
// least squares
// ---------------------------------------------------------------------------

// fit is a simple linear regression of y on x.
type fit struct {
	slope float64
	se    float64
	// seKnown is false when there were too few points to estimate residual
	// variance (n < 3).
	seKnown bool
	ok      bool
}

func regress(pts []Point) fit {
	xs := make([]float64, len(pts))
	ys := make([]float64, len(pts))
	for i, p := range pts {
		// Seconds, not milliseconds and not the sample index. Slopes are then
		// per-second and comparable regardless of sampling rate, and a gap in
		// the samples widens the x-axis instead of being silently closed up.
		xs[i] = float64(p.TMS) / 1000.0
		ys[i] = p.Value
	}
	return regressXY(xs, ys)
}

func regressXY(xs, ys []float64) fit {
	n := len(xs)
	if n < 2 || n != len(ys) {
		return fit{}
	}
	var meanX, meanY float64
	for i := 0; i < n; i++ {
		meanX += xs[i]
		meanY += ys[i]
	}
	meanX /= float64(n)
	meanY /= float64(n)

	var sxx, sxy float64
	for i := 0; i < n; i++ {
		dx := xs[i] - meanX
		sxx += dx * dx
		sxy += dx * (ys[i] - meanY)
	}
	if sxx == 0 {
		// Every x identical: the slope is undefined, not zero. Returning zero
		// here would read as "flat" and would be a claim the data cannot make.
		return fit{}
	}
	slope := sxy / sxx
	intercept := meanY - slope*meanX

	f := fit{slope: slope, ok: true}
	if n >= 3 {
		var rss float64
		for i := 0; i < n; i++ {
			r := ys[i] - (intercept + slope*xs[i])
			rss += r * r
		}
		f.se = math.Sqrt(rss / (float64(n-2) * sxx))
		f.seKnown = true
	}
	return f
}

// quadFit fits y = a + b*u + c*u^2 by least squares, where u is time in seconds
// centred on the window, and returns the SECOND DERIVATIVE 2c with its standard
// error.
//
// x is centred only for conditioning: over a window at t = 8200..9400 ms an
// uncentred u^2 term reaches 88, and the normal equations lose precision for no
// reason. Centring shifts a and b and leaves c (the only coefficient read here)
// unchanged.
func quadFit(pts []Point) fit {
	n := len(pts)
	if n < 3 {
		return fit{}
	}
	var mean float64
	for _, p := range pts {
		mean += float64(p.TMS) / 1000.0
	}
	mean /= float64(n)

	var s1, s2, s3, s4, ty, tuy, tu2y float64
	us := make([]float64, n)
	for i, p := range pts {
		u := float64(p.TMS)/1000.0 - mean
		us[i] = u
		u2 := u * u
		s1 += u
		s2 += u2
		s3 += u2 * u
		s4 += u2 * u2
		ty += p.Value
		tuy += u * p.Value
		tu2y += u2 * p.Value
	}
	s0 := float64(n)

	// Normal equations M * [a b c]' = rhs.
	det := s0*(s2*s4-s3*s3) - s1*(s1*s4-s3*s2) + s2*(s1*s3-s2*s2)
	if det == 0 || math.IsNaN(det) {
		// Degenerate: fewer than three distinct timestamps, so no curvature is
		// identifiable. Not an acceleration of zero: no acceleration at all.
		return fit{}
	}
	// Cramer's rule for c, replacing the third column with rhs.
	detC := s0*(s2*tu2y-s3*tuy) - s1*(s1*tu2y-s3*ty) + ty*(s1*s3-s2*s2)
	c := detC / det

	// a and b, needed for the residuals.
	detA := ty*(s2*s4-s3*s3) - s1*(tuy*s4-s3*tu2y) + s2*(tuy*s3-s2*tu2y)
	detB := s0*(tuy*s4-s3*tu2y) - ty*(s1*s4-s3*s2) + s2*(s1*tu2y-tuy*s2)
	a := detA / det
	b := detB / det

	f := fit{slope: 2 * c, ok: true}
	if n >= 4 {
		var rss float64
		for i, p := range pts {
			r := p.Value - (a + b*us[i] + c*us[i]*us[i])
			rss += r * r
		}
		// var(c) = sigma^2 * (M^-1)[2][2], and (M^-1)[2][2] = (s0*s2 - s1^2)/det.
		inv22 := (s0*s2 - s1*s1) / det
		sigma2 := rss / float64(n-3)
		if v := sigma2 * inv22; v > 0 {
			f.se = 2 * math.Sqrt(v)
		}
		f.seKnown = true
	}
	return f
}

// ---------------------------------------------------------------------------
// A.3's four criteria, as evidence
// ---------------------------------------------------------------------------

// Window is a fault's virtual-time window.
type Window struct{ StartMS, EndMS int64 }

// DurationMS is the window length.
func (w Window) DurationMS() int64 { return w.EndMS - w.StartMS }

// RoleChange is one observed role transition (leader election, failover).
type RoleChange struct {
	TMS  int64
	From string
	To   string
}

// Baseline is a pre-fault reference value for one metric.
type Baseline struct {
	Metric string
	Value  float64
	// Known is false when no baseline could be established. Every one of A.3's
	// criteria is comparative ("returned to baseline", "exceeded 2x baseline",
	// "got worse after the fault ended") so a missing baseline makes them
	// unevaluable, which is why A.3's sweep needs the no-fault CONTROL world it
	// drops (D-028 item 6).
	Known bool
}

// Observation is everything OBSERVE consumes about one node in one world.
type Observation struct {
	Node  string
	Fault Window
	// Metrics are the observed series. Order is the caller's; classification
	// does not depend on it beyond reporting.
	Metrics []Series
	// Baselines are the control-world reference values.
	Baselines []Baseline
	// RoleChanges are the observed transitions. RolesObserved distinguishes
	// "the role never changed" from "role was never observable", which for a
	// paused container is the common case.
	RoleChanges   []RoleChange
	RolesObserved bool

	// SampleIntervalMS is the rate these series were ACTUALLY collected at, or
	// 0 when the caller does not know.
	//
	// ADDITIVE (OQ-039). It exists because assuming the rate is how the
	// classifier silently refused to classify anything. A.3 asks for a 200 ms
	// trajectory during a probe world, and ObserveOptions.ForProbe honours that
	// by setting SampleIntervalMS to 200, but the harness's telemetry collector
	// samples at `search.observe.sample_interval_ms`, which defaults to 500. The
	// window-stretch guard then compared a real 3500 ms seven-sample span
	// against a nominal 1200 ms and reported INSUFFICIENT_DATA for EVERY metric
	// on EVERY probe. Measured: 21 of 21 probes in run r_2026_09_08_e363
	// classified INSUFFICIENT_DATA on a fixture that reinforces.
	//
	// telemetry.Document carries `interval_ms` for exactly this reason (its own
	// doc comment says "an oracle reasoning about '3 consecutive samples' needs
	// to know what a sample is worth in wall time") so the honest fix is to
	// read the rate rather than assume it. A caller that leaves this zero keeps
	// whatever ObserveOptions says.
	SampleIntervalMS int64
}

// CriterionName identifies one of A.3's four REINFORCE criteria.
type CriterionName string

const (
	// CritQueueMonotonic: "queue depth monotonically increased for >= 3
	// consecutive samples after fault withdrawal".
	CritQueueMonotonic CriterionName = "queue_depth_monotonic_after_withdrawal"
	// CritRetryOverBaseline: "retry count exceeded 2x baseline during or after
	// the fault window".
	CritRetryOverBaseline CriterionName = "retry_count_over_2x_baseline"
	// CritRoleChange: "a role change occurred within 1 second of fault
	// injection". Carries NO weight; see the Weighted field.
	CritRoleChange CriterionName = "role_change_within_1s"
	// CritErrorRateAfter: "error rate increased after fault withdrawal".
	CritErrorRateAfter CriterionName = "error_rate_rose_after_withdrawal"
)

// MinMonotonicRun is A.3's ">= 3 consecutive samples".
const MinMonotonicRun = 3

// RoleChangeWindowMS is A.3's "within 1 second of fault injection".
const RoleChangeWindowMS = 1000

// Criterion is one of A.3's four booleans, with the two things a bare boolean
// cannot carry: whether it could be evaluated at all, and whether it counts.
type Criterion struct {
	Name CriterionName
	// Known is false when nothing in the observation could evaluate it. It is
	// then neither met nor unmet.
	Known bool
	Met   bool
	// Weighted reports whether the criterion contributes to the ranking score.
	// Exactly one criterion does not: see EvaluateCriteria.
	Weighted bool
	Detail   string
}

// EvaluateCriteria computes A.3's four criteria over one observation.
//
// # Why the role-change criterion carries no weight (OQ-006 item 4)
//
// A.3 counts "a role change (leader election, failover) occurred within 1 second
// of fault injection" as evidence of REINFORCE. On the mandatory fixture it is
// a CONSTANT, not a signal: pausing a Raft leader always causes an election, in
// a correct implementation exactly as much as in a buggy one. Measured on this
// fixture, the pause alone moved the term from 63 to 64 inside the pause window
// (D-031): on the code path that behaves correctly.
//
// A criterion that fires identically on healthy and unhealthy systems carries no
// information, and giving it weight would spend Tier 2's expensive rollouts on
// worlds selected by a coin that always lands the same way up. It is kept and
// reported, because "the leader changed here" is the right place for a human or
// a later stage to start looking, and it is scored at zero.
func EvaluateCriteria(obs Observation, opt ObserveOptions) []Criterion {
	opt = opt.filled()
	out := make([]Criterion, 0, 4)
	out = append(out, queueMonotonicCriterion(obs, opt))
	out = append(out, ratioCriterion(obs, CritRetryOverBaseline, MetricRetryCount, 2.0,
		"internal/telemetry v1 collects no retry counter"))
	out = append(out, roleChangeCriterion(obs))
	out = append(out, errorRateAfterCriterion(obs))
	return out
}

func (obs Observation) series(metric string) (Series, bool) {
	for _, s := range obs.Metrics {
		if s.Metric == metric {
			return s, true
		}
	}
	return Series{}, false
}

func (obs Observation) baseline(metric string) Baseline {
	for _, b := range obs.Baselines {
		if b.Metric == metric {
			return b
		}
	}
	return Baseline{Metric: metric}
}

// after returns the observed points strictly after the fault window closed,
// sorted by time.
func after(s Series, endMS int64) []Point {
	out := make([]Point, 0, len(s.Points))
	for _, p := range s.Points {
		if p.TMS > endMS {
			out = append(out, p)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].TMS < out[j].TMS })
	return out
}

// queueMonotonicCriterion implements A.3's "queue depth monotonically increased
// for >= 3 consecutive samples after fault withdrawal".
//
// CONSECUTIVE is enforced in TIME, not merely in slice position. Absent samples
// are not in Points, so two readings either side of a ten-second blackout sit
// adjacent in the slice; counting them as a consecutive increase would build a
// "monotonic run" out of a gap in observation. A step whose spacing exceeds the
// sampling interval by more than MaxWindowStretch breaks the run instead.
func queueMonotonicCriterion(obs Observation, opt ObserveOptions) Criterion {
	c := Criterion{Name: CritQueueMonotonic, Weighted: true}
	s, ok := obs.series(MetricQueueDepth)
	if !ok {
		c.Detail = "no " + MetricQueueDepth + " series was observed"
		return c
	}
	pts := after(s, obs.Fault.EndMS)
	// A run of 3 consecutive increases needs 4 points.
	if len(pts) < MinMonotonicRun+1 {
		c.Detail = fmt.Sprintf("%d observed sample(s) after withdrawal at t+%dms, and a run of %d "+
			"consecutive increases needs %d%s",
			len(pts), obs.Fault.EndMS, MinMonotonicRun, MinMonotonicRun+1, absenceReasonSuffix(s.Absent))
		return c
	}
	maxGapMS := int64(0)
	if opt.SampleIntervalMS > 0 {
		maxGapMS = int64(opt.MaxWindowStretch * float64(opt.SampleIntervalMS))
	}
	c.Known = true
	best, run, broken := 0, 0, 0
	for i := 1; i < len(pts); i++ {
		if maxGapMS > 0 && pts[i].TMS-pts[i-1].TMS > maxGapMS {
			run = 0
			broken++
			continue
		}
		if pts[i].Value > pts[i-1].Value {
			run++
			if run > best {
				best = run
			}
			continue
		}
		run = 0
	}
	c.Met = best >= MinMonotonicRun
	c.Detail = fmt.Sprintf("longest run of consecutive increases after withdrawal: %d (threshold %d), "+
		"over %d observed sample(s); %d step(s) crossed a gap in observation and broke the run",
		best, MinMonotonicRun, len(pts), broken)
	return c
}

// ratioCriterion implements "metric exceeded N x baseline during or after the
// fault window".
func ratioCriterion(obs Observation, name CriterionName, metric string, factor float64, absentNote string) Criterion {
	c := Criterion{Name: name, Weighted: true}
	s, ok := obs.series(metric)
	if !ok {
		c.Detail = fmt.Sprintf("no %s series was observed: %s, so this criterion is UNEVALUABLE "+
			"on this stack rather than false", metric, absentNote)
		return c
	}
	b := obs.baseline(metric)
	if !b.Known {
		c.Detail = fmt.Sprintf("no baseline for %s; every A.3 criterion is comparative, so without "+
			"the no-fault control world there is nothing to compare against (D-028 item 6)", metric)
		return c
	}
	var peak float64
	seen := false
	for _, p := range s.Points {
		if p.TMS < obs.Fault.StartMS {
			continue
		}
		if !seen || p.Value > peak {
			peak, seen = p.Value, true
		}
	}
	if !seen {
		c.Detail = fmt.Sprintf("no %s sample during or after the fault window%s", metric, absenceReasonSuffix(s.Absent))
		return c
	}
	c.Known = true
	c.Met = peak > factor*b.Value
	c.Detail = fmt.Sprintf("peak %.4g during or after the fault against a baseline of %.4g "+
		"(threshold %.4g x)", peak, b.Value, factor)
	return c
}

func roleChangeCriterion(obs Observation) Criterion {
	// Weighted is FALSE, deliberately and permanently. See EvaluateCriteria.
	c := Criterion{Name: CritRoleChange, Weighted: false}
	if !obs.RolesObserved {
		c.Detail = "role was never observable on this node (a paused container answers nothing), " +
			"so a role change can be neither confirmed nor ruled out"
		return c
	}
	c.Known = true
	lo, hi := obs.Fault.StartMS, obs.Fault.StartMS+RoleChangeWindowMS
	for _, rc := range obs.RoleChanges {
		if rc.TMS >= lo && rc.TMS <= hi {
			c.Met = true
			c.Detail = fmt.Sprintf("role changed %s -> %s at t+%dms, within %dms of injection at "+
				"t+%dms. UNWEIGHTED: pausing a leader elects a new one in a correct implementation "+
				"too, so this is a place to look, not evidence of a defect",
				rc.From, rc.To, rc.TMS, RoleChangeWindowMS, obs.Fault.StartMS)
			return c
		}
	}
	c.Detail = fmt.Sprintf("no role change within %dms of injection at t+%dms", RoleChangeWindowMS, obs.Fault.StartMS)
	return c
}

func errorRateAfterCriterion(obs Observation) Criterion {
	c := Criterion{Name: CritErrorRateAfter, Weighted: true}
	s, ok := obs.series(MetricErrorRate)
	if !ok {
		c.Detail = "no " + MetricErrorRate + " series was observed: internal/telemetry v1 collects " +
			"no driver error rate, so this criterion is UNEVALUABLE on this stack rather than false"
		return c
	}
	var during, post []Point
	for _, p := range s.Points {
		switch {
		case p.TMS >= obs.Fault.StartMS && p.TMS <= obs.Fault.EndMS:
			during = append(during, p)
		case p.TMS > obs.Fault.EndMS:
			post = append(post, p)
		}
	}
	if len(during) == 0 || len(post) == 0 {
		c.Detail = fmt.Sprintf("%d sample(s) during the fault and %d after it; both sides are "+
			"needed to say whether the system got worse after the fault ended", len(during), len(post))
		return c
	}
	c.Known = true
	dm := mean(during)
	pm := mean(post)
	c.Met = pm > dm
	c.Detail = fmt.Sprintf("mean error rate %.4g during the fault, %.4g after withdrawal", dm, pm)
	return c
}

func mean(pts []Point) float64 {
	if len(pts) == 0 {
		return 0
	}
	var sum float64
	for _, p := range pts {
		sum += p.Value
	}
	return sum / float64(len(pts))
}

// ---------------------------------------------------------------------------
// the combined result
// ---------------------------------------------------------------------------

// Result is OBSERVE's answer for one node.
type Result struct {
	Node string
	// Class is the worst EVALUABLE class across the metrics. When no metric
	// could be evaluated it is SpiralInsufficientData, never DAMPEN.
	Class   SpiralClass
	Reason  string
	Metrics []Classification
	// Criteria are A.3's four, as evidence. They order the ranked list; they
	// never change Class.
	Criteria []Criterion
	// Rank is the "reinforcing signal strength" A.3 orders its output by.
	Rank float64
}

// Escalate reports A.4's trigger: "a REINFORCE_ACCELERATING classification on
// any metric triggers immediate escalation to Tier 2".
func (r Result) Escalate() bool { return r.Class == SpiralReinforceAccelerating }

// Classify runs the A.4 classifier over every metric and attaches A.3's
// evidence.
//
// # The ranking formula, and why it is shaped this way
//
//	Rank = 100 * class_score + 5 * weighted_criteria_met + min(10, max_trend_t)
//
// The class dominates by two orders of magnitude, so evidence and trend strength
// only ever order worlds WITHIN a class: a DAMPEN can never outrank a
// REINFORCE_LINEAR however many booleans it satisfies. The trend term is the
// t-statistic rather than the raw slope because slopes carry units: adding a
// queue depth in items per second to an RSS in bytes per second would rank by
// whichever metric happened to have the larger units.
func Classify(obs Observation, opt ObserveOptions) Result {
	opt = opt.filled()
	r := Result{Node: obs.Node, Class: SpiralInsufficientData}
	r.Metrics = make([]Classification, 0, len(obs.Metrics))

	var worstIdx = -1
	var maxT float64
	for _, s := range obs.Metrics {
		c := ClassifySignal(s, opt)
		r.Metrics = append(r.Metrics, c)
		if !c.Class.Evaluable() {
			continue
		}
		if c.TrendT > maxT {
			maxT = c.TrendT
		}
		if worstIdx < 0 || c.Class > r.Metrics[worstIdx].Class {
			worstIdx = len(r.Metrics) - 1
		}
	}
	if worstIdx >= 0 {
		r.Class = r.Metrics[worstIdx].Class
		r.Reason = fmt.Sprintf("%s on %s: %s", r.Class, r.Metrics[worstIdx].Metric, r.Metrics[worstIdx].Reason)
	} else {
		r.Reason = fmt.Sprintf("no metric could be classified across %d series; this is not a "+
			"DAMPEN and it must not be scored as one", len(obs.Metrics))
	}

	r.Criteria = EvaluateCriteria(obs, opt)
	met := 0
	for _, c := range r.Criteria {
		if c.Weighted && c.Known && c.Met {
			met++
		}
	}
	r.Rank = 100*float64(r.Class) + 5*float64(met) + math.Min(10, maxT)
	return r
}

// ---------------------------------------------------------------------------
// building a Series from telemetry, without ever turning absence into zero
// ---------------------------------------------------------------------------

// SeriesFrom projects one metric out of a node's telemetry samples.
//
// extract returns (value, true) when the metric was observed. A sample for which
// it returns false becomes an Absence carrying the reason telemetry recorded,
// never a zero, which is the property internal/telemetry's whole pointer-typed
// design exists to preserve, and which a consumer can undo in one careless line.
func SeriesFrom(samples []*telemetry.Sample, node, metric string,
	extract func(*telemetry.Sample) (int64, bool)) Series {
	s := Series{Metric: metric, Node: node}
	for _, sm := range samples {
		if sm == nil || (node != "" && sm.Node != node) {
			continue
		}
		if sm.VMS == nil {
			// No virtual timestamp: the sample cannot be placed on the axis the
			// fault windows are expressed in. Recorded as an absence rather than
			// dropped silently.
			s.Absent = append(s.Absent, Absence{Reason: "sample carries no virtual timestamp"})
			continue
		}
		tms := *sm.VMS
		v, ok := extract(sm)
		if !ok {
			reason, has := sm.AbsentReason(metric)
			if !has {
				reason = "metric not observed"
			}
			s.Absent = append(s.Absent, Absence{TMS: tms, Reason: reason})
			continue
		}
		s.Points = append(s.Points, Point{TMS: tms, Value: float64(v)})
	}
	return s
}

// QueueDepthSeries projects status.queue_depth for one node.
func QueueDepthSeries(samples []*telemetry.Sample, node string) Series {
	return SeriesFrom(samples, node, MetricQueueDepth,
		func(s *telemetry.Sample) (int64, bool) { return s.QueueDepth() })
}

// RSSSeries projects memory.rss_bytes for one node.
func RSSSeries(samples []*telemetry.Sample, node string) Series {
	return SeriesFrom(samples, node, telemetry.MetricMemoryRSS,
		func(s *telemetry.Sample) (int64, bool) { return s.RSS() })
}
