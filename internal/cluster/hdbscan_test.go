package cluster

import "testing"

// The same synthetic shape as the DBSCAN test: three dense clusters of
// sixteen and two isolated points, min_cluster_size = 3. Reference HDBSCAN
// semantics: an isolated point that falls out of a SELECTED cluster is a
// low-probability member of it, not noise; noise is what falls out above
// every selected cluster. The consensus layer is what keeps these outliers
// out (DBSCAN says noise and the probability is far below 0.7).
func hdbscanTestMatrix() ([][]float64, int) {
	var coords []float64
	for i := 0; i < 16; i++ {
		coords = append(coords, float64(i))
	}
	for i := 0; i < 16; i++ {
		coords = append(coords, 100+float64(i))
	}
	for i := 0; i < 16; i++ {
		coords = append(coords, 200+float64(i))
	}
	coords = append(coords, 50, 150)
	return matrixFromCoords(coords), 50 // n
}

func TestHDBSCANFindsTheThreeClusters(t *testing.T) {
	dist, n := hdbscanTestMatrix()
	res := HDBSCAN(dist, 3)
	if len(res.Labels) != n || len(res.Probabilities) != n {
		t.Fatalf("result shapes: labels %d probs %d, want %d", len(res.Labels), len(res.Probabilities), n)
	}
	sizes := clusterSizes(res.Labels)
	if len(sizes) != 3 {
		t.Fatalf("expected 3 clusters, got %d: %v", len(sizes), res.Labels)
	}
	// Every dense point is clustered, and each block shares one label.
	for _, block := range [][2]int{{0, 16}, {16, 32}, {32, 48}} {
		first := res.Labels[block[0]]
		if first == NoiseLabel {
			t.Fatalf("dense block %v is noise: %v", block, res.Labels)
		}
		for i := block[0]; i < block[1]; i++ {
			if res.Labels[i] != first {
				t.Fatalf("dense block %v split: %v", block, res.Labels)
			}
		}
	}
	if res.Labels[0] == res.Labels[16] || res.Labels[16] == res.Labels[32] || res.Labels[0] == res.Labels[32] {
		t.Fatalf("distinct clusters share a label: %v", res.Labels)
	}
	// The outliers attach weakly, below the consensus trust threshold.
	if res.Probabilities[48] >= HDBSCANProbThreshold || res.Probabilities[49] >= HDBSCANProbThreshold {
		t.Fatalf("outliers must be weak members: %v %v", res.Probabilities[48], res.Probabilities[49])
	}
	if res.Labels[48] == NoiseLabel && res.Labels[49] == NoiseLabel {
		t.Fatalf("outliers falling out of selected clusters are weak members, not noise: %v", res.Labels)
	}
}

// Core members of a dense cluster must carry high membership probability;
// probabilities stay inside [0,1] everywhere.
func TestHDBSCANMembershipProbabilities(t *testing.T) {
	dist, _ := hdbscanTestMatrix()
	res := HDBSCAN(dist, 3)
	for i, p := range res.Probabilities {
		if p < 0 || p > 1 {
			t.Fatalf("probability out of range at %d: %v", i, p)
		}
	}
	// The geometric centre of cluster A is as core as a point gets.
	if p := res.Probabilities[7]; p <= 0.5 {
		t.Fatalf("core member probability %v, want > 0.5", p)
	}
	if p := res.Probabilities[23]; p <= 0.5 {
		t.Fatalf("core member probability %v, want > 0.5", p)
	}
}

func TestHDBSCANTinyInputsAreNoise(t *testing.T) {
	if got := HDBSCAN(nil, 3); len(got.Labels) != 0 {
		t.Fatalf("empty input: %+v", got)
	}
	two := HDBSCAN(matrixFromCoords([]float64{0, 0.1}), 3)
	for i, l := range two.Labels {
		if l != NoiseLabel {
			t.Fatalf("n < min_cluster_size must be all noise, point %d labelled %d", i, l)
		}
	}
}

// Identical points (zero distances) must not divide by zero: they cluster
// together with maximal membership.
func TestHDBSCANIdenticalPoints(t *testing.T) {
	dist := matrixFromCoords([]float64{5, 5, 5, 5})
	res := HDBSCAN(dist, 2)
	if len(clusterSizes(res.Labels)) != 1 {
		t.Fatalf("identical points must form one cluster: %v", res.Labels)
	}
	for i, p := range res.Probabilities {
		if p <= 0 || p > 1 {
			t.Fatalf("probability out of range at %d: %v", i, p)
		}
	}
}

func TestHDBSCANDeterministic(t *testing.T) {
	dist, _ := hdbscanTestMatrix()
	a := HDBSCAN(dist, 3)
	b := HDBSCAN(dist, 3)
	for i := range a.Labels {
		if a.Labels[i] != b.Labels[i] || a.Probabilities[i] != b.Probabilities[i] {
			t.Fatalf("non-deterministic at %d: %v/%v vs %v/%v",
				i, a.Labels[i], a.Probabilities[i], b.Labels[i], b.Probabilities[i])
		}
	}
}
