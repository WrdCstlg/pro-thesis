package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/WrdCstlg/pro-thesis/internal/control"
	"github.com/WrdCstlg/pro-thesis/internal/recorder"
	"github.com/WrdCstlg/pro-thesis/internal/replay"
	"github.com/WrdCstlg/pro-thesis/internal/shrink"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

const replayUsage = `usage: thesis replay WORLD [flags]

Replay a recorded .thesis world and report, without rounding, whether it
reproduced. WORLD is a path, a world filename (w_a41f.thesis) or a short id
(a41f); the last two resolve inside .prothesis/regressions/.

Flags:
  -k N              confirmation replays (default 3). Reported as k/n.
  --expect ORACLE   count only a violation from this oracle as a reproduction.
                    Without it, ANY violation counts and its identity is printed.
  --profile NAME    run profile to execute under. Default: the profile whose
                    driver_profile matches the world's.
  --budget DUR      wall budget for the whole replay (invariant I7).
  --trace           print the schedule decision and every attempt as it happens.
  --attach          echo the system under test's node logs to this terminal.
  --keep-up         leave the topology standing after the last attempt.
                    ` + "`thesis down`" + ` still tears it down.

Exit codes: 0 did not reproduce · 1 REPRODUCED · 2 could not be judged ·
            3 budget expired · 4 oracle drift · 5 bad config or arguments
`

