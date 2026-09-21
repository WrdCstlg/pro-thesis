package shrink

import (
	"context"
	"reflect"
	"sort"
	"testing"
)

// syntheticTest turns a PURE predicate over a subset into a SubsetTest.
//
// It scans the batch in index order and answers with the first subset that
// reproduces: the same "lowest index wins" rule the real prober applies, so the
// algorithm tested here is the algorithm that runs against Docker.
func syntheticTest(reproduces func(subset []int) bool, calls *int) SubsetTest {
	return func(ctx context.Context, subsets [][]int) (int, StopReason) {
		for i, s := range subsets {
			*calls++
			if reproduces(s) {
				return i, StopComplete
			}
		}
		return -1, StopComplete
	}
}

func containsAll(subset []int, want ...int) bool {
	set := map[int]struct{}{}
	for _, e := range subset {
		set[e] = struct{}{}
	}
	for _, w := range want {
		if _, ok := set[w]; !ok {
			return false
		}
	}
	return true
}

func containsAny(subset []int, want ...int) bool {
	for _, e := range subset {
		for _, w := range want {
			if e == w {
				return true
			}
		}
	}
	return false
}

// assertOneMinimal is the property ddmin actually promises: removing any single
// remaining element stops it reproducing. It is checked directly rather than
// inferred from the returned size, so a result that happens to be small for the
// wrong reason still fails.
func assertOneMinimal(t *testing.T, got []int, reproduces func([]int) bool) {
	t.Helper()
	if !reproduces(got) {
		t.Fatalf("ddmin returned %v, which does not reproduce: the result must always reproduce", got)
	}
	for i := range got {
		less := make([]int, 0, len(got)-1)
		less = append(less, got[:i]...)
		less = append(less, got[i+1:]...)
		if reproduces(less) {
			t.Fatalf("ddmin returned %v but %v still reproduces: the result is not 1-minimal", got, less)
		}
	}
}

func TestDDMinTheClassicCases(t *testing.T) {
	cases := []struct {
		name       string
		n          int
		reproduces func([]int) bool
		want       []int
		// wantAny lets a case accept any one of several equally 1-minimal
		// answers, which "individually removable but not jointly" genuinely has.
		wantAny [][]int
	}{
		{
			name:       "a single essential element",
			n:          8,
			reproduces: func(s []int) bool { return containsAll(s, 3) },
			want:       []int{3},
		},
		{
			name:       "two elements essential TOGETHER, neither alone",
			n:          8,
			reproduces: func(s []int) bool { return containsAll(s, 2, 5) },
			want:       []int{2, 5},
		},
		{
			name: "individually removable but not jointly",
			// Either of 1 and 6 suffices, so removing either alone still
			// reproduces and removing both does not. A 1-minimal answer is
			// exactly one of them.
			n:          8,
			reproduces: func(s []int) bool { return containsAny(s, 1, 6) },
			wantAny:    [][]int{{1}, {6}},
		},
		{
			name:       "every element is essential",
			n:          5,
			reproduces: func(s []int) bool { return len(s) == 5 },
			want:       []int{0, 1, 2, 3, 4},
		},
		{
			name: "no element is needed at all",
			// The empty set reproduces. ddmin must find that, which is the
			// question "does the workload alone produce this bug".
			n:          6,
			reproduces: func(s []int) bool { return true },
			want:       []int{},
		},
		{
			name:       "one element, essential",
			n:          1,
			reproduces: func(s []int) bool { return containsAll(s, 0) },
			want:       []int{0},
		},
		{
			name:       "one element, not needed",
			n:          1,
			reproduces: func(s []int) bool { return true },
			want:       []int{},
		},
		{
			name:       "no elements at all",
			n:          0,
			reproduces: func(s []int) bool { return true },
			want:       []int{},
		},
		{
			name:       "three of fourteen, spread out",
			n:          14,
			reproduces: func(s []int) bool { return containsAll(s, 0, 7, 13) },
			want:       []int{0, 7, 13},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			got, stop := DDMin(context.Background(), tc.n, syntheticTest(tc.reproduces, &calls))
			if stop != StopComplete {
				t.Fatalf("stop = %q, want complete", stop)
			}
			sort.Ints(got)
			if tc.wantAny != nil {
				ok := false
				for _, w := range tc.wantAny {
					if reflect.DeepEqual(got, w) {
						ok = true
					}
				}
				if !ok {
					t.Fatalf("got %v, want one of %v", got, tc.wantAny)
				}
			} else if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			assertOneMinimal(t, got, tc.reproduces)
			t.Logf("%d element(s) -> %v in %d test(s)", tc.n, got, calls)
		})
	}
}

// TestDDMinIsAlwaysOneMinimal sweeps every predicate of the form "reproduces iff
// the subset contains this particular essential set" over a small universe, so
// the 1-minimality claim is checked across the whole space rather than on the
// handful of shapes somebody thought of.
func TestDDMinIsAlwaysOneMinimal(t *testing.T) {
	const n = 6
	for mask := 1; mask < 1<<n; mask++ {
		want := []int{}
		for i := 0; i < n; i++ {
			if mask&(1<<i) != 0 {
				want = append(want, i)
			}
		}
		reproduces := func(s []int) bool { return containsAll(s, want...) }
		calls := 0
		got, stop := DDMin(context.Background(), n, syntheticTest(reproduces, &calls))
		if stop != StopComplete {
			t.Fatalf("essential=%v: stop = %q", want, stop)
		}
		sort.Ints(got)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("essential=%v: got %v, want exactly the essential set", want, got)
		}
	}
}

