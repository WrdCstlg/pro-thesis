package search

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strconv"
	"strings"

	"github.com/WrdCstlg/pro-thesis/internal/telemetry"
)

// ---------------------------------------------------------------------------
// State-abstraction coverage
//
// A coarse per-node tuple (role, term/epoch, bucketed queue depth, connection
// count) sampled periodically; the coverage counter is the number of distinct
// GLOBAL tuples observed.
//
// Everything here is designed against one failure: an unbucketed value makes
// every sample unique, at which point the counter measures sample count. Three
// decisions follow from that, and each is a choice worth defending:
//
//  1. TERM IS RELATIVE TO THE ROUND, NOT ABSOLUTE. A Raft term is unbounded and
//     world-specific: the fixture reaches term 65 in one run and term 3 in
//     another, so an absolute term makes every world's states disjoint from
//     every other world's and the global counter becomes a world counter. What
//     actually carries signal is DIVERGENCE: one node two terms behind the
//     rest is the fixture's stale-read condition. So a node's term component is
//     (max term observed in this round) - (this node's term), bucketed.
//
//  2. QUEUE DEPTH AND CONNECTION COUNTS ARE LOG-BUCKETED. A queue at 41 and a
//     queue at 42 are the same state; a queue at 4 and a queue at 4000 are not.
//     Powers of two, labelled by the bucket's lower bound.
//
//  3. AN UNOBSERVED COMPONENT IS "?", NEVER ZERO. internal/telemetry's whole
//     design is that an absent metric stays absent (a nil pointer), because a
//     zero substituted for an absent metric turns a resource oracle into a
//     rubber stamp. The same rule applies here for the same reason, and it
//     matters more: a search that could not read /status on a paused node must
//     not record "queue depth 0, all quiet". It records "unknown", which is a
//     distinct state and an honest one.
// ---------------------------------------------------------------------------

// StateUnknown is the component value for a metric that was not observed.
const StateUnknown = "?"

// Buckets is the bucketing policy for the numeric components.
//
// Bounds are the INCLUSIVE upper edges of all but the last bucket; a value
// above the final bound falls in an overflow bucket labelled "<bound>+".
type Buckets struct {
	// Queue buckets queue depth.
	Queue []int64
	// Conns buckets a connection count.
	Conns []int64
	// TermLag buckets how many terms behind the round's leader-most node a node
	// is.
	TermLag []int64
}

// DefaultBuckets is the compiled-in bucketing.
//
// Powers of two up to a small ceiling. The ceiling is low on purpose: the point
// of the abstraction is to be COARSE. Distinguishing a 512-deep queue from a
// 1024-deep one buys nothing a search can act on, and each extra bucket
// multiplies the size of the global state space by the number of nodes.
func DefaultBuckets() Buckets {
	return Buckets{
		Queue:   []int64{0, 1, 3, 7, 15, 31, 63, 127},
		Conns:   []int64{0, 1, 3, 7, 15, 31, 63, 127},
		TermLag: []int64{0, 1, 2},
	}
}

// normalize fills any dimension the caller left empty from DefaultBuckets.
//
// It exists because an empty bound list means NO BUCKETING (bucket() returns
// the raw value) so a caller who set Queue and forgot TermLag would silently
// get an unbucketed, unbounded term component and every sample would be unique.
// That is the exact failure the whole file is designed against, and it should
// not be reachable by omission.
func (b Buckets) normalize() Buckets {
	d := DefaultBuckets()
	if len(b.Queue) == 0 {
		b.Queue = d.Queue
	}
	if len(b.Conns) == 0 {
		b.Conns = d.Conns
	}
	if len(b.TermLag) == 0 {
		b.TermLag = d.TermLag
	}
	return b
}

// bucket labels v against bounds. The label is the bucket's lower edge, so
// "8" means [8,15] and "128+" means everything above the last bound.
func bucket(v int64, bounds []int64) string {
	if len(bounds) == 0 {
		return strconv.FormatInt(v, 10)
	}
	if v < 0 {
		// A negative count is a target reporting something impossible. It is
		// recorded as its own state rather than clamped, so it cannot masquerade
		// as an empty queue.
		return "neg"
	}
	lower := int64(0)
	for _, b := range bounds {
		if v <= b {
			return strconv.FormatInt(lower, 10)
		}
		lower = b + 1
	}
	return strconv.FormatInt(lower, 10) + "+"
}

// NodeState is one node's abstracted state at one instant.
//
// Every field is a STRING because every field can be unobserved, and
// StateUnknown has to be representable in each of them. A pointer-per-field
// shape would work too and would be three times the code for the same
// guarantee.
type NodeState struct {
	Node string
	// Role is the target's self-reported role, lowercased. StateUnknown when
	// /status did not answer or did not carry one.
	Role string
	// TermLag is the bucketed distance from the highest term observed in the
	// same round. "0" means "level with the most advanced node".
	TermLag string
	// Queue and Conns are the bucketed queue depth and connection count.
	Queue string
	Conns string
	// Up is "up", "down", "paused" or StateUnknown: taken from the container's
	// observed process state, not inferred from a missing probe.
	Up string
}

