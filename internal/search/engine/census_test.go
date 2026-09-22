package engine

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// The corpus census
//
// The tests in verdict_test.go re-check the recorded run corpus in
// testdata/kvfixture/.prothesis/runs. That corpus is gitignored evidence: it
// exists only on machines that ran the acceptance procedure, so a skip when it
// is absent is legitimate -- a fresh clone has recorded nothing. But the same
// skip fires when the corpus was PRUNED, and then the suite stays green while
// the evidence it guards is gone. CENSUS.json, committed beside the corpus,
// records how much evidence existed when it was written; while the corpus
// exists, the suite fails if it holds less than the floor.
//
// Contract with `thesis prune`: an intentional prune lowers the floors by
// rewriting CENSUS.json in the same change that removes the bundles. The file
// is the diff a reviewer inspects; the tests only make silent shrinkage loud.
// ---------------------------------------------------------------------------

// corpusCensus mirrors testdata/kvfixture/.prothesis/CENSUS.json.
type corpusCensus struct {
	Schema         string   `json:"schema"`
	Recorded       string   `json:"recorded"`
	BundleFloor    int      `json:"bundle_floor"`
	VerdictedFloor int      `json:"verdicted_floor"`
	Protected      []string `json:"protected"`
	Reason         string   `json:"reason"`
}

const censusSchema = "prothesis.census/v1"

// censusPathFor returns the census file that governs a runs directory.
func censusPathFor(runsDir string) string {
	return filepath.Join(filepath.Dir(runsDir), "CENSUS.json")
}

// corpusState classifies what a checkout holds, which decides what the corpus
// tests may do with it.
type corpusState int

const (
	corpusAbsent   corpusState = iota // no runs dir: a fresh clone, the only legitimate skip
	corpusNoCensus                    // runs dir but no CENSUS.json: a checkout older than the census
	corpusCensused                    // both present: the floor is enforced
)

func classifyCorpus(runsDir string) corpusState {
	st, err := os.Stat(runsDir)
	if err != nil || !st.IsDir() {
		return corpusAbsent
	}
	if _, err := os.Stat(censusPathFor(runsDir)); err != nil {
		return corpusNoCensus
	}
	return corpusCensused
}

// corpusBundles returns the entries of the recorded run corpus, enforcing the
// committed census when one governs it. It is the ONLY way the corpus tests
// reach the corpus, so the floor cannot be bypassed by one test forgetting it.
func corpusBundles(t *testing.T) []os.DirEntry {
	t.Helper()
	dir := runsDir(t)
	switch classifyCorpus(dir) {
	case corpusAbsent:
		// Fresh clone or CI: nothing was ever recorded here. This is the only
		// legitimate skip; it says "nothing to check", never "checked and fine".
		t.Skipf("no run bundles at %s: nothing recorded on this checkout", dir)
	case corpusNoCensus:
		// Checkouts from before CENSUS.json existed have a corpus but no floor.
		// They keep the historical log-only behavior; failing them would brick
		// every older revision under bisect.
		t.Logf("no CENSUS.json beside %s: corpus floor not enforced on this checkout", dir)
	case corpusCensused:
		enforceCensus(t, dir)
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Skipf("no run bundles at %s: %v", dir, err)
	}
	return ents
}

// enforceCensus fails the calling test for every way the corpus breaks its
// committed floor.
func enforceCensus(t *testing.T, runsDir string) {
	t.Helper()
	data, err := os.ReadFile(censusPathFor(runsDir))
	if err != nil {
		t.Fatalf("classified as censused but %s does not read: %v", censusPathFor(runsDir), err)
	}
	var c corpusCensus
	if err := json.Unmarshal(data, &c); err != nil {
		t.Fatalf("%s does not parse: %v", censusPathFor(runsDir), err)
	}
	if c.Schema != censusSchema {
		t.Fatalf("%s declares schema %q, want %q", censusPathFor(runsDir), c.Schema, censusSchema)
	}
	for _, v := range censusViolations(runsDir, c) {
		t.Error(v)
	}
}

// censusViolations lists every way the corpus at runsDir falls short of the
// census: fewer bundles than the floor, fewer verdicts than the floor, or a
// protected run id gone. An empty slice means the floor holds.
func censusViolations(runsDir string, c corpusCensus) []string {
	bundles, verdicted, err := countBundles(runsDir)
	if err != nil {
		return []string{fmt.Sprintf("cannot count the corpus at %s: %v", runsDir, err)}
	}
	var out []string
	if bundles < c.BundleFloor {
		out = append(out, fmt.Sprintf("corpus holds %d run bundle(s), below the census floor of %d: "+
			"a prune shrank the recorded evidence without lowering CENSUS.json in the same change",
			bundles, c.BundleFloor))
	}
	if verdicted < c.VerdictedFloor {
		out = append(out, fmt.Sprintf("corpus holds %d verdicted bundle(s), below the census floor of %d: "+
			"a prune shrank the recorded evidence without lowering CENSUS.json in the same change",
			verdicted, c.VerdictedFloor))
	}
	for _, id := range c.Protected {
		if _, err := os.Stat(filepath.Join(runsDir, id)); err != nil {
			out = append(out, fmt.Sprintf("protected run %s is missing from %s: it is cited by a "+
				"committed observation bundle and must never be pruned", id, runsDir))
		}
	}
	return out
}

