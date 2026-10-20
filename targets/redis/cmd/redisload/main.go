// Command redisload is the Synthesis workload driver for the Redis target.
//
// It is written the way a third party would write it (D-065): the Go standard
// library only, RESP2 over TCP by hand (internal/redisapi), and the history
// format of docs/TARGETS.md section 5 spelled out here rather than imported
// from the harness. Single-key register operations:
//
//	write  SET <key> <value>   to the primary Sentinel names
//	read   GET <key>           from the same believed primary
//
// # Where every parameter comes from
//
// The command line (driver.cmd, lock-covered) and the plan file named by
// --plan, which the harness writes from driver.profiles (lock-covered for every
// profile a run profile names, D-090). The node and Sentinel addresses are on
// the command line too: on this target the harness's PROTHESIS_TARGETS carries
// the health shims' ports, not RESP addresses, because a node declares one port
// and the health probe needs it (README, "Contract gaps"). So this driver reads
// NO environment variable at all. A missing plan, an empty mix, a zero client
// or op count and an unknown op are refused before the history file exists.
//
// # Start-up checks
//
// Before the history exists, every node and every Sentinel is asked INFO
// server, and each must report --expect-version. Every Sentinel is asked which
// primary it monitors, and its runid is mapped to a node through the run_id
// each node reports (internal/redisapi explains the mapping). Each Sentinel
// must name --expect-primary and report --expect-quorum,
// --expect-down-after and --expect-failover-timeout. Any mismatch ends the run
// non-zero with no history: a world whose topology is not the one the locked
// command line states is not evidence about that topology.
//
// The topology observed is recorded as meta.topology on the first record of
// the history, so every world's evidence carries the configured primary, the
// Sentinel placement, the quorum and the timings.
//
// # The soundness rules
//
//   - `+OK` to SET is ok. Nothing else is.
//   - `-READONLY ...` is fail: a replica refuses a write before applying
//     anything (replication.c / server.c processCommand, the shared
//     roslaveerr reply).
//   - A command that provably never left the client (connection refused, no
//     such host) is fail.
//   - Every other error, a malformed reply, a timeout or a closed connection
//     after the command was sent is info.
//   - GET's null bulk string ($-1) is the absent key, recorded as the value
//     null. A bulk string must parse as a decimal integer, the only values
//     this driver writes.
//   - Every record names the node the command was sent to (meta.target), the
//     primary the driver believed in (meta.primary) and the Sentinel that
//     belief came from (meta.sentinel).
//
// # Finding the primary
//
// As a Sentinel-aware client does: ask the Sentinels in order, starting from
// the last one that answered, and take the first answer that maps to a node.
// The driver asks again after any operation that was not ok, at most once per
// --rediscover interval. A Sentinel that cannot see the rest of the cluster
// keeps naming the primary it last knew, and so does this driver while that
// Sentinel answers first: that is the behaviour of the clients the target
// models, recorded as it is.
//
// # The drain
//
// With --stdin-control, {"cmd":"stop"} or EOF on stdin stops new operations;
// the ones in flight complete and are recorded (docs/TARGETS.md section 4).
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/WrdCstlg/pro-thesis/targets/redis/internal/redisapi"
)

const (
	exitOK     = 0
	exitRun    = 1
	exitConfig = 2

	planSchema = "prothesis.driver_plan/v1"

	mixWrite = "write"
	mixRead  = "read"

	typeInvoke = "invoke"
	typeOK     = "ok"
	typeFail   = "fail"
	typeInfo   = "info"
)

func main() {
	code, err := run(os.Args[1:], os.Stdin, os.Stderr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "redisload: %v\n", err)
	}
	os.Exit(code)
}

type options struct {
	historyPath     string
	seed            uint64
	profile         string
	planPath        string
	nodes           string
	sentinels       string
	masterName      string
	expectVersion   string
	expectPrimary   string
	expectQuorum    int
	expectDownAfter time.Duration
	expectFailover  time.Duration
	stdinControl    bool
	keys            int
	timeout         time.Duration
	setupTimeout    time.Duration
	rediscover      time.Duration
}

