package control

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// OQ-077: a world whose driver exited non-zero, or did not drain, judged a
// history nobody can vouch for. These tests pin that such a world can never
// report pass, and, as the control that keeps them honest, that a driver which
// drained and exited cleanly leaves a pass standing. Without the control, a world
// that was INCONCLUSIVE for some unrelated reason would satisfy the first two
// tests with the defect present.

// exitedDriver is a workload that has already exited, of its own accord, with
// the given status. Unlike the shared stubDriver, its Result does not claim the
// harness stopped it: Stop on an exited process is a no-op, exactly as the real
// supervisor reports Killed=false for a process that was already gone.
type exitedDriver struct{ code int }

type exitedDriverHandle struct {
	done chan struct{}
	code int
}

func (d *exitedDriver) Start(context.Context, DriverRequest) (DriverHandle, error) {
	ch := make(chan struct{})
	close(ch)
	return &exitedDriverHandle{done: ch, code: d.code}, nil
}

func (h *exitedDriverHandle) Done() <-chan struct{} { return h.done }

func (h *exitedDriverHandle) Drain(_ context.Context, deadline time.Duration) DrainReport {
	return DrainReport{Outcome: DrainCompleted, Deadline: deadline}
}

func (h *exitedDriverHandle) Stop(context.Context) error { return nil }

func (h *exitedDriverHandle) Result() DriverResult {
	res := DriverResult{Started: true, ExitCode: h.code, Drain: DrainReport{Outcome: DrainCompleted}}
	if h.code != 0 {
		res.Err = fmt.Errorf("stub: driver exited %d", h.code)
	}
	return res
}

func runOneWorld(t *testing.T, drv Driver, deadline time.Duration) (Outcome, schema.ExitCode, string) {
	t.Helper()
	var errOut strings.Builder
	r := newDrainRunner(t, drv, deadline, &errOut)
	_, code, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	res := readResultDoc(t, r, 1)
	return Outcome(fmt.Sprint(res["outcome"])), code, errOut.String()
}

func TestADriverThatDrainedAndExitedCleanlyLeavesPassStanding(t *testing.T) {
	outcome, code, stderr := runOneWorld(t, &exitedDriver{code: 0}, 2*time.Second)
	if outcome != OutcomePass || code != schema.ExitPass {
		t.Fatalf("a clean driver gave outcome %q, exit %d, want pass and 0; the gate must not "+
			"refuse a world whose driver did nothing wrong\nstderr:\n%s", outcome, code, stderr)
	}
}

func TestADriverThatExitedNonZeroMakesTheWorldInconclusive(t *testing.T) {
	outcome, code, stderr := runOneWorld(t, &exitedDriver{code: 3}, 2*time.Second)
	if outcome != OutcomeInconclusive || code != schema.ExitInconclusive {
		t.Fatalf("a driver that exited 3 gave outcome %q, exit %d, want inconclusive and 2: "+
			"its history may stop short of what the system did (OQ-077)", outcome, code)
	}
	if !strings.Contains(stderr, "OQ-077") {
		t.Fatalf("the refusal was not reported on stderr:\n%s", stderr)
	}
}

func TestADriverThatMissedTheDrainDeadlineMakesTheWorldInconclusive(t *testing.T) {
	outcome, code, stderr := runOneWorld(t, &stubDriver{drainable: false}, 100*time.Millisecond)
	if outcome != OutcomeInconclusive || code != schema.ExitInconclusive {
		t.Fatalf("a driver killed at the drain deadline gave outcome %q, exit %d, want "+
			"inconclusive and 2: operations it held were never retired (OQ-077)", outcome, code)
	}
	if !strings.Contains(stderr, "OQ-077") {
		t.Fatalf("the refusal was not reported on stderr:\n%s", stderr)
	}
}
