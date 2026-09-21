package faults

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/WrdCstlg/pro-thesis/internal/recorder"
)

func bootedInjector(t *testing.T, w *fakeWorld) (*Injector, *fakeRunner) {
	t.Helper()
	in, r, err := newFakeInjector(w)
	if err != nil {
		t.Fatalf("NewInjector: %v", err)
	}
	if _, err := in.CaptureBaselines(context.Background()); err != nil {
		t.Fatalf("CaptureBaselines: %v", err)
	}
	return in, r
}

func TestCaptureBaselinesSnapshotsEveryNode(t *testing.T) {
	in, r := bootedInjector(t, newFakeWorld())
	set := in.Baselines()
	if len(set.Nodes) != 3 {
		t.Fatalf("got %d baselines, want 3", len(set.Nodes))
	}
	if set.Schema != BaselineSchema {
		t.Fatalf("baseline schema = %q", set.Schema)
	}
	for _, b := range set.Nodes {
		if len(b.Ifaces) != 3 {
			t.Fatalf("node %s: got %d interfaces, want lo/eth0/eth1", b.NodeID, len(b.Ifaces))
		}
		if len(b.Qdiscs) != 3 {
			t.Fatalf("node %s: got %d qdiscs", b.NodeID, len(b.Qdiscs))
		}
	}
	if n := len(r.subcommands("run")); n != 3 {
		t.Fatalf("baseline used %d sidecars for 3 nodes", n)
	}
}

// An injector with no baseline must refuse to inject rather than proceed with
// nothing to judge residue against.
func TestInjectRequiresABaseline(t *testing.T) {
	in, _, err := newFakeInjector(newFakeWorld())
	if err != nil {
		t.Fatalf("NewInjector: %v", err)
	}
	_, err = in.NewNetFault(NetRequest{FaultID: "f1", Spec: mustSpec(t, "net.partition(kv-n1)@0..1000")})
	if err == nil || !strings.Contains(err.Error(), "BOOT baseline") {
		t.Fatalf("expected a missing-baseline refusal, got %v", err)
	}
}

func TestPartitionInjectAndWithdraw(t *testing.T) {
	w := newFakeWorld()
	in, r := bootedInjector(t, w)
	ctx := context.Background()

	f, err := in.NewNetFault(NetRequest{
		FaultID: "f0001",
		Spec:    mustSpec(t, "net.partition(kv-n1)@8400..10900"),
	})
	if err != nil {
		t.Fatalf("NewNetFault: %v", err)
	}
	if got := f.Peers(); len(got) != 2 || got[0] != "kv-n2" || got[1] != "kv-n3" {
		t.Fatalf("peers = %v, want the complement [kv-n2 kv-n3]", got)
	}
	if err := f.Inject(ctx); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	if !f.Active() {
		t.Fatal("fault reports inactive after a successful injection")
	}

	// One sidecar, at ONE endpoint, carrying every rule for the cut.
	var injectScript string
	for _, c := range r.subcommands("run") {
		if !strings.Contains(c.Script, "#ADDR") && !strings.Contains(c.Script, "#TAGGED") {
			if injectScript != "" {
				t.Fatalf("more than one injection sidecar ran for a single-node partition")
			}
			injectScript = c.Script
			if c.Target != "c-kv-n1" {
				t.Fatalf("injection ran in %s, want the selected node's namespace", c.Target)
			}
		}
	}
	// Four addresses (two peers x two networks), each dropped both ways.
	if n := strings.Count(injectScript, "-I INPUT 1"); n != 4 {
		t.Fatalf("got %d INPUT drops, want 4:\n%s", n, injectScript)
	}
	if n := strings.Count(injectScript, "-I OUTPUT 1"); n != 4 {
		t.Fatalf("got %d OUTPUT drops, want 4:\n%s", n, injectScript)
	}
	if !strings.HasPrefix(injectScript, "set -e\n") {
		t.Fatal("an injection script must abort on the first failed rule")
	}

	if err := f.Withdraw(ctx); err != nil {
		t.Fatalf("Withdraw: %v", err)
	}
	if f.Active() {
		t.Fatal("fault still reports active after withdrawal")
	}
	if err := f.Withdraw(ctx); err != nil {
		t.Fatalf("Withdraw is not idempotent: %v", err)
	}

	// Withdrawal is not `set -e`: every deletion must be attempted.
	var withdrawScript string
	for _, s := range r.scripts() {
		if strings.Contains(s, "#TAGGED") {
			withdrawScript = s
		}
	}
	if strings.HasPrefix(withdrawScript, "set -e") {
		t.Fatal("a withdrawal that aborts on the first failure turns a partial withdrawal " +
			"into an unrecoverable one")
	}
	if !strings.Contains(withdrawScript, "grep -F 'thesis:r_2026_09_07_a41f:f0001'") {
		t.Fatalf("withdrawal does not match on the ownership tag:\n%s", withdrawScript)
	}
}

