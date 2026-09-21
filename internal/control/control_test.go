package control

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/WrdCstlg/pro-thesis/internal/harness"
	"github.com/WrdCstlg/pro-thesis/internal/recorder"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

func TestPhaseLogLinearity(t *testing.T) {
	clock := recorder.NewSimClock(time.Now())
	timeline := &recorder.Timeline{}
	pl, err := NewPhaseLog(clock, timeline, nil)
	if err != nil {
		t.Fatalf("NewPhaseLog error: %v", err)
	}

	if err := pl.Advance(schema.PhaseBoot); err != nil {
		t.Fatalf("Advance(BOOT) error: %v", err)
	}
	if err := pl.Advance(schema.PhaseSeed); err != nil {
		t.Fatalf("Advance(SEED) error: %v", err)
	}
	if err := pl.Advance(schema.PhaseDrive); err != nil {
		t.Fatalf("Advance(DRIVE) error: %v", err)
	}

	// Attempting to advance backwards must fail with ErrPhaseOutOfOrder
	err = pl.Advance(schema.PhaseSeed)
	if !errors.Is(err, ErrPhaseOutOfOrder) {
		t.Errorf("expected ErrPhaseOutOfOrder, got %v", err)
	}
}

func TestBudgetTracking(t *testing.T) {
	clock := recorder.NewSimClock(time.Now())
	b := Budget{
		Wall:   5 * time.Second,
		Worlds: 2,
	}
	tracker := NewBudgetTracker(b, clock)

	canStart, _ := tracker.MayStartWorld()
	if !canStart {
		t.Fatalf("expected to start world 1")
	}
	tracker.NoteWorldStarted()
	tracker.NoteWorldFinished()

	canStart, _ = tracker.MayStartWorld()
	if !canStart {
		t.Fatalf("expected to start world 2")
	}
	tracker.NoteWorldStarted()
	tracker.NoteWorldFinished()

	// Third world must be denied
	canStart, reason := tracker.MayStartWorld()
	if canStart {
		t.Errorf("expected world 3 to be denied by world count budget")
	}
	if reason == "" {
		t.Errorf("expected reason for budget exhaustion")
	}
}

func TestOutcomeRanking(t *testing.T) {
	if OutcomeViolation.Rank() <= OutcomePass.Rank() {
		t.Errorf("violation must outrank pass")
	}
	if OutcomeInconclusive.Rank() <= OutcomePass.Rank() {
		t.Errorf("inconclusive must outrank pass")
	}
	if OutcomeViolation.Rank() <= OutcomeInconclusive.Rank() {
		t.Errorf("violation must outrank inconclusive")
	}
}

// Mock backend to verify unconditional Down execution
type mockBackend struct {
	upCalled   bool
	downCalled bool
	failUp     bool
}

func (m *mockBackend) Name() schema.Backend { return schema.BackendCompose }
func (m *mockBackend) Up(ctx context.Context, req harness.UpRequest) (*recorder.Topology, error) {
	m.upCalled = true
	if m.failUp {
		return nil, errors.New("up simulated failure")
	}
	return &recorder.Topology{
		Schema:         recorder.TopologySchema,
		RunID:          req.RunID,
		Backend:        string(schema.BackendCompose),
		ComposeProject: "mock-proj",
		Nodes:          []recorder.NodeBinding{},
	}, nil
}
func (m *mockBackend) Down(ctx context.Context, top *recorder.Topology, opts harness.DownOptions) error {
	m.downCalled = true
	return nil
}

func TestRunnerTeardownGuarantee(t *testing.T) {
	ctx := context.Background()
	mockB := &mockBackend{failUp: true}

	cfg := &schema.Config{
		Version: schema.ConfigVersion,
		Name:    "test-run",
		Harness: schema.HarnessConfig{
			Backend: schema.BackendCompose,
			File:    "docker-compose.yaml",
		},
		Profiles: map[string]schema.Profile{
			"smoke": {
				Budget:        schema.Duration(30 * time.Second),
				Worlds:        func() *int { w := 1; return &w }(),
				DriverProfile: "smoke",
			},
		},
		Driver: schema.DriverConfig{
			Cmd: "echo test",
			Profiles: map[string]schema.DriverProfile{
				"smoke": {Clients: 1, Ops: 1},
			},
		},
		Artifacts: schema.ArtifactsConfig{
			Dir: t.TempDir(),
		},
	}

	runner, err := NewRunner(RunnerOptions{
		Config:        cfg,
		ProjectDir:    t.TempDir(),
		Backend:       mockB,
		ImageResolver: stubImages{},
		Quiet:         true,
	})
	if err != nil {
		t.Fatalf("NewRunner error: %v", err)
	}

	verdict, exitCode, err := runner.Run(ctx)
	if err != nil {
		t.Fatalf("unexpected runner fatal error: %v", err)
	}

	if !mockB.upCalled {
		t.Errorf("expected Up to be called")
	}
	if !mockB.downCalled {
		t.Errorf("INVARIANT VIOLATION: Down was not called after Up failed!")
	}
	if exitCode != schema.ExitInconclusive {
		t.Errorf("expected exit code 2 (INCONCLUSIVE) for harness failure, got %d (verdict %s)", exitCode, verdict.Verdict)
	}
}
