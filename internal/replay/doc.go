// Package replay executes a recorded `.thesis` world again and reports, without
// rounding, whether it reproduced.
//
// It is the consuming half of Phase 5: internal/shrink produces a small world,
// this package proves it still fails, and internal/corpus commits it only if it
// does. `thesis replay`, `thesis regress` and `thesis bisect` all execute
// through the Executor seam defined here.
//
// # THE REALIZED SCHEDULE IS WHAT REPLAYS (D-012)
//
// A world's `fault_schedule` has two halves and they are not interchangeable.
// `planned` is what was asked for; `realized` is what happened, against WHICH
// CONCRETE NODES. A planned `proc.pause(role:leader)` re-resolves at injection
// time from live cluster state, and Raft leadership moves, so replaying the
// PLAN can address a different node and quietly test a different world. The
// realized record names `kv-n2`, which is what makes invariant I2's
// "self-contained file that reproduces the issue" true rather than aspirational.
//
// ChooseSchedule prefers realized whenever it is present, reports which half it
// took, and reports the residue honestly:
//
//   - a realized target the frozen TARGET grammar cannot pin (a quorum or a
//     multi-node wildcard keeps its planned form; only Nodes carries the
//     binding) is listed as UNPINNED, because that fault WILL re-resolve on
//     replay and the reproduction rate is correspondingly weaker;
//   - `realized: null` means the world has NEVER BEEN EXECUTED, which is a
//     different statement from `realized: []`. The plan is then all there is,
//     and the report says so rather than implying a fidelity it does not have;
//   - `realized: []` with a non-empty plan means the world executed and injected
//     NOTHING. Replaying the plan would be replaying a different world, so the
//     empty realized schedule wins and the discrepancy is stated loudly.
//
// # TIER B: k/n IS REPORTED, NEVER ROUNDED
//
// The specification concedes that replays reproduce with high probability, not
// certainty, and requires the verdict to say `3/3` or `1/5` and never claim
// determinism it does not have. So a replay counts three outcomes, not two:
//
//	reproduced      the SAME violation fired
//	not reproduced  the world ran to a verdict and that violation did not
//	unknown         nobody could tell — the harness failed, an oracle could not
//	                evaluate, the run was cancelled
//
// An unknown is never folded into either of the other two. Counting it as a
// reproduction manufactures evidence; counting it as a non-reproduction
// manufactures flakiness, and under a confirmation gate that is the difference
// between committing a world and refusing it.
//
// # THE SAME VIOLATION, NOT ANY VIOLATION
//
// A replay that fails a DIFFERENT oracle has not reproduced anything. It is
// recorded as a DIVERGENCE and reported by name, because "the world still fails"
// and "the world still fails the way it used to" are different claims and only
// the second licenses a minimal repro. The comparison uses internal/shrink's
// Identity and Strictness so that the shrink pipeline and the replay that
// confirms its answer share ONE definition of sameness: two definitions would
// make a confirmed world one the shrink never actually produced.
//
// When no expected identity is supplied, ANY violation counts and its identity
// is reported. That is the right rule for `thesis regress`, whose corpus worlds
// carry no recorded violation: a committed regression that fails again is a
// finding whichever oracle makes it.
//
// # BUDGETS (I7, D-029)
//
// A world costs ~30s wall. Every entry point here takes an explicit budget,
// stops where it is, and reports what it did not get to. `thesis regress` runs
// on every gate invocation forever; silently running a subset of the corpus and
// reporting PASS would be the vacuous pass this project is ranked against.
package replay
