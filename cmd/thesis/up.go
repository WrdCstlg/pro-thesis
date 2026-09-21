package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/WrdCstlg/pro-thesis/internal/harness"
	"github.com/WrdCstlg/pro-thesis/internal/recorder"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

func cmdUp(ctx context.Context, g globals, args []string) schema.ExitCode {
	var noWait bool
	for _, a := range args {
		switch a {
		case "--no-wait":
			noWait = true
		default:
			errorf("up: unknown flag %q", a)
			return schema.ExitConfigError
		}
	}

	cfg, projectDir, err := loadConfig(g)
	if err != nil {
		errorf("%v", err)
		return schema.ExitConfigError
	}

	backend, err := harness.New(cfg)
	if err != nil {
		// An unsupported backend is a config problem: the user named it.
		errorf("%v", err)
		return schema.ExitConfigError
	}

	runsDir := recorder.RunsDir(projectDir)
	if err := os.MkdirAll(runsDir, 0o755); err != nil {
		errorf("create %s: %v", runsDir, err)
		return schema.ExitInconclusive
	}

	// Refuse to start a second topology over a live one. Two runs sharing a
	// compose project name would fight, and the CURRENT pointer can only name
	// one: so `down` would be unable to address whichever it did not record.
	if prev, err := recorder.ReadCurrentRun(runsDir); err == nil && prev != "" {
		errorf("run %s is already up (see %s). Run `thesis down` first.", prev, runsDir)
		return schema.ExitInconclusive
	}

	bundle, err := recorder.OpenBundle(runsDir, time.Now().UTC())
	if err != nil {
		errorf("open run bundle: %v", err)
		return schema.ExitInconclusive
	}
	infof(g, "run %s", bundle.RunID())

	top, err := backend.Up(ctx, harness.UpRequest{
		Config:     cfg,
		ProjectDir: projectDir,
		RunID:      bundle.RunID(),
		RunDir:     bundle.RunDir(),
		SkipHealth: noWait,
	})
	if err != nil {
		// The backend already tore down what it built. Close the bundle so the
		// failed run is not left looking live.
		_ = bundle.Close()
		if errors.Is(err, context.Canceled) {
			errorf("up: interrupted")
			return schema.ExitInconclusive
		}
		errorf("up: %v", err)
		return schema.ExitInconclusive
	}

	// Persist the handoff BEFORE reporting success. `up` and `down` are
	// separate OS processes: everything `down` needs to address what was
	// created (the compose project name, the generated overlay path, the
	// node-to-container bindings) exists only on disk. If this write fails,
	// the topology is unreachable by any later command, so tear it back down
	// rather than leaking it.
	if err := bundle.WriteTopology(top); err != nil {
		errorf("persist topology: %v", err)
		_ = backend.Down(ctx, top, harness.DownOptions{Volumes: true})
		_ = bundle.Close()
		return schema.ExitInconclusive
	}
	if err := recorder.WriteCurrentRun(runsDir, bundle.RunID()); err != nil {
		errorf("record current run: %v", err)
		_ = backend.Down(ctx, top, harness.DownOptions{Volumes: true})
		_ = bundle.Close()
		return schema.ExitInconclusive
	}

	// Deliberately NOT bundle.Close(). An unclosed bundle is the sentinel that
	// marks this run live; `down` closes it. Closing here would finalize the
	// manifest over a run that has not happened yet.

	if g.json {
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{
			"run_id":          top.RunID,
			"compose_project": top.ComposeProject,
			"nodes":           top.Nodes,
			"healthy":         !noWait,
		})
		return schema.ExitPass
	}

	infof(g, "topology up (compose project %s)", top.ComposeProject)
	for _, n := range top.Nodes {
		if n.HostPort > 0 {
			infof(g, "  %-8s %s  http://%s:%d", n.ID, shortID(n.ContainerID), harness.ProbeHost, n.HostPort)
		} else {
			infof(g, "  %-8s %s  (no published port)", n.ID, shortID(n.ContainerID))
		}
	}
	if noWait {
		infof(g, "health probes skipped (--no-wait)")
	} else {
		infof(g, "all health probes passed")
	}
	infof(g, "run `thesis down` to tear it down")
	return schema.ExitPass
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	if id == "" {
		return "-"
	}
	return id
}

var _ = fmt.Sprintf
