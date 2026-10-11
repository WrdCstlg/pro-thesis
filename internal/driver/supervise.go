package driver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// Status is how a supervised driver finished. The four outcomes are kept
// distinct because they demand different responses from the run engine:
//
//	StatusOK         the workload ran to completion; assert on it
//	StatusFailed     the driver exited non-zero; its history may be partial
//	StatusTimeout    the driver outlived its budget and was killed
//	StatusCanceled   the run was cancelled and the driver was killed
//	StatusStartError the driver never ran at all
//
// Collapsing timeout into failure would let a driver that hangs forever look
// like a driver that reported an error, and the two point at completely
// different bugs.
type Status string

const (
	StatusOK         Status = "ok"
	StatusFailed     Status = "failed"
	StatusTimeout    Status = "timeout"
	StatusCanceled   Status = "canceled"
	StatusStartError Status = "start_error"
)

// DefaultGrace is how long a killed driver is given to exit on its own before
// the tree is force-killed.
//
// On Unix the supervisor sends SIGTERM to the process group first and waits this
// long. On Windows there is no portable equivalent (a console application
// ignores WM_CLOSE and `taskkill` without /F does nothing useful to it) so the
// grace step is skipped there. See killtree_windows.go.
const DefaultGrace = 2 * time.Second

// Request describes one driver invocation.
type Request struct {
	// Cmd is the `driver.cmd` template from prothesis.yaml, verbatim.
	Cmd string
	// Dir is the working directory. Every relative path in Cmd resolves against
	// it, so it is normally the directory prothesis.yaml lives in.
	Dir string
	// Sub fills the command template's placeholders.
	Sub Substitution
	// Timeout bounds the driver's whole run. Zero means no timeout, which is
	// only appropriate when the caller's context already carries a deadline.
	Timeout time.Duration
	// Grace is how long a killed driver may take to exit before the tree is
	// force-killed. Zero means DefaultGrace.
	Grace time.Duration
	// Env is the driver's environment. Nil inherits the harness's own.
	// PROTHESIS_PLAN_PATH is appended to whichever is used.
	Env []string
	// Stdout and Stderr receive the driver's output. Nil discards it.
	Stdout io.Writer
	Stderr io.Writer

	// StdinControl gives the driver a stdin PIPE instead of the null device and
	// exports StdinControlEnv=1, so Supervisor.Drain can later ask it to stop
	// issuing new operations while completing the ones it holds. See drain.go.
	//
	// ADDITIVE, and off by default. Handing an unsuspecting driver a pipe that
	// is never written and never closed would turn an EOF it expects into an
	// indefinite block, so this is opted into rather than assumed.
	StdinControl bool
}

// Result is what a supervised driver did.
type Result struct {
	Status Status `json:"status"`
	// Argv is the resolved command, recorded so a verdict can state exactly
	// what was run rather than the template it came from.
	Argv []string `json:"argv"`
	// ExitCode is the process exit code, or -1 when the process did not exit
	// normally (killed, signalled, or never started).
	ExitCode int `json:"exit_code"`
	// Signaled and Signal report a process terminated by a signal. Always false
	// on Windows, which has no signals.
	Signaled bool   `json:"signaled"`
	Signal   string `json:"signal,omitempty"`
	// Killed records that the SUPERVISOR killed the process tree, as opposed to
	// the driver exiting on its own.
	Killed bool `json:"killed"`
	// TreeKillErr is set when killing the tree itself failed. This is serious:
	// an orphaned loadgen holds connections open and corrupts the next world.
	TreeKillErr string `json:"tree_kill_err,omitempty"`

	// StartTNS and EndTNS are Unix epoch nanoseconds, the same frame as the
	// history log's t_ns.
	StartTNS   int64 `json:"start_t_ns"`
	EndTNS     int64 `json:"end_t_ns"`
	DurationNS int64 `json:"duration_ns"`

	// Err is the underlying error, if any. Status is the classification;
	// this is the detail.
	Err error `json:"-"`
	// ErrText is Err rendered for the artifact record.
	ErrText string `json:"err,omitempty"`
}

