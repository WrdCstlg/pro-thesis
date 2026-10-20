# `targets/consul`: the fourth target

Upstream Consul 2.0.4, its key-value store over the plain HTTP API, three
voting servers, the unmodified image pinned by digest, no planted bug. One
cluster serves three read paths, selected by one query parameter: `?consistent`
(documented as linearizable: the leader confirms its leadership with a quorum
before answering), the default mode (the leader answers from its own state
without that round, so a leader that has just been cut off can answer stale),
and `?stale` (any server answers from its own state). The intended shape is a
PASS arm and a FAIL arm on the same cluster under the same leader partition,
as on etcd and rqlite.

Status: **onboarded, no verdict arm run.** One `smoke` world (no faults) has
run; no `default`, `stale` or `consistent` world has. Nothing below is a claim
about Consul.

## Licence

Consul is distributed by HashiCorp (an IBM company) under the **Business
Source License 1.1** (BUSL-1.1), not an open-source licence; the image's own
`docker-entrypoint.sh` carries `SPDX-License-Identifier: BUSL-1.1`. It is used
here as an in-house test target. Nothing in this repository contains or
redistributes Consul binaries or images: `docker-compose.yaml` pulls the
upstream image by digest. Any published result from this target must state the
licence and the version, `hashicorp/consul:2.0.4`
(`sha256:1c59f007df8ea1c21b2558dc73ef8d99047bc895d9e52e635d1e745e35fe9a54`).

## What is here

| Path | What it is |
|---|---|
| `docker-compose.yaml` | Three servers from `hashicorp/consul:2.0.4@sha256:1c59f007...9a54`, HTTP API published to 127.0.0.1 (host ports 15501..15503 by default), RPC and Serf not published, named volumes, `performance.raft_multiplier = 1` stated explicitly. Nothing harness-specific inside the containers. |
| `prothesis.yaml` | The project. Four driver profiles that differ only in the read entry of the mix: `smoke` and `consistent` (`read_consistent`), `default` (`read_default`), `stale` (`read_stale`). |
| `cmd/consulload` | The workload driver. Go standard library only. Writes to any node, reads in the mode the mix names, records that mode on every read, and checks every node's version at start. |
| `cmd/consulrole` | The `harness.role_probe`: every node's own `/v1/operator/raft/configuration?stale`, the leader's node name mapped to a node id. Refuses when the nodes disagree. |
| `cmd/consulsteady` | The steady-state probe: every node answers, all name the same leader, and each lists the three servers as voters. |
| `internal/consulapi` | The slice of Consul's HTTP surface both probes share. |
| `.prothesis/oracles/linearizable.kv.yaml` | The checker definition the etcd and rqlite targets use, byte for byte (its comments still name etcd, on purpose: the bytes are the same, which anyone can check by hash). |
| `.prothesis/lock` | The committed oracle lock. |

## Build and run

From the repository root, then from this directory. Run every `thesis`
command from this directory: launched from elsewhere, teardown can leave the
world's volumes behind (OQ-087).

```powershell
pwsh -File scripts/build.ps1                                   # bin/thesis.exe
go build -trimpath -buildvcs=false -o targets/consul/bin/consulload.exe   ./targets/consul/cmd/consulload
go build -trimpath -buildvcs=false -o targets/consul/bin/consulrole.exe   ./targets/consul/cmd/consulrole
go build -trimpath -buildvcs=false -o targets/consul/bin/consulsteady.exe ./targets/consul/cmd/consulsteady
# the checker: the fixture's locked build, copied unchanged
Copy-Item testdata/kvfixture/bin/linearizable-kv.exe targets/consul/bin/
docker pull hashicorp/consul:2.0.4

cd targets/consul
..\..\bin\thesis.exe oracles verify
..\..\bin\thesis.exe doctor
..\..\bin\thesis.exe run --profile smoke      # 1 world, no faults
```

The lock records the SHA-256 of each program it names (the driver and the
checker). A rebuild that changes the bytes is reported as a warning, not as
drift (D-060).

## The cluster

- **Three servers**, `agent -server -bootstrap-expect=3`, each `-retry-join`ing
  the other two by service name, `-node` set to the service name (which is the
  harness node id), `-client=0.0.0.0`, data on a named volume at the image's
  `/consul/data`. The bind address is eth0's, through the entrypoint's
  `CONSUL_BIND_INTERFACE`; the extra configuration goes through its
  `CONSUL_LOCAL_CONFIG`.
- **Production Raft timing.** `performance.raft_multiplier = 1`: heartbeat
  1 s, election 1 s, leader lease 500 ms. Consul's default multiplier is 5,
  its development setting. In the smoke world the first election started
  1.58 s after the servers entered the follower state (node logs), which fits
  a 1 s base and not a 5 s one.
- `disable_update_check`: a world makes no outbound call to HashiCorp.
- **PID 1 is the image's `dumb-init`**, not consul. That does not weaken the
  process faults: `proc.pause` is `docker pause`, a cgroup freeze of every
  process in the container, and `proc.kill` signals PID 1, which forwards the
  signal or, for SIGKILL, ends the namespace.

## The health probe

`harness.health` asks `GET /v1/operator/raft/configuration` in the default
mode. Consul forwards that request to the leader over RPC, so it answers 200
only when the node is the leader or knows one it can reach; with no leader the
node holds the request for up to `rpc_hold_timeout` and answers 500 "No cluster
leader" (`agent/consul/rpc.go` `forwardRequestToLeader`). `/v1/status/leader`
was rejected: it answers 200 with `""` when there is no leader. The compose
healthcheck asks the same endpoint from inside the container.

