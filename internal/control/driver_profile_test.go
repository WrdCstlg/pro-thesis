package control

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/WrdCstlg/pro-thesis/internal/driver"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// ---------------------------------------------------------------------------
// The run profile and the driver profile are DIFFERENT namespaces
//
// `profiles.<name>.driver_profile` names an entry of `driver.profiles`. The
// directive's own sample uses smoke/gate/soak for both, and so did every profile
// in the reference fixture, so for three phases the runner could pass the RUN
// profile's name where the DRIVER profile's belonged and every test still
// passed. Phase 4 added a run profile called `search` whose driver profile is
// `linear`, and the defect surfaced immediately.
//
// What it did, measured on run r_2026_09_08_5c6b world 3:
//
//	driver.NewPlan returned "driver.profiles has no entry \"search\""
//	the error was DISCARDED, so no plan file was written
//	the workload was launched with --profile search and exited 5
//	NO history.jsonl was written
//	linearizable.kv and no_stuck_op reported INCONCLUSIVE ("cannot open the history")
//	no_crash, resource_return_to_baseline and availability_after_heal
//	  returned verdicts about a system NOTHING HAD DRIVEN
//
// That last line is the reason these tests exist. A world that reports on a
// system it never exercised is the failure this codebase refuses to ship, and it
// arrived here through a discarded error rather than through a weakened
// assertion.
// ---------------------------------------------------------------------------

// distinctProfileConfig is a config whose run profile and driver profile have
// DIFFERENT names: the case the fixture could not previously express.
func distinctProfileConfig(t *testing.T) *schema.Config {
	t.Helper()
	w := 1
	return &schema.Config{
		Version: schema.ConfigVersion,
		Name:    "driver-profile-test",
		Harness: schema.HarnessConfig{
			Backend: schema.BackendCompose,
			File:    "docker-compose.yaml",
			Nodes:   []schema.NodeConfig{{ID: "n1", Service: "kv"}},
		},
		Driver: schema.DriverConfig{
			Cmd: "echo test",
			Profiles: map[string]schema.DriverProfile{
				"linear": {Clients: 16, Ops: 60000, Mix: map[string]float64{"read": 0.5, "write": 0.5}},
			},
		},
		Profiles: map[string]schema.Profile{
			// The run profile is `search`; the driver profile is `linear`.
			"search": {
				Budget:        schema.Duration(60 * time.Second),
				Worlds:        &w,
				DriverProfile: "linear",
				Search:        true,
			},
		},
		Oracles:   schema.OraclesConfig{Builtin: []schema.BuiltinOracle{}},
		Artifacts: schema.ArtifactsConfig{Dir: t.TempDir()},
	}
}

