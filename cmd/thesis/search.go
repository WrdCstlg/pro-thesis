package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/WrdCstlg/pro-thesis/internal/control"
	"github.com/WrdCstlg/pro-thesis/internal/harness"
	"github.com/WrdCstlg/pro-thesis/internal/lock"
	"github.com/WrdCstlg/pro-thesis/internal/search/engine"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

const searchUsage = `usage: thesis search [flags]

Run the Phase 4 guided search: Addendum A's Saboteur, or the uniform-random
baseline it is measured against.

Flags:
  --profile NAME       run profile (default: the first profile with search: true)
  --strategy NAME      saboteur | random | hybrid | llm (default: the config's search.strategy)
                       llm asks the local OpenAI-compatible endpoint named in
                       search.llm for each schedule, and requires --workers 1.
  --seed N             run seed; every world's seed derives from it
  --budget DURATION    narrow the profile's wall budget (never widens it)
  --worlds N           narrow the profile's world count (never widens it)
                       Narrowing either is recorded in the verdict and reports
                       oracle_lock: bypassed. A narrowed run that finds NOTHING
                       exits 2 (INCONCLUSIVE), never 0 — it did not perform the
                       gate the profile specifies. A violation still exits 1.
  --workers N          concurrent compose projects (default: 4)
  --worker-env SPEC    map worker slots onto this project's compose variables:
                         prefix=VAR      receives the per-worker name prefix
                         <node-id>=VAR   receives that node's published host port
                       e.g. --worker-env prefix=KV_PREFIX,kv-n1=KV_PORT_N1

WHETHER a search runs is decided by the PROFILE's ` + "`search:`" + ` boolean; WHICH search
runs is decided by the top-level ` + "`search:`" + ` block. ` + "`thesis gate`" + ` never runs MCTS.
`

