package search

import (
	"strings"
	"testing"

	"github.com/WrdCstlg/pro-thesis/internal/telemetry"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

func i64p(v int64) *int64     { return &v }
func strPtr(v string) *string { return &v }
func boolPtr(v bool) *bool    { return &v }

// sample builds a telemetry sample with a /status document.
func sample(node string, seq int64, role string, term, queue, clients, peers int64, running bool) telemetry.Sample {
	return telemetry.Sample{
		Schema:  telemetry.SampleSchema,
		Seq:     seq,
		Node:    node,
		Phase:   schema.PhaseDrive,
		Process: &telemetry.ProcessMetrics{Status: "running", Running: boolPtr(running), Paused: boolPtr(false)},
		Status: &telemetry.StatusMetrics{
			OK:                true,
			Role:              strPtr(role),
			Term:              i64p(term),
			QueueDepth:        i64p(queue),
			ClientConnections: i64p(clients),
			PeerConnections:   i64p(peers),
		},
	}
}

func doc(nodes ...telemetry.NodeSeries) *telemetry.Document {
	return &telemetry.Document{Schema: telemetry.DocumentSchema, Nodes: nodes}
}

// ---------------------------------------------------------------------------
// Bucketing
// ---------------------------------------------------------------------------

func TestBucketingCollapsesNearbyValuesAndSeparatesFarOnes(t *testing.T) {
	b := DefaultBuckets()
	if bucket(41, b.Queue) != bucket(42, b.Queue) {
		t.Fatal("queue depths 41 and 42 fell in different buckets; unbucketed values make every sample unique")
	}
	if bucket(4, b.Queue) == bucket(4000, b.Queue) {
		t.Fatal("queue depths 4 and 4000 fell in the same bucket; that is not a signal")
	}
	if got := bucket(0, b.Queue); got != "0" {
		t.Fatalf("bucket(0) = %q, want %q", got, "0")
	}
	if got := bucket(5000, b.Queue); !strings.HasSuffix(got, "+") {
		t.Fatalf("bucket(5000) = %q, want an overflow bucket", got)
	}
	if got := bucket(-1, b.Queue); got != "neg" {
		t.Fatalf("bucket(-1) = %q; a negative count must not masquerade as an empty queue", got)
	}
}

// TestBucketingIsMonotone: a larger value never lands in a lower bucket. A
// non-monotone bucketing would make the abstraction incoherent.
func TestBucketingIsMonotone(t *testing.T) {
	b := DefaultBuckets()
	prev := ""
	order := map[string]int{}
	n := 0
	for v := int64(0); v <= 300; v++ {
		lbl := bucket(v, b.Queue)
		if lbl != prev {
			if _, seen := order[lbl]; seen {
				t.Fatalf("bucket label %q reappeared after leaving it at v=%d", lbl, v)
			}
			order[lbl] = n
			n++
			prev = lbl
		}
	}
	if n < 5 {
		t.Fatalf("only %d buckets over [0,300]; the abstraction is too coarse to carry signal", n)
	}
}

// ---------------------------------------------------------------------------
// The term component
// ---------------------------------------------------------------------------

// TestTermIsRelativeToTheRound is the central design claim. An absolute term is
// unbounded and world-specific: two runs that reach term 3 and term 65 in the
// same cluster shape would share no state at all, and the global counter would
// become a world counter.
func TestTermIsRelativeToTheRound(t *testing.T) {
	low := doc(
		telemetry.NodeSeries{Node: "kv-n1", Samples: []telemetry.Sample{sample("kv-n1", 0, "leader", 3, 0, 4, 2, true)}},
		telemetry.NodeSeries{Node: "kv-n2", Samples: []telemetry.Sample{sample("kv-n2", 0, "follower", 3, 0, 4, 2, true)}},
	)
	high := doc(
		telemetry.NodeSeries{Node: "kv-n1", Samples: []telemetry.Sample{sample("kv-n1", 0, "leader", 65, 0, 4, 2, true)}},
		telemetry.NodeSeries{Node: "kv-n2", Samples: []telemetry.Sample{sample("kv-n2", 0, "follower", 65, 0, 4, 2, true)}},
	)
	b := DefaultBuckets()
	a := StatesFromDocument(low, b)
	c := StatesFromDocument(high, b)
	if len(a) != 1 || len(c) != 1 {
		t.Fatalf("want one round each, got %d and %d", len(a), len(c))
	}
	if a[0].Key() != c[0].Key() {
		t.Fatalf("the same cluster condition at term 3 and term 65 produced two states:\n  %s\n  %s",
			a[0].Key(), c[0].Key())
	}
}

// TestTermDivergenceIsVisible is the other half: the abstraction must still see
// a node falling behind, because that IS the fixture's stale-read condition.
func TestTermDivergenceIsVisible(t *testing.T) {
	together := doc(
		telemetry.NodeSeries{Node: "kv-n1", Samples: []telemetry.Sample{sample("kv-n1", 0, "leader", 66, 0, 4, 2, true)}},
		telemetry.NodeSeries{Node: "kv-n2", Samples: []telemetry.Sample{sample("kv-n2", 0, "follower", 66, 0, 4, 2, true)}},
	)
	behind := doc(
		telemetry.NodeSeries{Node: "kv-n1", Samples: []telemetry.Sample{sample("kv-n1", 0, "leader", 66, 0, 4, 2, true)}},
		telemetry.NodeSeries{Node: "kv-n2", Samples: []telemetry.Sample{sample("kv-n2", 0, "follower", 64, 0, 4, 2, true)}},
	)
	b := DefaultBuckets()
	a := StatesFromDocument(together, b)[0]
	c := StatesFromDocument(behind, b)[0]
	if a.Key() == c.Key() {
		t.Fatalf("a node two terms behind produced the same state as a converged cluster: %s", a.Key())
	}
}

// TestTwoLeadersAtDifferentNodesAreDifferentStates: node identity is part of the
// tuple, because a fault schedule targets concrete nodes and the two are not
// interchangeable under it.
func TestTwoLeadersAtDifferentNodesAreDifferentStates(t *testing.T) {
	b := DefaultBuckets()
	one := StatesFromDocument(doc(
		telemetry.NodeSeries{Node: "kv-n1", Samples: []telemetry.Sample{sample("kv-n1", 0, "leader", 5, 0, 4, 2, true)}},
		telemetry.NodeSeries{Node: "kv-n2", Samples: []telemetry.Sample{sample("kv-n2", 0, "follower", 5, 0, 4, 2, true)}},
	), b)[0]
	two := StatesFromDocument(doc(
		telemetry.NodeSeries{Node: "kv-n1", Samples: []telemetry.Sample{sample("kv-n1", 0, "follower", 5, 0, 4, 2, true)}},
		telemetry.NodeSeries{Node: "kv-n2", Samples: []telemetry.Sample{sample("kv-n2", 0, "leader", 5, 0, 4, 2, true)}},
	), b)[0]
	if one.Key() == two.Key() {
		t.Fatal("leadership on kv-n1 and on kv-n2 produced the same global state")
	}
}

// ---------------------------------------------------------------------------
// Absent stays absent
// ---------------------------------------------------------------------------

// TestAnAbsentMetricIsUnknownNotZero is the soundness claim internal/telemetry
// exists to protect, applied here. A paused node reports nothing; recording
// "queue depth 0" would tell the search the cluster was quiet.
func TestAnAbsentMetricIsUnknownNotZero(t *testing.T) {
	quiet := sample("kv-n2", 0, "follower", 5, 0, 0, 0, true)

	unseen := telemetry.Sample{
		Schema:  telemetry.SampleSchema,
		Seq:     0,
		Node:    "kv-n2",
		Process: &telemetry.ProcessMetrics{Status: "paused", Running: boolPtr(true), Paused: boolPtr(true)},
	}
	unseen.MarkAbsent(telemetry.MetricStatus, "container is paused")

	b := DefaultBuckets()
	a := abstractNode("kv-n2", &quiet, 5, true, b)
	c := abstractNode("kv-n2", &unseen, 5, true, b)

	if a.Queue == c.Queue {
		t.Fatalf("an unobserved queue depth was recorded as %q, the same as an observed empty queue", c.Queue)
	}
	if c.Queue != StateUnknown || c.Role != StateUnknown || c.TermLag != StateUnknown || c.Conns != StateUnknown {
		t.Fatalf("unobserved components were not all %q: %+v", StateUnknown, c)
	}
	if c.Up != "paused" {
		t.Fatalf("a paused container's process state = %q, want %q (it IS observable)", c.Up, "paused")
	}
}

// TestANodeMissingFromARoundIsUnknownNotDropped: a node that produced no sample
// in a round must change the global tuple rather than silently vanish from it,
// or "we could not see kv-n2" would look like "kv-n2 was never in the cluster".
func TestANodeMissingFromARoundIsUnknownNotDropped(t *testing.T) {
	d := doc(
		telemetry.NodeSeries{Node: "kv-n1", Samples: []telemetry.Sample{
			sample("kv-n1", 0, "leader", 5, 0, 4, 2, true),
			sample("kv-n1", 1, "leader", 5, 0, 4, 2, true),
		}},
		telemetry.NodeSeries{Node: "kv-n2", Samples: []telemetry.Sample{
			sample("kv-n2", 0, "follower", 5, 0, 4, 2, true),
			// no sample with Seq 1: the collector could not reach it.
		}},
	)
	states := StatesFromDocument(d, DefaultBuckets())
	if len(states) != 2 {
		t.Fatalf("got %d rounds, want 2", len(states))
	}
	if len(states[1].Nodes) != 2 {
		t.Fatalf("round 1 has %d node tuples, want 2 (the missing node must still appear)", len(states[1].Nodes))
	}
	if states[0].Key() == states[1].Key() {
		t.Fatal("a round in which one node was unobservable produced the same state as a fully observed one")
	}
	if !strings.Contains(states[1].Key(), StateUnknown) {
		t.Fatalf("the missing node did not appear as unknown: %s", states[1].Key())
	}
}

// TestClientAndPeerConnectionsAreNotSummed: two different counts folded into one
// number is the conflation internal/telemetry refuses to make with RSS.
func TestClientAndPeerConnectionsAreNotSummed(t *testing.T) {
	b := DefaultBuckets()
	fourClients := sample("kv-n1", 0, "leader", 5, 0, 4, 0, true)
	fourPeers := sample("kv-n1", 0, "leader", 5, 0, 0, 4, true)
	twoEach := sample("kv-n1", 0, "leader", 5, 0, 2, 2, true)

	a := abstractNode("kv-n1", &fourClients, 5, true, b).Conns
	c := abstractNode("kv-n1", &fourPeers, 5, true, b).Conns
	e := abstractNode("kv-n1", &twoEach, 5, true, b).Conns
	if a == c || a == e || c == e {
		t.Fatalf("connection counts collapsed: 4/0=%q 0/4=%q 2/2=%q", a, c, e)
	}
}

// ---------------------------------------------------------------------------
// Set behaviour
// ---------------------------------------------------------------------------

func TestStateSetCountsDistinctGlobalTuples(t *testing.T) {
	// A steady cluster sampled 50 times is ONE state, not fifty.
	samplesA := make([]telemetry.Sample, 0, 50)
	samplesB := make([]telemetry.Sample, 0, 50)
	for i := int64(0); i < 50; i++ {
		samplesA = append(samplesA, sample("kv-n1", i, "leader", 5, int64(i%2), 4, 2, true))
		samplesB = append(samplesB, sample("kv-n2", i, "follower", 5, int64(i%2), 4, 2, true))
	}
	d := doc(
		telemetry.NodeSeries{Node: "kv-n1", Samples: samplesA},
		telemetry.NodeSeries{Node: "kv-n2", Samples: samplesB},
	)
	set := NewStateSet()
	for _, g := range StatesFromDocument(d, DefaultBuckets()) {
		set.Add(g)
	}
	// Queue depth alternates 0,1 which are two buckets, on both nodes together,
	// so exactly two global tuples.
	if set.Len() != 2 {
		t.Fatalf("50 rounds of a steady cluster produced %d states, want 2:\n%v", set.Len(), set.IDs())
	}
	if set.Rounds() != 50 {
		t.Fatalf("Rounds() = %d, want 50", set.Rounds())
	}
}

// TestADriftingCounterDoesNotMakeEverySampleUnique is the reason bucketing
// exists, measured. A queue depth that climbs 40..89 across fifty rounds is one
// condition getting slowly worse, not fifty distinct cluster states. Without
// bucketing this returns 50 and the state counter becomes a sample counter.
func TestADriftingCounterDoesNotMakeEverySampleUnique(t *testing.T) {
	samples := make([]telemetry.Sample, 0, 50)
	for i := int64(0); i < 50; i++ {
		samples = append(samples, sample("kv-n1", i, "leader", 5, 40+i, 4, 2, true))
	}
	set := NewStateSet()
	for _, g := range StatesFromDocument(doc(telemetry.NodeSeries{Node: "kv-n1", Samples: samples}), DefaultBuckets()) {
		set.Add(g)
	}
	if set.Len() >= 10 {
		t.Fatalf("a queue climbing 40..89 produced %d distinct states; unbucketed values make "+
			"every sample unique and the counter measures sampling rate", set.Len())
	}
	if set.Len() < 2 {
		t.Fatalf("a queue climbing 40..89 produced %d state(s); the abstraction is too coarse "+
			"to see the queue growing at all", set.Len())
	}
}

func TestStateSetMergeAndDeltaAreOrderIndependent(t *testing.T) {
	mk := func(role string, term int64) GlobalState {
		d := doc(telemetry.NodeSeries{Node: "kv-n1", Samples: []telemetry.Sample{
			sample("kv-n1", 0, role, term, 0, 4, 2, true),
		}})
		return StatesFromDocument(d, DefaultBuckets())[0]
	}
	base := NewStateSet()
	base.Add(mk("leader", 5))

	world := NewStateSet()
	world.Add(mk("leader", 5))
	world.Add(mk("follower", 5))

	if got := world.NewAgainst(base); got != 1 {
		t.Fatalf("NewAgainst = %d, want 1", got)
	}
	if got := base.Merge(world); got != 1 {
		t.Fatalf("Merge = %d, want 1", got)
	}
	if got := world.NewAgainst(base); got != 0 {
		t.Fatalf("after merging, NewAgainst = %d, want 0", got)
	}
}
