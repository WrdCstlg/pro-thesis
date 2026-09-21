// Package perturber owns the Phase 2 fault schedule: TARGET RESOLUTION,
// SCHEDULE COMPILATION and EXECUTION against the virtual clock.
//
// It deliberately does NOT know how to talk to a container. The mechanics of
// iptables, tc/netem, SIGSTOP, cgroup quota and time namespaces belong to the
// fault-family agents in internal/perturber/faults, which implement the
// Injector interface declared here. Keeping the split at this line is what lets
// the scheduling logic be tested with a fake injector and no Docker daemon.
//
// # The three properties this package exists to hold
//
//  1. A target that resolves to ZERO nodes is an ERROR, never a silent no-op.
//     A fault that fires against nothing while the run reports it as injected is
//     how a tool reports PASS over a system it never perturbed. The same rule
//     already governs health probes (DECISIONS.md D-018) and it is enforced here
//     at compile time for static targets and at injection time for `role:`.
//
//  2. Every fault is WITHDRAWN and HEAL VERIFIES ZERO RESIDUAL (directive 4.3's
//     Critical Guarantee). Withdrawal is not best-effort: it runs on the normal
//     path, on cancellation, and again at HEAL, and a residue that survives all
//     three is a HARNESS ERROR (exit 2), not an oracle violation; the run could
//     not be conducted properly, so it has no verdict to give. A leaked iptables
//     rule or a SIGSTOPped container poisons every subsequent world, and on this
//     host it is triggered by success (OQ-004).
//
//  3. What was ACTUALLY injected is recorded as it happens. `role:leader`,
//     `minority(kv)` and `kv:*` bind to concrete nodes at injection time from
//     live cluster state, and Raft leadership moves at runtime, so a world
//     recording only the planned string does not reproduce (D-012, OQ-003). The
//     realized schedule is what makes invariant I2 true rather than aspirational
//     and is the input Phase 5 shrinks.
//
// # Determinism
//
// Every choice among equals (which node of a group a quorum target selects,
// what seed netem is given (D-025)) is drawn from the recorder's path-keyed
// PRNG, never from map iteration order and never from the order the topology
// happens to be written in. Each fault draws from its OWN sub-stream, keyed by a
// hash of its canonical string rather than by its position in the schedule, so
// deleting one fault during Phase 5 shrinking cannot perturb the randomness of
// any other (see recorder.Stream.Index).
//
// # What this package does not do
//
// No search, no mutation, no shrinking, no replay: those are Phases 4 and 5. The
// schedule handed to Compile is authored, not discovered.
package perturber
