// Package oracle is the property-verification engine and the six mandatory
// built-in oracles.
//
// Invariant I3 says the oracles are the product: fault injection is commodity,
// and what PRO-THESIS actually sells is a verdict a human can trust. Three
// rules follow from that, and every line in this package serves one of them.
//
// # I5: phase validity is enforced, not advisory
//
// Every oracle declares the lifecycle phases in which evaluating it is
// meaningful, and the engine REFUSES to evaluate one outside them
// (EvaluateOracleAt returns a *PhaseError). Asserting convergence during an
// active partition is a false-positive factory; an oracle asked to run outside
// its declared phases is an engine bug, so it fails loudly rather than
// producing a finding nobody can trust.
//
// The batch entry point (Engine.EvaluateAt) SELECTS the oracles valid in a
// phase and reports the rest as skipped. Selecting is not asking: skipping an
// oracle that does not apply is correct, whereas evaluating one that does not
// apply is the bug.
//
// The phase declarations themselves are not re-invented here. They come from
// schema.BuiltinOracle.ValidPhases and schema.BuiltinOracle.Class, which Phase 0
// froze; TestBuiltinsMatchSchemaDeclarations pins the agreement.
//
// # Inconclusive propagates; it never rounds down to PASS
//
// An oracle whose required evidence is absent returns INCONCLUSIVE, which maps
// to exit code 2 (retry once, then escalate) and never to 0. This is the single
// most important behaviour in the package. "The metric was missing so I passed"
// is how a chaos tool becomes decorative, and every built-in below has an
// explicit, tested inconclusive path:
//
//	no_crash                     no node state was collected
//	no_panic_log                 no logs were collected, or one could not be read
//	no_unbounded_queue           no queue-depth metric, or too few samples
//	resource_return_to_baseline  no baseline, or no post-QUIESCE sample
//	availability_after_heal      no probe was attempted in the convergence window
//	no_stuck_op                  no history, an unreliable history, no DRIVE
//	                             origin to convert its epoch timestamps, or
//	                             invokes that cannot be paired
//
// An empty Findings set maps to INCONCLUSIVE for the same reason: a run in
// which nothing was checked has not passed.
//
// # The engine never adopts the weaker reading
//
// An oracle that panics, malfunctions, or returns a status the contract does
// not define is recorded as INCONCLUSIVE with the reason attached, never as
// OK. A violation reported without an explanation keeps its violation and gains
// a note; findings are never downgraded.
//
// # External oracles (Phase 3)
//
// An external oracle is a separate executable honouring directive 4.5's
// stdin/stdout contract. ProcessOracle implements the same Oracle interface as
// the six built-ins and registers into the same Engine, so nothing downstream
// (the verdict, result.json, the exit-code mapping) can tell the two kinds
// apart. That is the point: an external oracle's inconclusive has to reach
// exit 2 by exactly the path a built-in's does, or the two would drift and only
// one of them would be trustworthy.
//
//	external.go   the prothesis.oracle_def/v1 definition file, and discovery
//	              over oracles.dir — deterministic, sorted by oracle name, and
//	              an ERROR on a definition that does not parse
//	process.go    execution: the input document on stdin, a BOUNDED read of
//	              stdout, stderr captured into the run bundle, the timeout with
//	              a process-TREE kill, and the interpretation rules
//	scaffold.go   what `thesis init` writes into an empty oracles directory
//
// Two rules in process.go carry the weight, and both are fail-closed:
//
//   - The exit code and the reported status must AGREE. A disagreement is a
//     defect in the oracle, so NEITHER reading is adopted: the finding is
//     inconclusive and says what each channel said. This is deliberately
//     stricter than schema.DecodeOracleOutput's "worse of the two", because
//     taking the worse manufactures a `violated` out of an oracle bug and a
//     false positive is the worst thing this tool can produce (D-038).
//   - The class and the valid phases come from the LOCK-COVERED definition,
//     never from the oracle's own document. Severity is derived from the class,
//     so an oracle that named its own would be grading its own finding; and I5
//     makes the ENGINE responsible for phase filtering, which it cannot do with
//     a declaration it only learns after running the process.
//
// Note that violations[].phase and oracle.valid_phases are DIFFERENT fields and
// are never checked against each other (OQ-004). An ASSERT-only consistency
// oracle reporting evidence that lies in DRIVE is the fixture's stale read, not
// a defect. The format reference is docs/ORACLE_DEFINITIONS.md.
//
// # Phase scope
//
// The .prothesis/lock manifest lives in internal/lock. Coverage and search are
// Phase 4; shrinking and replay are Phase 5.
//
// # A note on the option structs
//
// Every knob in Options (the SLO ceiling, the resource tolerance, the panic
// pattern set) is a gate-weakening surface under invariant I6: widening a
// tolerance or deleting a pattern silences an oracle without touching a line of
// oracle code. Options.Fingerprint exists so the Phase 3 lock manifest can cover
// them. See OPEN_QUESTIONS.md OQ-019.
package oracle
