package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/WrdCstlg/pro-thesis/internal/control"
	"github.com/WrdCstlg/pro-thesis/internal/corpus"
	"github.com/WrdCstlg/pro-thesis/internal/driver"
	"github.com/WrdCstlg/pro-thesis/internal/recorder"
	"github.com/WrdCstlg/pro-thesis/internal/replay"
	"github.com/WrdCstlg/pro-thesis/internal/shrink"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

const shrinkUsage = `usage: thesis shrink WORLD [flags]

Minimize a violating .thesis world down to its minimal reproducible schedule:
  Stage 0: Verify baseline reproduction of the target violation
  Stage 1: ddmin over the fault list (drop irrelevant faults)
  Stage 2: ddmin over the workload operation history (if supported by driver)
  Stage 3: Binary search timing narrowing on surviving fault windows
  Stage 4: Confirmation gate (k/k reproductions)

On confirmation, automatically commits the shrunk world to
.prothesis/regressions/w_<id>.thesis.

WORLD is a path, a world filename (w_a41f.thesis) or a short id (a41f); the
last two resolve inside .prothesis/regressions/.

Flags:
  --profile NAME       run profile to execute under. Default: the profile matching
                       the world's driver_profile.
  --expect ORACLE      oracle name to shrink toward. Default: inferred from the
                       world's verdict/result artifacts.
  --strictness LEVEL   identity matching rule: class | key | ops (default: key).
                       Left to the default, stage 0 may calibrate the level down
                       to "class" -- and says so -- if the UNREDUCED world proves
                       its own witness key moves between executions. Naming a
                       level here disables that: what you type is what is used.
  -k N, --confirm-k N  confirmation gate target (default: 3).
  --accept-trials N    reproductions required before a reduction is ACCEPTED
                       (default: 1). ddmin assumes a deterministic predicate; on
                       a nondeterministic target one reproduction is a sample,
                       not a proof, and an accepted subset is never revisited.
                       Raise it when the gate keeps failing on a target whose
                       violation is probabilistic. Costs one extra world per
                       accepted candidate per step. Must not exceed the reject
                       threshold (2).
  --budget DUR         wall-clock ceiling for the entire shrink (default: 20m).
  --world-budget N     maximum world executions ceiling (default: 60).
  --skip-ops           skip stage 2 (workload operation history ddmin).
  --skip-narrowing     skip stage 3 (timing window narrowing).
  --skip-baseline      skip stage 0 (pre-flight baseline verification).
  --trace              log candidates and evaluation decisions to stderr.
  --no-commit          do not commit confirmed minimal repro to .prothesis/regressions/.

Exit codes: 0 PASS (shrunk & confirmed) · 1 FAIL (did not reproduce / could not confirm) ·
            2 INCONCLUSIVE · 3 BUDGET_EXHAUSTED · 4 ORACLE_DRIFT · 5 CONFIG_ERROR
`

