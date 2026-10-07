# Open questions

One line per item, measured where possible. Scope-fence items (rule J) go here instead of into code.

- OQ-001 (2026-10-06): On Windows, Bun 1.3.12's `proc.kill("SIGINT")` ended a Bun child (exit 130) without running its `process.on("SIGINT")` handler (probe marker file stayed empty). A17 needs faultline to observe SIGINT and tear down; on this host it may be unable to. Unresolved; expected to surface at M5.
- OQ-002 (2026-10-06) RESOLVED by D-016: `node_modules/effect/dist/index.d.ts` (effect 4.0.1) has no `FastCheck` export; `Arbitrary` is used instead. 
- OQ-003 (2026-10-06): A14's "zero ports opened" is inferred (no workspace, no world file, no surviving process; D-007), not observed at the socket level.
- OQ-004 (2026-10-06): `typescript@7.0.2` installs a native `tsc.exe`. Whether its diagnostics match the JS compiler's for every option in `tsconfig.json` has not been checked; the gate trusts its exit code.
- OQ-005 (2026-10-06): The gate's row runner is covered by test/gate-rows.test.ts for rows A01 to A06 only (verdict-count, exit-code consistency, empty-verdict PASS, leftover process, leftover workspace; five gate mutants caught, D-015). Still untested: the interrupt, tamper and config-bump setup paths. Its first real exercise against faultline output comes at M2 (A01).
- OQ-006 (2026-10-06): Process survival after parent death was measured only on Windows (D-014). On POSIX, `Bun.spawn` children are expected to be re-parented, not killed; not measured.
- OQ-007 (2026-10-06): The specification names `oracle.timeoutMs` but does not list it among the seven locked keys, so changing it moves no lock entry (tested in test/lock.test.ts), although rule F counts widening a timeout as weakening a check.
- OQ-008 (2026-10-06): A typo'd key in `faultline.json` must not be silently ignored; the config schema does not enforce this by itself, so config decoding must pass `onExcessProperty: "error"` (to be done where the config is read, M2 or M5).
- OQ-009 (2026-10-06): Profile `faults[]` entries stay strings in the config schema and are validated by `parseFaults` outside it, so turning a bad entry into exit 64 is wiring for M5, not yet done.
