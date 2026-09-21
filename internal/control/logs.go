package control

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/WrdCstlg/pro-thesis/internal/recorder"
)

// Bounds on collected logs. A world is seconds long, so these are backstops
// against a node stuck in a log-spinning loop rather than routine limits, but
// they are backstops that matter: an unbounded collection would let one sick
// container fill the artifact bundle and then the disk, turning a diagnosable
// bug into an environment failure on the NEXT run.
const (
	maxLogBytesPerNode = 4 << 20 // 4 MiB
	maxLogTailLines    = 20000
)

// DockerLogCollector collects container logs through the Docker CLI.
//
// The CLI, never the Go SDK, for the reason recorded in DECISIONS.md D-016:
// DOCKER_HOST is unset on this host and the current context is `desktop-linux`,
// so the SDK would dial the wrong named pipe and fail in a way indistinguishable
// from a dead daemon.
//
// It addresses containers by the id `thesis up` recorded in topology.json, so it
// needs nothing from the compose project and works after an interrupted run.
type DockerLogCollector struct {
	// Bin is the docker executable. Empty means "docker". A field rather than a
	// package variable so a test can point one collector at a stub without
	// affecting anything else in the process.
	Bin string
	// Timeout bounds a single node's collection. Zero means
	// DefaultLogCollectTimeout.
	Timeout time.Duration
}

// DefaultLogCollectTimeout bounds one node's log collection.
const DefaultLogCollectTimeout = 20 * time.Second

// NewDockerLogCollector returns the default collector.
func NewDockerLogCollector() *DockerLogCollector { return &DockerLogCollector{} }

func (c *DockerLogCollector) bin() string {
	if c.Bin != "" {
		return c.Bin
	}
	return "docker"
}

func (c *DockerLogCollector) timeout() time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	return DefaultLogCollectTimeout
}

// Collect implements LogCollector.
//
// It NEVER returns a fatal error for a single node: a per-node failure is
// recorded in that node's CollectedLog.Err and collection continues. A missing
// log makes a causal timeline thinner; a failed teardown leaks a bridge network.
// Those are not comparable costs, and TEARDOWN is where both live.
func (c *DockerLogCollector) Collect(ctx context.Context, req LogRequest) ([]CollectedLog, error) {
	if req.Topology == nil {
		return nil, nil
	}
	if req.Dir != "" {
		if err := os.MkdirAll(req.Dir, 0o755); err != nil {
			return nil, fmt.Errorf("control: create log dir %s: %w", req.Dir, err)
		}
	}
	out := make([]CollectedLog, 0, len(req.Topology.Nodes))
	for _, n := range req.Topology.Nodes {
		cl := CollectedLog{Node: n.ID}
		if n.ContainerID == "" {
			cl.Err = fmt.Errorf("control: node %q has no recorded container id", n.ID)
			out = append(out, cl)
			continue
		}
		raw, err := c.dockerLogs(ctx, n.ContainerID)
		if err != nil {
			cl.Err = err
		}
		if len(raw) > 0 && req.Dir != "" {
			p := filepath.Join(req.Dir, sanitizeLogName(n.ID)+".log")
			if werr := os.WriteFile(p, raw, 0o644); werr != nil {
				if cl.Err == nil {
					cl.Err = fmt.Errorf("control: write %s: %w", p, werr)
				}
			} else {
				cl.Path = p
			}
		}
		cl.Lines = ParseTimestampedLog(raw, asVirtualClock(req.Timeline))
		out = append(out, cl)
	}
	return out, nil
}

// dockerLogs runs `docker logs --timestamps` and returns the combined streams,
// truncated at maxLogBytesPerNode.
func (c *DockerLogCollector) dockerLogs(ctx context.Context, containerID string) ([]byte, error) {
	lctx, cancel := context.WithTimeout(ctx, c.timeout())
	defer cancel()

	cmd := exec.CommandContext(lctx, c.bin(), "logs", "--timestamps",
		"--tail", fmt.Sprintf("%d", maxLogTailLines), containerID)
	var buf bytes.Buffer
	// stdout and stderr are interleaved deliberately: a Go service writes its
	// log to stderr and its panic to stderr too, but a crash message and the
	// last ordinary line before it are only useful together and only in order.
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	b := buf.Bytes()
	if len(b) > maxLogBytesPerNode {
		b = append(b[:maxLogBytesPerNode:maxLogBytesPerNode],
			[]byte("\n[truncated by PRO-THESIS at "+fmt.Sprintf("%d", maxLogBytesPerNode)+" bytes]\n")...)
	}
	if err != nil {
		return b, fmt.Errorf("control: docker logs %s: %w", shortContainerID(containerID), err)
	}
	return b, nil
}

func shortContainerID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// ParseTimestampedLog parses `docker logs --timestamps` output.
//
// Each line is `<RFC3339Nano> <text>`. A line without a parseable timestamp is
// kept with TNS 0 and HasVirtual false rather than dropped: an unparseable line
// is still evidence, and dropping evidence to keep a parser tidy is how a causal
// timeline ends up omitting the one line that explained the failure.
func ParseTimestampedLog(raw []byte, tl virtualClock) []LogLine {
	if len(raw) == 0 {
		return nil
	}
	lines := strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n")
	out := make([]LogLine, 0, len(lines))
	for _, ln := range lines {
		ln = strings.TrimRight(ln, "\r")
		if strings.TrimSpace(ln) == "" {
			continue
		}
		l := LogLine{Text: ln}
		if i := strings.IndexByte(ln, ' '); i > 0 {
			if t, err := time.Parse(time.RFC3339Nano, ln[:i]); err == nil {
				l.TNS = t.UnixNano()
				l.Text = ln[i+1:]
				if tl != nil {
					if ms, err := tl.VMSFromEpochNS(l.TNS); err == nil {
						l.TMS = ms
						l.HasVirtual = true
					}
				}
			}
		}
		out = append(out, l)
	}
	return out
}

// virtualClock is the sliver of recorder.Timeline this file needs. Declaring it
// keeps the log parser testable without constructing a clock.
type virtualClock interface {
	VMSFromEpochNS(tNS int64) (int64, error)
}

// asVirtualClock converts a possibly-nil *recorder.Timeline into a
// possibly-nil interface value.
//
// A typed nil pointer stored in an interface is NOT nil, so assigning
// (*recorder.Timeline)(nil) straight into virtualClock would make `tl != nil`
// true and the first method call would dereference through a nil receiver into
// a mutex. This is the one place that conversion happens.
func asVirtualClock(tl *recorder.Timeline) virtualClock {
	if tl == nil {
		return nil
	}
	return tl
}
