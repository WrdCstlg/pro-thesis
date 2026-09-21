package driver

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// -------------------------------------------------------------------------
// Status and Result: the outcomes must stay distinguishable
// -------------------------------------------------------------------------

var allStatuses = []Status{
	StatusOK, StatusFailed, StatusTimeout, StatusCanceled, StatusStartError,
}

// A supervisor that reported "failed" for every outcome would make every
// verdict unattributable: a driver that hung forever, a driver that could not
// start, and a driver that exited 1 point at three completely different bugs.
func TestStatusesAreDistinct(t *testing.T) {
	seen := map[Status]bool{}
	for _, s := range allStatuses {
		if s == "" {
			t.Fatalf("a status is the empty string; an unset Result would be indistinguishable " +
				"from a real outcome")
		}
		if seen[s] {
			t.Fatalf("status %q is used for two different outcomes; the run engine cannot tell "+
				"them apart and the verdict cannot say what happened", s)
		}
		seen[s] = true
	}
	if len(seen) != 5 {
		t.Fatalf("expected 5 distinct outcomes, got %d: %v", len(seen), seen)
	}
}

// Status is serialized into the run artifact, so its wire form is a contract.
func TestStatusSerializesAsItsOwnName(t *testing.T) {
	for _, s := range allStatuses {
		data, err := json.Marshal(&Result{Status: s})
		if err != nil {
			t.Fatalf("marshal Result{%s}: %v", s, err)
		}
		want := `"status":"` + string(s) + `"`
		if !bytes.Contains(data, []byte(want)) {
			t.Fatalf("Result{%s} did not serialize %s:\n%s", s, want, data)
		}
	}
}

func TestResultOKIsTrueForExactlyOneStatus(t *testing.T) {
	for _, s := range allStatuses {
		r := &Result{Status: s}
		want := s == StatusOK
		if r.OK() != want {
			t.Fatalf("Result{%s}.OK() = %v, want %v; OK must mean the workload ran as asked "+
				"and nothing else", s, r.OK(), want)
		}
	}
	if (*Result)(nil).OK() {
		t.Fatalf("a nil Result reported OK")
	}
}

// A driver that could not start, was killed, or exited non-zero is an
// ENVIRONMENT problem, not an oracle violation. Reporting exit 1 there would
// tell an agent loop that an oracle found a bug when no oracle ever ran.
func TestExitCodeHintNeverClaimsAnOracleViolation(t *testing.T) {
	tests := []struct {
		status Status
		want   schema.ExitCode
	}{
		{StatusOK, schema.ExitPass},
		{StatusFailed, schema.ExitInconclusive},
		{StatusTimeout, schema.ExitInconclusive},
		{StatusCanceled, schema.ExitInconclusive},
		{StatusStartError, schema.ExitInconclusive},
	}
	if len(tests) != len(allStatuses) {
		t.Fatalf("a status was added without deciding its exit code: %v", allStatuses)
	}
	for _, tc := range tests {
		got := (&Result{Status: tc.status}).ExitCodeHint()
		if got != tc.want {
			t.Fatalf("status %s maps to exit %d, want %d", tc.status, got, tc.want)
		}
		if got == schema.ExitFail {
			t.Fatalf("status %s maps to exit 1 (FAIL); the workload generator's own opinion of "+
				"itself is not an oracle verdict, and an agent loop would go hunting for a "+
				"violation that was never found", tc.status)
		}
	}
}

func TestResultDurationMirrorsTheRecordedNanoseconds(t *testing.T) {
	r := &Result{DurationNS: int64(1500 * time.Millisecond)}
	if r.Duration() != 1500*time.Millisecond {
		t.Fatalf("Duration() = %s, want 1.5s", r.Duration())
	}
}

// -------------------------------------------------------------------------
// classify: the four outcomes, without a process in the way
// -------------------------------------------------------------------------

