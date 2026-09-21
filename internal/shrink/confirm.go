package shrink

import (
	"context"
	"fmt"

	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// ---------------------------------------------------------------------------
// Stage 4; the confirmation gate
//
// "Confirmation gate: Shrunk world must reproduce k/k times (default 3/3)."
//
// And the constraint the specification itself puts on reporting it:
//
//	"Replays reproduce with high probability, not certainty. The verdict must
//	 report reproduced: 3/3 or 1/5 and never claim determinism it does not have."
//
// So the gate and the REPORT are two different things. The gate is k/k. The
// report is the measured k/n, whatever it turned out to be, and a 2/3 is
// published as "2/3" with Passed() false rather than rounded up to a pass or
// hidden by re-running until it looks better.
// ---------------------------------------------------------------------------

// Confirmation is the measured result of the gate.
type Confirmation struct {
	// K is the gate: how many of how many were required.
	K int
	// Trials is how many confirmation replays actually ran. It can be short of K
	// when the budget expired, and the shortfall is reported rather than hidden.
	Trials int
	// Reproduced is how many of those reproduced the ORIGINAL violation
	// identity, at the configured strictness.
	Reproduced int
	// Different counts replays that failed with a DIFFERENT violation. A shrunk
	// world that mostly reproduces something else is not a minimal repro of this
	// bug, however good its k/n looks.
	Different int
	// Unknown counts replays that could not be judged at all.
	Unknown int
	// World is the .thesis path the confirmation replays executed, as the
	// executor named it. It is what minimal_repro.world points at, so it comes
	// from the world that was actually measured rather than from a path
	// somebody assembled.
	World string
	// Stop says why confirmation stopped short, if it did.
	Stop StopReason
}

// Ran reports whether any confirmation replay executed.
func (c Confirmation) Ran() bool { return c.Trials > 0 }

// Passed reports whether the gate was met: k out of k, with k actually reached.
func (c Confirmation) Passed() bool {
	return c.K > 0 && c.Trials >= c.K && c.Reproduced == c.Trials
}

// String is the verdict's `reproduced` value: the MEASURED k/n.
//
// It is never synthesised. A confirmation that never ran has no honest string
// ("0/0" would be a claim about determinism from zero evidence, which the verdict
// schema calls out by name) so callers must check Ran() first, and
// Result.MinimalRepro does.
func (c Confirmation) String() string { return fmt.Sprintf("%d/%d", c.Reproduced, c.Trials) }

// Describe explains the measurement in one line.
func (c Confirmation) Describe() string {
	if !c.Ran() {
		return "confirmation did not run"
	}
	s := fmt.Sprintf("reproduced %s (gate %d/%d)", c.String(), c.K, c.K)
	if c.Different > 0 {
		s += fmt.Sprintf(", %d replay(s) failed with a DIFFERENT violation", c.Different)
	}
	if c.Unknown > 0 {
		s += fmt.Sprintf(", %d replay(s) could not be judged", c.Unknown)
	}
	if c.Stop != StopComplete {
		s += "; " + c.Stop.Describe()
	}
	return s
}

// confirm runs the gate: exactly k independent executions of the final
// candidate, with NO early exit in either direction.
//
// No early exit is the difference between this and the prober. During ddmin an
// early accept is correct because one reproduction is a proof and the question
// is binary. Here the question is a RATE, and a run that stopped as soon as it
// had enough successes would report a number biased upward by construction,
// which is exactly the "never claim determinism it does not have" failure.
func confirm(ctx context.Context, p *prober, c Candidate, k int) Confirmation {
	out := Confirmation{K: k}
	if k <= 0 {
		return out
	}
	for i := 0; i < k; i++ {
		attempts, reason := p.execute(ctx, []Candidate{c})
		if len(attempts) == 0 {
			out.Stop = reason
			if out.Stop == StopComplete {
				out.Stop = StopWorldBudget
			}
			return out
		}
		sig, ids, why := attempts[0].Classify(p.orig, p.pol.Strictness)
		out.Trials++
		if attempts[0].WorldPath != "" {
			out.World = attempts[0].WorldPath
		}
		switch sig {
		case SignalReproduced:
			out.Reproduced++
		case SignalDifferent:
			out.Different++
			p.noteDivergence(ids, p.isUnreduced(c))
			p.logf("confirmation replay %d: %s", i+1, why)
		case SignalUnknown:
			out.Unknown++
			p.logf("confirmation replay %d: %s", i+1, why)
		}
		if reason != StopComplete {
			out.Stop = reason
			return out
		}
		if ctx.Err() != nil {
			out.Stop = StopCanceled
			return out
		}
	}
	return out
}

// minimalRepro builds the verdict's `minimal_repro` object, or nil.
//
// nil at zero reproductions is failure mode #3 closed at the last possible
// moment: a .thesis that replays 0/3 committed to .prothesis/regressions/ is a
// permanent red on every future gate run that nobody can fix, and D-004 makes
// removing it a lock change. A world that reproduced at least once is a genuine
// repro and is named, with its honest rate; whether it may be COMMITTED is a
// separate question, answered by Confirmation.Passed().
func minimalRepro(worldPath string, c Confirmation) *schema.MinimalRepro {
	if worldPath == "" || !c.Ran() || c.Reproduced == 0 {
		return nil
	}
	return &schema.MinimalRepro{
		World:      worldPath,
		Cmd:        ReplayCmd(worldPath),
		Reproduced: c.String(),
	}
}

// ReplayCmd is the command the verdict's minimal_repro.cmd carries, matching
// directive 4.6's example spelling.
func ReplayCmd(worldPath string) string { return "thesis replay " + worldPath }
