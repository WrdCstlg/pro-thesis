package cluster

import "sort"

// hdbscan.go is a pure-Go HDBSCAN over a precomputed distance matrix:
//
//  1. core_k distances (k = min_cluster_size, k-th nearest excluding self)
//  2. mutual reachability max(core(a), core(b), d(a,b))
//  3. minimum spanning tree (Prim, O(n^2), deterministic tie-breaks)
//  4. single-linkage hierarchy from the sorted MST edges
//  5. condensed tree under min_cluster_size, with per-point exit lambdas
//     (lambda = 1/distance) at every fall-out and split
//  6. stability = sum(lambda_exit - lambda_birth) per cluster and EOM
//     (excess of mass) selection: bottom-up, a cluster is selected when its
//     own stability is at least the sum of its children's selected stability
//  7. membership probability = lambda_exit(point) / lambda_max(cluster)
//
// Points in no selected cluster are NOISE with probability 0. When
// n < min_cluster_size every point is noise. All tie-breaks are total orders
// ((weight, index) for Prim and the edge sort, min member id for label
// assignment), so the result depends only on the distance matrix.

// HDBSCANResult carries per-point labels and membership probabilities, plus
// the selected clusters' stabilities indexed by label.
type HDBSCANResult struct {
	Labels        []int
	Probabilities []float64
	Stabilities   []float64
}

const lambdaCap = 1e12

func lambdaOf(d float64) float64 {
	if d <= 0 {
		return lambdaCap
	}
	return 1 / d
}

type hEdge struct {
	a, b   int
	weight float64
}

type hNode struct {
	left, right int // child node ids; leaves are 0..n-1
	dist        float64
	size        int
}