func cmdShrink(ctx context.Context, g globals, args []string) schema.ExitCode {
	var (
		worldArg      string
		profile       string
		expect        string
		strictnessStr string
		confirmK      int
		acceptTrials  int
		wallBudget    time.Duration
		worldBudget   int
		skipOps       bool
		skipNarrow    bool
		skipBaseline  bool
		trace         bool
		noCommit      bool
	)

	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-h", a == "--help":
			fmt.Print(shrinkUsage)
			return schema.ExitPass
		case a == "--trace":
			trace = true
		case a == "--skip-ops", a == "--no-ops":
			skipOps = true
		case a == "--skip-narrowing", a == "--no-narrow":
			skipNarrow = true
		case a == "--skip-baseline":
			skipBaseline = true
		case a == "--no-commit":
			noCommit = true
		case a == "-k" || strings.HasPrefix(a, "-k="), a == "--confirm-k" || strings.HasPrefix(a, "--confirm-k="):
			name := "-k"
			if strings.HasPrefix(a, "--confirm-k") {
				name = "--confirm-k"
			}
			v, ni, err := flagValue(args, i, name)
			if err != nil {
				errorf("shrink: %v", err)
				return schema.ExitConfigError
			}
			i = ni
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 {
				errorf("shrink: invalid %s %q (want a positive integer)", name, v)
				return schema.ExitConfigError
			}
			confirmK = n
		case a == "--accept-trials" || strings.HasPrefix(a, "--accept-trials="):
			v, ni, err := flagValue(args, i, "--accept-trials")
			if err != nil {
				errorf("shrink: %v", err)
				return schema.ExitConfigError
			}
			i = ni
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 {
				errorf("shrink: invalid --accept-trials %q (want a positive integer)", v)
				return schema.ExitConfigError
			}
			acceptTrials = n
		case a == "--world-budget" || strings.HasPrefix(a, "--world-budget="):
			v, ni, err := flagValue(args, i, "--world-budget")
			if err != nil {
				errorf("shrink: %v", err)
				return schema.ExitConfigError
			}
			i = ni
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 {
				errorf("shrink: invalid --world-budget %q (want a positive integer)", v)
				return schema.ExitConfigError
			}
			worldBudget = n
		case a == "--expect" || strings.HasPrefix(a, "--expect="):
			v, ni, err := flagValue(args, i, "--expect")
			if err != nil {
				errorf("shrink: %v", err)
				return schema.ExitConfigError
			}
			i = ni
			expect = v
		case a == "--strictness" || strings.HasPrefix(a, "--strictness="):
			v, ni, err := flagValue(args, i, "--strictness")
			if err != nil {
				errorf("shrink: %v", err)
				return schema.ExitConfigError
			}
			i = ni
			strictnessStr = v
		case a == "--profile" || strings.HasPrefix(a, "--profile="):
			v, ni, err := flagValue(args, i, "--profile")
			if err != nil {
				errorf("shrink: %v", err)
				return schema.ExitConfigError
			}
			i = ni
			profile = v
		case a == "--budget" || strings.HasPrefix(a, "--budget="):
			v, ni, err := flagValue(args, i, "--budget")
			if err != nil {
				errorf("shrink: %v", err)
				return schema.ExitConfigError
			}
			i = ni
			d, err := parseDurationFlag("budget", v)
			if err != nil {
				errorf("shrink: %v", err)
				return schema.ExitConfigError
			}
			wallBudget = d
		case strings.HasPrefix(a, "-"):
			errorf("shrink: unknown flag %q\n\n%s", a, shrinkUsage)
			return schema.ExitConfigError
		default:
			if worldArg != "" {
				errorf("shrink: takes exactly one WORLD (already have %q, then %q)", worldArg, a)
				return schema.ExitConfigError
			}
			worldArg = a
		}
	}

	if worldArg == "" {
		fmt.Fprint(os.Stderr, shrinkUsage)
		return schema.ExitConfigError
	}

	p, code := loadProject(g)
	if code != schema.ExitPass {
		return code
	}

	worldPath := worldPathFor(p.dir, worldArg)
	abs, err := filepath.Abs(worldPath)
	if err == nil {
		worldPath = abs
	}

	world, err := recorder.LoadWorld(worldPath)
	if err != nil {
		errorf("shrink: %v", err)
		return schema.ExitConfigError
	}

	if profile == "" {
		profile, err = runProfileFor(p.cfg, world.DriverProfile)
		if err != nil {
			errorf("shrink: %v", err)
			return schema.ExitConfigError
		}
	} else if _, ok := p.cfg.Profiles[profile]; !ok {
		errorf("shrink: unknown profile %q", profile)
		return schema.ExitConfigError
	}

	schedChoice, err := replay.ChooseSchedule(world)
	if err != nil {
		errorf("shrink: schedule unusable: %v", err)
		return schema.ExitConfigError
	}

	origID, err := resolveOriginalViolation(worldPath, expect)
	if err != nil {
		errorf("shrink: target violation: %v", err)
		return schema.ExitConfigError
	}

	// Leave the level UNSET when the caller did not type one, rather than
	// resolving the default here.
	//
	// MatchUnset is not a match level (identity.go); it is how the package tells
	// "nobody chose this" from "somebody chose exactly this". Stage 0 may
	// calibrate the former against measured witness-key drift and must never
	// touch the latter, so resolving MatchWitnessKey in the CLI would silently
	// make every run look like a deliberate instruction and disable calibration
	// for everyone.
	var strictness shrink.Strictness // MatchUnset; Policy.withDefaults resolves it
	if strictnessStr != "" {
		s, err := parseStrictness(strictnessStr)
		if err != nil {
			errorf("shrink: %v", err)
			return schema.ExitConfigError
		}
		strictness = s
	} else if origID.Key == "" && len(origID.OpIDs) == 0 {
		// A target with no witness at all has no key to match on and none to
		// drift, so naming the floor here is a resolution, not a preference.
		strictness = shrink.MatchOracleClass
	}

	policy := shrink.Policy{
		Strictness:    strictness,
		ConfirmK:      confirmK,
		AcceptTrials:  acceptTrials,
		SkipNarrowing: skipNarrow,
		SkipBaseline:  skipBaseline,
		SkipOps:       skipOps,
	}

	budget := shrink.Budget{
		Wall:   wallBudget,
		Worlds: worldBudget,
	}

	runID, err := allocRunID(p)
	if err != nil {
		errorf("shrink: allocate run: %v", err)
		return schema.ExitInconclusive
	}

	runner, err := runnerFor(p, profile, runID, g.quiet && !trace, 0)
	if err != nil {
		errorf("shrink: %v", err)
		return schema.ExitConfigError
	}

	var (
		extractedOps []int64
		basePlan     *driver.Plan
		opPlanAvail  bool
	)

	if !skipOps {
		historyPath := filepath.Join(filepath.Dir(worldPath), "history.jsonl")
		if _, statErr := os.Stat(historyPath); statErr == nil {
			ext, extErr := driver.ExtractOpsFromFile(historyPath, driver.ExtractOptions{AllowMissingPinned: true})
			if extErr == nil && len(ext.Ops) > 0 {
				bp, pErr := driver.NewPlan(p.cfg, world.DriverProfile, historyPath, world.Seed)
				if pErr == nil {
					fullPlan, planErr := ext.Plan(bp)
					if planErr == nil {
						basePlan = fullPlan
						opPlanAvail = true
						extractedOps = make([]int64, len(ext.Ops))
						for idx, op := range ext.Ops {
							extractedOps[idx] = op.OpID
						}
					}
				}
			}
		}
	}

	var traceLogger func(string)
	if trace {
		traceLogger = func(msg string) {
			fmt.Fprintf(os.Stderr, "thesis: shrink: %s\n", msg)
		}
	}

	exec := &shrinkRunnerExecutor{
		runner:   runner,
		seed:     world.Seed,
		fullPlan: basePlan,
	}

	opts := shrink.Options{
		Original:  origID,
		Faults:    schedChoice.Faults,
		Ops:       extractedOps,
		OpsBefore: len(extractedOps),
		OpPlan:    opPlanAvail,
		Executor:  exec,
		Budget:    budget,
		Policy:    policy,
		Log:       traceLogger,
	}

	rctx, cancel := ctxWithBudget(ctx, wallBudget)
	defer cancel()

	res, err := shrink.Run(rctx, opts)
	if err != nil {
		errorf("shrink: %v", err)
		return schema.ExitConfigError
	}

	var corpusPath string
	if res.Confirmation.Passed() && !noCommit {
		if res.WorldPath != "" {
			shrunkWorld, loadErr := recorder.LoadWorld(res.WorldPath)
			if loadErr == nil {
				c := corpus.Open(p.dir)
				conf := corpus.Confirmation{
					Attempts:     res.Confirmation.Trials,
					Reproduced:   res.Confirmation.Reproduced,
					Inconclusive: res.Confirmation.Unknown,
				}
				cp, addErr := c.Add(shrunkWorld, conf)
				if addErr == nil {
					corpusPath = cp
				} else if !g.quiet {
					fmt.Fprintf(os.Stderr, "thesis: shrink: corpus commit: %v\n", addErr)
				}
			}
		}
	}

	if g.json {
		emitShrinkJSON(res, world, worldPath, corpusPath, runner.RunDir(), p.dir)
	} else if !g.quiet {
		printShrinkReport(res, world, worldPath, schedChoice, corpusPath)
	}

	return shrinkExitCode(res)
}