// OK reports the one outcome that means the workload ran as asked.
func (r *Result) OK() bool { return r != nil && r.Status == StatusOK }

// Duration returns the driver's wall-clock run time.
func (r *Result) Duration() time.Duration { return time.Duration(r.DurationNS) }

// ExitCodeHint maps a driver outcome onto the CLI exit-code space.
//
// A driver that could not START, or that was killed by the harness, is an
// ENVIRONMENT failure (exit 2, INCONCLUSIVE, "retry once or escalate") and
// never a FAIL. Reporting exit 1 there would tell an agent loop that an oracle
// found a violation, when in fact no oracle ever ran. A driver that ran and
// exited non-zero is also inconclusive rather than a failure: the workload
// generator's own opinion of itself is not an oracle verdict.
func (r *Result) ExitCodeHint() schema.ExitCode {
	if r.OK() {
		return schema.ExitPass
	}
	return schema.ExitInconclusive
}

// Supervisor runs a driver process.
type Supervisor struct {
	mu   sync.Mutex
	cmd  *exec.Cmd
	live bool

	// stdin is the drain control channel, non-nil only when the Request asked
	// for one. stdinClosed makes Drain idempotent: os/exec's Wait closes this
	// pipe too, and a second Close would otherwise be reported as a failure.
	stdin       io.WriteCloser
	stdinClosed bool

	// killRequested records that Kill was called, so Run can classify the exit
	// as a kill rather than as the driver's own failure.
	//
	// Without it, a driver stopped at QUIESCE (every driver, before the drain
	// existed) came back as StatusFailed with Killed false, and the run
	// recorded `stopped: false` for a workload the harness had just killed.
	// DriverResult.Stopped is what tells an oracle that a non-zero exit was
	// EXPECTED, so getting it wrong points the reader at the workload for
	// something the harness did.
	killRequested bool
}

// NewSupervisor returns a Supervisor.
func NewSupervisor() *Supervisor { return &Supervisor{} }

// Run starts the driver, streams its output, and waits for it to finish.
//
// It always returns a Result, even when it also returns an error: the caller
// needs the classification to decide what to report, and a nil Result would
// force every call site to re-derive it.
func (s *Supervisor) Run(ctx context.Context, req Request) (*Result, error) {
	res := &Result{ExitCode: -1}

	argv, err := ResolveArgv(req.Cmd, req.Sub)
	if err != nil {
		res.Status = StatusStartError
		res.Err = err
		res.ErrText = err.Error()
		return res, err
	}
	res.Argv = argv
	if left := UnresolvedPlaceholders(argv); len(left) > 0 {
		err := fmt.Errorf("driver: placeholders %s survived substitution in driver.cmd; "+
			"the driver would receive them literally", strings.Join(left, ", "))
		res.Status = StatusStartError
		res.Err = err
		res.ErrText = err.Error()
		return res, err
	}

	grace := req.Grace
	if grace <= 0 {
		grace = DefaultGrace
	}

	// The supervisor owns cancellation, NOT exec.CommandContext. CommandContext
	// kills only the direct child; an orphaned loadgen would keep its
	// connections open and corrupt the next world. So the command is started
	// without a context and the watchdog below kills the whole tree.
	runCtx := ctx
	var cancel context.CancelFunc
	if req.Timeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, req.Timeout)
		defer cancel()
	}

	prog := resolveProgram(argv[0], req.Dir)
	cmd := exec.Command(prog, argv[1:]...)
	cmd.Dir = req.Dir
	cmd.Stdout = req.Stdout
	cmd.Stderr = req.Stderr
	cmd.Env = buildEnv(req)
	setProcessGroup(cmd)

	// The drain control channel, when one was asked for. It must be created
	// BEFORE Start and recorded only AFTER a successful Start, so a Drain racing
	// a failed launch cannot write into a pipe nobody is reading.
	var stdinPipe io.WriteCloser
	if req.StdinControl {
		p, err := cmd.StdinPipe()
		if err != nil {
			res.Status = StatusStartError
			res.Err = fmt.Errorf("driver: open the stdin control channel: %w", err)
			res.ErrText = res.Err.Error()
			return res, res.Err
		}
		stdinPipe = p
	}

	res.StartTNS = time.Now().UnixNano()
	if err := cmd.Start(); err != nil {
		if stdinPipe != nil {
			_ = stdinPipe.Close()
		}
		res.EndTNS = time.Now().UnixNano()
		res.Status = StatusStartError
		res.Err = fmt.Errorf("driver: start %s: %w", argv[0], err)
		res.ErrText = res.Err.Error()
		return res, res.Err
	}

	s.mu.Lock()
	s.cmd, s.live = cmd, true
	s.stdin, s.stdinClosed = stdinPipe, false
	s.killRequested = false
	s.mu.Unlock()

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	var killErr error
	killed := false
	select {
	case err = <-done:
	case <-runCtx.Done():
		killed = true
		killErr = terminateTree(cmd, grace)
		// Wait unconditionally after killing. Skipping this leaks the child's
		// pipes and its zombie entry, and on Windows leaves the handle open so
		// the run directory cannot be removed.
		err = <-done
	}

	res.EndTNS = time.Now().UnixNano()
	res.DurationNS = res.EndTNS - res.StartTNS

	s.mu.Lock()
	s.live = false
	// os/exec's Wait has already closed the pipe it handed us; record that so a
	// late Drain does not report ErrClosed as a drain failure.
	s.stdinClosed = true
	// An EXTERNAL Kill (which is what QUIESCE does) does not cancel runCtx, so
	// without this the exit it caused would be classified as the driver's own
	// failure.
	killed = killed || s.killRequested
	s.mu.Unlock()

	if killErr != nil {
		res.TreeKillErr = killErr.Error()
	}
	res.Killed = killed
	classify(res, runCtx, ctx, err, killed)
	if res.Err != nil {
		return res, res.Err
	}
	return res, nil
}

