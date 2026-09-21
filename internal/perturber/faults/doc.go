// Package faults injects and withdraws the `net.*` fault family, and owns the
// NET_ADMIN sidecar mechanism every other family will reuse.
//
// # The mechanism (DECISIONS.md D-008)
//
// A short-lived container is joined to the target's network namespace with
// `--network container:<id> --cap-add NET_ADMIN`, runs one shell script, and is
// removed. The target is never modified, never restarted, and never has to
// cooperate: which is what invariant I1 ("work on unmodified systems")
// requires and what ambassador proxies cannot deliver.
//
// # Ownership, and the two different ownership stories
//
// iptables rules carry `-m comment --comment "thesis:<run_id>:<fault_id>"`.
// Withdrawal matches on the COMMENT and never reconstructs the rule spec: a
// reconstructed spec fails to match after a partial removal, and then HEAL
// cannot clean up after its own partial failure (D-026).
//
// tc has no comments, so tc ownership is POSITIONAL and is recorded rather than
// tagged. Every shaped interface gets exactly one perturber-owned root qdisc at
// handle 1: (a 16-band prio whose priomap sends all unfiltered traffic to band
// 1:2), and each shaping fault gets one SLOT in 3..16. A slot fixes three
// things at once, all derived from the slot number:
//
//	class  1:<slot>          where the filtered traffic goes
//	qdisc  <slot*10>:        the netem attached to that class
//	pref   <slot>            the u32 filters selecting the traffic
//
// so "what do we own on this interface" has an exact answer that survives a
// process restart, and withdrawal deletes exactly those three things. The
// authority on whether anything of ours is left, however, is not the ledger but
// the BOOT baseline: HEAL diffs `tc qdisc show` against the snapshot taken
// before any fault existed. See ResidualReport.
//
// # Why HEAL compares against an observed baseline and not an empty table
//
// Measured on this machine, an unmodified `nginx:alpine` container ships six
// DNAT/SNAT rules of its own in the nat table for Docker's embedded DNS
// resolver. An assumed-empty residual check false-fails on it immediately. The
// baseline is captured at BOOT, per node, and is what "clean" is defined
// against (D-026).
//
// # Why faults are applied at ONE endpoint
//
// A partition is applied at each node in the SELECTED set, dropping every
// address of every node in the complement, in the INPUT and OUTPUT chains both.
// That is already bidirectional: no packet crosses in either direction. The
// mistake D-026 warns about is a single `-I INPUT -s peer -j DROP`, which drops
// one direction only; the fix is INPUT *and* OUTPUT, not a second sidecar at
// the far end.
//
// Applying at one endpoint is preferred for three reasons. It is closer to
// atomic: one sidecar, one script, one netns transaction, instead of k
// separate `docker run` invocations with a half-applied partition between them.
// It bounds the blast radius of a partial withdrawal failure to one node. And
// it halves the injection latency, which matters because a fault window is a
// virtual-clock interval the schedule promised to honour.
//
// The same single-endpoint argument holds for shaping, for a less obvious
// reason: an egress delay at n1 toward every peer makes every ROUND TRIP
// involving n1 longer by that delay, whichever end initiated it. A second
// shaper at the far end would double the RTT penalty, not symmetrise it.
//
// # Why the affected traffic is peer-addressed, not interface-wide
//
// Rules and filters name the peers' IP addresses. Nothing else is touched, so
// traffic between a node and the orchestrator, or between a node and a client
// on the host, survives a partition untouched. That is not a convenience: the
// KV fixture's stale read is only observable while the displaced leader is
// still reachable by a client, and a fault that cut the client plane too would
// model a dead node, which cannot serve a stale read to anybody.
//
// # Determinism (D-025)
//
// Every netem qdisc is given an explicit `seed`, derived from the world seed
// through the recorder's path-keyed derivation. Without one the kernel picks a
// fresh random seed per injection (measured, three injections, three different
// seeds) and net.loss/reorder/duplicate stop replaying.
package faults
