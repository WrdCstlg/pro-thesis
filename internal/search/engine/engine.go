// Package engine is Phase 4's execution loop: the thing that turns a
// search.Strategy into executed worlds and executed worlds back into signal.
//
// It is the only package that depends on BOTH the pure search substrate
// (internal/search, internal/search/saboteur) and the real executor
// (internal/control). Keeping it separate is what lets the strategies be tested
// without a container runtime and the executor be tested without a strategy.
//
// # What this package is responsible for, and the order it matters in
//
//  1. NOT LEAKING. This is the constraint that decides whether search is usable
//     at all: the host has a HARD CEILING of about 24 free Docker bridge
//     networks and the fixture uses two per project, so a search that leaks one
//     project per world dies after a dozen worlds with an error that reads like
//     a Docker bug (D-017, OQ-013). The executor already refuses to start when
//     the pool cannot support the requested concurrency and retires a slot whose
//     teardown could not be verified; this package's job is to propagate
//     ErrWorkerPoolExhausted as a STOP rather than retrying into an empty pool,
//     and to report the network count it measured before and after.
//
//  2. NOT LEARNING FROM NOISE. A world that could not be judged must not be
//     scored as a world that was judged and found clean. Every outcome is
//     converted through search.Outcome, whose Signal is three-valued, and an
//     unobserved world is never folded into global coverage.
//
//  3. BEING REPRODUCIBLE. Worlds are proposed in ordinal order, each world's
//     seed is a path-keyed function of (run seed, ordinal) alone, and outcomes
//     are observed in ORDINAL order rather than completion order even though the
//     executor completes them out of order.
package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/WrdCstlg/pro-thesis/internal/buildinfo"
	"github.com/WrdCstlg/pro-thesis/internal/control"
	"github.com/WrdCstlg/pro-thesis/internal/harness"
	"github.com/WrdCstlg/pro-thesis/internal/perturber"
	"github.com/WrdCstlg/pro-thesis/internal/perturber/faults"
	"github.com/WrdCstlg/pro-thesis/internal/recorder"
	"github.com/WrdCstlg/pro-thesis/internal/search"
	"github.com/WrdCstlg/pro-thesis/internal/search/llm"
	"github.com/WrdCstlg/pro-thesis/internal/search/saboteur"
	"github.com/WrdCstlg/pro-thesis/internal/telemetry"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// DefaultWorkers is D-022's measured operating point: four concurrent compose
// projects use 8 of this host's ~24 free bridge networks, leaving headroom for
// anything else on the machine, and buy roughly a 3.8x speedup for a measured
// +3-5% timing distortion.
const DefaultWorkers = 4

// ErrSearchDisabled reports a profile whose `search:` boolean is false.
//
// D-028 item 7: the PROFILE's boolean decides WHETHER a search runs and the
// top-level `search:` block decides WHICH. `thesis gate` is a fixed-budget
// pre-commit check whose whole value is being a repeatable signal, so it must
// never run an adaptive MCTS search, and the way that is enforced is here,
// where the profile is read, rather than by trusting a caller not to ask.
var ErrSearchDisabled = errors.New("engine: this profile does not enable search")

// Options builds an Engine.
type Options struct {
	// Runner is a constructed control.Runner. The engine drives it world by
	// world through Parallel; it never calls Runner.Run, which owns its own
	// loop and its own stopping rule.
	Runner *control.Runner

	// Strategy names which search runs. Empty means the config's
	// `search.strategy`.
	Strategy schema.SearchStrategy

	// Workers is the concurrency. Zero means DefaultWorkers.
	Workers int

	// Budget and Worlds NARROW the profile's, exactly as `thesis run` does.
	// Widening either from argv would be a gate-weakening lever that never
	// touches prothesis.yaml.
	Budget time.Duration
	Worlds int

	// WorkerEnv maps a worker slot onto the compose variables the topology
	// needs. Without it every concurrent project publishes the same host ports
	// and shares container names, so the worlds collide.
	WorkerEnv func(control.WorkerSlot) []string

	// Networks bounds the concurrency by the host's bridge-network pool.
	Networks control.NetworkBudget

	// OracleLock is the drift check made before the run started, verbatim.
	OracleLock schema.OracleLock

	// Stderr receives progress. nil means os.Stderr; Quiet silences it.
	Stderr io.Writer
	Quiet  bool
}

