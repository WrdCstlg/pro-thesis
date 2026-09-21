package perturber

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/WrdCstlg/pro-thesis/internal/recorder"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// kvTopology is the reference shape: three nodes in ONE logical group `kv`,
// each backed by its OWN compose service. That split is the whole point of
// D-020: container IPs are not routable from a Windows host, so every node
// publishes its own port, while `kv:*` and `minority(kv)` still resolve over the
// logical group.
func kvTopology(t *testing.T) *Topology {
	t.Helper()
	top, err := NewTopologyFromNodes([]Node{
		{ID: "kv-n2", Service: "kv", ComposeService: "kv-n2", ContainerID: "c2", HostPort: 8082},
		{ID: "kv-n1", Service: "kv", ComposeService: "kv-n1", ContainerID: "c1", HostPort: 8081},
		{ID: "kv-n3", Service: "kv", ComposeService: "kv-n3", ContainerID: "c3", HostPort: 8083},
		{ID: "pg", Service: "postgres", ComposeService: "pg", ContainerID: "cp", HostPort: 5432},
	})
	if err != nil {
		t.Fatalf("NewTopologyFromNodes: %v", err)
	}
	return top
}

func bigTopology(t *testing.T, n int) *Topology {
	t.Helper()
	nodes := make([]Node, 0, n)
	for i := 0; i < n; i++ {
		id := string(rune('a' + i))
		nodes = append(nodes, Node{ID: "kv-" + id, Service: "kv", ComposeService: "kv-" + id})
	}
	top, err := NewTopologyFromNodes(nodes)
	if err != nil {
		t.Fatalf("NewTopologyFromNodes: %v", err)
	}
	return top
}

func mustTarget(t *testing.T, s string) schema.Target {
	t.Helper()
	tg, err := schema.ParseTarget(s)
	if err != nil {
		t.Fatalf("ParseTarget(%q): %v", s, err)
	}
	return tg
}

func stream(seed uint64, path ...string) *recorder.Stream {
	if len(path) == 0 {
		path = []string{recorder.StreamFaultSchedule}
	}
	return recorder.NewStream(recorder.MustDeriveKey(recorder.Seed(seed), path...))
}

// fakeRoleObserver reports a canned observation, terms included.
type fakeRoleObserver struct {
	obs []RoleObservation
	err error
}

func (f fakeRoleObserver) ObserveRoles(context.Context, []Node) ([]RoleObservation, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.obs, nil
}

func TestResolveNodeTarget(t *testing.T) {
	r, err := NewResolver(kvTopology(t), nil)
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	res, err := r.Resolve(context.Background(), mustTarget(t, "kv-n2"), nil)
	if err != nil {
		t.Fatalf("Resolve(kv-n2): %v", err)
	}
	if got := res.IDs(); !reflect.DeepEqual(got, []string{"kv-n2"}) {
		t.Errorf("Resolve(kv-n2) = %v, want [kv-n2]", got)
	}
	if res.LiveObserved {
		t.Errorf("a node target must not need live observation")
	}
}

func TestResolveWildcardUsesLogicalServiceNotComposeService(t *testing.T) {
	r, _ := NewResolver(kvTopology(t), nil)

	res, err := r.Resolve(context.Background(), mustTarget(t, "kv:*"), nil)
	if err != nil {
		t.Fatalf("Resolve(kv:*): %v", err)
	}
	want := []string{"kv-n1", "kv-n2", "kv-n3"}
	if got := res.IDs(); !reflect.DeepEqual(got, want) {
		t.Errorf("Resolve(kv:*) = %v, want %v", got, want)
	}

	// The PHYSICAL compose service name must NOT be a group. Matching it would
	// silently redefine minority(kv) as a prefix glob, which OQ-016 rejected.
	if _, err := r.Resolve(context.Background(), mustTarget(t, "kv-n1:*"), nil); !errors.Is(err, ErrNoMatch) {
		t.Errorf("Resolve(kv-n1:*) error = %v, want ErrNoMatch", err)
	}
}

