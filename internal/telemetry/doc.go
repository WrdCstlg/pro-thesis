// Package telemetry samples per-node observations during a run and materialises
// the document that `prothesis.oracle_input/v1`'s `telemetry_path` names.
//
// # Two artifacts, not one
//
// The collector writes BOTH:
//
//	telemetry.jsonl   one Sample per line, append-only, written as it is
//	                  observed. Tailable: a live consumer (the Phase 4 Saboteur's
//	                  DAMPEN/REINFORCE classifier) reads it while the world is
//	                  still running.
//	telemetry.json    a complete Document materialised at ASSERT. This is what
//	                  telemetry_path points at, because an external oracle is a
//	                  one-shot process that reads a whole file and exits, and
//	                  because the document carries the per-metric coverage census
//	                  a single sample cannot.
//
// The document is built by READING BACK the JSONL (MaterializeDocument), never
// from a second in-memory copy. One source of truth, so the two artifacts cannot
// disagree about what was observed.
//
// # Absent is not zero
//
// This is the single most important property of the format, and every type below
// is shaped by it.
//
// PRO-THESIS must work against an unmodified system under test. Goroutine count
// and queue depth are not observable from an arbitrary process; RSS is only
// observable when the container exposes a readable cgroup; a paused container
// answers nothing at all. So every metric is a POINTER: nil means "not
// observed", and a value means "observed, and this is it".
//
// Reporting an unobserved metric as 0 would be catastrophic rather than
// merely untidy. `resource_return_to_baseline` compares a post-QUIESCE reading
// against a baseline; if both are silently 0 the oracle returns PASS over a
// system whose memory it never measured, and a real regression ships. An oracle
// that needs an absent metric must return INCONCLUSIVE. Sample.Absent and
// Document.Coverage exist so it can say WHY, rather than reporting a bare
// "inconclusive" that reads like a flake.
//
// # RSS is anon, and nothing else
//
// MemoryMetrics.RSSBytes carries cgroup v2 `anon` (or cgroup v1 `rss`) and
// nothing else. It is deliberately NOT any of these, all of which were available
// and all of which are wrong:
//
//   - cgroup v2 `memory.current`: total charged memory INCLUDING PAGE CACHE.
//     Page cache grows under a write workload and is reclaimed lazily, so it
//     routinely fails to return to baseline after QUIESCE. Carried separately as
//     CurrentBytes, under its own name, so nobody mistakes it for RSS.
//   - docker stats' `MemUsage`: measured on this machine to be
//     `memory.current - inactive_file`. That removes INACTIVE page cache only;
//     the ACTIVE file cache a write workload just created is still in it.
//     Carried separately as DockerUsageBytes.
//   - the target's own reported figure. The kv fixture's `/status.rss_bytes` is
//     `runtime.MemStats.Sys`, which is address space obtained from the OS and
//     never comes back down. Carried verbatim under StatusMetrics, never
//     promoted into MemoryMetrics.RSSBytes. See OPEN_QUESTIONS.md OQ-020.
//
// If no true anon figure can be obtained, RSSBytes is nil and the reason is
// recorded in Sample.Absent. It is never substituted.
//
// # How metrics are obtained
//
// The Docker CLI is the only engine interface (DECISIONS.md D-016), which
// constrains what is reachable. Measured on the build machine:
//
//	docker inspect --format '{{.Id}} {{.RestartCount}} {{json .State}}' c1 c2 c3
//	  63 ms for three containers, one call. Gives running/paused/oom_killed/
//	  exit_code/pid/started_at — everything `no_crash` needs.
//
//	docker exec <c> sh -c '<batched cat of cgroup and proc files>'
//	  106 ms. Gives cgroup v2 `anon` (the only honest RSS), memory.current,
//	  cpu.stat, pids.current, /proc/1/status and the PID 1 fd count.
//
//	docker stats --no-stream --format json
//	  993 ms — twice the default sampling interval, and it cannot produce anon.
//	  It is therefore OFF by default and exists only as a degraded fallback for
//	  targets with no shell.
//
// The exec path requires a POSIX shell in the target image. A distroless target
// has none, and its cgroup metrics are then ABSENT with that reason recorded.
// The universal fix (a privileged helper container reading the target's cgroup
// from outside) is real infrastructure and is logged as OQ-063 rather than
// smuggled into Phase 1.
//
// A PAUSED container is never exec'd: `docker exec` against one is refused by
// the daemon. The collector reads Paused from inspect first and records the
// cgroup metrics as absent with reason "container is paused", which is a true
// statement about the world rather than a collection failure.
//
// # Encoding
//
// encoding/json with HTML escaping off, NOT internal/recorder/cjson. Three
// reasons, each decisive on its own: the format must carry the target's /status
// document VERBATIM as a json.RawMessage (cjson rejects json.Marshaler
// implementations by design); telemetry is not part of world_hash, so the
// no-bare-floats rule that protects hashed artifacts buys nothing here; and
// struct declaration order already gives deterministic bytes.
//
// Numbers are nonetheless integers throughout. Rates are integer milli-percent
// and durations are integer microseconds, so a telemetry file diffed across two
// Go toolchains cannot differ in float formatting.
//
// # Scope
//
// Phase 1. This package collects and serialises. It does not classify (Phase 4's
// DAMPEN/REINFORCE), does not evaluate (Phase 1's oracles live elsewhere), and
// injects nothing (Phase 2).
package telemetry
