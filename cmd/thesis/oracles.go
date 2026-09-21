package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/WrdCstlg/pro-thesis/internal/lock"
	"github.com/WrdCstlg/pro-thesis/internal/oracle"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
	"gopkg.in/yaml.v3"
)

const oraclesUsage = `thesis oracles — the oracle registry and the anti-gaming lock

usage:
  thesis oracles list                      registered oracles, with class and valid phases
  thesis oracles verify                    recompute the manifest and compare; exit 4 on drift
  thesis oracles lock --reason "..."       write or refresh .prothesis/lock

Exit codes: 0 PASS · 2 INCONCLUSIVE (could not determine) · 4 ORACLE_DRIFT · 5 CONFIG_ERROR
`

func cmdOracles(ctx context.Context, g globals, args []string) schema.ExitCode {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, oraclesUsage)
		return schema.ExitConfigError
	}
	sub := args[0]
	rest := args[1:]
	switch sub {
	case "-h", "--help", "help":
		fmt.Print(oraclesUsage)
		return schema.ExitPass
	case "list":
		return cmdOraclesList(ctx, g, rest)
	case "verify":
		return cmdOraclesVerify(ctx, g, rest)
	case "lock":
		return cmdOraclesLock(ctx, g, rest)
	default:
		errorf("oracles: unknown subcommand %q\n\n%s", sub, oraclesUsage)
		return schema.ExitConfigError
	}
}

// ---------------------------------------------------------------------------
// shared plumbing
// ---------------------------------------------------------------------------

// lockContext is everything the three subcommands need from disk.
//
// It reads the config bytes RAW and keeps them. The decoded Config is used only
// for display; the manifest is projected from the raw bytes, because digesting
// the resolved config would put every compiled-in default inside the digest and
// turn a thesis release into exit 4 on every downstream project (D-F).
type lockContext struct {
	configPath string
	projectDir string
	rawConfig  []byte
	cfg        *schema.Config
}

func loadLockContext(g globals) (*lockContext, schema.ExitCode) {
	abs, err := filepath.Abs(g.configPath)
	if err != nil {
		errorf("resolve config path: %v", err)
		return nil, schema.ExitConfigError
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		if os.IsNotExist(err) {
			errorf("no config at %s (run `thesis init` to scaffold one)", abs)
		} else {
			errorf("read config %s: %v", abs, err)
		}
		return nil, schema.ExitConfigError
	}
	cfg, err := schema.DecodeConfig(data)
	if err != nil {
		errorf("%s: %v", abs, err)
		return nil, schema.ExitConfigError
	}
	return &lockContext{
		configPath: abs,
		projectDir: filepath.Dir(abs),
		rawConfig:  data,
		cfg:        cfg,
	}, schema.ExitPass
}

// builtinFingerprint is the compiled-in built-in oracle tuning digest. It is
// recorded in the lock file and compared as a WARNING; it is never part of the
// manifest digest. See internal/lock's package doc and DECISIONS.md D-035.
func builtinFingerprint() string {
	fp, err := oracle.DefaultOptions().Fingerprint()
	if err != nil {
		return ""
	}
	return fp
}

// printJSON emits v as indented JSON with HTML escaping OFF.
//
// encoding/json escapes '<', '>' and '&' by default, which mangles every
// "old -> new" line in a drift report into "old -> new" and would corrupt
// an edge fault target such as n1<->n2. pkg/schema disables it for the same
// reason on every artifact it writes; this keeps the CLI consistent with that.
func printJSON(v any) {
	buf := &bytes.Buffer{}
	enc := json.NewEncoder(buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		errorf("encode json: %v", err)
		return
	}
	fmt.Print(buf.String())
}

func gateFor(lc *lockContext) (*lock.Report, error) {
	return lock.Gate(lock.GateOptions{
		ProjectDir:                lc.projectDir,
		ConfigPath:                lc.configPath,
		ConfigBytes:               lc.rawConfig,
		BuiltinOptionsFingerprint: builtinFingerprint(),
	})
}

// ---------------------------------------------------------------------------
// thesis oracles list
// ---------------------------------------------------------------------------

// listedOracle is one row of `thesis oracles list`.
type listedOracle struct {
	Name        string   `json:"name"`
	Kind        string   `json:"kind"`
	Class       string   `json:"class"`
	ValidPhases []string `json:"valid_phases"`
	Source      string   `json:"source"`
	Note        string   `json:"note,omitempty"`
}

