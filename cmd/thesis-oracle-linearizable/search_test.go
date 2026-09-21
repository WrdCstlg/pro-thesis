package main

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// ---------------------------------------------------------------------------
// Search-layer tests.
//
// These build operations directly rather than going through the loader, because
// the loader's interval policy and the search's branching policy are two
// SEPARATE soundness rules and each has to be pinned on its own. Going only
// through the loader would let one rule mask a defect in the other, which is
// exactly what happened the first time this suite was written: with an
// indeterminate write's interval extended to infinity, the write can always be
// linearized last, so the not-applied branch becomes unobservable from the
// outside and a checker that dropped it looked correct.
// ---------------------------------------------------------------------------

func mkOp(id int64, kind opKind, key, val string, t0, t1 int64) *operation {
	v, err := canonicalValue(json.RawMessage(val))
	if err != nil {
		panic(err)
	}
	return &operation{id: id, kind: kind, key: key, value: v, invokeNS: t0, returnNS: t1}
}

func searchOps(t *testing.T, ops []*operation) (outcome, stats) {
	t.Helper()
	s := newSearcher(ops, 1_000_000, time.Now().Add(10*time.Second), 32<<20)
	return s.run(), s.st
}

// D-B, directly: an indeterminate write offers BOTH successor states.
func TestAlternativesForAnIndeterminateWriteBranch(t *testing.T) {
	w := mkOp(1, opWrite, "k", "2", 0, 1)
	w.maybe = true
	alts := alternatives(knownValue("1"), w)
	if len(alts) != 2 {
		t.Fatalf("an indeterminate write offered %d alternative(s) (%v), want 2 "+
			"(applied, then not-applied)", len(alts), alts)
	}
	if !alts[0].Equal(knownValue("2")) {
		t.Fatalf("first alternative = %v, want the applied branch (2)", alts[0])
	}
	if !alts[1].Equal(knownValue("1")) {
		t.Fatalf("second alternative = %v, want the not-applied branch (1)", alts[1])
	}

	// A definite write offers only the applied branch.
	d := mkOp(2, opWrite, "k", "2", 0, 1)
	if alts := alternatives(knownValue("1"), d); len(alts) != 1 {
		t.Fatalf("a determinate write offered %d alternative(s), want 1", len(alts))
	}
}

// THE FALSE-POSITIVE TEST, at the layer where the branch is observable.
//
// The indeterminate write is given a BOUNDED interval here, so it cannot be
// pushed past the read. The history is then linearizable if and only if the
// search explores the not-applied branch.
func TestSearchExploresTheNotAppliedBranchOfAnIndeterminateWrite(t *testing.T) {
	build := func(maybe bool) []*operation {
		w1 := mkOp(1, opWrite, "k", "1", 0, 1)
		w2 := mkOp(2, opWrite, "k", "2", 2, 3)
		w2.maybe = maybe
		r := mkOp(3, opRead, "k", "1", 4, 5)
		return []*operation{w1, w2, r}
	}

	if out, _ := searchOps(t, build(true)); out != outcomeLinearizable {
		t.Fatalf("outcome = %v, want linearizable: an indeterminate write that may not have "+
			"applied was treated as definitely applied, which is a false-positive generator", out)
	}
	// The same history with the write DEFINITELY applied is a violation, so the
	// test above is measuring the branch and not a trivially linearizable input.
	if out, _ := searchOps(t, build(false)); out != outcomeViolated {
		t.Fatalf("outcome = %v, want violated with a determinate write; the branching test is vacuous", out)
	}
}

// The mirror branch: linearizable only if the indeterminate write DID apply.
func TestSearchExploresTheAppliedBranchOfAnIndeterminateWrite(t *testing.T) {
	w1 := mkOp(1, opWrite, "k", "1", 0, 1)
	w2 := mkOp(2, opWrite, "k", "2", 2, 3)
	w2.maybe = true
	r := mkOp(3, opRead, "k", "2", 4, 5)
	if out, _ := searchOps(t, []*operation{w1, w2, r}); out != outcomeLinearizable {
		t.Fatalf("outcome = %v, want linearizable", out)
	}
}

