package shrink

import (
	"context"
	"fmt"
	"time"
)

// StageName identifies one stage of the pipeline.
type StageName string

// The five stages, in pipeline order.
const (
	StageBaseline StageName = "baseline"
	StageFaults   StageName = "faults"
	StageOps      StageName = "ops"
	StageNarrow   StageName = "narrow"
	StageConfirm  StageName = "confirm"
)

// StageReport is what one stage did. It is the evidence behind a PARTIAL result:
// "14 faults to 6" is only useful if a reader can see WHICH stage got that far
// and which never ran.
type StageReport struct {
	Name StageName
	// Attempted is false when the stage was skipped. Skipped says why.
	Attempted bool
	// Skipped is the reason the stage did not run, when it did not. It is never
	// empty for a stage with Attempted false: a stage that silently does
	// nothing is indistinguishable from one that found nothing to do.
	Skipped string
	// Before and After are the stage's own units: faults for StageFaults, ops
	// for StageOps, faults for StageNarrow (unchanged, since narrowing shortens
	// windows rather than removing faults).
	Before, After int
	// Worlds is how many executions this stage cost.
	Worlds int
	// Elapsed is its wall cost.
	Elapsed time.Duration
	// Stop is why it stopped early, if it did.
	Stop StopReason
	// Note carries the stage's own commentary, e.g. the baseline's reason for
	// refusing to proceed.
	Note string
}

// pick returns the elements of xs at the given indices, in index order.
func pick[T any](xs []T, idx []int) []T {
	out := make([]T, 0, len(idx))
	for _, i := range idx {
		if i >= 0 && i < len(xs) {
			out = append(out, xs[i])
		}
	}
	return out
}

// runBaseline is stage 0: prove the unreduced world still reproduces the target
// before spending a budget reducing it.
//
// It returns the stage report, the calibration note (empty when none was needed),
// and the stop reason: StopComplete meaning "the pipeline may proceed".
//
// CALIBRATION. The baseline re-executes the world with NOTHING removed, so every
// violation it produces is by construction the same defect. If one of them
// matches the target on oracle AND class and differs only in the witness key,
// that is not a switched bug: it is a measurement proving the key is a scheduling
// artifact for this target rather than part of its identity.
//
// Continuing at MatchWitnessKey past that measurement would reject every
// legitimate reduction and report "nothing could be removed": identity.go's own
// stated worst outcome, a false negative that reads exactly like a correct one.
// It is not hypothetical: 27 recorded linearizable.kv violations of the ONE
// injected fixture defect land on three different keys (k/0 ×20, k/1 ×5, k/2 ×2),
// and two runs produced two different keys within a single run. See OQ-047.
//
// Four things keep this from being a gate quietly weakening itself:
//
//  1. It may only calibrate a DEFAULT. A caller who typed `--strictness key`
//     gets the level they asked for and a failed baseline, because overriding an
//     explicit instruction is the one thing an anti-gaming rule must never do.
//  2. It stops at MatchOracleClass (the directive's own floor) and the
//     demotion is one-way and one-step. A candidate that switches ORACLE or
//     CLASS is still rejected, which is Rule 1's whole substance.
//  3. The evidence comes from the UNREDUCED world. A REDUCED candidate may RAISE
//     the question (it is a free sample, and stage 0's two draws miss a real
//     drift ~55% of the time on this fixture) but only a re-execution of the
//     unreduced world may ANSWER it, at most Policy.CalibrationProbes times per
//     run (D-063). Instability there is a fact about the target, not something a
//     reduction can manufacture: a candidate that drops the essential fault and
//     fires the same oracle on another key can cost the run a re-probe, and
//     cannot move the level. Pinned by
//     TestReducedCandidatesMayAskButOnlyTheUnreducedWorldMayAnswer.
//  4. It is LOUD. The note returned here reaches Result.Calibration, the stage
//     report, and the rendered CLI output. Rule 1 forbids SILENTLY changing the
//     bug; this changes what "the same bug" means, on evidence, in writing.
func runBaseline(ctx context.Context, p *prober, led *ledger, orig Identity,
	c Candidate, mayCalibrate bool) (StageReport, string, StopReason) {

	stage := StageReport{Name: StageBaseline, Attempted: true,
		Before: len(c.Faults), After: len(c.Faults)}
	t0, w0 := led.elapsed(), p.worldsRun()
	cost := func() {
		stage.Worlds, stage.Elapsed = p.worldsRun()-w0, led.elapsed()-t0
	}

	calibratable := mayCalibrate && orig.Key != "" && p.strictness() == MatchWitnessKey
	calibrate := func(drift Identity) string {
		note := calibrationNote(orig, drift)
		p.logf("baseline: %s", note)
		p.relaxTo(MatchOracleClass)
		p.recordCalibration(note)
		return note
	}

	d, stop := p.test(ctx, c)
	cost()
	stage.Stop = stop

	switch {
	case stop != StopComplete:
		stage.Note = "the baseline could not be completed: " + stop.Describe()
		return stage, "", stop

	case d.Signal == SignalUnknown:
		stage.Note = "the unreduced world could not be judged: " + d.Reason
		return stage, "", StopBaselineUnjudgeable

	case d.Signal == SignalReproduced:
		// The world reproduces. Now the SECOND question, which is a different
		// question and needs its own execution: is the witness key part of this
		// defect's identity, or an artifact of which client was in flight?
		//
		// Asking it here, deliberately, rather than waiting for the answer to
		// surface as a rejection, is what makes the answer independent of luck.
		// A baseline that happens to land on the target's key and then rejects
		// every reduction for landing elsewhere reaches the same false negative
		// by a route no amount of retrying can detect; measured on this fixture:
		// see OQ-047.
		//
		// It costs exactly one world, and only for a keyed target at the default
		// level. Against a pipeline that routinely spends tens, buying a
		// determinate identity criterion for one is the cheap side of the trade.
		if !calibratable {
			stage.Note = fmt.Sprintf("the unreduced world reproduced %s in %d world(s)",
				orig.String(), d.Trials)
			return stage, "", StopComplete
		}
		ids, stop := p.probeOnce(ctx, c)
		cost()
		if stop != StopComplete {
			// No budget for the experiment. The baseline itself succeeded, so the
			// pipeline proceeds at the level it already has; saying so beats
			// failing a run that has not yet done anything wrong.
			stage.Note = fmt.Sprintf("the unreduced world reproduced %s in %d world(s); the "+
				"witness-key stability probe did not run (%s), so %s stands unverified",
				orig.String(), d.Trials, stop.Describe(), MatchWitnessKey)
			return stage, "", StopComplete
		}
		for _, got := range ids {
			if got.Key == "" || got.Key == orig.Key {
				continue
			}
			if ok, _ := MatchOracleClass.Match(orig, got); ok {
				note := calibrate(got)
				stage.Note = fmt.Sprintf("the unreduced world reproduced %s in %d world(s), then "+
					"on re-execution %s", orig.String(), d.Trials, note)
				return stage, note, StopComplete
			}
		}
		// Phrased as what this stage MEASURED, not as a conclusion about the run.
		// Two executions agreeing is weak evidence (on this fixture the most
		// common key holds about three quarters of the time, so two draws agree
		// more often than not) and the run may still calibrate later on evidence
		// this probe was too small to see. A note claiming the level "stands"
		// would then be contradicted by the same report's Calibration line.
		stage.Note = fmt.Sprintf("the unreduced world reproduced %s in %d world(s) and held "+
			"witness key %q on re-execution, so nothing HERE contradicts %s; later stages keep "+
			"checking", orig.String(), d.Trials, orig.Key, MatchWitnessKey)
		return stage, "", StopComplete
	}

	// The baseline did NOT reproduce. If what it produced instead was this same
	// defect on another key, the evidence for calibrating is already in hand and
	// no extra world is needed to collect it.
	drift := p.keyDrift()
	if drift == nil || !calibratable {
		stage.Note = fmt.Sprintf("the unreduced world did not reproduce %s in %d world(s): %s",
			orig.String(), d.Trials, d.Reason)
		return stage, "", StopBaselineNotReproduced
	}
	note := calibrate(*drift)

	d, stop = p.test(ctx, c)
	cost()
	stage.Stop = stop
	switch {
	case stop != StopComplete:
		stage.Note = note + "; the baseline could not then be completed: " + stop.Describe()
		return stage, note, stop
	case d.Signal == SignalReproduced:
		stage.Note = fmt.Sprintf("%s; the unreduced world then reproduced %s (%s) in %d world(s)",
			note, orig.Oracle, orig.Class, d.Trials)
		return stage, note, StopComplete
	case d.Signal == SignalUnknown:
		stage.Note = note + "; the unreduced world could not then be judged: " + d.Reason
		return stage, note, StopBaselineUnjudgeable
	default:
		stage.Note = fmt.Sprintf("%s; the unreduced world still did not reproduce it: %s",
			note, d.Reason)
		return stage, note, StopBaselineNotReproduced
	}
}

