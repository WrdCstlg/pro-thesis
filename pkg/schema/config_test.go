package schema

import (
	"os"
	"strings"
	"testing"
	"time"
)

func readTestdata(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("read testdata/%s: %v", name, err)
	}
	return b
}

func decodeGoldenConfig(t *testing.T) *Config {
	t.Helper()
	cfg, err := DecodeConfig(readTestdata(t, "prothesis.golden.yaml"))
	if err != nil {
		t.Fatalf("DecodeConfig(directive 4.2 sample): %v", err)
	}
	return cfg
}

// TestGoldenConfigDecodes pins every field name in directive 4.2 against a
// verbatim copy of the sample. It is the cheapest detector for a renamed or
// retyped configuration key.
func TestGoldenConfigDecodes(t *testing.T) {
	cfg := decodeGoldenConfig(t)

	if cfg.Version != ConfigVersion {
		t.Errorf("version = %q, want %q", cfg.Version, ConfigVersion)
	}
	if cfg.Name != "my-service" {
		t.Errorf("name = %q, want %q", cfg.Name, "my-service")
	}

	// harness
	if cfg.Harness.Backend != BackendCompose {
		t.Errorf("harness.backend = %q, want compose", cfg.Harness.Backend)
	}
	if cfg.Harness.File != "docker-compose.yaml" {
		t.Errorf("harness.file = %q", cfg.Harness.File)
	}
	if len(cfg.Harness.Nodes) != 4 {
		t.Fatalf("harness.nodes has %d entries, want 4", len(cfg.Harness.Nodes))
	}
	if got := cfg.Harness.Nodes[0]; got.ID != "n1" || got.Service != "kv" || got.RoleHint != "replica" {
		t.Errorf("harness.nodes[0] = %+v", got)
	}
	if got := cfg.Harness.Nodes[3]; got.ID != "pg" || got.Service != "postgres" || got.RoleHint != "storage" {
		t.Errorf("harness.nodes[3] = %+v", got)
	}
	if len(cfg.Harness.Health) != 1 {
		t.Fatalf("harness.health has %d entries, want 1", len(cfg.Harness.Health))
	}
	h := cfg.Harness.Health[0]
	if h.Node != "kv:*" || h.Probe != "http://{host}:{port}/healthz" {
		t.Errorf("harness.health[0] = %+v", h)
	}
	if h.Timeout != Duration(30*time.Second) {
		t.Errorf("harness.health[0].timeout = %s, want 30s", h.Timeout)
	}
	if cfg.Harness.SteadyState.Probe != "thesis-helpers/steady.sh" {
		t.Errorf("harness.steady_state.probe = %q", cfg.Harness.SteadyState.Probe)
	}
	if cfg.Harness.SteadyState.Timeout != Duration(60*time.Second) {
		t.Errorf("harness.steady_state.timeout = %s, want 60s", cfg.Harness.SteadyState.Timeout)
	}

	// driver
	wantCmd := "./bin/loadgen --history {history_path} --seed {seed} --profile {profile}"
	if cfg.Driver.Cmd != wantCmd {
		t.Errorf("driver.cmd = %q", cfg.Driver.Cmd)
	}
	if len(cfg.Driver.Profiles) != 3 {
		t.Fatalf("driver.profiles has %d entries, want 3", len(cfg.Driver.Profiles))
	}
	gate, ok := cfg.DriverProfileByName("gate")
	if !ok {
		t.Fatal("driver.profiles.gate missing")
	}
	if gate.Clients != 16 || gate.Ops != 20000 {
		t.Errorf("driver.profiles.gate = %+v", gate)
	}
	for k, want := range map[string]float64{"read": 0.4, "write": 0.4, "txn": 0.2} {
		if gate.Mix[k] != want {
			t.Errorf("driver.profiles.gate.mix[%s] = %v, want %v", k, gate.Mix[k], want)
		}
	}
	soak, _ := cfg.DriverProfileByName("soak")
	if soak.Clients != 64 || soak.Ops != 500000 || soak.Mix["admin"] != 0.1 {
		t.Errorf("driver.profiles.soak = %+v", soak)
	}

	// perturber
	if cfg.Perturber.Budget.MaxConcurrentFaults != 3 || cfg.Perturber.Budget.MaxFaultsPerWorld != 24 {
		t.Errorf("perturber.budget = %+v", cfg.Perturber.Budget)
	}
	wantAllow := []FaultKind{
		FaultNetPartition, FaultNetLatency, FaultNetLoss,
		FaultProcKill, FaultProcPause, FaultClockSkew, FaultIOLatency,
	}
	if len(cfg.Perturber.Allow) != len(wantAllow) {
		t.Fatalf("perturber.allow has %d entries, want %d", len(cfg.Perturber.Allow), len(wantAllow))
	}
	for i, k := range wantAllow {
		if cfg.Perturber.Allow[i] != k {
			t.Errorf("perturber.allow[%d] = %q, want %q", i, cfg.Perturber.Allow[i], k)
		}
	}
	if len(cfg.Perturber.Deny) != 1 || cfg.Perturber.Deny[0] != FaultIOFill {
		t.Errorf("perturber.deny = %v, want [io.fill]", cfg.Perturber.Deny)
	}
	if len(cfg.Perturber.Constraints) != 2 ||
		cfg.Perturber.Constraints[0] != "never partition more than minority of kv" {
		t.Errorf("perturber.constraints = %q", cfg.Perturber.Constraints)
	}
	// io.fill is denied even though it is not in allow; io.latency is allowed.
	for _, k := range cfg.Perturber.EffectiveFaultKinds() {
		if k == FaultIOFill {
			t.Error("EffectiveFaultKinds includes io.fill, which the sample denies")
		}
	}

	// oracles
	if cfg.Oracles.Dir != ".prothesis/oracles" {
		t.Errorf("oracles.dir = %q", cfg.Oracles.Dir)
	}
	if len(cfg.Oracles.Builtin) != 6 {
		t.Fatalf("oracles.builtin has %d entries, want 6", len(cfg.Oracles.Builtin))
	}
	for i, want := range AllBuiltinOracles {
		if cfg.Oracles.Builtin[i] != want {
			t.Errorf("oracles.builtin[%d] = %q, want %q", i, cfg.Oracles.Builtin[i], want)
		}
	}

	// profiles: the awkward cases
	smoke, ok := cfg.Profile("smoke")
	if !ok {
		t.Fatal("profiles.smoke missing")
	}
	if smoke.Budget != Duration(90*time.Second) {
		t.Errorf("profiles.smoke.budget = %s, want 90s", smoke.Budget)
	}
	if smoke.Worlds == nil || *smoke.Worlds != 3 {
		t.Errorf("profiles.smoke.worlds = %v, want 3", smoke.Worlds)
	}
	if smoke.Search {
		t.Error("profiles.smoke.search is true; the sample does not set it")
	}
	gateP, _ := cfg.Profile("gate")
	if gateP.Budget != Duration(10*time.Minute) {
		t.Errorf("profiles.gate.budget = %s, want 10m", gateP.Budget)
	}
	soakP, _ := cfg.Profile("soak")
	if soakP.Budget != Duration(8*time.Hour) {
		t.Errorf("profiles.soak.budget = %s, want 8h", soakP.Budget)
	}
	if soakP.Worlds == nil || *soakP.Worlds != WorldsUnbounded {
		t.Errorf("profiles.soak.worlds = %v, want -1", soakP.Worlds)
	}
	if !soakP.Search {
		t.Error("profiles.soak.search = false, want true (the profile-level BOOLEAN)")
	}

	// artifacts: the int-or-string union, both forms, in one file
	if cfg.Artifacts.Dir != ".prothesis/runs" {
		t.Errorf("artifacts.dir = %q", cfg.Artifacts.Dir)
	}
	if cfg.Artifacts.RetainPassing != RetainN(3) {
		t.Errorf("artifacts.retain_passing = %+v, want RetainN(3)", cfg.Artifacts.RetainPassing)
	}
	if cfg.Artifacts.RetainFailing != RetainAll() {
		t.Errorf("artifacts.retain_failing = %+v, want RetainAll()", cfg.Artifacts.RetainFailing)
	}

	// the top-level search block is absent from 4.2, so it must be defaulted
	if cfg.Search != DefaultSearchConfig() {
		t.Errorf("search = %+v, want the A.10 defaults %+v", cfg.Search, DefaultSearchConfig())
	}

	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate on the directive's own sample config: %v", err)
	}
}

