package faults

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/WrdCstlg/pro-thesis/internal/oracle"
	"github.com/WrdCstlg/pro-thesis/internal/perturber"
	"github.com/WrdCstlg/pro-thesis/internal/recorder"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// ---------------------------------------------------------------------------
// A fake docker CLI.
//
// It exists so that every decision these three families make (which flags are
// sent, in what order, whether a withdrawal is attempted for a target the
// injection skipped, whether a partial failure still tries the rest) is pinned
// by a test that runs everywhere, rather than only by a successful run on one
// machine with one daemon.
// ---------------------------------------------------------------------------

type fakeContainer struct {
	id         string
	node       string
	running    bool
	paused     bool
	image      string
	entrypoint []string
	cmd        []string
	nanoCPUs   int64
	cpuQuota   int64
	cpuPeriod  int64
	memory     int64
	memorySwap int64
	health     string // "" means the container declares no healthcheck
}

func (c *fakeContainer) status() string {
	switch {
	case c.paused:
		return "paused"
	case c.running:
		return "running"
	default:
		return "exited"
	}
}

type fakeDocker struct {
	mu    sync.Mutex
	byID  map[string]*fakeContainer
	imgs  map[string]bool
	calls [][]string
	// fail lets a test make one specific command fail, which is how the
	// "attempt every target" and "verify rather than assume" properties are
	// tested at all.
	fail func(args []string) error
	// execOut answers `docker exec` / `docker run` scripts.
	execOut func(args []string) (string, bool)
}

func newFakeDocker(cs ...*fakeContainer) *fakeDocker {
	f := &fakeDocker{byID: map[string]*fakeContainer{}, imgs: map[string]bool{}}
	for _, c := range cs {
		f.byID[c.id] = c
	}
	return f
}

func (f *fakeDocker) env() Env {
	return Env{RunID: "r_test", Exec: f.run}
}

func (f *fakeDocker) find(ref string) *fakeContainer {
	if c, ok := f.byID[ref]; ok {
		return c
	}
	for _, c := range f.byID {
		if c.node == ref {
			return c
		}
	}
	return nil
}

func (f *fakeDocker) recorded(verb string) [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out [][]string
	for _, c := range f.calls {
		if len(c) > 0 && c[0] == verb {
			out = append(out, c)
		}
	}
	return out
}

