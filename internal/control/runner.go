package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/WrdCstlg/pro-thesis/internal/buildinfo"
	"github.com/WrdCstlg/pro-thesis/internal/driver"
	"github.com/WrdCstlg/pro-thesis/internal/harness"
	"github.com/WrdCstlg/pro-thesis/internal/oracle"
	"github.com/WrdCstlg/pro-thesis/internal/perturber"
	"github.com/WrdCstlg/pro-thesis/internal/probehost"
	"github.com/WrdCstlg/pro-thesis/internal/recorder"
	"github.com/WrdCstlg/pro-thesis/internal/telemetry"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// RunnerOptions configures the control plane runner.
type RunnerOptions struct {
	ConfigPath string
	Config     *schema.Config
	ProjectDir string
	Profile    string
	Seed       uint64
	RunID      string

	Backend       harness.Backend
	Driver        Driver
	Telemetry     Telemetry
	OracleEngine  OracleEngine
	SteadyState   SteadyStateProber
	LogCollector  LogCollector
	ImageResolver ImageResolver
	Clock         recorder.Clock

	// Faults is the world's PLANNED fault schedule: canonical fault strings in
	// the KIND(TARGET[, PARAMS])@start..end grammar. Empty means a no-fault
	// world, which is Phase 1's smoke case and remains valid.
	//
	// Phase 2 authors it (`thesis run --fault ...`); Phase 4's search will
	// generate it. Either way it reaches the world file's
	// fault_schedule.planned unchanged, and what actually happened reaches
	// fault_schedule.realized.
	Faults []string

	// Injectors are the fault-family mechanisms, from internal/perturber/faults.
	//
	// An empty set with a non-empty Faults list is a HARD FAILURE, not a
	// degradation: a world whose faults never fired would satisfy every oracle
	// and report PASS over a system nobody perturbed.
	Injectors []perturber.Injector

	// Budget and Worlds override the profile's, and may only NARROW it. Zero
	// means "use the profile". See Run for why widening is refused.
	Budget time.Duration
	Worlds int

	// DrainDeadline bounds QUIESCE's drain: how long the driver is given to
	// finish its outstanding operations after being asked to stop issuing.
	//
	// Zero means DefaultDrainDeadline. A NEGATIVE value disables draining and
	// restores the Phase 1 behaviour of killing the driver outright, which is
	// available deliberately, because a project whose driver cannot drain would
	// otherwise pay the full deadline on every world for nothing. See drain.go
	// and OQ-033.
	DrainDeadline time.Duration

	// OracleLock is the drift check the caller already performed, verbatim, for
	// the verdict's `oracle_lock` field.
	//
	// The check itself lives in internal/lock and is made by the CLI BEFORE a
	// runner exists, because a drifted gate must not boot a topology (D-I). The
	// runner does not repeat it and does not invent it: a zero value normalizes
	// to "absent", which is the truth for any caller that did not check.
	OracleLock schema.OracleLock

	Stdout io.Writer
	Stderr io.Writer
	Quiet  bool
}

// Runner executes one run across one or more worlds according to the budget.
type Runner struct {
	opts    RunnerOptions
	cfg     *schema.Config
	profile string
	// driverProfile is profiles.<profile>.driver_profile, which is a DIFFERENT
	// namespace from the run profile. See the note in runWorld's DRIVE step.
	driverProfile string
	seed          uint64
	runID         string
	runDir        string

	backend       harness.Backend
	driver        Driver
	telemetry     Telemetry
	oracleEngine  OracleEngine
	steadyState   SteadyStateProber
	logCollector  LogCollector
	imageResolver ImageResolver
	clock         recorder.Clock

	faults []string

	// injectors is the EXPLICIT override from RunnerOptions and is read-only
	// after construction. It used to be reassigned per world, which made two
	// concurrent worlds race on it and made a serial world's fault mechanisms
	// depend on the previous world's: the exact hazard D-032 records. The
	// per-world set now lives on the stack of the world that built it.
	injectors []perturber.Injector

	// drainDeadline is RunnerOptions.DrainDeadline, normalized: positive is a
	// bound, negative means draining is off.
	drainDeadline time.Duration

	stdout io.Writer
	stderr io.Writer
	quiet  bool
}

// NewRunner creates a new Runner with defaults for any omitted subsystems.
func NewRunner(opts RunnerOptions) (*Runner, error) {
	if opts.Config == nil {
		return nil, fmt.Errorf("control: RunnerOptions requires Config")
	}
	cfg := opts.Config
	projDir := opts.ProjectDir
	if projDir == "" {
		if opts.ConfigPath != "" {
			projDir = filepath.Dir(opts.ConfigPath)
		} else {
			projDir = "."
		}
	}
	absProjDir, err := filepath.Abs(projDir)
	if err != nil {
		return nil, fmt.Errorf("control: resolve project dir: %w", err)
	}

	profile := opts.Profile
	if profile == "" {
		profile = "smoke"
	}
	profCfg, ok := cfg.Profiles[profile]
	if !ok {
		return nil, fmt.Errorf("control: unknown profile %q in prothesis.yaml", profile)
	}
	// `profiles.<name>.driver_profile` names an entry of `driver.profiles`.
	// Config validation makes it MANDATORY and cross-checks that it resolves, so
	// an empty value here means the caller supplied a config that never went
	// through Validate: refused rather than defaulted, because defaulting to
	// the run profile's own name is precisely the coincidence that hid the
	// discarded-plan defect described in runWorld's DRIVE step.
	driverProfile := profCfg.DriverProfile
	if driverProfile == "" {
		return nil, fmt.Errorf("control: profile %q has no driver_profile; it names an entry of "+
			"driver.profiles and is mandatory (run the config through Validate)", profile)
	}
	if _, ok := cfg.Driver.Profiles[driverProfile]; !ok {
		return nil, fmt.Errorf("control: profile %q names driver_profile %q, which is not an entry "+
			"of driver.profiles", profile, driverProfile)
	}

	seed := opts.Seed
	if seed == 0 {
		seed = uint64(time.Now().UnixNano())
	}

	runsDir := cfg.Artifacts.Dir
	if !filepath.IsAbs(runsDir) {
		if runsDir == "" {
			runsDir = recorder.RunsDir(absProjDir)
		} else {
			runsDir = filepath.Join(absProjDir, runsDir)
		}
	}

	runID := opts.RunID
	runDir := ""
	if runID != "" {
		runDir = filepath.Join(runsDir, runID)
		if err := os.MkdirAll(runDir, 0o755); err != nil {
			return nil, fmt.Errorf("control: create run dir %s: %w", runDir, err)
		}
	} else {
		id, dir, err := recorder.AllocRunID(runsDir, time.Now().UTC())
		if err != nil {
			return nil, fmt.Errorf("control: alloc run id in %s: %w", runsDir, err)
		}
		runID = id
		runDir = dir
	}

	b := opts.Backend
	if b == nil {
		var err error
		b, err = harness.New(cfg)
		if err != nil {
			return nil, err
		}
	}

	drv := opts.Driver
	if drv == nil {
		drv = &defaultDriverSupervisor{}
	}

	tel := opts.Telemetry
	if tel == nil {
		tel = &defaultTelemetrySampler{}
	}

	oe := opts.OracleEngine
	if oe == nil {
		oe = &defaultOracleEngine{}
	}

	st := opts.SteadyState
	if st == nil {
		st = &ExecSteadyState{}
	}

	lc := opts.LogCollector
	if lc == nil {
		lc = NewDockerLogCollector()
	}

	// Defaults to the daemon, like the log collector: a caller that wants no
	// daemon (every unit world) has to say so. See ImageResolver in seams.go.
	ir := opts.ImageResolver
	if ir == nil {
		ir = dockerImageResolver{}
	}

	clk := opts.Clock
	if clk == nil {
		clk = recorder.NewRealClock(recorder.RealClockOptions{SkipCalibration: true})
	}

	out := opts.Stdout
	if out == nil {
		out = os.Stdout
	}
	errOut := opts.Stderr
	if errOut == nil {
		errOut = os.Stderr
	}
	// Serialized, because the parallel executor has several worlds writing
	// progress lines at once and an interleaved half-line is worse than no line.
	// os.Stderr survives concurrent Write on both platforms; a *bytes.Buffer in a
	// test does not, and that is the writer this most often is.
	errOut = newSyncWriter(errOut)

	drainDeadline := opts.DrainDeadline
	if drainDeadline == 0 {
		drainDeadline = DefaultDrainDeadline
	}

	return &Runner{
		opts:          opts,
		cfg:           cfg,
		profile:       profile,
		driverProfile: driverProfile,
		seed:          seed,
		runID:         runID,
		runDir:        runDir,
		backend:       b,
		driver:        drv,
		telemetry:     tel,
		oracleEngine:  oe,
		steadyState:   st,
		logCollector:  lc,
		imageResolver: ir,
		clock:         clk,
		faults:        append([]string(nil), opts.Faults...),
		injectors:     append([]perturber.Injector(nil), opts.Injectors...),

		drainDeadline: drainDeadline,

		stdout: out,
		stderr: errOut,
		quiet:  opts.Quiet,
	}, nil
}