// TestRetainPolicyAcceptsBothForms is the direct regression test for the
// broken "probe with a string first" technique: yaml.v3 decodes the integer
// scalar 3 into a Go string without error, so a string-first probe would accept
// "3" and then reject it as not being "all".
func TestRetainPolicyAcceptsBothForms(t *testing.T) {
	cases := []struct {
		yaml string
		want RetainPolicy
	}{
		{"retain_passing: 3\nretain_failing: all\n", RetainN(3)},
		{"retain_passing: 0\nretain_failing: all\n", RetainN(0)},
		{"retain_passing: all\nretain_failing: 10\n", RetainAll()},
		{"retain_passing: -1\nretain_failing: all\n", RetainAll()},
	}
	for _, c := range cases {
		cfg, err := DecodeConfig([]byte("artifacts:\n  " + strings.ReplaceAll(c.yaml, "\n", "\n  ")))
		if err != nil {
			t.Fatalf("decode %q: %v", c.yaml, err)
		}
		if cfg.Artifacts.RetainPassing != c.want {
			t.Errorf("%q: retain_passing = %+v, want %+v", c.yaml, cfg.Artifacts.RetainPassing, c.want)
		}
	}

	if _, err := DecodeConfig([]byte("artifacts:\n  retain_passing: everything\n")); err == nil {
		t.Error("retain_passing: everything was accepted; only an integer or \"all\" is a policy")
	}
	if _, err := DecodeConfig([]byte("artifacts:\n  retain_passing: true\n")); err == nil {
		t.Error("retain_passing: true was accepted")
	}
}