func run(args []string, stdin io.Reader, stderr io.Writer) (int, error) {
	var o options
	fs := flag.NewFlagSet("redisload", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&o.historyPath, "history", "", "history file to write (required)")
	fs.Uint64Var(&o.seed, "seed", 1, "world seed")
	fs.StringVar(&o.profile, "profile", "", "driver profile name; when given it must equal the plan's")
	fs.StringVar(&o.planPath, "plan", "", "driver plan to execute (required): the harness's {plan_path}")
	fs.StringVar(&o.nodes, "nodes", "", "node RESP addresses, id=host:port,... in harness.nodes order (required)")
	fs.StringVar(&o.sentinels, "sentinels", "", "Sentinel addresses, <co-located node id>=host:port,... (required)")
	fs.StringVar(&o.masterName, "master-name", "", "the name Sentinel monitors the primary under (required)")
	fs.StringVar(&o.expectVersion, "expect-version", "", "the redis_version every node and Sentinel must report (required)")
	fs.StringVar(&o.expectPrimary, "expect-primary", "", "the node every Sentinel must name as primary at start (required)")
	fs.IntVar(&o.expectQuorum, "expect-quorum", 0, "the quorum every Sentinel must report (required)")
	fs.DurationVar(&o.expectDownAfter, "expect-down-after", 0, "the down-after-milliseconds every Sentinel must report (required)")
	fs.DurationVar(&o.expectFailover, "expect-failover-timeout", 0, "the failover-timeout every Sentinel must report (required)")
	fs.BoolVar(&o.stdinControl, "stdin-control", false, `stop on stdin EOF or a {"cmd":"stop"} line`)
	fs.IntVar(&o.keys, "keys", 8, "key space: k/0 .. k/N-1")
	fs.DurationVar(&o.timeout, "timeout", 2*time.Second, "per-command timeout, dial included")
	fs.DurationVar(&o.setupTimeout, "setup-timeout", 30*time.Second, "how long to keep trying the start-up checks while a server does not answer")
	fs.DurationVar(&o.rediscover, "rediscover", 200*time.Millisecond, "the least time between two questions to the Sentinels")
	if err := fs.Parse(args); err != nil {
		return exitConfig, err
	}
	if fs.NArg() > 0 {
		return exitConfig, fmt.Errorf("unexpected arguments %q", fs.Args())
	}

	// Everything is checked before the history file exists: a start this
	// driver cannot honour must not leave a history a checker would call clean.
	switch {
	case o.historyPath == "":
		return exitConfig, errors.New("--history is required")
	case o.planPath == "":
		return exitConfig, errors.New("--plan is required: this driver takes its workload from the harness's plan and has no default")
	case strings.TrimSpace(o.expectVersion) == "":
		return exitConfig, errors.New("--expect-version is required")
	case o.masterName == "":
		return exitConfig, errors.New("--master-name is required")
	case o.expectPrimary == "":
		return exitConfig, errors.New("--expect-primary is required")
	case o.expectQuorum <= 0 || o.expectDownAfter <= 0 || o.expectFailover <= 0:
		return exitConfig, errors.New("--expect-quorum, --expect-down-after and --expect-failover-timeout are required")
	case o.keys <= 0:
		return exitConfig, errors.New("--keys must be positive")
	case o.timeout <= 0:
		return exitConfig, errors.New("--timeout must be positive")
	}
	pl, err := loadPlan(o.planPath)
	if err != nil {
		return exitConfig, fmt.Errorf("plan: %w", err)
	}
	if o.profile != "" && o.profile != pl.Profile {
		return exitConfig, fmt.Errorf("--profile %q does not match the plan's profile %q", o.profile, pl.Profile)
	}
	table, err := buildMix(pl.Mix)
	if err != nil {
		return exitConfig, fmt.Errorf("plan %s: %w", o.planPath, err)
	}
	nodes, err := redisapi.ParseTargets(o.nodes)
	if err != nil {
		return exitConfig, fmt.Errorf("--nodes: %w", err)
	}
	sentinels, err := redisapi.ParseTargets(o.sentinels)
	if err != nil {
		return exitConfig, fmt.Errorf("--sentinels: %w", err)
	}
	if indexOf(nodes, o.expectPrimary) < 0 {
		return exitConfig, fmt.Errorf("--expect-primary %q is not one of --nodes", o.expectPrimary)
	}

	g := &generator{
		o:         o,
		nodes:     nodes,
		sentinels: sentinels,
		mix:       table,
		clients:   pl.Clients,
		stop:      make(chan struct{}),
	}
	g.remaining.Store(int64(pl.Ops))

	fmt.Fprintf(stderr, "redisload: profile=%s seed=%d clients=%d ops=%d mix=%s nodes=%d sentinels=%d expect-version=%s history=%s\n",
		pl.Profile, o.seed, pl.Clients, pl.Ops, table.describe(), len(nodes), len(sentinels), o.expectVersion, o.historyPath)

	topo, err := g.startupChecks()
	if err != nil {
		return exitRun, fmt.Errorf("start-up check: %w", err)
	}
	fmt.Fprintf(stderr, "redisload: topology primary=%s quorum=%d down-after=%dms failover-timeout=%dms sentinels=%d\n",
		topo.ObservedPrimary, topo.Quorum, topo.DownAfterMS, topo.FailoverTimeoutMS, len(topo.Sentinels))

	f, err := os.Create(o.historyPath)
	if err != nil {
		return exitRun, fmt.Errorf("history: %w", err)
	}
	g.w = newHistWriter(f)
	g.topology = topo

	if o.stdinControl {
		go g.watchStdin(stdin)
	}
	g.runClients()

	if err := g.w.close(); err != nil {
		return exitRun, fmt.Errorf("history: %w", err)
	}
	fmt.Fprintf(stderr, "redisload: done ops=%d ok=%d fail=%d info=%d records=%d\n",
		g.issued.Load(), g.nOK.Load(), g.nFail.Load(), g.nInfo.Load(), g.w.count())
	return exitOK, nil
}

