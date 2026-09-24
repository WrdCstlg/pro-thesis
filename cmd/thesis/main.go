// Command thesis is the PRO-THESIS control plane.
//
// Phase 0 implements init, up and down. Every other verb in the directive's CLI
// surface is declared here and refuses with a clear phase reference rather than
// a bare "unknown command", so the shape of the tool is discoverable before it
// is finished.
//
// EXIT CODES ARE NORMATIVE (directive section 4.7). An agent loop reads them:
//
//	0 PASS              proceed
//	1 FAIL              oracle violation; work from minimal_repro
//	2 INCONCLUSIVE      environment failure; retry once, then escalate
//	3 BUDGET_EXHAUSTED  soft warning; do not merge blind
//	4 ORACLE_DRIFT      stop, escalate to a human, NEVER auto-resolve
//	5 CONFIG_ERROR      fix prothesis.yaml
//
// The distinction that matters most in Phase 0 is 2 versus 5. A dead Docker
// daemon, a port already bound, or a cluster that will not come up are all
// ENVIRONMENT failures (2). Exit 5 is reserved for an invalid config or bad
// arguments: the things a human edits a file to fix.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"

	"github.com/WrdCstlg/pro-thesis/internal/buildinfo"
	"github.com/WrdCstlg/pro-thesis/internal/probehost"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

const usage = `thesis — PRO-THESIS, a system-level debugging protocol for agentic coding loops

usage: thesis <command> [flags]

Commands:
  init            scaffold prothesis.yaml and .prothesis/
  up              bring the topology up and wait for health
  down            tear the topology down
  run             execute the lifecycle state machine across planned worlds
  oracles         list registered oracles; verify and bump .prothesis/lock
  search          run the guided search (Saboteur / random baseline)
  shrink          minimize a violating world to its minimal reproducible schedule
  replay          replay a .thesis world and report k/n honestly
  regress         replay the committed regression corpus
  bisect          find the first revision at which a world reproduces
  diagnose        attribute every recorded non-terminal outcome to a known problem
  cluster         discover candidate failure categories in the unattributed pool
  history         check a history log against the driver contract, offline
  doctor          verify harness, container and target readiness pre-flight
  version         print version information

Declared, not yet implemented:
  gate, report, watch, serve

Global flags:
  --config PATH   path to prothesis.yaml (default: ./prothesis.yaml)
  --json          emit machine-readable output
  --quiet         suppress progress output

Exit codes: 0 PASS · 1 FAIL · 2 INCONCLUSIVE · 3 BUDGET_EXHAUSTED ·
            4 ORACLE_DRIFT · 5 CONFIG_ERROR
`

// globals holds flags every command accepts.
type globals struct {
	configPath string
	json       bool
	quiet      bool
}

func main() {
	os.Exit(int(run()))
}

