# OPEN QUESTIONS

Conflicts and requirements that are not implementable as written, logged rather than silently
relaxed (base directive, Core Implementation Rule 4).

Classification:
- **BLOCKING**: work cannot proceed correctly until a human decides.
- **RESOLVABLE-WITH-DECISION**: a defensible resolution exists and has been taken; recorded
  here because it departs from, or chooses between, the literal text of the spec.
- **ACCEPT-AND-DOCUMENT**: the requirement stands but cannot be fully met; the gap is stated
  honestly rather than papered over.

The governing rule from the specification, which applies to every entry below:
> "never make the gate weaker to make it pass. If the gate is wrong, that is an
> OPEN_QUESTIONS.md entry and a human decision."

---

## OQ-001: Two documents, both claiming frozen schemas, with incompatible names

**Classification:** RESOLVED; user-confirmed.

**Conflict.** The base directive states: *"All schemas (`prothesis.yaml`,
`prothesis.verdict/v1`, `prothesis.oracle_input/v1`, `prothesis.oracle_output/v1`, history log
JSONL, exit codes, and fault grammar) are normative. Do not invent, alter, or improvise field
names or semantics."* A second document supplied at the same time states the same thing about
`crucible.yaml`, `crucible.verdict/v1`, `crucible.oracle_input/v1`,
`crucible.oracle_output/v1`, and the `crux` binary. Both cannot be obeyed literally.

**Evidence.** Both `.md` files on disk are byte-identical (SHA-256 `1CE1C0BB…78F0`) and both
contain the PRO-THESIS text. The CRUCIBLE variant exists only in chat.

**Resolution.** PRO-THESIS naming wins; CRUCIBLE contributes normative *content* under
PRO-THESIS *names*. See D-001. Confirmed with the user before implementation began.

**Residual risk.** If the CRUCIBLE document is later declared authoritative, the rename is
mechanical (identifiers and file extensions) but touches every schema string, every artifact
path, and any `.thesis` files already committed to the regression corpus.

---

## OQ-002: `process` backend is named as a v1 backend but is not implementable on Windows

**Classification:** ACCEPT-AND-DOCUMENT.

**Conflict.** The directive's reference stack specifies *"Backends for v1: `compose` (Docker
Compose via Docker SDK/CLI) and `process` (local process supervision)."* The required fault
primitives are specified as SIGSTOP/SIGCONT for `proc.pause`, iptables for `net.partition`, and
tc/netem for the `net.*` shaping family. None exist on a Windows host.

**Consequence.** On this machine, a `process` backend could supervise processes but could not
inject the faults that make the tool meaningful, including the two faults that trigger the
mandatory fixture bug. A `process` backend that silently supports only `proc.kill` and
`proc.restart` would be worse than none, because it would report PASS on a world whose faults
never fired.

**Resolution taken.** `harness.backend: process` is accepted by the config parser, and the
backend returns an explicit unsupported-platform error on Windows rather than degrading. The
`Backend` interface accommodates it for Linux hosts and for future work.

**Needs a human decision if** the `process` backend is required to work on this machine.

---

## OQ-003: Addendum A's adaptive Saboteur may be incompatible with invariant I2

**Classification:** RESOLVED. The world file carries a planned/realized fault schedule split, shipped in Phase 0 and verified by the byte-identity suite. See DECISIONS.md D-012.

**Conflict.** Invariant I2 requires that *"Every execution is defined by a serializable world
tuple: `(seed, topology_variant, driver_profile, fault_schedule, phase_timings)`. Any violation
must serialize to a self-contained `.thesis` file that reproduces the issue."* Phase 5's
`ddmin` shrinking and `thesis replay` are built directly on that guarantee.

Addendum A describes an agent that selects faults *during* a world, in reaction to live
telemetry:
- A.4: *"A `REINFORCE_ACCELERATING` classification on any metric triggers immediate escalation
  to Tier 2."*
- A.5: *"Root node: The current observed distributed state at the moment REINFORCE was
  detected, plus the active probe fault."* and *"Child nodes: Each child represents an
  additional fault action from the grammar, applied at the current virtual clock time."*
- A.1: *"The Saboteur observes the system's live telemetry and selects faults that exploit
  observed weakness."*

If the fault schedule is a function of live, real-time telemetry, it is not known before
execution and is not a function of the seed alone. Under Tier B the spec itself concedes replay
is probabilistic (*"Replays reproduce with high probability, not certainty"*), so the telemetry
that drove the decisions will not recur identically, and the same seed will not regenerate the
same schedule.

Addendum A.8 nonetheless asserts *"All Saboteur decisions (fault selection, tree expansion) must
be deterministic given the same PRNG seed for Tier B reproducibility."* That is satisfiable for
the *policy's random choices* but not for choices *conditioned on non-reproducible observations*.

**Why this blocks Phase 0.** Phase 0 freezes the `.thesis` format and must satisfy a
byte-identical round-trip. If the format cannot express a *realized* schedule (what was actually
injected, at what virtual-clock time) distinctly from a *planned* one, Phase 4 will require
breaking a frozen format.

**Candidate resolution under evaluation.** The Perturber records the realized schedule; the
`.thesis` file stores it; `thesis replay` executes the fixed realized schedule rather than
re-running the adaptive policy. This preserves I2's reproduction guarantee and keeps `ddmin`
well-defined. It needs verification that a schedule originally timed to live events still
reproduces when replayed at fixed offsets.

**Alternative reading.** A.5 may intend each MCTS node to be a complete world with a
fully pre-planned schedule, with "current virtual clock time" meaning the offset at which the
new fault is *scheduled* in the next world rather than injected mid-flight. This reading is
fully compatible with I2. Both readings are being argued before the format is frozen.

---

## OQ-004: Addendum A.5 early termination contradicts invariant I5 and the run lifecycle

**Classification:** RESOLVED. HEAL and TEARDOWN run on every path including violation and cancellation (D-022); early termination is admissible only for prefix-closed oracle classes (crash, safety), so consistency and convergence still run to ASSERT.

**Conflict.** A.5 specifies: *"Early termination: If an oracle violation is detected during
`DRIVE` or `PERTURB`, terminate the rollout immediately and backpropagate the full violation
reward."*

But §4.1 places oracle evaluation in `ASSERT`, after `HEAL` and `QUIESCE`; and I5 states
*"Checking consistency during an active network partition generates false positives. Every
oracle declares the specific lifecycle phases in which it is valid."* The CRUCIBLE content is
more explicit still: *"A convergence oracle is valid only in `ASSERT` after a completed
`QUIESCE`. This distinction is what separates a real finding from noise."*

**The bite.** Consistency violations carry the highest reward in A.6 (100, ×1.5 for high
severity) and are the class the tool exists to find: the fixture's stale read is one. They are
also precisely the class that cannot be soundly evaluated during `DRIVE`/`PERTURB`. As written,
early termination can only ever fire on the *low-value* oracle classes (crash, resource),
biasing the Saboteur's search away from consistency bugs.

**Direction.** Early termination should be restricted to oracle classes whose `valid_phases`
include the phase in which the violation is observed: i.e. genuine invariant oracles such as
`no_crash` and `no_panic_log`. Consistency and convergence rollouts must run to `ASSERT`.

---

## OQ-005: `search` is a boolean in the base spec and a mapping in Addendum A.10

**Classification:** RESOLVED. Not a true collision: YAML resolves the two by path (`profiles.<name>.search` boolean versus root `search` mapping), and both are implemented with distinct Go types and pinned by `TestSearchBlockTwoLevels`. The semantic split is settled in D-028 item 7: the profile boolean decides WHETHER a search runs, the top-level block decides WHICH.

**Conflict.** The base configuration schema defines `search` as a boolean inside a profile:

```yaml
profiles:
  soak: { budget: 8h, worlds: -1, driver_profile: soak, search: true }
```

Addendum A.10 defines `search` as a top-level mapping:

```yaml
search:
  strategy: saboteur
  exploration_constant: 1.41
```

Two different keys named `search`, at two nesting levels, with different types. Both documents
declare their schema normative.

**Assessment.** Not a true collision (YAML resolves them by path (`profiles.<name>.search`
versus root `search`)) but it is a real trap for anyone reading or writing the config, and the
Go type for each must be distinct. Flagged so the naming is a deliberate choice rather than an
accident, and so the scaffolded `prothesis.yaml` documents both.

---

## OQ-006: Internal inconsistencies in Addendum A

