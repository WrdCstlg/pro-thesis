package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Docker is the engine interface telemetry needs.
//
// It is an interface so the collector can be tested without a daemon. Every
// implementation must obey the same contract, in particular: a container that
// does not exist is a per-container absence, not a whole-call failure, because
// one node disappearing mid-run is exactly the event `no_crash` exists to catch
// and must not blind the collector to the other two nodes.
type Docker interface {
	// InspectStates returns state for each container that answered, keyed by
	// the id or name it was asked for. Containers that did not answer are
	// simply missing from the map.
	InspectStates(ctx context.Context, containers []string) (map[string]ContainerState, error)

	// ExecScript runs a shell program inside a container and returns its
	// stdout. It must not be called against a paused container.
	ExecScript(ctx context.Context, container, script string) (string, error)

	// Stats returns one `docker stats --no-stream` row per container, keyed the
	// same way as InspectStates.
	Stats(ctx context.Context, containers []string) (map[string]DockerStats, error)
}

// ContainerState is the subset of `docker inspect` telemetry consumes.
type ContainerState struct {
	// ID is the container's full id, as docker reported it.
	ID string
	// RestartCount comes from the top level of the inspect document, not from
	// .State.
	RestartCount int64

	Status     string `json:"Status"`
	Running    bool   `json:"Running"`
	Paused     bool   `json:"Paused"`
	Restarting bool   `json:"Restarting"`
	OOMKilled  bool   `json:"OOMKilled"`
	Dead       bool   `json:"Dead"`
	Pid        int64  `json:"Pid"`
	ExitCode   int64  `json:"ExitCode"`
	Error      string `json:"Error"`
	StartedAt  string `json:"StartedAt"`
	FinishedAt string `json:"FinishedAt"`
}

// DockerStats is one row of `docker stats --no-stream --format json`.
//
// The fields are docker's own spellings and its own human-readable formatting
// ("12.34%", "408KiB / 30.05GiB"). Parsing them back is lossy, which is one more
// reason this path is a fallback rather than the primary source.
type DockerStats struct {
	Container string `json:"Container"`
	ID        string `json:"ID"`
	Name      string `json:"Name"`
	CPUPerc   string `json:"CPUPerc"`
	MemUsage  string `json:"MemUsage"`
	MemPerc   string `json:"MemPerc"`
	PIDs      string `json:"PIDs"`
	NetIO     string `json:"NetIO"`
	BlockIO   string `json:"BlockIO"`
}

// CLI is the Docker implementation that shells out to the `docker` binary.
//
// The Docker CLI is the only engine interface in this tree, never the Go SDK.
// DECISIONS.md D-016: the SDK does not resolve Docker CONTEXTS, and on this host
// DOCKER_HOST is unset with the current context set to `desktop-linux`, so the
// SDK would silently dial the wrong named pipe and fail in a way that looks like
// a dead daemon.
type CLI struct {
	// Binary is the docker executable. Empty means "docker".
	Binary string
	// Timeout bounds each individual docker invocation. Empty means
	// DefaultDockerTimeout. It must stay well below the sampling interval, or a
	// wedged daemon call stalls the whole series instead of producing one
	// absent sample.
	Timeout time.Duration
}

// DefaultDockerTimeout bounds one docker invocation.
//
// 3 seconds is roughly 30x the measured cost of the two calls telemetry makes
// (inspect at 63 ms for three containers, exec at 106 ms), so it fires only for
// a genuinely wedged daemon and never for ordinary slowness.
const DefaultDockerTimeout = 3 * time.Second

func (c *CLI) bin() string {
	if c.Binary != "" {
		return c.Binary
	}
	return "docker"
}

func (c *CLI) timeout() time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	return DefaultDockerTimeout
}