func TestClassifyDistinguishesTimeoutFromCancellation(t *testing.T) {
	// A run context whose deadline has already passed.
	expired, cancelExpired := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelExpired()
	// A caller context that has been cancelled.
	cancelled, cancelCancelled := context.WithCancel(context.Background())
	cancelCancelled()

	live := context.Background()
	exitErr := mustExitError(t, 7)

	tests := []struct {
		name       string
		runCtx     context.Context
		callerCtx  context.Context
		waitErr    error
		killed     bool
		wantStatus Status
		wantCode   int
		why        string
	}{
		{
			name: "clean exit", runCtx: live, callerCtx: live, waitErr: nil, killed: false,
			wantStatus: StatusOK, wantCode: 0,
			why: "a driver that ran to completion is the only outcome worth asserting on",
		},
		{
			name: "non-zero exit", runCtx: live, callerCtx: live, waitErr: exitErr, killed: false,
			wantStatus: StatusFailed, wantCode: 7,
			why: "the exit code is the driver's own diagnosis and must reach the verdict intact",
		},
		{
			name: "timeout", runCtx: expired, callerCtx: live, waitErr: nil, killed: true,
			wantStatus: StatusTimeout, wantCode: -1,
			why: "a wedged workload and a cancelled run demand different responses",
		},
		{
			name: "cancellation", runCtx: cancelled, callerCtx: cancelled, waitErr: nil, killed: true,
			wantStatus: StatusCanceled, wantCode: -1,
			why: "someone pressed Ctrl-C; that is not a finding about the system under test",
		},
		{
			name: "killed for another reason", runCtx: live, callerCtx: live, waitErr: nil, killed: true,
			wantStatus: StatusCanceled, wantCode: -1,
			why: "an explicit Kill is still a kill, not a driver failure",
		},
		{
			name: "wait failed for a non-exit reason", runCtx: live, callerCtx: live,
			waitErr: errors.New("i/o error on the pipe"), killed: false,
			wantStatus: StatusFailed, wantCode: -1,
			why: "a supervision error must not masquerade as a clean run",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res := &Result{ExitCode: -1, DurationNS: int64(time.Second)}
			classify(res, tc.runCtx, tc.callerCtx, tc.waitErr, tc.killed)
			if res.Status != tc.wantStatus {
				t.Fatalf("status = %s, want %s\nwhy this matters: %s", res.Status, tc.wantStatus, tc.why)
			}
			if res.ExitCode != tc.wantCode {
				t.Fatalf("exit code = %d, want %d", res.ExitCode, tc.wantCode)
			}
			if tc.wantStatus == StatusOK {
				if res.Err != nil || res.ErrText != "" {
					t.Fatalf("a clean run carries an error: %v / %q", res.Err, res.ErrText)
				}
				return
			}
			if res.Err == nil {
				t.Fatalf("status %s carries no error; the verdict would have nothing to say "+
					"about why the driver did not finish", res.Status)
			}
			if res.ErrText != res.Err.Error() {
				t.Fatalf("ErrText %q does not mirror Err %q, so the artifact and the log disagree",
					res.ErrText, res.Err)
			}
		})
	}
}

// The timeout and cancellation messages must not be interchangeable: an
// operator reading the verdict has to know which one happened.
func TestClassifyTimeoutAndCancellationSayDifferentThings(t *testing.T) {
	expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	cancelled, cancel2 := context.WithCancel(context.Background())
	cancel2()

	timeoutRes := &Result{ExitCode: -1}
	classify(timeoutRes, expired, context.Background(), nil, true)
	cancelRes := &Result{ExitCode: -1}
	classify(cancelRes, cancelled, cancelled, nil, true)

	if timeoutRes.ErrText == cancelRes.ErrText {
		t.Fatalf("a timeout and a cancellation produce the same message %q", timeoutRes.ErrText)
	}
	if !strings.Contains(timeoutRes.ErrText, "timed out") {
		t.Fatalf("the timeout message does not say so: %q", timeoutRes.ErrText)
	}
	if !strings.Contains(cancelRes.ErrText, "cancel") {
		t.Fatalf("the cancellation message does not say so: %q", cancelRes.ErrText)
	}
}

func TestExitCodeOf(t *testing.T) {
	if got := exitCodeOf(nil); got != -1 {
		t.Fatalf("exitCodeOf(nil) = %d, want -1 (no process exit to report)", got)
	}
	if got := exitCodeOf(errors.New("boom")); got != -1 {
		t.Fatalf("exitCodeOf(non-exit error) = %d, want -1", got)
	}
	if got := exitCodeOf(mustExitError(t, 3)); got != 3 {
		t.Fatalf("exitCodeOf(exit 3) = %d, want 3", got)
	}
	if got := exitCodeOf(fmt.Errorf("wrapped: %w", mustExitError(t, 4))); got != 4 {
		t.Fatalf("exitCodeOf could not see through a wrapped error: %d, want 4", got)
	}
}

