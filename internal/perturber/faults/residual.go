package faults

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// VerifyResidual is what HEAL calls.
//
// It answers one question with evidence rather than with bookkeeping: is the
// system's network state the same as it was at BOOT, and are there any rules
// left carrying our tag? The ledger is deliberately not consulted. A ledger can
// only report the faults it knows about, and the leaks that matter are the ones
// nobody recorded: an injection that half-succeeded before its process was
// killed, a rule left by a crash between `iptables -I` and the process exiting,
// a fault from a previous run against the same container.
//
// The check runs even when the caller believes every fault was withdrawn. That
// belief is exactly what a leaked DROP rule invalidates, and the cost of being
// wrong is not local: a surviving rule poisons every world executed afterwards
// on the same topology, and the resulting failure looks like a flaky system
// rather than a broken injector (OQ-004).
func (in *Injector) VerifyResidual(ctx context.Context) (*ResidualReport, error) {
	base := in.Baselines()
	if base == nil {
		return nil, fmt.Errorf("faults: cannot verify residue without a BOOT baseline " +
			"(DECISIONS.md D-026: HEAL compares against observed state, never an assumed-empty table)")
	}

	report := &ResidualReport{
		Schema:            ResidualSchema,
		RunID:             in.runID,
		CheckedWallUnixNS: in.now(),
	}

	// Which containers are still alive? A container that is gone took its
	// network namespace with it, so nothing of ours can have survived in it.
	// Asking first avoids a sidecar failure that reads like a leak.
	cids := make([]string, 0, len(in.targets))
	for _, t := range in.targets {
		cids = append(cids, t.ContainerID)
	}
	live, inspectErr := in.inspectAddrs(ctx, cids)

	results := make([]Residue, len(in.targets))
	var wg sync.WaitGroup
	for i, t := range in.targets {
		nb, hasBase := base.Node(t.NodeID)
		if !hasBase {
			results[i] = Residue{
				NodeID:      t.NodeID,
				Unreachable: "no BOOT baseline was captured for this node",
			}
			continue
		}
		if inspectErr == nil {
			if st, ok := live[t.NodeID]; ok && !st.Running {
				results[i] = Residue{NodeID: t.NodeID, NotRunning: true}
				continue
			}
		}
		wg.Add(1)
		go func(i int, t NetNode, nb Baseline) {
			defer wg.Done()
			results[i] = in.residueFor(ctx, t, nb)
		}(i, t, nb)
	}
	wg.Wait()

	report.Nodes = results
	sort.Slice(report.Nodes, func(a, b int) bool { return report.Nodes[a].NodeID < report.Nodes[b].NodeID })

	// Sweeping is part of verification, not a separate courtesy. A sidecar that
	// outlived its `--rm` holds a reference to the target's network namespace,
	// which makes the target's own teardown block, and teardown reliability is
	// a scalability property on a host with a hard ceiling of ~24 bridge
	// networks (D-017).
	strays, sweepErr := in.sidecar.SweepSidecars(ctx)
	report.StraySidecars = len(strays)
	report.StraySidecarIDs = strays

	if sweepErr != nil {
		return report, sweepErr
	}
	return report, nil
}

func (in *Injector) residueFor(ctx context.Context, t NetNode, base Baseline) Residue {
	res, err := in.sidecar.Exec(ctx, t.ContainerID, captureScript)
	if err != nil {
		msg := err.Error()
		if isNotRunning(msg) {
			return Residue{NodeID: t.NodeID, NotRunning: true}
		}
		return Residue{NodeID: t.NodeID, Unreachable: condense(msg)}
	}
	cur, err := parseCapture(res.Stdout)
	if err != nil {
		return Residue{NodeID: t.NodeID, Unreachable: condense(err.Error())}
	}
	return diffBaseline(t.NodeID, base, cur)
}

// isNotRunning recognises the daemon's refusal to join a dead container's
// network namespace. The message is matched rather than a status code because
// the CLI reports every such refusal as a generic exit 125.
func isNotRunning(msg string) bool {
	m := strings.ToLower(msg)
	return strings.Contains(m, "cannot join network of a non running container") ||
		strings.Contains(m, "is not running") ||
		strings.Contains(m, "no such container")
}

func condense(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 300 {
		s = s[:300] + "..."
	}
	return s
}

// SweepTagged removes every `thesis:`-tagged rule from every node, whatever
// fault installed it and whether or not this process knows about it.
//
// This is the recovery path, not the normal one: HEAL withdraws faults
// individually and then verifies. It exists because an interrupted run leaves
// rules no in-memory ledger describes, and `thesis down` must be able to clean
// up the mess an interrupted run made: the same reasoning that makes `down`
// unpause containers before tearing them down (D-017).
func (in *Injector) SweepTagged(ctx context.Context) error {
	cmds, err := WithdrawIPTablesCommands(TagPrefix)
	if err != nil {
		return err
	}
	// The shared prefix, deliberately: this is the recovery sweep, and by the time
	// it runs NOTHING of ours may survive on any node, including rules whose
	// owning fault never got as far as registering a withdrawal.
	cmds = append(cmds, CountTaggedCommand(TagPrefix), "echo '#QDISC'", "tc qdisc show", "echo '#END'")
	prog := script(false, cmds)

	var msgs []string
	for _, t := range in.targets {
		res, err := in.sidecar.Exec(ctx, t.ContainerID, prog)
		if err != nil {
			if isNotRunning(err.Error()) {
				continue
			}
			msgs = append(msgs, fmt.Sprintf("node %s: %v", t.NodeID, err))
			continue
		}
		tagged, _, perr := parseWithdrawOutput(res.Stdout)
		if perr != nil {
			msgs = append(msgs, fmt.Sprintf("node %s: %v", t.NodeID, perr))
			continue
		}
		if tagged > 0 {
			msgs = append(msgs, fmt.Sprintf("node %s: %d tagged rule(s) still present after sweeping",
				t.NodeID, tagged))
		}
	}
	if len(msgs) > 0 {
		return fmt.Errorf("faults: sweep tagged rules: %s", strings.Join(msgs, "; "))
	}
	return nil
}
