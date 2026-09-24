package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/WrdCstlg/pro-thesis/internal/doctor"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

const doctorUsage = `thesis doctor — verify harness, container and target readiness pre-flight

usage: thesis doctor [flags]

Runs pre-flight readiness checks against the Docker engine, probe host,
driver command, external oracles, project directory writability, and
health probe configurations. Reports every check performed so a reader
can tell "nothing was wrong" from "nothing was looked at".

Exit codes:
  0 PASS          ready (all checks passed)
  2 INCONCLUSIVE  environment check could not be performed (e.g. docker unreachable)
  5 CONFIG_ERROR  configuration or platform check failed (e.g. driver platform mismatch)
`

func cmdDoctor(ctx context.Context, g globals, args []string) schema.ExitCode {
	for _, a := range args {
		if a == "-h" || a == "--help" || a == "help" {
			fmt.Print(doctorUsage)
			return schema.ExitPass
		}
	}

	cfg, projectDir, err := loadConfig(g)
	if err != nil {
		errorf("doctor: %v", err)
		return schema.ExitConfigError
	}

	rep := doctor.Diagnose(ctx, cfg, projectDir, doctor.Options{})

	if g.json {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			errorf("doctor: encode json: %v", err)
			return schema.ExitInconclusive
		}
		return rep.ExitCode
	}

	if !g.quiet {
		for _, c := range rep.Checks {
			fmt.Printf("[%s] %s: %s\n", c.Status, c.Name, c.Detail)
		}
		switch rep.ExitCode {
		case schema.ExitPass:
			fmt.Printf("thesis doctor: ready (%d checks passed)\n", len(rep.Checks))
		case schema.ExitConfigError:
			fmt.Fprintf(os.Stderr, "thesis doctor: configuration check failed (exit 5)\n")
		case schema.ExitInconclusive:
			fmt.Fprintf(os.Stderr, "thesis doctor: environment check could not be performed (exit 2)\n")
		}
	}

	return rep.ExitCode
}
