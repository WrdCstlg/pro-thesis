# `testdata/kvfixture`: the 3-node Raft KV store with a known bug

This is the system PRO-THESIS is tested against. Its entire value is that it has
**exactly one** deliberate defect, in a file we own, on a line anyone can read.
Without a system with a known bug there is no way to tell whether PRO-THESIS
works, so if PRO-THESIS ever reports a safety violation that this document does
not explain, that is a bug in the fixture and it must be fixed, not celebrated.

Nested Go module (`prothesis.dev/kvfixture`), **zero third-party dependencies**,
no `go.sum`. It shares a *format* with the harness root module, never
code; the module boundary makes that structural rather than a matter of
discipline.

---

## 1. The defect

`internal/raft/lease.go`:

```go
const LeaseDuration = 5 * time.Second
```

The leader grants itself a read lease so it can answer reads from local state
with no quorum round trip. The lease is refreshed whenever a **majority of peers
acknowledge a heartbeat** (the correct renewal rule) and the deadline is a
genuine `CLOCK_MONOTONIC` reading, so it keeps running while the process is
frozen. None of that is the mistake.

The mistake is the number. A lease read is safe only while nobody else can have
become leader since the lease was granted, which requires

```
LeaseDuration + maxClockDrift  <  ElectionTimeoutMin
        5000ms +        ~100ms  <              600ms     VIOLATED by 8.3x
```

Two consequences, both required by the specification:

| Trigger | What happens |
|---|---|
| `net.partition(minority)` | The isolated leader keeps answering local reads for the remainder of its lease, which outlives its own replacement's election by ~4 s. It cannot learn about the new term, so the window is **deterministic**. |
| `proc.pause` | A frozen leader that wakes **before** the deadline it was granted still believes it holds the lease, and answers from a state machine that is a whole term behind. |

**The fix is one line.** `internal/raft/lease_fixed.go` (build tag `kvfixed`)
sets the lease to `300ms`, comfortably inside `ElectionTimeoutMin`.

### What is deliberately NOT wrong

The lease is an honest monotonic deadline, not a counter of scheduler ticks. A
tick-based lease would fire under any pause length, which is more convenient and
less true: it would make the anomaly independent of pause duration, which is the
opposite of the real-world failure mode, and it would be silently deleted the
day someone rewrote the tick loop as a monotonic-delta catch-up loop. Nothing in
this implementation counts delivered ticks; every decision compares monotonic
deadlines.

`internal/raft/raft_test.go` runs randomized partition schedules over six seeds
and asserts Election Safety, Log Matching and State Machine Safety. That test is
what licenses the "exactly one defect" claim; without it the claim would be an
assertion rather than a result.

---

## 2. Timing budget

| Constant | Value | Where | Rationale |
|---|---|---|---|
| `KV_TICK_MS` | 25 ms | env | Scheduler poll only; no logic depends on tick counts. |
| `KV_HEARTBEAT_MS` | 100 ms | env | Three heartbeats fit inside `ElectionMin` with 2x margin. |
| `KV_ELECTION_MIN_MS` | 600 ms | env | Six missed heartbeats; immune to VM scheduling jitter and WAL fsync spikes. |
| `KV_ELECTION_MAX_MS` | 900 ms | env | 300 ms of randomised spread makes split votes rare. |
| `KV_RPC_TIMEOUT_MS` | 250 ms | env | Under one heartbeat interval plus slack. |
| `KV_APPLY_WAIT_MS` | 2000 ms | env | Server-side ceiling on a write awaiting commit; bounds `queue_depth`. |
| **lease** | **5000 ms** | **compile-time constant** | **The defect.** |

The lease is deliberately **not** an environment variable. If it were, the
injected defect could be turned off (or quietly made worse) by configuration,
and an agent facing a failing gate could "fix" the system without touching a
line of code.

Three nodes receive the **same** `KV_SEED` and each derives its own stream in
process (`raft.DeriveNodeSeed`). Per-node compose defaults of the shape
`${KV_SEED:-1}` / `:-2` / `:-3` collapse to one value the instant anything sets
`KV_SEED`, which is exactly what a world-varying harness does; all three nodes
would then draw an identical election timeout and split every vote.

