package perturber

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/WrdCstlg/pro-thesis/internal/recorder"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// ---------------------------------------------------------------------------
// a fake injector
//
// It records what it was asked to do, in order, and can be made to fail on cue.
// Everything the executor claims about ordering, withdrawal and recording is
// checkable through it without a Docker daemon.
// ---------------------------------------------------------------------------

type fakeInjector struct {
	kinds []schema.FaultKind

	mu       sync.Mutex
	calls    []string
	live     map[string]bool // fault ids currently injected, by this injector
	seeds    map[string]uint64
	nodes    map[string][]string
	injectAt map[string]int64

	injectErr   map[string]error
	withdrawErr map[string]error
	residues    []Residue
	verifyErr   error
	verifyCalls int

	// onInject fires after a successful injection, inside the executor's
	// sequencing goroutine. It is how a test cancels mid-schedule.
	onInject func(faultID string)
}

func newFakeInjector(kinds ...schema.FaultKind) *fakeInjector {
	if len(kinds) == 0 {
		kinds = []schema.FaultKind{schema.FaultProcPause, schema.FaultNetPartition, schema.FaultProcKill}
	}
	return &fakeInjector{
		kinds:       kinds,
		live:        map[string]bool{},
		seeds:       map[string]uint64{},
		nodes:       map[string][]string{},
		injectAt:    map[string]int64{},
		injectErr:   map[string]error{},
		withdrawErr: map[string]error{},
	}
}

func (f *fakeInjector) Kinds() []schema.FaultKind { return f.kinds }

func (f *fakeInjector) Inject(_ context.Context, req InjectRequest) error {
	f.mu.Lock()
	if err := f.injectErr[req.FaultID]; err != nil {
		f.calls = append(f.calls, "inject-failed "+req.FaultID)
		f.mu.Unlock()
		return err
	}
	f.calls = append(f.calls, "inject "+req.FaultID)
	f.live[req.FaultID] = true
	f.seeds[req.FaultID] = req.Seed
	f.nodes[req.FaultID] = nodeIDs(req.Nodes)
	f.injectAt[req.FaultID] = req.AtMS
	hook := f.onInject
	f.mu.Unlock()
	if hook != nil {
		hook(req.FaultID)
	}
	return nil
}

func (f *fakeInjector) Withdraw(_ context.Context, req WithdrawRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.withdrawErr[req.FaultID]; err != nil {
		f.calls = append(f.calls, "withdraw-failed "+req.FaultID)
		return err
	}
	f.calls = append(f.calls, "withdraw "+req.FaultID)
	delete(f.live, req.FaultID)
	return nil
}

func (f *fakeInjector) VerifyClean(_ context.Context, _ VerifyRequest) ([]Residue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.verifyCalls++
	if f.verifyErr != nil {
		return nil, f.verifyErr
	}
	out := append([]Residue(nil), f.residues...)
	for id := range f.live {
		out = append(out, Residue{NodeID: "?", Mechanism: "fake", FaultID: id, Detail: "still injected"})
	}
	return out, nil
}

func (f *fakeInjector) log() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeInjector) liveIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.live))
	for id := range f.live {
		out = append(out, id)
	}
	return out
}

var _ Injector = (*fakeInjector)(nil)

// ---------------------------------------------------------------------------
// harness
// ---------------------------------------------------------------------------

// newExec wires an executor over a SimClock whose DRIVE origin is already
// stamped, so virtual time exists and the schedule can be walked instantly.
func newExec(t *testing.T, planned []string, inj Injector, roles RoleObserver) (*Executor, *recorder.SimClock) {
	t.Helper()
	cfg := testConfig(t, nil, nil)
	top := kvTopology(t)
	reg, err := NewRegistry(inj)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	sched, err := Compile(CompileOptions{Config: cfg, Topology: top, Planned: planned, Registry: reg})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	clock := recorder.NewSimClock(time.Unix(1788764420, 0))
	tl := &recorder.Timeline{}
	if err := tl.StartDriveAt(clock.NowR(), clock.NowWall().UnixNano()); err != nil {
		t.Fatalf("StartDriveAt: %v", err)
	}
	res, err := NewResolver(top, roles)
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	e, err := NewExecutor(ExecutorOptions{
		RunID:    "r_2026_09_07_a41f",
		Schedule: sched,
		Topology: top,
		Resolver: res,
		Registry: reg,
		Clock:    clock,
		Timeline: tl,
		Seed:     recorder.Seed(90210),
	})
	if err != nil {
		t.Fatalf("NewExecutor: %v", err)
	}
	return e, clock
}