func TestResolveEdgeTarget(t *testing.T) {
	r, _ := NewResolver(kvTopology(t), nil)

	// n2<->n1 parses into canonical (lexicographic) order, because a partition
	// is bidirectional and the two spellings are one fault.
	res, err := r.Resolve(context.Background(), mustTarget(t, "kv-n2<->kv-n1"), nil)
	if err != nil {
		t.Fatalf("Resolve(edge): %v", err)
	}
	if got := res.IDs(); !reflect.DeepEqual(got, []string{"kv-n1", "kv-n2"}) {
		t.Errorf("edge resolved to %v, want [kv-n1 kv-n2]", got)
	}

	_, err = r.Resolve(context.Background(), mustTarget(t, "kv-n1<->nope"), nil)
	if !errors.Is(err, ErrNoMatch) {
		t.Errorf("edge to a missing node: error = %v, want ErrNoMatch", err)
	}
}

func TestQuorumSizes(t *testing.T) {
	cases := []struct {
		fn   schema.QuorumFunc
		n    int
		argN int
		want int
	}{
		{schema.QuorumMinority, 3, 0, 1},
		{schema.QuorumMajority, 3, 0, 2},
		{schema.QuorumMinority, 5, 0, 2},
		{schema.QuorumMajority, 5, 0, 3},
		{schema.QuorumMinority, 1, 0, 0},
		{schema.QuorumMajority, 1, 0, 1},
		{schema.QuorumAny, 3, 2, 2},
	}
	for _, c := range cases {
		got, err := QuorumSize(c.fn, c.n, c.argN)
		if err != nil {
			t.Fatalf("QuorumSize(%s, %d, %d): %v", c.fn, c.n, c.argN, err)
		}
		if got != c.want {
			t.Errorf("QuorumSize(%s, n=%d) = %d, want %d", c.fn, c.n, got, c.want)
		}
	}
}

func TestResolveQuorumCardinality(t *testing.T) {
	r, _ := NewResolver(kvTopology(t), nil)
	cases := []struct {
		target string
		want   int
	}{
		{"minority(kv)", 1},
		{"majority(kv)", 2},
		{"any(2, kv)", 2},
		{"any(3, kv)", 3},
	}
	for _, c := range cases {
		res, err := r.Resolve(context.Background(), mustTarget(t, c.target), stream(7))
		if err != nil {
			t.Fatalf("Resolve(%s): %v", c.target, err)
		}
		if len(res.Nodes) != c.want {
			t.Errorf("Resolve(%s) selected %d nodes (%v), want %d",
				c.target, len(res.Nodes), res.IDs(), c.want)
		}
		for _, n := range res.Nodes {
			if n.Service != "kv" {
				t.Errorf("Resolve(%s) selected %s, which is not in the kv group", c.target, n.ID)
			}
		}
	}
}

// A quorum target that resolves to zero nodes is an ERROR, never a no-op.
// minority() of a one-node group is zero nodes, and a fault that fired against
// nothing while reporting as injected is how a run reports PASS over an
// untested system.
func TestResolveZeroMatchIsAlwaysAnError(t *testing.T) {
	single, err := NewTopologyFromNodes([]Node{{ID: "solo", Service: "kv", ComposeService: "solo"}})
	if err != nil {
		t.Fatalf("NewTopologyFromNodes: %v", err)
	}
	r, _ := NewResolver(single, nil)

	for _, target := range []string{"minority(kv)", "nope", "nope:*", "minority(postgres)", "any(4, kv)"} {
		res, err := r.Resolve(context.Background(), mustTarget(t, target), stream(1))
		if err == nil {
			t.Fatalf("Resolve(%s) returned %d nodes and no error; a zero-match target must error",
				target, len(res.Nodes))
		}
		if !errors.Is(err, ErrNoMatch) {
			t.Errorf("Resolve(%s) error = %v, want ErrNoMatch", target, err)
		}
		var re *ResolveError
		if !errors.As(err, &re) {
			t.Errorf("Resolve(%s) error is %T, want *ResolveError", target, err)
		}
	}
}

