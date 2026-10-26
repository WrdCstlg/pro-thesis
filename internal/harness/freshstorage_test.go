package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/WrdCstlg/pro-thesis/internal/recorder"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// The tests in this file are OQ-087's: a world must boot on fresh storage, and
// teardown must not leave a world's volumes behind without saying so.
//
// They run the compose backend against a STAND-IN docker: this test binary,
// re-executed with docker's arguments. TestMain dispatches to it before the
// testing package parses flags, because production code passes docker's own
// arguments ("compose", "--project-name", ...), not a -test.run selector. The
// stand-in keeps the engine's state in a JSON file and reproduces the two
// behaviours the defect lives in: compose resolves each --file against its own
// working directory and fails when one is missing, and a project's named
// volumes outlive its containers.

const (
	stubDockerMode  = "stub-docker"
	stubStateEnvVar = "PROTHESIS_STUB_DOCKER_STATE"
	stubLogEnvVar   = "PROTHESIS_STUB_DOCKER_LOG"
)

func TestMain(m *testing.M) {
	if os.Getenv(helperEnvVar) == stubDockerMode {
		os.Exit(stubDockerMain(os.Args[1:]))
	}
	os.Exit(m.Run())
}

// stubState is the stand-in engine, for one compose project.
type stubState struct {
	Project    string   `json:"project"`
	Services   []string `json:"services"`
	Containers []string `json:"containers"`
	Volumes    []string `json:"volumes"`
	// UpVolumes are the named volumes `compose up` creates when absent.
	UpVolumes []string `json:"up_volumes"`
	// Stuck are volumes `volume rm` refuses to remove.
	Stuck []string `json:"stuck"`
	// VolumeLsCalls counts `volume ls` invocations; once it exceeds a positive
	// VolumeLsFailAfter, every further `volume ls` fails, as a daemon that stops
	// answering mid-teardown does.
	VolumeLsCalls     int `json:"volume_ls_calls"`
	VolumeLsFailAfter int `json:"volume_ls_fail_after"`
	// PsAllFail fails every `ps --all` listing (the label sweep and its
	// verification; the paused-container query has no --all). NetLsFail fails
	// every `network ls`.
	PsAllFail bool `json:"ps_all_fail"`
	NetLsFail bool `json:"net_ls_fail"`
}

// stubCall is one invocation of the stand-in, as it saw it.
type stubCall struct {
	Dir  string   `json:"dir"`
	Args []string `json:"args"`
	Exit int      `json:"exit"`
}

func stubDockerMain(args []string) int {
	statePath, logPath := os.Getenv(stubStateEnvVar), os.Getenv(stubLogEnvVar)
	var st stubState
	if data, err := os.ReadFile(statePath); err == nil {
		_ = json.Unmarshal(data, &st)
	}
	dir, _ := os.Getwd()
	code := stubDocker(&st, dir, args)
	data, _ := json.Marshal(st)
	_ = os.WriteFile(statePath, data, 0o644)
	line, _ := json.Marshal(stubCall{Dir: dir, Args: args, Exit: code})
	if f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
		_, _ = f.Write(append(line, '\n'))
		_ = f.Close()
	}
	return code
}

