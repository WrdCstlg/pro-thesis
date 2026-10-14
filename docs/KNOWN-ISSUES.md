# Known issues

The open weaknesses that bear on what a verdict means, in one place, grouped by what they
put at risk. Each is an entry in [`OPEN_QUESTIONS.md`](../OPEN_QUESTIONS.md), which has the
code locations and the measurements; this page is the digest, and the ledger wins wherever the
two disagree. [`SECURITY.md`](../SECURITY.md) covers the ones an adversary would use.

Each *would close it* below is the entry's own proposal, not scheduled work.

---

## What a PASS does not yet guarantee

A false PASS is the failure this project exists to prevent, so these come first.

- **CLOSED (H1, OQ-085): omitting `--fault` no longer passes the planted defect.** The fault
  schedule lives in the lock-covered run profile (`profiles.<name>.faults`); an argv schedule
  that differs from it, or `--no-faults`, narrows the run under D-059, so a clean result is
  INCONCLUSIVE and a run under the locked schedule catches the defect. Measured closed: gaming
  benchmark attack B1, exit 1 with a witnessed violation (run `r_2026_10_12_4151`,
  `docs/reviews/2026-10-12-phase1-publication-verification.md`).
- **A truncated history is still judged as complete (OQ-077, second half open).** The first
  half closed with item H2: a driver that never started, missed the drain deadline or exited
  non-zero on its own now makes the world INCONCLUSIVE. What remains is a history cut cleanly
  on a record boundary by a well-behaved driver. *Would close it:* end every history with a
  record count the harness checks (H3).
- **Nothing checks that the load overlapped the fault (OQ-078).** A world is scored from oracle
  findings alone, and `verdict.json` records no fault count, operation count or list of the
  oracles evaluated. A PASS with zero faults realized and three seconds of load has the same
  shape as a PASS under a leader partition; one such run was measured. *Would close it:* score a
  faulted world with no operation in flight during any realized fault window as INCONCLUSIVE, and
  add `faults_realized`, `ops_judged` and `oracles_evaluated` to the verdict.
- **The checker leaves the register's initial value open (OQ-079; H6a fixes the timestamp half, unreviewed).**
  The first read may establish any value. That is right for a history that starts mid-stream, and
  because the harness's worlds do not reliably boot on fresh storage (OQ-087), it is also what keeps
  a world that boots on an earlier world's data from being reported as a violation. A record with no
  `t_ns` no longer loads as 0: the checker refuses it as INCONCLUSIVE (H6a, D-104; not yet reviewed
  by a session that did not build it). *Would close it:* a known-empty initial state, claimed only
  where the harness can show fresh storage and the target declares that it starts empty (H6b).
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
- **A deleted lock does not stop `thesis run` (D-037; measured in D-099).** The run warns, records
  `oracle_lock.status: "absent"`, and goes ahead, so deleting the lock and then weakening the
  workload produced exit 0 on the planted defect in one of two passes. `thesis oracles verify`
  refuses a missing lock with exit 4, and CI runs it before any world. Whether `run` should refuse
  too awaits the author's ruling.
- **Several attacks are held only by chance (D-099).** Deleting the lock,
  swapping the driver for a lighter one, and shrinking the workload through `PROTHESIS_OPS` each
  produced a false PASS in one of two passes. When they did not, what stopped them was a refusal the
  known-problem registry already accepts (KP-001, KP-002), not a defence. Omitting the faults was
  in this group until H1 closed it (attack B1 now refuses with exit 1).
- **The etcd target's programs are built by no script (OQ-088).** They come from a plain
  `go build`, without the build stamp or the reproducible flags, so what they were built from
  cannot be read back, and the lock's last reason for them said they came from a clean build.
  *Would close it:* build them in `scripts/build.ps1` and re-lock from that.

## Where the harness can fail quietly

- **Teardown can leave a world's volumes behind without saying so (OQ-087).** Every serial world
  of a checkout boots under one compose project, so a volume that survives teardown is the next
  world's storage. `Down` runs compose from the working directory with a relative file name, while
  `up` runs from the project directory; started elsewhere, `compose down` fails, the label sweep
  removes containers and networks only, and teardown reports success. Read from the code, not
  reproduced. *Would close it:* run `down` from where `up` ran, sweep volumes by label, and check at
  BOOT that the project has none.
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