// Engine runs the search.
type Engine struct {
	opts     Options
	runner   *control.Runner
	cfg      *schema.Config
	strategy search.Strategy
	// saboteurStrategy is the same object as strategy when the Saboteur is
	// running, so the engine can hand it the telemetry search.Outcome does not
	// carry. It is nil for the random baseline.
	saboteurStrategy *saboteur.Saboteur

	params  search.Params
	corpus  *search.Corpus
	streams *search.Streams
	space   *search.Space
	top     *perturber.Topology

	budget  control.Budget
	tracker *control.BudgetTracker
	split   saboteur.BudgetSplit

	stderr io.Writer
	quiet  bool

	// unsupported are the fault kinds this run may inject by config but this
	// PLATFORM cannot deliver. They are removed from the shared space and
	// reported once, rather than discovered a world at a time.
	unsupported []string
}

// restrictToPlatform removes fault kinds this host cannot actually deliver from
// the shared action space, and returns what it removed.
//
// This is not tidiness. Measured on the reference fixture: `perturber.allow`
// contains `io.latency`, which is in the frozen 17-kind registry and is NOT
// implementable on Docker Desktop (D-033b); delaying filesystem operations needs
// a shim under a running container's overlay2 mount, which cannot be inserted.
// A generated world naming it reached ErrUnsupported eight seconds into DRIVE,
// having already spent a compose project, two bridge networks and ~33 seconds to
// learn what a table lookup knows. At a ~62-rollout budget that is a real
// fraction of the search.
//
// It is applied to the SHARED Space, so BOTH arms of D-029's comparison lose
// exactly the same kinds. Restricting only the Saboteur would hand it a cleaner
// space than the baseline and make the benchmark measure the filter.
func restrictToPlatform(sp *search.Space, supported func(schema.FaultKind) error) []string {
	if sp == nil || supported == nil {
		return nil
	}
	kept := make([]schema.FaultKind, 0, len(sp.Kinds))
	var dropped []string
	for _, k := range sp.Kinds {
		if err := supported(k); err != nil {
			dropped = append(dropped, fmt.Sprintf("%s (%s)", k, firstSentence(err.Error())))
			delete(sp.Targets, k)
			continue
		}
		kept = append(kept, k)
	}
	// A space with nothing left is NOT silently emptied: search.NewSpace already
	// refuses an empty space because a search over one reports worlds it never
	// perturbed, and the same reasoning applies here. The caller sees the
	// original space back, and the first Compile will refuse honestly.
	if len(kept) == 0 {
		return dropped
	}
	sp.Kinds = kept
	return dropped
}

// firstSentence trims a long capability explanation to its first clause, so a
// startup line stays readable. The full reason still reaches the operator from
// the perturber if a kind is ever reached anyway.
func firstSentence(s string) string {
	if i := strings.Index(s, " — "); i > 0 {
		return s[:i]
	}
	if i := strings.Index(s, ": "); i > 0 && i < 120 {
		return s[:i]
	}
	if len(s) > 120 {
		return s[:120] + "..."
	}
	return s
}

// Result is what a completed search reports.
type Result struct {
	Verdict  schema.Verdict
	ExitCode schema.ExitCode

	// Worlds is every executed world, in ordinal order.
	Worlds []control.WorldOutcome
	// Outcomes is the search's own reading of each world, ordinal-ordered.
	Outcomes []search.Outcome
	// Strategy is the strategy that actually ran.
	Strategy schema.SearchStrategy
	// FirstViolationOrdinal is the 1-based ordinal of the first world whose
	// oracles reported a definite violation, or 0.
	FirstViolationOrdinal int
	// TimeToFirstViolation is measured from the first world starting. It is
	// meaningless unless FirstViolationOrdinal is non-zero, which is exactly
	// why D-029 pre-registers a test on the COUNT of trials that found one
	// rather than on a median of this right-censored quantity.
	TimeToFirstViolation time.Duration
	// FirstViolationFaults is the schedule that produced it.
	FirstViolationFaults []string

	// Coverage is the run's cumulative coverage.
	Coverage schema.Coverage

	// NetworksBefore and NetworksAfter are the measured free bridge-network
	// counts either side of the run. They are the leak check, and they are
	// -1 when the probe could not run.
	NetworksBefore int
	NetworksAfter  int

	// Signals is A.3's ranked probe output, when the Saboteur ran.
	Signals []saboteur.ProbeSignal
	// TreeStats is each escalation tree's measurement of itself.
	TreeStats []saboteur.TreeStats
	// ControlEvaluable reports whether A.3's no-fault control world produced a
	// usable verdict. When it is false, every comparative criterion in the sweep
	// reported unknown rather than false, and a sweep full of DAMPEN means "no
	// baseline" rather than "nothing was wrong".
	ControlEvaluable bool
	// Stage is the sub-module the Saboteur finished in.
	Stage string
	// EscalationNote says why Tier 2 was not activated, when it was not.
	EscalationNote string
	// Termination records why the search stopped.
	Termination saboteur.Termination
	// StoppedBecause is a human sentence.
	StoppedBecause string
}