func stubDocker(st *stubState, dir string, args []string) int {
	label := "label=com.docker.compose.project=" + st.Project
	has := func(s string) bool { return slices.Contains(args, s) }
	switch {
	case len(args) > 0 && args[0] == "info":
		fmt.Println("29.1.3")
		return 0
	case len(args) > 0 && args[0] == "ps":
		if st.PsAllFail && has("--all") {
			fmt.Fprintln(os.Stderr, "Error response from daemon: the daemon stopped answering")
			return 1
		}
		if has("status=paused") || !has(label) {
			return 0
		}
		for _, c := range st.Containers {
			fmt.Println(c)
		}
		return 0
	case len(args) > 0 && args[0] == "rm":
		st.Containers = slices.DeleteFunc(st.Containers, func(c string) bool { return has(c) })
		return 0
	case len(args) > 1 && args[0] == "network":
		if st.NetLsFail && args[1] == "ls" {
			fmt.Fprintln(os.Stderr, "Error response from daemon: the daemon stopped answering")
			return 1
		}
		return 0 // the project creates no networks the tests look at
	case len(args) > 1 && args[0] == "volume" && args[1] == "ls":
		st.VolumeLsCalls++
		if st.VolumeLsFailAfter > 0 && st.VolumeLsCalls > st.VolumeLsFailAfter {
			fmt.Fprintln(os.Stderr, "Error response from daemon: the daemon stopped answering")
			return 1
		}
		if has(label) {
			for _, v := range st.Volumes {
				fmt.Println(v)
			}
		}
		return 0
	case len(args) > 1 && args[0] == "volume" && args[1] == "rm":
		code := 0
		for _, v := range args[2:] {
			if slices.Contains(st.Stuck, v) {
				fmt.Fprintf(os.Stderr, "Error response from daemon: remove %s: volume is in use\n", v)
				code = 1
				continue
			}
			st.Volumes = slices.DeleteFunc(st.Volumes, func(x string) bool { return x == v })
		}
		return code
	case len(args) > 0 && args[0] == "compose":
		return stubCompose(st, dir, args[1:])
	}
	fmt.Fprintf(os.Stderr, "stub docker: unhandled %q\n", args)
	return 2
}

func stubCompose(st *stubState, dir string, args []string) int {
	var files, sub []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--project-name":
			i++
		case "--file":
			i++
			files = append(files, args[i])
		default:
			sub = args[i:]
			i = len(args)
		}
	}
	// What compose does: every --file resolves against the working directory.
	for _, f := range files {
		p := f
		if !filepath.IsAbs(p) {
			p = filepath.Join(dir, f)
		}
		if _, err := os.Stat(p); err != nil {
			fmt.Fprintf(os.Stderr, "open %s: The system cannot find the file specified.\n", p)
			return 1
		}
	}
	switch sub[0] {
	case "up":
		st.Containers = nil
		for _, s := range st.Services {
			st.Containers = append(st.Containers, "c-"+s)
		}
		for _, v := range st.UpVolumes {
			if !slices.Contains(st.Volumes, v) {
				st.Volumes = append(st.Volumes, v)
			}
		}
	case "ps":
		for _, s := range st.Services {
			fmt.Printf(`{"ID":"c-%s","Name":"%s","Service":"%s","State":"running"}`+"\n", s, s, s)
		}
	case "port":
		fmt.Println("0.0.0.0:18081")
	case "down":
		st.Containers = nil
		if slices.Contains(sub, "--volumes") {
			st.Volumes = slices.DeleteFunc(st.Volumes, func(v string) bool { return !slices.Contains(st.Stuck, v) })
		}
	}
	return 0
}

// stubWorld is one test's project directory, config and stand-in engine.
type stubWorld struct {
	t          *testing.T
	projectDir string
	runDir     string
	cfg        *schema.Config
	project    string
	statePath  string
	logPath    string
}