func cmdOraclesList(_ context.Context, g globals, args []string) schema.ExitCode {
	if len(args) > 0 {
		errorf("oracles list: unexpected argument %q", args[0])
		return schema.ExitConfigError
	}
	lc, code := loadLockContext(g)
	if lc == nil {
		return code
	}

	rows := builtinRows(lc.cfg)

	oraclesDir, err := lock.OracleDirFromConfig(lc.rawConfig)
	if err != nil {
		errorf("oracles list: %v", err)
		return schema.ExitConfigError
	}
	files, err := lock.HashOracleDir(lc.projectDir, oraclesDir)
	if err != nil {
		// An unreadable oracles directory is an ENVIRONMENT failure, not an
		// empty one. Listing zero external oracles because the directory could
		// not be read would misreport the registry.
		errorf("oracles list: %v", err)
		return schema.ExitInconclusive
	}
	rows = append(rows, externalRows(lc.projectDir, oraclesDir, files)...)

	if g.json {
		printJSON(rows)
		return schema.ExitPass
	}

	fmt.Printf("Registered oracles (%d)\n\n", len(rows))
	if len(rows) == 0 {
		fmt.Printf("  NONE. `oracles.builtin` is empty and %s holds no files.\n", oraclesDir)
		fmt.Printf("  A run with no oracle checks nothing, and a verdict over nothing is not a PASS.\n")
	}
	for _, r := range rows {
		fmt.Printf("  %-30s %-10s %-12s %s\n", r.Name, r.Kind, r.Class, "["+strings.Join(r.ValidPhases, " ")+"]")
		fmt.Printf("  %-30s %s\n", "", r.Source)
		if r.Note != "" {
			fmt.Printf("  %-30s %s\n", "", r.Note)
		}
	}

	rep, err := gateFor(lc)
	if err != nil {
		fmt.Printf("\nLock: could not be determined: %v\n", err)
		return schema.ExitInconclusive
	}
	fmt.Printf("\n%s", rep.Diagnosis())
	return schema.ExitPass
}

func builtinRows(cfg *schema.Config) []listedOracle {
	rows := make([]listedOracle, 0, len(cfg.Oracles.Builtin))
	want := map[schema.BuiltinOracle]bool{}
	for _, b := range cfg.Oracles.Builtin {
		want[b] = true
	}
	// The directive's order, not the file's, so two configs listing the same
	// six in different orders print identically.
	for _, b := range schema.AllBuiltinOracles {
		if !want[b] {
			continue
		}
		phases := make([]string, 0, 4)
		for _, p := range b.ValidPhases() {
			phases = append(phases, string(p))
		}
		rows = append(rows, listedOracle{
			Name:        string(b),
			Kind:        "builtin",
			Class:       string(b.Class()),
			ValidPhases: phases,
			Source:      "compiled in (internal/oracle)",
		})
	}
	return rows
}

// externalOracleDecl is a BEST-EFFORT read of an external oracle definition.
//
// The field names are the frozen prothesis.oracle_output/v1 spellings, so
// nothing normative is invented here. This is for DISPLAY only: the engine
// learns an external oracle's class and valid phases from the oracle itself,
// and a file that does not parse is still locked, still listed, and simply
// shows no declaration. Logged as OQ-028.
type externalOracleDecl struct {
	Name        string   `yaml:"name" json:"name"`
	Class       string   `yaml:"class" json:"class"`
	ValidPhases []string `yaml:"valid_phases" json:"valid_phases"`
}