// TestDurationRejectsBareNumber proves the diagnostic the design promised is
// actually reachable. A string-first probe would have swallowed `budget: 90`
// as the string "90" and produced a ParseDuration error instead.
func TestDurationRejectsBareNumber(t *testing.T) {
	_, err := DecodeConfig([]byte("profiles:\n  smoke: { budget: 90, driver_profile: smoke }\n"))
	if err == nil {
		t.Fatal("budget: 90 was accepted; a bare number has no defensible unit")
	}
	if !strings.Contains(err.Error(), "\"90s\"") {
		t.Errorf("error does not name the fix: %v", err)
	}

	cfg, err := DecodeConfig([]byte("profiles:\n  smoke: { budget: 90s, driver_profile: smoke }\n"))
	if err != nil {
		t.Fatalf("budget: 90s rejected: %v", err)
	}
	if cfg.Profiles["smoke"].Budget != Duration(90*time.Second) {
		t.Errorf("budget = %s", cfg.Profiles["smoke"].Budget)
	}
}

// TestMapSectionsAreNotPreSeeded is the D-H trap.
//
// If DefaultConfig pre-seeded the profile maps, this document would decode with
// three run profiles instead of one (map KEYS merge), and the surviving soak
// entry would silently keep compiled-in values the user deleted (map ENTRIES do
// not merge). Both would make a profile deletion invisible to the Phase 3 lock.
func TestMapSectionsAreNotPreSeeded(t *testing.T) {
	const doc = `
version: prothesis/v1
name: trimmed
profiles:
  soak: { budget: 8h, worlds: 5, driver_profile: soak }
driver:
  cmd: "./bin/loadgen"
  profiles:
    soak: { clients: 1, ops: 1 }
`
	cfg, err := DecodeConfig([]byte(doc))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(cfg.Profiles) != 1 {
		t.Fatalf("profiles = %v, want exactly [soak]: a deleted profile must stay deleted",
			cfg.ProfileNames())
	}
	if _, ok := cfg.Profile("smoke"); ok {
		t.Error("profiles.smoke was resurrected from compiled-in defaults")
	}
	if _, ok := cfg.Profile("gate"); ok {
		t.Error("profiles.gate was resurrected from compiled-in defaults")
	}
	soak := cfg.Profiles["soak"]
	if soak.Worlds == nil || *soak.Worlds != 5 {
		t.Errorf("profiles.soak.worlds = %v, want 5", soak.Worlds)
	}
	if soak.Budget != Duration(8*time.Hour) {
		t.Errorf("profiles.soak.budget = %s, want 8h", soak.Budget)
	}
	if len(cfg.Driver.Profiles) != 1 {
		t.Errorf("driver.profiles = %v, want exactly [soak]", sortedKeys(cfg.Driver.Profiles))
	}
	if cfg.Driver.Profiles["soak"].Mix != nil {
		t.Errorf("driver.profiles.soak.mix = %v, want nil: mixes are never pre-seeded",
			cfg.Driver.Profiles["soak"].Mix)
	}
	// Sequences are not pre-seeded either: an absent oracles.builtin must not
	// resurrect the full built-in list, or deleting one would not move the lock.
	if len(cfg.Oracles.Builtin) != 0 {
		t.Errorf("oracles.builtin = %v, want empty when the key is absent", cfg.Oracles.Builtin)
	}
	// Scalar and struct defaults, by contrast, MUST survive decode-in-place.
	if cfg.Harness.Backend != BackendCompose {
		t.Errorf("harness.backend = %q, want the compose default to survive", cfg.Harness.Backend)
	}
	if cfg.Harness.SteadyState.Timeout != Duration(DefaultSteadyTimeout) {
		t.Errorf("harness.steady_state.timeout = %s, want the 60s default to survive",
			cfg.Harness.SteadyState.Timeout)
	}
	if cfg.Search.ExplorationConstant != DefaultExplorationConstant {
		t.Errorf("search.exploration_constant = %v, want %v",
			cfg.Search.ExplorationConstant, DefaultExplorationConstant)
	}
}

