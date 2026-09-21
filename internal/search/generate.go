package search

import (
	"errors"
	"fmt"

	"github.com/WrdCstlg/pro-thesis/internal/recorder"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// ---------------------------------------------------------------------------
// Candidate generation
//
// One generator, used by the random baseline and by the add-fault and
// force-overlap mutation operators. Sharing it is what makes the two arms of
// D-029's pre-registered benchmark draw from the same distribution over the
// same space.
//
// Every product goes through schema.ParseFault, so a generated fault is
// canonical by construction; nothing here hand-builds a FaultSpec.
// ---------------------------------------------------------------------------

// ErrNoValidCandidate reports that generation could not produce a schedule the
// compiler accepts within its attempt budget.
//
// It is an error rather than a best-effort emission on purpose. "Never emit a
// world the compiler will reject mid-search" is only true if the failure to
// build one is loud: a silently truncated schedule would run a world with fewer
// faults than the search believes it injected, and the search would then
// attribute that world's result to a schedule that never existed.
var ErrNoValidCandidate = errors.New("search: could not generate a schedule the perturber accepts")

// GenerateAttempts bounds candidate generation before ErrNoValidCandidate.
//
// Generous, because every rejected candidate costs a few microseconds while an
// unnecessary failure costs a world. It is bounded at all because a
// misconfigured space (every target refused by a constraint) must terminate
// with a diagnosis rather than spin.
const GenerateAttempts = 64

// RandomFault draws one uniformly random fault from the space.
//
// Uniform over (kind, target, magnitude rung) and over a quantised window. It
// is deliberately flat: this is the honest baseline the Saboteur is measured
// against, so it gets no ladder prior, no overlap bias and no telemetry.
func RandomFault(sp *Space, st *recorder.Stream) (schema.FaultSpec, error) {
	if sp == nil {
		return schema.FaultSpec{}, errors.New("search: RandomFault needs a space")
	}
	// Draw a kind that actually has legal targets. Kinds are drawn uniformly
	// among those that do, rather than redrawing on failure, so the number of
	// values consumed does not depend on which kinds happen to be unusable.
	usable := make([]schema.FaultKind, 0, len(sp.Kinds))
	for _, k := range sp.Kinds {
		if len(sp.Targets[k]) > 0 {
			usable = append(usable, k)
		}
	}
	if len(usable) == 0 {
		return schema.FaultSpec{}, ErrEmptySpace
	}
	kind := pick(st, usable)
	target := pick(st, sp.Targets[kind])
	params := pick(st, sp.Rungs(kind))
	start, end := randomWindow(sp.Window, kind.Durative(), st)
	return BuildFault(kind, target, params, start, end)
}

// randomWindow draws a window inside the policy.
//
// An instantaneous kind (proc.kill, proc.restart, clock.jump) still gets a
// non-degenerate window, because the frozen registry defines [start, end] for
// such a kind as the period in which the resulting disturbance is EXPECTED:
// no_crash uses it to excuse the exit the fault caused. A zero-width window
// would make the perturber's own kill look like a crash the tool discovered.
func randomWindow(p WindowPolicy, durative bool, st *recorder.Stream) (int64, int64) {
	span := p.LatestMS - p.EarliestMS
	if span < p.MinDurationMS {
		span = p.MinDurationMS
	}
	maxDur := p.MaxDurationMS
	if maxDur > span {
		maxDur = span
	}
	if maxDur < p.MinDurationMS {
		maxDur = p.MinDurationMS
	}
	durRange := maxDur - p.MinDurationMS + 1
	dur := p.MinDurationMS + int64(st.Uint64n(uint64(durRange)))
	if !durative {
		// Keep an instantaneous kind's expectation window short but never zero.
		if dur > p.MinDurationMS*4 && p.MinDurationMS > 0 {
			dur = p.MinDurationMS * 4
		}
	}
	dur = p.quantize(dur)
	if dur < p.MinDurationMS {
		dur = p.MinDurationMS
	}

	latestStart := p.LatestMS - dur
	if latestStart < p.EarliestMS {
		latestStart = p.EarliestMS
	}
	startRange := latestStart - p.EarliestMS + 1
	start := p.EarliestMS + int64(st.Uint64n(uint64(startRange)))
	start = p.quantize(start)
	if start < p.EarliestMS {
		start = p.EarliestMS
	}
	end := start + dur
	if end > p.LatestMS {
		end = p.LatestMS
	}
	if end < start {
		end = start
	}
	return start, end
}

// RandomSchedule draws a whole schedule and validates it.
//
// It draws a fault count uniformly from [1, Space.MaxFaults]: the SAME ceiling
// every strategy uses (see Space.MaxFaults), because an arm with a larger fault
// budget is a different experiment, not a better strategy.
//
// On a rejected candidate it redraws the whole schedule rather than patching
// it. Patching would bias the distribution toward whatever the constraint
// happened to permit, and the baseline's value is that it is unbiased.
func RandomSchedule(sp *Space, v *Validator, st *recorder.Stream) ([]string, error) {
	if sp == nil {
		return nil, errors.New("search: RandomSchedule needs a space")
	}
	maxFaults := sp.MaxFaults
	if maxFaults < 1 {
		maxFaults = 1
	}
	var lastErr error
	for attempt := 0; attempt < GenerateAttempts; attempt++ {
		n := 1 + int(st.Uint64n(uint64(maxFaults)))
		planned := make([]string, 0, n)
		ok := true
		for i := 0; i < n; i++ {
			f, err := RandomFault(sp, st)
			if err != nil {
				lastErr = err
				ok = false
				break
			}
			planned = append(planned, f.String())
		}
		if !ok {
			continue
		}
		planned = schema.SortFaultStrings(planned)
		if v == nil {
			return planned, nil
		}
		if err := v.Validate(planned); err != nil {
			lastErr = err
			continue
		}
		return planned, nil
	}
	if lastErr == nil {
		lastErr = errors.New("no candidate was produced")
	}
	return nil, fmt.Errorf("%w after %d attempts: %v", ErrNoValidCandidate, GenerateAttempts, lastErr)
}
