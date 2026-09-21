package lock

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// baseConfig is a complete, valid prothesis.yaml covering every locked path.
// Tests mutate one line of it at a time.
const baseConfig = `version: prothesis/v1
name: locktest

harness:
  backend: compose
  file: docker-compose.yaml
  nodes:
    - { id: kv-n1, service: kv, compose_service: kv-n1, role_hint: replica }
  health:
    - node: "kv:*"
      probe: "http://{host}:{port}/healthz"
      timeout: 30s

driver:
  cmd: "./bin/loadgen --history {history_path} --seed {seed} --profile {profile}"
  profiles:
    smoke: { clients: 4, ops: 500, mix: { read: 0.5, write: 0.5 } }

perturber:
  budget: { max_concurrent_faults: 3, max_faults_per_world: 24 }
  allow: [net.partition, net.latency, proc.pause]
  deny: [io.fill]
  constraints:
    - "never partition more than minority of kv"

oracles:
  dir: .prothesis/oracles
  builtin: [no_crash, no_panic_log, no_stuck_op]

profiles:
  smoke: { budget: 90s, worlds: 3, driver_profile: smoke }
  gate: { budget: 10m, worlds: 30, driver_profile: gate }

search:
  strategy: saboteur
  exploration_constant: 1.41
  probe_budget_pct: 20
  max_mcts_depth: 4
  utility:
    violation_weight: 100
    novelty_weight: 10
    fault_penalty: 3
    duration_penalty: 0.1

artifacts: { dir: .prothesis/runs, retain_passing: 3, retain_failing: all }
`

