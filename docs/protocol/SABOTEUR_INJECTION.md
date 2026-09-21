---

## ADDENDUM A — THE SABOTEUR ENGINE (Game-Theoretic Adversarial Search)

> **Priority**: This addendum supersedes the coverage-guided mutation loop described in Phase 4 Section 6 of the base specification. Where the base spec describes stochastic corpus mutation, this addendum replaces it with a structured adversarial search. All other Phase 4 deliverables (log-template coverage, state-abstraction coverage, corpus management) remain in effect and serve as input signals to the Saboteur.

---

### A.1 Conceptual Framework

PRO-THESIS does not treat fault injection as random sampling. It models every test session as an **adversarial Markov Decision Process (MDP)** in which a single rational agent — **the Saboteur** — acts against a stochastic environment (the distributed system under test).

The system under test is **not** a strategic player. It is a deterministic program with internal nondeterminism (thread scheduling, real network jitter, GC pauses). It does not make choices; it executes code paths. The Saboteur is the sole decision-making agent. This distinction is architecturally important: do not implement two-player game solvers (e.g., CFR, Nash equilibrium search). Implement a **single-agent adversarial search using Monte Carlo Tree Search (MCTS) with UCT selection**.

This framing produces three structural advantages over stochastic fuzzing:

1. **Directed search**: The Saboteur observes the system's live telemetry and selects faults that exploit observed weakness, rather than guessing blindly.
2. **Intrinsic minimality**: The utility function penalizes fault count and duration, so the Saboteur is structurally motivated to find the most economical kill — often eliminating the need for post-hoc `ddmin` shrinking.
3. **Adaptive re-engagement**: When a coding agent patches a bug and re-runs `thesis gate`, the Saboteur does not replay old schedules. It plays a new game session, probing the patched code for novel counter-exploits.

---

### A.2 The Saboteur Subsystem

Add a new top-level subsystem to the architecture diagram. The Saboteur sits inside the SEARCH subsystem and governs fault selection strategy:

```
┌─────────────────────────────────────────────────────────────┐
│                      THE SABOTEUR                           │
│               (Adversarial MCTS Agent)                      │
│                                                             │
│  ┌───────────────┐    ┌────────────────┐    ┌────────────┐ │
│  │     PROBE     │───▶│    OBSERVE     │───▶│  ESCALATE  │ │
│  │   (Tier 1)    │    │  (Telemetry)   │    │  (Tier 2)  │ │
│  │               │    │                │    │            │ │
│  │ Inject cheap  │    │ Read live      │    │ MCTS+UCT   │ │
│  │ micro-faults  │    │ signals:       │    │ tree over  │ │
│  │ from grammar. │    │ - queue depth  │    │ fault      │ │
│  │ Measure log-  │    │ - retry count  │    │ grammar.   │ │
│  │ template and  │    │ - conn pool    │    │ Targeted   │ │
│  │ state-abstrac │    │ - error rate   │    │ kill shot. │ │
│  │ coverage Δ.   │    │ - role changes │    │            │ │
│  │               │    │ Classify:      │    │ Activated  │ │
│  │ Cost: ~5-15s  │    │ DAMPEN or      │    │ only when  │ │
│  │ per probe.    │    │ REINFORCE      │    │ OBSERVE    │ │
│  └───────────────┘    └────────────────┘    │ classifies │ │
│                                             │ REINFORCE. │ │
│                                             └────────────┘ │
│                                                             │
│  Utility Function (normative):                              │
│  U = R_viol × Severity + β × Δ_Novelty                     │
│      - γ × (FaultCount + Duration)                          │
│                                                             │
│  R_viol  = 100 (consistency), 80 (crash), 40 (resource),   │
│            20 (liveness)                                    │
│  β       = 10 per novel log-template, 5 per novel state    │
│  γ       = 3 per fault injected, 0.1 per second duration   │
└─────────────────────────────────────────────────────────────┘
```

**Implementation location**: `internal/search/saboteur/`

The Saboteur contains three sub-modules:

---

### A.3 PROBE (Tier 1 — Cheap Reconnaissance)

