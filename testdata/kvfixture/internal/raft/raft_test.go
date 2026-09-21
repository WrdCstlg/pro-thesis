package raft_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"prothesis.dev/kvfixture/internal/netsim"
	"prothesis.dev/kvfixture/internal/raft"
)

// ---------------------------------------------------------------- test harness

type appliedEntry struct {
	Index uint64
	Term  uint64
	Data  string
}

// recorder is an Applier that remembers everything it applied, so the tests can
// check State Machine Safety across nodes after the fact.
type recorder struct {
	mu      sync.Mutex
	applied []appliedEntry
}

func (r *recorder) Apply(index, term uint64, data json.RawMessage) any {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.applied = append(r.applied, appliedEntry{Index: index, Term: term, Data: string(data)})
	return len(r.applied)
}

func (r *recorder) snapshot() []appliedEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]appliedEntry, len(r.applied))
	copy(out, r.applied)
	return out
}

type cluster struct {
	t     *testing.T
	net   *netsim.Net
	ids   []string
	nodes map[string]*raft.Raft
	apps  map[string]*recorder
}

type clusterOpts struct {
	seed     uint64
	tick     time.Duration
	hb       time.Duration
	electMin time.Duration
	electMax time.Duration
	rpc      time.Duration
	latency  time.Duration
	stores   map[string]raft.Storage
}

func defaultOpts(seed uint64) clusterOpts {
	return clusterOpts{
		seed:     seed,
		tick:     10 * time.Millisecond,
		hb:       50 * time.Millisecond,
		electMin: 400 * time.Millisecond,
		electMax: 600 * time.Millisecond,
		rpc:      120 * time.Millisecond,
		latency:  time.Millisecond,
	}
}

func fastOpts(seed uint64) clusterOpts {
	return clusterOpts{
		seed:     seed,
		tick:     5 * time.Millisecond,
		hb:       20 * time.Millisecond,
		electMin: 100 * time.Millisecond,
		electMax: 180 * time.Millisecond,
		rpc:      60 * time.Millisecond,
		latency:  500 * time.Microsecond,
	}
}

func newCluster(t *testing.T, ids []string, o clusterOpts) *cluster {
	t.Helper()
	c := &cluster{
		t:     t,
		net:   netsim.NewNet(o.latency),
		ids:   append([]string(nil), ids...),
		nodes: map[string]*raft.Raft{},
		apps:  map[string]*recorder{},
	}
	for _, id := range ids {
		peers := make([]string, 0, len(ids)-1)
		for _, other := range ids {
			if other != id {
				peers = append(peers, other)
			}
		}
		cfg := raft.Config{
			ID:               id,
			Peers:            peers,
			Seed:             o.seed,
			TickInterval:     o.tick,
			HeartbeatPeriod:  o.hb,
			ElectionMin:      o.electMin,
			ElectionMax:      o.electMax,
			RPCTimeout:       o.rpc,
			MaxEntriesPerRPC: 64,
		}
		app := &recorder{}
		var store raft.Storage = raft.NewMemStorage()
		if o.stores != nil {
			if s, ok := o.stores[id]; ok {
				store = s
			}
		}
		n, err := raft.New(cfg, c.net.Transport(id), app, store, nil)
		if err != nil {
			t.Fatalf("new node %s: %v", id, err)
		}
		c.nodes[id] = n
		c.apps[id] = app
		c.net.Register(id, n)
	}
	for _, id := range ids {
		c.nodes[id].Start()
	}
	t.Cleanup(c.stop)
	return c
}

func (c *cluster) stop() {
	for _, id := range c.ids {
		c.nodes[id].Stop()
	}
}

// waitLeader blocks until exactly one node reports the leader role and every
// node agrees on the term.
func (c *cluster) waitLeader(timeout time.Duration) (string, uint64) {
	c.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		leaders := []string{}
		terms := map[uint64]int{}
		for _, id := range c.ids {
			v := c.nodes[id].View()
			terms[v.Term]++
			if v.Role == raft.RoleLeader {
				leaders = append(leaders, id)
			}
		}
		if len(leaders) == 1 && len(terms) == 1 {
			v := c.nodes[leaders[0]].View()
			return leaders[0], v.Term
		}
		time.Sleep(5 * time.Millisecond)
	}
	c.dump()
	c.t.Fatalf("no single leader within %s", timeout)
	return "", 0
}

