# 03-harness.verify

## Verdict

**NEEDS-REVISION**

## Spec fidelity

Mostly faithful: no renamed schema keys, no invented enum values in prothesis.yaml, exit codes 0-5 used with the right names. Four real drifts:

(1) EXIT-CODE DRIFT. §1.5 step 2: "preflight host ports ... in-use -> exit 5 naming the port". §3.2: "Preflight net.Listen's each one first and fails with exit 5 naming the port." §3.4 Tier 3: "No execution path resolvable -> exit 5 at thesis doctor / preflight". The directive freezes 5 = CONFIG_ERROR = "Invalid prothesis.yaml or CLI arguments" and 2 = INCONCLUSIVE = "Harness/environment error (e.g. docker daemon dead)". A host port held by a foreign process, and the absence of any usable shell on the box, are environment facts, not invalid config. Addendum §I makes the difference operational: 5 tells the agent loop "fix prothesis.yaml", 2 tells it "retry once, then escalate to human"; the design sends the agent to edit a file that is not wrong.

(2) LIFECYCLE DRIFT. §5.6 table: `--no-teardown` -> "HEAL runs (faults withdrawn): **no; faults stay injected**", with "Exit code: unchanged". Directive §4.1 makes HEAL a mandatory linear phase and §4.3's Critical Guarantee is "HEAL must verify that no residual network rules or stopped processes persist." A global flag named for TEARDOWN silently deleting HEAL is a semantic invention, and "exit code unchanged" means a verdict is still emitted from a run that skipped a normative phase.

(3) SCHEMA SHAPE DRIFT. §4.2 freezes `harness.file: docker-compose.yaml` (a single string) and `steady_state: {probe: ..., timeout: ...}`: a mapping. §1.5 templates `-f {{F}}...` ("each `-f` file, absolute") throughout, i.e. it assumes `harness.file` is a list; §3.4's heading writes `steady_state: "thesis-helpers/steady.sh"` as a bare string. Neither is announced as a schema change.

(4) WORLD-TUPLE ADDITION. §8: "`ResolvedTarget` *is* part of the world". I2 freezes the world as `(seed, topology_variant, driver_profile, fault_schedule, phase_timings)`. ResolvedTarget carries `Observation string` ("term=4") and `ResolvedAt`: runtime observations. Folding observations into the world is either an addition to a frozen tuple or an unstated redefinition of `fault_schedule`. §2.7's `--replay-targets=recorded|live` is likewise an invented flag absent from addendum §H's CLI surface.

Not drift, and worth crediting: `io.prothesis.*` labels, `.prothesis/state/`, and the `Caps` bitmask are additive and touch nothing frozen; §5.4's warning that `internal/lock` (the oracle lock, exit 4) must not be confused with the harness state lock is correct and important.

## Confirmed problems (17)

### P1 [high] `--progress json` is placed AFTER the compose subcommand in every §1.5 command sequence (`up ... --progress json`, `down ... --progress json`). It is a root-only flag on this binary, not a persistent one.

**Why it breaks:** VERIFIED on this machine (Compose v2.40.3-desktop.1): `docker compose ps --progress json` -> `unknown flag: --progress` (exit 1); `docker compose --progress json ps` -> works, and even emits `{"error":true,"message":"no configuration file provided: not found"}`. `docker compose up --help` and `docker compose down --help` do not list --progress; `docker compose --help` does. So the design's primary `up` and `down` invocations fail at argument parsing on the very first `thesis up`. Phase 0 DoD (a) does not survive the first run, and the nonzero exit is mapped by §1.5 step 0 logic to exit 2 with 'Docker daemon unreachable': a wrong diagnosis for a flag placement bug.

**Fix:** Move --progress to the root position inside composeArgs(): `docker compose --progress json -p P -f F... up -d ...`. Add a preflight that actually parses the built argv (`docker compose --progress json -p P config -q`) so a future flag migration fails loudly at preflight rather than mid-run.

### P2 [high] The generated label overlay (§2.2) stamps per-invocation values into SERVICE labels: `io.prothesis.run`, `io.prothesis.owner_pid`, `io.prothesis.created_at`.

