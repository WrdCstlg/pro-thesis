package schema

import (
	"bytes"
	"errors"
	"fmt"
	"sort"

	"gopkg.in/yaml.v3"
)

// Two-sided gate calibration (D-109).
//
// A gate is calibrated against arms whose correct answer is known in advance:
// a gold-positive arm runs a system that is correct, so the right answer is
// PASS; a buggy-negative arm runs one that is known to violate the checked
// property, so the right answer is FAIL. Each arm run is classified against its
// pole, and the counts give the gate's sensitivity (it catches what is there)
// and specificity (it passes what is clean). The two together, with an assumed
// base rate of defective builds, give the probability that a FAIL is real (PPV).
//
// The arms live in the evaluation suite's manifest (eval/manifest.yaml,
// eval_manifest/v1), whose `expected` is the exit code an arm is designed to
// produce: 0 is the gold-positive pole, 1 the buggy-negative pole. The suite's
// own `control` vocabulary (negative control = correct system, positive control
// = known defect) is kept beside the pole on every record.

const (
	// CalibrationSchema identifies one arm run's classification.
	CalibrationSchema = "prothesis.calibration/v1"
	// CalibrationReportSchema identifies `thesis report` / `thesis gate` output.
	CalibrationReportSchema = "prothesis.calibration_report/v1"
	// EvalManifestSchema is the evaluation suite manifest the arms come from.
	EvalManifestSchema = "eval_manifest/v1"
)

// ControlType is an arm's pole.
type ControlType string

const (
	// ControlGoldPositive is a correct system: the right answer is PASS.
	ControlGoldPositive ControlType = "gold_positive"
	// ControlBuggyNegative is a system known to violate the checked property:
	// the right answer is FAIL.
	ControlBuggyNegative ControlType = "buggy_negative"
)

// ControlTypeFor is the pole whose right answer is expected.
func ControlTypeFor(expected VerdictResult) ControlType {
	if expected == VerdictFail {
		return ControlBuggyNegative
	}
	return ControlGoldPositive
}

// CalibrationClass is one arm run against its pole.
type CalibrationClass string

const (
	ClassTruePass       CalibrationClass = "TRUE_PASS"       // expected PASS, got PASS
	ClassTrueReject     CalibrationClass = "TRUE_REJECT"     // expected FAIL, got FAIL
	ClassEscape         CalibrationClass = "ESCAPE"          // expected FAIL, got PASS
	ClassFalseRejection CalibrationClass = "FALSE_REJECTION" // expected PASS, got FAIL
	// ClassUndecided is any run whose verdict was neither PASS nor FAIL, or
	// whose expectation was not a pole. It enters no rate's denominator.
	ClassUndecided CalibrationClass = "INCONCLUSIVE"
)

// Classify places one verdict against an expectation.
func Classify(expected, observed VerdictResult) CalibrationClass {
	switch {
	case expected == VerdictPass && observed == VerdictPass:
		return ClassTruePass
	case expected == VerdictPass && observed == VerdictFail:
		return ClassFalseRejection
	case expected == VerdictFail && observed == VerdictFail:
		return ClassTrueReject
	case expected == VerdictFail && observed == VerdictPass:
		return ClassEscape
	default:
		return ClassUndecided
	}
}

// ClassifyVerdict is Classify with the target oracle rule. A buggy-negative arm
// may name the oracle that must catch its defect: a FAIL raised only by other
// oracles did not show that oracle catching it, so it is an ESCAPE of the target,
// not a TRUE_REJECT. With no target, any violation counts.
func ClassifyVerdict(expected VerdictResult, v *Verdict, targetOracle string) CalibrationClass {
	if v == nil {
		return ClassUndecided
	}
	c := Classify(expected, v.Verdict)
	if c == ClassTrueReject && targetOracle != "" {
		for _, vi := range v.Violations {
			if vi.Oracle == targetOracle {
				return ClassTrueReject
			}
		}
		return ClassEscape
	}
	return c
}