// The KV fixture's stale read is only observable while the displaced leader is
// still reachable by a client. Dropping by PEER ADDRESS rather than by
// interface or blanket policy is what preserves that.
func TestPartitionNamesPeerAddressesOnly(t *testing.T) {
	w := newFakeWorld()
	in, r := bootedInjector(t, w)
	f, err := in.NewNetFault(NetRequest{FaultID: "f1", Spec: mustSpec(t, "net.partition(kv-n1)@0..1000")})
	if err != nil {
		t.Fatalf("NewNetFault: %v", err)
	}
	if err := f.Inject(context.Background()); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	for _, s := range r.scripts() {
		if strings.Contains(s, "#ADDR") || strings.Contains(s, "#TAGGED") {
			continue
		}
		if strings.Contains(s, "-i eth") || strings.Contains(s, "-o eth") {
			t.Fatalf("partition matches on an interface, which would cut the client plane too:\n%s", s)
		}
		if strings.Contains(s, "-P INPUT DROP") || strings.Contains(s, "-P OUTPUT DROP") {
			t.Fatalf("partition changes a chain policy, which models a dead node:\n%s", s)
		}
		// The node's own addresses must never appear: rules name the far end.
		if strings.Contains(s, "172.23.0.2/32") || strings.Contains(s, "172.30.0.2/32") {
			t.Fatalf("partition drops the injecting node's own address:\n%s", s)
		}
	}
}

func TestEdgeTargetCutsOnlyThatLink(t *testing.T) {
	in, r := bootedInjector(t, newFakeWorld())
	f, err := in.NewNetFault(NetRequest{
		FaultID: "f1",
		Spec:    mustSpec(t, "net.partition(kv-n1<->kv-n2)@0..1000"),
	})
	if err != nil {
		t.Fatalf("NewNetFault: %v", err)
	}
	if got := f.Peers(); len(got) != 1 || got[0] != "kv-n2" {
		t.Fatalf("edge peers = %v, want only the far endpoint", got)
	}
	if err := f.Inject(context.Background()); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	for _, s := range r.scripts() {
		if strings.Contains(s, "#ADDR") || strings.Contains(s, "#TAGGED") {
			continue
		}
		if strings.Contains(s, "172.23.0.4") || strings.Contains(s, "172.30.0.4") {
			t.Fatalf("an edge fault touched kv-n3's addresses:\n%s", s)
		}
	}
}

