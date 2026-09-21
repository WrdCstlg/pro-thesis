package saboteur

import (
	"math"
	"testing"
)

// ---------------------------------------------------------------------------
// A.5's UCT formula, and the transcription slip in it
// ---------------------------------------------------------------------------

// A.5 writes `UCT = (U_avg / visits) + C * sqrt(...)`, which divides an average
// by the visit count a SECOND time. D-028 item 1 records the reading taken here.
// This test pins the difference numerically so nobody "restores" the addendum's
// text without a failure in their face.
func TestTheUCTFormulaIsTheStandardOne(t *testing.T) {
	const (
		avg     = 120.0
		visits  = 4
		parent  = 16
		explore = 1.41
	)
	want := avg + explore*math.Sqrt(math.Log(parent)/visits)
	got := UCT(avg, visits, parent, explore)
	if math.Abs(got-want) > 1e-12 {
		t.Fatalf("UCT = %v, want the standard form %v", got, want)
	}

	// The addendum's literal form, for contrast. It must NOT be what we compute.
	addendum := avg/visits + explore*math.Sqrt(math.Log(parent)/visits)
	if math.Abs(got-addendum) < 1e-9 {
		t.Fatalf("UCT matches A.5's literal (U_avg / visits) form. That divides an average by "+
			"visits a second time, so a node's exploitation term SHRINKS as it accumulates "+
			"evidence — the opposite of what a bandit does. got %v, addendum form %v",
			got, addendum)
	}
}

// A node that proves itself must not be penalised for having been sampled. This
// is the property the addendum's form inverts, stated as behaviour rather than
// as arithmetic.
func TestMoreEvidenceDoesNotShrinkAGoodNodesScore(t *testing.T) {
	// Same average utility, one visit versus twenty.
	few := UCT(100, 1, 100, DefaultExploration)
	many := UCT(100, 20, 100, DefaultExploration)
	if many < 100 {
		t.Fatalf("a well-sampled node with average utility 100 scored %v, below its own average; "+
			"the exploitation term has been divided away", many)
	}
	if few <= many {
		// Sanity: exploration still favours the less-visited node, which is the
		// only thing the second term is for.
		t.Fatalf("the exploration term does not favour the less-visited node: few=%v many=%v", few, many)
	}
}

// The infinite term at zero visits is what makes D-015's equivalence hold, and
// it must be exactly infinite rather than merely large: a large finite value
// could be beaten by a high-utility visited sibling, and then a child would be
// skipped without ever being tried.
func TestAnUnvisitedChildIsUnbeatable(t *testing.T) {
	unvisited := UCT(0, 0, 1000, DefaultExploration)
	if !math.IsInf(unvisited, 1) {
		t.Fatalf("an unvisited child scored %v, not +Inf; a sufficiently good visited sibling "+
			"could then be selected before a child that has never been tried", unvisited)
	}
	// Even against an enormous measured utility.
	if !(unvisited > UCT(1e12, 5, 1000, DefaultExploration)) {
		t.Fatal("an unvisited child did not outrank a visited one with utility 1e12")
	}
}

// UCT must be total. A parent with zero recorded visits is reachable in a
// resumed or hand-built tree, and ln(0) would be -Inf.
func TestUCTIsTotal(t *testing.T) {
	for _, tc := range []struct{ visits, parent int }{{1, 0}, {1, -3}, {3, 1}} {
		v := UCT(10, tc.visits, tc.parent, DefaultExploration)
		if math.IsNaN(v) || math.IsInf(v, -1) {
			t.Fatalf("UCT(10, %d, %d) = %v; the score must stay a usable number",
				tc.visits, tc.parent, v)
		}
	}
	if v := UCT(10, 2, 8, -1); math.IsNaN(v) {
		t.Fatal("a negative exploration constant produced NaN rather than being clamped")
	}
}

// A.5: "On ties, prefer the child with fewer faults (parsimony bias)."
// At zero visits EVERY child scores +Inf, so without this the winner would be
// whichever the slice happened to hold first, and the final fallback must be
// that position, because the ladder built the slice in rung order and that order
// IS the prior.
func TestTiesBreakOnParsimonyThenLadderOrder(t *testing.T) {
	inf := math.Inf(1)
	if !betterUCT(inf, 2, 5, inf, 3, 1) {
		t.Fatal("a tie was not broken toward the schedule with fewer faults")
	}
	if betterUCT(inf, 3, 1, inf, 2, 5) {
		t.Fatal("the parsimony tie-break is not antisymmetric")
	}
	// Equal score AND equal fault count: ladder position decides.
	if !betterUCT(inf, 2, 0, inf, 2, 1) {
		t.Fatal("a full tie did not fall back to ladder order")
	}
	if betterUCT(inf, 2, 1, inf, 2, 0) {
		t.Fatal("the ladder-order fallback is not antisymmetric")
	}
	// A genuinely higher score beats parsimony: parsimony is a TIE-break, not a
	// weight. Making it a weight would stop the search escalating.
	if !betterUCT(200, 5, 9, 100, 1, 0) {
		t.Fatal("parsimony overrode a strictly higher UCT score; it is a tie-break only")
	}
}

func TestAverageUtilityIsTotalAtZeroVisits(t *testing.T) {
	if v := AverageUtility(0, 0); v != 0 {
		t.Fatalf("AverageUtility(0,0) = %v; a NaN here would poison every diagnostic that "+
			"printed a node's average", v)
	}
	if v := AverageUtility(10, 4); v != 2.5 {
		t.Fatalf("AverageUtility(10,4) = %v, want 2.5", v)
	}
}

// The pruning threshold is A.5's, transcribed. It is unreachable at Addendum A's
// own budgets and that is a documented fact, not a bug, but the CONSTANT must
// still be what the addendum says, or the code and the spec have quietly parted.
func TestPruneVisitsIsTheAddendumsThreshold(t *testing.T) {
	if PruneVisits != 3 {
		t.Fatalf("PruneVisits = %d; A.5 says \"visited >= 3 times\"", PruneVisits)
	}
}

// A.10's default is the LITERAL 1.41, not math.Sqrt2. They differ in the third
// decimal and every UCT comparison downstream is a float comparison, so
// substituting sqrt(2) would silently diverge replays from any run recorded
// against the documented default.
func TestTheExplorationDefaultIsTheDocumentedLiteral(t *testing.T) {
	if DefaultExploration != 1.41 {
		t.Fatalf("DefaultExploration = %v, want the literal 1.41", DefaultExploration)
	}
	if DefaultExploration == math.Sqrt2 {
		t.Fatal("DefaultExploration is math.Sqrt2; A.10 writes 1.41 and the two differ")
	}
}
