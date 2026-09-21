package driver

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// The drain exists to close OQ-033: a world whose driver is killed with
// operations in flight can never report PASS, because no_stuck_op cannot tell a
// stuck operation from one that was simply not yet due. These tests pin the
// mechanism a lifecycle away from that decision: that a cooperating driver can
// be asked to finish, that one which will not is DISTINGUISHABLE from one that
// did, and that neither answer can be produced by accident.

// startHelper launches a helper mode and returns the supervisor plus a channel
// that closes when it exits.
func startHelper(t *testing.T, req Request) (*Supervisor, <-chan *Result) {
	t.Helper()
	req.Sub = goodSub()
	if req.Dir == "" {
		req.Dir = t.TempDir()
	}
	sup := NewSupervisor()
	done := make(chan *Result, 1)
	go func() {
		res, _ := sup.Run(context.Background(), req)
		done <- res
	}()
	return sup, done
}

// waitLive spins until the supervisor reports a running process, so a Drain
// cannot race the Start it is meant to follow.
func waitLive(t *testing.T, sup *Supervisor, limit time.Duration) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if sup.PID() != 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("the helper never started within %s", limit)
}

// The whole point: a driver told to stop finishes what it holds and EXITS OF ITS
// OWN ACCORD. `Killed` must be false: a killed driver is exactly the state
// OQ-033 says produces an unjudgeable history.
func TestACooperatingDriverDrainsAndExitsOnItsOwn(t *testing.T) {
	var out strings.Builder
	sup, done := startHelper(t, Request{
		Cmd:          helperTemplate(t, "drainable"),
		StdinControl: true,
		Stdout:       &out,
	})
	waitLive(t, sup, 5*time.Second)

	if !sup.HasControlChannel() {
		t.Fatalf("StdinControl was requested but no control channel exists")
	}
	if err := sup.Drain(); err != nil {
		t.Fatalf("Drain: %v", err)
	}

	select {
	case res := <-done:
		if res.Killed {
			t.Fatalf("the driver was KILLED after a drain; a killed driver leaves operations "+
				"in flight, which is the state OQ-033 exists to eliminate (status %s)", res.Status)
		}
		if !res.OK() {
			t.Fatalf("a drained driver should report %s, got %s (%v)", StatusOK, res.Status, res.Err)
		}
		if got := strings.TrimSpace(out.String()); got != "drained" {
			t.Fatalf("the driver did not run its post-drain work; stdout = %q, want %q", got, "drained")
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("the driver did not exit within 10s of being drained")
	}
}

// A driver that ignores the control channel must remain distinguishable from one
// that honoured it. Drain returning nil is NOT a claim that the driver drained;
// only the process exiting is, and nothing here may pretend otherwise.
func TestADriverThatIgnoresTheChannelDoesNotExit(t *testing.T) {
	sup, done := startHelper(t, Request{
		Cmd:          helperTemplate(t, "ignorestdin"),
		StdinControl: true,
	})
	waitLive(t, sup, 5*time.Second)

	if err := sup.Drain(); err != nil {
		t.Fatalf("Drain against a live driver should succeed at ASKING: %v", err)
	}

	select {
	case res := <-done:
		t.Fatalf("a driver that never reads stdin exited anyway (status %s); the test can no "+
			"longer tell a drain that worked from one that did not", res.Status)
	case <-time.After(400 * time.Millisecond):
		// Correct: asking is not the same as draining.
	}

	// The caller's remedy, and the one the lifecycle uses at the deadline.
	if err := sup.Kill(200 * time.Millisecond); err != nil {
		t.Fatalf("Kill after a refused drain: %v", err)
	}
	select {
	case res := <-done:
		if !res.Killed {
			t.Fatalf("the refusing driver was not recorded as killed: %+v", res.Status)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("the refusing driver survived Kill")
	}
}

// "Nobody asked" and "it refused" are different facts about a run and must not
// arrive as the same value.
func TestDrainWithoutAControlChannelIsItsOwnError(t *testing.T) {
	sup, done := startHelper(t, Request{
		Cmd: helperTemplate(t, "sleep", "3000"),
		// StdinControl deliberately absent.
	})
	waitLive(t, sup, 5*time.Second)

	if sup.HasControlChannel() {
		t.Fatalf("a control channel exists without StdinControl having been requested")
	}
	err := sup.Drain()
	if !errors.Is(err, ErrNoControlChannel) {
		t.Fatalf("Drain without a channel = %v, want ErrNoControlChannel; collapsing this into "+
			"success would report an unasked driver as a drained one", err)
	}

	_ = sup.Kill(200 * time.Millisecond)
	<-done
}

// Drain is called from a lifecycle path that also kills and tears down. It must
// be safe before the process starts, after it exits, and twice.
func TestDrainIsSafeAtEveryPointInTheLifecycle(t *testing.T) {
	sup := NewSupervisor()
	if err := sup.Drain(); !errors.Is(err, ErrNoControlChannel) {
		t.Fatalf("Drain on an idle supervisor = %v, want ErrNoControlChannel", err)
	}

	sup2, done := startHelper(t, Request{
		Cmd:          helperTemplate(t, "drainable"),
		StdinControl: true,
	})
	waitLive(t, sup2, 5*time.Second)

	if err := sup2.Drain(); err != nil {
		t.Fatalf("first Drain: %v", err)
	}
	if err := sup2.Drain(); err != nil {
		t.Fatalf("second Drain must be a no-op, got %v", err)
	}
	<-done
	if err := sup2.Drain(); err != nil {
		t.Fatalf("Drain after the driver exited must be a no-op, got %v", err)
	}
}

// The env var is the half of the dual transport a driver reads when its command
// line carries no flag. It must appear exactly when a channel exists.
func TestStdinControlEnvIsExportedOnlyWithAChannel(t *testing.T) {
	t.Run("exported when asked", func(t *testing.T) {
		var out strings.Builder
		res, err := runHelper(t, Request{
			Cmd:          helperTemplate(t, "echostdincontrol"),
			StdinControl: true,
			Stdout:       &out,
			// A pipe with nothing written blocks the helper's Scan forever, so
			// this mode does not read stdin at all: it reports the variable.
		})
		if err != nil || !res.OK() {
			t.Fatalf("helper failed: %v (%+v)", err, res)
		}
		if got := strings.TrimSpace(out.String()); got != "1" {
			t.Fatalf("%s = %q, want \"1\"", StdinControlEnv, got)
		}
	})

	t.Run("stripped when not asked, even if inherited", func(t *testing.T) {
		var out strings.Builder
		res, err := runHelper(t, Request{
			Cmd: helperTemplate(t, "echostdincontrol"),
			// Inherited as if some outer shell had exported it. Passing this
			// through with no pipe attached would point the fixture's loadgen at
			// the null device, give it instant EOF, and stop the workload before
			// it issued a single operation: a world reporting on a system it
			// never touched.
			Env:    append(os.Environ(), StdinControlEnv+"=1"),
			Stdout: &out,
		})
		if err != nil || !res.OK() {
			t.Fatalf("helper failed: %v (%+v)", err, res)
		}
		if got := strings.TrimSpace(out.String()); got != "" {
			t.Fatalf("%s = %q with no control channel; an inherited value must be stripped, "+
				"or a driver reads EOF from the null device and stops before doing any work",
				StdinControlEnv, got)
		}
	})
}

// A driver launched without StdinControl must behave exactly as it did before
// this file existed: stdin is the null device, not a pipe nobody writes to.
func TestWithoutStdinControlTheDriverIsUnchanged(t *testing.T) {
	res, err := runHelper(t, Request{Cmd: helperTemplate(t, "exit", "0")})
	if err != nil || !res.OK() {
		t.Fatalf("a plain driver must still run: %v (%+v)", err, res)
	}
}