## The driver

- **Where the workload comes from.** `driver.cmd` (lock-covered) and the plan
  the harness writes from `driver.profiles` (lock-covered for every profile a
  run profile names, D-090). The read mode is part of the mix entry's name
  (OQ-091). The driver reads no workload from the environment: the one variable
  it reads is `PROTHESIS_TARGETS`, the topology. A missing plan, a zero client
  or op count, an empty mix and an unknown read mode are refused before the
  history file exists. An operation-mode plan is refused too: this driver does
  not replay traces, so op shrinking against this target is not available.
- **Single-key register.** `PUT /v1/kv/k/<i>` with the decimal value as the
  body; `GET /v1/kv/k/<i>` with no parameter, `?stale` or `?consistent`. A 404
  is the absent key (`null`) only when it carries `X-Consul-Index` and no body,
  as KVSGet's not-found does; the value is base64 in the JSON array and must
  decode to a decimal integer under the requested key.
- **Writes go to any node.** A follower forwards a KV write to the leader over
  RPC, and in Consul 2.0.4 that forwarding cannot apply a write twice: the
  forwarding loop re-sends a write only on errors returned before Raft apply,
  and nothing below it retries (see "The forwarding question" below). So writes
  are not routed to a believed leader, unlike rqlite's.
- **ok, fail, info.** `ok` is HTTP 200 with the body `true`. `fail` needs
  positive evidence that nothing happened: the request was never sent, or the
  write was answered 500 "No cluster leader", which Consul produces only before
  the KVS.Apply handler runs. Everything else is `info`, including HTTP 200
  with `false`, with a JSON string, or empty, and a 500 "raft apply failed:
  leadership lost while committing log", whose entry may still commit.
- **The version.** Every node's `/v1/agent/self` `Config.Version` must equal
  `--expect-version` (`2.0.4`) before the history file exists; otherwise the
  run ends non-zero. Consul sends no version header on KV answers, so unlike
  the rqlite driver this one cannot detect a node replaced mid-run.
- **The read mode** sent is in every read record's `meta.read_mode`, with the
  answer's `X-Consul-KnownLeader` (`known_leader`), `X-Consul-LastContact`
  (`last_contact`) and `X-Consul-Index` (`index`). `X-Consul-Effective-Consistency`
  is copied when sent (`consistency`); in the smoke world no KV answer carried
  it.

## The forwarding question

Read from Consul's source at tag v2.0.4:

- `agent/consul/kvs_endpoint.go:103-106`: `KVS.Apply` calls `ForwardRPC`
  first; on a follower the request goes to the leader and the follower returns
  whatever the leader returned.
- `agent/consul/rpc.go:788-839` (`forwardRequestToLeader`): one forward per
  loop iteration; the loop repeats only when `canRetry` says so.
- `agent/consul/rpc.go:633-664` (`canRetry`) with the list at 817-822: a
  write is retried on "No cluster leader" (`structs.IsErrNoLeader`, produced
  by `getLeader` 855-858 or the leave check 795-799, before any handler runs),
  on `ErrChunkingResubmit` (only for values over `raft.SuggestedMaxDataSize`,
  `rpc.go:1000-1004`; this driver's values are a few bytes) and on
  `rate.ErrRetryLater` (the rate limiter, before the handler). A stream error
  (EOF) is retried for reads only (`rpc.go:663`). "raft apply failed" is not
  retried.
- `agent/pool/pool.go:627-666`: the connection pool makes one call and returns
  its error. `agent/agent.go:1706-1727` (`Agent.RPC`) and
  `agent/consul/server.go:1634-1658` (`Server.RPC`) do not retry.

So a write sent to a follower reaches Raft at most once through forwarding.
This is a reading of the source, not a measurement.

## What is claimed

Nothing about Consul yet. What was measured during onboarding: the image
digest pulled matches the pin and `consul version` in it prints `Consul
v2.0.4`; the tests below fail when their logic is removed; and one fault-free
world reached ASSERT, wrote its bundle and recorded the pinned image.

The driver's and the role probe's tests use response shapes read from Consul
v2.0.4's source (`agent/kvs_endpoint.go`, `agent/http.go`,
`agent/agent_endpoint.go`, `agent/operator_endpoint.go`,
`agent/consul/operator_raft_endpoint.go`, `agent/structs/operator.go`,
`agent/structs/structs.go`, `agent/structs/errors.go`), not from a live node.

## What is not claimed

- **The image under test is named in `docker-compose.yaml`, which the lock does
  not cover.** Pinning by digest means compose cannot silently pull a different
  image under the same reference, but the file itself can be edited without
  moving the lock. The harness records the image that actually ran in every
  world (`sut.images`, D-070); a world counts only after that identity is
  compared with
  `sha256:1c59f007df8ea1c21b2558dc73ef8d99047bc895d9e52e635d1e745e35fe9a54`.
- Single-key register operations only: no CAS, sessions, locks, transactions
  (`/v1/txn`), blocking queries, `?cached` or `max_stale`, and no client
  agents: the driver talks to the servers directly.
- The raft configuration reports no Raft term, so the role probe cannot rank
  two leadership claims; it refuses instead, and a `role:` fault then fails to
  bind (fail-closed) rather than land on a guess.
- Two built-in oracles are left out, as on etcd and rqlite:
  `no_unbounded_queue` (it reads the fixture's `/status` document) and
  `resource_return_to_baseline`. Neither has been measured against Consul.
- The driver, probe and checker binaries carry no build stamp; their identity
  is the SHA-256 the lock records. The role and steady probes are not
  fingerprinted by the lock (OQ-094).
