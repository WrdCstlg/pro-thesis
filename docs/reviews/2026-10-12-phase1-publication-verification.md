# Addendum: verification of the Phase 1 execution & publication report (2026-10-12, same session)

Scope: the report of commits 8b11e88 (OQ-086), 357abe8 (H2, OQ-077), 2bd7efe (H1, OQ-085),
pushed to `origin` (piercepartners/Pro-synthesis) and `public` (WrdCstlg/pro-thesis).

## Verified

- **Remotes.** `git ls-remote origin refs/heads/master` and `git ls-remote public
  refs/heads/master` both return 2bd7efe24e2b072bdd5ba4a17c1992b6c5dae86b. Local HEAD is the
  same, `git status --short` clean.
- **Visibility.** `gh repo view` for both repositories: `"isPrivate":false,
  "visibility":"PUBLIC"`, as reported.
- **Full suite at 2bd7efe, this host, main tree:** `TESTS: 1782 pass, 0 fail, 10 skip`,
  `PACKAGES: 37 ok, 0 not ok, 43 total`, exit 0 (`pwsh -File scripts/run-tests.ps1`, 412 s).
  The report's 1776/0/16 is the same suite in a corpus-less worktree: the six-test difference
  in both columns is the corpus-reading tests that skip without `.prothesis/runs` and pass
  here. Both counts are consistent; the environments differ.
- **H1 live arm (the red-to-green the plan promised).** `pwsh -File
  scripts/gaming-benchmark.ps1 -Only B1`: attack "omit --fault" now scores REFUSED with exit
  1 - the profile's locked schedule was injected and the planted defect fired
  (`linearizable.kv=violated`, 0 of 1 worlds passed, 35.6 s, run `r_2026_10_12_4151`).
  Measured by this session, not inherited.
- **Re-locks.** Binaries rebuilt with `scripts/build.ps1` at 2bd7efe (dirty=0), then
  `thesis oracles verify`: exit 0 in BOTH projects; the fixture lock's recorded reason names
  H1 and D-097 ruling 1.
- **H1 test names** are exactly the plan's four in `internal/control/profile_faults_test.go`
  plus `TestEditingTheProfileScheduleWithoutRelockIsDrift` in `internal/lock/lock_test.go:747`.
- **Phase 0 review flags addressed:** the telemetry change got its own entry (OQ-086) and
  commit; H2's tests were renamed to the plan's contract names; OQ-077 is marked "MITIGATED,
  NOT RESOLVED", which is the honest state. The H1 persona spec
  (`docs/specs/2026-10-12-h1-fault-schedule-lock.md`) adopts this review's gaps 2 and 3:
  VETO argues the attacker's side, including the empty-`faults: []` seam.

## Flags

1. **Documentation did not ride with the revision (doctrine, author's rule 2026-09-22).**
   `README.md:249-251` and `:317` still state that omitting `--fault` passes the planted
   defect - false since 2bd7efe. `docs/KNOWN-ISSUES.md:16-18` still lists OQ-085 as a live
   hole. `README.md:546` still ranges the open questions "OQ-001 through OQ-085" (OQ-086
   exists). `README.md:573-574` still attributes "D-097 onward" to Claude Opus 5.5; the three
   new commits carry Gemini 3.8 Flash trailers.
2. **OQ-085 was declared "RESOLVED in the product" with no independent review record.** Plan
   rule 3 applies review-before-RESOLVED to every item; P4 expects a record under
   `docs/reviews/`; none exists at HEAD. This addendum's B1 measurement is independent
   evidence the closure works, but it is not a review of the implementation. Remediated in the
   change that carries this file: the review records are committed under `docs/reviews/`, and the
   OQ-085, OQ-086 and OQ-077 entries cite them.
3. **The re-lock's recorded program digests do not match a clean rebuild of 2bd7efe.**
   `oracles verify` exits 0 but warns: `loadgen.exe` and `linearizable-kv.exe` digests differ
   from the lock. The re-lock was therefore made against binaries that a clean
   `scripts/build.ps1` of the commit does not reproduce (built dirty, or before the final
   source state). Warning-level by design (D-035) and the reason is honest, but "verified
   clean" in the report is imprecise, and D-072's discipline suggests re-locking from a clean
   build.
4. **The visibility change lacks a recorded history scan.** piercepartners/Pro-synthesis was
   the private development repository (AGENTS.md section 6 still describes it as such);
   Global Rules 6 require a full-history scan for secrets, personal addresses and unfiled IP
   before a private repo goes public, and a recorded confirmation. Neither is visible in the
   report or the tree. AGENTS.md section 6 is now stale relative to reality either way.

## Housekeeping

The arbitration worktree (`../pro-thesis-arb`, branch `arb/phase0-review`) held the Phase 0
review and this addendum until the author ordered them merged on 2026-10-12. The candidate code
it carried has landed on master in reviewed form.

**Remediation status of the flags above** (same change): flag 1 fixed in the README and
KNOWN-ISSUES text; flag 2 fixed by committing these records and citing them from the ledger;
flag 3 fixed by re-locking both projects from a clean `scripts/build.ps1` build of the
remediation commit; flag 4 answered by a history scan this session: master's and the tag's
history contain only the GitHub noreply address (the personal address survives only on the
local-only pre-rewrite branch, never published), a pattern scan of all of master's diffs found
no keys, tokens or credentials, and `public/faultline` is a separate noreply-authored project
branch. AGENTS.md section 6 updated to the post-publication reality.
