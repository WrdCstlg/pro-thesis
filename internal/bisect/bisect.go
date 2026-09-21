// Package bisect finds the first revision at which a recorded world reproduces.
//
// `thesis bisect WORLD --good SHA --bad SHA` checks out a revision, rebuilds
// what the target needs, replays the world, and narrows. Four properties are
// load-bearing and each closes a specific way this command becomes worse than
// useless.
//
// # THE WORKING TREE IS RESTORED ON EVERY EXIT PATH
//
// Including a failed build, a failed checkout, a cancelled context and a panic.
// Leaving a developer on a detached HEAD in the middle of their own repository
// is genuinely hostile (worse than the command simply not existing) and it is
// the failure that happens exactly when nobody is watching, because it happens
// on the error path. Restoration is a deferred close over the HEAD captured
// before the first checkout, it runs with a DETACHED context so a Ctrl-C that
// cancelled the search cannot also cancel the repair, and whether it succeeded
// is reported in the result rather than assumed.
//
// # A REBUILD IS EXPLICIT, BECAUSE A CONTAINERISED TARGET NEEDS ONE
//
// The compose backend brings a project up from whatever image is in the local
// store; `docker compose up` does not rebuild. So a bisect that only checks out
// source measures ONE image against N revisions of the source tree and returns a
// confident, wrong answer. There is no config field for a build command
// (prothesis.yaml has none), so Options.Build is required unless the caller
// states NoBuild deliberately. Refusing is the honest default: a silently wrong
// bisect is the expensive failure here.
//
// # AN INCONCLUSIVE REVISION IS SKIPPED, NOT GUESSED
//
// A revision whose world could not be judged is neither good nor bad (git
// bisect calls this `skip`) and calling it either would move the answer by an
// arbitrary amount. Skipped revisions are stepped over within the live range,
// and a range that is entirely skipped is reported as UNDETERMINED with the
// surviving range named, never as a first-bad commit.
//
// # IT IS BUDGETED (I7, D-029)
//
// Every step costs a build plus a world. The search stops where it is and
// reports the range it had narrowed to.
package bisect

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// Verdict is one revision's answer.
type Verdict string

const (
	// Good: the world did not reproduce here.
	Good Verdict = "good"
	// Bad: the world reproduced here.
	Bad Verdict = "bad"
	// Skip: the revision could not be judged; the build failed, the harness
	// failed, an oracle could not evaluate. Neither good nor bad.
	Skip Verdict = "skip"
)

// Step is one tested revision.
type Step struct {
	// Index is the revision's position in the oldest-first candidate list.
	Index int
	// Rev is the full object id; Short and Subject are for display.
	Rev     string
	Short   string
	Subject string

	Verdict Verdict
	// Reason explains a Skip, and the evidence behind a Good or Bad.
	Reason   string
	Duration time.Duration
}

// Options configures a bisect.
type Options struct {
	// Repo is the working tree. Required.
	Repo *Repo

	// Good and Bad are the endpoint revisions, as the user wrote them.
	Good string
	Bad  string

	// Build rebuilds the system under test at the checked-out revision.
	//
	// Nil is only legal with NoBuild. See the package doc: for a containerised
	// target a bisect without a rebuild measures one image against every
	// revision.
	Build func(ctx context.Context, rev string) error
	// NoBuild states deliberately that no rebuild is needed: an interpreted
	// target, or a harness that builds on its own.
	NoBuild bool
	// BuildDescription is what the report says was run at each revision.
	BuildDescription string

	// Test replays the world at the checked-out, rebuilt revision.
	//
	// It returns a Verdict and a reason. An error is treated as Skip with the
	// error as the reason: a test that could not run says nothing about the
	// revision, and turning it into a Good or a Bad would move the answer.
	Test func(ctx context.Context, rev string) (Verdict, string, error)

	// VerifyEndpoints re-measures --good and --bad before searching.
	//
	// Default ON. The user's assertion about the endpoints is the premise of the
	// entire answer, and two extra executions is a small price for not returning
	// a confident first-bad commit derived from a false premise. Set
	// SkipEndpointCheck to opt out.
	SkipEndpointCheck bool

	// AllowDirty permits a bisect over a working tree with uncommitted changes
	// to tracked files. Off by default: checkout would either refuse or carry
	// them across revisions, and neither is what the user meant.
	AllowDirty bool

	// Budget bounds the whole search in wall time. Zero means unbounded.
	Budget time.Duration
	// Now is the clock, for tests. Nil means time.Now.
	Now func() time.Time

	// Trace, when non-nil, receives a running commentary.
	Trace io.Writer
}

