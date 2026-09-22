package cluster

import (
	"encoding/csv"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFeaturesCSVRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cluster_features.csv")
	items := []Item{
		{ID: "r_a/world-0001", Level: "world", Features: VerdictFeatures{
			Component: "no_crash", ExitCode: 3, HasBaseline: true, WorldFixture: "linear",
			VerdictType: "inconclusive", DurationMs: 1234.5, ReasonTokenCount: 8, ReasonUniqueWords: 6,
		}},
		{ID: "r_b", Level: "bundle", Features: VerdictFeatures{
			Component: "bundle", WorldFixture: "unknown", VerdictType: "NO_VERDICT",
		}},
	}
	if err := WriteFeaturesCSV(path, items); err != nil {
		t.Fatal(err)
	}
	// The header is the frozen contract.
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := csv.NewReader(f).ReadAll()
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	wantHeader := "world_id,component,exit_code,has_baseline,world_fixture,verdict_type,duration_ms,reason_token_count,reason_unique_words"
	if strings.Join(rows[0], ",") != wantHeader {
		t.Fatalf("header: %v", rows[0])
	}
	if len(rows) != 3 {
		t.Fatalf("rows: %d, want 3", len(rows))
	}

	back, err := ReadFeaturesCSV(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(back) != 2 {
		t.Fatalf("items: %d", len(back))
	}
	if back[0].Features != items[0].Features || back[1].Features != items[1].Features {
		t.Fatalf("round trip drift:\n%+v\n%+v", back, items)
	}
	if back[0].ID != items[0].ID || back[1].ID != items[1].ID {
		t.Fatalf("ids: %+v", back)
	}
}

func TestManifestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cluster_manifest.json")
	m := ExtractManifest{
		CorpusSize:          42,
		AttributedCount:     40,
		UnattributedCount:   2,
		ExtractionTimestamp: "2026-09-21T00:00:00Z",
	}
	if err := WriteManifest(path, m); err != nil {
		t.Fatal(err)
	}
	back, err := ReadManifest(path)
	if err != nil {
		t.Fatal(err)
	}
	if back != m {
		t.Fatalf("manifest: %+v vs %+v", back, m)
	}
	if m.CorpusSize != m.AttributedCount+m.UnattributedCount {
		t.Fatalf("corpus_size must equal attributed + unattributed: %+v", m)
	}
}

func TestReadFeaturesCSVMissing(t *testing.T) {
	if _, err := ReadFeaturesCSV(filepath.Join(t.TempDir(), "nope.csv")); err == nil {
		t.Fatal("missing CSV must error loudly")
	}
}
