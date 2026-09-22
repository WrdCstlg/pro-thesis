package cluster

import (
	"math"
	"sort"
)

// hierarchy.go is agglomerative hierarchical clustering with Ward linkage
// over a pairwise distance matrix, using Lance-Williams updates on squared
// distances (no coordinate access needed):
//
//	d2(u,v) = ((|a|+|v|) d2(a,v) + (|b|+|v|) d2(b,v) - |v| d2(a,b)) / (|a|+|b|+|v|)
//
// seeded with d2(i,j) = d(i,j)^2 / 2 so singleton merges read as the
// variance increase Ward minimizes. Merge distances are reported as
// sqrt(d2): they START on the Gower scale but are not bounded by it, because
// the Lance-Williams update grows distances with cluster size. Every tie
// breaks by (distance, smallest member id, next smallest member id): a total
// order, so the dendrogram is a function of the matrix alone.

// Merge is one agglomeration step.
type Merge struct {
	Distance      float64 // sqrt of the Lance-Williams squared distance
	Size          int
	Members       []int   // sorted leaf indices
	Inconsistency float64 // inconsistency coefficient, see Ward
}

// wardCluster is an active cluster during agglomeration.
type wardCluster struct {
	members []int
}

// Ward runs the agglomeration and returns the n-1 merges in order.
func Ward(dist [][]float64) []Merge {
	n := len(dist)
	if n < 2 {
		return nil
	}
	d2 := make([][]float64, n)
	for i := range d2 {
		d2[i] = make([]float64, n)
		for j := range d2 {
			d2[i][j] = dist[i][j] * dist[i][j] / 2
		}
	}
	clusters := make([]wardCluster, n)
	size := make([]int, n)
	for i := range clusters {
		clusters[i] = wardCluster{members: []int{i}}
		size[i] = 1
	}
	active := make([]int, n)
	for i := range active {
		active[i] = i
	}

	merges := make([]Merge, 0, n-1)
	nextID := n
	for len(active) > 1 {
		// Nearest active pair, ties by (distance, min member, next member).
		bi, bj := -1, -1
		bestD := 0.0
		for x := 0; x < len(active); x++ {
			for y := x + 1; y < len(active); y++ {
				a, b := active[x], active[y]
				if bi < 0 || d2[a][b] < bestD ||
					d2[a][b] == bestD && lessMembers(clusters[a].members, clusters[b].members,
						clusters[bi].members, clusters[bj].members) {
					bi, bj, bestD = a, b, d2[a][b]
				}
			}
		}
		// Lance-Williams update into a new cluster id.
		na, nb := size[bi], size[bj]
		merged := append(append([]int(nil), clusters[bi].members...), clusters[bj].members...)
		sort.Ints(merged)
		nid := nextID
		newRow := make([]float64, nid+1)
		for _, v := range active {
			if v == bi || v == bj {
				continue
			}
			num := float64(na+size[v])*d2[bi][v] + float64(nb+size[v])*d2[bj][v] - float64(size[v])*d2[bi][bj]
			den := float64(na + nb + size[v])
			newRow[v] = num / den
		}
		clusters = append(clusters, wardCluster{members: merged})
		size = append(size, na+nb)
		d2 = append(d2, newRow)
		for i := 0; i < nid; i++ {
			d2[i] = append(d2[i], newRow[i])
		}
		merges = append(merges, Merge{
			Distance: math.Sqrt(math.Max(bestD, 0)),
			Size:     na + nb,
			Members:  merged,
		})
		// Retire the pair, activate the new cluster.
		keep := active[:0]
		for _, v := range active {
			if v != bi && v != bj {
				keep = append(keep, v)
			}
		}
		active = append(keep, nextID)
		nextID++
	}

	// Inconsistency coefficient per merge: (h - mean) / std of the up-to-3
	// preceding merge heights. The first merges have no meaningful baseline
	// and read 0.
	const window = 3
	for i := range merges {
		lo := i - window
		if lo < 0 {
			lo = 0
		}
		prev := merges[lo:i]
		if len(prev) == 0 {
			continue
		}
		mean := 0.0
		for _, m := range prev {
			mean += m.Distance
		}
		mean /= float64(len(prev))
		var sq float64
		for _, m := range prev {
			d := m.Distance - mean
			sq += d * d
		}
		std := math.Sqrt(sq / float64(len(prev)))
		merges[i].Inconsistency = (merges[i].Distance - mean) / (std + 1e-12)
	}
	return merges
}

