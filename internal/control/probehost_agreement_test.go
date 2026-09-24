package control

import (
	"testing"

	"github.com/WrdCstlg/pro-thesis/internal/harness"
	"github.com/WrdCstlg/pro-thesis/internal/probehost"
)

// control.ProbeHost and harness.ProbeHost address the same published ports on
// the same machine, and until D-087 they were two separate constants holding
// the same literal, kept in step by a comment. Two sources of truth is one too
// many: a run where the steady-state probe went to loopback and the health
// probes went somewhere else would produce an environment failure that looked
// like the target's fault. They must agree at every installed value.
func TestSteadyStateAndHealthProbesAgreeOnTheHost(t *testing.T) {
	t.Cleanup(func() {
		if err := probehost.Set(probehost.Default); err != nil {
			t.Fatalf("restoring the default failed: %v", err)
		}
	})

	for _, want := range []string{probehost.Default, "host.docker.internal", "172.17.0.1", "[fd00::1]"} {
		if err := probehost.Set(want); err != nil {
			t.Fatalf("Set(%q): %v", want, err)
		}
		if got := ProbeHost(); got != want {
			t.Errorf("control.ProbeHost() = %q, want %q", got, want)
		}
		if got := harness.ProbeHost(); got != want {
			t.Errorf("harness.ProbeHost() = %q, want %q", got, want)
		}
		if ProbeHost() != harness.ProbeHost() {
			t.Fatalf("the two disagree: control %q, harness %q", ProbeHost(), harness.ProbeHost())
		}
	}
}

// The DRIVER's target list is the third place the probe address lives, and it
// is the one that decides whether the workload reaches the cluster at all.
//
// Measured, run r_2026_09_24_a253: with the harness in a container and only the
// probe paths converted, every health probe passed and all 60,000 operations
// failed in all five worlds, because Targets still rendered loopback and the
// driver was addressing its own. The checker refused rather than passing, which
// is the design working, but the run proved nothing about the target. The
// function's own doc comment warns about exactly this: a driver whose targets
// do not name the cluster "reports on a system it never touched".
func TestDriverTargetsFollowTheProbeHost(t *testing.T) {
	t.Cleanup(func() {
		if err := probehost.Set(probehost.Default); err != nil {
			t.Fatalf("restoring the default failed: %v", err)
		}
	})

	slot := WorkerSlot{
		Nodes: []string{"kv-n1", "kv-n2"},
		Ports: map[string]int{"kv-n1": 19001, "kv-n2": 19002},
	}

	if err := probehost.Set(probehost.Default); err != nil {
		t.Fatal(err)
	}
	got := slot.Targets()
	want := []string{"127.0.0.1:19001", "127.0.0.1:19002"}
	if !equalStrings(got, want) {
		t.Fatalf("Targets() = %v, want %v: the default must not move", got, want)
	}

	if err := probehost.Set("host.docker.internal"); err != nil {
		t.Fatal(err)
	}
	got = slot.Targets()
	want = []string{"host.docker.internal:19001", "host.docker.internal:19002"}
	if !equalStrings(got, want) {
		t.Fatalf("Targets() = %v, want %v: the driver must be told the same address the "+
			"probes use, or it drives nothing and the world reports on a system it never "+
			"touched", got, want)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
