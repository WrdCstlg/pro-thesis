# 08-saboteur.phase0-reachback

## Summary

Addendum A forces surprisingly little *behaviour* into Phase 0, but it forces several *format* commitments that Phase 0 would otherwise freeze in an unusable shape. The single largest finding is structural, not additive: `prothesis.verdict/v1` is a per-RUN aggregate while A.6's `Utility()` scores a single world, so `WorldResult` can never be the verdict; it is an internal Phase 1 type, and Phase 0's only obligation is that every nested verdict object be a named exported type rather than an anonymous inline struct. The top-level `search:` block does NOT collide with the base spec's profile-level `search: true`; I verified empirically that yaml.v3 rejects both confusion directions with clean typed errors, and that `DefaultConfig()` + decode-in-place solves the zero-vs-unset problem with no pointers and no custom `UnmarshalYAML`: the latter being an active trap, since `KnownFields(true)` provably does not propagate through a nested `node.Decode` and would silently swallow config typos. Four further Phase 0 deltas are load-bearing: domain-keyed PRNG stream derivation (so Phase 4's seven Saboteur streams cannot perturb Phase 0's, verified), a world file carrying provenance/parent-hash/MCTS-path/realized-vs-planned slots under a strict canonical-JSON encoding with no maps and no bare floats, a timestamped JSONL telemetry format that sidesteps the A.3/A.4 200ms-vs-500ms conflict entirely, and a tiered metric model that resolves the goroutine-count-vs-unmodified-systems contradiction honestly by reporting INCONCLUSIVE rather than PASS on absent metrics.

## Findings (15)

### F1 [ACCEPT-AND-DOCUMENT] **(AFFECTS PHASE 0)** Top-level `search:` does not collide with the profile-level `search: true`: verified in both directions

Base spec §4.2 defines `profiles.soak.search: true` as a BOOLEAN inside a profile; Addendum A.10 defines a top-level `search:` MAPPING. These resolve to `Config.Profiles[name].Search bool` and `Config.Search SearchConfig`: different parents, different types, no shadowing. I tested both confusion directions with yaml.v3 and both fail loudly rather than silently: top-level `search: true` gives `line 7: cannot unmarshal !!bool 'true' into Search`, and a profile-level `search: {strategy: saboteur}` gives `line 3: cannot unmarshal !!map into bool`. Each error names the correct line. The residual risk is human confusion, not parser ambiguity.

**Recommendation:** Keep both keys exactly as specified. Define the layering explicitly: the profile boolean gates WHETHER search engages; the top-level block configures HOW it behaves once engaged. Map both yaml type errors to exit code 5. Have `thesis init` scaffold the top-level `search:` block commented out with defaults shown, cross-referencing `profiles.soak.search`, so the two-level naming is self-documenting.

### F2 [RESOLVABLE-WITH-DECISION] **(AFFECTS PHASE 0)** A custom UnmarshalYAML for defaulting would silently swallow config typos: KnownFields does not propagate

The idiomatic-looking defaulting pattern (`type raw T; n.Decode((*raw)(s))` inside a custom UnmarshalYAML) breaks strict field checking. I verified that `Decoder.KnownFields(true)` is NOT propagated through a nested `node.Decode`: decoding `inner: {a: 1, zzz: 9}` returned `err=nil` and silently discarded `zzz`. Applied to SearchConfig, this means `probe_budget_pctt: 50` would be ignored and the Saboteur would silently run at the default 20% while the operator believed it was 50%. That is a config-drift/gate-weakening vector, not just an ergonomics issue. The alternative (pre-seeding with DefaultConfig() and decoding in place) preserves nested defaults for absent fields, lets explicit zeros win, and keeps KnownFields active: verified that `fault_penalty: 0` overrode the default 3 while an untouched `observe:` sub-block kept all its defaults, and that `probe_budget_pctt` was correctly rejected.

**Recommendation:** Use `DefaultConfig()` + decode-in-place with `Decoder.KnownFields(true)`. No pointer fields (a nil pointer field causes yaml.v3 to allocate a fresh zero struct, discarding nested defaults) and no custom UnmarshalYAML anywhere in the config path. Record in DECISIONS.md with the KnownFields finding as the stated rationale, so a later contributor does not 'improve' it back to a custom unmarshaler.

### F3 [RESOLVABLE-WITH-DECISION] **(AFFECTS PHASE 0)** prothesis.verdict/v1 is a per-RUN aggregate; A.6's Utility() scores a single world: WorldResult cannot be the verdict

Mapping A.6's reads onto the frozen verdict: `Violations[].Class`, `Violations[].Severity`, `Coverage.NewTemplates` and `Coverage.NewStates` all EXIST. `FaultCount` and `DurationSeconds` do NOT. But the deeper problem is scope: the verdict carries `budget.worlds_run: 22`, one flat `violations` array and one `coverage` block; it aggregates an entire run. A.6's Utility is invoked once per MCTS rollout, on one world. So per-world utility is uncomputable from a verdict even if the two missing fields were added. Near-misses that must not be mistaken for them: `violations[].shrink.faults_before` is per-violation and post-shrink (Phase 5), and `budget.used_s` is the run's total seconds, not one world's duration.

**Recommendation:** Define WorldResult in `internal/control` as an internal type with fields World, WorldHash, Violations []schema.Violation, Coverage schema.Coverage, FaultCount int, DurationSeconds float64, Status. It is NOT a schema type: no `schema:` discriminator, no external consumer, not frozen, not cross-repo portable. Land it in Phase 1 (the first phase that executes a world), not Phase 0. Source FaultCount from len(World.Faults.Realized) and DurationSeconds from the recorder. Add nothing to prothesis.verdict/v1.

### F4 [BLOCKING] **(AFFECTS PHASE 0)** Anonymous inline structs in the Verdict would make WorldResult unbuildable without breaking pkg/schema

Reading §4.6's JSON literally invites `Violations []struct{...}` and `Coverage struct{...}` as anonymous inline types. WorldResult must embed Violation and Coverage BY VALUE at sub-verdict granularity. If Phase 0 freezes them as anonymous, Phase 4 cannot name the element type and must break the frozen package to construct a WorldResult. This is the entire concrete Phase 0 obligation arising from A.6, and it costs nothing today.

**Recommendation:** Every nested object in the verdict is a named exported type: Violation, Coverage, VerdictBudget, CoverageDelta, Witness, MinimalRepro, Shrink, CausalEvent, Suspect, OracleLock. Also pin the vocabulary as typed constants (Class (all eight CRUCIBLE §B classes) and Severity (high/medium/low)) since A.6 switches on these exact strings.

### F5 [ACCEPT-AND-DOCUMENT] A.6's utility function scores only 6 of the 8 normative oracle classes

CRUCIBLE §B defines eight oracle classes: crash, consistency, liveness, convergence, resource, safety, differential, metamorphic. A.6's Utility() switch has cases for six of them and none for `differential` or `metamorphic`, so violations in those two classes contribute exactly zero utility. The Saboteur is therefore structurally incapable of searching toward a differential or metamorphic bug: it will treat finding one as worth nothing and steer away. A.6 is explicitly normative and must not be modified without a DECISIONS.md entry.

**Recommendation:** Do not fix in Phase 0. Define all eight Class constants in pkg/schema so the gap is testable rather than latent, and file the OPEN_QUESTIONS.md entry now so it is a human decision before Phase 4 rather than a discovery mid-implementation.

### F6 [ACCEPT-AND-DOCUMENT] A.10's utility knobs do not cover A.6's hardcoded weights

A.10 exposes a single `violation_weight: 100`, but A.6 hardcodes a per-class ladder of 100/80/60/40/20. It is unspecified whether violation_weight is the consistency-class base with the rest as fixed ratios, a global scale factor, or applicable only to the consistency case. Likewise `novelty_weight: 10` covers log templates only, while A.6 hardcodes 5.0 per novel state with no configuration knob at all.

**Recommendation:** Phase 0 parses and defaults these fields exactly as A.10 specifies and does nothing more; the semantics are a Phase 4 runtime question. Do not invent a `state_novelty_weight` field; that would be inventing schema. File as an OPEN_QUESTIONS.md entry.

### F7 [ACCEPT-AND-DOCUMENT] **(AFFECTS PHASE 0)** Telemetry sampling interval is specified three times with two different values

A.3 requires the probe telemetry trajectory 'sampled every 200ms during DRIVE'. A.4 requires container/process metrics 'Sampled every 500ms'. A.10 exposes one knob, `observe.sample_interval_ms: 500`. A.3 and A.4 are in direct conflict and A.10 cannot express both. There is also a correctness corollary: A.4's linearRegression over trend and acceleration must regress against real elapsed timestamps, not sample index; index-based regression silently rescales every slope when the rate changes, and `reinforce_threshold` defaults to 0.0, exactly the boundary where a rescale flips the DAMPEN/REINFORCE classification.

**Recommendation:** Neutralise it in Phase 0 as a format decision rather than a rate decision: timestamp every telemetry record with `t_ns` and treat the header's `interval_ms` as advisory metadata only. Never infer the rate from record position. Phase 4 can then adopt 200ms, 500ms, or a probe-mode override with zero format change. Write the timestamp-not-index regression rule into DECISIONS.md now, before anyone implements the spiral detector.

### F8 [RESOLVABLE-WITH-DECISION] **(AFFECTS PHASE 0)** Goroutine count is unobservable from an unmodified process, contradicting CRUCIBLE §L

CRUCIBLE §L requires PRO-THESIS to work on unmodified systems, which rules out requiring instrumentation. Base spec Phase 1 oracle 4 requires 'RSS, FDs, goroutines return to within ±N% of baseline' and A.4 lists 'thread/goroutine count' as a telemetry source. A Go program's goroutine count is a runtime-internal quantity with no external observation point. cgroup v2's pids.current gives OS task/thread count, which is a different number: reporting it as a goroutine count would make the oracle assert something it is not measuring.

**Recommendation:** Three tiers encoded in the format. Tier 0 (always available via Docker API/cgroup v2, genuinely unmodified): rss_bytes, cpu_pct, fd_count, task_count. Tier 1 (opt-in, still zero code change): scrape go_goroutines only if the target ALREADY exposes pprof or Prometheus. Tier 2: out of scope, a `thesis doctor` recommendation. Name the Tier 0 metric `task_count` and keep `goroutines` a separate optional field: do not conflate them. The header declares which metrics the run could collect, and an oracle whose required metric is absent must return INCONCLUSIVE (exit 2), never PASS: silently passing on a missing metric is precisely the gate-weakening I6 exists to prevent.