func parseStrictness(s string) (shrink.Strictness, error) {
	switch strings.ToLower(s) {
	case "class", "oracle_class":
		return shrink.MatchOracleClass, nil
	case "key", "witness_key":
		return shrink.MatchWitnessKey, nil
	case "ops", "witness_ops":
		return shrink.MatchWitnessOps, nil
	default:
		return shrink.MatchUnset, fmt.Errorf("unknown strictness %q (want: class, key, or ops)", s)
	}
}

func resolveOriginalViolation(worldPath, expect string) (shrink.Identity, error) {
	worldDir := filepath.Dir(worldPath)
	verdictPath := filepath.Join(filepath.Dir(worldDir), "verdict.json")
	if data, err := os.ReadFile(verdictPath); err == nil {
		var v schema.Verdict
		if err := json.Unmarshal(data, &v); err == nil {
			for _, vio := range v.Violations {
				if expect == "" || vio.Oracle == expect {
					return shrink.IdentityOf(vio), nil
				}
			}
		}
	}

	resultPath := filepath.Join(worldDir, "result.json")
	if data, err := os.ReadFile(resultPath); err == nil {
		var r struct {
			Oracles []schema.OracleOutput `json:"oracles"`
		}
		if err := json.Unmarshal(data, &r); err == nil {
			for _, out := range r.Oracles {
				if out.Status == schema.StatusViolated {
					if expect == "" || out.Oracle == expect {
						return shrink.IdentityOfOutput(out), nil
					}
				}
			}
		}
	}

	if expect != "" {
		class := schema.ClassConsistency
		if strings.HasPrefix(expect, "no_crash") || strings.HasPrefix(expect, "no_panic") {
			class = schema.ClassCrash
		} else if strings.Contains(expect, "heal") || strings.Contains(expect, "stuck") {
			class = schema.ClassLiveness
		} else if strings.Contains(expect, "queue") || strings.Contains(expect, "resource") {
			class = schema.ClassResource
		}
		return shrink.Identity{
			Oracle: expect,
			Class:  class,
		}, nil
	}

	return shrink.Identity{}, fmt.Errorf("no violation found in world artifacts; pass --expect <oracle> explicitly")
}

