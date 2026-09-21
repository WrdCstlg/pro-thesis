package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/WrdCstlg/pro-thesis/internal/control"
	"github.com/WrdCstlg/pro-thesis/internal/lock"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

func cmdRun(ctx context.Context, g globals, args []string) schema.ExitCode {
	profile := "smoke"
	var seed uint64
	var faults []string
	var budget time.Duration
	var worlds int

	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		// --fault is repeatable and takes one canonical fault string,
		// KIND(TARGET[, PARAMS])@start_ms..end_ms, with the window in
		// milliseconds relative to DRIVE start. The Phase 2 reference schedule is
		//
		//	--fault "proc.pause(role:leader)@8200..11000"
		//	--fault "net.partition(minority(kv))@8400..10900"
		//
		// (DECISIONS.md D-019: the pause must be SHORTER than the 5-second read
		// lease, or the lease expires during it and the resumed ex-leader
		// correctly refuses the stale read.)
		case a == "--fault":
			if i+1 >= len(args) {
				errorf("run: --fault needs a fault string, e.g. \"proc.pause(role:leader)@8200..11000\"")
				return schema.ExitConfigError
			}
			i++
			faults = append(faults, args[i])
		case strings.HasPrefix(a, "--fault="):
			faults = append(faults, strings.TrimPrefix(a, "--fault="))

		// --budget and --worlds are in the directive's CLI surface. They may
		// only NARROW the profile: widening one from argv would hand an agent a
		// gate-weakening lever that never touches prothesis.yaml, and profile
		// budgets are lock-covered precisely so that cannot happen.
		//
		// Narrowing is a gate-weakening lever too (D-059 retracts D-033's claim
		// that it is not) so a narrowed run reports oracle_lock: bypassed and
		// may not return PASS. Enforced in control.BuildVerdict.
		case a == "--budget", strings.HasPrefix(a, "--budget="):
			v := strings.TrimPrefix(a, "--budget=")
			if a == "--budget" {
				if i+1 >= len(args) {
					errorf("run: --budget needs a duration, e.g. 10m")
					return schema.ExitConfigError
				}
				i++
				v = args[i]
			}
			d, err := time.ParseDuration(v)
			if err != nil || d <= 0 {
				errorf("run: invalid budget %q (want a Go duration such as 90s or 10m)", v)
				return schema.ExitConfigError
			}
			budget = d

		case a == "--worlds", strings.HasPrefix(a, "--worlds="):
			v := strings.TrimPrefix(a, "--worlds=")
			if a == "--worlds" {
				if i+1 >= len(args) {
					errorf("run: --worlds needs a count")
					return schema.ExitConfigError
				}
				i++
				v = args[i]
			}
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 {
				errorf("run: invalid world count %q (want a positive integer)", v)
				return schema.ExitConfigError
			}
			worlds = n

		case a == "--profile":
			if i+1 >= len(args) {
				errorf("run: --profile needs a name")
				return schema.ExitConfigError
			}
			i++
			profile = args[i]
		case strings.HasPrefix(a, "--profile="):
			profile = strings.TrimPrefix(a, "--profile=")

		case a == "--seed":
			if i+1 >= len(args) {
				errorf("run: --seed needs a number")
				return schema.ExitConfigError
			}
			i++
			s, err := strconv.ParseUint(args[i], 10, 64)
			if err != nil {
				errorf("run: invalid seed %q: %v", args[i], err)
				return schema.ExitConfigError
			}
			seed = s
		case strings.HasPrefix(a, "--seed="):
			s, err := strconv.ParseUint(strings.TrimPrefix(a, "--seed="), 10, 64)
			if err != nil {
				errorf("run: invalid seed: %v", err)
				return schema.ExitConfigError
			}
			seed = s

		default:
			errorf("run: unknown flag %q", a)
			return schema.ExitConfigError
		}
	}

	absConfig, err := filepath.Abs(g.configPath)
	if err != nil {
		errorf("resolve config path: %v", err)
		return schema.ExitConfigError
	}
	data, err := os.ReadFile(absConfig)
	if err != nil {
		if os.IsNotExist(err) {
			errorf("no config at %s (run `thesis init` to scaffold one)", absConfig)
		} else {
			errorf("read config %s: %v", absConfig, err)
		}
		return schema.ExitConfigError
	}

	cfg, err := schema.DecodeConfig(data)
	if err != nil {
		errorf("%s: %v", absConfig, err)
		return schema.ExitConfigError
	}

	if err := cfg.Validate(); err != nil {
		errorf("%s: %v", absConfig, err)
		return schema.ExitConfigError
	}

	// Reject a malformed fault before a container is booted. The runner refuses
	// it too, at compile time, but failing here costs nothing and gives the
	// author the error next to the flag that caused it.
	for _, f := range faults {
		if _, err := schema.ParseFault(f); err != nil {
			errorf("run: --fault %q: %v", f, err)
			return schema.ExitConfigError
		}
	}

	projectDir := filepath.Dir(absConfig)

	// INVARIANT I6 ENFORCEMENT POINT (D-I).
	//
	// This runs BEFORE anything is booted. There is no point starting a
	// cluster, burning a bridge network and injecting faults for a run whose
	// verdict is void before it begins, and a drifted gate's PASS is worse
	// than no run at all, because an agent would act on it.
	//
	// Phase 3's definition of done names `thesis gate`, which is a Phase 6
	// deliverable. The check therefore lives in lock.Gate, which `thesis gate`
	// will wrap unchanged rather than reimplement.
	lockReport, lockErr := lock.Gate(lock.GateOptions{
		ProjectDir:                projectDir,
		ConfigPath:                absConfig,
		ConfigBytes:               data,
		BuiltinOptionsFingerprint: builtinFingerprint(),
	})
	if lockErr != nil {
		// Drift could not be DETERMINED: an unreadable oracles directory, a
		// corrupt lock file. That is exit 2, never a silent proceed: running
		// with the drift check disabled would produce a PASS that means less
		// than it appears to.
		errorf("oracle lock: %v", lockErr)
		return schema.ExitInconclusive
	}
	if lockReport.EnforceExitCode() != schema.ExitPass {
		fmt.Fprint(os.Stderr, lockReport.Diagnosis())
		if g.json {
			emitLockJSON(lockReport)
		}
		return schema.ExitOracleDrift
	}
	if lockReport.Status == schema.LockAbsent && !g.quiet {
		// Absent does not block the run. It is not evidence that anything was
		// weakened, and refusing to run a project that has never been locked
		// would be a spurious exit 4 on first contact. It is still said out
		// loud, and the verdict records "absent" rather than "ok".
		fmt.Fprint(os.Stderr, lockReport.Diagnosis())
	}
	// Said BEFORE the run rather than after it, and said even when the lock is
	// ok: which is the only case that matters, because a swapped checker's
	// entire signature is that everything else looks normal. The verdict carries
	// it too (oracle_lock.executables_moved); this is for the human watching the
	// run start. See OQ-057, D-060.
	if len(lockReport.ExecutablesMoved) > 0 && !g.quiet && lockReport.Status == schema.LockOK {
		fmt.Fprint(os.Stderr, lock.FormatExecutableChanges(lockReport.ExecutablesMoved))
	}

	runner, err := control.NewRunner(control.RunnerOptions{
		ConfigPath: absConfig,
		Config:     cfg,
		ProjectDir: projectDir,
		Profile:    profile,
		Budget:     budget,
		Worlds:     worlds,
		Seed:       seed,
		Faults:     faults,
		OracleLock: lockReport.OracleLock(),
		// Injectors is deliberately left unset. The runner assembles the fault
		// families per world in control.BuildInjectors, because every world
		// boots fresh containers and an injector built for the previous world's
		// container ids fails mid-window with "no such object". Supplying them
		// here is the seam tests use to substitute fakes.
		Quiet: g.quiet,
	})
	if err != nil {
		errorf("create runner: %v", err)
		return schema.ExitConfigError
	}

	verdict, exitCode, err := runner.Run(ctx)
	if err != nil {
		errorf("run error: %v", err)
	}

	if g.json {
		b, _ := json.MarshalIndent(verdict, "", "  ")
		fmt.Println(string(b))
	} else if !g.quiet {
		printVerdictSummary(verdict)
	}

	return exitCode
}

