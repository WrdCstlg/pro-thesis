package cluster

import (
	"testing"
)

// matrixFromCoords builds a Euclidean distance matrix over 1-D coordinates,
// which keeps every expectation in these tests exact and hand-checkable.
func matrixFromCoords(coords []float64) [][]float64 {
	n := len(coords)
	m := make([][]float64, n)
	for i := range m {
		m[i] = make([]float64, n)
		for j := range m {
			d := coords[i] - coords[j]
			if d < 0 {
				d = -d
			}
			m[i][j] = d
		}
	}
	return m
}

// clusterSizes tallies non-noise label populations.
func clusterSizes(labels []int) map[int]int {
	out := map[int]int{}
	for _, l := range labels {
		if l >= 0 {
			out[l]++
		}
	}
	return out
}

// Three dense clusters of sixteen points each, plus two isolated noise
// points: the spec's DBSCAN shape.
func TestDBSCANFindsThreeClustersAndTwoNoise(t *testing.T) {
	var coords []float64
	for i := 0; i < 16; i++ {
		coords = append(coords, float64(i)) // cluster A: 0..15
	}
	for i := 0; i < 16; i++ {
		coords = append(coords, 100+float64(i)) // cluster B: 100..115
	}
	for i := 0; i < 16; i++ {
		coords = append(coords, 200+float64(i)) // cluster C: 200..215
	}
	coords = append(coords, 50, 150) // noise
	labels := DBSCAN(matrixFromCoords(coords), 3.5, 3)

	sizes := clusterSizes(labels)
	if len(sizes) != 3 {
		t.Fatalf("expected 3 clusters, got %d: labels %v", len(sizes), labels)
	}
	for l, s := range sizes {
		if s != 16 {
			t.Fatalf("cluster %d has %d members, want 16", l, s)
		}
	}
	if labels[48] != NoiseLabel || labels[49] != NoiseLabel {
		t.Fatalf("isolated points must be noise, got %d %d", labels[48], labels[49])
	}
	// Cluster membership is by construction: first 16 share a label, etc.
	if labels[0] != labels[15] || labels[16] != labels[31] || labels[32] != labels[47] {
		t.Fatalf("cluster members split apart: %v", labels)
	}
	if labels[0] == labels[16] || labels[16] == labels[32] || labels[0] == labels[32] {
		t.Fatalf("distinct clusters share a label: %v", labels)
	}
}

// Border points (reachable but not core) must still join the cluster.
func TestDBSCANBorderPointsJoin(t *testing.T) {
	// core group 0,1,2,3 dense; point 4.5 is within eps of 3 only.
	coords := []float64{0, 1, 2, 3, 4.5}
	labels := DBSCAN(matrixFromCoords(coords), 2.0, 3)
	if labels[4] == NoiseLabel {
		t.Fatalf("border point must join the cluster, got noise: %v", labels)
	}
	if labels[4] != labels[0] {
		t.Fatalf("border point joined a different cluster: %v", labels)
	}
}

func TestDBSCANEmptyAndTiny(t *testing.T) {
	if got := DBSCAN(nil, 1, 3); len(got) != 0 {
		t.Fatalf("empty input: %v", got)
	}
	one := DBSCAN(matrixFromCoords([]float64{7}), 1, 3)
	if len(one) != 1 || one[0] != NoiseLabel {
		t.Fatalf("single point must be noise: %v", one)
	}
}

// A k-distance profile with one obvious elbow must select it; a flat
// (monotonic) profile must fall back to the median.
func TestAutoEpsilonElbowAndMedianFallback(t *testing.T) {
	// Three tight clusters of four plus a distant pair: the k-distance curve
	// jumps at the pair, and the elbow sits on the last in-cluster distance.
	var coords []float64
	for _, base := range []float64{0, 10, 20} {
		for i := 0; i < 4; i++ {
			coords = append(coords, base+0.05*float64(i))
		}
	}
	coords = append(coords, 40, 40.05)
	dist := matrixFromCoords(coords)
	eps := AutoEpsilon(dist, 3)
	if eps < 0.1 || eps > 5 {
		t.Fatalf("elbow epsilon %v is not between intra- and inter-cluster distances", eps)
	}
	labels := DBSCAN(dist, eps, 3)
	if got := len(clusterSizes(labels)); got != 3 {
		t.Fatalf("auto-tuned epsilon should recover 3 clusters, got %d", got)
	}
	if labels[12] != NoiseLabel || labels[13] != NoiseLabel {
		t.Fatalf("the distant pair must stay noise, got %d %d", labels[12], labels[13])
	}

	// Uniform spacing: the k-distance curve is flat, so the median fallback
	// applies and must still separate the two clusters.
	flat := matrixFromCoords([]float64{0, 0.2, 0.4, 0.6, 10, 10.2, 10.4, 10.6})
	epsFlat := AutoEpsilon(flat, 2)
	if epsFlat > 1 {
		t.Fatalf("flat profile must fall back near the median spacing, got %v", epsFlat)
	}
	if got := len(clusterSizes(DBSCAN(flat, epsFlat, 2))); got != 2 {
		t.Fatalf("median-fallback epsilon should recover 2 clusters, got %d", got)
	}
}

func TestMinPtsRule(t *testing.T) {
	if got := MinPts(8); got != 16 {
		t.Fatalf("minPts = max(3, 2*dims): got %d, want 16", got)
	}
	if got := MinPts(1); got != 3 {
		t.Fatalf("minPts floor: got %d, want 3", got)
	}
}

// Labels must be assigned in a deterministic order on tied geometry: the same
// matrix twice yields byte-identical label vectors.
func TestDBSCANDeterministic(t *testing.T) {
	coords := []float64{0, 1, 2, 10, 11, 12, 5}
	dist := matrixFromCoords(coords)
	a := DBSCAN(dist, 2.5, 2)
	b := DBSCAN(dist, 2.5, 2)
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("non-deterministic labels: %v vs %v", a, b)
		}
	}
}