// TreeContributed reports whether backpropagation ever decided anything, across
// every escalation tree. False is the D-015 prediction confirmed: the ladder
// carried the search and the tree was a tie-breaker.
func (r Result) TreeContributed() bool {
	for _, s := range r.TreeStats {
		if s.TreeContributed() {
			return true
		}
	}
	return false
}

// New builds the engine, refusing a profile that does not enable search.
func New(o Options) (*Engine, error) {
	if o.Runner == nil {
		return nil, errors.New("engine: needs a runner")
	}
	cfg := o.Runner.Config()
	profName := o.Runner.Profile()
	prof, ok := cfg.Profiles[profName]
	if !ok {
		return nil, fmt.Errorf("engine: no profile %q in prothesis.yaml", profName)
	}
	if !prof.Search {
		return nil, fmt.Errorf("%w: profile %q has `search: false` (or omits it). The PROFILE's "+
			"boolean decides WHETHER a search runs; the top-level `search:` block decides WHICH. "+
			"Enable it on a profile meant for searching, never on a gate profile (D-028 item 7)",
			ErrSearchDisabled, profName)
	}

	stderr := o.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}

	// A `search:` block that never had A.10's defaults applied is REFUSED, not
	// defaulted here.
	//
	// The zero value is reachable only from a hand-built schema.Config: the
	// decoder fills these in and the validator refuses max_mcts_depth < 1, so a
	// config that came from a file always has them. Quietly substituting A.6's
	// literals for a block the caller may have written deliberately would make
	// the search rank worlds by weights nobody chose, and a violation_weight of
	// 0 scores a world that found a consistency bug exactly equal to one that
	// found nothing, which is the ranked #1 failure mode arriving through a
	// struct literal. saboteur.WeightsFrom refuses that; this refuses the
	// under-specified config that produces it, with a message that says which.
	if cfg.Search.MaxMCTSDepth < 1 {
		return nil, fmt.Errorf("engine: search.max_mcts_depth is %d, which is impossible after "+
			"schema validation — this config never had its defaults applied. Decode it with "+
			"schema.DecodeConfig, or fill in the `search:` block explicitly; the engine will not "+
			"invent utility weights the operator did not choose", cfg.Search.MaxMCTSDepth)
	}

	top, err := perturber.NewTopology(cfg, nil)
	if err != nil {
		return nil, fmt.Errorf("engine: build topology: %w", err)
	}
	space, err := search.NewSpace(cfg, top, search.DefaultWindowPolicy())
	if err != nil {
		return nil, err
	}
	unsupported := restrictToPlatform(space, faults.PlatformCapability)
	// The validator gets NO registry: the concrete injectors are built per
	// world at BOOT against live container ids (D-032), so there is nothing to
	// hand it here. Kind-level platform capability is checked separately, by
	// the ladder and the sweep, before a fault string is ever built.
	validator, err := search.NewValidator(cfg, top, nil)
	if err != nil {
		return nil, err
	}
	streams := search.NewStreams(recorder.Seed(o.Runner.Seed()))
	corpus := search.NewCorpus(search.DefaultEnergyParams())

	base := schema.NewWorld(0, "default", prof.DriverProfile)
	params := search.Params{
		Config:    cfg,
		Space:     space,
		Validator: validator,
		Corpus:    corpus,
		Streams:   streams,
		Base:      base,
	}

	e := &Engine{
		opts:    o,
		runner:  o.Runner,
		cfg:     cfg,
		params:  params,
		corpus:  corpus,
		streams: streams,
		space:   space,
		top:     top,
		stderr:  stderr,
		quiet:   o.Quiet,

		unsupported: unsupported,
	}
	e.budget = e.resolveBudget(prof)
	e.tracker = control.NewBudgetTracker(e.budget, o.Runner.Clock())
	e.split = saboteur.SplitBudget(e.budget.Wall, e.budget.Worlds, cfg.Search.ProbeBudgetPct)

	strat := o.Strategy
	if strat == "" {
		strat = cfg.Search.Strategy
	}
	if err := e.buildStrategy(strat); err != nil {
		return nil, err
	}
	return e, nil
}

