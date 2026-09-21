# 07-saboteur.i2-conflict

## Summary

The suspected primary conflict is real but mis-located. Addendum A is internally self-contradictory: its prose (A.1, A.4, A.5 root-node) describes online intra-world adaptation, but every piece of its normative machinery (UCT `visits`, the ≥3-visit prune, "each rollout executes a real world", A.8's determinism claim) requires per-world offline tree expansion, since a live distributed state can never be revisited without simulation, which I1 forbids. More importantly, the base spec **already** breaks a naive reading of I2 with no Saboteur involved: `role:leader` and `minority(kv)` bind targets at injection time from live state, and `oracle_input/v1` passes *measured* phase boundaries, so the `.thesis` file needs a planned/realized split for Phase 2 regardless. I2's five-tuple is also missing SUT build identity, which independently makes `thesis bisect` and `reproduced: "3/3"` unsound. The most dangerous single line in Addendum A is not the I5 phase conflict but A.5's "terminate the rollout immediately", which skips HEAL and therefore leaves live iptables rules and SIGSTOPped containers poisoning every subsequent rollout: triggered, perversely, by success. The `search:` key is **not** a YAML collision (distinct paths), but A.10's `strategy: saboteur` default contradicts §4.2's `search: true` on `soak` only, which would make `thesis gate` run an adaptive MCTS search; it must not.

## Findings (20)

### F1 [RESOLVABLE-WITH-DECISION] **(AFFECTS PHASE 0)** A.5 tree semantics are self-contradictory; only per-world (offline) expansion is implementable

A.5 states "Root node: The current observed distributed state at the moment REINFORCE was detected" and "Child nodes: Each child represents an additional fault action from the grammar, applied at the current virtual clock time", supported by A.1 ("observes the system's live telemetry") and A.4 ("A REINFORCE_ACCELERATING classification on any metric triggers immediate escalation"). That online reading is refuted by A.5's own machinery: UCT is defined as "(U_avg / visits) + C x sqrt(ln(parent.visits) / visits)" and pruning as "visited >=3 times"; both require node revisitation, which is impossible for a live distributed state with no snapshot/restore (I1: "Fakes and mocks are strictly forbidden in the system under test"; Tier A simulation is v4 per CRUCIBLE PART 5). Under the online reading every node has visits==1 permanently, UCT collapses to U_avg, and the prune rule can never fire. A.5 also says "Each rollout executes a real world (not simulated)" at "25-60 seconds each": that is a whole BOOT..TEARDOWN lifecycle, not a mid-DRIVE continuation. A.8's "All Saboteur decisions (fault selection, tree expansion) must be deterministic given the same PRNG seed" is only conceivable over plans, never over live telemetry.

**Recommendation:** Adopt the offline reading normatively: an MCTS node is a fault-schedule PREFIX, a rollout compiles prefix+ladder-guided continuation into a complete schedule and executes one full world. Separately adopt 'Reading 1.5' for the schedule itself: a fault's injection TIME may be bound by an event predicate, exactly as its TARGET is already bound by role:leader. The schedule remains a fixed serializable program. Record both decisions in DECISIONS.md and file the A.5 prose contradiction in OPEN_QUESTIONS.md per base Core Rule 4.

### F2 [BLOCKING] **(AFFECTS PHASE 0)** I2's fault_schedule is already under-specified by the BASE spec, independent of the Saboteur

Section 4.3 defines TARGET as including "roles (role:leader)" and "quorums (minority(kv), majority(kv))". These resolve at injection time from live cluster state, so which container is paused at t=8200ms is a function of real, non-reproducible timing. The Phase 2 Definition of Done depends on exactly this: "Executing a scripted fault schedule (proc.pause(role:leader)@8200..15100 overlapping net.partition(minority(kv))@8400..14900) triggers the Raft fixture's stale-read anomaly." Two further confirmations that a realized layer is already assumed: section 4.5 oracle_input passes phases with start_ms/end_ms of 0/42000/45000/50000/55000; measured outcomes, not planned budgets, though I2 names phase_timings as an input; and section 4.6's causal_timeline records the CONCRETE node ("proc.pause n2 (leader, term 4)") resolved from the symbolic role:leader, with nowhere in I2's tuple to store that resolution.

**Recommendation:** Split the .thesis World into Plan (pre-execution intent, symbolic targets, optional event triggers) and Realized (concrete resolved_targets, actual virtual-clock inject/withdraw times, measured phase_timings). Realized is written by RECORDER. This is required by Phase 2 on its own merits; the Saboteur only makes it urgent. Freeze it now while pkg/schema is empty.

### F3 [BLOCKING] **(AFFECTS PHASE 0)** I2's world tuple omits SUT build identity, making replay, bisect and reproduced-rate unsound

I2: "Every execution is defined by a serializable world tuple: (seed, topology_variant, driver_profile, fault_schedule, phase_timings)." No commit, image digest, or config hash. But section 4.6's verdict carries "commit": "9c1e0f2" and "reproduced": "3/3", and CRUCIBLE PART 7 requires 'thesis bisect WORLD --good SHA --bad SHA'. Replaying a world against a different binary is not a reproduction; without build identity, 'reproduced: 3/3' is unfalsifiable and bisect has no defined semantics. This also blocks the only real implementation consequence of cross-session non-stationarity (see the MDP finding): MCTS trees and REINFORCE classifications must be invalidated when the SUT changes, which requires a build key.

**Recommendation:** Add an SUT block to the World type: commit, dirty, image_digests (service -> sha256), config_sha (canonicalized prothesis.yaml), lock_sha (.prothesis/lock manifest). thesis replay warns loudly and thesis regress records a mismatch when SUT differs from the world's recorded SUT.

### F4 [BLOCKING] **(AFFECTS PHASE 0)** A.5 'terminate the rollout immediately' skips HEAL, voiding section 4.3's Critical Guarantee and poisoning every later rollout

A.5: "Early termination: If an oracle violation is detected during DRIVE or PERTURB, terminate the rollout immediately and backpropagate the full violation reward." Section 4.1 phase 5 is "HEAL: Withdraw all injected faults; assert zero residual faults remain", and section 4.3 states: "Critical Guarantee: Every fault must implement a safe withdraw() mechanism. HEAL must verify that no residual network rules or stopped processes persist." Skipping HEAL leaves live iptables rules and SIGSTOPped containers in the compose environment. Since violations are precisely what triggers early termination, the poisoning is triggered BY SUCCESS: after the first violation, every subsequent MCTS rollout runs against a corrupted topology, and the Saboteur attributes the resulting crash cascade to whatever fault it tried next. All tree value estimates after the first violation are garbage.

**Recommendation:** Redefine 'terminate the rollout' as 'stop DRIVE and skip the remaining PERTURB schedule'. HEAL, QUIESCE, ASSERT and TEARDOWN always run, unconditionally. ASSERT is cheap relative to DRIVE, so run the full oracle set over the truncated history and sum ALL violations found (A.6 already iterates 'for _, v := range result.Violations'). Set Realized.TerminatedEarly / TerminationReason so shrink and the verdict can flag truncated evidence.

### F5 [RESOLVABLE-WITH-DECISION] Only crash and safety oracles are prefix-closed and may terminate a rollout early; consistency must stay ASSERT-only in v1

I5: "Checking consistency during an active network partition generates false positives. Every oracle declares the specific lifecycle phases in which it is valid." Section 4.5's own example declares linearizable.kv with "valid_phases": ["ASSERT"]. The correct criterion for early termination is prefix-closure. crash (a process exited outside a planned window) and safety are prefix-closed and online-observable. resource is not: resource_return_to_baseline is defined post-QUIESCE and no_unbounded_queue is defined 'across QUIESCE'. liveness/convergence are not: availability_after_heal and no_stuck_op are defined relative to HEAL, so terminating earlier makes them undecidable, not violated. Consistency is the subtle case: linearizability IS a safety property and IS prefix-closed, so I5's stated rationale is really about bad checkers, not about linearizability; CRUCIBLE section M names the actual mechanism ("Drivers that cannot distinguish MUST emit info"). But a mid-DRIVE prefix is dominated by open, indeterminate operations, which makes a Porcupine/Elle check both combinatorially expensive and overwhelmingly likely to return INCONCLUSIVE rather than VIOLATED, and unaffordable to re-run repeatedly inside a 25-60s rollout budget.

**Recommendation:** Gate early termination on: oracle class in {crash, safety} AND the currently executing phase is in that oracle's declared valid_phases. Consistency, liveness, convergence and resource oracles keep valid_phases: [ASSERT] in v1. Under this rule I5 is not merely respected, it is the enforcement mechanism. Revisit online consistency checking only if a Phase 3 incremental checker with sound info handling ever lands.

### F6 [RESOLVABLE-WITH-DECISION] Early termination biases the search, but via evidence truncation, not the duration penalty; the naive version of this claim is false

At A.6's constants the duration term does NOT invert the class ranking. Crash, high severity, terminated at t=12s with 2 faults: 80x1.5 - 3x2 - 0.1x12 = 112.8. Consistency, high severity, found in ASSERT after a full 55s world with 2 faults: 100x1.5 - 3x2 - 0.1x55 = 138.5. The duration penalty contributes at most ~5.5 points over a 60s world against a 30-point class gap. The real bias has four other mechanisms: (1) MASKING; a world terminating on a crash at t=12s never reaches ASSERT, so any consistency violation it would also have exhibited is never discovered; crash violations structurally hide the exact bug class the tool exists to find. (2) DOWNWARD VALUE BIAS: the node is credited 112.8 when its true completed utility might be 262.8, and the bias concentrates on the subtree reaching the deepest failure states. (3) TRUNCATED NOVELTY: beta reward forfeits all log templates and state tuples that HEAL/QUIESCE/ASSERT would have produced, so the most interesting branches get the least novelty credit. (4) ROLLOUT-COST ASYMMETRY: crash branches are cheap and absorb more rollouts under a wall-clock budget; UCT's sqrt(ln(N)/visits) term self-corrects this only partially, and the emitted corpus and regression set stay crash-skewed.

**Recommendation:** Always run ASSERT (see the HEAL finding) so masking cannot occur, and change gamma_duration to apply to the SCHEDULED world duration rather than the realized one; this removes any duration advantage from early termination and eliminates the arithmetic question entirely rather than arguing about constants. A.6 requires the change be recorded in DECISIONS.md.

### F7 [ACCEPT-AND-DOCUMENT] **(AFFECTS PHASE 0)** violation.phase and oracle.valid_phases are different fields with different meanings; the frozen examples will be misread

Section 4.6's verdict shows "oracle": "linearizable.kv", "class": "consistency", "phase": "DRIVE", "first_seen_ms": 14320. Section 4.5's oracle_output shows the same oracle with "valid_phases": ["ASSERT"]. These are consistent under exactly one reading: violation.phase is the lifecycle phase in which the WITNESS EVENT occurred (the stale read happened at t=14320ms during DRIVE), while oracle.valid_phases is the set of phases in which EVALUATING the oracle is legal (ASSERT only). An implementer who conflates them will either reject the frozen verdict example as invalid or conclude that consistency oracles may be evaluated during DRIVE: the latter being exactly what I5 forbids and what A.5's early-termination rule would encourage.

**Recommendation:** Document the distinction in doc comments on both pkg/schema types at Phase 0, and add a round-trip test asserting a violation with phase=DRIVE from an oracle with valid_phases=[ASSERT] is well-formed.

### F8 [RESOLVABLE-WITH-DECISION] 'Supersedes' vs 'fall back to': the stochastic mutation loop is a required Phase 4 deliverable, not a superseded one

Addendum A preamble: "This addendum supersedes the coverage-guided mutation loop described in Phase 4 Section 6 ... Where the base spec describes stochastic corpus mutation, this addendum replaces it with a structured adversarial search." A.5 Budget Allocation: "If no REINFORCE signal is detected during probing, fall back to the base spec's stochastic corpus mutation for the remaining budget. The Saboteur is an accelerant, not a replacement for baseline coverage." You cannot fall back to an engine you deleted, and the final sentence explicitly retracts 'replaces'. Note this fallback is the COMMON path on a system with no reinforcing feedback, so it is not an edge case.

**Recommendation:** Read 'supersedes' as 'demotes from default engine to fallback engine'. CRUCIBLE section G (search-loop pseudocode, energy rules, rare-event bias, overlap-biased mutation) remains the normative spec of the fallback and must be fully implemented in Phase 4. Add to A.9's DoD: 'thesis search --strategy random independently satisfies the base spec Phase 4 DoD (finds the fixture bug within 30 minutes from an empty corpus)'; otherwise the fallback path ships untested.

### F9 [RESOLVABLE-WITH-DECISION] strategy: hybrid is undefined config surface

A.10 declares "strategy: saboteur # saboteur | random | hybrid (default: saboteur)". The token 'hybrid' appears exactly once across all three normative documents, as this enum value. Its behaviour is specified nowhere. A.5 defines only the saboteur path plus the no-REINFORCE fallback, which means 'saboteur' is already hybrid in the degenerate case, leaving 'hybrid' with no distinct meaning.

**Recommendation:** Define the three modes precisely in DECISIONS.md and the config schema: random = CRUCIBLE section G loop only, no probe sweep or MCTS; saboteur = probe sweep for probe_budget_pct then MCTS on top REINFORCE signals, auto-falling back to random if no signal fires; hybrid = after the probe sweep, split the escalation budget unconditionally between MCTS and the mutation loop over one shared corpus and coverage map. Add search.hybrid_split_pct (default 30, percent of escalation budget to mutation). Rationale: MCTS is deep-and-narrow, mutation is broad-and-shallow per I4; complementary over an 8h soak, competing over a 30m search.

### F10 [ACCEPT-AND-DOCUMENT] **(AFFECTS PHASE 0)** The single-agent MDP framing is correct; cross-session patching is non-stationarity, not co-evolution, and needs cache-keying not a game solver

A.1: "The system under test is not a strategic player ... do not implement two-player game solvers (e.g., CFR, Nash equilibrium search)." Within a session this is exactly right: the SUT has no objective function, no model of the Saboteur, and no policy that adapts to the Saboteur's actions; its nondeterminism (thread scheduling, GC pauses, network jitter) is exogenous randomness with a fixed unknown distribution. CFR/Nash require a second player with preferences and a strategy space; neither exists, and computing a best response to a non-responsive opponent is wasted work. Across sessions, A.1 point 3's 'adaptive re-engagement' is NOT adversarial co-evolution, for three reasons: (1) in the intended loop (CRUCIBLE section J) the coding agent and the Saboteur share the objective 'no bug survives' (a common-payoff game with no strategic content; (2) where the patcher IS adversarial) weakening an oracle, narrowing allow, shortening a budget, deleting a regression; the correct response is the I6 / CRUCIBLE section E commitment device, which REMOVES those moves from the action space ("Mismatch returns exit 4 and REFUSES TO RUN"); you do not solve a game against a player whose adversarial moves you have made illegal; (3) the SUT is fixed within a session and changes between them, which is episode-level non-stationarity, not co-evolution.

**Recommendation:** Keep the single-agent framing; implement no game solver. The one real consequence is cache invalidation keyed on SUT build identity: DISCARD MCTS trees, visit counts, REINFORCE/DAMPEN classifications and probe rankings when the build changes; DECAY (do not reset) corpus energy; KEEP unconditionally the regression corpus (CRUCIBLE section D) and the coverage baseline (needed for coverage_delta_vs_baseline, anti-gaming mechanism section E.3). That key is the SUT block from the earlier finding.

### F11 [RESOLVABLE-WITH-DECISION] **(AFFECTS PHASE 0)** 'search' is NOT a YAML collision, but A.10's default would make thesis gate run an adaptive MCTS search

The suspected collision is refuted syntactically: section 4.2 has profiles.soak.search (scalar bool) and A.10 has $.search (mapping); distinct YAML paths, no parser conflict, and no forced Go struct unification unless the author naively shares a field name. The REAL conflict is contradictory defaults for whether the Saboteur runs at all. Section 4.2 sets 'search: true' on soak ONLY ("soak: { budget: 8h, worlds: -1, driver_profile: soak, search: true }"), implying search is off for smoke and gate. A.10 makes 'strategy: saboteur' the default, reading as on-everywhere. If A.10 wins, 'thesis gate --profile gate --json' (CRUCIBLE section J: 10 minutes, pre-commit, every agent iteration) executes a stochastic MCTS search, making the gate's pass/fail signal vary run-to-run and therefore unusable in an agent loop. It also does not fit: A.3's sweep is one micro-probe per allowed kind per target, roughly 21 probe worlds at 25-60s = 9-21 minutes, against a probe budget of 20% of 10 minutes = 2 minutes.

**Recommendation:** Rename nothing (Core Rule 2 freezes both). Define them as orthogonal: profiles.<p>.search (bool, default FALSE) is the ENABLE switch; top-level search: is the CONFIGURATION applied only when enabled, with A.10's saboteur default scoped inside an enabled profile. Consequence: smoke and gate never run the Saboteur; soak does; 'thesis search' implies enabled regardless of profile. Go types: Profile.Search *bool (pointer so absent is distinguishable from false, which matters for lock hashing) and Config.Search *SearchConfig.

### F12 [RESOLVABLE-WITH-DECISION] **(AFFECTS PHASE 0)** The top-level search: block must be an input to .prothesis/lock, or it becomes the easiest gate-weakening vector

CRUCIBLE section E.2: "Fault-space floor. Removing a kind from allow, narrowing max_faults_per_world, or shortening a profile budget are ALL lock changes." Every A.10 knob is a fault-space narrowing under that rule: setting strategy: random, lowering probe_budget_pct, dropping max_mcts_depth to 1, or flipping profiles.soak.search to false all weaken the search without touching any oracle definition. An agent trying to turn the gate green would find 'max_mcts_depth: 1' a far easier and less conspicuous edit than touching .prothesis/oracles. Addendum A never mentions the lock.

**Recommendation:** Include the canonicalized top-level search: block and every profiles.*.search flag in the .prothesis/lock manifest input set. internal/lock must reserve this at Phase 0; trivial now, a lock-format break later.

### F13 [BLOCKING] **(AFFECTS PHASE 0)** A.6 requires a severity field that prothesis.oracle_output/v1 does not define, and severity outweighs the entire class hierarchy

Section 4.5's oracle_output/v1 defines schema, oracle, class, valid_phases, status, witness, explanation: there is NO severity field. Section 4.6's verdict has "severity": "high", and A.6's severityMultiplier returns 1.5/1.0/0.5, multiplying the whole violation reward. That is a 3x swing, larger than consistency-vs-crash (100/80 = 1.25x), consistency-vs-safety (1.67x) and consistency-vs-resource (2.5x); only consistency-vs-liveness (5x) exceeds it. So an undefined field with no specified source controls the Saboteur's search direction more than the normative class table does. Concretely: a low-severity consistency violation (50 - 6 - 5.5 = 38.5) loses decisively to a high-severity crash (120 - 6 - 1.2 = 112.8).

**Recommendation:** Add Severity string `json:"severity,omitempty"` to the OracleOutput struct NOW as an optional, additive, backward-compatible extension (it changes no existing semantics, unlike editing a frozen field). Engine derives severity from a per-class default table in prothesis.yaml when the oracle omits it; A.6's 'default: return 1.0' covers unknown strings. Doing this in Phase 0 avoids touching a frozen schema in Phase 3.

### F14 [ACCEPT-AND-DOCUMENT] A.6's utility switch scores differential and metamorphic violations at zero

CRUCIBLE section B defines eight oracle classes: "crash, consistency, liveness, convergence, resource, safety, differential, metamorphic". A.6's switch v.Class covers six, with no default case. A differential or metamorphic violation therefore contributes 0.0 utility, so the Saboteur is blind to that entire class and will never search toward it, and worse, a world that finds one scores NEGATIVE after the parsimony penalty, so the search actively avoids it.

**Recommendation:** Add 'default: u += 60.0 * severityMultiplier(v.Severity)' (matching safety) to the Utility switch and record the deviation in DECISIONS.md, as A.6 explicitly requires for any modification.

### F15 [RESOLVABLE-WITH-DECISION] A.3's probe sweep drops the no-fault control world that every DAMPEN/REINFORCE criterion depends on

A.3 says it "replaces the base spec's 'seed worlds (one per allowed fault kind)'" but CRUCIBLE section G's actual seed line is "seed worlds (one per allowed fault kind, plus one no-fault control)": the control is silently dropped. Yet every A.3 classification criterion is defined relative to a baseline that is never established: "Metrics returned to baseline within 2x the fault window" and "Retry count exceeded 2x baseline during or after the fault window". Compounding this, A.10 sets "reinforce_threshold: 0.0 # minimum positive trend slope to classify REINFORCE"; against noisy real telemetry, any positive regression slope classifying as REINFORCE yields roughly a 50% false-positive rate, which would send Tier 2 escalation (80% of the entire budget) after noise.

**Recommendation:** Mandate that the probe sweep begins with N=3 no-fault control worlds establishing per-metric mean and standard deviation. Redefine reinforce_threshold's default as a multiple of control-run standard deviation (suggest 2.0 sigma) rather than an absolute 0.0 slope.

### F16 [BLOCKING] Addendum A states three different telemetry sampling rates, and its detector cannot fire inside its own probe windows

A.3 step 2: "Telemetry trajectory: sampled every 200ms during DRIVE". A.4 source 1: "Sampled every 500ms." A.10: "sample_interval_ms: 500". Worse, A.4's ClassifySignal computes trend with windowSize=5 then acceleration with windowSize=3 over those trends, requiring a minimum of 7 samples: approximately 3.5 seconds at 500ms. A.3's probe worlds use a "short window (500ms-2000ms)". No probe can therefore ever produce REINFORCE_ACCELERATING, which is A.9 Definition of Done item 1 verbatim: "correctly classifies proc.pause on the leader node as REINFORCE_ACCELERATING". The Phase 4 DoD is unachievable at the stated constants.

**Recommendation:** Split into two independent samplers: observe.metrics_interval_ms (default 500, expensive container/process metrics) and observe.trajectory_interval_ms (default 200, cheap driver/health counters). Classify from the 200ms stream. Extend probe fault windows to at least 3s so the detector has a valid observation window, and record the deviation from A.3's 500ms-2000ms in DECISIONS.md.

### F17 [RESOLVABLE-WITH-DECISION] A.7 Rung 3 requires an asymmetric partition the frozen TARGET grammar cannot express

A.7: "Rung 3 - PARTITION: net.partition (asymmetric preferred). Purpose: Force quorum reconfiguration under load." Section 4.3's TARGET grammar provides "edges (n1<->n2)": bidirectional only. There is no directed-edge syntax, so a one-way drop (the highest-value partition variant for surfacing split-brain and lease-expiry bugs, and the one most likely to trigger the fixture flaw described in section 5) is unrepresentable in the fault grammar.

**Recommendation:** Extend TARGET with a directed edge form n1->n2, purely additive and non-breaking. Phase 0 impact is nil because PlannedFault.Target is an opaque string, but document the grammar extension NOW: adding a fault target form later is a lock bump under CRUCIBLE section E.2, so it is cheaper to include in the initial lock manifest.

### F18 [ACCEPT-AND-DOCUMENT] **(AFFECTS PHASE 0)** A.8 claims a determinism the system cannot have, contradicting CRUCIBLE PART 5

A.8 RECORDER row: "All Saboteur decisions (fault selection, tree expansion) must be deterministic given the same PRNG seed for Tier B reproducibility." This is false even under the offline reading: tree expansion depends on the measured utilities of prior REAL rollouts, which are not seed-determined (they depend on real network jitter, GC pauses and thread scheduling in the SUT). Only sampling, tie-breaking and expansion order can be seeded. This directly contradicts CRUCIBLE PART 5: "Replay reproduces with high probability, NOT certainty ... never claim determinism it does not have."

**Recommendation:** Restate A.8's row as: all Saboteur randomness is drawn from a dedicated RECORDER PRNG stream; the search TRAJECTORY is not reproducible; reproducibility is delivered by the realized world files the search emits, not by re-running the search. Phase 0 consequence: internal/recorder needs NAMED, INDEPENDENT PRNG streams (driver, perturber, topology, search) so that consuming search randomness does not shift driver randomness; a classic determinism bug. Reserve the 'search' stream now even though nothing draws from it until Phase 4.

### F19 [ACCEPT-AND-DOCUMENT] MCTS at the specified budgets is prior-dominated; UCT is a tie-breaker, and the tree needs progressive widening

A.9 requires discovery "within 10 minutes from a cold start" at A.5's stated "25-60 seconds each" per rollout: a total of roughly 10 to 24 rollouts. A.5's action space is "the full fault grammar filtered to actions compatible with current perturber.budget constraints": with section 4.2's 7 allowed kinds across roughly 6 target forms that is about 42 children per node, at "Maximum tree depth: 4". With 24 rollouts, UCT never finishes expanding the root's children even once, so the exploration term never engages and the escalation ladder (A.7) is doing effectively all the search work.

**Recommendation:** Implement progressive widening: expand children strictly in A.7 ladder order and admit a new child only after existing ones reach k visits. Describe the algorithm honestly in DECISIONS.md as 'ladder-prior best-first search with UCT tie-breaking'. Deferrable to Phase 4, but it determines whether A.9 DoD item 2 is achievable at all.

### F20 [ACCEPT-AND-DOCUMENT] **(AFFECTS PHASE 0)** CRUCIBLE section B misstates the on-disk built-in oracle list, and thesis init is being written today

CRUCIBLE section B claims "the on-disk 4.2 sample YAML omits no_stuck_op from oracles.builtin". That is false: PRO_THESIS_FABLE_PROMPT.md line 168 lists no_stuck_op. Section B also says CRUCIBLE 'requires SEVEN' built-ins and then lists six. The correct set is six, all six already present in section 4.2, all six required by Phase 1. Since Phase 0 implements 'thesis init', which scaffolds prothesis.yaml, an implementer following section B would 'fix' a non-existent omission or invent a seventh oracle.

**Recommendation:** Scaffold exactly the six built-ins from section 4.2: no_crash, no_panic_log, no_unbounded_queue, resource_return_to_baseline, availability_after_heal, no_stuck_op. Note the CRUCIBLE section B inaccuracy in OPEN_QUESTIONS.md so it is not re-litigated in Phase 1.

## Phase 0 deltas (15)

- **pkg/schema/world.go**: Split the World type into Plan (pre-execution intent) and Realized (post-execution facts), with Realized as *Realized so an unexecuted world omits it. Plan holds seed, seed_streams, topology_variant, driver_profile, profile, phase_budgets, fault_schedule. Realized holds faults (with resolved_targets and actual injected_at_ms/withdrawn_at_ms), measured phase_timings, duration_ms, terminated_early, heal_verified, artifact refs.
  - _why:_ I2's five-tuple cannot express late-bound targets (role:leader, minority(kv)) that the base spec's own Phase 2 DoD depends on, and section 4.5's oracle_input passes MEASURED phase boundaries. Required by Phase 2 regardless of the Saboteur; free to add today while pkg/schema is empty, expensive to retrofit in Phase 4.
- **pkg/schema/world.go**: Add an SUT struct to World: commit, dirty, image_digests (map service -> sha256), config_sha, lock_sha.
  - _why:_ I2's tuple omits build identity. Without it, 'thesis replay' against a different commit silently produces meaningless results, 'thesis bisect WORLD --good SHA --bad SHA' (CRUCIBLE PART 7) has no defined semantics, and 'reproduced: 3/3' is unfalsifiable. Also the required cache key for invalidating MCTS trees across patches.
- **pkg/schema/world.go**: Canonical encoding rules, enforced by a round-trip test: no float fields anywhere in World or its children; all times int64 milliseconds on the virtual clock relative to DRIVE start; fault params typed map[string]string, never map[string]interface{}; thresholds integer-scaled (value_milli).
  - _why:_ Phase 0's Definition of Done is 'A world configuration round-trips through .thesis serialization byte-identically'. Any float64 (e.g. A.6's 0.1 duration penalty, A.10's 1.41 exploration constant) breaks byte-identity across encoders. Utility constants belong in config and verdicts, never in the world file.
- **pkg/schema/world.go**: Give every PlannedFault a stable ID field, and add ResolvedTargets plus PlanID linkage on RealizedFault.
  - _why:_ One identity referenced by four consumers: ddmin element selection (Phase 5), MCTS node paths (Phase 4), causal_timeline entries (section 4.6), and the realized-to-planned mapping. Without it, shrinking a world whose faults were reordered silently drops the wrong fault.
- **pkg/schema/world.go**: Add the trigger union to PlannedFault: TriggerKind enum ('at' | 'when'), StartMS/EndMS for 'at', and a *TriggerPredicate (event, subject, detail, op, value_milli, nth, offset_ms) plus duration_ms/deadline_ms for 'when'. Phases 0-3 emit only 'at'; the parser accepts both.
  - _why:_ This is the single field that makes Phase 4 free. Event-conditioned injection times are what let the Saboteur exploit observed weakness without abandoning I2: the schedule stays a fixed serializable program. Reserving the variant now costs one enum and one struct; adding it in Phase 4 is a .thesis format break affecting every stored regression.
- **pkg/schema/world.go**: Add a Replay block: mode ('realized' | 'planned'), tier ('B'), confirm_k (default 3), attempts, successes, reproduced ('3/3').
  - _why:_ CRUCIBLE PART 5 requires 'The verdict must report reproduced 3/3 or 1/5 and never claim determinism it does not have'. Two modes are needed because faults timed to real events (an election at t=9010ms) do not reproduce well at absolute times; the Phase 5 confirmation gate must be able to try realized, fall back to planned, and persist whichever measured higher.
- **pkg/schema/world.go**: Add a Lineage block: parent_id, derivation (seed|control|probe|mutate|mcts_expand|shrink|replay|manual), strategy, mcts_path []string, shrink_step.
  - _why:_ Serves I4's corpus provenance, A.5's tree reconstruction, and Phase 5's shrink chain from one field set. 'control' and 'probe' derivations are needed by A.3's baseline worlds. Enumerating the values now prevents Phase 4 inventing a parallel provenance mechanism.
- **pkg/schema/world.go**: Add ArtifactRefs (dir plus history_sha / telemetry_sha / final_state_sha) and reference telemetry, history and logs by path+digest. Never embed them in the world file.
  - _why:_ A.4 mandates telemetry sampled every 200-500ms across every node for the whole DRIVE phase; megabytes per world. Embedding breaks both the byte-identical round-trip and any reasonable regression-corpus size. Digests preserve tamper-evidence for I6.
- **internal/recorder/prng.go**: Implement NAMED, INDEPENDENT PRNG streams derived from the world seed: 'driver', 'perturber', 'topology', 'search'. Serialize the derived per-stream seeds into Plan.SeedStreams.
  - _why:_ If search randomness is drawn from the same stream as driver randomness, enabling the Saboteur shifts every workload operation and no pre-Saboteur world replays. Reserve the 'search' stream now even though nothing draws from it until Phase 4. This also makes A.8's determinism claim precisely scopeable to 'randomness', not 'decisions'.
- **pkg/schema/config.go**: Model the two search keys as orthogonal: Profile.Search *bool (pointer, default nil meaning false) as the ENABLE switch, and Config.Search *SearchConfig (strategy, exploration_constant, probe_budget_pct, max_mcts_depth, hybrid_split_pct, escalation_ladder, utility{...}, observe{metrics_interval_ms, trajectory_interval_ms, spiral_window, reinforce_threshold}) as the CONFIGURATION. Document that A.10's saboteur default applies only inside a profile with search: true.
  - _why:_ There is no YAML collision (distinct paths), but A.10's default would otherwise make 'thesis gate --profile gate' run an adaptive MCTS search, making the agent loop's pass/fail signal non-reproducible. The pointer bool is required so 'absent' is distinguishable from 'false' when hashing for the lock.
- **pkg/schema/oracle.go**: Add Severity string `json:"severity,omitempty"` to the OracleOutput type as an optional additive extension, and document in doc comments that violation.phase (phase of the witness event) is a different field from oracle.valid_phases (phases in which evaluation is legal).
  - _why:_ A.6's severityMultiplier applies a 3x swing that outweighs every class distinction except liveness, but prothesis.oracle_output/v1 defines no severity field, so an undefined value would steer the entire search. Adding it now is additive and backward-compatible; adding it in Phase 3 means editing a frozen schema, which Core Rule 2 forbids. The phase-field distinction prevents an implementer rejecting section 4.6's own frozen example as invalid.
- **internal/lock/manifest.go**: Reserve the canonicalized top-level search: block and every profiles.*.search flag as inputs to the .prothesis/lock manifest, alongside oracle definitions, the allowed fault set and profile budgets.
  - _why:_ CRUCIBLE section E.2 makes any fault-space or budget narrowing a lock change. Setting strategy: random, dropping max_mcts_depth to 1, or flipping search to false all weaken the gate without touching an oracle; the path of least resistance for an agent trying to turn the gate green. Reserving the input set now is trivial; changing the manifest input set later invalidates every existing lock.
- **cmd/thesis/init.go**: Scaffold exactly the six built-in oracles from section 4.2 (no_crash, no_panic_log, no_unbounded_queue, resource_return_to_baseline, availability_after_heal, no_stuck_op), and scaffold profiles with search absent on smoke/gate and search: true on soak only.
  - _why:_ CRUCIBLE section B incorrectly claims the on-disk config omits no_stuck_op (it is present at line 168) and says 'SEVEN' while listing six; an implementer following it would invent a seventh oracle. The profile scaffolding encodes the search-enablement resolution so the gate never runs an adaptive search.
- **OPEN_QUESTIONS.md**: File four entries: (1) A.5's root-node prose describes an online tree that its own UCT visit counters make unimplementable; (2) A.5's 'terminate the rollout immediately' contradicts section 4.3's HEAL withdraw() Critical Guarantee; (3) A.4's 7-sample detector window (3.5s at 500ms) exceeds A.3's 500-2000ms probe windows, making A.9 DoD item 1 unachievable as written; (4) prothesis.oracle_output/v1 defines no source for the severity field that A.6 requires.
  - _why:_ Base Core Rule 4: 'If a requirement is unimplementable as written, stop and document the conflict in OPEN_QUESTIONS.md. Never silently relax a requirement.' All four are unimplementable-as-written, and three of them are resolved by Phase 0 schema decisions that need their rationale on record.
- **DECISIONS.md**: Record: the offline MCTS reading plus event-trigger schedules; planned/realized world split; gamma_duration applied to scheduled rather than realized duration; early termination restricted to crash/safety oracles with HEAL/QUIESCE/ASSERT always running; A.6 default case scoring differential/metamorphic at 60; the three strategy mode definitions; search-key orthogonality; reinforce_threshold as sigma-relative rather than 0.0.
  - _why:_ A.6 states the utility function 'is normative and must not be modified by the implementing agent without recording the change in DECISIONS.md', and base Core Rule 3 requires recording rationale wherever behaviour is specified but implementation is open. Two of these (gamma_duration, the default case) are direct modifications to A.6.

## Deferred

- Progressive widening in the MCTS expansion policy (expand A.7 ladder children in order, admit a new child only after existing ones reach k visits). Needed for A.9 DoD item 2 to be achievable at 10-24 rollouts against a ~42-wide action space, but it is pure Phase 4 search logic with no schema surface.
- The hybrid_split_pct interleaving implementation and the shared corpus/coverage-map plumbing between the MCTS and mutation engines. The config field must exist in the Phase 0 struct; the behaviour is Phase 4.
- Two-rate telemetry sampling (metrics_interval_ms 500 / trajectory_interval_ms 200) and lengthening probe windows to >=3s so ClassifySignal's 7-sample minimum fits. Config fields land in Phase 0; the samplers are Phase 4.
- The N=3 no-fault control worlds establishing per-metric baseline mean and sigma, and redefining reinforce_threshold as sigma-relative. Pure Phase 4 probe-sweep behaviour; the Lineage 'control' derivation value is the only Phase 0 surface.
- Directed-edge TARGET syntax (n1->n2) for A.7 Rung 3 asymmetric partitions. The world file stores targets as opaque strings so no format change is needed, but document the grammar extension before the first lock is written, since adding a target form later is a lock bump under CRUCIBLE section E.2.
- Phase 5 window re-widening after binary-search timing narrowing (narrow for the report, then widen to the smallest window still achieving k/k). Minimality and replay robustness trade off directly here; it is a Phase 5 shrink-pipeline decision, but note it now so narrowing is not implemented as irreversible.
- Online incremental consistency checking with sound handling of indeterminate (info) operations, which would allow consistency oracles to terminate rollouts early. Revisit only if a Phase 3 checker makes it affordable; consistency stays valid_phases: [ASSERT] in v1.
- MCTS tree and REINFORCE-classification cache invalidation keyed on SUT identity across gate invocations, with corpus energy decayed rather than reset. The SUT key must exist in Phase 0; the invalidation policy is Phase 4/6.
- Branch coverage as a third signal. CRUCIBLE section F ranks it highest-fidelity but it 'needs build cooperation', which conflicts with CRUCIBLE section L's requirement to work on unmodified systems. Log-template coverage first, per section F.

## Analysis

## 0. Ground truth established before analysis

- `C:\AI Projects\Pro-synthesis\PRO_THESIS_FABLE_PROMPT.md` and `CRUCIBLE_FABLE_PROMPT.md` are byte-identical (23,404 bytes each), confirming the CRUCIBLE addendum's own claim.
- `C:\AI Projects\Pro-synthesis\pkg\schema`, `internal\*`, `cmd\thesis` all exist and are **empty**. `go.mod` declares `github.com/prothesis/prothesis`, go 1.27.1.
- **No Go code exists yet.** The `.thesis` format is not merely "about to be frozen": it is entirely unwritten. Every recommendation below is free.
- Git Bash confirmed broken (`dofork: child -1 ... exit code 0xC0000142`). Analysis done via PowerShell/Read.

---

## (a) The primary conflict: does A.5 describe intra-world adaptation or per-world tree expansion?

### The two readings

**Reading 1: ONLINE / INTRA-WORLD.** The tree is over decision points inside a single live execution. Textual support:

> A.1: "The Saboteur observes the system's **live telemetry** and selects faults that exploit observed weakness."
> A.4: "Continuously classify the system's **real-time telemetry during any world execution**" … "A `REINFORCE_ACCELERATING` classification on any metric triggers **immediate** escalation to Tier 2."
> A.5: "**Root node**: The **current observed distributed state at the moment REINFORCE was detected**, plus the active probe fault." … "**Child nodes**: Each child represents an additional fault action from the grammar, applied at the **current virtual clock time**."

**Reading 2: OFFLINE / PER-WORLD.** Each tree node is a *fault-schedule prefix*. A rollout compiles that prefix plus a ladder-guided continuation into a complete schedule, executes one full world `BOOT`→`TEARDOWN`, and backpropagates. "Current virtual clock time" means "the schedule position reached by the parent node," not wall-clock *now*.

### Reading 1 is not implementable. Four independent proofs from A.5's own text.

1. **UCT requires node revisitation; live distributed state cannot be revisited.**
   > "`UCT(node) = (U_avg / visits) + C × sqrt(ln(parent.visits) / visits)`"
   > "**Pruning**: If a node has been visited **≥3 times** with zero coverage delta and zero violations…"

   `visits` is only meaningful if a node can be entered more than once. Under Reading 1, the root node *is* a specific live moment in a specific container set. It occurs exactly once and is destroyed by the first action taken from it. There is no undo, no snapshot/restore: I1 forbids simulation ("Fakes and mocks are strictly forbidden in the system under test") and Tier A deterministic simulation is explicitly v4 (CRUCIBLE PART 5). `visits` is permanently 1 for every node, UCT degenerates to `U_avg + C×sqrt(ln(1)/1) = U_avg + 0`, and the pruning rule can never fire. **This is decisive on its own.**

2. **"Each rollout executes a real world (not simulated)."** A *world* is the base spec's unit of execution: the full §4.1 lifecycle, serialized as one `.thesis` file. Under Reading 1 a rollout would be a *continuation*, not a world.

3. **"rollouts are expensive (25-60 seconds each)."** That is the duration of an entire world (BOOT + SEED + DRIVE + HEAL + QUIESCE + ASSERT), not of a continuation from a REINFORCE moment mid-`DRIVE`, which would be single-digit seconds.

4. **A.8 demands seed-determinism.**
   > "All Saboteur decisions (fault selection, tree expansion) must be deterministic given the same PRNG seed for Tier B reproducibility."

   Under Reading 1 this is flatly impossible: decisions are a function of live telemetry, which is not seed-determined. The author of A.8 evidently believes the tree is built over plans.

**Verdict on (a): the conflict with I2 is real as written, but Addendum A is internally self-contradictory rather than coherently adaptive. The *rhetoric* (A.1, A.4, A.5 root-node prose) is Reading 1; every piece of *normative machinery* (UCT, `visits`, the ≥3-visit prune, real-world rollouts, A.8 determinism) requires Reading 2. Reading 2 is the operative intent and the only implementable one.**

### But the suspected conflict is mis-located, and this is the important finding

Under Reading 2 the naive I2 story ("the schedule is fully known before execution") **still fails, because the base spec already breaks it, with no Saboteur involved.**

> §4.3: "`TARGET`: node (`n1`), wildcards (`kv:*`), **roles (`role:leader`)**, edges (`n1<->n2`), **quorums (`minority(kv)`, `majority(kv)`)**."
> §6 Phase 2 DoD: "Executing a scripted fault schedule (`proc.pause(role:leader)@8200..15100` …)"

`role:leader` is resolved **at injection time, from live cluster state**. Which container is leader at t=8200ms on this run is a function of real, non-reproducible timing: exactly the objection raised against the Saboteur. `minority(kv)` likewise depends on live membership. And the *canonical bug trigger* in the entire project (the Phase 2 DoD schedule, the fixture flaw in §5, the verdict example in §4.6) uses `role:leader`.

Two further confirmations that the frozen schemas already assume a realized layer:

- §4.5 `prothesis.oracle_input/v1` passes `phases: [{"phase":"DRIVE","start_ms":0,"end_ms":42000}, …]`. Those are *measured* boundaries, not planned budgets (42000/45000/50000/55000 are outcomes, and I2's tuple calls this `phase_timings` as if it were an input).
- §4.6 verdict `causal_timeline` records `{"t_ms":8200,"event":"fault","detail":"proc.pause **n2** (leader, term 4)"}`: the concrete node `n2`, resolved from the symbolic `role:leader`. That resolution has to be recorded somewhere, and today nothing in I2's tuple holds it.

**So: I2's five-tuple is under-specified independent of Addendum A. The world file needs a planned/realized split for Phase 2 regardless. The Saboteur does not create this problem; it makes an existing gap unignorable, and it is far cheaper to fix now, in an empty `pkg/schema`, than in Phase 4.**

### The reading I recommend adopting: Reading 1.5

There is a third position that captures the genuine value of Reading 1 without breaking I2:

**Late-bound triggers.** A fault's *time* is bound by a predicate over observed events, exactly as its *target* is already bound by `role:leader`. E.g. "inject `net.partition(minority(kv))` 200 ms after the first role change on any `kv` node." The schedule is a fixed, serializable **program**; only its firing instants are data-dependent. This is what actually finds the lease bug (you must partition *around* an election you did not schedule), it is a strict generalization of the late binding the base spec already concedes, and it is trivially serializable.

Recommendation: **implement Reading 2 for the MCTS control loop** (nodes are plan prefixes; rollouts are whole worlds) and **support Reading 1.5 in the fault schedule** (triggers may be `at` or `when`). Phase 0 must reserve the `when` variant in the format even though Phases 0–3 will only ever emit `at`.

---

## (b) The realized-schedule resolution: evaluated honestly

The proposal (Perturber records what was actually injected at what virtual-clock times; `.thesis` stores that realized schedule; replay executes fixed actions) is **correct and necessary, but it is not sufficient on its own, and the spec already tells us why.**

### It is legitimate, and CRUCIBLE PART 5 pre-authorizes it

> "**Tier B: record and replay (v1 default).** All *orchestration-level* nondeterminism is drawn from RECORDER … The system under test keeps its own internal nondeterminism. Replay reproduces with **high probability, NOT certainty**. The verdict must report `reproduced: "3/3"` or `"1/5"` and **never claim determinism it does not have**."

Record-and-replay of a realized schedule *is* Tier B. There is no invariant violation in adopting it. I2 says a world file "reproduces the issue"; Tier B defines reproduction probabilistically and requires the rate be reported. `minimal_repro.reproduced: "3/3"` in §4.6 is exactly this measurement.

### The cost is real and is exactly the one suspected

A fault timed to a real event does not replay well by absolute time. Concretely, from the §4.6 example timeline: the partition at t=8400 ms was placed to catch an election that happened at t=9010 ms. On replay the election may land at 8600 ms or 9900 ms. A fixed 8400 ms injection then misses the causal window and the world does not reproduce. Reproduction rate degrades **worst on exactly the event-triggered faults that make the search valuable**.

Mitigations, in order of value:

1. **Two replay modes, chosen empirically, recorded in the file.** `mode: realized` replays concrete targets at absolute times. `mode: planned` re-executes the symbolic program, re-resolving `role:leader` and re-evaluating `when` predicates against the fresh run: reproducing the *strategy* rather than the *instance*. The Phase 5 confirmation gate (`k=3`) should try realized first, fall back to planned, and **persist whichever mode achieved the higher k/n, plus the measured rate**. This is a small, concrete policy and it converts an unsolvable determinism problem into a measured one.

2. **Phase 5's timing narrowing must be reversible.** §6 Phase 5 step 3 is "Binary search timing narrowing on surviving fault windows." Narrowing *minimizes* the window and *reduces* replay robustness: a 100 ms window will miss a jittery election that a 2000 ms window catches. Minimality and reproducibility are in direct tension here. Mandate: narrow for the report, then **re-widen to the smallest window that still achieves k/k**, and store the widened window. Record in `DECISIONS.md`.

3. **Store the trigger provenance next to the realized time.** Each realized fault records `triggered_by` (the observation that fired it). This is what lets planned-mode replay exist at all, and it is what feeds `causal_timeline`.

Other costs, all small:

- **Storage**: a few hundred bytes per fault. Negligible.
- **Phase 0 DoD risk**: "A world configuration round-trips through `.thesis` serialization byte-identically." Adding a realized section does not endanger this *provided* the encoding is canonical. It **does** endanger it if any float appears: `0.1` round-trips through `float64` unpredictably across encoders. See the Phase 0 deltas: **integer-only time, no `float64`, no `map[string]interface{}` anywhere in the `World` type.** Fault params become `map[string]string` and are parsed by the Perturber, not the serializer.

**Verdict on (b): adopt it, but adopt it as one half of a planned/realized pair, not as a replacement for the plan. Storing only the realized schedule loses the ability to re-run the strategy after a patch, which is A.1 point 3 ("Adaptive re-engagement") and the whole point of `thesis regress` post-fix. Storing only the plan loses reproduction. The file must carry both.**

---

## (c) The `.thesis` format: the actionable output

This is `pkg/schema/world.go`, written to satisfy both readings so Phase 4 changes nothing.

```go
package schema

// SchemaVersion is the frozen identifier for all prothesis artifacts.
const SchemaVersion = "prothesis/v1"

// World is the serialized .thesis file: the I2 artifact.
//
// I2's tuple (seed, topology_variant, driver_profile, fault_schedule, phase_timings)
// is split into two layers, because the base spec's own fault grammar already binds
// targets late (role:leader, minority(kv)) and its oracle_input schema already passes
// MEASURED phase boundaries:
//
//	Plan     - decided BEFORE the world ran. May contain symbolic targets and event
//	           triggers. This is what the search engine chose.
//	Realized - what ACTUALLY happened: concrete node ids, concrete virtual-clock
//	           times, measured phase boundaries. Written by the RECORDER.
//
// Regression replay uses Realized. Post-patch strategy replay uses Plan.
// Canonicalization rules (required by the Phase 0 byte-identical round-trip DoD):
//   - no float fields anywhere in this type or its children
//   - no map[string]interface{}; all params are strings
//   - all times are int64 milliseconds on the virtual clock, relative to DRIVE start
type World struct {
	Schema   string    `json:"schema"`  // "prothesis/v1"
	Kind     string    `json:"kind"`    // "world"
	ID       string    `json:"id"`      // stable, e.g. "w_a41f"
	Created  string    `json:"created"` // RFC3339 UTC
	SUT      SUT       `json:"sut"`
	Plan     Plan      `json:"plan"`
	Realized *Realized `json:"realized,omitempty"` // absent until executed
	Replay   Replay    `json:"replay"`
	Lineage  Lineage   `json:"lineage"`
}

// SUT identifies the exact system this world ran against.
//
// I2's tuple OMITS this, which makes replay and `thesis bisect WORLD --good SHA
// --bad SHA` unsound: replaying a world against a different binary is not a
// reproduction, and "reproduced: 3/3" against an unknown build means nothing.
type SUT struct {
	Commit       string            `json:"commit"`                  // git SHA of the target repo
	Dirty        bool              `json:"dirty"`                   // uncommitted changes present
	ImageDigests map[string]string `json:"image_digests,omitempty"` // service -> "sha256:..."
	ConfigSHA    string            `json:"config_sha"`              // sha256 of canonical prothesis.yaml
	LockSHA      string            `json:"lock_sha"`                // .prothesis/lock manifest sha (I6)
}

type Plan struct {
	Seed            uint64            `json:"seed"`
	SeedStreams     map[string]uint64 `json:"seed_streams"` // stream name -> derived seed
	TopologyVariant string            `json:"topology_variant"`
	DriverProfile   string            `json:"driver_profile"`
	Profile         string            `json:"profile"`        // prothesis.yaml profile name
	PhaseBudgets    []PhaseTiming     `json:"phase_budgets"`  // PLANNED phase_timings
	FaultSchedule   []PlannedFault    `json:"fault_schedule"` // I2 field name kept verbatim
	DriverOps       *DriverOpPlan     `json:"driver_ops,omitempty"` // pinned ops after ddmin (Phase 5)
}

// TriggerKind discriminates how a planned fault's injection time is determined.
type TriggerKind string

const (
	// TriggerAt is a fixed window on the virtual clock, relative to DRIVE start.
	// The ONLY kind emitted in Phases 0-3, and the only kind realized-mode replay uses.
	TriggerAt TriggerKind = "at"

	// TriggerWhen binds injection time at run time from a predicate over live
	// telemetry (Addendum A / Phase 4). Not reproducible by absolute time;
	// reproducible only via Realized. Parsed but never emitted before Phase 4.
	TriggerWhen TriggerKind = "when"
)

type PlannedFault struct {
	ID     string            `json:"id"`     // stable; referenced by ddmin, MCTS paths, timeline, Realized
	Expr   string            `json:"expr"`   // canonical grammar text: KIND(TARGET[,PARAMS])@WINDOW
	Kind   string            `json:"kind"`   // net.partition | proc.pause | ...
	Target string            `json:"target"` // SYMBOLIC: n1 | kv:* | role:leader | minority(kv) | n1<->n2 | n1->n2
	Params map[string]string `json:"params,omitempty"`

	Trigger TriggerKind `json:"trigger"`

	// Trigger == "at"
	StartMS int64 `json:"start_ms,omitempty"`
	EndMS   int64 `json:"end_ms,omitempty"`

	// Trigger == "when"
	When       *TriggerPredicate `json:"when,omitempty"`
	DurationMS int64             `json:"duration_ms,omitempty"` // hold time once fired
	DeadlineMS int64             `json:"deadline_ms,omitempty"` // abandon if never fires
}

// TriggerPredicate is an event-conditioned injection time. It exists in Phase 0
// solely so Phase 4 does not have to change the frozen .thesis format.
type TriggerPredicate struct {
	Event      string `json:"event"`                  // role_change | log_template | metric_threshold | phase
	Subject    string `json:"subject,omitempty"`      // node or target the event must occur on
	Detail     string `json:"detail,omitempty"`       // template hash, metric name, phase name
	Op         string `json:"op,omitempty"`           // gt | lt | eq
	ValueMilli int64  `json:"value_milli,omitempty"`  // threshold, integer-scaled (no floats)
	Nth        int    `json:"nth,omitempty"`          // fire on Nth occurrence (default 1)
	OffsetMS   int64  `json:"offset_ms,omitempty"`    // fire this long after the event
}

// Realized is written by the RECORDER during execution. It is the reproducible
// artifact: concrete targets, concrete times, measured phase boundaries.
type Realized struct {
	Faults            []RealizedFault `json:"faults"`
	PhaseTimings      []PhaseTiming   `json:"phase_timings"` // MEASURED; source of oracle_input.phases
	DurationMS        int64           `json:"duration_ms"`
	TerminatedEarly   bool            `json:"terminated_early"`             // A.5 rollout early termination
	TerminationReason string          `json:"termination_reason,omitempty"` // oracle id that fired
	HealVerified      bool            `json:"heal_verified"`                // §4.3 zero-residual guarantee
	Artifacts         ArtifactRefs    `json:"artifacts"`
}

type RealizedFault struct {
	PlanID          string            `json:"plan_id"`          // == PlannedFault.ID; "" if unplanned
	Kind            string            `json:"kind"`
	ResolvedTargets []string          `json:"resolved_targets"` // CONCRETE node ids after role:/quorum resolution
	Params          map[string]string `json:"params,omitempty"`
	InjectedAtMS    int64             `json:"injected_at_ms"`
	WithdrawnAtMS   int64             `json:"withdrawn_at_ms"`
	WithdrawOK      bool              `json:"withdraw_ok"`
	TriggeredBy     string            `json:"triggered_by,omitempty"` // observation that fired a "when" trigger
	DecidedBy       string            `json:"decided_by"`             // plan | saboteur | operator
}

type PhaseTiming struct {
	Phase   string `json:"phase"`
	StartMS int64  `json:"start_ms"`
	EndMS   int64  `json:"end_ms"`
}

type ReplayMode string

const (
	// ModeRealized replays Realized.Faults as fixed concrete-target, absolute-time
	// actions. Default for .prothesis/regressions/*.thesis.
	ModeRealized ReplayMode = "realized"

	// ModePlanned re-executes Plan.FaultSchedule, re-resolving symbolic targets and
	// re-evaluating "when" predicates against the fresh run. Reproduces the STRATEGY,
	// not the instance. Required when realized replay reproduces poorly because the
	// original faults were timed to events that shift between runs.
	ModePlanned ReplayMode = "planned"
)

// Replay records how to re-execute this world and, honestly, how well it reproduces.
// CRUCIBLE PART 5 (Tier B) forbids claiming determinism we do not have: Reproduced
// is always a MEASURED "k/n", never asserted.
type Replay struct {
	Mode       ReplayMode `json:"mode"`
	Tier       string     `json:"tier"`      // "B" in v1, "A" in v4
	ConfirmK   int        `json:"confirm_k"` // default 3
	Attempts   int        `json:"attempts,omitempty"`
	Successes  int        `json:"successes,omitempty"`
	Reproduced string     `json:"reproduced,omitempty"` // "3/3", "1/5"
}

type Lineage struct {
	ParentID   string   `json:"parent_id,omitempty"`
	Derivation string   `json:"derivation"`          // seed|control|probe|mutate|mcts_expand|shrink|replay|manual
	Strategy   string   `json:"strategy,omitempty"`  // saboteur|random|hybrid
	MCTSPath   []string `json:"mcts_path,omitempty"` // PlannedFault IDs, root -> leaf
	ShrinkStep int      `json:"shrink_step,omitempty"`
}

// ArtifactRefs points at bulky artifacts by path + digest. Telemetry, history and
// logs are NEVER embedded in the world file — that would break both the size
// budget and the byte-identical round-trip.
type ArtifactRefs struct {
	Dir            string `json:"dir"`
	HistorySHA     string `json:"history_sha,omitempty"`
	TelemetrySHA   string `json:"telemetry_sha,omitempty"`
	FinalStateSHA  string `json:"final_state_sha,omitempty"`
}
```

**What each field buys, mapped to the readings:**

| Field | Reading 2 (offline MCTS) | Reading 1.5 (event triggers) | Base spec Phases 0–3 |
|---|---|---|---|
| `Plan.FaultSchedule` w/ `trigger: at` | the node's plan prefix | n/a | the entire schedule |
| `PlannedFault.Trigger`/`When` | unused | the whole mechanism | unused, must exist |
| `PlannedFault.ID` | MCTS path identity | trigger provenance link | ddmin element identity |
| `Realized.Faults.ResolvedTargets` | n/a | n/a | **required today** for `role:leader` |
| `Realized.PhaseTimings` | n/a | n/a | **required today** for `oracle_input.phases` |
| `Replay.Mode` | pick best-reproducing form | pick best-reproducing form | always `realized` |
| `Lineage.MCTSPath` | tree reconstruction | n/a | unused, must exist |
| `SUT.*` | tree cache key across commits | n/a | **required today** for `bisect`/`regress` |

---

## Second conflict: A.5 early termination vs I5

### The quotes

> A.5: "**Early termination**: If an oracle violation is detected during `DRIVE` or `PERTURB`, **terminate the rollout immediately** and backpropagate the full violation reward."
> §4.1: "5. `HEAL`: Withdraw all injected faults; assert zero residual faults remain." … "7. `ASSERT`: Evaluate invariant and convergence oracles against history and telemetry."
> §4.3: "*Critical Guarantee*: Every fault must implement a safe `withdraw()` mechanism. `HEAL` must verify that no residual network rules or stopped processes persist."
> I5: "Checking consistency during an active network partition generates false positives. Every oracle declares the specific lifecycle phases in which it is valid."

### The most dangerous line in Addendum A is not the I5 issue

"Terminate the rollout immediately" **skips HEAL**. Skipping HEAL leaves live `iptables` rules and `SIGSTOP`ped containers in the compose environment. Every subsequent rollout in the MCTS tree then runs against a silently poisoned topology, and since violations are precisely what triggers early termination, *the poisoning is triggered by success*. The MCTS tree's value estimates after the first violation are garbage, and the Saboteur will attribute the resulting cascade of crash violations to whatever fault it happened to try next. This is a straight contradiction of §4.3's Critical Guarantee and is **BLOCKING**.

Fix: "terminate" must mean "stop `DRIVE` and skip the remaining PERTURB schedule," never "skip HEAL/QUIESCE/TEARDOWN." HEAL is mandatory and unconditional.

### Which oracle classes can legitimately terminate early

The criterion is **prefix-closure**: once the property is violated on a history prefix, no continuation can repair it.

| Class | Prefix-closed? | Early-terminable? | Reasoning |
|---|---|---|---|
| `crash` | Yes | **Yes** | A process exited or panicked outside a planned fault window. Irreversible fact, observable online. Must exclude planned `proc.kill` windows: checkable online. |
| `safety` | Yes by definition | **Yes** | "A bad thing happened" is monotone. |
| `consistency` | **Yes, in theory** | **No, in v1** | See below. |
| `resource` | No | No | `resource_return_to_baseline` is defined *post-QUIESCE*; `no_unbounded_queue` is defined *across QUIESCE*. Both are undecidable before those phases exist. A true OOM manifests as a crash and is caught by the crash oracle. |
| `liveness` / `convergence` | No | No | `availability_after_heal` and `no_stuck_op` are defined *relative to HEAL*. Terminating before HEAL makes them undecidable, not violated. |
| `differential` / `metamorphic` | Varies | No | Require a completed comparison run. |

**Consistency deserves the precise answer, not the hand-wave.** Linearizability *is* a safety property and *is* prefix-closed: a history prefix admitting no linearization can never be repaired by extension. So I5's stated rationale ("checking consistency during an active network partition generates false positives") is **not** a statement about linearizability; it is a statement about *bad checkers*. CRUCIBLE §M names the real mechanism:

> "`ok` = definitely happened. `fail` = definitely did NOT happen. `info` = indeterminate. **Drivers that cannot distinguish MUST emit `info`.**"

A checker that treats a mid-partition timeout as `fail` produces false positives. A checker that emits `info` does not. But a prefix taken mid-`DRIVE` is dominated by *open* operations (invoked, not yet returned), which must be modeled as indeterminate. Porcupine/Elle over a prefix with many indeterminate ops is (a) combinatorially far more expensive than over a closed history and (b) overwhelmingly likely to return `INCONCLUSIVE` rather than `VIOLATED`. Running it repeatedly during `DRIVE` at every sample point is not affordable at rollout budgets of 25–60 s.

**Ruling: consistency oracles keep `valid_phases: ["ASSERT"]` in v1, exactly as §4.5's own example declares. Early termination is restricted to `crash` and `safety` oracles whose declared `valid_phases` include the currently executing phase. I5 is then not violated at all: it is *enforced*, because the check becomes "is the current phase in this oracle's `valid_phases`?"**

### A frozen-schema trap that will otherwise be miscoded today

§4.6 verdict shows `"oracle": "linearizable.kv", "class": "consistency", "phase": "DRIVE"`: a consistency violation labelled `DRIVE`. §4.5 oracle_output shows the *same* oracle with `"valid_phases": ["ASSERT"]`. These are only consistent under one reading:

- `violation.phase` = **the lifecycle phase in which the witness event occurred** (the stale read happened at t=14320 ms, during DRIVE).
- `oracle.valid_phases` = **the phases in which evaluating the oracle is legal** (ASSERT only).

They are different fields with different meanings. A naive implementer will conflate them and either (i) reject the frozen verdict example as invalid, or (ii) conclude consistency oracles may run in DRIVE. This must be documented in the Phase 0 schema comments.

### Does early termination bias the utility function? Yes, but not through the mechanism suspected

**The γ duration term does not invert the class ranking.** With A.6's constants:

- Crash, high severity, found at t=12 s in DRIVE, 2 faults: `80×1.5 − 3×2 − 0.1×12 = 120 − 6 − 1.2 = **112.8**`
- Consistency, high severity, found in ASSERT after a full 55 s world, 2 faults: `100×1.5 − 3×2 − 0.1×55 = 150 − 6 − 5.5 = **138.5**`

The duration penalty contributes at most ~5.5 points over a 60 s world; the consistency-vs-crash class gap is 30 points. **The naive claim that the duration penalty biases toward crashes is false at the stated constants.** State it that way and move on.

**The real bias is evidence truncation, and it is severe:**

1. **Masking.** A world that early-terminates on a crash at t=12 s never reaches ASSERT, so any consistency violation that same world would have exhibited is *never discovered*. Crash violations structurally hide consistency violations: the exact inversion the tool exists to prevent.
2. **Downward value bias on the best subtree.** That node is credited 112.8 when its true completed utility might be 112.8 + 150 = 262.8. The bias is systematically concentrated on the subtree that reaches the deepest failure states, i.e. the most valuable one.
3. **Truncated novelty reward.** β novelty is computed from coverage; terminating before HEAL/QUIESCE/ASSERT forfeits all log templates and state tuples those phases would have produced. The most interesting branches get the smallest novelty credit.
4. **Rollout-cost asymmetry.** Crash-terminating branches are cheap, so under a wall-clock budget they absorb more rollouts. UCT's `sqrt(ln(N)/visits)` term partially self-corrects this by shrinking the exploration bonus on heavily-visited nodes, but the emitted corpus and regression set remain crash-skewed.

**Recommended fixes (all normative, all recorded in `DECISIONS.md` per A.6):**

- Early termination applies only to `crash`/`safety` oracles with the current phase in `valid_phases`.
- Termination stops `DRIVE`/`PERTURB` only. **HEAL, QUIESCE, ASSERT and TEARDOWN always run.** ASSERT is cheap relative to DRIVE; run the full oracle set over the truncated history so consistency violations are still found and utility sums *all* violations found, per A.6's loop `for _, v := range result.Violations`.
- Change γ_duration to apply to the **scheduled** world duration, not the realized one. This removes the duration advantage of early termination entirely, eliminating the arithmetic question rather than arguing about it.
- Set `Realized.TerminatedEarly` so shrink and the verdict can flag truncated evidence.

---

## Third conflict: what is Phase 4's actual deliverable set?

### The contradiction is explicit and internal to Addendum A

> Preamble: "This addendum **supersedes** the coverage-guided mutation loop … Where the base spec describes stochastic corpus mutation, this addendum **replaces it** with a structured adversarial search."
> A.5 Budget Allocation: "If no REINFORCE signal is detected during probing, **fall back to the base spec's stochastic corpus mutation** for the remaining budget. The Saboteur is an accelerant, **not a replacement** for baseline coverage."

"Replaces it" and "fall back to it" cannot both hold; and the last sentence explicitly retracts the first. **Resolution: "supersedes" means "demotes from default engine to fallback engine." The mutation loop is a REQUIRED Phase 4 deliverable, fully implemented, not superseded.** CRUCIBLE §G (the explicit search-loop pseudocode, energy rules, rare-event bias, overlap-biased mutation) is the normative specification of that engine and stands unchanged.

### `strategy: hybrid` is defined nowhere

A.10 lists `saboteur | random | hybrid` and defines the mechanics of `saboteur` only. `hybrid` appears exactly once in all three documents, as an enum value. This is undefined config surface. Recommended definition:

- **`random`**: CRUCIBLE §G loop only. No probe sweep, no MCTS, no ladder.
- **`saboteur`** (default when search is enabled): Tier 1 probe sweep for `probe_budget_pct` of budget → MCTS trees rooted at top-ranked REINFORCE signals for the remainder; if zero REINFORCE signals, auto-fall-back to `random` for the remaining budget (A.5). Note this makes `saboteur` *already* hybrid in the degenerate case.
- **`hybrid`**: after the probe sweep, split the escalation budget between MCTS and the mutation loop unconditionally (new key `search.hybrid_split_pct`, default 30 = percent of escalation budget given to mutation), both engines sharing one corpus and one coverage map. Rationale: MCTS is deep-and-narrow (exploits one signal to exhaustion), mutation is broad-and-shallow (maintains global coverage per I4). For an 8 h soak they are complementary; for a 30 m search they compete.

### Required Phase 4 deliverable set (union, authoritative)

1. Coverage signals, in CRUCIBLE §F priority order: **log-template first**, then state-abstraction; branch coverage deferred (needs build cooperation, conflicts with CRUCIBLE §L "must work on unmodified systems").
2. Corpus pool, energy assignment, culling, rare-event bias (<1% templates), overlap-biased mutation: CRUCIBLE §G. Required both as Saboteur input signals *and* as the fallback engine.
3. Mutation operators: add/remove fault, shift window, escalate magnitude, retarget node/quorum, force fault overlap.
4. Saboteur: `probe.go`, `observe.go`, `escalate.go`, `mcts.go`, `uct.go`, `ladder.go` under `internal/search/saboteur/`.
5. A strategy selector implementing all three enum values, plus `hybrid_split_pct`.
6. `thesis search --budget 30m`.

**DoD** = A.9's five items (which replace the base DoD and are strictly stricter: 10 min vs 30 min, ≤4 faults, median TTFV ≤50% of uniform random) **plus one addition**: `thesis search --strategy random` must independently satisfy the base spec's Phase 4 DoD (finds the bug within 30 min from an empty corpus). Without that, the fallback path A.5 depends on ships untested, and it is the path taken whenever the probe sweep produces no REINFORCE signal, which on a *fixed* system is the common case.

---

## Fourth: is the single-agent MDP framing correct?

**Yes, decisively, and the instruction not to implement two-player solvers is correct. The cross-session dynamic does not change this, but it does impose one concrete implementation requirement that is otherwise easy to miss.**

**Within a session the framing is exactly right.** The SUT has no objective function, no model of the Saboteur, and no policy that adapts to the Saboteur's actions. Its nondeterminism (thread scheduling, GC pauses, network jitter) is exogenous randomness with a fixed, unknown distribution. That is the textbook definition of a single agent acting in a stochastic MDP. CFR and Nash equilibrium search require a second player with preferences over outcomes and a strategy space to optimize over; neither exists. Implementing them would be strictly wasted work: they would compute a best response to an opponent that does not respond.

**Across sessions, "adversarial co-evolution" is the wrong model for three reasons:**

1. **The patcher is cooperative by construction, not adversarial.** In the intended loop (CRUCIBLE §J) the coding agent and the Saboteur share the objective "no bug survives." That is a common-payoff game, and common-payoff games have no strategic content requiring equilibrium computation: the solution is just joint optimization.

2. **Where the patcher IS adversarial, the correct response is a commitment device, not a game solver.** The genuinely adversarial move (weakening an oracle, narrowing `allow`, shortening a profile budget, deleting a regression to make the gate pass) is precisely the I6 / CRUCIBLE §E threat model. The answer is the cryptographic lock file, which *removes those moves from the action space entirely*: "Mismatch returns exit 4 and REFUSES TO RUN." You do not solve an adversarial game against a player whose adversarial moves you have made illegal. This is architecturally settled and correct.

3. **The SUT is fixed within a session and changes between them.** This is not co-evolution, it is **non-stationarity across episodes**: a much weaker and better-understood condition.

**The one real implementation consequence, and it is a Phase 0 concern.** MCTS value estimates, REINFORCE classifications, and corpus energies are all *only valid for one SUT build*. A.1 point 3 says the Saboteur "does not replay old schedules. It plays a new game session," but never says what to *keep*. Concretely:

- **Discard on build change**: MCTS trees, node visit counts, REINFORCE/DAMPEN classifications, probe rankings. Their values were estimated against code that no longer exists.
- **Decay, do not reset**: corpus energy. Worlds that were interesting on the parent commit are a good prior, not a guarantee.
- **Keep unconditionally**: the regression corpus (CRUCIBLE §D: "runs on every gate invocation forever") and the coverage baseline (required to compute `coverage_delta_vs_baseline`, which is anti-gaming mechanism §E.3).

This requires the SUT build identity to be a first-class, serialized key, which is `SUT` in the struct above, and which **I2's five-tuple omits**. That omission independently breaks `thesis bisect WORLD --good SHA --bad SHA` (CRUCIBLE §H) and makes `reproduced: "3/3"` unfalsifiable. **Classification: the framing is ACCEPT-AND-DOCUMENT; the derived `SUT` field is a Phase 0 delta.**

---

## Fifth: the `search:` key: verified, and the real collision is not the one suspected

### There is no YAML parse collision

> §4.2: `profiles: { soak: { budget: 8h, worlds: -1, driver_profile: soak, **search: true** } }`
> A.10: top-level `**search:** { strategy: saboteur, exploration_constant: 1.41, … }`

These are distinct YAML paths: `profiles.soak.search` (scalar bool) vs `$.search` (mapping). No parser sees a conflict, and no Go struct is forced to unify them provided the schema author does not naively share a `Search` field name across `Profile` and `Config`. **The suspected collision is refuted at the syntactic level.**

### But there is a real semantic collision, and it is worse than a naming clash

The two keys imply **contradictory defaults for whether the Saboteur runs at all**:

- §4.2 puts `search: true` on `soak` **only**. `smoke` and `gate` omit it: implying search is **off** for them.
- A.10 says `strategy: saboteur` is the **default**, which reads as "the Saboteur is on everywhere unless overridden."

If A.10's default wins, then `thesis gate --profile gate --json` (CRUCIBLE §J: 10 min, pre-commit, runs on every agent iteration) executes an adaptive MCTS search. That is wrong on three counts:

1. **The gate must be a fixed, reproducible, lock-verified check**, not a stochastic search. An agent loop whose pass/fail signal varies run-to-run because the search took a different branch is unusable.
2. **The probe sweep does not fit.** A.3 mandates one micro-probe per fault kind in `perturber.allow` per target: with §4.2's 7 allowed kinds across 3 `kv` nodes that is ~21 probe worlds at 25–60 s each = 9–21 minutes. The probe budget for `gate` is 20% of 10 min = **2 minutes**. The sweep cannot even complete, so REINFORCE is never detected, so the Saboteur falls back to mutation: 2 minutes wasted on a truncated sweep every gate invocation.
3. A.9's DoD assumes a 10-minute cold-start budget *for escalation alone*.

### Resolution

Rename nothing: both keys appear in normative text and Core Rule 2 freezes them. Define them as orthogonal:

- **`profiles.<p>.search` (bool, default `false`)** is the **enable switch**: does this profile run the search engine at all?
- **top-level `search:`** is the **configuration**: how the search engine behaves *when enabled*. `search.strategy`'s `saboteur` default applies only within an enabled profile.
- Therefore: `smoke` and `gate` never run the Saboteur. `soak` does. `thesis search` implies enabled regardless of profile.
- If a profile sets `search: true` and the top-level block is absent, all A.10 defaults apply.
- Go: `Profile.Search *bool` (pointer, so "absent" is distinguishable from "false"; this matters for lock hashing) and `Config.Search *SearchConfig`.

### Derived requirement the addendum misses entirely: `search:` must be lock-covered

> CRUCIBLE §E.2: "**Fault-space floor.** Removing a kind from `allow`, narrowing `max_faults_per_world`, or shortening a profile budget are ALL lock changes."

Every A.10 knob is a fault-space narrowing under that rule: setting `strategy: random`, dropping `probe_budget_pct`, lowering `max_mcts_depth`, or flipping `profiles.soak.search` to `false` all weaken the search without touching an oracle. An agent trying to make the gate green would find `max_mcts_depth: 1` a far easier edit than touching `.prothesis/oracles`. **The top-level `search:` block and every `profiles.*.search` flag MUST be inputs to the `.prothesis/lock` manifest.** `internal/lock` must reserve this now; it is trivial to add today and a lock-format break to add later.

---

## Additional conflicts found in the sweep

**A.6 has no source for `severity`, and `severity` dominates the entire class hierarchy.**
`prothesis.oracle_output/v1` (§4.5) has fields `schema, oracle, class, valid_phases, status, witness, explanation`: **no `severity`**. The verdict (§4.6) has `"severity": "high"`, and A.6's `severityMultiplier` multiplies the whole violation reward by 0.5 / 1.0 / 1.5. That is a **3× swing**: larger than the consistency-vs-crash gap (1.25×), consistency-vs-safety (1.67×), or consistency-vs-resource (2.5×). Only consistency-vs-liveness (5×) is larger. **An undefined field controls the Saboteur's search more than the normative class table does.** Resolution: add `severity` as an *optional* field to `OracleOutput` now (additive, backward-compatible, does not change any existing semantics), with the engine falling back to a per-class default table when absent, and A.6's `default: return 1.0` covering unknown strings. This is a one-line Phase 0 struct change that avoids editing a frozen schema in Phase 3.

**A.6's utility switch silently scores two oracle classes at zero.** CRUCIBLE §B defines eight classes: `crash, consistency, liveness, convergence, resource, safety, differential, metamorphic`. A.6's `switch v.Class` covers six. `differential` and `metamorphic` violations contribute **0.0** to utility: the Saboteur is blind to them and will never search toward them. Add `default: u += 60.0 * severityMultiplier(v.Severity)` (same as `safety`) and record in `DECISIONS.md` as A.6 requires.

**A.3's probe sweep cannot classify anything, because it drops the baseline.** CRUCIBLE §G's seed corpus is "one per allowed fault kind, **plus one no-fault control**." A.3 says it "replaces the base spec's 'seed worlds (one per allowed fault kind)'", and quietly drops the control. But every REINFORCE criterion in A.3 is defined *relative to a baseline*: "Metrics returned to **baseline** within 2× the fault window", "Retry count exceeded **2× baseline**". A.3 never says how baseline is established. Fix: the probe sweep must begin with N=3 no-fault control worlds establishing per-metric mean and standard deviation. Related: A.10's `reinforce_threshold: 0.0` classifies *any* positive regression slope as REINFORCE; against noisy real telemetry that is roughly a 50% false-positive rate. Default it to a multiple of the control-run standard deviation instead.

**Three contradictory telemetry sampling rates, and the detector cannot fire inside a probe window.** A.3 says telemetry trajectory is "sampled every 200ms during DRIVE"; A.4 says container metrics "Sampled every 500ms"; A.10 sets `sample_interval_ms: 500`. Worse, A.4's `ClassifySignal` needs `windowSize=5` samples for the trend and `windowSize=3` trends for the acceleration: a minimum of 7 samples ≈ **3.5 s at 500 ms**. A.3's probe windows are **500–2000 ms**. *No probe can ever produce `REINFORCE_ACCELERATING` at the stated constants*, which is A.9 DoD item 1 verbatim. Fix: two independent samplers (`observe.metrics_interval_ms: 500` for expensive container metrics, `observe.trajectory_interval_ms: 200` for cheap driver/health counters), classify from the 200 ms stream, and extend probe windows to ≥3 s.

**A.7 requires a fault primitive the frozen grammar cannot express.** Rung 3 is "`net.partition` (**asymmetric preferred**)". §4.3's TARGET grammar offers `edges (n1<->n2)`: bidirectional. There is no directed-edge syntax, so a one-way drop (the highest-value partition variant for finding split-brain and lease bugs) is unrepresentable. Fix: extend TARGET with `n1->n2` (purely additive). Phase 0 impact is nil because `PlannedFault.Target` is an opaque string, but the grammar should be *documented* now, since adding a fault kind later is a lock bump under CRUCIBLE §E.2.

**A.8 overclaims determinism.** "All Saboteur decisions (fault selection, tree expansion) must be deterministic given the same PRNG seed" is false even under Reading 2: tree expansion depends on the measured utilities of prior *real* rollouts, which are not seed-determined. Only the sampling, tie-breaking, and expansion order can be. This directly contradicts CRUCIBLE PART 5's "**never claim determinism it does not have**." Restate as: *all Saboteur randomness is drawn from a dedicated RECORDER stream; the search trajectory is not reproducible; reproducibility is delivered by the realized world files the search emits, not by re-running the search.* Phase 0 consequence: the recorder needs **named, independent** PRNG streams (`driver`, `perturber`, `topology`, `search`) so that consuming search randomness does not shift driver randomness. Reserve `search` now even though nothing draws from it until Phase 4.

**MCTS at these budgets is prior-dominated, not UCT-dominated.** A.9 requires discovery in 10 minutes at 25–60 s per rollout = **10–24 rollouts total**. The action space is the full grammar filtered by budget: ~7 kinds × ~6 target forms ≈ 42 children per node, at depth 4. With 24 rollouts, UCT never finishes expanding the root's children once. The escalation ladder (A.7) is doing essentially all the work; UCT is a tie-breaker. Implement honestly: **progressive widening** (expand children strictly in ladder order and admit a new child only after existing ones have ≥k visits) and describe the algorithm as "ladder-prior best-first search with UCT tie-breaking." Deferrable to Phase 4, but it determines whether A.9 DoD item 2 is achievable at all.

**A documentation error that will propagate into `thesis init` today.** CRUCIBLE §B states the on-disk sample config "omits `no_stuck_op` from `oracles.builtin`." That is false: `PRO_THESIS_FABLE_PROMPT.md` line 168 lists it. §B also says "requires SEVEN" and then lists six. The correct answer is six built-ins, all six already present in §4.2, all six required by Phase 1. Since `thesis init` scaffolds `prothesis.yaml` in Phase 0, scaffold all six and do not "fix" a non-existent omission.

---

## Bottom line on the format freeze

The world file must be a **planned/realized pair keyed to an SUT identity**, with an `at`|`when` trigger union and stable per-fault IDs. Every one of those fields is required by Phase 2 or Phase 5 *on its own merits*, before the Saboteur exists. Addendum A adds only `Lineage.MCTSPath`, `Realized.TerminatedEarly`, and the `when` predicate: three small additions to a structure Phase 0 needs anyway. Freeze that structure today and Phase 4 changes nothing.
