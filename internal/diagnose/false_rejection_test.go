package diagnose

import (
	"encoding/json"
	"math"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// A run expected to PASS that came back FAIL used to be skipped as terminal, so
// a gold-positive arm's false rejections never reached attribution or the
// cluster pool. Each is now attributed (its violating worlds, or the run when
// it has none) and the unexplained share of each arm's decided gold-positive
// runs is computed over the calibration report's denominator.

const frRegistry = testRegistry + `
  - id: KP-005
    title: witness key drift on lease reads
    classification: defect-open
    ledger: OQ-054
    level: world
    match:
      false_rejection: true
      oracle: linearizable.kv
      reason_regex: 'witness key drift'
  - id: KP-006
    title: arm known to falsely reject
    classification: defect-open
    ledger: OQ-054
    level: bundle
    match:
      false_rejection: true
      arm: GP-flaky
  - id: KP-007
    title: node killed by the OOM killer
    classification: operational
    ledger: OQ-071
    level: world
    match:
      oracle: no_crash
      reason_regex: 'exited 137'
`

func frFixture(t *testing.T) (string, *schema.KnownProblems) {
	t.Helper()
	reg, err := schema.DecodeKnownProblems([]byte(frRegistry))
	if err != nil {
		t.Fatal(err)
	}
	runs := t.TempDir()
	violated := func(oracle, why string) string {
		return `{"world":1,"outcome":"violation","oracles":[{"oracle":"` + oracle +
			`","status":"violated","explanation":"` + why + `"},{"oracle":"no_stuck_op","status":"ok"}]}`
	}
	// Explained: the violation matches a false_rejection entry.
	b := mkBundle(t, runs, "r_2026_10_21_0101")
	writeFile(t, filepath.Join(b, "verdict.json"), `{"verdict":"FAIL","arm":"GP-01","expected":"PASS"}`)
	writeFile(t, filepath.Join(b, "world-0001", "result.json"), violated("linearizable.kv", "witness key drift on k/3"))
	// Unexplained: only an ordinary entry (KP-007) matches, and an ordinary
	// entry never explains a false rejection.
	b = mkBundle(t, runs, "r_2026_10_21_0102")
	writeFile(t, filepath.Join(b, "verdict.json"), `{"verdict":"FAIL","arm":"GP-01","expected":"PASS"}`)
	writeFile(t, filepath.Join(b, "world-0001", "result.json"), violated("no_crash", "kv-n2 exited 137"))
	// A true pass: in the denominator, nothing to attribute.
	b = mkBundle(t, runs, "r_2026_10_21_0103")
	writeFile(t, filepath.Join(b, "verdict.json"), `{"verdict":"PASS","arm":"GP-01","expected":"PASS"}`)
	writeFile(t, filepath.Join(b, "world-0001", "result.json"), `{"world":1,"outcome":"pass","oracles":[]}`)
	// Undecided: attributed as before, outside the denominator.
	b = mkBundle(t, runs, "r_2026_10_21_0104")
	writeFile(t, filepath.Join(b, "verdict.json"), `{"verdict":"INCONCLUSIVE","arm":"GP-01","expected":"PASS","budget":{"narrowed":true}}`)
	writeFile(t, filepath.Join(b, "world-0001", "result.json"), `{"world":1,"outcome":"pass","oracles":[]}`)
	// No expectation: a FAIL is terminal, as it always was.
	b = mkBundle(t, runs, "r_2026_10_21_0105")
	writeFile(t, filepath.Join(b, "verdict.json"), `{"verdict":"FAIL"}`)
	writeFile(t, filepath.Join(b, "world-0001", "result.json"), violated("no_crash", "kv-n2 exited 137"))
	// A false rejection with no violating world is the run's own item.
	b = mkBundle(t, runs, "r_2026_10_21_0106")
	writeFile(t, filepath.Join(b, "verdict.json"), `{"verdict":"FAIL","arm":"GP-flaky","expected":"PASS"}`)
	// The verdict names no expectation; the calibration record does.
	b = mkBundle(t, runs, "r_2026_10_21_0107")
	writeFile(t, filepath.Join(b, "verdict.json"), `{"verdict":"FAIL"}`)
	writeFile(t, filepath.Join(b, "world-0001", "result.json"), violated("linearizable.kv", "lost write"))
	return runs, reg
}

var frOptions = Options{Calibration: []schema.CalibrationRecord{
	{RunID: "r_2026_10_21_0107", Arm: "GP-02", Expected: schema.VerdictPass},
}}

func TestFalseRejectionsAreAttributedPerArm(t *testing.T) {
	runs, reg := frFixture(t)
	rep, err := DiagnoseWith(runs, reg, frOptions)
	if err != nil {
		t.Fatalf("DiagnoseWith: %v", err)
	}

	var fr []string
	for _, u := range rep.Unattributed {
		if u.Class != ClassFalseRejection {
			t.Errorf("unexpected unattributed item %+v", u)
			continue
		}
		fr = append(fr, u.Bundle)
	}
	if !reflect.DeepEqual(fr, []string{"r_2026_10_21_0102", "r_2026_10_21_0107"}) {
		t.Fatalf("unexplained false rejections %v, want 0102 (only an ordinary entry matched) and 0107 "+
			"(expected PASS per its calibration record)", fr)
	}

	counts := map[string]int{}
	for _, c := range rep.Counts {
		counts[c.ID] = c.N
	}
	if counts["KP-005"] != 1 || counts["KP-006"] != 1 || counts["KP-007"] != 0 || counts["KP-003"] != 1 {
		t.Fatalf("counts %v: want KP-005 and KP-006 to explain one false rejection each, KP-007 none, KP-003 the narrowed run", counts)
	}

	want := []FalseRejectionArm{
		{Arm: "GP-01", Denominator: 3, FalseRejections: 2, Unexplained: 1},
		{Arm: "GP-02", Denominator: 1, FalseRejections: 1, Unexplained: 1},
		{Arm: "GP-flaky", Denominator: 1, FalseRejections: 1, Unexplained: 0},
	}
	if len(rep.FalseRejections) != len(want) {
		t.Fatalf("per-arm tally %+v, want %+v", rep.FalseRejections, want)
	}
	for i, w := range want {
		g := rep.FalseRejections[i]
		wantPct := 100 * float64(w.Unexplained) / float64(w.Denominator)
		if g.Arm != w.Arm || g.Denominator != w.Denominator || g.FalseRejections != w.FalseRejections ||
			g.Unexplained != w.Unexplained || g.UnexplainedPct == nil || math.Abs(*g.UnexplainedPct-wantPct) > 1e-9 {
			t.Errorf("arm %d: %+v, want %+v at %.4f%%", i, g, w, wantPct)
		}
	}

	// Scan, which feeds the cluster pool, sees the same items and the same tally.
	bundles, err := ScanWith(runs, reg, frOptions)
	if err != nil {
		t.Fatalf("ScanWith: %v", err)
	}
	pooled := 0
	for _, b := range bundles {
		for _, w := range b.Worlds {
			if w.Unattributed {
				pooled++
				if !w.FalseRejection {
					t.Errorf("%s/%s unattributed but not marked a false rejection", b.Name, w.Name)
				}
			}
		}
		if b.Unattributed {
			pooled++
		}
	}
	if pooled != len(rep.Unattributed) {
		t.Fatalf("Scan pools %d items, Diagnose reports %d unattributed", pooled, len(rep.Unattributed))
	}
	if got := FalseRejectionRates(bundles); !reflect.DeepEqual(got, rep.FalseRejections) {
		t.Fatalf("Scan's tally %+v disagrees with Diagnose's %+v", got, rep.FalseRejections)
	}
}

// The report is a JSON document with stable snake_case keys, so --out and
// --json are a contract rather than Go field names.
func TestTheDiagnosisReportHasJSONKeys(t *testing.T) {
	runs, reg := frFixture(t)
	rep, err := DiagnoseWith(runs, reg, frOptions)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{`"bundles":`, `"worlds":`, `"counts":[{"id":"KP-003"`, `"unattributed":[`,
		`"class":"FALSE_REJECTION"`, `"false_rejections":[{"arm":"GP-01","denominator":3`, `"unexplained_pct":`} {
		if !strings.Contains(string(b), k) {
			t.Errorf("report JSON lacks %s:\n%s", k, b)
		}
	}
	empty, err := Diagnose(t.TempDir(), &schema.KnownProblems{Schema: schema.KnownProblemsSchema})
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := json.Marshal(empty); !strings.Contains(string(b), `"unattributed":[]`) || !strings.Contains(string(b), `"false_rejections":[]`) {
		t.Errorf("an empty report emits null lists: %s", b)
	}
}
