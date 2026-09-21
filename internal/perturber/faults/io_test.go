package faults

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/WrdCstlg/pro-thesis/internal/perturber"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// ---------------------------------------------------------------------------
// The refusals. These are the tests that keep the package honest: a kind this
// platform cannot inject must FAIL LOUDLY, because a world whose faults never
// fired reporting PASS is the worst output this tool can produce.
// ---------------------------------------------------------------------------

func TestPlatformCapabilityRefusesWhatCannotBeDoneHere(t *testing.T) {
	for _, k := range []schema.FaultKind{schema.FaultIOLatency, schema.FaultIOError} {
		err := PlatformCapability(k)
		if err == nil {
			t.Fatalf("%s cannot be injected on Docker Desktop and must say so", k)
		}
		if !IsUnsupported(err) {
			t.Fatalf("%s: want ErrUnsupported so the control plane reports exit 2, got %v", k, err)
		}
		if !strings.Contains(err.Error(), "perturber.allow") {
			t.Fatalf("%s: the message must tell the operator what to do: %v", k, err)
		}
	}
	for _, k := range []schema.FaultKind{
		schema.FaultIOFill, schema.FaultMemPressure, schema.FaultFDExhaust,
		schema.FaultProcPause, schema.FaultClockSkew,
	} {
		if err := PlatformCapability(k); err != nil {
			t.Fatalf("%s is implementable here; PlatformCapability must not refuse it: %v", k, err)
		}
	}
}

// The refusal must reach INJECT, not be swallowed. An unsupported fault that
// returned nil would be indistinguishable from one that worked.
func TestIOUnsupportedFailsAtInjectAndRecordsNothing(t *testing.T) {
	for _, s := range []string{
		"io.latency(kv-n1, ms=50)@8200..11000",
		"io.error(kv-n1, rate=0.1)@8200..11000",
	} {
		p, err := NewIOFault(PrimitiveRequest{
			Env: Env{RunID: "r"}, Spec: mustFault(t, s), FaultID: "f1",
			Targets: []Target{{NodeID: "kv-n1", ContainerID: "c1"}},
		})
		if err != nil {
			t.Fatalf("%s: construction must succeed so the refusal lands at inject: %v", s, err)
		}
		injErr := p.Inject(context.Background())
		if injErr == nil {
			t.Fatalf("%s: Inject must fail rather than silently no-op", s)
		}
		if !IsUnsupported(injErr) {
			t.Fatalf("%s: want ErrUnsupported, got %v", s, injErr)
		}
		if ExitCodeFor(injErr) != schema.ExitInconclusive {
			t.Fatalf("%s: an unsupported kind must map to exit 2 INCONCLUSIVE", s)
		}
		if p.Active() {
			t.Fatalf("%s: nothing was applied, so nothing may report itself active", s)
		}
		if len(p.Records()) != 0 {
			t.Fatalf("%s: a fault that did not fire must record no event", s)
		}
		if err := p.Withdraw(context.Background()); err != nil {
			t.Fatalf("%s: withdrawing what was never applied must succeed: %v", s, err)
		}
	}
}

// ---------------------------------------------------------------------------
// io.fill
// ---------------------------------------------------------------------------

func TestParseDF(t *testing.T) {
	out := "Filesystem     1024-blocks    Used Available Capacity Mounted on\n" +
		"overlay          102400000 1024000  96000000       2% /\n"
	total, used, avail, err := parseDF(out)
	if err != nil {
		t.Fatal(err)
	}
	if total != 102400000 || used != 1024000 || avail != 96000000 {
		t.Fatalf("got %d/%d/%d", total, used, avail)
	}
	if _, _, _, err := parseDF("Filesystem 1024-blocks Used Available\n"); err == nil {
		t.Fatal("df output with no filesystem row must be an error, not a zero-size filesystem")
	}
}