type shrinkRunnerExecutor struct {
	runner   *control.Runner
	seed     uint64
	fullPlan *driver.Plan
	ordinal  int
}

func (e *shrinkRunnerExecutor) Execute(ctx context.Context, c shrink.Candidate) (shrink.Attempt, error) {
	e.ordinal++
	var plan *driver.Plan
	if c.Ops != nil && e.fullPlan != nil {
		p, err := e.fullPlan.Subset(c.Ops)
		if err != nil {
			return shrink.Attempt{Err: fmt.Errorf("plan subset: %w", err)}, nil
		}
		plan = p
	}

	seed := e.seed
	wr := control.WorldRequest{
		Ordinal: e.ordinal,
		Faults:  c.Faults,
		Seed:    &seed,
		Plan:    plan,
	}

	out := e.runner.RunWorld(ctx, wr)
	reached := out.Outcome == control.OutcomePass || out.Outcome == control.OutcomeViolation

	// The driver is NOT trusted to have honoured the plan.
	//
	// Stage 2 reduces the workload by handing the driver a shorter op list. If
	// the driver ignores it and drives its usual load, EVERY candidate still
	// reproduces: so ddmin reduces in a straight line, reports a spectacular
	// reduction, and emits a minimal_repro naming operations that were never the
	// reason for anything. Measured on run r_2026_09_09_2904: a plan of 1
	// operation, a history of 13,259 lines, and a reported "6761 -> 1".
	//
	// Nothing about that is detectable from the oracle verdict, which is why it
	// has to be checked here, against the artifact the driver actually wrote.
	if plan != nil && c.Ops != nil {
		if err := planWasHonoured(out.Paths.History, len(c.Ops)); err != nil {
			return shrink.Attempt{Err: err}, nil
		}
	}

	var observed []shrink.Identity
	for _, f := range out.Findings {
		if f.Violated() {
			observed = append(observed, shrink.IdentityOfOutput(f.Output))
		}
	}

	return shrink.Attempt{
		Reached:   reached,
		Observed:  observed,
		WorldPath: out.Paths.World,
		Duration:  out.Duration,
		Err:       out.Err,
	}, nil
}

