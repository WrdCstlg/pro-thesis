package control

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/WrdCstlg/pro-thesis/internal/harness"
	"github.com/WrdCstlg/pro-thesis/internal/perturber"
	"github.com/WrdCstlg/pro-thesis/internal/recorder"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// ---------------------------------------------------------------------------
// stubs for the seams a world crosses, so PERTURB can be exercised without a
// Docker daemon
// ---------------------------------------------------------------------------

type stubBackend struct {
	mu         sync.Mutex
	nodes      []recorder.NodeBinding
	downCalled bool
	// upProjects and upEnv record what each world asked for, so a parallel test
	// can assert the worlds were actually isolated rather than merely
	// simultaneous.
	upProjects  []string
	upOverlays  []string
	upEnvs      [][]string
	downErr     error
	downErrFor  map[string]error
	downProject []string
	// upErr makes BOOT fail, so the teardown path can be exercised for a world
	// that never got a topology.
	upErr error
}

func (b *stubBackend) Name() schema.Backend { return schema.BackendCompose }

func (b *stubBackend) Up(_ context.Context, req harness.UpRequest) (*recorder.Topology, error) {
	project := req.ProjectName
	if project == "" {
		project = "stub-proj"
	}
	b.mu.Lock()
	upErr := b.upErr
	if upErr == nil {
		b.upProjects = append(b.upProjects, project)
		b.upOverlays = append(b.upOverlays, req.OverlayPath)
		b.upEnvs = append(b.upEnvs, append([]string(nil), req.Env...))
	}
	b.mu.Unlock()
	if upErr != nil {
		return nil, upErr
	}
	return &recorder.Topology{
		Schema:         recorder.TopologySchema,
		RunID:          req.RunID,
		Backend:        string(schema.BackendCompose),
		ComposeProject: project,
		Nodes:          b.nodes,
	}, nil
}

func (b *stubBackend) Down(_ context.Context, top *recorder.Topology, _ harness.DownOptions) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.downCalled = true
	project := ""
	if top != nil {
		project = top.ComposeProject
	}
	b.downProject = append(b.downProject, project)
	if err, ok := b.downErrFor[project]; ok {
		return err
	}
	return b.downErr
}

func (b *stubBackend) projects() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.upProjects...)
}

// stubDriverHandle is the workload seam. It models the three behaviours the
// drain has to distinguish: a driver that has already finished, one that drains
// when asked, and one that refuses.
type stubDriverHandle struct {
	done chan struct{}
	// drainable closes done when Drain is called, which is what a cooperating
	// driver does. When false, Drain is a no-op and the deadline must expire.
	drainable bool
	// unsupported reports no control channel at all.
	unsupported bool

	mu        sync.Mutex
	drainRep  DrainReport
	stopped   bool
	drainCall int
}

func (h *stubDriverHandle) Done() <-chan struct{} { return h.done }

func (h *stubDriverHandle) Drain(ctx context.Context, deadline time.Duration) DrainReport {
	h.mu.Lock()
	h.drainCall++
	h.mu.Unlock()

	rep := DrainReport{Deadline: deadline}
	if deadline <= 0 {
		rep.Outcome = DrainNotAttempted
		return h.noteDrain(rep)
	}
	select {
	case <-h.done:
		rep.Outcome = DrainCompleted
		return h.noteDrain(rep)
	default:
	}
	if h.unsupported {
		rep.Outcome = DrainUnsupported
		rep.Err = errors.New("stub: no control channel")
		return h.noteDrain(rep)
	}
	if h.drainable {
		close(h.done)
		rep.Outcome = DrainCompleted
		return h.noteDrain(rep)
	}
	start := time.Now()
	timer := time.NewTimer(deadline)
	defer timer.Stop()
	select {
	case <-h.done:
		rep.Outcome = DrainCompleted
	case <-timer.C:
		rep.Outcome = DrainDeadlineExceeded
		rep.Err = errors.New("stub: the driver ignored the drain request")
	case <-ctx.Done():
		rep.Outcome = DrainCanceled
		rep.Err = ctx.Err()
	}
	rep.Waited = time.Since(start)
	return h.noteDrain(rep)
}

func (h *stubDriverHandle) noteDrain(rep DrainReport) DrainReport {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.drainRep = rep
	return rep
}

