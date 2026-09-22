package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/WrdCstlg/pro-thesis/internal/cluster"
	"github.com/WrdCstlg/pro-thesis/internal/diagnose"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
	"gopkg.in/yaml.v3"
)

const clusterUsage = `thesis cluster - discover candidate failure categories in the unattributed pool

usage:
  thesis cluster extract     walk the corpus, write cluster_features.csv and cluster_manifest.json
  thesis cluster discover    DBSCAN + HDBSCAN + consensus, write cluster_report.yaml
  thesis cluster taxonomy    Ward taxonomy of candidates and KP entries, write taxonomy_report.yaml

The clustering layer is a lens, not a gate: it reads .prothesis/runs and
known-problems.yaml, writes reports under .prothesis/, and never promotes a
candidate, mutates a verdict, or touches the recorded corpus.

Exit codes: 0 PASS - 2 INCONCLUSIVE (discover: CONTESTED outcomes exist) -
5 CONFIG_ERROR (usage, missing config/registry, missing or unreadable inputs)
`

func cmdCluster(ctx context.Context, g globals, args []string) schema.ExitCode {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, clusterUsage)
		return schema.ExitConfigError
	}
	sub := args[0]
	rest := args[1:]
	switch sub {
	case "-h", "--help", "help":
		fmt.Print(clusterUsage)
		return schema.ExitPass
	case "extract":
		return cmdClusterExtract(ctx, g, rest)
	case "discover":
		return cmdClusterDiscover(ctx, g, rest)
	case "taxonomy":
		return cmdClusterTaxonomy(ctx, g, rest)
	default:
		errorf("cluster: unknown subcommand %q\n\n%s", sub, clusterUsage)
		return schema.ExitConfigError
	}
}

// clusterProject loads the config, the project directory and the
// known-problems registry: everything the three verbs share.
func clusterProject(g globals) (projectDir string, reg *schema.KnownProblems, code schema.ExitCode) {
	_, projectDir, err := loadConfig(g)
	if err != nil {
		errorf("%v", err)
		return "", nil, schema.ExitConfigError
	}
	regPath := filepath.Join(projectDir, ".prothesis", "known-problems.yaml")
	regData, err := os.ReadFile(regPath)
	if err != nil {
		if os.IsNotExist(err) {
			errorf("no known-problem registry at %s", regPath)
		} else {
			errorf("%v", err)
		}
		return "", nil, schema.ExitConfigError
	}
	reg, err = schema.DecodeKnownProblems(regData)
	if err != nil {
		errorf("%s: %v", regPath, err)
		return "", nil, schema.ExitConfigError
	}
	return projectDir, reg, schema.ExitPass
}

// clusterScan walks the corpus read-only. A missing runs dir is a config
// error here: clustering over nothing recorded would produce a report that
// looks measured and is not.
func clusterScan(projectDir string, reg *schema.KnownProblems) ([]diagnose.BundleFact, schema.ExitCode) {
	runsDir := filepath.Join(projectDir, ".prothesis", "runs")
	if st, err := os.Stat(runsDir); err != nil || !st.IsDir() {
		errorf("no run corpus at %s; nothing recorded to cluster", runsDir)
		return nil, schema.ExitConfigError
	}
	bundles, err := diagnose.Scan(runsDir, reg)
	if err != nil {
		errorf("%v", err)
		return nil, schema.ExitConfigError
	}
	return bundles, schema.ExitPass
}

func cmdClusterExtract(_ context.Context, g globals, args []string) schema.ExitCode {
	if len(args) > 0 {
		errorf("cluster extract: unexpected argument %q", args[0])
		return schema.ExitConfigError
	}
	projectDir, reg, code := clusterProject(g)
	if reg == nil {
		return code
	}
	bundles, code := clusterScan(projectDir, reg)
	if bundles == nil {
		return code
	}

	items := cluster.UnattributedItems(bundles)
	byKP := cluster.AttributedItems(bundles)
	attributed := 0
	for _, v := range byKP {
		attributed += len(v)
	}

	outDir := filepath.Join(projectDir, ".prothesis")
	csvPath := filepath.Join(outDir, "cluster_features.csv")
	if err := cluster.WriteFeaturesCSV(csvPath, items); err != nil {
		errorf("write %s: %v", csvPath, err)
		return schema.ExitConfigError
	}
	manifest := cluster.ExtractManifest{
		CorpusSize:          attributed + len(items),
		AttributedCount:     attributed,
		UnattributedCount:   len(items),
		ExtractionTimestamp: time.Now().UTC().Format(time.RFC3339),
	}
	manifestPath := filepath.Join(outDir, "cluster_manifest.json")
	if err := cluster.WriteManifest(manifestPath, manifest); err != nil {
		errorf("write %s: %v", manifestPath, err)
		return schema.ExitConfigError
	}

	infof(g, "wrote %s", csvPath)
	infof(g, "wrote %s", manifestPath)
	infof(g, "corpus: %d attributed, %d unattributed outcomes", attributed, len(items))
	return schema.ExitPass
}

