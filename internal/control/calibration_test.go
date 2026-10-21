package control

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// verdictOracles answers with a fixed finding per oracle name.
type verdictOracles struct{ results []OracleResult }

func (v verdictOracles) Evaluate(context.Context, EvalRequest) ([]OracleResult, error) {
	return append([]OracleResult(nil), v.results...), nil
}

func violatedBy(oracle string) verdictOracles {
	return verdictOracles{results: []OracleResult{{Output: schema.OracleOutput{
		Oracle: oracle, Class: schema.ClassConsistency, Status: schema.StatusViolated,
		Explanation: "planted defect caught",
	}}}}
}

func calibratedRun(t *testing.T, engine OracleEngine, cal *Calibration) (schema.Verdict, schema.ExitCode) {
	t.Helper()
	r, _ := newPerturbTestRunner(t, perturbTestConfig(t), nil)
	r.oracleEngine = engine
	r.opts.Calibration = cal
	v, exit, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return v, exit
}

func TestEveryVerdictCarriesItsRunSeed(t *testing.T) {
	r, _ := newPerturbTestRunner(t, perturbTestConfig(t), nil)
	r.seed = 4242
	v, _, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if v.Seed == nil || *v.Seed != 4242 {
		t.Fatalf("verdict seed = %v, want the run seed 4242", v.Seed)
	}
	raw, err := os.ReadFile(filepath.Join(r.runDir, VerdictFileName))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["seed"] == nil {
		t.Fatal("verdict.json does not record the run seed")
	}
}

// The dual-pole fold. A run made for a calibration arm is judged against its
// pole: the exit code says whether the GATE answered as it should, and
// verdict.verdict still says what the system did.
func TestTheDualPoleFold(t *testing.T) {
	cases := []struct {
		name      string
		engine    OracleEngine
		cal       Calibration
		verdict   schema.VerdictResult
		class     schema.CalibrationClass
		exit      schema.ExitCode
		poleLabel schema.ControlType
	}{
		{"buggy build caught by the target oracle", violatedBy("linearizable.kv"),
			Calibration{Arm: "HC-01", Expected: schema.VerdictFail, TargetOracle: "linearizable.kv"},
			schema.VerdictFail, schema.ClassTrueReject, schema.ExitPass, schema.ControlBuggyNegative},
		{"buggy build not caught", stubOracles{},
			Calibration{Arm: "HC-01", Expected: schema.VerdictFail, TargetOracle: "linearizable.kv"},
			schema.VerdictPass, schema.ClassEscape, schema.ExitFail, schema.ControlBuggyNegative},
		{"buggy build caught only by another oracle", violatedBy("no_stuck_op"),
			Calibration{Arm: "HC-01", Expected: schema.VerdictFail, TargetOracle: "linearizable.kv"},
			schema.VerdictFail, schema.ClassEscape, schema.ExitFail, schema.ControlBuggyNegative},
		{"correct build passed", stubOracles{},
			Calibration{Arm: "HC-02", Expected: schema.VerdictPass},
			schema.VerdictPass, schema.ClassTruePass, schema.ExitPass, schema.ControlGoldPositive},
		{"correct build rejected", violatedBy("linearizable.kv"),
			Calibration{Arm: "HC-02", Expected: schema.VerdictPass},
			schema.VerdictFail, schema.ClassFalseRejection, schema.ExitFail, schema.ControlGoldPositive},
		{"nothing judged", noFindings{},
			Calibration{Arm: "HC-02", Expected: schema.VerdictPass},
			schema.VerdictInconclusive, schema.ClassUndecided, schema.ExitInconclusive, schema.ControlGoldPositive},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cal := tc.cal
			v, exit := calibratedRun(t, tc.engine, &cal)
			if v.Verdict != tc.verdict {
				t.Fatalf("verdict %s, want %s: the system's verdict is unchanged by calibration", v.Verdict, tc.verdict)
			}
			if v.Calibration == nil || v.Calibration.Class != tc.class {
				t.Fatalf("calibration %+v, want class %s", v.Calibration, tc.class)
			}
			if exit != tc.exit {
				t.Fatalf("exit %d, want %d", exit, tc.exit)
			}
			if v.Arm != cal.Arm || v.Expected != cal.Expected || v.ControlType != tc.poleLabel {
				t.Fatalf("arm %q, expected %q, control_type %q", v.Arm, v.Expected, v.ControlType)
			}
		})
	}
}

// Without an expectation nothing changes: the exit code is the verdict's.
func TestAnUncalibratedRunKeepsItsExitCode(t *testing.T) {
	v, exit := calibratedRun(t, violatedBy("linearizable.kv"), nil)
	if exit != schema.ExitFail || v.Calibration != nil || v.Expected != "" {
		t.Fatalf("exit %d, calibration %+v: an ordinary run was calibrated", exit, v.Calibration)
	}
}