func TestShapingInstallsRootOnceAndRemovesItLast(t *testing.T) {
	in, r := bootedInjector(t, newFakeWorld())
	ctx := context.Background()

	f1, err := in.NewNetFault(NetRequest{FaultID: "f1", Spec: mustSpec(t, "net.latency(kv-n1, 40, 5)@0..1000")})
	if err != nil {
		t.Fatalf("NewNetFault f1: %v", err)
	}
	if err := f1.Inject(ctx); err != nil {
		t.Fatalf("Inject f1: %v", err)
	}
	f2, err := in.NewNetFault(NetRequest{FaultID: "f2", Spec: mustSpec(t, "net.loss(kv-n1, 30)@0..1000")})
	if err != nil {
		t.Fatalf("NewNetFault f2: %v", err)
	}
	if err := f2.Inject(ctx); err != nil {
		t.Fatalf("Inject f2: %v", err)
	}

	var injects []string
	for _, s := range r.scripts() {
		if !strings.Contains(s, "#ADDR") && !strings.Contains(s, "#TAGGED") {
			injects = append(injects, s)
		}
	}
	all := strings.Join(injects, "\n")
	// Two interfaces carry peer traffic, so the root goes on each exactly once
	// across both faults.
	if n := strings.Count(all, "root handle 1: prio"); n != 2 {
		t.Fatalf("root qdisc installed %d times, want once per shaped interface: \n%s", n, all)
	}
	if n := strings.Count(all, "handle 30: netem"); n != 2 {
		t.Fatalf("f1 should hold slot 3 on both interfaces, got %d installs:\n%s", n, all)
	}
	if n := strings.Count(all, "handle 40: netem"); n != 2 {
		t.Fatalf("f2 should hold slot 4 on both interfaces, got %d installs:\n%s", n, all)
	}

	// The first withdrawal must NOT remove the root: the other fault is still
	// using it.
	if err := f1.Withdraw(ctx); err != nil {
		t.Fatalf("Withdraw f1: %v", err)
	}
	w1 := lastWithdrawScript(r)
	if strings.Contains(w1, "qdisc del dev eth0 root") {
		t.Fatalf("first withdrawal removed the root qdisc while a second fault still shapes it:\n%s", w1)
	}
	if err := f2.Withdraw(ctx); err != nil {
		t.Fatalf("Withdraw f2: %v", err)
	}
	w2 := lastWithdrawScript(r)
	if !strings.Contains(w2, "qdisc del dev eth0 root handle 1:") {
		t.Fatalf("last withdrawal left the root qdisc behind:\n%s", w2)
	}
}

func lastWithdrawScript(r *fakeRunner) string {
	var last string
	for _, s := range r.scripts() {
		if strings.Contains(s, "#TAGGED") {
			last = s
		}
	}
	return last
}

// A partition applied to two of three nodes is a DIFFERENT fault, not a weaker
// one, so a partial failure must roll back rather than proceed.
func TestPartialInjectionRollsBack(t *testing.T) {
	w := newFakeWorld()
	w.injectFails["c-kv-n2"] = true
	in, r := bootedInjector(t, w)

	f, err := in.NewNetFault(NetRequest{FaultID: "f1", Spec: mustSpec(t, "net.partition(kv:*)@0..1000")})
	if err == nil {
		// kv:* selects all three nodes, leaving no complement.
		t.Fatalf("a partition of every node has no far side and must be refused, got %v", f)
	}

	f, err = in.NewNetFault(NetRequest{
		FaultID:    "f2",
		Spec:       mustSpec(t, "net.partition(kv:*)@0..1000"),
		Resolution: Resolution{Selected: []string{"kv-n1", "kv-n2"}},
	})
	if err != nil {
		t.Fatalf("NewNetFault: %v", err)
	}
	err = f.Inject(context.Background())
	if err == nil {
		t.Fatal("injection reported success although one node failed")
	}
	if !strings.Contains(err.Error(), "kv-n2") {
		t.Fatalf("error does not name the failing node: %v", err)
	}
	// Rollback must have swept BOTH nodes, including the one that succeeded.
	swept := map[string]bool{}
	for _, c := range r.subcommands("run") {
		if strings.Contains(c.Script, "#TAGGED") {
			swept[c.Target] = true
		}
	}
	if !swept["c-kv-n1"] || !swept["c-kv-n2"] {
		t.Fatalf("rollback swept %v, want both endpoints", swept)
	}
}

// A withdrawal whose self-check still counts tagged rules is a failed
// withdrawal, and saying so here beats discovering it at HEAL with no idea
// which fault left it.
func TestWithdrawDetectsSurvivingRules(t *testing.T) {
	w := newFakeWorld()
	w.taggedAfterWithdraw = 2
	in, _ := bootedInjector(t, w)

	f, err := in.NewNetFault(NetRequest{FaultID: "f1", Spec: mustSpec(t, "net.partition(kv-n1)@0..1000")})
	if err != nil {
		t.Fatalf("NewNetFault: %v", err)
	}
	if err := f.Inject(context.Background()); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	err = f.Withdraw(context.Background())
	if err == nil {
		t.Fatal("withdrawal reported success while its own self-check found 2 surviving rules")
	}
	if !strings.Contains(err.Error(), "survived") {
		t.Fatalf("error does not describe the residue: %v", err)
	}
}

