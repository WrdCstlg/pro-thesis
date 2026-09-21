# 02-recorder.design

## Summary

`internal/recorder` rests on three frozen substrates. (1) A virtual clock whose origin is DRIVE start, driven exclusively by Go's monotonic reading (QPC on Windows) with the wall clock captured once as an anchor and never used for arithmetic; fault windows are pre-compiled into a totally ordered event list executed by a single-goroutine sequencer that records planned/initiated/effective times and classifies each run exact/degraded/truncated rather than claiming millisecond precision Docker Desktop cannot deliver. (2) A hierarchical seed tree: HMAC-SHA256 derives per-domain 32-byte keys from the root seed, each key drives a hand-rolled ChaCha20 (RFC 8439), and every extraction primitive (Lemire Uint64n, exact 53-bit Float64, descending Fisher-Yates, integer-ppm weighted choice) is written out in-repo because math/rand's helpers carry no cross-version guarantee; stream independence is structural, since a stream's key is a function of its path alone. (3) A canonical JSON profile that removes floats from `.thesis` entirely (all ratios integer parts-per-million), sorts every object key by byte order, and whose loader re-encodes and byte-compares on every single Load: making byte-identity a runtime invariant, not just a test assertion. `world_hash` is SHA-256 over a frozen projection equal to exactly invariant I2's tuple, so the same world found on two commits dedupes correctly.

## Decisions (19)

### Virtual clock origin is DRIVE start; BOOT and SEED carry negative virtual time

**Choice:** VTime t=0 is stamped at the instant the driver process is spawned. BOOT and SEED are emitted in phases.json and oracle_input.phases with negative start_ms/end_ms. All internal stamping uses RTime (ns since recorder construction); VTime is a pure function VTime = RTime - originRT applied at read time, so nothing is ever rebased.

**Rationale:** Section 4.3 defines fault windows as 'relative to DRIVE start on virtual clock' and the oracle_input example shows DRIVE start_ms: 0. Anchoring at DRIVE makes the fault grammar and the oracle phase array the same coordinate system with zero conversion, and crucially makes a slow BOOT unable to shift the fault schedule -- the origin is stamped when DRIVE actually begins rather than predicted. Stamping everything in one immutable RTime frame avoids the alternative where pre-DRIVE events must be retroactively rebased once the origin is known.

**Rejected:** (a) t=0 at BOOT start: forces every fault window to be converted by an offset only known after BOOT completes, and lets a slow BOOT silently shift every fault. (b) Rebasing pre-DRIVE timestamps at SetOrigin: requires mutating already-written timeline records.

### Wall clock is derived from the monotonic clock, never sampled fresh

**Choice:** NowWall() returns wallBase.Add(NowR()) where wallBase is a single time.Now().Round(0).UTC() captured at construction. A background watcher samples the true wall clock every 5s and records wall-vs-monotonic divergence in clock.json. The monotonic base time.Time is private and is never serialized, never Round(0)'d, and never passed to encoding/json.

**Rationale:** Guarantees non-decreasing timestamps in history.jsonl, which every Jepsen/Elle-family consistency checker requires. A fresh wall read can step backwards on NTP correction, manual clock change, or Hyper-V VM save/restore -- all realistic on a Windows 11 box running Docker Desktop. Go silently strips the monotonic reading on Round(0), MarshalJSON, MarshalText and gob, so a monotonic base stored in a serializable struct degrades to a wall clock with no warning; keeping it private and unserialized is the only reliable defense.

