package control

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// ---------------------------------------------------------------------------
// The drain vocabulary
// ---------------------------------------------------------------------------

// A DrainReport that could not tell its outcomes apart would be worse than none:
// the whole reason the drain is recorded is that "asked and finished", "asked
// and refused" and "never asked" license different readings of the same PASS.
func TestDrainOutcomesAreDistinctAndOnlyOneMeansDrained(t *testing.T) {
	seen := map[DrainOutcome]bool{}
	drained := 0
	for _, o := range AllDrainOutcomes {
		if o == "" {
			t.Fatalf("a drain outcome is the empty string; an unset DrainReport would be " +
				"indistinguishable from a real one")
		}
		if seen[o] {
			t.Fatalf("drain outcome %q is used twice", o)
		}
		seen[o] = true
		if (DrainReport{Outcome: o}).Drained() {
			drained++
		}
	}
	if drained != 1 {
		t.Fatalf("%d outcomes report Drained(); exactly one may, or a refused drain could be "+
			"read as a completed one", drained)
	}
	if !(DrainReport{Outcome: DrainCompleted}).Drained() {
		t.Fatalf("DrainCompleted must be the one that reports Drained()")
	}
	// The zero value is the state of a world that never got as far as QUIESCE.
	// It must not claim a drain.
	if (DrainReport{}).Drained() {
		t.Fatalf("the zero DrainReport claims the driver drained")
	}
}

func TestDrainReportDescribesEveryOutcome(t *testing.T) {
	for _, o := range AllDrainOutcomes {
		d := DrainReport{Outcome: o, Deadline: time.Second}
		if s := d.Describe(); s == "" || strings.Contains(s, "unrecognised") {
			t.Fatalf("Describe(%s) = %q; every declared outcome needs its own sentence", o, s)
		}
	}
	if s := (DrainReport{Outcome: "nonsense"}).Describe(); !strings.Contains(s, "not drained") {
		t.Fatalf("an undeclared outcome described as %q; it must fail closed", s)
	}
}

// The artifact has to say "asked and refused" without a reader inferring it.
func TestDrainDocCarriesTheRefusal(t *testing.T) {
	d := DrainReport{
		Outcome:  DrainDeadlineExceeded,
		Deadline: 10 * time.Second,
		Waited:   10 * time.Second,
		Err:      context.DeadlineExceeded,
	}
	doc := d.Doc()
	if doc["outcome"] != string(DrainDeadlineExceeded) {
		t.Fatalf("outcome = %v", doc["outcome"])
	}
	if doc["drained"] != false {
		t.Fatalf("drained = %v, want false", doc["drained"])
	}
	if doc["error"] == nil {
		t.Fatalf("a refused drain carried no error into the artifact")
	}
	if _, err := json.Marshal(doc); err != nil {
		t.Fatalf("the drain doc does not serialize: %v", err)
	}
}

// ---------------------------------------------------------------------------
// The lifecycle
// ---------------------------------------------------------------------------

// drainWorldConfig is a minimal config whose world crosses every seam without a
// Docker daemon.
func drainWorldConfig(t *testing.T) *schema.Config {
	t.Helper()
	return &schema.Config{
		Version: schema.ConfigVersion,
		Name:    "drain-test",
		Harness: schema.HarnessConfig{
			Backend: schema.BackendCompose,
			File:    "docker-compose.yaml",
			Nodes: []schema.NodeConfig{
				{ID: "n1", Service: "kv"},
			},
		},
		Driver: schema.DriverConfig{
			Cmd:      "echo test",
			Profiles: map[string]schema.DriverProfile{"smoke": {Clients: 1, Ops: 1}},
		},
		Profiles: map[string]schema.Profile{
			"smoke": {
				Budget:        schema.Duration(60 * time.Second),
				Worlds:        func() *int { w := 1; return &w }(),
				DriverProfile: "smoke",
			},
		},
		Oracles:   schema.OraclesConfig{Builtin: []schema.BuiltinOracle{}},
		Artifacts: schema.ArtifactsConfig{Dir: t.TempDir()},
	}
}

