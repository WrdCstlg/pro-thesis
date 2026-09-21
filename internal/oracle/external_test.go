package oracle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// ===========================================================================
// The definition file
// ===========================================================================

func validDefinitionYAML() string {
	return "version: " + DefinitionSchema + "\n" +
		"name: linearizable.kv\n" +
		"class: consistency\n" +
		"valid_phases: [ASSERT]\n" +
		"cmd: \"./bin/checker --budget-ms 30000\"\n" +
		"timeout: 120s\n"
}

func TestParseDefinitionAcceptsAWellFormedFile(t *testing.T) {
	d, err := ParseDefinition([]byte(validDefinitionYAML()), "x.yaml")
	if err != nil {
		t.Fatalf("a well-formed definition was rejected: %v", err)
	}
	if d.Name != "linearizable.kv" {
		t.Fatalf("name = %q", d.Name)
	}
	if d.Class != schema.ClassConsistency {
		t.Fatalf("class = %q", d.Class)
	}
	if len(d.ValidPhases) != 1 || d.ValidPhases[0] != schema.PhaseAssert {
		t.Fatalf("valid_phases = %v", d.ValidPhases)
	}
	if d.Timeout.Std() != 2*time.Minute {
		t.Fatalf("timeout = %s", d.Timeout)
	}
	if d.Path != "x.yaml" {
		t.Fatalf("Path = %q; every diagnostic has to be able to name the file to edit", d.Path)
	}
}

// Every rejection here is a way an oracle could otherwise end up registered but
// unable to fire, or registered under a declaration nobody reviewed.
func TestParseDefinitionRejects(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
		why  string
	}{
		{
			name: "no version",
			body: "name: a\nclass: crash\nvalid_phases: [ASSERT]\ncmd: x\ntimeout: 1s\n",
			want: "version",
			why: "without a version a future format change is silently misread, and a field " +
				"REMOVED in v2 takes its zero value — which for valid_phases means valid nowhere",
		},
		{
			name: "wrong version",
			body: "version: prothesis.oracle_def/v2\nname: a\nclass: crash\nvalid_phases: [ASSERT]\ncmd: x\ntimeout: 1s\n",
			want: "version",
			why:  "a version this build does not understand must be refused, not guessed at",
		},
		{
			name: "no name",
			body: "version: " + DefinitionSchema + "\nclass: crash\nvalid_phases: [ASSERT]\ncmd: x\ntimeout: 1s\n",
			want: "name",
			why:  "the name is the identity the verdict and .prothesis/lock address",
		},
		{
			name: "unknown class",
			body: "version: " + DefinitionSchema + "\nname: a\nclass: vibes\nvalid_phases: [ASSERT]\ncmd: x\ntimeout: 1s\n",
			want: "class",
			why:  "severity is derived from the class; an unknown one has no severity",
		},
		{
			name: "no valid_phases",
			body: "version: " + DefinitionSchema + "\nname: a\nclass: crash\ncmd: x\ntimeout: 1s\n",
			want: "valid_phases",
			why: "an oracle valid in no phase can never be evaluated, and one that can never " +
				"fire is worse than none because it sits in the directory looking like coverage",
		},
		{
			name: "empty valid_phases",
			body: "version: " + DefinitionSchema + "\nname: a\nclass: crash\nvalid_phases: []\ncmd: x\ntimeout: 1s\n",
			want: "valid_phases",
			why:  "an explicitly empty list is the same unrunnable oracle as a missing one",
		},
		{
			name: "unknown phase",
			body: "version: " + DefinitionSchema + "\nname: a\nclass: crash\nvalid_phases: [ASSERTT]\ncmd: x\ntimeout: 1s\n",
			want: "phase",
			why:  "a typo'd phase would leave the oracle valid nowhere",
		},
		{
			name: "duplicate phase",
			body: "version: " + DefinitionSchema + "\nname: a\nclass: crash\nvalid_phases: [ASSERT, ASSERT]\ncmd: x\ntimeout: 1s\n",
			want: "twice",
			why:  "a duplicated phase means the author meant to write two different ones",
		},
		{
			name: "no cmd",
			body: "version: " + DefinitionSchema + "\nname: a\nclass: crash\nvalid_phases: [ASSERT]\ntimeout: 1s\n",
			want: "cmd",
			why:  "there is nothing to run",
		},
		{
			name: "cmd with an unterminated quote",
			body: "version: " + DefinitionSchema + "\nname: a\nclass: crash\nvalid_phases: [ASSERT]\ncmd: \"'./bin/x\"\ntimeout: 1s\n",
			want: "unterminated",
			why:  "silently closing the quote runs a different command than the file says",
		},
		{
			name: "cmd carrying a driver placeholder",
			body: "version: " + DefinitionSchema + "\nname: a\nclass: crash\nvalid_phases: [ASSERT]\ncmd: \"./x --history {history_path}\"\ntimeout: 1s\n",
			want: "STDIN",
			why: "nothing is substituted into an oracle's command line, so the oracle would " +
				"open a file literally called {history_path}",
		},
		{
			name: "no timeout",
			body: "version: " + DefinitionSchema + "\nname: a\nclass: crash\nvalid_phases: [ASSERT]\ncmd: x\n",
			want: "timeout",
			why:  "an oracle with no timeout can hang a run forever",
		},
		{
			name: "zero timeout",
			body: "version: " + DefinitionSchema + "\nname: a\nclass: crash\nvalid_phases: [ASSERT]\ncmd: x\ntimeout: 0s\n",
			want: "timeout",
			why:  "zero is not a bound",
		},
		{
			name: "timeout beyond the ceiling",
			body: "version: " + DefinitionSchema + "\nname: a\nclass: crash\nvalid_phases: [ASSERT]\ncmd: x\ntimeout: 25h\n",
			want: "ceiling",
			why: "a definition may not grant itself an effectively unbounded run; that is how a " +
				"wedged oracle is made to look like a slow one",
		},
		{
			name: "unknown key",
			body: validDefinitionYAML() + "valid_phase: [ASSERT]\n",
			want: "valid_phase",
			why: "a near-miss key silently takes a zero value; for valid_phases that means the " +
				"oracle is valid nowhere and never fires",
		},
		{
			name: "empty file",
			body: "# only a comment\n",
			want: "contains no oracle definition",
			why:  "a file that declares nothing must be reported, not skipped",
		},
		{
			name: "two documents in one file",
			body: validDefinitionYAML() + "---\n" + validDefinitionYAML(),
			want: "more than one YAML document",
			why: "yaml.Decoder reads only the first document, so the second oracle would be " +
				"registered nowhere and reported nowhere",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseDefinition([]byte(tc.body), "x.yaml")
			if err == nil {
				t.Fatalf("accepted:\n%s\nwhy this matters: %s", tc.body, tc.why)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error does not mention %q: %v\nwhy this matters: %s", tc.want, err, tc.why)
			}
		})
	}
}