func (h *stubDriverHandle) Stop(context.Context) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.stopped = true
	select {
	case <-h.done:
	default:
		close(h.done)
	}
	return nil
}

func (h *stubDriverHandle) Result() DriverResult {
	h.mu.Lock()
	defer h.mu.Unlock()
	return DriverResult{Started: true, Stopped: h.stopped, Drain: h.drainRep}
}

type stubDriver struct {
	// finished makes Start hand back a workload that has already exited, which
	// is the Phase 1 stub's behaviour and what most tests want.
	finished    bool
	drainable   bool
	unsupported bool

	mu      sync.Mutex
	handles []*stubDriverHandle
	reqs    []DriverRequest
}

func (d *stubDriver) Start(_ context.Context, req DriverRequest) (DriverHandle, error) {
	ch := make(chan struct{})
	if d.finished {
		close(ch)
	}
	h := &stubDriverHandle{done: ch, drainable: d.drainable, unsupported: d.unsupported}
	d.mu.Lock()
	d.handles = append(d.handles, h)
	d.reqs = append(d.reqs, req)
	d.mu.Unlock()
	return h, nil
}

func (d *stubDriver) requests() []DriverRequest {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]DriverRequest(nil), d.reqs...)
}

// finishedDriver is the Phase 1 stub: a workload that has already exited.
func finishedDriver() *stubDriver { return &stubDriver{finished: true} }

type stubTelemetryHandle struct{ path string }

func (h *stubTelemetryHandle) Stop(context.Context) error { return nil }
func (h *stubTelemetryHandle) Path() string               { return h.path }

type stubTelemetry struct{}

func (stubTelemetry) Start(_ context.Context, req TelemetryRequest) (TelemetryHandle, error) {
	return &stubTelemetryHandle{path: req.OutputPath}, nil
}

type stubOracles struct{}

func (stubOracles) Evaluate(context.Context, EvalRequest) ([]OracleResult, error) {
	return []OracleResult{{Output: schema.OracleOutput{Oracle: "stub", Status: schema.StatusOK}}}, nil
}

type stubLogs struct{}

func (stubLogs) Collect(context.Context, LogRequest) ([]CollectedLog, error) { return nil, nil }

// controlFakeInjector is a perturber.Injector that records its calls.
type controlFakeInjector struct {
	mu       sync.Mutex
	calls    []string
	live     map[string]bool
	residues []perturber.Residue
}

func newControlFakeInjector() *controlFakeInjector {
	return &controlFakeInjector{live: map[string]bool{}}
}

func (f *controlFakeInjector) Kinds() []schema.FaultKind {
	return []schema.FaultKind{schema.FaultProcPause, schema.FaultNetPartition}
}

func (f *controlFakeInjector) Inject(_ context.Context, req perturber.InjectRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "inject "+req.FaultID+" "+strings.Join(nodeIDsOf(req.Nodes), ","))
	f.live[req.FaultID] = true
	return nil
}

func (f *controlFakeInjector) Withdraw(_ context.Context, req perturber.WithdrawRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "withdraw "+req.FaultID)
	delete(f.live, req.FaultID)
	return nil
}

func (f *controlFakeInjector) VerifyClean(context.Context, perturber.VerifyRequest) ([]perturber.Residue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]perturber.Residue(nil), f.residues...), nil
}

func (f *controlFakeInjector) log() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func nodeIDsOf(nodes []perturber.Node) []string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, n.ID)
	}
	return out
}

// ---------------------------------------------------------------------------

func perturbTestConfig(t *testing.T) *schema.Config {
	t.Helper()
	one := 1
	return &schema.Config{
		Version: schema.ConfigVersion,
		Name:    "perturb-wiring",
		Harness: schema.HarnessConfig{
			Backend: schema.BackendCompose,
			File:    "docker-compose.yaml",
			Nodes: []schema.NodeConfig{
				{ID: "kv-n1", Service: "kv", ComposeService: "kv-n1"},
				{ID: "kv-n2", Service: "kv", ComposeService: "kv-n2"},
				{ID: "kv-n3", Service: "kv", ComposeService: "kv-n3"},
			},
		},
		Driver: schema.DriverConfig{
			Cmd:      "echo test",
			Profiles: map[string]schema.DriverProfile{"smoke": {Clients: 1, Ops: 1}},
		},
		Perturber: schema.PerturberConfig{
			Budget:      schema.PerturberBudget{MaxConcurrentFaults: 3, MaxFaultsPerWorld: 24},
			Constraints: []string{"never partition more than minority of kv"},
		},
		Profiles: map[string]schema.Profile{
			"smoke": {Budget: schema.Duration(60 * time.Second), Worlds: &one, DriverProfile: "smoke"},
		},
		Artifacts: schema.ArtifactsConfig{Dir: t.TempDir()},
	}
}

