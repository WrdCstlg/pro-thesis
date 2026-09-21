package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/WrdCstlg/pro-thesis/internal/bisect"
	"github.com/WrdCstlg/pro-thesis/internal/driver"
	"github.com/WrdCstlg/pro-thesis/internal/recorder"
	"github.com/WrdCstlg/pro-thesis/internal/replay"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

const bisectUsage = `usage: thesis bisect WORLD --good SHA --bad SHA [flags]

Find the first revision at which a recorded world reproduces. Each step checks
out a revision, REBUILDS the system under test, and replays the world.

The working tree is restored to where it started on every exit path, including
Ctrl-C and a failed build.

Required:
  --good SHA      a revision where the world is known NOT to reproduce
  --bad SHA       a revision where it does

Rebuilding is required, and must be stated:
  --build CMD     command to rebuild the target at each revision, run from the
                  project directory. For a containerised target this is the
                  image build, e.g.
                    --build "docker compose -f testdata/kvfixture/docker-compose.yml build"
                  It is split on whitespace and run directly; no shell.
  --no-build      state deliberately that no rebuild is needed. Without one of
                  these two, bisect refuses: the compose backend boots whatever
                  image is in the local store, so a bisect that only checks out
                  source measures ONE build against every revision and returns a
                  confident, wrong answer.

Flags:
  -k N            replays per revision (default 1)
  --budget DUR    wall budget for the whole search
  --profile NAME  run profile. Default: the profile matching the world's driver
  --allow-dirty   bisect over uncommitted changes to tracked files
  --no-verify     do not re-measure --good and --bad before searching
  --trace         print each step as it happens

Exit codes: 0 first bad revision found · 2 undetermined, or the tree could not
            be restored · 3 budget expired · 5 bad config or arguments
`

