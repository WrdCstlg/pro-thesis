package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/WrdCstlg/pro-thesis/internal/diagnose"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

const diagnoseUsage = `thesis diagnose - attribute every recorded non-terminal outcome to a known problem

usage: thesis diagnose

Reads .prothesis/known-problems.yaml and walks .prothesis/runs read-only.
Every inconclusive or otherwise unjudged world, and every non-terminal or
absent run verdict, must attribute to a registry entry. Anything that matches
no entry is UNATTRIBUTED and is printed with its evidence.

Exit codes: 0 PASS (everything attributed) - 2 INCONCLUSIVE (unattributed
items exist) - 5 CONFIG_ERROR (missing or invalid registry)
`

func cmdDiagnose(ctx context.Context, g globals, args []string) schema.ExitCode {
	if len(args) > 0 {
		switch args[0] {
		case "-h", "--help", "help":
			fmt.Print(diagnoseUsage)
			return schema.ExitPass
		default:
			errorf("diagnose: unknown argument %q\n\n%s", args[0], diagnoseUsage)
			return schema.ExitConfigError
		}
	}

	_, projectDir, err := loadConfig(g)
	if err != nil {
		errorf("%v", err)
		return schema.ExitConfigError
	}

	regPath := filepath.Join(projectDir, ".prothesis", "known-problems.yaml")
	regData, err := os.ReadFile(regPath)
	if err != nil {
		if os.IsNotExist(err) {
			errorf("no known-problem registry at %s", regPath)
			errorf("without one, no inconclusive outcome can be attributed; refusing to guess")
			return schema.ExitConfigError
		}
		errorf("%v", err)
		return schema.ExitConfigError
	}
	reg, err := schema.DecodeKnownProblems(regData)
	if err != nil {
		errorf("%s: %v", regPath, err)
		return schema.ExitConfigError
	}

	runsDir := filepath.Join(projectDir, ".prothesis", "runs")
	rep, err := diagnose.Diagnose(runsDir, reg)
	if err != nil {
		errorf("%v", err)
		return schema.ExitConfigError
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
		if len(rep.Unattributed) > 0 {
			fmt.Printf("\nUNATTRIBUTED (%d):\n", len(rep.Unattributed))
			for _, u := range rep.Unattributed {
				fmt.Printf("  %s %s: %s\n", u.Level, u.Bundle, u.Detail)
			}
		} else {
			fmt.Println("every non-terminal outcome attributed to a known problem")
		}
	}

	if len(rep.Unattributed) > 0 {
		return schema.ExitInconclusive
	}
	return schema.ExitPass
}
