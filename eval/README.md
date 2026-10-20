# Evaluation suite

This directory measures the harness itself as a fault-injection system, across several target
systems. The deliverable is evidence about the harness (how often it detects a known violation,
how often a known-correct system passes, what its refusals mean, whether its gate holds), not a
verdict about the target systems.

## Method

- **Controls before targets.** Every arm is a harness control, a positive control (documented to
  violate the checked contract), a negative control (documented correct), a discriminating control
  (one configuration bit flips correctness) or an oracle-discipline arm (a contract weaker than
  linearizability, run to see whether the harness declines to overclaim).
- **Pre-registration.** Each arm has a prediction file in `predictions/`, dated and committed before
  the arm's first world. It states the expected verdict, mechanism, violation class, fault kinds
  expected to realize, the risks of an INCONCLUSIVE, and what would falsify it. Verdict and
  mechanism are scored separately.
- **Real execution only.** Every number comes from worlds that ran under Docker and were recorded
  through the harness's normal artifacts (`verdict.json`, `world.thesis`, `history.jsonl`).
  Nothing is simulated or extrapolated; a phase that could not run is reported as such.
- **Independent re-derivation.** Every violation in the scorecard is re-derived from the raw
  history by a path that does not read the harness's verdict, and exit codes are read in the
  launching shell.
- **Refusals are data.** INCONCLUSIVE outcomes are scored and attributed (`thesis diagnose`), or
  reported as unattributed.
- **No gate is weakened to get an answer.** Oracle definitions, lock-covered keys and run profiles
  are not edited to make an arm land; a scoped "no claim" replaces a forced fit.

## Operating rules

- Every arm is launched from its target's project directory, and after every world the target's
  compose-labelled volumes are checked and the count recorded with the run id. A world that started
  on another world's storage is excluded from the precision and reproducibility figures and
  reported.
- Run artifacts stay in each target's `.prothesis/runs/`. After a phase is scored and cross-checked,
  its runs are archived with `tools/archive-runs.ps1` (compressed, every file hashed and verified,
  the original moved to `.prothesis/runs/.trash/`, never deleted). Runs cited as evidence are not
  archived. The pre-existing fixture corpus is never archived.

## Layout

| Path | What |
|---|---|
| `manifest.yaml` | Every arm, one row each: target, control type, variant, fault basis, N, seeds, oracles, profile, prediction file, phase, status |
| `predictions/` | One frozen pre-registration per arm (`TEMPLATE.md` shows the fields) |
| `runs/INDEX.md` | Every run the suite made: arm, phase, run id, commit, exit code, worlds, archive state |
| `scorecard.md` | The seven axes by target; every cell a measured value with run ids and bounds, or UNMEASURED with the reason |
| `tools/` | Suite tooling (no harness code) |

The report is `docs/eval/EVAL-001.md`; it ends with what the suite does not close.