// TestUnknownFieldRejected keeps the fail-closed posture on struct fields, and
// documents honestly that it does NOT extend to map keys.
func TestUnknownFieldRejected(t *testing.T) {
	if _, err := DecodeConfig([]byte("version: prothesis/v1\nnaem: typo\n")); err == nil {
		t.Error("a misspelled top-level key was accepted")
	}
	if _, err := DecodeConfig([]byte("harness:\n  backendd: compose\n")); err == nil {
		t.Error("a misspelled harness key was accepted")
	}
	if _, err := DecodeConfig([]byte("profiles:\n  gate: { budget: 1m, drivr_profile: gate }\n")); err == nil {
		t.Error("a misspelled key INSIDE a profile was accepted")
	}
	// KnownFields cannot protect map KEYS. This is recorded as a test so the
	// limitation is visible rather than assumed away.
	cfg, err := DecodeConfig([]byte("profiles:\n  smoek: { budget: 1m, driver_profile: gate }\n"))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := cfg.Profile("smoek"); !ok {
		t.Fatal("expected the typo'd profile name to decode as a phantom profile")
	}
}

// TestProfileWorldsDistinguishesAbsentFromZero pins the pointer.
func TestProfileWorldsDistinguishesAbsentFromZero(t *testing.T) {
	cfg, err := DecodeConfig([]byte("profiles:\n  a: { budget: 1m, driver_profile: x }\n"))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if cfg.Profiles["a"].Worlds != nil {
		t.Errorf("absent worlds decoded as %v, want nil (inherit)", cfg.Profiles["a"].Worlds)
	}

	cfg, err = DecodeConfig([]byte("profiles:\n  a: { budget: 1m, driver_profile: x, worlds: 0 }\n"))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if cfg.Profiles["a"].Worlds == nil || *cfg.Profiles["a"].Worlds != 0 {
		t.Fatalf("worlds: 0 decoded as %v, want an explicit 0", cfg.Profiles["a"].Worlds)
	}
	if err := cfg.ValidateProfiles(); err == nil {
		t.Error("worlds: 0 passed validation; it runs no worlds and is better reported than guessed")
	}
}

