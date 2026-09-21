package perturber

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

func testConfig(t *testing.T, allow, deny []schema.FaultKind) *schema.Config {
	t.Helper()
	return &schema.Config{
		Harness: schema.HarnessConfig{Nodes: []schema.NodeConfig{
			{ID: "kv-n1", Service: "kv", ComposeService: "kv-n1"},
			{ID: "kv-n2", Service: "kv", ComposeService: "kv-n2"},
			{ID: "kv-n3", Service: "kv", ComposeService: "kv-n3"},
		}},
		Perturber: schema.PerturberConfig{
			Budget: schema.PerturberBudget{MaxConcurrentFaults: 3, MaxFaultsPerWorld: 24},
			Allow:  allow,
			Deny:   deny,
		},
	}
}

func compileWith(t *testing.T, cfg *schema.Config, planned []string) (*Schedule, error) {
	t.Helper()
	return Compile(CompileOptions{Config: cfg, Topology: kvTopology(t), Planned: planned})
}

// eventLine renders one event as "<kind> <fault_id>@<ms>", which is exactly the
// claim the ordering test needs to make.
func eventLine(s *Schedule, ev Event) string {
	return fmt.Sprintf("%s %s@%d", ev.Kind, s.Faults[ev.Fault].ID, ev.AtMS)
}

func TestCompileOrdersEventsDeterministically(t *testing.T) {
	cfg := testConfig(t, nil, nil)
	// Deliberately written out of order: compilation must not depend on it.
	sched, err := compileWith(t, cfg, []string{
		"net.partition(kv-n1)@100..300",
		"proc.pause(kv-n2)@100..200",
		"net.latency(kv-n3, mean=50)@50..400",
	})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	wantPlanned := []string{
		"net.latency(kv-n3, mean=50, jitter=0)@50..400",
		"proc.pause(kv-n2)@100..200",
		"net.partition(kv-n1)@100..300",
	}
	if got := sched.Planned(); !reflect.DeepEqual(got, wantPlanned) {
		t.Errorf("Planned() = %v\nwant %v", got, wantPlanned)
	}
	for i, want := range []string{"f001", "f002", "f003"} {
		if sched.Faults[i].ID != want {
			t.Errorf("fault %d id = %q, want %q", i, sched.Faults[i].ID, want)
		}
	}

	wantEvents := []string{
		"inject f001@50",
		"inject f002@100",
		"inject f003@100",
		"withdraw f002@200",
		"withdraw f003@300",
		"withdraw f001@400",
	}
	got := make([]string, 0, len(sched.Events))
	for _, ev := range sched.Events {
		got = append(got, eventLine(sched, ev))
	}
	if !reflect.DeepEqual(got, wantEvents) {
		t.Errorf("event order =\n  %v\nwant\n  %v", got, wantEvents)
	}
	for i, ev := range sched.Events {
		if ev.Seq != i {
			t.Errorf("event %d has Seq %d", i, ev.Seq)
		}
	}
	if sched.PeakConcurrent != 3 {
		t.Errorf("PeakConcurrent = %d, want 3", sched.PeakConcurrent)
	}
	if sched.LastEventMS() != 400 {
		t.Errorf("LastEventMS = %d, want 400", sched.LastEventMS())
	}
}

// Compilation must be a pure function of the schedule's CONTENT, so the same
// faults written in any order compile to the same event list.
func TestCompileIsOrderIndependent(t *testing.T) {
	cfg := testConfig(t, nil, nil)
	a, err := compileWith(t, cfg, []string{
		"proc.pause(kv-n1)@8200..11000",
		"net.partition(kv-n2)@8400..10900",
	})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	b, err := compileWith(t, cfg, []string{
		"net.partition(kv-n2)@8400..10900",
		"proc.pause(kv-n1)@8200..11000",
	})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if !reflect.DeepEqual(a.Planned(), b.Planned()) {
		t.Errorf("planned differs by input order: %v vs %v", a.Planned(), b.Planned())
	}
	if !reflect.DeepEqual(a.Events, b.Events) {
		t.Errorf("events differ by input order")
	}
}

