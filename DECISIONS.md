# Decisions

Append-only. Supersede an entry by adding a new one that cites it; never edit an old entry.

## D-001 Location and how files reach it (2026-10-06)

faultline lives in its own repository at `C:\AI Projects\faultline` (own `git init`, branch `main`),
not inside any other project. The agent session that built M0 had file-tool access only to another
workspace; the user explicitly authorised writing via the terminal. Files are authored in the
agent's scratch directory and copied in with `Copy-Item`. Generated files (`bun.lock`,
`node_modules/`) are produced in place by `bun add`.

## D-002 The current milestone is read from `MILESTONE` (2026-10-06)

`scripts/gate.ts` is frozen by `GATE.lock` after M0 but must know the current milestone to mark later
rows PENDING. It reads a one-line `MILESTONE` file (`M0`..`M5`) next to `package.json`, decoded with
a literal schema. `MILESTONE` is deliberately not covered by `GATE.lock`: changing it is the act of
entering the next milestone, and it can only make the gate stricter (more rows become due).

## D-003 The gate executes every row on every run (2026-10-06)

Rows due after the current milestone are still executed. A later row that passes shows PASS; one
that fails shows PENDING and does not affect the gate's exit code. Rows due at or before the current
milestone show PASS or FAIL, and any FAIL makes the gate exit 1.

## D-004 How the acceptance matrix is encoded (2026-10-06)

- `acceptance.json` is a top-level JSON array of rows `{ id, due, args, env, expectedExit,
  expectedOracle?, extraAssert? }`. `args` is passed to `bun src/cli.ts` with cwd = repo root.
- The row shape has no setup field, so scenario setup lives in `extraAssert` as tagged entries the
  gate performs before the run: `bumpProfileWorlds` (A14), `interruptAfterMs` (A17),
  `tamperWorldSeedDigit` (A20). All other kinds are post-run assertions.
- Arguments of the form `{A03.world}`, `{A03.verdict}`, `{A03.tampered}` are resolved from an earlier
  row's run directory: the world named by that row's first violation from its `expectedOracle`, the
  run's `verdict.json`, or a one-digit-tampered copy of that world file.
