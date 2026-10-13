# Arbitration: Phase 0 of the hardening plan, at 081d667

**Reviewer:** Kimi Code CLI (this session did not build any of the work reviewed).
**Reviewed:** commits 4aaa749, 51584be, 1c7e126, 2508ccb, 081d667 on `master`; the
uncommitted candidate changes in the author's working tree; Appendix A of
`docs/design/10-hardening.plan.md`.
**Method:** read-only inspection of the author's tree; all test execution in a separate
worktree (`../pro-thesis-arb`, branch `arb/phase0-review`, cut from master, the candidates
applied on top). Nothing in the author's working tree was modified.

## Verdict in one paragraph

Phase 0's five commits conform to the doctrine: one item each, measured claims with commands,
riding documentation in the same change, trailers present, ledger ids resolve. The wrapper's
skip counting works on this host (measured below). **One premise of the review request is
false: no session has overturned D-037.** Nothing at 081d667 - ledger, code, KNOWN-ISSUES,
README, or any commit on any branch - records a ruling that an absent lock must block
`thesis run`; D-099 explicitly leaves that ruling to the author. The proposed ruling is
assessed on its merits in section 2. One uncommitted change (the telemetry reorder) is
unaccounted for by any ledger entry or by ruling 10's review list and must not be committed
as part of the OQ-077 candidate.

## 1. The five Phase 0 commits

| Commit | Item | One item? | Evidence checked |
|---|---|---|---|
| 4aaa749 | Adopt plan, D-097 | Yes (plan + riding AGENTS.md/README) | `git show --stat 4aaa749`; AGENTS.md section 7 worktree text and id counter moves read in the diff |
| 51584be | OQ-085 | Yes | `git show --stat 51584be`; OQ-085 read (`OPEN_QUESTIONS.md:3781`); both evidence bundles exist on disk (`Test-Path` → True for `r_2026_10_12_5bb0`, `r_2026_10_12_332c`) |
| 1c7e126 | E1 gofmt | Yes | **`git diff -w 1c7e126^ 1c7e126` is empty (0 lines), measured this session** - the "whitespace only" claim is true; `gofmt -l cmd internal pkg targets testdata` at HEAD prints nothing |
| 2508ccb | E2 wrapper counts, D-098 | Yes | `scripts/run-tests.ps1` read in full; counting logic inspected (lines 197–209); live demonstration below |
| 081d667 | G1 gaming benchmark, D-099 | Yes | `scripts/gaming-benchmark.ps1` read in full; `docs/observations/2026-10-12-GAMING-BASELINE/benchmark.json` exists and its attack grouping is REFUSED 9, HELD 3, FALSE PASS 3, NOT MEASURED 3 - matching D-099's recorded summary exactly |

Cross-cutting checks, measured this session on this host:

- `scripts/emdash-audit.ps1`: "em dash audit OK (44 markdown files scanned, 9 grandfathered)" - matches the claims in all five commit messages.
- `scripts/run-tests.ps1 -Match internal/ledger` at HEAD: "TESTS: 7 pass, 0 fail, 0 skip", exit 0 - the citation test passes; every D/OQ id cited in these commits resolves.
- All five commits carry the `Co-Authored-By` trailer and cite their ledger ids in the subject.
- `thesis diagnose` in `testdata/kvfixture`: exit 0, "every non-terminal outcome attributed to a known problem" - D-099's retention claim (nineteen benchmark bundles attributable) is consistent with the live corpus (230 bundles on disk).
- `thesis oracles verify` in the fixture: exit 0 (with the expected executables-moved warning for rebuilt binaries, the same warning OQ-085 records).

**Wrapper skip counting on this host (explicitly requested check).** Accurate. In the review
worktree, `-Match internal/control` printed "pass 115, fail 0, skip 2" and named both skips
(`TestFixtureDrainReachesPassOnACleanWorld`, `TestFixtureParallelWorldsAreIsolatedAndTornDown`
- the Docker-backed pair, correctly skipped with `PROTHESIS_DOCKER_TESTS` unset). The mutation
runs below additionally exercised the fail-counting path. No inaccuracy found.

**Failing-first evidence.** D-098 records before/after/mutation on a throwaway all-skip
package; D-099 records two full benchmark passes with run ids; the docs commits (4aaa749,
51584be) record the applicable gates (em dash, ledger test, gofmt). Compliant with plan rule 2
as far as the ledger text goes; this session independently mutation-tested the uncommitted
candidates (section 3).

## 2. The claimed D-037 overturn

**The premise is not supported by the repository.** At 081d667:

- D-037 stands unamended (`DECISIONS.md:1011`): "an ABSENT lock does not block a run, but it is
  never a `verify` pass". No later entry exists; nothing supersedes it.
