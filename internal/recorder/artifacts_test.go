package recorder

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/WrdCstlg/pro-thesis/internal/recorder/cjson"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

func testWall() time.Time { return time.Date(2026, 9, 3, 18, 4, 5, 0, time.UTC) }

// ---------------------------------------------------------------------------
// run_id
// ---------------------------------------------------------------------------

func TestAllocRunIDShapeAndUniqueness(t *testing.T) {
	runs := filepath.Join(t.TempDir(), ".prothesis", "runs")

	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		id, dir, err := AllocRunID(runs, testWall())
		if err != nil {
			t.Fatalf("attempt %d: %v", i, err)
		}
		if !ValidRunID(id) {
			t.Fatalf("run id %q does not match %s", id, schema.RunIDShape)
		}
		if len(id) != RunIDLen {
			t.Fatalf("run id %q is %d characters, want %d", id, len(id), RunIDLen)
		}
		if !strings.HasPrefix(id, "r_2026_09_03_") {
			t.Fatalf("run id %q does not carry the UTC date", id)
		}
		if strings.ContainsRune(id, ':') {
			t.Fatalf("run id %q contains a colon and is not a legal Windows path component", id)
		}
		if seen[id] {
			t.Fatalf("run id %q was allocated twice", id)
		}
		seen[id] = true
		if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
			t.Fatalf("run directory %s was not created: %v", dir, err)
		}
	}
}

// TestAllocRunIDDetectsCollisionExactly: uniqueness is not a probabilistic
// claim. If the directory already exists, mkdir fails and the id is retried.
func TestAllocRunIDDetectsCollisionExactly(t *testing.T) {
	runs := filepath.Join(t.TempDir(), "runs")
	id, _, err := AllocRunID(runs, testWall())
	if err != nil {
		t.Fatal(err)
	}
	// Re-creating the same directory must fail, which is the mechanism the
	// allocator relies on.
	if err := os.Mkdir(filepath.Join(runs, id), 0o755); err == nil {
		t.Fatal("os.Mkdir succeeded over an existing directory; the uniqueness " +
			"mechanism this allocator depends on does not hold on this filesystem")
	}
}

func TestValidRunIDRejectsNearMisses(t *testing.T) {
	bad := []string{
		"", "r_2026_09_03", "r_2026_09_03_a41", "r_2026_09_03_a41f5",
		"r_2026_09_03_A41F", "r_26_09_03_a41f", "x_2026_09_03_a41f",
		"r_2026_09_03_a41f ", "r:2026_09_03_a41f", "r_2026_09_03_zzzz",
	}
	for _, s := range bad {
		if ValidRunID(s) {
			t.Errorf("ValidRunID(%q) is true", s)
		}
	}
	if !ValidRunID("r_2026_09_03_a41f") {
		t.Error("the specification's own example run id was rejected")
	}
}

// ---------------------------------------------------------------------------
// Path safety
// ---------------------------------------------------------------------------

func TestSafeName(t *testing.T) {
	ok := []string{"n1", "kv-node-3", "driver.stdout.log", "w_a41f9c2b7e10.thesis", "A_1"}
	for _, s := range ok {
		if err := SafeName(s); err != nil {
			t.Errorf("SafeName(%q) = %v, want nil", s, err)
		}
	}
	bad := []string{
		"", "n 1", "n/1", `n\1`, "n:1", "..", ".", "n1.", "n1 ",
		"con", "CON", "nul.log", "COM1", "lpt9.txt", "aux",
		strings.Repeat("x", 65), "naïve",
	}
	for _, s := range bad {
		if err := SafeName(s); err == nil {
			t.Errorf("SafeName(%q) = nil, want an error", s)
		}
	}
}

func TestBundleRejectsUnsafeRelativePaths(t *testing.T) {
	b := newTestBundle(t)
	bad := []string{"", "/abs", `sub\file`, "../escape", "a/../b", "a//b", "con/x", "x/con"}
	for _, rel := range bad {
		if _, err := b.Path(rel); err == nil {
			t.Errorf("Path(%q) was accepted", rel)
		}
	}
	if _, err := b.Path("logs/n1.stdout.log"); err != nil {
		t.Errorf("Path on a legitimate relative path failed: %v", err)
	}
}

func newTestBundle(t *testing.T) *Bundle {
	t.Helper()
	b, err := OpenBundle(filepath.Join(t.TempDir(), "runs"), testWall())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Close() })
	return b
}