// A fault handed off at a single instant must not count as two concurrent
// faults: windows are half-open, so withdraw sorts before inject at the same
// millisecond.
func TestCompileWithdrawSortsBeforeInjectAtTheSameInstant(t *testing.T) {
	cfg := testConfig(t, nil, nil)
	cfg.Perturber.Budget.MaxConcurrentFaults = 1
	sched, err := compileWith(t, cfg, []string{
		"proc.pause(kv-n1)@0..100",
		"proc.pause(kv-n2)@100..200",
	})
	if err != nil {
		t.Fatalf("a handoff at t=100 must fit inside max_concurrent_faults=1: %v", err)
	}
	if sched.PeakConcurrent != 1 {
		t.Errorf("PeakConcurrent = %d, want 1", sched.PeakConcurrent)
	}
	if sched.Events[1].Kind != EventWithdraw || sched.Events[1].AtMS != 100 {
		t.Errorf("event[1] = %s, want the withdraw at 100", eventLine(sched, sched.Events[1]))
	}
	if sched.Events[2].Kind != EventInject || sched.Events[2].AtMS != 100 {
		t.Errorf("event[2] = %s, want the inject at 100", eventLine(sched, sched.Events[2]))
	}
}

func TestCompileEnforcesMaxConcurrentFaults(t *testing.T) {
	cfg := testConfig(t, nil, nil)
	cfg.Perturber.Budget.MaxConcurrentFaults = 2
	_, err := compileWith(t, cfg, []string{
		"proc.pause(kv-n1)@100..900",
		"proc.pause(kv-n2)@200..900",
		"proc.pause(kv-n3)@300..900",
	})
	if !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("error = %v, want ErrBudgetExceeded", err)
	}
	if !strings.Contains(err.Error(), "t+300ms") {
		t.Errorf("the error must name the instant the budget was exceeded: %v", err)
	}
}

func TestCompileEnforcesMaxFaultsPerWorld(t *testing.T) {
	cfg := testConfig(t, nil, nil)
	cfg.Perturber.Budget.MaxFaultsPerWorld = 1
	_, err := compileWith(t, cfg, []string{
		"proc.pause(kv-n1)@100..200",
		"proc.pause(kv-n2)@300..400",
	})
	if !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("error = %v, want ErrBudgetExceeded", err)
	}
}

// A zero budget must fail closed. Reading it as "unlimited" would turn a
// mis-decoded config into an unbounded fault schedule.
func TestCompileRefusesAZeroBudget(t *testing.T) {
	cfg := testConfig(t, nil, nil)
	cfg.Perturber.Budget = schema.PerturberBudget{}
	if _, err := compileWith(t, cfg, []string{"proc.pause(kv-n1)@1..2"}); err == nil {
		t.Fatal("a zero perturber.budget must be refused, not read as unlimited")
	}
}

func TestCompileEnforcesAllowAndDeny(t *testing.T) {
	// Denied outright.
	cfg := testConfig(t, nil, []schema.FaultKind{schema.FaultIOFill})
	_, err := compileWith(t, cfg, []string{"io.fill(kv-n1, pct=90)@100..200"})
	if !errors.Is(err, ErrKindNotAllowed) {
		t.Fatalf("denied kind: error = %v, want ErrKindNotAllowed", err)
	}
	if !strings.Contains(err.Error(), "perturber.deny") {
		t.Errorf("the error must say the kind was DENIED: %v", err)
	}

	// Not on a non-empty allow list.
	cfg = testConfig(t, []schema.FaultKind{schema.FaultNetPartition}, nil)
	_, err = compileWith(t, cfg, []string{"proc.pause(kv-n1)@100..200"})
	if !errors.Is(err, ErrKindNotAllowed) {
		t.Fatalf("unlisted kind: error = %v, want ErrKindNotAllowed", err)
	}
	if !strings.Contains(err.Error(), "perturber.allow") {
		t.Errorf("the error must say the kind was not ALLOWED: %v", err)
	}

	// On the allow list: fine.
	if _, err := compileWith(t, cfg, []string{"net.partition(kv-n1)@100..200"}); err != nil {
		t.Errorf("an allowed kind must compile: %v", err)
	}

	// deny beats allow.
	cfg = testConfig(t,
		[]schema.FaultKind{schema.FaultNetPartition},
		[]schema.FaultKind{schema.FaultNetPartition})
	if _, err := compileWith(t, cfg, []string{"net.partition(kv-n1)@100..200"}); !errors.Is(err, ErrKindNotAllowed) {
		t.Errorf("deny must beat allow: error = %v", err)
	}
}

// A target that matches nothing is refused at COMPILE time, before a container
// is touched, not eight seconds into DRIVE with the system already perturbed.
func TestCompileRefusesZeroMatchTargets(t *testing.T) {
	cfg := testConfig(t, nil, nil)
	for _, planned := range []string{
		"net.partition(minority(postgres))@100..200",
		"proc.pause(nope)@100..200",
		"net.partition(kv-n1<->nope)@100..200",
		"net.latency(nope:*, mean=10)@100..200",
	} {
		_, err := compileWith(t, cfg, []string{planned})
		if err == nil {
			t.Fatalf("Compile(%s) succeeded; a zero-match target must be refused", planned)
		}
		if !errors.Is(err, ErrNoMatch) {
			t.Errorf("Compile(%s) error = %v, want ErrNoMatch", planned, err)
		}
	}
}