### F9 [BLOCKING] **(AFFECTS PHASE 0)** Sequential PRNG child-seeding would invalidate every regression world when Phase 4 adds Saboteur streams

The Saboteur needs at least nine new named streams (probe.order, probe.params, ladder.tiebreak, uct.tiebreak, mcts.expand, mcts.rollout, mcts.target, search.select, search.mutate). If Phase 0 derives streams by drawing from a global generator (`rand.New(rand.NewSource(global.Int63()))`), stream values depend on creation ORDER, so adding any Saboteur stream reshuffles every existing stream and breaks replay for every .thesis world committed during Phases 0-3. I verified that domain-keyed derivation; sha256('prothesis/v1/prng' || 0 || be64(root) || 0 || name) seeding a rand/v2 ChaCha8; has the required property: after creating and drawing 100 values from two Phase 4 streams and 10,000 from another, both Phase 0 streams (fault.schedule, driver.workload) produced bit-identical sequences.

**Recommendation:** Adopt domain-keyed derivation in Phase 0 and state the property normatively: for a fixed root seed, a stream's values depend only on (root, name, draws taken from that stream), never on the existence, creation order, or draw counts of any other stream. Ban three anti-patterns by test: sequential child seeding, package-level math/rand, and stdlib convenience helpers (Shuffle/Perm/IntN) whose algorithms are not covered by the Go compatibility promise and could change replay results on a toolchain upgrade. Implement Shuffle/Perm/IntN/Float64 inside internal/recorder over ChaCha8.Uint64. Ship a golden vector test that Phase 4 must not change.

### F10 [BLOCKING] **(AFFECTS PHASE 0)** Explicit `[]` versus absent breaks the byte-identical round-trip DoD unless the decoder is strict

An `omitempty` slice has two legal input spellings (the field absent, and the field present as `[]`) which decode to the same Go value but re-encode to only one. I verified this: a world file containing `"planned":[]` and `"tree_path":[]` re-encoded with both fields omitted, so a naive decode-then-encode round-trip test fails on legal-looking input. The same class of hazard applies to Go maps (ordering) and bare floats (-0, exponent form, shortest-representation changes across Go versions) in a format that must round-trip byte-identically and is committed to the repo as a permanent regression corpus.

**Recommendation:** Define the canonical form as encode(struct) and make the decoder strict: DisallowUnknownFields, and reject explicit []/{} for omitempty fields. Adopt three format rules now, since retrofitting any is a break: no Go maps anywhere in the world file (ordered []Param{Key,Value string} instead), no bare floats (integer milliseconds or string Param.Value), and durations as integer ms with duration strings confined to prothesis.yaml. State the DoD test as a canonical fixed point: encode(decode(encode(S))) == encode(S) for golden worlds with all optional fields absent and with all present, plus rejection of non-canonical input. Both directions verified green.

### F11 [RESOLVABLE-WITH-DECISION] **(AFFECTS PHASE 0)** Seed representation must be decided before the first world file is written

A uint64 seed above 2^53 loses precision in any JavaScript consumer, and CRUCIBLE §L requires world and oracle formats to be portable across repos rather than tied to one project's toolchain. Once .thesis files are committed to .prothesis/regressions/ as a permanent append-only corpus (CRUCIBLE §E4), the seed's wire representation cannot be changed without invalidating them.

**Recommendation:** Represent the seed as a 16-lowercase-hex string with a strict parser. The CLI continues to accept decimal (`thesis run --seed N`) and canonicalises on write. Verified that both non-canonical case ('9C1E0F2A41F00D1E') and the numeric form are rejected, which keeps the canonical form unique and the round-trip well-defined.

### F12 [RESOLVABLE-WITH-DECISION] **(AFFECTS PHASE 0)** The config digest must cover the `search:` block or it becomes an unguarded gate-weakening vector

CRUCIBLE §E2 makes profile budgets and the allowed fault space lock-covered because narrowing them weakens the gate. Setting `search.strategy: random` or `probe_budget_pct: 100` weakens the gate by exactly the same mechanism (it makes the Saboteur search less hard) but nothing in the base spec's lock description covers it. Because the world file will carry a config_digest and .prothesis/lock will consume the same function, the digest algorithm is frozen the moment Phase 0 writes its first world file.

**Recommendation:** Define config_digest = sha256(json.Marshal(resolvedConfig)) over the RESOLVED, defaulted config, not raw file bytes, which would flag whitespace churn while missing a change in a compiled-in default. encoding/json sorts map keys, so profiles and driver.profiles digest deterministically. This places the whole search: block inside the I6 lock for free. Requires json tags on every config field in Phase 0 even though nothing reads config as JSON yet.

### F13 [RESOLVABLE-WITH-DECISION] **(AFFECTS PHASE 0)** The .thesis world file has no schema discriminator anywhere in the normative text

Base spec §4.6 names prothesis.verdict/v1, prothesis.oracle_input/v1 and prothesis.oracle_output/v1, and CRUCIBLE maps .crux to .thesis, but neither document ever gives the world file a schema identifier. Every other artifact in the system is self-describing. Adding one later to a committed, append-only regression corpus is a break.

**Recommendation:** Adopt `prothesis.world/v1` as the discriminator, emitted as the first field. Flag it explicitly in both DECISIONS.md and OPEN_QUESTIONS.md, because it is an invented schema name in a directive whose rule 2 forbids inventing field names: it should be recorded as a conscious choice rather than slipped in.

### F14 [ACCEPT-AND-DOCUMENT] **(AFFECTS PHASE 0)** exploration_constant: using math.Sqrt2 instead of the literal 1.41 would diverge replays

A.5 and A.10 both write the value `1.41` and gloss it as '(√2)'. math.Sqrt2 is 1.4142135623730951. Substituting the 'more correct' constant changes UCT child selection and would make any replay diverge from a world recorded under the literal: a subtle, hard-to-diagnose reproducibility failure that would surface only in Phase 4 as flaky MCTS replays.

**Recommendation:** Use the literal 1.41 as the default, and record in DECISIONS.md explicitly why math.Sqrt2 is wrong here, so a later contributor does not 'correct' it.

### F15 [ACCEPT-AND-DOCUMENT] **(AFFECTS PHASE 0)** The frozen oracle contract shows telemetry.json but the Saboteur requires a tailable JSONL stream

Base spec §4.5 shows `"telemetry_path": "/path/to/telemetry.json"`. A.4 requires the Saboteur to read live signals during execution to classify DAMPEN/REINFORCE, and A.5 requires escalation mid-world. A single JSON document cannot be appended to or tailed while a world is still running, so a .json-shaped telemetry artifact is incompatible with OBSERVE.

**Recommendation:** `telemetry_path` is the frozen FIELD NAME; the path VALUE is not frozen. Emit telemetry.jsonl and point telemetry_path at it: fully compliant, no schema change. Keep raw application logs verbatim in logs/<node>.log for no_panic_log and put only derived log-template counts in telemetry, so the file stays small enough to sample at 200ms and cheap enough to tail live.

## Phase 0 deltas (22)

- **pkg/schema/config.go**: Add SearchConfig, UtilityConfig and ObserveConfig structs with the exact A.10 yaml tags (strategy, exploration_constant, probe_budget_pct, max_mcts_depth, escalation_ladder, utility.{violation_weight,novelty_weight,fault_penalty,duration_penalty}, observe.{sample_interval_ms,spiral_window,reinforce_threshold}). All fields are plain value types: no pointers. All weights are float64 even where A.10 shows integer literals. Wire as `Search SearchConfig \`yaml:"search"\`` on Config.
  - _why:_ A.10 is normative and optional-with-defaults. Adding the block later is additive and harmless, but choosing pointer fields or float-vs-int now is effectively permanent once configs exist in the wild. Value types are required for the decode-in-place defaulting mechanism to work.
- **pkg/schema/config.go**: Add `Search bool \`yaml:"search"\`` to the Profile struct (base spec §4.2 `profiles.soak.search: true`), and document in a comment that it is unrelated to the top-level search mapping: the boolean gates WHETHER search engages, the mapping configures HOW.
  - _why:_ Verified there is no parser collision in either direction (both confusions produce clean typed errors) but the two-level naming is a standing trap for humans. A comment at the definition site is the cheapest permanent mitigation.
- **pkg/schema/config.go**: Add DefaultConfig()/DefaultSearchConfig() returning strategy=saboteur, exploration_constant=1.41 (literal, not math.Sqrt2), probe_budget_pct=20, max_mcts_depth=4, escalation_ladder=default, utility={100,10,3,0.1}, observe={500,5,0.0}. Add strategy/ladder string constants.
  - _why:_ A.10 requires defaults for every field while explicit zeros stay meaningful. Pre-seeding is the mechanism that makes zero-vs-unset work without pointers. math.Sqrt2 would silently change UCT selection and diverge replays.
- **pkg/schema/load.go**: LoadConfig starts from DefaultConfig() and decodes in place with yaml.Decoder.KnownFields(true). No custom UnmarshalYAML anywhere in the config path. All decode failures wrap into a ConfigError the CLI maps to exit code 5.
  - _why:_ Verified: decode-in-place preserves nested defaults for absent fields and lets explicit zeros win, while KnownFields catches typos. Verified that KnownFields does NOT propagate through a custom UnmarshalYAML's nested node.Decode: a custom unmarshaler would silently swallow `probe_budget_pctt`, which is a gate-weakening vector under CRUCIBLE §E.
- **pkg/schema/config.go**: Add SearchConfig.Validate() enforcing strategy in {saboteur,random,hybrid}, escalation_ladder in {default,custom}, exploration_constant >= 0, probe_budget_pct in 0..100, max_mcts_depth >= 0, spiral_window >= 2, sample_interval_ms > 0. Call it from Config.Validate().
  - _why:_ Costs nothing now and converts Phase 4 runtime panics into a clean exit-5 at load time. spiral_window >= 2 matters specifically: A.4 fits a linear regression over the window and it is undefined below 2 points.
- **pkg/schema/config.go**: Add json tags to every config field alongside the yaml tags, and add ConfigDigest() = sha256(json.Marshal(resolvedConfig)) over the resolved, defaulted config.
  - _why:_ The digest goes into every world file, so its definition is frozen from the first world written. Resolved-config digesting (not raw bytes) catches default changes and ignores whitespace, and it places the whole search: block inside the I6 lock for free.
