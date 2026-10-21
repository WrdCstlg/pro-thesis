package cluster

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/WrdCstlg/pro-thesis/internal/diagnose"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// An unexplained false rejection joins the pool `thesis cluster` reads, as its
// own verdict type, so clustering can separate it from unjudged outcomes.
func TestAnUnexplainedFalseRejectionJoinsThePool(t *testing.T) {
	runs := t.TempDir()
	write := func(rel, s string) {
		t.Helper()
		p := filepath.Join(runs, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("r_2026_10_21_0201/verdict.json", `{"verdict":"FAIL","arm":"GP-01","expected":"PASS","profile":"linear"}`)
	write("r_2026_10_21_0201/world-0001/result.json",
		`{"world":1,"outcome":"violation","oracles":[{"oracle":"no_crash","status":"violated","explanation":"kv-n2 exited 137"}]}`)
	write("r_2026_10_21_0202/verdict.json", `{"verdict":"FAIL","arm":"GP-01","expected":"PASS","profile":"linear"}`)

	bundles, err := diagnose.Scan(runs, &schema.KnownProblems{Schema: schema.KnownProblemsSchema})
	if err != nil {
		t.Fatal(err)
	}
	items := UnattributedItems(bundles)
	if len(items) != 2 {
		t.Fatalf("pool %+v, want the violating world and the run with none", items)
	}
	for _, it := range items {
		if it.Features.VerdictType != diagnose.ClassFalseRejection {
			t.Errorf("%s: verdict_type %q, want %q", it.ID, it.Features.VerdictType, diagnose.ClassFalseRejection)
		}
	}
	if items[0].Features.Component != "no_crash" {
		t.Errorf("component %q, want the oracle that fired", items[0].Features.Component)
	}
}
