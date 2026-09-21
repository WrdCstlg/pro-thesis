package shrink

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

func stageOf(t *testing.T, r *Result, name StageName) StageReport {
	t.Helper()
	for _, s := range r.Stages {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("no %s stage in %v", name, r.Stages)
	return StageReport{}
}

// needsFault reproduces only while the candidate still carries want.
func needsFault(want string) func(Candidate, int) Attempt {
	return func(c Candidate, nth int) Attempt {
		if hasFault(c, want) {
			return reproduced()
		}
		return clean()
	}
}

// ---------------------------------------------------------------------------
// The whole pipeline
// ---------------------------------------------------------------------------

func TestTheHappyPathShrinksNarrowsAndConfirms(t *testing.T) {
	faults := []string{
		"proc.pause(kv-n2)@1000..1500",
		"net.partition(kv-n1)@3000..5000",
		"proc.slow(kv-n3, cpu_pct=50)@6000..6500",
		"net.latency(kv-n1, mean=200)@7000..7500",
	}
	for i := range faults {
		c, err := schema.CanonicalFault(faults[i])
		if err != nil {
			t.Fatalf("fixture fault %d: %v", i, err)
		}
		faults[i] = c
	}
	// D-053's measured reproduction: the partition alone, and only while its
	// window still covers 3800..4200.
	essential := faults[1]
	fake := newFake(func(c Candidate, nth int) Attempt {
		for _, f := range c.Faults {
			spec, err := schema.ParseFault(f)
			if err != nil {
				return unjudgeable()
			}
			if spec.Kind == schema.FaultNetPartition && spec.StartMS <= 3800 && spec.EndMS >= 4200 {
				return reproduced()
			}
		}
		return clean()
	})

	res, err := Run(context.Background(), Options{
		Original: origID,
		Faults:   faults,
		Executor: fake,
		Budget:   Budget{Worlds: -1, Wall: -1},
	})
	if err != nil {
		t.Fatal(err)
	}

	if !res.Attempted || !res.Complete || res.Stop != StopComplete {
		t.Fatalf("attempted=%v complete=%v stop=%q", res.Attempted, res.Complete, res.Stop)
	}
	if res.FaultsBefore != 4 || res.FaultsAfter != 1 {
		t.Fatalf("faults %d -> %d, want 4 -> 1", res.FaultsBefore, res.FaultsAfter)
	}
	surviving, err := schema.ParseFault(res.SurvivingFaults[0])
	if err != nil {
		t.Fatalf("surviving fault is unparseable: %v", err)
	}
	orig, _ := schema.ParseFault(essential)
	if surviving.Kind != orig.Kind || surviving.Target.String() != orig.Target.String() {
		t.Fatalf("survived %s, want the %s", surviving, orig)
	}
	if surviving.StartMS < orig.StartMS || surviving.EndMS > orig.EndMS {
		t.Fatalf("the surviving window %d..%d is outside the original %d..%d",
			surviving.StartMS, surviving.EndMS, orig.StartMS, orig.EndMS)
	}
	if surviving.DurationMS() >= orig.DurationMS() {
		t.Fatalf("stage 3 narrowed nothing: %dms", surviving.DurationMS())
	}

	if !res.Confirmation.Passed() {
		t.Fatalf("confirmation = %s, want the gate met", res.Confirmation.Describe())
	}
	if got := res.Confirmation.String(); got != "3/3" {
		t.Fatalf("reproduced = %q, want 3/3", got)
	}
	mr := res.MinimalRepro()
	if mr == nil {
		t.Fatal("MinimalRepro() = nil after a 3/3 confirmation")
	}
	if mr.Reproduced != "3/3" || mr.World != "w_shrunk.thesis" ||
		mr.Cmd != "thesis replay w_shrunk.thesis" {
		t.Fatalf("minimal_repro = %+v", mr)
	}

	sh := res.Shrink()
	if !sh.Attempted || sh.FaultsBefore != 4 || sh.FaultsAfter != 1 || len(sh.SurvivingFaults) != 1 {
		t.Fatalf("shrink = %+v", sh)
	}
	t.Logf("%s", res.Describe())
	t.Logf("%d world(s) total", res.WorldsRun)
}

// ---------------------------------------------------------------------------
// Failure mode #3: a repro that does not reproduce
// ---------------------------------------------------------------------------

func TestABaselineThatDoesNotReproduceStopsThePipeline(t *testing.T) {
	fake := newFake(func(c Candidate, nth int) Attempt { return clean() })
	res, err := Run(context.Background(), Options{
		Original: origID,
		Faults:   faultList(14),
		Executor: fake,
		Budget:   Budget{Worlds: -1, Wall: -1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Stop != StopBaselineNotReproduced {
		t.Fatalf("stop = %q, want %q", res.Stop, StopBaselineNotReproduced)
	}
	if !res.Attempted {
		t.Fatal("attempted = false; the pipeline ran, it just could not proceed")
	}
	if res.Complete {
		t.Fatal("complete = true on a pipeline that never shrank anything")
	}
	if res.FaultsAfter != res.FaultsBefore {
		t.Fatalf("faults %d -> %d; nothing was shrunk, so nothing may be claimed",
			res.FaultsBefore, res.FaultsAfter)
	}
	if res.MinimalRepro() != nil {
		t.Fatal("a minimal_repro was published for a world that did not reproduce")
	}
	if want := (Policy{}).withDefaults().RejectTrials; res.WorldsRun != want {
		t.Fatalf("spent %d world(s) discovering the baseline does not reproduce, want %d; "+
			"the point of the baseline is that it is cheap", res.WorldsRun, want)
	}
	b := stageOf(t, res, StageBaseline)
	if !strings.Contains(b.Note, "did not reproduce") {
		t.Fatalf("baseline note = %q", b.Note)
	}
	for _, name := range []StageName{StageFaults, StageOps, StageNarrow, StageConfirm} {
		for _, s := range res.Stages {
			if s.Name == name && s.Attempted {
				t.Fatalf("%s ran after the baseline refused", name)
			}
		}
	}
}

func TestAnUnjudgeableBaselineIsNotReportedAsANonReproduction(t *testing.T) {
	fake := newFake(func(c Candidate, nth int) Attempt { return unjudgeable() })
	res, err := Run(context.Background(), Options{
		Original: origID,
		Faults:   faultList(3),
		Executor: fake,
		Budget:   Budget{Worlds: -1, Wall: -1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Stop != StopBaselineUnjudgeable {
		t.Fatalf("stop = %q, want %q: \"it did not reproduce\" is a claim about the bug and "+
			"\"it could not be judged\" is a claim about the harness", res.Stop, StopBaselineUnjudgeable)
	}
	if res.MinimalRepro() != nil {
		t.Fatal("a minimal_repro was published for a world nobody could judge")
	}
}

func TestMinimalReproIsNeverFabricated(t *testing.T) {
	cases := []struct {
		name string
		conf Confirmation
		want string // "" means nil
	}{
		{"nothing ran", Confirmation{K: 3}, ""},
		{"zero of three", Confirmation{K: 3, Trials: 3, Reproduced: 0}, ""},
		{"one of three is honest and is published", Confirmation{K: 3, Trials: 3, Reproduced: 1}, "1/3"},
		{"two of three is published as two of three", Confirmation{K: 3, Trials: 3, Reproduced: 2}, "2/3"},
		{"three of three", Confirmation{K: 3, Trials: 3, Reproduced: 3}, "3/3"},
		{"one of five, the directive's own example", Confirmation{K: 5, Trials: 5, Reproduced: 1}, "1/5"},
		{"a truncated gate reports what it measured", Confirmation{K: 3, Trials: 1, Reproduced: 1}, "1/1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := minimalRepro("w.thesis", tc.conf)
			if tc.want == "" {
				if got != nil {
					t.Fatalf("got %+v, want nil: there is no honest k/n here", got)
				}
				return
			}
			if got == nil {
				t.Fatalf("got nil, want %q", tc.want)
			}
			if got.Reproduced != tc.want {
				t.Fatalf("reproduced = %q, want %q", got.Reproduced, tc.want)
			}
		})
	}

	// And the gate is separate from the report: a 2/3 is published AND fails.
	if (Confirmation{K: 3, Trials: 3, Reproduced: 2}).Passed() {
		t.Fatal("2/3 passed a 3/3 gate")
	}
	if !(Confirmation{K: 3, Trials: 3, Reproduced: 3}).Passed() {
		t.Fatal("3/3 did not pass a 3/3 gate")
	}
	if (Confirmation{K: 3, Trials: 2, Reproduced: 2}).Passed() {
		t.Fatal("2/2 passed a 3/3 gate; the gate requires k trials, not merely no failures")
	}
	if minimalRepro("", Confirmation{K: 3, Trials: 3, Reproduced: 3}) != nil {
		t.Fatal("a minimal_repro was published with no world to point at")
	}
}

// TestTheConfirmationGateDoesNotStopEarly: the gate measures a RATE, so a run
// that stopped as soon as it had enough successes would report a number biased
// upward by construction.
func TestTheConfirmationGateDoesNotStopEarly(t *testing.T) {
	fake := newFake(func(c Candidate, nth int) Attempt { return reproduced() })
	p := newProber(fake, origID, Policy{}.withDefaults(), unlimited(), nil)
	got := confirm(context.Background(), p, Candidate{Faults: faultList(1)}, 5)
	if got.Trials != 5 || got.Reproduced != 5 {
		t.Fatalf("got %s, want 5/5", got.String())
	}
	if fake.total() != 5 {
		t.Fatalf("ran %d world(s) for a 5-trial gate", fake.total())
	}
}

func TestTheConfirmationGateCountsADifferentViolationAsAFailure(t *testing.T) {
	fake := newFake(func(c Candidate, nth int) Attempt {
		if nth == 2 {
			return differentBug()
		}
		return reproduced()
	})
	p := newProber(fake, origID, Policy{}.withDefaults(), unlimited(), nil)
	got := confirm(context.Background(), p, Candidate{Faults: faultList(1)}, 3)
	if got.Reproduced != 2 || got.Trials != 3 || got.Different != 1 {
		t.Fatalf("got %+v, want 2/3 with one divergence", got)
	}
	if got.Passed() {
		t.Fatal("a gate with a replay that failed at a DIFFERENT oracle passed")
	}
	if !strings.Contains(got.Describe(), "DIFFERENT") {
		t.Fatalf("describe = %q; the divergence must be visible", got.Describe())
	}
}

// ---------------------------------------------------------------------------
// RULE 3: budget expiry produces a PARTIAL result with honest counts
// ---------------------------------------------------------------------------

func TestBudgetExpiryProducesAPartialShrinkWithHonestCounts(t *testing.T) {
	faults := faultList(14)
	fake := newFake(needsFault(faults[7]))

	res, err := Run(context.Background(), Options{
		Original: origID,
		Faults:   faults,
		Executor: fake,
		// Eight worlds: one for the baseline, four for stage 1, three reserved
		// for the gate. Stage 1 cannot finish, and must say so.
		Budget: Budget{Worlds: 8, Wall: -1, ConfirmReserve: 3},
		Policy: Policy{SkipNarrowing: true},
	})
	if err != nil {
		t.Fatal(err)
	}

	if !res.Attempted {
		t.Fatal("attempted = false on a truncated run; a truncated attempt is still an attempt")
	}
	if res.Complete {
		t.Fatal("complete = true on a run the budget cut short: NEVER report a shrink as " +
			"complete when it was truncated")
	}
	if res.Stop != StopWorldBudget {
		t.Fatalf("stop = %q, want %q", res.Stop, StopWorldBudget)
	}
	if res.FaultsBefore != 14 {
		t.Fatalf("faults_before = %d, want 14", res.FaultsBefore)
	}
	if res.FaultsAfter >= res.FaultsBefore {
		t.Fatalf("faults_after = %d; a partial shrink must still report the reduction it "+
			"achieved (14 -> 6 is useful)", res.FaultsAfter)
	}
	if res.FaultsAfter != len(res.SurvivingFaults) {
		t.Fatalf("faults_after = %d but %d surviving fault(s) listed",
			res.FaultsAfter, len(res.SurvivingFaults))
	}
	if !hasFault(Candidate{Faults: res.SurvivingFaults}, faults[7]) {
		t.Fatalf("the partial result dropped the fault that actually matters: %v",
			res.SurvivingFaults)
	}
	if res.WorldsRun > 8 {
		t.Fatalf("ran %d world(s) against a budget of 8", res.WorldsRun)
	}
	// The reserve did its job: the partial repro was still measured.
	if !res.Confirmation.Ran() {
		t.Fatal("confirmation never ran; the reserve exists precisely so a PARTIAL shrink " +
			"can still say whether its own world reproduces")
	}
	if res.Confirmation.Trials != 3 {
		t.Fatalf("confirmation ran %d trial(s), want the reserved 3", res.Confirmation.Trials)
	}
	if !strings.Contains(res.Describe(), "PARTIAL") {
		t.Fatalf("Describe() = %q; a truncated result must say so", res.Describe())
	}
	sh := res.Shrink()
	if !sh.Attempted {
		t.Fatal("schema.Shrink.Attempted = false on a truncated shrink")
	}
	t.Logf("%s", res.Describe())
}

func TestTheConfirmationReserveSurvivesAnExhaustedStageBudget(t *testing.T) {
	faults := faultList(6)
	fake := newFake(func(c Candidate, nth int) Attempt { return reproduced() })
	res, err := Run(context.Background(), Options{
		Original: origID,
		Faults:   faults,
		Executor: fake,
		// Four worlds, three reserved: the baseline is all the stages get.
		Budget: Budget{Worlds: 4, Wall: -1, ConfirmReserve: 3},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Confirmation.Trials != 3 || res.Confirmation.Reproduced != 3 {
		t.Fatalf("confirmation = %s, want 3/3 from the reserve", res.Confirmation.String())
	}
	if res.WorldsRun != 4 {
		t.Fatalf("ran %d world(s), want exactly the budget of 4", res.WorldsRun)
	}
	if res.Complete {
		t.Fatal("complete = true although the stages never ran")
	}
}

func TestAWallBudgetThatExpiresPublishesNoFabricatedRepro(t *testing.T) {
	clock := newStepClock(2 * time.Second)
	fake := newFake(func(c Candidate, nth int) Attempt { return reproduced() })
	res, err := Run(context.Background(), Options{
		Original: origID,
		Faults:   faultList(10),
		Executor: fake,
		Budget:   Budget{Wall: 6 * time.Second, Worlds: -1, ConfirmReserve: 3},
		now:      clock.now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Stop != StopWallBudget {
		t.Fatalf("stop = %q, want %q", res.Stop, StopWallBudget)
	}
	if res.Complete {
		t.Fatal("complete = true on a wall-expired run")
	}
	if res.Confirmation.Ran() {
		t.Fatalf("confirmation ran %d trial(s) after the wall budget expired; the wall ceiling "+
			"is hard unless ConfirmWallGrace is set", res.Confirmation.Trials)
	}
	if res.MinimalRepro() != nil {
		t.Fatal("a minimal_repro was published without a single measured replay")
	}
	t.Logf("%s", res.Describe())
}

func TestConfirmWallGraceLetsTheGateRunAfterTheWallExpires(t *testing.T) {
	clock := newStepClock(2 * time.Second)
	fake := newFake(func(c Candidate, nth int) Attempt { return reproduced() })
	res, err := Run(context.Background(), Options{
		Original: origID,
		Faults:   faultList(10),
		Executor: fake,
		Budget: Budget{Wall: 6 * time.Second, Worlds: -1, ConfirmReserve: 3,
			ConfirmWallGrace: time.Hour},
		now: clock.now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Confirmation.Ran() {
		t.Fatal("ConfirmWallGrace was set and the gate still did not run")
	}
	if res.Complete {
		t.Fatal("the grace must not turn a truncated shrink into a complete one")
	}
	t.Logf("%s", res.Describe())
}

func TestCancellationStopsWithoutClaimingCompletion(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	fake := newFake(func(c Candidate, nth int) Attempt {
		cancel()
		return reproduced()
	})
	res, err := Run(ctx, Options{
		Original: origID,
		Faults:   faultList(6),
		Executor: fake,
		Budget:   Budget{Worlds: -1, Wall: -1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Complete {
		t.Fatal("complete = true on a cancelled run")
	}
	if res.Stop != StopCanceled {
		t.Fatalf("stop = %q, want %q", res.Stop, StopCanceled)
	}
	if res.Confirmation.Ran() {
		t.Fatal("the gate ran after cancellation")
	}
}

// ---------------------------------------------------------------------------
// Stage 2: the operation trace
// ---------------------------------------------------------------------------

func TestOpShrinkingIsReportedAsNotAttemptedWithoutThePlanSeam(t *testing.T) {
	ops := []int64{10, 20, 30, 40, 50, 60, 70, 80}
	faults := faultList(3)
	fake := newFake(needsFault(faults[1]))
	res, err := Run(context.Background(), Options{
		Original: origID,
		Faults:   faults,
		Ops:      ops,
		OpPlan:   false, // the fixture's loadgen, as it stands
		Executor: fake,
		Budget:   Budget{Worlds: -1, Wall: -1},
		Policy:   Policy{SkipNarrowing: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	s := stageOf(t, res, StageOps)
	if s.Attempted {
		t.Fatal("stage 2 ran against a driver that cannot accept an op plan")
	}
	if !strings.Contains(s.Skipped, "plan_path") {
		t.Fatalf("skip reason = %q; it must name the seam that is missing", s.Skipped)
	}
	if res.OpsBefore != len(ops) || res.OpsAfter != len(ops) {
		t.Fatalf("ops %d -> %d, want %d -> %d: the world drove the same workload before and "+
			"after, and claiming otherwise would be a fabricated reduction",
			res.OpsBefore, res.OpsAfter, len(ops), len(ops))
	}
	if res.Ops != nil {
		t.Fatalf("Ops = %v, want nil when stage 2 did not run", res.Ops)
	}
	// Every candidate must have been executed with the whole workload.
	for _, c := range fake.log {
		if c.Ops != nil {
			t.Fatalf("a candidate carried an op plan (%v) the driver cannot honour", c.Ops)
		}
	}
}

func TestOpShrinkingReducesTheWorkload(t *testing.T) {
	ops := []int64{10, 20, 30, 40, 50, 60, 70, 80}
	faults := faultList(3)
	fake := newFake(func(c Candidate, nth int) Attempt {
		if !hasFault(c, faults[1]) {
			return clean()
		}
		if !hasOp(c, ops, 40) {
			return clean()
		}
		return reproduced()
	})
	res, err := Run(context.Background(), Options{
		Original: origID,
		Faults:   faults,
		Ops:      ops,
		OpPlan:   true,
		Executor: fake,
		Budget:   Budget{Worlds: -1, Wall: -1},
		Policy:   Policy{SkipNarrowing: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	s := stageOf(t, res, StageOps)
	if !s.Attempted {
		t.Fatalf("stage 2 did not run: %q", s.Skipped)
	}
	if res.OpsBefore != 8 || res.OpsAfter != 1 {
		t.Fatalf("ops %d -> %d, want 8 -> 1", res.OpsBefore, res.OpsAfter)
	}
	if !reflect.DeepEqual(res.Ops, []int64{40}) {
		t.Fatalf("Ops = %v, want [40]", res.Ops)
	}
	if res.FaultsAfter != 1 {
		t.Fatalf("faults_after = %d, want 1", res.FaultsAfter)
	}
	t.Logf("%s", res.Describe())
}

func TestOpShrinkingIsSkippedWithNoTrace(t *testing.T) {
	faults := faultList(2)
	fake := newFake(needsFault(faults[0]))
	res, err := Run(context.Background(), Options{
		Original:  origID,
		Faults:    faults,
		OpPlan:    true,
		OpsBefore: 20000,
		Executor:  fake,
		Budget:    Budget{Worlds: -1, Wall: -1},
		Policy:    Policy{SkipNarrowing: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	s := stageOf(t, res, StageOps)
	if s.Attempted || s.Skipped == "" {
		t.Fatalf("stage = %+v, want a skipped stage with a reason", s)
	}
	if res.OpsBefore != 20000 || res.OpsAfter != 20000 {
		t.Fatalf("ops %d -> %d, want the caller's count carried through unchanged",
			res.OpsBefore, res.OpsAfter)
	}
}

// ---------------------------------------------------------------------------
// The frozen verdict objects
// ---------------------------------------------------------------------------

func TestTheResultProjectsOntoAValidVerdict(t *testing.T) {
	faults := []string{"net.partition(kv-n1)@3000..5000", "proc.pause(kv-n2)@8300..11000"}
	for i := range faults {
		c, err := schema.CanonicalFault(faults[i])
		if err != nil {
			t.Fatal(err)
		}
		faults[i] = c
	}
	fake := newFake(needsFault(faults[0]))
	res, err := Run(context.Background(), Options{
		Original: origID,
		Faults:   faults,
		Executor: fake,
		Budget:   Budget{Worlds: -1, Wall: -1},
		Policy:   Policy{SkipNarrowing: true},
	})
	if err != nil {
		t.Fatal(err)
	}

	out := schema.OracleOutput{
		Schema: schema.OracleOutputSchema, Oracle: origID.Oracle, Class: origID.Class,
		Status: schema.StatusViolated, Explanation: "no linearization exists",
		Witness: schema.Witness{Key: origID.Key, OpIDs: origID.OpIDs},
	}
	v := out.ToViolation("v1", schema.PhaseAssert, 11084)
	res.Apply(&v)

	verdict := schema.NewVerdict("r_2026_09_08_abcd", "gate", schema.VerdictFail)
	verdict.Violations = []schema.Violation{v}
	verdict.Normalize()
	if err := verdict.Validate(); err != nil {
		t.Fatalf("the shrink produced a verdict that does not validate: %v", err)
	}
	if v.Shrink.FaultsBefore != 2 || v.Shrink.FaultsAfter != 1 {
		t.Fatalf("shrink = %+v", v.Shrink)
	}
	if v.MinimalRepro == nil || v.MinimalRepro.Reproduced != "3/3" {
		t.Fatalf("minimal_repro = %+v", v.MinimalRepro)
	}
	b, err := schema.MarshalVerdict(&verdict)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), "net.partition(kv-n1)@3000..5000") {
		t.Fatalf("surviving_faults did not survive encoding:\n%s", b)
	}
	t.Logf("shrink object: %+v", v.Shrink)
}

func TestSurvivingFaultsAreCanonicalAndInScheduleOrder(t *testing.T) {
	r := &Result{SurvivingFaults: []string{
		"proc.pause( kv-n2 )@8300..11000",
		"net.partition(kv-n1)@3000..5000",
	}}
	got := r.Shrink().SurvivingFaults
	want := []string{"net.partition(kv-n1)@3000..5000", "proc.pause(kv-n2)@8300..11000"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("surviving_faults = %v, want %v", got, want)
	}
	for _, f := range got {
		if _, err := schema.ParseFault(f); err != nil {
			t.Fatalf("%q does not parse, so schema.Verdict.Validate would reject it: %v", f, err)
		}
	}
}

func TestNotAttemptedIsAValidShrinkObject(t *testing.T) {
	sh := NotAttempted()
	if sh.Attempted {
		t.Fatal("NotAttempted().Attempted = true")
	}
	if sh.SurvivingFaults == nil {
		t.Fatal("surviving_faults is null; schema.Verdict.Validate refuses that")
	}
	v := schema.Violation{ID: "v1", Oracle: "no_crash", Class: schema.ClassCrash, Shrink: sh}
	verdict := schema.NewVerdict("r_2026_09_08_abcd", "gate", schema.VerdictFail)
	verdict.Violations = []schema.Violation{v}
	verdict.Normalize()
	if err := verdict.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
}

func TestRunRefusesOptionsThatCannotProduceASoundShrink(t *testing.T) {
	fake := newFake(func(c Candidate, nth int) Attempt { return reproduced() })
	cases := []struct {
		name string
		opts Options
		want string
	}{
		{"no executor", Options{Original: origID}, "Executor"},
		{"no violation to shrink toward", Options{Executor: fake}, "no oracle"},
		{"an unparseable fault", Options{Original: origID, Executor: fake,
			Faults: []string{"nonsense"}}, "Faults[0]"},
		{"a match level below the floor", Options{Original: origID, Executor: fake,
			Policy: Policy{Strictness: MatchOracle}}, "floor"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Run(context.Background(), tc.opts)
			if err == nil {
				t.Fatal("Run returned no error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestSkippingTheBaselineIsRecordedAsASkip(t *testing.T) {
	faults := faultList(4)
	fake := newFake(needsFault(faults[2]))
	res, err := Run(context.Background(), Options{
		Original: origID,
		Faults:   faults,
		Executor: fake,
		Budget:   Budget{Worlds: -1, Wall: -1},
		Policy:   Policy{SkipBaseline: true, SkipNarrowing: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	b := stageOf(t, res, StageBaseline)
	if b.Attempted || !strings.Contains(b.Skipped, "SkipBaseline") {
		t.Fatalf("baseline stage = %+v", b)
	}
	if res.FaultsAfter != 1 {
		t.Fatalf("faults_after = %d, want 1", res.FaultsAfter)
	}
}

func TestBudgetDefaultsAreExplicitAndReported(t *testing.T) {
	b := Budget{}.withDefaults(DefaultConfirmK)
	if b.Wall != DefaultWallBudget || b.Worlds != DefaultWorldBudget {
		t.Fatalf("defaults = %+v", b)
	}
	if b.ConfirmReserve != DefaultConfirmK {
		t.Fatalf("ConfirmReserve = %d, want the gate size %d", b.ConfirmReserve, DefaultConfirmK)
	}
	if !strings.Contains(b.Describe(), "reserved for confirmation") {
		t.Fatalf("Describe() = %q", b.Describe())
	}
	// D-029's arithmetic: the default budget must be a fraction of the run
	// budget, not a multiple of it. 60 worlds at the measured ~30s is 30
	// minutes of execution against a 20-minute wall, so the wall binds first.
	if DefaultWorldBudget*30*time.Second <= DefaultWallBudget {
		t.Fatalf("the world budget (%d worlds) cannot exceed the wall budget (%s) at the "+
			"measured ~30s per world, or the wall ceiling would never bind",
			DefaultWorldBudget, DefaultWallBudget)
	}
}
