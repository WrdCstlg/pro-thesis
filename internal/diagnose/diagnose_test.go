package diagnose

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

const testRegistry = `
schema: prothesis.knownproblems/v1
problems:
  - id: KP-001
    title: rtb has no usable baseline
    classification: accepted-refusal
    ledger: OQ-072
    level: world
    match:
      oracle: resource_return_to_baseline
      reason_regex: 'no pre-DRIVE baseline'
  - id: KP-002
    title: world died before ASSERT
    classification: defect-open
    ledger: OQ-071
    level: world
    match:
      no_result: true
  - id: KP-003
    title: narrowed downgrade
    classification: by-design
    ledger: D-059
    level: bundle
    match:
      verdict: INCONCLUSIVE
      narrowed: true
  - id: KP-004
    title: folded-up refusals
    classification: by-design
    ledger: OQ-072
    level: bundle
    match:
      verdict: INCONCLUSIVE
`

func loadRegistry(t *testing.T) *schema.KnownProblems {
	t.Helper()
	reg, err := schema.DecodeKnownProblems([]byte(testRegistry))
	if err != nil {
		t.Fatalf("decode test registry: %v", err)
	}
	if err := reg.Validate(); err != nil {
		t.Fatalf("validate test registry: %v", err)
	}
	return reg
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mkBundle(t *testing.T, runsDir, name string) string {
	t.Helper()
	dir := filepath.Join(runsDir, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// A pass world and a violation world are terminal and need no attribution.
func TestTerminalOutcomesNeedNoAttribution(t *testing.T) {
	runsDir := t.TempDir()
	b := mkBundle(t, runsDir, "r_2026_09_21_aaaa")
	writeFile(t, filepath.Join(b, "verdict.json"), `{"schema":"prothesis.verdict/v1","verdict":"PASS"}`)
	writeFile(t, filepath.Join(b, "world-0001", "result.json"), `{"world":1,"seed":7,"outcome":"pass"}`)
	writeFile(t, filepath.Join(b, "world-0002", "result.json"), `{"world":2,"seed":8,"outcome":"violation"}`)

	rep, err := Diagnose(runsDir, loadRegistry(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Unattributed) != 0 {
		t.Fatalf("terminal outcomes must not need attribution, got %v", rep.Unattributed)
	}
	if len(rep.Counts) != 0 {
		t.Fatalf("no known problem should have fired, got %v", rep.Counts)
	}
}

// An inconclusive world whose oracle reason matches KP-001 attributes to it.
func TestInconclusiveWorldAttributesByOracleReason(t *testing.T) {
	runsDir := t.TempDir()
	b := mkBundle(t, runsDir, "r_2026_09_21_bbbb")
	writeFile(t, filepath.Join(b, "verdict.json"),
		`{"schema":"prothesis.verdict/v1","verdict":"INCONCLUSIVE"}`)
	writeFile(t, filepath.Join(b, "world-0001", "result.json"),
		`{"world":1,"seed":7,"outcome":"inconclusive","oracles":[{"oracle":"resource_return_to_baseline","status":"inconclusive","explanation":"metric rss_bytes: no pre-DRIVE baseline"}]}`)

	rep, err := Diagnose(runsDir, loadRegistry(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Unattributed) != 0 {
		t.Fatalf("expected full attribution, got %v", rep.Unattributed)
	}
	if !hasCount(rep, "KP-001", 1) {
		t.Fatalf("expected KP-001 x1, got %v", rep.Counts)
	}
	if !hasCount(rep, "KP-004", 1) {
		t.Fatalf("expected the INCONCLUSIVE verdict to fold up to KP-004, got %v", rep.Counts)
	}
}

// A world dir with no result.json attributes to the dead-world entry, and the
// run verdict to the narrowed entry when narrowed is set.
func TestDeadWorldAndNarrowedVerdict(t *testing.T) {
	runsDir := t.TempDir()
	b := mkBundle(t, runsDir, "r_2026_09_21_cccc")
	writeFile(t, filepath.Join(b, "verdict.json"),
		`{"schema":"prothesis.verdict/v1","verdict":"INCONCLUSIVE","budget":{"narrowed":true}}`)
	writeFile(t, filepath.Join(b, "world-0001", "result.json"), `{"world":1,"seed":7,"outcome":"pass"}`)
	if err := os.MkdirAll(filepath.Join(b, "world-0002"), 0o755); err != nil {
		t.Fatal(err)
	}

	rep, err := Diagnose(runsDir, loadRegistry(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Unattributed) != 0 {
		t.Fatalf("expected full attribution, got %v", rep.Unattributed)
	}
	if !hasCount(rep, "KP-002", 1) {
		t.Fatalf("expected KP-002 x1 for the dead world, got %v", rep.Counts)
	}
	if !hasCount(rep, "KP-003", 1) {
		t.Fatalf("expected KP-003 x1 for the narrowed verdict, got %v", rep.Counts)
	}
}

// THE REFUSAL TEST: an outcome shape the registry does not know must be
// reported unattributed, never silently absorbed. This is the property the
// whole system exists for; if a catch-all ever appears in a registry, this
// test is the one that should have failed.
func TestAnUnknownCauseIsUnattributedLoudly(t *testing.T) {
	runsDir := t.TempDir()
	b := mkBundle(t, runsDir, "r_2026_09_21_dddd")
	writeFile(t, filepath.Join(b, "verdict.json"),
		`{"schema":"prothesis.verdict/v1","verdict":"INCONCLUSIVE"}`)
	writeFile(t, filepath.Join(b, "world-0001", "result.json"),
		`{"world":1,"seed":7,"outcome":"inconclusive","oracles":[{"oracle":"resource_return_to_baseline","status":"inconclusive","explanation":"a brand new refusal reason nobody catalogued"}]}`)

	rep, err := Diagnose(runsDir, loadRegistry(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Unattributed) != 1 {
		t.Fatalf("expected exactly one unattributed world, got %v", rep.Unattributed)
	}
	u := rep.Unattributed[0]
	if u.Level != "world" || u.Bundle != "r_2026_09_21_dddd" {
		t.Fatalf("unattributed item misidentified: %+v", u)
	}
	if !strings.Contains(u.Detail, "world-0001") {
		t.Fatalf("unattributed detail must name the world, got %q", u.Detail)
	}
}

// A registry with no fallback leaves an unexplained INCONCLUSIVE verdict
// unattributed: bundle-level novelty is as loud as world-level novelty.
func TestAnUnknownBundleShapeIsUnattributedLoudly(t *testing.T) {
	runsDir := t.TempDir()
	b := mkBundle(t, runsDir, "r_2026_09_21_eeee")
	writeFile(t, filepath.Join(b, "verdict.json"),
		`{"schema":"prothesis.verdict/v1","verdict":"ORACLE_DRIFT"}`)

	rep, err := Diagnose(runsDir, loadRegistry(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Unattributed) != 1 || rep.Unattributed[0].Level != "bundle" {
		t.Fatalf("expected one unattributed bundle, got %v", rep.Unattributed)
	}
}

func hasCount(rep *Report, id string, n int) bool {
	for _, c := range rep.Counts {
		if c.ID == id {
			return c.N == n
		}
	}
	return false
}

// TestTheCommittedRegistryIsValid guards the real registry: ids unique,
// regexes compile, ledger citations well-formed. The ledger citation test
// (internal/ledger) separately enforces that those citations resolve.
func TestTheCommittedRegistryIsValid(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "kvfixture", ".prothesis", "known-problems.yaml"))
	if err != nil {
		t.Skipf("no committed registry on this host: %v", err)
	}
	reg, err := schema.DecodeKnownProblems(data)
	if err != nil {
		t.Fatalf("committed registry does not decode: %v", err)
	}
	if err := reg.Validate(); err != nil {
		t.Fatalf("committed registry is invalid: %v", err)
	}
}

// TestTheRealCorpusIsFullyAttributed is the recursive gate: every
// non-terminal outcome in the recorded corpus must attribute to a registry
// entry. A run that produces a NEW kind of inconclusive turns this test red
// until the cause is catalogued. It skips only where no corpus exists at all
// (a fresh clone); a corpus that shrank below its census floor is caught by
// the census test, not by this one.
func TestTheRealCorpusIsFullyAttributed(t *testing.T) {
	runsDir := filepath.Join("..", "..", "testdata", "kvfixture", ".prothesis", "runs")
	if st, err := os.Stat(runsDir); err != nil || !st.IsDir() {
		t.Skip("no recorded corpus on this host (fresh clone or CI)")
	}
	data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "kvfixture", ".prothesis", "known-problems.yaml"))
	if err != nil {
		t.Fatalf("corpus exists but no registry: %v", err)
	}
	reg, err := schema.DecodeKnownProblems(data)
	if err != nil {
		t.Fatal(err)
	}
	rep, err := Diagnose(runsDir, reg)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("corpus attribution: %d bundles, %d worlds", rep.Bundles, rep.Worlds)
	for _, c := range rep.Counts {
		t.Logf("  %s %-16s %5d  %s", c.ID, c.Classification, c.N, c.Title)
	}
	for _, u := range rep.Unattributed {
		t.Errorf("unattributed %s in %s: %s", u.Level, u.Bundle, u.Detail)
	}
}
