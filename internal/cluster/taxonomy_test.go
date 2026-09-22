package cluster

import (
	"strings"
	"testing"
	"time"
)

// homogeneousItems builds n identical-feature items attributed to one KP.
func homogeneousItems(idPrefix string, n int, f VerdictFeatures) []Item {
	items := make([]Item, 0, n)
	for i := 0; i < n; i++ {
		items = append(items, mkItem(idPrefix+"/world-"+itoa(i), f, idPrefix+" reason"))
	}
	return items
}

// Five taxonomy leaves with engineered Gower distances: CAND-001 is a twin
// of KP-001 (sub-type), CAND-002 differs from everything in three
// categoricals (orphan), KP-002 and KP-003 sit between.
func taxonomyFixture() ([]CandidateInput, map[string][]Item) {
	kp1 := VerdictFeatures{Component: "no_crash", WorldFixture: "linear", VerdictType: "inconclusive", HasBaseline: true}
	kp2 := VerdictFeatures{Component: "no_panic_log", WorldFixture: "linear", VerdictType: "inconclusive", HasBaseline: true}
	kp3 := VerdictFeatures{Component: "no_panic_log", WorldFixture: "smoke", VerdictType: "inconclusive", HasBaseline: true}
	cand1 := kp1 // identical to KP-001
	cand2 := VerdictFeatures{Component: "alien_oracle", WorldFixture: "soak", VerdictType: "driver_error", HasBaseline: false}

	cands := []CandidateInput{
		{ID: "CAND-001", Items: homogeneousItems("cand1", 4, cand1)},
		{ID: "CAND-002", Items: homogeneousItems("cand2", 4, cand2)},
	}
	kps := map[string][]Item{
		"KP-001": homogeneousItems("kp1", 5, kp1),
		"KP-002": homogeneousItems("kp2", 5, kp2),
		"KP-003": homogeneousItems("kp3", 5, kp3),
	}
	return cands, kps
}

func TestTaxonomyDetectsSubTypeAndOrphan(t *testing.T) {
	cands, kps := taxonomyFixture()
	rep := BuildTaxonomyReport(cands, kps, time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC))

	if rep.APIVersion != "prothesis.taxonomy/v1" || rep.Kind != "TaxonomyReport" {
		t.Fatalf("schema header: %q %q", rep.APIVersion, rep.Kind)
	}
	if rep.Metadata.Linkage != "ward" || rep.Metadata.DistanceMetric != "gower" {
		t.Fatalf("metadata: %+v", rep.Metadata)
	}
	if rep.Metadata.EntriesAnalyzed != 5 {
		t.Fatalf("entries_analyzed: %d, want 5", rep.Metadata.EntriesAnalyzed)
	}

	if len(rep.Insights.SubTypes) != 1 {
		t.Fatalf("sub_types: %+v", rep.Insights.SubTypes)
	}
	st := rep.Insights.SubTypes[0]
	if st.Candidate != "CAND-001" || st.NearestKP != "KP-001" {
		t.Fatalf("sub-type: %+v", st)
	}
	if st.Recommendation == "" {
		t.Fatal("sub-type must carry a recommendation")
	}

	if len(rep.Insights.Orphans) != 1 {
		t.Fatalf("orphans: %+v", rep.Insights.Orphans)
	}
	if rep.Insights.Orphans[0].Candidate != "CAND-002" {
		t.Fatalf("orphan: %+v", rep.Insights.Orphans[0])
	}

	// The dendrogram covers every entry exactly once at the domain level.
	seen := map[string]int{}
	for _, d := range rep.Dendrogram.Domains {
		for _, m := range d.Members {
			seen[m]++
		}
	}
	for _, id := range []string{"CAND-001", "CAND-002", "KP-001", "KP-002", "KP-003"} {
		if seen[id] != 1 {
			t.Fatalf("domain membership for %s: %d (domains %+v)", id, seen[id], rep.Dendrogram.Domains)
		}
	}
	// The twin leaves must share a domain.
	for _, d := range rep.Dendrogram.Domains {
		hasCand, hasKP := false, false
		for _, m := range d.Members {
			if m == "CAND-001" {
				hasCand = true
			}
			if m == "KP-001" {
				hasKP = true
			}
		}
		if hasCand != hasKP && (hasCand || hasKP) {
			t.Fatalf("twins separated at domain level: %+v", rep.Dendrogram.Domains)
		}
	}
}

func TestTaxonomyEmptyInput(t *testing.T) {
	rep := BuildTaxonomyReport(nil, nil, time.Now().UTC())
	if rep.Metadata.EntriesAnalyzed != 0 {
		t.Fatalf("empty input: %+v", rep.Metadata)
	}
	enc, err := rep.MarshalYAML()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(enc), "prothesis.taxonomy/v1") {
		t.Fatalf("empty taxonomy must stay schema-valid:\n%s", enc)
	}
}

func TestTaxonomyDeterministic(t *testing.T) {
	cands, kps := taxonomyFixture()
	when := time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC)
	a, err := BuildTaxonomyReport(cands, kps, when).MarshalYAML()
	if err != nil {
		t.Fatal(err)
	}
	b, err := BuildTaxonomyReport(cands, kps, when).MarshalYAML()
	if err != nil {
		t.Fatal(err)
	}
	if string(a) != string(b) {
		t.Fatalf("taxonomy must be byte-identical across runs:\n%s\n---\n%s", a, b)
	}
}

// A KP whose outcomes fall into two separated sub-groups must surface as a
// split candidate; a homogeneous KP must not.
func TestDetectSplits(t *testing.T) {
	left := VerdictFeatures{Component: "o1", WorldFixture: "linear", VerdictType: "inconclusive"}
	right := VerdictFeatures{Component: "o1", WorldFixture: "smoke", VerdictType: "driver_error"}
	kpItems := map[string][]Item{
		"KP-001": append(homogeneousItems("l", 4, left), homogeneousItems("r", 4, right)...),
		"KP-002": homogeneousItems("h", 5, left),
	}
	splits := DetectSplits(kpItems, 0.05)
	if len(splits) != 1 {
		t.Fatalf("splits: %+v", splits)
	}
	if splits[0].Entry != "KP-001" || splits[0].SubClusters != 2 {
		t.Fatalf("split: %+v", splits[0])
	}
	if splits[0].InternalDistance <= 0.05 {
		t.Fatalf("internal distance must exceed the cut: %+v", splits[0])
	}
	if splits[0].Recommendation == "" {
		t.Fatal("split candidate must carry a recommendation")
	}
}
