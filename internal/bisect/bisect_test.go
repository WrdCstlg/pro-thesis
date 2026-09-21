package bisect

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// bisect is the only command in this tree that mutates the USER'S WORKING TREE.
// Everything else creates containers and deletes them; this checks out
// revisions. The failure that matters is therefore not a wrong answer (a
// wrong answer costs a rerun) but leaving somebody on a detached HEAD after a
// build failed or a Ctrl-C landed. That is hostile in a way a testing tool has
// no licence to be, so the restoration tests come first and there are four of
// them.

// --- a disposable repository ----------------------------------------------

type testRepo struct {
	t    *testing.T
	repo *Repo
	revs []string // oldest first
}

func gitAvailable(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git is not on PATH: %v", err)
	}
}

// newTestRepo builds a repo with n commits on a branch called main, each
// touching one file so the revisions are distinguishable.
func newTestRepo(t *testing.T, n int) *testRepo {
	t.Helper()
	gitAvailable(t)
	dir := t.TempDir()
	r := &Repo{Dir: dir}
	ctx := context.Background()

	run := func(args ...string) {
		t.Helper()
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = dir
		// A committer identity must be supplied: a machine with no global git
		// config would otherwise fail here for reasons unrelated to the test.
		cmd.Env = append(cmd.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Skipf("git %s failed, skipping rather than reporting a false defect: %v: %s",
				strings.Join(args, " "), err, out)
		}
	}

	run("init", "--initial-branch=main")
	run("config", "user.name", "t")
	run("config", "user.email", "t@example.invalid")

	tr := &testRepo{t: t, repo: r}
	for i := 0; i < n; i++ {
		writeFile(t, dir, "f.txt", strings.Repeat("x", i+1)+"\n")
		run("add", "f.txt")
		run("commit", "-m", "c"+string(rune('0'+i)))
		rev, err := r.Resolve(ctx, "HEAD")
		if err != nil {
			t.Skipf("could not resolve HEAD: %v", err)
		}
		tr.revs = append(tr.revs, rev)
	}
	return tr
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := writeFileRaw(dir, name, content); err != nil {
		t.Fatal(err)
	}
}

func (tr *testRepo) head(t *testing.T) Head {
	t.Helper()
	h, err := tr.repo.Head(context.Background())
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	return h
}

// baseOptions is a bisect that would run cleanly, so each test changes exactly
// the one thing it is about.
func (tr *testRepo) baseOptions() Options {
	return Options{
		Repo:    tr.repo,
		Good:    tr.revs[0],
		Bad:     tr.revs[len(tr.revs)-1],
		NoBuild: true,
		Test: func(ctx context.Context, rev string) (Verdict, string, error) {
			// The last revision is bad, everything else good.
			if rev == tr.revs[len(tr.revs)-1] {
				return Bad, "reproduced", nil
			}
			return Good, "did not reproduce", nil
		},
	}
}

// --- THE HOSTILE FAILURE MODE ---------------------------------------------

func TestABrokenBuildLeavesTheUserOnTheirOriginalBranch(t *testing.T) {
	tr := newTestRepo(t, 6)
	before := tr.head(t)

	opts := tr.baseOptions()
	opts.NoBuild = false
	opts.Build = func(ctx context.Context, rev string) error {
		return errors.New("compiler exploded")
	}

	_, _ = Run(context.Background(), opts)

	after := tr.head(t)
	if after.Branch == "" {
		t.Fatalf("a failing build left the working tree on a DETACHED HEAD at %s. "+
			"A testing tool that strands a user mid-bisect after their build broke is worse "+
			"than one that refuses to bisect at all.", after)
	}
	if after.Branch != before.Branch {
		t.Fatalf("branch changed across a failed bisect: %q -> %q", before.Branch, after.Branch)
	}
}

func TestCancellationRestoresTheHead(t *testing.T) {
	tr := newTestRepo(t, 8)
	before := tr.head(t)

	ctx, cancel := context.WithCancel(context.Background())
	opts := tr.baseOptions()
	opts.Test = func(_ context.Context, rev string) (Verdict, string, error) {
		// Ctrl-C arrives in the middle of the search, while a revision is
		// checked out. This is the exact moment the restore has to survive.
		cancel()
		return Good, "cancelled mid-step", nil
	}

	_, _ = Run(ctx, opts)

	after := tr.head(t)
	if after.Branch == "" {
		t.Fatalf("cancellation left a DETACHED HEAD at %s. The restore must run on a context "+
			"the cancellation cannot also cancel, or Ctrl-C is precisely when it fails.", after)
	}
	if after.Branch != before.Branch {
		t.Fatalf("branch changed across a cancelled bisect: %q -> %q", before.Branch, after.Branch)
	}
}