- **pkg/schema/verdict.go** (Define every nested verdict object as a NAMED EXPORTED type) Violation, Coverage, VerdictBudget, CoverageDelta, Witness, MinimalRepro, Shrink, CausalEvent, Suspect, OracleLock, never anonymous inline structs. Add no new fields to prothesis.verdict/v1.
  - _why:_ This is the single concrete Phase 0 obligation from A.6. WorldResult must embed schema.Violation and schema.Coverage by value at sub-verdict granularity; anonymous types would leave Phase 4 unable to name the element type without breaking the frozen package.
- **pkg/schema/verdict.go**: Add `type Class string` with all eight CRUCIBLE §B constants (crash, consistency, liveness, convergence, resource, safety, differential, metamorphic) and `type Severity string` with high/medium/low. Use them as the field types on Violation.
  - _why:_ A.6's Utility() switches on these exact strings, so pinning the vocabulary now makes the differential/metamorphic scoring gap testable rather than latent, and prevents typo'd class strings from silently scoring zero.
- **pkg/schema/world.go**: Define World with Schema ("prothesis.world/v1"), Seed, TopologyVariant, Driver (profile + ordered params), Faults (FaultSchedule), PhasePlan []PhaseTiming, ConfigDigest, and Meta. FaultSchedule carries Adaptive bool, Realized []Fault (always present, authoritative), Planned []Fault (omitted when identical), and Policy *Policy.
  - _why:_ Covers the I2 tuple and the stipulated realized-schedule resolution. Adaptive/Planned/Policy make the world self-describing about whether its schedule was planned or realized; adding them in Phase 4 would break every committed regression world.
- **pkg/schema/world.go**: Define WorldMeta with Origin (closed enum: seeded, mutated, saboteur-probe, saboteur-mcts, shrunk, manual), Parent ("sha256:..."), TreeID, TreePath []int, OracleLock, Note. Define WorldHash() as sha256 over the world tuple with Meta zeroed out.
  - _why:_ Provenance slots must exist from day one; adding the fields later invalidates the append-only regression corpus. TreePath as []int (child indices) round-trips canonically and cannot drift out of sync with Faults.Realized the way a []string of fault expressions would. Excluding Meta from the hash is what keeps corpus dedup and parent chains stable once Phase 4 generates worlds by several different origins.
- **pkg/schema/world.go**: Define Param{Key,Value string} and use ordered []Param everywhere a key/value set is needed (driver params, fault params, policy params). No Go maps and no bare float fields anywhere in the world file. All durations are integer milliseconds.
  - _why:_ Byte-identical round-trip DoD. Map ordering and float formatting (-0, exponent form, shortest-representation drift across Go versions) are the two classic ways this DoD dies quietly, and both are unfixable later without breaking the format.
- **pkg/schema/world.go**: Define `type Seed uint64` with MarshalJSON emitting exactly 16 lowercase hex chars and UnmarshalJSON rejecting non-canonical case and numeric form. CLI accepts decimal and canonicalises on write.
  - _why:_ Verified both rejections work. A raw uint64 above 2^53 loses precision in JavaScript consumers, and CRUCIBLE §L requires world formats to be portable across repos. Frozen the moment the first world file is committed.
- **pkg/schema/world.go**: Implement canonical encode (json.Encoder, SetEscapeHTML(false), SetIndent("","  "), trailing newline) and a STRICT decode (DisallowUnknownFields, plus rejection of explicit []/{} for omitempty fields).
  - _why:_ Verified that a world file containing `"planned":[]` re-encodes with the field omitted, so absent and [] are two input spellings for one value and a naive round-trip test fails on legal-looking input. Strict decoding makes the canonical form unique and the DoD well-posed.
- **pkg/schema/world_test.go**: Round-trip DoD test as a canonical fixed point: for golden worlds with (a) every optional field absent and (b) every optional field populated, assert encode(decode(encode(S))) == encode(S); assert strict decode accepts the canonical bytes; assert strict decode rejects non-canonical input (explicit [], unknown field, uppercase seed, numeric seed).
  - _why:_ Phase 0's stated Definition of Done is byte-identical world round-trip. Both directions verified green in the probe; the negative cases are what stop the format drifting in Phase 4.
- **internal/recorder/streams.go**: Implement domain-keyed stream derivation: streamKey = sha256("prothesis/v1/prng" || 0x00 || be64(root) || 0x00 || name), seeding a rand/v2 ChaCha8 per stream. Streams created lazily, keyed deterministically. Add Derive(name, sub) for hierarchical per-world/per-node/per-tree-node sub-streams.
  - _why:_ Guarantees the required property; a stream's values depend only on (root, name, draws taken) and never on other streams' existence, creation order, or draw counts. Verified: creating and heavily drawing from two Phase 4 Saboteur streams left both Phase 0 streams bit-identical. Sequential child seeding would reshuffle every stream when Phase 4 adds its nine, invalidating every regression world from Phases 0-3.
- **internal/recorder/streams.go**: Implement IntN (Lemire), Shuffle (Fisher-Yates), Perm and Float64 inside the package over ChaCha8.Uint64. Register the Phase 0 stream names only (fault.schedule, driver.workload, harness.boot) in one central const block; Get on an unregistered name panics in tests.
  - _why:_ Stdlib rand.Rand.Shuffle/Perm/IntN algorithms are not covered by the Go compatibility promise, so a toolchain upgrade could silently change replay results for every committed regression world. ChaCha8 itself is a specified, stable core and is safe to depend on.
- **internal/recorder/streams_test.go** (Five tests: (1) golden vector) first 16 uint64s of every registered Phase 0 stream frozen to a file Phase 4 must not change; (2) stream-addition (different creation orders with extra streams interleaved produce identical sequences; (3) draw-count independence) 10,000 draws from A leave B unmoved; (4) name registry; uniqueness and regex conformance; (5) no-global-rand source scan outside internal/recorder.
  - _why:_ Test (1) is the actual regression guard for the whole determinism story: it is the artifact that fails loudly if Phase 4 adds a Saboteur stream in a way that perturbs Phase 0 streams.
- **pkg/schema/telemetry.go**: Define the prothesis.telemetry/v1 JSONL types: TelemetryHeader (schema, type, run_id, world_hash, start_wall_ns, interval_ms as ADVISORY, nodes, metrics availability list), TelemetrySample (t_ns, node, phase, pointer-valued rss_bytes/cpu_pct/fd_count/task_count/goroutines), TelemetryProbe, TelemetryDriver, TelemetryLogTemplate, TelemetryEvent. Timestamps are t_ns on the recorder run clock, matching the frozen history-log schema.
  - _why:_ The format is referenced by the frozen oracle_input contract and consumed by external oracles, so it is a near-frozen cross-repo contract that Phase 0 is the right place to fix. Timestamping every record (rather than encoding a rate) is what makes the A.3-vs-A.4 200ms/500ms conflict a Phase 4 decision with no format change. Pointer metrics distinguish 'not collected' from zero, which the tiered-availability resolution depends on.
- **internal/recorder/telemetry.go**: Implement TelemetryWriter appending records to telemetry.jsonl, with a golden round-trip test. Implement NO collectors: no Docker stats reader, no cgroup parser, no pprof scraper.
  - _why:_ Phase 0 has no lifecycle so there is nothing to sample; collectors are Phase 1 harness work. Writing the format and the writer now fixes the contract without implementing a later phase. JSONL rather than a single JSON document because the Saboteur must tail the file live during DRIVE.
- **cmd/thesis/init.go**: Scaffold prothesis.yaml with the top-level search: block COMMENTED OUT showing all A.10 defaults, plus a one-line comment cross-referencing profiles.soak.search. List all six built-in oracles including no_stuck_op (CRUCIBLE §B).
  - _why:_ Documents the two-level search naming at the point of maximum confusion without activating a Phase 4 feature. The oracle list correction comes from CRUCIBLE §B, where the base spec's sample YAML omits no_stuck_op that Phase 1 requires.
- **DECISIONS.md**: Add the eleven entries drafted in the analysis: two-level search naming; decode-in-place defaulting with the KnownFields non-propagation rationale; literal 1.41; resolved-config digest; WorldResult as internal Phase 1 type; no new verdict fields; canonical JSON with no maps/floats; hex seed; WorldHash excludes Meta; domain-keyed PRNG streams; telemetry JSONL with advisory interval and the task_count/goroutines tiering.
  - _why:_ Directive rule 3 requires recording choices where behavior is specified but implementation is open. Several of these (the KnownFields trap, the 1.41 literal, WorldHash excluding Meta) are decisions a later contributor would plausibly reverse as 'improvements' unless the rationale is written down.
- **OPEN_QUESTIONS.md**: Add the five entries drafted in the analysis: the 200ms/500ms telemetry conflict; A.6 scoring only 6 of 8 oracle classes; violation_weight/novelty_weight not covering A.6's hardcoded weights; resource_return_to_baseline requiring an unobservable goroutine count; .thesis having no schema discriminator in the normative text.
  - _why:_ Directive rule 4 forbids silently relaxing a requirement. Each of these is a genuine spec defect that Phase 0 works around at the format level without resolving, so the human decision must be queued rather than lost.

## Deferred

