# PHASE 0 BUILD BRIEF — authoritative

This document resolves every conflict surfaced by the design and audit passes. Where a design
document in `docs/design/` disagrees with this brief, **this brief wins**. Implementers read this
first, then consult the design docs for detail.

Phase 0 deliverables (base directive §6) and nothing else:
- `pkg/schema` — Go types for `prothesis.yaml`, `.thesis` worlds, history logs, verdicts
- `internal/recorder` — virtual clock, deterministic PRNG seed streams, `.thesis` codec
- `internal/harness` — `compose` backend skeleton
- `cmd/thesis` — `init`, `up`, `down`
- `testdata/kvfixture` — the 3-node buggy Raft KV store (built first, per §7)

Phase 0 definition of done:
- **(a)** `thesis up && thesis down` boots and tears down the KV fixture cleanly
- **(b)** a world configuration round-trips through `.thesis` serialization **byte-identically**

---

## 0. Measured environment facts

These were measured on this machine during the design pass. Do not re-derive or assume otherwise.

| Fact | Value |
|---|---|
| Host | Windows 11, PowerShell. **Git Bash is broken** (`dofork ... 0xC0000142`) |
| Go | 1.27.1 at `%LOCALAPPDATA%\Programs\Go` |
| Docker | Desktop 29.1.3, Compose v2.40.3, Linux engine, 8 CPUs, ~30 GiB |
| 3-node `compose up -d --wait` | 1.45–2.04 s |
| 3-node `compose down -v` | 1.63–1.68 s |
| Full orchestration cycle | ~3.3 s |
| 8 concurrent compose projects | 3.8× speedup, +3–5 % timing distortion |
| **Docker bridge networks** | **hard ceiling 30; 24 free.** Leaked projects consume them permanently |
| **Container IPs from Windows host** | **NOT routable.** Verified: `docker run nginx`, IP 172.17.0.2, request times out |
| tc/netem + iptables from `NET_ADMIN` sidecar sharing an unmodified container's netns | **works**: 8.3 ms → 311.7 ms → 6.7 ms inject/withdraw |

Two consequences bind Phase 0:
1. Health probes **must** run host-side against published `127.0.0.1` ports. Container IPs are
   unreachable, so "publish no ports" is not available as a parallelism strategy.
2. `down` must be reliable to the point of paranoia. A leaked project permanently consumes one
   of 24 bridge networks; leaks are unrecoverable within a session.

---

## 1. Decisions that bind every package

### D-A. Dependencies: `gopkg.in/yaml.v3` and nothing else
Config is YAML; the standard library has no YAML. One dependency, pinned.

`pkg/schema` **may** import `yaml.v3` for the config types. It must **not** import anything else.
Verdict, world, history and oracle types use `encoding/json` only, so third parties consuming
`prothesis.verdict/v1` need no dependencies.

Rejected: the "obsolete func-based `UnmarshalYAML`" trick to keep `pkg/schema` stdlib-only. It was
verified broken — it relies on `yaml.v3` *failing* to decode an integer scalar into a Go string,
which it does not do, so the directive's own `retain_passing: 3` would be rejected as
`CONFIG_ERROR`. Use `UnmarshalYAML(*yaml.Node)` and switch on `node.Tag`.

### D-B. `go.mod` declares `go 1.22`
The reference stack targets Go 1.22+. The installed toolchain is 1.27.1. Pinning `go 1.27.1` in
`go.mod` would refuse to build on any conforming toolchain. Language version `1.22`; no
`toolchain` directive.

### D-C. `t_ns` is Unix epoch nanoseconds
The field name is normative and says nanoseconds. §4.4's *example values* (`1725300000000000`)
are Unix epoch **microseconds** for Sept 2024 — epoch nanoseconds for that date are ~1.7e18,
three orders of magnitude larger. The example values are wrong; the field name governs.

Emit true `time.Now().UnixNano()`. The harness records `drive_origin_wall_ns` once at DRIVE start,
which makes the epoch→`start_ms`/`end_ms` conversion for `prothesis.oracle_input/v1` exact.
This is the anchor that makes I5 phase-aware assertion evaluable at all. Logged as OQ-008.

Rejected: run-relative nanoseconds. The driver is an external, unmodified process (§L) that
cannot know the harness's virtual-clock origin, so it can only emit wall time.

### D-D. Only the driver writes op records; only the harness writes phase markers
§4.4 shows phase markers interleaved with op records in one stream, but §4.2 hands
`{history_path}` to an external process. Two writers appending to one file gives torn lines
across the Docker Desktop VM / Windows bind-mount boundary, and one torn line makes the whole
history unparseable — surfacing as a spurious `INCONCLUSIVE` that reads like an environment flake.