// newProject writes a project directory with the given config and oracle files.
func newProject(t *testing.T, cfg string, oracleFiles map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "prothesis.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	od := filepath.Join(dir, ".prothesis", "oracles")
	if err := os.MkdirAll(od, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range oracleFiles {
		p := filepath.Join(od, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func digestOf(t *testing.T, dir string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "prothesis.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	od, err := OracleDirFromConfig(raw)
	if err != nil {
		t.Fatal(err)
	}
	m, err := Build(dir, raw, od)
	if err != nil {
		t.Fatal(err)
	}
	sum, err := m.Digest()
	if err != nil {
		t.Fatal(err)
	}
	return sum
}

// digestOfConfig digests a config string against an empty oracles directory.
func digestOfConfig(t *testing.T, cfg string) string {
	t.Helper()
	return digestOf(t, newProject(t, cfg, nil))
}

// ---------------------------------------------------------------------------
// stability
// ---------------------------------------------------------------------------

func TestManifestIsStableAcrossRuns(t *testing.T) {
	dir := newProject(t, baseConfig, map[string]string{
		"linearizable.yaml": "name: linearizable.kv\nclass: consistency\n",
		"sub/other.json":    `{"name":"other"}`,
	})
	first := digestOf(t, dir)
	for i := 0; i < 5; i++ {
		if got := digestOf(t, dir); got != first {
			t.Fatalf("digest is not stable: run %d gave %s, run 0 gave %s", i, got, first)
		}
	}
	if !strings.HasPrefix(first, schema.HashPrefix) {
		t.Fatalf("digest %q is missing the %q prefix", first, schema.HashPrefix)
	}
}

// TestManifestGolden pins the digest of a fixed document.
//
// A golden is worth having here beyond stability: if anyone rewires the
// projection to digest the RESOLVED config, or widens CoveredPaths, this fails
// immediately and deliberately rather than silently re-baselining every
// downstream project. Update it only together with a DECISIONS.md entry.
func TestManifestGolden(t *testing.T) {
	// Moved on 2026-09-15 by D-066: CoveredPaths gained harness.health and
	// harness.role_probe, and baseConfig carries a health entry. The previous
	// value was sha256:eab904f2…262e86.
	const golden = "sha256:1ce27e2ae493be47276cc73d6099e68cb4872e163933bcdd6add8f174fd7ef3b"
	got := digestOfConfig(t, baseConfig)
	if got != golden {
		t.Fatalf("manifest digest moved:\n  golden %s\n  got    %s\n"+
			"If this is intended, update the golden AND record why in DECISIONS.md.", golden, got)
	}
}

func TestOraclesDirOrderDoesNotMatter(t *testing.T) {
	a := newProject(t, baseConfig, map[string]string{"b.yaml": "b", "a.yaml": "a"})
	b := newProject(t, baseConfig, map[string]string{"a.yaml": "a", "b.yaml": "b"})
	if digestOf(t, a) != digestOf(t, b) {
		t.Fatal("the oracle file list is not sorted: two identical projects digest differently")
	}
}

// ---------------------------------------------------------------------------
// what MUST move the digest
// ---------------------------------------------------------------------------

func TestChangedOracleFileMovesDigest(t *testing.T) {
	before := newProject(t, baseConfig, map[string]string{"chk.yaml": "name: linearizable.kv\nbudget_ms: 30000\n"})
	after := newProject(t, baseConfig, map[string]string{"chk.yaml": "name: linearizable.kv\nbudget_ms: 1\n"})
	if digestOf(t, before) == digestOf(t, after) {
		t.Fatal("editing an oracle definition did not move the digest")
	}
}

func TestRemovedOracleFileMovesDigest(t *testing.T) {
	before := newProject(t, baseConfig, map[string]string{"a.yaml": "a", "b.yaml": "b"})
	after := newProject(t, baseConfig, map[string]string{"a.yaml": "a"})
	if digestOf(t, before) == digestOf(t, after) {
		t.Fatal("deleting an oracle definition did not move the digest")
	}
}

func TestShortenedProfileBudgetMovesDigest(t *testing.T) {
	shortened := strings.Replace(baseConfig,
		"gate: { budget: 10m, worlds: 30, driver_profile: gate }",
		"gate: { budget: 10s, worlds: 30, driver_profile: gate }", 1)
	if shortened == baseConfig {
		t.Fatal("test fixture did not change")
	}
	if digestOfConfig(t, baseConfig) == digestOfConfig(t, shortened) {
		t.Fatal("shortening a profile budget did not move the digest")
	}
}

func TestReducedProfileWorldsMovesDigest(t *testing.T) {
	fewer := strings.Replace(baseConfig, "worlds: 30", "worlds: 1", 1)
	if fewer == baseConfig {
		t.Fatal("test fixture did not change")
	}
	if digestOfConfig(t, baseConfig) == digestOfConfig(t, fewer) {
		t.Fatal("reducing a profile's world count did not move the digest")
	}
}

// TestSwappedDriverProfileMovesDigest covers the whole-`profiles`-block
// decision: swapping `driver_profile: gate` for `smoke` exchanges a heavy
// workload for a trivial one without touching an oracle or a budget.
func TestSwappedDriverProfileMovesDigest(t *testing.T) {
	swapped := strings.Replace(baseConfig, "driver_profile: gate", "driver_profile: smoke", 1)
	if swapped == baseConfig {
		t.Fatal("test fixture did not change")
	}
	if digestOfConfig(t, baseConfig) == digestOfConfig(t, swapped) {
		t.Fatal("swapping a profile's driver_profile did not move the digest")
	}
}

func TestDeletedProfileMovesDigest(t *testing.T) {
	deleted := strings.Replace(baseConfig,
		"  gate: { budget: 10m, worlds: 30, driver_profile: gate }\n", "", 1)
	if deleted == baseConfig {
		t.Fatal("test fixture did not change")
	}
	if digestOfConfig(t, baseConfig) == digestOfConfig(t, deleted) {
		t.Fatal("deleting a whole profile did not move the digest")
	}
}

func TestRemovedAllowListEntryMovesDigest(t *testing.T) {
	removed := strings.Replace(baseConfig,
		"allow: [net.partition, net.latency, proc.pause]",
		"allow: [net.partition, net.latency]", 1)
	if removed == baseConfig {
		t.Fatal("test fixture did not change")
	}
	if digestOfConfig(t, baseConfig) == digestOfConfig(t, removed) {
		t.Fatal("removing a fault kind from perturber.allow did not move the digest")
	}
}

func TestAddedDenyListEntryMovesDigest(t *testing.T) {
	added := strings.Replace(baseConfig, "deny: [io.fill]", "deny: [io.fill, proc.pause]", 1)
	if added == baseConfig {
		t.Fatal("test fixture did not change")
	}
	if digestOfConfig(t, baseConfig) == digestOfConfig(t, added) {
		t.Fatal("adding a fault kind to perturber.deny did not move the digest")
	}
}

func TestNarrowedPerturberBudgetMovesDigest(t *testing.T) {
	narrowed := strings.Replace(baseConfig, "max_faults_per_world: 24", "max_faults_per_world: 1", 1)
	if narrowed == baseConfig {
		t.Fatal("test fixture did not change")
	}
	if digestOfConfig(t, baseConfig) == digestOfConfig(t, narrowed) {
		t.Fatal("narrowing perturber.budget.max_faults_per_world did not move the digest")
	}
}

// TestChangedSearchWeightMovesDigest is D-028 item 8: every utility weight is
// reachable without touching an oracle, so the search block must be covered.
func TestChangedSearchWeightMovesDigest(t *testing.T) {
	for _, tc := range []struct{ name, from, to string }{
		{"violation_weight", "violation_weight: 100", "violation_weight: 1"},
		{"novelty_weight", "novelty_weight: 10", "novelty_weight: 0"},
		{"fault_penalty", "fault_penalty: 3", "fault_penalty: 99"},
		{"duration_penalty", "duration_penalty: 0.1", "duration_penalty: 0.2"},
		{"probe_budget_pct", "probe_budget_pct: 20", "probe_budget_pct: 99"},
		{"max_mcts_depth", "max_mcts_depth: 4", "max_mcts_depth: 1"},
		{"exploration_constant", "exploration_constant: 1.41", "exploration_constant: 0.0"},
		{"strategy", "strategy: saboteur", "strategy: random"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed := strings.Replace(baseConfig, tc.from, tc.to, 1)
			if changed == baseConfig {
				t.Fatalf("fixture does not contain %q", tc.from)
			}
			if digestOfConfig(t, baseConfig) == digestOfConfig(t, changed) {
				t.Fatalf("changing search.%s did not move the digest", tc.name)
			}
		})
	}
}

func TestRemovedBuiltinOracleMovesDigest(t *testing.T) {
	removed := strings.Replace(baseConfig,
		"builtin: [no_crash, no_panic_log, no_stuck_op]",
		"builtin: [no_crash, no_panic_log]", 1)
	if removed == baseConfig {
		t.Fatal("test fixture did not change")
	}
	if digestOfConfig(t, baseConfig) == digestOfConfig(t, removed) {
		t.Fatal("deleting no_stuck_op from oracles.builtin did not move the digest")
	}
}

func TestRepointedOraclesDirMovesDigest(t *testing.T) {
	moved := strings.Replace(baseConfig, "dir: .prothesis/oracles", "dir: .prothesis/other", 1)
	if moved == baseConfig {
		t.Fatal("test fixture did not change")
	}
	if digestOfConfig(t, baseConfig) == digestOfConfig(t, moved) {
		t.Fatal("repointing oracles.dir did not move the digest")
	}
}

// ---------------------------------------------------------------------------
// what MUST NOT move the digest: the spurious-exit-4 guards
// ---------------------------------------------------------------------------

// TestOmittedBlockDiffersFromExplicitDefaults is THE D-F regression.
//
// A config that OMITS `search:` and `perturber.budget:` must digest
// DIFFERENTLY from the same config that writes them out at exactly the
// compiled-in default values. If those two ever agree, the projection is
// filling defaults in, and a `thesis` release that adjusted any default would
// then move the digest on every downstream project at once, halting all of them
// on exit 4, the one code the agent loop must never auto-resolve.
func TestOmittedBlockDiffersFromExplicitDefaults(t *testing.T) {
	omitted := stripDefaultedKeys(t)
	base := digestOfConfig(t, omitted)

	// One subtest per defaulted key, each writing out THAT KEY ALONE at its
	// compiled-in default. Comparing whole blocks would let a projection that
	// fills in one default still pass; this way filling in any single one
	// fails.
	for _, tc := range []struct{ name, yaml string }{
		{"search.strategy", "search:\n  strategy: " + string(schema.StrategySaboteur) + "\n"},
		{"search.exploration_constant", "search:\n  exploration_constant: 1.41\n"},
		{"search.probe_budget_pct", "search:\n  probe_budget_pct: " + strconv.Itoa(schema.DefaultProbeBudgetPct) + "\n"},
		{"search.max_mcts_depth", "search:\n  max_mcts_depth: " + strconv.Itoa(schema.DefaultMaxMCTSDepth) + "\n"},
		{"search.escalation_ladder", "search:\n  escalation_ladder: " + string(schema.LadderDefault) + "\n"},
		{"search.utility.violation_weight", "search:\n  utility:\n    violation_weight: " + strconv.Itoa(schema.DefaultViolationWeight) + "\n"},
		{"search.utility.novelty_weight", "search:\n  utility:\n    novelty_weight: " + strconv.Itoa(schema.DefaultNoveltyWeight) + "\n"},
		{"search.utility.fault_penalty", "search:\n  utility:\n    fault_penalty: " + strconv.Itoa(schema.DefaultFaultPenalty) + "\n"},
		{"search.observe.sample_interval_ms", "search:\n  observe:\n    sample_interval_ms: " + strconv.Itoa(schema.DefaultSampleIntervalMS) + "\n"},
		{"search.observe.spiral_window", "search:\n  observe:\n    spiral_window: " + strconv.Itoa(schema.DefaultSpiralWindow) + "\n"},
		{"perturber.budget.max_concurrent_faults",
			"perturber:\n  budget: { max_concurrent_faults: " + strconv.Itoa(schema.DefaultMaxConcurrentFaults) + " }\n"},
		{"perturber.budget.max_faults_per_world",
			"perturber:\n  budget: { max_faults_per_world: " + strconv.Itoa(schema.DefaultMaxFaultsPerWorld) + " }\n"},
		{"oracles.dir", "oracles:\n  dir: " + schema.DefaultOraclesDir + "\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The fixture already strips `search:` and `perturber.budget:`;
			// merging two YAML mappings by concatenation would be a duplicate
			// key, so these are appended as separate documents-in-one by
			// writing them at top level. `oracles:` and `perturber:` already
			// exist, so those two cases replace rather than append.
			explicit := appendOrMerge(t, omitted, tc.yaml)
			if digestOfConfig(t, explicit) == base {
				t.Fatalf("a config that OMITS %s digests identically to one that writes it out "+
					"at its compiled-in default. That means the default is being filled in before "+
					"digesting, and a thesis release that adjusted it would move the digest on "+
					"every downstream project at once — exit 4, the one code the agent loop must "+
					"never auto-resolve (D-F, OQ-015.2).", tc.name)
			}
		})
	}
}

// appendOrMerge splices a small YAML fragment into cfg, merging into an
// existing top-level key rather than duplicating it.
func appendOrMerge(t *testing.T, cfg, frag string) string {
	t.Helper()
	nl := strings.IndexByte(frag, '\n')
	if nl < 0 {
		t.Fatalf("fragment has no newline: %q", frag)
	}
	head := frag[:nl+1] // e.g. "perturber:\n"
	body := frag[nl+1:]
	marker := "\n" + head
	if i := strings.Index(cfg, marker); i >= 0 {
		// Insert the body directly under the existing key.
		at := i + len(marker)
		return cfg[:at] + body + cfg[at:]
	}
	if strings.HasPrefix(cfg, head) {
		return head + body + cfg[len(head):]
	}
	return cfg + "\n" + frag
}

// TestCompiledInDefaultsNeverReachThePreimage is the same property stated
// directly: nothing the user did not write appears in the manifest.
func TestCompiledInDefaultsNeverReachThePreimage(t *testing.T) {
	omitted := stripDefaultedKeys(t)

	// Sanity: the schema DOES supply these defaults, so the test is not vacuous.
	cfg, err := schema.DecodeConfig([]byte(omitted))
	if err != nil {
		t.Fatalf("fixture must remain a decodable config: %v", err)
	}
	if cfg.Search.ProbeBudgetPct != schema.DefaultProbeBudgetPct ||
		cfg.Perturber.Budget.MaxFaultsPerWorld != schema.DefaultMaxFaultsPerWorld {
		t.Fatalf("fixture no longer omits the defaulted fields: probe_budget_pct=%d faults=%d",
			cfg.Search.ProbeBudgetPct, cfg.Perturber.Budget.MaxFaultsPerWorld)
	}

	entries, err := ProjectConfig([]byte(omitted))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Path, "search") || strings.HasPrefix(e.Path, "perturber.budget") || e.Path == "oracles.dir" {
			t.Fatalf("manifest carries %s = %q for a key the user never wrote; "+
				"a compiled-in default has reached the digest preimage", e.Path, e.Value)
		}
	}
}

