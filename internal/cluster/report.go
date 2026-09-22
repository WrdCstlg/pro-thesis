package cluster

import (
	"fmt"
	"sort"
	"time"

	"gopkg.in/yaml.v3"
)

// report.go builds the Layer 1 ClusterReport. The report is ADVISORY: it
// changes nothing it reads, promotes nothing, and its noise section lists
// the outcomes that stay unattributed, verbatim.

// Discovery is the full Layer 1 computation over the unattributed pool.
type Discovery struct {
	Epsilon        float64
	MinPts         int
	MinClusterSize int
	DBLabels       []int
	HDB            HDBSCANResult
	Assignments    []Assignment
}

// ClusterMeta carries the report's metadata counts.
type ClusterMeta struct {
	Generated  time.Time
	CorpusSize int // attributed + unattributed outcomes examined
	Attributed int
}

// DBSCAN and HDBSCAN tuning constants (the spec's values).
const hdbscanMinClusterSize = 3
const hdbscanClusterSelection = "eom"

// DiscoverClusters runs DBSCAN (auto-tuned epsilon, minPts = max(3, 2*dims))
// and HDBSCAN (min_cluster_size = 3, 'eom') over the pool and applies the
// consensus rules. The empty pool is a valid input and clusters into
// nothing.
func DiscoverClusters(items []Item) *Discovery {
	feats := make([]VerdictFeatures, len(items))
	for i, it := range items {
		feats[i] = it.Features
	}
	dist := DistanceMatrix(feats)
	minPts := MinPts(FeatureDimensions)
	d := &Discovery{
		MinPts:         minPts,
		MinClusterSize: hdbscanMinClusterSize,
	}
	if len(items) == 0 {
		d.DBLabels = nil
		d.HDB = HDBSCANResult{}
		return d
	}
	d.Epsilon = AutoEpsilon(dist, minPts)
	d.DBLabels = DBSCAN(dist, d.Epsilon, minPts)
	d.HDB = HDBSCAN(dist, hdbscanMinClusterSize)
	d.Assignments = ConsensusAssign(d.DBLabels, d.HDB)
	return d
}

// ClusterReport is the prothesis.clustering/v1 document.
type ClusterReport struct {
	APIVersion string          `yaml:"apiVersion"`
	Kind       string          `yaml:"kind"`
	Metadata   ClusterMetadata `yaml:"metadata"`
	Clusters   []ClusterEntry  `yaml:"clusters"`
	Noise      NoiseSection    `yaml:"noise"`
	Summary    ClusterSummary  `yaml:"summary"`
}

type ClusterMetadata struct {
	Generated         string        `yaml:"generated"`
	CorpusSize        int           `yaml:"corpus_size"`
	Attributed        int           `yaml:"attributed"`
	UnattributedInput int           `yaml:"unattributed_input"`
	DBSCANParams      DBSCANParams  `yaml:"dbscan_params"`
	HDBSCANParams     HDBSCANParams `yaml:"hdbscan_params"`
}

type DBSCANParams struct {
	Epsilon float64 `yaml:"epsilon"`
	MinPts  int     `yaml:"min_pts"`
}

type HDBSCANParams struct {
	MinClusterSize         int    `yaml:"min_cluster_size"`
	ClusterSelectionMethod string `yaml:"cluster_selection_method"`
}

type ClusterEntry struct {
	ID                    string   `yaml:"id"`
	Status                string   `yaml:"status"` // CANDIDATE | CONTESTED
	Size                  int      `yaml:"size"`
	Consensus             string   `yaml:"consensus"` // agree | disagree
	Stability             float64  `yaml:"stability"`
	CentroidDescription   string   `yaml:"centroid_description"`
	RepresentativeReasons []string `yaml:"representative_reasons"`
	MemberWorlds          []string `yaml:"member_worlds"`
}

type NoiseSection struct {
	Count  int          `yaml:"count"`
	Worlds []NoiseWorld `yaml:"worlds"`
}

type NoiseWorld struct {
	ID     string `yaml:"id"`
	Reason string `yaml:"reason"`
}

type ClusterSummary struct {
	Candidates int `yaml:"candidates"`
	Contested  int `yaml:"contested"`
	Noise      int `yaml:"noise"`
}

// MarshalYAML renders the report as YAML bytes.
func (r *ClusterReport) MarshalYAML() ([]byte, error) {
	return yaml.Marshal(r)
}

const maxReasonLen = 160
const maxRepresentativeReasons = 3

func truncate(s string) string {
	if len(s) <= maxReasonLen {
		return s
	}
	return s[:maxReasonLen-3] + "..."
}