// planWasHonoured reports whether the history the driver wrote is consistent with
// the op plan it was handed.
//
// It counts INVOCATIONS, because that is the number the plan controls: a planned
// operation may complete, fail or time out, but a driver executing a plan of n
// operations cannot invoke more than n of them. Completions, phase markers and
// any other record shape are ignored.
//
// The comparison is deliberately generous: it fails only when the history holds
// MORE invocations than were planned, and by a clear margin. A driver that
// retires fewer is not disobeying: the world may have been stopped at QUIESCE
// before the plan ran out, which is normal and is not evidence of anything.
func planWasHonoured(historyPath string, planned int) error {
	f, err := os.Open(historyPath)
	if err != nil {
		// No history is a different failure, already reported by the run itself.
		return nil
	}
	defer f.Close()

	invokes := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<24)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(line, &e) != nil {
			continue
		}
		if e.Type == "invoke" {
			invokes++
		}
	}
	if invokes <= planned {
		return nil
	}
	return fmt.Errorf("the driver ignored its operation plan: %d operation(s) were planned and "+
		"the history records %d invocation(s). Stage 2 reduces the workload by SHORTENING that "+
		"plan, so a driver that ignores it makes every candidate reproduce and turns ddmin into "+
		"a machine for inventing reductions. Wire {plan_path} / $PROTHESIS_PLAN_PATH into the "+
		"driver (D-021), or pass --skip-ops", planned, invokes)
}

func shrinkExitCode(res *shrink.Result) schema.ExitCode {
	if res.Stop == shrink.StopWallBudget || res.Stop == shrink.StopWorldBudget {
		return schema.ExitBudgetExhausted
	}
	if res.Stop == shrink.StopBaselineUnjudgeable || res.Stop == shrink.StopExecutorError {
		return schema.ExitInconclusive
	}
	if res.Stop == shrink.StopBaselineNotReproduced {
		return schema.ExitFail
	}
	if !res.Confirmation.Passed() {
		return schema.ExitFail
	}
	return schema.ExitPass
}

