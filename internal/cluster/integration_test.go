package cluster

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/WrdCstlg/pro-thesis/internal/diagnose"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
	"gopkg.in/yaml.v3"
)

// corpusDirs locates the recorded corpus; the test skips where it does not
// exist (fresh clone or CI), the same contract as the census tests.
func corpusDirs(t *testing.T) (runsDir, regPath string) {
	t.Helper()
	base := filepath.Join("..", "..", "testdata", "kvfixture", ".prothesis")
	runsDir = filepath.Join(base, "runs")
	if st, err := os.Stat(runsDir); err != nil || !st.IsDir() {
		t.Skip("no recorded corpus on this host (fresh clone or CI)")
	}
	return runsDir, filepath.Join(base, "known-problems.yaml")
}

// checksumTree hashes every file under root into one digest: relative path
// and content, in sorted order. It is the read-only guarantee: the pipeline
// must leave the corpus byte-identical.
func checksumTree(t *testing.T, root string) string {
	t.Helper()
	var files []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(files)
	h := sha256.New()
	for _, f := range files {
		rel, _ := filepath.Rel(root, f)
		h.Write([]byte(filepath.ToSlash(rel)))
		data, err := os.Open(f)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(h, data); err != nil {
			data.Close()
			t.Fatal(err)
		}
		data.Close()
	}
	return hex.EncodeToString(h.Sum(nil))
}

func loadRegistry(t *testing.T, regPath string) *schema.KnownProblems {
	t.Helper()
	data, err := os.ReadFile(regPath)
	if err != nil {
		t.Fatal(err)
	}
	reg, err := schema.DecodeKnownProblems(data)
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.Validate(); err != nil {
		t.Fatal(err)
	}
	return reg
}

// The full pipeline against the recorded corpus: extract, discover,
// taxonomy. The corpus is checksummed before and after; the clustering
// engine holds no write path into it, and this test proves it.
func TestPipelineAgainstTheRealCorpusIsReadOnly(t *testing.T) {
	runsDir, regPath := corpusDirs(t)
	reg := loadRegistry(t, regPath)

	before := checksumTree(t, runsDir)

	bundles, err := diagnose.Scan(runsDir, reg)
	if err != nil {
		t.Fatal(err)
	}
	items := UnattributedItems(bundles)
	byKP := AttributedItems(bundles)
	attributed := 0
	for _, v := range byKP {
		attributed += len(v)
	}

	outDir := t.TempDir()
	csvPath := filepath.Join(outDir, "cluster_features.csv")
	if err := WriteFeaturesCSV(csvPath, items); err != nil {
		t.Fatal(err)
	}
	manifest := ExtractManifest{
		CorpusSize:          attributed + len(items),
		AttributedCount:     attributed,
		UnattributedCount:   len(items),
		ExtractionTimestamp: time.Now().UTC().Format(time.RFC3339),
	}
	if err := WriteManifest(filepath.Join(outDir, "cluster_manifest.json"), manifest); err != nil {
		t.Fatal(err)
	}

	// The CSV row count must equal the unattributed pool size exactly: on
	// today's corpus that is ZERO, and the empty pool must still be a valid
	// extract, not a crash.
	back, err := ReadFeaturesCSV(csvPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(back) != len(items) {
		t.Fatalf("CSV rows %d != unattributed items %d", len(back), len(items))
	}
	t.Logf("real corpus: %d attributed, %d unattributed", attributed, len(items))

	// discover
	d := DiscoverClusters(back)
	rep := BuildClusterReport(d, back, ClusterMeta{
		Generated: time.Now().UTC(), CorpusSize: manifest.CorpusSize, Attributed: attributed,
	})
	enc, err := rep.MarshalYAML()
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := yaml.Unmarshal(enc, &decoded); err != nil {
		t.Fatalf("cluster report is not valid YAML: %v", err)
	}
	if decoded["apiVersion"] != "prothesis.clustering/v1" || decoded["kind"] != "ClusterReport" {
		t.Fatalf("cluster report header: %v %v", decoded["apiVersion"], decoded["kind"])
	}
	md, _ := decoded["metadata"].(map[string]any)
	if md["unattributed_input"] != len(items) && md["unattributed_input"] != 0 {
		t.Fatalf("unattributed_input: %v", md["unattributed_input"])
	}
	if len(items) == 0 && rep.Summary.Candidates+rep.Summary.Contested+rep.Summary.Noise != 0 {
		t.Fatalf("empty pool must yield an empty report: %+v", rep.Summary)
	}

	// taxonomy over candidate clusters (none today) plus every KP centroid.
	var cands []CandidateInput
	for _, c := range rep.Clusters {
		if c.Status != "CANDIDATE" {
			continue
		}
		var members []Item
		for _, id := range c.MemberWorlds {
			for _, it := range back {
				if it.ID == id {
					members = append(members, it)
				}
			}
		}
		cands = append(cands, CandidateInput{ID: c.ID, Items: members})
	}
	tax := BuildTaxonomyReport(cands, byKP, time.Now().UTC())
	tenc, err := tax.MarshalYAML()
	if err != nil {
		t.Fatal(err)
	}
	var tdecoded map[string]any
	if err := yaml.Unmarshal(tenc, &tdecoded); err != nil {
		t.Fatalf("taxonomy report is not valid YAML: %v", err)
	}
	if tdecoded["apiVersion"] != "prothesis.taxonomy/v1" || tdecoded["kind"] != "TaxonomyReport" {
		t.Fatalf("taxonomy header: %v %v", tdecoded["apiVersion"], tdecoded["kind"])
	}
	if tax.Metadata.EntriesAnalyzed != len(byKP)+len(cands) {
		t.Fatalf("entries_analyzed %d, want %d", tax.Metadata.EntriesAnalyzed, len(byKP)+len(cands))
	}
	// Domains partition every entry exactly once.
	seen := map[string]bool{}
	for _, dom := range tax.Dendrogram.Domains {
		for _, m := range dom.Members {
			if seen[m] {
				t.Fatalf("entry %s in two domains", m)
			}
			seen[m] = true
		}
	}
	if len(seen) != tax.Metadata.EntriesAnalyzed {
		t.Fatalf("domains cover %d entries, report analyzed %d", len(seen), tax.Metadata.EntriesAnalyzed)
	}
	for id := range byKP {
		if !seen[id] {
			t.Fatalf("KP %s missing from the taxonomy", id)
		}
	}
	if !strings.Contains(string(tenc), "linkage: ward") {
		t.Fatal("taxonomy must declare its linkage")
	}

	after := checksumTree(t, runsDir)
	if before != after {
		t.Fatalf("corpus changed under a read-only pipeline:\nbefore %s\nafter  %s", before, after)
	}
	t.Logf("corpus checksum stable across the pipeline: %s", before[:16])
}
