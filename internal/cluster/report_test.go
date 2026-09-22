package cluster

import (
	"strings"
	"testing"
	"time"
)

// itoa renders small integers without importing strconv in test tables.
func itoa(i int) string {
	if i < 10 {
		return string(rune('0' + i))
	}
	return string(rune('0'+i/10)) + string(rune('0'+i%10))
}

// mkItem is the test item constructor used everywhere below.
func mkItem(id string, f VerdictFeatures, reason string) Item {
	return Item{ID: id, Level: "world", Reasons: []string{reason}, Features: f}
}

// End to end over the pipeline: two dense eighteen-point groups and two
// outliers must come out as two CANDIDATE clusters and two noise points,
// with byte-identical YAML across runs (modulo the generated timestamp).
func TestDiscoverPipelineTwoClustersTwoNoise(t *testing.T) {
	var items []Item
	for i := 0; i < 18; i++ {
		items = append(items, mkItem("r_a/world-"+itoa(i), VerdictFeatures{
			Component: "no_crash", WorldFixture: "linear", VerdictType: "inconclusive",
			HasBaseline: true, ReasonTokenCount: 100 + i, ReasonUniqueWords: 40,
		}, "crash refusal"))
	}
	for i := 0; i < 18; i++ {
		items = append(items, mkItem("r_b/world-"+itoa(i), VerdictFeatures{
			Component: "no_panic_log", WorldFixture: "smoke", VerdictType: "inconclusive",
			ReasonTokenCount: 500 + i, ReasonUniqueWords: 90,
		}, "panic refusal"))
	}
	items = append(items,
		mkItem("r_c/world-00", VerdictFeatures{
			Component: "strange_oracle", WorldFixture: "soak", VerdictType: "oracle_error",
			ReasonTokenCount: 1500, ReasonUniqueWords: 300,
		}, "strange"),
		mkItem("r_d/world-00", VerdictFeatures{
			Component: "other_oracle", WorldFixture: "gate", VerdictType: "driver_error",
			HasBaseline: true, ReasonTokenCount: 2500, ReasonUniqueWords: 500,
		}, "other"),
	)

	d := DiscoverClusters(items)
	meta := ClusterMeta{Generated: time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC), CorpusSize: 40, Attributed: 2}
	rep := BuildClusterReport(d, items, meta)

	if rep.APIVersion != "prothesis.clustering/v1" || rep.Kind != "ClusterReport" {
		t.Fatalf("schema header: %q %q", rep.APIVersion, rep.Kind)
	}
	if rep.Metadata.UnattributedInput != 38 {
		t.Fatalf("unattributed_input: %d, want 38", rep.Metadata.UnattributedInput)
	}
	if rep.Summary.Candidates != 2 || rep.Summary.Contested != 0 || rep.Summary.Noise != 2 {
		t.Fatalf("summary: %+v", rep.Summary)
	}
	if len(rep.Clusters) != 2 {
		t.Fatalf("clusters: %+v", rep.Clusters)
	}
	for _, c := range rep.Clusters {
		if c.Status != "CANDIDATE" || c.Consensus != "agree" {
			t.Fatalf("dense groups must be agreed CANDIDATEs: %+v", c)
		}
		if c.Size != 18 || len(c.MemberWorlds) != 18 {
			t.Fatalf("cluster size: %+v", c)
		}
		if len(c.RepresentativeReasons) == 0 || c.CentroidDescription == "" {
			t.Fatalf("cluster description missing: %+v", c)
		}
	}
	if rep.Clusters[0].ID != "CAND-001" || rep.Clusters[1].ID != "CAND-002" {
		t.Fatalf("cluster ids: %q %q", rep.Clusters[0].ID, rep.Clusters[1].ID)
	}
	// Noise is sacred: the two outliers appear under noise and in no cluster.
	if rep.Noise.Count != 2 || len(rep.Noise.Worlds) != 2 {
		t.Fatalf("noise section: %+v", rep.Noise)
	}
	for _, c := range rep.Clusters {
		for _, m := range c.MemberWorlds {
			if m == "r_c/world-00" || m == "r_d/world-00" {
				t.Fatalf("noise point %q leaked into %s", m, c.ID)
			}
		}
	}

	// Idempotent modulo the generated timestamp.
	enc1, err := rep.MarshalYAML()
	if err != nil {
		t.Fatal(err)
	}
	rep2 := BuildClusterReport(DiscoverClusters(items), items, meta)
	enc2, err := rep2.MarshalYAML()
	if err != nil {
		t.Fatal(err)
	}
	if string(enc1) != string(enc2) {
		t.Fatalf("same input must produce byte-identical YAML:\n%s\n---\n%s", enc1, enc2)
	}
}