---

## 3. Topology

Two networks, one service per node.

```
                 kvclient (published to 127.0.0.1)
   host --------> :18081 kv-n1     :18082 kv-n2     :18083 kv-n3
                     |                |                |
                 kvpeer (internal), aliases n1-peer / n2-peer / n3-peer
                     +----------------+----------------+
```

* **One service per node, each publishing its own client port to `127.0.0.1`.**
  Container IPs are not routable from a Windows host, so a host-side health
  probe has no other way in.
* **The peer plane is separate and internal.** Cutting only the peer plane
  produces a *gray* failure: unresponsive to peers, alive to the orchestrator
  and to clients. That is the condition under which the defect is observable:
  a fault that cut both planes would model a dead node, and a dead node cannot
  serve a stale read to anybody. Peers address each other through
  network-scoped aliases that resolve only on the peer network, so peer traffic
  stays off the client plane by construction.
* **No proxies, no ambassadors, no `NET_ADMIN`, no `iptables` in the image.**
  PRO-THESIS injects network faults at run time from a sidecar joined to a
  container's network namespace, so the fixture stays an unmodified system
  under test.

---

## 4. HTTP API

Client plane (`:8080`, published):

```
GET  /healthz                          {"ok":true,"node":...,"variant":...}
GET  /readyz                           200 once a leader is known and commit_index > 0
GET  /status                           role / term / commit_index / lease / resources
GET  /dump                             full state machine with per-key provenance
GET  /kv/{key}[?consistency=lease|linearizable]     default is lease
PUT  /kv/{key}          {"value":N,"op_id":M}
POST /kv/{key}/cas      {"expect":N,"value":M,"op_id":K}
POST /txn               {"op_id":K,"ops":[["r","k/3",null],["w","k/7",N]]}
POST /admin/noop        {"op_id":K}
```

Peer plane (`:9090`, internal): `POST /raft/append`, `POST /raft/vote`.

`role:leader` resolves by reading `/status` on every `kv-*` node and taking the
one reporting `"role":"leader"` with the highest term. Two nodes claiming leader
**in the same term** is itself a finding.

### The error-body contract (soundness-critical)

Every error the server produces **before the command reaches the state machine**
carries `"applied": false`. That flag is a client's only positive evidence for
classifying an operation as `fail`. Everything else is `info`.

| Response | Meaning |
|---|---|
| `200` | Applied. A CAS that ran and did not match is `200` with `ok:false, applied:true`: a successful operation with a negative result. |
| `503 NOT_LEADER` / `NO_QUORUM`, `409 NOT_COMMITTED`, with `applied:false` | Definitely not executed. |
| `504 APPLY_TIMEOUT`, `503 INDETERMINATE`, any 5xx **without** `applied:false` | Unknown. The command may still commit. |

---

## 5. `loadgen`

```
./bin/loadgen --history {history_path} --seed {seed} --profile {profile}
```

Emits the normative history JSONL and **only operation records**: phase markers
belong to the harness, because two writers appending to one file across a bind
mount gives torn lines, and one torn line makes the whole history unparseable.

`t_ns` is **Unix epoch nanoseconds**, captured at the true event instant:
immediately before the request is written for an `invoke`, immediately after the
response is read for a completion. It is *not* taken when the writer wins its
mutex. A linearizability checker's entire input is the real-time interval of each
operation, and timestamping at flush time would widen those intervals under
contention and silently hide real violations. **Consequence: the file is not
guaranteed to be sorted by `t_ns`. Consumers must sort.** `meta.seq` preserves
append order.

**Measured host clock granularity: 505.6 µs.** On this Windows host both
`time.Now()` monotonic deltas and `UnixNano()` quantise to ~0.5 ms. Real
network operations measured 1.5–7 ms, comfortably above that, so their
intervals remain meaningful, but an operation faster than ~0.5 ms records a
zero-width interval, and a checker will treat it as overlapping its neighbours.
That is conservative (it permits more linearizations, so it can hide a
violation, never manufacture one), and it is a property of the host, not of the
driver. A Linux driver host does not have it.