func cmdReplay(ctx context.Context, g globals, args []string) schema.ExitCode {
	var (
		worldArg string
		attempts int
		expect   string
		profile  string
		budget   time.Duration
		trace    bool
		attach   bool
		keepUp   bool
	)

	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-h", a == "--help":
			fmt.Print(replayUsage)
			return schema.ExitPass
		case a == "--trace":
			trace = true
		case a == "--attach":
			attach = true
		case a == "--keep-up":
			keepUp = true
		case a == "-k" || strings.HasPrefix(a, "-k="), a == "--attempts" || strings.HasPrefix(a, "--attempts="):
			name := "-k"
			if strings.HasPrefix(a, "--attempts") {
				name = "--attempts"
			}
			v, ni, err := flagValue(args, i, name)
			if err != nil {
				errorf("replay: %v", err)
				return schema.ExitConfigError
			}
			i = ni
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 {
				errorf("replay: invalid %s %q (want a positive integer)", name, v)
				return schema.ExitConfigError
			}
			attempts = n
		case a == "--expect" || strings.HasPrefix(a, "--expect="):
			v, ni, err := flagValue(args, i, "--expect")
			if err != nil {
				errorf("replay: %v", err)
				return schema.ExitConfigError
			}
			i = ni
			expect = v
		case a == "--profile" || strings.HasPrefix(a, "--profile="):
			v, ni, err := flagValue(args, i, "--profile")
			if err != nil {
				errorf("replay: %v", err)
				return schema.ExitConfigError
			}
			i = ni
			profile = v
		case a == "--budget" || strings.HasPrefix(a, "--budget="):
			v, ni, err := flagValue(args, i, "--budget")
			if err != nil {
				errorf("replay: %v", err)
				return schema.ExitConfigError
			}
			i = ni
			d, err := parseDurationFlag("budget", v)
			if err != nil {
				errorf("replay: %v", err)
				return schema.ExitConfigError
			}
			budget = d
		case strings.HasPrefix(a, "-"):
			errorf("replay: unknown flag %q\n\n%s", a, replayUsage)
			return schema.ExitConfigError
		default:
			if worldArg != "" {
				errorf("replay: takes exactly one WORLD (already have %q, then %q)", worldArg, a)
				return schema.ExitConfigError
			}
			worldArg = a
		}
	}

	if worldArg == "" {
		fmt.Fprint(os.Stderr, replayUsage)
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
	// LoadWorld re-encodes and byte-compares (D-012 rule 8), so a hand-edited or
	// CRLF-translated world is refused here rather than silently participating in
	// a replay under an identity it no longer has.
	world, err := recorder.LoadWorld(worldPath)
	if err != nil {
		errorf("replay: %v", err)
		return schema.ExitConfigError
	}

	// --keep-up would write the CURRENT pointer, and CURRENT can name only one
	// live run. Refusing here costs a file read; refusing after the replay would
	// have already spent thirty seconds and two bridge networks.
	if keepUp {
		if live := control.KeepUpBlocked(p.dir); live != "" {
			errorf("replay: --keep-up: run %s is already up. Run `thesis down` first.", live)
			return schema.ExitConfigError
		}
	}

	if profile == "" {
		profile, err = runProfileFor(p.cfg, world.DriverProfile)
		if err != nil {
			errorf("replay: %v", err)
			return schema.ExitConfigError
		}
	} else if _, ok := p.cfg.Profiles[profile]; !ok {
		errorf("replay: unknown profile %q", profile)
		return schema.ExitConfigError
	}

	runID, err := allocRunID(p)
	if err != nil {
		errorf("replay: allocate run: %v", err)
		return schema.ExitInconclusive
	}
	// --attach means "do not detach from this run", so it also un-quiets the
	// harness: the phase-by-phase progress IS part of what a human attaches for.
	// What --attach adds beyond that is the system under test's OWN node output,
	// echoed after the world (printAttachedLogs). It is not a live stream, and
	// that distinction is stated rather than glossed: logs are collected at
	// TEARDOWN, before the topology is destroyed, and streaming them live would
	// need a BOOT hook the runner does not expose. Claiming one would be claiming
	// a capability this does not have.
	quiet := g.quiet && !attach
	runner, err := runnerFor(p, profile, runID, quiet, 0)
	if err != nil {
		errorf("replay: %v", err)
		return schema.ExitConfigError
	}

	var traceW io.Writer
	if trace || attach {
		traceW = os.Stderr
	}

	expectID := shrink.Identity{}
	strictness := shrink.MatchWitnessKey
	if expect != "" {
		expectID = shrink.Identity{Oracle: expect}
		// Only an oracle NAME was supplied, so only an oracle name can be
		// compared. Matching on class as well would compare the candidate's class
		// against an empty one and reject every reproduction. The report says
		// which rule was applied.
		strictness = shrink.MatchOracle
	}

	rctx, cancel := ctxWithBudget(ctx, budget)
	defer cancel()

	rep, err := replay.Run(rctx, replay.Options{
		World:      world,
		WorldPath:  worldPath,
		Exec:       replay.NewRunnerExecutor(runner),
		Expect:     expectID,
		Strictness: strictness,
		Attempts:   attempts,
		Budget:     budget,
		KeepUp:     keepUp,
		Trace:      traceW,
	})
	if err != nil {
		errorf("replay: %v", err)
		return schema.ExitConfigError
	}

	if attach {
		printAttachedLogs(rep)
	}

	if g.json {
		emitReplayJSON(rep, runner.RunDir(), p.dir)
	} else if !g.quiet {
		printReplayReport(rep, keepUp)
	}
	return rep.ExitCode()
}

// printAttachedLogs echoes each node's collected log.
func printAttachedLogs(rep replay.Report) {
	for _, a := range rep.Attempts {
		if a.Paths.Logs == "" {
			continue
		}
		ents, err := os.ReadDir(a.Paths.Logs)
		if err != nil {
			continue
		}
		for _, e := range ents {
			if e.IsDir() {
				continue
			}
			b, err := os.ReadFile(filepath.Join(a.Paths.Logs, e.Name()))
			if err != nil {
				continue
			}
			node := strings.TrimSuffix(e.Name(), filepath.Ext(e.Name()))
			fmt.Printf("\n--- attempt %d: %s ---\n", a.N, node)
			for _, line := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
				fmt.Printf("%s | %s\n", node, line)
			}
		}
	}
}

