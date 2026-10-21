package control

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/WrdCstlg/pro-thesis/internal/driver"
	"github.com/WrdCstlg/pro-thesis/internal/recorder"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// A search proposes a world WITH a driver profile (the swap_driver_profile
// operator varies it), but the request carried no profile, so every world drove
// the run profile's workload and recorded it, whatever was proposed. A request
// that names a driver profile now drives and records that one.
func TestAWorldRequestDrivesTheDriverProfileItNames(t *testing.T) {
	cfg := perturbTestConfig(t)
	cfg.Driver.Profiles["heavy"] = schema.DriverProfile{Clients: 2, Ops: 9}
	r := replayRunner(t, stubImages{})
	r.cfg = cfg

	out := r.RunWorld(context.Background(), WorldRequest{Ordinal: 1, DriverProfile: "heavy"})
	if out.Err != nil {
		t.Fatalf("RunWorld: %v", out.Err)
	}
	w, err := recorder.LoadWorld(out.Paths.World)
	if err != nil {
		t.Fatalf("load world: %v", err)
	}
	if w.DriverProfile != "heavy" {
		t.Fatalf("world records driver_profile %q, want heavy", w.DriverProfile)
	}
	raw, err := os.ReadFile(filepath.Join(out.Paths.Dir, driver.PlanFileName))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := driver.ParsePlan(raw)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Profile != "heavy" || plan.Ops != 9 {
		t.Fatalf("the driver was planned with %q (%d ops), want heavy (9 ops)", plan.Profile, plan.Ops)
	}
}

func TestAWorldRequestNamingAnUnknownDriverProfileIsAHarnessError(t *testing.T) {
	r := replayRunner(t, stubImages{})
	out := r.RunWorld(context.Background(), WorldRequest{Ordinal: 1, DriverProfile: "nope"})
	if out.Outcome != OutcomeHarnessError {
		t.Fatalf("outcome %q, want %q", out.Outcome, OutcomeHarnessError)
	}
}