// shrinkFaults is stage 1: ddmin over the fault list.
//
// This is "the stage that matters most and the cheapest to get right": a fault
// schedule is short (14 at the directive's worst case, ONE on this fixture per
// D-053), so ddmin converges in a few tens of worlds, and every fault it removes
// removes a whole causal story an agent would otherwise have to read.
func shrinkFaults(ctx context.Context, p *prober, faults []string, ops []int64) ([]string, StopReason) {
	keep, stop := DDMin(ctx, len(faults), func(ctx context.Context, subsets [][]int) (int, StopReason) {
		cands := make([]Candidate, 0, len(subsets))
		for _, s := range subsets {
			cands = append(cands, Candidate{Faults: pick(faults, s), Ops: ops})
		}
		i, _, stop := p.firstReproducing(ctx, cands)
		return i, stop
	})
	return pick(faults, keep), stop
}

// shrinkOps is stage 2: ddmin over the operation trace.
//
// It runs only for a driver that honours the {plan_path} seam (D-021). For any
// other driver the stage is REPORTED as not attempted rather than skipped
// silently, because "we did not shrink the workload" and "the workload could not
// be shrunk further" are different facts and only one of them is evidence.
func shrinkOps(ctx context.Context, p *prober, faults []string, ops []int64) ([]int64, StopReason) {
	keep, stop := DDMin(ctx, len(ops), func(ctx context.Context, subsets [][]int) (int, StopReason) {
		cands := make([]Candidate, 0, len(subsets))
		for _, s := range subsets {
			sel := pick(ops, s)
			if sel == nil {
				// A nil Ops means "the whole workload" to the executor, which is
				// the opposite of what an empty subset asks for. Keep it
				// non-nil.
				sel = []int64{}
			}
			cands = append(cands, Candidate{Faults: faults, Ops: sel})
		}
		i, _, stop := p.firstReproducing(ctx, cands)
		return i, stop
	})
	sel := pick(ops, keep)
	if sel == nil {
		sel = []int64{}
	}
	return sel, stop
}
