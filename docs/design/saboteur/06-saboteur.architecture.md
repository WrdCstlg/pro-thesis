# 06-saboteur.architecture

## Summary

The Saboteur's stated economics do not close: measured on this host, a probe world costs ~19.4 s (not A.3's 5–15 s; the 5 s QUIESCE forced by the fixture's 5-second lease alone makes it impossible), so the 22-world Tier 1 sweep needs 427 s serial, which is 356 % of its own 120 s probe budget and 71 % of the entire 10-minute DoD. That leaves ~16 serial rollouts for MCTS against a root branching factor of 249, at which point UCT is provably identical to ladder-ordered enumeration: no node is ever visited twice, so backpropagation never influences a single decision, and separating a violation arm from a dead one at Tier B's p≈0.2 would need ~15 samples per arm. Parallelism is the fix and it is sufficient: I measured 8 concurrent compose projects at 3.8× speedup with only +3–5 % timing distortion at full CPU saturation, so W=4 makes the sweep fit in 116 s and buys 62 rollouts, putting projected time-to-first-violation at ~135 s against the 10-minute DoD. Four spec defects need resolving now: A.5's UCT formula divides an average by visits again (inverting its own intent), A.6's class switch has no default so `differential` and `metamorphic` violations score 0.0 and are invisible to the search, A.6's parsimony claim is false as written (one novel log template outweighs 3.33 faults, so a 12-fault world with 5 novel templates scores 152 vs the minimal world's 141), and A.3's role-change criterion is a constant on any correct Raft rather than a bug signal. Phase 0 must land three things today or Phase 4 is a rewrite: a splittable path-addressed PRNG (a single shared `*rand.Rand` makes parallel rollouts irreproducible), a multi-project compose backend with zero published host ports, and settable `PhaseTimings` plus the `search:` config block in the schema before `.prothesis/lock` first exists.

## Findings (18)

### F1 [RESOLVABLE-WITH-DECISION] **(AFFECTS PHASE 0)** A.3's 5-15s probe cost is physically unachievable; the Tier 1 sweep is 356% of its own budget

Measured on this host: 3-node compose up --wait = 1.45-2.04s, down -v = 1.63-1.68s (cycle 3.13-3.67s). Adding Raft election (~2s), SEED (1.5s), HEAL residual-verify (1.5s), ASSERT (0.7s), and a QUIESCE that MUST exceed the fixture's 5-second leader lease (6s), the zero-DRIVE floor for a probe world is 15.4s and a realistic probe is 19.4s. The 7 allow-listed kinds x 3 kv nodes = 21 probes + 1 control = 22 worlds = 427s serial. A.5 allots the probe phase the first 20% of budget; against A.9 DoD #2's 600s that is 120s. 427/120 = 356%, and 427/600 = 71% of the entire run. Even at A.3's own optimistic 10s midpoint it is 183% of the probe budget. Honouring the 120s cap serially yields 6 of 21 probes, so leader proc.pause is covered with probability 6/21 = 29% and DoD #1 fails 71% of the time.

**Recommendation:** Two changes. (1) Make the sweep an anytime, ladder-ordered, budget-truncated sweep rather than an enumeration, so proc.pause (Rung 2) is probed in the first two batches by construction and DoD #1 is deterministic rather than 29% likely. (2) Run probes in parallel at W=4: 22 worlds / 4 x 21s = 116s, which fits the 120s budget. Correct A.3's stated cost to 15-25s in the spec and record in DECISIONS.md.

### F2 [ACCEPT-AND-DOCUMENT] **(AFFECTS PHASE 0)** At the stated budget, UCT is statistically indistinguishable from ladder-ordered random