// ===========================================================================
// Discovery
// ===========================================================================

// definitionYAML renders a definition file.
//
// `cmd` is written as a LITERAL BLOCK SCALAR. It has to be: on this host a
// command carries Windows paths, and YAML's double-quoted style reads backslash
// as an escape, so `C:\AI Projects` would either fail to parse or silently
// become something else.
func definitionYAML(name, cmd, timeout string) string {
	return "version: " + DefinitionSchema + "\n" +
		"name: " + name + "\n" +
		"class: consistency\n" +
		"valid_phases: [ASSERT]\n" +
		"cmd: |-\n  " + cmd + "\n" +
		"timeout: " + timeout + "\n"
}

func writeDef(t *testing.T, dir, file, name string) {
	t.Helper()
	body := definitionYAML(name, "./bin/"+name, "30s")
	if err := os.WriteFile(filepath.Join(dir, file), []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", file, err)
	}
}

// Registration order decides evaluation order, which decides the v1/v2/...
// numbering of violations in the verdict. An order that came from a map, or
// from the filesystem's whim, would make two runs of the same world produce
// different verdicts.
func TestDiscoverIsSortedAndDeterministic(t *testing.T) {
	dir := t.TempDir()
	writeDef(t, dir, "zzz.yaml", "aaa.check")
	writeDef(t, dir, "aaa.yaml", "zzz.check")
	writeDef(t, dir, "mmm.yml", "mmm.check")

	var first []string
	for i := 0; i < 8; i++ {
		d, err := Discover(dir)
		if err != nil {
			t.Fatalf("Discover: %v", err)
		}
		got := d.Names()
		want := []string{"aaa.check", "mmm.check", "zzz.check"}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("iteration %d: names = %v, want %v (sorted by oracle NAME, so renaming a "+
				"file cannot renumber a verdict's violations)", i, got, want)
		}
		if first == nil {
			first = got
		} else if strings.Join(got, ",") != strings.Join(first, ",") {
			t.Fatalf("discovery order changed between runs: %v then %v", first, got)
		}
	}
}

func TestDiscoverReportsAMissingDirectoryRatherThanPretendingItIsEmpty(t *testing.T) {
	d, err := Discover(filepath.Join(t.TempDir(), "nope"))
	if err != nil {
		t.Fatalf("Discover on a missing dir returned an error: %v", err)
	}
	if !d.Missing {
		t.Fatalf("a missing oracles.dir was not flagged; 'the directory is gone' and 'there are " +
			"no external oracles' are different facts and only one of them supports a PASS")
	}
	if d.Note() == "" {
		t.Fatalf("a missing oracles.dir produced no note for the operator")
	}
}

// A definition that does not parse must FAIL the run. Skipping it would mean an
// oracle silently vanished and the gate went green over a property nobody
// checked: the exact class of bug this project has been bitten by twice.
func TestDiscoverFailsOnAnUnparseableDefinitionRatherThanSkippingIt(t *testing.T) {
	dir := t.TempDir()
	writeDef(t, dir, "good.yaml", "good.check")
	if err := os.WriteFile(filepath.Join(dir, "bad.yaml"), []byte("class: [not, a, class\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	d, err := Discover(dir)
	if err == nil {
		t.Fatalf("a broken definition was skipped; discovery returned %v", d.Names())
	}
	if !strings.Contains(err.Error(), "bad.yaml") {
		t.Fatalf("the error does not name the file to fix: %v", err)
	}
}

func TestDiscoverRejectsDuplicateOracleNames(t *testing.T) {
	dir := t.TempDir()
	writeDef(t, dir, "a.yaml", "same.name")
	writeDef(t, dir, "b.yaml", "same.name")
	if _, err := Discover(dir); err == nil {
		t.Fatalf("two definitions claiming one name were accepted; the name is the identity in " +
			"the verdict and in .prothesis/lock and cannot be ambiguous")
	}
}

// The directory legitimately holds a README, a checker's source, and (once the
// lock exists) whatever else a project commits beside its definitions.
func TestDiscoverIgnoresNonDefinitionFilesAndSaysSo(t *testing.T) {
	dir := t.TempDir()
	writeDef(t, dir, "a.yaml", "a.check")
	for _, f := range []string{"README.md", "checker.go", "linearizable.kv.yaml.example"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o644); err != nil {
			t.Fatalf("write %s: %v", f, err)
		}
	}
	d, err := Discover(dir)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(d.Definitions) != 1 {
		t.Fatalf("definitions = %v, want just a.check", d.Names())
	}
	if len(d.Ignored) != 3 {
		t.Fatalf("ignored = %v, want the three non-definition files; they are reported so that a "+
			"definition saved under the wrong extension is visible rather than absent", d.Ignored)
	}
}

// ===========================================================================
// The scaffold
// ===========================================================================

func TestScaffoldWritesAnExampleThatIsItselfAValidDefinition(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "oracles")
	written, err := Scaffold(dir)
	if err != nil {
		t.Fatalf("Scaffold: %v", err)
	}
	if len(written) != 2 {
		t.Fatalf("Scaffold wrote %v, want the example and the README", written)
	}

	body, err := os.ReadFile(filepath.Join(dir, ExampleDefinitionFileName))
	if err != nil {
		t.Fatalf("read example: %v", err)
	}
	d, err := ParseDefinition(body, ExampleDefinitionFileName)
	if err != nil {
		t.Fatalf("the scaffolded example is not a definition this tool accepts: %v\n"+
			"a scaffold its own parser rejects is worse than none", err)
	}
	if d.Name != "linearizable.kv" {
		t.Fatalf("the scaffolded example is named %q, want linearizable.kv", d.Name)
	}
	if d.Class != schema.ClassConsistency {
		t.Fatalf("the scaffolded example's class is %q", d.Class)
	}

	// It must be INERT. A live definition naming a checker that has not been
	// built would make every run of a brand-new project exit 2 forever.
	disc, err := Discover(dir)
	if err != nil {
		t.Fatalf("Discover over the scaffold: %v", err)
	}
	if len(disc.Definitions) != 0 {
		t.Fatalf("the scaffolded example is LIVE (%v); a new project would report an oracle it "+
			"never configured, pointing at an executable that does not exist", disc.Names())
	}

	// ...and the documented activation step, one rename, must actually work,
	// or the scaffold is a dead end rather than a starting point.
	if err := os.WriteFile(filepath.Join(dir, "linearizable.kv.yaml"), body, 0o644); err != nil {
		t.Fatalf("activate: %v", err)
	}
	disc, err = Discover(dir)
	if err != nil {
		t.Fatalf("Discover after activation: %v", err)
	}
	if len(disc.Definitions) != 1 || disc.Names()[0] != "linearizable.kv" {
		t.Fatalf("renaming the example to *.yaml did not register it: %v; the README tells a user "+
			"that one rename activates it", disc.Names())
	}
	if _, err := NewProcessOracle(disc.Definitions[0], ExternalOptions{ProjectDir: dir}); err != nil {
		t.Fatalf("the activated definition does not construct an oracle: %v", err)
	}
}

