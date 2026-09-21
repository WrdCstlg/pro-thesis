package faults

import (
	"fmt"
	"sort"
)

// iptables rule construction.
//
// Every generated command is a pure function of validated inputs, so the whole
// of it is unit-testable without a container. That matters more than it looks:
// the difference between a partition and a one-way drop is one missing OUTPUT
// rule, and a one-way drop still looks like a fault while producing entirely
// the wrong distributed-systems behaviour.

const (
	// iptablesBin and ip6tablesBin are resolved from PATH inside the sidecar.
	iptablesBin  = "iptables"
	ip6tablesBin = "ip6tables"

	// waitFlag makes iptables block on the xtables lock rather than failing.
	// Each sidecar has its own mount namespace and therefore its own
	// /run/xtables.lock, so contention is only ever between the commands of one
	// script: but a partition applied while the target itself is editing rules
	// would otherwise fail intermittently.
	waitFlag = "-w"
)

// PartitionCommands builds the commands that cut a node off from a set of peer
// addresses, in both directions.
//
// Both directions, at ONE endpoint. `-I INPUT -s peer -j DROP` alone is a
// one-way drop: the peer still receives everything we send, so the cluster does
// not observe a symmetric partition and a Raft minority can still push
// AppendEntries out (D-026). Pairing it with `-I OUTPUT -d peer -j DROP` in the
// same namespace makes the cut symmetric without a second sidecar at the far
// end.
//
// Rules are INSERTED at position 1, not appended: a target that ships its own
// ACCEPT rules would otherwise accept the traffic before our DROP is reached,
// and the fault would be a silent no-op.
func PartitionCommands(tag string, peerAddrs []string) ([]string, error) {
	if tag == "" {
		return nil, fmt.Errorf("faults: partition needs an ownership tag")
	}
	if len(peerAddrs) == 0 {
		return nil, fmt.Errorf("faults: partition has no peer addresses to drop; " +
			"a fault that matches nothing is a silent no-op, not a partition")
	}
	sorted := append([]string(nil), peerAddrs...)
	sort.Strings(sorted)

	var cmds []string
	for _, a := range sorted {
		v6, err := checkAddr(a)
		if err != nil {
			return nil, err
		}
		bin := iptablesBin
		suffix := "/32"
		if v6 {
			bin = ip6tablesBin
			suffix = "/128"
		}
		cmds = append(cmds,
			fmt.Sprintf("%s %s -I INPUT 1 -s %s%s -m comment --comment '%s' -j DROP",
				bin, waitFlag, a, suffix, tag),
			fmt.Sprintf("%s %s -I OUTPUT 1 -d %s%s -m comment --comment '%s' -j DROP",
				bin, waitFlag, a, suffix, tag),
		)
	}
	return cmds, nil
}

// WithdrawIPTablesCommands builds the commands that remove every rule carrying
// tag, in both address families.
//
// It matches on the COMMENT and never reconstructs the rule spec. Two reasons,
// both learned the hard way in the D-026 rehearsal. A reconstructed spec fails
// to match if the rule was already partially removed, so HEAL cannot clean up
// after its own partial failure. And a reconstructed spec depends on the
// injector and the withdrawer agreeing about normalization (/32 suffixes,
// match ordering, protocol defaults) which they will eventually not.
//
// Deletion runs from the highest line number down. iptables renumbers on every
// delete, so removing rule 1 before rule 3 leaves the wrong rule at index 3.
func WithdrawIPTablesCommands(tag string) ([]string, error) {
	if tag == "" {
		return nil, fmt.Errorf("faults: withdrawal needs an ownership tag")
	}
	var cmds []string
	for _, bin := range []string{iptablesBin, ip6tablesBin} {
		cmds = append(cmds, fmt.Sprintf(
			"for c in INPUT OUTPUT FORWARD; do "+
				"for n in $(%s %s -L $c -n --line-numbers 2>/dev/null | grep -F '%s' | awk '{print $1}' | sort -rn); do "+
				"%s %s -D $c $n || true; "+
				"done; done",
			bin, waitFlag, tag, bin, waitFlag))
	}
	return cmds, nil
}

// CountTaggedCommand reports how many rules carrying `match` survive, across both
// families. It is what a withdrawal script ends with, so withdrawal verifies
// itself rather than trusting that its deletes landed.
//
// WHAT IS PASSED AS match IS THE WHOLE CORRECTNESS ARGUMENT, and the two callers
// need different things:
//
//   - A single fault's withdrawal must pass its OWN tag. It deletes by that tag,
//     so it may only be held to that tag. Counting the shared TagPrefix instead
//     makes a withdrawal fail because a CONCURRENT fault's rules legitimately
//     exist on the same node, and `perturber.budget.max_concurrent_faults` is 3,
//     so overlapping faults are the design, not an edge case. Measured: a
//     net.partition and a net.loss on kv-n2 in the same world reported "8 tagged
//     iptables rule(s) survived" and failed the whole world INCONCLUSIVE with
//     nothing actually leaked. See OQ-048.
//   - The HEAL-time residual sweep must pass TagPrefix. By then nothing of ours
//     may survive on any node, and per-fault counting would miss a rule whose
//     owning fault never got as far as registering a withdrawal.
//
// The qdisc half of the same self-check has always been per-fault (it verifies
// p.slots, not every slot on the node); this makes the iptables half agree.
func CountTaggedCommand(match string) string {
	if match == "" {
		match = TagPrefix
	}
	return "echo '#TAGGED'\n" +
		"{ " + iptablesBin + " " + waitFlag + " -S 2>/dev/null; " +
		ip6tablesBin + " " + waitFlag + " -S 2>/dev/null; } | grep -c -F '" + match + "' || true"
}
