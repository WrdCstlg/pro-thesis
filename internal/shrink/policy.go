package shrink

import "fmt"

// Policy defaults. Each is a decision with a cost, so each is named.
const (
	// DefaultAcceptTrials is 1: RULE 2's asymmetry. A single reproduction
	// PROVES the candidate still fails; asking for a second buys nothing and
	// doubles the cost of every accepted reduction, which is the branch a
	// successful shrink takes most often.
	//
	// "PROVES" is true for a DETERMINISTIC predicate and false for this one, and
	// the difference is not academic. ddmin's guarantee assumes the predicate is a
	// function of the input; under Tier B it is a coin weighted by the input, and
	// a single reproduction is then one sample, not a proof. Accepting a subset on
	// one lucky world DISCARDS the elements it dropped permanently (ddmin does
	// not revisit them) so a false accept is unrecoverable in a way a false
	// reject is not.
	//
	// MEASURED. Shrinking a 14-fault world whose essential fault was a leader
	// partition (run r_2026_09_09_2904 and its faults-only repeat), ddmin walked
	// 14 -> 7 -> 3, each accepted after ONE world, and the surviving 3 were the
	// benign noise faults scheduled BEFORE the partition:
	//
	//	net.latency(kv-n1, mean=20)@500..1000
	//	net.latency(kv-n2, mean=20)@1000..1500
	//	net.loss(kv-n3, pct=1)@1500..2000
	//
	// The partition was gone, and the confirmation gate then scored the result
	// 0/3. The gate caught it; the budget was already spent.
	//
	// Note the direction of the bias this default creates against
	// DefaultRejectTrials of 2: one hit accepts, two misses reject, so the search
	// leans toward accepting reductions; the direction that over-reduces. It is
	// left at 1 because raising it doubles the cost of every accepted reduction
	// and the right value depends on a target's reproduction probability, which
	// the pipeline does not know. It is now reachable from the CLI
	// (`--accept-trials`) and the report says so when the gate fails. See OQ-053.
	DefaultAcceptTrials = 1

	// DefaultRejectTrials is 2. A single non-reproduction proves almost nothing
	// under Tier B, and a wrong rejection keeps a fault that does not matter. At
	// a per-execution reproduction probability p the chance of a wrong rejection
	// falls from (1-p) to (1-p)^2, and only the REJECTION branch pays for it.
	//
	// Not higher, because rejection is the common branch: raising it to 3
	// multiplies the cost of the whole ddmin by 1.5 against a budget D-029
	// already had to fight for.
	DefaultRejectTrials = 2

	// DefaultUnknownRetries is 1. An UNKNOWN execution is not evidence, so it
	// must be retried rather than counted; but a candidate that is unknown twice
	// is usually unknown for a structural reason (the topology cannot boot),
	// and retrying it forever spends the budget on the one thing that cannot
	// answer.
	DefaultUnknownRetries = 1

	// DefaultConfirmK is the directive's confirmation gate: k/k, default 3/3.
	DefaultConfirmK = 3

	// DefaultNarrowGranularityMS stops the timing binary search once the
	// interval is below 100ms. The fault grammar's windows are milliseconds, so
	// narrowing could in principle run to 1ms: at log2(5300) ~ 13 probes per
	// direction per fault, which is a budget D-029 does not have. 100ms costs
	// ~6 probes and is finer than the fixture's own 600ms minimum election
	// timeout, so it is below the resolution at which the mechanism changes.
	DefaultNarrowGranularityMS = 100

	// DefaultNarrowMaxProbes bounds each direction of each fault's search
	// independently of the granularity, so a pathological window cannot consume
	// the whole budget on one fault.
	DefaultNarrowMaxProbes = 8

	// DefaultCalibrationProbes bounds how many times a run may re-execute the
	// UNREDUCED world to check a witness-key drift that a REDUCED candidate
	// raised (D-063). Each costs one world, and is spent only after a reduction
	// has actually shown drift: a target whose reductions never drift pays
	// nothing.
	//
	// Four, from the recorded key distribution: k/0 holds ~74% of the time on
	// the reference fixture (OQ-047), so stage 0's two draws miss a real drift
	// with probability 0.74^2 ~ 55%, and two draws plus four re-probes miss it
	// with 0.74^6 ~ 16%. Six would bring that to ~9% for two more worlds. It is
	// a Policy field precisely because that trade depends on the target.
	DefaultCalibrationProbes = 4
)