func (f *fakeDocker) run(_ context.Context, _, _ string, args []string) (string, error) {
	f.mu.Lock()
	f.calls = append(f.calls, append([]string(nil), args...))
	fail := f.fail
	f.mu.Unlock()

	if fail != nil {
		if err := fail(args); err != nil {
			return "", err
		}
	}
	if len(args) == 0 {
		return "", errors.New("fake docker: no arguments")
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	switch args[0] {
	case "inspect":
		c := f.find(args[len(args)-1])
		if c == nil {
			return "", fmt.Errorf("fake docker: no such container %q", args[len(args)-1])
		}
		return f.inspectJSON(c), nil

	case "pause":
		c := f.find(args[1])
		if c == nil {
			return "", errors.New("fake docker: no such container")
		}
		if c.paused {
			return "", errors.New("fake docker: container is already paused")
		}
		c.paused = true
		return "", nil

	case "unpause":
		c := f.find(args[1])
		if c == nil {
			return "", errors.New("fake docker: no such container")
		}
		if !c.paused {
			return "", errors.New("fake docker: container is not paused")
		}
		c.paused = false
		return "", nil

	case "kill":
		c := f.find(args[len(args)-1])
		if c == nil {
			return "", errors.New("fake docker: no such container")
		}
		c.running = false
		return "", nil

	case "stop":
		c := f.find(args[len(args)-1])
		if c == nil {
			return "", errors.New("fake docker: no such container")
		}
		if c.paused {
			return "", errors.New("fake docker: cannot stop a paused container")
		}
		c.running = false
		return "", nil

	case "start":
		c := f.find(args[len(args)-1])
		if c == nil {
			return "", errors.New("fake docker: no such container")
		}
		c.running = true
		return "", nil

	case "update":
		return f.update(args)

	case "ps":
		var node string
		for _, a := range args {
			if rest, ok := strings.CutPrefix(a, "label=io.prothesis.node="); ok {
				node = rest
			}
		}
		var ids []string
		for _, c := range f.byID {
			if c.node == node {
				ids = append(ids, c.id)
			}
		}
		return strings.Join(ids, "\n"), nil

	case "image":
		img := args[len(args)-1]
		if f.imgs[img] {
			return "sha256:deadbeef", nil
		}
		return "", fmt.Errorf("fake docker: no such image %q", img)

	case "exec", "run":
		if f.execOut != nil {
			if out, ok := f.execOut(args); ok {
				return out, nil
			}
		}
		return "", nil
	}
	return "", fmt.Errorf("fake docker: unhandled command %q", args[0])
}

func (f *fakeDocker) update(args []string) (string, error) {
	c := f.find(args[len(args)-1])
	if c == nil {
		return "", errors.New("fake docker: no such container")
	}
	for i := 1; i < len(args)-1; i++ {
		val := ""
		if i+1 < len(args)-1 {
			val = args[i+1]
		}
		switch args[i] {
		case "--cpus":
			// The daemon reads 0 as "no change". Reproducing that here is the
			// point: it is the trap that makes --cpus unusable for withdrawal.
			var v float64
			_, _ = fmt.Sscanf(val, "%g", &v)
			if v != 0 {
				c.nanoCPUs = int64(v * 1e9)
			}
			i++
		case "--cpu-quota":
			var v int64
			_, _ = fmt.Sscanf(val, "%d", &v)
			if c.nanoCPUs != 0 {
				return "", errors.New("Conflicting options: CPU Quota cannot be updated as NanoCPUs has already been set")
			}
			c.cpuQuota = v
			i++
		case "--cpu-period":
			var v int64
			_, _ = fmt.Sscanf(val, "%d", &v)
			if c.nanoCPUs != 0 {
				return "", errors.New("Conflicting options: CPU Period cannot be updated as NanoCPUs has already been set")
			}
			c.cpuPeriod = v
			i++
		case "--memory":
			var v int64
			_, _ = fmt.Sscanf(val, "%d", &v)
			if v != 0 {
				c.memory = v
			}
			i++
		case "--memory-swap":
			var v int64
			_, _ = fmt.Sscanf(val, "%d", &v)
			c.memorySwap = v
			i++
		}
	}
	return "", nil
}

func (f *fakeDocker) inspectJSON(c *fakeContainer) string {
	state := map[string]any{
		"Status":   c.status(),
		"Running":  c.running,
		"Paused":   c.paused,
		"ExitCode": 0,
	}
	if c.health != "" {
		state["Health"] = map[string]any{"Status": c.health}
	}
	doc := map[string]any{
		"Id":    c.id,
		"Name":  "/" + c.node,
		"State": state,
		"Config": map[string]any{
			"Image":      c.image,
			"Entrypoint": c.entrypoint,
			"Cmd":        c.cmd,
			"Labels":     map[string]string{"io.prothesis.node": c.node},
		},
		"HostConfig": map[string]any{
			"NanoCpus":   c.nanoCPUs,
			"CpuQuota":   c.cpuQuota,
			"CpuPeriod":  c.cpuPeriod,
			"Memory":     c.memory,
			"MemorySwap": c.memorySwap,
		},
	}
	b, err := json.Marshal(doc)
	if err != nil {
		panic(err)
	}
	return string(b) + "\n"
}

func mustFault(t *testing.T, s string) schema.FaultSpec {
	t.Helper()
	f, err := schema.ParseFault(s)
	if err != nil {
		t.Fatalf("ParseFault(%q): %v", s, err)
	}
	return f
}

func kvContainer(node, id string) *fakeContainer {
	return &fakeContainer{
		id: id, node: node, running: true,
		image:      "prothesis/kvfixture:buggy",
		entrypoint: []string{"/kv", "serve"},
	}
}

func targetsOf(cs ...*fakeContainer) []Target {
	out := make([]Target, 0, len(cs))
	for _, c := range cs {
		out = append(out, Target{
			NodeID: c.node, ContainerID: c.id,
			ComposeService: c.node, Group: "kv", ClientPort: 8080,
		})
	}
	return out
}

// ---------------------------------------------------------------------------
// proc.pause
// ---------------------------------------------------------------------------

func TestProcPauseInjectsAndWithdraws(t *testing.T) {
	c := kvContainer("kv-n1", "aaaa000000001111")
	fd := newFakeDocker(c)
	p, err := NewProcessFault(PrimitiveRequest{
		Env: fd.env(), Spec: mustFault(t, "proc.pause(kv-n1)@8200..11000"),
		FaultID: "f1", Targets: targetsOf(c),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Inject(context.Background()); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	if !c.paused {
		t.Fatal("container is not paused after Inject")
	}
	if !p.Active() {
		t.Fatal("fault does not report itself active while the container is paused")
	}
	if err := p.Withdraw(context.Background()); err != nil {
		t.Fatalf("Withdraw: %v", err)
	}
	if c.paused {
		t.Fatal("container is STILL PAUSED after Withdraw; a paused container cannot be stopped, " +
			"so teardown would leak it and one bridge network")
	}
	if p.Active() {
		t.Fatal("fault still reports itself active after Withdraw")
	}
	recs := p.Records()
	if len(recs) != 1 {
		t.Fatalf("want 1 record, got %d: %+v", len(recs), recs)
	}
	if recs[0].Open {
		t.Fatalf("the pause window was never closed: %+v", recs[0])
	}
	if recs[0].EndWallNS < recs[0].StartWallNS {
		t.Fatalf("the window ends before it begins: %+v", recs[0])
	}
	if recs[0].Resolved != "proc.pause(kv-n1)@8200..11000" {
		t.Fatalf("resolved = %q", recs[0].Resolved)
	}
}

// Windows' wall clock has a coarse tick, so a fault injected and withdrawn
// inside one tick reports the SAME nanosecond for both. A closed window must
// still read as closed: an `EndWallNS == StartWallNS` convention would have
// reported every fast fault as still active, and HEAL would then be unable to
// tell a withdrawn fault from a leaked one.
func TestClosedWindowIsFlaggedNotInferredFromTheTimestamps(t *testing.T) {
	c := kvContainer("kv-n1", "aaaa000000001111")
	fd := newFakeDocker(c)
	p, _ := NewProcessFault(PrimitiveRequest{
		Env: fd.env(), Spec: mustFault(t, "proc.pause(kv-n1)@0..1"),
		FaultID: "f1", Targets: targetsOf(c),
	})
	pause := p.(*ProcPause)
	pause.noteOpen(schema.FaultProcPause, []string{"kv-n1"}, 1000)
	if recs := pause.Records(); !recs[0].Open {
		t.Fatal("a freshly opened window must report itself open")
	}
	// Withdrawal observed at the very same tick.
	pause.closeWindow(schema.FaultProcPause, 1000)
	recs := pause.Records()
	if recs[0].Open {
		t.Fatal("a window closed within one clock tick must still read as CLOSED")
	}
	if recs[0].EndWallNS != 1000 {
		t.Fatalf("EndWallNS = %d", recs[0].EndWallNS)
	}

	// A clock that ticks backwards must not produce a window that ends before it
	// begins: schema.FaultSpec.Validate rejects one, and the causal timeline
	// would be non-monotonic.
	pause.noteOpen(schema.FaultProcPause, []string{"kv-n1"}, 5000)
	pause.closeWindow(schema.FaultProcPause, 4000)
	recs = pause.Records()
	last := recs[len(recs)-1]
	if last.EndWallNS < last.StartWallNS {
		t.Fatalf("window ends before it begins: %+v", last)
	}
}

// A pause that was never injected still unpauses whatever it FINDS paused. This
// is what lets HEAL clean up after a crashed run, and it is the reason
// withdrawal is driven by observed state rather than by in-process bookkeeping.
func TestProcPauseWithdrawUnpausesWithoutHavingInjected(t *testing.T) {
	c := kvContainer("kv-n1", "aaaa000000001111")
	c.paused = true
	fd := newFakeDocker(c)
	p, err := NewProcessFault(PrimitiveRequest{
		Env: fd.env(), Spec: mustFault(t, "proc.pause(kv-n1)@8200..11000"),
		FaultID: "f1", Targets: targetsOf(c),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Withdraw(context.Background()); err != nil {
		t.Fatalf("Withdraw: %v", err)
	}
	if c.paused {
		t.Fatal("a container found paused was not resumed")
	}
}

func TestProcPauseRefusesAContainerAlreadyPaused(t *testing.T) {
	c := kvContainer("kv-n1", "aaaa000000001111")
	c.paused = true
	fd := newFakeDocker(c)
	p, _ := NewProcessFault(PrimitiveRequest{
		Env: fd.env(), Spec: mustFault(t, "proc.pause(kv-n1)@8200..11000"),
		FaultID: "f1", Targets: targetsOf(c),
	})
	err := p.Inject(context.Background())
	if err == nil {
		t.Fatal("pausing an already-paused container must be an error: unpausing at withdrawal " +
			"would resume something this run did not stop")
	}
	if !strings.Contains(err.Error(), "ALREADY paused") {
		t.Fatalf("error does not name the condition: %v", err)
	}
}

// Withdraw must attempt EVERY target. A HEAL that stops at the first failure is
// the one thing HEAL must not be, because HEAL exists to clean up after a path
// that already went wrong once.
func TestProcPauseWithdrawAttemptsEveryTargetAfterAFailure(t *testing.T) {
	c1 := kvContainer("kv-n1", "aaaa000000001111")
	c2 := kvContainer("kv-n2", "bbbb000000002222")
	c3 := kvContainer("kv-n3", "cccc000000003333")
	fd := newFakeDocker(c1, c2, c3)
	p, _ := NewProcessFault(PrimitiveRequest{
		Env: fd.env(), Spec: mustFault(t, "proc.pause(kv:*)@8200..11000"),
		FaultID: "f1", Targets: targetsOf(c1, c2, c3),
	})
	if err := p.Inject(context.Background()); err != nil {
		t.Fatalf("Inject: %v", err)
	}

	// Make the middle unpause fail.
	fd.mu.Lock()
	fd.fail = func(args []string) error {
		if len(args) == 2 && args[0] == "unpause" && args[1] == c2.id {
			return errors.New("fake docker: daemon refused")
		}
		return nil
	}
	fd.mu.Unlock()

	err := p.Withdraw(context.Background())
	if err == nil {
		t.Fatal("a failed unpause must surface as an error")
	}
	if c1.paused || c3.paused {
		t.Fatal("withdrawal stopped at the first failure; kv-n1 or kv-n3 is still paused")
	}
	if !c2.paused {
		t.Fatal("the fake did not actually keep kv-n2 paused, so the test proves nothing")
	}
}

// ---------------------------------------------------------------------------
// proc.kill, and the seam that stops it reading as a crash
// ---------------------------------------------------------------------------

func TestProcKillDeliversTheSignal(t *testing.T) {
	c := kvContainer("kv-n1", "aaaa000000001111")
	fd := newFakeDocker(c)
	p, err := NewProcessFault(PrimitiveRequest{
		Env: fd.env(), Spec: mustFault(t, "proc.kill(kv-n1, signal=SIGTERM)@8200..8700"),
		FaultID: "f1", Targets: targetsOf(c),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Inject(context.Background()); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	kills := fd.recorded("kill")
	if len(kills) != 1 {
		t.Fatalf("want one kill, got %d", len(kills))
	}
	if got := strings.Join(kills[0], " "); got != "kill --signal SIGTERM "+c.id {
		t.Fatalf("kill args = %q", got)
	}
	// Withdrawing a delivered signal is a no-op, but it must exist and succeed.
	if err := p.Withdraw(context.Background()); err != nil {
		t.Fatalf("Withdraw: %v", err)
	}
	recs := p.Records()
	if len(recs) != 1 {
		t.Fatalf("want 1 record, got %d", len(recs))
	}
	if got := recs[0].EndWallNS - recs[0].StartWallNS; got != 500*int64(time.Millisecond) {
		t.Fatalf("the record must span the PLANNED window (500ms), got %dns", got)
	}
}

func TestProcKillRefusesAPausedContainer(t *testing.T) {
	c := kvContainer("kv-n1", "aaaa000000001111")
	c.paused = true
	fd := newFakeDocker(c)
	p, _ := NewProcessFault(PrimitiveRequest{
		Env: fd.env(), Spec: mustFault(t, "proc.kill(kv-n1)@8200..8700"),
		FaultID: "f1", Targets: targetsOf(c),
	})
	if err := p.Inject(context.Background()); err == nil {
		t.Fatal("signalling a SIGSTOPped process must be refused: the exit is not observable " +
			"until something resumes it, so the causal ordering would be wrong")
	}
}

// THE SEAM. no_crash reports a process exit outside a planned window as a
// violation. This drives the real oracle with windows built from the real
// records, and asserts both directions: inside the window is excused, outside it
// is not. Get this wrong and the perturber reports its own deliberate kill as a
// bug it found.
func TestOracleFaultWindowsExcuseADeliberateKill(t *testing.T) {
	c := kvContainer("kv-n1", "aaaa000000001111")
	fd := newFakeDocker(c)
	p, _ := NewProcessFault(PrimitiveRequest{
		Env: fd.env(), Spec: mustFault(t, "proc.kill(kv-n1, signal=SIGKILL)@8200..9200"),
		FaultID: "f1", Targets: targetsOf(c),
	})

	tl := &recorder.Timeline{}
	origin := time.Now().UnixNano()
	if err := tl.StartDriveAt(recorder.RTime(0), origin); err != nil {
		t.Fatal(err)
	}
	if err := p.Inject(context.Background()); err != nil {
		t.Fatalf("Inject: %v", err)
	}

	windows := OracleFaultWindows(p.Records(), tl)
	if len(windows) != 1 {
		t.Fatalf("want one fault window, got %d", len(windows))
	}
	if len(windows[0].Nodes) != 1 || windows[0].Nodes[0] != "kv-n1" {
		t.Fatalf("the window must name the CONCRETE node; got %v", windows[0].Nodes)
	}

	nc := oracle.NewNoCrash(oracle.DefaultNoCrashOptions())

	inside := windows[0].StartMS + (windows[0].EndMS-windows[0].StartMS)/2
	res, err := nc.Evaluate(context.Background(), schema.PhaseAssert, &oracle.Input{
		PlannedFaults: windows,
		Nodes: []oracle.NodeObservation{{
			NodeID: "kv-n1", Service: "kv", ContainerID: c.id, StateObserved: true,
			Running: false,
			Exit:    &oracle.ProcessExit{Code: 137, Signal: "SIGKILL", AtMS: &inside},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsViolation() {
		t.Fatalf("an exit INSIDE the planned window was reported as a crash: %s", res.Explanation)
	}

	outside := windows[0].EndMS + 5000
	res, err = nc.Evaluate(context.Background(), schema.PhaseAssert, &oracle.Input{
		PlannedFaults: windows,
		Nodes: []oracle.NodeObservation{{
			NodeID: "kv-n1", Service: "kv", ContainerID: c.id, StateObserved: true,
			Running: false,
			Exit:    &oracle.ProcessExit{Code: 2, AtMS: &outside},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsViolation() {
		t.Fatal("an exit OUTSIDE every planned window must be a violation; excusing it would make " +
			"no_crash unable to find the class of bug it exists for")
	}
}

// A record whose binding never reached the window list excuses nothing. This is
// the fail-closed half of the same seam.
func TestOracleFaultWindowsWithoutATimelineExcuseNothing(t *testing.T) {
	recs := []Record{{
		Kind: schema.FaultProcKill, Planned: "proc.kill(kv-n1)@0..1",
		Resolved: "proc.kill(kv-n1)@0..1", Nodes: []string{"kv-n1"},
		StartWallNS: 1, EndWallNS: 2,
	}}
	if got := OracleFaultWindows(recs, nil); len(got) != 0 {
		t.Fatalf("a nil timeline must yield no windows, got %v", got)
	}
	tl := &recorder.Timeline{} // no DRIVE origin
	if got := OracleFaultWindows(recs, tl); len(got) != 0 {
		t.Fatalf("a timeline with no origin must yield no windows, got %v", got)
	}
}

// ---------------------------------------------------------------------------
// proc.restart
// ---------------------------------------------------------------------------

func TestProcRestartStopsThenStartsAndRebinds(t *testing.T) {
	c := kvContainer("kv-n1", "aaaa000000001111")
	fd := newFakeDocker(c)
	p, err := NewProcessFault(PrimitiveRequest{
		Env: fd.env(), Spec: mustFault(t, "proc.restart(kv-n1)@8200..8200"),
		FaultID: "f1", Targets: targetsOf(c),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Inject(context.Background()); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	if !c.running {
		t.Fatal("the container did not come back up")
	}
	if len(fd.recorded("stop")) != 1 || len(fd.recorded("start")) != 1 {
		t.Fatalf("want exactly one stop and one start; stop=%v start=%v",
			fd.recorded("stop"), fd.recorded("start"))
	}
	// The binding is re-resolved from the node LABEL, not carried over from the
	// id we started with, because the clock family's recreate changes the id.
	if len(fd.recorded("ps")) == 0 {
		t.Fatal("the node binding was never re-resolved by label after the restart")
	}
	if err := p.Withdraw(context.Background()); err != nil {
		t.Fatalf("Withdraw: %v", err)
	}
}

func TestProcRestartUnpausesFirst(t *testing.T) {
	// A paused container CANNOT be stopped: the stop blocks for the full grace
	// period and then leaves it running.
	c := kvContainer("kv-n1", "aaaa000000001111")
	c.paused = true
	fd := newFakeDocker(c)
	p, _ := NewProcessFault(PrimitiveRequest{
		Env: fd.env(), Spec: mustFault(t, "proc.restart(kv-n1)@8200..8200"),
		FaultID: "f1", Targets: targetsOf(c),
	})
	if err := p.Inject(context.Background()); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	if len(fd.recorded("unpause")) != 1 {
		t.Fatal("a paused container must be unpaused before it is stopped")
	}
}

// ---------------------------------------------------------------------------
// proc.slow: the measured docker semantics
// ---------------------------------------------------------------------------

func TestProcSlowPrefersQuotaWhenNanoCPUsIsUnset(t *testing.T) {
	c := kvContainer("kv-n1", "aaaa000000001111")
	fd := newFakeDocker(c)
	p, err := NewProcessFault(PrimitiveRequest{
		Env: fd.env(), Spec: mustFault(t, "proc.slow(kv-n1, cpu_pct=25)@8200..11000"),
		FaultID: "f1", Targets: targetsOf(c),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Inject(context.Background()); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	if c.cpuQuota != 25000 || c.cpuPeriod != 100000 {
		t.Fatalf("want quota 25000 over period 100000, got %d/%d", c.cpuQuota, c.cpuPeriod)
	}
	if c.nanoCPUs != 0 {
		t.Fatal("NanoCpus must stay unset: once it is set the daemon refuses --cpu-period, and " +
			"--cpus 0 is a no-op, so the throttle could never be withdrawn")
	}
	if err := p.Withdraw(context.Background()); err != nil {
		t.Fatalf("Withdraw: %v", err)
	}
	if c.cpuQuota != -1 {
		t.Fatalf("withdrawal must clear the quota with -1 (0 is read as \"no change\"); got %d", c.cpuQuota)
	}
}

func TestWithdrawCPUArgsNeverAsksForZeroCPUs(t *testing.T) {
	// Measured: `docker update --cpus 0` leaves NanoCpus exactly where it was.
	// Emitting it would be a withdrawal that silently does nothing.
	cases := []struct {
		name string
		bl   dkCPUState
		want string
	}{
		{"no baseline limit at all", dkCPUState{Known: true}, "update --cpu-quota -1 c"},
		{"baseline quota", dkCPUState{CPUQuota: 50000, CPUPeriod: 100000, Known: true},
			"update --cpu-period 100000 --cpu-quota 50000 c"},
		{"baseline nanocpus", dkCPUState{NanoCPUs: 2e9, Known: true}, "update --cpus 2 c"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := strings.Join(withdrawCPUArgs(tc.bl, "c"), " ")
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
			for i, a := range withdrawCPUArgs(tc.bl, "c") {
				if a == "--cpus" && withdrawCPUArgs(tc.bl, "c")[i+1] == "0" {
					t.Fatal("`--cpus 0` is a no-op on this daemon and must never be emitted")
				}
			}
		})
	}
}

func TestProcSlowUsesCPUsOnlyWhenTheContainerAlreadyHasNanoCPUs(t *testing.T) {
	c := kvContainer("kv-n1", "aaaa000000001111")
	c.nanoCPUs = 2e9 // the compose file declared `cpus: 2`
	fd := newFakeDocker(c)
	p, _ := NewProcessFault(PrimitiveRequest{
		Env: fd.env(), Spec: mustFault(t, "proc.slow(kv-n1, cpu_pct=50)@8200..11000"),
		FaultID: "f1", Targets: targetsOf(c),
	})
	if err := p.Inject(context.Background()); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	if c.nanoCPUs != int64(0.5*1e9) {
		t.Fatalf("NanoCpus = %d, want 5e8", c.nanoCPUs)
	}
	if err := p.Withdraw(context.Background()); err != nil {
		t.Fatalf("Withdraw: %v", err)
	}
	if c.nanoCPUs != 2e9 {
		t.Fatalf("withdrawal must restore the exact baseline NanoCpus; got %d", c.nanoCPUs)
	}
}

func TestProcSlowRefusesAQuotaBelowTheKernelMinimum(t *testing.T) {
	c := kvContainer("kv-n1", "aaaa000000001111")
	fd := newFakeDocker(c)
	p, _ := NewProcessFault(PrimitiveRequest{
		Env: fd.env(), Spec: mustFault(t, "proc.slow(kv-n1, cpu_pct=0.5)@8200..11000"),
		FaultID: "f1", Targets: targetsOf(c),
	})
	err := p.Inject(context.Background())
	if err == nil || !IsUnsupported(err) {
		t.Fatalf("a quota below the kernel minimum must be an ErrUnsupported, got %v", err)
	}
}

// A withdrawal with no baseline must refuse rather than guess, because guessing
// "unlimited" on a container that legitimately shipped its own quota would
// silently widen the target's resources for every later world.
func TestProcSlowWithdrawWithoutABaselineRefusesRatherThanGuesses(t *testing.T) {
	c := kvContainer("kv-n1", "aaaa000000001111")
	c.cpuQuota = 40000
	c.cpuPeriod = 100000
	fd := newFakeDocker(c)
	p, _ := NewProcessFault(PrimitiveRequest{
		Env: fd.env(), Spec: mustFault(t, "proc.slow(kv-n1, cpu_pct=25)@8200..11000"),
		FaultID: "f1", Targets: targetsOf(c),
	})
	err := p.Withdraw(context.Background())
	if err == nil {
		t.Fatal("a throttled container with no recorded baseline must not be silently accepted")
	}
	if !strings.Contains(err.Error(), "no CPU baseline") {
		t.Fatalf("error does not name the cause: %v", err)
	}
}

// ---------------------------------------------------------------------------
// pure helpers
// ---------------------------------------------------------------------------

func TestDkResolvedKeepsParametersOnlyForTheSameKind(t *testing.T) {
	spec := mustFault(t, "clock.skew(role:leader, ms=3000)@8200..15100")
	if got := dkResolved(spec, schema.FaultClockSkew, []string{"kv-n2"}); got != "clock.skew(kv-n2, ms=3000)@8200..15100" {
		t.Fatalf("same kind: got %q", got)
	}
	// A restart does not declare `ms`, so carrying it over would produce a
	// string that does not parse back through the frozen grammar.
	got := dkResolved(spec, schema.FaultProcRestart, []string{"kv-n2"})
	if got != "proc.restart(kv-n2)@8200..15100" {
		t.Fatalf("different kind: got %q", got)
	}
	if _, err := schema.ParseFault(got); err != nil {
		t.Fatalf("a resolved string must parse back through the grammar: %v", err)
	}
}

func TestDkSignalArg(t *testing.T) {
	ok := []string{"proc.kill(n1, signal=SIGKILL)@0..1", "proc.kill(n1, signal=SIGTERM)@0..1",
		"proc.kill(n1, signal=9)@0..1"}
	for _, s := range ok {
		if _, err := dkSignalArg(mustFault(t, s)); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
}

func TestTargetsFromNodesRejectsAnUnboundNode(t *testing.T) {
	_, err := TargetsFromNodes([]perturber.Node{{ID: "kv-n1", Service: "kv"}})
	if err == nil {
		t.Fatal("a node with no container must be refused: injecting against nothing and reporting " +
			"success is how a world looks perturbed when it was not")
	}
}

func TestPrimitiveRequestRejectsAnEmptyTargetSet(t *testing.T) {
	_, err := NewProcessFault(PrimitiveRequest{
		Env: Env{RunID: "r"}, Spec: mustFault(t, "proc.pause(role:leader)@0..1"), FaultID: "f1",
	})
	if err == nil {
		t.Fatal("a target that resolved to nothing must be refused")
	}
}

func TestExitCodeForIsInconclusive(t *testing.T) {
	if got := ExitCodeFor(errors.New("boom")); got != schema.ExitInconclusive {
		t.Fatalf("an injection failure must map to exit 2, got %d", got)
	}
	if got := ExitCodeFor(nil); got != schema.ExitPass {
		t.Fatalf("no error must map to exit 0, got %d", got)
	}
}

// ---------------------------------------------------------------------------
// the perturber.Injector adapter
// ---------------------------------------------------------------------------

// The registry refuses two injectors for one kind, so the three families must
// partition the kinds they serve. This also proves each adapter satisfies the
// interface, which a compile-time assertion alone would not.
func TestInjectorsRegisterWithoutOverlapping(t *testing.T) {
	proc := NewProcessInjector(Env{RunID: "r"}, ContainerBaseline{})
	clock := NewClockInjector(Env{RunID: "r"})
	io := NewIOInjector(Env{RunID: "r"}, ContainerBaseline{})

	reg, err := perturber.NewRegistry(proc, clock, io)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	want := []schema.FaultKind{
		schema.FaultProcPause, schema.FaultProcKill, schema.FaultProcRestart, schema.FaultProcSlow,
		schema.FaultClockSkew, schema.FaultClockJump,
		schema.FaultIOLatency, schema.FaultIOError, schema.FaultIOFill,
		schema.FaultMemPressure, schema.FaultFDExhaust,
	}
	for _, k := range want {
		if _, err := reg.For(k); err != nil {
			t.Errorf("%s has no injector: %v", k, err)
		}
	}
}

func TestProcessInjectorRollsBackAPartialInjection(t *testing.T) {
	c1 := kvContainer("kv-n1", "aaaa000000001111")
	c2 := kvContainer("kv-n2", "bbbb000000002222")
	fd := newFakeDocker(c1, c2)
	// kv-n2 refuses to pause, so the injection as a whole fails.
	fd.fail = func(args []string) error {
		if len(args) == 2 && args[0] == "pause" && args[1] == c2.id {
			return errors.New("fake docker: daemon refused")
		}
		return nil
	}
	in := NewProcessInjector(fd.env(), ContainerBaseline{})
	err := in.Inject(context.Background(), perturber.InjectRequest{
		RunID: "r_test", FaultID: "f1",
		Spec: mustFault(t, "proc.pause(kv:*)@8200..11000"),
		Nodes: []perturber.Node{
			{ID: "kv-n1", Service: "kv", ComposeService: "kv-n1", ContainerID: c1.id},
			{ID: "kv-n2", Service: "kv", ComposeService: "kv-n2", ContainerID: c2.id},
		},
	})
	if err == nil {
		t.Fatal("a partial injection must be reported as a failure")
	}
	if c1.paused {
		t.Fatal("kv-n1 is still paused: a failed injection must leave nothing behind, because the " +
			"executor will not call Withdraw for a fault that never reported success")
	}
}

func TestProcessInjectorVerifyCleanFindsALeakedPause(t *testing.T) {
	c := kvContainer("kv-n1", "aaaa000000001111")
	fd := newFakeDocker(c)
	baseline, err := SnapshotContainerBaseline(context.Background(), fd.env(), targetsOf(c), "")
	if err != nil {
		t.Fatal(err)
	}
	in := NewProcessInjector(fd.env(), baseline)
	nodes := []perturber.Node{{ID: "kv-n1", Service: "kv", ComposeService: "kv-n1", ContainerID: c.id}}

	res, err := in.VerifyClean(context.Background(), perturber.VerifyRequest{
		RunID: "r_test", Nodes: nodes, OwnershipPrefix: perturber.OwnershipPrefix("r_test"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 0 {
		t.Fatalf("a clean topology must report no residue, got %v", res)
	}

	c.paused = true
	res, err = in.VerifyClean(context.Background(), perturber.VerifyRequest{
		RunID: "r_test", Nodes: nodes, OwnershipPrefix: perturber.OwnershipPrefix("r_test"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || res[0].Mechanism != "sigstop" {
		t.Fatalf("a leaked SIGSTOP must be reported, got %v", res)
	}
}

// A container that legitimately shipped its own CPU quota is NOT residue. This
// is D-026's rule (compare against the observed BOOT baseline, never against an
// assumed-clean one) applied to the process family.
func TestProcessInjectorVerifyCleanDoesNotFalseFailOnAPreExistingQuota(t *testing.T) {
	c := kvContainer("kv-n1", "aaaa000000001111")
	c.cpuQuota = 40000
	c.cpuPeriod = 100000
	fd := newFakeDocker(c)
	baseline, err := SnapshotContainerBaseline(context.Background(), fd.env(), targetsOf(c), "")
	if err != nil {
		t.Fatal(err)
	}
	in := NewProcessInjector(fd.env(), baseline)
	res, err := in.VerifyClean(context.Background(), perturber.VerifyRequest{
		RunID: "r_test",
		Nodes: []perturber.Node{{ID: "kv-n1", Service: "kv", ComposeService: "kv-n1", ContainerID: c.id}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 0 {
		t.Fatalf("a quota the target itself declared at BOOT is not residue; got %v", res)
	}
}

// ---------------------------------------------------------------------------
// integration: a real container, a real daemon
// ---------------------------------------------------------------------------

// requireDocker skips with a message that says what to do, rather than failing
// on a machine that legitimately has no daemon.
func requireDocker(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx, "docker", "info", "--format", "{{.ServerVersion}}").Run(); err != nil {
		t.Skipf("docker daemon is not reachable, so the fault primitives cannot be exercised "+
			"against a real container: %v. Start Docker Desktop and re-run.", err)
	}
}

const integrationImage = "alpine:3.20"

// startTestContainer launches a throwaway container and registers its removal.
func startTestContainer(t *testing.T, name string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := exec.CommandContext(ctx, "docker", "image", "inspect", integrationImage).Run(); err != nil {
		t.Skipf("image %s is not in the local store (docker pull %s); refusing to pull inside a test",
			integrationImage, integrationImage)
	}
	_ = exec.CommandContext(ctx, "docker", "container", "rm", "--force", name).Run()

	out, err := exec.CommandContext(ctx, "docker", "run", "--detach",
		"--name", name,
		"--label", "io.prothesis.node="+name,
		integrationImage, "sh", "-c", "while true; do sleep 1; done").Output()
	if err != nil {
		t.Fatalf("docker run: %v", err)
	}
	id := strings.TrimSpace(string(out))

	t.Cleanup(func() {
		// Unpause first. A paused container cannot be stopped: the removal
		// would block for the full timeout and then leak the container, which
		// is exactly the leak this whole family is careful about.
		cctx, ccancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer ccancel()
		_ = exec.CommandContext(cctx, "docker", "unpause", id).Run()
		_ = exec.CommandContext(cctx, "docker", "container", "rm", "--force", id).Run()
	})
	return id
}

func dockerPaused(t *testing.T, id string) bool {
	t.Helper()
	out, err := exec.Command("docker", "inspect", "--format", "{{.State.Paused}}", id).Output()
	if err != nil {
		t.Fatalf("docker inspect: %v", err)
	}
	return strings.TrimSpace(string(out)) == "true"
}

func TestIntegrationProcPauseUnpausesOnWithdraw(t *testing.T) {
	requireDocker(t)
	id := startTestContainer(t, "thesis-it-pause")

	env := Env{RunID: "r_it"}
	targets := []Target{{NodeID: "thesis-it-pause", ContainerID: id, ComposeService: "n", Group: "g"}}
	p, err := NewProcessFault(PrimitiveRequest{
		Env: env, Spec: mustFault(t, "proc.pause(n1)@0..1000"), FaultID: "f1", Targets: targets,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := p.Inject(ctx); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	if !dockerPaused(t, id) {
		t.Fatal("the container is not paused after Inject")
	}
	if err := p.Withdraw(ctx); err != nil {
		t.Fatalf("Withdraw: %v", err)
	}
	if dockerPaused(t, id) {
		t.Fatal("THE CONTAINER IS STILL PAUSED after Withdraw. A paused container cannot be " +
			"stopped, so `compose down` would block for its full timeout and then leak both the " +
			"container and one of this host's ~24 free bridge networks.")
	}

	// And HEAL agrees.
	in := NewProcessInjector(env, ContainerBaseline{Nodes: []ContainerNodeBaseline{
		{NodeID: "thesis-it-pause", ContainerID: id},
	}})
	res, err := in.VerifyClean(ctx, perturber.VerifyRequest{
		RunID: "r_it",
		Nodes: []perturber.Node{{ID: "thesis-it-pause", Service: "g", ComposeService: "n", ContainerID: id}},
	})
	if err != nil {
		t.Fatalf("VerifyClean: %v", err)
	}
	if len(res) != 0 {
		t.Fatalf("HEAL still sees residue after a clean withdrawal: %v", res)
	}
}

func TestIntegrationVerifyCleanCatchesALeakedPause(t *testing.T) {
	requireDocker(t)
	id := startTestContainer(t, "thesis-it-leak")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	env := Env{RunID: "r_it"}
	if _, err := dkRun(ctx, env, "pause", id); err != nil {
		t.Fatalf("docker pause: %v", err)
	}
	in := NewProcessInjector(env, ContainerBaseline{Nodes: []ContainerNodeBaseline{
		{NodeID: "thesis-it-leak", ContainerID: id},
	}})
	res, err := in.VerifyClean(ctx, perturber.VerifyRequest{
		RunID: "r_it",
		Nodes: []perturber.Node{{ID: "thesis-it-leak", Service: "g", ComposeService: "n", ContainerID: id}},
	})
	if err != nil {
		t.Fatalf("VerifyClean: %v", err)
	}
	if len(res) != 1 || res[0].Mechanism != "sigstop" {
		t.Fatalf("a leaked SIGSTOP must be reported by HEAL; got %v", res)
	}
	if _, err := dkRun(ctx, env, "unpause", id); err != nil {
		t.Fatalf("cleanup unpause: %v", err)
	}
}

func TestIntegrationProcKillStopsTheProcess(t *testing.T) {
	requireDocker(t)
	id := startTestContainer(t, "thesis-it-kill")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	env := Env{RunID: "r_it"}
	p, err := NewProcessFault(PrimitiveRequest{
		Env:     env,
		Spec:    mustFault(t, "proc.kill(n1, signal=SIGKILL)@0..1000"),
		FaultID: "f1",
		Targets: []Target{{NodeID: "thesis-it-kill", ContainerID: id, ComposeService: "n", Group: "g"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Inject(ctx); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		st, err := dkInspect(ctx, env, id)
		if err == nil && !st.State.Running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the container was still running 30s after SIGKILL")
		}
		time.Sleep(200 * time.Millisecond)
	}
	if err := p.Withdraw(ctx); err != nil {
		t.Fatalf("Withdraw of an instantaneous fault must succeed: %v", err)
	}
}

func TestIntegrationProcSlowRestoresTheCPUQuota(t *testing.T) {
	requireDocker(t)
	id := startTestContainer(t, "thesis-it-slow")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	env := Env{RunID: "r_it"}
	targets := []Target{{NodeID: "thesis-it-slow", ContainerID: id, ComposeService: "n", Group: "g"}}
	baseline, err := SnapshotContainerBaseline(ctx, env, targets, "")
	if err != nil {
		t.Fatalf("SnapshotContainerBaseline: %v", err)
	}

	p, err := NewProcessFault(PrimitiveRequest{
		Env: env, Spec: mustFault(t, "proc.slow(n1, cpu_pct=25)@0..1000"),
		FaultID: "f1", Targets: targets,
	})
	if err != nil {
		t.Fatal(err)
	}
	p.(*ProcSlow).SetBaseline(baseline)

	if err := p.Inject(ctx); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	st, err := dkInspect(ctx, env, id)
	if err != nil {
		t.Fatal(err)
	}
	if st.HostConfig.CPUQuota != 25000 {
		t.Fatalf("CpuQuota = %d, want 25000", st.HostConfig.CPUQuota)
	}

	if err := p.Withdraw(ctx); err != nil {
		t.Fatalf("Withdraw: %v", err)
	}
	in := NewProcessInjector(env, baseline)
	res, err := in.VerifyClean(ctx, perturber.VerifyRequest{
		RunID: "r_it",
		Nodes: []perturber.Node{{ID: "thesis-it-slow", Service: "g", ComposeService: "n", ContainerID: id}},
	})
	if err != nil {
		t.Fatalf("VerifyClean: %v", err)
	}
	if len(res) != 0 {
		t.Fatalf("a CPU throttle survived its own withdrawal: %v", res)
	}
}