// resolveBudget applies the narrowing rule Runner.Run applies.
//
// It CALLS that rule rather than restating it. The two were hand-copied
// duplicates until D-059, which is why a search narrowed from argv kept
// reporting PASS and `oracle_lock: ok` for as long as it did: the fix went into
// one copy and the other was never going to be found by reading the first.
func (e *Engine) resolveBudget(prof schema.Profile) control.Budget {
	worlds := -1
	if prof.Worlds != nil {
		worlds = *prof.Worlds
	}
	return control.ApplyCLIOverrides(
		control.Budget{Wall: prof.Budget.Std(), Worlds: worlds},
		e.opts.Budget, e.opts.Worlds)
}

func (e *Engine) buildStrategy(name schema.SearchStrategy) error {
	switch name {
	case schema.StrategyRandom:
		r, err := search.NewRandom(e.params)
		if err != nil {
			return err
		}
		e.strategy = r
		return nil
	case schema.StrategySaboteur, schema.StrategyHybrid, "":
		// `hybrid` is A.10's third name and is not a separate implementation:
		// the Saboteur IS the hybrid; it probes, escalates, and falls back to
		// stochastic corpus mutation when nothing reinforces, which is A.5's own
		// description of the fallback. Mapping it here rather than inventing a
		// third search keeps the vocabulary honest and is recorded as a
		// deviation rather than a silent alias.
		s, err := saboteur.New(saboteur.Options{
			Params:         e.params,
			Topology:       e.top,
			Search:         e.cfg.Search,
			MaxProbeWorlds: e.split.ProbeWorlds,
			Log:            e.logf,
		})
		if err != nil {
			return err
		}
		e.strategy = s
		e.saboteurStrategy = s
		return nil
	case schema.StrategyLLM:
		// The llm strategy carries per-ordinal feedback state: Observe coaches
		// the NEXT Propose. A parallel pool would interleave proposals and
		// observations out of order, so the worker count is pinned to one:
		// an explicit --workers > 1 is refused rather than silently narrowed,
		// and an unspecified one defaults to 1 instead of DefaultWorkers.
		if e.opts.Workers > 1 {
			return fmt.Errorf("engine: search.strategy llm requires --workers 1 (got %d): the "+
				"strategy coaches each proposal from the previous world's outcome, which parallel "+
				"workers would interleave out of order (D-075)", e.opts.Workers)
		}
		if e.opts.Workers == 0 {
			e.opts.Workers = 1
		}
		l, err := llm.New(llm.Options{
			Params: e.params,
			LLM:    e.cfg.Search.LLM,
			Dir:    e.runner.RunDir(),
			Log:    e.logf,
		})
		if err != nil {
			return err
		}
		e.strategy = l
		return nil
	default:
		return fmt.Errorf("engine: unknown search strategy %q (want one of %v)",
			name, schema.AllSearchStrategies)
	}
}

// Strategy returns the strategy that will run.
func (e *Engine) Strategy() search.Strategy { return e.strategy }

// Corpus returns the shared archive.
func (e *Engine) Corpus() *search.Corpus { return e.corpus }