**Why it breaks:** The design states the premise itself: 'Labels participate in compose's config-hash'. Those three values change on every invocation, so the config-hash changes on every invocation, so `compose up` force-recreates every container every time. That contradicts the design's own very next sentence ('repeated `up` with the same overlay is a genuine no-op') and destroys the topology on any re-`up`. Downstream it is worse than cosmetic: §2.5 promises `docker restart` preserves the container ID, but any subsequent full `up` (Phase 2 recovery, Phase 5 replay setup) silently recreates all nodes, discarding container state and PIDs mid-corpus.

**Fix:** Overlay carries config-time identity only: `io.prothesis.node`, `io.prothesis.service`, `io.prothesis.role_hint`, `io.prothesis.project`, `io.prothesis.system`. Per-run facts (run id, owner pid, created_at) live in `.prothesis/state/harness.json` and the run artifact, never in the compose model. If a run-scoped label is genuinely needed for reaping, put it on the network or a volume (created once), not on services.

### P3 [high] §1.5 step 2 preflights every declared published port with `net.Listen("tcp","127.0.0.1:P")` before `compose up`, unconditionally.

**Why it breaks:** §5.5 states '`thesis up` is permanently `--keep-up`; leaving the topology running *is its job*.' So after a successful `thesis up`, the project's own containers hold 18081/18082/18083 through Docker Desktop's forwarder. The next `thesis up` on the same project, which should be an idempotent no-op, fails preflight with 'port in use', naming a port held by itself. `thesis up && thesis up`, `thesis up` after a partial failure, and any Phase 1 `run` reusing a warm topology are all broken. The design compounds it by mapping this to exit 5, so the user is told to fix YAML that is correct.

**Fix:** Scope the preflight: enumerate this project's own containers (`docker ps -q --filter label=io.prothesis.project=P`) and exclude ports they already publish. For remaining conflicts, identify the holder: `docker ps --filter publish=P` naming a container from a DIFFERENT prothesis project is exit 2 with that project name and a `thesis down --project X` remediation; a non-Docker holder is exit 2 with the netsh hint. Reserve exit 5 for ports that are unparseable or out of range in the config.

### P4 [high] §3.2's port scheme `"127.0.0.1:${THESIS_PORT_BASE:-18080}1:8080"` is claimed to yield 18081 and to let 'concurrent runs be offset'.

**Why it breaks:** Compose interpolation is pure string substitution with no arithmetic. `${THESIS_PORT_BASE:-18080}` expands to the literal `18080` and appending `1` gives `180801:8080`, not 18081, and above 65535, so `docker compose config` rejects it. Phase 0 DoD (a) fails at §1.5 step 1. Beyond the arithmetic error, the offsetting story it exists to support cannot be written in a compose file at all. Fixed ports with no working offset also forecloses Phase 4: `thesis search --budget 8h` wants many worlds, and parallel worlds on one machine are impossible when three host ports are hard-coded.

**Fix:** Allocate ports in Go, not YAML. Compute per-node host ports at `up` time (base plus node ordinal, or OS-assigned free ports), export them as distinct per-node env vars (`THESIS_PORT_KV1`, `KV2`, `KV3`, `AMB1`...), and have the fixture write `"127.0.0.1:${THESIS_PORT_KV1}:8080"`. Record the resolved host ports in `.prothesis/state/harness.json` and the run artifact, since they are now dynamic. Phase 4 parallelism comes free.

### P5 [high] §5.6 defines `--no-teardown` as skipping HEAL ('faults stay injected') while leaving the exit code and verdict emission unchanged.

**Why it breaks:** Two normative built-in oracles are defined relative to HEAL: `availability_after_heal` ('Health probes respond within convergence window') and `no_stuck_op` ('Zero pending ops exceeding SLO ceiling after HEAL'). If HEAL never runs, both evaluate against a system still under an active partition, violating I5 by construction ('Checking consistency during an active network partition generates false positives'). The design emits a verdict anyway, so `--no-teardown` produces confidently wrong FAILs, or, if the residual fault masks the bug, a false PASS, which is precisely the anti-gaming failure addendum §E warns about. The flag's own name says teardown, not heal.

