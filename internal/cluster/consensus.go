package cluster

// consensus.go reconciles the two Layer 1 clusterings point by point, in the
// spec's order:
//
//	DBSCAN NOISE and HDBSCAN NOISE -> NOISE (genuinely novel, stays unattributed)
//	both cluster P and their clusters have Jaccard overlap > 0.6 -> CANDIDATE
//	HDBSCAN clusters P with probability > 0.7 but DBSCAN says NOISE -> CANDIDATE
//	DBSCAN clusters P but HDBSCAN NOISE -> CONTESTED
//	both cluster P but Jaccard overlap <= 0.6 -> CONTESTED
//	all other cases -> NOISE
//
// Noise is sacred: nothing here, and nothing downstream, ever reassigns a
// NOISE point into a cluster. That is the property the "noise is sacred"
// mutation test guards.

const (
	// JaccardThreshold is the member-set overlap above which two algorithms'
	// clusters are called the same cluster.
	JaccardThreshold = 0.6
	// HDBSCANProbThreshold is the membership probability above which an
	// HDBSCAN-only membership is trusted over DBSCAN's NOISE.
	HDBSCANProbThreshold = 0.7
)

// PointStatus is one outcome's consensus verdict.
type PointStatus string

const (
	StatusNoise     PointStatus = "NOISE"
	StatusCandidate PointStatus = "CANDIDATE"
	StatusContested PointStatus = "CONTESTED"
)

// Assignment is the consensus result for one point: its status, the cluster
// key it belongs to ("" for noise), and whether the two algorithms agreed.
type Assignment struct {
	Status  PointStatus
	Cluster string
	Agree   bool
}

// Jaccard is the overlap of two member sets. The empty/empty case is
// defined as 1 (identical sets) and never arises from real labels.
func Jaccard(a, b []int) float64 {
	if len(a) == 0 && len(b) == 0 {
		return 1
	}
	set := make(map[int]bool, len(a))
	inter := 0
	for _, x := range a {
		set[x] = true
	}
	for _, y := range b {
		if set[y] {
			inter++
		}
	}
	union := len(a) + len(b) - inter
	return float64(inter) / float64(union)
}

// membersByLabel groups point indices by cluster label (noise excluded).
func membersByLabel(labels []int) map[int][]int {
	out := map[int][]int{}
	for i, l := range labels {
		if l >= 0 {
			out[l] = append(out[l], i)
		}
	}
	return out
}

// ConsensusPoint applies the consensus rules to one point. dbMembers and
// hMembers map cluster label to member indices and are consulted only when
// both algorithms clustered the point.
func ConsensusPoint(dbLabel, hLabel int, hProb float64, dbMembers, hMembers map[int][]int) Assignment {
	dbNoise := dbLabel < 0
	hNoise := hLabel < 0
	switch {
	case dbNoise && hNoise:
		return Assignment{Status: StatusNoise}
	case !dbNoise && !hNoise:
		j := Jaccard(dbMembers[dbLabel], hMembers[hLabel])
		if j > JaccardThreshold {
			return Assignment{Status: StatusCandidate, Cluster: hKey(hLabel), Agree: true}
		}
		return Assignment{Status: StatusContested, Cluster: hKey(hLabel)}
	case dbNoise:
		// HDBSCAN clustered the point while DBSCAN saw noise: trust it only
		// when the membership is strong (it caught density variance).
		if hProb > HDBSCANProbThreshold {
			return Assignment{Status: StatusCandidate, Cluster: hKey(hLabel)}
		}
		return Assignment{Status: StatusNoise}
	default:
		// DBSCAN clustered the point while HDBSCAN saw noise: a low-stability
		// cluster, for human review.
		return Assignment{Status: StatusContested, Cluster: dKey(dbLabel)}
	}
}

func hKey(label int) string { return "h:" + itoaLabel(label) }
func dKey(label int) string { return "d:" + itoaLabel(label) }

func itoaLabel(v int) string {
	if v == 0 {
		return "0"
	}
	var buf [8]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}

// ConsensusAssign applies ConsensusPoint to every point.
func ConsensusAssign(dbLabels []int, hdb HDBSCANResult) []Assignment {
	dbMembers := membersByLabel(dbLabels)
	hMembers := membersByLabel(hdb.Labels)
	out := make([]Assignment, len(dbLabels))
	for i := range out {
		out[i] = ConsensusPoint(dbLabels[i], hdb.Labels[i], hdb.Probabilities[i], dbMembers, hMembers)
	}
	return out
}
