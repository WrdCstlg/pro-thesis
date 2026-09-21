package oracle

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/WrdCstlg/pro-thesis/internal/driver"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// ---------------------------------------------------------------------------
// Limits
// ---------------------------------------------------------------------------

const (
	// MaxOracleStdoutBytes bounds the prothesis.oracle_output/v1 document.
	//
	// The contract channel has to be bounded or a defective oracle emitting an
	// unbounded stream takes the harness's memory with it, and the harness is
	// the process holding the run's only record of what happened. One mebibyte
	// is three orders of magnitude more than a witness needs; an oracle that
	// exceeds it is reporting a bug in itself, so the finding is INCONCLUSIVE
	// and its process tree is killed at the moment the cap is crossed rather
	// than after it has finished writing.
	MaxOracleStdoutBytes int64 = 1 << 20

	// MaxOracleStderrBytes bounds the stderr captured into the run bundle.
	//
	// Larger than stdout because stderr is diagnostics, and a checker that
	// exhausts its budget usefully explains itself at length. Overflow is
	// recorded in the capture file rather than killing the oracle: stderr is not
	// the contract channel, and killing an oracle for being talkative would turn
	// a debuggable run into an inconclusive one.
	MaxOracleStderrBytes int64 = 4 << 20

	// DefaultOracleGrace is how long a killed oracle may take to exit before its
	// tree is force-killed. It matches driver.DefaultGrace.
	DefaultOracleGrace = 2 * time.Second

	// OracleStderrDirName is the subdirectory of a world's artifact directory
	// that external oracle stderr captures are written into.
	OracleStderrDirName = "oracles"

	// stderrExcerptBytes is how much stderr is quoted into an explanation. The
	// full capture is on disk; the explanation is read by humans and by agents
	// with a context budget.
	stderrExcerptBytes = 480
)

// ---------------------------------------------------------------------------
// ADDITIVE witness members an external oracle may use to place its finding
// ---------------------------------------------------------------------------
//
// prothesis.oracle_output/v1 carries `status`, `witness` and `explanation` and
// no timing at all, while prothesis.verdict/v1 requires every violation to
// carry `phase` and `first_seen_ms`. Something has to bridge that, and the
// witness is the only free-form member of the frozen envelope: the directive's
// own example shows `{"op_ids": [...], "key": "..."}` and says a witness is
// authored by the oracle.
//
// So these two members are read, OPTIONALLY, from the witness. An oracle that
// omits them loses nothing except precision on the timeline. See
// OPEN_QUESTIONS.md OQ-030.
const (
	// WitnessFirstSeenMS is the millisecond offset, relative to DRIVE start, of
	// the earliest evidence for the finding. The engine derives the OBSERVED
	// phase from it against the measured phase windows.
	WitnessFirstSeenMS = "first_seen_ms"

	// WitnessPhase pins the OBSERVED phase directly, for evidence that carries
	// no usable timestamp.
	//
	// This is violations[].phase, NOT oracle.valid_phases. It is legitimately
	// outside valid_phases and usually is: the fixture's stale read is found by
	// an ASSERT-only consistency oracle and is observed in DRIVE (D-031). Those
	// two fields are not the same thing and the engine never checks one against
	// the other (OQ-004).
	WitnessPhase = "phase"

	// WitnessStderrPath names the captured stderr file, added by the runner so a
	// failing oracle is one open() away from being debuggable.
	WitnessStderrPath = "stderr_path"
)

// ---------------------------------------------------------------------------
// ProcessOracle
// ---------------------------------------------------------------------------

// ProcessOracle is an external oracle: a separate executable honouring the
// stdin/stdout JSON contract of directive 4.5.
//
// It implements Oracle, so the engine, the verdict assembly, the exit-code
// mapping and result.json treat it exactly as they treat a built-in. Its class
// and valid phases come from the LOCK-COVERED definition, never from the
// oracle's own output.
type ProcessOracle struct {
	def        Definition
	projectDir string
	env        []string
	maxStdout  int64
	maxStderr  int64
	grace      time.Duration
}