func newDrainRunner(t *testing.T, drv Driver, deadline time.Duration, stderr *strings.Builder) *Runner {
	t.Helper()
	r, err := NewRunner(RunnerOptions{
		Config:        drainWorldConfig(t),
		ProjectDir:    t.TempDir(),
		Backend:       &stubBackend{},
		Driver:        drv,
		Telemetry:     stubTelemetry{},
		OracleEngine:  stubOracles{},
		SteadyState:   noSteadyState{},
		LogCollector:  stubLogs{},
		ImageResolver: stubImages{},
		DrainDeadline: deadline,
		Stderr:        stderr,
		Quiet:         false,
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	return r
}

type noSteadyState struct{}

func (noSteadyState) Probe(context.Context, SteadyStateRequest) error { return nil }

// The happy path: QUIESCE asks, the driver finishes, and the run records it.
func TestQuiesceDrainsBeforeStoppingTheDriver(t *testing.T) {
	drv := &stubDriver{drainable: true}
	var errOut strings.Builder
	r := newDrainRunner(t, drv, 2*time.Second, &errOut)

	if _, _, err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	drv.mu.Lock()
	handles := append([]*stubDriverHandle(nil), drv.handles...)
	drv.mu.Unlock()
	if len(handles) != 1 {
		t.Fatalf("expected one driver, got %d", len(handles))
	}
	h := handles[0]
	h.mu.Lock()
	calls, rep := h.drainCall, h.drainRep
	h.mu.Unlock()

	if calls != 1 {
		t.Fatalf("QUIESCE called Drain %d times, want exactly 1", calls)
	}
	if !rep.Drained() {
		t.Fatalf("the cooperating driver was not recorded as drained: %+v", rep)
	}
	if !strings.Contains(errOut.String(), "QUIESCE") {
		t.Fatalf("the drain was not reported on stderr:\n%s", errOut.String())
	}
}

// The requirement this test exists for: "a driver that will not drain is
// stopped and that fact is RECORDED, not hidden."
func TestADriverThatRefusesToDrainIsStoppedAtTheDeadlineAndRecorded(t *testing.T) {
	drv := &stubDriver{drainable: false}
	var errOut strings.Builder
	// Short, because the point is the bound, not its default value.
	r := newDrainRunner(t, drv, 150*time.Millisecond, &errOut)

	start := time.Now()
	if _, _, err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	elapsed := time.Since(start)

	drv.mu.Lock()
	h := drv.handles[0]
	drv.mu.Unlock()
	h.mu.Lock()
	rep, stopped := h.drainRep, h.stopped
	h.mu.Unlock()

	if rep.Outcome != DrainDeadlineExceeded {
		t.Fatalf("a refusing driver recorded %s, want %s; anything else would let the world "+
			"claim its operations were given a chance to retire", rep.Outcome, DrainDeadlineExceeded)
	}
	if !stopped {
		t.Fatalf("the refusing driver was not stopped after the deadline; a hung driver must " +
			"never hang the run")
	}
	if elapsed > 20*time.Second {
		t.Fatalf("the run took %s; the drain deadline did not bound it", elapsed)
	}

	// RECORDED, in all three places a reader looks.
	if !strings.Contains(errOut.String(), "did not drain") {
		t.Fatalf("the refusal was not reported on stderr:\n%s", errOut.String())
	}
	res := readResultDoc(t, r, 1)
	drvDoc, _ := res["driver"].(map[string]any)
	if drvDoc == nil {
		t.Fatalf("result.json has no driver record:\n%v", res)
	}
	drainDoc, _ := drvDoc["drain"].(map[string]any)
	if drainDoc == nil {
		t.Fatalf("result.json has no drain record:\n%v", drvDoc)
	}
	if drainDoc["drained"] != false || drainDoc["outcome"] != string(DrainDeadlineExceeded) {
		t.Fatalf("result.json hid the refusal: %v", drainDoc)
	}
}

// The refusal must ALSO be visible in quiet mode. It changes what the verdict
// means; it is not a progress message.
func TestARefusedDrainIsReportedEvenWhenQuiet(t *testing.T) {
	drv := &stubDriver{drainable: false}
	var errOut strings.Builder
	r := newDrainRunner(t, drv, 100*time.Millisecond, &errOut)
	r.quiet = true

	if _, _, err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(errOut.String(), "did not drain") {
		t.Fatalf("quiet mode suppressed a refused drain:\n%s", errOut.String())
	}
}

// A driver with no control channel is not the same fact as one that refused.
func TestADriverWithNoControlChannelIsRecordedAsUnsupported(t *testing.T) {
	drv := &stubDriver{unsupported: true}
	var errOut strings.Builder
	r := newDrainRunner(t, drv, time.Second, &errOut)

	if _, _, err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	drv.mu.Lock()
	h := drv.handles[0]
	drv.mu.Unlock()
	h.mu.Lock()
	rep := h.drainRep
	h.mu.Unlock()
	if rep.Outcome != DrainUnsupported {
		t.Fatalf("outcome = %s, want %s", rep.Outcome, DrainUnsupported)
	}
	// The ARTIFACT must carry it too. A search reading result.json is the
	// consumer that matters, and it cannot see the handle.
	drainDoc := readDrainDoc(t, r, 1)
	if drainDoc["outcome"] != string(DrainUnsupported) || drainDoc["drained"] != false {
		t.Fatalf("result.json did not record the missing control channel: %v", drainDoc)
	}
}

func readDrainDoc(t *testing.T, r *Runner, ordinal int) map[string]any {
	t.Helper()
	res := readResultDoc(t, r, ordinal)
	drvDoc, _ := res["driver"].(map[string]any)
	if drvDoc == nil {
		t.Fatalf("result.json has no driver record: %v", res)
	}
	doc, _ := drvDoc["drain"].(map[string]any)
	if doc == nil {
		t.Fatalf("result.json has no drain record: %v", drvDoc)
	}
	return doc
}

// A negative deadline restores the pre-drain lifecycle exactly, and says so
// rather than pretending a drain happened.
func TestANegativeDeadlineDisablesTheDrainWithoutClaimingOne(t *testing.T) {
	drv := &stubDriver{drainable: true}
	var errOut strings.Builder
	r := newDrainRunner(t, drv, -1, &errOut)

	if _, _, err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	reqs := drv.requests()
	drv.mu.Lock()
	h := drv.handles[0]
	drv.mu.Unlock()
	h.mu.Lock()
	calls, rep, stopped := h.drainCall, h.drainRep, h.stopped
	h.mu.Unlock()

	if calls != 0 {
		t.Fatalf("Drain was called %d times with draining disabled", calls)
	}
	if rep.Drained() {
		t.Fatalf("a disabled drain reported as drained")
	}
	if !stopped {
		t.Fatalf("the driver was not stopped at QUIESCE")
	}
	if reqs[0].StdinControl {
		t.Fatalf("a control channel was requested with draining disabled; the driver would " +
			"get a pipe nobody ever writes to")
	}
}

// Draining is on by default and asks for the channel it needs.
func TestDrainingIsOnByDefaultAndRequestsTheControlChannel(t *testing.T) {
	drv := &stubDriver{drainable: true}
	var errOut strings.Builder
	r := newDrainRunner(t, drv, 0, &errOut)
	if r.drainDeadline != DefaultDrainDeadline {
		t.Fatalf("drainDeadline = %s, want the default %s", r.drainDeadline, DefaultDrainDeadline)
	}
	if _, _, err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if reqs := drv.requests(); len(reqs) == 0 || !reqs[0].StdinControl {
		t.Fatalf("the driver was launched without a control channel, so it could never drain")
	}
}

// The deadline must exceed the SLO ceiling no_stuck_op measures against.
//
// A shorter one would stop observing before the oracle's own threshold could be
// reached, so an operation that is genuinely stuck would still be reported as
// merely in flight, which is the defect being fixed, reintroduced with extra
// steps.
func TestTheDefaultDrainDeadlineOutlastsTheStuckOpSLO(t *testing.T) {
	const sloCeiling = 5 * time.Second // oracle.DefaultStuckOpSLO
	if DefaultDrainDeadline <= sloCeiling {
		t.Fatalf("DefaultDrainDeadline (%s) does not outlast the no_stuck_op SLO ceiling (%s); "+
			"a drain that gives up first cannot expose a stuck operation, only hide it",
			DefaultDrainDeadline, sloCeiling)
	}
}

func readResultDoc(t *testing.T, r *Runner, ordinal int) map[string]any {
	t.Helper()
	paths := NewWorldPaths(joinRunWorld(r, ordinal))
	data, err := os.ReadFile(paths.Result)
	if err != nil {
		t.Fatalf("read result.json: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parse result.json: %v", err)
	}
	return doc
}