// TestDDMinNeverReturnsANonReproducingSet is the soundness floor: whatever else
// happens, the answer must still reproduce. A "minimal repro" that does not
// reproduce is worse than no shrink at all.
func TestDDMinNeverReturnsANonReproducingSet(t *testing.T) {
	// A deliberately non-monotone predicate: an odd-sized subset containing 2
	// reproduces, an even-sized one does not. ddmin gives no minimality
	// guarantee here, but it must never hand back something that fails the test.
	reproduces := func(s []int) bool { return containsAll(s, 2) && len(s)%2 == 1 }
	calls := 0
	got, stop := DDMin(context.Background(), 9, syntheticTest(reproduces, &calls))
	if stop != StopComplete {
		t.Fatalf("stop = %q", stop)
	}
	if !reproduces(got) {
		t.Fatalf("got %v, which does not reproduce", got)
	}
}

// TestDDMinStopsWhenTheTestStops pins RULE 3 at the algorithm level: a budget
// that expires mid-search returns the set reached so far, with the reason.
func TestDDMinStopsWhenTheTestStops(t *testing.T) {
	budget := 3
	reproduces := func(s []int) bool { return containsAll(s, 5) }
	test := func(ctx context.Context, subsets [][]int) (int, StopReason) {
		for i, s := range subsets {
			if budget <= 0 {
				return -1, StopWorldBudget
			}
			budget--
			if reproduces(s) {
				return i, StopComplete
			}
		}
		return -1, StopComplete
	}
	got, stop := DDMin(context.Background(), 12, test)
	if stop != StopWorldBudget {
		t.Fatalf("stop = %q, want %q", stop, StopWorldBudget)
	}
	if !reproduces(got) {
		t.Fatalf("a truncated ddmin returned %v, which does not reproduce", got)
	}
	if len(got) >= 12 {
		t.Fatalf("got %v (%d elements); the truncated result should still carry the reduction "+
			"it had confirmed", got, len(got))
	}
	t.Logf("truncated at %d test(s): 12 -> %d element(s)", 3, len(got))
}

func TestDDMinHonoursContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := 0
	got, stop := DDMin(ctx, 8, syntheticTest(func(s []int) bool { return true }, &calls))
	if stop != StopCanceled {
		t.Fatalf("stop = %q, want %q", stop, StopCanceled)
	}
	if len(got) != 8 {
		t.Fatalf("got %d element(s); a cancelled search reduces nothing", len(got))
	}
	if calls != 0 {
		t.Fatalf("%d test(s) ran after cancellation, want 0", calls)
	}
}

func TestPartitionCoversEveryElementExactlyOnce(t *testing.T) {
	for size := 1; size <= 12; size++ {
		c := make([]int, size)
		for i := range c {
			c[i] = i
		}
		for n := 1; n <= size; n++ {
			parts := partition(c, n)
			if len(parts) != n {
				t.Fatalf("size=%d n=%d: got %d part(s)", size, n, len(parts))
			}
			seen := map[int]int{}
			for _, p := range parts {
				if len(p) == 0 {
					t.Fatalf("size=%d n=%d: empty part in %v", size, n, parts)
				}
				for _, e := range p {
					seen[e]++
				}
			}
			if len(seen) != size {
				t.Fatalf("size=%d n=%d: covered %d element(s), want %d", size, n, len(seen), size)
			}
			for e, count := range seen {
				if count != 1 {
					t.Fatalf("size=%d n=%d: element %d appears %d times", size, n, e, count)
				}
			}
		}
	}
}

func TestComplementsAreTheSetMinusEachPart(t *testing.T) {
	c := []int{0, 1, 2, 3, 4}
	parts := partition(c, 3)
	comps := complements(c, parts)
	if len(comps) != len(parts) {
		t.Fatalf("got %d complement(s) for %d part(s)", len(comps), len(parts))
	}
	for i := range parts {
		if len(comps[i])+len(parts[i]) != len(c) {
			t.Fatalf("part %v and complement %v do not partition %v", parts[i], comps[i], c)
		}
		for _, e := range parts[i] {
			for _, g := range comps[i] {
				if e == g {
					t.Fatalf("element %d is in both part %v and complement %v", e, parts[i], comps[i])
				}
			}
		}
	}
}

// TestDDMinCostIsBoundedInPractice records what the algorithm actually costs, so
// D-029's budget arithmetic is grounded in a measurement rather than an estimate.
func TestDDMinCostIsBoundedInPractice(t *testing.T) {
	for _, n := range []int{4, 8, 14, 20} {
		calls := 0
		reproduces := func(s []int) bool { return containsAll(s, 0, n/2) }
		got, _ := DDMin(context.Background(), n, syntheticTest(reproduces, &calls))
		t.Logf("n=%2d -> %d element(s) in %2d subset test(s)", n, len(got), calls)
		if calls > 4*n+8 {
			t.Fatalf("n=%d took %d test(s); ddmin should stay near linear on this shape", n, calls)
		}
	}
}
