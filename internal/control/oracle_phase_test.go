package control

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// A world's oracles are evaluated once, at ASSERT, over the whole recorded
// history. An external oracle whose valid_phases leaves ASSERT out can therefore
// never run. It used to pass validation, be dropped by selection, appear in no
// record, and leave the world free to PASS. It is now refused before anything
// boots, and a world that somehow reaches ASSERT with one still cannot pass.

func writePhaseDefinition(t *testing.T, dir, name, phases string) {
	t.Helper()
	body := "version: prothesis.oracle_def/v1\n" +
		"name: " + name + "\n" +
		"class: consistency\n" +
		"valid_phases: [" + phases + "]\n" +
		"cmd: ./bin/" + name + "\n" +
		"timeout: 30s\n"
	if err := os.WriteFile(filepath.Join(dir, name+".yaml"), []byte(body), 0o644); err != nil {
		t.Fatalf("write definition: %v", err)
	}
}

func phaseTestConfig(t *testing.T, phases string) *schema.Config {
	t.Helper()
	cfg := perturbTestConfig(t)
	dir := t.TempDir()
	writePhaseDefinition(t, dir, "heal.check", phases)
	cfg.Oracles = schema.OraclesConfig{Dir: dir, Builtin: []schema.BuiltinOracle{schema.BuiltinNoCrash}}
	return cfg
}

func newPhaseTestRunner(cfg *schema.Config, projectDir string) (*Runner, error) {
	return NewRunner(RunnerOptions{
		Config:        cfg,
		ProjectDir:    projectDir,
		Backend:       &stubBackend{},
		Driver:        finishedDriver(),
		Telemetry:     stubTelemetry{},
		LogCollector:  stubLogs{},
		ImageResolver: stubImages{},
		Quiet:         true,
		// OracleEngine left nil: the production engine is the one under test.
	})
}

func TestNewRunnerRefusesAnOracleThatCanNeverBeEvaluated(t *testing.T) {
	cfg := phaseTestConfig(t, "HEAL, QUIESCE")
	_, err := newPhaseTestRunner(cfg, t.TempDir())
	if err == nil {
		t.Fatal("NewRunner accepted an oracle that declares only HEAL and QUIESCE; it would never be " +
			"evaluated and the run could pass without it")
	}
	for _, want := range []string{"heal.check", "ASSERT"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the refusal does not name %q: %v", want, err)
		}
	}
	// Refused before anything was allocated: no run directory left behind.
	entries, rerr := os.ReadDir(cfg.Artifacts.Dir)
	if rerr != nil {
		t.Fatalf("read artifacts dir: %v", rerr)
	}
	if len(entries) != 0 {
		t.Fatalf("a refused configuration left %d entries in the runs directory", len(entries))
	}

	// The same set with ASSERT declared is accepted, so the refusal above is
	// about the phase set and nothing else in this configuration.
	if _, err := newPhaseTestRunner(phaseTestConfig(t, "HEAL, ASSERT"), t.TempDir()); err != nil {
		t.Fatalf("control: an oracle that declares ASSERT was refused: %v", err)
	}
}

// Defence in depth: the production engine, handed such a set anyway, returns an
// inconclusive result for the oracle it could not evaluate instead of omitting
// it, so the world folds to INCONCLUSIVE.
func TestTheDefaultEngineFailsClosedOnAnOracleItCannotEvaluate(t *testing.T) {
	cfg := phaseTestConfig(t, "HEAL")
	world := t.TempDir()
	results, err := (&defaultOracleEngine{}).Evaluate(context.Background(), EvalRequest{
		Config:     cfg,
		ProjectDir: t.TempDir(),
		RunID:      "r_2026_10_21_0001",
		Paths:      NewWorldPaths(world),
	})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	var found *OracleResult
	for i := range results {
		if results[i].Output.Oracle == "heal.check" {
			found = &results[i]
		}
	}
	if found == nil {
		t.Fatalf("the HEAL-only oracle produced no result at all (%d results): it vanished", len(results))
	}
	if got := found.Outcome(); got != OutcomeInconclusive {
		t.Fatalf("the unevaluated oracle folds to %q, want %q", got, OutcomeInconclusive)
	}
}
