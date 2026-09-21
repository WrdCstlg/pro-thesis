# OBS-LIVE-001: Response to the 2026-09-17 audit

Every audit claim was re-verified against code and artifacts before being
accepted or rejected. Result: the audit's core technical finding (C2 unsound)
is **correct and confirmed**; most of its secondary findings are correct; two
of its own claims are **wrong** and one is imprecise. Corrections to
`record.md` / `design.md` / `crosscheck.py` are applied in revision 2.

## Confirmed audit findings

| # | Audit claim | Verification |
|---|---|---|
| 1 | C2 (v1) unsound; flags concurrent in-flight writes | **Confirmed by measurement.** v1 flags 826 / 941 / 761 reads on world-0001 of known-PASS runs `r_2026_09_15_0d1c`, `r_2026_09_15_f7e8`, `r_2026_09_09_c2a7` respectively: histories the oracle accepted. A checker that convicts clean runs is worse than none. |
| 2 | True hard stale-read counts are 72 and 46 | **Confirmed exactly.** Under the sound criterion (v3: `W_B.complete < W_A.invoke` AND `W_A.complete < R.invoke`), run 1 = 72, run 2 = 46, and **0** on all three PASS-world controls. |
| 3 | Run 1's OS exit code was never recorded; Tee-Object preserves `$LASTEXITCODE` | **Confirmed.** The empty `THEORY_EXIT=` came from the observer's bash→PowerShell wrapper expanding `$LASTEXITCODE` before PowerShell ran: the same wrapper bug that ate `$out` in a later launch. Run 2 measured exit `1` directly. |
| 4 | `retain_passing`/`retain_failing` validated but unimplemented; runs gitignored | **Confirmed.** `pkg/schema/retain.go` is the polymorphic type + validation only; `internal/recorder/retain.go` exists solely in `docs/design/02-recorder.design.md` as planned work; `.gitignore` excludes `**/.prothesis/runs/`. Evidence survives by accident. |
| 5 | DRIVE zero-width is an `Advance`-vs-`Nest` bug | **Confirmed.** `internal/control/runner.go:1064` calls `phaseLog.Advance(PhasePerturb)` under a comment saying "PERTURB: overlaps DRIVE". `Advance` closes the open primary phase (`phase.go:134-137`); `Nest` exists precisely for PERTURB ("the only phase this applies to in v1", `phase.go:151`). Code contradicts its own comment. |
| 6 | Determinism over-claimed; defect reproduces at p ≈ 0.78 | **Confirmed** via OQ-054 (7 of 9 on this exact schedule). 2/2 at p=0.78 is ~61%; record reworded. A third run (also FAIL, exit 1) makes 3/3, ~47% likely at p=0.78: consistent, still not determinism. |
| 7 | `--seed 1` worked as designed | **Confirmed empirically.** A third run with `--seed 1` reproduced run 1's world seed 2337997644773862495 exactly (`WorldSeedFor`, `runner.go:553`). Run 2 differed only because it omitted `--seed`. Revision 1's incidental finding #1 retracted. |
| 8 | Census: 73 info ops + 7 phase markers | **Confirmed.** history.jsonl interleaves phase markers as `{"type":"info","event":"phase"}` records; 73 + 7 = 80. |
| 9 | Traffic spans PERTURB *and* HEAL | **Confirmed.** Phase-bucketing run 1's history: ≈2,780 ops in PERTURB, ≈2,618 in HEAL (audit said 2,788/2,618). The driver runs until QUIESCE drains it. |
| 10 | Run 2 witness margin ≈ 1.04 ms (~2 clock ticks) | **Confirmed exactly** (1.04 ms vs ~0.5 ms host clock granularity). Run 1's witness margin is 456.7 ms; the oracle verdict rests on the exhausted whole-key search, not on either margin. |
| 11 | "Two later runs" was phantom wording | **Confirmed.** There was one failed PowerShell launch (never reached the harness) and one later run. Corrected. |
| 12 | No public verifiability: runs gitignored, observation docs untracked, binaries uncommitted | **Confirmed when made; partly remediated since (D-070).** The observation directory is now tracked (34 files, committed in c983354) and carries `evidence/`, so the bundle is self-verifying. The other two clauses still hold: runs are gitignored (`**/.prothesis/runs/`) and binaries are `*.exe`-ignored. |

## Audit claims that are themselves wrong