func externalRows(projectDir, oraclesDir string, files []lock.OracleFile) []listedOracle {
	base := oraclesDir
	if !filepath.IsAbs(base) {
		base = filepath.Join(projectDir, oraclesDir)
	}
	rows := make([]listedOracle, 0, len(files))
	for _, f := range files {
		row := listedOracle{
			Name:   f.Path,
			Kind:   "external",
			Source: filepath.ToSlash(filepath.Join(oraclesDir, f.Path)) + "  " + f.SHA256,
			Note: "class and valid phases are declared by the oracle process itself; " +
				"the DIGEST covers this DEFINITION, and since D-060 the executable it " +
				"names is fingerprinted outside the digest and reported when it moves",
		}
		if d, ok := readExternalDecl(filepath.Join(base, filepath.FromSlash(f.Path))); ok {
			if d.Name != "" {
				row.Name = d.Name
			}
			row.Class = d.Class
			row.ValidPhases = d.ValidPhases
		}
		rows = append(rows, row)
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
	return rows
}

// maxDeclBytes bounds the best-effort read. An oracle definition is a small
// document; a multi-megabyte file is not one, and `list` must not try to parse
// an arbitrary binary into memory.
const maxDeclBytes = 256 << 10

func readExternalDecl(path string) (externalOracleDecl, bool) {
	info, err := os.Stat(path)
	if err != nil || info.Size() > maxDeclBytes {
		return externalOracleDecl{}, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return externalOracleDecl{}, false
	}
	var d externalOracleDecl
	if err := yaml.Unmarshal(data, &d); err != nil {
		return externalOracleDecl{}, false
	}
	if d.Name == "" && d.Class == "" && len(d.ValidPhases) == 0 {
		return externalOracleDecl{}, false
	}
	return d, true
}

// ---------------------------------------------------------------------------
// thesis oracles verify
// ---------------------------------------------------------------------------

func cmdOraclesVerify(_ context.Context, g globals, args []string) schema.ExitCode {
	if len(args) > 0 {
		errorf("oracles verify: unexpected argument %q", args[0])
		return schema.ExitConfigError
	}
	lc, code := loadLockContext(g)
	if lc == nil {
		return code
	}
	rep, err := gateFor(lc)
	if err != nil {
		// Drift could not be DETERMINED. That is exit 2, never exit 0: a
		// verify that cannot read what it is verifying has verified nothing.
		errorf("oracles verify: %v", err)
		return schema.ExitInconclusive
	}
	if g.json {
		emitLockJSON(rep)
	} else {
		out := os.Stdout
		if rep.Status != schema.LockOK {
			out = os.Stderr
		}
		fmt.Fprint(out, rep.Diagnosis())
	}
	return rep.VerifyExitCode()
}

// lockJSON is the machine-readable form of a drift check.
//
// ADDITIVE and deliberately NOT shaped like prothesis.verdict/v1: a drift check
// is not a verdict, no world ran, and emitting a verdict-shaped document would
// invite an agent to read it as one. Logged as OQ-027.
type lockJSON struct {
	Schema      string   `json:"schema"`
	Status      string   `json:"status"`
	ManifestSHA string   `json:"manifest_sha"`
	LockedSHA   string   `json:"locked_sha"`
	LockPath    string   `json:"lock_path"`
	Reason      string   `json:"reason"`
	LockedAt    string   `json:"locked_at"`
	Changes     []string `json:"changes"`
	ExitCode    int      `json:"exit_code"`
	Limitations []string `json:"limitations"`
	// ExecutablesMoved is the WARNING half: oracles whose resolved program no
	// longer matches the lock. It never contributes to ExitCode, see D-060,
	// so a consumer that branches only on the exit code must read this field to
	// learn that the program deciding its verdicts changed.
	ExecutablesMoved []string `json:"executables_moved,omitempty"`
}

const lockReportSchema = "prothesis.lock_report/v1"

func emitLockJSON(rep *lock.Report) {
	changes := make([]string, 0, len(rep.Changes))
	for _, c := range rep.Changes {
		changes = append(changes, c.String())
	}
	moved := make([]string, 0, len(rep.ExecutablesMoved))
	for _, c := range rep.ExecutablesMoved {
		moved = append(moved, fmt.Sprintf("%s: %s", c.Oracle, c.Detail))
	}
	printJSON(lockJSON{
		Schema:           lockReportSchema,
		Status:           string(rep.Status),
		ManifestSHA:      rep.ManifestSHA,
		LockedSHA:        rep.LockedSHA,
		LockPath:         rep.LockPath,
		Reason:           rep.Reason,
		LockedAt:         rep.LockedAt,
		Changes:          changes,
		ExitCode:         int(rep.VerifyExitCode()),
		Limitations:      lock.Limitations,
		ExecutablesMoved: moved,
	})
}

// ---------------------------------------------------------------------------
// thesis oracles lock
// ---------------------------------------------------------------------------

func cmdOraclesLock(_ context.Context, g globals, args []string) schema.ExitCode {
	reason := ""
	sawReason := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--reason":
			if i+1 >= len(args) {
				errorf("oracles lock: --reason needs a value")
				return schema.ExitConfigError
			}
			i++
			reason = args[i]
			sawReason = true
		case strings.HasPrefix(a, "--reason="):
			reason = strings.TrimPrefix(a, "--reason=")
			sawReason = true
		default:
			errorf("oracles lock: unknown flag %q", a)
			return schema.ExitConfigError
		}
	}

	// The reason is mandatory, and this refusal is the point of the command
	// rather than an ergonomic detail. A lock bump is meant to be a reviewable
	// commit; a bump with no stated reason is indistinguishable from an agent
	// quietly re-baselining a gate it could not pass.
	if !sawReason || strings.TrimSpace(reason) == "" {
		errorf("oracles lock: --reason is MANDATORY and must not be empty.")
		fmt.Fprintf(os.Stderr, "        A lock bump is a reviewable commit. A bump with no stated reason is\n")
		fmt.Fprintf(os.Stderr, "        indistinguishable from re-baselining a gate that could not be passed.\n")
		fmt.Fprintf(os.Stderr, "        e.g. thesis oracles lock --reason \"add linearizable.kv; widen no_crash grace to 200ms\"\n")
		return schema.ExitConfigError
	}

	lc, code := loadLockContext(g)
	if lc == nil {
		return code
	}

	// Compute the report FIRST, so the command can print what is about to
	// change. That diff is what the reviewer of the resulting commit reads.
	rep, err := gateFor(lc)
	if err != nil {
		errorf("oracles lock: %v", err)
		return schema.ExitInconclusive
	}

	// Gate already fingerprinted the resolved programs, so this records what the
	// same check measured rather than walking the definitions a second time:
	// two walks could disagree, and a lock that records a different binary than
	// the one the check compared against is worse than no fingerprint.
	f, err := lock.Write(lock.WriteOptions{
		ProjectDir:                lc.projectDir,
		Manifest:                  rep.Manifest,
		Reason:                    reason,
		ToolVersion:               version,
		BuiltinOptionsFingerprint: builtinFingerprint(),
		Executables:               rep.ExecutablesNow,
	})
	if err != nil {
		errorf("oracles lock: %v", err)
		return schema.ExitConfigError
	}

	if g.json {
		printJSON(f)
		return schema.ExitPass
	}

	fmt.Printf("wrote %s\n", lock.Path(lc.projectDir))
	fmt.Printf("  manifest %s\n", f.ManifestSHA)
	fmt.Printf("  reason   %s\n", f.Reason)
	fmt.Printf("  covers   %d oracle file(s) under %s, %d config value(s) across %s\n",
		len(f.Manifest.Oracles), rep.OraclesDir, len(f.Manifest.Config), strings.Join(f.Covers, ", "))
	switch rep.Status {
	case schema.LockAbsent:
		fmt.Printf("  this is the FIRST lock for this project; drift is enforced from now on\n")
	case schema.LockOK:
		fmt.Printf("  no change: the manifest already matched\n")
	case schema.LockMismatch:
		fmt.Printf("  RE-BASELINED. What this commit accepts (%d):\n", len(rep.Changes))
		fmt.Print(lock.FormatChanges(rep.Changes))
	}
	// A re-lock ACCEPTS the current programs as the new baseline, so the reader
	// of this commit is the last person who can notice that one of them was not
	// rebuilt on purpose. Printing the fingerprints here is what makes that
	// reviewable rather than buried in the JSON.
	if len(f.Executables) > 0 {
		fmt.Printf("  oracle program(s) fingerprinted (recorded OUTSIDE the digest):\n")
		for _, e := range f.Executables {
			if e.SHA256 == "" {
				fmt.Printf("    %-24s %s — NOT fingerprinted: %s\n", e.Oracle, e.Cmd, e.Unresolved)
				continue
			}
			fmt.Printf("    %-24s %s (%d bytes)\n      %s\n", e.Oracle, e.Resolved, e.Size, e.SHA256)
		}
	}
	if len(rep.ExecutablesMoved) > 0 {
		fmt.Print(lock.FormatExecutableChanges(rep.ExecutablesMoved))
	}
	fmt.Printf("\nCommit %s together with the change it authorises. What this lock does NOT cover:\n",
		filepath.ToSlash(filepath.Join(".prothesis", lock.FileName)))
	for _, l := range lock.Limitations {
		fmt.Printf("  - %s\n", l)
	}
	return schema.ExitPass
}
