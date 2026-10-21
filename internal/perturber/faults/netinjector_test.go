package faults

import (
	"context"
	"strings"
	"testing"

	"github.com/WrdCstlg/pro-thesis/internal/perturber"
)

// The executor hands an edge `a<->b` down as Nodes [a, b] with Peers set to
// everyone else, the same shape it uses for a group partition. An adapter that
// reads that as "isolate {a, b} from the rest" cuts a<->c and b<->c and leaves
// a<->b open: the opposite of the fault the world records. Only the a<->b link
// may be touched, and it must be cut in both directions.
//
// The fixture addresses are kv-n1 .2, kv-n2 .3 and kv-n3 .4 on both the client
// (172.23.0.0/16) and peer (172.30.0.0/24) planes.
func TestNetInjectorEdgeCutsOnlyThatLink(t *testing.T) {
	cases := []struct {
		name string
		spec string
	}{
		{"partition", "net.partition(kv-n1<->kv-n2)@0..1000"},
		{"latency", "net.latency(kv-n1<->kv-n2, 40, 5)@0..1000"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in, r := bootedInjector(t, newFakeWorld())
			n := NewNetInjector(in)
			req := perturber.InjectRequest{
				RunID:     "r_2026_09_07_a41f",
				FaultID:   "f001",
				Spec:      mustSpec(t, tc.spec),
				Nodes:     []perturber.Node{{ID: "kv-n1", Service: "kv"}, {ID: "kv-n2", Service: "kv"}},
				Peers:     []perturber.Node{{ID: "kv-n3", Service: "kv"}},
				Ownership: "thesis:r_2026_09_07_a41f:f001",
			}
			if err := n.Inject(context.Background(), req); err != nil {
				t.Fatalf("Inject: %v", err)
			}

			var injections []fakeCall
			for _, c := range r.subcommands("run") {
				if strings.Contains(c.Script, "#ADDR") || strings.Contains(c.Script, "#TAGGED") {
					continue
				}
				injections = append(injections, c)
			}
			if len(injections) == 0 {
				t.Fatal("no injection sidecar ran")
			}
			sawFarEnd := false
			for _, c := range injections {
				// a<->c and b<->c: kv-n3's addresses must appear in no rule.
				if strings.Contains(c.Script, "172.23.0.4") || strings.Contains(c.Script, "172.30.0.4") {
					t.Fatalf("an edge kv-n1<->kv-n2 touched kv-n3's traffic (in %s):\n%s", c.Target, c.Script)
				}
				if c.Target == "c-kv-n1" && strings.Contains(c.Script, "172.30.0.3") {
					sawFarEnd = true
				}
			}
			if !sawFarEnd {
				t.Fatalf("no rule at kv-n1 names kv-n2, so the a<->b link was not impaired:\n%v", injections)
			}
			if tc.name == "partition" {
				s := injections[0].Script
				if strings.Count(s, "-I INPUT 1") != 2 || strings.Count(s, "-I OUTPUT 1") != 2 {
					t.Fatalf("want kv-n2's two addresses dropped in both directions, got:\n%s", s)
				}
			}

			if err := n.Withdraw(context.Background(), perturber.WithdrawRequest{
				RunID: req.RunID, FaultID: req.FaultID, Spec: req.Spec, Nodes: req.Nodes, Peers: req.Peers,
			}); err != nil {
				t.Fatalf("Withdraw: %v", err)
			}
		})
	}
}

// Withdrawal of an edge that is not in the live map (a resumed harness) rebuilds
// the fault from the request. It must rebuild the same edge, not a group cut,
// or the rebuilt fault would be refused or would target the wrong peers.
func TestNetInjectorRebuildsAnEdgeForWithdrawal(t *testing.T) {
	in, _ := bootedInjector(t, newFakeWorld())
	n := NewNetInjector(in)
	err := n.Withdraw(context.Background(), perturber.WithdrawRequest{
		RunID:   "r_2026_09_07_a41f",
		FaultID: "f009",
		Spec:    mustSpec(t, "net.partition(kv-n1<->kv-n2)@0..1000"),
		Nodes:   []perturber.Node{{ID: "kv-n1", Service: "kv"}, {ID: "kv-n2", Service: "kv"}},
		Peers:   []perturber.Node{{ID: "kv-n3", Service: "kv"}},
	})
	if err != nil {
		t.Fatalf("Withdraw of an unknown edge fault: %v", err)
	}
	got := n.Realized()
	if len(got) != 1 {
		t.Fatalf("Realized() = %d faults, want 1", len(got))
	}
	if p := got[0].Peers(); len(p) != 1 || p[0] != "kv-n2" {
		t.Fatalf("rebuilt edge has peers %v, want only the far endpoint [kv-n2]", p)
	}
}
