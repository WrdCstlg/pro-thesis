# 03-harness.design

## Summary

`internal/harness` uses the Docker **CLI** (never the Go SDK) as its only engine interface (`docker compose` for project lifecycle and `docker` for per-container ops) because the SDK cannot resolve Docker contexts (verified: DOCKER_HOST is unset and currentContext is `desktop-linux`, so the SDK would silently dial the wrong named pipe) and Compose v2 has no stable Go API. Logical nodes bind to containers through a **generated compose overlay that stamps `io.prothesis.node=<id>` labels**, making node→container lookup exact, restart-survivable, and independent of compose ordering; bindings are explicitly ephemeral and every consumer targets a node ID, with a `RoleResolver` seam so `role:leader` resolves dynamically in Phase 2 and its resolution is recorded into the world. Health probes run **host-side against published `127.0.0.1` ports** (the Windows host cannot route to the compose bridge network), which forces the fixture to publish one client port per node and therefore one compose service per node; `steady.sh` is executed **inside a helper container** via `docker compose exec`, which sidesteps the broken Git Bash entirely and is more correct anyway because steady state needs in-network vantage. Network faults commit **now** to per-node egress ambassador proxies rather than tc/netem (decisively because netem's randomness is in-kernel and unseedable, which would break invariant I2 ("every failure is a seed")) so the fixture topology routes all Raft peer traffic through proxies from Phase 0, at the cost of a ~60-line passthrough forwarder that needs no control plane until Phase 2.

## Decisions (14)

### D1: Docker CLI, not the Docker Go SDK

**Choice:** internal/harness drives Docker exclusively through the `docker` and `docker compose` CLIs, invoked as argv slices with `--format json`. No `github.com/docker/docker/client`, no `github.com/docker/compose/v2`. Phase 0 has zero third-party deps beyond the YAML parser pkg/schema already needs.

**Rationale:** Decisive: the Go SDK does not implement Docker contexts. Verified on this machine, DOCKER_HOST is unset and ~/.docker/config.json sets currentContext=desktop-linux (npipe:////./pipe/dockerDesktopLinuxEngine), while the SDK's client.FromEnv defaults to npipe:////./pipe/docker_engine. Those can be different engines (Linux vs Windows containers), so an SDK build could inspect a different daemon than Compose deployed to, with a 'container not found' symptom and no clue. Replicating context resolution means copying Docker's own source into our tree. Secondary: Compose v2 has no stable Go API and pulls buildkit/containerd (hundreds of modules), contradicting 'minimize third-party dependencies'; the engine SDK adds ~40 modules including go-winio for npipe. No capability is lost: inspect/exec/kill/pause/events/stats are all CLI-reachable with structured output. And every harness action becomes a command a human can paste into PowerShell, which for a debugging tool is a correctness property, not a convenience.

**Rejected:** (a) Pure SDK; context bug plus dependency weight. (b) Import compose/v2 as a library: explicitly unstable API, enormous transitive closure. (c) SDK for containers + CLI for compose: still carries the context-resolution bug and the version matrix, for no benefit over CLI-only. Cost accepted: the CLI is an unversioned contract, contained by never parsing human output, shape-sniffing compose ps JSON, falling back to `docker inspect`, a v2.21 version floor, and recording tool versions into env.json.

### D2: Deterministic, path-scoped compose project name

**Choice:** project = sanitize(config.name) + "-" + hex(sha256(normalized abs path of prothesis.yaml)[:4]), e.g. thesis-kvfixture-a41f0c2e. Normalization: EvalSymlinks, Clean, ToSlash, lowercase drive letter, lowercase whole path.

**Rationale:** Two required properties. Derivable from config alone, so `down` can destroy a crashed run's containers with the state directory deleted. Path-scoped, so two checkouts of the same repo get separate projects and cannot destroy each other's containers. Path normalization is mandatory on Windows: C:\x, c:/x and C:\X\..\x are the same directory and must hash identically, or the same tree gets two project names and orphans accumulate invisibly.

**Rejected:** Random per-run project names (unreapable after a crash; the exact failure mode we must prevent). Bare config name (two checkouts collide and cross-delete). Compose's default (basename of the project directory, not path-scoped, and silently changes if the directory is renamed).

### D3: Generated compose overlay stamps io.prothesis.node labels for node binding

**Choice:** `up` renders .prothesis/state/<project>.overlay.yaml adding io.prothesis.{node,service,role_hint,project,run,owner_pid,created_at,system} labels per service, appended as the final `-f`. Discovery is `docker ps -a --filter label=io.prothesis.node=n1`. Every subsequent compose invocation for that project must include the overlay.

**Rationale:** Makes the logical-node -> container mapping exact and ordering-free rather than inferred from compose's container-number, which compose does not contractually guarantee across recreate. It survives a lost state file, and it also catches containers we start outside compose in Phase 2 (ad-hoc ambassadors, one-off exec sidecars) which compose's own project label would miss. Fixture authors write nothing; compose merges multiple -f natively.

**Rejected:** container_name: in compose (blocks scaling entirely and collides across concurrent runs). Relying on com.docker.compose.container-number ordering alone (unstable across recreate). Requiring fixture authors to hand-write labels (works only for cooperative fixtures, and a missing label is a silent mis-binding).

### D4: Ambassador proxies over tc/netem for all network faults

**Choice:** Per-node egress ambassador containers (one proxy container per node, one listener per peer) carry all peer traffic. thesis-proxy is our own small Go TCP proxy with a per-edge policy control plane. The tc/netem provider stays as a documented, explicitly non-replayable fallback behind the same NetworkProvider interface, unimplemented in Phase 2. Committed now because it fixes the fixture's compose topology.

**Rationale:** Decisive: netem's randomness is in-kernel and unseedable. Invariant I2 requires all orchestration-level nondeterminism come from RECORDER; a world containing net.loss(30) via netem is not reproducible and the verdict's reproduced:"3/3" would be a claim we cannot support. Supporting: netem is egress-only and per-interface, so directed edges need tc filter classification rewritten on every container IP change, and ingress needs the ifb module in the Docker Desktop LinuxKit kernel which we cannot modprobe or verify. tc rules live in the target's netns, so a dead sidecar leaves invisible residual faults that poison every later world; exactly what the spec's 'HEAL must verify zero residual' guards against; a proxy's withdraw is one control call and is verifiable via GET /policy, and a dead proxy fails loudly as a down edge. Proxies also need no NET_ADMIN, give a path to msg.corrupt, and are the only option with an analogue in the process, k8s and sim backends.

**Rejected:** tc/netem+iptables in a NET_ADMIN sidecar; better statistical fidelity and works on unmodified systems, but unseedable (fatal), fragile withdraw, kernel-module dependent, no message-level future, no cross-backend analogue. One proxy container per directed edge: O(n^2) containers; solved instead by one egress proxy per node with per-peer listeners, O(n) containers and O(n^2) policies.

### D5: net.partition blackholes; it must never RST

**Choice:** ModeBlackhole is the default for net.partition: hold established connections open, accept and discard bytes, never send FIN or RST, accept new connections and never respond. ModeReset (immediate RST) exists as a separate, explicitly named mode.

**Rationale:** A proxy that closes the socket hands the application an immediate clean error; a real partition drops packets and lets TCP retransmit for minutes. Those are different bug classes, and the easy one is the one you get by accident. The spec calls proc.pause/gray failure ('unresponsive to peers, alive to the orchestrator') the top priority, and the fixture's stale-read lease bug requires the former leader to be unreachable-but-not-erroring. If partition is implemented as conn.Close(), Phase 2's Definition of Done will not reproduce and the cause will be extremely hard to find.

**Rejected:** Close on partition (simplest, and silently wrong). Refusing new connections with RST while blackholing existing ones (a hybrid no real network produces).

### D6: Health probes run host-side against published 127.0.0.1 ports; never `localhost`

**Choice:** {host}/{port} substitution is decided per node by publication: published -> {host}=127.0.0.1, {port}=host port, probe runs on the Windows host; unpublished -> {host}=compose network alias, {port}=container port, probe runs via docker exec. The resolved URL is recorded. Unknown placeholders are CONFIG_ERROR. Probes issue a full HTTP GET requiring 2xx.

**Rationale:** The Windows host has no route into the Docker Desktop VM's bridge networks, so container IPs are unreachable from the thesis process; publication is the only host-side option. Deriving the mode from publication needs no new schema field (the schema is frozen) and is observable from compose config/ps. 127.0.0.1 rather than localhost because localhost resolves ::1 first on Windows and Docker Desktop's forwarder binds v4/v6 inconsistently, producing intermittent unexplainable connection-refused. Full HTTP rather than TCP connect because Docker Desktop's forwarder accepts the connection before the container is listening, so a connect-only probe reports a dead container healthy.

**Rejected:** Container IPs from the host (unroutable). Depending on WSL-adapter routing folklore (unreliable, differs under WSL mirrored networking, absent under Hyper-V and in CI). Adding a probe-mode field to prothesis.yaml (schema is frozen; logged as OPEN_QUESTIONS #3 for v1.1).

### D7: steady.sh executes inside a helper container, not on the host

**Choice:** A `helpers` service (io.prothesis.system=helper, entrypoint sleep infinity, scripts baked in via COPY) runs steady_state as `docker compose exec -T --workdir /thesis-helpers helpers /bin/sh /thesis-helpers/steady.sh`. Host execution is a documented Tier-2 fallback ($THESIS_SHELL -> busybox.exe sh -> wsl.exe -> pwsh -File -> native .exe), and cmd.exe is never used. No resolvable path -> exit 5 at preflight.

**Rationale:** Verified: Git Bash on this machine cannot fork, Windows has no /bin/sh, and Go's exec has no shebang handling, so host execution of a .sh is impossible here. But container execution is the better design regardless of platform: steady state for a Raft cluster means all peers agree on term and commit index, which requires the peer network, and after Phase 2 introduces partitions a host-side check would be measuring the wrong network. It also makes results platform-identical, which invariant I2 requires; a world is not reproducible if the steady-state predicate depends on the operator's OS. cmd.exe is excluded deliberately: it would run a .sh as a batch file and might exit 0, producing a false pass on a gate.

**Rejected:** Rewriting steady.sh as .ps1 (spec names steady.sh; and it would give the wrong network vantage). Requiring WSL (verified present here but an environment assumption absent under Hyper-V and in CI). Bundling busybox.exe (host vantage still wrong). Skipping steady_state on Windows (weakens the gate to make it pass: forbidden).

### D8: One compose service per logical node in the fixture

**Choice:** kv1/kv2/kv3 as separate services, not `kv` scaled to 3. The binding rule for scaled services (nodes in declaration order -> containers by container-number ascending, count mismatch = exit 5) exists but is never exercised by the fixture.

**Rationale:** Four independent requirements converge. (1) D6 needs a distinct published host port per node; scaled services cannot statically publish distinct ports and port ranges bind nondeterministically, destroying the node<->endpoint mapping. (2) D4 needs each node to have its own peer list pointing at its own ambassador; scaled replicas share one environment block. (3) The ambiguous container-number ordering rule never has to be trusted. (4) `compose up --force-recreate --no-deps kv2` restarts exactly one node for proc.restart; under scaling there is no way to name replica 2. Cost is three near-identical YAML blocks, collapsible with anchors.

**Rejected:** `kv` scaled to 3 (breaks all four). container_name (blocks scaling and collides across runs).

### D9: Client port published, peer port unpublished and reachable only via ambassadors

**Choice:** 8080 (client HTTP) published to 127.0.0.1:1808x; 9090 (Raft peer) never published and addressed only through ambassador listeners. Phase 2 adds a self-test: inject a full partition and assert loss of quorum within an election timeout, else exit 2.

**Rationale:** Puts the health-probe and load-generator path outside the peer fault domain, so net.partition between replicas does not blind the BOOT gate or availability_after_heal, and keeps the stale read observable through the partition, which is the whole point of the fixture. The self-test exists because compose DNS still resolves kv2 on the shared network, so nothing structurally prevents the fixture from dialing a peer directly; if it did, every network fault would be silently ineffective and Phase 2's DoD would fail looking like a Raft bug. A chaos tool whose faults do nothing and reports green is worse than no tool.

**Rejected:** Publishing the peer port too (host-side traffic would bypass ambassadors and silently defeat partitions). Publishing nothing and probing only via exec (loses the outside-the-fault-domain vantage that availability_after_heal needs).

### D10: down = compose down + label reap + verify; state file removed last

**Choice:** compose down --remove-orphans (missing project = success), then force-remove by both com.docker.compose.project and io.prothesis.project labels, then networks, then volumes if requested, then re-list to verify. Clean sweep -> delete state file, exit 0. Residual -> exit 2 with the exact manual docker commands.

**Rationale:** Labels, not signal handlers, are the actual orphan guarantee; handlers do not survive SIGKILL, Task Manager, or a closed console window, and on Windows SIGTERM is never delivered at all. Reaping by both label families catches containers started outside compose in Phase 2. Every step treats absence as success, so idempotency falls out and a second down is a no-op. Removing the state file only after verification keeps a partial teardown discoverable by the next invocation instead of stranding orphans with no record.

**Rejected:** compose down alone (leaves non-compose containers and silently succeeds when the project label is stale). Deleting the state file first (a failed reap becomes undiscoverable). Reporting success on residual containers (would let a poisoned environment silently corrupt the next world).

### D11: --no-teardown and --keep-up are different, with a residual-fault interlock

**Choice:** --no-teardown (global): collect artifacts, skip HEAL, leave faults injected, leave containers, state leaked:true. --keep-up (replay/up): collect artifacts, run HEAL and withdraw faults, leave containers, state retained:true. Both always run TEARDOWN's collect half. Interlock: the next up/run on a project whose state records residual_faults refuses to start (exit 2) unless --force-recreate.

**Rationale:** The two flags express different intents (inspect the broken state versus leave a clean cluster to poke at) and conflating them silently corrupts later runs. The interlock matters most: starting a fresh world on top of an unrecorded residual partition produces a world whose recorded fault schedule does not describe what actually happened. That is a false failure at best and a false pass at worst, if the residual fault masks the bug. This is the anti-gaming rule ('never make the gate weaker to make it pass') applied to the harness. Artifacts are always collected because a verdict without artifacts is useless and skipping collection is never what anyone means.

**Rejected:** Treating them as aliases (loses the HEAL distinction and the interlock). Skipping artifact collection under --no-teardown (destroys the reason to use it).

### D12: Capability negotiation: a gap intersecting perturber.allow is a hard error

**Choice:** Backend.Capabilities() returns a Caps bitset; unsupported operations return typed ErrUnsupported{Backend, Op, Reason}. A capability gap that intersects perturber.allow fails at PLAN time with exit 2 INCONCLUSIVE, never a silent skip at inject time. Explicit waivers are recorded in the verdict and covered by the oracle lock.

**Rationale:** This is how 'process backend on Windows cannot proc.pause or net.partition' is handled without lying in a verdict. Silently dropping proc.pause and reporting PASS is operationally indistinguishable from a fault-space-floor violation (addendum E.2): the run genuinely explored a smaller fault space than the config declares, and nothing in the output says so. Failing at plan time rather than inject time also means the operator learns in the first second rather than after a partial world.

**Rejected:** Panicking on unsupported ops (crashes a long soak run on the first unsupported fault). Silently skipping (produces a passing verdict that is a lie). Auto-narrowing perturber.allow to fit the backend (a fault-space-floor violation performed by the tool itself).

### D13: proc.pause uses docker pause (cgroup freezer), not SIGSTOP

**Choice:** Freeze/Thaw on the compose backend are `docker pause` / `docker unpause`. SIGSTOP remains the implementation for the process backend on Linux.

**Rationale:** The cgroup freezer suspends every process and thread in the container atomically; SIGSTOP delivered to PID 1 stops only PID 1, leaving child processes and, in a multi-process container, most of the workload running. It is also uncatchable and unmaskable, and unfreezing is exact. The spec names 'SIGSTOP/SIGCONT' as the mechanism, but the required semantic is 'unresponsive to peers, alive to the orchestrator', and the freezer delivers that semantic more completely. Paused state is visible as State=paused in inspect, so HEAL's residual check is a simple state assertion.

**Rejected:** docker kill --signal=STOP (stops only PID 1; leaves helper threads and child processes running, producing a weaker gray failure that may not trigger the lease bug). docker stop (a clean shutdown, an entirely different fault).

### D14: Image reproducibility: --pull missing for up, --pull never for run/gate, digests recorded

**Choice:** `thesis up` uses --pull missing --quiet-pull. `thesis run`/`gate`/`replay` use --pull never and fail loudly if an image is absent. Resolved image digests are recorded in .prothesis/runs/<id>/env.json alongside docker and compose versions.

**Rationale:** Invariant I2 requires a world to reproduce. If a registry round-trip mid-corpus silently swaps the image under a :latest-style tag, worlds recorded before and after are not comparable and a shrink can chase a phantom. --pull never makes the image an input that cannot change during a run; recording digests makes a cross-run comparison auditable. up keeps --pull missing so the first-ever run is not a puzzle.

**Rejected:** --pull always everywhere (nondeterministic and slow). --pull missing everywhere (a pulled image mid-corpus silently invalidates the corpus). Pinning by digest in the compose file (correct but shifts the burden onto every fixture author; recording achieves the audit without the burden).

## Open questions (7)

### [RESOLVABLE-WITH-DECISION] OQ1: steady_state probe path has no declared execution location

prothesis.yaml §4.2 specifies `steady_state: { probe: "thesis-helpers/steady.sh" }` (a host-relative path to a POSIX shell script) with no field declaring where it runs. On the target environment there is no host execution path at all: Windows has no /bin/sh, Git Bash on this machine cannot fork (verified: dofork exit code 0xC0000142), and Go's exec has no shebang handling. Taken literally the requirement is unimplementable here. Resolving it by executing inside a helper container reinterprets the path as container-relative (/thesis-helpers/steady.sh), which is an interpretation of a normative field rather than a reading of it.

**Recommendation:** Adopt D7: execute in a helper container, treating the configured path as relative to /thesis-helpers inside that container. This is not merely a Windows workaround: it is more correct on every platform, because steady state for a replicated system requires peer-network vantage, and a host-side check would be measuring the wrong network once Phase 2 partitions exist. It also makes the predicate platform-identical, which I2 requires. For v1.1 propose an additive optional field `steady_state.exec: container|host` (default container) so the location is declared rather than inferred. Document the Tier-2 host fallback chain and its explicit exclusion of cmd.exe, which could run a .sh as a batch file and exit 0: a false pass on a gate.

### [ACCEPT-AND-DOCUMENT] OQ2: ambassador proxies contradict 'must work on unmodified systems'

CRUCIBLE PART 10 states PRO-THESIS must work on unmodified systems today, which rules out requiring instrumentation. Ambassador proxies require peers to address each other through proxy endpoints, i.e. a peer-address configuration change in the system under test. The alternative that satisfies PART 10 (tc/netem in a NET_ADMIN sidecar) violates invariant I2, because netem's randomness is in-kernel and unseedable, so a world containing net.loss is not reproducible and the verdict's reproduced field would claim determinism the tool does not have. Two normative requirements point in opposite directions for network faults specifically.

**Recommendation:** Accept the tension and scope it honestly rather than resolving it falsely. Ambassador proxies are the default and are the only network provider in Phase 2, because I2 (reproducibility) is a stated invariant while PART 10 is a positioning constraint, and a chaos tool that cannot replay its own findings has no product. Keep the NetworkProvider interface admitting both, and document tc as a Phase-2b provider for unmodified systems that is explicitly marked non-replayable: any world using it must set reproduced to a measured k/n and must never be promoted into the regression corpus. Note in DECISIONS that proc.*, clock.* and io.* faults remain fully instrumentation-free; only net.* carries this constraint, so the PART 10 claim holds for most of the fault matrix.

### [RESOLVABLE-WITH-DECISION] OQ3: health probe {host}/{port} has no execution-mode field in a frozen schema

harness.health[].probe is a URL template with {host} and {port}, but the schema declares no vantage point. On Docker Desktop for Windows the two candidate substitutions are not interchangeable: 127.0.0.1 plus a published host port works, and the container IP or compose DNS name on the bridge network is unroutable from the Windows host. The schema is frozen, so a `mode` field cannot be added, yet the correct substitution differs per node and per platform.

**Recommendation:** Derive the mode instead of declaring it (D6): if the node's probe port is published use 127.0.0.1 plus the host port and probe from the host; otherwise use the compose alias plus the container port and probe via docker exec. Publication is observable from `compose config --format json` and `compose ps --format json`, so the rule needs no new field, is deterministic, and is recorded per node into the run artifact. Reject unknown placeholders as CONFIG_ERROR rather than passing them through, so a literal {node} in a URL is not misattributed to the system under test. Propose an additive optional `health[].exec: host|container` for v1.1 to make the vantage explicit.

### [ACCEPT-AND-DOCUMENT] OQ4: no ordinal field to bind a logical node to a replica of a scaled service

harness.nodes entries are {id, service, role_hint}. Three nodes may name the same service. Compose offers no stable, contractual identifier for replica k of a scaled service: com.docker.compose.container-number exists but its stability across recreate and rescale is not guaranteed. The schema is frozen so no ordinal or replica field may be added, yet the fault grammar targets individual nodes and must not silently pause the wrong one.

**Recommendation:** Two layers. Primary: stamp io.prothesis.node labels via the generated overlay (D3), which makes binding exact and ordering-free for the one-service-per-node case. Secondary, for genuinely scaled services: nodes declared against a service bind in config declaration order to containers ordered by container-number ascending, with a count mismatch failing as exit 5 naming both numbers. Document the secondary rule as best-effort and steer fixtures to one service per node (D8), which four independent requirements already demand. Propose `nodes[].replica: <int>` as an additive optional field for v1.1 if scaled services become a real use case.

### [ACCEPT-AND-DOCUMENT] OQ5: backend capability gaps could silently shrink the declared fault space

The process backend on Windows cannot implement proc.pause (no SIGSTOP equivalent that composes with the Go runtime) and, without ambassadors, cannot implement net.partition. If the harness silently skips those faults at injection time, a run explores a smaller fault space than perturber.allow declares while still reporting PASS. That is operationally indistinguishable from the fault-space-floor violation that addendum E.2 makes a lock-guarded change, except that the tool performs it on itself and nothing in the verdict says so.

**Recommendation:** Make it loud and structural (D12). Backend.Capabilities() is compared against perturber.allow at PLAN time; any intersection gap is exit 2 INCONCLUSIVE with the exact list of unsupported kinds and why, never a skip at inject time. If an operator genuinely accepts a reduced space, require an explicit waiver that is recorded in the verdict and covered by the oracle lock, so it is a reviewable act rather than an invisible degradation. Also record the effective fault space in every world file, so a corpus built on a capability-limited backend is identifiable after the fact.

### [RESOLVABLE-WITH-DECISION] OQ6: two different things are called 'the lock file'

internal/lock in the fixed §3 layout is the oracle tamper-detection manifest (.prothesis/lock, invariant I6, exit code 4, human-reviewed bump via `thesis oracles lock --reason`). internal/harness independently needs a mutual-exclusion lock so two concurrent thesis processes do not fight over one compose project. Calling both 'the lock' in code, logs and docs will cause someone to wire harness contention into the ORACLE_DRIFT exit path, which would report a tampering violation for what is actually a second terminal window.

**Recommendation:** Name them apart everywhere and never let them share an exit code. The oracle lock keeps `lock` and exit 4. The harness one is `.prothesis/state/harness.lock`, implemented in internal/harness/statelock.go, referred to exclusively as the 'project state lock', and its contention is exit 2 INCONCLUSIVE. Do not use flock or PID liveness: os.FindProcess succeeds for any PID on Windows and PIDs recycle; instead treat a lock as stale when its created_at exceeds a TTL (default 4h) or when no container carries its io.prothesis.run label. Deriving liveness from Docker rather than the OS sidesteps the whole cross-platform problem.

### [ACCEPT-AND-DOCUMENT] OQ7: the Docker CLI's JSON output is an unversioned contract

D1 makes the CLI the sole engine interface, but `docker compose ps --format json` has changed output shape across Compose v2 minors (newline-delimited objects versus a single JSON array), and field names in that output are not covered by any stability guarantee. A Docker Desktop auto-update could change the shape underneath a working installation. The daemon is stopped on this machine, so the exact field set of this Compose build (v2.40.3-desktop.1) could not be verified during design.

**Recommendation:** Contain rather than avoid. Sniff the first non-whitespace byte to accept both array and NDJSON shapes, which removes the version question entirely at negligible cost. Ignore unknown fields (encoding/json default) so new fields never break us, and fall through to `docker ps --filter label=... --format '{{json .}}'` plus `docker inspect` (far older and far more stable surfaces) whenever a needed field is absent. Enforce a Compose v2.21 floor with exit 2 and a remediation string, record docker and compose versions in every run's env.json so a verdict is attributable to a toolchain, and add a `thesis doctor` check that parses live output and reports drift. Exercise the fallback path in a test rather than assuming it works, and confirm the composePS field names against a live daemon as the first implementation task.

## Risks

- Ambassador bypass is the highest-severity risk in the whole design. Compose DNS still resolves kv2 on the shared network, so a fixture that dials a peer directly makes every net.* fault silently ineffective while everything still looks healthy. Phase 2's Definition of Done would then fail in a way that looks like a Raft bug rather than a wiring bug. Mitigation is the D9 self-test: inject a full partition, assert loss of quorum within an election timeout, and exit 2 INCONCLUSIVE if the cluster does not notice. Build that self-test before trusting a single Phase 2 result.
- Implementing net.partition as conn.Close() instead of blackholing. A closed socket gives the application an immediate clean error; a real partition gives silence and TCP retransmits. The fixture's stale-read lease bug needs the paused/partitioned former leader to be unresponsive, not erroring, so a reset-based partition will simply fail to reproduce it, and the wasted debugging will be aimed at the Raft implementation. Encode ModeBlackhole as the default in the type system, not in a comment.
- Compose CLI JSON shape drift. `compose ps --format json` has emitted both a JSON array and newline-delimited objects across v2 minors, and the daemon was stopped during design so this build's exact field names are unverified. The first implementation task must be to run a live `docker compose ps --format json` and confirm the composePS struct, and the docker-inspect fallback must be exercised by a test rather than assumed to work.
- The overlay file must be passed to EVERY compose invocation for the project. Omit it from `ps` or `down` and compose computes a different config hash, believes the containers are stale, and either recreates them mid-run or fails to find them at teardown, leaving orphans. This is one shared composeArgs() helper and exactly one place to get wrong, so it will be gotten wrong at least once.
- Windows teardown truncation. CTRL_CLOSE_EVENT gives roughly five non-extendable seconds and Ctrl+C reaches the whole console process group including the docker.exe children the handler just spawned. If teardown is treated as the primary orphan defense rather than a convenience on top of label-based reaping, closing a terminal will strand three containers and two networks, and the next run will silently adopt or collide with them.
- Silent empty bind mounts on Windows. Mounting from a drive not enabled in Docker Desktop file sharing, or from a UNC path, produces an empty directory inside the container with no error at all. steady.sh then 'does not exist' and the failure is attributed to the fixture or the shell resolution logic. Baking helpers with COPY avoids it; any future convenience bind-mount reintroduces it.
- Port collisions across concurrent runs and stale processes. Fixed host ports make debugging sane but two thesis runs on one machine will fight, and a WinNAT-reserved range surfaces as 'permission denied' rather than 'address in use', sending the operator down the wrong path entirely. The preflight net.Listen check and an error message that names `netsh interface ipv4 show excludedportrange protocol=tcp` are not optional polish.
- Byte-identical .thesis round-trip is easy to break from the harness side. Any Go map, any RFC3339 timestamp, any unsorted slice that leaks from Topology or ResolvedTarget into the world file destroys Phase 0's Definition of Done non-deterministically: passing locally and failing in CI, or passing nine times in ten. Keep observed Bindings out of the world entirely and serialize ResolvedTarget through a canonical encoder with sorted node lists and int64 nanoseconds.
- Compose --wait gives false confidence. For services without their own healthcheck it waits only for 'running', which for a Raft node means the process started and nothing more. If anyone treats --wait as the BOOT gate and drops the PRO-THESIS health probes, DRIVE will begin against a cluster that has not elected a leader, and the resulting flaky failures will look like real bugs.
- Role resolution during a partition is exactly when it is least reliable and most needed. Zero leaders or two leaders in different terms is the interesting state, not an error, so treating ErrNoRole or ErrRoleAmbiguous as fatal would systematically discard the worlds most likely to contain bugs. Skipping the fault and recording an info event is correct, but it also means a world's recorded fault schedule can differ from its intended one, which must be visible in the verdict rather than buried.

## Files implied (32)

- `C:\AI Projects\Pro-synthesis\internal\harness\harness.go`: Backend interface, Caps bitset, NodeID/Node/Binding/Topology/Endpoint types, ErrUnsupported/ErrStaleBinding/ErrResidual, UpOptions/DownOptions/DownReport. _(0)_
- `C:\AI Projects\Pro-synthesis\internal\harness\docker.go`: Docker CLI wrapper: LookPath once and reject .bat/.cmd, argv-slice exec via CommandContext, stdout/stderr capture, per-command audit record to harness.jsonl. _(0)_
- `C:\AI Projects\Pro-synthesis\internal\harness\exec_windows.go`: CREATE_NEW_PROCESS_GROUP SysProcAttr so Ctrl+C to the console group does not kill teardown subprocesses mid-sweep. _(0)_
- `C:\AI Projects\Pro-synthesis\internal\harness\exec_unix.go`: No-op process-group twin for non-Windows builds (Setpgid), keeping the call site platform-free. _(0)_
- `C:\AI Projects\Pro-synthesis\internal\harness\project.go`: Deterministic path-scoped compose project name with Windows path normalization; io.prothesis.* label constants; atomic state-file read/write (.tmp, Sync, Rename). _(0)_
- `C:\AI Projects\Pro-synthesis\internal\harness\statelock.go`: Project state lock (O_CREATE|O_EXCL) with Docker-label-derived staleness instead of PID liveness; explicitly not the oracle lock. _(0)_
- `C:\AI Projects\Pro-synthesis\internal\harness\compose_backend.go`: composeBackend implementing Backend: Preflight, Up, Down, Topology, Resolve, Validate; Phase 2 methods present as typed ErrUnsupported stubs so the tree always compiles. _(0)_
- `C:\AI Projects\Pro-synthesis\internal\harness\compose_cli.go`: compose/docker argv builders and JSON decoders, including the array-vs-NDJSON sniffing decoder for `compose ps --format json` and the docker-inspect fallback path. _(0)_
- `C:\AI Projects\Pro-synthesis\internal\harness\compose_overlay.go`: Renders the generated label overlay YAML that stamps io.prothesis.node and friends, making node-to-container binding exact and ordering-free. _(0)_
- `C:\AI Projects\Pro-synthesis\internal\harness\compose_reap.go`: Label-based orphan reaper for containers, networks and volumes; survivors() verification; `thesis down --all` cross-project sweep. _(0)_
- `C:\AI Projects\Pro-synthesis\internal\harness\probe.go`: Health probe engine: {host}/{port} substitution by publication, all-green-in-one-round BOOT gate, full-HTTP (never TCP-connect) checks against 127.0.0.1. _(0)_
- `C:\AI Projects\Pro-synthesis\internal\harness\execplan.go`: ExecPlanner: container-exec (default) vs host interpreter resolution for steady.sh and later driver.cmd; CRLF and missing-interpreter detection; cmd.exe explicitly excluded. _(0)_
- `C:\AI Projects\Pro-synthesis\internal\harness\target.go`: Selector grammar shared by health probes and the fault grammar (n1, kv:*, role:leader, edges, minority/majority); RoleResolver interface with the static implementation only in Phase 0. _(0)_
- `C:\AI Projects\Pro-synthesis\internal\harness\net.go` (NetworkProvider interface, Edge, EdgePolicy, EdgeMode (Pass/Blackhole/Reset), NetCaps) the seam that lets proxy and tc providers coexist. _(0)_
- `C:\AI Projects\Pro-synthesis\internal\harness\net_proxy.go`: Ambassador NetworkProvider; Phase 0 ships discovery of ambassador containers only, with Apply/Get returning ErrUnsupported until Phase 2. _(0 stub, 2 full)_
- `C:\AI Projects\Pro-synthesis\internal\harness\net_tc.go`: tc/netem NetworkProvider stub documenting why it is not the default (unseedable randomness breaks I2) and reserving the path for unmodified third-party systems. _(2b stub)_
- `C:\AI Projects\Pro-synthesis\internal\harness\roles.go`: ProbeRoleResolver (/status, highest-term leader, ErrRoleAmbiguous) and LogRoleResolver for dynamic role:leader targeting. _(2)_
- `C:\AI Projects\Pro-synthesis\internal\harness\harness_test.go` (Unit tests for project-name normalization, overlay rendering, compose-ps dual-shape decoding, placeholder substitution and CRLF detection) all daemon-free. _(0)_
- `C:\AI Projects\Pro-synthesis\internal\harness\updown_test.go`: Integration test asserting up then down leaves zero labelled containers, networks and volumes, and that a second down is a clean no-op. _(0)_
- `C:\AI Projects\Pro-synthesis\cmd\thesis\main.go`: CLI entrypoint, global flags (--config, --json, --quiet, --artifacts-dir, --no-teardown), signal handling, and the single exit-code mapping site. _(0)_
- `C:\AI Projects\Pro-synthesis\cmd\thesis\cmd_up.go`: `thesis up`: preflight, port check, overlay render, compose up --wait, discovery, BOOT probes, state write. _(0)_
- `C:\AI Projects\Pro-synthesis\cmd\thesis\cmd_down.go`: `thesis down` with --project, --all, --volumes, --timeout; idempotent teardown plus verification. _(0)_
- `C:\AI Projects\Pro-synthesis\cmd\thesis\cmd_init.go`: `thesis init`: scaffolds prothesis.yaml with all six builtin oracles, .prothesis/ tree, .gitattributes with *.sh eol=lf, and thesis-helpers/steady.sh written LF. _(0)_
- `C:\AI Projects\Pro-synthesis\pkg\schema\config.go`: Go types for prothesis.yaml including harness.nodes, harness.health and harness.steady_state consumed by internal/harness. _(0)_
- `C:\AI Projects\Pro-synthesis\testdata\kvfixture\docker-compose.yaml`: Fixture topology: kv1/kv2/kv3 with published client ports, unpublished peer ports, amb1/amb2/amb3 egress ambassadors and a helpers service. _(0)_
- `C:\AI Projects\Pro-synthesis\testdata\kvfixture\Dockerfile`: Builds the buggy Raft KV node; multi-stage, static binary, no shell dependency at runtime. _(0)_
- `C:\AI Projects\Pro-synthesis\testdata\kvfixture\proxy\main.go`: thesis-proxy: Phase 0 pure TCP passthrough (listen, io.Copy both ways); Phase 2 adds the per-edge policy control plane with a RECORDER-seeded PRNG. _(0 skeleton, 2 full)_
- `C:\AI Projects\Pro-synthesis\testdata\kvfixture\helpers\Dockerfile`: Helper image with thesis-helpers/ baked in via COPY, avoiding the silently-empty Windows bind-mount failure mode. _(0)_
- `C:\AI Projects\Pro-synthesis\thesis-helpers\steady.sh`: Steady-state predicate for the KV cluster (all peers agree on term and commit index), LF-only, run inside the helper container. _(0)_
- `C:\AI Projects\Pro-synthesis\.gitattributes`: Forces *.sh to LF so helper scripts never acquire CRLF and fail as 'bad interpreter' inside Linux containers. _(0)_
- `C:\AI Projects\Pro-synthesis\DECISIONS.md`: Records D1-D14 from this design with rationale and rejected alternatives. _(0)_
- `C:\AI Projects\Pro-synthesis\OPEN_QUESTIONS.md`: Records OQ1-OQ7, each with conflict, classification and recommendation. _(0)_

## Design

# PRO-THESIS: `internal/harness` design

**Scope:** compose backend, `thesis up`, `thesis down`. Phase 0 skeleton, Phase 2-compatible.
**Environment:** verified on this machine, 2026-09-06. Findings that changed the design are marked ⚠ **VERIFIED**.

---

## 0. Environment findings that drove this design

| Finding | How verified | Design consequence |
|---|---|---|
| Git Bash forks fail (`dofork: child -1 … exit code 0xC0000142`) | ran `ls` via Bash tool, reproduced | `steady.sh` cannot run on the host. §3.4 |
| `DOCKER_HOST` unset; `~/.docker/config.json` has `currentContext: desktop-linux` → `npipe:////./pipe/dockerDesktopLinuxEngine` | `$env:DOCKER_HOST`, `cat config.json` | Docker Go SDK's `client.FromEnv` defaults to `npipe:////./pipe/docker_engine`: **a different pipe**. §1 |
| Docker CLI 29.1.3, Compose **v2.40.3-desktop.1** at `C:\Program Files\Docker\cli-plugins\docker-compose.exe` | `docker compose version`, `docker info --format {{json .ClientInfo.Plugins}}` | CLI path is viable; version floor can be set high. |
| Linux engine daemon **stopped**; `npipe` open fails | `docker info` | Preflight must map this to exit **2**, not a crash. |
| WSL2 backend, distros `Ubuntu-24.04`, `Ubuntu-22.04`, `docker-desktop` | `wsl.exe -l -v` | `wsl.exe` exists as a *fallback* shell, but must not be a *default* assumption. §3.4 |
| Compose supports `--progress json`, `--dry-run`, `ps --format json`, `compose ls`, `pause/unpause/kill/exec` | `docker compose --help` on this binary | Machine-readable everywhere; no table scraping. §1.4 |
| Reserved TCP ranges: 50000-50059, 62590-63523, 1462, 5357 | `netsh interface ipv4 show excludedportrange protocol=tcp` | Port base **18080** and control base **19100** are clear. §3.2 |
| Project path is `C:\AI Projects\Pro-synthesis`: **contains a space** | `Get-ChildItem` | Never build command *strings*; argv arrays only. §7 |

---

## 1. Docker CLI vs Docker SDK

### 1.1 Decision

**Docker CLI only. No `github.com/docker/docker/client`, no `github.com/docker/compose/v2`.**
`internal/harness` shells out to exactly two executables: `docker` and `docker compose` (the latter as a subcommand of the former). Phase 0 `go.mod` has **zero** third-party dependencies beyond a YAML parser (`gopkg.in/yaml.v3`) that `pkg/schema` needs anyway.

This is a *hybrid at the command level*: `docker compose …` owns the project (create/destroy/discover), `docker …` owns individual containers (inspect/exec/kill/pause/restart/logs/stats/events). That split is exactly the one the task anticipated, but it lives entirely in the CLI: the SDK is not needed to get it.

### 1.2 Why not the SDK: the decisive reason

The docker Go SDK **does not implement Docker contexts.** `client.FromEnv` reads only `DOCKER_HOST` / `DOCKER_CERT_PATH` / `DOCKER_TLS_VERIFY`. It never reads `~/.docker/config.json`'s `currentContext`, nor `~/.docker/contexts/meta/<sha256(name)>/meta.json`.

⚠ **VERIFIED on this machine:** `DOCKER_HOST` is unset and `currentContext` is `desktop-linux`. So:

- what the user's `docker ps` talks to: `npipe:////./pipe/dockerDesktopLinuxEngine`
- what an SDK client would talk to: `npipe:////./pipe/docker_engine`

Docker Desktop usually serves both pipes, but `docker_engine` is bound to *whichever engine Desktop last activated*. Flip Desktop to Windows containers and `docker_engine` becomes the Windows engine while `dockerDesktopLinuxEngine` stays Linux. An SDK build would then be inspecting a **different daemon than the one Compose just deployed to**, and the symptom would be "container not found" with no clue why.

Fixing that means reimplementing Docker's context resolution: parse `config.json`, hash the context name, read the meta blob, decode `Endpoints.docker.Host`, handle `DOCKER_CONTEXT`. That is copying a chunk of Docker's own source into our tree and keeping it in sync forever. The CLI does it for free and, critically, **always agrees with what the user sees when they type `docker ps`**, which for a debugging tool is not a convenience, it is a correctness property.

### 1.3 The supporting reasons

1. **No stable Compose Go API.** `github.com/docker/compose/v2` is importable but explicitly unstable; it transitively pulls buildkit, containerd, and the whole CLI: hundreds of modules. Directly contradicts "minimize third-party dependencies."
2. **SDK version matrix.** The engine client negotiates an API version; a client built against one `docker/docker` tag against a newer daemon requires `client.WithAPIVersionNegotiation()` and still breaks on type churn. `docker/docker` also drags `go-connections`, `distribution/reference`, `opencontainers/image-spec`, `pkg/errors`, and on Windows `Microsoft/go-winio` for npipe dialing. ~40 modules to run two commands in Phase 0.
3. **No capability is lost.** Everything Phase 2 needs is CLI-reachable with structured output: `docker inspect --format '{{json .}}'`, `docker exec`, `docker kill --signal`, `docker pause`/`unpause`, `docker events --format json` (streaming), `docker stats --format json`, `docker cp`.
4. **Debuggability.** Every action the harness takes is a command a human can paste into PowerShell. When a run wedges, the verdict artifact contains the literal argv that failed. With the SDK the equivalent is a stack trace.

### 1.4 The honest cost, and how it is contained

The CLI is an **unversioned contract**. Output shapes have changed across Compose v2 minors. Containment:

- **Never parse human output.** Always `--format json` / `--format '{{json .}}'`. Verified available on this binary.
- **Shape-sniff, don't assume.** `docker compose ps --format json` has historically emitted both newline-delimited objects *and* a single JSON array depending on Compose version. I am not going to assert which version flipped from memory. The decoder trims leading whitespace and branches on the first byte: `[` → array, `{` → NDJSON stream. Cheap, and immune to the question.
- **Fall back, don't fail.** If `compose ps` JSON lacks a field we need (e.g. `Publishers`), fall through to `docker ps --filter label=… --format '{{json .}}'` + `docker inspect`, which are far older and far more stable surfaces.
- **Probe capabilities at preflight, record them.** `docker version --format '{{json .}}'` and `docker compose version --format json` are captured verbatim into `.prothesis/runs/<id>/env.json`. A verdict is not reproducible unless you know which CLI produced it.
- **Version floor.** Refuse below Compose v2.21 (the era where `ps --format json`, `--wait-timeout`, and stable labels all landed) with exit **2** and a remediation string.
- **Streaming is the one weak spot** (`docker events`, `compose logs -f` need reconnect-on-EOF logic that an SDK would give more directly). Phase 1 concern; a supervised line-reader with restart-and-resume-by-`--since` is ~80 lines. Acceptable.

### 1.5 Exact command sequences

`{{P}}` = project name (§5.1). `{{F}}` = each `-f` file, absolute. `{{OV}}` = generated overlay (§2.2).

**`thesis up`**

```
 0  docker version --format {{json .}}
      err                        -> exit 2  "Docker daemon unreachable. Start Docker Desktop."
      .Server.Os != "linux"      -> exit 2  "Windows-container mode active; switch to Linux containers."
    docker compose version --format json          -> version floor check
 1  docker compose -p {{P}} --project-directory {{DIR}} -f {{F}}... config --format json
      -> canonical model: services, ports (fully expanded), networks, volumes, images
      err -> exit 5 CONFIG_ERROR (compose file is invalid)
 2  [local] preflight host ports: net.Listen("tcp","127.0.0.1:P") for each declared published port
      in-use -> exit 5 naming the port and the likely holder
 3  [local] write .prothesis/state/harness.json.tmp -> fsync -> rename      // INTENT record, before any mutation
 4  [local] render {{OV}} = .prothesis/state/{{P}}.overlay.yaml   (labels only; §2.2)
 5  docker compose -p {{P}} --project-directory {{DIR}} -f {{F}}... -f {{OV}} \
        up --detach --wait --wait-timeout {{S}} --remove-orphans \
           --pull missing --quiet-pull --progress json
      nonzero -> capture progress-json stream + `compose logs --no-color --tail 200` -> exit 2
 6  docker compose -p {{P}} -f {{F}}... -f {{OV}} ps --all --no-trunc --format json
      -> bind logical nodes (§2.3)
 7  docker inspect --format {{json .}} <cid>...            // one call, N args: pid, labels, NetworkSettings, State
 8  [local] resolve endpoints (§3.1), run BOOT health probes (§3.3)
 9  [local] rewrite harness.json with the node table + resolved endpoints; state = "up"
```

Note on step 5: `--wait` waits for `running|healthy` using *Compose's own* healthchecks. That is not the same thing as PRO-THESIS's `harness.health` probes, which are the orchestrator-level BOOT gate (§4.1 of the spec). Both exist and must not be conflated: a service with no compose healthcheck satisfies `--wait` the instant it is `running`, which is nearly meaningless. `--wait` is a cheap early-abort; step 8 is the real gate.

**`thesis down`**: see §5.3 for the full algorithm and its guarantees.

```
 1  [local] resolve {{P}}: state file if present, else derive from config (§5.1)
 2  docker compose -p {{P}} -f {{F}}... -f {{OV}} down --remove-orphans [--volumes] \
        --timeout {{T}} --progress json                       // tolerate "no configuration file" / missing project
 3  docker ps -aq --filter label=com.docker.compose.project={{P}}
    docker ps -aq --filter label=io.prothesis.project={{P}}     // catches non-compose containers we spawned
      any -> docker rm --force <ids>...
 4  docker network ls -q --filter label=com.docker.compose.project={{P}}   -> docker network rm <ids>...
 5  [if volumes] docker volume ls -q --filter label=com.docker.compose.project={{P}} -> docker volume rm --force
 6  re-run 3/4/5 as verification.  empty -> delete state file, exit 0
                                   residual -> exit 2 + print the exact docker commands to run by hand
```

Phase 2 additions, all CLI, no new dependency:
`docker kill --signal=SIGKILL <cid>` (proc.kill) · `docker pause`/`docker unpause` (proc.pause, §D13) · `docker restart --time N` then rebind (proc.restart) · `docker exec` into the ambassador control socket (net.*) · `docker update --cpus` (proc.slow) · `libfaketime` env at create (clock.*).

---

## 2. Logical node model

### 2.1 The problem, stated precisely

Config declares `{id: n1, service: kv, role_hint: replica}` three times against one service. Compose has services and containers; a service may scale; container IDs change on recreate; and `role:leader` is not knowable until runtime and *moves during the run*. Three separate problems wearing one hat.

**Framing that resolves it: node identity is config-time and permanent; container identity is runtime and disposable.** Nothing outside `internal/harness` may ever hold a container ID. The perturber says "pause n2"; the harness resolves.

### 2.2 Binding mechanism: a generated label overlay

Rather than infer the mapping from compose's ordering, **we stamp it**. `up` renders a small overlay file and appends it as the last `-f`:

```yaml
# .prothesis/state/thesis-kvfixture-a41f0c2e.overlay.yaml  — GENERATED, do not edit
services:
  kv1:
    labels:
      io.prothesis.node: "n1"
      io.prothesis.service: "kv"
      io.prothesis.role_hint: "replica"
      io.prothesis.project: "thesis-kvfixture-a41f0c2e"
      io.prothesis.run: "r_2026_09_06_7b31"
      io.prothesis.owner_pid: "24188"
      io.prothesis.created_at: "2026-09-06T11:04:22Z"
  kv2: { labels: { io.prothesis.node: "n2", ... } }
  kv3: { labels: { io.prothesis.node: "n3", ... } }
  helpers: { labels: { io.prothesis.system: "helper" } }
  amb1:    { labels: { io.prothesis.system: "ambassador", io.prothesis.ambassador_for: "n1" } }
```

Discovery becomes `docker ps -a --filter label=io.prothesis.node=n1 --format '{{json .}}'`: exact, ordering-free, and it works even when the state file is gone. Compose merges multiple `-f` natively, so the fixture author writes nothing.

Two consequences to be aware of:
- Labels participate in compose's `config-hash`, so adding the overlay causes a one-time recreate the first time it appears. Consistently, though: repeated `up` with the same overlay is a genuine no-op.
- The overlay must be listed in *every* subsequent compose invocation for that project (`ps`, `down`), or compose computes a different config hash and thinks containers are stale. The `composeArgs()` helper always appends it; there is exactly one place to get this right.

### 2.3 The binding rule (schema is frozen: nothing invented)

The frozen node schema is `{id, service, role_hint}`. No ordinal field, so the ordinal must be *derived*:

> **Nodes declared against the same `service`, in config declaration order, bind to that service's containers ordered by the `com.docker.compose.container-number` label, ascending.** Count mismatch → exit 5 with both numbers.

With the label overlay this rule is only ever exercised when a service is scaled >1, because each `service` in the fixture hosts exactly one node and the label is unambiguous. For scaled services the rule is a documented best-effort: compose does not contractually guarantee container-number stability across recreate. → `OPEN_QUESTIONS` #4; the mitigation is the fixture convention in §2.6.

### 2.4 Types

```go
package harness

// NodeID is the stable, config-time identity. Everything outside this package
// targets a NodeID. Container IDs never escape internal/harness.
type NodeID string

// Node is config-time and immutable for the life of a run.
type Node struct {
	ID       NodeID `json:"id"`
	Service  string `json:"service"`   // compose service name
	RoleHint string `json:"role_hint"` // static hint only; NOT the live role
	Ordinal  int    `json:"ordinal"`   // derived: index within Service, 0-based
	System   bool   `json:"system"`    // helpers/ambassadors: never a fault target, exempt from no_crash
}

// Binding is runtime and EPHEMERAL. Valid only until the next lifecycle event
// on this node. Never cache across proc.restart / proc.kill / force-recreate.
type Binding struct {
	Node        NodeID              `json:"node"`
	ContainerID string              `json:"container_id"` // 64-hex, full
	Name        string              `json:"name"`         // thesis-kvfixture-a41f-kv1-1
	State       ContainerState      `json:"state"`        // running|paused|exited|restarting|created|dead
	Health      HealthStatus        `json:"health"`       // healthy|unhealthy|starting|none
	ExitCode    int                 `json:"exit_code"`
	PID         int                 `json:"pid"`          // pid inside the Docker Desktop VM, NOT a Windows pid
	Endpoints   map[string]Endpoint `json:"endpoints"`    // "client","peer","ctl"
	Generation  uint64              `json:"generation"`   // ++ on every rebind; stale-use detector
	ObservedAt  time.Time           `json:"observed_at"`
}

// Endpoint carries BOTH vantage points. Which one is usable depends on where
// the caller runs. On Windows the host CANNOT reach InNetwork. See §3.1.
type Endpoint struct {
	Name          string `json:"name"`
	ContainerPort int    `json:"container_port"`
	Protocol      string `json:"protocol"`
	HostAddr      string `json:"host_addr"`   // "127.0.0.1" or "" if unpublished
	HostPort      int    `json:"host_port"`   // 0 if unpublished
	InNetwork     string `json:"in_network"`  // "kv1:8080" — compose DNS, VM-internal only
	Published     bool   `json:"published"`
}

type Topology struct {
	Project   string             `json:"project"`
	Backend   string             `json:"backend"`
	Nodes     []Node             `json:"nodes"`     // sorted by ID for stable serialization
	Bindings  map[NodeID]Binding `json:"bindings"`
	Networks  []string           `json:"networks"`
	Env       EnvInfo            `json:"env"`
	CreatedAt time.Time          `json:"created_at"`
}
```

`Generation` is the guard rail. `Freeze`/`Signal`/`Exec` take a `NodeID` and resolve internally, so there is no ergonomic reason to hold a `Binding`. Where a caller genuinely must (the perturber holds one to describe the fault it injected), a `Backend.Validate(b Binding) error` returns `ErrStaleBinding` when generations diverge.

### 2.5 Surviving `proc.restart`

| Operation | Container ID | Generation | Endpoints |
|---|---|---|---|
| `docker restart` / `compose restart` | same | ++ (PID, state changed) | same |
| `docker stop` + `docker start` | same | ++ | same |
| `compose up --force-recreate` | **new** | ++ | host port same (fixed), in-network same |
| `docker rm -f` + `compose up` | **new** | ++ | same |

```go
func (b *composeBackend) Restart(ctx context.Context, id NodeID, o RestartOptions) (Binding, error) {
	n, err := b.node(id)
	if err != nil { return Binding{}, err }
	prev, _ := b.Resolve(ctx, id)

	if o.Recreate {
		// New container ID. Only this service; --no-deps so peers are untouched.
		if err := b.compose(ctx, "up", "-d", "--force-recreate", "--no-deps", n.Service); err != nil {
			return Binding{}, err
		}
	} else {
		if err := b.docker(ctx, "restart", "--time",
			strconv.Itoa(int(o.StopGrace.Seconds())), prev.ContainerID); err != nil {
			return Binding{}, err
		}
	}
	// Rebind unconditionally: even a same-ID restart changed PID and State.
	return b.rebind(ctx, id) // re-discovers by label, bumps Generation
}
```

A restart that must be *observed* to have completed (Phase 2's `withdraw()` verification) chains `rebind` with a bounded `waitFor(id, StateRunning)` and then the node's health probe, because a container that is `running` but not yet serving is exactly the gray state the fixture is built to expose, and mistaking it for healed would corrupt the HEAL assertion.

### 2.6 Fixture convention: one compose service per logical node

The fixture uses `kv1`/`kv2`/`kv3`, not `kv` scaled to 3. Four independent reasons converge on this, which is how you know it is right:

1. **Distinct published host ports.** §3.1 requires each node to publish its own client port for host-side probing. A scaled service cannot statically publish distinct ports; port *ranges* assign nondeterministically, which destroys the node↔endpoint mapping.
2. **Distinct peer configuration.** Each Raft node needs its own peer list pointing at its own ambassador (§4.4). Scaled replicas share one environment block.
3. **Unambiguous binding.** §2.3's ordering rule never has to be exercised.
4. **Targeted recreate.** `compose up --force-recreate --no-deps kv2` restarts exactly one node. Under scaling there is no way to name replica 2.

The cost (three near-identical service blocks) is paid once by YAML anchors and is trivially worth it.

### 2.7 `role:leader`: the Phase 2 seam, designed now

Raft leadership moves at runtime. Role must be resolved **at injection time**, and the resolution must be **recorded**, or replay is not replay.

```go
// RoleResolver maps a node group to live roles. Phase 2 wires it in; the
// interface exists in Phase 0 so nothing has to change shape later.
type RoleResolver interface {
	Roles(ctx context.Context, group string) (map[NodeID]string, error)
	Source() string // "static" | "probe:/status" | "log" — recorded in the world
}

var (
	ErrNoRole        = errors.New("harness: no node holds the requested role")
	ErrRoleAmbiguous = errors.New("harness: multiple nodes claim the requested role")
)
```

Three implementations, selected by config, all behind that one interface:

1. **`StaticRoleResolver`** (Phase 0/1 default): returns `role_hint`. `role:leader` yields `ErrNoRole` with "no dynamic role source configured", exit 5. Honest: it does not pretend.
2. **`ProbeRoleResolver`** (Phase 2 default for the fixture): HTTP GET each node's `/status` → `{"role":"leader","term":4}`. Highest `term` among self-declared leaders wins; a tie at the same term is `ErrRoleAmbiguous`. The fixture already needs `/status` for the Phase 4 state-abstraction tuple `(role, term, queue_depth, conn_count)`, so this is not an extra ask.
3. **`LogRoleResolver`** (Phase 2b): regex over the log-template stream (`elected leader term N`). Requires zero cooperation from the system under test, which is what CRUCIBLE PART 10 demands of an "unmodified systems" tool.

**Two semantics that must be fixed now because they leak into the world file format:**

*Intension vs extension.* A fault carries both the target expression it was written with and the node set it resolved to:

```go
type ResolvedTarget struct {
	Expr        string   `json:"expr"`          // "role:leader"
	Nodes       []NodeID `json:"nodes"`         // ["n2"]  (sorted)
	ResolvedAt  int64    `json:"resolved_at_ms"`// virtual clock
	Source      string   `json:"source"`        // "probe:/status"
	Observation string   `json:"observation"`   // "term=4"
}
```
`.thesis` stores both. Replay defaults to `--replay-targets=recorded` (deterministic: re-pause n2 regardless of who leads now) and offers `--replay-targets=live` (re-resolve: tests whether the bug is about *the leader* or about *n2*). Tier B honesty per CRUCIBLE PART 5: the tool never claims determinism it does not have.

*Unresolvable targets must not abort the world.* During a partition there may be zero leaders or two in different terms: that is the interesting state, not an error. `ErrNoRole`/`ErrRoleAmbiguous` at injection time → skip that fault, append `{"type":"info","event":"target_unresolved","expr":"role:leader","reason":"no_leader"}` to the history log, and record it in the world. Aborting here would systematically discard exactly the worlds most likely to contain bugs.

---

## 3. Health probes

### 3.1 `{host}` and `{port}`: the substitution, and what it costs the fixture

**The hard constraint.** The Windows host has no route into the Docker Desktop Linux VM's bridge networks. The `thesis` process runs on Windows. Therefore **container IPs and compose DNS names are unreachable from `thesis`.** (There are folklore workarounds via the WSL adapter; they are unreliable, they differ under WSL mirrored networking, and they do not exist under the Hyper-V backend or in CI. Design as if they do not exist.)

**Decision; the substitution is a function of publication, decided per node, atomically, and recorded:**

```
if the node's probe port is PUBLISHED:
      {host} = "127.0.0.1"        {port} = the published HOST port      -> probe runs on the Windows host
else:
      {host} = the compose network alias   {port} = the CONTAINER port  -> probe runs via `docker exec`
```

This needs no new schema field: publication is observable from `docker compose config --format json` (declared) and `docker compose ps --format json` (actual). It is deterministic, it is logged, and the fully-substituted URL is written into the run artifact so it is never a mystery.

`{port}` when a node publishes several ports resolves to the node's **primary probe port**: the sole published port if there is exactly one, else the first entry in the service's canonical `ports:` list from `compose config --format json` (canonical order is deterministic). Ambiguity beyond that → exit 5 listing the candidates.

Unknown placeholders (`{node}`, `{service}`, …) are a **CONFIG_ERROR**, not a silent pass-through. A URL that still contains a literal `{node}` would produce a probe failure attributed to the system under test rather than to the config: fail closed.

**⚠ The consequence for the fixture, stated plainly:** every node that needs a health probe **must publish a host port**, and per §2.6 that means one compose service per node with a fixed, distinct `127.0.0.1:PORT` binding. There is no version of this design where the fixture uses a single scaled `kv` service and host-side probes both work.

**A second, subtler consequence that matters for Phase 2.** Because the client port is *published*, the host→node probe path bypasses the compose network entirely and is therefore **outside the peer fault domain**. `net.partition(n1<->n2)` must not blind the BOOT gate or `availability_after_heal`. That is the correct semantic: a partition between replicas should not make a replica unreachable *to clients*; that distinction is precisely what makes the stale-read bug observable. §4 preserves it by putting only the *peer* port behind ambassadors.

### 3.2 Ports

⚠ **VERIFIED** reserved ranges on this machine: `1462`, `5357`, `50000-50059`, `62590-63523`. Chosen bases are clear.

| Purpose | Base | Allocation |
|---|---|---|
| KV client HTTP (published) | `THESIS_PORT_BASE` = 18080 | 18081/18082/18083 |
| Raft peer (**not** published) | n/a | 9090 container-internal only |
| Ambassador control (published, debug) | 19100 | 19101/19102/19103 |
| Ambassador peer listeners (not published) | n/a | 19001/19002/19003 |

Fixture compose writes `"127.0.0.1:${THESIS_PORT_BASE:-18080}1:8080"`-style bindings so concurrent runs can be offset. Preflight `net.Listen`s each one first and fails with exit 5 naming the port.

⚠ **Windows hazard:** bind failures inside a WinNAT-reserved range surface as `permission denied` / "An attempt was made to access a socket in a way forbidden by its access permissions", not `address in use`. The error message must tell the user to run `netsh interface ipv4 show excludedportrange protocol=tcp`, or they will chase a ghost.

⚠ **Windows hazard:** bind to `127.0.0.1`, never `0.0.0.0`. `0.0.0.0` triggers a Windows Defender Firewall prompt on first bind (blocking, GUI, invisible in CI) and exposes the fixture to the LAN.

### 3.3 Probe execution

```go
type ProbeSpec struct {
	Selector string        // "kv:*" — same target grammar as faults (target.go)
	Template string        // "http://{host}:{port}/healthz"
	Timeout  time.Duration // overall deadline for the whole selector
}

type ProbeResult struct {
	Node     NodeID
	URL      string        // fully substituted — recorded, never re-derived
	OK       bool
	Status   int
	Latency  time.Duration
	Attempts int
	Err      string
}

// WaitHealthy is the BOOT gate. It returns only when EVERY matched node passes
// in the SAME poll round; a node that passes once and then flaps does not
// satisfy the gate.
func (h *Harness) WaitHealthy(ctx context.Context, specs []ProbeSpec) ([]ProbeResult, error)
```

Details that are load-bearing:

- **Full HTTP round trip, never a bare TCP dial.** ⚠ **Windows hazard:** Docker Desktop's port forwarder accepts the TCP connection on the host side *before* the container is listening, then resets. A connect-only probe reports healthy against a dead container. Issue a real GET and require 2xx.
- **`127.0.0.1`, never `localhost`.** ⚠ **Windows hazard:** `localhost` resolves `::1` first on Windows; Docker Desktop's forwarder binds v4 and v6 inconsistently, producing intermittent `connection refused` that looks like flaky software. This one costs hours if you do not know it.
- **Per-attempt timeout ≠ overall timeout.** Per-attempt default 2s; overall from config (`30s`). A single 30s-hanging request must not consume the whole budget.
- **Poll interval jitter is drawn from RECORDER**, not `math/rand`, so BOOT timing is part of the seed. Injected as a narrow interface so `harness` need not import `recorder`:
  ```go
  type RandSource interface{ Uint64() uint64 }
  ```
- **HTTP client**: one shared client, explicit `Transport`, `DisableCompression: true`, `MaxIdleConnsPerHost: 8`, `DialContext` with a 1s connect timeout. Do **not** use `http.DefaultClient` (no timeout: a hung probe wedges the run forever).
- **Failure at BOOT is exit 2 (INCONCLUSIVE), not exit 1.** The system never got to a testable state; that is an environment outcome, not an oracle violation. Contrast `availability_after_heal`, which probes the same endpoints during ASSERT and *is* an oracle → exit 1. Same probe machinery, different phase, different exit code. Easy to get wrong; getting it wrong makes CI blame the developer for a slow laptop.

### 3.4 `steady_state: "thesis-helpers/steady.sh"`: the real problem

Three compounding facts: Windows has no `/bin/sh`; ⚠ **VERIFIED** Git Bash on this machine cannot fork, so installing it does not help; and Go's `exec` has no shebang handling, so exec'ing a `.sh` on Windows fails with `%1 is not a valid Win32 application`.

**Resolution: run the helper *inside a container*. Tier 1, the default.**

The fixture gains a `helpers` service (`io.prothesis.system=helper`) with the helper scripts **baked in via `COPY`**, joined to the compose network, `entrypoint: ["sleep","infinity"]`.

```
docker compose -p {{P}} -f ... exec -T --workdir /thesis-helpers helpers \
      /bin/sh /thesis-helpers/steady.sh
```

This is not a Windows workaround that we tolerate; it is the better design on every platform, for three reasons:

1. **Correct vantage.** Steady state for a Raft cluster means "all three peers agree on term and commit index." That requires talking to all peers on the *peer* network. After Phase 2 introduces partitions, a host-side steady check would be measuring the wrong network. The in-network probe is the semantically right one.
2. **Platform-identical results.** Invariant I2 (a world must reproduce) is violated the moment the steady-state predicate depends on which OS the operator is running. Same container, same `sh`, same result on Windows, macOS, Linux, CI.
3. **Zero host dependencies.** No shell, no WSL, no Git Bash.

Cost: the helper container is part of the topology. It is excluded from fault targeting and from `no_crash` via `System: true`, and it must stay `running` or `--wait` fails (hence `sleep infinity`, not a one-shot).

⚠ **Windows hazards this specifically avoids or must still handle:**
- **CRLF is fatal.** A `.sh` checked out with CRLF fails as `bad interpreter: /bin/sh^M: no such file or directory`. Mitigations, all three: (a) `.gitattributes` with `*.sh text eol=lf`, committed by `thesis init`; (b) preflight scans `thesis-helpers/*.sh` for `\r\n` and fails with exit 5 naming the file; a two-line check that saves a support ticket; (c) `COPY` at build time rather than bind-mount, so the file is validated once at image build.
- **No `+x` bit through a Windows bind mount.** Hence `/bin/sh script.sh` explicitly, never `./script.sh`. Required, not stylistic.
- **⚠ Silent empty bind mounts.** If the project lives on a drive not enabled in Docker Desktop file sharing (or a network/UNC path), a bind mount produces an **empty directory in the container with no error**. `steady.sh` then "does not exist" and the failure is attributed to the fixture. This is the single strongest argument for `COPY` over bind-mount, and why bind-mounting helpers is an opt-in dev convenience (`--dev-mount-helpers`) rather than the default.
- `exec -T` disables TTY allocation. Without it, output is mangled by control sequences and Windows console handling; with it, stdout/stderr are clean byte streams. Always pass it.

**Tier 2, fallback: explicit host interpreter resolution.** For a `steady_state` that is not a `.sh`, or when `THESIS_EXEC_MODE=host`. Resolution order, first hit wins:

```
$THESIS_SHELL                          (explicit escape hatch, absolute path)
busybox.exe sh                         (if on PATH — a single self-contained exe)
wsl.exe -d <distro> -- /bin/sh ...     (VERIFIED available here: Ubuntu-24.04)
pwsh -NoProfile -File script.ps1       (for .ps1)
the file itself                        (for .exe)
```
⚠ **Never fall back to `cmd.exe`.** It would execute a `.sh` as a batch file, producing arbitrary garbage that might exit 0: a *false pass* on a gate. Fail closed instead.

**Tier 3: reject.** No execution path resolvable → exit 5 at `thesis doctor` / preflight, **before BOOT**, so the failure arrives in the first second rather than after a 60s timeout.

This resolver is not a probe detail. `driver.cmd` (`./bin/loadgen …`) has the identical problem in Phase 1, so it lives in `execplan.go` as `harness.ExecPlanner` and both callers use it.

---

## 4. Network faults: ambassador proxies vs tc/netem

Required per directed edge: **partition, delay, drop, reorder, duplicate, bandwidth-limit**, plus a stated path to `msg.corrupt`.

### 4.1 tc/netem + iptables in a NET_ADMIN sidecar

Sidecar joins the target's netns (`network_mode: "service:kv1"`, `cap_add: [NET_ADMIN]`), runs `iproute2`.

**In favor:** kernel-quality netem primitives; `delay`/`loss`/`reorder`/`duplicate`/`rate` are all native and statistically better than anything we would write. No topology change: works on an *unmodified* system, which is CRUCIBLE PART 10's stated requirement.

**Against, in descending severity:**

1. **⚠ netem randomness is in-kernel and unseedable.** This is disqualifying. Invariant **I2 (every failure is a seed**) requires that all orchestration-level nondeterminism be drawn from RECORDER. `netem loss 30%` makes its drop decisions from kernel entropy we cannot seed, record, or replay. A world containing `net.loss(30)` implemented via netem is *not reproducible*, and the verdict's `reproduced: "3/3"` would be a claim we cannot support. The addendum is explicit that the tool must never claim determinism it does not have.
2. **netem is egress-only and per-interface, not per-peer.** Directed-edge control requires `tc filter` classifying by destination IP into HTB/prio classes with per-class netem. That classification must be rewritten every time a container IP changes, which is every `up` and every `--force-recreate`. Ingress shaping needs `ifb` redirection, requiring the `ifb` module in the Docker Desktop LinuxKit kernel. We cannot `modprobe` there and cannot verify it in advance. If it is absent there is no user-space remedy.
3. **`withdraw()` is fragile in exactly the way §4.3's *Critical Guarantee* fears.** Rules live in the *target's* netns, not the sidecar's. If the sidecar dies mid-fault the rules persist invisibly, poisoning every subsequent world in the corpus with a fault nobody recorded. HEAL's "assert zero residual" then requires re-entering the netns to enumerate qdiscs and iptables chains: verification that is itself failure-prone.
4. **Requires `NET_ADMIN`**: a privilege escalation baked into the topology, which some CI environments refuse outright.
5. **No path to message-level faults.** netem `corrupt` flips random bits; it is not protocol-aware. `msg.corrupt`, `msg.delay`, partial-write, and truncation faults are permanently unreachable.
6. **No analogue in other backends.** `process` on Windows: impossible. `sim`: meaningless. The `Backend` abstraction would have a hole in it.

### 4.2 Ambassador proxies

A small Go TCP proxy (`thesis-proxy`, built from our own tree) between peers. Each node's peer configuration points at proxy addresses instead of peer addresses.

**In favor:**

1. **⚠ Seeded, replayable randomness.** Loss/reorder/duplicate decisions come from `RECORDER.Stream(edgeID)`. The network's nondeterminism becomes part of the world tuple. This alone decides it.
2. **Every required primitive is directly and exactly expressible.** partition = accept and blackhole; delay = timer wheel; loss = drop a chunk; reorder = swap adjacent buffered chunks; duplicate = re-emit; bandwidth = token bucket. No approximations.
3. **`withdraw()` is one control call and is *verifiable*.** `GET /policy` returns live state; HEAL asserts every edge reports identity. And the failure mode is loud, not silent: if a proxy dies the edge goes *down*, which health probes catch immediately; the opposite of a stale iptables rule that silently persists.
4. **Forward path to `msg.corrupt`.** The proxy sees bytes and can be taught a codec later.
5. **No `NET_ADMIN`, no kernel modules, no privileged containers.**
6. **The abstraction survives every backend.** Proxies are local processes under `process` (so `net.*` works on Windows even though `proc.pause` cannot), sidecars under `k8s`, and a pure function under `sim`. One `EdgePolicy` struct, four implementations.

**Against, honestly:**

1. **Topology invasion.** Peers must address each other through proxies. Free for our fixture, which we author, but on a third-party system it requires a peer-address override, contradicting "works on unmodified systems." → `OPEN_QUESTIONS` #2. Resolution: proxies are the default for cooperative systems; the `tc` provider stays as a documented, lower-fidelity, **explicitly non-replayable** option for unmodified ones, behind the same `NetworkProvider` interface. This is exactly why the decision must be taken now: the *interface* admits both, the *fixture* commits to proxies.
2. **⚠ Fidelity, and one trap that would silently break Phase 2's Definition of Done.** A TCP proxy terminates connections. A real partition drops packets and lets TCP retransmit for minutes before the peer notices; a proxy that closes the socket hands the application an *immediate*, clean error. Those are completely different bugs, and the easy one is the one you get by accident.
   **Therefore `partition` mode must BLACKHOLE, not reset**: hold established connections open, accept and discard bytes, never send FIN or RST; accept new connections and never respond. That reproduces the gray failure ("unresponsive to peers, alive to the orchestrator") that the spec calls the top priority and that the fixture's lease bug requires. `partition_reset` exists as a *separate, explicitly named* mode. If someone implements partition as `conn.Close()`, the fixture's stale read will not reproduce and the cause will be extremely hard to find.
3. **O(n²) edges.** Solved by topology, not by accepting it: **one egress ambassador container per node, with one listener per peer.** 3 nodes → 3 proxy containers, 6 directed policies. O(n) containers, O(n²) policies.
4. **TCP only in v1.** The fixture is HTTP/gRPC so this is fine. UDP is a documented proxy TODO.

### 4.3 Recommendation

**Ambassador proxies, egress-per-node.** Decided now, because it changes the fixture's compose topology and rewriting that in Phase 2 would invalidate every world in the corpus.

**Phase 0 cost is deliberately near zero:** `thesis-proxy` ships in Phase 0 as a ~60-line pure passthrough (`net.Listen` → `io.Copy` both ways), no control plane, no policy. The *topology* is final; the *behavior* arrives in Phase 2. Phase 0's DoD is unaffected and Phase 2 changes no YAML.

### 4.4 Resulting fixture topology

```
network thesisnet (bridge, labeled io.prothesis.project)

┌─ kv1 (svc kv1, alias kv1) ────────────┐   client 8080 → published 127.0.0.1:18081   ← health probe, loadgen
│   RAFT_PEERS = n2@amb1:19002,         │   peer   9090 → NOT published
│               n3@amb1:19003           │
└───────────────────────────────────────┘
┌─ amb1 (thesis-proxy, alias amb1) ─────┐   ctl 9100 → published 127.0.0.1:19101 (debug only)
│   :19002 ─ edge n1→n2 ─→ kv2:9090     │
│   :19003 ─ edge n1→n3 ─→ kv3:9090     │
└───────────────────────────────────────┘
   kv2 / amb2 and kv3 / amb3 symmetric   (18082/19102, 18083/19103)

┌─ helpers (alias helpers) ─────────────┐   no ports; /thesis-helpers baked via COPY
│   entrypoint sleep infinity           │   io.prothesis.system=helper
└───────────────────────────────────────┘
```

The two-port split is the crux:

- **8080 client: published.** Health probes and the load generator reach it from Windows, *outside* the peer fault domain. So `net.partition` between replicas does not blind the BOOT gate or `availability_after_heal`, and the stale read stays observable through the partition, which is the entire point of the fixture.
- **9090 peer: unpublished, reachable only via ambassadors.** All fault-injectable traffic flows here.

⚠ **A fixture bug that dials `kv2:9090` directly would make every network fault silently ineffective, and Phase 2's DoD would fail for reasons that look like a Raft bug.** Compose DNS resolves `kv2` on the shared network, so nothing prevents it structurally. Two defenses: (a) the fixture's config contains only ambassador addresses; (b) a Phase 2 **self-test**; inject a full partition and assert the cluster reports loss of quorum within an election timeout; if it does not, exit **2 INCONCLUSIVE** ("network faults are not reaching the system under test"), never pass. A chaos tool whose faults do nothing and reports green is worse than no tool.

### 4.5 The `NetworkProvider` seam

```go
type Edge struct{ From, To NodeID } // DIRECTED. n1<->n2 is two edges.

type EdgeMode int
const (
	ModePass      EdgeMode = iota
	ModeBlackhole          // hold open, discard, never FIN/RST  <- default for net.partition
	ModeReset              // immediate RST                      <- opt-in, different bug class
)

type EdgePolicy struct {
	Mode         EdgeMode
	DelayMean    time.Duration
	DelayJitter  time.Duration
	LossPct      float64
	ReorderPct   float64
	DuplicatePct float64
	BandwidthBps int64
	Seed         uint64 // from RECORDER; makes the edge replayable
}

type NetworkProvider interface {
	Edges(ctx context.Context) ([]Edge, error)
	Apply(ctx context.Context, e Edge, p EdgePolicy) error
	Get(ctx context.Context, e Edge) (EdgePolicy, error) // HEAL verification
	ResetAll(ctx context.Context) error                  // HEAL
	Capabilities() NetCaps
}
```

`HEAL` = `ResetAll` then `Get` every edge and assert `ModePass` with zero parameters. A provider that cannot answer `Get` cannot satisfy §4.3's *Critical Guarantee*, which is a further argument against tc.

---

## 5. Lifecycle and teardown guarantees

### 5.1 Deterministic project name: the foundation of orphan recovery

```go
func ProjectName(cfgName, cfgAbsPath string) string {
	p, err := filepath.EvalSymlinks(cfgAbsPath)
	if err != nil { p = cfgAbsPath }
	p = filepath.ToSlash(filepath.Clean(p))
	if len(p) > 1 && p[1] == ':' {              // "C:/..." -> "c:/..."
		p = strings.ToLower(p[:2]) + p[2:]
	}
	sum := sha256.Sum256([]byte(strings.ToLower(p)))
	return sanitize(cfgName) + "-" + hex.EncodeToString(sum[:4]) // thesis-kvfixture-a41f0c2e
}
```

Two properties, both required:

- **Derivable from config alone.** After a crash there is no state file; `down` must still know what to destroy. It re-derives.
- **Path-scoped.** Two checkouts of the same repo get different projects and cannot destroy each other's containers.

⚠ **Windows hazards in those five lines:** `C:\x` and `c:/x` and `C:\X\..\x` are the same directory and must hash identically; hence `EvalSymlinks` + `Clean` + `ToSlash` + drive-letter lowering. Compose requires `[a-z0-9][a-z0-9_-]*`, so `sanitize` lowercases and maps everything else to `-`. ⚠ **VERIFIED**: this project path is `C:\AI Projects\Pro-synthesis` (a space and a hyphen) so both code paths are exercised on day one.

### 5.2 Labels are the actual guarantee

Signal handlers do not survive `SIGKILL`, Task Manager, a BSOD, or a closed console window. **Labels do.** Everything the harness creates carries:

```
com.docker.compose.project = <project>        (compose's own)
io.prothesis.project       = <project>
io.prothesis.node          = <node id>        (service containers)
io.prothesis.run           = <run id>
io.prothesis.owner_pid     = <pid>
io.prothesis.created_at    = <RFC3339>
io.prothesis.system        = helper|ambassador|""   (excluded from targeting and no_crash)
```

Any PRO-THESIS artifact ever created, anywhere on the machine, is findable with `docker ps -aq --filter label=io.prothesis.project`. That is what makes crash recovery a *guarantee* rather than a hope.

### 5.3 `down`: idempotent and orphan-proof

```go
func (b *composeBackend) Down(ctx context.Context, o DownOptions) (*DownReport, error) {
	proj := b.project // state file if present, else re-derived from config
	rep := &DownReport{Project: proj}

	// 1. Best effort. A missing project is success, not an error.
	if err := b.compose(ctx, downArgs(o)...); err != nil && !isNoSuchProject(err) {
		rep.ComposeErr = err.Error() // recorded, not fatal — the reaper is the real guarantee
	}

	// 2. Reap by BOTH label families. compose's label misses anything we
	//    started outside compose (Phase 2 ad-hoc ambassadors, one-off execs).
	for _, sel := range []string{
		"com.docker.compose.project=" + proj,
		"io.prothesis.project=" + proj,
	} {
		ids, _ := b.dockerLines(ctx, "ps", "-aq", "--filter", "label="+sel)
		if len(ids) > 0 {
			rep.ForceRemoved = append(rep.ForceRemoved, ids...)
			_ = b.docker(ctx, append([]string{"rm", "--force", "--volumes"}, ids...)...)
		}
	}

	// 3. Networks. "has active endpoints" means step 2 raced; retry once.
	b.reapNetworks(ctx, proj, rep)

	// 4. Volumes, only if requested.
	if o.Volumes { b.reapVolumes(ctx, proj, rep) }

	// 5. VERIFY. State file is removed last and only on a clean sweep, so a
	//    partial teardown stays discoverable by the next invocation.
	if resid := b.survivors(ctx, proj); len(resid) > 0 {
		rep.Residual = resid
		return rep, &ErrResidual{Project: proj, Items: resid} // -> exit 2, prints manual commands
	}
	_ = os.Remove(b.statePath())
	return rep, nil
}
```

Idempotency falls out: every step treats absence as success, so the second `down` is a no-op returning 0. `down` with no state file and no containers is also a no-op returning 0.

### 5.4 Crash recovery, four independent nets

1. **Config-derived project name** (§5.1): `down` works with the state directory deleted.
2. **Intent-first state file.** `harness.json` is written *before* `compose up`, so a crash between "created containers" and "recorded them" is still recoverable. Written `.tmp` → `Sync()` → `os.Rename`. ⚠ **Windows hazard:** `os.Rename` over an existing file fails if *any* process holds the target open. Never keep a read handle open across a write: read fully, close, then write.
3. **Global reaper.** `thesis down --all` lists everything with an `io.prothesis.project` label plus `docker compose ls --all --format json` filtered to `thesis-*`, and reaps anything older than `harness.stale_ttl` (default 4h) or belonging to a run that is not the current one. `thesis up` runs the *report* half automatically and prints a one-line warning: noticing 40 orphaned containers before you start is worth the one extra `docker ps`.
4. **State lock without `flock`.** `.prothesis/state/harness.lock` created `O_CREATE|O_EXCL` holding `{pid, run_id, created_at}`. ⚠ **Do not try to detect liveness from the PID**: `os.FindProcess` on Windows succeeds for any PID and PIDs are recycled aggressively. **Derive liveness from Docker instead:** the lock is stale if `created_at` exceeds the TTL *or* no container carries `io.prothesis.run=<run_id>`. Breaking a stale lock logs a warning. This sidesteps all cross-platform PID misery.
   ⚠ **Naming collision to avoid:** `internal/lock` in the fixed layout is the *oracle* lock (invariant I6, exit code 4); a completely different concept. The harness lock lives in `internal/harness/statelock.go` and is never called "the lock file." → `OPEN_QUESTIONS` #6.

### 5.5 Signals

⚠ **Windows hazards, all four are real:**

- **`SIGTERM` does not exist on Windows.** `syscall.SIGTERM` compiles and is never delivered. Only `os.Interrupt` (Ctrl+C) arrives.
- **`CTRL_CLOSE_EVENT` gives ~5 seconds, non-extendable.** Closing the console window kills the process shortly after. So cleanup must be *fast*: the handler's first action is `docker compose down --timeout 5` (not the default 30), and it prints the manual `thesis down` command **immediately, before** starting cleanup, so the user has it even if the process dies mid-sweep.
- **Ctrl+C goes to the whole console process group**: including the `docker.exe` children the handler just spawned, which then die mid-`down`. Fix: spawn cleanup subprocesses in a new process group.
  ```go
  //go:build windows
  func newProcessGroup(c *exec.Cmd) {
      c.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x00000200} // CREATE_NEW_PROCESS_GROUP
  }
  ```
  (`syscall` is stdlib; a `//go:build !windows` twin is a no-op. No `golang.org/x/sys`.)
- **Second Ctrl+C aborts immediately**, prints the exact `thesis down --project <p>` line, and exits 2. Never trap the user in a cleanup they cannot escape.

Signals are a *convenience*. §5.2's labels are the guarantee.

### 5.6 `--no-teardown` versus `--keep-up`

Different flags, different intent, and conflating them produces silently corrupt subsequent runs.

| | `--no-teardown` (global) | `--keep-up` (`replay`, `up`) |
|---|---|---|
| Artifacts collected | **yes** | yes |
| HEAL runs (faults withdrawn) | **no: faults stay injected** | **yes** |
| Driver stopped | yes | yes |
| Containers destroyed | no | no |
| State file | kept, `leaked: true` | kept, `retained: true` |
| Intent | inspect the broken state | leave a clean cluster to poke at |
| Exit code | unchanged | unchanged |

Both leave TEARDOWN's *collect* half running: a verdict without artifacts is useless, and skipping collection is never what anyone means.

⚠ **Safety interlock: this one matters.** `--no-teardown` leaves faults injected. The *next* `up`/`run` on that project must **refuse to start** (exit 2) unless `--force-recreate`, detected via `residual_faults` in the state file. Starting a fresh world on top of an unrecorded residual partition produces a world whose recorded fault schedule does not describe what actually happened: a false failure at best, and at worst a *false pass* if the residual fault masks the bug. That is the anti-gaming rule ("never make the gate weaker to make it pass") applied to the harness.

Phase 0 note: `thesis up` is permanently `--keep-up`; leaving the topology running *is its job*. It writes the state file and returns.

---

## 6. Package API

### 6.1 `Backend`: implementable by compose, process, k8s, sim

```go
package harness

type Backend interface {
	Name() string
	Capabilities() Caps

	Preflight(ctx context.Context) (*EnvInfo, error)
	Up(ctx context.Context, o UpOptions) (*Topology, error)
	Down(ctx context.Context, o DownOptions) (*DownReport, error)
	Topology(ctx context.Context) (*Topology, error) // discover, never mutate

	Resolve(ctx context.Context, id NodeID) (Binding, error)
	Validate(b Binding) error // ErrStaleBinding

	// Phase 1+
	Exec(ctx context.Context, id NodeID, s ExecSpec) (ExecResult, error)
	Logs(ctx context.Context, s LogSpec) (io.ReadCloser, error)
	Stats(ctx context.Context, id NodeID) (Stats, error)

	// Phase 2 — proc.* family
	Signal(ctx context.Context, id NodeID, sig string) error       // proc.kill
	Freeze(ctx context.Context, id NodeID) error                   // proc.pause
	Thaw(ctx context.Context, id NodeID) error
	Restart(ctx context.Context, id NodeID, o RestartOptions) (Binding, error)

	Network() NetworkProvider
	Close() error
}
```

### 6.2 Capability negotiation: never silently narrow the fault space

This is how "a `process` backend on Windows cannot do `proc.pause` or `net.partition`" is handled without lying in a verdict.

```go
type Caps uint64
const (
	CapExec Caps = 1 << iota
	CapSignal
	CapFreeze      // proc.pause
	CapRestart
	CapNetPolicy   // net.*
	CapClockSkew
	CapIOFault
	CapStats
	CapLogs
)

type ErrUnsupported struct{ Backend, Op, Reason string }
func (e *ErrUnsupported) Error() string {
	return fmt.Sprintf("harness: %s backend cannot %s: %s", e.Backend, e.Op, e.Reason)
}
```

| Backend / platform | Freeze | NetPolicy | Note |
|---|---|---|---|
| compose (Docker Desktop, Linux VM) | ✅ `docker pause` | ✅ ambassadors | full v1 matrix |
| process / linux | ✅ SIGSTOP | ✅ ambassadors | |
| **process / windows** | ❌ | ✅ ambassadors | ⚠ no SIGSTOP equivalent; `SuspendThread` is not exposed and does not compose with Go runtime threads |
| sim | ✅ | ✅ | Tier A, v4 |

**The rule: a capability gap that intersects `perturber.allow` is a hard error at plan time (exit 2 INCONCLUSIVE) not a silent skip at inject time.** Quietly dropping `proc.pause` and reporting PASS would be indistinguishable from a fault-space-floor violation (addendum E.2). If the operator genuinely accepts the reduced space, they waive it explicitly, and the waiver is recorded in the verdict and covered by the oracle lock.

### 6.3 Target selector: shared by health probes and faults

Both `harness.health[].node` (`"kv:*"`) and the fault grammar's TARGET use the same language, so it is resolved once, in the package that owns the node table.

```go
// internal/harness/target.go
//   n1              exact node
//   kv:*            all nodes whose Service == "kv"   (System nodes never match)
//   role:leader     dynamic, via RoleResolver
//   n1<->n2         edge (both directions)
//   n1->n2          directed edge
//   minority(kv)    ⌊(n-1)/2⌋ nodes, chosen from RECORDER — recorded as ResolvedTarget
//   majority(kv)    ⌈(n+1)/2⌉ nodes
type Selector string
func (s Selector) Resolve(ctx context.Context, t *Topology, rr RoleResolver, rnd RandSource) (ResolvedTarget, error)
```

Dependency direction: `pkg/schema` ← `internal/harness` ← {`internal/perturber`, `internal/driver`, `internal/control`}. `harness` imports no other `internal` package; `RandSource` and `RoleResolver` are injected, so `recorder` can depend on `harness` types without a cycle.

### 6.4 Docker CLI wrapper

```go
// internal/harness/docker.go
type dockerCLI struct {
	bin     string   // absolute, resolved ONCE via exec.LookPath
	env     []string
	timeout time.Duration
}

func newDockerCLI() (*dockerCLI, error) {
	p, err := exec.LookPath("docker")
	if err != nil { return nil, &ErrEnv{Msg: "docker not found on PATH", Err: err} }
	// ⚠ Windows: Go >=1.19 refuses to exec .bat/.cmd with unquotable args
	// (CVE-2024-24576 hardening). Refuse up front with a clear message
	// rather than failing later inside a fault injection.
	if ext := strings.ToLower(filepath.Ext(p)); ext == ".bat" || ext == ".cmd" {
		return nil, &ErrEnv{Msg: "docker resolves to " + p + "; a .exe is required"}
	}
	abs, _ := filepath.Abs(p)
	return &dockerCLI{bin: abs, timeout: 60 * time.Second}, nil
}

// run takes argv as a SLICE. There is no shell anywhere in this package,
// which is why "C:\AI Projects\Pro-synthesis" needs no quoting anywhere.
func (d *dockerCLI) run(ctx context.Context, args ...string) (stdout, stderr []byte, err error)
```

Notes: `exec.CommandContext` everywhere so budget cancellation propagates (invariant I7). Every argv, exit code, and stderr tail is recorded to `.prothesis/runs/<id>/harness.jsonl`, when a run wedges, the artifact contains the literal command to paste. `exec.LookPath` in modern Go no longer resolves relative to cwd (`ErrDot`), which is the behavior we want.

### 6.5 Compose JSON decoding

```go
// decodeComposePS tolerates BOTH shapes of `compose ps --format json`
// (a JSON array in newer Compose, newline-delimited objects in older).
// Sniffing the first byte costs nothing and removes the version question.
func decodeComposePS(b []byte) ([]composePS, error) {
	t := bytes.TrimLeft(b, " \t\r\n")
	if len(t) == 0 { return nil, nil }
	if t[0] == '[' {
		var out []composePS
		return out, json.Unmarshal(t, &out)
	}
	var out []composePS
	dec := json.NewDecoder(bytes.NewReader(t))
	for {
		var one composePS
		if err := dec.Decode(&one); err == io.EOF { break } else if err != nil { return nil, err }
		out = append(out, one)
	}
	return out, nil
}

type composePS struct {
	ID, Name, Service, State, Health string
	ExitCode   int
	Publishers []struct {
		URL, Protocol string
		TargetPort, PublishedPort int
	}
}
```

Unknown fields are ignored by default (`encoding/json`), so new Compose fields never break us. Missing fields we *need* fall through to `docker inspect`, whose schema is far older and far more stable. Field names above are drawn from the Compose v2 `ps` output shape and are **unverified here (the daemon is stopped**) so implementation must confirm them against a live `docker compose ps --format json` and the fallback path must be exercised in a test, not assumed.

---

## 7. Windows hazard register

Consolidated. Every one of these is either verified on this machine or a known Docker-Desktop-on-Windows behavior that silently produces wrong results rather than an error.

| # | Hazard | Symptom if ignored | Mitigation |
|---|---|---|---|
| W1 | SDK ignores docker contexts (⚠ verified: no `DOCKER_HOST`, context `desktop-linux`) | inspects the wrong daemon; "container not found" | CLI only (§1) |
| W2 | Git Bash forks fail (⚠ verified) | `steady.sh` cannot run at all | run helpers in a container (§3.4) |
| W3 | No route from Windows to the compose bridge network | health probes time out against container IPs | publish ports; `{host}`=`127.0.0.1` (§3.1) |
| W4 | `localhost` resolves `::1` first; Desktop's forwarder binds v4/v6 inconsistently | intermittent, unexplainable `connection refused` | hard-code `127.0.0.1` |
| W5 | Desktop's forwarder accepts TCP before the container listens | connect-only probe reports a dead container healthy | full HTTP GET, require 2xx |
| W6 | WinNAT reserves port ranges (⚠ verified 50000-50059, 62590-63523) | bind fails as *permission denied*, not *in use* | base 18080/19100; error text names `netsh …excludedportrange` |
| W7 | `0.0.0.0` bind triggers a Firewall prompt | blocking GUI dialog; hangs CI | bind `127.0.0.1` only |
| W8 | CRLF in `.sh` | `bad interpreter: /bin/sh^M` | `.gitattributes` + preflight CRLF scan + `COPY` not mount |
| W9 | No `+x` bit through a Windows bind mount | `permission denied` on `./steady.sh` | invoke `/bin/sh script.sh` |
| W10 | **Bind mount from a non-shared/UNC drive yields an empty dir, no error** | "file not found" blamed on the fixture | bake helpers with `COPY`; mount is opt-in |
| W11 | `SIGTERM` never delivered on Windows | teardown handler never runs | handle `os.Interrupt`; rely on labels |
| W12 | `CTRL_CLOSE_EVENT` allows ~5s | teardown truncated | `--timeout 5`; print manual command first |
| W13 | Ctrl+C hits the whole console group, killing `docker.exe` mid-`down` | partial teardown, orphans | `CREATE_NEW_PROCESS_GROUP` |
| W14 | `os.Rename` fails when the target is open | state file write fails intermittently | read-close-then-write; `.tmp`+`Sync`+`Rename` |
| W15 | `os.FindProcess` succeeds for any PID; PIDs recycle | stale lock never broken, or a live run's lock stolen | derive liveness from Docker labels + TTL |
| W16 | Path case/separator variance (`C:\x` vs `c:/x`) | same dir → two project names → orphans | `EvalSymlinks`+`Clean`+`ToSlash`+lower drive |
| W17 | Space in project path (⚠ verified: `C:\AI Projects\…`) | broken quoting if commands are built as strings | argv slices only; no shell |
| W18 | `MAX_PATH` 260 | artifact writes fail deep in a run | short artifact paths; verify long-path support in `doctor` |
| W19 | `docker` may resolve to `.bat`/`.cmd` | Go refuses to exec with args, late and cryptically | reject at `newDockerCLI` |
| W20 | Windows-container mode active | compose deploys Linux images to a Windows engine | assert `.Server.Os == "linux"` in preflight |
| W21 | `docker exec` without `-T` allocates a TTY | control chars corrupt captured stdout | always `exec -T` |
| W22 | `Binding.PID` is a VM PID, not a Windows PID | host-side process operations target the wrong thing | documented on the field; never used host-side |

---

## 8. Phase 0 scope and definition of done

Phase 0 ships: `harness.go`, `project.go`, `statelock.go`, `docker.go`, `compose_backend.go`, `compose_cli.go`, `compose_overlay.go`, `compose_reap.go`, `probe.go`, `execplan.go`, `target.go` (static selectors only), `net.go`, `exec_windows.go`/`exec_unix.go`, plus `net_proxy.go` and `net_tc.go` as stubs returning `ErrUnsupported`, and the `Signal`/`Freeze`/`Thaw`/`Restart` methods likewise. The tree always compiles and every stub returns a typed, actionable error.

**DoD (a): `thesis up && thesis down` boots and destroys the fixture cleanly.** Covered by §1.5 and §5.3. "Cleanly" is defined operationally: after `down`, `docker ps -aq --filter label=io.prothesis.project` and the corresponding network/volume queries all return empty, verified *by the tool*, and a nonempty result is exit 2 rather than a silent success.

**DoD (b): byte-identical `.thesis` round-trip.** Harness's contribution, for the recorder's designer:

- Per invariant I2 the world tuple is `(seed, topology_variant, driver_profile, fault_schedule, phase_timings)`. **Observed `Binding`s are not part of it**: container IDs and PIDs change every run and would make byte-identity impossible by construction. Bindings belong in `.prothesis/runs/<id>/topology.json`, an artifact, not the world.
- **`ResolvedTarget` *is* part of the world** (§2.7): replay needs it. So it must serialize canonically: `Nodes` sorted, no maps, times as `int64` ns not RFC3339, explicit `omitempty` discipline, and an encoder with `SetEscapeHTML(false)`.
- `Topology.Nodes` is a slice sorted by `ID`, never a map, for the same reason.

Everything sits under the fixed §3 layout; `internal/harness` stays flat with file-name prefixes rather than subpackages, so no new directories are introduced beyond `testdata/kvfixture/…` and `thesis-helpers/`.

