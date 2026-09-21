package faults

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// The command runner
// ---------------------------------------------------------------------------

// CommandRunner executes an external program. It exists so the whole package
// can be tested without a Docker daemon, and so the one place that shells out
// is a single, auditable seam.
//
// The Docker CLI is the only engine interface in this tree; never the Go SDK
// (DECISIONS.md D-016). The SDK does not resolve Docker CONTEXTS, and on this
// machine the active context is `desktop-linux` with DOCKER_HOST unset, so an
// SDK client would dial the wrong pipe and fail like a dead daemon.
type CommandRunner interface {
	// Run executes name with args, writing stdin to the process if non-nil.
	// It returns stdout and stderr separately: docker writes progress to
	// stderr, so folding them together makes output parsing unreliable.
	Run(ctx context.Context, stdin []byte, name string, args ...string) (stdout, stderr string, err error)
}

// ExecRunner is the production CommandRunner.
type ExecRunner struct{}

// Run implements CommandRunner.
func (ExecRunner) Run(ctx context.Context, stdin []byte, name string, args ...string) (string, string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()
	return out.String(), errb.String(), err
}

// ---------------------------------------------------------------------------
// The sidecar image
// ---------------------------------------------------------------------------

const (
	// SidecarImage is the purpose-built NET_ADMIN image.
	//
	// The rehearsal used `alpine` plus `apk add iptables iproute2` at injection
	// time. That is the wrong shape for production use: it costs a registry
	// round trip and 2-4 seconds INSIDE the fault window, it makes injection
	// depend on outbound network access from the Docker VM, and it makes the
	// exact iptables and iproute2 versions vary run to run, which for a tool
	// whose entire product is a reproducible verdict is a determinism leak, not
	// just a slow path.
	//
	// So the packages are baked into a 19 MB image built once and cached in the
	// local image store. Building it costs ~1.6 s the first time and nothing
	// afterwards; `docker image inspect` is the guard.
	SidecarImage = "prothesis/netadmin:1"

	// SidecarDockerfile is built with `docker build -`; no build context at
	// all, so nothing from the project directory is uploaded to the daemon.
	//
	// alpine:3.20 is pinned rather than :latest because the image is a
	// measurement instrument: iproute2 v7.0.0 is the version whose `netem seed`
	// support D-025 depends on, and a floating base tag would silently change
	// it. ip6tables is included so a partition against an IPv6-addressed peer
	// is injectable rather than silently one-family.
	SidecarDockerfile = "FROM alpine:3.20\n" +
		"RUN apk add --no-cache iptables ip6tables iproute2\n" +
		"LABEL io.prothesis.image=\"netadmin\"\n" +
		"ENTRYPOINT [\"/bin/sh\"]\n"

	// LabelSidecar marks every sidecar container with the run that created it,
	// so a teardown can find everything a whole run left behind.
	LabelSidecar = "io.prothesis.sidecar"

	// LabelSidecarOwner marks it with the INSTANCE that created it.
	//
	// The run id alone is not a safe sweep key, and the reason is Phase 4:
	// D-022 runs worlds across concurrent compose projects WITHIN ONE RUN, so
	// several injectors share a run id and are live at the same time. A sweep
	// keyed on the run id would force-remove another world's in-flight
	// injection: turning one world's HEAL into another world's silent,
	// unexplainable fault failure. Keyed on the owner, a sweep can only ever
	// touch containers it created itself.
	LabelSidecarOwner = "io.prothesis.sidecar.owner"

	// DefaultSidecarTimeout bounds one sidecar invocation. Injection scripts
	// are a handful of iptables and tc calls; anything slower than this is a
	// wedged daemon, not slow work.
	DefaultSidecarTimeout = 30 * time.Second
)

// ---------------------------------------------------------------------------
// Sidecar
// ---------------------------------------------------------------------------

// Sidecar runs short-lived privileged containers inside a target's network
// namespace.
//
// It is safe for concurrent use.
type Sidecar struct {
	// Image is the sidecar image. Empty means SidecarImage.
	Image string
	// DockerBin is the docker executable. Empty means "docker".
	DockerBin string
	// RunID labels every sidecar so SweepSidecars can find strays.
	RunID string
	// Runner executes docker. Nil means ExecRunner.
	Runner CommandRunner
	// Timeout bounds one invocation. Zero means DefaultSidecarTimeout.
	Timeout time.Duration

	mu    sync.Mutex
	built bool
	owner string
}

