// Package saboteur implements the PURE, DETERMINISTIC core of Addendum A's
// adversarial search agent: the payoff function (A.6), the escalation ladder
// (A.7) and the telemetry spiral detector (A.4).
//
// Nothing here executes a world, touches Docker, or reads a file. Every
// function is a total function of its arguments plus a path-keyed PRNG stream,
// which is what makes the Saboteur's decisions reproducible under A.8 and what
// makes this package testable without a container runtime.
//
// # What this package is NOT
//
// It does not own the corpus, the coverage signal or the strategy interface:
// those live one directory up in `internal/search`. It executes nothing: no
// Docker, no compose project, no container, no goroutine, so it cannot leak a
// bridge network. The loop that DOES execute worlds is `internal/search/engine`,
// which is the only package depending on both this one and `internal/control`.
//
// `strategy.go` is the single exception to the purity above: it is the seam that
// implements `search.Strategy`, and it is the only file here that imports
// `internal/search`. Everything else is a total function of its arguments plus a
// path-keyed PRNG stream, which is what makes it testable without a container
// runtime.
//
// # The one thing to internalise before reading further (D-015, OQ-013)
//
// At the budgets Addendum A states (roughly 16 serial rollouts against a root
// branching factor near 249) UCT is PROVABLY EQUIVALENT to ladder-ordered
// enumeration. The exploration term is infinite at zero visits, so an unvisited
// child is always selected before any visited one; no node is ever visited
// twice; backpropagation never influences a decision; and A.5's own pruning
// rule ("visited >= 3 times") is unreachable. The tree becomes load-bearing
// only when parallelism raises the rollout count (~62 at 4-way).
//
// So the ESCALATION LADDER in ladder.go and early termination are what actually
// carry this search. The tree is a tie-breaker. That is stated here, in the
// code, rather than implied otherwise, so nobody later tunes an exploration
// constant that cannot matter.
//
// # Three soundness properties, and why each is here
//
//  1. A CLEAN WORLD AND AN UNEVALUABLE ONE MUST NOT LOOK ALIKE. If the
//     classifier reports DAMPEN when it simply had no data, the utility signal
//     is corrupt and every ranking downstream is meaningless. observe.go
//     therefore has a fourth label, INSUFFICIENT_DATA, which is NOT DAMPEN and
//     never contributes a reinforcing signal.
//
//  2. ABSENT IS NOT ZERO. internal/telemetry deliberately keeps an unobservable
//     metric ABSENT rather than reporting it as 0 (see that package's doc
//     comment). A classifier that read absence as zero would see a cliff and
//     report REINFORCE on a healthy system. Series carries absences explicitly
//     and they never enter the regression.
//
//  3. A SLOPE IS NOT A SIGNAL UNTIL IT BEATS ITS OWN NOISE. A.4's
//     `if trend > 0` classifies pure sensor noise as REINFORCE about half the
//     time. observe.go additionally requires the fitted slope to exceed a
//     multiple of its own standard error. This is a strengthening, recorded as
//     a deviation, and pinned by a test that shows the unguarded rule would
//     have fired on the same series.
//
// # Determinism
//
// Where a genuine tie exists, which of several equal-prior node targets to
// try first, it is broken by drawing from a stream derived at a FIXED PATH,
// `recorder.MustDeriveKey(seed, "search.ladder", <rung>)`. Because stream keys
// are path-keyed (D-013), adding a rung, or drawing more values inside one
// rung, cannot perturb any other rung's draws or any stream that already
// existed. Map iteration is never used to order anything.
package saboteur
