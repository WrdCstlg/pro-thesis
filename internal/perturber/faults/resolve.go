package faults

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sort"
	"strings"

	"github.com/WrdCstlg/pro-thesis/internal/recorder"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// NetNode is one logical node as the network family needs to see it.
//
// It is deliberately a small local type rather than recorder.Topology or the
// process family's Target: the injector must be constructible in a unit test
// with no compose project, the network family needs four facts about a node and
// not the whole handoff record, and the three families are being built in
// parallel: a shared struct that moved underneath one of them would break the
// other two for no gain.
type NetNode struct {
	// NodeID is the logical node id from prothesis.yaml.
	NodeID string
	// ContainerID is the container backing the node right now. A restart or a
	// compose recreate changes it.
	ContainerID string
	// ComposeService is the PHYSICAL compose service, never the logical group.
	ComposeService string
	// Group is the LOGICAL service group the fault grammar scopes over: the
	// `kv` of `kv:*` and `minority(kv)`, which under one-service-per-node is
	// not the compose service name (D-020).
	Group string
	// HostPort is the published 127.0.0.1 client port, when one is known.
	HostPort int64
}

// NodesFromTopology adapts the harness handoff record into the concrete
// targets every family in this package addresses.
//
// The logical group comes from prothesis.yaml, not from the topology record:
// recorder.NodeBinding.Service carries the COMPOSE service, which under
// one-service-per-node is `kv-n1` and not the `kv` that `minority(kv)` and
// `kv:*` resolve over (D-020).
func NodesFromTopology(cfg *schema.Config, top *recorder.Topology) ([]NetNode, error) {
	if top == nil {
		return nil, fmt.Errorf("faults: no topology; the network family needs container ids")
	}
	group := map[string]string{}
	if cfg != nil {
		for _, n := range cfg.Harness.Nodes {
			group[n.ID] = n.Service
		}
	}
	out := make([]NetNode, 0, len(top.Nodes))
	for _, nb := range top.Nodes {
		if nb.ContainerID == "" {
			return nil, fmt.Errorf("faults: node %q has no container id in the topology record; "+
				"a network fault cannot address it", nb.ID)
		}
		out = append(out, NetNode{
			NodeID:         nb.ID,
			ContainerID:    nb.ContainerID,
			ComposeService: nb.Service,
			Group:          group[nb.ID],
			HostPort:       nb.HostPort,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].NodeID < out[j].NodeID })
	return out, nil
}

// ---------------------------------------------------------------------------
// target resolution
// ---------------------------------------------------------------------------

// Resolution binds a fault's TARGET to concrete nodes.
//
// It is split into the nodes the fault ACTS ON and the nodes on the other side
// of the cut, because those are different sets and conflating them is how a
// partition of a two-node minority accidentally severs the link inside the
// minority as well.
type Resolution struct {
	// Selected are the nodes the rules and qdiscs are installed on.
	Selected []string
	// Peers are the nodes whose addresses the fault drops or shapes. Empty
	// means "every node not in Selected", which is what partitioning a subset
	// from the rest of the cluster means.
	Peers []string
}

// ResolveStatic binds the target forms that need no live cluster state: a node,
// a service wildcard, and an edge.
//
// role: and quorum targets are NOT resolved here and never silently guessed.
// They bind at injection time from live state (Raft leadership moves at
// runtime) and the component that owns that state owns the binding. Returning
// a concrete-looking answer for `role:leader` without asking the cluster would
// produce a world that records a fault against a node that was not the leader,
// which is precisely the failure the planned/realized split exists to prevent
// (D-012).
func ResolveStatic(spec schema.FaultSpec, nodes []NetNode) (Resolution, error) {
	byID := map[string]bool{}
	for _, t := range nodes {
		byID[t.NodeID] = true
	}
	t := spec.Target
	switch t.Kind {
	case schema.TargetNode:
		if !byID[t.Node] {
			return Resolution{}, fmt.Errorf("faults: %s targets node %q, which the topology does not contain",
				spec.Kind, t.Node)
		}
		return Resolution{Selected: []string{t.Node}}, nil

	case schema.TargetWildcard:
		var sel []string
		for _, n := range nodes {
			if n.Group == t.Service {
				sel = append(sel, n.NodeID)
			}
		}
		if len(sel) == 0 {
			// D-018's rule, applied to faults rather than to probes: a selector
			// that matches nothing must be an error. A fault that fires against
			// zero nodes would let a world report PASS having perturbed
			// nothing.
			return Resolution{}, fmt.Errorf("faults: %s targets %s, which matches no node; "+
				"a fault that matches nothing would be a silent no-op", spec.Kind, t)
		}
		sort.Strings(sel)
		return Resolution{Selected: sel}, nil

	case schema.TargetEdge:
		if !byID[t.A] || !byID[t.B] {
			return Resolution{}, fmt.Errorf("faults: %s targets edge %s, whose endpoints are not both in the topology",
				spec.Kind, t)
		}
		// One endpoint, both directions. Installing at A with INPUT and OUTPUT
		// rules against B already severs the link symmetrically; a second
		// install at B would double the work and the residue for no additional
		// effect.
		return Resolution{Selected: []string{t.A}, Peers: []string{t.B}}, nil

	case schema.TargetRole, schema.TargetQuorum:
		return Resolution{}, fmt.Errorf("faults: %s binds to concrete nodes only from live cluster state; "+
			"resolve it first and pass the result in NetRequest.Resolution", t)
	}
	return Resolution{}, fmt.Errorf("faults: unknown target kind %q", t.Kind)
}

