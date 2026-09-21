// Package control is the PRO-THESIS control plane: the lifecycle state machine,
// the budget, verdict assembly, and the mapping from an outcome to a normative
// exit code.
//
// # What lives here
//
//	the eight-phase lifecycle    BOOT SEED DRIVE PERTURB HEAL QUIESCE ASSERT TEARDOWN
//	the budget                   invariant I7: wall clock and world count
//	the verdict                  prothesis.verdict/v1 assembly
//	exit codes                   directive 4.7
//
// # What does NOT live here
//
// Fault injection (Phase 2), the external oracle process runner and the lock
// manifest (Phase 3), coverage and search (Phase 4), shrinking and replay
// (Phase 5), and the gate/MCP/watch surfaces (Phase 6). This package names the
// seams those phases plug into; it implements none of them.
//
// # Why the subsystem seams are interfaces declared HERE
//
// Driver, Telemetry, OracleEngine, SteadyStateProber and LogCollector are
// declared in this package rather than imported from internal/driver,
// internal/telemetry and internal/oracle. Two reasons, and the second is the
// load-bearing one:
//
//  1. The consumer declares the interface it needs, so the seam is exactly as
//     wide as the lifecycle actually uses and no wider.
//  2. The lifecycle's central guarantee (HEAL and TEARDOWN run on EVERY path)
//     is only testable with substitutable subsystems. A test must be able to
//     make the driver fail, make an oracle report a violation, and cancel the
//     context mid-flight, then assert that the topology was still torn down. A
//     control plane wired to concrete subsystems cannot prove that property, and
//     a property that cannot be proved is a property that quietly stops holding.
//
// The compose harness is the exception: internal/harness.Backend is already an
// interface, so it is used directly.
//
// # The one guarantee this package exists to make
//
// HEAL and TEARDOWN run on every path: success, oracle violation, budget
// expiry, harness error, and Ctrl-C. This is not tidiness.
//
//	Skipping HEAL leaves live faults — iptables rules and SIGSTOPped containers —
//	that poison every subsequent world, and it is triggered by SUCCESS, because
//	the fastest way out of a world is to find a violation in it (OQ-004).
//
//	Skipping TEARDOWN leaks a Docker bridge network. This host has a hard ceiling
//	of 30 with 24 free, and a leaked project holds one permanently, within the
//	session and beyond it. A run that leaks one network per world exhausts the
//	pool inside a couple of dozen worlds and then fails in a way that looks like
//	a Docker bug (DECISIONS.md D-017, D-022).
//
// The mechanism is two contexts. Everything up to ASSERT runs under the
// caller's cancellable context; HEAL and TEARDOWN run under a DETACHED context
// with its own timeout, so a cancelled parent cannot kill the `docker compose
// down` that has to happen. See worldRun.run.
package control