// ---------------------------------------------------------------------------
// The up -> down handoff
// ---------------------------------------------------------------------------

// TestUpDownHandoffSurvivesProcessBoundary is the artifact-layer half of Phase 0
// definition of done (a).
//
// `thesis up` and `thesis down` are separate OS processes. Everything `down`
// needs (the compose project name, the generated overlay file, the published
// port assignments, the logical node to container bindings) exists only inside
// the `up` process unless it is written to disk. The test deliberately shares
// NOTHING between the two halves except the runs directory path, which is all a
// second process would have.
func TestUpDownHandoffSurvivesProcessBoundary(t *testing.T) {
	runs := filepath.Join(t.TempDir(), ".prothesis", "runs")

	// --- what `thesis up` does -------------------------------------------
	var upRunID string
	{
		b, err := OpenBundle(runs, testWall())
		if err != nil {
			t.Fatal(err)
		}
		upRunID = b.RunID()
		topo := &Topology{
			Backend:           string(schema.BackendCompose),
			ComposeFile:       "docker-compose.yaml",
			ComposeProject:    "thesis-" + b.RunID(),
			CreatedWallUnixNS: testWall().UnixNano(),
			OverlayFile:       "docker-compose.thesis.yaml",
			ProjectDir:        `C:\AI Projects\Pro-synthesis\testdata\kvfixture`,
			Nodes: []NodeBinding{
				{ID: "n1", Service: "kv1", ContainerID: "abc123", HostPort: 18081, ContainerPort: 8080},
				{ID: "n2", Service: "kv2", ContainerID: "def456", HostPort: 18082, ContainerPort: 8080},
				{ID: "n3", Service: "kv3", ContainerID: "ghi789", HostPort: 18083, ContainerPort: 8080},
			},
		}
		if err := b.WriteTopology(topo); err != nil {
			t.Fatalf("WriteTopology: %v", err)
		}
		if err := WriteCurrentRun(runs, b.RunID()); err != nil {
			t.Fatal(err)
		}
		// `up` returns. Nothing else is carried forward.
	}

	// --- what `thesis down` does, in a different process -------------------
	{
		runID, err := ReadCurrentRun(runs)
		if err != nil {
			t.Fatalf("down could not find the live run: %v", err)
		}
		if runID != upRunID {
			t.Fatalf("current run is %q, want %q", runID, upRunID)
		}
		topo, err := ReadTopologyForRun(runs, runID)
		if err != nil {
			t.Fatalf("down could not read the topology: %v", err)
		}
		if topo.ComposeProject != "thesis-"+upRunID {
			t.Fatalf("compose project is %q", topo.ComposeProject)
		}
		if topo.OverlayFile != "docker-compose.thesis.yaml" {
			t.Fatalf("overlay file is %q; `down` must pass the same file set or "+
				"compose resolves a different project", topo.OverlayFile)
		}
		if len(topo.Nodes) != 3 {
			t.Fatalf("got %d node bindings, want 3", len(topo.Nodes))
		}
		want := map[string]int64{"n1": 18081, "n2": 18082, "n3": 18083}
		for _, n := range topo.Nodes {
			if want[n.ID] != n.HostPort {
				t.Fatalf("node %s published port %d, want %d", n.ID, n.HostPort, want[n.ID])
			}
			if n.ContainerID == "" {
				t.Fatalf("node %s lost its container binding", n.ID)
			}
		}

		b, err := OpenBundleAt(runs, runID)
		if err != nil {
			t.Fatal(err)
		}
		if err := b.Close(); err != nil {
			t.Fatal(err)
		}
		if err := ClearCurrentRun(runs); err != nil {
			t.Fatal(err)
		}
	}

	// --- `down` must be safe to run twice ---------------------------------
	if _, err := ReadCurrentRun(runs); !errors.Is(err, ErrNoCurrentRun) {
		t.Fatalf("after teardown, ReadCurrentRun returned %v, want ErrNoCurrentRun", err)
	}
	if err := ClearCurrentRun(runs); err != nil {
		t.Fatalf("a second teardown failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(runs, upRunID, RunningSentinel)); !os.IsNotExist(err) {
		t.Fatal("the RUNNING sentinel survived a clean teardown")
	}
}

func TestTopologyValidateRejectsUnusableRecords(t *testing.T) {
	base := func() *Topology {
		return &Topology{
			Schema:         TopologySchema,
			RunID:          "r_2026_09_03_a41f",
			Backend:        string(schema.BackendCompose),
			ComposeProject: "thesis-x",
			Nodes: []NodeBinding{
				{ID: "n1", Service: "kv1", HostPort: 18081, ContainerPort: 8080},
			},
		}
	}
	if err := base().Validate(); err != nil {
		t.Fatalf("a valid topology was rejected: %v", err)
	}

	cases := map[string]func(*Topology){
		"no compose project": func(x *Topology) { x.ComposeProject = "" },
		"bad run id":         func(x *Topology) { x.RunID = "nope" },
		"wrong schema":       func(x *Topology) { x.Schema = "other/v1" },
		"null nodes":         func(x *Topology) { x.Nodes = nil },
		"duplicate node id": func(x *Topology) {
			x.Nodes = append(x.Nodes, NodeBinding{ID: "n1", Service: "kv2", HostPort: 18082, ContainerPort: 8080})
		},
		"duplicate host port": func(x *Topology) {
			x.Nodes = append(x.Nodes, NodeBinding{ID: "n2", Service: "kv2", HostPort: 18081, ContainerPort: 8080})
		},
		"unsafe node id": func(x *Topology) { x.Nodes[0].ID = "n 1" },
		"port out of range": func(x *Topology) {
			x.Nodes[0].HostPort = 70000
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			x := base()
			mutate(x)
			if err := x.Validate(); err == nil {
				t.Fatal("Validate accepted an unusable topology")
			}
		})
	}
}