- ALL of internal/search/saboteur/: probe.go, observe.go, escalate.go, mcts.go, uct.go, ladder.go. No file in this package should exist after Phase 0.
- The Utility() function itself. Phase 0's obligation is only that the TYPES it reads exist and are nameable; the function is Phase 4 code and lives in internal/search/saboteur/.
- The WorldResult type. It has no Phase 0 consumer. Land it in Phase 1, where the lifecycle first produces a per-world outcome and the run-level aggregation seam becomes real. Phase 0 only owes it named exported Violation and Coverage types.
- Spiral classification: linearRegression, trend/acceleration computation, REINFORCE_ACCELERATING vs REINFORCE_LINEAR vs DAMPEN. Phase 4. (But write the 'regress against timestamps, not sample index' rule into DECISIONS.md now, before anyone implements it.)
- Micro-probe world generation, the probe sweep, and the ranked (fault_kind, target, observed_signal, classification) output of A.3.
- MCTS mechanics entirely: tree structure, UCT scoring, progressive widening, node pruning at 3 visits with zero delta, backpropagation, max-depth-4 enforcement, early termination on mid-DRIVE violation.
- The escalation ladder itself (rungs 0-5) and .prothesis/ladder.yaml parsing for escalation_ladder: custom. Phase 0 validates the enum value only; it does not read the file.
- Budget partitioning between Tier 1 and Tier 2 (the 20/80 split), and the fallback to stochastic corpus mutation when no REINFORCE signal appears.
- Telemetry COLLECTORS: Docker stats API reader, cgroup v2 parsers for memory.current / cpu.stat / pids.current, /proc/<pid>/fd counting, and the Tier 1 pprof/Prometheus scraper. Phase 1 harness work: Phase 0 defines the format and the writer only.
- Coverage computation: log-template regex normalization and hashing, state-abstraction tuple extraction, distinct counters, rare-event (<1%) tracking. Phase 4. Phase 0 defines the Coverage struct that holds the results.
- The nine Saboteur PRNG streams. Only the DERIVATION MECHANISM and the three Phase 0 stream names land now; registering saboteur.* names early would be implementing Phase 4's structure without its code.
- Corpus management: energy assignment, parent selection, culling of dominated worlds, rare-event bias.
- The `thesis search` command, and plumbing SearchConfig values into any actual search behaviour. Phase 0 parses, defaults and validates the config; nothing reads it.
- MCTS tree persistence across runs. The world file carries tree_id and tree_path so a tree can be reconstructed later, which is sufficient: do not design a tree serialization format now.
- Resolving the violation_weight / novelty_weight semantics ambiguity. Safe to defer: it is a Phase 4 runtime question, and Phase 0 only needs the fields to parse and default correctly.
- Choosing between 200ms and 500ms telemetry sampling. Safe to defer precisely because the format timestamps every record: the decision costs nothing later.
- Fixing A.6's missing differential/metamorphic cases. Safe to defer: it needs a human decision (A.6 is normative), and defining the eight Class constants in Phase 0 is enough to make the gap testable when Phase 4 arrives.
- Verdict signing, thesis bisect, the MCP server, thesis watch, and thesis doctor. Phases 5-6, unaffected by Addendum A.

## Analysis

## 0. What I verified, not assumed

The tree currently contains only `go.mod` (`module github.com/prothesis/prothesis`, go 1.27.1): Phase 0 has not been written yet, so every delta below is a greenfield commitment rather than a refactor. Git Bash is confirmed broken (`dofork: child -1 ... 0xC0000142`); Go is installed under the user's local programs directory and is not on the default PowerShell PATH.

I ran two throwaway Go probes to test the load-bearing claims rather than assert them:

- `…\scratchpad\yamlprobe\`: yaml.v3 defaulting, the `search` collision, `KnownFields` behaviour.
- `…\scratchpad\prngprobe\`: domain-keyed PRNG stream independence, canonical JSON world round-trip.

Results are quoted inline below. Two findings came out of the probes that I would otherwise have gotten wrong.

---

## 1. `pkg/schema`: CONFIG

### 1a. Is top-level `search:` a real collision with the profile-level `search: true`?

**No: it is not a collision, in either direction, and both fail loudly rather than silently.** This is the single most important result from the probe, because the obvious fear (one silently shadowing the other) turns out to be unfounded.

The two keys live under different parents:

- `Config.Search SearchConfig` ← top-level `search:` (Addendum A.10), a mapping.
- `Config.Profiles[name].Search bool` ← `profiles.soak.search: true` (base spec §4.2), a scalar.

Go resolves them as two unrelated struct fields on two unrelated types. YAML resolves them as two unrelated paths. Verified both confusion directions:

```
--- top-level scalar search:true (strict=true)
    ERR: line 7: cannot unmarshal !!bool `true` into main.Search
--- profile-level search mapping (strict=true)
    ERR: line 3: cannot unmarshal !!map into bool
```

Both mistakes produce a clean typed error at the right line. So the schema needs no disambiguation machinery: only a **semantic** decision and a loader that maps these errors to exit code 5.

**Semantic decision (DECISIONS.md):** the two keys are deliberately layered and mean different things.
- `profiles.<p>.search: bool`: *whether* the search engine is engaged for that profile (a gate). Default `false`. `thesis search` engages it regardless of profile.
- top-level `search:`: *how* the search engine behaves once engaged (strategy and parameters). Inert when nothing engages it.

Phase 0 must make this unambiguous in the `thesis init` scaffold by emitting the `search:` block **commented out with defaults shown**, next to a one-line comment pointing at `profiles.soak.search`. That documents the layering without activating a Phase 4 feature.

### 1b. Defaulting mechanism: decided, and it is *not* pointers and *not* a custom unmarshaler

A.10 requires all fields to have defaults while `fault_penalty: 0` and `reinforce_threshold: 0.0` remain meaningful explicit values. Three candidate mechanisms; the probe settles it:

**Chosen: `DefaultConfig()` + decode-in-place, with `KnownFields(true)`.** Seed the whole `Config` with defaults, then hand that same value to the decoder. yaml.v3 sets only the fields physically present in the document and does **not** zero the struct first, including recursively into nested structs. Verified:

```
--- absent search:  Utility:{ViolationWeight:100 NoveltyWeight:10 FaultPenalty:3 DurationPenalty:0.1}
--- partial + explicit zeros:
        ProbeBudgetPct:50            <- explicitly set
        MaxMCTSDepth:4               <- default survived a partial `search:` block
        Utility:{... FaultPenalty:0 ...}   <- explicit 0 beat the default 3
        Observe:{SampleIntervalMS:500 ...} <- untouched sub-block kept defaults
--- empty mapping `search: {}`:  defaults intact
--- explicit null `search:`:     defaults intact
```

So zero-vs-unset is resolved structurally, with **no pointer fields, no `*float64`, no `IsSet` bookkeeping, and no custom `UnmarshalYAML`.** Every field is a plain value type; the resolved config is always fully materialised, which also makes it canonically marshalable (needed for §1d).

**Rejected: pointer fields.** They would work, but they infect every read site with nil checks, they break the decode-in-place property (yaml.v3 allocates a *fresh zero* struct for a nil pointer field rather than merging into it, silently discarding nested defaults), and they make the config digest in §1d non-canonical.

**Rejected: custom `UnmarshalYAML` with the `type raw T` trick.** This is the trap. It appears to be the idiomatic defaulting pattern, but `KnownFields` is **not propagated** through a nested `node.Decode` call. Verified:

```
--- KnownFields through custom UnmarshalYAML: err=<nil> out={Inner:{A:1 B:0}}
```

The unknown key `zzz: 9` was silently swallowed with `err == nil`. If Phase 0 used a custom unmarshaler on `SearchConfig`, then `probe_budget_pctt: 50` would be silently ignored and the Saboteur would run with the default 20% while the operator believed they had set 50%. With plain decode-in-place, the same typo is caught:

```
--- typo inside search (strict=true)
    ERR: line 8: field probe_budget_pctt not found in type main.Search
```

That is a config-drift / anti-gaming concern (CRUCIBLE §E), not just ergonomics: a silently-ignored `probe_budget_pct` is a way to weaken the gate without tripping the lock.

### 1c. Exact Go definitions (Phase 0, `pkg/schema/config.go`)

```go
package schema

// SearchConfig is the top-level `search:` block (Addendum A.10 §A.10).
// Every field has a default; a zero value present in the document is an
// explicit, meaningful setting. Defaulting is by DefaultConfig() +
// decode-in-place -- never pointers, never a custom UnmarshalYAML.
type SearchConfig struct {
	Strategy            string        `yaml:"strategy"             json:"strategy"`
	ExplorationConstant float64       `yaml:"exploration_constant" json:"exploration_constant"`
	ProbeBudgetPct      int           `yaml:"probe_budget_pct"     json:"probe_budget_pct"`
	MaxMCTSDepth        int           `yaml:"max_mcts_depth"       json:"max_mcts_depth"`
	EscalationLadder    string        `yaml:"escalation_ladder"    json:"escalation_ladder"`
	Utility             UtilityConfig `yaml:"utility"              json:"utility"`
	Observe             ObserveConfig `yaml:"observe"              json:"observe"`
}

// UtilityConfig tunes the normative payoff function of Addendum A.6.
// All weights are float64 even where A.10 shows integer literals, so that
// `fault_penalty: 2.5` is expressible and `duration_penalty: 0.1` is natural.
type UtilityConfig struct {
	ViolationWeight float64 `yaml:"violation_weight"  json:"violation_weight"`
	NoveltyWeight   float64 `yaml:"novelty_weight"    json:"novelty_weight"`
	FaultPenalty    float64 `yaml:"fault_penalty"     json:"fault_penalty"`
	DurationPenalty float64 `yaml:"duration_penalty"  json:"duration_penalty"`
}

// ObserveConfig tunes the spiral detector of Addendum A.4.
type ObserveConfig struct {
	SampleIntervalMS   int     `yaml:"sample_interval_ms"  json:"sample_interval_ms"`
	SpiralWindow       int     `yaml:"spiral_window"       json:"spiral_window"`
	ReinforceThreshold float64 `yaml:"reinforce_threshold" json:"reinforce_threshold"`
}

// Profile is a base-spec §4.2 entry. NOTE: `search` here is a BOOLEAN and is
// unrelated to the top-level `search:` mapping above. It gates whether the
// search engine is engaged; the top-level block configures how it behaves.
type Profile struct {
	Budget        Duration `yaml:"budget"         json:"budget"`
	Worlds        int      `yaml:"worlds"         json:"worlds"` // -1 = unbounded
	DriverProfile string   `yaml:"driver_profile" json:"driver_profile"`
	Search        bool     `yaml:"search"         json:"search"`
}

func DefaultSearchConfig() SearchConfig {
	return SearchConfig{
		Strategy:            StrategySaboteur,
		ExplorationConstant: 1.41, // literal, NOT math.Sqrt2 -- see DECISIONS
		ProbeBudgetPct:      20,
		MaxMCTSDepth:        4,
		EscalationLadder:    LadderDefault,
		Utility: UtilityConfig{
			ViolationWeight: 100,
			NoveltyWeight:   10,
			FaultPenalty:    3,
			DurationPenalty: 0.1,
		},
		Observe: ObserveConfig{
			SampleIntervalMS:   500,
			SpiralWindow:       5,
			ReinforceThreshold: 0.0,
		},
	}
}

const (
	StrategySaboteur = "saboteur"
	StrategyRandom   = "random"
	StrategyHybrid   = "hybrid"

	LadderDefault = "default"
	LadderCustom  = "custom"
	LadderCustomPath = ".prothesis/ladder.yaml"
)

