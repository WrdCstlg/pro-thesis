package replay

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/WrdCstlg/pro-thesis/internal/corpus"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// ---------------------------------------------------------------------------
// thesis regress; replay the whole committed corpus
//
// This runs on every gate invocation forever, so three properties matter more
// here than anywhere else in the phase:
//
//	DETERMINISTIC ORDER   corpus.List sorts by content-addressed filename, so a
//	                      short budget always spends itself on the same prefix
//	                      and two runs are comparable.
//	BUDGETED (I7)         a world costs ~30s. What was NOT REACHED is reported
//	                      by name; silently running a subset and reporting PASS
//	                      would be the vacuous pass this project is ranked
//	                      against.
//	AN EMPTY CORPUS SAYS SO. "0 regressions passed" and "there are no
//	                      regressions" are different facts and only one of them
//	                      is evidence.
// ---------------------------------------------------------------------------

// Status is one corpus world's answer.
type Status string

const (
	// StatusPass: the world replayed and did NOT reproduce. The bug it records
	// is not present in this build.
	StatusPass Status = "PASS"
	// StatusFail: the world reproduced. The regression is back.
	StatusFail Status = "FAIL"
	// StatusInconclusive: the world could not be judged; it did not decode, the
	// harness failed, an oracle could not evaluate.
	StatusInconclusive Status = "INCONCLUSIVE"
	// StatusNotReached: the budget expired before this world was started. It is
	// reported, never omitted.
	StatusNotReached Status = "NOT REACHED"
)

// WorldResult is one corpus entry's outcome.
type WorldResult struct {
	Entry  corpus.Entry
	Status Status
	// Report is the replay's own report. Zero for a NOT REACHED world and for
	// one that never decoded.
	Report Report
	// Err is set when the world could not be replayed at all.
	Err error
}

// RegressOptions configures a corpus run.
type RegressOptions struct {
	// Corpus is the store to replay. Required.
	Corpus *corpus.Corpus

	// Exec executes every world. Required unless ExecFor is set.
	Exec Executor

	// ExecFor returns the executor for ONE world, and takes precedence over
	// Exec.
	//
	// It exists because a `.thesis` file names its DRIVER profile while
	// control.Runner is constructed against a RUN profile, and D-045 records what
	// happens when those two namespaces are conflated. Two corpus worlds driven
	// by different profiles therefore need two runners, and a world whose driver
	// profile no longer resolves in prothesis.yaml is INCONCLUSIVE (reported by
	// name) rather than silently skipped or run under somebody else's workload.
	ExecFor func(*schema.World) (Executor, error)

	// Attempts is how many times each world is replayed. ONE by default, not
	// three: `regress` asks "is this bug back", and a single reproduction
	// answers it. The confirmation gate's k belongs to the COMMIT decision, not
	// to the gate that reads the result.
	Attempts int

	// Budget bounds the whole run in wall time. Zero means unbounded, which a
	// gate should never pass.
	Budget time.Duration

	// Now is the clock, for tests. Nil means time.Now.
	Now func() time.Time

	// Trace, when non-nil, receives per-world commentary.
	Trace io.Writer
}

func (o RegressOptions) attempts() int {
	if o.Attempts <= 0 {
		return 1
	}
	return o.Attempts
}

func (o RegressOptions) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

// RegressReport is the whole corpus run.
type RegressReport struct {
	// CorpusDir is the directory that was read.
	CorpusDir string
	// CorpusExists reports whether the directory is even present. A project that
	// has never shrunk anything has no corpus, and that is not a failure.
	CorpusExists bool
	// Empty reports that the corpus holds no worlds. It is DISTINCT from
	// len(Worlds)==0 after a budget expiry, and the CLI must say so in words.
	Empty bool

	// Worlds is every entry, in corpus order, including the ones not reached.
	Worlds []WorldResult

	Passed       int
	Failed       int
	Inconclusive int
	NotReached   int

	BudgetExpired bool
	Elapsed       time.Duration
}

// ExitCode maps the corpus run onto the normative exit codes.
//
//	any reproduction        -> 1  the regression is back; this is the whole point
//	any inconclusive        -> 2  something could not be judged; retry or escalate
//	anything not reached    -> 3  the budget ran out with corpus left; soft warning
//	otherwise               -> 0  including an EMPTY corpus, which the report says
//	                              in words rather than implying with a zero
func (r RegressReport) ExitCode() schema.ExitCode {
	switch {
	case r.Failed > 0:
		return schema.ExitFail
	case r.Inconclusive > 0:
		return schema.ExitInconclusive
	case r.NotReached > 0:
		return schema.ExitBudgetExhausted
	default:
		return schema.ExitPass
	}
}