// TestTopologyIsCanonicalOnDisk: the handoff record is read by a different
// process, so it goes through the same strict codec as everything else.
func TestTopologyIsCanonicalOnDisk(t *testing.T) {
	b := newTestBundle(t)
	topo := &Topology{
		Backend:        string(schema.BackendCompose),
		ComposeProject: "thesis-x",
		Nodes: []NodeBinding{
			{ID: "n1", Service: "kv1", HostPort: 18081, ContainerPort: 8080},
		},
	}
	if err := b.WriteTopology(topo); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(b.RunDir(), TopologyFileName))
	if err != nil {
		t.Fatal(err)
	}
	if err := cjson.UnmarshalCanonical(raw, new(Topology)); err != nil {
		t.Fatalf("topology.json is not canonical: %v", err)
	}
	if topo.RunID != b.RunID() || topo.Schema != TopologySchema {
		t.Fatalf("WriteTopology did not stamp run id and schema: %+v", topo)
	}
}

func TestReadCurrentRunRejectsGarbage(t *testing.T) {
	runs := filepath.Join(t.TempDir(), "runs")
	if err := os.MkdirAll(runs, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runs, CurrentPointer), []byte("../../etc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadCurrentRun(runs); err == nil {
		t.Fatal("ReadCurrentRun accepted a value that is not a run id")
	}
	if err := WriteCurrentRun(runs, "not-a-run-id"); err == nil {
		t.Fatal("WriteCurrentRun accepted a value that is not a run id")
	}
}

// ---------------------------------------------------------------------------
// Bundle
// ---------------------------------------------------------------------------