1. **The audit's proposed C2 fix is still unsound.** Its written criterion
   `W_B.complete < W_A.complete < R.invoke` uses complete-before-complete,
   which is not real-time precedence: if W_A was *invoked* before W_B
   completed, the two overlapped and W_A can be linearized *before* W_B,
   making the read legal. Implemented as "C2 v2" in `crosscheck.py`, it flags
   89/72 on the failing runs and still **5–14 false positives on clean PASS
   worlds**. The sound criterion needs `W_B.complete < W_A.invoke` (strict
   precedence between the writes), which is what v3 implements and what
   reproduces the audit's correct 72/46. The audit's analysis was right; its
   published algorithm was not.
2. **"Strip pre-D-069 commit hashes" / detached-commit provenance.** The
   verdict's `commit: 4a3e10c` is the *current tip* of the rewritten history
   (D-069's purge landed at `ada1daa`, followed by `88cdc88` and `4a3e10c`);
   it is neither detached nor pre-purge, and no pre-D-069 hash appears in
   this observation bundle. The underlying provenance point stands on its own
   merits (the tree was dirty and the binaries embed no build provenance, so
   binary→source lineage is asserted, not proven) but no hashes needed
   stripping here.
3. **Terminology nit:** the audit describes the stale read as "at Term 1".
   Both runs' logs show the partition-era term as **2** (kv-n2 stepped down
   to `follower term=2`; kv-n1 led term 2). Immaterial to every conclusion.

## What was changed in this bundle (Layer 0, done)

- `crosscheck.py`: C2 v1 kept for reference, audit criterion added as v2,
  sound criterion added as v3; C3 re-checks the witness under v3 and reports
  the margin; phase and info census added; clean runs (no witness) are
  handled so the script doubles as a false-positive control.
- `record.md`: revision 2; 72/46 counts, exit-code footnote corrected,
  retention claim corrected, seed finding retracted, determinism reworded per
  OQ-054, DRIVE explanation corrected to the `Advance`/`Nest` bug, census and
  phantom-run wording fixed, provenance caveat added.
- `design.md`: seed determinism documented, probabilistic-reproduction note
  added, expected-outcome wording aligned with OQ-033/OQ-054.

## Engine-level findings handed back

- **L1a**: DONE since this writing: `runner.go` now uses `Nest`/`Unnest` for
  PERTURB (uncommitted at review time, on top of c983354); DRIVE spans
  PERTURB in worlds written after the fix.
- **L1b**: implement retention pruning or fail-fast/warn that it is
  unmanaged.
- **L1c**: record the measured OS exit code and raw oracle stdout in
  artifacts.
- **L3**: statistical gating per OQ-054's proposal (recorded k/n rate +
  confidence bounds instead of k/k).

## Layer 2: implemented 2026-09-17 (D-070)

The provenance layer was approved and implemented:

- **Build provenance**: `internal/buildinfo` carries `-ldflags -X` stamps
  (commit / dirty / source date; HEAD's committer date, never a build clock,
  for the reason D-072 gives); `scripts/build.ps1` builds all three binaries
  stamped; `verdict.json`'s `commit` now comes from the binary's own stamp
  (working-tree HEAD is only the unstamped fallback); `thesis version`,
  `thesis-oracle-linearizable -version`, `loadgen -version` read the stamp
  back. Verified at the time of the D-070 build: all three reported `commit
  4a3e10c dirty=1 at 2026-09-17T13:51:51Z`, archived in
  `evidence/BUILD_INFO.txt`. Under D-072 the wall-clock field is gone and the
  readback now prints `source_date`, so that archived line is a historical
  record rather than something today's binaries reproduce.
- **`sut.images` (OQ-055 → RESOLVED)**: `harness.ResolveImages` inspects
  bound containers at BOOT; world files now carry (logical service, image
  reference, resolved image ID), deduplicated per distinct build, fail-soft
  to the schema's honest `[]`. Verified live in run `r_2026_09_17_1a13`:
  `"sut":{"images":[{"digest":"sha256:e995c7ec…","image":"prothesis/kvfixture:buggy","service":"kv"}]}`.
- **Evidence bundle**: this directory's `evidence/` subtree contains both
  runs' `verdict.json` + `world-0001/` essentials plus `BUILD_INFO.txt`;
  `python crosscheck.py evidence/r_2026_09_17_5805` re-verifies the 72
  stale reads and the witness from the bundle alone.
- **Lock interaction**: rebuilding the oracle binary fired the OQ-057
  program-digest warning ("the program(s) that decided this verdict
  changed"), as designed; a warning, not exit 4. Re-locked with
  `--reason "D-070: …"`; `thesis oracles verify` is green; the new binary
  fingerprint is recorded outside the digest.
- Run 4 also reproduced the defect (4/4 observed), same pinned world seed
  under `--seed 1`, exit code 1.

Ledger updates: OQ-055 marked RESOLVED with the migration caveat preserved;
D-070 records the decision and its rejected alternatives.
