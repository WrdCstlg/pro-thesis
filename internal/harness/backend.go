// Package harness owns the environment: bring the topology up, wait for health,
// tear it down, and collect artifacts.
//
// Phase 0 delivers the compose backend skeleton and nothing more. There is no
// fault injection here: that is Phase 2's Perturber, which will drive a
// NET_ADMIN sidecar joined to a target container's network namespace (see
// DECISIONS.md D-008). The Backend interface below is deliberately shaped so
// that arrives as a new method set rather than a rewrite.
package harness

import (
	"context"
	"errors"
	"fmt"
	"runtime"

	"github.com/WrdCstlg/pro-thesis/internal/recorder"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// ErrUnsupportedBackend is returned when a backend named in prothesis.yaml
// cannot run on this host.
var ErrUnsupportedBackend = errors.New("harness: backend not supported on this platform")

// Backend supervises a topology. compose is the only Phase 0 implementation;
// process, k8s and sim are named by the directive and reserved here.
type Backend interface {
	// Name is the schema.Backend value this implementation serves.
	Name() schema.Backend

	// Up brings the topology to the point where every declared health probe
	// passes, and returns the node bindings it observed. It is responsible for
	// leaving nothing behind if it fails partway.
	Up(ctx context.Context, req UpRequest) (*recorder.Topology, error)

	// Down destroys everything Up created. It must be idempotent: calling it
	// twice, or against a topology that is already gone, is not an error.
	Down(ctx context.Context, top *recorder.Topology, opts DownOptions) error
}

// UpRequest is the input to Backend.Up.
type UpRequest struct {
	// Config is the validated prothesis.yaml.
	Config *schema.Config
	// ProjectDir is the directory prothesis.yaml lives in. Every relative path
	// in the config resolves against it.
	ProjectDir string
	// RunID is the run this topology belongs to.
	RunID string
	// RunDir is the artifact directory for the run; the backend writes its
	// generated overlay and boot logs here.
	RunDir string
	// SkipHealth brings the topology up without waiting for health probes.
	// Used by `thesis up --no-wait` for manual poking.
	SkipHealth bool

	// ProjectName overrides the derived compose project name.
	//
	// ADDITIVE, and empty means "derive it as ProjectName(cfg, projectDir)
	// does", which is every existing call site unchanged. Phase 4 sets it so
	// concurrent worlds address disjoint compose projects; two worlds sharing a
	// project name would have the second one's `up` adopt the first one's
	// containers and the first one's `down` destroy the second one's.
	ProjectName string

	// OverlayPath overrides where the generated label overlay is written.
	//
	// ADDITIVE, and empty means RunDir/OverlayName, which is every existing call
	// site unchanged. Concurrent worlds need distinct files because the overlay
	// carries the project label and `down` MUST be handed the identical file set
	// `up` used or compose resolves a different project.
	OverlayPath string

	// Env is extra environment for every docker invocation this request makes.
	//
	// ADDITIVE. It is how a per-world compose project gets distinct PUBLISHED
	// HOST PORTS and distinct network names: compose interpolates `${VAR}` in
	// the target's own compose file, and the fixture already parameterises both
	// (`${KV_PORT_N1:-18081}`, `${KV_PREFIX:-prothesis}`). The harness supplies
	// values; it never edits the system under test. See DECISIONS.md D-042.
	Env []string
}

// DownOptions controls teardown.
type DownOptions struct {
	// Volumes removes named volumes as well as containers and networks.
	Volumes bool
	// Timeout is the per-container stop grace period.
	Timeout schema.Duration

	// Env mirrors UpRequest.Env for teardown.
	//
	// ADDITIVE. `compose down` re-reads and re-interpolates the compose file, so
	// a project brought up with KV_PREFIX=w03 must be taken down with the same
	// value or compose looks for networks under the wrong names. The label sweep
	// is the backstop and catches them anyway, but a `down` that needed the
	// backstop has already printed a confusing error, and on this host a missed
	// network is a permanently consumed one.
	Env []string
}

// New returns the backend named by cfg.Harness.Backend.
func New(cfg *schema.Config) (Backend, error) {
	switch cfg.Harness.Backend {
	case schema.BackendCompose:
		return NewCompose(), nil

	case schema.BackendProcess:
		// OQ-002. The directive names `process` as a v1 backend, but every
		// fault the tool exists to inject is a Linux primitive: SIGSTOP for
		// proc.pause, iptables for net.partition, tc/netem for the shaping
		// family. None exists on a Windows host.
		//
		// Degrading to "supervises processes, injects nothing" would be worse
		// than refusing: a world whose faults never fired would report PASS,
		// which is the single most dangerous outcome this tool can produce.
		if runtime.GOOS == "windows" {
			return nil, fmt.Errorf("%w: `process` needs SIGSTOP, iptables and tc/netem, none of which "+
				"exist on Windows; use `backend: compose` (see OPEN_QUESTIONS.md OQ-002)", ErrUnsupportedBackend)
		}
		return nil, fmt.Errorf("%w: the process backend is not implemented until a later phase", ErrUnsupportedBackend)

	case schema.BackendK8s, schema.BackendSim:
		return nil, fmt.Errorf("%w: backend %q is a post-v1 backend (directive section 3)",
			ErrUnsupportedBackend, cfg.Harness.Backend)

	default:
		return nil, fmt.Errorf("harness: unknown backend %q", cfg.Harness.Backend)
	}
}