// String renders the tuple in a stable form.
func (n NodeState) String() string {
	return n.Node + "{role=" + n.Role + ",lag=" + n.TermLag +
		",q=" + n.Queue + ",c=" + n.Conns + ",up=" + n.Up + "}"
}

// GlobalState is one sampling round across every node.
type GlobalState struct {
	// Round is the telemetry sample ordinal the state was taken from.
	Round int64
	// Nodes are the per-node tuples, sorted by node id.
	Nodes []NodeState
}

// Key renders the global tuple. Node ids are INCLUDED: a leader on kv-n1 and a
// leader on kv-n3 are different global states, because a fault schedule targets
// concrete nodes and the two are not interchangeable under it.
func (g GlobalState) Key() string {
	parts := make([]string, 0, len(g.Nodes))
	for _, n := range g.Nodes {
		parts = append(parts, n.String())
	}
	return strings.Join(parts, "|")
}

// ID is the content address of the global tuple.
func (g GlobalState) ID() string {
	sum := sha256.Sum256([]byte(g.Key()))
	return "s_" + hex.EncodeToString(sum[:6])
}

// StateSet is a set of distinct global states.
type StateSet struct {
	byKey map[string]*StateStat
	// rounds is the number of sampling rounds offered, so "3 states from 200
	// rounds" is distinguishable from "3 states from 3 rounds".
	rounds int64
}

// StateStat is one distinct global state and how often it was observed.
type StateStat struct {
	ID    string
	Key   string
	Count int64
	// FirstRound is the earliest sampling round it appeared in.
	FirstRound int64
}

// NewStateSet returns an empty set.
func NewStateSet() *StateSet { return &StateSet{byKey: map[string]*StateStat{}} }

// Add records one global state, returning its id and whether it was new.
func (s *StateSet) Add(g GlobalState) (string, bool) {
	if s.byKey == nil {
		s.byKey = map[string]*StateStat{}
	}
	s.rounds++
	if len(g.Nodes) == 0 {
		// A round in which nothing at all was observed is not a state. Recording
		// it would make "the collector was down" look like a system state.
		return "", false
	}
	k := g.Key()
	st, ok := s.byKey[k]
	if !ok {
		st = &StateStat{ID: g.ID(), Key: k, FirstRound: g.Round}
		s.byKey[k] = st
	}
	st.Count++
	return st.ID, !ok
}

// Len is the number of distinct global states.
func (s *StateSet) Len() int { return len(s.byKey) }

// Rounds is the number of sampling rounds offered.
func (s *StateSet) Rounds() int64 { return s.rounds }

// IDs returns every state id, sorted.
func (s *StateSet) IDs() []string {
	out := make([]string, 0, len(s.byKey))
	for _, st := range s.byKey {
		out = append(out, st.ID)
	}
	sort.Strings(out)
	return out
}