- driver writes **only** op records to `{history_path}`
- harness writes phase markers to `phases.jsonl`
- recorder merges both by `t_ns` at TEARDOWN into the canonical `history.jsonl` that
  `oracle_input.history_path` points at

### D-E. Network faults use a run-time `NET_ADMIN` sidecar, not ambassador proxies
Empirically verified on this machine: a sidecar joined with `network_mode: container:<target>`
injects and withdraws `tc`/`netem` and `iptables` against an **unmodified** container.

This overrides the harness design's ambassador-proxy commitment. Reasons:
- ambassadors require modifying the target's topology, contradicting §L "works on unmodified
  systems"; the sidecar does not
- ambassadors inside the *fixture's* compose file would make Phase 2's injector depend on
  target-supplied infrastructure
- it dissolves the Phase 0/Phase 2 ordering hazard entirely

**Phase 0 consequence: the fixture compose file needs no proxies.** Nodes talk to each other
directly over the compose network.

Binding rules for Phase 2 (recorded now so Phase 0 does not foreclose them):
- `net.partition` must be **bidirectional** — a single `iptables -I INPUT -s <peer> -j DROP` in
  one netns is a one-way drop, not a partition
- every injected rule carries `-m comment --comment "thesis:<run_id>:<fault_id>"`; withdrawal
  matches on the comment, and `HEAL` residual verification greps for the `thesis:` prefix
- `iptables-save` and `tc qdisc show` are snapshotted at BOOT, so residual checks compare against
  the observed baseline rather than an assumed-empty one

Honest limitation: `netem`'s loss/reorder/duplicate randomness is in-kernel and unseedable, so
those faults are not bit-reproducible under replay. That is a Tier B property the spec already
concedes; the verdict reports `reproduced: "k/n"` and never claims more. Logged as OQ-009.

### D-F. Health probes host-side; `steady_state` inside a helper container
`{host}` = `127.0.0.1`, `{port}` = the **published host port**. Container IPs are unreachable
from the Windows host (measured), so each logical node needs its own published client port,
which means **one compose service per node** in the fixture.

`steady_state.probe: "thesis-helpers/steady.sh"` runs via `docker compose exec` in a helper
container. This sidesteps the broken Git Bash entirely, and is more correct anyway — steady state
needs an in-network vantage point.

### D-G. The lease bug uses an honest monotonic deadline, not a tick counter
Spec §5 says "a 5-second lease" and "wakes up before its local clock invalidates the lease".
Implement exactly that: `leaseDeadline = monotonicNow() + 5s`, refreshed on each successful
heartbeat quorum; local reads served while `monotonicNow() < leaseDeadline` with no quorum check.

**The spec's own reference schedule cannot trigger this bug.** §6 Phase 2 gives
`proc.pause(role:leader)@8200..15100` — a 6.9 s pause. `CLOCK_MONOTONIC` continues advancing
during `SIGSTOP`, so a 5 s lease has expired by the time the process resumes and the stale read
never happens. The schedule needs a pause **shorter** than the lease:

```
proc.pause(role:leader)@8200..11000          # 2.8 s pause  <  5 s lease
net.partition(minority(kv))@8400..10900      # overlapping
```

Logged as OQ-010 (BLOCKING for the Phase 2 DoD). Do **not** paper over this by expressing the
lease in raft ticks to exploit `time.Ticker` collapsing missed ticks under `SIGSTOP`. That
mechanism was proposed and rejected: it is fragile (any future rewrite of the tick loop as a
monotonic-delta catch-up loop silently deletes the bug), it deviates from §5's literal wording,
and it makes the anomaly independent of pause duration, which is the opposite of the real-world
failure mode this fixture is meant to model.

### D-H. Config defaulting: `DefaultConfig()` then decode-in-place — but never for maps
`yaml.v3` decodes in place and sets only fields physically present, so scalar and struct defaults
survive. Use `Decoder.KnownFields(true)` for typo rejection.

**Do not pre-seed map-valued sections** (`profiles`, `driver.profiles`, `mix`). Verified:
- map **entries** do not merge — `profiles: {soak: {worlds: 5}}` yields `soak` with an empty
  budget, silently destroying the entry's other defaults
- map **keys** do merge — a user who *deletes* `soak` gets a fully populated `soak` back from
  compiled-in defaults, which would make profile deletion invisible to the lock and hole
  anti-gaming rule 2
- `KnownFields(true)` does **not** protect map keys — a typo'd profile name decodes with
  `err == nil` and silently creates a phantom profile

