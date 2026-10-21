package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/WrdCstlg/pro-thesis/internal/diagnose"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

const diagnoseUsage = `thesis diagnose - attribute every recorded non-terminal outcome to a known problem

usage: thesis diagnose [--out PATH] [--calibration PATH]

Reads .prothesis/known-problems.yaml and walks .prothesis/runs read-only.
Every inconclusive or otherwise unjudged world, and every non-terminal or
absent run verdict, must attribute to a registry entry. Anything that matches
no entry is UNATTRIBUTED and is printed with its evidence.

A project with no registry is diagnosed against an empty one: nothing can be
attributed, so every unjudged outcome is UNATTRIBUTED, and it says so.

A run expected to PASS (a gold-positive calibration arm: verdict.json's
expected, or a calibration record) that came back FAIL is a FALSE_REJECTION.
Its violating worlds must attribute to an entry with false_rejection: true;
an unexplained one is UNATTRIBUTED and joins the pool thesis cluster reads.
Per arm, the unexplained share of the arm's decided gold-positive runs
(TRUE_PASS + FALSE_REJECTION, a calibration report's denominator) is printed.

Flags:
  --out PATH          also write the report as JSON to PATH.
  --calibration PATH  a calibration_report.json (thesis report); its records
                      give each run's arm and pre-registered expectation, which
                      outrank what the run's verdict.json says.

Exit codes: 0 PASS (everything attributed) - 2 INCONCLUSIVE (unattributed
items exist) - 5 CONFIG_ERROR (invalid registry, unreadable runs, bad flags)
`

func cmdDiagnose(ctx context.Context, g globals, args []string) schema.ExitCode {
	var outPath, calPath string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-h", a == "--help", a == "help":
			fmt.Print(diagnoseUsage)
			return schema.ExitPass
		case a == "--out" || strings.HasPrefix(a, "--out="):
			v, ni, err := flagValue(args, i, "--out")
			if err != nil {
				errorf("diagnose: %v", err)
				return schema.ExitConfigError
			}
			i, outPath = ni, v
		case a == "--calibration" || strings.HasPrefix(a, "--calibration="):
			v, ni, err := flagValue(args, i, "--calibration")
			if err != nil {
				errorf("diagnose: %v", err)
				return schema.ExitConfigError
			}
			i, calPath = ni, v
		default:
			errorf("diagnose: unknown argument %q\n\n%s", a, diagnoseUsage)
			return schema.ExitConfigError
		}
	}

	_, projectDir, err := loadConfig(g)
	if err != nil {
		errorf("%v", err)
		return schema.ExitConfigError
	}

	reg, note, code := loadKnownProblems(projectDir)
	if reg == nil {
		return code
	}
	if note != "" && !g.quiet {
		fmt.Fprintf(os.Stderr, "thesis: diagnose: %s\n", note)
	}

	var opts diagnose.Options
	if calPath != "" {
		recs, err := readCalibrationRecords(calPath)
		if err != nil {
			errorf("diagnose: --calibration: %v", err)
			return schema.ExitConfigError
		}
		opts.Calibration = recs
	}

	runsDir := filepath.Join(projectDir, ".prothesis", "runs")
	rep, err := diagnose.DiagnoseWith(runsDir, reg, opts)
	if err != nil {
		errorf("%v", err)
		return schema.ExitConfigError
	}

	if outPath != "" {
		b, err := json.MarshalIndent(rep, "", "  ")
		if err == nil {
			err = os.WriteFile(outPath, append(b, '\n'), 0o644)
		}
		if err != nil {
			errorf("diagnose: --out: %v", err)
			return schema.ExitConfigError
		}
	}

	if g.json {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			errorf("%v", err)
			return schema.ExitConfigError
		}
	} else {
		fmt.Printf("diagnose: %d bundles, %d worlds examined\n", rep.Bundles, rep.Worlds)
		for _, c := range rep.Counts {
			fmt.Printf("  %-7s %-16s %5d  %s\n", c.ID, c.Classification, c.N, c.Title)
		}
		if len(rep.FalseRejections) > 0 {
			fmt.Printf("\nfalse rejections (expected PASS, came back FAIL), per arm:\n")
			for _, a := range rep.FalseRejections {
				arm := a.Arm
				if arm == "" {
					arm = "(no arm)"
				}
				fmt.Printf("  %-24s %d of %d decided run(s) falsely rejected, %d unexplained (%.1f%%)\n",
					arm, a.FalseRejections, a.Denominator, a.Unexplained, *a.UnexplainedPct)
			}
		}
		if len(rep.Unattributed) > 0 {
			fmt.Printf("\nUNATTRIBUTED (%d):\n", len(rep.Unattributed))
			for _, u := range rep.Unattributed {
				fmt.Printf("  %s %s: %s\n", u.Level, u.Bundle, u.Detail)
			}
		} else if len(rep.Counts) == 0 {
			fmt.Println("nothing needed attribution: every recorded outcome is terminal")
		} else {
			fmt.Println("every non-terminal outcome attributed to a known problem")
		}
		if outPath != "" {
			fmt.Printf("\nwritten: %s\n", outPath)
		}
	}

	if len(rep.Unattributed) > 0 {
		return schema.ExitInconclusive
	}
	return schema.ExitPass
}

// loadKnownProblems reads the project's known-problem registry. A project with
// none (a target that has not catalogued anything yet) is diagnosed against an
// empty registry, and the note says so: nothing can attribute, so every
// unjudged outcome is reported UNATTRIBUTED rather than the command refusing
// to look. A registry that exists and does not decode is a config error.
func loadKnownProblems(projectDir string) (*schema.KnownProblems, string, schema.ExitCode) {
	regPath := filepath.Join(projectDir, ".prothesis", "known-problems.yaml")
	regData, err := os.ReadFile(regPath)
	if os.IsNotExist(err) {
		return &schema.KnownProblems{Schema: schema.KnownProblemsSchema},
			fmt.Sprintf("no known-problem registry at %s; using an empty one, so every unjudged "+
				"outcome is UNATTRIBUTED", regPath), schema.ExitPass
	}
	if err != nil {
		errorf("%v", err)
		return nil, "", schema.ExitConfigError
	}
	reg, err := schema.DecodeKnownProblems(regData)
	if err != nil {
		errorf("%s: %v", regPath, err)
		return nil, "", schema.ExitConfigError
	}
	return reg, "", schema.ExitPass
}

// readCalibrationRecords reads every arm's records from a calibration report.
func readCalibrationRecords(path string) ([]schema.CalibrationRecord, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var rep schema.CalibrationReport
	if err := json.Unmarshal(b, &rep); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if rep.Schema != schema.CalibrationReportSchema {
		return nil, fmt.Errorf("%s: schema is %q, want %q", path, rep.Schema, schema.CalibrationReportSchema)
	}
	var out []schema.CalibrationRecord
	for _, a := range rep.Arms {
		out = append(out, a.Records...)
	}
	return out, nil
}