func (o Options) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

// Report is the search's result.
type Report struct {
	Good      string
	Bad       string
	GoodShort string
	BadShort  string

	// Candidates is the oldest-first list of revisions in (good, bad].
	Candidates []string
	// Steps are the revisions actually tested, in the order tested.
	Steps []Step
	// Skipped names every revision that could not be judged.
	Skipped []string

	// FirstBad is the answer, or "" when the search could not determine one.
	FirstBad        string
	FirstBadShort   string
	FirstBadSubject string
	// Determined reports whether FirstBad is an answer rather than an absence.
	Determined bool
	// RangeLo and RangeHi bound the surviving range when the search could not
	// determine an answer, so the report says WHERE the first bad commit is even
	// when it cannot say WHICH.
	RangeLo string
	RangeHi string

	// BudgetExpired reports that the search stopped on its wall budget.
	BudgetExpired bool
	// Undetermined explains why FirstBad is empty.
	Undetermined string

	// Restored is where HEAD was put back to.
	Restored string
	// RestoreErr is non-nil when the working tree could NOT be restored. It is
	// the loudest thing in this report for a reason.
	RestoreErr error

	Elapsed time.Duration
}

// ExitCode maps the report onto the normative exit codes.
//
//	0  the first bad revision was found and the tree was restored
//	2  the search could not determine an answer, or the tree could not be
//	   restored — both are "retry once, then escalate to a human"
//	3  the wall budget expired before the range collapsed
func (r Report) ExitCode() schema.ExitCode {
	switch {
	case r.RestoreErr != nil:
		return schema.ExitInconclusive
	case r.Determined:
		return schema.ExitPass
	case r.BudgetExpired:
		return schema.ExitBudgetExhausted
	default:
		return schema.ExitInconclusive
	}
}