// NewProcessOracle constructs an external oracle from a validated definition.
func NewProcessOracle(def Definition, opts ExternalOptions) (*ProcessOracle, error) {
	if err := def.Validate(); err != nil {
		return nil, fmt.Errorf("oracle: %s: %w", def.Source(), err)
	}
	p := &ProcessOracle{
		def:        def,
		projectDir: opts.ProjectDir,
		env:        opts.Env,
		maxStdout:  opts.MaxStdoutBytes,
		maxStderr:  opts.MaxStderrBytes,
		grace:      opts.Grace,
	}
	if p.maxStdout <= 0 {
		p.maxStdout = MaxOracleStdoutBytes
	}
	if p.maxStderr <= 0 {
		p.maxStderr = MaxOracleStderrBytes
	}
	if p.grace <= 0 {
		p.grace = DefaultOracleGrace
	}
	return p, nil
}

// Definition returns the declaration this oracle was built from.
func (p *ProcessOracle) Definition() Definition { return p.def }

// Name is the oracle's identity, from the definition.
func (p *ProcessOracle) Name() string { return p.def.Name }

// Class is the definition's class. It is never read from the oracle's output:
// severity is derived from it, so an oracle choosing its own class could grade
// its own finding down.
func (p *ProcessOracle) Class() schema.OracleClass { return p.def.Class }

// ValidPhases is the definition's invariant I5 declaration. The ENGINE filters
// on it; this oracle is never asked to evaluate outside it.
func (p *ProcessOracle) ValidPhases() []schema.Phase {
	return append([]schema.Phase(nil), p.def.ValidPhases...)
}

// Evaluate runs the oracle process and interprets its two result channels.
//
// It returns (Result, nil) in every ordinary case, including every failure of
// the oracle itself: a crash, a hang, an unparseable document and a
// disagreement between exit code and reported status are all FINDINGS of
// inconclusive, not engine errors, because the run is still perfectly capable
// of reporting what the other oracles concluded.
func (p *ProcessOracle) Evaluate(ctx context.Context, phase schema.Phase, in *Input) (Result, error) {
	if in == nil {
		return Inconclusive("oracle %q was handed no evidence at all (a nil input), so it was not run",
			p.def.Name).inPhase(phase), nil
	}

	doc := in.OracleInput()
	if err := doc.Validate(); err != nil {
		// The oracle_input document is a PROMISE to a third party. Handing over
		// one that does not satisfy its own schema would have the oracle fail at
		// a place that points nowhere near the cause.
		return Inconclusive("the %s document for oracle %q is not valid, so it was not run: %v",
			schema.OracleInputSchema, p.def.Name, err).inPhase(phase), nil
	}
	stdin, err := schema.MarshalOracleInput(&doc)
	if err != nil {
		return Inconclusive("the %s document for oracle %q could not be encoded, so it was not run: %v",
			schema.OracleInputSchema, p.def.Name, err).inPhase(phase), nil
	}

	run := p.exec(ctx, stdin, p.stderrPath(in))
	return p.interpret(run, phase), nil
}

// stderrPath is where this oracle's stderr is captured for the run bundle.
// An input with no artifact directory captures in memory only.
func (p *ProcessOracle) stderrPath(in *Input) string {
	if in == nil || in.ArtifactDir == "" {
		return ""
	}
	return filepath.Join(in.ArtifactDir, OracleStderrDirName, safeFileName(p.def.Name)+".stderr.log")
}

// safeFileName reduces an oracle name to something openable on every platform.
// Oracle names carry dots (`linearizable.kv`) and could carry worse.
func safeFileName(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := b.String()
	out = strings.TrimLeft(out, ".")
	if out == "" {
		out = "oracle"
	}
	if len(out) > 60 {
		out = out[:60]
	}
	return out
}

// ---------------------------------------------------------------------------
// execution
// ---------------------------------------------------------------------------

