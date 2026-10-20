package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// node answers GET /nodes the way rqlite v10.5.3 does (http/service.go
// handleNodes, http/nodes.go): with ?ver=2, {"nodes":[...]} of voters sorted by
// id, each carrying id, api_addr (the node's -http-adv-addr as a URL), addr
// (its Raft address), voter, reachable and leader. `leader` is THIS node's
// view: the node whose Raft address is the leader this node knows. Without
// ver=2 it answers the legacy shape, a map keyed by id, which a probe that
// forgot ver=2 cannot read as a list.
//
// view names which node this one believes leads ("" for none). The api_addr
// carries the container host name, never the address the probe dialled.
func node(t *testing.T, view string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/nodes" || r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		type n struct {
			ID        string `json:"id"`
			APIAddr   string `json:"api_addr"`
			Addr      string `json:"addr"`
			Version   string `json:"version"`
			Voter     bool   `json:"voter"`
			Reachable bool   `json:"reachable"`
			Leader    bool   `json:"leader"`
		}
		var list []n
		for _, id := range []string{"rq-n1", "rq-n2", "rq-n3"} {
			list = append(list, n{ID: id, APIAddr: "http://" + id + ":4001", Addr: id + ":4002",
				Version: "v10.5.3", Voter: true, Reachable: true, Leader: id == view})
		}
		if view != "" && !strings.HasPrefix(view, "rq-n") {
			// A leader whose advertised address is no node of this world.
			list = append(list, n{ID: "stray", APIAddr: "http://" + view + ":4001", Addr: view + ":4002",
				Voter: true, Reachable: true, Leader: true})
		}
		if r.URL.Query().Get("ver") != "2" {
			legacy := map[string]n{}
			for _, x := range list {
				legacy[x.ID] = x
			}
			_ = json.NewEncoder(w).Encode(legacy)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"nodes": list})
	}))
	t.Cleanup(srv.Close)
	return srv
}

type roleLine struct {
	Node  string `json:"node"`
	Role  string `json:"role"`
	Term  uint64 `json:"term"`
	Error string `json:"error"`
}

func decode(t *testing.T, out string) map[string]roleLine {
	t.Helper()
	got := map[string]roleLine{}
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		if l == "" {
			continue
		}
		var v roleLine
		if err := json.Unmarshal([]byte(l), &v); err != nil {
			t.Fatalf("line %q: %v", l, err)
		}
		got[v.Node] = v
	}
	return got
}

func TestTheRoleProbeMapsApiAddrToNodeIds(t *testing.T) {
	// Every node agrees rq-n2 leads. The probe dials 127.0.0.1:<port>, and
	// only the api_addr host name says which harness node that is.
	urls := map[string]string{
		"rq-n1": node(t, "rq-n2").URL,
		"rq-n2": node(t, "rq-n2").URL,
		"rq-n3": node(t, "rq-n2").URL,
	}
	var out bytes.Buffer
	if err := run(&http.Client{Timeout: time.Second}, urls, time.Second, &out); err != nil {
		t.Fatalf("run: %v", err)
	}
	got := decode(t, out.String())
	want := map[string]string{"rq-n1": "follower", "rq-n2": "leader", "rq-n3": "follower"}
	for id, role := range want {
		if got[id].Role != role || got[id].Error != "" {
			t.Fatalf("%s: got %+v, want role %q\n%s", id, got[id], role, out.String())
		}
	}
	if len(got) != 3 {
		t.Fatalf("want one line per node, got %d:\n%s", len(got), out.String())
	}

	// A node that cannot be asked is an error line, never a guessed role,
	// and does not hide the others.
	urls["rq-n3"] = "http://127.0.0.1:1"
	out.Reset()
	if err := run(&http.Client{Timeout: time.Second}, urls, time.Second, &out); err != nil {
		t.Fatalf("run with one node unreachable: %v", err)
	}
	got = decode(t, out.String())
	if got["rq-n2"].Role != "leader" || got["rq-n1"].Role != "follower" {
		t.Fatalf("the reachable nodes lost their roles: %s", out.String())
	}
	if got["rq-n3"].Role != "" || got["rq-n3"].Error == "" {
		t.Fatalf("an unreachable node must be an error line: %+v", got["rq-n3"])
	}

	// A leader whose api_addr maps to no node of this world is refused.
	stray := map[string]string{
		"rq-n1": node(t, "10.9.9.9").URL,
		"rq-n2": node(t, "10.9.9.9").URL,
		"rq-n3": node(t, "10.9.9.9").URL,
	}
	out.Reset()
	err := run(&http.Client{Timeout: time.Second}, stray, time.Second, &out)
	if err == nil || !strings.Contains(err.Error(), "10.9.9.9") {
		t.Fatalf("a leader at an unknown address: err=%v, want a refusal naming it", err)
	}
	if strings.TrimSpace(out.String()) != "" {
		t.Fatalf("a refusal must print no role line:\n%s", out.String())
	}
}

func TestTheRoleProbeRefusesWhenNodesDisagree(t *testing.T) {
	cases := map[string][3]string{
		// Two claims of leadership, as a partitioned old leader and a new
		// majority produce. /nodes carries no term to break the tie.
		"two leaders": {"rq-n2", "rq-n1", "rq-n2"},
		// One node knows no leader while the others name one.
		"one sees none": {"rq-n2", "rq-n2", ""},
		// Nobody knows a leader.
		"no leader": {"", "", ""},
	}
	for name, views := range cases {
		t.Run(name, func(t *testing.T) {
			urls := map[string]string{}
			for i, v := range views {
				urls[fmt.Sprintf("rq-n%d", i+1)] = node(t, v).URL
			}
			var out bytes.Buffer
			err := run(&http.Client{Timeout: time.Second}, urls, time.Second, &out)
			if err == nil {
				t.Fatalf("views %v: the probe answered instead of refusing:\n%s", views, out.String())
			}
			if strings.TrimSpace(out.String()) != "" {
				t.Fatalf("views %v: a refusal must print no role line:\n%s", views, out.String())
			}
		})
	}
}