func TestVerifyResidualCleanAndDirty(t *testing.T) {
	w := newFakeWorld()
	in, _ := bootedInjector(t, w)
	ctx := context.Background()

	rep, err := in.VerifyResidual(ctx)
	if err != nil {
		t.Fatalf("VerifyResidual: %v", err)
	}
	if !rep.Clean() {
		t.Fatalf("an untouched topology reported residue: %v", rep.Err())
	}

	// Now leave a tagged rule behind on one node.
	w.capture["c-kv-n2"] = strings.Replace(w.capture["c-kv-n2"],
		":OUTPUT ACCEPT [0:0]",
		":OUTPUT ACCEPT [0:0]\n-A INPUT -s 172.30.0.2/32 -m comment --comment \"thesis:r_2026_09_07_a41f:f1\" -j DROP",
		1)
	rep, err = in.VerifyResidual(ctx)
	if err != nil {
		t.Fatalf("VerifyResidual: %v", err)
	}
	if rep.Clean() {
		t.Fatal("a surviving tagged rule was not detected")
	}
	if !strings.Contains(rep.Err().Error(), "kv-n2") {
		t.Fatalf("residual error does not name the node: %v", rep.Err())
	}
}

func TestVerifyResidualTreatsAGoneContainerAsClean(t *testing.T) {
	w := newFakeWorld()
	in, _ := bootedInjector(t, w)
	w.running["c-kv-n3"] = false

	rep, err := in.VerifyResidual(context.Background())
	if err != nil {
		t.Fatalf("VerifyResidual: %v", err)
	}
	if !rep.Clean() {
		t.Fatalf("a container that is gone took its netns with it: %v", rep.Err())
	}
	var found bool
	for _, n := range rep.Nodes {
		if n.NodeID == "kv-n3" && n.NotRunning {
			found = true
		}
	}
	if !found {
		t.Fatal("the report does not say why kv-n3 was not inspected")
	}
}

func TestVerifyResidualRefusesWithoutBaseline(t *testing.T) {
	in, _, err := newFakeInjector(newFakeWorld())
	if err != nil {
		t.Fatalf("NewInjector: %v", err)
	}
	if _, err := in.VerifyResidual(context.Background()); err == nil {
		t.Fatal("residual verification without a baseline would be comparing against an " +
			"assumed-empty table, which D-026 forbids")
	}
}

// Same world seed, same fault, same node, same interface => same netem seed.
// Without this, net.loss/reorder/duplicate do not replay (D-025).
func TestNetemSeedsAreDeterministicAndIndependent(t *testing.T) {
	a := NewSeeder(20260906)
	b := NewSeeder(20260906)
	if a("f1", "kv-n1", "eth1") != b("f1", "kv-n1", "eth1") {
		t.Fatal("the same (seed, fault, node, iface) produced two different netem seeds")
	}
	if a("f1", "kv-n1", "eth1") == a("f1", "kv-n1", "eth0") {
		t.Fatal("two interfaces of one node share a netem seed")
	}
	if a("f1", "kv-n1", "eth1") == a("f2", "kv-n1", "eth1") {
		t.Fatal("two faults share a netem seed")
	}
	if a("f1", "kv-n1", "eth1") == NewSeeder(20260907)("f1", "kv-n1", "eth1") {
		t.Fatal("two world seeds produced the same netem seed")
	}
	if a("f1", "kv-n1", "eth1") == 0 {
		t.Fatal("a zero seed reads like an absent one in a tc dump")
	}
}

// Adding a fault must not perturb the seed of a fault that already existed:
// the property path-keyed derivation exists to give (D-013).
func TestNetemSeedsDoNotDependOnInjectionOrder(t *testing.T) {
	s := NewSeeder(7)
	first := s("f1", "n1", "eth0")
	for i := 0; i < 100; i++ {
		s("other", "n1", "eth0")
	}
	if s("f1", "n1", "eth0") != first {
		t.Fatal("drawing other seeds changed an existing one; the derivation is not path-keyed")
	}
}