// procRun is everything observed about one oracle process.
type procRun struct {
	Argv     []string
	Started  bool
	StartErr error

	ExitCode int
	// ExitText is the runtime's own rendering of an abnormal exit
	// ("signal: segmentation fault"), which is the only portable way to name a
	// signal without a build-tagged file.
	ExitText string
	// Crashed reports a process that did not exit through a status code: on
	// POSIX, one killed by a signal.
	Crashed bool

	TimedOut bool
	Canceled bool
	Killed   bool
	KillErr  string
	// MainExited reports that the oracle's own process is confirmed gone. It
	// qualifies KillErr: a kill that "failed" because the process had already
	// exited is a race, not a leak, and reporting the two identically would
	// cry wolf on the one message that must be believed.
	MainExited bool

	Stdout         []byte
	StdoutOverflow bool

	StderrExcerpt  string
	StderrBytes    int64
	StderrOverflow bool
	StderrPath     string
	StderrErr      string

	Duration time.Duration
}

// exec runs the oracle process to completion, to its timeout, or to
// cancellation, and never leaves anything behind.
func (p *ProcessOracle) exec(ctx context.Context, stdin []byte, stderrPath string) procRun {
	r := procRun{ExitCode: -1, StderrPath: stderrPath}

	argv, err := driver.SplitCommand(p.def.Cmd)
	if err != nil {
		r.StartErr = fmt.Errorf("cmd: %w", err)
		return r
	}
	if len(argv) == 0 || argv[0] == "" {
		r.StartErr = errors.New("cmd: has no program name")
		return r
	}
	r.Argv = argv

	runCtx, cancel := context.WithTimeout(ctx, p.def.Timeout.Std())
	defer cancel()

	cmd := exec.Command(driver.ResolveProgram(argv[0], p.projectDir), argv[1:]...)
	cmd.Dir = p.projectDir
	cmd.Env = p.env
	cmd.Stdin = bytes.NewReader(stdin)
	// The process GROUP is set before Start so the whole tree can be addressed.
	// An oracle that shells out and is killed by pid alone leaves the child
	// holding the pipe open, and this function would then block until its own
	// timeout on a process that is already dead.
	driver.SetProcessGroup(cmd)

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		r.StartErr = fmt.Errorf("stdout pipe: %w", err)
		return r
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		r.StartErr = fmt.Errorf("stderr pipe: %w", err)
		return r
	}

	start := time.Now()
	if err := cmd.Start(); err != nil {
		r.Duration = time.Since(start)
		r.StartErr = fmt.Errorf("start %s: %w", argv[0], err)
		return r
	}
	r.Started = true

	var (
		killMu   sync.Mutex
		killOnce sync.Once
		killErr  string
	)
	kill := func() {
		killOnce.Do(func() {
			if err := driver.TerminateTree(cmd, p.grace); err != nil {
				killMu.Lock()
				killErr = err.Error()
				killMu.Unlock()
			}
		})
	}

	var (
		wg      sync.WaitGroup
		outBuf  []byte
		outOver bool
		errCap  stderrCapture
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		// Killing AT the cap, rather than after the oracle finishes writing, is
		// what makes the bound a memory bound rather than a reporting one.
		outBuf, outOver = readCapped(stdoutPipe, p.maxStdout, kill)
	}()
	go func() {
		defer wg.Done()
		errCap = captureStderr(stderrPipe, stderrPath, p.maxStderr)
	}()

	// StdoutPipe's contract: Wait closes the pipes, so every read must finish
	// first. Waiting on the readers before Wait is not an optimisation.
	waitDone := make(chan error, 1)
	go func() {
		wg.Wait()
		waitDone <- cmd.Wait()
	}()

	var waitErr error
	select {
	case waitErr = <-waitDone:
	case <-runCtx.Done():
		r.Killed = true
		kill()
		waitErr = <-waitDone
		switch {
		case ctx.Err() != nil:
			r.Canceled = true
		case errors.Is(runCtx.Err(), context.DeadlineExceeded):
			r.TimedOut = true
		default:
			r.Canceled = true
		}
	}
	r.Duration = time.Since(start)

	killMu.Lock()
	r.KillErr = killErr
	killMu.Unlock()
	r.MainExited = cmd.ProcessState != nil && cmd.ProcessState.Exited()

	r.Stdout, r.StdoutOverflow = outBuf, outOver
	r.StderrExcerpt = errCap.Excerpt
	r.StderrBytes = errCap.Bytes
	r.StderrOverflow = errCap.Overflow
	r.StderrErr = errCap.Err
	if errCap.Path == "" {
		r.StderrPath = ""
	}

	switch {
	case waitErr == nil:
		r.ExitCode = 0
	default:
		var ee *exec.ExitError
		if errors.As(waitErr, &ee) {
			r.ExitCode = ee.ExitCode()
			r.ExitText = ee.String()
			// ExitCode is -1 exactly when the process did not exit through a
			// status code, which on POSIX means a signal killed it.
			r.Crashed = r.ExitCode < 0
		} else {
			r.ExitCode = -1
			r.ExitText = waitErr.Error()
			r.Crashed = true
		}
	}
	return r
}

