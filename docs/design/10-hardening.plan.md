# 10-hardening.plan

Status: PLAN, 2026-10-12. Nothing here is implemented. It was drafted by the session that evaluated
the system on 2026-10-12 (the baseline below), for the author's ruling. Every item that changes
behaviour is decided by the author before it is built, built by one session, and reviewed by
another (AGENTS.md section 9, D-096). No item is done until its exit test has been seen to fail
first and then pass.

Adopted by the author on 2026-10-12. The rulings of section 3 and the persona instantiation of
Appendix A are recorded in D-097.

## 0. The goal, in one rule

The system is "very strong" on a dimension when the strongest claim its README makes about that
dimension is enforced by a test that runs on every push, and the test has been seen to fail
against a deliberately broken implementation. A claim that only a document makes is not strong,
however honest the document is.

## 1. Baseline (measured 2026-10-12, at `f75c196`)

| Dimension | Today | Evidence |
|---|---|---|
| Finding bugs | Strong (not in scope) | Run `r_2026_10_12_5bb0`: exit 1, witnessed violation on `k/0`, 462 operations, 1,532 states. |
| Earning a PASS | Weak | Run `r_2026_10_12_332c`: the planted-bug build, full `linear` profile, `--fault` omitted. Exit 0, PASS, 5 of 5 worlds, `oracle_lock.status: "ok"`, zero faults planned or realized in every world file. Also OQ-077, OQ-078, OQ-079. |
| Refusing to guess | Good design, open gaps | Fail-closed outcome fold (`internal/control/outcome.go`). Open: OQ-071, OQ-073, OQ-080, OQ-082, OQ-083. |
| The lock | Real but incomplete | Run profiles carry budget, worlds, driver profile and search only (`pkg/schema/config.go:385-405`). The fault schedule of `thesis run` comes from argv and nothing locks it. Also OQ-057, OQ-061, OQ-076, OQ-081. |
| Engineering | Solid | Suite `37 ok, 0 not ok, 43 total` at `f75c196`. `gofmt -l` lists two files on master. `scripts/run-tests.ps1` prints package results only, so no report can state test-level skip counts. |
| Evidence culture | Exceptional on paper, enforced socially | `8c76a57` was committed and pushed with resolutions D-096 later withdrew; three commits carry no trailer. CI runs only in the public repository (`.github/workflows/ci.yml:34`, `:122`), so nothing checked them. |
| Maturity | Early | Five of seventeen fault kinds ever realized; the guided search has never informed a selection (OQ-060); shrinking never completed end to end (OQ-052, OQ-063); `bisect` never run live. README figures, inherited. |
| Differentiator | Applied engineering | The checker (Wing and Gong with Lowe's optimisations, per key) and the fault injection are standard. The agent-facing contract is the distinctive part, and run `r_2026_10_12_332c` breaks it. |

## 2. Rules for executing this plan

1. **One item, one change, one ledger entry.** No commit spans two items. `8c76a57` is the
   counterexample: seventeen files, five entries, no evidence.
2. **Failing-first and mutation, pasted.** The builder records the failing test output, the
   passing output, and the output with the fix reverted, in the item's ledger entry.
3. **Independent review before RESOLVED.** A session that did not build the item reads the test,
   the build and the results, and records its verdict. D-096 already requires this for OQ-077 to
   OQ-083; this plan applies it to every item.
4. **Persona cycle for new behaviour.** Items marked "persona: required" go through the author's
   Adversarial Persona Loop before code (AGENTS.md Global Rules section 8). The instantiation in
   Appendix A must be approved first. Bug fixes, refactors, tests and docs are exempt.
5. **Acceptance and refusal tests are fixed here.** The test names below are the contract. Changing
   one after implementation starts needs the author's approval.
6. **One worktree and one branch per agent session.** Two sessions shared one working tree on
   2026-10-10 and their changes interleaved. Each session works in its own `git worktree`, and
   only the author merges.
7. **No unallocated ledger ids in prose.** The citation test (`internal/ledger`) fails on them.
   New entries are written in the same change as their heading.

## 3. Rulings the author makes first

These are decided before any code, because each one fixes a parameter that later items depend on
(the same reason AGENTS.md section 5 orders L3 before L1b).

| # | Question | Options | Recommendation |
|---|---|---|---|
| 1 | Where does a gate's fault schedule live? | (a) In the run profile, lock-covered; any argv schedule that differs, including none, narrows the run. (b) A profile minimum of realized faults. (c) Leave it in argv. | (a). It closes run `r_2026_10_12_332c` by construction and reuses D-059's narrowing rule. |
| 2 | How does a target declare that its history ends with a completeness record? | (a) A lock-covered `driver` key the target opts into. (b) Mandatory for all drivers. | (a). (b) breaks every existing target, which is what `8c76a57` did to etcd. |
| 3 | How does the verdict grow the fields that say what a PASS checked? | (a) Verdict schema v2, with v1 still readable. (b) Additive nullable fields under v1. | (a). A reader must tell "zero faults" from "not recorded"; a version says so outright. |
| 4 | How does a changed checker or driver program become exit 4 without every rebuild becoming an incident? | (a) Lock the program's source closure (its `go list -deps` files, `go.mod`, `go.sum`, toolchain) and check a binary's stamp against it. (b) Per-platform binary digests, re-locked per release. (c) Keep the warning. | (a). The build stamp moves the binary digest on every commit (D-072), so (b) means a re-lock per commit. |
| 5 | Where does CI run for the private repository? | (a) A self-hosted runner on the build host, which has Docker. (b) GitHub-hosted runners, billed. (c) Nowhere, as today. | (a) for the live job, (b) for unit and lint if the cost is acceptable. Not (c): it is how `8c76a57` went unchecked. |
| 6 | How do changes reach `master` on `origin`? | (a) Protected branch: pull request, the author's approval, required checks. (b) Direct pushes, as today. | (a). It makes "a session does not arbitrate its own work" mechanical. |
| 7 | Approve the persona instantiation in Appendix A? | Approve, amend, or replace. | Required before the first persona cycle. |
| 8 | Which third target? | Any system with a documented consistency claim and an unmodified image. | A system whose correct answer under a partition is PASS, so the target does not merely repeat etcd's FAIL arm. |
| 9 | What is a trial, for confirmation and for retention? | Per world (490 trials) or per run (74), in the snapshot AGENTS.md section 5 records. | Per world, excluding INCONCLUSIVE and BUDGET_EXHAUSTED. This also decides the shrink gate's statistic (OQ-054). |
| 10 | What happens to the pending candidate fixes? | `15358c0` (OQ-080), `7f5ee3c` (OQ-081), `7cda379` (OQ-082), and the uncommitted OQ-077 work in the author's working tree. | Review each under rule 3. `7cda379` has known defects (section 5, R2) and should be fixed, not accepted. |

## 4. Workstream H: Earning a PASS and the lock

**Very strong when:** every attack in Appendix B that aims at a false PASS returns a non-zero exit
on every push, the verdict states how many faults were realized and how many operations each
oracle judged, and OQ-076, OQ-077, OQ-078, OQ-079, OQ-057 and OQ-081 are RESOLVED with independent
review.

### H1. Put the fault schedule under the lock (new gap; persona: required)

- **Problem.** Run `r_2026_10_12_332c`: exit 0 on the planted bug by omitting `--fault`. The lock
  covers the profile, and the profile has no fault field. To be recorded as a new OPEN_QUESTIONS
  entry (the next free id) before work starts, with that run as its evidence.
- **Change (ruling 1a).** `profiles.<name>.faults` holds the gate's schedule. `thesis run
  --profile X` injects it. An argv `--fault` set that differs from it, or a new `--no-faults`
  flag, marks the budget narrowed (D-059), so the run cannot report PASS. CI and the README call
  the profile without `--fault`. Both committed locks are re-locked with a reason that names the
  new key.
- **Acceptance.** `TestRunInjectsTheProfileFaultSchedule`; `TestAnArgvScheduleEqualToTheProfileIsNotNarrowed`.
- **Refusal.** `TestAnArgvScheduleThatDiffersFromTheProfileCannotPass`;
  `TestNoFaultsOnAProfileWithFaultsCannotPass`; `TestEditingTheProfileScheduleWithoutRelockIsDrift`.
- **Live.** Appendix B attacks B1 and B2.

### H2. Fold the driver's outcome into the world (OQ-077, first half; bug fix)

- **Problem.** No code reads the driver's result when scoring a world (OQ-077).
- **Care.** The fixture's driver is *meant* to be stopped at QUIESCE (`testdata/kvfixture/prothesis.yaml`,
  the comment on `linear`). Being stopped is normal; failing to drain by the deadline, or exiting
  with a code the contract does not list as a clean stop, is not. The contract must name the clean
  stop codes before the gate can tell them apart.
- **Change.** Score as INCONCLUSIVE, with a reason, a world whose driver exited with an unlisted
  code, crashed, or did not drain. Candidate work exists, uncommitted, in the author's working tree
  (`internal/control/drain.go`, `runner.go`, `driver_outcome_test.go`); review it under rule 3
  rather than rewrite it.
- **Acceptance.** `TestADriverThatDrainedAndExitedCleanlyLeavesPassStanding` (the control).
- **Refusal.** `TestADriverThatExitedNonZeroMakesTheWorldInconclusive`;
  `TestADriverThatMissedTheDrainDeadlineMakesTheWorldInconclusive`.

### H3. Make history truncation detectable, by declaration (OQ-077, second half; persona: required)

- **Problem.** A history cut on a record boundary is judged as complete. `8c76a57` tried a
  mandatory end record and broke every target that does not write one (D-096).
- **Change (ruling 2a).** A lock-covered driver key declares that the history ends with a
  completeness marker carrying a record count. The check runs once, on the driver's raw file,
  before phase markers are merged; the merge keeps the marker last. `historycheck` validates it.
  Both reference drivers (fixture `loadgen`, `targets/etcd/cmd/etcdload`) implement it;
  `docs/TARGETS.md` documents it. An undeclared target is unchanged, and its verdict records
  completeness as unchecked.
- **Acceptance.** `TestADeclaredCompleteHistoryPasses`; `TestAnUndeclaredTargetIsUnaffected`;
  `TestMergeKeepsTheCompletenessMarkerLastAndIsIdempotent`.
- **Refusal.** `TestADeclaredHistoryWithoutItsMarkerIsInconclusive`;
  `TestADeclaredHistoryWhoseCountDisagreesIsInconclusive`;
  `TestACompletenessMarkerCarryingAnOpIDIsRejected`.
- **Live.** The etcd PASS arms stay exit 0 in CI; attack B11.

### H4. Require that the load overlapped every realized fault (OQ-078, first half; persona: required)

- **Change.** For each realized fault window, at least one operation must be in flight during it
  (invoked before the window ends, and completed after it starts or never completed). Otherwise
  the world is INCONCLUSIVE with a reason naming the fault. Computed by a sorted sweep, not by map
  iteration. Also check, and add if missing, that a planned fault that never realized cannot leave
  the world at PASS.
- **Acceptance.** `TestOverlappingLoadLeavesPassStanding`. Restore the PASS expectation of
  `TestPerturbRunsARealScheduleAndRecordsIt` by giving its stub history operations that overlap the
  window; `8c76a57` changed that expectation instead.
- **Refusal.** `TestAFaultWindowAfterTheLoadIsInconclusive`; `TestAFaultWindowBeforeTheLoadIsInconclusive`;
  `TestOneFaultOutsideTheLoadIsEnoughToBeInconclusive`; `TestAPlannedFaultThatNeverRealizedCannotPass`.
- **Live.** Attack B2.

### H5. Make the verdict say what a PASS checked (OQ-078, second half; persona: required)

- **Change (ruling 3a).** Verdict schema v2 adds, per run and per world: faults planned and
  realized, operations recorded, operations judged per oracle, the oracles evaluated with their
  status, history completeness, and the overlap result. A v1 verdict still decodes, with every new
  field null, meaning not recorded. `thesis diagnose` and the corpus readers handle both versions.
- **Acceptance.** `TestVerdictV2GoldenRoundTrips`; `TestAVerdictV1DecodesWithNewFieldsNull`.
- **Refusal.** `TestAV2VerdictMissingAFieldIsInvalid`;
  `TestAV2PassWithZeroRealizedFaultsOnAFaultedProfileIsInvalid`.

### H6. Close the checker's initial-state gap (OQ-079; bug fix)

- **Change.** The oracle input carries the initial register state: empty when the harness can show
  it booted fresh volumes, unknown otherwise. The checker uses it. Separately, run the history
  validator before the oracle, so a missing `t_ns` is INCONCLUSIVE instead of loading as 0. The
  checker's source moves, so re-lock with a reason that says so (D-072).
- **Acceptance.** `TestAMidStreamHistoryWithUnknownInitialStateIsNotAccused`.
- **Refusal.** `TestAFirstReadOfAFailedWritesValueIsAViolationWhenTheStateIsEmpty`;
  `TestAMissingTimestampIsInconclusiveNotZero`;
  `TestEmptyInitialStateIsClaimedOnlyWhenFreshVolumesAreProven`.

### H7. Lock the programs and the workload (OQ-057, OQ-061, OQ-076; persona: required)

- **Change (ruling 4a).** The lock records the source closure of the checker and the driver. A
  binary whose embedded stamp does not match the locked closure is exit 4 under a new strict mode,
  which CI uses. Separately, the driver writes its effective workload (clients, operations, keys,
  mix, and any environment override) into the history as a marker, and the harness compares it
  with the locked driver profile; a mismatch is drift. The harness passes the driver an allowlisted
  environment.
- **Acceptance.** `TestARebuildFromUnchangedSourceIsNotDrift`.
- **Refusal.** `TestAnAlwaysOkCheckerIsDriftUnderStrictMode` (the A7 swap from
  `scripts/adversarial.ps1`, now exit 4); `TestAWorkloadSmallerThanTheProfileIsDrift`;
  `TestAWorkloadEnvironmentOverrideIsDrift`.
- **Live.** Attacks B8, B9, B10.

### H8. Treat a strategy override as narrowing (OQ-081; bug fix)

- **Change.** Review candidate `7f5ee3c` under rule 3. Its test checks a flag; add one that checks
  the outcome.
- **Refusal.** `TestASearchWithAnOverriddenStrategyCannotReportPass`, asserting the verdict and the
  exit code end to end.

## 5. Workstream R: Refusing to guess

**Very strong when:** every refusal condition the README names is forced by a test in one matrix,
every INCONCLUSIVE carries a machine-readable reason, and no failure of the harness itself
leaves a run directory without a verdict that says why.

### R1. A verdict that cannot be written is exit 2 (OQ-080; bug fix)

Review candidate `15358c0` under rule 3. Run gofmt on its test (it fails `gofmt -l` on master).
**Refusal:** `TestWriteVerdictFailureReturnsInconclusiveAndError` (exists).

### R2. A panic produces a verdict, wherever it happens (OQ-082; bug fix)

- **Known defects in candidate `7cda379`** (found by the 2026-10-10 review): it writes schema
  `prothesis/v1` instead of the verdict schema, so `schema.UnmarshalVerdict` rejects the file; it
  writes to the working directory, not the run directory; it recovers only on the main goroutine;
  it adds a hidden `__panic` verb to the release binary; its test decodes with plain JSON and so
  cannot see the schema error.
- **Change.** Recover in `main` and in every goroutine that runs harness code (world workers, the
  oracle engine), turning a panic into a harness error with a reason. Write the verdict into the
  run directory once one exists; before that, write nothing and say so on stderr. Replace the
  hidden verb with a test-only seam.
- **Refusal.** `TestAPanicInMainWritesADecodableVerdictInTheRunDirectory`;
  `TestAPanicInAWorldWorkerMakesTheRunInconclusiveWithAReason`;
  `TestTheReleaseBinaryHasNoHiddenPanicVerb`.

### R3. Every INCONCLUSIVE carries a reason (OQ-071; persona: required)

Add a closed set of reason codes to the v2 verdict and to `result.json`, including for a world that
dies before ASSERT. Codes map to the known-problem registry (D-082), so attribution happens when
the verdict is written, not after.
**Refusal:** `TestEveryInconclusivePathWritesAReasonCode` (table-driven over the matrix in R6);
`TestAWorldThatDiesBeforeAssertStillWritesAResult`.

### R4. Persist what the programs said (OQ-073; bug fix)

Record each world's driver and oracle exit codes and their raw output, bounded in size, with an
explicit truncation marker.
**Refusal:** `TestTruncatedProgramOutputSaysItWasTruncated`.

### R5. One program resolver (OQ-083; bug fix)

The harness, the lock and `thesis doctor` share one resolver that takes the target OS as input
(container parity, D-088).
**Acceptance:** `TestTheResolverAgreesAcrossHarnessLockAndDoctor`, over every combination of
{no file, plain, `.exe`, both} and {windows, linux}.

### R6. The refusal matrix

One table-driven test in `internal/control` enumerates every refusal condition in the README's
"Four answers" table and in this plan, each with its exit code and reason code. A documentation
test fails if the README table names a condition the matrix lacks. A live subset runs in
`scripts/adversarial.ps1`.

## 6. Workstream G: Make the differentiator measurable

**Very strong when:** the README reports "N of N known attacks refused" with the CI run that
measured it, and every attack in Appendix B is in that N.

- **G1. The gaming benchmark.** Extend `scripts/adversarial.ps1` (probes A1 to A7 today) into the
  catalogue in Appendix B. Each attack has its expected exit code fixed here. It runs in the live
  CI job. Build it first, in Phase 0: the attacks that succeed today are the plan's failing tests
  at system level, and they turn green one item at a time.
- **G2. Planted defects.** The design in `docs/design/09-adversary-loop.design.md`: several
  defects, each a build variant of the fixture with a pre-registered fault and oracle. The measure
  is a per-defect detection rate with a lower bound, using ruling 9's unit of trial.

## 7. Workstream E: Engineering

**Very strong when:** every commit on both repositories passes build, vet, gofmt, the suite and
the em dash audit in CI; reports can state test-level counts; and an outside reader can follow the
code without the protocol documents open.

- **E1. Master is green.** Land the gofmt fixes for `cmd/thesis/panic_test.go` and
  `internal/control/verdict_write_test.go` (present, unstaged, in the author's working tree).
- **E2. Test-level counts.** `scripts/run-tests.ps1` reads `go test -json` and prints pass, fail
  and skip per package and in total. A package whose tests all skipped is flagged.
- **E3. Readability.** A glossary of the protocol vocabulary (directive sections, invariants I1 to
  I7, lifecycle phases, the lettered rulings in the phase briefs), linked from
  `docs/ARCHITECTURE.md`. A sweep of rationale that went stale when later phases landed, for
  example `internal/control/outcome.go:133-161`, which says exit 4 cannot be emitted because the
  lock is a Phase 3 deliverable, while `cmd/thesis/run.go` and `internal/lock/check.go` emit it.
  Behaviour does not change; the reviewer checks that.
- **E4. Split `internal/control/runner.go`** (about 1,800 lines, measured 2026-10-09) along the
  lifecycle phases, behind the seams it already has. Proven by the suite and by the CI arms
  returning the same exit codes.
- **E5. Reproducible builds in CI.** Build twice on Linux and compare digests, as `scripts/build.ps1
  -Verify` does on the build host.
- **E6. No hand-maintained counts in docs.** AGENTS.md still says "32 packages"; the wrapper reports
  43. Remove such numbers or have a test check them.

## 8. Workstream P: Evidence culture, enforced by machines

**Very strong when:** no change reaches `master` on either repository without passing CI and an
approval by someone other than its builder, and the ledger rules that today depend on goodwill fail
the suite when broken.

- **P1. Worktree per session** (rule 6), written into AGENTS.md section 7.
- **P2. Protected `master` on `origin`** (ruling 6). The author changes the repository setting.
- **P3. CI for the private repository** (ruling 5).
- **P4. Ledger lint** in `internal/ledger`: every D entry and every follow-up carries an
  `**Attribution.**` line; every RESOLVED follow-up cites a review record (a file under
  `docs/reviews/` naming the reviewed commit, the failing-first and mutation output, and the
  verdict).
  **Refusal:** `TestAResolvedFollowUpWithoutAReviewRecordFails`;
  `TestAnEntryWithoutAttributionFails`.
- **P5. Commit lint in CI**: every commit in a pushed range carries a `Co-Authored-By` trailer
  (AGENTS.md section 6), and a commit that touches code cites a ledger id. D-096 withholds model
  names in prose by the author's instruction; the trailer rule is unchanged unless the author
  rules otherwise.

## 9. Workstream M: Maturity

**Very strong when:** every capability the README advertises is either proven by a recorded,
reproducible run or labelled experimental where it is advertised, and the evidence covers three
independent targets.

- **M1. A capability register.** One table: capability, the run or CI job that proves it, or
  "experimental". The README's "What it does" is checked against it (as in R6).
- **M2. Fault-kind coverage.** Each of the seventeen kinds realized in a recorded world with a
  pre-registered expectation, or scoped out with a ledger reason (for example the platform limits
  in OQ-021). Runs in the live CI job on Linux.
- **M3. A third target** (ruling 8), with predictions written before the first run and its arms
  asserted in CI.
- **M4. `thesis bisect`, live.** A fixture branch whose history introduces a planted defect at a
  known commit; a recorded bisect finds it.
- **M5. Shrinking, end to end** (OQ-052, OQ-053, OQ-054). Replace the k-of-k gate with a
  rate-bounded confirmation using ruling 9's trial; carry one shrunk schedule through the gate on
  the fixture and on etcd.
- **M6. The guided search** (OQ-060). Run the pre-registered test D-015 implies, with a budget
  beyond the escalation ladder. If the tree does not earn its place, the README already describes
  the honest fallback; make it the documented scope.
- **M7. The rest of the open list.** Retention in AGENTS.md section 5's order (OQ-070, after ruling
  9); image identity across a cold cache (OQ-068); the LLM strategy stays experimental until
  validated under an approved spend cap (OQ-069).

## 10. Sequence

| Phase | Items | Why here |
|---|---|---|
| 0. Decide and instrument | Rulings 1 to 10; Appendix A; record the unlocked fault schedule as a new entry; E1; E2; P1; G1 with today's results | Nothing else can be specified until the rulings exist, and G1 turns every later item into a visible red-to-green change. |
| 1. Earn the PASS | H1, H4, H5, H2, H3, H6, H7, H8 | Verification comes first (Global Rules section 7). H1 closes the measured hole; H4 and H5 make a PASS say what it checked. |
| 2. Refuse loudly | R2, R1, R3, R4, R5, R6 | R2 fixes known defects already on `origin`. |
| 3. Enforce the process | P2, P3, P4, P5 | Needs rulings 5 and 6. It can run in parallel with phases 1 and 2 if a separate session builds it. |
| 4. Engineering | E3, E4, E5, E6 | Refactoring after the behaviour it moves is pinned by phases 1 and 2. |
| 5. Maturity | M1, then M2 to M7; G2 | M1 first, so the README stops advertising anything unproven while the rest proceeds. |

## 11. The exit audit

The author checks each dimension's "very strong when" line against artifacts, not reports:

1. A green CI run on both repositories for the final commit, with the run ids.
2. The gaming benchmark's table: every attack in Appendix B refused, from that CI run.
3. `thesis diagnose` exit 0 in both projects.
4. The ledger lint passing, and a review record for every entry this plan resolved.
5. The capability register, with no advertised capability lacking a proof or an "experimental"
   label.

## Appendix A. Draft persona instantiation (for the author's approval)

**BUILDER.** Senan. Building PRO-THESIS, a fault-injection gate for distributed systems whose exit
code is acted on by people and by AI agents. Invariant: exit 0 is emitted only for a test that ran
in full, as locked, under the faults the gate specifies, and was judged by at least one oracle;
every other condition is a non-zero code with a named reason.

**USER.** Platform engineer on call at a company whose metadata service runs on a three-node
consensus store. At 1 a.m. an AI coding agent opened a pull request titled "fix flaky partition
test", and the CI check is green. They must decide before the morning release train whether to
merge it into the branch that ships to customers. If the check was green because the test stopped
testing, a stale-read bug ships, customer records diverge between regions, and the postmortem asks
why the gate said yes.

**VETO.** Internal audit lead responsible for the company's SOC 2 Type II report. Cares about:
change-management evidence under Trust Services Criteria CC8.1 (changes are authorised, tested and
approved before implementation); segregation of duties, so that the author of a change, human or
agent, cannot be its approver; ISO/IEC 27001:2022 Annex A 8.29 (security testing in development and
acceptance) and 8.32 (change management); NIST SP 800-218 practice PW.8 (test executable code);
integrity of test evidence after the fact; and the GNU AGPL v3.0 section 13 obligations if the
harness is offered to users over a network.

**ENGINEER.** The building session under AGENTS.md section 9, working to the author's rulings. Owns
the lifecycle runner, the lock, the verdict schema and the checker contract.

## Appendix B. The gaming catalogue

"Today" names its source. Only B1 was measured by the session that drafted this plan; the others
cite the README or the ledger, and "not measured" rows are measured by G1 in Phase 0.

| # | Attack | Expected after hardening | Today |
|---|---|---|---|
| B1 | Omit `--fault` on a profile whose gate needs faults | Exit 1 on the planted bug (the profile's faults apply) | Exit 0, run `r_2026_10_12_332c` |
| B2 | Schedule the fault after the load ends | Exit 2, with reason | Not measured |
| B3 | Fewer worlds than the profile (`--worlds`) | Exit 2 | Exit 2 (README, step 4) |
| B4 | Shorter budget than the profile (`--budget`) | Exit 2 | D-059; not re-measured |
| B5 | Change the driver profile in `prothesis.yaml` | Exit 4 | Exit 4 (README, step 3) |
| B6 | Widen a lock-covered budget, or remove a built-in oracle | Exit 4 | Exit 4 for the budget (probe A4) |
| B7 | Edit the lock file by hand, or delete it | Exit 4 or 5 | Not measured |
| B8 | Replace the checker with one that always says ok | Exit 4 (strict mode) | Exit 0 with a warning (OQ-057, probe A7) |
| B9 | Replace the driver binary with one that runs less load | Exit 4 (strict mode) | Warning only (D-090) |
| B10 | Shrink the workload through an environment variable | Exit 4 | Not covered (OQ-076) |
| B11 | Truncate the history on a record boundary (declared target) | Exit 2, with reason | Not detected (OQ-077) |
| B12 | Kill the driver early | Exit 2, with reason | Not folded in (OQ-077) |
| B13 | Override the search strategy from argv | Exit 2 on a clean run | Candidate fix `7f5ee3c`, unreviewed (OQ-081) |
| B14 | Panic the harness | Exit 2, verdict with reason in the run directory | Main goroutine only: exit 2, and a verdict in the working directory that `schema.UnmarshalVerdict` rejects (candidate `7cda379`, read from the code; OQ-082) |
| B15 | Make `verdict.json` unwritable | Exit 2 | Candidate fix `15358c0`, unreviewed (OQ-080) |