func TestCompileRefusesNegativeWindows(t *testing.T) {
	cfg := testConfig(t, nil, nil)
	_, err := compileWith(t, cfg, []string{"proc.pause(kv-n1)@-10..100"})
	if err == nil {
		t.Fatal("a window starting before DRIVE must be refused: virtual time does not exist there")
	}
	if !strings.Contains(err.Error(), "DRIVE start") {
		t.Errorf("error should explain the virtual origin: %v", err)
	}
}

func TestCompileReportsEveryBadFaultAtOnce(t *testing.T) {
	cfg := testConfig(t, nil, nil)
	_, err := compileWith(t, cfg, []string{"not a fault", "also(not)@a..b"})
	if err == nil {
		t.Fatal("expected parse errors")
	}
	if !strings.Contains(err.Error(), "planned[0]") || !strings.Contains(err.Error(), "planned[1]") {
		t.Errorf("both entries should be reported: %v", err)
	}
}

// A schedule naming a kind nobody can inject must be refused. Running it would
// report a world as perturbed when nothing happened to it.
func TestCompileRefusesKindsWithNoMechanism(t *testing.T) {
	cfg := testConfig(t, nil, nil)
	reg, err := NewRegistry(&fakeInjector{kinds: []schema.FaultKind{schema.FaultProcPause}})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	_, err = Compile(CompileOptions{
		Config:   cfg,
		Topology: kvTopology(t),
		Planned:  []string{"net.partition(kv-n1)@100..200"},
		Registry: reg,
	})
	if !errors.Is(err, ErrNoInjector) {
		t.Fatalf("error = %v, want ErrNoInjector", err)
	}
	if _, err := Compile(CompileOptions{
		Config:   cfg,
		Topology: kvTopology(t),
		Planned:  []string{"proc.pause(kv-n1)@100..200"},
		Registry: reg,
	}); err != nil {
		t.Errorf("a served kind must compile: %v", err)
	}
}

// capabilityInjector serves a kind but declares that this host cannot deliver it,
// which is exactly the io.* family's situation on Docker Desktop.
type capabilityInjector struct {
	fakeInjector
	cannot map[schema.FaultKind]error
}

func (c *capabilityInjector) Capability(k schema.FaultKind) error { return c.cannot[k] }

// "An injector exists for this kind" and "this host can deliver it" are different
// questions, and only the first was being asked on the compile path.
//
// The cost of not asking the second is not abstract: run r_2026_09_09_42db spent
// a compose project, a bridge network and ~39 seconds reaching PERTURB before
// io.latency failed, and returned INCONCLUSIVE; a world that produced no
// evidence about the system under test. D-049 wired the same capability table
// into the search's action space and left every other path uncovered. See OQ-049.
func TestCompileRefusesAKindThisHostCannotDeliverBeforeSpendingAWorld(t *testing.T) {
	cfg := testConfig(t, nil, nil)
	cfg.Perturber.Allow = []schema.FaultKind{schema.FaultIOLatency, schema.FaultProcPause}

	why := errors.New("no FUSE shim can be inserted under a running overlay2 mount")
	inj := &capabilityInjector{
		fakeInjector: fakeInjector{kinds: []schema.FaultKind{schema.FaultIOLatency, schema.FaultProcPause}},
		cannot:       map[schema.FaultKind]error{schema.FaultIOLatency: why},
	}
	reg, err := NewRegistry(inj)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	_, err = Compile(CompileOptions{
		Config:   cfg,
		Topology: kvTopology(t),
		Planned:  []string{"io.latency(kv-n1, 5)@100..200"},
		Registry: reg,
	})
	if err == nil {
		t.Fatal("a schedule naming a kind this host cannot deliver compiled cleanly; the " +
			"refusal would land mid-PERTURB instead, after a compose project and a bridge " +
			"network had already been spent on a world that can produce no evidence")
	}
	if !errors.Is(err, why) {
		t.Fatalf("the refusal does not carry the injector's own reason, so the user is told "+
			"something failed and not what to do about it: %v", err)
	}

	// The check must be narrow. A kind the SAME injector serves and CAN deliver
	// must still compile: an over-broad capability check disables a whole family
	// and would read as "the platform got worse".
	if _, err := Compile(CompileOptions{
		Config:   cfg,
		Topology: kvTopology(t),
		Planned:  []string{"proc.pause(kv-n1)@100..200"},
		Registry: reg,
	}); err != nil {
		t.Errorf("a deliverable kind from the same injector was refused: %v", err)
	}
}

