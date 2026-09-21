package replay

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/WrdCstlg/pro-thesis/internal/control"
	"github.com/WrdCstlg/pro-thesis/internal/corpus"
	"github.com/WrdCstlg/pro-thesis/internal/shrink"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// DefaultAttempts is the confirmation gate's k. The directive fixes it: "Shrunk
// world must reproduce k/k times (default 3/3)".
const DefaultAttempts = 3

// Signal is what ONE execution said about the violation being replayed.
type Signal string

const (
	// SignalReproduced: the same violation fired.
	SignalReproduced Signal = "reproduced"
	// SignalNotReproduced: the world ran to a verdict and that violation did
	// not fire. This is a FACT about the world, not an error.
	SignalNotReproduced Signal = "not_reproduced"
	// SignalDifferent: a violation fired, but not the one being replayed. It is
	// NOT a reproduction: a repro that reproduces something else is worse than
	// none, because an agent will fix the wrong thing.
	SignalDifferent Signal = "different_violation"
	// SignalUnknown: nobody could tell. The harness failed, an oracle could not
	// evaluate, the run was cancelled. Never folded into either of the others.
	SignalUnknown Signal = "unknown"
)

// Attempt is one execution of the world.
type Attempt struct {
	// N is the 1-based attempt number.
	N       int
	Signal  Signal
	Outcome control.Outcome
	// Reason explains a non-reproduction in the words a reader needs.
	Reason string
	// Found is every violation this attempt produced, in oracle order.
	Found []shrink.Identity
	// Divergent is the subset of Found that did NOT match the expected identity.
	Divergent []shrink.Identity
	Duration  time.Duration
	Paths     control.WorldPaths
	Project   string
	HostPorts map[string]int64
	KeptUp    bool
	Err       error
}

// Options configures a replay.
type Options struct {
	// World is the decoded `.thesis` file. Required.
	World *schema.World
	// WorldPath is where it came from, for reporting. It is never read.
	WorldPath string

	// Exec executes worlds. Required.
	Exec Executor

	// Expect is the violation this replay is trying to reproduce.
	//
	// The ZERO VALUE means "any violation counts", which is the right rule for
	// `thesis regress`: a committed regression world carries no recorded
	// violation, and a world that fails again is a finding whichever oracle
	// makes it. A non-zero Expect switches on the same-violation rule.
	Expect shrink.Identity
	// Strictness is how much of Expect a violation must reproduce. Ignored when
	// Expect is zero. Zero value is shrink.MatchOracle, so a caller that supplies
	// only an oracle name gets exactly the comparison it can support; callers
	// with a full identity should ask for shrink.MatchWitnessKey.
	Strictness shrink.Strictness

	// Attempts is k. Zero means DefaultAttempts.
	Attempts int

	// FirstOrdinal is the world ordinal the first attempt runs under. Zero
	// means 1. `thesis regress` walks it forward across the corpus so every
	// world in one run bundle keeps its own artifact directory.
	FirstOrdinal int

	// Budget bounds the whole replay in wall time. Zero means unbounded, which
	// is legitimate for a single interactive replay and is NOT what a gate
	// should pass (invariant I7).
	Budget time.Duration

	// KeepUp leaves the topology standing after the LAST attempt.
	KeepUp bool

	// Now is the clock, for tests. Nil means time.Now.
	Now func() time.Time

	// Trace, when non-nil, receives a running commentary: the schedule decision,
	// every attempt's signal, and every divergence.
	Trace io.Writer
}

func (o Options) attempts() int {
	if o.Attempts <= 0 {
		return DefaultAttempts
	}
	return o.Attempts
}

func (o Options) firstOrdinal() int {
	if o.FirstOrdinal <= 0 {
		return 1
	}
	return o.FirstOrdinal
}

func (o Options) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

// Report is a replay's full result.
type Report struct {
	WorldPath string
	WorldHash string
	ShortID   string
	Seed      uint64
	Schedule  ScheduleChoice

	Expect     shrink.Identity
	Strictness shrink.Strictness
	// MatchRule is a one-line statement of what "reproduced" meant here.
	MatchRule string

	// Planned is k: how many attempts were asked for.
	Planned int
	// Attempts are the executions that actually happened, in order.
	Attempts []Attempt
	// Confirmation is the k/n arithmetic, never rounded.
	Confirmation corpus.Confirmation

	// BudgetExpired reports that fewer attempts ran than were planned because
	// the wall budget ran out.
	BudgetExpired bool
	// Elapsed is the wall time the replay took.
	Elapsed time.Duration
}

// Reproduced reports whether any attempt reproduced the violation.
func (r Report) Reproduced() bool { return r.Confirmation.Reproduced > 0 }