func indexOf(ts []redisapi.Target, id string) int {
	for i, t := range ts {
		if t.ID == id {
			return i
		}
	}
	return -1
}

// ---------------------------------------------------------------------------
// The plan (prothesis.driver_plan/v1)
// ---------------------------------------------------------------------------

type plan struct {
	Schema     string          `json:"schema"`
	Profile    string          `json:"profile"`
	Clients    int             `json:"clients"`
	Ops        int             `json:"ops"`
	Mix        []planMixEntry  `json:"mix"`
	Operations json.RawMessage `json:"operations"`
}

type planMixEntry struct {
	Op        string `json:"op"`
	WeightPPM int64  `json:"weight_ppm"`
}

func loadPlan(path string) (*plan, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var p plan
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if p.Schema != planSchema {
		return nil, fmt.Errorf("%s: schema %q, want %q", path, p.Schema, planSchema)
	}
	// An operation plan is a trace to execute verbatim (docs/TARGETS.md
	// section 4, rule 1). This driver does not execute traces, so it says so
	// rather than drive something else under the plan's name.
	if len(p.Operations) > 0 && string(p.Operations) != "null" {
		return nil, fmt.Errorf("%s: an operation-mode plan; this driver executes profile plans only", path)
	}
	if p.Clients <= 0 {
		return nil, fmt.Errorf("%s: clients is %d; the profile must set it", path, p.Clients)
	}
	if p.Ops <= 0 {
		return nil, fmt.Errorf("%s: ops is %d; the profile must set it", path, p.Ops)
	}
	return &p, nil
}

// ---------------------------------------------------------------------------
// The mix
// ---------------------------------------------------------------------------

type mixEntry struct {
	op     string
	weight int64
}

type mixTable struct {
	entries []mixEntry
	cum     []int64
	total   int64
}

func buildMix(raw []planMixEntry) (*mixTable, error) {
	t := &mixTable{}
	for _, m := range raw {
		if m.Op != mixWrite && m.Op != mixRead {
			return nil, fmt.Errorf("mix: unknown op %q; this driver knows %q and %q", m.Op, mixWrite, mixRead)
		}
		if m.WeightPPM < 0 {
			return nil, fmt.Errorf("mix: %s has negative weight %d", m.Op, m.WeightPPM)
		}
		if m.WeightPPM == 0 {
			continue
		}
		t.total += m.WeightPPM
		t.entries = append(t.entries, mixEntry{op: m.Op, weight: m.WeightPPM})
		t.cum = append(t.cum, t.total)
	}
	if t.total == 0 {
		return nil, errors.New("mix: no entry has a positive weight; there is no workload to drive")
	}
	return t, nil
}

