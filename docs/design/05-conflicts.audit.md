# 05-conflicts.audit

## Summary

Adversarial audit of the PRO-THESIS spec found 20 real defects: two normative documents that contradict each other and themselves, several requirements that are unimplementable as written, and two Definition-of-Done clauses that are unfalsifiable or physically impossible. The sharpest findings are that the spec's own marquee example (§4.6 causal_timeline) is impossible under Raft quorum arithmetic and the Phase 2 DoD reference schedule cannot trigger the fixture bug; that `clock.skew` via libfaketime cannot work against Go binaries at all; that the frozen `driver.cmd` placeholder set makes Phase 5's op-shrinking unimplementable; and that three of the six mandatory Phase 1 oracles reference tunables the config schema does not contain. I empirically resolved the biggest open architectural question on this machine: tc/netem and iptables inject and withdraw cleanly from a NET_ADMIN sidecar sharing an unmodified container's netns (8.3ms → 311.7ms → 6.7ms), which dissolves the Phase 0/Phase 2 ordering hazard entirely. Measured compose cycle cost is 7.2s, putting a realistic world at ~30s, which makes Phase 4's 30-minute DoD reachable but Phase 5's naive shrink pipeline a 9-52 hour operation.

## Decisions (16)

### D1: `.thesis` is canonical JSON, and "byte-identical" is defined as encoder idempotence

**Choice:** `.thesis` files are canonical JSON: UTF-8, LF line endings, struct-declaration key order (no maps in the type graph), `SetEscapeHTML(false)`, two-space indent, exactly one trailing newline, integers emitted as integers. Phase 0 DoD (b) is tested as `encode(decode(encode(w))) == encode(w)` AND `decode(encode(w))` deep-equals `w`, plus a committed golden `.thesis` fixture. `witness` (free-form per §4.6) is held as `json.RawMessage` and canonicalized on ingest.

**Rationale:** Neither document specifies a wire format for `.thesis`, so Phase 0 must choose one. YAML forecloses byte-identity outright (comment loss, quoting-style normalization, flow-vs-block ambiguity). Go's `json.Marshal` escapes `<`, `>`, `&` by default, which must be disabled for canonicality. Banning `map[string]interface{}` from the decode path is not stylistic: history `t_ns` values are Unix epoch nanoseconds (~1.76e18 in 2026), which exceed float64's exact-integer range of 2^53 ≈ 9.007e15, so any transit through `interface{}` silently corrupts them. The golden file makes the DoD non-self-certifying by catching future encoder drift.

