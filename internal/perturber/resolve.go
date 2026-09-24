package perturber

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/WrdCstlg/pro-thesis/internal/harness"
	"github.com/WrdCstlg/pro-thesis/internal/recorder"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// ErrNoMatch is the sentinel behind every "this target resolves to no node"
// failure.
//
// It is a distinct, testable error rather than an empty result because an empty
// result is indistinguishable from a fault that was injected and did nothing.
// The directive's Phase 2 definition of done is that a scripted schedule
// triggers the fixture's stale read; a schedule whose targets silently matched
// nothing would satisfy every oracle and report PASS over an untested system.
// The same rule already governs health probes (D-018).
var ErrNoMatch = errors.New("perturber: target matches no node")

// ErrNoRoleObserver is returned when a `role:` target is resolved without a way
// to observe live roles. Falling back to harness.nodes[].role_hint would be
// worse than failing: the hint is static and advisory, so it would either match
// nothing on a healthy cluster or pin a node that was deposed seconds ago.
var ErrNoRoleObserver = errors.New("perturber: a role: target needs a live role observer")

// ResolveError reports why a target could not be bound to concrete nodes.
type ResolveError struct {
	// Target is the canonical target text, e.g. "minority(kv)".
	Target string
	// Reason is the human explanation, including what WOULD have matched.
	Reason string
	err    error
}

func (e *ResolveError) Error() string {
	return fmt.Sprintf("perturber: target %s: %s", e.Target, e.Reason)
}

func (e *ResolveError) Unwrap() error { return e.err }

func noMatch(t schema.Target, format string, args ...any) error {
	return &ResolveError{Target: t.String(), Reason: fmt.Sprintf(format, args...), err: ErrNoMatch}
}

func resolveFailed(t schema.Target, err error, format string, args ...any) error {
	return &ResolveError{Target: t.String(), Reason: fmt.Sprintf(format, args...), err: err}
}

// ---------------------------------------------------------------------------
// Live role observation
// ---------------------------------------------------------------------------

// RoleObservation is one node's live role, as observed at injection time.
type RoleObservation struct {
	NodeID string
	// Role is the node's own claim, lowercased ("leader", "follower",
	// "candidate"). Empty when the node could not be asked.
	Role string
	// Term is the consensus term the claim was made in. It breaks a tie between
	// two nodes both claiming leadership across a partition: the standard Raft
	// rule that the higher term wins.
	Term uint64
	// Err records why this node could not be observed. A node that cannot be
	// reached is not evidence that it holds no role; it is missing evidence, and
	// the two are reported differently.
	Err error
}

// RoleObserver reports the live role of each node.
//
// It is an interface because the endpoint is a property of the system under
// test, not of PRO-THESIS. HTTPRoleObserver serves anything that exposes a JSON
// document with `role` and `term`, which the fixture's /status does, and a
// system exposing neither simply cannot use `role:` targets. That limitation is
// stated rather than hidden: see the package's open questions.
type RoleObserver interface {
	ObserveRoles(ctx context.Context, nodes []Node) ([]RoleObservation, error)
}

// DefaultStatusPath is the endpoint HTTPRoleObserver polls when none is given.
// It matches the fixture, which documents /status as the tuple Phase 2 resolves
// role:leader from.
const DefaultStatusPath = "/status"

// HTTPRoleObserver reads roles from a per-node HTTP endpoint.
//
// It probes 127.0.0.1 on each node's PUBLISHED port, never the container IP:
// container IPs are not routable from a Windows host (D-010), so the published
// port is the only way in.
type HTTPRoleObserver struct {
	// Host defaults to harness.ProbeHost.
	Host string
	// Path defaults to DefaultStatusPath.
	Path string
	// Timeout bounds a single node's observation. Zero means 2s: a role
	// observation is taken while faults are active, so a paused or partitioned
	// node MUST time out quickly rather than stall the whole schedule.
	Timeout time.Duration
	// Client is optional.
	Client *http.Client
}

// statusDoc is the subset of the system's status document this package reads.
// Unknown fields are ignored on purpose: the document belongs to the system
// under test and PRO-THESIS must not constrain its shape.
type statusDoc struct {
	Role string `json:"role"`
	Term uint64 `json:"term"`
}

