package saboteur

import (
	"math"

	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// ---------------------------------------------------------------------------
// A.5's selection policy, in its STANDARD form
//
// Addendum A.5 writes:
//
//	UCT(node) = (U_avg / visits) + C * sqrt(ln(parent.visits) / visits)
//
// while defining U_avg as "average utility across all rollouts through this
// node". Dividing an average by the visit count a SECOND time drives the
// exploitation term toward zero exactly as a node accumulates evidence, which
// is the opposite of what UCT does: no bandit algorithm penalises an arm for
// having been sampled. DECISIONS.md D-028 item 1 records the reading taken here:
//
//	UCT(node) = U_avg + C * sqrt(ln(parent.visits) / visits)
//
// This file implements that, and TestTheUCTFormulaIsTheStandardOne pins the
// difference so nobody "restores" the addendum's transcription slip.
//
// # The thing to internalise before tuning anything here (D-015, OQ-013)
//
// At Addendum A's stated budgets the exploration term is +Inf at zero visits, so
// an unvisited child is ALWAYS selected before any visited one. With ~16 serial
// rollouts against a root branching factor near 249, no node is ever visited
// twice, U_avg is never read, and C cannot change a single decision. UCT is
// then provably equivalent to ladder-ordered enumeration.
//
// That is not a reason to fake a tree. It is a reason to MEASURE whether the
// tree contributed, which is what SelectionKind exists for: every selection
// records whether it was forced (an unvisited child existed) or INFORMED (every
// child had been visited, so the scores actually decided). A run whose informed
// count is zero is a run in which the tree was a tie-breaker and the ladder did
// the work: a finding to report, not a failure to hide.
// ---------------------------------------------------------------------------

// DefaultExploration is A.10's literal 1.41, matching
// schema.DefaultExplorationConstant. It is deliberately NOT math.Sqrt2: the two
// differ in the third decimal place, and every UCT comparison downstream is a
// float comparison, so substituting sqrt(2) would silently diverge replays from
// any run recorded against the documented default.
const DefaultExploration = schema.DefaultExplorationConstant

// PruneVisits is A.5's pruning threshold: "if a node has been visited >= 3
// times with zero coverage delta and zero violations, prune it".
//
// It is implemented exactly as written and is UNREACHABLE at Addendum A's own
// budgets, for the reason above: a node is never visited twice, let alone
// three times. It becomes reachable only when parallelism raises the rollout
// count far beyond the ~62 D-022 measures. Tree.Stats reports how many nodes
// ever reached it, so the claim is measured rather than assumed.
const PruneVisits = 3

// SelectionKind records WHY a child was chosen, which is the only honest way to
// say whether the tree influenced anything.
type SelectionKind string

const (
	// SelectionForced means at least one child had never been visited, so the
	// infinite exploration term decided and the ladder's ORDER chose. U_avg was
	// not read. This is the only kind that occurs at Addendum A's budgets.
	SelectionForced SelectionKind = "forced"
	// SelectionInformed means every child had been visited at least once, so
	// the UCT scores (and therefore backpropagated utility) actually decided.
	SelectionInformed SelectionKind = "informed"
	// SelectionNone means there was nothing to choose between.
	SelectionNone SelectionKind = "none"
)

// UCT scores one child.
//
// visits == 0 returns +Inf, which is what makes an unvisited child unbeatable
// and what makes the D-015 equivalence hold. parentVisits < 1 is treated as 1 so
// ln() is never taken of a non-positive number; at the root's first expansion
// every child is unvisited anyway, so the branch is unreachable in practice and
// is here to make the function total.
func UCT(avgUtility float64, visits, parentVisits int, c float64) float64 {
	if visits <= 0 {
		return math.Inf(1)
	}
	if parentVisits < 1 {
		parentVisits = 1
	}
	if c < 0 {
		c = 0
	}
	return avgUtility + c*math.Sqrt(math.Log(float64(parentVisits))/float64(visits))
}

// AverageUtility is U_avg: total backpropagated utility over visits.
//
// Zero visits returns 0, which is never read (UCT short-circuits to +Inf
// first) but returning NaN here would poison any diagnostic that printed it.
func AverageUtility(total float64, visits int) float64 {
	if visits <= 0 {
		return 0
	}
	return total / float64(visits)
}

// betterUCT is the selection comparison, with A.5's parsimony tie-break.
//
// A.5: "Select the child with the highest UCT score. On ties, prefer the child
// with fewer faults (parsimony bias)." The tie-break matters more than it looks:
// at zero visits EVERY child scores +Inf, so without a deterministic secondary
// order the winner would be whichever child the slice happened to hold first,
// which is fine, because the ladder built that slice in rung order and that
// order IS the prior. So the final tie-break is the candidate's position, and
// the ladder's ordering is what decides. Never a map, never a float comparison
// that could be fused differently on another architecture.
//
// It reports whether a beats b.
func betterUCT(aScore float64, aFaults, aIndex int, bScore float64, bFaults, bIndex int) bool {
	// +Inf == +Inf compares equal, which is exactly right: two unvisited
	// children are tied and fall through to parsimony and then to ladder order.
	if aScore != bScore {
		return aScore > bScore
	}
	if aFaults != bFaults {
		return aFaults < bFaults
	}
	return aIndex < bIndex
}