// ---------------------------------------------------------------------------
// Accessors for a caller that drives the runner world by world
//
// Runner.Run owns the whole loop: it decides the schedule, the budget and the
// stopping rule. Phase 4's search owns those instead (it chooses each world
// from a strategy and executes it through Parallel) so it needs the four facts
// Run derives privately: which run bundle to write into, what to call the run,
// what seed every world derives from, and which clock the budget is measured on.
//
// They are read-only. Nothing here lets a caller change what the runner already
// allocated, because the run directory and the run id are on disk and in the
// CURRENT pointer by the time a caller could see them.
// ---------------------------------------------------------------------------

// RunDir is the run bundle this runner writes into.
func (r *Runner) RunDir() string { return r.runDir }

// RunID is the allocated run identifier.
func (r *Runner) RunID() string { return r.runID }

// Seed is the run seed every world's seed derives from (WorldSeedFor).
func (r *Runner) Seed() uint64 { return r.seed }

// Profile is the resolved run profile name.
func (r *Runner) Profile() string { return r.profile }

// Config is the resolved configuration.
func (r *Runner) Config() *schema.Config { return r.cfg }

// ProjectDir is the directory prothesis.yaml lives in.
func (r *Runner) ProjectDir() string { return r.opts.ProjectDir }

// Clock is the runner's clock, so a caller's budget is measured on the same
// monotonic reading the runner's own BudgetTracker would use.
func (r *Runner) Clock() recorder.Clock { return r.clock }