// ObserveRoles polls every node once, sequentially.
//
// Sequential rather than concurrent, and that is deliberate: this runs inside
// the executor's single sequencing goroutine, where the ordering of injections
// is the property being protected. The cost is bounded by Timeout per node.
func (o *HTTPRoleObserver) ObserveRoles(ctx context.Context, nodes []Node) ([]RoleObservation, error) {
	host := o.Host
	if host == "" {
		host = harness.ProbeHost()
	}
	path := o.Path
	if path == "" {
		path = DefaultStatusPath
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	timeout := o.Timeout
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	client := o.Client
	if client == nil {
		client = &http.Client{Timeout: timeout}
	}

	out := make([]RoleObservation, 0, len(nodes))
	for _, n := range nodes {
		obs := RoleObservation{NodeID: n.ID}
		if n.HostPort <= 0 {
			obs.Err = fmt.Errorf("node %q publishes no host port", n.ID)
			out = append(out, obs)
			continue
		}
		url := fmt.Sprintf("http://%s:%d%s", host, n.HostPort, path)
		reqCtx, cancel := context.WithTimeout(ctx, timeout)
		req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
		if err != nil {
			cancel()
			obs.Err = err
			out = append(out, obs)
			continue
		}
		resp, err := client.Do(req)
		if err != nil {
			cancel()
			obs.Err = fmt.Errorf("%s: %w", url, err)
			out = append(out, obs)
			continue
		}
		var doc statusDoc
		decErr := json.NewDecoder(resp.Body).Decode(&doc)
		_ = resp.Body.Close()
		cancel()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			obs.Err = fmt.Errorf("%s: HTTP %d", url, resp.StatusCode)
			out = append(out, obs)
			continue
		}
		if decErr != nil {
			obs.Err = fmt.Errorf("%s: %w", url, decErr)
			out = append(out, obs)
			continue
		}
		if doc.Role == "" {
			obs.Err = fmt.Errorf("%s: document carries no `role` field", url)
			out = append(out, obs)
			continue
		}
		obs.Role = strings.ToLower(strings.TrimSpace(doc.Role))
		obs.Term = doc.Term
		out = append(out, obs)
	}
	return out, nil
}

var _ RoleObserver = (*HTTPRoleObserver)(nil)

// StaticRoleObserver reports a fixed role map. It exists for tests and for
// replay, where the role a target bound to is read back from a recorded world
// rather than observed.
type StaticRoleObserver map[string]string

// ObserveRoles reports the fixed roles.
func (s StaticRoleObserver) ObserveRoles(_ context.Context, nodes []Node) ([]RoleObservation, error) {
	out := make([]RoleObservation, 0, len(nodes))
	for _, n := range nodes {
		r, ok := s[n.ID]
		obs := RoleObservation{NodeID: n.ID, Role: strings.ToLower(r)}
		if !ok {
			obs.Err = fmt.Errorf("no recorded role for node %q", n.ID)
		}
		out = append(out, obs)
	}
	return out, nil
}

var _ RoleObserver = (StaticRoleObserver)(nil)

// ---------------------------------------------------------------------------
// Resolution
// ---------------------------------------------------------------------------

// Resolution is a target bound to concrete nodes.
type Resolution struct {
	// Target is the target that was resolved.
	Target schema.Target
	// Nodes is the concrete node set, ID-sorted. It is never empty: a zero-node
	// resolution is an error, not a Resolution.
	//
	// It is a SET, not a list of independent victims. net.partition over
	// {n1, n2} isolates that pair AS A GROUP (n1 and n2 still reach each other)
	// which is a different fault from partitioning n1 and n2 separately. An
	// injector is handed the whole set for exactly this reason.
	Nodes []Node
	// LiveObserved reports whether binding required live cluster state. Only
	// `role:` targets do; wildcards and quorums are functions of the topology
	// and the seed, so they replay without observation.
	LiveObserved bool
	// Roles is the observation the binding was taken from, for a role target.
	// It is recorded so a verdict can say WHY n2 was chosen.
	Roles []RoleObservation
}

// IDs returns the resolved node ids in order.
func (r Resolution) IDs() []string { return nodeIDs(r.Nodes) }

// Resolver binds targets to concrete nodes.
type Resolver struct {
	top   *Topology
	roles RoleObserver
}

// NewResolver returns a resolver over top. roles may be nil, in which case a
// `role:` target is an error rather than a guess.
func NewResolver(top *Topology, roles RoleObserver) (*Resolver, error) {
	if top == nil {
		return nil, errors.New("perturber: NewResolver needs a topology")
	}
	return &Resolver{top: top, roles: roles}, nil
}

// Topology returns the resolver's view.
func (r *Resolver) Topology() *Topology { return r.top }

// ResolveStatic binds every target form EXCEPT `role:`, which needs live state.
//
// It is what Compile uses: a schedule naming `minority(pg)` against a topology
// with no pg node is rejected before a single container is touched, rather than
// failing eight seconds into DRIVE.
func (r *Resolver) ResolveStatic(t schema.Target, rng *recorder.Stream) (Resolution, error) {
	return r.resolve(context.Background(), t, rng, false)
}

