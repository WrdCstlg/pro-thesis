package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/WrdCstlg/pro-thesis/targets/redis/internal/redisapi"
	"github.com/WrdCstlg/pro-thesis/targets/redis/internal/respfake"
)

// Reply shapes from Redis's documentation, not from a live server: INFO
// <section> answers one bulk string of key:value lines (INFO server carries
// run_id, INFO replication carries role: master or slave); SENTINEL
// get-master-addr-by-name answers [ip, port]; SENTINEL master answers a flat
// field/value array whose ip is the CONTAINER address Sentinel reaches the
// primary at and whose runid is the primary's run_id.

type fakeNode struct {
	id, ip, runID, role string
	srv                 *respfake.Server
}

func (n *fakeNode) handle(args []string) string {
	if strings.EqualFold(args[0], "INFO") && len(args) > 1 {
		switch strings.ToLower(args[1]) {
		case "server":
			return respfake.Bulk("# Server\r\nredis_version:8.10.2\r\nrun_id:" + n.runID + "\r\n")
		case "replication":
			return respfake.Bulk("# Replication\r\nrole:" + n.role + "\r\n")
		}
	}
	return "-ERR unknown command\r\n"
}

func fakeSentinel(ip, runID string) func([]string) string {
	return func(args []string) string {
		if !strings.EqualFold(args[0], "SENTINEL") || len(args) < 3 || args[2] != "mymaster" {
			return "-ERR unknown command\r\n"
		}
		switch strings.ToLower(args[1]) {
		case "get-master-addr-by-name":
			return respfake.Fields(ip, "6379")
		case "master":
			return respfake.Fields("name", "mymaster", "ip", ip, "port", "6379", "runid", runID,
				"flags", "master", "quorum", "2", "num-slaves", "2", "num-other-sentinels", "2")
		}
		return "-ERR unknown subcommand\r\n"
	}
}

// cluster starts three nodes with the given roles and three Sentinels; views[i]
// is the index of the node Sentinel i names, or -1 for a runid no node has.
func cluster(t *testing.T, roles [3]string, views [3]int) (nodes, sentinels []redisapi.Target, fakes []*fakeNode) {
	t.Helper()
	for i := 0; i < 3; i++ {
		n := &fakeNode{id: fmt.Sprintf("redis-n%d", i+1), ip: fmt.Sprintf("172.20.0.%d", i+2),
			runID: fmt.Sprintf("%040d", i+1), role: roles[i]}
		srv, err := respfake.Start(n.handle)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(srv.Close)
		n.srv = srv
		fakes = append(fakes, n)
		nodes = append(nodes, redisapi.Target{ID: n.id, Addr: srv.Addr()})
	}
	for i, v := range views {
		ip, rid := "10.9.9.9", strings.Repeat("e", 40)
		if v >= 0 {
			ip, rid = fakes[v].ip, fakes[v].runID
		}
		srv, err := respfake.Start(fakeSentinel(ip, rid))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(srv.Close)
		sentinels = append(sentinels, redisapi.Target{ID: fakes[i].id, Addr: srv.Addr()})
	}
	return nodes, sentinels, fakes
}

type roleLine struct {
	Node  string `json:"node"`
	Role  string `json:"role"`
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

func TestTheRoleProbeMapsAddressesToNodeIds(t *testing.T) {
	// Every Sentinel names redis-n2 by 172.20.0.3, which the probe never dials
	// and no flag carries; only the runid says which node that is.
	nodes, sentinels, _ := cluster(t, [3]string{"slave", "master", "slave"}, [3]int{1, 1, 1})
	var out bytes.Buffer
	if err := run(nodes, sentinels, "mymaster", time.Second, &out); err != nil {
		t.Fatalf("run: %v", err)
	}
	got := decode(t, out.String())
	want := map[string]string{"redis-n1": "follower", "redis-n2": "leader", "redis-n3": "follower"}
	for id, role := range want {
		if got[id].Role != role || got[id].Error != "" {
			t.Fatalf("%s: got %+v, want role %q\n%s", id, got[id], role, out.String())
		}
	}
	if len(got) != 3 {
		t.Fatalf("want one line per node, got %d:\n%s", len(got), out.String())
	}

	// A node that cannot be asked is an error line, never a guessed role, and
	// does not hide the others.
	nodes[2].Addr = "127.0.0.1:1"
	out.Reset()
	if err := run(nodes, sentinels, "mymaster", time.Second, &out); err != nil {
		t.Fatalf("run with one node unreachable: %v", err)
	}
	got = decode(t, out.String())
	if got["redis-n2"].Role != "leader" || got["redis-n1"].Role != "follower" {
		t.Fatalf("the reachable nodes lost their roles: %s", out.String())
	}
	if got["redis-n3"].Role != "" || got["redis-n3"].Error == "" {
		t.Fatalf("an unreachable node must be an error line: %+v", got["redis-n3"])
	}

	// A runid no node reports is refused, never matched by address.
	nodes, sentinels, _ = cluster(t, [3]string{"slave", "master", "slave"}, [3]int{-1, -1, -1})
	out.Reset()
	err := run(nodes, sentinels, "mymaster", time.Second, &out)
	if err == nil || !strings.Contains(err.Error(), strings.Repeat("e", 40)) {
		t.Fatalf("a primary at an unknown runid: err=%v, want a refusal naming it", err)
	}
	if strings.TrimSpace(out.String()) != "" {
		t.Fatalf("a refusal must print no role line:\n%s", out.String())
	}
}

func TestTheRoleProbeRefusesTwoPrimaries(t *testing.T) {
	cases := map[string]struct {
		roles [3]string
		views [3]int
	}{
		// A partitioned old primary and a promoted replica, both master.
		"two nodes report role master": {[3]string{"master", "master", "slave"}, [3]int{0, 0, 0}},
		// The partitioned Sentinel still names the old primary.
		"the Sentinels disagree": {[3]string{"master", "slave", "slave"}, [3]int{0, 1, 0}},
		// The Sentinels name a node that says it is a replica.
		"a node and the Sentinels disagree": {[3]string{"slave", "master", "slave"}, [3]int{0, 0, 0}},
		// Nobody is primary.
		"no node reports role master": {[3]string{"slave", "slave", "slave"}, [3]int{0, 0, 0}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			nodes, sentinels, _ := cluster(t, c.roles, c.views)
			var out bytes.Buffer
			err := run(nodes, sentinels, "mymaster", time.Second, &out)
			if err == nil {
				t.Fatalf("roles %v, Sentinel views %v: the probe answered instead of refusing:\n%s", c.roles, c.views, out.String())
			}
			if strings.TrimSpace(out.String()) != "" {
				t.Fatalf("a refusal must print no role line:\n%s", out.String())
			}
		})
	}
}