func cmdSearch(ctx context.Context, g globals, args []string) schema.ExitCode {
	var (
		profile   string
		strategy  string
		seed      uint64
		budget    time.Duration
		worlds    int
		workers   int
		workerEnv string
	)

	need := func(i *int, flag string) (string, bool) {
		if *i+1 >= len(args) {
			errorf("search: %s needs a value", flag)
			return "", false
		}
		*i++
		return args[*i], true
	}

	for i := 0; i < len(args); i++ {
		a := args[i]
		var v string
		var ok bool
		switch {
		case a == "-h" || a == "--help":
			fmt.Print(searchUsage)
			return schema.ExitPass

		case a == "--profile":
			if v, ok = need(&i, a); !ok {
				return schema.ExitConfigError
			}
			profile = v
		case strings.HasPrefix(a, "--profile="):
			profile = strings.TrimPrefix(a, "--profile=")

		case a == "--strategy":
			if v, ok = need(&i, a); !ok {
				return schema.ExitConfigError
			}
			strategy = v
		case strings.HasPrefix(a, "--strategy="):
			strategy = strings.TrimPrefix(a, "--strategy=")

		case a == "--seed", strings.HasPrefix(a, "--seed="):
			v = strings.TrimPrefix(a, "--seed=")
			if a == "--seed" {
				if v, ok = need(&i, a); !ok {
					return schema.ExitConfigError
				}
			}
			s, err := strconv.ParseUint(v, 10, 64)
			if err != nil {
				errorf("search: invalid seed %q: %v", v, err)
				return schema.ExitConfigError
			}
			seed = s

		case a == "--budget", strings.HasPrefix(a, "--budget="):
			v = strings.TrimPrefix(a, "--budget=")
			if a == "--budget" {
				if v, ok = need(&i, a); !ok {
					return schema.ExitConfigError
				}
			}
			d, err := time.ParseDuration(v)
			if err != nil || d <= 0 {
				errorf("search: invalid budget %q (want a Go duration such as 30m)", v)
				return schema.ExitConfigError
			}
			budget = d

		case a == "--worlds", strings.HasPrefix(a, "--worlds="):
			v = strings.TrimPrefix(a, "--worlds=")
			if a == "--worlds" {
				if v, ok = need(&i, a); !ok {
					return schema.ExitConfigError
				}
			}
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 {
				errorf("search: invalid world count %q (want a positive integer)", v)
				return schema.ExitConfigError
			}
			worlds = n

		case a == "--workers", strings.HasPrefix(a, "--workers="):
			v = strings.TrimPrefix(a, "--workers=")
			if a == "--workers" {
				if v, ok = need(&i, a); !ok {
					return schema.ExitConfigError
				}
			}
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 {
				errorf("search: invalid worker count %q (want a positive integer)", v)
				return schema.ExitConfigError
			}
			workers = n

		case a == "--worker-env":
			if v, ok = need(&i, a); !ok {
				return schema.ExitConfigError
			}
			workerEnv = v
		case strings.HasPrefix(a, "--worker-env="):
			workerEnv = strings.TrimPrefix(a, "--worker-env=")

		default:
			errorf("search: unknown flag %q\n\n%s", a, searchUsage)
			return schema.ExitConfigError
		}
	}

	if strategy != "" && !schema.SearchStrategy(strategy).Valid() {
		errorf("search: unknown strategy %q (want one of %v)", strategy, schema.AllSearchStrategies)
		return schema.ExitConfigError
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

	// D-028 item 7, enforced at the point the profile is chosen rather than
	// deep inside the engine: the PROFILE's boolean decides WHETHER a search
	// runs. Defaulting to `smoke` here would make `thesis search` fail with a
	// message about the wrong profile, so a profile is only chosen for the user
	// when exactly one enables search.
	if profile == "" {
		names := searchProfiles(cfg)
		switch len(names) {
		case 0:
			errorf("search: no profile in %s sets `search: true`. WHETHER a search runs is a "+
				"property of the profile (D-028 item 7); enable it on a profile meant for "+
				"searching, never on a gate profile.", absConfig)
			return schema.ExitConfigError
		case 1:
			profile = names[0]
		default:
			errorf("search: %d profiles set `search: true` (%s); name one with --profile",
				len(names), strings.Join(names, ", "))
			return schema.ExitConfigError
		}
	}

	envFn, err := parseWorkerEnv(workerEnv, cfg)
	if err != nil {
		errorf("search: --worker-env: %v", err)
		return schema.ExitConfigError
	}

	projectDir := filepath.Dir(absConfig)

	// INVARIANT I6, checked before a container is booted, exactly as `run`
	// does. A drifted gate's PASS is worse than no run at all.
	lockReport, lockErr := lock.Gate(lock.GateOptions{
		ProjectDir:                projectDir,
		ConfigPath:                absConfig,
		ConfigBytes:               data,
		BuiltinOptionsFingerprint: builtinFingerprint(),
	})
	if lockErr != nil {
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
		fmt.Fprint(os.Stderr, lockReport.Diagnosis())
	}

	runner, err := control.NewRunner(control.RunnerOptions{
		ConfigPath: absConfig,
		Config:     cfg,
		ProjectDir: projectDir,
		Profile:    profile,
		Seed:       seed,
		OracleLock: lockReport.OracleLock(),
		Quiet:      g.quiet,
	})
	if err != nil {
		errorf("create runner: %v", err)
		return schema.ExitConfigError
	}

	eng, err := engine.New(engine.Options{
		Runner:     runner,
		Strategy:   schema.SearchStrategy(strategy),
		Workers:    workers,
		Budget:     budget,
		Worlds:     worlds,
		WorkerEnv:  envFn,
		Networks:   control.NetworkBudget{Probe: harness.FreeBridgeNetworks},
		OracleLock: lockReport.OracleLock(),
		Quiet:      g.quiet,
	})
	if err != nil {
		if errors.Is(err, engine.ErrSearchDisabled) {
			errorf("%v", err)
			return schema.ExitConfigError
		}
		errorf("create search engine: %v", err)
		return schema.ExitConfigError
	}

	res, runErr := eng.Run(ctx)
	if runErr != nil {
		errorf("search error: %v", runErr)
	}

	if g.json {
		b, _ := json.MarshalIndent(res.Verdict, "", "  ")
		fmt.Println(string(b))
	} else if !g.quiet {
		printSearchSummary(res)
		printVerdictSummary(res.Verdict)
	}
	return res.ExitCode
}

// searchProfiles lists the profiles that enable search, sorted.
func searchProfiles(cfg *schema.Config) []string {
	var out []string
	for name, p := range cfg.Profiles {
		if p.Search {
			out = append(out, name)
		}
	}
	sortStrings(out)
	return out
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// parseWorkerEnv turns --worker-env into the slot mapping the executor needs.
//
// With no spec it falls back to the mapping the CONFIG declares
// (harness.prefix_env and harness.nodes[].port_env), and only when the config
// declares none does it yield nil: the generic PROTHESIS_-namespaced variables
// of DefaultWorkerEnv.
//
// The fallback exists because the flag-only version could not be used correctly
// by accident. The claim that used to stand here (that a compose file which
// does not spell those variables "simply publishes the same ports for every
// worker, and the executor's own port check refuses to start") is false for the
// case that actually bites: that check catches two slots COLLIDING, which needs
// two slots. A single worker collides with nobody, publishes the file's default
// ports, and then drives against the slot's band, where nothing is listening.
// Measured on the reference fixture: 78,018 operation records, all 39,009 ops
// failed, nothing checkable (run r_2026_09_09_23c5). See OQ-056.
//
// The flag still wins when given, so an operator can override the config without
// editing it.
//
// A node id that is not in harness.nodes is an ERROR, not an ignored entry: a
// typo would silently leave one node unparameterised, which is the shape of
// failure that produces two worlds driving the same container.
func parseWorkerEnv(spec string, cfg *schema.Config) (func(control.WorkerSlot) []string, error) {
	if strings.TrimSpace(spec) == "" {
		return workerEnvFromConfig(cfg), nil
	}
	known := map[string]bool{}
	for _, n := range cfg.Harness.Nodes {
		known[n.ID] = true
	}
	prefixVar := ""
	portVars := map[string]string{}
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		k, v, found := strings.Cut(part, "=")
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if !found || k == "" || v == "" {
			return nil, fmt.Errorf("%q is not KEY=VAR", part)
		}
		if k == "prefix" {
			prefixVar = v
			continue
		}
		if !known[k] {
			return nil, fmt.Errorf("%q names no node in harness.nodes (have: %s)",
				k, strings.Join(nodeIDs(cfg), ", "))
		}
		portVars[k] = v
	}
	return control.ComposeVarEnv(prefixVar, portVars), nil
}

// workerEnvFromConfig builds the slot mapping from harness.prefix_env and
// harness.nodes[].port_env, or nil when the config declares neither.
//
// Nil is the honest answer for a topology file that does not parameterise its
// ports: there is nothing to map, DefaultWorkerEnv's generic names are all the
// executor can offer, and the BOOT-time port-agreement check (control.portsAgree)
// is what catches the case where that is not enough.
func workerEnvFromConfig(cfg *schema.Config) func(control.WorkerSlot) []string {
	if cfg == nil {
		return nil
	}
	portVars := map[string]string{}
	for _, n := range cfg.Harness.Nodes {
		if v := strings.TrimSpace(n.PortEnv); v != "" {
			portVars[n.ID] = v
		}
	}
	prefixVar := strings.TrimSpace(cfg.Harness.PrefixEnv)
	if prefixVar == "" && len(portVars) == 0 {
		return nil
	}
	return control.ComposeVarEnv(prefixVar, portVars)
}

func nodeIDs(cfg *schema.Config) []string {
	out := make([]string, 0, len(cfg.Harness.Nodes))
	for _, n := range cfg.Harness.Nodes {
		out = append(out, n.ID)
	}
	return out
}

func printSearchSummary(res engine.Result) {
	fmt.Printf("\n--- PRO-THESIS SEARCH ---\n")
	fmt.Printf("Strategy:   %s\n", res.Strategy)
	fmt.Printf("Worlds:     %d executed\n", len(res.Worlds))
	fmt.Printf("Stopped:    %s\n", orNone(res.StoppedBecause))
	fmt.Printf("Coverage:   %d log template(s), %d state(s) cumulative\n",
		res.Coverage.CumTemplates, res.Coverage.CumStates)

	if res.NetworksBefore >= 0 || res.NetworksAfter >= 0 {
		fmt.Printf("Networks:   %s free bridge networks before, %s after\n",
			countOrUnknown(res.NetworksBefore), countOrUnknown(res.NetworksAfter))
	}

	if res.FirstViolationOrdinal > 0 {
		fmt.Printf("\nFirst violation: world %d after %s, %d fault(s):\n",
			res.FirstViolationOrdinal, res.TimeToFirstViolation.Round(time.Second),
			len(res.FirstViolationFaults))
		for _, f := range res.FirstViolationFaults {
			fmt.Printf("  %s\n", f)
		}
	} else {
		// Said plainly. A search that found nothing has a right-censored
		// time-to-first-violation, which is exactly why D-029 pre-registers a
		// test on the COUNT of trials that found one.
		fmt.Printf("\nFirst violation: none within budget (time-to-first-violation is censored)\n")
	}

	if len(res.Signals) > 0 {
		fmt.Printf("\nTier 1 ranked signals (A.3):\n%s", engine.ExplainSignals(res.Signals, 10))
		if !res.ControlEvaluable {
			// Without a baseline every comparative criterion in A.3 reports
			// unknown rather than false, so the classifier ran on half its
			// evidence. A reader must not mistake the resulting DAMPENs for
			// "the system absorbed everything".
			fmt.Printf("  NOTE: the no-fault CONTROL world was not evaluable, so every\n")
			fmt.Printf("        comparative criterion above reported unknown rather than false.\n")
		}
	}
	if res.EscalationNote != "" {
		fmt.Printf("\nTier 2 was not activated: %s\n", res.EscalationNote)
	}
	if len(res.TreeStats) > 0 {
		fmt.Printf("\nTier 2 escalation trees:\n")
		for i, t := range res.TreeStats {
			fmt.Printf("  tree %d: %d node(s), %d expansion(s), %d forced / %d informed selection(s), "+
				"max visits %d, %d pruned\n",
				i+1, t.Nodes, t.Expansions, t.ForcedSelections, t.InformedSelections,
				t.MaxVisits, t.Pruned)
		}
		if !res.TreeContributed() {
			// D-015 / OQ-013, reported as a measurement rather than assumed.
			fmt.Printf("  NOTE: backpropagation never decided a selection — at this rollout count\n")
			fmt.Printf("        the tree is provably equivalent to ladder-ordered enumeration, and\n")
			fmt.Printf("        the escalation ladder is what carried the search (D-015, OQ-013).\n")
		}
	}
}

func orNone(s string) string {
	if s == "" {
		return "the world budget was fully spent"
	}
	return s
}

func countOrUnknown(n int) string {
	if n < 0 {
		return "unknown"
	}
	return strconv.Itoa(n)
}
