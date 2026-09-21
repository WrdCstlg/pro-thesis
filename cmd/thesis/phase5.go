package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/WrdCstlg/pro-thesis/internal/control"
	"github.com/WrdCstlg/pro-thesis/internal/corpus"
	"github.com/WrdCstlg/pro-thesis/internal/lock"
	"github.com/WrdCstlg/pro-thesis/internal/recorder"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// ---------------------------------------------------------------------------
// Shared wiring for the Phase 5 verbs: replay, regress, bisect
//
// All three execute worlds, so all three need the same four things `thesis run`
// needs and in the same order: the config bytes, a validated config, the I6
// drift gate BEFORE anything boots, and a runner. Factored here rather than
// copied three times, because the ORDER is the part that matters: a drifted
// gate must not boot a cluster (D-I), and three copies would eventually differ.
// ---------------------------------------------------------------------------

// ADDITIVE schema identifiers for the Phase 5 reports.
//
// The directive names no document for what `thesis replay`, `thesis regress` or
// `thesis bisect` emit. It names the EXIT CODES, which remain the contract.
// These follow the precedent already in the tree (OQ-027's prothesis.lock/v1 and
// friends) and are deliberately NOT shaped like prothesis.verdict/v1: none of
// them is a verdict over a run, and emitting a verdict-shaped document would
// invite an agent to read one as such.
const (
	ReplayReportSchema  = "prothesis.replay_report/v1"
	RegressReportSchema = "prothesis.regress_report/v1"
	BisectReportSchema  = "prothesis.bisect_report/v1"
	ShrinkReportSchema  = "prothesis.shrink_report/v1"
)

// project is the loaded, gated project state every Phase 5 verb starts from.
type project struct {
	cfg        *schema.Config
	configData []byte
	configPath string
	dir        string
	lockReport *lock.Report
}

// loadProject reads and validates prothesis.yaml and runs the I6 drift gate.
//
// It returns a non-zero exit code when the caller should stop, and has already
// printed the reason. The drift check runs BEFORE anything is booted, exactly as
// `thesis run` does it: there is no point spending a bridge network and thirty
// seconds on a run whose verdict is void before it begins, and a drifted gate's
// PASS is worse than no run at all because an agent would act on it.
func loadProject(g globals) (*project, schema.ExitCode) {
	absConfig, err := filepath.Abs(g.configPath)
	if err != nil {
		errorf("resolve config path: %v", err)
		return nil, schema.ExitConfigError
	}
	data, err := os.ReadFile(absConfig)
	if err != nil {
		if os.IsNotExist(err) {
			errorf("no config at %s (run `thesis init` to scaffold one)", absConfig)
		} else {
			errorf("read config %s: %v", absConfig, err)
		}
		return nil, schema.ExitConfigError
	}
	cfg, err := schema.DecodeConfig(data)
	if err != nil {
		errorf("%s: %v", absConfig, err)
		return nil, schema.ExitConfigError
	}
	if err := cfg.Validate(); err != nil {
		errorf("%s: %v", absConfig, err)
		return nil, schema.ExitConfigError
	}
	dir := filepath.Dir(absConfig)

	report, lockErr := lock.Gate(lock.GateOptions{
		ProjectDir:                dir,
		ConfigPath:                absConfig,
		ConfigBytes:               data,
		BuiltinOptionsFingerprint: builtinFingerprint(),
	})
	if lockErr != nil {
		errorf("oracle lock: %v", lockErr)
		return nil, schema.ExitInconclusive
	}
	if report.EnforceExitCode() != schema.ExitPass {
		fmt.Fprint(os.Stderr, report.Diagnosis())
		if g.json {
			emitLockJSON(report)
		}
		return nil, schema.ExitOracleDrift
	}
	if report.Status == schema.LockAbsent && !g.quiet {
		fmt.Fprint(os.Stderr, report.Diagnosis())
	}

	return &project{
		cfg:        cfg,
		configData: data,
		configPath: absConfig,
		dir:        dir,
		lockReport: report,
	}, schema.ExitPass
}