// Validate is Phase 0 work: it costs nothing and converts Phase 4 runtime
// panics into exit code 5 at load time.
func (s SearchConfig) Validate() error {
	switch s.Strategy {
	case StrategySaboteur, StrategyRandom, StrategyHybrid:
	default:
		return fmt.Errorf("search.strategy: want saboteur|random|hybrid, got %q", s.Strategy)
	}
	switch s.EscalationLadder {
	case LadderDefault, LadderCustom:
	default:
		return fmt.Errorf("search.escalation_ladder: want default|custom, got %q", s.EscalationLadder)
	}
	if s.ExplorationConstant < 0 {
		return fmt.Errorf("search.exploration_constant: must be >= 0, got %v", s.ExplorationConstant)
	}
	if s.ProbeBudgetPct < 0 || s.ProbeBudgetPct > 100 {
		return fmt.Errorf("search.probe_budget_pct: must be 0..100, got %d", s.ProbeBudgetPct)
	}
	if s.MaxMCTSDepth < 0 {
		return fmt.Errorf("search.max_mcts_depth: must be >= 0, got %d", s.MaxMCTSDepth)
	}
	// A.4 fits a linear regression over spiral_window samples; <2 is undefined.
	if s.Observe.SpiralWindow < 2 {
		return fmt.Errorf("search.observe.spiral_window: must be >= 2, got %d", s.Observe.SpiralWindow)
	}
	if s.Observe.SampleIntervalMS <= 0 {
		return fmt.Errorf("search.observe.sample_interval_ms: must be > 0, got %d", s.Observe.SampleIntervalMS)
	}
	return nil
}
```

Loader shape (`pkg/schema/load.go`):

```go
func LoadConfig(r io.Reader) (Config, error) {
	c := DefaultConfig() // pre-seeded; decode-in-place preserves unset fields
	d := yaml.NewDecoder(r)
	d.KnownFields(true) // typos are CONFIG_ERROR, never silent
	if err := d.Decode(&c); err != nil {
		return Config{}, &ConfigError{err} // caller maps to exit 5
	}
	return c, c.Validate()
}
```

`Duration` must be a Phase 0 type (`budget: 8h`, `health.timeout: 30s`) wrapping `time.Duration` with yaml marshal/unmarshal. Rule: **duration strings appear only in `prothesis.yaml`; the `.thesis` world file uses integer milliseconds exclusively** (see §3).

### 1d. Config digest: a Phase 0 commitment the Saboteur forces

CRUCIBLE §E2 makes profile budgets and the allowed fault space lock-covered. `search.strategy: random` plus `probe_budget_pct: 100` is exactly the same class of gate-weakening. Since the world file will carry a `config_digest` (§3) and `.prothesis/lock` will consume the same function, the digest algorithm is frozen the moment Phase 0 writes a world file.

**Decision:** `config_digest = sha256(json.Marshal(resolvedConfig))` over the **resolved, defaulted** config, not the raw file bytes. Raw bytes would flag whitespace churn and would *miss* a change in a compiled-in default. `encoding/json` sorts map keys deterministically, so `profiles` and `driver.profiles` are safe. `search:` is inside the struct, therefore inside the digest, therefore inside the lock, for free. This requires that every config field be JSON-tagged in Phase 0 (note the `json:` tags above) even though nothing reads config as JSON yet.

---

## 2. `pkg/schema`: VERDICT

### 2a. Field-by-field mapping of A.6's `Utility(result WorldResult)`

| A.6 reads | In frozen `prothesis.verdict/v1` §4.6? | Source of truth |
|---|---|---|
| `result.Violations[].Class` | **YES**: `violations[].class` | verdict |
| `result.Violations[].Severity` | **YES**: `violations[].severity` | verdict |
| `result.Coverage.NewTemplates` | **YES**: `coverage.new_templates` | verdict |
| `result.Coverage.NewStates` | **YES**: `coverage.new_states` | verdict |
| `result.FaultCount` | **NO** | world file: `len(world.Faults.Realized)` |
| `result.DurationSeconds` | **NO** | recorder: wall duration of *one world* execution |

Near-misses that must **not** be mistaken for the missing two:
- `violations[].shrink.faults_before` / `faults_after` exist, but they are per-violation and post-shrink (Phase 5). They are not the world's fault count.
- `budget.used_s` exists, but it is the **run's** consumed seconds across all worlds, not one world's duration.

### 2b. The structural finding: the verdict is per-RUN, `Utility` is per-WORLD

This is the real issue, and it is bigger than two missing fields. `prothesis.verdict/v1` has `budget.worlds_run: 22`, a single flat `violations` array, and a single `coverage` block. It is an **aggregate over the whole run**. A.6's `Utility` scores **one world** and is called once per MCTS rollout. You therefore cannot compute utility from a verdict at all, even if `FaultCount` and `DurationSeconds` were added to it.

Conclusion: **`WorldResult` is not the verdict and must never become a field of it.** It is the per-world record that the run aggregates *into* a verdict.

### 2c. `WorldResult`: internal type, `internal/control`, lands in Phase 1

```go
// internal/control/result.go  -- INTERNAL, not pkg/schema.
//
// WorldResult is the per-world outcome. The verdict (prothesis.verdict/v1) is
// the per-RUN aggregate of many of these. WorldResult has no `schema:`
// discriminator, no normative wire spelling, and no external consumer, so it
// does not belong in pkg/schema, which holds frozen contracts only.
type WorldResult struct {
	World      schema.World       // the realized world that was executed
	WorldHash  string             // sha256 of the world tuple (meta excluded)
	Violations []schema.Violation // this world's violations only
	Coverage   schema.Coverage    // deltas attributable to this world
	FaultCount int                // = len(World.Faults.Realized)
	DurationSeconds float64       // recorder wall duration, BOOT..TEARDOWN
	Status     WorldStatus        // ok | violated | inconclusive | budget_cut
}
```

**Why `internal/`, stated plainly:** putting it in `pkg/schema` would imply it is frozen and cross-repo portable (CRUCIBLE §L). It is neither. It is a search-engine ledger entry whose shape will move as Phase 4 learns what MCTS needs.

**It should land in Phase 1, not Phase 0.** Phase 1 is the first phase that actually executes a world and emits a verdict; that is where the aggregation seam becomes real. Defining it in Phase 0 would be implementing a later phase early with no consumer.

### 2d. What Phase 0 *must* do so Phase 1/4 can define it: the actual delta

`WorldResult` embeds `schema.Violation` and `schema.Coverage` **by value at sub-verdict granularity**. If Phase 0 writes the verdict with anonymous inline structs, as is very natural from reading §4.6's JSON:

```go
// WRONG -- freezes pkg/schema into a shape Phase 4 cannot reuse
type Verdict struct {
	Violations []struct{ ID, Oracle, Class string; ... } `json:"violations"`
	Coverage   struct{ NewTemplates int; ... }           `json:"coverage"`
}
```

…then Phase 4 cannot name the element type, cannot construct a `WorldResult`, and must break the frozen package. **Every nested object in the verdict must be a named exported type.** This costs nothing today and is the entire Phase 0 obligation arising from A.6.

```go
package schema

const VerdictSchema = "prothesis.verdict/v1"

type Verdict struct {
	Schema                 string                   `json:"schema"`
	RunID                  string                   `json:"run_id"`
	Profile                string                   `json:"profile"`
	Commit                 string                   `json:"commit"`
	Verdict                string                   `json:"verdict"`
	Budget                 VerdictBudget            `json:"budget"`
	Violations             []Violation              `json:"violations"`
	Coverage               Coverage                 `json:"coverage"`
	CoverageDeltaVsBaseline CoverageDelta           `json:"coverage_delta_vs_baseline"`
	OracleLock             OracleLock               `json:"oracle_lock"`
	Artifacts              string                   `json:"artifacts"`
}

type VerdictBudget struct {
	WallS         float64 `json:"wall_s"`
	UsedS         float64 `json:"used_s"`
	WorldsPlanned int     `json:"worlds_planned"`
	WorldsRun     int     `json:"worlds_run"`
}

// Violation is the unit A.6's Utility() iterates. Named, exported, and
// reusable at per-world granularity by internal/control.WorldResult.
type Violation struct {
	ID            string        `json:"id"`
	Oracle        string        `json:"oracle"`
	Class         Class         `json:"class"`
	Severity      Severity      `json:"severity"`
	Phase         string        `json:"phase"`
	FirstSeenMS   int64         `json:"first_seen_ms"`
	Explanation   string        `json:"explanation"`
	Witness       Witness       `json:"witness"`
	MinimalRepro  *MinimalRepro `json:"minimal_repro,omitempty"`
	Shrink        *Shrink       `json:"shrink,omitempty"`
	CausalTimeline []CausalEvent `json:"causal_timeline,omitempty"`
	Suspect       *Suspect      `json:"suspect,omitempty"`
}

// Coverage serves BOTH scopes with one shape: at run level New* means
// new-across-the-run; at world level New* means new-for-this-world.
// A.6 reads only NewTemplates and NewStates.
type Coverage struct {
	NewTemplates int `json:"new_templates"`
	CumTemplates int `json:"cum_templates"`
	NewStates    int `json:"new_states"`
	CumStates    int `json:"cum_states"`
}

type CoverageDelta struct {
	Templates int    `json:"templates"`
	Note      string `json:"note,omitempty"`
}

type Witness struct {
	OpIDs []int64 `json:"op_ids,omitempty"`
	Key   string  `json:"key,omitempty"`
}

type MinimalRepro struct {
	World      string `json:"world"`
	Cmd        string `json:"cmd"`
	Reproduced string `json:"reproduced"` // "3/3" -- Tier B honesty, CRUCIBLE §C
}

type Shrink struct {
	Attempted       bool     `json:"attempted"`
	FaultsBefore    int      `json:"faults_before"`
	FaultsAfter     int      `json:"faults_after"`
	OpsBefore       int      `json:"ops_before"`
	OpsAfter        int      `json:"ops_after"`
	SurvivingFaults []string `json:"surviving_faults,omitempty"`
}

type CausalEvent struct {
	TMS    int64  `json:"t_ms"`
	Event  string `json:"event"`
	Node   string `json:"node,omitempty"`
	Detail string `json:"detail"`
}

type Suspect struct {
	Files      []string `json:"files"`
	Confidence float64  `json:"confidence"`
	Basis      string   `json:"basis"`
}