**Purpose**: Rapidly characterize the system's failure surface using low-cost fault injections. This replaces the base spec's "seed worlds (one per allowed fault kind)" with a structured probe sweep.

**Behavior**:
1. For each fault kind in `prothesis.yaml` → `perturber.allow`, generate a **micro-probe world**:
   - Single fault, short window (500ms–2000ms), minimal magnitude.
   - Example: `net.latency(mean=50ms, jitter=10ms)` on each node individually.
   - Example: `proc.pause` for 500ms on each node individually.
2. Execute each probe world against the system. Record:
   - Log-template coverage delta (`Δ_templates`).
   - State-abstraction coverage delta (`Δ_states`).
   - **Telemetry trajectory**: sampled every 200ms during `DRIVE`, capturing `(queue_depth, retry_count, active_connections, error_rate, role)` per node.
3. Classify each probe result into one of two categories:
   - **DAMPEN**: The system absorbed the fault gracefully. Metrics returned to baseline within 2× the fault window. Low priority for escalation.
   - **REINFORCE**: The system exhibited a reinforcing feedback signal. At least one of:
     - Queue depth monotonically increased for ≥3 consecutive samples after fault withdrawal.
     - Retry count exceeded 2× baseline during or after the fault window.
     - A role change (leader election, failover) occurred within 1 second of fault injection.
     - Error rate increased after fault withdrawal (the system got worse after the fault ended).

**Output**: A ranked list of `(fault_kind, target, observed_signal, classification)` tuples, ordered by reinforcing signal strength.

**Implementation**: `internal/search/saboteur/probe.go`

---

### A.4 OBSERVE (Telemetry Spiral Detector)

**Purpose**: Continuously classify the system's real-time telemetry during any world execution into DAMPEN or REINFORCE states. This is the Saboteur's "eyes."

**Telemetry Sources** (collected by RECORDER, consumed by Saboteur):
1. **Container/process metrics**: RSS, CPU%, FD count, thread/goroutine count. Sampled every 500ms.
2. **Application logs**: Streamed in real-time, parsed for log-template hashing and error pattern detection.
3. **Health probe results**: HTTP status codes and latencies from the `health` probes defined in `prothesis.yaml`.
4. **Driver operation metrics**: Outstanding operation count, operation latency P50/P99, error rate.

**Spiral Classification Algorithm**:
```go
func ClassifySignal(samples []TelemetrySample) SpiralClass {
    // Compute first derivative (trend) over sliding window
    trend := linearRegression(samples, windowSize=5)

    // Compute second derivative (acceleration)
    accel := linearRegression(trends, windowSize=3)

    if trend > 0 && accel > 0 {
        return REINFORCE_ACCELERATING  // System is getting worse, faster
    }
    if trend > 0 && accel <= 0 {
        return REINFORCE_LINEAR        // System is getting worse, steady rate
    }
    return DAMPEN                      // System is recovering or stable
}
```

A `REINFORCE_ACCELERATING` classification on any metric triggers immediate escalation to Tier 2.

**Implementation**: `internal/search/saboteur/observe.go`

---

### A.5 ESCALATE (Tier 2 — MCTS Adversarial Tree Search)

**Purpose**: When OBSERVE detects a reinforcing spiral, the Saboteur switches from cheap probing to targeted MCTS search to find the minimal fault combination that converts the spiral into an oracle violation.

**MCTS Tree Structure**:
- **Root node**: The current observed distributed state at the moment REINFORCE was detected, plus the active probe fault.
- **Child nodes**: Each child represents an additional fault action from the grammar, applied at the current virtual clock time.
- **Action space**: The full fault grammar filtered to actions compatible with current `perturber.budget` constraints (max concurrent faults, max total faults).
- **Terminal condition**: Oracle violation detected, or HEAL phase reached without violation, or fault budget exhausted.

**UCT Selection Policy**:
```
UCT(node) = (U_avg / visits) + C × sqrt(ln(parent.visits) / visits)
```
Where:
- `U_avg`: Average utility (from the payoff function in A.2) across all rollouts through this node.
- `C`: Exploration constant. Default `1.41` (√2). Tunable via `prothesis.yaml` → `search.exploration_constant`.
- `visits`: Number of times this node has been visited.