// Run executes the search.
//
// It returns a verdict on every path, including cancellation and budget expiry:
// A.9 #5 requires `thesis search --budget 30m` to complete without panic and
// produce a well-formed prothesis.verdict/v1, and a run that ended early is
// still a run with something true to say.
func (e *Engine) Run(ctx context.Context) (Result, error) {
	res := Result{Strategy: e.strategy.Name(), NetworksBefore: -1, NetworksAfter: -1}

	workers := e.opts.Workers
	if workers <= 0 {
		workers = DefaultWorkers
	}
	if e.budget.Worlds > 0 && workers > e.budget.Worlds {
		workers = e.budget.Worlds
	}

	nets := e.opts.Networks
	if nets.Probe == nil {
		nets.Probe = harness.FreeBridgeNetworks
	}
	if free, err := nets.Probe(ctx); err == nil {
		res.NetworksBefore = free
	}

	pr, err := e.runner.Parallel(control.ParallelOptions{
		Workers:   workers,
		Networks:  nets,
		WorkerEnv: e.opts.WorkerEnv,
	})
	if err != nil {
		// FAILS CLOSED. The pool cannot support this concurrency, and the
		// message carries the arithmetic rather than silently truncating to
		// whatever fits: a search quietly demoted to one worker would take
		// four times as long and nobody would know why.
		return res, err
	}

	for _, u := range e.unsupported {
		e.logf("thesis: search: EXCLUDED %s from the action space: this platform cannot deliver it, "+
			"so a world naming it would spend a compose project and ~30s to reach ErrUnsupported", u)
	}
	e.logf("thesis: search: strategy=%s workers=%d %s", e.strategy.Name(), workers, explainBudget(e.budget))
	e.logf("thesis: search: %s", e.split.Explain())
	if res.NetworksBefore >= 0 {
		e.logf("thesis: search: %s", nets.Explain())
	}

	ordinal := 0
	stopReason := ""
	probeStarted := time.Now()

	for {
		if canStart, reason := e.tracker.MayStartWorld(); !canStart {
			stopReason = reason
			break
		}
		if ctx.Err() != nil {
			stopReason = "cancelled"
			break
		}

		// A.5's "whichever is reached first" for the Tier 1 wall budget.
		if e.saboteurStrategy != nil && e.saboteurStrategy.Stage() == saboteur.StageProbe {
			if done, why := saboteur.ProbeStageExpired(probeStarted, e.split, e.tracker.WorldsStarted()); done {
				e.saboteurStrategy.EndProbeStage(why)
			}
		}

		batch, worlds, err := e.proposeBatch(ctx, workers, &ordinal)
		if err != nil {
			if errors.Is(err, search.ErrExhausted) {
				stopReason = "the strategy has nothing further to propose"
				break
			}
			return e.finish(ctx, res, stopReason, err), err
		}
		if len(batch) == 0 {
			stopReason = "the strategy proposed no further worlds"
			break
		}

		for range batch {
			e.tracker.NoteWorldStarted()
		}
		outs, runErr := pr.Run(ctx, batch)
		for range batch {
			e.tracker.NoteWorldFinished()
		}

		res.Worlds = append(res.Worlds, outs...)
		violation := e.absorb(ctx, &res, outs, worlds, probeStarted)

		if runErr != nil {
			if errors.Is(runErr, control.ErrWorkerPoolExhausted) {
				// STOP, do not retry. Every slot has been retired because its
				// teardown could not be verified; the networks those projects
				// hold are gone until an operator sweeps them, and continuing
				// would burn the rest of the pool discovering that again.
				e.logf("thesis: search: STOPPING: %v", runErr)
				stopReason = "the worker pool is exhausted: " + runErr.Error()
				break
			}
			e.logf("thesis: search: world batch error: %v", runErr)
		}

		if violation.Terminates() {
			res.Termination = violation
			// A violation during TIER 1 is RECORDED but does not abandon the
			// sweep, and this is a deliberate scoping of A.5's early
			// termination rather than a reluctance to stop.
			//
			// A.3's probe sweep is reconnaissance whose OUTPUT is a ranked list
			// over the whole (kind, target) space; A.5 then roots Tier 2 at the
			// top-ranked reinforcing signals. Ending the run at probe 3 of 21
			// destroys that list and Tier 2 never happens, which is exactly
			// what the measured first attempt did: a `proc.kill` probe produced
			// a crash finding and the search stopped, having never escalated.
			//
			// Nothing is lost by continuing: the violation is already in
			// res.Worlds, it reaches the verdict, the exit code is still 1, and
			// FirstViolationOrdinal / TimeToFirstViolation were recorded at the
			// moment it was found, so D-029's pre-registered count metric is
			// unaffected. Once the search is escalating, a violation IS the kill
			// shot and the run stops.
			if e.saboteurStrategy != nil && e.saboteurStrategy.Stage() == saboteur.StageProbe {
				e.logf("thesis: search: world %d violated during Tier 1 reconnaissance; recorded, "+
					"and the sweep continues so A.3's ranked output is complete", res.FirstViolationOrdinal)
			} else {
				stopReason = "a violation was found: " + violation.Reason
				break
			}
		}
		if ctx.Err() != nil {
			stopReason = "cancelled"
			break
		}
	}

	if free, err := nets.Probe(ctx); err == nil {
		res.NetworksAfter = free
	}
	return e.finish(ctx, res, stopReason, nil), nil
}

