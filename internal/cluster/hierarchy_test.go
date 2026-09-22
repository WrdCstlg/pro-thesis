package cluster

import (
	"reflect"
	"testing"
)

// Five centroids at 1-D coordinates {0, 1, 2.4, 10, 11.5}. Ward's merge
// order on this geometry is hand-computed: {0,1} first (d2 = 0.5), then the
// compact pair {3,4} (d2 = 1.125) because Ward's inflated {0,1}x{2}
// distance (d2 = 2.41) loses to it, then 2 joins {0,1}, then the rest.
// Single linkage would instead merge {0,1,2} SECOND (its 1.4 edge beats the
// 1.5 edge of {3,4}), so the single-linkage mutation changes the second
// merge and turns this test red.
func TestWardMergeOrderOnFiveCentroids(t *testing.T) {
	dist := matrixFromCoords([]float64{0, 1, 2.4, 10, 11.5})
	merges := Ward(dist)
	if len(merges) != 4 {
		t.Fatalf("n=5 must produce 4 merges, got %d", len(merges))
	}
	wantMembers := [][]int{
		{0, 1},
		{3, 4},
		{0, 1, 2},
		{0, 1, 2, 3, 4},
	}
	for i, w := range wantMembers {
		if !reflect.DeepEqual(merges[i].Members, w) {
			t.Fatalf("merge %d members: got %v, want %v (all merges: %+v)", i, merges[i].Members, w, merges)
		}
	}
	// Ward merge distances are non-decreasing.
	for i := 1; i < len(merges); i++ {
		if merges[i].Distance < merges[i-1].Distance {
			t.Fatalf("merge distances must be non-decreasing: %+v", merges)
		}
	}
	if merges[0].Size != 2 || merges[3].Size != 5 {
		t.Fatalf("merge sizes: %+v", merges)
	}
}

// Tied merge distances must break by smallest member id, so the same matrix
// always yields the same dendrogram.
func TestWardDeterministicOnTies(t *testing.T) {
	dist := matrixFromCoords([]float64{0, 1, 10, 11, 20, 21})
	a := Ward(dist)
	b := Ward(dist)
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("non-deterministic dendrogram:\n%+v\n%+v", a, b)
	}
	if a[0].Members[0] != 0 {
		t.Fatalf("tie between equal pair merges must break to the smaller member id: %+v", a[0])
	}
}

func TestWardTinyInputs(t *testing.T) {
	if got := Ward(nil); len(got) != 0 {
		t.Fatalf("empty: %+v", got)
	}
	if got := Ward(matrixFromCoords([]float64{5})); len(got) != 0 {
		t.Fatalf("single leaf: %+v", got)
	}
	two := Ward(matrixFromCoords([]float64{0, 3}))
	if len(two) != 1 || !reflect.DeepEqual(two[0].Members, []int{0, 1}) {
		t.Fatalf("two leaves: %+v", two)
	}
}

// Eight points in four distant pairs: the merge-distance profile has exactly
// one obvious jump (pairs merging into quads), and the cut selector must put
// the domain boundary there: four clusters.
func TestInconsistencyCutFindsTheObviousJump(t *testing.T) {
	dist := matrixFromCoords([]float64{0, 0.1, 10, 10.1, 20, 20.1, 30, 30.1})
	merges := Ward(dist)
	// The jump merge must carry a large inconsistency coefficient.
	var maxInco float64
	for _, m := range merges {
		if m.Inconsistency > maxInco {
			maxInco = m.Inconsistency
		}
	}
	if maxInco < 1 {
		t.Fatalf("expected an obvious inconsistency jump, max %v: %+v", maxInco, merges)
	}
	count := SelectCut(merges, 8, 3, 5)
	if count != 4 {
		t.Fatalf("the obvious jump yields 4 clusters, got %d", count)
	}
	got := CutClusters(merges, 8, count)
	want := [][]int{{0, 1}, {2, 3}, {4, 5}, {6, 7}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("cut clusters: got %v, want %v", got, want)
	}
}

func TestSelectCutFallbacks(t *testing.T) {
	// Two leaves: no cut can reach 3, so every leaf is its own cluster.
	two := Ward(matrixFromCoords([]float64{0, 1}))
	if got := SelectCut(two, 2, 3, 5); got != 2 {
		t.Fatalf("n < range floor must fall back to n clusters, got %d", got)
	}
}

func TestMedianAndPercentile(t *testing.T) {
	merges := []Merge{{Distance: 1}, {Distance: 2}, {Distance: 3}, {Distance: 9}}
	if m := MedianMergeDistance(merges); m != 2 {
		t.Fatalf("lower median of [1 2 3 9]: got %v, want 2", m)
	}
	// Interpolated 90th percentile: position 0.9*(4-1) = 2.7 -> 3 + 0.7*(9-3).
	if p := PercentileMergeDistance(merges, 0.9); p < 7.1 || p > 7.3 {
		t.Fatalf("p90 of [1 2 3 9]: got %v, want 7.2", p)
	}
	if p := PercentileMergeDistance(merges[:1], 0.9); p != 1 {
		t.Fatalf("p90 of a single merge: got %v", p)
	}
}

func TestCutClustersBoundary(t *testing.T) {
	dist := matrixFromCoords([]float64{0, 0.1, 10, 10.1})
	merges := Ward(dist)
	if got := CutClusters(merges, 4, 1); len(got) != 1 || len(got[0]) != 4 {
		t.Fatalf("count 1 must return everything: %v", got)
	}
	if got := CutClusters(merges, 4, 4); len(got) != 4 {
		t.Fatalf("count n must return singletons: %v", got)
	}
}