func TestScaffoldNeverOverwrites(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "oracles")
	if _, err := Scaffold(dir); err != nil {
		t.Fatalf("Scaffold: %v", err)
	}
	path := filepath.Join(dir, ExampleDefinitionFileName)
	if err := os.WriteFile(path, []byte("edited by a human\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	written, err := Scaffold(dir)
	if err != nil {
		t.Fatalf("second Scaffold: %v", err)
	}
	if len(written) != 0 {
		t.Fatalf("Scaffold rewrote %v; clobbering a file here silently moves the lock digest, and "+
			"a scaffold that can rewrite a reviewed oracle is a gate-weakening tool", written)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "edited by a human\n" {
		t.Fatalf("the human's file was overwritten")
	}
}

// ===========================================================================
// Process execution: the fake oracle is this test binary, re-entered
// ===========================================================================

// helperCmd builds a definition `cmd` that re-executes this test binary as the
// oracle. Single quotes because both the temp directory and this repo's own
// project directory contain spaces.
func helperCmd(t *testing.T, args ...string) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	if strings.Contains(exe, "'") {
		t.Fatalf("the test binary path contains a single quote and cannot be quoted: %q", exe)
	}
	var b strings.Builder
	b.WriteString("'" + exe + "' -test.run=^TestOracleHelperProcess$ --")
	for _, a := range args {
		if strings.Contains(a, "'") {
			t.Fatalf("helper argument contains a single quote: %q", a)
		}
		b.WriteString(" '" + a + "'")
	}
	return b.String()
}

const testOracleName = "linearizable.kv"

func helperDef(t *testing.T, cmd string, tweak ...func(*Definition)) Definition {
	t.Helper()
	d := Definition{
		Version:     DefinitionSchema,
		Name:        testOracleName,
		Class:       schema.ClassConsistency,
		ValidPhases: []schema.Phase{schema.PhaseAssert},
		Cmd:         cmd,
		Timeout:     schema.Duration(20 * time.Second),
		Path:        filepath.Join("oracles", "linearizable.kv.yaml"),
	}
	for _, f := range tweak {
		f(&d)
	}
	return d
}

// testInput builds an Input whose oracle_input document is valid and whose
// phase windows are the ones the fixture actually produces, so a DRIVE-observed
// finding from an ASSERT-only oracle is expressible.
func testInput(t *testing.T) *Input {
	t.Helper()
	dir := t.TempDir()
	hist := filepath.Join(dir, "history.jsonl")
	if err := os.WriteFile(hist, []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("write history: %v", err)
	}
	return &Input{
		RunID:          "r_2026_09_08_test",
		WorldPath:      filepath.Join(dir, "world.thesis"),
		HistoryPath:    hist,
		FinalStatePath: filepath.Join(dir, "final_state.json"),
		TelemetryPath:  filepath.Join(dir, "telemetry.jsonl"),
		ArtifactDir:    dir,
		Phases: schema.PhaseTimings{
			{Phase: schema.PhaseBoot, StartMS: -8000, EndMS: -3000},
			{Phase: schema.PhaseSeed, StartMS: -3000, EndMS: 0},
			{Phase: schema.PhaseDrive, StartMS: 0, EndMS: 18000},
			{Phase: schema.PhasePerturb, StartMS: 8200, EndMS: 13500},
			{Phase: schema.PhaseHeal, StartMS: 18000, EndMS: 18500},
			{Phase: schema.PhaseQuiesce, StartMS: 18500, EndMS: 24500},
			{Phase: schema.PhaseAssert, StartMS: 24500, EndMS: 25000},
		},
	}
}

// docFile writes an oracle_output document and returns its path.
func docFile(t *testing.T, out schema.OracleOutput) string {
	t.Helper()
	b, err := schema.MarshalOracleOutput(&out)
	if err != nil {
		t.Fatalf("marshal oracle output: %v", err)
	}
	return rawFile(t, string(b))
}

// rawFile writes arbitrary bytes for the helper to print verbatim.
func rawFile(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "out.json")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("write doc: %v", err)
	}
	return p
}

func evaluate(t *testing.T, d Definition, in *Input, opts ExternalOptions) Result {
	t.Helper()
	o, err := NewProcessOracle(d, opts)
	if err != nil {
		t.Fatalf("NewProcessOracle: %v", err)
	}
	res, err := o.Evaluate(context.Background(), schema.PhaseAssert, in)
	if err != nil {
		t.Fatalf("Evaluate returned an engine error, which would abort the whole ASSERT phase "+
			"instead of recording one oracle's failure: %v", err)
	}
	return res
}

func okDoc() schema.OracleOutput {
	return schema.OracleOutput{
		Schema:      schema.OracleOutputSchema,
		Oracle:      testOracleName,
		Class:       schema.ClassConsistency,
		ValidPhases: []schema.Phase{schema.PhaseAssert},
		Status:      schema.StatusOK,
		Explanation: "every key's subhistory linearized",
	}
}

// ---------------------------------------------------------------------------
// The three clean answers
// ---------------------------------------------------------------------------

func TestExternalOracleReportsOK(t *testing.T) {
	in := testInput(t)
	d := helperDef(t, helperCmd(t, "emit", "0", docFile(t, okDoc())))
	res := evaluate(t, d, in, ExternalOptions{})

	if res.Status != schema.StatusOK {
		t.Fatalf("status = %q (%s), want ok", res.Status, res.Explanation)
	}
	if res.Explanation != "every key's subhistory linearized" {
		t.Fatalf("the oracle's own explanation did not survive: %q", res.Explanation)
	}
}

func TestExternalOracleReportsAViolationWithItsWitness(t *testing.T) {
	in := testInput(t)
	out := okDoc()
	out.Status = schema.StatusViolated
	out.Explanation = "process 1 read k/42=7 from kv-n2 under a stale read lease"
	out.Witness = schema.Witness{
		OpIDs: []int64{90002, 90117},
		Key:   "k/42",
		Extra: map[string]json.RawMessage{
			WitnessFirstSeenMS: json.RawMessage("11084"),
		},
	}
	d := helperDef(t, helperCmd(t, "emit", "1", docFile(t, out)))
	res := evaluate(t, d, in, ExternalOptions{})

	if res.Status != schema.StatusViolated {
		t.Fatalf("status = %q (%s), want violated", res.Status, res.Explanation)
	}
	if len(res.Witness.OpIDs) != 2 || res.Witness.OpIDs[0] != 90002 {
		t.Fatalf("witness op_ids = %v; the witness is what makes a violation checkable", res.Witness.OpIDs)
	}
	if res.Witness.Key != "k/42" {
		t.Fatalf("witness key = %q", res.Witness.Key)
	}
	if res.FirstSeenMS != 11084 {
		t.Fatalf("first_seen_ms = %d, want 11084", res.FirstSeenMS)
	}
}

