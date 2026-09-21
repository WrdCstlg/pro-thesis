# 01-schema.design

## Summary

pkg/schema is a stdlib-only Go package holding every frozen PRO-THESIS contract: prothesis.yaml config, the .thesis world file, history JSONL, prothesis.verdict/v1, the two oracle envelopes, exit codes, and the KIND(TARGET[,PARAMS])@WINDOW fault AST. Polymorphism is handled with three named techniques rather than improvised structs: a fault is a canonical string on the wire with an AST only in memory; history entries are one flat struct plus a Validate() that enforces the op/marker union (with `event` present as the discriminator, since `type:"info"` is overloaded as both an indeterminate op outcome and a phase marker); and int-or-string fields get small custom types (Duration, RetainPolicy) implemented against yaml's obsolete func-based Unmarshaler so pkg/schema imports nothing outside the standard library. The verdict is a total contract with no omitempty anywhere (empty lists are `[]`, empty witness is `{}`, and exactly three fields (minimal_repro, suspect, coverage_delta_vs_baseline) are ever null) and world_hash is SHA-256 over a sorted-key compact canonical JSON of an embedded WorldCore, so the hashed subset can never drift from the file.

## Decisions (25)

### pkg/schema depends only on the Go standard library

**Choice:** Implement YAML support via the func-based (obsolete) `UnmarshalYAML(unmarshal func(any) error) error` interface, which yaml.v3 still honours and which needs no import. `gopkg.in/yaml.v3` is imported exactly once in the tree, by internal/config, which uses `yaml.NewDecoder(...).KnownFields(true)`.

**Rationale:** The directive says minimize third-party dependencies, and pkg/ is public API: a zero-dependency verdict/oracle contract is consumable by third-party oracle authors and CI tooling without pulling a YAML parser.

**Rejected:** Node-based `UnmarshalYAML(*yaml.Node)` (forces yaml.v3 into pkg/schema and onto every consumer); hand-rolling a YAML parser (large, and YAML flow maps plus anchors are not worth reimplementing).

### A fault's wire form is a canonical string; the AST exists only in memory

**Choice:** `FaultSpec` implements MarshalJSON/UnmarshalJSON as the canonical string `KIND(TARGET[, PARAMS])@start..end`. `.thesis` fault_schedule and the verdict's surviving_faults are both arrays of these strings.

**Rationale:** The directive already fixes the string form: verdict `surviving_faults` is literally `["proc.pause(role:leader)@8200..15100", ...]`. One representation across the world file, the verdict, the CLI and log lines is portable across repos (CRUCIBLE PART 10), diffable, and hash-stable.

