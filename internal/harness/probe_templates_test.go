package harness

import (
	"testing"

	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

func TestProbeTemplatesFirstDeclarationWinsAndUncoveredNodesAreAbsent(t *testing.T) {
	cfg := &schema.Config{Harness: schema.HarnessConfig{
		Nodes: []schema.NodeConfig{
			{ID: "kv-n1", Service: "kv"},
			{ID: "kv-n2", Service: "kv"},
			{ID: "proxy-1", Service: "proxy"},
		},
		Health: []schema.HealthProbe{
			{Node: "kv-n1", Probe: "http://{host}:{port}/ready"},
			{Node: "kv:*", Probe: "http://{host}:{port}/health"},
		},
	}}
	got, err := ProbeTemplates(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got["kv-n1"] != "http://{host}:{port}/ready" {
		t.Fatalf("kv-n1: first declaration must win, got %q", got["kv-n1"])
	}
	if got["kv-n2"] != "http://{host}:{port}/health" {
		t.Fatalf("kv-n2: got %q", got["kv-n2"])
	}
	if _, ok := got["proxy-1"]; ok {
		t.Fatalf("proxy-1 is covered by no entry and must be absent, got %q", got["proxy-1"])
	}
	if len(got) != 2 {
		t.Fatalf("got %d entries, want 2: %v", len(got), got)
	}
}

func TestProbeTemplatesRefusesASelectorFormItCannotResolve(t *testing.T) {
	cfg := &schema.Config{Harness: schema.HarnessConfig{
		Nodes:  []schema.NodeConfig{{ID: "n1", Service: "kv"}},
		Health: []schema.HealthProbe{{Node: "role:leader", Probe: "http://{host}:{port}/health"}},
	}}
	if _, err := ProbeTemplates(cfg); err == nil {
		t.Fatal("a role selector in harness.health must be an error, not silently no coverage")
	}
}