// Owner is this instance's sweep key: a nonce allocated on first use.
//
// It is what stops one injector's HEAL from reaping another's live sidecars
// when several share a run id: see LabelSidecarOwner.
func (s *Sidecar) Owner() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.owner == "" {
		var b [6]byte
		if _, err := rand.Read(b[:]); err != nil {
			// Falling back to the run id is worse than a nonce but far better
			// than an empty label, which would make the sweep match every
			// sidecar on the host.
			s.owner = s.runLabelLocked()
		} else {
			s.owner = hex.EncodeToString(b[:])
		}
	}
	return s.owner
}

func (s *Sidecar) image() string {
	if s.Image == "" {
		return SidecarImage
	}
	return s.Image
}

func (s *Sidecar) docker() string {
	if s.DockerBin == "" {
		return "docker"
	}
	return s.DockerBin
}

func (s *Sidecar) runner() CommandRunner {
	if s.Runner == nil {
		return ExecRunner{}
	}
	return s.Runner
}

func (s *Sidecar) timeout() time.Duration {
	if s.Timeout <= 0 {
		return DefaultSidecarTimeout
	}
	return s.Timeout
}

// EnsureImage makes the sidecar image available, building it if it is not in
// the local image store. It is idempotent and cheap after the first call.
func (s *Sidecar) EnsureImage(ctx context.Context) error {
	s.mu.Lock()
	already := s.built
	s.mu.Unlock()
	if already {
		return nil
	}

	img := s.image()
	if _, _, err := s.runner().Run(ctx, nil, s.docker(), "image", "inspect", img); err == nil {
		s.mu.Lock()
		s.built = true
		s.mu.Unlock()
		return nil
	}

	// `docker build -` reads the Dockerfile from stdin with NO build context,
	// so the project directory is never uploaded to the daemon. `-f - .` would
	// tar the whole working tree, which on this repo is both slow and a
	// needless disclosure.
	_, stderr, err := s.runner().Run(ctx, []byte(SidecarDockerfile), s.docker(), "build", "--tag", img, "-")
	if err != nil {
		return fmt.Errorf("faults: build sidecar image %s: %w: %s", img, err, strings.TrimSpace(stderr))
	}
	s.mu.Lock()
	s.built = true
	s.mu.Unlock()
	return nil
}

// SidecarResult is one sidecar invocation's output.
type SidecarResult struct {
	// Name is the container name that ran, for correlating with docker events.
	Name string
	// Stdout and Stderr are the script's output.
	Stdout string
	Stderr string
}

// Exec runs script inside target's network namespace and returns its output.
//
// The container is removed on every path. `--rm` covers the ordinary case; the
// deferred force-remove covers the ones it does not: a cancelled context kills
// the docker CLI but leaves the container running, and a daemon that accepted
// `create` but failed `start` leaves a created container behind. A leaked
// sidecar is not merely untidy: it holds a reference to the target's network
// namespace, which makes the target's own teardown block.
func (s *Sidecar) Exec(ctx context.Context, target, script string) (SidecarResult, error) {
	if err := checkContainer(target); err != nil {
		return SidecarResult{}, err
	}
	if err := s.EnsureImage(ctx); err != nil {
		return SidecarResult{}, err
	}

	name, err := s.containerName()
	if err != nil {
		return SidecarResult{}, err
	}

	runCtx := ctx
	if s.timeout() > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, s.timeout())
		defer cancel()
	}

	args := []string{
		"run", "--rm",
		"--name", name,
		"--label", LabelSidecar + "=" + s.runLabel(),
		"--label", LabelSidecarOwner + "=" + s.Owner(),
		"--network", "container:" + target,
		"--cap-add", "NET_ADMIN",
		"--entrypoint", "/bin/sh",
		s.image(), "-c", script,
	}
	stdout, stderr, err := s.runner().Run(runCtx, nil, s.docker(), args...)
	res := SidecarResult{Name: name, Stdout: stdout, Stderr: stderr}
	if err != nil {
		// Detached context: the caller's may be the one that just expired, and
		// cleanup must still happen. This is the same reasoning that puts
		// TEARDOWN on a detached context in internal/control.
		cleanCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		_, _, _ = s.runner().Run(cleanCtx, nil, s.docker(), "rm", "--force", "--volumes", name)
		cancel()
		return res, fmt.Errorf("faults: sidecar in netns of %s: %w: %s",
			target, err, strings.TrimSpace(stderr))
	}
	return res, nil
}

