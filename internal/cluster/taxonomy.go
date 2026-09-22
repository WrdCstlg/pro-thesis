package cluster

import (
	"fmt"
	"sort"
	"time"

	"gopkg.in/yaml.v3"
)

// taxonomy.go is Layer 2: Ward hierarchical clustering over the centroids of
// the Layer 1 CANDIDATE clusters and of every known-problem entry that has
// attributed outcomes, with sub-type, split and orphan detection. The
// taxonomy is an organizing lens over the registry; it never rewrites it.

// CandidateInput is one Layer 1 CANDIDATE cluster with its member items.
type CandidateInput struct {
	ID    string
	Items []Item
}

// TaxonomyReport is the prothesis.taxonomy/v1 document.
type TaxonomyReport struct {
	APIVersion string           `yaml:"apiVersion"`
	Kind       string           `yaml:"kind"`
	Metadata   TaxonomyMetadata `yaml:"metadata"`
	Dendrogram Dendrogram       `yaml:"dendrogram"`
	Insights   Insights         `yaml:"insights"`
}

type TaxonomyMetadata struct {
	Generated       string `yaml:"generated"`
	EntriesAnalyzed int    `yaml:"entries_analyzed"`
	Linkage         string `yaml:"linkage"`
	DistanceMetric  string `yaml:"distance_metric"`
}

type Dendrogram struct {
	Domains  []Domain `yaml:"domains"`
	Families []Family `yaml:"families,omitempty"`
}

type Domain struct {
	Name          string   `yaml:"name"`
	Members       []string `yaml:"members"`
	MergeDistance float64  `yaml:"merge_distance"`
	Impact        int      `yaml:"impact"` // number of entries in the domain
}

type Family struct {
	Name          string   `yaml:"name"`
	ParentDomain  string   `yaml:"parent_domain"`
	Members       []string `yaml:"members"`
	MergeDistance float64  `yaml:"merge_distance"`
}

type Insights struct {
	SubTypes        []SubType        `yaml:"sub_types"`
	SplitCandidates []SplitCandidate `yaml:"split_candidates"`
	Orphans         []Orphan         `yaml:"orphans"`
}

type SubType struct {
	Candidate      string  `yaml:"candidate"`
	NearestKP      string  `yaml:"nearest_kp"`
	MergeDistance  float64 `yaml:"merge_distance"`
	Recommendation string  `yaml:"recommendation"`
}

type SplitCandidate struct {
	Entry            string  `yaml:"entry"`
	SubClusters      int     `yaml:"sub_clusters"`
	InternalDistance float64 `yaml:"internal_distance"`
	Recommendation   string  `yaml:"recommendation"`
}

type Orphan struct {
	Candidate             string  `yaml:"candidate"`
	NearestBranchDistance float64 `yaml:"nearest_branch_distance"`
	Recommendation        string  `yaml:"recommendation"`
}

// MarshalYAML renders the report as YAML bytes.
func (r *TaxonomyReport) MarshalYAML() ([]byte, error) {
	return yaml.Marshal(r)
}

// taxLeaf is one dendrogram leaf: a CANDIDATE cluster or a KP entry, reduced
// to its centroid.
type taxLeaf struct {
	id          string
	isCandidate bool
	centroid    VerdictFeatures
}