// TestSearchBlockTwoLevels covers OQ-005: a top-level `search:` MAPPING and a
// profile-level `search:` BOOLEAN in the same document.
func TestSearchBlockTwoLevels(t *testing.T) {
	const doc = `
version: prothesis/v1
name: two-level
profiles:
  soak: { budget: 8h, worlds: -1, driver_profile: soak, search: true }
search:
  strategy: hybrid
  exploration_constant: 2.0
  probe_budget_pct: 35
  max_mcts_depth: 6
  escalation_ladder: custom
  utility:
    violation_weight: 120
    novelty_weight: 8
    fault_penalty: 5
    duration_penalty: 0.25
  observe:
    sample_interval_ms: 200
    spiral_window: 7
    reinforce_threshold: 0.5
`
	cfg, err := DecodeConfig([]byte(doc))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !cfg.Profiles["soak"].Search {
		t.Error("profiles.soak.search (boolean) = false, want true")
	}
	want := SearchConfig{
		Strategy:            StrategyHybrid,
		ExplorationConstant: 2.0,
		ProbeBudgetPct:      35,
		MaxMCTSDepth:        6,
		EscalationLadder:    LadderCustom,
		Utility:             UtilityConfig{ViolationWeight: 120, NoveltyWeight: 8, FaultPenalty: 5, DurationPenalty: 0.25},
		Observe:             ObserveConfig{SampleIntervalMS: 200, SpiralWindow: 7, ReinforceThreshold: 0.5},
		// The llm block is defaulted even when another strategy is selected;
		// its values are inert until strategy: llm reads them.
		LLM: DefaultLLMConfig(),
	}
	if cfg.Search != want {
		t.Errorf("search = %+v, want %+v", cfg.Search, want)
	}
	if err := cfg.ValidateSearch(); err != nil {
		t.Errorf("ValidateSearch: %v", err)
	}

	// A partial block keeps the untouched defaults (struct decode-in-place).
	cfg, err = DecodeConfig([]byte("search:\n  probe_budget_pct: 10\n"))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if cfg.Search.ProbeBudgetPct != 10 {
		t.Errorf("probe_budget_pct = %d", cfg.Search.ProbeBudgetPct)
	}
	if cfg.Search.Strategy != StrategySaboteur || cfg.Search.Utility.NoveltyWeight != DefaultNoveltyWeight {
		t.Errorf("partial search block clobbered defaults: %+v", cfg.Search)
	}
	if _, err := DecodeConfig([]byte("search:\n  strategy: mcts\n")); err == nil {
		t.Error("an unknown search strategy was accepted")
	}
}

// TestValidateForTopologyIgnoresLaterPhases keeps `thesis up` reachable in
// Phase 0: it must not require a driver, a perturber budget or a run profile.
func TestValidateForTopologyIgnoresLaterPhases(t *testing.T) {
	const doc = `
version: prothesis/v1
name: phase0
harness:
  backend: compose
  file: docker-compose.yaml
  nodes:
    - { id: n1, service: kv }
`
	cfg, err := DecodeConfig([]byte(doc))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if err := cfg.ValidateForTopology(); err != nil {
		t.Errorf("ValidateForTopology on a harness-only config: %v", err)
	}
	if err := cfg.Validate(); err == nil {
		t.Error("full Validate accepted a config with no driver and no profiles")
	}
}

// TestConfigValidationCatchesRealMistakes proves the validators can fail.
func TestConfigValidationCatchesRealMistakes(t *testing.T) {
	cfg := decodeGoldenConfig(t)
	cfg.Profiles["gate"] = Profile{
		Budget: cfg.Profiles["gate"].Budget, DriverProfile: "nonexistent",
	}
	if err := cfg.Validate(); err == nil {
		t.Error("a profile naming a driver profile that does not exist was accepted")
	}

	cfg = decodeGoldenConfig(t)
	cfg.Harness.Nodes[1].ID = "n1"
	if err := cfg.ValidateHarness(); err == nil {
		t.Error("duplicate node ids were accepted")
	}

	cfg = decodeGoldenConfig(t)
	cfg.Harness.Health[0].Node = "redis:*"
	if err := cfg.Validate(); err == nil {
		t.Error("a health probe targeting a service with no nodes was accepted")
	}
}

// TestMixSumIsNotValidated: nothing in the directive declares the mix a
// probability distribution, and relative weights are a common load-generator
// convention. Rejecting them would be an invented CONFIG_ERROR.
func TestMixSumIsNotValidated(t *testing.T) {
	cfg, err := DecodeConfig([]byte(
		"driver:\n  cmd: x\n  profiles:\n    w: { clients: 4, ops: 10, mix: { read: 5, write: 5 } }\n"))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if err := cfg.ValidateDriver(); err != nil {
		t.Errorf("relative mix weights rejected: %v", err)
	}
}