// CalibrationOutcome is the verdict's `calibration` block: the run's
// classification against its declared expectation (D-109, the dual-pole
// fold).
type CalibrationOutcome struct {
	Expected     VerdictResult    `json:"expected"`
	TargetOracle string           `json:"target_oracle,omitempty"`
	Class        CalibrationClass `json:"class"`
}

// ExitCode is the calibrated exit code: 0 when the gate answered as the pole
// requires, 1 when it did not (an escape or a false rejection), and the
// verdict's own code when the run was not decided.
func (c CalibrationOutcome) ExitCode(observed VerdictResult) ExitCode {
	switch c.Class {
	case ClassTruePass, ClassTrueReject:
		return ExitPass
	case ClassEscape, ClassFalseRejection:
		return ExitFail
	default:
		if observed == VerdictPass || observed == VerdictFail {
			return ExitInconclusive
		}
		return observed.ExitCode()
	}
}

// CalibrationRecord is one arm run's classification.
type CalibrationRecord struct {
	Schema       string           `json:"schema"`
	Arm          string           `json:"arm"`
	Control      string           `json:"control,omitempty"`
	ControlType  ControlType      `json:"control_type"`
	Expected     VerdictResult    `json:"expected"`
	Observed     VerdictResult    `json:"observed"`
	Class        CalibrationClass `json:"class"`
	TargetOracle string           `json:"target_oracle,omitempty"`
	RunID        string           `json:"run_id"`
	VerdictPath  string           `json:"verdict_path"`
	Seed         *uint64          `json:"seed,omitempty"`
	Commit       string           `json:"commit"`
}

// CalibrationCounts are the denominators.
type CalibrationCounts struct {
	Runs           int `json:"runs"`
	TruePass       int `json:"true_pass"`
	TrueReject     int `json:"true_reject"`
	Escape         int `json:"escape"`
	FalseRejection int `json:"false_rejection"`
	Undecided      int `json:"inconclusive"`
}

// Add counts one classified run.
func (c *CalibrationCounts) Add(class CalibrationClass) {
	c.Runs++
	switch class {
	case ClassTruePass:
		c.TruePass++
	case ClassTrueReject:
		c.TrueReject++
	case ClassEscape:
		c.Escape++
	case ClassFalseRejection:
		c.FalseRejection++
	default:
		c.Undecided++
	}
}

// Merge adds another set of counts.
func (c *CalibrationCounts) Merge(o CalibrationCounts) {
	c.Runs += o.Runs
	c.TruePass += o.TruePass
	c.TrueReject += o.TrueReject
	c.Escape += o.Escape
	c.FalseRejection += o.FalseRejection
	c.Undecided += o.Undecided
}

// CalibrationRates are the gate's measured rates. A rate whose denominator is
// zero is null: no runs at a pole is not a rate of zero, it is no measurement.
type CalibrationRates struct {
	// Sensitivity is TRUE_REJECT / (TRUE_REJECT + ESCAPE).
	Sensitivity *float64 `json:"sensitivity"`
	// Specificity is TRUE_PASS / (TRUE_PASS + FALSE_REJECTION).
	Specificity *float64 `json:"specificity"`
	// FalseRejectionRate is 1 - Specificity.
	FalseRejectionRate *float64 `json:"false_rejection_rate"`
}

// Rates computes the rates from the counts.
func (c CalibrationCounts) Rates() CalibrationRates {
	var r CalibrationRates
	if n := c.TrueReject + c.Escape; n > 0 {
		se := float64(c.TrueReject) / float64(n)
		r.Sensitivity = &se
	}
	if n := c.TruePass + c.FalseRejection; n > 0 {
		sp := float64(c.TruePass) / float64(n)
		fr := 1 - sp
		r.Specificity, r.FalseRejectionRate = &sp, &fr
	}
	return r
}

// DefaultBaseRates are the assumed fractions of defective builds the PPV is
// reported at.
var DefaultBaseRates = []float64{0.20, 0.10, 0.042, 0.01}

// PPVPoint is the positive predictive value of a FAIL at one base rate.
type PPVPoint struct {
	BaseRate float64  `json:"base_rate"`
	PPV      *float64 `json:"ppv"`
}