**Rejected:** A nested JSON object per fault (would contradict the verdict's own array-of-strings, and would need a discriminated union for the five target shapes); a Go interface for Target (needs custom polymorphic JSON for no gain, since the wire form is a string).

### Fault params: accept positional or named, always emit named and complete

**Choice:** The parser binds unnamed args positionally against the kind registry's declared param order and accepts `name=value` in any order. The canonical form emits every parameter, named, in declared order, INCLUDING defaults (so `proc.kill(n1)@100..200` normalizes to `proc.kill(n1, signal=SIGKILL)@100..200`). Param values are stored as EXACT source text and never reformatted.

**Rationale:** The directive declares params positionally (`net.latency(mean,jitter)`) but shows no concrete parameterized example, so both forms must be accepted. Materializing defaults makes a `.thesis` self-contained per invariant I2 and immune to a future change of a default silently changing an archived repro's meaning. Keeping raw text means no float ever round-trips through binary, so world hashes are stable.

**Rejected:** Emitting only explicitly-supplied params (leaves worlds hostage to default drift); parsing values into typed Go numbers (0.05 could re-render as 0.05000000000000000277, changing the hash).

### Window semantics for instantaneous fault kinds

**Choice:** `KindDecl.Durative` splits the 17 kinds. For durative kinds (net.*, proc.pause, proc.slow, clock.skew, io.*, mem.pressure, fd.exhaust) the fault is active over [start,end) and withdrawn at end. For instantaneous kinds (proc.kill, proc.restart, clock.jump) the fault fires AT start and [start,end] is the tolerated-disturbance window.

**Rationale:** The grammar gives every fault a window, but `proc.kill` has no duration. The built-in oracle `no_crash` is specified as 'no process exit outside planned fault windows', which only makes sense if a kill's window bounds the period during which the resulting exit is expected. This reading makes the grammar total with no new syntax.

**Rejected:** Requiring start==end for instantaneous kinds (leaves no_crash with no tolerance window and would flag every planned kill); a separate point-fault syntax (invents grammar).

### prothesis.verdict/v1 is a total contract: no omitempty anywhere

**Choice:** Every field defined by the verdict schema is present in every emitted verdict. Empty lists are `[]`, an empty witness is `{}`, absent scalars are `""`/`0`. Exactly three fields may be `null`: minimal_repro, suspect, coverage_delta_vs_baseline. `Verdict.Normalize()` is the single choke point and `WriteVerdict` always calls it.

**Rationale:** The verdict is consumed by an autonomous agent. `for v in verdict['violations']` must never crash on null, and a fabricated `reproduced: "0/0"` would violate the Tier B rule against claiming determinism we do not have, so the three genuinely-absent cases are explicit nulls rather than fake zeros.

**Rejected:** omitempty throughout (agents must probe for key existence on every field, and empty slices become null); making everything non-nullable (forces fabricated values that misreport 'shrink never ran' as 'shrink reproduced 0 of 0 times').

### `shrink` is always an object, never null

**Choice:** Violation.Shrink is a value, not a pointer, and carries `attempted: false` with zeroed counters and `surviving_faults: []` when shrinking never ran.

**Rationale:** The schema itself contains an `attempted` boolean, which exists precisely to express the not-attempted case; a null shrink would make that field unreachable.

**Rejected:** Pointer + null (contradicts the presence of `attempted`).

### History entries are one flat struct plus Validate(), not a sealed union

**Choice:** A single `HistoryEntry` covers both record shapes. `Validate()` enforces the union rules. Optional fields whose zero value is meaningful (`process`, `key`, `op_id`) are pointers; fields whose empty value is definitionally invalid (`f`, `error`, `event`, `phase`) are strings with omitempty; `value` is `json.RawMessage`.

**Rationale:** `type` does not discriminate the shapes (invoke/ok/fail share a shape and `info` spans both), so a sealed union would need a double parse of every line; unacceptable at 500k ops. Pointers make absent-vs-zero exact: process 0, op_id 0, key "" and value 0/false/null are all legitimate.

**Rejected:** Separate OpRecord/MarkerRecord types with a custom top-level unmarshaler (double parse per line); plain ints with omitempty (silently drops process 0 and op_id 0).

### The history record discriminator is `event`, not `type`

**Choice:** A record carrying a non-empty `event` is a MARKER record (and must have type "info"); a record without `event` is an OPERATION record. `RecordKind()` exposes this. `type:"info"` therefore legitimately means both 'indeterminate op outcome' and 'marker'.

**Rationale:** The directive defines info as the indeterminate op outcome ('crucial for sound consistency validation') AND uses it for the phase marker in the same example. Both uses are normative; only `event` separates them. Forbidding op fields on info records would destroy the indeterminate-outcome semantics that consistency checking depends on.

**Rejected:** Adding a new `type` value for markers (alters a frozen enum); treating every info record as a marker (makes sound consistency checking impossible).

### History `value` is json.RawMessage; unknown driver fields are preserved as raw lines

**Choice:** `Value json.RawMessage` with omitempty. `HistoryEntry` has no catch-all Extra map; `HistoryReader.Next()` returns both the decoded entry and the raw line bytes for callers that need byte fidelity.

**Rationale:** RawMessage distinguishes absent / null / 0 / false and never lossily round-trips a large integer or float. A per-line Extra map would cost an allocation on every one of up to 500,000 soak-profile lines to serve a need already met by keeping the raw line.

**Rejected:** `Value any` (float64 coercion destroys int64 values); an Extra map (hot-path allocation).

### Canonical JSON with HTML escaping disabled everywhere

**Choice:** Canonical form = object members sorted by key (ASCII), compact, scalar tokens copied verbatim. All emission routes through `marshalNoEscape` (`json.Encoder` with `SetEscapeHTML(false)`); the package doc forbids calling `json.Marshal` on these types directly, and a test asserts `<->` appears literally.

**Rationale:** Go's default encoder escapes `<` and `>`, so the edge target `n1<->n2` would serialize as `n1<->n2`; a different byte string, hence a different world_hash and an unreadable verdict. Copying scalar tokens verbatim means no number is ever reformatted, so floats in driver-profile mix values cannot drift the config hash.

**Rejected:** Full RFC 8785 with UTF-16 key ordering and number re-derivation (all schema keys are ASCII, and re-deriving numbers is exactly the drift risk being avoided).

### .thesis is indented JSON in struct order; the hash is over compact sorted-key canonical JSON of the embedded core

**Choice:** `World` embeds `WorldCore` anonymously so encoding/json inlines the five normative fields into a flat file. The file is `json.Indent(marshalNoEscape(world))` + newline, in struct declaration order (readable). `world_hash` is sha256 over `Canonicalize(marshalNoEscape(w.WorldCore))` (sorted keys, compact), independent of file layout.

**Rationale:** Embedding makes the hashable subset literally the same struct as the file's core fields, so the two can never drift as fields are added. Struct-order indentation is deterministic and human-diffable; sorted-key hashing is reproducible by a non-Go implementation.

**Rejected:** Nesting the tuple under a `core` key (harder to read, less portable, invents a structural key); hashing the whole file (cosmetic metadata such as created_at would change a world's identity); YAML for .thesis (too many representations for byte-identity).

### Nil slices are normalized to empty before hashing and emitting

**Choice:** `WorldCore.normalize()` converts nil FaultSchedule/PhaseTimings to empty slices and sorts the schedule by (start, end, canonical string); `Verdict.Normalize()` does the same for every verdict list.

**Rationale:** JSON `null` and `[]` canonicalize to different bytes and therefore different hashes, so a no-fault control world could otherwise hash two different ways depending on how it was constructed. Sorting the schedule makes hash identity independent of the order in which the search engine discovered the faults.

**Rejected:** Leaving nil slices (non-deterministic hashes for semantically identical worlds).

### world_hash mismatch on load is an error by default

**Choice:** `UnmarshalWorld` verifies the content address and returns `ErrWorldHashMismatch`, which the CLI maps to exit 5. Re-stamping requires an explicit `thesis world reseal`.

**Rationale:** Regression worlds under .prothesis/regressions/ are lock-covered and append-only (CRUCIBLE 6.3 mechanism 4). Silently accepting a hand-edited world would be a way to weaken a regression without tripping the lock.

**Rejected:** Warn and continue (an anti-gaming hole); rehash on load (destroys the content address).

### retain_passing and retain_failing share one polymorphic RetainPolicy type

**Choice:** `RetainPolicy{Unlimited bool; N int}` accepts either a non-negative integer or the string "all", on BOTH fields. A negative integer is a synonym for "all".

**Rationale:** The sample happens to use an int on one field and "all" on the other, but nothing makes that asymmetry normative and a user will write `retain_failing: 10`. One type is symmetric, and negative-means-unbounded matches `worlds: -1` so the config has one sentinel convention, not two.

**Rejected:** int for retain_passing and string for retain_failing (breaks the moment a user swaps the forms); `any` (pushes the type switch to every read site).

### profiles[].worlds is *int: -1 unbounded, nil inherit, explicit 0 rejected

**Choice:** `Worlds *int` with `WorldsUnbounded = -1`. nil (key absent) means inherit the CLI/default; -1 means run until the budget expires; an explicit 0 is a validation error.

**Rationale:** A pointer is the only way to distinguish 'key absent' from 'explicitly zero', and the distinction is load-bearing because profile budgets are covered by the oracle lock (narrowing one is drift); substituting a default for an explicit value there must never happen silently. `worlds: 0` is meaningless and is better reported than guessed.

**Rejected:** Plain int with 0 meaning unset (silently rewrites an explicit user value in a lock-covered field).

### Durations are Go duration strings with a mandatory unit

**Choice:** `type Duration time.Duration` parsed with `time.ParseDuration`. A bare number is rejected with a message naming the fix. Re-emission uses `time.Duration.String()`, which normalizes `90s` to `1m30s`.

**Rationale:** The tool never rewrites the user's prothesis.yaml (`thesis init` writes a literal template), so the normalization is only visible in effective-config dumps and lock hashing, where the normalized semantic value is exactly what should be hashed. Rejecting bare numbers avoids a seconds-vs-milliseconds guess that would silently change a budget.

**Rejected:** Storing the original text alongside the value (makes `==` semantically wrong and adds a field for cosmetics); accepting bare numbers as seconds (a silent unit guess in a lock-covered field).

### Config decoding is strict except inside driver.profiles

**Choice:** internal/config decodes with `KnownFields(true)`, so an unknown or misspelled key anywhere in prothesis.yaml is exit 5. The sole exception is `driver.profiles.*`, whose `DriverProfile.UnmarshalYAML` decodes to a map and captures unknown keys into `Extra`. Extra IS included in the canonical hash.

**Rationale:** A typo'd key that silently does nothing is a way to appear to have tightened a budget while changing nothing, so strictness is a fail-closed requirement. But driver profiles are a payload for a user-authored load generator (CRUCIBLE PART 10: must work on unmodified systems), so custom keys like key_space or value_size must survive. Hashing Extra means the escape hatch cannot smuggle changes past the lock.

**Rejected:** Strict everywhere (breaks real load generators); lenient everywhere (typos in budgets pass silently).

### Driver profile payload reaches the load generator via {profile_json} and an env var

**Choice:** ADDITIVE placeholders: `driver.cmd` may contain `{profile_json}`, and the runner always sets `PROTHESIS_DRIVER_PROFILE` to the canonical JSON of the resolved driver profile.

**Rationale:** The specified placeholders are only `{history_path}`, `{seed}` and `{profile}`; a profile NAME. Without this the load generator has no way to learn clients/ops/mix and would have to duplicate prothesis.yaml. This adds tokens without altering any specified field name.

**Rejected:** Requiring the load generator to parse prothesis.yaml (couples the fixture to the tool's config format); passing clients/ops/mix as fixed flags (assumes a load-generator CLI we do not control).

### Severity is derived from oracle class by a fixed table

**Choice:** `DefaultSeverity(class)`: consistency/safety -> critical; crash/liveness/convergence/differential/metamorphic -> high; resource -> medium. Applied in `OracleOutput.ToViolation` and backstopped in `Verdict.Normalize`.

**Rationale:** The verdict requires `severity` but prothesis.oracle_output/v1 has no severity field and prothesis.yaml has no per-oracle severity setting, so the engine must assign it. Deriving it from class needs no new config field and, importantly, means an oracle cannot grade down its own finding.

**Rejected:** Adding a severity field to oracle_output (alters a frozen schema and lets an oracle downgrade itself); a per-oracle severity key in prothesis.yaml (invents config surface and becomes a gaming vector).

### Oracle status reconciliation takes the worse of exit code and stdout

**Choice:** `OracleStatus.Worse` orders ok < inconclusive < violated. `DecodeOracleOutput` returns `max(statusFromExitCode, statusFromStdout)`, notes the disagreement in the explanation, and maps any parse failure, wrong schema id, or empty stdout to inconclusive, never to ok.

**Rationale:** The directive gives an oracle two independent channels for its result and does not say what happens when they disagree. Fail-closed (invariant I6) means never adopting the weaker reading; inconclusive maps to exit 2, which the agent-loop contract already handles as retry-once-then-escalate.

**Rejected:** Trusting the exit code (a crashing oracle that prints `violated` before dying would pass); trusting stdout (an oracle that prints ok but exits 1 would pass); erroring out (turns a disagreement into a run failure instead of a graded signal).

### Closed enums decode strictly; event and role_hint stay open strings

**Choice:** Phase, HistoryType, OracleStatus, OracleClass, VerdictResult, Backend and BuiltinOracle have strict UnmarshalJSON/UnmarshalYAML that reject unknown values. History `event`, causal_timeline `event` and `role_hint` are plain strings with exported constants.

**Rationale:** Soundness depends on the closed sets: an unrecognized history `type` silently treated as info would corrupt consistency checking. The open sets are narrative or advisory: closing them would foreclose new marker events and system-specific roles for no safety gain.

**Rejected:** Closing every string (blocks new event kinds and third-party role vocabularies); opening every string (unknown enum values become silent misreadings).

### Fault availability is modelled as required capabilities, not a backend allowlist

**Choice:** Each `KindDecl` declares `Requires []Capability` (net.filter, net.shape, signal.stop, proc.control, cpu.quota, mem.quota, clock.shift, io.inject, disk.fill, fd.limit). The backend reports which capabilities it provides on the current host; the schema declares only the requirement side.

**Rationale:** The limitation on this machine is host-OS-shaped, not backend-shaped: the process backend can do SIGSTOP and iptables on Linux but neither on Windows, while the compose backend can do both because the containers run inside Docker Desktop's Linux VM. A (backend, os) matrix in the schema would encode host facts into a frozen contract; capabilities let `thesis doctor` compute availability at runtime.

**Rejected:** `Backends []Backend` per kind (wrong on Linux, and bakes host assumptions into the schema); no capability model (`thesis doctor` cannot explain why proc.pause is unavailable).

### Seeds are limited to 2^53-1

**Choice:** `MaxSeed = 1<<53 - 1`; the seed stays a JSON number and `Seal` rejects larger values.

**Rationale:** Worlds are meant to be a shared, portable corpus (CRUCIBLE PART 10). A uint64 seed above 2^53 is silently mangled by any JSON parser backing numbers with float64 (JavaScript, jq), which would corrupt a repro without any error. 2^53 is far more entropy than a fault schedule needs.

**Rejected:** Full uint64 as a number (silent corruption in JS/jq); encoding the seed as a decimal string (fights `--seed N` and every other numeric use).

### Module path and world schema identifier

**Choice:** Module path `prothesis` (imports read `prothesis/pkg/schema`); the .thesis file carries `"schema": "prothesis.world/v1"`.

**Rationale:** The directory is not yet a git repo and there is no chosen VCS host, so a non-domain module path builds offline today and is a mechanical find/replace if the project is later published. The directive names schema IDs for the config, verdict and both oracle envelopes but never for the world file; `prothesis.world/v1` follows the established family so the file is self-identifying and version-gated on load.

**Rejected:** Guessing a github.com/... path (wrong path baked into every import); leaving .thesis unversioned (no way to reject a future format, and no way to tell a .thesis from any other JSON).

### causal_timeline[].node is always present

**Choice:** Emit `"node": ""` on timeline rows that are not node-scoped, rather than omitting the key.

**Rationale:** The directive's sample carries `node` only on the log row, but the verdict's presence rule is that every defined key is always present; a uniform shape is what a machine consumer needs, and a timeline is on the order of ten rows so the cost is nil.

**Rejected:** omitempty on this one field (one exception is enough to make consumers defensive about every field).

## Open questions (10)

### [RESOLVABLE-WITH-DECISION] History `t_ns` time base is undefined and the sample value contradicts the field name

Directive 4.4 names the field `t_ns` (nanoseconds) but its sample value 1725300000000000 decodes to 2024-09-02T18:00:00Z only when read as MICROSECONDS since the Unix epoch; read as nanoseconds it is 1970-01-20. Separately, directive 4.5 gives oracle `phases` as `start_ms`/`end_ms` with DRIVE starting at 0, and nothing defines the mapping between that origin and history `t_ns`. An oracle therefore cannot reliably decide which phase an operation falls in, which is exactly what invariant I5 (phase-aware assertion) requires.

**Recommendation:** Treat the field NAME as authoritative and the sample magnitude as a spec typo. Define `t_ns` as nanoseconds on the recorder virtual clock with t=0 fixed at DRIVE start, so BOOT and SEED records are negative and `phase_ms == t_ns/1_000_000` exactly. Recover wall-clock time without putting it on every line by emitting one marker record using only normative fields: `{"t_ns":0,"type":"info","event":"t_origin","value":<unix_nanos>}`. External drivers emit nanoseconds since their own start (they are launched at DRIVE start) and the recorder rebases when it interleaves markers. Needs a human ruling before Phase 1 wires the driver, because changing it later invalidates every archived history and regression.

### [RESOLVABLE-WITH-DECISION] perturber.constraints are natural-language sentences and cannot be enforced as written

The sample config specifies `constraints: ["never partition more than minority of kv", "pg must be reachable during SEED"]`. These are safety constraints on the fault search, not documentation, but no grammar is defined and no deterministic parser can enforce them. Phase 2's quorum resolver and Phase 4's mutator both need them to be machine-checkable, and a constraint the perturber silently ignores is a live safety hole: the search could partition a majority and manufacture a false violation.

**Recommendation:** Phase 0 stores them verbatim as `[]string` and hashes them into the lock, so no fidelity is lost. Phase 2 must introduce a small typed grammar over the existing target vocabulary (for example `max_partition(kv) <= minority` and `reachable(pg) during SEED`) and MUST reject an unparseable constraint with exit 5 (CONFIG_ERROR) rather than ignoring it. The sample config's two sentences get rewritten into that grammar. Requires a human decision on the grammar's surface syntax; do not let the search run with unenforced constraints in the meantime.

### [RESOLVABLE-WITH-DECISION] prothesis.verdict/v1 requires `severity` but no schema supplies it

Every violation must carry `severity` (the sample shows `"high"`), but prothesis.oracle_output/v1 has no severity field, prothesis.yaml has no per-oracle severity setting, and the value set is never enumerated anywhere in either document.

**Recommendation:** Adopt the four-value ladder low|medium|high|critical and derive severity in the engine from the oracle class via a fixed table (consistency/safety -> critical, crash/liveness/convergence/differential/metamorphic -> high, resource -> medium). Deliberately do NOT add a severity field to oracle_output: letting an oracle grade its own finding is an obvious gate-weakening vector under invariant I6. If a human wants per-oracle severity overrides, that is new config surface and must be a lock-covered change.

### [ACCEPT-AND-DOCUMENT] coverage_delta_vs_baseline cannot express a regression in state-tuple coverage

`coverage` tracks four counters (new_templates, cum_templates, new_states, cum_states) but `coverage_delta_vs_baseline` shows only `{templates, note}`. CRUCIBLE 6.3 mechanism 3 requires flagging a patch that reduces reachable log templates OR state tuples versus the baseline commit; with only `templates`, a patch that deletes a state-space code path is undetectable in the verdict.

**Recommendation:** Add a `states` integer alongside `templates` and `note`. This is purely additive: no specified field is renamed, removed or given new meaning, and consumers that read only `templates` are unaffected. Recorded in DECISIONS.md and flagged here so a human can veto the extension before Phase 4 depends on it.

### [RESOLVABLE-WITH-DECISION] The .thesis world file has no specified schema identifier

The directive names `prothesis/v1`, `prothesis.verdict/v1`, `prothesis.oracle_input/v1` and `prothesis.oracle_output/v1`, and it freezes the world TUPLE (seed, topology_variant, driver_profile, fault_schedule, phase_timings), but it never names a schema ID or a file format for `.thesis`. Without one there is no way to version-gate the format or to distinguish a .thesis from arbitrary JSON.

**Recommendation:** Use `"schema": "prothesis.world/v1"`, following the established family, and reject any other value on load. The tuple field names stay exactly as specified and are inlined flat into the file; the schema id, world_hash and an unhashed provenance block are the only additions.

### [RESOLVABLE-WITH-DECISION] A .thesis world is not self-contained across repositories

Invariant I2 requires a violation to serialize to a self-contained .thesis that reproduces the issue, and CRUCIBLE PART 10 requires world formats to be portable across repos so a shared corpus can accumulate. But `driver_profile` is a NAME resolved against the local prothesis.yaml, and the fault schedule targets node ids and service names defined by the local harness. Moving a .thesis to another repo, or editing prothesis.yaml in place, silently changes what it replays.

**Recommendation:** Keep the hashed core exactly the five specified fields, and add an unhashed `provenance` block carrying config_sha (sha256 of the defaulted config), tool version, determinism_tier, parent_world_hash and a note. `thesis replay` verifies config_sha and warns loudly on mismatch. Full self-containment would mean embedding the resolved driver profile and topology in the world, which changes what world_hash covers and would make every cosmetic config edit produce a new world identity: that is a human decision, not one to take silently.

### [ACCEPT-AND-DOCUMENT] The required fault matrix is not implementable on the process backend on this Windows host

Phase 2 requires every one of the 17 fault kinds to inject and cleanly withdraw. The host is Windows 11 with no Linux userland: SIGSTOP/SIGCONT, iptables and tc/netem do not exist outside Docker Desktop's Linux VM. A `process` backend on Windows therefore cannot implement proc.pause (the directive's stated top priority for gray failures) or any net.* kind at all, so `thesis up` on the process backend can never satisfy the Phase 2 definition of done here.

**Recommendation:** Model the gap explicitly rather than pretending it away: each fault kind declares `Requires []Capability`, the backend reports the capabilities it actually provides on the running host, and `thesis doctor` reports precisely which kinds are unavailable and why. Make `compose` the supported backend on Windows for all of Phases 1-5, and have the process backend refuse an unsupported kind with a clear error and exit 2 (INCONCLUSIVE, environment) rather than silently skipping the fault: a skipped fault would make a gate pass for the wrong reason.

### [RESOLVABLE-WITH-DECISION] oracle_lock.status enumerates only one value

The verdict sample shows `"oracle_lock": {"status": "ok", ...}` and exit code 4 is ORACLE_DRIFT, but the value set is never enumerated. The first-ever run, before `thesis oracles lock` has been invoked, has no third state to report and would have to lie with "ok" or "mismatch".

**Recommendation:** Define exactly three: `ok` (manifest matches .prothesis/lock), `mismatch` (drift -> exit 4, refuse to run), and `absent` (no lock file exists yet). `absent` must not be treated as `ok` by `thesis gate`: an agent that deletes .prothesis/lock would otherwise clear the drift check entirely.

### [ACCEPT-AND-DOCUMENT] The built-in oracle count is stated as seven but only six are named

The CRUCIBLE addendum says 'CRUCIBLE 3.4 requires SEVEN' built-in oracles, then lists six. It also asserts the on-disk directive omits `no_stuck_op` from `oracles.builtin`; verification of the file shows `no_stuck_op` appears twice, in both the sample config and the Phase 1 deliverables. Both documents agree on the same six names, and the seventh is not named anywhere in available text.

**Recommendation:** Implement and scaffold exactly the six named oracles (no_crash, no_panic_log, no_unbounded_queue, resource_return_to_baseline, availability_after_heal, no_stuck_op), which is what Phase 1's definition of done actually enumerates. If the unquoted CRUCIBLE 3.4 contains a seventh, a human must supply its name and semantics; the BuiltinOracle enum is a one-line addition plus its class and valid-phase entries.

### [ACCEPT-AND-DOCUMENT] Additive fields versus the 'do not improvise field names' rule

Core implementation rule 2 freezes all schemas and forbids inventing field names, but several contracts are incomplete as written: .thesis has no schema id or provenance, coverage_delta cannot express a state regression, driver.cmd has no way to convey a profile's parameters, and causal_timeline[].node appears inconsistently. Every fix adds a key.

**Recommendation:** Adopt a written policy: no specified field may be renamed, removed, retyped, or given new meaning; new keys may be ADDED only where a required capability is otherwise unrepresentable, must be marked `// ADDITIVE` at the declaration, and must each carry a DECISIONS.md entry. Every addition in this design is additive-only, so a consumer written strictly against the directive's samples keeps working. A human should confirm this policy once, up front, rather than adjudicating each field.

## Risks

- The `t_ns` time base is the highest-leverage unresolved item. Every history, every archived world and every regression is stamped with it, and changing the origin later invalidates the whole accumulated corpus. It must be ruled on before Phase 1 wires the driver, not after.
- Go's default JSON encoder HTML-escapes `<` and `>`, which silently turns the edge target n1<->n2 into n1<->n2. A single stray `json.Marshal` on a World or Verdict anywhere in the tree produces a different world_hash and an unreadable verdict, with no error. The mitigation is the marshalNoEscape choke point plus an explicit test, but it is a discipline requirement across every later phase, not a one-time fix.
- The canonical-JSON hash function is load-bearing for content addressing, the oracle lock and regression identity, yet it is easy to perturb accidentally: adding a field, changing the fault printer, or letting a nil slice through where an empty slice was expected all silently change every world_hash. The frozen golden hash test is the only thing that turns such a change into a loud failure.
- Strict config decoding (KnownFields) is a fail-closed requirement but will reject real-world prothesis.yaml files that carry harmless extra keys, and the exemption carved out for driver.profiles is judgement, not spec. If users hit friction, the pressure will be to relax strictness globally, which is exactly the gate-weakening the directive forbids.
- Go is not yet installed, so none of this code has been compiled. The riskiest constructs by inspection are the anonymous WorldCore embedding (relies on encoding/json inlining an untagged embedded struct), the func-based UnmarshalYAML path (relies on yaml.v3 still honouring the obsolete interface and permitting the closure to be called twice on one node), and the interaction between DisallowUnknownFields and the custom FaultSpec unmarshaller. Each needs a compile-and-test pass the moment Go lands.
- The 17-kind registry, the capability model and the severity table are all schema-level data that Phase 2 and Phase 3 will lean on heavily. If the fault-parameter shapes turn out wrong once real tc/netem and iptables injection is written, changing a param name or default changes every archived world's canonical string and therefore its hash, so the registry should be pressure-tested against a real injection prototype before any corpus is accumulated.
- The verdict presence rule assumes agents parse JSON and branch on values. If a consumer instead does schema validation against a strict generated JSON-schema derived from the directive's samples, the additive fields (coverage_delta.states, causal_timeline[].node, provenance) will fail validation. The additive-fields policy needs a human sign-off before Phase 6 exposes the verdict over MCP.

## Files implied (33)

- `C:\AI Projects\Pro-synthesis\pkg\schema\doc.go`: Package documentation, the schema-ID constants (prothesis/v1, prothesis.world/v1, prothesis.verdict/v1, prothesis.oracle_input/v1, prothesis.oracle_output/v1), and the standing rule never to call json.Marshal directly on these types. _(0)_
- `C:\AI Projects\Pro-synthesis\pkg\schema\errors.go`: ValidationError/ValidationErrors with dotted field paths, so a bad prothesis.yaml reports every problem at once and maps cleanly to exit code 5. _(0)_
- `C:\AI Projects\Pro-synthesis\pkg\schema\canonical.go`: Canonical JSON (sorted ASCII keys, compact, verbatim scalar tokens), the no-HTML-escape marshaller that keeps n1<->n2 intact, and HashCanonical for sha256 content addressing. _(0)_
- `C:\AI Projects\Pro-synthesis\pkg\schema\exit.go`: ExitCode 0-5 as a Go type, its total bijection with VerdictResult, and the agent-loop action table from the CRUCIBLE exit-code contract. _(0)_
- `C:\AI Projects\Pro-synthesis\pkg\schema\phase.go`: The eight lifecycle phases, PhaseWindow (ms relative to DRIVE start, may overlap and may be negative), and PhaseTimings with lookup and validation. _(0)_
- `C:\AI Projects\Pro-synthesis\pkg\schema\enums.go`: Every closed enum (oracle class x8, oracle status x3, severity x4, history type x4, verdict x6, lock status x3, backend x4, builtin oracle x6) with strict decoders, plus the open-string constant sets for event and role_hint. _(0)_
- `C:\AI Projects\Pro-synthesis\pkg\schema\duration.go`: Duration type for Go-style duration strings (90s, 10m, 8h) that rejects bare numbers rather than guessing a unit. _(0)_
- `C:\AI Projects\Pro-synthesis\pkg\schema\retain.go`: RetainPolicy, the int-or-"all" polymorphic type shared by retain_passing and retain_failing. _(0)_
- `C:\AI Projects\Pro-synthesis\pkg\schema\fault_kinds.go`: The frozen 17-kind fault registry: parameter declarations, durative vs instantaneous, edge-target legality, and the required-capability list that makes the Windows host limitation expressible. _(0)_
- `C:\AI Projects\Pro-synthesis\pkg\schema\fault.go`: FaultSpec plus the Target AST (node, wildcard selector, role, edge, quorum, all), the KIND(TARGET[,PARAMS])@WINDOW parser with depth-aware splitting, and the canonical string printer that is the wire form. _(0)_
- `C:\AI Projects\Pro-synthesis\pkg\schema\config.go`: The prothesis.yaml types with normative yaml/json tags, including the DriverProfile unknown-key escape hatch and the placeholder-token constants. _(0)_
- `C:\AI Projects\Pro-synthesis\pkg\schema\config_validate.go`: Config.ApplyDefaults, Config.Validate (mix sums to 1.0, allow/deny disjoint, profiles resolve, k8s/sim rejected as unimplemented), and the LockPayload projection Phase 3 will hash. _(0)_
- `C:\AI Projects\Pro-synthesis\pkg\schema\world.go`: WorldCore (the five-field normative tuple), World with WorldCore embedded so the hashed subset cannot drift from the file, unhashed provenance, world_hash computation, and byte-stable .thesis marshal/unmarshal. _(0)_
- `C:\AI Projects\Pro-synthesis\pkg\schema\history.go`: HistoryEntry with pointer fields for absent-vs-zero fidelity, the op-vs-marker union validator keyed on `event`, and streaming bufio reader/writer that also hands back raw lines. _(0)_
- `C:\AI Projects\Pro-synthesis\pkg\schema\verdict.go`: prothesis.verdict/v1 in full (budget, violations with minimal_repro/shrink/causal_timeline/suspect/witness, coverage, coverage_delta, oracle_lock, artifacts), Normalize enforcing the total-contract presence rule, and WriteVerdict as the single emission choke point. _(0)_
- `C:\AI Projects\Pro-synthesis\pkg\schema\oracle.go`: prothesis.oracle_input/v1 and prothesis.oracle_output/v1, the separate OracleExitCode space, the fail-closed status reconciliation, and ToViolation which assigns severity from class. _(0)_
- `C:\AI Projects\Pro-synthesis\pkg\schema\ids.go`: run_id (r_YYYY_MM_DD_a41f) and world filename (w_a41f.thesis) constructors and parsers. _(0)_
- `C:\AI Projects\Pro-synthesis\pkg\schema\ptr.go`: IntPtr/U64Ptr/StrPtr helpers required by the history schema's absent-vs-zero pointer fields. _(0)_
- `C:\AI Projects\Pro-synthesis\pkg\schema\testdata\prothesis.golden.yaml`: The directive's section 4.2 config copied verbatim; pins config field-name fidelity. _(0)_
- `C:\AI Projects\Pro-synthesis\pkg\schema\testdata\verdict.golden.json`: The directive's section 4.6 verdict copied verbatim; pins every nested verdict field name. _(0)_
- `C:\AI Projects\Pro-synthesis\pkg\schema\testdata\history.golden.jsonl`: The directive's section 4.4 five-line history; pins the op-record and marker-record shapes. _(0)_
- `C:\AI Projects\Pro-synthesis\pkg\schema\testdata\oracle_input.golden.json`: The directive's section 4.5 oracle stdin document copied verbatim. _(0)_
- `C:\AI Projects\Pro-synthesis\pkg\schema\testdata\oracle_output.golden.json`: The directive's section 4.5 oracle stdout document copied verbatim. _(0)_
- `C:\AI Projects\Pro-synthesis\pkg\schema\testdata\world.golden.thesis`: A frozen .thesis with a known world_hash; any change to the canonicalizer or fault printer must fail this test loudly. _(0)_
- `C:\AI Projects\Pro-synthesis\pkg\schema\config_test.go`: Decodes the golden config strictly and asserts the awkward cases: retain_failing==all, retain_passing==3, soak worlds==-1, soak search==true, budget 90s. _(0)_
- `C:\AI Projects\Pro-synthesis\pkg\schema\fault_test.go`: Table and property tests for parse/print round-tripping across all 17 kinds and all six target shapes, including net.partition(n1<->n2) and net.partition(minority(kv)). _(0)_
- `C:\AI Projects\Pro-synthesis\pkg\schema\world_test.go`: The Phase 0 definition-of-done test: byte-identical .thesis round-trip (including an empty fault schedule), hash stability, and hash independence from provenance. _(0)_
- `C:\AI Projects\Pro-synthesis\pkg\schema\verdict_test.go`: Asserts the presence rule on a zero verdict (violations is [], exactly three fields are null, no defined key missing) and that surviving_faults survive without HTML escaping. _(0)_
- `C:\AI Projects\Pro-synthesis\pkg\schema\history_test.go`: Proves the info-as-marker vs info-as-indeterminate-op split and that absent/null/0/false values and an empty key stay distinguishable. _(0)_
- `C:\AI Projects\Pro-synthesis\pkg\schema\oracle_test.go`: Proves fail-closed reconciliation: exit 1 with status ok becomes violated, and malformed stdout with exit 0 becomes inconclusive rather than ok. _(0)_
- `C:\AI Projects\Pro-synthesis\internal\config\load.go`: The only file in the tree importing gopkg.in/yaml.v3; loads prothesis.yaml with KnownFields(true), applies defaults, validates, and maps failures to exit code 5. _(0)_
- `C:\AI Projects\Pro-synthesis\DECISIONS.md`: Records the 25 schema decisions listed above, each with its rationale and rejected alternatives. _(0)_
- `C:\AI Projects\Pro-synthesis\OPEN_QUESTIONS.md`: Records the 10 spec conflicts, chiefly the t_ns time base, the natural-language perturber constraints, and the unimplementable fault matrix on the Windows process backend. _(0)_

## Design

# pkg/schema: design

## 0. Position and boundaries

`pkg/schema` is the single source of truth for every **frozen** contract. It contains types,
enums, parsers, canonical serializers, and `Validate()`. It contains **no I/O policy, no
subprocess management, no Docker, no search logic**. Phase 0 delivers it complete for all
seven contracts; later phases add *consumers*, not new schema fields.

Three rules govern the whole package:

1. **Extraction, not invention.** Every `json:`/`yaml:` tag below is copied from the spec text.
   Where I add a field, it is marked `// ADDITIVE` and appears in DECISIONS.md.
2. **`pkg/schema` imports only the Go standard library.** YAML support is provided via the
   *obsolete* `UnmarshalYAML(unmarshal func(any) error) error` interface, which `gopkg.in/yaml.v3`
   still honours and which requires no import. `yaml.v3` is imported exactly once in the tree,
   by `internal/config`. This keeps the public package dependency-free and makes the verdict
   contract consumable by third parties with zero transitive deps.
3. **Emission is total; ingestion is strict except where a third party authored the bytes.**

---

## 1. File-by-file map

```
pkg/schema/
  doc.go          package doc + schema-ID constants
  errors.go       ValidationError / ValidationErrors (drives exit 5 messages)
  canonical.go    canonical JSON, no-HTML-escape marshal, sha256 content addressing
  exit.go         ExitCode 0..5 + agent-loop action table
  phase.go        Phase (8) + PhaseWindow + PhaseTimings
  enums.go        OracleClass(8) OracleStatus(3) Severity(4) HistoryType(4)
                  VerdictResult(6) LockStatus(3) Backend(4) BuiltinOracle(6) + open-string consts
  duration.go     Duration  (Go-style "90s"/"10m"/"8h")
  retain.go       RetainPolicy (int | "all")
  fault_kinds.go  the 17-kind registry: params, arity, durativity, required capabilities
  fault.go        FaultSpec + Target AST + parser + canonical printer
  config.go       prothesis.yaml
  config_validate.go  Config.ApplyDefaults / Config.Validate
  world.go        World / WorldCore / Provenance, world_hash, .thesis read+write
  history.go      HistoryEntry + streaming reader/writer + union validation
  verdict.go      prothesis.verdict/v1 in full + Normalize + WriteVerdict
  oracle.go       prothesis.oracle_input/v1, prothesis.oracle_output/v1, OracleExitCode
  ids.go          run_id / world filename helpers
  ptr.go          IntPtr / U64Ptr / StrPtr
  testdata/       spec-verbatim golden fixtures
  *_test.go       golden + round-trip + property tests
```

---

## 2. doc.go

```go
// Package schema defines the frozen, normative wire contracts of PRO-THESIS.
//
// Field names in this package are NORMATIVE. They are transcribed from the
// PRO-THESIS build directive and must not be renamed, reordered in meaning, or
// improvised. Fields marked ADDITIVE are extensions; they add keys, never alter
// or remove specified ones.
//
// This package depends only on the Go standard library.
//
// EMISSION RULE: never call encoding/json.Marshal directly on the types in this
// package. Use MarshalWorld, WriteVerdict, MarshalOracleInput, or
// marshalNoEscape. Go's default encoder HTML-escapes '<' and '>', which would
// turn the edge target n1<->n2 into n1<->n2 and change world hashes.
package schema

const (
	// ConfigVersion is the required value of prothesis.yaml `version`.
	ConfigVersion = "prothesis/v1"

	// WorldSchema identifies a .thesis world file. ADDITIVE: the directive
	// fixes the world tuple but never names a schema ID for the file.
	WorldSchema = "prothesis.world/v1"

	VerdictSchema      = "prothesis.verdict/v1"
	OracleInputSchema  = "prothesis.oracle_input/v1"
	OracleOutputSchema = "prothesis.oracle_output/v1"
)

// ToolName is the binary name; `prothesis` is an accepted alias.
const ToolName = "thesis"
```

---

## 3. canonical.go: content addressing

Two jobs: (a) produce byte-stable bytes to hash, (b) never let Go's HTML escaping corrupt
fault strings.

```go
package schema

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
)

// marshalNoEscape marshals v as JSON with HTML escaping disabled.
// Every emitter in this package must route through it.
func marshalNoEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// Canonicalize rewrites valid JSON into PRO-THESIS canonical form:
//   - object members sorted by key, byte order (all schema keys are ASCII)
//   - no insignificant whitespace
//   - scalar tokens (numbers, strings, true/false/null) copied VERBATIM, so
//     numeric formatting is never re-derived and floats never drift
//
// This is RFC 8785 restricted to ASCII keys and to input this package produced.
func Canonicalize(raw []byte) ([]byte, error) {
	var out bytes.Buffer
	if err := canon(&out, raw); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func canon(dst *bytes.Buffer, raw json.RawMessage) error {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 {
		return errors.New("schema: empty JSON value")
	}
	switch t[0] {
	case '{':
		var m map[string]json.RawMessage
		if err := json.Unmarshal(t, &m); err != nil {
			return err
		}
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		dst.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				dst.WriteByte(',')
			}
			kb, err := marshalNoEscape(k)
			if err != nil {
				return err
			}
			dst.Write(kb)
			dst.WriteByte(':')
			if err := canon(dst, m[k]); err != nil {
				return err
			}
		}
		dst.WriteByte('}')
	case '[':
		var a []json.RawMessage
		if err := json.Unmarshal(t, &a); err != nil {
			return err
		}
		dst.WriteByte('[')
		for i := range a {
			if i > 0 {
				dst.WriteByte(',')
			}
			if err := canon(dst, a[i]); err != nil {
				return err
			}
		}
		dst.WriteByte(']')
	default:
		if !json.Valid(t) {
			return fmt.Errorf("schema: invalid JSON scalar %q", string(t))
		}
		dst.Write(t)
	}
	return nil
}

// CanonicalMarshal marshals then canonicalizes.
func CanonicalMarshal(v any) ([]byte, error) {
	b, err := marshalNoEscape(v)
	if err != nil {
		return nil, err
	}
	return Canonicalize(b)
}

// HashCanonical returns "sha256:"+hex over the canonical encoding of v.
// This is the content-address function for worlds, configs and the oracle lock.
func HashCanonical(v any) (string, error) {
	c, err := CanonicalMarshal(v)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(c)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// PrettyCanonical returns canonical JSON, deterministically indented, with a
// trailing newline. Used for on-disk artifacts that humans diff.
func PrettyCanonical(v any) ([]byte, error) {
	c, err := CanonicalMarshal(v)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	if err := json.Indent(&out, c, "", "  "); err != nil {
		return nil, err
	}
	out.WriteByte('\n')
	return out.Bytes(), nil
}
```

---

## 4. exit.go: exit codes 0–5

```go
package schema

import "fmt"

// ExitCode is the normative CLI exit code space (directive 4.7).
// It is a DIFFERENT space from OracleExitCode.
type ExitCode int

const (
	ExitPass            ExitCode = 0
	ExitFail            ExitCode = 1
	ExitInconclusive    ExitCode = 2
	ExitBudgetExhausted ExitCode = 3
	ExitOracleDrift     ExitCode = 4
	ExitConfigError     ExitCode = 5
)

func (c ExitCode) Valid() bool { return c >= ExitPass && c <= ExitConfigError }

func (c ExitCode) String() string { return string(c.Verdict()) }

// Verdict is the total bijection between exit code and verdict string.
func (c ExitCode) Verdict() VerdictResult {
	switch c {
	case ExitPass:
		return VerdictPass
	case ExitFail:
		return VerdictFail
	case ExitInconclusive:
		return VerdictInconclusive
	case ExitBudgetExhausted:
		return VerdictBudgetExhausted
	case ExitOracleDrift:
		return VerdictOracleDrift
	case ExitConfigError:
		return VerdictConfigError
	}
	return VerdictResult(fmt.Sprintf("UNKNOWN(%d)", int(c)))
}

// AgentAction is the agent-loop contract (CRUCIBLE PART 3.7). Surfaced by
// `thesis gate --json` and the MCP tools so an autonomous loop can branch
// without hardcoding the table.
type AgentAction string

const (
	ActionProceed      AgentAction = "proceed"
	ActionFixBug       AgentAction = "fix_bug_using_minimal_repro"
	ActionRetryOnce    AgentAction = "retry_once_then_escalate"
	ActionSoftWarn     AgentAction = "soft_warning_do_not_merge_blind"
	ActionEscalate     AgentAction = "stop_escalate_to_human_never_auto_resolve"
	ActionFixConfig    AgentAction = "fix_prothesis_yaml"
)

func (c ExitCode) AgentAction() AgentAction {
	switch c {
	case ExitPass:
		return ActionProceed
	case ExitFail:
		return ActionFixBug
	case ExitInconclusive:
		return ActionRetryOnce
	case ExitBudgetExhausted:
		return ActionSoftWarn
	case ExitOracleDrift:
		return ActionEscalate
	default:
		return ActionFixConfig
	}
}
```

---

## 5. phase.go: the 8 lifecycle phases

```go
package schema

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Phase is one of the eight virtual-clock lifecycle phases (directive 4.1).
// Phases ADVANCE linearly but their time WINDOWS may overlap: PERTURB is
// contained within DRIVE. Callers must not assume phase_timings partitions time.
type Phase string

const (
	PhaseBoot     Phase = "BOOT"
	PhaseSeed     Phase = "SEED"
	PhaseDrive    Phase = "DRIVE"
	PhasePerturb  Phase = "PERTURB"
	PhaseHeal     Phase = "HEAL"
	PhaseQuiesce  Phase = "QUIESCE"
	PhaseAssert   Phase = "ASSERT"
	PhaseTeardown Phase = "TEARDOWN"
)

// AllPhases is in lifecycle order; the index is the phase ordinal.
var AllPhases = [...]Phase{
	PhaseBoot, PhaseSeed, PhaseDrive, PhasePerturb,
	PhaseHeal, PhaseQuiesce, PhaseAssert, PhaseTeardown,
}

func (p Phase) Valid() bool {
	for _, q := range AllPhases {
		if p == q {
			return true
		}
	}
	return false
}

// Ordinal returns the lifecycle index, or -1.
func (p Phase) Ordinal() int {
	for i, q := range AllPhases {
		if p == q {
			return i
		}
	}
	return -1
}

// ParsePhase is case-insensitive on input and canonical-uppercase on output.
// Leniency is safe here: the set is closed, so an unknown value still errors.
func ParsePhase(s string) (Phase, error) {
	p := Phase(strings.ToUpper(strings.TrimSpace(s)))
	if !p.Valid() {
		return "", fmt.Errorf("schema: unknown phase %q (want one of %v)", s, AllPhases)
	}
	return p, nil
}

func (p *Phase) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("schema: phase must be a string: %w", err)
	}
	q, err := ParsePhase(s)
	if err != nil {
		return err
	}
	*p = q
	return nil
}

func (p *Phase) UnmarshalYAML(unmarshal func(any) error) error {
	var s string
	if err := unmarshal(&s); err != nil {
		return err
	}
	q, err := ParsePhase(s)
	if err != nil {
		return err
	}
	*p = q
	return nil
}

// PhaseWindow is one entry of `phases` (oracle_input) and `phase_timings`
// (.thesis). Times are MILLISECONDS on the recorder virtual clock with
// t=0 fixed at DRIVE start. BOOT and SEED therefore have NEGATIVE bounds.
type PhaseWindow struct {
	Phase   Phase `json:"phase"    yaml:"phase"`
	StartMS int64 `json:"start_ms" yaml:"start_ms"`
	EndMS   int64 `json:"end_ms"   yaml:"end_ms"`
}

func (w PhaseWindow) Contains(ms int64) bool { return ms >= w.StartMS && ms < w.EndMS }

func (w PhaseWindow) Validate() error {
	if !w.Phase.Valid() {
		return fmt.Errorf("phase_timings: unknown phase %q", w.Phase)
	}
	if w.EndMS < w.StartMS {
		return fmt.Errorf("phase_timings[%s]: end_ms %d < start_ms %d", w.Phase, w.EndMS, w.StartMS)
	}
	return nil
}

// PhaseTimings is the ordered list of windows for one run.
type PhaseTimings []PhaseWindow

func (t PhaseTimings) Lookup(p Phase) (PhaseWindow, bool) {
	for _, w := range t {
		if w.Phase == p {
			return w, true
		}
	}
	return PhaseWindow{}, false
}

// At returns every phase whose window covers ms (may be >1: PERTURB within DRIVE).
func (t PhaseTimings) At(ms int64) []Phase {
	var out []Phase
	for _, w := range t {
		if w.Contains(ms) {
			out = append(out, w.Phase)
		}
	}
	return out
}

// Validate checks each window and that DRIVE starts at 0 (the clock origin).
func (t PhaseTimings) Validate() error {
	seen := map[Phase]bool{}
	for _, w := range t {
		if err := w.Validate(); err != nil {
			return err
		}
		if seen[w.Phase] {
			return fmt.Errorf("phase_timings: duplicate phase %s", w.Phase)
		}
		seen[w.Phase] = true
	}
	if d, ok := t.Lookup(PhaseDrive); ok && d.StartMS != 0 {
		return fmt.Errorf("phase_timings: DRIVE.start_ms must be 0 (clock origin), got %d", d.StartMS)
	}
	return nil
}
```

---

## 6. enums.go: every closed enum, plus the open-string sets

```go
package schema

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ---------- verdict ----------

// VerdictResult is the `verdict` field of prothesis.verdict/v1. Six values,
// bijective with ExitCode.
type VerdictResult string

const (
	VerdictPass            VerdictResult = "PASS"
	VerdictFail            VerdictResult = "FAIL"
	VerdictInconclusive    VerdictResult = "INCONCLUSIVE"
	VerdictBudgetExhausted VerdictResult = "BUDGET_EXHAUSTED"
	VerdictOracleDrift     VerdictResult = "ORACLE_DRIFT"
	VerdictConfigError     VerdictResult = "CONFIG_ERROR"
)

var AllVerdictResults = [...]VerdictResult{
	VerdictPass, VerdictFail, VerdictInconclusive,
	VerdictBudgetExhausted, VerdictOracleDrift, VerdictConfigError,
}

func (v VerdictResult) Valid() bool { return v.ExitCode() >= 0 }

func (v VerdictResult) ExitCode() ExitCode {
	switch v {
	case VerdictPass:
		return ExitPass
	case VerdictFail:
		return ExitFail
	case VerdictInconclusive:
		return ExitInconclusive
	case VerdictBudgetExhausted:
		return ExitBudgetExhausted
	case VerdictOracleDrift:
		return ExitOracleDrift
	case VerdictConfigError:
		return ExitConfigError
	}
	return -1
}

// ---------- oracle class (8 values, CRUCIBLE 3.4) ----------

type OracleClass string

const (
	ClassCrash        OracleClass = "crash"
	ClassConsistency  OracleClass = "consistency"
	ClassLiveness     OracleClass = "liveness"
	ClassConvergence  OracleClass = "convergence"
	ClassResource     OracleClass = "resource"
	ClassSafety       OracleClass = "safety"
	ClassDifferential OracleClass = "differential"
	ClassMetamorphic  OracleClass = "metamorphic"
)

var AllOracleClasses = [...]OracleClass{
	ClassCrash, ClassConsistency, ClassLiveness, ClassConvergence,
	ClassResource, ClassSafety, ClassDifferential, ClassMetamorphic,
}

func (c OracleClass) Valid() bool {
	for _, x := range AllOracleClasses {
		if c == x {
			return true
		}
	}
	return false
}

// ---------- oracle status (3 values) ----------

type OracleStatus string

const (
	StatusOK           OracleStatus = "ok"
	StatusViolated     OracleStatus = "violated"
	StatusInconclusive OracleStatus = "inconclusive"
)

func (s OracleStatus) Valid() bool {
	return s == StatusOK || s == StatusViolated || s == StatusInconclusive
}

// severityRank orders statuses for fail-closed reconciliation:
// ok < inconclusive < violated.
func (s OracleStatus) rank() int {
	switch s {
	case StatusViolated:
		return 2
	case StatusInconclusive:
		return 1
	default:
		return 0
	}
}

// Worse returns the more severe of two statuses. Used to reconcile an external
// oracle's exit code with its stdout status: we never take the weaker reading.
func (s OracleStatus) Worse(o OracleStatus) OracleStatus {
	if o.rank() > s.rank() {
		return o
	}
	return s
}

// ---------- severity ----------

// Severity is the verdict-level severity of a violation. The directive shows
// only "high"; the ladder below is inferred (see DECISIONS.md).
// prothesis.oracle_output/v1 has NO severity field: severity is assigned by the
// engine from the oracle class, never reported by the oracle.
type Severity string

const (
	SeverityLow      Severity = "low"
	SeverityMedium   Severity = "medium"
	SeverityHigh     Severity = "high"
	SeverityCritical Severity = "critical"
)

var AllSeverities = [...]Severity{SeverityLow, SeverityMedium, SeverityHigh, SeverityCritical}

func (s Severity) Valid() bool {
	for _, x := range AllSeverities {
		if s == x {
			return true
		}
	}
	return false
}

// DefaultSeverity is the fixed class->severity table.
func DefaultSeverity(c OracleClass) Severity {
	switch c {
	case ClassConsistency, ClassSafety:
		return SeverityCritical
	case ClassCrash, ClassLiveness, ClassConvergence, ClassDifferential, ClassMetamorphic:
		return SeverityHigh
	case ClassResource:
		return SeverityMedium
	default:
		return SeverityMedium
	}
}

// ---------- history record type (4 values) ----------

type HistoryType string

const (
	HistoryInvoke HistoryType = "invoke" // operation issued
	HistoryOK     HistoryType = "ok"     // definitely succeeded
	HistoryFail   HistoryType = "fail"   // definitely did NOT happen
	HistoryInfo   HistoryType = "info"   // indeterminate outcome, OR a marker record
)

func (t HistoryType) Valid() bool {
	switch t {
	case HistoryInvoke, HistoryOK, HistoryFail, HistoryInfo:
		return true
	}
	return false
}

// ---------- oracle lock status ----------

// LockStatus is the `oracle_lock.status` value. The directive shows only "ok";
// the rest are inferred (see OPEN_QUESTIONS.md).
type LockStatus string

const (
	LockOK       LockStatus = "ok"       // manifest matches .prothesis/lock
	LockMismatch LockStatus = "mismatch" // drift -> exit 4, refuse to run
	LockAbsent   LockStatus = "absent"   // no lock file has been created yet
)

func (s LockStatus) Valid() bool {
	return s == LockOK || s == LockMismatch || s == LockAbsent
}

// ---------- harness backend ----------

type Backend string

const (
	BackendCompose Backend = "compose"
	BackendProcess Backend = "process"
	BackendK8s     Backend = "k8s" // parseable, not implemented in v1
	BackendSim     Backend = "sim" // parseable, not implemented in v1 (Tier A)
)

var AllBackends = [...]Backend{BackendCompose, BackendProcess, BackendK8s, BackendSim}

func (b Backend) Valid() bool {
	for _, x := range AllBackends {
		if b == x {
			return true
		}
	}
	return false
}

// ImplementedV1 reports whether this backend is buildable in v1. k8s and sim
// parse successfully but Validate rejects them with exit 5, so the enum stays
// stable for forward compatibility.
func (b Backend) ImplementedV1() bool {
	return b == BackendCompose || b == BackendProcess
}

// ---------- built-in oracles (6) ----------

type BuiltinOracle string

const (
	BuiltinNoCrash                  BuiltinOracle = "no_crash"
	BuiltinNoPanicLog               BuiltinOracle = "no_panic_log"
	BuiltinNoUnboundedQueue         BuiltinOracle = "no_unbounded_queue"
	BuiltinResourceReturnToBaseline BuiltinOracle = "resource_return_to_baseline"
	BuiltinAvailabilityAfterHeal    BuiltinOracle = "availability_after_heal"
	BuiltinNoStuckOp                BuiltinOracle = "no_stuck_op"
)

var AllBuiltinOracles = [...]BuiltinOracle{
	BuiltinNoCrash, BuiltinNoPanicLog, BuiltinNoUnboundedQueue,
	BuiltinResourceReturnToBaseline, BuiltinAvailabilityAfterHeal, BuiltinNoStuckOp,
}

// BuiltinOracleClass is the fixed class of each built-in.
func (b BuiltinOracle) Class() OracleClass {
	switch b {
	case BuiltinNoCrash, BuiltinNoPanicLog:
		return ClassCrash
	case BuiltinNoUnboundedQueue, BuiltinResourceReturnToBaseline:
		return ClassResource
	case BuiltinAvailabilityAfterHeal:
		return ClassLiveness
	case BuiltinNoStuckOp:
		return ClassLiveness
	}
	return ClassSafety
}

// ValidPhases is the I5 phase-validity declaration for each built-in.
func (b BuiltinOracle) ValidPhases() []Phase {
	switch b {
	case BuiltinNoUnboundedQueue, BuiltinResourceReturnToBaseline, BuiltinAvailabilityAfterHeal:
		return []Phase{PhaseQuiesce, PhaseAssert}
	case BuiltinNoStuckOp:
		return []Phase{PhaseHeal, PhaseQuiesce, PhaseAssert}
	default: // invariant oracles: valid throughout
		return []Phase{PhaseBoot, PhaseSeed, PhaseDrive, PhasePerturb,
			PhaseHeal, PhaseQuiesce, PhaseAssert, PhaseTeardown}
	}
}

func (b BuiltinOracle) Valid() bool {
	for _, x := range AllBuiltinOracles {
		if b == x {
			return true
		}
	}
	return false
}

// ---------- OPEN string sets (constants, not closed enums) ----------

// Role hints are advisory and system-specific; NOT a closed enum.
const (
	RoleLeader   = "leader"
	RoleFollower = "follower"
	RoleReplica  = "replica"
	RoleStorage  = "storage"
	RoleProxy    = "proxy"
	RoleClient   = "client"
)

// History `event` values for marker records. OPEN: drivers may emit others.
const (
	EventPhase   = "phase"    // {"type":"info","event":"phase","phase":"HEAL"}
	EventFault   = "fault"    // fault injected / withdrawn
	EventTOrigin = "t_origin" // ADDITIVE: carries the wall-clock origin of t_ns
	EventNote    = "note"
)

// causal_timeline `event` values. OPEN: presentational narrative.
const (
	TimelineFault  = "fault"
	TimelineLog    = "log"
	TimelineOp     = "op"
	TimelinePhase  = "phase"
	TimelineOracle = "oracle"
)

// strictEnum is the shared strict JSON decoder helper for closed string enums.
func strictEnum(b []byte, kind string, ok func(string) bool) (string, error) {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return "", fmt.Errorf("schema: %s must be a string: %w", kind, err)
	}
	if !ok(s) {
		return "", fmt.Errorf("schema: unknown %s %q", kind, strings.TrimSpace(s))
	}
	return s, nil
}

func (c *OracleClass) UnmarshalJSON(b []byte) error {
	s, err := strictEnum(b, "oracle class", func(v string) bool { return OracleClass(v).Valid() })
	if err != nil {
		return err
	}
	*c = OracleClass(s)
	return nil
}

func (s *OracleStatus) UnmarshalJSON(b []byte) error {
	v, err := strictEnum(b, "oracle status", func(v string) bool { return OracleStatus(v).Valid() })
	if err != nil {
		return err
	}
	*s = OracleStatus(v)
	return nil
}

func (t *HistoryType) UnmarshalJSON(b []byte) error {
	v, err := strictEnum(b, "history type", func(v string) bool { return HistoryType(v).Valid() })
	if err != nil {
		return err
	}
	*t = HistoryType(v)
	return nil
}

func (v *VerdictResult) UnmarshalJSON(b []byte) error {
	s, err := strictEnum(b, "verdict", func(v string) bool { return VerdictResult(v).Valid() })
	if err != nil {
		return err
	}
	*v = VerdictResult(s)
	return nil
}

func (b *Backend) UnmarshalYAML(unmarshal func(any) error) error {
	var s string
	if err := unmarshal(&s); err != nil {
		return err
	}
	if !Backend(s).Valid() {
		return fmt.Errorf("harness.backend: unknown backend %q (want %v)", s, AllBackends)
	}
	*b = Backend(s)
	return nil
}

func (o *BuiltinOracle) UnmarshalYAML(unmarshal func(any) error) error {
	var s string
	if err := unmarshal(&s); err != nil {
		return err
	}
	if !BuiltinOracle(s).Valid() {
		return fmt.Errorf("oracles.builtin: unknown built-in oracle %q (want %v)", s, AllBuiltinOracles)
	}
	*o = BuiltinOracle(s)
	return nil
}
```

---

## 7. duration.go: Go-style duration strings

**Problem:** `budget: 90s`, `timeout: 30s`, `budget: 8h`. YAML has no duration type.

**Resolution:** `type Duration time.Duration`. **A unit is mandatory**; a bare number is a
CONFIG_ERROR with a message that names the fix. Re-emission uses `time.Duration.String()`,
which normalizes `90s` to `1m30s`: acceptable because the tool never rewrites the user's
`prothesis.yaml` (`thesis init` writes a literal template), and lock hashing wants the
normalized semantic value anyway.

```go
package schema

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Duration is a Go-style duration string: "90s", "10m", "8h", "250ms".
// A unit is REQUIRED. Bare numbers are rejected rather than guessed at.
type Duration time.Duration

func (d Duration) Duration() time.Duration { return time.Duration(d) }
func (d Duration) String() string          { return time.Duration(d).String() }
func (d Duration) Seconds() float64        { return time.Duration(d).Seconds() }

func parseDurationText(s string) (Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf(`duration must not be empty (e.g. "90s", "10m", "8h")`)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf(`invalid duration %q: want a Go duration with a unit, e.g. "90s", "10m", "8h"`, s)
	}
	if v < 0 {
		return 0, fmt.Errorf("duration %q must not be negative", s)
	}
	return Duration(v), nil
}

// UnmarshalYAML uses the func-based (obsolete) yaml.Unmarshaler interface so
// pkg/schema needs no yaml import. yaml.v3 honours it, and the closure may be
// called more than once on the same node.
func (d *Duration) UnmarshalYAML(unmarshal func(any) error) error {
	var s string
	if err := unmarshal(&s); err != nil {
		var n float64
		if unmarshal(&n) == nil {
			return fmt.Errorf(`duration must be a string with a unit (got bare number %v); write "%vs" if you meant seconds`, n, n)
		}
		return fmt.Errorf(`duration must be a string with a unit, e.g. "90s", "10m", "8h"`)
	}
	v, err := parseDurationText(s)
	if err != nil {
		return err
	}
	*d = v
	return nil
}

func (d Duration) MarshalYAML() (any, error) { return d.String(), nil }

func (d Duration) MarshalJSON() ([]byte, error) { return marshalNoEscape(d.String()) }

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf(`duration must be a JSON string with a unit, e.g. "90s"`)
	}
	v, err := parseDurationText(s)
	if err != nil {
		return err
	}
	*d = v
	return nil
}
```

---

## 8. retain.go: `retain_passing: 3` vs `retain_failing: all`

**Problem:** one field is an int, its sibling is the string `all`. Two Go types would be
asymmetric and would break if a user writes `retain_failing: 10`.

**Resolution:** one polymorphic `RetainPolicy` used by *both* fields. `all` and any negative
integer mean unlimited (consistent with `worlds: -1`).

```go
package schema

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// RetainAllToken is the literal spelling of an unlimited retention policy.
const RetainAllToken = "all"

// RetainPolicy is `retain_passing` / `retain_failing`. It accepts either a
// non-negative integer count or the string "all". A negative integer is
// accepted as a synonym for "all", matching `worlds: -1`.
type RetainPolicy struct {
	Unlimited bool
	N         int
}

func RetainAll() RetainPolicy       { return RetainPolicy{Unlimited: true} }
func RetainN(n int) RetainPolicy    { return RetainPolicy{N: n} }
func (r RetainPolicy) IsAll() bool  { return r.Unlimited }

func (r RetainPolicy) String() string {
	if r.Unlimited {
		return RetainAllToken
	}
	return strconv.Itoa(r.N)
}

// Keep reports whether the i-th newest run (0-based) is retained.
func (r RetainPolicy) Keep(i int) bool { return r.Unlimited || i < r.N }

func (r *RetainPolicy) set(s string) error {
	if strings.EqualFold(strings.TrimSpace(s), RetainAllToken) {
		*r = RetainAll()
		return nil
	}
	return fmt.Errorf(`retention must be a non-negative integer or %q, got %q`, RetainAllToken, s)
}

func (r *RetainPolicy) UnmarshalYAML(unmarshal func(any) error) error {
	var s string
	if err := unmarshal(&s); err == nil {
		return r.set(s)
	}
	var n int
	if err := unmarshal(&n); err != nil {
		return fmt.Errorf(`retention must be a non-negative integer or %q`, RetainAllToken)
	}
	if n < 0 {
		*r = RetainAll()
		return nil
	}
	*r = RetainN(n)
	return nil
}

func (r RetainPolicy) MarshalYAML() (any, error) {
	if r.Unlimited {
		return RetainAllToken, nil
	}
	return r.N, nil
}

func (r RetainPolicy) MarshalJSON() ([]byte, error) {
	if r.Unlimited {
		return marshalNoEscape(RetainAllToken)
	}
	return marshalNoEscape(r.N)
}

func (r *RetainPolicy) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		return r.set(s)
	}
	var n int
	if err := json.Unmarshal(b, &n); err != nil {
		return fmt.Errorf(`retention must be a number or %q`, RetainAllToken)
	}
	if n < 0 {
		*r = RetainAll()
		return nil
	}
	*r = RetainN(n)
	return nil
}
```

---

## 9. fault_kinds.go: the 17-kind registry

The registry is schema-level because config validation (`allow`/`deny`), the fault parser,
and the oracle-lock "fault-space floor" all need it in Phase 0. The *implementations* are
Phase 2 and live in `internal/perturber/faults`.

```go
package schema

// FaultKind is a dotted fault identifier, e.g. "net.partition".
type FaultKind string

// Capability is a host/backend facility a fault kind needs. The perturber
// declares which capabilities the running backend actually provides; the
// schema declares only what each kind REQUIRES.
//
// This is the model that makes the Windows host expressible: on Windows the
// `process` backend provides neither CapSignalStop (no SIGSTOP) nor CapNetFilter
// (no iptables), so proc.pause and every net.* kind are unavailable there,
// while the `compose` backend provides both inside the Docker Desktop Linux VM.
type Capability string

const (
	CapNetFilter  Capability = "net.filter"   // iptables / nftables in a netns
	CapNetShape   Capability = "net.shape"    // tc / netem
	CapSignalStop Capability = "signal.stop"  // SIGSTOP / SIGCONT
	CapProcCtl    Capability = "proc.control" // spawn/kill/restart a supervised process
	CapCPUQuota   Capability = "cpu.quota"    // cgroup cpu throttling
	CapMemQuota   Capability = "mem.quota"    // cgroup memory pressure
	CapClockShift Capability = "clock.shift"  // libfaketime / time namespaces
	CapIOInject   Capability = "io.inject"    // FUSE shim / device-mapper delay
	CapDiskFill   Capability = "disk.fill"    // write into the node's volume
	CapFDLimit    Capability = "fd.limit"     // rlimit manipulation
)

// ParamType constrains a fault parameter's textual value.
type ParamType uint8

const (
	ParamDurationType ParamType = iota + 1 // "100ms", "2s"
	ParamPercentType                       // 0..100, or 0..1 fraction
	ParamRateType                          // 0..1
	ParamIntType
	ParamBytesType  // bits/bytes per second
	ParamSignalType // "SIGKILL" or 9
)

type ParamDecl struct {
	Name     string
	Type     ParamType
	Required bool
	Default  string // exact canonical source text; "" if none
}

// KindDecl is the frozen declaration of one fault kind.
type KindDecl struct {
	Kind     FaultKind
	Family   string // derived from the dotted prefix
	Params   []ParamDecl
	Durative bool // true: active over [start,end), withdrawn at end.
	// false: fires AT start; [start,end] is the tolerated
	// disturbance window used by no_crash and friends.
	EdgeOK   bool // may target an edge (a<->b)
	Requires []Capability
	Doc      string
}

// Kinds is the complete v1 fault matrix (17 kinds).
var Kinds = map[FaultKind]KindDecl{
	"net.partition": {Kind: "net.partition", Family: "net", Durative: true, EdgeOK: true,
		Requires: []Capability{CapNetFilter},
		Doc:      "drop all traffic to/from the target (or across the edge)"},
	"net.latency": {Kind: "net.latency", Family: "net", Durative: true, EdgeOK: true,
		Params: []ParamDecl{
			{Name: "mean", Type: ParamDurationType, Required: true},
			{Name: "jitter", Type: ParamDurationType, Default: "0ms"},
		},
		Requires: []Capability{CapNetShape}},
	"net.loss": {Kind: "net.loss", Family: "net", Durative: true, EdgeOK: true,
		Params:   []ParamDecl{{Name: "pct", Type: ParamPercentType, Required: true}},
		Requires: []Capability{CapNetShape}},
	"net.reorder": {Kind: "net.reorder", Family: "net", Durative: true, EdgeOK: true,
		Params:   []ParamDecl{{Name: "pct", Type: ParamPercentType, Required: true}},
		Requires: []Capability{CapNetShape}},
	"net.duplicate": {Kind: "net.duplicate", Family: "net", Durative: true, EdgeOK: true,
		Params:   []ParamDecl{{Name: "pct", Type: ParamPercentType, Required: true}},
		Requires: []Capability{CapNetShape}},
	"net.bandwidth": {Kind: "net.bandwidth", Family: "net", Durative: true, EdgeOK: true,
		Params:   []ParamDecl{{Name: "bps", Type: ParamBytesType, Required: true}},
		Requires: []Capability{CapNetShape}},

	"proc.kill": {Kind: "proc.kill", Family: "proc", Durative: false,
		Params:   []ParamDecl{{Name: "signal", Type: ParamSignalType, Default: "SIGKILL"}},
		Requires: []Capability{CapProcCtl}},
	"proc.pause": {Kind: "proc.pause", Family: "proc", Durative: true,
		Requires: []Capability{CapSignalStop},
		Doc:      "SIGSTOP at start, SIGCONT at end; the primary gray-failure generator"},
	"proc.restart": {Kind: "proc.restart", Family: "proc", Durative: false,
		Requires: []Capability{CapProcCtl}},
	"proc.slow": {Kind: "proc.slow", Family: "proc", Durative: true,
		Params:   []ParamDecl{{Name: "cpu_pct", Type: ParamPercentType, Required: true}},
		Requires: []Capability{CapCPUQuota}},

	"clock.skew": {Kind: "clock.skew", Family: "clock", Durative: true,
		Params:   []ParamDecl{{Name: "ms", Type: ParamIntType, Required: true}},
		Requires: []Capability{CapClockShift}},
	"clock.jump": {Kind: "clock.jump", Family: "clock", Durative: false,
		Params:   []ParamDecl{{Name: "ms", Type: ParamIntType, Required: true}},
		Requires: []Capability{CapClockShift}},

	"io.latency": {Kind: "io.latency", Family: "io", Durative: true,
		Params:   []ParamDecl{{Name: "ms", Type: ParamIntType, Required: true}},
		Requires: []Capability{CapIOInject}},
	"io.error": {Kind: "io.error", Family: "io", Durative: true,
		Params:   []ParamDecl{{Name: "rate", Type: ParamRateType, Required: true}},
		Requires: []Capability{CapIOInject}},
	"io.fill": {Kind: "io.fill", Family: "io", Durative: true,
		Params:   []ParamDecl{{Name: "pct", Type: ParamPercentType, Required: true}},
		Requires: []Capability{CapDiskFill}},

	"mem.pressure": {Kind: "mem.pressure", Family: "mem", Durative: true,
		Params:   []ParamDecl{{Name: "pct", Type: ParamPercentType, Required: true}},
		Requires: []Capability{CapMemQuota}},
	"fd.exhaust": {Kind: "fd.exhaust", Family: "fd", Durative: true,
		Requires: []Capability{CapFDLimit}},
}

// AllFaultKinds returns every kind, sorted, for stable lock hashing and --help.
func AllFaultKinds() []FaultKind { /* sorted keys of Kinds */ }

func (k FaultKind) Valid() bool { _, ok := Kinds[k]; return ok }

func (k FaultKind) Decl() (KindDecl, bool) { d, ok := Kinds[k]; return d, ok }

func (k FaultKind) Family() string {
	if d, ok := Kinds[k]; ok {
		return d.Family
	}
	return ""
}
```

---

## 10. fault.go: `KIND(TARGET[, PARAMS])@WINDOW`

### The central decision

The verdict's `surviving_faults` is literally an array of **strings**
(`"proc.pause(role:leader)@8200..15100"`). So the *wire form of a fault is the string*.
`FaultSpec` therefore implements `MarshalJSON`/`UnmarshalJSON` as a canonical string; the AST
exists only in memory. This gives one representation across `.thesis`, the verdict, the CLI,
and log lines: portable across repos (CRUCIBLE PART 10), diffable, and hash-stable.

### Canonical form

`kind(target, name=value, name=value)@start..end`: comma-space separator (matching the
metasyntax `KIND(TARGET[, PARAMS])@WINDOW`), params **named and complete** (defaults are
materialized, so a world is immune to future default drift), window in **milliseconds
relative to DRIVE start** (may be negative).

Round-trip property, asserted by test:
`Canonical(Parse(Canonical(f))) == Canonical(f)` for every f.

```go
package schema

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ---------- targets ----------

type TargetKind uint8

const (
	TargetInvalid TargetKind = iota
	TargetAll                // *          ADDITIVE: every node
	TargetNode               // n1
	TargetSelector           // kv:*  kv:n1
	TargetRole               // role:leader
	TargetEdge               // n1<->n2
	TargetQuorum             // minority(kv) | majority(kv)
)

type QuorumFunc string

const (
	QuorumMinority QuorumFunc = "minority"
	QuorumMajority QuorumFunc = "majority"
)

// Target is a tagged union. A struct rather than an interface because the wire
// form is a string: the AST never needs polymorphic (de)serialization.
type Target struct {
	Kind    TargetKind
	Node    string     // TargetNode
	Service string     // TargetSelector, TargetQuorum
	Glob    string     // TargetSelector  ("*", "n?", "n1")
	Role    string     // TargetRole
	Quorum  QuorumFunc // TargetQuorum
	A, B    *Target    // TargetEdge endpoints (never themselves edges)
}

var (
	identRe  = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.\-]*$`)
	globRe   = regexp.MustCompile(`^[A-Za-z0-9_*?\[\].\-]+$`)
	windowRe = regexp.MustCompile(`^(-?\d+)\.\.(-?\d+)$`)
)