**Rejected:** YAML (cannot round-trip byte-identically); CBOR or protobuf (deterministic but unreadable, and I2's `.thesis` is meant to be a human-inspectable seed artifact); gob (Go-only, defeats addendum §L portability); requiring byte-fidelity for arbitrary input (false for any non-canonical file, and would require the decoder to preserve formatting).

### D2: `.gitattributes` ships in the first commit

**Choice:** Commit `.gitattributes` containing `* text=auto eol=lf`, `*.thesis binary`, `*.sh text eol=lf`, `*.jsonl text eol=lf`, `.prothesis/lock binary` before any `.thesis`, oracle script, or lock file exists.

**Rationale:** Measured on this machine: `core.autocrlf=true` with no `.gitattributes`, and git already emits "LF will be replaced by CRLF the next time Git touches it" on a repo with zero commits. Three things break: (1) `steady.sh` checked out with CRLF gets shebang `#!/bin/sh\r` and fails inside a Linux container with a misleading "no such file or directory"; (2) `.thesis` regression files (which addendum §D requires be committed) lose byte-identity, breaking Phase 0 DoD (b); (3) `.prothesis/lock` digests over oracle scripts change on checkout, producing a false `ORACLE_DRIFT` exit 4; the worst failure mode, because addendum §I instructs agents to halt and escalate and never auto-resolve it. Ordering matters: this must precede the artifacts it protects.

**Rejected:** Setting `core.autocrlf=false` locally (does not travel with the repo, so any collaborator or CI clone reintroduces the bug); normalizing line endings in the codec at read time (hides corruption rather than preventing it, and cannot help the lock digests).

### D3: Network faults use a runtime-attached NET_ADMIN sidecar, not ambassador proxies

**Choice:** `net.*` faults are applied with `tc`/`netem` and `iptables` executed inside the target container's network namespace by a throwaway sidecar launched at fault time via `docker run --rm --network=container:<cid> --cap-add=NET_ADMIN thesis-netadmin`. The SUT image is never modified and the sidecar never appears in the compose file. Partition rules are scoped to peer addresses and the Raft port only, never a blanket DROP.

**Rationale:** Empirically validated on this exact machine: an unmodified alpine target measured 8.347 ms baseline, 311.661 ms after sidecar netem injection, 6.674 ms after withdrawal, ending at `qdisc noqueue`; the residual-free state HEAL must assert. Kernel config confirms `CONFIG_NET_SCH_NETEM=m` and `CONFIG_IP_NF_FILTER=m`, and both inject and withdraw succeeded with only `--cap-add=NET_ADMIN`. This choice satisfies addendum §L ("must work on unmodified systems") because it requires no `iproute2` in the SUT image. Critically, because the sidecar attaches at runtime, Phase 0's compose topology needs no fault-related structure at all: this dissolves the ordering hazard where the fixture topology would otherwise have to anticipate the Phase 2 network-fault decision. Peer-scoped rules are load-bearing: a blanket DROP would also sever the client path, and the lease-violation stale read is only observable if a client can still reach the isolated leader.

**Rejected:** Ambassador/toxiproxy per edge (requires the fixture to dial through proxy addresses, hard-coding the fault mechanism into the Phase 0 topology and violating the unmodified-systems constraint for third-party SUTs); adding `iproute2`+`iptables` to the SUT image (modifies the system under test); a long-lived sidecar declared in compose (works, but adds 3 containers per world to every run including no-fault controls, cutting achievable parallelism for no benefit).

### D4: `proc.pause` is implemented as `docker pause`/`docker unpause`

**Choice:** On the compose backend, `proc.pause` uses the cgroup freezer via `docker pause`, withdrawn with `docker unpause`. Residual verification asserts `.State.Status == running` for every node at HEAL.

**Rationale:** Measured: after `docker pause` the container reports status `paused` while its published TCP port still completes handshakes (the kernel accepts into the listen backlog with no userspace involvement). That is precisely §4.3's stated goal for this fault ("unresponsive to peers, alive to orchestrator") confirmed rather than assumed. Withdrawal is a single, reliable, idempotent command, satisfying §4.3's Critical Guarantee. A necessary consequence documented alongside this: neither the freezer nor SIGSTOP stops CLOCK_MONOTONIC or CLOCK_REALTIME, so a pause longer than the fixture's 5-second lease causes the lease to expire on resume, which is why the spec's own reference schedule fails (see the Q1 open question).

**Rejected:** `docker exec kill -STOP 1` (depends on the image containing a shell and on PID 1 not trapping signals; distroless images have no shell); stopping the container (that is `proc.kill`, a different fault, and it loses the gray-failure property that makes `proc.pause` valuable).

### D5: Probes become a typed union with an execution locus; ports are resolved per-world

**Choice:** `health[].probe` and `steady_state.probe` are modelled as a typed union: `{http: "..."}` or `{exec: {cmd: [...], in: "host"|"node:<id>"}}`, with `in` defaulting to the node. `{host}`/`{port}` are resolved at run time by querying `docker compose port <service> <container_port>` for the world's compose project. The fixture compose file publishes no fixed host ports. The resolved endpoint map is also passed to the driver as `{node_addrs}`.

**Rationale:** Three forcing constraints converge here. (1) Measured: `172.17.0.2:8080` is unreachable from the Windows host while `127.0.0.1:18099` (published) is reachable, so `{host}` cannot mean the container address on Docker Desktop, and this defect is invisible on Linux CI. (2) The node schema `{id, service, role_hint}` binds no port, so `{port}` has no source without runtime resolution. (3) Phase 4 parallelism requires concurrent compose projects, and fixed host ports collide across them. Ephemeral publish plus `docker compose port` solves all three at once. The typed union additionally makes `steady_state: "thesis-helpers/steady.sh"` executable by running it inside a node container, where a POSIX shell actually exists; necessary because Git Bash on this host is broken (verified: `dofork ... exit code 0xC0000142`).

**Rejected:** Adding a static `port:` field to each node (still collides under parallelism); running probes from a sidecar on the compose network (works, but adds a container per probe and still needs port discovery for the driver); requiring the user to publish fixed ports (makes the 30-minute Phase 4 DoD unreachable by forcing sequential execution).

### D6: The KV fixture is built on `hashicorp/raft`, not a hand-rolled Raft

**Choice:** `testdata/kvfixture` uses `hashicorp/raft` with `raft-boltdb` for the log store, and PRO-THESIS authors only the intentionally-unsound leader-lease read path on top. The fixture Dockerfile is multi-stage (`FROM golang:1.22 AS build`) so fixture builds need no host Go.

**Rationale:** The fixture is the measuring instrument for every subsequent phase, so its correctness outside the injected flaw is methodologically load-bearing. A hand-rolled Raft is roughly 1,500-2,500 LOC and will contain its own unintended bugs; every oracle violation would then be ambiguous between "PRO-THESIS found the planted bug" and "the fixture's Raft is broken", which destroys the evidentiary value of Phases 2-5 and makes the Phase 4 benchmark uninterpretable. Building on a battle-tested library reduces the authored surface to the ~600 LOC lease path we actually want to be wrong. Using `raft-boltdb` rather than an in-memory log store is deliberate: it gives `io.latency` and `io.error` a real disk target, without which those required Phase 2 fault kinds have nothing to act on. The "minimize third-party dependencies" guidance in §1.6 governs the tool; the fixture is test data.

**Rejected:** Hand-rolled Raft (ambiguous failure attribution, weeks of work); `etcd-io/raft` (equally sound, but is a bare consensus module requiring us to write transport, storage wiring, and an FSM host; more authored surface, which is the thing being minimized); a non-consensus fixture (the mandated bug in §5 is specifically a Raft lease violation).

### D7: The driver placeholder vocabulary is frozen in Phase 0 and includes `{plan_path}`

**Choice:** `driver.cmd` accepts exactly `{history_path}`, `{seed}`, `{profile}`, `{plan_path}`, `{node_addrs}`. When `{plan_path}` is present the driver executes exactly the JSONL op plan at that path and ignores the profile's op count. The Phase 0 loadgen implements plan-replay mode. Unknown placeholders are a CONFIG_ERROR (exit 5).

**Rationale:** Phase 5 requires `ddmin` over the workload operation history and a shrink to ≤10 ops, which requires re-executing a world with an arbitrary chosen op subset. The §4.2 template offers only `{history_path} {seed} {profile}`; `{seed}` regenerates the whole profile-shaped workload and cannot express a subset. Without this addition Phase 5 is unimplementable, and because `pkg/schema` freezes the driver block in Phase 0 and the loadgen is written in Phase 0, the fix costs hours now and requires reopening a frozen schema later. This is not "inventing field names" under core rule 2: `driver.cmd` is an opaque template string and its placeholder vocabulary is nowhere enumerated as normative, so it is being defined for the first time, and frozen here so it cannot drift. `{node_addrs}` is required regardless by D5.

**Rejected:** Shrinking ops by seed search (the seed-to-op-sequence map is not monotone; there is no seed that yields "the same workload minus op 7"); passing the plan on stdin (collides with nothing today but forecloses drivers that read stdin, and file paths are already the contract's idiom per `{history_path}`); deferring to Phase 5 (forces a frozen-schema reopening and a loadgen rewrite).

### D8: Missing oracle tunables and constraint semantics are added to the schema in Phase 0

**Choice:** Phase 0 adds: `oracles.params` (a map keyed by oracle name, supplying `no_stuck_op`'s SLO ceiling, `resource_return_to_baseline`'s ±N% and resource list, and per-oracle thresholds); a `lifecycle` block carrying the convergence grace window; and a normative mini-grammar for `perturber.constraints` entries. All are included in the lock projection. An unparseable constraint is a CONFIG_ERROR (exit 5), never a warning.

**Rationale:** Three of the six mandatory Phase 1 oracles reference tunables that `prothesis.yaml` §4.2 does not contain; the SLO ceiling, ±N%, and the convergence window. Hardcoding them in Go is specifically wrong rather than merely inelegant: anti-gaming rule 1 requires `.prothesis/lock` to hash "every oracle definition", and an oracle's thresholds are part of its definition. A threshold living in Go source can be weakened by an agent with the lock none the wiser, defeating I6 exactly where it is supposed to bite. Constraints must fail loudly for a parallel reason: `perturber.constraints` are English sentences nothing can evaluate, and silently ignoring them is the "silently relax a requirement" that core rule 4 forbids, while also letting the search wedge the cluster and generate false `availability_after_heal` violations. This lands in Phase 0 because Phase 1 needs the values and Phase 3 hashes the projection.

**Rejected:** Hardcoded constants (invisible to the lock, defeats I6); a free-form `params: map[string]any` (breaks D1's no-interface{} rule and the canonical encoder); treating constraints as documentation-only comments (violates rule 4 and produces a search that generates unusable worlds).

### D9: Shrinking is separately budgeted, never inline in `gate`, and uses staged reduction

**Choice:** Shrinking is invoked as `thesis shrink WORLD --budget 4h`, enabled by default in `search`/nightly and disabled in `gate`. When skipped, the verdict emits `"shrink": {"attempted": false}` with a reason and never a fabricated result. The op-reduction pipeline is staged (reduce client count by binary search, then project to ops touching `witness.key`, then truncate to the fault-to-violation time window, then `ddmin` over the ~40 survivors) rather than raw `ddmin` over 20,000 ops.

**Rationale:** Grounded in the measured 7.19 s compose cycle, a shrink trial costs ~30 s. Tier B determinism (addendum §C) makes reproduction probabilistic, so every ddmin test must be repeated k=3 or a false negative corrupts the search. Raw ddmin over 20,000 ops is then 900-6,000 executions, i.e. 7.5-50 hours, against a `gate` budget of 10 minutes: a 45-300x overrun that also contradicts I7's strict wall-clock budget. Staged reduction exploits structure that ddmin ignores (most ops touch irrelevant keys; most ops fall outside the fault window), collapsing the total to ~2.5-4.5 hours. Emitting `attempted: false` rather than omitting the field matters because §4.6 shows shrink results inline in a verdict, and a consumer must be able to distinguish "not shrunk" from "shrunk to nothing".

**Rejected:** Raw ddmin over the full op list (9-52 hours, per the measured estimate); shrinking inline in `gate` with a truncated budget (produces partially-shrunk worlds that fail the 3/3 confirmation gate, wasting the budget and yielding nothing); skipping op-shrinking entirely (abandons the Phase 5 DoD's ≤10-ops requirement).

### D10: The lock hashes a canonical projection, and mismatches are diffed by direction

**Choice:** `.prothesis/lock` hashes a canonical projection (the allow/deny fault set, `max_concurrent_faults`, `max_faults_per_world`, per-profile budgets and world counts, `oracles.params`, per-oracle-file SHA-256 digests, and the regression file list) not the raw `prothesis.yaml`. Strict equality is retained; on mismatch the tool prints a field-level diff labelling each change `WEAKENING` or `STRENGTHENING`.

**Rationale:** Hashing the whole config file would make any unrelated edit (adding a node, renaming a service, changing a comment) trip `ORACLE_DRIFT` and block ordinary development; a false positive that addendum §I says agents must escalate to a human and never auto-resolve, so the cost per occurrence is high. Strict equality is nonetheless kept rather than replaced by a monotone "only weakening fails" rule, because a monotonicity check is exactly the surface an adversarial agent would probe, and because deciding whether a change is a weakening requires semantics the lock cannot safely infer for oracle scripts. The ergonomic cost (that legitimately strengthening the gate also requires `thesis oracles lock --reason`) is accepted and mitigated by the directional diff, which reduces the mandated human review to seconds.

**Rejected:** Hashing the raw YAML (constant false positives on unrelated edits); allowing strengthening without a lock bump (requires inferring intent, and creates an exploitable asymmetry); signing instead of hashing (addendum §E.5 defers verdict signing to v3).

### D11: I1 binds the system under test only; the test fake is unreachable from any config

**Choice:** I1's mock prohibition is read as scoped to the SUT, per its own wording. PRO-THESIS's own unit tests may use a `harness.Backend` fake, constructed only by an unexported test constructor. There is no `backend: mock` config value; the validator accepts only `compose` and `process` in v1 and rejects `k8s` and `sim` with CONFIG_ERROR (exit 5) while still parsing them for forward compatibility.

**Rationale:** I1 says "strictly forbidden in the system under test", which is textually scoped and does not reach the tool's own test suite. The reading is also forced by necessity: at a measured ~30 s per world, a single ddmin unit test executed against real containers would take hours, so Phases 4 and 5 would have no CI-testable regression coverage at all. The actual risk is not that a fake exists but that it becomes reachable from a real run, letting an agent make the gate pass by repointing `prothesis.yaml`, so the mitigation is unreachability by construction rather than a policy. Rejecting `sim` and `k8s` in v1 reconciles §4.2's backend enum with addendum §K ("Does not require Kubernetes in v1") and §C (Tier A simulation is v4).

**Rejected:** Reading I1 as binding all tests (leaves Phases 4-5 untestable in CI and makes every unit test a 30-second container cycle); exposing the fake as `backend: mock` (creates precisely the gate-bypass surface I6 exists to close); implementing `sim` in v1 (it is scheduled for v4 and would consume the entire budget).

### D12: Unsupported faults return INCONCLUSIVE and are recorded, never silently skipped

**Choice:** A fault kind that cannot be applied on the current backend and platform returns `ErrFaultUnsupported`, which maps to exit code 2 (`INCONCLUSIVE`) and is recorded in the world file and verdict. Known cases: `clock.skew`/`clock.jump` against any unmodified Go SUT, and `proc.pause`/`net.*` on `backend: process` under Windows. `thesis doctor` reports the full capability matrix up front.

**Rationale:** Silently skipping an inapplicable fault narrows the effective fault space, which anti-gaming rule 2 classifies as a lock-level weakening; the tool would be committing the exact offence it exists to detect, and a run that quietly injected 5 of 7 fault kinds would report PASS on reduced coverage. The clock cases are genuinely unsupported rather than merely unimplemented: LD_PRELOAD requires a dynamic loader that CGO_ENABLED=0 Go binaries do not have; Go's runtime calls the vDSO `clock_gettime` directly from assembly, bypassing the libc symbols libfaketime interposes; Linux time namespaces offset only CLOCK_MONOTONIC and CLOCK_BOOTTIME, never CLOCK_REALTIME, which is the clock a lease bug depends on; and Docker does not plumb CLONE_NEWTIME regardless. On the Windows `process` backend there is no SIGSTOP, no iptables, and no tc. Surfacing these via `doctor` before a run turns a mid-run INCONCLUSIVE into an up-front, actionable statement.

**Rejected:** Skipping with a log warning (silently weakens the fault space, and log warnings are invisible to an autonomous agent parsing the verdict); failing the whole run with exit 1 (conflates an environment limitation with an oracle violation, which is exactly what exit 2 exists to separate); auto-substituting a similar fault (fabricates coverage that was never exercised).

### D13: All eight lifecycle phases are emitted, may overlap, and use DRIVE start as the epoch

**Choice:** `prothesis.oracle_input/v1.phases` carries all eight §4.1 phases. Intervals may overlap, and `BOOT`/`SEED` carry negative `start_ms`. The millisecond epoch is `DRIVE` start, as pinned by §4.3. Consumers must not assume the array is disjoint or sorted.

**Rationale:** §4.1 states that `PERTURB` "overlaps `DRIVE`", but §4.5's example array is flat, disjoint, contiguous, and contains only four of the eight phases. As exemplified it cannot represent `PERTURB` at all, and cannot represent `BOOT` or `SEED` because `DRIVE` starts at 0. That defeats I5 ("Every oracle declares the specific lifecycle phases in which it is valid") for half the lifecycle, including the one phase whose exclusion matters most for suppressing false positives: an oracle cannot say "I am invalid during PERTURB" if PERTURB is absent from its input. Keeping the epoch at DRIVE start is not optional, since §4.3 defines fault windows relative to it; negative offsets for the pre-DRIVE phases are the consequence. No field names are added, so core rule 2 is respected.

**Rejected:** Re-basing the epoch to BOOT start (would make all phase offsets positive, but silently changes the meaning of every fault window in §4.3 and every existing `.thesis` file); emitting only the four example phases (leaves I5 unimplementable for BOOT, SEED, PERTURB, TEARDOWN); a separate non-overlapping `perturb_windows` array (invents a field name, which rule 2 forbids).

### D14: `commit` is best-effort, `bisect` requires git, and `thesis` never writes via git

**Choice:** `verdict.commit` is the HEAD of the repository containing `prothesis.yaml`, or `""` when unavailable, never fabricated. `thesis bisect` outside a git repo, or in one with no commits, exits 5 (`CONFIG_ERROR`) with an explicit message. `thesis` never invokes git for writes: it writes `.prothesis/regressions/w_<id>.thesis` and prints the `git add` command for a human.

**Rationale:** Measured: the project has a `.git` directory but `git log` reports "your current branch 'master' does not have any commits yet", so there is no HEAD to record and nothing for `bisect` to traverse. Emitting a placeholder commit would put a false provenance claim into a machine-consumed verdict. Addendum §L additionally requires worlds to be portable across repos, so `commit` is meaningless for a shared `.thesis` and must be optional by design rather than by accident. Declining to auto-commit is a deliberate boundary: addendum §D says shrunk worlds are "committed to the repo" without saying by whom, and having the gate acquire commit authority as a side effect gives an autonomous agent write access to history. Append-only enforcement does not need git anyway: anti-gaming rule 4 is satisfied by the regression file list inside the lock manifest (D10).

**Rejected:** Auto-running `git commit` for regressions (grants the gate write access to history, and would fail on this repo which has no initial commit); falling back to a directory hash when git is absent (produces a value that looks like a commit SHA but is not, misleading every consumer); making `commit` a required non-empty field (unsatisfiable in the current repo state and for cross-repo worlds).

### D15: The go directive is set to `go 1.22` with no toolchain line

**Choice:** Change `go.mod` from `go 1.27.1` to `go 1.22`, and add no `toolchain` directive.

**Rationale:** The brief targets "Go 1.22+" but the on-disk `go.mod` declares `go 1.27.1`, and Go is not yet installed. Under Go 1.21+ toolchain semantics a `go` directive above the installed toolchain either forces an automatic toolchain download (requiring module-proxy network access) or hard-fails under `GOTOOLCHAIN=local`. This breaks the very first `go build` of the project. Pinning the floor at the newest available patch release also forecloses CI on any older runner for no compensating benefit, since nothing in the design needs a post-1.22 language feature.

**Rejected:** Leaving `go 1.27.1` (first build fails or silently downloads a toolchain); pinning an even older floor such as 1.21 (gives up `log/slog` and the 1.22 loop-variable semantics, both of which the codebase will use).

### D16: The fixture ships explicit difficulty presets, pre-registered per Definition of Done

**Choice:** `testdata/kvfixture` takes a `--difficulty=easy|hard` build/run flag controlling lease duration and the probability that a client routes reads to a potentially-stale leader. `easy` is pre-registered for the Phase 2 DoD (reproducible with a single `net.partition(role:leader)`); `hard` is pre-registered for the Phase 4 benchmark (requires two overlapping faults with sub-second timing). The active preset is recorded in the verdict.

**Rationale:** Phase 4's two DoD clauses are controlled by a single hidden variable (how hard the planted bug is to hit) and they pull in opposite directions. At easy difficulty the 30-minute discovery clause passes in a handful of worlds, but uniform random also finds the bug immediately, both arms saturate, and "statistically outperforms uniform random" becomes unachievable for any test. At hard difficulty the statistical clause becomes demonstrable but the 30-minute clause gets tight. Making difficulty an explicit, named, recorded parameter converts an invisible confound into a stated experimental condition, and pre-registering which preset each DoD is judged against prevents the tempting failure mode of retuning the fixture mid-benchmark, which would silently invalidate the trials. This must be a Phase 0 commitment because the fixture is written in Phase 0.

**Rejected:** A single fixed difficulty (makes one of the two Phase 4 DoD clauses unachievable, whichever way it is set); tuning the fixture ad hoc when a DoD fails (invalidates the benchmark and is indistinguishable from gaming the gate); using two separate fixtures (doubles the maintenance surface and makes Phase 2 and Phase 4 results non-comparable).

## Open questions (23)

### [BLOCKING] Q1: The verdict's marquee causal timeline is impossible under Raft quorum arithmetic, and the Phase 2 reference schedule cannot trigger the bug

§4.6 shows `{"t_ms": 8200, ... "proc.pause n2 (leader, term 4)"}`, `{"t_ms": 8400, ... "partition n3 from {n1,n2}"}`, `{"t_ms": 9010, ... "n1 elected leader term 5"}`. With three nodes and quorum 2, n2 is frozen (cannot answer RequestVote) and n3 is unreachable from n1, so n1 can collect exactly one vote and cannot be elected. The Phase 2 DoD inherits the defect: `proc.pause(role:leader)@8200..15100` overlapping `net.partition(minority(kv))@8400..14900` fails under both resolutions. If `minority(kv)` is a non-leader, no election is possible and the cluster wedges. If it is the paused leader itself, an election succeeds but the pause runs 6900 ms against §5's 5-second lease, and since SIGSTOP and the cgroup freezer do not stop CLOCK_MONOTONIC or CLOCK_REALTIME, the lease has expired by resume and no stale read is served. The spec's only concrete reproduction recipe does not reproduce.

**Recommendation:** Restate the Phase 2 DoD in terms of the anomaly class ('a scripted schedule reproduces the fixture's lease-violation stale read') rather than a literal schedule, and replace the reference schedule. Easy preset: single fault `net.partition(role:leader)@8000..14000`, where the isolated leader keeps serving reads on its still-valid local lease while the majority elects and commits; the client path survives because partition rules are peer-scoped (D3). Hard preset for Phase 4: `proc.pause(role:leader)@8000..9200` (1200 ms, deliberately shorter than the 5 s lease) overlapping a partition, long enough to lose leadership but short enough that the lease survives the resume. Correct §4.6's timeline to match or mark it explicitly non-normative.

### [RESOLVABLE-WITH-DECISION] Q2: The CRUCIBLE addendum misreports the on-disk document and contradicts itself on oracle count

Addendum §B states 'CRUCIBLE §3.4 requires SEVEN, the on-disk sample config lists five' and 'The on-disk §4.2 sample YAML omits `no_stuck_op` from `oracles.builtin`'. All three claims are false. Verified against the file: §4.2 lists six builtins including `no_stuck_op`, exactly matching §6 Phase 1's 'Implement all 6 built-in generic oracles'. The on-disk document is internally consistent; the addendum is wrong about it and additionally contradicts itself, since its own heading says SEVEN while its body enumerates six names. The task briefing inherited this error.

**Recommendation:** Implement exactly the six on-disk names and record in DECISIONS.md that 'SEVEN' is treated as a miscount. Two consequences must be acted on now. First, every other addendum claim about on-disk content is suspect and must be independently verified before being treated as normative. Second, if a genuine seventh builtin exists in the un-pasted CRUCIBLE §3.4, adding it after Phase 3 becomes a lock change under anti-gaming rule 1 (the lock manifests every oracle definition), requiring `thesis oracles lock --reason` and a human-reviewed commit, so ask the user to grep their original CRUCIBLE §3.4 for a seventh name before Phase 3 creates the lock.

### [BLOCKING] Q3: `clock.skew` and `clock.jump` via libfaketime cannot work against Go binaries

§4.3 requires 'Clock: `clock.skew(ms)`, `clock.jump(ms)` via `libfaketime` or time namespaces' and the Phase 2 DoD requires 'Every fault kind can be injected and cleanly withdrawn.' Neither named mechanism works. LD_PRELOAD is read by the dynamic linker, and a CGO_ENABLED=0 Go binary is statically linked with no dynamic loader, so it is silently ignored. Even a dynamically linked Go binary is immune because Go's runtime calls the vDSO `__vdso_clock_gettime` directly from assembly and never calls the libc symbols libfaketime interposes. Linux time namespaces (CLONE_NEWTIME, available on this 6.6 kernel) virtualize only CLOCK_MONOTONIC and CLOCK_BOOTTIME, never CLOCK_REALTIME, and a lease-expiry bug is a wall-clock phenomenon, so the one clock that matters is the one namespaces cannot skew. Docker exposes no time-namespace option regardless, and `date -s` in a privileged container moves the entire shared WSL2 VM clock, hitting every container and corrupting parallel worlds.

**Recommendation:** Amend §4.3; 'via libfaketime or time namespaces' is factually wrong for the reference stack and will cost an implementer days. Implement `clock.*` against a cooperating SUT via an injectable-clock shim (`THESIS_CLOCK_OFFSET_MS` read through the fixture's clock abstraction), which is legitimate for our own fixture and is exactly the determinism gradient `thesis doctor` is meant to score. Against an unmodified SUT, `clock.*` must return UNSUPPORTED mapping to exit 2 INCONCLUSIVE and be recorded in the world, never silently skipped, since dropping a fault kind narrows the fault space, which anti-gaming rule 2 treats as a lock-level weakening. Mitigating fact that de-risks this: `clock.*` is not load-bearing for any DoD, since the corrected Q1 schedule reaches the fixture bug with partition and pause alone.

### [ACCEPT-AND-DOCUMENT] Q4: `io.fill` is simultaneously required and denied, and its withdrawal is not safe on this platform

On disk, §4.3's required fault families omit `io.fill` entirely while §4.2 places it under `deny:`. The addendum contradicts this, listing 'io: `io.latency(ms)`, `io.error(rate)`, `io.fill(pct)`' as part of 'the full v1 matrix as REQUIRED in Phase 2'. There is also a platform hazard neither document could know: on Docker Desktop/WSL2 the container filesystem lives in a sparse VHDX that auto-grows and never shrinks, so a successful `io.fill(90)` permanently consumes host disk even after withdrawal. That makes `io.fill` the one fault whose `withdraw()` cannot restore the prior state, violating §4.3's Critical Guarantee.

**Recommendation:** Implement the dispatcher so the fault space is complete and the Phase 2 DoD is satisfiable, but keep it in `deny:` in the scaffolded config to match the on-disk sample, implement it against a bounded tmpfs volume with an explicit size cap rather than the container's writable layer so withdrawal is a real bounded `rm`, and exercise it in the Phase 2 DoD test via an explicit `allow` override. Shipping it default-denied is safe under anti-gaming rule 2, since a default-deny sets the floor and only later removal from `allow:` counts as a weakening.

### [BLOCKING] Q5: The frozen driver contract makes Phase 5's operation shrinking unimplementable

§4.2 freezes `driver.cmd: "./bin/loadgen --history {history_path} --seed {seed} --profile {profile}"`, a placeholder set of `{history_path}`, `{seed}`, `{profile}`. Phase 5 requires 'ddmin over the workload operation history' with a DoD demanding a shrink to ≤10 ops. ddmin over operations requires re-executing a world with an arbitrary chosen subset of operations, and there is no channel in the contract to express 'run exactly these 6 ops': `{seed}` regenerates the whole profile-shaped workload and cannot subset it. Phase 5 cannot be built on this contract.

**Recommendation:** This is the most important ordering hazard in the spec: a Phase 5 requirement that must be satisfied by a Phase 0 schema decision, because `pkg/schema` freezes the driver block and the loadgen is written in Phase 0. Add `{plan_path}` (a JSONL op plan; when present the driver executes exactly that sequence and ignores the profile op count) and `{node_addrs}` (needed independently by Q10) to the placeholder vocabulary in Phase 0, and implement plan-replay in the Phase 0 loadgen. This is not 'inventing field names' under core rule 2, since `driver.cmd` is an opaque template string whose placeholder vocabulary is nowhere enumerated as frozen: record the vocabulary as frozen in DECISIONS.md so it cannot drift.

### [RESOLVABLE-WITH-DECISION] Q6: Phase 5's shrink pipeline takes 9 to 52 hours, contradicting I7 and the gate budget

Phase 5 mandates ddmin over 14 faults, then ddmin over 20,000 ops, then binary-search window narrowing, then 3/3 confirmation; §4.6 shows the results inline in a verdict and `profiles.gate` has `budget: 10m`, while I7 states 'Every run adheres to a strict wall-clock time or world-count budget.' Grounded in the measured 7.19 s compose cycle, a shrink trial costs about 30 s, and Tier B determinism (addendum §C) makes reproduction probabilistic so every ddmin test must be repeated k=3 or false negatives corrupt the search. Fault ddmin is then 90-180 executions (45-90 min), op ddmin is 900-6,000 executions (7.5-50 hours), window narrowing 84 executions (42 min), confirmation 3 executions. Naive total 9-52 hours, which is 45 to 300 times the entire gate budget.

**Recommendation:** Two changes. First, shrinking is never inline in `gate`: it becomes a separately budgeted command (`thesis shrink WORLD --budget 4h`), on by default in search/nightly and off in gate, with the verdict emitting `"shrink": {"attempted": false}` and a reason rather than a fabricated result. Second, replace raw op-ddmin with staged reduction that exploits structure ddmin ignores: binary-search the client count (16 to 2, ~12 execs), project to ops touching `witness.key` (20,000 to ~200 in one step, ~3 execs), truncate to the fault-to-violation time window (~10 execs), then ddmin over the ~40 survivors (~120-250 execs), then narrow and confirm (~87 execs). Staged total is roughly 2.5-4.5 hours: still not gate-compatible, but tractable overnight.

### [RESOLVABLE-WITH-DECISION] Q7: Phase 4's two DoD clauses are mutually antagonistic and the statistical one is unfalsifiable

Phase 4 requires both 'Automated search discovers the fixture stale-read bug from an empty/seed corpus within 30 minutes' and 'Statistically outperforms uniform random fault injection across 10 benchmark trials.' The second clause names no metric, no test, no effect size and no significance threshold, and n=10 is a weak instrument: a two-sided Mann-Whitney U at alpha=0.05 with 10 vs 10 requires U<=23, i.e. near-total separation. The outcome is also right-censored, since random trials that never find the bug have no time-to-detection, making naive mean or median comparison invalid. Worse, the two clauses fight each other through a single hidden variable, fixture difficulty: make the bug easy and clause 1 passes in a handful of worlds but random also saturates at 10/10 detection so no test can show a difference; make it hard and clause 2 becomes demonstrable but the 30-minute budget gets tight against a measured 40-60 sequential worlds. The DoD also costs 10 trials x 2 arms x 30 min = 10 hours of pure compute per execution.

**Recommendation:** Pre-register the benchmark before writing the search loop. Metric: detection within a fixed world budget, not wall clock, since wall clock varies by fault kind and is not comparable across arms. Test: one-sided Fisher's exact on detection counts, which handles censoring correctly and is powerful enough at n=10 (9/10 vs 2/10 gives p≈0.0055; 10/10 vs 0/10 gives p≈1.1e-5). Difficulty: build the fixture with an explicit `--difficulty=easy|hard` flag controlling lease duration and stale-read routing probability, pre-registering `easy` for the Phase 2 DoD and `hard` for the Phase 4 benchmark and stating the preset in the verdict. Run 3-4 worlds concurrently (8 vCPU, 32 GB measured) so the 30-minute clause has real headroom at `hard`.

### [RESOLVABLE-WITH-DECISION] Q8: The `phases` array cannot represent the mandated eight-phase lifecycle

§4.1 mandates eight phases and states that PERTURB 'overlaps `DRIVE`', but §4.5's `prothesis.oracle_input/v1` gives a flat, disjoint, contiguous array containing only DRIVE, HEAL, QUIESCE and ASSERT. PERTURB cannot be expressed because it overlaps DRIVE while the array is disjoint, and BOOT and SEED cannot be expressed because §4.3 pins the millisecond epoch to DRIVE start ('WINDOW: start_ms..end_ms relative to DRIVE start') and DRIVE starts at 0, so they would need negative offsets. This defeats I5 ('Every oracle declares the specific lifecycle phases in which it is valid') for half the lifecycle, including the phase whose exclusion matters most for false-positive suppression: an oracle cannot declare itself invalid during PERTURB if PERTURB is absent from its input.

**Recommendation:** Emit all eight phases, allow intervals to overlap, allow negative `start_ms` for BOOT and SEED, and keep the epoch at DRIVE start as §4.3 requires. Document that consumers must not assume the array is disjoint or sorted. This adds no field names, so core rule 2 is respected. Re-basing the epoch to BOOT start would make offsets positive but would silently change the meaning of every fault window in §4.3 and every existing `.thesis` file, so it is rejected.

### [BLOCKING] Q9: Three of the six mandatory Phase 1 oracles depend on tunables absent from the config schema

Phase 1 requires `no_stuck_op` ('Zero pending ops exceeding SLO ceiling after HEAL'), `resource_return_to_baseline` ('RSS, FDs, goroutines return to within ±N% of baseline post-quiesce') and `availability_after_heal` ('Health probes respond within convergence window'). The SLO ceiling, N, and the convergence window appear in no field of `prothesis.yaml` §4.2: the convergence grace window is mentioned narratively in §4.1's QUIESCE description but never given a config field. Half the mandatory Phase 1 oracle set is therefore unimplementable without inventing configuration.

**Recommendation:** Add `oracles.params` (a map keyed by oracle name) and a `lifecycle` block carrying the convergence window to the Phase 0 schema, with documented defaults, and include both in the lock projection. Hardcoding in Go source is specifically wrong rather than merely inelegant: anti-gaming rule 1 requires the lock to hash 'every oracle definition', and an oracle's thresholds are part of its definition; a threshold living in Go source can be weakened by an agent with the lock none the wiser, defeating I6 exactly where it is meant to bite. This must land in Phase 0 because Phase 1 needs the values and Phase 3 hashes the projection.

### [BLOCKING] Q10: The health probe template is unbound, and container IPs are unreachable from the Windows host

§4.2 specifies `probe: "http://{host}:{port}/healthz"` but the node schema is `{id, service, role_hint}` and binds no port, so `{port}` has no source. Separately, measured on this machine: the container address `172.17.0.2:8080` is unreachable from the Windows host while the published `127.0.0.1:18099` is reachable, so `{host}` cannot mean the container address on Docker Desktop. On Linux the container IP would work and this defect would be invisible, which makes it a latent CI-vs-local divergence. Phase 0's DoD (a), '`thesis up && thesis down` boots and tears down the KV fixture cleanly', requires health polling to succeed, so this blocks Phase 0 itself. A compounding constraint: Phase 4 parallelism requires concurrent compose projects, and fixed host ports collide across them, so `{port}` cannot be a static config value either.

**Recommendation:** Make probes a typed union with an explicit execution locus (`{http: ...}` or `{exec: {cmd: [...], in: "host"|"node:<id>"}}`) in the Phase 0 schema, and resolve `{host}`/`{port}` at run time per world by querying `docker compose port <service> <container_port>`. Never publish fixed host ports in the fixture compose file. Expose the resolved endpoint map to the driver as `{node_addrs}`, which Q5 needs anyway.

### [BLOCKING] Q11: `steady.sh`, CRLF translation, and hash stability form a three-way Windows failure

§4.2 specifies `steady_state: probe: "thesis-helpers/steady.sh"`. A `.sh` probe cannot execute on this host: Git Bash is broken, verified by `dofork: child -1 ... exit code 0xC0000142`. The repository is also configured to corrupt it: measured `core.autocrlf=true` with no `.gitattributes`, and git already emits 'LF will be replaced by CRLF the next time Git touches it' on a repo with zero commits. Three casualties follow. `steady.sh` committed on Windows and checked out with CRLF gets shebang `#!/bin/sh\r` and fails inside a Linux container with a misleading 'no such file or directory'. `.thesis` regression files, which addendum §D requires be committed, lose byte-identity, breaking Phase 0 DoD (b) and every embedded digest. And `.prothesis/lock`, which hashes oracle definitions that are mostly scripts, sees their SHA-256 change on checkout, producing a spurious exit 4 ORACLE_DRIFT on every clone from a different platform: the worst outcome, since addendum §I instructs agents to halt, escalate to a human, and never auto-resolve it.

**Recommendation:** Ship `.gitattributes` in the first commit, before any `.thesis`, oracle script, or lock file exists: `* text=auto eol=lf`, `*.thesis binary`, `*.sh text eol=lf`, `*.jsonl text eol=lf`, `.prothesis/lock binary`. Separately, make `steady_state` a typed probe (per Q10) that runs inside a node container via `docker compose exec` by default, where a POSIX shell genuinely exists. Do not depend on a host shell on Windows. Setting `core.autocrlf=false` locally is not a fix, since it does not travel with the repo.

### [RESOLVABLE-WITH-DECISION] Q12: `perturber.constraints` are English sentences the tool cannot enforce

§4.2 specifies `constraints: ["never partition more than minority of kv", "pg must be reachable during SEED"]`. These are natural language and nothing can evaluate them. They are not decorative: the first prevents the search from wedging the cluster into a trivially-unavailable state that would generate false `availability_after_heal` violations in most worlds (exactly the I5 false-positive problem) and the second protects SEED. Silently ignoring them is the 'silently relax a requirement' that core rule 4 forbids, and would make the search generate unusable worlds.

**Recommendation:** Keep the field a `[]string` so the schema shape is unchanged, but require each entry to parse against a small normative grammar defined in Phase 0: `never partition more than minority of <group>`, `never <kind> more than <n> of <group>`, `<node> must be reachable during <PHASE>[,<PHASE>...]`. An unparseable constraint is a CONFIG_ERROR (exit 5), not a warning. Failing loudly on a constraint that cannot be enforced is the only behaviour consistent with core rule 4.

### [RESOLVABLE-WITH-DECISION] Q13: I1's mock prohibition versus testing PRO-THESIS itself, and the `sim`/`k8s` backends

I1 states that fakes and mocks are 'strictly forbidden in the system under test (permitted only inside future Tier A deterministic simulation runtimes)'. Read maximally it would forbid the tool's own unit tests from using fakes, which at a measured ~30 s per world would make a single ddmin unit test take hours and leave Phases 4 and 5 with no CI-testable regression coverage. Separately, §4.2 declares `backend: compose | process | k8s | sim` while addendum §K says 'Does not require Kubernetes in v1' and §C places Tier A simulation in v4, so two of the four enumerated backends are out of v1 scope.

**Recommendation:** I1 is textually scoped to the SUT ('in the system under test') and binds the SUT only; record this reading in DECISIONS.md so it is not relitigated. The tool's own tests may use a `harness.Backend` fake constructed only by an unexported test constructor, with no `backend: mock` config value, so it is unreachable from any `prothesis.yaml`. The real risk is not that a fake exists but that it becomes reachable from a real run, letting an agent make the gate pass by repointing the config, so the mitigation is unreachability by construction, not policy. `sim` and `k8s` parse for forward compatibility but are rejected in v1 with CONFIG_ERROR (exit 5).

### [RESOLVABLE-WITH-DECISION] Q14: `verdict.commit` and `thesis bisect` in a repository with no commits

`prothesis.verdict/v1` carries `"commit": "9c1e0f2"` and addendum §H specifies `thesis bisect WORLD --good SHA --bad SHA`. Measured: the repo exists but `git log` reports 'your current branch master does not have any commits yet', so there is no HEAD to populate `commit` and nothing for bisect to traverse. Separately, addendum §L requires worlds to be 'portable across repos, not tied to one project's layout', but `commit` implicitly assumes a single repo containing both `prothesis.yaml` and the SUT source, making it meaningless or misleading for a shared `.thesis`. Also unspecified: addendum §D says shrunk worlds are 'committed to the repo' without saying by whom; if `thesis` runs git itself, an autonomous agent gains commit authority as a side effect of running the gate.

**Recommendation:** Set `commit` to the HEAD of the repository containing `prothesis.yaml`, or the empty string when unavailable, never fabricated, since a placeholder would put a false provenance claim into a machine-consumed verdict. `thesis bisect` outside a git repo, or in one with no commits, exits 5 CONFIG_ERROR with a clear message. `thesis` never invokes git for writes: it writes `.prothesis/regressions/w_<id>.thesis` and prints the `git add` command for a human. Append-only enforcement does not need git, since anti-gaming rule 4 is satisfied by the regression file list inside the lock manifest.

### [RESOLVABLE-WITH-DECISION] Q15: Phase 3's Definition of Done depends on `thesis gate`, a Phase 6 deliverable

Core rule 1 states 'Under no circumstances may you skip ahead or implement phases out of order.' Phase 3's DoD requires that 'Any unauthorized manual edit to an oracle script or budget immediately causes `thesis gate` to exit with code `4`.' But `thesis gate --profile gate --json` is listed as a Phase 6 deliverable. Phase 3 cannot satisfy its own DoD without violating rule 1.

**Recommendation:** Restate Phase 3's DoD as: 'Any unauthorized edit to an oracle definition, the allowed fault set, or a profile budget causes `thesis oracles verify` and any lock-checking run to exit 4.' Implement lock verification as a library check in `internal/lock`, invoked by `run` from Phase 3 onward; Phase 6's `gate` then merely calls the same library. No forward implementation is required and the invariant is enforced from Phase 3.

### [RESOLVABLE-WITH-DECISION] Q16: Phase 2 has no normative way to execute a scripted fault schedule

Phase 2's DoD requires 'Executing a scripted fault schedule (`proc.pause(role:leader)@8200..15100` overlapping `net.partition(minority(kv))@8400..14900`)'. But the CLI surface in addendum §H offers `thesis replay WORLD`, which is a Phase 5 deliverable, and `thesis run` has no `--faults` or `--world` flag. Phase 2 can parse the grammar and dispatch faults but has no user-facing way to invoke a chosen schedule, so its DoD is untestable within Phase 2.

**Recommendation:** Add `thesis run --faults "<schedule>"` (repeatable) and `thesis run --world FILE` in Phase 2. Core rule 2 freezes schemas, not the CLI surface, and addendum §H is described as 'fuller than on-disk' rather than exhaustive. Record the addition in DECISIONS.md. Phase 5's `replay` then becomes a thin alias over `run --world` plus determinism reporting, which also avoids duplicating the execution path.

### [RESOLVABLE-WITH-DECISION] Q17: Linearizability checking will blow up on gate-profile histories, and its worst case is exactly what the tool produces

Phase 3 requires a linearizability checker over histories from `profiles.gate: { clients: 16, ops: 20000, mix: { read: 0.4, write: 0.4, txn: 0.2 } }`. Linearizability checking is NP-complete in general, and §4.4's own semantics supply the aggravating factor: '`info`: State is indeterminate'. Every `info` operation is a may-or-may-not-have-happened that the checker must branch on, and network partitions and process pauses (that is, precisely what PRO-THESIS does for a living) generate `info` operations in bulk. The tool's core function drives its own checker into its worst case, and a 20,000-op, 16-client history with a burst of indeterminate ops around the fault window will exhaust memory or run unbounded.

**Recommendation:** Partition histories by key before checking (a per-key register model is the right abstraction for the KV fixture), impose a hard per-partition timeout and memory ceiling, and cap ops for checked profiles or check only the window around the fault schedule. Critically, a checker timeout must map to status `inconclusive` and exit 2, never PASS: a checker that gives up must not be readable as 'no violation', which would be the most dangerous silent gate weakening in the system given that addendum §J puts `thesis gate` in an autonomous merge loop.

### [RESOLVABLE-WITH-DECISION] Q18: Two mandatory Phase 1 oracles require instrumentation that addendum §L forbids requiring

Phase 1 mandates `no_unbounded_queue` ('Monotonically increasing queue depth across QUIESCE') and `resource_return_to_baseline` ('RSS, FDs, goroutines return to within ±N% of baseline'). Addendum §L states the opposite constraint: the tool 'Must work on unmodified systems today (rules out requiring instrumentation)'. Goroutine counts require `/debug/pprof` or `expvar`, and 'queue depth' is an application-specific metric no unmodified system exposes generically, so two of six mandatory oracles are unimplementable against an unmodified SUT. A secondary wrinkle: FD counts need `/proc/1/fd`, and distroless images have no shell for `docker exec ls`.

**Recommendation:** Tier the acquisition with honest degradation. Always available without SUT cooperation or a shell: RSS and CPU from the cgroup via `docker stats` or a sidecar sharing `pid: "service:<node>"` reading `/proc`. Opt-in: goroutines and queue depth via a configured `telemetry.endpoint`. When a signal is unavailable the oracle reports `inconclusive` for that dimension and names the missing signal: it must never silently pass on a dimension it could not measure, which would be a gate weakening by omission. `thesis doctor` reports which dimensions are observable, which is precisely its stated readiness-scoring role.

### [RESOLVABLE-WITH-DECISION] Q19: Exit code 3's definition leaves the most common truncated-run outcome unmapped

§4.7 defines '`3`: BUDGET_EXHAUSTED; Budget expired without coverage progress; soft warning.' The clause is conditioned on no coverage progress. The common case (budget expires, coverage did progress, no violation found, but only 22 of 30 planned worlds ran (exactly the situation §4.6's own example verdict shows with `worlds_planned: 30, worlds_run: 22`)) matches neither code 0 nor code 3 by their own words. Returning 0 is a lie, since §4.7 defines PASS as 'All oracles satisfied across all worlds' and eight planned worlds never ran. This matters because addendum §J puts `thesis gate` in an autonomous merge loop, so a truncated run reported as PASS merges unverified code.

**Recommendation:** Exit 0 only if `worlds_run == worlds_planned` and all oracles passed. Any budget-truncated run without a violation exits 3, with the coverage-progress distinction carried in the verdict body (`coverage.new_templates > 0`) rather than in the exit code. This is a deliberate reading of an ambiguous clause and is recorded as such; it is a strengthening rather than a relaxation, which core rule 4 permits.

### [RESOLVABLE-WITH-DECISION] Q20: `go.mod` pins `go 1.27.1` against a stated target of Go 1.22+

The briefing states 'Target Go 1.22+ standard library' but the on-disk `go.mod` declares `go 1.27.1`, and Go is not yet installed on this machine. Under Go 1.21+ toolchain semantics a `go` directive above the installed toolchain either forces an automatic toolchain download (requiring module-proxy network access) or hard-fails under `GOTOOLCHAIN=local`. This breaks the very first `go build` of the project, and pinning the floor at the newest available patch release forecloses CI on any older runner for no compensating benefit.

**Recommendation:** Change the directive to `go 1.22` and add no `toolchain` line. Raise it only when a specific language feature demands it. Nothing in this design needs a post-1.22 feature; 1.22 already supplies `log/slog` and the corrected loop-variable semantics the codebase will rely on.

### [RESOLVABLE-WITH-DECISION] Q21: 'Build the fixture first' conflicts with strict phase order, though the largest instance dissolves under measurement

§7 instructs 'Build `testdata/kvfixture/` first' and then 'Progress strictly through Phase 0, Phase 1, ...', while core rule 1 forbids implementing phases out of order. But the fixture is inherently a cross-phase artifact: it needs a compose topology and health endpoints (Phase 0/1), a loadgen emitting normative JSONL with correct ok/fail/info semantics and stable op_ids for witnesses (Phase 1/3), plan-replay mode (Phase 5, per Q5), a difficulty knob (Phase 4, per Q7), and an injectable clock (Phase 2, per Q3). Building it 'first' necessarily means making commitments on behalf of Phases 1 through 5 during Phase 0.

**Recommendation:** Declare that phase order governs `internal/` and `cmd/` only; `testdata/` is a test input, not a phase deliverable. The tension is smaller than it appears because the biggest instance dissolves empirically: the briefing anticipated that the fixture topology would have to commit to the Phase 2 network-fault decision (ambassador proxies versus tc), but a NET_ADMIN sidecar attached at runtime via `docker run --network=container:<cid>` injects and cleanly withdraws netem in an unmodified target's namespace (measured 8.347 ms baseline, 311.661 ms injected, 6.674 ms withdrawn, ending at `qdisc noqueue`). The sidecar never appears in the compose file, so Phase 0's topology needs no fault-related structure at all. Hold the Phase 0 fixture to exactly eight minimal forward commitments: one compose service per node (never `deploy.replicas`); no fixed host ports; loadgen `--plan` mode plus `{plan_path}`; Raft port separate from client port; injectable clock behind an interface; a difficulty flag; `.gitattributes`; and `oracles.params` plus `lifecycle` in the schema. Each costs hours now and days after the schema is frozen or the lock is created.

### [RESOLVABLE-WITH-DECISION] Q22: 'Byte-identical' round-trip is undefined in direction and encoding, and has a live integer-precision hazard

Phase 0's DoD (b) requires that 'A world configuration round-trips through `.thesis` serialization byte-identically.' Three problems. Direction is unstated: `decode(encode(w)) == w` and `encode(decode(b)) == b` are different claims, and the second is false for any non-canonical input. The format is undefined: neither document specifies a wire format for `.thesis`, so Phase 0 chooses both the format and the test that validates it, making the DoD self-certifying; note that YAML forecloses the requirement outright through comment loss and quoting normalization. And integer precision is a live hazard: history `t_ns` values are int64 Unix epoch nanoseconds (~1.76e18 in 2026), which exceed float64's exact-integer range of 2^53 ≈ 9.007e15, so any decode path through `map[string]interface{}` silently corrupts them. Tellingly, §4.4's own sample uses `t_ns: 1725300000000000` ≈ 1.7e15, about 20 days after the epoch, so the sample is either relative-to-run-start or has lost three digits.

**Recommendation:** Define the DoD as testable and honest. `.thesis` is canonical JSON: UTF-8, LF, struct-declaration key order with no maps in the type graph, `SetEscapeHTML(false)` since Go escapes `<`, `>` and `&` by default, two-space indent, exactly one trailing newline, integers as integers. Typed structs only, with `witness` held as `json.RawMessage` and canonicalized on ingest. Test as `encode(decode(encode(w))) == encode(w)` plus `decode(encode(w))` deep-equalling `w`, plus a committed golden `.thesis` fixture so future encoder changes are caught, which is a further reason `.gitattributes` (Q11) must exist first. Pin `t_ns` explicitly as int64 Unix epoch nanoseconds and treat §4.4's sample magnitude as illustrative.

### [ACCEPT-AND-DOCUMENT] Q23: Strict lock equality punishes strengthening the gate, and whole-file hashing blocks ordinary development

Addendum §E.1 states that `thesis gate` 'recomputes and compares. Mismatch returns exit 4 and REFUSES TO RUN.' Strict hash equality is symmetric, so adding an oracle, widening `allow:`, or lengthening a budget also produces exit 4: the anti-gaming mechanism fires hardest on the behaviour it exists to encourage. Combined with addendum §I ('ORACLE_DRIFT, lock mismatch → stop, escalate to human, NEVER auto-resolve'), an agent that correctly strengthens the gate is halted and escalated. Separately, if the lock hashes the whole `prothesis.yaml`, then adding an unrelated node or renaming a service trips ORACLE_DRIFT and blocks ordinary development.

**Recommendation:** Keep strict equality; it is simple and unforgeable, and a monotone 'only weakening fails' rule is exactly the surface an adversarial agent would probe, while safely inferring weakening-versus-strengthening for oracle scripts is not tractable. Mitigate ergonomically instead: hash a canonical projection (allow/deny set, `max_concurrent_faults`, `max_faults_per_world`, per-profile budgets and world counts, `oracles.params`, per-oracle-file digests, regression file list) rather than the raw file, and on mismatch print a field-level diff labelling each change WEAKENING or STRENGTHENING so the human review that `--reason` demands takes seconds. Accept and document that both directions require a lock bump.

## Risks

- The fixture bug may not be reproducible at all under the spec's own reference schedule (Q1). If Phase 2's DoD is attempted literally before this is corrected, the implementer will spend days debugging a correct perturber against a schedule that is physically incapable of producing the anomaly, and may 'fix' it by weakening an oracle: the exact failure mode I6 exists to prevent.
- Phase 5 is the schedule risk. Even with the staged reduction pipeline it is a 2.5-4.5 hour operation per violation, and 9-52 hours if raw ddmin over 20,000 ops is attempted as written. Validating Phase 5's DoD requires multiple such runs, so Phase 5 validation alone could consume 20-60 hours of wall clock that no one has budgeted.
- Phase 4's two DoD clauses cannot both be satisfied at a single fixture difficulty (Q7). If the difficulty knob (D16) is not built in Phase 0, discovering this in Phase 4 forces a fixture retune mid-benchmark, which silently invalidates every trial already run and is indistinguishable from gaming the gate.
- The CRLF hazard (Q11) is the highest-probability unplanned time sink. It is a one-line fix in the first commit and multi-day archaeology afterwards, and its worst presentation (a spurious ORACLE_DRIFT exit 4 after a cross-platform clone) is one that addendum §I instructs agents never to auto-resolve, so it will halt an autonomous loop with a misleading security-flavoured error.
- Docker Desktop on Windows diverges from Linux in ways that are invisible in CI. Container IPs are unreachable from the host (measured), the VHDX never shrinks after io.fill, and the WSL2 VM clock is shared across all containers. A design validated only on Linux CI will fail on the user's actual machine, and vice versa.
- The addendum is demonstrably unreliable as a description of the on-disk document (Q2). Three of its claims in §B alone are false. Any implementation that trusts addendum statements about on-disk content without verification will encode further errors, and the one that matters most (whether a seventh builtin oracle exists) becomes expensive to correct after Phase 3 creates the lock.
- Linearizability checking blows up precisely on the histories PRO-THESIS is designed to produce (Q17). Heavy fault injection generates indeterminate `info` operations in bulk, which is the checker's worst case, so the tool's effectiveness at finding bugs is inversely correlated with its ability to check them. If a checker timeout is ever mapped to PASS rather than INCONCLUSIVE, the gate silently stops working at exactly the moment it is needed.
- Scope expectation is itself a risk. Total build is 23,000-34,000 LOC across 160-235 Go files and 10-16 focused engineer-weeks. 'Begin immediately' delivers Phase 0 only: init/up/down, a schema package, a world codec, and a fixture that boots. It injects zero faults, runs zero oracles, and produces zero verdicts.

## Files implied (25)

- `C:\AI Projects\Pro-synthesis\OPEN_QUESTIONS.md`: The 23 audited conflicts, each with a quote, a BLOCKING/RESOLVABLE/ACCEPT classification, and a recommended resolution _(Phase 0)_
- `C:\AI Projects\Pro-synthesis\DECISIONS.md`: The 16 conflict-resolution decisions (D1-D16) with rationale and rejected alternatives _(Phase 0)_
- `C:\AI Projects\Pro-synthesis\.gitattributes`: Prevents CRLF corruption of .thesis worlds, oracle scripts and the lock manifest; must be in the first commit (Q11, D2) _(Phase 0)_
- `C:\AI Projects\Pro-synthesis\go.mod`: Existing file: change the go directive from 1.27.1 to 1.22 so the first build does not force a toolchain download (Q20, D15) _(Phase 0)_
- `C:\AI Projects\Pro-synthesis\pkg\schema\config.go`: prothesis.yaml types including the Phase 0 additions oracles.params, lifecycle, node port and the typed probe union (Q9, Q10, D5, D8) _(Phase 0)_
- `C:\AI Projects\Pro-synthesis\pkg\schema\world.go`: The I2 world tuple plus provenance, with no map or interface{} anywhere in the type graph (Q22, D1) _(Phase 0)_
- `C:\AI Projects\Pro-synthesis\pkg\schema\verdict.go`: prothesis.verdict/v1 types with the verdict enum frozen and witness held as json.RawMessage (Q22, Q19) _(Phase 0)_
- `C:\AI Projects\Pro-synthesis\pkg\schema\history.go`: JSONL history types with t_ns pinned as int64 Unix epoch nanoseconds and the ok/fail/info rule enforced (Q22) _(Phase 0)_
- `C:\AI Projects\Pro-synthesis\pkg\schema\oracle_io.go`: oracle_input/v1 and oracle_output/v1, emitting all eight lifecycle phases with overlapping and negative windows (Q8, D13) _(Phase 0)_
- `C:\AI Projects\Pro-synthesis\pkg\schema\constraints.go`: Parser for the perturber.constraints mini-grammar; an unparseable constraint is CONFIG_ERROR, never a warning (Q12, D8) _(Phase 0)_
- `C:\AI Projects\Pro-synthesis\pkg\schema\projection.go`: Canonical lock projection of the config, so the lock hashes semantics rather than the raw YAML file (Q23, D10) _(Phase 0)_
- `C:\AI Projects\Pro-synthesis\internal\recorder\world_codec.go`: Canonical JSON encoder/decoder for .thesis; the sole definition of what byte-identical means (Q22, D1) _(Phase 0)_
- `C:\AI Projects\Pro-synthesis\internal\recorder\world_codec_test.go`: Phase 0 DoD (b): encoder idempotence, value round-trip, and golden-file comparison _(Phase 0)_
- `C:\AI Projects\Pro-synthesis\internal\recorder\testdata\golden_world.thesis`: Committed golden world file that makes DoD (b) non-self-certifying by catching encoder drift _(Phase 0)_
- `C:\AI Projects\Pro-synthesis\internal\harness\compose.go`: Compose backend: per-world project naming, ephemeral port discovery via docker compose port, up/down (Q10, D5) _(Phase 0)_
- `C:\AI Projects\Pro-synthesis\internal\harness\probe.go`: Typed probe execution with a host-or-node locus, so steady.sh runs inside a Linux container (Q11, D5) _(Phase 0)_
- `C:\AI Projects\Pro-synthesis\internal\harness\backend.go`: Backend interface plus the v1 validator rejecting sim and k8s with exit 5 (Q13, D11) _(Phase 0)_
- `C:\AI Projects\Pro-synthesis\cmd\thesis\main.go`: CLI entrypoint with init, up and down, and the normative exit-code mapping (Q19) _(Phase 0)_
- `C:\AI Projects\Pro-synthesis\testdata\kvfixture\docker-compose.yaml`: One service per node, no fixed host ports, no fault-specific structure needed thanks to runtime sidecars (Q21, D3) _(Phase 0)_
- `C:\AI Projects\Pro-synthesis\testdata\kvfixture\Dockerfile`: Multi-stage build from golang:1.22 so the fixture builds without host Go (D6) _(Phase 0)_
- `C:\AI Projects\Pro-synthesis\testdata\kvfixture\kv\lease.go`: The intentionally unsound leader-lease read path, with the difficulty knob and injectable clock (Q1, Q3, Q7, D6, D16) _(Phase 0)_
- `C:\AI Projects\Pro-synthesis\testdata\kvfixture\loadgen\main.go`: Load generator emitting normative JSONL, with --plan replay mode required by Phase 5 (Q5, D7) _(Phase 0)_
- `C:\AI Projects\Pro-synthesis\internal\perturber\capability.go`: ErrFaultUnsupported mapping to INCONCLUSIVE so unsupported faults are never silently skipped (Q3, Q18, D12) _(Phase 2 (interface stubbed in Phase 0))_
- `C:\AI Projects\Pro-synthesis\internal\perturber\faults\net.go`: Peer-scoped tc/netem and iptables injection via a runtime NET_ADMIN sidecar, with residual verification for HEAL (Q21, D3) _(Phase 2)_
- `C:\AI Projects\Pro-synthesis\docker\netadmin\Dockerfile`: The thesis-netadmin sidecar image (alpine plus iproute2 and iptables) built once and reused per fault _(Phase 2)_

## Design

# PRO-THESIS: Specification Audit

**Auditor:** adversarial review of `PRO_THESIS_FABLE_PROMPT.md` (on-disk normative) + `CRUCIBLE_ADDENDUM.md` (pasted normative).
**Date:** 2026-09-06
**Scope discipline:** Phase 0 only is in build scope. This audit's job is to make sure Phase 0 does not foreclose Phases 1–6.

---

## 0. Environment corrections to the briefing

Three briefing facts were stale. I verified all of them:

| Briefing said | Actually true | Consequence |
|---|---|---|
| "The project directory is NOT yet a git repo" | `.git` exists on branch `master` with **zero commits**; `cmd/ internal/ pkg/ testdata/ docs/ go.mod .gitignore` already scaffolded | `thesis bisect` and `verdict.commit` have no history to work with yet; see Q14 |
| "Docker Desktop... daemon is currently STOPPED" | Daemon **running**, server 29.1.3, Compose v2.40.3-desktop.1, kernel `6.6.87.2-microsoft-standard-WSL2`, 8 vCPU / 32 GB | Phase 0 can be validated today |
| "Target Go 1.22+" | `go.mod` declares `go 1.27.1` | First `go build` will force a toolchain download or fail; see Q20 |

Confirmed as briefed: Git Bash is broken (`dofork: child -1 ... exit code 0xC0000142`), Go is not installed.

### Empirical probes run for this audit

These four measurements decide multiple design questions, so I ran them rather than reasoning about them.

**1. Kernel supports netem and netfilter (as modules):**
```
CONFIG_NET_SCH_NETEM=m   CONFIG_NET_SCH_PRIO=m   CONFIG_NET_CLS_U32=m
CONFIG_IP_NF_FILTER=m    CONFIG_NETFILTER_XT_MATCH_OWNER=m
```

**2. tc/netem + iptables inject AND withdraw inside a container with `--cap-add=NET_ADMIN`:**
`NETEM OK` → `NETEM WITHDRAW OK` → `IPTABLES OK` → `IPTABLES WITHDRAW OK`.

**3. A sidecar can fault an *unmodified* target container's netns.** This is the load-bearing result:
```
baseline ping from target      : avg 8.347 ms
sidecar injects netem 300ms    : SIDECAR_INJECT_OK
ping from target after inject  : avg 311.661 ms     <-- target image never touched
sidecar withdraws              : SIDECAR_WITHDRAW_OK ; qdisc noqueue 0: root
ping after withdraw            : avg 6.674 ms
```
`qdisc noqueue` after withdraw is exactly the residual-free state `HEAL` must assert.

**4. Compose cycle cost, 3 nodes, warm images, healthcheck-gated:**
```
trial 1: up+wait = 5.54s  down -v = 1.65s  cycle = 7.19s
trial 2: up+wait = 5.53s  down -v = 1.66s  cycle = 7.19s
trial 3: up+wait = 5.53s  down -v = 1.70s  cycle = 7.23s
```

**5. Windows host networking and `docker pause`:**
```
container IP = 172.17.0.2
direct container IP reachable from Windows host : False
published port reachable from Windows host      : True
state after pause : paused
TCP accepts while paused (gray-failure check)   : True
```

---

## PART 1: OPEN_QUESTIONS.md content

### Q1: The spec's marquee example is impossible under Raft quorum arithmetic **[BLOCKING]**

§4.6 `causal_timeline`:
> `{ "t_ms": 8200, "event": "fault", "detail": "proc.pause n2 (leader, term 4)" }`
> `{ "t_ms": 8400, "event": "fault", "detail": "partition n3 from {n1,n2}" }`
> `{ "t_ms": 9010, "event": "log", "node": "n1", "detail": "elected leader term 5" }`

Three nodes, quorum = 2. At t=8400: **n2 is frozen** (cannot answer `RequestVote`), **n3 is partitioned from n1**. So at t=9010, n1 can collect exactly **one** vote: its own. n1 cannot be elected leader in term 5. The event at 9010 cannot happen.

The Phase 2 DoD inherits the same defect:
> Executing a scripted fault schedule (`proc.pause(role:leader)@8200..15100` overlapping `net.partition(minority(kv))@8400..14900`) triggers the Raft fixture's stale-read anomaly.

Both resolutions of `minority(kv)` fail:
- **`minority(kv)` resolves to a non-leader** → the case above: no election is possible, the cluster is wedged, no violation is produced.
- **`minority(kv)` resolves to the paused leader itself** → n1+n3 form a quorum and *can* elect, but the pause runs 8200→15100 = **6900 ms, which exceeds the 5-second lease** from §5. `SIGSTOP` and the cgroup freezer do **not** stop `CLOCK_MONOTONIC` or `CLOCK_REALTIME`; time advances while the process is frozen. On resume the former leader's lease has already expired, it steps down, and no stale read is served.

So the spec's one concrete reproduction recipe does not reproduce.

**Recommendation.** Restate the Phase 2 DoD in terms of the *anomaly class* ("a scripted schedule reproduces the fixture's lease-violation stale read"), and replace the reference schedule with one that actually works:
- Single-fault (easy preset): `net.partition(role:leader)@8000..14000`. The isolated leader keeps serving reads on its still-valid local lease while the majority elects a new leader and commits. Client traffic reaches it because partitions are scoped to *peer* addresses only (see D3).
- Two-fault overlap (hard preset, for Phase 4): `proc.pause(role:leader)@8000..9200` (**1200 ms, deliberately shorter than the 5 s lease**) overlapping a partition. Long enough to lose leadership, short enough that the lease survives the resume.

Correct §4.6's timeline to match, or mark it explicitly non-normative.

---

### Q2: The addendum misreports the on-disk document and contradicts itself on oracle count **[RESOLVABLE-WITH-DECISION]**

Addendum §B heading:
> "CRUCIBLE §3.4 requires **SEVEN**, the on-disk sample config lists **five**"

and body:
> "The on-disk §4.2 sample YAML **omits `no_stuck_op`** from `oracles.builtin`"

I verified the on-disk file. **All three claims are false.** §4.2 lists **six**, `no_stuck_op` included:
```
  builtin:
    - no_crash
    - no_panic_log
    - no_unbounded_queue
    - resource_return_to_baseline
    - availability_after_heal
    - no_stuck_op
```
This matches §6 Phase 1 exactly ("Implement all 6 built-in generic oracles"). The on-disk document is internally **consistent**; the addendum is wrong about it, and the addendum's own heading ("SEVEN") contradicts its own six-name enumeration.

Two consequences, one of them expensive:
1. **The task briefing inherited this error** ("the sample prothesis.yaml lists 5 builtin oracles but Phase 1 requires 6"). It is not a conflict; there is nothing to reconcile.
2. **Every other addendum claim about on-disk content is now suspect** and must be independently verified before being treated as normative.
3. If a genuine seventh builtin exists in the un-pasted CRUCIBLE §3.4, adding it **after Phase 3** is a lock change under anti-gaming rule 1 (the lock manifests "every oracle definition"), requiring `thesis oracles lock --reason` and a human-reviewed commit. The cheap moment to decide is now.

**Recommendation.** Implement exactly the six on-disk names. Record in `DECISIONS.md` that "SEVEN" is treated as a miscount. Ask the user to grep their original CRUCIBLE §3.4 for a seventh name **before Phase 3 locks the manifest**.

---

### Q3: `clock.skew` / `clock.jump` via libfaketime cannot work against Go binaries **[BLOCKING for the Phase 2 DoD]**

§4.3 requires:
> **Clock**: `clock.skew(ms)`, `clock.jump(ms)` via `libfaketime` or time namespaces.

and the Phase 2 DoD requires:
> Every fault kind can be injected and cleanly withdrawn.

Neither named mechanism works. This has a definite answer:

1. **`LD_PRELOAD` requires a dynamic loader.** A Go binary built with `CGO_ENABLED=0` (the default for the fixture and the normative "single, self-contained Go static binary" style) is statically linked and has no `ld.so`. `LD_PRELOAD` is read by the dynamic linker and is silently ignored. libfaketime interposes nothing.
2. **Even a `CGO_ENABLED=1` Go binary is immune.** Go's runtime does not route `time.Now()` through libc. `runtime.walltime`/`runtime.nanotime` call the vDSO's `__vdso_clock_gettime` directly from assembly. libfaketime works by interposing libc's `clock_gettime`/`gettimeofday`/`time` symbols, which Go never calls. Symbol interposition cannot reach it.
3. **Linux time namespaces do not virtualize wall-clock time.** `CLONE_NEWTIME` (kernel 5.6+, and this kernel is 6.6) offsets **`CLOCK_MONOTONIC` and `CLOCK_BOOTTIME` only**. `man 7 time_namespaces` states `CLOCK_REALTIME` is not virtualized. A lease-expiry bug is a wall-clock phenomenon, so the one clock that matters is precisely the one namespaces cannot skew.
4. **Docker does not expose time namespaces anyway.** There is no `--time-offset` or equivalent in the CLI/compose schema; containerd does not plumb `CLONE_NEWTIME`.
5. `date -s` in a privileged container moves the **entire shared WSL2 VM clock**, hitting every container at once. That is not per-node skew, and it will corrupt every other world running in parallel.

**Mitigating fact:** `clock.*` is **not load-bearing for any DoD**. Per Q1's corrected schedule the fixture bug is reachable with partition and/or a short pause. Nothing else depends on clock faults.

**Recommendation.**
- Implement `clock.skew`/`clock.jump` against a **cooperating** SUT via an injectable-clock shim (`THESIS_CLOCK_OFFSET_MS` read through the fixture's clock abstraction). This is legitimate for our own fixture and is exactly the "gradient toward determinism" `thesis doctor` is supposed to score (addendum §C).
- Against an **unmodified** SUT, `clock.*` must return `UNSUPPORTED` → exit 2 `INCONCLUSIVE`, recorded in the world file. It must **never** be silently skipped: silently dropping a fault kind narrows the fault space, which anti-gaming rule 2 classifies as a lock-level weakening.
- Amend §4.3's "*via libfaketime or time namespaces*": it is factually wrong for the reference stack and will send an implementer down a dead end for days.

---

### Q4: `io.fill` is simultaneously required and denied, and is genuinely dangerous here **[ACCEPT-AND-DOCUMENT]**

On disk, §4.3's required families omit `io.fill` entirely while §4.2 puts it in `deny:`. The addendum contradicts this:
> "io: `io.latency(ms)`, `io.error(rate)`, `io.fill(pct)`" listed as "the full v1 matrix as REQUIRED in Phase 2"

There is also an environment-specific hazard the spec cannot know about: on Docker Desktop/WSL2 the container filesystem lives inside a **sparse VHDX that auto-grows and never shrinks**. An `io.fill(90)` that succeeds permanently consumes host disk even after the fault is withdrawn: `withdraw()` cannot undo it. This makes `io.fill` the one fault whose withdrawal is not truly safe, violating §4.3's *Critical Guarantee*.

**Recommendation.** Implement the dispatcher (so the fault space is complete and the Phase 2 DoD is satisfiable), but: (a) keep it in `deny:` in the scaffolded config, matching the on-disk sample; (b) implement it against a **bounded tmpfs volume with an explicit size cap**, never the container's writable layer, so withdraw is a real `rm` on a bounded region; (c) exercise it in the Phase 2 DoD test with an explicit `allow` override. Note that shipping it default-denied is safe under anti-gaming rule 2: a default-deny *sets* the floor; only later removal from `allow:` is a weakening.

---

### Q5: The frozen driver contract makes Phase 5's op-shrinking unimplementable **[BLOCKING]**

§4.2 freezes the driver invocation:
```yaml
driver:
  cmd: "./bin/loadgen --history {history_path} --seed {seed} --profile {profile}"
```
The placeholder set is `{history_path}`, `{seed}`, `{profile}`. Phase 5 requires:
> 2. `ddmin` over the workload operation history.

and the DoD demands a shrink to **≤10 ops**. `ddmin` over ops requires re-executing the world with an *arbitrary chosen subset* of operations. There is no channel in the contract to express "run exactly these 6 ops". `{seed}` only regenerates the *whole* profile-shaped workload; you cannot subset it. Phase 5 cannot be built on this contract.

This is the single most important **ordering hazard**: it is a Phase 5 requirement that must be satisfied by a **Phase 0** schema decision, because `pkg/schema` freezes the driver block in Phase 0 and the fixture's `loadgen` is written in Phase 0.

**Recommendation.** Add two placeholders to the driver template in Phase 0's schema and implement plan-replay in the Phase 0 loadgen:
- `{plan_path}`: a JSONL op plan; when present the driver executes exactly that sequence and ignores `{profile}`'s op count.
- `{node_addrs}`: resolved node endpoints (needed anyway, see Q10).

Adding placeholders is not "inventing field names" under rule 2: `driver.cmd` is an opaque template string, and the placeholder vocabulary is nowhere enumerated as frozen. Record the vocabulary as frozen in `DECISIONS.md` so it cannot drift.

---

### Q6: Phase 5's shrink pipeline takes 9–52 hours and contradicts I7 and the gate budget **[RESOLVABLE-WITH-DECISION]**

Phase 5 mandates ddmin over 14 faults, then ddmin over 20,000 ops, then binary-search window narrowing, then 3/3 confirmation. §4.6 shows the results **inline in a verdict**, and `profiles.gate` has `budget: 10m`. I7 says:
> Every run adheres to a strict wall-clock time or world-count budget.

Grounded in the measured 7.2 s compose cycle, a shrink trial is ~30 s (see §Part 3). Tier B (addendum §C) means reproduction is probabilistic, so **every ddmin test must be repeated k=3 times** or a false negative corrupts the search:

| Stage | Executions | Wall clock |
|---|---|---|
| ddmin over 14 faults → 2 (typ. 30–60 tests × 3) | 90–180 | 45–90 min |
| **ddmin over 20,000 ops → 6** (300–2000 tests × 3) | **900–6000** | **7.5–50 h** |
| Window binary search (2 faults × 2 edges × ~7 steps × 3) | 84 | 42 min |
| Confirmation 3/3 | 3 | 1.5 min |
| **Naive total** | | **9–52 hours** |

Raw ddmin over 20,000 elements is the killer, and it is 100–500× the entire `gate` budget.

**Recommendation.** Two changes:
1. **Shrinking is never inline in `gate`.** It is a separately budgeted command (`thesis shrink WORLD --budget 4h`), on by default in `search`/nightly, off in `gate`. When skipped the verdict must emit `"shrink": {"attempted": false}` with a reason, never a fabricated result.
2. **Replace raw op-ddmin with staged reduction**, which exploits structure ddmin ignores:
   - Reduce client count by binary search (16 → 2): ~12 execs
   - **Key-space projection**: keep only ops touching `witness.key`; 20,000 → ~200 in one step: ~3 execs
   - Time-window truncation to `[first_fault − Δ, violation + Δ]`: ~10 execs
   - ddmin over the ~40 survivors: ~120–250 execs
   - Window narrowing + confirmation: ~87 execs
   - **Staged total: ~2.5–4.5 hours.** Still not gate-compatible, but tractable overnight.

---

### Q7: Phase 4's two DoD clauses are mutually antagonistic and the statistical one is unfalsifiable **[RESOLVABLE-WITH-DECISION]**

> - Automated search discovers the fixture stale-read bug from an empty/seed corpus **within 30 minutes**...
> - **Statistically outperforms uniform random fault injection across 10 benchmark trials.**

**The statistical clause is not falsifiable as written.** It names no metric, no test, no effect size, and no significance threshold. "Statistically outperforms" over n=10 is a very weak instrument: a two-sided Mann-Whitney U at α=0.05 with 10 vs 10 requires U ≤ 23, i.e. near-total separation of the two arms. Worse, the outcome is **right-censored**: random trials that never find the bug have no time-to-detection at all, so a naive mean/median comparison is invalid.

**The two clauses fight each other, and fixture difficulty is the single knob controlling both:**
- Make the bug easy (reachable by one partition, per Q1's corrected easy preset) → clause 1 passes trivially in ~2–5 worlds, but **random also finds it in ~2–5 worlds**, both arms saturate at 10/10 detection, and no test can show a difference. **Clause 2 becomes unachievable.**
- Make the bug hard (requires two overlapping faults with sub-second timing) → clause 2 becomes demonstrable, but clause 1's 30-minute budget gets tight (see Part 3: 40–60 sequential worlds).

**The measured cost is also non-trivial:** 10 trials × 2 arms × 30 min = **10 hours of pure compute** for one run of this DoD.

**Recommendation.** Pre-register the benchmark before writing the search loop:
- **Metric:** detection within a fixed **world budget** (not wall clock; wall clock varies with fault kind and is not comparable across arms).
- **Test:** one-sided **Fisher's exact** on detection counts. This handles censoring correctly and is powerful at n=10: 9/10 vs 2/10 gives p ≈ 0.0055; 10/10 vs 0/10 gives p ≈ 1.1e-5. Both are achievable.
- **Difficulty:** build the fixture with an explicit `--difficulty=easy|hard` flag (lease duration, probability the client routes reads to a stale leader). Pre-register `easy` for the Phase 2 DoD and `hard` for the Phase 4 benchmark, and state it in the verdict.
- **Parallelism:** run worlds 3–4 at a time (8 vCPU, 32 GB) so the 30-minute clause has real headroom.

---

### Q8: The `phases` array cannot represent the mandated 8-phase lifecycle **[RESOLVABLE-WITH-DECISION]**

§4.1 mandates eight phases and states:
> 4. `PERTURB`: Execute scheduled faults against the virtual clock (**overlaps `DRIVE`**).

§4.5's `prothesis.oracle_input/v1` gives a flat, disjoint, ordered array containing only four:
```json
"phases": [
  {"phase": "DRIVE",   "start_ms": 0,     "end_ms": 42000},
  {"phase": "HEAL",    "start_ms": 42000, "end_ms": 45000},
  {"phase": "QUIESCE", "start_ms": 45000, "end_ms": 50000},
  {"phase": "ASSERT",  "start_ms": 50000, "end_ms": 55000}
]
```
Two structural problems:
1. **`PERTURB` cannot be expressed.** It overlaps `DRIVE`, and the array as exemplified is disjoint and contiguous.
2. **`BOOT` and `SEED` cannot be expressed.** §4.3 pins the millisecond epoch to `DRIVE` start (`WINDOW: start_ms..end_ms relative to DRIVE start`), and `DRIVE` starts at 0 here, so `BOOT` and `SEED` would need negative offsets.

This directly defeats **I5; Phase-Aware Assertion** ("Every oracle declares the specific lifecycle phases in which it is valid") for four of the eight phases, including the one that matters most for false-positive suppression: an oracle cannot say "I am invalid during `PERTURB`" if `PERTURB` is not in the input.

**Recommendation.** Emit **all eight** phases; allow intervals to **overlap**; allow **negative** `start_ms` for `BOOT`/`SEED`; keep epoch = `DRIVE` start (required by §4.3). Document that consumers must not assume the array is disjoint or sorted. This adds no field names, so rule 2 is respected.

---

### Q9: Three of the six mandatory Phase 1 oracles depend on tunables absent from the config schema **[BLOCKING for Phase 1]**

| Oracle | Spec text | Missing parameter |
|---|---|---|
| `no_stuck_op` | "Zero pending ops exceeding **SLO ceiling** after `HEAL`" | the SLO ceiling |
| `resource_return_to_baseline` | "return to within **±N%** of baseline post-quiesce" | N, and which resources are mandatory |
| `availability_after_heal` | "Health probes respond within **convergence window**" | the convergence window (mentioned in §4.1 `QUIESCE` as "convergence grace window", never given a config field) |

`prothesis.yaml` §4.2 has no field for any of them. Hardcoding is the wrong fix for a specific reason: anti-gaming rule 1 requires `.prothesis/lock` to hash "every oracle definition". **An oracle's thresholds are part of its definition.** If N lives in Go source, an agent can weaken the gate by editing a constant and the lock will not notice: defeating I6 precisely where it matters.

**Recommendation.** Phase 0 adds `oracles.params` (a map keyed by oracle name) and a `lifecycle` block carrying the convergence window, both **included in the lock projection** (D10). Defaults documented in `DECISIONS.md`. This is required in Phase 0 because Phase 3 hashes the config projection and Phase 1 needs the values.

---

### Q10: The health probe template is unbound, and container IPs are unreachable from the Windows host **[BLOCKING for the Phase 0 DoD]**

```yaml
  health:
    - node: "kv:*"
      probe: "http://{host}:{port}/healthz"
```
Two independent failures:

1. **`{port}` has no source.** The node schema is `{id, service, role_hint}`. Nothing in `prothesis.yaml` binds a port. The template cannot be resolved.
2. **`{host}` cannot mean the container address on this platform.** Measured: `172.17.0.2:8080` → **unreachable from the Windows host**; `127.0.0.1:18099` (published) → reachable. On Linux the container IP would work and this defect would be invisible; on Docker Desktop for Windows it is fatal. Phase 0's DoD (a) ("`thesis up && thesis down` boots and tears down the KV fixture cleanly") requires health polling to succeed, so this blocks Phase 0 itself.

There is a compounding constraint: **parallel worlds require ephemeral published ports** (fixed host ports collide across concurrent compose projects), so `{port}` cannot be a static config value either.

**Recommendation.** Phase 0 makes probes a typed union with an explicit execution locus, and resolves `{host}`/`{port}` at runtime by querying `docker compose port <service> <container_port>` per world. Never publish fixed host ports in the fixture compose file. Also expose the resolved map to the driver as `{node_addrs}` (Q5).

---

### Q11: `steady.sh`, CRLF, and hash stability: a three-way Windows failure **[BLOCKING]**

```yaml
  steady_state:
    probe: "thesis-helpers/steady.sh"
```
A `.sh` probe on a Windows host with broken Git Bash cannot execute. And the repository is configured to corrupt it. Measured:
```
core.autocrlf = true          (no .gitattributes present)
warning: in the working copy of 'PRO_THESIS_FABLE_PROMPT.md',
         LF will be replaced by CRLF the next time Git touches it
```
Git is *already* warning about this, on a repo with zero commits. Three distinct casualties:

1. **`steady.sh`** committed on Windows, checked out with CRLF, then executed inside a Linux container → shebang becomes `#!/bin/sh\r` → `no such file or directory`. A classic, and it will present as a mysterious `INCONCLUSIVE`.
2. **`.thesis` regression files.** Addendum §D requires them "committed to the repo" and Phase 0's DoD (b) requires **byte-identical** round-trip. Any LF→CRLF translation on checkout breaks byte-identity and every embedded digest.
3. **`.prothesis/lock`.** It hashes oracle definitions (mostly scripts). CRLF translation changes their SHA-256 on checkout → spurious exit 4 `ORACLE_DRIFT` on every clone from a different platform. This is the worst outcome: the anti-gaming mechanism fires as a false positive, and the documented agent response (addendum §I) is "stop, escalate to human, NEVER auto-resolve."

**Recommendation.** Ship `.gitattributes` **in the first commit**, before any `.thesis` or lock file exists:
```gitattributes
* text=auto eol=lf
*.thesis  binary
*.sh      text eol=lf
*.jsonl   text eol=lf
.prothesis/lock binary
```
And make `steady_state` a typed probe that runs **inside a node container** by default (`docker compose exec`), where a POSIX shell genuinely exists. Do not depend on a host shell on Windows.

---

### Q12: `perturber.constraints` are English sentences the tool cannot enforce **[RESOLVABLE-WITH-DECISION]**

```yaml
  constraints:
    - "never partition more than minority of kv"
    - "pg must be reachable during SEED"
```
These are natural language. Nothing can evaluate them. But they are safety constraints on the fault scheduler: the first prevents the search from wedging the cluster into a trivially-unavailable state (which would generate false `availability_after_heal` violations in most worlds; exactly the I5 false-positive problem), and the second protects `SEED`.

Silently ignoring them is not an option: it is precisely the "silently relax a requirement" that rule 4 forbids, and it would make the search generate garbage.

**Recommendation.** Keep the field a `[]string` (schema shape unchanged), but require each entry to parse against a small normative grammar defined in Phase 0:
```
never partition more than minority of <group>
never <kind> more than <n> of <group>
<node> must be reachable during <PHASE>[,<PHASE>...]
```
An unparseable constraint is a **`CONFIG_ERROR` (exit 5)**, not a warning. Failing loudly on a constraint we cannot enforce is the only behavior consistent with rule 4.

---

### Q13: I1 "mocks strictly forbidden" vs. testing PRO-THESIS itself, and the `sim`/`k8s` backends **[RESOLVABLE-WITH-DECISION]**

> **I1; Real Execution Only**: PRO-THESIS runs real binaries over real network transports. Fakes and mocks are strictly forbidden **in the system under test** (permitted only inside future Tier A deterministic simulation runtimes).

**I1 is textually scoped to the SUT** ("in the system under test"), so it does not bind PRO-THESIS's own unit tests. That reading is also forced by necessity: Phase 4's benchmark DoD and Phase 5's ddmin correctness cannot be regression-tested in CI if every test costs a 30-second compose cycle; a single ddmin unit test would take hours.

The real risk is not that we write a fake; it is that a fake becomes **reachable from a real run**, letting an agent make the gate pass by pointing `prothesis.yaml` at it.

Related: §4.2 declares `backend: compose | process | k8s | sim` but addendum §K says "Does not require Kubernetes in v1" and §C places Tier A simulation in **v4**.

**Recommendation.**
- I1 binds the SUT only. Record this reading explicitly in `DECISIONS.md` so it is not relitigated.
- The tool's own tests may use a `harness.Backend` fake, constructed **only** by an unexported test constructor. There is no `backend: mock` config value, and the config validator rejects any backend not in `{compose, process}` with exit 5. The fake is unreachable from any `prothesis.yaml`.
- `sim` and `k8s` parse (forward compatibility) but are rejected in v1 with `CONFIG_ERROR`.

---

### Q14: `commit` and `thesis bisect` in a repo with no commits **[RESOLVABLE-WITH-DECISION]**

`prothesis.verdict/v1` carries `"commit": "9c1e0f2"` and addendum §H specifies `thesis bisect WORLD --good SHA --bad SHA`. Measured: the repo exists but `git log` reports *"your current branch 'master' does not have any commits yet"*; there is no HEAD, so `commit` cannot be populated and `bisect` has nothing to bisect.

There is a second, subtler problem: addendum §L requires worlds to be "portable across repos, not tied to one project's layout", but `commit` implicitly assumes a single repo containing both `prothesis.yaml` and the SUT source. For a `.thesis` file shared between repos, `commit` is meaningless or actively misleading.

Also unspecified: addendum §D says shrunk worlds are "**committed to the repo**"; by whom? If `thesis` runs `git add && git commit` itself, an autonomous agent gains commit authority as a side effect of running the gate.

**Recommendation.** (a) `commit` = HEAD of the repository containing `prothesis.yaml`, `""` when unavailable, never fabricated. (b) `thesis bisect` outside a git repo, or with no commits, exits 5 `CONFIG_ERROR` with a clear message. (c) **`thesis` never invokes git for writes.** It writes `.prothesis/regressions/w_<id>.thesis` and prints the `git add` command for a human. Enforcement of append-only-ness comes from the lock manifest (anti-gaming rule 4), which does not require git at all.

---

### Q15: Phase 3's DoD depends on `thesis gate`, a Phase 6 deliverable **[RESOLVABLE-WITH-DECISION]**

Rule 1:
> Under no circumstances may you skip ahead or implement phases out of order.

Phase 3 DoD:
> Any unauthorized manual edit to an oracle script or budget immediately causes **`thesis gate`** to exit with code `4`.

But `thesis gate --profile gate --json` is listed as a **Phase 6** deliverable. Phase 3 cannot satisfy its DoD without violating rule 1.

**Recommendation.** Restate Phase 3's DoD as: *"Any unauthorized edit to an oracle definition, the allowed fault set, or a profile budget causes `thesis oracles verify` and any lock-checking run to exit 4."* Implement lock verification as a **library check** in `internal/lock`, invoked by `run` in Phase 3; Phase 6's `gate` then merely calls the same library. No forward implementation required, and the invariant is enforced from Phase 3 onward.

---

### Q16: Phase 2 has no normative way to execute a scripted fault schedule **[RESOLVABLE-WITH-DECISION]**

Phase 2's DoD requires "Executing a scripted fault schedule". The CLI surface (addendum §H) offers `thesis replay WORLD` (a **Phase 5** deliverable) and `thesis run` has no `--faults` or `--world` flag. Phase 2 can parse the grammar and dispatch faults but has no user-facing way to *invoke* a chosen schedule, so its DoD is untestable.

**Recommendation.** Add `thesis run --faults "<schedule>"` (repeatable) and `thesis run --world FILE` in Phase 2. Rule 2 freezes *schemas*, not the CLI surface; §H is described as "fuller than on-disk", not exhaustive. Record the addition in `DECISIONS.md`. Phase 5's `replay` then becomes a thin alias over `run --world` plus determinism reporting.

---

### Q17: Linearizability checking will blow up on gate-profile histories **[RESOLVABLE-WITH-DECISION]**

Phase 3 requires a checker (Porcupine/Elle/Knossos) over histories from `profiles.gate` = `{clients: 16, ops: 20000, mix: {read, write, txn}}`. Linearizability checking is NP-complete in general. The specific aggravating factor is §4.4's own semantics:
> `info`: State is indeterminate

Every `info` op is a "may or may not have happened" that the checker must branch on. Network partitions and process pauses: i.e. **exactly what PRO-THESIS does for a living**: generate `info` ops in bulk. The tool's core function drives its own checker into its worst case. A 20,000-op, 16-client history with a burst of indeterminate ops around the fault window will exhaust memory or run unbounded.

**Recommendation.** (a) Partition histories **by key** before checking (Porcupine's `Partition`); a per-key register model is the right abstraction for the KV fixture. (b) Impose a hard per-partition timeout and memory ceiling. (c) **A checker timeout is `INCONCLUSIVE` (status `inconclusive`, exit 2), never `PASS`.** A checker that gives up must not be readable as "no violation": that would be the most dangerous silent gate weakening in the system. (d) Cap `ops` for checked profiles, or check only the window around the fault schedule.

---

### Q18: `resource_return_to_baseline` and `no_unbounded_queue` require instrumentation that contradicts §L **[RESOLVABLE-WITH-DECISION]**

Phase 1 mandates:
> 3. `no_unbounded_queue`: Monotonically increasing **queue depth** across `QUIESCE`.
> 4. `resource_return_to_baseline`: RSS, FDs, **goroutines** return to within ±N% of baseline.

Addendum §L states the opposite constraint:
> Must work on **unmodified** systems today (rules out requiring instrumentation)

Goroutine counts require `/debug/pprof` or `expvar`. "Queue depth" is an application-specific metric no unmodified system exposes generically. Two of six mandatory oracles are unimplementable against an unmodified SUT. There is a secondary wrinkle: FD counts need `/proc/1/fd`, and distroless images have no shell for `docker exec ls`.

**Recommendation.** Tiered acquisition with honest degradation:
- **Always available:** RSS and CPU from the cgroup (`docker stats` / a sidecar sharing `pid: "service:<node>"` reading `/proc`). No SUT cooperation, no shell needed.
- **Opt-in:** goroutines and queue depth via a configured `telemetry.endpoint` (pprof/expvar/Prometheus).
- **When a signal is unavailable, the oracle reports `inconclusive` for that dimension** and says which signal was missing. It must **never** silently pass on a dimension it could not measure: that is a gate weakening by omission.
- `thesis doctor` (addendum §C) reports which dimensions are observable; this is precisely its "determinism/observability readiness" role.

---

### Q19: Exit code 3's definition leaves a common outcome unmapped **[RESOLVABLE-WITH-DECISION]**

> **`3`**: `BUDGET_EXHAUSTED`; Budget expired **without coverage progress**; soft warning.

The clause is conditioned on *no coverage progress*. The common case (budget expires, coverage **did** progress, no violation found, but only 22 of 30 planned worlds ran) matches neither code 0 nor code 3 by its own words. Returning 0 (`PASS`) is a lie: "All oracles satisfied across all worlds" is false when 8 planned worlds never ran. This matters because addendum §J puts `thesis gate` in an autonomous merge loop; a truncated run reported as `PASS` merges unverified code.

**Recommendation.** Exit 0 **only** if `worlds_run == worlds_planned` and all oracles passed. Any budget-truncated run without a violation exits 3, with the coverage-progress distinction carried in the verdict body (`coverage.new_templates > 0`) rather than in the exit code. This is a deliberate reading of an ambiguous clause and is recorded as such: it is a *strengthening*, which rule 4 permits.

---

### Q20: `go.mod` pins `go 1.27.1` against a stated target of Go 1.22+ **[RESOLVABLE-WITH-DECISION]**

Measured: `go.mod` contains `go 1.27.1`; the briefing states "Target Go 1.22+"; Go is not yet installed. Under Go 1.21+ toolchain semantics, a `go` directive above the installed toolchain forces an automatic toolchain download (needing proxy access) or hard-fails under `GOTOOLCHAIN=local`. This will bite on the very first `go build`, and pinning the floor at the newest possible patch release forecloses CI on anything older for no benefit.

**Recommendation.** Set `go 1.22`, omit any `toolchain` line. Raise it only when a specific language feature demands it.

---

### Q21: "Build the fixture first" vs. strict phase order **[RESOLVABLE-WITH-DECISION]**, *and the good news*

§7 says:
> 2. Build `testdata/kvfixture/` **first**...
> 3. Progress **strictly** through Phase 0...

But the fixture is a cross-phase artifact. It needs: a compose topology and health endpoints (Phase 0/1), a loadgen emitting normative JSONL with correct `ok`/`fail`/`info` semantics and stable `op_id`s for witnesses (Phase 1/3), **plan-replay mode** (Phase 5, per Q5), a **difficulty knob** (Phase 4, per Q7), and an **injectable clock** (Phase 2, per Q3). Building it "first" necessarily means making commitments for phases 1–5 during Phase 0.

**This is a real tension, but it is smaller than it looks, and the biggest instance of it has been empirically dissolved.**

The task briefing anticipated that the fixture's compose topology would have to commit to the Phase 2 network-fault decision (ambassador proxies vs. tc). **It does not.** Probe 3 above shows a NET_ADMIN sidecar attached at *runtime* via `docker run --network=container:<target>` can inject and cleanly withdraw netem in an **unmodified** target's namespace (8.3 → 311.7 → 6.7 ms, ending at `qdisc noqueue`). The sidecar never appears in the compose file. Phase 0's topology therefore needs **no** fault-related structure at all.

**Recommendation.** Declare in `DECISIONS.md` that phase order governs `internal/` and `cmd/` only; `testdata/` is a test *input*, not a phase deliverable. Then hold the Phase 0 fixture to exactly this minimal forward-looking commitment list; deliberately short:

| # | Phase 0 commitment | Needed by | Cost if deferred |
|---|---|---|---|
| 1 | **One compose service per node** (`kv-n1/2/3`), never `deploy.replicas` | Phase 2 targeting | Node identity becomes ambiguous; rewrite topology + all fault targeting |
| 2 | **No fixed host ports**; ephemeral publish + `docker compose port` | Phase 4 parallelism, Q10 | Parallel worlds collide; 30-min DoD unreachable |
| 3 | **Loadgen `--plan` mode** and `{plan_path}` in the driver template | Phase 5, Q5 | Op-shrinking unimplementable; frozen schema must be reopened |
| 4 | **Separate Raft port from client port** in the fixture | Phase 2, Q1 | Partitions cut the client path too; the lease bug becomes unobservable |
| 5 | **Injectable clock** behind an interface in the fixture | Phase 2, Q3 | `clock.*` untestable even against our own fixture |
| 6 | **Difficulty flag** (`easy`/`hard`) | Phase 4, Q7 | Fixture must be re-tuned mid-benchmark, invalidating trials |
| 7 | `.gitattributes` with `*.thesis binary`, `*.sh eol=lf` | Q11, Phase 0 DoD (b) | Corrupted worlds and false ORACLE_DRIFT after the first cross-platform clone |
| 8 | `oracles.params` + `lifecycle` in the schema | Phase 1 (Q9), Phase 3 lock | Thresholds land in Go source, outside the lock: defeats I6 |

Commitments 3–6 are fixture-internal and cost hours now; each costs days after the schema is frozen or the lock is created.

---

### Q22: "Byte-identical" is undefined in direction and encoding **[RESOLVABLE-WITH-DECISION]**

Phase 0 DoD (b):
> A world configuration round-trips through `.thesis` serialization **byte-identically**.

Three problems.

**(a) Direction is unstated.** `decode(encode(w)) == w` (value fidelity) and `encode(decode(b)) == b` (byte fidelity for *arbitrary* input) are different claims. The second is false for any non-canonical input (a `.thesis` with different whitespace decodes fine but re-encodes differently) and demanding it would require the decoder to preserve formatting.

**(b) The format is undefined.** Neither document specifies `.thesis`'s wire format; it is only named. So Phase 0 chooses both the format **and** the test that validates it: a self-certifying DoD. Note that **YAML forecloses the requirement outright** (comment loss, quoting-style normalization, flow-vs-block ambiguity).

**(c) Integer precision is a live hazard.** History timestamps are `t_ns` (int64). Real Unix epoch nanoseconds in 2026 are ~1.76e18, which **exceeds float64's exact-integer range (2^53 ≈ 9.007e15)**. Any decode path through `map[string]interface{}` converts numbers to float64 and silently corrupts them. (Tellingly, §4.4's own sample uses `t_ns: 1725300000000000` ≈ 1.7e15 (about 20 days after the epoch) so the sample is either relative-to-run-start or has lost three digits. Either way the epoch must be pinned explicitly.)

**Recommendation.** Define the DoD as testable and honest:
1. `.thesis` is **canonical JSON**: UTF-8, LF, lexicographically sorted keys, no HTML escaping (`SetEscapeHTML(false)`; Go's default escapes `<`, `>`, `&`), fixed two-space indent, exactly one trailing `\n`, integers emitted as integers.
2. **Typed structs only.** No `map[string]interface{}` anywhere in the decode path. `witness` (free-form by §4.6) is held as `json.RawMessage` and canonicalized on ingest.
3. The DoD test asserts **`encode(decode(encode(w))) == encode(w)`** (encoder idempotence) **and** `decode(encode(w))` deep-equals `w`, plus a **committed golden `.thesis` fixture** so future encoder changes are caught. The golden file is why `.gitattributes` (Q11) must exist first.
4. Pin `t_ns` = Unix epoch nanoseconds, int64, and treat §4.4's sample magnitude as illustrative.

---

### Q23: Strict lock equality punishes strengthening the gate **[ACCEPT-AND-DOCUMENT]**

Addendum §E.1:
> `thesis gate` recomputes and compares. **Mismatch returns exit 4 and REFUSES TO RUN.**

Strict hash equality is symmetric, so *adding* an oracle, *widening* `allow:`, or *lengthening* a budget also produces exit 4: the anti-gaming mechanism fires hardest on the behavior it exists to encourage. Combined with addendum §I ("ORACLE_DRIFT → stop, escalate to human, NEVER auto-resolve"), an agent that correctly strengthens the gate is halted and escalated.

A second issue: if the lock hashes the whole `prothesis.yaml`, then adding an unrelated node or renaming the service trips `ORACLE_DRIFT` and blocks ordinary development.

**Recommendation.** Keep strict equality: it is simple and unforgeable, and a weaker rule is exactly what an adversarial agent would exploit. Mitigate ergonomically instead: (a) hash a **canonical projection** of the config (allow/deny set, `max_concurrent_faults`, `max_faults_per_world`, per-profile budgets and world counts, `oracles.params`, oracle file digests, regression file list), not the raw file; (b) on mismatch, print a **field-level diff labelling each change `WEAKENING` or `STRENGTHENING`**, so the human review that `--reason` demands takes seconds. Accept that both directions require a lock bump, and document why.

---

## PART 2: DECISIONS.md (conflict-resolution half)

Full entries are in the `decisions` array. Summary of the sixteen bindings:

| # | Decision |
|---|---|
| D1 | `.thesis` = canonical JSON; typed structs only; `witness` as `json.RawMessage`; DoD (b) = encoder idempotence + golden file |
| D2 | `.gitattributes` in commit #1: `*.thesis binary`, `*.sh text eol=lf`, `.prothesis/lock binary` |
| D3 | Network faults via **runtime-attached NET_ADMIN sidecar** sharing the target netns, not ambassadors, not SUT image changes (empirically validated) |
| D4 | `proc.pause` = `docker pause`/`unpause` (cgroup freezer); validated gray failure |
| D5 | Probes = typed union with execution locus; `{host}`/`{port}` resolved per-world via `docker compose port`; no fixed host ports |
| D6 | Fixture built on `hashicorp/raft` + `raft-boltdb`, lease bug authored on top, not a hand-rolled Raft |
| D7 | Driver placeholder vocabulary frozen in Phase 0 as `{history_path} {seed} {profile} {plan_path} {node_addrs}` |
| D8 | Schema gains `oracles.params`, `lifecycle`, node `port`, constraint grammar: all inside the lock projection |
| D9 | Shrinking is separately budgeted and never inline in `gate`; staged reduction replaces raw op-ddmin |
| D10 | Lock hashes a canonical projection, not the raw file; mismatch diff labels WEAKENING vs STRENGTHENING |
| D11 | I1 binds the SUT only; test fake is unreachable from any config; `sim`/`k8s` rejected with exit 5 in v1 |
| D12 | Unsupported faults → `UNSUPPORTED` → exit 2 INCONCLUSIVE, recorded in the world; never silently skipped |
| D13 | All 8 phases emitted; intervals may overlap; `BOOT`/`SEED` negative; epoch = `DRIVE` start |
| D14 | `commit` = HEAD of the config's repo or `""`; `bisect` exits 5 outside git; `thesis` never writes via git |
| D15 | `go 1.22` directive, no `toolchain` line |
| D16 | Fixture difficulty presets `easy`/`hard`, pre-registered per DoD |

### Go sketches for the load-bearing decisions

**D1; canonical world codec** (`internal/recorder/world_codec.go`):

```go
// Package-level guarantee: EncodeWorld is deterministic and idempotent.
//   EncodeWorld(w) == EncodeWorld(must(DecodeWorld(EncodeWorld(w))))
// This is the testable form of Phase 0 DoD (b).

// World is the I2 tuple. Every field is a concrete type: no interface{},
// no map[string]any anywhere reachable from here, so int64 nanosecond
// values never transit float64.
type World struct {
    Schema          string          `json:"schema"`           // "prothesis/v1"
    Seed            uint64          `json:"seed"`
    TopologyVariant string          `json:"topology_variant"`
    DriverProfile   string          `json:"driver_profile"`
    FaultSchedule   []Fault         `json:"fault_schedule"`
    PhaseTimings    []PhaseWindow   `json:"phase_timings"`
    // Provenance is NOT part of the I2 tuple; it is carried so a .thesis
    // is self-contained per I2 and portable per addendum §L.
    Provenance      Provenance      `json:"provenance"`
}

type PhaseWindow struct {
    Phase   Phase `json:"phase"`
    // Epoch is DRIVE start (§4.3). BOOT and SEED are therefore negative.
    // Windows MAY overlap: PERTURB overlaps DRIVE by §4.1. See Q8.
    StartMS int64 `json:"start_ms"`
    EndMS   int64 `json:"end_ms"`
}

func EncodeWorld(w *World) ([]byte, error) {
    var buf bytes.Buffer
    enc := json.NewEncoder(&buf)
    enc.SetEscapeHTML(false) // Go escapes < > & by default; that is not canonical.
    enc.SetIndent("", "  ")
    if err := enc.Encode(w); err != nil { // Encode appends exactly one '\n'.
        return nil, err
    }
    // Struct field order is fixed by declaration order, so no key sorting is
    // needed -- which is precisely why no map may appear in this type graph.
    return buf.Bytes(), nil
}

func DecodeWorld(b []byte) (*World, error) {
    var w World
    dec := json.NewDecoder(bytes.NewReader(b))
    dec.DisallowUnknownFields() // unknown field => CONFIG_ERROR, never silent drop
    dec.UseNumber()             // belt and braces against float64 coercion
    if err := dec.Decode(&w); err != nil {
        return nil, err
    }
    return &w, nil
}
```

**D3; network fault injection via runtime sidecar** (`internal/perturber/faults/net.go`):

```go
// netnsExec runs a command inside the target container's network namespace
// using a throwaway sidecar. The target image is never modified, which is what
// addendum §L ("must work on unmodified systems") requires.
//
// Validated on Docker Desktop 29.1.3 / WSL2 kernel 6.6.87.2:
//   baseline 8.3ms -> netem 300ms -> 311.7ms -> withdraw -> 6.7ms, qdisc noqueue.
func (b *ComposeBackend) netnsExec(ctx context.Context, node string, argv ...string) error {
    cid, err := b.containerID(ctx, node)
    if err != nil {
        return err
    }
    args := append([]string{
        "run", "--rm",
        "--network=container:" + cid,
        "--cap-add=NET_ADMIN",
        b.netadminImage, // thesis-netadmin: alpine + iproute2 + iptables, built once
    }, argv...)
    return b.docker(ctx, args...)
}

// Partition scopes rules to PEER addresses only. A blanket DROP would also cut
// the client path, and the lease-violation stale read is only observable if a
// client can still reach the isolated leader (see Q1).
func (f *Partition) Inject(ctx context.Context, b Backend) error {
    for _, peer := range f.PeerAddrs {
        if err := b.netnsExec(ctx, f.Node,
            "iptables", "-I", "INPUT", "-s", peer, "-p", "tcp",
            "--dport", strconv.Itoa(f.RaftPort), "-j", "DROP"); err != nil {
            return err
        }
    }
    return nil
}

// Withdraw is required to be safe by §4.3. HEAL asserts zero residuals.
func (f *Partition) Withdraw(ctx context.Context, b Backend) error { /* -D each rule */ }

func (f *Partition) VerifyResidual(ctx context.Context, b Backend) error {
    // HEAL: assert `iptables -S INPUT` contains no thesis-owned rules and
    // `tc qdisc show` reports noqueue/pfifo_fast on every managed interface.
}
```

**D12; unsupported faults fail loudly** (`internal/perturber/capability.go`):

```go
// ErrFaultUnsupported maps to exit code 2 (INCONCLUSIVE), never to a skip.
// Silently dropping a fault kind narrows the effective fault space, which
// anti-gaming rule 2 classifies as a lock-level weakening (see Q3, Q18).
type ErrFaultUnsupported struct {
    Kind, Backend, Platform, Reason string
}

func (e *ErrFaultUnsupported) Error() string {
    return fmt.Sprintf("fault %s unsupported on backend=%s platform=%s: %s",
        e.Kind, e.Backend, e.Platform, e.Reason)
}

// Known-unsupported combinations, stated once so `thesis doctor` can report them:
//   clock.skew / clock.jump + any unmodified Go SUT
//     -> LD_PRELOAD needs a dynamic loader (CGO_ENABLED=0 has none); Go's runtime
//        calls the vDSO clock_gettime directly, bypassing libc interposition;
//        CLONE_NEWTIME offsets only CLOCK_MONOTONIC/CLOCK_BOOTTIME, not
//        CLOCK_REALTIME; and Docker does not expose time namespaces.
//   proc.pause / net.* + backend=process on Windows
//     -> no SIGSTOP, no iptables, no tc on the Windows host.
```

---

## PART 3: Scale reality check

### Per-world wall clock, grounded in measurement

The measured floor is **7.19 s** for a 3-container compose `up --wait` + `down -v` with a trivial app, warm images, and no volumes. A real fixture world adds:

| Stage | Estimate |
|---|---|
| `docker compose up` + container start | 3.0 s |
| Go process boot + Raft election + health OK (`BOOT`) | 3.0–5.0 s |
| `SEED` + `steady_state` probe | 1.0–2.0 s |
| `DRIVE` (must exceed election + 5 s lease to expose the bug) | 10–15 s |
| `HEAL` + residual verification | 1.0–2.0 s |
| `QUIESCE` convergence grace | 3.0–5.0 s |
| `ASSERT` (partitioned linearizability check, small history) | 0.2–3.0 s |
| `TEARDOWN` + artifact collection (3× `docker logs`, file writes) | 2.5–4.0 s |
| **Total** | **24–39 s; use 30 s median, 45 s conservative** |

Cold first run adds 60–180 s for the fixture image build (multi-stage `FROM golang:1.22`, so no host Go needed for the fixture).

**Phase 4's 30-minute DoD:** 1800 s ÷ 30 s = **60 worlds** sequential (40 at 45 s). With 3–4 concurrent worlds on 8 vCPU / 32 GB: **120–240 worlds**. The DoD is **reachable**, but only comfortably at the `easy` difficulty preset, which is exactly what makes the "outperforms random" clause unachievable (Q7). Parallel execution is not optional; it is what buys the headroom to run the benchmark at `hard`.

**Phase 4's benchmark:** 10 trials × 2 arms × 30 min = **10 hours of compute** per execution of the DoD.

**Phase 5's shrink:** **9–52 hours** naive, **2.5–4.5 hours** with the staged pipeline (Q6). Either way it is 15–300× the `gate` budget.

### Implementation size

| Phase | LOC | Go files | Focused days |
|---|---|---|---|
| 0: schema, recorder, compose skeleton, CLI, **+ fixture + loadgen** | 5,500–8,000 | 35–55 | 5–9 |
| 1: lifecycle SM, driver supervisor, 6 oracles, verdict engine, telemetry | 3,500–5,000 | 25–35 | 6–10 |
| 2: grammar, scheduler, ~17 fault kinds × 2 backends, resolver, residual verify | 4,000–6,000 | 30–40 | 9–14 |
| 3: external oracle runner, checker wrapper, phase validity, lock | 2,000–3,000 | 15–20 | 5–9 |
| 4: coverage signals, corpus, energy, mutators, benchmark | 3,000–4,500 | 20–30 | 8–13 (+10 h compute) |
| 5: ddmin, narrowing, confirmation, replay/regress/bisect | 2,500–3,500 | 15–25 | 7–11 (+20–60 h validation) |
| 6: gate, MCP server, watch, reports, suspect | 2,500–4,000 | 20–30 | 7–11 |
| **Total** | **23,000–34,000** | **160–235** | **47–77 days** |

Roughly **10–16 focused engineer-weeks**, or **3–5 calendar months** at a normal pace. Phase 0 alone is the largest single phase, because the mandatory fixture (a Raft KV store with an *intentionally* unsound read path plus a normative-JSONL load generator) is bundled into it.

### What "begin immediately" actually delivers

Phase 0's honest deliverable is: a `thesis` binary with `init`, `up`, and `down`; a schema package; a canonical world codec with a golden-file round-trip test; and a 3-node buggy-Raft fixture that boots and tears down under Docker Compose.

It injects **zero** faults, runs **zero** oracles, and produces **zero** verdicts. The first output a coding agent could consume is a Phase 1 deliverable; the first *chaos* is Phase 2. The user should not expect a working chaos tool from the first sitting: they should expect a validated foundation with the eight forward commitments from Q21 already in place, which is what keeps phases 1–5 from requiring a schema reopening.

### Highest-confidence prediction

The two things most likely to consume unplanned days are **not** in the spec's risk profile: (1) the CRLF/`.gitattributes` interaction (Q11), which will surface as an inexplicable `ORACLE_DRIFT` or a container that cannot exec its own health probe, and (2) `{host}:{port}` resolution on Docker Desktop (Q10), which is invisible on Linux CI and fatal on this machine. Both are one-hour fixes **if made in the first commit** and multi-day archaeology if discovered in Phase 3.