type OracleLock struct {
	Status      string `json:"status"`
	ManifestSHA string `json:"manifest_sha"`
}
```

Plus the class/severity vocabulary, which Phase 0 should pin because A.6 switches on these exact strings and CRUCIBLE §B supplies the closed set:

```go
type Class string

const (
	ClassCrash       Class = "crash"
	ClassConsistency Class = "consistency"
	ClassLiveness    Class = "liveness"
	ClassConvergence Class = "convergence"
	ClassResource    Class = "resource"
	ClassSafety      Class = "safety"
	ClassDifferential Class = "differential"
	ClassMetamorphic Class = "metamorphic"
)

type Severity string

const (
	SeverityHigh   Severity = "high"
	SeverityMedium Severity = "medium"
	SeverityLow    Severity = "low"
)
```

**No new fields were added to `prothesis.verdict/v1`.** `FaultCount` and `DurationSeconds` are deliberately absent; they are supplied by the world file and the recorder respectively.

Two gaps this vocabulary exposes (report, do not fix; A.6 is normative and cannot be edited without a DECISIONS.md entry): A.6's switch has no case for `differential` or `metamorphic`, so two of the eight normative oracle classes contribute **zero** utility and the Saboteur is structurally blind to them. Defining the constants in Phase 0 makes that testable later rather than discovering it mid-Phase-4.

---

## 3. `pkg/schema`: WORLD FILE

Given the stipulated resolution (world stores the **realized** schedule, optionally the policy that generated it), the question is: what must exist on day one so Phase 4 does not break a format whose DoD is byte-identical round-trip?

### 3a. Format and canonicalisation decision

The base spec never says what encoding `.thesis` is. **Decision: canonical JSON**; `json.Encoder` with `SetEscapeHTML(false)` and `SetIndent("", "  ")`, trailing newline, struct field order. JSON is far safer than YAML for a byte-identical DoD (no quoting-style choices, no block-vs-flow ambiguity, no anchor/alias surface).

Three hard rules that must be adopted now, because retrofitting any of them is a format break:

1. **No Go maps anywhere in the world file.** Use ordered `[]Param{Key, Value string}` slices. This kills map-ordering risk permanently and, more importantly, kills float-formatting risk: fault parameters like `net.loss(5%)` and `net.latency(mean=50ms)` are stored as the *string* values the grammar parser produced.
2. **No bare floats.** All magnitudes are integers in canonical units (ms) or strings in `Param.Value`. Float round-tripping (`-0`, exponent form, shortest-repr changes across Go versions) is the classic way a byte-identical DoD dies quietly.
3. **All durations are integer milliseconds.** Duration *strings* live only in `prothesis.yaml`.

### 3b. The empty-slice hazard: verified, and it dictates the DoD test

The probe found a real ambiguity that would silently fail the Phase 0 DoD:

```
empty-slice hazard: input has planned:[] -> re-encoded omits it: true
  (so decode(bytes)->encode != bytes; strict decoder MUST reject explicit [] for omitempty fields)
```

An `omitempty` slice has two input spellings (absent, and `[]`) that decode to the same Go value and re-encode to only one. So a naive `decode(bytes) == encode(...)` round-trip test fails on legal-looking input. Fix: define the canonical form as `encode(struct)` and make the decoder **strict**, rejecting explicit `[]`/`{}` for optional fields and rejecting unknown fields (`json.Decoder.DisallowUnknownFields`). The DoD test then becomes well-posed:

```go
// for every golden world S, including one with every optional field absent
// and one with every optional field populated:
require(encode(decode(encode(S))) == encode(S))   // canonical fixed point
require(decode(goldenBytes))                       // strict decode accepts canon
require(decodeStrict(nonCanonicalBytes) != nil)    // and rejects non-canon
```

Both directions verified green in the probe:
```
round-trip (optionals absent)  byte-identical=true
round-trip (optionals present) byte-identical=true
```

### 3c. Seed representation

**Decision: seed is a 16-lowercase-hex string, not a JSON number.** A `uint64` seed above 2^53 loses precision in any JavaScript consumer, and CRUCIBLE §L requires world formats to be portable across repos and toolchains. The CLI still accepts decimal (`thesis run --seed 12345`) and canonicalises on write. Strict parsing rejects both non-canonical case and numeric form; verified:

```
non-canonical seed rejected: true (non-canonical seed "9C1E0F2A41F00D1E")
numeric seed rejected: true
```

### 3d. The minimal field set

```go
package schema

const WorldSchema = "prothesis.world/v1" // spec gives no discriminator -> DECISIONS.md

type World struct {
	Schema          string        `json:"schema"`
	Seed            Seed          `json:"seed"`             // I2: seed
	TopologyVariant string        `json:"topology_variant"` // I2: topology_variant
	Driver          WorldDriver   `json:"driver"`           // I2: driver_profile
	Faults          FaultSchedule `json:"faults"`           // I2: fault_schedule
	PhasePlan       []PhaseTiming `json:"phase_plan"`       // I2: phase_timings
	ConfigDigest    string        `json:"config_digest"`    // sha256 of resolved config
	Meta            WorldMeta     `json:"meta"`             // provenance; EXCLUDED from WorldHash
}

// WorldDriver pins resolved driver params into the world so it replays even
// after prothesis.yaml changes (CRUCIBLE §L portability).
type WorldDriver struct {
	Profile string  `json:"profile"`
	Params  []Param `json:"params,omitempty"` // ordered; never a map
}

type Param struct {
	Key   string `json:"key"`
	Value string `json:"value"` // always a string: no float round-trip risk
}

// FaultSchedule resolves the I2 / adaptive-schedule tension: Realized is
// authoritative and always present; Planned is recorded only when it differs;
// Policy records what generated an adaptive schedule.
type FaultSchedule struct {
	Adaptive bool    `json:"adaptive"`
	Realized []Fault `json:"realized"`
	Planned  []Fault `json:"planned,omitempty"`
	Policy   *Policy `json:"policy,omitempty"`
}

type Fault struct {
	Kind    string  `json:"kind"`             // "proc.pause"
	Target  string  `json:"target"`           // "role:leader", "minority(kv)", "n1<->n2"
	Params  []Param `json:"params,omitempty"` // ordered, string-valued
	StartMS int64   `json:"start_ms"`         // relative to DRIVE start, virtual clock
	EndMS   int64   `json:"end_ms"`
}

// Policy is the generator, not the output. Ordered params only.
type Policy struct {
	Name   string  `json:"name"` // "saboteur.mcts", "saboteur.probe", "random.mutate"
	Params []Param `json:"params,omitempty"`
}

// PhaseTiming is shared with prothesis.oracle_input/v1's `phases` array --
// one type, two consumers.
type PhaseTiming struct {
	Phase   string `json:"phase"`
	StartMS int64  `json:"start_ms"`
	EndMS   int64  `json:"end_ms"`
}

type WorldOrigin string

const (
	OriginSeeded  WorldOrigin = "seeded"
	OriginMutated WorldOrigin = "mutated"
	OriginProbe   WorldOrigin = "saboteur-probe"
	OriginMCTS    WorldOrigin = "saboteur-mcts"
	OriginShrunk  WorldOrigin = "shrunk"
	OriginManual  WorldOrigin = "manual"
)

// WorldMeta is provenance. It is EXCLUDED from WorldHash so that two worlds
// with identical tuples hash identically regardless of how they were found --
// this is what makes corpus dedup and parent-hash chains stable in Phase 4.
type WorldMeta struct {
	Origin     WorldOrigin `json:"origin"`
	Parent     string      `json:"parent,omitempty"`      // "sha256:..." of parent world
	TreeID     string      `json:"tree_id,omitempty"`     // MCTS tree identity
	TreePath   []int       `json:"tree_path,omitempty"`   // child indices root->node
	OracleLock string      `json:"oracle_lock,omitempty"` // manifest sha at discovery
	Note       string      `json:"note,omitempty"`
}
```

Justification for each forward-looking field, kept deliberately tight:

- **`Meta.Origin`**: the closed enum is defined in full now. Adding an enum *value* later is additive and cheap; adding the *field* later is a format break that invalidates every committed regression world. Define the whole set today.
- **`Meta.Parent`**: a `sha256:` string. Enables lineage, mutation-chain debugging, and Phase 5 shrink provenance. One string.
- **`Meta.TreeID` + `Meta.TreePath []int`**: the minimal encoding of "the MCTS tree path that produced it". `[]int` of child indices round-trips canonically and is trivially stable; a `[]string` of fault expressions would duplicate `Faults.Realized` and could drift out of sync with it. I deliberately did **not** add a `rung` field: the escalation ladder rung is derivable from `Fault.Kind` + params, so it is not minimal.
- **`Faults.Adaptive` + `Planned`**: makes "was this schedule planned or realized" explicit and self-describing rather than inferred. `Planned` is omitted when identical to `Realized`, which is the common case.
- **`Faults.Policy`**: the "optionally the policy that generated it" half of the stipulated I2 resolution, in a shape (name + ordered params) that cannot break canonical encoding.
- **`ConfigDigest`**: lets `thesis replay` detect config drift and ties into the I6 lock. One string, enormous leverage.
- **`Meta.OracleLock`**: records which oracle manifest the world was found under. In `Meta` (hash-excluded) so a lock bump does not change world identity.

**`WorldHash`; freeze the projection now:**

```go
// WorldHash is sha256 over the canonical encoding of the world TUPLE only.
// Meta is excluded by construction. This projection is frozen in Phase 0
// because Meta.Parent chains and the corpus dedup index depend on it.
func (w World) WorldHash() string {
	c := w
	c.Meta = WorldMeta{}
	return "sha256:" + hex(sha256.Sum256(canonicalJSON(c)))
}
```

---

## 4. `internal/recorder`: PRNG STREAMS

### 4a. The streams the Saboteur will need

Phase 4 will register, at minimum:

| Stream name | Used by |
|---|---|
| `saboteur.probe.order` | probe sweep ordering / shuffling across (kind, target) pairs |
| `saboteur.probe.params` | micro-probe magnitude and window sampling in A.3's 500–2000ms band |
| `saboteur.ladder.tiebreak` | A.7 ladder rung tie-breaks |
| `saboteur.uct.tiebreak` | A.5 UCT ties *after* the normative parsimony bias |
| `saboteur.mcts.expand` | action-space sampling under progressive widening |
| `saboteur.mcts.rollout` | rollout / default-policy scheduling |
| `saboteur.mcts.target` | choosing among equivalent nodes in a quorum target |
| `search.select` | corpus energy-weighted parent selection (base spec + A.5 fallback) |
| `search.mutate` | stochastic mutation on the `random`/`hybrid` path |

Phase 0 registers only what Phase 0 and Phase 1/2 need: `fault.schedule`, `driver.workload`, `harness.boot`.

### 4b. The required property, stated precisely

> **Stream independence.** For a fixed root seed R, the value sequence produced by the stream named N depends only on (R, N) and on the number of draws already taken from N. It must never depend on the existence, the creation order, or the draw counts of any other stream.

This is exactly what makes it safe to add seven Saboteur streams in Phase 4 without invalidating every `.thesis` regression world committed during Phases 0–3.

### 4c. How to guarantee it: domain-keyed derivation

```go
// internal/recorder/streams.go
const streamDomain = "prothesis/v1/prng"

