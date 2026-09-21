package faults

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/WrdCstlg/pro-thesis/internal/perturber"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// NetInjector adapts the network mechanism to perturber.Injector.
//
// Injector (this file's embedded *Injector) is the MECHANISM: it owns the
// sidecar, the baselines, the tc slot ledger and the seed derivation, and it
// mints a *NetFault per occurrence. NetInjector is the thin layer above it that
// the executor drives: the same shape ProcessInjector, ClockInjector and
// IOInjector present.
//
// The split is worth keeping. The mechanism is exercised directly by the
// integration tests against a real container, without a schedule or an executor
// anywhere in the picture; the adapter is what makes it addressable from a
// compiled schedule. Collapsing them would make the mechanism untestable except
// through the whole control plane.
type NetInjector struct {
	*Injector

	mu   sync.Mutex
	live map[string]*NetFault
	done []*NetFault
}

// NewNetInjector wraps a network mechanism for the executor.
func NewNetInjector(in *Injector) *NetInjector {
	return &NetInjector{Injector: in, live: map[string]*NetFault{}}
}

// Kinds reports the network family.
//
// net.bandwidth is included because the mechanism implements it; a kind the
// mechanism cannot serve must still be claimed here if it is to fail loudly at
// injection time rather than as "no injector is registered", which reads like a
// build error rather than a platform limit.
func (n *NetInjector) Kinds() []schema.FaultKind {
	return []schema.FaultKind{
		schema.FaultNetPartition,
		schema.FaultNetLatency,
		schema.FaultNetLoss,
		schema.FaultNetReorder,
		schema.FaultNetDuplicate,
		schema.FaultNetBandwidth,
	}
}

// Inject applies one network fault.
//
// A failed injection is rolled back immediately rather than left half-applied.
// A partition applied to two of three nodes is not "mostly a partition": it is
// an unrecorded topology nobody planned, and leaving it would poison every world
// that follows on this host.
func (n *NetInjector) Inject(ctx context.Context, req perturber.InjectRequest) error {
	f, err := n.NewNetFault(NetRequest{
		FaultID:    req.FaultID,
		Spec:       req.Spec,
		Resolution: resolutionFromNodes(req.Nodes),
	})
	if err != nil {
		return err
	}
	if err := f.Inject(ctx); err != nil {
		if wErr := f.Withdraw(ctx); wErr != nil {
			return errors.Join(err, fmt.Errorf("rollback after a failed injection also failed, so "+
				"residue may remain on the network: %w", wErr))
		}
		return err
	}
	n.mu.Lock()
	n.live[req.FaultID] = f
	n.mu.Unlock()
	return nil
}

// Withdraw removes one network fault.
//
// If the fault is not in the live map (a resumed process, or an injection that
// failed after applying part of its plan) it is rebuilt from the spec and
// withdrawn anyway. Withdrawal matches on the ownership comment, so a rebuilt
// fault removes exactly what the original applied (D-026).
func (n *NetInjector) Withdraw(ctx context.Context, req perturber.WithdrawRequest) error {
	n.mu.Lock()
	f, ok := n.live[req.FaultID]
	if ok {
		delete(n.live, req.FaultID)
	}
	n.mu.Unlock()

	if !ok {
		var err error
		f, err = n.NewNetFault(NetRequest{
			FaultID:    req.FaultID,
			Spec:       req.Spec,
			Resolution: resolutionFromNodes(req.Nodes),
		})
		if err != nil {
			return err
		}
	}
	err := f.Withdraw(ctx)
	n.mu.Lock()
	n.done = append(n.done, f)
	n.mu.Unlock()
	return err
}