// proposeBatch asks the strategy for up to n worlds.
//
// A proposal failure mid-batch is NOT fatal: the batch already assembled is
// executed and the failure is reported. A strategy that ran out of legal
// candidates at world 37 has still produced 36 useful worlds.
func (e *Engine) proposeBatch(ctx context.Context, n int, ordinal *int) ([]control.WorldRequest, map[int]schema.World, error) {
	reqs := make([]control.WorldRequest, 0, n)
	worlds := make(map[int]schema.World, n)
	for i := 0; i < n; i++ {
		if canStart, _ := e.tracker.MayStartWorld(); !canStart && len(reqs) > 0 {
			break
		}
		if e.budget.Worlds > 0 && *ordinal+len(reqs) >= e.budget.Worlds {
			break
		}
		w, err := e.strategy.Propose(ctx, *ordinal)
		if err != nil {
			if len(reqs) > 0 {
				e.logf("thesis: search: proposal stopped after %d world(s) in this batch: %v", len(reqs), err)
				break
			}
			return nil, nil, err
		}
		*ordinal++
		label := string(w.Meta.Origin)
		if len(w.FaultSchedule.Planned) == 0 {
			label = "control"
		}
		reqs = append(reqs, control.WorldRequest{
			// 1-based for the executor; the strategy counts from 0, and
			// saboteur.WorldSeedFor / control.WorldSeedFor agree across that
			// offset by construction.
			Ordinal: *ordinal,
			Faults:  append([]string(nil), w.FaultSchedule.Planned...),
			Label:   label,
		})
		worlds[*ordinal] = w
	}
	return reqs, worlds, nil
}

// absorb converts executed worlds into signal and feeds the strategy.
//
// Outcomes are processed in ORDINAL order, which control.ParallelRunner
// guarantees, so the corpus's coverage deltas and the strategy's observations do
// not depend on which world happened to finish first.
func (e *Engine) absorb(ctx context.Context, res *Result, outs []control.WorldOutcome,
	worlds map[int]schema.World, started time.Time) saboteur.Termination {

	term := saboteur.Termination{Scope: saboteur.TerminateNothing, Reason: "no definite violation"}

	for _, o := range outs {
		// Matched by ORDINAL, never by position. The executor returns outcomes
		// sorted by ordinal and the batch was built in ordinal order, so index
		// alignment happens to hold, but it holds only while both of those
		// stay true, and a world paired with the wrong proposal would archive
		// one schedule under another's hash and backpropagate one rollout's
		// evidence to another's tree node.
		w, ok := worlds[o.Ordinal]
		if !ok {
			e.logf("thesis: search: world %d came back with no matching proposal; not scored", o.Ordinal)
			continue
		}
		out := OutcomeOf(o, w)
		cov := e.coverageOf(o)
		add, err := e.corpus.Add(w, cov, out)
		if err != nil {
			e.logf("thesis: search: world %d: corpus: %v", o.Ordinal, err)
		}
		// Corpus.Add fills Coverage/Templates/States on the outcome it was
		// given, by value; take the recomputed delta back.
		if add.Entry != nil {
			out = add.Entry.Outcome
		} else {
			out.Coverage = add.Delta
		}
		res.Outcomes = append(res.Outcomes, out)

		e.logf("thesis: search: world %d [%s] %d fault(s) %v -> %s (+%dt/+%ds, %s)",
			o.Ordinal, o.Label, len(o.Planned), o.Planned, out.Signal(),
			out.Coverage.NewTemplates, out.Coverage.NewStates, o.Duration.Round(time.Millisecond))
		if out.Signal() == search.SignalUnknown {
			e.logf("thesis: search: world %d: %s", o.Ordinal, out.Why())
		}

		e.observe(ctx, out, o)

		if t := terminationFor(out); t.Terminates() && !term.Terminates() {
			term = t
			if res.FirstViolationOrdinal == 0 {
				res.FirstViolationOrdinal = o.Ordinal
				res.TimeToFirstViolation = time.Since(started)
				res.FirstViolationFaults = append([]string(nil), o.Planned...)
			}
		}
	}
	return term
}

func (e *Engine) observe(ctx context.Context, out search.Outcome, o control.WorldOutcome) {
	if e.saboteurStrategy != nil {
		obs := e.observationsOf(o)
		if err := e.saboteurStrategy.ObserveWorld(ctx, out, obs); err != nil {
			e.logf("thesis: search: observe world %d: %v", o.Ordinal, err)
		}
		return
	}
	if err := e.strategy.Observe(ctx, out); err != nil {
		e.logf("thesis: search: observe world %d: %v", o.Ordinal, err)
	}
}

// terminationFor applies OQ-004's prefix-closed rule to one world's findings.
func terminationFor(out search.Outcome) saboteur.Termination {
	ins := make([]saboteur.TerminationInput, 0, len(out.Findings))
	for _, f := range out.Findings {
		ins = append(ins, saboteur.TerminationInput{
			Oracle:        f.Oracle,
			Class:         f.Class,
			Definite:      f.Definite(),
			Violated:      f.Definite() && f.Status == schema.StatusViolated,
			ObservedPhase: f.Phase,
		})
	}
	return saboteur.FirstTermination(ins)
}