func newDistinctProfileRunner(t *testing.T, drv Driver) *Runner {
	t.Helper()
	r, err := NewRunner(RunnerOptions{
		Config:        distinctProfileConfig(t),
		ProjectDir:    t.TempDir(),
		Profile:       "search",
		Backend:       &stubBackend{},
		Driver:        drv,
		Telemetry:     stubTelemetry{},
		OracleEngine:  stubOracles{},
		SteadyState:   noSteadyState{},
		LogCollector:  stubLogs{},
		ImageResolver: stubImages{},
		Stderr:        &strings.Builder{},
		Quiet:         true,
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	return r
}

// The driver must be told which WORKLOAD to run, not which RUN it belongs to.
func TestTheDriverIsGivenTheDriverProfileNotTheRunProfile(t *testing.T) {
	drv := finishedDriver()
	r := newDistinctProfileRunner(t, drv)

	if _, _, err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	reqs := drv.requests()
	if len(reqs) != 1 {
		t.Fatalf("expected one driver start, got %d", len(reqs))
	}
	if reqs[0].Profile != "linear" {
		t.Fatalf("DriverRequest.Profile = %q, want %q.\n"+
			"The run profile is `search` and its driver_profile is `linear`. Passing the run "+
			"profile launches the workload with a profile it cannot resolve, and it writes no "+
			"history — every history-based oracle then reports INCONCLUSIVE while the others "+
			"report on a system nothing drove.", reqs[0].Profile, "linear")
	}
}

// The plan file is the workload's contract. It must exist, and it must carry the
// driver profile's own numbers.
func TestTheDriverPlanCarriesTheDriverProfilesNumbers(t *testing.T) {
	drv := finishedDriver()
	r := newDistinctProfileRunner(t, drv)

	if _, _, err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	planPath := filepath.Join(r.RunDir(), WorldDirName(1), driver.PlanFileName)
	data, err := os.ReadFile(planPath)
	if err != nil {
		t.Fatalf("no driver plan was written at %s: %v.\n"+
			"NewPlan's error used to be discarded, so a world whose plan could not be built ran "+
			"a workload with no plan at all and produced no history.", planPath, err)
	}
	var plan struct {
		Profile string `json:"profile"`
		Clients int    `json:"clients"`
		Ops     int64  `json:"ops"`
	}
	if err := json.Unmarshal(data, &plan); err != nil {
		t.Fatalf("plan.json does not parse: %v", err)
	}
	if plan.Profile != "linear" {
		t.Fatalf("plan.profile = %q, want linear", plan.Profile)
	}
	if plan.Clients != 16 || plan.Ops != 60000 {
		t.Fatalf("plan = %d clients / %d ops, want the `linear` driver profile's 16 / 60000; "+
			"the plan was built from the wrong profile", plan.Clients, plan.Ops)
	}
}

// The world file records what was RUN. Recording the run profile there tells a
// Phase 5 replay to use a workload the world never used.
func TestTheWorldFileRecordsTheDriverProfile(t *testing.T) {
	drv := finishedDriver()
	r := newDistinctProfileRunner(t, drv)

	if _, _, err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(r.RunDir(), WorldDirName(1), "world.thesis"))
	if err != nil {
		t.Fatalf("read world file: %v", err)
	}
	var w struct {
		DriverProfile string `json:"driver_profile"`
	}
	if err := json.Unmarshal(data, &w); err != nil {
		t.Fatalf("world.thesis does not parse: %v", err)
	}
	if w.DriverProfile != "linear" {
		t.Fatalf("world.thesis driver_profile = %q, want linear. A replay reading this would "+
			"run a workload the world never ran.", w.DriverProfile)
	}
}

// A plan that cannot be built is a HARNESS ERROR, not a world that quietly runs
// an unconfigured workload. This is the assertion the discarded error removed.
func TestAWorldWhoseDriverPlanCannotBeBuiltIsAHarnessError(t *testing.T) {
	cfg := distinctProfileConfig(t)
	// Break the cross-reference AFTER construction, which is the only way to
	// reach the branch: NewRunner now refuses a config whose driver_profile does
	// not resolve, and Validate refuses it before that.
	r, err := NewRunner(RunnerOptions{
		Config:        cfg,
		ProjectDir:    t.TempDir(),
		Profile:       "search",
		Backend:       &stubBackend{},
		Driver:        finishedDriver(),
		Telemetry:     stubTelemetry{},
		OracleEngine:  stubOracles{},
		SteadyState:   noSteadyState{},
		LogCollector:  stubLogs{},
		ImageResolver: stubImages{},
		Stderr:        &strings.Builder{},
		Quiet:         true,
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	delete(cfg.Driver.Profiles, "linear")

	_, code, _ := r.Run(context.Background())
	if code == schema.ExitPass {
		t.Fatalf("a world whose driver plan could not be built reported PASS (exit 0). "+
			"It ran a workload with no plan and no history; every history-based oracle was "+
			"INCONCLUSIVE and the rest reported on a system nothing drove. got exit %d", code)
	}
}

// NewRunner refuses a config whose driver_profile does not resolve, rather than
// defaulting to the run profile's own name. Defaulting is exactly the
// coincidence that hid the defect for three phases.
func TestNewRunnerRefusesAnUnresolvableDriverProfile(t *testing.T) {
	cfg := distinctProfileConfig(t)
	p := cfg.Profiles["search"]
	p.DriverProfile = "nonesuch"
	cfg.Profiles["search"] = p

	_, err := NewRunner(RunnerOptions{
		Config:     cfg,
		ProjectDir: t.TempDir(),
		Profile:    "search",
		Backend:    &stubBackend{},
		Driver:     finishedDriver(),
		Quiet:      true,
	})
	if err == nil {
		t.Fatal("NewRunner accepted a profile whose driver_profile names no driver profile")
	}
	if !strings.Contains(err.Error(), "nonesuch") {
		t.Fatalf("the refusal does not name the offending value: %v", err)
	}
}
