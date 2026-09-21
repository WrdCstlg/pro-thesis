package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/WrdCstlg/pro-thesis/internal/control"
	"github.com/WrdCstlg/pro-thesis/internal/corpus"
	"github.com/WrdCstlg/pro-thesis/internal/replay"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

const regressUsage = `usage: thesis regress [flags]

Replay every world in the committed regression corpus (.prothesis/regressions/)
in deterministic order and report each one PASS, FAIL or INCONCLUSIVE.

A world that REPRODUCES is a FAIL: the regression is back. A world that does not
reproduce is a PASS. An empty corpus is exit 0 and says so in words — "there are
no regressions" and "0 regressions passed" are different facts.

Flags:
  --budget DUR   wall budget for the whole corpus (invariant I7). Worlds not
                 reached are REPORTED, never silently skipped.
  -k N           replays per world (default 1). A single reproduction answers
                 "is this bug back"; k/k belongs to the commit decision.
  --trace        print each world's result as it completes.

Exit codes: 0 no regression reproduced · 1 a regression REPRODUCED ·
            2 something could not be judged · 3 budget expired with corpus left ·
            4 oracle drift · 5 bad config or arguments
`

func cmdRegress(ctx context.Context, g globals, args []string) schema.ExitCode {
	var (
		budget   time.Duration
		attempts int
		trace    bool
	)

	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-h", a == "--help":
			fmt.Print(regressUsage)
			return schema.ExitPass
		case a == "--trace":
			trace = true
		case a == "--budget" || strings.HasPrefix(a, "--budget="):
			v, ni, err := flagValue(args, i, "--budget")
			if err != nil {
				errorf("regress: %v", err)
				return schema.ExitConfigError
			}
			i = ni
			d, err := parseDurationFlag("budget", v)
			if err != nil {
				errorf("regress: %v", err)
				return schema.ExitConfigError
			}
			budget = d
		case a == "-k" || strings.HasPrefix(a, "-k="):
			v, ni, err := flagValue(args, i, "-k")
			if err != nil {
				errorf("regress: %v", err)
				return schema.ExitConfigError
			}
			i = ni
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 {
				errorf("regress: invalid -k %q (want a positive integer)", v)
				return schema.ExitConfigError
			}
			attempts = n
		default:
			errorf("regress: unknown argument %q\n\n%s", a, regressUsage)
			return schema.ExitConfigError
		}
	}

	p, code := loadProject(g)
	if code != schema.ExitPass {
		return code
	}

	store := corpus.Open(p.dir)

	// Read the corpus BEFORE allocating a run bundle. An empty corpus must not
	// leave a run directory behind claiming something ran.
	entries, err := store.List()
	if err != nil {
		errorf("regress: %v", err)
		return schema.ExitInconclusive
	}
	if len(entries) == 0 {
		rep := replay.RegressReport{
			CorpusDir:    store.Dir(),
			CorpusExists: store.Exists(),
			Empty:        true,
		}
		if g.json {
			emitRegressJSON(rep, "", p.dir)
		} else if !g.quiet {
			fmt.Printf("\n--- PRO-THESIS REGRESS ---\n")
			fmt.Printf("%s\n", rep.Summary())
			fmt.Printf("\nNothing was checked. This is exit 0 because there is no regression to fail,\n")
			fmt.Printf("not because a corpus passed.\n")
		}
		return rep.ExitCode()
	}

	// A corpus with a member that cannot be replayed is REFUSED, before a run
	// bundle exists to suggest anything ran. Each such member is a committed
	// regression test that has stopped existing (misnamed, corrupt, or carrying
	// a world other than the one its name claims) and replaying the members
	// that happen to be intact would return a verdict about a corpus this command
	// has just proved it does not fully hold. INCONCLUSIVE is the honest code:
	// nothing here is evidence the regressions are fixed, and nothing here is
	// evidence they are back (OQ-062, D-063).
	var broken []corpus.Entry
	for _, e := range entries {
		if !e.OK() {
			broken = append(broken, e)
		}
	}
	if len(broken) > 0 {
		errorf("regress: %d of %d member(s) of the regression corpus at %s cannot be replayed; "+
			"refusing to run any of it", len(broken), len(entries), store.Dir())
		for _, e := range broken {
			fmt.Fprintf(os.Stderr, "        %-24s %v\n", e.Name, e.Err)
		}
		fmt.Fprintf(os.Stderr, "        A corpus member is a committed regression test. Repair it, or remove it "+
			"deliberately — removal is a lock change (D-004) — and rerun.\n")
		return schema.ExitInconclusive
	}

	runID, err := allocRunID(p)
	if err != nil {
		errorf("regress: allocate run: %v", err)
		return schema.ExitInconclusive
	}

	// One runner PER RUN PROFILE, all sharing this run bundle. A world names its
	// DRIVER profile and control.Runner is built against a RUN profile (D-045),
	// so a corpus spanning two driver profiles needs two runners, and a world
	// whose driver profile no longer resolves is reported INCONCLUSIVE by name
	// rather than run under somebody else's workload.
	runners := map[string]*control.Runner{}
	execFor := func(w *schema.World) (replay.Executor, error) {
		profile, err := runProfileFor(p.cfg, w.DriverProfile)
		if err != nil {
			return nil, err
		}
		if r, ok := runners[profile]; ok {
			return replay.NewRunnerExecutor(r), nil
		}
		r, err := runnerFor(p, profile, runID, g.quiet, 0)
		if err != nil {
			return nil, err
		}
		runners[profile] = r
		return replay.NewRunnerExecutor(r), nil
	}

	var traceW io.Writer
	if trace {
		traceW = os.Stderr
	}

	rctx, cancel := ctxWithBudget(ctx, budget)
	defer cancel()

	rep, err := replay.Regress(rctx, replay.RegressOptions{
		Corpus:   store,
		ExecFor:  execFor,
		Attempts: attempts,
		Budget:   budget,
		Trace:    traceW,
	})
	if err != nil {
		errorf("regress: %v", err)
		return schema.ExitInconclusive
	}

	runDir := ""
	for _, r := range runners {
		runDir = r.RunDir()
		break
	}

	if g.json {
		emitRegressJSON(rep, runDir, p.dir)
	} else if !g.quiet {
		printRegressReport(rep)
	}
	return rep.ExitCode()
}

