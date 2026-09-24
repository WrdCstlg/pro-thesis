package control

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/WrdCstlg/pro-thesis/internal/probehost"
	"github.com/WrdCstlg/pro-thesis/internal/recorder"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// Environment variables exported into the steady-state probe's process.
//
// ADDITIVE, and forced by a real gap. The frozen config gives
// `steady_state.probe: "thesis-helpers/steady.sh"` with NO placeholder
// vocabulary, and the reference probe for the compose backend is a
// `docker compose exec` invocation (DECISIONS.md D-010). But `thesis up`
// creates its project under a derived name (`thesis-<name>-<hash>`) so a bare
// `docker compose exec` run from the project directory resolves the DIRECTORY's
// default project instead and fails against a topology that is right there.
//
// Compose reads COMPOSE_PROJECT_NAME and COMPOSE_FILE from the environment, so
// exporting them makes the directive's own probe form work verbatim, with no
// change to the frozen config surface and no new placeholder. The PROTHESIS_*
// variables are for probes that want to address the topology directly.
// Logged as OQ-064 (promised as "OQ-021" in Phase 1 and never written; see
// D-067).
const (
	EnvComposeProject = "COMPOSE_PROJECT_NAME"
	EnvComposeFile    = "COMPOSE_FILE"
	EnvComposePathSep = "COMPOSE_PATH_SEPARATOR"

	EnvRunID          = "PROTHESIS_RUN_ID"
	EnvProjectDir     = "PROTHESIS_PROJECT_DIR"
	EnvBackend        = "PROTHESIS_BACKEND"
	EnvNodes          = "PROTHESIS_NODES"
	EnvHost           = "PROTHESIS_HOST"
	EnvNodePortPrefix = "PROTHESIS_PORT_"
)

// ErrNoSteadyStateProbe is returned when a probe was requested but none is
// declared.
var ErrNoSteadyStateProbe = errors.New("control: no harness.steady_state.probe is declared")

// ExecSteadyState is the default SteadyStateProber: it EXECs the probe
// directly, without a shell.
//
// No shell, for a measured reason: this host's Git Bash is non-functional
// (PHASE0_BUILD_BRIEF section 0), and routing a probe through cmd.exe would give
// the probe string two incompatible quoting dialects depending on the platform.
// Direct exec has one.
//
// The probe's stdout and stderr are captured and folded into the error, because
// a steady-state failure that reports only an exit status is undiagnosable,
// and it is the failure most likely to be a genuine bug in the system under
// test rather than in PRO-THESIS.
type ExecSteadyState struct{}

// NewExecSteadyState returns the default prober.
func NewExecSteadyState() *ExecSteadyState { return &ExecSteadyState{} }

