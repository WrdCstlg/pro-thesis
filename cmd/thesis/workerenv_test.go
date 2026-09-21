package main

import (
	"strings"
	"testing"

	"github.com/WrdCstlg/pro-thesis/internal/control"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

func fixtureLikeConfig() *schema.Config {
	return &schema.Config{
		Harness: schema.HarnessConfig{
			PrefixEnv: "KV_PREFIX",
			Nodes: []schema.NodeConfig{
				{ID: "kv-n1", Service: "kv", PortEnv: "KV_PORT_N1"},
				{ID: "kv-n2", Service: "kv", PortEnv: "KV_PORT_N2"},
				{ID: "kv-n3", Service: "kv", PortEnv: "KV_PORT_N3"},
			},
		},
	}
}

func envOf(t *testing.T, fn func(control.WorkerSlot) []string) map[string]string {
	t.Helper()
	if fn == nil {
		return nil
	}
	slot := control.WorkerSlot{
		Index:   0,
		Prefix:  "w00",
		Project: "thesis-test-w00",
		// Nodes is what Targets() iterates; a slot without it exports an empty
		// PROTHESIS_TARGETS, which is its own bug and not the one under test.
		Nodes: []string{"kv-n1", "kv-n2", "kv-n3"},
		Ports: map[string]int{"kv-n1": 19000, "kv-n2": 19001, "kv-n3": 19002},
	}
	out := map[string]string{}
	for _, kv := range fn(slot) {
		if k, v, ok := strings.Cut(kv, "="); ok {
			out[k] = v
		}
	}
	return out
}

// The mapping between a worker's port band and the variables a topology file
// spells is a fact about the PROJECT, not a per-invocation choice. Leaving it
// flag-only meant a `thesis search` that forgot --worker-env drove every world
// against ports nothing was listening on: 78,018 operation records, all 39,009
// ops failed, none checkable (run r_2026_09_09_23c5). See OQ-056.
func TestTheWorkerPortMappingCanComeFromConfigSoABareSearchIsCorrect(t *testing.T) {
	fn, err := parseWorkerEnv("", fixtureLikeConfig())
	if err != nil {
		t.Fatalf("parseWorkerEnv: %v", err)
	}
	if fn == nil {
		t.Fatal("a config declaring prefix_env and port_env produced no worker mapping, so a " +
			"bare `thesis search` still publishes the file's default ports while telling the " +
			"driver about the slot's band")
	}
	env := envOf(t, fn)
	for k, want := range map[string]string{
		"KV_PREFIX":  "w00",
		"KV_PORT_N1": "19000",
		"KV_PORT_N2": "19001",
		"KV_PORT_N3": "19002",
	} {
		if env[k] != want {
			t.Errorf("%s = %q, want %q", k, env[k], want)
		}
	}
	// The generic names must still be exported: the driver reads PROTHESIS_TARGETS
	// and it is not the compose file's business.
	if env[control.TargetsEnv] == "" {
		t.Errorf("%s was not exported, so the driver has no way to find this world's cluster",
			control.TargetsEnv)
	}
}

// An explicit flag is an instruction and must win over config.
func TestAnExplicitWorkerEnvFlagOverridesTheConfig(t *testing.T) {
	fn, err := parseWorkerEnv("prefix=OTHER_PREFIX,kv-n1=OTHER_PORT", fixtureLikeConfig())
	if err != nil {
		t.Fatalf("parseWorkerEnv: %v", err)
	}
	env := envOf(t, fn)
	if env["OTHER_PREFIX"] != "w00" || env["OTHER_PORT"] != "19000" {
		t.Errorf("the flag's names were not used: %v", env)
	}
	if _, ok := env["KV_PORT_N1"]; ok {
		t.Error("the config's mapping was merged into an explicit --worker-env; the flag is an " +
			"instruction and silently adding to it is how a caller stops being able to predict " +
			"what their own command does")
	}
}

// A project whose topology file does not parameterise anything has nothing to
// map, and must not be handed invented variable names.
func TestAConfigThatDeclaresNoMappingYieldsNone(t *testing.T) {
	cfg := &schema.Config{Harness: schema.HarnessConfig{
		Nodes: []schema.NodeConfig{{ID: "kv-n1", Service: "kv"}},
	}}
	fn, err := parseWorkerEnv("", cfg)
	if err != nil {
		t.Fatalf("parseWorkerEnv: %v", err)
	}
	if fn != nil {
		t.Error("a config declaring no prefix_env and no port_env produced a mapping anyway")
	}
	if fn, err := parseWorkerEnv("", nil); err != nil || fn != nil {
		t.Errorf("a nil config produced a mapping (%t) or an error (%v), want neither",
			fn != nil, err)
	}
}

// Only some nodes parameterised is legal: ComposeVarEnv skips what it was not
// given a port for, and exporting an empty value would make compose publish
// port "" and fail to parse.
func TestAPartiallyParameterisedConfigMapsOnlyWhatItDeclares(t *testing.T) {
	cfg := &schema.Config{Harness: schema.HarnessConfig{
		Nodes: []schema.NodeConfig{
			{ID: "kv-n1", Service: "kv", PortEnv: "KV_PORT_N1"},
			{ID: "kv-n2", Service: "kv"},
		},
	}}
	fn, err := parseWorkerEnv("", cfg)
	if err != nil {
		t.Fatalf("parseWorkerEnv: %v", err)
	}
	env := envOf(t, fn)
	if env["KV_PORT_N1"] != "19000" {
		t.Errorf("KV_PORT_N1 = %q, want 19000", env["KV_PORT_N1"])
	}
	// kv-n2 declared no port_env, so NOTHING may be exported for it. Exporting
	// the variable empty is worse than omitting it: `${KV_PORT_N2:-18082}` falls
	// back correctly when the variable is UNSET, and publishes port "" when it is
	// set-but-empty, which compose rejects with a parse error.
	if v, ok := env["KV_PORT_N2"]; ok {
		t.Errorf("KV_PORT_N2 was exported as %q for a node that declares no port_env", v)
	}
}