// readCapped reads at most max bytes, calls onOverflow the instant the cap is
// crossed, and then DRAINS the reader.
//
// Draining is not tidiness. A reader that stops reading leaves the child
// blocked on a full pipe, and a blocked child cannot be waited on: the run
// would then sit until its own timeout on a process that has already said
// everything it has to say.
func readCapped(r io.Reader, max int64, onOverflow func()) ([]byte, bool) {
	if max <= 0 {
		max = MaxOracleStdoutBytes
	}
	// A read error simply ends the capture; the exit code and the absence of a
	// parseable document carry the verdict from there.
	b, _ := io.ReadAll(io.LimitReader(r, max+1))
	if int64(len(b)) > max {
		b = b[:max]
		if onOverflow != nil {
			onOverflow()
		}
		_, _ = io.Copy(io.Discard, r)
		return b, true
	}
	return b, false
}

// stderrCapture is one oracle's captured diagnostics.
type stderrCapture struct {
	// Path is the file written, or "" when nothing was captured to disk.
	Path string
	// Excerpt is the leading stderrExcerptBytes, for the explanation.
	Excerpt string
	// Bytes is how much was captured.
	Bytes int64
	// Overflow reports that the oracle wrote more than the cap.
	Overflow bool
	// Err records a capture failure. A stderr capture that fails must never
	// fail the evaluation: it makes a finding harder to debug, not wrong.
	Err string
}

// captureStderr streams an oracle's stderr into the run bundle.
func captureStderr(r io.Reader, path string, max int64) stderrCapture {
	var c stderrCapture
	if max <= 0 {
		max = MaxOracleStderrBytes
	}

	var f *os.File
	if path != "" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			c.Err = err.Error()
		} else if handle, err := os.Create(path); err != nil {
			c.Err = err.Error()
		} else {
			f = handle
			c.Path = path
			defer func() { _ = f.Close() }()
		}
	}

	var excerpt bytes.Buffer
	buf := make([]byte, 32*1024)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			if c.Bytes < max {
				room := max - c.Bytes
				if int64(len(chunk)) > room {
					chunk = chunk[:room]
					c.Overflow = true
				}
				c.Bytes += int64(len(chunk))
				if f != nil {
					if _, werr := f.Write(chunk); werr != nil && c.Err == "" {
						c.Err = werr.Error()
					}
				}
				if excerpt.Len() < stderrExcerptBytes {
					take := stderrExcerptBytes - excerpt.Len()
					if take > len(chunk) {
						take = len(chunk)
					}
					excerpt.Write(chunk[:take])
				}
			} else {
				c.Overflow = true
			}
		}
		if err != nil {
			break
		}
	}
	if c.Overflow && f != nil {
		_, _ = fmt.Fprintf(f, "\n[prothesis: stderr truncated at %d bytes]\n", max)
	}
	c.Excerpt = strings.ToValidUTF8(excerpt.String(), "")
	return c
}

// ---------------------------------------------------------------------------
// interpretation
// ---------------------------------------------------------------------------

