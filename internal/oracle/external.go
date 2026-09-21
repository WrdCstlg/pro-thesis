package oracle

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/WrdCstlg/pro-thesis/internal/driver"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
	"gopkg.in/yaml.v3"
)

// ---------------------------------------------------------------------------
// The oracle definition file
// ---------------------------------------------------------------------------
//
// An external oracle is an EXECUTABLE (directive 4.5): it reads
// prothesis.oracle_input/v1 on stdin, writes prothesis.oracle_output/v1 on
// stdout, and exits 0/1/2. Nothing in that contract says how PRO-THESIS learns
// that the executable exists, what class it belongs to, which lifecycle phases
// it is valid in, or how long it may run. Those four facts have to live
// somewhere, and `oracles.dir` is the only place the directive provides.
//
// ADDITIVE: see OPEN_QUESTIONS.md OQ-029 and DECISIONS.md D-039. It also
// RESOLVES OQ-028, which asked for exactly this format. The directive freezes
// prothesis.yaml,
// prothesis.verdict/v1, prothesis.oracle_input/v1, prothesis.oracle_output/v1,
// the history JSONL, the exit codes and the fault grammar. It does NOT define
// a definition-file format, so this one is designed here, in the same shape as
// the other additive schema identifiers already in the tree
// (recorder.ManifestSchema, recorder.TopologySchema).
//
// Four properties are deliberate:
//
//   - YAML, one flat mapping, one document per file. The lock content-hashes
//     every file in this directory, so the format has to diff cleanly in a pull
//     request: a lock bump is meant to be a REVIEWABLE commit, and a reviewer
//     cannot review a blob.
//   - STRICT decoding. An unknown key is an error, not a default. `valid_phase`
//     for `valid_phases` would otherwise leave the oracle valid nowhere, and an
//     oracle that can never fire sits in the directory looking like coverage.
//   - No `enabled:` flag. Switching an oracle off without deleting it is a
//     gate-weakening move that reads, in a diff, as one character. The way to
//     stop running an oracle is to remove its file, which is unmissable.
//   - No placeholders in `cmd`. Everything an oracle is given arrives on stdin.
//     A `{history_path}` in an oracle command would be passed through
//     literally, and the oracle would open a file called "{history_path}".

// DefinitionSchema is the version string every definition file carries.
//
// It is required rather than optional so that a future format change can be
// detected instead of silently misread: a v2 field decoded by a v1 reader with
// KnownFields would be an error, but a v1 field REMOVED in v2 would silently
// take its zero value, and for `valid_phases` that means "valid nowhere".
const DefinitionSchema = "prothesis.oracle_def/v1"

// DefinitionExtensions are the file extensions Discover treats as definitions.
//
// A distinct extension list is what lets `oracles.dir` also hold the things a
// project legitimately keeps beside its definitions (a README, a checker's
// source, a committed helper script) without either failing to parse them or
// silently ignoring a definition. Everything ending in one of these MUST parse;
// everything else is ignored and reported in Discovery.Ignored.
var DefinitionExtensions = []string{".yaml", ".yml"}

// MaxOracleTimeout caps `timeout:`.
//
// A definition may not grant itself an effectively unbounded run. Without a cap
// the timeout is the easiest way to make a wedged oracle look like a slow one,
// and a run that never returns has no verdict at all, which invariant I7 ("every
// invocation takes a budget and returns a verdict") rules out.
const MaxOracleTimeout = time.Hour

// Definition is one oracle declaration read from `oracles.dir`.
type Definition struct {
	// Version is DefinitionSchema.
	Version string `yaml:"version"`

	// Name is the oracle's identity. It appears verbatim in violation.oracle
	// and in the verdict, and it is what .prothesis/lock addresses.
	Name string `yaml:"name"`

	// Class is one of the eight schema.OracleClass values. It is declared HERE,
	// in the lock-covered file, and never taken from the oracle's own output:
	// severity is derived from the class (schema.DefaultSeverity), so an oracle
	// that could choose its own class could grade its own finding down.
	Class schema.OracleClass `yaml:"class"`

	// ValidPhases is the invariant I5 declaration: the lifecycle phases in
	// which EVALUATING this oracle is meaningful.
	//
	// It is NOT where a finding may be observed. A consistency oracle valid
	// only in ASSERT routinely reports evidence that lies in DRIVE (that is
	// exactly the fixture's stale read (D-031)) and conflating the two fields
	// would report the tool's own marquee finding as an oracle defect (OQ-004).
	ValidPhases []schema.Phase `yaml:"valid_phases"`

	// Cmd is the executable and its arguments, split by the same quote-aware
	// splitter driver.cmd uses. Relative programs resolve against the directory
	// prothesis.yaml lives in.
	Cmd string `yaml:"cmd"`

	// Timeout bounds one evaluation. On expiry the oracle's PROCESS TREE is
	// killed and the finding is INCONCLUSIVE. Required and non-zero: an oracle
	// with no timeout is an oracle that can hang a run forever.
	Timeout schema.Duration `yaml:"timeout"`

	// Path is the file this definition was read from. It is not a YAML field;
	// it exists so every diagnostic can name the file a human has to edit.
	Path string `yaml:"-"`
}

