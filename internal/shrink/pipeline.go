package shrink

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// Options configures one shrink.
type Options struct {
	// Original is the violation being shrunk toward. RULE 1: every accepted
	// reduction must reproduce THIS, not merely something.
	Original Identity

	// Faults is the violating world's PLANNED schedule, as canonical fault
	// strings.
	//
	// The REALIZED schedule is what a replay should execute (D-012), because
	// role:leader and minority(kv) re-bind at injection time and a replay that
	// re-resolves them addresses a different node. Resolving planned to realized
	// is the replay owner's job; what this package needs is a list whose entries
	// it can remove and re-window, and the caller decides which list that is by
	// what it passes here. Passing the RESOLVED strings from
	// fault_schedule.realized is the sound choice for a Tier B target and is
	// what makes stage 1's removals mean the same thing on every re-execution.
	Faults []string

	// Ops are the violating world's op ids, in history order, for stage 2. Nil
	// or empty means stage 2 has nothing to work on.
	Ops []int64
	// OpsBefore is how many operations the violating world drove, when that is
	// known and Ops is not carried. Zero means len(Ops).
	OpsBefore int
	// OpPlan says the driver honours {plan_path} / PROTHESIS_PLAN_PATH and can
	// therefore be handed a reduced op list. False disables stage 2, and the
	// StageReport says so in words.
	OpPlan bool

	// Executor runs candidates. Required. Implement BatchExecutor too to let
	// Policy.Parallelism mean anything.
	Executor Executor

	Budget Budget
	Policy Policy

	// Log receives progress lines. Optional.
	Log func(string)

	// now is a clock seam for the budget tests. Nil means time.Now.
	now func() time.Time
}

// Validate refuses options that cannot produce a sound shrink.
func (o Options) Validate() error {
	if o.Executor == nil {
		return errors.New("shrink: Options.Executor is required")
	}
	if o.Original.Zero() {
		return errors.New("shrink: Options.Original names no oracle; there is nothing to shrink toward")
	}
	if err := o.Policy.Validate(); err != nil {
		return err
	}
	for i, f := range o.Faults {
		if _, err := schema.ParseFault(f); err != nil {
			return fmt.Errorf("shrink: Options.Faults[%d]: %w", i, err)
		}
	}
	return nil
}

// Result is what a shrink produced, complete or partial.
type Result struct {
	// Attempted mirrors schema.Shrink.Attempted. It is true whenever Run
	// executed, including when the budget expired immediately: the field exists
	// to express the NOT-attempted case, and a truncated attempt is still an
	// attempt.
	Attempted bool
	// Complete is true only when the pipeline ran every stage to the end.
	Complete bool
	// Stop is why it stopped, empty when it ran to completion.
	Stop StopReason

	// Original is the violation the reductions were judged against.
	Original Identity

	FaultsBefore int
	FaultsAfter  int
	OpsBefore    int
	OpsAfter     int

	// SurvivingFaults is the reduced schedule, canonical and in schedule order.
	SurvivingFaults []string
	// Ops is the reduced operation list, nil when stage 2 did not run.
	Ops []int64

	// Confirmation is stage 4's measurement.
	Confirmation Confirmation
	// WorldPath is the .thesis the confirmation replayed, when the executor
	// named one.
	WorldPath string

	// Stages is what each stage did.
	Stages []StageReport
	// Divergences are the DIFFERENT violations seen while shrinking. A non-empty
	// list is the visible trace of RULE 1 doing its job: those candidates failed,
	// and were rejected anyway because they failed at something else.
	Divergences []Identity

	// Calibration is non-empty when an execution of the UNREDUCED world proved
	// its own witness key moves between executions: asked deliberately by stage
	// 0, or asked again when a reduced candidate in stages 1-3 showed drift
	// (D-063). A reduced candidate can raise that question; only the unreduced
	// world can answer it, so the note always describes a fact about the target.
	//
	// It exists so the demotion can never be silent. Rule 1's prohibition is on
	// SILENTLY changing the bug; a calibration that is measured, bounded by the
	// floor, and printed in the report is the opposite of that, but only as long
	// as every renderer of a Result carries this field. Policy.Strictness on the
	// same Result is already the calibrated level, not the requested one.
	Calibration string

	// WorldsRun and Elapsed are the honest cost.
	WorldsRun int
	Elapsed   time.Duration
	// Budget and Policy are the resolved settings, so a report can state what
	// the numbers above were produced under.
	Budget Budget
	Policy Policy
}

