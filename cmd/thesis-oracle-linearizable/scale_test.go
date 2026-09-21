package main

import (
	"fmt"
	"testing"
	"time"

	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// ---------------------------------------------------------------------------
// Scale
//
// The brief's Phase 3 plan adds a single-key driver profile of ~20,000
// operations over a few keys, spanning the fault window. A checker that cannot
// finish that history inside its budget reports INCONCLUSIVE, which is honest
// but useless: the definition of done needs a `violated`. These tests measure
// the search at that size so the claim "tractable at gate scale" is a
// measurement rather than an assertion.
// ---------------------------------------------------------------------------

// linearizableHistory builds a history that is linearizable BY CONSTRUCTION:
// operations are laid out in a serial order and given overlapping intervals, so
// the checker has to reorder concurrent operations to accept it, but a valid
// serialization is known to exist.
//
// staleAt >= 0 plants a stale read at that position in one key's sequence: it
// returns the value the register held two writes earlier, invoked strictly after
// the later write returned. That is the fixture's lease bug, at scale.
func linearizableHistory(keys, opsPerKey int, staleAt int) []rec {
	out := make([]rec, 0, 2*keys*opsPerKey)
	var id int64
	for k := 0; k < keys; k++ {
		key := fmt.Sprintf("k/%d", k)
		var last string
		var prev string
		for s := 0; s < opsPerKey; s++ {
			id++
			t0 := int64(s) * 10
			t1 := t0 + 25 // overlaps the next two operations
			proc := int64((s + k) % 16)
			switch {
			case s%3 == 0 || last == "":
				v := fmt.Sprintf("%d", id)
				out = append(out, op(id, proc, "write", key, t0, t1, v, v, schema.HistoryOK)...)
				prev, last = last, v
			case k == 0 && s == staleAt && prev != "":
				// A read of a superseded value, invoked strictly after the write
				// that superseded it returned.
				out = append(out, op(id, proc, "read", key, t1+50, t1+55, "", prev, schema.HistoryOK)...)
			default:
				out = append(out, op(id, proc, "read", key, t0, t1, "", last, schema.HistoryOK)...)
			}
		}
	}
	return out
}

func TestScaleOfAGateSizedSingleKeyHistory(t *testing.T) {
	if testing.Short() {
		t.Skip("scale test")
	}
	const keys, opsPerKey = 4, 5000 // 20,000 operations, the brief's profile size
	path := writeHistory(t, linearizableHistory(keys, opsPerKey, -1))

	start := time.Now()
	got := check(path, testOptions(), start)
	elapsed := time.Since(start)

	wantStatus(t, got, schema.StatusOK)
	t.Logf("%d operations over %d keys checked in %s", keys*opsPerKey, keys, elapsed)
	if elapsed > 15*time.Second {
		t.Fatalf("checking %d operations took %s; the search is not tractable at profile scale",
			keys*opsPerKey, elapsed)
	}
}

func TestScaleOfAGateSizedHistoryCarryingAViolation(t *testing.T) {
	if testing.Short() {
		t.Skip("scale test")
	}
	const keys, opsPerKey = 4, 5000
	path := writeHistory(t, linearizableHistory(keys, opsPerKey, 2500))

	start := time.Now()
	got := check(path, testOptions(), start)
	elapsed := time.Since(start)

	wantStatus(t, got, schema.StatusViolated)
	if got.Witness.Key != "k/0" {
		t.Fatalf("witness key = %q, want %q", got.Witness.Key, "k/0")
	}
	if len(got.Witness.OpIDs) == 0 {
		t.Fatal("a violation with an empty witness points at nothing")
	}
	t.Logf("violation on a %d-operation history found in %s; witness %v: %s",
		keys*opsPerKey, elapsed, got.Witness.OpIDs, got.Explanation)
	if elapsed > 15*time.Second {
		t.Fatalf("finding the violation took %s", elapsed)
	}
}