// classify turns a Wait error and the cancellation state into a Status.
//
// Timeout is distinguished from cancellation by asking which context expired:
// the run context carries the driver's own timeout, the caller's context carries
// the run's. The distinction matters (a timeout means the workload is too slow
// or wedged, a cancellation means someone pressed Ctrl-C) and collapsing them
// would put the wrong sentence in the verdict.
func classify(res *Result, runCtx, callerCtx context.Context, waitErr error, killed bool) {
	if killed {
		switch {
		case callerCtx.Err() != nil:
			res.Status = StatusCanceled
			res.Err = fmt.Errorf("driver: run cancelled after %s: %w",
				res.Duration().Round(time.Millisecond), callerCtx.Err())
		case errors.Is(runCtx.Err(), context.DeadlineExceeded):
			res.Status = StatusTimeout
			res.Err = fmt.Errorf("driver: timed out after %s and the process tree was killed",
				res.Duration().Round(time.Millisecond))
		default:
			res.Status = StatusCanceled
			res.Err = fmt.Errorf("driver: killed after %s", res.Duration().Round(time.Millisecond))
		}
		res.ErrText = res.Err.Error()
		res.ExitCode = exitCodeOf(waitErr)
		res.Signaled, res.Signal = signalOf(waitErr)
		return
	}

	if waitErr == nil {
		res.Status = StatusOK
		res.ExitCode = 0
		return
	}

	var ee *exec.ExitError
	if errors.As(waitErr, &ee) {
		res.Status = StatusFailed
		res.ExitCode = ee.ExitCode()
		res.Signaled, res.Signal = signalOf(waitErr)
		res.Err = fmt.Errorf("driver: exited %d after %s",
			res.ExitCode, res.Duration().Round(time.Millisecond))
		res.ErrText = res.Err.Error()
		return
	}

	res.Status = StatusFailed
	res.Err = fmt.Errorf("driver: %w", waitErr)
	res.ErrText = res.Err.Error()
}

