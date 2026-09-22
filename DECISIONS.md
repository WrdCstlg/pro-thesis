# DECISIONS

Architectural choices made where the specification fixes behaviour but leaves implementation
open (base directive, Core Implementation Rule 3). Each entry states the choice, the rationale,
and what was rejected. Append-only; supersede by adding a new entry that references the old one.

Spec sources in force:
- The base build directive (authoritative for naming and schemas). NOT PUBLISHED in this
  repository: removed from the tree and from history on 2026-09-15, see D-069. Entries below
  written before that date cite it by its old filename.
- `docs/protocol/SABOTEUR_INJECTION.md`: Addendum A, the Saboteur engine (supersedes the Phase 4
  mutation loop)
- A second pasted document using "CRUCIBLE"/`crux` naming, never on disk in this repository: see
  D-001

---

## D-001: PRO-THESIS naming is authoritative; CRUCIBLE contributes content, not names

**Choice.** The binary is `thesis`, the config is `prothesis.yaml`, the state directory is
`.prothesis/`, world files are `*.thesis`, and the schema identifiers are `prothesis/v1`,
`prothesis.verdict/v1`, `prothesis.oracle_input/v1`, `prothesis.oracle_output/v1`.
The "CRUCIBLE" document's `crux` / `crucible.yaml` / `.crucible/` / `*.crux` / `crucible/v1`
names are treated as superseded aliases and are not used anywhere in the tree.

**Rationale.** Both `PRO_THESIS_FABLE_PROMPT.md` and `CRUCIBLE_FABLE_PROMPT.md` on disk are
byte-identical (SHA-256 `1CE1C0BB…78F0`) and both contain the PRO-THESIS directive. The
CRUCIBLE-named text exists only in the user's chat message. The on-disk artefacts, the
directive header, and the project directory name all point one way. Confirmed with the user
before any code was written.

**But the CRUCIBLE document is not discarded.** It carries substantial normative content absent
from the on-disk directive, all of which is in force: the Tier A / Tier B determinism model,
`thesis doctor`, the five anti-gaming mechanisms, the full v1 fault matrix, the eight oracle
classes, the coverage-signal priority ordering, the search-loop pseudocode, the full CLI
surface, the exit-code agent-loop contract, the non-goals, and the `ok`/`fail`/`info` soundness
rule. Extracted and reconciled in `docs/spec-merge.md`.

**Rejected.** Adopting `crux` naming (would orphan the on-disk directive and the repo layout in
spec §3). Ignoring the CRUCIBLE text (would drop the determinism tiers, `doctor`, the detailed
anti-gaming rules, and several required fault kinds).

**Conflict logged separately** as OQ-001, per Core Implementation Rule 4.

---

## D-002: Go 1.27.1, installed per-user, no third-party dependencies without justification

**Choice.** Go 1.27.1 (latest stable at time of build), installed from the official
checksum-verified ZIP to `%LOCALAPPDATA%\Programs\Go` and added to the user PATH only. The
module path is `github.com/prothesis/prothesis`. *(Amended by D-064 on 2026-09-14: the path is
now `github.com/WrdCstlg/pro-thesis`; the one chosen here was never an account this project held.)*

**Rationale.** Go was not present on the build machine. A per-user ZIP install needs no
administrator rights, does not modify system state, and is reversible by deleting one
directory. SHA-256 verified against the value published by `https://go.dev/dl/?mode=json`
(`a3911b5e…d95d`) before extraction.

The dependency posture follows from the directive's "single, self-contained static binary"
requirement and from I3's claim that the oracle library is the defensible asset: every
third-party dependency is a supply-chain surface on a tool whose entire value proposition is
trustworthiness of its verdict. Standard library by default. Each dependency added must be
justified in this file.

**Rejected.** `winget install GoLang.Go` (system-wide, would likely prompt for elevation this
account does not hold).

---

## D-003: Compose backend targets the Docker Desktop Linux VM; a Windows `process` backend cannot exist for v1

**Choice.** The `compose` backend is the only functional v1 backend on this machine. The
`process` backend, though named in the directive's reference stack, is not implementable on a
Windows host for the fault families the spec requires.

**Rationale.** This is a capability fact, not a preference. `proc.pause` is specified as
SIGSTOP/SIGCONT, `net.partition` as iptables, and `net.latency`/`net.loss`/`net.reorder` as
tc/netem. None of these primitives exist on Windows. They exist inside Docker Desktop's Linux
VM, so containerised nodes can be paused and partitioned; host processes cannot.

The directive calls `proc.pause` "top priority for producing gray failures", and the KV
fixture's stale-read bug is specifically triggered by `proc.pause(role:leader)` overlapping
`net.partition(minority(kv))`. A backend that cannot deliver those two faults cannot
demonstrate the fixture bug, and therefore cannot satisfy the Phase 2 definition of done.

**Consequence.** The `Backend` interface is still defined so `process`, `k8s` and `sim` can be
added later, and the backend is selected by `harness.backend` in config as specified. But
`process` returns a clear "unsupported on this platform" error on Windows rather than silently
degrading. Verified environment: Docker Desktop 29.1.3, Compose v2.40.3, Linux engine,
8 CPUs, 32 GB.

**Logged** as OQ-002 because the directive names `process` as a v1 backend.

---

## D-004: Oracles, the lock manifest, and the regression corpus are committed; only run output is ignored

**Choice.** `.gitignore` excludes `.prothesis/runs/`, `.prothesis/corpus/` and `.prothesis/tmp/`
but deliberately does **not** exclude `.prothesis/oracles/`, `.prothesis/lock`, or
`.prothesis/regressions/`.

**Rationale.** Direct consequence of invariant I6 and the anti-gaming rules. The lock manifest
is the tamper-detection mechanism; an ignored lock file could be silently regenerated. The
regression corpus is specified as append-only, with removal of a `.thesis` regression file
constituting a lock change: an ignored corpus could be silently emptied. Oracles are specified
as "first-class, versioned, hash-locked artifacts managed separately from the target
application code", which requires them to be in version control.

---

## D-005: Repository initialised as a git repository

**Choice.** `git init` on the project directory, which was not previously under version control.

**Rationale.** Required by several normative features rather than by convention: the verdict
schema carries a `commit` field; `thesis bisect WORLD --good SHA --bad SHA` takes git revisions;
`coverage_delta_vs_baseline` is defined against "the baseline commit"; the anti-gaming design
requires that bumping the oracle lock be "a separate, human-reviewed commit"; and the shrunk
regression world is specified as "committed to the repo". None of these are implementable
outside a git repository.

---

## D-006: Addendum A is in force for Phase 4, and is not implemented before Phase 4

**Choice.** `SABOTEUR_INJECTION.md` replaces the base Phase 4 search design and its Phase 4
definition of done (A.9). No Saboteur code is written before Phases 0–3 are complete and
verified.

**Rationale.** Core Implementation Rule 1 forbids implementing phases out of order, and the
rule is load-bearing here rather than bureaucratic: the Saboteur consumes oracle verdicts
(Phase 3), coverage signals (Phase 4 base deliverables, explicitly retained by the addendum),
the Perturber's `Inject`/`Withdraw` API (Phase 2), and telemetry (Phase 1). Building it first
would mean building it against five interfaces that do not exist.

**However**, Addendum A reaches back into Phase 0 in two narrow places that must be handled now,
because Phase 0 freezes formats that Phase 4 would otherwise have to break:
- the `search:` configuration block (A.10) belongs in `pkg/schema` now
- A.8's determinism requirement constrains the `internal/recorder` PRNG stream derivation now

These forward-looking commitments are scoped to *format capacity only*: the fields exist and
round-trip; no Saboteur behaviour is implemented. Specific deltas recorded in later entries.

---

## D-007: `gopkg.in/yaml.v3` is the only dependency

**Choice.** One third-party module, pinned. `pkg/schema` may import it for the config types only;
verdict, world, history and oracle types use `encoding/json` alone, so third parties consuming
`prothesis.verdict/v1` need no dependencies.

**Rationale.** Config is YAML and the standard library has no YAML. Required by D-002's rule that
each dependency be justified here.

**Rejected.** Keeping `pkg/schema` strictly stdlib-only by implementing the int-or-string fields
against yaml's obsolete function-based `Unmarshaler`. That approach was found to be broken: it
relies on `yaml.v3` *failing* to decode an integer scalar into a Go string, which it does not do,
so the directive's own `retain_passing: 3` would be rejected as `CONFIG_ERROR`. The correct
mechanism is `UnmarshalYAML(*yaml.Node)` switching on `node.Tag`, which needs the import.

---

## D-008: Network faults use a run-time `NET_ADMIN` sidecar, not ambassador proxies

**Choice.** `net.*` faults are injected by a sidecar container joined to the target's network
namespace (`network_mode: container:<target>`), running `tc`/`netem` and `iptables`. The fixture's
compose file contains **no** proxies; nodes talk directly over the compose network.

**Rationale.** Measured on this machine: a sidecar injects and withdraws against an *unmodified*
container cleanly (8.3 ms → 311.7 ms → 6.7 ms). This satisfies the requirement to work on
unmodified systems, which ambassador proxies cannot: they require rewriting the target's
topology so peers talk through them. Baking proxies into the *fixture's* compose file would also
make PRO-THESIS's own fault injector depend on target-supplied infrastructure, which is backwards.

Crucially it also **dissolves the Phase 0 / Phase 2 ordering hazard**: the fixture's topology no
longer depends on a Phase 2 decision, so "build the fixture first" and "strict phase order" stop
conflicting.

**Rejected.** Per-edge ambassador proxies. Their one genuine advantage is seeded per-packet
control, since `netem`'s randomness is in-kernel and unseedable: a real tension with I2, recorded
honestly as OQ-009 rather than hidden. They remain available for Phase 2 if the measured
reproduction rate on stochastic network faults proves too low.

**Binding for Phase 2:** partitions must be bidirectional; every injected rule carries a
`thesis:<run_id>:<fault_id>` iptables comment so withdrawal and residual verification match on
ownership; `iptables-save` and `tc qdisc show` are snapshotted at BOOT so `HEAL` compares against
the observed baseline rather than an assumed-empty one.

---

## D-009: The lease bug uses an honest monotonic deadline

**Choice.** `leaseDeadline = monotonicNow() + 5s`, refreshed on each successful heartbeat quorum;
local reads served while `monotonicNow() < leaseDeadline` with no quorum check. Exactly what §5
describes.

**Rationale.** This is the real-world failure mode the fixture exists to model, and it matches the
specification's literal wording. Its consequence is that the triggering pause must be *shorter*
than the lease, which the specification's own reference schedule is not. That conflict is logged
as OQ-010 rather than engineered around.

**Rejected.** Expressing the lease in raft *ticks* to exploit `time.Ticker` collapsing missed ticks
under `SIGSTOP`. It would make the bug fire under the spec's literal 6.9-second window, but: it
deviates from "a 5-second lease"; it makes the anomaly independent of pause duration, inverting
the real failure mode; and it is silently deleted by any future rewrite of the tick loop into a
monotonic-delta catch-up loop, at which point every downstream phase's definition of done becomes
untestable with no visible cause. Bending the fixture to fit a defective schedule is the failure
mode I6 exists to prevent.

---

## D-010: Health probes run host-side; `steady_state` runs in a helper container

**Choice.** `{host}` = `127.0.0.1`, `{port}` = the published host port. One compose **service per
node**, each publishing its own client port. `steady_state.probe` executes via
`docker compose exec` inside a helper container.

**Rationale.** Container IPs are **not routable from the Windows host**: measured directly
(`docker run nginx`, IP 172.17.0.2, request times out). So probing container IPs is not an option,
and "publish no host ports" is unavailable as a parallelism strategy. Running `steady.sh` in a
container also sidesteps the broken Git Bash entirely, and is more correct regardless: steady state
needs an in-network vantage point.

---

## D-011: `t_ns` is Unix epoch nanoseconds; only the harness writes phase markers

**Choice.** Drivers emit `time.Now().UnixNano()`. The harness records `drive_origin_wall_ns` once
at DRIVE start as the conversion anchor. Drivers write **only** op records to `{history_path}`;
the harness writes phase markers to `phases.jsonl`; the recorder merges them by timestamp at
TEARDOWN into the canonical `history.jsonl` that `oracle_input.history_path` names.

**Rationale.** The field name is normative and says nanoseconds; §4.4's example values are
microseconds and are simply wrong (OQ-008). The anchor is what makes `oracle_input`'s
`start_ms`/`end_ms` computable, and therefore what makes I5 phase-aware assertion evaluable at all.

The single-writer rule exists because §4.4 shows phase markers interleaved with op records while
§4.2 hands the history path to an *external process*. Two writers appending to one file across the
Docker Desktop VM / Windows bind-mount boundary gives torn lines, and one torn line makes the
entire history unparseable: surfacing as a spurious `INCONCLUSIVE` that reads like an environment
flake rather than a bug.

---

## D-012: `.thesis` canonical encoding, and the planned/realized fault schedule

**Choice.** Canonical JSON: byte-order-sorted keys, no insignificant whitespace, LF only, one
trailing newline, HTML escaping off, **no bare floats** (ratios are integer parts-per-million), no
`map` or `interface{}` in the type graph, strict decoding, and `Load` re-encodes and byte-compares
on every call so byte-identity is a runtime invariant rather than only a test assertion.

`fault_schedule` splits into `planned` and `realized`.

**Rationale.** Each rule closes a specific failure: Go escapes `<` and `>` by default, which would
corrupt the edge target `n1<->n2` and change every world hash; map iteration order is randomized;
`interface{}` decoding turns integers into `float64` and loses precision above 2⁵³; Go's
shortest-float representation carries no cross-version stability guarantee, so a bare float would
break every committed world on a toolchain upgrade.

The planned/realized split is required by the **base** specification, independent of Addendum A:
`role:leader`, `minority(kv)` and `kv:*` bind to concrete nodes at injection time from live state,
and Raft leadership moves at runtime. A world recording only `proc.pause(role:leader)` does not
reproduce. `realized` is what makes I2's "self-contained file that reproduces the issue" true
rather than aspirational.

---

## D-013: PRNG streams are path-keyed, so new streams cannot perturb old ones

**Choice.** `key = HMAC-SHA256(rootSeed, domainPath)`, keying a stream cipher. A stream's key
depends on its path alone. Value-extraction primitives are written in-repo and pinned by a
golden-vector test.

**Rationale.** The required property is that **adding a new named stream must not change any value
produced by an existing stream for the same root seed**. Sequential child-seeding violates it, and
the consequence is severe: every committed regression world would be invalidated the moment
Phase 4 adds the Saboteur's streams. Path-keying makes the independence structural rather than
incidental.

`math/rand` is avoided for value extraction because neither it nor `math/rand/v2` guarantees a
stable value sequence across Go versions, and every archived world depends on that sequence.

---

## D-014: Phase 0 freezes formats, not behaviour

**Choice.** Phase 0 ships the `search:` config block (A.10) and path-keyed PRNG derivation, and
nothing else from Addendum A. Explicitly **not** built: telemetry format, `thesis doctor`,
retention, coverage extraction, the lock manifest, any oracle, and MCTS provenance fields.

**Rationale.** D-006 fixed the permitted reach at exactly two items and this holds it there. The
forward-compatibility argument for adding MCTS provenance now was examined and found **false**:
`WorldMeta` is excluded from `world_hash` and every field is `omitempty`, so those fields can be
added in Phase 4 without changing any world's hash or invalidating a single committed world. An
argument that does not survive its own premises is not a reason to smuggle Phase 4 design into a
frozen Phase 0 format.

---

## D-015: The Saboteur's value is the ladder and early termination, not the tree

**Choice.** Recorded now so Phase 4 is not built on a false premise: at the budgets Addendum A
specifies, the MCTS tree is a tie-breaker. The escalation ladder (A.7) and early termination carry
the search.

**Rationale.** At ~16 serial rollouts against a root branching factor near 249, UCT is *provably
equivalent* to ladder-ordered enumeration: the exploration term is infinite at zero visits, so
unvisited children are always selected first, no node is ever visited twice, and backpropagation
never influences a single decision. A.5's own pruning rule ("visited ≥3 times") is unreachable.
Full arithmetic in OQ-013.

This is not a reason to drop the tree: with the measured 3.8× parallel speedup the rollout budget
rises to ~62, where the tree does begin to earn its place. It is a reason to state plainly which
component is doing the work, so nobody later tunes an exploration constant that cannot matter.

---

## D-016: The Docker CLI is the only engine interface; never the Go SDK

**Choice.** `docker compose` for project lifecycle and `docker` for per-container operations,
invoked as subprocesses. No `github.com/docker/docker/client`.

**Rationale.** Verified on this machine: `DOCKER_HOST` is unset and the current context is
`desktop-linux`. The SDK does not resolve Docker *contexts*, so it would dial the default named
pipe, miss the one Docker Desktop actually listens on, and fail in a way indistinguishable from a
dead daemon. Compose v2 is additionally a CLI plugin with no stable Go API at all. The SDK is also
a heavy dependency with a painful version matrix, against D-002's posture.

**Consequence accepted.** The CLI's JSON output is an unversioned contract: `compose ps --format
json` emits a JSON array on some versions and one object per line on others. Both shapes are
parsed rather than pinning a version that cannot be enforced on a user's machine.

---

## D-017: `up` persists its handoff before reporting success; `down` verifies its own work

**Choice.** `up` writes `topology.json` (compose project name, overlay path, node→container
bindings, published ports) and the `CURRENT` pointer *before* printing success, and deliberately
leaves the run bundle **unclosed**: the `RUNNING` sentinel is what marks a run live. `down` reads
that record, unpauses, tears down, sweeps by label, verifies emptiness, then closes the bundle and
clears the pointer.

**Rationale.** `up` and `down` are separate OS processes. Everything needed to address what `up`
created exists only on disk; if that write failed and `up` still reported success, the topology
would be unreachable by any later command. So a failed persist tears the topology back down rather
than leaking it.

`down` unpauses first because a **paused container cannot be stopped**: `compose down` blocks for
the full timeout and then leaves it running, leaking both the container and its network. Phase 2's
`proc.pause` is `SIGSTOP`, so an interrupted run leaves exactly that state, and coping with an
interrupted run is what `down` is for.

`down` verifies rather than assumes because this host has a **hard ceiling of 24 free Docker bridge
networks** and a leaked project consumes one permanently. A search leaking one network per world
exhausts the pool within a couple of dozen worlds and then fails in a way that looks like a Docker
bug. Teardown reliability is a search-scalability property here, not hygiene.

---

## D-018: A health probe that matches no node is an error, never a vacuous pass

**Choice.** `WaitHealthy` fails when a probe's target selector resolves to zero nodes.

**Rationale.** Found the hard way: the fixture's own `prothesis.yaml` used the directive's `kv:*`
wildcard, which resolved to nothing under a one-service-per-node topology. A literal reading
health-checked **zero nodes** and would have reported the cluster up when it had never formed:
`up` returning PASS over a dead system, which is the most dangerous outcome this tool can produce.
The Phase 0 definition of done says `up` boots the fixture *cleanly*, and "cleanly" is only
observable through the probes; a probe set that vacuously passes makes the definition of done
unfalsifiable.

Phase 2 selector forms (`role:`, edges, quorums) are rejected with a specific message rather than
silently resolving empty, for the same reason.

---

## D-019: The Phase 2 reference schedule is amended to a 2.8-second pause

**Choice.** The Phase 2 definition of done uses:

```
proc.pause(role:leader)@8200..11000          # 2.8 s pause  <  5 s lease
net.partition(minority(kv))@8400..10900      # overlapping
```

replacing the directive's `@8200..15100` / `@8400..14900`. The lease implementation is untouched.

**Rationale.** The original window is a 6.9-second pause against a 5-second lease, and
`CLOCK_MONOTONIC` advances while a process is `SIGSTOP`ped, so the lease expires during the pause
and the resumed ex-leader correctly refuses the read. The schedule is incapable of producing the
anomaly it is cited to produce (OQ-010).

2.8 s is bounded on both sides. It is long enough: the minimum election timeout is 600 ms, so the
majority elects a new leader and commits in term 2 well inside the window. It is short enough: the
displaced leader resumes with ~2.2 s of lease remaining and serves the stale local read.

**Empirically grounded, not assumed.** `provebug` already defaults to `--pause 2800ms`
("must be < the lease"), and its committed witness records the anomaly: *"process 1 read k/42=7
from kv-n2 under a leader read lease at t+1715ms, 1ms after op_id 90002 committed k/42=9 in term 2
on kv-n1 … the displaced leader kept answering local reads for 4327ms after it was replaced."*

**Rejected.** Expressing the lease in raft ticks so the literal 6.9 s window fires. See D-009:
it deviates from "a 5-second lease", inverts the real failure mode, and is silently deleted by any
future rewrite of the tick loop.

---

## D-020: `service` is the logical group; `compose_service` is the physical one

**Choice.** `harness.nodes[]` gains an additive `compose_service`. `service` is the LOGICAL group
the frozen grammar targets (`kv:*`, `minority(kv)`, `majority(kv)`, `any(2, kv)`, and the
constraint vocabulary); `compose_service` is the physical compose service that publishes a port.
Empty means "same as `service`", so the directive's §4.2 sample and any single-service topology
are unchanged. Backends address `EffectiveComposeService()`, never `Service`.

**Rationale.** Resolves OQ-016. Container IPs are not routable from a Windows host, so each node
must publish its own 127.0.0.1 port, which forces one compose service per node. But the frozen
grammar resolves `minority(kv)` over nodes whose `service` is `kv`. Those two requirements are
only satisfiable together if the logical group and the physical service are separate fields.

**Rejected.** Reinterpreting the token before the colon as a prefix glob so `kv:*` matches
`kv-n1`. It avoids the new field but silently redefines `minority(kv)`, `majority(kv)` and the
frozen constraint string: changing the meaning of a normative grammar to dodge an additive field.

**Verified end to end.** The fixture now uses the directive's own `kv:*` wildcard; all three
health probes resolve and pass, and containers carry `io.prothesis.group=kv` so Phase 2 can select
a quorum by label without re-reading `prothesis.yaml`.

**Validation subtlety worth keeping.** Explicit duplicate `compose_service` values are a config
error: the author named one physical service twice, so a fault between those nodes would be a
silent no-op. Implicit sharing (several nodes with one `service` and no `compose_service`) is NOT
rejected, because that is precisely the directive's §4.2 sample describing a scaled service.
Whether it is resolvable depends on the backend, so it is diagnosed at bind time by the backend
that knows, not by the schema that does not.

---

## D-021: `{plan_path}` is reserved now, with an environment fallback

**Choice.** `PlaceholderPlanPath` (`{plan_path}`) and `PlanPathEnv` (`PROTHESIS_PLAN_PATH`) are
defined in `pkg/schema`. The driver supervisor substitutes the placeholder when present and
ALWAYS exports the environment variable.

**Rationale.** Resolves OQ-012. Phase 5's `ddmin` over the operation trace must hand the driver an
explicit reduced op list; `{seed}` regenerates the whole stream and `{profile}` names a profile,
so no frozen placeholder can express it. The directive freezes schema FIELD NAMES and nowhere
declares the placeholder vocabulary closed, and `driver.cmd` is an opaque template string, so
widening it is additive.

Dual transport exists so a driver that does not spell `{plan_path}` in its command line can still
find the plan. A driver supporting neither ignores both, and op shrinking is then reported as **not
attempted** for that target rather than silently producing a wrong minimal repro.

**Scope.** Reserved in Phase 0; substitution lands with the Phase 1 driver supervisor; the
fixture's `loadgen` grows `--plan` when Phase 5 needs it. Note the earlier claim that loadgen
already supports plan replay was checked and is **not** correct: its flags are `history`,
`profile`, `seed`, `clients`, `ops`, `keys`, `duration`, the timeouts, `think-max-ms`,
`lease-read-ppt`, `stdin-control` and `quiet`. Building it now would be implementing Phase 5 out
of order.

---

## D-022: Search economics close through parallelism, not by skipping HEAL

**Choice.** Phase 4 runs worlds across concurrent compose projects (`thesis-<run_id>-w<N>`).
Probe worlds shorten the post-`HEAL` window rather than removing it. `HEAL` and `TEARDOWN` run on
every path without exception.

**Rationale.** Measured: 8 concurrent projects give 3.8× on orchestration for +3–5 % timing
distortion, which brings the 22-world Tier 1 sweep inside its 120 s budget and raises the rollout
count from ~16 to ~62; the point at which the MCTS tree stops being decorative (D-015, OQ-013).
Four-way concurrency uses 8 of this host's 24 available bridge networks, leaving headroom.

**Amendment to the proposed plan, and the reason for it.** Probe worlds cannot drop the
post-withdrawal observation window entirely. Two of Addendum A.3's four REINFORCE criteria are
defined *after fault withdrawal*: *"Queue depth monotonically increased for ≥3 consecutive samples
after fault withdrawal"* and *"Error rate increased after fault withdrawal (the system got worse
after the fault ended)"*. A probe with no post-`HEAL` observation can never evaluate them, so it
would classify on half its criteria and systematically under-report REINFORCE: losing exactly the
signal the probe sweep exists to find.

What a probe genuinely does not need is the **convergence grace window sized for the 5-second
lease**, because probes classify telemetry rather than assert convergence. So the lease-sized
`QUIESCE` is dropped and a short post-`HEAL` observation window (~1.5 s) is kept. That preserves
most of the saving and all of the signal.

`HEAL` is never skipped, including on early termination. Skipping it leaves live `iptables` rules
and `SIGSTOP`ped containers that poison every subsequent rollout: triggered, perversely, by
success (OQ-004).

---

## D-023: The race detector runs in a container, not on the host

**Choice.** `go test -race` runs inside `golang:1.22` with the workspace bind-mounted and
`CGO_ENABLED=1`. Nothing is installed on the Windows host.

**Rationale.** Resolves OQ-018. The race detector needs cgo and a C compiler; this host has
`CGO_ENABLED=0` and no gcc. Docker Desktop's Linux VM is already a hard dependency of the compose
backend, so it costs nothing extra, and ThreadSanitizer is far better exercised on Linux.

**Result.** Both modules pass race-clean. This also confirms `go.mod`'s `go 1.22` declaration is
honest: the tree genuinely builds and passes on 1.22, not merely on the 1.27.1 toolchain that
wrote it.

Note `sh -lc` does not work for this: a login shell re-runs `/etc/profile` and drops Go from
`PATH`. Use `sh -c`.

---

## D-024: `clock.skew` uses Linux time namespaces; libfaketime cannot work

**Choice.** `clock.skew` and `clock.jump` are injected by starting the target in a Linux time
namespace with a monotonic offset, via an entrypoint override the harness supplies:

```
docker run --cap-add SYS_ADMIN --cap-add SYS_TIME \
  --entrypoint unshare <image> --time --monotonic=<seconds> --fork --pid --mount-proc <argv...>