// Source renders the definition's origin for a message.
func (d Definition) Source() string {
	if d.Path == "" {
		return "(definition supplied in memory)"
	}
	return d.Path
}

// Validate checks a decoded definition.
//
// Every rejection here is a defect that would otherwise become a silent
// non-finding: an unnamed oracle cannot be addressed by the lock, an empty
// phase list makes the oracle unrunnable, and a zero timeout makes it
// unstoppable.
func (d Definition) Validate() error {
	var errs schema.ValidationErrors

	if d.Version != DefinitionSchema {
		errs.Add("version", "is %q, want %q", d.Version, DefinitionSchema)
	}
	if strings.TrimSpace(d.Name) == "" {
		errs.Add("name", "missing (the oracle's identity in the verdict and in .prothesis/lock)")
	} else if d.Name != strings.TrimSpace(d.Name) {
		errs.Add("name", "%q has leading or trailing whitespace; the name is compared verbatim "+
			"against the oracle's own output and against the lock", d.Name)
	}
	if !d.Class.Valid() {
		errs.Add("class", "unknown class %q (want one of %v)", d.Class, schema.AllOracleClasses)
	}

	if len(d.ValidPhases) == 0 {
		errs.Add("valid_phases", "missing; an oracle valid in no phase can never be evaluated, "+
			"and one that can never fire is worse than none because it looks like coverage "+
			"(invariant I5)")
	}
	seen := map[schema.Phase]bool{}
	for i, p := range d.ValidPhases {
		if !p.Valid() {
			errs.Add(fmt.Sprintf("valid_phases[%d]", i),
				"unknown phase %q (want one of %v)", p, schema.AllPhases)
			continue
		}
		if seen[p] {
			errs.Add(fmt.Sprintf("valid_phases[%d]", i), "phase %s is declared twice", p)
		}
		seen[p] = true
	}

	switch {
	case strings.TrimSpace(d.Cmd) == "":
		errs.Add("cmd", "missing (the executable to run)")
	default:
		argv, err := driver.SplitCommand(d.Cmd)
		switch {
		case err != nil:
			errs.Add("cmd", "%s", err.Error())
		case len(argv) == 0 || argv[0] == "":
			errs.Add("cmd", "has no program name")
		}
		if left := placeholdersIn(d.Cmd); len(left) > 0 {
			errs.Add("cmd", "contains %s; an oracle receives prothesis.oracle_input/v1 on STDIN "+
				"and nothing is substituted into its command line, so it would be handed that "+
				"text literally", strings.Join(left, ", "))
		}
	}

	switch {
	case d.Timeout <= 0:
		errs.Add("timeout", "missing or not positive; an oracle with no timeout can hang a run "+
			"forever, and a run that never returns has no verdict")
	case d.Timeout.Std() > MaxOracleTimeout:
		errs.Add("timeout", "%s exceeds the %s ceiling", d.Timeout, MaxOracleTimeout)
	}

	return errs.OrNil()
}