// signalOf is the one genuinely OS-specific classifier. Its POSIX form reads a
// WaitStatus; its Windows form reports "no signals, ever". Both must agree that
// a nil error and an ordinary non-zero exit are not signals, or the verdict
// would claim a driver was killed when it exited on its own.
func TestSignalOfDoesNotInventSignals(t *testing.T) {
	if sig, name := signalOf(nil); sig || name != "" {
		t.Fatalf("signalOf(nil) = (%v, %q), want (false, \"\")", sig, name)
	}
	if sig, name := signalOf(errors.New("boom")); sig || name != "" {
		t.Fatalf("signalOf(non-exit error) = (%v, %q), want (false, \"\")", sig, name)
	}
	if sig, name := signalOf(mustExitError(t, 3)); sig || name != "" {
		t.Fatalf("a driver that exited 3 on its own was reported as signalled (%v, %q); the "+
			"verdict would blame the harness for a failure the driver chose", sig, name)
	}
}

// -------------------------------------------------------------------------
// buildEnv
// -------------------------------------------------------------------------

func TestBuildEnvExportsThePlanPathExactlyOnce(t *testing.T) {
	const planPath = `C:\AI Projects\Pro-synthesis\.prothesis\runs\r1\w1\plan.json`
	prefix := schema.PlanPathEnv + "="

	tests := []struct {
		name string
		env  []string
		why  string
	}{
		{
			name: "inherited environment",
			env:  nil,
			why:  "a nil Env inherits the harness's own environment and still gets the plan path",
		},
		{
			name: "explicit environment",
			env:  []string{"A=1", "B=2"},
			why:  "an explicit Env must still receive the plan path (D-021: it is ALWAYS exported)",
		},
		{
			name: "environment already carrying a stale plan path",
			env:  []string{"A=1", prefix + "/stale/plan.json", "B=2"},
			why: "two entries with one name is undefined behaviour across platforms, and the " +
				"harness's value must win over anything inherited",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out := buildEnv(Request{Env: tc.env, Sub: Substitution{PlanPath: planPath}})
			var hits []string
			for _, kv := range out {
				if strings.HasPrefix(kv, prefix) {
					hits = append(hits, kv)
				}
			}
			if len(hits) != 1 {
				t.Fatalf("%s appears %d times (%v); %s", schema.PlanPathEnv, len(hits), hits, tc.why)
			}
			if hits[0] != prefix+planPath {
				t.Fatalf("%s = %q, want %q; %s", schema.PlanPathEnv, hits[0], prefix+planPath, tc.why)
			}
			if tc.env != nil {
				for _, want := range []string{"A=1", "B=2"} {
					if !containsString(out, want) {
						t.Fatalf("buildEnv dropped %q from the caller's environment: %v", want, out)
					}
				}
			}
		})
	}
}

func containsString(hay []string, needle string) bool {
	for _, s := range hay {
		if s == needle {
			return true
		}
	}
	return false
}

// -------------------------------------------------------------------------
// resolveProgram and terminateTree guards
// -------------------------------------------------------------------------

