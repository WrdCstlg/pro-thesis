package main

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Differential test against an independent brute-force reference.
//
// The Wing & Gong search with lift/unlift and memoisation is the kind of code
// whose bugs are silent: it keeps answering, just wrongly. So it is checked
// against a second implementation that shares no code with it: enumerate every
// permutation of the operations, discard the ones that violate real-time
// precedence, and simulate each survivor directly.
//
// Disagreement in EITHER direction is a defect: the search reporting violated
// where a linearization exists is a false positive, and the search reporting
// linearizable where none exists is a false pass.
// ---------------------------------------------------------------------------

// mustPrecede reports that a's whole interval ends before b's begins, so every
// linearization must place a before b.
//
// The comparison is STRICT, matching newSearcher's event ordering: two
// operations that touch at a single nanosecond are concurrent, not ordered.
func mustPrecede(a, b *operation) bool {
	if a.openAtEnd || b.openAtStart {
		return false
	}
	return a.returnNS < b.invokeNS
}

func permutations(n int) [][]int {
	if n == 0 {
		return [][]int{{}}
	}
	var out [][]int
	idx := make([]int, n)
	for i := range idx {
		idx[i] = i
	}
	var rec func(prefix []int, rest []int)
	rec = func(prefix, rest []int) {
		if len(rest) == 0 {
			cp := make([]int, len(prefix))
			copy(cp, prefix)
			out = append(out, cp)
			return
		}
		for i := range rest {
			next := make([]int, 0, len(rest)-1)
			next = append(next, rest[:i]...)
			next = append(next, rest[i+1:]...)
			rec(append(prefix, rest[i]), next)
		}
	}
	rec(nil, idx)
	return out
}

func respectsRealTime(ops []*operation, perm []int) bool {
	for a := 0; a < len(perm); a++ {
		for b := a + 1; b < len(perm); b++ {
			if mustPrecede(ops[perm[b]], ops[perm[a]]) {
				return false
			}
		}
	}
	return true
}

// simulateFrom walks one candidate serial order over a last-write-wins register.
// An indeterminate write branches: applied first, then not-applied.
func simulateFrom(ops []*operation, perm []int, pos int, state Value) bool {
	if pos == len(perm) {
		return true
	}
	op := ops[perm[pos]]
	if op.kind == opRead {
		if !state.Known() {
			return simulateFrom(ops, perm, pos+1, op.value)
		}
		if !state.Equal(op.value) {
			return false
		}
		return simulateFrom(ops, perm, pos+1, state)
	}
	if simulateFrom(ops, perm, pos+1, op.value) {
		return true
	}
	if op.maybe {
		return simulateFrom(ops, perm, pos+1, state)
	}
	return false
}

// referenceLinearizable is the independent answer.
func referenceLinearizable(ops []*operation) bool {
	for _, perm := range permutations(len(ops)) {
		if !respectsRealTime(ops, perm) {
			continue
		}
		if simulateFrom(ops, perm, 0, unknownValue) {
			return true
		}
	}
	return false
}

func describe(ops []*operation) string {
	var b strings.Builder
	for _, op := range ops {
		kind := "R"
		if op.kind == opWrite {
			kind = "W"
			if op.maybe {
				kind = "W?"
			}
		}
		lo, hi := fmt.Sprint(op.invokeNS), fmt.Sprint(op.returnNS)
		if op.openAtStart {
			lo = "-inf"
		}
		if op.openAtEnd {
			hi = "+inf"
		}
		fmt.Fprintf(&b, "%s%s[%s,%s] ", kind, op.value.String(), lo, hi)
	}
	return strings.TrimSpace(b.String())
}

