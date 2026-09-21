# COMPREHENSIVE TEST PROTOCOL

**High-Assurance Verification & Fault-Injection Standard**

**Core Doctrine:** The entire value of a verification harness is that its verdicts are true. A false PASS is the catastrophic defect: worse than a crash, worse than a timeout, worse than no verdict at all. This protocol defines the mandatory rules for test design, execution, and release.

## 1. Foundational Invariants

- **The Soundness Invariant:** No gate may be weakened to allow a build to pass. A test that passes when a defect is present certifies the defect and must be treated as hostile code.
- **Empirical Measurement:** Never assert what was not directly measured. Every claim in a report, log, commit message, or ledger must cite the command executed alongside the quantitative result obtained.
- **The Refusal Surface Invariant:** The quality of a system is measured by what it refuses. When observation fails, the harness must emit INCONCLUSIVE loudly. Silence or implicit fallback to PASS is a protocol violation.
- **Normative Exit Codes:** Exit codes form an unbendable state contract:
  - `0` : PASS (Defect absence confirmed under tested operational profile)
  - `1` : FAIL (Oracle violation or planted defect caught)
  - `2` : INCONCLUSIVE (Refusal to judge: observation failure, harness anomaly, or unobserved world)
  - `3` : BUDGET_EXHAUSTED (Search, step, or wall-clock budget reached before definitive resolution)
  - `4` : ORACLE_DRIFT (Tamper detection triggered: oracle definition or binary hash mismatch)
  - `5` : CONFIG_ERROR (Invalid configuration, missing fixture, or malformed schema)

  Never invent an exit code and never collapse two.
- **Bitwise Determinism:** All state definitions, replay logs, and `.thesis` world files must be content-addressed canonical JSON. No unordered maps, no floating-point numbers, no wall-clock timestamps, and all comparators must define a total ordering.

## 2. The 5-Tier Verification Hierarchy

Testing proceeds upward through five distinct tiers. Higher tiers may not be executed if lower tiers are red.

```
+-----------------------------------------------------------------------+
| TIER 4: Dual-Arm Empirical Controls                                   |
| (Planted defect vs patched control; upstream etcd linearizable vs ser)|
+-----------------------------------------------------------------------+
                                  ^
+-----------------------------------------------------------------------+
| TIER 3: Containerized Fault-Injection & Seams                         |
| (Docker Compose, tc/netem network partitions, SIGSTOP process pauses) |
+-----------------------------------------------------------------------+
                                  ^
+-----------------------------------------------------------------------+
| TIER 2: Oracle & Invariant Engines                                    |
| (External linearizability checkers, search trees, single-key soundness)|
+-----------------------------------------------------------------------+
                                  ^
+-----------------------------------------------------------------------+
| TIER 1: Deterministic Unit & Contract Tests                           |
| (Total order sorts, canonical JSON, mock timeframes, PRNG seed streams)|
+-----------------------------------------------------------------------+
                                  ^
+-----------------------------------------------------------------------+
| TIER 0: Static Analysis, Provenance & Tamper Locks                    |
| (gofmt, vet, git commit/VCS build stripping, SHA-256 oracle locks)    |
+-----------------------------------------------------------------------+
```

### Tier 0: Static Analysis, Provenance & Tamper-Evident Locks

- **Linters & Formatters:** Code must be gofmt clean and pass go vet without warnings.
- **Reproducible Binaries:** Release binaries must be compiled with `-trimpath -buildvcs=false` and stamped with the exact commit SHA and committer date (never wall clock). Two consecutive builds must produce identical SHA-256 digests.
- **Oracle Lock Enforcement:** All oracle definitions and checker executables are fingerprinted in `.prothesis/lock`. If an oracle binary moves, the change must be re-locked with an explicit, human-reviewed reason stating whether code changed or solely the commit stamp moved.

### Tier 1: Deterministic Unit & Contract Tests

- Tests verify logic in isolation using synthetic data.
- Clocks, PRNGs, and network streams must be injected dependencies.
- Any test asserting ordering must test ties and permutations to ensure stable total ordering.

### Tier 2: Oracle Engines & Linearizability Checkers

- Invariant checkers (e.g., Porcupine/WGL linearizability checkers) run against execution histories.
- Verification must include both valid and invalid histories to confirm oracles fail loudly on bad state.

### Tier 3: Containerized Fault-Injection & Seams

- Executed against real containers managed via Docker Compose.
- Injects real kernel-level network and process faults:
  - `proc.pause`: Process suspension (SIGSTOP / SIGCONT).
  - `net.partition`: Bidirectional network isolation via packet filter rules.
  - `net.loss` / `net.delay`: Emulated degradation via `tc` (traffic control) and netem.
- **Fault-Injection Discipline:** Load drives across the perturbation window (DRIVE must span PERTURB).
- **Residue Checks:** After execution, a mandatory teardown sweep verifies zero lingering containers, networks, or routing tables.

### Tier 4: Dual-Arm Empirical Controls

