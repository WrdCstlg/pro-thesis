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

// node answers GET /v1/operator/raft/configuration the way Consul v2.0.4 does
// (agent/operator_endpoint.go OperatorRaftConfiguration, agent/consul/
// operator_raft_endpoint.go RaftGetConfiguration, agent/structs/operator.go
// RaftServer and RaftConfigurationResponse): {"Servers":[...],"Index":N},
// each server carrying ID, Node (its Consul node name, or "(unknown)" when
// Serf does not know the address), Address (the Raft IP:port), Leader,
// ProtocolVersion, Voter and LastIndex. Leader is the answering node's own
// view (`server.Address == raft.Leader()`) only when the request is served
// locally, which ?stale allows; without it the request is forwarded to the
// leader (agent/consul/rpc.go forwardRPC), and every node would answer with
// the LEADER's view, hiding a disagreement. That forwarded answer is
// `forwarded` here.
//
// view names which node this one believes leads ("" for none). The probe
// dials 127.0.0.1:<port>; only the Node name says which harness node a server
// is.
func node(t *testing.T, view, forwarded string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/operator/raft/configuration" || r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		leader := forwarded
		if _, ok := r.URL.Query()["stale"]; ok {
			leader = view
		}
		type server struct {
			ID              string
			Node            string
			Address         string
			Leader          bool
			ProtocolVersion string
			Voter           bool
			LastIndex       uint64
		}
		var list []server
		for i, id := range []string{"consul-n1", "consul-n2", "consul-n3"} {
			list = append(list, server{ID: fmt.Sprintf("0b5f9a1c-0000-0000-0000-00000000000%d", i+1), Node: id,
				Address: fmt.Sprintf("172.20.0.%d:8300", i+2), Leader: id == leader, ProtocolVersion: "3",
				Voter: true, LastIndex: 40})
		}
		if leader != "" && !strings.HasPrefix(leader, "consul-n") {
			// A leader whose node name is no node of this world.
			list = append(list, server{ID: "stray", Node: leader, Address: "172.20.0.9:8300", Leader: true,
				ProtocolVersion: "3", Voter: true})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"Servers": list, "Index": 12})
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

func TestTheRoleProbeMapsNodeNamesToNodeIds(t *testing.T) {
	// Every node agrees consul-n2 leads.
	urls := map[string]string{
		"consul-n1": node(t, "consul-n2", "consul-n2").URL,
		"consul-n2": node(t, "consul-n2", "consul-n2").URL,
		"consul-n3": node(t, "consul-n2", "consul-n2").URL,
	}
	var out bytes.Buffer
	if err := run(&http.Client{Timeout: time.Second}, urls, time.Second, &out); err != nil {
		t.Fatalf("run: %v", err)
	}
	got := decode(t, out.String())
	want := map[string]string{"consul-n1": "follower", "consul-n2": "leader", "consul-n3": "follower"}
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
	urls["consul-n3"] = "http://127.0.0.1:1"
	out.Reset()
	if err := run(&http.Client{Timeout: time.Second}, urls, time.Second, &out); err != nil {
		t.Fatalf("run with one node unreachable: %v", err)
	}
	got = decode(t, out.String())
	if got["consul-n2"].Role != "leader" || got["consul-n1"].Role != "follower" {
		t.Fatalf("the reachable nodes lost their roles: %s", out.String())
	}
	if got["consul-n3"].Role != "" || got["consul-n3"].Error == "" {
		t.Fatalf("an unreachable node must be an error line: %+v", got["consul-n3"])
	}

	// A leader whose node name is no node of this world, or that Serf does
	// not know, is refused.
	for _, stray := range []string{"(unknown)", "consul-x9"} {
		urls := map[string]string{
			"consul-n1": node(t, stray, stray).URL,
			"consul-n2": node(t, stray, stray).URL,
			"consul-n3": node(t, stray, stray).URL,
		}
		out.Reset()
		err := run(&http.Client{Timeout: time.Second}, urls, time.Second, &out)
		if err == nil || !strings.Contains(err.Error(), stray) {
			t.Fatalf("a leader named %q: err=%v, want a refusal naming it", stray, err)
		}
		if strings.TrimSpace(out.String()) != "" {
			t.Fatalf("a refusal must print no role line:\n%s", out.String())
		}
	}
}

func TestTheRoleProbeRefusesWhenNodesDisagree(t *testing.T) {
	cases := map[string][3]string{
		// Two claims of leadership, as a partitioned old leader and a new
		// majority produce. The configuration carries no term to break the tie.
		"two leaders": {"consul-n2", "consul-n1", "consul-n2"},
		// One node knows no leader while the others name one.
		"one sees none": {"consul-n2", "consul-n2", ""},
		// Nobody knows a leader.
		"no leader": {"", "", ""},
	}
	for name, views := range cases {
		t.Run(name, func(t *testing.T) {
			urls := map[string]string{}
			for i, v := range views {
				// Forwarded to the leader, every node would agree on
				// consul-n2: only the local (?stale) views disagree.
				urls[fmt.Sprintf("consul-n%d", i+1)] = node(t, v, "consul-n2").URL
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
