// Package cluster is the two-layer unsupervised clustering engine for the
// unattributed pool that `thesis diagnose` reports. Layer 1 (discover) runs
// DBSCAN and HDBSCAN over Tier 1 structural features of every unattributed
// outcome and applies a per-outcome consensus rule; Layer 2 (taxonomy) runs
// Ward hierarchical clustering over the centroids of the Layer 1 CANDIDATE
// clusters and the existing known-problem entries.
//
// The engine is a lens, not a gate: it never writes to known-problems.yaml,
// never promotes a candidate, never mutates a verdict, and never touches the
// recorded corpus (`.prothesis/runs` is read-only through diagnose.Scan).
// Noise is sacred: an outcome neither algorithm clusters stays unattributed,
// exactly as diagnose reported it. Output is deterministic given the corpus;
// the only varying byte in any artifact is the `generated` timestamp.
//
// FEATURE MAPPING (spec field -> recorded corpus source)
//
//	Component         name of the first refusing oracle (status != "ok") in the
//	                  world's result.json oracles list; "bundle" for bundle-level
//	                  items; "unknown" when no oracle refused or none were recorded
//	ExitCode          driver.exit_code from result.json (0 when absent)
//	HasBaseline       telemetry.json present in the world dir (the baseline is
//	                  computed from pre-DRIVE telemetry; no telemetry, no baseline)
//	WorldFixture      the run's profile from verdict.json ("unknown" when absent)
//	VerdictType       the world outcome from result.json; for bundle-level items
//	                  the verdict string, or "NO_VERDICT" when no verdict.json exists
//	DurationMs        the world's duration_ms from search.json (search runs only);
//	                  0 when unrecorded, which is deterministic and documented here
//	ReasonTokenCount  tokens over the concatenated explanations+errors of the
//	ReasonUniqueWords REFUSING oracles (status != "ok"); unique tokens over the
//	                  same text (bundle-level items: over the bundle detail)
//
// corpus_size in the extract manifest is defined as attributed_count +
// unattributed_count: the number of non-terminal outcomes examined, which is
// the population diagnose judges.
package cluster

import (
	"fmt"
	"sort"
	"strings"

	"github.com/WrdCstlg/pro-thesis/internal/diagnose"
)

// VerdictFeatures is the Tier 1 structural feature vector of one outcome.
type VerdictFeatures struct {
	Component         string
	ExitCode          int
	HasBaseline       bool
	WorldFixture      string
	VerdictType       string
	DurationMs        float64
	ReasonTokenCount  int
	ReasonUniqueWords int
}

// FeatureDimensions is the number of features in VerdictFeatures; DBSCAN's
// minPts rule (max(3, 2*dims)) keys off it.
const FeatureDimensions = 8

// Item is one outcome with its extracted features. ID is "bundle/world-NNNN"
// for world-level outcomes and "bundle" for bundle-level ones.
type Item struct {
	ID       string
	Bundle   string
	Level    string // world | bundle
	KP       string // attribution id, "" for unattributed items
	Reasons  []string
	Features VerdictFeatures
}

// Tokenize splits on non-alphanumeric bytes and lowercases; underscores are
// separators. Deterministic by construction.
func Tokenize(s string) []string {
	var out []string
	start := -1
	for i := 0; i < len(s); i++ {
		c := s[i]
		alnum := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
		if alnum && start < 0 {
			start = i
		} else if !alnum && start >= 0 {
			out = append(out, strings.ToLower(s[start:i]))
			start = -1
		}
	}
	if start >= 0 {
		out = append(out, strings.ToLower(s[start:]))
	}
	return out
}

// reasonStats counts tokens and unique words over the concatenated reasons.
func reasonStats(reasons []string) (tokens, unique int) {
	seen := map[string]bool{}
	for _, r := range reasons {
		for _, tok := range Tokenize(r) {
			tokens++
			seen[tok] = true
		}
	}
	return tokens, len(seen)
}

// worldItem extracts one world-level outcome's features. The reason text is
// taken from the REFUSING oracles (status != "ok"): the feature is about why
// the world could not be judged, and green oracles' explanations would only
// dilute it. A world with no refusing oracle has empty reasons.
func worldItem(bundle string, w diagnose.WorldFact, profile string) Item {
	reasons := make([]string, 0, len(w.Oracles))
	component := "unknown"
	for _, o := range w.Oracles {
		if o.Status == "" || o.Status == "ok" {
			continue
		}
		if component == "unknown" {
			component = o.Oracle
		}
		if r := strings.TrimSpace(o.Explanation + " " + o.Error); r != "" {
			reasons = append(reasons, r)
		}
	}
	fixture := profile
	if fixture == "" {
		fixture = "unknown"
	}
	verdictType := w.Outcome
	if verdictType == "" {
		verdictType = "NO_RESULT"
	}
	tokens, unique := reasonStats(reasons)
	return Item{
		ID: bundle + "/" + w.Name, Bundle: bundle, Level: "world",
		Reasons: reasons,
		Features: VerdictFeatures{
			Component:         component,
			ExitCode:          w.ExitCode,
			HasBaseline:       w.HasTelemetry,
			WorldFixture:      fixture,
			VerdictType:       verdictType,
			DurationMs:        w.DurationMs,
			ReasonTokenCount:  tokens,
			ReasonUniqueWords: unique,
		},
	}
}

