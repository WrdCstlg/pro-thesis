# `targets/rqlite`: the third target

Upstream rqlite 10.5.3 (a Raft-replicated SQLite with a plain HTTP API), three
voting nodes, the unmodified image pinned by digest, no planted bug. Its
correct answer under a leader partition is PASS at the `linearizable` and
`strong` read levels; `weak` (rqlite's default) and `none` are documented as
weaker. One query parameter selects the level, so one target carries a PASS
arm and a FAIL arm, the shape the etcd target has with linearizable and
serializable reads.

Status: **onboarded, no verdict arm run.** One `smoke` world (no faults) has
run; no `linear`, `strong`, `weak` or `none` world has. Nothing below is a
claim about rqlite.

## What is here

| Path | What it is |
|---|---|
| `docker-compose.yaml` | Three rqlite nodes from `rqlite/rqlite:10.5.3@sha256:de385382...b6b5`, HTTP API published to 127.0.0.1 (host ports 14001..14003 by default), Raft not published, named volumes, Raft timings stated explicitly. Nothing harness-specific inside the containers. |
| `prothesis.yaml` | The project. Five driver profiles that differ only in the read entry of the mix: `smoke` and `linear` (`read_linearizable`), `strong`, `weak`, `none`. |
| `cmd/rqliteload` | The workload driver. Go standard library only. Writes to the leader only with `?redirect`, reads at the level the mix names, records that level on every read, and checks `X-RQLITE-VERSION` on every response. |
| `cmd/rqliterole` | The `harness.role_probe`: every node's `/nodes?ver=2`, each leader `api_addr` mapped to a node id. Refuses when the nodes disagree. |
| `cmd/rqlitesteady` | The steady-state probe: every node `/readyz` with an explicit timeout, and one leader every node agrees on. |
| `internal/rqliteapi` | The slice of rqlite's HTTP surface both probes share. |
| `.prothesis/oracles/linearizable.kv.yaml` | The checker definition the etcd target uses, byte for byte (its comments still name etcd, on purpose: the bytes are the same, which anyone can check by hash). |
| `.prothesis/lock` | The committed oracle lock. |

## Build and run

From the repository root, then from this directory. Run every `thesis`
command from this directory: launched from elsewhere, teardown can leave the
world's volumes behind (OQ-087).

```powershell
pwsh -File scripts/build.ps1                                   # bin/thesis.exe
go build -trimpath -buildvcs=false -o targets/rqlite/bin/rqliteload.exe   ./targets/rqlite/cmd/rqliteload
go build -trimpath -buildvcs=false -o targets/rqlite/bin/rqliterole.exe   ./targets/rqlite/cmd/rqliterole
go build -trimpath -buildvcs=false -o targets/rqlite/bin/rqlitesteady.exe ./targets/rqlite/cmd/rqlitesteady
# the checker: the fixture's locked build, copied unchanged
Copy-Item testdata/kvfixture/bin/linearizable-kv.exe targets/rqlite/bin/
docker pull rqlite/rqlite:10.5.3

cd targets/rqlite
..\..\bin\thesis.exe oracles verify
..\..\bin\thesis.exe doctor
..\..\bin\thesis.exe run --profile smoke      # 1 world, no faults
```

The lock records the SHA-256 of each program it names (the driver and the
checker). A rebuild that changes the bytes is reported as a warning, not as
drift (D-060).

## The driver

- **Where the workload comes from.** `driver.cmd` (lock-covered) and the plan
  the harness writes from `driver.profiles` (lock-covered for every profile a
  run profile names, D-090). The read level is part of the mix entry's name,
  because a driver profile carries only `clients`, `ops` and `mix`. The driver
  reads no workload from the environment: the one variable it reads is
  `PROTHESIS_TARGETS`, the topology. A missing plan, a zero client or op count,
  an empty mix and an unknown read level are refused before the history file
  exists. An operation-mode plan is refused too: this driver does not replay
  traces, so op shrinking against this target is not available.
- **Writes go to the leader only.** `POST /db/execute?redirect`. A follower
  answers 301 without forwarding; the driver maps the Location's host name to a
  node and sends the next write there. It never follows the redirect: a
  forwarded write is retried by rqlite at least once and can apply twice
  (rqlite issue 1628).
- **ok, fail, info.** `fail` needs positive evidence that nothing happened: the
  request was never sent, or the write was answered 301 (rqlite refuses with
  "not leader" before it applies anything). Everything else that is not a
  fully parsed success is `info`, including an HTTP 200 carrying a top-level
  `error` or a per-statement `error`.
- **The version.** Every response's `X-RQLITE-VERSION` must equal
  `--expect-version` (`v10.5.3`). A mismatch is recorded on the operation that
  saw it, stops the run and ends it non-zero.
- **The read level** sent is in every read record's `meta.read_mode`.

## What is claimed

Nothing about rqlite yet. What was measured during onboarding: the image digest
pulled matches the pin, the tests below fail when their logic is removed, and
one fault-free world reached ASSERT and wrote its bundle.

The driver's and the role probe's tests use response shapes read from rqlite
v10.5.3's source (`http/service.go`, `http/nodes.go`,
`command/encoding/json.go`, `store/store.go`), not from a live node.

## What is not claimed

- **The image under test is named in `docker-compose.yaml`, which the lock does
  not cover.** Pinning by digest means compose cannot silently pull a different
  image under the same reference, but the file itself can be edited without
  moving the lock. The harness records the image that actually ran in every
  world (`sut.images`, D-070); a world counts only after that identity is
  compared with `sha256:de385382ef6d93a4b4b831eac4d419cfa35fdb1515621a4ad8ba8b5a5beeb6b5`.
- Single-key register operations only: no transactions, queued writes,
  multi-statement requests or read-only replicas.
- `/nodes` reports no Raft term, so the role probe cannot rank two leadership
  claims; it refuses instead, and a `role:` fault then fails to bind
  (fail-closed) rather than land on a guess.
- Two built-in oracles are left out, as on etcd: `no_unbounded_queue` (it reads
  the fixture's `/status` document) and `resource_return_to_baseline`. Neither
  has been measured against rqlite.
- The driver, probe and checker binaries carry no build stamp; their identity
  is the SHA-256 the lock records.
