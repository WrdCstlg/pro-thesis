# 04-fixture.design

## Summary

The kvfixture is a hand-written ~900-line Raft (Figure 2 minus snapshots and membership change) in a nested Go module under testdata/kvfixture, with exactly one injected defect: the leader's read lease is expressed in raft ticks rather than elapsed real time. Because a SIGSTOPped or cgroup-frozen process's tick counter stops while its peers' keep running, the frozen ex-leader wakes up still believing it holds a lease and serves a stale local read: the anomaly fires deterministically and independently of pause duration, which a monotonic-clock lease would not. Peer traffic runs over a userspace HTTP ambassador ("mesh") on an internal Docker network so net.partition cuts replication without cutting client or health-probe reachability (a true gray failure), while client ports stay published to the Windows host for the host-side loadgen. A dependency-free provebug binary proves the anomaly end-to-end without PRO-THESIS and emits a golden history.jsonl plus witness.json that Phase 3's consistency oracle can be developed against with zero Docker.

## Decisions (18)

### Hand-written Raft over hashicorp/raft or etcd/raft

**Choice:** Implement ~900 lines of Raft from scratch in testdata/kvfixture/internal/raft: Figure 2 in full, minus snapshots and minus membership changes.

**Rationale:** The fixture's entire value is that it has EXACTLY ONE known defect (spec §5: 'without a system with a known bug, there is no way to tell whether PRO-THESIS works'). A vendored library makes any library-side timing quirk unattributable, which would undermine every Phase 3/4/5 Definition of Done, all of which are stated as 'finds the bug'. Hand-rolled also gives the full observability tuple (role, term, commit_index, queue_depth, connection_count, lag_ms, goroutines, fds, rss, tick-vs-monotonic) from one /status handler, gives seeded election-timeout randomization for Tier B replay fidelity, keeps the fixture module at zero third-party dependencies, and puts the defect on a diff-able line in a file we own, which is what spec §4.6's suspect.files, Phase 5's `thesis regress`, and Phase 6's agentic-repair DoD all require.

**Rejected:** hashicorp/raft: ~8 transitive deps against an explicit minimize-dependencies directive, no lease-read API (the bug would sit outside the library anyway), Stats() returns map[string]string with no queue depth or connection count, unseeded internal randomization. etcd/raft: genuinely correct and even ships the buggy mode as ReadOnlyLeaseBased + CheckQuorum:false, but still needs ~600 lines of transport/storage/WAL glue, drags in protobuf, and reduces 'the bug' to a config flag rather than a patchable line.

### The lease deadline is measured in raft ticks, not elapsed real time

**Choice:** leaseHeld() renews on majority ACK (the correct rule) but compares r.tick - r.lastAckTick[peer] against LeaseTicks=200. The fix (build tag kvfixed) compares monotonic time against a 300ms LeaseDuration.

**Rationale:** CLOCK_MONOTONIC keeps advancing during SIGSTOP and cgroup freeze, so a monotonic-deadline 5s lease would EXPIRE during the spec's own 6.9s example pause and the bug would never fire; the naive implementation is self-healing by accident. A tick counter driven by a time.Ticker freezes with the process (Go's Ticker buffers one tick and drops the rest, so a 5.8s freeze advances the counter by exactly 1), so the believed remaining lease at resume is ~4725ms regardless of pause length. This makes reproducibility INDEPENDENT of pause duration, which is both what makes the anomaly reliable and what makes Phase 5's binary-search window narrowing well-behaved. It is also a real, credible bug: measuring a timeout in units of your own event loop's progress is the canonical process-pause/lease failure (DDIA ch.8).

**Rejected:** Monotonic deadline refreshed on heartbeat SEND: fires under net.partition but not under a pause longer than the lease. CLOCK_PROCESS_CPUTIME_ID: an idle-but-running process would never expire its lease, which is incoherent, and needs syscall plumbing. A tick counter computed as a monotonic delta: identical to monotonic, removes the bug.

### Two network planes: client on kvclient (published), peer on kvpeer (internal)

**Choice:** Each node runs two HTTP listeners; client plane :8080 on network kvclient (published to the Windows host as 18081/2/3), peer plane :9090 on network kvpeer with internal:true. net.partition cuts only the peer plane.

**Rationale:** This is what makes the fault a GRAY failure in the spec's own sense (§4.3: 'unresponsive to peers, alive to orchestrator'). It is load-bearing, not cosmetic: if a partition also cut the client plane, the ex-leader could not serve a stale read to anybody and the bug would be unobservable. It also keeps health probes and the host-side loadgen working against a partitioned node, which the harness's BOOT polling and the loadgen's info-classification both depend on.

**Rejected:** A single plane: partitions would blind the orchestrator and make the anomaly unobservable. Cutting the client plane too: models full node isolation, a different and less interesting fault.

### Userspace HTTP ambassador ('mesh') instead of tc/netem/iptables

**Choice:** One `mesh` container hosting six directed-edge httputil.ReverseProxy listeners plus an admin API (GET /rules, POST /rules/bulk, DELETE /rules/{id}, DELETE /rules, GET /stats). Nodes only ever dial mesh URLs. Included in docker-compose.yaml from Phase 0 with an empty rule set.

**Rationale:** Requires no NET_ADMIN and no kernel privileges, so it works identically inside Docker Desktop's Linux VM and on any CI host; decisive given tc/iptables are unavailable on this Windows host. withdraw() is an HTTP DELETE and GET /rules lets HEAL mechanically verify zero residual rules, discharging §4.3's 'Critical Guarantee' and Phase 2's DoD. Because the peer transport is HTTP/JSON, message-level fault modelling is the semantically CORRECT model, not a compromise: a byte-splice proxy cannot implement loss/reorder/duplicate coherently, whereas dropping or duplicating a whole RPC is exactly right (both raft RPCs are idempotent; votedFor is persisted). Shipping it in Phase 0 as pass-through means Phase 2 requires no topology re-architecture.

**Rejected:** tc/netem + iptables in container netns: needs NET_ADMIN, is Linux-kernel-version-sensitive, and leaves residual rules that are hard to verify as absent. Six per-edge containers: same fidelity, more moving parts. Per-node egress ambassadors: cannot express a single directed edge fault cleanly.

### net.partition is modelled as a blackhole, not a connection reset

**Choice:** The partition rule stops copying in both directions and holds connections open silently; new dials are accepted and never answered.

**Rationale:** An RST would let the peer fail fast, which is a materially different failure mode: it changes when elections start and how quickly a client discovers a dead leader. A blackhole is the faithful model of an iptables DROP, which is what net.partition is specified to mean, and it keeps the timing budget in §2.2 valid.

**Rejected:** Immediate close/RST: faster to implement, but changes election timing and models 'process gone' rather than 'network cut'.

### Loadgen runs on the Windows host, not in a container

**Choice:** ./bin/loadgen(.exe) is a host-side Go binary talking to 127.0.0.1:18081/2/3 via Docker Desktop's published ports, writing {history_path} directly to a host path the oracle engine reads.

**Rationale:** The normative driver.cmd is a relative host path ('./bin/loadgen'), the oracle engine runs on the host, and containerizing would require bind-mounting a Windows path into the Linux VM (slow, permission-odd) plus supervising an extra container lifecycle. Because partitions live on the internal peer network, a host-side client remains fully able to reach a partitioned node, which is required for the bug to be observable. Host-clock t_ns also matches the harness's virtual clock; the recorder captures a host-to-VM offset once at BOOT from /status wall_unix_ns for the causal timeline.

**Rejected:** Container-hosted loadgen with a bind mount: contradicts the literal ./bin path, adds VM filesystem latency and CRLF/permission hazards. A --in-cluster flag is implemented anyway so containerizing later needs no rewrite.

### ok/fail/info classification: fail requires positive evidence of non-execution

**Choice:** Only connect-refused/DNS/no-route/TLS errors and explicit server responses carrying "applied": false (503 NOT_LEADER, 429, 409 CAS_MISMATCH) are `fail`. Every timeout, every reset after the request write began, and every 5xx without applied:false is `info`.

**Rationale:** CRUCIBLE §M: drivers that cannot distinguish MUST emit info, and a single miscategorised timeout makes every consistency verdict unsound. The frozen-node case is exactly why: a SIGSTOPped node's kernel completes the TCP handshake and buffers the request, so a client timeout genuinely does not know whether the write will be applied on resume. The server's side of the contract is that any error produced before touching the state machine carries applied:false; that flag is the client's only positive evidence. Covered by a table-driven test with one case per row.

**Rejected:** Treating timeouts as fail: simpler and much more precise histories, but unsound; the exact failure mode that silently invalidates a linearizability checker.

### Globally unique write values plus op_id provenance in final_state.json

**Choice:** value = clientIdx*1_000_000 + seq, so every written value is globally unique. Each write's client-generated op_id is stored in the raft entry, and GET /dump returns per-key {value, op_id, index, term} into final_state.json at ASSERT.

**Rationale:** Unique values make every stale read trivially attributable to the exact write that produced it, giving precise witness op_ids. Provenance in final_state.json lets the consistency oracle determine after quiescence which `info` writes actually landed: recovering ground truth without weakening the history's honest `info` classification, since the evidence is gathered post-hoc rather than claimed by the client. Spec §4.4's "value":7 is illustrative, not a schema constraint.

**Rejected:** Small random ints as in the spec example: many-to-one value-to-write mapping, ambiguous witnesses, and a much weaker signal for the oracle.

### Phase markers and graceful shutdown via the driver's stdin

**Choice:** The harness writes newline-delimited JSON to loadgen's stdin ({"cmd":"phase",...} and {"cmd":"stop"}); loadgen injects phase markers in order and on stop/EOF drains, flushes, fsyncs and exits 0.

**Rationale:** Phase markers must interleave with ops in one file (§4.4 shows exactly that), but two writers to one file is a bug factory, and terminating the driver at QUIESCE on Windows (TerminateProcess) would lose the buffered history tail. Stdin works identically on Windows and Linux, needs no extra port or flag, and leaves driver.cmd exactly as the normative template specifies.

**Rejected:** A separate phases.jsonl merged by t_ns at ASSERT: workable but leaves the history non-self-describing and still gives no graceful-stop channel. A loadgen control HTTP port: needs port negotiation and a handshake line on stdout.

### History writer takes t_ns inside the mutex

**Choice:** One bufio.Writer behind one mutex; time.Now().UnixNano() is read inside the critical section immediately before the write.

**Rationale:** Makes the file monotonically nondecreasing in t_ns so append order equals time order, which removes a whole class of checker bugs and makes streaming parsers trivial. The distortion (timestamp is 'when logged' rather than 'when it happened') is microseconds. LF only, no BOM; .gitattributes marks *.jsonl and *.thesis as -text so Windows checkouts cannot corrupt golden files or break Phase 0's byte-identical round-trip requirement.

**Rejected:** Timestamp at op completion then lock: two goroutines can interleave, producing out-of-order t_ns in an append-only file.

### testdata/kvfixture is a nested Go module

**Choice:** testdata/kvfixture/go.mod declares module prothesis.dev/kvfixture with no require block; a root go.work ties the two modules together for editors. Built with `go build -C testdata/kvfixture ./cmd/kv`.