// finish assembles the verdict.
func (e *Engine) finish(ctx context.Context, res Result, stopReason string, fatal error) Result {
	outcome := control.OutcomePass
	sawViolation := false
	for _, o := range res.Worlds {
		if o.Outcome.Rank() > outcome.Rank() {
			outcome = o.Outcome
		}
		if o.Violated() {
			sawViolation = true
		}
	}
	if len(res.Worlds) == 0 {
		outcome = control.OutcomeInconclusive
	}
	if fatal != nil && outcome.Rank() < control.OutcomeHarnessError.Rank() {
		outcome = control.OutcomeHarnessError
	}
	if ctx.Err() != nil && !sawViolation {
		outcome = outcome.Worse(control.OutcomeCanceled)
	}
	if outcome == control.OutcomePass && e.tracker.WallExpired() && !sawViolation {
		outcome = control.BudgetOutcome(false)
	}

	var violations []schema.Violation
	idx := 1
	for _, o := range res.Worlds {
		for _, f := range o.Findings {
			if !f.Violated() {
				continue
			}
			violations = append(violations, control.NewViolation(control.ViolationID(idx), f, nil))
			idx++
		}
	}

	cov := e.corpus.Global().Counts()
	var newT, newS int64
	for _, o := range res.Outcomes {
		newT += o.Coverage.NewTemplates
		newS += o.Coverage.NewStates
	}
	cov.NewTemplates = newT
	cov.NewStates = newS
	res.Coverage = cov

	v := control.BuildVerdict(control.VerdictInput{
		RunID:      e.runner.RunID(),
		Profile:    e.runner.Profile(),
		Commit:     buildinfo.VerdictCommit(control.GitCommit(ctx, e.runner.ProjectDir())),
		Outcome:    outcome,
		Budget:     e.tracker.Snapshot(),
		Violations: violations,
		Artifacts:  control.RelativeArtifacts(e.runner.ProjectDir(), e.runner.RunDir()),
		OracleLock: e.opts.OracleLock,
	})
	// Phase 4 is the phase that MEASURES coverage, so the verdict stops
	// carrying zeros for it. Everything else BuildVerdict leaves empty stays
	// empty: coverage_delta_vs_baseline has no baseline commit to compare
	// against, and inventing one would read as a claim nobody made.
	v.Coverage = cov
	v.Normalize()

	res.Verdict = v
	res.ExitCode = v.Verdict.ExitCode()
	res.StoppedBecause = stopReason

	if e.saboteurStrategy != nil {
		res.Signals = e.saboteurStrategy.Signals()
		res.TreeStats = e.saboteurStrategy.TreeStats()
		res.ControlEvaluable = e.saboteurStrategy.ControlEvaluable()
		res.Stage = string(e.saboteurStrategy.Stage())
		if err := e.saboteurStrategy.EscalationError(); err != nil {
			res.EscalationNote = err.Error()
		}
	}

	_ = control.WriteVerdictFile(filepath.Join(e.runner.RunDir(), control.VerdictFileName), &v)
	e.writeSearchRecord(res)
	return res
}

func (e *Engine) logf(format string, args ...any) {
	if e.quiet || e.stderr == nil {
		return
	}
	fmt.Fprintf(e.stderr, format+"\n", args...)
}

// ---------------------------------------------------------------------------
// turning artifacts back into signal
// ---------------------------------------------------------------------------

// OutcomeOf converts an executed world into the search's three-valued reading.
//
// The world is OBSERVED only when it reached DRIVE and produced a history. A
// world that failed in BOOT wrote a truncated boot log, and folding that into
// coverage would teach the search that failing to boot is novel, which is how
// a corpus fills with worlds that break the harness rather than the target.
func OutcomeOf(o control.WorldOutcome, w schema.World) search.Outcome {
	out := search.Outcome{
		Faults:     append([]string(nil), o.Planned...),
		Ordinal:    o.Ordinal,
		DurationMS: o.Duration.Milliseconds(),
	}
	if h, err := w.Hash(); err == nil {
		out.WorldHash = h
	}
	if o.Err != nil {
		out.Err = o.Err.Error()
	}
	out.Observed = hasDrive(o.Phases) && o.Outcome != control.OutcomeHarnessError
	for _, f := range o.Findings {
		fd := search.Finding{
			Oracle:      f.Output.Oracle,
			Class:       f.Output.Class,
			Severity:    schema.DefaultSeverity(f.Output.Class),
			Status:      f.Output.Status,
			Phase:       f.ObservedPhase,
			FirstSeenMS: f.FirstSeenMS,
		}
		if f.Err != nil {
			fd.Err = f.Err.Error()
		}
		out.Findings = append(out.Findings, fd)
	}
	sort.SliceStable(out.Findings, func(i, j int) bool { return out.Findings[i].Oracle < out.Findings[j].Oracle })
	return out
}