func TestAFailingTestFunctionStillRestoresTheHead(t *testing.T) {
	tr := newTestRepo(t, 6)
	before := tr.head(t)

	opts := tr.baseOptions()
	opts.Test = func(_ context.Context, rev string) (Verdict, string, error) {
		return Skip, "", errors.New("the harness died")
	}

	_, _ = Run(context.Background(), opts)

	after := tr.head(t)
	if after.Branch == "" || after.Branch != before.Branch {
		t.Fatalf("a Test that errored at every revision left HEAD at %s, want %s", after, before)
	}
}

func TestACleanBisectRestoresTheHead(t *testing.T) {
	tr := newTestRepo(t, 8)
	before := tr.head(t)

	rep, err := Run(context.Background(), tr.baseOptions())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	after := tr.head(t)
	if after.Branch == "" || after.Branch != before.Branch {
		t.Fatalf("a SUCCESSFUL bisect left HEAD at %s, want %s — restoration must not be "+
			"conditional on failing", after, before)
	}
	if rep.FirstBad == "" {
		t.Fatalf("a bisect over a repo whose last commit is bad found no first-bad revision: %+v", rep)
	}
	if rep.FirstBad != tr.revs[len(tr.revs)-1] {
		t.Fatalf("first bad = %s, want the last revision %s", rep.FirstBad, tr.revs[len(tr.revs)-1])
	}
}

// --- refusing to start ------------------------------------------------------

func TestADirtyWorkingTreeIsRefusedUpFront(t *testing.T) {
	tr := newTestRepo(t, 4)
	// An uncommitted change to a TRACKED file. Checking out over it would
	// either fail or, worse, silently carry it across revisions and make every
	// verdict measure the user's uncommitted work.
	writeFile(t, tr.repo.Dir, "f.txt", "uncommitted local edit\n")

	_, err := Run(context.Background(), tr.baseOptions())
	if err == nil {
		t.Fatal("a dirty working tree was accepted; a bisect that checks out over uncommitted " +
			"changes can destroy them")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "dirty") &&
		!strings.Contains(strings.ToLower(err.Error()), "uncommitted") {
		t.Fatalf("the refusal does not name the cause, so a user cannot act on it: %v", err)
	}

	// And the escape hatch must exist, or a user with an unrelated stray file
	// can never bisect at all.
	opts := tr.baseOptions()
	opts.AllowDirty = true
	if _, err := Run(context.Background(), opts); err != nil {
		t.Fatalf("AllowDirty did not permit the bisect: %v", err)
	}
}

// An endpoint that contradicts its label means the user named the wrong
// revisions, and every subsequent checkout would be wasted work against a
// question that has no answer.
func TestContradictoryEndpointsAreRefusedWithoutCheckingOutAnything(t *testing.T) {
	t.Run("good already reproduces", func(t *testing.T) {
		tr := newTestRepo(t, 6)
		before := tr.head(t)
		opts := tr.baseOptions()
		opts.Test = func(_ context.Context, rev string) (Verdict, string, error) {
			return Bad, "reproduces everywhere", nil
		}
		rep, _ := Run(context.Background(), opts)
		if rep.FirstBad != "" {
			t.Fatalf("a bisect whose GOOD endpoint reproduces returned a first-bad revision %q; "+
				"there is no first bad commit in that range", rep.FirstBad)
		}
		if after := tr.head(t); after.Branch == "" || after.Branch != before.Branch {
			t.Fatalf("HEAD not restored after refusing: %s", after)
		}
	})

	t.Run("bad does not reproduce", func(t *testing.T) {
		tr := newTestRepo(t, 6)
		opts := tr.baseOptions()
		opts.Test = func(_ context.Context, rev string) (Verdict, string, error) {
			return Good, "reproduces nowhere", nil
		}
		rep, _ := Run(context.Background(), opts)
		if rep.FirstBad != "" {
			t.Fatalf("a bisect whose BAD endpoint does not reproduce returned %q", rep.FirstBad)
		}
	})
}

// --- the search itself ------------------------------------------------------

func TestTheSearchFindsTheFirstBadRevision(t *testing.T) {
	const n = 9
	for introduced := 1; introduced < n; introduced++ {
		tr := newTestRepo(t, n)
		want := tr.revs[introduced]

		// Bad from `introduced` onwards, good before it: the shape a real
		// regression has.
		badFrom := map[string]bool{}
		for i := introduced; i < n; i++ {
			badFrom[tr.revs[i]] = true
		}
		opts := tr.baseOptions()
		opts.Test = func(_ context.Context, rev string) (Verdict, string, error) {
			if badFrom[rev] {
				return Bad, "reproduced", nil
			}
			return Good, "did not", nil
		}

		rep, err := Run(context.Background(), opts)
		if err != nil {
			t.Fatalf("introduced at %d: %v", introduced, err)
		}
		if rep.FirstBad != want {
			t.Fatalf("introduced at index %d: first bad = %s, want %s", introduced, rep.FirstBad, want)
		}
		// Binary search, not linear: over 9 revisions it must not test them all.
		if len(rep.Steps) >= n {
			t.Fatalf("introduced at %d: tested %d revisions over a range of %d — that is a "+
				"linear scan, not a bisection", introduced, len(rep.Steps), n)
		}
	}
}