func printRegressReport(rep replay.RegressReport) {
	fmt.Printf("\n--- PRO-THESIS REGRESS ---\n")
	fmt.Printf("Corpus:  %s\n\n", rep.CorpusDir)
	for _, w := range rep.Worlds {
		line := fmt.Sprintf("  %-16s %-12s", w.Entry.Name, w.Status)
		if w.Status != replay.StatusNotReached && w.Report.Confirmation.Attempts > 0 {
			line += " reproduced " + w.Report.Confirmation.String()
		}
		fmt.Println(line)
		if w.Err != nil {
			fmt.Printf("      %v\n", w.Err)
		}
		for _, a := range w.Report.Attempts {
			for _, id := range a.Found {
				fmt.Printf("      violation: %s\n", id)
			}
		}
	}
	fmt.Printf("\n%s\n", rep.Summary())
	if rep.NotReached > 0 {
		fmt.Printf("\nThe budget expired with corpus left. The worlds above marked NOT REACHED were\n")
		fmt.Printf("not run at all — this result is NOT evidence about them.\n")
	}
	fmt.Printf("\nVerdict: %s (exit %d)\n", rep.ExitCode().Verdict(), rep.ExitCode())
}

func emitRegressJSON(rep replay.RegressReport, runDir, projectDir string) {
	worlds := make([]map[string]any, 0, len(rep.Worlds))
	for _, w := range rep.Worlds {
		d := map[string]any{
			"world":  w.Entry.Name,
			"path":   w.Entry.Path,
			"status": string(w.Status),
		}
		if w.Entry.Hash != "" {
			d["world_hash"] = w.Entry.Hash
		}
		if w.Report.Confirmation.Attempts > 0 {
			d["reproduced"] = w.Report.Confirmation.String()
		}
		if w.Err != nil {
			d["error"] = w.Err.Error()
		}
		var found []string
		for _, a := range w.Report.Attempts {
			for _, id := range a.Found {
				found = append(found, id.String())
			}
		}
		if found == nil {
			found = []string{}
		}
		d["violations"] = found
		worlds = append(worlds, d)
	}
	doc := map[string]any{
		"schema":         RegressReportSchema,
		"corpus_dir":     rep.CorpusDir,
		"corpus_exists":  rep.CorpusExists,
		"empty":          rep.Empty,
		"worlds":         worlds,
		"passed":         rep.Passed,
		"failed":         rep.Failed,
		"inconclusive":   rep.Inconclusive,
		"not_reached":    rep.NotReached,
		"budget_expired": rep.BudgetExpired,
		"elapsed_ms":     rep.Elapsed.Milliseconds(),
		"summary":        rep.Summary(),
		"exit_code":      int(rep.ExitCode()),
	}
	if runDir != "" {
		doc["artifacts"] = control.RelativeArtifacts(projectDir, runDir)
	}
	b, _ := json.MarshalIndent(doc, "", "  ")
	fmt.Println(string(b))
}