// Divergences returns every distinct violation seen that was NOT the one being
// replayed, deduplicated and in first-seen order.
func (r Report) Divergences() []shrink.Identity {
	seen := map[string]bool{}
	var out []shrink.Identity
	for _, a := range r.Attempts {
		for _, id := range a.Divergent {
			k := id.Oracle + "\x00" + string(id.Class) + "\x00" + id.Key
			if seen[k] {
				continue
			}
			seen[k] = true
			out = append(out, id)
		}
	}
	return out
}

// ExitCode maps the report onto the normative CLI exit codes (directive 4.7).
//
// A REPRODUCTION IS EXIT 1. The world still fails, which is the actionable
// answer and the one `thesis regress` folds over.
//
// A replay that did not reproduce is exit 0, and that is honest rather than
// generous: the world ran, every oracle was satisfied, and `reproduced: "0/3"`
// is on the report for anyone who wants to know how confident that is.
//
// An unknown outranks a budget expiry because it is the stronger claim on the
// reader's attention: "an oracle could not check" is retry-once-then-escalate,
// while a short budget is a soft warning.
func (r Report) ExitCode() schema.ExitCode {
	switch {
	case r.Confirmation.Reproduced > 0:
		return schema.ExitFail
	case r.Confirmation.Inconclusive > 0:
		return schema.ExitInconclusive
	case r.BudgetExpired:
		return schema.ExitBudgetExhausted
	case r.Confirmation.Attempts == 0:
		// Nothing ran and nothing said why. Refusing to call that a pass is the
		// point: a replay that executed zero worlds has checked nothing.
		return schema.ExitInconclusive
	default:
		return schema.ExitPass
	}
}

// Run replays the world and reports what happened.
//
// The returned error is reserved for conditions that made the replay
// IMPOSSIBLE: no world, no executor, an unusable recorded schedule. A world
// that ran and did not reproduce is a Report, not an error.
func Run(ctx context.Context, opts Options) (Report, error) {
	if opts.World == nil {
		return Report{}, errors.New("replay: Options.World is required")
	}
	if opts.Exec == nil {
		return Report{}, errors.New("replay: Options.Exec is required")
	}

	choice, err := ChooseSchedule(opts.World)
	if err != nil {
		return Report{}, err
	}

	hash, err := opts.World.Hash()
	if err != nil {
		return Report{}, fmt.Errorf("replay: hash world: %w", err)
	}
	shortID, _ := schema.ShortIDFromHash(hash)

	rep := Report{
		WorldPath:  opts.WorldPath,
		WorldHash:  hash,
		ShortID:    shortID,
		Seed:       opts.World.Seed,
		Schedule:   choice,
		Expect:     opts.Expect,
		Strictness: opts.Strictness,
		MatchRule:  matchRule(opts.Expect, opts.Strictness),
		Planned:    opts.attempts(),
	}

	tracef(opts.Trace, "replay %s (world %s, seed %d)", opts.WorldPath, shortID, opts.World.Seed)
	tracef(opts.Trace, "schedule: %s", choice.Summary())
	for _, n := range choice.Notes {
		tracef(opts.Trace, "  note: %s", n)
	}
	for _, f := range choice.Faults {
		tracef(opts.Trace, "  fault: %s", f)
	}
	tracef(opts.Trace, "match rule: %s", rep.MatchRule)

	start := opts.now()
	deadline := time.Time{}
	if opts.Budget > 0 {
		deadline = start.Add(opts.Budget)
	}

	k := opts.attempts()
	ordinal := opts.firstOrdinal()
	for i := 1; i <= k; i++ {
		if ctx.Err() != nil {
			rep.BudgetExpired = false
			tracef(opts.Trace, "attempt %d/%d: cancelled before it started", i, k)
			break
		}
		// The budget is checked BEFORE starting an attempt, never during one. A
		// world torn off half way leaves a topology the next attempt collides
		// with, and its verdict would be about a run nobody finished.
		if !deadline.IsZero() && !opts.now().Before(deadline) {
			rep.BudgetExpired = true
			tracef(opts.Trace, "attempt %d/%d: not started, wall budget %s expired", i, k, opts.Budget)
			break
		}

		req := ExecRequest{
			Ordinal: ordinal,
			Seed:    opts.World.Seed,
			Faults:  choice.Faults,
			Label:   fmt.Sprintf("replay %s attempt %d/%d", shortID, i, k),
			KeepUp:  opts.KeepUp && i == k,
		}
		ordinal++

		exec := opts.Exec.Execute(ctx, req)
		att := judge(i, exec, opts.Expect, opts.Strictness)
		rep.Attempts = append(rep.Attempts, att)

		tracef(opts.Trace, "attempt %d/%d: %s (%s) in %s", i, k, att.Signal, att.Outcome, att.Duration.Round(time.Millisecond))
		if att.Reason != "" {
			tracef(opts.Trace, "  %s", att.Reason)
		}
		for _, d := range att.Divergent {
			tracef(opts.Trace, "  DIVERGENT violation: %s", d)
		}
	}

	rep.Elapsed = opts.now().Sub(start)
	rep.Confirmation = tally(rep.Attempts)
	return rep, nil
}

