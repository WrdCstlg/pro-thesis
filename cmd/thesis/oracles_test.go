package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/WrdCstlg/pro-thesis/internal/lock"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

const testConfig = `version: prothesis/v1
name: clitest

harness:
  backend: compose
  file: docker-compose.yaml
  nodes:
    - { id: kv-n1, service: kv, compose_service: kv-n1 }
  health:
    - node: "kv-n1"
      probe: "http://{host}:{port}/healthz"
      timeout: 30s

driver:
  cmd: "./bin/loadgen --history {history_path} --seed {seed} --profile {profile}"
  profiles:
    smoke: { clients: 4, ops: 500, mix: { read: 0.5, write: 0.5 } }

perturber:
  budget: { max_concurrent_faults: 3, max_faults_per_world: 24 }
  allow: [net.partition, proc.pause]
  deny: [io.fill]

oracles:
  dir: .prothesis/oracles
  builtin: [no_crash, no_stuck_op]

profiles:
  smoke: { budget: 90s, worlds: 3, driver_profile: smoke }

search:
  probe_budget_pct: 20
  utility: { violation_weight: 100 }

artifacts: { dir: .prothesis/runs }
`

func cliProject(t *testing.T) globals {
	t.Helper()
	dir := t.TempDir()
	cfg := filepath.Join(dir, "prothesis.yaml")
	if err := os.WriteFile(cfg, []byte(testConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".prothesis", "oracles"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".prothesis", "oracles", "linearizable.yaml"),
		[]byte("name: linearizable.kv\nclass: consistency\nvalid_phases: [ASSERT]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return globals{configPath: cfg, quiet: true}
}

func projectDirOf(g globals) string { return filepath.Dir(g.configPath) }

// TestLockRefusesWithoutReason is the anti-gaming requirement stated as a test:
// a bump with no stated reason is indistinguishable from an agent quietly
// re-baselining a gate it could not pass, so the flag is mandatory.
func TestLockRefusesWithoutReason(t *testing.T) {
	for _, args := range [][]string{
		nil,
		{},
		{"--reason", ""},
		{"--reason="},
		{"--reason", "   "},
	} {
		g := cliProject(t)
		code := cmdOraclesLock(context.Background(), g, args)
		if code != schema.ExitConfigError {
			t.Errorf("args %v: got exit %d, want %d (CONFIG_ERROR)", args, code, schema.ExitConfigError)
		}
		if _, err := os.Stat(lock.Path(projectDirOf(g))); !os.IsNotExist(err) {
			t.Errorf("args %v: a refused bump still wrote %s", args, lock.Path(projectDirOf(g)))
		}
	}
}

func TestLockThenVerifyPasses(t *testing.T) {
	g := cliProject(t)
	if code := cmdOraclesLock(context.Background(), g, []string{"--reason", "initial baseline"}); code != schema.ExitPass {
		t.Fatalf("lock exited %d", code)
	}
	raw, err := os.ReadFile(lock.Path(projectDirOf(g)))
	if err != nil {
		t.Fatal(err)
	}
	// The honest limitation must be IN the artifact, not only in a doc comment.
	// Both halves are asserted, because D-060 changed one without changing the
	// other: the digest still does not cover the executable (and never will,
	// since hashing it would make a rebuild exit 4) but the program is now
	// fingerprinted alongside it. A lock that stated only the first half would
	// understate what it does; only the second would overstate it.
	if !strings.Contains(string(raw), "does not cover the EXECUTABLE") {
		t.Error("the lock file does not state that its DIGEST excludes the executable an oracle names")
	}
	if !strings.Contains(string(raw), "OUTSIDE the digest") {
		t.Error("the lock file does not state that the executable fingerprint is recorded outside " +
			"the digest, which is the whole of what D-060 provides")
	}
	if !strings.Contains(string(raw), "initial baseline") {
		t.Error("the lock file does not record the reason")
	}
	if code := cmdOraclesVerify(context.Background(), g, nil); code != schema.ExitPass {
		t.Fatalf("verify exited %d after a fresh lock", code)
	}
}

// TestVerifyWithoutLockIsNotAPass: `verify` answering "there is no baseline"
// with exit 0 would be a vacuous pass an agent could manufacture by deleting
// one file.
func TestVerifyWithoutLockIsNotAPass(t *testing.T) {
	g := cliProject(t)
	if code := cmdOraclesVerify(context.Background(), g, nil); code != schema.ExitOracleDrift {
		t.Fatalf("verify against an absent lock exited %d, want %d (ORACLE_DRIFT)",
			code, schema.ExitOracleDrift)
	}
}

// TestVerifyExitsFourOnEachWeakening walks the gate-weakening moves the lock
// exists to catch, one at a time, and requires exit 4 for every one.
func TestVerifyExitsFourOnEachWeakening(t *testing.T) {
	type mutation struct {
		name    string
		config  func(string) string
		oracles func(t *testing.T, dir string)
	}
	mutations := []mutation{
		{name: "shortened_profile_budget", config: func(s string) string {
			return strings.Replace(s, "budget: 90s", "budget: 1s", 1)
		}},
		{name: "removed_allow_entry", config: func(s string) string {
			return strings.Replace(s, "allow: [net.partition, proc.pause]", "allow: [net.partition]", 1)
		}},
		{name: "narrowed_perturber_budget", config: func(s string) string {
			return strings.Replace(s, "max_faults_per_world: 24", "max_faults_per_world: 1", 1)
		}},
		{name: "zeroed_search_weight", config: func(s string) string {
			return strings.Replace(s, "violation_weight: 100", "violation_weight: 0", 1)
		}},
		{name: "deleted_builtin_oracle", config: func(s string) string {
			return strings.Replace(s, "builtin: [no_crash, no_stuck_op]", "builtin: [no_crash]", 1)
		}},
		{name: "edited_oracle_definition", oracles: func(t *testing.T, dir string) {
			p := filepath.Join(dir, ".prothesis", "oracles", "linearizable.yaml")
			if err := os.WriteFile(p, []byte("name: linearizable.kv\nclass: liveness\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "deleted_oracle_definition", oracles: func(t *testing.T, dir string) {
			if err := os.Remove(filepath.Join(dir, ".prothesis", "oracles", "linearizable.yaml")); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "deleted_lock_file", oracles: func(t *testing.T, dir string) {
			if err := os.Remove(lock.Path(dir)); err != nil {
				t.Fatal(err)
			}
		}},
	}

	for _, m := range mutations {
		t.Run(m.name, func(t *testing.T) {
			g := cliProject(t)
			dir := projectDirOf(g)
			if code := cmdOraclesLock(context.Background(), g, []string{"--reason", "baseline"}); code != schema.ExitPass {
				t.Fatalf("lock exited %d", code)
			}
			if code := cmdOraclesVerify(context.Background(), g, nil); code != schema.ExitPass {
				t.Fatalf("verify exited %d before the mutation; the test would be vacuous", code)
			}

			if m.config != nil {
				next := m.config(testConfig)
				if next == testConfig {
					t.Fatal("mutation did not change the config")
				}
				if err := os.WriteFile(g.configPath, []byte(next), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if m.oracles != nil {
				m.oracles(t, dir)
			}

			if code := cmdOraclesVerify(context.Background(), g, nil); code != schema.ExitOracleDrift {
				t.Fatalf("verify exited %d after %s, want %d (ORACLE_DRIFT)",
					code, m.name, schema.ExitOracleDrift)
			}
		})
	}
}

func TestOraclesListRunsAndNamesTheBuiltins(t *testing.T) {
	g := cliProject(t)
	if code := cmdOraclesList(context.Background(), g, nil); code != schema.ExitPass {
		t.Fatalf("list exited %d", code)
	}
	rows := builtinRows(mustDecode(t, testConfig))
	if len(rows) != 2 {
		t.Fatalf("expected the 2 configured built-ins, got %d", len(rows))
	}
	for _, r := range rows {
		if r.Class == "" || len(r.ValidPhases) == 0 {
			t.Fatalf("%s is listed with no class or no valid phases: %+v", r.Name, r)
		}
	}
}

func mustDecode(t *testing.T, s string) *schema.Config {
	t.Helper()
	cfg, err := schema.DecodeConfig([]byte(s))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestOraclesDispatchRejectsUnknownSubcommand(t *testing.T) {
	g := cliProject(t)
	if code := cmdOracles(context.Background(), g, []string{"relock"}); code != schema.ExitConfigError {
		t.Fatalf("unknown subcommand exited %d, want %d", code, schema.ExitConfigError)
	}
	if code := cmdOracles(context.Background(), g, nil); code != schema.ExitConfigError {
		t.Fatalf("bare `oracles` exited %d, want %d", code, schema.ExitConfigError)
	}
}

// TestRunIsWiredToTheLock proves the enforcement point is reachable from
// `thesis run` and fires BEFORE anything is booted. The config names a compose
// backend and no daemon is contacted, because the drift check returns first.
func TestRunIsWiredToTheLock(t *testing.T) {
	g := cliProject(t)
	if code := cmdOraclesLock(context.Background(), g, []string{"--reason", "baseline"}); code != schema.ExitPass {
		t.Fatalf("lock exited %d", code)
	}
	weakened := strings.Replace(testConfig, "budget: 90s", "budget: 1s", 1)
	if err := os.WriteFile(g.configPath, []byte(weakened), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := cmdRun(context.Background(), g, []string{"--profile", "smoke"}); code != schema.ExitOracleDrift {
		t.Fatalf("run exited %d on a drifted gate, want %d (ORACLE_DRIFT). "+
			"There is no point booting a cluster for a run whose verdict is already void.",
			code, schema.ExitOracleDrift)
	}
	// And no run bundle was created, because nothing was booted.
	if entries, err := os.ReadDir(filepath.Join(projectDirOf(g), ".prothesis", "runs")); err == nil && len(entries) > 0 {
		t.Fatalf("a drifted run still allocated %d run bundle(s)", len(entries))
	}
}
