package shrink

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func unlimited() *ledger { return newLedger(Budget{Worlds: -1, Wall: -1}, nil) }

// ---------------------------------------------------------------------------
// RULE 2: the confirmation asymmetry
// ---------------------------------------------------------------------------

// TestAcceptanceCostsOneWorld: a single reproduction is a PROOF, so the branch a
// successful shrink takes over and over must not pay for a second world.
func TestAcceptanceCostsOneWorld(t *testing.T) {
	fake := newFake(func(c Candidate, nth int) Attempt { return reproduced() })
	p := newProber(fake, origID, Policy{}.withDefaults(), unlimited(), nil)

	d, _ := p.test(context.Background(), Candidate{Faults: faultList(2)})
	if !d.accepted() {
		t.Fatalf("signal = %s, want reproduced", d.Signal)
	}
	if fake.total() != 1 {
		t.Fatalf("acceptance cost %d world(s), want 1: one reproduction proves the candidate "+
			"still fails", fake.total())
	}
}

// TestRejectionCostsRejectTrialsWorlds is the other half: one non-reproduction
// proves almost nothing under a probabilistic replay, so the rejection branch is
// where the retries are spent.
func TestRejectionCostsRejectTrialsWorlds(t *testing.T) {
	fake := newFake(func(c Candidate, nth int) Attempt { return clean() })
	pol := Policy{}.withDefaults()
	p := newProber(fake, origID, pol, unlimited(), nil)

	d, _ := p.test(context.Background(), Candidate{Faults: faultList(2)})
	if d.accepted() {
		t.Fatal("a candidate that never reproduced was accepted")
	}
	if d.Signal != SignalClean {
		t.Fatalf("signal = %s, want clean", d.Signal)
	}
	if fake.total() != pol.RejectTrials {
		t.Fatalf("rejection cost %d world(s), want RejectTrials=%d", fake.total(), pol.RejectTrials)
	}
	if pol.RejectTrials <= pol.AcceptTrials {
		t.Fatalf("RejectTrials=%d is not greater than AcceptTrials=%d; the asymmetry is the design",
			pol.RejectTrials, pol.AcceptTrials)
	}
}

// TestAFlakyCandidateIsNotRejected is failure mode #2, directly.
//
// The candidate genuinely still reproduces, but misses on its first execution:
// exactly what Tier B's "high probability, not certainty" permits. Under the
// default policy it is ACCEPTED on the retry. The subtest that follows shows the
// policy is load-bearing rather than decorative: with RejectTrials cut to 1, the
// identical executor produces a wrong rejection, and ddmin would then keep a
// fault that does not matter.
func TestAFlakyCandidateIsNotRejected(t *testing.T) {
	flaky := func(c Candidate, nth int) Attempt {
		if nth == 1 {
			return clean()
		}
		return reproduced()
	}

	t.Run("default policy retries and accepts", func(t *testing.T) {
		fake := newFake(flaky)
		p := newProber(fake, origID, Policy{}.withDefaults(), unlimited(), nil)
		d, _ := p.test(context.Background(), Candidate{Faults: faultList(2)})
		if !d.accepted() {
			t.Fatalf("a candidate that reproduces on its second execution was rejected "+
				"(%s: %s); under Tier B one miss is not evidence", d.Signal, d.Reason)
		}
		if fake.total() != 2 {
			t.Fatalf("took %d world(s), want 2", fake.total())
		}
	})

	t.Run("RejectTrials=1 gets it wrong, which is why the default is 2", func(t *testing.T) {
		fake := newFake(flaky)
		pol := Policy{RejectTrials: 1}.withDefaults()
		p := newProber(fake, origID, pol, unlimited(), nil)
		d, _ := p.test(context.Background(), Candidate{Faults: faultList(2)})
		if d.accepted() {
			t.Fatal("RejectTrials=1 accepted a candidate that missed its first execution; " +
				"this subtest exists to show the default is doing work")
		}
	})
}

