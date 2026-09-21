package faults

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func hasStatusFilter(c fakeCall) bool {
	for _, a := range c.Args {
		if strings.HasPrefix(a, "status=") {
			return true
		}
	}
	return false
}

// `docker run --rm` reaps ASYNCHRONOUSLY, so a sweep run right after a clean
// withdrawal still sees the exited container. Counting that as a leak would
// make HEAL's most important signal a false-positive generator.
func TestSweepDoesNotCountAnExitedSidecarAsStray(t *testing.T) {
	r := &fakeRunner{respond: func(c fakeCall) (string, string, error) {
		if c.Sub == "ps" {
			if hasStatusFilter(c) {
				return "", "", nil // nothing alive
			}
			return "deadbeefcafe\n", "", nil // one corpse mid-reap
		}
		return "", "", nil
	}}
	sc := &Sidecar{RunID: "r_2026_09_07_a41f", Runner: r}
	strays, err := sc.SweepSidecars(context.Background())
	if err != nil {
		t.Fatalf("SweepSidecars: %v", err)
	}
	if len(strays) != 0 {
		t.Fatalf("got %d strays, want 0: an exited container holds no network namespace", len(strays))
	}
	// It must still be reaped, or a long search accumulates one per fault.
	var reaped bool
	for _, c := range r.subcommands("rm") {
		for _, a := range c.Args {
			if a == "deadbeefcafe" {
				reaped = true
			}
		}
	}
	if !reaped {
		t.Fatal("the exited sidecar was not removed; they would accumulate one per fault")
	}
}

// A sidecar that is still RUNNING holds a reference to the target's network
// namespace, which blocks the target's own teardown. That is a real leak.
func TestSweepCountsALiveSidecarAsStray(t *testing.T) {
	r := &fakeRunner{respond: func(c fakeCall) (string, string, error) {
		if c.Sub == "ps" && hasStatusFilter(c) {
			return "aliveaaaa\n", "", nil
		}
		return "", "", nil
	}}
	sc := &Sidecar{RunID: "r_2026_09_07_a41f", Runner: r}
	strays, err := sc.SweepSidecars(context.Background())
	if err != nil {
		t.Fatalf("SweepSidecars: %v", err)
	}
	if len(strays) != 1 {
		t.Fatalf("got %d strays, want 1", len(strays))
	}
}

func TestSidecarArgs(t *testing.T) {
	r := &fakeRunner{respond: func(c fakeCall) (string, string, error) { return "", "", nil }}
	sc := &Sidecar{RunID: "r_2026_09_07_a41f", Runner: r}
	if _, err := sc.Exec(context.Background(), "target123", "echo hi"); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	runs := r.subcommands("run")
	if len(runs) != 1 {
		t.Fatalf("got %d run invocations, want 1", len(runs))
	}
	joined := strings.Join(runs[0].Args, " ")
	for _, want := range []string{
		"--rm",
		"--network container:target123",
		"--cap-add NET_ADMIN",
		"--entrypoint /bin/sh",
		"--label " + LabelSidecar + "=r_2026_09_07_a41f",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("sidecar invocation is missing %q:\n%s", want, joined)
		}
	}
	// The sidecar must never create a network of its own: this host has a hard
	// ceiling of about two dozen free bridge networks (D-017).
	if strings.Contains(joined, "--network bridge") {
		t.Fatalf("sidecar creates its own network:\n%s", joined)
	}
	if strings.Contains(joined, "--privileged") {
		t.Fatalf("sidecar asks for --privileged; NET_ADMIN is the whole requirement:\n%s", joined)
	}
}

// A leaked sidecar holds the target's network namespace open, so removal must
// be attempted even when `--rm` cannot have run.
func TestSidecarForceRemovesOnFailure(t *testing.T) {
	r := &fakeRunner{respond: func(c fakeCall) (string, string, error) {
		if c.Sub == "run" {
			return "", "boom", errors.New("exit status 1")
		}
		return "", "", nil
	}}
	sc := &Sidecar{RunID: "r_2026_09_07_a41f", Runner: r}
	if _, err := sc.Exec(context.Background(), "target123", "false"); err == nil {
		t.Fatal("a failing script must surface as an error")
	}
	if len(r.subcommands("rm")) != 1 {
		t.Fatal("a failed sidecar was not force-removed")
	}
}

