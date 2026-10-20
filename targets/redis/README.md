# `targets/redis`: the fourth target

Upstream Redis 8.10.2, one primary and two replicas with asynchronous
replication, and one Sentinel per node (quorum 2) that promotes a replica when
the primary is unreachable. The unmodified image is pinned by digest and there
is no planted bug. Redis documents that writes it acknowledged can be lost when
a primary cut off from its replicas goes on accepting them and is then demoted;
the `failover` profile partitions the primary to look for exactly that.

Status: **onboarded, no verdict arm run.** One `smoke` world (no faults) has
run; no `failover` world has. Nothing below is a claim about Redis.

## Licence

Redis 8 is distributed under a choice of RSALv2, SSPLv1 or AGPLv3, at the
user's option. This target runs the unmodified upstream image as an in-house
test target. A published result must state the version (8.10.2) and which of
the three licences it was used under; that choice is not recorded here.

## What is here

| Path | What it is |
|---|---|
| `docker-compose.yaml` | Three nodes `redis-n1..n3` from `redis:8.10.2@sha256:2c2dff79...6bfd6`, a Sentinel and a health shim in each node's network namespace, named data volumes, Sentinel timings stated. Nothing harness-specific inside the containers. |
| `prothesis.yaml` | The project. Two driver profiles: `smoke` and `failover`, both half `write`, half `read`. |
| `cmd/redisload` | The workload driver. Go standard library only, RESP2 by hand. Writes and reads go to the primary the Sentinels name. Checks every server's version and the Sentinels' topology at start, and records the topology on the history's first record. |
| `cmd/redisrole` | The `harness.role_probe`: every node's `INFO replication` and every Sentinel's primary, each mapped to a node id by runid. Refuses on any disagreement. |
| `cmd/redissteady` | The steady-state probe: one primary, two replicas online, three Sentinels agreeing and knowing each other. |
| `cmd/redishealth` | The health shim: HTTP 200 when the node answers `PING` and `INFO replication`, 503 otherwise. Runs inside Linux, one per node. |
| `internal/redisapi` | The RESP2 reader and writer, `INFO` parsing, and the Sentinel questions every program shares. |
| `internal/respfake` | A scripted RESP2 server for the tests. |
| `.prothesis/oracles/linearizable.kv.yaml` | The checker definition the etcd and rqlite targets use, byte for byte (its comments still name etcd, on purpose: anyone can check the bytes by hash). |
| `.prothesis/lock` | The committed oracle lock. |

## Topology

| Service | Image | Network | Role |
|---|---|---|---|
| `redis-n1` | pinned `redis` | `rd` (owns its namespace), publishes 16379, 16479, 16581 | primary at boot; harness node |
| `redis-n2` | pinned `redis` | `rd`, publishes 16380, 16480, 16582 | replica of `redis-n1` at boot; harness node |
| `redis-n3` | pinned `redis` | `rd`, publishes 16381, 16481, 16583 | replica of `redis-n1` at boot; harness node |
| `sentinel-nX` | pinned `redis`, `redis-server <conf> --sentinel` | `network_mode: service:redis-nX` | Sentinel co-located with node X |
| `health-nX` | pinned `redis`, entrypoint `bin/redishealth` (bind-mounted read-only) | `network_mode: service:redis-nX` | HTTP health for node X |

Sentinel configuration, identical on all three: `sentinel monitor mymaster
redis-n1 6379 2` (quorum 2) with `sentinel resolve-hostnames yes`,
`down-after-milliseconds 1000`, `failover-timeout 3000`, `parallel-syncs 1`,
`protected-mode no`. Sentinel needs a writable config file, which it rewrites
with what it learns: a shell from the image writes these lines into a tmpfs and
execs the image's own entrypoint as `redis-server <conf> --sentinel`, the form
that entrypoint recognises and whose config file it hands to the `redis` user.
A tmpfs means every world's Sentinels start from these lines and nothing else.

Redis runs with its defaults plus `--port 6379 --protected-mode no`.
`min-replicas-to-write` is 0, the default, which is what lets a partitioned
primary go on accepting writes.

**Trade-off of co-location.** A `net.partition` of a node is iptables rules in
that node's network namespace dropping every other node's address
(`internal/perturber/faults/iptables.go`). Because the node's Sentinel and
health shim share the namespace, the partition cuts the Sentinel off too, as a
co-located deployment is cut: the partitioned primary keeps its own Sentinel,
which goes on naming it, and the two Sentinels on the majority side make the
quorum that fails over. The shim reaches its node over 127.0.0.1, which no
partition cuts, so it reports whether the node's process answers, not whether
the cluster can reach it. A `proc.kill` or restart of a node container takes
that namespace with it; the Sentinel and shim containers are then attached to
a namespace that no longer exists. That interaction has not been run.