// bundleDetail renders a bundle-level outcome as one detail string; it is
// the bundle item's reason text.
func bundleDetail(b diagnose.BundleFact) string {
	verdict := b.Verdict
	if verdict == "" {
		verdict = "NO_VERDICT"
	}
	return fmt.Sprintf("verdict=%s", verdict)
}

// bundleItem extracts one bundle-level outcome's features.
func bundleItem(b diagnose.BundleFact) Item {
	verdictType := b.Verdict
	if verdictType == "" {
		verdictType = "NO_VERDICT"
	}
	fixture := b.Profile
	if fixture == "" {
		fixture = "unknown"
	}
	detail := bundleDetail(b)
	tokens, unique := reasonStats([]string{detail})
	return Item{
		ID: b.Name, Bundle: b.Name, Level: "bundle",
		Reasons: []string{detail},
		Features: VerdictFeatures{
			Component:         "bundle",
			WorldFixture:      fixture,
			VerdictType:       verdictType,
			ReasonTokenCount:  tokens,
			ReasonUniqueWords: unique,
		},
	}
}

// UnattributedItems is the Layer 1 input pool: every outcome diagnose
// reported unattributed, in deterministic scan order.
func UnattributedItems(bundles []diagnose.BundleFact) []Item {
	var out []Item
	for _, b := range bundles {
		for _, w := range b.Worlds {
			if w.Unattributed {
				out = append(out, worldItem(b.Name, w, b.Profile))
			}
		}
		if b.Unattributed {
			out = append(out, bundleItem(b))
		}
	}
	return out
}

// AttributedItems groups every non-terminal outcome that attributed to a
// registry entry by KP id, for the Layer 2 KP centroids.
func AttributedItems(bundles []diagnose.BundleFact) map[string][]Item {
	out := map[string][]Item{}
	for _, b := range bundles {
		for _, w := range b.Worlds {
			if w.KP == "" {
				continue
			}
			it := worldItem(b.Name, w, b.Profile)
			it.KP = w.KP
			out[w.KP] = append(out[w.KP], it)
		}
		if b.KP != "" {
			it := bundleItem(b)
			it.KP = b.KP
			out[b.KP] = append(out[b.KP], it)
		}
	}
	return out
}

// Centroid averages a group's feature vectors: numeric fields take the mean
// (integer fields rounded half away from zero), categorical fields take the
// mode with ties broken by the lexicographically smallest value, so the
// result is a total-order deterministic reduction.
func Centroid(items []Item) VerdictFeatures {
	if len(items) == 0 {
		return VerdictFeatures{}
	}
	var c VerdictFeatures
	var exitSum, durSum, tokSum, uniqSum float64
	comp, fixture, verdict := map[string]int{}, map[string]int{}, map[string]int{}
	baselines := 0
	for _, it := range items {
		f := it.Features
		exitSum += float64(f.ExitCode)
		durSum += f.DurationMs
		tokSum += float64(f.ReasonTokenCount)
		uniqSum += float64(f.ReasonUniqueWords)
		comp[f.Component]++
		fixture[f.WorldFixture]++
		verdict[f.VerdictType]++
		if f.HasBaseline {
			baselines++
		}
	}
	n := float64(len(items))
	c.ExitCode = roundHalfAway(exitSum / n)
	c.DurationMs = durSum / n
	c.ReasonTokenCount = roundHalfAway(tokSum / n)
	c.ReasonUniqueWords = roundHalfAway(uniqSum / n)
	c.Component = mode(comp)
	c.WorldFixture = mode(fixture)
	c.VerdictType = mode(verdict)
	c.HasBaseline = baselines*2 > len(items) // strict majority; ties go false
	return c
}

func roundHalfAway(f float64) int {
	if f < 0 {
		return -roundHalfAway(-f)
	}
	return int(f + 0.5)
}

// mode picks the most frequent key, ties broken by smallest key.
func mode(counts map[string]int) string {
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	best, bestN := "", -1
	for _, k := range keys {
		if counts[k] > bestN {
			best, bestN = k, counts[k]
		}
	}
	return best
}