```

**Rationale: measured, not reasoned.** The directive proposes libfaketime via `LD_PRELOAD`.
It does not work on Go binaries, and the test included a control to prove libfaketime itself
was functioning:

| binary | wall clock under `FAKETIME="+3600s"` |
|---|---|
| C program (control) | 1788764420 → 1788768020: **shifted +3600s** |
| Go, `CGO_ENABLED=0` | unchanged |
| Go, `CGO_ENABLED=1` | unchanged |

The Go runtime reads `CLOCK_MONOTONIC` and `CLOCK_REALTIME` through the vDSO rather than libc,
so `LD_PRELOAD` never sees the call, and enabling cgo does not help, because it is the runtime's
own clock path that bypasses libc, not the linkage.

Time namespaces do work, measured on `runtime.nanotime`, the exact clock the fixture's lease is
a deadline against:

| | monotonic (ms) | delta |
|---|---|---|
| no namespace | 35,191,383 | n/a |
| `--monotonic=3600` | 38,791,385 | **+3,600,002 ms** |
| `--monotonic=-300` | 34,891,388 | **−299,995 ms** |

Both directions, exact, on an **unmodified image**, without `--privileged`, under default seccomp.
`CAP_SYS_ADMIN` (create the namespace) and `CAP_SYS_TIME` (set the offset) are both required;
either alone fails with `Operation not permitted`.

**Two constraints that must be stated, not discovered later.**

1. **Monotonic and boottime only, never wall clock.** `wall_unix` was identical across all three
   measurements. Logic keyed on `CLOCK_REALTIME` (calendar TTLs, certificate expiry) cannot be
   attacked this way. The fixture's read lease is a monotonic deadline, so the case the fixture
   exists to demonstrate is covered; a wall-clock skew primitive is not available and must not be
   claimed. Logged as OQ-019.
2. **The offset is fixed at namespace creation.** The kernel accepts writes to
   `timens_offsets` only before any process exists in the namespace, so a clock fault cannot be
   injected into an already-running process. `clock.skew(...)@8200..15100` is therefore
   implementable only as a restart into a skewed namespace, which entails a `proc.restart`. That
   is honest and Raft tolerates it, but it is a different physical event from what the `@WINDOW`
   grammar implies, and Phase 2 must present it as such rather than pretending otherwise.

**Rejected.** `date -s` inside a privileged container. It succeeded in testing, which is precisely
the danger: Docker Desktop's containers share one kernel clock, so it would move *every* node's
clock at once and leak into the host VM. A per-node fault must not be global.

---

## D-025: netem is seeded from the recorder, so stochastic network faults replay

**Choice.** Every `net.loss`, `net.reorder` and `net.duplicate` injection passes an explicit
`seed` drawn from the recorder's path-keyed PRNG stream:

```
tc qdisc add dev eth0 root netem loss 30% seed <derived-from-world-seed>
```

**Rationale.** OQ-009 was logged on the premise that netem's randomness is in-kernel and
unseedable, permanently weakening invariant I2 for three fault kinds. **That premise is false on
this kernel** (6.6 WSL2, iproute2 v7.0.0). Measured:

- `netem loss 30% seed 12345` is **accepted**
- the kernel **stores it faithfully**: asked 11111 → reports 11111; 22222 → 22222; 11111 → 11111
- with **no** seed given it picks a fresh random one every time (12367817659504283901,
  11126548977008843825, 17522413185903285832) which is exactly the nondeterminism OQ-009
  described, and exactly what passing a seed avoids

**Consequence.** The strongest argument for per-edge ambassador proxies disappears. Proxies were
kept in reserve (D-008) specifically because seeded per-packet control might be needed for I2;
the kernel provides it. The sidecar remains the mechanism, and stochastic network faults join the
deterministic ones in being replayable.

**Caveat to verify in Phase 2:** the seed is stored, and identical seeds should give identical
drop sequences, but that was not measured end to end; the probe could not shape loopback traffic
through the eth0 qdisc. Phase 2's first task for `net.loss` is to confirm the reproduction rate
empirically and report it in `reproduced: "k/n"` rather than assuming it is 3/3.

---

## D-026: Every injected fault carries an ownership tag, and HEAL verifies against a baseline

**Choice.** Injected `iptables` rules carry `-m comment --comment "thesis:<run_id>:<fault_id>"`.
Withdrawal matches on the comment rather than reconstructing the rule spec. `HEAL` compares
against an `iptables-save` / `tc qdisc show` snapshot taken at `BOOT`, not against an assumed-empty
table.

**Rationale: rehearsed end to end before Phase 2 depends on it.** A `NET_ADMIN` sidecar joined to
an unmodified container's network namespace injected a bidirectional partition and a netem delay,
withdrew both by tag, and verified zero residue. The rehearsal also demonstrated why the naive
version fails: after installing an untagged rule to stand in for one the target owns, an
assumed-empty residual check reports a violation while the tag-scoped check correctly reports
clean. Under the requirement to work on unmodified systems, a target may legitimately ship its own
firewall rules, so an assumed-empty check is either unimplementable or a false-failure generator.

Two details that are easy to get wrong and were fixed in the rehearsal:
- **A partition must be bidirectional.** A single `-I INPUT -s peer -j DROP` is a one-way drop:
  the peer still receives our traffic, so the cluster does not see a symmetric partition.
- **Withdraw by tag, never by rule spec.** Reconstructing the spec fails if the rule was already
  partially removed, and then `HEAL` cannot clean up after its own partial failure.

---

## D-027: `harness.nodes[].port` binds the frozen `{port}` placeholder

**Choice.** An additive `port` field on `harness.nodes`, defaulting to 8080
(`schema.DefaultClientPort`). It is the **container** port; the published host port stays
compose-assigned and discovered at bind time.

**Rationale.** Resolves OQ-017. The frozen probe template is `"http://{host}:{port}/healthz"`, but
nothing in the config said which port a node serves, so the backend had to assume one. The
assumption holds for the directive's own sample and the fixture, both 8080, and breaks on the
first real target that differs. Pinning the *host* port in config instead would make two concurrent
runs collide, which Phase 4's parallelism depends on avoiding.

---

## D-028: Chosen readings for Addendum A's defective machinery

Addendum A.6 says its utility function "is normative and must not be modified by the implementing
agent without recording the change in `DECISIONS.md`." This is that record. Each item is a defect
found by reading the addendum against itself; each reading below is the one that preserves the
addendum's evident intent. **None is implemented yet** (Phase 4 owns them) but they are settled
now so Phase 4 is not built on ambiguity.

**1. A.5's UCT formula divides an average by visits a second time.** It states
`UCT(node) = (U_avg / visits) + C × sqrt(ln(parent.visits) / visits)` while defining `U_avg` as
"average utility across all rollouts through this node". Dividing an average by the visit count
again drives the exploitation term toward zero exactly as a node accumulates evidence: the
opposite of what UCT does. **Reading taken:** the standard form,
`U_avg + C × sqrt(ln(parent.visits) / visits)`. This is a transcription slip, not a design choice:
no bandit algorithm penalises a node for being sampled.

**2. A.6's class switch has no `default`, so two normative classes score zero.** `differential`
and `metamorphic` are in the eight-value `OracleClass` vocabulary but absent from the switch, so a
violation of either contributes exactly 0.0 and is invisible to the search. **Reading taken:** a
`default` scoring 20.0, matching the lowest named tier (`liveness`/`convergence`). An unrecognised
class scoring zero means the Saboteur cannot see a bug it just found, which cannot be the intent.

**3. A.6's parsimony claim is false as written: FLAGGED, NOT SILENTLY CHANGED.** Its "Key
property" paragraph argues the function always prefers the minimal path. It does not: a novel log
template is worth 10 and a fault costs 3, so a 12-fault world with 5 novel templates scores
`150 + 50 − 36 − 12 = 152` against the minimal world's `150 + 0 − 6 − 3 = 141`. Novelty outweighs
parsimony at 3.33 faults per template. The *arithmetic in the paragraph* is right for the two
worlds it compares; the *general claim* it draws is wrong. **The weights are left exactly as
specified**: they are normative, and the behaviour may well be wanted, since novelty is what
drives coverage. What is corrected is the claim about them. Raised for a human ruling in OQ-014;
if minimality is genuinely required, `fault_penalty` must exceed `novelty_weight`, which is a
change only the author should make.

**4. A.7 Rung 3 wants an asymmetric partition the frozen grammar cannot express.** `TARGET`
offers node, wildcard, role, edge (`n1<->n2`) and quorum forms; all are symmetric. **Reading
taken:** Phase 2 implements symmetric partitions, and an asymmetric form needs an additive
directed target (`n1->n2`) which is a grammar extension, not an implementation detail. Rung 3's
"asymmetric preferred" is treated as aspirational until that exists.

**5. A.6 needs a `severity` that `prothesis.oracle_output/v1` does not define.** Severity
multiplies the entire class hierarchy (×1.5 / ×1.0 / ×0.5), so it is load-bearing, yet the frozen
oracle output envelope has no such field. **Reading taken:** severity is DERIVED from class by
the engine, using the directive's one normative pairing (`consistency` → `high`) as the anchor;
an oracle may override it through an additive `severity` member in its output, which older oracles
simply omit. Deriving keeps third-party oracles working; requiring the field would break every
oracle written against the frozen envelope.

**6. A.3's probe sweep drops the no-fault control world.** Every DAMPEN/REINFORCE criterion is
comparative ("returned to baseline", "exceeded 2× baseline", "got worse after the fault ended"),
so a baseline is required. CRUCIBLE §G's seed corpus explicitly includes "one no-fault control".
**Reading taken:** the control world is restored to the sweep. Without it the classifier has no
baseline and every criterion is unevaluable.

**7. A.10's `strategy: saboteur` default would make `thesis gate` run an adaptive search.**
§4.2 enables search on the `soak` profile only (`search: true`), and the gate is a fixed-budget
pre-commit check whose whole value is being a repeatable signal. **Reading taken:** the top-level
`search.strategy` selects WHICH search runs, and the profile's `search` boolean decides WHETHER
one runs at all. `thesis gate` never runs MCTS.

**8. The top-level `search:` block is an input to `.prothesis/lock`.** Otherwise it is the easiest
gate-weakening vector in the system: `probe_budget_pct`, `max_mcts_depth` and every utility weight
are reachable without touching a single oracle. CRUCIBLE §E.2 already makes budgets lock changes;
this is the same rule applied to the block that supersedes them.

---

## D-029: Phase 4's benchmark is pre-registered; Phase 5's shrink is budgeted

Both come from OQ-007 items that were "quantification in progress".

**Phase 4's statistical claim (A.9 #4).** "Median time-to-first-violation ≤ 50% of uniform random"
over 10 trials is not a testable claim as stated: time-to-first-violation is **right-censored**
(a trial that never finds the bug has no finite time), and a median over censored data is
undefined. **Decision:** the comparison is a **one-sided Fisher's exact test on the count of
trials that found a violation within the budget**, pre-registered before the search loop is
written; direction, alpha (0.05), per-arm world budget, and fixture difficulty all fixed in
advance. At n=10 per arm this has real power: 9/10 vs 2/10 gives p = 0.0027, 10/10 vs 0/10 gives
p = 5.4e-6. Where both arms find it every time, the secondary comparison is Mann-Whitney U on
time-to-first-violation over the uncensored subset, reported with the censoring rate. Registering
this in advance is the point: a benchmark whose test is chosen after seeing the numbers is not
evidence.

**Phase 5's cost.** Naive `ddmin` over 14 faults, then 20,000 ops, then binary-search window
narrowing, then 3× confirmation, is an estimated 9–52 hours at a measured ~29s per world, which
contradicts I7's "every invocation takes a budget and returns a verdict". **Decision:** shrinking
takes an explicit budget like every other operation, defaulting to a fraction of the run budget,
and emits a PARTIAL shrink with `attempted: true` and the honest before/after counts when it
expires. A half-shrunk repro that says so is useful; an unbounded shrink that never returns is not.
The `shrink` object in the verdict already has the fields to express this.

---

## D-030: `suspect` is null unless there is a defensible basis

**Choice.** `violations[].suspect` is emitted as `null` in every phase before there is a real
basis for it, and is never populated with an invented confidence number.

**Rationale.** The verdict schema asks for suspected source files plus a `confidence` float, on the
basis of "log-template locality + blame over shrunk timeline window". That is a research-grade
capability, and the field's danger is specific: a coding agent reads this verdict and will open the
files it names. A fabricated 0.42 pointing at the wrong file costs more than an absent field,
because the agent has no way to tell a guess from a finding.

**The minimum honest version, when it arrives** (Phase 5 at the earliest, since it depends on a
shrunk timeline window): files ranked by the intersection of git-blame over the shrunk window and
the log templates that appear only in violating worlds, with `confidence` reported as the measured
separation between violating and passing corpora, not a hand-tuned constant. Until that exists,
`null` is the truthful answer.

---

## D-031: The Phase 2 acceptance schedule, corrected a second time and proven

**Choice.** The schedule that reproduces the fixture's stale read is:

```
net.partition(role:leader)@8200..13500     # isolate the leader, OUTLASTING the pause
proc.pause(role:leader)@8300..11000        # 2.7 s pause  <  5 s lease
```

**This is a second, independent defect in the directive's reference schedule**, on top of the
timing error already recorded as D-019/OQ-010. Both had to be fixed before the anomaly could be
observed, and each was found by running the thing rather than reading it.

**What the directive specifies, and why it cannot work.** §6 Phase 2 pairs
`proc.pause(role:leader)` with `net.partition(minority(kv))`. `minority(kv)` selects *an*
arbitrary minority: measured, it resolved to `kv-n1`, a follower. Pausing the leader *and*
partitioning a follower removes two of three nodes from the connected majority, so the survivors
cannot reach quorum, no election completes, no write is committed in a new term, and there is
nothing stale to read. Measured directly: with that schedule the term advanced only at
t+12270 ms, *after* the pause was withdrawn at t+11320 ms, and zero stale reads occurred.

**Why the partition must target the leader.** Isolating the leader leaves `kv-n1`+`kv-n3`, a
majority, free to elect. Measured with the pause alone: term 63→64 at t+9287 ms, *inside* the
pause window. Quorum preserved, election completed.

**Why the partition must OUTLAST the pause.** Pause alone still yields no stale read: the instant
the ex-leader unfreezes, the new leader's heartbeat reaches it, it learns of the higher term and
steps down before serving anything. The displaced leader must be unable to *learn* it was
displaced while a client can still reach it, which is exactly what the fixture's split
client/peer networks exist to permit, and what §4.3's note about partitioning the peer plane
rather than the client plane is for.

**Measured result with the corrected schedule** (run `r_2026_09_08_fcf4`):
- election: term 65 → 66 at t+9408 ms, inside the pause window
- **23 lease reads served by `kv-n2` at term 65 while the cluster was at term 66**, the first at
  t+11084 ms: 84 ms after the pause lifted
- they continue to t+12451 ms and stop when the partition lifts at 13500 ms
- 14 of them returned a value superseded by a higher-index commit

That is the anomaly, with the timing signature the mechanism predicts.

**Scope of the claim.** Phase 2 has no consistency oracle (that is Phase 3) so what is proven
here is that the SCHEDULE EXECUTES and the ANOMALY OCCURS, evidenced from the history log and the
nodes' own term reporting. Detecting it automatically, and emitting a `linearizable.kv` violation
with witness op ids, is Phase 3's definition of done and is NOT claimed.

**Recorded in OQ-021b.** The directive's schedule is left in the document as written; this entry is
the correction, not a silent edit.

---

## D-032: Fault injectors are rebuilt per world, never cached across them

**Choice.** `control.BuildInjectors` runs once per world, at BOOT, and its result is discarded at
teardown. `RunnerOptions.Injectors` remains as an explicit override, which is the seam tests use
to substitute fakes.

**Rationale.** Found by running it: caching the set across worlds produced
`docker inspect: no such object: <id>` mid-window in worlds 2 and 3. Every world tears its
topology down and boots a fresh one, so every container id changes; an injector built for world 1
addresses containers that no longer exist. The failure mode is the worst shape available: it
lands *after* the schedule has committed to a window, so the fault silently fails to inject at the
moment it was supposed to fire.

The BOOT baselines have the same lifetime for the same reason: "clean" is defined against the
containers *this* world actually booted, so a baseline carried over from a previous world would
be judging residue against a topology that no longer exists.

---

## D-033: `--budget` and `--worlds` may only narrow a profile

> **PARTLY SUPERSEDED BY D-059.** The choice below stands: both flags exist and both may only
> narrow. **The final sentence of the rationale is wrong**, and it was wrong for three phases: it
> contradicts the sentence two paragraphs above it, and the code written from it let
> `--worlds 1` cut a 30-world locked gate to one world, exit 0, and report `oracle_lock: ok`.
> Read D-059 before relying on anything here. Measured in OQ-059.

**Choice.** Both flags are implemented (they are in the directive's CLI surface and were missing),
and both are clamped: they can shorten a wall budget or reduce a world count, never extend either.

**Rationale.** A profile's budget is lock-covered precisely because shortening it is a
gate-weakening move (CRUCIBLE §E.2). The mirror case is worse: an agent that could *widen* a
budget from argv would never need to touch `prothesis.yaml` at all, so the lock would guard a file
nobody edits. ~~Narrowing is safe in the direction that matters; it can only make the gate harder
to pass.~~ **This last clause is retracted by D-059.** "Harder to pass" is the right intuition for
a budget you must fit work into and the wrong one for a search: fewer worlds is fewer chances to
find the defect. The clause is struck rather than deleted so that the error, and the fact that this
document contained its own refutation two paragraphs earlier, stay readable.

---

## D-034: Every oracle's finding is recorded, not only the violations

**Choice.** `result.json` carries one entry per oracle (name, class, status, valid phases,
observed phase, explanation, and the error when one could not evaluate) including oracles that
returned `ok`.

**Rationale.** An INCONCLUSIVE verdict with zero violations was unactionable. The exit-code
contract says code 2 means "retry once, then escalate to a human", and a human cannot escalate a
verdict that does not say which oracle could not evaluate or why. Recording the `ok` results
matters for the same reason in the opposite direction: "checked and satisfied" and "never ran" are
different facts, and only one of them supports a PASS.

Immediately useful; the first run with it explained an INCONCLUSIVE that had been opaque:
`no_stuck_op` reported *"16 operation(s) were still in flight when observation ended at t+18928ms
before the 5000ms SLO ceiling elapsed"*. The oracle was right to refuse: it could not distinguish
a stuck operation from one that was simply not yet due, so it declined to pass.

---

<!-- Subsequent entries appended as subsystem designs are finalised. -->

---

## D-031b: `proc.slow` throttles through the CFS quota, never through `--cpus`

**Choice.** `proc.slow(cpu_pct)` sets `--cpu-period` / `--cpu-quota` when the container's
`HostConfig.NanoCpus` is 0, and falls back to `--cpus` only when it is not. Withdrawal restores the
recorded BOOT baseline, or clears the quota with `--cpu-quota -1` when there was no baseline quota.
`--cpus 0` is never emitted.

**Rationale: measured, not reasoned.** `docker update --cpus` is the obvious route and it does
throttle, but it CANNOT BE UNDONE:

| command | `HostConfig.NanoCpus` | `/sys/fs/cgroup/cpu.max` inside the container |
|---|---|---|
| `--cpus 0.25` | 250000000 | `25000 100000` |
| `--cpus 0` | **still 250000000** | **unchanged** |
| `--cpu-quota -1` | 250000000 (stale) | **`max 100000`**: cleared |

The daemon reads 0 as "no change", so a container that started unlimited can never be returned to
unlimited through `--cpus`. That is a permanent throttle leaked into every subsequent world on that
container: precisely the class of leak directive 4.3's Critical Guarantee exists to prevent, and
one that would surface much later as a mysteriously slow node.

The quota/period pair is only usable while `NanoCpus` is unset: once it is set the daemon refuses
with "CPU Period cannot be updated as NanoCPUs has already been set". Hence the two branches. Both
have an exact inverse and both are verifiable through `docker inspect`, which is what the process
injector's `VerifyClean` checks.

**One residue is accepted and stated rather than hidden.** On the quota branch, withdrawing with
`--cpu-quota -1` leaves `HostConfig.CpuPeriod` at the value we set where the baseline had 0. A
period with no quota does not throttle (the cgroup reads `max 100000`) so the residual check
compares the quota and `NanoCpus`, not the period.

---

## D-032b: `mem.pressure` shrinks the limit rather than holding memory resident, and refuses where it cannot be withdrawn

**Choice.** `mem.pressure(pct)` reduces the target's memory limit to `(100-pct)%` of its original
value, so that `pct` of the original limit is no longer available. It is refused, with
`ErrUnsupported`, against a container that declares no memory limit.

**Rationale.** The frozen registry describes the kind as "hold a percentage of the target's memory
limit resident". Holding memory resident needs an allocator running inside the target's own cgroup,
which the Docker CLI cannot arrange for a container that is already running. Shrinking the limit
produces the same condition from the other side (reclaim pressure, then the OOM killer) and it has
an exact inverse. This is a MECHANISM deviation from the registry's wording; it changes no schema
field name, and it is recorded here, in the code, in the realized schedule and in OQ-022 rather than
being left for a reader to discover.

The refusal is not a preference. Measured: `docker update --memory 0` is a no-op and
`--memory -1` is rejected by the CLI, so a limit placed on an unlimited container cannot be removed.
Injecting there would create a fault with no withdrawal, and "every fault must implement a safe
withdraw()" is the one rule this family has no discretion over.

---

## D-033b: the process, clock and I/O families are `Primitive`s behind three `perturber.Injector`s

**Choice.** `internal/perturber/faults` carries two mechanisms with two shapes. The `net.*` family is
a `Fault` driven by a NET_ADMIN sidecar (D-008). The process, clock and I/O families are
`Primitive`s driven by the Docker CLI directly, exposed to the executor through
`ProcessInjector`, `ClockInjector` and `IOInjector`, each implementing `perturber.Injector`.

**Rationale.** The two mechanisms genuinely differ: a network fault is a script run inside somebody
else's namespace and withdrawn by an iptables comment, while a process fault is a daemon-level state
change on the container object itself and is withdrawn by restoring an inspected value. Forcing one
type over both would make the sidecar's ownership-tag machinery meaningless for `docker pause` and
the container-baseline machinery meaningless for `iptables`. `perturber.Injector` is already the
contract that unifies them, and `perturber.NewRegistry` already refuses two injectors for one kind,
so the kind space is partitioned by construction rather than by convention.

**Consequence for withdrawal.** `perturber.Injector` requires that withdrawal "never depend on
in-process bookkeeping that a crashed or restarted harness would have lost". Every primitive here
therefore decides what to undo from OBSERVED state (is it paused, is the entrypoint one we
installed, does the ballast file exist, does the limit differ from the baseline) and the
irreducible remainder (what the CPU quota, memory limit and descriptor limit WERE) lives in
`ContainerBaseline`, a serializable BOOT snapshot, which is the same artifact D-026 already requires
HEAL to judge residue against.

**`io.latency` and `io.error` are CLAIMED by `IOInjector` even though it cannot inject them.**
Leaving them unclaimed would make the executor report `ErrNoInjector`: true, but silent about why.
Claiming them and failing with the measured reason turns a dead end into an instruction. Callers
wanting the schedule rejected before a world is booted pre-flight every kind through
`faults.PlatformCapability`.


---

## D-035: the lock digests the USER-SUPPLIED config text; compiled-in tuning is recorded, not digested

**Choice.** `internal/lock` projects the covered subset of `prothesis.yaml` from RAW BYTES
(`ProjectConfig([]byte)` never sees a `*schema.Config`) and carries each covered leaf as the
VERBATIM YAML scalar text the user wrote. A key the user did not write contributes no entry at all.
`internal/oracle.Options.Fingerprint()` is written into `.prothesis/lock` for review but is
deliberately OUTSIDE the digest; `thesis oracles verify` reports a moved fingerprint as a WARNING
and never as drift.

**Rationale.** D-F and OQ-015.2 require that the digest cover the user-supplied config with defaults
NOT filled in, because digesting the resolved config would move the digest on every downstream
project the moment a release adjusted any compiled-in default, and exit 4 is the one code the agent
loop must never auto-resolve. Taking bytes rather than a decoded struct makes that structural rather
than a rule someone must remember.

The same argument decides the built-in thresholds. `internal/oracle/options.go` states that
`Fingerprint` exists so "the manifest must include" it. Followed literally that would put a value
derived ENTIRELY from compiled-in constants inside the digest, which is exactly the failure D-F
forbids: those thresholds are reachable from no config key, so the only thing that can move that
fingerprint is a new `thesis` build. The intent behind the comment (that widening a tolerance be
visible rather than invisible) is served by recording the fingerprint in the lock file and warning
when it moves. Widening one still requires a Go source diff, which is reviewed on its own terms.

**Raw text rather than decoded values.** Decoding would reintroduce the schema's typed parsers and
with them its defaults, and Go's shortest-float representation carries no cross-version stability
guarantee, so a toolchain upgrade that reformatted `1.41` would be a spurious exit 4. The accepted
cost is that rewriting `90s` as `1m30s` moves the digest. That is a real edit to a lock-covered
field, and the diagnosis names it in exactly those words rather than saying only "mismatch".

**Rejected.** Digesting the resolved `Config` (turns every release into a fleet-wide exit-4
incident). Hashing file modes (the executable bit does not survive a Windows checkout, so a
cross-platform clone would false-fail). Hashing the tool version (already refused in
`cmd/thesis/version.go` for the same reason).

---

## D-036: the lock covers the WHOLE `profiles:` block, and sequences are compared as sets

**Choice.** `CoveredPaths` is `oracles.builtin`, `oracles.dir`, `perturber.allow`,
`perturber.budget`, `perturber.deny`, `profiles` and `search`. Scalar sequences flatten to a
MULTISET under the parent path, so reordering `perturber.allow` does not move the digest while
removing an entry does.

**Rationale.** The Phase 3 brief fixes a minimum of "every profile's `budget` and `worlds`".
Covering only those two leaves `driver_profile` open, and
`profiles.gate.driver_profile: gate -> smoke` exchanges a 20,000-operation workload for a
500-operation one in a single token, without touching an oracle or a budget. Covering the block is a
superset, so nothing required is lost and the cheapest remaining weakening is closed.
`oracles.builtin` is covered because `pkg/schema` already refuses to default that list precisely so
that deleting `no_stuck_op` cannot be invisible; `oracles.dir` because repointing it swaps the whole
oracle set in one line.

The set treatment of sequences is a defence against the THIRD way Phase 3 fails. `allow` and `deny`
are sets of fault kinds and reordering one is semantically nothing; firing exit 4 on a reorder would
be a spurious lock failure, and a lock that cries wolf is one a team learns to re-baseline reflexively,
which is the behaviour I6 exists to prevent.

**A residual hole, stated rather than hidden.** `driver.profiles` (`clients` / `ops` / `mix`) is NOT
covered, so shrinking the workload there is a weakening this lock does not catch. Logged as OQ-026
with a passing test that pins the current behaviour so it cannot be forgotten.

---

## D-037: an ABSENT lock does not block a run, but it is never a `verify` pass

**Choice.** `lock.Gate` is the single enforcement point (D-I): `thesis run` calls it before booting
anything and `thesis gate` will wrap it in Phase 6. A MISMATCH exits 4 from both `run` and
`oracles verify`. An ABSENT lock exits 4 from `verify` but does NOT block `run`; the run proceeds,
warns on stderr, and the verdict records `oracle_lock.status: "absent"`. A CORRUPT or unreadable lock
is an error mapping to exit 2, never to "absent".

**Rationale.** Three different failures need three different answers. Refusing to run a project that
has never been locked would be a spurious exit 4 on first contact, and running a test is not
conditional on a baseline existing. But `verify` is an explicit request to check the gate against its
baseline, and answering "there is no baseline" with exit 0 would be a vacuous pass (the SECOND way
this phase fails) and one an agent could manufacture by deleting a single file. Exit 4 is right there
because the remedy is a HUMAN-authored `thesis oracles lock --reason`, which is precisely what an
agent must not do for itself.

Treating a corrupt lock as an absent one was rejected for the same reason: one corrupting write would
then silently clear the drift check.

**One further integrity check.** A lock file whose recorded manifest does not hash to its recorded
`manifest_sha` has been hand-edited, and is reported as a mismatch naming that fact. Trusting the
recorded digest over the recorded manifest would let an agent paste in a digest computed from a
config it then changed back.

---

## D-038: a disagreement between an oracle's exit code and its reported status is INCONCLUSIVE, not the worse of the two

**Choice.** `internal/oracle`'s external runner returns `ok` only when the process exit code AND the
`status` on its stdout BOTH say ok, and `violated` only when both say violated. Every other
combination is `inconclusive`, with a message naming what each channel said.

This is deliberately STRICTER than `schema.DecodeOracleOutput`, which reconciles a disagreement by
taking the WORSE of the two readings. Both functions remain: `DecodeOracleOutput` is the Phase 0
total decoder for anyone reading an archived document, and the runner applies this additional rule
on top of a parse.

**Rationale.** The two rules differ on exactly one pair of cases, and only in a direction that
matters:

| stdout says | exits | "worse of two" | this rule |
|---|---|---|---|
| `ok` | 1 | **violated** | inconclusive |
| `violated` | 0 | **violated** | inconclusive |
| `inconclusive` | 0 | inconclusive | inconclusive |

Neither rule ever lets a disagreeing oracle PASS, so this is not a weakening: it is a choice
between two non-passing answers. "Worse of two" manufactures a `violated` out of a defect in the
oracle, and a **false positive is the ranked #1 way this phase fails**: the entire value
proposition is "when it says FAIL, something is genuinely wrong". An `inconclusive` is exit 2,
which the agent-loop contract already defines as *retry once, then escalate to a human*: the
correct handling for "the oracle is broken", which is what a self-contradicting oracle is.

The brief is explicit on the point (§2: *"exit code and reported `status` must agree. If they
disagree, that is an oracle defect → treat as `inconclusive` and say so. Never trust one over the
other silently"*), and this entry records that the two functions in the tree now differ, and why,
rather than leaving a reader to discover it.

**Rejected.** Rewriting `DecodeOracleOutput` to match. Its "never adopt the weaker reading" rule is
right for its own job (decoding a document with no oracle definition to check it against) and
changing a Phase 0 function's semantics under Phase 1's tests to save one comparison is not worth
the blast radius. Pinned by `TestExternalOracleFailureModesAreNeverOK` and
`TestDisagreementNamesBothChannels`.

---

## D-039: the oracle definition file, and why `thesis init` scaffolds it INERT

**Choice.** An external oracle is declared by a small strict YAML file in `oracles.dir`:

```yaml
version: prothesis.oracle_def/v1
name: linearizable.kv
class: consistency
valid_phases: [ASSERT]
cmd: "./bin/linearizable-kv"
timeout: 120s
```

Discovery reads `*.yaml` and `*.yml`, ignores every other file, sorts by oracle NAME, and treats a
definition that does not parse as a hard ERROR. `thesis init` scaffolds the directory with a
`README.md` and this exact definition: written as `linearizable.kv.yaml.example`, which discovery
does not pick up.

**Rationale, field by field.** Four facts have to exist somewhere and the frozen contract carries
none of them: which executable to run, what class it is (severity is derived from class, so an
oracle that named its own would be grading its own finding), which phases it is valid in
(invariant I5; the ENGINE filters, so the engine has to know before it runs anything), and how long
it may run. `oracles.dir` is the only place the directive provides. This resolves OQ-028, which
asked for exactly this and noted that without it `thesis oracles list` cannot tell an operator which
phases an external oracle will be evaluated in until it has already run.

Four properties are chosen against specific failures:

- **Strict decoding.** `valid_phase` for `valid_phases` would otherwise take its zero value, leaving
  the oracle valid in no phase: an oracle that can never fire, sitting in the directory looking
  like coverage.
- **A required `version`.** A field REMOVED in a future v2 would decode silently to its zero value;
  `KnownFields` catches added keys but not removed ones.
- **No `enabled:` flag.** Switching an oracle off without deleting it is a gate-weakening move that
  reads as one character in a diff. Removing the file is unmissable.
- **A required, capped `timeout`.** An oracle with no timeout can hang a run forever, and invariant
  I7 requires every invocation to return a verdict.

**Why the scaffold is inert.** A LIVE definition names an executable that a project which has just
run `thesis init` has not built, so every run would report an oracle the user never configured,
INCONCLUSIVE, forever. Reporting exit 2 for a configured-but-missing oracle is right; configuring
one on the user's behalf and then reporting it is not. Renaming the file is the activation step, and
it is exactly the change `.prothesis/lock` exists to see.

**Rejected.** Discovering every file in the directory regardless of extension. The lock hashes the
whole directory, so a README, a checker's source and a committed helper script legitimately live
there; parsing them as definitions would fail the run on a README. The cost is that a definition
saved as `foo.txt` is ignored: mitigated by `Discovery.Ignored`, which reports every non-definition
file so an operator can see it, and by the lock, which covers the directory's contents either way.

---

## D-040: the `linear` driver profile, and why it is not ~20,000 operations

**Choice.** A fourth fixture driver profile, `linear`: 16 clients, **60,000** operations, **8**
keys, mix read 0.5 / write 0.5. It is the only profile whose histories `linearizable.kv` can
soundly check, and it is added to BOTH `testdata/kvfixture/prothesis.yaml` and loadgen's built-in
table, because `{profile}` passes only a NAME and loadgen resolves the parameters itself (OQ-012).

**Why a new profile at all.** None of the three existing profiles can produce a checkable history
that also contains an anomaly:

| profile | single-key? | spans a fault window at @8200..13500ms? |
|---|---|---|
| `smoke` | yes | **no**: 500 ops retire in ~700ms |
| `gate`  | **no**: 20% txn | yes |
| `soak`  | **no**: 20% txn | yes |

The fixture's `txn` reads one key and writes a DIFFERENT, independently drawn key, so it breaks the
precondition of Herlihy & Wing's locality theorem. A per-key checker handed such an operation must
refuse the whole history rather than partition it (PHASE3_BUILD_BRIEF D-A), and does: run against
a real `gate` history it refuses in 88ms naming the offending op id. That is correct behaviour, not
a gap, so `gate` and `soak` are permanently unavailable to this oracle.

**Why 60,000 and not the brief's "~20,000".** The brief's number was written against `gate`'s mix.
Read and write operations are several times cheaper than a transaction, so 20,000 of them retire
well before @8200ms: reproducing `smoke`'s failure mode, which is the exact thing the brief
introduced the profile to avoid ("long enough to span the fault window"). The op budget is a
CEILING, not a target: the driver is stopped at QUIESCE, so a world retires whatever it reaches and
overshooting the ceiling costs nothing, while undershooting it ends DRIVE before the anomaly can be
observed. Measured, the definition-of-done world retires ~15,000 operations of the 60,000 budget.
The brief's INTENT is honoured exactly; its arithmetic is not, and this is the deviation.

**Why 8 keys.** Conflict density, and therefore the checker's power to witness a stale read, is a
function of operations per key. Eight keys over sixteen clients holds per-key concurrency near two,
which keeps the Wing & Gong search cheap (measured: 15,283 states, whole run 34s wall) while still
placing ~2,000 operations on every key. Fewer keys would raise the count of concurrent
indeterminate writes on one key, and it is `info` writes that make the search branch.

**Rejected: reusing `smoke` with a longer budget.** A profile's op count is not lock-covered
(OQ-026), so tuning the workload to reach an anomaly is a knob that leaves no trace in the digest.
A separate, named profile is a reviewable diff.

**Pinned by** `TestLinearProfileIsSingleKeyOnly` and `TestLinearProfileNeverPicksAMultiKeyOperation`
in the fixture module, which fail if any txn or admin weight is ever added: verified by mutation,
since a profile that silently became multi-key would turn the consistency oracle into a permanent
`inconclusive`, i.e. a gate that is green because nothing was checked.

---

## D-041: a finding with no timing is placed at its PHASE, not at DRIVE start

**Choice.** `control.BuildCausalTimeline` places the oracle's own row at `oracleRowMS(...)` rather
than at the raw `FirstSeenMS`. When a finding carries no timing AND its observed phase's window does
not contain 0, the row is emitted at that phase's start.

**Why.** `internal/oracle` already refuses to make the matching mistake about the PHASE:
`Result.inPhase` exists precisely so the engine does not derive a phase "from a number the oracle
never gave", and its comment says a fabricated position "is worse than none: it is the field a human
reads first". The timeline builder was not applying that same rule to the COORDINATE. `FirstSeenMS`
is a plain `int64`, so "no timing supplied" and "evidence at exactly DRIVE start" both arrive as 0.

**The concrete symptom**, observed on the definition-of-done run before the fix: `linearizable.kv`
emits only the frozen witness shape `{"op_ids", "key"}` and therefore no `first_seen_ms`. Its
violation was correctly attributed to `phase: ASSERT`, and then rendered as the FIRST row of its own
causal timeline at `t+0ms`, ahead of the fault injection at t+8202ms and of the operations that are
the evidence. After the fix the row sits at t+18779ms, where ASSERT begins, which is where the
oracle actually reached the conclusion.

**Why the phase start is honest rather than another guess.** It is a true statement about the run:
the finding was reached during that phase. The discriminator needs no new field, when timing is
absent the runner pins the EVALUATED phase, whose window does not contain 0; when evidence really is
at DRIVE start the engine DERIVES a phase whose window does contain 0.

**Rejected: dropping the row.** It is the only entry naming the violation on the violation's own
timeline.

**Pinned by** `TestAFindingWithNoTimingIsNotPlacedAtDriveStart`,
`TestAFindingWithRealTimingKeepsIt`, `TestAFindingGenuinelyAtDriveStartStaysAtZero`,
`TestAnUnscopedFindingKeepsItsOffset`, `TestAFindingInAnUnmeasuredPhaseKeepsItsOffset`:
mutation-verified.

---

## D-042: the DRAIN is the first act of QUIESCE, it is bounded, and a refusal is recorded

**Choice.** QUIESCE no longer opens by killing the driver. It opens by ASKING it to stop issuing
new operations while completing the ones it holds, waits up to `DefaultDrainDeadline` (10s), and
then stops it exactly as before. The channel is the driver's stdin: the harness writes
`{"cmd":"stop"}` and then closes the pipe, so a driver honouring either signal drains. The request
is announced by `PROTHESIS_STDIN_CONTROL=1`, on the same dual-transport terms as D-021's
`PROTHESIS_PLAN_PATH`. `RunnerOptions.DrainDeadline` bounds it; a NEGATIVE value restores the
Phase 1 behaviour outright.

**No ninth phase name.** Directive 4.1 freezes eight and invariant I5 is built on them, so the
drain adds none: "stop the workload and wait for convergence" is what QUIESCE already says, and
stopping a workload by asking it to finish is a better rendering of that sentence than killing it
mid-operation. Nothing new appears in `schema.Phase`, in `phases.jsonl`, or in
`oracle_input.phases`.

**Rationale: this closes OQ-033, and Phase 4 cannot work without it.** Measured before the
change: *"16 operation(s) were still in flight when observation ended at t+19002ms before the
5000ms SLO ceiling elapsed"*, and the world reported INCONCLUSIVE. `no_stuck_op` was RIGHT and was
not touched: an operation with no completion record is indistinguishable from a wedged one, so
passing would be a false PASS and violating a false FAIL. The LIFECYCLE was wrong. D-040 gives
`linear` an op budget that deliberately outlives the fault window, so its driver was ALWAYS killed
with operations outstanding: every clean long world was inconclusive *by construction rather than
by evidence*.

For Phase 4 that is fatal rather than untidy. The Saboteur's utility function scores oracle
findings; if a clean world and an unevaluable one produce the same document, the search cannot
tell "nothing is wrong here" from "I could not tell", and it ranks worlds on noise.

**Measured, on the fixture, three ways** (`linear` profile, seed 424242, one world):

| image | faults | before (OQ-033) | after |
|---|---|---|---|
| buggy | none | INCONCLUSIVE, exit 2 | **PASS, exit 0** |
| kvfixed | D-031 schedule | INCONCLUSIVE, exit 2 | **PASS, exit 0** |
| buggy | D-031 schedule | FAIL, exit 1 | **FAIL, exit 1** |

The third row is the one that matters: the fix does not weaken detection. The passing world
retired 3745 operations and `no_stuck_op` reported *"all 3745 operation(s) completed within SLO
ceiling (5000ms) after HEAL"*: a pass on evidence, from an oracle that ran, not from an oracle
that was relaxed.

**Why it cannot manufacture a pass.** Draining EXPOSES a stuck operation, it does not hide one.
`no_stuck_op` still violates on anything that took longer than the SLO ceiling to retire after
HEAL, and an operation that would previously have been in flight at observation end (inconclusive,
unjudged) now either completes and is judged, or is still open with the ceiling long elapsed and is
reported STUCK. That is why `DefaultDrainDeadline` (10s) deliberately EXCEEDS
`oracle.DefaultStuckOpSLO` (5s): a shorter deadline would stop observing before the oracle's own
threshold was reachable, which is the defect being fixed rather than a fix for it. Pinned by
`TestTheDefaultDrainDeadlineOutlastsTheStuckOpSLO`.

**Why it runs AFTER HEAL.** Draining under an active partition measures the fault, not the system:
in-flight operations cannot retire while the node they address is unreachable, so every perturbed
world would report its driver as refusing. After HEAL the faults are withdrawn and verified
withdrawn, so an operation that still will not complete is genuinely stuck.

**A refusal is recorded, in three places, and in quiet mode too.** A driver that ignores the
channel is killed at the deadline exactly as before, and `DrainDeadlineExceeded` reaches
`result.json` (`driver.drain`), stderr, and `DriverResult.Drain`, which is on `EvalRequest.Driver`,
so an oracle reasoning about in-flight operations can see whether they were given a chance. It is a
fact about the run, never a licence to assume the benign reading; OQ-033 option 2 rejects that move
by name. Quiet mode does not suppress it, because a refused drain changes what the verdict means.

**Rejected.** Weakening `no_stuck_op` to ignore operations open at a deliberate driver stop
(OQ-033 option 2): it is the vacuous pass this project has been bitten by twice. Shrinking
`linear`'s op budget (option 3): it defeats D-040, since a profile that finishes before the fault
window can only ever produce a clean history.

**One defect found on the way, and fixed.** `Supervisor.Kill` did not record that it had killed
anything, because only context cancellation set `Result.Killed`. Every driver stopped at QUIESCE
(i.e. every driver, before this change) therefore came back as `StatusFailed` with `Killed` false,
and the run recorded `stopped: false` for a workload the harness had just killed.
`DriverResult.Stopped` is what tells an oracle a non-zero exit was EXPECTED, so getting it wrong
points the reader at the workload for something the harness did.

---

## D-043: a parallel world is a compose project, a port band and a slot; a leaked slot is RETIRED

**Choice.** `Runner.Parallel(ParallelOptions)` returns an executor that runs N worlds at once, each
with its own compose project name (`thesis-<run_id>-w<NN>`, D-022's shape), its own generated
overlay, its own band of published host ports, its own artifact directory and its own seed. Results
come back SORTED BY ORDINAL. Concurrency is checked against a bridge-network budget BEFORE anything
boots, and a slot whose compose project could not be verified destroyed is permanently withdrawn.

**Rationale: the binding constraint is an address pool, not a CPU.** This host has a hard ceiling
near 24 free Docker bridge networks and the KV fixture creates TWO per project, so 4-way
concurrency costs 8 and 8-way costs 16. Three things follow and they are the whole design:

1. **The cap is computed and REFUSES.** `NetworkBudget{Available: 24, PerWorld: 2}.MaxWorkers()` is
   12, and asking for 13 fails with the arithmetic before a container exists. Silently reducing
   concurrency instead would make D-022's measured "8 projects give 3.8x" an unreproducible claim
   and would move the failure to whichever world happened to exhaust the pool. An optional `Probe`
   MEASURES the free pool (`harness.FreeBridgeNetworks`); a measured shortfall is fatal, a probe
   that cannot answer falls back to the configured budget and says so: a daemon hiccup must not
   stop a search.
2. **`Backend.Down`'s verdict is no longer discarded.** `runWorld` used to write
   `_ = r.backend.Down(...)`. That was the one place a leaked compose project could pass unnoticed,
   and D-017 built `Down` to verify its own work precisely so somebody would read the answer. It
   now reaches `worldResult.teardownErr`, the world's outcome, and `WorldOutcome.TeardownErr`.
3. **A leaked slot is RETIRED, not reused.** Its networks are gone for the rest of this engine's
   life; handing it to the next world would boot a project on top of a live one. When retirement
   empties the pool the executor fails closed with `ErrWorkerPoolExhausted`, naming every leaked
   project and the `docker network rm` that recovers them, and it stops the batch rather than
   letting each remaining world burn a boot to fail the same way.

**How a world gets its own published ports, and why that is a seam rather than a rule.** Not by
editing the system under test. Compose interpolates `${VAR}` in the target's own compose file, and
the fixture already parameterises exactly the two things that collide; `${KV_PORT_N1:-18081}` and
`${KV_PREFIX:-prothesis}`. So the harness supplies VALUES per slot through the new
`harness.UpRequest.Env` (and `DownOptions.Env`, because `down` re-interpolates too). WHICH variable
names to set is a property of the target's compose file, which this package cannot know:
`DefaultWorkerEnv` emits generic `PROTHESIS_*` names and `ComposeVarEnv` maps a slot onto a
project's own. For the fixture that mapping is `KV_PREFIX` plus `KV_PORT_N1/N2/N3`, pinned by
`TestComposeVarEnvMapsTheFixtureVariables`.

**The driver's half, which is easy to miss and was.** Isolating the CLUSTERS is only half of it. A
driver whose targets are baked in addresses whichever cluster owns the default port, or, when no
world owns it, nothing at all, and a world that drove nothing still writes a history a checker will
happily call clean. That is the vacuous pass this project is ranked against, reached from a new
direction. So each slot also exports `PROTHESIS_TARGETS` (`127.0.0.1:<port>,...` in config node
order), the fixture's loadgen reads it as the default for `--targets`, and an inherited value is
STRIPPED when the harness did not set one. Verified by mutation: with the targets suppressed, both
concurrent worlds retire **zero** successful operations and
`TestFixtureParallelWorldsAreIsolatedAndTornDown` fails naming exactly that.

**Measured, two concurrent worlds on the fixture:** projects `…-w00` / `…-w01`, host ports
`{kv-n1:19000, kv-n2:19001, kv-n3:19002}` and `{kv-n1:19016, kv-n2:19017, kv-n3:19018}`, 500
successful operations each against its OWN cluster, zero networks left behind, 13-19s wall for two
worlds against ~20s for one.

**What had to stop being shared.** `runWorld` read its schedule and its injectors off the `Runner`,
and ASSIGNED the per-world injector set back to `r.injectors` mid-flight. That is a data race
between goroutines and a hidden dependency between worlds even when they run one at a time: the
hazard D-032 already records for the same field. Both now travel in a `worldSpec` and on the
world's own stack. The runner's stderr is serialized, because an interleaved half-line from four
concurrent worlds is worse than no line.

**Rejected.** Publishing no host ports (D-010: container IPs are not routable from a Windows host,
so there is no other way in). Editing the fixture's compose file to hard-code per-worker ports (it
already parameterises them; the harness only had to supply values). Reusing a leaked slot after a
`docker network prune` (that is a human's decision about a human's machine, and an executor that
made it silently would hide the leak it exists to surface).

---

## D-044: a world's seed is path-derived from its ORDINAL, so scheduling cannot change it

**Choice.** `control.WorldSeedFor(runSeed, ordinal)` delegates to `recorder.WorldSeed`, and BOTH the
serial loop and the parallel executor use it. It replaces `r.seed + uint64(ordinal)*10007`.

**Rationale.** The old expression had the right shape and none of the guarantee. D-013 made stream
keys a function of their PATH alone precisely so that a value cannot depend on what else exists or
on what order things happened in, and `recorder.WorldSeed` is the derivation that inherits it.
Under the parallel executor the difference becomes observable: worlds complete out of order, and a
seed derived from a running counter would make a world's behaviour depend on which worker picked it
up, which Addendum A.8 forbids outright ("the same seed must expand the same tree"). Pinned by
`TestAWorldSeedDependsOnItsOrdinalAndNothingElse` and
`TestTheSameRequestsGiveTheSameWorldsAtAnyConcurrency`, which runs the same six requests at 1-way
and 4-way concurrency and compares the seeds.

**Blast radius checked before changing it, not assumed.** A world seed reaches the driver, the
perturber's executor and the world file, so moving it changes what every future run does. It
invalidates nothing already committed: `.prothesis/regressions/` is empty and no `.thesis` file
exists outside run bundles, so there is no archived world whose seed this moves. Had there been
one, the old expression would have had to stay for those worlds.

---

## D-045: the run profile and the driver profile are different namespaces, and the runner conflated them

**Choice.** `control.Runner` resolves `profiles.<name>.driver_profile` once at construction and
uses THAT for `driver.NewPlan`, for `DriverRequest.Profile` and for the world file's
`driver_profile`. `NewPlan`'s error is now FATAL to the world, and `NewRunner` refuses a profile
whose `driver_profile` does not resolve rather than defaulting to the profile's own name.

**Rationale: found by running it, and it was invisible for three phases.** The directive's sample
config and the reference fixture both name their run profiles and their driver profiles
identically (smoke/gate/soak), so passing the RUN profile where the DRIVER profile belonged
produced correct behaviour by coincidence. Phase 4 added a run profile called `search` whose
driver profile is `linear`, and the coincidence broke:

```
driver.NewPlan  -> "driver.profiles has no entry \"search\""
the error       -> DISCARDED at the call site (`if err == nil { write }`)
the workload    -> launched with --profile search, exited 5
history.jsonl   -> NEVER WRITTEN
linearizable.kv -> INCONCLUSIVE ("cannot open the history")
no_stuck_op     -> INCONCLUSIVE ("cannot open the history")
no_crash, resource_return_to_baseline, availability_after_heal
                -> returned verdicts about a system NOTHING HAD DRIVEN