// newStubWorld points dockerBin at the stand-in and gives it a project whose
// compose file exists ONLY in the project directory, never in the directory the
// test process runs in: the condition under which OQ-087 leaked.
func newStubWorld(t *testing.T, mutate func(*stubState)) *stubWorld {
	t.Helper()
	w := &stubWorld{t: t, projectDir: t.TempDir(), runDir: t.TempDir()}
	if err := os.WriteFile(filepath.Join(w.projectDir, "docker-compose.yaml"), []byte("services: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat("docker-compose.yaml"); err == nil {
		t.Fatal("the test's own working directory has a docker-compose.yaml; the test needs one that does not")
	}
	w.cfg = &schema.Config{
		Name: "stubbed",
		Harness: schema.HarnessConfig{
			Backend: schema.BackendCompose,
			File:    "docker-compose.yaml",
			Nodes:   []schema.NodeConfig{{ID: "n1", Service: "kv", ComposeService: "kv-n1"}},
		},
	}
	w.project = ProjectName(w.cfg, w.projectDir)
	st := stubState{Project: w.project, Services: []string{"kv-n1"}, UpVolumes: []string{w.project + "_n1data"}}
	if mutate != nil {
		mutate(&st)
	}
	stateDir := t.TempDir()
	w.statePath, w.logPath = filepath.Join(stateDir, "state.json"), filepath.Join(stateDir, "calls.jsonl")
	data, _ := json.Marshal(st)
	if err := os.WriteFile(w.statePath, data, 0o644); err != nil {
		t.Fatal(err)
	}

	old := dockerBin
	dockerBin = os.Args[0]
	t.Cleanup(func() { dockerBin = old })
	t.Setenv(helperEnvVar, stubDockerMode)
	t.Setenv(stubStateEnvVar, w.statePath)
	t.Setenv(stubLogEnvVar, w.logPath)
	return w
}

// up boots the project the way the runner does, from a RELATIVE project
// directory: the Docker-backed tests launch from their package directory and
// name the fixture as ../../testdata/kvfixture.
func (w *stubWorld) up() (*recorder.Topology, error) {
	w.t.Helper()
	return w.upWith(w.runDir)
}

// upWith is up with the given run directory, which may itself be relative.
func (w *stubWorld) upWith(runDir string) (*recorder.Topology, error) {
	w.t.Helper()
	return NewCompose().Up(context.Background(), UpRequest{
		Config: w.cfg, ProjectDir: w.rel(w.projectDir), RunID: "r_2026_10_23_0b03", RunDir: runDir, SkipHealth: true,
	})
}

// rel is path relative to the test's working directory, the form the
// Docker-backed tests use. Windows cannot express a path on another drive
// relatively; then the absolute path is used, which still runs every test that
// does not depend on the relative form (the wrong-directory teardown happens
// with an absolute project directory too) instead of skipping them.
func (w *stubWorld) rel(path string) string {
	w.t.Helper()
	cwd, err := os.Getwd()
	if err != nil {
		w.t.Fatal(err)
	}
	rel, err := filepath.Rel(cwd, path)
	if err != nil {
		w.t.Logf("%s cannot be named relative to %s (another drive?): %v; using it absolute", path, cwd, err)
		return path
	}
	return rel
}

func (w *stubWorld) state() stubState {
	w.t.Helper()
	data, err := os.ReadFile(w.statePath)
	if err != nil {
		w.t.Fatal(err)
	}
	var st stubState
	if err := json.Unmarshal(data, &st); err != nil {
		w.t.Fatal(err)
	}
	return st
}

func (w *stubWorld) setState(mutate func(*stubState)) {
	w.t.Helper()
	st := w.state()
	mutate(&st)
	data, _ := json.Marshal(st)
	if err := os.WriteFile(w.statePath, data, 0o644); err != nil {
		w.t.Fatal(err)
	}
}

func (w *stubWorld) calls() []stubCall {
	w.t.Helper()
	data, err := os.ReadFile(w.logPath)
	if err != nil {
		return nil
	}
	var out []stubCall
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var c stubCall
		if err := json.Unmarshal([]byte(line), &c); err != nil {
			w.t.Fatalf("stand-in log: %v", err)
		}
		out = append(out, c)
	}
	return out
}

// composeCall returns the index and record of the first `compose ... <sub>`
// invocation, or -1.
func composeCall(calls []stubCall, sub string) (int, stubCall) {
	for i, c := range calls {
		if len(c.Args) > 0 && c.Args[0] == "compose" && slices.Contains(c.Args, sub) {
			return i, c
		}
	}
	return -1, stubCall{}
}

func volumeRmCalls(calls []stubCall) []stubCall {
	var out []stubCall
	for _, c := range calls {
		if len(c.Args) > 1 && c.Args[0] == "volume" && c.Args[1] == "rm" {
			out = append(out, c)
		}
	}
	return out
}