// stripDefaultedKeys removes every key from the fixture whose absence the
// schema would supply a compiled-in default for, so that "omitted" really is
// omitted.
func stripDefaultedKeys(t *testing.T) string {
	t.Helper()
	out := baseConfig
	for _, line := range []string{
		"  budget: { max_concurrent_faults: 3, max_faults_per_world: 24 }\n",
		"  dir: .prothesis/oracles\n",
	} {
		next := strings.Replace(out, line, "", 1)
		if next == out {
			t.Fatalf("fixture no longer contains %q", strings.TrimSpace(line))
		}
		out = next
	}
	i := strings.Index(out, "\nsearch:\n")
	j := strings.Index(out, "\nartifacts:")
	if i < 0 || j < 0 || j < i {
		t.Fatal("fixture no longer contains a search block followed by artifacts")
	}
	return out[:i+1] + out[j+1:]
}

// TestAllowListReorderDoesNotMoveDigest guards failure mode 3. Reordering a set
// is semantically nothing, and firing exit 4 on it would be a spurious lock
// failure. Removal is still caught: see TestRemovedAllowListEntryMovesDigest.
func TestAllowListReorderDoesNotMoveDigest(t *testing.T) {
	reordered := strings.Replace(baseConfig,
		"allow: [net.partition, net.latency, proc.pause]",
		"allow: [proc.pause, net.partition, net.latency]", 1)
	if reordered == baseConfig {
		t.Fatal("test fixture did not change")
	}
	if digestOfConfig(t, baseConfig) != digestOfConfig(t, reordered) {
		t.Fatal("reordering perturber.allow moved the digest; a set reorder is not a gate change")
	}
}

