package telemetry

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/WrdCstlg/pro-thesis/internal/recorder"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// ---------------------------------------------------------------------------
// a Docker that answers from a script instead of a daemon
// ---------------------------------------------------------------------------

type fakeDocker struct {
	mu sync.Mutex

	states     map[string]ContainerState
	inspectErr error

	// execOut is consumed one entry per call, per container; the last entry
	// repeats once exhausted.
	execOut   map[string][]string
	execErr   map[string]error
	execCalls []string
	scripts   []string

	stats    map[string]DockerStats
	statsErr error
}

func newFakeDocker() *fakeDocker {
	return &fakeDocker{
		states:  map[string]ContainerState{},
		execOut: map[string][]string{},
		execErr: map[string]error{},
		stats:   map[string]DockerStats{},
	}
}

func (f *fakeDocker) InspectStates(_ context.Context, containers []string) (map[string]ContainerState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.inspectErr != nil {
		return nil, f.inspectErr
	}
	out := map[string]ContainerState{}
	for _, c := range containers {
		if st, ok := f.states[c]; ok {
			out[c] = st
		}
	}
	return out, nil
}

func (f *fakeDocker) ExecScript(_ context.Context, container, script string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.execCalls = append(f.execCalls, container)
	f.scripts = append(f.scripts, script)
	if err, ok := f.execErr[container]; ok && err != nil {
		return "", err
	}
	outs := f.execOut[container]
	if len(outs) == 0 {
		return "", nil
	}
	if len(outs) == 1 {
		return outs[0], nil
	}
	head := outs[0]
	f.execOut[container] = outs[1:]
	return head, nil
}

func (f *fakeDocker) Stats(_ context.Context, containers []string) (map[string]DockerStats, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.statsErr != nil {
		return nil, f.statsErr
	}
	out := map[string]DockerStats{}
	for _, c := range containers {
		if r, ok := f.stats[c]; ok {
			out[c] = r
		}
	}
	return out, nil
}

func (f *fakeDocker) execCount(container string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.execCalls {
		if c == container {
			n++
		}
	}
	return n
}

var _ Docker = (*fakeDocker)(nil)

// cpuStatAdvanced is cpuStatFixture after 250 ms more CPU time.
const cpuStatAdvanced = `usage_usec 5071334
user_usec 3270445
system_usec 1800889
nr_periods 413
nr_throttled 7
throttled_usec 91234`

// ---------------------------------------------------------------------------
// a whole sampling round
// ---------------------------------------------------------------------------

