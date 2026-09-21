// Package shrink is the Phase 5 minimization pipeline.
//
// It turns a violating world into the smallest world that still reproduces THE
// SAME violation, and it reports honestly when it could not finish.
//
// # The four stages (directive section 6, Phase 5)
//
//  1. ddmin over the FAULT LIST: remove faults, re-execute, keep the removal
//     when the violation persists (stages.go, ddmin.go).
//  2. ddmin over the OPERATION trace: narrow the workload to the minimal op
//     sequence, through the {plan_path} seam D-021 reserved (ops.go).
//  3. TIMING NARROWING: binary-search each surviving fault window toward its
//     shortest reproducing form (narrow.go).
//  4. CONFIRMATION: the shrunk world must reproduce k/k, default 3/3
//     (confirm.go), and what is REPORTED is the measured k/n.
//
// # What this package deliberately does not own
//
// It does not execute worlds. Execution is the Executor seam (candidate.go),
// implemented by whoever owns the harness: internal/control's parallel
// executor (D-043) for a real shrink, a pure function for the algorithm tests.
// That is not a testing convenience: it is what lets ddmin's correctness be
// pinned WITHOUT Docker, and it keeps the ~24-bridge-network ceiling under the
// control of the one component that already computes it (control.NetworkBudget).
//
// It does not write .thesis files, does not touch .prothesis/regressions/, and
// does not run `thesis replay`. Committing a shrunk world is a lock-adjacent act
// (D-004) and belongs to the replay/regress owner; this package hands it a
// Result and a Confirmation and lets it decide.
//
// It does not import internal/control. control will drive a shrink, so the
// dependency would be a cycle, and the Executor seam already carries everything
// that crosses the boundary.
//
// # The three rules this package is built to satisfy
//
// RULE 1: AN ACCEPTED REDUCTION MUST REPRODUCE THE SAME VIOLATION.
// This is the ranked #1 way Phase 5 produces something worse than nothing: ddmin
// drops a fault, the world still fails, the reduction is accepted, and the
// surviving faults now trigger a DIFFERENT bug. The "minimal repro" then
// reproduces something else, and an agent reading it fixes the wrong thing.
// So a candidate is never judged by "did something fail". It is judged against
// the ORIGINAL violation's Identity (identity.go), and a candidate that fails a
// different oracle is SignalDifferent: a rejection, recorded as a divergence,
// never a successful reduction.
//
// RULE 2: CONFIRMATION APPLIES TO ACCEPTANCE, NOT ONLY TO THE FINAL ANSWER.
// Tier B replay is probabilistic; the spec concedes it. The asymmetry is the
// whole design (probe.go):
//
//	ONE reproduction PROVES the candidate still fails       -> accept on 1
//	ONE non-reproduction proves almost nothing              -> reject on N (2)
//
// So the retries are spent on the REJECTION side, where a wrong answer keeps a
// fault that does not matter. An UNKNOWN execution (the harness failed, the
// world never reached DRIVE) is neither, and is never silently counted as a
// rejection.
//
// RULE 3: THE BUDGET IS EXPLICIT AND EXPIRY IS REPORTED (D-029).
// A world costs ~30s wall. Naive ddmin over 14 faults, then 20,000 ops, then
// window narrowing, then 3x confirmation is 9-52 HOURS, which contradicts I7.
// So shrinking takes a wall AND world ceiling (budget.go), stops where it is,
// and emits a PARTIAL shrink: attempted true, the honest before/after counts,
// the surviving faults reached so far, and a StopReason naming what ran out.
// 14 faults down to 6 is useful; an unbounded shrink that never returns is not.
// The confirmation gate is RESERVED out of the world budget up front, so the
// pipeline can never spend itself into a position where it cannot measure the
// number it is about to publish.
//
// # The fourth failure mode, and where it is closed
//
// A REPRO THAT DOES NOT REPRODUCE poisons every future gate run with a
// permanent red nobody can fix. Two guards: the baseline probe runs FIRST and
// stops the whole pipeline when the unreduced world does not reproduce
// (StopBaselineNotReproduced), and Result.MinimalRepro returns nil at 0/n rather
// than naming a world that replayed zero times. A measured 2/3 IS emitted, as
// "2/3", with Confirmation.Passed() false: the caller must not auto-commit it.
//
// # Determinism
//
// ddmin's answer must not depend on scheduling. Candidates are tested in ROUNDS
// (one trial each, in index order, batched), and the accepted candidate is the
// LOWEST-INDEX one that reproduced in the earliest round, never the first to
// finish. So the same inputs give the same shrink at any Parallelism, which is
// the same guarantee D-044 established for world seeds and is pinned by
// TestTheSameShrinkAtAnyParallelism.
package shrink