// syncWriter serializes writes to a writer several worlds share.
type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func newSyncWriter(w io.Writer) io.Writer {
	if w == nil {
		return nil
	}
	if _, ok := w.(*syncWriter); ok {
		return w
	}
	return &syncWriter{w: w}
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

// Run executes the run across planned worlds.
func (r *Runner) Run(ctx context.Context) (schema.Verdict, schema.ExitCode, error) {
	if err := os.MkdirAll(r.runDir, 0o755); err != nil {
		return schema.Verdict{}, schema.ExitConfigError, fmt.Errorf("control: create run dir: %w", err)
	}

	profCfg := r.cfg.Profiles[r.profile]
	worlds := -1
	if profCfg.Worlds != nil {
		worlds = *profCfg.Worlds
	}
	budget := Budget{
		Wall:   profCfg.Budget.Std(),
		Worlds: worlds,
	}
	// `--budget` and `--worlds` narrow the profile; see ApplyCLIOverrides for
	// why narrowing is permitted but is not safe, and D-059 for the correction
	// to D-033 that claimed it was. The two consequences (no PASS and no
	// `oracle_lock: ok`) are applied by BuildVerdict from the budget snapshot,
	// so every command that builds a verdict inherits them.
	budget = ApplyCLIOverrides(budget, r.opts.Budget, r.opts.Worlds)
	if budget.Narrowed && !r.quiet {
		fmt.Fprintf(r.stderr, "thesis: WARNING: the command line narrowed profile %q "+
			"(worlds %s -> %s, wall %s -> %s). A run that finds no violation under a "+
			"narrowed budget cannot report PASS, because it did not perform the gate the "+
			"profile specifies.\n",
			r.profile,
			describeWorlds(budget.RequiredWorlds), describeWorlds(budget.Worlds),
			describeWall(budget.RequiredWall), describeWall(budget.Wall))
	}
	tracker := NewBudgetTracker(budget, r.clock)

	var (
		worldResults []worldResult
		allFindings  []OracleResult
		runOutcome   = OutcomePass
	)

	ordinal := 0
	for {
		canStart, reason := tracker.MayStartWorld()
		if !canStart {
			if !r.quiet && reason != "" {
				fmt.Fprintf(r.stderr, "thesis: stopping world loop: %s\n", reason)
			}
			break
		}

		if ctx.Err() != nil {
			runOutcome = OutcomeCanceled
			break
		}

		tracker.NoteWorldStarted()
		spec := r.newWorldSpec(ordinal+1, r.faults)

		if !r.quiet {
			fmt.Fprintf(r.stderr, "thesis: entering world %d (seed %d, profile %s)\n",
				spec.ordinal, spec.seed, r.profile)
		}

		wres, err := r.runWorld(ctx, spec)
		tracker.NoteWorldFinished()
		if err != nil {
			if !r.quiet {
				fmt.Fprintf(r.stderr, "thesis: world %d error: %v\n", ordinal+1, err)
			}
		}
		worldResults = append(worldResults, wres)
		allFindings = append(allFindings, wres.findings...)

		// Fold world outcome
		if wres.outcome.Rank() > runOutcome.Rank() {
			runOutcome = wres.outcome
		}

		// If violation found, stop subsequent worlds (work from minimal repro)
		if wres.outcome == OutcomeViolation {
			break
		}

		ordinal++
	}

	// Check if budget expired without finding violations
	if runOutcome == OutcomePass && tracker.WallExpired() {
		runOutcome = BudgetOutcome(false)
	}

	// The verdict names the binary's build stamp when it carries one; the
	// working tree's HEAD is only the fallback for an unstamped binary. What
	// executed is the binary, not the tree. See internal/buildinfo.
	commit := buildinfo.VerdictCommit(GitCommit(ctx, r.opts.ProjectDir))

	// Build violations list from findings
	var violations []schema.Violation
	vIdx := 1
	for _, f := range allFindings {
		if f.Violated() {
			vID := ViolationID(vIdx)
			vIdx++

			var timelineEvents []schema.TimelineEvent
			for _, wres := range worldResults {
				for _, wf := range wres.findings {
					if wf.Output.Oracle == f.Output.Oracle && wf.Violated() {
						var witnessOpIDs []int64
						for _, id := range f.Output.Witness.OpIDs {
							witnessOpIDs = append(witnessOpIDs, id)
						}
						witnessOps, _ := LoadWitnessOps(wres.paths.History, witnessOpIDs)
						src := TimelineSource{
							Phases: wres.phases,
							// What was ACTUALLY injected, against which concrete
							// node, at which virtual millisecond. A causal
							// timeline built from the planned schedule would name
							// `role:leader` where the reader needs `kv-n2`.
							Realized: wres.realized,
							Ops:      witnessOps,
							Logs:     wres.logs,
						}
						timelineEvents = BuildCausalTimeline(src, f, wres.timeline)
						break
					}
				}
				if len(timelineEvents) > 0 {
					break
				}
			}

			violations = append(violations, NewViolation(vID, f, timelineEvents))
		}
	}

	vInput := VerdictInput{
		RunID:      r.runID,
		Profile:    r.profile,
		Commit:     commit,
		Outcome:    runOutcome,
		Budget:     tracker.Snapshot(),
		Violations: violations,
		Artifacts:  RelativeArtifacts(r.opts.ProjectDir, r.runDir),
		OracleLock: r.opts.OracleLock,
	}
	verdict := BuildVerdict(vInput)

	// Write verdict.json
	verdictPath := filepath.Join(r.runDir, VerdictFileName)
	_ = WriteVerdictFile(verdictPath, &verdict)

	return verdict, verdict.Verdict.ExitCode(), nil
}

// worldSpec is everything ONE world's execution needs that no other world
// shares.
//
// Every field here used to be read off the Runner mid-flight. Moving them out is
// what makes concurrent worlds independent rather than merely simultaneous: a
// Runner field reassigned during a world (r.injectors was exactly that) is both
// a data race between goroutines and a hidden dependency between worlds even
// when they run one at a time.
type worldSpec struct {
	// ordinal is 1-based, and is what the seed and the world directory derive
	// from. It is the world's identity, not its position in a queue: world 7
	// runs identically whether it is executed seventh or first.
	ordinal int
	seed    uint64
	paths   WorldPaths
	// faults is this world's PLANNED schedule. Phase 4's search varies it per
	// world; a plain `thesis run` gives every world the same list.
	faults []string
	// plan overrides the driver plan for this world (Phase 5 workload reduction).
	plan *driver.Plan
	// env is the per-world environment for compose and for the driver. Empty
	// for a serial run; the parallel executor fills it so each world publishes
	// its own host ports (D-042).
	env []string
	// project overrides the compose project name; empty derives the default.
	project string
	// overlay overrides the generated compose overlay path.
	overlay string
	// slot is the parallel worker slot, or -1 for a serial world.
	slot int
	// expectPorts is the published host port this world's driver will be TOLD to
	// use, per node id: the slot's own band. Empty for a serial world, which
	// publishes whatever the compose file says and tells the driver the same.
	//
	// It exists so BOOT can check the two halves agree before anything drives.
	// See the check in runWorld.
	expectPorts map[string]int64
	// keepUp leaves the topology standing after this world instead of tearing
	// it down, and persists the up -> down handoff so `thesis down` can still
	// remove it (D-017).
	//
	// It is set ONLY by Runner.RunWorld, i.e. by `thesis replay --keep-up`. The
	// parallel executor never sets it: a kept-up slot would hold two of this
	// host's ~24 bridge networks for the rest of the session and the next world
	// to claim that slot would boot on top of a live project.
	keepUp bool
}

// WorldSeedFor derives world `ordinal`'s seed from the run seed.
//
// It delegates to recorder.WorldSeed, whose derivation is PATH-KEYED
// (DECISIONS.md D-013): the seed is a function of (run seed, ordinal) and of
// nothing else, not of how many worlds ran before it, not of which stream
// domains exist, and NOT of the order in which the worlds were scheduled. That
// last one is why Phase 4 needs it: a parallel executor completes worlds out of
// order, and a seed derived from a running counter would make a world's
// behaviour depend on which worker picked it up.
//
// This replaces an earlier `seed + ordinal*10007`, which had the right shape but
// derived nothing from the recorder and had no such guarantee.
func WorldSeedFor(runSeed uint64, ordinal int) uint64 {
	if ordinal < 1 {
		ordinal = 1
	}
	return uint64(recorder.WorldSeed(recorder.Seed(runSeed), uint64(ordinal-1)))
}

// WorldDirName is the directory one world's artifacts live in, inside the run
// directory. It is a pure function of the ordinal so a parallel run lays its
// artifacts out exactly as a serial one does.
func WorldDirName(ordinal int) string { return fmt.Sprintf("world-%04d", ordinal) }

// newWorldSpec builds the serial world spec for a 1-based ordinal.
func (r *Runner) newWorldSpec(ordinal int, faults []string) worldSpec {
	return worldSpec{
		ordinal: ordinal,
		seed:    WorldSeedFor(r.seed, ordinal),
		paths:   NewWorldPaths(filepath.Join(r.runDir, WorldDirName(ordinal))),
		faults:  append([]string(nil), faults...),
		slot:    -1,
	}
}

type worldResult struct {
	ordinal  int
	seed     uint64
	outcome  Outcome
	phases   schema.PhaseTimings
	history  *oracle.History
	logs     []CollectedLog
	findings []OracleResult
	paths    WorldPaths
	timeline *recorder.Timeline
	// planned and realized are the two halves of the world's fault_schedule.
	// realized is what actually happened and is what a causal timeline and a
	// Phase 5 shrink both read.
	planned  []string
	realized []schema.RealizedFault
	// drain records what QUIESCE's drain did (drain.go, OQ-033).
	drain DrainReport
	// project is the compose project this world used, so a leaked one can be
	// named in the message that refuses to start the next world.
	project string
	// hostPorts is the published host port each node was OBSERVED on, read back
	// from `docker compose port` rather than from what the slot asked for.
	//
	// It is the only direct evidence that two concurrent worlds were actually
	// isolated: the ports a slot REQUESTS prove nothing on their own, because a
	// compose file that ignores the variable would publish the same port for
	// every world and the collision would surface as an unattributable boot
	// failure. Phase 4's OBSERVE also needs it to reach a node.
	hostPorts map[string]int64
	// teardownErr is Backend.Down's own verdict on its own work.
	//
	// It used to be discarded. That was the one place in the tree where a LEAKED
	// COMPOSE PROJECT could pass unnoticed, and on this host a leaked project
	// holds two bridge networks permanently out of about twenty-four, so a
	// search leaking one world in twelve dies partway through with an error that
	// reads like a Docker bug (D-017, OQ-013).
	teardownErr error

	// keptUp reports that TEARDOWN deliberately left this world's topology
	// standing AND persisted the up -> down handoff, so a caller can tell a
	// kept-up world from a leaked one. Only `thesis replay --keep-up` sets it.
	keptUp bool
}

type fileMarkerSink struct {
	f  *os.File
	mu sync.Mutex
}

func (s *fileMarkerSink) AppendLine(line []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.f.Write(append(line, '\n'))
	return err
}

func (s *fileMarkerSink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.f.Close()
}

// teardownStandIn is the minimal topology a VERIFICATION sweep needs when BOOT
// never produced one.
//
// It carries a project name and a compose file and nothing else, which is
// exactly what Backend.Down addresses: the project by name for `compose down`,
// and the io.prothesis / com.docker.compose labels for the sweep that follows
// and for the emptiness check. Against a world that created nothing it finds
// nothing and reports success, which is the honest answer; against one whose
// boot rolled back badly it is the only chance anybody gets to notice.
func (r *Runner) teardownStandIn(spec worldSpec) *recorder.Topology {
	project := spec.project
	if project == "" {
		project = harness.ProjectName(r.cfg, r.opts.ProjectDir)
	}
	if project == "" {
		return nil
	}
	file := r.cfg.Harness.File
	if file != "" && !filepath.IsAbs(file) {
		file = filepath.Join(r.opts.ProjectDir, file)
	}
	return &recorder.Topology{
		Schema:         recorder.TopologySchema,
		RunID:          r.runID,
		Backend:        string(r.cfg.Harness.Backend),
		ComposeFile:    file,
		ComposeProject: project,
	}
}

// portsAgree checks that the ports this world's driver will be TOLD about are
// the ports the harness actually PUBLISHED.
//
// The parallel executor allocates a per-slot port band, exports it as
// PROTHESIS_TARGETS for the driver, and exports it again under whatever compose
// variables the target's file happens to spell, which it can only do if the
// operator supplied the mapping (`--worker-env`). Omit it and the two halves
// disagree silently: the driver dials the slot's band while compose publishes
// the file's own defaults, every operation fails with "connection refused", and
// the world produces a full history in which nothing is checkable. The oracle
// then reports INCONCLUSIVE (correctly, it has nothing to check) and a search
// reading that answer concludes the fault it injected told it nothing.
//
// MEASURED: `thesis search` on the reference fixture without --worker-env
// produced 78,018 operation records over 39,009 op ids, of which 39,009 failed
// and zero were checkable (run r_2026_09_09_23c5). Every Phase 4 result came from a
// script that passed the flag; a bare invocation was broken and said so only as
// "unknown". See OQ-056.
//
// ParallelOptions.VerifyPortsFree does NOT cover this. It refuses when two slots
// would collide on a port, which needs at least two slots; a single worker never
// collides with anyone and sails straight into the mismatch.
//
// A node the slot allocated no port for is SKIPPED rather than failed: not every
// declared node must publish one, and only a node with a real disagreement is
// evidence of anything.
func portsAgree(want map[string]int64, nodes []recorder.NodeBinding) error {
	if len(want) == 0 {
		return nil
	}
	var wrong []string
	for _, n := range nodes {
		exp, ok := want[n.ID]
		if !ok || exp <= 0 {
			continue
		}
		if n.HostPort != exp {
			wrong = append(wrong, fmt.Sprintf("%s: driver was told %d, compose published %d",
				n.ID, exp, n.HostPort))
		}
	}
	if len(wrong) == 0 {
		return nil
	}
	sort.Strings(wrong)
	return fmt.Errorf("this world's published ports are not the ports its driver will use, so every "+
		"operation would fail and the history would be unjudgeable (%s). The compose file "+
		"parameterises its published ports, so the worker slot's band has to be mapped onto the "+
		"variables it spells: pass --worker-env, e.g. --worker-env "+
		"\"prefix=KV_PREFIX,kv-n1=KV_PORT_N1,kv-n2=KV_PORT_N2,kv-n3=KV_PORT_N3\"",
		strings.Join(wrong, "; "))
}

func findNodeBinding(top *recorder.Topology, id string) (recorder.NodeBinding, bool) {
	if top == nil {
		return recorder.NodeBinding{}, false
	}
	for _, n := range top.Nodes {
		if n.ID == id {
			return n, true
		}
	}
	return recorder.NodeBinding{}, false
}

// runWorld executes one world through the eight-phase lifecycle.
//
// The results are NAMED so the unconditional cleanup can still fold a late
// failure into the outcome. That matters for exactly one case: HEAL's residual
// check. A world that returned early (a dead driver, a cancelled context)
// never reached the HEAL step, and if faults were injected before that happened
// the residue has to be swept and reported, not discovered by the next world.
func (r *Runner) runWorld(ctx context.Context, spec worldSpec) (wres worldResult, wErr error) {
	ordinal := spec.ordinal - 1 // legacy 0-based index used in messages below
	worldSeed := spec.seed
	paths := spec.paths

	if err := os.MkdirAll(paths.Dir, 0o755); err != nil {
		return worldResult{outcome: OutcomeHarnessError, paths: paths}, err
	}
	if err := os.MkdirAll(paths.Logs, 0o755); err != nil {
		return worldResult{outcome: OutcomeHarnessError, paths: paths}, err
	}

	timeline := &recorder.Timeline{}
	var markerSink MarkerSink
	phasesFile, err := os.OpenFile(paths.PhasesLog, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err == nil {
		sink := &fileMarkerSink{f: phasesFile}
		markerSink = sink
		defer func() { _ = sink.Close() }()
	}

	phaseLog, err := NewPhaseLog(r.clock, timeline, markerSink)
	if err != nil {
		return worldResult{outcome: OutcomeHarnessError, paths: paths, timeline: timeline}, err
	}

	var (
		topology        *recorder.Topology
		telemetryHandle TelemetryHandle
		driverHandle    DriverHandle
		collectedLogs   []CollectedLog
		probes          []oracle.ProbeObservation
		findings        []OracleResult
		worldOutcome    = OutcomePass
		perturb         *perturbation
		healed          bool
		drain           DrainReport
		sutImages       []schema.SUTImage
	)

	// Invariant: HEAL and TEARDOWN run unconditionally, with a detached context.
	//
	// HEAL is here as well as in its own lifecycle step because every early
	// return above it (a dead driver, a cancelled context, a failed injection)
	// would otherwise skip it. A leaked iptables rule or a SIGSTOPped container
	// poisons every subsequent world on this host, and the leak is triggered by
	// the failure paths, which is precisely when nobody is watching (OQ-004).
	defer func() {
		detachedCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		_ = phaseLog.Unnest()

		if perturb != nil && !healed {
			if _, err := healPerturbation(detachedCtx, perturb); err != nil {
				fmt.Fprintf(r.stderr, "thesis: world %d: HEAL: %v\n", ordinal+1, err)
				if wres.outcome.Rank() < OutcomeHarnessError.Rank() {
					wres.outcome = OutcomeHarnessError
				}
			}
		}
		wres.planned = plannedOf(perturb)
		if rf := realizedOf(perturb); rf != nil {
			wres.realized = rf
		}
		wres.drain = drain
		wres.seed = worldSeed
		if topology != nil {
			wres.project = topology.ComposeProject
			wres.hostPorts = map[string]int64{}
			for _, n := range topology.Nodes {
				wres.hostPorts[n.ID] = n.HostPort
			}
		} else {
			wres.project = spec.project
		}

		_ = phaseLog.Advance(schema.PhaseTeardown)

		// The driver is stopped BEFORE the topology goes away, on every path.
		//
		// An early return (a failed steady-state probe, a cancelled context, a
		// perturbation that would not compile) used to leave the workload
		// running against containers that were about to be destroyed. Serially
		// that produced connection errors in a history nobody was going to read;
		// under the parallel executor it is worse, because the slot's published
		// ports are reused by the next world and an orphaned loadgen would be
		// hammering them.
		if driverHandle != nil {
			_ = driverHandle.Stop(detachedCtx)
		}

		if telemetryHandle != nil {
			_ = telemetryHandle.Stop(detachedCtx)
		}

		if topology != nil && len(collectedLogs) == 0 {
			logs, _ := r.logCollector.Collect(detachedCtx, LogRequest{
				Topology: topology,
				Dir:      paths.Logs,
				Timeline: timeline,
			})
			collectedLogs = logs
		}

		// Down VERIFIES its own work and returns what it could not remove
		// (D-017). Recording that verdict is the whole point: a discarded error
		// here is a compose project nobody knows survived, and on this host a
		// surviving project holds two of about twenty-four bridge networks for
		// good. The parallel executor RETIRES a worker slot whose teardown could
		// not be verified rather than reusing it, so the pool drains visibly
		// instead of the next `up` failing with a message about address pools.
		//
		// It runs even when BOOT produced no topology. A nil topology USUALLY
		// means nothing was created, or that Up rolled itself back, but Up's
		// rollback is a Down whose error it discards, so "usually" is not good
		// enough here: a project that survived a failed boot would be invisible.
		// The sweep addresses the project by NAME and verifies by LABEL, so it
		// is correct either way and returns nil when there is nothing to remove.
		downTop := topology
		if downTop == nil {
			downTop = r.teardownStandIn(spec)
		}

		// --keep-up: leave the topology standing for inspection.
		//
		// It is only honoured when this world actually booted one, and only when
		// the handoff PERSISTS. `up` and `down` are separate OS processes, so a
		// kept-up topology whose topology.json was never written is unreachable by
		// any later command and permanently holds two of this host's ~24 bridge
		// networks. D-017 answers that case by tearing back down rather than
		// leaking, and this path answers it the same way.
		if spec.keepUp && topology != nil {
			if err := r.persistHandoff(topology); err != nil {
				fmt.Fprintf(r.stderr, "thesis: world %d: --keep-up: %v\n", ordinal+1, err)
				fmt.Fprintf(r.stderr, "thesis: world %d: --keep-up: tearing the topology down rather than "+
					"leaking a project no later command could address\n", ordinal+1)
				if wres.outcome.Rank() < OutcomeHarnessError.Rank() {
					wres.outcome = OutcomeHarnessError
				}
			} else {
				wres.keptUp = true
				fmt.Fprintf(r.stderr, "thesis: world %d: --keep-up: topology left running as compose project %s; "+
					"run `thesis down` to remove it\n", ordinal+1, topology.ComposeProject)
				return
			}
		}

		if downTop != nil {
			if err := r.backend.Down(detachedCtx, downTop, harness.DownOptions{
				Volumes: true,
				Env:     spec.env,
			}); err != nil {
				wres.teardownErr = err
				fmt.Fprintf(r.stderr, "thesis: world %d: TEARDOWN: %v\n", ordinal+1, err)
				if wres.outcome.Rank() < OutcomeHarnessError.Rank() {
					wres.outcome = OutcomeHarnessError
				}
			}
		}
	}()

	// 1. BOOT
	if err := phaseLog.Advance(schema.PhaseBoot); err != nil {
		return worldResult{outcome: OutcomeHarnessError, paths: paths, timeline: timeline}, err
	}
	// The overlay is written into THIS WORLD's directory rather than the run's.
	//
	// It has to be per-world: `down` must be handed the identical file set `up`
	// used or compose resolves a different project and tears down nothing while
	// reporting success. Two concurrent worlds writing one overlay path is a
	// torn file at best and a cross-wired teardown at worst.
	overlay := spec.overlay
	if overlay == "" {
		overlay = filepath.Join(paths.Dir, harness.OverlayName)
	}
	topo, err := r.backend.Up(ctx, harness.UpRequest{
		Config:      r.cfg,
		ProjectDir:  r.opts.ProjectDir,
		RunID:       r.runID,
		RunDir:      r.runDir,
		ProjectName: spec.project,
		OverlayPath: overlay,
		Env:         spec.env,
	})
	if err != nil {
		return worldResult{outcome: OutcomeHarnessError, paths: paths, timeline: timeline}, fmt.Errorf("boot up failed: %w", err)
	}
	topology = topo

	// Resolve the SUT's image digests NOW, while the bound containers exist
	// (OQ-055). Fail-soft by contract: World.Validate accepts [] as "recorded
	// that none were resolved", so a resolution error costs the world its
	// provenance, never its execution. Through the seam, and bounded in time
	// inside it (D-073): a daemon that will not answer costs this world a few
	// seconds and its provenance, not its budget (OQ-067).
	sutImages, imgErr := r.imageResolver.Resolve(ctx, topology.Nodes, r.cfg.Harness.Nodes)
	if imgErr != nil {
		fmt.Fprintf(r.stderr, "thesis: world %d: image digest resolution failed: %v (sut.images will be [])\n", ordinal+1, imgErr)
		sutImages = nil
	}

	// Before anything drives: the ports this world's driver will be TOLD about
	// must be the ports the harness actually published. See portsAgree.
	if err := portsAgree(spec.expectPorts, topology.Nodes); err != nil {
		return worldResult{outcome: OutcomeHarnessError, paths: paths, timeline: timeline}, err
	}

	// Assemble the fault mechanisms and capture their BOOT baselines, while the
	// topology is up and before anything has been perturbed. The ordering is
	// load-bearing: every family judges residue against a snapshot taken before
	// any fault existed, so capturing later would bake a fault into the
	// definition of clean and HEAL would verify nothing (D-026).
	//
	// Only when the world actually has faults. A no-fault world does not need
	// the baselines, and running four capture sidecars per world would add
	// seconds to every smoke run for nothing.
	//
	// REBUILT PER WORLD, never cached across them. Every world tears its
	// topology down and boots a fresh one, so every container id changes. An
	// injector built for world 1 addresses containers that no longer exist by
	// world 2, and the symptom is a mid-window `docker inspect: no such object`:
	// a fault that fails to inject after the schedule has already committed to
	// its window. The baselines are per-world for the same reason: "clean" is
	// defined against the containers this world actually booted.
	//
	// The set lives on this world's STACK and is passed down explicitly. It used
	// to be assigned to a Runner field, which two concurrent worlds would race
	// on and which silently carried world N's container ids into world N+1's
	// perturbation if the rebuild was ever skipped.
	worldInjectors := r.injectors // an explicitly supplied set (tests, fakes) wins
	if len(worldInjectors) == 0 && len(spec.faults) > 0 {
		set, err := BuildInjectors(ctx, r.cfg, topology, r.runID, r.opts.ProjectDir, worldSeed)
		if err != nil {
			// A partial set is still usable: the families that got their
			// baseline work, and the ones that did not refuse loudly at
			// injection time rather than running unverifiably.
			if set == nil {
				return worldResult{outcome: OutcomeHarnessError, paths: paths, timeline: timeline},
					fmt.Errorf("perturb: %w", err)
			}
			fmt.Fprintf(r.stderr, "thesis: world %d: %v\n", ordinal+1, err)
		}
		worldInjectors = set.Injectors
	}

	// 2. SEED
	if err := phaseLog.Advance(schema.PhaseSeed); err != nil {
		return worldResult{outcome: OutcomeHarnessError, paths: paths, timeline: timeline}, err
	}
	telHandle, err := r.telemetry.Start(ctx, TelemetryRequest{
		Config:     r.cfg,
		Topology:   topology,
		OutputPath: paths.Telemetry,
		Clock:      r.clock,
		Timeline:   timeline,
		Interval:   500 * time.Millisecond,
	})
	if err != nil {
		return worldResult{outcome: OutcomeHarnessError, paths: paths, timeline: timeline}, fmt.Errorf("telemetry start failed: %w", err)
	}
	telemetryHandle = telHandle

	probeCmd := r.cfg.Harness.SteadyState.Probe
	probeTimeout := r.cfg.Harness.SteadyState.Timeout.Std()
	if probeTimeout <= 0 {
		probeTimeout = 60 * time.Second
	}
	if probeCmd != "" {
		err := r.steadyState.Probe(ctx, SteadyStateRequest{
			Config:     r.cfg,
			ProjectDir: r.opts.ProjectDir,
			Topology:   topology,
			Probe:      probeCmd,
			Timeout:    probeTimeout,
		})
		if err != nil {
			return worldResult{outcome: OutcomeHarnessError, paths: paths, timeline: timeline}, fmt.Errorf("steady state probe failed: %w", err)
		}
	}

	// 3. DRIVE
	if err := phaseLog.Advance(schema.PhaseDrive); err != nil {
		return worldResult{outcome: OutcomeHarnessError, paths: paths, timeline: timeline}, err
	}

	planPath := filepath.Join(paths.Dir, driver.PlanFileName)
	// The DRIVER profile, not the RUN profile. `profiles.<name>.driver_profile`
	// names an entry of `driver.profiles`, and the two namespaces are different:
	// the directive's own sample happens to use smoke/gate/soak for both,
	// which is what hid this for three phases.
	//
	// Measured, on the first run profile whose name was NOT also a driver
	// profile name: NewPlan returned "driver.profiles has no entry", the error
	// was DISCARDED here, no plan file was written, the workload was launched
	// with a profile it could not resolve, it exited 5, and NO HISTORY WAS
	// WRITTEN. linearizable.kv and no_stuck_op then reported INCONCLUSIVE
	// ("cannot open the history") while no_crash, resource_return_to_baseline
	// and availability_after_heal returned verdicts about a system nothing had
	// driven. That is a world reporting on a system it never exercised, which is
	// the one thing this codebase refuses to do, so the error is now FATAL.
	plan, err := driver.NewPlan(r.cfg, r.driverProfile, paths.History, worldSeed)
	if err != nil {
		return worldResult{outcome: OutcomeHarnessError, paths: paths, timeline: timeline},
			fmt.Errorf("driver plan: %w", err)
	}
	if spec.plan != nil {
		plan = spec.plan.Clone()
		plan.HistoryPath = paths.History
		plan.Seed = worldSeed
	}
	if err := driver.WritePlan(planPath, plan); err != nil {
		return worldResult{outcome: OutcomeHarnessError, paths: paths, timeline: timeline},
			fmt.Errorf("write driver plan: %w", err)
	}

	drvHandle, err := r.driver.Start(ctx, DriverRequest{
		Config:       r.cfg,
		ProjectDir:   r.opts.ProjectDir,
		RunID:        r.runID,
		WorldOrdinal: ordinal + 1,
		WorldDir:     paths.Dir,
		HistoryPath:  paths.History,
		PlanPath:     planPath,
		Seed:         worldSeed,
		Profile:      r.driverProfile,
		Topology:     topology,
		Env:          spec.env,
		// Ask for the control channel whenever draining is enabled. A driver
		// that does not read stdin is unaffected: it simply does not drain, and
		// the deadline reports that as a fact rather than hiding it.
		StdinControl: r.drainDeadline > 0,
	})
	if err != nil {
		return worldResult{outcome: OutcomeHarnessError, paths: paths, timeline: timeline}, fmt.Errorf("driver start failed: %w", err)
	}
	driverHandle = drvHandle

	// 4. PERTURB: overlaps DRIVE (directive 4.1 step 4).
	if err := phaseLog.Nest(schema.PhasePerturb); err != nil {
		return worldResult{outcome: OutcomeHarnessError, paths: paths, timeline: timeline}, err
	}

	// The schedule is compiled HERE rather than at BOOT because the virtual
	// clock's origin is DRIVE start: before it, `@8200..11000` names an instant
	// that does not exist. Compilation refuses everything it can refuse (an
	// unknown kind, a zero-match target, a budget overrun, a violated safety
	// constraint, a kind with no mechanism) so those never surface half way
	// through a perturbed world.
	p, err := r.newPerturbation(topology, timeline, worldSeed, spec.faults, worldInjectors)
	if err != nil {
		return worldResult{outcome: OutcomeHarnessError, paths: paths, timeline: timeline},
			fmt.Errorf("perturb: %w", err)
	}
	perturb = p

	if p.schedule.Empty() {
		// No faults: PERTURB simply overlaps whatever DRIVE is still doing.
		if err := waitOutDriveWindow(ctx, driverHandle.Done(), DefaultDriveWindow); err != nil {
			return worldResult{outcome: OutcomeCanceled, paths: paths, timeline: timeline}, err
		}
	} else {
		if !r.quiet {
			fmt.Fprintf(r.stderr, "thesis: world %d: perturbing with %d fault(s), peak concurrency %d\n",
				ordinal+1, p.schedule.Len(), p.schedule.PeakConcurrent)
		}
		// Run blocks until the last withdrawal. It withdraws everything it
		// injected on cancellation and on failure before returning, and HEAL
		// sweeps again below regardless.
		pres, perr := p.exec.Run(ctx)
		if perr != nil {
			outcome := OutcomeHarnessError
			if pres.Canceled && ctx.Err() != nil {
				outcome = OutcomeCanceled
			}
			return worldResult{outcome: outcome, paths: paths, timeline: timeline},
				fmt.Errorf("perturb: %w", perr)
		}
	}

	// 5. HEAL: withdraw everything still active, then VERIFY zero residual.
	_ = phaseLog.Unnest()
	if err := phaseLog.Advance(schema.PhaseHeal); err != nil {
		return worldResult{outcome: OutcomeHarnessError, paths: paths, timeline: timeline}, err
	}
	healReport, healErr := healPerturbation(ctx, perturb)
	healed = true
	if healErr != nil {
		// Directive 4.3's Critical Guarantee. A residue means the run could not
		// be CONDUCTED properly, so it has no verdict to give about the system
		// under test: harness error (exit 2), never an oracle violation.
		fmt.Fprintf(r.stderr, "thesis: world %d: HEAL: %v\n", ordinal+1, healErr)
		worldOutcome = worldOutcome.Worse(OutcomeHarnessError)
	}
	if !r.quiet && len(healReport.Withdrawn) > 0 {
		fmt.Fprintf(r.stderr, "thesis: world %d: HEAL withdrew %v\n", ordinal+1, healReport.Withdrawn)
	}
	time.Sleep(100 * time.Millisecond)

	// 6. QUIESCE: DRAIN, then stop.
	//
	// The drain is the FIRST ACT of this phase, not a ninth phase name: directive
	// 4.1 freezes eight, invariant I5 is built on them, and "stop the workload
	// and wait for convergence" is exactly what a drain does. Nothing new appears
	// in schema.Phase, in phases.jsonl or in oracle_input.phases.
	//
	// It goes AFTER HEAL deliberately. Draining under an active partition would
	// measure the fault, not the system: in-flight operations cannot retire while
	// the node they are addressed to is unreachable, so every world would report
	// its driver as refusing to drain. After HEAL the faults are withdrawn and
	// verified withdrawn, so an operation that still will not complete is
	// genuinely stuck, which is the finding no_stuck_op exists to make and could
	// not previously reach. See drain.go and OQ-033.
	if err := phaseLog.Advance(schema.PhaseQuiesce); err != nil {
		return worldResult{outcome: OutcomeHarnessError, paths: paths, timeline: timeline}, err
	}

	drain = r.drainDriver(ctx, ordinal, driverHandle)
	// Stop regardless, on every drain outcome. A drained driver has already
	// exited and Stop is then a no-op; one that refused has to be killed exactly
	// as it was before this existed.
	_ = driverHandle.Stop(ctx)

	// Take one health probe per node that harness.health covers, at the URL
	// its own entry declares (convergenceProbes, OQ-063): once at the start of
	// QUIESCE and once after the convergence sleep, each attempt stamped when
	// it is taken. availability_after_heal also receives every telemetry probe
	// sample from the window (readProbes), so the verdict rests on the whole
	// window rather than on its first instant.
	nowMS := func() int64 {
		v, err := timeline.V(r.clock.NowR())
		if err != nil {
			return 0
		}
		return int64(v / recorder.VTime(time.Millisecond))
	}
	probeClient := &http.Client{Timeout: 3 * time.Second}
	for round := 0; round < 2; round++ {
		if round == 1 {
			time.Sleep(2000 * time.Millisecond)
		}
		converged, cErr := convergenceProbes(r.cfg, topology, nowMS, probeClient)
		if cErr != nil {
			return worldResult{outcome: OutcomeHarnessError, paths: paths, timeline: timeline},
				fmt.Errorf("quiesce: convergence probe: %w", cErr)
		}
		probes = append(probes, converged...)
	}

	// 7. ASSERT
	if err := phaseLog.Advance(schema.PhaseAssert); err != nil {
		return worldResult{outcome: OutcomeHarnessError, paths: paths, timeline: timeline}, err
	}

	timings, _ := phaseLog.Timings()
	// Write phases.json
	phaseBytes, _ := json.MarshalIndent(timings, "", "  ")
	_ = os.WriteFile(paths.Phases, append(phaseBytes, '\n'), 0o644)

	// Materialise every artifact prothesis.oracle_input/v1 PROMISES, before any
	// oracle is handed that document.
	//
	// The contract is a promise to a third party: an external oracle in Phase 3
	// opens the paths it names. A document naming a file that does not exist is
	// worse than one that omits it, because the oracle fails at the open() call
	// rather than degrading. Verified missing before this was added:
	// final_state_path was dangling and telemetry_path named the JSONL stream
	// with no complete document beside it.
	if err := r.materializeOracleArtifacts(paths, topology, timings, timeline); err != nil {
		// Not fatal to the world: oracles that do not need these still run, and
		// the ones that do report INCONCLUSIVE rather than PASS. But it must not
		// pass unnoticed, so it goes to stderr rather than being swallowed.
		fmt.Fprintf(r.stderr, "thesis: world %d: %v\n", ordinal+1, err)
	}

	// Merge the harness's phase markers into the canonical history.
	//
	// Directive 4.4's own sample carries a phase marker inline in the history
	// stream, and brief D-D makes the recorder the merger. The driver writes
	// only op records and the harness only phase markers, precisely so neither
	// tears the other's lines; the merge is by raw line ordered on t_ns and
	// NEVER decode-then-re-encode, because the fixture attaches a namespaced
	// `meta` object per op that schema.HistoryEntry does not model and would
	// silently strip: taking the stale-read attribution Phase 3 needs with it.
	if err := MergePhaseMarkers(paths.History, paths.PhasesLog); err != nil {
		fmt.Fprintf(r.stderr, "thesis: world %d: could not merge phase markers into the history: %v\n", ordinal+1, err)
	}

	// Collect node logs now so oracles have them
	logs, _ := r.logCollector.Collect(ctx, LogRequest{
		Topology: topology,
		Dir:      paths.Logs,
		Timeline: timeline,
	})
	collectedLogs = logs

	// Read history
	hist := oracle.ReadHistoryFile(paths.History)

	// Evaluate oracles
	oeFindings, err := r.oracleEngine.Evaluate(ctx, EvalRequest{
		Config:       r.cfg,
		ProjectDir:   r.opts.ProjectDir,
		RunID:        r.runID,
		WorldOrdinal: ordinal + 1,
		Topology:     topology,
		Paths:        paths,
		Input:        paths.OracleInputDoc(timings),
		Phases:       timings,
		Clock:        r.clock,
		Timeline:     timeline,
		Driver:       driverHandle.Result(),
		Logs:         collectedLogs,
		Probes:       probes,
		// What the perturber ACTUALLY injected, against which concrete nodes, in
		// which measured window. Without it no_crash's "outside a planned fault
		// window" clause has nothing to excuse: see EvalRequest.Realized.
		Realized: realizedOf(perturb),
	})
	if err != nil {
		worldOutcome = OutcomeInconclusive
	} else {
		findings = oeFindings
		for _, f := range findings {
			if f.Outcome().Rank() > worldOutcome.Rank() {
				worldOutcome = f.Outcome()
			}
		}
	}

	// Write world.thesis
	world := schema.NewWorld(worldSeed, "default", r.driverProfile)
	world.PhaseTimings = timings
	world.FaultSchedule.Planned = plannedOf(perturb)
	// nil means resolution never ran or failed: keep NewWorld's explicit []
	// ("recorded that none were resolved"), never a null.
	if sutImages != nil {
		world.SUT.Images = sutImages
	}
	// Realized is a non-nil empty list for a world that executed and injected
	// nothing. That is a DIFFERENT statement from null, which means the world was
	// never executed at all.
	if rf := realizedOf(perturb); rf != nil {
		world.FaultSchedule.Realized = rf
	}
	world = world.Normalized()

	worldBytes, wErr := recorder.EncodeWorld(&world)
	if wErr == nil {
		_ = os.WriteFile(paths.World, worldBytes, 0o644)
	}

	// Write oracle_input.json
	oiDoc := paths.OracleInputDoc(timings)
	oiBytes, _ := json.MarshalIndent(oiDoc, "", "  ")
	_ = os.WriteFile(paths.OracleInput, append(oiBytes, '\n'), 0o644)

	// Write result.json
	// Record EVERY oracle's finding, not only the violations.
	//
	// An INCONCLUSIVE run with zero violations is otherwise unactionable: the
	// exit-code contract says code 2 means "retry once, then escalate to a
	// human", and a human cannot act on a verdict that does not say WHICH oracle
	// could not evaluate or why. An oracle that returns ok is recorded too,
	// because "checked and satisfied" and "never ran" are different facts and
	// only one of them supports a PASS.
	oracleDocs := make([]map[string]any, 0, len(findings))
	for _, f := range findings {
		d := map[string]any{
			"oracle":         f.Output.Oracle,
			"class":          string(f.Output.Class),
			"status":         string(f.Output.Status),
			"valid_phases":   f.Output.ValidPhases,
			"observed_phase": string(f.ObservedPhase),
			"first_seen_ms":  f.FirstSeenMS,
			"explanation":    f.Output.Explanation,
		}
		if f.Err != nil {
			d["error"] = f.Err.Error()
		}
		oracleDocs = append(oracleDocs, d)
	}

	// The DRAIN is recorded here, next to the oracle findings, because it is what
	// licenses reading them.
	//
	// `no_stuck_op` returning ok over a drained history is a statement about
	// operations that were given a chance to retire and did. The same ok over a
	// history whose driver was killed mid-flight would be a statement about
	// operations nobody waited for. A reader of result.json must be able to tell
	// those apart without re-deriving it from timings, and an agent reading a
	// PASS is entitled to see that the drain happened (OQ-033).
	driverRes := driverHandle.Result()
	resDoc := map[string]any{
		"world":   ordinal + 1,
		"seed":    worldSeed,
		"outcome": string(worldOutcome),
		"oracles": oracleDocs,
		"driver": map[string]any{
			"started":   driverRes.Started,
			"exit_code": driverRes.ExitCode,
			"stopped":   driverRes.Stopped,
			"drain":     drain.Doc(),
		},
	}
	if topology != nil && topology.ComposeProject != "" {
		resDoc["compose_project"] = topology.ComposeProject
	}
	if spec.slot >= 0 {
		resDoc["worker_slot"] = spec.slot
	}
	resBytes, _ := json.MarshalIndent(resDoc, "", "  ")
	_ = os.WriteFile(paths.Result, append(resBytes, '\n'), 0o644)

	return worldResult{
		ordinal:  ordinal + 1,
		seed:     worldSeed,
		outcome:  worldOutcome,
		phases:   timings,
		history:  hist,
		logs:     collectedLogs,
		findings: findings,
		paths:    paths,
		timeline: timeline,
		planned:  plannedOf(perturb),
		realized: realizedOf(perturb),
		drain:    drain,
	}, nil
}

// drainDriver runs QUIESCE's drain and reports it, once, in one place.
//
// The report goes to stderr on EVERY outcome that is not a clean drain, quiet
// mode included. A driver that will not drain changes what a verdict means
// (operations left in flight are unjudgeable, so the world's best achievable
// answer drops from PASS to INCONCLUSIVE) and that is not a progress message
// to be suppressed.
func (r *Runner) drainDriver(ctx context.Context, ordinal int, h DriverHandle) DrainReport {
	if h == nil {
		return DrainReport{Outcome: DrainNotAttempted}
	}
	if r.drainDeadline <= 0 {
		return DrainReport{Outcome: DrainNotAttempted}
	}
	rep := h.Drain(ctx, r.drainDeadline)
	switch {
	case rep.Drained():
		if !r.quiet {
			fmt.Fprintf(r.stderr, "thesis: world %d: QUIESCE: %s\n", ordinal+1, rep.Describe())
		}
	default:
		fmt.Fprintf(r.stderr, "thesis: world %d: QUIESCE: %s\n", ordinal+1, rep.Describe())
	}
	return rep
}

// ---------------------------------------------------------------------------
// default subsystem adapters
// ---------------------------------------------------------------------------

type defaultDriverSupervisor struct{}

func (d *defaultDriverSupervisor) Start(ctx context.Context, req DriverRequest) (DriverHandle, error) {
	sup := driver.NewSupervisor()
	h := &defaultDriverHandle{
		sup:  sup,
		done: make(chan struct{}),
	}

	sub := driver.Substitution{
		HistoryPath: req.HistoryPath,
		Seed:        req.Seed,
		Profile:     req.Profile,
		PlanPath:    req.PlanPath,
	}

	drvReq := driver.Request{
		Cmd:          req.Config.Driver.Cmd,
		Dir:          req.ProjectDir,
		Sub:          sub,
		Env:          driverEnv(req.Topology, req.Env),
		StdinControl: req.StdinControl,
	}

	go func() {
		defer close(h.done)
		res, err := sup.Run(ctx, drvReq)
		h.mu.Lock()
		defer h.mu.Unlock()
		if res != nil {
			h.res = DriverResult{
				Started:  res.Status != driver.StatusStartError,
				ExitCode: res.ExitCode,
				Stopped:  res.Killed,
				Err:      err,
			}
		} else {
			h.res = DriverResult{Err: err}
		}
	}()

	return h, nil
}

// driverEnv returns the driver's environment: the harness's own, with the
// harness-owned names stripped, this world's real targets derived from the
// topology, and extra appended so a later entry wins.
//
// The strip is the load-bearing half. TargetsEnv is in the PROTHESIS_ namespace,
// which means the HARNESS is its authority: if this world did not set it, no
// value should be visible. An inherited one (left over from a shell, or from an
// outer `thesis` invocation) would silently point a serial world's driver at
// some other cluster, and a world that drove the wrong system still writes a
// history that looks clean.
//
// What used to follow from that was "harness silence means use your own
// default". That was safe only while the default was right, which is true for a
// driver and a harness on the same host and false the moment the harness runs
// in a container: the driver's built-in loopback is then its OWN loopback.
// Measured, runs r_2026_09_24_a253 and r_2026_09_24_9c90, five worlds each:
// every health probe passed, every fault injected and withdrew, and all 60,000
// operations failed in every world, because the harness said nothing and the
// fixture driver dialled 127.0.0.1. The checker refused rather than passing, so
// nothing false was reported, but neither run measured anything.
//
// So the harness now says what it knows. It published the ports and it knows
// the address they are reachable on, and a value it can derive is not a value
// it should withhold. An explicit entry in extra still wins, which is how the
// parallel executor keeps authority over its own lane's band (D-087).
//
// driver.buildEnv applies the same rule to PROTHESIS_STDIN_CONTROL and
// PROTHESIS_PLAN_PATH, which it owns.
func driverEnv(top *recorder.Topology, extra []string) []string {
	base := os.Environ()
	out := make([]string, 0, len(base)+len(extra)+1)
	prefix := TargetsEnv + "="
	for _, kv := range base {
		if strings.HasPrefix(kv, prefix) {
			continue
		}
		out = append(out, kv)
	}
	if derived := topologyTargets(top); derived != "" {
		out = append(out, prefix+derived)
	}
	return append(out, extra...)
}

// topologyTargets renders this world's published nodes as `<probe host>:<port>`
// in topology order, the same spelling WorkerSlot.Targets uses.
//
// A node the harness published no port for contributes nothing: not every
// declared node must publish one, and a malformed entry would be worse than an
// absent one. No topology, or no published ports in it, yields no claim.
func topologyTargets(top *recorder.Topology) string {
	if top == nil {
		return ""
	}
	host := probehost.Host()
	out := make([]string, 0, len(top.Nodes))
	for _, n := range top.Nodes {
		if n.HostPort > 0 {
			out = append(out, host+":"+strconv.FormatInt(n.HostPort, 10))
		}
	}
	return strings.Join(out, ",")
}

type defaultDriverHandle struct {
	sup      *driver.Supervisor
	done     chan struct{}
	res      DriverResult
	drain    DrainReport
	mu       sync.Mutex
	stopOnce sync.Once
}

func (h *defaultDriverHandle) Done() <-chan struct{} { return h.done }

// Drain implements DriverHandle: ask, then wait, then report honestly.
//
// The wait is what the deadline is FOR. Asking is cheap and tells us nothing
// (a driver that ignores stdin looks exactly like one that is finishing up) so
// the only evidence that a drain worked is the process exiting on its own.
func (h *defaultDriverHandle) Drain(ctx context.Context, deadline time.Duration) DrainReport {
	rep := DrainReport{Deadline: deadline}
	if deadline <= 0 {
		rep.Outcome = DrainNotAttempted
		h.noteDrain(rep)
		return rep
	}

	start := time.Now()
	// Already gone. The workload retired its own budget, so nothing is
	// outstanding and there is nothing to drain, which is the outcome a drain
	// asks for, so it is reported as one rather than as "not attempted".
	select {
	case <-h.done:
		rep.Outcome = DrainCompleted
		rep.Waited = time.Since(start)
		h.noteDrain(rep)
		return rep
	default:
	}

	if err := h.sup.Drain(); err != nil {
		rep.Waited = time.Since(start)
		rep.Err = err
		if errors.Is(err, driver.ErrNoControlChannel) {
			rep.Outcome = DrainUnsupported
		} else {
			rep.Outcome = DrainFailed
		}
		h.noteDrain(rep)
		return rep
	}

	timer := time.NewTimer(deadline)
	defer timer.Stop()
	select {
	case <-h.done:
		rep.Outcome = DrainCompleted
	case <-timer.C:
		rep.Outcome = DrainDeadlineExceeded
		rep.Err = fmt.Errorf("control: the driver was asked to drain and had not exited after %s",
			deadline.Round(time.Millisecond))
	case <-ctx.Done():
		rep.Outcome = DrainCanceled
		rep.Err = ctx.Err()
	}
	rep.Waited = time.Since(start)
	h.noteDrain(rep)
	return rep
}

func (h *defaultDriverHandle) noteDrain(rep DrainReport) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.drain = rep
}

func (h *defaultDriverHandle) Stop(ctx context.Context) error {
	var err error
	h.stopOnce.Do(func() {
		err = h.sup.Kill(2 * time.Second)
		select {
		case <-h.done:
		case <-ctx.Done():
		}
	})
	return err
}

func (h *defaultDriverHandle) Result() DriverResult {
	h.mu.Lock()
	defer h.mu.Unlock()
	res := h.res
	res.Drain = h.drain
	return res
}

type defaultTelemetrySampler struct{}

func (t *defaultTelemetrySampler) Start(ctx context.Context, req TelemetryRequest) (TelemetryHandle, error) {
	sink, err := telemetry.NewFileSink(req.OutputPath)
	if err != nil {
		return nil, err
	}

	var targets []telemetry.Target
	if req.Config != nil && req.Topology != nil {
		tgts, err := telemetry.TargetsFromTopology(req.Config, req.Topology)
		if err != nil {
			_ = sink.Close()
			return nil, err
		}
		targets = tgts
	}

	collector, err := telemetry.New(telemetry.Options{
		Targets:  targets,
		Sink:     sink,
		Interval: req.Interval,
		Clock:    req.Clock,
		Timeline: req.Timeline,
	})
	if err != nil {
		_ = sink.Close()
		return nil, err
	}

	if err := collector.Start(ctx); err != nil {
		_ = sink.Close()
		return nil, err
	}

	return &defaultTelemetryHandle{
		collector: collector,
		sink:      sink,
		path:      req.OutputPath,
	}, nil
}

type defaultTelemetryHandle struct {
	collector *telemetry.Collector
	sink      *telemetry.WriterSink
	path      string
}

func (h *defaultTelemetryHandle) Stop(_ context.Context) error {
	h.collector.Stop()
	if h.sink != nil {
		_ = h.sink.Close()
	}
	return nil
}

func (h *defaultTelemetryHandle) Path() string { return h.path }

type defaultOracleEngine struct{}

// plannedFaultWindows converts the perturber's realized schedule into the
// windows no_crash is willing to excuse.
//
// A realized entry with NO resolved nodes yields a window with an empty node
// list, which oracle.FaultWindow documents as excusing nothing. That is
// deliberate and is the safe direction: a fault whose binding was not recorded
// cannot show that any particular node's exit was planned, and treating
// "unknown" as "planned" would let one unrecorded fault silence every crash in
// its time range.
func plannedFaultWindows(realized []schema.RealizedFault) []oracle.FaultWindow {
	out := make([]oracle.FaultWindow, 0, len(realized))
	for _, rf := range realized {
		kind, target := "", rf.Fault
		if spec, err := schema.ParseFault(rf.Fault); err == nil {
			kind = string(spec.Kind)
			target = spec.Target.String()
		}
		out = append(out, oracle.FaultWindow{
			Kind:    kind,
			Target:  target,
			Nodes:   append([]string(nil), rf.Nodes...),
			StartMS: rf.StartMS,
			EndMS:   rf.EndMS,
		})
	}
	return out
}

func (e *defaultOracleEngine) Evaluate(ctx context.Context, req EvalRequest) ([]OracleResult, error) {
	// Built-ins AND the external oracles discovered under oracles.dir register
	// into the SAME engine, so nothing downstream can tell them apart: an
	// external oracle's inconclusive reaches exit 2 by exactly the path a
	// built-in's does, and result.json records both the same way (D-034).
	//
	// A discovery failure is an ERROR, not a shrug. An oracle definition that
	// does not parse must never be quietly skipped: the run would then report
	// PASS over a property nobody checked.
	engine, disc, err := oracle.NewEngineForConfig(req.Config, oracle.DefaultOptions(),
		oracle.ExternalOptions{ProjectDir: req.ProjectDir})
	if err != nil {
		return nil, err
	}
	// "No external oracle ran" and "every external oracle passed" are different
	// facts, and only one of them supports a PASS. Say which one this was.
	if note := disc.Note(); note != "" {
		fmt.Fprintf(os.Stderr, "thesis: %s\n", note)
	}

	input, err := buildOracleInput(ctx, req)
	if err != nil {
		return nil, err
	}

	findings, err := engine.EvaluateAt(ctx, schema.PhaseAssert, input)
	if err != nil {
		return nil, err
	}
	return oracleResultsOf(findings), nil
}

// buildOracleInput assembles what every oracle is handed for one world.
//
// It is a named function rather than a block inside Evaluate so that the ONE
// thing an assembly step can get catastrophically wrong (handing an oracle an
// empty field where a fact belongs) is reachable from a test. PlannedFaults was
// hardcoded to an empty list here for three phases and no test could see it.
func buildOracleInput(ctx context.Context, req EvalRequest) (*oracle.Input, error) {
	originNS := int64(0)
	if req.Timeline != nil {
		originNS, _ = req.Timeline.DriveOriginWallNS()
	}

	hist := oracle.ReadHistoryFile(req.Paths.History)

	// Populate NodeObservations
	var nodes []oracle.NodeObservation
	for _, n := range req.Config.Harness.Nodes {
		containerID := ""
		if nb, ok := findNodeBinding(req.Topology, n.ID); ok {
			containerID = nb.ContainerID
		}

		obs := oracle.NodeObservation{
			NodeID:        n.ID,
			Service:       n.Service,
			ContainerID:   containerID,
			StateObserved: false,
		}

		// Query container state via docker inspect
		if containerID != "" {
			inspectState, err := queryDockerState(ctx, containerID)
			if err == nil && inspectState != nil {
				obs.StateObserved = true
				obs.Running = inspectState.Running
				obs.RestartCount = inspectState.RestartCount
				if !inspectState.Running || inspectState.ExitCode != 0 {
					var atMS *int64
					if t, tErr := time.Parse(time.RFC3339Nano, inspectState.FinishedAt); tErr == nil && !t.IsZero() {
						ms := (t.UnixNano() - originNS) / int64(time.Millisecond)
						atMS = &ms
					}
					obs.Exit = &oracle.ProcessExit{
						Code:      inspectState.ExitCode,
						OOMKilled: inspectState.OOMKilled,
						Error:     inspectState.Error,
						AtMS:      atMS,
					}
				}
			}
		}

		// Populate basic telemetry metrics from telemetry file if available
		obs.Metrics = readMetricsFromTelemetry(req.Paths.Telemetry, n.ID, originNS)
		nodes = append(nodes, obs)
	}

	// Populate LogStreams
	var logStreams []oracle.LogStream
	for _, l := range req.Logs {
		logStreams = append(logStreams, oracle.LogStream{
			NodeID: l.Node,
			Name:   l.Node,
			Path:   l.Path,
		})
	}

	// Probes from QUIESCE
	probes := readProbes(req)

	input := &oracle.Input{
		RunID:          req.RunID,
		WorldPath:      req.Paths.World,
		HistoryPath:    req.Paths.History,
		FinalStatePath: req.Paths.FinalState,
		TelemetryPath:  req.Paths.Telemetry,
		// Where an external oracle's stderr is captured, so a failing oracle is
		// one open() away from being debuggable.
		ArtifactDir:       req.Paths.Dir,
		Phases:            req.Phases,
		DriveOriginWallNS: originNS,
		Nodes:             nodes,
		Logs:              logStreams,
		Probes:            probes,
		HealthProbed:      healthProbedNodes(req.Config),
		History:           hist,
		PlannedFaults:     plannedFaultWindows(req.Realized),
	}
	return input, nil
}

// oracleResultsOf projects the engine's findings onto the control-plane record.
func oracleResultsOf(findings []oracle.Finding) []OracleResult {
	out := make([]OracleResult, 0, len(findings))
	for _, f := range findings {
		out = append(out, OracleResult{
			Output: f.Output(),
			// The phase the finding was OBSERVED in, which is NOT the phase the
			// oracle was evaluated in. The engine has already resolved it from
			// the evidence against the measured windows, and it is never empty.
			//
			// Reporting EvaluatedIn here made every violation say ASSERT, which
			// is the exact conflation OQ-004 warns about and would have put the
			// wrong phase on the fixture's stale read: found by an ASSERT-only
			// consistency oracle, observed in DRIVE (D-031), and directive 4.6's
			// own example carries "phase": "DRIVE" for precisely that shape.
			ObservedPhase: observedPhaseOf(f),
			FirstSeenMS:   f.Result.FirstSeenMS,
			Err:           f.Err,
		})
	}
	return out
}

// observedPhaseOf returns the phase a finding's evidence falls in, falling back
// to the phase it was evaluated in when the engine had nothing to resolve
// against. The fallback is honest rather than fabricated: "concluded in ASSERT"
// is true, whereas defaulting to DRIVE because FirstSeenMS is zero would invent
// a position on the timeline.
func observedPhaseOf(f oracle.Finding) schema.Phase {
	if !f.Result.Phase.IsUnscoped() {
		return f.Result.Phase
	}
	return f.EvaluatedIn
}

type dockerContainerState struct {
	Status       string `json:"Status"`
	Running      bool   `json:"Running"`
	ExitCode     int    `json:"ExitCode"`
	OOMKilled    bool   `json:"OOMKilled"`
	Error        string `json:"Error"`
	StartedAt    string `json:"StartedAt"`
	FinishedAt   string `json:"FinishedAt"`
	RestartCount int    `json:"RestartCount"`
}

func queryDockerState(ctx context.Context, container string) (*dockerContainerState, error) {
	cmd := exec.CommandContext(ctx, "docker", "inspect", "--format", "{{json .State}}", container)
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var s dockerContainerState
	if err := json.Unmarshal(out, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

func readMetricsFromTelemetry(path, nodeID string, originNS int64) []oracle.Series {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()

	var qdPoints, rssPoints, fdPoints, grPoints, thPoints []oracle.Sample
	dec := json.NewDecoder(f)
	for dec.More() {
		var s telemetry.Sample
		if err := dec.Decode(&s); err != nil {
			break
		}
		if s.Node != nodeID {
			continue
		}
		tms := int64(0)
		if s.VMS != nil {
			tms = *s.VMS
		} else if originNS > 0 {
			tms = (s.TNS - originNS) / int64(time.Millisecond)
		}
		if qd, ok := s.QueueDepth(); ok {
			qdPoints = append(qdPoints, oracle.Sample{TMS: tms, Value: qd})
		}
		if rss, ok := s.RSS(); ok {
			rssPoints = append(rssPoints, oracle.Sample{TMS: tms, Value: rss})
		}
		if s.Tasks != nil && s.Tasks.OpenFDs != nil {
			fdPoints = append(fdPoints, oracle.Sample{TMS: tms, Value: *s.Tasks.OpenFDs})
		}
		if gr, ok := s.Goroutines(); ok {
			grPoints = append(grPoints, oracle.Sample{TMS: tms, Value: gr})
		}
		if s.Tasks != nil && s.Tasks.Threads != nil {
			thPoints = append(thPoints, oracle.Sample{TMS: tms, Value: *s.Tasks.Threads})
		}
	}

	var series []oracle.Series
	if len(qdPoints) > 0 {
		series = append(series, oracle.Series{Metric: oracle.MetricQueueDepth, Points: qdPoints})
	}
	if len(rssPoints) > 0 {
		series = append(series, oracle.Series{Metric: oracle.MetricRSSBytes, Points: rssPoints})
	}
	if len(fdPoints) > 0 {
		series = append(series, oracle.Series{Metric: oracle.MetricFDCount, Points: fdPoints})
	}
	if len(grPoints) > 0 {
		series = append(series, oracle.Series{Metric: oracle.MetricGoroutines, Points: grPoints})
	}
	if len(thPoints) > 0 {
		series = append(series, oracle.Series{Metric: oracle.MetricThreads, Points: thPoints})
	}
	return series
}

// readProbes returns every probe observation the world produced: the direct
// convergence probes the runner took PLUS every probe sample the telemetry
// collector recorded at the node's declared health URL.
//
// Before this existed the telemetry samples were consulted only when NO direct
// probe was taken, so availability_after_heal judged each node on one attempt
// at the first instant of QUIESCE while the run's own telemetry.jsonl held four
// in-window answers it never saw (OQ-063). The oracle filters by window and
// wants every attempt; a node that answers any of them is available.
func readProbes(req EvalRequest) []oracle.ProbeObservation {
	probes := append([]oracle.ProbeObservation(nil), req.Probes...)
	f, err := os.Open(req.Paths.Telemetry)
	if err == nil {
		defer func() { _ = f.Close() }()
		dec := json.NewDecoder(f)
		for dec.More() {
			var s telemetry.Sample
			if err := dec.Decode(&s); err != nil {
				break
			}
			if s.Probe != nil && s.VMS != nil {
				latMS := int64(0)
				if s.Probe.LatencyUS != nil {
					latMS = *s.Probe.LatencyUS / 1000
				}
				probes = append(probes, oracle.ProbeObservation{
					NodeID:    s.Node,
					Target:    s.Probe.URL,
					TMS:       *s.VMS,
					OK:        s.Probe.OK,
					LatencyMS: latMS,
					Err:       s.Probe.Error,
				})
			}
		}
	}
	return probes
}
