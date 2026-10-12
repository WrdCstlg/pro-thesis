package control

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/WrdCstlg/pro-thesis/internal/perturber"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

func faultedWorldConfig(t *testing.T, profileFaults []string) *schema.Config {
	t.Helper()
	cfg := drainWorldConfig(t)
	cfg.Perturber.Budget = schema.PerturberBudget{MaxConcurrentFaults: 2, MaxFaultsPerWorld: 10}
	cfg.Perturber.Allow = []schema.FaultKind{schema.FaultNetPartition, schema.FaultProcKill}
	pr := cfg.Profiles["smoke"]
	pr.Faults = append([]string(nil), profileFaults...)
	cfg.Profiles["smoke"] = pr
	return cfg
}

func newFaultedRunner(t *testing.T, cfg *schema.Config, faults []string, supplied bool, stderr *strings.Builder) *Runner {
	t.Helper()
	r, err := NewRunner(RunnerOptions{
		Config:         cfg,
		ProjectDir:     t.TempDir(),
		Backend:        &stubBackend{},
		Driver:         &stubDriver{drainable: true},
		Telemetry:      stubTelemetry{},
		OracleEngine:   stubOracles{},
		SteadyState:    noSteadyState{},
		LogCollector:   stubLogs{},
		ImageResolver:  stubImages{},
		Injectors:      []perturber.Injector{newControlFakeInjector()},
		OracleLock:     schema.OracleLock{Status: schema.LockOK, ManifestSHA: "sha256:test"},
		DrainDeadline:  2 * time.Second,
		Stderr:         stderr,
		Quiet:          false,
		Faults:         faults,
		FaultsSupplied: supplied,
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	return r
}

func TestRunInjectsTheProfileFaultSchedule(t *testing.T) {
	schedule := []string{"net.partition(n1)@1000..2000"}
	cfg := faultedWorldConfig(t, schedule)
	var errOut strings.Builder
	r := newFaultedRunner(t, cfg, nil, false, &errOut)

	if len(r.faults) != 1 || r.faults[0] != schedule[0] {
		t.Fatalf("r.faults = %v, want %v", r.faults, schedule)
	}
	if r.faultsNarrowed {
		t.Fatal("defaulting to the profile schedule must not mark the run narrowed")
	}
}

func TestAnArgvScheduleEqualToTheProfileIsNotNarrowed(t *testing.T) {
	schedule := []string{"net.partition(n1)@1000..2000"}
	cfg := faultedWorldConfig(t, schedule)
	var errOut strings.Builder
	r := newFaultedRunner(t, cfg, schedule, true, &errOut)

	if r.faultsNarrowed {
		t.Fatal("an argv schedule equal to the profile must not mark the run narrowed")
	}

	verdict, code, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if verdict.Budget.Narrowed {
		t.Fatal("verdict.Budget.Narrowed = true, want false")
	}
	if code != schema.ExitPass {
		t.Fatalf("exit code = %d, want %d; stderr:\n%s", code, schema.ExitPass, errOut.String())
	}
}

func TestAnArgvScheduleThatDiffersFromTheProfileCannotPass(t *testing.T) {
	schedule := []string{"net.partition(n1)@1000..2000"}
	cfg := faultedWorldConfig(t, schedule)
	var errOut strings.Builder
	different := []string{"proc.kill(n1)@1000..2000"}
	r := newFaultedRunner(t, cfg, different, true, &errOut)

	if !r.faultsNarrowed {
		t.Fatal("an argv schedule differing from the profile must mark the run narrowed")
	}

	verdict, code, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !verdict.Budget.Narrowed {
		t.Fatal("verdict.Budget.Narrowed = false, want true")
	}
	if verdict.Verdict != schema.VerdictInconclusive || code != schema.ExitInconclusive {
		t.Fatalf("verdict = %s (exit %d), want inconclusive (exit %d)",
			verdict.Verdict, code, schema.ExitInconclusive)
	}
	if verdict.OracleLock.Status != schema.LockBypassed {
		t.Fatalf("lock status = %s, want bypassed", verdict.OracleLock.Status)
	}
}

func TestNoFaultsOnAProfileWithFaultsCannotPass(t *testing.T) {
	schedule := []string{"net.partition(n1)@1000..2000"}
	cfg := faultedWorldConfig(t, schedule)
	var errOut strings.Builder
	r := newFaultedRunner(t, cfg, nil, true, &errOut)

	if !r.faultsNarrowed {
		t.Fatal("--no-faults on a profile with faults must mark the run narrowed")
	}

	verdict, code, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !verdict.Budget.Narrowed {
		t.Fatal("verdict.Budget.Narrowed = false, want true")
	}
	if verdict.Verdict != schema.VerdictInconclusive || code != schema.ExitInconclusive {
		t.Fatalf("verdict = %s (exit %d), want inconclusive (exit %d)",
			verdict.Verdict, code, schema.ExitInconclusive)
	}
	if verdict.OracleLock.Status != schema.LockBypassed {
		t.Fatalf("lock status = %s, want bypassed", verdict.OracleLock.Status)
	}
}