// run executes docker with args. Stdout is returned even when the command
// failed, because `docker inspect a b` prints the rows it could resolve on
// stdout and reports the one it could not on stderr with exit 1.
func (c *CLI) run(ctx context.Context, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout())
	defer cancel()

	cmd := exec.CommandContext(ctx, c.bin(), args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		return stdout.String(), fmt.Errorf("docker %s: %w: %s",
			strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// inspectFormat asks for exactly the three things a sample needs, one row per
// container, each row self-identifying.
//
// Self-identification is load-bearing: `docker inspect a missing b` prints two
// rows, not three, and matching by position would then attribute b's state to
// the wrong node. Carrying the id in the row removes the ambiguity entirely.
const inspectFormat = `{{.Id}} {{.RestartCount}} {{json .State}}`

// InspectStates implements Docker.
func (c *CLI) InspectStates(ctx context.Context, containers []string) (map[string]ContainerState, error) {
	if len(containers) == 0 {
		return map[string]ContainerState{}, nil
	}
	args := append([]string{"inspect", "--format", inspectFormat}, containers...)
	out, runErr := c.run(ctx, args...)

	byID, parseErr := parseInspect(out)
	if parseErr != nil {
		return nil, parseErr
	}
	// Key the result by whatever the caller asked for, matching an id prefix in
	// either direction so a short id from `compose ps` finds its full row.
	res := make(map[string]ContainerState, len(containers))
	for _, want := range containers {
		if st, ok := matchContainer(byID, want); ok {
			res[want] = st
		}
	}
	if len(res) == 0 && runErr != nil {
		return nil, runErr
	}
	return res, nil
}

// parseInspect parses the self-identifying inspect rows.
func parseInspect(out string) (map[string]ContainerState, error) {
	res := map[string]ContainerState{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		id, rest, ok := strings.Cut(line, " ")
		if !ok {
			return nil, fmt.Errorf("telemetry: cannot parse inspect row %q", line)
		}
		countTxt, stateJSON, ok := strings.Cut(rest, " ")
		if !ok {
			return nil, fmt.Errorf("telemetry: cannot parse inspect row %q", line)
		}
		var st ContainerState
		if err := json.Unmarshal([]byte(stateJSON), &st); err != nil {
			return nil, fmt.Errorf("telemetry: cannot parse inspect state for %s: %w", id, err)
		}
		st.ID = id
		fmt.Sscan(countTxt, &st.RestartCount)
		res[id] = st
	}
	return res, nil
}

// matchContainer resolves a requested id/name against the rows docker returned.
func matchContainer(byID map[string]ContainerState, want string) (ContainerState, bool) {
	if st, ok := byID[want]; ok {
		return st, true
	}
	if want == "" {
		return ContainerState{}, false
	}
	for id, st := range byID {
		if strings.HasPrefix(id, want) || strings.HasPrefix(want, id) {
			return st, true
		}
	}
	return ContainerState{}, false
}

// ExecScript implements Docker.
//
// The script runs under `sh -c`, which is the only way to read several files in
// one round trip; see cgroup.go. This is the one place PRO-THESIS requires
// anything of the target image, and the requirement degrades to ABSENT metrics
// rather than to an error, so an unmodified distroless target still runs.
func (c *CLI) ExecScript(ctx context.Context, container, script string) (string, error) {
	if container == "" {
		return "", fmt.Errorf("telemetry: exec needs a container")
	}
	return c.run(ctx, "exec", container, "sh", "-c", script)
}

// Stats implements Docker.
//
// Measured at 993 ms for a single container on the build machine (roughly twice
// the default sampling interval) which is why the collector leaves it disabled
// unless asked. It also cannot produce a page-cache-free memory figure, so it
// never contributes to RSSBytes.
func (c *CLI) Stats(ctx context.Context, containers []string) (map[string]DockerStats, error) {
	if len(containers) == 0 {
		return map[string]DockerStats{}, nil
	}
	args := append([]string{"stats", "--no-stream", "--format", "json"}, containers...)
	out, runErr := c.run(ctx, args...)

	rows, err := parseStats(out)
	if err != nil {
		return nil, err
	}
	res := make(map[string]DockerStats, len(containers))
	for _, want := range containers {
		for _, r := range rows {
			if r.ID == want || r.Name == want || r.Container == want ||
				(r.ID != "" && (strings.HasPrefix(want, r.ID) || strings.HasPrefix(r.ID, want))) {
				res[want] = r
				break
			}
		}
	}
	if len(res) == 0 && runErr != nil {
		return nil, runErr
	}
	return res, nil
}

// parseStats handles both shapes the CLI emits.
//
// `--format json` is an UNVERSIONED contract: depending on the version it is a
// JSON array or one object per line. Both are parsed rather than pinning a
// version that cannot be enforced on a user's machine: the same reasoning as
// internal/harness's `compose ps` handling.
func parseStats(out string) ([]DockerStats, error) {
	t := strings.TrimSpace(out)
	if t == "" {
		return nil, nil
	}
	if strings.HasPrefix(t, "[") {
		var rows []DockerStats
		if err := json.Unmarshal([]byte(t), &rows); err != nil {
			return nil, fmt.Errorf("telemetry: parse `docker stats` array: %w", err)
		}
		return rows, nil
	}
	var rows []DockerStats
	for _, line := range strings.Split(t, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var r DockerStats
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			return nil, fmt.Errorf("telemetry: parse `docker stats` line %q: %w", line, err)
		}
		rows = append(rows, r)
	}
	return rows, nil
}

// applyState folds a docker inspect state into a sample.
func applyState(s *Sample, st ContainerState) {
	p := &ProcessMetrics{
		Status:       st.Status,
		Running:      boolp(st.Running),
		Paused:       boolp(st.Paused),
		Restarting:   boolp(st.Restarting),
		OOMKilled:    boolp(st.OOMKilled),
		Dead:         boolp(st.Dead),
		PID:          i64(st.Pid),
		ExitCode:     i64(st.ExitCode),
		Error:        st.Error,
		RestartCount: i64(st.RestartCount),
	}
	if ns, ok := parseDockerTime(st.StartedAt); ok {
		p.StartedAtNS = i64(ns)
	}
	if ns, ok := parseDockerTime(st.FinishedAt); ok {
		p.FinishedAtNS = i64(ns)
	}
	s.Process = p
}

// parseDockerTime converts docker's RFC3339 nano timestamps to Unix epoch
// nanoseconds. Docker's zero time ("0001-01-01T00:00:00Z") means "never", and
// is reported as absent rather than as a nanosecond value 62 billion seconds in
// the past.
func parseDockerTime(s string) (int64, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return 0, false
	}
	if t.IsZero() || t.Year() <= 1 {
		return 0, false
	}
	return t.UnixNano(), true
}
