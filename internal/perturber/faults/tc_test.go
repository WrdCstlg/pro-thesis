package faults

import (
	"strings"
	"testing"

	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

func mustSpec(t *testing.T, s string) schema.FaultSpec {
	t.Helper()
	spec, err := schema.ParseFault(s)
	if err != nil {
		t.Fatalf("ParseFault(%q): %v", s, err)
	}
	return spec
}

func TestNetemArgs(t *testing.T) {
	cases := []struct {
		fault string
		want  []string
	}{
		{"net.latency(n1, 40, 5)@0..1", []string{"delay", "40ms", "5ms", "seed", "99"}},
		{"net.latency(n1, 40)@0..1", []string{"delay", "40ms", "seed", "99"}},
		{"net.loss(n1, 30)@0..1", []string{"loss", "30%", "seed", "99"}},
		{"net.duplicate(n1, 10)@0..1", []string{"duplicate", "10%", "seed", "99"}},
		{"net.bandwidth(n1, 1000000)@0..1", []string{"rate", "1000000", "seed", "99"}},
		// netem refuses `reorder` with no delay; the enabling delay is added.
		{"net.reorder(n1, 25)@0..1", []string{"delay", "10ms", "reorder", "25%", "seed", "99"}},
	}
	for _, tc := range cases {
		got, err := NetemArgs(mustSpec(t, tc.fault), 99)
		if err != nil {
			t.Fatalf("%s: %v", tc.fault, err)
		}
		if strings.Join(got, " ") != strings.Join(tc.want, " ") {
			t.Fatalf("%s: got %v, want %v", tc.fault, got, tc.want)
		}
	}
}

// D-025: netem invents a fresh random seed when none is given; measured, three
// injections, three different seeds. Every qdisc must be seeded, including the
// non-stochastic ones, so the realized world records the value a replay will
// install.
func TestEveryNetemQdiscIsSeeded(t *testing.T) {
	for _, f := range []string{
		"net.latency(n1, 40)@0..1",
		"net.loss(n1, 30)@0..1",
		"net.reorder(n1, 25)@0..1",
		"net.duplicate(n1, 10)@0..1",
		"net.bandwidth(n1, 1000000)@0..1",
	} {
		args, err := NetemArgs(mustSpec(t, f), 12345)
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		joined := strings.Join(args, " ")
		if !strings.Contains(joined, "seed 12345") {
			t.Fatalf("%s produced %q with no seed", f, joined)
		}
	}
}

// Parameter values are re-emitted as SOURCE TEXT. A value that round-tripped
// through a float would change the canonical fault string and with it the hash
// of every world containing the fault.
func TestNetemArgsPreservesSourceText(t *testing.T) {
	args, err := NetemArgs(mustSpec(t, "net.loss(n1, 0.5)@0..1"), 1)
	if err != nil {
		t.Fatalf("NetemArgs: %v", err)
	}
	if strings.Join(args, " ") != "loss 0.5% seed 1" {
		t.Fatalf("got %v, want the source text 0.5 preserved", args)
	}
}

func TestNetemArgsRejectsNoOps(t *testing.T) {
	for _, f := range []string{
		"net.loss(n1, 0)@0..1",
		"net.reorder(n1, 0)@0..1",
		"net.duplicate(n1, 0)@0..1",
		"net.bandwidth(n1, 0)@0..1",
		"net.latency(n1, 0, 0)@0..1",
	} {
		if _, err := NetemArgs(mustSpec(t, f), 1); err == nil {
			t.Fatalf("%s injects nothing observable and must be rejected, not silently applied", f)
		}
	}
}

func TestNetemArgsRejectsNonShapingKind(t *testing.T) {
	if _, err := NetemArgs(mustSpec(t, "net.partition(n1)@0..1"), 1); err == nil {
		t.Fatal("net.partition is an iptables fault, not a netem one")
	}
}

// A default prio priomap spreads traffic across bands 0-2 by TOS, so the band
// the shaping recipe uses would also carry an arbitrary slice of unrelated
// traffic. Every TOS value must map to the default band.
func TestShapeRootSendsAllUnfilteredTrafficToOneBand(t *testing.T) {
	cmd, err := ShapeRootCommand("eth0")
	if err != nil {
		t.Fatalf("ShapeRootCommand: %v", err)
	}
	if !strings.Contains(cmd, "handle 1: prio bands 16") {
		t.Fatalf("root command %q is not the 16-band prio the slot scheme needs", cmd)
	}
	i := strings.Index(cmd, "priomap ")
	if i < 0 {
		t.Fatalf("root command %q has no priomap", cmd)
	}
	bands := strings.Fields(cmd[i+len("priomap "):])
	if len(bands) != 16 {
		t.Fatalf("priomap has %d entries, want 16", len(bands))
	}
	for _, b := range bands {
		if b != "1" {
			t.Fatalf("priomap entry %q would leak unfiltered traffic into a shaped band", b)
		}
	}
}

func TestShapeSlotCommands(t *testing.T) {
	cmds, err := ShapeSlotCommands("eth1", 4, []string{"delay", "40ms", "seed", "7"},
		[]string{"172.30.0.3", "172.30.0.2"})
	if err != nil {
		t.Fatalf("ShapeSlotCommands: %v", err)
	}
	if len(cmds) != 3 {
		t.Fatalf("got %d commands, want one qdisc and two filters: %v", len(cmds), cmds)
	}
	if !strings.Contains(cmds[0], "parent 1:4 handle 40: netem delay 40ms seed 7") {
		t.Fatalf("slot qdisc %q does not derive its handles from the slot number", cmds[0])
	}
	// Sorted, so the same fault produces the same script byte for byte.
	if !strings.Contains(cmds[1], "172.30.0.2/32") || !strings.Contains(cmds[2], "172.30.0.3/32") {
		t.Fatalf("filters are not in sorted address order: %v", cmds[1:])
	}
	for _, c := range cmds[1:] {
		if !strings.Contains(c, "prio 4") || !strings.Contains(c, "flowid 1:4") {
			t.Fatalf("filter %q does not use the slot's own pref and class", c)
		}
	}
}

func TestShapeSlotRefusesIPv6Destination(t *testing.T) {
	_, err := ShapeSlotCommands("eth0", 3, []string{"delay", "10ms"}, []string{"10.0.0.2", "fd00::2"})
	if err == nil {
		t.Fatal("shaping only the IPv4 half of a dual-stack path under-injects; " +
			"it must refuse rather than silently skip")
	}
}

func TestShapeSlotRejectsEmptyDestinations(t *testing.T) {
	if _, err := ShapeSlotCommands("eth0", 3, []string{"delay", "10ms"}, nil); err == nil {
		t.Fatal("a filter set matching no traffic is a silent no-op")
	}
}

func TestShapeSlotRejectsOutOfRangeSlot(t *testing.T) {
	for _, s := range []int{0, 1, 2, 17, 99} {
		if _, err := ShapeSlotCommands("eth0", s, []string{"delay", "1ms"}, []string{"10.0.0.1"}); err == nil {
			t.Fatalf("slot %d is outside the prio band range and must be rejected", s)
		}
	}
}

// Filters must go before the qdisc: deleting the qdisc first leaves filters
// pointing at a class with no qdisc, and `tc qdisc show` cannot see them, so
// the baseline diff would report the node clean.
func TestShapeSlotDeleteRemovesFiltersFirst(t *testing.T) {
	cmds, err := ShapeSlotDeleteCommands("eth0", 5)
	if err != nil {
		t.Fatalf("ShapeSlotDeleteCommands: %v", err)
	}
	if len(cmds) != 2 {
		t.Fatalf("got %d commands, want a filter delete and a qdisc delete: %v", len(cmds), cmds)
	}
	if !strings.HasPrefix(cmds[0], "tc filter del ") {
		t.Fatalf("first delete is %q, want the filter", cmds[0])
	}
	if !strings.Contains(cmds[1], "handle 50:") {
		t.Fatalf("qdisc delete %q does not name the slot's handle", cmds[1])
	}
	for _, c := range cmds {
		if !strings.HasSuffix(c, "|| true") {
			t.Fatalf("delete %q aborts the withdrawal script on failure; "+
				"a partial withdrawal must still attempt every deletion", c)
		}
	}
}

func TestRootQdiscKind(t *testing.T) {
	lines := []string{
		"qdisc noqueue 0: dev lo root",
		"qdisc noqueue 0: dev eth0 root",
		"qdisc netem 30: dev eth1 parent 1:3 limit 1000 delay 40ms",
	}
	if k, ok := RootQdiscKind(lines, "eth0"); !ok || k != "noqueue" {
		t.Fatalf("RootQdiscKind(eth0) = %q, %v; want noqueue, true", k, ok)
	}
	if _, ok := RootQdiscKind(lines, "eth1"); ok {
		t.Fatal("eth1 has no ROOT qdisc in the sample; a child qdisc must not be reported as one")
	}
	if _, ok := RootQdiscKind(lines, "eth9"); ok {
		t.Fatal("RootQdiscKind reported a root for an interface that is not present")
	}
}

// Installing a shaping root over an operator's own qdisc destroys a
// configuration `tc qdisc del root` cannot restore: it returns the KERNEL
// default, not what was there.
func TestCheckRootQdiscSafe(t *testing.T) {
	safe := Baseline{Qdiscs: []string{"qdisc noqueue 0: dev eth0 root"}}
	if err := CheckRootQdiscSafe("n1", safe, "eth0"); err != nil {
		t.Fatalf("noqueue root should be safe to displace: %v", err)
	}
	owned := Baseline{Qdiscs: []string{"qdisc htb 1: dev eth0 root refcnt 2 r2q 10"}}
	if err := CheckRootQdiscSafe("n1", owned, "eth0"); err == nil {
		t.Fatal("a target-owned htb root must not be silently displaced")
	}
	none := Baseline{Qdiscs: []string{"qdisc noqueue 0: dev lo root"}}
	if err := CheckRootQdiscSafe("n1", none, "eth0"); err == nil {
		t.Fatal("an interface with no recorded BOOT root qdisc must not be shaped")
	}
}

func TestSupportsAndIsShaping(t *testing.T) {
	for _, k := range []schema.FaultKind{
		schema.FaultNetPartition, schema.FaultNetLatency, schema.FaultNetLoss,
		schema.FaultNetReorder, schema.FaultNetDuplicate, schema.FaultNetBandwidth,
	} {
		if !Supports(k) {
			t.Fatalf("%s is a net.* kind and must be supported", k)
		}
	}
	for _, k := range []schema.FaultKind{
		schema.FaultProcPause, schema.FaultClockSkew, schema.FaultIOLatency, schema.FaultMemPressure,
	} {
		if Supports(k) {
			t.Fatalf("%s is not the network family's to inject", k)
		}
	}
	if IsShaping(schema.FaultNetPartition) {
		t.Fatal("net.partition is an iptables fault, not a netem one")
	}
}