**Rationale:** Two structural reasons. (1) `go build ./...` silently skips any directory named testdata, so a fixture inside the root module would never be vetted, tested or built by any wildcard command and would rot invisibly. (2) I1 says PRO-THESIS runs real, external systems; a module boundary makes it structurally impossible for the fixture to import pkg/schema or for thesis to import the fixture. They share a format, never code: duplicating a 40-line history struct across that boundary is correct, not waste.

**Rejected:** Single module with explicit build paths: works, but leaves the fixture outside all wildcard tooling and permits accidental code sharing that would let the tool cheat. Moving the fixture out of testdata/: violates the spec-fixed §3 layout.

### The fix ships alongside the bug behind build tag kvfixed

**Choice:** lease.go (//go:build !kvfixed) and lease_fixed.go (//go:build kvfixed) are both in the tree from day one. KV_TAGS=kvfixed in docker-compose builds the patched image.

**Rationale:** Phase 5's DoD ('thesis regress passes once the lease bug is patched') and Phase 6's DoD (an agent repairs the bug and verifies green) both need a verified 'after' state. Having it in-tree also gives provebug a negative control (--expect-clean), which guards against a fix that is wrong in the other direction: a lease so conservative the leader never serves a local read at all. PRO-THESIS never reads the build tag.

**Rejected:** No in-tree fix: makes the negative control impossible and leaves Phase 5/6 without a known-good target. Note this creates an anti-gaming hazard, logged as an Open Question: the lock manifest must cover the fixture's build tags and image digest.

### leaseView is published once per heartbeat tick

**Choice:** The raft loop publishes an atomic.Pointer[LeaseView]{Role, Term, CommitIndex, LeaseHeld, LeaderID} on each heartbeat tick only, not on every state transition; the read path does one atomic load.

**Rationale:** A plausible RCU-style hot-path optimization, and a deliberate reliability amplifier recorded as such. After a step-down the read path keeps serving from the stale published view until the next heartbeat publish, converting the sub-millisecond post-SIGCONT race into a 50-100ms window. This is what lets even the spec's literal (racy) example schedule fire at high probability. It is not needed for the canonical schedule in §2.3, which has zero race.

**Rejected:** Publishing on every state transition: correct-looking, but leaves the spec's own example schedule at coin-flip reliability.

### proc.pause is implemented as docker pause, not docker kill -s STOP

**Choice:** Use the cgroup freezer via docker pause/unpause (verified available on this host).

**Rationale:** Atomic across all threads of the process; `docker ps` reports state `paused` so HEAL can mechanically verify zero residual paused containers; `docker unpause` is a trivially correct withdraw(). Both mechanisms leave kernel timers running and collapse to one delivered Go tick on resume, so the lease design is robust to either, but the freezer gives better observability and a cleaner withdraw. Critically, `thesis down` must unpause every paused container BEFORE `docker compose down -v`, because compose down on a paused container hangs.

**Rejected:** docker kill -s STOP: works identically for the bug, but PID-1-scoped, less observable via docker ps, and awkward if init:true is ever set.

### Quorum and wildcard targets resolve over the service-name glob

**Choice:** Compose services are named kv-n1/kv-n2/kv-n3 and mesh. `kv:*`, `minority(kv)` and `majority(kv)` resolve over nodes whose service matches the glob `kv*`. role_hint is static config metadata; `role:leader` resolves dynamically via GET /status.

**Rationale:** Gives the spec's quorum and wildcard target grammar a concrete meaning with no new schema field, and naturally excludes the mesh proxy from quorum sets. Distinguishing static role_hint from dynamic role: targets removes a real ambiguity between §4.2 and §4.3, where all three kv nodes carry role_hint: replica yet role:leader must select exactly one at injection time.

**Rejected:** A single `kv` service with deploy.replicas: 3: generated container names, no per-node env or volumes. Adding a `group:` field to the node schema: the schemas are frozen.

### No Postgres, no log compaction, no admin/compact

**Choice:** The fixture is three kv nodes plus the mesh. The `admin` op class is GET /status, GET /metrics, and POST /admin/noop only.

**Rationale:** §4.2's pg node with role_hint: storage belongs to the sample config for 'my-service'; adding Postgres to the fixture would be pure ballast against an in-memory-plus-WAL KV store and would add a second failure domain the oracles would have to reason about. Log compaction without an InstallSnapshot RPC would break catch-up for a lagging follower: a SECOND bug, which is exactly what a single-known-defect fixture must not have. Bounded workloads (500k ops max, ~50MB WAL) make compaction unnecessary. thesis init's template config still mirrors §4.2 including pg.

**Rejected:** Implementing InstallSnapshot: several hundred more lines of auditable surface for no fixture value. Shipping compaction without it: introduces an unrelated safety bug.

### driver.cmd and steady_state.probe are exec'd directly, not through a shell

**Choice:** Parse with a POSIX-style shell-words splitter, substitute {history_path}/{seed}/{profile}, exec directly. On Windows, fall back from ./bin/loadgen to ./bin/loadgen.exe. The fixture ships cmd/steady (a Go binary) rather than the sample's thesis-helpers/steady.sh.

**Rationale:** Git Bash is broken on this host and cmd.exe/sh -c differ in quoting, globbing and exit-code propagation. Direct exec is portable, deterministic and avoids shell injection through config. A .sh steady probe is simply not executable on a Windows host; a Go binary is, and it can express the real steady-state condition (all nodes healthy AND exactly one leader AND all agree on term and leader_id AND commit_index>0) which no URL probe can.

**Rejected:** sh -c / cmd.exe: not portable here and non-deterministic across shells. An HTTP steady-state probe: cannot express a cluster-wide condition.

### Absolute paths handed to external oracles use forward slashes on Windows

**Choice:** oracle_input.history_path and friends are emitted as C:/AI Projects/... rather than C:\AI Projects\...

**Rationale:** Every Windows API accepts forward slashes, and it removes JSON backslash-escaping hazards for external oracle processes written in Go, Python or anything else. Combined with LF-only output, this keeps the JSON contract byte-identical across platforms.

**Rejected:** Native backslashes: correct but requires every external oracle author to handle backslash escaping correctly.

## Open questions (7)

### [RESOLVABLE-WITH-DECISION] Spec §4.6's example causal timeline is impossible on a 3-node cluster

The timeline shows `proc.pause n2 (leader, term 4)` at 8200 and `partition n3 from {n1,n2}` at 8400, then `n1 elected leader term 5` at 9010. On three nodes, freezing n2 and isolating n3 leaves {n1} alone, not a majority. n1 cannot win an election, so term 5 never exists and no stale read is possible. The two faults must target the SAME node for the schedule to mean anything; the timeline says they target different nodes. (It would be coherent at n=5.)

**Recommendation:** Treat §4.6's causal timeline as illustrative, not normative. Require that when proc.pause(role:leader) and net.partition(minority(kv)) appear in the same schedule, the target resolver CO-RESOLVES them to the same node, and record the resolved node set (not just the target expression) per fault in the .thesis world file, which I2's reproducibility requirement demands anyway. Adopt net.partition(minority(kv))@8000..16000 + proc.pause(role:leader)@8200..14000 as the fixture's documented canonical repro.

### [ACCEPT-AND-DOCUMENT] Spec's example fault windows leave only a race, not a deterministic anomaly

In `proc.pause(role:leader)@8200..15100` overlapping `net.partition(minority(kv))@8400..14900`, the partition is withdrawn 200ms BEFORE the pause ends. During those 200ms the frozen node's kernel completes TCP handshakes and buffers ~2 AppendEntries(term 5). At SIGCONT the raft goroutine and the HTTP read handlers all become runnable simultaneously, so whether a stale read is served before step-down is roughly a coin flip (~30-60% per world). Phase 2's DoD says this schedule 'triggers the stale-read anomaly'; at ~50% it triggers it unreliably.

**Recommendation:** Accept, and mitigate rather than relax. The schedule DOES still fire (partly via the partition half alone) and publishing leaseView once per heartbeat widens the post-resume window to 50-100ms, lifting it to high probability. Document in DECISIONS.md that the spec's named schedule is racy and that the canonical repro requires partition.end >= pause.end + 500ms. Do NOT quietly change the spec's numbers in the Phase 2 test; run both and report the spec schedule's reproduction rate honestly as `reproduced: "k/n"` per CRUCIBLE §C.

### [RESOLVABLE-WITH-DECISION] The in-tree kvfixed build tag is an anti-gaming hole

The fixture ships its own fix behind `-tags kvfixed`, selected by the KV_TAGS build arg in docker-compose.yaml. An autonomous coding agent facing a failing gate could make it pass by setting KV_TAGS=kvfixed instead of fixing anything: exactly the failure mode CRUCIBLE §E ('never make the gate weaker to make it pass') and I6 exist to prevent. Nothing in the current lock-manifest design covers a target system's build configuration.

**Recommendation:** Extend .prothesis/lock to cover the system under test's build inputs, not just oracles and budgets: hash the harness compose file including build args, and record the resolved image digest of every node service in the world file and in the verdict. Flipping KV_TAGS then changes the image digest, the lock recomputation mismatches, and `thesis gate` exits 4 (ORACLE_DRIFT). Worth doing regardless of the fixture: 'the agent rebuilt the SUT differently' is a general gaming vector.

### [ACCEPT-AND-DOCUMENT] The bug is findable with a single fault, weakening Phase 4's benchmark

net.partition(minority(kv))@8000..16000 alone produces a 4.0s stale-read window, because a 5s lease with sub-second elections inherently outlives its own replacement's election. That is what spec §5 asks for (the bug must fire under EITHER pause OR partition), but it means uniform random fault injection will find it readily. Phase 4's DoD requires guided search to 'statistically outperform uniform random fault injection across 10 benchmark trials': hard to demonstrate against a bug a single random fault finds.

**Recommendation:** Accept for the fixture as specified; §5 mandates both triggers and the fixture must not be made harder than specified. Flag now that Phase 4's benchmark will need a SECOND, deliberately harder fixture defect requiring genuine fault overlap (e.g. txn non-atomicity needing proc.pause + net.latency + a specific op interleaving), added out of Phase 0 scope. Do not weaken or complicate the lease bug to manufacture difficulty; add a second bug instead.

### [BLOCKING] proc.pause and net.* are unimplementable on the `process` backend on Windows

Spec §1.6 lists `process` as a v1 backend and §4.3 requires proc.pause (SIGSTOP/SIGCONT) and the full net.* family. Windows has no SIGSTOP, no iptables and no tc. A `process` backend on this host cannot implement proc.pause or net.partition at all. Silently skipping those faults would violate the fault-space floor (CRUCIBLE §E.2) and make a PASS meaningless.

**Recommendation:** For the `process` backend, declare per-fault-kind capability explicitly and return exit 2 (INCONCLUSIVE) with a named unsupported-capability list when a schedule requires an unsupported kind, never silently skip. Phase 0 and Phase 2 target the `compose` backend only, where the Linux VM provides everything. Windows-native process supervision could later use NtSuspendProcess/SuspendThread for proc.pause, but that is out of v1 scope and should be recorded as such rather than half-implemented.

### [RESOLVABLE-WITH-DECISION] History schema has no field for which node served an operation

§4.4's history record fields are t_ns, process, type, f, key, value, error, op_id. But §4.6's verdict requires causal_timeline entries naming nodes ('proc.pause n2 (leader, term 4)'), witness op_ids tied to a specific replica's stale answer, and suspect.files blame over a shrunk window. Without knowing which node served each op, a stale read cannot be attributed and the causal timeline cannot be built.

**Recommendation:** Add a single namespaced optional object `meta` to history records carrying {node, term, commit_index, read_mode, served_by, latency_us, attempt}. The normative top-level field names and semantics are untouched, so §4.2 rule 2 ('do not invent, alter, or improvise field names or semantics') is honoured: this is a namespaced extension, not an alteration. Consistency checkers ignore meta; oracles that build causal timelines read it.

### [ACCEPT-AND-DOCUMENT] driver.profiles has no key-space parameter but checker power depends on it

§4.2 freezes driver profiles as {clients, ops, mix}. A linearizability checker's ability to detect a stale read depends heavily on key density: too many keys and concurrent ops on the same key become rare, so violations go unwitnessed even when they occur. There is nowhere in the normative schema to express it.

**Recommendation:** Do not extend the frozen profile schema. Have loadgen derive the key space deterministically from the profile NAME (smoke: 8, gate: 64, soak: 256) and document the mapping in the fixture README, with a non-normative --keys flag for the provebug demo and experiments. Revisit only if a real workload needs it, at which point it is a schema-version bump, not a silent field addition.

## Risks

- The whole bug rests on Go's time.Ticker COLLAPSING missed ticks (channel buffer 1), so a 5.8s freeze advances the counter by exactly 1. If anyone later replaces the tick loop with a catch-up loop that computes ticks from a monotonic delta, the bug silently vanishes and every downstream phase's Definition of Done becomes untestable. Mitigation: raft_test.go must assert tick advances by <= 2 across a simulated freeze, and the assertion must be commented as load-bearing.
- The spec's own example schedule (pause 8200..15100 outliving partition 8400..14900) is only ~30-60% reproducible, and its causal timeline is impossible on 3 nodes. If Phase 2's DoD is tested literally against that schedule without co-resolution of the two targets and without the leaseView amplifier, it will flake and look like a PRO-THESIS bug rather than a spec bug.
- WAL fsync latency inside Docker Desktop's Linux VM on a named volume can spike into the tens of milliseconds. At a 600ms minimum election timeout that is survivable, but a p99 spike above ~100ms would cause spurious elections that look exactly like injected faults. Mitigation: move fsync off the raft loop into a batching writer goroutine, measure append latency at BOOT, and raise KV_ELECTION_MIN_MS via env if p99 exceeds 100ms.
- Docker Desktop's VM clock jumps when Windows sleeps or hibernates. A multi-second jump fires spurious elections mid-run and would be indistinguishable from a real anomaly. Mitigation: record wall_unix_ns and monotonic_ms per node, and have the harness abort a world as INCONCLUSIVE (exit 2) if any node's wall clock jumps more than 500ms relative to the host.
- `go build ./...` silently skips anything under testdata/, so without the nested module plus an explicit build script the fixture will never be vetted, tested or built by any wildcard command and will rot invisibly between phases. CI must build and test the fixture module explicitly.
- The ok/fail/info classifier is the soundness lynchpin: one miscategorised timeout (especially the frozen-node case, where the kernel accepts and buffers the request) makes every consistency verdict untrustworthy while still looking like it works. It needs a dedicated table-driven test, and it must never be 'simplified' to treat timeouts as fail for cleaner histories.
- `docker compose down` HANGS on a paused container. Once Phase 2 lands, any world that ends mid-pause will wedge teardown and poison every subsequent world. `thesis down` must unpause every paused container in the project and DELETE the mesh rules before `down -v`, and that ordering must exist from Phase 0 rather than be retrofitted.
- The bug is findable with a single fault (partition alone yields a 4.0s stale-read window), which makes Phase 4's 'guided search statistically outperforms uniform random' benchmark weak. A second, deliberately harder fixture defect requiring genuine fault overlap will be needed, and it must be added rather than making the lease bug artificially harder.
- The in-tree kvfixed build tag lets an agent make a failing gate pass by rebuilding the system under test instead of fixing it, which is precisely the anti-gaming scenario I6 and CRUCIBLE §E exist to block. The lock manifest must cover harness build args and record resolved image digests in the world file and verdict, or the whole fail-closed story has a hole in it.
- Under the soak profile (64 clients, 8 hours) Docker Desktop's Windows-to-VM port proxy can exhaust ephemeral ports or drop connections, producing proxy-level errors that are not faults of the system under test. These must be classified `info` and counted separately in telemetry, or they will masquerade as findings.

## Files implied (34)

- `C:\AI Projects\Pro-synthesis\testdata\kvfixture\go.mod`: Nested module `prothesis.dev/kvfixture` with zero requires, so the fixture is structurally isolated from the thesis module and not skipped by `go build ./...` _(Fixture (prerequisite to Phase 0))_
- `C:\AI Projects\Pro-synthesis\testdata\kvfixture\internal\raft\raft.go`: Raft Figure 2 core: single event loop, elections, AppendEntries/RequestVote, step-down on higher term, Figure-8-restricted commit advance, no-op-on-election _(Fixture step 1)_
- `C:\AI Projects\Pro-synthesis\testdata\kvfixture\internal\raft\clock.go` (Clock abstraction exposing Tick() (frozen while the process is frozen) and Monotonic() (never frozen)) their divergence is the mechanism of the bug _(Fixture step 1)_
- `C:\AI Projects\Pro-synthesis\testdata\kvfixture\internal\raft\lease.go`: THE INJECTED DEFECT (//go:build !kvfixed): leaseHeld() compares tick deltas against LeaseTicks=200, so a frozen leader resumes believing it still holds its lease _(Fixture step 1)_
- `C:\AI Projects\Pro-synthesis\testdata\kvfixture\internal\raft\lease_fixed.go`: THE FIX (//go:build kvfixed): monotonic-time lease of 300ms satisfying Lease + drift < ElectionTimeoutMin; the target state for Phase 5 regress and Phase 6 agentic repair _(Fixture step 1)_
- `C:\AI Projects\Pro-synthesis\testdata\kvfixture\internal\raft\log.go`: In-memory raft log over the WAL: append, truncate-conflicting-suffix, TermAt, LastIndex _(Fixture step 1)_
- `C:\AI Projects\Pro-synthesis\testdata\kvfixture\internal\raft\storage.go` (Durable term/votedFor via write-temp+fsync+rename to state.json, plus append-only fsynced wal.jsonl) required so proc.restart cannot introduce a second safety bug _(Fixture step 1)_
- `C:\AI Projects\Pro-synthesis\testdata\kvfixture\internal\raft\transport.go`: HTTP/JSON peer RPC client and server on the peer plane; always dials mesh edge URLs, never peers directly _(Fixture step 1)_
- `C:\AI Projects\Pro-synthesis\testdata\kvfixture\internal\raft\raft_test.go`: Election, replication and Figure-8 commit-rule tests, plus the load-bearing assertion that the tick counter advances by at most 2 across a simulated freeze _(Fixture step 1)_
- `C:\AI Projects\Pro-synthesis\testdata\kvfixture\internal\kv\statemachine.go`: Applied state: map[string]entry{value, opID, index, term} with provenance, backing both local lease reads and GET /dump _(Fixture step 2)_
- `C:\AI Projects\Pro-synthesis\testdata\kvfixture\internal\kv\read_path.go`: The buggy default lease read path (one atomic leaseView load, no quorum round trip) alongside the correct readIndex path _(Fixture step 2)_
- `C:\AI Projects\Pro-synthesis\testdata\kvfixture\internal\kv\write_path.go`: write/cas/txn admission, raft propose, await apply with the KV_APPLY_WAIT_MS ceiling; emits applied:false on every pre-state-machine rejection _(Fixture step 2)_
- `C:\AI Projects\Pro-synthesis\testdata\kvfixture\internal\kv\server.go`: Client-plane HTTP server: /healthz, /readyz, /status (the coverage and role-targeting tuple), /dump, /metrics, ConnState connection counting _(Fixture step 2)_
- `C:\AI Projects\Pro-synthesis\testdata\kvfixture\internal\mesh\proxy.go`: Six directed-edge httputil.ReverseProxy listeners with a per-edge rule pipeline: partition (blackhole), latency, loss, reorder, duplicate, bandwidth _(Fixture step 3)_
- `C:\AI Projects\Pro-synthesis\testdata\kvfixture\internal\mesh\admin.go` (Rule CRUD plus GET /rules and GET /stats) the mechanism by which HEAL verifies zero residual network faults _(Fixture step 3)_
- `C:\AI Projects\Pro-synthesis\testdata\kvfixture\internal\history\writer.go`: JSONL history writer: one mutex, t_ns taken inside the critical section, LF-only, periodic flush and fsync on stop _(Fixture step 6)_
- `C:\AI Projects\Pro-synthesis\testdata\kvfixture\internal\history\classify.go` (The ok/fail/info decision table) the single most soundness-critical function in the fixture _(Fixture step 6)_
- `C:\AI Projects\Pro-synthesis\testdata\kvfixture\internal\history\classify_test.go`: Table-driven test with one case per row of the classification table, including the frozen-node timeout that must be info and never fail _(Fixture step 6)_
- `C:\AI Projects\Pro-synthesis\testdata\kvfixture\cmd\kv\main.go`: Node binary with `serve` and `healthcheck` subcommands; healthcheck-as-subcommand keeps the image free of curl/wget _(Fixture step 2)_
- `C:\AI Projects\Pro-synthesis\testdata\kvfixture\cmd\mesh\main.go`: Ambassador binary with `serve` and `healthcheck` subcommands; parses MESH_EDGES into the six directed-edge listeners _(Fixture step 3)_
- `C:\AI Projects\Pro-synthesis\testdata\kvfixture\cmd\provebug\main.go` (Independent end-to-end demonstration of the stale read with no `thesis` binary involved; emits the golden history, witness and telemetry; --expect-clean is the negative control against the kvfixed build _(Fixture step 5) the gate on starting Phase 0)_
- `C:\AI Projects\Pro-synthesis\testdata\kvfixture\cmd\loadgen\main.go`: Host-side driver: per-client sequential goroutines, seeded PCG streams, unique values, stdin phase-marker and stop control, normative JSONL output _(Fixture step 6)_
- `C:\AI Projects\Pro-synthesis\testdata\kvfixture\cmd\steady\main.go` (steady_state probe binary: asserts all nodes healthy, exactly one leader, agreement on term and leader_id, commit_index > 0) replaces the sample's non-portable thesis-helpers/steady.sh _(Fixture step 7)_
- `C:\AI Projects\Pro-synthesis\testdata\kvfixture\Dockerfile`: Multi-stage build with `kv` and `mesh` targets, a KV_TAGS build arg for the patched variant, and all timing constants as ENV defaults _(Fixture step 4)_
- `C:\AI Projects\Pro-synthesis\testdata\kvfixture\docker-compose.yaml`: Three kv nodes plus the mesh across the kvclient and internal kvpeer networks, restart:"no", per-node volumes, published host ports 18081-18083 and 18090 _(Fixture step 4)_
- `C:\AI Projects\Pro-synthesis\testdata\kvfixture\prothesis.yaml`: The fixture's own prothesis/v1 config consumed by `thesis up`/`down`: nodes, health probes, driver profiles, all six built-in oracles _(Phase 0)_
- `C:\AI Projects\Pro-synthesis\testdata\kvfixture\golden\stale_read_history.jsonl`: A real normative history containing the known violation with known witness op_ids, so Phase 3's consistency oracle can be built and tested with zero Docker _(Fixture step 5 output)_
- `C:\AI Projects\Pro-synthesis\testdata\kvfixture\golden\witness.json` (prothesis.oracle_output/v1-shaped golden file) exactly the artifact PRO-THESIS must independently reproduce in Phase 3 _(Fixture step 5 output)_
- `C:\AI Projects\Pro-synthesis\testdata\kvfixture\golden\telemetry.json`: Captured /status snapshots including the tick-versus-monotonic divergence that is the smoking gun _(Fixture step 5 output)_
- `C:\AI Projects\Pro-synthesis\testdata\kvfixture\scripts\prove_stale_read.ps1`: PowerShell wrapper (Git Bash is broken on this host): compose up --wait, go run ./cmd/provebug, compose down -v _(Fixture step 5)_
- `C:\AI Projects\Pro-synthesis\testdata\kvfixture\scripts\prove_stale_read.sh`: POSIX twin of the PowerShell wrapper for eventual Linux CI _(Fixture step 5)_
- `C:\AI Projects\Pro-synthesis\testdata\kvfixture\README.md`: States the defect precisely, the timing budget, the canonical repro schedule, how to prove it, and how to build the patched variant _(Fixture step 5)_
- `C:\AI Projects\Pro-synthesis\.gitattributes`: Marks *.thesis, *.jsonl and golden files as -text so Windows checkouts cannot inject CRLF and break Phase 0's byte-identical round-trip requirement _(Phase 0)_
- `C:\AI Projects\Pro-synthesis\go.work`: Ties the root module and the nested fixture module together for editor and tooling convenience without letting either import the other _(Phase 0)_

## Design

# PRO-THESIS: `testdata/kvfixture` Design

**Scope note.** The fixture is a *prerequisite*, not a phase deliverable (spec §7.2: "Build `testdata/kvfixture/` first"). It is built whole, before Phase 0's `pkg/schema` / `internal/recorder` / `internal/harness` / `cmd/thesis`. Nothing here implements Phase 1–6 of `thesis` itself; the two places where the fixture must not foreclose later phases (the `mesh` ambassador and the `/status` coverage tuple) are called out explicitly.

---

## 0. The one-paragraph summary of the bug

The leader grants itself a 5-second read lease and serves reads from local state with no quorum round trip. The lease is renewed when a **majority acks** a heartbeat, which is the *correct* renewal rule, but its deadline is measured in **raft ticks**, not in elapsed real time. The author's stated reasoning (in the comment) is that election timeouts are measured in ticks, so the lease should use "the same time base." That is wrong: election timeouts are enforced by the **peers'** ticks, which keep running while this process is frozen; this process's ticks do not. A leader that is `SIGSTOP`ped, cgroup-frozen, hit by a multi-second GC pause, or stopped by a hypervisor wakes up believing it still holds a lease the rest of the cluster stopped honouring seconds ago, and serves a read from a state machine missing every write committed in the new term.

It is *also* too long: `LeaseTicks` (5 s) exceeds `ElectionTimeoutMax` (900 ms) by 5.5×, so even without any freeze a minority-partitioned leader outlives its own replacement's election.

Both halves are needed. The tick base makes `proc.pause` fire; the over-long duration makes `net.partition(minority)` fire. Spec §5 requires both triggers.

---

## 1. Raft implementation: **write it from scratch**

### Recommendation: hand-written, ~900 lines, zero third-party dependencies.

Rejected alternatives and why:

**`hashicorp/raft`**: pulls `go-msgpack`, `go-hclog`, `armon/go-metrics`, `golang-lru` and their transitives, against an explicit directive to minimize third-party dependencies and ship a self-contained static binary. It has no lease-read API at all, so the bug would have to be bolted on outside the library anyway. Its internal randomization is unseeded, so `thesis doctor` (CRUCIBLE §C) would score our own fixture badly on determinism readiness. Decisive objection: it is an opaque blob inside the one artifact whose entire purpose is to have **exactly one** known defect. If hashicorp/raft has any timing quirk of its own, we cannot distinguish "PRO-THESIS found our injected bug" from "PRO-THESIS found a library bug," and every Phase 3/4/5 Definition of Done is stated in terms of finding *the* bug.

**`go.etcd.io/raft/v3`**: genuinely correct, pure state machine, and it even ships the buggy mode as a config flag (`ReadOnlyOption: ReadOnlyLeaseBased` with `CheckQuorum: false`, documented as unsafe). Tempting, but: you still write ~600 lines of transport + storage + WAL glue, so it is not less code; it drags in protobuf; and the defect becomes *a config flag* rather than a diff-able line. Spec §4.6 expects `suspect.files: ["internal/raft/lease.go", "internal/kv/read_path.go"]`, Phase 5's DoD is "`thesis regress` passes once the lease bug in `testdata/kvfixture` is patched," and Phase 6 requires an agent to *repair* the bug. A one-line fix in a file we own is worth far more to those four DoDs than a vendored library.

**Why from-scratch wins on the merits, not just by elimination:**

1. **Ground truth is the product.** Spec §5: "Without a system with a known bug, there is no way to tell whether PRO-THESIS works." That claim only holds if the fixture is auditable end to end. 900 lines is reviewable in an hour against the Raft paper's Figure 2.
2. **Observability is a hard requirement, not a nicety.** Phase 2 needs `role:leader` resolution; Phase 4 needs the state-abstraction tuple `(role, term, bucketed_queue_depth, connection_count)` plus (CRUCIBLE §F) bucketed lag; Phase 1 needs queue depth for `no_unbounded_queue` and goroutines/FDs/RSS for `resource_return_to_baseline`. Hand-rolled gives all of it from one `/status` handler. `hashicorp.Stats()` returns a `map[string]string` with no queue depth or connection count; `etcd.Status()` likewise.
3. **Seeded, injectable nondeterminism.** CRUCIBLE §C (Tier B) requires orchestration nondeterminism to come from RECORDER. Our node takes `KV_SEED` and derives its randomized election timeout from it, so replay fidelity is real rather than aspirational.
4. **Zero dependencies.** The fixture module's `go.mod` has no `require` block at all. `go.sum` does not exist.

### Minimum correct Raft: exactly Figure 2, minus two things

**In scope (non-negotiable for correctness):**

- Persistent state, fsynced before responding to any RPC: `currentTerm`, `votedFor`, `log[]`.
- `RequestVote` with the up-to-date-log restriction (§5.4.1).
- `AppendEntries` with the `prevLogIndex`/`prevLogTerm` consistency check, conflicting-suffix truncation, and `leaderCommit` propagation.
- Randomized election timeouts, seeded per node.
- Step down to follower on **any** higher term observed in any RPC *request or response*.
- Commit advance by majority `matchIndex` **restricted to entries in the leader's current term** (§5.4.2 / Figure 8). Skipping this creates a second, independent safety bug and is the single most commonly botched rule.
- A **no-op entry appended on election** so a new leader can commit prior-term entries. Commits in ~1 RTT (≈5 ms), so a new leader is write-serving ~10 ms after winning.
- `lastApplied → commitIndex` apply loop.

**Deliberately out of scope, each with a reason:**

- **Snapshots / log compaction.** Bounded workloads (max 500 000 ops at soak, ~50 MB WAL). Compacting without `InstallSnapshot` would break catch-up for a lagging follower: i.e. a *second* bug. So the `admin` op class is read-only observability plus `POST /admin/noop`; there is no `admin/compact`.
- **Membership changes.** Fixed 3 nodes.
- **Pre-vote.** Not required; and note that **CheckQuorum's absence is part of the injected bug**, so adding pre-vote's cousin would be actively wrong here.
- **ReadIndex is present but is not the default read path**: it is the correct path the buggy lease path bypasses, and it gives the loadgen a differential comparison (§4).
- **Leadership transfer.**

**Durability matters more than it looks.** Phase 2 requires `proc.restart`. A Raft node that loses `currentTerm`/`votedFor` on restart can double-vote and violate election safety independently of our injected bug. So term/vote go to `state.json` via write-temp+fsync+rename, and the log to an append-only `wal.jsonl` with fsync. ~150 lines. As a bonus this gives `io.latency` / `io.error` faults something real to hit.

### Raft core sketch

```go
// testdata/kvfixture/internal/raft/raft.go
package raft

type Role int32
const (RoleFollower Role = iota; RoleCandidate; RoleLeader)

type Config struct {
	ID               string
	Peers            map[string]string // peerID -> base URL (ALWAYS a mesh edge URL)
	Seed             uint64
	TickInterval     time.Duration // 25ms
	HeartbeatTicks   int           // 4   -> 100ms
	ElectionMinTicks int           // 24  -> 600ms
	ElectionMaxTicks int           // 36  -> 900ms
	LeaseTicks       int           // 200 -> 5000ms   (spec §5: the 5-second lease)
	DataDir          string
}

type Raft struct {
	cfg Config
	mu  sync.Mutex

	// --- persistent (fsynced before any RPC reply) ---
	currentTerm uint64
	votedFor    string
	log         *Log

	// --- volatile ---
	role        Role
	leaderID    string
	commitIndex uint64
	lastApplied uint64

	// --- leader volatile ---
	nextIndex  map[string]uint64
	matchIndex map[string]uint64

	// --- the two clocks. Their divergence IS the bug. ---
	tick             uint64    // advances ONLY when this process runs
	bootMono         time.Time // basis for clock.Monotonic(); advances even while frozen
	electionElapsed  int
	heartbeatElapsed int
	electionTimeout  int // randomized from Seed, in ticks

	// Both recorded on every successful ack. lease.go reads lastAckTick,
	// lease_fixed.go reads lastAckTime. Keeping both avoids build-tagging the struct.
	lastAckTick map[string]uint64
	lastAckTime map[string]time.Time

	leaseView atomic.Pointer[LeaseView] // published to the hot read path
	sm        *kv.StateMachine
	rng       *rand.Rand // math/rand/v2 PCG, seeded from Config.Seed
	inbox     chan message
}

// The single event loop. Every state transition happens here, under mu.
func (r *Raft) run(ctx context.Context) {
	t := time.NewTicker(r.cfg.TickInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			// CRITICAL, LOAD-BEARING: time.Ticker COLLAPSES missed ticks (channel
			// buffer is 1). After a 5.8s freeze this fires ONCE, so r.tick advances
			// by exactly 1. That collapse is what makes the bug deterministic. A
			// catch-up loop computing ticks from a monotonic delta would silently
			// remove it; raft_test.go asserts tick advances by <= 2 across a
			// simulated freeze.
			r.mu.Lock()
			r.tick++
			r.onTick()
			r.mu.Unlock()
		case m := <-r.inbox:
			r.mu.Lock()
			r.step(m)
			r.mu.Unlock()
		}
	}
}

func (r *Raft) onTick() {
	switch r.role {
	case RoleLeader:
		r.heartbeatElapsed++
		if r.heartbeatElapsed >= r.cfg.HeartbeatTicks {
			r.heartbeatElapsed = 0
			r.broadcastAppendEntries()
			r.publishLeaseView() // published ONCE PER HEARTBEAT -- see §3
		}
	default:
		r.electionElapsed++
		if r.electionElapsed >= r.electionTimeout {
			r.becomeCandidate()
		}
	}
	r.applyCommitted()
}

// Figure 8 rule. Omitting the term check here is a second, independent safety bug.
func (r *Raft) maybeAdvanceCommit() {
	for n := r.log.LastIndex(); n > r.commitIndex; n-- {
		if r.log.TermAt(n) != r.currentTerm {
			break // NEVER commit a prior-term entry by counting replicas
		}
		cnt := 1
		for _, p := range r.peerIDs() {
			if r.matchIndex[p] >= n { cnt++ }
		}
		if cnt >= r.quorum() { r.commitIndex = n; return }
	}
}

// Recorded on EVERY successful AppendEntries response, in both time bases.
func (r *Raft) onAppendResponse(peer string, resp appendResp) {
	if resp.Term > r.currentTerm { r.becomeFollower(resp.Term, ""); return }
	if !resp.Success { r.nextIndex[peer]--; return }
	r.lastAckTick[peer] = r.tick
	r.lastAckTime[peer] = r.clock.Monotonic()
	r.matchIndex[peer] = resp.MatchIndex
	r.nextIndex[peer] = resp.MatchIndex + 1
	r.maybeAdvanceCommit()
}
```

### THE INJECTED DEFECT

```go
// testdata/kvfixture/internal/raft/lease.go
//go:build !kvfixed

package raft

// LeaseTicks is the read-lease length, in raft ticks. 200 * 25ms = 5s (spec §5).
const LeaseTicks = 200

// leaseHeld reports whether this leader may serve a read from local state
// without a quorum round trip.
//
// We renew on majority ACK (not on heartbeat send), and we express the deadline
// in raft ticks so the lease shares a time base with the election timeout -- no
// clock-skew or NTP-step worries.
//
// ^^^ THAT REASONING IS THE BUG, and it is wrong in two independent ways:
//
//  (1) TIME BASE. The election timeout that displaces this leader is counted by
//      the PEERS' ticks. Those keep running while this process is frozen
//      (SIGSTOP, cgroup freeze, a multi-second GC pause, a hypervisor
//      stop-the-world). r.tick does not. A frozen leader therefore resumes with
//      its lease arithmetic unchanged and serves reads from a state machine
//      missing every write committed in the new term. Reproducibility is
//      INDEPENDENT OF PAUSE DURATION -- a 60s pause fires as reliably as 1.2s.
//
//  (2) DURATION. Lease safety requires
//          LeaseDuration + maxClockDrift < ElectionTimeoutMin
//      Here 5000ms vs 600ms: violated by 8.3x. So even with a correct time base,
//      a minority-partitioned leader outlives its own replacement's election.
func (r *Raft) leaseHeld() bool {
	if r.role != RoleLeader { return false }
	acks := 1 // self
	for _, id := range r.peerIDs() {
		if r.tick-r.lastAckTick[id] < LeaseTicks { acks++ }
	}
	return acks >= r.quorum()
}
```

```go
// testdata/kvfixture/internal/raft/lease_fixed.go
//go:build kvfixed

package raft

// Lease safety condition: LeaseDuration + maxClockDrift < ElectionTimeoutMin (600ms).
const LeaseDuration = 300 * time.Millisecond

func (r *Raft) leaseHeld() bool {
	if r.role != RoleLeader { return false }
	now := r.clock.Monotonic() // advances during a freeze; that is the point
	acks := 1
	for _, id := range r.peerIDs() {
		if now.Sub(r.lastAckTime[id]) < LeaseDuration { acks++ }
	}
	return acks >= r.quorum()
}
```

Both files are in the tree from day one. `KV_TAGS=kvfixed` builds the patched fixture. This gives Phase 5's `thesis regress` DoD and Phase 6's agentic-repair DoD a ready-made "after" state, **and it is itself an anti-gaming hazard**, see Open Questions.

---

## 2. Making the bug fire reliably: the crux

### 2.1 Why a monotonic-deadline lease would NOT fire

`CLOCK_MONOTONIC` keeps advancing while a process is `SIGSTOP`ped or cgroup-frozen; the kernel counter is system-wide, and only *scheduling* is suspended. So the obvious implementation:

```go
leaseDeadline = time.Now().Add(5 * time.Second) // monotonic reading
```

is *safe against a long pause by accident*: a 6.9 s pause outlives a 5 s deadline, so on resume the first lease check fails, the leader refuses the local read, and **the anomaly never occurs**. Under the spec's own example schedule (`proc.pause(role:leader)@8200..15100` = 6.9 s > 5 s lease) a monotonic lease is *self-healing*. That is exactly the trap this section exists to avoid.

The tick-based lease inverts it. During the freeze the tick counter is frozen, so on resume `r.tick - r.lastAckTick[peer]` is essentially unchanged: Go's `time.Ticker` buffers one tick and drops the rest, so a 5.8 s freeze advances `r.tick` by **exactly 1**. The believed remaining lease at resume is ~4 725 ms regardless of how long the pause was. **Pause duration drops out of the reproducibility argument entirely**, which is both what makes the anomaly reliable and what makes it well-behaved under Phase 5's binary-search window narrowing.

This is not a contrivance. It is the canonical process-pause / lease-expiry failure (Kleppmann, *DDIA* ch. 8, "Process Pauses"), and measuring a timeout in units of your own event loop's progress is a mistake real systems have shipped.

`docker pause` (cgroup freezer, verified available on this host) and `docker kill -s STOP` behave identically for this purpose: both freeze all threads of the thread group, both leave kernel timers running, and both collapse to one delivered tick on resume. The design is robust to either mechanism. **`docker pause` is preferred**: it is atomic across all threads, `docker ps` reports state `paused` so `HEAL` can *verify* zero residual paused containers, and `docker unpause` is a trivially correct `withdraw()`.

### 2.2 Exact timing budget

| Constant | Value | Ticks | Rationale |
|---|---|---|---|
| `KV_TICK_MS` | **25 ms** | 1 | Fine enough that lease/election arithmetic is smooth; coarse enough that the ticker is cheap. |
| `KV_HEARTBEAT_MS` | **100 ms** | 4 | 3 heartbeats (300 ms) fit inside the min election timeout with 2× margin. |
| `KV_ELECTION_MIN_MS` | **600 ms** | 24 | 6 missed heartbeats: immune to VM scheduling jitter and WAL fsync spikes. |
| `KV_ELECTION_MAX_MS` | **900 ms** | 36 | 300 ms randomized spread makes split votes rare. |
| `KV_LEASE_TICKS` | **200 (= 5 000 ms)** | 200 | **Spec-mandated 5-second lease.** |
| `KV_RPC_TIMEOUT_MS` | 250 ms | 10 | Under one heartbeat interval plus slack. |
| `KV_APPLY_WAIT_MS` | 2 000 ms | n/a | Server-side ceiling on a client write awaiting commit; bounds `queue_depth`. |

Derived facts:
- **Correct-lease safety condition:** `Lease + drift < ElectionTimeoutMin` → `5000 < 600`; **violated by 8.3×**. This is the stated bug.
- **Liveness margin:** `3 × Heartbeat = 300 ms < 600 ms` min election timeout.
- **Election completion:** `ElectionTimeoutMax + 2 RTT ≈ 950 ms`; worst case with one split-vote retry ≈ 1.9 s. Comfortably "within a few seconds under partition."
- **Time from fault to a committed new-term write:** election (≤900 ms) + no-op commit (~10 ms) + client write commit (~10 ms) + loadgen scheduling (~10 ms) ≈ **≤ 950 ms**.

### 2.3 Canonical repro schedule (recommended)

```
net.partition(minority(kv))@8000..16000     # isolates the leader on the PEER network only
proc.pause(role:leader)@8200..14000         # docker pause, SAME node
```

The partition **strictly contains** the pause, with a 2 000 ms tail. Walkthrough (t = ms since `DRIVE` start; leader is `kv-n2`, term 4):

| t (ms) | Event |
|---|---|
| ~7 950 | n2's last pre-fault heartbeat is acked by n1 and n3. `lastAckTick[n1] = lastAckTick[n3] = 318`. |
| 8 000 | Mesh applies 4 directed partition rules isolating kv-n2 on `kvpeer`. **Client ports and health probes are untouched.** |
| 8 050, 8 150 | n2 heartbeats; they hang in the mesh. No acks. Lease still held (`tick 324 − 318 = 6 ≪ 200`). |
| **8 200** | `docker pause kv-n2`. Tick frozen at 328. `leaseHeld` = `328 − 318 = 10 < 200` ✔ |
| 8 550 – 8 850 | n1/n3 election timers (last heard 7 950, timeout 600–900 ms) fire → candidate, term 5. |
| ~8 600 – 8 900 | **n1 elected leader, term 5**; appends and commits its no-op. |
| ~8 950 | First client write in term 5 commits: `k/42 = 5000117` (globally unique value). |
| 8 200 – 14 000 | n2 frozen. Loadgen ops to n2 time out → recorded **`info`** (§4.3). |
| **14 000** | `docker unpause kv-n2`. Ticker delivers **one** collapsed tick → `tick = 329`. `leaseHeld` = `329 − 318 = 11 < 200` ✔✔. Role still Leader, term still 4. Believed remaining lease: **(200−11) × 25 ms = 4 725 ms.** |
| 14 000 + ≤25 ms | Raft loop's next tick republishes `leaseView{Leader, term 4, leaseHeld:true}`. |
| **14 000 – 16 000** | **STALE READS.** `GET kv-n2/kv/k/42` → `200 {"value":<term-4 value>,"read_mode":"lease","term":4,"served_by":"kv-n2"}`. The linearizable value is the term-5 write, `ok`-acknowledged to a client at 8 950. **n2 is still partitioned and cannot learn otherwise. Guaranteed window: 2 000 ms. Zero race.** |
| 16 000 | Mesh rules deleted. n1's next `AppendEntries(term 5)` reaches n2 within ≤100 ms. |
| ~16 100 | n2 steps down → follower, term 5, truncates its conflicting term-4 tail, catches up. |
| ~16 200 | `leaseView` republished `{Follower, held:false}`; reads to n2 now `503 NOT_LEADER` → **`fail`**. |
| `HEAL` | Mesh `GET /rules` returns `[]` ✔; no paused containers ✔; all three nodes agree on term 5, leader n1, and `k/42 = 5000117` ✔. |

At the `gate` profile (16 clients, ~⅓ targeting n2, ~40 % reads, sub-millisecond ops) that 2 000 ms window yields **hundreds** of stale reads. The witness is overwhelming, not marginal.

### 2.4 Does the spec's own example schedule work?

Spec §4.6: `proc.pause(role:leader)@8200..15100` overlapping `net.partition(minority(kv))@8400..14900`.

**Case B; the spec's own causal timeline (n2 paused, n3 partitioned): the anomaly is impossible.** On a 3-node cluster, freezing n2 and isolating n3 leaves `{n1}` alone, not a majority. n1 cannot win an election, so term 5 never exists and no stale read is possible. Yet the timeline asserts `9010 n1 elected leader term 5`. **The spec's example causal timeline is internally inconsistent for a 3-node cluster.** (It would be coherent at n = 5.) Logged as an Open Question.

**Case A; `minority(kv)` co-resolves to the same node as `role:leader` (n2):** the anomaly **does** fire, but for a slightly different reason than the timeline claims, and only partly reliably:

- *The election half checks out.* The pause at 8 200 freezes n2; n1/n3 last heard from n2 at ~8 150, so a candidate fires at 8 750–9 050 and **n1 becomes leader at ≈ 8 800–9 100, which matches the spec's own `9010`.** A term-5 write commits by ~9 200.
- *The resume is racy.* The partition is withdrawn at **14 900**, 200 ms *before* the pause ends at **15 100**. During those 200 ms n2 is still frozen but its kernel completes TCP handshakes and buffers ~2 `AppendEntries(term 5)` in the receive queue. At `SIGCONT` the raft goroutine and the HTTP read handlers all become runnable simultaneously, so whether a stale read is served before step-down is a coin flip, roughly 30–60 % per world.

**Verdict: the spec's example schedule produces the anomaly only if (i) `minority(kv)` and `role:leader` co-resolve, and (ii) you accept ~50 % flakiness.** The numbers it needs are **`partition.end ≥ pause.end + 500 ms`**: the partition must strictly contain the pause. Use the canonical schedule in §2.3.

Two mitigations make even Case A fire at high probability, both defensible as realistic engineering rather than fixture-rigging (and both recorded in `DECISIONS.md` as deliberate amplifiers):

1. **`leaseView` is published once per heartbeat tick, not on every state transition.** The hot read path does a single `atomic.Pointer.Load()`: a plausible RCU-style optimization ("publish the read view once per heartbeat"). Consequence: after n2 steps down, the read path keeps serving from the stale published view until the next heartbeat publish, converting a sub-millisecond race into a **50–100 ms window**.
2. **Loadgen clients keep hammering the frozen node.** A client that times out on n2 emits `info` and *retries against n2* rather than abandoning it, so n2's accept backlog always holds pending reads at resume.

### 2.5 Minimum fault set (shrink targets for Phase 5)

- **Single fault suffices:** `net.partition(minority(kv))@8000..16000` alone. The isolated leader's `lastAckTick` freezes at 318 while `tick` advances; the lease lapses only at `tick ≥ 518` → t = 12 950. Stale-read window = [~8 950, 12 950] = **4.0 s**. Fires.
- **Two-fault minimum windows:** partition `@8000..9400` + pause `@8100..9200` → a 200 ms post-resume window. So the shrinker should converge to **pause ≈ 1.1 s, partition ≈ 1.4 s**. Concrete targets to validate Phase 5's binary-search narrowing against.
- Phase 5's DoD (≤ 3 faults, ≤ 10 ops) is comfortably reachable: likely 1 fault.

---

## 3. HTTP API

**Two listeners on two networks. This is the most important topology decision in the fixture.**

- **Client plane, port 8080, network `kvclient`, published to the Windows host.** Health probes, orchestrator, loadgen, `provebug`.
- **Peer plane, port 9090, network `kvpeer` (`internal: true`), reachable only through the `mesh` ambassador.**

`net.partition(minority(kv))` cuts **only** the peer plane. That is what makes this a *gray failure* in the spec's own sense (§4.3: "unresponsive to peers, alive to orchestrator") and it is load-bearing: if a partition also cut the client plane, the ex-leader could not serve a stale read to anyone and the bug would be unobservable.

### Client plane

```
GET  /healthz                    -> 200 {"ok":true,"node":"kv-n2"}          liveness
GET  /readyz                     -> 200 once leader known && commit_index>0  readiness
GET  /status                     -> the coverage / targeting tuple (below)
GET  /dump                       -> full SM with provenance (for final_state.json)
GET  /metrics                    -> Prometheus-ish text; /status is canonical
GET  /kv/{key}[?consistency=lease|linearizable]
PUT  /kv/{key}          {"value":N,"op_id":M}
POST /kv/{key}/cas      {"expect":N,"value":M,"op_id":K}
POST /txn               {"op_id":K,"ops":[["r","k/3",null],["w","k/7",N]]}
POST /admin/noop        {"op_id":K}
```

### Peer plane

```
POST /raft/append   AppendEntries
POST /raft/vote     RequestVote
```

HTTP+JSON throughout, per the dependency-free-fixture constraint. It also makes the userspace ambassador *message-aware* rather than a byte splice (§5).

### `GET /status`: the normative observability tuple

```json
{
  "node": "kv-n2", "role": "leader", "role_hint": "replica",
  "term": 4, "leader_id": "kv-n2",
  "commit_index": 1873, "last_applied": 1873, "last_log_index": 1875,
  "queue_depth": 3, "connection_count": 11, "lag_ms": 0,
  "lease": { "held": true, "granted_tick": 318, "tick": 329,
             "remaining_ticks": 189, "remaining_ms_believed": 4725 },
  "clock": { "tick": 329, "tick_interval_ms": 25,
             "monotonic_ms": 15100, "wall_unix_ns": 1725300015100000000 },
  "goroutines": 42, "open_fds": 37, "rss_bytes": 18874368, "uptime_ms": 15100
}
```

Every field earns its place:

| Field | Consumer |
|---|---|
| `role`, `term` | `role:leader` target resolution (Phase 2); state-abstraction coverage (Phase 4) |
| `role_hint` | Static label from `prothesis.yaml`. **`role:` targets resolve dynamically; `role_hint` is config metadata.** |
| `commit_index`, `last_applied` | Steady-state probe; convergence oracles |
| `queue_depth` | `no_unbounded_queue` (Phase 1); bucketed for coverage: `0, 1, 2-3, 4-7, 8-15, 16+` |
| `connection_count` | State abstraction (Phase 4); tracked via `http.Server.ConnState` |
| `lag_ms` | Bucketed lag, CRUCIBLE §F coverage tuple |
| `goroutines`, `open_fds`, `rss_bytes` | `resource_return_to_baseline` (Phase 1) |
| **`clock.tick` vs `clock.monotonic_ms`** | **The smoking gun.** Their divergence is a first-class coverage dimension and exactly the signal a `differential`-class oracle needs. |
| `wall_unix_ns` | Host↔VM clock-offset calibration at `BOOT`; VM clock-jump detection |

`role:leader` resolution = `GET /status` on all kv nodes; pick `role=="leader"` with the highest `term`. If two nodes claim leader **in the same term**, the resolution fails loudly: that is itself a finding.

While a node is frozen, `/status` times out. The sampler must record that as a distinct tuple `role:"unreachable"` rather than dropping the sample: an unreachable node is a real, coverage-worthy global state.

### The read path: the second half of the defect

```go
// testdata/kvfixture/internal/kv/read_path.go
func (s *Server) handleRead(w http.ResponseWriter, req *http.Request) {
	key := pathKey(req)
	mode := req.URL.Query().Get("consistency")
	if mode == "" { mode = "lease" } // DEFAULT is the fast path -> the bug is reachable
	                                 // by an ordinary client that asks for nothing special

	// Single atomic load. Published by the raft loop once per heartbeat tick.
	view := s.leaseView.Load()

	if mode == "lease" && view.Role == raft.RoleLeader && view.LeaseHeld {
		v, found, prov := s.sm.GetLocal(key) // NO quorum round trip
		writeJSON(w, 200, readResp{
			Key: key, Value: v, Found: found, ServedBy: s.id,
			Term: view.Term, CommitIndex: view.CommitIndex,
			ReadMode: "lease", Provenance: prov, Applied: true,
		})
		return
	}
	if view.Role != raft.RoleLeader {
		// Positive evidence of non-execution -> client MUST record `fail`.
		writeJSON(w, 503, errResp{Code: "NOT_LEADER",
			LeaderHint: view.LeaderID, Applied: false})
		return
	}
	s.readIndexRead(w, req, key) // correct path: commitIndex + majority heartbeat round
}
```

**Error-body contract (soundness-critical, §4.3).** Every error the server produces **before touching the state machine** carries `"applied": false`. That is the client's only positive evidence for classifying an op as `fail`. Anything else is `info`.

---

## 4. Loadgen

### 4.1 Host or container?: **Host.**

`driver.cmd` is `./bin/loadgen --history {history_path} --seed {seed} --profile {profile}`: a relative path executed by the `thesis` process, writing a host path that the host-side oracle engine reads afterwards. Running it in a container would require bind-mounting a Windows path into the Docker Desktop Linux VM (slow, permission-odd) and supervising an extra container lifecycle, and would contradict the literal `./bin/` path.

Docker Desktop publishes container ports to `127.0.0.1` on Windows, so the host loadgen targets `127.0.0.1:18081/18082/18083`. Consequences, all favourable:

- **A partitioned node stays fully reachable by the loadgen**, because partitions live on the internal peer network. Required for the bug to be observable at all.
- `t_ns` timestamps are host-clock, matching the harness's virtual clock. Container logs carry VM time; the recorder captures a host↔VM offset at `BOOT` from one `/status` `wall_unix_ns` reading, for the causal-timeline builder.
- Port-forward overhead ≈ 0.3–1 ms; irrelevant against 600 ms election timeouts.

**Not foreclosed:** an `--in-cluster` flag builds targets from compose service DNS names, so the same binary can be containerized later without a rewrite.

### 4.2 Concurrency model

- **`clients` goroutines, each a Jepsen-style logical process with exactly one op in flight.** The `process` field in the history is the client index. Strict per-client sequentiality is what makes linearizability checking tractable and is assumed by every checker we might wrap (Porcupine, Knossos, Elle).
- **Target selection is per-op, uniform over the 3 nodes**, drawn from the client's own PRNG stream. At `gate` that puts ~5 clients continuously against every node, including the frozen or partitioned one.
- **PRNG:** one `math/rand/v2` PCG per client, seeded `rand.NewPCG(seed, uint64(clientIdx))`. Each client's op sequence is fully deterministic and independent of other clients' timing. Cross-client *interleaving* is not deterministic: that is Tier B, and the verdict must report `reproduced: "3/3"` honestly rather than claim determinism it does not have (CRUCIBLE §C).
- **Values are globally unique:** `value = clientIdx*1_000_000 + seq`. A read returning value V identifies *exactly* which write produced it, making every stale read trivially attributable and the oracle's witness precise. (Spec §4.4's `"value":7` is illustrative.)
- **Key space** is derived from the profile name (`smoke: 8`, `gate: 64`, `soak: 256`) because `driver.profiles` is a frozen schema with no key-space field. Few keys / many ops maximizes conflict density and thus checker power. A non-normative `--keys` override exists for the demo.
- **Read mode split:** 90 % `consistency=lease` (the buggy path, and the default an ordinary client gets), 10 % `consistency=linearizable`, chosen from the client PRNG. Both recorded in `meta.read_mode`, so oracles can partition the history and a `differential`-class oracle gets a clean same-key comparison.
- **Timeouts:** read 500 ms, write 1 000 ms, txn 1 500 ms, connect 200 ms. `http.Transport` with `MaxConnsPerHost = clients`, keep-alives on.
- **Think time:** 0–2 ms per client drawn from its PRNG, so an unthrottled run does not pin the host CPU and distort timings.

### 4.3 ok / fail / info: the soundness lynchpin

CRUCIBLE §M: *"Drivers that cannot distinguish MUST emit `info`."* The governing rule:

> **`fail` requires positive evidence of non-execution. Everything ambiguous is `info`.**

| Observation | Type | Why |
|---|---|---|
| `200` with a parseable body | `ok` | Server confirmed. |
| `409 CAS_MISMATCH` with `applied:false` | `ok` | A *definite* CAS failure is a successful operation with a negative result. |
| `ECONNREFUSED` / DNS failure / no route | `fail` | The request provably never reached the server process. |
| TLS/handshake error | `fail` | Never reached the application. |
| `503 NOT_LEADER` / `NO_LEASE` / `NO_QUORUM` with `applied:false` | `fail` | Server explicitly declined *before* touching the state machine. |
| `429` with `applied:false` | `fail` | Rejected at admission. |
| Deadline exceeded before response headers | **`info`** | May or may not have been applied. |
| Deadline exceeded while reading the body | **`info`** | Applied, response lost. |
| Connection reset **after** the request write began | **`info`** | The server may have read a complete request. |
| `500` / `502` / `504` | **`info`** | Unknown. |
| Any `5xx` **without** `applied:false` | **`info`** | Absence of evidence is not evidence. |

**The frozen-node case is exactly why this matters.** A `SIGSTOP`ped node's *kernel* completes the TCP handshake and buffers the request; the client times out. If the loadgen emitted `fail` there and the request were applied on resume, every consistency verdict built on that history would be unsound. It emits `info`.

A dedicated table-driven unit test covers every row. This is the single highest-consequence 60 lines in the fixture.

**Recovering ground truth without weakening `info`.** Every write carries a client-generated globally unique `op_id`, which the server stores in the raft entry. At `ASSERT`, the harness dumps `GET /dump` from every node into `final_state.json` with per-key provenance `{value, op_id, index, term}`. The consistency oracle can then determine *after the fact* exactly which `info` writes actually landed: recovering ground truth without weakening the history's honest `info` classification, since the evidence is gathered post-hoc rather than claimed by the client. This is a large win in checker precision.

### 4.4 History records

Normative top level exactly as §4.4 specifies. Everything additional goes in a namespaced `meta` object, so no normative field name or semantic is altered:

```json
{"t_ns":1725300000000000,"process":3,"type":"invoke","f":"write","key":"k/42","value":3000042,"op_id":8891}
{"t_ns":1725300000410000,"process":3,"type":"ok","f":"write","key":"k/42","value":3000042,"op_id":8891,
 "meta":{"node":"kv-n2","term":4,"commit_index":1873,"latency_us":410,"attempt":1}}
{"t_ns":1725300014050000,"process":5,"type":"ok","f":"read","key":"k/42","value":3000042,"op_id":9410,
 "meta":{"node":"kv-n2","term":4,"read_mode":"lease","served_by":"kv-n2"}}
{"t_ns":1725300002000000,"type":"info","event":"phase","phase":"HEAL"}
```

Transactions use the Elle rw-register convention, keeping `value` as the normative field name:

```json
{"t_ns":...,"process":3,"type":"invoke","f":"txn","op_id":9001,"value":[["r","k/3",null],["w","k/7",30000123]]}
{"t_ns":...,"process":3,"type":"ok",    "f":"txn","op_id":9001,"value":[["r","k/3",29999001],["w","k/7",30000123]]}
```

**Writer discipline:** one `bufio.Writer` behind one mutex, and **`t_ns` is taken inside the mutex immediately before the write**. The file is then monotonically nondecreasing in `t_ns`, so append order equals time order: trivial to parse, and it removes a whole class of checker bugs. LF only, no BOM, no CRLF (`.gitattributes` marks `*.jsonl` and `*.thesis` as `-text`).

### 4.5 Phase markers and graceful shutdown: **stdin control**

Phase markers must interleave with ops in a single file (§4.4 shows exactly that), but two writers to one file is a bug factory, and killing the driver at `QUIESCE` on Windows (`TerminateProcess`) would lose the buffered tail.

**The harness writes newline-delimited JSON to the driver's stdin:**

```
{"cmd":"phase","phase":"PERTURB","t_ns":1725300008000000000}
{"cmd":"phase","phase":"HEAL","t_ns":1725300016000000000}
{"cmd":"stop"}
```

Loadgen reads stdin in a goroutine, injects phase markers in order, and on `stop` (or stdin EOF) drains in-flight ops, flushes, fsyncs, and exits 0. This works identically on Windows and Linux, needs no extra port or flag, and leaves `driver.cmd` exactly as the normative template specifies.

### 4.6 Flags

```
--history PATH     (normative)   --seed N   (normative)   --profile NAME (normative)
--targets 127.0.0.1:18081,127.0.0.1:18082,127.0.0.1:18083
--clients N --ops N --keys N --duration D
--read-timeout 500ms --write-timeout 1s --txn-timeout 1500ms
--in-cluster        (build targets from compose DNS; unused in v1, keeps the door open)
```

`admin` ops (soak mix) are `GET /status`, `GET /metrics`, and `POST /admin/noop`: recorded as ordinary ops with `f:"admin"`, which consistency checkers filter out. **There is no `admin/compact`**; see §1.

---

## 5. Compose topology

### The `mesh` ambassador

Faults are injected by a **userspace HTTP reverse proxy**, one directed edge at a time, in a single container. Justification:

- **No `NET_ADMIN`, no `tc`, no `iptables`.** Works identically inside Docker Desktop's Linux VM and on any CI host. On this machine `tc`/`iptables` are unavailable on the Windows host and awkward inside containers; this sidesteps that entirely.
- **`withdraw()` is an HTTP DELETE, and `GET /rules` lets `HEAL` *verify* zero residual rules.** That is precisely §4.3's "Critical Guarantee" and Phase 2's DoD, discharged mechanically rather than by hope.
- **Message-level fidelity is the *right* model here, not a compromise.** Our peer transport is HTTP/JSON, so `net.loss` means "drop this RPC," not "corrupt a byte stream mid-splice." A byte-splice proxy cannot implement loss/reorder/duplicate coherently at all. `net.duplicate` is safe because both raft RPCs are idempotent (`votedFor` is persisted).
- Documented limitation: this models **application-message** faults, not L3 packet faults. `tc`-based L3 injection inside containers remains available later if higher fidelity is ever needed.
- One central container hosting all 6 directed edges beats 6 per-edge containers (fewer moving parts) and beats per-node egress ambassadors (an `n1<->n2` edge fault needs exactly one control point).

**`partition` is modelled as a blackhole, not a reset.** An `RST` would let the peer fail fast, which is a *different* failure mode and would change election timing. The mesh's partition rule stops copying in both directions and holds connections open silently: a faithful model of an `iptables DROP`.

Directed-edge port map (mesh listens; forwards to `<dst>:9090`):

| Edge | Mesh port | Edge | Mesh port |
|---|---|---|---|
| n1→n2 | 9112 | n2→n3 | 9123 |
| n1→n3 | 9113 | n3→n1 | 9131 |
| n2→n1 | 9121 | n3→n2 | 9132 |

Admin API on `:9099` (published to host as `18090`):

```
GET    /healthz
GET    /rules                 -> {"rules":[...]}      HEAL residual verification
POST   /rules/bulk            atomic multi-edge (a partition is 4 directed edges)
DELETE /rules/{id}            withdraw one
DELETE /rules                 panic button
GET    /stats                 per-edge {requests,dropped,delayed,bytes}
```

A node **only ever dials mesh URLs** (from `KV_PEERS`). That is a fixture invariant: a node that dialled `kv-n2:9090` directly would silently defeat every partition.

**Phase 0 note:** the `mesh` service ships from day one with an empty rule set (pure pass-through). Phase 0 only boots and health-checks it; nothing calls the admin API until Phase 2. This is the "don't foreclose" move: Phase 2 requires no topology re-architecture.

### `docker-compose.yaml`

```yaml
name: prothesis-kvfixture

networks:
  kvclient: { name: prothesis-kvclient }
  kvpeer:   { name: prothesis-kvpeer, internal: true }

volumes: { n1data: {}, n2data: {}, n3data: {} }

x-kv: &kv
  build:
    context: .
    dockerfile: Dockerfile
    target: kv
    args: { KV_TAGS: "${KV_TAGS:-}" }        # KV_TAGS=kvfixed builds the patched fixture
  image: prothesis/kvfixture:${KV_VARIANT:-buggy}
  restart: "no"                              # auto-restart would mask no_crash. NEVER change.
  init: false                                # PID 1 must be /kv, not tini
  stop_grace_period: 2s
  networks: [kvclient, kvpeer]
  depends_on: { mesh: { condition: service_healthy } }
  healthcheck:
    test: ["CMD", "/kv", "healthcheck", "--url", "http://127.0.0.1:8080/healthz"]
    interval: 1s
    timeout: 2s
    retries: 40
    start_period: 1s

services:
  mesh:
    build: { context: ., dockerfile: Dockerfile, target: mesh }
    image: prothesis/kvmesh:1
    container_name: prothesis-kv-mesh
    restart: "no"
    networks: [kvpeer, kvclient]
    ports: ["18090:9099"]
    environment:
      MESH_ADMIN_ADDR: "0.0.0.0:9099"
      MESH_EDGES: >-
        n1>n2=:9112>kv-n2:9090,n1>n3=:9113>kv-n3:9090,
        n2>n1=:9121>kv-n1:9090,n2>n3=:9123>kv-n3:9090,
        n3>n1=:9131>kv-n1:9090,n3>n2=:9132>kv-n2:9090
    healthcheck:
      test: ["CMD", "/mesh", "healthcheck", "--url", "http://127.0.0.1:9099/healthz"]
      interval: 1s
      timeout: 2s
      retries: 40

  kv-n1:
    <<: *kv
    container_name: prothesis-kv-n1
    hostname: n1
    ports: ["18081:8080"]
    volumes: ["n1data:/var/lib/kv"]
    environment:
      KV_NODE_ID: kv-n1
      KV_PEERS: "kv-n2=http://mesh:9112,kv-n3=http://mesh:9113"
      KV_SEED: "${KV_SEED:-1}"

  kv-n2:
    <<: *kv
    container_name: prothesis-kv-n2
    hostname: n2
    ports: ["18082:8080"]
    volumes: ["n2data:/var/lib/kv"]
    environment:
      KV_NODE_ID: kv-n2
      KV_PEERS: "kv-n1=http://mesh:9121,kv-n3=http://mesh:9123"
      KV_SEED: "${KV_SEED:-2}"

  kv-n3:
    <<: *kv
    container_name: prothesis-kv-n3
    hostname: n3
    ports: ["18083:8080"]
    volumes: ["n3data:/var/lib/kv"]
    environment:
      KV_NODE_ID: kv-n3
      KV_PEERS: "kv-n1=http://mesh:9131,kv-n2=http://mesh:9132"
      KV_SEED: "${KV_SEED:-3}"
```

Notes:
- **YAML merge keys replace whole keys**, so a per-service `environment:` would clobber an anchored one. All defaults (`KV_TICK_MS`, `KV_LEASE_TICKS`, …) live as `ENV` in the Dockerfile; compose sets only the three per-node values.
- **Services are named `kv-n1/2/3`.** `kv:*` and `minority(kv)` / `majority(kv)` resolve over nodes whose *service* matches the glob `kv*`. No new schema field; `mesh` is naturally excluded.
- **Named volumes**, so `down -v` gives every world a clean slate while `proc.restart` preserves the WAL.
- **No Postgres.** §4.2's `pg` node belongs to the sample config for "my-service"; adding it here would be pure ballast. `thesis init`'s *template* still mirrors §4.2.

### `Dockerfile`

```dockerfile
FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod go.su[m] ./
RUN go mod download
COPY . .
ARG KV_TAGS=""
RUN CGO_ENABLED=0 go build -tags "$KV_TAGS" -trimpath -o /out/kv   ./cmd/kv \
 && CGO_ENABLED=0 go build              -trimpath -o /out/mesh ./cmd/mesh

FROM alpine:3.20 AS mesh
COPY --from=build /out/mesh /mesh
ENTRYPOINT ["/mesh","serve"]

FROM alpine:3.20 AS kv
COPY --from=build /out/kv /kv
ENV KV_TICK_MS=25 KV_HEARTBEAT_MS=100 KV_ELECTION_MIN_MS=600 KV_ELECTION_MAX_MS=900 \
    KV_LEASE_TICKS=200 KV_RPC_TIMEOUT_MS=250 KV_APPLY_WAIT_MS=2000 \
    KV_CLIENT_ADDR=0.0.0.0:8080 KV_PEER_ADDR=0.0.0.0:9090 KV_DATA_DIR=/var/lib/kv
EXPOSE 8080 9090
ENTRYPOINT ["/kv","serve"]
```

`/kv healthcheck` and `/mesh healthcheck` are subcommands of the same binary, so the images need no `curl`/`wget`.

### `thesis up` / `thesis down` implications for Phase 0

- **`up`:** ping the Docker daemon first and exit **2 (INCONCLUSIVE)** with a clear message if it is unreachable; the daemon is stopped on this machine right now, so this path will be exercised immediately. Then `docker compose -p prothesis-kvfixture up -d --build`, then poll the configured `harness.health[].probe` from the host, resolving `{host}`/`{port}` via `docker compose port`. Write `.prothesis/state/up.json` so `down` is exact and `up` is idempotent.
- **`down` MUST `docker unpause` every paused container in the project first**, and `DELETE /rules` on the mesh, *before* `docker compose down -v --remove-orphans`. **`docker compose down` on a paused container hangs.** This is fault-residue hygiene appearing in Phase 0, and it is the reason `thesis up && thesis down` would otherwise start flaking the moment Phase 2 lands.

---

## 6. Proving the bug independently

A `PASS` is uninformative unless we know the bug is there. The demonstration is a **Go program, not a shell script**: Git Bash is broken on this host and PowerShell + `curl`/`jq` is fragile.

`testdata/kvfixture/cmd/provebug`: ~10 s, no `thesis` binary involved, no loadgen:

1. Wait for all three `/readyz`; identify leader **L** and followers via `/status`.
2. `PUT /kv/k/42 {"value":7}` on L → `200`, record `op_id W1`, `commit_index C1`. Verify all three report `last_applied ≥ C1`.
3. Assert `L.status.lease.held == true`; record `granted_tick` and `tick`.
4. **Partition L**: `POST mesh /rules/bulk` with the 4 directed edges isolating L. Record `T_p`.
5. **Pause L**: `docker pause prothesis-kv-<L>`. Record `T_s`; assert `T_s − T_p ≤ 300 ms`.
6. Poll the two survivors until one reports `role=="leader" && term > term0`. **Assert within 3 s.** Record L′ and term T5.
7. `PUT /kv/k/42 {"value":9}` on L′ → `200`, `served_by == L′`, `term == T5`, `op_id W2`. Poll L′ until `last_applied ≥ commit(W2)`. **`k/42 = 9` is now committed and acknowledged.**
8. Sleep to `T_s + 5800 ms`; `docker unpause`. **The partition is still in place.**
9. Loop for up to 1 500 ms at 20 ms: `GET 127.0.0.1:18082/kv/k/42`.
   **Assert at least one response is `200 {"value":7,"read_mode":"lease","term":4,"served_by":L}`.**
10. Capture L's `/status` showing `tick × 25 ms ≪ monotonic_ms` (the smoking gun) and print the divergence.
11. Withdraw the partition. Poll until all three agree on `term == T5`, `leader == L′`, and `k/42 == 9`. **Assert convergence within 5 s.** This is what proves the anomaly is *a stale read*, not a permanent split-brain: the system is otherwise healthy and the defect is exactly the injected one.
12. Exit `0` iff the anomaly was observed **and** convergence succeeded.

**The three artifacts it emits are the highest-leverage part of the whole fixture:**

- `golden/witness.json`: a `prothesis.oracle_output/v1`-shaped blob: `{"oracle":"linearizable.kv","class":"consistency","status":"violated","witness":{"op_ids":[W1,W2,R],"key":"k/42"},"explanation":"..."}`. This is *exactly* the artifact PRO-THESIS must independently produce in Phase 3. It is the golden file.
- `golden/stale_read_history.jsonl`: a real, normative history containing a known violation with known witness op_ids. **Phase 3's consistency oracle can be written and unit-tested against this with zero Docker, before the perturber or the loadgen exist.**
- `golden/telemetry.json`: the `/status` snapshots including the tick/monotonic divergence.

**Negative control:** `provebug --expect-clean` against an image built with `KV_TAGS=kvfixed` must observe **no** anomaly and full convergence. That gives Phase 5's "`thesis regress` passes once the lease bug is patched" a ready-made verified "after" state, and it guards against the fix being wrong in the other direction (a lease so conservative the leader never serves a local read at all).

Both are wrapped by `scripts/prove_stale_read.ps1` (PowerShell, host) and a `.sh` twin for eventual Linux CI, each three lines: `docker compose up -d --wait`, `go run ./cmd/provebug`, `docker compose down -v`.

---

## 7. Fixture package layout

`testdata/kvfixture/` is a **nested Go module** with its own `go.mod` and **no `require` block**.

Two reasons, both structural. First, **`go build ./...` silently skips any directory named `testdata`**: a fixture inside the root module would never be vetted, tested, or built by any wildcard command, and would rot invisibly. Second, and more important, **I1 says PRO-THESIS runs *real*, external systems.** A separate module makes it structurally impossible for the fixture to import `pkg/schema` or for `thesis` to import the fixture. They share a *format*, never code. Duplicating a 40-line history-record struct across that boundary is correct, not waste. A root `go.work` ties them together for editor convenience.

```
testdata/kvfixture/
├── go.mod                          # module prothesis.dev/kvfixture — zero requires
├── docker-compose.yaml
├── Dockerfile
├── prothesis.yaml                  # the fixture's own config, consumed by `thesis`
├── README.md                       # what the bug is, how to prove it, how to patch it
├── cmd/
│   ├── kv/main.go                  # node binary: `serve` | `healthcheck`
│   ├── mesh/main.go                # ambassador: `serve` | `healthcheck`
│   ├── loadgen/main.go             # host-side driver -> ./bin/loadgen(.exe)
│   ├── steady/main.go              # steady_state probe -> ./bin/steady(.exe)
│   └── provebug/main.go            # independent bug demonstration
├── internal/
│   ├── raft/
│   │   ├── raft.go                 # Figure 2 core: elections, replication, commit
│   │   ├── log.go                  # in-memory log over the WAL
│   │   ├── storage.go              # state.json (fsync+rename) + wal.jsonl (fsync)
│   │   ├── transport.go            # HTTP/JSON peer RPC client + server
│   │   ├── clock.go                # Clock: Tick() / Monotonic(). Divergence = the bug.
│   │   ├── lease.go                # //go:build !kvfixed   <-- THE BUG
│   │   ├── lease_fixed.go          # //go:build  kvfixed   <-- THE FIX
│   │   └── raft_test.go            # elections, replication, Figure-8 commit rule, and:
│   │                               # tick advances <=2 across a simulated freeze
│   ├── kv/
│   │   ├── statemachine.go         # map[string]entry{value,opID,index,term}
│   │   ├── server.go               # client HTTP plane, /status, /healthz, /readyz, /dump
│   │   ├── read_path.go            # lease read (buggy default) + readIndex read (correct)
│   │   └── write_path.go           # write / cas / txn -> propose -> await apply
│   ├── mesh/
│   │   ├── proxy.go                # per-edge httputil.ReverseProxy + rule pipeline
│   │   └── admin.go                # rule CRUD, /rules listing, /stats
│   └── history/
│       ├── writer.go               # JSONL, timestamp taken inside the mutex
│       ├── classify.go             # the ok/fail/info table  <-- soundness critical
│       └── classify_test.go        # table-driven, one case per row of §4.3
├── golden/
│   ├── stale_read_history.jsonl
│   ├── witness.json
│   └── telemetry.json
└── scripts/
    ├── prove_stale_read.ps1
    └── prove_stale_read.sh
```

**Fixture build order** (each step independently testable): `raft` core + tests → `kv` state machine + HTTP → `mesh` → Dockerfile + compose (`docker compose up` gives a working cluster) → `provebug` (**this is where the bug is proven**) → `loadgen` → `steady`. Only after `provebug` exits 0 does Phase 0 of `thesis` begin.

---

## 8. `testdata/kvfixture/prothesis.yaml`

```yaml
version: prothesis/v1
name: kvfixture
harness:
  backend: compose
  file: docker-compose.yaml
  nodes:
    - { id: kv-n1, service: kv-n1, role_hint: replica }
    - { id: kv-n2, service: kv-n2, role_hint: replica }
    - { id: kv-n3, service: kv-n3, role_hint: replica }
    - { id: mesh,  service: mesh,  role_hint: proxy   }
  health:
    - node: "kv:*"
      probe: "http://{host}:{port}/healthz"
      timeout: 30s
  steady_state:
    probe: "./bin/steady --timeout 60s"
    timeout: 60s
driver:
  cmd: "./bin/loadgen --history {history_path} --seed {seed} --profile {profile}"
  profiles:
    smoke: { clients: 4,  ops: 500,    mix: { read: 0.5, write: 0.5 } }
    gate:  { clients: 16, ops: 20000,  mix: { read: 0.4, write: 0.4, txn: 0.2 } }
    soak:  { clients: 64, ops: 500000, mix: { read: 0.3, write: 0.4, txn: 0.2, admin: 0.1 } }
perturber:
  budget: { max_concurrent_faults: 3, max_faults_per_world: 24 }
  allow: [net.partition, net.latency, net.loss, proc.kill, proc.pause, clock.skew, io.latency]
  deny:  [io.fill]
  constraints:
    - "never partition more than minority of kv"
    - "never fault role_hint:proxy"
oracles:
  dir: .prothesis/oracles
  builtin: [no_crash, no_panic_log, no_unbounded_queue,
            resource_return_to_baseline, availability_after_heal, no_stuck_op]
profiles:
  smoke: { budget: 90s, worlds: 3,  driver_profile: smoke }
  gate:  { budget: 10m, worlds: 30, driver_profile: gate  }
  soak:  { budget: 8h,  worlds: -1, driver_profile: soak, search: true }
artifacts: { dir: .prothesis/runs, retain_passing: 3, retain_failing: all }
```

All six built-in oracles are listed, per CRUCIBLE §B (the on-disk §4.2 sample omits `no_stuck_op` while §6 Phase 1 requires it).

---

## 9. Two harness constraints this design imposes (Phase 2, flagged now)

1. **Fault targets must be resolved once and recorded in the world file.** `role:leader` and `minority(kv)` must **co-resolve to the same node** for the spec's two-fault schedule to mean anything on n = 3. The world file should record, per fault, both the target *expression* and the *resolved node set at injection time*: required for deterministic replay under I2 anyway, and it makes co-targeting explicit rather than accidental.
2. **`driver.cmd` and `steady_state.probe` are parsed with a POSIX-style shell-words splitter and exec'd directly: no `sh -c`, no `cmd.exe`.** Portable, safer, deterministic. On Windows, if `./bin/loadgen` does not exist, try `./bin/loadgen.exe`.

Absolute paths handed to external oracles use **forward slashes even on Windows** (`C:/AI Projects/...`): accepted by every Windows API and free of JSON backslash-escaping hazards.