// The loader's rule that an indeterminate operation's interval extends to
// infinity is what makes THIS history linearizable. Pinned at the search layer
// so a change to the loader cannot silently take the guarantee away.
func TestAnUnboundedIndeterminateWriteCanLinearizeLast(t *testing.T) {
	mk := func(unbounded bool) []*operation {
		w1 := mkOp(1, opWrite, "k", "1", 0, 1)
		w2 := mkOp(2, opWrite, "k", "2", 2, 3)
		w2.maybe = true
		w2.openAtEnd = unbounded
		ra := mkOp(3, opRead, "k", "1", 4, 6)
		rb := mkOp(4, opRead, "k", "2", 7, 8)
		return []*operation{w1, w2, ra, rb}
	}
	if out, _ := searchOps(t, mk(true)); out != outcomeLinearizable {
		t.Fatalf("outcome = %v, want linearizable when the indeterminate write is unbounded", out)
	}
	// Bounding it at its own recorded completion is what would make this a FALSE
	// POSITIVE: the write may genuinely have applied after its request timed out.
	if out, _ := searchOps(t, mk(false)); out != outcomeViolated {
		t.Fatalf("outcome = %v, want violated when the indeterminate write is bounded; "+
			"the infinite-interval rule is then doing nothing", out)
	}
}

// The register starts undetermined, so a read of a value written before the
// window is not a violation. Assuming null would make it one.
func TestUndeterminedInitialStateIsLearnedByTheFirstRead(t *testing.T) {
	r1 := mkOp(1, opRead, "k", "42", 0, 1)
	r2 := mkOp(2, opRead, "k", "42", 2, 3)
	if out, _ := searchOps(t, []*operation{r1, r2}); out != outcomeLinearizable {
		t.Fatalf("outcome = %v, want linearizable", out)
	}
	// It is learned ONCE: two reads of different values with no write between
	// them still cannot both be satisfied.
	r3 := mkOp(3, opRead, "k", "43", 4, 5)
	if out, _ := searchOps(t, []*operation{r1, r2, r3}); out != outcomeViolated {
		t.Fatalf("outcome = %v, want violated; the undetermined initial state is absorbing "+
			"every read and the checker can no longer fail", out)
	}
}

// An operation with no invoke record opens before everything, which widens its
// interval. Widening can lose a violation; it must never invent one.
func TestAnOperationWithNoInvokeOpensAtTheStart(t *testing.T) {
	w := mkOp(1, opWrite, "k", "1", 0, 1)
	r := mkOp(2, opRead, "k", "9", 10, 11)
	r.openAtStart = true
	// The read may be ordered before the write, so this is linearizable.
	if out, _ := searchOps(t, []*operation{w, r}); out != outcomeLinearizable {
		t.Fatalf("outcome = %v, want linearizable", out)
	}
}

// Memoisation must not change any answer, only the cost of reaching it.
func TestMemoisationDoesNotChangeTheAnswer(t *testing.T) {
	cases := []struct {
		name string
		ops  []*operation
		want outcome
	}{
		{"linearizable", []*operation{
			mkOp(1, opWrite, "k", "1", 0, 10),
			mkOp(2, opWrite, "k", "2", 1, 11),
			mkOp(3, opRead, "k", "1", 12, 13),
		}, outcomeLinearizable},
		{"violated", []*operation{
			mkOp(1, opWrite, "k", "1", 0, 1),
			mkOp(2, opWrite, "k", "2", 2, 3),
			mkOp(3, opRead, "k", "1", 4, 5),
		}, outcomeViolated},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withMemo := newSearcher(tc.ops, 1_000_000, time.Now().Add(10*time.Second), 32<<20)
			got := withMemo.run()
			if got != tc.want {
				t.Fatalf("with memo: outcome = %v, want %v", got, tc.want)
			}
			// maxEntries has a floor of 1024, so a memo that can record nothing is
			// simulated by clearing it after construction.
			noMemo := newSearcher(tc.ops, 1_000_000, time.Now().Add(10*time.Second), 32<<20)
			noMemo.memo.maxEntries = 0
			if got := noMemo.run(); got != tc.want {
				t.Fatalf("with a full memo: outcome = %v, want %v", got, tc.want)
			}
		})
	}
}

// A memo that has stopped recording must never PRUNE. Pruning an unexplored
// branch could turn a linearizable history into a reported violation.
func TestAFullMemoNeverPrunes(t *testing.T) {
	m := newMemo(1, 1<<20)
	m.maxEntries = 0
	bits := []uint64{0b101}
	if !m.insert(knownValue("1"), bits) {
		t.Fatal("a full memo refused a branch it had never recorded")
	}
	if !m.insert(knownValue("1"), bits) {
		t.Fatal("a full memo refused the same branch twice; it is pruning on state it did not store")
	}
}

func TestMemoDistinguishesStateAndLinearizedSet(t *testing.T) {
	m := newMemo(1, 1<<20)
	if !m.insert(knownValue("1"), []uint64{0b1}) {
		t.Fatal("first insert must be new")
	}
	if m.insert(knownValue("1"), []uint64{0b1}) {
		t.Fatal("the identical pair must be recognised as already explored")
	}
	if !m.insert(knownValue("2"), []uint64{0b1}) {
		t.Fatal("a different register value is a different state")
	}
	if !m.insert(knownValue("1"), []uint64{0b11}) {
		t.Fatal("a different linearized set is a different state")
	}
	if !m.insert(unknownValue, []uint64{0b1}) {
		t.Fatal("the undetermined value is distinct from every determined one")
	}
}