// RolePrefix is reserved: a target beginning "role:" is always a role target,
// never a service selector. "role" may not be used as a service name.
const RolePrefix = "role:"

func ParseTarget(s string) (*Target, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, fmt.Errorf("target: empty")
	}
	if i := indexTopLevel(s, "<->"); i >= 0 {
		a, err := parseSimpleTarget(s[:i])
		if err != nil {
			return nil, err
		}
		b, err := parseSimpleTarget(s[i+3:])
		if err != nil {
			return nil, err
		}
		return &Target{Kind: TargetEdge, A: a, B: b}, nil
	}
	return parseSimpleTarget(s)
}

func parseSimpleTarget(s string) (*Target, error) {
	s = strings.TrimSpace(s)
	switch {
	case s == "":
		return nil, fmt.Errorf("target: empty")
	case s == "*":
		return &Target{Kind: TargetAll}, nil
	case strings.Contains(s, "<->"):
		return nil, fmt.Errorf("target %q: nested edge targets are not allowed", s)
	case strings.HasPrefix(s, RolePrefix):
		r := strings.TrimSpace(s[len(RolePrefix):])
		if !identRe.MatchString(r) {
			return nil, fmt.Errorf("target %q: invalid role name", s)
		}
		return &Target{Kind: TargetRole, Role: r}, nil
	case strings.HasSuffix(s, ")"):
		op := strings.IndexByte(s, '(')
		if op < 0 {
			return nil, fmt.Errorf("target %q: unbalanced parentheses", s)
		}
		fn := QuorumFunc(strings.TrimSpace(s[:op]))
		if fn != QuorumMinority && fn != QuorumMajority {
			return nil, fmt.Errorf("target %q: unknown quorum function %q (want minority|majority)", s, fn)
		}
		svc := strings.TrimSpace(s[op+1 : len(s)-1])
		if !identRe.MatchString(svc) {
			return nil, fmt.Errorf("target %q: invalid service name %q", s, svc)
		}
		return &Target{Kind: TargetQuorum, Quorum: fn, Service: svc}, nil
	case strings.ContainsRune(s, ':'):
		i := strings.IndexByte(s, ':')
		svc, glob := strings.TrimSpace(s[:i]), strings.TrimSpace(s[i+1:])
		if !identRe.MatchString(svc) || !globRe.MatchString(glob) {
			return nil, fmt.Errorf("target %q: want service:glob", s)
		}
		return &Target{Kind: TargetSelector, Service: svc, Glob: glob}, nil
	default:
		if !identRe.MatchString(s) {
			return nil, fmt.Errorf("target %q: invalid node id", s)
		}
		return &Target{Kind: TargetNode, Node: s}, nil
	}
}