func printVerdictSummary(v schema.Verdict) {
	fmt.Printf("\n--- PRO-THESIS VERDICT ---\n")
	fmt.Printf("Run ID:     %s\n", v.RunID)
	fmt.Printf("Profile:    %s\n", v.Profile)
	if v.Commit != "" {
		fmt.Printf("Commit:     %s\n", v.Commit)
	}
	fmt.Printf("Verdict:    %s (exit %d)\n", v.Verdict, v.Verdict.ExitCode())
	fmt.Printf("Budget:     %ds used / %ds wall, %d worlds run / %d planned\n",
		v.Budget.UsedS, v.Budget.WallS, v.Budget.WorldsRun, v.Budget.WorldsPlanned)
	// A narrowed budget is printed next to the verdict, not buried in the JSON.
	// The number a reader needs in order to judge the result is the one the
	// PROFILE asks for, and before D-059 it was not in the artifact at all.
	if v.Budget.Narrowed {
		req := "unbounded"
		if v.Budget.WorldsRequired > 0 {
			req = fmt.Sprintf("%d", v.Budget.WorldsRequired)
		}
		fmt.Printf("            NARROWED from the command line: profile %q specifies %s world(s). "+
			"A clean run under a narrowed budget cannot report PASS.\n", v.Profile, req)
	}
	if v.OracleLock.Status != schema.LockOK {
		fmt.Printf("Lock:       %s\n", v.OracleLock.Status)
	}
	// Printed next to the verdict, not only at run start. A PASS whose checker
	// changed is the result a reader is most likely to accept without scrolling
	// back, which is precisely why the qualification belongs here.
	if len(v.OracleLock.ExecutablesMoved) > 0 {
		fmt.Printf("Checker:    %s — the program(s) that decided this verdict changed since the "+
			"lock was written\n", strings.Join(v.OracleLock.ExecutablesMoved, ", "))
	}

	if len(v.Violations) > 0 {
		fmt.Printf("\nViolations (%d):\n", len(v.Violations))
		for _, viol := range v.Violations {
			fmt.Printf("  [%s] %s (%s, severity: %s)\n",
				viol.ID, viol.Oracle, viol.Class, viol.Severity)
			fmt.Printf("       %s\n", viol.Explanation)
			if len(viol.CausalTimeline) > 0 {
				fmt.Printf("       Causal Timeline (%d events):\n", len(viol.CausalTimeline))
				for _, ev := range viol.CausalTimeline {
					fmt.Printf("         t+%4dms [%s] %s\n", ev.TMS, ev.Event, ev.Detail)
				}
			}
		}
	}
	fmt.Printf("\nArtifacts:  %s\n", v.Artifacts)
}
