package main

import (
	"testing"
	"time"

	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// ---------------------------------------------------------------------------
// Histories that MUST pass
// ---------------------------------------------------------------------------

func TestLinearizableSequentialHistoryPasses(t *testing.T) {
	got := run1(t, cat(
		wOK(1, "k", 0, 1, "7"),
		rOK(2, "k", 2, 3, "7"),
		wOK(3, "k", 4, 5, "9"),
		rOK(4, "k", 6, 7, "9"),
	))
	wantStatus(t, got, schema.StatusOK)
	wantMentions(t, got, "2 determinate read(s)", "2 determinate write(s)")
}

// Overlapping writes have a valid serialization only if the checker is willing
// to reorder concurrent operations. A checker that fixed the order by invoke
// time would report a violation here.
func TestConcurrentWritesWithValidSerializationPass(t *testing.T) {
	got := run1(t, cat(
		wOK(1, "k", 0, 10, "1"),
		wOK(2, "k", 1, 11, "2"),
		rOK(3, "k", 12, 13, "1"), // requires the order W2, W1
	))
	wantStatus(t, got, schema.StatusOK)
}

// The register's value before the recorded window is not known. Assuming null
// would make this a violation of a system that is behaving correctly.
func TestReadOfAValueWrittenBeforeTheWindowPasses(t *testing.T) {
	got := run1(t, cat(
		rOK(1, "k", 0, 1, "42"),
		rOK(2, "k", 2, 3, "42"),
		wOK(3, "k", 4, 5, "43"),
		rOK(4, "k", 6, 7, "43"),
	))
	wantStatus(t, got, schema.StatusOK)
}

func TestIndependentKeysAreCheckedIndependently(t *testing.T) {
	got := run1(t, cat(
		wOK(1, "a", 0, 1, "1"),
		wOK(2, "b", 0, 1, "9"),
		rOK(3, "a", 2, 3, "1"),
		rOK(4, "b", 2, 3, "9"),
	))
	wantStatus(t, got, schema.StatusOK)
	wantMentions(t, got, "every one of the 2 key(s)")
}

// 7 and 7.0 are the same register value. Comparing raw bytes would report a
// violation here: a false positive manufactured entirely by the checker.
func TestNumericFormattingDoesNotProduceAViolation(t *testing.T) {
	got := run1(t, cat(
		wOK(1, "k", 0, 1, "7"),
		rOK(2, "k", 2, 3, "7.0"),
		rOK(3, "k", 4, 5, "7e0"),
	))
	wantStatus(t, got, schema.StatusOK)
}

// ---------------------------------------------------------------------------
// Histories that MUST fail, with the right witness
// ---------------------------------------------------------------------------

// THE STALE-READ SHAPE. This is the fixture's lease bug reduced to three
// operations: W(1) ok, W(2) ok, then a read of 1 invoked strictly after W(2)
// returned.
func TestStaleReadIsViolatedWithTheRightWitness(t *testing.T) {
	got := run1(t, cat(
		wOK(1, "k", 0, 1, "1"),
		wOK(2, "k", 2, 3, "2"),
		rOK(3, "k", 4, 5, "1"),
	))
	wantStatus(t, got, schema.StatusViolated)
	if got.Witness.Key != "k" {
		t.Fatalf("witness key = %q, want %q", got.Witness.Key, "k")
	}
	wantOpIDs(t, got, 1, 2, 3)
	wantMentions(t, got,
		"no linearization exists",
		"WITNESSED failure, not a",
		"the register held 2",
		"written by op 2",
		"returned 1",
		"written by op 1",
	)
}

func TestTwoReadsThatCannotBothBeSatisfiedAreViolated(t *testing.T) {
	got := run1(t, cat(
		wOK(1, "k", 0, 10, "1"),
		wOK(2, "k", 1, 11, "2"),
		rOK(3, "k", 12, 13, "1"),
		rOK(4, "k", 14, 15, "2"), // no write is available between the two reads
	))
	wantStatus(t, got, schema.StatusViolated)
	wantOpIDs(t, got, 1, 2, 4)
}

func TestReadOfAValueNobodyWroteIsViolated(t *testing.T) {
	got := run1(t, cat(
		wOK(1, "k", 0, 1, "1"),
		rOK(2, "k", 2, 3, "1"),
		rOK(3, "k", 4, 5, "99"),
	))
	wantStatus(t, got, schema.StatusViolated)
	wantMentions(t, got, "returned 99")
}

// A violation on ONE key is a violation of the history, even when other keys are
// fine. It must not be diluted into an ok.
func TestAViolationOnOneKeyDecidesTheHistory(t *testing.T) {
	got := run1(t, cat(
		wOK(1, "a", 0, 1, "1"),
		wOK(2, "a", 2, 3, "2"),
		rOK(3, "a", 4, 5, "1"),
		wOK(4, "z", 0, 1, "1"),
		rOK(5, "z", 2, 3, "1"),
	))
	wantStatus(t, got, schema.StatusViolated)
	if got.Witness.Key != "a" {
		t.Fatalf("witness key = %q, want %q", got.Witness.Key, "a")
	}
}

// ---------------------------------------------------------------------------
// D-B. ok / fail / info
// ---------------------------------------------------------------------------

// THE FALSE-POSITIVE TEST. The history is linearizable if and only if the
// indeterminate write did NOT apply. A checker that treats info as
// definitely-applied reports a violation of a correct system.
func TestInfoWriteThatMustNotHaveAppliedIsOK(t *testing.T) {
	recs := cat(
		wOK(1, "k", 0, 1, "1"),
		wInfo(2, "k", 2, 3, "2"),
		rOK(3, "k", 4, 5, "1"),
	)
	got := run1(t, recs)
	wantStatus(t, got, schema.StatusOK)
	wantMentions(t, got, "1 indeterminate write(s) explored on both")

	// The test is able to fail: the SAME history with the write reported as a
	// definite success is a violation. If the two agreed, the branch would not be
	// doing anything.
	definite := cat(
		wOK(1, "k", 0, 1, "1"),
		wOK(2, "k", 2, 3, "2"),
		rOK(3, "k", 4, 5, "1"),
	)
	if g := run1(t, definite); g.Status != schema.StatusViolated {
		t.Fatalf("the same history with an ok write is %q, want %q; the info branch is vacuous",
			g.Status, schema.StatusViolated)
	}
}

// The mirror case: linearizable only if the indeterminate write DID apply. A
// checker that treats info as definitely-not-applied reports a violation here.
func TestInfoWriteThatMustHaveAppliedIsOK(t *testing.T) {
	got := run1(t, cat(
		wOK(1, "k", 0, 1, "1"),
		wInfo(2, "k", 2, 3, "2"),
		rOK(3, "k", 4, 5, "2"),
	))
	wantStatus(t, got, schema.StatusOK)
}

// An indeterminate operation's interval is NOT bounded by its own recorded
// completion: a request that timed out at t may still apply after t. Bounding it
// at t makes this history unlinearizable, which is a false positive.
func TestInfoWriteIntervalExtendsPastItsRecordedCompletion(t *testing.T) {
	got := run1(t, cat(
		wOK(1, "k", 0, 1, "1"),
		wInfo(2, "k", 2, 3, "2"), // recorded as finished at t=3 ...
		rOK(3, "k", 4, 6, "1"),   // ... but it can only have applied after t=6
		rOK(4, "k", 7, 8, "2"),
	))
	wantStatus(t, got, schema.StatusOK)
}

// An invoke with no completion at all is treated exactly as info.
func TestPendingWriteIsTreatedAsInfo(t *testing.T) {
	got := run1(t, cat(
		wOK(1, "k", 0, 1, "1"),
		wPending(2, "k", 2, "2"),
		rOK(3, "k", 4, 6, "1"),
		rOK(4, "k", 7, 8, "2"),
	))
	wantStatus(t, got, schema.StatusOK)
	wantMentions(t, got, "1 indeterminate write(s)")
}

// A failed operation definitely did not happen and constrains nothing.
func TestFailedWriteIsDropped(t *testing.T) {
	got := run1(t, cat(
		wOK(1, "k", 0, 1, "1"),
		wFail(2, "k", 2, 3, "2"),
		rOK(3, "k", 4, 5, "1"),
	))
	wantStatus(t, got, schema.StatusOK)
	wantMentions(t, got, "1 failed operation(s) dropped")
}

// An indeterminate read returns an unknown value, so it constrains nothing.
func TestInfoReadIsDropped(t *testing.T) {
	got := run1(t, cat(
		wOK(1, "k", 0, 1, "1"),
		op(2, 1, "read", "k", 2, 3, "", "999", schema.HistoryInfo),
		rOK(3, "k", 4, 5, "1"),
	))
	wantStatus(t, got, schema.StatusOK)
	wantMentions(t, got, "1 indeterminate read(s) dropped")
}

// ---------------------------------------------------------------------------
// D-A. The per-key decomposition precondition
// ---------------------------------------------------------------------------

// A multi-key transaction breaks the locality theorem's hypothesis. The checker
// must refuse, name the operation, and never partition.
func TestMultiKeyTransactionIsInconclusive(t *testing.T) {
	txn := []rec{
		{t: 0, proc: 2, typ: schema.HistoryInvoke, f: "txn", noKey: true, id: 77,
			val: `[{"f":"r","key":"k/1"},{"f":"w","key":"k/2","value":5}]`},
		{t: 1, proc: 2, typ: schema.HistoryOK, f: "txn", noKey: true, id: 77,
			val: `[{"f":"r","key":"k/1","value":3},{"f":"w","key":"k/2","value":5}]`},
	}
	// A history that would otherwise be a clear violation, so a checker that
	// silently partitioned would report `violated` here instead of refusing.
	got := run1(t, cat(
		wOK(1, "k/1", 0, 1, "1"),
		wOK(2, "k/1", 2, 3, "2"),
		rOK(3, "k/1", 4, 5, "1"),
		txn,
	))
	wantStatus(t, got, schema.StatusInconclusive)
	wantMentions(t, got,
		"op_id 77",
		"touches 2 keys",
		"locality theorem",
		"UNSOUND",
		"Elle-style",
	)
	if !got.Witness.IsEmpty() {
		t.Fatalf("a refusal must carry no witness, got %+v", got.Witness)
	}
}

// THE ENCODING THE FIXTURE ACTUALLY EMITS. loadgen writes its txn op list in
// Elle's read/write-register convention (["r","k/61",null]) not as objects. A
// key extractor that only understood the object form would see these records as
// key-less and produce the weaker refusal, missing the multi-key diagnostic on
// the one history it exists to diagnose.
func TestMultiKeyTransactionInElleTupleEncodingIsInconclusive(t *testing.T) {
	txn := []rec{
		{t: 6, proc: 7, typ: schema.HistoryInvoke, f: "txn", noKey: true, id: 15,
			val: `[["r","k/61",null],["w","k/23",7000001]]`},
		{t: 7, proc: 7, typ: schema.HistoryOK, f: "txn", noKey: true, id: 15,
			val: `[["r","k/61",3],["w","k/23",7000001]]`},
	}
	got := run1(t, cat(wOK(1, "k/23", 0, 1, "1"), rOK(2, "k/23", 2, 3, "1"), txn))
	wantStatus(t, got, schema.StatusInconclusive)
	wantMentions(t, got,
		"op_id 15",
		"touches 2 keys",
		`"k/61"`,
		`"k/23"`,
		"locality theorem",
		"Elle-style",
	)
}

// A transaction that happens to touch ONE key is still not a single-key register
// read or write, and the refusal must say so rather than reporting the generic
// unmodelled-operation message on its own.
func TestSingleKeyTransactionIsRefusedAsTransactional(t *testing.T) {
	txn := []rec{
		{t: 6, proc: 7, typ: schema.HistoryInvoke, f: "txn", noKey: true, id: 9,
			val: `[["r","k/23",null],["w","k/23",10000001]]`},
		{t: 7, proc: 7, typ: schema.HistoryOK, f: "txn", noKey: true, id: 9,
			val: `[["r","k/23",1],["w","k/23",10000001]]`},
	}
	got := run1(t, cat(wOK(1, "k/23", 0, 1, "1"), rOK(2, "k/23", 2, 3, "1"), txn))
	wantStatus(t, got, schema.StatusInconclusive)
	wantMentions(t, got, "op_id 9", "TRANSACTION", "Elle-style", `key "k/23"`)
}

// An operation this checker does not model is refused rather than guessed at.
func TestUnmodelledOperationIsInconclusive(t *testing.T) {
	admin := []rec{
		{t: 0, proc: 3, typ: schema.HistoryInvoke, f: "admin", noKey: true, id: 88},
		{t: 1, proc: 3, typ: schema.HistoryOK, f: "admin", noKey: true, id: 88},
	}
	got := run1(t, cat(wOK(1, "k", 0, 1, "1"), rOK(2, "k", 2, 3, "1"), admin))
	wantStatus(t, got, schema.StatusInconclusive)
	wantMentions(t, got, "op_id 88", `"admin"`, "does not model")
}

func TestReadWithNoKeyIsInconclusive(t *testing.T) {
	got := run1(t, []rec{
		{t: 0, proc: 1, typ: schema.HistoryInvoke, f: "read", noKey: true, id: 5},
		{t: 1, proc: 1, typ: schema.HistoryOK, f: "read", noKey: true, id: 5, val: "1"},
	})
	wantStatus(t, got, schema.StatusInconclusive)
	wantMentions(t, got, "op_id 5", "names no key")
}

// ---------------------------------------------------------------------------
// Vacuous passes
// ---------------------------------------------------------------------------

func TestEmptyHistoryIsInconclusive(t *testing.T) {
	got := run1(t, nil)
	wantStatus(t, got, schema.StatusInconclusive)
	wantMentions(t, got, "no operation records", "never ok")
}

func TestHistoryOfPhaseMarkersOnlyIsInconclusive(t *testing.T) {
	path := writeHistory(t, nil,
		`{"t_ns":1,"type":"info","event":"phase","phase":"DRIVE"}`,
		`{"t_ns":2,"type":"info","event":"phase","phase":"ASSERT"}`,
	)
	got := check(path, testOptions(), time.Now())
	wantStatus(t, got, schema.StatusInconclusive)
	wantMentions(t, got, "no operation records", "2 marker record(s)")
}

// A history of writes alone is trivially linearizable and is no evidence at all.
func TestHistoryWithNoDeterminateReadsIsInconclusive(t *testing.T) {
	got := run1(t, cat(
		wOK(1, "k", 0, 1, "1"),
		wOK(2, "k", 2, 3, "2"),
		wOK(3, "k", 4, 5, "3"),
	))
	wantStatus(t, got, schema.StatusInconclusive)
	wantMentions(t, got, "NOT ONE determinate read")
}

func TestHistoryWhereEveryOperationIsDroppedIsInconclusive(t *testing.T) {
	got := run1(t, cat(
		wFail(1, "k", 0, 1, "1"),
		op(2, 1, "read", "k", 2, 3, "", "", schema.HistoryInfo),
	))
	wantStatus(t, got, schema.StatusInconclusive)
	wantMentions(t, got, "none of them is checkable", "never ok")
}

// ---------------------------------------------------------------------------
// Unreliable input
// ---------------------------------------------------------------------------

func TestMalformedLineIsInconclusive(t *testing.T) {
	got := run1(t, cat(wOK(1, "k", 0, 1, "1"), rOK(2, "k", 2, 3, "1")), `{"t_ns":4,"type":`)
	wantStatus(t, got, schema.StatusInconclusive)
	wantMentions(t, got, "did not decode", "completion record")
}

func TestRecordWithNoOpIDIsInconclusive(t *testing.T) {
	got := run1(t, []rec{
		{t: 0, proc: 1, typ: schema.HistoryInvoke, f: "read", key: "k", noID: true},
		{t: 1, proc: 1, typ: schema.HistoryOK, f: "read", key: "k", noID: true, val: "1"},
	})
	wantStatus(t, got, schema.StatusInconclusive)
	wantMentions(t, got, "carries no op_id")
}

func TestDuplicateInvokeIsInconclusive(t *testing.T) {
	got := run1(t, []rec{
		{t: 0, proc: 1, typ: schema.HistoryInvoke, f: "read", key: "k", id: 1},
		{t: 1, proc: 1, typ: schema.HistoryInvoke, f: "read", key: "k", id: 1},
	})
	wantStatus(t, got, schema.StatusInconclusive)
	wantMentions(t, got, "two invoke records")
}

func TestMissingHistoryFileIsInconclusive(t *testing.T) {
	got := check("does-not-exist.jsonl", testOptions(), time.Now())
	wantStatus(t, got, schema.StatusInconclusive)
	wantMentions(t, got, "cannot open the history")
}

// ---------------------------------------------------------------------------
// D-C. Budget exhaustion
// ---------------------------------------------------------------------------

func TestStateCeilingExhaustionIsInconclusiveNotOK(t *testing.T) {
	recs := seq("k", 40)
	opt := testOptions()
	opt.MaxStates = 5
	got := check(writeHistory(t, recs), opt, time.Now())
	wantStatus(t, got, schema.StatusInconclusive)
	wantMentions(t, got, "budget was exhausted", "explored-state ceiling", "never ok")

	// The same history finishes, and passes, on a real budget, so the test above
	// is measuring the budget and not a broken history.
	if g := check(writeHistory(t, recs), testOptions(), time.Now()); g.Status != schema.StatusOK {
		t.Fatalf("with a full budget the history is %q, want %q: %s", g.Status, schema.StatusOK, g.Explanation)
	}
}

func TestExpiredWallBudgetIsInconclusive(t *testing.T) {
	opt := testOptions()
	opt.Wall = time.Nanosecond
	got := check(writeHistory(t, seq("k", 10)), opt, time.Now().Add(-time.Hour))
	wantStatus(t, got, schema.StatusInconclusive)
	wantMentions(t, got, "wall-clock budget expired")
}

// A key that did not finish is never reported ok, even when every other key did.
func TestAnUnfinishedKeyIsNeverReportedOK(t *testing.T) {
	recs := cat(
		wOK(1, "a", 0, 1, "1"),
		rOK(2, "a", 2, 3, "1"),
		seq("b", 20),
	)
	opt := testOptions()
	opt.MaxStates = 8
	got := check(writeHistory(t, recs), opt, time.Now())
	wantStatus(t, got, schema.StatusInconclusive)
	wantMentions(t, got, `"b"`, "1 of 2 key(s) were searched to completion")
}

// A WITNESSED violation is definite, so it outranks an unfinished key.
func TestAWitnessedViolationOutranksAnUnfinishedKey(t *testing.T) {
	recs := cat(
		wOK(1, "a", 0, 1, "1"),
		wOK(2, "a", 2, 3, "2"),
		rOK(3, "a", 4, 5, "1"),
		seq("b", 50),
	)
	opt := testOptions()
	opt.MaxStates = 200
	got := check(writeHistory(t, recs), opt, time.Now())
	wantStatus(t, got, schema.StatusViolated)
	if got.Witness.Key != "a" {
		t.Fatalf("witness key = %q, want %q", got.Witness.Key, "a")
	}
}

// The budget must never turn a linearizable history into a violation.
func TestExhaustionIsNeverReportedAsAViolation(t *testing.T) {
	for _, states := range []int64{1, 2, 3, 5, 9, 17, 33} {
		opt := testOptions()
		opt.MaxStates = states
		got := check(writeHistory(t, seq("k", 30)), opt, time.Now())
		if got.Status == schema.StatusViolated {
			t.Fatalf("max-states=%d turned a linearizable history into a violation: %s",
				states, got.Explanation)
		}
	}
}