func newPerturbTestRunner(t *testing.T, cfg *schema.Config, faults []string, injs ...perturber.Injector) (*Runner, *stubBackend) {
	t.Helper()
	backend := &stubBackend{nodes: []recorder.NodeBinding{
		{ID: "kv-n1", ContainerID: "c1", HostPort: 18081, ContainerPort: 8080, Service: "kv-n1"},
		{ID: "kv-n2", ContainerID: "c2", HostPort: 18082, ContainerPort: 8080, Service: "kv-n2"},
		{ID: "kv-n3", ContainerID: "c3", HostPort: 18083, ContainerPort: 8080, Service: "kv-n3"},
	}}
	r, err := NewRunner(RunnerOptions{
		Config:        cfg,
		ProjectDir:    t.TempDir(),
		Backend:       backend,
		Driver:        finishedDriver(),
		Telemetry:     stubTelemetry{},
		OracleEngine:  stubOracles{},
		LogCollector:  stubLogs{},
		ImageResolver: stubImages{},
		Faults:        faults,
		Injectors:     injs,
		Quiet:         true,
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	return r, backend
}

// PERTURB no longer runs an empty schedule: it compiles the world's planned
// faults, injects and withdraws them on the virtual clock, and records what
// actually happened into the world file.
func TestPerturbRunsARealScheduleAndRecordsIt(t *testing.T) {
	cfg := perturbTestConfig(t)
	inj := newControlFakeInjector()
	r, backend := newPerturbTestRunner(t, cfg, []string{
		"proc.pause(kv-n1)@10..60",
		"net.partition(minority(kv))@20..40",
	}, inj)

	verdict, exit, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if exit != schema.ExitPass {
		t.Fatalf("exit = %d (verdict %s), want 0", exit, verdict.Verdict)
	}
	if !backend.downCalled {
		t.Error("TEARDOWN did not run")
	}

	got := inj.log()
	if len(got) != 4 {
		t.Fatalf("injector calls = %v, want two injections and two withdrawals", got)
	}
	if !strings.HasPrefix(got[0], "inject f001 kv-n1") {
		t.Errorf("first call = %q, want the pause on kv-n1", got[0])
	}
	if !strings.HasPrefix(got[1], "inject f002 kv-n") {
		t.Errorf("second call = %q, want the partition", got[1])
	}
	if got[2] != "withdraw f002" || got[3] != "withdraw f001" {
		t.Errorf("withdrawals = %v, want f002 then f001", got[2:])
	}

	// The world file carries both halves of the schedule, and the dynamic
	// quorum target is pinned to the node it actually bound to.
	worldPath := filepath.Join(r.runDir, "world-0001", "world.thesis")
	data, readErr := os.ReadFile(worldPath)
	if readErr != nil {
		t.Fatalf("read world file: %v", readErr)
	}
	w, uErr := schema.UnmarshalWorld(data)
	if uErr != nil {
		t.Fatalf("world file is not canonical: %v", uErr)
	}
	if len(w.FaultSchedule.Planned) != 2 {
		t.Errorf("planned = %v, want two faults", w.FaultSchedule.Planned)
	}
	if len(w.FaultSchedule.Realized) != 2 {
		t.Fatalf("realized = %+v, want two entries", w.FaultSchedule.Realized)
	}
	for _, rf := range w.FaultSchedule.Realized {
		if len(rf.Nodes) == 0 {
			t.Errorf("realized entry %q bound to no node", rf.Fault)
		}
		if _, perr := schema.ParseFault(rf.Resolved); perr != nil {
			t.Errorf("realized.resolved %q does not parse: %v", rf.Resolved, perr)
		}
	}
	if q := w.FaultSchedule.Realized[1]; !strings.HasPrefix(q.Resolved, "net.partition("+q.Nodes[0]+")") {
		t.Errorf("minority(kv) was not pinned: resolved=%q nodes=%v", q.Resolved, q.Nodes)
	}
}

// A world whose faults have no mechanism must fail LOUDLY. Executing it and
// reporting PASS would be a verdict about a system nobody perturbed.
func TestPerturbWithNoInjectorIsAHarnessError(t *testing.T) {
	cfg := perturbTestConfig(t)
	r, backend := newPerturbTestRunner(t, cfg, []string{"proc.pause(kv-n1)@10..60"})

	verdict, exit, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if exit != schema.ExitInconclusive {
		t.Errorf("exit = %d (verdict %s), want 2 for a fault with no mechanism", exit, verdict.Verdict)
	}
	if !backend.downCalled {
		t.Error("TEARDOWN must still run")
	}
}

// Directive 4.3's Critical Guarantee: a residual fault is a HARNESS ERROR, not
// an oracle violation and never a pass.
func TestPerturbResidualFaultIsAHarnessError(t *testing.T) {
	cfg := perturbTestConfig(t)
	inj := newControlFakeInjector()
	inj.residues = []perturber.Residue{{
		NodeID: "kv-n1", Mechanism: "iptables", FaultID: "f001",
		Detail: `-A INPUT -j DROP -m comment --comment "thesis:r:f001"`,
	}}
	r, backend := newPerturbTestRunner(t, cfg, []string{"proc.pause(kv-n1)@10..40"}, inj)

	verdict, exit, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if exit != schema.ExitInconclusive {
		t.Errorf("exit = %d (verdict %s), want 2 for residual fault state", exit, verdict.Verdict)
	}
	if !backend.downCalled {
		t.Error("TEARDOWN must still run")
	}
}

// A schedule that violates a safety constraint is refused before anything is
// injected. A silently ignored constraint would let a run partition a majority
// and then report the resulting unavailability as a bug in the target.
func TestPerturbRefusesAConstraintViolatingSchedule(t *testing.T) {
	cfg := perturbTestConfig(t)
	inj := newControlFakeInjector()
	r, _ := newPerturbTestRunner(t, cfg, []string{"net.partition(majority(kv))@10..40"}, inj)

	_, exit, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if exit != schema.ExitInconclusive {
		t.Errorf("exit = %d, want 2", exit)
	}
	if calls := inj.log(); len(calls) != 0 {
		t.Errorf("a refused schedule injected %v", calls)
	}
}

// A no-fault world keeps working exactly as it did in Phase 1.
func TestPerturbEmptyScheduleStillPasses(t *testing.T) {
	cfg := perturbTestConfig(t)
	r, backend := newPerturbTestRunner(t, cfg, nil)

	verdict, exit, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if exit != schema.ExitPass {
		t.Errorf("exit = %d (verdict %s), want 0", exit, verdict.Verdict)
	}
	if !backend.downCalled {
		t.Error("TEARDOWN did not run")
	}
}

func TestPerturbHelpersAreNilSafe(t *testing.T) {
	if rep, err := healPerturbation(context.Background(), nil); err != nil || len(rep.Residues) != 0 {
		t.Errorf("healPerturbation(nil) = %+v, %v", rep, err)
	}
	if realizedOf(nil) != nil {
		t.Error("realizedOf(nil) must be nil: a world that never reached PERTURB has no realized schedule")
	}
	if plannedOf(nil) != nil {
		t.Error("plannedOf(nil) must be nil")
	}
	if realizedOf(&perturbation{}) != nil {
		t.Error("realizedOf with no executor must be nil")
	}
}

func TestWaitOutDriveWindow(t *testing.T) {
	done := make(chan struct{})
	close(done)
	if err := waitOutDriveWindow(context.Background(), done, time.Hour); err != nil {
		t.Errorf("a finished driver must end the window immediately: %v", err)
	}

	start := time.Now()
	if err := waitOutDriveWindow(context.Background(), make(chan struct{}), 20*time.Millisecond); err != nil {
		t.Errorf("the window must expire cleanly: %v", err)
	}
	if time.Since(start) < 15*time.Millisecond {
		t.Error("the window returned before it elapsed")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := waitOutDriveWindow(ctx, make(chan struct{}), time.Hour); !errors.Is(err, context.Canceled) {
		t.Errorf("cancellation must be reported: %v", err)
	}
}