func (t *mixTable) pick(r *rand.Rand) mixEntry {
	x := r.Int63n(t.total)
	for i, c := range t.cum {
		if x < c {
			return t.entries[i]
		}
	}
	return t.entries[len(t.entries)-1]
}

func (t *mixTable) describe() string {
	parts := make([]string, 0, len(t.entries))
	for _, e := range t.entries {
		parts = append(parts, fmt.Sprintf("%s=%d", e.op, e.weight))
	}
	return strings.Join(parts, ",")
}

// ---------------------------------------------------------------------------
// History records (docs/TARGETS.md section 5)
// ---------------------------------------------------------------------------

type record struct {
	TNS     int64           `json:"t_ns"`
	Process *int64          `json:"process,omitempty"`
	Type    string          `json:"type"`
	F       string          `json:"f"`
	Key     *string         `json:"key,omitempty"`
	Value   json.RawMessage `json:"value,omitempty"`
	OpID    *int64          `json:"op_id,omitempty"`
	Error   string          `json:"error,omitempty"`
	Meta    *meta           `json:"meta,omitempty"`
}

// meta is this driver's namespaced evidence. target is the node the command
// was sent to, primary the node the driver believed was primary when it sent
// it, sentinel the node whose co-located Sentinel gave that belief.
type meta struct {
	Target    string    `json:"target"`
	Primary   string    `json:"primary"`
	Sentinel  string    `json:"sentinel,omitempty"`
	Reply     string    `json:"reply,omitempty"`
	LatencyUS int64     `json:"latency_us,omitempty"`
	Topology  *topology `json:"topology,omitempty"`
}

// topology is what the start-up checks observed, recorded once, on the first
// record of the history. Every value was read from a server, except
// configured_primary (--expect-primary) and each Sentinel's node, which is the
// label --sentinels gives it: the node whose network namespace it shares in
// docker-compose.yaml.
type topology struct {
	MasterName        string         `json:"master_name"`
	ConfiguredPrimary string         `json:"configured_primary"`
	ObservedPrimary   string         `json:"observed_primary"`
	Quorum            int            `json:"quorum"`
	DownAfterMS       int64          `json:"down_after_ms"`
	FailoverTimeoutMS int64          `json:"failover_timeout_ms"`
	Placement         string         `json:"sentinel_placement"`
	Nodes             []nodeTopo     `json:"nodes"`
	Sentinels         []sentinelTopo `json:"sentinels"`
}

type nodeTopo struct {
	ID      string `json:"id"`
	Addr    string `json:"addr"`
	Role    string `json:"role"`
	RunID   string `json:"run_id"`
	Version string `json:"version"`
}

type sentinelTopo struct {
	Node              string `json:"node"`
	Addr              string `json:"addr"`
	Version           string `json:"version"`
	Primary           string `json:"primary"`
	PrimaryAddr       string `json:"primary_addr"`
	ConfigEpoch       string `json:"config_epoch"`
	Quorum            string `json:"quorum"`
	DownAfterMS       string `json:"down_after_ms"`
	FailoverTimeoutMS string `json:"failover_timeout_ms"`
	ParallelSyncs     string `json:"parallel_syncs"`
	NumReplicas       string `json:"num_replicas"`
	NumOtherSentinels string `json:"num_other_sentinels"`
}

type histWriter struct {
	mu  sync.Mutex
	f   *os.File
	bw  *bufio.Writer
	enc *json.Encoder
	n   int64
	err error
}

func newHistWriter(f *os.File) *histWriter {
	bw := bufio.NewWriterSize(f, 1<<16)
	enc := json.NewEncoder(bw)
	enc.SetEscapeHTML(false)
	return &histWriter{f: f, bw: bw, enc: enc}
}

// write flushes after every record: a driver the harness has to kill must not
// lose the completions it already has.
func (h *histWriter) write(r *record) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.err != nil {
		return
	}
	if err := h.enc.Encode(r); err != nil {
		h.err = err
		return
	}
	h.n++
	if err := h.bw.Flush(); err != nil {
		h.err = err
	}
}

func (h *histWriter) count() int64 { h.mu.Lock(); defer h.mu.Unlock(); return h.n }

func (h *histWriter) close() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := h.bw.Flush(); err != nil && h.err == nil {
		h.err = err
	}
	if err := h.f.Close(); err != nil && h.err == nil {
		h.err = err
	}
	return h.err
}

