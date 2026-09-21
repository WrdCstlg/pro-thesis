package kv_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"prothesis.dev/kvfixture/internal/kv"
	"prothesis.dev/kvfixture/internal/netsim"
	"prothesis.dev/kvfixture/internal/raft"
)

const (
	testElectionMin = 400 * time.Millisecond
	testElectionMax = 600 * time.Millisecond
)

type node struct {
	id   string
	raft *raft.Raft
	sm   *kv.StateMachine
	srv  *kv.Server
	h    http.Handler
}

type kvCluster struct {
	t     *testing.T
	net   *netsim.Net
	ids   []string
	nodes map[string]*node
}

func newKVCluster(t *testing.T, seed uint64) *kvCluster {
	t.Helper()
	ids := []string{"kv-n1", "kv-n2", "kv-n3"}
	c := &kvCluster{t: t, net: netsim.NewNet(time.Millisecond), ids: ids, nodes: map[string]*node{}}
	for _, id := range ids {
		peers := []string{}
		for _, o := range ids {
			if o != id {
				peers = append(peers, o)
			}
		}
		cfg := raft.Config{
			ID:               id,
			Peers:            peers,
			Seed:             seed,
			TickInterval:     10 * time.Millisecond,
			HeartbeatPeriod:  50 * time.Millisecond,
			ElectionMin:      testElectionMin,
			ElectionMax:      testElectionMax,
			RPCTimeout:       120 * time.Millisecond,
			MaxEntriesPerRPC: 64,
		}
		sm := kv.NewStateMachine()
		r, err := raft.New(cfg, c.net.Transport(id), sm, raft.NewMemStorage(), nil)
		if err != nil {
			t.Fatalf("new node %s: %v", id, err)
		}
		srv := kv.NewServer(kv.Options{NodeID: id, ApplyWait: 1500 * time.Millisecond}, r, sm)
		c.nodes[id] = &node{id: id, raft: r, sm: sm, srv: srv, h: srv.Handler()}
		c.net.Register(id, r)
	}
	for _, id := range ids {
		c.nodes[id].raft.Start()
	}
	t.Cleanup(func() {
		for _, id := range ids {
			c.nodes[id].raft.Stop()
		}
	})
	return c
}

func (c *kvCluster) do(id, method, path string, body any) (int, []byte) {
	c.t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			c.t.Fatalf("marshal body: %v", err)
		}
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rdr)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	c.nodes[id].h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

func (c *kvCluster) waitLeader(timeout time.Duration) (string, uint64) {
	c.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		leaders := []string{}
		terms := map[uint64]bool{}
		for _, id := range c.ids {
			v := c.nodes[id].raft.View()
			terms[v.Term] = true
			if v.Role == raft.RoleLeader {
				leaders = append(leaders, id)
			}
		}
		if len(leaders) == 1 && len(terms) == 1 {
			return leaders[0], c.nodes[leaders[0]].raft.View().Term
		}
		time.Sleep(5 * time.Millisecond)
	}
	c.t.Fatalf("no leader within %s", timeout)
	return "", 0
}