- D-099 (`DECISIONS.md:3885-3891`) frames the B7b/D-037 conflict and says: "Whether `thesis
  run` should refuse a missing lock … is the author's ruling. Until it is made, the benchmark
  scores B7b against Appendix B."
- `docs/KNOWN-ISSUES.md:56` (added by 081d667): "Whether `run` should refuse too awaits the
  author's ruling."
- The code implements D-037 as written: `internal/lock/check.go:157-162` (absent → warn,
  `LockAbsent`, run proceeds) and `:204-210` (only mismatch blocks).
- `git log --all` shows no commit after 081d667 on any branch.

If a session made this ruling, it was never recorded, and an unrecorded ruling does not exist
for this project. The author should either record it as a new D entry superseding D-037 or
treat the question as still open.

**On the merits, the proposed ruling is directionally sound but underspecified.** For: B7b
measured the exploit (lock deleted + workload weakened → exit 0 on the planted defect in one
of two passes, D-099); the verdict's `oracle_lock.status: "absent"` is a record an agent
acting on exit 0 never reads; and "a tool that cannot answer must say so, loudly" argues that
a gate with no baseline cannot answer the drift question at all. Against, and needing
resolution before adoption:

1. **First contact.** D-037's rationale stands: a project that has never been locked cannot be
   told apart from one whose lock was deleted. A uniform refuse-on-absent makes the first run
   of any project exit 4 until a human authors `thesis oracles lock --reason`. That may be the
   right onboarding contract for a gate (the lock is human-authored by design), but the ruling
   must say so explicitly, and the README's first-five-minutes flow changes with it.
2. **Exit-code coherence.** Today a corrupt lock is exit 2 and absent would become exit 4:
   the worse tampering signal would carry the softer code. An overturn should re-tier these,
   not just flip one.
3. **Riding documentation.** KNOWN-ISSUES, README ("A deleted lock does not stop `thesis
   run`"), D-099's open question and B7b's note in `scripts/gaming-benchmark.ps1:279-283` all
   state the current rule and must move in the same change as the code.

## 3. The uncommitted candidate fixes

The review request describes candidates for OQ-077, OQ-080 and OQ-082. The tree holds
something different (`git status --short`): `internal/control/drain.go`,
`internal/control/runner.go`, `internal/telemetry/collector.go`,
`internal/telemetry/collector_test.go` modified, `internal/control/driver_outcome_test.go`
new. **The OQ-080 and OQ-082 candidates are not uncommitted - they are commits 15358c0 and
7cda379 already on master**, awaiting review under plan items R1/R2 (Phase 2 of the sequence,
not Phase 0). The tree holds the OQ-077 work plus a telemetry change.

### 3a. OQ-077: fold the driver's outcome into the world (drain.go, runner.go, driver_outcome_test.go)

`driverOutcomeRefusal` (`drain.go:178`) refuses a world whose driver never started, did not
drain, or exited non-zero of its own accord; `runner.go:1187-1191` folds that in with
`Worse(OutcomeInconclusive)` after drain and stop, before ASSERT.

Consequence-aware assessment - passes, with two flags:

- **Direction is fail-closed and correctly ranked.** `Worse`, not assignment: an oracle-proven
  violation (rank 5) still outranks the inconclusive (rank 3) (`outcome.go:101-128`). The
  comment at `runner.go:1181-1186` honestly states the unsettled half (whether a cut history
  can also manufacture a FAIL).
- **The D-096 failure mode is not repeated.** The reverted `8c76a57` would have made every
  etcd world inconclusive. This candidate does not: `targets/etcd/cmd/etcdload` honours the
  stdin drain (`main.go:125,216-217`), and `defaultDriverHandle.Drain` reports a driver that
  already exited on its own as `DrainCompleted` (`runner.go:1580-1587`), so a driver that
  finishes its plan and exits 0 - drained or channel-less - leaves PASS standing. Only a
  driver still running at QUIESCE without a control channel is refused, which is precisely the
  ambiguous case OQ-077 names.
- **Mutation-verified this session** (in the review worktree): with the drain.go/runner.go
  changes reverted, `TestADriverThatExitedNonZeroOnItsOwnMakesTheWorldInconclusive` and
  `TestADriverThatRefusedToDrainMakesTheWorldInconclusive` both FAIL (outcome "pass", exit 0
  with the defect present) while the control
  `TestADriverThatDrainedAndExitedZeroLeavesThePassStanding` PASSES - the tests see the
  defect and are not vacuous. Restored, `-Match internal/control`: 115 pass, 0 fail, 2 skip,
  exit 0.
- **FLAG 3a-1: test names breach plan rule 5.** The plan fixes H2's names
  (`TestADriverThatDrainedAndExitedCleanlyLeavesPassStanding`,
  `TestADriverThatExitedNonZeroMakesTheWorldInconclusive`,
  `TestADriverThatMissedTheDrainDeadlineMakesTheWorldInconclusive`); the candidate's names
  differ ("...ExitedZero...", "...ExitedNonZeroOnItsOwn...", "...RefusedToDrain..."). The
  candidate predates the plan and ruling 10 orders its review rather than a rewrite, so this
  needs either a rename at commit time or the author's explicit approval of the divergence.
- **FLAG 3a-2: the refusal reason reaches stderr only.** The world goes inconclusive and the
  drain report is already in `result.json` (D-042), but nothing machine-readable ties the
  outcome to the driver. Plan item R3 (reason codes) will cover this; acceptable to land
  before R3, provided H2's ledger entry says so.

### 3b. The telemetry reorder (collector.go, collector_test.go)

`collectProbe`/`collectStatus` (HTTP to published ports) now run before the in-container
`ExecScript` read, so a slow `docker exec` cannot starve the probe and queue-depth samples of
the round's deadline.

- **The tradeoff is real and only half-recorded.** What was starved before (probe/status) is
  now served; what is now last (cgroup memory/CPU/tasks) may starve instead on a slow-exec
  host. That direction is defensible - the starved metrics are recorded absent with named
  reasons (`markContainerMetricsAbsent`), while a starved probe silently weakened
  `resource_return_to_baseline` and `no_unbounded_queue`, the two refusals (KP-001, KP-002)
  D-099 shows are the only thing holding four gaming attacks. But the reverse starvation is a
  consequence the change's comment does not name.
- **Mutation-verified this session:** with the reorder reverted,
  `TestProbeAndStatusCollectedEvenWhenExecScriptTimesOut` FAILS ("probe was not collected …
  context deadline exceeded"); restored, `-Match internal/telemetry`: 103 pass, 0 fail, 0
  skip, exit 0.
- **FLAG 3b-1: unaccounted change.** Ruling 10 names the review population: `15358c0`,
  `7f5ee3c`, `7cda379`, and "the uncommitted OQ-077 work". The telemetry reorder is none of
  these; no OQ entry records the starvation it fixes, and no ledger entry cites it. Under plan
  rule 1 (one item, one change, one ledger entry) it must not ride into an OQ-077 commit. It
  needs its own OQ entry (with the measurement that motivated it - none is cited) and its own
  commit, or it must be reverted from the working tree.

## 4. Persona instantiation readiness for Phase 1 (H1)

Appendix A of the plan (`docs/design/10-hardening.plan.md:338-361`), approved by D-097 ruling
7, is the only instantiation in the repository. Assessment against H1 ("persona: required"):

**Sound as far as it goes.** USER is concrete (an on-call engineer deciding on a green check
at 1 a.m., with the cost of a false PASS spelled out), VETO names auditable criteria that map
directly onto H1's subject (SOC 2 CC8.1 change authorisation and segregation of duties are
the lock's reason to exist; NIST PW.8 covers testing executable code), BUILDER states the
invariant H1 defends ("exit 0 only for a test that ran in full, as locked, under the faults
the gate specifies"), and ENGINEER's scope matches H1's blast radius (lifecycle runner, lock,
verdict schema, checker contract).

**Three gaps to close before the first cycle:**

1. **The protocol is not in the room.** AGENTS.md Global Rules section 8 keeps the
   Adversarial Persona Loop protocol outside this repository and orders the session to stop if
   it is unavailable. Appendix A instantiates roles, not the loop: who speaks when, how a
   cycle terminates, and where its outcome is recorded are nowhere defined in-repo. Plan item
   P4 implies `docs/reviews/`; Appendix A should say so. The H1 building session must confirm
   it has the external protocol before writing code, or stop per the rule.
2. **No persona argues the attacker's side.** H1 exists because an agent told "make the gate
   pass" deletes one argument (OQ-085). USER is a victim and VETO an auditor; neither asks
   "how do I still get a false PASS after this lands". Appendix B plays that role mechanically
   for the finished item, but the spec review needs it argued. Recommend assigning that
   question to VETO explicitly, with Appendix B's catalogue as its evidence base.
3. **No adversary-grade challenge to H1's own narrowing rule.** Ruling 1 makes an argv
   schedule that differs from the profile "narrowed" under D-059. A persona should probe the
   seams: a profile whose `faults` list is empty (is the gate then a no-fault gate by
   lock?), and the re-lock of both committed locks that H1 requires.

Verdict: sufficient to govern H1 once gap 1 is confirmed closed (protocol available) and gaps
2–3 are assigned in the H1 spec. Do not amend Appendix A silently; a replacement or amendment
is the author's ruling under the same rule that approved it.

## Flags, collected

1. **No D-037 overturn exists in the repository.** Section 2. The author must record the
   ruling or treat the question as open.
2. **The telemetry reorder is unaccounted** - no OQ entry, no ledger entry, outside ruling
   10's review population. Section 3b.
3. **The OQ-077 candidate's test names diverge from the plan's fixed H2 names** (rule 5).
   Section 3a.
4. The review request's premise that OQ-080/OQ-082 candidates are uncommitted is wrong: they
   are commits on master awaiting Phase 2 items R1/R2. Section 3.

## What was not verified

- The whole suite was not re-run end to end (273 s at D-098's measurement); the packages
  touched by the candidates, the ledger test, gofmt, the em dash audit, `oracles verify` and
  `diagnose` were all run this session and are reported above with their outputs.
- D-099's live benchmark was not re-executed (a 888 s run against Docker); its recorded output
  was checked for internal consistency against the ledger instead.
- This record was written uncommitted in the arbitration worktree and merged to `master` with the
  author's instruction on 2026-10-12, in the change that remediated the documentation drift.