// ---------------------------------------------------------------------------
// Start-up checks
// ---------------------------------------------------------------------------

// startupChecks asks every node and every Sentinel, retrying while a server
// does not answer (up to --setup-timeout), and refuses at once on an answer
// that contradicts the command line.
func (g *generator) startupChecks() (*topology, error) {
	deadline := time.Now().Add(g.o.setupTimeout)
	for {
		topo, transient, err := g.observe()
		if err == nil {
			return topo, nil
		}
		if !transient || time.Now().After(deadline) {
			return nil, err
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// observe returns the topology, or an error and whether it is worth retrying
// (a server that did not answer) or final (an answer that contradicts the
// command line).
func (g *generator) observe() (*topology, bool, error) {
	o := g.o
	topo := &topology{
		MasterName:        o.masterName,
		ConfiguredPrimary: o.expectPrimary,
		Quorum:            o.expectQuorum,
		DownAfterMS:       o.expectDownAfter.Milliseconds(),
		FailoverTimeoutMS: o.expectFailover.Milliseconds(),
		Placement:         "one Sentinel per node, in that node's network namespace (docker-compose.yaml network_mode service:<node>)",
	}
	infos := make([]redisapi.NodeInfo, 0, len(g.nodes))
	for _, n := range g.nodes {
		ni := redisapi.AskNode(n, o.timeout)
		if ni.Err != nil {
			return nil, true, fmt.Errorf("node %s (%s): %v", n.ID, n.Addr, ni.Err)
		}
		if ni.Version != o.expectVersion {
			return nil, false, fmt.Errorf("version mismatch: node %s reports redis_version %s, --expect-version is %s",
				n.ID, ni.Version, o.expectVersion)
		}
		infos = append(infos, ni)
		topo.Nodes = append(topo.Nodes, nodeTopo{ID: n.ID, Addr: n.Addr, Role: ni.Role, RunID: ni.RunID, Version: ni.Version})
	}
	byRunID, err := redisapi.RunIDTable(infos)
	if err != nil {
		return nil, false, err
	}
	for _, s := range g.sentinels {
		st, transient, err := askSentinelTopo(s, o.masterName, o.timeout, byRunID)
		if err != nil {
			return nil, transient, fmt.Errorf("Sentinel at %s (%s): %v", s.ID, s.Addr, err)
		}
		if st.Version != o.expectVersion {
			return nil, false, fmt.Errorf("version mismatch: the Sentinel at %s reports redis_version %s, --expect-version is %s",
				s.ID, st.Version, o.expectVersion)
		}
		for _, c := range []struct{ name, got, want string }{
			{"primary", st.Primary, o.expectPrimary},
			{"quorum", st.Quorum, strconv.Itoa(o.expectQuorum)},
			{"down-after-milliseconds", st.DownAfterMS, strconv.FormatInt(o.expectDownAfter.Milliseconds(), 10)},
			{"failover-timeout", st.FailoverTimeoutMS, strconv.FormatInt(o.expectFailover.Milliseconds(), 10)},
		} {
			if c.got != c.want {
				return nil, false, fmt.Errorf("topology mismatch: the Sentinel at %s reports %s %s, the command line states %s",
					s.ID, c.name, c.got, c.want)
			}
		}
		topo.Sentinels = append(topo.Sentinels, st)
	}
	topo.ObservedPrimary = o.expectPrimary
	g.primary.Store(int64(indexOf(g.nodes, o.expectPrimary)))
	g.believedFrom.Store(int64(0))
	g.runIDs.Store(&byRunID)
	return topo, false, nil
}

func askSentinelTopo(s redisapi.Target, name string, timeout time.Duration, byRunID map[string]string) (sentinelTopo, bool, error) {
	st := sentinelTopo{Node: s.ID, Addr: s.Addr}
	c, err := redisapi.Dial(s.Addr, timeout)
	if err != nil {
		return st, true, err
	}
	defer c.Close()
	srv, err := redisapi.Info(c, "server")
	if err != nil {
		return st, true, err
	}
	st.Version = srv["redis_version"]
	m, err := redisapi.AskSentinel(c, name)
	if err != nil {
		return st, true, err
	}
	primary, err := redisapi.NodeForRunID(m.RunID, byRunID)
	if err != nil {
		// An empty runid is Sentinel not having read the primary's INFO yet:
		// worth waiting for. An unknown one is final.
		return st, m.RunID == "", fmt.Errorf("primary %s: %w", m.Addr, err)
	}
	st.Primary, st.PrimaryAddr = primary, m.Addr
	st.ConfigEpoch = m.Fields["config-epoch"]
	st.Quorum = m.Fields["quorum"]
	st.DownAfterMS = m.Fields["down-after-milliseconds"]
	st.FailoverTimeoutMS = m.Fields["failover-timeout"]
	st.ParallelSyncs = m.Fields["parallel-syncs"]
	st.NumReplicas = m.Fields["num-slaves"]
	st.NumOtherSentinels = m.Fields["num-other-sentinels"]
	return st, false, nil
}

// ---------------------------------------------------------------------------
// The generator
// ---------------------------------------------------------------------------

type generator struct {
	o         options
	nodes     []redisapi.Target
	sentinels []redisapi.Target
	w         *histWriter
	mix       *mixTable
	clients   int
	topology  *topology
	topoOnce  sync.Once

	primary      atomic.Int64 // index into nodes of the believed primary
	believedFrom atomic.Int64 // index into sentinels of the Sentinel that said so
	runIDs       atomic.Pointer[map[string]string]

	discoverMu sync.Mutex
	lastAsked  time.Time

	stop      chan struct{}
	stopOnce  sync.Once
	remaining atomic.Int64
	nextOpID  atomic.Int64
	issued    atomic.Int64
	nOK       atomic.Int64
	nFail     atomic.Int64
	nInfo     atomic.Int64
}

func (g *generator) requestStop() { g.stopOnce.Do(func() { close(g.stop) }) }

func (g *generator) stopped() bool {
	select {
	case <-g.stop:
		return true
	default:
		return false
	}
}

func (g *generator) watchStdin(r io.Reader) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		var msg struct {
			Cmd string `json:"cmd"`
		}
		if err := json.Unmarshal(sc.Bytes(), &msg); err == nil && msg.Cmd == "stop" {
			break
		}
	}
	g.requestStop()
}

// rediscover asks the Sentinels again, at most once per --rediscover, starting
// from the one that answered last; the first answer that maps to a node wins.
func (g *generator) rediscover() {
	g.discoverMu.Lock()
	defer g.discoverMu.Unlock()
	if time.Since(g.lastAsked) < g.o.rediscover {
		return
	}
	g.lastAsked = time.Now()
	start := int(g.believedFrom.Load())
	for k := 0; k < len(g.sentinels); k++ {
		i := (start + k) % len(g.sentinels)
		idx, err := g.askOne(g.sentinels[i])
		if err != nil {
			continue
		}
		g.primary.Store(int64(idx))
		g.believedFrom.Store(int64(i))
		return
	}
}

func (g *generator) askOne(s redisapi.Target) (int, error) {
	c, err := redisapi.Dial(s.Addr, g.o.timeout)
	if err != nil {
		return -1, err
	}
	defer c.Close()
	m, err := redisapi.AskSentinel(c, g.o.masterName)
	if err != nil {
		return -1, err
	}
	table := *g.runIDs.Load()
	id, err := redisapi.NodeForRunID(m.RunID, table)
	if err != nil && m.RunID != "" {
		// A node restarted and drew a new run_id: ask the nodes once more.
		var infos []redisapi.NodeInfo
		for _, n := range g.nodes {
			infos = append(infos, redisapi.AskNode(n, g.o.timeout))
		}
		if fresh, terr := redisapi.RunIDTable(infos); terr == nil {
			g.runIDs.Store(&fresh)
			id, err = redisapi.NodeForRunID(m.RunID, fresh)
		}
	}
	if err != nil {
		return -1, err
	}
	return indexOf(g.nodes, id), nil
}

// runClients seeds every key once from process 0 (value = the key's index,
// below every client value), then runs the clients until the op budget is
// spent or a drain is requested.
func (g *generator) runClients() {
	seed := newClient(g)
	for k := 0; k < g.o.keys; k++ {
		if g.stopped() || g.remaining.Add(-1) < 0 {
			break
		}
		seed.doWrite(0, g.nextOpID.Add(1), key(k), int64(k))
	}
	seed.close()
	var wg sync.WaitGroup
	for p := 0; p < g.clients; p++ {
		wg.Add(1)
		go func(process int) {
			defer wg.Done()
			c := newClient(g)
			defer c.close()
			c.loop(process)
		}(p)
	}
	wg.Wait()
}

func key(i int) string { return "k/" + strconv.Itoa(i) }

// client is one process: its own connection per node, redialled after any
// error, because a connection that saw a timeout or a malformed reply is out
// of step with the server.
type client struct {
	g     *generator
	conns map[int]*redisapi.Conn
}

func newClient(g *generator) *client { return &client{g: g, conns: map[int]*redisapi.Conn{}} }

func (c *client) close() {
	for _, cn := range c.conns {
		_ = cn.Close()
	}
}

func (c *client) loop(process int) {
	g := c.g
	r := rand.New(rand.NewSource(int64(g.o.seed)*1000003 + int64(process)*7919 + 1))
	seq := int64(0)
	for {
		if g.stopped() || g.remaining.Add(-1) < 0 {
			return
		}
		e := g.mix.pick(r)
		k := key(r.Intn(g.o.keys))
		id := g.nextOpID.Add(1)
		if e.op == mixWrite {
			seq++
			// Unique per process for seq < 1e6, and above every seed value:
			// 3000017 is process 2's 17th write.
			c.doWrite(process, id, k, int64(process+1)*1000000+seq)
			continue
		}
		c.doRead(process, id, k)
	}
}

// send runs one command against node idx and reports what was observed.
func (c *client) send(idx int, args ...string) attempt {
	cn := c.conns[idx]
	if cn == nil {
		var err error
		cn, err = redisapi.Dial(c.g.nodes[idx].Addr, c.g.o.timeout)
		if err != nil {
			return attempt{err: err}
		}
		c.conns[idx] = cn
	}
	if err := cn.Send(args...); err != nil {
		c.drop(idx)
		// Some bytes may have left: never provably unsent.
		return attempt{sent: true, err: err}
	}
	rep, err := cn.Receive()
	if err != nil {
		c.drop(idx)
		return attempt{sent: true, err: err}
	}
	return attempt{sent: true, reply: rep}
}

func (c *client) drop(idx int) {
	if cn := c.conns[idx]; cn != nil {
		_ = cn.Close()
		delete(c.conns, idx)
	}
}

// invokeMeta is the meta of an invoke; the history's first record also
// carries the topology.
func (g *generator) invokeMeta(target, primary, sentinel string) *meta {
	m := &meta{Target: target, Primary: primary, Sentinel: sentinel}
	g.topoOnce.Do(func() { m.Topology = g.topology })
	return m
}

func (c *client) belief() (int, string) {
	g := c.g
	idx := int(g.primary.Load())
	return idx, g.sentinels[int(g.believedFrom.Load())].ID
}

func (c *client) doWrite(process int, opID int64, k string, v int64) {
	g := c.g
	g.issued.Add(1)
	p64 := int64(process)
	val := json.RawMessage(strconv.FormatInt(v, 10))
	idx, from := c.belief()
	node := g.nodes[idx].ID
	g.w.write(&record{TNS: time.Now().UnixNano(), Process: &p64, Type: typeInvoke, F: mixWrite,
		Key: &k, Value: val, OpID: &opID, Meta: g.invokeMeta(node, node, from)})

	start := time.Now()
	att := c.send(idx, "SET", k, string(val))
	out := classifyWrite(att)
	m := &meta{Target: node, Primary: node, Sentinel: from, Reply: att.describe(), LatencyUS: time.Since(start).Microseconds()}
	comp := record{TNS: time.Now().UnixNano(), Process: &p64, Type: out.typ, F: mixWrite,
		Key: &k, OpID: &opID, Error: out.label, Meta: m}
	if out.typ == typeOK {
		comp.Value = val
	}
	g.count(out.typ)
	g.w.write(&comp)
	if out.typ != typeOK {
		g.rediscover()
	}
}

func (c *client) doRead(process int, opID int64, k string) {
	g := c.g
	g.issued.Add(1)
	p64 := int64(process)
	idx, from := c.belief()
	node := g.nodes[idx].ID
	g.w.write(&record{TNS: time.Now().UnixNano(), Process: &p64, Type: typeInvoke, F: mixRead,
		Key: &k, OpID: &opID, Meta: g.invokeMeta(node, node, from)})

	start := time.Now()
	att := c.send(idx, "GET", k)
	out := classifyRead(att)
	m := &meta{Target: node, Primary: node, Sentinel: from, Reply: att.describe(), LatencyUS: time.Since(start).Microseconds()}
	comp := record{TNS: time.Now().UnixNano(), Process: &p64, Type: out.typ, F: mixRead,
		Key: &k, OpID: &opID, Error: out.label, Meta: m}
	if out.typ == typeOK {
		comp.Value = out.value
	}
	g.count(out.typ)
	g.w.write(&comp)
	if out.typ != typeOK {
		g.rediscover()
	}
}

func (g *generator) count(typ string) {
	switch typ {
	case typeOK:
		g.nOK.Add(1)
	case typeFail:
		g.nFail.Add(1)
	default:
		g.nInfo.Add(1)
	}
}

// ---------------------------------------------------------------------------
// Classification: fail needs proof
// ---------------------------------------------------------------------------

// attempt is everything the client observed about one command, explicit so
// the classifiers are pure functions a table test can drive.
type attempt struct {
	sent  bool // the command was handed to the connection (some bytes may have left)
	err   error
	reply redisapi.Reply
}

func (a attempt) describe() string {
	if a.err != nil {
		return ""
	}
	return a.reply.String()
}

type outcome struct {
	typ   string
	label string
	value json.RawMessage // a read's value, when typ is ok
}

// transport classifies an attempt that produced no reply. done is false when
// there is a reply to read.
func transport(a attempt) (outcome, bool) {
	if a.err == nil {
		return outcome{}, false
	}
	if !a.sent && provablyNeverSent(a.err) {
		return outcome{typ: typeFail, label: errorLabel(a.err)}, true
	}
	return outcome{typ: typeInfo, label: errorLabel(a.err)}, true
}

// classifyWrite is the SET table:
//
//	never sent (refused, no such host)       -> fail
//	any other transport or protocol error    -> info
//	+OK                                      -> ok
//	-READONLY ...                            -> fail (a replica refused it first)
//	any other error reply                    -> info
//	any other reply                          -> info
func classifyWrite(a attempt) outcome {
	if out, done := transport(a); done {
		return out
	}
	r := a.reply
	switch {
	case r.Kind == redisapi.SimpleString && r.Str == "OK":
		return outcome{typ: typeOK}
	case r.Kind == redisapi.Error && (r.Str == "READONLY" || strings.HasPrefix(r.Str, "READONLY ")):
		return outcome{typ: typeFail, label: clip(r.Str)}
	case r.Kind == redisapi.Error:
		return outcome{typ: typeInfo, label: clip(r.Str)}
	}
	return outcome{typ: typeInfo, label: "unexpected reply to SET: " + r.String()}
}

// classifyRead is the GET table. A read changes nothing, so no read outcome
// can hide a write; it is ok only for the null bulk string (the absent key,
// value null) or a bulk string holding a decimal integer.
func classifyRead(a attempt) outcome {
	if out, done := transport(a); done {
		return out
	}
	r := a.reply
	switch {
	case r.Kind == redisapi.BulkString && r.Nil:
		return outcome{typ: typeOK, value: json.RawMessage("null")}
	case r.Kind == redisapi.BulkString:
		if _, err := strconv.ParseInt(r.Str, 10, 64); err != nil {
			return outcome{typ: typeInfo, label: "the value is not an integer: " + clip(strconv.Quote(r.Str))}
		}
		return outcome{typ: typeOK, value: json.RawMessage(r.Str)}
	case r.Kind == redisapi.Error:
		return outcome{typ: typeInfo, label: clip(r.Str)}
	}
	return outcome{typ: typeInfo, label: "unexpected reply to GET: " + r.String()}
}

func provablyNeverSent(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) {
		return false
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		if opErr.Timeout() {
			return false
		}
		return opErr.Op == "dial"
	}
	return false
}

func errorLabel(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, os.ErrDeadlineExceeded):
		return "timeout"
	case errors.Is(err, io.EOF):
		return "connection closed with no reply"
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "timeout"
	}
	return clip(err.Error())
}

func clip(s string) string {
	if len(s) > 200 {
		return s[:200]
	}
	return s
}