// ---------------------------------------------------------------------------
// tests
// ---------------------------------------------------------------------------

func TestExecutorInjectsAndWithdrawsInScheduleOrder(t *testing.T) {
	inj := newFakeInjector()
	e, _ := newExec(t, []string{
		"proc.pause(kv-n1)@8200..11000",
		"net.partition(kv-n2)@8400..10900",
	}, inj, nil)

	res, err := e.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Canceled {
		t.Error("Run reported cancellation on a clean run")
	}

	want := []string{"inject f001", "inject f002", "withdraw f002", "withdraw f001"}
	if got := inj.log(); !reflect.DeepEqual(got, want) {
		t.Errorf("call order = %v, want %v", got, want)
	}
	if live := inj.liveIDs(); len(live) != 0 {
		t.Errorf("faults still injected after Run: %v", live)
	}
}

// The executor must not fire a goroutine per event. If it did, the order below
// would depend on the Go scheduler rather than on the compiled sequence.
func TestExecutorSequencingIsSingleThreaded(t *testing.T) {
	inj := newFakeInjector()
	e, _ := newExec(t, []string{
		"proc.pause(kv-n1)@100..400",
		"net.partition(kv-n2)@200..300",
		"proc.pause(kv-n3)@250..500",
	}, inj, nil)
	if _, err := e.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	want := []string{
		"inject f001",   // 100
		"inject f002",   // 200
		"inject f003",   // 250
		"withdraw f002", // 300
		"withdraw f001", // 400
		"withdraw f003", // 500
	}
	if got := inj.log(); !reflect.DeepEqual(got, want) {
		t.Errorf("call order = %v\nwant %v", got, want)
	}
}

func TestExecutorRecordsWhatActuallyHappened(t *testing.T) {
	inj := newFakeInjector()
	roles := fakeRoleObserver{obs: []RoleObservation{
		{NodeID: "kv-n1", Role: "follower", Term: 3},
		{NodeID: "kv-n2", Role: "leader", Term: 3},
		{NodeID: "kv-n3", Role: "follower", Term: 3},
	}}
	e, _ := newExec(t, []string{
		"proc.pause(role:leader)@8200..11000",
		"net.partition(minority(kv))@8400..10900",
	}, inj, roles)

	if _, err := e.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := e.Heal(context.Background()); err != nil {
		t.Fatalf("Heal: %v", err)
	}

	realized := e.Realized()
	if len(realized) != 2 {
		t.Fatalf("realized %d faults, want 2: %+v", len(realized), realized)
	}

	// The dynamic target is PINNED to the node it actually bound to. This is
	// what makes a world replay: on a re-run a different node may be leader.
	leader := realized[0]
	if leader.Fault != "proc.pause(role:leader)@8200..11000" {
		t.Errorf("Fault = %q", leader.Fault)
	}
	if leader.Resolved != "proc.pause(kv-n2)@8200..11000" {
		t.Errorf("Resolved = %q, want the concrete node the role bound to", leader.Resolved)
	}
	if !reflect.DeepEqual(leader.Nodes, []string{"kv-n2"}) {
		t.Errorf("Nodes = %v, want [kv-n2]", leader.Nodes)
	}
	if leader.StartMS != 8200 || leader.EndMS != 11000 {
		t.Errorf("realized window = %d..%d, want 8200..11000", leader.StartMS, leader.EndMS)
	}

	quorum := realized[1]
	if len(quorum.Nodes) != 1 {
		t.Errorf("minority(kv) bound to %v, want exactly one node", quorum.Nodes)
	}
	if !strings.HasPrefix(quorum.Resolved, "net.partition("+quorum.Nodes[0]+")") {
		t.Errorf("Resolved = %q, want it pinned to %s", quorum.Resolved, quorum.Nodes[0])
	}

	// Every realized entry must survive the world file's own validation, or the
	// .thesis it goes into is unwritable.
	w := schema.NewWorld(1, "default", "smoke")
	w.FaultSchedule.Planned = e.Schedule().Planned()
	w.FaultSchedule.Realized = realized
	w = w.Normalized()
	if err := w.Validate(); err != nil {
		t.Errorf("realized schedule does not validate inside a world: %v", err)
	}

	// The injector must have been handed a deterministic seed (D-025) and the
	// virtual millisecond the injection actually landed on.
	inj.mu.Lock()
	defer inj.mu.Unlock()
	if inj.injectAt["f001"] != 8200 || inj.injectAt["f002"] != 8400 {
		t.Errorf("injections landed at %d and %d, want 8200 and 8400 on the virtual clock",
			inj.injectAt["f001"], inj.injectAt["f002"])
	}
	if !reflect.DeepEqual(inj.nodes["f001"], []string{"kv-n2"}) {
		t.Errorf("the injector was handed %v, want the node the role bound to", inj.nodes["f001"])
	}
	if inj.seeds["f001"] == 0 && inj.seeds["f002"] == 0 {
		t.Error("no injection carried a seed; netem would pick its own and stop replaying")
	}
	if inj.seeds["f001"] == inj.seeds["f002"] {
		t.Error("two faults were handed the same seed")
	}
}