// Most mechanisms have nothing to say about capability, and an injector that does
// not implement the optional interface must not be treated as incapable.
func TestAnInjectorThatDeclaresNoCapabilityIsAssumedCapable(t *testing.T) {
	cfg := testConfig(t, nil, nil)
	reg, err := NewRegistry(&fakeInjector{kinds: []schema.FaultKind{schema.FaultProcPause}})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if _, err := Compile(CompileOptions{
		Config:   cfg,
		Topology: kvTopology(t),
		Planned:  []string{"proc.pause(kv-n1)@100..200"},
		Registry: reg,
	}); err != nil {
		t.Fatalf("an injector that implements no capability interface had its kind refused, "+
			"which would disable every mechanism that simply works everywhere: %v", err)
	}
}

func TestCompileEmptyScheduleStillParsesConstraints(t *testing.T) {
	cfg := testConfig(t, nil, nil)
	cfg.Perturber.Constraints = []string{"never partition more than minority of kv"}
	sched, err := compileWith(t, cfg, nil)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if !sched.Empty() || len(sched.Events) != 0 {
		t.Errorf("an empty schedule must compile to no events")
	}
	if len(sched.Constraints) != 1 {
		t.Errorf("constraints must be parsed even with no faults, got %d", len(sched.Constraints))
	}

	cfg.Perturber.Constraints = []string{"do something vaguely safe"}
	if _, err := compileWith(t, cfg, nil); !errors.Is(err, ErrUnparseableConstraint) {
		t.Fatalf("an unparseable constraint must fail closed even with no faults: %v", err)
	}
}

func TestCompileNeedsATopology(t *testing.T) {
	_, err := Compile(CompileOptions{Config: testConfig(t, nil, nil), Planned: []string{"proc.pause(kv-n1)@1..2"}})
	if err == nil {
		t.Fatal("Compile without a topology must fail: targets would never be checked")
	}
}

func TestScheduleKinds(t *testing.T) {
	cfg := testConfig(t, nil, nil)
	sched, err := compileWith(t, cfg, []string{
		"proc.pause(kv-n1)@1..2",
		"net.partition(kv-n2)@1..2",
		"proc.pause(kv-n3)@3..4",
	})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	want := []schema.FaultKind{schema.FaultNetPartition, schema.FaultProcPause}
	if got := sched.Kinds(); !reflect.DeepEqual(got, want) {
		t.Errorf("Kinds() = %v, want %v", got, want)
	}
}

// Two identical faults must draw from different PRNG sub-streams, or they would
// resolve to the same node and one of them would be a no-op.
func TestCompileTracksOccurrencesOfIdenticalFaults(t *testing.T) {
	cfg := testConfig(t, nil, nil)
	sched, err := compileWith(t, cfg, []string{
		"net.partition(minority(kv))@100..200",
		"net.partition(minority(kv))@100..200",
	})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if sched.Faults[0].Occurrence != 0 || sched.Faults[1].Occurrence != 1 {
		t.Fatalf("occurrences = %d, %d; want 0, 1", sched.Faults[0].Occurrence, sched.Faults[1].Occurrence)
	}
	a := substreamIndex(sched.Faults[0].Canonical, sched.Faults[0].Occurrence, "target")
	b := substreamIndex(sched.Faults[1].Canonical, sched.Faults[1].Occurrence, "target")
	if a == b {
		t.Error("identical faults share a PRNG sub-stream; they would resolve to the same node")
	}
}

// The sub-stream index must depend on the fault, NOT on its position, so that
// Phase 5 can delete a fault without perturbing the randomness of the rest.
func TestSubstreamIndexIsPositionIndependent(t *testing.T) {
	cfg := testConfig(t, nil, nil)
	full, err := compileWith(t, cfg, []string{
		"proc.pause(kv-n1)@100..200",
		"net.partition(minority(kv))@300..400",
	})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	shrunk, err := compileWith(t, cfg, []string{"net.partition(minority(kv))@300..400"})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	before := substreamIndex(full.Faults[1].Canonical, full.Faults[1].Occurrence, "target")
	after := substreamIndex(shrunk.Faults[0].Canonical, shrunk.Faults[0].Occurrence, "target")
	if before != after {
		t.Error("deleting an earlier fault changed a later fault's sub-stream; " +
			"ddmin would be minimizing against a moving target")
	}
	// Distinct purposes must not share a stream either.
	if substreamIndex("x", 0, "target") == substreamIndex("x", 0, "seed") {
		t.Error("the target and seed sub-streams collide")
	}
}
