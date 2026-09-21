package control

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/WrdCstlg/pro-thesis/internal/perturber"
	"github.com/WrdCstlg/pro-thesis/internal/recorder"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// DefaultRoleProbeTimeout bounds one role observation when harness.role_probe
// names no timeout. Role resolution happens INSIDE a fault window, so it must
// be short: a probe that hangs for a minute moves the injection it was meant
// to place.
const DefaultRoleProbeTimeout = 10 * time.Second

// ExecRoleObserver resolves `role:` targets through a probe the TARGET
// declares (`harness.role_probe`), exec'd on the host the way
// harness.steady_state.probe is, with the same PROTHESIS_* environment
// (SteadyStateEnv: node list, host, and every node's published port).
//
// It exists because perturber.HTTPRoleObserver reads the reference fixture's
// /status document, which a third-party system does not have (OQ-063 item 4).
// Without it every `role:` target on such a system fails at injection time
// (fail-closed, exit 2) and the guided search's first rung, which pins
// role:leader, can never bind. Rather than teach the harness etcd's status
// API, the target says how to ask it: the probe is a program in the target's
// own directory, and it is covered by .prothesis/lock because a swapped probe
// changes which node `role:leader` hits (D-066).
//
// The probe prints one JSON object per line, order irrelevant:
//
//	{"node":"etcd-n1","role":"leader","term":7}
//	{"node":"etcd-n2","role":"follower","term":7}
//	{"node":"etcd-n3","error":"dial tcp 127.0.0.1:12381: connection refused"}
//
// A node the output does not name is UNOBSERVED (RoleObservation.Err), never a
// follower by default: absence of evidence is not evidence of absence, and the
// resolver must not bind a role to a node nobody asked. A non-zero exit,
// unparseable output, or the deadline expiring is an error for the whole
// request, which the resolver reports as a failed resolution: the same
// fail-closed path a 404 on /status takes.
type ExecRoleObserver struct {
	Probe      string
	Timeout    time.Duration
	ProjectDir string
	Config     *schema.Config
	Topology   *recorder.Topology
	// Exec runs argv in dir with env and returns its stdout. Nil means
	// exec.CommandContext; a test supplies its own.
	Exec func(ctx context.Context, argv []string, dir string, env []string) ([]byte, error)
}

// roleLine is one line of probe output.
type roleLine struct {
	Node  string `json:"node"`
	Role  string `json:"role"`
	Term  uint64 `json:"term"`
	Error string `json:"error"`
}

// ObserveRoles implements perturber.RoleObserver.
func (o *ExecRoleObserver) ObserveRoles(ctx context.Context, nodes []perturber.Node) ([]perturber.RoleObservation, error) {
	probe := strings.TrimSpace(o.Probe)
	if probe == "" {
		return nil, errors.New("control: harness.role_probe is empty")
	}
	argv, err := SplitCommand(probe)
	if err != nil || len(argv) == 0 {
		return nil, fmt.Errorf("control: harness.role_probe: %v", err)
	}
	timeout := o.Timeout
	if timeout <= 0 {
		timeout = DefaultRoleProbeTimeout
	}
	env := append(os.Environ(), SteadyStateEnv(SteadyStateRequest{
		Config:     o.Config,
		ProjectDir: o.ProjectDir,
		Topology:   o.Topology,
		Probe:      probe,
		Timeout:    timeout,
	})...)
	pctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	run := o.Exec
	if run == nil {
		run = execRoleProbe
	}
	out, err := run(pctx, argv, o.ProjectDir, env)
	if err != nil {
		if errors.Is(pctx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("control: harness.role_probe %q did not answer within %s", probe, timeout)
		}
		return nil, fmt.Errorf("control: harness.role_probe %q failed: %w", probe, err)
	}

	reported := map[string]roleLine{}
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var l roleLine
		if err := json.Unmarshal([]byte(line), &l); err != nil {
			return nil, fmt.Errorf("control: harness.role_probe: output line %d is not a JSON role record (%v): %q", lineNo, err, line)
		}
		if l.Node == "" {
			return nil, fmt.Errorf("control: harness.role_probe: output line %d names no node: %q", lineNo, line)
		}
		reported[l.Node] = l
	}

	obs := make([]perturber.RoleObservation, 0, len(nodes))
	for _, n := range nodes {
		o := perturber.RoleObservation{NodeID: n.ID}
		l, ok := reported[n.ID]
		switch {
		case !ok:
			o.Err = fmt.Errorf("the role probe did not report node %q", n.ID)
		case l.Error != "":
			o.Err = errors.New(l.Error)
		case l.Role == "":
			o.Err = fmt.Errorf("the role probe reported node %q with no role", n.ID)
		default:
			o.Role = strings.ToLower(strings.TrimSpace(l.Role))
			o.Term = l.Term
		}
		obs = append(obs, o)
	}
	return obs, nil
}

// execRoleProbe is the production Exec: no shell, stderr folded into the
// error so a failing probe is diagnosable.
func execRoleProbe(ctx context.Context, argv []string, dir string, env []string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if detail := strings.TrimSpace(stderr.String()); detail != "" {
			return nil, fmt.Errorf("%w: %s", err, detail)
		}
		return nil, err
	}
	return stdout.Bytes(), nil
}

// roleObserver picks the observer for this world: the target-declared probe
// when harness.role_probe names one, else the fixture-shaped /status reader.
func (r *Runner) roleObserver(topology *recorder.Topology) perturber.RoleObserver {
	rp := r.cfg.Harness.RoleProbe
	if strings.TrimSpace(rp.Probe) == "" {
		return &perturber.HTTPRoleObserver{}
	}
	return &ExecRoleObserver{
		Probe:      rp.Probe,
		Timeout:    time.Duration(rp.Timeout),
		ProjectDir: r.opts.ProjectDir,
		Config:     r.cfg,
		Topology:   topology,
	}
}
