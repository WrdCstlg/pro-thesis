package faults

import (
	"strings"
	"testing"

	"github.com/WrdCstlg/pro-thesis/internal/recorder"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

func fixtureNodes() []NetNode {
	return []NetNode{
		{NodeID: "kv-n1", ContainerID: "c1", ComposeService: "kv-n1", Group: "kv"},
		{NodeID: "kv-n2", ContainerID: "c2", ComposeService: "kv-n2", Group: "kv"},
		{NodeID: "kv-n3", ContainerID: "c3", ComposeService: "kv-n3", Group: "kv"},
		{NodeID: "pg", ContainerID: "c4", ComposeService: "pg", Group: "postgres"},
	}
}

func TestResolveStaticNode(t *testing.T) {
	r, err := ResolveStatic(mustSpec(t, "net.partition(kv-n2)@0..1"), fixtureNodes())
	if err != nil {
		t.Fatalf("ResolveStatic: %v", err)
	}
	if len(r.Selected) != 1 || r.Selected[0] != "kv-n2" {
		t.Fatalf("Selected = %v", r.Selected)
	}
	if len(r.Peers) != 0 {
		t.Fatalf("a node target's far side is the complement, computed later; got %v", r.Peers)
	}
}

// `kv:*` must resolve over the LOGICAL group, not the compose service. Under
// one-service-per-node the compose services are kv-n1..kv-n3 and the group is
// kv (D-020).
func TestResolveStaticWildcardUsesTheLogicalGroup(t *testing.T) {
	r, err := ResolveStatic(mustSpec(t, "net.partition(kv:*)@0..1"), fixtureNodes())
	if err != nil {
		t.Fatalf("ResolveStatic: %v", err)
	}
	if strings.Join(r.Selected, ",") != "kv-n1,kv-n2,kv-n3" {
		t.Fatalf("Selected = %v, want the three kv nodes and not pg", r.Selected)
	}
}

// D-018's rule, applied to faults: a selector matching nothing must be an
// error, never a fault that fires against zero nodes.
func TestResolveStaticWildcardMatchingNothingIsAnError(t *testing.T) {
	_, err := ResolveStatic(mustSpec(t, "net.partition(redis:*)@0..1"), fixtureNodes())
	if err == nil {
		t.Fatal("a wildcard matching no node must be an error, not a silent no-op")
	}
	if !strings.Contains(err.Error(), "no-op") {
		t.Fatalf("error does not explain the danger: %v", err)
	}
}

func TestResolveStaticEdge(t *testing.T) {
	r, err := ResolveStatic(mustSpec(t, "net.latency(kv-n1<->kv-n3, 40)@0..1"), fixtureNodes())
	if err != nil {
		t.Fatalf("ResolveStatic: %v", err)
	}
	if len(r.Selected) != 1 || r.Selected[0] != "kv-n1" {
		t.Fatalf("Selected = %v, want one endpoint", r.Selected)
	}
	if len(r.Peers) != 1 || r.Peers[0] != "kv-n3" {
		t.Fatalf("Peers = %v, want the far endpoint only", r.Peers)
	}
}

func TestResolveStaticEdgeCanonicalOrder(t *testing.T) {
	// pkg/schema stores edge endpoints lexicographically, so n3<->n1 and
	// n1<->n3 must resolve identically.
	a, err := ResolveStatic(mustSpec(t, "net.partition(kv-n3<->kv-n1)@0..1"), fixtureNodes())
	if err != nil {
		t.Fatalf("ResolveStatic: %v", err)
	}
	b, _ := ResolveStatic(mustSpec(t, "net.partition(kv-n1<->kv-n3)@0..1"), fixtureNodes())
	if a.Selected[0] != b.Selected[0] || a.Peers[0] != b.Peers[0] {
		t.Fatalf("edge order changed the resolution: %+v vs %+v", a, b)
	}
}

// role: and quorum bind from live cluster state. Guessing here would record a
// world against a node that was not the leader.
func TestResolveStaticRefusesDynamicTargets(t *testing.T) {
	for _, f := range []string{
		"net.partition(role:leader)@0..1",
		"net.partition(minority(kv))@0..1",
		"net.partition(majority(kv))@0..1",
		"net.partition(any(2, kv))@0..1",
	} {
		if _, err := ResolveStatic(mustSpec(t, f), fixtureNodes()); err == nil {
			t.Fatalf("%s must not be resolved without live cluster state", f)
		}
	}
}

func TestResolveStaticUnknownNode(t *testing.T) {
	if _, err := ResolveStatic(mustSpec(t, "net.partition(kv-n9)@0..1"), fixtureNodes()); err == nil {
		t.Fatal("a target naming a node outside the topology must be an error")
	}
}

func TestComplement(t *testing.T) {
	got := complement(fixtureNodes(), []string{"kv-n1"})
	if strings.Join(got, ",") != "kv-n2,kv-n3,pg" {
		t.Fatalf("complement = %v", got)
	}
	if len(complement(fixtureNodes(), []string{"kv-n1", "kv-n2", "kv-n3", "pg"})) != 0 {
		t.Fatal("the complement of everything must be empty")
	}
}

// Shaping is per-interface: a u32 filter on eth0 never sees traffic that
// egresses eth1. On the fixture that is the difference between a gray failure
// and a dead node.
func TestIfaceForAddrPicksTheRightPlane(t *testing.T) {
	c, err := parseCapture(sampleCapture)
	if err != nil {
		t.Fatalf("parseCapture: %v", err)
	}
	base := Baseline{Ifaces: c.Ifaces}

	if got, ok := ifaceForAddr(base, "172.23.0.7"); !ok || got != "eth0" {
		t.Fatalf("client-plane address resolved to %q, %v; want eth0", got, ok)
	}
	if got, ok := ifaceForAddr(base, "172.30.0.9"); !ok || got != "eth1" {
		t.Fatalf("peer-plane address resolved to %q, %v; want eth1", got, ok)
	}
	if _, ok := ifaceForAddr(base, "10.9.9.9"); ok {
		t.Fatal("an address outside every prefix must not be attributed to an interface")
	}
	// A loopback address must never select lo: shaping lo would delay the
	// container's own health check rather than any peer traffic.
	if got, ok := ifaceForAddr(base, "127.0.0.5"); ok {
		t.Fatalf("loopback resolved to %q; lo must be excluded", got)
	}
}

func TestNodesFromTopology(t *testing.T) {
	cfg := &schema.Config{}
	cfg.Harness.Nodes = []schema.NodeConfig{
		{ID: "kv-n1", Service: "kv"},
		{ID: "kv-n2", Service: "kv"},
	}
	top := &recorder.Topology{Nodes: []recorder.NodeBinding{
		{ID: "kv-n2", Service: "kv-n2", ContainerID: "c2", HostPort: 18082},
		{ID: "kv-n1", Service: "kv-n1", ContainerID: "c1", HostPort: 18081},
	}}
	got, err := NodesFromTopology(cfg, top)
	if err != nil {
		t.Fatalf("NodesFromTopology: %v", err)
	}
	if len(got) != 2 || got[0].NodeID != "kv-n1" {
		t.Fatalf("targets are not sorted by node id: %+v", got)
	}
	// The LOGICAL group comes from prothesis.yaml; the compose service comes
	// from the topology record. Conflating them breaks `minority(kv)`.
	if got[0].Group != "kv" {
		t.Fatalf("group = %q, want the logical group from the config", got[0].Group)
	}
	if got[0].ComposeService != "kv-n1" {
		t.Fatalf("compose service = %q", got[0].ComposeService)
	}
}

func TestNodesFromTopologyRejectsMissingContainer(t *testing.T) {
	top := &recorder.Topology{Nodes: []recorder.NodeBinding{{ID: "kv-n1", Service: "kv-n1"}}}
	if _, err := NodesFromTopology(nil, top); err == nil {
		t.Fatal("a node with no container id cannot be addressed by a network fault")
	}
}