// Rerunning the same world seed must bind the same nodes and hand out the same
// seeds. This is invariant I2 at the level this package is responsible for.
func TestExecutorIsDeterministicForAWorldSeed(t *testing.T) {
	run := func() ([]schema.RealizedFault, map[string]uint64) {
		inj := newFakeInjector()
		e, _ := newExec(t, []string{
			"net.partition(minority(kv))@100..200",
			"proc.pause(any(2, kv))@300..400",
		}, inj, nil)
		if _, err := e.Run(context.Background()); err != nil {
			t.Fatalf("Run: %v", err)
		}
		inj.mu.Lock()
		seeds := map[string]uint64{}
		for k, v := range inj.seeds {
			seeds[k] = v
		}
		inj.mu.Unlock()
		return e.Realized(), seeds
	}
	a, sa := run()
	b, sb := run()
	if !reflect.DeepEqual(a, b) {
		t.Errorf("same seed produced different realized schedules:\n%+v\n%+v", a, b)
	}
	if !reflect.DeepEqual(sa, sb) {
		t.Errorf("same seed produced different injection seeds: %v vs %v", sa, sb)
	}
}

// Cancellation must not leak. A SIGSTOPped container or a live iptables rule
// poisons every subsequent world on this host.
func TestExecutorWithdrawsOnCancel(t *testing.T) {
	inj := newFakeInjector()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	inj.onInject = func(id string) {
		if id == "f001" {
			cancel()
		}
	}

	e, _ := newExec(t, []string{
		"proc.pause(kv-n1)@100..9000",
		"net.partition(kv-n2)@200..8000",
	}, inj, nil)

	res, err := e.Run(ctx)
	if err == nil {
		t.Fatal("a cancelled schedule must report the cancellation")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", err)
	}
	if !res.Canceled {
		t.Error("Result.Canceled must be set")
	}
	if live := inj.liveIDs(); len(live) != 0 {
		t.Fatalf("cancellation leaked %v; every injected fault must be withdrawn", live)
	}
	if got := inj.log(); !reflect.DeepEqual(got, []string{"inject f001", "withdraw f001"}) {
		t.Errorf("call log = %v, want inject then withdraw of f001 only", got)
	}

	// The second fault never landed, so it must not appear in the realized
	// schedule: realized means injected, not planned.
	if r := e.Realized(); len(r) != 1 || r[0].Nodes[0] != "kv-n1" {
		t.Errorf("realized = %+v, want only the fault that actually landed", r)
	}

	// HEAL after a cancelled run must still verify, and must find nothing.
	rep, err := e.Heal(context.Background())
	if err != nil {
		t.Fatalf("Heal after cancel: %v", err)
	}
	if !rep.Verified {
		t.Error("HEAL must verify residual state even after a cancelled run")
	}
	if len(rep.Residues) != 0 {
		t.Errorf("residues after a clean cancel: %v", rep.Residues)
	}
}

