package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// `thesis diagnose` used to refuse (exit 5) a project with no known-problem
// registry, so a target that had catalogued nothing yet could not be diagnosed
// or clustered at all. It now diagnoses against an empty registry and says so;
// a registry that exists and does not decode is still a config error.
func TestDiagnoseFallsBackToAnEmptyRegistry(t *testing.T) {
	dir := t.TempDir()
	reg, note, code := loadKnownProblems(dir)
	if reg == nil || code != schema.ExitPass || len(reg.Problems) != 0 || reg.Validate() != nil {
		t.Fatalf("no registry: reg %+v code %d; want a valid empty registry", reg, code)
	}
	if !strings.Contains(note, "empty") {
		t.Fatalf("the fallback is not announced: %q", note)
	}

	p := filepath.Join(dir, ".prothesis", "known-problems.yaml")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("problems: [unclosed"), 0o644); err != nil {
		t.Fatal(err)
	}
	if reg, _, code := loadKnownProblems(dir); reg != nil || code != schema.ExitConfigError {
		t.Fatalf("a malformed registry: reg %+v code %d; want exit 5", reg, code)
	}
}

func TestDiagnoseReadsCalibrationRecords(t *testing.T) {
	p := filepath.Join(t.TempDir(), "calibration_report.json")
	doc := `{"schema":"` + schema.CalibrationReportSchema + `","arms":[
	  {"arm":"GP-01","records":[{"arm":"GP-01","run_id":"r_2026_10_21_0301","expected":"PASS","observed":"FAIL"}]},
	  {"arm":"BN-01","records":[{"arm":"BN-01","run_id":"r_2026_10_21_0302","expected":"FAIL","observed":"FAIL"}]}]}`
	if err := os.WriteFile(p, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	recs, err := readCalibrationRecords(p)
	if err != nil || len(recs) != 2 || recs[0].RunID != "r_2026_10_21_0301" || recs[0].Expected != schema.VerdictPass {
		t.Fatalf("records %+v, err %v", recs, err)
	}
	if err := os.WriteFile(p, []byte(`{"schema":"something/else"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readCalibrationRecords(p); err == nil {
		t.Fatal("a document that is not a calibration report was read as one")
	}
}