func (t *Target) String() string {
	if t == nil {
		return ""
	}
	switch t.Kind {
	case TargetAll:
		return "*"
	case TargetNode:
		return t.Node
	case TargetSelector:
		return t.Service + ":" + t.Glob
	case TargetRole:
		return RolePrefix + t.Role
	case TargetEdge:
		return t.A.String() + "<->" + t.B.String()
	case TargetQuorum:
		return string(t.Quorum) + "(" + t.Service + ")"
	}
	return ""
}

func (t *Target) Equal(o *Target) bool { /* deep compare */ }

// ---------- params ----------

// Param holds the parameter name and the EXACT source text of its value.
// Values are never re-formatted: "0.05" is not rewritten to "0.05000", and no
// float ever round-trips through binary, so world hashes are stable.
type Param struct {
	Name string
	Raw  string
}

// ---------- fault spec ----------

// FaultSpec is one scheduled fault. Its wire form is the canonical string.
type FaultSpec struct {
	Kind    FaultKind
	Target  *Target
	Params  []Param // in KindDecl order, complete (defaults materialized)
	StartMS int64   // ms relative to DRIVE start; may be negative
	EndMS   int64
}

// ParseFault parses and validates against the kind registry.
func ParseFault(s string) (FaultSpec, error) {
	f, err := ParseFaultLoose(s)
	if err != nil {
		return FaultSpec{}, err
	}
	if err := f.bind(); err != nil {
		return FaultSpec{}, fmt.Errorf("fault %q: %w", s, err)
	}
	return f, nil
}

