package shrink

import (
	"context"
	"fmt"

	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// ---------------------------------------------------------------------------
// Stage 3: timing narrowing
//
// "Binary search timing narrowing on surviving fault windows."
//
// Each surviving fault's @start..end window is searched toward its shortest
// reproducing form: first pull the END in, then push the START out. Both
// directions are monotone in the same way (a shorter window is a subset of a
// longer one) so a binary search is the right instrument, and both are bounded
// by a granularity and a probe count so one pathological fault cannot eat the
// budget (RULE 3).
//
// THE INVARIANT: a narrowing step may never WIDEN a window. It is enforced
// structurally rather than by discipline. Every candidate window is built by
// narrowedTo, which REFUSES a start earlier or an end later than the window it
// is narrowing from; the searches then only ever move hi down and lo up, and
// because each step narrows relative to the CURRENT window, containment in the
// ORIGINAL window is transitive. TestNarrowedToRefusesEveryWidening pins the
// refusal, so a future search that gets its arithmetic wrong fails loudly
// instead of quietly producing a fault that fires outside the window the world
// says it does.
// ---------------------------------------------------------------------------

// ErrWiden is returned by narrowedTo when asked to widen.
var ErrWiden = fmt.Errorf("shrink: narrowing may not widen a fault window")

// narrowedTo returns f with its window replaced by [start, end], refusing any
// window that is not contained in f's own.
func narrowedTo(f schema.FaultSpec, start, end int64) (schema.FaultSpec, error) {
	if start < f.StartMS {
		return schema.FaultSpec{}, fmt.Errorf("%w: start %d is earlier than %d in %s",
			ErrWiden, start, f.StartMS, f.String())
	}
	if end > f.EndMS {
		return schema.FaultSpec{}, fmt.Errorf("%w: end %d is later than %d in %s",
			ErrWiden, end, f.EndMS, f.String())
	}
	if end < start {
		return schema.FaultSpec{}, fmt.Errorf("shrink: window %d..%d ends before it starts", start, end)
	}
	out := f
	out.Params = append([]schema.Param(nil), f.Params...)
	out.StartMS = start
	out.EndMS = end
	if err := out.Validate(); err != nil {
		return schema.FaultSpec{}, err
	}
	return out, nil
}

// narrowAll runs stage 3 over every surviving fault, in schedule order.
//
// It returns the narrowed schedule, how many worlds' worth of probes it issued,
// and the reason it stopped early if it did. On an early stop the schedule
// returned is the one reached so far: every entry of which has been CONFIRMED
// to reproduce, because a narrowing is only adopted when its probe was accepted.
func narrowAll(ctx context.Context, p *prober, faults []string, ops []int64) ([]string, StopReason) {
	cur := append([]string(nil), faults...)
	for i := range cur {
		narrowed, stop := narrowOne(ctx, p, cur, i, ops)
		cur[i] = narrowed
		if stop != StopComplete {
			return cur, stop
		}
	}
	return cur, StopComplete
}

// narrowOne narrows the window of cur[idx], leaving every other fault alone.
func narrowOne(ctx context.Context, p *prober, cur []string, idx int, ops []int64) (string, StopReason) {
	spec, err := schema.ParseFault(cur[idx])
	if err != nil {
		// An unparseable fault is not narrowable, and it is not this stage's job
		// to report it: the world file's own Validate already refuses it.
		return cur[idx], StopComplete
	}
	gran := p.pol.NarrowGranularityMS
	if spec.DurationMS() <= gran {
		return spec.String(), StopComplete
	}

	probe := func(f schema.FaultSpec) (bool, StopReason) {
		next := append([]string(nil), cur...)
		next[idx] = f.String()
		d, stop := p.test(ctx, Candidate{Faults: next, Ops: ops})
		if stop != StopComplete {
			return false, stop
		}
		return d.accepted(), StopComplete
	}

	// Phase A: pull the END in. hi is always a window known to reproduce (it
	// starts at the current end, which the surviving schedule reproduced with);
	// lo is the earliest end worth considering. The answer is hi, so the end can
	// only move earlier.
	lo, hi := spec.StartMS, spec.EndMS
	probes := 0
	for hi-lo > gran && probes < p.pol.NarrowMaxProbes {
		mid := lo + (hi-lo)/2
		cand, err := narrowedTo(spec, spec.StartMS, mid)
		if err != nil {
			break
		}
		ok, stop := probe(cand)
		if stop != StopComplete {
			return withWindow(spec, spec.StartMS, hi), stop
		}
		probes++
		if ok {
			hi = mid
		} else {
			lo = mid + 1
		}
	}
	end := hi

	// Phase B: push the START out. lo is always a window known to reproduce (it
	// starts at the current start); hi is the latest start worth considering.
	// The answer is lo, so the start can only move later.
	lo, hi = spec.StartMS, end
	probes = 0
	for hi-lo > gran && probes < p.pol.NarrowMaxProbes {
		mid := lo + (hi-lo+1)/2
		cand, err := narrowedTo(spec, mid, end)
		if err != nil {
			break
		}
		ok, stop := probe(cand)
		if stop != StopComplete {
			return withWindow(spec, lo, end), stop
		}
		probes++
		if ok {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return withWindow(spec, lo, end), StopComplete
}

// withWindow renders spec with the given window, falling back to the unmodified
// spec if the window would widen, which the searches above cannot produce, so
// the fallback is a belt-and-braces guard rather than a live path.
func withWindow(spec schema.FaultSpec, start, end int64) string {
	f, err := narrowedTo(spec, start, end)
	if err != nil {
		return spec.String()
	}
	return f.String()
}