func TestFillBallastMB(t *testing.T) {
	// 10 GiB filesystem, 1 GiB used, 9 GiB free; fill to 50%.
	mb, err := fillBallastMB(50, 10*1024*1024, 1*1024*1024, 9*1024*1024)
	if err != nil {
		t.Fatal(err)
	}
	if mb != 4096 {
		t.Fatalf("want 4096 MiB of ballast to reach 50%%, got %d", mb)
	}

	// Capped by what is actually available.
	mb, err = fillBallastMB(90, 10*1024*1024, 1*1024*1024, 2*1024*1024)
	if err != nil {
		t.Fatal(err)
	}
	if mb != 2048 {
		t.Fatalf("ballast must be capped at the available space, got %d", mb)
	}
}

// Already fuller than the request is NOT success. Writing nothing and reporting
// success is exactly the failure this package refuses to have.
func TestFillBallastRefusesWhenAlreadyFullerThanRequested(t *testing.T) {
	_, err := fillBallastMB(10, 10*1024*1024, 5*1024*1024, 5*1024*1024)
	if err == nil {
		t.Fatal("a filesystem already above the requested fill must be refused, not silently skipped")
	}
	if !IsUnsupported(err) {
		t.Fatalf("want ErrUnsupported, got %v", err)
	}
}

func TestFillBallastRefusesSubMegabyteRequests(t *testing.T) {
	if _, err := fillBallastMB(1, 1024, 0, 1024); err == nil {
		t.Fatal("a request below the mechanism's granularity must be refused")
	}
}

