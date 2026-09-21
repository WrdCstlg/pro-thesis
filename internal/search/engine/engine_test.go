package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/WrdCstlg/pro-thesis/internal/control"
	"github.com/WrdCstlg/pro-thesis/internal/perturber"
	"github.com/WrdCstlg/pro-thesis/internal/search"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

func engineTestConfig(t *testing.T, search bool) *schema.Config {
	t.Helper()
	w := -1
	return &schema.Config{
		Version: schema.ConfigVersion,
		Name:    "engine-test",
		Harness: schema.HarnessConfig{
			Backend: schema.BackendCompose,
			File:    "docker-compose.yaml",
			Nodes: []schema.NodeConfig{
				{ID: "kv-n1", Service: "kv"},
				{ID: "kv-n2", Service: "kv"},
				{ID: "kv-n3", Service: "kv"},
			},
		},
		Driver: schema.DriverConfig{
			Cmd:      "echo test",
			Profiles: map[string]schema.DriverProfile{"linear": {Clients: 1, Ops: 1}},
		},
		Perturber: schema.PerturberConfig{
			Budget: schema.PerturberBudget{MaxConcurrentFaults: 3, MaxFaultsPerWorld: 24},
			Allow: []schema.FaultKind{
				schema.FaultNetPartition, schema.FaultNetLatency, schema.FaultNetLoss,
				schema.FaultProcPause, schema.FaultIOLatency,
			},
			Constraints: []string{"never partition more than minority of kv"},
		},
		Profiles: map[string]schema.Profile{
			"search": {
				Budget:        schema.Duration(10 * time.Minute),
				Worlds:        &w,
				DriverProfile: "linear",
				Search:        search,
			},
		},
		Oracles:   schema.OraclesConfig{Builtin: []schema.BuiltinOracle{}},
		Artifacts: schema.ArtifactsConfig{Dir: t.TempDir()},
		// A.10's defaults, as schema.DecodeConfig applies them. A hand-built
		// Config leaves `search:` zero, and the engine REFUSES that rather than
		// inventing utility weights nobody chose: see
		// TestAnUndefaultedSearchBlockIsRefused for the hazard that closes.
		Search: schema.SearchConfig{
			Strategy:            schema.StrategySaboteur,
			ExplorationConstant: schema.DefaultExplorationConstant,
			ProbeBudgetPct:      schema.DefaultProbeBudgetPct,
			MaxMCTSDepth:        schema.DefaultMaxMCTSDepth,
			Utility: schema.UtilityConfig{
				ViolationWeight: 100, NoveltyWeight: 10, FaultPenalty: 3, DurationPenalty: 0.1,
			},
			Observe: schema.ObserveConfig{SampleIntervalMS: 500, SpiralWindow: 5},
		},
	}
}

// A `search:` block that never had its defaults applied is REFUSED.
//
// The zero value is reachable only from a hand-built Config, and a
// violation_weight of 0 makes the Saboteur score a world that found a
// consistency bug exactly equal to one that found nothing: the ranked #1
// failure mode arriving through a struct literal. Defaulting silently would
// substitute A.6's literals for a block the operator may have written on
// purpose.
func TestAnUndefaultedSearchBlockIsRefused(t *testing.T) {
	cfg := engineTestConfig(t, true)
	cfg.Search = schema.SearchConfig{}
	_, err := New(Options{Runner: newEngineRunner(t, cfg), Quiet: true})
	if err == nil {
		t.Fatal("the engine accepted a config whose `search:` block was never defaulted")
	}
	if !strings.Contains(err.Error(), "max_mcts_depth") {
		t.Fatalf("the refusal does not name what is wrong: %v", err)
	}
}

