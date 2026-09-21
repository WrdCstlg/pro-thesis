package shrink

import (
	"context"
	"testing"
)

// D-063 (amended). The panel's contract for calibration: a REDUCED candidate may
// raise the drift question, but only an execution of the UNREDUCED world may
// answer it.
//
// This is the attack stages.go guardrail 3 originally promised was impossible.
// The unreduced world reproduces the target on its own key on EVERY execution:
// stage 0's two draws, and any re-probe the run may later make. The only drift
// the run ever sees comes from reduced candidates that have DROPPED the
// essential fault and fire the same oracle and class on another key: a second
// mechanism, not the target. If that evidence is allowed to relax the level,
// the wrong reduction is accepted, narrowed around, and certified k/k with a
// Calibration note asserting "the same defect reproduced".
//
// The assertion is therefore exact: the level never moves, because nothing the
// unreduced world did justified moving it.
func TestReducedCandidatesMayAskButOnlyTheUnreducedWorldMayAnswer(t *testing.T) {
	full := faultList(4)
	fake := newFake(func(c Candidate, nth int) Attempt {
		if len(c.Faults) == len(full) {
			// The unreduced world holds its key on every draw, however many
			// times it is asked.
			return reproduced()
		}
		if !hasFault(c, full[0]) {
			// Dropped the essential fault; something ELSE fires the same
			// oracle+class on a different key. This must stay a divergence.
			return reproducedAs(sameBugDifferentKey)
		}
		return clean()
	})

	res, err := Run(context.Background(), Options{
		Executor: fake,
		Original: origID, // key k/0
		Faults:   full,
		Policy:   Policy{SkipNarrowing: true, ConfirmK: 3},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if res.Calibration != "" {
		t.Fatalf("the identity level was relaxed on evidence that came ONLY from reduced "+
			"candidates; the unreduced world held key %q on every execution and never "+
			"justified it. Calibration note: %s", origID.Key, res.Calibration)
	}
	if res.Policy.Strictness != MatchWitnessKey {
		t.Fatalf("Policy.Strictness = %s, want %s: a reduced candidate answered the drift "+
			"question that only the unreduced world may answer", res.Policy.Strictness, MatchWitnessKey)
	}
	// Corollary: whatever survived must still carry the essential fault, or the
	// run must have reduced nothing at all. A survivor without full[0] is the
	// wrong bug wearing the target's name.
	surviving := Candidate{Faults: res.SurvivingFaults}
	if !hasFault(surviving, full[0]) && res.FaultsAfter < res.FaultsBefore {
		t.Fatalf("the surviving faults %v dropped the essential fault %q: a different mechanism "+
			"was accepted as a reduction of the target (confirmation %s)",
			res.SurvivingFaults, full[0], res.Confirmation)
	}
}