// ParseFaultLoose parses syntax only, with no registry check. For diagnostics
// and for rendering faults produced by a newer tool version.
func ParseFaultLoose(s string) (FaultSpec, error) {
	s = strings.TrimSpace(s)
	at := strings.LastIndexByte(s, '@')
	if at < 0 {
		return FaultSpec{}, fmt.Errorf("fault %q: missing @WINDOW (want KIND(TARGET)@start..end)", s)
	}
	head := strings.TrimSpace(s[:at])
	m := windowRe.FindStringSubmatch(strings.TrimSpace(s[at+1:]))
	if m == nil {
		return FaultSpec{}, fmt.Errorf("fault %q: window must be start_ms..end_ms", s)
	}
	start, _ := strconv.ParseInt(m[1], 10, 64)
	end, _ := strconv.ParseInt(m[2], 10, 64)
	if end < start {
		return FaultSpec{}, fmt.Errorf("fault %q: window end %d precedes start %d", s, end, start)
	}

	op := strings.IndexByte(head, '(')
	if op < 0 || !strings.HasSuffix(head, ")") {
		return FaultSpec{}, fmt.Errorf("fault %q: missing (TARGET[, PARAMS])", s)
	}
	kind := FaultKind(strings.TrimSpace(head[:op]))
	args, err := splitTopLevel(head[op+1:len(head)-1], ',')
	if err != nil {
		return FaultSpec{}, fmt.Errorf("fault %q: %w", s, err)
	}
	if len(args) == 0 || strings.TrimSpace(args[0]) == "" {
		return FaultSpec{}, fmt.Errorf("fault %q: missing TARGET", s)
	}
	tgt, err := ParseTarget(args[0])
	if err != nil {
		return FaultSpec{}, fmt.Errorf("fault %q: %w", s, err)
	}
	f := FaultSpec{Kind: kind, Target: tgt, StartMS: start, EndMS: end}
	for _, a := range args[1:] {
		a = strings.TrimSpace(a)
		if a == "" {
			return FaultSpec{}, fmt.Errorf("fault %q: empty parameter", s)
		}
		if i := strings.IndexByte(a, '='); i >= 0 {
			f.Params = append(f.Params, Param{
				Name: strings.TrimSpace(a[:i]),
				Raw:  strings.TrimSpace(a[i+1:]),
			})
		} else {
			f.Params = append(f.Params, Param{Raw: a}) // positional, bound in bind()
		}
	}
	return f, nil
}