func TestSearchAgreesWithABruteForceReference(t *testing.T) {
	rng := rand.New(rand.NewSource(0xC0FFEE))
	const trials = 3000

	var (
		nLin  int
		nViol int
	)
	for trial := 0; trial < trials; trial++ {
		n := 2 + rng.Intn(5) // 2..6 operations
		ops := make([]*operation, 0, n)
		for i := 0; i < n; i++ {
			t0 := int64(rng.Intn(8))
			t1 := t0 + int64(rng.Intn(5))
			kind := opRead
			if rng.Intn(2) == 0 {
				kind = opWrite
			}
			// A small value domain forces genuine conflicts; a wide one would
			// make almost every history trivially non-linearizable.
			val := fmt.Sprintf("%d", rng.Intn(3))
			op := mkOp(int64(i+1), kind, "k", val, t0, t1)
			if kind == opWrite && rng.Intn(4) == 0 {
				op.maybe = true
			}
			if rng.Intn(12) == 0 {
				op.openAtStart = true
			}
			if rng.Intn(12) == 0 {
				op.openAtEnd = true
			}
			ops = append(ops, op)
		}

		want := referenceLinearizable(ops)
		s := newSearcher(ops, 10_000_000, time.Now().Add(30*time.Second), 64<<20)
		got := s.run()
		if got == outcomeExhausted {
			t.Fatalf("trial %d exhausted a 10M-state budget on %d operations: %s", trial, n, describe(ops))
		}
		gotLin := got == outcomeLinearizable
		if gotLin != want {
			verdict := "violated"
			if gotLin {
				verdict = "linearizable"
			}
			refVerdict := "violated"
			if want {
				refVerdict = "linearizable"
			}
			t.Fatalf("trial %d: search says %s, brute force says %s\nhistory: %s",
				trial, verdict, refVerdict, describe(ops))
		}
		if want {
			nLin++
		} else {
			nViol++
		}
	}

	// Guard against a vacuous differential test: if every generated history fell
	// on one side, agreement would prove nothing.
	if nLin < trials/10 || nViol < trials/10 {
		t.Fatalf("the generator is one-sided: %d linearizable, %d violated out of %d",
			nLin, nViol, trials)
	}
	t.Logf("%d histories: %d linearizable, %d violated, all agreeing with brute force",
		trials, nLin, nViol)
}

// The reference itself must be able to tell the two cases apart, or the
// differential test above is comparing two constants.
func TestBruteForceReferenceIsDiscriminating(t *testing.T) {
	lin := []*operation{
		mkOp(1, opWrite, "k", "1", 0, 1),
		mkOp(2, opRead, "k", "1", 2, 3),
	}
	if !referenceLinearizable(lin) {
		t.Fatal("the reference called a linearizable history non-linearizable")
	}
	viol := []*operation{
		mkOp(1, opWrite, "k", "1", 0, 1),
		mkOp(2, opWrite, "k", "2", 2, 3),
		mkOp(3, opRead, "k", "1", 4, 5),
	}
	if referenceLinearizable(viol) {
		t.Fatal("the reference called the stale-read shape linearizable")
	}
}

// The loader and the search must agree end to end, not just at the search layer.
func TestLoaderAndSearchAgreeWithTheReferenceEndToEnd(t *testing.T) {
	rng := rand.New(rand.NewSource(0x5EED))
	for trial := 0; trial < 400; trial++ {
		n := 2 + rng.Intn(4)
		recs := make([]rec, 0, 2*n)
		ops := make([]*operation, 0, n)
		var ts int64
		for i := 0; i < n; i++ {
			id := int64(i + 1)
			t0 := ts + int64(rng.Intn(3))
			t1 := t0 + int64(rng.Intn(4))
			ts += int64(1 + rng.Intn(3))
			val := fmt.Sprintf("%d", rng.Intn(3))
			if rng.Intn(2) == 0 {
				recs = append(recs, rOK(id, "k", t0, t1, val)...)
				ops = append(ops, mkOp(id, opRead, "k", val, t0, t1))
			} else if rng.Intn(4) == 0 {
				recs = append(recs, wInfo(id, "k", t0, t1, val)...)
				w := mkOp(id, opWrite, "k", val, t0, t1)
				w.maybe, w.openAtEnd = true, true
				ops = append(ops, w)
			} else {
				recs = append(recs, wOK(id, "k", t0, t1, val)...)
				ops = append(ops, mkOp(id, opWrite, "k", val, t0, t1))
			}
		}

		got := run1(t, recs)
		want := referenceLinearizable(ops)

		hasRead := false
		for _, op := range ops {
			if op.kind == opRead {
				hasRead = true
			}
		}
		if !hasRead {
			// A history with no determinate read is refused, by design.
			wantStatus(t, got, "inconclusive")
			continue
		}
		if want && got.Status != "ok" {
			t.Fatalf("trial %d: end to end says %q for a linearizable history\n%s\n%s",
				trial, got.Status, describe(ops), got.Explanation)
		}
		if !want && got.Status != "violated" {
			t.Fatalf("trial %d: end to end says %q for a NON-linearizable history\n%s\n%s",
				trial, got.Status, describe(ops), got.Explanation)
		}
	}
}

// canonicalValue is on the soundness path; a value that fails to canonicalise
// must never be silently equated with another.
func TestCanonicalValueIsInjectiveOnDistinctScalars(t *testing.T) {
	seen := map[string]string{}
	for _, raw := range []string{"0", "1", "2", "-1", "null", "true", "false", `""`, `"0"`, "[]", "{}"} {
		v, err := canonicalValue(json.RawMessage(raw))
		if err != nil {
			t.Fatalf("canonicalValue(%q): %v", raw, err)
		}
		if prev, ok := seen[v.canon]; ok {
			t.Fatalf("%q and %q both canonicalise to %q", prev, raw, v.canon)
		}
		seen[v.canon] = raw
	}
}