func newEngineRunner(t *testing.T, cfg *schema.Config) *control.Runner {
	t.Helper()
	r, err := control.NewRunner(control.RunnerOptions{
		Config:     cfg,
		ProjectDir: t.TempDir(),
		Profile:    "search",
		Seed:       42,
		Quiet:      true,
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	return r
}

// ---------------------------------------------------------------------------
// D-028 item 7: WHETHER versus WHICH
// ---------------------------------------------------------------------------

// The PROFILE's boolean decides whether a search runs. `thesis gate` is a
// fixed-budget pre-commit check whose whole value is being a repeatable signal,
// so it must never run an adaptive MCTS search, and the enforcement lives here,
// not in a caller's good intentions.
func TestAProfileThatDoesNotEnableSearchIsRefused(t *testing.T) {
	_, err := New(Options{Runner: newEngineRunner(t, engineTestConfig(t, false)), Quiet: true})
	if err == nil {
		t.Fatal("the engine ran a search on a profile with `search: false`")
	}
	if !strings.Contains(err.Error(), "search") {
		t.Fatalf("the refusal does not explain itself: %v", err)
	}
}

func TestAProfileThatEnablesSearchIsAccepted(t *testing.T) {
	if _, err := New(Options{
		Runner: newEngineRunner(t, engineTestConfig(t, true)), Quiet: true,
	}); err != nil {
		t.Fatalf("New: %v", err)
	}
}

// The top-level block decides WHICH. An unknown name is refused rather than
// silently defaulted: defaulting would run a different search from the one the
// operator asked for and report the one they asked for.
func TestTheStrategyNameSelectsTheStrategy(t *testing.T) {
	for _, tc := range []struct {
		name schema.SearchStrategy
		want schema.SearchStrategy
	}{
		{schema.StrategyRandom, schema.StrategyRandom},
		{schema.StrategySaboteur, schema.StrategySaboteur},
		// `hybrid` is A.10's third name and is not a separate implementation:
		// the Saboteur probes, escalates, and falls back to stochastic corpus
		// mutation when nothing reinforces, which IS A.5's hybrid.
		{schema.StrategyHybrid, schema.StrategySaboteur},
	} {
		e, err := New(Options{
			Runner: newEngineRunner(t, engineTestConfig(t, true)), Strategy: tc.name, Quiet: true,
		})
		if err != nil {
			t.Fatalf("New(%s): %v", tc.name, err)
		}
		if got := e.Strategy().Name(); got != tc.want {
			t.Fatalf("strategy %q built %q, want %q", tc.name, got, tc.want)
		}
	}
	if _, err := New(Options{
		Runner: newEngineRunner(t, engineTestConfig(t, true)), Strategy: "nonesuch", Quiet: true,
	}); err == nil {
		t.Fatal("an unknown strategy name was accepted")
	}
}

// ---------------------------------------------------------------------------
// The shared space (D-029 fairness)
// ---------------------------------------------------------------------------

// A kind this platform cannot deliver is removed from the SHARED space, so both
// arms of the pre-registered comparison lose exactly the same kinds. Restricting
// only the Saboteur would hand it a cleaner space and make the benchmark measure
// the filter rather than the strategy.
func TestPlatformRestrictionAppliesToTheSharedSpace(t *testing.T) {
	cfg := engineTestConfig(t, true)
	top, err := perturber.NewTopology(cfg, nil)
	if err != nil {
		t.Fatalf("topology: %v", err)
	}
	sp, err := search.NewSpace(cfg, top, search.DefaultWindowPolicy())
	if err != nil {
		t.Fatalf("NewSpace: %v", err)
	}
	before := len(sp.Kinds)
	dropped := restrictToPlatform(sp, func(k schema.FaultKind) error {
		if k == schema.FaultIOLatency {
			return errUnsupported
		}
		return nil
	})
	if len(dropped) != 1 {
		t.Fatalf("dropped %v, want exactly io.latency", dropped)
	}
	if len(sp.Kinds) != before-1 {
		t.Fatalf("the space still has %d kinds, want %d", len(sp.Kinds), before-1)
	}
	for _, k := range sp.Kinds {
		if k == schema.FaultIOLatency {
			t.Fatal("io.latency survived the restriction; a world naming it spends a compose " +
				"project and ~30s to reach ErrUnsupported mid-DRIVE")
		}
	}
	if _, ok := sp.Targets[schema.FaultIOLatency]; ok {
		t.Fatal("the dropped kind still has targets; a generator reading Targets would resurrect it")
	}
}

// Emptying the space entirely is refused rather than done silently: a search
// over an empty space reports worlds it never perturbed.
func TestRestrictionNeverEmptiesTheSpaceSilently(t *testing.T) {
	cfg := engineTestConfig(t, true)
	top, _ := perturber.NewTopology(cfg, nil)
	sp, err := search.NewSpace(cfg, top, search.DefaultWindowPolicy())
	if err != nil {
		t.Fatalf("NewSpace: %v", err)
	}
	before := append([]schema.FaultKind(nil), sp.Kinds...)
	restrictToPlatform(sp, func(schema.FaultKind) error { return errUnsupported })
	if len(sp.Kinds) != len(before) {
		t.Fatalf("the space was emptied to %d kinds; a search over an empty space would report "+
			"worlds it never perturbed", len(sp.Kinds))
	}
}

var errUnsupported = unsupportedErr{}

type unsupportedErr struct{}

func (unsupportedErr) Error() string { return "not deliverable here" }

// ---------------------------------------------------------------------------
// Reading a world back (the ranked #1 failure mode)
// ---------------------------------------------------------------------------

func drivePhases() schema.PhaseTimings {
	return schema.PhaseTimings{{Phase: schema.PhaseDrive, StartMS: 0, EndMS: 5000}}
}

// A world that never reached DRIVE produced a truncated boot log. Folding that
// into coverage teaches the search that failing to boot is novel, and the corpus
// fills with worlds that break the harness rather than the target.
func TestAWorldThatNeverReachedDriveIsNotObserved(t *testing.T) {
	out := OutcomeOf(control.WorldOutcome{
		Ordinal: 1,
		Outcome: control.OutcomeHarnessError,
		Phases:  schema.PhaseTimings{{Phase: schema.PhaseBoot, StartMS: -1000, EndMS: 0}},
	}, schema.NewWorld(1, "default", "linear"))
	if out.Observed {
		t.Fatal("a world that never reached DRIVE was marked observed")
	}
	if out.Signal() != search.SignalUnknown {
		t.Fatalf("signal = %s, want unknown", out.Signal())
	}
}

// A harness error is not evidence about the system, even when the world got far
// enough to run.
func TestAHarnessErrorIsNeverObserved(t *testing.T) {
	out := OutcomeOf(control.WorldOutcome{
		Ordinal: 1,
		Outcome: control.OutcomeHarnessError,
		Phases:  drivePhases(),
	}, schema.NewWorld(1, "default", "linear"))
	if out.Observed {
		t.Fatal("a world whose harness failed was marked observed; its logs are not evidence " +
			"about the system under test")
	}
}

// An oracle that could not RUN is inconclusive, never ok, even if its status
// field happens to say ok.
func TestAnOracleErrorReachesTheOutcome(t *testing.T) {
	out := OutcomeOf(control.WorldOutcome{
		Ordinal: 1,
		Outcome: control.OutcomePass,
		Phases:  drivePhases(),
		Findings: []control.OracleResult{{
			Output: schema.OracleOutput{Oracle: "x", Class: schema.ClassCrash, Status: schema.StatusOK},
			Err:    errUnsupported,
		}},
	}, schema.NewWorld(1, "default", "linear"))
	if out.Signal() != search.SignalUnknown {
		t.Fatalf("signal = %s; an oracle that could not be run must not read as a pass", out.Signal())
	}
}

// Severity is assigned by the ENGINE from the class, never read from the oracle.
// Letting an oracle grade its own finding down would let it turn the Saboteur
// away from a bug it just found.
func TestSeverityIsDerivedFromTheClass(t *testing.T) {
	out := OutcomeOf(control.WorldOutcome{
		Ordinal: 1,
		Outcome: control.OutcomeViolation,
		Phases:  drivePhases(),
		Findings: []control.OracleResult{{
			Output: schema.OracleOutput{
				Oracle: "linearizable.kv", Class: schema.ClassConsistency,
				Status: schema.StatusViolated,
			},
			ObservedPhase: schema.PhaseDrive,
		}},
	}, schema.NewWorld(1, "default", "linear"))
	v := out.Violations()
	if len(v) != 1 {
		t.Fatalf("got %d violations", len(v))
	}
	if v[0].Severity != schema.DefaultSeverity(schema.ClassConsistency) {
		t.Fatalf("severity = %s, want the class default %s",
			v[0].Severity, schema.DefaultSeverity(schema.ClassConsistency))
	}
}

// Findings are ordered by oracle name so a ranking cannot inherit the order the
// engine happened to collect them in.
func TestFindingsAreOrderedByOracleName(t *testing.T) {
	out := OutcomeOf(control.WorldOutcome{
		Ordinal: 1, Outcome: control.OutcomePass, Phases: drivePhases(),
		Findings: []control.OracleResult{
			{Output: schema.OracleOutput{Oracle: "zeta", Class: schema.ClassCrash, Status: schema.StatusOK}},
			{Output: schema.OracleOutput{Oracle: "alpha", Class: schema.ClassCrash, Status: schema.StatusOK}},
		},
	}, schema.NewWorld(1, "default", "linear"))
	if out.Findings[0].Oracle != "alpha" {
		t.Fatalf("findings are not name-ordered: %s first", out.Findings[0].Oracle)
	}
}

// The termination policy the engine applies is OQ-004's, not A.5's literal one.
func TestTheEngineAppliesThePrefixClosedTerminationRule(t *testing.T) {
	consistencyInDrive := OutcomeOf(control.WorldOutcome{
		Ordinal: 1, Outcome: control.OutcomeViolation, Phases: drivePhases(),
		Findings: []control.OracleResult{{
			Output: schema.OracleOutput{
				Oracle: "linearizable.kv", Class: schema.ClassConsistency,
				Status: schema.StatusViolated,
			},
			ObservedPhase: schema.PhaseDrive,
		}},
	}, schema.NewWorld(1, "default", "linear"))
	if terminationFor(consistencyInDrive).Terminates() {
		t.Fatal("the engine terminated on a consistency violation observed in DRIVE; " +
			"consistency is not prefix-closed and the rollout must run to ASSERT (OQ-004)")
	}

	crashInDrive := OutcomeOf(control.WorldOutcome{
		Ordinal: 1, Outcome: control.OutcomeViolation, Phases: drivePhases(),
		Findings: []control.OracleResult{{
			Output: schema.OracleOutput{
				Oracle: "no_crash", Class: schema.ClassCrash, Status: schema.StatusViolated,
			},
			ObservedPhase: schema.PhaseDrive,
		}},
	}, schema.NewWorld(1, "default", "linear"))
	if !terminationFor(crashInDrive).Terminates() {
		t.Fatal("the engine did not terminate on a prefix-closed crash violation")
	}
}

// ---------------------------------------------------------------------------
// Budget narrowing
// ---------------------------------------------------------------------------

// --budget and --worlds may only NARROW the profile's. Widening either from
// argv would be a gate-weakening lever that never touches prothesis.yaml.
func TestTheCLIBudgetOnlyNarrows(t *testing.T) {
	cfg := engineTestConfig(t, true)
	prof := cfg.Profiles["search"]

	e, err := New(Options{
		Runner: newEngineRunner(t, cfg), Budget: 2 * time.Hour, Worlds: 9999, Quiet: true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if e.budget.Wall != prof.Budget.Std() {
		t.Fatalf("a 2h CLI budget widened the profile's %s to %s", prof.Budget.Std(), e.budget.Wall)
	}
	if e.budget.RequiredWall != prof.Budget.Std() || e.budget.Wall != e.budget.RequiredWall {
		t.Error("a refused wall widening did not leave the profile's own budget in force")
	}
	// NOTE: e.budget.Narrowed is TRUE here, and correctly so. This profile's
	// world count is unbounded, so `--worlds 9999` caps something that had no
	// ceiling: that is a narrowing whatever the number. The conservatism is
	// deliberate and is recorded in D-059: nothing at budget-resolution time
	// knows how many worlds fit in the wall budget, so a cap is treated as
	// binding. An unbounded profile that ends on wall expiry reports budget
	// exhaustion rather than PASS in any case.

	e2, err := New(Options{
		Runner: newEngineRunner(t, cfg), Budget: time.Minute, Worlds: 3, Quiet: true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if e2.budget.Wall != time.Minute {
		t.Fatalf("a 1m CLI budget did not narrow the profile's: got %s", e2.budget.Wall)
	}
	if e2.budget.Worlds != 3 {
		t.Fatalf("--worlds 3 did not narrow an unbounded profile: got %d", e2.budget.Worlds)
	}

	// D-059. resolveBudget was a hand-copied duplicate of Runner.Run's rule, so
	// a search narrowed from argv kept reporting PASS after the runner stopped.
	// The engine builds its own verdict from this budget, so the flag has to
	// survive the copy, which is why there is now only one copy.
	if !e2.budget.Narrowed {
		t.Fatal("a narrowed search budget was not recorded as narrowed; the engine builds its " +
			"own verdict from this snapshot, so a clean narrowed search would report PASS")
	}
	if e2.budget.RequiredWall != prof.Budget.Std() {
		t.Errorf("the profile's wall requirement was lost: got %s, want %s",
			e2.budget.RequiredWall, prof.Budget.Std())
	}
}

// hasDrive is the line between "a world that observed the system" and "a world
// that failed to boot", and it must not be fooled by a zero-length window.
func TestHasDriveNeedsADriveWindow(t *testing.T) {
	if hasDrive(schema.PhaseTimings{{Phase: schema.PhaseBoot, StartMS: -1, EndMS: 0}}) {
		t.Fatal("hasDrive accepted phase timings with no DRIVE window")
	}
	if !hasDrive(drivePhases()) {
		t.Fatal("hasDrive rejected a real DRIVE window")
	}
	// A world that entered DRIVE and was cut short still OBSERVED something.
	if !hasDrive(schema.PhaseTimings{{Phase: schema.PhaseDrive, StartMS: 0, EndMS: 0}}) {
		t.Fatal("hasDrive rejected a zero-length DRIVE window; the world still reached DRIVE")
	}
}

// ---------------------------------------------------------------------------
// search.strategy: llm (D-075)
// ---------------------------------------------------------------------------

// The llm strategy is built through the same seam as the others, and its
// single-worker requirement is enforced at construction: the strategy carries
// per-ordinal feedback state, so a parallel pool would interleave proposals
// and observations out of order.
func TestTheLLMStrategyIsBuiltWithOneWorker(t *testing.T) {
	cfg := engineTestConfig(t, true)
	cfg.Search.LLM = schema.DefaultLLMConfig()

	e, err := New(Options{
		Runner: newEngineRunner(t, cfg), Strategy: schema.StrategyLLM, Quiet: true,
	})
	if err != nil {
		t.Fatalf("New(llm): %v", err)
	}
	if got := e.Strategy().Name(); got != schema.StrategyLLM {
		t.Fatalf("strategy built %q, want %q", got, schema.StrategyLLM)
	}
	if e.opts.Workers != 1 {
		t.Fatalf("an unspecified --workers left opts.Workers = %d, want it pinned to 1", e.opts.Workers)
	}

	if _, err := New(Options{
		Runner: newEngineRunner(t, cfg), Strategy: schema.StrategyLLM, Workers: 4, Quiet: true,
	}); err == nil {
		t.Fatal("strategy llm accepted --workers 4: proposals and observations would interleave")
	} else if !strings.Contains(err.Error(), "workers") {
		t.Fatalf("the refusal does not name workers: %v", err)
	}

	if _, err := New(Options{
		Runner: newEngineRunner(t, cfg), Strategy: schema.StrategyLLM, Workers: 1, Quiet: true,
	}); err != nil {
		t.Fatalf("New(llm, workers=1): %v", err)
	}
}