Every live test run requires dual-arm verification:

- **Positive Control (Planted Defect Arm):** Target with a known architectural flaw (e.g., stale read on minority partition) must consistently yield exit 1 (FAIL).
- **Negative Control (Patched Arm):** Corrected target under the exact same fault sequence must consistently yield exit 0 (PASS).
- **Multi-Target Arm:** Verification against production software (e.g., upstream etcd) under linearizable (arm A -> PASS) and serializable (arm B -> FAIL) configurations.

## 3. Test Authoring Protocol: Failing-First & Mutation

No test may be committed without demonstrating that it catches the defect it was written for.

**The 4-Step Authoring Cycle**

```
  [1. Write Failing Test] --> Must fail with specific expected error
            |
            v
  [2. Implement Fix]      --> Full suite passes
            |
            v
  [3. Mutation Phase]     --> Revert fix / mutate condition; test MUST fail again
            |
            v
  [4. Document Evidence]  --> Log failure outputs in commit/test record
```

- **Step 1: Write the Failing Test:**
  - Write the test asserting the required invariant before touching production code.
  - Run the test and capture the exact failure output.
  - Confirm it failed for the correct reason, not a syntax or configuration issue.
- **Step 2: Implement Fix:**
  - Apply the minimal code change to satisfy the test.
  - Run the test and verify it passes.
- **Step 3: Mutation Verification:**
  - Revert or invert the code fix (e.g., change `>` to `>=`, delete an assignment, bypass a check).
  - Re-run the test. It must fail. If it passes, the test is invalid (e.g., a silent tautology or dead branch).
- **Step 4: Evidence Recording:**
  - Record the mutation failure output directly in the pull request, commit description, or decision ledger.

## 4. Host Discipline & Safety Rules

- **PowerShell Runner Requirement:**
  Tests on Windows hosts must run through `pwsh -File scripts/run-tests.ps1`, not bare `go test ./...`, to adhere to local Application Control policies, unstripped binary fallbacks, and directory constraints.
- **Disk & Artifact Management:**
  - Never generate uncontrolled build artifacts in temporary directories.
  - Disk space must be measured before and after large multi-agent or matrix runs.
  - Run directories (`.prothesis/runs/`) must never be purged without explicit instruction; retiring runs must be staged to `.prothesis/runs/.trash/`.
- **Daemon Safety & Seams:**
  External service calls (e.g., `docker inspect`) must be bounded behind defensive seams (e.g., 5-second hard timeouts) so an unresponsive daemon costs a run its provenance, not its budget.
- **Attestation Control:**
  All container build steps must enforce `BUILDX_NO_DEFAULT_ATTESTATIONS=1` to prevent image metadata drift and excessive cache bloat.

## 5. Statistical Validity & Trial Frames

- **Trial Definition:**
  The unit of statistical observation must be explicitly declared: Run-Level (entire end-to-end execution) vs World-Level (individual fault seed execution).
- **Population Stratification:**
  - Never pool runs across different architecture versions or lifecycle revisions.
  - Non-Bernoulli outcomes (INCONCLUSIVE, BUDGET_EXHAUSTED, CONFIG_ERROR) must be segregated from pass/fail rates. They reflect unobserved space, not system reliability.
- **Retention Floors:**
  - Retention policies must never drop passing worlds below the statistical threshold needed for confidence calculations.
  - File-level stripping (e.g., pruning `history.jsonl`) requires rewriting `MANIFEST.json` while preserving original root bundle hashes.

## 6. Pre-Publication Release Gate

Before making any repository or release public, the following automated gate must execute to completion with zero errors:

| Stage | Command | Criterion |
|---|---|---|
| Tree Cleanliness | `git status --porcelain` | Must return 0 lines (clean working tree). |
| Leak Audit | `git grep -I -i -c -E 'gmail\|<author_email>' HEAD` | Must return 0 matches across the entire tree. |
| Org Name Audit | `git grep -I -i -c '<private_org>' HEAD` | Must return 0 matches across the entire tree. |
| Punctuation Standard | Em dash audit script | Zero unapproved em dashes in prose or markdown. |
| Local Test Suite | `pwsh -File scripts/run-tests.ps1` | 32 packages ok, 0 not ok, 38 total. |
| Build Determinism | `pwsh -File scripts/build.ps1 -Verify` | Two independent builds produce identical SHA-256 digests. |
| Oracle Lock | `thesis.exe oracles verify` | oracle lock OK. |
| Hosted Linux CI | GitHub Actions matrix (go stable + go 1.22.x + live worlds) | All jobs green; all 5 exit codes verified. |
| Branch Protection | GitHub API branch protection | master has admin enforcement, no force-push, no deletion. |
| Repository Visibility | `gh repo view --json isPrivate` | Confirmed PRIVATE until human user authorization. |

This protocol is normative. Any deviation requires an explicit, append-only entry in `DECISIONS.md` citing rationale, alternatives, and trade-offs.