// tally reduces the attempts to the k/n the verdict publishes.
func tally(atts []Attempt) corpus.Confirmation {
	var c corpus.Confirmation
	for _, a := range atts {
		c.Attempts++
		switch a.Signal {
		case SignalReproduced:
			c.Reproduced++
		case SignalUnknown:
			c.Inconclusive++
		}
	}
	return c
}

// judge turns one execution into a signal.
//
// The order of the arms is the whole rule:
//
//  1. an execution that could not be conducted is UNKNOWN, whatever else it
//     reported. A harness error's oracle results are about a run that did not
//     happen;
//  2. a matching violation is a REPRODUCTION, even if other oracles also fired;
//  3. a violation that does not match is DIFFERENT; recorded, reported, and
//     never counted as a reproduction;
//  4. an inconclusive world with no violation is UNKNOWN, not a clean negative:
//     an oracle that could not check has not established that the bug is gone;
//  5. only a world that ran to a clean verdict is NOT REPRODUCED.
func judge(n int, exec Execution, expect shrink.Identity, strict shrink.Strictness) Attempt {
	att := Attempt{
		N:         n,
		Outcome:   exec.Outcome,
		Duration:  exec.Duration,
		Paths:     exec.Paths,
		Project:   exec.Project,
		HostPorts: exec.HostPorts,
		KeptUp:    exec.KeptUp,
		Err:       exec.Err,
	}

	for _, f := range exec.Findings {
		if f.Violated() {
			att.Found = append(att.Found, shrink.IdentityOfOutput(f.Output))
		}
	}

	switch exec.Outcome {
	case control.OutcomeHarnessError, control.OutcomeCanceled, control.OutcomeConfigError:
		att.Signal = SignalUnknown
		att.Reason = fmt.Sprintf("the execution could not be conducted (%s): %s",
			exec.Outcome, exec.Outcome.Describe())
		if exec.Err != nil {
			att.Reason += ": " + exec.Err.Error()
		}
		return att
	}

	var matched bool
	for i, f := range exec.Findings {
		if !f.Violated() {
			continue
		}
		id := shrink.IdentityOfOutput(f.Output)
		if expect.Zero() {
			// No expected identity: any violation is the violation. This is
			// `thesis regress`'s rule, and it is stated rather than implied.
			matched = true
			continue
		}
		ok, why := strict.Match(expect, id)
		if ok {
			matched = true
			continue
		}
		att.Divergent = append(att.Divergent, id)
		if att.Reason == "" {
			att.Reason = fmt.Sprintf("violation %d (%s) is not the one being replayed: %s",
				i+1, id, why)
		}
	}

	switch {
	case matched:
		att.Signal = SignalReproduced
		att.Reason = ""
		if len(att.Divergent) > 0 {
			att.Reason = fmt.Sprintf("reproduced, and %d other violation(s) also fired", len(att.Divergent))
		}
	case len(att.Divergent) > 0:
		att.Signal = SignalDifferent
	case exec.Outcome == control.OutcomeInconclusive:
		att.Signal = SignalUnknown
		att.Reason = "an oracle could not evaluate, so this execution establishes nothing either way"
	case len(exec.Findings) == 0:
		// No oracle produced a result at all. That is not a clean negative; it
		// is a world nobody checked, which is the vacuous pass this project
		// refuses to emit.
		att.Signal = SignalUnknown
		att.Reason = "no oracle produced a result, so nothing was checked"
	default:
		att.Signal = SignalNotReproduced
		att.Reason = "the world ran and every oracle was satisfied"
	}
	return att
}

// matchRule states, in one line, what counted as a reproduction.
func matchRule(expect shrink.Identity, strict shrink.Strictness) string {
	if expect.Zero() {
		return "any oracle violation counts as a reproduction (no expected violation was supplied)"
	}
	return fmt.Sprintf("a violation matching %s on %s", expect, strict)
}

func tracef(w io.Writer, format string, args ...any) {
	if w == nil {
		return
	}
	line := fmt.Sprintf(format, args...)
	if !strings.HasSuffix(line, "\n") {
		line += "\n"
	}
	_, _ = io.WriteString(w, line)
}