func TestResolveProgram(t *testing.T) {
	dir := t.TempDir()
	write := func(name string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
		return p
	}
	// A path-bearing program, which is the form every shipped config uses.
	if err := os.Mkdir(filepath.Join(dir, "bin"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	nested := write(filepath.Join("bin", "loadgen"))
	nestedExe := write(filepath.Join("bin", "winonly.exe"))

	// The planted binaries of OQ-061. Both sit where a cloned repository would
	// put them, and neither may ever be chosen for a BARE argv[0].
	write("python")
	write("python.exe")

	if err := os.Mkdir(filepath.Join(dir, "adir"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	tests := []struct {
		name string
		prog string
		dir  string
		want string
		why  string
	}{
		{
			name: "path-bearing program resolves against the project directory",
			prog: "./bin/loadgen", dir: dir, want: nested,
			why: "driver.cmd's ./bin/loadgen is relative to the directory prothesis.yaml lives in",
		},
		{
			name: "a relative path without ./ is still a path",
			prog: "bin/loadgen", dir: dir, want: nested,
			why: "thesis-helpers/steady.sh is a shipped form; a separator means a path, as in any shell",
		},
		{
			name: "windows executable suffix is supplied for a path",
			prog: "./bin/winonly", dir: dir, want: nestedExe,
			why: "prothesis.yaml is portable; the .exe suffix is not written in the config",
		},
		{
			name: "OQ-061: a bare name NEVER resolves inside the project",
			prog: "python", dir: dir, want: "python",
			why: "a repository shipping ./python.exe must not capture `cmd: python check.py`. " +
				"This is exec.ErrDot's rule and it is reached by the driver AND by every " +
				"external oracle, so the captured program could be the one deciding the verdict",
		},
		{
			name: "a directory is not a program",
			prog: "./adir", dir: dir, want: "./adir",
			why: "resolving to a directory would produce a confusing exec failure",
		},
		{
			name: "unknown path is left alone for exec to report",
			prog: "./nope", dir: dir, want: "./nope",
			why: "reporting a missing program belongs to exec, not to this function",
		},
		{
			name: "absolute paths are returned untouched",
			prog: filepath.Join(dir, "bin", "loadgen"), dir: dir, want: nested,
			why: "an absolute program is already resolved; joining it to dir would corrupt it",
		},
		{
			name: "no project directory",
			prog: "./bin/loadgen", dir: "", want: "./bin/loadgen",
			why: "with no directory the path is relative to the process's own cwd, for exec to resolve",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveProgram(tc.prog, tc.dir); got != tc.want {
				t.Fatalf("resolveProgram(%q, %q) = %q, want %q\nwhy this matters: %s",
					tc.prog, tc.dir, got, tc.want, tc.why)
			}
		})
	}
}

// terminateTree runs on the cancellation path, which is exactly where a nil
// dereference would replace a clean shutdown with a panic.
func TestTerminateTreeIsSafeWithNothingToKill(t *testing.T) {
	if err := terminateTree(nil, time.Second); err != nil {
		t.Fatalf("terminateTree(nil) = %v, want nil", err)
	}
	if err := terminateTree(&exec.Cmd{}, time.Second); err != nil {
		t.Fatalf("terminateTree(unstarted command) = %v, want nil", err)
	}
}

func TestSupervisorIsQuietWhenIdle(t *testing.T) {
	s := NewSupervisor()
	if pid := s.PID(); pid != 0 {
		t.Fatalf("an idle supervisor reports pid %d, want 0", pid)
	}
	if err := s.Kill(time.Second); err != nil {
		t.Fatalf("killing an idle supervisor returned %v, want nil; teardown must be safe to "+
			"call on every path", err)
	}
}

// -------------------------------------------------------------------------
// End-to-end supervision, using this test binary as the driver
// -------------------------------------------------------------------------

func TestSupervisorReportsACleanRun(t *testing.T) {
	var stdout bytes.Buffer
	before := time.Now().UnixNano()
	res, err := runHelper(t, Request{
		Cmd:    helperTemplate(t, "exit", "0"),
		Stdout: &stdout,
	})
	after := time.Now().UnixNano()

	if err != nil {
		t.Fatalf("Run returned an error for a clean driver: %v", err)
	}
	if res.Status != StatusOK {
		t.Fatalf("status = %s (%v), want %s", res.Status, res.Err, StatusOK)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0", res.ExitCode)
	}
	if res.Killed {
		t.Fatalf("a driver that exited on its own was recorded as killed by the supervisor")
	}
	if res.TreeKillErr != "" {
		t.Fatalf("TreeKillErr set on a clean run: %q", res.TreeKillErr)
	}
	if len(res.Argv) == 0 {
		t.Fatalf("Result carries no argv; a verdict could only name the template, not what ran")
	}

	// t_ns is Unix epoch NANOSECONDS (D-011, OQ-008). Microseconds would be
	// ~1000x too small and would silently misplace every op against the phase
	// markers the recorder merges by timestamp.
	if res.StartTNS < before || res.EndTNS > after {
		t.Fatalf("timestamps [%d,%d] fall outside the wall-clock window [%d,%d]; they are not "+
			"Unix epoch nanoseconds in the same frame as the history log's t_ns",
			res.StartTNS, res.EndTNS, before, after)
	}
	if res.DurationNS != res.EndTNS-res.StartTNS {
		t.Fatalf("duration %d does not equal end-start (%d)", res.DurationNS, res.EndTNS-res.StartTNS)
	}
}

func TestSupervisorReportsANonZeroExitAsFailedNotKilled(t *testing.T) {
	res, err := runHelper(t, Request{Cmd: helperTemplate(t, "exit", "3")})
	if err == nil {
		t.Fatalf("a driver that exited 3 produced no error; status=%s", res.Status)
	}
	if res.Status != StatusFailed {
		t.Fatalf("status = %s, want %s", res.Status, StatusFailed)
	}
	if res.ExitCode != 3 {
		t.Fatalf("exit code = %d, want 3; the driver's own diagnosis must reach the verdict",
			res.ExitCode)
	}
	if res.Killed {
		t.Fatalf("a driver that chose to exit was recorded as killed by the supervisor; the " +
			"verdict would blame the harness for the driver's failure")
	}
	if res.Signaled {
		t.Fatalf("an ordinary non-zero exit was reported as signalled (%q)", res.Signal)
	}
	if hint := res.ExitCodeHint(); hint != schema.ExitInconclusive {
		t.Fatalf("exit hint = %d, want %d", hint, schema.ExitInconclusive)
	}
}

func TestSupervisorReportsATimeout(t *testing.T) {
	start := time.Now()
	res, err := runHelper(t, Request{
		Cmd:     helperTemplate(t, "sleep", "30000"),
		Timeout: 400 * time.Millisecond,
		Grace:   500 * time.Millisecond,
	})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("a timed-out driver produced no error; status=%s", res.Status)
	}
	if res.Status != StatusTimeout {
		t.Fatalf("status = %s (%v), want %s; collapsing a hang into 'failed' makes a wedged "+
			"workload look like a driver that reported an error", res.Status, res.Err, StatusTimeout)
	}
	if !res.Killed {
		t.Fatalf("a timed-out driver was not recorded as killed by the supervisor")
	}
	if res.TreeKillErr != "" {
		t.Fatalf("killing the tree failed: %q; an orphaned driver keeps its connections open "+
			"and corrupts the NEXT world", res.TreeKillErr)
	}
	// NOTE: res.ExitCode is deliberately not asserted here. Result documents it as
	// "-1 when the process did not exit normally (killed, ...)", but a
	// force-killed process on Windows exits with a real code (measured: 1), so
	// -1 is not portable. The attribution a verdict needs is carried by Status
	// and Killed, both asserted above.
	if elapsed > 20*time.Second {
		t.Fatalf("the supervisor waited %s for a 400ms timeout; it did not enforce the budget",
			elapsed)
	}
}

// A driver that ignores the polite signal must still die. On Unix the
// supervisor sends SIGTERM to the process group, waits out the grace window and
// escalates to SIGKILL; on Windows taskkill /F is unconditional. If the
// escalation were missing, cmd.Wait would never return and this test would hang
// until the whole suite times out, which is exactly what a wedged driver would
// do to a real run.
func TestSupervisorKillsADriverThatIgnoresSIGTERM(t *testing.T) {
	start := time.Now()
	res, err := runHelper(t, Request{
		Cmd:     helperTemplate(t, "ignoreterm"),
		Timeout: 300 * time.Millisecond,
		Grace:   700 * time.Millisecond,
	})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("a driver ignoring SIGTERM produced no error; status=%s", res.Status)
	}
	if res.Status != StatusTimeout {
		t.Fatalf("status = %s (%v), want %s", res.Status, res.Err, StatusTimeout)
	}
	if !res.Killed {
		t.Fatalf("a driver that ignored SIGTERM was not recorded as killed")
	}
	if res.TreeKillErr != "" {
		t.Fatalf("killing the tree reported %q", res.TreeKillErr)
	}
	if elapsed > 25*time.Second {
		t.Fatalf("the supervisor took %s to kill a driver that ignores SIGTERM; it did not "+
			"escalate, and a single wedged driver would stall the whole search", elapsed)
	}
}

func TestSupervisorReportsACancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(300 * time.Millisecond)
		cancel()
	}()
	defer cancel()

	sup := NewSupervisor()
	res, err := sup.Run(ctx, Request{
		Cmd:   helperTemplate(t, "sleep", "30000"),
		Dir:   t.TempDir(),
		Sub:   goodSub(),
		Grace: 500 * time.Millisecond,
	})
	if err == nil {
		t.Fatalf("a cancelled driver produced no error; status=%s", res.Status)
	}
	if res.Status != StatusCanceled {
		t.Fatalf("status = %s (%v), want %s; a Ctrl-C is not a finding about the system under "+
			"test and must not be reported as one", res.Status, res.Err, StatusCanceled)
	}
	if !res.Killed {
		t.Fatalf("a cancelled driver was not recorded as killed by the supervisor")
	}
}