// runProfileFor finds the RUN profile that drives a world.
//
// A `.thesis` file records `driver_profile`, which names an entry of
// `driver.profiles`. control.Runner is constructed against a RUN profile and
// derives the driver profile from `profiles.<name>.driver_profile`; two
// different namespaces that D-045 records the cost of conflating: for three
// phases the reference config happened to name them identically, and the first
// config that did not produced a world whose oracles reported on a system
// nothing had driven.
//
// So the mapping is made explicitly, deterministically (sorted, first match),
// and a world whose driver profile no longer resolves is an ERROR naming both
// halves rather than a guess.
func runProfileFor(cfg *schema.Config, driverProfile string) (string, error) {
	if driverProfile == "" {
		return "", fmt.Errorf("the world records no driver_profile")
	}
	names := make([]string, 0, len(cfg.Profiles))
	for name := range cfg.Profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if cfg.Profiles[name].DriverProfile == driverProfile {
			return name, nil
		}
	}
	return "", fmt.Errorf("the world was driven by driver profile %q, and no run profile in %s names it "+
		"(profiles: %s). Add a profile whose driver_profile is %q, or pass --profile to choose one "+
		"deliberately — running it under a different workload would replay a different world",
		driverProfile, "prothesis.yaml", strings.Join(names, ", "), driverProfile)
}

// runnerFor builds a control.Runner for one profile inside an existing run
// bundle.
//
// runID is deliberately shared across every runner a command creates, so one
// `thesis regress` produces ONE run bundle however many driver profiles its
// corpus spans.
func runnerFor(p *project, profile, runID string, quiet bool, budget time.Duration) (*control.Runner, error) {
	return control.NewRunner(control.RunnerOptions{
		ConfigPath: p.configPath,
		Config:     p.cfg,
		ProjectDir: p.dir,
		Profile:    profile,
		RunID:      runID,
		Budget:     budget,
		OracleLock: p.lockReport.OracleLock(),
		Quiet:      quiet,
	})
}

// allocRunID reserves a run bundle for a Phase 5 command.
func allocRunID(p *project) (string, error) {
	runsDir := recorder.RunsDir(p.dir)
	if d := p.cfg.Artifacts.Dir; d != "" {
		if filepath.IsAbs(d) {
			runsDir = d
		} else {
			runsDir = filepath.Join(p.dir, d)
		}
	}
	id, _, err := recorder.AllocRunID(runsDir, time.Now().UTC())
	return id, err
}

// parseDurationFlag reads a Go duration from a flag value.
func parseDurationFlag(name, v string) (time.Duration, error) {
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("invalid %s %q (want a Go duration such as 90s or 10m)", name, v)
	}
	return d, nil
}

// flagValue reads `--name VALUE` or `--name=VALUE` from args at index i,
// returning the value and the new index.
func flagValue(args []string, i int, name string) (string, int, error) {
	a := args[i]
	if strings.HasPrefix(a, name+"=") {
		return strings.TrimPrefix(a, name+"="), i, nil
	}
	if i+1 >= len(args) {
		return "", i, fmt.Errorf("%s needs a value", name)
	}
	return args[i+1], i + 1, nil
}

// worldPathFor resolves a positional world argument.
//
// A bare short id (`a41f`) or a bare world filename (`w_a41f.thesis`) resolves
// inside the regression corpus, because that is where a `minimal_repro.world`
// points and typing the whole path is friction on the one command an agent runs
// most. Anything containing a separator is taken as a path, untouched.
func worldPathFor(projectDir, arg string) string {
	if strings.ContainsAny(arg, `/\`) {
		return arg
	}
	if _, err := schema.ParseWorldFilename(arg); err == nil {
		return filepath.Join(corpus.Dir(projectDir), arg)
	}
	if schema.ValidShortID(arg) {
		if name, err := schema.WorldFilename(arg); err == nil {
			return filepath.Join(corpus.Dir(projectDir), name)
		}
	}
	return arg
}

// ctxWithBudget bounds a command's whole execution when a budget is given.
func ctxWithBudget(ctx context.Context, budget time.Duration) (context.Context, context.CancelFunc) {
	if budget <= 0 {
		return ctx, func() {}
	}
	// The budget is enforced by the report loops, which stop BEFORE starting a
	// world. This context is the backstop for a single world that overruns it
	// badly, and it is deliberately generous: cutting a world off mid-flight
	// leaves a topology behind, so the loop's own check is the mechanism and
	// this is only the ceiling.
	return context.WithTimeout(ctx, budget*2)
}
