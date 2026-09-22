package cluster

import (
	"testing"

	"github.com/WrdCstlg/pro-thesis/internal/diagnose"
)

// The feature mapping from scan facts to VerdictFeatures is the contract the
// whole engine stands on; these tests pin every field.
func TestUnattributedItemsWorldMapping(t *testing.T) {
	bundles := []diagnose.BundleFact{{
		Name:    "r_2026_09_21_aaaa",
		Verdict: "INCONCLUSIVE",
		Profile: "linear",
		Worlds: []diagnose.WorldFact{{
			Name:      "world-0003",
			HasResult: true,
			Outcome:   "inconclusive",
			Oracles: []diagnose.OracleFact{
				{Oracle: "no_crash", Status: "ok", Explanation: "fine"},
				{Oracle: "resource_return_to_baseline", Status: "inconclusive", Explanation: "No pre-DRIVE baseline, no baseline", Error: "metric missing"},
			},
			ExitCode:     2,
			HasTelemetry: true,
			DurationMs:   4321.5,
			Unattributed: true,
		}},
	}}
	items := UnattributedItems(bundles)
	if len(items) != 1 {
		t.Fatalf("expected one item, got %d", len(items))
	}
	it := items[0]
	if it.ID != "r_2026_09_21_aaaa/world-0003" || it.Level != "world" {
		t.Fatalf("item identity: %+v", it)
	}
	f := it.Features
	if f.Component != "resource_return_to_baseline" {
		t.Fatalf("component must be the first REFUSING oracle, got %q", f.Component)
	}
	if f.ExitCode != 2 || !f.HasBaseline || f.WorldFixture != "linear" || f.VerdictType != "inconclusive" {
		t.Fatalf("scalar fields: %+v", f)
	}
	if f.DurationMs != 4321.5 {
		t.Fatalf("duration: %v", f.DurationMs)
	}
	// Reasons concatenated: "No pre-DRIVE baseline, no baseline metric missing".
	// Tokens: no pre drive baseline no baseline metric missing -> 8 tokens,
	// 6 unique (no, pre, drive, baseline, metric, missing).
	if f.ReasonTokenCount != 8 || f.ReasonUniqueWords != 6 {
		t.Fatalf("reason stats: tokens=%d unique=%d", f.ReasonTokenCount, f.ReasonUniqueWords)
	}
}

func TestUnattributedItemsBundleMapping(t *testing.T) {
	bundles := []diagnose.BundleFact{{
		Name:         "r_2026_09_21_bbbb",
		Unattributed: true,
		// no verdict.json at all
	}}
	items := UnattributedItems(bundles)
	if len(items) != 1 {
		t.Fatalf("expected one item, got %d", len(items))
	}
	f := items[0].Features
	if f.Component != "bundle" {
		t.Fatalf("bundle-level component must be \"bundle\", got %q", f.Component)
	}
	if f.VerdictType != "NO_VERDICT" {
		t.Fatalf("absent verdict must map to NO_VERDICT, got %q", f.VerdictType)
	}
	if f.WorldFixture != "unknown" {
		t.Fatalf("absent profile must map to unknown, got %q", f.WorldFixture)
	}
	if f.HasBaseline || f.ExitCode != 0 || f.DurationMs != 0 {
		t.Fatalf("bundle-level scalars must be zero values: %+v", f)
	}
}

// Attributed and terminal outcomes are not part of the unattributed pool.
func TestUnattributedPoolExcludesAttributedAndTerminal(t *testing.T) {
	bundles := []diagnose.BundleFact{{
		Name: "r_x",
		KP:   "KP-004", // attributed bundle
		Worlds: []diagnose.WorldFact{
			{Name: "world-0001", Outcome: "pass", HasResult: true},      // terminal
			{Name: "world-0002", Outcome: "inconclusive", KP: "KP-001"}, // attributed
			{Name: "world-0003", Outcome: "inconclusive", Unattributed: true},
		},
	}}
	items := UnattributedItems(bundles)
	if len(items) != 1 || items[0].ID != "r_x/world-0003" {
		t.Fatalf("pool: %+v", items)
	}
}

func TestAttributedItemsGroupByKP(t *testing.T) {
	bundles := []diagnose.BundleFact{{
		Name:    "r_x",
		KP:      "KP-003",
		Verdict: "INCONCLUSIVE",
		Profile: "smoke",
		Worlds: []diagnose.WorldFact{
			{Name: "world-0001", Outcome: "inconclusive", KP: "KP-001", HasResult: true,
				Oracles: []diagnose.OracleFact{{Oracle: "o1", Status: "inconclusive", Explanation: "reason one"}}},
			{Name: "world-0002", Outcome: "inconclusive", KP: "KP-001", HasResult: true,
				Oracles: []diagnose.OracleFact{{Oracle: "o1", Status: "inconclusive", Explanation: "reason two"}}},
			{Name: "world-0003", Outcome: "pass", HasResult: true},
		},
	}}
	byKP := AttributedItems(bundles)
	if len(byKP["KP-001"]) != 2 {
		t.Fatalf("KP-001 items: %v", byKP)
	}
	if len(byKP["KP-003"]) != 1 {
		t.Fatalf("KP-003 (bundle level) items: %v", byKP)
	}
	if _, ok := byKP[""]; ok {
		t.Fatal("terminal outcomes must not appear")
	}
	if byKP["KP-001"][0].Features.WorldFixture != "smoke" {
		t.Fatalf("attributed worlds inherit the run profile: %+v", byKP["KP-001"][0].Features)
	}
}

func TestTokenizeRules(t *testing.T) {
	toks := Tokenize("No pre-DRIVE baseline, again_no!")
	want := []string{"no", "pre", "drive", "baseline", "again", "no"}
	if len(toks) != len(want) {
		t.Fatalf("tokens: %v", toks)
	}
	for i := range want {
		if toks[i] != want[i] {
			t.Fatalf("tokens: got %v, want %v", toks, want)
		}
	}
	if got := Tokenize(""); len(got) != 0 {
		t.Fatalf("empty: %v", got)
	}
}

func TestCentroidRules(t *testing.T) {
	items := []Item{
		{Features: VerdictFeatures{Component: "a", ExitCode: 0, HasBaseline: true, WorldFixture: "x", VerdictType: "p", DurationMs: 10, ReasonTokenCount: 2, ReasonUniqueWords: 2}},
		{Features: VerdictFeatures{Component: "a", ExitCode: 2, HasBaseline: true, WorldFixture: "x", VerdictType: "q", DurationMs: 20, ReasonTokenCount: 4, ReasonUniqueWords: 4}},
		{Features: VerdictFeatures{Component: "b", ExitCode: 4, HasBaseline: false, WorldFixture: "x", VerdictType: "q", DurationMs: 30, ReasonTokenCount: 6, ReasonUniqueWords: 6}},
	}
	c := Centroid(items)
	if c.Component != "a" || c.WorldFixture != "x" || c.VerdictType != "q" {
		t.Fatalf("categorical modes: %+v", c)
	}
	if c.ExitCode != 2 || c.DurationMs != 20 || c.ReasonTokenCount != 4 || c.ReasonUniqueWords != 4 {
		t.Fatalf("numeric means: %+v", c)
	}
	if !c.HasBaseline {
		t.Fatalf("boolean mode: %+v", c)
	}
}