func TestSupervisorReportsAStartFailure(t *testing.T) {
	res, err := runHelper(t, Request{
		Cmd: "./no-such-loadgen-binary --history {history_path} --seed {seed} --profile {profile}",
	})
	if err == nil {
		t.Fatalf("a missing driver binary produced no error; status=%s", res.Status)
	}
	if res.Status != StatusStartError {
		t.Fatalf("status = %s (%v), want %s; a driver that never ran must not be reported as "+
			"one that failed, because no workload was generated at all",
			res.Status, res.Err, StatusStartError)
	}
	if res.ExitCode != -1 {
		t.Fatalf("exit code = %d, want -1; there was no process to exit", res.ExitCode)
	}
	if len(res.Argv) == 0 {
		t.Fatalf("a start failure carries no argv, so the verdict cannot say what it tried to run")
	}
	if res.ExitCodeHint() == schema.ExitFail {
		t.Fatalf("a driver that never started maps to exit 1 (FAIL); no oracle ever ran")
	}
}

// The supervisor must refuse to launch a driver whose argv still contains a
// placeholder, rather than handing it a literal brace string.
func TestSupervisorRefusesToLaunchWithASurvivingPlaceholder(t *testing.T) {
	sub := goodSub()
	sub.Profile = schema.PlaceholderHistoryPath // arrives inside a substituted value

	sup := NewSupervisor()
	res, err := sup.Run(context.Background(), Request{
		Cmd: helperTemplate(t, "exit", "0") + " --profile {profile}",
		Dir: t.TempDir(),
		Sub: sub,
	})
	if err == nil {
		t.Fatalf("a driver was launched with %s still in its argv: %#v",
			schema.PlaceholderHistoryPath, res.Argv)
	}
	if res.Status != StatusStartError {
		t.Fatalf("status = %s, want %s", res.Status, StatusStartError)
	}
	if !strings.Contains(err.Error(), schema.PlaceholderHistoryPath) {
		t.Fatalf("the error does not name the offending placeholder: %v", err)
	}
}

