package shrink

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// theFixtureFault is D-053's measured one-fault reproduction of the fixture's
// stale read: net.partition(kv-n1)@3000..5000.
func theFixtureFault(t *testing.T) schema.FaultSpec {
	t.Helper()
	f, err := schema.ParseFault("net.partition(kv-n1)@3000..5000")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return f
}

// ---------------------------------------------------------------------------
// The invariant: narrowing NEVER widens a window
// ---------------------------------------------------------------------------

func TestNarrowedToRefusesEveryWidening(t *testing.T) {
	f := theFixtureFault(t)
	cases := []struct {
		name       string
		start, end int64
		wantErr    error
	}{
		{"an earlier start is a widening", 2999, 5000, ErrWiden},
		{"a later end is a widening", 3000, 5001, ErrWiden},
		{"both is a widening", 0, 100000, ErrWiden},
		{"an inverted window is not a window", 4000, 3500, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := narrowedTo(f, tc.start, tc.end)
			if err == nil {
				t.Fatalf("narrowedTo(%d, %d) = %s with no error; the window grew outside %s",
					tc.start, tc.end, got, f)
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("error = %v, want %v", err, tc.wantErr)
			}
		})
	}

	// The permitted moves still work, or the stage would be a no-op.
	for _, tc := range [][2]int64{{3000, 5000}, {3000, 4000}, {4000, 5000}, {4000, 4000}} {
		if _, err := narrowedTo(f, tc[0], tc[1]); err != nil {
			t.Fatalf("narrowedTo(%d, %d) refused a legitimate narrowing: %v", tc[0], tc[1], err)
		}
	}
}

// ---------------------------------------------------------------------------
// Convergence
// ---------------------------------------------------------------------------

// windowNeeds returns a responder that reproduces only while the fault's window
// still covers [needStart, needEnd].
func windowNeeds(t *testing.T, needStart, needEnd int64) func(Candidate, int) Attempt {
	t.Helper()
	return func(c Candidate, nth int) Attempt {
		if len(c.Faults) == 0 {
			return clean()
		}
		f, err := schema.ParseFault(c.Faults[0])
		if err != nil {
			return unjudgeable()
		}
		if f.StartMS <= needStart && f.EndMS >= needEnd {
			return reproduced()
		}
		return clean()
	}
}

func TestTimingNarrowingConvergesOnTheWindowThatMatters(t *testing.T) {
	orig := theFixtureFault(t)
	// The mechanism actually needs 3800..4200 of the 3000..5000 window.
	fake := newFake(windowNeeds(t, 3800, 4200))
	pol := Policy{}.withDefaults()
	p := newProber(fake, origID, pol, unlimited(), nil)

	got, stop := narrowOne(context.Background(), p, []string{orig.String()}, 0, nil)
	if stop != StopComplete {
		t.Fatalf("stop = %q", stop)
	}
	spec, err := schema.ParseFault(got)
	if err != nil {
		t.Fatalf("narrowOne produced an unparseable fault %q: %v", got, err)
	}

	// Contained in the original: the invariant, checked on the real output.
	if spec.StartMS < orig.StartMS || spec.EndMS > orig.EndMS {
		t.Fatalf("narrowed to %d..%d, which is not inside the original %d..%d",
			spec.StartMS, spec.EndMS, orig.StartMS, orig.EndMS)
	}
	// Still reproducing: a narrowing that broke the repro would be worse than
	// none.
	if spec.StartMS > 3800 || spec.EndMS < 4200 {
		t.Fatalf("narrowed to %d..%d, which no longer covers the 3800..4200 the mechanism needs",
			spec.StartMS, spec.EndMS)
	}
	// And actually narrower, within the granularity the policy stops at.
	if spec.DurationMS() >= orig.DurationMS() {
		t.Fatalf("narrowed to %dms from %dms: no convergence at all",
			spec.DurationMS(), orig.DurationMS())
	}
	slack := spec.DurationMS() - 400
	if slack > 2*pol.NarrowGranularityMS {
		t.Fatalf("narrowed to %dms around a 400ms requirement: %dms of slack exceeds twice the "+
			"%dms granularity", spec.DurationMS(), slack, pol.NarrowGranularityMS)
	}
	t.Logf("%s -> %s in %d probe(s)", orig, spec, fake.total())
}

// TestTimingNarrowingNeverWidensWhateverThePredicateSays sweeps the two
// pathological predicates. Neither may produce a window outside the original.
func TestTimingNarrowingNeverWidensWhateverThePredicateSays(t *testing.T) {
	orig := theFixtureFault(t)
	cases := []struct {
		name    string
		respond func(Candidate, int) Attempt
	}{
		{"nothing narrower ever reproduces", func(c Candidate, nth int) Attempt {
			f, _ := schema.ParseFault(c.Faults[0])
			if f.StartMS == 3000 && f.EndMS == 5000 {
				return reproduced()
			}
			return clean()
		}},
		{"everything reproduces", func(c Candidate, nth int) Attempt { return reproduced() }},
		{"every probe is unjudgeable", func(c Candidate, nth int) Attempt { return unjudgeable() }},
		{"reproduction flips on every execution", func(c Candidate, nth int) Attempt {
			if nth%2 == 1 {
				return clean()
			}
			return reproduced()
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFake(tc.respond)
			p := newProber(fake, origID, Policy{}.withDefaults(), unlimited(), nil)
			got, _ := narrowOne(context.Background(), p, []string{orig.String()}, 0, nil)
			spec, err := schema.ParseFault(got)
			if err != nil {
				t.Fatalf("unparseable %q: %v", got, err)
			}
			if spec.StartMS < orig.StartMS || spec.EndMS > orig.EndMS {
				t.Fatalf("narrowed to %d..%d, outside the original %d..%d",
					spec.StartMS, spec.EndMS, orig.StartMS, orig.EndMS)
			}
			if spec.EndMS < spec.StartMS {
				t.Fatalf("narrowed to an inverted window %d..%d", spec.StartMS, spec.EndMS)
			}
			if spec.Kind != orig.Kind || spec.Target.String() != orig.Target.String() {
				t.Fatalf("narrowing changed the fault itself: %s -> %s", orig, spec)
			}
			t.Logf("%-40s -> %s (%d probe(s))", tc.name, spec, fake.total())
		})
	}
}

