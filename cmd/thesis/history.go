package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/WrdCstlg/pro-thesis/internal/historycheck"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

const historyUsage = `thesis history - check a history log against the driver contract

usage: thesis history verify PATH [PATH...]

Reads each history.jsonl and reports every structural breach of the contract a
driver must satisfy: records that are not valid JSON, records that are neither
a valid operation nor a valid marker, operations with no op_id, an op_id that
names two operations, a completion with no invoke, a completion that precedes
its own invoke, and timestamps that are not Unix epoch nanoseconds.

An operation left open at the end is reported separately, as INCONCLUSIVE. It
does not prove the file is wrong; it proves the history stops before the
answer, which is what no_stuck_op refuses over. A driver asked to drain that
leaves operations open is the usual cause.

It needs no prothesis.yaml, no Docker and no run: the point is to answer in a
second, against a file, what otherwise costs a whole world to learn.

What it CANNOT check is the rule that matters most. Whether a timeout was
recorded as "info" rather than "fail" is a claim about what the target did, and
no reading of the file can settle it. See docs/TARGETS.md.

Exit codes: 0 PASS (conforms) - 1 FAIL (a witnessed breach, with its line) -
2 INCONCLUSIVE (nothing to check, or a history that stops before the answer) -
5 CONFIG_ERROR (a path that cannot be read)
`

func cmdHistory(ctx context.Context, g globals, args []string) schema.ExitCode {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, historyUsage)
		return schema.ExitConfigError
	}
	switch args[0] {
	case "-h", "--help", "help":
		fmt.Print(historyUsage)
		return schema.ExitPass
	case "verify":
	default:
		errorf("history: unknown subcommand %q\n\n%s", args[0], historyUsage)
		return schema.ExitConfigError
	}

	paths := args[1:]
	if len(paths) == 0 {
		errorf("history verify: no path given\n\n%s", historyUsage)
		return schema.ExitConfigError
	}

	reports, unreadable := verifyPaths(ctx, paths)

	if g.json {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(reports); err != nil {
			errorf("%v", err)
			return schema.ExitConfigError
		}
	} else if !g.quiet {
		for _, rep := range reports {
			printHistoryReport(rep)
		}
	}

	// An unreadable path stays loud and outranks every other code: it is a
	// usage error, and no count of clean files makes it a pass. What it must
	// not do is erase the reports gathered either side of it.
	if len(unreadable) > 0 {
		for _, u := range unreadable {
			errorf("history verify: %v", u.Err)
		}
		errorf("history verify: %d of %d path(s) could not be read; %d were checked",
			len(unreadable), len(paths), len(reports))
		return schema.ExitConfigError
	}
	return worstHistoryExit(reports)
}

// unreadablePath is a path that could not be opened, read or closed, kept with
// the error that says why.
type unreadablePath struct {
	Path string
	Err  error
}

// verifyPaths checks every path it is given and reports which ones it could
// not read, rather than abandoning the run at the first failure.
func verifyPaths(ctx context.Context, paths []string) ([]*historycheck.Report, []unreadablePath) {
	reports := make([]*historycheck.Report, 0, len(paths))
	var bad []unreadablePath
	for _, p := range paths {
		if err := ctx.Err(); err != nil {
			bad = append(bad, unreadablePath{Path: p, Err: err})
			return reports, bad
		}
		f, err := os.Open(p)
		if err != nil {
			bad = append(bad, unreadablePath{Path: p, Err: err})
			continue
		}
		rep, err := historycheck.Check(f, p)
		closeErr := f.Close()
		switch {
		case err != nil:
			bad = append(bad, unreadablePath{Path: p, Err: fmt.Errorf("reading %s: %w", p, err)})
		case closeErr != nil:
			bad = append(bad, unreadablePath{Path: p, Err: fmt.Errorf("closing %s: %w", p, closeErr)})
		default:
			reports = append(reports, rep)
		}
	}
	return reports, bad
}

// worstHistoryExit reduces several files to one code. A witnessed breach
// outranks "nothing to check", for the same reason it does within one file.
func worstHistoryExit(reports []*historycheck.Report) schema.ExitCode {
	worst := schema.ExitPass
	for _, rep := range reports {
		switch rep.ExitCode() {
		case schema.ExitFail:
			return schema.ExitFail
		case schema.ExitInconclusive:
			worst = schema.ExitInconclusive
		}
	}
	return worst
}

func printHistoryReport(rep *historycheck.Report) {
	fmt.Printf("history verify: %s\n", rep.Path)
	fmt.Printf("  %d line(s), %d operation record(s), %d marker(s), %d operation(s)\n",
		rep.Lines, rep.Records, rep.Markers, rep.Operations)
	fmt.Printf("  %d indeterminate, %d unclosed, %d process(es), %d key(s)\n",
		rep.Indeterminate, rep.Unclosed, rep.Processes, rep.Keys)
	if len(rep.Types) > 0 {
		fmt.Printf("  types:")
		for _, tc := range rep.Types {
			fmt.Printf(" %s %d", tc.Type, tc.N)
		}
		fmt.Println()
	}
	if rep.FirstTNS != 0 {
		fmt.Printf("  span: t_ns %d to %d (%.3f s)\n",
			rep.FirstTNS, rep.LastTNS, float64(rep.LastTNS-rep.FirstTNS)/1e9)
	}
	for _, f := range rep.Findings {
		where := ""
		if f.Line > 0 {
			where = fmt.Sprintf("line %d: ", f.Line)
		}
		op := ""
		if f.OpID != nil {
			op = fmt.Sprintf(" [op_id %d]", *f.OpID)
		}
		fmt.Printf("  %s%s (%s)%s: %s\n", where, f.Code, f.Severity, op, f.Message)
	}
	switch {
	case rep.Violations() > 0:
		fmt.Printf("  FAIL: %d violation(s), %d warning(s)\n", rep.Violations(), rep.Warnings())
	case rep.ExitCode() == schema.ExitInconclusive:
		fmt.Printf("  INCONCLUSIVE: nothing to check\n")
	default:
		fmt.Printf("  OK: conforms, %d warning(s)\n", rep.Warnings())
	}
}