// End to end: template -> SplitCommand -> substitution -> exec -> the driver's
// own os.Args. A path with a space must arrive as ONE argument. This is the
// property that survives this repo's own directory name.
func TestSupervisorPassesASpacedPathAsOneArgument(t *testing.T) {
	spaced := filepath.Join(t.TempDir(), "AI Projects", "Pro synthesis", "history.jsonl")
	sub := goodSub()
	sub.HistoryPath = spaced

	var stdout bytes.Buffer
	res, err := runHelperWithSub(t, Request{
		Cmd:    helperTemplate(t, "echoargs") + " {history_path} --seed {seed}",
		Stdout: &stdout,
	}, sub)
	if err != nil {
		t.Fatalf("Run: %v (status %s)", err, res.Status)
	}
	got := splitLines(stdout.String())
	want := []string{"echoargs", spaced, "--seed", "12345"}
	if len(got) != len(want) {
		t.Fatalf("the driver saw %d arguments, want %d; a path containing a space was split "+
			"across argument boundaries.\n got: %#v\nwant: %#v", len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("argument %d reached the driver as %q, want %q", i, got[i], want[i])
		}
	}
}

// PROTHESIS_PLAN_PATH is ALWAYS exported (D-021), so a driver that does not
// spell {plan_path} in its command line can still find the plan.
func TestSupervisorExportsThePlanPathToTheDriver(t *testing.T) {
	sub := goodSub()
	sub.PlanPath = filepath.Join(t.TempDir(), "w1", "plan.json")

	var stdout bytes.Buffer
	res, err := runHelperWithSub(t, Request{
		// Deliberately no {plan_path} in the command line.
		Cmd:    helperTemplate(t, "echoenv"),
		Stdout: &stdout,
		Env:    append(os.Environ(), schema.PlanPathEnv+"=/stale/plan.json"),
	}, sub)
	if err != nil {
		t.Fatalf("Run: %v (status %s)", err, res.Status)
	}
	got := strings.TrimSpace(stdout.String())
	if got != sub.PlanPath {
		t.Fatalf("the driver read %s = %q, want %q; the environment fallback is the only way a "+
			"driver that does not spell %s can find its plan (D-021)",
			schema.PlanPathEnv, got, sub.PlanPath, schema.PlaceholderPlanPath)
	}
}