// interpret turns a completed process run into a Result.
//
// THE ONE RULE THIS FUNCTION EXISTS TO ENFORCE: it returns ok only when the
// oracle's exit code AND its reported status BOTH say ok, and it returns
// violated only when both say violated. Every other combination, and every
// failure of the process itself, is INCONCLUSIVE with the reason attached.
//
// That is stricter than schema.DecodeOracleOutput, which reconciles a
// disagreement by taking the worse of the two readings. The deliberate
// difference is Phase 3's ranking of failure modes: taking the worse reading of
// (stdout says ok, exit 1) manufactures a `violated`; a FALSE POSITIVE, and a
// false positive destroys the product, while an inconclusive is exit 2 and is
// already handled by the agent-loop contract as "retry once, then escalate".
// Neither reading lets a disagreeing oracle pass, so this is strictly the safer
// of two non-passing answers. See DECISIONS.md D-038.
func (p *ProcessOracle) interpret(run procRun, phase schema.Phase) Result {
	fail := func(format string, a ...any) Result {
		res := Inconclusive(format, a...).inPhase(phase)
		return p.decorate(res, run)
	}

	switch {
	case run.StartErr != nil:
		return fail("oracle %q could not be started (%s): %v; a definition that names an "+
			"executable this machine does not have has checked nothing",
			p.def.Name, p.def.Source(), run.StartErr)

	case run.Canceled:
		return fail("oracle %q was cancelled after %s and its process tree was killed",
			p.def.Name, run.Duration.Round(time.Millisecond))

	case run.TimedOut:
		return fail("oracle %q exceeded its %s timeout after %s and its process tree was killed; "+
			"an oracle that did not finish has not checked anything, so this is inconclusive "+
			"and never ok", p.def.Name, p.def.Timeout, run.Duration.Round(time.Millisecond))

	case run.StdoutOverflow:
		return fail("oracle %q wrote more than %d bytes to stdout and was killed; the %s "+
			"document is bounded so that a defective oracle cannot exhaust the harness's memory",
			p.def.Name, p.maxStdout, schema.OracleOutputSchema)

	case run.Crashed:
		return fail("oracle %q crashed without an exit status after %s (%s)",
			p.def.Name, run.Duration.Round(time.Millisecond), nonEmpty(run.ExitText, "no detail"))
	}

	exitCode := schema.OracleExitCode(run.ExitCode)
	if !exitCode.Valid() {
		return fail("oracle %q exited %d, which is outside the 0/1/2 contract of %s",
			p.def.Name, run.ExitCode, schema.OracleOutputSchema)
	}

	out, err := schema.ParseOracleOutput(run.Stdout)
	if err != nil {
		if len(bytes.TrimSpace(run.Stdout)) == 0 {
			return fail("oracle %q wrote nothing to stdout (it exited %d); a %s document is the "+
				"only thing that makes an exit code a finding",
				p.def.Name, run.ExitCode, schema.OracleOutputSchema)
		}
		return fail("oracle %q wrote stdout that is not a %s document (it exited %d): %v; first "+
			"bytes: %s", p.def.Name, schema.OracleOutputSchema, run.ExitCode, err,
			quoteExcerpt(run.Stdout, 200))
	}

	if out.Status == "" {
		return fail("oracle %q wrote a %s document with no status (it exited %d)",
			p.def.Name, schema.OracleOutputSchema, run.ExitCode)
	}
	if !out.Status.Valid() {
		return fail("oracle %q reported status %q, which is not one of %v",
			p.def.Name, out.Status, schema.AllOracleStatuses)
	}

	// The two channels must AGREE. Never trust one over the other silently.
	fromExit := exitCode.Status()
	if out.Status != fromExit {
		return fail("oracle %q disagrees with itself: its %s says status %q while it exited %d, "+
			"which the contract defines as %q. Exit code and reported status must agree; a "+
			"disagreement is a defect in the oracle, so neither reading is adopted",
			p.def.Name, schema.OracleOutputSchema, out.Status, run.ExitCode, fromExit)
	}

	// The oracle's own declaration must match the lock-covered definition.
	//
	// This is I5 enforcement, and it is a check on the DECLARATION, not on where
	// the evidence lies. An oracle whose executable believes it is valid in
	// DRIVE while its definition says ASSERT is either the wrong binary or a
	// binary that has drifted from its reviewed declaration, and a finding from
	// either is untrustworthy in both directions, so an `ok` is refused here
	// exactly as a `violated` is.
	if err := p.checkDeclaration(out, phase); err != nil {
		return fail("%s", err.Error())
	}

	res := Result{
		Status:      out.Status,
		Witness:     out.Witness,
		Explanation: out.Explanation,
	}

	// Place the finding on the timeline from the ADDITIVE witness members, when
	// the oracle supplied them.
	if ms, ok, err := witnessInt(out.Witness, WitnessFirstSeenMS); err != nil {
		return fail("oracle %q wrote witness.%s that is not an integer: %v",
			p.def.Name, WitnessFirstSeenMS, err)
	} else if ok {
		res.FirstSeenMS = ms
	} else if out.Status != schema.StatusOK {
		// With no timestamp, FirstSeenMS stays 0, and 0 is DRIVE start: a
		// FABRICATED position on the timeline, which is worse than none because
		// it is the field a human reads first. Pin the phase instead, so the
		// engine does not derive one from a number the oracle never gave.
		res = res.inPhase(phase)
	}

	if s, ok, err := witnessString(out.Witness, WitnessPhase); err != nil {
		return fail("oracle %q wrote witness.%s that is not a string: %v",
			p.def.Name, WitnessPhase, err)
	} else if ok {
		observed, perr := schema.ParsePhase(s)
		if perr != nil {
			return fail("oracle %q wrote witness.%s = %q, which is not one of the eight lifecycle "+
				"phases %v", p.def.Name, WitnessPhase, s, schema.AllPhases)
		}
		// Deliberately NOT checked against valid_phases. The observed phase and
		// the declared valid phases are different fields with different meanings
		// (OQ-004): the fixture's stale read is found by an ASSERT-only
		// consistency oracle and is observed in DRIVE.
		res = res.inPhase(observed)
	}

	return p.decorate(res, run)
}

