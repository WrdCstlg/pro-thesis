package diagnose

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// Scan must expose the rich per-world and per-bundle facts that the
// clustering layer extracts features from, with the SAME attribution verdicts
// Diagnose computes: identical inputs, identical KP ids.
func TestScanExposesRichFacts(t *testing.T) {
	runsDir := t.TempDir()
	b := mkBundle(t, runsDir, "r_2026_09_21_scan")
	writeFile(t, filepath.Join(b, "verdict.json"),
		`{"schema":"prothesis.verdict/v1","profile":"linear","verdict":"INCONCLUSIVE"}`)
	writeFile(t, filepath.Join(b, "search.json"),
		`{"worlds":[{"ordinal":1,"duration_ms":1234.5},{"ordinal":2,"duration_ms":77}]}`)
	writeFile(t, filepath.Join(b, "world-0001", "result.json"),
		`{"world":1,"seed":7,"outcome":"inconclusive","driver":{"exit_code":3},`+
			`"oracles":[{"oracle":"resource_return_to_baseline","status":"inconclusive","explanation":"metric rss_bytes: no pre-DRIVE baseline"}]}`)
	writeFile(t, filepath.Join(b, "world-0001", "telemetry.json"), `{}`)
	writeFile(t, filepath.Join(b, "world-0002", "result.json"),
		`{"world":2,"seed":8,"outcome":"inconclusive",`+
			`"oracles":[{"oracle":"no_crash","status":"violation","explanation":"a refusal shape nobody catalogued"}]}`)

	bundles, err := Scan(runsDir, loadRegistry(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(bundles) != 1 {
		t.Fatalf("expected one bundle, got %d", len(bundles))
	}
	bf := bundles[0]
	if bf.Name != "r_2026_09_21_scan" {
		t.Fatalf("bundle name: %q", bf.Name)
	}
	if bf.Verdict != "INCONCLUSIVE" || bf.Profile != "linear" {
		t.Fatalf("bundle verdict/profile: %q %q", bf.Verdict, bf.Profile)
	}
	if bf.KP != "KP-004" || bf.Unattributed {
		t.Fatalf("bundle should attribute to KP-004, got KP=%q unattributed=%v", bf.KP, bf.Unattributed)
	}
	if len(bf.Worlds) != 2 {
		t.Fatalf("expected two worlds, got %d", len(bf.Worlds))
	}

	w1 := bf.Worlds[0]
	if w1.Name != "world-0001" || w1.Outcome != "inconclusive" || !w1.HasResult {
		t.Fatalf("world-0001 basics: %+v", w1)
	}
	if w1.KP != "KP-001" || w1.Unattributed {
		t.Fatalf("world-0001 should attribute to KP-001, got KP=%q unattributed=%v", w1.KP, w1.Unattributed)
	}
	if w1.ExitCode != 3 {
		t.Fatalf("world-0001 exit code: %d, want 3", w1.ExitCode)
	}
	if !w1.HasTelemetry {
		t.Fatal("world-0001 telemetry presence not detected")
	}
	if w1.DurationMs != 1234.5 {
		t.Fatalf("world-0001 duration: %v, want 1234.5", w1.DurationMs)
	}
	if len(w1.Oracles) != 1 || w1.Oracles[0].Oracle != "resource_return_to_baseline" {
		t.Fatalf("world-0001 oracles: %+v", w1.Oracles)
	}

	w2 := bf.Worlds[1]
	if !w2.Unattributed || w2.KP != "" {
		t.Fatalf("world-0002 should be unattributed, got KP=%q unattributed=%v", w2.KP, w2.Unattributed)
	}
	if w2.HasTelemetry {
		t.Fatal("world-0002 has no telemetry.json and must report HasBaseline=false")
	}
	if w2.DurationMs != 77 {
		t.Fatalf("world-0002 duration: %v, want 77", w2.DurationMs)
	}
}

// A terminal world and a terminal verdict carry no attribution and are not
// unattributed either: they are simply out of the diagnosis pool.
func TestScanMarksTerminalOutcomesAsNeither(t *testing.T) {
	runsDir := t.TempDir()
	b := mkBundle(t, runsDir, "r_2026_09_21_term")
	writeFile(t, filepath.Join(b, "verdict.json"), `{"schema":"prothesis.verdict/v1","verdict":"PASS"}`)
	writeFile(t, filepath.Join(b, "world-0001", "result.json"), `{"world":1,"seed":7,"outcome":"pass"}`)
	writeFile(t, filepath.Join(b, "world-0002", "result.json"), `{"world":2,"seed":8,"outcome":"violation"}`)

	bundles, err := Scan(runsDir, loadRegistry(t))
	if err != nil {
		t.Fatal(err)
	}
	bf := bundles[0]
	if bf.KP != "" || bf.Unattributed {
		t.Fatalf("terminal verdict must be neither attributed nor unattributed: %+v", bf)
	}
	for _, w := range bf.Worlds {
		if w.KP != "" || w.Unattributed {
			t.Fatalf("terminal world must be neither attributed nor unattributed: %+v", w)
		}
	}
}

// Scan and Diagnose walk the same corpus; their attribution verdicts must
// agree exactly on the recorded corpus.
func TestScanAgreesWithDiagnoseOnTheRealCorpus(t *testing.T) {
	runsDir := filepath.Join("..", "..", "testdata", "kvfixture", ".prothesis", "runs")
	if st, err := os.Stat(runsDir); err != nil || !st.IsDir() {
		t.Skip("no recorded corpus on this host (fresh clone or CI)")
	}
	data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "kvfixture", ".prothesis", "known-problems.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	reg, err := schema.DecodeKnownProblems(data)
	if err != nil {
		t.Fatal(err)
	}
	rep, err := Diagnose(runsDir, reg)
	if err != nil {
		t.Fatal(err)
	}
	bundles, err := Scan(runsDir, reg)
	if err != nil {
		t.Fatal(err)
	}

	scanCounts := map[string]int{}
	scanUnattributed := 0
	for _, b := range bundles {
		for _, w := range b.Worlds {
			if w.Unattributed {
				scanUnattributed++
			}
			if w.KP != "" {
				scanCounts[w.KP]++
			}
		}
		if b.Unattributed {
			scanUnattributed++
		}
		if b.KP != "" {
			scanCounts[b.KP]++
		}
	}
	if scanUnattributed != len(rep.Unattributed) {
		t.Fatalf("unattributed count: Scan=%d Diagnose=%d", scanUnattributed, len(rep.Unattributed))
	}
	for _, c := range rep.Counts {
		if scanCounts[c.ID] != c.N {
			t.Fatalf("%s: Scan=%d Diagnose=%d", c.ID, scanCounts[c.ID], c.N)
		}
	}
	t.Logf("Scan agrees with Diagnose: %d bundles, %d unattributed", len(bundles), scanUnattributed)
}
