package main

import (
	"testing"

	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// OQ-058. The history file carries two kinds of record (driver-emitted
// operations and harness-emitted markers) and the ONLY thing separating them
// is whether `event` is non-empty. A reader that classifies before it validates
// therefore deletes any operation record that carries a stray event, silently
// and without counting it.
//
// Both tests below are pairs. The control establishes what the checker says
// about the history when it can see all of it; the attack adds one field and
// nothing else. Before the fix the two arms disagreed, which is the whole
// defect: one field, invisible in the verdict, moved the answer.

// TestHiddenCompletionCannotSilenceAViolation is the false-NEGATIVE direction.
//
// A stale read is the violation this system exists to find. Tagging its two
// records with an event removed them from the checker's view, leaving a history
// of two writes and no read (trivially linearizable) and the run came back
// `ok` with exit 0. The surrounding traffic matters: it keeps other determinate
// reads in the history, so the "not one determinate read" vacuity guard cannot
// fire and the acquittal is silent rather than refused.
func TestHiddenCompletionCannotSilenceAViolation(t *testing.T) {
	background := seq("k/9", 5)

	staleRead := cat(
		wOK(1, "k/1", 100, 101, "1"),
		wOK(2, "k/1", 102, 103, "2"),
		rOK(3, "k/1", 104, 105, "1"), // returns a value two writes stale
	)

	control := run1(t, cat(background, staleRead))
	wantStatus(t, control, schema.StatusViolated)
	wantOpIDs(t, control, 1, 2, 3)

	// One field added to the read's two records. Nothing else changes.
	hidden := cat(background,
		wOK(1, "k/1", 100, 101, "1"),
		wOK(2, "k/1", 102, 103, "2"),
		tag("note", rOK(3, "k/1", 104, 105, "1")),
	)

	got := run1(t, hidden)
	if got.Status == schema.StatusOK {
		t.Fatal("an operation record carrying an event was skipped as a marker, and the " +
			"violation it recorded disappeared: the checker reported ok on a history it " +
			"had deleted the evidence from")
	}
	wantStatus(t, got, schema.StatusInconclusive)
	wantMentions(t, got, "event")
}

// TestHiddenTransactionCannotManufactureAViolation is the false-POSITIVE
// direction, and it is the worse of the two.
//
// The history is LINEARIZABLE: the transaction writes k/1=1 between the write
// of 2 and the read, so a read returning 1 is correct. The checker refuses such
// a history by design, because a multi-key transaction breaks the precondition
// of the Herlihy & Wing locality theorem and per-key decomposition is unsound
// for it.
//
// Tagging the transaction with an event removed it before that refusal could
// see it. What remained (W(1), W(2), R->1) admits no linearization, and the
// checker convicted, reporting a witness and the words "this is a WITNESSED
// failure, not a timeout and not a budget" about a history that never happened.
// It also defeats the multi-key refusal main.go claims is unconditional.
func TestHiddenTransactionCannotManufactureAViolation(t *testing.T) {
	// A transaction reading nothing and writing two different keys.
	txn := []rec{
		{t: 104, proc: 2, typ: schema.HistoryInvoke, f: "txn", noKey: true, id: 4,
			val: `[["w","k/1",1],["w","k/2",5]]`},
		{t: 105, proc: 2, typ: schema.HistoryOK, f: "txn", noKey: true, id: 4,
			val: `[["w","k/1",1],["w","k/2",5]]`},
	}

	linearizable := cat(
		wOK(1, "k/1", 100, 101, "1"),
		wOK(2, "k/1", 102, 103, "2"),
		txn,
		rOK(3, "k/1", 106, 107, "1"), // legitimate: the txn wrote 1 after the 2
	)

	// Control: the checker sees the transaction and refuses to partition.
	control := run1(t, linearizable)
	wantStatus(t, control, schema.StatusInconclusive)
	wantMentions(t, control, "txn")

	hidden := cat(
		wOK(1, "k/1", 100, 101, "1"),
		wOK(2, "k/1", 102, 103, "2"),
		tag("note", txn),
		rOK(3, "k/1", 106, 107, "1"),
	)

	got := run1(t, hidden)
	if got.Status == schema.StatusViolated {
		t.Fatalf("the checker convicted a LINEARIZABLE history after an event field removed "+
			"the transaction that explains it; a false violation is the one verdict this "+
			"oracle must never produce\nexplanation: %s", got.Explanation)
	}
	wantStatus(t, got, schema.StatusInconclusive)
	wantMentions(t, got, "event")
}

// TestLegitimatePhaseMarkersStillLoad guards the fix from overcorrecting. The
// harness merges its own phase markers into the same file, so a well-formed
// marker must continue to be skipped rather than refused: otherwise every real
// run becomes inconclusive and the cure is worse than the disease.
func TestLegitimatePhaseMarkersStillLoad(t *testing.T) {
	marker := schema.PhaseMarker(103, schema.PhaseHeal)
	line, err := marker.MarshalLine()
	if err != nil {
		t.Fatalf("marshal phase marker: %v", err)
	}

	got := run1(t, cat(
		wOK(1, "k/1", 100, 101, "1"),
		rOK(2, "k/1", 104, 105, "1"),
	), string(line))

	wantStatus(t, got, schema.StatusOK)
}