### `ok` / `fail` / `info`

`fail` requires **positive evidence of non-execution**. Everything ambiguous is
`info`. The frozen-node case is why this matters: a paused node's *kernel*
completes the TCP handshake and buffers the request, so a client timeout
genuinely does not know whether the write will be applied on resume. Recording
that as `fail` would make every consistency verdict built on the history
unsound, and it would still look correct. `internal/history/classify_test.go`
has one case per row of the decision table.

The driver follows a `NOT_LEADER` leader hint and retries, but **only** after a
definite non-execution. Retrying an indeterminate outcome would put two possible
executions behind one `op_id`, which is the same soundness hole by another route.

### Profile resolution (OQ-012)

The frozen `driver.cmd` template passes the profile **name** only, so the driver
resolves the parameters itself:

| Profile | clients | ops | keys | mix (read/write/txn/admin) |
|---|---|---|---|---|
| `smoke` | 4 | 500 | 8 | 50/50/0/0 |
| `gate` | 16 | 20000 | 64 | 40/40/20/0 |
| `soak` | 64 | 500000 | 256 | 30/40/20/10 |
| `linear` | 16 | 60000 | 8 | 50/50/0/0 |

#### `linear`: the only profile `linearizable.kv` can check

`txn` reads one key and writes a **different**, independently drawn key, so any
profile carrying txn weight breaks the precondition of Herlihy & Wing's locality
theorem. A per-key linearizability checker handed such an operation must refuse
the whole history rather than partition it, and ours does: run against a real
`gate` history it refuses in 88 ms, naming the offending op id. So `gate` and
`soak` are permanently unavailable to that oracle **by design**, not by
oversight. `smoke` is single-key but retires its 500 operations in ~700 ms,
finishing long before a fault window opening at `@8200ms`.

`linear` is single-key, few-keyed (conflict density is what gives the checker
power) and has a deliberately over-large operation ceiling. The ceiling is not a
target: the driver is stopped at QUIESCE, so a world retires whatever it reaches
(measured, ~15,000 of the 60,000) and overshooting costs nothing while
undershooting ends DRIVE before an anomaly can be observed. See DECISIONS D-040.

> **A clean `linear` world reports INCONCLUSIVE, not PASS.** Because the driver
> is always stopped with operations in flight, `no_stuck_op` cannot finish
> observing them and honestly refuses rather than passing vacuously. A violation
> still outranks it, so the fault-injection runs exit 1 as they should; the cost
> falls only on the negative case. Treat `linear` as a fault-injection profile.
> Tracked as OQ-033.

A harness that *can* pass the resolved profile should, via `PROTHESIS_CLIENTS`,
`PROTHESIS_OPS`, `PROTHESIS_KEYS`, `PROTHESIS_MIX_{READ,WRITE,TXN,ADMIN}`. Those
win over the table and need no change to the frozen template. **An unknown
profile name with no environment override is exit 5 (`CONFIG_ERROR`)**, never a
silent default, because a silently-defaulted profile is a gate nobody can see has
been weakened.

Values are globally unique (`clientIdx * 1_000_000 + seq`), so a read returning
V identifies exactly which write produced it and every stale read is
attributable.

---

## 6. Proving the bug: `provebug`

```powershell
# deterministic, no Docker, ~8 s -- writes golden/
go run ./cmd/provebug -mode inproc

# against the live compose cluster
docker compose up -d --wait
go run ./cmd/provebug -mode docker `
  -targets    "kv-n1=http://127.0.0.1:18081,kv-n2=http://127.0.0.1:18082,kv-n3=http://127.0.0.1:18083" `
  -containers "kv-n1=prothesis-kv-n1,kv-n2=prothesis-kv-n2,kv-n3=prothesis-kv-n3" `
  -scenario pause -pause 2800ms