// SweepSidecars force-removes every sidecar container THIS INSTANCE created and
// returns the ids of the ones that were genuinely STRAY.
//
// Two narrowings, both of which took a failing test to find.
//
// "Stray" means still alive (running, paused, or created but never started)
// and not merely "still listed". `docker run --rm` reaps ASYNCHRONOUSLY: the
// CLI returns when the process exits and the daemon removes the container a
// moment later, so a sweep run immediately afterwards still sees the corpse.
// Counting that reported a leak on every clean withdrawal, which would have
// made HEAL's most important signal useless through false positives. A live
// container joined to a target's network namespace holds a reference that
// blocks the target's teardown; an exited one holds nothing.
//
// And the sweep is scoped to this instance's OWNER label, not to the run id.
// Phase 4 runs several worlds concurrently within one run (D-022), so injectors
// share a run id and are live at the same time; a run-scoped sweep would
// force-remove another world's in-flight injection, turning one world's HEAL
// into another world's inexplicable fault failure. Use SweepRunSidecars for the
// teardown path that legitimately wants everything a whole run left behind.
//
// A non-empty result is therefore a real defect: `--rm` plus Exec's own error
// path should have removed every one. HEAL calls this as a backstop.
func (s *Sidecar) SweepSidecars(ctx context.Context) ([]string, error) {
	return s.sweep(ctx, LabelSidecarOwner+"="+s.Owner())
}

// SweepRunSidecars force-removes every sidecar container from this RUN,
// whichever injector instance created it.
//
// It is the recovery path for `thesis down`, which must cope with the mess an
// interrupted run made and has no in-memory owner to scope by. It must NOT be
// called while other worlds of the same run are still executing.
func (s *Sidecar) SweepRunSidecars(ctx context.Context) ([]string, error) {
	return s.sweep(ctx, LabelSidecar+"="+s.runLabel())
}

func (s *Sidecar) sweep(ctx context.Context, label string) ([]string, error) {
	alive, err := s.listSidecars(ctx, label, "running", "paused", "created")
	if err != nil {
		return nil, err
	}
	if len(alive) > 0 {
		args := append([]string{"rm", "--force", "--volumes"}, alive...)
		if _, stderr, rmErr := s.runner().Run(ctx, nil, s.docker(), args...); rmErr != nil {
			return alive, fmt.Errorf("faults: remove %d stray sidecar(s): %w: %s",
				len(alive), rmErr, strings.TrimSpace(stderr))
		}
	}
	// Reap exited leftovers too, but silently: they are harmless, and over a
	// long search they would otherwise accumulate one container per fault.
	if dead, listErr := s.listSidecars(ctx, label); listErr == nil && len(dead) > 0 {
		args := append([]string{"rm", "--force", "--volumes"}, dead...)
		_, _, _ = s.runner().Run(ctx, nil, s.docker(), args...)
	}
	return alive, nil
}

// listSidecars returns sidecar container ids matching a label, optionally
// restricted to the given container statuses.
func (s *Sidecar) listSidecars(ctx context.Context, label string, statuses ...string) ([]string, error) {
	args := []string{"ps", "--all", "--quiet", "--filter", "label=" + label}
	for _, st := range statuses {
		// Docker ORs repeated filters of the same key.
		args = append(args, "--filter", "status="+st)
	}
	out, stderr, err := s.runner().Run(ctx, nil, s.docker(), args...)
	if err != nil {
		return nil, fmt.Errorf("faults: list sidecars: %w: %s", err, strings.TrimSpace(stderr))
	}
	return nonEmptyLines(out), nil
}

func (s *Sidecar) runLabel() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.runLabelLocked()
}

func (s *Sidecar) runLabelLocked() string {
	if s.RunID == "" {
		return "unknown"
	}
	return s.RunID
}

func (s *Sidecar) containerName() (string, error) {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("faults: name sidecar: %w", err)
	}
	return "thesis-net-" + hex.EncodeToString(b[:]), nil
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func nonEmptyLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}