So: decode maps from the document only, then apply per-entry defaults explicitly after decode,
and validate every profile (non-zero budget, non-empty `driver_profile`, `worlds != 0`).

### D-I. Every nested verdict object is a named exported type
`prothesis.verdict/v1` is a **per-run aggregate**; A.6's `Utility()` scores a **single world**.
`WorldResult` therefore can never be the verdict — it is an internal type arriving in Phase 1.
Phase 0's only obligation is that no verdict field is an anonymous inline struct, so Phase 1 can
compose them. Name them all: `Budget`, `Violation`, `Witness`, `MinimalRepro`, `Shrink`,
`TimelineEvent`, `Suspect`, `Coverage`, `CoverageDelta`, `OracleLock`.

### D-J. Phase 0 scope — explicitly out
Do **not** build, even "for forward compatibility": telemetry format or `telemetry.jsonl`,
`determinism.Scan` / `thesis doctor`, artifact retention, MCTS provenance fields (`TreeID`,
`TreePath`), coverage extraction, the lock manifest, or any oracle. `WorldMeta` is
hash-excluded and `omitempty`, so provenance fields can be added later without invalidating a
single committed world — the forward-compatibility argument for adding them now is false.

---

## 2. `.thesis` world file — the format byte-identity depends on

### Canonical encoding rules (all mandatory)
1. Object keys sorted by byte order.
2. No insignificant whitespace. Exactly one trailing `\n`.
3. **LF only.** Requires `.gitattributes` in the first commit (see §4).
4. HTML escaping **off** — `json.Encoder.SetEscapeHTML(false)`. Go escapes `<` and `>` by
   default, which would corrupt the edge target `n1<->n2` and change every world hash.
5. **No bare floats anywhere.** Ratios are integer parts-per-million. Go's shortest-float
   representation is not guaranteed stable across versions, and a change would break every
   committed world at once.
6. **No `map` and no `interface{}`** in the world type graph. Map iteration order is randomized;
   `interface{}` decoding turns integers into `float64` and loses precision above 2^53.
7. Decoder is strict: `DisallowUnknownFields`.
8. `Load` re-encodes and byte-compares on every call, so byte-identity is a **runtime invariant**,
   not merely a test assertion.

`seed` is `uint64` emitted as a JSON number. This is exact because the type graph is fully typed —
`encoding/json` marshals and unmarshals `uint64` losslessly. The float64 precision trap only
applies to `interface{}` decoding, which rule 6 forbids.

### Structure
```
schema            "prothesis.world/v1"   ADDITIVE — the directive names no schema id for the file
seed              uint64
topology_variant  string
driver_profile    string
fault_schedule    { planned: [...], realized: [...] | null }
phase_timings     [...]
sut               { images: [{service, image, digest}] }   ADDITIVE — see below
meta              { origin, parent_hash }  HASH-EXCLUDED, omitempty
```

`world_hash` = SHA-256 over the canonical encoding of exactly invariant I2's five-tuple plus
`sut`. `meta` is excluded so provenance never changes a world's identity.

**Why `fault_schedule` is split into planned/realized.** This is required by the *base* spec,
independent of the Saboteur. `role:leader`, `minority(kv)` and `kv:*` bind to concrete nodes at
injection time from live cluster state — Raft leadership moves at runtime. A world that records
only `proc.pause(role:leader)` does not reproduce, because on replay a different node may be
leader. `realized` records what was actually injected against which concrete node at which
virtual-clock time; `thesis replay` executes `realized` when present. This is what makes I2's
"self-contained file that reproduces the issue" true rather than aspirational. `realized` is
`null` for a world that has not been executed.

**Why `sut` is additive and necessary.** I2's five-tuple omits the identity of the system under
test. Without it, a regression world replayed after a patch silently tests a different program,
`thesis bisect` cannot be sound, and `reproduced: "3/3"` is a claim about an unknown binary.
Record the resolved image digests. Logged as OQ-011.

---

## 3. `internal/recorder` — PRNG streams

**Required property:** adding a *new* named stream must not change any value produced by an
*existing* stream for the same root seed. Sequential child-seeding (`child_i = f(parent, i)`)
violates this and would invalidate every committed regression world the moment Phase 4 adds the
Saboteur's streams.

Derivation: `key = HMAC-SHA256(rootSeed, domainPath)`, and that key seeds a stream cipher. A
stream's key is a function of its path alone, so independence is structural rather than
accidental.

Do not use `math/rand`'s helpers for value extraction — neither `math/rand` nor `math/rand/v2`
guarantees a stable value sequence across Go versions, and every archived world depends on it.
Write the extraction primitives in-repo (bounded uint64, float64, shuffle) and pin them with a
golden-vector test.

