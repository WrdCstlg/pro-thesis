package harness

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/WrdCstlg/pro-thesis/internal/recorder"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

const (
	// LabelProject marks every container in a PRO-THESIS-managed project.
	LabelProject = "io.prothesis.project"
	// LabelNode binds a container to a logical node id from prothesis.yaml.
	// This is what makes node -> container lookup exact and restart-survivable
	// instead of guessing from compose's container naming convention.
	LabelNode = "io.prothesis.node"
	// LabelGroup records the node's LOGICAL service group: the thing
	// `minority(kv)` and `kv:*` resolve over. Stamped so Phase 2 can select a
	// quorum by label without re-reading prothesis.yaml, and so the grouping is
	// visible in `docker ps` when diagnosing a partition by hand.
	LabelGroup = "io.prothesis.group"

	// OverlayName is the generated compose overlay `up` writes into the run
	// directory. `down` MUST be given the same file set or compose resolves a
	// different project.
	OverlayName = "prothesis-overlay.yaml"
)

// Compose is the Docker Compose backend.
type Compose struct{}

// NewCompose returns the compose backend.
func NewCompose() *Compose { return &Compose{} }

// Name implements Backend.
func (c *Compose) Name() schema.Backend { return schema.BackendCompose }

// ProjectName derives the compose project name for a config.
//
// It must be stable across processes, because `up` and `down` are separate OS
// invocations and compose addresses a project by name. It is derived from the
// config name plus a hash of the absolute compose file path, so two checkouts
// of the same repo on one machine do not collide.
func ProjectName(cfg *schema.Config, projectDir string) string {
	abs, err := filepath.Abs(filepath.Join(projectDir, cfg.Harness.File))
	if err != nil {
		abs = filepath.Join(projectDir, cfg.Harness.File)
	}
	sum := sha256.Sum256([]byte(strings.ToLower(filepath.ToSlash(abs))))
	return "thesis-" + sanitizeProject(cfg.Name) + "-" + hex.EncodeToString(sum[:4])
}

// sanitizeProject reduces s to what compose accepts: [a-z0-9_-].
func sanitizeProject(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-_")
	if out == "" {
		out = "project"
	}
	if len(out) > 32 {
		out = strings.Trim(out[:32], "-_")
	}
	return out
}

// Up implements Backend.
func (c *Compose) Up(ctx context.Context, req UpRequest) (*recorder.Topology, error) {
	cfg := req.Config
	project := req.ProjectName
	if project == "" {
		project = ProjectName(cfg, req.ProjectDir)
	}

	if err := dockerAvailable(ctx); err != nil {
		return nil, err
	}

	overlayPath, err := c.writeOverlay(req, project)
	if err != nil {
		return nil, fmt.Errorf("write compose overlay: %w", err)
	}
	files := []string{cfg.Harness.File, overlayPath}

	top := &recorder.Topology{
		Schema:         recorder.TopologySchema,
		RunID:          req.RunID,
		Backend:        string(schema.BackendCompose),
		ComposeFile:    cfg.Harness.File,
		ComposeProject: project,
		OverlayFile:    overlayPath,
	}

	// Bring the project up. --wait makes compose block on its own healthchecks
	// where the target defines them; PRO-THESIS's `harness.health` probes are a
	// separate, host-side gate applied afterwards. They answer different
	// questions and both are wanted: compose's healthcheck is the container's
	// own opinion, ours is reachability from where the driver actually runs.
	if _, err := runEnv(ctx, req.ProjectDir, req.Env,
		composeArgs(project, files, "up", "--detach", "--wait", "--remove-orphans")...); err != nil {
		// Do not leave a half-built project behind holding one of the 24
		// available bridge networks.
		_ = c.Down(ctx, top, DownOptions{Volumes: true, Env: req.Env})
		return nil, fmt.Errorf("compose up: %w", err)
	}

	bindings, err := c.bind(ctx, req, project, files)
	if err != nil {
		_ = c.Down(ctx, top, DownOptions{Volumes: true, Env: req.Env})
		return nil, err
	}
	top.Nodes = bindings
	top.CreatedWallUnixNS = time.Now().UnixNano()

	if !req.SkipHealth {
		if err := WaitHealthy(ctx, cfg, top); err != nil {
			_ = c.Down(ctx, top, DownOptions{Volumes: true, Env: req.Env})
			return nil, err
		}
	}
	return top, nil
}

