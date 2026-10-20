# Onboarding log (axis A7)

The honest cost of bringing each target to the P1 definition of done: `thesis doctor` green, one
world reaching ASSERT with a complete bundle (INCONCLUSIVE or better), no verdict arm run. Time
includes everything; operator interventions are listed.

## rqlite 10.5.3

| | |
|---|---|
| Image | `rqlite/rqlite:10.5.3@sha256:de385382ef6d93a4b4b831eac4d419cfa35fdb1515621a4ad8ba8b5a5beeb6b5` (MIT) |
| Design before the build | about ten minutes, not timed precisely |
| Build to done | 2026-10-19 23:18:28 to 23:48:16 -04:00: **29 min 48 s** |
| Review | to 23:50:38 -04:00 (tests re-run, classification logic read, gaps verified) |
| Configuration | `docker-compose.yaml` 120 lines (72 non-comment), `prothesis.yaml` 91 (44), oracle definition 35 (6, byte-identical to etcd's) |
| Go | driver 967 lines, role probe 82, steady-state probe 85, shared package 279; tests 775 (driver) and 167 (role probe) |
| Tests | 36 pass, 0 fail (`scripts/run-tests.ps1 -Match targets/rqlite`); every test failed before its code existed and again under a targeted mutation |
| Lock | manifest `sha256:3395e4cf...a261`, 60 configuration values; fingerprints the driver and the checker |
| Smoke world | `r_2026_10_20_95ab`: PASS, exit 0; 500 operations (241 determinate reads, 259 writes), every read at `linearizable`, every response `v10.5.3`; image identity equal to the pin; 0 volumes, containers or networks left |
| Not exercised live | the write redirect path (the leader never changed in the smoke world) and the role probe |

Contract gaps found: OQ-091 (driver profile fields), OQ-092 (no node ids in `PROTHESIS_TARGETS`),
OQ-093 (image outside the lock), OQ-094 (probe programs not fingerprinted), OQ-095 (term
tie-break), OQ-096 (steady-state success not recorded), OQ-088 (unstamped target programs), and
OQ-087 (worked around by launching from the project directory). Each is also noted in
`docs/TARGETS.md` section 11.

Operator interventions: the worktree had no harness binary, so `scripts/build.ps1` ran first and
overwrote the fixture's locked checker and driver, which were restored from the locked bytes and
verified; three one-off `docker run` calls to read `rqlited -version` and `-h`; one retried commit
(a quoting error; nothing had been committed). No restarts, no manual cleanup, no retried worlds.

## Consul 2.0.4

| | |
|---|---|
| Image | `hashicorp/consul:2.0.4@sha256:1c59f007df8ea1c21b2558dc73ef8d99047bc895d9e52e635d1e745e35fe9a54` (BUSL-1.1; used in-house as a test target, nothing redistributed) |
| Design before the build | not timed |
| Build to done | 2026-10-19 23:57:56 to 2026-10-20 00:21:08 -04:00: **23 min 12 s** |
| Review | tests, lock, doctor, image identity and leftovers re-run independently; all reproduced |
| Configuration | `docker-compose.yaml` 133 lines (66 non-comment), `prothesis.yaml` 94 (42), oracle definition 35 (6, byte-identical to etcd's and rqlite's) |
| Go | driver 881 lines, role probe 83, steady-state probe 102, shared package 255; tests 718 (driver) and 175 (role probe) |
| Tests | 42 pass, 0 fail (`scripts/run-tests.ps1 -Match targets/consul`); every test failed against a stub and again under a targeted mutation (11 mutations) |
| Lock | manifest `sha256:b15b1649...074a`, 52 configuration values; fingerprints the driver and the checker |
| Smoke world | `r_2026_10_20_9237`: PASS, exit 0; 500 operations (227 reads, 273 writes, all ok), every read at `consistent`; image identity equal to the pin (`eval/tools/check-images.ps1`, exit 0); 0 volumes, containers or networks left |
| Not exercised live | the default and stale read modes, a leader change, the "No cluster leader" refusal, the health probe's leaderless answer, and a role-probe disagreement |

Design facts that decide later verdicts:

- **Writes go to any node.** A KV write sent to a follower is forwarded to the leader over RPC, and
  Consul re-sends a forwarded write only on errors raised before the Raft apply ("No cluster
  leader", the chunking resubmit, the rate limiter), so a forwarded write cannot be applied twice.
  Read from the v2.0.4 source (`agent/consul/rpc.go` `canRetry` and `forwardRequestToLeader`,
  `agent/consul/kvs_endpoint.go`); not exercised live.
- **The server version is checked once per node at start.** Consul sends no version on KV answers,
  so a node replaced during a run would not be detected.
- **The health probe is `/v1/operator/raft/configuration` without `?stale`**: it is forwarded to
  the leader, so it answers 200 only when one is reachable. `/v1/status/leader` was rejected
  because it answers 200 with an empty body when there is no leader.
- **`performance.raft_multiplier = 1`** is set in the compose file, which no lock covers (OQ-097).

Contract gaps: none new to the harness contract; OQ-091, OQ-092, OQ-094, OQ-095 and OQ-096 applied
as for rqlite, OQ-093 checked with the image tool, and OQ-097 recorded from this target.

Operator interventions: one image pull; two throwaway `docker run` calls (`consul version`, the
entrypoint's licence header). One commit message was wrong and was amended twice before anything
was pushed; the tree did not change. No restarts, no manual cleanup, no retried worlds.

## Redis 8.10.2 with Sentinel

| | |
|---|---|
| Image | `redis:8.10.2@sha256:2c2dff791878316e3b083188be0d578423b24813cdda5de0e80ea4f7b3e6bfd6` (RSALv2, SSPLv1 or AGPLv3 at the user's option; a published result names the version and the licence chosen) |
| Topology | three nodes (`redis-n1` primary at boot, two replicas); one Sentinel and one health shim per node, each in that node's network namespace (`network_mode: service:redis-nX`); quorum 2, `down-after-milliseconds` 1000, `failover-timeout` 3000; `min-replicas-to-write` 0 (the default) |
| Design before the build | not timed |
| Build to done | 2026-10-19 23:57:56 to 2026-10-20 00:24:13 -04:00: **26 min 17 s** |
| Review | tests, gofmt, vet, lock, doctor, image identity, leftovers and the topology record re-run independently; all reproduced |
| Configuration | `docker-compose.yaml` 206 lines (124 non-comment, nine services), `prothesis.yaml` 88 (38), oracle definition 35 (6, byte-identical); compose SHA-256 `f4143df1...09f1` |
| Go | driver 988 lines, role probe 94, steady-state probe 125, health shim 96, RESP client 610, test fake 162; tests 726 (driver), 181 (role probe), 103 (shim) |
| Tests | 43 pass, 0 fail (`scripts/run-tests.ps1 -Match targets/redis`); every test failed against a stub and again under a targeted mutation, including one for the topology record |
| Lock | manifest `sha256:273d78bf...00db`, 36 configuration values; fingerprints the driver and the checker |
| Smoke world | `r_2026_10_20_c22a`: PASS, exit 0; 500 operations (263 reads, 237 writes, all ok), every one served by and believed primary `redis-n1`; topology on the first record; image identity equal to the pin (`eval/tools/check-images.ps1`, exit 0); 0 volumes, containers or networks left |
| Not exercised live | a failover, a `-READONLY` refusal, the role probe's refusal of two primaries, and whether a 4.5 s partition completes a failover with these timings |

Design facts that decide later verdicts:

- **The driver follows the Sentinels it can reach.** It asks the first Sentinel that answers,
  starting from the last one that did, and maps Sentinel's container address to a node by `run_id`
  (`SENTINEL master` against each node's `INFO server`). Under a partition of the primary, the
  primary's own Sentinel is partitioned with it and keeps naming it, so the driver keeps writing to
  the old primary while the majority side fails over. That is the client placement in which Redis
  documents acknowledged-write loss.
- **The topology is checked, not only recorded.** The expected primary, quorum and timings are
  arguments of the lock-covered `driver.cmd`, and the driver refuses a world whose Sentinels report
  otherwise. This is the one place a compose setting is held to the lock (OQ-097).
- **Health goes through a shim** (OQ-098), and **the node port is the shim's**, so RESP and Sentinel
  addresses are written literally into the driver and probes (OQ-099).

Contract gaps found: OQ-098 (HTTP-only health), OQ-099 (one port per node; blocks parallel lanes for
this target), OQ-100 (`topology_variant` is always "default"), OQ-101 (Sentinel and shim images,
logs and crashes are outside the bundle, so a FAIL cannot yet show when the failover happened); and
OQ-093 to OQ-097 as for the other targets.

Operator interventions: two image pulls; two throwaway `docker run` calls (`redis-server --version`,
the image entrypoint); one manual bring-up of the topology under a separate compose project before
the smoke world (probes, a 200-operation driver run, then `down --volumes`, leaving 0 resources). No
restarts, no retried worlds. One commit message named the wrong target count and was corrected before
anything was pushed; the tree did not change.
