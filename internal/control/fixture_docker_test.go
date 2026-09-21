package control

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/WrdCstlg/pro-thesis/internal/harness"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// ---------------------------------------------------------------------------
// The two claims that can only be made against a real cluster.
//
//  1. A DRAINED world with nothing wrong reaches PASS. That is the OQ-033 fix,
//     and asserting it against a stub would assert nothing: the whole defect was
//     that a REAL driver, killed mid-flight, leaves operations no oracle can
//     judge. Only the fixture's loadgen can demonstrate otherwise.
//
//  2. Concurrent worlds are genuinely isolated: distinct compose projects,
//     distinct published host ports, distinct networks, and every project torn
//     down. Port and network collisions do not exist in a stub.
//
// Both are OPT-IN. They cost real minutes and real bridge networks, and this
// host has about twenty-four of the latter. Run them with:
//
//	$env:PROTHESIS_DOCKER_TESTS = "1"
//	go test -count=1 -timeout 20m -run TestFixture ./internal/control/...
//
// Skipping is explicit and says exactly what is missing, so a skip is never
// mistaken for a pass.
// ---------------------------------------------------------------------------

const dockerTestsEnv = "PROTHESIS_DOCKER_TESTS"

// fixtureDir locates testdata/kvfixture from this package.
func fixtureDir(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatalf("cannot locate this source file")
	}
	root := filepath.Dir(filepath.Dir(filepath.Dir(thisFile))) // internal/control -> internal -> repo
	return filepath.Join(root, "testdata", "kvfixture")
}

// requireFixture gates on everything the fixture needs, naming each miss.
func requireFixture(t *testing.T) (dir string, cfg *schema.Config) {
	t.Helper()
	if os.Getenv(dockerTestsEnv) != "1" {
		t.Skipf("SKIPPED: set %s=1 to run the Docker-backed fixture tests "+
			"(they cost minutes and bridge networks)", dockerTestsEnv)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx, "docker", "info", "--format", "{{.ServerVersion}}").Run(); err != nil {
		t.Skipf("SKIPPED: the Docker daemon is not reachable (%v); these tests need it", err)
	}

	dir = fixtureDir(t)
	if _, err := os.Stat(filepath.Join(dir, "prothesis.yaml")); err != nil {
		t.Skipf("SKIPPED: the KV fixture is not present at %s (%v)", dir, err)
	}
	loadgen := filepath.Join(dir, "bin", "loadgen.exe")
	if runtime.GOOS != "windows" {
		loadgen = filepath.Join(dir, "bin", "loadgen")
	}
	if _, err := os.Stat(loadgen); err != nil {
		t.Skipf("SKIPPED: the fixture driver is not built at %s; build it with "+
			"`go build -o bin/loadgen ./cmd/loadgen` inside the fixture module", loadgen)
	}

	data, err := os.ReadFile(filepath.Join(dir, "prothesis.yaml"))
	if err != nil {
		t.Fatalf("read the fixture config: %v", err)
	}
	cfg, err = schema.DecodeConfig(data)
	if err != nil {
		t.Fatalf("decode the fixture config: %v", err)
	}
	return dir, cfg
}

