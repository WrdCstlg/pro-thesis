package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

const calibrationManifest = `schema: eval_manifest/v1
arms:
  - id: BUG-01
    target: proj
    control: positive
    profile: linear
    expected: 1
    target_oracle: linearizable.kv
  - id: OK-01
    target: proj
    control: negative
    profile: linear
    expected: 0
  - id: LATER-01
    target: undecided
    control: positive
`

// calibrationRepo lays out a repository with the manifest and one verdict per
// (arm, verdict, violating oracle) triple, and returns the manifest path.
func calibrationRepo(t *testing.T, runs ...[3]string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "eval"), 0o755); err != nil {
		t.Fatal(err)
	}
	mp := filepath.Join(root, "eval", "manifest.yaml")
	if err := os.WriteFile(mp, []byte(calibrationManifest), 0o644); err != nil {
		t.Fatal(err)
	}
	for i, r := range runs {
		id := fmt.Sprintf("r_2026_10_21_%04x", i+1)
		dir := filepath.Join(root, "proj", ".prothesis", "runs", id)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		v := schema.Verdict{RunID: id, Profile: "linear", Commit: "abc1234", Arm: r[0],
			Verdict: schema.VerdictResult(r[1]), Violations: []schema.Violation{}}
		if r[2] != "" {
			v.Violations = append(v.Violations, schema.Violation{ID: "v1", Oracle: r[2], Class: schema.ClassConsistency})
		}
		v.Normalize()
		b, _ := json.MarshalIndent(v, "", "  ")
		if err := os.WriteFile(filepath.Join(dir, "verdict.json"), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return mp
}

func readReport(t *testing.T, manifest string) schema.CalibrationReport {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(manifest), "calibration", "calibration_report.json"))
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	var rep schema.CalibrationReport
	if err := json.Unmarshal(raw, &rep); err != nil {
		t.Fatalf("parse report: %v", err)
	}
	return rep
}

func TestReportWritesTheCalibrationAsJSONAndMarkdown(t *testing.T) {
	mp := calibrationRepo(t,
		[3]string{"BUG-01", "FAIL", "linearizable.kv"},
		[3]string{"BUG-01", "FAIL", "no_stuck_op"},
		[3]string{"OK-01", "PASS", ""},
		[3]string{"OK-01", "PASS", ""},
	)
	if code := cmdReport(context.Background(), globals{quiet: true}, []string{"--manifest", mp}); code != schema.ExitPass {
		t.Fatalf("report exit %d, want 0", code)
	}
	rep := readReport(t, mp)
	if rep.Aggregate.TrueReject != 1 || rep.Aggregate.Escape != 1 || rep.Aggregate.TruePass != 2 {
		t.Fatalf("aggregate %+v: the FAIL raised only by no_stuck_op must be an escape of linearizable.kv", rep.Aggregate)
	}
	md, err := os.ReadFile(filepath.Join(filepath.Dir(mp), "calibration", "calibration_report.md"))
	if err != nil {
		t.Fatalf("markdown: %v", err)
	}
	for _, want := range []string{"| BUG-01 |", "| OK-01 |", "ESCAPE", "LATER-01", "0.042"} {
		if !strings.Contains(string(md), want) {
			t.Fatalf("the markdown report lacks %q:\n%s", want, md)
		}
	}
}

func TestGateFailsOnAnEscape(t *testing.T) {
	mp := calibrationRepo(t, [3]string{"BUG-01", "PASS", ""}, [3]string{"OK-01", "PASS", ""})
	if code := cmdGate(context.Background(), globals{quiet: true}, []string{"--manifest", mp}); code != schema.ExitFail {
		t.Fatalf("gate exit %d with a buggy-negative arm that passed, want 1", code)
	}
	if rep := readReport(t, mp); len(rep.Breaches) == 0 {
		t.Fatal("the report does not say why the gate failed")
	}
}

func TestGatePassesACleanCalibration(t *testing.T) {
	mp := calibrationRepo(t, [3]string{"BUG-01", "FAIL", "linearizable.kv"}, [3]string{"OK-01", "PASS", ""})
	if code := cmdGate(context.Background(), globals{quiet: true}, []string{"--manifest", mp}); code != schema.ExitPass {
		t.Fatalf("gate exit %d, want 0", code)
	}
}

func TestGateEnforcesItsThresholds(t *testing.T) {
	mp := calibrationRepo(t,
		[3]string{"BUG-01", "FAIL", "linearizable.kv"},
		[3]string{"OK-01", "PASS", ""}, [3]string{"OK-01", "FAIL", "linearizable.kv"},
	)
	args := []string{"--manifest", mp, "--min-specificity", "0.9"}
	if code := cmdGate(context.Background(), globals{quiet: true}, args); code != schema.ExitFail {
		t.Fatalf("specificity 0.5 under a 0.9 floor: gate exit %d, want 1", code)
	}
	args = []string{"--manifest", mp, "--min-ppv", "0.9", "--ppv-base-rate", "0.042"}
	if code := cmdGate(context.Background(), globals{quiet: true}, args); code != schema.ExitFail {
		t.Fatalf("a PPV below its floor: gate exit %d, want 1", code)
	}
}

// `thesis run --arm` takes its pole, target oracle and profile from the
// manifest, and refuses to run an arm somewhere it does not belong: a verdict
// filed under the wrong arm would be classified against the wrong pole.
func TestRunCalibrationResolvesAnArmAndRefusesAMisfiledOne(t *testing.T) {
	mp := calibrationRepo(t)
	root := filepath.Dir(filepath.Dir(mp))
	proj := filepath.Join(root, "proj")

	profile := "smoke"
	cal, code := runCalibration(proj, "BUG-01", mp, "", "", &profile, false)
	if code != schema.ExitPass || cal == nil {
		t.Fatalf("exit %d", code)
	}
	if cal.Expected != schema.VerdictFail || cal.TargetOracle != "linearizable.kv" || profile != "linear" {
		t.Fatalf("calibration %+v, profile %q: want FAIL, linearizable.kv, and the arm's profile linear", cal, profile)
	}

	for name, call := range map[string]func() schema.ExitCode{
		"another project": func() schema.ExitCode {
			p := "linear"
			_, c := runCalibration(filepath.Join(root, "elsewhere"), "BUG-01", mp, "", "", &p, false)
			return c
		},
		"another profile": func() schema.ExitCode {
			p := "smoke"
			_, c := runCalibration(proj, "BUG-01", mp, "", "", &p, true)
			return c
		},
		"a contradicting --expect": func() schema.ExitCode {
			p := "linear"
			_, c := runCalibration(proj, "BUG-01", mp, "PASS", "", &p, false)
			return c
		},
		"an arm with no pole": func() schema.ExitCode {
			p := "linear"
			_, c := runCalibration(proj, "LATER-01", mp, "", "", &p, false)
			return c
		},
		"an unknown arm": func() schema.ExitCode {
			p := "linear"
			_, c := runCalibration(proj, "NOPE", mp, "", "", &p, false)
			return c
		},
	} {
		if code := call(); code != schema.ExitConfigError {
			t.Errorf("%s: exit %d, want 5", name, code)
		}
	}
}

// No decided run, no claim: a gate with nothing measured is not a pass.
func TestGateWithNothingMeasuredIsInconclusive(t *testing.T) {
	mp := calibrationRepo(t)
	if code := cmdGate(context.Background(), globals{quiet: true}, []string{"--manifest", mp}); code != schema.ExitInconclusive {
		t.Fatalf("gate exit %d with no runs, want 2", code)
	}
}