// TestAnUnknownExecutionIsNotARejection: a world that never reached a judgeable
// state says nothing about whether the removed elements mattered. Counting it as
// a non-reproduction is how ddmin keeps the wrong faults.
func TestAnUnknownExecutionIsNotARejection(t *testing.T) {
	fake := newFake(func(c Candidate, nth int) Attempt { return unjudgeable() })
	pol := Policy{}.withDefaults()
	p := newProber(fake, origID, pol, unlimited(), nil)

	d, _ := p.test(context.Background(), Candidate{Faults: faultList(2)})
	if d.Signal != SignalUnknown {
		t.Fatalf("signal = %s, want unknown: an unjudgeable world is neither a reproduction "+
			"nor a rejection", d.Signal)
	}
	if d.accepted() {
		t.Fatal("an unjudgeable candidate was accepted")
	}
	if want := pol.UnknownRetries + 1; fake.total() != want {
		t.Fatalf("took %d world(s), want %d (the first attempt plus UnknownRetries)",
			fake.total(), want)
	}
}

func TestAnUnknownExecutionIsRetriedAndCanStillReproduce(t *testing.T) {
	fake := newFake(func(c Candidate, nth int) Attempt {
		if nth == 1 {
			return unjudgeable()
		}
		return reproduced()
	})
	p := newProber(fake, origID, Policy{}.withDefaults(), unlimited(), nil)

	d, _ := p.test(context.Background(), Candidate{Faults: faultList(2)})
	if !d.accepted() {
		t.Fatalf("signal = %s (%s); an unknown first execution must be retried, not counted",
			d.Signal, d.Reason)
	}
}

// TestTheProberChargesEveryWorldToTheLedger keeps RULE 3 honest at the source:
// there is no execution path that does not cost a grant.
func TestTheProberChargesEveryWorldToTheLedger(t *testing.T) {
	fake := newFake(func(c Candidate, nth int) Attempt { return clean() })
	led := newLedger(Budget{Worlds: 3, Wall: -1, ConfirmReserve: 0}, nil)
	p := newProber(fake, origID, Policy{}.withDefaults(), led, nil)

	_, stop := p.test(context.Background(), Candidate{Faults: faultList(2)})
	if stop != StopComplete {
		t.Fatalf("stop = %q on the first candidate", stop)
	}
	// Two worlds spent. The next rejection needs two more and only one remains.
	d, stop := p.test(context.Background(), Candidate{Faults: faultList(3)})
	if stop != StopWorldBudget {
		t.Fatalf("stop = %q, want %q", stop, StopWorldBudget)
	}
	if !d.Truncated {
		t.Fatal("a budget-truncated judgement must be marked truncated, or it would be " +
			"cached and reused as though it were evidence")
	}
	if fake.total() != 3 {
		t.Fatalf("ran %d world(s) against a budget of 3", fake.total())
	}
}

func TestATruncatedJudgementIsNotCached(t *testing.T) {
	fake := newFake(func(c Candidate, nth int) Attempt { return clean() })
	led := newLedger(Budget{Worlds: 1, Wall: -1}, nil)
	p := newProber(fake, origID, Policy{}.withDefaults(), led, nil)

	c := Candidate{Faults: faultList(2)}
	if _, stop := p.test(context.Background(), c); stop != StopWorldBudget {
		t.Fatalf("stop = %q, want %q", stop, StopWorldBudget)
	}
	if _, cached := p.cached(c); cached {
		t.Fatal("a truncated judgement was cached; one unlucky moment at the ceiling would " +
			"then harden into a permanent rejection")
	}
}

func TestTheCacheStopsRepeatedCandidatesCostingWorlds(t *testing.T) {
	fake := newFake(func(c Candidate, nth int) Attempt { return clean() })
	pol := Policy{}.withDefaults()
	p := newProber(fake, origID, pol, unlimited(), nil)

	c := Candidate{Faults: faultList(2)}
	_, _ = p.test(context.Background(), c)
	first := fake.total()
	_, _ = p.test(context.Background(), c)
	if fake.total() != first {
		t.Fatalf("a repeated candidate cost %d more world(s)", fake.total()-first)
	}

	// And the switch turns it off, so a caller who wants an independent sample
	// every time can have one.
	fake2 := newFake(func(c Candidate, nth int) Attempt { return clean() })
	p2 := newProber(fake2, origID, Policy{DisableCache: true}.withDefaults(), unlimited(), nil)
	_, _ = p2.test(context.Background(), c)
	_, _ = p2.test(context.Background(), c)
	if fake2.total() != 2*pol.RejectTrials {
		t.Fatalf("DisableCache ran %d world(s), want %d", fake2.total(), 2*pol.RejectTrials)
	}
}