// A revision that cannot be judged is neither good nor bad. Guessing either way
// would move the answer to a commit that was never actually tested.
func TestAnUnjudgeableRevisionIsSkippedRatherThanGuessed(t *testing.T) {
	const n = 9
	tr := newTestRepo(t, n)
	const introduced = 5
	unbuildable := map[string]bool{tr.revs[3]: true, tr.revs[6]: true}

	opts := tr.baseOptions()
	opts.Test = func(_ context.Context, rev string) (Verdict, string, error) {
		if unbuildable[rev] {
			return Skip, "does not build at this revision", nil
		}
		for i := introduced; i < n; i++ {
			if rev == tr.revs[i] {
				return Bad, "reproduced", nil
			}
		}
		return Good, "did not", nil
	}

	rep, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, s := range rep.Steps {
		if s.Verdict == Skip && s.Reason == "" {
			t.Fatalf("a skipped revision carries no reason; a user cannot tell a broken build "+
				"from a broken harness: %+v", s)
		}
	}
	// The answer must still be a revision the search actually judged bad, and
	// never one it skipped.
	if rep.FirstBad != "" && unbuildable[rep.FirstBad] {
		t.Fatalf("the reported first-bad revision %s is one the search SKIPPED", rep.FirstBad)
	}
	if after := tr.head(t); after.Branch == "" {
		t.Fatalf("skips left a detached HEAD at %s", after)
	}
}

// bisect is a DIAGNOSTIC QUERY, not a gate, and its exit codes say so.
//
// The question is "which commit introduced this?", asked about a violation
// already known to exist, so answering it is success, exactly as `git bisect`
// exits 0 having found the first bad commit. Exit 1 in this project means
// "oracle violation, work from minimal_repro", which is what `run` and `regress`
// report; a bisect returning 1 would be claiming to have FOUND a violation
// rather than to have LOCATED one already found.
//
// The distinction that carries the weight here is the other one: an answer the
// search could not determine, and a working tree it could not restore, are both
// exit 2; "retry once, then escalate to a human".
func TestReportExitCodeFollowsTheNormativeContract(t *testing.T) {
	cases := []struct {
		name string
		rep  Report
		want schema.ExitCode
		why  string
	}{
		{
			name: "determined",
			rep:  Report{FirstBad: "abc123", Determined: true},
			want: schema.ExitPass,
			why:  "the query was answered and the tree restored; bisect asserts no violation of its own",
		},
		{
			name: "undetermined",
			rep:  Report{},
			want: schema.ExitInconclusive,
			why:  "'I could not tell' must never read as 'nothing is wrong'",
		},
		{
			name: "budget expired before the range collapsed",
			rep:  Report{BudgetExpired: true},
			want: schema.ExitBudgetExhausted,
			why:  "a truncated search is its own answer, distinct from a search that ran and failed to decide",
		},
		{
			// The case that matters most in practice: the search may have found
			// the answer and still left the user somewhere they never asked to
			// be. A correct answer does not excuse an unrestored working tree,
			// so RestoreErr outranks Determined.
			name: "determined but the tree could not be restored",
			rep:  Report{FirstBad: "abc123", Determined: true, RestoreErr: errors.New("checkout failed")},
			want: schema.ExitInconclusive,
			why:  "an unrestored working tree needs a human whatever the answer was",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.rep.ExitCode(); got != tc.want {
				t.Fatalf("ExitCode() = %d, want %d — %s", got, tc.want, tc.why)
			}
		})
	}
}

func TestRunRefusesWithoutARepoOrABuildDecision(t *testing.T) {
	if _, err := Run(context.Background(), Options{}); err == nil {
		t.Fatal("Run with no Repo was accepted")
	}

	tr := newTestRepo(t, 3)
	// Neither Build nor NoBuild. For a containerised target this is the
	// difference between bisecting the code and bisecting one image against
	// every revision, so it must be a deliberate choice rather than a default.
	opts := tr.baseOptions()
	opts.NoBuild = false
	opts.Build = nil
	if _, err := Run(context.Background(), opts); err == nil {
		t.Fatal("a bisect with no Build and no NoBuild was accepted; for a containerised " +
			"target that silently measures a single stale image against every revision")
	}
}

func TestTimeoutsAndTracing(t *testing.T) {
	tr := newTestRepo(t, 5)
	var trace strings.Builder
	opts := tr.baseOptions()
	opts.Trace = &trace
	opts.Now = func() time.Time { return time.Unix(0, 0) }

	if _, err := Run(context.Background(), opts); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if trace.Len() == 0 {
		t.Fatal("Trace was set and nothing was written; a bisect is long enough that a user " +
			"needs to see which revision it is on")
	}
}
