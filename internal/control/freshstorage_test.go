package control

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/WrdCstlg/pro-thesis/internal/harness"
	"github.com/WrdCstlg/pro-thesis/internal/recorder"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

func newFreshStorageRunner(t *testing.T, b *stubBackend, errOut *strings.Builder) *Runner {
	t.Helper()
	r, err := NewRunner(RunnerOptions{
		Config:        drainWorldConfig(t),
		ProjectDir:    t.TempDir(),
		Backend:       b,
		Driver:        &stubDriver{drainable: true},
		Telemetry:     stubTelemetry{},
		OracleEngine:  stubOracles{},
		SteadyState:   noSteadyState{},
		LogCollector:  stubLogs{},
		ImageResolver: stubImages{},
		DrainDeadline: 2 * time.Second,
		Stderr:        errOut,
		Quiet:         true,
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	return r
}

// A world records the fresh-storage check its BOOT made (OQ-087) in
// result.json, beside its compose project. The check lives on the topology,
// which no world file keeps; without this record nobody reading a world later
// can tell whether it booted on clean storage, cleaned storage or an unchecked
// project, and H6b cannot claim an empty initial state.
//
// It also says so on stderr, quiet or not: a removal is how volumes kept on
// purpose (`thesis down --keep-volumes`) disappear, and a record nobody is told
// about is found too late.
//
// Mutation: drop the storage entry from the runner's result document, or the
// stderr line, and this fails.
func TestAWorldRecordsItsFreshStorageCheckInResultJSON(t *testing.T) {
	leftover := "stub-proj_n1data"
	var errOut strings.Builder
	r := newFreshStorageRunner(t, &stubBackend{storage: &recorder.StorageCheck{
		Found: []string{leftover}, Removed: []string{leftover}, Fresh: true,
	}}, &errOut)
	if _, _, err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	res := readResultDoc(t, r, 1)
	st, _ := res["storage"].(map[string]any)
	if st == nil {
		t.Fatalf("result.json has no storage record, so the world's fresh-storage check is lost:\n%v", res)
	}
	found, _ := st["found"].([]any)
	removed, _ := st["removed"].([]any)
	if st["fresh"] != true || len(found) != 1 || found[0] != leftover || len(removed) != 1 || removed[0] != leftover {
		t.Fatalf("result.json storage = %v, want fresh, found and removed [%s]", st, leftover)
	}
	if !strings.Contains(errOut.String(), "removed 1 leftover volume") || !strings.Contains(errOut.String(), leftover) {
		t.Fatalf("BOOT removed %s without saying so on stderr (quiet mode):\n%s", leftover, errOut.String())
	}
}

// Up refuses a project that already has containers, because they belong to
// another world, a kept-up topology or a crashed run. The refusal protects
// nothing if the refused world's own teardown then sweeps that project by
// name, which is what the teardown stand-in did: it removed the containers
// Up had just declined to adopt, and their volumes with them.
//
// Mutation: drop the ErrProjectInUse check in runWorld's teardown and this
// fails on the Down call.
func TestARefusedBootDoesNotTearDownTheProjectItRefused(t *testing.T) {
	b := &stubBackend{upErr: fmt.Errorf("harness: compose project stub-proj already has 1 container(s): %w",
		harness.ErrProjectInUse)}
	var errOut strings.Builder
	r := newFreshStorageRunner(t, b, &errOut)
	_, code, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	b.mu.Lock()
	downs := append([]string(nil), b.downProject...)
	b.mu.Unlock()
	if len(downs) != 0 {
		t.Fatalf("the world BOOT refused because its project was in use then tore that project down (Down on %v): "+
			"the containers it declined to adopt, and their volumes, are gone", downs)
	}
	if !strings.Contains(errOut.String(), "teardown skipped") {
		t.Fatalf("the skipped teardown was not reported on stderr:\n%s", errOut.String())
	}
	if code != schema.ExitInconclusive {
		t.Fatalf("exit %d, want %d: a world that could not boot is a harness error, never a verdict", code,
			schema.ExitInconclusive)
	}
}

// Any other BOOT failure still sweeps the project by name: a project that
// survived a failed boot must not be left holding a bridge network. This is
// the behaviour the refusal above must not disturb.
func TestAnOrdinaryBootFailureIsStillSweptByName(t *testing.T) {
	b := &stubBackend{upErr: fmt.Errorf("compose up: exit status 1")}
	var errOut strings.Builder
	r := newFreshStorageRunner(t, b, &errOut)
	if _, _, err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	b.mu.Lock()
	downs := append([]string(nil), b.downProject...)
	b.mu.Unlock()
	if len(downs) != 1 {
		t.Fatalf("an ordinary BOOT failure ran Down %d times, want 1 (the sweep by name)", len(downs))
	}
}

// The stand-in topology a world tears down when BOOT produced none must carry
// the directory compose has to run in, absolute: Down runs compose there, and
// the runner may have been given a relative project directory.
//
// Mutation: drop ProjectDir from teardownStandIn and this fails.
func TestTheTeardownStandInCarriesAnAbsoluteProjectDir(t *testing.T) {
	r := newFreshStorageRunner(t, &stubBackend{}, &strings.Builder{})
	r.opts.ProjectDir = "."
	top := r.teardownStandIn(worldSpec{})
	if top == nil || !filepath.IsAbs(top.ProjectDir) {
		t.Fatalf("teardown stand-in = %+v, want an absolute project_dir", top)
	}
}
