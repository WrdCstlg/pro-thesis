package cluster

import "testing"

// The five consensus branches, one test each, in the spec's order. The
// mutation checks hang off these: dropping the probability gate flips
// branch 3b, and zeroing the Jaccard threshold flips branch 5.

func TestConsensusBothNoiseIsNoise(t *testing.T) {
	a := ConsensusPoint(NoiseLabel, NoiseLabel, 0, nil, nil)
	if a.Status != StatusNoise || a.Cluster != "" {
		t.Fatalf("both algorithms noise must stay NOISE, got %+v", a)
	}
}

func TestConsensusAgreementIsCandidate(t *testing.T) {
	dbMembers := map[int][]int{0: {0, 1, 2, 3, 4}}
	hMembers := map[int][]int{0: {0, 1, 2, 3, 5}}
	a := ConsensusPoint(0, 0, 0.9, dbMembers, hMembers) // jaccard 4/6 > 0.6
	if a.Status != StatusCandidate {
		t.Fatalf("agreeing clusters must be CANDIDATE, got %+v", a)
	}
	if !a.Agree {
		t.Fatalf("agree branch must set Agree, got %+v", a)
	}
}

func TestConsensusHDBSCANOnlyHighProbabilityIsCandidate(t *testing.T) {
	a := ConsensusPoint(NoiseLabel, 2, 0.9, nil, nil)
	if a.Status != StatusCandidate || a.Agree {
		t.Fatalf("HDBSCAN-only membership with prob > 0.7 must be CANDIDATE, got %+v", a)
	}
}

func TestConsensusHDBSCANOnlyLowProbabilityIsNoise(t *testing.T) {
	a := ConsensusPoint(NoiseLabel, 2, 0.5, nil, nil)
	if a.Status != StatusNoise {
		t.Fatalf("HDBSCAN-only membership with prob <= 0.7 must be NOISE, got %+v", a)
	}
}

func TestConsensusDBSCANOnlyIsContested(t *testing.T) {
	a := ConsensusPoint(1, NoiseLabel, 0, nil, nil)
	if a.Status != StatusContested {
		t.Fatalf("DBSCAN-only membership must be CONTESTED, got %+v", a)
	}
}

func TestConsensusDisagreeingClustersAreContested(t *testing.T) {
	dbMembers := map[int][]int{0: {0, 1, 2, 3}}
	hMembers := map[int][]int{0: {0, 5, 6, 7}}
	a := ConsensusPoint(0, 0, 0.9, dbMembers, hMembers) // jaccard 1/7 <= 0.6
	if a.Status != StatusContested {
		t.Fatalf("clusters with jaccard <= 0.6 must be CONTESTED, got %+v", a)
	}
}

func TestJaccardKnownValues(t *testing.T) {
	if j := Jaccard([]int{0, 1, 2, 3, 4}, []int{0, 1, 2, 3, 5}); j < 0.66 || j > 0.67 {
		t.Fatalf("jaccard 4/6: got %v", j)
	}
	if j := Jaccard([]int{0, 1}, []int{0, 1}); j != 1 {
		t.Fatalf("identical sets: got %v", j)
	}
	if j := Jaccard([]int{0}, []int{1}); j != 0 {
		t.Fatalf("disjoint sets: got %v", j)
	}
}

// ConsensusAssign must hand every noise point a noise assignment and every
// clustered point a stable cluster key: reassigning noise into the nearest
// cluster (the "noise is sacred" mutation) turns this red.
func TestConsensusAssignKeepsNoiseOutOfClusters(t *testing.T) {
	dbLabels := []int{0, 0, NoiseLabel}
	hdb := HDBSCANResult{
		Labels:        []int{0, 0, NoiseLabel},
		Probabilities: []float64{1, 1, 0},
	}
	as := ConsensusAssign(dbLabels, hdb)
	if len(as) != 3 {
		t.Fatalf("assignments: %v", as)
	}
	if as[0].Status != StatusCandidate || as[0].Cluster == "" {
		t.Fatalf("clustered point: %+v", as[0])
	}
	if as[2].Status != StatusNoise || as[2].Cluster != "" {
		t.Fatalf("noise point must keep empty cluster key: %+v", as[2])
	}
	if as[0].Cluster != as[1].Cluster {
		t.Fatalf("same-cluster points must share a key: %+v", as)
	}
}