// describeCentroid renders a centroid as one stable description line.
func describeCentroid(c VerdictFeatures) string {
	return fmt.Sprintf("component=%s exit_code=%d baseline=%t fixture=%s verdict=%s duration_ms=%.1f tokens=%d unique=%d",
		c.Component, c.ExitCode, c.HasBaseline, c.WorldFixture, c.VerdictType,
		c.DurationMs, c.ReasonTokenCount, c.ReasonUniqueWords)
}

// BuildClusterReport renders a Discovery as a ClusterReport. Clusters are
// ordered by (-size, smallest member id) and take CAND-NNN ids in that
// order; noise points are listed, never reassigned.
func BuildClusterReport(d *Discovery, items []Item, meta ClusterMeta) *ClusterReport {
	rep := &ClusterReport{
		APIVersion: "prothesis.clustering/v1",
		Kind:       "ClusterReport",
		Metadata: ClusterMetadata{
			Generated:         meta.Generated.UTC().Format(time.RFC3339),
			CorpusSize:        meta.CorpusSize,
			Attributed:        meta.Attributed,
			UnattributedInput: len(items),
			DBSCANParams:      DBSCANParams{Epsilon: d.Epsilon, MinPts: d.MinPts},
			HDBSCANParams: HDBSCANParams{
				MinClusterSize:         d.MinClusterSize,
				ClusterSelectionMethod: hdbscanClusterSelection,
			},
		},
		Clusters: []ClusterEntry{},
		Noise:    NoiseSection{Worlds: []NoiseWorld{}},
	}

	// Group non-noise points by consensus cluster key.
	groups := map[string][]int{}
	for i, a := range d.Assignments {
		if a.Status == StatusNoise {
			reason := "no recorded reason"
			if len(items[i].Reasons) > 0 && items[i].Reasons[0] != "" {
				reason = truncate(items[i].Reasons[0])
			}
			rep.Noise.Worlds = append(rep.Noise.Worlds, NoiseWorld{ID: items[i].ID, Reason: reason})
			continue
		}
		groups[a.Cluster] = append(groups[a.Cluster], i)
	}
	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		gi, gj := groups[keys[i]], groups[keys[j]]
		if len(gi) != len(gj) {
			return len(gi) > len(gj)
		}
		return items[gi[0]].ID < items[gj[0]].ID
	})

	for ci, k := range keys {
		members := groups[k]
		entry := ClusterEntry{
			ID:                    fmt.Sprintf("CAND-%03d", ci+1),
			Status:                "CANDIDATE",
			Consensus:             "agree",
			RepresentativeReasons: []string{},
			MemberWorlds:          []string{},
		}
		var memberItems []Item
		probSum := 0.0
		reasons := map[string]bool{}
		for _, mi := range members {
			a := d.Assignments[mi]
			if a.Status == StatusContested {
				entry.Status = "CONTESTED"
			}
			if !a.Agree {
				entry.Consensus = "disagree"
			}
			probSum += d.HDB.Probabilities[mi]
			memberItems = append(memberItems, items[mi])
			entry.MemberWorlds = append(entry.MemberWorlds, items[mi].ID)
			for _, r := range items[mi].Reasons {
				if r != "" {
					reasons[truncate(r)] = true
				}
			}
		}
		sort.Strings(entry.MemberWorlds)
		entry.Size = len(members)
		if len(members) > 0 {
			entry.Stability = probSum / float64(len(members))
		}
		entry.CentroidDescription = describeCentroid(Centroid(memberItems))
		uniq := make([]string, 0, len(reasons))
		for r := range reasons {
			uniq = append(uniq, r)
		}
		sort.Strings(uniq)
		if len(uniq) > maxRepresentativeReasons {
			uniq = uniq[:maxRepresentativeReasons]
		}
		entry.RepresentativeReasons = uniq
		rep.Clusters = append(rep.Clusters, entry)
		if entry.Status == "CONTESTED" {
			rep.Summary.Contested++
		} else {
			rep.Summary.Candidates++
		}
	}

	sort.Slice(rep.Noise.Worlds, func(i, j int) bool { return rep.Noise.Worlds[i].ID < rep.Noise.Worlds[j].ID })
	rep.Noise.Count = len(rep.Noise.Worlds)
	rep.Summary.Noise = rep.Noise.Count
	return rep
}

// reasonByID indexes item reasons for the discover verb's enrichment pass.
func reasonByID(items []Item) map[string][]string {
	out := make(map[string][]string, len(items))
	for _, it := range items {
		out[it.ID] = it.Reasons
	}
	return out
}

// EnrichReasons copies reason text from a fresh corpus scan into items read
// back from a CSV (which carries no free text). Items with no scan match
// keep empty reasons; the report stays valid without them.
func EnrichReasons(items []Item, scanned []Item) {
	byID := reasonByID(scanned)
	for i := range items {
		if r, ok := byID[items[i].ID]; ok {
			items[i].Reasons = r
		}
	}
}