func run() schema.ExitCode {
	args := os.Args[1:]
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return schema.ExitConfigError
	}

	cmd := args[0]
	rest := args[1:]

	switch cmd {
	case "-h", "--help", "help":
		fmt.Print(usage)
		return schema.ExitPass
	case "version", "--version":
		fmt.Printf("thesis %s (spec %s)\n", version, schema.ConfigVersion)
		if buildinfo.Stamped() {
			dirty := ""
			if buildinfo.Dirty == "1" {
				dirty = " (dirty tree)"
			}
			// A binary stamped by an older build.ps1 carries no source date;
			// provenance never errors, so say so and keep going.
			date := buildinfo.SourceDate
			if date == "" {
				date = "unknown"
			}
			fmt.Printf("build: commit %s%s, source date %s\n", buildinfo.Commit, dirty, date)
		} else {
			fmt.Println("build: unstamped (built without scripts/build.ps1 provenance)")
		}
		return schema.ExitPass
	}

	g, rest, err := parseGlobals(rest)
	if err != nil {
		fmt.Fprintf(os.Stderr, "thesis: %v\n", err)
		return schema.ExitConfigError
	}

	// Where host-side probes are sent. Absent means loopback, which is every
	// existing project. Present and unusable is refused here rather than at the
	// first probe, because a bad address turns every world INCONCLUSIVE and the
	// reason would otherwise be buried in a health-check timeout (D-087).
	if err := probehost.SetFromEnv(os.LookupEnv); err != nil {
		fmt.Fprintf(os.Stderr, "thesis: %v\n", err)
		return schema.ExitConfigError
	}

	// Ctrl-C must not leave a project behind holding one of the host's limited
	// bridge networks, so cancellation is plumbed through every command.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	switch cmd {
	case "init":
		return cmdInit(ctx, g, rest)
	case "up":
		return cmdUp(ctx, g, rest)
	case "down":
		return cmdDown(ctx, g, rest)
	case "run":
		return cmdRun(ctx, g, rest)
	case "oracles":
		return cmdOracles(ctx, g, rest)
	case "search":
		return cmdSearch(ctx, g, rest)
	case "replay":
		return cmdReplay(ctx, g, rest)
	case "regress":
		return cmdRegress(ctx, g, rest)
	case "bisect":
		return cmdBisect(ctx, g, rest)
	case "diagnose":
		return cmdDiagnose(ctx, g, rest)
	case "cluster":
		return cmdCluster(ctx, g, rest)
	case "shrink":
		return cmdShrink(ctx, g, rest)
	case "history":
		return cmdHistory(ctx, g, rest)
	case "doctor":
		return cmdDoctor(ctx, g, rest)

	case "gate", "report", "watch", "serve":
		fmt.Fprintf(os.Stderr, "thesis: `%s` is declared by the directive but not implemented yet.\n", cmd)
		fmt.Fprintf(os.Stderr, "        Phase 0 delivers init, up and down. Phase 1 delivers run. %s\n", phaseOf(cmd))
		return schema.ExitConfigError

	default:
		fmt.Fprintf(os.Stderr, "thesis: unknown command %q\n\n%s", cmd, usage)
		return schema.ExitConfigError
	}
}

// phaseOf says which phase a declared-but-unimplemented verb belongs to, so the
// message is a pointer rather than a dead end.
func phaseOf(cmd string) string {
	switch cmd {
	case "run":
		return "`run` arrives in Phase 1 with the lifecycle state machine and the built-in oracles."
	case "oracles":
		return "`oracles` arrives in Phase 3 with the external oracle contract and the lock manifest."
	case "search":
		return "`search` arrives in Phase 4 with the Saboteur engine."
	case "gate", "watch", "serve", "report":
		return "This arrives in Phase 6 with the agent-loop surfaces."
	default:
		return ""
	}
}

func parseGlobals(args []string) (globals, []string, error) {
	g := globals{configPath: "prothesis.yaml"}
	var rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--json":
			g.json = true
		case a == "--quiet":
			g.quiet = true
		case a == "--config":
			if i+1 >= len(args) {
				return g, nil, errors.New("--config needs a path")
			}
			i++
			g.configPath = args[i]
		case strings.HasPrefix(a, "--config="):
			g.configPath = strings.TrimPrefix(a, "--config=")
		default:
			rest = append(rest, a)
		}
	}
	return g, rest, nil
}

// loadConfig reads and validates prothesis.yaml.
//
// It uses ValidateForTopology rather than the full Validate: `up` and `down`
// need a harness, and nothing else. Demanding a driver, a perturber budget and
// run profiles before a user can boot their topology would make the first
// command anyone runs fail on fields Phase 0 has no use for.
func loadConfig(g globals) (*schema.Config, string, error) {
	path := g.configPath
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, "", err
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, "", fmt.Errorf("no config at %s (run `thesis init` to scaffold one)", abs)
		}
		return nil, "", err
	}
	cfg, err := schema.DecodeConfig(data)
	if err != nil {
		return nil, "", fmt.Errorf("%s: %w", abs, err)
	}
	if err := cfg.ValidateForTopology(); err != nil {
		return nil, "", fmt.Errorf("%s: %w", abs, err)
	}
	return cfg, filepath.Dir(abs), nil
}

func infof(g globals, format string, a ...any) {
	if !g.quiet && !g.json {
		fmt.Printf(format+"\n", a...)
	}
}

func errorf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "thesis: "+format+"\n", a...)
}