// `down` must run compose from the directory `up` ran it from. Up runs in the
// project directory; Down ran in filepath.Dir of the RECORDED compose file,
// which is the bare name prothesis.yaml gives, so "." and the caller's working
// directory. From anywhere else compose cannot find the file, and the world's
// volumes stayed behind with no error (OQ-087).
//
// Mutation: restore `dir := filepath.Dir(top.ComposeFile)` in Down, or stop
// recording ProjectDir in Up, and this fails on the directory.
func TestDownRunsComposeFromTheDirectoryUpUsed(t *testing.T) {
	w := newStubWorld(t, nil)
	top, err := w.up()
	if err != nil {
		t.Fatalf("Up: %v", err)
	}
	if !filepath.IsAbs(top.ProjectDir) || !sameDir(top.ProjectDir, w.projectDir) {
		t.Fatalf("topology project_dir = %q, want the absolute project directory %q: a later `down`, "+
			"possibly in another process, has no other way to find the compose file", top.ProjectDir, w.projectDir)
	}
	if err := NewCompose().Down(context.Background(), top, DownOptions{Volumes: true}); err != nil {
		t.Fatalf("Down: %v", err)
	}
	_, down := composeCall(w.calls(), "down")
	if !sameDir(down.Dir, w.projectDir) || down.Exit != 0 {
		t.Fatalf("compose down ran in %q (exit %d), want the project directory %q, where `up` ran it",
			down.Dir, down.Exit, w.projectDir)
	}
	if v := w.state().Volumes; len(v) != 0 {
		t.Fatalf("after Down(volumes) the project still has volumes %v: the next world of this project "+
			"boots on them", v)
	}
}

// When compose cannot address the project at all (the file was moved or
// deleted since `up`), the label sweep is the backstop. It removed containers
// and networks but never volumes, and Down then returned nil (OQ-087).
//
// Mutation: drop the volume pass from sweep and this fails.
func TestTheTeardownSweepRemovesTheProjectsVolumesWhenComposeCannot(t *testing.T) {
	w := newStubWorld(t, nil)
	top, err := w.up()
	if err != nil {
		t.Fatalf("Up: %v", err)
	}
	if err := os.Remove(filepath.Join(w.projectDir, "docker-compose.yaml")); err != nil {
		t.Fatal(err)
	}
	err = NewCompose().Down(context.Background(), top, DownOptions{Volumes: true})
	if v := w.state().Volumes; len(v) != 0 {
		t.Fatalf("compose could not run, and the sweep left the project's volumes %v behind (Down "+
			"returned %v)", v, err)
	}
	if err != nil {
		t.Fatalf("Down returned %v although the sweep removed everything", err)
	}
}

// A volume the sweep cannot remove is residue, and residue is an error: a
// `down` that reports success over it is the silent leak itself.
//
// Mutation: drop the volume count from the sweep's verification and this fails.
func TestDownReportsAVolumeItCouldNotRemove(t *testing.T) {
	w := newStubWorld(t, nil)
	top, err := w.up()
	if err != nil {
		t.Fatalf("Up: %v", err)
	}
	stuck := w.project + "_n1data"
	w.setState(func(st *stubState) { st.Stuck = []string{stuck} })
	err = NewCompose().Down(context.Background(), top, DownOptions{Volumes: true})
	if err == nil || !strings.Contains(err.Error(), "volume(s)") {
		t.Fatalf("Down returned %v with volume %s still present; want an error that names the volume residue",
			err, stuck)
	}
}

// A verification that could not be made is not a clean result. With a volume
// that will not go and a daemon that stops answering `volume ls` when the sweep
// checks, Down reported success, and `thesis down` would print "torn down"
// over the residue.
//
// Mutation: restore `err == nil && len(vols) > 0` in the verification and this
// fails.
func TestDownTreatsAListingItCouldNotMakeAsResidue(t *testing.T) {
	w := newStubWorld(t, nil)
	top, err := w.up()
	if err != nil {
		t.Fatalf("Up: %v", err)
	}
	stuck := w.project + "_n1data"
	w.setState(func(st *stubState) {
		st.Stuck = []string{stuck}
		// The sweep's own listing still answers; the verification after it does not.
		st.VolumeLsFailAfter = st.VolumeLsCalls + 1
	})
	err = NewCompose().Down(context.Background(), top, DownOptions{Volumes: true})
	if err == nil || !strings.Contains(err.Error(), "could not verify") {
		t.Fatalf("Down returned %v although it could not list the project's volumes to verify them and %s "+
			"is still there; want an error saying the teardown could not be verified", err, stuck)
	}
}