Phase 0 registers only the streams Phase 0 and Phase 1 need. Reserve, but do not implement, the
Saboteur's. Path separator must be a byte the domain grammar forbids, so `Derive("a","b/c")` and
`Derive("a/b","c")` cannot collide.

---

## 4. `.gitattributes` — must be in the first commit

The repository is on Windows with `core.autocrlf=true` and no `.gitattributes`. If a `.thesis`
file is ever committed without one, a later checkout rewrites every line ending, the strict
parser rejects the file, and every `world_hash` in the regression corpus breaks at once. The
failure surfaces in someone else's clone, long after the mistake.

```
*.thesis          text eol=lf
*.jsonl           text eol=lf
.prothesis/lock   text eol=lf
*.sh              text eol=lf
```

Use `text eol=lf`, **not** `binary`. `binary` implies `-diff`, which would render every lock and
regression-corpus change as "Binary files differ" — defeating the human review that anti-gaming
rules 1 and 4 depend on.

---

## 5. `internal/harness` — compose backend

- **Docker CLI, never the Go SDK.** Verified: `DOCKER_HOST` is unset and the current context is
  `desktop-linux`, so the SDK would silently dial the wrong named pipe. Compose v2 also has no
  stable Go API.
- `--progress json` is a **root-only** flag: `docker compose --progress json up`, not
  `docker compose up --progress json`.
- Logical node → container binding via a generated overlay stamping `io.prothesis.node=<id>`
  labels. Do **not** stamp per-invocation values (`run`, `owner_pid`, `created_at`) into *service*
  labels — that changes the service definition hash and forces a recreate on every command.
- **`up` and `down` are separate OS processes.** State that `down` needs — project name, port
  assignments, node→container bindings — must be persisted to `.prothesis/runs/<run_id>/` (or a
  well-known current-run pointer) by `up`, not held in memory.
- `down` is idempotent, must not leave orphans after a crash, and verifies its own work:
  after it returns, label-filtered container/network queries must come back empty. Given the
  24-network ceiling, also provide a reaper for leaked `thesis-*` projects.
- Port preflight failure is an **environment** error → exit `2` (INCONCLUSIVE), not `5`
  (CONFIG_ERROR). Exit 5 is for invalid `prothesis.yaml` or CLI arguments.

---

## 6. `testdata/kvfixture`

- Nested Go module. Hand-written minimal Raft (Figure 2 minus snapshots and membership change),
  or a vetted library — implementer's call, but the injected defect must be the **only** safety
  bug, and role/term must be observable over HTTP for `role:leader` targeting.
- One compose **service per node** (D-F), each publishing its own client port to `127.0.0.1`.
- No ambassador proxies (D-E).
- Per-node seeds must not collapse: `KV_SEED: "${KV_SEED:-1}"` / `:-2` / `:-3` gives all three
  nodes the *same* seed the moment anything sets `KV_SEED`. Use distinct variable names.
- The patched-build tag and the image tag must not be independent, or
  `KV_TAGS=kvfixed docker compose build` produces the *patched* binary tagged `:buggy`.
- HTTP: `/healthz`, read, write, plus role/term/commit-index for targeting.
- `loadgen` emits the normative history JSONL and **must** distinguish `ok` / `fail` / `info`
  correctly — `info` for any indeterminate outcome such as a timeout where the write may or may
  not have landed. Getting this wrong makes every consistency oracle unsound.
- `provebug`: a dependency-free binary that demonstrates the anomaly **without** PRO-THESIS and
  emits a golden `history.jsonl` + `witness.json`. Without it, a PASS from PRO-THESIS is
  uninformative. Note `driver.profiles` (`clients`, `ops`, `mix`) do not reach loadgen through
  the frozen `driver.cmd` template — `{profile}` passes only the profile *name*, so loadgen must
  resolve the profile itself or receive it another way. Logged as OQ-012.

---

## 7. Rules for the implementer

1. Never weaken an assertion to make something pass. If a requirement is wrong, it goes in
   `OPEN_QUESTIONS.md` and stops there.
2. Every phase ships a compiling binary. `go build ./... && go vet ./...` must pass at every commit.
3. `gofmt` clean.
4. Do not invent normative field names. Additive fields are permitted only when the directive
   names no field for something that must exist; mark them `// ADDITIVE` and log them.
5. Tests must be able to fail. A test that asserts a property the code under test computes
   internally proves nothing — the byte-identity fuzz test must compare against bytes read from
   disk, not against the decoder's own re-encoding.