// A target that resolves to nothing must abort the schedule, not be skipped.
func TestExecutorZeroMatchTargetFailsTheSchedule(t *testing.T) {
	inj := newFakeInjector()
	// No role observer: role:leader cannot bind, which is the same failure shape
	// as a role nobody claims.
	e, _ := newExec(t, []string{
		"proc.pause(kv-n1)@100..900",
		"net.partition(role:leader)@200..800",
	}, inj, nil)

	_, err := e.Run(context.Background())
	if err == nil {
		t.Fatal("an unresolvable target must fail the schedule, never be skipped")
	}
	if !errors.Is(err, ErrNoRoleObserver) {
		t.Errorf("error = %v, want ErrNoRoleObserver", err)
	}
	if live := inj.liveIDs(); len(live) != 0 {
		t.Errorf("the already-injected fault leaked: %v", live)
	}
	// The failed fault is recorded as an ATTEMPT but never as realized.
	var sawFailure bool
	for _, in := range e.Injections() {
		if in.FaultID == "f002" {
			sawFailure = true
			if in.InjectOK {
				t.Error("f002 is marked injected but its target never resolved")
			}
			if in.InjectErr == nil {
				t.Error("f002 carries no error")
			}
		}
	}
	if !sawFailure {
		t.Error("the failed attempt was not recorded at all")
	}
	for _, r := range e.Realized() {
		if strings.Contains(r.Fault, "role:leader") {
			t.Error("a fault that never resolved appears in the realized schedule")
		}
	}
}

func TestExecutorInjectionFailureAbortsAndUnwinds(t *testing.T) {
	inj := newFakeInjector()
	inj.injectErr["f002"] = errors.New("sidecar exited 1")
	e, _ := newExec(t, []string{
		"proc.pause(kv-n1)@100..900",
		"net.partition(kv-n2)@200..800",
		"proc.pause(kv-n3)@300..700",
	}, inj, nil)

	if _, err := e.Run(context.Background()); err == nil {
		t.Fatal("an injection failure must fail the schedule")
	}
	if live := inj.liveIDs(); len(live) != 0 {
		t.Errorf("unwinding leaked %v", live)
	}
	want := []string{"inject f001", "inject-failed f002", "withdraw f001"}
	if got := inj.log(); !reflect.DeepEqual(got, want) {
		t.Errorf("call log = %v, want %v", got, want)
	}
}

// A withdrawal that fails must NOT strand the rest of the schedule, and the
// fault must stay marked active so HEAL retries it.
func TestExecutorWithdrawFailureIsRetriedAtHeal(t *testing.T) {
	inj := newFakeInjector()
	inj.withdrawErr["f001"] = errors.New("iptables: device busy")
	e, _ := newExec(t, []string{
		"proc.pause(kv-n1)@100..200",
		"net.partition(kv-n2)@300..400",
	}, inj, nil)

	_, err := e.Run(context.Background())
	if err == nil {
		t.Fatal("a failed withdrawal must be reported")
	}
	// f002 was still injected and withdrawn despite f001's failure.
	if got := inj.log(); !containsString(got, "withdraw f002") {
		t.Errorf("a stuck withdrawal stranded the rest of the schedule: %v", got)
	}

	// HEAL retries. Let it succeed this time and the world comes out clean.
	inj.mu.Lock()
	delete(inj.withdrawErr, "f001")
	inj.mu.Unlock()

	rep, err := e.Heal(context.Background())
	if err != nil {
		t.Fatalf("Heal: %v", err)
	}
	if !containsString(rep.Withdrawn, "f001") {
		t.Errorf("HEAL should have retried f001, withdrew %v", rep.Withdrawn)
	}
	if len(rep.Residues) != 0 {
		t.Errorf("residues: %v", rep.Residues)
	}
}