// writeOverlay generates the compose overlay that stamps PRO-THESIS labels onto
// every declared node's service.
//
// Only STABLE values go in here. Per-invocation values (run id, pid, timestamp)
// must never be stamped into a service label: compose hashes the service
// definition to decide whether a container needs recreating, so a changing
// label would force a full recreate on every single command. Run identity lives
// in the run directory, which is where it belongs.
func (c *Compose) writeOverlay(req UpRequest, project string) (string, error) {
	var b strings.Builder
	b.WriteString("# Generated by `thesis up`. Do not edit.\n")
	b.WriteString("#\n")
	b.WriteString("# This overlay stamps io.prothesis.* labels onto the services backing the\n")
	b.WriteString("# logical nodes in prothesis.yaml, so node -> container lookup is exact and\n")
	b.WriteString("# survives a restart. It deliberately carries no per-invocation values:\n")
	b.WriteString("# compose hashes the service definition, so a changing label would force a\n")
	b.WriteString("# recreate on every command.\n")
	b.WriteString("services:\n")
	for _, n := range req.Config.Harness.Nodes {
		// EffectiveComposeService, never Service: `service` is the LOGICAL
		// group the fault grammar targets (`minority(kv)`), which is not a
		// compose service name when each node publishes its own port.
		fmt.Fprintf(&b, "  %s:\n", n.EffectiveComposeService())
		b.WriteString("    labels:\n")
		fmt.Fprintf(&b, "      %s: %q\n", LabelProject, project)
		fmt.Fprintf(&b, "      %s: %q\n", LabelNode, n.ID)
		fmt.Fprintf(&b, "      %s: %q\n", LabelGroup, n.Service)
	}
	path := req.OverlayPath
	if path == "" {
		path = filepath.Join(req.RunDir, OverlayName)
	}
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", err
		}
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// bind resolves every logical node to its container and published host port.
func (c *Compose) bind(ctx context.Context, req UpRequest, project string, files []string) ([]recorder.NodeBinding, error) {
	rows, err := composePS(ctx, req.ProjectDir, project, files, req.Env)
	if err != nil {
		return nil, fmt.Errorf("compose ps: %w", err)
	}
	byService := map[string]psEntry{}
	for _, r := range rows {
		byService[r.Service] = r
	}

	var out []recorder.NodeBinding
	for _, n := range req.Config.Harness.Nodes {
		cs := n.EffectiveComposeService()
		row, ok := byService[cs]
		if !ok {
			return nil, fmt.Errorf("node %q: compose service %q is not running "+
				"(is it declared in %s?)", n.ID, cs, req.Config.Harness.File)
		}
		nb := recorder.NodeBinding{
			ID:            n.ID,
			Service:       cs,
			ContainerID:   row.ID,
			ContainerPort: int64(n.ClientPort()),
		}
		// A node with no published port is not an error at bind time: only a
		// node that a health probe targets needs one. Record 0 and let the
		// probe stage produce the specific complaint.
		if hp, err := publishedPort(ctx, req.ProjectDir, project, files, req.Env, cs, int64(n.ClientPort())); err == nil {
			nb.HostPort = hp
		}
		out = append(out, nb)
	}
	return out, nil
}

// Down implements Backend. It is idempotent and verifies its own work.
func (c *Compose) Down(ctx context.Context, top *recorder.Topology, opts DownOptions) error {
	if top == nil || top.ComposeProject == "" {
		return fmt.Errorf("harness: down needs a topology with a compose project name")
	}
	if err := dockerAvailable(ctx); err != nil {
		return err
	}
	project := top.ComposeProject

	// Unpause first. A paused container cannot be stopped: `compose down`
	// blocks for the full timeout and then leaves it running, leaking the
	// container and its network. Phase 2's proc.pause is SIGSTOP, so an
	// interrupted run leaves exactly this state behind, and coping with an
	// interrupted run is what `down` is for.
	if paused, err := pausedContainers(ctx, project); err == nil {
		for _, id := range paused {
			_, _ = run(ctx, "", "unpause", id)
		}
	}

	files := []string{top.ComposeFile}
	if top.OverlayFile != "" {
		if _, err := os.Stat(top.OverlayFile); err == nil {
			files = append(files, top.OverlayFile)
		}
	}

	sub := []string{"down", "--remove-orphans"}
	if opts.Volumes {
		sub = append(sub, "--volumes")
	}
	if opts.Timeout > 0 {
		sub = append(sub, "--timeout", fmt.Sprintf("%d", int(opts.Timeout.Std().Seconds())))
	}
	dir := filepath.Dir(top.ComposeFile)
	composeErr := func() error {
		_, err := runEnv(ctx, dir, opts.Env, composeArgs(project, files, sub...)...)
		return err
	}()

	// Sweep by label regardless of whether compose succeeded. The compose file
	// may have been edited or deleted since `up`, in which case `compose down`
	// cannot address what it created but the labels still can.
	residual := c.sweep(ctx, project)

	if composeErr != nil && residual != nil {
		return fmt.Errorf("compose down failed (%v) and residual resources remain: %w", composeErr, residual)
	}
	if residual != nil {
		return residual
	}
	return nil
}

// sweep force-removes anything still carrying the project's labels and reports
// what it could not remove.
//
// This matters more than ordinary hygiene: the Docker bridge-network pool on
// this host has a hard ceiling of 30 with 24 free, and a leaked project holds
// one permanently. A search that leaks a network per world exhausts the pool
// after a couple of dozen worlds and then fails in a way that looks like a
// Docker bug.
func (c *Compose) sweep(ctx context.Context, project string) error {
	if ids, err := containersByLabel(ctx, "com.docker.compose.project", project); err == nil && len(ids) > 0 {
		args := append([]string{"rm", "--force", "--volumes"}, ids...)
		_, _ = run(ctx, "", args...)
	}
	if nets, err := networksByLabel(ctx, "com.docker.compose.project", project); err == nil {
		for _, n := range nets {
			_, _ = run(ctx, "", "network", "rm", n)
		}
	}

	// Verify. `down` claiming success while resources survive is exactly the
	// failure that exhausts the network pool silently.
	var left []string
	if ids, err := containersByLabel(ctx, "com.docker.compose.project", project); err == nil && len(ids) > 0 {
		left = append(left, fmt.Sprintf("%d container(s)", len(ids)))
	}
	if nets, err := networksByLabel(ctx, "com.docker.compose.project", project); err == nil && len(nets) > 0 {
		left = append(left, fmt.Sprintf("%d network(s)", len(nets)))
	}
	if len(left) > 0 {
		return fmt.Errorf("harness: project %s still has %s after down", project, strings.Join(left, " and "))
	}
	return nil
}