// The same holds for containers and networks: a listing that fails during the
// verification is a teardown nobody verified, whatever kind of resource it was.
//
// Mutation: ignore listing errors for containers or networks in the
// verification and the matching subtest fails.
func TestDownTreatsAnUnlistableContainerOrNetworkAsUnverified(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(*stubState)
		want string
	}{
		{"containers", func(st *stubState) { st.PsAllFail = true }, "containers"},
		{"networks", func(st *stubState) { st.NetLsFail = true }, "networks"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newStubWorld(t, nil)
			top, err := w.up()
			if err != nil {
				t.Fatalf("Up: %v", err)
			}
			w.setState(tc.set)
			err = NewCompose().Down(context.Background(), top, DownOptions{Volumes: true})
			if err == nil || !strings.Contains(err.Error(), "could not verify") || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Down returned %v although it could not list the project's %s to verify them; "+
					"want an error saying so", err, tc.want)
			}
		})
	}
}

// A run directory given relative to the caller is written relative to the
// caller, so compose, which runs in the project directory, must be handed it
// absolute. Otherwise it resolves the overlay against the project directory and
// cannot find it. Production callers pass absolute paths today; this keeps a
// relative one from breaking BOOT.
//
// Mutation: drop the filepath.Abs in writeOverlay and this fails.
func TestUpAcceptsARelativeRunDirectory(t *testing.T) {
	w := newStubWorld(t, nil)
	top, err := w.upWith(w.rel(w.runDir))
	if err != nil {
		t.Fatalf("Up with a relative run directory: %v", err)
	}
	if !filepath.IsAbs(top.OverlayFile) {
		t.Fatalf("topology overlay_file = %q, want an absolute path a later `down` can find from anywhere", top.OverlayFile)
	}
}

// `thesis down --keep-volumes` asks for the volumes to stay. The volume sweep
// must honour that. A guard against over-correction, not a failing-first test:
// it passes before and after the fix.
func TestDownWithoutVolumesKeepsThem(t *testing.T) {
	w := newStubWorld(t, nil)
	top, err := w.up()
	if err != nil {
		t.Fatalf("Up: %v", err)
	}
	if err := NewCompose().Down(context.Background(), top, DownOptions{Volumes: false}); err != nil {
		t.Fatalf("Down: %v", err)
	}
	if rm := volumeRmCalls(w.calls()); len(rm) != 0 {
		t.Fatalf("Down without volumes ran %d `volume rm`: %v", len(rm), rm[0].Args)
	}
	if v := w.state().Volumes; len(v) != 1 {
		t.Fatalf("Down without volumes left %v, want the project's one volume kept", v)
	}
}