// Summary renders the one-line answer, distinguishing an empty corpus from a
// clean one.
func (r RegressReport) Summary() string {
	if r.Empty {
		if !r.CorpusExists {
			return fmt.Sprintf("there are no regressions: %s does not exist", r.CorpusDir)
		}
		return fmt.Sprintf("there are no regressions: %s holds no worlds", r.CorpusDir)
	}
	s := fmt.Sprintf("%d regression(s): %d passed, %d failed, %d inconclusive",
		len(r.Worlds), r.Passed, r.Failed, r.Inconclusive)
	if r.NotReached > 0 {
		s += fmt.Sprintf(", %d NOT REACHED (budget expired)", r.NotReached)
	}
	return s
}

// Regress replays every world in the corpus.
func Regress(ctx context.Context, opts RegressOptions) (RegressReport, error) {
	if opts.Corpus == nil {
		return RegressReport{}, errors.New("replay: RegressOptions.Corpus is required")
	}
	if opts.Exec == nil && opts.ExecFor == nil {
		return RegressReport{}, errors.New("replay: RegressOptions needs Exec or ExecFor")
	}

	rep := RegressReport{
		CorpusDir:    opts.Corpus.Dir(),
		CorpusExists: opts.Corpus.Exists(),
	}

	entries, err := opts.Corpus.List()
	if err != nil {
		return rep, err
	}
	if len(entries) == 0 {
		rep.Empty = true
		tracef(opts.Trace, "%s", rep.Summary())
		return rep, nil
	}

	start := opts.now()
	deadline := time.Time{}
	if opts.Budget > 0 {
		deadline = start.Add(opts.Budget)
	}

	// Ordinals walk forward across the whole corpus so every world's artifacts
	// land in their own directory inside one run bundle.
	ordinal := 1

	for _, e := range entries {
		// The budget is checked before a world STARTS. A world stopped mid-flight
		// leaves a topology behind and has no verdict to give.
		if !deadline.IsZero() && !opts.now().Before(deadline) {
			rep.BudgetExpired = true
			rep.Worlds = append(rep.Worlds, WorldResult{Entry: e, Status: StatusNotReached})
			rep.NotReached++
			tracef(opts.Trace, "%-16s NOT REACHED (wall budget %s expired)", e.Name, opts.Budget)
			continue
		}
		if ctx.Err() != nil {
			rep.Worlds = append(rep.Worlds, WorldResult{
				Entry:  e,
				Status: StatusNotReached,
				Err:    ctx.Err(),
			})
			rep.NotReached++
			tracef(opts.Trace, "%-16s NOT REACHED (cancelled)", e.Name)
			continue
		}

		if !e.OK() {
			// A corpus file that does not decode is INCONCLUSIVE, never skipped.
			// It is a committed artifact that no longer loads, which is a corpus
			// integrity failure and exactly the kind of thing a gate exists to
			// surface. Hiding it would let a corrupt regression masquerade as a
			// clean one.
			rep.Worlds = append(rep.Worlds, WorldResult{
				Entry:  e,
				Status: StatusInconclusive,
				Err:    e.Err,
			})
			rep.Inconclusive++
			tracef(opts.Trace, "%-16s INCONCLUSIVE: %v", e.Name, e.Err)
			continue
		}

		exec := opts.Exec
		if opts.ExecFor != nil {
			ex, xerr := opts.ExecFor(e.World)
			if xerr != nil {
				rep.Worlds = append(rep.Worlds, WorldResult{
					Entry:  e,
					Status: StatusInconclusive,
					Err:    xerr,
				})
				rep.Inconclusive++
				tracef(opts.Trace, "%-16s INCONCLUSIVE: %v", e.Name, xerr)
				continue
			}
			exec = ex
		}

		remaining := time.Duration(0)
		if !deadline.IsZero() {
			remaining = deadline.Sub(opts.now())
		}

		r, rerr := Run(ctx, Options{
			World:        e.World,
			WorldPath:    e.Path,
			Exec:         exec,
			Attempts:     opts.attempts(),
			FirstOrdinal: ordinal,
			Budget:       remaining,
			Now:          opts.Now,
			Trace:        opts.Trace,
			// Expect is deliberately ZERO. A committed regression world carries
			// no recorded violation identity (the `.thesis` format has no field
			// for one and inventing one would be normative surface) so the rule
			// is "any violation means the regression is back". That is the right
			// rule for a gate: a corpus world that fails again is a finding
			// whichever oracle makes it.
		})
		ordinal += opts.attempts()

		wr := WorldResult{Entry: e, Report: r, Err: rerr}
		switch {
		case rerr != nil:
			wr.Status = StatusInconclusive
			rep.Inconclusive++
		case r.Confirmation.Reproduced > 0:
			wr.Status = StatusFail
			rep.Failed++
		case r.Confirmation.Inconclusive > 0 || r.Confirmation.Attempts == 0:
			wr.Status = StatusInconclusive
			rep.Inconclusive++
		default:
			wr.Status = StatusPass
			rep.Passed++
		}
		if r.BudgetExpired {
			rep.BudgetExpired = true
		}
		rep.Worlds = append(rep.Worlds, wr)
		tracef(opts.Trace, "%-16s %-12s %s", e.Name, wr.Status, r.Confirmation)
	}

	rep.Elapsed = opts.now().Sub(start)
	return rep, nil
}