// bind resolves positional params against the registry, applies defaults,
// checks types and arity, and sorts params into declared order.
func (f *FaultSpec) bind() error {
	d, ok := Kinds[f.Kind]
	if !ok {
		return fmt.Errorf("unknown fault kind %q", f.Kind)
	}
	if f.Target.Kind == TargetEdge && !d.EdgeOK {
		return fmt.Errorf("kind %s does not accept an edge target", f.Kind)
	}
	if d.Durative && f.EndMS == f.StartMS {
		return fmt.Errorf("kind %s is durative: window must have end_ms > start_ms", f.Kind)
	}

	// bind positionals
	pos := 0
	byName := map[string]string{}
	for _, p := range f.Params {
		name := p.Name
		if name == "" {
			if pos >= len(d.Params) {
				return fmt.Errorf("kind %s takes at most %d parameters", f.Kind, len(d.Params))
			}
			name = d.Params[pos].Name
			pos++
		}
		if _, dup := byName[name]; dup {
			return fmt.Errorf("duplicate parameter %q", name)
		}
		byName[name] = p.Raw
	}

	out := make([]Param, 0, len(d.Params))
	for _, decl := range d.Params {
		raw, given := byName[decl.Name]
		if !given {
			if decl.Required {
				return fmt.Errorf("missing required parameter %q", decl.Name)
			}
			raw = decl.Default
			if raw == "" {
				continue
			}
		}
		if err := validateParamValue(decl, raw); err != nil {
			return err
		}
		out = append(out, Param{Name: decl.Name, Raw: raw})
		delete(byName, decl.Name)
	}
	if len(byName) > 0 {
		names := make([]string, 0, len(byName))
		for k := range byName {
			names = append(names, k)
		}
		sort.Strings(names)
		return fmt.Errorf("unknown parameter(s) %v for kind %s", names, f.Kind)
	}
	f.Params = out
	return nil
}

func validateParamValue(d ParamDecl, raw string) error {
	switch d.Type {
	case ParamDurationType:
		if _, err := time.ParseDuration(raw); err != nil {
			return fmt.Errorf("parameter %s: want a duration with a unit, got %q", d.Name, raw)
		}
	case ParamPercentType, ParamRateType, ParamBytesType:
		if _, err := strconv.ParseFloat(raw, 64); err != nil {
			return fmt.Errorf("parameter %s: want a number, got %q", d.Name, raw)
		}
	case ParamIntType:
		if _, err := strconv.ParseInt(raw, 10, 64); err != nil {
			return fmt.Errorf("parameter %s: want an integer, got %q", d.Name, raw)
		}
	case ParamSignalType:
		if !strings.HasPrefix(raw, "SIG") {
			if _, err := strconv.Atoi(raw); err != nil {
				return fmt.Errorf("parameter %s: want SIGNAME or a signal number, got %q", d.Name, raw)
			}
		}
	}
	return nil
}

// Canonical renders the normative string form.
func (f FaultSpec) Canonical() string {
	var b strings.Builder
	b.WriteString(string(f.Kind))
	b.WriteByte('(')
	b.WriteString(f.Target.String())
	for _, p := range f.Params {
		b.WriteString(", ")
		b.WriteString(p.Name)
		b.WriteByte('=')
		b.WriteString(p.Raw)
	}
	b.WriteString(")@")
	b.WriteString(strconv.FormatInt(f.StartMS, 10))
	b.WriteString("..")
	b.WriteString(strconv.FormatInt(f.EndMS, 10))
	return b.String()
}

func (f FaultSpec) String() string { return f.Canonical() }

// Durative reports whether the fault occupies [StartMS,EndMS) and needs withdraw().
// For non-durative kinds the fault fires at StartMS and [StartMS,EndMS] is the
// window during which the resulting disturbance is EXPECTED (so no_crash does
// not flag a planned proc.kill).
func (f FaultSpec) Durative() bool {
	d, ok := Kinds[f.Kind]
	return ok && d.Durative
}

// RequiredCapabilities is what a backend must provide to inject this fault.
func (f FaultSpec) RequiredCapabilities() []Capability {
	if d, ok := Kinds[f.Kind]; ok {
		return d.Requires
	}
	return nil
}

// Overlaps reports whether two faults are concurrent (search biases toward this).
func (f FaultSpec) Overlaps(o FaultSpec) bool {
	return f.StartMS < o.EndMS && o.StartMS < f.EndMS
}

// --- wire form: a canonical string ---

func (f FaultSpec) MarshalJSON() ([]byte, error) { return marshalNoEscape(f.Canonical()) }

func (f *FaultSpec) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("schema: fault must be a JSON string: %w", err)
	}
	p, err := ParseFault(s)
	if err != nil {
		return err
	}
	*f = p
	return nil
}

func (f *FaultSpec) UnmarshalYAML(unmarshal func(any) error) error { /* same via string */ }
func (f FaultSpec) MarshalYAML() (any, error)                      { return f.Canonical(), nil }

// FaultSchedule is an ordered fault list.
type FaultSchedule []FaultSpec

// Normalize sorts by (start, end, canonical string) so that two schedules with
// the same faults produce the same world_hash regardless of discovery order.
func (s FaultSchedule) Normalize() FaultSchedule { /* stable sort, never nil */ }

// MaxConcurrent is the peak overlap, checked against perturber.budget.
func (s FaultSchedule) MaxConcurrent() int { /* sweep line over endpoints */ }

// --- helpers ---

// splitTopLevel splits on sep at paren depth 0, so that
// "minority(kv), pct=5" yields ["minority(kv)", " pct=5"].
func splitTopLevel(s string, sep byte) ([]string, error) { /* depth counter, error on imbalance */ }

// indexTopLevel finds sub at paren depth 0, or -1.
func indexTopLevel(s, sub string) int { /* depth counter */ }
```

---

## 11. config.go: `prothesis.yaml`

```go
package schema

import "fmt"

// Config is prothesis.yaml (directive 4.2). Field names are normative.
// No omitempty: the effective (defaulted) config is canonicalized and hashed
// into .prothesis/lock, so its JSON form must be total and stable.
type Config struct {
	Version   string             `yaml:"version"   json:"version"`
	Name      string             `yaml:"name"      json:"name"`
	Harness   Harness            `yaml:"harness"   json:"harness"`
	Driver    Driver             `yaml:"driver"    json:"driver"`
	Perturber Perturber          `yaml:"perturber" json:"perturber"`
	Oracles   Oracles            `yaml:"oracles"   json:"oracles"`
	Profiles  map[string]Profile `yaml:"profiles"  json:"profiles"`
	Artifacts Artifacts          `yaml:"artifacts" json:"artifacts"`
}

type Harness struct {
	Backend     Backend       `yaml:"backend"      json:"backend"`
	File        string        `yaml:"file"         json:"file"`
	Nodes       []Node        `yaml:"nodes"        json:"nodes"`
	Health      []HealthProbe `yaml:"health"       json:"health"`
	SteadyState *SteadyState  `yaml:"steady_state" json:"steady_state"`
}

type Node struct {
	ID       string `yaml:"id"        json:"id"`
	Service  string `yaml:"service"   json:"service"`
	RoleHint string `yaml:"role_hint" json:"role_hint"` // advisory, open string
}

// HealthProbe.Probe is polymorphic by shape, not by type: a value parsing as an
// http/https URL is an HTTP probe (success = 2xx); anything else is an exec
// probe (success = exit 0). Placeholders {host} {port} {node} {service} are
// substituted before use.
type HealthProbe struct {
	Node    string   `yaml:"node"    json:"node"` // a fault-grammar target selector, e.g. "kv:*"
	Probe   string   `yaml:"probe"   json:"probe"`
	Timeout Duration `yaml:"timeout" json:"timeout"`
}

type SteadyState struct {
	Probe   string   `yaml:"probe"   json:"probe"`
	Timeout Duration `yaml:"timeout" json:"timeout"`
}

// Probe placeholder tokens.
const (
	PlaceholderHost    = "{host}"
	PlaceholderPort    = "{port}"
	PlaceholderNode    = "{node}"
	PlaceholderService = "{service}"
)

type Driver struct {
	Cmd      string                   `yaml:"cmd"      json:"cmd"`
	Profiles map[string]DriverProfile `yaml:"profiles" json:"profiles"`
}

// driver.cmd placeholder tokens. {profile_json} and the
// PROTHESIS_DRIVER_PROFILE env var are ADDITIVE: without them a driver can
// receive only a profile NAME and has no way to learn clients/ops/mix.
const (
	PlaceholderHistoryPath = "{history_path}"
	PlaceholderSeed        = "{seed}"
	PlaceholderProfile     = "{profile}"
	PlaceholderProfileJSON = "{profile_json}" // ADDITIVE
	EnvDriverProfile       = "PROTHESIS_DRIVER_PROFILE"
)

// DriverProfile is the ONLY part of prothesis.yaml that is not strictly
// decoded. Its contents are a payload for a user-authored load generator, so
// unknown keys are captured in Extra rather than rejected. Extra IS included in
// the canonical hash, so it cannot be used to smuggle changes past the lock.
type DriverProfile struct {
	Clients int                `yaml:"clients" json:"clients"`
	Ops     int                `yaml:"ops"     json:"ops"`
	Mix     map[string]float64 `yaml:"mix"     json:"mix"`
	Extra   map[string]any     `yaml:"-"       json:"-"`
}

// UnmarshalYAML decodes into a map first so that the strict decoder's
// KnownFields check never sees this subtree, then extracts the known keys.
func (p *DriverProfile) UnmarshalYAML(unmarshal func(any) error) error {
	var m map[string]any
	if err := unmarshal(&m); err != nil {
		return fmt.Errorf("driver profile must be a mapping: %w", err)
	}
	*p = DriverProfile{}
	for k, v := range m {
		switch k {
		case "clients":
			n, ok := asInt(v)
			if !ok {
				return fmt.Errorf("driver profile: clients must be an integer, got %v", v)
			}
			p.Clients = n
		case "ops":
			n, ok := asInt(v)
			if !ok {
				return fmt.Errorf("driver profile: ops must be an integer, got %v", v)
			}
			p.Ops = n
		case "mix":
			mm, ok := v.(map[string]any)
			if !ok {
				return fmt.Errorf("driver profile: mix must be a mapping, got %T", v)
			}
			p.Mix = make(map[string]float64, len(mm))
			for op, w := range mm {
				f, ok := asFloat(w)
				if !ok {
					return fmt.Errorf("driver profile: mix.%s must be a number, got %v", op, w)
				}
				p.Mix[op] = f
			}
		default:
			if p.Extra == nil {
				p.Extra = map[string]any{}
			}
			p.Extra[k] = v
		}
	}
	return nil
}

// MarshalJSON re-merges Extra so the canonical hash covers every key the user wrote.
func (p DriverProfile) MarshalJSON() ([]byte, error) {
	m := map[string]any{"clients": p.Clients, "ops": p.Ops, "mix": p.Mix}
	if p.Mix == nil {
		m["mix"] = map[string]float64{}
	}
	for k, v := range p.Extra {
		if _, clash := m[k]; !clash {
			m[k] = v
		}
	}
	return marshalNoEscape(m)
}

func (p DriverProfile) MarshalYAML() (any, error) { /* same map */ }

type Perturber struct {
	Budget PerturberBudget `yaml:"budget" json:"budget"`
	Allow  []FaultKind     `yaml:"allow"  json:"allow"`
	Deny   []FaultKind     `yaml:"deny"   json:"deny"`
	// Constraints are FREE TEXT in the directive's sample
	// ("never partition more than minority of kv"). Phase 0 stores them
	// verbatim and hashes them into the lock. Phase 2 must introduce a parsed
	// grammar; an unparseable constraint is CONFIG_ERROR, never silently
	// ignored (a constraint the perturber ignores is a safety hole).
	// See OPEN_QUESTIONS.md.
	Constraints []string `yaml:"constraints" json:"constraints"`
}

type PerturberBudget struct {
	MaxConcurrentFaults int `yaml:"max_concurrent_faults" json:"max_concurrent_faults"`
	MaxFaultsPerWorld   int `yaml:"max_faults_per_world"   json:"max_faults_per_world"`
}

type Oracles struct {
	Dir     string          `yaml:"dir"     json:"dir"`
	Builtin []BuiltinOracle `yaml:"builtin" json:"builtin"`
}

// Profile is one entry of the top-level `profiles` map, written in YAML flow
// style: { budget: 8h, worlds: -1, driver_profile: soak, search: true }.
// Keys are heterogeneous only in that they are optional; the key SET is closed
// and decoded strictly.
type Profile struct {
	Budget Duration `yaml:"budget" json:"budget"`
	// Worlds: -1 means UNBOUNDED (run until the budget expires).
	// nil (key absent) means "inherit the CLI default"; an explicit 0 is a
	// config error. A pointer is the only way to tell absent from 0, and the
	// distinction is load-bearing because profile budgets are lock-covered.
	Worlds        *int   `yaml:"worlds"         json:"worlds"`
	DriverProfile string `yaml:"driver_profile" json:"driver_profile"`
	Search        bool   `yaml:"search"         json:"search"`
}

// WorldsUnbounded is the sentinel for `worlds: -1`.
const WorldsUnbounded = -1

func (p Profile) Unbounded() bool { return p.Worlds != nil && *p.Worlds == WorldsUnbounded }

