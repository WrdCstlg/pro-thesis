package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/WrdCstlg/pro-thesis/internal/recorder"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// ---------------------------------------------------------------------------
// world.thesis exists before any oracle is handed a path to it
//
// prothesis.oracle_input/v1 names world_path, and an external oracle is entitled
// to open it. The runner used to evaluate first and write world.thesis after, so
// the path named a file that did not exist yet, and a failed write was discarded.
// ---------------------------------------------------------------------------

const worldReaderOracle = "world.reader"

// TestControlOracleHelperProcess is not a test. Re-executed by the oracle
// definition below, it is an external oracle speaking the real protocol: it
// reads prothesis.oracle_input/v1 on stdin, opens world_path, parses it as a
// canonical world, and reports ok only if that worked.
func TestControlOracleHelperProcess(t *testing.T) {
	_ = t
	var args []string
	for i, a := range os.Args {
		if a == "--" {
			args = os.Args[i+1:]
		}
	}
	if len(args) == 0 || args[0] != "read-world" {
		return
	}
	status, code := schema.StatusViolated, 1
	var explanation string
	var in schema.OracleInput
	if err := json.NewDecoder(os.Stdin).Decode(&in); err != nil {
		explanation = "could not read oracle input on stdin: " + err.Error()
	} else if w, err := recorder.LoadWorld(in.WorldPath); err != nil {
		explanation = fmt.Sprintf("world_path %s could not be opened and parsed: %v", in.WorldPath, err)
	} else {
		status, code = schema.StatusOK, 0
		explanation = fmt.Sprintf("world_path parsed: seed %d, schema %s", w.Seed, w.Schema)
	}
	out := schema.OracleOutput{
		Oracle: worldReaderOracle, Class: schema.ClassConsistency,
		ValidPhases: []schema.Phase{schema.PhaseAssert}, Status: status, Explanation: explanation,
	}
	b, _ := schema.MarshalOracleOutput(&out)
	_, _ = os.Stdout.Write(b)
	os.Exit(code)
}

func worldReaderConfig(t *testing.T) *schema.Config {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	if strings.Contains(exe, "'") {
		t.Fatalf("test binary path contains a single quote: %q", exe)
	}
	dir := t.TempDir()
	body := "version: prothesis.oracle_def/v1\n" +
		"name: " + worldReaderOracle + "\n" +
		"class: consistency\n" +
		"valid_phases: [ASSERT]\n" +
		"cmd: |-\n  '" + exe + "' -test.run=^TestControlOracleHelperProcess$ -- read-world\n" +
		"timeout: 60s\n"
	if err := os.WriteFile(filepath.Join(dir, worldReaderOracle+".yaml"), []byte(body), 0o644); err != nil {
		t.Fatalf("write definition: %v", err)
	}
	cfg := perturbTestConfig(t)
	cfg.Oracles = schema.OraclesConfig{Dir: dir, Builtin: []schema.BuiltinOracle{}}
	return cfg
}

func TestAnExternalOracleCanOpenAndParseWorldPath(t *testing.T) {
	origInspect := inspectDockerState
	t.Cleanup(func() { inspectDockerState = origInspect })
	inspectDockerState = func(context.Context, string) (*dockerContainerState, error) {
		return nil, errors.New("stub backend has no docker containers")
	}
	r, err := NewRunner(RunnerOptions{
		Config:     worldReaderConfig(t),
		ProjectDir: t.TempDir(),
		Backend: &stubBackend{nodes: []recorder.NodeBinding{
			{ID: "kv-n1", ContainerID: "c1", HostPort: 18081, ContainerPort: 8080, Service: "kv-n1"},
			{ID: "kv-n2", ContainerID: "c2", HostPort: 18082, ContainerPort: 8080, Service: "kv-n2"},
			{ID: "kv-n3", ContainerID: "c3", HostPort: 18083, ContainerPort: 8080, Service: "kv-n3"},
		}},
		Driver:        finishedDriver(),
		Telemetry:     stubTelemetry{},
		LogCollector:  stubLogs{},
		ImageResolver: stubImages{},
		Quiet:         true,
		// OracleEngine nil: the production engine runs the external process.
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	verdict, exit, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	doc := readResultDoc(t, r, 1)
	oracles, _ := doc["oracles"].([]any)
	if len(oracles) != 1 {
		t.Fatalf("result.json records %d oracles, want the one external oracle", len(oracles))
	}
	o, _ := oracles[0].(map[string]any)
	if exit != schema.ExitPass {
		t.Fatalf("exit = %d (verdict %s): the external oracle could not use world_path: %v",
			exit, verdict.Verdict, o["explanation"])
	}
	// The world it parsed is THIS world: the seed it read is the seed recorded.
	// Decoded exactly: a uint64 seed does not survive a float64.
	raw, err := os.ReadFile(NewWorldPaths(joinRunWorld(r, 1)).Result)
	if err != nil {
		t.Fatalf("read result.json: %v", err)
	}
	var seeded struct {
		Seed uint64 `json:"seed"`
	}
	if err := json.Unmarshal(raw, &seeded); err != nil {
		t.Fatalf("parse result.json seed: %v", err)
	}
	want := fmt.Sprintf("seed %d,", seeded.Seed)
	if expl, _ := o["explanation"].(string); !strings.Contains(expl, want) {
		t.Fatalf("the oracle parsed a world whose seed is not this world's (%s): %s", want, expl)
	}
}

// A world.thesis that cannot be written is not a detail to drop: replay,
// shrink and the corpus all start from it. The world is a harness error.
func TestAWorldFileThatCannotBeWrittenIsAHarnessError(t *testing.T) {
	var stderr bytes.Buffer
	r, _ := newPerturbTestRunner(t, perturbTestConfig(t), nil)
	r.stderr = &stderr
	// A directory where the file must go makes the write fail.
	if err := os.MkdirAll(NewWorldPaths(joinRunWorld(r, 1)).World, 0o755); err != nil {
		t.Fatalf("plant directory: %v", err)
	}
	_, exit, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if exit != schema.ExitInconclusive {
		t.Fatalf("exit = %d, want 2: the world's record could not be written", exit)
	}
	if got := readResultDoc(t, r, 1)["outcome"]; got != string(OutcomeHarnessError) {
		t.Fatalf("result.json outcome = %v, want %q", got, OutcomeHarnessError)
	}
	if !strings.Contains(stderr.String(), "world.thesis") {
		t.Fatalf("stderr does not say which artifact failed:\n%s", stderr.String())
	}
}