func cmdBisect(ctx context.Context, g globals, args []string) schema.ExitCode {
	var (
		worldArg   string
		good, bad  string
		buildCmd   string
		noBuild    bool
		attempts   int
		budget     time.Duration
		profile    string
		allowDirty bool
		noVerify   bool
		trace      bool
	)

	for i := 0; i < len(args); i++ {
		a := args[i]
		var err error
		switch {
		case a == "-h", a == "--help":
			fmt.Print(bisectUsage)
			return schema.ExitPass
		case a == "--trace":
			trace = true
		case a == "--no-build":
			noBuild = true
		case a == "--allow-dirty":
			allowDirty = true
		case a == "--no-verify":
			noVerify = true
		case a == "--good" || strings.HasPrefix(a, "--good="):
			good, i, err = flagValue(args, i, "--good")
		case a == "--bad" || strings.HasPrefix(a, "--bad="):
			bad, i, err = flagValue(args, i, "--bad")
		case a == "--build" || strings.HasPrefix(a, "--build="):
			buildCmd, i, err = flagValue(args, i, "--build")
		case a == "--profile" || strings.HasPrefix(a, "--profile="):
			profile, i, err = flagValue(args, i, "--profile")
		case a == "-k" || strings.HasPrefix(a, "-k="):
			var v string
			v, i, err = flagValue(args, i, "-k")
			if err == nil {
				n := 0
				if _, serr := fmt.Sscanf(v, "%d", &n); serr != nil || n < 1 {
					err = fmt.Errorf("invalid -k %q (want a positive integer)", v)
				} else {
					attempts = n
				}
			}
		case a == "--budget" || strings.HasPrefix(a, "--budget="):
			var v string
			v, i, err = flagValue(args, i, "--budget")
			if err == nil {
				budget, err = parseDurationFlag("budget", v)
			}
		case strings.HasPrefix(a, "-"):
			errorf("bisect: unknown flag %q\n\n%s", a, bisectUsage)
			return schema.ExitConfigError
		default:
			if worldArg != "" {
				errorf("bisect: takes exactly one WORLD (already have %q, then %q)", worldArg, a)
				return schema.ExitConfigError
			}
			worldArg = a
		}
		if err != nil {
			errorf("bisect: %v", err)
			return schema.ExitConfigError
		}
	}

	switch {
	case worldArg == "":
		fmt.Fprint(os.Stderr, bisectUsage)
		return schema.ExitConfigError
	case good == "" || bad == "":
		errorf("bisect: --good and --bad are both required")
		return schema.ExitConfigError
	case buildCmd == "" && !noBuild:
		errorf("bisect: a rebuild step is required.")
		errorf("        The compose backend boots whatever image is already in the local store, so a")
		errorf("        bisect that only checks out source measures ONE build against every revision")
		errorf("        and returns a confident, wrong answer. Pass --build CMD, or --no-build if the")
		errorf("        target genuinely needs no rebuild.")
		return schema.ExitConfigError
	case buildCmd != "" && noBuild:
		errorf("bisect: --build and --no-build contradict each other")
		return schema.ExitConfigError
	}

	p, code := loadProject(g)
	if code != schema.ExitPass {
		return code
	}

	worldPath := worldPathFor(p.dir, worldArg)
	if abs, err := filepath.Abs(worldPath); err == nil {
		worldPath = abs
	}
	world, err := recorder.LoadWorld(worldPath)
	if err != nil {
		errorf("bisect: %v", err)
		return schema.ExitConfigError
	}

	if profile == "" {
		profile, err = runProfileFor(p.cfg, world.DriverProfile)
		if err != nil {
			errorf("bisect: %v", err)
			return schema.ExitConfigError
		}
	} else if _, ok := p.cfg.Profiles[profile]; !ok {
		errorf("bisect: unknown profile %q", profile)
		return schema.ExitConfigError
	}

	var buildArgv []string
	if buildCmd != "" {
		buildArgv, err = driver.SplitCommand(buildCmd)
		if err != nil || len(buildArgv) == 0 {
			errorf("bisect: --build %q: %v", buildCmd, err)
			return schema.ExitConfigError
		}
	}

	var traceW io.Writer
	if trace {
		traceW = os.Stderr
	}

	repo := &bisect.Repo{Dir: p.dir}

	var buildFn func(context.Context, string) error
	if len(buildArgv) > 0 {
		buildFn = func(bctx context.Context, rev string) error {
			cmd := exec.CommandContext(bctx, buildArgv[0], buildArgv[1:]...)
			cmd.Dir = p.dir
			if trace {
				cmd.Stdout = os.Stderr
				cmd.Stderr = os.Stderr
			}
			return cmd.Run()
		}
	}

	// Each step gets its OWN run bundle. A bisect's steps are separate
	// executions of separate builds, and folding them into one bundle would make
	// the artifacts of a revision that was later excluded indistinguishable from
	// the answer's.
	testFn := func(tctx context.Context, rev string) (bisect.Verdict, string, error) {
		runID, err := allocRunID(p)
		if err != nil {
			return bisect.Skip, "", err
		}
		runner, err := runnerFor(p, profile, runID, g.quiet, 0)
		if err != nil {
			return bisect.Skip, "", err
		}
		rep, err := replay.Run(tctx, replay.Options{
			World:     world,
			WorldPath: worldPath,
			Exec:      replay.NewRunnerExecutor(runner),
			Attempts:  attempts,
			Trace:     traceW,
			// Expect is zero: a bisect asks "does this world reproduce here",
			// and any violation answers it. Narrowing to one oracle would need a
			// recorded identity the `.thesis` format does not carry.
		})
		if err != nil {
			return bisect.Skip, "", err
		}
		switch {
		case rep.Confirmation.Reproduced > 0:
			return bisect.Bad, "reproduced " + rep.Confirmation.String(), nil
		case rep.Confirmation.Inconclusive > 0 || rep.Confirmation.Attempts == 0:
			// Neither good nor bad. Guessing either way moves the answer by an
			// arbitrary number of revisions; git bisect calls this skip.
			return bisect.Skip, "could not be judged (" + rep.Confirmation.String() + ")", nil
		default:
			return bisect.Good, "did not reproduce " + rep.Confirmation.String(), nil
		}
	}

	bctx, cancel := ctxWithBudget(ctx, budget)
	defer cancel()

	rep, err := bisect.Run(bctx, bisect.Options{
		Repo:              repo,
		Good:              good,
		Bad:               bad,
		Build:             buildFn,
		NoBuild:           noBuild,
		BuildDescription:  buildCmd,
		Test:              testFn,
		SkipEndpointCheck: noVerify,
		AllowDirty:        allowDirty,
		Budget:            budget,
		Trace:             traceW,
	})
	if err != nil {
		errorf("bisect: %v", err)
		if rep.RestoreErr != nil {
			errorf("bisect: %v", rep.RestoreErr)
			return schema.ExitInconclusive
		}
		return schema.ExitConfigError
	}

	if g.json {
		emitBisectJSON(rep, buildCmd, worldPath)
	} else if !g.quiet {
		printBisectReport(rep, worldPath)
	}
	return rep.ExitCode()
}