func TestNarrowingIsBoundedByTheProbeCeiling(t *testing.T) {
	orig, err := schema.ParseFault("net.partition(kv-n1)@0..600000")
	if err != nil {
		t.Fatal(err)
	}
	// A predicate that needs the whole window forces every probe to fail, which
	// is the worst case for the search.
	fake := newFake(windowNeeds(t, 0, 600000))
	pol := Policy{NarrowMaxProbes: 4}.withDefaults()
	p := newProber(fake, origID, pol, unlimited(), nil)
	_, _ = narrowOne(context.Background(), p, []string{orig.String()}, 0, nil)

	// Each failing probe costs RejectTrials worlds, and there are at most
	// NarrowMaxProbes per direction, in two directions.
	max := 2 * pol.NarrowMaxProbes * pol.RejectTrials
	if fake.total() > max {
		t.Fatalf("narrowing cost %d world(s), above the %d the ceiling permits", fake.total(), max)
	}
	t.Logf("worst case on a 600s window: %d world(s), ceiling %d", fake.total(), max)
}

func TestNarrowingSkipsAWindowAlreadyAtGranularity(t *testing.T) {
	f, err := schema.ParseFault("net.partition(kv-n1)@3000..3050")
	if err != nil {
		t.Fatal(err)
	}
	fake := newFake(func(c Candidate, nth int) Attempt { return reproduced() })
	p := newProber(fake, origID, Policy{}.withDefaults(), unlimited(), nil)
	got, _ := narrowOne(context.Background(), p, []string{f.String()}, 0, nil)
	if got != f.String() {
		t.Fatalf("got %q, want the window unchanged", got)
	}
	if fake.total() != 0 {
		t.Fatalf("spent %d world(s) on a window already below the granularity", fake.total())
	}
}

func TestNarrowAllLeavesEveryOtherFaultAlone(t *testing.T) {
	faults := []string{
		"net.partition(kv-n1)@3000..5000",
		"proc.pause(kv-n2)@8300..11000",
	}
	for i := range faults {
		c, err := schema.CanonicalFault(faults[i])
		if err != nil {
			t.Fatal(err)
		}
		faults[i] = c
	}
	fake := newFake(func(c Candidate, nth int) Attempt {
		if len(c.Faults) != 2 {
			return clean()
		}
		return reproduced()
	})
	p := newProber(fake, origID, Policy{}.withDefaults(), unlimited(), nil)
	got, stop := narrowAll(context.Background(), p, faults, nil)
	if stop != StopComplete {
		t.Fatalf("stop = %q", stop)
	}
	if len(got) != len(faults) {
		t.Fatalf("narrowing changed the fault COUNT: %d -> %d", len(faults), len(got))
	}
	for i := range got {
		before, _ := schema.ParseFault(faults[i])
		after, err := schema.ParseFault(got[i])
		if err != nil {
			t.Fatalf("fault %d unparseable after narrowing: %v", i, err)
		}
		if after.Kind != before.Kind || after.Target.String() != before.Target.String() {
			t.Fatalf("fault %d changed identity: %s -> %s", i, before, after)
		}
		if after.StartMS < before.StartMS || after.EndMS > before.EndMS {
			t.Fatalf("fault %d widened: %s -> %s", i, before, after)
		}
	}
	t.Logf("narrowed %v", got)
}

func TestNarrowingLeavesAnUnparseableFaultAlone(t *testing.T) {
	fake := newFake(func(c Candidate, nth int) Attempt { return reproduced() })
	p := newProber(fake, origID, Policy{}.withDefaults(), unlimited(), nil)
	const junk = "not a fault at all"
	got, stop := narrowOne(context.Background(), p, []string{junk}, 0, nil)
	if got != junk || stop != StopComplete {
		t.Fatalf("got (%q, %q); an unparseable fault is the world file's problem to report, "+
			"not something this stage should rewrite", got, stop)
	}
	if fake.total() != 0 {
		t.Fatalf("spent %d world(s) narrowing an unparseable fault", fake.total())
	}
}

func TestNarrowingReportsABudgetStopWithoutLosingItsWork(t *testing.T) {
	orig := theFixtureFault(t)
	fake := newFake(windowNeeds(t, 3800, 4200))
	led := newLedger(Budget{Worlds: 2, Wall: -1}, nil)
	p := newProber(fake, origID, Policy{}.withDefaults(), led, nil)

	got, stop := narrowOne(context.Background(), p, []string{orig.String()}, 0, nil)
	if stop != StopWorldBudget {
		t.Fatalf("stop = %q, want %q", stop, StopWorldBudget)
	}
	spec, err := schema.ParseFault(got)
	if err != nil {
		t.Fatalf("unparseable %q: %v", got, err)
	}
	if spec.StartMS < orig.StartMS || spec.EndMS > orig.EndMS {
		t.Fatalf("a truncated narrowing widened the window: %s -> %s", orig, spec)
	}
	if !strings.Contains(got, "net.partition(kv-n1)") {
		t.Fatalf("got %q, want the same fault", got)
	}
}