- A12's matrix text says only "1 (FAIL dominates)". The row also carries `expectedOracle:
  edit-integrity`, because the specification maps d1 to edit-integrity; this is stricter, not weaker.
- A09–A12 need oracles and profiles the four named profiles lack. Their names are fixed here and are
  contracts for M4: profiles `ext-timeout`, `ext-malformed`, `no-oracles`,
  `disconnect-ext-inconclusive`; external oracles `sleep-forever`, `truncated-ok`,
  `always-inconclusive` (which reports reason `missing-probe`). `sleep-forever` must write its pid
  to a `*.pid` file inside the run directory so the gate can check the process is gone.
- A20 has no `noWorldStarted` assertion because replay has no run directory the gate can inspect;
  that assertion would pass vacuously there.

## D-005 Output layout and verdict fields the gate depends on (2026-10-06)

The gate finds a run by diffing `.faultline/runs/*/verdict.json` before and after the row, and
requires exactly one new file for every `run` row except usage errors. Layout:
`.faultline/runs/<runId>/verdict.json` and `.faultline/runs/<runId>/worlds/<worldId>/world.json`.
`verdict.json` must carry at least `exitCode`, `lockStatus` (`ok|bypassed|drift`),
`worlds[{ world, verdicts[{ _tag, oracle }] }]`, `violations[{ world, oracle, witness (non-empty),
explanation }]` and `inconclusive[{ reason, world?, oracle? }]`. The gate decodes these through its
own schemas rather than importing `src/schema.ts`, so a defect in the harness's schema cannot also
blind the judge. `src/schema.ts` (M1) must produce a superset of these fields.

For every `run` row the gate also checks that `verdict.json`'s `exitCode` equals the process exit
code, and that the verdict's contents fit the exit: a 0 needs lock status ok, as many judged worlds as
the profile plans, at least one verdict per world, all Ok, and no violations or inconclusive entries;
a 1 needs a violation (from `expectedOracle` when given); a 2 needs at least one inconclusive entry
and no violation; a 3 needs no violation; a 4 needs lock status drift.

## D-006 Workspaces are `faultline-*` directories under `os.tmpdir()` (2026-10-06)

For each row the gate points `TMP`, `TEMP` and `TMPDIR` at a fresh `.gate/tmp/<row>/` and fails the
row if any `faultline-*` entry remains after exit (a leaked world workspace). A17's
`workspaceExistsAtInterrupt` is the positive control: it requires a `faultline-*` entry to exist at
the moment the interrupt is sent, which proves the redirect is honoured. Without that control, the
"empty afterwards" check could pass because workspaces went somewhere else. The runner must
therefore create each world's workspace under `os.tmpdir()` with the `faultline-` prefix, and acquire
it before the stub and the SUT.

## D-007 Process accounting, and what "zero ports" means here (2026-10-06)

After every row the gate lists processes and fails the row if any process started during the row
outlives the CLI. It re-checks up to 3 times, 1000 ms apart, before declaring a leftover, and then
kills it so later rows are not poisoned. On Windows it walks `ParentProcessId` links from the CLI's pid
(Windows keeps a dead parent's pid). On POSIX it also counts new processes in the gate's own process
group, because orphans are re-parented. Known limit: on POSIX, a process that calls `setsid` and is
orphaned escapes the check.

Ports are not measured directly. The stub runs inside the CLI process, so its listening socket closes
when the CLI exits, and a SUT port cannot outlive the SUT process. "Zero open stub ports" is
therefore checked as "the CLI exited and no descendant survived". For A14, "zero processes spawned,
zero ports opened" is checked as: exit 4, exactly one `verdict.json` with lock status drift, no
`world.json` written, no `faultline-*` workspace created. That relies on the D-006 ordering (the
workspace is acquired before the stub and the SUT). See OQ-003.

## D-008 Banned-pattern scan: method and limits (2026-10-06)

- Code patterns are matched line by line on source whose comments and string/template literals were
  blanked by one regex alternation. Newlines are kept, so reported line numbers are exact.
- Marker patterns (the to-do/fix-me/triple-x markers, "not implemented", the p-word, `stub()`, the
  three ts-directives) are matched on the raw source, because those words appear in comments and
  strings. Their regexes are assembled from fragments so `scripts/gate.ts` does not match itself.
- Limits of the crude stripper: a regex literal containing a quote character or `//` can open a
  fake string or comment; a template literal whose `${}` contains a backtick is mis-split; the regex
  alternation does not understand JSX. Any of these can hide or invent a match on the affected
  line. `test/gate-scan.test.ts` pins the intended behavior, including a check that the gate script
  itself scans clean.
- The non-null rule is broader than the spec's `!.` / `!)`: it also flags `!]`, `!,` and `!;`
  after an identifier, `)` or `]`.
- `scripts/` is scanned like `src/`. The gate writes its report with `process.stdout.write`, since
  `console.` is banned there.

## D-009 Dependency versions and API naming (2026-10-06)

Resolved from `bun.lock`: `effect@4.0.1`, `typescript@7.0.2`, `@types/bun@1.3.12` (→
`bun-types@1.3.12`, `@types/node@26.6.4`, `undici-types@8.9.0`). `bun add` first resolved
`@types/bun@1.4.2`, newer than the installed runtime (`bun --version` → `1.3.12`). It was pinned to
`1.3.12` so the types cannot describe APIs the runtime lacks.

effect 4 differs from the v3 names used in the specification's examples: decoding is
`Schema.decodeUnknownResult` (returns `Result`, not `Either`), `Schema.Union` takes an array, and
declarations live in `node_modules/effect/dist/*.d.ts` (there is no `dist/dts/`). Code follows the
installed declarations, not the examples.

## D-010 How the gate invokes commands (2026-10-06)

The gate spawns `process.execPath` with an explicit `cwd`, never `bun --cwd <dir> run <script>`.
Measured on this host: `bun --cwd "C:\AI Projects\faultline" run typecheck` printed the `bun run`
help and exited 0 without running `tsc`. A gate step built on that form would report a typecheck it
never performed. `bun run --cwd <dir> typecheck` ran `tsc --noEmit`.

## D-011 Row timeout and leftover grace (2026-10-06)

Each row gets 900000 ms. On timeout the gate kills the CLI with signal 9 and records the row as
failed (`gate-timeout`); a timeout never passes. The leftover-process re-check (3 attempts, 1000 ms
apart) only absorbs process-exit propagation delay. A process alive after that is a leak.

## D-012 Interruption is delivered with `proc.kill("SIGINT")` (2026-10-06)

A17 sends the signal exactly as the specification states. Measured on this host (Windows, Bun
1.3.12): `proc.kill("SIGINT")` ended a Bun child with exit code 130 and its SIGINT handler never ran.
The gate does not emulate Ctrl+C some other way, because that would test a different scenario
than the one specified. See OQ-001.

## D-013 A14's config mutation is in place, with a backup (2026-10-06)

A14 edits `profiles.disconnect.worlds` (+1) in the real `faultline.json`. The edit is acquired in a
scope whose finalizer restores the original bytes. Before editing, the gate writes the original to
`.gate/faultline.json.orig`; if the gate dies before the finalizer runs, the next gate run restores
from that file first and says so in its output. A sandbox copy was rejected: external-oracle
executables are referenced by relative path, so a copied config would also drift on
`oracles.external`, and A14 requires the report to name only `profiles`.

## D-014 The gate's hygiene checks were proven with positive controls before freezing (2026-10-06)

A detector that never fires passes everything, so both of the gate's leak checks were made to fire
once, with a temporary `src/cli.ts` that was deleted afterwards and never committed.

- Workspace check: the control created `<os.tmpdir()>/faultline-control` and slept 3000 ms. All 16
  rows that launched the CLI reported `workspaces left in TMP: faultline-control`. A17 reported no
  positive-control failure, so the per-row TMP redirect is honoured. Log:
  `gate-m0-leak-control.log` in the M0 agent's scratch directory.
- Process check, first attempt, vacuous: the control leaked a plain `Bun.spawn` child, and the gate
  reported no leftovers. A probe then measured why: on this host a non-detached Bun child dies when
  its parent exits normally (`childAliveAfter=false`) and when the parent is killed with signal 9
  (`false`); only a `detached: true` child survives (`true`). The control had leaked nothing.
- Process check, second attempt: with `detached: true`, all 16 CLI-launching rows reported
  `leftover processes after exit (killed by the gate)`, and afterwards zero leaked `bun.exe` children
  were alive.

Consequence for the runner: on Windows, a SUT started with a plain `Bun.spawn` does not outlive the
CLI, even when the CLI is killed with signal 9. The leftover check therefore matters most for
anything spawned detached, and on POSIX, where this was not measured.

## D-015 The gate's row checks now have a test that fires them, and five mutants of the gate were each caught (2026-10-06)

D-014 proved the hygiene detectors with one-off controls. `test/gate-rows.test.ts` replaces those
with a standing test: it builds a throwaway project under `.gate/fixture-<pid>`, copies the real
`scripts/gate.ts` into it, and runs that gate against a fake CLI that misbehaves in one known way
per row (A01 control; A02 exit code right by coincidence with no `verdict.json`; A03 `verdict.json`
exitCode disagrees with the process exit; A04 a PASS with a world judged by no oracle; A05 a
surviving detached process; A06 a workspace left in TMP). Covered rows: A01 to A06 only. Not
covered: the interrupt, tamper and config-bump setup paths, see OQ-005.

`FAULTLINE_GATE_SOURCE` points the test at a different gate file so a mutant can be exercised
without touching the frozen `scripts/gate.ts`. Rule F applied to the test itself: five mutants of
the gate were made in the M0 agent's scratch directory, each `String.Replace` was checked to have
changed the text, and `bun test --cwd C:\AI Projects\faultline gate-rows` was run against each.
Every run exited 1. The failing tests, from `rerun-full.log` (M1, M3, M4) and `mutation-run.log`
(M2, M5):

| Mutant | Gate change | Test(s) that failed |
|---|---|---|
| M1 | `leftoversOf(...)` call replaced by `[]` | A01 control, A05 |
| M2 | "expected exactly one new verdict.json" check removed | A01 control, A02 |
| M3 | "workspaces left in TMP" check removed | A01 control, A06 |
| M4 | empty-`verdicts` world check made `false` | A01 control, A04 |
| M5 | `verdict.exitCode === exit` made `true` | A03 |

The first pass filtered output to 14 lines, which cut the M2 listing after A02, so the status of
A03 to A06 under M2 was not captured. M5's listing was complete (7 pass, 1 fail, the fail being A03).
M1, M3 and M4 were re-run to list every test (`rerun-full.log`).

The real `scripts/gate.ts` was hashed before and after the five runs: both
`A9B90F558B4A624C844A98A8FD93D0C9162708A35E0B14E6FDA28D34989481BC`, equal to `GATE.lock`.

## D-016 M1 scope ruling on the oracle-soundness contradiction, and OQ-002 resolved (2026-10-06)

Two parts of the specification could not both be satisfied, and the author ruled on it. Quoted:
`<TYPESCRIPT_STANDARD>` lists "oracle soundness: histories generated from a correct reference model
give Ok; one seeded mutation (a duplicated append, a dropped result, a turn replayed after a crash, a
duplicated messageId) gives Violation ..." as a property test, and `Build in order` gives M1 "all
property tests from the standard. No processes, no sockets." while the oracles are built in M2
(`tool-pairing`), M3 (`edit-integrity`, `no-provider-retry-after-crash`) and M4
(`exactly-once-admission`, `quiescence`).

Ruling (author, 2026-10-06, answer "A"): M1 delivers the canonical-digest property, the
aggregation-monotonicity property, and the reference-model history generator. Each oracle-soundness
property lands in the milestone that builds that oracle. The generator is a test helper, so M1 adds
nothing under `src/` for it.

OQ-002 is resolved by measurement. The specification asks for "FastCheck exported by effect" and says
to confirm the export first. Effect 4.0.1 has no such export: no file under
`node_modules/effect/dist/*.d.ts` or `dist/testing/*.d.ts` mentions `fast-check` or `FastCheck`, and
`Test-Path node_modules/fast-check` printed `False`. It does export `Arbitrary`
(`node_modules/effect/dist/index.d.ts:33`) with `Arbitrary.schema`, `Arbitrary.all`,
`Arbitrary.map`, `Arbitrary.checkEffect`, `Arbitrary.formatCheckFailure` and
`CheckOptions { runs, size, maxDiscards, maxShrinks, seed, replay }`
(`node_modules/effect/dist/Arbitrary.d.ts`). The property tests use those. No dependency is added.

`MILESTONE` is set to `M1` by this entry.

## D-017 Details the specification leaves open, fixed in M1 (2026-10-06)

The specification names these shapes but not their exact form. Each is fixed in `src/` and tested in
`test/`; changing one is a change to this entry.

- Id patterns (`src/schema.ts`): `SessionId ^session-\d+$`, `WorldId ^world-\d+$`,
  `RunId ^run-[0-9a-z-]+$`, `Sha256 ^[0-9a-f]{64}$`, `ToolCallId ^call_[0-9a-f]+$`,
  `OracleName ^[a-z0-9][a-z0-9-]*$`, `MessageId` any non-empty string. Only `OpId ^op-\d+$` is given
  by the specification. Tested in `test/schema.test.ts` (brand patterns).
- `Seed` is a decimal string with no leading zero and a value of at most 2^64 - 1, so that
  `BigInt.asUintN` in the PRNG never truncates it. `HistoryEvent.value` is `Schema.Json`.
- History decoder (`src/history.ts`) refuses: a blank line, a line with an unknown field, a `t` lower
  than the previous event's, a second invoke for one id, a second completion for one id, an ok or
  fail with no earlier invoke, and a completion whose process or `f` differs from its invoke. `info`
  may complete an invoke or stand alone. A missing final newline is accepted.
- Fault grammar (`src/schedule.ts`): parentheses optional for kinds with no params; integers are
  canonical decimal only and at most 999999999999999; `afterChunks`, `ms` and `turn` are at least 1;
  `restartAfter`, `retryAfter` and `t` are at least 0; `status` is 429 or 500; no trimming.
- `InconclusiveReason` has 13 members: `oracle-crashed`, `oracle-nonzero-exit`, `oracle-timeout`,
  `oracle-malformed-output`, `missing-probe`, `unsupported-platform`, `history-unsplittable`,
  `no-oracle-judged`, `world-not-asserted`, `fault-missed-load`, `provider-not-exercised`,
  `narrowed-run`, `interrupted`.
- Config keys: `driver{cmd,envAllow}`, `profiles.<name>{worlds,sessions,turnsPerSession,
  minProviderTurns,quiesceMs,faults[],oracles[]}`, `oracles{builtin,external[{name,cmd}]}`,
  `oracle{timeoutMs}`, `faults{allow,deny}`, `budget{wallClockMs}`.
- Aggregation (`src/verdict.ts`): lock drift is exit 4 with empty lists; a world the plan called for
  that has no outcome, a plan that is not a positive safe integer, and a run of zero worlds each add a
  `world-not-asserted` entry; a bypassed lock or `narrowed` adds `narrowed-run`.
- Lock (`src/lock.ts`): an external oracle is bound as name, cmd and executable digest, sorted by
  name; locking when nothing moved returns the previous file unchanged; `from` is null for a first
  lock.

## D-018 History payload shapes are provisional and live in a test helper (2026-10-06)

`PromptValue`, `ProviderTurnValue`, `ToolCallValue` and `ToolResultValue` are defined in
`test/support/reference-model.ts`, not in `src/schema.ts`, because the specification types `value` as
unknown and no oracle reads them yet. They move to `src/schema.ts`, with a ledger entry, when
`tool-pairing` is built in M2. `test/reference-model.test.ts` checks that the generated histories
decode under the decoder's own rules and under these payload schemas.

## D-019 Mutation check of the M1 tests: 33 mutants, 33 caught (2026-10-06)

Method: `scratch/mut/run-m1-mutations.ps1`. For each mutant it replaces one literal string in one
`src/` file (the string must occur exactly once), runs the whole `bun test`, records the exit code and
the names of failing tests, and restores the file. A mutant counts as caught when `bun test` exits
non-zero. Result in `scratch/mut/m1-mutations.log`: 33 of 33 mutants gave `bun-exit=1`; the survivor
list printed `0`; and a SHA-256 comparison of every `src/*.ts` against the scratch source printed
`identical` for all seven files after the run.

| File | Mutants (each name is one change) |
|---|---|
| `canonical.ts` (3) | keys unsorted; non-finite numbers accepted; class instances accepted |
| `verdict.ts` (7) | budget and inconclusive swapped; missing-worlds entry dropped; drift falls through; budget outranks violation; bypassed lock not narrowed; no-oracle-judged dropped; world guards dropped |
| `schedule.ts` (4) | leading zeros accepted; excess param ignored; repeated param accepted; input trimmed |
| `history.ts` (6) | time regression allowed; duplicate completion allowed; completion mismatch allowed; excess field ignored; duplicate invoke allowed; blank line undetected |
| `lock.ts` (7) | budget digest reads another field; external oracles unsorted; executable digest not bound; history overwritten; empty reason accepted; unchanged keys recorded; duplicate external names accepted |
| `schema.ts` (6) | OpId accepts `op-`; Seed above 2^64 - 1; empty Violation witness; exit code 5 admitted; HTTP 404 admitted; Sha256 open-ended |

Limits: whether each mutant failed on the assertion meant for it, and not on an incidental one, was
checked only through the first failing test name in the log, which named the intended test in every
case. Mutants are a hand-picked set, not a generated one.

## D-020 Line endings are never converted by git (2026-10-06)

`.gitattributes` contains `* -text`. Reason: DEF-002. Every digest faultline checks
(`GATE.lock`, lock entries, world files) is over raw bytes, so a checkout must reproduce the committed
bytes exactly whatever the cloning machine's `core.autocrlf` is. This entry is written before the
re-measurement; the result is appended as the next entry, not edited into this one.