func exitCodeOf(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

// buildEnv assembles the driver's environment.
//
// PROTHESIS_PLAN_PATH is ALWAYS exported (DECISIONS.md D-021). The dual
// transport exists so a driver that does not spell {plan_path} in its command
// line can still find the plan; a driver that supports neither ignores both.
// Any inherited value of the same name is REPLACED, not appended: two entries
// with one name is undefined behaviour across platforms, and the harness's
// value must win.
//
// StdinControlEnv is exported only when a control channel actually exists, and
// an inherited value is stripped in BOTH directions. That second half is not
// tidiness: the fixture's loadgen defaults --stdin-control to
// PROTHESIS_STDIN_CONTROL=1, so an inherited 1 with no pipe attached would point
// its stdin watcher at the null device, read instant EOF, and stop the workload
// before it issued a single operation; a world that reports on a system it
// never touched.
func buildEnv(req Request) []string {
	base := req.Env
	if base == nil {
		base = os.Environ()
	}
	planPrefix := schema.PlanPathEnv + "="
	stdinPrefix := StdinControlEnv + "="
	out := make([]string, 0, len(base)+2)
	for _, kv := range base {
		if strings.HasPrefix(kv, planPrefix) || strings.HasPrefix(kv, stdinPrefix) {
			continue
		}
		out = append(out, kv)
	}
	out = append(out, planPrefix+req.Sub.PlanPath)
	if req.StdinControl {
		out = append(out, stdinPrefix+"1")
	}
	return out
}

// Kill terminates a running driver's process tree. Safe to call when nothing is
// running.
func (s *Supervisor) Kill(grace time.Duration) error {
	s.mu.Lock()
	cmd, live := s.cmd, s.live
	if live {
		s.killRequested = true
	}
	s.mu.Unlock()
	if !live || cmd == nil || cmd.Process == nil {
		return nil
	}
	if grace <= 0 {
		grace = DefaultGrace
	}
	return terminateTree(cmd, grace)
}

// PID returns the running driver's process id, or 0.
func (s *Supervisor) PID() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.live || s.cmd == nil || s.cmd.Process == nil {
		return 0
	}
	return s.cmd.Process.Pid
}

// resolveProgram maps a command's argv[0] to the file that will be executed.
//
// A BARE name (one containing no path separator) is a PATH lookup and nothing
// else. It is never resolved against dir.
//
// That restriction is the whole point of this function's current shape, and it
// was not always true. Until OQ-061 a bare name was stat'd inside the project
// directory FIRST, so a repository shipping a file called `python.exe` at its
// root captured `cmd: python check.py`: a reviewer reading the config saw system
// Python and PRO-THESIS ran the repository's binary. That is precisely what Go
// 1.19 removed with exec.ErrDot, re-introduced by hand, and it is reached by
// the driver AND by every external oracle (oracle/process.go), so the captured
// program could be the one deciding every verdict. The lock does not cover it:
// a planted file sits outside oracles.dir.
//
// The rule below is Go's own, and it is also the shell's: a separator means "a
// path", no separator means "look it up". Both forms the repository actually
// ships keep working (`./bin/loadgen` and `thesis-helpers/steady.sh` carry a
// separator) while `python` can only ever mean the one on PATH.
//
// The .exe suffix is still supplied for path-bearing programs, because
// prothesis.yaml is portable and does not write it (`cmd: ./bin/linearizable-kv`
// resolves to the .exe on Windows). For a bare name that is exec's job, via
// PATHEXT.
func resolveProgram(prog, dir string) string {
	if !strings.ContainsAny(prog, `/\`) {
		return prog
	}
	if filepath.IsAbs(prog) {
		return prog
	}
	if dir != "" {
		candidate := filepath.Join(dir, prog)
		if fi, err := os.Stat(candidate); err == nil && !fi.IsDir() {
			return candidate
		}
		if fi, err := os.Stat(candidate + ".exe"); err == nil && !fi.IsDir() {
			return candidate + ".exe"
		}
	}
	// No project directory: the path is relative to the process's own working
	// directory, and only the suffix needs supplying.
	if fi, err := os.Stat(prog + ".exe"); err == nil && !fi.IsDir() {
		return prog + ".exe"
	}
	return prog
}
