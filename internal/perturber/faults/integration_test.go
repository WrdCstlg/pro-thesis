package faults

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The network family's integration test: inject, observe the effect, withdraw,
// verify clean. It runs against real containers because every claim this
// package makes is a claim about a kernel: that a comment survives an iptables
// insert, that a u32 filter selects the right traffic, that deleting a root
// qdisc restores the one that was there before. None of that is testable
// against a mock, and a mock that agreed with a wrong belief would be worse
// than no test.
//
// It is skipped, loudly, when Docker is unavailable.

const (
	itTimeout   = 4 * time.Minute
	itPingCount = 8
)

func dockerOrSkip(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping the Docker integration test in -short mode")
	}
	if os.Getenv("PROTHESIS_SKIP_DOCKER") != "" {
		t.Skip("PROTHESIS_SKIP_DOCKER is set; skipping the Docker integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, stderr, err := ExecRunner{}.Run(ctx, nil, "docker", "info", "--format", "{{.ServerVersion}}")
	if err != nil {
		t.Skipf("Docker is not available, so the network family cannot be exercised end to end "+
			"(this test verifies real iptables and tc behaviour and has no meaningful mock): %v: %s",
			err, strings.TrimSpace(stderr))
	}
}

// itEnv is a throwaway two-container topology on its own bridge network.
type itEnv struct {
	t       *testing.T
	prefix  string
	network string
	// nodes are logical ids "a" and "b"; containers carry the prefix.
	containers map[string]string
	addrs      map[string]string
}

func newITEnv(t *testing.T) *itEnv {
	t.Helper()
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("entropy: %v", err)
	}
	e := &itEnv{
		t:          t,
		prefix:     "thesis-it-" + hex.EncodeToString(b[:]),
		containers: map[string]string{},
		addrs:      map[string]string{},
	}
	e.network = e.prefix + "-net"

	// Teardown is registered BEFORE anything is created, and removes
	// everything unconditionally. This host has a hard ceiling of roughly two
	// dozen free bridge networks and a leaked one is held permanently, so a
	// test that leaks a network on a failure path degrades the machine for
	// every later run (D-017).
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		for _, cid := range e.containers {
			_, _, _ = ExecRunner{}.Run(ctx, nil, "docker", "rm", "--force", "--volumes", cid)
		}
		_, _, _ = ExecRunner{}.Run(ctx, nil, "docker", "network", "rm", e.network)
	})

	e.run("network", "create", e.network)
	img := SidecarImage
	sc := &Sidecar{RunID: "r_2026_09_07_a41f"}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if err := sc.EnsureImage(ctx); err != nil {
		t.Fatalf("build the sidecar image: %v", err)
	}
	for _, id := range []string{"a", "b"} {
		name := e.prefix + "-" + id
		e.run("run", "--detach", "--name", name, "--network", e.network,
			"--entrypoint", "/bin/sh", img, "-c", "sleep 900")
		e.containers[id] = name
	}
	for id, name := range e.containers {
		out := e.run("inspect", "--format", "{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}", name)
		e.addrs[id] = strings.TrimSpace(out)
		if e.addrs[id] == "" {
			t.Fatalf("container %s has no address", name)
		}
	}
	return e
}

