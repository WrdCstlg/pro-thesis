package shrink

import (
	"context"
	"strings"
	"testing"
)

// Stage 0 calibration, and the four things that keep it from being a gate
// quietly weakening itself.
//
// The rule being defended is RULE 1: "a shrink cannot SILENTLY change the bug."
// Calibration changes what counts as the same bug, so every test here is really
// asking the same question from a different side: is the change measured, is it
// bounded, is it refusable, and is it audible?

// keyDriftingExecutor reproduces the defect on a key the target never named,
// EVERY time, which is what the fixture does when the target's key was one of the
// rare ones: the shrink of world r_2026_09_09_2b4e targeted k/1 and both baseline
// executions of the identical world came back k/0.
//
// Note that a retry cannot rescue this and is not supposed to. The existing
// AcceptTrials/RejectTrials retry already absorbs an occasional drift; what it
// cannot absorb is a target key that is simply improbable, and that is the case
// calibration exists for.
func keyDriftingExecutor(full []string) *fakeExecutor {
	return newFake(func(c Candidate, nth int) Attempt {
		if len(c.Faults) == len(full) || hasFault(c, full[0]) {
			return reproducedAs(sameBugDifferentKey)
		}
		return clean()
	})
}

func TestStage0CalibratesAwayAWitnessKeyTheUnreducedWorldItselfCannotHold(t *testing.T) {
	full := faultList(3)
	fake := keyDriftingExecutor(full)

	res, err := Run(context.Background(), Options{
		Executor: fake,
		Original: origID, // key k/0
		Faults:   full,
		// Policy.Strictness deliberately UNSET: this is the default path, the
		// only one calibration is allowed to touch.
		Policy: Policy{SkipNarrowing: true, ConfirmK: 1},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if res.Calibration == "" {
		t.Fatalf("the unreduced world reproduced the defect on key %q when the target named %q, "+
			"and the shrink did not calibrate; at oracle+class+witness_key every legitimate "+
			"reduction is now rejected and the run will report 'nothing could be removed' — "+
			"a false negative that reads exactly like a correct one (stop=%v, %d -> %d faults)",
			sameBugDifferentKey.Key, origID.Key, res.Stop, res.FaultsBefore, res.FaultsAfter)
	}
	if res.Policy.Strictness != MatchOracleClass {
		t.Fatalf("calibrated strictness is %s, want %s: the demotion must land on the "+
			"directive's floor and nowhere else", res.Policy.Strictness, MatchOracleClass)
	}
	if res.Stop != StopComplete {
		t.Fatalf("stopped with %v after calibrating; the point of calibrating was to let the "+
			"pipeline proceed", res.Stop)
	}
	if res.FaultsAfter >= res.FaultsBefore {
		t.Fatalf("faults %d -> %d: the shrink still reduced nothing after calibration",
			res.FaultsBefore, res.FaultsAfter)
	}
}

// The demotion must never go BELOW the directive's floor, whatever the evidence.
// MatchOracle would accept a liveness violation as a reduction of a consistency
// one, which is the switched bug itself.
func TestCalibrationStopsAtTheFloorAndRejectsADifferentOracleAfterwards(t *testing.T) {
	full := faultList(3)
	fake := newFake(func(c Candidate, nth int) Attempt {
		if len(c.Faults) == len(full) {
			return reproducedAs(sameBugDifferentKey)
		}
		// EVERY reduction fails, but with an unrelated oracle. Post-calibration
		// these must still be rejected: the key moved, the bug did not.
		return differentBug()
	})

	res, err := Run(context.Background(), Options{
		Executor: fake,
		Original: origID,
		Faults:   full,
		Policy:   Policy{SkipNarrowing: true, ConfirmK: 1},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Calibration == "" {
		t.Fatal("no calibration happened, so this test is not exercising what it claims to")
	}
	if res.FaultsAfter != res.FaultsBefore {
		t.Fatalf("faults %d -> %d: a candidate whose only failure was %s (%s) was accepted as a "+
			"reduction of %s (%s). Calibration relaxed the WITNESS KEY; it must never have "+
			"relaxed the oracle or the class",
			res.FaultsBefore, res.FaultsAfter, aDifferentBug.Oracle, aDifferentBug.Class,
			origID.Oracle, origID.Class)
	}
	if len(res.Divergences) == 0 {
		t.Fatal("every candidate failed with a different oracle and not one was reported as a " +
			"divergence; Rule 1's report side went silent exactly when it had the most to say")
	}
}

// The refusable half. A caller who TYPED the strictness gets it, evidence or no
// evidence: overriding an explicit instruction is the one thing an anti-gaming
// rule must never do, and "the tool knew better" is how a gate stops being one.
func TestAnExplicitlyRequestedStrictnessIsNeverCalibratedAway(t *testing.T) {
	full := faultList(3)
	fake := keyDriftingExecutor(full)

	res, err := Run(context.Background(), Options{
		Executor: fake,
		Original: origID,
		Faults:   full,
		// Typed, not inherited. Identical in value to the default; not identical
		// in meaning.
		Policy: Policy{Strictness: MatchWitnessKey, SkipNarrowing: true, ConfirmK: 1},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Calibration != "" {
		t.Fatalf("an explicitly requested %s was calibrated to %s anyway: %s",
			MatchWitnessKey, res.Policy.Strictness, res.Calibration)
	}
	if res.Policy.Strictness != MatchWitnessKey {
		t.Fatalf("enforced strictness is %s, want the requested %s",
			res.Policy.Strictness, MatchWitnessKey)
	}
	if res.Stop != StopBaselineNotReproduced {
		t.Fatalf("stop is %v, want %v: at the level the caller asked for, this baseline does "+
			"not reproduce, and saying so is the correct answer",
			res.Stop, StopBaselineNotReproduced)
	}
}

// The audible half. A calibration that happened but is not written down IS the
// silent bug-change Rule 1 forbids: the demotion would be real and invisible.
func TestACalibrationIsRecordedInTheResultAndInTheStageItHappenedIn(t *testing.T) {
	full := faultList(3)
	res, err := Run(context.Background(), Options{
		Executor: keyDriftingExecutor(full),
		Original: origID,
		Faults:   full,
		Policy:   Policy{SkipNarrowing: true, ConfirmK: 1},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Calibration == "" {
		t.Fatal("no calibration recorded")
	}
	for _, want := range []string{origID.Key, sameBugDifferentKey.Key,
		MatchWitnessKey.String(), MatchOracleClass.String()} {
		if !strings.Contains(res.Calibration, want) {
			t.Errorf("Result.Calibration does not mention %q; a reader cannot tell what was "+
				"relaxed or why:\n%s", want, res.Calibration)
		}
	}

	var baseline *StageReport
	for i := range res.Stages {
		if res.Stages[i].Name == StageBaseline {
			baseline = &res.Stages[i]
		}
	}
	if baseline == nil {
		t.Fatal("no baseline stage was reported at all")
	}
	if !strings.Contains(baseline.Note, "calibrated") {
		t.Errorf("the baseline stage note does not mention the calibration that happened "+
			"inside it:\n%s", baseline.Note)
	}
	// Exactly one baseline stage, carrying the cost of BOTH probes. Two entries
	// would double-count the stage; a note without the retry's cost would
	// understate what the calibration charged.
	n := 0
	for _, st := range res.Stages {
		if st.Name == StageBaseline {
			n++
		}
	}
	if n != 1 {
		t.Errorf("the pipeline reported %d baseline stages, want 1", n)
	}
	if baseline.Worlds < 2 {
		t.Errorf("the baseline reports %d world(s); it probed, calibrated and re-probed, so it "+
			"cost at least 2 and the report must not hide the retry", baseline.Worlds)
	}
}

// Calibration is evidence-driven, and the evidence is specifically "the same
// oracle and class on a different key". A baseline that fails because a genuinely
// different bug fired must NOT be talked into proceeding.
func TestADifferentOracleAtBaselineIsNotEvidenceOfKeyInstability(t *testing.T) {
	full := faultList(3)
	fake := newFake(func(c Candidate, nth int) Attempt {
		if len(c.Faults) == len(full) {
			return differentBug()
		}
		return clean()
	})

	res, err := Run(context.Background(), Options{
		Executor: fake,
		Original: origID,
		Faults:   full,
		Policy:   Policy{SkipNarrowing: true, ConfirmK: 1},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Calibration != "" {
		t.Fatalf("the baseline produced %s (%s) — a different oracle entirely — and the shrink "+
			"treated it as a moved witness key: %s",
			aDifferentBug.Oracle, aDifferentBug.Class, res.Calibration)
	}
	if res.Stop != StopBaselineNotReproduced {
		t.Fatalf("stop is %v, want %v", res.Stop, StopBaselineNotReproduced)
	}
}

// The route that luck can hide. A baseline that happens to land on the target's
// own key proves the world reproduces and proves NOTHING about whether the key
// identifies the defect, and every reduction afterwards is then rejected for
// landing somewhere else. Stage 0 must ask the second question on purpose.
func TestAStableLookingBaselineIsStillProbedForKeyStability(t *testing.T) {
	full := faultList(3)
	fake := newFake(func(c Candidate, nth int) Attempt {
		if len(c.Faults) == len(full) {
			// First execution lands on the target's key; every later one drifts.
			// Exactly the live run r_2026_09_09_2b4e, which reproduced k/1 at
			// baseline and then saw k/0 and k/6 for the rest of the pipeline.
			if nth == 1 {
				return reproduced()
			}
			return reproducedAs(sameBugDifferentKey)
		}
		if hasFault(c, full[0]) {
			return reproducedAs(sameBugDifferentKey)
		}
		return clean()
	})

	res, err := Run(context.Background(), Options{
		Executor: fake,
		Original: origID,
		Faults:   full,
		Policy:   Policy{SkipNarrowing: true, ConfirmK: 1},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Calibration == "" {
		t.Fatalf("the baseline reproduced on the target's own key and the shrink asked no "+
			"further question; the very next execution moved the key, so every reduction was "+
			"about to be rejected for a reason the run would never report (%d -> %d faults, "+
			"confirmation %s)", res.FaultsBefore, res.FaultsAfter, res.Confirmation)
	}
	if res.FaultsAfter >= res.FaultsBefore {
		t.Fatalf("faults %d -> %d: nothing was reduced even after calibrating",
			res.FaultsBefore, res.FaultsAfter)
	}
	if !res.Confirmation.Passed() {
		t.Fatalf("confirmation %s did not pass; the minimal repro is not trustworthy",
			res.Confirmation)
	}
}

// Stage 0's probe is two executions, and two executions cannot separate "stable"
// from "usually". MEASURED: run r_2026_09_09_9d07 drew k/0 twice, concluded the
// key was part of the defect's identity, and then watched the same defect land on
// k/1, k/2, k/3 and k/4 over the following 60 worlds. k/0 holds about three
// quarters of the time here, so two draws agree more often than not.
//
// The run must therefore keep listening, but under D-063 what it listens FOR is
// a question, not an answer. A reduced candidate's drift prompts one more
// execution of the UNREDUCED world; only that execution may calibrate. Here the
// unreduced world holds its key for stage 0's two draws and drifts on the third,
// which is the re-probe a reduced candidate triggered. That, and only that, is
// the evidence the level moves on.
func TestDriftDiscoveredAfterStage0StillCalibrates(t *testing.T) {
	full := faultList(4)
	fake := newFake(func(c Candidate, nth int) Attempt {
		if len(c.Faults) == len(full) {
			// Two draws on the target's key satisfy stage 0's probe; the third
			// execution (the re-probe) is where the target's own drift shows.
			if nth >= 3 {
				return reproducedAs(sameBugDifferentKey)
			}
			return reproduced()
		}
		// Every reduction that keeps the essential fault reproduces the SAME
		// defect on a key the target never named. This raises the question; it
		// cannot answer it.
		if hasFault(c, full[0]) {
			return reproducedAs(sameBugDifferentKey)
		}
		return clean()
	})

	res, err := Run(context.Background(), Options{
		Executor: fake,
		Original: origID,
		Faults:   full,
		Policy:   Policy{SkipNarrowing: true, ConfirmK: 1},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if n := fake.seen[Candidate{Faults: full}.key()]; n < 3 {
		t.Fatalf("the unreduced world was executed %d time(s); the drift a reduced candidate "+
			"raised was never checked against it, so nothing the run learned was evidence "+
			"about the target", n)
	}
	if res.Calibration == "" {
		t.Fatalf("a reduced candidate raised drift, the unreduced world was re-executed and "+
			"itself drifted to key %q, and nothing calibrated; every reduction is now rejected "+
			"for a reason the run has measured to be false (%d -> %d faults, confirmation %s)",
			sameBugDifferentKey.Key, res.FaultsBefore, res.FaultsAfter, res.Confirmation)
	}
	if res.Policy.Strictness != MatchOracleClass {
		t.Errorf("Result.Policy.Strictness is %s; it must report the level ENFORCED, not the one "+
			"requested", res.Policy.Strictness)
	}
	if res.FaultsAfter >= res.FaultsBefore {
		t.Fatalf("faults %d -> %d: nothing was reduced", res.FaultsBefore, res.FaultsAfter)
	}
	if !res.Confirmation.Passed() {
		t.Fatalf("confirmation %s: the gate ran at the pre-calibration level and threw away a "+
			"minimal repro the run had already justified", res.Confirmation)
	}
}

// ...and the line that stops this being a gate that argues its way open.
//
// Calibration is allowed while REDUCING and forbidden while CONFIRMING. k/k is
// the number a human reads to decide whether to trust the minimal repro, and a
// gate that relaxes its own criterion partway through being applied is
// indistinguishable from a gate being gamed: however good its reasons.
func TestTheConfirmationGateCannotCalibrateItselfOpen(t *testing.T) {
	// Both faults are essential, so ddmin over {f0, f1} tests only the singletons,
	// never the pair. With the baseline skipped, the FULL set is executed for
	// the first time by the confirmation gate, which is what puts the drift where
	// no earlier stage can have seen it.
	full := faultList(2)
	fake := newFake(func(c Candidate, nth int) Attempt {
		if len(c.Faults) == len(full) {
			return reproducedAs(sameBugDifferentKey)
		}
		return clean()
	})

	res, err := Run(context.Background(), Options{
		Executor: fake,
		Original: origID,
		Faults:   full,
		Policy:   Policy{SkipBaseline: true, SkipNarrowing: true, ConfirmK: 3},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Calibration != "" {
		t.Fatalf("the only drift this run ever saw was inside the confirmation gate, and the "+
			"gate calibrated on it: %s", res.Calibration)
	}
	if res.Confirmation.Passed() {
		t.Fatalf("the confirmation gate reported %s: every replay fired on a key the target "+
			"never named, so the gate relaxed its own criterion while being applied",
			res.Confirmation)
	}
	if res.Policy.Strictness != MatchWitnessKey {
		t.Errorf("strictness is %s, want %s", res.Policy.Strictness, MatchWitnessKey)
	}
}

// The same guarantee at the seam that enforces it, so a future refactor of the
// pipeline's stage order cannot quietly lose it.
func TestAFrozenProberRefusesToCalibrateOnEvidenceItWouldOtherwiseAccept(t *testing.T) {
	newP := func() *prober {
		p := newProber(newFake(func(Candidate, int) Attempt { return clean() }),
			origID, Policy{}.withDefaults(),
			newLedger(Budget{}.withDefaults(1), nil), nil)
		// Evidence from the UNREDUCED world: the kind that may calibrate. This
		// test is about the freeze, so it supplies the kind the freeze must
		// refuse even though nothing else would.
		p.noteDivergence([]Identity{sameBugDifferentKey}, true)
		return p
	}

	thawed := newP()
	thawed.allowCalibration(true)
	thawed.maybeCalibrate()
	if thawed.calibration() == "" || thawed.strictness() != MatchOracleClass {
		t.Fatalf("a prober that MAY calibrate did not, on drift evidence it holds: "+
			"strictness=%s note=%q", thawed.strictness(), thawed.calibration())
	}

	frozen := newP()
	frozen.allowCalibration(false)
	frozen.maybeCalibrate()
	if frozen.calibration() != "" {
		t.Errorf("a frozen prober calibrated anyway: %s", frozen.calibration())
	}
	if frozen.strictness() != MatchWitnessKey {
		t.Errorf("a frozen prober moved its strictness to %s", frozen.strictness())
	}
}

// The converse, and the reason this is calibration rather than a weakened
// default: a target whose key DOES hold on re-execution keeps the stricter rule.
// Tier A drivers exist, and for them the key is real signal.
func TestAKeyThatHoldsOnReExecutionKeepsTheStricterRule(t *testing.T) {
	full := faultList(3)
	fake := newFake(func(c Candidate, nth int) Attempt {
		if len(c.Faults) == len(full) || hasFault(c, full[0]) {
			// Same defect, same key, different op ids every time: OQ-034's
			// measured behaviour, which the witness-key level already tolerates.
			if nth%2 == 0 {
				return reproducedAs(sameBugDifferentOps)
			}
			return reproduced()
		}
		return clean()
	})

	res, err := Run(context.Background(), Options{
		Executor: fake,
		Original: origID,
		Faults:   full,
		Policy:   Policy{SkipNarrowing: true, ConfirmK: 1},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Calibration != "" {
		t.Fatalf("the witness key held on every execution and the level was relaxed anyway: %s",
			res.Calibration)
	}
	if res.Policy.Strictness != MatchWitnessKey {
		t.Fatalf("strictness is %s, want %s: with no evidence of drift there is nothing to "+
			"calibrate and the default must stand", res.Policy.Strictness, MatchWitnessKey)
	}
	if res.FaultsAfter >= res.FaultsBefore {
		t.Fatalf("faults %d -> %d: the run reduced nothing", res.FaultsBefore, res.FaultsAfter)
	}
}

// OQ-062 / D-063. A candidate during reduction that fires a DIFFERENT oracle or
// class is a divergence and stays one. It must never trigger calibration:
// relaxation to MatchOracleClass is permitted ONLY when oracle and class match.
func TestMidRunDifferentOracleOrClassDoesNotCalibrateStrictness(t *testing.T) {
	full := faultList(3)
	fake := newFake(func(c Candidate, nth int) Attempt {
		// Stage 0 baseline reproduces the original defect.
		if len(c.Faults) == len(full) {
			return reproduced()
		}
		// Reduced candidates fire an unrelated bug (different oracle/class).
		return reproducedAs(aDifferentBug)
	})

	res, err := Run(context.Background(), Options{
		Executor: fake,
		Original: origID,
		Faults:   full,
		Policy:   Policy{SkipNarrowing: true, ConfirmK: 1},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Calibration != "" {
		t.Fatalf("a candidate firing an unrelated oracle/class calibrated the gate: %s",
			res.Calibration)
	}
	if res.Policy.Strictness != MatchWitnessKey {
		t.Fatalf("strictness relaxed to %s; unrelated oracle/class must never trigger calibration",
			res.Policy.Strictness)
	}
}

// "We did not shrink the workload" and "the workload could not be shrunk" are
// different facts, and only one of them is evidence about the caller's system.
// Reporting the second when the first is true tells a caller who declined the
// stage that their driver lacks a seam it has.
func TestDecliningStage2IsReportedAsDecliningItNotAsAMissingDriverSeam(t *testing.T) {
	full := faultList(2)
	ops := []int64{1, 2, 3, 4}
	fake := newFake(func(c Candidate, nth int) Attempt {
		if hasFault(c, full[0]) {
			return reproduced()
		}
		return clean()
	})

	res, err := Run(context.Background(), Options{
		Executor: fake,
		Original: origID,
		Faults:   full,
		Ops:      ops,
		OpPlan:   true, // the seam IS available
		Policy:   Policy{SkipOps: true, SkipNarrowing: true, ConfirmK: 1},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var st *StageReport
	for i := range res.Stages {
		if res.Stages[i].Name == StageOps {
			st = &res.Stages[i]
		}
	}
	if st == nil {
		t.Fatal("no ops stage was reported at all")
	}
	if strings.Contains(st.Skipped, "does not accept") {
		t.Fatalf("the caller passed SkipOps with OpPlan available, and the report blames their "+
			"driver for lacking the {plan_path} seam:\n%s", st.Skipped)
	}
	if !strings.Contains(st.Skipped, "SkipOps") {
		t.Errorf("the ops stage does not say who declined it:\n%s", st.Skipped)
	}
}

// A target with no witness key at all (a crash oracle, say) has no key to drift,
// so there is nothing to calibrate and the baseline's refusal must stand.
func TestATargetWithNoWitnessKeyIsNeverCalibrated(t *testing.T) {
	full := faultList(3)
	keyless := Identity{Oracle: origID.Oracle, Class: origID.Class, Severity: origID.Severity}
	fake := newFake(func(c Candidate, nth int) Attempt {
		if len(c.Faults) == len(full) {
			return differentBug()
		}
		return clean()
	})

	res, err := Run(context.Background(), Options{
		Executor: fake,
		Original: keyless,
		Faults:   full,
		Policy:   Policy{SkipNarrowing: true, ConfirmK: 1},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Calibration != "" {
		t.Fatalf("a target with no witness key was calibrated on witness-key evidence: %s",
			res.Calibration)
	}
}

// relaxTo only ever widens, and it must undo the judgements made under the
// stricter rule. A rejection cached under oracle+class+witness_key that survived
// the demotion would keep a legitimate reduction rejected for the whole run, and
// the cache is keyed by candidate, not by the rule it was judged under.
func TestRelaxToWidensOnlyAndDiscardsWhatTheStricterRuleDecided(t *testing.T) {
	p := newProber(newFake(func(Candidate, int) Attempt { return clean() }),
		origID, Policy{Strictness: MatchWitnessKey}.withDefaults(),
		newLedger(Budget{}.withDefaults(1), nil), nil)

	p.noteDivergence([]Identity{sameBugDifferentKey, aDifferentBug}, true)
	p.memo(Candidate{Faults: []string{"x"}}, decision{Signal: SignalDifferent})

	// Narrowing is refused outright rather than silently applied.
	p.relaxTo(MatchWitnessOps)
	if got := p.strictness(); got != MatchWitnessKey {
		t.Fatalf("relaxTo(%s) TIGHTENED the level to %s; it may only widen",
			MatchWitnessOps, got)
	}

	p.relaxTo(MatchOracleClass)
	if got := p.strictness(); got != MatchOracleClass {
		t.Fatalf("strictness is %s, want %s", got, MatchOracleClass)
	}
	if _, ok := p.cached(Candidate{Faults: []string{"x"}}); ok {
		t.Error("a decision made under the stricter rule survived the demotion; the cache is " +
			"keyed by candidate, not by the rule that judged it, so that rejection would " +
			"outlive the rule it was based on")
	}
	got := p.divergences()
	for _, d := range got {
		if d.key() == sameBugDifferentKey.key() {
			t.Errorf("%s is still reported as a DIVERGENT violation after the calibration that "+
				"declared it the same bug; the report would contradict its own explanation", d)
		}
	}
	found := false
	for _, d := range got {
		if d.key() == aDifferentBug.key() {
			found = true
		}
	}
	if !found {
		t.Errorf("%s stopped being a divergence when the WITNESS KEY was relaxed; it differs by "+
			"oracle and class, which no calibration touches", aDifferentBug)
	}
}