docker compose down -v
```

It commits a value, cuts the leader's peer plane, waits for the majority to
elect a replacement and commit a new value, reads the displaced leader with an
ordinary (default-consistency) request, then heals and **requires the cluster to
converge**. Convergence is what proves the anomaly is a *stale read* rather than
a permanent split brain: the system is otherwise healthy and the defect is
exactly the injected one.

Exit `0` only if a stale read was observed **and** the cluster converged.
`-expect-clean` inverts it: the negative control for the `kvfixed` build.

Artifacts written to `golden/`:

* `history.jsonl`: a real normative history containing the known violation,
  sorted by `t_ns`.
* `witness.json`: a conformant `prothesis.oracle_output/v1` object, including
  `schema` and `valid_phases`. This is the artifact Phase 3's consistency oracle
  must independently reproduce.
* `telemetry.json`: `/status` snapshots at each stage, plus the measured
  summary.

### Measured reproduction windows

Docker cluster, `buggy` build, lease 5000 ms, election 600–900 ms,
`pause-delay` 200 ms.

| Schedule | Stale-read window | Stale reads |
|---|---|---|
| partition only | **3971 ms** | 148 |
| partition + 2800 ms pause | **1598 ms** (3/3 runs: 1598 / 1587 / 1548 ms) | 62 |
| partition + 3200 ms pause | 1127 ms | 44 |
| partition + 3600 ms pause | 733 ms | 29 |
| partition + 4000 ms pause | 395 ms | 16 |
| partition + 4400 ms pause | **none** | 0 |
| partition + 4800 ms pause | **none** | 0 |
| **partition + 6900 ms pause** (the reference schedule) | **none** | 0 |

The window closes when the pause outlives the lease: the deadline was granted at
the last quorum ack, roughly 200–350 ms before the partition took effect, so a
pause longer than about 4.2 s resumes into an already-expired lease. **The
specification's own reference schedule (a 6.9 s pause against a 5 s lease)
cannot produce this anomaly**, and `provebug -pause 6900ms` demonstrates that
rather than asserting it. Logged as OQ-010. Use

```
net.partition(minority(kv))@8000..16000
proc.pause(role:leader)@8200..11000        # 2.8 s pause  <  5 s lease
```

with both faults **co-resolved to the same node**. On three nodes, freezing one
node and isolating a different one leaves a single node alone (not a majority)
so no election can occur and no stale read is possible.

---

## 7. Building the patched variant

`KV_VARIANT` is **one** variable: it selects the Go build tag in the Dockerfile
*and* the image tag in `docker-compose.yaml`.

```powershell
$env:KV_VARIANT = "kvfixed"
docker compose build
docker compose up -d --wait
```

That coupling exists because a patched binary published as `:buggy` would
produce a false green indistinguishable from a true one; the worst possible
failure for a tool whose entire claim is "we found the known bug". As a second,
independent check the binary reports its compiled-in `variant` at `/status`,
derived from the build tag rather than from any environment value, so a
mislabelled image still tells the truth about itself.

Keeping the fix in-tree is an anti-gaming hazard in its own right: an agent
facing a failing gate could flip a build argument instead of writing a fix.
The lock manifest must cover the harness build arguments and record the resolved
image digest in the world file and the verdict.

---

## 8. Layout

```
internal/raft/       Figure 2 minus snapshots and membership change
  lease.go             THE DEFECT      (//go:build !kvfixed)
  lease_fixed.go       THE FIX         (//go:build  kvfixed)
  raft.go              event loop, elections, replication, commit advance
  log.go storage.go    in-memory log; state.json (fsync+rename) + wal.jsonl
  transport.go         HTTP/JSON peer RPC client and server
  raft_test.go         elections, replication, restart, randomized safety sweep
internal/kv/         state machine, client-plane HTTP, read and write paths
  lease_bug_test.go    the load-bearing lease test (passes under both tags)
internal/history/    JSONL writer and the ok/fail/info decision table
internal/client/     HTTP client that gathers the evidence the table needs
internal/netsim/     in-process peer transport with cuttable links
internal/workload/   profile-name resolution
cmd/kv/              node binary: serve | healthcheck | steady | version
cmd/loadgen/         the driver
cmd/provebug/        independent demonstration of the anomaly
```

## 9. Checks

```powershell
$env:PATH = "$env:LOCALAPPDATA\Programs\Go\bin;$env:PATH"
go build ./... ; go vet ./... ; gofmt -l . ; go test ./...
go test -tags kvfixed ./...
```