func printShrinkReport(res *shrink.Result, world *schema.World, worldPath string, repChoice replay.ScheduleChoice, corpusPath string) {
	fmt.Printf("--- PRO-THESIS SHRINK ---\n")
	fmt.Printf("World:      %s\n", worldPath)
	fmt.Printf("Target:     %s\n", res.Original.String())
	// res.Policy.Strictness is the level actually ENFORCED, which is not the level
	// requested when stage 0 calibrated it. Printing the enforced level without
	// saying it was calibrated would be the silent identity change Rule 1 exists
	// to forbid, so the two are never printed apart.
	if res.Calibration != "" {
		fmt.Printf("Strictness: %s (CALIBRATED from %s)\n",
			res.Policy.Strictness.String(), shrink.MatchWitnessKey.String())
		fmt.Printf("            %s\n", res.Calibration)
	} else {
		fmt.Printf("Strictness: %s\n", res.Policy.Strictness.String())
	}
	fmt.Printf("Schedule:   %s\n", repChoice.Summary())
	fmt.Printf("\nStages:\n")
	for _, st := range res.Stages {
		status := "complete"
		if st.Skipped != "" {
			status = "SKIPPED (" + st.Skipped + ")"
		} else if st.Stop.Truncated() {
			status = "STOPPED (" + st.Stop.Describe() + ")"
		}
		countStr := ""
		if st.Attempted {
			countStr = fmt.Sprintf("%d -> %d", st.Before, st.After)
		}
		fmt.Printf("  %-10s  %-12s  %2d world(s)  %6s  %s\n",
			st.Name, countStr, st.Worlds, st.Elapsed.Round(time.Millisecond), status)
		if st.Note != "" {
			fmt.Printf("              note: %s\n", st.Note)
		}
	}

	if len(res.Divergences) > 0 {
		fmt.Printf("\nDIVERGENT violations (Rule 1: rejected reductions that triggered a different bug):\n")
		for _, d := range res.Divergences {
			fmt.Printf("  %s\n", d.String())
		}
	}

	fmt.Printf("\nMinimization summary:\n")
	fmt.Printf("  Faults:       %d -> %d\n", res.FaultsBefore, res.FaultsAfter)
	for _, f := range res.SurvivingFaults {
		fmt.Printf("                %s\n", f)
	}
	if res.OpsBefore > 0 {
		fmt.Printf("  Operations:   %d -> %d\n", res.OpsBefore, res.OpsAfter)
	}
	fmt.Printf("  Total cost:   %d world(s) in %s\n", res.WorldsRun, res.Elapsed.Round(time.Second))
	fmt.Printf("  Confirmation: %s (target %d/%d)\n", res.Confirmation.String(), res.Confirmation.K, res.Confirmation.K)

	// A gate that fails after reductions were accepted on WEAKER evidence than
	// the gate itself demands is not a mystery, and the report should not present
	// it as one. ddmin assumes a deterministic predicate; where it is not, a
	// subset accepted on one lucky world is discarded-from forever, and the first
	// place anyone finds out is here.
	if res.Confirmation.Ran() && !res.Confirmation.Passed() &&
		res.Policy.AcceptTrials < res.Confirmation.K {
		fmt.Printf("\nWhy this may have failed:\n")
		fmt.Printf("  Reductions were ACCEPTED on %d reproduction(s) each, and this gate asks for "+
			"%d.\n", res.Policy.AcceptTrials, res.Confirmation.K)
		fmt.Printf("  ddmin assumes the predicate is a function of the input. Where the violation\n")
		fmt.Printf("  is probabilistic, one reproduction is a sample rather than a proof, and a\n")
		fmt.Printf("  subset accepted by luck drops the faults it excluded permanently — ddmin\n")
		fmt.Printf("  does not revisit them. Re-run with --accept-trials %d to make the evidence\n",
			res.Confirmation.K)
		fmt.Printf("  for accepting a reduction as strong as the evidence for believing one.\n")
	}

	if corpusPath != "" {
		fmt.Printf("\nCommitted to regression corpus:\n")
		fmt.Printf("  %s\n", corpusPath)
	}

	code := shrinkExitCode(res)
	fmt.Printf("\nVerdict:    %s (exit %d)\n", code.Verdict(), code)
}

func emitShrinkJSON(res *shrink.Result, world *schema.World, worldPath string, corpusPath string, runDir, projectDir string) {
	stages := make([]map[string]any, 0, len(res.Stages))
	for _, st := range res.Stages {
		stages = append(stages, map[string]any{
			"name":        string(st.Name),
			"attempted":   st.Attempted,
			"skipped":     st.Skipped,
			"before":      st.Before,
			"after":       st.After,
			"worlds":      st.Worlds,
			"elapsed_ms":  st.Elapsed.Milliseconds(),
			"stop_reason": string(st.Stop),
			"note":        st.Note,
		})
	}

	divergences := make([]string, 0, len(res.Divergences))
	for _, d := range res.Divergences {
		divergences = append(divergences, d.String())
	}

	doc := map[string]any{
		"schema":           ShrinkReportSchema,
		"world":            worldPath,
		"target_violation": res.Original.String(),
		"complete":         res.Complete,
		"stop_reason":      string(res.Stop),
		"faults_before":    res.FaultsBefore,
		"faults_after":     res.FaultsAfter,
		"surviving_faults": res.SurvivingFaults,
		"ops_before":       res.OpsBefore,
		"ops_after":        res.OpsAfter,
		"worlds_run":       res.WorldsRun,
		"elapsed_ms":       res.Elapsed.Milliseconds(),
		"confirmation": map[string]any{
			"k":          res.Confirmation.K,
			"trials":     res.Confirmation.Trials,
			"reproduced": res.Confirmation.Reproduced,
			"different":  res.Confirmation.Different,
			"unknown":    res.Confirmation.Unknown,
			"passed":     res.Confirmation.Passed(),
			"world":      res.Confirmation.World,
		},
		"divergences": divergences,
		"stages":      stages,
		"artifacts":   control.RelativeArtifacts(projectDir, runDir),
	}
	if corpusPath != "" {
		doc["corpus_entry"] = corpusPath
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(doc)
}