// Phase 4 runs worlds concurrently within one run (D-022), so several injectors
// share a run id and are live at once. A run-scoped sweep would force-remove
// another world's IN-FLIGHT injection, and that world would then fail for
// reasons nothing in its own logs could explain.
func TestSweepIsScopedToTheInstanceNotTheRun(t *testing.T) {
	var listedLabels []string
	respond := func(c fakeCall) (string, string, error) {
		if c.Sub == "ps" {
			for i, a := range c.Args {
				if a == "--filter" && i+1 < len(c.Args) && strings.HasPrefix(c.Args[i+1], "label=") {
					listedLabels = append(listedLabels, c.Args[i+1])
				}
			}
		}
		return "", "", nil
	}
	a := &Sidecar{RunID: "r_2026_09_07_a41f", Runner: &fakeRunner{respond: respond}}
	b := &Sidecar{RunID: "r_2026_09_07_a41f", Runner: &fakeRunner{respond: respond}}
	if a.Owner() == b.Owner() {
		t.Fatal("two injectors sharing a run id got the same sweep key; " +
			"each would reap the other's live sidecars")
	}
	if _, err := a.SweepSidecars(context.Background()); err != nil {
		t.Fatalf("SweepSidecars: %v", err)
	}
	for _, l := range listedLabels {
		if !strings.HasPrefix(l, "label="+LabelSidecarOwner+"=") {
			t.Fatalf("the instance sweep filtered on %q, not on its owner label", l)
		}
	}

	// The teardown path DOES want everything from the run.
	listedLabels = nil
	if _, err := a.SweepRunSidecars(context.Background()); err != nil {
		t.Fatalf("SweepRunSidecars: %v", err)
	}
	for _, l := range listedLabels {
		if !strings.HasPrefix(l, "label="+LabelSidecar+"=") {
			t.Fatalf("the run sweep filtered on %q, not on the run label", l)
		}
	}
}

func TestSidecarRejectsAnUnsafeContainerRef(t *testing.T) {
	sc := &Sidecar{RunID: "r_2026_09_07_a41f", Runner: &fakeRunner{}}
	for _, bad := range []string{"a b", "a;rm -rf /", "", "$(id)"} {
		if _, err := sc.Exec(context.Background(), bad, "true"); err == nil {
			t.Fatalf("Exec accepted container ref %q", bad)
		}
	}
}

// The image is built once and cached. Pulling packages inside every fault
// window would cost seconds of the window and make the iptables and iproute2
// versions vary run to run, which for a reproducibility tool is a determinism
// leak rather than merely a slow path.
func TestEnsureImageBuildsOnceWithNoBuildContext(t *testing.T) {
	var inspects, builds int
	r := &fakeRunner{respond: func(c fakeCall) (string, string, error) {
		switch {
		case c.Sub == "image":
			inspects++
			return "", "No such image", errors.New("exit status 1")
		case c.Sub == "build":
			builds++
			return "", "", nil
		}
		return "", "", nil
	}}
	sc := &Sidecar{RunID: "r_2026_09_07_a41f", Runner: r}
	for i := 0; i < 3; i++ {
		if err := sc.EnsureImage(context.Background()); err != nil {
			t.Fatalf("EnsureImage: %v", err)
		}
	}
	if inspects != 1 || builds != 1 {
		t.Fatalf("inspects=%d builds=%d; the image must be built once and then cached", inspects, builds)
	}
	args := strings.Join(r.subcommands("build")[0].Args, " ")
	if !strings.HasSuffix(args, " -") {
		t.Fatalf("build does not read the Dockerfile from stdin with no context: %q", args)
	}
	if strings.Contains(args, " . ") || strings.HasSuffix(args, " .") {
		t.Fatalf("build uploads a context directory to the daemon: %q", args)
	}
}

// The base image is pinned. It is a measurement instrument: iproute2 v7.0.0 is
// the version whose `netem seed` support D-025 depends on, and a floating tag
// would change it silently.
func TestSidecarDockerfilePinsItsBase(t *testing.T) {
	if strings.Contains(SidecarDockerfile, ":latest") || !strings.Contains(SidecarDockerfile, "FROM alpine:3.") {
		t.Fatalf("the sidecar base image is not pinned:\n%s", SidecarDockerfile)
	}
	for _, pkg := range []string{"iptables", "ip6tables", "iproute2"} {
		if !strings.Contains(SidecarDockerfile, pkg) {
			t.Fatalf("the sidecar image does not install %s:\n%s", pkg, SidecarDockerfile)
		}
	}
}
