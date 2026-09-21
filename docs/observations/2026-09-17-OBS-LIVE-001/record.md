# OBS-LIVE-001: Observation record

Date: 2026-09-17
Observer: Claude (external; recorded the execution, did not influence it)
Design: [`design.md`](design.md); scenario, expected outcome, function criteria F1–F5

> **Revision 2 (2026-09-17).** An external audit of revision 1 found the
> observer-side cross-check (C2) unsound and several claims over-stated. All
> audit findings were re-verified against code and artifacts before being
> accepted; two audit claims were themselves found wrong and are documented in
> `audit-response.md`. This revision corrects the record. The unsound
> algorithm is preserved in `crosscheck.py` as "C2 v1" for comparison.

## Verdict of the observation

**PRO-THESIS functioned end-to-end.** The harness booted a 3-node Raft
cluster, drove ~5.4k–7.4k client operations, partitioned the leader's peer
plane at t+3s, healed at t+7.5s, recorded a normative history, and the
external `linearizable.kv` oracle proved (by exhausted search, not timeout)
that the history is not linearizable. Measured process exit code was `1`
(runs 2, 3, 4: see `evidence/exit-codes.txt`), per the normative contract.

The observer's independent recount (`crosscheck.py`, sound criterion C2 v3)
finds **72** (run 1) and **46** (run 2) hard real-time stale reads, not the
1,161/1,583 that revision 1 reported; see "Independent cross-check" below.
The anomaly is real; the original magnitude claim was an artifact of a broken
observer heuristic.

## Setup evidence

- Host: Windows, PowerShell (Git Bash fork-unstable on this host; known, see
  `prothesis.yaml` steady_state comment; all observation went through
  PowerShell).
- `go version go1.27.1 windows/amd64`; Docker Engine `29.1.3` (Docker Desktop
  was not running at session start; the observer started it).
- Binary SHA-256:
  - `bin/thesis.exe` = `096D9D7B…C76476B6`
  - `testdata/kvfixture/bin/loadgen.exe` = `A89E29F5…AF64349`
  - `testdata/kvfixture/bin/linearizable-kv.exe` = `4DC6FB1F…C2D24E4`
- Images present: `prothesis/kvfixture:buggy`, `:kvfixed`,
  `prothesis/netadmin:1`, `prothesis/kvtools:1` (no build needed).
- Gate integrity: `thesis oracles verify` → `oracle lock OK:
  sha256:0ebddd17…af1766`, 1 oracle file, 37 covered config values. No run
  exited `4` (ORACLE_DRIFT): the lock did not move.
- Provenance caveat: `verdict.json` records the working-tree HEAD. The tree
  was dirty (modified README/SECURITY files),
  and the binaries carry no embedded build provenance, so "what source
  produced these binaries" is asserted, not proven.

## Executions

Run 1 and run 3 (from `testdata/kvfixture/`):

```
../../bin/thesis run --profile linear --seed 1 --fault 'net.partition(role:leader)@3000..7500'
```

Run 2 omitted `--seed 1`. Revision 1 of this record showed a single command
block that only matched run 2; corrected.

| | Run 1 | Run 2 | Run 3 (seed check) |
|---|---|---|---|
| Run ID | `r_2026_09_17_5805` | `r_2026_09_17_66d5` | see `harness-run3-seedcheck.log` |
| `--seed` | 1 | *(omitted)* | 1 |
| World seed | 2337997644773862495 | 15886428869686273937 | **2337997644773862495** |
| Verdict | FAIL | FAIL | FAIL |
| **Process exit code** | not recorded¹ | **measured `1`** | **measured `1`** |
| Wall time | 38s / 600s budget | 32s / 600s budget | ~33s |
| History records | 10,819 (5,406 ops: 5,317 ok / 16 fail / 73 info, + 7 phase markers) | 14,881 | n/a |
| Violating oracle | `linearizable.kv` (consistency, high) | same | same |
| Oracle proof | exhausted search, 2,030 states / 7,289 steps | exhausted search, 5,287 states / 18,797 steps | n/a |
| Witness (key k/0) | write 15000136 (2320) → write 6000153 (2447) → read returns **15000136** (2470) | write 9000265 (4340) → write 15000281 (4391) → read returns **9000265** (4408) | n/a |
| Witness margin | 456.7 ms | **1.04 ms** (~2 host clock ticks) | n/a |
| Built-in oracles | all `ok` | all `ok` | n/a |
| Hard stale reads (C2 v3) | **72** | **46** | n/a |