func (c *kvCluster) waitLeaderAmong(ids []string, minTerm uint64, timeout time.Duration) (string, uint64) {
	c.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, id := range ids {
			v := c.nodes[id].raft.View()
			if v.Role == raft.RoleLeader && v.Term >= minTerm {
				return id, v.Term
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	c.t.Fatalf("no leader with term >= %d among %v within %s", minTerm, ids, timeout)
	return "", 0
}

// TestLeaseBugMechanism is the load-bearing test for the injected defect.
//
// It does NOT assert "the fixture is buggy". It asserts the LEASE SAFETY
// CONDITION and its consequence, so it is meaningful under both build tags:
//
//	LeaseDuration + drift < ElectionTimeoutMin  ==>  no stale lease read
//	LeaseDuration        >= ElectionTimeoutMin  ==>  a stale lease read is served
//
// Flipping the constant in lease.go flips which branch runs, and both branches
// can fail. If someone "fixes" the fixture by changing the read path instead of
// the constant, the buggy branch fails and says so.
func TestLeaseBugMechanism(t *testing.T) {
	c := newKVCluster(t, 20260906)
	leader, term0 := c.waitLeader(6 * time.Second)

	// 1. Commit a value everyone agrees on.
	code, body := c.do(leader, http.MethodPut, "/kv/k/42", map[string]any{"value": 7, "op_id": 1001})
	if code != http.StatusOK {
		t.Fatalf("initial write: status %d body %s", code, body)
	}
	var w kv.WriteResponse
	if err := json.Unmarshal(body, &w); err != nil {
		t.Fatalf("decode write response: %v", err)
	}
	c.waitAllApplied(w.Index, 3*time.Second)

	if !c.nodes[leader].raft.LeaseHeld() {
		t.Fatalf("leader %s does not hold its lease before the fault", leader)
	}

	// 2. Isolate the leader on the peer plane. Its client plane stays up, which
	//    is what makes this a gray failure rather than a dead node.
	partitionedAt := time.Now()
	c.net.Isolate(leader)
	defer c.net.HealAll()

	survivors := []string{}
	for _, id := range c.ids {
		if id != leader {
			survivors = append(survivors, id)
		}
	}

	// 3. The majority elects a new leader and commits a new value.
	newLeader, term1 := c.waitLeaderAmong(survivors, term0+1, 6*time.Second)
	var w2 kv.WriteResponse
	deadline := time.Now().Add(4 * time.Second)
	for {
		code, body = c.do(newLeader, http.MethodPut, "/kv/k/42", map[string]any{"value": 9, "op_id": 1002})
		if code == http.StatusOK {
			if err := json.Unmarshal(body, &w2); err != nil {
				t.Fatalf("decode second write: %v", err)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("new leader %s never accepted a write: status %d body %s", newLeader, code, body)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Logf("new leader %s committed k/42=9 at index %d term %d, %s after the partition",
		newLeader, w2.Index, term1, time.Since(partitionedAt).Round(time.Millisecond))

	// 4. Read the OLD leader with the default (lease) consistency.
	code, body = c.do(leader, http.MethodGet, "/kv/k/42", nil)
	elapsed := time.Since(partitionedAt)

	safe := raft.LeaseDuration < testElectionMin
	if safe {
		if code == http.StatusOK {
			var rr kv.ReadResponse
			_ = json.Unmarshal(body, &rr)
			if rr.Value != nil && *rr.Value == 7 {
				t.Fatalf("PATCHED BUILD SERVED A STALE READ: lease=%s election_min=%s elapsed=%s body=%s",
					raft.LeaseDuration, testElectionMin, elapsed, body)
			}
		}
		if code != http.StatusServiceUnavailable {
			t.Fatalf("patched build: expected 503 from a partitioned leader, got %d body %s", code, body)
		}
		var e struct {
			Code    string `json:"code"`
			Applied *bool  `json:"applied"`
		}
		if err := json.Unmarshal(body, &e); err != nil {
			t.Fatalf("decode error body: %v", err)
		}
		if e.Applied == nil || *e.Applied {
			t.Fatalf("a declined read must carry applied:false, got %s", body)
		}
		t.Logf("patched build (lease=%s < election_min=%s): read declined with %s, no stale value",
			raft.LeaseDuration, testElectionMin, e.Code)
		return
	}

	// Buggy build: the lease outlives the election, so the isolated ex-leader
	// happily answers from a state machine that is a whole term behind.
	if code != http.StatusOK {
		t.Fatalf("expected the buggy build to serve a lease read, got %d body %s (elapsed %s, lease %s)",
			code, body, elapsed, raft.LeaseDuration)
	}
	var rr kv.ReadResponse
	if err := json.Unmarshal(body, &rr); err != nil {
		t.Fatalf("decode read response: %v", err)
	}
	if rr.ReadMode != "lease" {
		t.Fatalf("expected read_mode=lease, got %q", rr.ReadMode)
	}
	if rr.Value == nil {
		t.Fatalf("stale read returned no value: %s", body)
	}
	if *rr.Value != 7 {
		t.Fatalf("expected the STALE value 7 from the displaced leader, got %d (body %s)", *rr.Value, body)
	}
	if rr.Term >= term1 {
		t.Fatalf("displaced leader reported term %d, expected something older than %d", rr.Term, term1)
	}
	t.Logf("STALE READ CONFIRMED: node=%s term=%d served value=%d while term %d had committed 9 (%s after partition)",
		rr.ServedBy, rr.Term, *rr.Value, term1, elapsed.Round(time.Millisecond))

	// 5. The lease deadline is honest monotonic time: it MUST expire on its own
	//    while the node is still partitioned. A tick-counter lease that only
	//    advances when the process is scheduled would not, and this assertion is
	//    what stops anyone reintroducing one.
	if testing.Short() {
		t.Skip("skipping lease-expiry wait in -short mode")
	}
	expiry := partitionedAt.Add(raft.LeaseDuration + 750*time.Millisecond)
	for time.Now().Before(expiry) {
		if !c.nodes[leader].raft.LeaseHeld() {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if c.nodes[leader].raft.LeaseHeld() {
		t.Fatalf("lease still held %s after the partition; a %s lease must expire on the monotonic clock",
			time.Since(partitionedAt), raft.LeaseDuration)
	}
	held := time.Since(partitionedAt)
	t.Logf("lease expired %s after the partition (LeaseDuration=%s)", held.Round(time.Millisecond), raft.LeaseDuration)

	code, body = c.do(leader, http.MethodGet, "/kv/k/42", nil)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("after the lease expired the partitioned node must decline reads, got %d body %s", code, body)
	}
}

// A client that explicitly asks for a linearizable read must never get a stale
// answer, even in the buggy build. The read-index path is correct; the lease
// path is the defect.
func TestLinearizableReadNeverStale(t *testing.T) {
	c := newKVCluster(t, 777)
	leader, term0 := c.waitLeader(6 * time.Second)

	code, body := c.do(leader, http.MethodPut, "/kv/k/1", map[string]any{"value": 5, "op_id": 1})
	if code != http.StatusOK {
		t.Fatalf("write: %d %s", code, body)
	}
	var w kv.WriteResponse
	_ = json.Unmarshal(body, &w)
	c.waitAllApplied(w.Index, 3*time.Second)

	c.net.Isolate(leader)
	defer c.net.HealAll()
	survivors := []string{}
	for _, id := range c.ids {
		if id != leader {
			survivors = append(survivors, id)
		}
	}
	newLeader, _ := c.waitLeaderAmong(survivors, term0+1, 6*time.Second)
	deadline := time.Now().Add(4 * time.Second)
	for {
		code, body = c.do(newLeader, http.MethodPut, "/kv/k/1", map[string]any{"value": 6, "op_id": 2})
		if code == http.StatusOK || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if code != http.StatusOK {
		t.Fatalf("new leader write: %d %s", code, body)
	}

	code, body = c.do(leader, http.MethodGet, "/kv/k/1?consistency=linearizable", nil)
	if code == http.StatusOK {
		var rr kv.ReadResponse
		_ = json.Unmarshal(body, &rr)
		if rr.Value != nil && *rr.Value == 5 {
			t.Fatalf("LINEARIZABLE READ RETURNED A STALE VALUE: %s", body)
		}
	}
	if code != http.StatusServiceUnavailable {
		t.Fatalf("a partitioned leader must decline a linearizable read, got %d body %s", code, body)
	}
	var e struct {
		Code    string `json:"code"`
		Applied *bool  `json:"applied"`
	}
	_ = json.Unmarshal(body, &e)
	if e.Applied == nil || *e.Applied {
		t.Fatalf("declined linearizable read must carry applied:false, got %s", body)
	}
	if e.Code != "NO_QUORUM" && e.Code != "NOT_LEADER" {
		t.Fatalf("unexpected decline code %q (body %s)", e.Code, body)
	}
}

func (c *kvCluster) waitAllApplied(index uint64, timeout time.Duration) {
	c.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		ok := true
		for _, id := range c.ids {
			if c.nodes[id].raft.View().LastApplied < index {
				ok = false
			}
		}
		if ok {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	var sb string
	for _, id := range c.ids {
		v := c.nodes[id].raft.View()
		sb += fmt.Sprintf("%s(applied=%d) ", id, v.LastApplied)
	}
	c.t.Fatalf("not all nodes applied index %d within %s: %s", index, timeout, sb)
}

// The CAS contract: a compare-and-swap that RAN and did not match is a
// successful operation with a negative result. It must carry applied:true so
// the driver records `ok`, never `fail`.
func TestCASMismatchIsAppliedNotFailed(t *testing.T) {
	c := newKVCluster(t, 31337)
	leader, _ := c.waitLeader(6 * time.Second)

	code, body := c.do(leader, http.MethodPut, "/kv/k/9", map[string]any{"value": 1, "op_id": 10})
	if code != http.StatusOK {
		t.Fatalf("seed write: %d %s", code, body)
	}
	code, body = c.do(leader, http.MethodPost, "/kv/k/9/cas", map[string]any{"expect": 999, "value": 2, "op_id": 11})
	if code != http.StatusOK {
		t.Fatalf("a CAS that ran must return 200, got %d body %s", code, body)
	}
	var cr kv.CASResponse
	if err := json.Unmarshal(body, &cr); err != nil {
		t.Fatalf("decode cas response: %v", err)
	}
	if cr.OK {
		t.Fatalf("CAS against the wrong expected value must not swap: %s", body)
	}
	if !cr.Applied {
		t.Fatalf("a CAS that ran must report applied:true: %s", body)
	}
	if cr.Have == nil || *cr.Have != 1 {
		t.Fatalf("CAS should report the value it actually found: %s", body)
	}
}
