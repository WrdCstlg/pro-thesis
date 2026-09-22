package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/WrdCstlg/pro-thesis/internal/cluster"
	"gopkg.in/yaml.v3"
)

// clusterTestProject scaffolds a minimal valid project: config, registry and
// a two-outcome corpus with one attributed world and one attributed bundle
// verdict, so the unattributed pool is empty (today's real-corpus shape).
func clusterTestProject(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	cfg := `version: prothesis/v1
name: clustertest
harness:
  backend: compose
  file: docker-compose.yaml
  nodes:
    - { id: n1, service: kv }
`
	if err := os.WriteFile(filepath.Join(dir, "prothesis.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	reg := `schema: prothesis.knownproblems/v1
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
    title: folded-up refusals
    classification: by-design
    ledger: OQ-072
    level: bundle
    match:
      verdict: INCONCLUSIVE
`
	pro := filepath.Join(dir, ".prothesis")
	if err := os.MkdirAll(pro, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pro, "known-problems.yaml"), []byte(reg), 0o644); err != nil {
		t.Fatal(err)
	}
	world := filepath.Join(pro, "runs", "r_2026_09_21_aaaa", "world-0001")
	if err := os.MkdirAll(world, 0o755); err != nil {
		t.Fatal(err)
	}
	verdict := `{"schema":"prothesis.verdict/v1","profile":"linear","verdict":"INCONCLUSIVE"}`
	if err := os.WriteFile(filepath.Join(pro, "runs", "r_2026_09_21_aaaa", "verdict.json"), []byte(verdict), 0o644); err != nil {
		t.Fatal(err)
	}
	result := `{"world":1,"seed":7,"outcome":"inconclusive","oracles":[` +
		`{"oracle":"resource_return_to_baseline","status":"inconclusive","explanation":"metric rss_bytes: no pre-DRIVE baseline"}]}`
	if err := os.WriteFile(filepath.Join(world, "result.json"), []byte(result), 0o644); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, "prothesis.yaml")
}

func TestClusterUsageErrors(t *testing.T) {
	g := globals{}
	if code := cmdCluster(context.Background(), g, nil); code != 5 {
		t.Fatalf("no subcommand: exit %d, want 5", code)
	}
	if code := cmdCluster(context.Background(), g, []string{"frobnicate"}); code != 5 {
		t.Fatalf("unknown subcommand: exit %d, want 5", code)
	}
	if code := cmdCluster(context.Background(), g, []string{"extract", "--bogus"}); code != 5 {
		t.Fatalf("unknown flag: exit %d, want 5", code)
	}
}

// extract -> discover -> taxonomy over the synthetic project: every verb
// exits 0, the artifacts land under .prothesis/, and the reports decode.
func TestClusterVerbsEndToEnd(t *testing.T) {
	cfgPath := clusterTestProject(t)
	g := globals{configPath: cfgPath, quiet: true}
	ctx := context.Background()
	pro := filepath.Join(filepath.Dir(cfgPath), ".prothesis")

	if code := cmdCluster(ctx, g, []string{"extract"}); code != 0 {
		t.Fatalf("extract: exit %d, want 0", code)
	}
	for _, f := range []string{"cluster_features.csv", "cluster_manifest.json"} {
		if _, err := os.Stat(filepath.Join(pro, f)); err != nil {
			t.Fatalf("extract did not write %s: %v", f, err)
		}
	}

	if code := cmdCluster(ctx, g, []string{"discover"}); code != 0 {
		t.Fatalf("discover with no contested outcomes: exit %d, want 0", code)
	}
	data, err := os.ReadFile(filepath.Join(pro, "cluster_report.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var rep map[string]any
	if err := yaml.Unmarshal(data, &rep); err != nil {
		t.Fatalf("cluster_report.yaml does not decode: %v", err)
	}
	if rep["apiVersion"] != "prothesis.clustering/v1" || rep["kind"] != "ClusterReport" {
		t.Fatalf("cluster report header: %v %v", rep["apiVersion"], rep["kind"])
	}

	if code := cmdCluster(ctx, g, []string{"taxonomy"}); code != 0 {
		t.Fatalf("taxonomy: exit %d, want 0", code)
	}
	tdata, err := os.ReadFile(filepath.Join(pro, "taxonomy_report.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var tax map[string]any
	if err := yaml.Unmarshal(tdata, &tax); err != nil {
		t.Fatalf("taxonomy_report.yaml does not decode: %v", err)
	}
	if tax["apiVersion"] != "prothesis.taxonomy/v1" || tax["kind"] != "TaxonomyReport" {
		t.Fatalf("taxonomy header: %v %v", tax["apiVersion"], tax["kind"])
	}
	md, _ := tax["metadata"].(map[string]any)
	if md["entries_analyzed"] != 2 { // KP-001 world + KP-002 bundle, no candidates
		t.Fatalf("entries_analyzed: %v, want 2", md["entries_analyzed"])
	}
}

// discover must refuse loudly when extract never ran.
func TestClusterDiscoverWithoutExtract(t *testing.T) {
	cfgPath := clusterTestProject(t)
	g := globals{configPath: cfgPath, quiet: true}
	if code := cmdCluster(context.Background(), g, []string{"discover"}); code != 5 {
		t.Fatalf("discover without extract: exit %d, want 5", code)
	}
	if code := cmdCluster(context.Background(), g, []string{"taxonomy"}); code != 5 {
		t.Fatalf("taxonomy without discover: exit %d, want 5", code)
	}
}

// The exit-2-on-CONTESTED rule is normative; pin it directly.
func TestDiscoverExitCodeRule(t *testing.T) {
	clean := &cluster.ClusterReport{}
	if code := discoverExitCode(clean); code != 0 {
		t.Fatalf("no contested: %d, want 0", code)
	}
	contested := &cluster.ClusterReport{}
	contested.Summary.Contested = 1
	if code := discoverExitCode(contested); code != 2 {
		t.Fatalf("contested present: %d, want 2", code)
	}
}