// One round must produce one sample per target no matter what any individual
// target did. A collector that stopped on the first failure would stop exactly
// when the system under test started misbehaving.
func TestSampleRoundProducesOneSamplePerTargetAndDegradesPerNode(t *testing.T) {
	clk := recorder.NewSimClock(time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC))
	defer clk.Close()

	var tl recorder.Timeline
	if err := tl.StartDriveAt(clk.NowR(), clk.NowWall().UnixNano()); err != nil {
		t.Fatalf("timeline: %v", err)
	}

	fd := newFakeDocker()
	fd.states["c1"] = ContainerState{
		Status: "running", Running: true, Pid: 4242,
		StartedAt: "2026-09-07T11:59:00.123456789Z", FinishedAt: "0001-01-01T00:00:00Z",
	}
	// A paused container: docker reports Running true AND Paused true.
	fd.states["c2"] = ContainerState{
		Status: "paused", Running: true, Paused: true, Pid: 4243,
		StartedAt: "2026-09-07T11:59:00.500000000Z", FinishedAt: "0001-01-01T00:00:00Z",
	}
	fd.execOut["c1"] = []string{
		healthyExecOutput(),
		execOutputV2(memoryStatV2Baseline, "381681664", cpuStatAdvanced, "14", procStatusFixture, "23"),
	}

	var buf bytes.Buffer
	sink := NewWriterSink(&buf)

	c, err := New(Options{
		Targets: []Target{
			{Node: "kv-n1", Container: "c1"},
			{Node: "kv-n2", Container: "c2"},
			{Node: "kv-n3"}, // bound to no container at all
		},
		Sink:     sink,
		Docker:   fd,
		Clock:    clk,
		Timeline: &tl,
		Interval: 500 * time.Millisecond,
		PhaseFn:  func() schema.Phase { return schema.PhaseDrive },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	round1 := c.SampleRound(context.Background())
	clk.Advance(500 * time.Millisecond)
	round2 := c.SampleRound(context.Background())

	if len(round1) != 3 || len(round2) != 3 {
		t.Fatalf("rounds produced %d and %d samples, want 3 each; a target that cannot be measured "+
			"must still produce a sample carrying the reason", len(round1), len(round2))
	}
	if c.Rounds() != 2 {
		t.Fatalf("Rounds() = %d, want 2", c.Rounds())
	}

	byNode := func(rd []*Sample, node string) *Sample {
		t.Helper()
		for _, s := range rd {
			if s.Node == node {
				return s
			}
		}
		t.Fatalf("no sample for %s", node)
		return nil
	}

	// --- the healthy node -------------------------------------------------
	n1a, n1b := byNode(round1, "kv-n1"), byNode(round2, "kv-n1")
	if n1a.Seq != 0 || n1b.Seq != 1 {
		t.Fatalf("kv-n1 seq = %d then %d, want 0 then 1; a consumer detects a dropped round by "+
			"a gap in this number", n1a.Seq, n1b.Seq)
	}
	if rss, ok := n1a.RSS(); !ok || rss != fixtureAnonBytes {
		t.Fatalf("kv-n1 rss = %d ok=%v, want %d", rss, ok, fixtureAnonBytes)
	}
	if !n1a.Process.IsUp() {
		t.Fatal("kv-n1 was reported not up")
	}
	if n1a.Process.StartedAtNS == nil {
		t.Fatal("started_at_ns was dropped; no_crash needs it to tell a planned restart from a crash")
	}
	if n1a.Process.FinishedAtNS != nil {
		t.Fatalf("finished_at_ns = %d for a running container. Docker's zero time means NEVER and "+
			"must not become a timestamp 62 billion seconds in the past.", *n1a.Process.FinishedAtNS)
	}
	if n1a.CPU.MilliPct != nil {
		t.Fatalf("a rate (%d) was derived from the very first sample of a node",
			*n1a.CPU.MilliPct)
	}
	if reason, ok := n1a.AbsentReason(MetricCPUMilliPct); !ok || !strings.Contains(reason, "two samples") {
		t.Fatalf("first sample's cpu.milli_pct absence reason = %q", reason)
	}
	// 250 ms of CPU over the 500 ms interval = half a core = 50000 milli-pct.
	if n1b.CPU == nil || n1b.CPU.MilliPct == nil {
		t.Fatal("no rate on the second sample, where two cumulative readings and a positive " +
			"interval both exist")
	}
	if *n1b.CPU.MilliPct != 50000 {
		t.Fatalf("cpu.milli_pct = %d, want 50000 (250ms of CPU over a 500ms interval is half a "+
			"core). The rate must come from the monotonic interval, not from wall time.",
			*n1b.CPU.MilliPct)
	}

	// --- the paused node --------------------------------------------------
	n2 := byNode(round1, "kv-n2")
	if fd.execCount("c2") != 0 {
		t.Fatalf("a paused container was exec'd %d times. `docker exec` against a paused container "+
			"is refused by the daemon, so this would turn a fault the harness deliberately "+
			"injected into a collection error every interval.", fd.execCount("c2"))
	}
	if n2.Memory != nil || n2.CPU != nil || n2.Tasks != nil {
		t.Fatalf("a paused container produced cgroup metrics: memory=%+v cpu=%+v tasks=%+v",
			n2.Memory, n2.CPU, n2.Tasks)
	}
	for _, m := range []string{MetricMemory, MetricMemoryRSS, MetricCPU, MetricCPUMilliPct,
		MetricTasks, MetricTasksOpenFDs} {
		reason, ok := n2.AbsentReason(m)
		if !ok {
			t.Fatalf("%s carries no absence reason for a paused container", m)
		}
		if !strings.Contains(reason, "paused") {
			t.Fatalf("%s absence reason %q does not say the container was paused. \"We could not "+
				"measure\" and \"the process was frozen by the fault under test\" are different "+
				"facts and the oracle's diagnosis depends on which one it is.", m, reason)
		}
	}
	if n2.Process == nil || n2.Process.Paused == nil || !*n2.Process.Paused {
		t.Fatal("the paused state itself must still be recorded; it is the observation, not the failure")
	}
	if n2.Process.IsUp() {
		t.Fatal("IsUp() must be false for a paused container")
	}

	// --- the unbound node -------------------------------------------------
	n3 := byNode(round1, "kv-n3")
	if n3.Process != nil {
		t.Fatalf("process metrics were invented for a node the topology binds no container to: %+v",
			n3.Process)
	}
	if reason, ok := n3.AbsentReason(MetricProcess); !ok || !strings.Contains(reason, "no container") {
		t.Fatalf("kv-n3 process absence reason = %q, want it to name the missing binding", reason)
	}

	// --- one exec per running container per round -------------------------
	if got := fd.execCount("c1"); got != 2 {
		t.Fatalf("kv-n1 was exec'd %d times over two rounds, want 2. The batched read exists "+
			"because five separate execs cost ~530ms, which exceeds the 500ms interval on its own.",
			got)
	}
	fd.mu.Lock()
	script := fd.scripts[0]
	fd.mu.Unlock()
	for _, path := range []string{pathMemoryStatV2, pathMemoryCurrent, pathCPUStat, pathPidsCurrent,
		pathMemoryStatV1, pathProcStatus} {
		if !strings.Contains(script, path) {
			t.Fatalf("the batched read script does not mention %s, so that metric can never be "+
				"observed", path)
		}
	}

	// --- virtual time -----------------------------------------------------
	if n1a.VMS == nil || *n1a.VMS != 0 {
		t.Fatalf("v_ms = %v at the DRIVE origin, want 0", derefI64(n1a.VMS))
	}
	if n1b.VMS == nil || *n1b.VMS != 500 {
		t.Fatalf("v_ms = %v 500ms after the DRIVE origin, want 500. v_ms is the frame "+
			"prothesis.oracle_input/v1's phases array uses, so an oracle cannot phase-scope "+
			"anything without it.", derefI64(n1b.VMS))
	}
	if n1a.Phase != schema.PhaseDrive {
		t.Fatalf("phase = %q, want DRIVE", n1a.Phase)
	}
	if n1a.TNS == 0 || n1b.TNS <= n1a.TNS {
		t.Fatalf("t_ns did not advance: %d then %d", n1a.TNS, n1b.TNS)
	}
	first, last, ok := c.Window()
	if !ok || first != n1a.TNS || last != n1b.TNS {
		t.Fatalf("Window() = (%d, %d, %v), want (%d, %d, true)", first, last, ok, n1a.TNS, n1b.TNS)
	}

	// --- the JSONL the sink wrote is the document's only source -----------
	if err := sink.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if got := sink.Count(); got != 6 {
		t.Fatalf("sink received %d samples, want 6", got)
	}
	doc, err := BuildDocument(bytes.NewReader(buf.Bytes()), DocumentMeta{RunID: "r1", IntervalMS: 500})
	if err != nil {
		t.Fatalf("BuildDocument: %v", err)
	}
	if doc.SampleCount != 6 || len(doc.Nodes) != 3 {
		t.Fatalf("document has %d samples over %d nodes, want 6 over 3", doc.SampleCount, len(doc.Nodes))
	}
	for _, ns := range doc.Nodes {
		if ns.Gaps != 0 {
			t.Fatalf("node %s reports %d sequence gaps in a stream with none", ns.Node, ns.Gaps)
		}
	}
	if !doc.Coverage.Observed(MetricMemoryRSS) {
		t.Fatal("coverage says memory.rss_bytes was never observed, but kv-n1 reported it")
	}
	// Absent on ANY node means absent in the census: an oracle comparing three
	// nodes cannot soundly conclude anything from two of them.
	miss, ok := doc.Coverage.Missing(MetricMemoryRSS)
	if !ok {
		t.Fatal("memory.rss_bytes was absent on kv-n2 and kv-n3 but the census does not record it; " +
			"\"observed somewhere\" must not read as \"observed\"")
	}
	if len(miss.Nodes) != 2 || miss.Nodes[0] != "kv-n2" || miss.Nodes[1] != "kv-n3" {
		t.Fatalf("memory.rss_bytes absent on %v, want [kv-n2 kv-n3]", miss.Nodes)
	}
	if miss.Reason == "" {
		t.Fatal("the census records the absence but not why")
	}
}

// Virtual time does not exist before DRIVE. Emitting 0 would place every BOOT
// sample at the DRIVE origin, which is a lie an oracle cannot detect.
func TestVirtualTimeIsNullBeforeDrive(t *testing.T) {
	clk := recorder.NewSimClock(time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC))
	defer clk.Close()

	fd := newFakeDocker()
	fd.states["c1"] = ContainerState{Status: "running", Running: true}
	fd.execOut["c1"] = []string{healthyExecOutput()}

	sink := &MemorySink{}
	c, err := New(Options{
		Targets: []Target{{Node: "kv-n1", Container: "c1"}},
		Sink:    sink,
		Docker:  fd,
		Clock:   clk,
		PhaseFn: func() schema.Phase { return schema.PhaseBoot },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s := c.SampleRound(context.Background())[0]

	if s.VMS != nil {
		t.Fatalf("v_ms = %d during BOOT, before any DRIVE origin was stamped. It must be null: "+
			"there is no virtual clock yet, and 0 means \"at the DRIVE origin\".", *s.VMS)
	}
	line, err := s.MarshalLine()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(line), `"v_ms":null`) {
		t.Fatalf("v_ms did not serialize as null.\n%s", line)
	}
}

// An exec that fails (no POSIX shell in the image) must degrade to absences
// with a reason, never to zeros and never to a dropped sample.
func TestExecFailureBecomesAbsenceNotZero(t *testing.T) {
	clk := recorder.NewSimClock(time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC))
	defer clk.Close()

	fd := newFakeDocker()
	fd.states["c1"] = ContainerState{Status: "running", Running: true}
	fd.execErr["c1"] = errExecFailed{}

	sink := &MemorySink{}
	c, err := New(Options{
		Targets: []Target{{Node: "kv-n1", Container: "c1"}},
		Sink:    sink, Docker: fd, Clock: clk,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s := c.SampleRound(context.Background())[0]

	if s.Memory != nil || s.CPU != nil || s.Tasks != nil {
		t.Fatalf("cgroup metrics were produced from a failed exec: memory=%+v cpu=%+v tasks=%+v",
			s.Memory, s.CPU, s.Tasks)
	}
	reason, ok := s.AbsentReason(MetricMemoryRSS)
	if !ok {
		t.Fatal("no reason recorded for the missing rss on a distroless-style target")
	}
	if !strings.Contains(reason, "OQ-063") {
		t.Fatalf("absence reason %q does not point at the open question that explains the "+
			"limitation, so a user has no way to learn why their target is unmeasurable", reason)
	}
	if s.Process == nil || s.Process.Running == nil || !*s.Process.Running {
		t.Fatal("a failed cgroup read must not cost the inspect-derived process state, which is " +
			"what no_crash actually runs on")
	}
}

type errExecFailed struct{}

func (errExecFailed) Error() string {
	return `OCI runtime exec failed: exec failed: unable to start container process: exec: "sh": executable file not found in $PATH`
}

func TestNewRejectsUnusableOptions(t *testing.T) {
	sink := &MemorySink{}
	if _, err := New(Options{Sink: sink}); err == nil {
		t.Fatal("a collector with no targets was accepted; it would produce an empty telemetry " +
			"document that reads exactly like a healthy quiet run")
	}
	if _, err := New(Options{Targets: []Target{{Node: "n1"}}}); err == nil {
		t.Fatal("a collector with no sink was accepted; every sample would be discarded silently")
	}
	if _, err := New(Options{Targets: []Target{{Node: ""}}, Sink: sink}); err == nil {
		t.Fatal("a target with no node id was accepted; its samples could not be attributed")
	}
	if _, err := New(Options{Targets: []Target{{Node: "n1"}, {Node: "n1"}}, Sink: sink}); err == nil {
		t.Fatal("duplicate node ids were accepted; their sequence numbers would interleave and " +
			"every rate derived from them would be wrong")
	}
	c, err := New(Options{Targets: []Target{{Node: "n1"}}, Sink: sink})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Stop()
	if c.Interval() != DefaultIntervalMS*time.Millisecond {
		t.Fatalf("default interval = %s, want %dms (Addendum A.4 and A.10)",
			c.Interval(), DefaultIntervalMS)
	}
}

// ---------------------------------------------------------------------------
// 6. TargetsFromTopology
// ---------------------------------------------------------------------------

func topologyConfig() *schema.Config {
	cfg := &schema.Config{}
	cfg.Harness.Nodes = []schema.NodeConfig{
		{ID: "kv-n1", Service: "kv", ComposeService: "kv-n1"},
		{ID: "kv-n2", Service: "kv", ComposeService: "kv-n2"},
		{ID: "kv-n3", Service: "kv", ComposeService: "kv-n3"},
		{ID: "pg", Service: "postgres"},
	}
	cfg.Harness.Health = []schema.HealthProbe{
		{Node: "kv:*", Probe: "http://{host}:{port}/healthz"},
	}
	return cfg
}

// Every node the topology bound must become a target. A node silently missing
// from the sampling set is a node no oracle can ever say anything about, and
// nothing in the run would report that it was skipped.
func TestTargetsFromTopologyCoversEveryBoundNode(t *testing.T) {
	cfg := topologyConfig()
	top := &recorder.Topology{Nodes: []recorder.NodeBinding{
		{ID: "kv-n1", ContainerID: "c1", HostPort: 18081, ContainerPort: 8080, Service: "kv-n1"},
		{ID: "kv-n2", ContainerID: "c2", HostPort: 18082, ContainerPort: 8080, Service: "kv-n2"},
		{ID: "kv-n3", ContainerID: "c3", HostPort: 18083, ContainerPort: 8080, Service: "kv-n3"},
		{ID: "pg", ContainerID: "c4", HostPort: 15432, ContainerPort: 5432, Service: "postgres"},
	}}

	got, err := TargetsFromTopology(cfg, top)
	if err != nil {
		t.Fatalf("TargetsFromTopology: %v", err)
	}
	if len(got) != len(top.Nodes) {
		t.Fatalf("%d targets for %d bound nodes; a dropped node is invisible in the telemetry "+
			"document and no oracle can report on it", len(got), len(top.Nodes))
	}
	for i, want := range top.Nodes {
		if got[i].Node != want.ID || got[i].Container != want.ContainerID {
			t.Fatalf("target[%d] = %s/%s, want %s/%s; the order must follow the topology so a "+
				"reader can line the two files up", i, got[i].Node, got[i].Container,
				want.ID, want.ContainerID)
		}
	}
	if got[0].ProbeURL != "http://127.0.0.1:18081/healthz" {
		t.Fatalf("kv-n1 probe = %q. It must be the SAME expansion WaitHealthy used at BOOT, or a "+
			"node is healthy at boot and unreachable ever after for no visible reason",
			got[0].ProbeURL)
	}
	if got[2].ProbeURL != "http://127.0.0.1:18083/healthz" {
		t.Fatalf("kv-n3 probe = %q, want the kv:* wildcard to reach every kv node", got[2].ProbeURL)
	}
	if got[0].StatusURL != "http://127.0.0.1:18081"+StatusPath {
		t.Fatalf("kv-n1 status = %q, want the conventional %s on the same published port",
			got[0].StatusURL, StatusPath)
	}
	// postgres declares no health probe. That is not an error: the probe metric
	// is simply absent for it.
	if got[3].ProbeURL != "" {
		t.Fatalf("pg got probe %q, but no health entry targets it", got[3].ProbeURL)
	}
	if got[3].StatusURL == "" {
		t.Fatal("a node with a published port must still get a status URL; a target that serves " +
			"nothing there simply has those metrics absent")
	}
}

// A node with no published host port cannot be probed from a Windows host at
// all: container IPs are not routable here (D-010). It must still be sampled
// (its container metrics and process state are reachable through docker) and it
// must not acquire a probe URL that could never work.
func TestTargetsFromTopologyHandlesANodeWithNoPublishedPort(t *testing.T) {
	cfg := topologyConfig()
	top := &recorder.Topology{Nodes: []recorder.NodeBinding{
		{ID: "kv-n1", ContainerID: "c1", HostPort: 18081, ContainerPort: 8080},
		{ID: "kv-n2", ContainerID: "c2", HostPort: 0, ContainerPort: 8080}, // published nothing
	}}

	got, err := TargetsFromTopology(cfg, top)
	if err != nil {
		t.Fatalf("TargetsFromTopology: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("%d targets, want 2. A node that cannot be PROBED can still be INSPECTED and its "+
			"cgroup read, so dropping it would lose its memory, cpu and crash evidence too.",
			len(got))
	}
	if got[1].Node != "kv-n2" || got[1].Container != "c2" {
		t.Fatalf("target[1] = %+v, want the unpublished node still bound to its container", got[1])
	}
	if got[1].ProbeURL != "" {
		t.Fatalf("kv-n2 got probe URL %q despite publishing no host port. Expanding {port} to 0 "+
			"would produce a URL that fails every interval and would be indistinguishable from "+
			"the node being down.", got[1].ProbeURL)
	}
	if got[1].StatusURL != "" {
		t.Fatalf("kv-n2 got status URL %q despite publishing no host port", got[1].StatusURL)
	}
	if got[0].ProbeURL == "" {
		t.Fatal("the node that DID publish a port lost its probe; one unprobeable node must not " +
			"take the others down with it")
	}
}

func TestTargetsFromTopologyRejectsWhatItCannotSample(t *testing.T) {
	cfg := topologyConfig()
	top := &recorder.Topology{Nodes: []recorder.NodeBinding{
		{ID: "kv-n1", ContainerID: "c1", HostPort: 18081},
	}}

	if _, err := TargetsFromTopology(nil, top); err == nil {
		t.Fatal("a nil config was accepted")
	}
	if _, err := TargetsFromTopology(cfg, nil); err == nil {
		t.Fatal("a nil topology was accepted")
	}
	if _, err := TargetsFromTopology(cfg, &recorder.Topology{}); err == nil {
		t.Fatal("a topology binding no nodes produced a sampling set; it would yield an empty " +
			"telemetry document that reads like a healthy quiet run")
	}

	// A Phase 2 selector form must surface as an error rather than silently
	// resolving to no nodes, for the same reason WaitHealthy refuses it.
	bad := topologyConfig()
	bad.Harness.Health = []schema.HealthProbe{{Node: "role:leader", Probe: "http://{host}:{port}/healthz"}}
	if _, err := TargetsFromTopology(bad, top); err == nil {
		t.Fatal("a role selector was accepted; Phase 0 cannot resolve it and a probe that matches " +
			"nothing passes vacuously")
	}
}

// Two probes can match the same node. The first declared wins, and the second
// must not silently replace it: otherwise the address telemetry samples
// depends on map iteration order.
func TestTargetsFromTopologyFirstMatchingProbeWins(t *testing.T) {
	cfg := topologyConfig()
	cfg.Harness.Health = append(cfg.Harness.Health,
		schema.HealthProbe{Node: "kv-n1", Probe: "http://{host}:{port}/second"})
	top := &recorder.Topology{Nodes: []recorder.NodeBinding{
		{ID: "kv-n1", ContainerID: "c1", HostPort: 18081},
	}}

	for i := 0; i < 8; i++ {
		got, err := TargetsFromTopology(cfg, top)
		if err != nil {
			t.Fatalf("TargetsFromTopology: %v", err)
		}
		if got[0].ProbeURL != "http://127.0.0.1:18081/healthz" {
			t.Fatalf("probe = %q on iteration %d, want the first declared probe every time; an "+
				"address that varies between runs makes a probe failure unreproducible",
				got[0].ProbeURL, i)
		}
	}
}

// ---------------------------------------------------------------------------
// the engine seam: rows must be attributed to the right container
// ---------------------------------------------------------------------------

// `docker inspect a missing b` prints TWO rows, not three. Matching by position
// would attribute b's state to the wrong node, and a crash attributed to the
// wrong node is a false verdict with a name on it.
func TestParseInspectSelfIdentifiesRows(t *testing.T) {
	out := `9f3c1a2b4d5e 0 {"Status":"running","Running":true,"Paused":false,"Restarting":false,"OOMKilled":false,"Dead":false,"Pid":4242,"ExitCode":0,"Error":"","StartedAt":"2026-09-07T11:59:00.123456789Z","FinishedAt":"0001-01-01T00:00:00Z"}
7ab8c9d0e1f2 3 {"Status":"exited","Running":false,"Paused":false,"Restarting":false,"OOMKilled":true,"Dead":false,"Pid":0,"ExitCode":137,"Error":"","StartedAt":"2026-09-07T11:59:01.000000000Z","FinishedAt":"2026-09-07T12:00:03.500000000Z"}
`
	byID, err := parseInspect(out)
	if err != nil {
		t.Fatalf("parseInspect: %v", err)
	}
	if len(byID) != 2 {
		t.Fatalf("parsed %d rows, want 2", len(byID))
	}
	live := byID["9f3c1a2b4d5e"]
	if !live.Running || live.Paused || live.Pid != 4242 {
		t.Fatalf("running row parsed as %+v", live)
	}
	dead := byID["7ab8c9d0e1f2"]
	if dead.Running || !dead.OOMKilled || dead.ExitCode != 137 || dead.RestartCount != 3 {
		t.Fatalf("oom-killed row parsed as %+v; OOMKilled is the kernel's verdict and no_crash "+
			"reads it directly rather than inferring it from an exit code", dead)
	}

	// A short id from `compose ps` must find its full row, in either direction.
	if st, ok := matchContainer(byID, "9f3c1a2b"); !ok || st.Pid != 4242 {
		t.Fatal("a short container id did not match its full row; the node would be reported as " +
			"removed on every sample")
	}
	if _, ok := matchContainer(byID, "deadbeef"); ok {
		t.Fatal("an unrelated id matched a row; that attributes one container's state to another")
	}
	if _, ok := matchContainer(byID, ""); ok {
		t.Fatal("the empty container id matched a row")
	}

	if _, err := parseInspect("this is not an inspect row\n"); err == nil {
		t.Fatal("an unparseable inspect row was accepted; silently dropping it would look like " +
			"the container had been removed")
	}
	if m, err := parseInspect("  \n\n"); err != nil || len(m) != 0 {
		t.Fatalf("blank output: %v %v", m, err)
	}
}

// `docker stats --format json` is an unversioned contract: an array on some
// versions, one object per line on others. Both must parse or telemetry breaks
// on a Docker upgrade nobody controls.
func TestParseStatsHandlesBothShapes(t *testing.T) {
	row := `{"BlockIO":"0B / 0B","CPUPerc":"12.34%","Container":"c1","ID":"9f3c1a2b4d5e","MemPerc":"1.14%","MemUsage":"365.1MiB / 31.19GiB","Name":"thesis-kv-n1","NetIO":"1.2kB / 900B","PIDs":"37"}`

	arr, err := parseStats("[" + row + "]")
	if err != nil || len(arr) != 1 || arr[0].MemUsage != "365.1MiB / 31.19GiB" {
		t.Fatalf("array shape: %v %+v", err, arr)
	}
	lines, err := parseStats(row + "\n" + row + "\n")
	if err != nil || len(lines) != 2 {
		t.Fatalf("line shape: %v %+v", err, lines)
	}
	if rows, err := parseStats("  \n"); err != nil || len(rows) != 0 {
		t.Fatalf("empty output: %v %+v", err, rows)
	}
	if _, err := parseStats("not json\n"); err == nil {
		t.Fatal("unparseable stats output was accepted")
	}
}

func TestParseDockerTime(t *testing.T) {
	cases := []struct {
		name string
		in   string
		ok   bool
	}{
		{"rfc3339 nano", "2026-09-07T11:59:00.123456789Z", true},
		{"rfc3339 seconds", "2026-09-07T11:59:00Z", true},
		{"docker zero time means never", "0001-01-01T00:00:00Z", false},
		{"empty", "", false},
		{"garbage", "not a time", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseDockerTime(tc.in)
			if ok != tc.ok {
				t.Fatalf("parseDockerTime(%q) ok = %v, want %v. Docker's zero time means the "+
					"event NEVER HAPPENED; converting it would put finished_at 62 billion "+
					"seconds before the epoch and make any duration computed from it nonsense.",
					tc.in, ok, tc.ok)
			}
			if ok && got <= 0 {
				t.Fatalf("parseDockerTime(%q) = %d, want a positive Unix epoch nanosecond value",
					tc.in, got)
			}
		})
	}
}