// Shrink projects the result onto the frozen verdict object.
//
// Every field is measured. On a truncated run the counts are the honest partial
// ones (14 before and 6 after is a real, useful reduction) and nothing here
// claims the pipeline finished. Whether it finished is Result.Complete and
// Result.Stop, which a caller renders alongside; the frozen object has no field
// for it and none is invented.
func (r *Result) Shrink() schema.Shrink {
	surviving := make([]string, 0, len(r.SurvivingFaults))
	for _, f := range r.SurvivingFaults {
		if c, err := schema.CanonicalFault(f); err == nil {
			surviving = append(surviving, c)
			continue
		}
		// Keep it verbatim. schema.Verdict.Validate reports an unparseable
		// surviving fault by path; dropping it here would hide the defect and
		// silently understate faults_after.
		surviving = append(surviving, f)
	}
	return schema.Shrink{
		Attempted:       r.Attempted,
		FaultsBefore:    r.FaultsBefore,
		FaultsAfter:     r.FaultsAfter,
		OpsBefore:       r.OpsBefore,
		OpsAfter:        r.OpsAfter,
		SurvivingFaults: schema.SortFaultStrings(surviving),
	}
}

// MinimalRepro builds the verdict's minimal_repro from the confirmed world, or
// nil when there is nothing honest to point at. See minimalRepro.
func (r *Result) MinimalRepro() *schema.MinimalRepro {
	return minimalRepro(r.WorldPath, r.Confirmation)
}

// MinimalReproAt is MinimalRepro with the world path the caller intends to
// commit the shrunk world to, e.g. .prothesis/regressions/w_<id>.thesis.
//
// The caller supplies the path; this package still supplies the rate, and still
// refuses to name a world that reproduced zero times.
func (r *Result) MinimalReproAt(worldPath string) *schema.MinimalRepro {
	return minimalRepro(worldPath, r.Confirmation)
}

// Apply writes the shrink and the minimal repro onto a violation.
func (r *Result) Apply(v *schema.Violation) {
	v.Shrink = r.Shrink()
	if mr := r.MinimalRepro(); mr != nil {
		v.MinimalRepro = mr
	}
}

// Describe renders the result for a human or a log, in the order a reader needs
// it: what came out, whether it is trustworthy, and what it cost.
func (r *Result) Describe() string {
	var b strings.Builder
	fmt.Fprintf(&b, "shrink %s: %d -> %d fault(s)", r.Original.String(), r.FaultsBefore, r.FaultsAfter)
	if r.OpsBefore > 0 {
		fmt.Fprintf(&b, ", %d -> %d op(s)", r.OpsBefore, r.OpsAfter)
	}
	if r.Complete {
		b.WriteString("; complete")
	} else {
		fmt.Fprintf(&b, "; PARTIAL (%s)", r.Stop.Describe())
	}
	b.WriteString("; " + r.Confirmation.Describe())
	fmt.Fprintf(&b, "; %d world(s) in %s", r.WorldsRun, r.Elapsed.Round(time.Second))
	if len(r.Divergences) > 0 {
		names := make([]string, 0, len(r.Divergences))
		for _, d := range r.Divergences {
			names = append(names, d.String())
		}
		b.WriteString("; rejected candidates that failed differently: " + strings.Join(names, ", "))
	}
	return b.String()
}