// PPVByBaseRate is Se*b / (Se*b + (1-Sp)*(1-b)) at each base rate b: the
// probability that a FAIL is a real defect when a fraction b of builds are
// defective. Null when either rate is unmeasured.
func PPVByBaseRate(r CalibrationRates, rates []float64) []PPVPoint {
	out := make([]PPVPoint, 0, len(rates))
	for _, b := range rates {
		p := PPVPoint{BaseRate: b}
		if r.Sensitivity != nil && r.Specificity != nil {
			tp := *r.Sensitivity * b
			fp := (1 - *r.Specificity) * (1 - b)
			if tp+fp > 0 {
				v := tp / (tp + fp)
				p.PPV = &v
			}
		}
		out = append(out, p)
	}
	return out
}

// EvalManifest is the part of eval/manifest.yaml calibration reads.
type EvalManifest struct {
	Schema string    `yaml:"schema"`
	Arms   []EvalArm `yaml:"arms"`
}

// EvalArm is one arm row. Keys calibration does not read are ignored.
type EvalArm struct {
	ID      string `yaml:"id"`
	Target  string `yaml:"target"`
	Control string `yaml:"control"`
	Variant string `yaml:"variant"`
	Profile string `yaml:"profile"`
	// Expected is the exit code the arm is designed to produce; nil when the
	// manifest gives none.
	Expected *int `yaml:"expected"`
	// TargetOracle is the oracle that must catch a buggy-negative arm's
	// defect, when the arm names one.
	TargetOracle string `yaml:"target_oracle"`
}

// ExpectedVerdict is the arm's pole: exit 0 is PASS, exit 1 is FAIL. Any other
// value, or none, is not a pole and the arm cannot be calibrated.
func (a EvalArm) ExpectedVerdict() (VerdictResult, bool) {
	if a.Expected == nil {
		return "", false
	}
	switch *a.Expected {
	case int(ExitPass):
		return VerdictPass, true
	case int(ExitFail):
		return VerdictFail, true
	}
	return "", false
}

// Arm looks an arm up by id.
func (m *EvalManifest) Arm(id string) (EvalArm, bool) {
	for _, a := range m.Arms {
		if a.ID == id {
			return a, true
		}
	}
	return EvalArm{}, false
}

// ParseEvalManifest decodes eval/manifest.yaml.
func ParseEvalManifest(data []byte) (*EvalManifest, error) {
	var m EvalManifest
	if err := yaml.NewDecoder(bytes.NewReader(data)).Decode(&m); err != nil {
		return nil, fmt.Errorf("eval manifest: %w", err)
	}
	if m.Schema != EvalManifestSchema {
		return nil, fmt.Errorf("eval manifest: schema is %q, want %q", m.Schema, EvalManifestSchema)
	}
	seen := map[string]bool{}
	for i, a := range m.Arms {
		if a.ID == "" {
			return nil, fmt.Errorf("eval manifest: arms[%d] has no id", i)
		}
		if seen[a.ID] {
			return nil, fmt.Errorf("eval manifest: arm %q appears twice", a.ID)
		}
		seen[a.ID] = true
	}
	if len(m.Arms) == 0 {
		return nil, errors.New("eval manifest: no arms")
	}
	return &m, nil
}

// LocatedVerdict is a verdict read from disk, with where it came from.
type LocatedVerdict struct {
	Path    string
	Verdict Verdict
}

// ArmCalibration is one arm's runs and rates.
type ArmCalibration struct {
	Arm          string              `json:"arm"`
	Target       string              `json:"target"`
	Profile      string              `json:"profile"`
	Control      string              `json:"control"`
	ControlType  ControlType         `json:"control_type"`
	Expected     VerdictResult       `json:"expected"`
	TargetOracle string              `json:"target_oracle,omitempty"`
	Counts       CalibrationCounts   `json:"counts"`
	Rates        CalibrationRates    `json:"rates"`
	Records      []CalibrationRecord `json:"records"`
}