type Artifacts struct {
	Dir           string       `yaml:"dir"            json:"dir"`
	RetainPassing RetainPolicy `yaml:"retain_passing" json:"retain_passing"`
	RetainFailing RetainPolicy `yaml:"retain_failing" json:"retain_failing"`
}

func asInt(v any) (int, bool)     { /* int, int64, uint64, float64 with no fraction */ }
func asFloat(v any) (float64, bool) { /* int, int64, float64 */ }
```

### config_validate.go

```go
// ApplyDefaults fills unset fields with their documented defaults. Lock hashing
// operates on the DEFAULTED config, so writing out a default explicitly does
// not change the manifest, but changing a value does.
func (c *Config) ApplyDefaults() { /* artifacts.dir, oracles.dir, timeouts, worlds */ }

// Validate returns a ValidationErrors listing every problem, so a user fixes
// the whole file in one pass. Any non-nil result maps to ExitConfigError.
func (c *Config) Validate() error {
	var errs ValidationErrors
	// version must equal ConfigVersion
	// name non-empty and slug-shaped
	// backend valid AND ImplementedV1 (k8s/sim -> "not implemented in v1")
	// compose backend: harness.file non-empty
	// nodes: >=1, ids unique, id/service non-empty, service != "role"
	// health[].node parses as a Target; timeout > 0
	// driver.cmd non-empty and contains {history_path}
	// driver.profiles: >=1; clients>0; ops>0; mix keys non-empty;
	//   sum(mix) == 1.0 +/- 1e-6
	// perturber.allow/deny: every kind in Kinds; allow and deny disjoint
	// perturber.budget: max_concurrent_faults >= 1; max_faults_per_world >= 1
	// oracles.builtin: every name a known BuiltinOracle
	// profiles: >=1; budget > 0; worlds != 0 (explicit 0 rejected);
	//   worlds >= -1; driver_profile resolves in driver.profiles
	// artifacts.dir non-empty
	return errs.OrNil()
}

// EffectiveFaultSpace is the allow-minus-deny kind set, sorted. This is what
// the oracle lock hashes as the "fault-space floor": narrowing it is drift.
func (c *Config) EffectiveFaultSpace() []FaultKind { /* ... */ }

// LockPayload is the canonical, hashable projection of the config used by
// .prothesis/lock: the fault space, the perturber budget, and every profile
// budget. Defined here so Phase 3 cannot quietly change what is covered.
func (c *Config) LockPayload() map[string]any { /* ... */ }
```

---

## 12. world.go: the `.thesis` file

### Structure

`WorldCore` is the five-field normative tuple. `World` **embeds it anonymously**, so
`encoding/json` inlines the fields into a flat file *and* `w.WorldCore` is, by construction,
exactly the hashable subset. The hashed subset can never drift from the file, because it is
the same struct.

```go
package schema

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// MaxSeed keeps seeds inside IEEE-754 exact integer range so that .thesis files
// survive JSON parsers that back numbers with float64 (JavaScript, jq).
const MaxSeed = uint64(1)<<53 - 1

// WorldCore is the normative world tuple (invariant I2):
// (seed, topology_variant, driver_profile, fault_schedule, phase_timings).
// world_hash covers exactly this struct and nothing else.
type WorldCore struct {
	Seed            uint64        `json:"seed"`
	TopologyVariant string        `json:"topology_variant"`
	DriverProfile   string        `json:"driver_profile"`
	FaultSchedule   FaultSchedule `json:"fault_schedule"`
	PhaseTimings    PhaseTimings  `json:"phase_timings"`
}

// Provenance is ADDITIVE and is NOT hashed: it records where a world came from
// without making cosmetic metadata part of its identity.
type Provenance struct {
	CreatedAt       string `json:"created_at"`        // RFC3339 UTC
	Tool            string `json:"tool"`              // "thesis/0.1.0"
	DeterminismTier string `json:"determinism_tier"`  // "B" in v1 (record & replay)
	ConfigSHA       string `json:"config_sha"`        // sha256: of the defaulted config
	ParentWorldHash string `json:"parent_world_hash"` // shrink/mutation lineage, "" if seed world
	Note            string `json:"note"`
}

// World is a .thesis file. Written as deterministic indented JSON in struct
// order; hashed as compact sorted-key canonical JSON of the embedded core.
type World struct {
	Schema     string     `json:"schema"`
	WorldHash  string     `json:"world_hash"`
	WorldCore             // embedded: fields inline into the flat file
	Provenance Provenance `json:"provenance"`
}

var ErrWorldHashMismatch = errors.New("schema: world_hash does not match world contents")

// normalize makes the hash input canonical: nil slices become empty slices
// (JSON null and [] canonicalize differently and would hash differently),
// and the fault schedule is sorted into a stable order.
func (c *WorldCore) normalize() {
	if c.FaultSchedule == nil {
		c.FaultSchedule = FaultSchedule{}
	}
	c.FaultSchedule = c.FaultSchedule.Normalize()
	if c.PhaseTimings == nil {
		c.PhaseTimings = PhaseTimings{}
	}
	if c.TopologyVariant == "" {
		c.TopologyVariant = DefaultTopologyVariant
	}
}

const DefaultTopologyVariant = "default"

// ComputeHash returns "sha256:"+hex over the canonical encoding of the core.
func (w *World) ComputeHash() (string, error) {
	c := w.WorldCore
	c.normalize()
	return HashCanonical(c)
}

// Seal normalizes, stamps the schema id, and fills world_hash. Call before write.
func (w *World) Seal() error {
	w.Schema = WorldSchema
	w.WorldCore.normalize()
	if w.Seed > MaxSeed {
		return fmt.Errorf("schema: seed %d exceeds MaxSeed %d", w.Seed, MaxSeed)
	}
	h, err := w.ComputeHash()
	if err != nil {
		return err
	}
	w.WorldHash = h
	return nil
}

// Verify recomputes the hash and compares. A mismatch is an error by default:
// a .thesis under .prothesis/regressions/ is lock-covered, so silently
// accepting a hand-edited world would be an anti-gaming hole. `thesis world
// reseal` is the explicit, auditable way to re-stamp one.
func (w *World) Verify() error {
	h, err := w.ComputeHash()
	if err != nil {
		return err
	}
	if h != w.WorldHash {
		return fmt.Errorf("%w: file says %s, contents hash to %s", ErrWorldHashMismatch, w.WorldHash, h)
	}
	return nil
}

func (w *World) Validate() error {
	var errs ValidationErrors
	// schema == WorldSchema
	// seed in [1, MaxSeed]
	// topology_variant, driver_profile non-empty
	// phase_timings valid (DRIVE starts at 0)
	// every fault kind known; windows ordered
	return errs.OrNil()
}

// MarshalWorld renders the on-disk form: HTML-escaping OFF (so n1<->n2 survives),
// two-space indent in struct declaration order, trailing newline.
func MarshalWorld(w *World) ([]byte, error) {
	if err := w.Seal(); err != nil {
		return nil, err
	}
	b, err := marshalNoEscape(w)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	if err := json.Indent(&out, b, "", "  "); err != nil {
		return nil, err
	}
	out.WriteByte('\n')
	return out.Bytes(), nil
}

// UnmarshalWorld parses, normalizes, and verifies the content address.
func UnmarshalWorld(b []byte) (*World, error) {
	var w World
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&w); err != nil {
		return nil, fmt.Errorf("schema: invalid .thesis file: %w", err)
	}
	if w.Schema != WorldSchema {
		return nil, fmt.Errorf("schema: .thesis has schema %q, want %q", w.Schema, WorldSchema)
	}
	w.WorldCore.normalize()
	if err := w.Verify(); err != nil {
		return nil, err
	}
	if err := w.Validate(); err != nil {
		return nil, err
	}
	return &w, nil
}

// Filename is the conventional name: w_<first 4 hex of world_hash>.thesis,
// matching the directive's w_a41f.thesis. Callers extend by 4 on collision.
func (w *World) Filename() string { return WorldFilename(w.WorldHash, 4) }
```

**Byte-identical round-trip** (Phase 0 DoD (b)) is then a two-line test:

```go
b1, _ := MarshalWorld(w)
w2, _ := UnmarshalWorld(b1)
b2, _ := MarshalWorld(w2)
// require bytes.Equal(b1, b2) AND w.WorldHash == w2.WorldHash
```

It holds because: hashing normalizes nil→empty and sorts the schedule; faults serialize as
canonical strings (defaults materialized, so parse→print is idempotent); numbers are integers
only; and `json.Indent` over a fixed struct order is deterministic.

---

## 13. history.go: the JSONL log

### The union problem, stated precisely

The directive gives two record shapes under one `type` field:

```json
{"t_ns":…,"process":3,"type":"invoke","f":"write","key":"k/42","value":7,"op_id":8891}
{"t_ns":…,"type":"info","event":"phase","phase":"HEAL"}
```

and separately defines `info` as the **indeterminate operation outcome** ("crucial for sound
consistency validation"; CRUCIBLE: "Drivers that cannot distinguish MUST emit `info`").

So `type:"info"` is **overloaded**: it is both an op outcome and a marker record. `type` alone
does not discriminate. **The discriminator is the presence of `event`.**

```
event present  => marker record  (phase transitions, fault notes, clock origin)
event absent   => operation record (invoke | ok | fail | info)
```

### Zero-value hazards

`process: 0`, `op_id: 0`, `key: ""`, and `value: 0/false/null` are all legitimate. So every
optional field whose zero value is meaningful is a **pointer**; only fields whose empty value
is definitionally invalid (`f`, `error`, `event`, `phase`) use `string` + `omitempty`.

```go
package schema

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// HistoryEntry is one line of the append-only history log (directive 4.4).
//
// Field NAMES are normative; field ORDER within the JSON object is not.
//
// Semantics (CRUCIBLE 3.6, required for a sound consistency checker):
//   invoke - operation issued
//   ok     - definitely happened
//   fail   - definitely did NOT happen
//   info   - INDETERMINATE. A driver that cannot distinguish MUST emit info.
//
// A record carrying `event` is a MARKER record, not an operation. Markers
// always use type "info".
type HistoryEntry struct {
	// TNS is nanoseconds on the recorder virtual clock, with t=0 at DRIVE
	// start. BOOT and SEED records are therefore NEGATIVE. See OPEN_QUESTIONS.md
	// entry "history t_ns time base".
	TNS int64 `json:"t_ns"`

	// Process is the client/process id. Pointer: 0 is a valid process id.
	Process *int `json:"process,omitempty"`

	Type HistoryType `json:"type"`

	// F is the operation name ("read", "write", "txn"). Empty is never valid
	// for an operation record, so a plain string with omitempty is exact.
	F string `json:"f,omitempty"`

	// Key is pointer-typed because "" is a legal key in a KV store and absent
	// must be distinguishable from empty.
	Key *string `json:"key,omitempty"`

	// Value is arbitrary JSON. RawMessage preserves absent vs null vs 0 vs
	// false exactly, and never lossily round-trips a float.
	Value json.RawMessage `json:"value,omitempty"`

	// OpID pairs an invoke with its completion. Pointer: 0 is a valid op id.
	OpID *uint64 `json:"op_id,omitempty"`

	Error string `json:"error,omitempty"`

	// Event, when non-empty, marks this as a marker record. Open string;
	// see EventPhase / EventFault / EventTOrigin / EventNote.
	Event string `json:"event,omitempty"`

	Phase Phase `json:"phase,omitempty"`
}

// RecordKind distinguishes the two shapes sharing HistoryEntry.
type RecordKind uint8

const (
	RecordOp RecordKind = iota + 1
	RecordMarker
)

func (e *HistoryEntry) RecordKind() RecordKind {
	if e.Event != "" {
		return RecordMarker
	}
	return RecordOp
}

// Validate enforces the union invariants that the type system cannot.
func (e *HistoryEntry) Validate() error {
	if !e.Type.Valid() {
		return fmt.Errorf("history: unknown type %q (want invoke|ok|fail|info)", e.Type)
	}
	if e.RecordKind() == RecordMarker {
		if e.Type != HistoryInfo {
			return fmt.Errorf("history: marker record (event=%q) must have type \"info\", got %q", e.Event, e.Type)
		}
		if e.Event == EventPhase && !e.Phase.Valid() {
			return fmt.Errorf("history: event=phase requires a valid phase, got %q", e.Phase)
		}
		return nil
	}
	if e.Process == nil {
		return errors.New("history: operation record requires \"process\"")
	}
	if e.OpID == nil {
		return errors.New("history: operation record requires \"op_id\"")
	}
	if e.F == "" {
		return errors.New("history: operation record requires \"f\"")
	}
	if e.Phase != "" {
		return errors.New("history: operation record must not carry \"phase\"")
	}
	return nil
}

// Definite reports whether this outcome is certain. Consistency checkers MUST
// treat !Definite completions as indeterminate.
func (e *HistoryEntry) Definite() bool {
	return e.RecordKind() == RecordOp && (e.Type == HistoryOK || e.Type == HistoryFail)
}

func (e *HistoryEntry) HasValue() bool     { return len(e.Value) > 0 }
func (e *HistoryEntry) IsNullValue() bool  { return string(e.Value) == "null" }
func (e *HistoryEntry) DecodeValue(v any) error { /* json.Unmarshal(e.Value, v) */ }

// --- constructors (the driver and recorder use these, never literals) ---

func Invoke(tns int64, process int, f, key string, value any, opID uint64) (*HistoryEntry, error)
func Complete(tns int64, process int, typ HistoryType, f, key string, value any, opID uint64, errMsg string) (*HistoryEntry, error)
func PhaseMarker(tns int64, p Phase) *HistoryEntry {
	return &HistoryEntry{TNS: tns, Type: HistoryInfo, Event: EventPhase, Phase: p}
}
func FaultMarker(tns int64, f FaultSpec, withdrawn bool) *HistoryEntry
// TOriginMarker records the wall-clock origin of the virtual clock so absolute
// timestamps remain recoverable without putting wall time in every line.
func TOriginMarker(unixNanos int64) *HistoryEntry

// --- streaming IO ---
// bufio.Reader (not Scanner): a single history line may exceed Scanner's 64 KiB
// token cap when `value` carries a large payload.

type HistoryReader struct {
	br   *bufio.Reader
	line int
}

func NewHistoryReader(r io.Reader) *HistoryReader { return &HistoryReader{br: bufio.NewReaderSize(r, 1<<16)} }

// Next returns the decoded entry and the raw line bytes. Callers that must
// preserve unknown driver-emitted fields byte-for-byte keep the raw line; the
// struct deliberately does not carry a catch-all map, which would cost an
// allocation per line on 500k-op soak histories.
func (r *HistoryReader) Next() (*HistoryEntry, []byte, error) { /* ReadBytes('\n'), skip blanks, decode, Validate */ }

type HistoryWriter struct{ w *bufio.Writer }

func NewHistoryWriter(w io.Writer) *HistoryWriter { return &HistoryWriter{w: bufio.NewWriter(w)} }

// Write validates then appends one compact line + "\n".
func (w *HistoryWriter) Write(e *HistoryEntry) error {
	if err := e.Validate(); err != nil {
		return err
	}
	b, err := marshalNoEscape(e)
	if err != nil {
		return err
	}
	if _, err := w.w.Write(b); err != nil {
		return err
	}
	return w.w.WriteByte('\n')
}

func (w *HistoryWriter) Flush() error { return w.w.Flush() }
```

---

## 14. verdict.go: `prothesis.verdict/v1` in full

### The presence rule (this is the part that breaks agents if wrong)

> **prothesis.verdict/v1 is a TOTAL contract. No field uses `omitempty`. Every key defined
> here is present in every emitted verdict.**
>
> - Empty lists serialize as `[]`, never `null`: `violations`, `causal_timeline`,
>   `surviving_faults`, `suspect.files`.
> - An empty `witness` serializes as `{}`, never `null`.
> - Absent scalars are `""` / `0`.
> - **Exactly three fields are ever `null`**, and each expresses a genuine "this did not
>   happen" that a zero value would misreport:
>   `minimal_repro`, `suspect`, `coverage_delta_vs_baseline`.
>
> `shrink` is deliberately **not** in the nullable set: it carries `attempted`, which exists
> precisely to express the not-attempted case, so it is always an object.

```go
package schema

import (
	"encoding/json"
	"fmt"
	"io"
)