```

Measured on run `r_2026_09_08_5c6b` world 3. That last line is why the error is now fatal: a world
that reports on a system it never exercised is the one thing this codebase refuses to ship, and it
arrived through a discarded error rather than through a weakened assertion.

**Refused rather than defaulted.** `NewRunner` could fall back to the profile's own name when
`driver_profile` is empty. It does not, because that fallback is exactly the coincidence that hid
this. Config validation already makes the key mandatory and cross-checks that it resolves, so an
empty value can only reach the runner from a hand-built `schema.Config`, and saying so is more
useful than guessing.

Pinned by `TestTheDriverIsGivenTheDriverProfileNotTheRunProfile`,
`TestTheDriverPlanCarriesTheDriverProfilesNumbers`, `TestTheWorldFileRecordsTheDriverProfile`,
`TestAWorldWhoseDriverPlanCannotBeBuiltIsAHarnessError` and
`TestNewRunnerRefusesAnUnresolvableDriverProfile`. Mutation-verified: restoring `r.profile` at the
three call sites fails the first three.

---

## D-046: `no_crash` is told what the perturber actually injected; the field was hardcoded empty

**Choice.** `EvalRequest` carries an ADDITIVE `Realized []schema.RealizedFault`, and
`oracle.Input.PlannedFaults` is built from it. The oracle-input assembly moved out of
`defaultOracleEngine.Evaluate` into a named `buildOracleInput` so the wiring is reachable from a
test.

**Rationale.** `PlannedFaults` was `[]oracle.FaultWindow{}` (a literal empty slice) so
`no_crash`'s entire *"exited outside any planned fault window"* clause was dead code in
production. Measured on run `r_2026_09_08_5c6b` world 3, whose only fault was
`proc.kill(kv-n1, signal=SIGTERM)@12600..14600`:

> `no_crash` VIOLATED; "1 process exited outside any planned fault window: kv-n1 at t+12713ms
> (exit code 0)"

against a window that opened 113 ms earlier and named that very node. The oracle's logic was
right; it was handed nothing to reason with. Its own doc comment explains why nobody noticed
(*"In Phase 1 the perturber injects nothing, so Input.PlannedFaults is empty"*) which was true in
Phase 1 and was never revisited when Phase 2 added the perturber.

**Why this is a Phase 4 concern rather than a cosmetic one.** A guided search over a config whose
`perturber.allow` contains `proc.kill` or `proc.restart` reaches its FIRST kill world, is handed a
fabricated crash violation, and stops: having "found" a bug that is the harness reporting its own
fault injection. That is the ranked #1 failure mode of this phase, arriving through an unwired
field.

**REALIZED, not PLANNED.** A planned `role:leader` binds to a concrete node only at INJECTION time
(D-012), so the planned schedule cannot say whose exit to excuse. The realized record can. A
realized entry with no recorded binding yields a window with an empty node list, which
`oracle.FaultWindow` documents as excusing nothing: the safe direction, because treating
"unknown" as "planned" would let one unrecorded fault silence every crash in its time range.

Mutation-verified in both halves: re-hardcoding `PlannedFaults`, and dropping `Realized` from the
call site, each fail `TestTheOracleEngineIsToldWhatThePerturberInjected`.

---

## D-047: the OBSERVE classifier reads the telemetry's OWN sampling rate rather than assuming it

**Choice.** `saboteur.Observation` carries an ADDITIVE `SampleIntervalMS`, filled from
`telemetry.Document.IntervalMS`, and it overrides `ObserveOptions.ForProbe()`'s assumed rate.

**Rationale: measured, and it silently disabled the entire Tier 1 classifier.** A.3 asks for a
200 ms trajectory during a probe world and `ForProbe()` honours that literally. The harness's
collector, however, samples at `search.observe.sample_interval_ms`, which defaults to 500. The
classifier's window-stretch guard then compared a real seven-sample span of ~3500 ms against a
nominal 1200 ms, concluded the samples straddled a blackout, and returned INSUFFICIENT_DATA.

Measured on run `r_2026_09_08_e363`: **21 of 21 probes classified INSUFFICIENT_DATA**, so nothing
reinforced, Tier 2 was never activated, and the Saboteur fell straight through to corpus mutation;
on a fixture that does reinforce. With the observed rate used instead, the same sweep produced
DAMPEN, REINFORCE_LINEAR and REINFORCE_ACCELERATING classifications.

`telemetry.Document` carries `interval_ms` for precisely this reason; its own doc comment says
*"an oracle reasoning about '3 consecutive samples' needs to know what a sample is worth in wall
time"*. A.3's 200 ms is a statement about what the harness SHOULD do; the document is a
measurement of what it DID, and the measurement wins.

---

## D-048: a violation found during Tier 1 is recorded but does not abandon the sweep

**Choice.** The search stops at the first definite violation, EXCEPT while the Saboteur is still
in its Tier 1 probe stage, where the violation is recorded and the sweep continues.

**Rationale.** A.3's probe sweep is reconnaissance whose OUTPUT is a ranked list over the whole
(kind, target) space, and A.5 roots Tier 2 at the top-ranked reinforcing signals. Ending the run
at probe 3 of 21 destroys that list and Tier 2 never happens. Measured: the first attempt did
exactly that; a `proc.kill` probe produced a crash finding at world 3 and the search stopped,
having never escalated.

Nothing is lost by continuing. The violation is already in the world record, it reaches the
verdict, the exit code is still 1, and `first_violation_ordinal` / time-to-first-violation were
recorded at the moment it was found, so D-029's pre-registered COUNT metric is unaffected. Once
the search is escalating, a violation IS the kill shot and the run stops.

---

## D-049: kinds this platform cannot deliver are removed from the SHARED action space

**Choice.** `internal/search/engine` filters the enumerated `search.Space` through
`faults.PlatformCapability` at construction, reports what it removed, and applies the filter to
the space BOTH strategies draw from.

**Rationale.** The reference fixture's `perturber.allow` contains `io.latency`, which is in the
frozen 17-kind registry and is NOT implementable on Docker Desktop (D-033b): delaying filesystem
operations needs a shim under a running container's overlay2 mount, which cannot be inserted.
Measured on run `r_2026_09_08_5c6b`: a generated world naming it reached `ErrUnsupported` eight
seconds into DRIVE, having already spent a compose project, two of the host's ~24 bridge networks
and ~33 seconds to learn what a table lookup knows. At the ~62-rollout budget D-022 measures, that
is a real fraction of the search.

**Applied to the SHARED space, deliberately.** Restricting only the Saboteur would hand it a
cleaner space than the baseline and make D-029's pre-registered comparison measure the filter
rather than the strategy. Emptying the space entirely is refused rather than done silently, for
the same reason `search.NewSpace` refuses one: a search over an empty space reports worlds it
never perturbed.

---

## D-050: the MCTS expansion order IS the search, and the tree measures its own contribution

**Choice.** Children are generated by the A.7 ladder in rung order, then STABLY reordered by an
integer priority: (0) the action overlaps a committed fault in time on the same target, (1) the
action names the target the Tier 1 probe reinforced on, (2) pure ladder order. `Tree.Stats()`
reports FORCED versus INFORMED selections, and `thesis search` prints the D-015 note when the
informed count is zero.

**Rationale.** Tier 0 is A.7 Rung 4's own normative text (*"Bias toward overlapping proc.pause
with net.partition) this is the canonical Raft lease bug trigger."* Without it, strict rung order
spends the entire budget on RECON and STRESS children before reaching a single PARTITION, and at
~62 rollouts the compound interaction is never reached at all. Tier 1 comes from the MEASURED
classification, not from a constant: nothing in the ordering names a node, a role or a
millisecond. Priorities are integers and the sort is stable, so the order never depends on map
iteration or on a float comparison another architecture could fuse differently.

**The measurement is the point.** D-015 records that at Addendum A's budgets UCT is provably
equivalent to ladder-ordered enumeration. Rather than assert that, the tree counts it. Measured on
run `r_2026_09_08_19d9`, three escalation trees over 19 rollouts: **0 informed selections out of
19**, max visits 7, 0 nodes pruned. Backpropagation never decided anything and A.5's pruning rule
was never reachable. The prediction holds, and the CLI says so in the run's own output rather than
leaving a reader to infer it.

---

## D-051: the escalation root re-issues the reinforcing signal at LADDER magnitude

**Choice.** `saboteur.LadderRootFor` builds a Tier 2 tree's committed prefix from the reinforcing
signal's (kind, target) at the escalation ladder's own magnitude and window, not at the probe's.

**Rationale.** A probe deliberately runs at minimal magnitude in a 2000 ms window, because that is
what makes it cheap. Rooting the tree at that exact fault would carry the probe's deliberately weak
magnitude into every branch below it, and A.5's entire purpose is to convert a detected spiral
into a violation. The TARGET is what the probe measured and is carried over verbatim; the
MAGNITUDE is what Tier 2 chooses.

The window geometry is derived from the branch rather than from a constant: an added fault opens
`CompoundLeadInMS` before the earliest committed fault and, by the ladder's tail rule, outlasts
it. At `AtMS` 8300 with the default timing that is byte-for-byte D-031's proven schedule; at every
other offset it is the same SHAPE. `DefaultEscalateAtMS` is deliberately 3000 and not 8200:
anchoring the default on D-031's measured millisecond would bake the answer into the search, which
is exactly what A.9 #2's "no hardcoded fault schedules" forbids.

---

## D-052: A.9 #1 does NOT hold on this fixture, and the reason is structural

**Choice.** Recorded as a MEASURED NEGATIVE rather than tuned away. The Tier 1 sweep classifies
`proc.pause` on the leader as **DAMPEN**, not `REINFORCE_ACCELERATING`.

**Measured** (run `r_2026_09_08_19d9`, seed 424242, 21-probe sweep; the control world was
evaluable and two baseline metrics were recorded, so the comparative criteria had a baseline):

```
proc.pause(role:leader)  node=kv-n3  DAMPEN  rank 101.8
  "DAMPEN on status.queue_depth: trend -4.001/s does not exceed the reinforce
   threshold +0/s: the system is stable or recovering"