// lessMembers orders two cluster pairs lexicographically by member list.
func lessMembers(a1, b1, a2, b2 []int) bool {
	x := append(append([]int(nil), a1...), b1...)
	y := append(append([]int(nil), a2...), b2...)
	for i := 0; i < len(x) && i < len(y); i++ {
		if x[i] != y[i] {
			return x[i] < y[i]
		}
	}
	return len(x) < len(y)
}

// MedianMergeDistance is the lower median of the merge distances.
func MedianMergeDistance(merges []Merge) float64 {
	if len(merges) == 0 {
		return 0
	}
	ds := make([]float64, len(merges))
	for i, m := range merges {
		ds[i] = m.Distance
	}
	sort.Float64s(ds)
	return ds[(len(ds)-1)/2]
}

// PercentileMergeDistance is the linear-interpolation percentile of the
// merge distances (position p*(m-1) in the sorted order).
func PercentileMergeDistance(merges []Merge, p float64) float64 {
	if len(merges) == 0 {
		return 0
	}
	ds := make([]float64, len(merges))
	for i, m := range merges {
		ds[i] = m.Distance
	}
	sort.Float64s(ds)
	if len(ds) == 1 {
		return ds[0]
	}
	pos := p * float64(len(ds)-1)
	lo := int(math.Floor(pos))
	hi := lo + 1
	if hi >= len(ds) {
		return ds[len(ds)-1]
	}
	frac := pos - float64(lo)
	return ds[lo] + frac*(ds[hi]-ds[lo])
}

// SelectCut chooses how many clusters to keep, within [lo, hi], by cutting
// just below the merge with the largest inconsistency coefficient (the
// biggest jump). Ties prefer fewer clusters (a higher cut). When n is
// smaller than lo, every leaf is its own cluster.
func SelectCut(merges []Merge, n, lo, hi int) int {
	bestCount, bestScore := -1, -1.0
	for i := -1; i < len(merges); i++ {
		count := n - (i + 1)
		if count < lo || count > hi {
			continue
		}
		score := 0.0
		if i+1 < len(merges) {
			score = merges[i+1].Inconsistency
		}
		if score > bestScore || score == bestScore && bestCount >= 0 && count < bestCount {
			bestCount, bestScore = count, score
		}
	}
	if bestCount < 0 {
		if n < lo {
			return n
		}
		return hi
	}
	return bestCount
}

// CutClusters partitions the leaves into count clusters by applying the
// first n-count merges. Groups are ordered by smallest member, members
// sorted.
func CutClusters(merges []Merge, n, count int) [][]int {
	return CutAtThresholdIndex(merges, n, n-count)
}

// CutAtThresholdIndex applies the first k merges and returns the groups.
func CutAtThresholdIndex(merges []Merge, n, k int) [][]int {
	if k < 0 {
		k = 0
	}
	if k > len(merges) {
		k = len(merges)
	}
	parent := make([]int, n)
	for i := range parent {
		parent[i] = i
	}
	var find func(x int) int
	find = func(x int) int {
		for parent[x] != x {
			parent[x] = parent[parent[x]]
			x = parent[x]
		}
		return x
	}
	for i := 0; i < k; i++ {
		mem := merges[i].Members
		for _, p := range mem[1:] {
			parent[find(p)] = find(mem[0])
		}
	}
	groups := map[int][]int{}
	for i := 0; i < n; i++ {
		r := find(i)
		groups[r] = append(groups[r], i)
	}
	out := make([][]int, 0, len(groups))
	for _, g := range groups {
		sort.Ints(g)
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i][0] < out[j][0] })
	return out
}

// CutAtDistance partitions the leaves by applying every merge whose distance
// is at most threshold.
func CutAtDistance(merges []Merge, n int, threshold float64) [][]int {
	k := 0
	for k < len(merges) && merges[k].Distance <= threshold {
		k++
	}
	return CutAtThresholdIndex(merges, n, k)
}