// VerifyClean diffs every node's live network state against its BOOT baseline.
//
// The authority is the baseline, not this injector's own ledger. A ledger only
// knows what it believes it applied; the baseline knows what is actually there,
// including a rule a crashed earlier run left behind. That difference is the
// whole reason HEAL exists (D-026).
func (n *NetInjector) VerifyClean(ctx context.Context, _ perturber.VerifyRequest) ([]perturber.Residue, error) {
	if n.Baselines() == nil {
		// No baseline means no definition of clean. Saying so is the honest
		// answer; reporting "clean" would be a verified-nothing that reads as a
		// verified-something.
		return nil, fmt.Errorf("%w: no BOOT network baseline was captured, so residue cannot be "+
			"judged against anything", ErrUnsupported)
	}
	rep, err := n.VerifyResidual(ctx)
	if err != nil {
		return nil, err
	}
	var out []perturber.Residue
	for _, r := range rep.Nodes {
		// A container that is gone took its network namespace with it, so
		// nothing of ours can have survived. That is clean, and reporting it as
		// residue would fail every world that legitimately killed a node.
		if r.NotRunning {
			continue
		}
		// An unreachable node is NOT clean: nothing was verified. Silence here
		// would turn "could not check" into "checked and fine", which is the
		// difference between an honest INCONCLUSIVE and a false PASS.
		if r.Unreachable != "" {
			out = append(out, perturber.Residue{
				NodeID:    r.NodeID,
				Mechanism: "netns",
				Detail:    "could not be inspected, so no residual check ran: " + r.Unreachable,
			})
		}
		for _, rule := range r.TaggedRules {
			out = append(out, perturber.Residue{
				NodeID:    r.NodeID,
				Mechanism: "iptables",
				FaultID:   faultIDFromTag(rule),
				Detail:    rule,
			})
		}
		for _, q := range r.QdiscAdded {
			out = append(out, perturber.Residue{
				NodeID:    r.NodeID,
				Mechanism: "tc-qdisc",
				Detail:    q,
			})
		}
	}
	// A stray sidecar holds a target's network namespace open. It is residue
	// even though it installs no rule, because it is a container this run
	// created and did not remove.
	for _, id := range rep.StraySidecarIDs {
		out = append(out, perturber.Residue{
			Mechanism: "sidecar",
			Detail:    "sidecar container " + id + " was still running and had to be swept",
		})
	}
	return out, nil
}

// faultIDFromTag recovers the fault id from an ownership comment of the form
// `thesis:<run_id>:<fault_id>`, so a residue names the fault that leaked it
// rather than only the rule text.
func faultIDFromTag(rule string) string {
	i := strings.Index(rule, "thesis:")
	if i < 0 {
		return ""
	}
	rest := rule[i+len("thesis:"):]
	// rest is `<run_id>:<fault_id>...`; take the field after the first colon,
	// stopping at the first character that cannot be part of an id.
	j := strings.IndexByte(rest, ':')
	if j < 0 {
		return ""
	}
	rest = rest[j+1:]
	end := strings.IndexAny(rest, `" `)
	if end < 0 {
		return rest
	}
	return rest[:end]
}

// Realized reports what the network family actually did, for the world file.
func (n *NetInjector) Realized() []*NetFault {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := append([]*NetFault(nil), n.done...)
	for _, f := range n.live {
		out = append(out, f)
	}
	return out
}

// resolutionFromNodes converts the executor's already-resolved node list into
// the mechanism's Resolution.
//
// The executor resolves `role:leader` and the quorum forms against live cluster
// state at injection time and hands down concrete nodes, so the mechanism never
// re-resolves and cannot disagree with what the realized schedule records.
func resolutionFromNodes(nodes []perturber.Node) Resolution {
	ids := make([]string, 0, len(nodes))
	for _, nd := range nodes {
		ids = append(ids, nd.ID)
	}
	// Peers is left empty on purpose. Empty means "every node not in Selected",
	// which is exactly what partitioning a subset from the rest of the cluster
	// means, and it is what `minority(kv)` denotes. Naming peers explicitly here
	// would silently narrow a partition to whatever the resolver happened to
	// return, turning a quorum-losing partition into a partial one.
	return Resolution{Selected: ids}
}