**Fix:** `--no-teardown` skips only TEARDOWN's destroy half. HEAL always runs and §4.3's residual-fault assertion always runs. If the operator genuinely wants faults frozen for inspection, that is a separate, explicitly named flag (`--freeze-faults`) which forces the verdict to INCONCLUSIVE / exit 2 rather than emitting PASS or FAIL, and which stamps the state file so the §5.6 interlock fires on the next `up`.

### P6 [high] DoD (a)'s definition of 'clean' contradicts the Down algorithm on volumes. §8: 'after `down`, `docker ps -aq --filter label=io.prothesis.project` and the corresponding network/volume queries all return empty, verified *by the tool*, and a nonempty result is exit 2.' §5.3 step 4: 'Volumes, only if requested.'

**Why it breaks:** A plain `thesis down` (no --volumes) deliberately leaves named volumes, then `survivors()` finds them and returns `ErrResidual` -> exit 2. If the KV fixture declares any named volume for Raft log persistence (which a Raft fixture normally does, and which Phase 2's `proc.restart` semantics arguably require) Phase 0 DoD (a) fails 100% of the time with a false residual report. If the fixture is instead written with no named volumes to dodge this, `proc.restart` in Phase 2 silently becomes 'restart with amnesia' and the lease bug's reproduction changes character.

**Fix:** Make `survivors()` symmetric with the requested teardown: check volumes only when `o.Volumes` was set. Report retained named volumes as an informational line in DownReport, never as residual. Decide the fixture's persistence story now (it changes what proc.restart means) and record it in DECISIONS.md.

### P7 [medium] §5.3 step 2 runs `docker rm --force --volumes <ids>` unconditionally, regardless of `DownOptions.Volumes`.

**Why it breaks:** `docker rm -v` removes anonymous volumes attached to the container, and `docker compose down` without `-v` deliberately preserves both named and anonymous volumes (verified in `docker compose down --help`: `-v, --volumes  Remove named volumes ... and anonymous volumes attached to containers`). So the reaper, which the design calls 'the real guarantee' and which always runs, destroys data the command the user typed promised to keep, with no way to opt out because the flag that would control it is ignored on this path.

**Fix:** Gate the flag: `rm --force` by default, `rm --force --volumes` only when `o.Volumes` is set.

### P8 [medium] §4.4's egress-per-node ambassador topology cannot express directed policy on the return leg of a peer-initiated TCP connection, yet §4.5 declares `Edge{From,To}` 'DIRECTED. n1<->n2 is two edges.'

**Why it breaks:** n1's AppendEntries to n2 flows n1 -> amb1:19002 -> kv2:9090 on one TCP connection; n2's RESPONSE returns on that same connection, i.e. through amb1, not amb2. The policy for edge n2->n1 installed at amb2 therefore governs only RPCs n2 itself initiates: it does not touch n2's replies to n1. An asymmetric partition (n2->n1 blackholed, n1->n2 open) half-works: n1 keeps receiving n2's AppendEntries responses and never observes the one-way failure. Asymmetric partitions are among the highest-yield Raft bug generators, and the promised primitive silently degrades into something else. The design never says which io.Copy direction carries which edge's policy.

**Fix:** Fix it in the type system: each ambassador listener applies `policy[From->To]` to the client->server copy and `policy[To->From]` to the server->client copy, so an edge policy is installed at BOTH ambassadors that carry it. `NetworkProvider.Get(e)` must then aggregate across both installation points and `ResetAll` must clear both: otherwise §4.3's Critical Guarantee ('assert zero residual') passes while half a policy is still live.

### P9 [medium] Only the peer port (9090) traverses an ambassador; the client port (8080) is published straight to the Windows host, so §4.5's `NetworkProvider` has no path to shape client traffic at all.

**Why it breaks:** The frozen fault grammar's TARGET is a node, not only an edge: `net.latency(n1)`, `net.bandwidth(n1, bps)`, `net.loss(n1, pct)` are all legal per §4.3 and all appear in `perturber.allow`. In this topology they can only affect n1's peer traffic, so `net.latency(n1)` reports as injected while the driver's ops to n1 are wholly unaffected. Concretely for Phase 1/2 oracles: `no_stuck_op` and `availability_after_heal` observe the client path, and no `net.*` fault can ever move them. The design argues this is 'the correct semantic' for partitions and it is, but it silently narrows the whole net.* family for every other target form, which is the fault-space narrowing addendum §E.2 treats as a lock-level event.

**Fix:** Either (a) put the published client port behind a per-node ingress ambassador too (`127.0.0.1:18081 -> amb1:18081 -> kv1:8080`) so client-plane shaping exists, keeping the health probe on a separate unshaped admin port so BOOT stays outside the fault domain; or (b) declare in DECISIONS.md that `net.*` against a node target means peer-plane only, have `Capabilities()` say so, and have the perturber refuse client-plane semantics rather than injecting a no-op. Do not leave it implicit.

### P10 [medium] §2.7 defines `ResolvedTarget` (and §4.5 `EdgePolicy` with its `Seed`) inside `internal/harness`, while §8 states these are serialized into the `.thesis` world file.

**Why it breaks:** Directive §3 assigns `pkg/schema` the job of holding 'Go structs for all normative schemas', and `.thesis` is normative under I2. Putting a world-serialized type in an internal package inverts §6.3's own stated dependency direction: `recorder` (the `.thesis` serializer) must import `internal/harness`, and so must `internal/shrink`. Phase 5 is where it bites hardest: ddmin drops faults from the list, and there is no stated rule for what happens to the recorded `ResolvedTarget`/`Observation` of a dropped or re-timed fault. The shrunk world would carry stale observations claiming `term=4` from a run that no longer exists, and the confirmation gate's `reproduced: "3/3"` would be computed against a world whose recorded content is partly fiction.

**Fix:** Move `ResolvedTarget`, `EdgePolicy`, `Edge` and `NodeID` into `pkg/schema`; `internal/harness` consumes them. Define now, in DECISIONS.md, what shrink does to a recorded target: recorded targets are replay inputs and are rewritten wholesale when a fault is dropped, never partially edited.

### P11 [medium] §3.3: 'Poll interval jitter is drawn from RECORDER, not `math/rand`, so BOOT timing is part of the seed': with no statement that BOOT draws from an independent stream.

**Why it breaks:** The NUMBER of BOOT poll rounds is a function of how long containers take to come up on that machine that day; it is not seeded and cannot be. If BOOT jitter draws from the same PRNG stream that later supplies fault timings, workload op order, or edge loss decisions, a replay that boots in 4 polls instead of 7 consumes a different prefix and every downstream draw shifts. Replay silently becomes a different world while claiming to be the same one: exactly the determinism claim addendum §C forbids ('never claim determinism it does not have'). The observable symptom is a `reproduced: "1/5"` that the team debugs as fixture flakiness for a week.

**Fix:** Give BOOT/probe jitter its own named independent recorder stream (`RECORDER.Stream("harness.probe")`), as §4.2 already does for edges with `Stream(edgeID)`. State explicitly that no phase may draw a variable number of times from a shared stream. Better still for BOOT: drop jitter entirely and use a fixed poll interval; BOOT jitter buys nothing and its variable draw count is pure risk.

### P12 [medium] §5.1's `ProjectName` returns `sanitize(cfgName) + "-" + hex`, but §5.4's global reaper filters `docker compose ls --all --format json` 'to `thesis-*`'.

**Why it breaks:** `cfgName` is `name:` from prothesis.yaml; for the fixture that is something like `kvfixture`, giving project `kvfixture-a41f0c2e`. Nothing forces a `thesis-` prefix; the `thesis-kvfixture-a41f0c2e` used throughout §2.2 and §5.1 only holds if the user happens to name their service `thesis-kvfixture`. So `thesis down --all` (the crash-recovery net of last resort, the one that runs precisely when the state directory is gone) matches nothing. Orphans accumulate invisibly and §5.4's claim that crash recovery is 'a *guarantee* rather than a hope' is false for any normally-named project.

**Fix:** Drop the prefix filter and reap on `--filter label=io.prothesis.project` (which the design already stamps and which is the real authority), falling back to `docker ps -a --filter label=io.prothesis.project` for projects compose no longer tracks. If a readable prefix is wanted, make `ProjectName` emit it unconditionally: `"thesis-" + sanitize(cfgName) + "-" + hex`.

### P13 [medium] §3.4 gives the helpers service `entrypoint: ["sleep","infinity"]` and requires it to stay running or `--wait` fails.

**Why it breaks:** `sleep infinity` is a GNU coreutils extension. BusyBox `sleep` (i.e. alpine, the obvious base for a scripts-only helper container) parses its argument as a number and errors on `infinity`. The container exits immediately, `docker compose up --wait` fails because a service never reaches running, `thesis up` returns nonzero, and Phase 0 DoD (a) fails with an error attributed to the harness. The design also never names a base image or Dockerfile for `helpers`, so the choice that decides this is simply unmade.

**Fix:** Use `["tail","-f","/dev/null"]` (works under busybox and coreutils) or pin a coreutils base explicitly. While fixing it, name the base image and commit the Dockerfile: the helper image must also carry whatever the §3.1 exec-path probe needs.

### P14 [medium] The design never accounts for building the three images it requires (kv, thesis-proxy ambassador, helpers), and §1.5's `up` uses `--pull missing` with no build handling or build-time budget.

**Why it breaks:** Phase 0 DoD (a) is a cold-machine boot: Go is not installed, the Linux VM has no image cache, and `thesis-proxy` must be a linux/amd64 binary produced from our tree. `compose up` does build services declaring `build:`, but the multi-minute first build lands inside `--wait-timeout {{S}}`, so a timeout sized for boot (tens of seconds) turns a normal first run into a spurious exit 2. Separately, shipping `thesis-proxy` as its own executable contradicts the directive's stated target: 'Single, self-contained Go static binary; alias `prothesis`'.

**Fix:** Make the proxy a hidden subcommand of the one binary (`thesis proxy --listen ... --upstream ...`) and have the ambassador image run that same static binary; one artifact, one `GOOS=linux GOARCH=amd64` cross-compile, directive satisfied. Split `up` into an explicit build step (`docker compose build`, its own generous timeout, its own exit-2 message) followed by `up --no-build --wait`, so a slow build is never mistaken for a slow boot.

### P15 [medium] §5.4's state-lock liveness rule: 'the lock is stale if `created_at` exceeds the TTL *or* no container carries `io.prothesis.run=<run_id>`'; races against §1.5's own ordering, and the lock's lifetime across the up/down process split is undefined.

**Why it breaks:** §1.5 takes the lock and writes state at step 3, but the first container does not exist until step 5. Any concurrent `thesis up` evaluating the lock inside that window sees zero containers with that run id, concludes the lock is stale, breaks it, and both processes proceed to `compose up` on the same project: the exact interleaved-mutation scenario the lock exists to prevent. Separately, `thesis up` is a short-lived process that exits leaving the topology running, and the design never says whether the lock file survives that exit: if it does, `thesis down` must break its own lock; if it does not, nothing guards the long-lived topology.

**Fix:** Two-phase the lock: `pending` from acquisition until the first container is observed (pending locks go stale only on a short TTL, e.g. 120s, never on the container test), then `active` (container test applies). State explicitly that `up` releases the lock on exit and that the running topology is protected by labels plus the state file: the lock guards concurrent mutators, not the world.

### P16 [medium] §1.4's version floor ('Refuse below Compose v2.21') does not cover the features actually used, and the version string this build reports will not parse as strict semver.

**Why it breaks:** VERIFIED: `docker compose version --format json` -> `{"version":"v2.40.3-desktop.1"}`. A strict semver parse either fails on the leading `v` or treats `-desktop.1` as a prerelease, which under semver ordering sorts BELOW `2.40.3`; a naive `>= 2.21.0` comparison can therefore reject a perfectly healthy install with exit 2 at preflight. Separately the design relies on `--progress json`, `compose version --format json` and `config --format json`, none of which are established as present in v2.21, so the floor sits below the feature set and would admit a Compose that fails mid-run instead of at preflight.

**Fix:** Strip the leading `v` and everything from the first `-`, then compare numerically. Raise the floor to the version that actually carries every flag used, and verify by running `--help` for each flag at preflight rather than trusting a number (help works with the daemon stopped, as demonstrated here). Record the verbatim version string in env.json regardless: the design is right that a verdict without it is not reproducible.

### P17 [medium] §3.1's non-published probe branch: '{host} = the compose network alias, {port} = the CONTAINER port -> probe runs via `docker exec`'; is unspecified in every detail that matters.

**Why it breaks:** It does not say which container the exec runs in, what HTTP client exists inside that image, or how `ProbeResult.Status`/`Latency` are recovered from a shelled-out curl. Worse, §3.1's own stated consequence is that every probed node MUST publish a host port, so this branch is dead code on the fixture, will be written once and never exercised, and will first be relied upon on a third-party system where it does not work. §3.3's carefully specified HTTP client (shared Transport, 1s dial, 2s per attempt) applies only to the published branch; the exec branch has no timeout story, and a hung `docker exec` with no per-attempt deadline wedges the BOOT gate: the exact failure §3.3 says the shared client exists to prevent.

**Fix:** Either implement it concretely in Phase 0 (name the container (helpers), bake a static prober into the same `thesis` binary and exec `thesis probe --url ... --json`, sidestepping curl/wget availability entirely, wrapped in `exec.CommandContext` with the same per-attempt deadline) or return a typed `ErrUnsupported` for unpublished probe ports with the remediation 'publish this port', and mark it a Phase 1 deliverable. Do not ship a half-specified fallback on the BOOT gate.

## Missing

- `thesis init`: a named Phase 0 CLI deliverable. The design mentions it exactly once, in passing, as the thing that commits `.gitattributes`. It never says what harness contributes to scaffolding: the generated `prothesis.yaml` must list all SIX builtin oracles per addendum §B (the directive's own §4.2 sample omits `no_stuck_op` while §6 Phase 1 requires all six), and must scaffold `harness.backend/file/nodes/health/steady_state` consistently with the fixture topology this design mandates.
- Dockerfiles and base images for the two containers the design invents (`helpers`, `amb1..3`), plus the cross-compile step producing the linux/amd64 proxy binary. Go is not installed on this host yet; the build path is the first thing exercised and is entirely unwritten.
- How `.thesis` actually serializes. §8 asserts constraints on the recorder (sorted slices, no maps, int64 ns, SetEscapeHTML(false)) but Phase 0 DoD (b) is a joint deliverable and nothing here names the format, the encoder, or the round-trip test. Note the 'no maps' rule is over-cautious (`encoding/json` has sorted map keys since Go 1.12) while the real byte-identity risks (trailing newline from Encoder vs Marshal, `omitempty` on zero-valued ints, float formatting in EdgePolicy's pct fields) go unaddressed.
- Any test plan. Neither DoD has a named test: no up/down/up/down idempotency test, no orphan-recovery test (delete `.prothesis/` then `down`), no byte-identical round-trip test, and (despite §6.5 itself flagging the composePS field names as unverified) no test exercising the `docker inspect` fallback path.
- The `process` backend. Directive §1.6 names `compose` AND `process` as the v1 backends. The design's `Backend` interface accommodates it and §6.2's table has a row for it, but no phase is ever assigned to building it, so it will not exist when Phase 2 needs the Windows capability matrix that table describes.
- Reconciliation with the stated environment fact that a Windows `process` backend 'cannot do proc.pause or net.partition at all'. §6.2's table claims `process/windows` gets `NetPolicy ✅ ambassadors`. That is arguably the better answer (user-space proxies do work on Windows) but the design asserts it against a briefed constraint without flagging the divergence, which is what OPEN_QUESTIONS.md exists for.
- Global flag plumbing: `--json`, `--quiet`, `--artifacts-dir`, `--config` (addendum §H) against `up`/`down`. The design specifies rich `DownReport`/`Topology` structures but never says what `thesis down --json` prints, which is the shape a Phase 6 agent will parse.
- What harness does with `perturber.constraints` (§4.2: 'never partition more than minority of kv', 'pg must be reachable during SEED'). Target resolution lives in `internal/harness/target.go` per §6.3, so the minority/majority selectors are harness's job, but the constraint strings that gate those selectors (and that are a lock-manifest input per addendum §E.2) are never mentioned.