// placeholdersIn returns the frozen driver placeholders present in s.
func placeholdersIn(s string) []string {
	known := []string{
		schema.PlaceholderHistoryPath,
		schema.PlaceholderSeed,
		schema.PlaceholderProfile,
		schema.PlaceholderPlanPath,
	}
	var out []string
	for _, p := range known {
		if strings.Contains(s, p) {
			out = append(out, p)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// parsing
// ---------------------------------------------------------------------------

// ParseDefinition decodes one definition file's bytes.
//
// Decoding is STRICT in three ways, and each closes a way for an oracle to
// vanish quietly: an unknown key is rejected, a file carrying no document is
// rejected, and a file carrying a SECOND document is rejected. The last one
// matters because yaml.Decoder reads only the first document, so two oracles in
// one file would register one and drop the other with no error anywhere.
func ParseDefinition(data []byte, path string) (Definition, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)

	var d Definition
	if err := dec.Decode(&d); err != nil {
		if errors.Is(err, io.EOF) {
			return Definition{}, fmt.Errorf("%s: contains no oracle definition (an empty file, or "+
				"comments only); remove the file or write one", path)
		}
		return Definition{}, fmt.Errorf("%s: %w", path, err)
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); err == nil {
		return Definition{}, fmt.Errorf("%s: carries more than one YAML document; one oracle per "+
			"file, or the second one is registered nowhere and reported nowhere", path)
	} else if !errors.Is(err, io.EOF) {
		return Definition{}, fmt.Errorf("%s: %w", path, err)
	}

	d.Path = path
	if err := d.Validate(); err != nil {
		return Definition{}, fmt.Errorf("%s: %w", path, err)
	}
	return d, nil
}

// LoadDefinition reads and parses one definition file.
func LoadDefinition(path string) (Definition, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Definition{}, fmt.Errorf("oracle: read definition: %w", err)
	}
	return ParseDefinition(data, path)
}

// ---------------------------------------------------------------------------
// discovery
// ---------------------------------------------------------------------------

// Discovery is the result of scanning `oracles.dir`.
type Discovery struct {
	// Dir is the directory scanned, absolute where the caller supplied one.
	Dir string

	// Missing reports that the directory does not exist.
	//
	// This is NOT silently equivalent to "no external oracles". A configured
	// directory that is not on disk means either a project that has not run
	// `thesis init` or one whose oracles were removed, and the second is exactly
	// what .prothesis/lock exists to catch: the lock covers every file in this
	// directory, so a directory that lost its contents moves the digest and
	// exits 4. Callers surface this rather than assuming.
	Missing bool

	// Definitions are the parsed definitions, sorted by oracle NAME.
	//
	// Sorted by name rather than by filename so that renaming a file cannot
	// reorder a verdict: findings are produced in registration order and
	// violations are numbered v1, v2, ... in that order, so an unstable order
	// would make two runs of the same world produce different verdicts.
	Definitions []Definition

	// Ignored lists files in the directory that are not definitions, by base
	// name and sorted. A checker binary, a README and a committed helper script
	// all legitimately live here; they are reported so that a definition saved
	// under the wrong extension is visible rather than absent.
	Ignored []string
}

// Names returns the discovered oracle names, in registration order.
func (d *Discovery) Names() []string {
	if d == nil {
		return nil
	}
	out := make([]string, 0, len(d.Definitions))
	for _, def := range d.Definitions {
		out = append(out, def.Name)
	}
	return out
}

// Note renders a one-line human account of the scan, or "" when there is
// nothing worth saying.
func (d *Discovery) Note() string {
	switch {
	case d == nil:
		return ""
	case d.Missing:
		return fmt.Sprintf("oracles.dir %s does not exist, so NO external oracle ran; "+
			"`thesis init` creates it", d.Dir)
	case len(d.Definitions) == 0 && len(d.Ignored) > 0:
		return fmt.Sprintf("oracles.dir %s holds no oracle definition; %d file(s) were ignored "+
			"because they do not end in %s: %s",
			d.Dir, len(d.Ignored), strings.Join(DefinitionExtensions, " or "),
			strings.Join(d.Ignored, ", "))
	case len(d.Definitions) == 0:
		return fmt.Sprintf("oracles.dir %s is empty, so no external oracle ran", d.Dir)
	default:
		return ""
	}
}

// isDefinitionFile reports whether a base name carries a definition extension.
func isDefinitionFile(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	for _, e := range DefinitionExtensions {
		if ext == e {
			return true
		}
	}
	return false
}

// Discover scans dir for oracle definitions.
//
// It is DETERMINISTIC and TOTAL about failure. A definition file that does not
// parse is an ERROR, never a skip: a run that quietly dropped an oracle because
// of a typo would report PASS over a property nobody checked, which is the
// failure this project has already been bitten by twice.
//
// A missing directory is reported through Discovery.Missing rather than as an
// error, because an empty oracle set is a legitimate state (a project running
// built-ins only) and because the enforcement point for "my oracles disappeared"
// is .prothesis/lock, which covers the directory's contents.
func Discover(dir string) (*Discovery, error) {
	d := &Discovery{Dir: dir}
	if dir == "" {
		return d, nil
	}
	if abs, err := filepath.Abs(dir); err == nil {
		d.Dir = abs
	}

	entries, err := os.ReadDir(d.Dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			d.Missing = true
			return d, nil
		}
		return nil, fmt.Errorf("oracle: read oracles.dir %s: %w", d.Dir, err)
	}

	byName := map[string]Definition{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !isDefinitionFile(name) {
			d.Ignored = append(d.Ignored, name)
			continue
		}
		path := filepath.Join(d.Dir, name)
		def, err := LoadDefinition(path)
		if err != nil {
			return nil, fmt.Errorf("oracle: %w", err)
		}
		if prev, dup := byName[def.Name]; dup {
			return nil, fmt.Errorf("oracle: %s and %s both define an oracle named %q; the name is "+
				"the identity in the verdict and in .prothesis/lock and must be unique",
				prev.Source(), def.Source(), def.Name)
		}
		byName[def.Name] = def
		d.Definitions = append(d.Definitions, def)
	}

	sort.Slice(d.Definitions, func(i, j int) bool { return d.Definitions[i].Name < d.Definitions[j].Name })
	sort.Strings(d.Ignored)
	return d, nil
}