// TestFixtureDrainReachesPassOnACleanWorld is the OQ-033 demonstration.
//
// Before the drain existed, this exact configuration was measured as
// INCONCLUSIVE (exit 2): the `linear` profile's op budget deliberately outlives
// the fault window, so the driver was ALWAYS killed with operations in flight,
// and no_stuck_op, correctly, refused to call an unobserved operation either
// stuck or fine. A clean world could not report PASS, so the Saboteur's utility
// function could not tell "nothing is wrong here" from "I could not tell".
//
// What is asserted is the WORLD's outcome, not the run's verdict. This run is
// narrowed to 1 of the profile's 5 worlds, and since D-059 a narrowed run that
// finds nothing may not report PASS: it did not perform the gate the profile
// specifies, so the RUN verdict is INCONCLUSIVE by construction. OQ-033 is about
// whether a clean world's own outcome is distinguishable from an unevaluable
// one, and narrowing does not touch that. Until 2026-09-15 this test asserted
// the run verdict; it is opt-in, D-059 had made the assertion impossible five
// days earlier, and nothing noticed until CI on a Linux runner turned it on
// (OQ-066).
func TestFixtureDrainReachesPassOnACleanWorld(t *testing.T) {
	dir, cfg := requireFixture(t)

	// The fixture's external checker is part of every world's verdict. Without
	// it linearizable.kv cannot start and the world is, correctly,
	// INCONCLUSIVE, which this test would otherwise report as the OQ-033 defect.
	checker := filepath.Join(dir, "bin", "linearizable-kv")
	if runtime.GOOS == "windows" {
		checker += ".exe"
	}
	if _, err := os.Stat(checker); err != nil {
		t.Skipf("SKIPPED: the fixture's external checker is not built at %s; build it from the "+
			"repository root with `go build -o testdata/kvfixture/bin/linearizable-kv "+
			"./cmd/thesis-oracle-linearizable`", checker)
	}

	runsDir := t.TempDir()
	cfg.Artifacts.Dir = runsDir

	r, err := NewRunner(RunnerOptions{
		Config:     cfg,
		ProjectDir: dir,
		Profile:    "linear",
		Seed:       424242,
		Worlds:     1,
		Budget:     8 * time.Minute,
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	verdict, exit, err := r.Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	drainDoc := readDrainDoc(t, r, 1)
	if drainDoc["drained"] != true {
		t.Fatalf("the fixture's loadgen did not drain: %v\n"+
			"without a drain the world cannot reach PASS, which is the whole of OQ-033", drainDoc)
	}

	res := readResultDoc(t, r, 1)
	if res["outcome"] != "pass" {
		t.Fatalf("a CLEAN drained world's own outcome is %v, not pass.\n"+
			"This is the OQ-033 defect: if a clean world is indistinguishable from an "+
			"unevaluable one, the search ranks worlds on noise.\nresult.json:\n%s",
			res["outcome"], mustJSON(res))
	}

	// D-059: the run was narrowed, so a clean run is INCONCLUSIVE and says why.
	if exit != schema.ExitInconclusive || !verdict.Budget.Narrowed {
		t.Fatalf("the run was narrowed to 1 of the profile's worlds, so D-059 requires "+
			"INCONCLUSIVE (exit 2) with budget.narrowed set; got %s (exit %d), narrowed=%v",
			verdict.Verdict, exit, verdict.Budget.Narrowed)
	}

	// The oracle whose refusal caused OQ-033 must be the one that now passes,
	// on evidence rather than by being weakened.
	found := false
	for _, o := range res["oracles"].([]any) {
		m := o.(map[string]any)
		if m["oracle"] != "no_stuck_op" {
			continue
		}
		found = true
		if m["status"] != "ok" {
			t.Fatalf("no_stuck_op reported %v: %v", m["status"], m["explanation"])
		}
		if !strings.Contains(m["explanation"].(string), "completed within SLO ceiling") {
			t.Fatalf("no_stuck_op passed for an unexpected reason: %v", m["explanation"])
		}
	}
	if !found {
		t.Fatalf("no_stuck_op did not run; a PASS it did not participate in proves nothing")
	}
}

// TestFixtureParallelWorldsAreIsolatedAndTornDown exercises the real constraint:
// two concurrent compose projects, four bridge networks, distinct published host
// ports, and nothing left behind.
//
// Two workers rather than four, deliberately. The measured 3.8x needs eight
// projects, but a TEST that consumes sixteen of this host's twenty-four networks
// would be a hazard to whatever else is running; two proves isolation, and
// isolation is the property under test.
func TestFixtureParallelWorldsAreIsolatedAndTornDown(t *testing.T) {
	dir, cfg := requireFixture(t)

	cfg.Artifacts.Dir = t.TempDir()

	r, err := NewRunner(RunnerOptions{
		Config:     cfg,
		ProjectDir: dir,
		Profile:    "smoke",
		Seed:       777,
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	pr, err := r.Parallel(ParallelOptions{
		Workers: 2,
		Networks: NetworkBudget{
			Probe: func(ctx context.Context) (int, error) { return harness.FreeBridgeNetworks(ctx) },
		},
		// The fixture's own compose variables. This mapping is the worked
		// example ComposeVarEnv documents.
		WorkerEnv: ComposeVarEnv("KV_PREFIX", map[string]string{
			"kv-n1": "KV_PORT_N1",
			"kv-n2": "KV_PORT_N2",
			"kv-n3": "KV_PORT_N3",
		}),
	})
	if err != nil {
		t.Fatalf("Parallel: %v", err)
	}

	before := countNetworks(t)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	out, err := pr.Run(ctx, []WorldRequest{{Ordinal: 1}, {Ordinal: 2}})
	if err != nil {
		t.Fatalf("parallel Run: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("got %d outcomes, want 2", len(out))
	}

	// Ordinal order, whatever the completion order.
	if out[0].Ordinal != 1 || out[1].Ordinal != 2 {
		t.Fatalf("outcomes came back out of order: %d, %d", out[0].Ordinal, out[1].Ordinal)
	}
	if out[0].Project == out[1].Project {
		t.Fatalf("both worlds used compose project %q", out[0].Project)
	}
	for _, o := range out {
		if o.TeardownErr != nil {
			t.Fatalf("world %d leaked project %s: %v", o.Ordinal, o.Project, o.TeardownErr)
		}
		if o.Outcome == OutcomeHarnessError {
			t.Fatalf("world %d failed to run: %v (project %s)", o.Ordinal, o.Err, o.Project)
		}
	}

	// The direct evidence that the compose-variable mapping worked. The ports a
	// slot REQUESTS prove nothing on their own: a compose file that ignored
	// KV_PORT_N1 would publish 18081 for both worlds, and the collision would
	// surface as an unattributable boot failure rather than as this assertion.
	// These are the ports `docker compose port` reported for the live cluster.
	if len(out[0].HostPorts) == 0 || len(out[1].HostPorts) == 0 {
		t.Fatalf("a world reported no published host ports: %v / %v",
			out[0].HostPorts, out[1].HostPorts)
	}
	for node, p := range out[0].HostPorts {
		q, ok := out[1].HostPorts[node]
		if !ok {
			t.Fatalf("world 2 published no port for node %q", node)
		}
		if p == q {
			t.Fatalf("both worlds published node %q on host port %d; the per-world "+
				"KV_PORT_* mapping did not reach compose", node, p)
		}
		if p == 0 || q == 0 {
			t.Fatalf("node %q was bound to port 0 in a world (%d / %d)", node, p, q)
		}
	}
	t.Logf("world 1 host ports %v; world 2 host ports %v", out[0].HostPorts, out[1].HostPorts)

	// And the driver's half. Isolating the CLUSTERS is only half of it: a driver
	// whose targets are baked in addresses whichever cluster owns the default
	// port, or nothing at all, and a world that drove nothing still writes a
	// history a checker will happily call clean. So each world's history must
	// contain operations that actually SUCCEEDED against its own nodes.
	for _, o := range out {
		ok, nodes := historyOKCount(t, o.Paths.History)
		if ok == 0 {
			t.Fatalf("world %d (project %s, ports %v) retired no successful operation; its "+
				"driver did not reach its own cluster, and the world reports on a system it "+
				"never touched", o.Ordinal, o.Project, o.HostPorts)
		}
		if len(nodes) == 0 {
			t.Fatalf("world %d recorded %d ok operation(s) but named no serving node", o.Ordinal, ok)
		}
		t.Logf("world %d: %d ok operation(s) served by %v", o.Ordinal, ok, nodes)
	}

	// The leak check that actually matters on this host.
	after := countNetworks(t)
	if after > before {
		t.Fatalf("the run left %d extra bridge network(s) behind (%d -> %d); on this host a "+
			"leaked network is gone until someone removes it by hand", after-before, before, after)
	}
}

// historyOKCount reports how many operations succeeded and which nodes served
// them, straight out of the history the driver wrote.
func historyOKCount(t *testing.T, path string) (int, []string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read history %s: %v", path, err)
	}
	count := 0
	nodes := map[string]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var rec struct {
			Type string `json:"type"`
			Meta struct {
				Node string `json:"node"`
			} `json:"meta"`
		}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		if rec.Type == "ok" {
			count++
			if rec.Meta.Node != "" {
				nodes[rec.Meta.Node] = true
			}
		}
	}
	out := make([]string, 0, len(nodes))
	for n := range nodes {
		out = append(out, n)
	}
	sort.Strings(out)
	return count, out
}

func countNetworks(t *testing.T) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	n, err := harness.CountBridgeNetworks(ctx)
	if err != nil {
		t.Fatalf("count bridge networks: %v", err)
	}
	return n
}

func mustJSON(v any) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "<unmarshalable>"
	}
	return string(b)
}
