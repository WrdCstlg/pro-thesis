package control

import (
	"strings"
	"testing"

	"github.com/WrdCstlg/pro-thesis/internal/probehost"
	"github.com/WrdCstlg/pro-thesis/internal/recorder"
)

func targetsOf(t *testing.T, env []string) (string, bool) {
	t.Helper()
	prefix := TargetsEnv + "="
	out, found := "", false
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			// A later entry wins in an environment, so keep the last.
			out, found = strings.TrimPrefix(kv, prefix), true
		}
	}
	return out, found
}

func topo(ports ...int64) *recorder.Topology {
	top := &recorder.Topology{}
	names := []string{"kv-n1", "kv-n2", "kv-n3"}
	for i, p := range ports {
		top.Nodes = append(top.Nodes, recorder.NodeBinding{ID: names[i], HostPort: p})
	}
	return top
}

// The harness knows the published ports and the address it can reach them on.
// Staying silent and letting the driver guess is only safe while the guess is
// right, which is true on the host and false everywhere else.
//
// Measured, runs r_2026_09_24_a253 and r_2026_09_24_9c90: driven from inside a
// container, every health probe passed and all 60,000 operations failed in all
// five worlds of both runs, because the serial path never set PROTHESIS_TARGETS
// and the fixture driver fell back to its own 127.0.0.1 default. The history
// records carry `"target":"http://127.0.0.1:18082"` and say so.
func TestSerialWorldsTellTheDriverWhereTheClusterIs(t *testing.T) {
	t.Cleanup(func() {
		if err := probehost.Set(probehost.Default); err != nil {
			t.Fatalf("restoring the default failed: %v", err)
		}
	})

	if err := probehost.Set("host.docker.internal"); err != nil {
		t.Fatal(err)
	}
	got, found := targetsOf(t, driverEnv(topo(18081, 18082, 18083), nil))
	if !found {
		t.Fatal("the driver was told nothing about where the cluster is, so it can only " +
			"guess; on the host that guess is right and in a container it is not")
	}
	want := "host.docker.internal:18081,host.docker.internal:18082,host.docker.internal:18083"
	if got != want {
		t.Fatalf("%s = %q, want %q", TargetsEnv, got, want)
	}
}

// The parallel executor allocates its own port band and sets the variable
// itself. That value is authoritative for its lane and must not be overwritten
// by the topology-derived one.
func TestAnExplicitTargetsValueWins(t *testing.T) {
	explicit := TargetsEnv + "=127.0.0.1:29001,127.0.0.1:29002"
	got, found := targetsOf(t, driverEnv(topo(18081, 18082), []string{explicit}))
	if !found {
		t.Fatal("the explicit value disappeared")
	}
	if want := "127.0.0.1:29001,127.0.0.1:29002"; got != want {
		t.Fatalf("%s = %q, want the explicitly supplied %q", TargetsEnv, got, want)
	}
}

// The strip stays load-bearing: an inherited value from a shell or an outer
// `thesis` would point this world's driver at somebody else's cluster, and a
// world that drove the wrong system still writes a history that looks clean.
// The topology-derived value replaces it rather than letting it through.
func TestAnInheritedTargetsValueIsNotHonoured(t *testing.T) {
	t.Setenv(TargetsEnv, "127.0.0.1:19999")
	got, _ := targetsOf(t, driverEnv(topo(18081), nil))
	if strings.Contains(got, "19999") {
		t.Fatalf("%s = %q: an inherited value survived", TargetsEnv, got)
	}
	if want := "127.0.0.1:18081"; got != want {
		t.Fatalf("%s = %q, want the topology's own %q", TargetsEnv, got, want)
	}
}

// A node the harness published no port for contributes nothing rather than a
// malformed entry: not every declared node must publish one.
func TestNodesWithNoPublishedPortAreOmitted(t *testing.T) {
	got, found := targetsOf(t, driverEnv(topo(18081, 0, 18083), nil))
	if !found {
		t.Fatal("no targets at all")
	}
	if want := "127.0.0.1:18081,127.0.0.1:18083"; got != want {
		t.Fatalf("%s = %q, want %q", TargetsEnv, got, want)
	}
}

// With no topology there is nothing to say, and saying nothing is correct: the
// driver falls back to its own default, which is the pre-existing behaviour.
func TestNoTopologyMeansNoClaim(t *testing.T) {
	if _, found := targetsOf(t, driverEnv(nil, nil)); found {
		t.Fatal("a value was invented with no topology to derive it from")
	}
	if _, found := targetsOf(t, driverEnv(topo(), nil)); found {
		t.Fatal("a value was invented from a topology with no nodes")
	}
}
