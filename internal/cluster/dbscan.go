package cluster

import "sort"

// dbscan.go is a pure-Go DBSCAN over a precomputed distance matrix:
// eps-neighborhood range queries, core-point detection, cluster expansion,
// and NOISE for everything unreachable. Deterministic: points are visited in
// index order and neighbor lists are sorted by (distance, index), so labels
// depend only on the matrix.

// NoiseLabel marks points assigned to no cluster.
const NoiseLabel = -1

// MinPts implements the spec rule: max(3, 2 x feature dimensions).
func MinPts(featureDims int) int {
	if m := 2 * featureDims; m > 3 {
		return m
	}
	return 3
}

// sortedNeighbors returns point i's neighbors by (distance, index).
func sortedNeighbors(dist [][]float64, i int) []int {
	n := len(dist)
	idx := make([]int, 0, n-1)
	for j := 0; j < n; j++ {
		if j != i {
			idx = append(idx, j)
		}
	}
	sort.Slice(idx, func(a, b int) bool {
		if dist[i][idx[a]] != dist[i][idx[b]] {
			return dist[i][idx[a]] < dist[i][idx[b]]
		}
		return idx[a] < idx[b]
	})
	return idx
}

// DBSCAN clusters the distance matrix. minPts counts the point itself, per
// the classic definition. Returns one label per point, NoiseLabel for noise.
func DBSCAN(dist [][]float64, eps float64, minPts int) []int {
	n := len(dist)
	labels := make([]int, n)
	if n == 0 {
		return labels
	}
	neighbors := make([][]int, n)
	core := make([]bool, n)
	for i := 0; i < n; i++ {
		for _, j := range sortedNeighbors(dist, i) {
			if dist[i][j] > eps {
				break
			}
			neighbors[i] = append(neighbors[i], j)
		}
		core[i] = len(neighbors[i])+1 >= minPts
	}

	nextCluster := 0
	for i := 0; i < n; i++ {
		labels[i] = NoiseLabel
	}
	visited := make([]bool, n)
	for i := 0; i < n; i++ {
		if visited[i] {
			continue
		}
		visited[i] = true
		if !core[i] {
			continue // noise for now; a later expansion may claim it as border
		}
		labels[i] = nextCluster
		queue := append([]int(nil), neighbors[i]...)
		for len(queue) > 0 {
			j := queue[0]
			queue = queue[1:]
			if !visited[j] {
				visited[j] = true
				if core[j] {
					queue = append(queue, neighbors[j]...)
				}
			}
			if labels[j] == NoiseLabel {
				labels[j] = nextCluster
			}
		}
		nextCluster++
	}
	return labels
}

// AutoEpsilon tunes eps by the k-distance elbow method with k = minPts: the
// k-distance of every point (k-th nearest neighbor, excluding the point
// itself) is sorted ascending and the point of maximum curvature (largest
// second difference) is the elbow. When the curve has no positive curvature
// anywhere (monotonic/concave profiles), the median k-distance is the
// documented fallback.
func AutoEpsilon(dist [][]float64, minPts int) float64 {
	n := len(dist)
	if n < 2 {
		return 0
	}
	k := minPts
	if k > n-1 {
		k = n - 1
	}
	if k < 1 {
		k = 1
	}
	kd := make([]float64, n)
	for i := 0; i < n; i++ {
		nb := sortedNeighbors(dist, i)
		kd[i] = dist[i][nb[k-1]]
	}
	sort.Float64s(kd)
	median := kd[(n-1)/2]
	if n < 3 {
		return median
	}
	best, bestCurve := -1, 0.0
	for i := 1; i < n-1; i++ {
		curve := kd[i+1] - 2*kd[i] + kd[i-1]
		if curve > bestCurve {
			best, bestCurve = i, curve
		}
	}
	if best < 0 {
		return median
	}
	return kd[best]
}