// A driver spawns clients. Killing only the direct child leaves an orphaned
// loadgen holding connections open and writing to the history file, which
// corrupts the NEXT world: a failure that surfaces one world after its cause
// and reads as flakiness.
func TestSupervisorKillsTheWholeProcessTree(t *testing.T) {
	dir, err := os.MkdirTemp("", "prothesis-treekill")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	beacon := filepath.Join(dir, "beacon")
	tmpl := helperTemplate(t, "spawn", beacon) // built here: t.Fatalf is not goroutine-safe

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	type outcome struct {
		res *Result
		err error
	}
	done := make(chan outcome, 1)
	sup := NewSupervisor()
	go func() {
		res, err := sup.Run(ctx, Request{
			Cmd:   tmpl,
			Dir:   dir,
			Sub:   goodSub(),
			Grace: 500 * time.Millisecond,
		})
		done <- outcome{res, err}
	}()

	// Wait until the GRANDCHILD is demonstrably alive, so the kill has something
	// to prove. Without this the test could pass vacuously.
	if !waitForSize(beacon, 3, 15*time.Second) {
		cancel()
		<-done
		t.Fatalf("the grandchild never wrote to %s, so the tree kill could not be exercised", beacon)
	}

	cancel()
	got := <-done
	if got.err == nil {
		t.Fatalf("a cancelled driver produced no error; status=%s", got.res.Status)
	}
	if got.res.Status != StatusCanceled {
		t.Fatalf("status = %s (%v), want %s", got.res.Status, got.res.Err, StatusCanceled)
	}
	if got.res.TreeKillErr != "" {
		t.Fatalf("killing the tree reported %q", got.res.TreeKillErr)
	}

	// Give the kill a moment to propagate, then require the grandchild to have
	// stopped writing.
	time.Sleep(400 * time.Millisecond)
	first := fileSize(beacon)
	time.Sleep(900 * time.Millisecond)
	second := fileSize(beacon)
	if second != first {
		t.Fatalf("the grandchild kept writing after the driver was killed (%d -> %d bytes); an "+
			"orphaned loadgen holds its connections open and corrupts the next world",
			first, second)
	}
}

// -------------------------------------------------------------------------
// helpers
// -------------------------------------------------------------------------

func runHelper(t *testing.T, req Request) (*Result, error) {
	t.Helper()
	return runHelperWithSub(t, req, goodSub())
}

func runHelperWithSub(t *testing.T, req Request, sub Substitution) (*Result, error) {
	t.Helper()
	req.Sub = sub
	if req.Dir == "" {
		req.Dir = t.TempDir()
	}
	return NewSupervisor().Run(context.Background(), req)
}

// helperTemplate builds a driver.cmd that re-executes this test binary as the
// workload generator. The program is single-quoted because the build machine's
// temporary directory, like its project directory, may contain a space.
func helperTemplate(t *testing.T, args ...string) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	if strings.ContainsAny(exe, "'") {
		t.Fatalf("the test binary path contains a single quote and cannot be quoted: %q", exe)
	}
	var b strings.Builder
	b.WriteString("'" + exe + "' -test.run=^TestHelperProcess$ --")
	for _, a := range args {
		b.WriteString(" '" + a + "'")
	}
	return b.String()
}

// mustExitError returns a real *exec.ExitError for the given exit code.
func mustExitError(t *testing.T, code int) error {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	cmd := exec.Command(exe, "-test.run=^TestHelperProcess$", "--", "exit", strconv.Itoa(code))
	runErr := cmd.Run()
	var ee *exec.ExitError
	if !errors.As(runErr, &ee) {
		t.Fatalf("expected an *exec.ExitError from a driver exiting %d, got %v", code, runErr)
	}
	return runErr
}