func TestAnExecutorErrorStopsRatherThanBurningTheBudget(t *testing.T) {
	fake := newFake(func(c Candidate, nth int) Attempt { return clean() })
	fake.fail = errors.New("docker daemon is not running")
	p := newProber(fake, origID, Policy{}.withDefaults(), unlimited(), nil)

	_, stop := p.test(context.Background(), Candidate{Faults: faultList(2)})
	if stop != StopExecutorError {
		t.Fatalf("stop = %q, want %q", stop, StopExecutorError)
	}
}

// ---------------------------------------------------------------------------
// determinism
// ---------------------------------------------------------------------------

// TestTheSameShrinkAtAnyParallelism is D-044's guarantee applied to candidates:
// the answer is the LOWEST-INDEX candidate accepted in the earliest round, never
// the first to finish, so concurrency changes the wall clock and nothing else.
func TestTheSameShrinkAtAnyParallelism(t *testing.T) {
	faults := faultList(9)
	essential := []string{faults[1], faults[6]}
	respond := func(c Candidate, nth int) Attempt {
		for _, want := range essential {
			if !hasFault(c, want) {
				return clean()
			}
		}
		return reproduced()
	}

	var got [][]string
	for _, workers := range []int{1, 2, 4, 8} {
		fake := newFake(respond)
		fake.delay = 200 * time.Microsecond
		res, err := Run(context.Background(), Options{
			Original: origID,
			Faults:   faults,
			Executor: batchExecutor{fake},
			Budget:   Budget{Worlds: -1, Wall: -1},
			Policy:   Policy{Parallelism: workers, SkipNarrowing: true},
		})
		if err != nil {
			t.Fatalf("workers=%d: %v", workers, err)
		}
		got = append(got, res.SurvivingFaults)
		if workers > 1 && fake.peakInflight() < 2 {
			t.Fatalf("workers=%d never ran two candidates at once (peak %d); the parallel "+
				"path is not being exercised", workers, fake.peakInflight())
		}
		t.Logf("workers=%d -> %d fault(s), %d world(s), peak in flight %d",
			workers, len(res.SurvivingFaults), res.WorldsRun, fake.peakInflight())
	}
	for i := 1; i < len(got); i++ {
		if !reflect.DeepEqual(got[0], got[i]) {
			t.Fatalf("parallelism changed the answer: %v vs %v", got[0], got[i])
		}
	}
	if len(got[0]) != 2 {
		t.Fatalf("shrank to %v, want the two essential faults", got[0])
	}
}

func TestParallelismIsIgnoredWithoutABatchExecutor(t *testing.T) {
	fake := newFake(func(c Candidate, nth int) Attempt { return clean() })
	p := newProber(fake, origID, Policy{Parallelism: 8}.withDefaults(), unlimited(), nil)
	if got := p.parallelism(); got != 1 {
		t.Fatalf("parallelism() = %d without a BatchExecutor, want 1: claiming a concurrency "+
			"that never happened would misreport the run", got)
	}
}

func TestABatchExecutorReturningTheWrongCountIsAnError(t *testing.T) {
	fake := newFake(func(c Candidate, nth int) Attempt { return reproduced() })
	p := newProber(shortBatch{fake}, origID, Policy{Parallelism: 4}.withDefaults(), unlimited(), nil)
	_, _, stop := p.firstReproducing(context.Background(),
		[]Candidate{{Faults: faultList(1)}, {Faults: faultList(2)}})
	if stop != StopExecutorError {
		t.Fatalf("stop = %q, want %q: a short batch would silently misalign every result "+
			"with the candidate it belongs to", stop, StopExecutorError)
	}
}

// shortBatch returns fewer attempts than it was given candidates, which would
// misattribute every result if it were not caught.
type shortBatch struct{ *fakeExecutor }

func (s shortBatch) ExecuteBatch(ctx context.Context, cs []Candidate) ([]Attempt, error) {
	a, err := s.Execute(ctx, cs[0])
	if err != nil {
		return nil, err
	}
	return []Attempt{a}, nil
}

func TestStopReasonsDescribeThemselves(t *testing.T) {
	for _, r := range []StopReason{
		StopComplete, StopWallBudget, StopWorldBudget, StopCanceled,
		StopExecutorError, StopBaselineNotReproduced, StopBaselineUnjudgeable,
	} {
		if strings.TrimSpace(r.Describe()) == "" {
			t.Fatalf("StopReason(%q) has no description", r)
		}
		if r.Truncated() != (r != StopComplete) {
			t.Fatalf("StopReason(%q).Truncated() = %v", r, r.Truncated())
		}
	}
}
