// Package lock implements `.prothesis/lock`: the tamper-detection manifest that
// makes invariant I6 structural rather than advisory.
//
// # Why this exists
//
// In a loop where an agent optimises for a green gate, the cheapest path to
// green is to weaken the gate: delete an oracle, drop a fault kind from
// `perturber.allow`, shorten a profile budget, or turn down a search weight.
// None of those is a code change to the system under test, and none of them
// shows up as a failing test. The lock makes every one of them a digest
// movement, and a digest movement is exit 4; the one code the agent loop must
// never auto-resolve.
//
// # What is covered
//
// The manifest digest is SHA-256 over the canonical encoding (internal/recorder/cjson,
// the tree's single definition of canonical bytes) of:
//
//   - every file under `oracles.dir`, content-hashed, sorted by slash path
//   - `oracles.dir` and `oracles.builtin` as the user wrote them
//   - `perturber.allow`, `perturber.deny` and `perturber.budget`
//   - the whole `profiles:` block, which is a superset of the required
//     "every profile's budget and worlds"; it also closes the
//     `driver_profile` swap, a one-token edit that exchanges a 20,000-op gate
//     workload for a 500-op smoke one
//   - the whole top-level `search:` block, because `probe_budget_pct`,
//     `max_mcts_depth` and every utility weight are reachable without touching
//     a single oracle (DECISIONS.md D-028 item 8)
//
// # The USER-SUPPLIED config, with defaults NOT filled in (D-F, OQ-015.2)
//
// ProjectConfig takes raw prothesis.yaml BYTES, never a decoded *schema.Config.
// That is not a stylistic choice. If the digest covered the RESOLVED config,
// shipping a release that adjusts any compiled-in default would move the digest
// on every downstream project simultaneously, halting all of them on exit 4
// pending human sign-off. A key the user did not write contributes no entry at
// all, so no default can reach the preimage.
//
// The regression that pins this is TestOmittedBlockDiffersFromExplicitDefaults:
// a config that omits `search:` must digest DIFFERENTLY from the same config
// that writes `search:` out at exactly the compiled-in default values. If those
// two ever agree, defaults are being filled in.
//
// # THE HONEST LIMITATION
//
// The DIGEST does not cover the executable an oracle names, and never will.
//
// A definition under `oracles.dir` is covered byte for byte. The binary it
// invokes is not hashed into manifest_sha, and neither is any interpreter,
// shared library, container image or PATH lookup that binary resolves at run
// time. That exclusion is deliberate and permanent: hashing a binary into the
// digest would move it on every rebuild and every platform, so a routine
// `go build` would produce ORACLE_DRIFT and the correct response would become
// re-locking without reading the diff; training the exact reflex the lock
// exists to prevent.
//
// Since D-060 the lock's `executables` block records each resolved program's
// SHA-256 OUTSIDE the digest, `verify` and every run report a moved or missing
// one as a WARNING, and the verdict carries oracle_lock.executables_moved. That
// is the whole of the mitigation and it must not be described as more: a
// swapped checker still produces exit 0. What it can no longer do is produce a
// run that looks untouched, which is what OQ-057 measured and what made the gap
// exploitable rather than merely possible. Nothing here is signed. See
// DECISIONS.md D-060, and OQ-061 for the program-resolution hole that a
// fingerprint taken through the same resolver cannot see.
//
// Three narrower gaps, stated for the same reason:
//
//   - File modes are not covered. The executable bit does not survive the
//     Windows host this tool is built on, so hashing it would produce a
//     spurious exit 4 on every clone across platforms.
//   - Built-in oracle thresholds (internal/oracle.Options) are compiled in and
//     reachable from no config key. They are RECORDED in the lock file for
//     review but held OUTSIDE the digest, because including them would make
//     every `thesis` upgrade an exit-4 incident: the exact failure D-F
//     forbids. `verify` reports a moved fingerprint as a warning, never as
//     drift. See DECISIONS.md D-035.
//   - `driver.profiles` (clients / ops / mix) is not covered. Logged as
//     OQ-026.
//
// # Enforcement (D-I)
//
// Gate is the single enforcement point. `thesis run` calls it before booting
// anything, and Phase 6's `thesis gate` wraps the same function. An ABSENT lock
// does not block a run (a fresh project that has never locked must still be
// runnable, and refusing would be a spurious exit 4) but it is reported as
// "absent" in the verdict, never as "ok", and `thesis oracles verify` treats it
// as a failure to verify rather than as a pass.
package lock