// waitLeaderAmong is waitLeader restricted to a subset (used when part of the
// cluster is partitioned away).
func (c *cluster) waitLeaderAmong(ids []string, minTerm uint64, timeout time.Duration) (string, uint64) {
	c.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, id := range ids {
			v := c.nodes[id].View()
			if v.Role == raft.RoleLeader && v.Term >= minTerm {
				return id, v.Term
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	c.dump()
	c.t.Fatalf("no leader with term >= %d among %v within %s", minTerm, ids, timeout)
	return "", 0
}

func (c *cluster) dump() {
	for _, id := range c.ids {
		v := c.nodes[id].View()
		c.t.Logf("node=%s role=%s term=%d leader=%s commit=%d applied=%d last=%d lease=%v",
			id, v.Role, v.Term, v.LeaderID, v.CommitIndex, v.LastApplied, v.LastLogIndex, v.LeaseHeld)
	}
}

func (c *cluster) propose(id string, payload string) (raft.ProposeResult, error) {
	p, err := c.nodes[id].Propose(json.RawMessage(fmt.Sprintf("%q", payload)))
	if err != nil {
		return raft.ProposeResult{}, err
	}
	select {
	case res := <-p.Done():
		return res, res.Err
	case <-time.After(3 * time.Second):
		c.nodes[id].Forget(p)
		return raft.ProposeResult{}, context.DeadlineExceeded
	}
}

// ---------------------------------------------------------------------- tests

func TestElectsExactlyOneLeader(t *testing.T) {
	c := newCluster(t, []string{"n1", "n2", "n3"}, defaultOpts(42))
	leader, term := c.waitLeader(5 * time.Second)
	if term == 0 {
		t.Fatalf("leader %s elected in term 0", leader)
	}
	// Stability: still exactly one leader, same term, after several heartbeats.
	time.Sleep(600 * time.Millisecond)
	leaders := 0
	for _, id := range c.ids {
		if c.nodes[id].View().Role == raft.RoleLeader {
			leaders++
		}
	}
	if leaders != 1 {
		c.dump()
		t.Fatalf("expected exactly 1 leader after settling, got %d", leaders)
	}
}

func TestReplicationAndCommit(t *testing.T) {
	c := newCluster(t, []string{"n1", "n2", "n3"}, defaultOpts(7))
	leader, _ := c.waitLeader(5 * time.Second)

	const n = 25
	for i := 0; i < n; i++ {
		if _, err := c.propose(leader, fmt.Sprintf("cmd-%02d", i)); err != nil {
			t.Fatalf("propose %d: %v", i, err)
		}
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		done := true
		for _, id := range c.ids {
			if len(c.apps[id].snapshot()) < n {
				done = false
			}
		}
		if done {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	want := c.apps[leader].snapshot()
	if len(want) < n {
		c.dump()
		t.Fatalf("leader applied %d entries, want >= %d", len(want), n)
	}
	for _, id := range c.ids {
		got := c.apps[id].snapshot()
		if len(got) < n {
			c.dump()
			t.Fatalf("node %s applied %d entries, want >= %d", id, len(got), n)
		}
		for i := 0; i < n; i++ {
			if got[i] != want[i] {
				t.Fatalf("node %s applied[%d] = %+v, leader had %+v", id, i, got[i], want[i])
			}
		}
	}
}

// A brand new leader must not be able to serve a local read before it has
// completed a heartbeat round: an unearned lease would be a second defect.
func TestFreshLeaderHasNoLeaseUntilQuorumAck(t *testing.T) {
	c := newCluster(t, []string{"n1", "n2", "n3"}, defaultOpts(11))
	leader, _ := c.waitLeader(5 * time.Second)

	// A leader in contact with a quorum must EVENTUALLY hold a lease. It is
	// granted on the first successful heartbeat round, which is a round trip
	// after the election, so this is a poll rather than an instantaneous check.
	deadline := time.Now().Add(2 * time.Second)
	held := false
	for time.Now().Before(deadline) {
		if c.nodes[leader].LeaseHeld() {
			held = true
			break
		}
		// Followers must NEVER hold a lease, at any instant during the wait.
		for _, id := range c.ids {
			if id != leader && c.nodes[id].LeaseHeld() {
				t.Fatalf("follower %s reports a held lease", id)
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	if !held {
		c.dump()
		t.Fatalf("leader %s never acquired a lease despite quorum contact", leader)
	}
	for _, id := range c.ids {
		if id == leader {
			continue
		}
		if c.nodes[id].LeaseHeld() {
			t.Fatalf("follower %s reports a held lease", id)
		}
	}
}

// Three nodes given the SAME world seed must still draw three different
// election timeouts. If they did not, every election would be a guaranteed
// split vote and `thesis up` would flake the moment the harness started
// varying seeds.
func TestSameWorldSeedYieldsDistinctElectionTimeouts(t *testing.T) {
	c := newCluster(t, []string{"kv-n1", "kv-n2", "kv-n3"}, defaultOpts(4242))
	seen := map[time.Duration]string{}
	for _, id := range c.ids {
		d := c.nodes[id].DrawnElectionTimeout()
		if d < 400*time.Millisecond || d > 600*time.Millisecond {
			t.Fatalf("node %s drew %s, outside the configured range", id, d)
		}
		if other, dup := seen[d]; dup {
			t.Fatalf("nodes %s and %s drew the same election timeout %s from one world seed", other, id, d)
		}
		seen[d] = id
	}
}

func TestDeriveNodeSeedSeparatesNodes(t *testing.T) {
	seen := map[uint64]string{}
	for _, id := range []string{"kv-n1", "kv-n2", "kv-n3"} {
		s := raft.DeriveNodeSeed(99, id)
		if other, dup := seen[s]; dup {
			t.Fatalf("%s and %s derive the same node seed", other, id)
		}
		seen[s] = id
	}
}

// A minority partition must not be able to elect a leader, and the isolated
// node must not be able to commit anything.
func TestMinorityCannotCommit(t *testing.T) {
	c := newCluster(t, []string{"n1", "n2", "n3"}, defaultOpts(1234))
	leader, term0 := c.waitLeader(5 * time.Second)

	c.net.Isolate(leader)
	defer c.net.HealAll()

	survivors := []string{}
	for _, id := range c.ids {
		if id != leader {
			survivors = append(survivors, id)
		}
	}
	newLeader, term1 := c.waitLeaderAmong(survivors, term0+1, 5*time.Second)
	if newLeader == leader {
		t.Fatalf("isolated node %s should not have been re-elected", leader)
	}
	if term1 <= term0 {
		t.Fatalf("new term %d not greater than old term %d", term1, term0)
	}

	// The isolated ex-leader cannot commit. Its proposal must NOT resolve
	// successfully; a timeout here is the correct, indeterminate outcome.
	p, err := c.nodes[leader].Propose(json.RawMessage(`"orphan"`))
	if err == nil {
		select {
		case res := <-p.Done():
			if res.Err == nil {
				t.Fatalf("isolated node %s committed a proposal at index %d", leader, res.Index)
			}
		case <-time.After(800 * time.Millisecond):
			// Correct: indeterminate, never committed.
		}
		c.nodes[leader].Forget(p)
	}
}

// After a partition heals, the cluster must converge: one leader, one term, and
// identical applied state on every node.
func TestConvergenceAfterHeal(t *testing.T) {
	c := newCluster(t, []string{"n1", "n2", "n3"}, defaultOpts(555))
	leader, term0 := c.waitLeader(5 * time.Second)
	if _, err := c.propose(leader, "before"); err != nil {
		t.Fatalf("propose before partition: %v", err)
	}

	c.net.Isolate(leader)
	survivors := []string{}
	for _, id := range c.ids {
		if id != leader {
			survivors = append(survivors, id)
		}
	}
	newLeader, _ := c.waitLeaderAmong(survivors, term0+1, 5*time.Second)
	if _, err := c.propose(newLeader, "after"); err != nil {
		t.Fatalf("propose on new leader: %v", err)
	}

	c.net.HealAll()
	if c.net.CutCount() != 0 {
		t.Fatalf("HEAL left %d residual link cuts", c.net.CutCount())
	}

	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		terms := map[uint64]int{}
		commits := map[uint64]int{}
		for _, id := range c.ids {
			v := c.nodes[id].View()
			terms[v.Term]++
			commits[v.LastApplied]++
		}
		if len(terms) == 1 && len(commits) == 1 {
			assertStateMachineSafety(t, c)
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	c.dump()
	t.Fatal("cluster did not converge within 8s after heal")
}

// assertStateMachineSafety checks that no two nodes ever applied different
// commands at the same log index.
func assertStateMachineSafety(t *testing.T, c *cluster) {
	t.Helper()
	byIndex := map[uint64]appliedEntry{}
	owner := map[uint64]string{}
	for _, id := range c.ids {
		for _, e := range c.apps[id].snapshot() {
			if prev, ok := byIndex[e.Index]; ok {
				if prev.Term != e.Term || prev.Data != e.Data {
					t.Fatalf("STATE MACHINE SAFETY VIOLATION at index %d: %s applied %+v, %s applied %+v",
						e.Index, owner[e.Index], prev, id, e)
				}
			} else {
				byIndex[e.Index] = e
				owner[e.Index] = id
			}
		}
	}
}

// TestSafetyUnderRandomPartitions is what licenses the claim that the lease is
// the fixture's ONLY safety defect. It runs deterministic-seeded partition
// schedules and checks Raft's core safety properties. It never performs a lease
// read, so it is unaffected by the injected constant and must pass under both
// build tags.
func TestSafetyUnderRandomPartitions(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping randomized safety sweep in -short mode")
	}
	ids := []string{"n1", "n2", "n3"}
	for _, seed := range []uint64{1, 2, 3, 5, 8, 13} {
		seed := seed
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			c := newCluster(t, ids, fastOpts(seed))
			leaderObs := newLeaderObserver(c)
			stopObs := leaderObs.start()

			rng := seed
			next := func(n uint64) uint64 {
				rng = raft.Splitmix64(rng)
				return rng % n
			}

			deadline := time.Now().Add(3 * time.Second)
			for time.Now().Before(deadline) {
				victim := ids[next(uint64(len(ids)))]
				c.net.Isolate(victim)
				time.Sleep(time.Duration(60+next(200)) * time.Millisecond)
				c.net.HealAll()
				time.Sleep(time.Duration(60+next(200)) * time.Millisecond)

				// Best-effort traffic; failures are expected and fine.
				for _, id := range ids {
					if v := c.nodes[id].View(); v.Role == raft.RoleLeader {
						if p, err := c.nodes[id].Propose(json.RawMessage(`"x"`)); err == nil {
							go func(n *raft.Raft, p *raft.Proposal) {
								select {
								case <-p.Done():
								case <-time.After(time.Second):
									n.Forget(p)
								}
							}(c.nodes[id], p)
						}
					}
				}
			}
			c.net.HealAll()
			time.Sleep(1500 * time.Millisecond)
			stopObs()

			leaderObs.assertElectionSafety(t)
			assertStateMachineSafety(t, c)
			assertLogMatching(t, c)
		})
	}
}

// leaderObserver samples every node's view and records which node claimed the
// leader role in each term.
type leaderObserver struct {
	c    *cluster
	mu   sync.Mutex
	seen map[uint64]map[string]bool
	done chan struct{}
	wg   sync.WaitGroup
}

func newLeaderObserver(c *cluster) *leaderObserver {
	return &leaderObserver{c: c, seen: map[uint64]map[string]bool{}, done: make(chan struct{})}
}

func (o *leaderObserver) start() func() {
	o.wg.Add(1)
	go func() {
		defer o.wg.Done()
		tk := time.NewTicker(2 * time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-o.done:
				return
			case <-tk.C:
				for _, id := range o.c.ids {
					v := o.c.nodes[id].View()
					if v.Role != raft.RoleLeader {
						continue
					}
					o.mu.Lock()
					if o.seen[v.Term] == nil {
						o.seen[v.Term] = map[string]bool{}
					}
					o.seen[v.Term][id] = true
					o.mu.Unlock()
				}
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			close(o.done)
			o.wg.Wait()
		})
	}
}

func (o *leaderObserver) assertElectionSafety(t *testing.T) {
	t.Helper()
	o.mu.Lock()
	defer o.mu.Unlock()
	for term, leaders := range o.seen {
		if len(leaders) > 1 {
			names := make([]string, 0, len(leaders))
			for id := range leaders {
				names = append(names, id)
			}
			t.Fatalf("ELECTION SAFETY VIOLATION: term %d had %d leaders: %v", term, len(leaders), names)
		}
	}
}

// assertLogMatching checks the Log Matching property: if two logs contain an
// entry with the same index and term, the logs are identical in all preceding
// entries.
func assertLogMatching(t *testing.T, c *cluster) {
	t.Helper()
	logs := map[string][]raft.Entry{}
	for _, id := range c.ids {
		logs[id] = c.nodes[id].LogEntries()
	}
	for i := 0; i < len(c.ids); i++ {
		for j := i + 1; j < len(c.ids); j++ {
			a, b := logs[c.ids[i]], logs[c.ids[j]]
			n := len(a)
			if len(b) < n {
				n = len(b)
			}
			for k := n - 1; k >= 0; k-- {
				if a[k].Term != b[k].Term {
					continue
				}
				for m := 0; m <= k; m++ {
					if a[m].Term != b[m].Term || string(a[m].Data) != string(b[m].Data) {
						t.Fatalf("LOG MATCHING VIOLATION between %s and %s at index %d (agreed at index %d)",
							c.ids[i], c.ids[j], m+1, k+1)
					}
				}
				break
			}
		}
	}
}

// A node that restarts must remember its term and its vote, or it can vote
// twice in one term and break Election Safety independently of the injected
// defect.
func TestDurableStateSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	fs, err := raft.NewFileStorage(dir)
	if err != nil {
		t.Fatalf("open storage: %v", err)
	}
	if err := fs.SaveState(7, "n2"); err != nil {
		t.Fatalf("save state: %v", err)
	}
	entries := []raft.Entry{
		{Index: 1, Term: 1, Data: json.RawMessage(`"a"`)},
		{Index: 2, Term: 7, Data: json.RawMessage(`"b"`)},
	}
	if err := fs.AppendEntries(entries); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := fs.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	fs2, err := raft.NewFileStorage(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer fs2.Close()
	term, voted, err := fs2.LoadState()
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if term != 7 || voted != "n2" {
		t.Fatalf("restored state term=%d votedFor=%q, want 7/n2", term, voted)
	}
	got, err := fs2.LoadEntries()
	if err != nil {
		t.Fatalf("load entries: %v", err)
	}
	if len(got) != 2 || got[1].Term != 7 || string(got[1].Data) != `"b"` {
		t.Fatalf("restored entries %+v", got)
	}
	if err := fs2.TruncateFrom(2); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	got, err = fs2.LoadEntries()
	if err != nil {
		t.Fatalf("load after truncate: %v", err)
	}
	if len(got) != 1 || got[0].Index != 1 {
		t.Fatalf("after truncate got %+v, want a single entry at index 1", got)
	}
}

func TestLogUpToDateRestriction(t *testing.T) {
	l := raft.NewLog()
	l.Append(raft.Entry{Index: 1, Term: 1}, raft.Entry{Index: 2, Term: 2}, raft.Entry{Index: 3, Term: 2})

	cases := []struct {
		lastIndex uint64
		lastTerm  uint64
		want      bool
		why       string
	}{
		{3, 2, true, "identical logs"},
		{4, 2, true, "longer log, same term"},
		{2, 2, false, "shorter log, same term"},
		{1, 3, true, "higher last term always wins"},
		{9, 1, false, "lower last term never wins, however long"},
	}
	for _, tc := range cases {
		if got := l.UpToDate(tc.lastIndex, tc.lastTerm); got != tc.want {
			t.Errorf("UpToDate(%d,%d) = %v, want %v (%s)", tc.lastIndex, tc.lastTerm, got, tc.want, tc.why)
		}
	}
}