func printBisectReport(rep bisect.Report, worldPath string) {
	fmt.Printf("\n--- PRO-THESIS BISECT ---\n")
	fmt.Printf("World:      %s\n", worldPath)
	fmt.Printf("Range:      %s (good) .. %s (bad), %d candidate revision(s)\n",
		rep.GoodShort, rep.BadShort, len(rep.Candidates))
	fmt.Printf("\nSteps:\n")
	for _, s := range rep.Steps {
		fmt.Printf("  %-10s %-6s %-40s %s\n", s.Short, s.Verdict, truncate(s.Subject, 40),
			s.Duration.Round(time.Second))
		if s.Reason != "" {
			fmt.Printf("             %s\n", s.Reason)
		}
	}
	if len(rep.Skipped) > 0 {
		fmt.Printf("\nSkipped (neither good nor bad): %s\n", strings.Join(rep.Skipped, ", "))
	}
	fmt.Println()
	if rep.Determined {
		fmt.Printf("FIRST BAD REVISION: %s %s\n", rep.FirstBadShort, rep.FirstBadSubject)
	} else {
		fmt.Printf("UNDETERMINED: %s\n", rep.Undetermined)
		if rep.RangeLo != "" {
			fmt.Printf("              The first bad revision is somewhere in %s..%s.\n", rep.RangeLo, rep.RangeHi)
		}
	}
	if rep.RestoreErr != nil {
		fmt.Printf("\n!! %v\n", rep.RestoreErr)
	} else if rep.Restored != "" {
		fmt.Printf("\nWorking tree restored to %s.\n", rep.Restored)
	}
	fmt.Printf("\nVerdict:    %s (exit %d)\n", rep.ExitCode().Verdict(), rep.ExitCode())
}

func emitBisectJSON(rep bisect.Report, buildCmd, worldPath string) {
	steps := make([]map[string]any, 0, len(rep.Steps))
	for _, s := range rep.Steps {
		steps = append(steps, map[string]any{
			"rev":         s.Rev,
			"short":       s.Short,
			"subject":     s.Subject,
			"verdict":     string(s.Verdict),
			"reason":      s.Reason,
			"duration_ms": s.Duration.Milliseconds(),
		})
	}
	skipped := rep.Skipped
	if skipped == nil {
		skipped = []string{}
	}
	doc := map[string]any{
		"schema":            BisectReportSchema,
		"world":             worldPath,
		"good":              rep.Good,
		"bad":               rep.Bad,
		"build":             buildCmd,
		"candidates":        len(rep.Candidates),
		"steps":             steps,
		"skipped":           skipped,
		"determined":        rep.Determined,
		"first_bad":         rep.FirstBad,
		"first_bad_short":   rep.FirstBadShort,
		"first_bad_subject": rep.FirstBadSubject,
		"undetermined":      rep.Undetermined,
		"range_lo":          rep.RangeLo,
		"range_hi":          rep.RangeHi,
		"budget_expired":    rep.BudgetExpired,
		"restored":          rep.Restored,
		"elapsed_ms":        rep.Elapsed.Milliseconds(),
		"exit_code":         int(rep.ExitCode()),
	}
	if rep.RestoreErr != nil {
		doc["restore_error"] = rep.RestoreErr.Error()
	}
	b, _ := json.MarshalIndent(doc, "", "  ")
	fmt.Println(string(b))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return s[:n]
	}
	return s[:n-1] + "…"
}