Select the child with the highest UCT score. On ties, prefer the child with fewer faults (parsimony bias).

**Rollout Policy**:
Each rollout executes a real world (not simulated). Because rollouts are expensive (25-60 seconds each), the Saboteur must be parsimonious:
1. **Maximum tree depth**: 4 additional faults beyond the triggering probe fault. Deeper trees are computationally infeasible at real-execution cost.
2. **Early termination**: If an oracle violation is detected during `DRIVE` or `PERTURB`, terminate the rollout immediately and backpropagate the full violation reward.
3. **Pruning**: If a node has been visited ≥3 times with zero coverage delta and zero violations, prune it from the tree (proven uninteresting).

**Backpropagation**:
After each rollout, compute utility $U$ using the payoff function and propagate the value up to the root. Update `U_avg` and `visits` for each node on the path.

**Budget Allocation**:
The Saboteur partitions the total world budget between Tier 1 and Tier 2:
- **Probe budget**: First 20% of worlds (or first 20% of wall-clock budget, whichever is reached first).
- **Escalation budget**: Remaining 80%, allocated to MCTS trees rooted at the top-ranked REINFORCE signals from probing.
- If no REINFORCE signal is detected during probing, fall back to the base spec's stochastic corpus mutation for the remaining budget. The Saboteur is an accelerant, not a replacement for baseline coverage.

**Implementation**: `internal/search/saboteur/escalate.go`, `internal/search/saboteur/mcts.go`, `internal/search/saboteur/uct.go`

---

### A.6 Utility Function (Normative)

The Saboteur's payoff function governs all search decisions. It is normative and must not be modified by the implementing agent without recording the change in `DECISIONS.md`.

```go
func Utility(result WorldResult) float64 {
    u := 0.0

    // Violation reward (highest weight)
    for _, v := range result.Violations {
        switch v.Class {
        case "consistency":
            u += 100.0 * severityMultiplier(v.Severity)
        case "crash":
            u += 80.0 * severityMultiplier(v.Severity)
        case "safety":
            u += 60.0 * severityMultiplier(v.Severity)
        case "resource":
            u += 40.0 * severityMultiplier(v.Severity)
        case "liveness", "convergence":
            u += 20.0 * severityMultiplier(v.Severity)
        }
    }

    // Novelty reward
    u += 10.0 * float64(result.Coverage.NewTemplates)
    u += 5.0 * float64(result.Coverage.NewStates)

    // Parsimony penalty (incentivizes minimal fault schedules)
    u -= 3.0 * float64(result.FaultCount)
    u -= 0.1 * result.DurationSeconds

    return u
}

func severityMultiplier(s string) float64 {
    switch s {
    case "high":
        return 1.5
    case "medium":
        return 1.0
    case "low":
        return 0.5
    default:
        return 1.0
    }
}
```

**Key property**: A world that finds a consistency violation using 2 faults in 30 seconds scores `100×1.5 + 0 - 6 - 3 = 141`. A world that finds the same violation using 12 faults in 120 seconds scores `100×1.5 + 0 - 36 - 12 = 102`. The Saboteur will always prefer the minimal path.

---

### A.7 Escalation Ladder (Normative Fault Sequencing)

When the Saboteur decides to escalate, it does not choose faults randomly from the grammar. It follows a **structured escalation ladder** inspired by how real distributed system failures cascade. The ladder defines the order in which fault categories are explored:

```
Rung 0 — RECON:       net.latency(50ms) | clock.skew(100ms)
                       Purpose: Detect timing-sensitive code paths.

Rung 1 — STRESS:      net.latency(500ms) | net.loss(5%) | io.latency(200ms)
                       Purpose: Push retry/timeout logic to its limits.

Rung 2 — GRAY FAIL:   proc.pause (SIGSTOP/SIGCONT)
                       Purpose: Create ambiguous liveness — node appears
                       alive to orchestrator, dead to peers. This is the
                       single most productive fault kind in distributed
                       systems testing.

Rung 3 — PARTITION:   net.partition (asymmetric preferred)
                       Purpose: Force quorum reconfiguration under load.

Rung 4 — COMPOUND:    Overlap two faults from Rungs 1-3 in time.
                       Purpose: Multi-fault interactions are where real
                       bugs hide. Bias toward overlapping proc.pause with
                       net.partition — this is the canonical Raft lease bug
                       trigger.

Rung 5 — TEMPORAL:    clock.jump(5000ms) | clock.skew(3000ms)
                       Purpose: Attack lease timers, TTLs, and any logic
                       that assumes monotonic or bounded clock drift.
```