// Residual fault state is a HARNESS ERROR. HEAL must surface it, loudly, with
// what was found.
func TestHealReportsResidualState(t *testing.T) {
	inj := newFakeInjector()
	inj.residues = []Residue{{
		NodeID: "kv-n1", Mechanism: "iptables", FaultID: "f001",
		Detail: `-A INPUT -s 172.19.0.3 -m comment --comment "thesis:r_2026_09_07_a41f:f001" -j DROP`,
	}}
	e, _ := newExec(t, []string{"proc.pause(kv-n1)@100..200"}, inj, nil)
	if _, err := e.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	rep, err := e.Heal(context.Background())
	if err == nil {
		t.Fatal("residual state must be an error")
	}
	if !errors.Is(err, ErrResidual) {
		t.Errorf("error = %v, want ErrResidual", err)
	}
	var re *ResidualError
	if !errors.As(err, &re) || len(re.Residues) != 1 {
		t.Fatalf("error does not carry the residues: %v", err)
	}
	if !strings.Contains(err.Error(), "thesis:r_2026_09_07_a41f:f001") {
		t.Errorf("the error must quote the evidence: %v", err)
	}
	if len(rep.Residues) != 1 {
		t.Errorf("report residues = %v", rep.Residues)
	}
	inj.mu.Lock()
	defer inj.mu.Unlock()
	if inj.verifyCalls == 0 {
		t.Error("HEAL did not ask the injector to verify at all")
	}
}

// "Could not check" is not "clean".
func TestHealTreatsUnverifiedAsFailure(t *testing.T) {
	inj := newFakeInjector()
	inj.verifyErr = errors.New("docker daemon unreachable")
	e, _ := newExec(t, []string{"proc.pause(kv-n1)@100..200"}, inj, nil)
	if _, err := e.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	rep, err := e.Heal(context.Background())
	if err == nil {
		t.Fatal("a residual check that could not run must not report clean")
	}
	if !errors.Is(err, ErrUnverified) {
		t.Errorf("error = %v, want ErrUnverified", err)
	}
	if rep.Verified {
		t.Error("HealReport.Verified must be false")
	}
}

// A world with no faults needs no mechanism and no verification.
func TestEmptyScheduleIsAClearNoOp(t *testing.T) {
	cfg := testConfig(t, nil, nil)
	top := kvTopology(t)
	sched, err := Compile(CompileOptions{Config: cfg, Topology: top})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	clock := recorder.NewSimClock(time.Unix(1788764420, 0))
	tl := &recorder.Timeline{}
	if err := tl.StartDriveAt(clock.NowR(), clock.NowWall().UnixNano()); err != nil {
		t.Fatalf("StartDriveAt: %v", err)
	}
	e, err := NewExecutor(ExecutorOptions{
		Schedule: sched, Topology: top, Clock: clock, Timeline: tl,
	})
	if err != nil {
		t.Fatalf("NewExecutor: %v", err)
	}
	res, err := e.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.Realized) != 0 {
		t.Errorf("realized = %v, want none", res.Realized)
	}
	rep, err := e.Heal(context.Background())
	if err != nil {
		t.Fatalf("Heal: %v", err)
	}
	if len(rep.Residues) != 0 {
		t.Errorf("residues = %v", rep.Residues)
	}
}

func TestNewExecutorRefusesMissingWiring(t *testing.T) {
	cfg := testConfig(t, nil, nil)
	top := kvTopology(t)
	reg, err := NewRegistry(newFakeInjector(schema.FaultProcPause))
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	sched, err := Compile(CompileOptions{
		Config: cfg, Topology: top, Planned: []string{"proc.pause(kv-n1)@1..2"},
	})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	clock := recorder.NewSimClock(time.Unix(1788764420, 0))

	// Before DRIVE there is no virtual clock to schedule against.
	unstarted := &recorder.Timeline{}
	if _, err := NewExecutor(ExecutorOptions{
		RunID: "r", Schedule: sched, Topology: top, Registry: reg, Clock: clock, Timeline: unstarted,
	}); !errors.Is(err, recorder.ErrNoOrigin) {
		t.Errorf("error = %v, want ErrNoOrigin", err)
	}

	tl := &recorder.Timeline{}
	if err := tl.StartDriveAt(clock.NowR(), clock.NowWall().UnixNano()); err != nil {
		t.Fatalf("StartDriveAt: %v", err)
	}

	// No registry at all.
	if _, err := NewExecutor(ExecutorOptions{
		RunID: "r", Schedule: sched, Topology: top, Clock: clock, Timeline: tl,
	}); !errors.Is(err, ErrNoInjector) {
		t.Errorf("error = %v, want ErrNoInjector", err)
	}

	// No run id: the ownership tag would be unformable.
	if _, err := NewExecutor(ExecutorOptions{
		Schedule: sched, Topology: top, Registry: reg, Clock: clock, Timeline: tl,
	}); err == nil {
		t.Error("an executor with faults but no run id must be refused")
	}
}