// BuildTaxonomyReport computes the Layer 2 taxonomy. Candidate clusters come
// from the ClusterReport (status CANDIDATE only); KP centroids average the
// feature vectors of their attributed outcomes. KPs with no attributed
// outcomes have no centroid and are skipped; entries_analyzed counts the
// leaves actually clustered.
func BuildTaxonomyReport(cands []CandidateInput, kpItems map[string][]Item, generated time.Time) *TaxonomyReport {
	rep := &TaxonomyReport{
		APIVersion: "prothesis.taxonomy/v1",
		Kind:       "TaxonomyReport",
		Metadata: TaxonomyMetadata{
			Generated:      generated.UTC().Format(time.RFC3339),
			Linkage:        "ward",
			DistanceMetric: "gower",
		},
		Dendrogram: Dendrogram{Domains: []Domain{}},
		Insights: Insights{
			SubTypes:        []SubType{},
			SplitCandidates: []SplitCandidate{},
			Orphans:         []Orphan{},
		},
	}

	var leaves []taxLeaf
	cs := append([]CandidateInput(nil), cands...)
	sort.Slice(cs, func(i, j int) bool { return cs[i].ID < cs[j].ID })
	for _, c := range cs {
		if len(c.Items) == 0 {
			continue
		}
		leaves = append(leaves, taxLeaf{id: c.ID, isCandidate: true, centroid: Centroid(c.Items)})
	}
	kpIDs := make([]string, 0, len(kpItems))
	for id := range kpItems {
		kpIDs = append(kpIDs, id)
	}
	sort.Strings(kpIDs)
	for _, id := range kpIDs {
		if len(kpItems[id]) == 0 {
			continue
		}
		leaves = append(leaves, taxLeaf{id: id, centroid: Centroid(kpItems[id])})
	}
	rep.Metadata.EntriesAnalyzed = len(leaves)
	if len(leaves) == 0 {
		return rep
	}

	feats := make([]VerdictFeatures, len(leaves))
	for i, l := range leaves {
		feats[i] = l.centroid
	}
	dist := DistanceMatrix(feats)
	merges := Ward(dist)
	median := MedianMergeDistance(merges)
	p90 := PercentileMergeDistance(merges, 0.9)

	rep.Dendrogram.Domains = buildDomains(merges, leaves)
	rep.Dendrogram.Families = buildFamilies(merges, leaves, rep.Dendrogram.Domains)
	rep.Insights.SubTypes = DetectSubTypes(merges, leaves, dist, median)
	rep.Insights.SplitCandidates = DetectSplits(kpItems, median)
	rep.Insights.Orphans = DetectOrphans(merges, leaves, p90)
	return rep
}

// leafIDs renders a leaf-index group as sorted leaf ids.
func leafIDs(group []int, leaves []taxLeaf) []string {
	ids := make([]string, len(group))
	for i, g := range group {
		ids[i] = leaves[g].id
	}
	sort.Strings(ids)
	return ids
}

// groupMergeDistance is the largest internal merge distance of a group (0
// for singletons): the height at which the group became one.
func groupMergeDistance(group []int, merges []Merge) float64 {
	max := 0.0
	in := make(map[int]bool, len(group))
	for _, g := range group {
		in[g] = true
	}
	for _, m := range merges {
		allIn := true
		for _, p := range m.Members {
			if !in[p] {
				allIn = false
				break
			}
		}
		if allIn && len(m.Members) > 1 && m.Distance > max {
			max = m.Distance
		}
	}
	return max
}

func buildDomains(merges []Merge, leaves []taxLeaf) []Domain {
	count := SelectCut(merges, len(leaves), 3, 5)
	groups := CutClusters(merges, len(leaves), count)
	out := make([]Domain, 0, len(groups))
	for i, g := range groups {
		out = append(out, Domain{
			Name:          fmt.Sprintf("domain-%d", i+1),
			Members:       leafIDs(g, leaves),
			MergeDistance: groupMergeDistance(g, merges),
			Impact:        len(g),
		})
	}
	return out
}

// buildFamilies cuts at the family level (8-15 groups). When the family
// partition is identical to the domain partition, families are omitted, as
// the spec allows.
func buildFamilies(merges []Merge, leaves []taxLeaf, domains []Domain) []Family {
	count := SelectCut(merges, len(leaves), 8, 15)
	groups := CutClusters(merges, len(leaves), count)
	if samePartition(groups, leaves, domains) {
		return nil
	}
	domainOf := map[string]string{}
	for _, d := range domains {
		for _, m := range d.Members {
			domainOf[m] = d.Name
		}
	}
	out := make([]Family, 0, len(groups))
	for i, g := range groups {
		ids := leafIDs(g, leaves)
		out = append(out, Family{
			Name:          fmt.Sprintf("family-%d", i+1),
			ParentDomain:  domainOf[ids[0]],
			Members:       ids,
			MergeDistance: groupMergeDistance(g, merges),
		})
	}
	return out
}