func (e *itEnv) run(args ...string) string {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	out, stderr, err := ExecRunner{}.Run(ctx, nil, "docker", args...)
	if err != nil {
		e.t.Fatalf("docker %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr))
	}
	_ = stderr
	return out
}

// exec runs a shell command inside one of the topology's containers. It returns
// output and success separately: an unreachable ping is an expected outcome
// here, not a test failure.
func (e *itEnv) exec(node, script string) (string, bool) {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	out, _, err := ExecRunner{}.Run(ctx, nil, "docker", "exec", e.containers[node], "sh", "-c", script)
	return out, err == nil
}

// rttMS returns the average round-trip time from one node to another.
func (e *itEnv) rttMS(from, to string) (float64, bool) {
	e.t.Helper()
	out, ok := e.exec(from, fmt.Sprintf("ping -c %d -i 0.2 -W 2 -q %s", itPingCount, e.addrs[to]))
	if !ok {
		return 0, false
	}
	i := strings.Index(out, "min/avg/max")
	if i < 0 {
		return 0, false
	}
	rest := out[i:]
	eq := strings.Index(rest, "=")
	if eq < 0 {
		return 0, false
	}
	fields := strings.Fields(rest[eq+1:])
	if len(fields) == 0 {
		return 0, false
	}
	parts := strings.Split(fields[0], "/")
	if len(parts) < 2 {
		return 0, false
	}
	v, err := strconv.ParseFloat(parts[1], 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

func (e *itEnv) reachable(from, to string) bool {
	_, ok := e.exec(from, fmt.Sprintf("ping -c 2 -W 2 -q %s", e.addrs[to]))
	return ok
}

func (e *itEnv) nodes() []NetNode {
	return []NetNode{
		{NodeID: "a", ContainerID: e.containers["a"], ComposeService: "a", Group: "grp"},
		{NodeID: "b", ContainerID: e.containers["b"], ComposeService: "b", Group: "grp"},
	}
}

func newITInjector(t *testing.T, e *itEnv) *Injector {
	t.Helper()
	in, err := NewInjector(Options{
		RunID:     "r_2026_09_07_a41f",
		WorldSeed: 20260906,
		Nodes:     e.nodes(),
	})
	if err != nil {
		t.Fatalf("NewInjector: %v", err)
	}
	return in
}

// TestIntegrationBaselineSeesTheTargetsOwnRules is the measurement D-026 rests
// on: an UNMODIFIED container already carries iptables rules of its own, so a
// residual check against an assumed-empty table false-fails immediately.
func TestIntegrationBaselineSeesTheTargetsOwnRules(t *testing.T) {
	dockerOrSkip(t)
	ctx, cancel := context.WithTimeout(context.Background(), itTimeout)
	defer cancel()

	e := newITEnv(t)
	in := newITInjector(t, e)

	set, err := in.CaptureBaselines(ctx)
	if err != nil {
		t.Fatalf("CaptureBaselines: %v", err)
	}
	if len(set.Nodes) != 2 {
		t.Fatalf("got %d baselines, want 2", len(set.Nodes))
	}
	base := set.Nodes[0]
	if len(base.IPTables) == 0 {
		t.Fatal("an unmodified container reported an empty iptables table; " +
			"if that is genuinely true on this host, D-026's baseline argument needs re-examining")
	}
	t.Logf("unmodified container carries %d iptables lines of its own at BOOT", len(base.IPTables))
	if len(base.Qdiscs) == 0 {
		t.Fatal("no qdiscs recorded at BOOT, so residual shaping could never be detected")
	}
	var haveEth bool
	for _, ifc := range base.Ifaces {
		if ifc.Name != "lo" && len(ifc.CIDRs) > 0 {
			haveEth = true
		}
	}
	if !haveEth {
		t.Fatalf("no non-loopback interface recorded: %+v", base.Ifaces)
	}

	// The snapshot must be stable: taken twice against an unchanged container it
	// must produce an identical diff, or every HEAL is noise.
	rep, err := in.VerifyResidual(ctx)
	if err != nil {
		t.Fatalf("VerifyResidual: %v", err)
	}
	if !rep.Clean() {
		t.Fatalf("an untouched topology reported residue: %v", rep.Err())
	}
}

// The full contract, end to end: inject, observe the effect, withdraw, verify
// clean.
func TestIntegrationLatencyInjectObserveWithdraw(t *testing.T) {
	dockerOrSkip(t)
	ctx, cancel := context.WithTimeout(context.Background(), itTimeout)
	defer cancel()

	e := newITEnv(t)
	in := newITInjector(t, e)
	if _, err := in.CaptureBaselines(ctx); err != nil {
		t.Fatalf("CaptureBaselines: %v", err)
	}

	before, ok := e.rttMS("a", "b")
	if !ok {
		t.Fatal("baseline ping failed; the topology is not usable")
	}
	if before > 50 {
		t.Fatalf("baseline RTT is already %.1f ms; this host is too loaded to measure a 200 ms injection", before)
	}

	f, err := in.NewNetFault(NetRequest{
		FaultID: "f0001",
		Spec:    mustSpec(t, "net.latency(a, 200, 10)@0..5000"),
	})
	if err != nil {
		t.Fatalf("NewNetFault: %v", err)
	}
	if err := f.Inject(ctx); err != nil {
		t.Fatalf("Inject: %v", err)
	}

	during, ok := e.rttMS("a", "b")
	if !ok {
		t.Fatal("ping failed while a latency fault was active; latency must not sever the link")
	}
	if during < 150 {
		t.Fatalf("RTT under a 200 ms delay is %.1f ms; the fault did not take effect", during)
	}

	// The health-probe path must survive: only peer-addressed traffic is
	// shaped, so the container's own loopback is untouched. This is what makes
	// the fault gray rather than fatal.
	if _, ok := e.exec("a", "ping -c 2 -W 2 -q 127.0.0.1"); !ok {
		t.Fatal("loopback was shaped; a latency fault must not touch the node's own health path")
	}

	if err := f.Withdraw(ctx); err != nil {
		t.Fatalf("Withdraw: %v", err)
	}
	after, ok := e.rttMS("a", "b")
	if !ok {
		t.Fatal("ping failed after withdrawal")
	}
	if after > 50 {
		t.Fatalf("RTT after withdrawal is %.1f ms; the delay was not removed", after)
	}
	t.Logf("latency: %.1f ms -> %.1f ms -> %.1f ms", before, during, after)

	rep, err := in.VerifyResidual(ctx)
	if err != nil {
		t.Fatalf("VerifyResidual: %v", err)
	}
	if !rep.Clean() {
		t.Fatalf("residue after withdrawal: %v", rep.Err())
	}
}

// A partition must cut BOTH directions. A single `-I INPUT -s peer -j DROP` is
// a one-way drop, and a cluster does not observe a one-way drop as a partition.
func TestIntegrationPartitionIsBidirectional(t *testing.T) {
	dockerOrSkip(t)
	ctx, cancel := context.WithTimeout(context.Background(), itTimeout)
	defer cancel()

	e := newITEnv(t)
	in := newITInjector(t, e)
	if _, err := in.CaptureBaselines(ctx); err != nil {
		t.Fatalf("CaptureBaselines: %v", err)
	}

	if !e.reachable("a", "b") || !e.reachable("b", "a") {
		t.Fatal("the topology is not connected before the fault")
	}

	f, err := in.NewNetFault(NetRequest{
		FaultID: "f0002",
		Spec:    mustSpec(t, "net.partition(a)@0..5000"),
	})
	if err != nil {
		t.Fatalf("NewNetFault: %v", err)
	}
	if err := f.Inject(ctx); err != nil {
		t.Fatalf("Inject: %v", err)
	}

	if e.reachable("a", "b") {
		t.Fatal("a can still reach b: the OUTPUT half of the partition is missing")
	}
	// The half a one-way drop would leave standing. Rules are installed only in
	// a's namespace, so this asserts that the cut is symmetric WITHOUT a second
	// sidecar at the far end.
	if e.reachable("b", "a") {
		t.Fatal("b can still reach a: this is a one-way drop, not a partition (D-026)")
	}

	if err := f.Withdraw(ctx); err != nil {
		t.Fatalf("Withdraw: %v", err)
	}
	if !e.reachable("a", "b") || !e.reachable("b", "a") {
		t.Fatal("connectivity was not restored after withdrawal")
	}

	rep, err := in.VerifyResidual(ctx)
	if err != nil {
		t.Fatalf("VerifyResidual: %v", err)
	}
	if !rep.Clean() {
		t.Fatalf("residue after withdrawal: %v", rep.Err())
	}
}

// The two halves of D-026, demonstrated rather than asserted: an UNTAGGED rule
// the target installs itself must not fail HEAL, and a TAGGED one must.
func TestIntegrationResidualDistinguishesOwnershipFromDrift(t *testing.T) {
	dockerOrSkip(t)
	ctx, cancel := context.WithTimeout(context.Background(), itTimeout)
	defer cancel()

	e := newITEnv(t)
	in := newITInjector(t, e)
	if _, err := in.CaptureBaselines(ctx); err != nil {
		t.Fatalf("CaptureBaselines: %v", err)
	}

	// A rule the TARGET owns, arriving after BOOT.
	if _, ok := e.exec("a", "true"); !ok {
		t.Fatal("cannot exec in the target")
	}
	sc := in.Sidecar()
	if _, err := sc.Exec(ctx, e.containers["a"],
		"iptables -w -I INPUT 1 -p tcp --dport 9999 -j ACCEPT"); err != nil {
		t.Fatalf("install an untagged rule: %v", err)
	}
	rep, err := in.VerifyResidual(ctx)
	if err != nil {
		t.Fatalf("VerifyResidual: %v", err)
	}
	if !rep.Clean() {
		t.Fatalf("an untagged rule the target added was reported as our residue: %v", rep.Err())
	}
	var sawDrift bool
	for _, n := range rep.Nodes {
		if n.NodeID == "a" && len(n.RuleDrift) > 0 {
			sawDrift = true
		}
	}
	if !sawDrift {
		t.Fatal("the untagged rule was not reported as drift; it should be visible for diagnosis")
	}

	// A rule WE own, left behind.
	tag := MustTag("r_2026_09_07_a41f", "leaked")
	if _, err := sc.Exec(ctx, e.containers["a"],
		"iptables -w -I INPUT 1 -p tcp --dport 9998 -m comment --comment '"+tag+"' -j DROP"); err != nil {
		t.Fatalf("install a tagged rule: %v", err)
	}
	rep, err = in.VerifyResidual(ctx)
	if err != nil {
		t.Fatalf("VerifyResidual: %v", err)
	}
	if rep.Clean() {
		t.Fatal("a surviving tagged rule was not detected; every later world would be poisoned")
	}

	// And the recovery path clears it.
	if err := in.SweepTagged(ctx); err != nil {
		t.Fatalf("SweepTagged: %v", err)
	}
	rep, err = in.VerifyResidual(ctx)
	if err != nil {
		t.Fatalf("VerifyResidual: %v", err)
	}
	if !rep.Clean() {
		t.Fatalf("sweeping did not clear the tagged rule: %v", rep.Err())
	}
}

// D-025 left one thing measured but unverified: netem STORES a seed, but that
// identical seeds give identical drop sequences was never confirmed end to end.
// This confirms it, and reports the reproduction rate rather than assuming it.
func TestIntegrationSeededLossReproduces(t *testing.T) {
	dockerOrSkip(t)
	ctx, cancel := context.WithTimeout(context.Background(), itTimeout)
	defer cancel()

	e := newITEnv(t)
	in := newITInjector(t, e)
	if _, err := in.CaptureBaselines(ctx); err != nil {
		t.Fatalf("CaptureBaselines: %v", err)
	}

	// Three trials of the SAME fault id against the same world seed, so the
	// derived netem seed is identical every time.
	const trials = 3
	received := make([]int, trials)
	for i := 0; i < trials; i++ {
		f, err := in.NewNetFault(NetRequest{
			FaultID: "floss",
			Spec:    mustSpec(t, "net.loss(a, 40)@0..5000"),
		})
		if err != nil {
			t.Fatalf("NewNetFault: %v", err)
		}
		if err := f.Inject(ctx); err != nil {
			t.Fatalf("Inject: %v", err)
		}
		out, _ := e.exec("a", fmt.Sprintf("ping -c 40 -i 0.05 -W 1 -q %s", e.addrs["b"]))
		received[i] = parsePingReceived(out)
		if err := f.Withdraw(ctx); err != nil {
			t.Fatalf("Withdraw: %v", err)
		}
	}

	same := 0
	for _, r := range received {
		if r == received[0] {
			same++
		}
	}
	t.Logf("seeded net.loss(40%%) reproduction: packets received per trial = %v (%d/%d identical)",
		received, same, trials)
	if received[0] == 0 || received[0] == 40 {
		t.Fatalf("a 40%% loss injection delivered %d/40 packets; the fault did not take effect", received[0])
	}
	if same != trials {
		t.Fatalf("identical seeds produced %v; net.loss does not replay, and the verdict must not "+
			"claim determinism it does not have (D-025, OQ-009)", received)
	}

	rep, err := in.VerifyResidual(ctx)
	if err != nil {
		t.Fatalf("VerifyResidual: %v", err)
	}
	if !rep.Clean() {
		t.Fatalf("residue after the loss trials: %v", rep.Err())
	}
}

func parsePingReceived(out string) int {
	for _, l := range strings.Split(out, "\n") {
		i := strings.Index(l, " packets received")
		if i < 0 {
			continue
		}
		fields := strings.Fields(l[:i])
		if len(fields) == 0 {
			continue
		}
		n, err := strconv.Atoi(fields[len(fields)-1])
		if err != nil {
			continue
		}
		return n
	}
	return -1
}

// Every remaining shaping kind, injected and withdrawn against a real kernel.
// The Phase 2 definition of done is "every fault kind can be injected and
// cleanly withdrawn", and only a real tc can say whether the argument list is
// acceptable: netem's refusal to reorder without a delay was found exactly
// this way.
func TestIntegrationEveryNetKindInjectsAndWithdraws(t *testing.T) {
	dockerOrSkip(t)
	ctx, cancel := context.WithTimeout(context.Background(), itTimeout)
	defer cancel()

	e := newITEnv(t)
	in := newITInjector(t, e)
	if _, err := in.CaptureBaselines(ctx); err != nil {
		t.Fatalf("CaptureBaselines: %v", err)
	}

	cases := []string{
		"net.partition(a)@0..5000",
		"net.latency(a, 40, 5)@0..5000",
		"net.loss(a, 20)@0..5000",
		"net.reorder(a, 25)@0..5000",
		"net.duplicate(a, 10)@0..5000",
		"net.bandwidth(a, 1000000)@0..5000",
	}
	for i, fs := range cases {
		spec := mustSpec(t, fs)
		f, err := in.NewNetFault(NetRequest{FaultID: fmt.Sprintf("fk%d", i), Spec: spec})
		if err != nil {
			t.Fatalf("%s: NewNetFault: %v", fs, err)
		}
		if err := f.Inject(ctx); err != nil {
			t.Fatalf("%s: Inject: %v", fs, err)
		}
		if err := f.Withdraw(ctx); err != nil {
			t.Fatalf("%s: Withdraw: %v", fs, err)
		}
		rep, err := in.VerifyResidual(ctx)
		if err != nil {
			t.Fatalf("%s: VerifyResidual: %v", fs, err)
		}
		if !rep.Clean() {
			t.Fatalf("%s left residue: %v", fs, rep.Err())
		}
		t.Logf("%s: injected and withdrawn clean", spec)
	}
}

// Two shaping faults on one interface must coexist, and the first withdrawal
// must not tear out the root qdisc the second is still using.
func TestIntegrationConcurrentShapingCoexists(t *testing.T) {
	dockerOrSkip(t)
	ctx, cancel := context.WithTimeout(context.Background(), itTimeout)
	defer cancel()

	e := newITEnv(t)
	in := newITInjector(t, e)
	if _, err := in.CaptureBaselines(ctx); err != nil {
		t.Fatalf("CaptureBaselines: %v", err)
	}

	f1, err := in.NewNetFault(NetRequest{FaultID: "fc1", Spec: mustSpec(t, "net.latency(a, 150)@0..5000")})
	if err != nil {
		t.Fatalf("NewNetFault f1: %v", err)
	}
	f2, err := in.NewNetFault(NetRequest{FaultID: "fc2", Spec: mustSpec(t, "net.loss(a, 10)@0..5000")})
	if err != nil {
		t.Fatalf("NewNetFault f2: %v", err)
	}
	if err := f1.Inject(ctx); err != nil {
		t.Fatalf("Inject f1: %v", err)
	}
	if err := f2.Inject(ctx); err != nil {
		t.Fatalf("Inject f2: %v", err)
	}
	if rtt, ok := e.rttMS("a", "b"); !ok || rtt < 100 {
		t.Fatalf("RTT with a 150 ms delay and 10%% loss both active is %.1f ms (ok=%v)", rtt, ok)
	}
	if err := f1.Withdraw(ctx); err != nil {
		t.Fatalf("Withdraw f1: %v", err)
	}
	// f2 is still shaping, so its slot and the shared root must survive.
	if _, ok := e.rttMS("a", "b"); !ok {
		t.Fatal("the link died when the first of two shaping faults withdrew")
	}
	if err := f2.Withdraw(ctx); err != nil {
		t.Fatalf("Withdraw f2: %v", err)
	}
	rep, err := in.VerifyResidual(ctx)
	if err != nil {
		t.Fatalf("VerifyResidual: %v", err)
	}
	if !rep.Clean() {
		t.Fatalf("residue after both withdrawals: %v", rep.Err())
	}
}

// A sidecar whose command fails must still be removed. A leaked one holds a
// reference to the target's network namespace, which makes the target's own
// teardown block.
func TestIntegrationSidecarIsRemovedOnFailure(t *testing.T) {
	dockerOrSkip(t)
	ctx, cancel := context.WithTimeout(context.Background(), itTimeout)
	defer cancel()

	e := newITEnv(t)
	sc := &Sidecar{RunID: "r_2026_09_07_a41f"}
	if err := sc.EnsureImage(ctx); err != nil {
		t.Fatalf("EnsureImage: %v", err)
	}
	if _, err := sc.Exec(ctx, e.containers["a"], "exit 7"); err == nil {
		t.Fatal("a failing script must surface as an error")
	}
	strays, err := sc.SweepSidecars(ctx)
	if err != nil {
		t.Fatalf("SweepSidecars: %v", err)
	}
	if len(strays) != 0 {
		t.Fatalf("sidecar container(s) survived a failed command: %v", strays)
	}
}
