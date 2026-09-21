package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// The Docker CLI is the only engine interface in this tree, never the Go SDK.
//
// Two reasons, both verified on the build machine rather than assumed:
//
//  1. The SDK does not resolve Docker CONTEXTS. DOCKER_HOST is unset here and
//     the current context is `desktop-linux`, so client.NewClientWithOpts would
//     silently dial the wrong named pipe and fail in a way that looks like a
//     dead daemon.
//  2. Compose v2 is a CLI plugin with no stable Go API at all.
//
// See DECISIONS.md and docs/design/03-harness.design.md.

// dockerBin is the docker executable. It is a variable so tests can point it at
// a stub.
var dockerBin = "docker"

// run executes docker with args and returns stdout. Stderr is folded into the
// error, because docker writes progress there and a bare exit status is
// useless for diagnosis.
func run(ctx context.Context, dir string, args ...string) (string, error) {
	return runEnv(ctx, dir, nil, args...)
}

// dockerEnv is environment EVERY docker invocation carries, after the inherited
// environment and before the per-request one.
//
// BUILDX_NO_DEFAULT_ATTESTATIONS=1 (OQ-068). Compose builds through buildx, and
// buildx attaches a provenance attestation to every image by default. The
// attestation records the build's own timestamps, and the containerd image
// store reports the digest of the INDEX that wraps image and attestation
// together, so two fully cached builds of one unchanged Dockerfile yield two
// different image ids. `sut.images` records that id as the identity of what a
// world ran against (D-070); measured, twenty consecutive worlds against one
// Dockerfile recorded twenty identities. The switch changes nothing inside the
// image. It is set here, by the harness, rather than as `provenance: false` in
// a compose file, because the harness owns the invocation and a target's
// compose file is not ours to edit: this way every target gets a stable id, on
// any Compose version.
var dockerEnv = []string{"BUILDX_NO_DEFAULT_ATTESTATIONS=1"}

// runEnv is run with extra environment appended to the inherited one.
//
// The extra entries exist for exactly one purpose: compose INTERPOLATES
// `${VAR}` in the target's own compose file, so two concurrent worlds can only
// publish different host ports and create differently named networks if each
// compose invocation sees different values for the variables that file uses
// (DECISIONS.md D-042). A later entry wins in Go's exec, which is what makes
// this an override rather than a suggestion, and it is why dockerEnv sits
// BEFORE extra: the harness's default beats whatever the operator's shell
// exports, and a per-request entry beats the harness.
func runEnv(ctx context.Context, dir string, extra []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, dockerBin, args...)
	cmd.Dir = dir
	cmd.Env = append(append(os.Environ(), dockerEnv...), extra...)
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

// composeArgs builds a `docker compose` invocation.
//
// Flag placement is load-bearing and got this wrong once already: --progress is
// a ROOT-only flag on `docker compose`, not a per-subcommand one. It must come
// before `up`/`down`, never after, or the CLI rejects it.
//
// The file set and project name must be IDENTICAL between up and down, or
// compose resolves a different project and `down` tears down nothing while
// reporting success.
func composeArgs(project string, files []string, sub ...string) []string {
	args := []string{"compose", "--project-name", project}
	for _, f := range files {
		args = append(args, "--file", f)
	}
	args = append(args, sub...)
	return args
}

// dockerAvailable reports whether the daemon is reachable.
//
// A dead daemon is an ENVIRONMENT failure (exit 2, INCONCLUSIVE), never a
// config error (exit 5). The directive is explicit that exit 2 means "retry
// once, then escalate to a human", and that is exactly right for a Docker
// Desktop that has not finished starting.
func dockerAvailable(ctx context.Context) error {
	if _, err := run(ctx, "", "info", "--format", "{{.ServerVersion}}"); err != nil {
		return fmt.Errorf("docker daemon is not reachable: %w", err)
	}
	return nil
}

// psEntry is one row of `docker compose ps --format json`.
type psEntry struct {
	ID      string `json:"ID"`
	Name    string `json:"Name"`
	Service string `json:"Service"`
	State   string `json:"State"`
	Health  string `json:"Health"`
}

// composePS lists the project's containers.
//
// The CLI's JSON output is an UNVERSIONED contract (OQ-007 in the harness
// design): depending on the version it emits either a JSON array or one object
// per line. Handle both rather than pinning a version we cannot enforce on a
// user's machine.
func composePS(ctx context.Context, dir, project string, files []string, env []string) ([]psEntry, error) {
	out, err := runEnv(ctx, dir, env, composeArgs(project, files, "ps", "--all", "--format", "json")...)
	if err != nil {
		return nil, err
	}
	return parsePS(out)
}