// Selection among equals must be a function of the SEED, not of map iteration
// order and not of the order harness.nodes happens to be written in.
func TestQuorumSelectionIsDeterministicAndSeedDriven(t *testing.T) {
	top := bigTopology(t, 5)
	r, _ := NewResolver(top, nil)
	target := mustTarget(t, "minority(kv)") // 2 of 5

	// (a) Same seed, same answer: every time.
	first, err := r.Resolve(context.Background(), target, stream(4242))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	for i := 0; i < 8; i++ {
		again, err := r.Resolve(context.Background(), target, stream(4242))
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if !reflect.DeepEqual(first.IDs(), again.IDs()) {
			t.Fatalf("seed 4242 selected %v then %v; selection must be deterministic",
				first.IDs(), again.IDs())
		}
	}

	// (b) The seed actually drives it. If selection were "the first k in some
	// fixed order", every seed would agree, so at least two seeds must differ.
	seen := map[string]bool{}
	for seed := uint64(1); seed <= 64; seed++ {
		res, err := r.Resolve(context.Background(), target, stream(seed))
		if err != nil {
			t.Fatalf("Resolve(seed=%d): %v", seed, err)
		}
		if len(res.Nodes) != 2 {
			t.Fatalf("seed %d selected %d nodes, want 2", seed, len(res.Nodes))
		}
		seen[strings.Join(res.IDs(), ",")] = true
	}
	if len(seen) < 2 {
		t.Fatalf("64 seeds produced only %d distinct selections (%v); selection is not seed-driven",
			len(seen), seen)
	}

	// (c) Reordering the config must not change the answer. Otherwise every
	// committed regression world would depend on prothesis.yaml's formatting.
	reversed := top.Nodes()
	for i, j := 0, len(reversed)-1; i < j; i, j = i+1, j-1 {
		reversed[i], reversed[j] = reversed[j], reversed[i]
	}
	shuffledTop, err := NewTopologyFromNodes(reversed)
	if err != nil {
		t.Fatalf("NewTopologyFromNodes: %v", err)
	}
	r2, _ := NewResolver(shuffledTop, nil)
	res2, err := r2.Resolve(context.Background(), target, stream(4242))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !reflect.DeepEqual(first.IDs(), res2.IDs()) {
		t.Errorf("declaration order changed the selection: %v vs %v", first.IDs(), res2.IDs())
	}
}

func TestQuorumSelectionNeedsAStream(t *testing.T) {
	r, _ := NewResolver(bigTopology(t, 5), nil)
	if _, err := r.Resolve(context.Background(), mustTarget(t, "minority(kv)"), nil); err == nil {
		t.Fatal("selecting 2 of 5 without a PRNG stream must be an error, not a fallback to " +
			"declaration order")
	}
	// Selecting the WHOLE group needs no choice, so no stream is needed.
	if _, err := r.Resolve(context.Background(), mustTarget(t, "any(5, kv)"), nil); err != nil {
		t.Errorf("any(5, kv) over 5 nodes needs no selection: %v", err)
	}
}

func TestResolveRoleUsesLiveState(t *testing.T) {
	top := kvTopology(t)
	obs := fakeRoleObserver{obs: []RoleObservation{
		{NodeID: "kv-n1", Role: "follower", Term: 4},
		{NodeID: "kv-n2", Role: "leader", Term: 4},
		{NodeID: "kv-n3", Role: "follower", Term: 4},
	}}
	r, _ := NewResolver(top, obs)

	res, err := r.Resolve(context.Background(), mustTarget(t, "role:leader"), nil)
	if err != nil {
		t.Fatalf("Resolve(role:leader): %v", err)
	}
	if got := res.IDs(); !reflect.DeepEqual(got, []string{"kv-n2"}) {
		t.Errorf("role:leader = %v, want [kv-n2]", got)
	}
	if !res.LiveObserved {
		t.Error("a role target binds from live state and must say so")
	}

	fol, err := r.Resolve(context.Background(), mustTarget(t, "role:follower"), nil)
	if err != nil {
		t.Fatalf("Resolve(role:follower): %v", err)
	}
	if got := fol.IDs(); !reflect.DeepEqual(got, []string{"kv-n1", "kv-n3"}) {
		t.Errorf("role:follower = %v, want [kv-n1 kv-n3]", got)
	}
}