// CalibrationReport is the calibration of every arm the manifest can calibrate.
type CalibrationReport struct {
	Schema    string            `json:"schema"`
	Manifest  string            `json:"manifest"`
	Arms      []ArmCalibration  `json:"arms"`
	Aggregate CalibrationCounts `json:"aggregate"`
	Rates     CalibrationRates  `json:"rates"`
	PPV       []PPVPoint        `json:"ppv_by_base_rate"`
	// Uncalibrated names each arm that has no PASS/FAIL pole, with the reason.
	Uncalibrated []string `json:"uncalibrated_arms"`
	// Unmatched lists verdicts that name an arm the manifest does not have.
	Unmatched []string `json:"unmatched_verdicts"`
	// Thresholds and Breaches are filled by `thesis gate`.
	Thresholds *GateThresholds `json:"thresholds,omitempty"`
	Breaches   []string        `json:"breaches"`
}

// GateThresholds are `thesis gate`'s pass conditions beyond "no escape".
type GateThresholds struct {
	MinSpecificity *float64 `json:"min_specificity,omitempty"`
	MinPPV         *float64 `json:"min_ppv,omitempty"`
	PPVBaseRate    float64  `json:"ppv_base_rate,omitempty"`
}

// HasEscape reports whether any run escaped.
func (r CalibrationReport) HasEscape() bool { return r.Aggregate.Escape > 0 }

// BuildCalibrationReport joins the manifest's arms with the verdicts that name
// them (Verdict.Arm). The manifest's expectation is the truth: it is the
// pre-registered pole, and a verdict cannot move it.
func BuildCalibrationReport(manifestPath string, m *EvalManifest, verdicts []LocatedVerdict, baseRates []float64) CalibrationReport {
	rep := CalibrationReport{
		Schema: CalibrationReportSchema, Manifest: manifestPath,
		Arms: []ArmCalibration{}, Uncalibrated: []string{}, Unmatched: []string{}, Breaches: []string{},
	}
	byArm := map[string][]LocatedVerdict{}
	for _, lv := range verdicts {
		if lv.Verdict.Arm == "" {
			continue
		}
		if _, ok := m.Arm(lv.Verdict.Arm); !ok {
			rep.Unmatched = append(rep.Unmatched, lv.Path)
			continue
		}
		byArm[lv.Verdict.Arm] = append(byArm[lv.Verdict.Arm], lv)
	}
	for _, a := range m.Arms {
		exp, ok := a.ExpectedVerdict()
		if !ok {
			why := "no expected outcome"
			if a.Expected != nil {
				why = fmt.Sprintf("expected exit %d is not a PASS/FAIL pole", *a.Expected)
			}
			rep.Uncalibrated = append(rep.Uncalibrated, a.ID+": "+why)
			continue
		}
		ac := ArmCalibration{
			Arm: a.ID, Target: a.Target, Profile: a.Profile, Control: a.Control,
			ControlType: ControlTypeFor(exp), Expected: exp, TargetOracle: a.TargetOracle,
			Records: []CalibrationRecord{},
		}
		runs := byArm[a.ID]
		sort.SliceStable(runs, func(i, j int) bool { return runs[i].Verdict.RunID < runs[j].Verdict.RunID })
		for _, lv := range runs {
			v := lv.Verdict
			class := ClassifyVerdict(exp, &v, a.TargetOracle)
			ac.Counts.Add(class)
			ac.Records = append(ac.Records, CalibrationRecord{
				Schema: CalibrationSchema, Arm: a.ID, Control: a.Control, ControlType: ac.ControlType,
				Expected: exp, Observed: v.Verdict, Class: class, TargetOracle: a.TargetOracle,
				RunID: v.RunID, VerdictPath: lv.Path, Seed: v.Seed, Commit: v.Commit,
			})
		}
		ac.Rates = ac.Counts.Rates()
		rep.Aggregate.Merge(ac.Counts)
		rep.Arms = append(rep.Arms, ac)
	}
	rep.Rates = rep.Aggregate.Rates()
	rep.PPV = PPVByBaseRate(rep.Rates, baseRates)
	return rep
}