**Topology in every world's evidence.** The primary at start, the quorum and
the two timings are restated in `driver.cmd`, which the lock covers. At start
the driver asks every Sentinel and refuses (exit non-zero, no history) unless
each names that primary and reports that quorum and those timings. It then
records what it observed as `meta.topology` on the first record of
`history.jsonl`: master name, configured and observed primary, quorum,
timings, the placement, every node's address, role, run_id and version, and
every Sentinel's address, version, named primary (with the container address
it reported), config epoch, quorum, timings, replica count and count of other
Sentinels.

## Mapping Sentinel's addresses to node ids

Sentinel names the primary by its container address (172.x), which the host
cannot route to and which no harness node id carries. The mapping is by
**run_id**: `SENTINEL master mymaster` reports the primary's `runid` beside its
`ip` and `port`, and every node reports its own `run_id` in `INFO server`,
asked through its published port. Redis draws a run_id at random at every
process start, so no two nodes share one and a restarted node gets a new one
(the driver then asks the nodes again). A Sentinel answer whose runid is empty
or matches no node is refused, never matched by address. The driver also asks
`SENTINEL get-master-addr-by-name` first, the call a client uses, and refuses
an answer whose address differs from `SENTINEL master`'s. No `announce-ip` is
configured anywhere, so replication and Sentinel traffic stay on the compose
network, where a partition reaches them.

## Build and run

From the repository root, then from this directory. Run every `thesis`
command from this directory: launched from elsewhere, teardown can leave the
world's volumes behind (OQ-087). `bin/redishealth` must exist before any
`compose up`: Docker creates a missing bind-mount source as a directory.

```powershell
pwsh -File scripts/build.ps1                                   # bin/thesis.exe
go build -trimpath -buildvcs=false -o targets/redis/bin/redisload.exe   ./targets/redis/cmd/redisload
go build -trimpath -buildvcs=false -o targets/redis/bin/redisrole.exe   ./targets/redis/cmd/redisrole
go build -trimpath -buildvcs=false -o targets/redis/bin/redissteady.exe ./targets/redis/cmd/redissteady
$env:GOOS='linux'; $env:CGO_ENABLED='0'
go build -trimpath -buildvcs=false -o targets/redis/bin/redishealth ./targets/redis/cmd/redishealth
$env:GOOS=''; $env:CGO_ENABLED=''
# the checker: the fixture's locked build, copied unchanged
Copy-Item testdata/kvfixture/bin/linearizable-kv.exe targets/redis/bin/
docker pull redis:8.10.2

cd targets/redis
..\..\bin\thesis.exe oracles verify
..\..\bin\thesis.exe doctor
..\..\bin\thesis.exe run --profile smoke      # 1 world, no faults
```

## The driver

- **Where the workload comes from.** `driver.cmd` (lock-covered) and the plan
  the harness writes from `driver.profiles` (lock-covered for every profile a
  run profile names, D-090). The node and Sentinel addresses are on
  `driver.cmd` too. The driver reads no environment variable at all. A missing
  plan, a zero client or op count, an empty mix and an op other than `write`
  or `read` are refused before the history file exists; so is an
  operation-mode plan, so op shrinking against this target is not available.
- **Finding the primary.** As a Sentinel-aware client does: ask the Sentinels
  in order, starting from the last one that answered, and take the first
  answer that maps to a node. Asked again after any operation that was not ok,
  at most every 200 ms. While the partitioned primary's own Sentinel answers
  first, the driver goes on believing in that primary; that is the client
  behaviour the target models, recorded as it is.
- **ok, fail, info.** `+OK` to `SET` is ok, and nothing else is. `-READONLY`
  is fail: a replica refuses a write before applying anything. A command that
  provably never left the client (connection refused, no such host) is fail.
  Every other error reply, a malformed reply, a timeout and a connection
  closed after the command was sent are info. `GET`'s null bulk string `$-1`
  is the absent key, recorded as value `null`.
- **Evidence on every record.** `meta.target` (the node the command was sent
  to), `meta.primary` (the primary the driver believed in), `meta.sentinel`
  (whose Sentinel said so), `meta.reply` (the reply, rendered) and
  `meta.latency_us`.