// TestSearchStrategyLLMDecodes pins `search.strategy: llm` and its block.
func TestSearchStrategyLLMDecodes(t *testing.T) {
	const doc = `
version: prothesis/v1
name: llm-search
search:
  strategy: llm
  llm:
    endpoint: http://127.0.0.1:9/v1/chat/completions
    model: qwen3.8
    api_key_env: MY_LLM_KEY
    max_retries: 3
    timeout_ms: 30000
`
	cfg, err := DecodeConfig([]byte(doc))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if cfg.Search.Strategy != StrategyLLM {
		t.Errorf("search.strategy = %q, want %q", cfg.Search.Strategy, StrategyLLM)
	}
	want := LLMConfig{
		Endpoint:   "http://127.0.0.1:9/v1/chat/completions",
		Model:      "qwen3.8",
		APIKeyEnv:  "MY_LLM_KEY",
		MaxRetries: 3,
		TimeoutMS:  30000,
	}
	if cfg.Search.LLM != want {
		t.Errorf("search.llm = %+v, want %+v", cfg.Search.LLM, want)
	}
	if err := cfg.ValidateSearch(); err != nil {
		t.Errorf("ValidateSearch: %v", err)
	}
}

// TestLLMDefaultsSurviveDecode: a `strategy: llm` with no `llm:` block runs
// against the local default endpoint, and a partial block keeps the rest.
func TestLLMDefaultsSurviveDecode(t *testing.T) {
	cfg, err := DecodeConfig([]byte("search:\n  strategy: llm\n"))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if cfg.Search.LLM != DefaultLLMConfig() {
		t.Errorf("search.llm = %+v, want the defaults %+v", cfg.Search.LLM, DefaultLLMConfig())
	}
	if err := cfg.ValidateSearch(); err != nil {
		t.Errorf("ValidateSearch on defaults: %v", err)
	}

	cfg, err = DecodeConfig([]byte("search:\n  strategy: llm\n  llm:\n    max_retries: 5\n"))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if cfg.Search.LLM.MaxRetries != 5 || cfg.Search.LLM.Endpoint != DefaultLLMEndpoint {
		t.Errorf("partial llm block clobbered defaults: %+v", cfg.Search.LLM)
	}
}

// TestLLMBlockIsValidatedWhenStrategyIsLLM pins the refusal surface: a config
// that names llm but cannot reach a model is a CONFIG_ERROR, not a search
// that fails thirty seconds in.
func TestLLMBlockIsValidatedWhenStrategyIsLLM(t *testing.T) {
	for _, tc := range []struct {
		doc  string
		want string
	}{
		{"search:\n  strategy: llm\n  llm:\n    endpoint: \"\"\n", "search.llm.endpoint"},
		{"search:\n  strategy: llm\n  llm:\n    max_retries: -1\n", "search.llm.max_retries"},
		{"search:\n  strategy: llm\n  llm:\n    timeout_ms: 10\n", "search.llm.timeout_ms"},
	} {
		cfg, err := DecodeConfig([]byte(tc.doc))
		if err != nil {
			t.Fatalf("decode %q: %v", tc.doc, err)
		}
		err = cfg.ValidateSearch()
		if err == nil {
			t.Errorf("%q passed validation, want a refusal naming %s", tc.doc, tc.want)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%q: error %v does not name %s", tc.doc, err, tc.want)
		}
	}
	// The llm block is not validated when another strategy is selected: its
	// defaults are inert, and refusing them would gate a saboteur run on a
	// service it never calls.
	cfg, err := DecodeConfig([]byte("search:\n  strategy: saboteur\n  llm:\n    timeout_ms: 10\n"))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if err := cfg.ValidateSearch(); err != nil {
		t.Errorf("ValidateSearch rejected an llm block no selected strategy reads: %v", err)
	}
}

// TestAMisspelledLLMKeyIsRejected keeps KnownFields' fail-closed posture on
// the new block.
func TestAMisspelledLLMKeyIsRejected(t *testing.T) {
	if _, err := DecodeConfig([]byte("search:\n  llm:\n    endpont: http://x\n")); err == nil {
		t.Error("a misspelled search.llm key was accepted")
	}
}