¹ Run 1's OS exit code was never captured: the observer's bash→PowerShell
wrapper expanded `$LASTEXITCODE` (to the empty string) *before* PowerShell
ran. Revision 1 blamed `Tee-Object`; that was wrong: Tee-Object preserves
`$LASTEXITCODE`. Runs 2–4 were measured at the OS level (all `1`); the
measurements are recorded in `evidence/exit-codes.txt` (the per-run harness
logs contain only the harness's self-reported `FAIL (exit 1)`).

### What the executions do and do not show about determinism

The defect is **probabilistic, not deterministic**: OPEN_QUESTIONS.md OQ-054
measured this exact minimal schedule reproducing at **p ≈ 0.78 (7 of 9)**,
because a client must be mid-read on the isolated leader inside the lease
window. At p = 0.78, observing 2/2 happens ~61% of the time: revision 1's
framing of 2/2 as demonstrating a deterministic window was over-claimed, and
contradicted the ledger. With runs 3 and 4 the observed tally here is 4/4, which at
p = 0.78 occurs ~37% of the time; consistent with the ledger, still not proof
of determinism. The `--seed` flag *is* deterministic, though: run 3
reproduced run 1's world seed exactly (verified empirically), which retracts
revision 1's incidental finding #1.

## Live timeline (run 1, observed in real time)

- t+0s: `thesis run` launched (background), stdout/stderr to `harness.log`.
- t+20s: `docker ps`; `prothesis-kv-n1/n2/n3` all `Up (healthy)`, client
  ports published on 127.0.0.1:18081–18083.
- Fault injected t+3014 ms world-time: `net.partition(role:leader) -> kv-n2`;
  withdrawn t+7500 ms. Role targeting resolved the *current* leader (kv-n2)
  live, not a static name.
- t+45s: kv containers already torn down; harness printed verdict and exited.
- Phases per `phases.json`: BOOT (−22,751..−142 ms) → SEED → DRIVE (0..0 ms)
  → PERTURB (0..7,941) → HEAL (7,941..10,512) → QUIESCE (driver drained in
  20 ms) → ASSERT. kv-n2 logged `role_change role=follower term=2` at
  t+7688 ms; the partitioned leader discovering its replacement after heal.
- Traffic was NOT confined to PERTURB: phase-bucketing the history gives
  PERTURB ≈ 2,780 ops and HEAL ≈ 2,618 ops (run 1; records/2). The driver
  keeps running through HEAL until QUIESCE drains it.

The anomaly, as recorded in the history: kv-n2, isolated on the peer plane,
kept serving lease reads from its pre-partition state for the remainder of
its 5 s lease while the majority elected kv-n1 (term 2) and committed newer
writes. This is the defect documented in `testdata/kvfixture/README.md` §1.

## Independent cross-check (observer-side, evidence only)

`crosscheck.py` reads only `history.jsonl` plus the witness claim from
`verdict.json`. Three variants of the stale-read scan are printed side by
side:

- **C2 v1 (revision 1's algorithm, UNSOUND)**: flags a read whose value
  differs from the newest write *completed* before the read was invoked. This
  convicts legal reads: the write that produced the returned value may have
  overlapped that newer write and can be linearized *after* it. Measured
  damage: 1,161/1,583 flags on the two failing runs, and **761–941 false
  positives on single worlds of known-PASS runs** (r_2026_09_15_0d1c,
  r_2026_09_15_f7e8, r_2026_09_09_c2a7). A "checker" that convicts clean
  histories is worse than none.
- **C2 v2 (the audit's proposed criterion, STILL UNSOUND)**:
  `W_B.complete < W_A.complete < R.invoke`. Complete-before-complete is not
  real-time precedence; if W_A was *invoked* before W_B completed, W_A can be
  linearized before W_B. Flags 89/72 on the failing runs and still 5–14 on
  clean PASS worlds.
- **C2 v3 (sound)**: R returning v is a hard violation iff the write W_B that
  produced v completed, and some write W_A on the key satisfies
  `W_B.complete < W_A.invoke` AND `W_A.complete < R.invoke`. Then W_B ≺ W_A ≺ R
  in every linearization, so R must return W_A's value or newer. Conservative
  by construction: it can miss violations a full search catches; it cannot
  manufacture one. **Result: 72 (run 1), 46 (run 2), and 0 on all three
  checked PASS worlds.**

Other checks: **C1**; witness op_ids exist in the cited history with
invoke+completion (both runs; the repo's own
`scripts/adversarial_forensics.py witness` agrees). **C3**: the oracle's
witness reconstructs under the v3 criterion in both runs. Run 2's witness
margin is 1.04 ms against a measured host clock granularity of ~0.5 ms
(fixture README §5): genuine but tight; run 1's witness margin is a
comfortable 456.7 ms, and the oracle's verdict rests on the exhausted
whole-key search, not on either single margin.

## Function criteria

- **F1 lifecycle**: all phases completed, no harness crash. PASS (but see
  note 3 below on DRIVE's recorded window).
- **F2 normative history**: `prothesis.history/v1` JSONL emitted, fully
  parseable (zero unparseable lines). PASS
- **F3 oracle verdicts**: 6 built-in oracles `ok`; external oracle spoke the
  pipe protocol and returned a violation with witness. PASS
- **F4 exit-code contract**: measured process exit `1` on FAIL verdicts
  (runs 2, 3, 4; see `evidence/exit-codes.txt`); run 1's OS code was not
  captured. PASS, with the caveat that
  the harness does not itself record the OS exit code in any artifact.
- **F5 reproducibility**: 4/4 observed here; ledger-measured rate is
  p ≈ 0.78 (OQ-054), so reproduction is probable, not guaranteed. The
  observer's sound recount independently confirms the anomaly in both
  analyzed runs. PASS (reworded from revision 1's determinism framing).

## Notes and corrections

1. **Artifact retention is not implemented.** Revision 1 said the run
   directories are protected by `retain_failing: all`. In fact
   `retain_passing`/`retain_failing` are parsed and validated
   (`pkg/schema/retain.go`, `config_validate.go`) but no pruning or
   protection code exists (`internal/recorder/retain.go` appears only in
   design docs), and `**/.prothesis/runs/` is gitignored. The evidence
   survives only because nothing deletes it; `git clean -Xdf` would destroy it.
2. **`--seed` works.** Run 3 with `--seed 1` reproduced run 1's world seed
   (2337997644773862495) exactly; world seeds derive deterministically via
   `control.WorldSeedFor(runSeed, ordinal)` (`internal/control/runner.go:553`).
   Run 2 differed because it omitted `--seed`. Revision 1's incidental
   finding #1 is retracted.
3. **DRIVE's zero-width window was a harness bug, not a schedule artifact.**
   `internal/control/runner.go:1064` (at the time of runs 1–3) called
   `phaseLog.Advance(PhasePerturb)` directly under a comment reading "PERTURB:
   overlaps DRIVE (directive 4.1 step 4)". `Advance` *closes* the open
   primary phase (`internal/control/phase.go:134-137`); the overlap the
   comment promises is what `PhaseLog.Nest` exists for (`phase.go:151`,
   "PERTURB is the only phase this applies to in v1"). Revision 1's
   explanation ("a pinned fault schedule moves the world straight into
   PERTURB") was wrong. **The fix (L1a) has since been applied** (`Nest`/
   `Unnest` in the tree after c983354) so worlds written after that change
   record DRIVE spanning PERTURB; the 0..0 window in this bundle's evidence
   is the pre-fix behavior.
4. **Census**: the history's 80 `info`-typed records are 73 operation-info
   records + 7 phase lifecycle markers (`{"type":"info","event":"phase"}`).
   Revision 1 quoted 80 as the op-info count.
5. **Oracle stderr log is 0 bytes** although `witness.stderr_path` points at
   it: the oracle reports on stdout per the pipe protocol; the empty file is
   benign but the pointer is a red herring.
6. **Exit-code capture noise**: revision 1's run-1 wrapper bug is documented
   in the table footnote. PowerShell surfaces the harness's stderr progress
   lines as `NativeCommandError` noise; harmless.
7. **Container sampling**: run 1's container lifecycle was captured directly
   (healthy at t+20s, gone at t+45s). Sampling during the two later
   executions (one failed PowerShell launch that never reached the harness,
   and run 2) missed the window: revision 1's wording implied more runs than
   existed.
8. **Verifiability of this bundle**: remediated 2026-09-17 (D-070). The
   `evidence/` subtree carries both runs' verdicts and world artifacts plus
   `BUILD_INFO.txt`, so the bundle is self-contained:
   `python crosscheck.py evidence/r_2026_09_17_5805` re-verifies the counts
   and the witness without the gitignored runs directories. The binaries are
   now build-stamped and world files now record `sut.images` (OQ-055
   resolved); runs 1–3 predate both, which `BUILD_INFO.txt` says plainly.
9. **The bundled copies of `verdict.json`, `oracle_input.json` and `plan.json`
   were edited after the fact.** Seven path values in each run, spread over
   those three files, were absolute paths on the build host
   (`C:\AI Projects\Pro-synthesis\…`) as the harness emitted them: the
   witness's `oracle_definition` and `stderr_path` in `verdict.json`;
   `history_path`, `final_state_path`, `telemetry_path` and `world_path` in
   `oracle_input.json`; and `history_path` again in `plan.json`, which carries
   no other path. They were rewritten to repository-relative form before this
   bundle was committed. Nothing else was changed (every other file under
   `evidence/` hash-matches its live run directory, including `history.jsonl`,
   the file every count and both witnesses are recomputed from) so only those
   three files per run no longer hash-match theirs. The harness still emits
   absolute paths (`internal/oracle/process.go`), so re-running this scenario
   reproduces them: this is a redaction in the bundle, not a fix in the tool,
   and a reader comparing the two copies is owed that sentence rather than
   left to discover it.

## Artifacts

- This directory: `design.md`, `record.md` (this file), `audit-response.md`,
  `crosscheck.py`, `harness.log` (run 1), `harness-run2.log` (run 2),
  `harness-run3-seedcheck.log` (run 3), `harness-run4-provenance.log`
  (run 4), and `evidence/` (both runs' verdicts and world artifacts,
  `BUILD_INFO.txt`, `exit-codes.txt`).
- Harness run directories (gitignored, unprotected; see note 1):
  `testdata/kvfixture/.prothesis/runs/r_2026_09_17_5805/`,
  `…/r_2026_09_17_66d5/`, and the run-3 directory listed in
  `harness-run3-seedcheck.log`.
