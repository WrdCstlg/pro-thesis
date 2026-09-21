package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// member answers /v3/maintenance/status with the given identity, leader and
// term, exactly as etcd's gRPC gateway spells them (numbers as strings).
func member(t *testing.T, id, leader, term string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v3/maintenance/status" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"header":{"member_id":"` + id + `"},"leader":"` + leader + `","raftIndex":"9","raftTerm":"` + term + `"}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func decode(t *testing.T, out string) map[string]line {
	t.Helper()
	got := map[string]line{}
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		var v line
		if err := json.Unmarshal([]byte(l), &v); err != nil {
			t.Fatalf("line %q: %v", l, err)
		}
		got[v.Node] = v
	}
	return got
}

func TestLeaderIsTheMemberWhoseOwnIdIsTheReportedLeader(t *testing.T) {
	n1 := member(t, "11", "22", "7")
	n2 := member(t, "22", "22", "7")
	n3 := member(t, "33", "0", "6") // cut from its peers: no leader
	var out bytes.Buffer
	err := run(&http.Client{Timeout: time.Second},
		map[string]string{"etcd-n1": n1.URL, "etcd-n2": n2.URL, "etcd-n3": n3.URL, "etcd-n4": "http://127.0.0.1:1"},
		time.Second, &out)
	if err != nil {
		t.Fatal(err)
	}
	got := decode(t, out.String())
	if got["etcd-n1"].Role != "follower" || got["etcd-n1"].Term != 7 || got["etcd-n1"].Error != "" {
		t.Fatalf("n1: %+v", got["etcd-n1"])
	}
	if got["etcd-n2"].Role != "leader" || got["etcd-n2"].Term != 7 {
		t.Fatalf("n2: %+v", got["etcd-n2"])
	}
	if got["etcd-n3"].Role != "" || got["etcd-n3"].Error != "reports no leader" {
		t.Fatalf("a member reporting leader 0 must be an error line, never a guessed role: %+v", got["etcd-n3"])
	}
	if got["etcd-n4"].Role != "" || got["etcd-n4"].Error == "" {
		t.Fatalf("an unreachable member must be an error line: %+v", got["etcd-n4"])
	}
	// Lines are in node order so the harness's trace is stable.
	if !strings.HasPrefix(out.String(), `{"node":"etcd-n1"`) {
		t.Fatalf("output is not in node order: %s", out.String())
	}
}
