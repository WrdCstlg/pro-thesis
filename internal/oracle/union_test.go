package oracle

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// OQ-058. `no_stuck_op` is the second reader that classifies a history record
// before validating it: no_stuck_op.go skips anything whose RecordKind is not
// RecordOperation, and RecordKind reads `event` alone. Tagging BOTH records of
// an operation with an event therefore removed it from the oracle's view
// entirely: and an operation the oracle cannot see is an operation it cannot
// report as stuck.
//
// The fix is at the shared loader rather than in the oracle, so every present
// and future consumer of History inherits it: ReadHistory counts a record that
// breaks the operation/marker union, which makes the history unreliable, which
// every conforming oracle already refuses over.

const stuckOpNS = int64(1700000000000000000)

func nsAt(ms int64) int64 { return stuckOpNS + ms*int64(time.Millisecond) }

// stuckPhases is a timeline where an operation invoked at 5000ms and never
// completed is unambiguously stuck by ASSERT: HEAL ends at 12000 and the
// default SLO ceiling is 5000, so 20000 >= 17000.
func stuckPhases() schema.PhaseTimings {
	return schema.PhaseTimings{
		{Phase: schema.PhaseDrive, StartMS: 0, EndMS: 10000},
		{Phase: schema.PhaseHeal, StartMS: 10000, EndMS: 12000},
		{Phase: schema.PhaseQuiesce, StartMS: 12000, EndMS: 20000},
		{Phase: schema.PhaseAssert, StartMS: 20000, EndMS: 20500},
	}
}

func TestAnOperationHiddenByAnEventMakesTheHistoryUnreliable(t *testing.T) {
	// Control: a plain never-completed invoke. The loader keeps it and the
	// history is trustworthy evidence.
	control := ReadHistory(strings.NewReader(
		`{"t_ns":`+itoa(nsAt(5000))+`,"type":"invoke","f":"write","op_id":1}`+"\n",
	), "control.jsonl")

	if !control.Reliable() {
		t.Fatalf("a well-formed history was called unreliable: %s", control.Unreliable())
	}
	if len(control.Entries) != 1 {
		t.Fatalf("control kept %d entries, want 1", len(control.Entries))
	}

	// Attack: the same record, plus an event. Every marker-skipping reader
	// discards it; the loader must refuse to hand it on as if nothing happened.
	attacked := ReadHistory(strings.NewReader(
		`{"t_ns":`+itoa(nsAt(5000))+`,"type":"invoke","f":"write","op_id":1,"event":"note"}`+"\n",
	), "attacked.jsonl")

	if attacked.Reliable() {
		t.Fatal("a record carrying both an event and an op_id was accepted as trustworthy; " +
			"it is skipped as a marker by every reader, so the operation it recorded is gone")
	}
	if attacked.InvalidRecords != 1 {
		t.Errorf("InvalidRecords = %d, want 1", attacked.InvalidRecords)
	}
	if len(attacked.Entries) != 0 {
		t.Errorf("an unclassifiable record was appended to Entries; a consumer would have to "+
			"guess its kind, which is the defect (%d entries)", len(attacked.Entries))
	}
	if !strings.Contains(attacked.Unreliable(), "event") {
		t.Errorf("the explanation does not name the cause: %s", attacked.Unreliable())
	}
}

func TestNoStuckOpRefusesRatherThanMissingAHiddenOperation(t *testing.T) {
	ctx := context.Background()
	oracle := NewNoStuckOp(DefaultNoStuckOpOptions())

	// A second operation that completes normally. Without it the attack arm
	// would report inconclusive for the wrong reason ("no operations were
	// observed" rather than "a record could not be classified") and the test
	// would pass even with the fix removed.
	const healthy = `{"t_ns":` + `1700000001000000000` + `,"type":"invoke","f":"read","op_id":2}` + "\n" +
		`{"t_ns":` + `1700000001050000000` + `,"type":"ok","f":"read","op_id":2}` + "\n"

	// Control: the stuck operation is visible, and the oracle convicts.
	visible := ReadHistory(strings.NewReader(
		`{"t_ns":`+itoa(nsAt(5000))+`,"type":"invoke","f":"write","op_id":1}`+"\n"+healthy,
	), "visible.jsonl")

	res, err := oracle.Evaluate(ctx, schema.PhaseAssert, &Input{
		DriveOriginWallNS: stuckOpNS, Phases: stuckPhases(), History: visible,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Status != schema.StatusViolated {
		t.Fatalf("the control did not convict a stuck operation: %v: %s", res.Status, res.Explanation)
	}

	// Attack: one field hides the invoke. The oracle must not report OK.
	hidden := ReadHistory(strings.NewReader(
		`{"t_ns":`+itoa(nsAt(5000))+`,"type":"invoke","f":"write","op_id":1,"event":"note"}`+"\n"+healthy,
	), "hidden.jsonl")

	res, err = oracle.Evaluate(ctx, schema.PhaseAssert, &Input{
		DriveOriginWallNS: stuckOpNS, Phases: stuckPhases(), History: hidden,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Status == schema.StatusOK {
		t.Fatalf("a stuck operation was hidden by an event field and the oracle passed: %s",
			res.Explanation)
	}
	if res.Status != schema.StatusInconclusive {
		t.Fatalf("status = %v, want inconclusive: %s", res.Status, res.Explanation)
	}
}

// TestReadHistoryStillAcceptsRealMarkers guards against overcorrection: the
// harness merges its own phase markers into this same file, and refusing those
// would make every real run unreliable.
func TestReadHistoryStillAcceptsRealMarkers(t *testing.T) {
	marker := schema.PhaseMarker(nsAt(11000), schema.PhaseHeal)
	line, err := marker.MarshalLine()
	if err != nil {
		t.Fatalf("marshal phase marker: %v", err)
	}

	h := ReadHistory(strings.NewReader(
		`{"t_ns":`+itoa(nsAt(5000))+`,"type":"invoke","f":"write","op_id":1}`+"\n"+
			string(line)+"\n",
	), "mixed.jsonl")

	if !h.Reliable() {
		t.Fatalf("a history mixing operations with a legitimate phase marker was refused: %s",
			h.Unreliable())
	}
	if len(h.Entries) != 2 {
		t.Fatalf("kept %d entries, want 2 (the operation and the marker)", len(h.Entries))
	}
}