The MCTS tree search uses this ladder as a **prior** for the rollout policy: when expanding a node, generate children in ladder order (Rung 0 first) and let UCT exploration lift higher rungs if lower rungs prove uninteresting. This front-loads cheap, high-signal probes before committing to expensive compound faults.

**Implementation**: `internal/search/saboteur/ladder.go`

---

### A.8 Integration with Existing Subsystems

The Saboteur integrates into the existing PRO-THESIS architecture as follows:

| Subsystem | Saboteur's Relationship |
| :--- | :--- |
| **RECORDER** | Saboteur reads virtual clock timestamps and PRNG streams. All Saboteur decisions (fault selection, tree expansion) must be deterministic given the same PRNG seed for Tier B reproducibility. |
| **PERTURBER** | Saboteur calls Perturber's `Inject(fault)` and `Withdraw(fault)` APIs. Saboteur decides *what* to inject; Perturber executes *how*. |
| **DRIVER** | Saboteur does not control workload directly. It reads the Driver's live operation metrics (outstanding ops, latency, error rate) via OBSERVE. |
| **ORACLE ENGINE** | Saboteur reads oracle verdicts after each world execution to compute utility. It does not modify or inspect oracle definitions. |
| **HARNESS** | Saboteur reads container/process health and resource metrics via OBSERVE. It does not control topology directly. |
| **SHRINK** | When the Saboteur's MCTS search produces a violation, the shrinking pipeline (Phase 5) still runs on the result. However, because the Saboteur's utility function penalizes fault count, the input to shrinking is typically already near-minimal (2-4 faults instead of 10-14). Shrinking serves as a confirmation and polishing step, not the primary minimization mechanism. |

---

### A.9 Modified Phase 4 Definition of Done

The base spec's Phase 4 definition of done is **replaced** with:

> **Done when**:
> 1. The Saboteur's Tier 1 PROBE sweep completes against the KV fixture and correctly classifies `proc.pause` on the leader node as `REINFORCE_ACCELERATING` (due to the lease bug).
> 2. The Saboteur's Tier 2 ESCALATE phase, starting from the `proc.pause` REINFORCE signal, discovers the stale-read bug via MCTS within **10 minutes** from a cold start (no pre-seeded corpus, no hardcoded fault schedules).
> 3. The discovered violation uses **≤ 4 faults** (demonstrating the utility function's parsimony incentive).
> 4. Across 10 independent trials with different PRNG seeds, the Saboteur's median time-to-first-violation is **≤ 50%** of the median time achieved by uniform random fault injection with the same budget.
> 5. `thesis search --budget 30m` completes without panic and produces well-formed `prothesis.verdict/v1` output.

---

### A.10 Configuration Extensions to `prothesis.yaml`

Add the following optional section to the configuration schema. All fields have defaults and are not required:

```yaml
search:
  strategy: saboteur           # saboteur | random | hybrid (default: saboteur)
  exploration_constant: 1.41   # UCT exploration parameter (default: √2)
  probe_budget_pct: 20         # % of total budget allocated to Tier 1 probes
  max_mcts_depth: 4            # maximum additional faults per MCTS branch
  escalation_ladder: default   # default | custom (if custom, reads .prothesis/ladder.yaml)
  utility:
    violation_weight: 100      # base reward for oracle violation
    novelty_weight: 10         # reward per novel log template
    fault_penalty: 3           # cost per fault injected
    duration_penalty: 0.1      # cost per second of execution
  observe:
    sample_interval_ms: 500    # telemetry sampling interval
    spiral_window: 5           # number of samples in trend regression window
    reinforce_threshold: 0.0   # minimum positive trend slope to classify REINFORCE
```

---