**Classification:** RESOLVED; readings taken. (1) A.2's utility box omits `safety` and `convergence` that A.6's Go includes: A.6 governs, being labelled normative and being code; A.2 is an abridged illustration. See also D-028 item 2, which adds the missing `default`. (2) Telemetry sampling is 500 ms by default (`observe.sample_interval_ms`) with 200 ms as the probe-specific trajectory rate: the two figures describe different things. (3) A.3's boolean DAMPEN/REINFORCE criteria and A.4's trend/acceleration `ClassifySignal` are two schemes sharing names; A.4's is the classifier, A.3's four criteria are the EVIDENCE it consumes, so `REINFORCE_ACCELERATING` (required by A.9 #1) is reachable from a probe. (4) A role change within 1 s of pausing a Raft leader is a CONSTANT, not a signal (a correct implementation elects too) so it is retained as a trigger for closer inspection but carries no weight on its own.

1. **Utility table incomplete.** A.2's diagram lists rewards for consistency (100), crash (80),
   resource (40) and liveness (20) only. A.6's normative Go adds `safety` (60) and
   `convergence` (20). A.6 is labelled normative and is code, so A.6 governs; A.2's box is an
   abridged illustration.
2. **Telemetry sampling interval stated twice, differently.** A.3 specifies telemetry *"sampled
   every 200ms during `DRIVE`"* for probe worlds; A.4 specifies *"Sampled every 500ms"* for
   container/process metrics, and A.10 defaults `observe.sample_interval_ms` to 500. Reading
   taken: 500 ms is the configurable default for general telemetry; 200 ms is the probe-specific
   trajectory rate. To be confirmed.
3. **Two classification schemes share names.** A.3 classifies a probe as DAMPEN or REINFORCE
   using boolean event criteria (queue depth monotonic over ≥3 samples, retry count >2×
   baseline, a role change within 1 s, error rate rising after withdrawal). A.4's
   `ClassifySignal` returns `DAMPEN` / `REINFORCE_LINEAR` / `REINFORCE_ACCELERATING` from trend
   and acceleration of a regression over a metric series. A.9's definition of done requires the
   *`REINFORCE_ACCELERATING`* label from a *probe*, which A.3's scheme cannot produce. The two
   schemes must be unified.
4. **A role change is a constant, not a signal, for this fixture.** A.3 counts *"a role change
   (leader election, failover) occurred within 1 second of fault injection"* as evidence of
   REINFORCE. Pausing a Raft leader always causes an election, in a correct implementation as
   much as a buggy one. As a discriminator between healthy and unhealthy systems this criterion
   carries no information for the mandatory fixture.

---

## OQ-007: Several definitions of done may not be reachable as written

**Classification:** RESOLVED, item by item. (1) Phase 4 timing: quantified in OQ-013, closed by 4-way parallelism (D-022). (2) Statistical claim: pre-registered one-sided Fisher's exact (D-029); the original median-based claim was untestable because time-to-first-violation is right-censored. (3) Phase 5 cost: shrinking is budgeted and emits a PARTIAL shrink honestly (D-029). (4) `clock.skew`: **measured**; libfaketime cannot work on Go binaries, Linux time namespaces can (D-024). (5) `steady.sh`: runs in a helper container, sidestepping the broken host shell (D-010). (6) `suspect.files`: emitted as null until there is a defensible basis, never a fabricated confidence (D-030).

Flagged early because they determine whether a phase can ever be declared complete, and the
directive requires each phase to satisfy its definition of done before the next begins.

1. **Phase 4 timing (A.9).** The Saboteur must find the stale-read bug *"within 10 minutes"*
   from a cold start, with the first 20% of budget spent on the Tier 1 probe sweep. A.5 states
   each rollout costs 25–60 s of real execution. The arithmetic on the number of surviving MCTS
   rollouts, and whether that number supports a meaningful UCT search, is being computed against
   this machine's actual per-world compose cost.
2. **Phase 4 statistical claim (A.9 #4).** *"Across 10 independent trials … median
   time-to-first-violation is ≤ 50% of … uniform random fault injection."* Ten trials is a small
   sample for a median ratio claim, and if the fixture bug is easy enough for random search to
   hit quickly, the comparison may be unfalsifiable in either direction. The test, effect size,
   and what constitutes failure need stating.
3. **Phase 5 cost.** Shrinking re-executes worlds: `ddmin` over 14 faults, then over 20,000
   ops, then binary search on each surviving window, then 3× confirmation; each step a full
   world execution. Total wall clock is being estimated.
4. **`clock.skew` implementability.** The spec proposes libfaketime via LD_PRELOAD. Whether that
   intercepts time in statically-linked Go binaries, which resolve `clock_gettime` through the
   vDSO rather than libc, determines whether `clock.skew` and `clock.jump` are implementable at
   all in Phase 2, and Addendum A.7's Rung 5 depends entirely on them.
5. **`steady_state` probe is a shell script.** The config specifies
   `probe: "thesis-helpers/steady.sh"`. The host is Windows and Git Bash is non-functional on
   this machine.
6. **`suspect.files` with a confidence score.** The verdict schema requires suspected source
   files and a confidence value, on the basis of *"log-template locality + blame over shrunk
   timeline window"*. This is a research-grade capability. The minimum honest implementation
   needs defining, so the field is not populated with a fabricated number.

---

## OQ-003 RESOLVED: the adaptive/replay conflict is real, but it is not the Saboteur's fault

**Update.** The audit resolved OQ-003 in an unexpected direction: **the base specification already
breaks a naive reading of I2, with no Saboteur involved.**

`role:leader`, `minority(kv)` and `kv:*` are normative fault targets, and they bind to concrete
nodes *at injection time* from live cluster state. Raft leadership moves at runtime. So a world
recording only `proc.pause(role:leader)` does not reproduce; on replay, a different node may be
leader. `prothesis.oracle_input/v1` compounds this by carrying *measured* phase boundaries.

The planned/realized split is therefore required for **Phase 2**, regardless of Addendum A.
Adopted into the Phase 0 world format (see `docs/protocol/PHASE0_BUILD_BRIEF.md` §2): `fault_schedule`
carries `planned` and `realized`; `realized` records what was actually injected against which
concrete node at which virtual-clock time; `thesis replay` executes `realized` when present.

Addendum A.5 was also found to be **internally self-contradictory**: its prose (A.1, A.4, and the
A.5 root-node definition) describes online intra-world adaptation, but every piece of its
normative machinery (UCT `visits`, the ≥3-visit prune, "each rollout executes a real world",
A.8's determinism claim) requires per-world *offline* tree expansion. A live distributed state
can never be revisited without simulation, and I1 forbids simulation. Only the offline reading is
implementable, and it is compatible with I2.

---

## OQ-004 UPDATED: early termination has a worse problem than the I5 phase conflict

The I5 conflict stands as recorded. But a sharper defect was found in the same sentence.

A.5's *"terminate the rollout immediately"* skips `HEAL`. §4.3 states the Critical Guarantee:
*"Every fault must implement a safe `withdraw()` mechanism. `HEAL` must verify that no residual
network rules or stopped processes persist."* A rollout that terminates early leaves live
`iptables` rules and `SIGSTOP`ped containers behind, poisoning every subsequent rollout, and it
is triggered, perversely, **by success**. The more effective the Saboteur, the more corrupted its
own search becomes.

**Resolution.** Early termination may skip only `DRIVE`/`QUIESCE` *waiting*. `HEAL` and
`TEARDOWN` are mandatory on every path, including violation, budget expiry, and error. Further,
early termination is admissible only for oracle classes that are **prefix-closed**: a violation
observed on a prefix of the history remains a violation on the whole history. That is `crash` and
`safety`. `consistency` and `convergence` remain `ASSERT`-only in v1.

Two related corrections: `violation.phase` and `oracle.valid_phases` are different fields with
different meanings and the frozen examples invite conflating them. And the naive form of "early
termination biases the utility function via the duration penalty" is **false**: the real bias is
evidence truncation, not `-0.1 × duration`.

---

## OQ-008: `t_ns`: the field name and the specification's own example values disagree

**Classification:** RESOLVED.

§4.4 names the field `t_ns` and shows values like `1725300000000000`. That value is ~1.7×10¹⁵.
As Unix epoch **nanoseconds** it is 20 days after 1970. As Unix epoch **microseconds** it is
September 2024: evidently the intended date. Epoch nanoseconds for that date would be ~1.7×10¹⁸,
three orders of magnitude larger. **The field name and the example values cannot both be right.**

**Resolution.** The field name governs; the example values are a specification error. Drivers emit
true Unix epoch nanoseconds. The harness records `drive_origin_wall_ns` once at DRIVE start, which
makes the conversion to `prothesis.oracle_input/v1`'s `start_ms`/`end_ms` exact. That anchor is
what makes I5 phase-aware assertion evaluable at all.

Rejected: run-relative nanoseconds. The driver is an external, unmodified process and cannot know
the harness's virtual-clock origin.

---

## OQ-009: `netem`'s randomness is unseedable, so some network faults cannot replay bit-exactly

**Classification:** RESOLVED; THE PREMISE WAS FALSE. Measured on this kernel: netem accepts an explicit `seed` and stores it faithfully (asked 11111 -> reports 11111). Seeding it from the recorder's PRNG stream makes net.loss/reorder/duplicate replayable. See DECISIONS.md D-025.

`net.loss`, `net.reorder` and `net.duplicate` are implemented by `tc netem`, whose randomness is
in-kernel and takes no seed. A world replayed with the same root seed will not reproduce the same
per-packet drops. This is in tension with I2's "every failure is a seed".

**Accepted, not concealed.** This is precisely the Tier B property the specification already
concedes: *"Replays reproduce with high probability, not certainty… The verdict must report
`reproduced: "3/3"` or `"1/5"` and never claim determinism it does not have."* `net.partition` and
`net.latency` without jitter are deterministic and unaffected.

A per-edge ambassador proxy would give seeded per-packet control, and remains available as a
Phase 2 option if the reproduction rate on stochastic network faults proves too low to be useful.
It was not adopted as the default because it requires modifying the target's topology, which
contradicts the requirement to work on unmodified systems.

---

## OQ-010: the Phase 2 reference schedule cannot trigger the fixture bug

**Classification:** RESOLVED. The schedule is amended to a 2.8s pause; see DECISIONS.md D-019.
**Original classification:** BLOCKING for the Phase 2 definition of done.

§6 Phase 2's definition of done requires that this schedule trigger the stale read:

```
proc.pause(role:leader)@8200..15100
net.partition(minority(kv))@8400..14900
```

That is a **6.9-second pause** against §5's **5-second lease**. `proc.pause` is specified as
`SIGSTOP`, and `CLOCK_MONOTONIC` continues advancing while a process is stopped. The lease
therefore expires *during* the pause, and the resumed ex-leader correctly refuses the local read.
**The specification's own reference schedule is incapable of producing the anomaly it is cited to
produce.** §4.6's marquee `causal_timeline` example has the same defect.

**Recommended correction.** The pause must be shorter than the lease:

```
proc.pause(role:leader)@8200..11000          # 2.8 s pause  <  5 s lease
net.partition(minority(kv))@8400..10900
```

**Explicitly rejected workaround.** A lease expressed in raft *ticks* would fire under the
spec's literal window, because a `SIGSTOP`ped process's `time.Ticker` collapses missed ticks. It
was rejected: it deviates from §5's "5-second lease", it makes the anomaly independent of pause
duration (the opposite of the real failure mode), and it is silently deleted by any future rewrite
of the tick loop into a monotonic-delta catch-up loop. Weakening the fixture to fit a defective
schedule is the failure mode I6 exists to prevent.

**Needs a human decision** on whether the Phase 2 definition of done is amended to the corrected
window.

---

## OQ-011: invariant I2's world tuple omits the identity of the system under test

**Classification:** RESOLVED; resolved additively (D-012 `sut`).

I2 defines a world as `(seed, topology_variant, driver_profile, fault_schedule, phase_timings)`.
Nothing identifies *which build* of the system under test ran. Consequences:
- a regression world replayed after a patch silently tests a different program
- `thesis bisect --good SHA --bad SHA` cannot be sound
- `reproduced: "3/3"` is a claim about an unknown binary

**Resolution.** The `.thesis` file carries an additive `sut` object recording resolved image
digests, included in `world_hash`. Marked `// ADDITIVE`. The alternative (leaving replay unsound)
is not acceptable for the artifact the whole tool is built to produce.

---

## OQ-012: `driver.profiles` never reaches the driver through the frozen command template

**Classification:** RESOLVED for the format. `{plan_path}` and `PROTHESIS_PLAN_PATH` are reserved; see DECISIONS.md D-021. Driver-side substitution lands in Phase 1, loadgen `--plan` in Phase 5.

§4.2 defines rich driver profiles (`clients`, `ops`, `mix`) and a command template that
interpolates only three placeholders:

```
cmd: "./bin/loadgen --history {history_path} --seed {seed} --profile {profile}"
```

`{profile}` passes the profile **name**, not its contents. So a driver has no way to receive
`clients: 16, ops: 20000, mix: {read: 0.4, write: 0.4, txn: 0.2}` unless it independently parses
`prothesis.yaml`, which a portable third-party driver cannot be assumed to do.

The same gap makes **Phase 5's operation shrinking unimplementable**: `ddmin` over the workload
requires handing the driver a *reduced operation list*, and no placeholder can express one.

**Recommendation.** Reserve an additive `{plan_path}` placeholder now (pointing at a file
containing the resolved profile, and later a reduced op plan) and document that op-shrinking is
available only for cooperating drivers. The placeholder vocabulary is nowhere declared frozen,
unlike the schema field names.

---

## OQ-013: Addendum A's stated search economics do not close

**Classification:** ACCEPT-AND-DOCUMENT; quantified, supersedes OQ-007.1.

Measured on this machine rather than estimated. A probe world costs **~19.4 s**, not A.3's
"~5–15s": the 5-second lease alone forces a ~6 s `QUIESCE` before convergence can be asserted.
A.3's sweep is 7 allowed fault kinds × 3 kv nodes + 1 control = **22 worlds = 427 s serial**,
which is **356 % of its own 120 s probe budget** and 71 % of the entire 10-minute definition of
done. Honouring the 120 s cap serially yields 6 of 21 probes, so leader `proc.pause` is sampled
only ~29 % of the time, and A.9's first success criterion requires it specifically.

The remaining ~480 s buys **~16 serial rollouts** against a root branching factor around 249.
At that ratio UCT is **provably equivalent to ladder-ordered enumeration**: the exploration term
is infinite at zero visits, so unvisited children are always selected first, no node is ever
visited twice, backpropagation never influences a decision, and A.5's own pruning rule ("visited
≥3 times") is unreachable. Separating a violation-bearing arm from a dead one at Tier B's
p ≈ 0.2 needs ~15 samples per arm; we would have one.

**This does not sink the design: it relocates its value.** The escalation ladder (A.7) and early
termination carry the search; the tree is a tie-breaker. That should be stated in `DECISIONS.md`
rather than implied otherwise.

**Mitigation, measured:** 8 concurrent compose projects give a 3.8× speedup for +3–5 % timing
distortion. At 4-way parallelism the sweep fits in ~116 s and buys ~62 rollouts, putting projected
time-to-first-violation around 135 s against the 600 s target. **Hard ceiling: 24 free Docker
bridge networks**, and leaked projects consume them permanently, so `thesis down` reliability is
a search-scalability concern, not merely hygiene.

---

## OQ-014: further defects in Addendum A's normative machinery

**Classification:** RESOLVED except item 2. Chosen readings for all eight defects are recorded in DECISIONS.md D-028; none is implemented yet, but Phase 4 is no longer built on ambiguity. **Item 2 (A.6's parsimony claim) still needs a human ruling**: see below. All are Phase 4 concerns; none blocks Phase 2.

1. **A.5's UCT formula is wrong.** It gives `UCT(node) = (U_avg / visits) + C × sqrt(...)` while
   defining `U_avg` as *"Average utility … across all rollouts through this node."* Dividing an
   average by `visits` again is not UCT and inverts the intent: it drives the exploitation term
   toward zero as a node proves itself. The standard form is `U_avg + C × sqrt(ln(parent.visits) /
   visits)`.
2. **A.6's parsimony claim is false as written.** Its "Key property" paragraph argues the utility
   function always prefers the minimal path. It does not: one novel log template is worth 10 and a
   fault costs 3, so a 12-fault world with 5 novel templates scores 152 against the minimal
   world's 141. Novelty outweighs parsimony at a ratio of 3.33 faults per template.
3. **A.6's class switch has no `default` branch**, so `differential` and `metamorphic` violations
   (both normative oracle classes) score exactly 0.0 and are invisible to the search.
4. **A.7 Rung 3 requires an asymmetric partition** the frozen `TARGET` grammar cannot express.
5. **A.6 requires a `severity` field that `prothesis.oracle_output/v1` does not define**, and
   severity multiplies the entire class hierarchy.
6. **A.3's sweep drops the no-fault control world** that every DAMPEN/REINFORCE baseline
   comparison depends on.
7. **A.10's `strategy: saboteur` default would make `thesis gate` run an adaptive MCTS search.**
   §4.2 enables search on the `soak` profile only. The gate must remain a fixed-budget
   regression check.
8. **The top-level `search:` block must be an input to `.prothesis/lock`**, or it is the easiest
   gate-weakening vector in the system; `probe_budget_pct`, `max_mcts_depth` and every utility
   weight are all reachable without touching an oracle.

---

## OQ-015: two anti-gaming holes found in the proposed lock mechanism

**Classification:** RESOLVED in the Phase 0 brief, before the lock is built in Phase 3.

1. **Deleting a profile would be invisible to the lock.** `yaml.v3` merges map *keys* against
   pre-seeded defaults, so a user who removes `soak` from `prothesis.yaml` gets a fully populated
   `soak` back from compiled-in defaults, and the config digest never moves. CRUCIBLE §E.2 makes
   profile budgets a lock change, so this holes the rule directly. Resolution: never pre-seed
   map-valued config sections; apply per-entry defaults explicitly after decoding, and validate
   every profile. Also note `KnownFields(true)` does **not** protect map keys: a typo'd profile
   name decodes without error and silently creates a phantom profile.
2. **Digesting the *resolved* config would turn every binary upgrade into an exit-4 incident.**
   If the digest covers compiled-in defaults, shipping a release that adjusts any default halts
   every downstream project pending human sign-off, and code 4 is the one the agent loop must
   *never* auto-resolve. Resolution: digest the **user-supplied** config, normalised but with
   defaults **not** filled in, and keep the digest out of `world_hash`.

---
## OQ-016: `kv:*` and `minority(kv)` cannot resolve under one-compose-service-per-node

**Classification:** RESOLVED. `service` is the logical group and an additive `compose_service` carries the physical one; see DECISIONS.md D-020. Verified end to end: the fixture uses the directive's own `kv:*` and all three probes resolve and pass.

**Raised by:** the Phase 0 integration pass. Predicted by `docs/design/04-fixture.verify.md` finding
P6, which required an `OPEN_QUESTIONS.md` entry; the fixture implementation reproduced the defect
and the entry was never written. This is that entry.

### The conflict

Base directive §4.2 declares the reference topology as three nodes sharing one service:

```yaml
nodes:
  - id: n1
    service: kv
  - id: n2
    service: kv
  - id: n3
    service: kv
  - id: pg
    service: postgres
health:
  - node: "kv:*"
```

§4.3 then defines `TARGET` as "node (`n1`), wildcards (`kv:*`), roles (`role:leader`), edges
(`n1<->n2`), quorums (`minority(kv)`, `majority(kv)`)". Under §4.2, the token before the colon and
the argument of `minority(...)` are both an **exact service name**, and `kv` names three nodes.

`docs/protocol/PHASE0_BUILD_BRIEF.md` D-F requires **one compose service per node**, because container IPs
are not routable from the Windows host (measured) and every node therefore needs its own published
`127.0.0.1` client port. Docker Compose service names are unique, so three nodes cannot share
`service: kv` without `deploy.replicas`/scaling, and scaled replicas cannot each publish a distinct
fixed host port, which is the thing D-F exists to guarantee.

So §4.2's sample topology is **not expressible on the compose backend**, and with it the wildcard
and quorum target vocabulary that §4.3 freezes.

### What is on disk today

- `pkg/schema` resolves `kv:*` and `minority(kv)` by **exact** service match
  (`Config.targetResolves`), which is the directive-faithful reading. It is unchanged.
- `testdata/kvfixture` declares `service: kv-n1 / kv-n2 / kv-n3`, so the wildcard matches nothing.
  Its `harness.health` block used `node: "kv:*"`, which made the fixture's own `prothesis.yaml`
  fail `Config.Validate()`; the project's reference configuration was invalid against the
  project's own schema. Phase 0 changed those three probes to plain node targets (`kv-n1`,
  `kv-n2`, `kv-n3`), a normative target form that needs no reinterpretation.
- `internal/integration.TestFixtureConfigDecodesAndValidates` carries a canary that fails if any
  node starts declaring `service: kv`, so this question cannot be settled silently.

Nothing else in the tree depends on the `kv` group: `perturber.constraints` is free text and
`perturber.allow` lists kinds, not targets. Phase 0 is therefore unblocked. **Phase 2 is not**;
the directive's own Phase 2 definition of done is the schedule
`proc.pause(role:leader)@8200..15100` overlapping `net.partition(minority(kv))@8400..14900`, and
`minority(kv)` does not resolve against this fixture.

### The three candidate resolutions

1. **Prefix glob.** Reinterpret the token before the colon as a glob, so `kv:*` matches `kv-n1`.
   *Rejected as a silent change.* It rewrites frozen grammar in a parenthetical, and it silently
   redefines `minority(kv)`, `majority(kv)` and the frozen constraint string
   `"never partition more than minority of kv"`. If it is adopted it must be adopted explicitly,
   in `DECISIONS.md`, with the glob syntax stated.

2. **Compose scaling.** Keep `service: kv` for all three nodes and use `deploy.replicas`.
   Preserves the grammar exactly, and contradicts D-F: replicas cannot publish distinct fixed host
   ports, so host-side health probing (the only probing this platform permits) becomes
   impossible. This trades a Phase 2 blocker for a Phase 0 one.

3. **Separate the group from the backend service (recommended).** Additive optional field on
   `harness.nodes[]` naming the backend's concrete service, leaving `service` as the logical group
   the grammar scopes over:

   ```yaml
   nodes:
     - { id: n1, service: kv, compose_service: kv-n1, role_hint: replica }
   ```

   `kv:*` and `minority(kv)` then keep their §4.3 meaning verbatim, D-F is satisfied, and node →
   container binding still happens through the `io.prothesis.node=<id>` overlay labels the brief
   §5 already specifies. The cost is one additive field, permitted by brief rule 4 because the
   directive names no field for a distinction that provably must exist. It must be marked
   `// ADDITIVE`.

**Recommendation.** Take (3), and take it before `internal/harness` is written rather than after:
the overlay generator needs the node → compose-service mapping on its first line of code, and
retrofitting it means rewriting the topology record that `up` persists for `down`.

---

<!-- Subsequent entries appended as analysis completes. -->

---

## OQ-017: the health probe template has a {port} placeholder but no field to bind it

**Classification:** RESOLVED. Additive `harness.nodes[].port`, defaulting to 8080. See DECISIONS.md D-027.

**Conflict.** The frozen config gives `probe: "http://{host}:{port}/healthz"`, but
`harness.nodes` carries only `id`, `service` and `role_hint`. Nothing in `prothesis.yaml` says
which port a node serves on, so `{port}` has nothing to bind to.

**Resolution taken.** Phase 0 assumes a container-side client port of 8080 and resolves the
published host port through `docker compose port`. This works for the fixture and for the
directive's own sample, both of which use 8080, but it is an assumption a real target will break.

**Recommendation.** Add an additive `port` field to `harness.nodes`, defaulting to 8080. Deferred
because it is config surface the lock will cover, and adding it after `up` starts persisting a
topology record means rewriting that record.

---

## OQ-018: the race detector cannot run on this host

**Classification:** RESOLVED. It runs in a Linux container; both modules pass race-clean. See DECISIONS.md D-023.

`go test -race` requires cgo, and this machine has `CGO_ENABLED=0` with no C compiler on PATH.
The full suite passes without it in both modules, but that is a strictly weaker statement than a
race-clean result, and the fixture's "exactly one injected defect" claim is partly licensed by
race-freedom.

**Recommendation.** Run `go test -race ./...` in both modules on the first machine with a C
toolchain, before Phase 2 depends on the one-defect property. Surfaces worth watching: the
recorder's `Timeline` and `RealClock` mutexes and drift goroutine, the `Appender` and bundle
mutexes, and the fixture's Raft mutex plus its HTTP handlers.

---

## OQ-019: `clock.skew` can move monotonic time but not the wall clock

**Classification:** ACCEPT-AND-DOCUMENT; a real capability gap, stated rather than papered over.

**What was measured.** Linux time namespaces offset `CLOCK_MONOTONIC` and `CLOCK_BOOTTIME` and
nothing else. Across every measurement in D-024, `wall_unix` was identical whether the offset was
+3600s, −300s or absent. This is the documented behaviour of the kernel feature, not a limitation
of the invocation.

**Consequence.** `clock.skew(ms)` and `clock.jump(ms)` attack logic keyed on *monotonic* time:
lease deadlines, timeouts, election timers, backoff. They cannot attack logic keyed on
`CLOCK_REALTIME`: calendar TTLs, certificate expiry, "expires at 03:00 UTC" scheduling, or any
protocol comparing wall timestamps between nodes.

The fixture's read lease is a monotonic deadline, so the anomaly the fixture exists to demonstrate
is fully reachable, and Addendum A.7's Rung 5 ("attack lease timers, TTLs, and any logic that
assumes monotonic or bounded clock drift") is substantially served. But a system whose bug lives
behind a wall-clock comparison is out of reach, and the verdict must not imply otherwise.

**Rejected alternative.** `date -s` in a privileged container does move the wall clock (it
succeeded in testing) but Docker Desktop's containers share one kernel clock, so it would move
every node at once and leak into the host VM. A per-node fault that is actually global is worse
than an absent one, because the resulting verdict would be attributed to the wrong node.

**Needs a human decision if** wall-clock skew is required. The honest options are a cooperating
system under test that reads an offset (which contradicts the requirement to work on unmodified
systems), or gVisor/Firecracker-class isolation with a per-sandbox clock.

---

## OQ-020: clock faults cannot be injected mid-window

**Classification:** ACCEPT-AND-DOCUMENT.

**Conflict.** The fault grammar is `KIND(TARGET[, PARAMS])@WINDOW`, so `clock.skew(n1, 3000)@8200..15100`
reads as "skew this node's clock between 8.2s and 15.1s". But the kernel accepts writes to
`timens_offsets` only *before any process exists in the namespace*, so the offset is fixed when the
namespace is created and cannot be changed while the target runs.

**Consequence.** A clock fault is implementable only as a **restart into a skewed namespace**,
which entails a `proc.restart` the schedule did not ask for. Raft tolerates the restart, so the
fixture still works, but the physical event differs from what the window syntax implies: the node
goes away and returns skewed, rather than having its clock shifted underneath it.

**Recommendation.** Phase 2 should represent this honestly rather than silently: a `clock.*` fault
whose window starts at t records BOTH the restart and the skew in the realized schedule, so the
`.thesis` world and the causal timeline show what actually happened. The alternative reading
(treating clock skew as a `topology_variant` fixed for the whole world, which the I2 tuple already
has a slot for) is cleaner but cannot express a mid-run jump at all.

---

## OQ-021: `io.latency` and `io.error` are not implementable on Docker Desktop

**Classification:** ACCEPT-AND-DOCUMENT; a real capability gap in a normative fault family,
stated rather than papered over.

**Conflict.** Directive 4.3 lists `io.latency(ms)` and `io.error(rate)` among the required Phase 2
fault families. Both need a shim BETWEEN the process and its storage (a FUSE layer, `dm-flakey`,
or a device-mapper target) and neither can be inserted under a container that is already running.

**What was checked.** `docker update` cannot change device throttles after create (they are
create-time `--device-read-bps` / `--device-write-bps` options only). The cgroup v2 `io` controller
expresses bandwidth and IOPS, not latency, and it addresses a block device by major:minor, but a
container's writable layer is overlay2 on the LinuxKit VM's virtual disk, which the host does not
own. Neither expresses a per-operation error rate at all.

**Resolution taken.** `PlatformCapability(kind)` refuses both statically, and `IOUnsupported.Inject`
fails with an `ErrUnsupported` naming the mechanism, the reason and the workaround. It is never a
silent no-op: a world whose faults never fired reporting PASS is the worst output this tool can
produce, so the failure maps to exit 2 INCONCLUSIVE. The fixture's `perturber.allow` does not list
either kind, so nothing in the mandatory path is blocked.

**Needs a human decision if** the Phase 2 definition of done requires all seventeen kinds to be
injectable on this host. The honest routes are a target whose data directory is a bind mount the
harness can interpose a FUSE filesystem on (which contradicts "work on unmodified systems"), or a
Linux host where the perturber owns the storage stack.

---

## OQ-022: `mem.pressure` cannot be withdrawn on a container that declares no memory limit

**Classification:** ACCEPT-AND-DOCUMENT.

**What was measured on this host** (Docker Desktop 29.1.3, cgroup v2):

| command | effect |
|---|---|
| `docker update --memory 268435456 c` | limit applied |
| `docker update --memory 0 c` | **no-op**: `HostConfig.Memory` unchanged at 268435456 |
| `docker update --memory -1 c` | **rejected by the CLI** (`invalid size: '-1'`) |

So once a limit is placed on a container that started unlimited, nothing reachable through the
Docker CLI can make it unlimited again.

**Consequence.** Injecting `mem.pressure` against an unlimited container would produce a fault that
cannot be withdrawn, which violates directive 4.3's Critical Guarantee outright. The kind is
therefore refused with `ErrUnsupported` unless the target ALREADY declares a memory limit, where
withdrawal restores the exact recorded byte value and `docker inspect` verifies it. **The KV fixture
declares no `mem_limit`, so `mem.pressure` is currently unavailable against the fixture.**

**Rejected alternative.** Writing `max` into the container's `memory.max` from a privileged helper.
It restores the kernel's view but leaves `HostConfig.Memory` saying otherwise, so HEAL's residual
check false-fails and the next `docker update` silently re-applies the old limit. A withdrawal that
leaves two sources of truth disagreeing is not a withdrawal.

**Recommendation.** Add `mem_limit` to the fixture's compose services if `mem.pressure` is wanted
against it. That is a fixture change and belongs to a human.

---

## OQ-023: `clock.*` has second granularity and needs util-linux in the TARGET image

**Classification:** ACCEPT-AND-DOCUMENT. Two limits found while implementing D-024; neither
contradicts it, both narrow it.

**1. BusyBox's `unshare` does not implement `--time`.** D-024 fixes the mechanism as an entrypoint
override to `unshare --time --monotonic=<s> …`, which silently assumes util-linux is present in the
target image. Measured:

```
alpine:3.20        unshare --time  ->  "unshare: unrecognized option: time"   (BusyBox v1.36.1)
debian:stable-slim unshare --time  ->  works (util-linux 2.41.5)
alpine + apk add util-linux-misc   ->  works (util-linux 2.40.1)
```

**This project's own kv fixture image is `alpine:3.20`, so `clock.skew` and `clock.jump` are not
injectable against the fixture as it stands.** `ClockCapability` probes the image with
`unshare --version` before anything is recreated and returns `ErrUnsupported` naming the remedy,
rather than recreating a container that will not boot, which would look like a bug in the system
under test.

**2. The offset is whole seconds.** util-linux parses `--monotonic` / `--boottime` as integer
seconds; `--monotonic=1.5` is rejected outright. So `clock.skew(ms)` truncates toward zero. An
offset that truncates to 0 is refused (it would be a fault that never fires); a non-zero truncation
is applied and the APPLIED value, not the requested one, is what the realized record carries:
`clock.skew(kv-n2, ms=5000)` for a requested 5500.

**Recommendation.** Either add `RUN apk add --no-cache util-linux-misc` to the fixture's runtime
stage (noting that this modifies the system under test, which the sidecar design exists to avoid)
or accept that the clock family is exercisable only against images that already ship util-linux, and
say so in the Phase 2 report rather than reporting the kinds as available.

---

## OQ-024: `fd.exhaust` needs a helper image and acts on PID 1

**Classification:** RESOLVABLE-WITH-DECISION; implemented, with two constraints that must be
visible.

**Mechanism, measured end to end on this host.** A helper container joined to the target's PID
namespace lowers `RLIMIT_NOFILE` on the target's PID 1:

```
docker run --rm --pid container:<target> --cap-add SYS_RESOURCE <helper> \
  prlimit --pid 1 --nofile=64:64
```

The target's "Max open files" went 1048576 → 64 and back, observed from INSIDE the target, with no
change to the target's image or configuration.

**Constraint 1: it acts on PID 1.** A container started with `init: true` has `docker-init` as
PID 1 and the application beneath it, so the limit would land on the wrong process. The fixture sets
`init: false` deliberately (so `proc.pause` freezes the real process); any target that does not must
be excluded from `fd.exhaust`.

**Constraint 2: a helper image is required.** BusyBox has no `prlimit`, so an alpine image is not a
substitute. The default is `debian:stable-slim` and it must already be in the local image store:
pulling one at injection time would put a registry round trip inside a timed fault window and make
injection depend on outbound network access from the Docker VM. Absent, the fault returns
`ErrUnsupported` naming the `docker pull` to run.

**Additive, and flagged as such.** `fd.exhaust` declares no parameters in the frozen registry, so
the magnitude is derived: the new limit is the target's CURRENT open-descriptor count plus a
headroom of 8. That makes the fault mean the same thing on a process holding 12 descriptors and on
one holding 12,000, which a fixed absolute limit would not. The headroom is a Go field, not a schema
field, so no normative name is invented, but it IS a behavioural constant that a human should
review.

---

## OQ-025: the perturber's own restarts must reach the realized schedule, and nothing consumes them yet

**Classification:** BLOCKING for the Phase 2 definition of done; a wiring gap, not a design flaw.

**The problem.** `no_crash` reports any process exit outside a planned fault window as a violation,
and it matches on CONCRETE NODE IDS (an empty node list excuses nothing, by design). Three of the
process/clock families' kinds cause a process exit on purpose:

- `proc.kill`: the exit IS the fault
- `proc.restart`: one stop/start
- `clock.skew` / `clock.jump`: TWO container recreates, in and out of the time namespace (OQ-020)

`internal/perturber/faults` emits a `Record` for every one of these physical events, and
`faults.OracleFaultWindows(records, timeline)` converts them into the `oracle.FaultWindow` list that
`oracle.Input.PlannedFaults` expects. **Nothing calls it yet.** `internal/control/runner.go` still
hard-codes `PlannedFaults: []oracle.FaultWindow{}`.

**Consequence if left.** Every deliberate kill and every clock fault is reported by `no_crash` as a
crash the tool discovered: a false positive produced by the tool's own perturber, which is worse
than a false negative because an agent reading the verdict has no way to tell it from a real finding.

**What has to change, and it is small.** `internal/control/runner.go` must (a) collect
`Records()` from each injector after PERTURB, (b) convert with `faults.OracleFaultWindows` against
the world's `recorder.Timeline`, and (c) pass the result as `oracle.Input.PlannedFaults`. The same
records convert to `schema.RealizedFault` through `Record.RealizedFault(timeline)` for the world
file's `fault_schedule.realized`.

**Note for whoever wires it.** A clock fault contributes THREE records, not one. Folding only the
schedule's planned faults into the realized list drops the two restarts, and `no_crash` then has no
window excusing the exits they caused. `perturber.Injection.Realized()` in `execute.go` builds one
realized entry per planned fault and therefore cannot express this on its own.

---

## OQ-021b: the Phase 2 reference schedule cannot trigger the anomaly, for a SECOND reason

**Classification:** RESOLVED for the acceptance test; the directive's text is left as written.

**Conflict.** §6 Phase 2's definition of done names this schedule:

```
proc.pause(role:leader)@8200..15100
net.partition(minority(kv))@8400..14900
```

OQ-010 already recorded that the 6.9 s pause exceeds the 5 s lease, so the lease expires during
the pause and the resumed ex-leader correctly refuses the read. That is one defect. There are
two more, and all three must be fixed together before the anomaly is observable.

**Second defect: `minority(kv)` destroys quorum.** It selects *an* arbitrary minority. Measured,
it resolved to `kv-n1`, a follower. Pausing the leader while partitioning a follower removes two
of three nodes from the connected majority, so no election can complete: no new term, no
committed write, nothing stale to read. Measured: the term advanced only at t+12270 ms, *after*
the pause was withdrawn at t+11320 ms, and zero stale reads occurred. The partition has to isolate
**the leader**, leaving the other two as a working majority.

**Third defect: the partition must outlast the pause.** With the pause alone, quorum is preserved
and the election does complete inside the window (measured: term 63→64 at t+9287 ms), but there
is still no stale read, because the instant the ex-leader unfreezes it receives the new leader's
heartbeat, learns of the higher term and steps down before serving anything. The displaced leader
must be unable to *learn* it was displaced while a client can still reach it.

**Corrected schedule, and the measurement that confirms it** (D-031):

```
net.partition(role:leader)@8200..13500
proc.pause(role:leader)@8300..11000
```

23 lease reads served by `kv-n2` at term 65 while the cluster stood at term 66, beginning 84 ms
after the pause lifted and ending when the partition lifted.

**Needs a human decision** on whether §6's stated schedule is amended in the directive. Nothing in
the code depends on the answer (the acceptance test uses the corrected schedule and D-031 records
why) but a reader following the directive literally will conclude the fixture has no bug.

---

## OQ-022b: `no_stuck_op` is structurally inconclusive when QUIESCE is shorter than the SLO ceiling

**Classification:** ACCEPT-AND-DOCUMENT; the oracle is behaving correctly; the lifecycle timing
is the question.

**Observed.** On a perturbed gate run the oracle reported: *"16 operation(s) were still in flight
when observation ended at t+18928ms before the 5000ms SLO ceiling elapsed"*, and returned
INCONCLUSIVE, taking the whole run to exit 2.

**Why that is right.** The oracle asks whether any operation stayed outstanding beyond the SLO
ceiling after HEAL. If observation stops before the ceiling has elapsed for the ops still in
flight, it cannot distinguish a stuck operation from one that is simply not yet due. Passing would
be a false PASS; violating would be a false FAIL. Inconclusive is the honest third answer, and the
run correctly reports exit 2.

**The real question is a lifecycle one:** QUIESCE's convergence window is currently shorter than
the SLO ceiling it is measured against, so a perturbed run that leaves any operation in flight is
inconclusive by construction rather than by evidence. Either QUIESCE must extend past the ceiling
before ASSERT runs, or the ceiling needs to be declared per profile, and the directive supplies
no config field for the ceiling, which is the gap already noted under Phase 1.

Not urgent for Phase 2, whose definition of done is about injection and withdrawal, but it will
make Phase 3's consistency oracles hard to exercise on perturbed runs until it is settled.

---

## OQ-026: `driver.profiles` is a gate-weakening vector the lock does not cover

**Classification:** ACCEPT-AND-DOCUMENT; a real residual hole, pinned by a test so it cannot be
forgotten.

`.prothesis/lock` covers `profiles.<name>.driver_profile`, so pointing the gate profile at a
different workload is caught. It does NOT cover `driver.profiles.<name>` itself, so editing
`gate: { clients: 16, ops: 20000, ... }` down to `{ clients: 1, ops: 5 }` weakens the gate as
effectively as shortening its budget, and moves no digest.

**Why it was not simply added.** The Phase 3 brief enumerates the covered set and every addition
widens the surface on which a spurious exit 4 can fire: the third way this phase fails. The three
additions that WERE made (`profiles` in whole, `oracles.builtin`, `oracles.dir`) each close a
one-token weakening with no plausible innocent edit. `driver.profiles` is different in kind: a
workload definition is edited during ordinary development far more often than a budget or an
allow-list, so covering it trades one hole for a stream of lock bumps.

**Recommendation.** Cover `driver.profiles` for the profile(s) named by a lock-covered run profile
only, rather than the whole map: the gate's workload is lock-relevant, an unused soak profile is
not. That needs a decision about whether the lock may follow a reference between two config
sections, which is a design question rather than an implementation detail.

**Pinned by** `TestUncoveredEditsDoNotMoveDigest/driver_ops_OQ026`, which asserts the CURRENT
behaviour. If the hole is closed, that subtest must be deleted deliberately.

---

## OQ-027: the lock file and the drift report have no normative schema

**Classification:** RESOLVABLE-WITH-DECISION; additive identifiers chosen, flagged rather than
assumed.

The directive names the PATH `.prothesis/lock` and the verdict field `oracle_lock.manifest_sha`. It
fixes no document schema for the lock file itself, no schema for the manifest projection that is
digested, and no machine-readable form for a drift report. Three identifiers were therefore chosen
in the `prothesis.*` family and are marked ADDITIVE in the code:

- `prothesis.lock/v1`: the `.prothesis/lock` document
- `prothesis.lock_manifest/v1`: the digested projection, carried INSIDE the preimage so that a
  future change to what the lock covers cannot be mistaken for a change to what the user wrote
- `prothesis.lock_report/v1`: `thesis oracles verify --json`

The report is deliberately NOT shaped like `prothesis.verdict/v1`. A drift check is not a verdict
(no world ran) and emitting a verdict-shaped document would invite an agent to read it as one.

**Needs a human decision if** any of these three names is later fixed normatively; the change is
mechanical but moves every downstream digest, which is an exit-4 event.

---

## OQ-028: `thesis oracles list` reads external oracle declarations best-effort

**Classification:** ACCEPT-AND-DOCUMENT.

`list` prints each built-in's class and valid phases authoritatively, from `schema.BuiltinOracle`.
For an EXTERNAL oracle it can only guess: the engine learns an external oracle's class and valid
phases from the oracle's own `prothesis.oracle_output/v1` document at run time, and no definition
file format is fixed anywhere.

`list` therefore attempts a tolerant YAML read for `name`, `class` and `valid_phases` (the frozen
oracle-output spellings, so no field name is invented) and falls back to showing the file path and
content hash when the file does not parse or carries none of them. A file that shows no declaration
is still locked and still listed; only the display degrades.

**Needs a human decision if** external oracle DEFINITIONS are to have a fixed schema. They probably
should: without one, `list` cannot tell an operator which phases an external oracle will be evaluated
in until it has been run at least once, and that is exactly the invariant-I5 declaration a reviewer
wants to see before trusting a verdict.

---

## OQ-029: the external oracle definition file format is additive; OQ-028 is resolved by it

**Classification:** RESOLVABLE-WITH-DECISION; resolved; recorded because it adds a format the
directive does not define.

**The gap.** The directive freezes `prothesis.yaml`, `prothesis.verdict/v1`,
`prothesis.oracle_input/v1`, `prothesis.oracle_output/v1`, the history JSONL, the exit codes and the
fault grammar. It says an oracle is an executable honouring the stdin/stdout contract, and it says
oracles live in `oracles.dir` as *"first-class, versioned, hash-locked artifacts"*. It never says
how PRO-THESIS learns that an executable exists, what class it belongs to, which phases it is valid
in, or how long it may run.

Those four facts cannot be deferred to the oracle's own output. Invariant I5 makes the ENGINE
responsible for phase filtering (*"the engine refuses to evaluate it outside them"*) so the engine
must know `valid_phases` **before** it runs the process. And severity is derived from `class`
(D-028 item 5), so a class read from the oracle's own document would let an oracle grade its own
finding down.

**Resolution taken.** `prothesis.oracle_def/v1`, a strict flat YAML mapping (`version`, `name`,
`class`, `valid_phases`, `cmd`, `timeout`) one oracle per file, discovered from `*.yaml`/`*.yml`
under `oracles.dir` and sorted by oracle name. Full rationale in DECISIONS.md D-039; the format
reference ships as `.prothesis/oracles/README.md` and `docs/ORACLE_DEFINITIONS.md`. The schema
identifier follows the additive precedent already in the tree (`prothesis.manifest/v1`,
`prothesis.topology/v1`).

**This resolves OQ-028**, which asked for exactly this and predicted the consequence of not having
it. `name`, `class` and `valid_phases` are spelled as OQ-028's best-effort reader already expects,
so `thesis oracles list` reads a conforming definition without change.

**One stale claim to correct.** `cmd/thesis/oracles.go` still tells the operator that *"class and
valid phases are declared by the oracle process itself"*. That was true when it was written and is
no longer: the engine takes both from the definition and REFUSES a finding whose output contradicts
it. The other half of that note (*"the lock covers this DEFINITION, not the executable it names"*)
remains exactly true and is the honest limitation D-G already requires the lock to state.

**Needs a human decision if** the definition format should instead be a block inside
`prothesis.yaml`. It was not chosen because CRUCIBLE §E requires oracles to be *"managed separately
from the target application code"* and because a per-file layout is what makes "adding an oracle" a
one-file diff a reviewer can read.

---

## OQ-030: `prothesis.oracle_output/v1` cannot place its own finding on the timeline

**Classification:** RESOLVABLE-WITH-DECISION; resolved additively, and the additive part is
OPTIONAL so no existing oracle breaks.

**Conflict.** `prothesis.verdict/v1` requires every violation to carry `phase` and `first_seen_ms`.
`prothesis.oracle_output/v1` carries `schema`, `oracle`, `class`, `valid_phases`, `status`,
`witness` and `explanation`: **no timing of any kind**. A built-in bridges the gap because the
engine holds its `Result`; an external oracle is a separate process and has no such channel.

Leaving it unbridged is not neutral. `first_seen_ms` would default to 0, which is DRIVE start, so
every external violation would be reported at a fabricated point on the timeline, and that is the
field a human reads first when reconstructing what happened.

**Resolution taken.** Two OPTIONAL members are read from the witness, which is the only free-form
part of the frozen envelope (the directive's own example shows `{"op_ids": [...], "key": "..."}` and
a witness is authored by the oracle):

- `first_seen_ms`: integer milliseconds relative to DRIVE start; the engine derives the observed
  phase from it against the measured windows.
- `phase`: pins the observed phase directly, for evidence with no usable timestamp.

An oracle that omits both is fully valid; its finding is then pinned to the phase it was evaluated
in, which is the honest answer rather than a fabricated one.

**The distinction these members do NOT collapse.** They describe `violations[].phase`, which is a
different field from `oracle.valid_phases` (OQ-004). Neither is ever checked against the other: the
fixture's stale read is found by an ASSERT-only consistency oracle and is OBSERVED in DRIVE (D-031),
and directive 4.6's own marquee example carries `"phase": "DRIVE"` for exactly that shape. Checking
the observed phase against `valid_phases` would report this tool's own definition-of-done finding as
an oracle defect. Pinned by `TestAViolationObservedOutsideValidPhasesIsNotADefect`.

What IS checked, and is the I5 enforcement the Phase 3 brief asks for, is the oracle's own
DECLARATION: if the output's `oracle`, `class` or `valid_phases` are present and contradict the
lock-covered definition, the finding is INCONCLUSIVE whatever its status; an executable that
disagrees with the declaration it was registered under is either the wrong binary or a drifted one,
and neither its violations nor its passes can be believed.

**Needs a human decision if** `first_seen_ms` should instead be promoted to a top-level member of
`prothesis.oracle_output/v1`. That would be cleaner and is where it belongs, but it is a change to a
frozen schema and belongs to the author of that schema, not to the implementer.

---

## OQ-031: a missing `oracles.dir` is reported, not refused

**Classification:** ACCEPT-AND-DOCUMENT.

**The tension.** A configured `oracles.dir` that is not on disk could mean a project that has not
run `thesis init`, or one whose oracles were deleted. The second is a gate-weakening move and the
second failure mode this phase is ranked against: a run reporting PASS because nothing was
checked.

**What was done.** `Discover` reports `Missing` rather than erroring, and the runner prints the
note (*"oracles.dir X does not exist, so NO external oracle ran"*) to stderr. The built-ins still
run, so the world is not unchecked; what is lost is any external oracle the project had.

**Why not an error.** The enforcement point already exists and is stronger: `.prothesis/lock` covers
every file in `oracles.dir`, so a directory that lost its contents moves the manifest digest and
exits **4**, the one code an agent loop must never resolve for itself. Refusing to run here would
add a second, weaker check that also breaks every project that has not yet created the directory,
including this repo's own `testdata/kvfixture`.

**The residual gap, stated rather than hidden.** The note goes to stderr and does not reach
`result.json` or the verdict, so a reader of the artifacts alone cannot tell "no external oracle was
configured" from "the directory was gone". Closing it means an additive field in the per-world
result record, which is the runner's format and is deferred rather than invented here.

**Needs a human decision if** a project with a lock file should refuse to run at all when
`oracles.dir` is absent. The lock's own drift check already produces that outcome; this entry
records that the oracle engine does not duplicate it.

---

---

## OQ-032: `thesis oracles list` lists every locked FILE as an oracle, and now says something untrue

**Classification:** RESOLVABLE-WITH-DECISION; display only, no gate impact, but it misleads an
operator in the one direction that matters. Raised by the Phase 3 integration pass, **not yet
fixed**, because the code is `cmd/thesis/oracles.go` and belongs to the lock work.

**Observed**, on a project freshly created by `thesis init` with the example definition activated:

```
  README.md                      external                []
                                 .prothesis/oracles/README.md  sha256:d2e8...
  linearizable.kv                external   consistency  [ASSERT]
                                 .prothesis/oracles/linearizable.kv.yaml  sha256:2ae9...
  linearizable.kv                external   consistency  [ASSERT]
                                 .prothesis/oracles/linearizable.kv.yaml.example  sha256:2ae9...
```

Three problems, all downstream of one cause: `externalRows` builds a row per file the LOCK hashes,
and the lock deliberately hashes every file in `oracles.dir` (D-G).

1. **`README.md` is listed as a registered oracle.** It is a locked file; it is not an oracle. The
   engine ignores it.
2. **The inert `.example` is listed as a registered oracle, under the same name as the live one.**
   That is the dangerous one: an operator seeing `linearizable.kv` already listed may conclude the
   scaffolded example is active and never perform the rename that actually activates it; the exact
   opposite of what the scaffold's own README instructs (D-039).
3. **The note is now false.** *"class and valid phases are declared by the oracle process itself"*
   was true when written, before a definition format existed. It is not any more: the engine takes
   both from the lock-covered definition and REFUSES a finding whose output contradicts it (OQ-029,
   OQ-030). The other half (*"the lock covers this DEFINITION, not the executable it names"*)
   remains exactly true and is the honest limitation D-G requires.

**Recommended fix**, which needs no new machinery because `internal/oracle` already returns the
data:

- build the oracle rows from `oracle.Discover(dir)`, which returns exactly the registered
  definitions with their authoritative `name`, `class` and `valid_phases`;
- render the remaining hashed files (`Discovery.Ignored` names them) under a separate heading such
  as *"also covered by the lock (not oracle definitions)"*, with path and hash but no name, class or
  phases;
- replace the note with the surviving half: *"the lock covers this definition, not the executable it
  names"*.

That also makes `list` agree with what will actually be evaluated, which is the property a reviewer
is reading the command for.

**Not a gate concern.** `list` over-reports rather than under-reports, and the engine's own
discovery is unaffected: `TestScaffoldWritesAnExampleThatIsItselfAValidDefinition` pins that the
`.example` is inert and that one rename activates it. The lock digest is also unaffected: it covers
file contents either way.

---

## OQ-033: a `linear` world can never report PASS, because `no_stuck_op` cannot finish observing

**Classification:** RESOLVABLE-WITH-DECISION; no soundness impact, but it caps the best achievable
verdict for the new profile at INCONCLUSIVE. Raised by the Phase 3 integration pass, **not fixed**,
because every available fix either weakens an oracle or defeats the profile's purpose.

**Observed.** Three measured `linear` worlds, all with `linearizable.kv` returning a well-evidenced
answer:

```
run    image     faults   linearizable.kv   no_stuck_op     verdict
8400   buggy     yes      violated          inconclusive    FAIL (exit 1)
d731   buggy     yes      violated          inconclusive    FAIL (exit 1)
ce48   kvfixed   yes      ok                inconclusive    INCONCLUSIVE (exit 2)
bcb7   buggy     no       ok                inconclusive    INCONCLUSIVE (exit 2)
```

`no_stuck_op`'s explanation is honest and correct: *"14 operation(s) were still in flight when
observation ended at t+18779ms before the 5000ms SLO ceiling elapsed"*. It refuses rather than
passing vacuously, which is exactly the behaviour invariant I5 and brief section 0 item 2 demand.

**The mechanism.** D-040 gives `linear` a deliberately over-large op budget so DRIVE outlives a
fault window reaching @13500ms. The driver is therefore ALWAYS stopped by `driverHandle.Stop` at
QUIESCE with operations in flight: that is the design, not an accident. `no_stuck_op` then has
in-flight operations whose fate it never observed, and it cannot distinguish "would have completed"
from "wedged". A profile that retires its whole budget (`smoke`) never hits this; a profile long
enough to span a fault window always does.

**Why a FAIL is unaffected.** `violated` outranks `inconclusive` in outcome ordering, so the
definition-of-done runs still exit 1. The cost is confined to the negative case: a `linear` world in
which nothing is wrong reports exit 2 rather than exit 0.

**Options, none taken here:**

1. **Drain before QUIESCE**: stop issuing new operations but let in-flight ones settle before the
   driver is killed. This is the real fix. It is a change to the DRIVE/QUIESCE boundary in
   `internal/control/runner.go`, i.e. lifecycle surgery, which is out of Phase 3's scope and would
   affect every profile.
2. **Have `no_stuck_op` ignore operations still in flight at a deliberate driver stop.** REJECTED as
   written: "the harness stopped the driver" and "the operation was wedged" are not distinguishable
   from the history alone, and an oracle that assumes the benign reading is precisely the vacuous
   pass this project has already been bitten by twice.
3. **Shrink the op budget** so the driver finishes on its own. Defeats D-040: the profile would
   then finish before the fault window, which is why `smoke` cannot serve.

**Recommendation.** Option 1, in the phase that owns the lifecycle. Until then the `linear` profile
should be understood as a FAULT-INJECTION profile whose clean-run verdict is INCONCLUSIVE by
construction, and that fact belongs in the fixture README rather than being discovered per-run.

---

## OQ-034: the checker's witness names a superseding write that may be an INDETERMINATE one

**Classification:** ACCEPTABLE-AS-IS, recorded because it changes how a witness should be read.

**Observed.** Two definition-of-done runs, same defect, same schedule:

```
8400  witness {key: k/0, op_ids: [11430, 11597, 11602]}   all three DETERMINATE
d731  witness {key: k/0, op_ids: [11402, 11580, 11578]}   11580 is an `info` write
```

Run 8400's witness is verifiable by inspection: write 11597 of `14000744` RETURNED 8.83ms before
read 11602 was INVOKED, and 11602 returned `10000729`, written by 11430. Strict real-time
precedence, no scheduling freedom, no interpretation of indeterminate records. Run d731's is not: op
11580 is an `info` write whose interval extends to infinity, so the named write and the named read
are concurrent and the pair alone proves nothing.

**This is not a defect.** The checker states it in the explanation itself (*"The witness op_ids
point at the failure; the proof is the exhausted search over the whole key, not those operations
alone"*) and the witness is documented as a POINTER built from the deepest partial linearization.
Deletion-based minimisation was rejected as unsound for good reason: removing a write from a
linearizable history can make it non-linearizable, so a "minimal" subset is not evidence about the
parent history. There is no small certificate for non-membership; that is why the problem is
NP-complete.

**Why it still matters.** A coding agent reading `violations[].witness` will try to reproduce the
anomaly from the named operations. When one of them is indeterminate, that reproduction fails, and
the agent may wrongly conclude the finding is spurious: the worst possible misreading of a
correct FAIL.

**Verified independently.** A search-free sufficient condition applied to run d731 using DETERMINATE
operations only (read R returned v, W_v completed before W' was invoked, W' completed before R was
invoked) finds **6** violations on key k/0, including read **11578**, the very read the checker's
witness names. So d731's finding is genuine; only its *superseding write* is the harder-to-read one.
The same condition finds **0** on both `ok` runs (kvfixed+faults, and buggy with no faults).

**Recommendation.** Prefer a witness whose superseding write is DETERMINATE when one exists,
falling back to the current pointer otherwise, and mark the witness with which kind it is. That is a
change to `cmd/thesis-oracle-linearizable` witness selection, not to the search, so it cannot affect
soundness. Deferred: it needs an additive witness member and therefore a decision on the frozen
shape (see OQ-030).

---

## OQ-033 RESOLVED: the drain landed, and it did not weaken the oracle

**Update.** Option 1 was taken: DRAIN before QUIESCE, in the phase that owns the lifecycle. See
DECISIONS.md D-042. `no_stuck_op` is unchanged; what changed is that the operations it judges are
now allowed to retire before observation ends.

**Measured on the fixture** (`linear`, seed 424242, one world), against the table this entry
originally recorded:

| image | faults | this entry recorded | now |
|---|---|---|---|
| buggy | none | INCONCLUSIVE (exit 2) | **PASS (exit 0)** |
| kvfixed | D-031 schedule | INCONCLUSIVE (exit 2) | **PASS (exit 0)** |
| buggy | D-031 schedule | FAIL (exit 1) | **FAIL (exit 1)** |

The last row is what makes the first two mean anything. `no_stuck_op` now reports *"all 3745
operation(s) completed within SLO ceiling (5000ms) after HEAL"*: a pass produced by an oracle that
ran and was satisfied, not by an oracle that was relaxed.

**The residual, stated rather than hidden.** A driver that does not implement the control channel
still cannot drain, so a project whose driver ignores stdin pays the deadline on every world and
its `linear`-shaped profiles stay INCONCLUSIVE exactly as before. That is now VISIBLE rather than
structural: the refusal is recorded in `result.json`, on stderr and on `DriverResult.Drain`, and
`RunnerOptions.DrainDeadline` may be set negative to stop paying for it. The fix makes a
cooperating driver's clean world reachable; it cannot make an uncooperative driver drain.

---

## OQ-022b RESOLVED: QUIESCE now outlasts the SLO ceiling

That entry asked: *"Either QUIESCE must extend past the ceiling before ASSERT runs, or the ceiling
needs to be declared per profile."* The first option was taken, in a stronger form. QUIESCE does not
merely wait longer; it ASKS the driver to finish, so on a cooperating driver there is usually
nothing left in flight at all, and on any driver the drain deadline (10s) exceeds the SLO ceiling
(5s), so an operation that is genuinely stuck now crosses the oracle's own threshold while someone
is still watching. See D-042. No config field for the ceiling was invented.

---

## OQ-035: additive environment variables carry what the frozen command template cannot

**Classification:** RESOLVABLE-WITH-DECISION; resolved additively, and recorded because it widens
a vocabulary the directive does not define.

**The gap.** `driver.cmd` is a frozen template interpolating `{history_path}`, `{seed}` and
`{profile}` (plus `{plan_path}`, reserved in D-021). None of them can carry a value that differs
per WORLD at run time, and Phase 4 needs two such values:

- **which cluster this world's driver should address.** Under parallel execution every world
  publishes different host ports, and a driver with baked-in targets addresses somebody else's
  cluster or nothing at all.
- **whether the driver has a stdin control channel.** The drain (D-042) needs the driver to know
  its stdin is a control channel rather than the null device.

**Resolution taken.** Two additive, `PROTHESIS_`-namespaced environment variables, on exactly the
terms D-021 established for `PROTHESIS_PLAN_PATH`; a driver that does not know the name ignores it,
and one that does needs no change to its command line:

- `PROTHESIS_STDIN_CONTROL=1` (`driver.StdinControlEnv`), set only when a pipe actually exists;
- `PROTHESIS_TARGETS` (`control.TargetsEnv`), `127.0.0.1:<host port>,...` in config node order, set
  only by the parallel executor.

Plus `harness.UpRequest.Env` / `DownOptions.Env`, which is not a driver variable at all: it is how
per-world values reach COMPOSE's `${VAR}` interpolation in the target's own compose file.

**Both are STRIPPED from the inherited environment when the harness did not set them**, and that
half is load-bearing rather than tidy. The fixture's loadgen defaults `--stdin-control` to
`PROTHESIS_STDIN_CONTROL=1`, so an inherited 1 with no pipe attached would point its stdin watcher
at the null device, read instant EOF, and stop the workload before it issued a single operation: a
world reporting on a system it never touched. An inherited `PROTHESIS_TARGETS` would silently point
a serial world's driver at some other cluster, with the same shape of consequence. Harness silence
must mean "use your own default", never "inherit whatever was lying around".

**One fixture change, and it is to the DRIVER, not to the system under test.** loadgen's
`--targets` default now reads `PROTHESIS_TARGETS`; an explicit `--targets` still wins. The `kv`
server is untouched.

**Needs a human decision if** the placeholder vocabulary is later declared closed, or if a
`{targets}` placeholder is preferred to an environment variable. The environment was chosen because
it needs no edit to a project's `driver.cmd`: a project that never runs a parallel search should not
have to change its config to keep working.

---

## OQ-036: `DefaultWorkerEnv`'s generic names are not what any real compose file spells

**Classification:** ACCEPT-AND-DOCUMENT.

`control.DefaultWorkerEnv` exports `PROTHESIS_PORT_<NODE>`, `PROTHESIS_PREFIX` and friends, so a
compose file that spells those names gets per-world isolation with no configuration. No real compose
file does. The KV fixture uses `KV_PORT_N1` and `KV_PREFIX`, because it was written before this
existed and because those are the names its author would naturally choose.

`ComposeVarEnv(prefixVar, portVars)` bridges the two, and the fixture's mapping is written out in
its documentation and pinned by `TestComposeVarEnvMapsTheFixtureVariables`. But it is a mapping the
CALLER must supply, so a project that runs a parallel search without one gets worlds that all
publish the same ports, and the second world's `up` then fails with a Docker "port is already
allocated" error rather than with a message about configuration.

**Why it was not made automatic.** Two routes were considered and both are worse. Scanning the
target's compose file for `${VAR}` and guessing which variable is a port is magic that fails
silently on the first file that names them differently. Requiring the target to adopt
`PROTHESIS_PORT_*` contradicts the requirement to work on unmodified systems, and would make
parallel execution a reason to edit the system under test.

**Partial mitigation in place.** `ParallelOptions.VerifyPortsFree` (default on) binds each slot's
ports before the run and refuses with a message naming the port, the node, the slot and
`thesis down`. That catches a stale project and a second concurrent search. It does NOT catch a
missing mapping, because the harness's ports are free: it is the target that is ignoring them.

**Needs a human decision if** an additive `harness.nodes[].port_env` field is wanted, so the mapping
lives in `prothesis.yaml` next to the node it describes rather than in the caller's Go. That is the
obvious next step and it was deliberately not taken here: it is lock-covered config surface, and
adding a field to `harness.nodes` to serve a search-only feature deserves its own review.

---

## OQ-037: the drain deadline is charged to every world, including ones that cannot use it

**Classification:** ACCEPT-AND-DOCUMENT.

`DefaultDrainDeadline` is 10 seconds and must exceed the 5-second `no_stuck_op` SLO ceiling (D-042).
A driver that honours the control channel pays almost nothing (measured on the fixture, 13ms to
21ms) because the deadline is a bound, not a wait. A driver that does NOT honour it pays the full
10 seconds on every world, which against the measured ~30s world cost is a 33% tax on a search whose
budget only closes through parallelism in the first place.

There is no way to detect the difference cheaply: a driver ignoring stdin is indistinguishable, from
this side of the pipe, from one that is finishing up. The deadline IS the discriminator, and paying
it once per world is what buys the answer.

**Mitigations available today.** `RunnerOptions.DrainDeadline` accepts a shorter bound or a negative
value that disables draining outright, and the refusal is recorded on the first world so an operator
learns it immediately rather than after a long run.

**Needs a human decision if** the executor should REMEMBER a refusal across worlds within one run
and stop asking. That would recover the tax, and it was not done because it makes the second world's
behaviour depend on the first's: a coupling this phase spent considerable effort removing
everywhere else (D-043), for a saving only an uncooperative driver ever sees.

---

## OQ-038: `EvalRequest.Realized` is additive, and the field it feeds was dead for three phases

**Classification:** ACCEPTED, implemented, recorded (D-046).

`oracle.Input.PlannedFaults` existed from Phase 1 and was populated with
`[]oracle.FaultWindow{}` (a literal empty slice) at its only production call site. `no_crash`'s
entire *"exited outside any planned fault window"* clause therefore never excused anything.

**Why nobody saw it.** `no_crash`'s own doc comment says *"In Phase 1 the perturber injects
nothing, so Input.PlannedFaults is empty and EVERY process exit is unexcused"*, which was true and
was even the Phase 1 definition of done. Phase 2 added the perturber and did not revisit the
sentence. Every Phase 2 acceptance schedule used `proc.pause` and `net.partition`, neither of
which makes a container exit, so the clause was never exercised against a fault that would have
needed it.

**The additive field.** `EvalRequest.Realized []schema.RealizedFault`. It carries the RESOLVED
node ids and the MEASURED window, which is what the excuse has to be keyed on; a planned
`role:leader` binds to a concrete node only at injection time (D-012). It invents no schema field:
`schema.RealizedFault` is the frozen world file's own type.

**Residual, stated.** A realized entry whose binding was not recorded excuses nothing. That is the
safe direction and it is deliberate, but it does mean a fault whose resolver output was lost will
still report its target's exit as a crash. Nothing observed has produced that state.

---

## OQ-039: `Observation.SampleIntervalMS` is additive, and assuming the rate disabled the classifier

**Classification:** ACCEPTED, implemented, recorded (D-047).

A.3 asks for a 200 ms telemetry trajectory during a probe world; the harness samples at
`search.observe.sample_interval_ms` (default 500). `ObserveOptions.ForProbe()` set the classifier's
assumed rate to 200, and the window-stretch guard then compared a real ~3500 ms seven-sample span
against a nominal 1200 ms and concluded the samples straddled a blackout.

**Measured:** 21 of 21 probes in run `r_2026_09_08_e363` classified INSUFFICIENT_DATA. Tier 2 was
never activated. The guard was doing exactly what it was written to do, on a premise nobody had
checked.

**The additive field** carries the rate the samples were ACTUALLY taken at, read from
`telemetry.Document.IntervalMS`. Zero keeps whatever `ObserveOptions` says, so no existing caller
changes behaviour.

**The open part.** A.3's 200 ms request is still unimplemented: the collector samples every probe
world at 500 ms. Honouring it would need a per-world telemetry interval on the runner, which is a
`RunnerOptions` change and a `TelemetryRequest` change. The cost of not honouring it is a coarser
trajectory, not a wrong one, because the classifier now knows what it is looking at. Left for a
human to weigh against the extra seam.

---

## OQ-040: A.9 #1 asks for a signal this defect does not emit

**Classification:** NEEDS A HUMAN RULING. The criterion as written cannot be satisfied on the
mandatory fixture, and the two ways to make it pass are both worse than failing it.

**The criterion.** A.9 #1: *"The Saboteur's Tier 1 PROBE sweep completes against the KV fixture and
correctly classifies `proc.pause` on the leader node as `REINFORCE_ACCELERATING` (due to the lease
bug)."*

**Measured** (run `r_2026_09_08_19d9`, full 21-probe sweep, control world evaluable):

```
proc.pause(role:leader)  DAMPEN  "trend -4.001/s does not exceed the reinforce
                                  threshold +0/s: the system is stable or recovering"
```

**Why.** A.4's classifier detects a RESOURCE spiral. The fixture's defect is a stale READ: a
displaced leader serving a value from an expired lease view. It produces no queue growth, no retry
explosion and no resource spiral; the surviving nodes' queue depth FALLS after the pause, which is
what a correctly-recovering Raft cluster does. The one A.3 criterion that would fire is the role
change, which carries zero weight for the reason OQ-006 item 4 gives: it fires identically on the
healthy and the buggy code path, so it carries no information.

**The two ways to "pass", and why neither is taken.**

1. Lower `reinforce_threshold` below zero. That classifies a FALLING queue as reinforcing: the
   ranked #1 failure mode written into a config key.
2. Give the role-change criterion weight. D-031 measured a leader election inside the pause window
   on the CORRECT code path. Weighting it would spend Tier 2's expensive rollouts on worlds
   selected by a coin that always lands the same way up.

**What the phase delivered instead.** The same sweep's `net.partition` probe found the stale read
directly, with ONE fault, at 107 s from a cold start (D-053). The Saboteur reached the bug A.9 #1
was using `REINFORCE_ACCELERATING` as a proxy for; it did not reach the proxy.

**Recommendation.** Restate A.9 #1 as *"the Tier 1 sweep completes, classifies every probe with a
definite label, and ranks at least one reinforcing signal"*, which IS satisfied, and move the
lease bug's detection to A.9 #2, where it belongs and where it passes. Only the specification's
author should make that change.

---

## OQ-041: `proc.kill` on this fixture is a guaranteed violation, which saturates any "found a bug" metric

**Classification:** ACCEPT-AND-DOCUMENT. It changes how a Phase 4 benchmark must be scored.

**Observed.** The fixture's `perturber.allow` contains `proc.kill`; its compose file sets
`restart: "no"` (correctly; auto-restart would mask `no_crash`); and `proc.kill` is an
instantaneous kind whose `Withdraw` is a no-op. So a killed node never comes back, and every
`proc.kill` world ends with:

```
availability_after_heal      VIOLATED  the killed node fails its probe after HEAL
resource_return_to_baseline  VIOLATED  goroutines on a surviving node stay elevated
```

Measured on run `r_2026_09_08_19d9`: worlds 14, 15, 16 and 17 (the four `proc.kill` probes) all
violated, on all four targets.

**Why it matters.** A search that stops at the first violation stops on its first `proc.kill`
world and never escalates. D-048 addresses the search-loop half. The measurement half is that any
benchmark endpoint of the form "did this trial find a violation" SATURATES at 10/10 for both arms
and measures nothing, which is why the A.9 #4 pre-registration uses `linearizable.kv`
specifically. That choice is disclosed in `PREREGISTRATION.md` rather than buried.

**Is it a true positive?** Arguably: a three-node cluster that loses one node to SIGTERM ought to
stay available, and the oracle is checking per-node availability. Arguably not: the harness killed
the node itself and cannot restore it, so the world is UNEVALUABLE for availability rather than
evidence of a defect. Deciding that is an oracle-semantics question, not a Phase 4 one, and
nothing here weakens either oracle to sidestep it.

---

## OQ-042: a `proc.kill` compounded with a durative net fault produces an unwithdrawable world

**Classification:** OPEN; a real defect, not fixed in Phase 4, with the reason for not fixing it.

**Observed.** When a branch pairs `proc.kill` with a durative `net.*` fault on the same node, the
net fault cannot be withdrawn: the sidecar cannot join the network namespace of a container that
has exited.

```
world 24: net.latency(kv-n1, mean=50, jitter=10)@2900..4400
          proc.kill(kv-n1, signal=SIGKILL)@3000..5000
  -> perturb: withdraw net.latency: sidecar in netns of 7d1aedac8a02:
     cannot join network namespace of a non running container ... is exited
  -> harness_error, world UNEVALUABLE
```

Measured on run `r_2026_09_08_19d9`: **13 of 40 worlds** ended this way. That is a third of the
budget spent on worlds that could not produce evidence.

**Phase 5 addendum: it also happens on INJECT, and via targets that only COLLIDE once resolved.**

The Phase 4 evidence above is the withdraw side, on a schedule naming one node twice. A Phase 5
end-to-end sweep produced it on the INJECT side, from two selectors that name nothing in common:

```
world 2: proc.kill(role:leader, SIGKILL)@7300..9300      -> resolved to kv-n2
         net.loss(any(1, kv), pct=30)@14700..16800       -> ALSO resolved to kv-n2
  -> inject net.loss on kv-n2: sidecar in netns of 431fe197c4c1: cannot join network
     namespace of a non running container: container ...-kv-n2 is exited
  -> world UNEVALUABLE (run r_2026_09_09_312a)
```

This matters for fix #2 above. "Refuse the pairing in the search" reads as a check over fault
STRINGS, and no such check would have caught this one: `role:leader` and `any(1, kv)` are disjoint
as written and collide only after resolution, which happens per world at inject time. A guard would
have to run on RESOLVED targets, which is the perturber's business rather than the tree's, and that
is a stronger argument for fix #1 (teach the perturber that a destroyed container has no residue,
and that a fault targeting one is unschedulable rather than failed) than the Phase 4 evidence alone
made.

**Two candidate fixes, and why neither was taken here.**

1. **Teach the perturber that a destroyed container has no residue.** This is probably the correct
   fix: the container's network namespace is gone, so the rules are gone with it, and reporting a
   residue is a false positive. But it edits `HEAL`'s residual verification, which is the check
   directive 4.3's Critical Guarantee rests on, and weakening a residue check to make a search
   tidier is precisely the move this project forbids. It needs its own decision and its own
   evidence.
2. **Refuse the pairing in the search.** A one-line guard in the tree's expansion. Rejected
   because the guard would live in `internal/search/saboteur` while the random baseline generates
   its schedules through `internal/search`, so only ONE ARM of D-029's pre-registered comparison
   would get the cleaner space, and the benchmark would then be measuring the guard. Putting it
   in `search.Validator` instead is worse: that type is documented as being
   `perturber.Compile` and nothing else, precisely so the tree and the executor cannot end up with
   two definitions of a legal schedule.

**Consequence accepted for now.** Both arms waste the same fraction of their budget on the same
pairing, so the comparison stays fair; the search is simply less efficient than it could be. The
honest number is 13/40.

---

## OQ-043: teardown removes volumes but never VERIFIES it, and they accumulate

**Classification:** OPEN; measured, cause not established, not fixed in Phase 4.

**Measured.** After a sequence of Phase 4 search runs the host carried **285 stale
`thesis-*_n?data` Docker volumes**, twelve per run (4 worker slots x 3 nodes). They are the
fixture's `n1data`/`n2data`/`n3data` named volumes, prefixed with the per-slot compose project
name.

**What is already right.** `control.runWorld` calls `Backend.Down` with `Volumes: true`, which
does pass `--volumes` to `docker compose down`, and that command demonstrably works; running it
by hand against a leftover project removed all three volumes immediately. Run
`r_2026_09_08_19d9` left none at all.

**What is missing.** `harness.Compose.sweep` (the verification that runs after `compose down` and
decides whether teardown SUCCEEDED) enumerates containers and networks by label and nothing else.
Volumes are outside it. So when `compose down --volumes` does not remove them, for whatever
reason, nothing notices and `Down` still reports success.

**Why the cause is not stated here.** Some runs leave twelve and some leave none, and the
difference has not been isolated. It is not the bridge-network ceiling (the network count returns
to baseline) so it is disk growth rather than a scalability wall, and asserting a mechanism that
has not been demonstrated would be exactly the kind of claim this project refuses. What IS certain
is that the verification does not cover the resource, so this class of residue is invisible to
`Down`'s own verdict.

**Direction.** Extend the label sweep to volumes carrying the project's compose labels, and count
them in the residual report. Then a run that leaves them says so.

---

## OQ-044: a transient Docker daemon outage leaks a project, and the run says so

**Classification:** ACCEPT-AND-DOCUMENT. The mechanism worked; the finding is what it cost.

**Measured.** Run `r_2026_09_08_5bc6` world 7:

```
faults:   net.latency(kv-n1<->kv-n3, mean=500, jitter=100)@11800..19500
          proc.kill(kv-n3, signal=SIGTERM)@12000..14000
outcome:  harness_error
teardown: docker daemon is not reachable: docker info --format {{.ServerVersion}}:
          exit status 1
```

Compose project `thesis-r_2026_09_08_5bc6-w02` survived with three containers and **two bridge
networks**, out of a pool of about twenty-four. `search.json` recorded
`free_bridge_networks_before: 24, after: 22`.

**What worked, and it is the part that matters.** The failure was RECORDED rather than swallowed:
`Backend.Down`'s error reached `worldResult.teardownErr` (D-043), the world was escalated to
`harness_error` rather than reported as a result about the system, and the run's own artifact
carried the before/after network counts. `TestEveryRecordedSearchRecordIsConsistent` then failed on
the recorded artifact and named the run, the direction and the magnitude, which is how the leak
was found at all. A run that leaks and says so is recoverable; one that leaks quietly exhausts the
pool and then fails with a message about address pools twenty worlds later.

**The residual.** A daemon that disappears mid-teardown cannot be swept BY that run, because the
sweep needs the daemon. The project stays until an operator or a later `thesis down` removes it.
That is inherent, not a defect, but it means the network-count check is a REPORT rather than a
guarantee, and a long soak needs an operator-facing sweep between runs.

**Confound worth stating.** The free-network count is GLOBAL. Another process creating networks
during a run looks identical to a leak. In this instance the leak was confirmed independently (the
surviving project was listed by `docker compose ls`) but the counter alone cannot distinguish the
two, and a report that treats it as proof is overstating it.

---

## OQ-045: A.9 criterion #1 is unfalsifiable on this fixture class

**Classification:** BLOCKING; requires a spec amendment before Phase 4 can be declared complete.

**Raised by:** D-052 (run `r_2026_09_08_19d9`), which measured the Saboteur's Tier 1 probe
classification of `proc.pause(role:leader)` and found `DAMPEN`, not `REINFORCE_ACCELERATING`.

**The criterion (A.9 #1).** *"The Saboteur's Tier 1 PROBE sweep classifies `proc.pause` on the
leader as `REINFORCE_ACCELERATING`."*

**Why it cannot be met on this fixture.** The fixture's defect is a stale read from an expired
Raft lease. A displaced leader silently serves an old value from local state. From a telemetry
perspective:

- No queue grows (the read completes successfully with HTTP 200).
- No retry fires (the client got a valid-looking response).
- The surviving majority correctly elects a new leader and proceeds normally, so their queue
  depth **falls**.

The Saboteur's `observe.go` spiral detector measures trend and acceleration of resource metrics.
A falling queue is DAMPEN by definition. The bug is invisible to resource telemetry because **it
is a correctness violation, not a performance degradation**. The system's metrics look healthy
while it serves wrong data.

**D-052's measurement:**

```
proc.pause(role:leader)  node=kv-n3  DAMPEN  rank 101.8
  "DAMPEN on status.queue_depth: trend -4.001/s does not exceed
   the reinforce threshold +0/s: the system is stable or recovering"
```

**Not fixable by tuning.** Lowering `reinforce_threshold` below zero would classify a FALLING
queue as reinforcing, which inverts the meaning of the classification system. This is the ranked
#1 failure mode for the Saboteur's detection model (a silent correctness bug that emits no
telemetry signature) and it is exactly the defect class the tool exists to find.

**The system found the bug anyway (D-053).** The probe sweep also runs the oracle engine on every
micro-probe world. `linearizable.kv` caught the violation directly during Tier 1, before any
signal classification or MCTS escalation was needed. The architecture is resilient to its own
detection model being wrong, because it does not depend on a single detection channel.

**Recommendation.** Amend A.9 #1 to accept EITHER:

1. `REINFORCE_ACCELERATING` classification from the probe sweep (the current criterion), **OR**
2. Direct oracle violation witnessed during the probe sweep (what D-053 actually delivered).

This makes the Definition of Done reachable for the class of bugs the tool exists to find
(silent correctness violations) without weakening the signal classification system or its gate.

**Needs a human decision** on whether to amend A.9 #1 as recommended, or to construct a second
fixture whose bug DOES emit a reinforcing telemetry spiral (e.g. a retry storm from a timeout
misconfiguration) and satisfy the criterion against that fixture instead.

---

## OQ-046: the unscripted write-path bug was dead code: a defence-in-depth ordering makes it unreachable

**Classification:** RESOLVED; experimental finding, no action required.

**Experiment (D-054).** Inject a novel "false success" bug into `raft.go:applyCommitted()`:
when a log entry at index N is overwritten by a higher-term entry, 25% of the time deliver
the overwriting entry's result to the old proposal's waiter as if the original write succeeded.
The client records `applied:true` for a command that was never committed.

Run `thesis search` (Saboteur + random, 24+8 worlds, 10 min budgets) and observe whether the
linearizability oracle catches the violation.

**Result: zero violations detected.** Not because the oracle is weak, but because the bug
**cannot fire** on this implementation.

**Why the bug is dead code.** The Raft implementation sequences three operations atomically
under the mutex:

1. **Truncate** conflicting entries from the log (`rlog.TruncateFrom(idx)`, line 589).
2. **Fail waiters** for every truncated index (`failWaiters(truncated)`, line 589). This
   removes the waiter from `r.waiters[idx]` and sends `ErrOverwritten` on its channel.
3. **Append** the new leader's entries to the log (lines 594-610).

`applyCommitted()` runs later, when the newly appended entries are committed. By that point
`r.waiters[N]` is already gone: `failWaiters` deleted it in step 2. The injected condition
`e.Term > p.Term && r.rng.next()%4 == 0` can never evaluate to `true` because `p` does not
exist.

```
                  HandleAppendEntries (mutex held)
                  ┌─────────────────────────────────────────────┐
                  │ 1. detect term conflict at index N          │
                  │ 2. TruncateFrom(N) → returns [N, N+1, …]   │
                  │ 3. failWaiters([N, N+1, …])  ← waiter GONE │
                  │ 4. append new entries at [N, N+1, …]        │
                  │ 5. advance commitIndex                      │
                  └─────────────────────────────────────────────┘
                            │
                            ▼
                  applyCommitted()
                  ┌──────────────────────────────────────────┐
                  │ r.waiters[N] → nil. Bug never fires.     │
                  └──────────────────────────────────────────┘
```

**What this teaches us about the fixture.** The implementation has a defence-in-depth property:
the waiter lifecycle is coupled to the LOG lifecycle, not to the APPLY lifecycle. A waiter is
failed the moment its log entry is truncated, not when a replacement entry is applied. This
ordering is load-bearing for correctness and is exactly the kind of invariant a Raft
implementation gets wrong under refactoring. The fact that it is PRESENT (and prevents the
injected bug from firing) is itself a finding worth recording.

**What this teaches us about PRO-THESIS.** The tool correctly produced zero violations for a
world with zero violations in it. It did NOT produce a false positive. That is the most boring
possible outcome of a negative experiment, and it is exactly what a sound tool should do. The
Saboteur explored 32 worlds including `net.partition`, `proc.pause`, `proc.kill`, `net.loss`,
and multi-fault compositions. None of them triggered the injected condition because none of them
CAN: the precondition is structurally impossible, not probabilistically unlikely.

**The experiment's actual contribution.** This failed injection attempt reveals that a
*credible* write-path bug in this fixture must be injected BEFORE `failWaiters` in the
truncation path: either by removing the `failWaiters` call, or by injecting a bug in
`HandleAppendEntries` that skips truncation for certain entries. The `applyCommitted` path is
the wrong injection site because the implementation has already closed the vulnerability window
by the time apply runs.

---

## OQ-047: the witness key is NOT stable, and the default identity rule therefore rejected every legitimate reduction

**Classification:** RESOLVED in Phase 5 by D-054, and logged because the resolution changes what
"the same bug" means.

**The claim that was assumed.** `internal/shrink/identity.go` made `MatchWitnessKey` the default
strictness on this reasoning:

> The key is the discriminator that actually catches a switched bug. A stale read on k/0 and a
> stale read on k/7 are the same defect; a consistency violation that moved to a key the original
> never touched is a different finding and must not be accepted as a reduction.

Those two sentences disagree with each other. The first says a moved key is the same defect; the
second says it must be rejected. Only the second was implemented.

**Measured.** Every `linearizable.kv` violation this repository has ever recorded, by witness key:

```
27 violations of the ONE injected Raft lease defect
  k/0  x20
  k/1  x5
  k/2  x2
```

Two runs produced two DIFFERENT keys within a single run (`r_2026_09_08_04ad`: k/0 and k/2;
`r_2026_09_08_c066`: k/0 and k/1). Phase 5 then observed k/6 as well, so the spread is at least
four keys. The fixture drives 16 concurrent clients over 8 keys; which key catches the stale read
is decided by what was in flight when the partition landed, not by the defect.

**Consequence, observed twice, by two different routes.** Shrinking world
`r_2026_09_09_2b4e/world-0001` (target key `k/1`):

```
run 1: baseline executed the UNREDUCED world twice, got k/0 both times
       -> "the unreduced world did not reproduce ... witness key k/0, want k/1"
       -> StopBaselineNotReproduced, exit 1, nothing shrunk

run 2: baseline got LUCKY and hit k/1 on the first world
       -> stage 1 ran 4 worlds; every reduction landed on k/0 or k/6 and was rejected
       -> faults 2 -> 2, confirmation 0/1, exit 1, nothing shrunk
```

The second route is the dangerous one. The run looks healthy, spends its budget, and reports "no
fault could be removed": a false negative that reads exactly like a correct one, which is the
outcome `identity.go`'s own doc comment names as the thing to avoid. No amount of retrying detects
it, because retrying is what produces it.

**A third route, found while fixing the first two.** The first fix asked the question deliberately
at stage 0: execute the unreduced world a second time and compare the witness keys. Run
`r_2026_09_09_9d07` then showed why two samples are not a measurement:

```
stage 0 probe: k/0, k/0  -> "the key is part of this defect's identity"
same run, next 60 worlds: k/1, k/2, k/3, k/4 all observed for the SAME oracle+class
```

`k/0` holds ~74% of the time (20/27), so two draws agree ~55% of the time while proving nothing.
That run reduced **14 faults to 1** and **6761 operations to 1**, narrowed the window to 915ms, and
then failed its own confirmation gate **1/3**, because the gate matches at the same level. A
correct minimal repro was computed and discarded.

**Resolved by D-054**, which measures in two places: stage 0 asks deliberately, and the whole run
keeps listening, so any later drift observation calibrates too. Calibration is frozen before the
confirmation gate, because a gate that relaxes its own criterion while being applied is
indistinguishable from a gate being gamed.

**Deferred sub-question.** The same argument applies to `MatchWitnessOps` on a Tier A driver, where
op ids *should* be stable and a drift would mean something real. Nothing measures that today,
because this repository has no Tier A driver to measure it on.

---

## OQ-048: a fault's withdrawal was verified against EVERY fault's rules, so concurrent net faults failed each other

**Classification:** RESOLVED in Phase 5. Logged because it silently cost worlds for three phases.

**Observed.** A 14-fault world pairing `net.partition(role:leader)@3000..7500` with
`net.loss(kv-n2, pct=1)@4000..4500` (both resolving to kv-n2) failed its own withdrawal:

```
world 1 error: perturb: perturber: withdraw net.loss(kv-n2, pct=1)@4000..4500 [f006] on kv-n2:
  faults: withdraw net.loss: node kv-n2: 8 tagged iptables rule(s) survived
-> world INCONCLUSIVE, exit 2 (run r_2026_09_09_42db)
```

Nothing had actually leaked.

**Cause.** `WithdrawIPTablesCommands(f.tag)` deletes by the fault's OWN tag
(`thesis:<run>:<fault>`), and then `CountTaggedCommand()` verified the result by grepping the
SHARED prefix `thesis:`. Any other PRO-THESIS rule legitimately installed on that node (including
one belonging to a fault still in its window) was counted as this fault's residue.

The asymmetry made it visible: the qdisc half of the same self-check has always been per-fault
(it verifies `p.slots`, not every slot on the node). Only the iptables half was global.

`perturber.budget.max_concurrent_faults` is 3, so overlapping faults are the design, not an edge
case. Any schedule with two iptables-based faults on one node was unrunnable.

**Fixed** by making `CountTaggedCommand` take what to count: the fault's own tag from the per-fault
withdrawal, `TagPrefix` from the HEAL-time residual sweep, which is the one place a global count
is the correct question. An empty argument falls back to the prefix rather than matching nothing,
so a mistake cannot turn the self-check vacuous.

---

## OQ-049: D-049's capability table was wired into the SEARCH only, and every other path paid a world to learn it

**Classification:** PARTIALLY RESOLVED in Phase 5. The remaining gap is stated below.

**Observed.** A hand-authored 14-fault schedule naming `io.latency` reached PERTURB before failing:

```
perturber: inject io.latency(kv-n3, ms=5)@4800..5300 [f007] on kv-n3: io.latency: delaying
filesystem operations needs a shim between the process and its storage ... perturber: fault is
not injectable in this environment
-> world INCONCLUSIVE (run r_2026_09_09_42db), ~39s and one compose project spent
```

**Cause.** `faults.PlatformCapability` answers this statically, and its own doc comment says it
"exists so a schedule can be rejected before a world is booted and driven". D-049 wired it into
`internal/search/engine`, which covered the Saboteur and the random baseline. Nothing called it on
any other path: `thesis run --fault`, a hand-authored schedule, and every shrink candidate reached
PERTURB and failed there. The reference fixture ships `io.latency` in `perturber.allow`, so this
was reachable out of the box.

**Fixed** by inverting the dependency. `internal/perturber` cannot import
`internal/perturber/faults` (faults imports perturber), so perturber now declares an optional
`PlatformCapable` interface and `faults.IOInjector` implements it. Both gates
(`checkKindsInjectable` on the compile path and `checkScheduleInjectable` on the executor path)
ask the same two questions through one helper.

**Remaining gap, deliberately not closed here.** The refusal now lands at executor construction,
which is after BOOT. It cannot move earlier without a second copy of the capability table outside
the registry, because injectors are rebuilt per world (container ids change every world, D-032) and
no registry exists before BOOT. So a schedule naming an undeliverable kind still costs a BOOT, but
no longer a DRIVE, and it now fails at the layer that owns the answer rather than mid-PERTURB.
Closing it properly means deciding where a pre-BOOT capability check should live, which is a
layering decision and not a bug fix.

---

## OQ-050: `--skip-ops` reported that the user's driver lacks a seam it has

**Classification:** RESOLVED in Phase 5.

**Observed.** Every Phase 5 shrink invoked with `--skip-ops` reported:

```
  ops     0 world(s)  SKIPPED (the driver does not accept an explicit op plan
          ({plan_path} / PROTHESIS_PLAN_PATH, D-021), so the workload cannot be
          reduced for this target)
```

The fixture's loadgen implements plan mode, and `driver.ExtractOpsFromFile` +
`driver.NewPlan` + `ext.Plan` all succeed on its histories (6761 operations extracted from
`r_2026_09_09_9d07/world-0001`). The statement was false.

**Cause.** The CLI implemented `--skip-ops` by not populating `Options.Ops`/`Options.OpPlan`, so
the pipeline could not distinguish "the caller declined this stage" from "this driver cannot do
it". `stages.go` insists elsewhere that these are different facts (*"'we did not shrink the
workload' and 'the workload could not be shrunk further' are different facts and only one of them
is evidence"*) and the ops stage was the place that conflated them.

**Fixed** with `Policy.SkipOps`, so the report names who declined. This matters beyond tidiness:
the shrink report is the artifact an agent reads to decide what to do next, and this one told it
to go fix a driver that was already correct.

---

## OQ-051: five OQ ids and three D ids are duplicated, and 77 references cannot be disambiguated mechanically

**Classification:** RESOLVED by D-067 (see the follow-up at the end of the file); a
documentation-integrity defect, with the reason it was not fixed by renumbering at the time. The
counts below are as recorded then; three of the five OQ "collisions" turned out to be follow-up
sections of one topic, not collisions.

**Observed.**

```
OPEN_QUESTIONS.md   OQ-003 x2   OQ-004 x2   OQ-021 x2   OQ-022 x3   OQ-033 x2
DECISIONS.md        D-031  x2   D-032  x2   D-033  x2
```

Both documents are append-only and both are cited from code comments. References to the colliding
ids, across `*.md`, `*.go` and `prothesis.yaml`:

```
OQ-003  4      OQ-004  29     OQ-021  10     OQ-022  5      OQ-033  29
```

**Why renumbering was refused.** Each of those 77 references was written meaning ONE of the two or
three entries that now share its number. Renumbering requires deciding, per reference, which one,
and a reference silently pointing at the wrong entry is a worse defect than a visibly duplicated
heading, because a duplicate announces itself and a wrong pointer does not. The safe fix is to
disambiguate each reference against its subject by hand, which is a documentation task, not a
mechanical one.

**Interim mitigation.** This entry is the index: a reader who follows a citation to a colliding id
and finds two or three entries knows the collision is known, is listed here, and must be resolved
by subject rather than by number.

---

## OQ-052: the fixture driver's plan mode is a doc comment and an unread field, and stage 2 believed it

**Classification:** OPEN in the fixture; the PRODUCT side is fixed. This is the more serious of the
two and it is worth stating in that order.

**Observed.** The Phase 5 acceptance shrink of a 14-fault world reported:

```
  ops         6761 -> 1      28 world(s)  13m58s  complete
  Operations:   6761 -> 1
  Confirmation: 0/3 (target 3/3)     -> FAIL, exit 1
```

A linearizability violation needs at least a write and a conflicting read. A one-operation world
cannot be non-linearizable, so the reduction was not merely aggressive: it was impossible.

The artifacts say why. From world 60 of run `r_2026_09_09_2904`:

```
plan.json      operations: 1
history.jsonl  13,259 records -> invoke 6626, ok 6593, fail 19, info 21
```

The driver was handed a plan of one operation and drove six and a half thousand.

**Cause, in the fixture.** `testdata/kvfixture/cmd/loadgen/main.go` declares

```go
// planPath is {plan_path} / $PROTHESIS_PLAN_PATH.
planPath string
```

and never reads it. There is no flag registration, no environment lookup, no plan load and no
execution path. `testdata/kvfixture/internal/plan/` (the parser, with its own tests) was written
and never wired in. The file's header comment describes PLAN MODE in the present tense throughout.

**Why this is worse than a missing feature.** Stage 2's whole inference is "I shortened the plan and
the violation survived, therefore the removed operations were not the cause". If the driver ignores
the plan, *every* candidate reproduces, so ddmin never encounters a rejection, walks straight down
to a single operation in the minimum possible number of worlds, and reports a large, confident,
entirely fictional reduction. Nothing in the oracle verdict can detect it: every world really did
violate.

**What caught it.** The confirmation gate, at 0/3: a layer doing its job over a layer beneath it
that had failed. That is the architecture working, and it is the reason a k/k gate is worth its
worlds.

**Fixed on the product side (D-056).** `thesis shrink` no longer trusts a driver to have honoured
its plan. When a candidate carries an explicit op list, the executor counts `invoke` records in the
history the driver actually wrote and refuses the attempt if there are more than were planned. The
check is deliberately one-sided (FEWER invocations than planned is normal, because the driver is
stopped at QUIESCE) and it names both remedies: wire `{plan_path}` / `$PROTHESIS_PLAN_PATH`
(D-021), or pass `--skip-ops`.

This belongs in the product rather than the fixture because ANY user's driver may ignore the plan,
and a tool that silently converts that into a reported reduction is worse than one that has no
stage 2 at all.

**Still open in the fixture.** loadgen does not implement plan mode, so stage 2 cannot be
DEMONSTRATED on this fixture: it can now only be correctly refused. Implementing it means reading
the plan, executing its operations in order, and honouring the `at_ms` offsets the parser already
carries (`planPace`, also declared and unread). Until then the Phase 5 acceptance evidence covers
fault reduction only, which is stated plainly rather than papered over.

---

## OQ-052 UPDATE (D-062): the fixture driver now executes its plan; stage 2 is still not demonstrated end to end

**Classification:** the FIXTURE half is implemented. The ACCEPTANCE half ("a shrunk operation
trace still reproduces the defect") remains unproven, and the distinction is the whole point of
this entry.

**What was wired.** `cmd/loadgen` now registers `--plan`, reads `$PROTHESIS_PLAN_PATH`, loads the
trace through the `internal/plan` parser that had been written and left unimported, and executes
it: one goroutine per RECORDED process, operations in plan order, recorded op ids carried verbatim,
targets mapped by POSITION rather than by recorded address, and `--plan-pace` honouring `at_ms`.
The two blank imports that existed only to keep the file compiling are gone, as is `_ = t0`.

`doTxn` was split so a replayed transaction executes its recorded op list rather than being
reconstructed from a read-key/write-key pair, which would have silently rewritten any transaction
shape the generator does not itself produce. Plan mode chooses arguments; it does not add a second
way to perform an operation, so the ok/fail/info soundness property stays structural.

**Measured.** Six tests against `httptest`, so plan mode is verified without Docker. The decisive
one hands the driver a 4-operation plan and requires exactly 4 invocations. Mutation-verified by
loading the plan and not dispatching it; the original defect, reproduced verbatim:

```
the driver issued 60000 invocation(s) for a 4-operation plan
```

60,000 is the `linear` profile's op budget. That is the number that made every stage-2 candidate
reproduce, walked ddmin down to one operation, and produced the fictional "6761 -> 1".

**What is STILL NOT demonstrated, and must not be reported as if it were.** A driver that honours
its plan is a precondition for operation shrinking, not a demonstration of it. Nothing here shows
that a REDUCED trace still reproduces the lease defect, because that needs the real cluster and a
real shrink run. Specifically unproven:

- that stage 2 converges to a small trace on this fixture rather than to nothing;
- that a reduced trace passes the k/k confirmation gate at all: the defect reproduces ~78% of the
  time (OQ-054), and a 3-operation trace may well reproduce far less often than a 15,000-operation
  one, because fewer operations means fewer chances to catch the stale-read window;
- that the minimal trace is CAUSAL rather than incidental.

The honest current claim is therefore unchanged from Phase 5: **fault reduction is demonstrated
(14 -> 1, confirmed 3/3); operation reduction is not.** What changed is that stage 2 can now be
attempted on this fixture instead of correctly refused. Until an acceptance run exists, this entry
stays open and the README continues to list operation-level shrinking under "not demonstrated".

**Related.** D-056 is the product-side refusal that caught this; it stays, because any user's driver
may ignore its plan and the fixture honouring its own proves nothing about theirs.

---

## OQ-053: ddmin accepts a reduction on ONE lucky world, and the discarded faults never come back

**Classification:** OPEN; the sharpest finding of Phase 5. Mitigated, not resolved, with the reason
below.

**Observed.** Shrinking a 14-fault world whose ONE essential fault was a leader partition, with the
op stage and narrowing both off so nothing else could confound it:

```
shrink: candidate 0 (14 fault(s)): reproduced after 1 world(s)
shrink: candidate 0 ( 7 fault(s)): reproduced after 1 world(s)
shrink: candidate 0 ( 3 fault(s)): reproduced after 1 world(s)
shrink: stage faults: 14 -> 3
shrink: stage confirm: reproduced 0/3 (gate 3/3)      -> FAIL, exit 1

surviving faults:
  net.latency(kv-n1, mean=20, jitter=0)@500..1000
  net.latency(kv-n2, mean=20, jitter=0)@1000..1500
  net.loss(kv-n3, pct=1)@1500..2000
```

Those three are the BENIGN NOISE faults, scheduled before the partition. The partition
(`net.partition(role:leader)@3000..7500`, the fault the world was built around and which D-053 shows
reproduces the defect on its own) was dropped.

**The character of the failure is LUCK, not a wrong constant.** Two earlier full-pipeline runs of
the same world at the same `AcceptTrials` of 1 kept the partition and reduced to exactly it. A third
kept three noise faults instead. Same world, same policy, same seed; three runs, two right answers
and one wrong one, and the report cannot tell them apart, because a false accept looks exactly like
a true one. The only thing that distinguished them was the confirmation gate at the very end, after
the whole budget was spent.

Raising the threshold to 2 fixed it on the next run (14 -> 1, the partition), which is the direct
confirmation of this diagnosis: at a per-candidate false-reproduction probability q, one trial
accepts a wrong subset with probability q and two with q².

**Cause.** `DefaultAcceptTrials` is 1. ddmin's correctness argument assumes the predicate is a
FUNCTION of the input; under Tier B it is a coin weighted by the input. One reproduction is then a
sample, not a proof, and the asymmetry runs the wrong way for this failure: one hit accepts, two
misses reject (`DefaultRejectTrials` = 2), so the search leans toward accepting reductions, which is
the direction that over-reduces.

The damage is asymmetric too. A false REJECT keeps a fault that did not matter: the repro is bigger
than necessary, and it still reproduces. A false ACCEPT drops faults ddmin will never revisit, so
the essential one is gone for the rest of the run and nothing downstream can recover it.

**The incoherence, stated plainly.** The pipeline accepted reductions on 1/1 and then judged the
result at 3/3. It spent its entire budget constructing an answer it had already decided it would not
believe.

**What caught it.** The confirmation gate, again: 0/3, on a schedule that visibly cannot produce a
stale read. Two independent Phase 5 defects (this and OQ-052) were both caught by that gate and by
nothing else.

**Mitigated, not resolved.** `--accept-trials N` now exists, and when the gate fails on weaker
evidence than it demands the report says so and names the flag. The default stays 1 because raising
it doubles the cost of every accepted reduction (the branch a successful shrink takes most often)
and the right value depends on the target's per-execution reproduction probability, which the
pipeline does not know and this repository has not measured.

**The real fix, deferred.** MEASURE that probability. The baseline stage already executes the
unreduced world more than once for the witness-key experiment (D-054); the same worlds are a sample
of the reproduction rate, and `AcceptTrials` could be derived from it: enough trials that the
false-accept probability is below some stated bound. That turns a guessed constant into a measured
one, which is the same move D-054 makes for identity, and it is the correct end state. It needs a
decision about what bound to target and a budget to validate it on.

---

## OQ-054: the k/k gate assumes a determinism the target does not have, and the schema already knew better

**Classification:** OPEN; a design question, not a bug, with a concrete proposal. It is the reason
Phase 5's acceptance evidence stops where it does.

**Observed.** The Phase 5 acceptance shrink reduced a 14-fault world to the ONE fault that causes
the defect (`net.partition(kv-n2)@3000..7500`, which is D-053's mechanism exactly) and then failed
its own confirmation gate at **1/3**. The reduction was correct.

**And 1/3 was not the rate.** Replaying that same world file six times measured **6/6**:

```
thesis replay world-0024/world.thesis -k 6 --expect linearizable.kv
  attempt 1..6/6: reproduced (violation)   Reproduced: 6/6
```

The two confirmation worlds that did not reproduce were checked directly and are honest: their
histories are 16.7k and 17.0k records and every one of the 8 keys admits a linearization. Pooling
both batches, the minimal schedule reproduced **7 of 9 times, p ~ 0.78**.

So the gate did not fail because the repro is bad. It failed because **a 3/3 gate on a p = 0.78
defect passes only p³ ~ 47% of the time**: a coin flip, applied to a correct answer, after the
whole budget has been spent computing it. That is the finding, and it is worse than a merely strict
threshold: the gate's outcome is dominated by luck rather than by the quality of the reduction.

**The tension.** `Confirmation.Passed()` requires k/k with zero inconclusive, and `corpus.Add`
refuses anything less. That is the right instinct: a corpus entry that reproduces one time in three
makes `thesis regress` FLAKY, and a regression suite that reports PASS two times in three is worse
than no suite at all; it is a false green over a live defect.

But the consequence is that **a probabilistic defect can never enter the corpus**, no matter how
correct its minimization. The fixture's own injected defect is probabilistic: it needs a client to
be mid-read on the isolated leader inside a 5-second lease window, which is a race, not a certainty.
PRO-THESIS can find it, minimize it to one fault, and then is structurally unable to commit it.

**The schema already disagrees with the gate.** `schema.MinimalRepro` carries:

```go
// Reproduced is "k/n": how many confirmation replays reproduced the
// violation. It never claims more than was measured.
Reproduced string `json:"reproduced"`
```

A field whose whole purpose is to record that reproduction is fractional: in a pipeline that
refuses to commit anything fractional. The frozen schema anticipated this and the policy did not.

**Proposal, not implemented here.** Store the measured rate and test against it, rather than
demanding a determinism the target lacks:

1. `corpus.Add` accepts a world whose measured rate is recorded, with a floor (a world that
   reproduced 0/n is still refused: that is the "repro that does not reproduce" failure and it must
   stay refused).
2. `thesis regress` replays each world enough times to distinguish present from absent AT ITS
   RECORDED RATE, rather than once. At the measured p = 0.78, two replays give ~95% power to see at
   least one reproduction and three give ~99%; at p = 1/3 it takes six for ~91%. `regress -k N`
   already exists, so the machinery is there: what is missing is deriving N per world from the rate
   the corpus stored.

   Note the direction: at p = 0.78 the ANY-of-N test needs 2–3 replays to be reliable, while the
   current ALL-of-3 test is a coin flip. The gate is not merely stricter than necessary: it is
   strict in a way that costs the same worlds and yields a less reliable answer.
3. The verdict distinguishes "did not reproduce in N replays at recorded rate p" (evidence the
   defect is fixed, with a stated confidence) from "reproduced", which is the honest version of the
   PASS a single replay currently reports.

**Why not now.** It changes what a corpus entry MEANS and what `regress` exit 0 CLAIMS, both of
which are normative surface. It also needs a decision on the confidence bound, and that bound is the
same quantity OQ-053 needs for `AcceptTrials`: the per-target reproduction probability. The two
should be decided together, from one measurement, rather than separately from two guesses.

**Interim.** Phase 5's evidence reports the measured rate rather than asserting a gate passed. The
acceptance criterion for fault reduction is met and demonstrated; the corpus commit is not, and the
reason is recorded here rather than worked around by lowering `-k` until the gate happened to open.

**Update 2026-09-19: a post-D-071 RUN sample, which is a different experiment from the REPLAY
measurement above.** The 7/9 here is replays of ONE committed world: n attempts at the same plan,
k reproducing the same violation. What follows is `thesis run` over FRESH worlds (twenty sequential
runs of `--profile linear --fault 'net.partition(role:leader)@3000..7500'` with `--seed 1` through
`--seed 20`, so twenty distinct plans, one world each) on binaries stamped `commit 14b8b0f dirty=0`
(clean tree, D-072 stamps), against `prothesis/kvfixture:buggy`, which compose rebuilt before
EVERY world (OQ-068), so the twenty `sut.images` digests are twenty distinct image IDs for one
unchanged Dockerfile; read the SUT as one program under twenty names. Every exit code was read in
the launching shell on the line after the call.

Measured: **20 of 20 FAIL, exit 1, one world each**, wall 28.3–35.1 s, and **all twenty world files
record DRIVE spanning PERTURB**, the D-071 shape, now demonstrated across a sample rather than by
one artifact. OBS-LIVE-002 the day before was also `--seed 1` (the identical world seed
2337997644773862495, so the same plan as r_2026_09_19_76c5) and also failed: 21 observations over
20 DISTINCT plans, the 21st a repeat of one of them. Run ids, so the claim is checkable while the
gitignored run directories survive: r_2026_09_19_76c5, 561c, a514, 9aa1, 5a5a, 7d33, b05c, 3ab5,
74c3, d080, 323e, 12ec, aa54, 8310, b4d9, 5a7d, 704a, 605f, f1f2, c659.

What that does and does not say. At the replay rate of 0.78, twenty successes in a row has
probability 0.78²⁰ ≈ 0.7%; the exact one-sided 95% lower bound on the RUN rate from the 20 distinct
plans is 0.05^(1/20) ≈ 0.86 (the seed-1 repeat is not counted twice). So the run-level rate on this
code and fixture is very unlikely to be as low as 0.78, but the two experiments differ in more than
one way at once (fresh plans versus one replayed plan; a rebuilt image, and per OQ-068 a different
image id for every world; the D-071 lifecycle; BOOT 14.9–21.1 s here, median 16.1 s, overlapping
OBS-LIVE-001 run 1's 22.6 s at the top), and nothing here isolates which. One difference is closed
rather than open: the replay batch pinned `kv-n2` by name, and these runs target `role:leader`,
which resolved to kv-n2 in all twenty. It is not evidence of determinism: no finite sample is. It IS the first
stratified post-D-071 sample L3 can be designed against, and its unit of observation is the world,
recorded per world in each run's `result.json`.

---

## OQ-055: no world has ever recorded `sut.images`, so a committed regression cannot say what it reproduced against

**Classification:** RESOLVED 2026-09-17 (D-070). `harness.ResolveImages` inspects every bound
container at BOOT and records (logical service, image reference, resolved image ID) into
`World.SUT.Images`, deduplicated per distinct build; resolution failure degrades to the honest `[]`,
never blocks the world. Verified live: the first run with the resolver recorded
`prothesis/kvfixture:buggy` at its image ID for all three fixture nodes. The migration warning below
still stands for the historical corpus: worlds written before this date keep their old hashes and
their empty `images`; only new worlds carry digests.

_The original entry is kept verbatim below, because the corpus worlds it names still exist with
empty `sut.images` and the reasoning is still why the field exists._

**Observed.** The world committed by the Phase 5 acceptance shrink:

```json
"sut":{"images":[]}
```

and every world this repository has produced says the same, back to the Phase 3 definition-of-done
run:

```
r_2026_09_08_8400/world-0001   sut.images: 0
r_2026_09_09_9d07/world-0001   sut.images: 0
r_2026_09_09_fa45/world-0024   sut.images: 0   <- the committed regression
```

**This is legal and it is honest.** `World.Validate` rejects a NULL images list with
`"is null; write [] if no image digests were resolved"`, and accepts `[]`. The schema deliberately
distinguishes "we failed to record" from "we recorded that none were resolved". Nothing here is
lying; the field is empty because the resolution step does not exist, and the artifact says so.

**Why it matters anyway.** OQ-011 is the reason a world carries `sut` at all: a reduction executed
against a different build is not a reduction of anything. The regression corpus is where that bites
hardest. `w_e952.thesis` reproduces the injected Raft lease defect, and the file cannot say which
image it reproduced against, so a future green result from `thesis regress` is ambiguous between
"the defect is fixed" and "this ran against a different build".

Phase 5's own acceptance differential is the demonstration:

```
thesis regress             -> FAIL, reproduced 3/3   (KV_VARIANT unset = buggy)
KV_VARIANT=kvfixed regress -> PASS, reproduced 0/3
```

That is a clean result and it is only interpretable because the operator knew which variant was
running. The artifacts do not record it. A reviewer reading the corpus in six months has the fault
schedule, the seed, the phase timings and the world hash, and no way to tell that the green run and
the red run were different builds rather than the same build behaving differently, which is exactly
the ambiguity `sut` was frozen into the schema to remove.

**What is missing.** The harness knows the answer at BOOT: `docker inspect` on each bound container
yields `Image` and `RepoDigests`, and `recorder.Topology` already carries the container ids to ask
about. Nothing resolves them into `World.SUT.Images`.

**Not fixed here** because it changes what every world file contains: the digest lands inside the
canonically encoded, content-addressed world, so every existing `world_hash` moves at once, and the
committed corpus and any golden files move with it. That is a migration with a review cost, not a
patch, and it should be done deliberately rather than folded into a phase that was closing.

---

## OQ-056: `thesis search` drove every world against ports nothing was listening on, and said only "unknown"

**Classification:** RESOLVED in Phase 5, two ways. Logged because the failure was silent for two
phases and every Phase 4 result depended on remembering a flag.

**Observed.** A bare `thesis search` on the reference fixture, with no `--worker-env`:

```
world 1 [control] 0 fault(s) -> unknown
  linearizable.kv could not answer, so a clean result cannot be claimed

history.jsonl: 78,018 operation record(s) over 39,009 op_id(s), none checkable:
               39,009 failed, 0 indeterminate reads, 0 ok reads with no value
first failure: Get "http://127.0.0.1:19001/kv/k/5": dial tcp 127.0.0.1:19001:
               connectex: No connection could be made because the target machine
               actively refused it
```

(run `r_2026_09_09_23c5`.) The cluster was healthy the whole time: kv-n2 won an election at term 1
and the containers passed their health probes. Nobody was listening on 19001 because compose had
published 18082.

**Cause.** The parallel executor allocates a per-slot port band and exports it TWICE, for two
different consumers:

  * to the DRIVER as `PROTHESIS_TARGETS`, which it always does; and
  * to COMPOSE under whatever variables the topology file spells, which it can only do if someone
    tells it what they are called.

That second mapping lived exclusively in a `--worker-env` CLI flag. Omit it and the halves disagree:
the driver dials the slot's band, compose publishes the file's defaults, every operation fails, and
the history is full but unjudgeable. The oracle then reports INCONCLUSIVE (correctly, it has
nothing to check) and the Saboteur reads that as "the fault I injected told me nothing" and
declines to reinforce, so the whole search degrades to corpus mutation over an empty tree.

Every Phase 4 result came from `scripts/a9-benchmark*.ps1`, which pass the flag. A bare invocation
was broken for two phases and the only symptom was the word "unknown".

**The comment in `parseWorkerEnv` claimed a safety net that does not exist.** It said a compose file
not spelling those variables "simply publishes the same ports for every worker, and the executor's
own port check refuses to start rather than letting two worlds share a cluster". That check
(`ParallelOptions.VerifyPortsFree`) catches two slots COLLIDING on a port, which requires two slots.
A single worker collides with nobody and sails straight into the mismatch, which is exactly the
configuration a first-time user runs.

**Fixed twice, deliberately.**

1. **The mapping moved into config.** `harness.prefix_env` and `harness.nodes[].port_env` carry it,
   next to `compose_service`, because it is a fact about the project's topology file rather than a
   per-invocation choice. `--worker-env` still wins when given. A bare `thesis search` on the fixture
   now runs correctly and finds violations.
2. **BOOT checks the two halves agree.** `control.portsAgree` compares the ports the driver will be
   TOLD about against the ports the harness OBSERVED published, and refuses the world before
   anything drives: naming each mismatch and the remedy. This is what protects a project that
   declares no mapping, which config alone cannot.

Belt and braces on purpose: config removes the footgun for this project, the BOOT check removes it
for every other one.

**Not covered by the oracle lock, and that is correct.** The lock covers `oracles.*`,
`perturber.*`, `profiles` and `search`. `harness.*` is outside it, so declaring the mapping cost no
gate change and the fixture's digest is byte-identical.

---

## OQ-057: the oracle lock does not cover the program that decides the verdict, and that is exploitable end to end

**Classification:** OPEN; the most serious weakness the adversarial suite found. Known and
documented as a limitation since Phase 3; MEASURED for the first time here.

**The claim under test.** `.prothesis/oracles/linearizable.kv.yaml` states its own worst limitation
in plain words, and deserves credit for it:

> HONEST LIMITATION, stated rather than implied away: the lock hashes THIS DEFINITION, not the
> executable `cmd` names. Swapping `./bin/linearizable-kv` for a program that always prints `ok`
> does not move the digest. That is the verdict-signing concern (v3).

A limitation written in a comment is a claim. Probe A7 of `scripts/adversarial.ps1` performs the
swap and asks whether the claim is true, and whether it matters.

**Measured.** `bin/linearizable-kv.exe` replaced with nine lines that drain STDIN, print a
well-formed `prothesis.oracle_output/v1` carrying `"status":"ok"`, and exit 0:

```
thesis oracles verify                        -> exit 0, "oracle lock OK: sha256:5363...a774f"
thesis run --profile linear --worlds 1 \
     --fault net.partition(role:leader)@3000..7500
                                             -> exit 0, PASS
```

That world reproduces the injected Raft lease defect roughly 78% of the time (measured 7/9,
OQ-054). Under the stub it came back green: no drift, no warning, no INCONCLUSIVE. The original
binary was restored and independently verified byte-identical by SHA-256, and the restored checker
was re-run to confirm it reproduces again: a probe that replaces the program deciding every verdict
has to prove it put the real one back, not assume it.

**Why this is the sharpest weakness in the system.** The oracle lock is the anti-gaming device
everything else leans on. Invariant I6 fails CLOSED on drift; `thesis oracles lock` demands a written
reason; the digest is committed and reviewable. All of that governs what PRO-THESIS *reads* and none
of it governs what PRO-THESIS *executes*. The gate is bolted to the door frame and the wall is
plasterboard.

The threat is not exotic. Anything that can write into the target's `bin/` (a build script, a stale
artifact from an earlier branch, a dependency, or an agent under pressure to produce a green run)
converts every red verdict this system has ever produced into a green one, and the run reports
nothing unusual at all. It is precisely the failure mode the lock was built to prevent, reached by
going around it.

**Why it was not simply fixed by hashing the binary.** Hashing the executable into the lock digest
would make the digest move on every rebuild of the checker and on every platform (the fixture's
`.exe` suffix is supplied per-host), so a routine `go build` would produce ORACLE_DRIFT and the
correct response would become "re-lock without reading the diff", which trains exactly the reflex
the lock exists to prevent. That is a real objection and it is why the limitation was documented
rather than closed.

**Proposal, not implemented here.** The lock already has a precedent for exactly this shape of
problem: `builtin_options_fingerprint` records the compiled-in oracle thresholds OUTSIDE the digest,
and `thesis oracles verify` reports a moved fingerprint as a WARNING rather than as drift, for the
same reason, including it would fire exit 4 on every downstream project whenever a default changed.

Apply the same treatment to external oracle executables:

1. `thesis oracles lock` records `sha256` and size of each resolved `cmd` target under an
   informational `executables` block, outside the digest.
2. `thesis oracles verify` and every run report a moved or missing executable hash as a WARNING
   naming the oracle and both hashes.
3. A run whose verdict is PASS and whose executable hash has moved says so in the verdict, so a
   green result carries the fact that the thing which produced it changed.

That keeps the digest portable and rebuild-stable while making a swapped checker impossible to miss.
It does not make the swap impossible (nothing short of signing does) but it removes the silence,
and silence is what makes this exploitable rather than merely possible.

**Related.** OQ-054 records that `MinimalRepro.Reproduced` already carries a measured k/n, so the
verdict has somewhere honest to put "this was checked by a binary we did not recognise".

**Update 2026-09-17 (D-072): the warning had become unconditional, which is its own failure mode.**
D-060 implemented the proposal above, and then D-070 stamped a wall-clock build time into the very
binary the `executables` block fingerprints. Every rebuild of unchanged source therefore produced a
new digest and a new warning, and the lock was re-locked three times in one day: twice under reasons
naming no cause at all. A warning that fires on every build is indistinguishable from noise, so the
mechanism that removes the SILENCE this entry measured was, for a day, training its reader to dismiss
it. D-072 removes the wall clock (the stamp is HEAD's committer date) and adds `-buildvcs=false`,
without which Go's own VCS stamping moved the digest whenever a stray untracked file flipped
`vcs.modified` on a clean tree. Measured via `scripts/build.ps1 -Verify`: two builds of one tree on
one machine are byte-identical, and a stray untracked file no longer moves them.

**What remains open here.** Three things, none closed by D-072. First, the fingerprint is still
outside the digest, so a swapped checker is a warning and never ORACLE_DRIFT: the original finding
stands unchanged. Second, a commit that touches nothing the checker compiles still moves its `Commit`
stamp, so a rebuild after any commit costs one re-lock; stamping each binary with the last commit
touching its own dependency set would remove that residual, at the cost of two binaries built together
reporting different commits. Third, "byte-identical" is scoped to this machine: `-trimpath` removes
absolute paths, but the Go toolchain version (unpinned; `go.mod` carries no `toolchain` directive),
`GOFLAGS`, `GOAMD64` and `CGO_ENABLED` all change the bytes and none of them is pinned, so a digest
recorded here is not reproducible by a downstream consumer on a different toolchain. Until that is
addressed, the `executables` block is a local-tree fact, and a consumer who rebuilds and sees a
different digest has learned nothing about whether the program changed.

---

## OQ-058: the history's two record kinds are told apart by an unvalidated field, and both readers dropped operations silently

**Classification:** RESOLVED in the product. Found by external audit, reproduced against the real
binary BEFORE any change was written, and mutation-verified after.

**The defect.** A history file carries two kinds of record: driver-emitted operations and
harness-emitted markers, merged into one stream by `MergePhaseMarkers`. The only thing separating
them is `RecordKind()`, which tests `e.Event != ""` and nothing else. Every reader then skips
markers. So an OPERATION record carrying a stray `event` is classified as a marker and discarded,
not counted, not reported, not visible in the verdict.

`HistoryEntry.Validate()` already existed, already enforced the union, and was called by neither
reader. Its own doc comment explains why the reader does not call it ("one malformed line must not
abort a 500,000-line read: the caller decides the policy"). The policy the callers chose was to
never ask.

**Measured before the fix**, four histories through `bin/linearizable-kv`:

```
genuine stale read among 120 background ops        -> violated,  exit 1   (correct)
same, "event":"x" on the stale read's two records  -> ok,        exit 0   (SILENT ACQUITTAL)
a LINEARIZABLE history, "event":"x" on its txn     -> violated,  exit 1   (FALSE CONVICTION)
same history, field removed                        -> inconclusive, exit 2 (correct refusal)
```

Both directions matter, and the third line is the worse one. The checker emitted a witness naming
three op ids and the words *"this is a WITNESSED failure, not a timeout and not a budget"* about a
history it had never finished reading. It also defeats the multi-key refusal `main.go` calls
unconditional: hiding the transaction removes it before the refusal can see it.

**The second reader.** `internal/oracle/no_stuck_op.go:96` performs the same unvalidated skip.
Tagging both records of an operation removes it from that oracle's view entirely, and an operation
the oracle cannot see is one it cannot report as stuck. Mutating the fix back out reproduces it:

```
a stuck operation was hidden by an event field and the oracle passed:
  "all 1 operation(s) completed within SLO ceiling (5000ms) after HEAL"
```

**Fixed at two seams**, deliberately not three. See D-058.

1. `pkg/schema/history.go`: `Validate()` now also rejects a marker carrying `op_id` or `value`.
   `f` was already covered, and `f` alone catches every record a driver would actually emit; the
   other two are the fields that make a record *evidence*, and they close the shape where a record
   is malformed but the existing rule happens to accept it.
2. `cmd/thesis-oracle-linearizable/load.go`: validates every entry BEFORE kind dispatch and
   refuses the history. An external checker reads the file itself and cannot inherit anything.
3. `internal/oracle/input.go`: `ReadHistory` counts a union violation as `InvalidRecords`, does
   not append the record, and `Reliable()` is false while any exist. Every built-in oracle already
   refuses over an unreliable history, so `no_stuck_op` and every future consumer inherit the fix
   without touching the oracles themselves.

**What this does NOT close, stated rather than implied away.**

- **A third-party external oracle is still free to skip without validating.** The contract can
  publish the union rule; it cannot enforce it inside somebody else's process. This is the same
  boundary as OQ-057: we govern what PRO-THESIS reads, not what an external program does with it.
- **`key` on a marker is still accepted.** It is not an exploit vector: hiding an operation
  requires the record to be dropped, every real operation record carries `f`, and `f` is refused.
  Left uncovered rather than widened on speculation, and recorded here so the choice is visible.
- **The residual is a driver that emits an event on an operation record BY ACCIDENT.** That
  history is now INCONCLUSIVE rather than silently wrong, which is the correct direction, but it
  is a real cost: such a driver's runs stop producing verdicts until it is fixed. The refusal
  message names the field and both failure modes so the cause is not a mystery.

**Related.** OQ-057 is the same class one layer out: the lock governs what is read, not what is
executed. This one was the lock governing what is read, and the reader not looking.

---

## OQ-059: a lock-covered gate could be cut to a thirtieth of itself from argv, and the verdict could not tell

**Classification:** RESOLVED in the product. Found by external audit. The defect is not in the
implementation (the code did exactly what D-033 specified) it is in D-033's reasoning.

**Observed.** `profiles.gate.worlds: 30` is lock-covered, which is the whole reason `profiles` is
in the lock's `covers` list. And:

```
thesis run --profile gate --worlds 1
  -> runs 1 world
  -> exit 0, verdict PASS
  -> oracle_lock: ok          (true: no FILE changed)
  -> budget: "1 worlds run / 1 planned"
```

No file was edited, so there was no drift to detect. The lock exists to stop someone editing `30`
to `1`; `--worlds 1` achieves the identical effect and was not covered by anything.

**Three aggravating factors, each measured.**

1. `runner.go` consulted only `tracker.WallExpired()` when deciding whether a clean run was a PASS,
   so exhausting the WORLD budget without a violation was indistinguishable from a completed gate.
2. `budget.Snapshot()` wrote the ALREADY-NARROWED value into `worlds_planned`. The profile's own
   requirement appeared nowhere in the artifact. Recovering it meant opening `prothesis.yaml` and
   cross-referencing `verdict.profile` by hand, and nothing prompted anyone to.
3. `engine.resolveBudget` was a hand-copied duplicate of the same rule, so `thesis search` had the
   defect twice over and a fix to one copy would not have been found by reading the other.

**The reasoning error, quoted.** D-033 contains both halves of the contradiction four lines apart:

> A profile's budget is lock-covered precisely because **shortening it is a gate-weakening move**
> (CRUCIBLE §E.2). ... **Narrowing is safe** in the direction that matters: it can only make the
> gate harder to pass.

Both cannot be true. The second sentence is the one that is wrong: "harder to pass" is the right
intuition for a budget you must FIT WORK INTO, and the wrong one for a SEARCH. Fewer worlds is
fewer chances to find the defect. On this fixture the reference world reproduces roughly 78% of the
time, so 30 worlds finds it essentially always and 1 world finds it 78% of the time, and for a
rarer defect at 5% per world the gate falls from 79% detection to 5%.

**Fixed** per D-059: the narrowing is recorded, a narrowed clean run reports INCONCLUSIVE rather
than PASS, and `oracle_lock` reports `bypassed` rather than `ok`.

**What this does NOT close.**

- **The flags still narrow.** They were not removed, because a narrowed run is genuinely useful
  while developing and a violation found in one world is still a violation: it still exits 1. What
  changed is that a narrowed run can no longer come back GREEN.
- **A deliberate conservatism.** Capping an UNBOUNDED world count is treated as a narrowing
  whatever the number, so `--worlds 9999` against a `worlds: -1` profile is recorded as one even
  though a 30m wall budget would never reach it. Nothing at budget-resolution time knows how many
  worlds fit in the wall budget. The cost is small in practice (an unbounded profile that ends on
  wall expiry reports budget exhaustion rather than PASS anyway) and the error is in the
  fail-closed direction.
- **`--seed` is not covered by any of this.** Pinning a seed narrows the sampled space as surely as
  narrowing the world count does, and it is not recorded as a narrowing. Whether it should be is a
  real question and is deliberately left open rather than answered in passing.
- **The lock still does not COVER the flags.** `bypassed` is a report, not a refusal. An agent that
  wants a weaker gate can still have one; it can no longer have one silently, and it cannot call
  the result a PASS. Refusing outright was considered and rejected: see D-059.

**Related.** OQ-026 is the same shape inside the config (`driver.profiles` is a gate-weakening
vector the lock does not cover). Both are cases of the digest being an honest statement about a
file while the gate that ran was decided somewhere else.

---

## OQ-057 MITIGATED, NOT RESOLVED (D-060): the swap is still possible; it is no longer silent

**Classification:** the original entry stands unedited above, including its measurement. This
records what D-060 changed and, more importantly, what it did not.

**What was implemented.** OQ-057's own three-point proposal, in full:

1. `thesis oracles lock` records each external oracle's resolved program under an `executables`
   block (oracle name, the `cmd` verbatim, the path it resolved to, SHA-256 and size) OUTSIDE
   the digest.
2. `thesis oracles verify` and every `thesis run` compare the current program against that
   fingerprint and report a moved, missing or newly-appearing one as a WARNING.
3. The verdict carries `oracle_lock.executables_moved`, so a PASS produced by a checker that
   changed says so in the artifact rather than only on someone's terminal.

**Measured, end to end, on the fixture.** The stub from the original entry, rebuilt and swapped in:

```
thesis oracles verify  -> exit 0, "oracle lock OK: sha256:fcfa3746...3891"
                          thesis: WARNING: 1 oracle program(s) differ from the lock:
                            linearizable.kv: bin/linearizable-kv.exe changed
                                             (3f18775f9a2c -> fdf646651415)
thesis oracles verify --json
                       -> the same fact as "executables_moved"
restore                -> verified byte-identical by SHA-256, then verify: exit 0, no warning
```

**Exit 0 is still correct and is not a partial fix.** Hashing the binary INTO the digest was
considered and refused for the reason the original entry already gives: the digest would move on
every rebuild and on every platform, an ordinary `go build` would produce ORACLE_DRIFT, and the
correct response would become "re-lock without reading the diff"; training exactly the reflex the
lock exists to prevent. Reporting is the strongest thing available that does not do that.

**What remains open, stated so it cannot be mistaken for closed.**

- **A swapped checker still produces a green run.** The warning is on stderr and in the verdict;
  the exit code is unchanged. A CI gate that branches only on the exit code learns nothing. That is
  a deliberate trade (D-060) and it is the residual risk.
- **Only the PROGRAM is fingerprinted.** Not its arguments, not its interpreter, not a shared
  library or container image it loads at run time. `cmd: python check.py` fingerprints nothing at
  all, and records that it fingerprinted nothing.
- **The fingerprint is taken where PRO-THESIS looks, and PRO-THESIS looks in the project directory
  first.** `driver.ResolveProgram` stats `<projectDir>/<prog>` before falling through to PATH, so a
  file planted in a cloned repository is BOTH what executes and what gets fingerprinted. The
  fingerprint is then consistent and useless against that attack. This is a separate defect, is not
  introduced by D-060, and is logged as **OQ-061**.
- **Nothing is signed.** A fingerprint recorded by the same run that could have been compromised is
  evidence for a reviewer, not an attestation.

**Probe A7 changed shape.** It was previously a pure measurement that recorded PASS whichever way
the measurement came out, so an exploitable hole appeared in the summary table as a green row: the
suite's own header forbids exactly that ("a probe that can only pass is theatre"). A7 now asserts
detection (A7c) and FAILS when the swap goes unreported; it also verifies its restore by SHA-256,
which the original write-up claimed and the script did not do. It no longer needs Docker for the
detection half.

---

## OQ-060: D-015 predicted the tree would start earning its place at ~62 rollouts; no recorded run has ever tested that

**Classification:** OPEN. This does not report a defect: it reports that a mitigation recorded in
D-015 has never been measured, and that the analytic prediction D-015 made is still the only
evidence for it.

**What was already known.** D-015 states the equivalence plainly and states it correctly: at
Addendum A's budgets, "UCT is *provably equivalent* to ladder-ordered enumeration; the exploration
term is infinite at zero visits, so unvisited children are always selected first, no node is ever
visited twice, and backpropagation never influences a single decision." `uct.go`'s own
`SelectionForced` doc says the same: "This is the only kind that occurs at Addendum A's budgets."
The arithmetic is in OQ-013.

So the architectural claim was downgraded honestly and in advance. Nothing here contradicts it.

**What had never been done is checking it against the runs.** All 38 committed `search.json`
records, 30 of them Saboteur:

```
informed_selections, ALL 30 runs                    0
forced_selections,   ALL 30 runs                   55
tree_contributed                       false in 30 / 30
runs that built an escalation tree at all      5 / 30
max_visits reached by ANY node in ANY tree          7
```

The prediction holds exactly. `U_avg` (the backpropagated utility the whole MCTS layer exists to
accumulate) has never been read to make a single decision, in any recorded execution of this
system. What guides the search is `ladder.go`'s hand-written escalation order plus `betterUCT`'s
parsimony tie-break among the infinities.

**The part that is genuinely open.** D-015 does not stop at the equivalence; it forecasts an exit
from it:

> with the measured 3.8× parallel speedup the rollout budget rises to ~62, where the tree does
> begin to earn its place.

That forecast is **untested, not refuted**, and the distinction matters:

- The largest Saboteur run that built a tree at all ran **40** worlds (below the threshold) and
  still recorded 0 informed selections, which is consistent with the prediction rather than against
  it.
- Exactly **one** Saboteur run reached the threshold: `r_2026_09_08_942d`, at 72 worlds. It built
  **zero** trees, because all 72 worlds came back unevaluable (the OQ-056 class). The one run that
  could have answered the question measured nothing.

So the corpus contains no observation of the tree operating under the conditions D-015 says it
needs, and the single run that got there failed for an unrelated reason.

**What would settle it.** A Saboteur run of ≥62 EVALUABLE worlds (the qualifier is the whole
difficulty, given the unknown rates in OQ-056) reporting `informed_selections`. Either the
selections appear, and the tree is doing something the ladder cannot, or they do not, and the tree
should be removed rather than tuned. Until then no claim in either direction is supported, and in
particular **"MCTS-guided search" must not be stated as a property of this system**: what is
demonstrated is a fixture-derived escalation ladder, whose default timings came from D-031's
measurements of this specific defect.

**Related.** OQ-013 has the arithmetic. OQ-056 is why the one qualifying run measured nothing.
OQ-045 records that A.9's criterion #1 is unfalsifiable on this fixture class, which is the same
problem one level up: the benchmark cannot distinguish the components it is comparing.

---

## OQ-061: the program PRO-THESIS fingerprints is the one a cloned repository can plant

**Classification:** RESOLVED by D-061. Found while implementing D-060 and reported rather than
fixed in passing, because the fix changes how every driver and oracle command resolves. The
original statement of the defect stands below; the resolution is at the end.

**Observed.** `internal/driver/supervise.go:408` (`resolveProgram`, exported as `ResolveProgram`)
stats `filepath.Join(dir, prog)` and `prog + ".exe"` BEFORE falling through to `PATH`. It is
reached by the driver (`supervise.go`) and by every external oracle
(`internal/oracle/process.go:280`, with `dir` = the project directory).

So a repository that ships

```yaml
cmd: "python ./check.py"
```

alongside a file named `python.exe` at its root runs the repository's binary. A reviewer reading
the config sees system Python. This is what Go 1.19 removed via `exec.ErrDot`, re-introduced.

**Why it matters more after D-060 than before.** The executable fingerprint is taken through the
same resolver, by design: a fingerprint of a different file than the one that will run would be a
false assurance. That is the right choice and it means the fingerprint agrees with the planted
binary: consistent, recorded, and no help at all. D-060's warning defends against a checker
REPLACED after locking. It does not defend against one that was never the right program.

**Not covered by the lock**, because the planted file sits outside `oracles.dir`.

**Proposal, not implemented.** Require an explicit `./` prefix for a project-relative program,
matching Go's own post-1.19 rule, and resolve a bare name through PATH only. `cmd: "./bin/checker"`
(the form the fixture already uses and the form the definition's own comment documents) is
unaffected. The migration cost falls on configs relying on the implicit behaviour, which should be
named in the error rather than silently changed.

**Related.** This is the same shape as OQ-057 one layer further out: the lock governs what
PRO-THESIS reads, D-060 governs what it executes, and this governs which file "what it executes"
resolves to.

**RESOLVED (D-061).** A bare argv[0] (one containing no path separator) is now a PATH lookup and
is never resolved against the project directory. A program containing a separator still resolves
against it, with the `.exe` suffix still supplied.

The rule chosen is Go's own and the shell's, rather than the `./`-prefix requirement proposed
above: a separator means "a path", no separator means "look it up". That choice costs nothing and
breaks nothing here, because **both forms this repository ships already carry a separator**:
`./bin/loadgen`, `./bin/linearizable-kv`, and `thesis-helpers/steady.sh` in the golden config. The
`./`-prefix proposal would have broken the third.

The existing `TestResolveProgram` encoded the vulnerability as intended behaviour, and its own
stated rationale did not support it: the case asserted that a BARE `loadgen` resolves inside the
project, and justified that with *"driver.cmd's ./bin/loadgen is relative to the directory
prothesis.yaml lives in"*; an example carrying a separator. The test was rewritten rather than
deleted, and gained a case that plants both `python` and `python.exe` in the project directory and
requires a bare `python` to resolve to neither. Mutation-verified: neutering the guard makes
`resolveProgram("python", dir)` return the planted file, and that case fails.

**Still not covered**, so the boundary stays visible: what a resolved program LOADS at run time
(an interpreter reading a script from the repository, a shared library, a container image) is
neither resolved nor fingerprinted here. `cmd: ./bin/checker` is now unambiguous about which file
runs; it says nothing about what that file reads.

---

## OQ-062: corpus.List silently dropped unparseable filenames, and never bound short IDs to content hashes

**Classification:** RESOLVED by D-063.

**Observed.**
1. `internal/corpus/corpus.go:List()` parsed `shortID` from `w_xxxx.thesis`, loaded the world, computed `HashWorld(w)`, and never checked whether `shortID` matched the hash. A file named `w_1111.thesis` carrying `w_e952` was loaded and replayed without error under the wrong identity.
2. In the same loop, any directory entry failing `schema.ParseWorldFilename` was skipped via `continue`. If a regression file was named `w_bad.thesis`, `world-0001.thesis`, or had uppercase hex, it was silently ignored. If all files in `.prothesis/regressions` failed parsing, `thesis regress` returned exit 0 ("there are no regressions: holds no worlds"); a vacuous green pass over a corrupt or missing corpus.
3. In `internal/shrink/stages.go:85`, Guardrail #3 promised that calibration evidence comes *only* from the unreduced world, while `probe.go:506` fed candidate divergences from Stages 1–3 into `maybeCalibrate()`. The code made a deliberate trade-off to avoid false-negative rejections of valid reductions (OQ-047), but left an untruthful comment in `stages.go`.

**Fixed (D-063).**
1. `Corpus.List()` checks `schema.ShortIDFromHash(h) == shortID` and reports a mismatch as
   `Entry.Err` with no `World`, so nothing downstream can execute it.
2. A name that claims to be a world (`w_` prefix, or `.thesis` anywhere in it in any case) and
   fails `ParseWorldFilename` is an `Entry.Err`, not a skipped file. A first draft tested only the
   `.thesis` suffix and still dropped `w_A41F.thesis` and `w_e952.thesis.orig` silently; both are
   now in the test. `thesis regress` REFUSES a corpus with any such member (exit 2) before
   allocating a run bundle.
3. Item 3 was NOT fixed by rewording guardrail 3, though a first draft of this entry said so and
   labelled the result `retain_bounded`. The reworded comment described the trade-off honestly and
   added no bound: a reduced candidate could still relax the level in its own round, and the
   panel's attack test failed against that draft with the exact Calibration note the attack
   produces. The bound is now real: divergences carry provenance, `keyDrift` consults only
   unreduced evidence, and a reduced candidate's drift triggers at most `Policy.CalibrationProbes`
   re-executions of the unreduced world, which alone may calibrate. Measured: two draws plus four
   re-probes miss a real drift 16% of the time on this fixture, against 55% for stage 0 alone
   (the draft's "~45%" was the complement, stated beside a parenthetical computing 0.55).

**Still open.** `thesis regress` now halts on one damaged member rather than running the rest; a
per-member INCONCLUSIVE with the intact members still replayed would carry more information at the
same exit code, and was not chosen because a corpus is a committed gate artifact and a partial
verdict over one reads as a verdict. Revisit if it proves to be the thing operators route around.

---


## OQ-063: the first third-party target found a hard-coded probe path, and three harness features assume an endpoint or a shell the target does not have

**Classification:** PARTIALLY RESOLVED. Item 1 is fixed (D-065) and its evidence was widened
(D-066); item 4 is fixed (D-066, `harness.role_probe`). Items 2–3 are open: stated limitations of
the harness against a shell-less, status-less image, each with a documented workaround and a
recorded design (see the update at the end of this entry).

**Context.** `targets/etcd/` is the first system under test that is not the reference fixture
(D-065). Its image ships no shell and no `/status` endpoint. Everything below was found by pointing
the unmodified harness at it, and nothing below could have been found by the fixture, because the
fixture IS the assumption each item encodes.

**Observed.**
1. **The post-HEAL convergence probe was hard-coded to the fixture's path.** Run
   `r_2026_09_15_ace7`: every world FAIL on `availability_after_heal`, "3 nodes failed to become
   available after HEAL … (HTTP status 404)" on every member, while `telemetry.jsonl` shows the
   DECLARED probe, `http://127.0.0.1:<port>/health`, answering 200 on all 39 samples. `runner.go`
   built the probe the oracle judges from the literal `"http://{host}:{port}/healthz"` rather than
   from the node's `harness.health` entry. Against the fixture the two strings are equal, so no run
   before this one could notice. The oracle judged a URL the config never declared, and returned a
   confident FAIL on a healthy cluster.
   **Fixed.** `convergenceProbes` (`internal/control/converge.go`) resolves each node's template
   through `harness.ProbeTemplates` (the resolution BOOT and telemetry already used) and a node no
   entry covers gets NO observation rather than an invented URL, so the oracle reports it unprobed
   and returns INCONCLUSIVE. Pinned by `TestConvergenceProbesUseTheNodesOwnHealthTemplate`;
   mutation-verified by restoring the literal path ("was probed at …/healthz; the declared template
   ends in /health").
2. **`no_unbounded_queue` needs `/status`.** The telemetry collector polls
   `http://{host}:{port}/status` on every node (`telemetry.StatusPath`) for `queue_depth` and
   `goroutines`. etcd answers 404, the metrics are marked absent, and the oracle returns
   INCONCLUSIVE on every world; correct and fail-closed, and it means the oracle cannot run against
   any target that does not implement the fixture's status document. Workaround: leave it out of
   `oracles.builtin`, as `targets/etcd/prothesis.yaml` does, with the reason in a comment. A fix
   needs either a target-declared metrics endpoint in `prothesis.yaml` or a Prometheus scrape (etcd
   exposes `/metrics`); both are new config surface the lock would have to cover.
3. **`resource_return_to_baseline` needs a shell in the container.** RSS, file-descriptor and
   thread counts are read by exec'ing a script inside the container (`collector.go`, `ExecScript`).
   A distroless image has nothing to exec; every metric is absent; INCONCLUSIVE on every world.
   Same workaround. A fix would read `/proc/<pid>` from a `--pid container:` sidecar, the way the
   `io.*` family already does.
4. **`role:leader` cannot bind.** Role resolution derives the leader from the `/status` document.
   Without it a `role:` target does not resolve, so the etcd pre-registration names a member id.
   etcd's `/v3/maintenance/status` carries the leader id and would serve; wiring it is target-specific
   config, again under the lock.
5. *(Recorded for the next target author, not a defect.)* The directive's steady-state shape,
   `docker compose exec -T <svc> <probe>`, needs a shell or a probe binary inside the image.
   `targets/etcd/cmd/etcdsteady` is the alternative: a host-side probe reading `PROTHESIS_NODES`,
   `PROTHESIS_HOST` and `PROTHESIS_PORT_<NODE>` from the environment the harness already exports.
   `clock.skew` is denied for this target because the image lacks util-linux's `unshare --time`;
   the injector's own precondition check would refuse it at injection time.

**What this says about the "second target" claim.** A real system was onboarded with one code
change to the harness, and that change was a fixture assumption the fixture could not have found.
The two pre-registered arms, the verdicts, and the re-derived mechanism, which was not the
pre-registered one, are in D-065 and `targets/etcd/README.md`.

**Shrink of arm B's world, measured (OQ-052's acceptance half, on a real target).**
`thesis shrink .prothesis/runs/r_2026_09_15_9a29/world-0001/world.thesis --budget 8m --world-budget 16`:

| Stage | Result | Worlds | Note |
|---|---|---|---|
| baseline | 1 → 1 | 2 | reproduced `linearizable.kv` key `k/0`; witness key held on re-execution |
| faults | 1 → 0 | 1 | **the shrinker dropped the partition**, agreeing with the revision re-derivation above |
| ops | 16,788 → 2,098 | 10 | STOPPED, world budget exhausted; partial |
| narrow | n/a | 0 | skipped, earlier stage stopped |
| confirm | **1/3** | 3 | two replays failed on a DIFFERENT key (`k/1`); Rule 1 rejects |

Exit 3, BUDGET_EXHAUSTED, nothing committed. Three things this measured. The etcd driver's
OPERATION MODE was exercised by the harness for real: plans of 8,394, 4,197 and 2,098 operations
were executed verbatim and D-056's invocation count accepted each. D-063's bound was exercised for
real: reduced candidates raised witness-key drift three times, the unreduced world was re-executed
each time (of at most 4), held `k/0`, and the level stayed at `witness_key`. And the gate did its
job: on a target where the first stale read lands on whichever key a lagging member serves first,
the witness key is not stable across executions, so a 2,098-operation reduction that reproduced
once was refused rather than committed, and the report says why and what to try
(`--accept-trials 3`, or `--strictness class` since the finding is "some key is stale", not "k/0 is
stale").

A second run took the second remedy: `--strictness class --skip-narrowing --budget 9m
--world-budget 24`. Faults 1 → 0 again; ops 16,788 → 131 over 19 worlds, every candidate accepted
on one reproduction; confirmation **1/3 at class level**, exit 3, nothing committed. So the key
was not the only thing moving: a 131-operation trace with no fault reproduces the stale read about
one time in three, because the race it depends on (a serializable read landing on a member that
has not yet applied an acknowledged write) is a matter of timing that a shorter trace hits less
often. ddmin accepted each halving on a single lucky sample, exactly the failure the report's
last paragraph describes, and `--accept-trials 3` is the documented next step (at roughly three
times the world cost). OQ-052's acceptance half (a shrunk operation trace carried through the
gate) remains undemonstrated on both targets; the machinery it depends on has now run end to end
on a system nobody here wrote, twice, and stopped both times exactly where it should have.

---

## OQ-063 UPDATE (D-066): what the second iteration measured and fixed

**Audit.** A five-lens workflow audit of the etcd target, the D-065 fix and every published claim
was run on 2026-09-15; two lenses (the harness change, the open items) completed before the session's
model quota ran out, the three verification refuters per finding never ran, and the thirteen
findings were verified by hand against the code instead. Eleven became changes in D-066. The two
deferred are items 2 and 3 below, with the audit's designs recorded so the next iteration does not
rediscover them.

**Item 1, widened.** The D-065 fix probed the right URL: once, at the first millisecond of
QUIESCE, before the 2 s convergence sleep, and `availability_after_heal` consulted telemetry's
in-window probe samples only when no direct probe existed. Measured in `r_2026_09_15_ace7`: the
witness `t_ms` was 1814 for all three nodes, exactly QUIESCE start, while `telemetry.jsonl` held four
in-window `/health` answers per node, all 200, that the oracle never saw. A target needing 300 ms
after a heal to re-elect would have been reported unavailable "within the 2004ms convergence window"
on a single sample. D-066: two direct rounds, one before and one after the sleep, each attempt
stamped when taken; every telemetry probe sample merged in; any 2xx is available. The test that
pinned the D-065 fix bound all three nodes to one port and never fed a failing probe, so it could
not catch a wrong-port, any-status-OK, or misattributed-node regression; measured: three of four
mutants survived it. Replaced.

**Item 4, RESOLVED.** `harness.role_probe` (D-066). etcd: `net.partition(role:leader)@3000..7500`
bound to `etcd-n2` under the stale profile (`r_2026_09_15_d939`, FAIL) and to `etcd-n1` under the
linear profile (`r_2026_09_15_fa3e`, PASS 2/2). The probe binary's first execution on this host
cost about 1.3 s inside the window (injection at t+4341 ms; t+3038 and t+3020 ms on the next two
worlds).

**Undeclared nodes.** Before D-066 a node `harness.health` did not cover was probed at the invented
`/healthz` (old code) or, after D-065, reported "never probed" and made the world INCONCLUSIVE over
a config omission. Now it is NOT JUDGED and named in the OK text. The directive's own sample config
carries such a node (`pg`, role_hint storage) and was never valid under either earlier rule.

**Lock.** `harness.health` was outside the lock while being the URL a verdict rests on; an agent
that could not pass `availability_after_heal` could have repointed the probe at an always-200 path
with no ORACLE_DRIFT. Covered now, with `harness.role_probe` (D-066); both committed locks re-locked.

**Shrink, third attempt.** OQ-063's first update named `--accept-trials 3` as the remedy. Taken:
`--strictness class --accept-trials 3 --skip-narrowing --budget 25m --world-budget 70`. Faults
1 → 0 again; ops 16,788 → 197 over 61 worlds, every candidate accepted only after 3 of 3
reproductions; confirmation **1/3**, exit 3, nothing committed, 70 worlds in 18 m 33 s. Three
consecutive reproductions during acceptance and one in three at the gate is what a violation whose
probability falls with trace length looks like: ddmin's monotonicity assumption does not hold on
this target, and no number of acceptance trials fixes that; a candidate that reproduces 3/3 by luck
at p≈0.5 is accepted one time in eight, and ddmin never revisits. OQ-052's acceptance half stays
undemonstrated; what is demonstrated is that three escalating attempts to make it pass all ended at
the gate, which is the gate's purpose.

**Items 2 and 3, still open: the recorded designs.**
- *Item 2, `no_unbounded_queue` / goroutines from a target-declared endpoint.* Add
  `harness.metrics: { probe: "http://{host}:{port}/metrics", map: { queue_depth: <gauge>, goroutines:
  go_goroutines } }`, a Prometheus text parser (none exists in the tree), and three refusals: an
  unmapped metric stays absent; a mapped name that is not a gauge on the wire is refused; a map that
  points a queue metric at a counter is refused. `harness.metrics` must join the lock, because a map
  pointing `queue_depth` at a flat gauge makes the oracle an unconditional PASS. The oracle side needs
  no change; `readMetricsFromTelemetry` already reads the sample accessors. Confirm etcd's metric
  names against a live scrape before committing its map.
- *Item 3, RSS / fd / threads without a shell.* Phase A only: `fd_count` and `threads` through the
  `--pid container:<target>` mechanism `io.fill`/fd.exhaust already use, as a persistent observer
  sidecar (a per-round `docker run` is unmeasured against the 400 ms collect timeout). RSS and CPU
  need the target's cgroup files, which a pid-namespace sidecar does not see under Docker's private
  cgroup namespace; keep them absent with the honest reason rather than read the sidecar's own
  cgroup as the target's. `resource_return_to_baseline`'s OK text now says which metrics it did not
  evaluate, so a partial map cannot read as a full check.

---

## OQ-064: the steady-state probe has no placeholder vocabulary, so the harness exports the topology into its environment

**Classification:** RESOLVED additively, in Phase 1, by `SteadyStateEnv`
(`internal/control/steady.go`). Written on 2026-09-15: the Phase 1 code comment has said "Logged as
OQ-021" since 2026-09-06, when the ledger ended at OQ-018, and no entry was ever written. The
unrelated OQ-021 that appeared the next day answered the pointer by accident (D-067).

**Observed.** The frozen config gives `harness.steady_state.probe` as a command with no placeholders.
The compose backend's natural probe is `docker compose exec -T <service> <probe>` (D-010), but
`thesis up` creates its project under a derived name: `thesis-<name>-<hash>`, e.g.
`thesis-kvfixture-0eff35b9`, so a bare `docker compose exec` run from the project directory resolves
the directory's default project and fails against a topology that is running.

**Resolved.** The probe runs with `COMPOSE_PROJECT_NAME`, `COMPOSE_FILE` (the topology file plus the
generated overlay) and `COMPOSE_PATH_SEPARATOR` set explicitly (compose splits `COMPOSE_FILE` on `:` by
default, which cuts a Windows path at the drive letter), plus `PROTHESIS_RUN_ID`,
`PROTHESIS_PROJECT_DIR`, `PROTHESIS_BACKEND`, `PROTHESIS_NODES`, `PROTHESIS_HOST` and one
`PROTHESIS_PORT_<NODE>` per node with a published port. The directive's own probe form then works
verbatim, with no new placeholder and no change to the frozen config surface. The `PROTHESIS_*`
half turned out to be the part that mattered for a third-party target: `targets/etcd`'s image ships no
shell, so its steady-state probe and its role probe (D-066, which reuses this environment) run on the
host and address each node through `PROTHESIS_PORT_<NODE>`.

**What this does not close.** The variables are additive and nothing declares them normative; a probe
that ignores them gets the old failure. `PROTHESIS_PORT_<NODE>` upper-cases the node id and replaces
anything outside `[A-Z0-9]` with `_`, so two ids differing only in punctuation (`kv-n1`, `kv_n1`)
collide on one variable; nothing refuses such a config.

---

## OQ-065: the log lines quoted into a causal timeline are chosen by a keyword heuristic tuned to one kind of system

**Classification:** ACCEPT-AND-DOCUMENT. Written on 2026-09-15: the Phase 1 code comment on
`interestingLogRE` (`internal/control/timeline.go`) has said "Logged as OQ-022" since 2026-09-06, when
the ledger ended at OQ-018, and no entry was ever written; the unrelated OQ-022 created the next day
answered the pointer by accident (D-067).

**Observed.** A violation's causal timeline quotes node log lines that fall within
`TimelineLogWindowMS` (5,000 ms) of the finding, at most `MaxTimelineLogRows` (12), selected by one
case-insensitive pattern: `panic`, `fatal`, `oom`, `out of memory`, `segmentation fault`,
`goroutine N [`, `exit status`, `term N`, `leader`, `elected`, `election`, `partition`, `unreachable`,
`connection refused`, `timeout`, `timed out`, `error`, `unhealthy`, `lease`. That vocabulary is a
Raft key-value store's. On etcd it happened to fit: arm B's verdict (`r_2026_09_15_9a29`) quoted
etcd's own warnings about ReadIndex retries and "RAFT NO LEADER" health failures beside the stale read.
A system whose failure logs say "quorum lost", "replica lag" or "rejected" would get none quoted.

**Why it is accepted.** The pattern decides only what is QUOTED; it is never evidence. Every node's
complete log is written into the run bundle regardless, and no oracle reads the quoted rows. A missed
line costs a reader a grep, not a verdict.

**What this does not close.** Nothing measures the heuristic's recall, and the vocabulary is not
configurable per target. If a target's timelines turn out to be empty of the lines that explain them,
the fix is a target-declared pattern, which, like every other target-declared input, would then have
to be considered for the lock.

---

## OQ-051 RESOLVED (D-067): the colliding ids are split, every citation was read, and a test now refuses a duplicate

**What was actually duplicated.** Of the five OQ ids OQ-051 listed, three were not collisions: OQ-003,
OQ-004 and OQ-033 each carry a later `RESOLVED` or `UPDATED` section about the SAME topic, and the 62
references to them needed no disambiguation. The real collisions were two OQ ids and three D ids, each
naming two unrelated entries. The second topic under each number now carries a `b` suffix in place:
OQ-021b (the Phase 2 reference schedule), OQ-022b (the `no_stuck_op` lifecycle, and its RESOLVED
section), D-031b (`proc.slow`), D-032b (`mem.pressure`), D-033b (the process, clock and I/O
`Primitive`s).

**Every citation was read against its subject.** 85 reference lines across the tree. All 51 to D-031
and all 7 to D-032 meant the first entry; the second entries under those numbers were never cited.
Of 16 to D-033, 5 meant D-033b (the `io.latency` capability gap) and were rewritten. Of 7 to OQ-021,
1 meant OQ-021b and was rewritten. OQ-051's own count tables are left as the record of what it
measured.

**The fear OQ-051 stated was real, in a form it did not anticipate.** It refused renumbering because
"a reference silently pointing at the wrong entry is a worse defect than a visibly duplicated heading".
Two such references existed already, and not because of the collision: Phase 1 comments in
`steady.go` and `timeline.go` promised entries "OQ-021" and "OQ-022" on a day the ledger ended at
OQ-018, the entries were never written, and the next day's unrelated OQ-021 and OQ-022 answered them.
They are now OQ-064 and OQ-065, written from the code they describe.

**Pinned.** `internal/ledger`: `TestTheCommittedLedgersHaveUniqueAddresses` fails on a second defining
heading for an id or a follow-up section with no entry above it, and `TestEveryLedgerCitationInTheTreeResolves`
fails on any citation, in any Go, Markdown, YAML, PowerShell or Python file outside the verbatim
historical documents under `docs/protocol` and `docs/design`, that names an entry which does not exist.
Before the two entries above were written it failed on exactly those two comments and the not-yet-written
D-067; the historical documents carry no dangling citation either.

---

## OQ-066: an opt-in test asserted what D-059 later forbade, and nothing ran it for five days

**Classification:** RESOLVED in the test (2026-09-15). Recorded because the pattern is the finding:
a test that only runs on request is a claim nobody re-checks.

**Observed.** `TestFixtureDrainReachesPassOnACleanWorld` (`internal/control/fixture_docker_test.go`,
added 2026-09-08 for OQ-033) runs the fixture's `linear` profile narrowed to one of its five worlds
and asserted the RUN exited 0. D-059 (2026-09-10) made that impossible: a narrowed run that finds no
violation may not report PASS, because it did not perform the gate the profile specifies. The test is
opt-in (`PROTHESIS_DOCKER_TESTS=1`) and appears as a skip in every suite count recorded since, so
from 2026-09-10 it asserted something the product correctly refuses, and no recorded run could show
it.

It surfaced the first time anything turned it on: CI on a hosted Linux runner (D-068), run
`35015380182`. The failure it reported was misleading twice over. Its message called the result
"the OQ-033 defect", and the proximate cause was a third thing entirely; the CI job had built the
fixture's driver but not its external checker, so `linearizable.kv` could not start
(`fork/exec ./bin/linearizable-kv: no such file or directory`) and the world was correctly
INCONCLUSIVE. Run `35015859119` printed the world's full result: every other oracle `ok` on
evidence (6,792 operations completed within the SLO ceiling, three nodes available after HEAL,
resource metrics back within band), `linearizable.kv` inconclusive, and the harness's own warning
that the narrowed run "cannot report PASS". Building the checker would have exposed the D-059
contradiction next.

**Resolved.** The test now asserts what OQ-033 is about (the WORLD's own outcome in `result.json`
is `pass`, and `no_stuck_op` passed on evidence) and separately asserts D-059's run-level
consequence: exit 2 with `budget.narrowed` set. A missing external checker is a named skip reason
rather than a failure blamed on OQ-033, and CI builds the checker.

**What this does not close.** Two Docker-backed tests remain opt-in by design (they cost minutes and
bridge networks on the build host), and three search-engine tests skip wherever no recorded run
bundles exist, which includes every fresh clone and CI. An opt-in test is exercised only where
something opts in; CI now does for these two, and nothing does for the other three.

---

## OQ-067: image resolution is fail-soft in its error and not in its time, so a sick daemon spends the world's budget

**Classification:** MITIGATED 2026-09-19 (D-073); measured 2026-09-17, twice, on the unit suite.

`harness.ResolveImages` (D-070, closing OQ-055) inspects the bound containers at BOOT to record
`sut.images`. Its error contract is careful and correct: a resolution failure costs the world its
provenance and never its execution, degrading to the schema's honest `[]` with a warning
(`internal/control/runner.go`). Its TIME contract does not exist. It is handed the world's own
context, so the call is bounded only by the world's whole wall budget.

**What that cost, measured.** After a disk-full event corrupted the Docker content store, `docker
inspect` on a NONEXISTENT container took 32 seconds to return a 500 instead of failing immediately.
`TestPerturbRunsARealScheduleAndRecordsIt` and `TestPerturbEmptyScheduleStillPasses` each ran 94
seconds against a 60-second profile budget and failed `exit = 3 (BUDGET_EXHAUSTED), want 0`. Both had
passed minutes earlier with the same code. Nothing about the system under test changed; a metadata
lookup ate the budget and the verdict reported a budget the workload never spent.

**The second half is test isolation.** Those two tests drive a `stubBackend` with invented container
ids (`c1`, `c2`, `c3`). They have no containers and want none, yet they shell out to the real daemon
on the developer's machine and take its health as an input. A unit test that passes or fails
according to whether Docker is well is not a unit test, and it reports the wrong cause when it fails:
the assertion blames the budget.

**Two candidate fixes, neither implemented here.** (1) Bound the call with its own short timeout;
provenance is metadata, so a few seconds is generous, and exceeding it should look exactly like any
other resolution failure: `[]` plus a warning. (2) Skip resolution when no backend can have bound a
real container, so the stub path never reaches the daemon. (1) fixes the product, (2) fixes the
tests; they are not alternatives. Neither is written yet because the failing-first test wants a slow
or sick daemon on demand, and `run(ctx, ...)` shells out to the `docker` binary with no seam for one.
That seam is the actual first task.

**Related.** D-070 introduced the call; OQ-055 is what it closed. The same disk-full event is what
corrupted the store, so the trigger here was environmental, but the sensitivity it exposed is not,
and nothing in the suite would have revealed it on a healthy machine.

**Update 2026-09-19 (D-073).** Both candidate fixes above are in, and the seam that "the actual
first task" named turned out to be two seams. In `harness`, `inspectRun` is the docker invocation
behind a variable, so a test can stand in a daemon that blocks until told to stop; in `control`,
`ImageResolver` is a runner seam like `Driver` and `LogCollector`, so a unit world never reaches a
daemon at all. The bound is five seconds inside `ResolveImages`, reported as its own error, with an
inherited deadline attributed to the caller, and named as the caller's. Six tests pin it and seven
targeted mutations were each shown to fail (the bound removed, the attribution guard removed, the
caller's abort left unnamed, the seam bypassed, the default removed, the bound silently lengthened,
and the fail-soft branch made fatal) with every failure text recorded in D-073 as emitted. The two
tests this entry measured no longer touch the daemon and cannot fail for its health again.

**What remains open here.** The bound is a constant. A daemon that is healthy but slower than five
seconds now loses a world's provenance rather than its budget, which is the intended trade but is a
trade. `internal/search/engine`'s test runner still inherits the daemon default; it runs no world
today. And the driver's own stdout and stderr are still discarded, which is L1c's to fix. The
perturber's own `docker inspect` calls (`dkInspect` in `internal/perturber/faults`) carry no
`context.WithTimeout` of their own either (measured: none outside test files) but they are
fault-injection paths, outside this entry's scope, and D-073 does not cover them.

---

## OQ-068: the SUT image is rebuilt before every world with the run corpus inside its build context, so `sut.images` names one program twenty ways and the build cache grows by gigabytes per world

**Classification:** MITIGATED 2026-09-20 (D-078); measured 2026-09-19, found while checking the
OQ-054 sample's provenance. Warm-cache identity and cache growth are fixed; identity across a COLD
cache is not (see the update at the end).

**Observed.** Twenty consecutive `thesis run` worlds against one unchanged Dockerfile recorded
**twenty distinct `sut.images` digests**; OBS-LIVE-002 the day before had recorded a twenty-first.
Each run re-pointed the `prothesis/kvfixture:buggy` tag at its own new ID, and `docker images` showed
only the current one, so nothing on the daemon's side reveals the churn; only the world files do.

**Mechanism, measured in three parts.**

1. `testdata/kvfixture/docker-compose.yaml:71` sets `pull_policy: build` on `kv-n1` (the anchor at
   `:51` says `never` for the other two), so compose **rebuilds the image on every `up`** with no
   `--build` flag from the harness. The harness passes none (`internal/harness/compose.go:114`).
2. The image ID that `docker images` reports is not a function of the image's content, and the
   image itself IS reproducible. Two `docker compose build kv-n1` back to back with NO change: 9.2 s
   → `d7f0c4cd…`, then **1.4 s, every layer a cache hit, → `ad5f777f…`**, while the image's `Created`
   stayed at the morning's value and its `RootFS.Layers` were identical. Isolated by elimination:
   raw `docker buildx build` with `SOURCE_DATE_EPOCH=0`, twice, gave a byte-identical config,
   manifest and layer list; `docker compose build --provenance=false --sbom=false`, twice, gave the
   SAME id (`776d2893…`) with no epoch pin at all; compose with its defaults, twice, gave two ids
   (`d86d0f2f…`, `a72c5db2…`) over identical layers. Compose 2.40.3 attaches provenance and SBOM
   attestations to the image index by default, and Docker Desktop's containerd store reports the
   INDEX digest as the image id. The attestation (a record of the build, carrying the build's own
   timestamps) is what moves. The "content address" D-070 records is therefore the address of a
   build record, not of the program, and identical source yields a new digest every world for that
   reason and no other.
3. `.dockerignore` excludes `golden/`, `bin/`, `*.md` and `.git/` and NOT `.prothesis/`, while
   `Dockerfile:17` is `COPY . .`. The fixture directory is 2,452 MB, **of which `.prothesis/runs` is
   2,426 MB**. Every world allocates its run directory before compose runs, so every world's build
   context differs from the last, the `COPY` layer misses the cache, and a fresh multi-gigabyte cache
   entry is written: the build cache stood at **19.13 GB after roughly 22 builds**, and one further
   build after the sample added **2.70 GB**. Docker on this host was capped at 50 GB on 2026-09-17;
   ordinary sampling reaches that cap in tens of worlds. The 21.83 GB of cache was pruned after
   being measured.

**What it costs.** Three things, and only the third is about disk.

- **Provenance says the wrong thing.** D-070 records the container's image ID as "a content address
  no tag drift can fake," which is true of the container, but the image is remade for every world,
  so the digest identifies a BUILD, not the system under test. A comparability tuple keyed on SUT
  image ID, as the L3 design proposes, would split the twenty-run sample above into twenty strata of
  one. The honest reading today is: same Dockerfile, same source, twenty names.
- **The corpus is inside the SUT's build input.** Every recorded run enlarges the next world's build
  context. That is a layering fault regardless of the cache: the artifacts of past executions have no
  business in the bytes shipped to build the thing under test, and it is why BOOT costs 15–22 s per
  world with a WARM cache. The cold case is measured: on the first world after the cache was pruned
  to zero (`r_2026_09_19_3213`, the post-D-073 confirmation run), **BOOT took 186.5 s of a 200.6 s
  run**, and that one world wrote **5.70 GB** back into the build cache. How much of the warm-cache
  BOOT is context transfer is still not separated here, because separating it requires the fix.
- **The cap will be hit by normal use**, not by an accident, and when it is the failure is the one
  OQ-067 recorded: a daemon that stops answering.

**Candidates, none implemented here.** (1) Add `.prothesis/` to `.dockerignore`: removes 99% of the
context and the per-world cache entry, and cuts the corpus-to-build coupling; its effect is directly
measurable as context size, cache growth per world and BOOT time. (2) Turn attestations off for the
fixture build. The harness runs `compose up`, not `build`, so no flag can be passed; the setting
belongs in `docker-compose.yaml` under `kv-n1.build` as `provenance: false` and `sbom: false`.
Measured above: that alone makes two builds agree while the cache is warm. To agree across a COLD
cache as well, `SOURCE_DATE_EPOCH` must also pin the config's `created` (the binaries inside are
already `-trimpath -ldflags="-s -w"`); measured: raw buildx so pinned is byte-identical. With both,
`sut.images` becomes a content identity in fact as well as in name, and the acceptance check is the
two-builds experiment producing one id. (3) Revisit `pull_policy: build`; the compose file explains
why `kv-n1` builds (the variant lock), and with (1) and (2) a rebuild is cheap and stable, so this is
the least urgent.

**Related.** OQ-055 is what `sut.images` closed; D-070 recorded the digest; D-072 made the
harness's own binaries reproducible for exactly the reason (2) applies to the image. OQ-054's
2026-09-19 update carries the sample this was found in.

**Update 2026-09-19, later: what a changed context costs on a WARM cache, separated from the cold
case.** A review of `r_2026_09_19_58f2` (BOOT 181.2 s) read "both images present" as a warm cache
and concluded that, because `COPY . .` re-copies a context every run changes, the first world of
every run after a prior run lands in the 181 s path. The record says otherwise, and the warm case
is now measured directly rather than left as "not separated here".

- `58f2` was the first world after the 21.83 GB cache prune recorded above. Images being present
  is not the build cache being warm: the builder stage (`golang:1.22-alpine`, the 2.4 GB `COPY`,
  `go build`) is what the cache holds, and `docker system df` before today's measurement showed
  5.707 GB in 14 entries, which is that one cold world's write and nothing else.
- The OQ-054 sample is twenty consecutive runs, each of which added a run directory to the
  context before the next build, so every one of those twenty builds missed the `COPY` cache.
  BOOT was 15.3–21.1 s in all twenty (from each world file's `phase_timings`).
- Measured now on the build host, warm cache, tree unchanged: `docker compose build` with the
  context unchanged since `58f2` took 13.1 s wall; with ONE new file placed under
  `.prothesis/runs` it took 8.9 s wall, and the build cache went from 5.707 GB (14 entries) to
  11.11 GB (18 entries); +5.4 GB for one re-executed `COPY` layer. The probe file was deleted
  afterwards; `git status` is unchanged.

So a changed context costs seconds of BOOT and gigabytes of cache per world; a cold cache costs
three minutes once.

**Update 2026-09-20: mitigated by D-078, with candidate (2) taken by a different route.** Candidate
(1) landed as written: `.prothesis/` is in the fixture's `.dockerignore`. Candidate (2) did not land
as `provenance: false` in the compose file; the harness instead sets
`BUILDX_NO_DEFAULT_ATTESTATIONS=1` on every docker invocation, because `sut.images` is the harness's
claim about any target and a target's compose file is not ours to edit. Measured through
`docker compose build`: default environment, two builds of an unchanged context, two different ids
(the defect, reproduced); switch set, the same id twice; switch set with a new run directory created
in between, the same id a third time, 0.7 s, and the build cache unchanged at 11.32 GB in 22 entries
where the same event had cost +5.4 GB the day before. Candidate (3) is declined: `pull_policy: build`
stays, since the variant lock depends on it and a rebuild now costs under a second.

**What is still open.** Identity across a COLD cache. The image config's `created` moves when the
layers are rebuilt from nothing, so the first world after a prune records a different `sut.images`
digest from the worlds before it, for an unchanged Dockerfile. Pinning it needs
`SOURCE_DATE_EPOCH`, and for the fixture the natural value is the commit's date, but the harness
has no principled value for a target it does not own. Until that is decided, read a `sut.images`
digest as stable WITHIN a build-cache lifetime and not across one.

**Update 2026-09-20, later: four live worlds on the fixed harness, the first and the last on a cold
cache.** Binaries from `scripts/build.ps1 -Verify` at the clean commit `69e13fc` (two builds, one
digest, `76b5ededccd7…`); the fixture re-locked first, so `oracle_lock` was `ok` with no
`executables_moved` in all four verdicts. `docker builder prune -af` took the cache from 11.32 GB to
0 B, then `thesis run --profile linear --seed N --fault 'net.partition(role:leader)@3000..7500'` for
N = 1, 2, 3; then a second prune and N = 4. Every exit code was read in the launching shell on the
line after the call.

| World | Run | Cache | Exit | BOOT | Wall | `sut.images` digest |
|---|---|---|---|---|---|---|
| seed 1 | `r_2026_09_20_1956` | cold | 1 | 20.2 s | 34.0 s | `2d24ac4257559ee5…` |
| seed 2 | `r_2026_09_20_fc66` | warm | 1 | 7.5 s | 20.8 s | `2d24ac4257559ee5…` |
| seed 3 | `r_2026_09_20_2e52` | warm | 1 | 7.4 s | 20.7 s | `2d24ac4257559ee5…` |
| seed 4 | `r_2026_09_20_8a38` | cold again | 1 | 20.1 s | 33.5 s | `2689d1154a72dfea…` |

What that shows, against the 2026-09-19 figures above. **Cold BOOT fell from 181–187 s to 20 s**, so
the three-minute cold start had been the 2.4 GB context and not the base images or the compile.
**Warm BOOT fell from 14.9–21.1 s to 7.4–7.5 s**, and a whole warm world from 28.3–35.1 s to 20.7 s.
**A full cold build now writes 518.8 MB of cache in 14 entries, where one cold world had written
5.70 GB**, and the cache did not grow at all across the two warm worlds, each of which added a run
directory to a tree the build no longer sees. **Three consecutive worlds recorded one `sut.images`
digest**: the day before, twenty recorded twenty. And **the digest did move across the second
prune**, in the same invocation path, for an unchanged Dockerfile: the open half of this entry is
now measured rather than predicted. All four world files record DRIVE spanning PERTURB, and
`role:leader` bound to `kv-n2` in each.

---

## OQ-069: an LLM-proposed schedule stream is not seed-reproducible; the audit trail is `llm-proposals.jsonl`

**Classification:** OPEN; accepted risk, measured basis: the D-075 design and its unit tests
(2026-09-20). One live llm-driven world has been executed (`r_2026_09_20_178c`), on an unstamped
binary and a narrowed profile; D-075 records why it is not validation evidence.

**Observed.** `search.strategy: llm` (D-075) asks a local OpenAI-compatible model server for each
world's fault schedule. `--seed` still pins what it always pinned (the per-world seeds
(`saboteur.WorldSeedFor`, the same derivation the executor uses) and everything downstream of the
world document) but the proposal TEXT comes from an external process. The strategy sends
`temperature: 0` on every call and there is deliberately no knob for it, yet temperature 0 does not
guarantee bitwise-identical output across server versions, hardware, or batching state: the same
prompt can legitimately yield a different schedule tomorrow. A `--seed N` rerun of an llm search is
therefore NOT guaranteed to ask for the same worlds, and a claim that it does would be false.

**What makes this acceptable, and what does not.**

- Every attempt (prompt, response, proposed schedule, validator error, accepted) is appended to
  `llm-proposals.jsonl` in the run directory (`internal/search/llm/record.go`), including the
  rejected attempts and transport failures. The proposal stream is reconstructible from the record
  even though it is not reproducible from the seed.
- The world file still records exactly what ran, canonicalized, so REPLAY of a recorded world is
  unaffected: `thesis run` of an archived world never consults the model.
- What is lost is search-level replay: given only the seed, a second run may explore a different
  sequence of worlds. Any future statistical gate over llm-strategy runs (L3) must therefore treat
  the recorded worlds (not the seed) as the unit of evidence.
- The endpoint defaults to a localhost server with no auth; pointing `search.llm.endpoint` at a
  remote service changes the trust boundary and is visible in the config and the lock digest, not
  hidden in a flag.

**Related.** D-075 is the design that accepts this risk; OQ-033 is why the feedback signal the
model is coached with stays three-valued.

---

## OQ-070: the retention policy is parsed, validated and defaulted, and enforced by nothing

**Classification:** OPEN; measured 2026-09-21. The attribution layer that any pruner must
consult first landed as D-082, and the census floor that makes a shrunken corpus fail the suite
instead of skipping landed as D-083; the pruner itself is not scheduled.

**Observed.** `RetainPolicy.Keeps` (`pkg/schema/retain.go:63`) has zero callers anywhere:
`git grep -n '\.Keeps\(' -- '*.go'` matches only its own definition. The policy it serves is
scaffolded into every `prothesis.yaml` (`cmd/thesis/init.go:209-210`), defaulted
(`pkg/schema/config_validate.go:94-98`: `retain_passing: 3`, `retain_failing: all`), validated
(`config_validate.go:443-452`), and read by no code path. The only spec is
`docs/design/02-recorder.design.md` section 4.5 (`func Retain(runsDir string, cfg
schema.ArtifactsConfig) error`); `internal/recorder/retain.go` was never written
(`02-recorder.verify.md` records the Phase 0 descope).

**Two measured spec defects an implementer must route around.** (a) The spec's ordering key
`run.json.started_wall_ns` does not exist: no Go file references `run.json` or
`started_wall_ns`, and no bundle on disk contains one. (b) "A run referenced by any
`.prothesis/regressions/*.thesis` is NEVER pruned" is unimplementable as written: `World` and
`WorldOrigin` carry no run_id back-reference to the run directory that holds the evidence
(`02-recorder.verify.md` notes the same gap).

**Why it matters less than it looks, on this corpus.** Simulated whole-directory deletion under
the literal policy frees 104.6 MiB = 4.42% of the 2.48 GB corpus: PASS is the only class the
default policy deletes and it is the smallest, at 5.6% of bytes. Corpus re-measured 2026-09-21:
184 run bundles under `testdata/kvfixture/.prothesis/runs` (146 with `verdict.json`), gitignored,
existing nowhere else. Deletion remains final; the census floor that makes a shrink loud is
OQ-072's companion.

---

## OQ-071: an INCONCLUSIVE verdict records no reason, and a world that dies before ASSERT records nothing at all

**Classification:** OPEN; measured 2026-09-21. This is the harness-record defect class of
OQ-072's census. Attribution of the already-recorded cases landed as D-082 (KP-006, KP-007,
KP-010); persisting reasons at write time is unscheduled.

**Observed.** `verdict.json` (`prothesis.verdict/v1`, `pkg/schema/verdict.go:31-53`) has no
reason field. Of the 43 INCONCLUSIVE run verdicts in the corpus, 32 predate even the
`budget.narrowed` field and cannot say why they refused. At world level, `result.json` is written
only on the path that reaches ASSERT (`internal/control/runner.go:1347-1366`); every early-return
path in `runWorld` (boot, telemetry, steady-state, driver, perturb, heal, quiesce failures,
`runner.go:759-1205`) returns without writing it. 16 of the 43 INCONCLUSIVE runs are exactly this
shape: the dead world left logs, phase markers and an overlay but no `result.json`, so the reason
existed only on that run's stderr and is unrecoverable. `result.json` also has no Go schema type;
it is built ad hoc as a map.

**Consequence.** A reader cannot tell "the defect did not appear" from "we never got to look"
without reconstructing the run from stderr that no longer exists. Recorded bundles are immutable,
so the 16 runs stay unexplained permanently; the fix is prospective (reasons persist at write
time) and is attributed, not backfilled.

---

## OQ-072: census: why the recorded corpus is INCONCLUSIVE (measured classification, 2026-09-21)

**Classification:** ACCEPT-AND-DOCUMENT for the refusal classes; the harness-record half is
OQ-071. This entry is the measured census the known-problem registry (KP-001 through KP-014,
`testdata/kvfixture/.prothesis/known-problems.yaml`) is seeded from.

**Population.** 184 run bundles; 146 with `verdict.json` (FAIL 79, INCONCLUSIVE 43, PASS 23,
BUDGET_EXHAUSTED 1), 38 without. 802 `result.json` world files: pass 285, violation 233,
inconclusive 284. Measured with PowerShell `ConvertFrom-Json` sweeps of
`testdata/kvfixture/.prothesis/runs`.

**The 284 inconclusive worlds, per (world x oracle) instance, by reason string:**

| Instances | Oracle / cause | Code path |
|---|---|---|
| 202 | `resource_return_to_baseline`: "no pre-DRIVE baseline" | `internal/oracle/resource_return_to_baseline.go:92` |
| 143 | `no_unbounded_queue`: fewer QUIESCE samples than needed to call a rise monotonic | `internal/oracle/no_unbounded_queue.go:83` |
| 47 | `no_unbounded_queue`: carries none of the queue-depth metrics | `no_unbounded_queue.go:77` |
| 33 | `resource_return_to_baseline`: "no samples" | `resource_return_to_baseline.go:87` |
| 10 | `resource_return_to_baseline`: "no sample at or after QUIESCE start" | `:97` |
| 6 | legacy `result.json`, no oracle fields at all | pre-`oracles[]` schema |
| 4 | `no_stuck_op`: QUIESCE shorter than the SLO ceiling (OQ-033's structural case) | `internal/oracle/no_stuck_op.go:232` |
| 4 | `linearizable.kv`: nothing checkable (OQ-056's port-mismatch runs) | `cmd/thesis-oracle-linearizable/load.go:581` |
| 2 | one world: `history.jsonl` missing mid-read, both oracles refuse | `internal/oracle/input.go:601`, `load.go:134` |

**The 43 INCONCLUSIVE run verdicts, by cause:** 13 folded-up world-level oracle inconclusive; 16
harness-error worlds with no `result.json` (OQ-071); 7 narrowed-budget PASS downgrades (D-059,
`internal/control/verdict.go:104-107`); 4 narrowed plus harness-error; 1 zero-worlds search
(`internal/search/engine/engine.go:721-723`); 3 legacy, cause unrecoverable from disk.

**The 38 verdict-less bundles:** 8 `thesis up` standing-topology bundles (no verdict by design);
2 empty directories (run id allocated, aborted before BOOT); 26-28 interrupted `thesis search`
runs (complete per-world artifacts, process never reached `engine.finish`; 16 of the 28 contain
violation worlds, so "no verdict.json" must be read as "interrupted", never as "no evidence");
2 died mid-first-world.

**Declaration.** Roughly 88% of inconclusive world-instances are telemetry-coverage refusals: no
baseline, or too few QUIESCE samples. Those are honest refusals (the refusal surface working as
designed) and must be counted and reported, never "fixed" into another verdict. The avoidable
classes are OQ-071's (a dead world that records nothing) and the already-resolved OQ-033/OQ-056
classes. Every future inconclusive data point must attribute to a registry entry or turn the
diagnostic red; unattributed silence is the failure this entry exists to prevent.

---

## OQ-073: L1c: the OS exit code and raw oracle/driver stdout are not persisted

**Classification:** OPEN; not scheduled in the current work; recorded so the backlog names it.

**Observed.** Oracle INPUT is evidenced (`oracle_input.json` per world) but oracle stdout is not
persisted, so the output half is evidenced only by the parsed result
(`docs/observations/2026-09-19-OBS-LIVE-002/record.md:198-201`). The driver's own stdout and
stderr are discarded (D-073 records this as L1c's to fix). `witness.stderr_path` and
`oracle_definition` leak absolute host paths into bundles; the observation record's proposal is
run-relative paths (`world-0001/oracles/linearizable.kv.stderr.log`), which would end the class.
Source list: `docs/observations/2026-09-17-OBS-LIVE-001/audit-response.md:63-73`.

---

## OQ-074: the parallel-lane port pre-flight cannot answer from inside a container

**Classification:** OPEN; found while containerizing the harness (D-087); not yet measured with a
real collision, so it is recorded from the code rather than from a failure.

**Observed.** `internal/control/parallel.go` reserves a band of host ports per worker slot and
checks each one first with `portFree`, which does `net.Listen("tcp", "127.0.0.1:<port>")` and
treats a successful bind as evidence the port is available. That is sound when the harness runs on
the host, which is what it was written for.

It stops being sound when the harness runs inside a container. The bind then happens in the
CONTAINER's network namespace, which is not the one the target's ports are published into. A port
busy on the host binds cleanly in the container and the pre-flight reports it free; the collision
then surfaces later as compose failing with "Bind for 127.0.0.1:19001 failed: port is already
allocated", which is the message the pre-flight exists to turn into something readable.

**Why it is recorded rather than fixed.** The failure is not silent in the verdict sense: the world
fails to boot and the run is INCONCLUSIVE, so nothing is passed that should not be. What is lost is
the diagnosis, and the check reporting "free" when it cannot know is the part that offends the
refusal rule. A check that cannot answer should say so.

**What would close it.** Either skip the pre-flight when `probehost.Host()` is not loopback and say
in the output that lane collision detection is unavailable, or ask the daemon which host ports are
bound instead of asking the local netstack. The second is the real fix and needs a measured
collision to prove.

**Meanwhile**, `docs/TARGETS.md` says to run single-lane from a container, which is the only form
exercised so far.

---

## OQ-074 RESOLVED (D-088): the parallel-lane port pre-flight derives published ports from the Docker daemon

**Classification:** RESOLVED in D-088.

**Resolution.** Closed by consulting BOTH signals, because neither sees the whole host.
`harness.PublishedHostPorts(ctx)` queries `docker ps --format '{{.Ports}}'` and parses the
host-bound published ports across every running container, reached through the
`daemonPublishedPorts` seam. That is the half the local netstack cannot answer from inside a
container. `checkPortsFree(ctx)` then also binds `127.0.0.1:<port>`, which is the half the daemon
cannot answer: a port held by anything that is not a container. The local bind is not gated on
"are we in a container"; inside one it finds nothing and contributes no information, which is
cheaper than a heuristic that silently drops coverage when it guesses wrong. The daemon query is
bounded at five seconds, for the reason D-073 bounded its own (OQ-067).

**A correction to the first draft of this entry.** It said the check fell back to local
`net.Listen`, and the code it described did not: `checkPortsFree` consulted the daemon alone and
`portFree` had zero callers. The entry asserted a safety property the code did not have, which is
the failure this ledger exists to catch, and it was found in review before the change was
committed. The narrowing was real: between the first draft and this one, a port held by a
non-container process was not detected at all, so the pre-flight reported it free and compose went
on to fail with `Bind for 127.0.0.1:19001 failed`, which is the exact symptom the check was written
to prevent. Both halves are now present and each is covered by a test that fails when its half is
removed.

**Evidence.** `TestAnOccupiedHostPortIsRefusedWithItsOwnMessage` binds a real loopback socket with
the daemon mocked as reporting nothing, and `TestPortPreflightQueriesDaemonSeam` reports a port
through the daemon that nothing holds locally. Mutation: removing the local half makes the first
fail ("a slot whose first port was already bound by a non-container process was accepted"), removing
the daemon half makes the second fail ("run succeeded when daemon reported port 19000 occupied"),
and the source restored byte-identically after each (SHA-256
`6C882E18E32F6A136ED43688D027841D5C782ED61B48AC02C0AD288F667A6BC1`). A clean parallel run proves
nothing about detection and is not offered as evidence here.