// Policy is every knob that decides HOW a shrink runs, as distinct from Budget,
// which decides how long.
//
// The zero value is the recommended policy. Fields that would want to default to
// true are spelled as negatives (SkipBaseline, DisableCache) precisely so that
// is true: a caller who fills in nothing gets the sound configuration, and every
// weakening is a field somebody had to write.
type Policy struct {
	// Strictness is RULE 1's identity test. The zero value is MatchUnset, which
	// withDefaults resolves to MatchWitnessKey: the Strictness type has no
	// match level at zero precisely so that leaving this field alone cannot
	// select the weakest test in the package.
	Strictness Strictness

	// AcceptTrials is how many reproductions accept a reduction. Zero means
	// DefaultAcceptTrials.
	AcceptTrials int
	// RejectTrials is how many consecutive non-reproductions reject one. Zero
	// means DefaultRejectTrials.
	RejectTrials int
	// UnknownRetries is how many additional executions an unjudgeable candidate
	// gets before it is abandoned as UNKNOWN. Zero means DefaultUnknownRetries.
	UnknownRetries int

	// ConfirmK is the confirmation gate's k. Zero means DefaultConfirmK.
	ConfirmK int

	// Parallelism is how many candidates the pipeline offers the executor at
	// once. Zero or one means serial. It is capped by the executor: an Executor
	// that is not a BatchExecutor runs one at a time whatever this says.
	//
	// This package never chooses a number that would exceed the host's Docker
	// bridge-network ceiling, because it does not know one. The executor
	// (control.ParallelRunner) computes and enforces that budget, and refuses
	// before anything boots (D-043).
	Parallelism int

	// NarrowGranularityMS stops the window binary search. Zero means
	// DefaultNarrowGranularityMS.
	NarrowGranularityMS int64
	// NarrowMaxProbes bounds each direction of each window search. Zero means
	// DefaultNarrowMaxProbes.
	NarrowMaxProbes int
	// SkipNarrowing turns stage 3 off. Reported as skipped, never as complete.
	SkipNarrowing bool

	// SkipBaseline turns off the pre-flight check that the UNREDUCED world still
	// reproduces.
	//
	// Setting it is a considered risk: without the baseline, a world whose
	// violation does not replay makes every ddmin rejection meaningless, and the
	// pipeline spends its whole budget concluding "no fault could be removed";
	// a confident-looking negative result produced entirely by noise.
	SkipBaseline bool

	// SkipOps turns off stage 2, the ddmin over the operation trace.
	//
	// It exists so the pipeline can REPORT the right reason. Stage 2 not running
	// has three causes (the caller declined it, the driver has no {plan_path}
	// seam (D-021), or no trace was supplied) and they call for three different
	// responses. Without this field the first is indistinguishable from the
	// second, and the report tells a caller who typed --skip-ops that their
	// driver cannot accept an op plan: a false statement about their system,
	// produced by the tool, in the artifact they are meant to trust.
	SkipOps bool

	// DisableCache stops the prober memoizing candidate decisions.
	//
	// The cache is on by default because ddmin re-tests the same subsets across
	// rounds and each test costs a ~30s world. A cached REJECTION already
	// carries RejectTrials executions of evidence, which is exactly the evidence
	// a fresh evaluation would gather, so reusing it is not weaker than the
	// policy: it is the same policy, paid for once. A caller who wants an
	// independent sample every time sets this.
	DisableCache bool

	// CalibrationProbes is how many times the run may re-execute the UNREDUCED
	// world to answer a witness-key drift question that a REDUCED candidate
	// raised (D-063). Zero means DefaultCalibrationProbes. Negative disables the
	// re-probe, which makes calibration stage-0-only: reduced candidates can
	// then never move the level, at the measured cost that stage 0's two draws
	// miss a real drift ~55% of the time on the reference fixture.
	CalibrationProbes int
}

func (p Policy) withDefaults() Policy {
	out := p
	if out.Strictness == MatchUnset {
		out.Strictness = MatchWitnessKey
	}
	if out.AcceptTrials <= 0 {
		out.AcceptTrials = DefaultAcceptTrials
	}
	if out.RejectTrials <= 0 {
		out.RejectTrials = DefaultRejectTrials
	}
	if out.UnknownRetries < 0 {
		out.UnknownRetries = 0
	} else if out.UnknownRetries == 0 {
		out.UnknownRetries = DefaultUnknownRetries
	}
	if out.ConfirmK <= 0 {
		out.ConfirmK = DefaultConfirmK
	}
	if out.Parallelism <= 0 {
		out.Parallelism = 1
	}
	if out.NarrowGranularityMS <= 0 {
		out.NarrowGranularityMS = DefaultNarrowGranularityMS
	}
	if out.NarrowMaxProbes <= 0 {
		out.NarrowMaxProbes = DefaultNarrowMaxProbes
	}
	if out.CalibrationProbes < 0 {
		out.CalibrationProbes = 0
	} else if out.CalibrationProbes == 0 {
		out.CalibrationProbes = DefaultCalibrationProbes
	}
	return out
}

// Validate refuses a policy that cannot produce a sound shrink.
func (p Policy) Validate() error {
	if !p.Strictness.Valid() {
		return fmt.Errorf("shrink: unknown Strictness(%d)", int(p.Strictness))
	}
	if p.Strictness == MatchOracle {
		// Reachable only by an explicit assignment, because the zero value is
		// MatchUnset. The directive's floor is oracle AND class: an oracle name
		// alone would accept a consistency violation as a reduction of a crash
		// violation from the same oracle.
		return fmt.Errorf("shrink: Strictness MatchOracle is below the directive's floor " +
			"(\"at minimum oracle name and class must match\"); use MatchOracleClass or stronger")
	}
	if p.AcceptTrials < 0 || p.RejectTrials < 0 || p.ConfirmK < 0 {
		return fmt.Errorf("shrink: AcceptTrials, RejectTrials and ConfirmK must not be negative")
	}
	if p.RejectTrials > 0 && p.AcceptTrials > 0 && p.AcceptTrials > p.RejectTrials {
		// The asymmetry is the design (RULE 2). Inverting it spends the budget
		// on the branch that is already proven and starves the branch that is
		// not.
		return fmt.Errorf("shrink: AcceptTrials (%d) exceeds RejectTrials (%d), inverting the "+
			"confirmation asymmetry: one reproduction proves a candidate still fails, one "+
			"non-reproduction proves almost nothing", p.AcceptTrials, p.RejectTrials)
	}
	return nil
}