func TestLedgerRecordsSeedsAndCommands(t *testing.T) {
	in, _ := bootedInjector(t, newFakeWorld())
	ctx := context.Background()
	f, err := in.NewNetFault(NetRequest{FaultID: "f1", Spec: mustSpec(t, "net.loss(kv-n1, 30)@0..1000")})
	if err != nil {
		t.Fatalf("NewNetFault: %v", err)
	}
	if err := f.Inject(ctx); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	if err := f.Withdraw(ctx); err != nil {
		t.Fatalf("Withdraw: %v", err)
	}
	l := in.Ledger()
	if len(l) != 1 {
		t.Fatalf("got %d ledger entries, want 1", len(l))
	}
	e := l[0]
	if e.Tag != "thesis:r_2026_09_07_a41f:f1" {
		t.Fatalf("ledger tag = %q", e.Tag)
	}
	if len(e.Seeds) != 2 {
		t.Fatalf("got %d seed records, want one per shaped interface: %+v", len(e.Seeds), e.Seeds)
	}
	if e.StartWallNS == 0 || e.EndWallNS <= e.StartWallNS {
		t.Fatalf("ledger window is %d..%d", e.StartWallNS, e.EndWallNS)
	}
	if len(e.Commands) == 0 || len(e.Withdrawal) == 0 {
		t.Fatal("the ledger must carry the exact commands, so a human can see what ran")
	}
	if _, err := in.EncodeLedger(); err != nil {
		t.Fatalf("EncodeLedger: %v", err)
	}
}

func TestReorderRecordsItsEnablingDelay(t *testing.T) {
	in, _ := bootedInjector(t, newFakeWorld())
	ctx := context.Background()
	f, err := in.NewNetFault(NetRequest{FaultID: "f1", Spec: mustSpec(t, "net.reorder(kv-n1, 25)@0..1000")})
	if err != nil {
		t.Fatalf("NewNetFault: %v", err)
	}
	if err := f.Inject(ctx); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	if err := f.Withdraw(ctx); err != nil {
		t.Fatalf("Withdraw: %v", err)
	}
	notes := strings.Join(in.Ledger()[0].Notes, "\n")
	if !strings.Contains(notes, "enabling delay") {
		t.Fatalf("the added delay is not disclosed in the ledger: %q", notes)
	}
}

// A realized entry must bind a dynamic target to the concrete node, so a world
// file replays the fault that actually happened rather than the one that was
// planned (D-012).
func TestRealizedFaultBindsTheResolvedTarget(t *testing.T) {
	in, _ := bootedInjector(t, newFakeWorld())
	ctx := context.Background()
	f, err := in.NewNetFault(NetRequest{
		FaultID:    "f1",
		Spec:       mustSpec(t, "net.partition(minority(kv))@8400..10900"),
		Resolution: Resolution{Selected: []string{"kv-n2"}},
	})
	if err != nil {
		t.Fatalf("NewNetFault: %v", err)
	}
	if _, _, ok := f.Window(); ok {
		t.Fatal("a fault that has not been injected has no realized window")
	}
	if err := f.Inject(ctx); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	if err := f.Withdraw(ctx); err != nil {
		t.Fatalf("Withdraw: %v", err)
	}

	startNS, endNS, ok := f.Window()
	if !ok || endNS <= startNS {
		t.Fatalf("realized window = %d..%d, ok=%v", startNS, endNS, ok)
	}

	// Anchor a timeline so DRIVE started 1 ms before the injection.
	tl := &recorder.Timeline{}
	if err := tl.StartDriveAt(recorder.RTime(0), startNS-int64(time.Millisecond)); err != nil {
		t.Fatalf("StartDriveAt: %v", err)
	}
	rf, ok := f.RealizedFault(tl)
	if !ok {
		t.Fatal("RealizedFault could not place the fault on the virtual clock")
	}
	if rf.Fault != "net.partition(minority(kv))@8400..10900" {
		t.Fatalf("planned = %q", rf.Fault)
	}
	if rf.Resolved != "net.partition(kv-n2)@8400..10900" {
		t.Fatalf("resolved = %q, want the target bound to the concrete node", rf.Resolved)
	}
	if len(rf.Nodes) != 1 || rf.Nodes[0] != "kv-n2" {
		t.Fatalf("nodes = %v", rf.Nodes)
	}
	if rf.StartMS != 1 {
		t.Fatalf("start_ms = %d, want the measured offset from DRIVE, not the planned 8400", rf.StartMS)
	}
}