// The empty pool (today's real corpus) must produce a schema-valid report
// with zero clusters and no crash.
func TestDiscoverPipelineEmptyPool(t *testing.T) {
	d := DiscoverClusters(nil)
	rep := BuildClusterReport(d, nil, ClusterMeta{Generated: time.Now().UTC()})
	if rep.Summary.Candidates != 0 || rep.Summary.Contested != 0 || rep.Summary.Noise != 0 {
		t.Fatalf("empty pool summary: %+v", rep.Summary)
	}
	if rep.Metadata.UnattributedInput != 0 {
		t.Fatalf("empty pool input count: %d", rep.Metadata.UnattributedInput)
	}
	enc, err := rep.MarshalYAML()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(enc), "prothesis.clustering/v1") {
		t.Fatalf("empty pool YAML must still carry the schema header:\n%s", enc)
	}
}

// A hand-built Discovery with a contested cluster and noise: cluster status
// rules, and noise never reassigned (the "noise is sacred" mutation target).
func TestBuildClusterReportContestedAndNoise(t *testing.T) {
	items := []Item{
		mkItem("r/world-00", VerdictFeatures{Component: "a"}, "reason zero"),
		mkItem("r/world-01", VerdictFeatures{Component: "a"}, "reason one"),
		mkItem("r/world-02", VerdictFeatures{Component: "b"}, "reason two"),
	}
	d := &Discovery{
		Assignments: []Assignment{
			{Status: StatusCandidate, Cluster: "h:0", Agree: true},
			{Status: StatusContested, Cluster: "h:0"},
			{Status: StatusNoise},
		},
		HDB: HDBSCANResult{Probabilities: []float64{1, 0.8, 0}},
	}
	rep := BuildClusterReport(d, items, ClusterMeta{Generated: time.Now().UTC()})
	if len(rep.Clusters) != 1 {
		t.Fatalf("clusters: %+v", rep.Clusters)
	}
	c := rep.Clusters[0]
	if c.Status != "CONTESTED" || c.Consensus != "disagree" {
		t.Fatalf("one contested member makes the cluster CONTESTED/disagree: %+v", c)
	}
	if c.Size != 2 {
		t.Fatalf("size: %+v", c)
	}
	if c.ID != "CAND-001" {
		t.Fatalf("contested clusters still take CAND-NNN ids: %q", c.ID)
	}
	// stability is the mean HDBSCAN membership probability over members.
	if c.Stability < 0.89 || c.Stability > 0.91 {
		t.Fatalf("stability: %v, want ~0.9", c.Stability)
	}
	if rep.Summary.Contested != 1 || rep.Summary.Candidates != 0 {
		t.Fatalf("summary: %+v", rep.Summary)
	}
	if rep.Noise.Count != 1 || rep.Noise.Worlds[0].ID != "r/world-02" {
		t.Fatalf("noise: %+v", rep.Noise)
	}
	if rep.Noise.Worlds[0].Reason != "reason two" {
		t.Fatalf("noise reason: %+v", rep.Noise.Worlds[0])
	}
	for _, m := range c.MemberWorlds {
		if m == "r/world-02" {
			t.Fatal("noise point reassigned into a cluster")
		}
	}
}