func splitLines(s string) []string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.TrimSuffix(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

func fileSize(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return -1
	}
	return fi.Size()
}

func waitForSize(path string, want int64, limit time.Duration) bool {
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if fileSize(path) >= want {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

// TestHelperProcess is not a test. It is the workload generator the supervision
// tests drive, re-entered through this same binary so the suite needs no build
// step, no shell, and nothing installed on the host. It does nothing unless the
// command line carries the "--" separator this package's helpers add.
func TestHelperProcess(t *testing.T) {
	args := helperArgs()
	if len(args) == 0 {
		return
	}
	needsParam := map[string]bool{"exit": true, "sleep": true, "spawn": true, "beacon": true}
	if needsParam[args[0]] && len(args) < 2 {
		fmt.Fprintln(os.Stderr, "helper: mode", args[0], "needs a parameter")
		os.Exit(94)
	}
	switch args[0] {
	case "exit":
		code, err := strconv.Atoi(args[1])
		if err != nil {
			fmt.Fprintln(os.Stderr, "helper: bad exit code:", args[1])
			os.Exit(99)
		}
		os.Exit(code)

	case "sleep":
		ms, err := strconv.Atoi(args[1])
		if err != nil {
			os.Exit(99)
		}
		time.Sleep(time.Duration(ms) * time.Millisecond)
		os.Exit(0)

	case "ignoreterm":
		// Swallow the polite signal so the supervisor must escalate.
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGTERM, os.Interrupt)
		time.Sleep(30 * time.Second)
		os.Exit(0)

	case "echoargs":
		for _, a := range args {
			fmt.Println(a)
		}
		os.Exit(0)

	case "echoenv":
		fmt.Println(os.Getenv(schema.PlanPathEnv))
		os.Exit(0)

	case "spawn":
		exe, err := os.Executable()
		if err != nil {
			os.Exit(98)
		}
		child := exec.Command(exe, "-test.run=^TestHelperProcess$", "--", "beacon", args[1])
		if err := child.Start(); err != nil {
			os.Exit(97)
		}
		time.Sleep(30 * time.Second)
		os.Exit(0)

	case "beacon":
		f, err := os.OpenFile(args[1], os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			os.Exit(96)
		}
		// Self-limiting: if the tree kill ever fails, this still goes away on its
		// own rather than leaving a process behind on the build machine.
		for i := 0; i < 100; i++ {
			if _, err := f.Write([]byte("x")); err != nil {
				break
			}
			_ = f.Sync()
			time.Sleep(100 * time.Millisecond)
		}
		_ = f.Close()
		os.Exit(0)

	// ---- drain modes (drain_test.go) -------------------------------------
	//
	// These three model the whole space a drain has to tell apart: a driver
	// that honours the control channel, one that ignores it, and one that
	// reports what it was told about the channel.

	case "drainable":
		// A cooperating driver: it works until stdin says stop or reaches EOF,
		// then finishes the operation it is holding and exits cleanly. The
		// 30-second ceiling is a safety net, not the mechanism: a test that
		// passed because of it would be measuring the ceiling.
		done := make(chan struct{})
		go func() {
			defer close(done)
			sc := bufio.NewScanner(os.Stdin)
			for sc.Scan() {
				if strings.Contains(sc.Text(), `"stop"`) {
					return
				}
			}
		}()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			os.Exit(93)
		}
		// The "in-flight operation" this drain exists to let finish.
		time.Sleep(50 * time.Millisecond)
		fmt.Println("drained")
		os.Exit(0)

	case "ignorestdin":
		// A driver that never reads stdin. Nothing on the harness side can tell
		// this from one that is slowly finishing up, which is exactly why the
		// drain needs a deadline rather than a promise.
		time.Sleep(30 * time.Second)
		os.Exit(0)

	case "echostdincontrol":
		fmt.Println(os.Getenv(StdinControlEnv))
		os.Exit(0)
	}

	fmt.Fprintln(os.Stderr, "helper: unknown mode:", args[0])
	os.Exit(95)
}

func helperArgs() []string {
	for i, a := range os.Args {
		if a == "--" {
			return os.Args[i+1:]
		}
	}
	return nil
}