// checkDeclaration compares the oracle's self-declaration against the
// lock-covered definition.
//
// ABSENT IS NOT A DISAGREEMENT. A minimal oracle that emits only `schema`,
// `status` and `explanation` is a perfectly good oracle, and rejecting it would
// punish the simplest correct implementation. Only a declaration that is
// present AND contradicts the definition is a defect.
func (p *ProcessOracle) checkDeclaration(out *schema.OracleOutput, phase schema.Phase) error {
	if out.Oracle != "" && out.Oracle != p.def.Name {
		return fmt.Errorf("oracle %q identifies itself as %q; %s declares %q, and the name is the "+
			"identity the verdict and .prothesis/lock address, so this is either the wrong "+
			"executable or a drifted one",
			p.def.Name, out.Oracle, p.def.Source(), p.def.Name)
	}
	if out.Class != "" && out.Class != p.def.Class {
		return fmt.Errorf("oracle %q reports class %q while %s declares %q; severity is derived "+
			"from the declared class, so the two must not diverge",
			p.def.Name, out.Class, p.def.Source(), p.def.Class)
	}
	if len(out.ValidPhases) == 0 {
		return nil
	}
	if !phaseSetsEqual(out.ValidPhases, p.def.ValidPhases) {
		return fmt.Errorf("oracle %q declares valid_phases %s in its own output while %s declares "+
			"%s, and it was evaluated in %s; the engine, not the oracle, decides where an oracle "+
			"runs (invariant I5), so a declaration the executable disagrees with makes every "+
			"finding it reports untrustworthy",
			p.def.Name, PhaseList(out.ValidPhases), p.def.Source(),
			PhaseList(p.def.ValidPhases), phase)
	}
	// Belt and braces: the engine has already established this, but an oracle
	// that agrees with the definition and still says it is invalid here would
	// mean the two lists agreed on something that excludes the evaluation phase.
	if !out.ValidIn(phase) {
		return fmt.Errorf("oracle %q was evaluated in %s but declares %s; the engine must not have "+
			"asked it, and a finding produced outside an oracle's declared phases is an oracle "+
			"defect, not a violation", p.def.Name, phase, PhaseList(out.ValidPhases))
	}
	return nil
}