// Resolve binds a target to concrete nodes, consulting live state when the
// target requires it.
//
// rng is the fault's own PRNG sub-stream and is required whenever a quorum
// target must choose among equals. Passing nil there is an error, not a fallback
// to declaration order: "whichever node the config happens to list first" is not
// a function of the seed, so the world would not replay.
func (r *Resolver) Resolve(ctx context.Context, t schema.Target, rng *recorder.Stream) (Resolution, error) {
	return r.resolve(ctx, t, rng, true)
}

func (r *Resolver) resolve(ctx context.Context, t schema.Target, rng *recorder.Stream, live bool) (Resolution, error) {
	if err := t.Validate(); err != nil {
		return Resolution{}, resolveFailed(t, err, "%s", err.Error())
	}
	switch t.Kind {
	case schema.TargetNode:
		n, ok := r.top.Node(t.Node)
		if !ok {
			return Resolution{}, noMatch(t, "no node with id %q (have: %s)",
				t.Node, strings.Join(r.top.NodeIDs(), ", "))
		}
		return Resolution{Target: t, Nodes: []Node{n}}, nil

	case schema.TargetWildcard:
		// D-020: the wildcard selects over the LOGICAL group, not over the
		// compose service. `kv:*` matches nodes whose `service` is kv even when
		// each is backed by its own compose service kv-n1/kv-n2/kv-n3.
		nodes := r.top.Service(t.Service)
		if len(nodes) == 0 {
			return Resolution{}, noMatch(t, "no node has service %q (have: %s)",
				t.Service, servicesText(r.top))
		}
		return Resolution{Target: t, Nodes: nodes}, nil

	case schema.TargetEdge:
		a, aok := r.top.Node(t.A)
		b, bok := r.top.Node(t.B)
		switch {
		case !aok && !bok:
			return Resolution{}, noMatch(t, "neither %q nor %q is a node (have: %s)",
				t.A, t.B, strings.Join(r.top.NodeIDs(), ", "))
		case !aok:
			return Resolution{}, noMatch(t, "no node with id %q (have: %s)",
				t.A, strings.Join(r.top.NodeIDs(), ", "))
		case !bok:
			return Resolution{}, noMatch(t, "no node with id %q (have: %s)",
				t.B, strings.Join(r.top.NodeIDs(), ", "))
		}
		// Endpoints are already in canonical (lexicographic) order: ParseTarget
		// enforces it, because a partition is bidirectional and n2<->n1 must hash
		// the same as n1<->n2.
		return Resolution{Target: t, Nodes: []Node{a, b}}, nil

	case schema.TargetQuorum:
		group := r.top.Service(t.Scope)
		if len(group) == 0 {
			return Resolution{}, noMatch(t, "no node has service %q (have: %s)",
				t.Scope, servicesText(r.top))
		}
		k, err := QuorumSize(t.Func, len(group), t.N)
		if err != nil {
			return Resolution{}, resolveFailed(t, err, "%s", err.Error())
		}
		if k <= 0 {
			return Resolution{}, noMatch(t, "%s of a %d-node group is %d nodes",
				t.Func, len(group), k)
		}
		if k > len(group) {
			return Resolution{}, noMatch(t, "needs %d nodes but service %q has only %d",
				k, t.Scope, len(group))
		}
		picked, err := pickK(group, k, rng)
		if err != nil {
			return Resolution{}, resolveFailed(t, err, "%s", err.Error())
		}
		return Resolution{Target: t, Nodes: picked}, nil

	case schema.TargetRole:
		if !live {
			return Resolution{}, resolveFailed(t, errRoleNeedsLive,
				"binds to concrete nodes only at injection time, from live cluster state")
		}
		if r.roles == nil {
			return Resolution{}, resolveFailed(t, ErrNoRoleObserver,
				"no role observer is configured; harness.nodes[].role_hint is static and advisory "+
					"and must not be used to resolve a live role")
		}
		obs, err := r.roles.ObserveRoles(ctx, r.top.Nodes())
		if err != nil {
			return Resolution{}, resolveFailed(t, err, "could not observe live roles: %s", err.Error())
		}
		nodes, err := r.matchRole(t, obs)
		if err != nil {
			return Resolution{}, err
		}
		return Resolution{Target: t, Nodes: nodes, LiveObserved: true, Roles: obs}, nil
	}
	return Resolution{}, resolveFailed(t, fmt.Errorf("unknown target kind %q", t.Kind),
		"unknown target kind %q", t.Kind)
}