proc.pause(kv-n1)        node=kv-n3  DAMPEN  trend +0/s
proc.pause(kv-n2)        node=kv-n1  DAMPEN  trend +0/s
```

**Why the classifier is right and the criterion is wrong.** A.4's spiral detector looks for a
RESOURCE spiral: a queue, an RSS, a retry count that grows and keeps growing. The fixture's
injected defect is a stale READ: a displaced leader serves a value from an expired view. It
produces no queue growth, no retry explosion and no resource spiral, and the surviving nodes'
queue depth actually FALLS after the pause, which is what a correctly-recovering Raft cluster
does. A.9 #1 asserts the lease bug will surface as `REINFORCE_ACCELERATING` "due to the lease
bug"; on this fixture the lease bug has no reinforcing telemetry signature at all.

The one A.3 criterion that WOULD fire ("a role change occurred within 1 second of fault
injection") is scored at zero weight, and correctly so (OQ-006 item 4): pausing a Raft leader
elects a new one in a correct implementation exactly as much as in a buggy one, and D-031 measured
precisely that on the healthy code path. A criterion that fires identically on healthy and
unhealthy systems carries no information.

**Not fixable by tuning.** Lowering `reinforce_threshold` below zero would classify a FALLING
queue as reinforcing, which is the ranked #1 failure mode written into a config key. The honest
reading is that A.9 #1 tests for a signal this defect does not emit.

**What happened instead is stronger than the criterion asked for.** The sweep's `net.partition`
probe found the stale read directly, in ONE fault: see D-053. Raised for a human ruling in
OQ-040.

---

## D-053: the fixture's stale read reproduces with ONE fault, not two

**Measured.** Run `r_2026_09_08_19d9` world 12, a Tier 1 micro-probe:

```
net.partition(kv-n1)@3000..5000
  -> linearizable.kv VIOLATED (consistency, severity high)
     key "k/0": no linearization exists for the 445 operation(s) on this key.
     The search space was exhausted (850 states over 2690 steps) with every
     branch rejected; a WITNESSED failure, not a timeout and not a budget.