func printReplayReport(rep replay.Report, keepUp bool) {
	fmt.Printf("\n--- PRO-THESIS REPLAY ---\n")
	fmt.Printf("World:      %s\n", rep.WorldPath)
	fmt.Printf("Identity:   %s (seed %d)\n", rep.WorldHash, rep.Seed)
	fmt.Printf("Schedule:   %s\n", rep.Schedule.Summary())
	for _, f := range rep.Schedule.Faults {
		fmt.Printf("              %s\n", f)
	}
	for _, n := range rep.Schedule.Notes {
		fmt.Printf("  note:     %s\n", n)
	}
	fmt.Printf("Match rule: %s\n", rep.MatchRule)
	fmt.Printf("\nAttempts:\n")
	for _, a := range rep.Attempts {
		fmt.Printf("  %d/%d  %-20s %-14s %s\n", a.N, rep.Planned, a.Signal, a.Outcome,
			a.Duration.Round(time.Millisecond))
		if a.Reason != "" {
			fmt.Printf("        %s\n", a.Reason)
		}
		for _, f := range a.Found {
			fmt.Printf("        violation: %s\n", f)
		}
	}
	if rep.BudgetExpired {
		fmt.Printf("\nBUDGET EXPIRED after %d of %d attempt(s).\n", len(rep.Attempts), rep.Planned)
	}
	if d := rep.Divergences(); len(d) > 0 {
		fmt.Printf("\nDIVERGENT violations (NOT the one being replayed):\n")
		for _, id := range d {
			fmt.Printf("  %s\n", id)
		}
		fmt.Printf("  A world that reproduces something else is not a repro for what it claims.\n")
	}
	fmt.Printf("\nReproduced: %s\n", rep.Confirmation)
	if rep.Confirmation.Reproduced == 0 && rep.Confirmation.Attempts > 0 {
		fmt.Printf("            The world ran and did not reproduce. That is a fact about the world,\n")
		fmt.Printf("            not an error — under Tier B it is also not proof the bug is gone.\n")
	}
	if keepUp {
		for _, a := range rep.Attempts {
			if !a.KeptUp {
				continue
			}
			fmt.Printf("\nTopology left up (compose project %s). Run `thesis down` to remove it.\n", a.Project)
			for node, port := range a.HostPorts {
				fmt.Printf("  %-8s http://127.0.0.1:%d\n", node, port)
			}
		}
	}
	fmt.Printf("\nVerdict:    %s (exit %d)\n", rep.ExitCode().Verdict(), rep.ExitCode())
}

func emitReplayJSON(rep replay.Report, runDir, projectDir string) {
	atts := make([]map[string]any, 0, len(rep.Attempts))
	for _, a := range rep.Attempts {
		found := make([]string, 0, len(a.Found))
		for _, id := range a.Found {
			found = append(found, id.String())
		}
		div := make([]string, 0, len(a.Divergent))
		for _, id := range a.Divergent {
			div = append(div, id.String())
		}
		atts = append(atts, map[string]any{
			"n":               a.N,
			"signal":          string(a.Signal),
			"outcome":         string(a.Outcome),
			"reason":          a.Reason,
			"violations":      found,
			"divergent":       div,
			"duration_ms":     a.Duration.Milliseconds(),
			"artifacts":       control.RelativeArtifacts(projectDir, a.Paths.Dir),
			"kept_up":         a.KeptUp,
			"compose_project": a.Project,
		})
	}
	doc := map[string]any{
		"schema":          ReplayReportSchema,
		"world":           rep.WorldPath,
		"world_hash":      rep.WorldHash,
		"short_id":        rep.ShortID,
		"seed":            rep.Seed,
		"schedule_source": string(rep.Schedule.Source),
		"faults":          rep.Schedule.Faults,
		"unpinned":        rep.Schedule.Unpinned,
		"notes":           rep.Schedule.Notes,
		"match_rule":      rep.MatchRule,
		"attempts":        atts,
		"planned":         rep.Planned,
		"reproduced":      rep.Confirmation.String(),
		"budget_expired":  rep.BudgetExpired,
		"elapsed_ms":      rep.Elapsed.Milliseconds(),
		"artifacts":       control.RelativeArtifacts(projectDir, runDir),
		"exit_code":       int(rep.ExitCode()),
	}
	b, _ := json.MarshalIndent(doc, "", "  ")
	fmt.Println(string(b))
}