func TestExternalOracleReportsInconclusive(t *testing.T) {
	in := testInput(t)
	out := okDoc()
	out.Status = schema.StatusInconclusive
	out.Explanation = "budget exhausted after 412000 states on key k/17"
	d := helperDef(t, helperCmd(t, "emit", "2", docFile(t, out)))
	res := evaluate(t, d, in, ExternalOptions{})

	if res.Status != schema.StatusInconclusive {
		t.Fatalf("status = %q, want inconclusive", res.Status)
	}
	if !strings.Contains(res.Explanation, "budget exhausted") {
		t.Fatalf("the oracle's reason did not survive: %q", res.Explanation)
	}
}

// The stdin half of the contract: the oracle must actually receive a valid
// prothesis.oracle_input/v1 naming this world's artifacts.
func TestExternalOracleReceivesTheOracleInputOnStdin(t *testing.T) {
	in := testInput(t)
	d := helperDef(t, helperCmd(t, "echostdin"))
	res := evaluate(t, d, in, ExternalOptions{})

	if res.Status != schema.StatusOK {
		t.Fatalf("status = %q (%s); the helper reports ok only when stdin decoded as %s",
			res.Status, res.Explanation, schema.OracleInputSchema)
	}
	if res.Witness.Key != in.HistoryPath {
		t.Fatalf("the oracle was handed history_path %q, want %q", res.Witness.Key, in.HistoryPath)
	}
}

// ---------------------------------------------------------------------------
// The failure modes. NONE of them may produce ok.
// ---------------------------------------------------------------------------

func TestExternalOracleFailureModesAreNeverOK(t *testing.T) {
	violatedDoc := func() schema.OracleOutput {
		o := okDoc()
		o.Status = schema.StatusViolated
		o.Explanation = "a violation"
		return o
	}

	tests := []struct {
		name string
		args func(t *testing.T) []string
		want string
		opts ExternalOptions
		why  string
	}{
		{
			name: "the executable does not exist",
			args: func(*testing.T) []string { return nil },
			want: "could not be started",
			why: "a definition naming a checker this machine does not have has checked " +
				"nothing, and a run that reported PASS over it would be lying",
		},
		{
			name: "crash: a non-zero exit outside the contract",
			args: func(t *testing.T) []string { return []string{"exit", "42"} },
			want: "outside the 0/1/2 contract",
			why:  "42 has no defined meaning; reading it as anything but inconclusive is invention",
		},
		{
			// No substring is asserted: a runtime crash's exit status and any
			// bytes it leaves on stdout are the runtime's business, so the
			// finding may land on "wrote nothing", "not a document" or "outside
			// the 0/1/2 contract" depending on the platform. The property under
			// test is the one that matters and it holds in all three: NOT ok.
			name: "crash: the runtime tears the oracle down",
			args: func(t *testing.T) []string { return []string{"panic"} },
			want: "",
			why:  "a crashed oracle produces no verdict, so it has concluded nothing",
		},
		{
			name: "exit 0 with no output at all",
			args: func(t *testing.T) []string { return []string{"exit", "0"} },
			want: "wrote nothing to stdout",
			why: "an exit code alone is not a finding; a silent success is exactly what a " +
				"broken oracle looks like, and it must not read as ok",
		},
		{
			name: "malformed stdout",
			args: func(t *testing.T) []string {
				return []string{"emit", "0", rawFile(t, "{\"schema\": \"prothesis.oracle_out")}
			},
			want: "not a prothesis.oracle_output/v1 document",
			why:  "a truncated document may have been cut off before the status that mattered",
		},
		{
			name: "stdout carrying the wrong schema id",
			args: func(t *testing.T) []string {
				return []string{"emit", "0", rawFile(t, `{"schema":"prothesis.oracle_output/v2","status":"ok"}`)}
			},
			want: "not a prothesis.oracle_output/v1 document",
			why:  "a document this build cannot claim to understand must not be believed",
		},
		{
			name: "a document with no status",
			args: func(t *testing.T) []string {
				return []string{"emit", "0", rawFile(t, `{"schema":"prothesis.oracle_output/v1","explanation":"hi"}`)}
			},
			want: "no status",
			why:  "an absent status is not an ok status",
		},
		{
			name: "disagreement: says ok, exits 1",
			args: func(t *testing.T) []string { return []string{"emit", "1", docFile(t, okDoc())} },
			want: "disagrees with itself",
			why: "the two channels are independent evidence; adopting the stricter one would " +
				"manufacture a violation out of a bug in the oracle, and a false positive is " +
				"the one failure that destroys the product",
		},
		{
			name: "disagreement: says violated, exits 0",
			args: func(t *testing.T) []string { return []string{"emit", "0", docFile(t, violatedDoc())} },
			want: "disagrees with itself",
			why:  "an oracle that prints violated and exits 0 must not pass either",
		},
		{
			name: "disagreement: says inconclusive, exits 0",
			args: func(t *testing.T) []string {
				o := okDoc()
				o.Status = schema.StatusInconclusive
				return []string{"emit", "0", docFile(t, o)}
			},
			want: "disagrees with itself",
			why:  "the channel that says 'I could not check' must never be overruled by exit 0",
		},
		{
			name: "stdout beyond the bound",
			args: func(t *testing.T) []string { return []string{"flood", "200000", "0"} },
			opts: ExternalOptions{MaxStdoutBytes: 4096, Grace: 200 * time.Millisecond},
			want: "wrote more than 4096 bytes to stdout",
			why: "an unbounded document would take the harness's memory with it, and the " +
				"harness holds the run's only record of what happened",
		},
		{
			name: "a violation reported outside the declared valid phases",
			args: func(t *testing.T) []string {
				o := violatedDoc()
				o.ValidPhases = []schema.Phase{schema.PhaseDrive}
				return []string{"emit", "1", docFile(t, o)}
			},
			want: "valid_phases",
			why: "the executable disagrees with the lock-covered declaration it was registered " +
				"under, so it is either the wrong binary or a drifted one and neither its " +
				"violations nor its passes can be believed",
		},
		{
			name: "an ok reported outside the declared valid phases",
			args: func(t *testing.T) []string {
				o := okDoc()
				o.ValidPhases = []schema.Phase{schema.PhaseQuiesce}
				return []string{"emit", "0", docFile(t, o)}
			},
			want: "valid_phases",
			why: "the declaration check is not violation-only; a PASS from an oracle whose " +
				"declaration drifted is a vacuous pass",
		},
		{
			name: "the wrong executable, identifying itself by another name",
			args: func(t *testing.T) []string {
				o := okDoc()
				o.Oracle = "some.other.oracle"
				return []string{"emit", "0", docFile(t, o)}
			},
			want: "identifies itself as",
			why:  "the definition and the binary must be the pair the lock reviewed",
		},
		{
			name: "a class the definition did not declare",
			args: func(t *testing.T) []string {
				o := okDoc()
				o.Class = schema.ClassResource
				return []string{"emit", "0", docFile(t, o)}
			},
			want: "reports class",
			why: "severity is derived from the declared class, so an oracle reporting its own " +
				"would be grading its own finding",
		},
		{
			name: "witness.first_seen_ms that is not an integer",
			args: func(t *testing.T) []string {
				o := violatedDoc()
				o.Witness = schema.Witness{Extra: map[string]json.RawMessage{
					WitnessFirstSeenMS: json.RawMessage(`"soon"`),
				}}
				return []string{"emit", "1", docFile(t, o)}
			},
			want: "first_seen_ms",
			why:  "a malformed placement would put the violation at a fabricated point on the timeline",
		},
		{
			name: "witness.phase that is not a lifecycle phase",
			args: func(t *testing.T) []string {
				o := violatedDoc()
				o.Witness = schema.Witness{Extra: map[string]json.RawMessage{
					WitnessPhase: json.RawMessage(`"SETTLING"`),
				}}
				return []string{"emit", "1", docFile(t, o)}
			},
			want: "eight lifecycle phases",
			why:  "an unrecognised phase silently treated as a known one corrupts I5 reasoning",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := testInput(t)
			var cmd string
			if args := tc.args(t); len(args) == 0 {
				cmd = "./no-such-oracle-executable-anywhere"
			} else {
				cmd = helperCmd(t, args...)
			}
			res := evaluate(t, helperDef(t, cmd), in, tc.opts)

			if res.Status == schema.StatusOK {
				t.Fatalf("THIS FAILURE MODE PRODUCED ok.\nexplanation: %q\nwhy this matters: %s",
					res.Explanation, tc.why)
			}
			if res.Status != schema.StatusInconclusive {
				t.Fatalf("status = %q, want inconclusive\nexplanation: %q\nwhy this matters: %s",
					res.Status, res.Explanation, tc.why)
			}
			if res.Explanation == "" {
				t.Fatalf("inconclusive with no reason; exit 2 means 'retry once then escalate to " +
					"a human', and a human cannot escalate a verdict that does not say why")
			}
			if tc.want != "" && !strings.Contains(res.Explanation, tc.want) {
				t.Fatalf("the explanation does not say %q:\n%s\nwhy this matters: %s",
					tc.want, res.Explanation, tc.why)
			}
		})
	}
}