// samePartition compares a leaf-index partition with a domain partition by
// comparing sorted member-id lists as sets.
func samePartition(groups [][]int, leaves []taxLeaf, domains []Domain) bool {
	if len(groups) != len(domains) {
		return false
	}
	groupSets := make(map[string]bool, len(groups))
	for _, g := range groups {
		groupSets[fmt.Sprint(leafIDs(g, leaves))] = true
	}
	for _, d := range domains {
		if !groupSets[fmt.Sprint(d.Members)] {
			return false
		}
	}
	return true
}

// firstMergeDistance returns the distance of the first merge whose members
// include leaf i.
func firstMergeDistance(merges []Merge, i int) float64 {
	for _, m := range merges {
		for _, p := range m.Members {
			if p == i {
				return m.Distance
			}
		}
	}
	return 0
}

// mergeJoinsKP reports whether the merge's members contain both leaf i and
// at least one KP leaf.
func mergeJoinsKP(m Merge, i int, leaves []taxLeaf) bool {
	hasI, hasKP := false, false
	for _, p := range m.Members {
		if p == i {
			hasI = true
		} else if !leaves[p].isCandidate {
			hasKP = true
		}
	}
	return hasI && hasKP
}

// DetectSubTypes finds CANDIDATEs that merge with an existing KP at less
// than 0.3 x the median merge distance: the candidate looks like a sub-type
// of something already catalogued.
func DetectSubTypes(merges []Merge, leaves []taxLeaf, dist [][]float64, median float64) []SubType {
	out := []SubType{}
	for i, l := range leaves {
		if !l.isCandidate {
			continue
		}
		for _, m := range merges {
			if !mergeJoinsKP(m, i, leaves) {
				continue
			}
			if m.Distance >= 0.3*median {
				break
			}
			nearest := ""
			nearestD := 0.0
			for j, o := range leaves {
				if o.isCandidate || j == i {
					continue
				}
				if nearest == "" || dist[i][j] < nearestD || dist[i][j] == nearestD && o.id < nearest {
					nearest, nearestD = o.id, dist[i][j]
				}
			}
			out = append(out, SubType{
				Candidate:     l.id,
				NearestKP:     nearest,
				MergeDistance: m.Distance,
				Recommendation: fmt.Sprintf(
					"review whether %s is a sub-type of %s; if so, catalogue it under %s in known-problems.yaml with its own match predicates",
					l.id, nearest, nearest),
			})
			break
		}
	}
	return out
}

// DetectOrphans finds CANDIDATEs whose nearest merge distance exceeds the
// 90th percentile of all merge distances: nothing in the taxonomy looks like
// them.
func DetectOrphans(merges []Merge, leaves []taxLeaf, p90 float64) []Orphan {
	out := []Orphan{}
	for i, l := range leaves {
		if !l.isCandidate {
			continue
		}
		d := firstMergeDistance(merges, i)
		if d > p90 {
			out = append(out, Orphan{
				Candidate:             l.id,
				NearestBranchDistance: d,
				Recommendation: fmt.Sprintf(
					"%s merges with nothing below the p90 distance; treat it as a genuinely new failure family and investigate before cataloguing",
					l.id),
			})
		}
	}
	return out
}

// DetectSplits finds KP entries whose attributed outcomes decompose into two
// or more sub-groups when cut at the taxonomy's median merge distance: one
// registry entry may be covering two causes.
func DetectSplits(kpItems map[string][]Item, median float64) []SplitCandidate {
	ids := make([]string, 0, len(kpItems))
	for id := range kpItems {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := []SplitCandidate{}
	for _, id := range ids {
		items := kpItems[id]
		if len(items) < 2 {
			continue
		}
		feats := make([]VerdictFeatures, len(items))
		for i, it := range items {
			feats[i] = it.Features
		}
		merges := Ward(DistanceMatrix(feats))
		groups := CutAtDistance(merges, len(items), median)
		if len(groups) < 2 {
			continue
		}
		internal := 0.0
		for _, m := range merges {
			if m.Distance > internal {
				internal = m.Distance
			}
		}
		out = append(out, SplitCandidate{
			Entry:            id,
			SubClusters:      len(groups),
			InternalDistance: internal,
			Recommendation: fmt.Sprintf(
				"%s's attributed outcomes split into %d sub-groups above the median merge distance; consider splitting it into %d registry entries with tighter match predicates",
				id, len(groups), len(groups)),
		})
	}
	return out
}
