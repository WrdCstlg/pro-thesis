# Defects

Append-only. Each entry: id, how it was found, symptom, cause, fix, and the test that now guards it.
Defects found in faultline's own harness while building it belong here first.

## DEF-001 A PASS-row check aborted the row and hid the CLI's exit code (2026-10-06)

- Found by: the first full M0 gate run (`bun run --cwd "C:\AI Projects\faultline" gate`).
- Symptom: A01, A02, A04 and A07 showed `actual = gate-error` with "read ...faultline.json: ENOENT".
  The CLI's own exit code and the hygiene results for those rows were discarded.
- Cause: `plannedWorlds` in `scripts/gate.ts` failed the whole row effect when `faultline.json`
  could not be read, so a missing config while checking an exit-0 row was treated as a gate error,
  not as a failure of that row.
- Fix: `plannedWorlds` is now total. An unreadable or undecodable config yields `Option.none`,
  which `expectedShape` already reports as the failure "profile's planned world count not found in
  faultline.json". The row still fails (no weakening); the exit code and hygiene results stay
  visible. Applied before `GATE.lock` was written.
- Guard: none in `bun test`, because the gate's row runner has no unit test. Observed in the final
  M0 gate output: A01/A02/A04/A07 now show `exit=1` with "exit 1, expected 0" and "expected exactly
  one new verdict.json, found 0" instead of `gate-error`. The planned-count line itself only appears
  once a `verdict.json` exists, which no row produces yet, so that path has not been exercised.
  OPEN_QUESTIONS records that the row runner lacks a unit test.

## DEF-002 A Windows clone fails its own gate-lock step (2026-10-06)

- Found by: cloning the first commit (`e7c8179`) with `git clone` on this host, where
  `core.autocrlf` is `true`, and hashing the checkout with `Get-FileHash`.
- Symptom: in the clone, `acceptance.json` hashed `8f6e2794...503d15` and `scripts/gate.ts` hashed
  `e3367362...afcb6e4`; `GATE.lock` records `d28062d5...18aa115dc` and `a9b90f55...89481bc`. The
  committed blobs themselves (`git cat-file blob HEAD:<file>`) hash to the locked values.
- Cause: with `core.autocrlf=true` git writes CRLF on checkout, and the gate hashes raw bytes. The
  effect is a gate-lock FAIL on a fresh Windows clone, not a false PASS.
- Fix: `.gitattributes` with `* -text`, so git never converts line endings in this repository (D-020).
- Guard: the clone-and-hash measurement is repeated after the fix and recorded in D-020. There is no
  `bun test` guard, because the condition depends on the cloning machine's git config.