// decorate attaches the debugging trail to a result: where the stderr capture
// went, and an excerpt of it when something went wrong.
//
// It never changes a status. Its whole job is to make sure that "this oracle
// failed" is one open() away from "this is why".
func (p *ProcessOracle) decorate(res Result, run procRun) Result {
	if res.Status == schema.StatusOK {
		return res
	}

	extra := map[string]json.RawMessage{}
	for k, v := range res.Witness.Extra {
		extra[k] = v
	}
	set := func(name string, v any) {
		raw, err := encodeJSON(v)
		if err != nil {
			return
		}
		extra[name] = raw
	}

	if run.StderrPath != "" {
		set(WitnessStderrPath, run.StderrPath)
	}
	if len(run.Argv) > 0 {
		set("oracle_argv", run.Argv)
	}
	set("oracle_definition", p.def.Source())

	notes := make([]string, 0, 4)
	if run.StderrExcerpt != "" {
		cut, truncated := truncateText(run.StderrExcerpt, stderrExcerptBytes)
		note := "oracle stderr: " + strings.TrimRight(cut, "\r\n")
		if truncated || run.StderrOverflow {
			note += " [...]"
		}
		notes = append(notes, note)
	}
	if run.StderrPath != "" && run.StderrBytes > 0 {
		notes = append(notes, fmt.Sprintf("full stderr (%d bytes) captured at %s",
			run.StderrBytes, run.StderrPath))
	}
	if run.StderrErr != "" {
		notes = append(notes, "stderr could not be captured: "+run.StderrErr)
	}
	if run.KillErr != "" {
		if run.MainExited {
			// The oracle exited between the deadline firing and the kill
			// landing. Not a leak of the oracle itself; a child it spawned is
			// still unaccounted for, so this is reported rather than dropped.
			notes = append(notes, "the oracle exited before the kill landed; terminating its "+
				"process tree reported: "+run.KillErr)
		} else {
			// Serious enough to say out loud: a surviving oracle process holds
			// whatever it opened and can corrupt the next world.
			notes = append(notes, "killing the oracle's process tree FAILED: "+run.KillErr)
		}
	}

	for _, n := range notes {
		res.Explanation = joinExplanation(res.Explanation, n)
	}
	if len(extra) > 0 {
		res.Witness.Extra = extra
	}
	return res
}

// ---------------------------------------------------------------------------
// small helpers
// ---------------------------------------------------------------------------

// witnessInt reads an integer witness member. The third return distinguishes
// "absent" from "present and wrong", which must not be collapsed: absent is
// fine, wrong is a malformed document.
func witnessInt(w schema.Witness, name string) (int64, bool, error) {
	raw, ok := w.Extra[name]
	if !ok {
		return 0, false, nil
	}
	var v int64
	if err := json.Unmarshal(raw, &v); err != nil {
		return 0, false, err
	}
	return v, true, nil
}

func witnessString(w schema.Witness, name string) (string, bool, error) {
	raw, ok := w.Extra[name]
	if !ok {
		return "", false, nil
	}
	var v string
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", false, err
	}
	return v, true, nil
}

// phaseSetsEqual compares two phase declarations as SETS. Order is not part of
// the declaration, and rejecting [ASSERT QUIESCE] against [QUIESCE ASSERT]
// would be a false-inconclusive generator.
func phaseSetsEqual(a, b []schema.Phase) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[schema.Phase]int, len(a))
	for _, p := range a {
		seen[p]++
	}
	for _, p := range b {
		seen[p]--
		if seen[p] < 0 {
			return false
		}
	}
	for _, n := range seen {
		if n != 0 {
			return false
		}
	}
	return true
}

func nonEmpty(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}

// quoteExcerpt renders the head of an oracle's stdout for a diagnostic.
func quoteExcerpt(b []byte, n int) string {
	s := strings.ToValidUTF8(string(b), "")
	s = strings.TrimSpace(s)
	cut, truncated := truncateText(s, n)
	cut = strings.ReplaceAll(cut, "\n", "\\n")
	if truncated {
		return fmt.Sprintf("%q...", cut)
	}
	return fmt.Sprintf("%q", cut)
}