**Rejected:** Sampling time.Now() per stamp: exposes history.jsonl to clock steps, letting a single NTP correction invalidate a consistency verdict. Ignoring wall drift entirely: loses the ability to correlate container log timestamps (from Docker Desktop's Linux VM clock) with the virtual timeline over a multi-hour soak.

### Fault events are pre-compiled into a totally ordered list keyed by (At, Op, Seq)

**Choice:** Compile() sorts faults by their canonical grammar string Fault.String() to assign Seq, emits paired inject/withdraw events, then sorts by (At, Op, Seq) with withdraw ranking before inject at the same instant. A single sequencer goroutine consumes the list in order; actuator calls are offloaded to bounded workers so a slow docker exec cannot stall ordering.

**Rationale:** Two faults scheduled at the same millisecond would otherwise fire in slice-append or map iteration order, which is not a function of the world -- that alone would make I2 ('every failure is a seed') false. Sorting by canonical text makes Seq a pure function of the world. Ranking withdrawals first at an instant means max_concurrent_faults is never transiently exceeded at a window boundary. Offloading dispatch is required because docker exec into a container on Docker Desktop costs 100-800ms.

**Rejected:** One goroutine per fault sleeping until its window: makes ordering at equal timestamps depend on the Go scheduler, and makes withdrawal-on-abort unreliable because a cancelled goroutine may never reach its withdraw call.

### Three timestamps recorded per fault: planned, initiated, effective

**Choice:** timeline.jsonl records planned_v_ms (from the world), initiated_v_ms (when the sequencer woke and dispatched) and effective_v_ms (when the actuator confirmed the fault is in force), plus late_ms and actuator_latency_ms. The verdict's causal_timeline is built from effective_v_ms.

**Rationale:** On Docker Desktop the host cannot honor a millisecond-precision fault window: injecting an iptables rule via docker exec takes hundreds of milliseconds. Reporting that a partition began at exactly 8400 when the rule landed at 8612 is a small lie that destroys a debugging tool's credibility, and it would make a shrunk repro's timing window meaningless. Recording all three keeps the report honest and gives Phase 5's binary-search window narrowing real data to work from.

**Rejected:** Recording only the planned time: hides real injection latency and makes causal timelines subtly wrong. Recording only the effective time: loses the scheduler lateness measurement that feeds the fidelity classification.

### Runs are classified exact / degraded / truncated, and a failed injection can never PASS

**Choice:** Every run carries a Fidelity. Injects later than 250ms degrade it; injects past their end_ms are skipped along with their paired withdraw and degrade it; budget expiry or cancellation truncates it. A withdraw is NEVER skipped regardless of lateness. An Actuator.Do error, a residual fault at HEAL, or a clock anomaly makes the run INCONCLUSIVE (exit 2) rather than letting it report PASS.

**Rationale:** Silently running fewer faults than the world specifies and then reporting PASS is exactly the gate-weakening failure mode invariant I6 exists to prevent, and it would be invisible in the verdict. Making injection failure INCONCLUSIVE rather than PASS fails closed. Phase 5's confirmation gate can then count only 'exact' executions toward reproduced: k/k, so that number measures the system under test rather than the host's scheduling mood.

**Rejected:** Treating a missed fault as a no-op and continuing to a PASS verdict: converts an environment problem into a false green, the worst possible outcome for an agentic gate.

### Withdrawals drain on a detached context

**Choice:** Sequencer.drain() is deferred on every exit path and builds its own context.WithTimeout(context.Background(), 30s) rather than using the run context.

**Rationale:** If drain used the run context, a Ctrl-C or budget expiry would cancel the withdrawals too, leaving iptables rules installed and containers SIGSTOP'd on the developer's machine after the tool exits. Section 4.3's 'every fault must implement a safe withdraw()' guarantee and the HEAL zero-residual assertion both depend on withdrawals surviving cancellation of the thing that scheduled them.

**Rejected:** Using the run context: correct-looking and catastrophic in practice. An os/signal handler instead: does not cover budget expiry or panics, and races with Go's default signal disposition.

### Sleep to within a measured spin threshold, then busy-yield; do not call timeBeginPeriod

**Choice:** SleepUntilR sleeps until spinNs before the deadline, then loops on runtime.Gosched() until NowR() >= target. spinNs defaults to 2ms and is set from a startup calibration of 20 x 1ms sleeps; it is 0 under simClock. timeBeginPeriod is not called.

**Rationale:** Windows default timer granularity is ~15.6ms; Go 1.16+ uses high-resolution waitable timers on Win10 1803+ for roughly 1ms, but that is a runtime implementation detail rather than a contract, so sleeping the full remaining duration can overshoot a deadline by a whole tick -- a 6% timing error against a 250ms window. With at most 48 events per world the spin costs at most ~96ms of one core across a 10-minute world. timeBeginPeriod is process-global, perturbs system-wide power management, and requires golang.org/x/sys/windows.

**Rejected:** Plain time.Sleep to the deadline: up to 15.6ms overshoot per event. Calling timeBeginPeriod(1): a global side effect on the user's machine for a marginal gain, plus a dependency.

### Seed streams derive from an HMAC-SHA256 key tree over ChaCha20, with all value extraction written out in-repo

**Choice:** RootKey = HMAC-SHA256(be64(seed), "prothesis/v1\x00root"); named children via HMAC-SHA256(parent, 0x01||name); indexed children via HMAC-SHA256(parent, 0x02||be64(i)). Each 32-byte key drives a hand-implemented ChaCha20 (RFC 8439, 20 rounds). Uint64 (little-endian), Uint64n (Lemire multiply-shift with full rejection via bits.Mul64), Float64 (float64(u>>11) * 2^-53), Shuffle (descending Fisher-Yates) and WeightedPPM (integer-only) are all specified and implemented here rather than delegated.

**Rationale:** math/rand's derived helpers carry no cross-version stability guarantee and Shuffle has changed; math/rand/v2 documents that its algorithms may change. HMAC-SHA256 and ChaCha20 are frozen by RFC 2104/FIPS 180-4 and RFC 8439 and reproducible in any language, which CRUCIBLE PART 10 requires for portable worlds. Stream independence becomes structural rather than disciplinary: a stream's key is a function of its path in the tree alone, so consumption from one stream provably cannot shift another. ChaCha's counter-addressability gives O(1) Seek and 2^96 free sub-streams per key via the nonce.

**Rejected:** math/rand.NewSource: the source sequence has been stable in practice but the helpers are not, and Go reserves the right to change both. math/rand/v2 ChaCha8: a named type, but Uint64N's reduction is not frozen by the API. golang.org/x/crypto/chacha20: a dependency where 60 lines verified against RFC test vectors is safer. splitmix64: fast and simple, but not counter-addressable, so index derivation would cost a rehash.

### Anything a shrinker may delete draws from an index-derived sub-stream, never sequentially

**Choice:** Per-operation, per-fault, per-client and per-world randomness is drawn from stream.Index(ordinal), whose keystream depends only on the ordinal.

**Rationale:** Phase 5's ddmin deletes operation #7 from a 20,000-op workload. Under sequential consumption that shifts the randomness of ops #8..#20000, so the shrinker would minimize against a moving target and could never converge. Index derivation makes deletion of one element invisible to every other element. This is the recorder decision Phase 5 most depends on, which is why it is fixed in Phase 0.

**Rejected:** Sequential draws with a replay of consumed counts: fragile, requires recording draw counts per element, and breaks the moment a code path changes how many values it consumes.

### .thesis contains no floating-point numbers at all

**Choice:** Every ratio is an integer in parts-per-million (loss_ppm, rate_ppm, mix_ppm), every duration an integer in ms, every size an integer in bytes. The root seed is a 16-hex-digit string. Driver mix weights are normalized to sum to exactly 1,000,000 by largest-remainder apportionment in sorted-key order at config load. cjson.Lint rejects any schema type containing float32 or float64. Where the spec genuinely shows a float (verdict suspect.confidence), schema.Dec serializes an exact decimal as a string at a per-field declared scale.

**Rationale:** This single decision eliminates, at once: float formatting and shortest-round-trip algorithm dependence, -0.0 vs 0.0, NaN/Inf, and the integer-vs-float ambiguity JSON numbers otherwise create on decode. It also removes libm from the determinism boundary. Integer-only weights additionally close a real trap: float addition is not associative, so a cumulative sum over a Go map's randomized iteration order would select different buckets at boundaries run to run.

**Rejected:** Canonical float encoding via strconv.FormatFloat(f,'e',17,64): byte-stable and round-trip-exact, but unreadable and still raises NaN/Inf questions. Encoding float64 as its IEEE-754 hex bit pattern: bulletproof but defeats the human reviewability committed regression worlds require. RFC 8785's ECMAScript number rule: differs from Go's strconv at exponent boundaries (1e-07 vs 1e-7).

### All object keys sorted by UTF-8 byte order, including struct fields

**Choice:** The encoder sorts every object's keys ascending by byte sequence, applying the same rule to struct fields and map keys. Keys must match ^[a-z][a-z0-9_]*$.

**Rationale:** One rule to state, test and reimplement, rather than two. Sorting struct fields means a developer reordering a Go struct is a no-op instead of silently rehashing every world in the regression corpus. Constraining keys to lowercase ASCII identifiers makes byte order and RFC 8785's UTF-16 code-unit order coincide, so we are JCS-compatible on ordering without importing anything and without a Unicode normalization question.

**Rejected:** Struct declaration order: stable across builds but fragile against refactoring, and needs a second rule for maps. Preserving input order: not canonical at all.

### The .thesis loader re-encodes and byte-compares on every load, and rejects non-canonical input

**Choice:** DecodeWorld parses under the strict grammar, unmarshals, re-encodes, and fails with ErrNonCanonical unless the result is bytewise equal to the input; it then recomputes and compares world_hash. Only `thesis world canonicalize` may pass allowNonCanonical.

**Rationale:** Byte-identity stops being an assertion in a test suite and becomes a runtime invariant checked for every regression world on every gate invocation. The alternative -- silently normalizing a hand-edited world -- is an I6 hole: the file would rewrite to a different world_hash and the regression corpus would drift with nobody noticing. Cost is a few microseconds on a file of a few kilobytes.

**Rejected:** Lenient parsing with normalization on write: silent corpus drift. Parser-only enforcement without the re-encode check: leaves any encoder bug undetected in production.

### world_hash covers a frozen projection equal to invariant I2's tuple, not the whole file

**Choice:** world_hash = SHA-256("prothesis.world/v1\n" || canonical_encoding(project(world, {driver_profile, fault_schedule, phase_timings, seed, topology_variant}))). The origin block (commit, config_hash, created_by, tool_version), notes, schema and world_hash itself are excluded. Byte-identity still covers the entire file.

**Rationale:** Resolves the self-reference problem cleanly, and more importantly makes the same logical world discovered on two different commits hash identically -- without which the search corpus cannot dedupe and the regression corpus grows without bound. The projection is literally I2's stated tuple, so a world's identity is exactly what the invariant says it is. The domain-separating prefix stops a world hash colliding with any other document's hash.

**Rejected:** Hashing the whole file: makes world identity depend on the commit that found it. Not storing the hash in the file: a committed regression could not self-attest and a reviewer could not see its identity in the diff.

### A .gitattributes marking *.thesis as -text is a required deliverable, and the parser reports CRLF specifically

**Choice:** Ship .gitattributes with `* text=auto eol=lf` plus `*.thesis -text`, `*.jsonl -text`, `*.json -text`. All recorder file I/O is binary. cjson.Parse detects \r\n and reports a line-endings-specific error naming .gitattributes and core.autocrlf.

**Rationale:** Go never translates line endings but git does. A .thesis committed on Linux and checked out on Windows with default core.autocrlf=true arrives with CRLF, fails the strict parser, and breaks every world_hash in the corpus. Phase 5 commits shrunk regressions to the repo, so this sits on the critical path for the tool's core value proposition, and on Windows it is the default configuration rather than an edge case.

**Rejected:** Accepting CRLF in the parser and normalizing: reintroduces silent hash drift. Relying on developers configuring git correctly: not a control.

### run_id keeps the exact 4-hex spec shape with mkdir-based uniqueness; world filenames use 12 hex

**Choice:** run_id matches ^r_[0-9]{4}_[0-9]{2}_[0-9]{2}_[0-9a-f]{4}$ with a UTC date and crypto/rand hex, made unique by os.Mkdir returning ErrExist and retrying up to 64 times. World files are w_<12 hex chars of world_hash>.thesis.

**Rationale:** run_ids are allocated with the artifacts directory right there, so mkdir-EEXIST is a free exact uniqueness test and the normative 4-hex shape survives even for a `thesis watch` daemon doing hundreds of runs a day. World files are content-addressed and merged across git branches, so they cannot be uniquified by retry: 16 bits collides at roughly 300 files by the birthday bound and a regression corpus will exceed that. UTC dates keep runs sorting coherently across time zones and CI regions.

**Rejected:** 4 hex for world files to match the illustrative w_a41f.thesis: a corpus-corrupting collision risk. Deriving run_id from the seed: two runs of the same world are two runs and must have distinct ids.

### World directories are keyed by ordinal, not by world_hash

**Choice:** worlds/<6-digit ordinal>/ with a world_hash file inside, plus worlds/index.jsonl mapping ordinal -> world_hash -> outcome.

**Rationale:** Phase 5's confirmation gate executes the same world k times (default 3), so world_hash is not unique within a run and cannot be a directory key. Ordinal directories give a stable, sortable layout while index.jsonl preserves fast hash lookup without walking the tree.

**Rejected:** Hash-keyed directories: collide under the k-times confirmation protocol. Hash-plus-attempt-suffix: encodes execution history in a path that should be purely positional.

### bundle_sha is computed over the manifest, never over a compressed archive

**Choice:** MANIFEST.json lists every file with size and sha256; bundle_sha is SHA-256 over the canonical encoding of that sorted list. Transport archives use archive/tar + compress/gzip with normalized entries (mtime 0, uid/gid 0, fixed modes, sorted paths, USTAR, gzip header with no name and zero mtime) and are verified against the manifest rather than by their own bytes.

**Rationale:** compress/flate's output is not guaranteed stable across Go versions, so hashing compressed bytes would make bundle identity depend on the toolchain. Hashing the manifest sidesteps that entirely while still giving an end-to-end content address, and it is the natural hook for CRUCIBLE PART 6.3 mechanism 5 (verdict signing, v3) with no redesign.

**Rejected:** Hashing the .tar.gz: toolchain-dependent. Hashing the uncompressed tar stream: stable, but requires materializing a tar just to compute an identity.

### history.jsonl is exempt from the CJSON canonical contract

**Choice:** CJSON governs .thesis, run.json, verdict.json, phases.json, timeline.jsonl, streams.json and MANIFEST.json. history.jsonl is ingested with json.Decoder + UseNumber into exact int64 and its integrity is covered only by MANIFEST.json.

**Rationale:** history.jsonl is produced by an external driver process specified by driver.cmd, so its byte layout is not ours to dictate; requiring canonical form would violate CRUCIBLE PART 10's requirement to work on unmodified systems. Using UseNumber rather than the default float64 decode is essential because the normative t_ns field carries absolute Unix nanoseconds, which exceed 2^53 and would lose precision through a double.

**Rejected:** Requiring drivers to emit canonical JSONL: rules out every existing Jepsen-style loadgen. Rewriting history into canonical form on ingest: destroys the original artifact and hides driver bugs.

### The determinism source scanner is built in Phase 0 and shared with thesis doctor

**Choice:** internal/recorder/determinism.go uses go/parser and go/ast to find time.Now/Since/After/Sleep/Tick/NewTimer, math/rand, crypto/rand and order-dependent map iteration. determinism_scan_test.go fails the build if any appear outside clock_real.go and runid.go. thesis doctor later runs the identical scanner over the user's repo.

**Rationale:** The virtual clock is only real if nothing bypasses it, and a source-level ban is far more reliable than a code-review convention. Building it in Phase 0 costs about a hundred lines and makes CRUCIBLE PART 5's thesis doctor a CLI wrapper in a later phase rather than a new subsystem, while immediately protecting the substrate the whole tool's credibility rests on.

**Rejected:** A linter config or a review checklist: not enforced by the build. Deferring the scanner to the phase that ships thesis doctor: leaves Phases 0-5 free to leak ambient time and randomness into the very layers whose determinism is being claimed.

## Open questions (7)

### [RESOLVABLE-WITH-DECISION] History log field t_ns: the field name says nanoseconds, the normative example values are microseconds

Section 4.4 names the field t_ns and shows the value 1725300000000000. Read as nanoseconds since the Unix epoch that is 1,725,300 seconds, i.e. 1970-01-20 -- clearly wrong. Read as MICROseconds it is exactly 2024-09-02T18:00:00.000Z, a suspiciously round instant, and the inter-event deltas become plausible latencies (410000 -> 410ms for a write, 1010000 -> 1.01s between operations) rather than implausible ones (0.41ms, 1.01ms). Verified by computation on this machine. Every example line in the spec is affected. A driver written against the field name and a driver written against the example values disagree by a factor of 1000, and any consistency checker comparing them silently produces nonsense.

**Recommendation:** Treat the field NAME as normative -- t_ns is nanoseconds since the Unix epoch -- and the example values as an authoring error, because the name is what every parser and driver author keys on. Emit true nanoseconds from the built-in kvfixture loadgen. Add a non-silent ingest lint: any t_ns implying a date before 2001-01-01 (value < 1e18) raises a loud warning naming this ambiguity, rather than being heuristically multiplied by 1000. Note separately that absolute Unix nanoseconds exceed 2^53, so history.jsonl must be parsed with exact int64 semantics (json.Decoder + UseNumber, jq 1.7+); consumers that parse JSON numbers as doubles will silently corrupt these values. Record the resolution in DECISIONS.md and correct the examples in any regenerated spec copy.

### [ACCEPT-AND-DOCUMENT] Byte-identical round-trip is impossible for 'all valid worlds' unless valid is defined as canonical

The Phase 0 definition of done states that a world configuration must round-trip through .thesis serialization byte-identically. Read as 'for every parseable JSON document' this is mathematically impossible: {"a":1,"b":2} and {"b":2,"a":1} denote the same world and no single encoder can reproduce both. The spec never defines what makes a .thesis file valid, so the requirement has no well-defined success criterion as written.

**Recommendation:** Define the .thesis canonical form explicitly (the eight CJSON rules) and define a valid .thesis file as exactly the output of the canonical encoder. Make the loader REJECT non-canonical input with ErrNonCanonical rather than silently normalizing it, and ship `thesis world canonicalize` as the sanctioned repair path. This makes the definition of done both well-defined and strictly stronger than the literal reading, and closes an I6 hole: silent normalization would let a hand-edited regression world rewrite to a different world_hash with nobody noticing. Verified continuously by re-encoding and byte-comparing on every load, not only in tests.

### [RESOLVABLE-WITH-DECISION] Invariant I2's world tuple names phase_timings, which is simultaneously an input and an output

I2 defines the serializable world tuple as (seed, topology_variant, driver_profile, fault_schedule, phase_timings). But phase timings are also a RESULT of execution: DRIVE planned for 42000ms may observably run 44300ms because the driver was slow. If observed timings are stored in the world, every replay produces a different world_hash and the world can never be replayed or deduped -- defeating the invariant the field is part of. If only planned timings are stored, oracles cannot see real phase boundaries and invariant I5 (phase-aware assertion) becomes unsound.

**Recommendation:** Split the concept. schema.PhasePlan (planned budgets: drive_ms, heal_ms, quiesce_ms and the BOOT/SEED/ASSERT timeouts) lives in .thesis, is hashed into world_hash, and is the I2 input. []schema.PhaseSpan (observed boundaries in virtual ms) lives in phases.json and run.json, is NOT hashed, and is what prothesis.oracle_input/v1's phases array is built from -- so oracles always assert against real boundaries. Record in DECISIONS.md that 'phase_timings' in I2 is read as the planned plan.

### [BLOCKING] The process backend cannot implement proc.pause, net.partition or clock.skew on Windows

Section 4.3 makes proc.pause (SIGSTOP/SIGCONT) 'top priority for producing gray failures' and requires net.partition via iptables and tc/netem, and clock.skew via libfaketime or time namespaces. None of these exist on the Windows host. Windows has no SIGSTOP (SuspendThread/NtSuspendProcess is not equivalent and is not exposed by Go's os/exec), no iptables, no netem, and no time namespaces. Docker Desktop runs containers inside a Linux VM where all of these ARE available, so the compose backend is unaffected -- but the process backend on this machine cannot inject the single most important fault kind in the specification.

**Recommendation:** For the process backend on Windows, declare it capability-limited rather than partially working. Publish a capability matrix (backend x fault kind x host OS) and have the perturber refuse to compile a world requiring an unsupported fault, exiting 5 (CONFIG_ERROR) at compile time or 2 (INCONCLUSIVE) at run time -- NEVER silently omitting the fault and reporting PASS, which is exactly the gate weakening I6 forbids. Use the compose backend for all Windows development and for the Phase 2 definition of done. Have `thesis doctor` report the matrix for the host it runs on. Revisit for a WSL2-hosted process backend later if a Windows-native process backend is genuinely wanted.

### [ACCEPT-AND-DOCUMENT] Millisecond-precision fault windows cannot be honored on Docker Desktop

The fault grammar specifies windows to the millisecond (proc.pause(role:leader)@8200..15100) and the Phase 2 definition of done depends on a 200ms offset between two overlapping faults (8200 vs 8400). On this host, injecting a fault means docker exec into a container inside Docker Desktop's Linux VM, which costs 100-800ms end to end -- larger than the offset the definition of done relies on. The spec's timing model implicitly assumes injection is instantaneous.

**Recommendation:** Do not pretend to millisecond precision. Record planned_v_ms, initiated_v_ms and effective_v_ms for every fault event and build the verdict's causal_timeline from effective_v_ms. Classify each run's Fidelity as exact/degraded/truncated and count only 'exact' executions toward Phase 5's reproduced: k/k. Measure actuator latency during `thesis doctor` and set min_window_ms (default 250ms) from the measurement, rejecting worlds with shorter windows at compile time. Where sub-100ms precision is genuinely needed, prefer a per-edge ambassador proxy (already listed as an alternative in section 4.3) over docker exec, since a proxy can be pre-warmed and toggled in microseconds -- flag this as the Phase 2 implementation to prefer for net.* faults.

### [ACCEPT-AND-DOCUMENT] World filename w_a41f gives only 16 bits of collision resistance

The verdict schema example shows .prothesis/regressions/w_a41f.thesis, a 4-hex-character short hash. CRUCIBLE PART 5.2 requires the regression corpus to be committed to the repo, appended to forever, and run on every gate invocation. Under the birthday bound 16 bits collides at roughly 300 files, and regression files are content-addressed and merged across git branches, so a collision cannot be resolved by retrying with new entropy the way a run_id can.

**Recommendation:** Use 12 hex characters (48 bits, safe to roughly 16 million files) for world filenames: w_a41f9c2b7e10.thesis. Accept any w_<hex>.thesis on read so the spec's example remains loadable. Keep run_id at the exact 4-hex normative shape, since run directories are allocated against a live directory and os.Mkdir returning ErrExist provides free exact uniqueness. Record the deviation in DECISIONS.md; the spec value is an illustrative example rather than a stated format.

### [RESOLVABLE-WITH-DECISION] The seed stream domain table is a reproducibility contract with no home in the lock manifest as specified

CRUCIBLE PART 6.3 mechanism 1 says .prothesis/lock holds a hash manifest of every oracle definition, the allowed fault space, and profile budgets. It does not mention PRNG derivation. But renaming a stream domain string, changing the HMAC construction, or altering a value-extraction primitive would silently change every world's behavior while leaving every world_hash unchanged -- invalidating the entire committed regression corpus with no signal. That is a strictly larger blast radius than weakening one oracle, which the lock does cover.

**Recommendation:** Extend the Phase 3 lock manifest to cover a recorder determinism digest: the sorted stream domain table, a version tag for the derivation construction, and the hashes of testdata/streams/*.json (frozen stream goldens) and testdata/world_hashes.json (frozen encoder goldens). Any change to seed derivation, value extraction or canonical encoding then requires `thesis oracles lock --reason` as a separate human-reviewed commit, exactly like weakening an oracle. Note the extension in DECISIONS.md so Phase 3 does not omit it; the recorder ships the digest function in Phase 0 and Phase 3 only wires it into the manifest.

## Risks

- The CRLF trap is the most likely thing to actually break this project. The repo is not yet a git repo, so .gitattributes must land in the first commit. If a .thesis is ever committed without it, a Windows checkout with default core.autocrlf=true silently rewrites every line ending, the strict parser rejects the file, and every world_hash in the regression corpus breaks at once. The failure surfaces long after the mistake, in someone else's clone.
- Scope creep out of Phase 0 through the sequencer. The sequencer is the natural place to start writing fault code and the Actuator interface makes it easy. Phase 0 must ship only the ordered event loop plus a NopActuator; the moment a docker exec appears in internal/recorder, the recorder's zero-dependency property (stdlib + pkg/schema only) is gone and its tests stop being fast and hermetic.
- A future schema field reintroduces a float. Every byte-identity guarantee here rests on .thesis containing no floating-point numbers. A `Confidence float64` or `Rate float64` added during Phase 4's search work would compile, pass casual review, and silently make world_hash platform-dependent. cjson.Lint over every registered type is the only real defense, and it only works if new types are actually registered -- the registration list needs a test that enumerates the package's exported types rather than a hand-maintained slice.
- Docker Desktop actuator latency may exceed the fault window offsets Phase 2's definition of done depends on. The DoD requires proc.pause@8200 overlapping net.partition@8400 -- a 200ms gap against 100-800ms docker exec latency. If the fixture's stale-read bug needs that ordering precisely, docker exec may never reproduce it and the ambassador-proxy approach becomes mandatory rather than optional. Worth measuring in Phase 0 with a throwaway timing harness before committing to the docker exec path.
- Go is not installed yet, so none of this is compiled. The ChaCha20 block function, the Lemire rejection bound and the reflection codec are all specified precisely enough to be wrong in a way that is deterministic AND biased -- the worst failure mode, because every determinism test would still pass while the distribution is skewed. The RFC 8439 vectors and the chi-square sanity check exist to catch exactly this and should be the first tests written, before any world serialization work.
- The re-encode-and-compare-on-every-load invariant makes DecodeWorld strictly stricter than most developers expect. The first time someone hand-edits a regression world to try a variation they get ErrNonCanonical rather than a working experiment. `thesis world canonicalize` must exist and must be named in the error message from day one, or this correct design will be experienced as an obstruction and someone will add a lenient flag that defeats it.
- Negative start_ms for BOOT and SEED in prothesis.oracle_input/v1's phases array deviates from the spec's illustrative example. Third-party oracle authors may assume non-negative milliseconds and index arrays with them. This needs to be stated prominently in the external oracle documentation when Phase 3 arrives, not discovered by an oracle author at runtime.
- Retention deletion on Windows can fail because Docker Desktop or an editor holds a handle on a log file inside a run directory. The rename-to-trash fallback covers the common case, but a .trash directory that never drains quietly consumes disk across a long soak. It needs an explicit sweep at the start of every run, not only at teardown.

## Files implied (37)

- `C:\AI Projects\Pro-synthesis\pkg\schema\scalars.go`: HexU64, Dec, Hash and Seed named types -- the string-backed scalars that keep every number in a canonical document safe for JSON/JavaScript consumers. _(0)_
- `C:\AI Projects\Pro-synthesis\pkg\schema\world.go`: World, Fault, PhasePlan, DriverProfile, TopoVariant, WorldOrigin -- the .thesis types, plus Fault.String() rendering the normative KIND(TARGET,PARAMS)@START..END grammar. _(0)_
- `C:\AI Projects\Pro-synthesis\pkg\schema\phase.go`: The eight lifecycle phase constants and PhaseSpan (observed boundaries in virtual ms) feeding prothesis.oracle_input/v1's phases array. _(0)_
- `C:\AI Projects\Pro-synthesis\pkg\schema\streams.go`: Frozen PRNG stream domain constants and RegisteredStreams -- part of the reproducibility contract, later hashed into .prothesis/lock. _(0)_
- `C:\AI Projects\Pro-synthesis\pkg\schema\run.go`: RunRecord, Anchor, ClockHealth, Fidelity, StreamAudit and Manifest -- run-level artifact types that are outputs rather than world inputs. _(0)_
- `C:\AI Projects\Pro-synthesis\pkg\schema\history.go`: History JSONL line types for ingest only (t_ns, process, type, f, key, value, op_id), decoded with exact int64 semantics. _(0)_
- `C:\AI Projects\Pro-synthesis\internal\recorder\cjson\value.go`: The canonical value tree (Obj/Arr/Str/Int/Bool) with no Null and no Float, plus Project and Delete used by world hashing. _(0)_
- `C:\AI Projects\Pro-synthesis\internal\recorder\cjson\encode.go`: The canonical encoder: byte-sorted keys, integer-only numbers, minimal escaping with no HTML escaping, 2-space indent, single trailing LF. _(0)_
- `C:\AI Projects\Pro-synthesis\internal\recorder\cjson\parse.go`: The strict parser rejecting duplicate keys, unsorted keys, floats, exponents, leading zeros, -0, null, BOMs, CRLF and invalid UTF-8, with line/column errors. _(0)_
- `C:\AI Projects\Pro-synthesis\internal\recorder\cjson\reflect.go`: Reflection Marshal/Unmarshal driven by cjson struct tags, with memoized per-type field plans. _(0)_
- `C:\AI Projects\Pro-synthesis\internal\recorder\cjson\lint.go`: Lint rejecting float32/float64, interface{}, pointers, non-string map keys, time.Time and bad key names -- the guard that keeps byte-identity true as the schema grows. _(0)_
- `C:\AI Projects\Pro-synthesis\internal\recorder\clock.go`: RTime, VTime, the Clock interface, and Timeline (the single RTime-to-VTime conversion constant). _(0)_
- `C:\AI Projects\Pro-synthesis\internal\recorder\clock_real.go`: Tier B clock: monotonic-only arithmetic, derived wall time, sleep calibration, spin-tail SleepUntilR, and the wall-vs-monotonic drift watcher. _(0)_
- `C:\AI Projects\Pro-synthesis\internal\recorder\clock_sim.go`: Deterministic clock for tests today and the Tier A (v4) simulation seam tomorrow; identical interface, manual Advance. _(0 (Tier A seam))_
- `C:\AI Projects\Pro-synthesis\internal\recorder\sequencer.go`: Compile (pure world-to-event-list), the Actuator interface, the ordered event loop, late policy, fidelity classification, and detached-context drain. _(0 (perturber seam))_
- `C:\AI Projects\Pro-synthesis\internal\recorder\phases.go`: PhaseRecorder tracking entry/exit, soft and hard phase budgets, overrun events, and the observed []PhaseSpan written to phases.json. _(0)_
- `C:\AI Projects\Pro-synthesis\internal\recorder\chacha.go`: The RFC 8439 ChaCha20 block function in pure Go, ~60 lines, verified against the RFC test vectors. _(0)_
- `C:\AI Projects\Pro-synthesis\internal\recorder\stream.go`: StreamKey, the RootKey/Derive/DeriveIndex HMAC-SHA256 tree, Stream with Index/Seek, and the frozen Uint64/Uint64n/Float64/Shuffle/WeightedPPM primitives. _(0)_
- `C:\AI Projects\Pro-synthesis\internal\recorder\streams.go`: Per-world Streams registry with memoization, panic on unregistered domains, and the draw-count audit written to streams.json. _(0)_
- `C:\AI Projects\Pro-synthesis\internal\recorder\world.go`: EncodeWorld, DecodeWorld (with the re-encode-and-byte-compare runtime invariant), HashWorld over the frozen I2 projection, and Load/Save. _(0)_
- `C:\AI Projects\Pro-synthesis\internal\recorder\bundle.go`: Bundle and Appender: atomic tmp+fsync+rename JSON writes, crash-safe append-only logs, WorldDir allocation, and MANIFEST.json generation. _(0)_
- `C:\AI Projects\Pro-synthesis\internal\recorder\runid.go`: AllocRunID producing r_YYYY_MM_DD_xxxx from a UTC date and crypto/rand, made unique by os.Mkdir ErrExist retry. _(0)_
- `C:\AI Projects\Pro-synthesis\internal\recorder\retain.go`: retain_passing/retain_failing pruning ordered by started_wall_ns, never pruning runs referenced by regressions, with a Windows rename-to-trash fallback. _(0)_
- `C:\AI Projects\Pro-synthesis\internal\recorder\pathsafe.go`: SafeName validating path components against Windows reserved device names, trailing dots and spaces, and the allowed character class. _(0)_
- `C:\AI Projects\Pro-synthesis\internal\recorder\determinism.go`: go/ast scanner for ambient nondeterminism (time.Now, math/rand, order-dependent map iteration); used by the build-failing self-test now and by thesis doctor later. _(0 (doctor seam))_
- `C:\AI Projects\Pro-synthesis\internal\recorder\recorder.go`: The composition root wiring Clock, Timeline, Streams, Bundle and Sequencer into the single object every other subsystem receives. _(0)_
- `C:\AI Projects\Pro-synthesis\internal\recorder\cjson\encode_test.go`: Golden-corpus byte-identity tests, map-order chaos repetition, and byte-mutation testing of golden files. _(0)_
- `C:\AI Projects\Pro-synthesis\internal\recorder\cjson\fuzz_test.go`: FuzzWorldRoundTrip -- because DecodeWorld rejects non-canonical input, the fuzzer explores exactly the canonical language and directly tests the guarantee. _(0)_
- `C:\AI Projects\Pro-synthesis\internal\recorder\cjson\lint_test.go`: Runs Lint over every registered schema type -- the highest-value test in the recorder, because the risk to byte-identity is the field someone adds in Phase 4. _(0)_
- `C:\AI Projects\Pro-synthesis\internal\recorder\world_property_test.go`: Stream-driven world generator asserting idempotence, semantic round-trip, hash stability and injectivity over 100k seeds -- dogfooding I2 in the recorder's own tests. _(0)_
- `C:\AI Projects\Pro-synthesis\internal\recorder\stream_test.go`: RFC 8439 vectors, frozen stream goldens, the stream-independence test, Seek/Index equivalence, and chi-square/monobit sanity checks. _(0)_
- `C:\AI Projects\Pro-synthesis\internal\recorder\sequencer_test.go`: Conformance suite run against both clocks: event ordering, no early fire, withdraw-always-fires, drain-under-cancellation, and every row of the overrun table. _(0)_
- `C:\AI Projects\Pro-synthesis\internal\recorder\determinism_scan_test.go`: Fails the build if time.Now/Since/After/Sleep/Tick or math/rand appears outside clock_real.go and runid.go. _(0)_
- `C:\AI Projects\Pro-synthesis\internal\recorder\testdata\worlds\*.thesis`: Committed canonical golden worlds spanning zero faults, 24 faults, every kind, unicode strings, and 2^53 boundary integers. _(0)_
- `C:\AI Projects\Pro-synthesis\internal\recorder\testdata\streams\root_002a.json`: Frozen per-domain PRNG goldens (Uint64, Uint64n, Float64 as IEEE-754 hex bit patterns, Shuffle, WeightedPPM) run on every Go version in CI. _(0)_
- `C:\AI Projects\Pro-synthesis\internal\recorder\testdata\world_hashes.json`: Golden file to expected world_hash mapping -- the alarm that fires if anyone changes the encoder and silently rehashes the regression corpus. _(0)_
- `C:\AI Projects\Pro-synthesis\.gitattributes`: Marks *.thesis, *.jsonl and *.json as -text so git's CRLF translation cannot corrupt committed regression worlds on Windows checkout. _(0 (required))_

## Design

# `internal/recorder`: Determinism Substrate Design

**Scope:** Phase 0 only. Everything below is either Phase 0 code or a Phase-0 *seam* (an interface with a trivial implementation) that a later phase plugs into. Nothing here implements faults, oracles, search or shrinking.

**Dependency rule (enforced by test):** `internal/recorder` imports only `pkg/schema` and the Go standard library. Every other subsystem imports the recorder, never the reverse. That is why `Actuator` and `Clock` are interfaces.

---

## 0. The three guarantees, stated precisely

| # | Guarantee | Enforced by |
|---|---|---|
| **G1** | Virtual time never goes backwards, never depends on the wall clock, and every scheduled event's *planned*, *initiated* and *effective* time is recorded. | monotonic-only arithmetic + `timeline.jsonl` |
| **G2** | Stream `B`'s value sequence is a function of `(root_seed, path_of_B)` alone, never of how much anyone drew from stream `A`. | key-tree derivation (structural, not disciplinary) |
| **G3** | `Encode(Decode(b)) == b` bytewise for every `b` the loader accepts, and the loader accepts exactly the canonical language. | strict parser + **re-encode-and-compare on every `Load`** |

G3's phrasing matters. `Marshal(Unmarshal(b)) == b` "for all valid worlds" is **impossible** if "valid" means "parseable JSON": `{"a":1,"b":2}` and `{"b":2,"a":1}` cannot both round-trip. It is trivially true if "valid" means "canonical". So we define canonical, and we make the loader *reject* non-canonical input rather than silently normalize it. Silent normalization would be an I6 hole: a hand-edited regression world would rewrite to a different `world_hash` and the corpus would drift with nobody noticing.

---

# PART 1: VIRTUAL CLOCK

## 1.1 Time frames

Three frames, one conversion, no rebasing:

```go
// RTime is nanoseconds since recorder construction. Monotonic, always >= 0.
// Everything is *stamped* in RTime. It never needs rebasing.
type RTime int64

// VTime is nanoseconds relative to the DRIVE origin (t=0).
// NEGATIVE during BOOT and SEED. This is the frame the fault grammar
// ("@8200..15100") and prothesis.oracle_input/v1 "phases" both use.
type VTime int64

// Wall is time.Time, recorded ONCE as an anchor and used only to correlate
// externally-timestamped artifacts (container stdout, docker events).
```

`VTime` is the public frame because §4.3 says fault windows are "relative to `DRIVE` start on virtual clock" and the `oracle_input` example shows `DRIVE start_ms: 0`. Anchoring t=0 at DRIVE means the fault grammar and the oracle phase array are the *same coordinate system with zero conversion*, and, crucially, **a slow BOOT cannot shift the fault schedule**, because the origin is stamped when DRIVE actually begins rather than predicted.

Consequence: `BOOT` and `SEED` appear in `phases` with negative `start_ms`. We emit them anyway (I5 phase-aware assertion needs them). Documented deviation from the illustrative example, which shows only DRIVE onward.

```go
// Timeline holds the single conversion constant. Immutable after DRIVE start.
type Timeline struct {
    originRT atomic.Int64 // RTime of DRIVE start; 0 => not yet set
    set      atomic.Bool
}

func (t *Timeline) V(r RTime) VTime { return VTime(r - RTime(t.originRT.Load())) }
func (t *Timeline) R(v VTime) RTime { return RTime(v) + RTime(t.originRT.Load()) }

// SetOrigin is called exactly once, by the lifecycle machine, at the instant
// the driver process is spawned. A second call panics.
func (t *Timeline) SetOrigin(r RTime)
```

## 1.2 The Clock interface

```go
// Clock is the ONLY time source in PRO-THESIS.
//
// No package outside internal/recorder may call time.Now, time.Since,
// time.After, time.Sleep, time.Tick or time.NewTimer.
// determinism_scan_test.go fails the build if one appears.
//
// Two implementations exist in v1: realClock (Tier B, anchored to the host
// monotonic clock) and simClock (tests today; the Tier A v4 seam tomorrow).
// The interface is identical for both, which is what keeps Tier A open.
type Clock interface {
    // NowR is the monotonic reading. Never decreases. Unaffected by NTP
    // steps, manual clock changes, DST or VM save/restore.
    NowR() RTime

    // NowWall is DERIVED from NowR plus the wall anchor -- it is not a fresh
    // wall read. This guarantees non-decreasing timestamps in history.jsonl,
    // which every consistency checker requires. True wall drift is sampled
    // separately by the drift watcher and recorded in clock.json.
    NowWall() time.Time

    // SleepUntilR blocks until NowR() >= at, ctx is done, or the clock closes.
    // Returns the actual wake time, so lateness is always measured rather
    // than assumed.
    SleepUntilR(ctx context.Context, at RTime) (woke RTime, err error)

    // Anchor is the artifact record: wall instant and calibration at t=0.
    Anchor() schema.Anchor

    // Health reports clock anomalies observed so far (wall/mono divergence,
    // suspend detection, sleep overshoot).
    Health() schema.ClockHealth
}
```

## 1.3 Monotonic vs wall on Windows: explicitly

Facts this design depends on:

- Go's `time.Now()` returns a `time.Time` carrying **both** a wall reading and a monotonic reading. `time.Since(t)` and `t2.Sub(t1)` use the monotonic reading when both operands have one. On Windows, `runtime.nanotime` is backed by `QueryPerformanceCounter` (invariant TSC on modern hardware).
- **The monotonic reading is silently stripped** by `t.Round(0)`, by `MarshalJSON`/`MarshalText`/`gob`, and by anything reconstructing a `time.Time` from parsed fields. A monotonic base stored in a struct that later gets serialized and reloaded becomes a wall clock with no warning. This is the most common way Go programs lose monotonicity.
- Windows default timer granularity is ~15.6 ms unless `timeBeginPeriod` is called. Go 1.16+ uses `CREATE_WAITABLE_TIMER_HIGH_RESOLUTION` on Win10 1803+ for roughly 1 ms, but that is a runtime implementation detail, not a contract.
- Wall clock (`GetSystemTimeAsFileTime`) steps on NTP correction, user change, and Hyper-V VM save/restore. Docker Desktop's Linux VM has its *own* clock; container log timestamps are in that clock, not ours.

Design responses:

```go
type realClock struct {
    monoBase time.Time // HAS a monotonic reading. PRIVATE. NEVER serialized,
                       // never Round(0)'d, never passed to encoding/json.
    wallBase time.Time // monotonic STRIPPED (.Round(0).UTC()). Artifact only.

    spinNs    int64    // measured sleep-tail spin threshold (see 1.5)
    drift     driftLog // wall-vs-mono samples, every 5s
    anomalies atomic.Int64
}

func NewRealClock() *realClock {
    now := time.Now()
    c := &realClock{
        monoBase: now,                 // keeps the monotonic reading
        wallBase: now.Round(0).UTC(),  // deliberately strips it
    }
    c.spinNs = calibrateSleep()        // 20 x 1ms sleeps, median overshoot
    go c.watchDrift()
    return c
}

func (c *realClock) NowR() RTime {
    return RTime(time.Since(c.monoBase)) // monotonic subtraction, QPC-backed
}

func (c *realClock) NowWall() time.Time {
    return c.wallBase.Add(time.Duration(c.NowR())) // derived => monotonic
}
```

The drift watcher samples `time.Now().Round(0)` every 5 s and compares its delta to the monotonic delta:

```go
func (c *realClock) watchDrift() {
    for range ticker {
        wallDelta := time.Now().Round(0).Sub(c.wallBase)
        monoDelta := time.Duration(c.NowR())
        d := wallDelta - monoDelta
        c.drift.append(c.NowR(), d)
        if abs(d) > 2*time.Second {
            c.emit("clock.anomaly", "wall_drift_ms", d.Milliseconds())
            c.anomalies.Add(1)
        }
    }
}
```

Two anomalies are treated as **environment failures, not oracle violations**:

- `|wall − mono| > 2 s` appearing as a step → NTP step or VM resume.
- a sequencer wakeup arriving > 5 s after its deadline with no pending work → host suspend.

Both mark the run `INCONCLUSIVE` (exit 2). Reporting a stale-read violation that was actually a laptop lid closing is worse than reporting nothing.

## 1.4 How a fault at `8200..15100` actually executes

**Step 1: compile.** Before DRIVE starts, the world's fault list becomes a totally ordered event list. Pure function of the world.

```go
type EventOp uint8
const (
    OpWithdraw EventOp = iota // rank 0 -- withdrawals sort BEFORE injects
    OpInject                  // rank 1
)

type Event struct {
    At     VTime
    Op     EventOp
    Seq    int // deterministic tiebreak
    Action Action
}

// Compile is a pure function of the world. The ordering key is (At, Op, Seq),
// where Seq is the index after sorting faults by their canonical grammar
// string Fault.String().
//
// Without Seq, two faults at t=8400 would fire in slice-append or map
// iteration order -- not a function of the world, which alone would make I2
// ("every failure is a seed") false.
//
// Withdrawals sort before injects at the same instant so max_concurrent_faults
// is never transiently exceeded at a window boundary.
func Compile(faults []schema.Fault, plan schema.PhasePlan) ([]Event, error) {
    sorted := append([]schema.Fault(nil), faults...)
    sort.Slice(sorted, func(i, j int) bool {
        return sorted[i].String() < sorted[j].String() // canonical text order
    })
    var evs []Event
    for i, f := range sorted {
        if f.EndMs > plan.DriveMs+plan.HealMs {
            return nil, fmt.Errorf("fault %s ends after DRIVE+HEAL (%dms): "+
                "config error, not a silent skip", f, plan.DriveMs+plan.HealMs)
        }
        a := Action{ID: f.String(), Kind: f.Kind, Target: f.Target, Params: f.Params}
        evs = append(evs,
            Event{At: VMs(f.StartMs), Op: OpInject,   Seq: i, Action: a},
            Event{At: VMs(f.EndMs),   Op: OpWithdraw, Seq: i, Action: a})
    }
    sort.SliceStable(evs, func(i, j int) bool {
        if evs[i].At != evs[j].At { return evs[i].At < evs[j].At }
        if evs[i].Op != evs[j].Op { return evs[i].Op < evs[j].Op }
        return evs[i].Seq < evs[j].Seq
    })
    return evs, nil
}
```

Note the compile-time rejection: a fault ending after DRIVE+HEAL is a **config error (exit 5)**, never a silently skipped fault. Silently running fewer faults than the world specifies and then passing is exactly the gate-weakening failure mode I6 forbids.

**Step 2: the sequencer.** One goroutine owns ordering. Dispatch is offloaded, because `docker exec` into a container on Docker Desktop takes 100–800 ms and must not stall the loop.

```go
type Actuator interface {
    // Do injects. MUST honor ctx. MUST be safe to retry.
    Do(ctx context.Context, a Action) error
    // Undo withdraws a previously successful Do.
    Undo(ctx context.Context, a Action) error
}

type Sequencer struct {
    clock Clock
    tl    *Timeline
    act   Actuator
    tlog  *Appender     // timeline.jsonl
    sem   chan struct{} // max_concurrent_faults

    MaxLateness     time.Duration // default 250ms  (injects only)
    DispatchTimeout time.Duration // default 10s
    DrainTimeout    time.Duration // default 30s

    mu       sync.Mutex
    withdraw []Action // LIFO stack of successful injects
    residual []Action
    fidelity schema.Fidelity
}

func (s *Sequencer) Run(ctx context.Context, evs []Event) error {
    defer s.drain() // ALWAYS, on every exit path
    for i, ev := range evs {
        target := s.tl.R(ev.At)
        woke, err := s.clock.SleepUntilR(ctx, target)
        if err != nil { // budget expired / Ctrl-C
            s.fidelity = schema.FidelityTruncated
            s.log("schedule.truncated",
                "at_v_ms", s.tl.V(woke).Ms(), "remaining_events", len(evs)-i)
            return nil // the deferred drain still runs
        }
        late := time.Duration(woke - target)
        switch {
        case ev.Op == OpWithdraw:
            // Withdrawals are NEVER skipped, no matter how late. A residual
            // iptables rule or a SIGSTOP'd container outlives the run.
            s.dispatch(ev, woke, late)
        case late > ev.Action.WindowDur():
            // Window entirely missed. Skip inject AND its paired withdraw.
            s.fidelity = degrade(s.fidelity)
            s.log("fault.inject.skipped", "reason", "window_elapsed",
                "late_ms", late.Milliseconds())
            s.cancelPairedWithdraw(ev.Seq)
        case late > s.MaxLateness:
            s.fidelity = degrade(s.fidelity) // fire, but the run is degraded
            s.dispatch(ev, woke, late)
        default:
            s.dispatch(ev, woke, late)
        }
    }
    return nil
}
```

**Step 3: three timestamps per fault, recorded honestly.**

```go
func (s *Sequencer) dispatch(ev Event, woke RTime, late time.Duration) {
    s.log("fault."+ev.Op.String()+".initiated",
        "fault", ev.Action.ID,
        "planned_v_ms",   ev.At.Ms(),
        "initiated_v_ms", s.tl.V(woke).Ms(),
        "late_ms",        late.Milliseconds())

    go func() { // ordering is already fixed by the loop above
        dctx, cancel := context.WithTimeout(s.baseCtx, s.DispatchTimeout)
        defer cancel()
        err := s.act.Do(dctx, ev.Action)
        eff := s.clock.NowR()
        s.log("fault."+ev.Op.String()+".effective",
            "fault", ev.Action.ID,
            "effective_v_ms",      s.tl.V(eff).Ms(),
            "actuator_latency_ms", (eff - woke).Millis(),
            "err",                 errString(err))
        if err != nil {
            // NEVER treat a failed injection as "fewer faults, still passed".
            s.fail(schema.ErrInjectFailed) // -> run is INCONCLUSIVE (exit 2)
            return
        }
        s.push(ev.Action) // register the withdrawal obligation immediately
    }()
}
```

`planned_v_ms` / `initiated_v_ms` / `effective_v_ms` all land in `timeline.jsonl`. The verdict's `causal_timeline` uses **`effective_v_ms`**, because that is when the fault was actually in force. Claiming a partition began at exactly 8400 when `iptables` returned at 8612 is the kind of small lie that destroys a debugging tool's credibility, and it would make a shrunk repro's timing window meaningless.

**Step 4: drain, on a detached context.**

```go
// drain pops the withdrawal stack LIFO. It deliberately builds a DETACHED
// context: if it used the cancelled run context, Ctrl-C would leave iptables
// rules installed and containers SIGSTOP'd on the developer's machine.
// Section 4.3's "every fault must implement a safe withdraw()" and the HEAL
// zero-residual assertion both depend on withdrawals surviving cancellation
// of the thing that scheduled them.
func (s *Sequencer) drain() {
    ctx, cancel := context.WithTimeout(context.Background(), s.DrainTimeout)
    defer cancel()
    for a, ok := s.pop(); ok; a, ok = s.pop() {
        if err := s.act.Undo(ctx, a); err != nil {
            s.log("fault.withdraw.failed", "fault", a.ID, "err", err.Error())
            s.residual = append(s.residual, a)
        }
    }
}
```

If `residual` is non-empty at HEAL the run is `INCONCLUSIVE` and the verdict says so. A residual fault means the environment is now dirty and the next run's results are untrustworthy.

## 1.5 Windows sleep granularity: the spin tail

```go
// SleepUntilR sleeps to within spinNs of the target, then busy-yields.
//
// Rationale: Windows timer granularity is ~15.6ms by default; Go 1.16+ uses
// high-resolution waitable timers on Win10 1803+ for ~1ms, but that is a
// runtime detail rather than a contract. Sleeping the full remaining duration
// can overshoot an 8200ms deadline by a whole 15.6ms tick -- a 6% timing
// error against a 250ms fault window.
//
// Cost: at most spinNs (default 2ms) of one core per event. With <= 48 events
// per world (24 faults x inject+withdraw) that is <= 96ms of spin across a
// 10-minute world. Acceptable.
//
// We deliberately do NOT call timeBeginPeriod(1): it is process-global,
// perturbs system-wide power management, and needs x/sys/windows.
// We measure granularity instead and report it in clock.json.
func (c *realClock) SleepUntilR(ctx context.Context, at RTime) (RTime, error) {
    for {
        now := c.NowR()
        d := time.Duration(at - now)
        if d <= 0 {
            return now, ctx.Err()
        }
        if d > time.Duration(c.spinNs) {
            t := time.NewTimer(d - time.Duration(c.spinNs))
            select {
            case <-t.C:
            case <-ctx.Done():
                t.Stop()
                return c.NowR(), ctx.Err()
            }
            continue
        }
        for c.NowR() < at {
            if err := ctx.Err(); err != nil { return c.NowR(), err }
            runtime.Gosched()
        }
        return c.NowR(), nil
    }
}
```

`calibrateSleep()` runs 20 × 1 ms sleeps at startup and records the median overshoot into `clock.json` as `sleep_granularity_ns`. Above 5 ms, the recorder raises `min_window_ms` and warns. `simClock` sets `spinNs = 0`.

## 1.6 Phase overrun: the full taxonomy

| Case | Detection | Response | Fidelity | Exit |
|---|---|---|---|---|
| BOOT/SEED exceed `health.timeout` / `steady_state.timeout` | hard deadline | abort *before* the origin is set; no fault ever ran | n/a | **2** |
| BOOT slow but succeeds | n/a | **no effect**: origin stamped at real DRIVE start | `exact` | n/a |
| inject late ≤ 250 ms | `woke − target` | fire, record `late_ms` | `exact` | n/a |
| inject late > 250 ms, window still open | same | fire, effective window shortened, record | `degraded` | n/a |
| inject late past `end_ms` | same | skip inject **and** its paired withdraw | `degraded` | n/a |
| **any** withdraw late | n/a | **always fire**, never skip | unchanged | n/a |
| driver exits before schedule drains | process exit | hold DRIVE open until last withdraw + `drain_ms`, then HEAL | `exact` | n/a |
| budget expires / Ctrl-C mid-schedule | `ctx.Done()` | stop injects, drain withdrawals on **detached** ctx | `truncated` | **3** |
| `Actuator.Do` returns error | dispatch | do **not** push withdrawal; fail the run | `degraded` | **2** |
| residual faults at HEAL | non-empty stack | force-drain, then fail | `degraded` | **2** |
| wall/mono divergence > 2 s, or > 5 s wakeup stall | drift watcher | mark clock anomaly | `degraded` | **2** |

```go
type Fidelity string
const (
    FidelityExact     Fidelity = "exact"
    FidelityDegraded  Fidelity = "degraded"  // schedule ran, timing compromised
    FidelityTruncated Fidelity = "truncated" // schedule did not complete
)
```

`Fidelity` lands in `run.json` and in each world record. A violation found under `degraded` fidelity is still a **real** violation (I1: real execution), but Phase 5's confirmation gate must count only `exact` executions toward `reproduced: "k/k"`; otherwise that number measures the host's mood rather than the system under test.

**Observed phases feed the oracles.** If DRIVE planned 42000 ms but ran 44300, `oracle_input.phases` carries 44300. I5 (phase-aware assertion) is only sound against *observed* boundaries.

## 1.7 The I2 tuple has an input/output collision: resolved

I2 defines the world tuple as `(seed, topology_variant, driver_profile, fault_schedule, phase_timings)`. But `phase_timings` is both what you *ask for* and what you *got*. If observed timings go into the world, the hash changes every replay and the world can never be replayed. Resolution:

- **`schema.PhasePlan`** (planned budgets: `drive_ms`, `heal_ms`, `quiesce_ms`, timeouts) lives in `.thesis`, is hashed, is an input.
- **`[]schema.PhaseSpan`** (observed boundaries in virtual ms) lives in `phases.json` and `run.json`, is *not* hashed, is an output, and is what `oracle_input.phases` is built from.

---

# PART 2: DETERMINISTIC PRNG SEED STREAMS

## 2.1 Why nothing from the standard library generates values

- `math/rand`: `rand.Seed` and the global source changed behavior in Go 1.20. The derived helpers (`Intn`, `Float64`, `Perm`, `Shuffle`) carry **no** cross-version stability guarantee, and `Shuffle`'s implementation has changed.
- `math/rand/v2`: documents that top-level functions are randomly seeded and algorithms may change. `PCG`/`ChaCha8` are named types, but `Uint64N`'s reduction is not frozen by the API.
- `math.Log`, `math.Exp`: pure-Go implementations that may differ by one ULP across architectures. Any distribution derived through libm breaks cross-platform bit-identity.

So: **the recorder generates its own bits and extracts its own values, and both algorithms live in this repo.** The only stdlib crypto used is `crypto/sha256` and `crypto/hmac`, whose outputs are fixed by FIPS 180-4 and RFC 2104 and cannot drift.

A second reason to hand-roll: CRUCIBLE PART 10 requires worlds to be portable across repos. A Python or Rust oracle can reproduce HMAC-SHA256 + ChaCha20 exactly. It cannot reproduce Go's ALFG.

## 2.2 The key tree: why independence is structural

```go
// Seed is the run/world root seed: 64 bits, presented as 16 lowercase hex
// digits (see 3.6 for why not a JSON number).
type Seed uint64

// StreamKey is a node in the seed tree: a 32-byte ChaCha20 key.
type StreamKey [32]byte

const domainTag = "prothesis/v1\x00"

// RootKey = HMAC-SHA256(key = be64(seed), msg = "prothesis/v1\x00root")
func RootKey(s Seed) StreamKey {
    var k [8]byte
    binary.BigEndian.PutUint64(k[:], uint64(s))
    m := hmac.New(sha256.New, k[:])
    m.Write([]byte(domainTag + "root"))
    var out StreamKey
    copy(out[:], m.Sum(nil))
    return out
}

// Derive: named child.  child = HMAC-SHA256(key = parent, msg = 0x01 || name)
func (k StreamKey) Derive(name string) StreamKey {
    m := hmac.New(sha256.New, k[:])
    m.Write([]byte{0x01})
    m.Write([]byte(name))
    var out StreamKey; copy(out[:], m.Sum(nil)); return out
}

// DeriveIndex: numeric child. child = HMAC-SHA256(key = parent, msg = 0x02 || be64(i))
func (k StreamKey) DeriveIndex(i uint64) StreamKey {
    var b [9]byte
    b[0] = 0x02
    binary.BigEndian.PutUint64(b[1:], i)
    m := hmac.New(sha256.New, k[:]); m.Write(b[:])
    var out StreamKey; copy(out[:], m.Sum(nil)); return out
}
```

The `0x01`/`0x02` prefix bytes keep the named and indexed namespaces disjoint, so `Derive("7")` can never collide with `DeriveIndex(7)`. HMAC rather than plain `SHA256(root || domain)` gives standard domain separation with no length-extension or prefix-ambiguity concern.

**This is why G2 holds structurally.** A stream's key is a function of its *path in the tree* only. Drawing ten million values from `fault.schedule` cannot move `driver.workload` by one bit, because `driver.workload`'s key was never a function of `fault.schedule`'s consumption. That is a property of the derivation, not a discipline anyone must remember.

## 2.3 The generator: ChaCha20, RFC 8439

```go
// Stream is a seekable deterministic bit source: ChaCha20 (RFC 8439, 20
// rounds) keyed by a StreamKey, with a 96-bit nonce and 32-bit block counter.
//
// ChaCha20 is chosen because:
//   1. The algorithm is frozen forever by RFC 8439. Bit-exact in any language.
//   2. It is COUNTER-ADDRESSABLE: block N is computable without generating
//      blocks 0..N-1. That gives O(1) Seek, which is what makes shrinking
//      stable -- see 2.5.
//   3. The 96-bit nonce gives 2^96 free independent sub-streams per key at
//      zero derivation cost.
//   4. ~60 lines of pure Go verified against the RFC 8439 test vectors --
//      safer than a third-party dependency and cheaper than trusting one.
type Stream struct {
    key   StreamKey
    nonce [12]byte
    block uint32
    buf   [64]byte
    off   int

    draws uint64 // audit counter -> streams.json
    mode  uint8  // 0 unset | 1 sequential | 2 indexed (mixing panics)
}

func NewStream(k StreamKey) *Stream

// Index returns an independent sibling sharing the key but using nonce = i.
// Cheap: no hashing, no key schedule.
//
// A Stream may be used EITHER sequentially OR via Index, never both --
// otherwise sequential draws (nonce 0) collide with Index(0). The mode field
// enforces this; misuse panics in tests and is flagged by the audit.
func (s *Stream) Index(i uint64) *Stream

// Seek positions the sequential stream at byte offset n. O(1).
func (s *Stream) Seek(n uint64) { s.block = uint32(n / 64); s.off = int(n % 64) }
```

## 2.4 Value extraction: every primitive written out

This is the half people get wrong. A fixed keystream is not enough; the *reduction* must be frozen too.

```go
// Uint64 returns the next 8 keystream bytes, LITTLE-ENDIAN.
// (Little-endian because the ChaCha state serialization is little-endian;
// stated explicitly so a Python reimplementation matches.)
func (s *Stream) Uint64() uint64

// Uint64n returns a uniform value in [0, n) via Lemire's multiply-shift with
// full rejection. Written out here rather than delegated to math/rand, whose
// helpers carry no cross-version stability guarantee.
func (s *Stream) Uint64n(n uint64) uint64 {
    if n == 0 { panic("recorder: Uint64n(0)") }
    x := s.Uint64()
    hi, lo := bits.Mul64(x, n) // stdlib widening multiply; semantics fixed
    if lo < n {
        t := (-n) % n          // == 2^64 mod n
        for lo < t {
            x = s.Uint64()
            hi, lo = bits.Mul64(x, n)
        }
    }
    return hi
}

// Float64 returns a uniform value in [0,1) with exactly 53 bits of entropy:
//
//     float64(Uint64() >> 11) * (1.0 / (1 << 53))
//
// Both the integer-to-float conversion (a 53-bit value, exactly representable)
// and the multiply by a power of two are EXACT in IEEE-754 binary64 on every
// conforming platform. No libm call is involved, so this is bit-identical
// across Go versions, architectures and operating systems.
func (s *Stream) Float64() float64 {
    return float64(s.Uint64()>>11) * (1.0 / (1 << 53))
}

// Shuffle is Fisher-Yates, DESCENDING:
//   for i := n-1; i > 0; i-- { j := int(Uint64n(uint64(i)+1)); swap(i, j) }
func (s *Stream) Shuffle(n int, swap func(i, j int))

// WeightedPPM selects an index given integer weights in parts-per-million.
//
// Integer arithmetic ONLY. This closes a real determinism trap: a config mix
// like {read: 0.4, write: 0.4, txn: 0.2} arrives as a Go map, whose iteration
// order Go randomizes; float addition is not associative, so the cumulative
// sum -- and therefore the selection at bucket boundaries -- would differ run
// to run. Weights are canonicalized to sorted-key order and to integer ppm
// summing to exactly 1_000_000 by largest-remainder apportionment before they
// ever reach this function.
func (s *Stream) WeightedPPM(w []uint32) int {
    var total uint64
    for _, x := range w { total += uint64(x) }
    r := s.Uint64n(total)
    var acc uint64
    for i, x := range w {
        acc += uint64(x)
        if r < acc { return i }
    }
    return len(w) - 1 // unreachable
}
```

**Deliberately absent from the frozen core:** `NormFloat64`, `ExpFloat64`, anything needing `math.Log`/`math.Exp`. When Phase 1's driver needs exponential inter-arrival times it samples a committed 4096-entry inverse-CDF table of `int64` microseconds (generated once by a committed generator, checked in with its hash). `math.Sqrt` would be safe (IEEE-754 mandates correct rounding and it is a hardware instruction) but `math.Log` is not, and one transcendental is enough to break cross-platform reproduction.

## 2.5 Index derivation is what makes shrinking work

Phase 5's `ddmin` deletes operation #7 from a 20,000-op workload. Under sequential consumption, deleting op #7 shifts the randomness of ops #8–#20000, so the shrinker would be minimizing against a moving target and could never converge.

The rule: **anything a shrinker might delete draws from an index-derived sub-stream, never sequentially.**

```go
// Correct: op parameters are addressed by op_id, so deleting an op is
// invisible to every other op.
opRnd := streams.Get(schema.StreamDriverWorkload).Index(uint64(opID))
key   := opRnd.Uint64n(keyspace)
val   := opRnd.Uint64()

// Wrong: sequential draws couple every op to every earlier op.
```

Same for faults (`Index(faultOrdinal)`), clients (`Index(clientID)`) and worlds (`Index(worldOrdinal)`).

## 2.6 Domain registry: frozen, and part of the reproducibility contract

```go
// pkg/schema/streams.go
//
// These strings are part of the reproducibility contract, not implementation
// detail: changing one silently invalidates the entire regression corpus.
// The table is therefore hashed into .prothesis/lock (Phase 3), so renaming a
// domain is an ORACLE_DRIFT event (exit 4), not a refactor.
const (
    StreamFaultSchedule   = "fault.schedule"
    StreamDriverWorkload  = "driver.workload"
    StreamClientSchedule  = "client.schedule"
    StreamInjectDelay     = "inject.delay"
    StreamSearchMutate    = "search.mutate"
    StreamTopologyVariant = "topology.variant"
)

var RegisteredStreams = []string{ /* ...the above, sorted... */ }
```

```go
// Streams is the per-WORLD root. Note: per-world, not per-run.
type Streams struct {
    root StreamKey
    mu   sync.Mutex
    used map[string]*Stream
}

// NewStreams builds the tree for a single world from the world's OWN seed.
// A .thesis file must be self-contained (I2), so a world's randomness must
// never depend on its ordinal within some run.
func NewStreams(worldSeed schema.Seed) *Streams

// Get returns the memoized stream for a registered domain. Panics on an
// unregistered domain: a typo'd domain string would otherwise silently
// produce a fresh, unreproducible stream.
func (s *Streams) Get(domain string) *Stream

// Audit reports per-stream draw counts for streams.json.
func (s *Streams) Audit() []schema.StreamAudit
```

Run-level seeds flow: `--seed N` sets the **run root**; world *w* gets `worldSeed = low64(RootKey(N).DeriveIndex(w))`; that 64-bit value is written into the world's `.thesis`, so `thesis replay w.thesis` needs nothing else. `--seed` on `replay` is rejected: the world carries its seed.

## 2.7 Handing a seed to an external driver

`driver.cmd` templates `{seed}`. Two placeholders, both deterministic:

- `{seed}` → decimal `uint64`, the big-endian low 64 bits of the `driver.workload` stream key. Compatible with any existing loadgen expecting an integer.
- `{seed_hex}` → the full 64-hex-char stream key, for PRO-THESIS-aware drivers implementing the same ChaCha derivation and therefore getting shrink-stable index addressing.

Tier B honesty: an external driver only has to be *self-consistent*. We do not claim to control randomness inside the system under test.

---

# PART 3: `.thesis` CANONICAL SERIALIZATION

## 3.1 The decisive move: no floats in the file

Every quantity in a world is expressible as an integer with a declared unit, or as an identifier:

| Spec form | `.thesis` form |
|---|---|
| `net.latency(mean, jitter)` | `{"jitter_ms": 20, "mean_ms": 150}` |
| `net.loss(pct)`: 1.25% | `{"loss_ppm": 12500}` |
| `net.bandwidth(bps)` | `{"bps": 1048576}` |
| `proc.slow(cpu_pct)` | `{"cpu_pct": 30}` |
| `clock.skew(ms)` | `{"skew_ms": -400}` |
| `io.error(rate)` | `{"rate_ppm": 5000}` |
| `mix: {read: 0.5, write: 0.5}` | `{"read_ppm": 500000, "write_ppm": 500000}` |
| root seed (uint64) | `"seed": "000000000000002a"` |

So **`.thesis` contains only objects, arrays, strings, booleans and integers.** That one decision eliminates, at a stroke: float formatting and shortest-round-trip algorithm dependence, `-0.0` vs `0.0`, NaN/Inf, and integer-vs-float ambiguity on decode. It also removes libm from the determinism boundary. Mix weights normalize to exactly 1,000,000 by largest-remainder apportionment in sorted-key order at config load.

The rule is enforced, not merely intended: `cjson.Lint` (§3.7) rejects any schema type containing `float32`/`float64`.

For the one place the spec genuinely shows a float (`verdict.suspect.confidence: 0.42`) we use `schema.Dec`, an exact decimal serialized as a string at a per-field declared scale (`cjson:"confidence,dec=2"` → `"0.42"`). Never used inside `.thesis`.

## 3.2 The CJSON profile

```go
package cjson

// Value is the canonical value tree. There is deliberately no Null and no
// Float.
type Value interface{ isValue() }

type Obj struct{ M []Member } // invariant: keys unique, sorted by UTF-8 bytes
type Member struct{ K string; V Value }
type Arr  []Value
type Str  string
type Int  int64
type Bool bool
```

**The eight rules.** `Encode` implements them; `Parse` enforces them.

1. **Key ordering.** *All* object keys (struct fields and map keys alike) sorted ascending by UTF-8 byte sequence. Sorting struct fields too (rather than using declaration order) means a developer reordering a struct is a no-op instead of silently rehashing the entire regression corpus. Keys must match `^[a-z][a-z0-9_]*$`, so byte order and RFC 8785's UTF-16 code-unit order coincide: JCS-compatible on ordering, with no import and no Unicode normalization question.

2. **Numbers.** Grammar `-?(0|[1-9][0-9]*)`. No `+`, no `.`, no `e`/`E`, no leading zeros, no `-0`. Magnitude ≤ 2⁵³−1. Anything larger **must** use `schema.HexU64` or a string type, keeping verdicts safe for JavaScript and MCP consumers, which parse JSON numbers as doubles.

3. **Strings.** Raw UTF-8. Escape only `"`, `\` and U+0000–U+001F, using `\b \f \n \r \t` where defined and `\u00xx` (lowercase hex) otherwise. **`<`, `>`, `&` emitted literally**: `encoding/json` escapes them by default, which alone breaks byte-identity against any other JSON writer. Invalid UTF-8 and lone surrogates are **errors**, never silently replaced; `json.Marshal` substitutes U+FFFD, which is data loss and breaks injectivity.

4. **Layout.** Two-space indent per level; `"key": value`; `,\n` between members; `{}` and `[]` for empties with no inner whitespace; array elements one per line. Whitespace is a total function of structure.

5. **Trailing newline.** Exactly one `\n` (LF) at EOF. See §3.5: a Windows landmine.

6. **Absent ≡ zero.** `null` is never emitted and is a parse error. A field is always emitted unless tagged `cjson:",omitempty"`. Nil and empty slices/maps both encode as `[]`/`{}`. With no pointers and no null, absent-vs-zero is a bijection, so injectivity is preserved.

7. **Duplicate keys are a decode error.** `encoding/json` silently takes the last one.

8. **No BOM, no CR, no trailing bytes.**

```go
// Encode writes the canonical form. Deterministic and total.
func Encode(v Value) ([]byte, error)

// Parse is a STRICT reader: it accepts only canonical documents. It rejects
// duplicate keys, out-of-order keys, whitespace deviation, floats, exponents,
// leading zeros, "-0", null, NaN/Inf, BOMs, CR, invalid UTF-8, lone
// surrogates, non-minimal escapes and trailing bytes -- each with a
// line/column and a "run `thesis world canonicalize`" hint.
func Parse(b []byte) (Value, error)
```

**Why not careful `encoding/json`?** It HTML-escapes by default, orders struct fields by declaration, accepts duplicate keys on decode, substitutes U+FFFD for invalid UTF-8, and cannot reject non-canonical input.

**Why not CBOR/RFC 8949 canonical?** A better wire format, and it needs a third-party library, but Phase 5 commits shrunk regression worlds *to the repo*, where humans review them in pull requests. A regression world must be diffable text. That outranks compactness.

**Why not RFC 8785 JCS?** JCS mandates a single line with no insignificant whitespace, making a 24-fault world unreviewable, and its number rule requires the ECMAScript shortest-round-trip algorithm, whose Go equivalent differs at exponent boundaries (`1e-07` vs `1e-7`). We take JCS's key-ordering discipline and drop the rest.

## 3.3 Byte-identity as a runtime invariant

```go
// DecodeWorld parses canonical .thesis bytes.
//
// It fails unless ALL of:
//   (a) the document parses under the strict grammar,
//   (b) EncodeWorld(result) == b BYTEWISE,
//   (c) the embedded world_hash equals the recomputed hash.
//
// (b) is the point. Byte-identity is not merely asserted in a test suite --
// it is checked on EVERY load, in production, for every regression world on
// every gate invocation. A subtle encoder or decoder bug cannot be silent.
// Cost is a few microseconds on a file of a few kilobytes.
//
// Only `thesis world canonicalize` may pass allowNonCanonical.
func DecodeWorld(b []byte, opts ...LoadOpt) (*schema.World, error) {
    tree, err := cjson.Parse(b)
    if err != nil { return nil, err }
    var w schema.World
    if err := cjson.Unmarshal(tree, &w); err != nil { return nil, err }

    out, err := EncodeWorld(&w)
    if err != nil { return nil, err }
    if !bytes.Equal(b, out) {
        return nil, &ErrNonCanonical{Diff: firstDiff(b, out)}
    }
    if got := HashWorld(&w); got != w.WorldHash {
        return nil, &ErrHashMismatch{Want: w.WorldHash, Got: got}
    }
    return &w, nil
}
```

## 3.4 `world_hash`: content addressing over I2's tuple, not the file

Naive self-reference: you cannot hash a document containing its own hash. Subtler: if `origin.commit` and `origin.tool_version` were in the preimage, the *same logical world* discovered on two commits would get two hashes, and the corpus would never dedupe.

Resolution: hash a **frozen projection** equal to exactly invariant I2's tuple:

```go
// HashableKeys is the frozen projection over which world_hash is computed.
// It is EXACTLY invariant I2's world tuple:
//   (seed, topology_variant, driver_profile, fault_schedule, phase_timings)
//
// Fields outside it -- origin (commit, config_hash, created_by, tool_version),
// notes, schema, world_hash -- are PROVENANCE, not IDENTITY. The same world
// found on two commits must hash identically or the search corpus cannot
// dedupe and the regression corpus grows without bound.
var HashableKeys = []string{
    "driver_profile", "fault_schedule", "phase_timings",
    "seed", "topology_variant",
}

const worldHashDomain = "prothesis.world/v1\n" // domain-separates the preimage

func HashWorld(w *schema.World) schema.Hash {
    tree, _ := cjson.Marshal(w)               // full tree
    proj := cjson.Project(tree, HashableKeys) // keep only the tuple keys
    pre, _ := cjson.Encode(proj)              // canonical bytes of the tuple
    h := sha256.New()
    h.Write([]byte(worldHashDomain))
    h.Write(pre)
    return schema.Hash("sha256:" + hex.EncodeToString(h.Sum(nil)))
}

func EncodeWorld(w *schema.World) ([]byte, error) {
    c := *w
    c.WorldHash = HashWorld(&c) // over the projection, not the file
    tree, err := cjson.Marshal(&c)
    if err != nil { return nil, err }
    return cjson.Encode(tree)   // full file, hash field included
}
```

Byte-identity still covers the *whole file*. Only the *hash* is over the projection.

## 3.5 Windows-specific hazards this design must survive

**CRLF will destroy a committed regression corpus.** Go never translates line endings; `git` does. A `.thesis` committed on Linux and checked out on Windows with default `core.autocrlf=true` arrives with CRLF, fails the strict parser, and breaks every `world_hash`. Required mitigations, all three:

```gitattributes
# .gitattributes -- REQUIRED. Without this, committed regression worlds are
# corrupted by git's newline translation on Windows checkout.
* text=auto eol=lf
*.thesis   -text
*.jsonl    -text
*.json     -text
```

Plus: all recorder file I/O is binary; plus a targeted parse error; if `Parse` sees `\r\n` it reports *"CRLF line endings detected: git has translated this file. Check .gitattributes and `git config core.autocrlf`."* rather than a generic syntax error.

**Filename safety.** Node ids come from user config and end up in log filenames.

```go
// SafeName validates an identifier for use as a Windows path component.
// Rejects: anything outside ^[A-Za-z0-9_.\-]{1,64}$; the reserved device names
// CON PRN AUX NUL COM1-9 LPT1-9 (case-insensitive, with or without extension);
// trailing dots and trailing spaces. These are silent file-creation failures
// on NTFS, not errors you can debug from a stack trace.
func SafeName(s string) error
```

**Path length.** `MAX_PATH` is 260. `thesis init` validates the artifacts dir is under 150 characters and warns otherwise; Go's `os` package applies `\\?\` extended-path handling for absolute paths, but external tools invoked on those paths may not.

**Atomic writes.** Every JSON artifact: write `name.json.tmp` in the same directory, `f.Sync()`, `os.Rename` over the target (Go uses `MoveFileEx` with `REPLACE_EXISTING`, atomic on NTFS). Append-only logs use a `bufio.Writer` flushed at every phase transition and every fault event, and `Sync()`ed at phase boundaries.

## 3.6 Schema types

```go
// pkg/schema/world.go
// Fields are declared alphabetically to mirror wire order. A readability
// convention only -- the encoder sorts regardless.
type World struct {
    DriverProfile   DriverProfile `cjson:"driver_profile"`
    FaultSchedule   []Fault       `cjson:"fault_schedule"`
    Notes           string        `cjson:"notes,omitempty"`
    Origin          WorldOrigin   `cjson:"origin"`
    PhaseTimings    PhasePlan     `cjson:"phase_timings"`
    Schema          string        `cjson:"schema"` // "prothesis.world/v1"
    Seed            HexU64        `cjson:"seed"`
    TopologyVariant TopoVariant   `cjson:"topology_variant"`
    WorldHash       Hash          `cjson:"world_hash"`
}

type Fault struct {
    EndMs   int64            `cjson:"end_ms"`
    Kind    string           `cjson:"kind"`   // "net.partition"
    Params  map[string]int64 `cjson:"params"` // integers only; ratios in ppm
    StartMs int64            `cjson:"start_ms"`
    Target  string           `cjson:"target"` // "minority(kv)" | "n1<->n2" | "role:leader"
}

// String renders the normative grammar KIND(TARGET[, k=v...])@START..END with
// params in sorted-key order. It is the sort key that fixes Event.Seq, so it
// MUST be a pure function of the Fault. Also what appears in
// verdict.shrink.surviving_faults.
func (f Fault) String() string

// PhasePlan is PLANNED timing -- an input, hashed into world_hash.
// Observed timing lives in phases.json as []PhaseSpan and is NOT hashed.
type PhasePlan struct {
    AssertTimeoutMs int64 `cjson:"assert_timeout_ms"`
    BootTimeoutMs   int64 `cjson:"boot_timeout_ms"`
    DriveMs         int64 `cjson:"drive_ms"`
    HealMs          int64 `cjson:"heal_ms"`
    QuiesceMs       int64 `cjson:"quiesce_ms"`
    SeedTimeoutMs   int64 `cjson:"seed_timeout_ms"`
}

// WorldOrigin is provenance. Excluded from world_hash on purpose.
type WorldOrigin struct {
    Commit      string `cjson:"commit,omitempty"`
    ConfigHash  Hash   `cjson:"config_hash"`  // sha256 of the CJSON projection
                                              // of prothesis.yaml, so YAML
                                              // reformatting never rehashes
    CreatedBy   string `cjson:"created_by"`   // seed|manual|search|shrink
    ToolVersion string `cjson:"tool_version"`
}

// pkg/schema/scalars.go

// HexU64 is a uint64 serialized as exactly 16 lowercase hex digits. Avoids the
// 2^53 double-precision limit that would corrupt values in JavaScript and MCP
// consumers of prothesis.verdict/v1.
type HexU64 uint64

// Dec is an exact decimal serialized as a string at a per-field declared
// scale: Dec{Units:42, Scale:2} -> "0.42". Used only where the spec shows a
// float (verdict suspect.confidence). Never appears in .thesis.
type Dec struct { Units int64; Scale uint8 }

// Hash is "sha256:" + 64 lowercase hex digits.
type Hash string
```

Example canonical `.thesis` (alphabetical keys, integer-only numbers, hex seed, one trailing LF):

```json
{
  "driver_profile": {
    "clients": 16,
    "mix_ppm": { "read": 400000, "txn": 200000, "write": 400000 },
    "name": "gate",
    "ops": 20000
  },
  "fault_schedule": [
    {
      "end_ms": 15100,
      "kind": "proc.pause",
      "params": {},
      "start_ms": 8200,
      "target": "role:leader"
    },
    {
      "end_ms": 14900,
      "kind": "net.partition",
      "params": {},
      "start_ms": 8400,
      "target": "minority(kv)"
    }
  ],
  "origin": {
    "commit": "9c1e0f2",
    "config_hash": "sha256:1b4f0e98...",
    "created_by": "shrink",
    "tool_version": "prothesis/v1+dev"
  },
  "phase_timings": {
    "assert_timeout_ms": 60000,
    "boot_timeout_ms": 30000,
    "drive_ms": 42000,
    "heal_ms": 3000,
    "quiesce_ms": 5000,
    "seed_timeout_ms": 60000
  },
  "schema": "prothesis.world/v1",
  "seed": "000000000000002a",
  "topology_variant": { "name": "kv3", "nodes": 3 },
  "world_hash": "sha256:a41f9c2b7e10..."
}
```

## 3.7 The reflection codec and the linter

```go
// Marshal reflects a Go value into a canonical Value tree.
//
// Supported: struct (with `cjson:"name[,omitempty][,dec=N]"`), string, bool,
// int/int8/../int64, []T, map[string]T, and the named types HexU64, Dec, Hash.
//
// REJECTED at registration time: float32, float64, interface{}, pointers,
// uint/uintN (other than via HexU64), time.Time, non-string map keys, channels,
// funcs, untagged embedded structs, duplicate cjson names, and any key not
// matching ^[a-z][a-z0-9_]*$.
func Marshal(v any) (Value, error)
func Unmarshal(val Value, ptr any) error

// Lint walks a type and returns every rule violation.
// schema_lint_test.go runs Lint over EVERY registered schema type.
//
// This is the highest-value test in the whole recorder, because the risk to
// byte-identity is not today's fields -- it is the field someone adds in
// Phase 4. A `float64 Confidence` added to a world in six months would
// silently break every hash in the regression corpus. Lint makes that a
// compile-time-adjacent failure instead.
func Lint(t reflect.Type) []error
```

Per-type field plans (name, index, tag flags, sorted order) are computed once and memoized in a `sync.Map`.

## 3.8 How byte-identity is tested convincingly

A ladder, weakest to strongest. All of it runs in CI on a matrix of Go 1.22/1.23/1.24 × {windows/amd64, linux/amd64, linux/arm64, darwin/arm64}, comparing against the *same* committed golden bytes. The cross-platform matrix is what makes it convincing rather than merely green.

**1. Golden corpus.** `testdata/worlds/*.thesis`, hand-written and reviewed, covering: zero faults; 24 faults (the `max_faults_per_world` ceiling); every fault kind; overlapping windows; identical windows (tiebreak coverage); `start_ms == end_ms`; negative `clock.skew`; empty vs populated maps; string fields containing `<`, `>`, `&`, `"`, `\`, tab, newline, non-BMP emoji, RTL text; `±(2⁵³−1)` boundary integers. Test: `Encode(Decode(b)) == b`. A `-update` flag regenerates them, guarded so CI cannot run it.

**2. Fuzz round-trip.** Native Go fuzzing, seeded from the golden corpus:

```go
func FuzzWorldRoundTrip(f *testing.F) {
    for _, g := range goldens { f.Add(g) }
    f.Fuzz(func(t *testing.T, b []byte) {
        w, err := DecodeWorld(b)
        if err != nil { return } // non-canonical input is rejected, so the
                                 // fuzzer explores exactly the canonical
                                 // language
        out, err := EncodeWorld(w)
        if err != nil { t.Fatal(err) }
        if !bytes.Equal(b, out) { t.Fatalf("byte-identity violated") }
    })
}
```

Because `DecodeWorld` rejects non-canonical input, this is *literally* G3. 60 s in CI, 30 min nightly, crashers committed to `testdata/fuzz/`. This is what finds `-0`, `1e2`, `1.0`, duplicate keys, BOMs, overlong UTF-8 and lone surrogates.

**3. Generator property test: dogfooding.** A deterministic `genWorld(s *recorder.Stream) schema.World` produces arbitrary valid worlds. For 100k seeds:

```go
b1 := MustEncode(w)
w2 := MustDecode(b1)
b2 := MustEncode(w2)
require.Equal(b1, b2)                       // R2: idempotence
require.Equal(HashWorld(&w), HashWorld(w2)) // hash stability
require.True(canonicalEqual(w, *w2))        // semantic round-trip
```

The generator is driven by our own PRNG, so a failure *is a seed*: invariant I2 applied to the recorder's own test suite, exercising both subsystems at once.

**4. Injectivity (R3).** Worlds differing *only* by map insertion order, nil-vs-empty slice, or struct field order in a copy must encode **identically**. Worlds differing by one millisecond must encode **differently** and hash differently. Both directions, 100k cases.

**5. Map-order chaos.** Go randomizes map iteration per run. Encode the same map-bearing world 1000× in one process and 50× across processes (`-count=50`), asserting a constant result. Any map-order leak surfaces immediately.

**6. Cross-build determinism.** Compare output from `-gcflags=all=-N -l` against the optimized build, and `GOMAXPROCS=1` against `GOMAXPROCS=16`. With no floats and no libm these must be bit-identical; the test proves it rather than assuming it.

**7. Schema lint.** §3.7. Guards the future.

**8. Byte-mutation of goldens.** Apply single-byte mutations to golden bytes; assert `Decode` either errors or yields different re-encoded bytes. Catches "the decoder ignores this byte" bugs. 100k mutations, fast.

**9. CRLF / git simulation.** Convert a golden LF→CRLF; assert the loader rejects it with the line-endings-specific message. Assert `.gitattributes` exists and contains `*.thesis -text`.

**10. Hash goldens.** `testdata/world_hashes.json` maps golden file → expected `sha256:`. Any encoder change fails this, and since the regression corpus is content-addressed, that alarm is exactly what we want. This file is later referenced by `.prothesis/lock`.

**PRNG-specific:**

**11. RFC 8439 test vectors.** §2.3.2 keystream vectors plus §2.4.2: proves the core is real ChaCha20.

**12. Frozen stream goldens.** `testdata/streams/root_002a.json` records, per registered domain: first 16 `Uint64`; first 16 `Uint64n(1000)`; first 16 `Float64` **as IEEE-754 hex bit patterns, not decimal**; a `Shuffle` of 0..15; a `WeightedPPM` sequence. Run on every Go version in CI. This converts "reproducible across Go versions" from a claim into a check.

**13. Independence (G2, direct).** For k in 0..1000: draw k values from stream A, then 16 from stream B; assert B's 16 values are invariant in k. The requirement transcribed into a test.

**14. Seek equivalence.** `s.Index(i).Uint64()` equals a freshly derived stream at index i, for 10k random i up to 2⁶⁴; `Seek(n)` matches sequential consumption to n.

**15. Statistical sanity.** Chi-square on `Uint64n(k)` buckets and monobit on the raw keystream, loose thresholds, fixed seed so it never flakes. Catches a broken Lemire rejection bound, which would be deterministic *and* biased, the worst kind of bug, since every determinism test would still pass.

**Clock:**

**16. Conformance suite** run against both `realClock` and `simClock`: events fire in `(At, Op, Seq)` order; no event fires before `At`; every successful inject has a matching withdraw; `drain()` empties the stack under cancellation.

**17. Lateness injection.** A `simClock` delaying wakeups by a configured distribution; assert every row of the §1.6 table, including that withdrawals fire regardless of lateness.

**18. The `time.Now` ban.**

```go
// internal/recorder/determinism.go
//
// Scan reports ambient nondeterminism in a Go source tree using go/parser and
// go/ast: time.Now/Since/After/Sleep/Tick/NewTimer, math/rand, crypto/rand,
// os.Getenv in hot paths, and map range whose iteration order reaches output.
//
// Two consumers:
//   - determinism_scan_test.go, which FAILS THE BUILD if any of these appear
//     outside internal/recorder/clock_real.go and runid.go.
//   - thesis doctor (CRUCIBLE PART 5), which runs the same scanner over the
//     USER's repo and scores it toward Tier A.
//
// Building it here in Phase 0 makes `thesis doctor` a CLI wrapper later
// rather than a new subsystem.
func Scan(fsys fs.FS, allow []string) ([]Finding, error)
```

---

# PART 4: ARTIFACT BUNDLING & RUN LAYOUT

## 4.1 `run_id`

```go
// AllocRunID returns a fresh run_id matching the normative shape from
// prothesis.verdict/v1: r_2026_09_03_a41f
//
//   ^r_[0-9]{4}_[0-9]{2}_[0-9]{2}_[0-9a-f]{4}$   (21 chars)
//
//   - Date is UTC, not local. Local dates make CI runs sort incoherently
//     across time zones and produce two "2026_09_03" folders around midnight.
//   - The 4 hex chars come from crypto/rand, NOT the seeded stream: a run_id
//     identifies a RUN, and two runs of the same world are two runs.
//   - Uniqueness comes from os.Mkdir returning ErrExist atomically on NTFS;
//     we retry with fresh entropy up to 64 times. That keeps the exact 4-hex
//     spec shape while eliminating the birthday collision 16 bits would give
//     a `thesis watch` daemon doing hundreds of runs a day.
//   - No ':' anywhere, so it is a legal Windows path component.
func AllocRunID(runsDir string, wallUTC time.Time) (string, error)
```

World *files* use 12 hex chars (`w_a41f9c2b7e10.thesis`), not 4. World files are content-addressed and merged across git branches, so they cannot be uniquified by retry; 16 bits collides at ~300 files by the birthday bound, and a regression corpus will exceed that. Documented deviation from the illustrative `w_a41f.thesis`; the loader accepts any `w_<hex>.thesis`.

## 4.2 Directory layout

```
.prothesis/
├── lock                                  # Phase 3
├── oracles/                              # Phase 3
├── corpus/                               # Phase 4
├── regressions/                          # Phase 5, committed to the repo
│   └── w_a41f9c2b7e10.thesis
└── runs/
    └── r_2026_09_03_a41f/
        ├── RUNNING                       # sentinel; absent => clean teardown
        ├── run.json                      # prothesis.run/v1
        ├── verdict.json                  # prothesis.verdict/v1   (Phase 1)
        ├── clock.json                    # anchor, calibration, drift, anomalies
        ├── env.json                      # os, arch, go version, docker version, cpus
        ├── MANIFEST.json                 # sha256 + size of every file
        ├── worlds/
        │   ├── index.jsonl               # ordinal -> world_hash -> outcome
        │   └── 000000/
        │       ├── world.thesis
        │       ├── world_hash            # one line, for grep and scripts
        │       ├── history.jsonl         # from the driver (Phase 1)
        │       ├── phases.json           # OBSERVED spans, virtual ms
        │       ├── timeline.jsonl        # recorder events
        │       ├── streams.json          # per-stream draw audit
        │       ├── final_state.json      # Phase 1
        │       ├── telemetry.json        # Phase 1
        │       ├── oracles/<name>.json   # Phase 3
        │       └── logs/
        │           ├── <node_id>.stdout.log
        │           ├── <node_id>.stderr.log
        │           └── docker_events.jsonl
        └── driver/
            ├── driver.stdout.log
            └── driver.stderr.log
```

World directories are keyed by 6-digit **ordinal**, not by hash, because Phase 5's confirmation gate executes the same world k times: hash is not unique within a run. `worlds/index.jsonl` gives ordinal → hash → outcome for fast scanning without walking the tree. `oracle_input`'s `history_path` / `world_path` are absolute paths into this tree.

## 4.3 The Bundle API

```go
// Bundle owns one run directory. Every artifact write goes through it, so
// atomicity, path sanitation and the manifest cannot drift apart.
type Bundle struct {
    Root  string
    RunID string
    clock Clock
    mu    sync.Mutex
    files map[string]fileRec // rel path -> size, running sha256
}

func OpenBundle(artifactsDir string, clock Clock) (*Bundle, error) // allocates run_id

// WriteJSON canonically encodes v and atomically replaces rel:
// write rel+".tmp" -> Sync -> Rename. Hashes as it writes.
func (b *Bundle) WriteJSON(rel string, v any) error

// Appender returns a crash-safe append-only writer. Flushed at every phase
// transition and every fault event; Synced at phase boundaries; flushed on
// abort. Used for history.jsonl and timeline.jsonl.
func (b *Bundle) Appender(rel string) (*Appender, error)

// WorldDir allocates worlds/<6-digit ordinal>/ and returns a sub-Bundle
// sharing the parent's manifest.
func (b *Bundle) WorldDir(ordinal int) (*Bundle, error)

// Close flushes everything, writes MANIFEST.json, and removes RUNNING.
func (b *Bundle) Close() error
```

`MANIFEST.json`:

```json
{
  "schema": "prothesis.manifest/v1",
  "bundle_sha": "sha256:...",
  "files": [
    { "path": "run.json", "sha256": "sha256:...", "size": 1842 },
    { "path": "worlds/000000/history.jsonl", "sha256": "sha256:...", "size": 3910221 }
  ]
}
```

`bundle_sha` = SHA-256 over the *canonical encoding of the sorted file list*, never over a compressed archive. `compress/flate`'s output is not guaranteed stable across Go versions; hashing the manifest sidesteps that entirely while still giving an end-to-end content address, and it is the natural hook for CRUCIBLE PART 6.3 mechanism 5 (verdict signing, v3) with no redesign.

For transport, `thesis report --bundle` writes a **reproducible** `tar.gz` using only stdlib: entries sorted by path, `mtime=0`, `uid=gid=0`, `uname=gname=""`, mode `0644`/`0755`, USTAR format, gzip header with zero mtime and no filename. Verification is against `MANIFEST.json`, not against the archive bytes.

## 4.4 `timeline.jsonl`

Each line is an independently canonical CJSON object plus `\n`, so the file is itself content-hashable and greppable.

```jsonl
{"kind":"phase.enter","phase":"BOOT","rt_ns":1204100,"v_ms":-42100}
{"kind":"clock.origin","rt_ns":42101300000,"wall_ns":1788451200000000000}
{"kind":"phase.enter","phase":"DRIVE","rt_ns":42101300000,"v_ms":0}
{"fault":"proc.pause(role:leader)@8200..15100","initiated_v_ms":8203,"kind":"fault.inject.initiated","late_ms":3,"planned_v_ms":8200,"rt_ns":50304400000}
{"actuator_latency_ms":195,"effective_v_ms":8398,"fault":"proc.pause(role:leader)@8200..15100","kind":"fault.inject.effective","rt_ns":50499500000}
{"kind":"clock.anomaly","rt_ns":51000000000,"v_ms":8899,"wall_drift_ms":1420}
```

`rt_ns` stays well under 2⁵³ (2⁵³ ns ≈ 104 days), so it is a safe JSON integer.

**`history.jsonl` is deliberately exempt from CJSON.** It is produced by an *external* driver process (§4.2 `driver.cmd`), so we cannot dictate its byte layout: requiring canonical form would violate CRUCIBLE PART 10's "must work on unmodified systems". We ingest it with `json.Decoder` + `UseNumber()` into exact `int64` (essential, since `t_ns` carries absolute Unix nanoseconds, which exceed 2⁵³ and would lose precision through a `float64`), and its integrity is covered by `MANIFEST.json`. The CJSON contract applies to `.thesis`, `run.json`, `verdict.json`, `phases.json`, `timeline.jsonl`, `streams.json` and `MANIFEST.json`.

## 4.5 Retention

```go
// Retain implements artifacts.retain_passing / retain_failing.
//
// - Ordering is by run.json.started_wall_ns, NOT run_id lexicographic: within
//   a single day the 4 hex chars are random, so run_ids do not sort
//   chronologically.
// - A run referenced by any .prothesis/regressions/*.thesis is NEVER pruned.
// - Windows: a run dir may be undeletable because Docker Desktop or an editor
//   holds a handle. Pruning first RENAMES into .prothesis/runs/.trash/ (which
//   succeeds even with open handles on NTFS) and deletes best-effort. A
//   retention failure NEVER fails the run. .trash is also swept at run start,
//   not only at teardown.
func Retain(runsDir string, cfg schema.ArtifactsConfig) error
```

---

# PART 5: WIRING

```go
// Recorder is the composition root every other subsystem receives.
type Recorder struct {
    Clock    Clock
    Timeline *Timeline
    Bundle   *Bundle
    Streams  *Streams
    Seq      *Sequencer
}

// New wires a Tier B recorder for one world.
func New(cfg Config) (*Recorder, error)

// Phase 0 ships:
//   - realClock, simClock, Timeline
//   - Sequencer + a NopActuator (perturber supplies the real one in Phase 2)
//   - the full ChaCha/HMAC stream tree
//   - the full CJSON codec, EncodeWorld/DecodeWorld/HashWorld
//   - Bundle, AllocRunID, Retain, SafeName, determinism.Scan
//
// Phase 0 does NOT ship: fault implementations, oracles, search, shrinking.
// It forecloses none of them: Actuator is the perturber's seam, Clock is
// Tier A's seam, the Streams domain table is the search engine's seam, and
// index-derived streams are what make Phase 5's shrinker stable.
```

**Phase 0 definition of done, mapped:**

- *(b) byte-identical round-trip* → §3.3 (runtime invariant) and §3.8 (the test ladder). This is the recorder's own deliverable.
- *(a) `thesis up && thesis down`* → harness and CLI, but the recorder supplies `AllocRunID`, `Bundle`, `SafeName` and the clock those commands stamp their timeline with.
