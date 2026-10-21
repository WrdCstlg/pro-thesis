package control

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/WrdCstlg/pro-thesis/internal/recorder"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// The lock is checked once, before anything boots, against the oracle
// definitions on disk. The runner used to re-read those definitions at every
// world's ASSERT, so an edit made after the check passed (mid-run, or by a
// bisect step checking out another revision) was evaluated without the lock
// ever seeing it. The set is now built once when the runner is created and
// every world uses that set.
func TestTheOracleSetIsReadOnceAtStart(t *testing.T) {
	origInspect := inspectDockerState
	t.Cleanup(func() { inspectDockerState = origInspect })
	inspectDockerState = func(context.Context, string) (*dockerContainerState, error) {
		return nil, errors.New("stub backend has no docker containers")
	}
	cfg := worldReaderConfig(t)
	r, err := NewRunner(RunnerOptions{
		Config:     cfg,
		ProjectDir: t.TempDir(),
		Backend: &stubBackend{nodes: []recorder.NodeBinding{
			{ID: "kv-n1", ContainerID: "c1", HostPort: 18081, ContainerPort: 8080, Service: "kv-n1"},
			{ID: "kv-n2", ContainerID: "c2", HostPort: 18082, ContainerPort: 8080, Service: "kv-n2"},
			{ID: "kv-n3", ContainerID: "c3", HostPort: 18083, ContainerPort: 8080, Service: "kv-n3"},
		}},
		Driver:        finishedDriver(),
		Telemetry:     stubTelemetry{},
		LogCollector:  stubLogs{},
		ImageResolver: stubImages{},
		Quiet:         true,
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	// After the runner (and, in production, the lock check) has the set, the
	// definition on disk is replaced by one that does not parse.
	def := filepath.Join(cfg.Oracles.Dir, worldReaderOracle+".yaml")
	if err := os.WriteFile(def, []byte("version: not-an-oracle\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	verdict, exit, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if exit != schema.ExitPass {
		t.Fatalf("exit = %d (%s): the world read the definition again after the run started", exit, verdict.Verdict)
	}
}