// errRoleNeedsLive marks a role target rejected by the static path.
var errRoleNeedsLive = errors.New("perturber: role: targets resolve only against live cluster state")

// matchRole selects the nodes currently holding the named role.
//
// When several nodes claim the role (which is exactly what a partition
// produces, and exactly the state the fixture's stale-read bug lives in) the
// HIGHEST TERM wins, by Raft's own rule that a claim from an older term is
// stale. Ties keep every claimant, ID-sorted: two nodes genuinely leading the
// same term is split brain, and hiding half of it behind an arbitrary choice
// would misattribute whatever the fault then produced.
func (r *Resolver) matchRole(t schema.Target, obs []RoleObservation) ([]Node, error) {
	want := strings.ToLower(t.Role)
	var (
		matched  []RoleObservation
		maxTerm  uint64
		observed int
	)
	for _, o := range obs {
		if o.Err == nil {
			observed++
		}
		if o.Err != nil || o.Role != want {
			continue
		}
		matched = append(matched, o)
		if o.Term > maxTerm {
			maxTerm = o.Term
		}
	}
	if observed == 0 {
		return nil, resolveFailed(t, ErrNoMatch,
			"no node could be observed at all, so the role is unknown: %s", observationText(obs))
	}
	if len(matched) == 0 {
		return nil, noMatch(t, "no node reports role %q: %s", want, observationText(obs))
	}
	out := make([]Node, 0, len(matched))
	for _, m := range matched {
		if m.Term != maxTerm {
			continue
		}
		n, ok := r.top.Node(m.NodeID)
		if !ok {
			continue
		}
		out = append(out, n)
	}
	if len(out) == 0 {
		return nil, noMatch(t, "role %q was claimed but by no node in the topology: %s",
			want, observationText(obs))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// QuorumSize returns how many of n nodes a quorum function selects.
//
//	minority(S)  floor((n-1)/2)   1 of 3, 2 of 5
//	majority(S)  floor(n/2)+1     2 of 3, 3 of 5
//	any(k, S)    k
//
// minority of a 1-node group is 0, which the caller reports as a zero-node
// resolution rather than a silent no-op: "partition the minority of a
// single-node service" is a request that cannot be honoured, and pretending it
// fired would be worse than refusing it.
func QuorumSize(fn schema.QuorumFunc, n, argN int) (int, error) {
	if n < 0 {
		return 0, fmt.Errorf("perturber: quorum over a negative group size %d", n)
	}
	switch fn {
	case schema.QuorumMinority:
		return (n - 1) / 2, nil
	case schema.QuorumMajority:
		return n/2 + 1, nil
	case schema.QuorumAny:
		if argN < 1 {
			return 0, fmt.Errorf("perturber: any(%d, ...): count must be at least 1", argN)
		}
		return argN, nil
	}
	return 0, fmt.Errorf("perturber: unknown quorum function %q (want minority, majority or any)", fn)
}

// pickK selects k of nodes deterministically from rng.
//
// The candidates arrive ID-sorted, so the permutation is a function of (world
// seed, fault string) alone, not of the order harness.nodes was written in.
// Reordering prothesis.yaml must not change which node minority(kv) picks, or
// every committed regression world would depend on the config's formatting.
//
// The SELECTED set is re-sorted by id before it is returned, so the realized
// record and the injector both see one canonical order regardless of how the
// shuffle happened to land.
func pickK(nodes []Node, k int, rng *recorder.Stream) ([]Node, error) {
	if k >= len(nodes) {
		out := make([]Node, len(nodes))
		copy(out, nodes)
		return out, nil
	}
	if rng == nil {
		return nil, errors.New("perturber: selecting among equals needs a PRNG stream; " +
			"picking by declaration order would not be a function of the seed and the world would not replay")
	}
	shuffled := make([]Node, len(nodes))
	copy(shuffled, nodes)
	rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
	out := shuffled[:k:k]
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func servicesText(t *Topology) string {
	s := t.Services()
	if len(s) == 0 {
		return "the topology declares no services"
	}
	return strings.Join(s, ", ")
}

func observationText(obs []RoleObservation) string {
	if len(obs) == 0 {
		return "no nodes were observed"
	}
	parts := make([]string, 0, len(obs))
	for _, o := range obs {
		if o.Err != nil {
			parts = append(parts, fmt.Sprintf("%s=unreachable(%v)", o.NodeID, o.Err))
			continue
		}
		parts = append(parts, fmt.Sprintf("%s=%s@term%d", o.NodeID, o.Role, o.Term))
	}
	sort.Strings(parts)
	return strings.Join(parts, " ")
}
