package main

import (
	"context"
	"encoding/json"
	"os"

	"github.com/WrdCstlg/pro-thesis/internal/harness"
	"github.com/WrdCstlg/pro-thesis/internal/recorder"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

func cmdDown(ctx context.Context, g globals, args []string) schema.ExitCode {
	var keepVolumes bool
	var runID string
	for i := 0; i < len(args); i++ {
		switch a := args[i]; a {
		case "--keep-volumes":
			keepVolumes = true
		case "--run":
			if i+1 >= len(args) {
				errorf("down: --run needs a run id")
				return schema.ExitConfigError
			}
			i++
			runID = args[i]
		default:
			errorf("down: unknown flag %q", a)
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
		errorf("%v", err)
		return schema.ExitConfigError
	}

	runsDir := recorder.RunsDir(projectDir)

	if runID == "" {
		runID, err = recorder.ReadCurrentRun(runsDir)
		if err != nil || runID == "" {
			// Nothing is up. `down` is idempotent by contract, so this is
			// success, not failure: an agent loop that always calls `down`
			// before `up` must not be punished for a clean slate.
			infof(g, "nothing is up")
			if g.json {
				_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"run_id": "", "torn_down": false})
			}
			return schema.ExitPass
		}
	}

	top, err := recorder.ReadTopologyForRun(runsDir, runID)
	if err != nil {
		// The topology record is missing or unreadable, so the compose project
		// name is unknown and there is nothing safe to address. Say so
		// precisely rather than tearing down a guess.
		errorf("run %s: cannot read topology record: %v", runID, err)
		errorf("       without it the compose project name is unknown; "+
			"remove %s/%s by hand if the topology is already gone",
			runsDir, recorder.CurrentPointer)
		return schema.ExitInconclusive
	}

	downErr := backend.Down(ctx, top, harness.DownOptions{
		Volumes: !keepVolumes,
		Timeout: cfg.Harness.SteadyState.Timeout,
	})

	// Finalize the bundle and clear the pointer even if teardown reported
	// residue: leaving CURRENT pointing at a half-destroyed run would make the
	// next `up` refuse to start with no way forward.
	if b, err := recorder.OpenBundleAt(runsDir, runID); err == nil {
		_ = b.Close()
	}
	if err := recorder.ClearCurrentRun(runsDir); err != nil {
		errorf("clear current run pointer: %v", err)
		return schema.ExitInconclusive
	}

	if downErr != nil {
		errorf("down: %v", downErr)
		return schema.ExitInconclusive
	}

	if g.json {
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{
			"run_id":          runID,
			"compose_project": top.ComposeProject,
			"torn_down":       true,
		})
		return schema.ExitPass
	}
	infof(g, "run %s torn down (compose project %s)", runID, top.ComposeProject)
	return schema.ExitPass
}