// A disagreement must name BOTH readings. "Never silently trust one" is only
// checkable by a human if the message says what each channel said.
func TestDisagreementNamesBothChannels(t *testing.T) {
	in := testInput(t)
	d := helperDef(t, helperCmd(t, "emit", "1", docFile(t, okDoc())))
	res := evaluate(t, d, in, ExternalOptions{})

	for _, want := range []string{`"ok"`, "exited 1", `"violated"`} {
		if !strings.Contains(res.Explanation, want) {
			t.Fatalf("the disagreement message does not contain %s:\n%s", want, res.Explanation)
		}
	}
}

// A hang is the failure mode most likely to look like success from the outside:
// it produces no error, no output and no exit code. It must be bounded, the
// PROCESS TREE must die, and the finding must be inconclusive.
func TestExternalOracleThatHangsIsKilledAndIsInconclusive(t *testing.T) {
	in := testInput(t)
	d := helperDef(t, helperCmd(t, "hang", "30000"), func(d *Definition) {
		d.Timeout = schema.Duration(400 * time.Millisecond)
	})

	start := time.Now()
	res := evaluate(t, d, in, ExternalOptions{Grace: 400 * time.Millisecond})
	elapsed := time.Since(start)

	if res.Status != schema.StatusInconclusive {
		t.Fatalf("status = %q (%s), want inconclusive", res.Status, res.Explanation)
	}
	if !strings.Contains(res.Explanation, "timeout") {
		t.Fatalf("the explanation does not say it timed out: %s", res.Explanation)
	}
	if strings.Contains(res.Explanation, "process tree FAILED") {
		t.Fatalf("the oracle's process tree survived: %s; a surviving oracle holds whatever it "+
			"opened and can corrupt the next world", res.Explanation)
	}
	if elapsed > 25*time.Second {
		t.Fatalf("a 400ms timeout took %s to enforce; the budget was not enforced at all", elapsed)
	}
}

// An oracle that outlives its timeout by ignoring the polite signal must still
// die. If the escalation were missing this test would hang until the suite
// times out, which is exactly what a wedged oracle would do to a real run.
func TestExternalOracleThatIgnoresSIGTERMIsStillKilled(t *testing.T) {
	in := testInput(t)
	d := helperDef(t, helperCmd(t, "ignoreterm"), func(d *Definition) {
		d.Timeout = schema.Duration(300 * time.Millisecond)
	})

	start := time.Now()
	res := evaluate(t, d, in, ExternalOptions{Grace: 600 * time.Millisecond})
	elapsed := time.Since(start)

	if res.Status != schema.StatusInconclusive {
		t.Fatalf("status = %q, want inconclusive", res.Status)
	}
	if elapsed > 25*time.Second {
		t.Fatalf("killing an oracle that ignores SIGTERM took %s; one wedged oracle would stall "+
			"the whole search", elapsed)
	}
}

