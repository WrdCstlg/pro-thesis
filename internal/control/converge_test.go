package control

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/WrdCstlg/pro-thesis/internal/recorder"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// healthServer answers /health with the given status and everything else
// (including the reference fixture's /healthz) with 404, exactly as etcd does.
func healthServer(t *testing.T, status int) (*httptest.Server, int64) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(status)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	port, err := strconv.ParseInt(srv.URL[strings.LastIndex(srv.URL, ":")+1:], 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return srv, port
}

// counterClock returns 1814, 1815, 1816, ...: a distinct stamp per call, so a
// probe stamped once for the whole round is visible.
func counterClock(start int64) func() int64 {
	n := start - 1
	return func() int64 { n++; return n }
}

// OQ-063. The post-HEAL convergence probe judged by availability_after_heal
// must use the probe template the node's own `harness.health` entry declares,
// expanded against THAT node's published port, and must call any 2xx
// available. Every node gets its own server here so a probe sent to the wrong
// node's port, a fixed path, or a non-2xx counted as available each fail.
func TestConvergenceProbesUseEachNodesOwnTemplatePortAndStatus(t *testing.T) {
	srv1, port1 := healthServer(t, http.StatusOK)
	_, port2 := healthServer(t, http.StatusNotFound)
	_, port3 := healthServer(t, http.StatusNoContent)
	cfg := &schema.Config{Harness: schema.HarnessConfig{
		Nodes: []schema.NodeConfig{
			{ID: "etcd-n1", Service: "etcd"},
			{ID: "etcd-n2", Service: "etcd"},
			{ID: "etcd-n3", Service: "etcd"},
		},
		Health: []schema.HealthProbe{{Node: "etcd:*", Probe: "http://{host}:{port}/health"}},
	}}
	top := &recorder.Topology{Nodes: []recorder.NodeBinding{
		{ID: "etcd-n1", HostPort: port1},
		{ID: "etcd-n2", HostPort: port2},
		{ID: "etcd-n3", HostPort: port3},
	}}

	probes, err := convergenceProbes(cfg, top, counterClock(1814), srv1.Client())
	if err != nil {
		t.Fatal(err)
	}
	if len(probes) != 3 {
		t.Fatalf("got %d probe observation(s), want one per node", len(probes))
	}
	want := []struct {
		node string
		port int64
		ok   bool
		err  string
		tms  int64
	}{
		{"etcd-n1", port1, true, "", 1814},
		{"etcd-n2", port2, false, "HTTP status 404", 1815},
		{"etcd-n3", port3, true, "", 1816},
	}
	for i, w := range want {
		p := probes[i]
		if p.NodeID != w.node {
			t.Fatalf("probe %d is for %s, want %s", i, p.NodeID, w.node)
		}
		if wantURL := fmt.Sprintf("http://127.0.0.1:%d/health", w.port); p.Target != wantURL {
			t.Fatalf("%s was probed at %s, want %s (its own port, its declared path)", w.node, p.Target, wantURL)
		}
		if p.OK != w.ok || p.Err != w.err {
			t.Fatalf("%s: OK=%v Err=%q, want OK=%v Err=%q", w.node, p.OK, p.Err, w.ok, w.err)
		}
		if p.TMS != w.tms {
			t.Fatalf("%s stamped t+%dms, want t+%dms: each attempt is stamped when it is taken", w.node, p.TMS, w.tms)
		}
	}
}

// A node no health entry covers must not be probed at an invented URL. No
// observation is the honest output; the oracle reports the node as never
// probed and returns INCONCLUSIVE, and config validation refuses the
// combination up front.
func TestConvergenceProbesDoNotInventAURLForANodeWithoutAHealthEntry(t *testing.T) {
	srv, port := healthServer(t, http.StatusOK)
	cfg := &schema.Config{Harness: schema.HarnessConfig{
		Nodes: []schema.NodeConfig{
			{ID: "n1", Service: "kv"},
			{ID: "n2", Service: "kv"},
		},
		Health: []schema.HealthProbe{{Node: "n1", Probe: "http://{host}:{port}/health"}},
	}}
	top := &recorder.Topology{Nodes: []recorder.NodeBinding{
		{ID: "n1", HostPort: port},
		{ID: "n2", HostPort: port},
	}}

	probes, err := convergenceProbes(cfg, top, counterClock(0), srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	if len(probes) != 1 || probes[0].NodeID != "n1" {
		t.Fatalf("got %+v; want exactly one observation, for n1, and none invented for n2", probes)
	}
}