// complement returns every node not in sel, in id order.
func complement(nodes []NetNode, sel []string) []string {
	in := map[string]bool{}
	for _, s := range sel {
		in[s] = true
	}
	var out []string
	for _, t := range nodes {
		if !in[t.NodeID] {
			out = append(out, t.NodeID)
		}
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------------------
// address discovery
// ---------------------------------------------------------------------------

// NodeAddrs is one node's container addresses, across every attached network.
type NodeAddrs struct {
	NodeID  string
	Addrs   []string // every IPv4 and IPv6 address, sorted
	Running bool
}

// dockerNetwork is the shape of one entry of .NetworkSettings.Networks.
type dockerNetwork struct {
	IPAddress         string `json:"IPAddress"`
	GlobalIPv6Address string `json:"GlobalIPv6Address"`
}

type dockerInspectNet struct {
	State struct {
		Running bool `json:"Running"`
	} `json:"State"`
	NetworkSettings struct {
		Networks map[string]dockerNetwork `json:"Networks"`
	} `json:"NetworkSettings"`
}

// inspectAddrs resolves live container addresses.
//
// Addresses are read fresh on every injection rather than cached with the BOOT
// baseline. A container recreated between BOOT and the fault window (by
// proc.restart, or by the clock family, which can only skew a clock by
// recreating into a time namespace (D-024, OQ-020)) keeps its interface names
// and its subnets but need not keep its address, and a partition built from a
// stale address is a partition against nobody.
func (in *Injector) inspectAddrs(ctx context.Context, ids []string) (map[string]NodeAddrs, error) {
	if len(ids) == 0 {
		return map[string]NodeAddrs{}, nil
	}
	args := append([]string{"inspect", "--format", "{{json .}}"}, ids...)
	stdout, stderr, err := in.runner().Run(ctx, nil, in.dockerBin(), args...)
	if err != nil {
		return nil, fmt.Errorf("faults: docker inspect: %w: %s", err, strings.TrimSpace(stderr))
	}
	lines := nonEmptyLines(stdout)
	if len(lines) != len(ids) {
		return nil, fmt.Errorf("faults: docker inspect returned %d records for %d containers",
			len(lines), len(ids))
	}
	out := make(map[string]NodeAddrs, len(ids))
	for i, l := range lines {
		var d dockerInspectNet
		if err := json.Unmarshal([]byte(l), &d); err != nil {
			return nil, fmt.Errorf("faults: parse docker inspect output: %w", err)
		}
		na := NodeAddrs{NodeID: in.nodeIDForContainer(ids[i]), Running: d.State.Running}
		for _, n := range d.NetworkSettings.Networks {
			if n.IPAddress != "" {
				na.Addrs = append(na.Addrs, n.IPAddress)
			}
			if n.GlobalIPv6Address != "" {
				na.Addrs = append(na.Addrs, n.GlobalIPv6Address)
			}
		}
		sort.Strings(na.Addrs)
		out[na.NodeID] = na
	}
	return out, nil
}

// ifaceForAddr picks the interface on the injecting node whose prefix contains
// addr.
//
// Shaping is per-interface: a u32 filter installed on eth0 never sees traffic
// that egresses eth1. On the reference fixture that distinction is the whole
// point: the client plane and the Raft peer plane are separate networks, and a
// fault that shaped the wrong one would either be invisible or would model a
// dead node instead of a gray one.
func ifaceForAddr(base Baseline, addr string) (string, bool) {
	ip := net.ParseIP(addr)
	if ip == nil {
		return "", false
	}
	for _, ifc := range base.Ifaces {
		if ifc.Name == "lo" {
			continue
		}
		for _, c := range ifc.CIDRs {
			_, netw, err := net.ParseCIDR(c)
			if err != nil || netw == nil {
				continue
			}
			if netw.Contains(ip) {
				return ifc.Name, true
			}
		}
	}
	return "", false
}