// The budget stops the search; it never decides it.
func TestSearchExhaustionIsNeverViolated(t *testing.T) {
	ops := make([]*operation, 0, 60)
	var ts int64
	for i := 0; i < 30; i++ {
		ops = append(ops, mkOp(int64(i*2+1), opWrite, "k", "1", ts, ts+1))
		ts += 2
		ops = append(ops, mkOp(int64(i*2+2), opRead, "k", "1", ts, ts+1))
		ts += 2
	}
	for _, limit := range []int64{1, 2, 5, 11} {
		s := newSearcher(ops, limit, time.Now().Add(10*time.Second), 32<<20)
		if got := s.run(); got != outcomeExhausted {
			t.Fatalf("limit %d: outcome = %v, want exhausted", limit, got)
		}
		if s.st.Why != exhaustStates {
			t.Fatalf("limit %d: exhaustion reason = %q, want %q", limit, s.st.Why, exhaustStates)
		}
	}
}

func TestAnExpiredDeadlineStopsTheSearchImmediately(t *testing.T) {
	ops := []*operation{
		mkOp(1, opWrite, "k", "1", 0, 1),
		mkOp(2, opRead, "k", "1", 2, 3),
	}
	s := newSearcher(ops, 1_000_000, time.Now().Add(-time.Hour), 32<<20)
	if got := s.run(); got != outcomeExhausted {
		t.Fatalf("outcome = %v, want exhausted", got)
	}
	if s.st.Why != exhaustWall {
		t.Fatalf("exhaustion reason = %q, want %q", s.st.Why, exhaustWall)
	}
}

// Value canonicalisation is a soundness surface: a value that compares unequal
// to itself would be a false-positive generator.
func TestCanonicalValue(t *testing.T) {
	same := [][2]string{
		{"7", "7.0"},
		{"7", "7e0"},
		{"-0", "0"},
		{`{"a":1,"b":2}`, `{"b":2,"a":1}`},
		{"null", " null "},
		{`"x"`, `"x"`},
	}
	for _, p := range same {
		a, err := canonicalValue(json.RawMessage(p[0]))
		if err != nil {
			t.Fatalf("canonicalValue(%q): %v", p[0], err)
		}
		b, err := canonicalValue(json.RawMessage(p[1]))
		if err != nil {
			t.Fatalf("canonicalValue(%q): %v", p[1], err)
		}
		if !a.Equal(b) {
			t.Fatalf("canonicalValue(%q)=%v and canonicalValue(%q)=%v differ", p[0], a, p[1], b)
		}
	}

	differ := [][2]string{
		{"1", "2"},
		{"null", "0"},
		{"null", `"null"`},
		{"[1,2]", "[2,1]"},
		{"true", "1"},
	}
	for _, p := range differ {
		a, _ := canonicalValue(json.RawMessage(p[0]))
		b, _ := canonicalValue(json.RawMessage(p[1]))
		if a.Equal(b) {
			t.Fatalf("canonicalValue(%q) and canonicalValue(%q) both gave %v", p[0], p[1], a)
		}
	}

	if _, err := canonicalValue(json.RawMessage("")); err == nil {
		t.Fatal("an empty value must not canonicalise")
	}
	if _, err := canonicalValue(json.RawMessage("{oops")); err == nil {
		t.Fatal("malformed JSON must not canonicalise")
	}
	if unknownValue.Known() {
		t.Fatal("the zero Value must be undetermined")
	}
	if unknownValue.Equal(knownValue("null")) {
		t.Fatal("undetermined must not equal null; that is the whole point of the distinction")
	}
}

// The oracle's declaration is part of its identity in the verdict and in the
// lock manifest.
func TestOracleDeclaration(t *testing.T) {
	if oracleName != "linearizable.kv" {
		t.Fatalf("oracle name = %q", oracleName)
	}
	if oracleClass != schema.ClassConsistency {
		t.Fatalf("oracle class = %q", oracleClass)
	}
	ph := oracleValidPhases()
	if len(ph) != 1 || ph[0] != schema.PhaseAssert {
		t.Fatalf("valid_phases = %v, want [ASSERT]", ph)
	}
	if schema.DefaultSeverity(oracleClass) != schema.SeverityHigh {
		t.Fatalf("consistency must map to high severity, got %q", schema.DefaultSeverity(oracleClass))
	}
}