func parsePS(out string) ([]psEntry, error) {
	trimmed := strings.TrimSpace(out)
	if trimmed == "" {
		return nil, nil
	}
	if strings.HasPrefix(trimmed, "[") {
		var rows []psEntry
		if err := json.Unmarshal([]byte(trimmed), &rows); err != nil {
			return nil, fmt.Errorf("parse `compose ps` array: %w", err)
		}
		return rows, nil
	}
	var rows []psEntry
	for _, line := range strings.Split(trimmed, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var e psEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			return nil, fmt.Errorf("parse `compose ps` line %q: %w", line, err)
		}
		rows = append(rows, e)
	}
	return rows, nil
}

// publishedPort returns the host port compose published for service:port.
//
// `docker compose port` prints `0.0.0.0:18081` on this host. 0.0.0.0 is a bind
// address, not a destination: probing it is wrong in principle and flaky in
// practice on Windows. Only the port number is kept, and every probe is
// addressed to 127.0.0.1 explicitly.
func publishedPort(ctx context.Context, dir, project string, files []string, env []string, service string, containerPort int64) (int64, error) {
	out, err := runEnv(ctx, dir, env, composeArgs(project, files,
		"port", service, strconv.FormatInt(containerPort, 10))...)
	if err != nil {
		return 0, err
	}
	return parsePublishedPort(out)
}

func parsePublishedPort(out string) (int64, error) {
	s := strings.TrimSpace(out)
	if s == "" {
		return 0, fmt.Errorf("`compose port` printed nothing; the service publishes no such port")
	}
	// Take the first line: a service publishing both IPv4 and IPv6 prints two.
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	i := strings.LastIndex(s, ":")
	if i < 0 {
		return 0, fmt.Errorf("cannot parse published port from %q", s)
	}
	p, err := strconv.ParseInt(strings.TrimSpace(s[i+1:]), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("cannot parse published port from %q: %w", s, err)
	}
	if p <= 0 || p > 65535 {
		return 0, fmt.Errorf("published port %d out of range (from %q)", p, s)
	}
	return p, nil
}

// containersByLabel returns container IDs carrying label=value, including
// stopped ones.
func containersByLabel(ctx context.Context, label, value string) ([]string, error) {
	out, err := run(ctx, "", "ps", "--all", "--quiet", "--filter", "label="+label+"="+value)
	if err != nil {
		return nil, err
	}
	return nonEmptyLines(out), nil
}

// pausedContainers returns the project's paused container IDs.
//
// This exists because of a specific trap: `docker compose down` cannot stop a
// PAUSED container; the stop blocks until the timeout and the container
// survives, leaking both it and its network. Phase 2's proc.pause is SIGSTOP,
// so any interrupted run can leave paused containers behind, and `down` is
// exactly the command that must cope with the mess an interrupted run made.
// Unpause first, then tear down.
func pausedContainers(ctx context.Context, project string) ([]string, error) {
	out, err := run(ctx, "", "ps", "--quiet",
		"--filter", "label=com.docker.compose.project="+project,
		"--filter", "status=paused")
	if err != nil {
		return nil, err
	}
	return nonEmptyLines(out), nil
}

// BridgeNetworkPool is how many bridge networks this engine's default address
// pool can carve out at once.
//
// MEASURED on the build machine (Docker Desktop 29.1.3): the ceiling is 30, of
// which about six are held at rest by the default bridge, Docker Desktop's own
// networks and whatever else is up, leaving ~24 free. It is a package variable
// rather than a constant because the pool is a function of
// `default-address-pools` in daemon.json, which an operator may have changed.
//
// This number is load-bearing for Phase 4 and not merely informational: the KV
// fixture creates TWO networks per compose project, so four concurrent worlds
// cost eight, and a LEAKED project holds its pair permanently. A search that
// leaks one world in twelve dies partway through with an error that reads like a
// Docker bug rather than like a teardown failure.
var BridgeNetworkPool = 30

// CountBridgeNetworks returns how many bridge networks currently exist.
func CountBridgeNetworks(ctx context.Context) (int, error) {
	out, err := run(ctx, "", "network", "ls", "--quiet", "--filter", "driver=bridge")
	if err != nil {
		return 0, err
	}
	return len(nonEmptyLines(out)), nil
}

// FreeBridgeNetworks estimates how many more bridge networks this engine can
// create: BridgeNetworkPool minus the ones already in existence.
//
// It is an ESTIMATE and says so. The pool is carved out of an address range, so
// a network created with an unusually wide subnet consumes more of it than one
// network's worth. Reporting a floor of zero rather than a negative number keeps
// the caller's arithmetic honest: "no room" is the answer either way.
func FreeBridgeNetworks(ctx context.Context) (int, error) {
	n, err := CountBridgeNetworks(ctx)
	if err != nil {
		return 0, err
	}
	free := BridgeNetworkPool - n
	if free < 0 {
		free = 0
	}
	return free, nil
}

func networksByLabel(ctx context.Context, label, value string) ([]string, error) {
	out, err := run(ctx, "", "network", "ls", "--quiet", "--filter", "label="+label+"="+value)
	if err != nil {
		return nil, err
	}
	return nonEmptyLines(out), nil
}

func nonEmptyLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}