// Run performs the bisect.
//
// The returned error is reserved for conditions that stopped the search from
// starting at all. Everything that happens once HEAD has moved is reported in
// the Report, including a failure to move it back, which is why Report is
// returned even alongside an error.
func Run(ctx context.Context, opts Options) (rep Report, err error) {
	if opts.Repo == nil {
		return Report{}, errors.New("bisect: Options.Repo is required")
	}
	if opts.Test == nil {
		return Report{}, errors.New("bisect: Options.Test is required")
	}
	if opts.Build == nil && !opts.NoBuild {
		return Report{}, errors.New("bisect: a rebuild step is required. " +
			"The compose backend boots whatever image is in the local store, so a bisect that only " +
			"checks out source measures ONE build against every revision and returns a confident, " +
			"wrong answer. Supply Options.Build, or state Options.NoBuild deliberately")
	}
	repo := opts.Repo

	if !repo.IsRepo(ctx) {
		return Report{}, fmt.Errorf("bisect: %s is not a git working tree", repo.Dir)
	}
	if !opts.AllowDirty {
		dirty, detail, derr := repo.Dirty(ctx)
		if derr != nil {
			return Report{}, derr
		}
		if dirty {
			return Report{}, fmt.Errorf("bisect: %s has uncommitted changes to tracked files, and "+
				"bisecting checks out other revisions over them:\n%s\ncommit or stash first, or pass "+
				"--allow-dirty if you are certain", repo.Dir, detail)
		}
	}

	good, gerr := repo.Resolve(ctx, opts.Good)
	if gerr != nil {
		return Report{}, gerr
	}
	bad, berr := repo.Resolve(ctx, opts.Bad)
	if berr != nil {
		return Report{}, berr
	}
	if good == bad {
		return Report{}, fmt.Errorf("bisect: --good and --bad both resolve to %s; "+
			"there is nothing between them to search", repo.Short(ctx, good))
	}
	if anc, aerr := repo.IsAncestor(ctx, good, bad); aerr == nil && !anc {
		return Report{}, fmt.Errorf("bisect: %s (--good) is not an ancestor of %s (--bad); "+
			"there is no linear history between them to bisect",
			repo.Short(ctx, good), repo.Short(ctx, bad))
	}

	candidates, cerr := repo.RevList(ctx, good, bad)
	if cerr != nil {
		return Report{}, cerr
	}
	if len(candidates) == 0 {
		return Report{}, fmt.Errorf("bisect: no revisions between %s and %s",
			repo.Short(ctx, good), repo.Short(ctx, bad))
	}

	rep = Report{
		Good:       good,
		Bad:        bad,
		GoodShort:  repo.Short(ctx, good),
		BadShort:   repo.Short(ctx, bad),
		Candidates: candidates,
	}

	// Capture HEAD BEFORE the first checkout, and arrange the repair before the
	// first thing that could fail. Everything from here on runs with the tree
	// restorable.
	head, herr := repo.Head(ctx)
	if herr != nil {
		return rep, fmt.Errorf("bisect: cannot read HEAD, so the working tree could not be "+
			"restored afterwards; refusing to move it: %w", herr)
	}
	tracef(opts.Trace, "bisect: HEAD is %s; it will be restored on every exit path", head)

	defer func() {
		// DETACHED context, deliberately. A Ctrl-C that cancelled the search must
		// not also cancel putting the user's repository back.
		rctx, cancel := context.WithTimeout(context.Background(), 2*gitTimeout)
		defer cancel()
		if rerr := repo.Restore(rctx, head); rerr != nil {
			rep.RestoreErr = fmt.Errorf("bisect: COULD NOT RESTORE the working tree to %s: %w. "+
				"Run `git checkout %s` to recover", head, rerr, restoreTarget(head))
			tracef(opts.Trace, "%v", rep.RestoreErr)
			return
		}
		rep.Restored = head.String()
		tracef(opts.Trace, "bisect: working tree restored to %s", head)
	}()

	start := opts.now()
	deadline := time.Time{}
	if opts.Budget > 0 {
		deadline = start.Add(opts.Budget)
	}
	defer func() { rep.Elapsed = opts.now().Sub(start) }()

	// Test a revision, with the budget and cancellation checked first.
	test := func(idx int) (Verdict, bool) {
		if ctx.Err() != nil {
			rep.Undetermined = "the search was cancelled"
			return Skip, false
		}
		if !deadline.IsZero() && !opts.now().Before(deadline) {
			rep.BudgetExpired = true
			rep.Undetermined = fmt.Sprintf("the wall budget %s expired", opts.Budget)
			return Skip, false
		}
		st := runStep(ctx, opts, repo, idx, candidates[idx])
		rep.Steps = append(rep.Steps, st)
		if st.Verdict == Skip {
			rep.Skipped = append(rep.Skipped, st.Short)
		}
		tracef(opts.Trace, "bisect: %s %-9s %s", st.Short, st.Verdict, st.Reason)
		return st.Verdict, true
	}

	// Endpoint verification. The user's assertion is the premise of the whole
	// answer; a false premise produces a confident, wrong first-bad commit.
	if !opts.SkipEndpointCheck {
		v, cont := testRev(ctx, opts, repo, -1, good, &rep)
		if !cont {
			return rep, nil
		}
		if v != Good {
			rep.Undetermined = fmt.Sprintf(
				"--good %s did not measure as good (it measured %s), so there is nothing to bisect "+
					"between: the premise of the search is false", rep.GoodShort, v)
			tracef(opts.Trace, "bisect: %s", rep.Undetermined)
			return rep, nil
		}
		v, cont = testRev(ctx, opts, repo, len(candidates)-1, bad, &rep)
		if !cont {
			return rep, nil
		}
		if v != Bad {
			rep.Undetermined = fmt.Sprintf(
				"--bad %s did not measure as bad (it measured %s). Under Tier B a single "+
					"non-reproduction is weak evidence, so this may be flakiness rather than a wrong "+
					"endpoint — but bisecting from it would narrow toward an answer nothing supports",
				rep.BadShort, v)
			tracef(opts.Trace, "bisect: %s", rep.Undetermined)
			return rep, nil
		}
	}

	// Binary search for the smallest index that is bad. good is known good and
	// sits before candidates[0]; candidates[len-1] is bad.
	lo, hi := 0, len(candidates)-1
	for lo < hi {
		mid := lo + (hi-lo)/2

		v, idx, ok := probe(lo, hi, mid, test)
		if !ok {
			// Every revision in the live range was skipped, or the search ran out
			// of budget. Either way the answer is a RANGE, not a commit.
			rep.RangeLo = repo.Short(ctx, candidates[lo])
			rep.RangeHi = repo.Short(ctx, candidates[hi])
			if rep.Undetermined == "" {
				rep.Undetermined = fmt.Sprintf(
					"every revision in the surviving range %s..%s was skipped, so the first bad "+
						"revision cannot be narrowed further", rep.RangeLo, rep.RangeHi)
			}
			tracef(opts.Trace, "bisect: %s", rep.Undetermined)
			return rep, nil
		}
		if v == Bad {
			hi = idx
		} else {
			lo = idx + 1
		}
	}

	rep.FirstBad = candidates[lo]
	rep.FirstBadShort = repo.Short(ctx, rep.FirstBad)
	rep.FirstBadSubject = repo.Subject(ctx, rep.FirstBad)
	rep.Determined = true
	tracef(opts.Trace, "bisect: first bad revision is %s %s", rep.FirstBadShort, rep.FirstBadSubject)
	return rep, nil
}