// discoverExitCode is the normative rule: any CONTESTED outcome is exit 2.
func discoverExitCode(rep *cluster.ClusterReport) schema.ExitCode {
	if rep.Summary.Contested > 0 {
		return schema.ExitInconclusive
	}
	return schema.ExitPass
}

func cmdClusterDiscover(_ context.Context, g globals, args []string) schema.ExitCode {
	if len(args) > 0 {
		errorf("cluster discover: unexpected argument %q", args[0])
		return schema.ExitConfigError
	}
	projectDir, reg, code := clusterProject(g)
	if reg == nil {
		return code
	}
	outDir := filepath.Join(projectDir, ".prothesis")
	csvPath := filepath.Join(outDir, "cluster_features.csv")
	items, err := cluster.ReadFeaturesCSV(csvPath)
	if err != nil {
		if os.IsNotExist(err) {
			errorf("no %s; run `thesis cluster extract` first", csvPath)
		} else {
			errorf("%v", err)
		}
		return schema.ExitConfigError
	}

	meta := cluster.ClusterMeta{Generated: time.Now().UTC(), CorpusSize: len(items)}
	if m, err := cluster.ReadManifest(filepath.Join(outDir, "cluster_manifest.json")); err == nil {
		meta.CorpusSize = m.CorpusSize
		meta.Attributed = m.AttributedCount
	}

	// The CSV carries no free text; re-scan the corpus for reason strings.
	// Best-effort: a corpus that moved since extract leaves reasons empty and
	// the report stays valid.
	if bundles, scanCode := clusterScan(projectDir, reg); scanCode == schema.ExitPass {
		cluster.EnrichReasons(items, cluster.UnattributedItems(bundles))
	}

	rep := cluster.BuildClusterReport(cluster.DiscoverClusters(items), items, meta)
	enc, err := rep.MarshalYAML()
	if err != nil {
		errorf("%v", err)
		return schema.ExitConfigError
	}
	repPath := filepath.Join(outDir, "cluster_report.yaml")
	if err := os.WriteFile(repPath, enc, 0o644); err != nil {
		errorf("write %s: %v", repPath, err)
		return schema.ExitConfigError
	}
	fmt.Print(string(enc))
	infof(g, "wrote %s", repPath)
	return discoverExitCode(rep)
}

func cmdClusterTaxonomy(_ context.Context, g globals, args []string) schema.ExitCode {
	if len(args) > 0 {
		errorf("cluster taxonomy: unexpected argument %q", args[0])
		return schema.ExitConfigError
	}
	projectDir, reg, code := clusterProject(g)
	if reg == nil {
		return code
	}
	outDir := filepath.Join(projectDir, ".prothesis")

	repPath := filepath.Join(outDir, "cluster_report.yaml")
	repData, err := os.ReadFile(repPath)
	if err != nil {
		if os.IsNotExist(err) {
			errorf("no %s; run `thesis cluster discover` first", repPath)
		} else {
			errorf("%v", err)
		}
		return schema.ExitConfigError
	}
	var crep cluster.ClusterReport
	if err := yaml.Unmarshal(repData, &crep); err != nil {
		errorf("%s: %v", repPath, err)
		return schema.ExitConfigError
	}

	// Candidate centroids come from the discover report's CANDIDATE clusters;
	// member features come from the extract CSV. A member the CSV does not
	// know means the two artifacts are out of sync: regenerate, loudly.
	var cands []cluster.CandidateInput
	needCSV := false
	for _, c := range crep.Clusters {
		if c.Status == "CANDIDATE" {
			needCSV = true
			break
		}
	}
	if needCSV {
		csvPath := filepath.Join(outDir, "cluster_features.csv")
		items, err := cluster.ReadFeaturesCSV(csvPath)
		if err != nil {
			if os.IsNotExist(err) {
				errorf("no %s; run `thesis cluster extract` first", csvPath)
			} else {
				errorf("%v", err)
			}
			return schema.ExitConfigError
		}
		byID := map[string]cluster.Item{}
		for _, it := range items {
			byID[it.ID] = it
		}
		for _, c := range crep.Clusters {
			if c.Status != "CANDIDATE" {
				continue
			}
			cand := cluster.CandidateInput{ID: c.ID}
			for _, id := range c.MemberWorlds {
				it, ok := byID[id]
				if !ok {
					errorf("cluster report member %q is not in %s; re-run extract and discover", id, csvPath)
					return schema.ExitConfigError
				}
				cand.Items = append(cand.Items, it)
			}
			cands = append(cands, cand)
		}
	}

	bundles, code := clusterScan(projectDir, reg)
	if bundles == nil {
		return code
	}
	byKP := cluster.AttributedItems(bundles)

	tax := cluster.BuildTaxonomyReport(cands, byKP, time.Now().UTC())
	enc, err := tax.MarshalYAML()
	if err != nil {
		errorf("%v", err)
		return schema.ExitConfigError
	}
	taxPath := filepath.Join(outDir, "taxonomy_report.yaml")
	if err := os.WriteFile(taxPath, enc, 0o644); err != nil {
		errorf("write %s: %v", taxPath, err)
		return schema.ExitConfigError
	}
	fmt.Print(string(enc))
	infof(g, "wrote %s", taxPath)
	return schema.ExitPass
}