// Cancelling the run is not a finding about the system under test.
func TestExternalOracleUnderACancelledContextIsInconclusiveNotViolated(t *testing.T) {
	in := testInput(t)
	d := helperDef(t, helperCmd(t, "hang", "30000"))
	o, err := NewProcessOracle(d, ExternalOptions{Grace: 400 * time.Millisecond})
	if err != nil {
		t.Fatalf("NewProcessOracle: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(250 * time.Millisecond)
		cancel()
	}()
	defer cancel()

	res, err := o.Evaluate(ctx, schema.PhaseAssert, in)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if res.Status != schema.StatusInconclusive {
		t.Fatalf("status = %q, want inconclusive", res.Status)
	}
	if !strings.Contains(res.Explanation, "cancelled") {
		t.Fatalf("a cancellation was not reported as one: %s", res.Explanation)
	}
}

// ---------------------------------------------------------------------------
// violations[].phase and oracle.valid_phases are DIFFERENT fields (OQ-004)
// ---------------------------------------------------------------------------

// The fixture's marquee finding: an ASSERT-only consistency oracle reporting a
// stale read whose evidence lies in DRIVE. If the observed phase were checked
// against valid_phases, the tool's own definition-of-done finding would be
// thrown away as an oracle defect.
func TestAViolationObservedOutsideValidPhasesIsNotADefect(t *testing.T) {
	in := testInput(t)
	out := okDoc()
	out.Status = schema.StatusViolated
	out.Explanation = "stale read at t+11084ms"
	out.Witness = schema.Witness{
		OpIDs: []int64{90002},
		Key:   "k/42",
		Extra: map[string]json.RawMessage{
			WitnessFirstSeenMS: json.RawMessage("11084"),
		},
	}
	d := helperDef(t, helperCmd(t, "emit", "1", docFile(t, out)))

	o, err := NewProcessOracle(d, ExternalOptions{})
	if err != nil {
		t.Fatalf("NewProcessOracle: %v", err)
	}
	e := NewEngine()
	if err := e.Register(o); err != nil {
		t.Fatalf("Register: %v", err)
	}
	f, err := e.EvaluateOracleAt(context.Background(), o, schema.PhaseAssert, in)
	if err != nil {
		t.Fatalf("EvaluateOracleAt: %v", err)
	}

	if f.Result.Status != schema.StatusViolated {
		t.Fatalf("status = %q (%s), want violated.\nAn ASSERT-only oracle is SUPPOSED to report "+
			"evidence that lies in DRIVE — that is what a stale read is. Rejecting it conflates "+
			"violations[].phase with oracle.valid_phases (OQ-004).",
			f.Result.Status, f.Result.Explanation)
	}
	// 11084 ms falls inside both DRIVE (0..18000) and PERTURB (8200..13500);
	// the engine takes the earliest in lifecycle order, as directive 4.6 does.
	if f.Result.Phase != schema.PhaseDrive {
		t.Fatalf("observed phase = %q, want DRIVE", f.Result.Phase)
	}
	if len(f.ValidPhases) != 1 || f.ValidPhases[0] != schema.PhaseAssert {
		t.Fatalf("the finding's valid_phases = %v, want [ASSERT]; the declaration must survive "+
			"alongside the observation, not be replaced by it", f.ValidPhases)
	}
	v := f.ToViolation("v1")
	if v.Phase != schema.PhaseDrive {
		t.Fatalf("the verdict's violation phase = %q, want DRIVE", v.Phase)
	}
	if v.Severity != schema.DefaultSeverity(schema.ClassConsistency) {
		t.Fatalf("severity = %q; it must come from the DECLARED class, never from the oracle",
			v.Severity)
	}
}

// An oracle that pins the observed phase directly, for evidence with no
// timestamp, is also not reporting a defect.
func TestAnExplicitWitnessPhaseOutsideValidPhasesIsAccepted(t *testing.T) {
	in := testInput(t)
	out := okDoc()
	out.Status = schema.StatusViolated
	out.Explanation = "divergent replica state"
	out.Witness = schema.Witness{Extra: map[string]json.RawMessage{
		WitnessPhase: json.RawMessage(`"PERTURB"`),
	}}
	d := helperDef(t, helperCmd(t, "emit", "1", docFile(t, out)))
	res := evaluate(t, d, in, ExternalOptions{})

	if res.Status != schema.StatusViolated {
		t.Fatalf("status = %q (%s), want violated", res.Status, res.Explanation)
	}
	if res.Phase != schema.PhasePerturb {
		t.Fatalf("observed phase = %q, want PERTURB", res.Phase)
	}
}

// With no timing at all, the finding must NOT be silently placed at DRIVE start
// by a zero first_seen_ms. A fabricated position is worse than none: it is the
// field a human reads first when reconstructing what happened.
func TestAViolationWithNoTimingIsPinnedToTheEvaluationPhase(t *testing.T) {
	in := testInput(t)
	out := okDoc()
	out.Status = schema.StatusViolated
	out.Explanation = "no timestamp available"
	d := helperDef(t, helperCmd(t, "emit", "1", docFile(t, out)))

	o, _ := NewProcessOracle(d, ExternalOptions{})
	e := NewEngine()
	if err := e.Register(o); err != nil {
		t.Fatalf("Register: %v", err)
	}
	f, err := e.EvaluateOracleAt(context.Background(), o, schema.PhaseAssert, in)
	if err != nil {
		t.Fatalf("EvaluateOracleAt: %v", err)
	}
	if f.Result.Phase != schema.PhaseAssert {
		t.Fatalf("observed phase = %q, want ASSERT; with no first_seen_ms the honest answer is "+
			"'concluded in ASSERT', not a fabricated DRIVE derived from a zero the oracle "+
			"never supplied", f.Result.Phase)
	}
}

// ---------------------------------------------------------------------------
// stderr reaches the run bundle
// ---------------------------------------------------------------------------

func TestFailingOracleStderrIsCapturedIntoTheRunBundle(t *testing.T) {
	in := testInput(t)
	d := helperDef(t, helperCmd(t, "stderrthen", "exit", "42"))
	res := evaluate(t, d, in, ExternalOptions{})

	if res.Status != schema.StatusInconclusive {
		t.Fatalf("status = %q, want inconclusive", res.Status)
	}
	if !strings.Contains(res.Explanation, "porcupine ate the budget") {
		t.Fatalf("the oracle's own diagnostics did not reach the explanation:\n%s", res.Explanation)
	}

	raw, ok := res.Witness.Extra[WitnessStderrPath]
	if !ok {
		t.Fatalf("the witness does not name the stderr capture; a failing oracle has to be one "+
			"open() away from being debuggable. witness: %#v", res.Witness.Extra)
	}
	var path string
	if err := json.Unmarshal(raw, &path); err != nil {
		t.Fatalf("witness.%s is not a string: %v", WitnessStderrPath, err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the stderr capture named by the witness does not exist: %v", err)
	}
	if !strings.Contains(string(body), "porcupine ate the budget") {
		t.Fatalf("the capture file does not hold the oracle's stderr:\n%s", body)
	}
	if !strings.HasPrefix(path, filepath.Join(in.ArtifactDir, OracleStderrDirName)) {
		t.Fatalf("the capture landed at %q, outside the world's artifact directory %q",
			path, in.ArtifactDir)
	}
}

// A stderr capture that cannot be written must not fail the evaluation: it
// makes a finding harder to debug, not wrong.
func TestAnUncapturableStderrDoesNotChangeTheFinding(t *testing.T) {
	in := testInput(t)
	in.ArtifactDir = "" // no bundle to write into
	d := helperDef(t, helperCmd(t, "emit", "0", docFile(t, okDoc())))
	res := evaluate(t, d, in, ExternalOptions{})
	if res.Status != schema.StatusOK {
		t.Fatalf("status = %q (%s), want ok", res.Status, res.Explanation)
	}
}

// ---------------------------------------------------------------------------
// Integration with the engine: a verdict cannot tell the two kinds apart
// ---------------------------------------------------------------------------

func externalConfig(t *testing.T, dir string) *schema.Config {
	t.Helper()
	return &schema.Config{
		Oracles: schema.OraclesConfig{
			Dir:     dir,
			Builtin: []schema.BuiltinOracle{schema.BuiltinNoCrash},
		},
	}
}

func writeHelperDefinition(t *testing.T, dir, name string, args ...string) {
	t.Helper()
	body := definitionYAML(name, helperCmd(t, args...), "20s")
	if err := os.WriteFile(filepath.Join(dir, name+".yaml"), []byte(body), 0o644); err != nil {
		t.Fatalf("write definition: %v", err)
	}
}

func TestEngineRunsBuiltinsAndExternalsThroughOnePath(t *testing.T) {
	dir := t.TempDir()
	writeHelperDefinition(t, dir, "linearizable.kv", "emit", "0", docFile(t, func() schema.OracleOutput {
		o := okDoc()
		o.ValidPhases = nil // a minimal oracle need not echo its declaration
		o.Oracle = ""
		o.Class = ""
		return o
	}()))

	e, disc, err := NewEngineForConfig(externalConfig(t, dir), DefaultOptions(),
		ExternalOptions{ProjectDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewEngineForConfig: %v", err)
	}
	if disc.Missing {
		t.Fatalf("the oracles directory was reported missing")
	}
	if e.Len() != 2 {
		t.Fatalf("engine holds %d oracles (%v), want no_crash and linearizable.kv",
			e.Len(), e.Names())
	}
	if _, ok := e.Lookup("linearizable.kv"); !ok {
		t.Fatalf("the external oracle did not register: %v", e.Names())
	}

	in := testInput(t)
	in.Nodes = []NodeObservation{{NodeID: "n1", Service: "kv", StateObserved: true, Running: true}}
	fs, err := e.EvaluateAt(context.Background(), schema.PhaseAssert, in)
	if err != nil {
		t.Fatalf("EvaluateAt: %v", err)
	}
	if len(fs) != 2 {
		t.Fatalf("got %d findings, want 2: %v", len(fs), fs.Names())
	}
	for _, f := range fs {
		if f.Result.Status != schema.StatusOK {
			t.Fatalf("oracle %q: %q (%s)", f.Oracle, f.Result.Status, f.Result.Explanation)
		}
		// The two kinds must be indistinguishable in the finding, which is what
		// makes them indistinguishable in the verdict.
		out := f.Output()
		if out.Schema != schema.OracleOutputSchema || out.Oracle == "" ||
			!out.Class.Valid() || len(out.ValidPhases) == 0 {
			t.Fatalf("finding for %q does not render as a complete %s document: %#v",
				f.Oracle, schema.OracleOutputSchema, out)
		}
	}
	if code := fs.ExitCode(); code != schema.ExitPass {
		t.Fatalf("exit code = %d, want 0", code)
	}
}

// An external oracle's inconclusive must reach exit 2 by exactly the path a
// built-in's does.
func TestAnExternalInconclusiveTakesTheRunToExitTwo(t *testing.T) {
	dir := t.TempDir()
	writeHelperDefinition(t, dir, "flaky.check", "exit", "42")

	e, _, err := NewEngineForConfig(externalConfig(t, dir), DefaultOptions(),
		ExternalOptions{ProjectDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewEngineForConfig: %v", err)
	}

	in := testInput(t)
	in.Nodes = []NodeObservation{{NodeID: "n1", Service: "kv", StateObserved: true, Running: true}}
	fs, err := e.EvaluateAt(context.Background(), schema.PhaseAssert, in)
	if err != nil {
		t.Fatalf("EvaluateAt: %v", err)
	}
	if got := fs.ExitCode(); got != schema.ExitInconclusive {
		t.Fatalf("exit code = %d, want 2 (INCONCLUSIVE). A broken external oracle that let the "+
			"run report PASS is the worst output this tool can produce.\n%s", got, fs.Summary())
	}
	if len(fs.Inconclusive()) != 1 {
		t.Fatalf("inconclusive findings = %d, want 1: %s", len(fs.Inconclusive()), fs.Summary())
	}
}

// The engine (not the oracle) decides where an oracle runs (invariant I5).
func TestTheEngineRefusesToEvaluateAnExternalOracleOutsideItsPhases(t *testing.T) {
	d := helperDef(t, helperCmd(t, "emit", "0", docFile(t, okDoc())))
	o, err := NewProcessOracle(d, ExternalOptions{})
	if err != nil {
		t.Fatalf("NewProcessOracle: %v", err)
	}
	e := NewEngine()
	if err := e.Register(o); err != nil {
		t.Fatalf("Register: %v", err)
	}

	// SELECTING skips it, which is correct behaviour for a phase-aware run.
	run, skipped := e.SelectFor(schema.PhaseDrive)
	if len(run) != 0 || len(skipped) != 1 {
		t.Fatalf("SelectFor(DRIVE) = run %d, skipped %d, want 0 and 1", len(run), len(skipped))
	}

	// ASKING it anyway is an engine bug and must fail loudly.
	_, err = e.EvaluateOracleAt(context.Background(), o, schema.PhaseDrive, testInput(t))
	if err == nil {
		t.Fatalf("the engine evaluated an ASSERT-only oracle in DRIVE; checking consistency " +
			"during an active partition is a false-positive factory")
	}
	var pe *PhaseError
	if !errors.As(err, &pe) {
		t.Fatalf("the refusal is not a *PhaseError: %v", err)
	}
	if !errors.Is(err, ErrPhaseInvalid) {
		t.Fatalf("the refusal is not an ErrPhaseInvalid: %v", err)
	}
}

// An external oracle must not be able to shadow a mandatory built-in.
func TestAnExternalOracleCannotShadowABuiltin(t *testing.T) {
	dir := t.TempDir()
	writeHelperDefinition(t, dir, "no_crash", "emit", "0", docFile(t, okDoc()))

	_, _, err := NewEngineForConfig(externalConfig(t, dir), DefaultOptions(),
		ExternalOptions{ProjectDir: t.TempDir()})
	if err == nil {
		t.Fatalf("an external oracle named no_crash replaced the mandatory built-in; that is an " +
			"unreviewable way to swap a required oracle for a permissive one")
	}
	if !strings.Contains(err.Error(), "no_crash") {
		t.Fatalf("the error does not name the collision: %v", err)
	}
}

func TestOraclesDirResolution(t *testing.T) {
	abs := filepath.Join(t.TempDir(), "abs-oracles")
	tests := []struct {
		name    string
		cfgDir  string
		project string
		want    string
	}{
		{"unset", "", "/proj", ""},
		{"relative", ".prothesis/oracles", "/proj", filepath.Join("/proj", ".prothesis/oracles")},
		{"absolute", abs, "/proj", abs},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &schema.Config{Oracles: schema.OraclesConfig{Dir: tc.cfgDir}}
			if got := OraclesDir(cfg, tc.project); got != tc.want {
				t.Fatalf("OraclesDir = %q, want %q", got, tc.want)
			}
		})
	}
	if got := OraclesDir(nil, "/proj"); got != "" {
		t.Fatalf("OraclesDir(nil) = %q, want \"\"", got)
	}
}

// ---------------------------------------------------------------------------
// Unit-level guards
// ---------------------------------------------------------------------------

func TestPhaseSetsEqualIgnoresOrderAndCatchesDifference(t *testing.T) {
	a := []schema.Phase{schema.PhaseAssert, schema.PhaseQuiesce}
	b := []schema.Phase{schema.PhaseQuiesce, schema.PhaseAssert}
	if !phaseSetsEqual(a, b) {
		t.Fatalf("order is not part of a phase declaration; rejecting a reordered list would be " +
			"a false-inconclusive generator")
	}
	if phaseSetsEqual(a, []schema.Phase{schema.PhaseAssert}) {
		t.Fatalf("a shorter list compared equal")
	}
	if phaseSetsEqual(a, []schema.Phase{schema.PhaseAssert, schema.PhaseDrive}) {
		t.Fatalf("a genuinely different declaration compared equal")
	}
	if phaseSetsEqual(a, []schema.Phase{schema.PhaseAssert, schema.PhaseAssert}) {
		t.Fatalf("a duplicate compared equal to two distinct phases")
	}
}

func TestReadCappedBoundsAndSignals(t *testing.T) {
	fired := 0
	b, over := readCapped(strings.NewReader(strings.Repeat("x", 10)), 4, func() { fired++ })
	if !over || len(b) != 4 {
		t.Fatalf("readCapped = %d bytes, over=%v, want 4 and true", len(b), over)
	}
	if fired != 1 {
		t.Fatalf("the overflow callback fired %d times, want 1; killing AT the cap is what makes "+
			"the bound a memory bound rather than a reporting one", fired)
	}
	b, over = readCapped(strings.NewReader("abc"), 16, func() { t.Fatalf("overflow fired under the cap") })
	if over || string(b) != "abc" {
		t.Fatalf("readCapped under the cap = %q, over=%v", b, over)
	}
}

func TestSafeFileNameProducesAnOpenableName(t *testing.T) {
	tests := map[string]string{
		"linearizable.kv": "linearizable.kv",
		"a/b\\c":          "a_b_c",
		"":                "oracle",
		"...":             "oracle",
	}
	for in, want := range tests {
		if got := safeFileName(in); got != want {
			t.Fatalf("safeFileName(%q) = %q, want %q", in, got, want)
		}
	}
}

// ===========================================================================
// The fake oracle
// ===========================================================================

// TestOracleHelperProcess is not a test. It is the external oracle the runner
// tests drive, re-entered through this same binary so the suite needs no build
// step, no shell, and nothing installed on the host, which matters here,
// because the host's Git Bash is non-functional.
//
// Every branch calls os.Exit, so the testing framework never gets to print
// "PASS" onto the stdout the parent is reading as an oracle_output document.
func TestOracleHelperProcess(t *testing.T) {
	_ = t
	args := oracleHelperArgs()
	if len(args) == 0 {
		return
	}
	runOracleHelper(args)
}

func runOracleHelper(args []string) {
	switch args[0] {
	case "emit":
		// emit <exit code> <path to a file printed verbatim on stdout>
		if len(args) < 3 {
			fmt.Fprintln(os.Stderr, "helper: emit needs an exit code and a document path")
			os.Exit(94)
		}
		code, err := strconv.Atoi(args[1])
		if err != nil {
			os.Exit(99)
		}
		body, err := os.ReadFile(args[2])
		if err != nil {
			fmt.Fprintln(os.Stderr, "helper:", err)
			os.Exit(98)
		}
		_, _ = os.Stdout.Write(body)
		os.Exit(code)

	case "exit":
		if len(args) < 2 {
			os.Exit(94)
		}
		code, err := strconv.Atoi(args[1])
		if err != nil {
			os.Exit(99)
		}
		os.Exit(code)

	case "panic":
		// A GENUINE crash, not a simulated one: the runtime tears the process
		// down, nothing well-formed reaches stdout, and the exit status is the
		// runtime's, not one this helper chose.
		var m map[string]int
		m["boom"] = 1
		os.Exit(0) // unreachable

	case "hang":
		if len(args) < 2 {
			os.Exit(94)
		}
		ms, err := strconv.Atoi(args[1])
		if err != nil {
			os.Exit(99)
		}
		time.Sleep(time.Duration(ms) * time.Millisecond)
		os.Exit(0)

	case "ignoreterm":
		// Swallow the polite signal so the runner must escalate.
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGTERM, os.Interrupt)
		// Self-limiting: if the tree kill ever fails this still goes away rather
		// than leaving a process behind on the build machine.
		time.Sleep(30 * time.Second)
		os.Exit(0)

	case "flood":
		// flood <bytes> <exit code>
		if len(args) < 3 {
			os.Exit(94)
		}
		n, err := strconv.Atoi(args[1])
		if err != nil {
			os.Exit(99)
		}
		code, err := strconv.Atoi(args[2])
		if err != nil {
			os.Exit(99)
		}
		chunk := []byte(strings.Repeat("A", 4096))
		for written := 0; written < n; written += len(chunk) {
			if _, err := os.Stdout.Write(chunk); err != nil {
				break
			}
		}
		os.Exit(code)

	case "stderrthen":
		// stderrthen <rest...>: write diagnostics, then behave as <rest...>.
		fmt.Fprintln(os.Stderr, "checker: the porcupine ate the budget on key k/17")
		fmt.Fprintln(os.Stderr, "checker: explored 412000 states before giving up")
		if len(args) < 2 {
			os.Exit(94)
		}
		runOracleHelper(args[1:])
		os.Exit(0)

	case "echostdin":
		raw, err := io.ReadAll(os.Stdin)
		if err != nil {
			fmt.Fprintln(os.Stderr, "helper: read stdin:", err)
			os.Exit(2)
		}
		in, err := schema.UnmarshalOracleInput(raw)
		if err != nil {
			fmt.Fprintln(os.Stderr, "helper: stdin is not an oracle_input:", err)
			os.Exit(2)
		}
		out := schema.OracleOutput{
			Oracle:      testOracleName,
			Class:       schema.ClassConsistency,
			ValidPhases: []schema.Phase{schema.PhaseAssert},
			Status:      schema.StatusOK,
			Witness:     schema.Witness{Key: in.HistoryPath},
			Explanation: fmt.Sprintf("received %d phase window(s)", len(in.Phases)),
		}
		b, err := schema.MarshalOracleOutput(&out)
		if err != nil {
			os.Exit(2)
		}
		_, _ = os.Stdout.Write(b)
		os.Exit(0)
	}

	fmt.Fprintln(os.Stderr, "helper: unknown mode:", args[0])
	os.Exit(95)
}

func oracleHelperArgs() []string {
	for i, a := range os.Args {
		if a == "--" {
			return os.Args[i+1:]
		}
	}
	return nil
}
