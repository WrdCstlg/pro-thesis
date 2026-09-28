# Known issues

The open weaknesses that bear on what a verdict means, in one place, grouped by what they
put at risk. Each is an entry in [`OPEN_QUESTIONS.md`](../OPEN_QUESTIONS.md), which has the
code locations and the measurements; this page is the digest, and the ledger wins wherever the
two disagree. [`SECURITY.md`](../SECURITY.md) covers the ones an adversary would use.

Each *would close it* below is the entry's own proposal, not scheduled work.

---

## What a PASS does not yet guarantee

A false PASS is the failure this project exists to prevent, so these come first.

- **A driver that fails or is killed is not folded into the world's outcome (OQ-077).** No code
  reads the driver's result when scoring a world, and the fixture's history writer can be cut
  cleanly on a record boundary. A shortened history is judged as if it were complete: losing
  trailing reads can hide a stale read. *Would close it:* score a driver that exited non-zero, was
  killed, or did not drain as INCONCLUSIVE, and end every history with a record count the harness
  checks.
- **Nothing checks that the load overlapped the fault (OQ-078).** A world is scored from oracle
  findings alone, and `verdict.json` records no fault count, operation count or list of the
  oracles evaluated. A PASS with zero faults realized and three seconds of load has the same
  shape as a PASS under a leader partition; one such run was measured. *Would close it:* score a
  faulted world with no operation in flight during any realized fault window as INCONCLUSIVE, and
  add `faults_realized`, `ops_judged` and `oracles_evaluated` to the verdict.
- **The checker leaves the register's initial value open (OQ-079).** The first read may
  establish any value, which is right for a history that starts mid-stream and buys nothing for
  the harness's own worlds, which boot fresh volumes. A missing `t_ns` also decodes as 0 with no
  check. *Would close it:* a known-empty initial state when volumes were fresh, and the history
  validator run before the oracle.
- **`thesis search --strategy` is not treated as narrowing (OQ-081).** The command-line strategy
  overrides the locked `search.strategy`, and D-059's narrowing rule covers only `--budget` and
  `--worlds`, so such a run can report PASS with the lock `ok`. *Would close it:* treat the
  override as narrowing.

## What the lock does not cover

- **The checker's program sits outside the digest (OQ-057, mitigated by D-060).** A swapped,
  rebuilt or missing checker binary is a WARNING in the verdict (`oracle_lock.executables_moved`),
  never exit 4, because a routine rebuild would otherwise be an incident. Measured: a fake checker
  that always prints `ok` passes `thesis oracles verify` with exit 0, and the run reports PASS with
  exit 0; the warning is the only signal. The swap is possible; it is no longer silent.
- **A driver can change its workload without moving the digest (OQ-076, the residual after
  D-090).** `driver.cmd` and every driver profile a run profile names are covered, but not a
  workload the driver compiles in for a profile name, and not the environment variables it reads
  (the fixture's `loadgen` honours `PROTHESIS_CLIENTS`, `PROTHESIS_OPS`, `PROTHESIS_KEYS` and
  `PROTHESIS_MIX_*`). The driver's program is fingerprinted like the checker's, with the same
  warning.

## Where the harness can fail quietly

- **A failure to write `verdict.json` is discarded (OQ-080).** On a full disk, which this
  repository has suffered once, the run exits with its verdict code and leaves no verdict file.
  *Would close it:* exit 2 when the verdict cannot be written.
- **A panic in the harness exits 2 and writes nothing (OQ-082).** Go exits 2 on an unrecovered
  panic, and 2 is the INCONCLUSIVE code, so a loop reads a crash of the harness itself as "nothing
  was proven", with no verdict saying why. *Would close it:* recover in `main`, write an
  INCONCLUSIVE verdict that names the panic, and exit 2 on purpose.
- **An INCONCLUSIVE verdict records no reason (OQ-071).** `verdict.json` has no reason field,
  and a world that dies before ASSERT writes no `result.json`, so the cause lives only on that
  run's stderr. `thesis diagnose` attributes the recorded cases to a known-problem registry after
  the fact (D-082); persisting reasons at write time is not done.
- **`thesis doctor` and the harness resolve programs differently (OQ-083).** Where a program
  exists both with and without `.exe`, the harness runs the extensionless one on every operating
  system and `doctor` checks the `.exe` on Windows. It bit once, with Linux builds left in the
  fixture's `bin/`; those were removed, so this is latent. *Would close it:* one resolver shared
  by the harness, the lock and `doctor`.

## Designed, not yet proven

These are in the README under
[What is not yet proven](../README.md#what-is-not-yet-proven), in more detail:

- the guided search has never influenced a decision (OQ-060);
- operation-level shrinking has never completed end to end (OQ-052, OQ-063);
- the k/k confirmation gate assumes a determinism the target does not have (OQ-054);
- the model-driven search strategy is unvalidated (OQ-069);
- image identity is stable only while the build cache lives (OQ-068);
- the retention policy is enforced by nothing, and the run corpus only grows (OQ-070).
