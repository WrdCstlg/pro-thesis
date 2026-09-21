package control

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/WrdCstlg/pro-thesis/internal/harness"
	"github.com/WrdCstlg/pro-thesis/internal/recorder"
)

// ---------------------------------------------------------------------------
// One world, serially, with an answer the caller can read
//
// Runner.Run owns the whole loop and returns a VERDICT. Phase 5 needs the other
// shape: execute exactly one named world and hand back what every oracle said,
// so replay can compare a finding against the ORIGINAL violation's identity
// rather than against "something failed". ParallelRunner.runOne already has that
// shape and this is its serial twin: same worldSpec, same runWorld, no worker
// slot, no port band, no compose project rename.
//
// Nothing here is a second lifecycle. If it ever diverges from runOne, the
// divergence is the bug.
// ---------------------------------------------------------------------------

// RunWorld executes ONE world and returns its full outcome.
//
// It is the seam `thesis replay`, `thesis regress` and `thesis bisect` execute
// through. Three properties it has that Runner.Run does not:
//
//   - the seed may be PINNED (WorldRequest.Seed), which is what makes replaying
//     a recorded world the same world rather than the same fault schedule;
//   - every oracle result comes back, not only the violations, so a caller can
//     tell "the same violation reproduced" from "a different one appeared" from
//     "nothing was checked";
//   - keepUp leaves the topology standing, having first persisted the handoff.
//
// It runs SERIALLY and in the caller's goroutine. Confirmation replays are
// deliberately sequential: k executions of one world against one compose project
// name cannot overlap, and the parallel executor's slot machinery exists to give
// DIFFERENT worlds different ports, which is not this problem.
func (r *Runner) RunWorld(ctx context.Context, req WorldRequest) WorldOutcome {
	return r.runWorldSpec(ctx, req, false)
}

// RunWorldKeepUp is RunWorld that leaves the topology standing for inspection.
//
// It is separate from a boolean on WorldRequest so that keeping a topology up is
// never something a caller can do by accident: on this host a kept-up project
// holds two of about twenty-four bridge networks until someone runs
// `thesis down`, and a search that reached this by passing a struct field
// through would exhaust the pool.
func (r *Runner) RunWorldKeepUp(ctx context.Context, req WorldRequest) WorldOutcome {
	return r.runWorldSpec(ctx, req, true)
}

func (r *Runner) runWorldSpec(ctx context.Context, req WorldRequest, keepUp bool) WorldOutcome {
	ordinal := req.Ordinal
	if ordinal < 1 {
		ordinal = 1
	}
	faults := req.Faults
	if faults == nil {
		faults = r.faults
	}

	spec := worldSpec{
		ordinal: ordinal,
		seed:    req.seedFor(r.seed),
		paths:   NewWorldPaths(filepath.Join(r.runDir, WorldDirName(ordinal))),
		faults:  append([]string(nil), faults...),
		plan:    req.Plan,
		slot:    -1,
		keepUp:  keepUp,
	}

	if !r.quiet {
		fmt.Fprintf(r.stderr, "thesis: world %d (seed %d, %d fault(s))\n",
			spec.ordinal, spec.seed, len(spec.faults))
	}

	start := time.Now()
	wres, err := r.runWorld(ctx, spec)
	elapsed := time.Since(start)

	return WorldOutcome{
		Ordinal:     spec.ordinal,
		Seed:        spec.seed,
		Label:       req.Label,
		Slot:        -1,
		Project:     wres.project,
		HostPorts:   wres.hostPorts,
		Outcome:     wres.outcome,
		Findings:    wres.findings,
		Phases:      wres.phases,
		Paths:       wres.paths,
		Planned:     wres.planned,
		Realized:    wres.realized,
		Drain:       wres.drain,
		Duration:    elapsed,
		Err:         err,
		TeardownErr: wres.teardownErr,
		KeptUp:      wres.keptUp,
	}
}

// ---------------------------------------------------------------------------
// The --keep-up handoff
// ---------------------------------------------------------------------------

// persistHandoff writes everything `thesis down` needs to remove a topology this
// process is about to stop owning.
//
// It writes exactly what `thesis up` writes and in the same order (D-017):
// topology.json first, then the RUNNING sentinel that marks the run live, then
// the CURRENT pointer that says WHICH run is live. A caller that cannot complete
// all three must tear the topology down rather than leave it addressable by
// nobody, which is what runWorld's TEARDOWN does with the error.
//
// The RUNNING sentinel matters even though `down` does not read it: `up`'s
// contract is that an unclosed bundle marks a live run, and a kept-up topology
// that skipped it would look, to every other reader of the artifact tree, like a
// run that had already been torn down cleanly.
func (r *Runner) persistHandoff(top *recorder.Topology) error {
	if top == nil {
		return fmt.Errorf("control: --keep-up: no topology to persist")
	}
	runsDir := filepath.Dir(r.runDir)

	// Refuse to overwrite somebody else's live topology. `up` refuses the same
	// way, and for the same reason: CURRENT can name only one run, so pointing it
	// at this one would make the other unreachable by `thesis down` forever.
	if prev, err := recorder.ReadCurrentRun(runsDir); err == nil && prev != "" && prev != r.runID {
		return fmt.Errorf("control: --keep-up: run %s is already recorded as live in %s; "+
			"run `thesis down` before keeping another topology up", prev, runsDir)
	}

	bundle, err := recorder.OpenBundleAt(runsDir, r.runID)
	if err != nil {
		return fmt.Errorf("control: --keep-up: reopen run bundle: %w", err)
	}
	if err := bundle.WriteTopology(top); err != nil {
		return fmt.Errorf("control: --keep-up: persist topology: %w", err)
	}
	sentinel := filepath.Join(r.runDir, recorder.RunningSentinel)
	if err := os.WriteFile(sentinel, []byte(r.runID+"\n"), 0o644); err != nil {
		return fmt.Errorf("control: --keep-up: write %s: %w", sentinel, err)
	}
	if err := recorder.WriteCurrentRun(runsDir, r.runID); err != nil {
		return fmt.Errorf("control: --keep-up: record current run: %w", err)
	}
	return nil
}

// KeepUpBlocked reports the run id of an already-live topology, or "".
//
// `thesis replay --keep-up` calls it BEFORE booting anything, because refusing
// after a world has run would have already spent thirty seconds and two bridge
// networks to learn something a file read answers.
func KeepUpBlocked(projectDir string) string {
	id, err := recorder.ReadCurrentRun(recorder.RunsDir(projectDir))
	if err != nil {
		return ""
	}
	return id
}

// ComposeProjectFor reports the compose project name a serial world will use, so
// a caller can name it in a message before anything boots.
func ComposeProjectFor(r *Runner) string {
	if r == nil || r.cfg == nil {
		return ""
	}
	return harness.ProjectName(r.cfg, r.opts.ProjectDir)
}