Serial escalation budget is 480s at 28.7s/rollout = 16 rollouts (8 at A.5's 60s worst case). Root branching factor after ladder gating and type-compatible target enumeration is 249 raw actions for the KV fixture. Three independent failures follow. (a) UCT's exploration term is +infinity at visits==0, so with 16 rollouts and b>=16 every rollout descends into a never-visited child; no node is ever visited twice, U_avg is never an average over more than one sample, and backpropagation provably never influences a selection. The algorithm is exactly first-play-urgency ordering, i.e. the escalation ladder. (b) Tier B gives p ~ 0.2-0.6; a violation arm is Bernoulli over U=+141 vs U=-9, giving mu=21.0, sigma=60.0, CV=2.86. Separating it from a dead arm at 95% confidence needs n ~ (1.96*60/30)^2 ~ 15 samples per arm; with b=20 that is 300 rollouts = 2.4 hours serial. (c) Kocsis-Szepesvari's O(log n) regret bound holds only for n >> K; here n < K, so the bound is vacuous.

**Recommendation:** State plainly in DECISIONS.md that at gate budgets the ladder prior and early termination do the work and the tree is a thin refinement; do not claim MCTS efficacy the budget cannot support. Then buy the rollouts back with parallelism (W=4 gives 62 rollouts; progressive widening caps root arms at ceil(2*sqrt(62))=16, so top arms get 5-8 visits and the bandit starts to carry signal at the root). Invest engineering in the action ordering, not in tree depth; max_mcts_depth=4 is fine because width, not depth, is binding.

### F3 [BLOCKING] A.5's UCT formula divides an average by visits a second time, inverting its own intent

A.5 specifies UCT(node) = (U_avg / visits) + C * sqrt(ln(parent.visits)/visits). If U_avg is already the mean, dividing by visits again decays the exploitation term as 1/n. Concretely a node visited 3 times with average utility 141 scores 141/3 = 47, while a node visited once with average utility 100 scores 100. The formula therefore prefers under-visited nodes twice over, double-counting the exploration preference and actively penalising nodes that have accumulated evidence of being good.

**Recommendation:** The stored field must be U_total (a sum) and the formula U_total/visits + C*sqrt(ln N / n). Implement Node.UTotal as a sum, never an average. Log the correction in OPEN_QUESTIONS.md since A.5 is normative text.

### F4 [RESOLVABLE-WITH-DECISION] C = 1.41 is the wrong exploration constant for an unnormalised reward range of ~200

sqrt(2) is the correct UCT constant only when rewards lie in [0,1]. Utilities here span roughly [-50, +150]. At n=1, N=3 the exploration bonus is 1.41*sqrt(ln 3) ~ 1.48, against reward differences of order 100. The exploration term is therefore numerically irrelevant and UCT degenerates to pure greedy exploitation of the first sampled value - which, given the single-sample-per-arm regime, means greedy exploitation of pure noise.

**Recommendation:** Affine-normalise node utility to [0,1] with a running (min,max) over observed tree utilities before applying C - the standard MCTS fix for unbounded rewards - after which C=1.41 becomes meaningful. Alternatively scale C_eff = C * (U_max - U_min). Prefer Thompson sampling as the default policy, which sidesteps normalisation entirely.

### F5 [RESOLVABLE-WITH-DECISION] **(AFFECTS PHASE 0)** A.6's parsimony claim is false as written: one novel log template outweighs 3.33 faults

A.6's two worked examples are arithmetically CORRECT (150-6-3=141 and 150-36-12=102). But the conclusion they support - 'The Saboteur will always prefer the minimal path' - is false using the spec's own weights. The 12-fault/120s world, if it also finds 5 novel log templates, scores 102 + 50 = 152 > 141, so the Saboteur prefers the 12-fault world. The exchange rates are beta/gamma_fault = 10/3 (one novel template outweighs 3.33 faults) and beta/gamma_dur = 10/0.1 (one novel template outweighs 100 seconds). Worse, novelty is unbounded and structurally correlated with fault count, because a 24-fault world touches more code and emits more novel templates essentially by construction. At cold start - exactly DoD #2's scenario - the corpus is empty, novelty dominates, and parsimony is irrelevant. This directly threatens DoD #3's <= 4 faults requirement.

**Recommendation:** Saturate the novelty term: u += 10.0 * min(NewTemplates, noveltyCap) with noveltyCap default 5, exposed as search.utility.novelty_cap. Keep Utility() verbatim per A.6 for the verdict and for ranking final candidate repros, and add SearchUtility() for MCTS backprop. Record both in DECISIONS.md, which A.6 explicitly permits.

### F6 [BLOCKING] A.6's class switch has no default, so differential and metamorphic violations score 0.0 and are invisible to the search

CRUCIBLE section B defines eight normative oracle classes: crash, consistency, liveness, convergence, resource, safety, differential, metamorphic. A.6 scores only six. A.2's box scores only four (it omits safety=60 and convergence=20 entirely). Because A.6's switch has no default case, a differential or metamorphic violation contributes exactly 0.0 utility. This is exploitable given I3 ('oracles are the product'): external oracles are pluggable processes that declare their own class string per section 4.5, so a third-party oracle declaring class 'correctness' contributes 0.0 and is silently ignored by the search. A user's most valuable custom oracle would be invisible to the very engine meant to hunt for it.

**Recommendation:** Add 'default: u += 20.0' (matching the lowest defined tier) plus a one-time warning log naming the unrecognised class. Treat A.6 as governing over A.2's incomplete box diagram and note in DECISIONS.md that A.2 omits safety and convergence.

### F7 [RESOLVABLE-WITH-DECISION] **(AFFECTS PHASE 0)** 'Duration' in the utility function is ambiguous between A.2 and A.6, and the two readings differ by more than 2x

A.2 writes -gamma * (FaultCount + Duration) with gamma = 0.1 per second duration, which reads as total fault duration. A.6 uses result.DurationSeconds, which reads as world wall-clock duration. For the canonical repro (proc.pause@8200..15100 = 6.9s plus net.partition@8400..14900 = 6.5s, totalling 13.4s of fault inside a ~30s world) the penalty is 1.34 under A.2's reading versus 3.00 under A.6's. Every utility comparison in the search depends on which is meant.

**Recommendation:** Adopt world wall-clock duration. It matches A.6's field name and creates the incentive the budget arithmetic needs - preferring fast repros - which is exactly what the economics in finding 1 demand. Record in DECISIONS.md.

### F8 [RESOLVABLE-WITH-DECISION] The parsimony penalty is deterministic while the violation reward is stochastic, biasing the search shallow against Rung 4

The -3/fault and -0.1/s penalties are applied with certainty; the violation reward fires only with Tier B probability p. With one sample per arm, a depth-1 node that missed its violation scores -6 while a depth-4 node that missed scores -15. The search is therefore systematically pushed shallow - directly against A.7 Rung 4 COMPOUND, which the ladder itself identifies as where real bugs hide, and which the canonical Raft lease bug requires (proc.pause AND net.partition concurrently, i.e. depth 2).

**Recommendation:** Split the function: Utility() stays A.6-verbatim for reporting and repro ranking; SearchUtility() used in backprop applies the parsimony term depth-normalised, or at ~10% of its reporting weight, so it discriminates among siblings at equal depth without penalising depth itself. Record in DECISIONS.md.

### F9 [ACCEPT-AND-DOCUMENT] **(AFFECTS PHASE 0)** A.8's literal determinism requirement is unsatisfiable under Tier B

A.8 requires that all Saboteur decisions including tree expansion be deterministic given the same PRNG seed. But tree shape is a function of backpropagated utilities, utilities are a function of rollout outcomes, and rollout outcomes are Tier B nondeterministic - the spec itself states replay is '3/3' or '1/5', never certain. Two runs with an identical seed diverge the instant one rollout's violation fires and the other's does not, after which every subsequent selection differs. Full session determinism is achievable only under Tier A (v4 deterministic simulation).

**Recommendation:** Amend A.8 to the three properties that ARE achievable and log the conflict in OPEN_QUESTIONS.md. D1 decision-function determinism: Decide(treeState, observations, streams) is pure and total, so identical tree state plus identical observations plus identical root seed yields an identical action regardless of worker count. D2 artifact determinism: every emitted world is a self-contained (seed, topology_variant, driver_profile, fault_schedule, phase_timings) tuple replaying independently of the search - this is what I2 actually demands, since I2 requires the violation to be reproducible, not the search. D3-prime journal replay: record (nodePath, actionID, streamCounters, observedReward, violated, completionOrder) to saboteur_journal.jsonl so 'thesis search --replay-journal' reconstructs a bit-identical tree with no execution, which delivers the debuggability A.8 was reaching for and is CI-testable in milliseconds.

### F10 [BLOCKING] **(AFFECTS PHASE 0)** A single shared PRNG makes parallel rollouts irreproducible; streams must be path-addressed

With W parallel workers drawing from one sequential *rand.Rand, the draw ORDER depends on goroutine completion order, so nothing downstream is reproducible and even D1 decision-function determinism is lost. Separately, backprop completion order under W>1 is nondeterministic even when selection is deterministic. If Phase 0 ships recorder with a single rand.New(rand.NewSource(seed)) handed around by value, this becomes a rewrite touching every package in Phase 4.

**Recommendation:** Ship a splittable, domain- and path-addressed stream family in Phase 0: Streams.Derive(domain string, path ...string) *rand.Rand seeded by SHA-256(root || domain || path) into math/rand/v2.NewChaCha8([32]byte), which is available in Go 1.27.1 and is natively 32-byte seeded. Nine streams are needed: topology, driver, probe.plan, probe.jitter, mcts.widen, mcts.select, mcts.jitter, mcts.rollout, harness.ports - the mcts.* ones addressed by (nodePath, visitIdx). Additionally provide --deterministic-backprop (default for thesis gate) which buffers a batch of W results and applies them in submission order, so D1 holds at batch granularity even with W>1.

### F11 [BLOCKING] A.3's role-change REINFORCE criterion is a constant on any correct Raft, not a bug signal

A.3 criterion (c) fires when 'a role change occurred within 1 second of fault injection'. In a CORRECT Raft, pausing the leader MUST trigger an election - that is the algorithm working. So the criterion fires on every leader proc.pause regardless of whether a bug exists. It has zero discriminative power: it detects Raft-ness, not bugginess. Because A.3 ranks REINFORCE signals by strength, a constant term flattens the ranking. It would also fire identically after the lease bug is patched, breaking A.1's 'adaptive re-engagement' premise that the Saboteur probes patched code for novel counter-exploits.

**Recommendation:** Demote role-change from a REINFORCE class member to a quantitative gate with two measured quantities. RoleChurn: count of term changes and time-to-stable-leadership after HEAL (a correct Raft has one election converging within ~1 election timeout; a degraded one thrashes terms). LeaseOverlapMs = max(0, lease_expiry(old_leader) - election_time(new_leader)): this is > 0 exactly when the lease bug is exploitable and 0 for a correct implementation, and it is derivable from logs alone via term changes plus resume timestamp, requiring no instrumentation - which satisfies CRUCIBLE section L's 'must work on unmodified systems'. Use these as a multiplier on signal strength, never as a member of the class set.

### F12 [BLOCKING] **(AFFECTS PHASE 0)** A.3 and A.4 are two classification schemes with overlapping names and incompatible codomains; DoD #1 is unsatisfiable by A.3 alone

A.3 returns {DAMPEN, REINFORCE}; A.4's ClassifySignal returns {DAMPEN, REINFORCE_LINEAR, REINFORCE_ACCELERATING}. A.9 DoD #1 requires the probe sweep to emit REINFORCE_ACCELERATING for leader proc.pause, which A.3's classifier literally cannot produce. The inputs also differ in type: A.3 criterion (c) is a discrete boolean event while A.4 requires a numeric time series to regress - you cannot compute the second derivative of a boolean. And the sampling rates contradict: A.3 says 200ms while A.4 and A.10's observe.sample_interval_ms say 500ms; with spiral_window=5 that gives a 2.5s regression window, which is LONGER than the probe's own 500-2000ms fault window, so the trend regression physically cannot resolve the fault it is meant to measure.

**Recommendation:** Treat them as two layers of one pipeline. A.4's ClassifySignal is the primitive, kept exactly as specified, mapping one numeric metric series to one of three classes. A.3 criteria (a) queue depth, (b) retry count and (d) error rate map onto it natively as evidence predicates. Criterion (c) is demoted per the previous finding. Node classification is the max over metric series, with RoleChurn/LeaseOverlapMs as a strength multiplier. DoD #1 is then satisfied legitimately: REINFORCE_ACCELERATING for leader proc.pause arises from the driver error/anomaly-rate series, because when the paused leader resumes and serves stale reads the anomaly rate rises with positive acceleration as more clients hit the stale leader. Add observe.probe_sample_interval_ms: 200 alongside sample_interval_ms: 500, and constrain spiral_window * interval <= 0.5 * fault_window.

### F13 [RESOLVABLE-WITH-DECISION] A.5's MCTS root cannot be a live distributed-state snapshot under a compose harness

A.4 says REINFORCE_ACCELERATING 'triggers immediate escalation to Tier 2' and A.5 defines the root as 'the current observed distributed state at the moment REINFORCE was detected'. There is no fork primitive for a running 3-container topology, so a live state cannot be snapshotted and branched from. Mid-world escalation would also violate I5, which places oracle evaluation in ASSERT after HEAL and QUIESCE.

**Recommendation:** The only implementable reading under I1 (real execution only): the MCTS root is not a state snapshot but the (probe fault, target, timing) PREFIX that produced the REINFORCE signal, and every rollout re-executes that prefix from BOOT. Record in DECISIONS.md as a deliberate reinterpretation of A.5. Cost consequence is that each rollout re-pays BOOT+SEED, which I measured at ~5.5s and is already priced into the rollout model - affordable.

### F14 [RESOLVABLE-WITH-DECISION] **(AFFECTS PHASE 0)** Docker bridge-network pool is a hard ceiling of 24 concurrent worlds on this host, and leaked projects consume it permanently

Measured empirically: docker network create fails at the 25th additional network, giving 30 total user bridge networks, of which 6 are already occupied by the user's other projects (autobriefingengine_chord-net, ellment_default, newversion_ai-ide-network, stratum_stratum-net, supabase_network_hydra, plus the default bridge). Each compose world requires one network, so W <= 24 absolutely. Leaked projects from a panicked or interrupted search hold their network until reaped, so at W=8 the pool exhausts after three generations of leaks. The failure mode is not a hang: docker network create fails mid-search, surfacing as exit 2 INCONCLUSIVE.

**Recommendation:** Clamp W = min(cfg.Workers, floor(dockerCPUs/2), floor(freeNetworks/2)), which yields 4 on this machine with 12 as the absolute maximum. Implement harness.Reap() invoked at every startup: docker compose ls --filter name=prothesis- -q, then down -v on each. This is load-bearing, not hygiene.

### F15 [RESOLVABLE-WITH-DECISION] **(AFFECTS PHASE 0)** Parallel rollouts distort timing 3-5% at saturation and foreign containers inject uncontrolled noise

Measured sleep fidelity (200 x 50ms usleep, 10.000s ideal): 10096ms idle (+0.96%) versus 10305-10486ms with 8 CPU-burner containers saturating all 8 vCPU (+3.0% to +4.9%). A 300ms Raft election timeout at +5% drifts 15ms (tolerable), a 500ms probe fault window drifts 25ms, and the LeaseOverlapMs measurement needs sub-100ms resolution. Separately, the host already runs 5 foreign containers and chord-api spiked to 75% CPU unprompted during the load test, injecting uncontrolled CPU noise into rollouts. This directly threatens Tier B replay fidelity and the DoD #4 statistical comparison against uniform random.

**Recommendation:** Default W=4 (keeps the box at ~50-60% and distortion near the +1% baseline), W=6 max for search. Apply per-service cpus:/mem_limit via a generated compose override so no world starves peers. Record EnvFingerprint{workers, foreignContainers, hostLoad1m} in every WorldResult; on replay, a materially different fingerprint marks the attempt INCONCLUSIVE rather than counting toward reproduced: k/n - this is the honest handling CRUCIBLE section C demands ('never claim determinism it does not have'). Run thesis replay and thesis shrink at W=1 always, so the 3/3 confirmation gate is never polluted by contention. Have thesis doctor warn on foreign containers.

### F16 [ACCEPT-AND-DOCUMENT] Warm topology reuse is not worth it and breaks I2

Measured full compose orchestration cycle is only 3.13-3.67s of a ~29s rollout, i.e. 11%. Reusing a warm topology across rollouts therefore saves at most ~3.3s while allowing state to leak between rollouts, at which point the world tuple (seed, topology_variant, driver_profile, fault_schedule, phase_timings) no longer defines the execution and I2 is violated. The intuition that container startup dominates rollout cost is simply wrong on this hardware; the cost is in DRIVE and in the QUIESCE window that must exceed the 5-second lease.

**Recommendation:** Reject warm topology reuse. Spend the effort on parallelism (4-6x), early termination on violation during DRIVE/PERTURB (~40% saving on successful rollouts, and required rather than optional given the budget), and progressive abandonment at t=0.5*DRIVE when the spiral score is below threshold (turns a 29s worst case into ~19s expected). Truncated rollouts must set Truncated: true so they never count as a clean PASS.

### F17 [RESOLVABLE-WITH-DECISION] **(AFFECTS PHASE 0)** A.10's search config cannot express A.6's utility function, and must land before the lock file exists

search.utility exposes only violation_weight, novelty_weight, fault_penalty and duration_penalty. There is no knob for the per-class ratios (80 crash / 60 safety / 40 resource / 20 liveness), none for the novel-state weight of 5, and none for the severity multipliers (1.5/1.0/0.5). So the configuration cannot actually express the normative function it claims to parameterise. This is compounded by CRUCIBLE section E.2, which makes the allow-list and profile budgets lock-covered inputs to .prothesis/lock: if the search: block lands after the lock first exists, adding it becomes an ORACLE_DRIFT event requiring a separate human-reviewed 'thesis oracles lock --reason' commit.

**Recommendation:** Add the full search: block to pkg/schema in Phase 0 with all A.10 fields plus the extensions this design needs (workers, bandit, novelty_cap, probe_sample_interval_ms, pw_const, pw_alpha), and document explicitly which utility parameters are tunable versus frozen. Cheap now; a lock-bump ceremony later.

### F18 [ACCEPT-AND-DOCUMENT] DoD #4's comparison against uniform random is rigged unless the baseline gets the same overlap bias

A.7 Rung 4 states literally that overlapping proc.pause with net.partition 'is the canonical Raft lease bug trigger'. The ladder therefore encodes the answer to the fixture as a hardcoded prior. My projection shows the Saboteur finds the bug in ~135s at W=4 largely because of this. A.9 DoD #2 requires 'no hardcoded fault schedules' - a prior is not a schedule, so this is defensible - but DoD #4 requires the Saboteur's median time-to-first-violation to be <= 50% of uniform random's. If the random baseline is denied the same two-fault-overlap bias, the experiment measures the ladder rather than the search, and will report a large win that does not generalise to a system whose bug the ladder does not already name.

**Recommendation:** Run DoD #4 as a genuine three-way ablation: (a) uniform random over the same discretised action set, (b) ladder-ordered random with no tree, (c) full Saboteur. The (b) versus (c) delta is the only honest measure of what MCTS contributes. Expect (b) to be close to (c) at gate budgets per the earlier finding, and report that plainly. Ship all three bandit policies behind search.bandit so the ablation is a config change, not a code change.

## Phase 0 deltas (10)

- **internal/recorder/prng.go**: Ship a splittable, domain- and path-addressed PRNG stream family instead of a single *rand.Rand. API: type Streams struct{ root [32]byte }; NewStreams(seed uint64) *Streams; (s *Streams) Derive(domain string, path ...string) *rand.Rand seeded by SHA-256(root || domain || 0x00-separated path) into math/rand/v2.NewChaCha8([32]byte); (s *Streams) Root() [32]byte. Reserve the nine domains now: topology, driver, probe.plan, probe.jitter, mcts.widen, mcts.select, mcts.jitter, mcts.rollout, harness.ports.
  - _why:_ THE critical delta. With W parallel rollout workers, a single sequential PRNG's draw order depends on goroutine completion order, so nothing downstream is reproducible and even D1 decision-function determinism is lost. math/rand/v2.NewChaCha8 is natively 32-byte seeded and is present in Go 1.27.1, so Derive is just SHA-256 plus a constructor. If Phase 0 ships a shared *rand.Rand handed around by value, Phase 4 is a rewrite touching every package.
- **pkg/schema/world.go**: Make PhaseTimings a first-class, serialized, per-world SETTABLE struct: {BootTimeoutMS, SeedTimeoutMS, DriveMS, HealMS, QuiesceMS, AssertTimeoutMS}. Do not hardcode phase durations in the harness.
  - _why:_ I2 already freezes phase_timings into the world tuple, but the Saboteur must be able to shorten DRIVE for 19s probe worlds and lengthen QUIESCE past the fixture's 5-second lease. If Phase 0 bakes phase durations into the lifecycle runner, Tier 1 probing is impossible and the budget arithmetic collapses.
- **pkg/schema/fault.go**: Give Fault a stable ID, and carry BOTH the symbolic target and the resolved binding: TargetExpr string (e.g. "role:leader", "minority(kv)"), ResolvedTarget []string (e.g. ["n2"]), and ReplayBinding enum {symbolic, resolved}.
  - _why:_ proc.pause(role:leader) and proc.pause(n2) replay differently - the first re-resolves the leader at injection time, the second pins a node. I2 reproducibility requires the world file to state which. It is also required for the MCTS transposition table: role and quorum targets are portable across worlds while node-id targets are not, which is why the enumeration rule opens role targets first.
- **pkg/schema/world.go**: Add Saboteur provenance to the world tuple: Provenance enum {probe, mcts, mutation, manual, shrunk}, ParentWorldID string, SaboteurPath []ActionID, LadderRung uint8.
  - _why:_ Without provenance, a Phase 4 world file is format-incompatible with every Phase 0 world already written, and the search cannot reconstruct a tree from serialized worlds. Cheap to add now; a migration later.
- **pkg/schema/result.go**: Add EnvFingerprint{Workers int, DockerCPUs int, ForeignContainers int, HostLoad1m float64} to WorldResult, plus Truncated bool.
  - _why:_ Measured: timing distortion is +0.96% idle but +3.0-4.9% at full 8-vCPU saturation, and a foreign container (chord-api) spiked to 75% CPU unprompted on this host. Without recording the concurrency level and ambient load a world ran at, a failed replay is uninterpretable and CRUCIBLE section C's 'never claim determinism it does not have' cannot be honoured - a differing fingerprint must mark a replay INCONCLUSIVE rather than counting against reproduced: k/n. Truncated marks progressively-abandoned rollouts so they never count as a clean PASS.
- **internal/harness/compose.go**: Make the compose project name a parameter, not a constant: -p prothesis-<runID>-<worldID[:8]>. Up()/Down() take context.Context and a world ID. Down() must be 'down -v --remove-orphans', idempotent, and crash-safe. Add Reap() that enumerates 'docker compose ls --filter name=prothesis- -q' and tears each down at startup.
  - _why:_ Parallelism is the only lever that makes the Phase 4 DoD achievable, and it requires N concurrent compose projects. Measured: 8 concurrent projects work (wall 7.05s for 8 cycles, 3.8x speedup). But the bridge-network pool is a hard measured ceiling of 30 total with 6 already in use, so leaked projects exhaust it after three generations at W=8, failing as exit 2 INCONCLUSIVE mid-search. Reap() is load-bearing.
- **testdata/kvfixture/docker-compose.yaml**: Publish NO host ports. Expose ports on the world's own bridge network only; the harness reaches nodes by container IP or via docker exec. If host ports prove unavoidable, add a PortLeaser with an exclusive flock on .prothesis/ports.lock.
  - _why:_ If Phase 0 hardcodes ports: ["8080:8080"], the second concurrent world collides on bind and the entire parallel plan dies. My W=8 benchmark used per-project networks with zero published ports and ran cleanly, which is the empirical proof this design works.
- **pkg/schema/config.go**: Add the full A.10 search: block now, with defaults, plus the extensions this design needs: workers (default 4), bandit (uct | uct_tuned | thompson, default thompson), novelty_cap (default 5), probe_sample_interval_ms (default 200), pw_const (2.0), pw_alpha (0.5), deterministic_backprop (bool).
  - _why:_ CRUCIBLE section E.2 makes the allow-list and profile budgets hashed inputs to .prothesis/lock. If the search: block lands after the lock first exists, adding it is an ORACLE_DRIFT event (exit 4) requiring a separate human-reviewed 'thesis oracles lock --reason' commit. Settling the schema shape now is free; settling it in Phase 4 is a governance ceremony.
- **internal/recorder/telemetry.go**: Define the TelemetrySample type and a Sampler interface in Phase 0 with a per-world configurable interval, even if Phase 1 supplies the only implementation. Sample tuple: (t_ms, node, queue_depth, retry_count, active_connections, error_rate, role, term, rss, cpu_pct, fd_count).
  - _why:_ OBSERVE consumes this, and A.3 (200ms) versus A.4/A.10 (500ms) forces the interval to be per-world configurable rather than a constant. With spiral_window=5 a 500ms interval gives a 2.5s regression window, which is longer than a probe's own 500-2000ms fault window - so the rate must be settable or probe classification is impossible. Retrofitting a fixed-rate sampler in Phase 4 touches the whole lifecycle runner.
- **cmd/thesis/root.go and .prothesis/runs layout**: Add a global --workers N flag, and make the artifacts layout per-world: .prothesis/runs/<runID>/worlds/<worldID>/{history.jsonl,telemetry.json,logs/,world.thesis}. Never a singular .prothesis/runs/<runID>/history.jsonl.
  - _why:_ Parallel worlds writing to a run-level singular history file trample each other. The per-world directory is also what section 4.5's oracle_input contract already implies (it passes distinct history_path, telemetry_path and world_path per evaluation).

## Deferred

- Full MCTS tree serialization format (.prothesis/runs/<id>/tree.json) - needed only when a tree exists, and its shape follows from the Node type. No Phase 0 dependency beyond ActionID stability.
- Implementation of UCB1-Tuned and the D-UCB windowed average. Thompson is the recommended default and UCT is required for A.5 conformance; the third policy is an ablation nicety whose value only appears at soak (8h) budgets where n gets large.
- The custom escalation ladder loader (.prothesis/ladder.yaml, per A.10 escalation_ladder: custom). DefaultLadder as Go code is sufficient through Phase 4 DoD; only the magnitude TABLE shape needs to be right, not its file format.
- Progressive abandonment tuning (the spiral-score threshold and the 0.5*DRIVE checkpoint). The mechanism needs the ctx-cancellation seam in Executor.Execute, which Phase 1 provides; the thresholds require real telemetry to calibrate.
- The fallback to base-spec stochastic corpus mutation when no REINFORCE signal is detected (A.5 budget allocation). This is Phase 4's corpus/energy machinery, which the addendum explicitly preserves; it does not constrain Phase 0.
- Rare-event bias over sub-1% log templates (CRUCIBLE section G) as an input to ProbePrior. Enhances the ladder prior but is orthogonal to the tree design.
- thesis doctor's foreign-container and determinism-readiness scoring (CRUCIBLE section C). Valuable for interpreting the measured 3-5% timing distortion, but it is a Phase 6 developer-interface deliverable; Phase 0 only needs to record EnvFingerprint so doctor has data to read later.
- Verdict signing (CRUCIBLE section E.5, marked v3) and the MCP surface for search telemetry.

## Analysis

## 0. Measurements taken on THIS machine (not estimates)

Everything below is grounded in benchmarks I ran on the host during this analysis.

| Quantity | Measured |
|---|---|
| CPU exposed to Windows | AMD Ryzen 9 9950X3D, **8 cores / 8 logical** (not 16: SMT+half the CCD are not exposed) |
| Docker Linux VM | 8 CPUs, **30.05 GiB**, engine 29.1.3 |
| 3-node compose `up -d --wait` (1 s app boot) | **1.45 – 2.04 s** serial |
| 3-node compose `down -v` | **1.63 – 1.68 s** serial |
| Full orchestration cycle | **3.13 – 3.67 s** (mean 3.33 s) |
| Same at W=8 concurrent projects | up 2.23–2.78 s, down 1.82–3.01 s; **wall 7.05 s for 8 cycles** = 0.88 s amortized (3.8× speedup) |
| Sleep fidelity, idle (200 × 50 ms) | 10 096 ms vs 10 000 ideal = **+0.96 %** |
| Sleep fidelity, 8 vCPU fully saturated | 10 305 – 10 486 ms = **+3.0 % to +4.9 %** |
| Docker bridge-network hard ceiling | **30 total; 6 already in use → 24 free** (`network create` fails at #25) |
| Pre-existing foreign containers | 5 (`ellment-*`, `chord-api`): `chord-api` spiked to **75 % CPU** unprompted during the load test |

Two of these are load-bearing and surprising:

1. **Compose lifecycle is cheap (3.3 s), not the bottleneck.** This kills "warm topology reuse" as a serious lever: it can save at most 3.3 s of a ~29 s rollout (11 %) while destroying I2 (state leaks across rollouts, so the world tuple no longer defines the execution).
2. **The bridge-network pool is a hard ceiling of 24 concurrent worlds**, and leaked compose projects consume it permanently. At W=8 the pool exhausts after 3 generations of leaks, and the failure mode is `docker network create` failing mid-search → exit 2 INCONCLUSIVE.

---

## 1. The economics. The stated budget does not support MCTS. Here is the arithmetic.

### 1.1 Honest rollout cost model

`A.3`'s "5–15 s per probe" is not achievable. Floor argument:

| Phase | Probe world | Escalation rollout | Why it cannot shrink |
|---|---|---|---|
| BOOT (compose up + Raft election) | 4.0 s | 4.0 s | 2.0 s measured compose + ~2 s app boot & first election |
| SEED (`steady_state` probe) | 1.5 s | 1.5 s | |
| DRIVE + PERTURB | 4.0 s | 12.0 s | probe = 2 s window + 2 s tail |
| HEAL (withdraw + residual verify) | 1.5 s | 1.5 s | §4.3 requires verifying zero residual iptables/stopped procs |
| QUIESCE | **6.0 s** | **6.0 s** | **§5: the leader lease is 5 seconds.** You cannot assert convergence before the lease expires |
| ASSERT (Porcupine over history) | 0.7 s | 2.0 s | |
| TEARDOWN (`down -v`) | 1.7 s | 1.7 s | measured |
| **Total** | **19.4 s** | **28.7 s** | |

Even with `DRIVE = 0`, a probe costs **15.4 s**. So A.3's 5 s lower bound is physically unreachable and its 15 s upper bound is roughly the zero-drive floor. **A.5's 25–60 s for escalation rollouts is correct; A.3's 5–15 s for probes is wrong by ~2×.**

### 1.2 Tier 1 sweep size

`perturber.allow` has exactly **7** kinds (`net.partition`, `net.latency`, `net.loss`, `proc.kill`, `proc.pause`, `clock.skew`, `io.latency`). A.3 says "on each node individually", 3 kv nodes:

- **7 × 3 = 21 probe worlds**, + 1 no-fault control (CRUCIBLE §G) = **22 worlds**
- (with the 4th `pg` node: 29; adding `minority(kv)`/`majority(kv)` partition probes: 24)

### 1.3 The budget collision

A.9 DoD #2: cold start → bug in **10 min = 600 s**. A.5: probe budget = first **20 % = 120 s**.

| | Serial time | vs 120 s probe budget | vs 600 s total |
|---|---|---|---|
| 22 probes @ measured 19.4 s | **427 s** | **356 %** | **71 %** |
| 22 probes @ A.3's optimistic 10 s | 220 s | 183 % | 37 % |
| 22 probes @ A.3's 5 s floor | 110 s | 92 % | 18 % |

**The Tier 1 sweep cannot complete inside its own budget under serial execution.** A.3, A.5's 20/80 split, and A.9's 10-minute DoD are mutually unsatisfiable as written.

If we honour the 120 s cap serially we get **6 probes of 21**. Probability that leader `proc.pause` is among 6 chosen uniformly = 6/21 = **29 %** → DoD #1 fails 71 % of the time. *(Fix: the sweep must be ladder-ordered and budget-truncated; an anytime sweep, not an enumeration. `proc.pause` is Rung 2, so it gets probed in the first two batches by construction.)*

### 1.4 What is left for MCTS, and is UCT better than random?

Escalation budget = 480 s. At 28.7 s/rollout → **16 rollouts** (8 at A.5's 60 s worst case).

**No. At 16 rollouts UCT is provably worthless here, for three independent reasons.**

**(a) Every rollout hits an unvisited child, so backprop never influences a decision.** Root branching factor after ladder gating is ≥ 20 (I compute 249 raw below). UCT's exploration term is `+∞` at `visits == 0`, so unvisited children are always selected first. With 16 rollouts and b ≥ 16, **no node is ever visited twice**, `U_avg` is never an average over more than one sample, and the algorithm is *provably identical* to "enumerate children in first-play-urgency order": i.e. the escalation ladder order. UCT contributes exactly zero bits.

**(b) Statistical power is absent.** Tier B gives p ≈ 0.2–0.6 (the spec's own "3/3" or "1/5"). A violation-bearing arm is Bernoulli(p) over U ≈ +141 vs U ≈ −9. At p = 0.2:

```
μ = 0.2(141) + 0.8(−9) = 21.0
σ = 150·√(0.2·0.8)      = 60.0      coefficient of variation = 2.86
```

Separating that from a μ = −9 arm at 95 % confidence needs `n ≈ (1.96σ/Δ)² = (1.96·60/30)² ≈ 15 samples per arm`. With b = 20 arms that is **300 rollouts = 2.4 hours serial.** We have one sample per arm.

**(c) UCT's regret bound is vacuous here.** Kocsis–Szepesvári's `O(log n)` regret holds asymptotically and only once `n ≫ K`. Here `n < K`.

**Honest verdict: at the stated budget the value comes entirely from (i) the ladder prior and (ii) early termination. The tree is decoration.** Say this in `DECISIONS.md` rather than pretending otherwise.

### 1.5 Mitigation: parallelism is the lever, and it is sufficient

Measured: 8 concurrent compose projects give a 3.8× speedup on orchestration, and rollouts are dominated by *waiting* (DRIVE/QUIESCE are wall-clock, not CPU), which parallelises better than the control-plane work I measured.

**With W = 4 workers** (contention surcharge ~8 % → 21 s/probe, 31 s/rollout):

| | |
|---|---|
| Probe sweep (22 worlds ÷ 4 × 21 s) | **116 s: fits the 120 s budget** |
| Escalation rollouts (480 s × 4 ÷ 31 s) | **62 rollouts** |
| Root arms after progressive widening `⌈2√62⌉` | **16** → ~3.9 visits/arm, top arms 5–8 visits |

That is still below the 15 needed for confidence at depth ≥ 2, but **UCT/Thompson now carries real signal at the root**. W = 6 → 93 rollouts; W = 8 → 124 rollouts.

**Projected time-to-first-violation, W = 4:** ladder-ordered sweep reaches Rung 2 `proc.pause` in batch 2 → REINFORCE at ~50 s. Target is a depth-2 compound (`proc.pause(role:leader)` ∧ `net.partition(minority)`), which Rung 4 explicitly biases toward; under ladder-ordered widening it is among the first ~8 children opened. At p ≈ 0.3: `8 + 1/0.3 ≈ 11 rollouts × 31 s ÷ 4 = 85 s`. **Total ≈ 135 s ≈ 2.3 min, against a 10 min DoD: 4× margin.**

**DoD #2 is achievable, but only with parallelism, and only because the ladder encodes the answer.** Note the integrity problem this creates: A.7 Rung 4 literally says *"this is the canonical Raft lease bug trigger."* That is a hardcoded prior. DoD #2 says "no hardcoded fault schedules": a prior is not a schedule, so it is defensible, but **DoD #4's comparison against uniform random is rigged unless the random baseline is granted the same 2-fault-overlap bias.** Otherwise you are measuring the ladder, not the search.

### 1.6 Why W = 4 default, and what breaks

- **Port collisions**: solved by *not publishing host ports at all*. My W=8 benchmark used per-project bridge networks and zero published ports and worked cleanly. The harness reaches nodes by container IP on the world's own network or via `docker exec`. **If Phase 0 hardcodes `ports: ["8080:8080"]` in the fixture compose, N=2 collides and the entire parallel plan dies.** Where host ports are unavoidable, a `PortLeaser` with an exclusive lock on `.prothesis/ports.lock`.
- **CPU contention distorting timing-sensitive faults**: measured **+3.0–4.9 % at full 8-vCPU saturation, +0.96 % idle**. A 300 ms election timeout ±5 % = ±15 ms (tolerable); a 500 ms probe window ±5 % = ±25 ms; the lease-overlap measurement needs sub-100 ms resolution. W=4 keeps the box at ~50–60 % and distortion near the +1 % baseline. Apply per-service `cpus:`/`mem_limit` via a generated compose override so one runaway world cannot starve peers.
- **Network pool**: hard ceiling 24 free. Clamp `W ≤ min(cfg.Workers, ⌊dockerCPUs/2⌋, ⌊freeNetworks/2⌋)` = 4 by default, 12 absolute max.
- **Leaked projects**: guaranteed over a 30 m search at W=8. `Reap()` at startup is load-bearing, not hygiene.
- **Foreign containers**: `chord-api` spiked to 75 % CPU unprompted. `thesis doctor` must warn on foreign containers; every `WorldResult` must record an `EnvFingerprint`.
- **Memory is never the constraint**: 4 workers × (3 kv @ ~100 MB + loadgen ~150 MB) ≈ 1.8 GB against 30.05 GiB.
- **Replay and shrink must run at W = 1.** The `3/3` confirmation gate must not be polluted by contention.

### 1.7 The other levers, ranked

1. **Parallelism (W=4–6)**: 4–6×. The only sufficient lever.
2. **Early termination (A.5 #2)**: required, not optional. Cancel the world's context the instant an oracle violation is observable during DRIVE/PERTURB. Saves ~40 % on successful rollouts.
3. **Progressive abandonment (my addition)**: at t = 0.5·DRIVE, if the OBSERVE spiral score is below threshold and no violation, cancel and backprop partial utility at reduced weight. Turns a 29 s worst case into ~19 s expected. Must set `Truncated: true` so a truncated world never counts as a clean PASS.
4. **Shorter probe worlds**: bounded below by the 5 s lease; ~19 s → ~17 s at best.
5. **Warm topology reuse: reject.** Measured payoff 3.3 s of 29 s (11 %) and it breaks I2.

---

## 2. MCTS over a non-stationary, noisy environment

### 2.1 A.5's UCT formula is arithmetically wrong

```
UCT(node) = (U_avg / visits) + C × sqrt(ln(parent.visits) / visits)
```

If `U_avg` is already the average, dividing by `visits` again decays the exploitation term as `1/n`. A node visited 3× with avg 141 scores 47; a node visited once with avg 100 scores 100. **This inverts the intent and double-counts the exploration preference.** The field must be `U_total` (a sum) and the formula `U_total/visits + C·√(ln N/n)`. Report in `OPEN_QUESTIONS.md`.

### 2.2 C = 1.41 is the wrong constant at this reward scale

√2 is correct only for rewards normalised to [0, 1]. Utilities here span roughly [−50, +150]. At n=1, N=3 the exploration bonus is `1.41·√(ln3/1) ≈ 1.48` against reward differences of ~100. **UCT degenerates to pure greedy exploitation.** Fix: affine-normalise utility to [0,1] using a running (min, max) over observed tree utilities (the standard MCTS fix for unbounded rewards) after which C = 1.41 is meaningful.

### 2.3 The parsimony penalty interacts badly with high-variance rewards, and biases the search shallow

The penalty (−3/fault, −0.1/s) is **deterministic and always applied**; the violation reward is **stochastic**. With one sample per arm:

- depth-1 node that missed its violation: `−3 − 3 = −6`
- depth-4 node that missed its violation: `−12 − 3 = −15`

So the search is systematically pushed **shallow: exactly against Rung 4 COMPOUND**, which the ladder itself identifies as where the bugs are, and which the canonical lease bug requires (2 concurrent faults, depth 2).

**Recommendation:** split the function.

- `Utility()`: A.6 **verbatim, unmodified, normative**. Used for the verdict, for ranking candidate repros, and for the parsimony guarantee the spec wants.
- `SearchUtility()`: used inside MCTS backprop, with the parsimony term **depth-normalised** (or scaled to ~10 % of its reporting weight) so it discriminates *among siblings at equal depth* without penalising depth itself.

This honours A.6's "must not be modified without recording in `DECISIONS.md`" by recording it, and preserves A.6 for the purpose its "Key property" paragraph actually describes (choosing between two worlds that *both* found the bug).

### 2.4 Non-stationarity

MCTS is inherently non-stationary: a node's value distribution shifts as the policy below it improves. Kocsis–Szepesvári handle this with a bounded-drift assumption that only washes out as n → ∞. On top of that we have *environmental* non-stationarity (Tier B: the same schedule replayed gives 3/3 or 1/5). Both push the same direction: **estimates at internal nodes are unreliable at our n.**

Mitigation that costs nothing: use a **windowed/discounted average** at internal nodes (D-UCB, γ = 0.95, or a sliding window of the last 20 rewards) so stale rewards from an early bad subtree policy do not anchor the estimate. Moot at n = 62; it matters at the `soak` 8 h budget where n does get large.

### 2.5 Recommended bandit: Thompson sampling on a Beta-Bernoulli, with the ladder as the prior

| Policy | Verdict at n ≤ 3 pulls/arm |
|---|---|
| UCT (A.5) | Degenerates to first-play-urgency ordering. Zero information. Keep for spec conformance and DoD #4 ablation. |
| UCB1-Tuned | Needs an empirical variance `V_j`; undefined at n=1, unstable at n=3 for a bimodal Bernoulli. Its edge appears at n ≳ 30. **Marginal here.** |
| **Thompson (Beta-Bernoulli)** | **Recommended default.** |

Justification, four points:

1. **The dominant signal is binary**: violation / no violation. That is exactly Beta-Bernoulli. The continuous utility is a tiebreaker, not the signal.
2. **It is the best-performing bandit at very low pull counts** and is the only one of the three that expresses a *prior* natively, which is precisely how the escalation ladder should enter the algorithm. With 0 pulls, Thompson samples the prior, so it **reproduces ladder order in expectation and keeps working at n=1**, where UCT's exploration term is meaningless.
3. **It needs no reward normalisation**, sidestepping the C-scaling bug in §2.2 entirely.
4. **It degrades gracefully.** As visits accumulate the posterior sharpens; there is no cliff between "unvisited" and "visited once".

Prior construction: `α₀ = 1 + ladderBonus(rung) + 2·probeReinforceStrength`, `β₀ = 1 + 0.5·rung`. Novelty gives fractional credit `α += min(1, ΔNovelty/noveltyPerSuccess)`.

Ship all three behind `search.bandit: uct | uct_tuned | thompson` (default `thompson`) so A.5's UCT remains implemented and the DoD #4 comparison can be run as a genuine ablation.

---

## 3. The discretization. Exact enumeration rule.

### 3.1 Where the continuity actually goes away

- **Windows are not free parameters.** A.5 says a child's fault is "applied at the current virtual clock time", so *start time is determined by tree depth*, not chosen. This collapses the largest continuous dimension almost entirely. Formalise it as an **epoch grid**: DRIVE is divided into K = 8 decision epochs of `driveMS/8`.
- **Magnitudes are given by the ladder.** A.7 already names the canonical values. Promote them to a normative 3-level table (Low/Med/High) per kind.
- **Sub-epoch jitter is drawn from a PRNG stream and deliberately excluded from the `ActionID`**, so jittered variants transpose onto one tree node instead of shattering the tree.

### 3.2 Normative magnitude table (all 17 CRUCIBLE kinds)

| Kind | Low | Med | High |
|---|---|---|---|
| `net.latency` | 50 ms / 10 ms | 200 ms / 50 ms | 500 ms / 100 ms |
| `net.loss` | 1 % | 5 % | 20 % |
| `net.reorder` | 1 % | 5 % | 20 % |
| `net.duplicate` | 1 % | 5 % | 15 % |
| `net.bandwidth` | 10 Mbps | 1 Mbps | 100 Kbps |
| `net.partition` | n/a | n/a |: (asymmetry is a *target* property) |
| `proc.pause` | n/a | n/a |: (duration **is** the magnitude) |
| `proc.kill` | SIGTERM | SIGKILL | SIGKILL, no restart |
| `proc.slow` | 25 % | 50 % | 90 % |
| `proc.restart` | n/a | n/a | n/a |
| `clock.skew` | 100 ms | 1000 ms | 3000 ms |
| `clock.jump` | +1000 ms | +5000 ms | −5000 ms |
| `io.latency` | 50 ms | 200 ms | 800 ms |
| `io.error` | 1 % | 10 % | 50 % |
| `io.fill` | 80 % | 95 % | 99 % |
| `mem.pressure` | 50 % | 80 % | 95 % |
| `fd.exhaust` | n/a | n/a | n/a |

### 3.3 The exact enumeration rule

At node `v` of depth `d` with fault prefix `F(v) = [f₁..f_d]`:

```
1. KIND SET
   K(v) = allow ∩ ⋃_{r ≤ rmax(v)} Ladder[r].Kinds
   rmax(v) = min(RungTemporal, seedRung(v) + d)
   seedRung(v) = the ladder rung of the probe fault at the root.
   (Escalation semantics: climb at most one rung per level of depth.)

2. TARGET SET, per kind k — type-compatible only
   proc.*, clock.*, io.*, mem.*, fd.*   -> {role:leader, role:follower} ∪ {n_i}
   net.latency|loss|reorder|duplicate|bandwidth
                                        -> {role:leader, role:follower} ∪ {n_i} ∪ {edges}
   net.partition                        -> {minority(svc), majority(svc)}
                                           ∪ {isolate(n_i)} ∪ {asymmetric edges}

   ROLE-FIRST RULE: role and quorum targets enumerate BEFORE node-id targets.
   Role targets are stable across worlds; a node-id target is not portable and
   shatters the transposition table. Node-id targets open only after role
   targets at that (kind, mag) are exhausted.

3. MAGNITUDE
   M(k) = {Low, Med, High} if Parametric(k) else {None}
   Gated by rung: net.latency Low is Rung 0, net.latency High is Rung 1.

4. DURATION
   D = {Short = 1 epoch, Med = 2 epochs, TillHeal = [start, HEAL)}
   Probe worlds force D = Short (A.3: 500–2000 ms).

5. TIMING
   StartEpoch(child) ∈ { d, d+1 }                      (sequential escalation)
                     ∪ { StartEpoch(f_d) }  iff Overlap admissible
   Overlap admissible iff |concurrent(v)| < max_concurrent_faults
                      AND rmax(v) ≥ RungCompound.
   Sub-epoch jitter ~ stream mcts.jitter:<nodePath>:<visit>, NOT in the ActionID.

6. CONSTRAINT FILTER — drop action a if
   |F(v)|+1 > max_faults_per_world
   OR concurrency(F(v) ∪ {a}) > max_concurrent_faults
   OR perturber.constraints reject it ("never partition more than minority of kv")
   OR a.ID() already appears on the path.

7. TOTAL ORDER (no ties, so ordering is deterministic without a PRNG)
   sort by ( Ladder.Priority(a),
             −ProbePrior(a.Kind, a.Target),
             a.Mag asc, a.Dur asc,
             a.Overlap desc if rmax ≥ RungCompound,
             a.ID() )

8. PROGRESSIVE WIDENING
   |children(v)| ≤ ⌈C_pw · n(v)^α⌉,  C_pw = 2, α = 0.5
   When widening admits a slot, open the next action in the order from (7).
```

### 3.4 Worked cardinality for the KV fixture root (`seedRung = GrayFail = 2`, `d = 0`)

Kinds ≤ Rung 2 ∩ `allow` = {`net.latency`, `clock.skew`, `net.loss`, `io.latency`, `proc.pause`}.

| Kind | Targets | Mags | Durs | Actions |
|---|---|---|---|---|
| `proc.pause` | 5 (2 roles + 3 nodes) | 1 | 3 | 15 |
| `net.latency` | 8 (+3 edges) | 3 | 3 | 72 |
| `net.loss` | 8 | 3 | 3 | 72 |
| `clock.skew` | 5 | 3 | 3 | 45 |
| `io.latency` | 5 | 3 | 3 | 45 |
| | | | **Total** | **249** |

Progressive widening at n = 62 root visits opens `⌈2·√62⌉ = 16` children. **We explore 6.4 % of the root action space.** This is the quantitative proof that *the ordering in step 7 (the ladder prior plus the probe prior) is the algorithm*, and the tree is a thin refinement on top of it. Design accordingly: invest in the prior, not in tree depth.

`max_mcts_depth = 4` (A.10) is fine; the binding constraint is width, not depth.

---

## 4. Determinism. A.8 is unsatisfiable as written. Here is what IS achievable.

### 4.1 The PRNG design (this is the critical Phase 0 delta)

**A single shared `*rand.Rand` is fatal**: with W parallel workers, draw *order* becomes nondeterministic, so nothing downstream is reproducible. The Saboteur must draw from **derived, domain- and path-addressed streams**, so draw order across streams is irrelevant.

```go
// internal/recorder/prng.go — MUST land in Phase 0
package recorder

import (
    "crypto/sha256"
    "encoding/binary"
    "math/rand/v2"
)

// Streams is a splittable family of deterministic generators. Streams are
// addressed by (domain, path...), never by draw order, so parallel workers
// cannot perturb one another's randomness.
type Streams struct{ root [32]byte }

func NewStreams(seed uint64) *Streams {
    var b [8]byte
    binary.BigEndian.PutUint64(b[:], seed)
    return &Streams{root: sha256.Sum256(b[:])}
}

func (s *Streams) Derive(domain string, path ...string) *rand.Rand {
    h := sha256.New()
    h.Write(s.root[:])
    h.Write([]byte(domain))
    for _, p := range path {
        h.Write([]byte{0})
        h.Write([]byte(p))
    }
    var k [32]byte
    copy(k[:], h.Sum(nil))
    return rand.New(rand.NewChaCha8(k)) // math/rand/v2, natively 32-byte seeded
}

func (s *Streams) Root() [32]byte { return s.root }
```

`math/rand/v2.NewChaCha8([32]byte)` is exactly the right primitive and is available in Go 1.27.1.

### 4.2 The stream table: every draw the Saboteur makes

| Domain | Path | Consumer | Drawn at |
|---|---|---|---|
| `topology` | worldID | topology variant | world materialization |
| `driver` | worldID | loadgen `{seed}` | world materialization |
| `probe.plan` | n/a | sweep ordering, node shuffle | probe planning |
| `probe.jitter` | probeID | window jitter in bucket | world materialization |
| `mcts.widen` | nodePath | tie-break in widening order | expansion |
| `mcts.select` | nodePath, visitIdx | Thompson Beta draws / UCT tie-break | selection |
| `mcts.jitter` | nodePath, visitIdx | sub-epoch window jitter | materialization |
| `mcts.rollout` | nodePath, visitIdx | default-policy completion | materialization |
| `harness.ports` | worldID | ephemeral port lease | `Up()` |

`visitIdx` = the node's visit counter at the moment of the draw. That makes each draw stable under replay *given the same visit sequence*, which is exactly the D1 property below.

### 4.3 What determinism is and is not achievable: stated precisely

- **D1: Decision-function determinism. ACHIEVABLE and testable.**
  `Decide(treeState, observations, streams) → Action` is pure and total. Same tree state + same observation history + same root seed ⇒ same action, regardless of worker count, map iteration order, or wall clock. Enforced by: derived streams (§4.1), **sorted iteration everywhere, never `range` over a map**, no `time.Now()` in the decision path (virtual clock only), and one mutex serialising all tree mutations. Unit-testable in milliseconds against a mocked `Executor`.

- **D2: Artifact determinism. ACHIEVABLE, and this is what I2 actually demands.**
  Every world the Saboteur emits is a complete self-contained `(seed, topology_variant, driver_profile, fault_schedule, phase_timings)` tuple that replays independently of the search that found it. **I2 requires the *violation* to be reproducible, not the *search*.**

- **D3: Session determinism. NOT ACHIEVABLE under Tier B.**
  Tree shape is a function of observed utilities; utilities are Tier B nondeterministic (the spec's own "1/5"). Two runs with the same seed diverge the moment one rollout's violation fires and the other's does not, and every subsequent selection differs thereafter. **A.8 as literally written ("tree expansion must be deterministic given the same PRNG seed") is unsatisfiable and must be amended.** → `OPEN_QUESTIONS.md`.

- **D3′: Journal replay. ACHIEVABLE substitute, and it delivers what A.8 was reaching for.**
  `.prothesis/runs/<id>/saboteur_journal.jsonl` records `(nodePath, actionID, streamCounters, observedReward, violated, completionOrder)`. `thesis search --replay-journal <f>` reconstructs a bit-identical tree without executing anything. Debuggable, and CI-testable in milliseconds.

### 4.4 The parallel-backprop wrinkle

With W > 1, *completion order* is nondeterministic even if selection is not. Two resolutions, both cheap:

- `--deterministic-backprop` (**default for `thesis gate`**): buffer a full batch of W results and apply them in *submission* order. Costs a small latency; makes D1 hold at batch granularity even with W > 1.
- Free-running (**default for `thesis search`**): record actual completion order in the journal; D3′ covers reproduction.

---

## 5. Utility function fidelity: A.2 vs A.6

### 5.1 A.6's worked arithmetic: both examples are CORRECT

```
2 faults / 30 s:   100 × 1.5 + 0 − (3×2) − (0.1×30)  = 150 − 6 − 3   = 141  ✓
12 faults / 120 s: 100 × 1.5 + 0 − (3×12) − (0.1×120) = 150 − 36 − 12 = 102  ✓
```

### 5.2 But the CLAIM the arithmetic supports is false

> *"The Saboteur will always prefer the minimal path."*

**Counterexample using the spec's own weights:** the 12-fault / 120 s world, if it additionally discovers 5 novel log templates, scores `102 + 50 = 152 > 141`. **The Saboteur prefers the 12-fault world.**

The exchange rates are:
- `β/γ_fault = 10/3` → **one novel log template outweighs 3.33 faults**
- `β/γ_dur = 10/0.1` → **one novel log template outweighs 100 seconds**

Worse, **novelty is unbounded and structurally correlated with fault count**: a 24-fault world touches more code and therefore emits more novel templates than a 2-fault world, essentially by construction. **At cold start (which is exactly DoD #2's scenario) the corpus is empty, novelty dominates, and parsimony is irrelevant.** This directly threatens DoD #3 (≤ 4 faults).

*Mitigation:* saturate the novelty term (`u += 10.0 * min(NewTemplates, noveltyCap)` with `noveltyCap` default 5) and/or use `SearchUtility()` per §2.3. Record in `DECISIONS.md`.

### 5.3 A.2 vs A.6 class-weight divergence

| Class | A.2 box | A.6 code |
|---|---|---|
| consistency | 100 | 100 |
| crash | 80 | 80 |
| **safety** | **absent** | **60** |
| resource | 40 | 40 |
| liveness | 20 | 20 |
| **convergence** | **absent** | **20** |
| **differential** | absent | **absent → 0.0** |
| **metamorphic** | absent | **absent → 0.0** |

- A.2 omits `safety` (60) and `convergence` (20). **A.6 governs** (it is the normative code).
- **A.6's switch has no `default:` case.** CRUCIBLE §B defines **eight** oracle classes; A.6 scores six. A `differential` or `metamorphic` violation contributes **exactly 0.0 utility: the Saboteur is blind to two of the eight normative classes.**
- **Worse, this is exploitable given I3 ("oracles are the product").** External oracles are pluggable processes that declare their own `class` string (§4.5). A third-party oracle declaring `class: "correctness"` contributes 0.0 and is **invisible to the search**: the user's most valuable custom oracle would be silently ignored. **Add `default: u += 20.0` plus a one-time warning log.**
- A.2 writes `U = R_viol × Severity` but gives no multiplier table; A.6 supplies high 1.5 / medium 1.0 / low 0.5. Not contradictory, just incomplete in A.2.
- β and γ **agree** between A.2 and A.6.

### 5.4 `Duration` is ambiguous between the two, and the two readings differ by 2×

A.2: `−γ × (FaultCount + Duration)` reads as **total fault duration**. A.6: `result.DurationSeconds` reads as **world wall-clock duration**. For the canonical repro (`proc.pause@8200..15100` = 6.9 s + `net.partition@8400..14900` = 6.5 s = 13.4 s of fault, in a ~30 s world) the penalty is **1.34 vs 3.00**. Must be resolved. **Recommend world wall-clock**: it matches A.6's field name and creates the right incentive (fast repros), which the budget arithmetic in §1 badly needs.

### 5.5 A.10's config cannot express A.6

`search.utility` exposes only `violation_weight`, `novelty_weight`, `fault_penalty`, `duration_penalty`. There is **no knob for the per-class ratios (80/60/40/20), none for `novelty_state_weight` (5), and none for the severity multipliers.** Either extend the config or document that only those four are tunable and the rest are frozen. **This matters for the lock file**: CRUCIBLE §E.2 makes profile budgets and the fault space lock-covered, so the shape of `search:` must be settled before `.prothesis/lock` first exists.

---

## 6. Probe classification: A.3 and A.4 are two different schemes. Reconciliation.

### 6.1 The four conflicts, stated exactly

1. **Different codomains.** A.3 → `{DAMPEN, REINFORCE}`. A.4 → `{DAMPEN, REINFORCE_LINEAR, REINFORCE_ACCELERATING}`. **A.9 DoD #1 demands the probe sweep emit `REINFORCE_ACCELERATING`, which A.3's classifier literally cannot produce.** DoD #1 is unsatisfiable by A.3 alone.
2. **Different input types.** A.3 criterion (c) is a **discrete event** (role change). A.4 requires a **numeric time series** to regress. *You cannot compute the second derivative of a boolean.*
3. **Different sampling rates.** A.3 says **200 ms**; A.4 and A.10 `observe.sample_interval_ms` say **500 ms**. With `spiral_window = 5`, 500 ms gives a **2.5 s regression window**: **longer than the probe's own 500–2000 ms fault window.** The trend regression physically cannot resolve the fault it is meant to measure. Real defect.
4. **The constant-signal problem.** In a *correct* Raft, pausing the leader **must** trigger an election. So criterion (c) fires on **every** leader `proc.pause` regardless of bugginess. It has **zero discriminative power**: it is a detector of Raft-ness, not of bugginess. It would fire identically after the lease bug is patched, breaking A.1's "adaptive re-engagement" premise. And since A.3 ranks REINFORCE signals by strength, a constant term dominates and flattens the ranking.

### 6.2 Reconciliation: one pipeline, two layers

Treat A.4 as the **primitive** and A.3's criteria as **evidence predicates feeding it**, not as a rival scheme.

- **A.4's `ClassifySignal` is kept exactly as specified**: one numeric metric series → one of three classes.
- **A.3 criteria (a), (b), (d) map natively onto it**:
  - (a) queue depth → `ClassifySignal(queue_depth)`
  - (b) retry count → `ClassifySignal(retry_rate)`
  - (d) error rate → `ClassifySignal(error_rate)` restricted to the post-withdrawal window
- **A.3 criterion (c) is demoted from a class member to a quantitative gate.** Replace "a role change occurred" (constant) with two measured quantities:
  - `RoleChurn`: count of term changes and time-to-stable-leadership after HEAL. A correct Raft: one election, converging within ~1 election timeout. A degraded one: term thrashing.
  - **`LeaseOverlapMs`: the discriminating signal.** `overlap_ms = max(0, lease_expiry(old_leader) − election_time(new_leader))`. This is **> 0 exactly when the lease bug is exploitable and 0 for a correct implementation**, and it is derivable from logs alone (term changes + resume timestamp) with **no instrumentation**, which satisfies CRUCIBLE §L's "must work on unmodified systems".
- **Node classification** = `max` over metric series of `ClassifySignal`, with `RoleChurn`/`LeaseOverlapMs` acting as a **multiplier on signal strength**, never as a member of the class set.
- **DoD #1 is then satisfied legitimately**: `REINFORCE_ACCELERATING` for leader `proc.pause` arises from the **driver error/anomaly-rate series**, when the paused leader resumes and serves stale reads, the anomaly rate rises with positive acceleration as more clients hit the stale leader. That is A.4's actual algorithm producing the required label from a real metric, not a boolean smuggled into an enum.
- **Sampling:** unify with a probe-specific rate. Add `observe.probe_sample_interval_ms: 200` (A.3) alongside `sample_interval_ms: 500` (A.4/A.10), and constrain `spiral_window × interval ≤ ½ × fault_window`.

### 6.3 A second, structural conflict in A.4/A.5

A.4: *"A `REINFORCE_ACCELERATING` classification on any metric triggers **immediate** escalation to Tier 2."* A.5: *"Root node: the current observed distributed state **at the moment** REINFORCE was detected."*

**You cannot snapshot a live distributed state and branch from it under a compose harness.** There is no fork primitive for a running 3-container topology, and mid-world escalation would also violate I5 (assertions belong in ASSERT, after HEAL and QUIESCE).

**Resolution; the only implementable reading under I1 (real execution only):** the MCTS root is **not** a live state snapshot. It is the *(probe fault, target, timing) prefix* that produced the REINFORCE signal, and **every rollout re-executes that prefix from BOOT**. This is a significant reinterpretation of A.5 and belongs in `DECISIONS.md`. Cost consequence: each rollout re-pays BOOT+SEED (~5.5 s), which my measurements show is affordable, and it is already priced into the §1.1 model.

---

## 7. Package design

### 7.1 Layout

```
internal/search/
  strategy.go      // Strategy, Executor, Budget — the seam that makes the
                   // Saboteur testable without Docker
  saboteur/
    doc.go
    config.go      // A.10 config + extensions, defaults, validation
    action.go      // Action, ActionID, canonical encoding, total order
    ladder.go      // Rung, Ladder, magnitude tables, Priority()
    discretize.go  // the §3.3 enumeration rule
    probe.go       // Tier 1 sweep planner, ProbeResult, ranking
    observe.go     // TelemetrySample, ClassifySignal, SpiralClass, Observer
    signal.go      // A.3 evidence predicates reconciled onto ClassifySignal;
                   // RoleChurn, LeaseOverlapMs
    utility.go     // Utility() (A.6 verbatim) + SearchUtility() + normalizer
    node.go        // Node, tree storage, transposition key
    bandit.go      // Policy interface
    uct.go         // UCT + UCB1-Tuned (A.5 conformance)
    thompson.go    // Beta-Bernoulli Thompson (recommended default)
    mcts.go        // search loop, progressive widening, virtual loss, backprop
    escalate.go    // Tier 2 driver: root construction, budget management
    scheduler.go   // parallel rollout pool, worker leasing, W clamping
    journal.go     // decision journal (record / replay)
    serialize.go   // tree -> .prothesis/runs/<id>/tree.json
    telemetry.go   // adapters from recorder
    saboteur.go    // top-level Strategy implementation
```

### 7.2 The seam (in `internal/search`, so `saboteur` never imports Docker)

```go
package search

type Executor interface {
    // Execute runs one real world. It MUST honour ctx cancellation for
    // early termination (A.5 #2) and progressive abandonment.
    Execute(ctx context.Context, w *schema.World) (*schema.WorldResult, error)
    Workers() int
}

type Strategy interface {
    Plan(ctx context.Context, b *Budget) (*schema.Verdict, error)
}
```

This single interface is what lets the entire MCTS loop, the discretizer, the bandits, and D1 determinism be unit-tested in milliseconds against a mocked `Executor`: essential, because a Docker-backed test of the search loop costs 10 minutes per run.

### 7.3 Core types

```go
// action.go
type Magnitude uint8
const (MagNone Magnitude = iota; MagLow; MagMed; MagHigh)

type DurationClass uint8
const (DurShort DurationClass = iota; DurMed; DurTillHeal)

type TargetKind uint8
const (TgtNode TargetKind = iota; TgtRole; TgtQuorum; TgtEdge; TgtWildcard)

type Target struct {
    Kind TargetKind
    Expr string // symbolic: "role:leader", "minority(kv)", "n1<->n2"
}

// Action is one MCTS edge: fully discrete, canonically encodable.
type Action struct {
    Kind       schema.FaultKind
    Target     Target
    Mag        Magnitude
    Dur        DurationClass
    StartEpoch uint8
    Overlap    bool
    Rung       Rung
}

type ActionID [16]byte
func (a Action) ID() ActionID      // stable canonical hash; excludes jitter
func (a Action) String() string    // renders exact fault grammar once bound

// node.go
type Node struct {
    Parent     *Node
    Action     Action
    Children   []*Node
    Depth      int
    FaultCount int

    Visits  int
    UTotal  float64 // SUM, not average — see §2.1
    USq     float64 // for UCB1-Tuned variance
    Alpha   float64 // Thompson posterior
    Beta    float64
    VLoss   int     // virtual loss, for parallel MCTS

    Unopened []Action // remaining actions in §3.3 step-7 order
    Pruned   bool
}

func (n *Node) Path() []ActionID   // addresses the PRNG stream and the journal

// bandit.go
type Reward struct {
    Utility   float64
    Violated  bool
    Class     string
    Novelty   int
    Truncated bool
}

type Policy interface {
    Select(parent *Node, rng *rand.Rand) *Node
    Backup(n *Node, r Reward)
    Prior(a Action, p ProbePrior) (alpha, beta float64)
}
```

### 7.4 The corrected UCT, and Thompson

```go
// uct.go — A.5 conformance, with the §2.1 and §2.2 defects fixed.
func (p *UCTPolicy) score(parent, c *Node) float64 {
    v := c.Visits + c.VLoss
    if v == 0 { return math.Inf(1) }
    exploit := p.norm.To01(c.UTotal / float64(v)) // running min/max normaliser
    explore := p.C * math.Sqrt(math.Log(float64(parent.Visits+parent.VLoss))/float64(v))
    return exploit + explore
}

// thompson.go — recommended default.
func (p *ThompsonPolicy) Select(parent *Node, rng *rand.Rand) *Node {
    var best *Node
    bestT := math.Inf(-1)
    for _, c := range parent.Children { // Children kept in §3.3 step-7 order
        if c.Pruned { continue }
        t := betaSample(rng, c.Alpha, c.Beta)
        // A.5 tie-break: prefer fewer faults (parsimony bias).
        if t > bestT || (t == bestT && best != nil && c.FaultCount < best.FaultCount) {
            best, bestT = c, t
        }
    }
    return best
}

func (p *ThompsonPolicy) Backup(n *Node, r Reward) {
    if r.Violated { n.Alpha++ } else { n.Beta++ }
    n.Alpha += math.Min(1.0, float64(r.Novelty)/p.NoveltyPerSuccess)
    n.UTotal += r.Utility
    n.USq += r.Utility * r.Utility
    n.Visits++
}
```

### 7.5 The MCTS loop

```go
func (s *Saboteur) Escalate(ctx context.Context, root *Node, b *search.Budget) error {
    for b.Remaining() > 0 && !s.pool.Saturated() {
        s.mu.Lock()
        path := s.selectPath(root)                    // Policy.Select down to a leaf
        if child, ok := s.widen(path[len(path)-1]); ok { path = append(path, child) }
        s.applyVirtualLoss(path)                      // MANDATORY: without it all W
        s.mu.Unlock()                                 // workers pick the same path

        w := s.materialize(path)  // path -> complete .thesis world, BOOT-to-TEARDOWN (I2)

        s.pool.Submit(ctx, w, func(res *schema.WorldResult, err error) {
            r := s.reward(res, err)
            s.mu.Lock()
            s.removeVirtualLoss(path)
            s.backup(path, r)                         // uses SearchUtility (§2.3)
            s.prune(path[len(path)-1])                // A.5 #3
            s.journal.Record(path, r)                 // D3'
            s.mu.Unlock()
        })
    }
    return s.pool.Drain(ctx)
}

func (s *Saboteur) widen(v *Node) (*Node, bool) {
    limit := int(math.Ceil(s.cfg.PWConst * math.Pow(float64(v.Visits), s.cfg.PWAlpha)))
    if len(v.Children) >= limit || len(v.Unopened) == 0 { return nil, false }
    a := v.Unopened[0]                     // already in §3.3 step-7 total order
    v.Unopened = v.Unopened[1:]
    c := newNode(v, a)
    c.Alpha, c.Beta = s.policy.Prior(a, s.probePrior)   // ladder enters HERE
    v.Children = append(v.Children, c)
    return c, true
}
```

Three loop details that are not optional:

- **Virtual loss**: without it, all W workers select the identical path and you burn W rollouts on one action.
- **`materialize(path)` produces a complete world from BOOT**, per §6.3, not a continuation of a live one.
- **Early termination + progressive abandonment** live inside `Executor.Execute` via `ctx` cancellation, and set `Truncated: true` so a truncated world never counts as a clean PASS.

### 7.6 Concurrency plan

```go
type Scheduler struct {
    W     int
    ports *PortLeaser    // flock on .prothesis/ports.lock
    nets  *NetworkBudget // hard cap; measured 24 free on this host
    sem   chan struct{}
}

// W = min(cfg.Workers, dockerCPUs/2, freeNetworks/2)   -> 4 on this machine
```

- Compose project name `prothesis-<runID>-<worldID[:8]>`; own bridge network; **zero published host ports**.
- `Reap()` at startup: `docker compose ls --filter name=prothesis- -q` → `down -v` each. Load-bearing, given the 24-network ceiling.
- Generated compose override applies `cpus:` / `mem_limit` per service so no world starves its peers.
- Every `WorldResult` records `EnvFingerprint{workers, foreignContainers, hostLoad1m}`. On replay, a materially different fingerprint marks the attempt **INCONCLUSIVE rather than counting toward `reproduced: k/n`**; this is the honest handling of my measured +3–5 % timing distortion and it directly serves CRUCIBLE §C's rule: *never claim determinism it does not have*.
- **`thesis replay` and `thesis shrink` run at W = 1, always.**