// probe tests mid and, when it is skipped, walks outward within [lo, hi] until
// a revision answers or the range is exhausted.
//
// Walking outward rather than giving up is what makes a skip cost one revision
// instead of the whole search, and confining the walk to the LIVE range is what
// stops it from testing revisions the search has already excluded.
func probe(lo, hi, mid int, test func(int) (Verdict, bool)) (Verdict, int, bool) {
	tried := map[int]bool{}
	for off := 0; ; off++ {
		cands := []int{mid - off, mid + off}
		if off == 0 {
			cands = []int{mid}
		}
		any := false
		for _, i := range cands {
			if i < lo || i > hi || tried[i] {
				continue
			}
			any = true
			tried[i] = true
			v, cont := test(i)
			if !cont {
				return Skip, i, false
			}
			if v != Skip {
				return v, i, true
			}
		}
		if !any && mid-off < lo && mid+off > hi {
			return Skip, mid, false
		}
	}
}

// testRev tests one revision that is not part of the binary search (an
// endpoint), appending its step.
func testRev(ctx context.Context, opts Options, repo *Repo, idx int, rev string, rep *Report) (Verdict, bool) {
	if ctx.Err() != nil {
		rep.Undetermined = "the search was cancelled"
		return Skip, false
	}
	st := runStep(ctx, opts, repo, idx, rev)
	rep.Steps = append(rep.Steps, st)
	if st.Verdict == Skip {
		rep.Skipped = append(rep.Skipped, st.Short)
	}
	tracef(opts.Trace, "bisect: %s %-9s %s", st.Short, st.Verdict, st.Reason)
	return st.Verdict, true
}

// runStep checks out, rebuilds and tests one revision.
//
// Every failure here is a SKIP, never a Good or a Bad. A revision that will not
// check out, or whose build is broken, says nothing about whether the world
// reproduces there, and the two ways of guessing move the answer in opposite
// directions.
func runStep(ctx context.Context, opts Options, repo *Repo, idx int, rev string) Step {
	start := opts.now()
	st := Step{
		Index:   idx,
		Rev:     rev,
		Short:   repo.Short(ctx, rev),
		Subject: repo.Subject(ctx, rev),
	}
	defer func() { st.Duration = opts.now().Sub(start) }()

	if err := repo.Checkout(ctx, rev); err != nil {
		st.Verdict = Skip
		st.Reason = "could not check out: " + err.Error()
		return st
	}

	if opts.Build != nil {
		if err := opts.Build(ctx, rev); err != nil {
			st.Verdict = Skip
			st.Reason = "rebuild failed: " + err.Error()
			return st
		}
	}

	v, reason, err := opts.Test(ctx, rev)
	if err != nil {
		st.Verdict = Skip
		st.Reason = "replay could not be judged: " + err.Error()
		return st
	}
	switch v {
	case Good, Bad, Skip:
		st.Verdict = v
	default:
		st.Verdict = Skip
		st.Reason = fmt.Sprintf("the test returned an unrecognised verdict %q, treated as skip", v)
		return st
	}
	st.Reason = reason
	return st
}

func restoreTarget(h Head) string {
	if h.Branch != "" {
		return h.Branch
	}
	return h.Commit
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