// coverageOf extracts log-template and state-abstraction coverage from a
// world's artifacts.
//
// An UNOBSERVED world contributes an EMPTY coverage set rather than whatever its
// truncated boot log happens to contain. search.Corpus.Add refuses it anyway,
// but building the set at all would be one edit away from folding it in.
func (e *Engine) coverageOf(o control.WorldOutcome) *search.Coverage {
	obs := search.WorldObservation{}
	if !hasDrive(o.Phases) {
		return search.Observe(obs)
	}
	obs.Logs = readNodeLogs(o.Paths.Logs)
	if doc, err := readTelemetryDoc(o.Paths); err == nil {
		obs.Telemetry = doc
	}
	return search.Observe(obs)
}

// observationsOf builds OBSERVE's per-node input from a world's telemetry.
func (e *Engine) observationsOf(o control.WorldOutcome) []saboteur.Observation {
	doc, err := readTelemetryDoc(o.Paths)
	if err != nil || doc == nil {
		return nil
	}
	win := saboteur.Window{}
	for _, rf := range o.Realized {
		if win.StartMS == 0 && win.EndMS == 0 {
			win.StartMS, win.EndMS = rf.StartMS, rf.EndMS
			continue
		}
		if rf.StartMS < win.StartMS {
			win.StartMS = rf.StartMS
		}
		if rf.EndMS > win.EndMS {
			win.EndMS = rf.EndMS
		}
	}
	out := make([]saboteur.Observation, 0, len(doc.Nodes))
	for i := range doc.Nodes {
		ns := &doc.Nodes[i]
		samples := make([]*telemetry.Sample, 0, len(ns.Samples))
		for j := range ns.Samples {
			samples = append(samples, &ns.Samples[j])
		}
		ob := saboteur.Observation{
			Node:  ns.Node,
			Fault: win,
			Metrics: []saboteur.Series{
				saboteur.QueueDepthSeries(samples, ns.Node),
				saboteur.RSSSeries(samples, ns.Node),
			},
			RoleChanges:   roleChanges(samples),
			RolesObserved: rolesObserved(samples),
			// The rate the samples were ACTUALLY taken at, read from the
			// document rather than assumed. See Observation.SampleIntervalMS.
			SampleIntervalMS: doc.IntervalMS,
		}
		out = append(out, ob)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Node < out[j].Node })
	return out
}

// roleChanges reads observed role transitions out of a node's samples.
//
// A sample with no role is a GAP, not a transition to "": the status endpoint
// not answering is exactly what a paused node looks like, and recording that as
// "the node stopped being leader" would manufacture the role change A.3 counts
// as evidence.
func roleChanges(samples []*telemetry.Sample) []saboteur.RoleChange {
	var out []saboteur.RoleChange
	prev := ""
	for _, s := range samples {
		if s == nil || s.Status == nil || s.Status.Role == nil {
			continue
		}
		role := *s.Status.Role
		if prev != "" && role != prev && s.VMS != nil {
			out = append(out, saboteur.RoleChange{TMS: *s.VMS, From: prev, To: role})
		}
		prev = role
	}
	return out
}

func rolesObserved(samples []*telemetry.Sample) bool {
	for _, s := range samples {
		if s != nil && s.Status != nil && s.Status.Role != nil {
			return true
		}
	}
	return false
}

func readTelemetryDoc(p control.WorldPaths) (*telemetry.Document, error) {
	data, err := os.ReadFile(control.TelemetryDocPath(p))
	if err != nil {
		return nil, err
	}
	return telemetry.ParseDocument(data)
}

// readNodeLogs reads every per-node log file a world collected.
//
// A log that could not be read yields no lines rather than an error: log
// collection never fails a world (a missing log makes a causal timeline thinner,
// not wrong), and the coverage signal follows the same rule.
func readNodeLogs(dir string) []search.NodeLog {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	out := make([]search.NodeLog, 0, len(ents))
	for _, ent := range ents {
		if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".log") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, ent.Name()))
		if err != nil {
			continue
		}
		node := strings.TrimSuffix(ent.Name(), ".log")
		lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
		kept := lines[:0]
		for _, l := range lines {
			if strings.TrimSpace(l) != "" {
				kept = append(kept, l)
			}
		}
		out = append(out, search.NodeLog{Node: node, Lines: append([]string(nil), kept...)})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Node < out[j].Node })
	return out
}