// deriveKey is domain-separated and depends ONLY on (root, name).
func deriveKey(root schema.Seed, name string) [32]byte {
	h := sha256.New()
	h.Write([]byte(streamDomain))
	h.Write([]byte{0})
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(root))
	h.Write(b[:])
	h.Write([]byte{0})
	h.Write([]byte(name))
	var k [32]byte
	copy(k[:], h.Sum(nil))
	return k
}

// Stream wraps a pinned ChaCha8 core. Streams are created lazily but keyed
// deterministically, so creation order is irrelevant.
type Stream struct{ c *rand.ChaCha8 }

func newStream(root schema.Seed, name string) *Stream {
	k := deriveKey(root, name)
	return &Stream{c: rand.NewChaCha8(k)}
}

// Derive builds hierarchical sub-streams: per-world, per-node, per-tree-node.
// Sub-keys must themselves be deterministic (derive MCTS node ids from the
// tree path, never from a wall-clock-ordered counter).
func (s *Streams) Derive(name, sub string) *Stream {
	return s.Get(name + "/" + sub)
}
```

Verified empirically; creating and heavily drawing from the two Phase 4 streams left both Phase 0 streams bit-identical:

```
stream independence: fault.schedule stable=true driver.workload stable=true
golden fault.schedule = [16902176827544870997 8761768444073867199 13430911132033770671 6635156222228127103]
golden driver.workload = [18056820808863337364 7666657782584719251 6438714995589028089 8458372966780434647]
draw-count independence: true
distinct names differ: true
```

**Three anti-patterns Phase 0 must ban outright, because each silently destroys the property:**

1. **Sequential child seeding**: `child := rand.New(rand.NewSource(global.Int63()))`. Stream values then depend on creation *order*, so adding a Saboteur stream reshuffles every downstream stream. This is the failure mode the whole design exists to prevent.
2. **The `math/rand` (v1) or `math/rand/v2` package-level global.** Ban with a test that greps the tree for `math/rand` imports outside `internal/recorder`.
3. **Stdlib convenience helpers whose algorithm is not covered by the Go compatibility promise**: `rand.Rand.Shuffle`, `.Perm`, `.IntN` bias correction. A Go upgrade could silently change replay results for every committed regression world. Implement `IntN` (Lemire), `Shuffle` (Fisher-Yates), `Perm`, and `Float64` *inside* `internal/recorder` on top of `ChaCha8.Uint64()`. `rand.ChaCha8` itself is a specified, stable stream cipher and is a good pinned core.

### 4d. How to test it

Five tests, all Phase 0:

1. **Golden vector test (the load-bearing one).** For a fixed root seed, freeze the first 16 `uint64`s of every registered Phase 0 stream into a golden file. Phase 4 adds streams; **this file must not change**. It is the regression guard for the whole determinism story.
2. **Stream-addition test.** Construct streams in several different orders, interleaving unregistered extras, and assert identical sequences.
3. **Draw-count independence test.** Draw 10,000 values from stream A, assert stream B's first value is unmoved.
4. **Name registry test.** All names unique, matching `^[a-z0-9]+(\.[a-z0-9_]+)*(/[a-z0-9_=.-]+)*$`, sourced from one central `streams.go` const block; `Get` on an unregistered name panics in tests.
5. **No-global-rand test.** Source scan asserting no `math/rand` import outside `internal/recorder`, and no use of package-level rand functions.

---

## 5. `internal/recorder`: TELEMETRY

### 5a. The sampling-interval inconsistency: reported

Three different rates appear across the normative text:

- **A.3:** probe telemetry trajectory *"sampled every 200ms during DRIVE"*.
- **A.4:** container/process metrics *"Sampled every 500ms"*.
- **A.10:** `observe.sample_interval_ms: 500 # telemetry sampling interval`.

A.3 and A.4 are in direct conflict, and A.10 exposes only one knob for what A.3/A.4 describe as two rates. This is a genuine spec defect and goes in OPEN_QUESTIONS.md.

**The Phase 0 resolution is a format decision, not a rate decision, and that is the point:** do not encode the rate in the format's semantics. **Timestamp every sample explicitly**, and record the nominal interval in the header as advisory metadata only. Phase 4 can then adopt 200ms, 500ms, or a probe-mode override without any format change.

A correctness corollary that must be written down now: **A.4's `linearRegression` must regress against real elapsed timestamps, not against sample index.** Index-based regression silently rescales every trend slope when the rate changes, which would make `reinforce_threshold` mean different things at 200ms and 500ms, and `reinforce_threshold` defaults to `0.0`, exactly the boundary where a rescale flips the classification.

### 5b. Filename and the frozen contract

Base spec §4.5 shows `"telemetry_path": "/path/to/telemetry.json"`. **`telemetry_path` is the frozen field name; the path value is not frozen.** So emitting `telemetry.jsonl` is fully compliant. JSONL is required here: the file is appended in real time during DRIVE and must be tailable by the Saboteur while the world is still running; a single JSON document cannot be.

### 5c. The JSONL schema

One `type`-discriminated record per line. Timestamps use **`t_ns`**, matching the frozen history-log schema (§4.4), because oracles must correlate telemetry against history on a single time base. `t_ns` is the recorder's run clock in nanoseconds since run start: the same base as the history log. (Note the spec's own pre-existing unit split: history is `t_ns`, `oracle_input.phases` is `start_ms`/`end_ms`. Telemetry follows history.)

```go
package schema

const TelemetrySchema = "prothesis.telemetry/v1"

// Line 1 of telemetry.jsonl. `Metrics` declares which metrics this run was
// ABLE to collect -- see the unmodified-systems resolution below.
type TelemetryHeader struct {
	Schema      string   `json:"schema"`      // "prothesis.telemetry/v1"
	Type        string   `json:"type"`        // "header"
	RunID       string   `json:"run_id"`
	WorldHash   string   `json:"world_hash"`
	StartWallNS int64    `json:"start_wall_ns"`
	IntervalMS  int      `json:"interval_ms"` // ADVISORY ONLY; never assume uniformity
	Nodes       []string `json:"nodes"`
	Metrics     []string `json:"metrics"`     // e.g. ["rss_bytes","cpu_pct","fd_count","task_count"]
}

// Per-node resource sample. Every metric is a pointer: absent means "not
// collected", which is distinct from zero. A.4 source 1.
type TelemetrySample struct {
	Type      string   `json:"type"` // "sample"
	TNS       int64    `json:"t_ns"`
	Node      string   `json:"node"`
	Phase     string   `json:"phase,omitempty"`
	RSSBytes  *int64   `json:"rss_bytes,omitempty"`
	CPUPct    *float64 `json:"cpu_pct,omitempty"`
	FDCount   *int64   `json:"fd_count,omitempty"`
	TaskCount *int64   `json:"task_count,omitempty"` // OS tasks/threads (cgroup pids.current)
	Goroutines *int64  `json:"goroutines,omitempty"` // Tier 1 ONLY -- scraped, never assumed
}

// A.4 source 3: health probe results and latencies.
type TelemetryProbe struct {
	Type      string  `json:"type"` // "probe"
	TNS       int64   `json:"t_ns"`
	Node      string  `json:"node"`
	Probe     string  `json:"probe"`
	Status    int     `json:"status"`     // HTTP status, 0 = transport failure
	LatencyMS float64 `json:"latency_ms"`
	Error     string  `json:"error,omitempty"`
}

// A.4 source 4: driver op metrics. Run-scoped, not per-node.
type TelemetryDriver struct {
	Type        string  `json:"type"` // "driver"
	TNS         int64   `json:"t_ns"`
	Outstanding int64   `json:"outstanding"`
	P50MS       float64 `json:"p50_ms"`
	P99MS       float64 `json:"p99_ms"`
	ErrorRate   float64 `json:"error_rate"`
}

// A.4 source 2, DERIVED only. Raw logs stay verbatim in logs/<node>.log for
// no_panic_log; telemetry carries template counts so it stays small enough to
// sample at 200ms and cheap enough for the Saboteur to tail live.
type TelemetryLogTemplate struct {
	Type         string `json:"type"` // "log_template"
	TNS          int64  `json:"t_ns"`
	Node         string `json:"node"`
	TemplateHash string `json:"template_hash"`
	Count        int    `json:"count"`
	Severity     string `json:"severity,omitempty"`
}

// Phase transitions, so consumers can segment without a second file.
type TelemetryEvent struct {
	Type   string `json:"type"` // "phase" | "fault_inject" | "fault_withdraw"
	TNS    int64  `json:"t_ns"`
	Phase  string `json:"phase,omitempty"`
	Detail string `json:"detail,omitempty"`
}
```

### 5d. The goroutine-count vs unmodified-systems conflict: resolved

CRUCIBLE §L: *"Must work on unmodified systems today (rules out requiring instrumentation)."* Base spec Phase 1 oracle 4: *"RSS, FDs, goroutines return to within ±N% of baseline."* A.4: *"thread/goroutine count."* **A goroutine count is a Go-runtime-internal quantity and is not observable from outside the process. There is no way to satisfy both requirements as literally written.**

Resolution, in three tiers encoded in the format:

- **Tier 0: always available on a genuinely unmodified system.** From the Docker API and cgroup v2 inside the container: `rss_bytes` (`memory.current`), `cpu_pct` (`cpu.stat`), `task_count` (`pids.current`), `fd_count` (`/proc/<pid>/fd` via the harness). Note that `docker exec` and cgroup reads are *harness* capabilities, not modifications to the system under test, so this tier is honest. This matches the verified environment: Linux containers, where cgroup/`/proc` are real.
- **Tier 1: opt-in, still zero code change.** If the target *already* exposes `/debug/pprof` or a Prometheus endpoint, scrape `go_goroutines` into `Goroutines`. Configurable, never assumed.
- **Tier 2: requires cooperation.** Out of scope for v1; a `thesis doctor` recommendation (CRUCIBLE §C).