// Probe implements SteadyStateProber.
func (p *ExecSteadyState) Probe(ctx context.Context, req SteadyStateRequest) error {
	probe := strings.TrimSpace(req.Probe)
	if probe == "" {
		return ErrNoSteadyStateProbe
	}
	argv, err := SplitCommand(probe)
	if err != nil {
		return fmt.Errorf("control: steady_state.probe: %w", err)
	}
	if len(argv) == 0 {
		return ErrNoSteadyStateProbe
	}

	timeout := req.Timeout
	if timeout <= 0 {
		timeout = schema.DefaultSteadyTimeout
	}
	// The probe's own timeout is a config value; the deadline here is a hard
	// backstop. A probe that ignores SIGTERM must not be able to hold the run
	// open past its declared window.
	pctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(pctx, argv[0], argv[1:]...)
	cmd.Dir = req.ProjectDir
	cmd.Env = append(os.Environ(), SteadyStateEnv(req)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	start := time.Now()
	runErr := cmd.Run()
	if runErr == nil {
		return nil
	}
	detail := strings.TrimSpace(stderr.String())
	if detail == "" {
		detail = strings.TrimSpace(stdout.String())
	}
	if errors.Is(pctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("control: steady state not reached within %s (%s): %s",
			timeout, time.Since(start).Round(time.Millisecond), detail)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return fmt.Errorf("control: steady_state.probe %q failed: %w: %s", probe, runErr, detail)
}

// SteadyStateEnv builds the additive environment described on EnvComposeProject.
func SteadyStateEnv(req SteadyStateRequest) []string {
	env := []string{}
	if req.ProjectDir != "" {
		env = append(env, EnvProjectDir+"="+req.ProjectDir)
	}
	env = append(env, EnvHost+"="+ProbeHost())
	top := req.Topology
	if top == nil {
		return env
	}
	env = append(env,
		EnvRunID+"="+top.RunID,
		EnvBackend+"="+top.Backend,
	)
	if top.ComposeProject != "" {
		env = append(env, EnvComposeProject+"="+top.ComposeProject)
	}
	files := []string{}
	if top.ComposeFile != "" {
		files = append(files, top.ComposeFile)
	}
	if top.OverlayFile != "" {
		files = append(files, top.OverlayFile)
	}
	if len(files) > 0 {
		// COMPOSE_PATH_SEPARATOR is set explicitly rather than relying on the
		// platform default: compose splits COMPOSE_FILE on ':' by default on
		// some platforms, which would cut a Windows path in half at the drive
		// letter.
		env = append(env,
			EnvComposePathSep+"="+string(os.PathListSeparator),
			EnvComposeFile+"="+strings.Join(files, string(os.PathListSeparator)),
		)
	}
	names := make([]string, 0, len(top.Nodes))
	for _, n := range top.Nodes {
		names = append(names, n.ID)
		if n.HostPort > 0 {
			env = append(env, EnvNodePortPrefix+envNodeKey(n.ID)+"="+strconv.FormatInt(n.HostPort, 10))
		}
	}
	env = append(env, EnvNodes+"="+strings.Join(names, ","))
	return env
}

// envNodeKey reduces a node id to an environment-variable-safe suffix.
func envNodeKey(id string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(id) {
		switch {
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// ProbeHost is the address a host-side probe is sent to. It mirrors
// harness.ProbeHost without dragging the harness into every consumer of the
// steady-state seam: both now read the same installed value, so the two cannot
// drift apart the way two constants could.
//
// Never 0.0.0.0: container IPs are NOT routable from a Windows host under
// Docker Desktop's Linux engine, verified directly, so a published port is the
// only way in (D-010). It defaults to loopback and is set by
// PROTHESIS_PROBE_HOST when the harness itself runs in a container (D-087).
func ProbeHost() string { return probehost.Host() }

// ErrUnterminatedQuote reports a probe or command template with an unbalanced
// quote.
var ErrUnterminatedQuote = errors.New("unterminated quote")

// SplitCommand splits a command template into argv using POSIX-ish quoting:
// whitespace separates words, single quotes are literal, double quotes allow a
// backslash escape of `"` and `\`.
//
// It deliberately implements NO expansion: no globbing, no variable
// substitution, no command substitution. The probe is EXEC'd, not interpreted,
// so a template that looks like it will expand must not silently half-expand.
func SplitCommand(s string) ([]string, error) {
	var (
		out  []string
		cur  strings.Builder
		have bool
	)
	flush := func() {
		if have {
			out = append(out, cur.String())
			cur.Reset()
			have = false
		}
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case ' ', '\t', '\n', '\r':
			flush()
		case '\'':
			have = true
			j := strings.IndexByte(s[i+1:], '\'')
			if j < 0 {
				return nil, fmt.Errorf("%w (') in %q", ErrUnterminatedQuote, s)
			}
			cur.WriteString(s[i+1 : i+1+j])
			i += j + 1
		case '"':
			have = true
			i++
			closed := false
			for ; i < len(s); i++ {
				if s[i] == '\\' && i+1 < len(s) && (s[i+1] == '"' || s[i+1] == '\\') {
					cur.WriteByte(s[i+1])
					i++
					continue
				}
				if s[i] == '"' {
					closed = true
					break
				}
				cur.WriteByte(s[i])
			}
			if !closed {
				return nil, fmt.Errorf("%w (\") in %q", ErrUnterminatedQuote, s)
			}
		default:
			have = true
			cur.WriteByte(c)
		}
	}
	flush()
	return out, nil
}

// steadyStateRequestFor builds the SEED probe request for a live topology.
func steadyStateRequestFor(cfg *schema.Config, projectDir string, top *recorder.Topology) SteadyStateRequest {
	return SteadyStateRequest{
		Config:     cfg,
		ProjectDir: projectDir,
		Topology:   top,
		Probe:      cfg.Harness.SteadyState.Probe,
		Timeout:    cfg.Harness.SteadyState.Timeout.Std(),
	}
}
