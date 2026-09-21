package perturber

import (
	"fmt"
	"sort"

	"github.com/WrdCstlg/pro-thesis/internal/recorder"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// Node is one logical node of the live topology, as the resolver sees it.
//
// It is a JOIN of the two records that each hold half the truth: prothesis.yaml
// knows the logical grouping the fault grammar targets, and the backend's
// recorder.Topology knows which container is actually serving it.
type Node struct {
	// ID is the logical node id from harness.nodes[].id. It is what a node
	// target (`n1`) names, and what a realized fault records.
	ID string

	// Service is the LOGICAL group, and it is what `kv:*`, `minority(kv)`,
	// `majority(kv)`, `any(2, kv)` and the constraint vocabulary select over,
	// NOT ComposeService. The two are separate fields because container IPs are
	// not routable from a Windows host, which forces one compose service per
	// node while the frozen grammar still resolves `minority(kv)` over a group
	// of three. See DECISIONS.md D-020 and OQ-016. Containers carry this value
	// as the `io.prothesis.group` label, so a backend can select a group without
	// re-reading prothesis.yaml.
	Service string

	// ComposeService is the PHYSICAL service backing the node: what an injector
	// addresses. Equal to Service when the config declares no compose_service.
	ComposeService string

	// ContainerID is the container bound to this node, or "" when no live
	// topology was supplied. An injector that needs a container namespace must
	// treat "" as an error rather than injecting against nothing.
	ContainerID string

	// HostPort is the published 127.0.0.1 port and ContainerPort the
	// in-container client port. HostPort is how a role observation is taken:
	// container IPs are unreachable from this host (D-010).
	HostPort      int64
	ContainerPort int64

	// RoleHint is harness.nodes[].role_hint. It is ADVISORY and static, and it
	// is deliberately NOT used to resolve a `role:` target: see Resolver. A
	// hint that said "replica" would resolve `role:leader` to nothing on a
	// healthy cluster, and a hint that said "leader" would resolve it to a node
	// that was deposed ten seconds ago.
	RoleHint string
}

// Topology is the resolver's view of the live system.
//
// Nodes are held in ID-sorted order and every derived slice preserves it, so a
// resolution never depends on the order harness.nodes happens to be written in.
// Reordering the config must not change which node a quorum target selects.
type Topology struct {
	nodes []Node
}

// NewTopology joins the configured nodes with the backend's live bindings.
//
// bind may be nil: a schedule can be compiled and its static targets checked
// before anything is running, which is what lets `Compile` reject
// `minority(pg)` against a topology that has no pg without booting a container.
func NewTopology(cfg *schema.Config, bind *recorder.Topology) (*Topology, error) {
	if cfg == nil {
		return nil, fmt.Errorf("perturber: NewTopology needs a config")
	}
	bound := map[string]recorder.NodeBinding{}
	if bind != nil {
		for _, b := range bind.Nodes {
			bound[b.ID] = b
		}
	}
	nodes := make([]Node, 0, len(cfg.Harness.Nodes))
	for _, n := range cfg.Harness.Nodes {
		node := Node{
			ID:             n.ID,
			Service:        n.Service,
			ComposeService: n.EffectiveComposeService(),
			ContainerPort:  int64(n.ClientPort()),
			RoleHint:       n.RoleHint,
		}
		if b, ok := bound[n.ID]; ok {
			node.ContainerID = b.ContainerID
			node.HostPort = b.HostPort
			if b.ContainerPort > 0 {
				node.ContainerPort = b.ContainerPort
			}
			if b.Service != "" {
				node.ComposeService = b.Service
			}
		}
		nodes = append(nodes, node)
	}
	return NewTopologyFromNodes(nodes)
}

// NewTopologyFromNodes builds a topology from an explicit node list. It is the
// seam tests build against.
func NewTopologyFromNodes(nodes []Node) (*Topology, error) {
	out := make([]Node, len(nodes))
	copy(out, nodes)
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })

	seen := make(map[string]bool, len(out))
	for _, n := range out {
		if n.ID == "" {
			return nil, fmt.Errorf("perturber: topology has a node with an empty id")
		}
		if seen[n.ID] {
			return nil, fmt.Errorf("perturber: topology has two nodes with id %q", n.ID)
		}
		seen[n.ID] = true
	}
	return &Topology{nodes: out}, nil
}

// Nodes returns every node, ID-sorted.
func (t *Topology) Nodes() []Node {
	out := make([]Node, len(t.nodes))
	copy(out, t.nodes)
	return out
}

// Len is the number of nodes.
func (t *Topology) Len() int { return len(t.nodes) }

// Node looks a node up by logical id.
func (t *Topology) Node(id string) (Node, bool) {
	for _, n := range t.nodes {
		if n.ID == id {
			return n, true
		}
	}
	return Node{}, false
}

// Service returns every node in the named LOGICAL group, ID-sorted.
func (t *Topology) Service(name string) []Node {
	out := make([]Node, 0, len(t.nodes))
	for _, n := range t.nodes {
		if n.Service == name {
			out = append(out, n)
		}
	}
	return out
}

// Services lists the logical groups present, sorted. Used only for error
// messages, so a "matches no node" failure can say what WOULD have matched.
func (t *Topology) Services() []string {
	seen := map[string]bool{}
	out := []string{}
	for _, n := range t.nodes {
		if n.Service == "" || seen[n.Service] {
			continue
		}
		seen[n.Service] = true
		out = append(out, n.Service)
	}
	sort.Strings(out)
	return out
}

// NodeIDs lists the logical node ids, sorted. Error messages only.
func (t *Topology) NodeIDs() []string {
	out := make([]string, 0, len(t.nodes))
	for _, n := range t.nodes {
		out = append(out, n.ID)
	}
	return out
}

// nodeIDs projects a node slice to its ids, preserving order.
func nodeIDs(nodes []Node) []string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, n.ID)
	}
	return out
}