type Verdict struct {
	Schema     string        `json:"schema"`
	RunID      string        `json:"run_id"`
	Profile    string        `json:"profile"`
	Commit     string        `json:"commit"` // "" when not a git checkout
	Verdict    VerdictResult `json:"verdict"`
	Budget     Budget        `json:"budget"`
	Violations []Violation   `json:"violations"` // [] never null

	Coverage      Coverage       `json:"coverage"`
	CoverageDelta *CoverageDelta `json:"coverage_delta_vs_baseline"` // null when no baseline
	OracleLock    OracleLock     `json:"oracle_lock"`
	Artifacts     string         `json:"artifacts"`
}

type Budget struct {
	WallS         int64 `json:"wall_s"`
	UsedS         int64 `json:"used_s"`
	WorldsPlanned int   `json:"worlds_planned"` // -1 when the profile is unbounded
	WorldsRun     int   `json:"worlds_run"`
}

type Violation struct {
	ID          string      `json:"id"` // "v1", "v2", ... stable within a run
	Oracle      string      `json:"oracle"`
	Class       OracleClass `json:"class"`
	Severity    Severity    `json:"severity"`
	Phase       Phase       `json:"phase"`
	FirstSeenMS int64       `json:"first_seen_ms"` // ms relative to DRIVE start
	Explanation string      `json:"explanation"`

	// Witness is an OPEN object, deliberately untyped: a consistency oracle
	// emits {"op_ids":[…],"key":"…"}, a resource oracle something else.
	// Typing it would foreclose oracle classes. Always present; {} when empty.
	Witness json.RawMessage `json:"witness"`

	MinimalRepro   *MinimalRepro   `json:"minimal_repro"` // null when not shrunk/committed
	Shrink         Shrink          `json:"shrink"`        // always an object
	CausalTimeline []TimelineEvent `json:"causal_timeline"`
	Suspect        *Suspect        `json:"suspect"` // null when blame was not computed
}

// WitnessCommon is a convenience view over Witness. Decoding is best-effort;
// oracles are free to emit other shapes.
type WitnessCommon struct {
	OpIDs []uint64 `json:"op_ids"`
	Key   *string  `json:"key"`
	Nodes []string `json:"nodes"`
}

func (v *Violation) DecodeWitness(out any) error { /* json.Unmarshal(v.Witness, out) */ }

type MinimalRepro struct {
	World string `json:"world"` // path under .prothesis/regressions/
	Cmd   string `json:"cmd"`   // "thesis replay <world>"
	// Reproduced is the honest determinism report required by the Tier B
	// contract: "3/3", "1/5". Never claim determinism we do not have.
	Reproduced string `json:"reproduced"`
}

// FormatReproduced / ParseReproduced keep the k/n string structured without
// inventing sibling integer fields.
func FormatReproduced(k, n int) string { return fmt.Sprintf("%d/%d", k, n) }
func ParseReproduced(s string) (k, n int, err error)

type Shrink struct {
	Attempted       bool          `json:"attempted"`
	FaultsBefore    int           `json:"faults_before"`
	FaultsAfter     int           `json:"faults_after"`
	OpsBefore       int           `json:"ops_before"`
	OpsAfter        int           `json:"ops_after"`
	SurvivingFaults FaultSchedule `json:"surviving_faults"` // canonical strings; [] never null
}

type TimelineEvent struct {
	TMS   int64  `json:"t_ms"`
	Event string `json:"event"` // open: fault | log | op | phase | oracle
	// Node is always present ("" when the event is not node-scoped). The
	// directive's sample omits it on non-log rows; emitting it uniformly keeps
	// the contract total, which is what a machine consumer needs.
	Node   string `json:"node"`
	Detail string `json:"detail"`
}

type Suspect struct {
	Files      []string `json:"files"`
	Confidence float64  `json:"confidence"` // 0..1
	Basis      string   `json:"basis"`
}

type Coverage struct {
	NewTemplates int `json:"new_templates"`
	CumTemplates int `json:"cum_templates"`
	NewStates    int `json:"new_states"`
	CumStates    int `json:"cum_states"`
}

type CoverageDelta struct {
	Templates int `json:"templates"`
	// States is ADDITIVE: the directive's sample shows only `templates`, but
	// `coverage` tracks state tuples too and CRUCIBLE 6.3 mechanism 3 requires
	// detecting a reduction in reachable STATE TUPLES as well as templates.
	// Without this field that signal is unrepresentable.
	States int    `json:"states"`
	Note   string `json:"note"`
}

type OracleLock struct {
	Status      LockStatus `json:"status"`
	ManifestSHA string     `json:"manifest_sha"` // "sha256:…"
}

// Normalize turns every nil slice into an empty slice and every empty witness
// into {}, so the emitted JSON satisfies the presence rule. It is the single
// choke point; WriteVerdict always calls it.
func (v *Verdict) Normalize() {
	v.Schema = VerdictSchema
	if v.Violations == nil {
		v.Violations = []Violation{}
	}
	for i := range v.Violations {
		x := &v.Violations[i]
		if len(x.Witness) == 0 {
			x.Witness = json.RawMessage("{}")
		}
		if x.CausalTimeline == nil {
			x.CausalTimeline = []TimelineEvent{}
		}
		if x.Shrink.SurvivingFaults == nil {
			x.Shrink.SurvivingFaults = FaultSchedule{}
		}
		if x.Suspect != nil && x.Suspect.Files == nil {
			x.Suspect.Files = []string{}
		}
		if x.Severity == "" {
			x.Severity = DefaultSeverity(x.Class)
		}
	}
}

// Validate checks the presence rule and every enum. Run in tests and before emit.
func (v *Verdict) Validate() error { /* ValidationErrors */ }

// ExitCode is the pure mapping. The verdict document deliberately does NOT
// carry an exit_code field: the directive does not define one, and a duplicated
// value could disagree with the process's actual exit status.
func (v *Verdict) ExitCode() ExitCode { return v.Verdict.ExitCode() }

// WriteVerdict is the ONLY supported way to emit a verdict: it normalizes,
// disables HTML escaping (so "n1<->n2" inside surviving_faults survives),
// indents deterministically, and terminates with a newline.
func WriteVerdict(w io.Writer, v *Verdict) error {
	v.Normalize()
	if err := v.Validate(); err != nil {
		return err
	}
	b, err := marshalNoEscape(v)
	if err != nil {
		return err
	}
	var out bytes.Buffer
	if err := json.Indent(&out, b, "", "  "); err != nil {
		return err
	}
	out.WriteByte('\n')
	_, err = w.Write(out.Bytes())
	return err
}
```

---

## 15. oracle.go: the two envelopes

```go
package schema

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// OracleInput is delivered on the oracle's stdin. All paths are ABSOLUTE
// because the oracle subprocess may run with a different working directory.
// Total contract: a path whose artifact was not produced is "" (present, empty),
// never omitted.
type OracleInput struct {
	Schema         string        `json:"schema"`
	HistoryPath    string        `json:"history_path"`
	FinalStatePath string        `json:"final_state_path"`
	TelemetryPath  string        `json:"telemetry_path"`
	WorldPath      string        `json:"world_path"`
	Phases         []PhaseWindow `json:"phases"` // [] never null
}

func NewOracleInput(historyPath, finalStatePath, telemetryPath, worldPath string, phases PhaseTimings) *OracleInput

func MarshalOracleInput(in *OracleInput) ([]byte, error) { /* normalize, marshalNoEscape */ }

// OracleOutput is read from the oracle's stdout.
type OracleOutput struct {
	Schema      string          `json:"schema"`
	Oracle      string          `json:"oracle"`
	Class       OracleClass     `json:"class"`
	ValidPhases []Phase         `json:"valid_phases"` // I5 phase-validity declaration
	Status      OracleStatus    `json:"status"`
	Witness     json.RawMessage `json:"witness"`
	Explanation string          `json:"explanation"`
}

// OracleExitCode is the oracle process exit space (directive 4.5). It is a
// SEPARATE space from ExitCode: 1 means VIOLATED here and FAIL there.
type OracleExitCode int

const (
	OracleExitOK           OracleExitCode = 0
	OracleExitViolated     OracleExitCode = 1
	OracleExitInconclusive OracleExitCode = 2
)

func (c OracleExitCode) Status() (OracleStatus, bool) {
	switch c {
	case OracleExitOK:
		return StatusOK, true
	case OracleExitViolated:
		return StatusViolated, true
	case OracleExitInconclusive:
		return StatusInconclusive, true
	}
	return StatusInconclusive, false
}

// DecodeOracleOutput reads a third-party oracle's stdout and reconciles it with
// the process exit code.
//
// Ingestion policy for THIS type only (third parties author these bytes):
//   - LENIENT about missing optional fields: no witness, no explanation, no
//     valid_phases is fine.
//   - STRICT about enum VALUES and about the schema id. A bad class or status
//     is not silently coerced.
//   - FAIL CLOSED: any parse failure yields inconclusive (exit 2), never ok.
//   - Reconciliation takes the WORSE of the exit code and the reported status
//     under ok < inconclusive < violated. We never adopt the weaker reading.
func DecodeOracleOutput(stdout []byte, exit OracleExitCode, oracleName string) (*OracleOutput, error) {
	fromExit, known := exit.Status()
	out := &OracleOutput{
		Schema: OracleOutputSchema,
		Oracle: oracleName,
		Class:  ClassSafety,
		Status: StatusInconclusive,
	}
	if !known {
		out.Explanation = fmt.Sprintf("oracle %s exited with unexpected code %d", oracleName, int(exit))
		return out, nil
	}
	trimmed := bytes.TrimSpace(stdout)
	if len(trimmed) == 0 {
		out.Status = fromExit.Worse(StatusInconclusive)
		out.Explanation = "oracle produced no stdout"
		return out, nil
	}
	var got OracleOutput
	if err := json.Unmarshal(trimmed, &got); err != nil {
		out.Status = fromExit.Worse(StatusInconclusive)
		out.Explanation = fmt.Sprintf("malformed oracle output: %v", err)
		return out, nil
	}
	if got.Schema != OracleOutputSchema {
		out.Status = fromExit.Worse(StatusInconclusive)
		out.Explanation = fmt.Sprintf("oracle output schema %q, want %q", got.Schema, OracleOutputSchema)
		return out, nil
	}
	if got.Oracle == "" {
		got.Oracle = oracleName
	}
	if len(got.Witness) == 0 {
		got.Witness = json.RawMessage("{}")
	}
	if got.ValidPhases == nil {
		got.ValidPhases = []Phase{}
	}
	// fail closed: take the worse reading
	if got.Status != fromExit {
		got.Explanation = fmt.Sprintf("%s [reconciled: exit code says %s, stdout says %s]",
			got.Explanation, fromExit, got.Status)
	}
	got.Status = got.Status.Worse(fromExit)
	return &got, nil
}

// ToViolation lifts a violated oracle result into a verdict Violation.
// Severity is assigned HERE, from the class: prothesis.oracle_output/v1 has no
// severity field, so an oracle can never grade its own finding.
func (o *OracleOutput) ToViolation(id string, phase Phase, firstSeenMS int64) Violation {
	return Violation{
		ID:             id,
		Oracle:         o.Oracle,
		Class:          o.Class,
		Severity:       DefaultSeverity(o.Class),
		Phase:          phase,
		FirstSeenMS:    firstSeenMS,
		Explanation:    o.Explanation,
		Witness:        o.Witness,
		Shrink:         Shrink{SurvivingFaults: FaultSchedule{}},
		CausalTimeline: []TimelineEvent{},
	}
}
```

---

## 16. errors.go, ids.go, ptr.go

```go
// errors.go
type ValidationError struct {
	Path string // "harness.nodes[2].id"
	Msg  string
}

func (e ValidationError) Error() string { return e.Path + ": " + e.Msg }

type ValidationErrors []ValidationError

func (e ValidationErrors) Error() string  { /* one per line */ }
func (e *ValidationErrors) Add(path, format string, a ...any)
func (e ValidationErrors) OrNil() error   { if len(e) == 0 { return nil }; return e }
```

```go
// ids.go
// NewRunID renders r_YYYY_MM_DD_<4 lowercase hex>, matching r_2026_09_03_a41f.
func NewRunID(t time.Time, nonce uint16) string
func ParseRunID(s string) (time.Time, uint16, error)

// WorldFilename renders w_<n hex chars of the hash>.thesis (n=4 by default,
// extended by 4 on collision), matching w_a41f.thesis.
func WorldFilename(hash string, n int) string
func ShortHash(hash string, n int) string
```

```go
// ptr.go — the history schema needs pointers for zero-valued-but-present fields.
func IntPtr(v int) *int
func U64Ptr(v uint64) *uint64
func StrPtr(v string) *string
```

---

## 17. Tests and golden data

`pkg/schema/testdata/` holds the directive's samples **verbatim**:

| file | source |
|---|---|
| `prothesis.golden.yaml` | directive 4.2, copied byte-for-byte |
| `history.golden.jsonl` | directive 4.4, the five lines |
| `oracle_input.golden.json` | directive 4.5 input |
| `oracle_output.golden.json` | directive 4.5 output |
| `verdict.golden.json` | directive 4.6, complete |
| `world.golden.thesis` | generated once, then frozen |

Test set:

- `TestGoldenConfigDecodes`: decode `prothesis.golden.yaml` strictly, assert every field
  (including `retain_failing == RetainAll()`, `retain_passing == RetainN(3)`,
  `profiles["soak"].Worlds == -1`, `profiles["soak"].Search == true`,
  `profiles["smoke"].Budget == 90s`).
- `TestGoldenVerdictDecodesAndReEmits`: decode the spec verdict, re-emit, compare
  **canonicalized** forms (the spec's key order and whitespace are not normative).
- `TestVerdictPresenceRule`: build a zero `Verdict`, `WriteVerdict`, then assert the emitted
  JSON contains `"violations":[]`, that `minimal_repro`/`suspect`/`coverage_delta_vs_baseline`
  are `null`, and that **no other** key is null and no defined key is missing. This is the test
  that protects agent consumers.
- `TestFaultRoundTrip` (table + property): for every kind in `Kinds`, parse the canonical
  string, re-render, re-parse, assert stability. Includes `net.partition(n1<->n2)@0..1`.
- `TestNoHTMLEscaping`: assert `MarshalWorld` and `WriteVerdict` output contains `<->`
  literally and never `<`.
- `TestWorldByteIdenticalRoundTrip`: the Phase 0 DoD (b) test, including a world with an
  **empty** fault schedule (proves `null` vs `[]` normalization).
- `TestWorldHashStability`: the golden world's `world_hash` is a frozen constant in the test;
  changing the canonicalizer or the fault printer must fail loudly.
- `TestWorldHashIgnoresProvenance`: mutate every `provenance` field, assert the hash is
  unchanged.
- `TestHistoryUnion`: `info`+`event` is a marker; `info`+`op_id` is an indeterminate op; an
  op record missing `op_id` fails; `"value":0`, `"value":false`, `"value":null` and an absent
  `value` are four distinguishable states; `"key":""` survives.
- `TestOracleReconciliation`: exit 1 + `status:"ok"` yields `violated`; garbage stdout + exit 0
  yields `inconclusive`, never `ok`.
- `TestExitCodeBijection`: round-trip all six.

---

## 18. What this design deliberately does **not** do (Phase discipline)

- No `.prothesis/lock` **file** format. Phase 0 defines only the verdict's `oracle_lock`
  object and `Config.LockPayload()` (the hashable projection), so Phase 3 cannot quietly
  change what the lock covers.
- No constraint grammar. `perturber.constraints` stays `[]string` (see OPEN_QUESTIONS).
- No coverage computation, no corpus, no energy function: only the `Coverage` /
  `CoverageDelta` result types the verdict needs.
- No fault *implementations*. Only `Kinds`, the AST, and `Requires []Capability`, which is
  what lets `thesis doctor` say "proc.pause is unavailable: the process backend on windows
  provides no signal.stop" without any Phase 2 code existing.
- No `Extra`/catch-all on `HistoryEntry`: byte-fidelity for unknown driver fields is served by
  returning the raw line from `HistoryReader.Next()`, which costs nothing per line.
