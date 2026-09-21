package control

import (
	"context"
	"fmt"
	"time"

	"github.com/WrdCstlg/pro-thesis/internal/perturber"
	"github.com/WrdCstlg/pro-thesis/internal/recorder"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// ---------------------------------------------------------------------------
// PERTURB
//
// Directive 4.1 step 4: "Execute scheduled faults against the virtual clock
// (overlaps DRIVE)". Everything about HOW a fault is applied lives in
// internal/perturber and internal/perturber/faults; this file is only the wiring
// between the lifecycle and the schedule.
// ---------------------------------------------------------------------------

// DefaultDriveWindow is how long a world with NO faults spends in PERTURB before
// moving on, when the driver has not finished on its own.
//
// A world with faults is bounded by its schedule instead: the last withdrawal is
// the end of PERTURB, and cutting it short would leave a fault active into HEAL.
const DefaultDriveWindow = 3 * time.Second

// perturbation is one world's compiled schedule plus the executor that drives
// it.
type perturbation struct {
	schedule *perturber.Schedule
	exec     *perturber.Executor
}

// newPerturbation compiles this world's planned faults and wires an executor.
//
// It is built at PERTURB rather than at BOOT for one reason that matters: the
// virtual clock's origin IS DRIVE start, so before DRIVE there is no frame for
// `@8200..11000` to mean anything, and perturber.NewExecutor refuses to be
// constructed without one.
//
// The schedule and the injectors are PARAMETERS rather than Runner fields.
// Phase 4's search gives every world a different schedule, and every world
// builds its own injectors against its own containers (D-032); reading either
// off the Runner would make two concurrent worlds share one.
func (r *Runner) newPerturbation(
	topology *recorder.Topology,
	timeline *recorder.Timeline,
	worldSeed uint64,
	planned []string,
	injectors []perturber.Injector,
) (*perturbation, error) {
	top, err := perturber.NewTopology(r.cfg, topology)
	if err != nil {
		return nil, err
	}

	reg, err := perturber.NewRegistry(injectors...)
	if err != nil {
		return nil, err
	}

	sched, err := perturber.Compile(perturber.CompileOptions{
		Config:   r.cfg,
		Topology: top,
		Planned:  planned,
		Registry: reg,
	})
	if err != nil {
		return nil, err
	}

	// A `role:` target binds to whichever node holds the role AT INJECTION TIME,
	// read either from the probe the target declares in harness.role_probe or,
	// absent that, from the system's own /status endpoint over its published
	// loopback port (roleObserver, D-066). harness.nodes[].role_hint is static
	// and advisory and is deliberately not consulted: Raft leadership moves,
	// and a hint would pin a node that was deposed ten seconds ago.
	resolver, err := perturber.NewResolver(top, r.roleObserver(topology))
	if err != nil {
		return nil, err
	}

	exec, err := perturber.NewExecutor(perturber.ExecutorOptions{
		RunID:    r.runID,
		Schedule: sched,
		Topology: top,
		Resolver: resolver,
		Registry: reg,
		Clock:    r.clock,
		Timeline: timeline,
		Seed:     recorder.Seed(worldSeed),
		Log:      r.perturbLogf,
	})
	if err != nil {
		return nil, err
	}
	return &perturbation{schedule: sched, exec: exec}, nil
}

func (r *Runner) perturbLogf(format string, args ...any) {
	if r.quiet {
		return
	}
	fmt.Fprintf(r.stderr, format+"\n", args...)
}

// waitOutDriveWindow is the no-fault PERTURB: there is nothing to inject, so the
// phase simply overlaps whatever DRIVE is still doing.
func waitOutDriveWindow(ctx context.Context, done <-chan struct{}, window time.Duration) error {
	if window <= 0 {
		window = DefaultDriveWindow
	}
	timer := time.NewTimer(window)
	defer timer.Stop()
	select {
	case <-done:
		return nil
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// healPerturbation withdraws whatever is still active and verifies zero
// residual.
//
// A residue is a HARNESS ERROR (exit 2), never an oracle violation: the run
// could not be conducted properly, so it has no verdict to give about the system
// under test, and on this host the residue would poison every world that
// follows it. Returning "unverified" is treated the same way, because a check
// that could not run is not a clean result.
func healPerturbation(ctx context.Context, p *perturbation) (perturber.HealReport, error) {
	if p == nil || p.exec == nil {
		return perturber.HealReport{}, nil
	}
	return p.exec.Heal(ctx)
}

// realizedOf reports what was actually injected, for the world file and the
// causal timeline. It is safe on a nil perturbation, which is the state of a
// world that never reached PERTURB.
func realizedOf(p *perturbation) []schema.RealizedFault {
	if p == nil || p.exec == nil {
		return nil
	}
	return p.exec.Realized()
}

// plannedOf reports the compiled planned schedule in canonical form.
func plannedOf(p *perturbation) []string {
	if p == nil || p.schedule == nil {
		return nil
	}
	return p.schedule.Planned()
}
