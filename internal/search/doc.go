// Package search is the generic search substrate for Phase 4.
//
// It owns four things and deliberately nothing else:
//
//  1. COVERAGE SIGNALS: log-template coverage (template.go) and
//     state-abstraction coverage (state.go), combined in coverage.go. These are
//     the "input signals to the Saboteur" that Addendum A's preamble keeps in
//     force from the base Phase 4 spec.
//  2. THE CORPUS: an energy-weighted archive of worlds with provenance
//     (corpus.go), selection drawn from the recorder's path-keyed PRNG.
//  3. THE STRATEGY INTERFACE and the uniform-random BASELINE (strategy.go,
//     random.go). D-029 pre-registers a Fisher's exact comparison of the
//     Saboteur against this baseline, so the baseline is evidence, not filler.
//  4. THE FAULT SPACE and MUTATION OPERATORS (space.go, mutate.go), every
//     product of which is validated through internal/perturber before it is
//     returned.
//
// It does NOT own the Saboteur. PROBE, OBSERVE, ESCALATE, the escalation
// ladder, the MCTS tree and Addendum A.6's utility function live in
// internal/search/saboteur and are another agent's deliverable. Nothing in this
// package computes A.6's utility: the Outcome type carries every field A.6
// reads (violation class, severity, coverage delta, fault count, duration) so
// the Saboteur can, but the payoff function itself is not defined here.
//
// # What this package is honest about
//
// D-015 and OQ-013 settle a fact this package must not contradict anywhere: at
// Addendum A's stated budgets (~16 serial rollouts against a root branching
// factor near 249) UCT is provably equivalent to ladder-ordered enumeration.
// The escalation ladder and early termination carry the search; the tree is a
// tie-breaker until parallelism raises the rollout count. Nothing here pretends
// otherwise, and nothing here is tuned as though an exploration constant could
// matter at 16 rollouts.
//
// # The failure this package is built against
//
// The ranked #1 way Phase 4 fails is A SEARCH THAT LEARNS FROM NOISE: if a
// clean world is indistinguishable from an unevaluable one, every ranking is
// meaningless. OQ-033 makes that concrete: a `linear` world's best achievable
// verdict is INCONCLUSIVE by construction, because no_stuck_op cannot finish
// observing operations the harness stopped. A search that reads "no violation"
// off such a world has learned nothing and believes it learned something.
//
// So Signal is a THREE-valued type (violated / clean / unknown), never a bool;
// Outcome.Observed records whether the world reached DRIVE at all; and
// Corpus.Add refuses an unobserved world outright. See outcome.go.
//
// # Import direction
//
// This package imports pkg/schema, internal/recorder, internal/perturber and
// internal/telemetry. It does NOT import internal/control, because control is
// what will drive a search and the dependency would be a cycle. Log lines
// therefore arrive as (node, text) pairs: Phase 1's collector already parses
// the docker timestamp prefix into control.LogLine.Text, and the caller passes
// that text straight in. Nothing here re-implements log collection.
//
// # Determinism
//
// Addendum A.8 requires that the same seed expand the same tree. Every random
// decision in this package is drawn from a recorder Stream keyed by an explicit
// PATH (rand.go). No map iteration order, no math/rand, and no float
// arithmetic in any selection weight: weights are integers and selection goes
// through recorder.Stream.WeightedPPM, which is already the frozen
// deterministic weighted selector.
package search