// Run executes the minimization pipeline.
//
// It returns an error only for a caller mistake (bad options). Everything else
// (budget expiry, a baseline that will not reproduce, an executor that broke) is
// a RESULT with a StopReason, because those are findings about the run and a
// caller that got an error instead would have nothing to report.
func Run(ctx context.Context, opts Options) (*Result, error) {
	if err := opts.Validate(); err != nil {
		return nil, err
	}
	// Whether the caller CHOSE the identity level or merely inherited it decides
	// whether stage 0 may calibrate it. MatchUnset is the zero value and is
	// deliberately not a match level (see identity.go), which makes "nobody typed
	// this" distinguishable from "somebody typed exactly this" without a second
	// field to keep in step.
	strictnessWasDefault := opts.Policy.Strictness == MatchUnset

	pol := opts.Policy.withDefaults()
	budget := opts.Budget.withDefaults(pol.ConfirmK)
	led := newLedger(budget, opts.now)
	p := newProber(opts.Executor, opts.Original, pol, led, opts.Log)
	p.allowCalibration(strictnessWasDefault)

	opsBefore := opts.OpsBefore
	if opsBefore == 0 {
		opsBefore = len(opts.Ops)
	}

	res := &Result{
		Attempted:       true,
		Original:        opts.Original,
		FaultsBefore:    len(opts.Faults),
		FaultsAfter:     len(opts.Faults),
		OpsBefore:       opsBefore,
		OpsAfter:        opsBefore,
		SurvivingFaults: append([]string(nil), opts.Faults...),
		Budget:          budget,
		Policy:          pol,
	}
	defer func() {
		res.WorldsRun = p.worldsRun()
		res.Elapsed = led.elapsed()
		res.Divergences = p.divergences()
		// The prober owns the calibration, whichever stage triggered it, so the
		// Result reads it from there rather than from whichever stage happened to
		// notice. Policy must be the level ENFORCED, not the level requested.
		if note := p.calibration(); note != "" {
			res.Calibration = note
			res.Policy.Strictness = p.strictness()
		}
		res.Complete = res.Stop == StopComplete
		res.FaultsAfter = len(res.SurvivingFaults)
		if res.Ops != nil {
			res.OpsAfter = len(res.Ops)
		}
		res.WorldPath = res.Confirmation.World
	}()

	faults := append([]string(nil), opts.Faults...)
	var ops []int64 // nil = the whole workload

	// The prober keeps the unreduced world so that a witness-key drift raised by
	// a REDUCED candidate in stages 1-3 can be checked against it rather than
	// believed (D-063). When the baseline is skipped the prober never sees that
	// world and therefore never calibrates: there is no sound evidence to do it
	// on, and guessing is the thing the bound exists to prevent.
	if !pol.SkipBaseline {
		p.setUnreduced(Candidate{Faults: faults, Ops: ops})
	}

	// ---- stage 0: baseline -------------------------------------------------
	//
	// Failure mode #3 ("a repro that does not reproduce") is cheapest to catch
	// here, before a budget has been spent on rejections that are all noise.
	if !pol.SkipBaseline {
		stage, calibrated, stop := runBaseline(ctx, p, led, opts.Original,
			Candidate{Faults: faults, Ops: ops}, strictnessWasDefault)
		if calibrated != "" {
			res.Calibration = calibrated
			pol.Strictness = MatchOracleClass
			res.Policy = pol
		}
		res.Stages = append(res.Stages, stage)
		if stop != StopComplete {
			res.Stop = stop
			return res, nil
		}
	} else {
		res.Stages = append(res.Stages, StageReport{Name: StageBaseline,
			Skipped: "Policy.SkipBaseline: the pipeline was told not to verify that the " +
				"unreduced world still reproduces",
			Before: len(faults), After: len(faults)})
	}

	// ---- stage 1: ddmin over the fault list --------------------------------
	stop := StopComplete
	{
		stage := StageReport{Name: StageFaults, Attempted: true, Before: len(faults)}
		t0, w0 := led.elapsed(), p.worldsRun()
		kept, s := shrinkFaults(ctx, p, faults, ops)
		faults = kept
		res.SurvivingFaults = append([]string(nil), faults...)
		stage.After = len(faults)
		stage.Worlds, stage.Elapsed = p.worldsRun()-w0, led.elapsed()-t0
		stage.Stop, stop = s, s
		p.logf("stage faults: %d -> %d", stage.Before, stage.After)
		res.Stages = append(res.Stages, stage)
	}

	// ---- stage 2: ddmin over the operation trace ---------------------------
	switch {
	case stop != StopComplete:
		res.Stages = append(res.Stages, StageReport{Name: StageOps,
			Skipped: "an earlier stage stopped: " + stop.Describe(),
			Before:  opsBefore, After: opsBefore})
	case pol.SkipOps:
		res.Stages = append(res.Stages, StageReport{Name: StageOps,
			Skipped: "Policy.SkipOps: the caller declined stage 2, so the workload was not " +
				"reduced — this says nothing about whether it could have been",
			Before: opsBefore, After: opsBefore})
	case !opts.OpPlan:
		res.Stages = append(res.Stages, StageReport{Name: StageOps,
			Skipped: "the driver does not accept an explicit op plan ({plan_path} / " +
				"PROTHESIS_PLAN_PATH, D-021), so the workload cannot be reduced for this target",
			Before: opsBefore, After: opsBefore})
	case len(opts.Ops) == 0:
		res.Stages = append(res.Stages, StageReport{Name: StageOps,
			Skipped: "no operation trace was supplied",
			Before:  opsBefore, After: opsBefore})
	default:
		stage := StageReport{Name: StageOps, Attempted: true, Before: len(opts.Ops)}
		t0, w0 := led.elapsed(), p.worldsRun()
		kept, s := shrinkOps(ctx, p, faults, opts.Ops)
		ops = kept
		res.Ops = kept
		stage.After = len(kept)
		stage.Worlds, stage.Elapsed = p.worldsRun()-w0, led.elapsed()-t0
		stage.Stop, stop = s, s
		p.logf("stage ops: %d -> %d", stage.Before, stage.After)
		res.Stages = append(res.Stages, stage)
	}

	// ---- stage 3: timing narrowing -----------------------------------------
	switch {
	case stop != StopComplete:
		res.Stages = append(res.Stages, StageReport{Name: StageNarrow,
			Skipped: "an earlier stage stopped: " + stop.Describe(),
			Before:  len(faults), After: len(faults)})
	case pol.SkipNarrowing:
		res.Stages = append(res.Stages, StageReport{Name: StageNarrow,
			Skipped: "Policy.SkipNarrowing", Before: len(faults), After: len(faults)})
	case len(faults) == 0:
		res.Stages = append(res.Stages, StageReport{Name: StageNarrow,
			Skipped: "no fault survived stage 1, so there is no window to narrow",
			Before:  0, After: 0})
	default:
		stage := StageReport{Name: StageNarrow, Attempted: true,
			Before: len(faults), After: len(faults)}
		t0, w0 := led.elapsed(), p.worldsRun()
		narrowed, s := narrowAll(ctx, p, faults, ops)
		faults = narrowed
		res.SurvivingFaults = append([]string(nil), faults...)
		stage.Worlds, stage.Elapsed = p.worldsRun()-w0, led.elapsed()-t0
		stage.Stop, stop = s, s
		stage.Note = "windows narrowed to " + strings.Join(faults, ", ")
		res.Stages = append(res.Stages, stage)
	}

	res.Stop = stop

	// ---- stage 4: confirmation ---------------------------------------------
	//
	// It runs even after a truncated stage, on the reserve the budget held back
	// for exactly this. A PARTIAL shrink whose repro has been measured is useful;
	// a PARTIAL shrink that also cannot say whether its own world reproduces is
	// not.
	//
	// THE GATE IS FROZEN BEFORE IT IS APPLIED. Calibration may happen in stages
	// 0 through 3, and confirmation then runs at whatever level those settled on:
	// decided before the gate opened, on evidence gathered while reducing. It
	// may not happen DURING stage 4. A gate that relaxes itself while being
	// applied is indistinguishable from a gate being gamed, however good its
	// reasons, and k/k is the number a human reads to decide whether to trust the
	// minimal repro.
	p.allowCalibration(false)

	if stop == StopCanceled || ctx.Err() != nil {
		res.Stages = append(res.Stages, StageReport{Name: StageConfirm,
			Skipped: "the run was cancelled", Before: pol.ConfirmK, After: 0})
		if res.Stop == StopComplete {
			res.Stop = StopCanceled
		}
		return res, nil
	}
	led.openReserve()
	stage := StageReport{Name: StageConfirm, Attempted: true, Before: pol.ConfirmK}
	t0, w0 := led.elapsed(), p.worldsRun()
	res.Confirmation = confirm(ctx, p, Candidate{Faults: faults, Ops: ops}, pol.ConfirmK)
	stage.After = res.Confirmation.Reproduced
	stage.Worlds, stage.Elapsed = p.worldsRun()-w0, led.elapsed()-t0
	stage.Stop = res.Confirmation.Stop
	stage.Note = res.Confirmation.Describe()
	res.Stages = append(res.Stages, stage)
	p.logf("stage confirm: %s", res.Confirmation.Describe())
	return res, nil
}

// NotAttempted is the shrink object for a violation nobody tried to minimize.
//
// It exists so the not-attempted case has one spelling. schema.Shrink is never
// null in a verdict (`attempted` is the field that carries the distinction)
// and a hand-built zero value that forgot SurvivingFaults would fail
// schema.Verdict.Validate with "is null; write []".
func NotAttempted() schema.Shrink {
	return schema.Shrink{Attempted: false, SurvivingFaults: []string{}}
}
