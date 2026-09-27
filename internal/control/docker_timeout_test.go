package control

import (
	"context"
	"errors"
	"os/exec"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/WrdCstlg/pro-thesis/internal/recorder"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// ---------------------------------------------------------------------------
// The container-state inspect at ASSERT is bounded, and reached through a seam
// (D-089).
//
// buildOracleInput runs `docker inspect` on every bound container. A sick
// daemon has been measured taking 32 s to answer `docker inspect` (OQ-067), and
// this call had no bound of its own: D-073 bounded only image resolution. Two
// properties are pinned here:
//   - buildOracleInput goes through the inspectDockerState seam, so a unit test
//     can keep a stub-backed world away from the daemon.
//   - queryDockerState gives up at dockerInspectTimeout, whatever the command
//     does. On a timeout the node is recorded as unobserved.
// ---------------------------------------------------------------------------

// TestBuildOracleInputCallsInspectDockerStateSeam verifies that
// buildOracleInput uses the inspectDockerState package variable, not a
// direct call to queryDockerState. This is the property that allows unit
// tests to prevent leaks to the Docker daemon.
func TestBuildOracleInputCallsInspectDockerStateSeam(t *testing.T) {
	origInspect := inspectDockerState
	t.Cleanup(func() { inspectDockerState = origInspect })

	var callCount int64
	inspectDockerState = func(_ context.Context, container string) (*dockerContainerState, error) {
		atomic.AddInt64(&callCount, 1)
		return nil, errors.New("seam test: not a real daemon")
	}

	// Build a minimal EvalRequest with nodes that have container IDs,
	// which is what triggers the inspectDockerState call in buildOracleInput.
	one := 1
	cfg := &schema.Config{
		Harness: schema.HarnessConfig{
			Nodes: []schema.NodeConfig{
				{ID: "n1", Service: "svc1"},
				{ID: "n2", Service: "svc2"},
			},
		},
		Profiles: map[string]schema.Profile{
			"smoke": {Worlds: &one, Budget: schema.Duration(60 * time.Second)},
		},
	}

	topology := &recorder.Topology{
		Nodes: []recorder.NodeBinding{
			{ID: "n1", ContainerID: "c1", HostPort: 18081, ContainerPort: 8080, Service: "svc1"},
			{ID: "n2", ContainerID: "c2", HostPort: 18082, ContainerPort: 8080, Service: "svc2"},
		},
	}

	tmpDir := t.TempDir()
	paths := WorldPaths{
		History:   tmpDir + "/history.jsonl",
		Telemetry: tmpDir + "/telemetry.jsonl",
	}

	clock := recorder.NewSimClock(time.Now())
	timeline := &recorder.Timeline{}

	req := EvalRequest{
		Config:   cfg,
		Topology: topology,
		Paths:    paths,
		Clock:    clock,
		Timeline: timeline,
	}

	// buildOracleInput may return an error for missing files, but the seam
	// calls happen BEFORE file I/O. The call count is what matters.
	_, _ = buildOracleInput(context.Background(), req)

	if n := atomic.LoadInt64(&callCount); n == 0 {
		t.Fatal("inspectDockerState was never called by buildOracleInput — " +
			"the seam is not wired correctly")
	}
	if n := atomic.LoadInt64(&callCount); n != 2 {
		t.Errorf("inspectDockerState called %d times, want 2 (one per node with a container ID)", n)
	}
}

// TestAnUnansweredInspectIsCutOffAtTheBound pins the bound itself. The command
// is replaced by one that does not answer for 30 s; queryDockerState must give
// up at dockerInspectTimeout rather than wait for it. Mutation: without the
// context timeout this test fails at its 10 s guard.
//
// It replaces a test that ran a stub world and timed it. That world used the
// stub oracle engine, which never calls buildOracleInput, so it never reached
// the inspect at all and passed with the bound removed.
func TestAnUnansweredInspectIsCutOffAtTheBound(t *testing.T) {
	origCmd, origTimeout := dockerInspectCommand, dockerInspectTimeout
	t.Cleanup(func() { dockerInspectCommand, dockerInspectTimeout = origCmd, origTimeout })
	dockerInspectTimeout = 300 * time.Millisecond
	dockerInspectCommand = func(ctx context.Context, _ string) *exec.Cmd {
		if runtime.GOOS == "windows" {
			return exec.CommandContext(ctx, "ping", "-n", "30", "127.0.0.1")
		}
		return exec.CommandContext(ctx, "sleep", "30")
	}

	type outcome struct {
		err  error
		took time.Duration
	}
	done := make(chan outcome, 1)
	start := time.Now()
	go func() {
		_, err := queryDockerState(context.Background(), "c1")
		done <- outcome{err, time.Since(start)}
	}()

	select {
	case o := <-done:
		if o.err == nil {
			t.Fatal("an inspect that never answered returned no error")
		}
		if o.took > 5*time.Second {
			t.Fatalf("queryDockerState gave up after %v; its bound is %v", o.took, dockerInspectTimeout)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("queryDockerState did not return within 10s of a %v bound: an unanswered "+
			"docker inspect would spend the world's budget at ASSERT (OQ-067)", dockerInspectTimeout)
	}
}
