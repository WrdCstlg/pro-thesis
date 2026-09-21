package bisect

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// gitTimeout bounds any single git invocation. A hung git must not hold a
// bisect open on a detached HEAD.
const gitTimeout = 60 * time.Second

// Repo is a git working tree.
//
// It is a thin wrapper over the git CLI rather than a library, for the same
// reason D-016 gives for Docker: the CLI is the interface the user's own
// configuration (worktrees, includes, credential helpers, core.autocrlf) is
// resolved through, and a Go reimplementation would silently disagree with it.
type Repo struct {
	// Dir is the working tree.
	Dir string
	// Git is the executable name. Empty means "git".
	Git string
}

func (r *Repo) bin() string {
	if r.Git == "" {
		return "git"
	}
	return r.Git
}

// run executes a git subcommand and returns trimmed stdout.
func (r *Repo) run(ctx context.Context, args ...string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()

	full := append([]string{"-C", r.Dir}, args...)
	cmd := exec.CommandContext(cctx, r.bin(), full...)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = strings.TrimSpace(out.String())
		}
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, msg)
	}
	return strings.TrimRight(out.String(), "\r\n"), nil
}

// IsRepo reports whether Dir is inside a git working tree.
func (r *Repo) IsRepo(ctx context.Context) bool {
	out, err := r.run(ctx, "rev-parse", "--is-inside-work-tree")
	return err == nil && strings.TrimSpace(out) == "true"
}

// Resolve turns a revision into a full object id, and fails when it does not
// name a commit.
func (r *Repo) Resolve(ctx context.Context, rev string) (string, error) {
	out, err := r.run(ctx, "rev-parse", "--verify", rev+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("bisect: %q does not name a commit in %s: %w", rev, r.Dir, err)
	}
	return strings.TrimSpace(out), nil
}

// Short abbreviates a revision for display. It never fails the caller: an
// unresolvable revision renders as itself.
func (r *Repo) Short(ctx context.Context, rev string) string {
	out, err := r.run(ctx, "rev-parse", "--short", rev)
	if err != nil {
		if len(rev) > 12 {
			return rev[:12]
		}
		return rev
	}
	return strings.TrimSpace(out)
}

// Subject is a commit's first log line, for the report.
func (r *Repo) Subject(ctx context.Context, rev string) string {
	out, err := r.run(ctx, "log", "-1", "--format=%s", rev)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// Head describes where HEAD points now, so it can be put back.
//
// Two shapes, and the difference matters on restoration: a BRANCH is restored by
// name so the user keeps their branch, while a detached HEAD is restored by
// object id with --detach so the user is put back exactly where they were rather
// than silently attached to a branch they were not on.
type Head struct {
	// Branch is the branch name, or "" when HEAD is detached.
	Branch string
	// Commit is the object id HEAD resolved to.
	Commit string
}

// String renders the head for a message.
func (h Head) String() string {
	if h.Branch != "" {
		return h.Branch
	}
	if len(h.Commit) > 12 {
		return "detached at " + h.Commit[:12]
	}
	return "detached at " + h.Commit
}

// Head reads the current HEAD.
func (r *Repo) Head(ctx context.Context) (Head, error) {
	commit, err := r.run(ctx, "rev-parse", "HEAD")
	if err != nil {
		return Head{}, err
	}
	h := Head{Commit: strings.TrimSpace(commit)}
	// --quiet exits non-zero on a detached HEAD, which is not an error here.
	if branch, err := r.run(ctx, "symbolic-ref", "--quiet", "--short", "HEAD"); err == nil {
		h.Branch = strings.TrimSpace(branch)
	}
	return h, nil
}

// Dirty reports whether the working tree has uncommitted changes to TRACKED
// files.
//
// Untracked files are deliberately NOT dirty. A PRO-THESIS project always has
// them (`.prothesis/runs/` alone) and refusing to bisect because of run output
// would make the command unusable in the tree it was written for. Checkout
// refuses on its own if it would overwrite an untracked file, and that error is
// handled as a failed step with the tree restored.
func (r *Repo) Dirty(ctx context.Context) (bool, string, error) {
	out, err := r.run(ctx, "status", "--porcelain", "--untracked-files=no")
	if err != nil {
		return false, "", err
	}
	out = strings.TrimSpace(out)
	return out != "", out, nil
}

// Checkout moves HEAD to rev, detaching.
func (r *Repo) Checkout(ctx context.Context, rev string) error {
	_, err := r.run(ctx, "checkout", "--detach", rev)
	return err
}

// Restore puts HEAD back where h says it was.
func (r *Repo) Restore(ctx context.Context, h Head) error {
	if h.Branch != "" {
		_, err := r.run(ctx, "checkout", h.Branch)
		return err
	}
	if h.Commit == "" {
		return errors.New("bisect: nothing recorded to restore to")
	}
	_, err := r.run(ctx, "checkout", "--detach", h.Commit)
	return err
}

// RevList returns the commits reachable from bad but not from good, OLDEST
// FIRST, with bad last.
//
// Oldest-first is the order the binary search assumes, and reversing here rather
// than at the call site keeps the one place that knows git's default ordering
// next to the invocation that produces it.
func (r *Repo) RevList(ctx context.Context, good, bad string) ([]string, error) {
	out, err := r.run(ctx, "rev-list", "--reverse", good+".."+bad)
	if err != nil {
		return nil, err
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return nil, nil
	}
	var revs []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			revs = append(revs, line)
		}
	}
	return revs, nil
}

// IsAncestor reports whether a is an ancestor of b.
func (r *Repo) IsAncestor(ctx context.Context, a, b string) (bool, error) {
	cctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, r.bin(), "-C", r.Dir, "merge-base", "--is-ancestor", a, b)
	var errb bytes.Buffer
	cmd.Stderr = &errb
	err := cmd.Run()
	if err == nil {
		return true, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 1 {
		return false, nil
	}
	return false, fmt.Errorf("git merge-base --is-ancestor: %w: %s", err, strings.TrimSpace(errb.String()))
}