func TestRegistryRejectsDuplicateKinds(t *testing.T) {
	_, err := NewRegistry(
		newFakeInjector(schema.FaultProcPause),
		newFakeInjector(schema.FaultProcPause),
	)
	if err == nil {
		t.Fatal("two injectors for one kind must be refused, not silently chained")
	}
	if _, err := NewRegistry(newFakeInjector()); err != nil {
		t.Errorf("NewRegistry: %v", err)
	}
}

func TestOwnershipTagSpelling(t *testing.T) {
	// D-026 fixes this spelling; withdrawal and HEAL both match on it.
	if got := OwnershipTag("r_2026_09_07_a41f", "f001"); got != "thesis:r_2026_09_07_a41f:f001" {
		t.Errorf("OwnershipTag = %q", got)
	}
	if got := OwnershipPrefix("r_2026_09_07_a41f"); got != "thesis:r_2026_09_07_a41f:" {
		t.Errorf("OwnershipPrefix = %q", got)
	}
}

func TestExecutorHandsInjectorsTheWholeSetAndItsComplement(t *testing.T) {
	inj := newFakeInjector()
	var gotPeers []string
	wrapped := &peerCapturingInjector{fakeInjector: inj, peers: &gotPeers}
	cfg := testConfig(t, nil, nil)
	top := kvTopology(t)
	reg, err := NewRegistry(wrapped)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	sched, err := Compile(CompileOptions{
		Config: cfg, Topology: top, Planned: []string{"net.partition(minority(kv))@100..200"}, Registry: reg,
	})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	clock := recorder.NewSimClock(time.Unix(1788764420, 0))
	tl := &recorder.Timeline{}
	if err := tl.StartDriveAt(clock.NowR(), clock.NowWall().UnixNano()); err != nil {
		t.Fatalf("StartDriveAt: %v", err)
	}
	e, err := NewExecutor(ExecutorOptions{
		RunID: "r_2026_09_07_a41f", Schedule: sched, Topology: top,
		Registry: reg, Clock: clock, Timeline: tl, Seed: recorder.Seed(5),
	})
	if err != nil {
		t.Fatalf("NewExecutor: %v", err)
	}
	if _, err := e.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// One kv node is isolated; the peers are the other two kv nodes plus pg.
	if len(gotPeers) != 3 {
		t.Fatalf("peers = %v, want the three nodes outside the target set", gotPeers)
	}
	target := inj.nodes["f001"]
	for _, p := range gotPeers {
		if containsString(target, p) {
			t.Errorf("peer %s is also in the target set %v", p, target)
		}
	}
}

type peerCapturingInjector struct {
	*fakeInjector
	peers *[]string
}

func (p *peerCapturingInjector) Inject(ctx context.Context, req InjectRequest) error {
	*p.peers = nodeIDs(req.Peers)
	return p.fakeInjector.Inject(ctx, req)
}

func TestRunTwiceIsRefused(t *testing.T) {
	e, _ := newExec(t, []string{"proc.pause(kv-n1)@1..2"}, newFakeInjector(), nil)
	if _, err := e.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := e.Run(context.Background()); err == nil {
		t.Fatal("a second Run must be refused; the schedule has already been walked")
	}
}

func TestResidueStringIsLegible(t *testing.T) {
	r := Residue{NodeID: "kv-n1", Mechanism: "tc-qdisc", Detail: "netem 1:3"}
	if got := r.String(); !strings.Contains(got, "kv-n1") || !strings.Contains(got, "unattributed") {
		t.Errorf("Residue.String() = %q", got)
	}
	// Residue must satisfy fmt.Stringer, so an error that embeds it reads.
	if got := fmt.Sprintf("%v", r); got != r.String() {
		t.Errorf("fmt.Sprintf(%%v) = %q, want %q", got, r.String())
	}
}
