package control

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/WrdCstlg/pro-thesis/internal/perturber"
	"github.com/WrdCstlg/pro-thesis/internal/recorder"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// stubImages is the resolver every unit-world constructor uses. A stub backend
// binds invented container ids (c1, c2, c3) that no daemon has ever heard of;
// asking one about them is not a test of anything, and on a sick daemon it is a
// 32-second stall per world (OQ-067). Answering "none resolved" is exactly what
// a real resolver would honestly say about containers that do not exist.
type stubImages struct{}

func (stubImages) Resolve(context.Context, []recorder.NodeBinding, []schema.NodeConfig) ([]schema.SUTImage, error) {
	return nil, nil
}

// recordingImages remembers it was asked and answers with a fixed fleet, so a
// test can prove the runner's provenance step goes through the seam and lands
// in world.thesis.
type recordingImages struct {
	mu    sync.Mutex
	calls int
	fleet []schema.SUTImage
}

func (s *recordingImages) Resolve(context.Context, []recorder.NodeBinding, []schema.NodeConfig) ([]schema.SUTImage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	return append([]schema.SUTImage(nil), s.fleet...), nil
}

func (s *recordingImages) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// The BOOT-time provenance step must go through the ImageResolver seam and its
// answer must reach the world file. Before D-073 the runner called
// harness.ResolveImages directly, so every unit world asked the real Docker
// daemon about c1, c2 and c3: harmless while the daemon failed fast, and a
// BUDGET_EXHAUSTED for two of these tests on the day it took 32 s to say no
// (OQ-067).
//
// Mutation: restore the direct call at the BOOT step and this fails on
// count() == 0, because the seam is then decoration the runner walks past.
func TestSUTImagesComeThroughTheResolverSeam(t *testing.T) {
	cfg := perturbTestConfig(t)
	backend := &stubBackend{nodes: []recorder.NodeBinding{
		{ID: "kv-n1", ContainerID: "c1", HostPort: 18081, ContainerPort: 8080, Service: "kv-n1"},
		{ID: "kv-n2", ContainerID: "c2", HostPort: 18082, ContainerPort: 8080, Service: "kv-n2"},
		{ID: "kv-n3", ContainerID: "c3", HostPort: 18083, ContainerPort: 8080, Service: "kv-n3"},
	}}
	fleet := []schema.SUTImage{{Service: "kv", Image: "prothesis/kvfixture:buggy", Digest: "sha256:through-the-seam"}}
	res := &recordingImages{fleet: fleet}

	r, err := NewRunner(RunnerOptions{
		Config:        cfg,
		ProjectDir:    t.TempDir(),
		Backend:       backend,
		Driver:        finishedDriver(),
		Telemetry:     stubTelemetry{},
		OracleEngine:  stubOracles{},
		LogCollector:  stubLogs{},
		ImageResolver: res,
		Faults:        []string{"proc.pause(kv-n1)@10..60"},
		Injectors:     []perturber.Injector{newControlFakeInjector()},
		Quiet:         true,
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	if _, _, err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if res.count() == 0 {
		t.Fatal("the runner never asked the ImageResolver: provenance is being resolved somewhere the seam does not reach, so the seam protects nothing")
	}

	data, err := os.ReadFile(filepath.Join(r.runDir, "world-0001", "world.thesis"))
	if err != nil {
		t.Fatalf("read world file: %v", err)
	}
	w, err := schema.UnmarshalWorld(data)
	if err != nil {
		t.Fatalf("world file is not canonical: %v", err)
	}
	if len(w.SUT.Images) != 1 || w.SUT.Images[0] != fleet[0] {
		t.Errorf("world.thesis sut.images = %+v, want exactly what the resolver answered: %+v", w.SUT.Images, fleet)
	}
}

// A caller that says nothing gets the daemon. `thesis run`, `search`, `replay`,
// `regress` and `shrink` all construct their runner without naming a resolver,
// so this default IS production's provenance: if it ever became a stub, every
// live world would record sut.images as [] and no stub-driven test would
// notice, because those tests name their resolver explicitly. This one names
// none. Mutation: remove the default so the field stays nil, and this fails.
func TestNewRunnerResolvesImagesFromTheDaemonByDefault(t *testing.T) {
	r, err := NewRunner(RunnerOptions{
		Config:       perturbTestConfig(t),
		ProjectDir:   t.TempDir(),
		Backend:      &stubBackend{},
		Driver:       finishedDriver(),
		Telemetry:    stubTelemetry{},
		OracleEngine: stubOracles{},
		LogCollector: stubLogs{},
		Quiet:        true,
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	if _, ok := r.imageResolver.(dockerImageResolver); !ok {
		t.Fatalf("with no ImageResolver named, NewRunner installed %T; production provenance depends on this being the daemon-backed resolver", r.imageResolver)
	}
}

// erroringImages is a resolver that cannot reach its daemon. It exists to pin
// the promise D-073 repeats from D-070 (a resolution failure costs the world
// its provenance and never its execution) which no other stub can, because
// none of them can fail.
type erroringImages struct{}

func (erroringImages) Resolve(context.Context, []recorder.NodeBinding, []schema.NodeConfig) ([]schema.SUTImage, error) {
	return nil, errors.New("daemon unreachable (stub)")
}

// A resolver failure must cost the world its provenance and nothing else: the
// world still runs to its verdict, world.thesis records sut.images as [], and
// the cause is printed where an operator sees it. Before this test nothing in
// the tree exercised that branch: every other resolver stub answers nil, nil.
// Mutation: at the BOOT call site, return the error instead of warning, and this
// fails on the exit code.
func TestAResolverFailureCostsProvenanceNotExecution(t *testing.T) {
	cfg := perturbTestConfig(t)
	backend := &stubBackend{nodes: []recorder.NodeBinding{
		{ID: "kv-n1", ContainerID: "c1", HostPort: 18081, ContainerPort: 8080, Service: "kv-n1"},
		{ID: "kv-n2", ContainerID: "c2", HostPort: 18082, ContainerPort: 8080, Service: "kv-n2"},
		{ID: "kv-n3", ContainerID: "c3", HostPort: 18083, ContainerPort: 8080, Service: "kv-n3"},
	}}
	var stderr strings.Builder
	r, err := NewRunner(RunnerOptions{
		Config:        cfg,
		ProjectDir:    t.TempDir(),
		Backend:       backend,
		Driver:        finishedDriver(),
		Telemetry:     stubTelemetry{},
		OracleEngine:  stubOracles{},
		LogCollector:  stubLogs{},
		ImageResolver: erroringImages{},
		Faults:        []string{"proc.pause(kv-n1)@10..60"},
		Injectors:     []perturber.Injector{newControlFakeInjector()},
		Stderr:        &stderr,
		Quiet:         true,
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	verdict, exit, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if exit != schema.ExitPass {
		t.Fatalf("exit = %d (verdict %s), want 0: a provenance failure must not end the world", exit, verdict.Verdict)
	}
	if !strings.Contains(stderr.String(), "image digest resolution failed") {
		t.Errorf("the failure was not reported; stderr:\n%s", stderr.String())
	}

	data, err := os.ReadFile(filepath.Join(r.runDir, "world-0001", "world.thesis"))
	if err != nil {
		t.Fatalf("read world file: %v", err)
	}
	w, err := schema.UnmarshalWorld(data)
	if err != nil {
		t.Fatalf("world file is not canonical: %v", err)
	}
	if len(w.SUT.Images) != 0 {
		t.Errorf("world.thesis sut.images = %+v, want [] when resolution failed", w.SUT.Images)
	}
}