// BOOT on a project that still has volumes boots on an earlier world's data;
// the fixture replays its Raft log from them (OQ-087, OQ-090; P2's HC-01 seed 1
// did exactly this). Up removes them BEFORE `compose up` and records that it
// did, so a reader can tell a clean project from a cleaned one.
//
// Mutation: drop the pre-up check and this fails on the record and the order.
func TestUpRemovesLeftoverVolumesBeforeBootingAndRecordsThem(t *testing.T) {
	var leftover string
	w := newStubWorld(t, func(st *stubState) {
		leftover = st.Project + "_n1data"
		st.Volumes = []string{leftover}
	})
	top, err := w.up()
	if err != nil {
		t.Fatalf("Up: %v", err)
	}
	calls := w.calls()
	upAt, _ := composeCall(calls, "up")
	rmAt := -1
	for i, c := range calls {
		if len(c.Args) > 2 && c.Args[0] == "volume" && c.Args[1] == "rm" && slices.Contains(c.Args, leftover) {
			rmAt = i
			break
		}
	}
	if rmAt < 0 || upAt < 0 || rmAt > upAt {
		t.Fatalf("leftover volume %s: `volume rm` at call %d, `compose up` at call %d; want the removal "+
			"before the boot", leftover, rmAt, upAt)
	}
	want := recorder.StorageCheck{Found: []string{leftover}, Removed: []string{leftover}, Fresh: true}
	if top.Storage == nil || !slices.Equal(top.Storage.Found, want.Found) ||
		!slices.Equal(top.Storage.Removed, want.Removed) || !top.Storage.Fresh {
		t.Fatalf("topology storage = %+v, want %+v", top.Storage, want)
	}
}

// A clean project is recorded as checked and fresh, with nothing found: the
// claim a later reader (H6b's empty initial state) is entitled to rely on.
//
// Mutation: record Storage only when something was found and this fails.
func TestUpRecordsACleanProjectAsFresh(t *testing.T) {
	w := newStubWorld(t, nil)
	top, err := w.up()
	if err != nil {
		t.Fatalf("Up: %v", err)
	}
	if top.Storage == nil || !top.Storage.Fresh || len(top.Storage.Found) != 0 || top.Storage.Found == nil {
		t.Fatalf("topology storage = %+v, want checked, fresh, found = [] (a list, not null)", top.Storage)
	}
}

// A leftover volume Up cannot remove means the world cannot boot fresh. Up
// refuses rather than booting on it.
//
// Mutation: ignore the failed `volume rm` and this fails.
func TestUpRefusesToBootOnALeftoverVolumeItCannotRemove(t *testing.T) {
	var leftover string
	w := newStubWorld(t, func(st *stubState) {
		leftover = st.Project + "_n1data"
		st.Volumes = []string{leftover}
		st.Stuck = []string{leftover}
	})
	top, err := w.up()
	if err == nil || !strings.Contains(err.Error(), leftover) {
		t.Fatalf("Up returned (%v, %v) with %s still present; want a refusal naming it", top, err, leftover)
	}
	if at, _ := composeCall(w.calls(), "up"); at >= 0 {
		t.Fatalf("Up ran `compose up` (call %d) on a project that still had volume %s", at, leftover)
	}
}

// Containers already in the project are another world, a kept-up topology or a
// crashed run. `compose up` would adopt them and boot on their state. Up
// refuses, and touches nothing: they are not its to remove.
//
// Mutation: drop the container check and this fails.
func TestUpRefusesAProjectThatAlreadyHasContainers(t *testing.T) {
	w := newStubWorld(t, func(st *stubState) { st.Containers = []string{"c-earlier"} })
	_, err := w.up()
	if err == nil || !strings.Contains(err.Error(), "container") {
		t.Fatalf("Up returned %v on a project that already had a container; want a refusal", err)
	}
	// Typed, so the caller can tell this refusal from any other BOOT failure and
	// leave the project alone at teardown too (the runner's half of the property).
	if !errors.Is(err, ErrProjectInUse) {
		t.Fatalf("Up's refusal %v does not wrap ErrProjectInUse; the runner cannot tell it apart and will tear "+
			"the project down", err)
	}
	if at, _ := composeCall(w.calls(), "up"); at >= 0 {
		t.Fatalf("Up ran `compose up` (call %d) and adopted the earlier container", at)
	}
	if c := w.state().Containers; !slices.Equal(c, []string{"c-earlier"}) {
		t.Fatalf("Up removed or changed the containers it refused to adopt: %v", c)
	}
}

func sameDir(a, b string) bool {
	ra, err1 := filepath.EvalSymlinks(a)
	rb, err2 := filepath.EvalSymlinks(b)
	if err1 != nil || err2 != nil {
		return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
	}
	return strings.EqualFold(filepath.Clean(ra), filepath.Clean(rb))
}