```

**Why one fault suffices, and why D-031 needed two.** D-031's schedule pauses the leader inside a
partition that outlasts the pause, because a pause ALONE lets the ex-leader learn it was displaced
the instant it unfreezes. A partition alone has no such moment: the isolated leader never stops
running, never hears the higher term, and keeps serving reads under its still-valid 5-second lease
for the whole 2-second window. The partition cuts only the PEER plane (D-026), so clients can
still reach it. Same mechanism, one fault.

**This is not a correction to D-031.** That schedule was authored to reproduce the anomaly at a
specific late offset with a pause in it, and it does. What is new is that the search found a
STRICTLY MORE PARSIMONIOUS reproduction than the hand-authored one, which is A.6's parsimony
incentive doing what A.1 claims it will, and it satisfies A.9 #3's "<= 4 faults" with three to
spare.

---

## D-054: identity strictness is CALIBRATED against measured drift, not assumed

**Choice.** The shrink pipeline MEASURES whether the target's witness key identifies the defect, and
lowers the identity-matching level from `MatchWitnessKey` to `MatchOracleClass` when the measurement
says it does not. It measures in two places, because one is not enough:

1. **Stage 0 asks deliberately.** One extra execution of the UNREDUCED world, spent only for a keyed
   target at the default level. Cheap, early, and the safest possible evidence: nothing has been
   removed, so any violation it produces is the same defect by construction.
2. **The whole run keeps listening.** Any later observation of the target's oracle AND class on a
   different key is the same evidence, and it arrives free on worlds already being spent.

The demotion is recorded in `Result.Calibration`, in the stage note, and in the rendered CLI report,
which prints the enforced level and the word CALIBRATED together and never apart.

**Why two places, measured rather than reasoned.** Stage 0's probe is two executions, and two
executions cannot separate "stable" from "usually". Run `r_2026_09_09_9d07` drew `k/0` twice,
concluded the key was part of the defect's identity, and then watched the same defect land on `k/1`,
`k/2`, `k/3` and `k/4` over the following 60 worlds. `k/0` holds roughly three quarters of the time
here, so two draws agree more often than not while proving nothing.

The cost of getting it wrong was not a slower run. That shrink reduced **14 faults to 1** and
**6761 operations to 1**, narrowed the window to 915ms, and then failed its own confirmation gate
**1/3**, because the gate matched at the same too-strict level. A correct, minimal, hard-won repro
was computed and thrown away. Exit 1.

**Rationale.** RULE 1 says an accepted reduction must reproduce the SAME violation. That is only
implementable if something defines "the same", and `identity.go` defined it to include the witness
key on an argument that contradicted itself: asserting in one sentence that a stale read on k/0
and one on k/7 are the same defect, and in the next that a moved key must be rejected.

The measurement settles it. 27 recorded violations of the ONE injected fixture defect land on three
different keys, two runs produced two keys within a single run, and Phase 5 saw a fourth (OQ-047).
The key is decided by which of 16 concurrent clients was in flight when the partition landed. It is
not part of the defect.

Holding the stricter rule anyway does not fail safe. It rejects every legitimate reduction and
reports "no fault could be removed": the false negative `identity.go` itself names as the outcome
to avoid, and one that survives any amount of retrying because retrying is what produces it.

**Why measure rather than change the default.** Changing the default to `MatchOracleClass`
everywhere would discard real signal on a Tier A driver, where the same seed issues the same
operations and a moved key WOULD mean a different finding. Calibration keeps the stricter rule
wherever the evidence supports it: a target whose key holds on re-execution keeps
`MatchWitnessKey`, and the report says so.

**Five constraints keep this from being a gate weakening itself.**

1. **It may only calibrate a DEFAULT.** `MatchUnset` is the zero value and is deliberately not a
   match level, which is what distinguishes "nobody chose this" from "somebody chose exactly this".
   A caller who typed `--strictness key` gets that level and a failed baseline. Overriding an
   explicit instruction is the one thing an anti-gaming rule must never do. (This required removing
   the CLI's own default resolution, which had been collapsing both cases to `MatchWitnessKey` and
   would have disabled calibration for every invocation.)
2. **It stops at the floor.** `MatchOracleClass` is the directive's own stated minimum. The
   demotion is one-way, one-step, and happens at most once per run; a candidate that switches ORACLE
   or CLASS is still rejected, which is Rule 1's whole substance.
3. **The evidence must match on oracle AND class.** That is what makes a *switched bug* unable to
   trigger this: a candidate firing a different oracle, or the same oracle in a different class, is
   a divergence and stays one. What is being relaxed is the witness key and nothing else.
4. **It cannot happen during confirmation.** Calibration is allowed while REDUCING and frozen before
   stage 4 opens. Confirmation runs at whatever level the earlier stages settled on: decided before
   the gate was applied. A gate that relaxes its own criterion partway through being applied is
   indistinguishable from a gate being gamed, however good its reasons, and `k/k` is the number a
   human reads to decide whether to trust the minimal repro.
5. **It is loud.** Rule 1 forbids SILENTLY changing the bug. This changes what "the same bug" means,
   on evidence, in writing, in three places a reader cannot miss.

**Rejected alternative: only ask at stage 0.** Tried first, and measured to fail more than half the
time on this fixture for the reason above. Keeping the stage 0 probe *as well* is still worth its
one world: it settles the question before ddmin starts, so the reductions rejected before the
evidence arrives are fewer. The two together cover each other's failure mode: the probe is early
but underpowered, the accumulation is fully powered but late.

**Rejected alternative: change the default to `MatchOracleClass`.** Discards real signal on a Tier A
driver, where the same seed issues the same operations and a moved key WOULD mean a different
finding. Calibration keeps the stricter rule wherever evidence supports it: a target whose key holds
keeps `MatchWitnessKey`, and the report says so.

**Known limitation.** A reduction rejected BEFORE the calibration fires is not revisited: `relaxTo`
clears the memo table, but ddmin's own control flow has already moved past that subset. The result
stays sound (never a wrong reduction, only a possibly less minimal one) and the stage 0 probe
exists to make the window small. Closing it entirely would mean restarting ddmin on calibration,
which trades a bounded loss of minimality for an unbounded loss of budget.

**Cost.** One world per shrink for the stage 0 probe, for a keyed target at the default level.
The whole-run half costs nothing: it reads evidence from worlds already spent.

---

## D-055: an injector declares what its host cannot deliver, and both gates ask it

**Choice.** `internal/perturber` declares an optional `PlatformCapable` interface;
`faults.IOInjector` implements it by returning `faults.PlatformCapability`. Both the compile-path
gate (`checkKindsInjectable`) and the executor-path gate (`checkScheduleInjectable`) route through
one helper that asks two questions: is there a mechanism for this kind, and can this host deliver
it?

**Rationale.** D-049 established that kinds this platform cannot deliver must be removed from the
action space, and wired `faults.PlatformCapability` into `internal/search/engine`. That covered the
search. It left `thesis run --fault`, hand-authored schedules and every shrink candidate reaching
PERTURB before failing, and the reference fixture ships `io.latency` in `perturber.allow`, so the
gap was reachable out of the box (OQ-049). `PlatformCapability`'s own doc comment already claimed
the property the code did not have: that it "exists so a schedule can be rejected before a world is
booted and driven".

**Why an interface rather than a direct call.** `internal/perturber/faults` imports
`internal/perturber`, so the dependency runs one way and perturber cannot call the table directly.
Declaring the interface in perturber and implementing it in faults inverts the dependency without
moving the table out of the package that owns it.

**Why optional rather than a method on Injector.** Most mechanisms have nothing to say: tc,
iptables and `docker kill` work wherever a container does. An injector that declares no capability
is assumed capable, which is tested, because the opposite default would disable every mechanism
that simply works everywhere.

**Rejected alternative: deny `io.latency` in the fixture's `perturber.allow`.** Tried first, and
reverted. It fixes one config rather than the platform, and it moved the oracle-lock digest:
spending a gate change on a code defect. The general fix leaves the fixture's gate byte-identical.

---

## D-056: the driver is not trusted to have honoured its operation plan

**Choice.** When a shrink candidate carries an explicit operation list, `thesis shrink` counts the
`invoke` records in the history the driver actually wrote and refuses the attempt if there are more
than were planned. The refusal names both remedies: wire `{plan_path}` / `$PROTHESIS_PLAN_PATH` into
the driver (D-021), or pass `--skip-ops`.

**Rationale.** Stage 2's entire inference is *"I shortened the plan and the violation survived,
therefore the removed operations were not the cause."* That is valid only if the driver executed the
plan. A driver that ignores it drives its usual workload, so every candidate reproduces, ddmin never
meets a rejection, and it walks straight down to a single operation: reporting a large, confident,
fictional reduction in the minimum possible number of worlds.

No oracle can catch this, because every one of those worlds really did violate. It is only visible
by comparing the plan against the artifact the driver wrote, which is what this does.

**Measured.** Run `r_2026_09_09_2904`: a plan of 1 operation, a history of 13,259 records containing
6,626 invocations, and a reported "operations 6761 -> 1" whose minimal repro was a one-operation
world, which cannot be non-linearizable at all. The fixture's own loadgen declares `planPath` and
never reads it (OQ-052).

**One-sided, deliberately.** The check fires only when the history holds MORE invocations than were
planned. Fewer is not disobedience: the driver is stopped at QUIESCE, so a world that ends before
its plan runs out is the normal case, and failing those would refuse nearly every short world.

**Counting invocations, not records.** The plan bounds what may be INVOKED. A planned operation may
complete, fail or time out, and phase markers share the stream, so counting every record would fire
on a fully compliant driver.

**Why in the product rather than the fixture.** Fixing loadgen would fix one driver. Any user's
driver may ignore the plan: silently, since the frozen `driver.cmd` template need not carry
`{plan_path}` at all and the environment variable is always exported whether or not the driver reads
it (D-021's dual transport is precisely what makes this failure quiet). A tool that converts that
into a reported reduction is worse than one with no stage 2.

**What caught it before this existed.** The confirmation gate, at 0/3. That is the k/k gate earning
its worlds: a layer doing its job over a layer beneath it that had failed. The gate is not a
formality and this is the evidence.

---

## D-057: the worker port mapping is CONFIG, and BOOT verifies it landed

**Choice.** Two changes, both kept:

1. `harness.prefix_env` and `harness.nodes[].port_env` declare which topology-file variables carry a
   worker's namespace and its published host port. `thesis search` reads them when `--worker-env` is
   not given; the flag still wins when it is.
2. `control.portsAgree` compares, at BOOT, the ports this world's driver will be TOLD about against
   the ports the harness OBSERVED published, and refuses the world before anything drives.

**Rationale.** The parallel executor exports a worker's port band twice: to the driver as
`PROTHESIS_TARGETS`, always; and to compose under the file's own variable names, only if told what
they are. When only the first happens, the halves disagree and nothing says so: the driver dials the
band, compose publishes the file's defaults, every operation fails, and the world hands the oracle a
full history with nothing checkable. INCONCLUSIVE is the correct answer to that question and a
useless one to receive (OQ-056).

**Why both, rather than either.** They cover different populations. Config removes the footgun for a
project that declares its variables; a bare `thesis search` on the reference fixture now works, and
the fact lives beside `compose_service`, which is the same KIND of fact: what this project's
topology file spells. The BOOT check covers every project that declares nothing, which config by
definition cannot reach.

**Why the flag still wins.** An explicit instruction must not be silently merged with config.

**What this replaces.** A comment in `parseWorkerEnv` asserting that a compose file which does not
spell those variables "publishes the same ports for every worker, and the executor's own port check
refuses to start". That check (`ParallelOptions.VerifyPortsFree`) detects two slots COLLIDING on a
port, which requires two slots. One worker collides with nobody, and one worker is what a
first-time invocation runs. The claim was false for the only case that reaches a new user.

**Cost.** None at runtime: the comparison is over a map already in scope at BOOT. `harness.*` is
outside the oracle lock's coverage, so declaring the mapping moved no gate.

---

## D-058: a history record whose KIND cannot be determined is refused, not classified

**Choice.** Every reader of a history validates the operation/marker union before it dispatches on
record kind, and refuses the history when a record breaks it. Concretely: `Validate()` gained
`op_id` and `value` to the marker prohibition; the external linearizability checker validates each
entry in `readHistory` and returns a refusal; and `oracle.ReadHistory` counts a union violation,
declines to append the record, and reports the history as unreliable.

**Rationale.** `RecordKind()` discriminates on `event` alone. That is a deliberate design (it is
what lets type `info` keep both of its normative meanings) but it means the discriminator cannot
detect its own misuse. A record carrying both an `event` and an `op_id` is not ambiguous in
principle; it is simply invalid, and every marker-skipping reader resolves it the same wrong way,
silently.

The measurement is in OQ-058 and it moved the verdict in **both** directions: a real stale read
came back `ok`/exit 0, and a genuinely linearizable history came back `violated`/exit 1 with a
confident witness. A false conviction is the one verdict a consistency oracle must never produce,
and it arrived through a dropped line rather than through a weakened assertion: the same shape as
D-045.

**Why refuse rather than repair.** Two repairs were available and both were rejected. Treating the
record as an operation (ignoring `event`) would silently reinterpret a line the writer may have
meant as a marker. Keeping it in `Entries` and letting each consumer decide reproduces the defect
one level down, since the guess every consumer makes is "marker". A record whose kind is
undecidable is not evidence of a violation and is not evidence of its absence, and INCONCLUSIVE is
the only status that says so.

**Why the fix is at the loader, not in the oracle.** `no_stuck_op` was the second reader with this
bug and it is not plausibly the last. `History.Reliable()` already existed, every built-in oracle
already refuses over an unreliable history, and the completeness fields already carried "a dropped
line may be the completion record for an operation" as their stated purpose. Counting a union
violation there costs one branch and covers every present and future consumer; patching the oracle
would have covered one.

**Cost, stated plainly.** A driver that puts an `event` on an operation record by accident now
produces INCONCLUSIVE runs instead of silently wrong ones. That is the correct direction and it is
not free. The refusal names the field, both failure modes, and the remedy.

**Not widened on speculation.** `key` on a marker remains accepted. Hiding an operation requires
the record to be dropped, every real operation record carries `f`, and `f` was already refused, so
`key` is not a vector. Recorded in OQ-058 rather than fixed, so the boundary is visible.

Pinned by `TestHiddenCompletionCannotSilenceAViolation`,
`TestHiddenTransactionCannotManufactureAViolation`, `TestLegitimatePhaseMarkersStillLoad`,
`TestAnOperationHiddenByAnEventMakesTheHistoryUnreliable`,
`TestNoStuckOpRefusesRatherThanMissingAHiddenOperation`,
`TestReadHistoryStillAcceptsRealMarkers`, and two new rows in `TestHistoryUnionValidation`.
Mutation-verified in both packages: reverting `load.go` reproduces the silent `ok` and the false
`violated`; removing the validation block from `ReadHistory` reproduces the passed stuck operation.
The two overcorrection guards fail if the fix refuses a legitimate phase marker.

---

## D-059: D-033 was wrong: narrowing a profile weakens the gate, and a narrowed run may not report PASS

**Supersedes the rationale of D-033**: specifically *"`--budget` and `--worlds` may only narrow a
profile"*, not D-033b, the injector decision further down, which shared this number until D-067
split the collision OQ-051 recorded. The flags and the narrow-only clamp stand; the claim that narrowing is
*safe* does not.

**Choice.** `--budget` and `--worlds` continue to narrow a profile and continue to refuse to widen
one. Three things change:

1. The narrowing is RECORDED. `control.Budget` carries `RequiredWall`, `RequiredWorlds` and
   `Narrowed`; `schema.Budget` gains `narrowed`, `worlds_required` and `wall_s_required`, emitted
   only when something narrowed.
2. A narrowed run that finds nothing reports **INCONCLUSIVE (exit 2)**, not PASS.
3. A narrowed run reports **`oracle_lock: bypassed`**, not `ok`.

Both consequences are applied in `BuildVerdict`, from the budget snapshot, so every command that
builds a verdict inherits them rather than each remembering separately.

**Rationale.** D-033's own rationale refutes its conclusion four lines earlier: a profile's budget
is lock-covered *precisely because shortening it is a gate-weakening move*, and then narrowing is
declared safe because "it can only make the gate harder to pass". That intuition is correct for a
budget you must fit work into and wrong for a search. Fewer worlds is fewer chances to find the
defect. Measured on this fixture, the reference world reproduces ~78% of the time: 30 worlds finds
it essentially always, 1 world finds it 78% of the time, and a 5%-per-world defect falls from 79%
detection to 5%. `thesis run --profile gate --worlds 1` was a thirtieth of the committed gate,
exiting 0 with a true `oracle_lock: ok`. See OQ-059.

**Why not simply refuse a narrowed run.** Considered and rejected. `--worlds 1` is the correct
thing to type while iterating on a fix, and a violation found in one world is still a violation:
downgrading THAT would make the flag useless and push people to edit `prothesis.yaml` instead,
which is strictly worse because it moves the digest and trains the reflex of re-locking without
reading the diff. Only PASS is downgraded. The flag stays useful for finding bugs and stops being
usable for certifying their absence.

**Why `bypassed` rather than leaving `ok`.** The digest genuinely matches the file. `ok` is a true
sentence, and that is the problem: it functions as a false one, because every reader takes it to
mean "the committed gate ran". A fourth status is cheaper than teaching readers that `ok` has a
footnote. An ABSENT lock stays absent: nothing was locked, so nothing was bypassed.

**Why the required values are additive and omitted by default.** `TestGoldenVerdictRoundTrip`
asserts byte equality against the directive's own sample and says in its comment that an additive
field must be a visible, reviewed edit. Emitting the new fields only when `narrowed` is true means
a run that took its budget from the profile is byte-identical to one recorded before D-059, so the
directive's sample stays a valid document and no world hash moves.

**One deliberate conservatism.** Capping an unbounded world count counts as narrowing whatever the
number, because nothing at budget-resolution time knows how many worlds fit in the wall budget. The
error is in the fail-closed direction and the practical cost is small: an unbounded profile that
ends on wall expiry reports budget exhaustion, not PASS.

**One duplicate removed, which was the real lesson.** `engine.resolveBudget` was a hand-copied
restatement of `Runner.Run`'s narrowing, with a comment saying so. It now calls
`control.ApplyCLIOverrides`. Had it not, `thesis search --worlds 1` would have kept the old
behaviour indefinitely: the fix would have gone into the copy that was read, and the copy that was
not read would not have been found by reading it.

Pinned by `TestApplyCLIOverridesOnlyNarrows` (seven cases including both refused widenings and the
equal-value boundary), `TestCappingAnUnboundedProfileIsANarrowing`,
`TestBudgetSnapshotCarriesTheProfilesRequirement`, `TestANarrowedRunCannotReportPass`,
`TestANarrowedRunCannotReportAnUnqualifiedLock`, and `TestTheCLIBudgetOnlyNarrows`.
Mutation-verified: removing the `BuildVerdict` invariants restores the silent PASS and the
unqualified `ok`; removing the `Narrowed` assignments restores the unrecorded narrowing. The
un-narrowed cases in each test fail if the fix over-corrects and downgrades an honest run.

---

## D-060: the oracle's executable is FINGERPRINTED outside the digest, and reported, not refused

**Choice.** `.prothesis/lock` gains an `executables` block recording each external oracle's
resolved program (name, `cmd`, resolved path, SHA-256, size). It is written by
`thesis oracles lock`, compared by `thesis oracles verify` and by every `thesis run`, and a moved,
missing or newly-appearing program is a **WARNING**, never ORACLE_DRIFT. The verdict carries
`oracle_lock.executables_moved`. The lock status stays `ok` and the exit code is unchanged.

**Rationale.** OQ-057 measured the exploit end to end: nine lines that print `"status":"ok"`,
dropped over `bin/linearizable-kv`, produced `oracle lock OK` and a PASS on a world that reproduces
~78% of the time, with no drift, no warning and nothing unusual in the artifact. The lock governs
what PRO-THESIS READS and never governed what it EXECUTES.

**Why not hash the binary into the digest.** Because that fix is worse than the defect. The digest
would move on every rebuild of the checker and on every platform (the fixture supplies the `.exe`
suffix per host) so a routine `go build` would produce exit 4, and the correct response would
become `thesis oracles lock --reason "rebuild"` without reading the diff. A lock that teaches
people to re-lock reflexively has inverted its own purpose. OQ-057 stated this objection when it
declined to close itself, and it still holds.

The precedent is already in the file: `builtin_options_fingerprint` records compiled-in thresholds
outside the digest for the identical reason (D-035), and reports movement as a warning. This
applies the same treatment to the same shape of problem.

**What this buys, stated exactly.** Not prevention: *silence removal*. A swapped checker still
produces a green run; what it can no longer do is produce one that looks untouched. That is a real
reduction in the attack's value and it is not the same as closing it, which is why OQ-057 is marked
MITIGATED rather than RESOLVED.

**Why the fingerprint uses the engine's own resolver.** `FingerprintExecutables` parses definitions
with `oracle.ParseDefinition` and resolves argv[0] with `driver.ResolveProgram`: the same calls
`process.go` makes when it executes the oracle. A fingerprint taken of a different file than the
one that will run is a false assurance, which is worse than none. D-059 records what happens when
two implementations of one rule are hand-copied: the gate stayed broken for two phases because the
copy that was fixed was not the copy that ran.

That choice has a consequence worth naming rather than discovering later: `ResolveProgram` prefers
a project-relative file over PATH, so a planted binary is both what runs and what gets
fingerprinted. The fingerprint is then consistent and useless against that attack. Logged as
**OQ-061**; not fixed here, because changing program resolution affects every driver and oracle
command and deserves its own decision.

**Why an unfingerprintable oracle is recorded, not refused.** `cmd: python check.py` resolves
through PATH at run time; a project can legitimately be locked before its checker is built. Both
produce an entry with no digest and a stated reason. Refusing to lock would make the honest
configuration the hard one.

**Why a lock with no `executables` block produces no warning.** Every existing project's first run
after this change would otherwise emit one, and a warning that fires on every project on day one is
a warning people configure away. It arms at the next re-lock.

**Cost.** One extra walk of the oracle definitions per gate check, reusing the result for the
write so a `lock` does not measure twice. The block is `omitempty`, so a project with no external
oracles and a lock written before D-060 are both byte-identical to before.

**A second cost, measured after the fact and worth stating plainly.** A project whose checker is
BUILT FROM SOURCE by each user, which is exactly what `testdata/kvfixture` is, warns on every
run for every user but the one who wrote the lock. Verified on the first clean run after this
change: rebuilding `linearizable-kv` moved it from `3f18775f9a2c` to `4dc6fb1fff54` and produced
the warning, correctly and unhelpfully.

That is the honest shape of the trade. The alternative was refusing instead of warning, which is
worse: it makes a rebuild exit 4. The alternative to BOTH is pinning a published binary rather than
building from source, which most real projects do and this fixture deliberately does not. The
fixture's README says the warning is expected and how to clear it; a downstream project that finds
the noise intolerable should pin its checker, and that is a better answer than weakening the
report. Revisit if it proves to be the thing people disable.

**Found while doing this, and fixed here.** The root `.gitattributes` rule `.prothesis/lock text
eol=lf` is path-anchored and therefore matched only a lock at the repository root, never
`testdata/kvfixture/.prothesis/lock`, the only lock this repository commits. `git check-attr`
reported no attributes for it at all. Changed to `**/.prothesis/lock`. The committed blob was
already LF; the exposure was a Windows clone checking it out as CRLF.

Pinned by `TestFingerprintHashesTheProgramThatWillActuallyRun`,
`TestAnUnbuiltCheckerIsRecordedNotRefused`, `TestDiffExecutablesReportsTheChangesThatMatter` (five
cases including the pre-D-060 upgrade path), `TestFingerprintingDoesNotMoveTheDigest` (which is
the load-bearing one, since a fix that moved the digest would be the fix this decision refuses)
and `TestASwappedProgramIsWarnedAboutAndTheStatusStaysOK`, which asserts both halves: the warning
fires AND the status stays `ok`. Mutation-verified: removing the `DiffExecutables` call restores
the pre-D-060 silence and fails both the unit test and probe A7.

---

## D-061: a bare program name is a PATH lookup, never a file in the project

**Choice.** `resolveProgram` resolves argv[0] against the project directory ONLY when it contains a
path separator. A bare name is returned untouched for `exec` to look up on PATH. The `.exe` suffix
is still supplied for path-bearing programs, because `prothesis.yaml` is portable and does not
write it.

**Rationale.** The previous order stat'd the project directory FIRST, for any program. A cloned
repository shipping a file called `python.exe` at its root therefore captured
`cmd: python check.py`: a reviewer reading the config saw system Python, and PRO-THESIS ran the
repository's binary. That is exactly what Go 1.19 removed with `exec.ErrDot`, reintroduced by hand.

It is reached by the driver (`supervise.go`) and by **every external oracle**
(`oracle/process.go:280`, with dir = the project directory), so the captured program could be the
one deciding every verdict, and the lock cannot see it, because a planted file sits outside
`oracles.dir`. Measured in OQ-061.

**Why the separator rule and not the `./` prefix OQ-061 proposed.** Requiring an explicit `./`
would have broken `thesis-helpers/steady.sh`, the relative probe in this repository's own golden
config, which carries a separator but no prefix. The separator rule is Go's, is the shell's, is
what a reader already expects, and admits every form this repository ships unchanged.

**Migration.** A config relying on the old implicit behaviour (a bare name intended to mean a file
in the project) now resolves through PATH and fails at exec with the program name. The remedy is
to write the path the config always meant: `./bin/checker` rather than `checker`.

**What this does NOT cover, stated rather than implied.** What the resolved program LOADS is
untouched: an interpreter reading a script out of the repository, a shared library, a container
image. `cmd: ./bin/checker` is now unambiguous about which file executes and says nothing about
what that file reads. D-060 fingerprints the program for the same reason and with the same limit.

Pinned by `TestResolveProgram`, which previously encoded the vulnerability as intended behaviour
while its own stated rationale described a path-bearing example. It was rewritten, not deleted, and
now plants both `python` and `python.exe` in the project directory and requires a bare `python` to
choose neither. Mutation-verified: neutering the guard returns the planted file and fails that case.

---

## D-062: the fixture driver executes its plan, and refuses one it cannot execute faithfully

**Choice.** `cmd/loadgen` implements PLAN MODE: `--plan` / `$PROTHESIS_PLAN_PATH` loads a trace,
and when the plan carries `operations` the driver replays it instead of generating a workload. One
goroutine per RECORDED process, operations in plan order, recorded op ids verbatim, targets mapped
by position, `--plan-pace` for `at_ms`.

**Rationale.** OQ-052. The field existed and was never read, the parser existed and was never
imported, and the package doc described the feature in the present tense, so stage 2 reported a
6761→1 operation reduction against a driver running its full 60,000-op budget. D-056 taught the
harness to refuse that; this makes the refusal unnecessary on this fixture rather than merely
correct.

**Four choices worth recording, each of which could have been made wrongly.**

1. **`HasOperations`, never the path, is the discriminator.** The harness has exported
   `PROTHESIS_PLAN_PATH` pointing at a PROFILE plan since Phase 1. A driver treating the variable's
   existence as "there is a trace" would execute nothing and write a history every consistency
   oracle calls clean: the vacuous pass this project has been bitten by twice.
2. **One goroutine per RECORDED process, with the recorded identities.** A process is a sequential
   thread with one operation in flight. Collapsing two into one, or renumbering them 0..n-1,
   changes which interleavings were possible and therefore which histories are linearizable. A
   replay that got this wrong could reproduce an anomaly the original run could not have produced,
   which is worse than not reproducing.
3. **Op ids are carried verbatim.** The verdict's witness names op ids; a replay that renumbered
   them could not be cross-checked against the violation it exists to reproduce.
4. **`doTxn` was split rather than inverted.** A replayed transaction executes its recorded op
   list. Decomposing that list back into the generator's read-one/write-another pair would quietly
   rewrite any transaction shape the generator does not itself produce. Plan mode chooses
   arguments; it does not add a second execution path.

**Two refusals, both fail-closed.** A plan recorded against more targets than the driver has is
refused: `Op.Target` is a POSITION, and remapping anyway would drive a different cluster than the
trace names. A plan naming an operation this driver cannot perform is refused WHOLE rather than in
part, because a trace executed partially is not the trace the minimizer is reasoning about. Both
refuse BEFORE the history writer is created: a zero-length history is indistinguishable from a run
that drove nothing and passed.

**Pacing is off by default.** A shrink candidate is run to find out WHETHER it still reproduces,
and faithful pacing makes that answer take as long as the original world did, which is the cost
stage 2 exists to avoid.

**What this does NOT establish.** That operation shrinking WORKS. A driver honouring its plan is a
precondition, not a demonstration: no evidence here says a reduced trace still reproduces the lease
defect, and there is reason to doubt it will reproduce as reliably (OQ-054's ~78% is measured on a
full workload; fewer operations means fewer chances at the stale-read window). OQ-052 stays open
and the README continues to list operation-level shrinking as not demonstrated.

Pinned by six tests in `cmd/loadgen/plan_test.go`, all against `httptest` so they need no Docker:
a 4-operation plan must produce exactly 4 invocations with the recorded ids and processes and the
recorded txn op list; a profile plan must NOT suppress the workload; the environment fallback must
work; both refusals must fire and must not leave a history behind; and a table of plan sizes must
never exceed its plan, which is the property D-056 checks from the other side. Mutation-verified:
loading the plan and not dispatching it reproduces OQ-052 exactly; "the driver issued 60000
invocation(s) for a 4-operation plan".

---

## D-063: the shrinker admits candidate drift to avoid false rejections, and corpus entries are bound to their content hash

**Choice.** Two integrity decisions in one package. The first was drafted, then found not to do
what its label said, then bounded for real; the draft's text is replaced rather than appended to,
because it was never committed and a ledger entry that describes a bound the code lacks is the
defect this repository exists to catch.

1. **Shrinker calibration: a reduced candidate may ASK, only the unreduced world may ANSWER.**
   Witness-key divergences carry PROVENANCE (`probe.go`, `divergence.unreduced`). `keyDrift` (the
   only input to `maybeCalibrate`) consults unreduced evidence alone. A divergence from a REDUCED
   candidate is a prompt: `answerDriftQuestion` re-executes the unreduced world once, at most
   `Policy.CalibrationProbes` (default 4) times per run, and only THAT execution's divergences can
   move the level. The four guardrails stand (default-only; floor at `MatchOracleClass`; oracle AND
   class must match; frozen before stage 4), and guardrail 3 ("the evidence comes from the
   UNREDUCED world") is literally true again.
2. **Corpus entries are bound to their content and their name.** `corpus.List` compares
   `schema.ShortIDFromHash(hash)` to the filename's short id and reports a mismatch as `Entry.Err`
   with no `World`, so a file carrying a world other than the one its name claims cannot be replayed
   under that name. A name that CLAIMS to be a world (the `w_` prefix, or `.thesis` anywhere in it
   in any case) that fails `ParseWorldFilename` is an `Entry.Err`, not a skipped file; a suffix test
   alone missed `w_A41F.thesis` and `w_e952.thesis.orig`. And `thesis regress` REFUSES a corpus with
   any such member, exit 2, before a run bundle exists: replaying the intact members would be a
   verdict about a corpus the command has just proved it does not fully hold.

**Rationale, shrinker.** Guardrail 3 promised the evidence came only from the unreduced world, and
`probe.go` fed reduced-candidate divergences into `maybeCalibrate` in every round of stages 1-3. The
author did that deliberately (D-054: stage-0-only "measured to fail more than half the time") and it
was the right diagnosis with the wrong cure. The numbers, from OQ-047's 27 recorded violations
(k/0 x20, k/1 x5, k/2 x2): stage 0's two draws miss a real drift on a k/0 target with probability
0.74^2 = 55%, and an uncalibrated run then fails its own 3/3 gate with probability 1 - 0.74^3 = 59%,
so roughly a third of k/0-target runs (about one default run in four overall) computed a CORRECT
reduction and threw it away. That is the false negative D-054 measured. But the cure let ANY reduced
candidate that fired the target's oracle+class on another key relax the gate in its own round, be
accepted under `AcceptTrials=1`, and pass stage 4 k/k at the relaxed level: with `relaxTo` pruning
the very divergence that triggered it from the report. A candidate that has dropped the essential
fault while a second mechanism fires the same oracle elsewhere is indistinguishable from legitimate
drift AT THE CANDIDATE, and perfectly distinguishable at the unreduced world, whose behaviour no
reduction controls. So the question may be raised anywhere and answered in one place. Bounded at 4
re-probes: two draws plus four miss a real k/0 drift with 0.74^6 = 16%, versus 55% for stage 0
alone; six would give 9% for two more worlds, and `Policy.CalibrationProbes` exists because that
trade depends on the target. Negative disables the re-probe and restores strict stage-0-only.

Three judges, given the code map independently and asked to argue anti-gaming, statistics, and
comment-honesty, each converged on this rule unprompted. Their strongest counter (that stage 0
could simply take eight draws unconditionally) was rejected because it charges every run six extra
worlds whether or not anything ever drifts; the bound charges only runs whose reductions do.

**Rationale, corpus.** Filenames were derived from the content hash on write
(`StoreWorldNoClobber`) and never checked on read; `recorder.LoadWorldExpecting` was written for
this and had no production caller. Unparseable names were dropped with `continue`, so a corpus of
nothing but misnamed files was "empty": exit 0, "holds no worlds". The README in that directory
says removing a file is a lock change; a rename to a non-parsing name was a removal the tooling
could not see.

**What this does not close.** `thesis regress` now refuses on ANY unreplayable member, including a
corrupt-but-well-named one that previously ran alongside the intact members as INCONCLUSIVE. That is
stricter and it is deliberate (exit 2 either way, but a refused corpus says so before spending a
run) and it means one damaged file halts all regression coverage until repaired. And a bound of 4
is a bound: a target whose unreduced key holds on six consecutive draws while its reductions drift
stays at `MatchWitnessKey`, which is the fail-closed direction and is stated here.

Pinned by:
- `TestReducedCandidatesMayAskButOnlyTheUnreducedWorldMayAnswer` (`calibrate_bound_test.go`); the
  attack: unreduced world holds its key on every execution, reduced candidates that DROP the
  essential fault fire the same oracle+class elsewhere; asserts the level never moves. Failed on the
  pre-bound code with the exact Calibration note the attack produces.
- `TestDriftDiscoveredAfterStage0StillCalibrates` (rewritten): the unreduced world drifts on its
  THIRD execution, the re-probe; asserts it was executed at least three times AND calibrated.
- Mutation-verified with two complementary mutations: ignoring provenance in `driftFrom` fails the
  first test on the attack note; short-circuiting `answerDriftQuestion` fails the second with
  `4 -> 4 faults, confirmation 0/1`: OQ-047's false negative, reproduced.
- `TestTheConfirmationGateCannotCalibrateItselfOpen`, `TestAFrozenProberRefusesToCalibrateOnEvidenceItWouldOtherwiseAccept`
  (now supplying UNREDUCED evidence, since that is the only kind the freeze must refuse), and the
  rest of `calibrate_test.go` unchanged.
- `TestListRejectsWorldWhoseFilenameDoesNotMatchContentHash`; mutation: neutering the comparison
  accepts `w_0000.thesis` as OK.
- `TestListSurfacesMalformedThesisFilenamesAsErrors` (four shapes incl. `.orig` and uppercase);
  mutation: a suffix-only test yields 3 of 4.
- `TestRegressRefusesACorpusWithAMemberItCannotReplay` (`cmd/thesis/regress_test.go`); mutation:
  removing the refusal allocates run bundle `r_2026_09_13_c6bd` for a corpus that cannot run.

---

## D-064: the module path is `github.com/WrdCstlg/pro-thesis`, because the path D-002 chose belongs to someone else

**Choice.** The Go module path moves from `github.com/prothesis/prothesis` to
`github.com/WrdCstlg/pro-thesis`, the repository's actual public location. 173 Go files and
`go.mod` change their import prefix and nothing else; the test runner's package-name regex follows.
The nested fixture module keeps `prothesis.dev/kvfixture`. D-002 is amended in place with a pointer
here rather than rewritten.

**Rationale.** D-002 chose a path under a GitHub account this project never held. Checked on
2026-09-14: `github.com/prothesis` exists and is an unrelated user's account. So
`go install github.com/prothesis/prothesis/cmd/thesis@latest` could never have resolved, and any
claim that the tool is installable was false for as long as the path stood. A module path is a
statement about where the code lives. Making the repository public was the moment that statement
became checkable by someone other than the author, and it did not survive the check.

**What this does not close.** `prothesis.dev/kvfixture` is a path under a domain nobody in this
project controls either. It is never fetched (the fixture is built in place by `docker compose
build` and by `go test` from inside its directory) so the false claim is inert, but it is the same
kind of claim and it stays until the fixture is given a real home or a deliberately non-resolving
one. Two design documents under `docs/design/saboteur/` quote the old path as the state of the tree
on the day they were written; they are snapshots and are left as written.

Pinned by: `go build ./...`, `go vet ./...`, `gofmt -l`, and the full suite at the new path (1463
pass, 0 fail; the rename is mechanical, and a missed file fails to compile). Whether
`go install github.com/WrdCstlg/pro-thesis/cmd/thesis@<commit>` resolves is checkable only after
this commit is pushed, and is not claimed here.

---

## D-065: a second target, upstream etcd, with a driver written the way a third party would write one

**Choice.** `targets/etcd/` is a PRO-THESIS project against unmodified etcd v3.5.17, three
members, the upstream image. Its driver (`targets/etcd/cmd/etcdload`) lives in the MAIN module and
imports exactly one thing from it: `pkg/schema`, the published history types. Its steady-state
probe (`cmd/etcdsteady`) runs on the host and reads the topology from the `PROTHESIS_*` environment
the harness exports, because the etcd image ships no shell. Only four built-in oracles are enabled
(`no_crash`, `no_panic_log`, `availability_after_heal`, `no_stuck_op`); the two that need a
`/status` endpoint or a shell in the container are left out and the reason is written in the
config (OQ-063). The driver's read consistency is chosen by the MIX (`read` is a linearizable read,
`read_serializable` a serializable one, both recorded as `f: "read"` with `meta.read_mode`), so the
one variable the experiment turns on is visible in `prothesis.yaml` and in every history record.
The expectation was pre-registered in `targets/etcd/.prothesis/PREREGISTRATION.md` before the
first run.

**Rationale.** Until this existed the harness had only ever run against a fixture built to carry
one bug it was built to find, and every headline number was a statement about that pair. The
question a second target answers is not "is etcd correct" (nothing here claims that beyond etcd's
own documentation) but "does the same unmodified checker, with the same fault injectors,
discriminate a correct read mode from an incorrect one on a system nobody here wrote". The fixture's
driver is a separate module by rule (D-021's wire-format argument: a reference driver sharing the
harness's types proves nothing about a third party's). This driver deliberately takes the other
side of that argument: it IS the third party, written against the published schema package the way
a user would write one, and if `pkg/schema` is not enough to write a sound driver, that is a finding
about the package.

**Measured, 2026-09-15.** Arm A, `--profile linear --fault 'net.partition(etcd-n2)@3000..7500'`:
run `r_2026_09_15_8c77`, 2 of 2 worlds PASS, exit 0. The partition bit: every operation on
`etcd-n2` from t+3.5 s to the heal timed out (16 clients, three rounds of 2 s timeouts), 32
`info` records inside the window, 0 `fail`. Arm B, `--profile stale`, same fault: run
`r_2026_09_15_9a29`, world 1 FAIL, exit 1, `linearizable.kv` on `k/0` by exhausted search (3,451
states over 12,699 steps, every branch rejected). Both verdicts matched the pre-registration.

**The mechanism did not.** Re-deriving staleness from etcd's OWN `header.revision` rather than from
the checker (an `ok` read whose response revision is below the revision of a write acknowledged
before the read was invoked) finds 313 revision-stale serializable reads in arm B's history and 0
in arm A's. They are on all three members, not on the partitioned one, and they begin at t+69 ms,
long before the fault window opens. Ordinary replication lag under sixteen clients is enough to make
a serializable read non-linearizable; the partition was not necessary. The pre-registration's
prediction was right and its causal story was wrong, and that is recorded here rather than the
story being revised to fit.

**What the first run found.** `r_2026_09_15_ace7`: every world FAIL on `availability_after_heal`
with "HTTP status 404" on all three members, while the health probes the config declares answered
200 throughout. The post-HEAL convergence probe in `runner.go` did not use the declared template; it
used the literal `"/healthz"`, the fixture's path. Fixed by `convergenceProbes` /
`harness.ProbeTemplates` (OQ-063), and the fixture is unaffected because its template is the path
that was hard-coded.

**What this does not close.** Single-key register operations only: the checker says nothing about
transactions, leases, watches or multi-key reads. `role:leader` cannot be targeted on this system
(OQ-063). Two built-in oracles cannot run against it. The op-level shrink of arm B's world is
reported in OQ-063 as measured, whichever way it came out.

Pinned by:
- `TestConvergenceProbesUseTheNodesOwnHealthTemplate`, `TestConvergenceProbesDoNotInventAURLForANodeWithoutAHealthEntry`
  (`internal/control/converge_test.go`); mutation: restoring the literal `/healthz` fails the first
  with "was probed at …/healthz; the declared template ends in /health".
- `targets/etcd/cmd/etcdload/main_test.go`: a paired, schema-validated history in profile mode; the
  read mode the mix asked for on every read record; operation mode executing a trace verbatim (op
  ids, values, per-process order) and refusing an empty trace, an unknown `f`, or more targets than
  it has; drain on `{"cmd":"stop"}` leaving no operation in flight; the classification table (a
  timeout is `info` whether or not the request was written; a refused dial is `fail`; a 5xx is
  `info`); and a server that refuses everything producing no `ok` record.
- The two arms above, re-run from `targets/etcd/` with the commands in the pre-registration.

---

## D-066: the harness asks the target who leads, judges availability on the whole window at the declared URL, and locks the health probe

**Choice.** Five changes in one package, every one of them found by pointing the harness at a system
it was not built around (D-065) and then auditing what it did there (OQ-063).

1. **`harness.role_probe`**, new and optional: a host-side program exec'd exactly like
   `steady_state.probe`, with the same `PROTHESIS_*` environment, printing one JSON object per
   node; `{"node","role","term"}` or `{"node","error"}`. `ExecRoleObserver`
   (`internal/control/roleprobe.go`) implements `perturber.RoleObserver` over it. A node the probe
   does not name is UNOBSERVED, never a follower by default; a non-zero exit, unparseable output, or
   the deadline expiring fails the resolution, the same fail-closed path a 404 on `/status` takes.
   Absent, `HTTPRoleObserver` reads `/status` as before, so the fixture is untouched.
   `targets/etcd/cmd/etcdrole` is etcd's probe: leader iff the member's own id is the id it reports
   as leader, follower iff another non-zero id is, an error line when it reports no leader.
2. **The post-HEAL convergence probe spans the window.** One direct probe per node at QUIESCE start
   and one after the 2 s convergence sleep, each stamped when it is taken; any 2xx is available (the
   rule BOOT and telemetry already applied); and `availability_after_heal` receives every telemetry
   probe sample too (`readProbes`), where before it received them only when no direct probe existed.
3. **A node with no declared probe is not judged.** `oracle.Input.HealthProbed` carries the nodes
   `harness.health` covers; `availability_after_heal` names the others in its OK text as not judged,
   keeps INCONCLUSIVE for a declared node that was never probed, and returns INCONCLUSIVE when
   nothing is declared. The directive's own sample config (a `pg` node with no probe) is valid again
   under this rule and was never valid under the old one.
4. **The lock covers `harness.health` and `harness.role_probe`.** Both committed locks were
   re-locked with this entry as the reason; `TestManifestGolden`'s digest moved and the old value is
   recorded beside the new one.
5. **Three honesty fixes.** `resource_return_to_baseline`'s OK text names the metrics it could not
   evaluate instead of claiming "every resource metric"; telemetry's three pointers to OQ-021, which
   is about `io.latency`, now point at OQ-063 item 3, and so does the absence reason written into
   every shell-less target's `telemetry.jsonl`; `harness.ProbeTemplates`' comment no longer claims
   `WaitHealthy` resolves through it (BOOT waits on every matching entry, which is stricter).

**Rationale.** A `role:` target on etcd resolved against a 404 and exited 2 on every world, which is
fail-closed and useless: the guided search pins `role:leader` on its first rung, so half its action
space was dead on the one target that was not the fixture. The alternative (teaching the harness
etcd's maintenance API) would put per-system knowledge in the wrong place. The target knows its
own status document; a probe in the target's directory is auditable, replaceable, and can be
lock-covered, and since it decides which node a fault hits it must be. `harness.health` joins the
lock for the same reason one step later: D-065 made the declared probe the URL the availability
verdict rests on, and an agent that cannot pass that oracle must not be able to repoint the probe at
an always-200 path without ORACLE_DRIFT; `TestUncoveredEditsDoNotMoveDigest` had documented the
hole as scope, not as intent. The window change follows from the oracle's own words: it reports a
node "failed to become available within the Nms convergence window" on one attempt at the window's
first millisecond, while the run's telemetry file held four in-window answers it never read. Not
judging an undeclared node follows from what "available" means: nothing, until a probe defines it;
turning a config omission into a full-world INCONCLUSIVE blocked the verdict on the nodes that WERE
probed, and asserting availability of an unprobed node would have been false.

**Measured, 2026-09-15.** etcd, `--profile stale --fault 'net.partition(role:leader)@3000..7500'`:
run `r_2026_09_15_d939`, `role:leader` bound to `etcd-n2`, injected at t+4341 ms (the probe binary's
first execution on this host cost about 1.3 s inside the window; the second and third runs injected
at t+3038 and t+3020 ms), FAIL exit 1 on `linearizable.kv`. `--profile linear`, same fault: run
`r_2026_09_15_fa3e`, bound to `etcd-n1` in both worlds, PASS 2/2. Smoke `r_2026_09_15_f9cb` PASS.
The fixture's quick-start command still FAILs on `linearizable.kv` (`r_2026_09_15_093c`). Before
this entry every `role:` target on etcd exited 2.

**What this does not close.** `no_unbounded_queue` and `resource_return_to_baseline` against a
shell-less, status-less image (OQ-063 items 2-3; the audit's designs are recorded there). The role
probe reports what a member says: a member cut from its peers may keep naming the leader it last
knew until its election timeout, and `role:leader` then binds by the highest reported term; the
same tie-break `HTTPRoleObserver` applies, now fed by the probe. A new probe binary's first exec on
Windows can cost a second inside the fault window; nothing here hides that, the realized schedule
records the actual injection time.

Pinned by (each mutation-checked, the mutant named):
- `TestRunnerUsesTheConfiguredRoleProbe`, `TestExecRoleObserverBindsRolesFromTheProbeOutput`,
  `TestExecRoleObserverErrorLinesAreUnobservedNodes`, `TestExecRoleObserverFailsClosedOnExitErrorOrGarbage`
  (`internal/control/roleprobe_test.go`); mutant: unconditional `HTTPRoleObserver`, "a declared
  role_probe must select ExecRoleObserver, got *perturber.HTTPRoleObserver".
- `TestLeaderIsTheMemberWhoseOwnIdIsTheReportedLeader` (`targets/etcd/cmd/etcdrole`).
- `TestConvergenceProbesUseEachNodesOwnTemplatePortAndStatus`: three servers on three ports, one
  answering 404, one 204, a counting clock; mutants: every node expanded against node 1's port
  ("etcd-n2 was probed at …:61063/health, want …:61064"), `== 200` ("etcd-n3: OK=false"), one stamp
  per round ("etcd-n2 stamped t+1814ms, want t+1815ms"). The previous test bound every node to one
  port and could not catch any of the three (audit finding, this iteration).
- `TestReadProbesMergesDirectAndTelemetryAttempts`; mutant: return the direct probes when non-empty;
  "want 2 direct + 3 telemetry attempts, got 2".
- `TestAvailabilityDoesNotJudgeANodeWithoutADeclaredProbe` and three siblings
  (`internal/oracle/availability_declared_test.go`); mutant: ignore `HealthProbed`, "want ok, got
  inconclusive: 1 node(s) were never probed … pg".
- `TestResourceOKTextNamesTheMetricsItCouldNotEvaluate`; mutant: unconditional text, "the OK text
  asserts metrics that were never evaluated".
- `TestCoveredHarnessProbeEditsMoveTheDigest`; mutant: drop `harness.health` from `CoveredPaths`,
  "editing health_probe_path did not move the digest".
- `TestProbeTemplatesFirstDeclarationWinsAndUncoveredNodesAreAbsent`, `TestProbeTemplatesRefusesASelectorFormItCannotResolve`.

---

## D-067: a ledger id is an address: collisions are split by suffix, every citation must resolve, and a test enforces both

**Choice.** The second topic under each colliding number keeps its position and gains a `b`:
OQ-021b, OQ-022b, D-031b, D-032b, D-033b. Every citation of those five numbers was read against its
subject and only the six that meant a second topic were rewritten; the rest already meant the first
entry and are unchanged, so no existing citation (including those in commit messages, which cannot be
edited) changes meaning. Two Phase 1 code comments that cited entries never written got those
entries (OQ-064, OQ-065). `internal/ledger` parses both ledgers, and two tests fail the build on a
second defining heading for an id, a follow-up section with nothing above it to follow, or a citation
anywhere in the tree to an entry that does not exist.

**Rationale.** OQ-051 refused renumbering on a sound argument (a pointer silently aimed at the wrong
entry is worse than a duplicate that announces itself) and then left the fix as a documentation task
for later. Two outside readings of the public repository named it the first thing a careful reader
hits, in the place the project's claim to rigor lives. Reading the citations settled the cost: 85
lines, of which 58 pointed at a first entry whose twin was never cited at all, and 6 needed rewriting.
A suffix rather than a new number at the end of the file keeps each entry beside the work it was
written with, and keeps "D-031" meaning what 51 citations already take it to mean.

The test is the part that matters. A one-time cleanup of an append-only document written by several
agent sessions in parallel would collide again the next time two sessions append at once, which is
how the original five happened. Checking citations as well as headings is what found the two
never-written entries: "Logged as OQ-021" and "Logged as OQ-022" were written on 2026-09-06 when the
ledger ended at OQ-018, and the next day's unrelated entries answered them. No reader following those
comments could have known they had landed on the wrong subject.

**What this does not close.** The suffix is a convention the test recognises (`[a-z]?`), not a
numbering scheme; a third topic under one number would need a `c`, and the test would accept it.
Documents under `docs/protocol` and `docs/design` are verbatim historical snapshots and are exempt
from the citation check; they carry no dangling citation today, and nothing keeps it that way. Commit
messages are outside the tree and outside the check. OQ-051's count tables are left as it measured
them, including the three "collisions" that were follow-up sections.

Pinned by (`internal/ledger/ledger_test.go`):
- `TestTheCommittedLedgersHaveUniqueAddresses`: the committed ledgers.
- `TestEveryLedgerCitationInTheTreeResolves`: failed before OQ-064, OQ-065 and this entry were
  written, naming exactly those citations and no others.
- `TestCheckReportsADuplicatedDefiningHeading`, `TestCheckReportsAFollowUpWithNothingToFollow`,
  `TestParseRefusesAHeadingItCannotClassify`, `TestParseClassifiesDefiningAndFollowUpHeadings`: the
  parser on synthetic ledgers.

---

## D-068: CI on a hosted Linux runner: the first measurements not taken on the build machine

**Choice.** `.github/workflows/ci.yml` runs on GitHub's `ubuntu-24.04` runners, in the public
repository only. The unit job builds, vets, checks formatting and runs the full suite on Go 1.22.x
(the version `go.mod` and the README declare) and on stable; the stable job also runs the race
detector, pulls the images the fault-family integration tests refuse to pull themselves, and turns on
the two Docker-backed fixture tests. The live job runs five worlds' worth of pre-registered
expectations with every exit code asserted through `scripts/ci-run.sh`, which also prints each
oracle's conclusion and evidence counts for every world: the fixture's planted defect (exit 1), its
patched negative control over all five worlds (exit 0), and on etcd the smoke profile and both
pre-registered arms with `role:leader` targets (exit 0, 0, 1). Verdicts and per-world results are
uploaded as artifacts either way.

**Rationale.** A second outside reading of the repository named the remaining weakness precisely:
every number was produced on one machine, by this tool, judged by these oracles. Nothing in the
repository can make the judging independent, but the machine can be. A hosted runner is a different
kernel (native cgroup v2, routable container networks, no Docker Desktop VM), a different Docker, a
different CPU count, and nobody's hardware. Asserting exit codes is what makes it a measurement rather
than a smoke test, and printing the oracle evidence is what makes a green step readable as one: a world
that drove nothing would still exit 0 on some oracles, and the explanation text says how many
operations were actually checked. The private mirror receives the same pushes and is excluded so it
spends no organisation minutes.

**Measured so far, 2026-09-15.**

- Run `35013974434`, the first: every job green on its first attempt. The fixture's defect found in
  world 1 (`r_2026_09_15_9bd7`, `linearizable.kv` by exhausted search on 460 operations of `k/0`);
  the patched build PASS on 5 of 5 worlds with the partition bound to the live leader each time
  (`kv-n2`, `kv-n3`, `kv-n1`, `kv-n2`, `kv-n1`); etcd smoke PASS 2/2, arm A PASS 2/2, arm B FAIL on
  `linearizable.kv` (`r_2026_09_15_9f66`). The race detector passed on both modules. The etcd
  pre-registration's verdicts therefore reproduced on hardware other than the build host.
- That run did not measure three things, which is why the workflow was revised: per-test counts, the
  Docker-backed fixture tests (skipped for want of their opt-in), and seven fault-family integration
  tests (skipped for want of two base images).
- Run `35015380182` turned the fixture tests on and one failed; run `35015859119` printed why. The
  test had been asserting, for five days, a verdict D-059 forbids: OQ-066. The same run counted
  1,523 tests passing, 1 failing and 3 skipping on the stable job, with the seven integration tests
  now running.
- Run `35016418522`, after the OQ-066 fix, every job green:

| Job | Result |
|---|---|
| unit, Go 1.27.1 | 1,524 pass, 3 skip, 0 fail. Both Docker-backed fixture tests pass; the skips are the three search-engine tests that need recorded run bundles. Race detector clean on both modules. |
| unit, Go 1.22.12 | 1,522 pass, 5 skip, 0 fail. The two extra skips are the fixture tests, opted in on the stable job only. |
| fixture, buggy, `linear`, `net.partition(role:leader)` | exit 1 in world 1: `linearizable.kv` exhausted search over 1,877 operations on `k/0` |
| fixture, `kvfixed`, same fault | exit 0, 5 of 5 worlds `pass`; each world's `linearizable.kv` checked all 8 keys, between 14,108 and 14,855 operations completed per world |
| etcd `smoke` | exit 0, 2 of 2, 500 operations each |
| etcd arm A, `linear`, `role:leader` | exit 0, 2 of 2, 9,514 and 9,529 operations |
| etcd arm B, `stale`, `role:leader` | exit 1 in world 1: `linearizable.kv` exhausted search over 1,345 operations on `k/0` |
| residue | no container or network left behind |

  Counts include subtests, as the build host's scripts count them; on the build host the same tree
  counted 1,509 pass and 18 skip with the Docker engine down.

**What this does not close.** The runner is a second machine, not an independent judge: the same
code, the same oracles and the same expectations run there. Runner timing differs from the build
host's, so a probabilistic expectation (arm B finding a stale read in two worlds) can fail there for
reasons that are not defects, and a failure must be read before it is believed. Nothing here
exercises Windows, which is where every earlier number came from and where the Docker engine died
twice in a day under the adversarial suite.

---
## D-069: the build directive and the Phase 4 closure prompt are removed from the repository and from its history

**Choice.** Two documents are no longer published: the base build directive (23,404 bytes) and the
prompt that asked the agent to close Phase 4. Both were removed from the working tree AND from every
commit, by rewriting all 32 commits with `git filter-repo --invert-paths` over the five paths those
files ever had, including the byte-identical `CRUCIBLE_FABLE_PROMPT.md` copy that D-001 records and
that had already been deleted from the tree on 2026-09-14. The author's decision, taken after the
agent set out the scope and the consequences of rewriting history. Addendum A and the two phase
briefs stay.

**What changed, precisely.** Every commit hash in the repository changed, both remotes were
force-updated, and the two `copilot/*` branches on the public remote, which carried the unrewritten
history, were deleted, which closed the redundant pull request #1. Verified afterwards: no object
path matching `*PROMPT*` in any ref, no commit adding or removing one, and no blob anywhere
containing the directive's own header line or the closure prompt's opening line.

No pre-purge commit hash is printed anywhere in this repository, and the two that were (one in an
earlier draft of this entry, one in a commit message) were redacted from history in a second pass.
That is not fastidiousness. **Measured immediately after the force-push:** GitHub served the removed
file at `raw.githubusercontent.com/<owner>/<repo>/<pre-purge-sha>/docs/protocol/…` with HTTP 200,
while the same path on `master` returned 404. A published pre-purge hash is therefore a working link
to the file this entry says was removed.

**Rationale.** Publishing the directive was a deliberate disclosure: it let a reader check that a
rule the ledgers keep citing was actually a rule, and two outside reviews named it as the thing that
made the agent authorship undeniable. The author has decided the directive itself is not for
publication. What the disclosure was for is preserved in other form: `docs/protocol/README.md` states
the rules the directive imposed and says plainly that it is withheld, the README's "How this was
built" section says the work was done by the author and AI coding agents together under a written
protocol, and every place the implementation departed from the directive is still an entry in one
of the two ledgers.

**What this does not close, and what a reader should know.**

- **History rewriting is not deletion everywhere, and this was measured rather than assumed.** A
  pre-purge commit still resolved on GitHub after the force-push and still served the removed file.
  Redacting the hashes from this repository raises the cost of finding one; it does not remove the
  objects, and GitHub's own public activity feed records the before-and-after hashes of every push.
  Forks, caches and any existing clone are untouched, and anyone who cloned before 2026-09-15 has
  the files. The only remedies that actually delete the objects are asking GitHub support to
  garbage-collect the repository, or deleting and recreating it.
- **Names and excerpts remain.** Ledger entries written before today cite the old filenames (D-001's
  rationale is left as written, because this ledger is append-only and superseded by entry, not by
  edit) and the verbatim design snapshots under `docs/design/` quote short passages. The longest
  quoted run anywhere in the tree is three lines; nothing reproduces either document.
- **Every other commit hash changed**, so any link, bookmark or note pointing at a pre-purge commit
  in either remote is now dead, including the hashes this ledger cites for earlier work.
- A full pre-purge bundle of all refs was taken before the rewrite and kept outside the repository,
  so the removal is recoverable by the author and by nobody else.

---

## D-070: build provenance travels inside the binary; sut.images records the resolved image ID, not the tag

**Decision.** Two halves, one purpose; an artifact must be able to say what produced it:

1. `internal/buildinfo` carries three `-ldflags -X` stamps (`Commit`, `Dirty`, `BuildTime`),
   injected by `scripts/build.ps1` into `thesis`, `thesis-oracle-linearizable` and the fixture's
   `loadgen`. `verdict.json`'s `commit` is the binary's stamp when present and the working tree's
   HEAD only as the unstamped fallback (`buildinfo.VerdictCommit`), because what executed is the
   binary, not the tree. The stamp stays hex-only: `commit` is consumed by `thesis bisect`, so the
   dirty bit is reported by `thesis version` rather than decorated into the field. An unstamped
   binary runs unimpaired: provenance is metadata, and its failure mode is the empty string, never
   an error (same contract as `control.GitCommit`).
2. `harness.ResolveImages` inspects every bound container at BOOT and writes (logical service,
   image reference, resolved image ID) into `World.SUT.Images`, closing OQ-055. The digest is the
   container's `.Image` image ID (a content address no tag drift can fake) not `RepoDigests`,
   which is empty for the local builds this harness exercises. Entries are deduplicated per distinct
   (service, image, digest), so three nodes on one image produce one entry and a mixed fleet
   produces two. Resolution failure degrades to the schema's honest `[]` with a stderr warning; it
   never blocks a world.

**Why now, and what it costs.** OQ-055 warned that populating `sut.images` moves every new world's
hash, because the digest lands inside the canonically encoded, content-addressed world file. That
is accepted: historical worlds keep their hashes and their empty `images`; only worlds written after
this change carry digests. The committed regression corpus still reproduces: its world hashes are
computed over its own files, unchanged. The first live run with the resolver
(`r_2026_09_17_1a13`) recorded `prothesis/kvfixture:buggy` at its image ID for service `kv`, and
rebuilding the oracle binary with stamps triggered the lock's binary-digest warning exactly as
OQ-057 intends; a rebuild is a warning to re-lock with a reason, not drift.

**Rejected.** Stamping the containerized kv node binary: the image digest already identifies it
more strongly than any in-binary string. Hashing the oracle binary INTO the lock: OQ-057's design
deliberately keeps program digests outside the definition lock so a rebuild warns rather than
exit-4s; this entry changes nothing there. Recording the stamps in verdict.json as a new field: the
verdict's key set is frozen surface, and `thesis version` plus the world file cover the need.

## D-071: PERTURB nests inside DRIVE, and every world written before this keeps a DRIVE that does not span it

**Decision.** `runWorld` opens PERTURB with `PhaseLog.Nest` rather than `Advance`, and closes it with
`Unnest` before HEAL and in the cancellation defer.

`Advance` closes the open primary phase, so DRIVE ended at the instant PERTURB began. `PhaseLog.Nest`
existed for exactly this (`pkg/schema/phase.go` and the comment at the call site both say PERTURB
overlaps DRIVE) and had no callers anywhere in the tree.

**What the artifacts actually show**, measured across every `.thesis` file on this machine rather
than asserted: 777 world files carry a DRIVE window, all 777 also carry PERTURB, and **0 of 777 have
a DRIVE window that spans its PERTURB**. DRIVE covers only the milliseconds between SEED and the
first fault: 238 are literally 0..0, the rest run out to 36 ms, median 1 ms. So the pre-fix marker
is NOT the literal `0..0` it is tempting to quote: the committed regression corpus
(`testdata/kvfixture/.prothesis/regressions/w_e952.thesis`) records DRIVE 0..1 with PERTURB 1..7906.
The reliable discriminator is the failure to span, not the width.

This is not only a reading defect in `phases.json`, though it is that too: a reader of that file
alone would conclude no traffic was driven, while the driver in fact ran through PERTURB and on into
HEAL. It also moves attribution: `PhaseTimings.At` returns every window covering a timestamp and the
oracle engine takes the lowest lifecycle ordinal, so a finding carrying `first_seen_ms` is labelled
PERTURB while DRIVE does not span it and DRIVE once it does. The OBS-LIVE-001 worlds do not exhibit
the flip only because the external oracle supplies no `first_seen_ms` and the engine pins those to
ASSERT.

Pinned by `TestPerturbNestsInsideDriveInTheWorldFile` (`internal/control/phase_test.go`), which
asserts against the world FILE rather than the `PhaseLog` API: the API was always correct, the call
site was not, and an API-level test passes either way. Reverting the call site to `Advance` fails it
with `DRIVE is 0..0, a zero-width window, while PERTURB ran 0..60`. Verified by mutation, not assumed.

**What this does not fix.** Every world recorded before this keeps a DRIVE window that ends where
PERTURB begins. Nothing rejects such a window, so those worlds stay valid and replayable. But a
sample that mixes pre-fix and post-fix runs mixes two attribution semantics, and `verdict.json`
records nothing that says which produced it. L3's statistical gating must not pool runs across this
change; the cheap discriminator is the world file itself, where a DRIVE window that spans its PERTURB
means post-fix.

## D-072: the build stamp records the source's date, so the oracle fingerprint is a function of source

**Decision.** `scripts/build.ps1` stamps `Commit`, `Dirty` and `SourceDate` (HEAD's committer date)
and builds every binary with `-trimpath -buildvcs=false`. Nothing stamps a wall clock.

**Why.** `linearizable-kv.exe` is fingerprinted by SHA-256 outside the lock digest so a swapped
checker is visible (D-060, OQ-057). A wall-clock `BuildTime` put that mechanism in direct conflict
with ordinary work: every rebuild of identical source produced different bytes, so the "oracle
program changed" warning fired on every build and the honest response to it (re-lock with a reason)
decayed into a reflex. The lock moved four times on 2026-09-17. Two of those reached git: `13:56Z`
under "rebuilt with build-provenance stamps from unchanged source", which was false, since the same
commit added 17 lines to the checker's `main.go`; and `21:51Z` under "lock stabilized via strict
source determinism", which names no cause at all. A warning that fires unconditionally trains its
reader to dismiss the one case it exists for.

`-buildvcs=false` is the second half and matters as much as the date. Go's own VCS stamping embeds
`vcs.revision`, `vcs.time` and `vcs.modified` independently of `-ldflags`, so on a clean tree adding a
single unrelated UNTRACKED file flipped `vcs.modified` and moved the checker's digest: the lock would
then report a changed checker because somebody left a scratch file lying about. Provenance is not
lost by dropping it: commit, dirty and source date are stamped explicitly and read back by
`thesis version`, the oracle's `-version`, `loadgen -version` and `go version -m`.

**Measured, not assumed.** `scripts/build.ps1 -Verify` builds the checker a second time to a scratch
path and compares digests, re-deriving the stamp rather than reusing the first one: an earlier draft
froze one `-ldflags` string and handed it to both builds, which would have let a reintroduced clock
pass unnoticed. On this working tree (f775e61 plus this change, so the stamp reads `commit f775e61
dirty=1`) two builds agreed on
`sha256:14b1a567c6843e0e59c8b9d304c9d4ae097d62e06458c8473c73c84d92e5f838`, and adding a stray
untracked file left that digest unchanged. Note this digest is a property of THIS TREE, not of
f775e61: the change adds `buildinfo.SourceDate` and the oracle's `source_date` readback, both of which
compile into the checker, and f775e61's own lock records a different value.

**The scope of that claim, exactly.** `-Verify` measures that two builds ON THIS MACHINE agree, which
is what the lock needs. `-trimpath` removes the build machine's absolute paths, which is one
requirement for reproducing off it, but not sufficient: the binary still embeds toolchain and target
identity, and nothing here pins the Go version (`go.mod` carries no `toolchain` directive), `GOFLAGS`,
`GOAMD64` or `CGO_ENABLED`. A different Go patch release or a C compiler appearing on PATH produces
different bytes with `-trimpath` in force.

**The git calls that produce the stamp are now checked.** `$ErrorActionPreference` does not apply to
native commands, and `git status --porcelain` unguarded failed OPEN: a non-zero exit with no stdout is
falsy and reads exactly like a clean tree, stamping `Dirty=0` (an affirmative claim about the source
that nothing had verified) into the one artifact whose whole job is provenance.

**Correction to D-070.** D-070 states the `commit` field stays hex-only because it "is consumed by
`thesis bisect`". That reason does not hold: nothing reads `verdict.json`'s `commit`. `bisect`
resolves revisions itself through `git rev-parse` (`internal/bisect/git.go`), and the only consumers
of `Verdict.Commit` in the tree are the writer in `internal/control/verdict.go` and a print in
`cmd/thesis/run.go`. `buildinfo.VerdictCommit` therefore appends `+dirty`, so a verdict produced from
an uncommitted tree says so in the artifact and not only on the terminal.

**Residual, carried under OQ-057.** A commit that touches nothing the checker compiles still moves its
`Commit` stamp, so a rebuild after any commit costs one re-lock. Stamping each binary with the last
commit that touched its own dependency set would remove that; it is not done, because it would make
two binaries built together report different commits.

## D-073: image resolution is bounded in time and reached through a seam, so a daemon that will not answer costs a world its provenance and at most five seconds

**Decision.** Two halves, closing OQ-067.

1. `harness.ResolveImages` bounds its one `docker inspect` with a five-second timeout of its own
   (`imageResolveTimeout`), independent of whatever the caller's context allows. Five seconds is
   two orders of magnitude above the healthy case (a daemon on the build host answered an inspect
   in 44–57 ms over five samples on 2026-09-19) and pinned by a test of its own. Exceeding it is
   reported as its own error (`docker inspect did not answer within 5s`) so the runner's existing
   fail-soft warning names the cause instead of printing a bare `context deadline exceeded` that reads
   like the world's budget. A deadline inherited from the caller is reported as the caller's, never as
   ours: from the bounded context alone the two are indistinguishable, and only the parent's own error
   tells them apart.
2. `control` gains an `ImageResolver` seam (`RunnerOptions.ImageResolver`), alongside `Driver`,
   `Telemetry`, `OracleEngine` and `LogCollector`, for the same reason those exist: the production
   implementation asks the daemon, and a unit test that drives a stub backend with invented container
   ids has no daemon to ask and must not find out whether one is healthy. `NewRunner` defaults to the
   daemon-backed resolver when none is named, so `thesis run`, `search`, `replay`, `regress` and `shrink`
   record provenance without being told to; every unit-world constructor names `stubImages`.

**Why, measured.** OQ-067 records the event: after a disk-full incident corrupted the Docker content
store, `docker inspect` on a NONEXISTENT container took 32 seconds to return a 500. Two tests that
drive a stub backend (`TestPerturbRunsARealScheduleAndRecordsIt` and
`TestPerturbEmptyScheduleStillPasses`) each ran 94 seconds against a 60-second profile budget and
failed `exit = 3 (BUDGET_EXHAUSTED), want 0`. Both had passed minutes earlier on identical code and
both recovered with no code change once the daemon was healthy. A metadata lookup the workload never
made spent the budget, and the verdict blamed the budget. A test whose result depends on the health of
a daemon it does not use is not a unit test.

**Pinned by six tests and seven mutations, each shown to fail, not assumed to.**

- `TestResolveImagesGivesUpOnADaemonThatNeverAnswers` (`internal/harness`): a stand-in daemon that
  blocks on `ctx.Done`, a caller with no deadline. With the `WithTimeout` replaced by `WithCancel` it
  fails in exactly 2.00 s on its own select, quoted as emitted: *"ResolveImages has not returned: it
  is waiting on the daemon, bounded by nothing but a caller that set no deadline"*.
- `TestResolveImagesReportsTheCallersDeadlineAsTheCallers` (`internal/harness`): a caller whose
  deadline has already passed. With the `ctx.Err() == nil` guard removed it fails with the false
  explanation the guard exists to prevent, quoted as emitted: *"the caller's own deadline expired, but
  the error blames the inspect bound: docker inspect did not answer within 5s: context deadline
  exceeded"*. An expired deadline rather than a cancellation, deliberately: a cancelled parent never
  enters the timeout branch and could not tell whether the guard was there. The same test also
  requires the caller's abort to be named ("aborted by the caller's context") and classifiable
  through `errors.Is`, because docker's own wrap of a killed child reads "signal: killed" (a daemon
  failure) for a deadline the harness set. With the naming branch removed it fails, quoted as
  emitted: *"the error does not attribute the abort to the caller's context: context deadline
  exceeded"*.
- `TestSUTImagesComeThroughTheResolverSeam` (`internal/control`): a recording resolver on a full
  unit world, asserting both that it was asked and that its answer reached `world.thesis`. With the
  call site restored to a direct `harness.ResolveImages` it fails, quoted as emitted: *"the runner
  never asked the ImageResolver: provenance is being resolved somewhere the seam does not reach, so
  the seam protects nothing"*.
- `TestNewRunnerResolvesImagesFromTheDaemonByDefault` (`internal/control`): names no resolver and
  asserts the daemon-backed one was installed. With the default removed it fails, quoted as emitted:
  *"with no ImageResolver named, NewRunner installed <nil>; production provenance depends on this
  being the daemon-backed resolver"*. This is the test that guards production: every stub-driven test names its resolver
  explicitly, so without this one the default could silently become nothing and `sut.images` would
  read `[]` in every live world while the suite stayed green.
- `TestAResolverFailureCostsProvenanceNotExecution` (`internal/control`): a resolver that cannot
  reach its daemon, on a full unit world. Before it, nothing in the tree exercised the fail-soft
  branch at all: every other resolver stub answers `nil, nil`, so the promise this entry repeats
  from D-070 was pinned by no test. It asserts the world still reaches exit 0, that `world.thesis`
  records `sut.images` as `[]`, and that the cause is printed. With the BOOT call site changed to
  return the error instead of warning it fails, quoted as emitted: *"exit = 2 (verdict INCONCLUSIVE),
  want 0: a provenance failure must not end the world"*.
- `TestImageResolveBoundIsFiveSeconds` (`internal/harness`): the one test that reads the default,
  because every other test here shortens it. With the bound silently changed to five minutes it
  fails, quoted as emitted: *"imageResolveTimeout = 5m0s, want 5s: the ledger documents five seconds
  and this is the only test that reads the default"*.

**What a timeout records, and costs.** Exactly what any resolution failure already recorded:
`sut.images` is `[]` and stderr carries `image digest resolution failed: docker inspect did not
answer within 5s: … (sut.images will be [])`. Nothing new was invented; the existing fail-soft path
is now reachable in bounded time. The cost is up to five seconds of the world's wall budget (about
8% of the 60 s profile OQ-067 measured) spent inside BOOT, which is the price of asking at all.
The artifacts still record no REASON for an empty `sut.images`: a timeout, a daemon error and a stub
resolver all leave `[]` and only stderr says which. That is D-070's shape, unchanged here.

**What this does not fix.** The bound is a constant, not configuration, on purpose: a knob would need
a config key, and provenance is metadata that is not worth a budget under any setting. A daemon that
is healthy but genuinely slower than five seconds now loses the world's provenance where before it
would have waited; that trade is explicit. `internal/search/engine`'s test runner constructs a
`Runner` without naming a resolver and inherits the daemon default; it runs no world today, and the
day it does it must name `stubImages` like the others. The driver's discarded stdout and stderr
(OQ-067's neighbour) are untouched and remain L1c's. And the bound is only as hard as `cmd.Run`
returning once the child is killed: `runEnv` sets no `WaitDelay`, so a docker child whose output
pipe stayed open could hold the call past five seconds. Not observed on this host, not tested, and
not changed here: a `WaitDelay` in the shared exec path would want its own test, which no permitted
subprocess on this host can provide. Finally, the attribution guard keys on the bounded context
(the only signal docker's exec wrap leaves, since a killed child returns "signal: killed" and never a
context error) so a genuine daemon failure that lands in the same instant the bound fires is
reported as the bound. That window is the width of one scheduler tick, and it is named here rather
than rounded away.

**Rejected.** Making the stub backend bind empty container ids so resolution short-circuits before
the daemon: that hides the dependency instead of naming it, and the next stub with ids brings it back.
Placing the bound at the runner's call site rather than inside `ResolveImages`: any other caller
would inherit the defect.

---

## D-075: `search.strategy: llm`: distadv's retry/feedback/temp-0 machinery ported into `internal/search/llm`, speaking PRO-THESIS's own fault grammar

**Decision.** A fourth search strategy, `llm`, proposes each world's fault schedule by asking a
local OpenAI-compatible model endpoint (default `http://localhost:8080/v1/chat/completions`, a
llama.cpp server running Qwen3.8, verified answering at temperature 0 on 2026-09-20). The
machinery (the retry-with-rejection loop, the verbatim-rejection prompt, the three-valued
feedback coaching, the temperature-0 transport, the scripted-server tests) is ported and adapted
from the author's `distadv` project (a library built on the Z.ai platform): its chat transport, its
retry loop and rejection prompt, its three-valued feedback outcome, and the shape of its prompt
builder. `distadv` lives OUTSIDE this repository, in a directory this tree ignores, so no file or
line in it is cited here: a reader could not resolve one. Attribution recorded here because the
code carries it only in comments.

**What was NOT ported, and why.**

- **distadv's DSL.** Its 7-primitive fault grammar maps onto nothing in the frozen 17-kind
  registry (`pkg/schema/fault_kinds.go:150-239`); vendoring it would have been a silent
  reinterpretation layer between the model and the executor. Proposals are written in
  PRO-THESIS's own wire format (`KIND(target[,params])@start..end`), parsed by
  `schema.ParseFaults`, canonicalized, and then refused or accepted by `search.Validator`, which
  IS `internal/perturber.Compile`, the same code path that validates human-authored schedules.
  The prompt's grammar primer is built programmatically from `search.Space` and the schema
  registry, so it cannot drift from the parser.
- **distadv's missing timeout.** Its client has none; the port carries `search.llm.timeout_ms`
  (default 120000) and honours context cancellation, both pinned by tests against a hung server.
- **The Next.js console and mini-services** in `GLM Build`: not harness machinery.
- **A temperature knob.** Temperature is always sent as 0. The plan sketched an
  `acknowledge_nondeterminism` escape hatch; it was dropped: a knob that defaults to
  reproducible but can be turned is a knob that will be left turned, and the residual
  nondeterminism temp 0 does not close (server version, hardware) is recorded as accepted risk in
  OQ-069 rather than gated behind a flag that changes nothing we send.

**The guards, and where they live.**

- **Never an unvalidated world.** `validateSchedule` (wire format, per-world fault ceiling,
  perturber policy) and a final `ValidateWorld` gate both stand between the model and the
  executor; a rejected proposal is retried with the validator's error verbatim in the next prompt,
  up to `search.llm.max_retries` (default 2).
- **Exhaustion is loud.** Retries exhausted is an error that is NOT `search.ErrExhausted`:
  ErrExhausted means "the space is enumerated", and a model that never produced a legal schedule
  is a broken integration, not a finished search.
- **Unknown is never clean.** Observe coaches from `Outcome.Signal()`'s three values; the unknown
  branch directs a milder schedule and never text from the clean branch (OQ-033's rule).
- **Workers are pinned to 1 in `engine.New`** (`internal/search/engine/engine.go` `buildStrategy`
  `case schema.StrategyLLM`): the strategy coaches each proposal from the previous world's
  outcome, and a parallel pool would interleave proposals and observations out of order. An
  explicit `--workers > 1` is refused with a CONFIG_ERROR-class message (cmd/thesis maps engine
  construction errors to exit 5); an unspecified `--workers` defaults to 1 instead of
  DefaultWorkers. Chosen over CLI-side enforcement because the engine is the last common point
  every caller passes through.
- **The recording lands at `<run-dir>/llm-proposals.jsonl`.** The run directory is known at
  `engine.New` time (`control.NewRunner` allocates it), so the strategy is constructed eagerly in
  `buildStrategy` with `e.runner.RunDir()`: no lazy wiring. One JSON line per attempt:
  `{ordinal, attempt, prompt, response, schedule, validator_error, accepted}`. Deliberately not a
  frozen contract; it is the audit trail for a stream that is not seed-reproducible (OQ-069).
- **Config.** `StrategyLLM` joins the closed `search.strategy` enum; `search.llm`
  (`endpoint`, `model`, `api_key_env`, `max_retries`, `timeout_ms`) is defaulted by
  `DefaultLLMConfig` and validated by `ValidateSearch` only when llm is the selected strategy, so
  its defaults never gate a saboteur run. **Lock implication, measured: none for an existing
  project.** `internal/lock/config.go` does cover the whole `search:` block, but `ProjectConfig`
  projects the RAW `prothesis.yaml` bytes and says of itself that "a compiled-in default is
  unreachable from here", so a key nobody wrote produces no entry. Measured 2026-09-20 with a
  binary built from this tree: `thesis oracles verify` exit 0 in both projects, fixture
  `sha256:0ebddd17…` and etcd `sha256:872c9151…`, both unchanged. No re-lock is needed and none was
  made. (An earlier draft of this entry said the defaults move both locks and deferred a re-lock;
  that was asserted, not measured, and a re-lock "for a reason" that is not one is the failure
  D-072 was written against.) A project that WRITES a `search.llm` block does move its own digest,
  which is the point: repointing the endpoint is a lock change with a recorded reason.

**Failing-first and mutation evidence.** The tests were written first and failed as BUILD-FAIL
(`pkg/schema`: `undefined: StrategyLLM, LLMConfig, DefaultLLMConfig`; `internal/search/llm`:
`undefined: Options, newClient, chatRequest`; `internal/search/engine`: `undefined:
schema.StrategyLLM, schema.DefaultLLMConfig`). After implementation all passed, and seven
mutations were each shown to fail, quoted as emitted:

1. Both validation gates removed (`validateSchedule`'s `Validator.Validate` AND `world`'s
   `ValidateWorld`; each gate alone is redundant with the other by design, and removing only one
   left `TestAPolicyDeniedKindIsRejectedAndRetried` green, which is the defense-in-depth working):
   *"a denied kind reached a world: [clock.skew(kv-n1, ms=100)@4000..9000]"*, *"callCount = 1,
   want 2"*.
2. Unknown folded into clean in `coaching`: `TestObserveMapsTheThreeSignalsToThreeCoachings`
   failed; the unknown-world prompt carried the clean coaching verbatim.
3. Exhaustion returned as `ErrExhausted`: `TestExhaustedRetriesFailLoudlyNotErrExhausted`,
   *"the error wraps ErrExhausted (llm: ordinal 0: search: the strategy has no further worlds to
   propose (2 calls; last rejection: fault_schedule.planned[0]: fault \"still.bogus\": missing
   @start_ms..end_ms window)): a model failure must not read as a completed enumeration"*.
4. Rejected attempts not recorded: `TestEveryAttemptIsRecorded`, *"1 lines recorded, want 2 (one
   per attempt)"*.
5. Client timeout removed (distadv's original defect): `TestClientTimesOutOnAHungServer`,
   *"the hung server held the call for 10.0013768s; the timeout is the contract"*.
6. Workers pin removed from `buildStrategy`: `TestTheLLMStrategyIsBuiltWithOneWorker`,
   *"an unspecified --workers left opts.Workers = 0, want it pinned to 1"*.
7. `llm` dropped from `AllSearchStrategies`: `TestSearchStrategyLLMDecodes` and
   `TestLLMDefaultsSurviveDecode`, *"search.strategy: unknown value \"llm\" (want one of
   [saboteur random hybrid])"*.

**Rejected.** Vendoring distadv wholesale (above). A temperature knob (above). Enforcing
workers==1 in `cmd/thesis/search.go` instead of the engine: any other caller of `engine.New`
would inherit the hole. Lazy strategy construction at `Run` time for the recording directory: the
run dir exists at `New` time, so the indirection would buy nothing.

**What this does not do.** No campaign has run, and the one live world that has is NOT validation
evidence. After the code was written, the writing session ran `thesis search --strategy llm
--profile search --worlds 2` against the fixture (run `r_2026_09_20_178c`): the model proposed
`net.partition(role:leader)@2000..10000`, it bound to `kv-n2`, and `linearizable.kv` reported an
exhausted-search violation on `k/0` (683 operations, 2,822 states over 9,324 steps), with
`llm-proposals.jsonl` written. Read back from the artifacts by a different session, three caveats
attach. (1) The binary was a root-level `thesis.exe` from a bare `go build` (`thesis version`
says "unstamped") so `verdict.json` fell back to the tree's HEAD and records `commit: 53cf41c`
while twelve files, this feature among them, were uncommitted: exactly the misattribution D-070
exists to prevent. (2) `--worlds 2` narrowed the profile, so the verdict carries `oracle_lock:
bypassed` (D-059); correct, and it means the run could not have reported PASS. (3) The exit code
was reported by the session that ran it, not read in a shell by an observer. It shows the wiring
works end to end once; a stamped run is owed. `llm-proposals.jsonl` is audit, not a frozen
contract, and nothing reads it back yet.

---

## D-076: the adversary loop: a worker that bombards, a builder that widens, an arbiter that judges, and a human on every high-risk item

**Decision.** The design is `docs/design/09-adversary-loop.design.md`; this entry records the
rulings it rests on, because they were made by the author and the ledger has recorded an author's
ruling exactly once before (D-001). Nothing described here is implemented except the worker, which
is D-075.

**The author's rulings, 2026-09-20, in the order they were made.**

1. **The layer above the worker produces both hypothesis families and planted defects.** A
   hypothesis family is typed data the worker instantiates; a planted defect is a new bug written
   into the reference fixture as a build variant, with a pre-registered claim about which fault
   makes which oracle fire. A planted defect the campaign fails to detect is a measured gap in the
   harness.
2. **The system is compatible with any model, cloud or local.** Three transports: OpenAI-shaped
   chat completions, Anthropic's Messages API, and a `command` adapter spoken to over
   stdin/stdout (the idiom external oracles already use) so a model behind anything else needs
   no code change. Qwen is the local default on the build host; model identifiers are the user's
   and pass through verbatim.
3. **Every high-risk item requires human feedback, presented with its full diagnosis.** The
   design's register classes twelve risks; the GATED ones produce a review packet (what, why with
   run ids, the exact bytes and their hash, pre-registration, blast radius including what leaves
   the machine, what could go wrong, reversibility, how to verify, static findings, expiry), and
   an approval is one append-only line bound to that packet's hash.
4. **The loop is a `thesis campaign` verb, not a script**, so it inherits the exit-code contract,
   provenance and tests. It stops unconditionally on exit 4 or 5 and refuses an unstamped binary.
5. **The budget follows the deployment environment, specified up front or evaluated by a model.**
   `thesis doctor` measures; fixed formulas derive a safe envelope; a model may propose within it;
   only a human raises a ceiling.
6. **Order of work:** the D-075 commit and its corrections, the OQ-068 fix, retention floors,
   the k-of-n gate (OQ-054), then the design's stages.
7. **Model selection follows the task.** Building is done by the second most capable model
   available; the arbiter (review, adjudication, the last read before a human) is the most
   capable coding model. The harness cannot measure capability, so the tier is the user's
   declaration; what the harness enforces is separation: the builder and the arbiter may not
   resolve to the same provider, endpoint and model unless a lock-covered override says so, and
   every review packet then states that they were the same. The arbiter may reject; it may never
   approve. The same ruling governs the agent sessions that build this repository (AGENTS.md §9).

**Rationale for recording rulings rather than only the design.** The design document can be
revised; who decided what, and when, should not move with it.

**What this does not do.** It allocates no budget constants, no k-of-n floor and no packet expiry
window: the design lists those as the author's to set when reached. It does not resolve OQ-040 or
OQ-045, which still wait for a ruling.

---

## D-078: the harness switches off buildx's default attestations for every target, and the fixture's build context no longer contains its own run corpus

**Decision.** Two changes, mitigating OQ-068.

1. **Every docker invocation the harness makes carries `BUILDX_NO_DEFAULT_ATTESTATIONS=1`**
   (`internal/harness/docker.go`, `dockerEnv`), placed after the inherited environment and BEFORE
   the per-request one. So the harness's default beats whatever the operator's shell exports, and a
   per-request entry (D-042's override channel) still beats the harness.
2. **`testdata/kvfixture/.dockerignore` excludes `.prothesis/`.** Nothing under it is needed to
   compile the fixture.

**Why the switch lives in the harness and not in a compose file.** OQ-068's own candidate (2) was
`provenance: false` and `sbom: false` under `kv-n1.build`. That would have fixed exactly one
target, ours, and only on a Compose new enough to know those keys. The defect is not the
fixture's: `sut.images` is the harness's claim about ANY system under test (D-070), the harness owns
the docker invocation, and a target's compose file is not ours to edit. Set in the environment, the
switch reaches every target on every Compose version that builds through buildx, and changes
nothing inside the image.

**Measured on the build host, 2026-09-20, through `docker compose build` (the path the harness
uses) in this order.** Default environment, two consecutive builds of an unchanged context:
image ids `5ed78c4668f24538…` and `3496bee40ba0d0a6…` (different, which is OQ-068 reproduced).
Switch set, two consecutive builds: `0a229cb155a622b0…` both times. Switch set and a NEW directory
created under `.prothesis/runs/` between builds, which is what every run does: the same id again,
0.7 s wall, and `docker system df` reported the build cache at 11.32 GB in 22 entries before and
after; against +5.4 GB for the same event the day before. The first build after `.prothesis/`
left the context took 5.6 s; the day before, a build with an unchanged context took 13.1 s.

**Failing-first and mutation evidence.** `TestEveryDockerInvocationSwitchesOffDefaultBuildAttestations`
re-executes the test binary in docker's place and reads the variable from inside the child; it sets
the inherited value to `"0"` first, so it cannot pass because the machine running the suite exports
the switch. Against the unfixed code both subtests failed (*"the child saw
BUILDX_NO_DEFAULT_ATTESTATIONS="0", want "1""*) and `TestTheFixtureBuildContextExcludesItsOwnRunCorpus`
failed: *"does not exclude .prothesis/"*. Three mutations, each shown to fail and then restored
byte-for-byte: `dockerEnv` dropped from `runEnv` (both attestation subtests, on `"0"`); `dockerEnv`
appended AFTER the per-request entries (`TestAPerRequestEnvironmentCanStillOverrideTheSwitch`, on
`"1"`, "the harness default must sit BEFORE the per-request entries"); the `.prothesis/` line
deleted (the fixture-context test). Full suite afterwards: 32 packages ok, 0 not ok, 38 total; vet
exit 0; gofmt clean.

**Rejected.** The compose keys (above). `--provenance=false` on the command line: the harness calls
`compose up`, which builds implicitly and has no such flag. Excluding only `.prothesis/runs/`: the
lock, the oracle definitions and the corpus are not build inputs either, and a narrower pattern is
one more thing to keep true. Dropping `pull_policy: build`: the variant lock depends on it, and
with this change a rebuild costs under a second.

**What this does not do.** It does not make an image id survive a COLD build cache: the image
config's `created` field still moves, and pinning it needs `SOURCE_DATE_EPOCH` with a value the
harness has no principled source for on a target it does not own. That half of OQ-068 stays open.
It sets no SBOM switch, because buildx attaches no SBOM by default. And a target whose own
configuration turns attestations back on gets the unstable identity it asked for.

**Update 2026-09-20, later: live, and the re-lock this change cost.** Four live worlds on binaries
stamped `69e13fc dirty=0` are tabulated in OQ-068: exit 1 in all four, cold BOOT 20 s where it had
been 181–187 s, warm BOOT 7.4–7.5 s where it had been 14.9–21.1 s, 518.8 MB of cache for a full cold
build where one cold world had written 5.70 GB, one `sut.images` digest across three consecutive
worlds, and a different digest after a second prune; the limitation above, measured.

Rebuilding moved the checker's fingerprint (`fac33f30d966…` → `76b5ededccd7…`), and the usual
question (did only the stamp move?) was answered by the usual experiment and the answer was NO.
The pre-change tree built with the old stamp reproduces the locked digest byte for byte (the
control: the method can find it); the CURRENT tree built with that same old stamp gives
`afa058602382…`. The checker imports `pkg/schema`, and D-075's additions to `config.go` and
`config_validate.go` are linked into it. Nothing under `cmd/thesis-oracle-linearizable`,
`internal/buildinfo` or `internal/recorder/cjson` changed. The lock's `reason` says all of that.
A reason reading "only the stamp moved" would have been the third false one; D-072 counts two.

---

## D-081: the comprehensive test protocol is adopted verbatim, and the em dash audit its release gate names is now a script

**Decision.** Two additions.

1. **The author's comprehensive test protocol is committed as
   `docs/protocol/VERIFICATION_PROTOCOL.md`, verbatim as supplied.** It restates the invariants
   this repository already runs under (soundness, measured claims, the refusal surface, exit codes
   0-5, bitwise determinism), fixes the 5-tier verification hierarchy, the failing-first and
   mutation authoring cycle, host discipline, statistical trial frames, and a 10-stage
   pre-publication release gate. It declares itself normative and routes every deviation through
   this ledger, which is how this repository already works.
2. **`scripts/emdash-audit.ps1` implements the gate's punctuation stage**, which named a script
   that did not exist. It scans every tracked Markdown file for U+2014 and exits 1 naming the
   offenders. Nine files are grandfathered by exact path, measured on 2026-09-21 to hold all 95
   existing occurrences: the three verbatim briefs in `docs/protocol/` and six snapshot documents
   under `docs/design/` (the set `docs/protocol/README.md` describes as kept as written). The list
   is exact, not a directory pattern, so a NEW em-dashed file under those directories fails too.

**Measured, failing-first and mutation evidence.** On the clean tree:
`pwsh -File scripts/emdash-audit.ps1` printed "em dash audit OK (36 markdown files scanned, 9
grandfathered)", exit 0. Mutation: one U+2014 appended to `AGENTS.md`; the script printed
"EM DASH AUDIT FAILED: AGENTS.md: 1 em dash(es)", exit 1. `AGENTS.md` was then restored
byte-for-byte (`git diff` empty) and the script returned to exit 0.

**Rejected.** Scanning all tracked text, code included: 53 `.go`, `.ps1` and `.py` files carry
U+2014 in string literals and comments, and the protocol scopes the rule to prose and markdown.
Grandfathering `docs/design/` and `docs/protocol/` wholesale by pattern: a new file would inherit
the exemption silently, which is the soundness invariant's failure mode applied to punctuation.

**What this does not do.** It does not touch the en dash, arrow and ellipsis characters the
ledgers already use; the audit is U+2014 only, matching the gate row. It does not make the gate's
"Repository Visibility" row retroactively true: the public repository was published at the
author's decision before this protocol arrived, and the row's "PRIVATE until human user
authorization" criterion is read from that point on as "private until the author says publish",
which is the standing rule in AGENTS.md section 6. And it does not run the gate: the first full
execution is reported with the commit that lands this entry.

---

## D-082: the known-problem registry and `thesis diagnose`: every recorded refusal attributes to a catalogued cause, or the diagnostic itself goes red

**Decision.** Three pieces.

1. **`pkg/schema/knownproblems.go`: the `prothesis.knownproblems/v1` registry schema.** Each
   entry is a KP id, a title, a classification (`accepted-refusal`, `by-design`, `defect-fixed`,
   `defect-open`, `operational`), a mandatory OQ-/D- ledger citation, a level (`world` or
   `bundle`), and a conjunctive matcher over that level's facts. Registry order is precedence
   order; the first full match wins.
2. **`internal/diagnose`: the attribution engine.** Walks `.prothesis/runs` read-only. Every
   world whose outcome is not `pass`/`violation` attributes to a world-level entry; every world
   dir with no `result.json` attributes to KP-007; every run whose verdict is not PASS/FAIL, and
   every bundle with no verdict at all, attributes to a bundle-level entry. There is deliberately
   NO catch-all entry: an item matching nothing is UNATTRIBUTED, printed with its evidence, and
   makes `thesis diagnose` exit 2 (INCONCLUSIVE: the diagnostic refuses to claim coverage). A
   catch-all would be the silent tautology the authoring protocol exists to catch.
3. **`thesis diagnose`** (`cmd/thesis/diagnose.go`): exit 0 when everything attributed, 2 when
   anything is not, 5 for a missing or invalid registry. Exit 1 and 4 are never used: no oracle
   ran and no lock was checked.

**The recursion.** A future run that produces a NEW kind of refusal attributes to nothing,
`thesis diagnose` and `TestTheRealCorpusIsFullyAttributed` go red, and the fix is never to
broaden a matcher past what was measured: investigate, add a KP entry with its ledger citation,
watch it go green. The catalogue grows by accretion of measured causes.

**Measured on the real corpus, 2026-09-21** (`go test -v -run TestTheRealCorpusIsFullyAttributed
./internal/diagnose/`): 184 bundles, 958 world dirs, zero unattributed. KP-001 rtb no baseline
245; KP-002 nuq insufficient QUIESCE evidence 24; KP-003 nso QUIESCE<SLO 4; KP-004 lin nothing
checkable 4; KP-005 legacy record 6; KP-006 history missing 1; KP-007 dead world 156; KP-008
narrowed downgrade 12; KP-009 budget exhausted 1; KP-010 dead-world verdict 17; KP-011
zero-worlds search 1; KP-012 folded-up refusals 13; KP-013 interrupted search 26; KP-014
unrecorded bundle 12. Cross-check against OQ-072's census: world-level 245+24+4+4+6+1+156 = 440
= 284 recorded inconclusive + 156 dead worlds; bundle-level non-terminal verdicts
12+1+17+1+13 = 44 = 146 verdicted - 79 FAIL - 23 PASS; verdict-less 26+12 = 38. Exact. World
attribution is first-match per world: a world whose rtb AND nuq both refuse counts once, at
KP-001, which is why KP-002 reads 24 against the census's 190 per-oracle instances.

**Failing-first and mutation evidence.** The refusal tests were written before the engine was
exercised on them. Three mutations, each shown red and reverted byte-for-byte: (i) the
unattributed branch disabled (`if false && id == ""`): `TestAnUnknownCauseIsUnattributedLoudly`
failed, "expected exactly one unattributed world, got []"; (ii) the reason matcher gutted
(`return p.ID` replaced by `continue`): `TestInconclusiveWorldAttributesByOracleReason` failed,
"expected full attribution, got [{world r_2026_09_21_bbbb world-0001: outcome="inconclusive"
...}]"; (iii) the bundle verdict predicate gutted: `TestAnUnknownBundleShapeIsUnattributedLoudly`
failed, "expected one unattributed bundle, got []". A first attempt at (iii) was itself defective
(it skipped every verdict-bearing entry instead of ignoring the mismatch) and PASSED the test,
which would have been a mutation that proves nothing; it was corrected and re-run to red. Full
package afterwards via `scripts/run-tests.ps1 -Match internal/diagnose`: ok, 0.2 s.

**Rejected.** A catch-all entry (above). Multi-attribution (count every matching KP per world):
totals would stop reconciling against the census. Backfilling reasons into recorded bundles:
recorded artifacts are immutable; the 34 runs whose reasons never persisted stay attributed to
OQ-071's class, which is honest about being unrecoverable. Persisting reasons prospectively in
verdict.json/result.json: worthwhile, unscheduled, does not change the registry's job.

**What this does not do.** It changes no recorded verdict and no exit code of any run; diagnose
reads, it never writes the corpus. It does not fix OQ-071 prospectively (dead worlds still write
no result.json; they are now at least attributed). It does not make the search-engine corpus
tests loud when the corpus shrinks (the census-floor work remains open and is a prerequisite for
any pruner, OQ-070). And on the build host, measured 2026-09-21: four consecutive freshly built
`bin/thesis.exe` binaries (stamped and unstripped) were refused by the Application Control
policy, so the diagnose VERB was exercised only through the test suite against the real corpus,
not as a CLI process; `cmdDiagnose` compiles into the same binary, and the policy refusal is the
OQ-067 class of host flakiness, not a code signal.

---

## D-083: a committed census floor makes a shrunken corpus fail the suite instead of skipping it

**Decision.** `testdata/kvfixture/.prothesis/CENSUS.json` (schema `prothesis.census/v1`) records
`bundle_floor: 184`, `verdicted_floor: 146`, the two protected run ids, and the contract: the
floor may be lowered only by a documented prune that rewrites the file in the same change. The
`.gitignore` rules ignore only `runs/`, `corpus/` and `tmp/` under `.prothesis/`, so the census
is committable without touching ignore rules. In `internal/search/engine`, every corpus-reading
test now reaches the corpus through one helper, `corpusBundles`, which skips ONLY when the runs
dir does not exist (a fresh clone has recorded nothing; that skip says "nothing to check", never
"checked and fine"), keeps log-only behaviour on pre-census checkouts so bisecting old revisions
is not bricked, and fails on every census violation when both exist. `.trash` and dot-directories
are not bundles and are not counted.

**Measured.** Corpus re-counted before writing the floor: 184 bundles, 146 verdicted
(`Get-ChildItem testdata/kvfixture/.prothesis/runs -Directory`), matching the OQ-072 census taken
the same day. The two protected ids were confirmed present first.

**Failing-first and mutation evidence.** `TestCensusViolationsFlagsAShrunkCorpus` builds a
synthetic corpus below every floor and demands exactly 3 violations naming each shortfall; the
real corpus is never touched by tests. Mutations, each shown red and reverted: (i) the bundle
floor comparison inverted to `>=`: `TestCensusViolationsAcceptsTheFloorAndGrowth` failed, "a
corpus exactly at its floor must report no violations, got [corpus holds 2 run bundle(s), below
the census floor of 2: ...]"; (ii) the protected-id check dropped: `...FlagsAShrunkCorpus`
failed, "must report 3 violations, got 2".

**Rejected.** An env-var opt-in (the OQ-066 pattern: a check that is off by default is a check
that silently never runs; this one is on by default wherever the corpus exists). Failing when
the runs dir is absent: that would brick CI and fresh clones, which legitimately hold no corpus.
Counting `.trash` contents toward the floor: trash is evidence staged for deletion, not evidence.

**What this does not do.** It does not implement the pruner (OQ-070 stays open); it makes the
pruner's danger loud BEFORE the pruner exists, which is the only safe order. It also cannot
distinguish "the whole runs directory was deleted" from "fresh clone": both skip, because the
committed CENSUS.json must not brick CI checkouts that legitimately hold no corpus. The floor
guards shrinkage below 184/146 with the corpus present; wholesale deletion of the corpus
directory remains indistinguishable from never having run.

---

## D-084: the clustering engine: DBSCAN + HDBSCAN discovery over the unattributed pool, and a Ward taxonomy against the KP entries

**Decision.** The author's written spec, implemented with four recorded deviations. Three verbs
under `thesis cluster`: `extract` (Tier 1 structural features per unattributed outcome to
`cluster_features.csv` + `cluster_manifest.json`), `discover` (DBSCAN with elbow-tuned epsilon +
pure-Go HDBSCAN + the five-rule consensus into CANDIDATE / CONTESTED / NOISE; exit 2 when any
CONTESTED), and `taxonomy` (Ward agglomeration over KP and candidate centroids, inconsistency-cut
domain/family/variant levels, sub-type/split/orphan insights; exit 0). New package
`internal/cluster`; corpus access goes through `diagnose.Scan`, an additive read-only export of
the diagnose walk (`internal/diagnose/scan.go`) that reuses the same matchers and cannot
disagree with `Diagnose`. Reports land under `.prothesis/` and are gitignored as derived
artifacts.

**Deviations from the spec, with reasons.** (1) NO gonum: D-002's dependency rule stands; Ward
linkage is implemented directly via Lance-Williams updates. (2) HDBSCAN is Option A, pure Go;
the Python bridge would add a runtime dependency and a nondeterminism surface for a report-only
verb. (3) The spec's "exit 1 on error" is overwritten: 1 is the oracle-violation code and
clustering renders no verdict, so errors exit 5. (4) Features map to fields that exist on disk:
Component = first refusing oracle's name; ExitCode = driver.exit_code; HasBaseline =
telemetry.json presence; WorldFixture = verdict.json profile; DurationMs = search.json duration
(0 when unrecorded, documented); reason statistics over refusing oracles' explanation+error text.

**Invariants kept.** No auto-promotion and no registry writes; read-only against the corpus
(proven by `TestPipelineAgainstTheRealCorpusIsReadOnly`: one SHA-256 over sorted corpus paths +
contents, identical before and after the full pipeline); noise is sacred; no catch-all;
deterministic (no randomness, total-order tie-breaks), idempotent modulo the `generated`
timestamp.

**Measured, 2026-09-22.** Full suite: 34 ok, 0 not ok, 40 total (`scripts/run-tests.ps1`).
Real corpus: 522 attributed, 0 unattributed, so the live pool is empty; `cluster extract` exit 0,
`discover` exit 0 with `unattributed_input: 0` and `clusters: []` (valid YAML, empty-pool path),
`taxonomy` exit 0 over 14 KP centroids (4 domains, 12 families, 5 advisory split candidates, no
sub-types or orphans). The populated paths are proven on synthetic fixtures.

**Failing-first and mutation evidence** (from the builder's report, spot-verified): every test
written before implementation and red as BUILD-FAIL (undefined Ranges/ConsensusPoint/Scan, "no
non-test Go files"); two real bugs caught and fixed (a Lance-Williams matrix indexing error
producing 0-distance merges; green oracles' explanations diluting the reason feature - now
refusing oracles only). Four mutations, each red then reverted: prob>0.7 gate removed
(`TestConsensusHDBSCANOnlyLowProbabilityIsNoise`: "must be NOISE, got CANDIDATE"); Jaccard
0.6->0.0 (`TestConsensusDisagreeingClustersAreContested`: "must be CONTESTED, got CANDIDATE");
noise reassigned to the largest cluster (noise count 2->0 in `TestDiscoverPipelineTwoClustersTwoNoise`);
Ward->single linkage (`TestWardMergeOrderOnFiveCentroids`: "merge 1 members: got [0 1 2], want
[3 4]").

**Rejected.** Python HDBSCAN bridge (above). gonum (above). A "promote" verb: the spec itself
defers it, and it would need human-in-the-loop design. Tier 2 semantic features (TF-IDF /
embeddings): deferred by the spec; Tier 1 structural only.

**What this does not do.** It does not change any verdict, exit code of a run, or the registry;
it is a lens, not a gate. On today's corpus it has nothing to cluster (zero unattributed); its
value lands the first time a NEW refusal shape appears faster than it can be hand-catalogued.
Built by Kimi Code CLI (a delegated session); nobody has arbitrated it yet.