- **The version.** Every node's and every Sentinel's `redis_version` must equal
  `--expect-version` (`8.10.2`) at start, or the run ends non-zero before the
  history exists.

## Contract gaps

Each is a place the harness contract cannot express what Redis needs. Nothing
outside this directory was changed for any of them.

1. **The health probe is HTTP only.** `harness.health` is a URL template
   polled with an HTTP GET (`pkg/schema/config.go`, `HealthProbe`;
   `internal/harness/health.go`, `pollHTTP`). Redis speaks RESP. Workaround:
   `cmd/redishealth`, one per node, in the node's namespace.
2. **A node declares one port, and it serves two masters.** `harness.nodes[].port`
   is both what a health probe's `{port}` resolves to
   (`internal/harness/health.go`, `WaitHealthy`) and what `PROTHESIS_TARGETS`
   and `PROTHESIS_PORT_<NODE>` export to the driver and probes
   (`internal/control/runner.go`, `topologyTargets`;
   `internal/control/steady.go`, `SteadyStateEnv`). Here it must be the health
   shim's port, so the harness tells the driver and probes nothing usable.
   Workaround: the RESP and Sentinel addresses are literal on `driver.cmd`,
   `role_probe` and `steady_state`. Consequences: `PROTHESIS_PROBE_HOST` does
   not reach them (127.0.0.1 is written in), and a parallel lane can remap only
   the health port (`port_env`), not the RESP or Sentinel ports, so parallel
   worlds against this target would collide on those ports at `compose up`.
3. **The world file cannot name the topology.** `world.thesis` has a
   `topology_variant` field, but the harness always writes `"default"`
   (`internal/control/runner.go`, `schema.NewWorld(worldSeed, "default", ...)`).
   Workaround: the topology rides on `driver.cmd` (lock-covered, checked at
   start) and on `meta.topology` in the history, described above.
4. **Services that are not nodes are partly invisible.** The overlay labels,
   `sut.images`, log collection and `no_crash` all iterate the declared nodes
   only (`internal/harness/compose.go`, `writeOverlay`;
   `internal/control/runner.go`, `imageResolver.Resolve(ctx, topology.Nodes, ...)`;
   `internal/control/logs.go`, `Collect`). So the Sentinels' and shims' image
   is not recorded per world (it is the same pinned reference, but that is
   read from this file, not measured), the Sentinels' logs (`+sdown`, `+odown`,
   `+switch-master`) are not captured into the bundle, and a crashed Sentinel
   is not a `no_crash` violation. Teardown does cover them: the label sweep
   matches the compose project label every container carries, and binding
   looks up only declared services, so the extra services do not confuse
   either. Declaring the Sentinels as nodes was not attempted: a container in
   another container's namespace has no address of its own, and how the
   partition resolver treats such a peer has not been measured.
5. **The steady-state probe is outside the lock** (OQ-094), so its addresses
   could be edited without moving the lock; a passing probe leaves no trace in
   the bundle (OQ-096).
6. **Two primaries cannot be ranked by the role probe's contract** (OQ-095).
   Sentinel does carry a config epoch, but a node's own `INFO` does not, so the
   role probe refuses every disagreement rather than rank claims.

## What is claimed

Nothing about Redis yet. What was measured during onboarding: the image digest
pulled matches the pin, `redis-server --version` in it reports `v=8.10.2`, the
tests fail when their logic is removed, and one fault-free world reached ASSERT
and wrote its bundle.

The tests use reply shapes from the RESP specification and Redis's command
and Sentinel documentation, not from a live server.

## What is not claimed

- **The image under test is named in `docker-compose.yaml`, which the lock does
  not cover** (OQ-093). Pinning by digest means compose cannot silently pull a
  different image under the same reference, but the file can be edited without
  moving the lock. The harness records the image the three nodes ran in every
  world (`sut.images`, D-070); a world counts only after that identity is
  compared with
  `sha256:2c2dff791878316e3b083188be0d578423b24813cdda5de0e80ea4f7b3e6bfd6`.
- Single-key register operations only: no transactions, pipelines, scripts,
  cluster mode or client-side caching.
- The driver, probe, shim and checker binaries carry no build stamp; the lock
  records the SHA-256 of the driver and the checker, and nothing records the
  probes' or the shim's (OQ-094).
- Two built-in oracles are left out, as on etcd and rqlite:
  `no_unbounded_queue` (it reads the fixture's `/status` document) and
  `resource_return_to_baseline`. Neither has been measured against Redis.
