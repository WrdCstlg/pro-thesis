# OBS-LIVE-001: Live partition-anomaly run with external observation

Date: 2026-09-17
Observer: Claude (external to the harness; records execution, does not influence it)

## Purpose

Demonstrate that PRO-THESIS functions end-to-end (orchestration, fault
injection, normative history recording, oracle evaluation, verdict emission)
while an external observer watches the execution live and records it into a
durable observation record (`record.md`, same directory).

This is an *observational* test of the harness, not a new gate: it changes no
code, no config, no lock. The pass/fail question is "did PRO-THESIS execute its
documented lifecycle and produce its normative artifacts and exit-code
contract", not "did the SUT behave".

## System under test

`testdata/kvfixture`: the 3-node Raft KV store, `buggy` variant
(`LeaseDuration = 5s` vs `ElectionTimeoutMin = 600ms`, the one deliberate
defect; see `testdata/kvfixture/README.md` §1).

## Scenario

From `testdata/kvfixture/`:

```
../../bin/thesis run --profile linear --seed 1 --fault 'net.partition(role:leader)@3000..7500'
```

- `--profile linear`: the only driver profile the `linearizable.kv` oracle can
  soundly check (single-key, read/write only; README §5).
- `--seed 1`: pins the run seed; world seeds derive deterministically
  (`control.WorldSeedFor`), so the world is replayable. Verified 2026-09-17:
  two executions with `--seed 1` produced the identical world seed.
- Five worlds planned and a 10m budget, both from the `linear` profile
  (`worlds: 5` in `testdata/kvfixture/prothesis.yaml`, and both bundled
  verdicts record `worlds_planned: 5`). `--fault` pins the fault SCHEDULE that
  every world replays, not the world count; nothing here narrows it. In
  practice one world ran, because world 1 produced a violation and the runner
  stops at the first one (`internal/control/runner.go:421`).
- Fault: isolate the current leader's peer plane at t+3000ms, heal at t+7500ms.
  The isolated leader keeps serving lease reads for the remainder of its 5s
  lease while the majority elects a replacement; the documented anomaly
  window (README §6: ~4s stale-read window under partition-only).

Note on determinism: the window's *existence* is structural, but catching a
stale read in it requires a client to be mid-read on the isolated leader
inside the lease window; a race. OQ-054 measured this exact schedule
reproducing at p ≈ 0.78 (7 of 9). Expect reproduction to be probable, not
certain; a single non-reproducing run is not evidence the defect is gone.

## Expected outcome

- Exit code `1` (FAIL): the `linearizable.kv` oracle witnesses a stale read
  served by the partitioned leader. Given p ≈ 0.78 (OQ-054), a single
  non-FAIL run is not by itself a finding: only a measured rate is.
- A new run directory `.prothesis/runs/r_<date>_<id>/` containing
  `verdict.json`, `world-0001/{history.jsonl, phases.jsonl, result.json,
  oracle_input.json, oracles/linearizable.kv.stderr.log, logs/kv-n*.log,
  final_state.json, telemetry.json, plan.json, world.thesis}`.

## What the observer records

1. **Setup evidence**: tool versions (go, docker engine), binary hashes
   (`bin/thesis.exe`, `bin/loadgen.exe`, `bin/linearizable-kv.exe`), image
   state before the run.
2. **Live timeline**: harness stdout/stderr captured verbatim to
   `harness.log`; phase transitions as they print; container lifecycle from
   `docker ps` sampled during the run.
3. **Artifact inventory**: the run directory tree with sizes; key fields of
   `verdict.json` and `result.json`; the oracle's witness from
   `oracles/linearizable.kv.stderr.log`; operation/violation counts from
   `history.jsonl`.
4. **Independent cross-check**: the observer recomputes the anomaly claim from
   the raw history (a read returning a value written *before* the last
   completed write on the same key), rather than trusting the tool's verdict;
   the same discipline as `scripts/adversarial.ps1`.

## Function criteria (what "pro-thesis functions" means here)

- F1: Lifecycle completes BOOT → SEED → DRIVE → PERTURB → HEAL → QUIESCE →
  ASSERT without a harness-level crash.
- F2: Normative history (`prothesis.history/v1`) is emitted and parseable.
- F3: Both built-in oracles and the external `linearizable.kv` oracle produce
  verdicts; the external oracle speaks the pipe protocol
  (`prothesis.oracle_input/v1` in, `prothesis.oracle_output/v1` out).
- F4: Exit code follows the normative contract (0/1/2), specifically `1` here.
- F5: The observed anomaly is independently reproducible from the raw history
  by the observer's own recount.

## Risks / notes

- Git Bash on this host is fork-unstable (0xC0000142); all shell work goes
  through PowerShell. This is also why `steady_state.probe` runs inside a
  container (see `prothesis.yaml`).
- `.prothesis/lock` must not drift: the run must exit 0/1/2, never 4
  (ORACLE_DRIFT). No config or oracle file is touched by this test.
- First run on a cold Docker engine may need `docker compose build`; image
  state is recorded in the setup evidence either way.