// HDBSCAN clusters the distance matrix with the given min_cluster_size using
// the 'eom' cluster selection method.
func HDBSCAN(dist [][]float64, minClusterSize int) HDBSCANResult {
	n := len(dist)
	res := HDBSCANResult{
		Labels:        make([]int, n),
		Probabilities: make([]float64, n),
	}
	for i := range res.Labels {
		res.Labels[i] = NoiseLabel
	}
	if minClusterSize < 2 {
		minClusterSize = 2
	}
	if n < minClusterSize {
		return res
	}

	// Core distances.
	k := minClusterSize
	if k > n-1 {
		k = n - 1
	}
	core := make([]float64, n)
	for i := 0; i < n; i++ {
		nb := sortedNeighbors(dist, i)
		core[i] = dist[i][nb[k-1]]
	}
	mr := func(a, b int) float64 {
		d := dist[a][b]
		if core[a] > d {
			d = core[a]
		}
		if core[b] > d {
			d = core[b]
		}
		return d
	}

	// Prim MST from vertex 0.
	inTree := make([]bool, n)
	key := make([]float64, n)
	parent := make([]int, n)
	for i := range key {
		key[i] = -1
	}
	inTree[0] = true
	for j := 1; j < n; j++ {
		key[j] = mr(0, j)
		parent[j] = 0
	}
	var edges []hEdge
	for len(edges) < n-1 {
		best, bestW := -1, 0.0
		for v := 0; v < n; v++ {
			if inTree[v] || key[v] < 0 {
				continue
			}
			if best < 0 || key[v] < bestW || key[v] == bestW && v < best {
				best, bestW = v, key[v]
			}
		}
		inTree[best] = true
		edges = append(edges, hEdge{a: parent[best], b: best, weight: bestW})
		for v := 0; v < n; v++ {
			if inTree[v] {
				continue
			}
			w := mr(best, v)
			if key[v] < 0 || w < key[v] {
				key[v] = w
				parent[v] = best
			}
		}
	}

	// Single-linkage hierarchy from the sorted MST edges.
	sort.Slice(edges, func(i, j int) bool {
		ai, bi := edges[i].a, edges[i].b
		aj, bj := edges[j].a, edges[j].b
		if ai > bi {
			ai, bi = bi, ai
		}
		if aj > bj {
			aj, bj = bj, aj
		}
		if edges[i].weight != edges[j].weight {
			return edges[i].weight < edges[j].weight
		}
		if ai != aj {
			return ai < aj
		}
		return bi < bj
	})
	nodes := make([]hNode, 2*n-1)
	for i := 0; i < n; i++ {
		nodes[i] = hNode{left: -1, right: -1, size: 1}
	}
	uf := make([]int, 2*n-1)
	ufNode := make([]int, n) // leaf -> current cluster node id
	for i := range uf {
		uf[i] = i
	}
	for i := range ufNode {
		ufNode[i] = i
	}
	var find func(x int) int
	find = func(x int) int {
		for uf[x] != x {
			uf[x] = uf[uf[x]]
			x = uf[x]
		}
		return x
	}
	next := n
	for _, e := range edges {
		ra, rb := find(ufNode[e.a]), find(ufNode[e.b])
		id := next
		next++
		nodes[id] = hNode{
			left: ra, right: rb, dist: e.weight,
			size: nodes[ra].size + nodes[rb].size,
		}
		uf[ra], uf[rb] = id, id
		ufNode[e.a], ufNode[e.b] = id, id
	}
	root := next - 1

	pointsUnder := func(v int) []int {
		var out []int
		stack := []int{v}
		for len(stack) > 0 {
			x := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if x < n {
				out = append(out, x)
				continue
			}
			stack = append(stack, nodes[x].left, nodes[x].right)
		}
		sort.Ints(out)
		return out
	}

	// Condensed tree.
	type ccluster struct {
		birth    float64
		top      int // hierarchy node whose points the cluster covers
		exits    map[int]float64
		children []*ccluster
		stab     float64
		sel      bool
	}
	rootC := &ccluster{birth: 0, top: root, exits: map[int]float64{}}
	var walk func(v int, c *ccluster)
	walk = func(v int, c *ccluster) {
		nd := nodes[v]
		l := lambdaOf(nd.dist)
		bigL := nodes[nd.left].size >= minClusterSize
		bigR := nodes[nd.right].size >= minClusterSize
		switch {
		case bigL && bigR:
			for _, p := range pointsUnder(v) {
				c.exits[p] = l
			}
			cl := &ccluster{birth: l, top: nd.left, exits: map[int]float64{}}
			cr := &ccluster{birth: l, top: nd.right, exits: map[int]float64{}}
			c.children = append(c.children, cl, cr)
			walk(nd.left, cl)
			walk(nd.right, cr)
		case bigL:
			for _, p := range pointsUnder(nd.right) {
				c.exits[p] = l
			}
			walk(nd.left, c)
		case bigR:
			for _, p := range pointsUnder(nd.left) {
				c.exits[p] = l
			}
			walk(nd.right, c)
		default:
			for _, p := range pointsUnder(v) {
				c.exits[p] = l
			}
		}
	}
	walk(root, rootC)

	var stability func(c *ccluster) float64
	stability = func(c *ccluster) float64 {
		s := 0.0
		for _, exit := range c.exits {
			s += exit - c.birth
		}
		c.stab = s
		return s
	}

	// EOM selection, bottom-up.
	var markDeselected func(c *ccluster)
	markDeselected = func(c *ccluster) {
		c.sel = false
		for _, ch := range c.children {
			markDeselected(ch)
		}
	}
	var selectEOM func(c *ccluster) float64
	selectEOM = func(c *ccluster) float64 {
		own := stability(c)
		sum := 0.0
		for _, ch := range c.children {
			sum += selectEOM(ch)
		}
		if own >= sum {
			c.sel = true
			for _, ch := range c.children {
				markDeselected(ch)
			}
			return own
		}
		return sum
	}
	selectEOM(rootC)

	// Collect selected clusters, order by smallest member id, assign labels.
	type selected struct {
		c       *ccluster
		members []int
	}
	var sels []selected
	var gather func(c *ccluster)
	gather = func(c *ccluster) {
		if c.sel {
			sels = append(sels, selected{c: c, members: pointsUnder(c.top)})
			return
		}
		for _, ch := range c.children {
			gather(ch)
		}
	}
	gather(rootC)
	sort.Slice(sels, func(i, j int) bool { return sels[i].members[0] < sels[j].members[0] })

	for label, s := range sels {
		res.Stabilities = append(res.Stabilities, s.c.stab)
		// exit lambda of each member relative to this selected cluster: its
		// recorded fall-out lambda, or the birth lambda of the immediate
		// child cluster it moved into.
		maxExit := 0.0
		exits := make([]float64, len(s.members))
		for mi, p := range s.members {
			exit, ok := s.c.exits[p]
			if !ok {
				for _, ch := range s.c.children {
					if containsPoint(nodes, n, ch.top, p) {
						exit = ch.birth
						break
					}
				}
			}
			exits[mi] = exit
			if exit > maxExit {
				maxExit = exit
			}
		}
		for mi, p := range s.members {
			res.Labels[p] = label
			if maxExit > 0 {
				res.Probabilities[p] = exits[mi] / maxExit
			} else {
				res.Probabilities[p] = 1
			}
			if res.Probabilities[p] > 1 {
				res.Probabilities[p] = 1
			}
		}
	}
	return res
}

func containsPoint(nodes []hNode, n, v, p int) bool {
	stack := []int{v}
	for len(stack) > 0 {
		x := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if x < n {
			if x == p {
				return true
			}
			continue
		}
		stack = append(stack, nodes[x].left, nodes[x].right)
	}
	return false
}