// A timeline with no DRIVE origin must yield ok=false rather than a
// plausible-looking zero: a fault misplaced at t+0 would make the causal
// timeline lie about ordering.
func TestRealizedFaultRefusesAnUnanchoredTimeline(t *testing.T) {
	in, _ := bootedInjector(t, newFakeWorld())
	f, err := in.NewNetFault(NetRequest{FaultID: "f1", Spec: mustSpec(t, "net.partition(kv-n1)@0..1000")})
	if err != nil {
		t.Fatalf("NewNetFault: %v", err)
	}
	if err := f.Inject(context.Background()); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	if _, ok := f.RealizedFault(&recorder.Timeline{}); ok {
		t.Fatal("a timeline with no origin cannot place a fault on the virtual clock")
	}
	if _, ok := f.RealizedFault(nil); ok {
		t.Fatal("a nil timeline cannot place a fault on the virtual clock")
	}
}

// A fault this injector cannot carry out must say so with a sentinel a
// scheduler can route on, rather than reporting success over an unperturbed
// world.
func TestNonNetworkKindIsRefused(t *testing.T) {
	in, _ := bootedInjector(t, newFakeWorld())
	_, err := in.NewNetFault(NetRequest{FaultID: "f1", Spec: mustSpec(t, "proc.pause(kv-n1)@0..1000")})
	if err == nil {
		t.Fatal("the network injector accepted a process fault")
	}
	if !errors.Is(err, ErrNotNetworkFault) {
		t.Fatalf("error does not wrap ErrNotNetworkFault: %v", err)
	}
}

func TestConcurrentShapingClaimsDistinctSlots(t *testing.T) {
	in, _ := bootedInjector(t, newFakeWorld())
	ctx := context.Background()

	const n = 6
	faults := make([]*NetFault, n)
	for i := 0; i < n; i++ {
		spec := mustSpec(t, "net.latency(kv-n1, 40)@0..1000")
		f, err := in.NewNetFault(NetRequest{FaultID: faultID(i), Spec: spec})
		if err != nil {
			t.Fatalf("NewNetFault %d: %v", i, err)
		}
		faults[i] = f
	}
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = faults[i].Inject(ctx)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("Inject %d: %v", i, err)
		}
	}

	seen := map[int]string{}
	for i, f := range faults {
		f.mu.Lock()
		for _, p := range f.plans {
			for _, s := range p.slots {
				if s.iface != "eth0" {
					continue
				}
				if prev, dup := seen[s.slot]; dup {
					f.mu.Unlock()
					t.Fatalf("fault %d and fault %s both claimed eth0 slot %d", i, prev, s.slot)
				}
				seen[s.slot] = faultID(i)
			}
		}
		f.mu.Unlock()
	}
	if len(seen) != n {
		t.Fatalf("got %d distinct eth0 slots for %d faults", len(seen), n)
	}
	for _, f := range faults {
		if err := f.Withdraw(ctx); err != nil {
			t.Fatalf("Withdraw: %v", err)
		}
	}
	in.mu.Lock()
	left := len(in.slots)
	in.mu.Unlock()
	if left != 0 {
		t.Fatalf("%d interface slot table(s) survived every withdrawal", left)
	}
}

func faultID(i int) string { return "f" + string(rune('a'+i)) }

func TestSlotExhaustionIsAnError(t *testing.T) {
	in, _ := bootedInjector(t, newFakeWorld())
	ctx := context.Background()
	for i := 0; i <= LastSlot-FirstSlot+1; i++ {
		f, err := in.NewNetFault(NetRequest{FaultID: faultID(i), Spec: mustSpec(t, "net.latency(kv-n1, 40)@0..1000")})
		if err != nil {
			t.Fatalf("NewNetFault %d: %v", i, err)
		}
		err = f.Inject(ctx)
		if i < LastSlot-FirstSlot+1 {
			if err != nil {
				t.Fatalf("Inject %d: %v", i, err)
			}
			continue
		}
		if err == nil {
			t.Fatal("the prio qdisc has 14 usable bands; the 15th shaping fault must be refused, " +
				"not silently dropped")
		}
	}
}