// TestUncoveredEditsDoNotMoveDigest documents the scope honestly. An edit
// outside CoveredPaths must not fire exit 4, including the driver profile
// hole recorded as OQ-026, which is stated rather than hidden.
func TestUncoveredEditsDoNotMoveDigest(t *testing.T) {
	for _, tc := range []struct{ name, from, to string }{
		{"name", "name: locktest", "name: renamed"},
		{"harness_file", "file: docker-compose.yaml", "file: other-compose.yaml"},
		{"artifacts_dir", "dir: .prothesis/runs", "dir: .prothesis/elsewhere"},
		// OQ-026: this SHOULD arguably be covered and is not. The test asserts
		// the behaviour that exists so the hole cannot be forgotten.
		{"driver_ops_OQ026", "ops: 500", "ops: 5"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed := strings.Replace(baseConfig, tc.from, tc.to, 1)
			if changed == baseConfig {
				t.Fatalf("fixture does not contain %q", tc.from)
			}
			if digestOfConfig(t, baseConfig) != digestOfConfig(t, changed) {
				t.Fatalf("editing %s moved the digest, but it is outside CoveredPaths", tc.name)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// the lock file
// ---------------------------------------------------------------------------

func TestWriteRefusesWithoutAReason(t *testing.T) {
	dir := newProject(t, baseConfig, nil)
	m, err := Build(dir, []byte(baseConfig), ".prothesis/oracles")
	if err != nil {
		t.Fatal(err)
	}
	for _, reason := range []string{"", "   ", "\t\n"} {
		if _, err := Write(WriteOptions{ProjectDir: dir, Manifest: m, Reason: reason}); err == nil {
			t.Fatalf("Write accepted an empty reason %q; a bump with no stated reason is "+
				"indistinguishable from re-baselining a gate that could not be passed", reason)
		}
	}
	if _, err := os.Stat(Path(dir)); !os.IsNotExist(err) {
		t.Fatal("a refused lock bump still wrote a lock file")
	}
}

func TestWriteThenGateIsOK(t *testing.T) {
	dir := newProject(t, baseConfig, map[string]string{"chk.yaml": "name: linearizable.kv\n"})
	rep := gate(t, dir)
	if rep.Status != schema.LockAbsent {
		t.Fatalf("a project with no lock should be absent, got %s", rep.Status)
	}
	if rep.EnforceExitCode() != schema.ExitPass {
		t.Fatal("an absent lock must not block a run: a project that has never locked is still runnable")
	}
	if rep.VerifyExitCode() != schema.ExitOracleDrift {
		t.Fatal("`oracles verify` against an absent lock must not exit 0; " +
			"answering `there is no baseline` with a pass is a vacuous pass")
	}

	if _, err := Write(WriteOptions{
		ProjectDir: dir, Manifest: rep.Manifest, Reason: "initial baseline", Now: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	rep2 := gate(t, dir)
	if rep2.Status != schema.LockOK {
		t.Fatalf("after locking, status is %s: %s", rep2.Status, rep2.Diagnosis())
	}
	if rep2.VerifyExitCode() != schema.ExitPass || rep2.EnforceExitCode() != schema.ExitPass {
		t.Fatal("a matching lock must pass both verify and enforce")
	}
	if rep2.OracleLock().Status != schema.LockOK || rep2.OracleLock().ManifestSHA != rep2.ManifestSHA {
		t.Fatalf("verdict oracle_lock is wrong: %+v", rep2.OracleLock())
	}
}

func gate(t *testing.T, dir string) *Report {
	t.Helper()
	rep, err := Gate(GateOptions{ProjectDir: dir})
	if err != nil {
		t.Fatalf("Gate: %v", err)
	}
	return rep
}

// TestVerifyDriftNamesWhatMoved is the message a human is escalated to. "Lock
// mismatch" alone forces a hand diff, so the diagnosis must name the subject
// and both values.
func TestVerifyDriftNamesWhatMoved(t *testing.T) {
	dir := newProject(t, baseConfig, map[string]string{"chk.yaml": "budget_ms: 30000\n"})
	rep := gate(t, dir)
	if _, err := Write(WriteOptions{ProjectDir: dir, Manifest: rep.Manifest, Reason: "baseline"}); err != nil {
		t.Fatal(err)
	}

	// Three weakenings at once: a shortened budget, a dropped fault kind, a
	// zeroed utility weight, and an edited oracle definition.
	weakened := strings.NewReplacer(
		"gate: { budget: 10m, worlds: 30, driver_profile: gate }",
		"gate: { budget: 5s, worlds: 30, driver_profile: gate }",
		"allow: [net.partition, net.latency, proc.pause]",
		"allow: [net.latency]",
		"violation_weight: 100", "violation_weight: 0",
	).Replace(baseConfig)
	if err := os.WriteFile(filepath.Join(dir, "prothesis.yaml"), []byte(weakened), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".prothesis", "oracles", "chk.yaml"),
		[]byte("budget_ms: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	drift := gate(t, dir)
	if drift.Status != schema.LockMismatch {
		t.Fatalf("expected mismatch, got %s", drift.Status)
	}
	if drift.VerifyExitCode() != schema.ExitOracleDrift {
		t.Fatalf("drift must exit 4, got %d", drift.VerifyExitCode())
	}
	if drift.EnforceExitCode() != schema.ExitOracleDrift {
		t.Fatalf("`thesis run` must exit 4 on drift, got %d", drift.EnforceExitCode())
	}

	msg := drift.Diagnosis()
	for _, want := range []string{
		"profiles.gate.budget", "10m", "5s",
		"perturber.allow", "net.partition", "proc.pause",
		"search.utility.violation_weight", "100", "0",
		"chk.yaml",
		drift.LockedSHA, drift.ManifestSHA,
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("diagnosis does not name %q; a bare \"lock mismatch\" forces a hand diff.\n%s", want, msg)
		}
	}
}

// TestCorruptLockIsNotAnAbsentLock: a single corrupting write must not be able
// to clear the drift check.
func TestCorruptLockIsNotAnAbsentLock(t *testing.T) {
	dir := newProject(t, baseConfig, nil)
	if err := os.MkdirAll(filepath.Dir(Path(dir)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(Path(dir), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Gate(GateOptions{ProjectDir: dir}); err == nil {
		t.Fatal("a corrupt lock file was accepted; it must be an error, never an absent lock")
	}
}

// TestHandEditedLockIsMismatch: a lock whose recorded manifest does not hash to
// its recorded digest has been edited by hand.
func TestHandEditedLockIsMismatch(t *testing.T) {
	dir := newProject(t, baseConfig, nil)
	rep := gate(t, dir)
	if _, err := Write(WriteOptions{ProjectDir: dir, Manifest: rep.Manifest, Reason: "baseline"}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(Path(dir))
	if err != nil {
		t.Fatal(err)
	}
	// Pretend an agent pasted in a digest without updating the manifest.
	tampered := strings.Replace(string(raw), rep.ManifestSHA,
		schema.HashPrefix+strings.Repeat("0", 64), 1)
	if tampered == string(raw) {
		t.Fatal("could not tamper with the fixture")
	}
	if err := os.WriteFile(Path(dir), []byte(tampered), 0o644); err != nil {
		t.Fatal(err)
	}
	drift := gate(t, dir)
	if drift.Status != schema.LockMismatch || !drift.InternallyInconsistent {
		t.Fatalf("a hand-edited lock must be a mismatch, got %s (inconsistent=%v)",
			drift.Status, drift.InternallyInconsistent)
	}
}

// TestMissingOracleDirIsNotAnError: a project with no external oracles is
// legitimate. An UNREADABLE directory is a different matter and is covered by
// HashOracleDir returning an error.
func TestMissingOracleDirIsNotAnError(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "prothesis.yaml"), []byte(baseConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	files, err := HashOracleDir(dir, ".prothesis/oracles")
	if err != nil {
		t.Fatalf("a missing oracles dir must not be an error: %v", err)
	}
	if len(files) != 0 {
		t.Fatalf("expected no files, got %d", len(files))
	}
}

// TestOptionsFingerprintIsAWarningNotDrift pins D-035: a compiled-in threshold
// change is recorded and warned about, and is NEVER exit 4.
func TestOptionsFingerprintIsAWarningNotDrift(t *testing.T) {
	dir := newProject(t, baseConfig, nil)
	rep := gate(t, dir)
	if _, err := Write(WriteOptions{
		ProjectDir: dir, Manifest: rep.Manifest, Reason: "baseline",
		BuiltinOptionsFingerprint: "sha256:aaa",
	}); err != nil {
		t.Fatal(err)
	}
	after, err := Gate(GateOptions{ProjectDir: dir, BuiltinOptionsFingerprint: "sha256:bbb"})
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != schema.LockOK {
		t.Fatalf("a moved built-in fingerprint must not be drift, got %s", after.Status)
	}
	if !after.OptionsMoved {
		t.Fatal("a moved built-in fingerprint must be reported")
	}
	if !strings.Contains(after.Diagnosis(), "WARNING (not drift)") {
		t.Fatalf("the moved fingerprint is not surfaced:\n%s", after.Diagnosis())
	}
}

// ---------------------------------------------------------------------------
// projection details
// ---------------------------------------------------------------------------

func TestProjectionDistinguishesAbsentFromEmptyFromNull(t *testing.T) {
	base := "version: prothesis/v1\nname: x\n"
	cases := map[string]string{
		"absent": base,
		"null":   base + "search:\n",
		"empty":  base + "search: {}\n",
		"set":    base + "search:\n  probe_budget_pct: 20\n",
	}
	digests := map[string]string{}
	for name, cfg := range cases {
		entries, err := ProjectConfig([]byte(cfg))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		m := Manifest{Schema: ManifestSchema, Oracles: []OracleFile{}, Config: entries}
		d, err := m.Digest()
		if err != nil {
			t.Fatal(err)
		}
		digests[name] = d
	}
	seen := map[string]string{}
	for name, d := range digests {
		if other, dup := seen[d]; dup {
			t.Fatalf("%q and %q digest identically; absent, null, empty and set must stay distinct",
				name, other)
		}
		seen[d] = name
	}
}

func TestProjectionRejectsNothingItCannotValidate(t *testing.T) {
	// A config that Validate() would reject must still be projectable: the
	// lock's job is to notice change, not to validate.
	if _, err := ProjectConfig([]byte("profiles:\n  gate: { budget: not-a-duration }\n")); err != nil {
		t.Fatalf("projection must tolerate an invalid config: %v", err)
	}
	if _, err := ProjectConfig([]byte("")); err != nil {
		t.Fatalf("projection must tolerate an empty config: %v", err)
	}
	if _, err := ProjectConfig([]byte("::: not yaml :::\n  - [")); err == nil {
		t.Fatal("unparseable YAML must be an error, not an empty projection")
	}
}

func TestOracleDirFromConfigUsesTheWrittenValue(t *testing.T) {
	got, err := OracleDirFromConfig([]byte(baseConfig))
	if err != nil {
		t.Fatal(err)
	}
	if got != ".prothesis/oracles" {
		t.Fatalf("oracles dir = %q", got)
	}
	got, err = OracleDirFromConfig([]byte("version: prothesis/v1\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got != schema.DefaultOraclesDir {
		t.Fatalf("absent oracles.dir should fall back to %q for WALKING, got %q",
			schema.DefaultOraclesDir, got)
	}
}