**Naming decision:** the format calls the Tier 0 metric `task_count` (OS tasks/threads) and keeps `goroutines` as a separate Tier 1 field. Conflating them would be a silent lie: `pids.current` is not a goroutine count, and reporting it as one would make `resource_return_to_baseline` assert something it is not measuring.

**Anti-gaming corollary (CRUCIBLE §E, "never make the gate weaker to make it pass"):** when a metric an oracle requires is absent from the header's `Metrics` list, that oracle must return **INCONCLUSIVE (exit 2)**, never PASS. Silently passing because a metric was unavailable is precisely the gate-weakening failure mode I6 exists to prevent. The header's `Metrics` declaration is what makes this checkable.

### 5e. Scope for Phase 0

Phase 0 has no lifecycle, so there is nothing to sample. Correct scope: **define the `pkg/schema` types above and a `recorder.TelemetryWriter` that appends records with a golden round-trip test. Implement no collectors.** Docker stats readers, cgroup parsers, and the pprof scraper are Phase 1 harness work.

---

## 6. Ready-to-paste `DECISIONS.md` entries

```markdown
### D-00x  `search:` exists at two levels, deliberately
Base spec §4.2 defines `profiles.<p>.search` as a BOOLEAN (engage the search
engine). Addendum A.10 defines a top-level `search:` MAPPING (how the engine
behaves). These are different parents and different types; verified that yaml.v3
rejects both confusion directions with a typed error at the correct line, so
there is no silent shadowing. Both are kept exactly as specified. `thesis init`
scaffolds the top-level block COMMENTED OUT with defaults shown, cross-
referencing `profiles.soak.search`.

### D-00x  Config defaulting is DefaultConfig() + decode-in-place
All `search:` fields have defaults while explicit zeros (`fault_penalty: 0`,
`reinforce_threshold: 0.0`) must remain meaningful. Mechanism: seed the whole
Config with defaults and decode into it. yaml.v3 sets only fields present in the
document, recursively, so absent fields keep defaults and explicit zeros win.
No pointer fields. NO custom UnmarshalYAML: verified that `KnownFields(true)`
does NOT propagate through a nested `node.Decode`, so a custom unmarshaler would
silently swallow typos such as `probe_budget_pctt`. Loader uses
`Decoder.KnownFields(true)`; all unmarshal failures map to exit code 5.

### D-00x  exploration_constant default is the literal 1.41, not math.Sqrt2
A.5 and A.10 both write `1.41` and gloss it as "(√2)". Using math.Sqrt2
(1.4142135...) would change UCT selection and make replays diverge from any
world recorded under the literal. The literal is normative.

### D-00x  config_digest is over the RESOLVED config, not raw file bytes
sha256(json.Marshal(resolvedConfig)). Raw bytes would flag whitespace churn and
would MISS a change in a compiled-in default. encoding/json sorts map keys, so
`profiles` and `driver.profiles` digest deterministically. This puts the whole
`search:` block inside the I6 lock for free (CRUCIBLE §E2: narrowing the search
is the same class of gate-weakening as narrowing the fault space).

### D-00x  WorldResult is an internal type, not a schema type; lands in Phase 1
A.6's Utility() scores ONE world; prothesis.verdict/v1 aggregates a whole RUN
(`budget.worlds_run`, one flat `violations` array, one `coverage` block). You
cannot compute per-world utility from a verdict. WorldResult therefore lives in
internal/control, has no `schema:` discriminator, and is NOT frozen. It lands in
Phase 1, the first phase that executes a world. Phase 0's only obligation is
that every nested verdict object is a NAMED EXPORTED type so WorldResult can
embed schema.Violation and schema.Coverage by value.

### D-00x  FaultCount and DurationSeconds are NOT added to prothesis.verdict/v1
The verdict is frozen. FaultCount comes from the world file
(len(World.Faults.Realized)); DurationSeconds comes from the recorder as the
wall duration of a single world. `violations[].shrink.faults_before` is
per-violation and post-shrink, and `budget.used_s` is run-scoped -- neither is a
substitute.

### D-00x  .thesis is canonical JSON with no maps and no bare floats
Encoder with SetEscapeHTML(false), SetIndent("", "  "), trailing newline, struct
field order. No Go maps (ordered []Param instead) and no bare floats (integer ms
or string Param.Value) -- both are byte-identical round-trip hazards. Durations
are integer milliseconds; duration strings appear only in prothesis.yaml.
Decoder is strict: DisallowUnknownFields, and explicit `[]`/`{}` for omitempty
fields is REJECTED, because absent and `[]` decode identically but re-encode to
only one spelling (verified).

### D-00x  seed is a 16-lowercase-hex string
A uint64 above 2^53 loses precision in any JavaScript consumer, and CRUCIBLE §L
requires world formats to be portable across repos. CLI accepts decimal and
canonicalises on write. Non-canonical case and numeric form are rejected.

### D-00x  WorldHash excludes Meta
sha256 over the world TUPLE only (seed, topology_variant, driver, faults,
phase_plan, config_digest). Provenance in Meta is excluded so two identical
tuples hash identically regardless of how each was discovered -- required for
stable corpus dedup and Meta.Parent chains in Phase 4.

### D-00x  PRNG streams are domain-keyed, never sequentially seeded
streamKey = sha256("prothesis/v1/prng" || 0 || be64(root) || 0 || name), seeding
a pinned rand/v2 ChaCha8. Property: for a fixed root seed, a stream's values
depend only on (root, name, draws taken) -- never on the existence, creation
order, or draw counts of any other stream. Verified. Sequential child seeding
and package-level rand are banned by test. Shuffle/Perm/IntN/Float64 are
implemented inside internal/recorder over ChaCha8.Uint64 rather than calling
stdlib helpers, whose algorithms are not covered by the Go compatibility promise
and could silently change replay results on a toolchain upgrade.

### D-00x  Telemetry is JSONL at telemetry.jsonl; every sample is timestamped
`telemetry_path` is the frozen field name; the path value is not frozen, so
.jsonl is compliant. JSONL is required because the Saboteur tails the file live
during DRIVE. Every record carries `t_ns` on the recorder run clock, matching the
frozen history-log schema so oracles can correlate the two. The header's
`interval_ms` is ADVISORY: the sampling rate is never inferred from record
position, so the 200ms/500ms question can be settled in Phase 4 with no format
change. A.4's trend/acceleration regression must use real elapsed timestamps,
not sample index.

### D-00x  task_count vs goroutines; absent metric => INCONCLUSIVE
Goroutine count is not observable from outside a Go process, so requiring it
conflicts with CRUCIBLE §L (unmodified systems). Tier 0 (always available via
cgroup v2 / Docker API): rss_bytes, cpu_pct, fd_count, task_count. Tier 1
(opt-in, still zero code change): scrape go_goroutines if the target ALREADY
exposes pprof/Prometheus. `task_count` is OS tasks and is deliberately NOT
reported as a goroutine count. The header declares which metrics the run could
collect; an oracle whose required metric is absent returns INCONCLUSIVE (exit 2),
never PASS -- passing on a missing metric is exactly the gate-weakening I6
forbids.
```

## 7. Ready-to-paste `OPEN_QUESTIONS.md` entries

```markdown
### Q-00x  Telemetry sampling interval: A.3 says 200ms, A.4 says 500ms
A.3 specifies a probe telemetry trajectory "sampled every 200ms during DRIVE";
A.4 specifies container/process metrics "Sampled every 500ms"; A.10 exposes a
single `observe.sample_interval_ms: 500`. Three rates, one knob. Phase 0
neutralises this by timestamping every sample and treating the header interval
as advisory, so no format change is needed either way. Phase 4 must decide
whether 200ms is a probe-mode override of the configured rate or whether A.4's
500ms is authoritative. If an override is wanted, A.10 needs a second field and
that is a schema addition requiring sign-off.

### Q-00x  A.6 scores only 6 of the 8 normative oracle classes
CRUCIBLE §B defines eight classes: crash, consistency, liveness, convergence,
resource, safety, differential, metamorphic. A.6's Utility() switch has no case
for `differential` or `metamorphic`, so violations in those classes contribute
ZERO utility and the Saboteur is structurally blind to them -- it will never
search toward a differential bug. A.6 is normative and must not be edited
without a DECISIONS.md entry. Needs a human decision before Phase 4.

### Q-00x  search.utility.violation_weight does not cover A.6's class weights
A.10 exposes one `violation_weight: 100`, but A.6 hardcodes 100/80/60/40/20 per
class. Is `violation_weight` (a) the consistency-class base with the others as
fixed ratios, (b) a global scale factor, or (c) applicable only to the
consistency case? Similarly, A.10's `novelty_weight: 10` covers templates only,
while A.6 hardcodes 5.0 per novel state with no knob. Phase 0 parses and defaults
these fields correctly; the semantics are a Phase 4 decision.

### Q-00x  Phase 1's resource_return_to_baseline requires an unobservable metric
Base spec Phase 1 oracle 4 requires "RSS, FDs, goroutines return to within ±N% of
baseline". Goroutine count cannot be obtained from an unmodified Go process
(CRUCIBLE §L). Resolved for the FORMAT (tiered metrics, INCONCLUSIVE on absence)
but the ORACLE's contract still names goroutines. Phase 1 must decide whether the
oracle degrades to task_count, or reports INCONCLUSIVE whenever the target does
not expose a runtime endpoint.

### Q-00x  .thesis has no schema discriminator in the normative text
Base spec §4.6 names prothesis.verdict/v1, oracle_input/v1, oracle_output/v1, but
never gives an identifier for the world file. Phase 0 adopts
`prothesis.world/v1`. Flagged because it is an invented schema name in a document
that forbids inventing field names; recorded in DECISIONS.md rather than assumed
silently.
```

## 8. Scope discipline

Nothing above implements Phase 4. The Saboteur package `internal/search/saboteur/` is not created, no MCTS/UCT/probe/observe/ladder code is written, and `Utility()` is not implemented. What lands is: config fields that parse and default correctly, named verdict types that can be reused at sub-verdict granularity, a world file with provenance slots, a PRNG derivation scheme with an independence guarantee, and a telemetry format. Every one of these is a *format* commitment that Phase 0 freezes anyway: the only question was whether it would be frozen in a shape Phase 4 can use.