// countBundles counts run bundle directories and how many of them hold a
// verdict.json.
func countBundles(runsDir string) (bundles, verdicted int, err error) {
	ents, err := os.ReadDir(runsDir)
	if err != nil {
		return 0, 0, err
	}
	for _, e := range ents {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue // .trash and friends are not bundles
		}
		bundles++
		if st, err := os.Stat(filepath.Join(runsDir, e.Name(), "verdict.json")); err == nil && !st.IsDir() {
			verdicted++
		}
	}
	return bundles, verdicted, nil
}

// ---------------------------------------------------------------------------
// A pruned corpus must be LOUD. These build synthetic corpora in temp dirs and
// point the enforcement at them; the real corpus is never touched.
// ---------------------------------------------------------------------------

func writeBundle(t *testing.T, runsDir, name string, withVerdict bool) {
	t.Helper()
	dir := filepath.Join(runsDir, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if withVerdict {
		if err := os.WriteFile(filepath.Join(dir, "verdict.json"), []byte(`{}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCensusViolationsFlagsAShrunkCorpus(t *testing.T) {
	dir := t.TempDir()
	// The census recorded 4 bundles, 3 of them verdicted, and one protected run.
	c := corpusCensus{BundleFloor: 4, VerdictedFloor: 3, Protected: []string{"r_keep"}}
	// The prune took two bundles, two verdicts and the protected run.
	writeBundle(t, dir, "r_a", true)
	writeBundle(t, dir, "r_b", false)
	v := censusViolations(dir, c)
	if len(v) != 3 {
		t.Fatalf("a corpus pruned below every floor must report 3 violations, got %d: %v", len(v), v)
	}
	joined := strings.Join(v, "\n")
	for _, want := range []string{"2 run bundle(s)", "1 verdicted bundle(s)", "r_keep"} {
		if !strings.Contains(joined, want) {
			t.Errorf("violations do not mention %q:\n%s", want, joined)
		}
	}
}

func TestCensusViolationsAcceptsTheFloorAndGrowth(t *testing.T) {
	dir := t.TempDir()
	c := corpusCensus{BundleFloor: 2, VerdictedFloor: 1, Protected: []string{"r_keep"}}
	writeBundle(t, dir, "r_keep", true)
	writeBundle(t, dir, "r_a", false)
	if v := censusViolations(dir, c); len(v) != 0 {
		t.Errorf("a corpus exactly at its floor must report no violations, got %v", v)
	}
	writeBundle(t, dir, "r_b", true)
	if v := censusViolations(dir, c); len(v) != 0 {
		t.Errorf("a corpus grown past its floor must report no violations, got %v", v)
	}
}

func TestClassifyCorpus(t *testing.T) {
	root := t.TempDir()
	if got := classifyCorpus(filepath.Join(root, "nope")); got != corpusAbsent {
		t.Errorf("missing runs dir classified as %v, want corpusAbsent (%v)", got, corpusAbsent)
	}
	runs := filepath.Join(root, "runs")
	if err := os.Mkdir(runs, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := classifyCorpus(runs); got != corpusNoCensus {
		t.Errorf("runs dir without CENSUS.json classified as %v, want corpusNoCensus (%v)", got, corpusNoCensus)
	}
	if err := os.WriteFile(censusPathFor(runs), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := classifyCorpus(runs); got != corpusCensused {
		t.Errorf("runs dir with CENSUS.json classified as %v, want corpusCensused (%v)", got, corpusCensused)
	}
}

// TestRecordedCorpusHonorsItsCensus is the named guard for the real corpus:
// corpusBundles enforces the floor (or legitimately skips on a fresh clone),
// so a shrunk corpus turns this red instead of silently skipping the checks
// in verdict_test.go.
func TestRecordedCorpusHonorsItsCensus(t *testing.T) {
	ents := corpusBundles(t)
	bundles, verdicted, err := countBundles(runsDir(t))
	if err != nil {
		t.Fatalf("corpus was classified present but does not read: %v", err)
	}
	t.Logf("census floor enforced: %d corpus entrie(s), %d bundle(s), %d verdicted",
		len(ents), bundles, verdicted)
}