func TestBundleWorldDirAndManifest(t *testing.T) {
	runs := filepath.Join(t.TempDir(), "runs")
	b, err := OpenBundle(runs, testWall())
	if err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(filepath.Join(b.RunDir(), RunningSentinel)); err != nil {
		t.Fatalf("OpenBundle did not write the RUNNING sentinel: %v", err)
	}

	if err := b.WriteJSON(ClockFileName, NewSimClock(testWall()).Health()); err != nil {
		t.Fatal(err)
	}

	wd, err := b.WorldDir(0)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := filepath.Base(wd.Dir()), "000000"; got != want {
		t.Fatalf("world directory is %q, want %q", got, want)
	}
	w := goldenWorld(t)
	if err := wd.StoreWorld(&w); err != nil {
		t.Fatal(err)
	}
	hashLine, err := wd.ReadFile(WorldHashFileName)
	if err != nil {
		t.Fatal(err)
	}
	wantHash, err := HashWorld(&w)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(hashLine)) != wantHash {
		t.Fatalf("world_hash sidecar is %q, want %q", hashLine, wantHash)
	}

	app, err := wd.Appender("timeline.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if err := app.AppendJSON(StreamAudit{Domain: StreamFaultSchedule, Draws: uint64(i)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := app.AppendLine([]byte("bad\nline")); err == nil {
		t.Fatal("AppendLine accepted a record containing a newline")
	}

	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	if err := b.Close(); err != nil {
		t.Fatalf("Close is not idempotent: %v", err)
	}
	if _, err := os.Stat(filepath.Join(b.RunDir(), RunningSentinel)); !os.IsNotExist(err) {
		t.Fatal("Close did not remove the RUNNING sentinel")
	}

	// The manifest must content-address every file, including ones written
	// through an Appender rather than WriteJSON.
	raw, err := os.ReadFile(filepath.Join(b.RunDir(), ManifestFileName))
	if err != nil {
		t.Fatal(err)
	}
	var m Manifest
	if err := cjson.UnmarshalCanonical(raw, &m); err != nil {
		t.Fatalf("MANIFEST.json is not canonical: %v", err)
	}
	if m.Schema != ManifestSchema || m.RunID != b.RunID() {
		t.Fatalf("manifest header is %+v", m)
	}
	wantFiles := map[string]bool{
		"clock.json":                   false,
		"worlds/000000/world.thesis":   false,
		"worlds/000000/world_hash":     false,
		"worlds/000000/timeline.jsonl": false,
	}
	for _, f := range m.Files {
		if _, ok := wantFiles[f.Path]; ok {
			wantFiles[f.Path] = true
		}
		if f.Path == ManifestFileName || f.Path == RunningSentinel {
			t.Fatalf("the manifest lists %q, which it must exclude", f.Path)
		}
		// Verify the recorded digest against the file itself.
		data, err := os.ReadFile(filepath.Join(b.RunDir(), filepath.FromSlash(f.Path)))
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(data)
		if want := schema.HashPrefix + hex.EncodeToString(sum[:]); f.Sha256 != want {
			t.Fatalf("%s: manifest records %s, actual %s", f.Path, f.Sha256, want)
		}
		if f.Size != int64(len(data)) {
			t.Fatalf("%s: manifest records size %d, actual %d", f.Path, f.Size, len(data))
		}
	}
	for p, found := range wantFiles {
		if !found {
			t.Errorf("the manifest omits %s", p)
		}
	}

	// bundle_sha must be a function of the file list alone.
	pre, err := cjson.EncodeCompact(m.Files)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(pre)
	if want := schema.HashPrefix + hex.EncodeToString(sum[:]); m.BundleSha != want {
		t.Fatalf("bundle_sha is %s, want %s", m.BundleSha, want)
	}
}

func TestOnlyTheRootBundleCloses(t *testing.T) {
	b := newTestBundle(t)
	wd, err := b.WorldDir(1)
	if err != nil {
		t.Fatal(err)
	}
	if err := wd.Close(); err == nil {
		t.Fatal("a world sub-bundle was allowed to close the run")
	}
}

func TestWorldDirOrdinalBounds(t *testing.T) {
	b := newTestBundle(t)
	for _, n := range []int{-1, 1000000} {
		if _, err := b.WorldDir(n); err == nil {
			t.Errorf("WorldDir(%d) was accepted", n)
		}
	}
	if _, err := b.WorldDir(999999); err != nil {
		t.Errorf("WorldDir(999999) failed: %v", err)
	}
}

func TestOpenBundleAtRejectsBadRunIDs(t *testing.T) {
	runs := filepath.Join(t.TempDir(), "runs")
	if _, err := OpenBundleAt(runs, "../escape"); err == nil {
		t.Fatal("OpenBundleAt accepted a path traversal")
	}
	if _, err := OpenBundleAt(runs, "r_2026_09_03_a41f"); err == nil {
		t.Fatal("OpenBundleAt accepted a run directory that does not exist")
	}
}

func TestAppenderIsCrashSafeAcrossReopen(t *testing.T) {
	b := newTestBundle(t)
	write := func(n int) {
		app, err := b.Appender("events.jsonl")
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < n; i++ {
			if err := app.AppendJSON(StreamAudit{Domain: StreamInjectDelay, Draws: uint64(i)}); err != nil {
				t.Fatal(err)
			}
		}
		if err := app.Close(); err != nil {
			t.Fatal(err)
		}
	}
	write(3)
	write(2)

	raw, err := b.ReadFile("events.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if len(lines) != 5 {
		t.Fatalf("got %d lines, want 5 (the appender must not truncate on reopen)", len(lines))
	}
	for i, ln := range lines {
		if err := cjson.Unmarshal(append([]byte(ln), '\n'), new(StreamAudit)); err != nil {
			t.Fatalf("line %d is not canonical: %v (%q)", i, err, ln)
		}
	}
}