// OraclesDir resolves `oracles.dir` against the project directory. An empty
// `oracles.dir` yields "", which Discover treats as "no external oracles".
func OraclesDir(cfg *schema.Config, projectDir string) string {
	if cfg == nil || cfg.Oracles.Dir == "" {
		return ""
	}
	dir := cfg.Oracles.Dir
	if filepath.IsAbs(dir) {
		return dir
	}
	return filepath.Join(projectDir, dir)
}

// ---------------------------------------------------------------------------
// construction
// ---------------------------------------------------------------------------

// ExternalOptions configures the process-backed oracles built from a directory.
type ExternalOptions struct {
	// ProjectDir is the directory prothesis.yaml lives in. Relative programs in
	// `cmd` resolve against it and it is the oracle's working directory.
	ProjectDir string

	// Dir overrides the directory scanned. Empty resolves `oracles.dir` against
	// ProjectDir.
	Dir string

	// MaxStdoutBytes bounds the oracle_output document. Zero means
	// MaxOracleStdoutBytes.
	MaxStdoutBytes int64

	// MaxStderrBytes bounds the captured stderr. Zero means
	// MaxOracleStderrBytes.
	MaxStderrBytes int64

	// Grace is how long a killed oracle may take to exit before its tree is
	// force-killed. Zero means DefaultOracleGrace.
	Grace time.Duration

	// Env is the oracle's environment. Nil inherits the harness's own.
	Env []string
}

// NewExternalOracles discovers and constructs the external oracles for a
// configuration.
func NewExternalOracles(cfg *schema.Config, opts ExternalOptions) ([]Oracle, *Discovery, error) {
	dir := opts.Dir
	if dir == "" {
		dir = OraclesDir(cfg, opts.ProjectDir)
	}
	disc, err := Discover(dir)
	if err != nil {
		return nil, nil, err
	}
	out := make([]Oracle, 0, len(disc.Definitions))
	for _, def := range disc.Definitions {
		o, err := NewProcessOracle(def, opts)
		if err != nil {
			return nil, disc, err
		}
		out = append(out, o)
	}
	return out, disc, nil
}

// NewEngineForConfig returns an engine carrying the configured built-ins AND
// the external oracles discovered under `oracles.dir`.
//
// Both kinds register through the SAME Engine and produce the same Finding, so
// nothing downstream (the verdict, result.json, the exit-code mapping) can
// tell them apart. That is the point: an external oracle's inconclusive has to
// reach exit 2 by exactly the path a built-in's does, or the two would drift
// and only one of them would be trustworthy.
//
// Built-ins register first, in the directive's canonical order; externals
// follow, sorted by name. A name collision between an external oracle and a
// built-in is an ERROR, not a silent override: an external `no_crash` that
// shadowed the built-in would be an unreviewable way to replace a mandatory
// oracle with a permissive one.
func NewEngineForConfig(cfg *schema.Config, opts Options, ext ExternalOptions) (*Engine, *Discovery, error) {
	builtins, err := NewBuiltinsFromConfig(cfg, opts)
	if err != nil {
		return nil, nil, err
	}
	externals, disc, err := NewExternalOracles(cfg, ext)
	if err != nil {
		return nil, disc, err
	}

	e := NewEngine()
	if err := e.RegisterAll(builtins); err != nil {
		return nil, disc, err
	}
	for _, o := range externals {
		if err := e.Register(o); err != nil {
			return nil, disc, fmt.Errorf("oracle: registering external oracle %q: %w", o.Name(), err)
		}
	}
	return e, disc, nil
}