// Stats returns every state, ordered by id.
func (s *StateSet) Stats() []StateStat {
	out := make([]StateStat, 0, len(s.byKey))
	for _, st := range s.byKey {
		out = append(out, *st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Merge folds other into s, returning how many of other's states were new.
func (s *StateSet) Merge(other *StateSet) int64 {
	if other == nil {
		return 0
	}
	if s.byKey == nil {
		s.byKey = map[string]*StateStat{}
	}
	keys := make([]string, 0, len(other.byKey))
	for k := range other.byKey {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var added int64
	for _, k := range keys {
		o := other.byKey[k]
		st, ok := s.byKey[k]
		if !ok {
			cp := *o
			s.byKey[k] = &cp
			added++
			continue
		}
		st.Count += o.Count
		if o.FirstRound < st.FirstRound {
			st.FirstRound = o.FirstRound
		}
	}
	s.rounds += other.rounds
	return added
}

// NewAgainst reports how many of s's states are absent from base.
func (s *StateSet) NewAgainst(base *StateSet) int64 {
	if base == nil {
		return int64(len(s.byKey))
	}
	var n int64
	for k := range s.byKey {
		if _, ok := base.byKey[k]; !ok {
			n++
		}
	}
	return n
}

// ---------------------------------------------------------------------------
// Extraction from telemetry
// ---------------------------------------------------------------------------

// StatesFromDocument abstracts a telemetry document into one GlobalState per
// sampling round.
//
// Rounds are keyed by telemetry.Sample.Seq, which is the per-node sample
// ordinal the collector assigns and never reassigns, so round N holds each
// node's Nth sample, and a node that produced no Nth sample is ABSENT from that
// round rather than silently aligned with a neighbour's.
//
// A node absent from a round contributes an all-unknown tuple rather than being
// dropped, so "we could not see kv-n2" is a different global state from "kv-n2
// was not part of this cluster".
func StatesFromDocument(doc *telemetry.Document, b Buckets) []GlobalState {
	if doc == nil {
		return nil
	}
	b = b.normalize()
	nodes := make([]string, 0, len(doc.Nodes))
	byNodeRound := map[string]map[int64]*telemetry.Sample{}
	maxSeq := int64(-1)
	for i := range doc.Nodes {
		ns := &doc.Nodes[i]
		if ns.Node == "" {
			continue
		}
		nodes = append(nodes, ns.Node)
		m := map[int64]*telemetry.Sample{}
		for j := range ns.Samples {
			s := &ns.Samples[j]
			m[s.Seq] = s
			if s.Seq > maxSeq {
				maxSeq = s.Seq
			}
		}
		byNodeRound[ns.Node] = m
	}
	sort.Strings(nodes)
	if len(nodes) == 0 || maxSeq < 0 {
		return nil
	}

	out := make([]GlobalState, 0, maxSeq+1)
	for seq := int64(0); seq <= maxSeq; seq++ {
		round := make([]*telemetry.Sample, 0, len(nodes))
		any := false
		for _, n := range nodes {
			s := byNodeRound[n][seq]
			round = append(round, s)
			if s != nil {
				any = true
			}
		}
		if !any {
			continue
		}
		out = append(out, buildGlobalState(seq, nodes, round, b))
	}
	return out
}

// buildGlobalState abstracts one aligned round.
func buildGlobalState(seq int64, nodes []string, round []*telemetry.Sample, b Buckets) GlobalState {
	// The round's reference term is the highest term any node reported. It is
	// the only scale-free anchor available: no node knows the "true" term, and
	// an absolute term is world-specific (see the file header).
	var maxTerm int64
	haveTerm := false
	for _, s := range round {
		if s == nil || s.Status == nil || s.Status.Term == nil {
			continue
		}
		if !haveTerm || *s.Status.Term > maxTerm {
			maxTerm = *s.Status.Term
			haveTerm = true
		}
	}

	g := GlobalState{Round: seq, Nodes: make([]NodeState, 0, len(nodes))}
	for i, name := range nodes {
		g.Nodes = append(g.Nodes, abstractNode(name, round[i], maxTerm, haveTerm, b))
	}
	return g
}

func abstractNode(name string, s *telemetry.Sample, maxTerm int64, haveTerm bool, b Buckets) NodeState {
	n := NodeState{
		Node:    name,
		Role:    StateUnknown,
		TermLag: StateUnknown,
		Queue:   StateUnknown,
		Conns:   StateUnknown,
		Up:      StateUnknown,
	}
	if s == nil {
		return n
	}

	n.Up = processState(s)

	if s.Status != nil {
		if s.Status.Role != nil && *s.Status.Role != "" {
			n.Role = strings.ToLower(*s.Status.Role)
		}
		if haveTerm && s.Status.Term != nil {
			lag := maxTerm - *s.Status.Term
			if lag < 0 {
				lag = 0
			}
			n.TermLag = bucket(lag, b.TermLag)
		}
		if q, ok := s.QueueDepth(); ok {
			n.Queue = bucket(q, b.Queue)
		}
		n.Conns = connections(s, b)
	}
	return n
}

// processState reads the container's observed process state.
//
// It reads ProcessMetrics rather than inferring liveness from a failed probe: a
// probe can fail because the host is busy, and calling that "down" would put a
// fabricated state into the coverage counter.
func processState(s *telemetry.Sample) string {
	p := s.Process
	if p == nil {
		return StateUnknown
	}
	if p.Paused != nil && *p.Paused {
		return "paused"
	}
	if p.Running != nil {
		if *p.Running {
			return "up"
		}
		return "down"
	}
	if p.Status != "" {
		return strings.ToLower(p.Status)
	}
	return StateUnknown
}

// connections renders the client and peer connection counts as one component,
// "<client>/<peer>", each independently bucketed and each independently able to
// be StateUnknown.
//
// They are NOT summed. A node reporting 4 client connections and no peer figure
// would then be indistinguishable from one reporting 4 peers and no clients,
// and from one reporting 2 of each: three different cluster conditions folded
// into one state. Conflating two quantities because they are both counts is the
// same mistake internal/telemetry refuses to make with RSS.
func connections(s *telemetry.Sample, b Buckets) string {
	client, peer := StateUnknown, StateUnknown
	if s.Status != nil {
		if s.Status.ClientConnections != nil {
			client = bucket(*s.Status.ClientConnections, b.Conns)
		}
		if s.Status.PeerConnections != nil {
			peer = bucket(*s.Status.PeerConnections, b.Conns)
		}
	}
	if client == StateUnknown && peer == StateUnknown {
		return StateUnknown
	}
	return client + "/" + peer
}