// The ballast file is named after its owner so residual verification matches on
// OWNERSHIP: a target may legitimately keep files in /tmp, and demanding an
// empty directory would false-fail on unmodified systems (D-026).
func TestIOFillBallastPathCarriesTheOwner(t *testing.T) {
	p, err := NewIOFault(PrimitiveRequest{
		Env: Env{RunID: "r_2026_09_07_abcd"}, Spec: mustFault(t, "io.fill(kv-n1, pct=80)@0..1000"),
		FaultID: "f7", Targets: []Target{{NodeID: "kv-n1", ContainerID: "c1"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := p.(*IOFill).BallastPath()
	want := "/tmp/.thesis-fill-r_2026_09_07_abcd-f7"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if id := fillFaultIDOf(got, "r_2026_09_07_abcd"); id != "f7" {
		t.Fatalf("a residue report must be able to name the owning fault; got %q", id)
	}
}

func TestIOFillWithdrawRemovesTheBallast(t *testing.T) {
	c := kvContainer("kv-n1", "aaaa000000001111")
	fd := newFakeDocker(c)
	fd.execOut = func(args []string) (string, bool) {
		for _, a := range args {
			if a == "df" {
				return "Filesystem 1024-blocks Used Available Capacity Mounted\n" +
					"overlay 10485760 1048576 9437184 10% /\n", true
			}
		}
		return "", true
	}
	p, err := NewIOFault(PrimitiveRequest{
		Env: fd.env(), Spec: mustFault(t, "io.fill(kv-n1, pct=50)@0..1000"),
		FaultID: "f1", Targets: targetsOf(c),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Inject(context.Background()); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	if err := p.Withdraw(context.Background()); err != nil {
		t.Fatalf("Withdraw: %v", err)
	}
	var sawRM bool
	for _, call := range fd.recorded("exec") {
		if strings.Contains(strings.Join(call, " "), "rm -f") {
			sawRM = true
		}
	}
	if !sawRM {
		t.Fatal("withdrawal never removed the ballast file; the space would stay occupied in every " +
			"subsequent world")
	}
}

// ---------------------------------------------------------------------------
// mem.pressure
// ---------------------------------------------------------------------------

// Measured: `docker update --memory 0` is a no-op and `--memory -1` is rejected
// by the CLI. A limit placed on an unlimited container can therefore never be
// removed, so injecting one would produce a fault that cannot be withdrawn.
func TestMemPressureRefusesAContainerWithNoMemoryLimit(t *testing.T) {
	c := kvContainer("kv-n1", "aaaa000000001111") // memory == 0
	fd := newFakeDocker(c)
	p, err := NewIOFault(PrimitiveRequest{
		Env: fd.env(), Spec: mustFault(t, "mem.pressure(kv-n1, pct=80)@0..1000"),
		FaultID: "f1", Targets: targetsOf(c),
	})
	if err != nil {
		t.Fatal(err)
	}
	err = p.Inject(context.Background())
	if err == nil || !IsUnsupported(err) {
		t.Fatalf("want ErrUnsupported for an unlimited container, got %v", err)
	}
	if !strings.Contains(err.Error(), "not be withdrawable") {
		t.Fatalf("the message must say WHY it is refused: %v", err)
	}
	if c.memory != 0 {
		t.Fatal("the refusal must not have applied a limit")
	}
}

func TestMemPressureInjectsAndRestoresExactly(t *testing.T) {
	c := kvContainer("kv-n1", "aaaa000000001111")
	c.memory = 512 * 1024 * 1024
	fd := newFakeDocker(c)
	targets := targetsOf(c)
	baseline, err := SnapshotContainerBaseline(context.Background(), fd.env(), targets, "")
	if err != nil {
		t.Fatal(err)
	}
	p, err := NewIOFault(PrimitiveRequest{
		Env: fd.env(), Spec: mustFault(t, "mem.pressure(kv-n1, pct=75)@0..1000"),
		FaultID: "f1", Targets: targets,
	})
	if err != nil {
		t.Fatal(err)
	}
	p.(*MemPressure).SetBaseline(baseline)

	if err := p.Inject(context.Background()); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	if want := int64(128 * 1024 * 1024); c.memory != want {
		t.Fatalf("memory = %d, want %d (25%% of the original limit left available)", c.memory, want)
	}
	if err := p.Withdraw(context.Background()); err != nil {
		t.Fatalf("Withdraw: %v", err)
	}
	if c.memory != 512*1024*1024 {
		t.Fatalf("the exact baseline limit must be restored; got %d", c.memory)
	}
}

func TestMemPressureLimitRefusesBelowDockersMinimum(t *testing.T) {
	if _, err := memPressureLimit(99, 8*1024*1024); err == nil {
		t.Fatal("a limit below docker's 6MiB minimum must be refused rather than sent to the daemon")
	}
	if _, err := memPressureLimit(50, 512*1024*1024); err != nil {
		t.Fatalf("a workable limit must be accepted: %v", err)
	}
}

func TestMemPressureRejectsDegenerateParameters(t *testing.T) {
	for _, s := range []string{
		"mem.pressure(kv-n1, pct=0)@0..1000",
		"mem.pressure(kv-n1, pct=100)@0..1000",
	} {
		_, err := NewIOFault(PrimitiveRequest{
			Env: Env{RunID: "r"}, Spec: mustFault(t, s), FaultID: "f1",
			Targets: []Target{{NodeID: "kv-n1", ContainerID: "c1"}},
		})
		if err == nil {
			t.Fatalf("%s must be refused", s)
		}
	}
}

// ---------------------------------------------------------------------------
// fd.exhaust
// ---------------------------------------------------------------------------

func TestParseFDState(t *testing.T) {
	out := "Max open files            1048576              1048576              files     \n" +
		"OPENFDS 37\n"
	st, err := parseFDState(out)
	if err != nil {
		t.Fatal(err)
	}
	if st.Soft != "1048576" || st.Hard != "1048576" || st.Open != 37 {
		t.Fatalf("got %+v", st)
	}
	if _, err := parseFDState("OPENFDS 3\n"); err == nil {
		t.Fatal("output with no limits row must be an error, not a zero limit")
	}

	st, err = parseFDState("Max open files            unlimited            unlimited            files\nOPENFDS 5\n")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := dkParseLimit(st.Soft); ok {
		t.Fatal("\"unlimited\" has no numeric value and must report ok=false")
	}
}

func TestFDTargetLimit(t *testing.T) {
	limit, err := fdTargetLimit(dkFDState{Soft: "1048576", Hard: "1048576", Open: 37}, DefaultFDHeadroom)
	if err != nil {
		t.Fatal(err)
	}
	if limit != 45 {
		t.Fatalf("want 37 open + 8 headroom = 45, got %d", limit)
	}
}

// A limit that would not actually constrain the target is a fault that never
// fires. It is refused rather than applied.
func TestFDTargetLimitRefusesWhenItWouldNotConstrain(t *testing.T) {
	_, err := fdTargetLimit(dkFDState{Soft: "40", Hard: "40", Open: 37}, DefaultFDHeadroom)
	if err == nil {
		t.Fatal("a computed limit at or above the current soft limit constrains nothing and must " +
			"be refused")
	}
	if !IsUnsupported(err) {
		t.Fatalf("want ErrUnsupported, got %v", err)
	}
}

func TestFDPrlimitArgs(t *testing.T) {
	got := strings.Join(fdPrlimitArgs("debian:stable-slim", "abc123", "64", "64"), " ")
	want := "run --rm --pid container:abc123 --cap-add SYS_RESOURCE debian:stable-slim " +
		"prlimit --pid 1 --nofile=64:64"
	if got != want {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
}

// Pulling an image inside a fault window would put a registry round trip in the
// middle of a timed disturbance and make injection depend on outbound network
// access from the Docker VM. It is refused, with the remedy in the message.
func TestFDExhaustRefusesWhenTheHelperImageIsAbsent(t *testing.T) {
	c := kvContainer("kv-n1", "aaaa000000001111")
	fd := newFakeDocker(c) // no images registered
	p, err := NewIOFault(PrimitiveRequest{
		Env: fd.env(), Spec: mustFault(t, "fd.exhaust(kv-n1)@0..1000"),
		FaultID: "f1", Targets: targetsOf(c),
	})
	if err != nil {
		t.Fatal(err)
	}
	err = p.Inject(context.Background())
	if err == nil || !IsUnsupported(err) {
		t.Fatalf("want ErrUnsupported when the helper image is missing, got %v", err)
	}
	if !strings.Contains(err.Error(), "docker pull") {
		t.Fatalf("the message must name the remedy: %v", err)
	}
}

func TestFDExhaustLowersAndRestoresTheLimit(t *testing.T) {
	c := kvContainer("kv-n1", "aaaa000000001111")
	fd := newFakeDocker(c)
	fd.imgs[DefaultHelperImage] = true

	soft, hard := "1048576", "1048576"
	fd.execOut = func(args []string) (string, bool) {
		joined := strings.Join(args, " ")
		if strings.Contains(joined, "prlimit") {
			// Apply what prlimit was told to do.
			for _, a := range args {
				if rest, ok := strings.CutPrefix(a, "--nofile="); ok {
					parts := strings.SplitN(rest, ":", 2)
					soft, hard = parts[0], parts[1]
				}
			}
			return "", true
		}
		if strings.Contains(joined, "Max open files") {
			return "Max open files            " + soft + "              " + hard +
				"              files\nOPENFDS 37\n", true
		}
		return "", true
	}

	targets := targetsOf(c)
	baseline, err := SnapshotContainerBaseline(context.Background(), fd.env(), targets, DefaultHelperImage)
	if err != nil {
		t.Fatal(err)
	}
	if bl, _ := baseline.Node("kv-n1"); bl.FDSoft != "1048576" {
		t.Fatalf("the BOOT snapshot must capture the descriptor limits; got %+v", bl)
	}

	p, err := NewIOFault(PrimitiveRequest{
		Env: fd.env(), Spec: mustFault(t, "fd.exhaust(kv-n1)@0..1000"),
		FaultID: "f1", Targets: targets,
	})
	if err != nil {
		t.Fatal(err)
	}
	p.(*FDExhaust).SetBaseline(baseline)

	if err := p.Inject(context.Background()); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	if soft != "45" {
		t.Fatalf("the soft limit was not lowered to 37 open + 8 headroom; got %q", soft)
	}
	if err := p.Withdraw(context.Background()); err != nil {
		t.Fatalf("Withdraw: %v", err)
	}
	if soft != "1048576" || hard != "1048576" {
		t.Fatalf("the baseline descriptor limits were not restored; got %s:%s", soft, hard)
	}
}

// ---------------------------------------------------------------------------
// the perturber.Injector adapter
// ---------------------------------------------------------------------------

func TestIOInjectorClaimsEveryKindInItsFamilies(t *testing.T) {
	in := NewIOInjector(Env{RunID: "r"}, ContainerBaseline{})
	want := map[schema.FaultKind]bool{
		schema.FaultIOLatency: true, schema.FaultIOError: true, schema.FaultIOFill: true,
		schema.FaultMemPressure: true, schema.FaultFDExhaust: true,
	}
	for _, k := range in.Kinds() {
		if !want[k] {
			t.Errorf("unexpected kind %s", k)
		}
		delete(want, k)
	}
	if len(want) != 0 {
		t.Fatalf("kinds left unclaimed: %v", want)
	}
}

func TestIOInjectorVerifyCleanReportsALeakedBallastFile(t *testing.T) {
	c := kvContainer("kv-n1", "aaaa000000001111")
	fd := newFakeDocker(c)
	fd.execOut = func(args []string) (string, bool) {
		if strings.Contains(strings.Join(args, " "), fillPrefix) {
			return "/tmp/.thesis-fill-r_test-f9\n", true
		}
		return "", true
	}
	in := NewIOInjector(fd.env(), ContainerBaseline{})
	res, err := in.VerifyClean(context.Background(), perturber.VerifyRequest{
		RunID:           "r_test",
		OwnershipPrefix: perturber.OwnershipPrefix("r_test"),
		Nodes: []perturber.Node{
			{ID: "kv-n1", Service: "kv", ComposeService: "kv-n1", ContainerID: c.id},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 {
		t.Fatalf("want one residue, got %v", res)
	}
	if res[0].Mechanism != "disk-ballast" || res[0].FaultID != "f9" {
		t.Fatalf("the residue must name its mechanism and owning fault; got %+v", res[0])
	}
}

func TestIOInjectorVerifyCleanIgnoresForeignFilesInTheSameDirectory(t *testing.T) {
	c := kvContainer("kv-n1", "aaaa000000001111")
	fd := newFakeDocker(c)
	fd.execOut = func(args []string) (string, bool) {
		// The target keeps its own scratch files in /tmp. The scan globs on the
		// ownership prefix, so it never sees them.
		return "", true
	}
	in := NewIOInjector(fd.env(), ContainerBaseline{})
	res, err := in.VerifyClean(context.Background(), perturber.VerifyRequest{
		RunID: "r_test",
		Nodes: []perturber.Node{
			{ID: "kv-n1", Service: "kv", ComposeService: "kv-n1", ContainerID: c.id},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 0 {
		t.Fatalf("files the target owns are not residue; got %v", res)
	}
}

func TestDkShellQuote(t *testing.T) {
	if got := dkShellQuote("/tmp/a b"); got != "'/tmp/a b'" {
		t.Fatalf("got %q", got)
	}
	if got := dkShellQuote("it's"); got != `'it'\''s'` {
		t.Fatalf("got %q", got)
	}
}

// ---------------------------------------------------------------------------
// integration: a real container, a real daemon
// ---------------------------------------------------------------------------

// startContainerWith launches a throwaway container with extra `docker run`
// arguments, and registers its removal.
func startContainerWith(t *testing.T, name, image string, runArgs ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := exec.CommandContext(ctx, "docker", "image", "inspect", image).Run(); err != nil {
		t.Skipf("image %s is not in the local store (docker pull %s); refusing to pull inside a test",
			image, image)
	}
	_ = exec.CommandContext(ctx, "docker", "container", "rm", "--force", name).Run()

	args := append([]string{"run", "--detach", "--name", name,
		"--label", "io.prothesis.node=" + name}, runArgs...)
	args = append(args, image, "sh", "-c", "while true; do sleep 1; done")
	out, err := exec.CommandContext(ctx, "docker", args...).Output()
	if err != nil {
		t.Fatalf("docker run: %v", err)
	}
	id := strings.TrimSpace(string(out))
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer ccancel()
		_ = exec.CommandContext(cctx, "docker", "unpause", id).Run()
		_ = exec.CommandContext(cctx, "docker", "container", "rm", "--force", id).Run()
	})
	return id
}

// io.fill is exercised against a SMALL tmpfs rather than the container's real
// filesystem. Filling a percentage of Docker Desktop's ~1TB virtual disk would
// be a genuine denial of service on the developer's machine, and a test that is
// dangerous to run is a test nobody runs.
func TestIntegrationIOFillWritesAndReleasesBallast(t *testing.T) {
	requireDocker(t)
	id := startContainerWith(t, "thesis-it-fill", integrationImage, "--tmpfs", "/small:size=32m")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	env := Env{RunID: "r_it"}
	targets := []Target{{NodeID: "thesis-it-fill", ContainerID: id, ComposeService: "n", Group: "g"}}
	p, err := NewIOFault(PrimitiveRequest{
		Env: env, Spec: mustFault(t, "io.fill(n1, pct=50)@0..1000"),
		FaultID: "f1", Targets: targets,
	})
	if err != nil {
		t.Fatal(err)
	}
	fill := p.(*IOFill)
	fill.Dir = "/small"

	if err := fill.Inject(ctx); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	if _, err := dkRun(ctx, env, "exec", id, "test", "-f", fill.BallastPath()); err != nil {
		t.Fatalf("the ballast file was not written: %v", err)
	}
	out, err := dkRun(ctx, env, "exec", id, "df", "-kP", "/small")
	if err != nil {
		t.Fatal(err)
	}
	total, used, _, err := parseDF(out)
	if err != nil {
		t.Fatal(err)
	}
	if pct := 100 * float64(used) / float64(total); pct < 40 {
		t.Fatalf("the filesystem is only %.1f%% full; the fault did not fire", pct)
	}

	if err := fill.Withdraw(ctx); err != nil {
		t.Fatalf("Withdraw: %v", err)
	}
	if _, err := dkRun(ctx, env, "exec", id, "test", "-f", fill.BallastPath()); err == nil {
		t.Fatal("the ballast file survived its own withdrawal; the space would stay occupied in " +
			"every subsequent world")
	}

	in := NewIOInjector(env, ContainerBaseline{})
	res, err := in.VerifyClean(ctx, perturber.VerifyRequest{
		RunID:           "r_it",
		OwnershipPrefix: perturber.OwnershipPrefix("r_it"),
		Nodes: []perturber.Node{
			{ID: "thesis-it-fill", Service: "g", ComposeService: "n", ContainerID: id},
		},
	})
	if err != nil {
		t.Fatalf("VerifyClean: %v", err)
	}
	if len(res) != 0 {
		t.Fatalf("HEAL still sees residue after a clean withdrawal: %v", res)
	}
}

// The measured fd.exhaust mechanism, end to end: a helper container sharing the
// target's PID namespace lowers RLIMIT_NOFILE on the target's PID 1, and the
// change is observed from INSIDE the target.
func TestIntegrationFDExhaustLowersAndRestoresRLimit(t *testing.T) {
	requireDocker(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	env := Env{RunID: "r_it", HelperImage: DefaultHelperImage}
	if !dkImageExists(ctx, env, DefaultHelperImage) {
		t.Skipf("helper image %s is not in the local store (docker pull %s)",
			DefaultHelperImage, DefaultHelperImage)
	}
	id := startContainerWith(t, "thesis-it-fd", integrationImage)

	targets := []Target{{NodeID: "thesis-it-fd", ContainerID: id, ComposeService: "n", Group: "g"}}
	baseline, err := SnapshotContainerBaseline(ctx, env, targets, DefaultHelperImage)
	if err != nil {
		t.Fatalf("SnapshotContainerBaseline: %v", err)
	}
	bl, ok := baseline.Node("thesis-it-fd")
	if !ok || bl.FDSoft == "" {
		t.Fatalf("the BOOT snapshot did not capture the descriptor limits: %+v", bl)
	}

	p, err := NewIOFault(PrimitiveRequest{
		Env: env, Spec: mustFault(t, "fd.exhaust(n1)@0..1000"), FaultID: "f1", Targets: targets,
	})
	if err != nil {
		t.Fatal(err)
	}
	p.(*FDExhaust).SetBaseline(baseline)

	if err := p.Inject(ctx); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	// Observed from inside the target, not from the helper that changed it.
	out, err := dkRun(ctx, env, "exec", id, "cat", "/proc/1/limits")
	if err != nil {
		t.Fatal(err)
	}
	lowered, err := parseFDState(out + "OPENFDS 0\n")
	if err != nil {
		t.Fatal(err)
	}
	if lowered.Soft == bl.FDSoft {
		t.Fatalf("the descriptor limit was not lowered: still %s", lowered.Soft)
	}

	if err := p.Withdraw(ctx); err != nil {
		t.Fatalf("Withdraw: %v", err)
	}
	out, err = dkRun(ctx, env, "exec", id, "cat", "/proc/1/limits")
	if err != nil {
		t.Fatal(err)
	}
	restored, err := parseFDState(out + "OPENFDS 0\n")
	if err != nil {
		t.Fatal(err)
	}
	if restored.Soft != bl.FDSoft || restored.Hard != bl.FDHard {
		t.Fatalf("the baseline descriptor limits were not restored: got %s:%s, want %s:%s",
			restored.Soft, restored.Hard, bl.FDSoft, bl.FDHard)
	}

	in := NewIOInjector(env, baseline)
	res, err := in.VerifyClean(ctx, perturber.VerifyRequest{
		RunID: "r_it",
		Nodes: []perturber.Node{
			{ID: "thesis-it-fd", Service: "g", ComposeService: "n", ContainerID: id},
		},
	})
	if err != nil {
		t.Fatalf("VerifyClean: %v", err)
	}
	if len(res) != 0 {
		t.Fatalf("HEAL still sees residue after a clean withdrawal: %v", res)
	}
}

// The measured clock capability probe, against two real images. This is the
// finding that narrows D-024: the fixture's own alpine image cannot host a time
// namespace, and discovering that at injection time would look like a bug in the
// system under test.
func TestIntegrationClockCapabilityDistinguishesBusyBoxFromUtilLinux(t *testing.T) {
	requireDocker(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	env := Env{RunID: "r_it"}

	if !dkImageExists(ctx, env, integrationImage) {
		t.Skipf("image %s is not in the local store", integrationImage)
	}
	err := ClockCapability(ctx, env, integrationImage)
	if err == nil {
		t.Fatalf("%s ships BusyBox's unshare, which has no --time; the probe must refuse it",
			integrationImage)
	}
	if !IsUnsupported(err) {
		t.Fatalf("want ErrUnsupported so the control plane reports exit 2, got %v", err)
	}
	if !strings.Contains(err.Error(), "util-linux") {
		t.Fatalf("the message must name the missing package: %v", err)
	}

	if !dkImageExists(ctx, env, DefaultHelperImage) {
		t.Skipf("no util-linux image in the local store to prove the positive case "+
			"(docker pull %s)", DefaultHelperImage)
	}
	if err := ClockCapability(ctx, env, DefaultHelperImage); err != nil {
		t.Fatalf("%s ships util-linux and must be accepted: %v", DefaultHelperImage, err)
	}
}
