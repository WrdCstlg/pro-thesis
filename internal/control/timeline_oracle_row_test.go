package control

import (
	"testing"

	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// phasesForRowTest is a realistic measured lifecycle: DRIVE starts at 0, and
// ASSERT, where an external consistency oracle is evaluated, begins nearly
// nineteen seconds later. Those two numbers are what the whole question turns
// on.
func phasesForRowTest() schema.PhaseTimings {
	return schema.PhaseTimings{
		{Phase: schema.PhaseBoot, StartMS: -14001, EndMS: -185},
		{Phase: schema.PhaseSeed, StartMS: -185, EndMS: 0},
		{Phase: schema.PhaseDrive, StartMS: 0, EndMS: 1},
		{Phase: schema.PhasePerturb, StartMS: 1, EndMS: 13887},
		{Phase: schema.PhaseHeal, StartMS: 13887, EndMS: 16701},
		{Phase: schema.PhaseQuiesce, StartMS: 16701, EndMS: 18809},
		{Phase: schema.PhaseAssert, StartMS: 18809, EndMS: 18809},
	}
}

func oracleRowOf(t *testing.T, evs []schema.TimelineEvent) schema.TimelineEvent {
	t.Helper()
	var found []schema.TimelineEvent
	for _, e := range evs {
		if e.Event == schema.TimelineOracle {
			found = append(found, e)
		}
	}
	if len(found) != 1 {
		t.Fatalf("expected exactly one oracle row in the causal timeline, got %d", len(found))
	}
	return found[0]
}

// An external oracle emits only the frozen witness shape ({"op_ids", "key"})
// and therefore supplies no first_seen_ms. The runner correctly refuses to
// invent an observed PHASE from that missing number and pins the phase the
// oracle was evaluated in. The timeline must not then undo that care by
// emitting the row at t_ms 0, which is DRIVE start: that places the finding
// ahead of every fault on the timeline a human reads first.
//
// This is the fixture's definition-of-done shape exactly: a consistency
// violation found by an ASSERT-only oracle over a history whose evidence lies
// in PERTURB.
func TestAFindingWithNoTimingIsNotPlacedAtDriveStart(t *testing.T) {
	src := TimelineSource{Phases: phasesForRowTest()}
	res := OracleResult{
		Output: schema.OracleOutput{
			Oracle:      "linearizable.kv",
			Class:       schema.ClassConsistency,
			Status:      schema.StatusViolated,
			Explanation: "no linearization exists for key k/0",
		},
		ObservedPhase: schema.PhaseAssert,
		FirstSeenMS:   0, // the oracle supplied no timing at all
	}

	row := oracleRowOf(t, BuildCausalTimeline(src, res, nil))

	if row.TMS == 0 {
		t.Fatal("the oracle row was placed at t_ms 0 (DRIVE start) from a zero the oracle " +
			"never supplied; a fabricated position is worse than none, and this one puts " +
			"the finding before every fault on its own timeline")
	}
	if row.TMS != 18809 {
		t.Fatalf("oracle row t_ms = %d, want 18809 (the start of ASSERT, the phase the "+
			"finding is attributed to)", row.TMS)
	}
}

// The correction must not fire on an oracle that DID supply timing. This is the
// case that matters most: a stale read observed mid-PERTURB is the fixture's
// real anomaly, and moving its row to ASSERT would destroy the very information
// the timeline exists to carry.
func TestAFindingWithRealTimingKeepsIt(t *testing.T) {
	src := TimelineSource{Phases: phasesForRowTest()}
	res := OracleResult{
		Output: schema.OracleOutput{
			Oracle: "linearizable.kv",
			Class:  schema.ClassConsistency,
			Status: schema.StatusViolated,
		},
		ObservedPhase: schema.PhasePerturb,
		FirstSeenMS:   11124,
	}

	if got := oracleRowOf(t, BuildCausalTimeline(src, res, nil)).TMS; got != 11124 {
		t.Fatalf("oracle row t_ms = %d, want the reported 11124", got)
	}
}

// A finding whose evidence genuinely lies at DRIVE start must still be placed
// at 0. Without this the correction would be indiscriminate, moving real
// evidence to a phase boundary: the same fabrication in the other direction.
func TestAFindingGenuinelyAtDriveStartStaysAtZero(t *testing.T) {
	src := TimelineSource{Phases: phasesForRowTest()}
	res := OracleResult{
		Output: schema.OracleOutput{
			Oracle: "no_crash",
			Class:  schema.ClassCrash,
			Status: schema.StatusViolated,
		},
		// The engine DERIVED this phase from the offset, so the two agree.
		ObservedPhase: schema.PhaseDrive,
		FirstSeenMS:   0,
	}

	if got := oracleRowOf(t, BuildCausalTimeline(src, res, nil)).TMS; got != 0 {
		t.Fatalf("oracle row t_ms = %d, want 0; evidence that really is at DRIVE start "+
			"must not be relocated", got)
	}
}

// An unscoped finding has no phase to fall back to, and nothing better than the
// offset it was given. It must not panic or invent one.
func TestAnUnscopedFindingKeepsItsOffset(t *testing.T) {
	src := TimelineSource{Phases: phasesForRowTest()}
	res := OracleResult{
		Output:        schema.OracleOutput{Oracle: "x", Status: schema.StatusViolated},
		ObservedPhase: "",
		FirstSeenMS:   0,
	}

	if got := oracleRowOf(t, BuildCausalTimeline(src, res, nil)).TMS; got != 0 {
		t.Fatalf("oracle row t_ms = %d, want 0 for an unscoped finding", got)
	}
}

// A finding attributed to a phase the harness never measured has nothing to
// resolve against. It must degrade to the reported offset rather than dropping
// the row or reaching past the end of the window list.
func TestAFindingInAnUnmeasuredPhaseKeepsItsOffset(t *testing.T) {
	src := TimelineSource{Phases: schema.PhaseTimings{
		{Phase: schema.PhaseDrive, StartMS: 0, EndMS: 1},
	}}
	res := OracleResult{
		Output:        schema.OracleOutput{Oracle: "x", Status: schema.StatusViolated},
		ObservedPhase: schema.PhaseAssert, // no window was recorded for ASSERT
		FirstSeenMS:   0,
	}

	if got := oracleRowOf(t, BuildCausalTimeline(src, res, nil)).TMS; got != 0 {
		t.Fatalf("oracle row t_ms = %d, want 0 when the observed phase has no window", got)
	}
}
