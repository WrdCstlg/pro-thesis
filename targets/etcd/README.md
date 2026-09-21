# `targets/etcd`: the second target

Upstream etcd v3.5.17, three members, unmodified image, no planted bug. The
reference fixture proves the harness finds a defect it was built to find; this
directory asks whether the same unmodified checker and the same fault
injectors discriminate a correct read mode from an incorrect one on a system
nobody here wrote.

The expectation was written down before the first run:
[`.prothesis/PREREGISTRATION.md`](.prothesis/PREREGISTRATION.md).

## What is here

| Path | What it is |
|---|---|
| `docker-compose.yaml` | Three etcd members, client plane published to 127.0.0.1, peer plane internal. Nothing harness-specific inside the containers. |
| `prothesis.yaml` | The project. Two driver profiles differ in ONE mix entry: `read` (linearizable) versus `read_serializable`. |
| `cmd/etcdload` | The workload driver. net/http against etcd's JSON gateway; writes `prothesis.history/v1` through the public `pkg/schema` package. Honours `PROTHESIS_TARGETS`, the driver plan in both modes, and the stdin drain. |
| `cmd/etcdsteady` | The steady-state probe, host-side, because the image ships no shell. One leader, one term, bounded raftIndex spread, every member healthy. |
| `cmd/etcdrole` | The `harness.role_probe`: one JSON line per member from `/v3/maintenance/status`, so `role:leader` and `role:follower` targets bind here without the fixture's `/status` document (D-066). |
| `internal/etcdapi` | The slice of etcd's HTTP surface both probes share. |
| `.prothesis/oracles/linearizable.kv.yaml` | The same external checker the fixture uses, unchanged. |
| `.prothesis/lock` | The committed oracle lock. |

## Run it

From the repository root, then from this directory. Add `.exe` on Windows.

```bash
go build -o bin/thesis ./cmd/thesis
go build -o targets/etcd/bin/etcdload ./targets/etcd/cmd/etcdload
go build -o targets/etcd/bin/etcdsteady ./targets/etcd/cmd/etcdsteady
go build -o targets/etcd/bin/etcdrole ./targets/etcd/cmd/etcdrole
go build -o targets/etcd/bin/linearizable-kv ./cmd/thesis-oracle-linearizable
docker pull quay.io/coreos/etcd:v3.5.17

cd targets/etcd
../../bin/thesis run --profile smoke                                             # 2 worlds, no faults
../../bin/thesis run --profile linear --fault 'net.partition(etcd-n2)@3000..7500' # arm A
../../bin/thesis run --profile stale  --fault 'net.partition(etcd-n2)@3000..7500' # arm B
```

You will see a warning about the checker binary on first run; the lock
records the SHA-256 of the one built on the author's machine (D-060).

## What was measured

Both arms were run once, on 2026-09-15, exactly as pre-registered.

| Arm | Run | Worlds | Verdict | Pre-registered |
|---|---|---|---|---|
| A: linearizable reads, partition `etcd-n2` | `r_2026_09_15_8c77` | 2 / 2 | PASS, exit 0 | PASS |
| B: serializable reads, same partition | `r_2026_09_15_9a29` | 1 (stops on first FAIL) | FAIL, exit 1, `linearizable.kv` on `k/0` | FAIL |

**Reproduced on a second machine.** CI on a GitHub-hosted Ubuntu 24.04 runner
reruns both arms, with `role:leader` targets, on every push to `master`, and
asserts the exit codes (D-068). In CI verification, arm A passed 2 of 2 worlds
with 9,514 and 9,529 operations checked, and arm B failed in world 1 with
`linearizable.kv` exhausting its search over 1,345 operations on `k/0`.

**The partition bit.** In arm A every operation on `etcd-n2` from about
t+3.5 s until the heal timed out: 32 `info` records inside the window, in
three rounds of sixteen (one per client, 2 s each), and zero `fail` records,
because a timeout is not evidence of non-execution.

**The verdict was re-derived, not trusted.** Independently of the checker,
an `ok` read is revision-stale when etcd's own `header.revision` on its
response is below the revision of a write that was acknowledged before the
read was invoked. Arm B's history holds 313 such reads; arm A's holds 0.

**The pre-registered mechanism was wrong.** Those 313 stale reads are spread
across all three members, not concentrated on the partitioned one, and the
first of them lands at t+69 ms (the fault window opens at t+3000 ms). Under
sixteen clients, ordinary replication lag is enough to make a serializable
read non-linearizable; the partition was not needed. The prediction was
right for a reason other than the one written down, and that is the finding.

**The first run found a harness bug.** `r_2026_09_15_ace7` failed every
world on `availability_after_heal` with "HTTP status 404" on all members
while the declared health probe (`/health`) answered 200 throughout: the
post-HEAL convergence probe used a hard-coded `/healthz`, the fixture's path.
Fixed with a test that fails on the old code (OQ-063, D-065).

## Role targets

With `harness.role_probe` declared, a fault can name the leader instead of a
member id. Measured on 2026-09-15, same fault window as the arms above:

| Profile | Run | Bound to | Verdict |
|---|---|---|---|
| `stale`, `net.partition(role:leader)` | `r_2026_09_15_d939` | `etcd-n2` (injected t+4341 ms: the probe binary's first run on this host cost ~1.3 s) | FAIL, `linearizable.kv` |
| `linear`, `net.partition(role:leader)` | `r_2026_09_15_fa3e` | `etcd-n1` in both worlds (t+3038, t+3020 ms) | PASS, 2 / 2 |

Before D-066 every `role:` target on this system exited 2, because roles
were read from a `/status` document etcd does not have.

## Shrinking the failing world

`thesis shrink` on arm B's world, 16-world budget, default strictness:

| Stage | Result | Note |
|---|---|---|
| faults | 1 → 0 | the shrinker dropped the partition on its own: it was never needed |
| ops | 16,788 → 2,098 | partial; budget exhausted after 10 worlds |
| confirm | 1/3 | two replays failed on a *different* key (`k/1`); refused |

Exit 3, nothing committed. The driver executed reduced operation plans of
8,394, 4,197 and 2,098 operations verbatim; the calibration bound (D-063)
re-executed the unreduced world three times and held the witness key at
`k/0`; and the gate refused a reduction that reproduced once, because on this
target the first stale read lands on whichever key a lagging member serves
first. The report names the two remedies (`--accept-trials 3`,
`--strictness class`).

A second run at `--strictness class` with a 24-world budget reached
16,788 → 131 operations and confirmed **1/3** again: with no fault and a
short trace, the replication-lag race is hit about one time in three, and
ddmin had accepted each halving on a single lucky sample. A third run took
the report's own remedy, `--accept-trials 3`, with a 70-world budget: every
halving down to 197 operations reproduced three times out of three, and the
gate still said **1/3**. A violation whose probability falls with trace
length breaks ddmin's monotonicity assumption, and more acceptance trials
do not restore it. All three runs are the harness refusing to invent a
minimal repro, which is the behaviour it exists for; none is a
demonstration that operation-level shrinking completes (OQ-052, OQ-063).

## What is not claimed

- Nothing about etcd beyond the documented semantics of its two read modes.
- Single-key register operations only. No transactions, leases or watches.
- Two built-in oracles are disabled: `no_unbounded_queue` (needs `/status`)
  and `resource_return_to_baseline` (reads counters through a shell exec'd
  in the container, and there is no shell). Enabled, they return INCONCLUSIVE
  on every world, which is the fail-closed behaviour working and a real
  limitation of the harness against a shell-less image. See OQ-063.