// Two nodes claiming leadership is what a partition produces. The higher term
// is the current holder, by Raft's own rule; a claim from an older term is
// stale.
func TestResolveRolePrefersTheHighestTerm(t *testing.T) {
	obs := fakeRoleObserver{obs: []RoleObservation{
		{NodeID: "kv-n1", Role: "leader", Term: 2},
		{NodeID: "kv-n2", Role: "leader", Term: 7},
		{NodeID: "kv-n3", Role: "follower", Term: 7},
	}}
	r, _ := NewResolver(kvTopology(t), obs)
	res, err := r.Resolve(context.Background(), mustTarget(t, "role:leader"), nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got := res.IDs(); !reflect.DeepEqual(got, []string{"kv-n2"}) {
		t.Errorf("split-brain leader = %v, want [kv-n2] (term 7 beats term 2)", got)
	}
}

func TestResolveRoleNoClaimantIsAnError(t *testing.T) {
	obs := fakeRoleObserver{obs: []RoleObservation{
		{NodeID: "kv-n1", Role: "follower", Term: 9},
		{NodeID: "kv-n2", Role: "candidate", Term: 10},
		{NodeID: "kv-n3", Role: "follower", Term: 9},
	}}
	r, _ := NewResolver(kvTopology(t), obs)
	_, err := r.Resolve(context.Background(), mustTarget(t, "role:leader"), nil)
	if !errors.Is(err, ErrNoMatch) {
		t.Fatalf("no leader: error = %v, want ErrNoMatch", err)
	}
	// The message must say what WAS observed, or the failure is undebuggable.
	if !strings.Contains(err.Error(), "kv-n2=candidate") {
		t.Errorf("error does not report the observation: %v", err)
	}
}

func TestResolveRoleNeedsAnObserver(t *testing.T) {
	r, _ := NewResolver(kvTopology(t), nil)
	_, err := r.Resolve(context.Background(), mustTarget(t, "role:leader"), nil)
	if !errors.Is(err, ErrNoRoleObserver) {
		t.Fatalf("error = %v, want ErrNoRoleObserver", err)
	}

	// role_hint is static and advisory. It must never stand in for a live
	// observation: it would pin a node that was deposed ten seconds ago.
	hinted, err := NewTopologyFromNodes([]Node{
		{ID: "kv-n1", Service: "kv", RoleHint: schema.RoleLeader},
		{ID: "kv-n2", Service: "kv", RoleHint: schema.RoleFollower},
	})
	if err != nil {
		t.Fatalf("NewTopologyFromNodes: %v", err)
	}
	r2, _ := NewResolver(hinted, nil)
	if _, err := r2.Resolve(context.Background(), mustTarget(t, "role:leader"), nil); err == nil {
		t.Fatal("role:leader resolved from role_hint; the hint is advisory and must not bind a fault")
	}
}

func TestResolveStaticRejectsRoleTargets(t *testing.T) {
	obs := fakeRoleObserver{obs: []RoleObservation{{NodeID: "kv-n1", Role: "leader", Term: 1}}}
	r, _ := NewResolver(kvTopology(t), obs)
	if _, err := r.ResolveStatic(mustTarget(t, "role:leader"), nil); err == nil {
		t.Fatal("ResolveStatic must refuse a role target: it binds only at injection time")
	}
}

func TestResolveRoleAllNodesUnreachable(t *testing.T) {
	obs := fakeRoleObserver{obs: []RoleObservation{
		{NodeID: "kv-n1", Err: errors.New("connection refused")},
		{NodeID: "kv-n2", Err: errors.New("connection refused")},
		{NodeID: "kv-n3", Err: errors.New("connection refused")},
	}}
	r, _ := NewResolver(kvTopology(t), obs)
	_, err := r.Resolve(context.Background(), mustTarget(t, "role:leader"), nil)
	if err == nil {
		t.Fatal("no node could be observed; the role is unknown and that must be an error")
	}
	if !strings.Contains(err.Error(), "unreachable") {
		t.Errorf("error should distinguish `unobservable` from `no claimant`: %v", err)
	}
}

func TestNewTopologyJoinsConfigAndBindings(t *testing.T) {
	cfg := testConfig(t, nil, nil)
	bind := &recorder.Topology{Nodes: []recorder.NodeBinding{
		{ID: "kv-n1", ContainerID: "abc", HostPort: 32768, ContainerPort: 8080, Service: "kv-n1"},
	}}
	top, err := NewTopology(cfg, bind)
	if err != nil {
		t.Fatalf("NewTopology: %v", err)
	}
	n1, ok := top.Node("kv-n1")
	if !ok {
		t.Fatal("kv-n1 missing from the joined topology")
	}
	if n1.Service != "kv" {
		t.Errorf("Service = %q, want the LOGICAL group kv", n1.Service)
	}
	if n1.ComposeService != "kv-n1" {
		t.Errorf("ComposeService = %q, want kv-n1", n1.ComposeService)
	}
	if n1.ContainerID != "abc" || n1.HostPort != 32768 {
		t.Errorf("binding not joined: %+v", n1)
	}
	// A node with no binding still exists: that is what lets Compile check a
	// schedule before anything is running.
	if _, ok := top.Node("kv-n3"); !ok {
		t.Error("kv-n3 missing from the joined topology")
	}
}
