// Command consulload is the Synthesis workload driver for the Consul target.
//
// It is written the way a third party would write it (D-065): the Go standard
// library only, Consul's plain HTTP KV API, and the history format of
// docs/TARGETS.md section 5 spelled out here rather than imported from the
// harness. Single-key register operations:
//
//	write  PUT /v1/kv/<key>            body: the value, decimal
//	read   GET /v1/kv/<key>[?<mode>]   mode: none, ?stale or ?consistent
//
// # Where every parameter comes from
//
// The command line (driver.cmd, lock-covered) and the plan file named by
// --plan, which the harness writes from driver.profiles (lock-covered for every
// profile a run profile names, D-090). The read mode is part of the mix
// entry's name: read_default, read_stale or read_consistent. No workload
// parameter is read from the environment and nothing falls back to a built-in
// default: a missing plan, an empty mix, a zero client or op count and an
// unknown read mode are refused before the history file exists. The one
// environment variable read is the topology the harness exports
// (PROTHESIS_TARGETS), and only when --targets is not given.
//
// # The soundness rules
//
//   - A completion is `fail` only with positive evidence that the operation
//     did not take effect: the request was never sent (connection refused, no
//     such host), or a write was answered "No cluster leader". Consul v2.0.4
//     produces that error only in forwardRequestToLeader, before the KVS.Apply
//     handler body runs (agent/consul/rpc.go:795-799 and getLeader 855-858;
//     the text is agent/structs/errors.go:12), and a forwarding hop retries
//     only that and two other errors returned before apply (rpc.go:817-824),
//     so whichever hop it came from applied nothing.
//   - Everything else that is not a fully parsed success is `info`: a timeout,
//     a reset after the request was written, any other status (including a
//     500 "raft apply failed: leadership lost while committing log", whose
//     entry may still commit), and an HTTP 200 whose body is not `true`.
//   - Writes go to ANY node, like reads. A follower forwards a KV write to the
//     leader over RPC (agent/consul/kvs_endpoint.go:103-106, rpc.go
//     ForwardRPC 672-681). Unlike rqlite's forwarding, Consul's cannot apply
//     a write twice: the forwarding loop re-sends a write only on "No cluster
//     leader", ErrChunkingResubmit (values over raft.SuggestedMaxDataSize
//     only; ours are a few bytes) and rate.ErrRetryLater, all returned before
//     raftApply (rpc.go canRetry 633-664, forwardRequestToLeader 788-839); a
//     stream error is retried for reads only (rpc.go:663); the connection
//     pool makes one call (agent/pool/pool.go:627-666); and neither
//     Agent.RPC nor Server.RPC retries (agent/agent.go:1706-1727,
//     agent/consul/server.go:1634-1658). So there is no reason to route
//     writes to a believed leader, and a client of any agent is what the
//     target's users run. Go's net/http re-sends a PUT only when nothing was
//     written (it is not a replayable method).
//   - The server version is read once per node at start from
//     /v1/agent/self (Config.Version, agent/agent_endpoint.go:117-145) and
//     must equal --expect-version on every node; otherwise the run ends
//     non-zero before the history exists. Consul sends no version header, so
//     a node replaced mid-run is not detected here.
//   - The read mode sent is recorded in every read record's meta.read_mode,
//     with the X-Consul-KnownLeader, X-Consul-LastContact and
//     X-Consul-Effective-Consistency headers of the answer.
//
// # The drain
//
// With --stdin-control, {"cmd":"stop"} or EOF on stdin stops new operations;
// the ones in flight complete and are recorded (docs/TARGETS.md section 4).
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/http/httptrace"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	exitOK     = 0
	exitRun    = 1
	exitConfig = 2

	envTargets = "PROTHESIS_TARGETS"
	planSchema = "prothesis.driver_plan/v1"

	indexHeader       = "X-Consul-Index"
	knownLeaderHeader = "X-Consul-KnownLeader"
	lastContactHeader = "X-Consul-LastContact"
	consistencyHeader = "X-Consul-Effective-Consistency"

	// noLeader is structs.ErrNoLeader's text (agent/structs/errors.go:12).
	noLeader = "No cluster leader"

	mixWrite   = "write"
	readPrefix = "read_"

	typeInvoke = "invoke"
	typeOK     = "ok"
	typeFail   = "fail"
	typeInfo   = "info"
)

// readModes are the modes this driver will send, each as Consul's HTTP layer
// reads it (agent/http.go parseConsistency, 986-1041): `default` sends no
// parameter, the others their name. Consul ignores a parameter it does not
// know and serves the default mode without saying so, so anything else is
// refused at start-up.
var readModes = map[string]string{"default": "", "stale": "?stale", "consistent": "?consistent"}

var readModeNames = []string{"default", "stale", "consistent"}

func main() {
	code, err := run(os.Args[1:], os.Stdin, os.Stderr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "consulload: %v\n", err)
	}
	os.Exit(code)
}

type options struct {
	historyPath   string
	seed          uint64
	profile       string
	planPath      string
	targets       string
	expectVersion string
	stdinControl  bool
	keys          int
	timeout       time.Duration
	setupTimeout  time.Duration
}

func run(args []string, stdin io.Reader, stderr io.Writer) (int, error) {
	var o options
	fs := flag.NewFlagSet("consulload", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&o.historyPath, "history", "", "history file to write (required)")
	fs.Uint64Var(&o.seed, "seed", 1, "world seed")
	fs.StringVar(&o.profile, "profile", "", "driver profile name; when given it must equal the plan's")
	fs.StringVar(&o.planPath, "plan", "", "driver plan to execute (required): the harness's {plan_path}")
	fs.StringVar(&o.targets, "targets", "", "comma-separated [label=]host:port list; default: $"+envTargets+", labelled consul-n1.. by position")
	fs.StringVar(&o.expectVersion, "expect-version", "", "the Config.Version every node's /v1/agent/self must report (required)")
	fs.BoolVar(&o.stdinControl, "stdin-control", false, `stop on stdin EOF or a {"cmd":"stop"} line`)
	fs.IntVar(&o.keys, "keys", 8, "key space: k/0 .. k/N-1")
	fs.DurationVar(&o.timeout, "timeout", 2*time.Second, "per-request client timeout")
	fs.DurationVar(&o.setupTimeout, "setup-timeout", 30*time.Second, "how long to keep asking every node for its version")
	if err := fs.Parse(args); err != nil {
		return exitConfig, err
	}
	if fs.NArg() > 0 {
		return exitConfig, fmt.Errorf("unexpected arguments %q", fs.Args())
	}

	// Everything is checked before the history file exists: a start this
	// driver cannot honour must not leave a history a checker would call clean.
	if o.historyPath == "" {
		return exitConfig, errors.New("--history is required")
	}
	if o.planPath == "" {
		return exitConfig, errors.New("--plan is required: this driver takes its workload from the harness's plan and has no default")
	}
	if strings.TrimSpace(o.expectVersion) == "" {
		return exitConfig, errors.New("--expect-version is required")
	}
	if o.keys <= 0 {
		return exitConfig, errors.New("--keys must be positive")
	}
	if o.timeout <= 0 {
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
	ts := o.targets
	if ts == "" {
		ts = os.Getenv(envTargets)
	}
	targets, err := parseTargets(ts)
	if err != nil {
		return exitConfig, err
	}

	g := &generator{
		o:       o,
		targets: targets,
		http:    newHTTPClient(),
		mix:     table,
		clients: pl.Clients,
		stop:    make(chan struct{}),
	}
	g.remaining.Store(int64(pl.Ops))

	fmt.Fprintf(stderr, "consulload: profile=%s seed=%d clients=%d ops=%d mix=%s targets=%d expect-version=%s history=%s\n",
		pl.Profile, o.seed, pl.Clients, pl.Ops, table.describe(), len(targets), o.expectVersion, o.historyPath)

	if err := g.checkVersions(); err != nil {
		return exitRun, fmt.Errorf("setup: %w", err)
	}

	f, err := os.Create(o.historyPath)
	if err != nil {
		return exitRun, fmt.Errorf("history: %w", err)
	}
	g.w = newHistWriter(f)

	if o.stdinControl {
		go g.watchStdin(stdin)
	}
	g.runClients()

	if err := g.w.close(); err != nil {
		return exitRun, fmt.Errorf("history: %w", err)
	}
	fmt.Fprintf(stderr, "consulload: done ops=%d ok=%d fail=%d info=%d records=%d\n",
		g.issued.Load(), g.nOK.Load(), g.nFail.Load(), g.nInfo.Load(), g.w.count())
	return exitOK, nil
}

// ---------------------------------------------------------------------------
// Targets
// ---------------------------------------------------------------------------

// target is one node's HTTP API. Its label is what meta.target carries: the
// harness node id, consul-n<i+1> by position in PROTHESIS_TARGETS
// (prothesis.yaml's node order), or the explicit label=address form.
type target struct {
	label string
	base  string
}

func parseTargets(s string) ([]target, error) {
	var out []target
	for _, raw := range strings.Split(s, ",") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		label := ""
		if i := strings.Index(raw, "="); i > 0 && !strings.Contains(raw[:i], "/") {
			label, raw = raw[:i], raw[i+1:]
		}
		if !strings.HasPrefix(raw, "http://") && !strings.HasPrefix(raw, "https://") {
			raw = "http://" + raw
		}
		raw = strings.TrimRight(raw, "/")
		if label == "" {
			label = "consul-n" + strconv.Itoa(len(out)+1)
		}
		out = append(out, target{label: label, base: raw})
	}
	if len(out) == 0 {
		return nil, errors.New("no targets: pass --targets or run under the harness, which exports " + envTargets)
	}
	return out, nil
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
	op     string // mixWrite or "read"
	mode   string // the read mode, for a read
	name   string // as the profile spells it
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
		e := mixEntry{name: m.Op, weight: m.WeightPPM}
		switch {
		case m.Op == mixWrite:
			e.op = mixWrite
		case strings.HasPrefix(m.Op, readPrefix):
			mode := strings.TrimPrefix(m.Op, readPrefix)
			if _, ok := readModes[mode]; !ok {
				return nil, fmt.Errorf("mix: %q names read mode %q; this driver sends only %v, because Consul serves the default mode for a parameter it does not know without saying so",
					m.Op, mode, readModeNames)
			}
			e.op, e.mode = "read", mode
		default:
			return nil, fmt.Errorf("mix: unknown op %q; this driver knows %q and %s<mode> with mode one of %v",
				m.Op, mixWrite, readPrefix, readModeNames)
		}
		if e.weight < 0 {
			return nil, fmt.Errorf("mix: %s has negative weight %d", m.Op, e.weight)
		}
		if e.weight == 0 {
			continue
		}
		t.total += e.weight
		t.entries = append(t.entries, e)
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
		parts = append(parts, fmt.Sprintf("%s=%d", e.name, e.weight))
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

// meta is this driver's namespaced evidence. read_mode is the mode sent, on
// every read record; the same key the fixture, etcd and rqlite drivers use.
// The header values are copied as Consul sent them.
type meta struct {
	Target      string `json:"target,omitempty"`
	ReadMode    string `json:"read_mode,omitempty"`
	Status      int    `json:"status,omitempty"`
	KnownLeader string `json:"known_leader,omitempty"`
	LastContact string `json:"last_contact,omitempty"`
	Consistency string `json:"consistency,omitempty"`
	Index       string `json:"index,omitempty"`
	LatencyUS   int64  `json:"latency_us,omitempty"`
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
// The generator
// ---------------------------------------------------------------------------

type generator struct {
	o       options
	targets []target
	w       *histWriter
	http    *http.Client
	mix     *mixTable
	clients int

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

// checkVersions asks every node GET /v1/agent/self, retrying a node that does
// not answer until --setup-timeout, and refuses the run unless every node
// reports Config.Version == --expect-version. It is not an operation and is
// not recorded in the history.
func (g *generator) checkVersions() error {
	deadline := time.Now().Add(g.o.setupTimeout)
	for _, t := range g.targets {
		for {
			ver, err := agentVersion(g.http, t.base, g.o.timeout)
			if err == nil {
				if ver != g.o.expectVersion {
					return fmt.Errorf("version mismatch: %s reports Consul %s, --expect-version is %s", t.label, ver, g.o.expectVersion)
				}
				break
			}
			if errors.Is(err, errNoVersion) {
				return fmt.Errorf("%s: %w; --expect-version is %s", t.label, err, g.o.expectVersion)
			}
			if time.Now().After(deadline) || g.stopped() {
				return fmt.Errorf("could not read %s's version within %s: %v", t.label, g.o.setupTimeout, err)
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	return nil
}

var errNoVersion = errors.New("/v1/agent/self carries no Config.Version")

func agentVersion(client *http.Client, base string, timeout time.Duration) (string, error) {
	a := send(client, http.MethodGet, base+"/v1/agent/self", nil, timeout)
	if a.err != nil {
		return "", a.err
	}
	if a.status != http.StatusOK {
		return "", errors.New(statusLabel(a.status, a.body))
	}
	var self struct {
		Config struct {
			Version *string
		}
	}
	if err := json.Unmarshal(a.body, &self); err != nil {
		return "", fmt.Errorf("unparseable /v1/agent/self body: %w", err)
	}
	if self.Config.Version == nil || *self.Config.Version == "" {
		return "", errNoVersion
	}
	return *self.Config.Version, nil
}

// runClients seeds every key once from process 0 (value = the key's index,
// below every client value), then runs the clients until the op budget is
// spent or a drain is requested.
func (g *generator) runClients() {
	for k := 0; k < g.o.keys; k++ {
		if g.stopped() || g.remaining.Add(-1) < 0 {
			break
		}
		g.doWrite(0, g.nextOpID.Add(1), g.targets[k%len(g.targets)], key(k), int64(k))
	}
	var wg sync.WaitGroup
	for p := 0; p < g.clients; p++ {
		wg.Add(1)
		go func(process int) {
			defer wg.Done()
			g.clientLoop(process)
		}(p)
	}
	wg.Wait()
}

func key(i int) string { return "k/" + strconv.Itoa(i) }

func (g *generator) clientLoop(process int) {
	r := rand.New(rand.NewSource(int64(g.o.seed)*1000003 + int64(process)*7919 + 1))
	seq := int64(0)
	for {
		if g.stopped() || g.remaining.Add(-1) < 0 {
			return
		}
		e := g.mix.pick(r)
		k := key(r.Intn(g.o.keys))
		t := g.targets[r.Intn(len(g.targets))]
		id := g.nextOpID.Add(1)
		if e.op == mixWrite {
			seq++
			// Unique per process for seq < 1e6, and above every seed value:
			// 3000017 is process 2's 17th write.
			g.doWrite(process, id, t, k, int64(process+1)*1000000+seq)
			continue
		}
		g.doRead(process, id, t, k, e.mode)
	}
}

func (g *generator) doWrite(process int, opID int64, t target, k string, v int64) {
	g.issued.Add(1)
	p64 := int64(process)
	val := json.RawMessage(strconv.FormatInt(v, 10))
	g.w.write(&record{TNS: time.Now().UnixNano(), Process: &p64, Type: typeInvoke, F: mixWrite,
		Key: &k, Value: val, OpID: &opID, Meta: &meta{Target: t.label}})

	start := time.Now()
	att := send(g.http, http.MethodPut, t.base+"/v1/kv/"+k, []byte(val), g.o.timeout)
	out := classifyWrite(att)
	m := responseMeta(att, t.label, start)
	comp := record{TNS: time.Now().UnixNano(), Process: &p64, Type: out.typ, F: mixWrite,
		Key: &k, OpID: &opID, Error: out.label, Meta: m}
	if out.typ == typeOK {
		comp.Value = val
	}
	g.count(out.typ)
	g.w.write(&comp)
}

func (g *generator) doRead(process int, opID int64, t target, k string, mode string) {
	g.issued.Add(1)
	p64 := int64(process)
	g.w.write(&record{TNS: time.Now().UnixNano(), Process: &p64, Type: typeInvoke, F: "read",
		Key: &k, OpID: &opID, Meta: &meta{Target: t.label, ReadMode: mode}})

	start := time.Now()
	att := send(g.http, http.MethodGet, t.base+"/v1/kv/"+k+readModes[mode], nil, g.o.timeout)
	out := classifyRead(att, k)
	m := responseMeta(att, t.label, start)
	m.ReadMode = mode
	comp := record{TNS: time.Now().UnixNano(), Process: &p64, Type: out.typ, F: "read",
		Key: &k, OpID: &opID, Error: out.label, Meta: m}
	if out.typ == typeOK {
		comp.Value = out.value
	}
	g.count(out.typ)
	g.w.write(&comp)
}

func responseMeta(a attempt, label string, start time.Time) *meta {
	m := &meta{Target: label, Status: a.status, LatencyUS: time.Since(start).Microseconds()}
	if a.header != nil {
		m.KnownLeader = a.header.Get(knownLeaderHeader)
		m.LastContact = a.header.Get(lastContactHeader)
		m.Consistency = a.header.Get(consistencyHeader)
		m.Index = a.header.Get(indexHeader)
	}
	return m
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
// HTTP, with the evidence the classifier needs
// ---------------------------------------------------------------------------

// attempt is everything the client observed about one round trip, explicit so
// the classifiers are pure functions a table test can drive.
type attempt struct {
	wroteRequest bool
	gotResponse  bool
	err          error
	status       int
	header       http.Header
	body         []byte
}

// newHTTPClient never follows a redirect; Consul's KV API sends none, and one
// would not be this API's answer.
func newHTTPClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{MaxIdleConnsPerHost: 64, IdleConnTimeout: 30 * time.Second},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func send(client *http.Client, method, u string, body []byte, timeout time.Duration) attempt {
	var att attempt
	var mu sync.Mutex
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	trace := &httptrace.ClientTrace{
		WroteRequest: func(i httptrace.WroteRequestInfo) {
			mu.Lock()
			att.wroteRequest = att.wroteRequest || i.Err == nil
			mu.Unlock()
		},
		GotFirstResponseByte: func() {
			mu.Lock()
			att.gotResponse = true
			mu.Unlock()
		},
	}
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, trace), method, u, rd)
	if err != nil {
		att.err = err
		return att
	}
	resp, err := client.Do(req)
	mu.Lock()
	defer mu.Unlock()
	if err != nil {
		att.err = err
		return att
	}
	defer resp.Body.Close()
	att.gotResponse = true
	att.status = resp.StatusCode
	att.header = resp.Header
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		att.err = err
		return att
	}
	att.body = raw
	return att
}

// ---------------------------------------------------------------------------
// Classification: fail needs proof
// ---------------------------------------------------------------------------

type outcome struct {
	typ   string
	label string
	value json.RawMessage // a read's value, when typ is ok
}

// transport classifies an attempt that produced no usable response. done is
// false when the caller must go on to read the status and body.
func transport(a attempt) (outcome, bool) {
	if a.err != nil {
		if !a.wroteRequest && provablyNeverSent(a.err) {
			return outcome{typ: typeFail, label: errorLabel(a.err)}, true
		}
		return outcome{typ: typeInfo, label: errorLabel(a.err)}, true
	}
	if !a.gotResponse {
		return outcome{typ: typeInfo, label: "no-response"}, true
	}
	return outcome{}, false
}

// classifyWrite is the write table:
//
//	no request byte sent (refused, no such host)         -> fail
//	any other transport error, or no response            -> info
//	500 carrying "No cluster leader"                     -> fail (refused before apply)
//	200 with the body `true`                             -> ok
//	200 with any other body (false, a string, empty)     -> info
//	any other status                                     -> info
func classifyWrite(a attempt) outcome {
	if out, done := transport(a); done {
		return out
	}
	body := strings.TrimSpace(string(a.body))
	switch {
	case a.status == http.StatusOK && body == "true":
		return outcome{typ: typeOK}
	case a.status == http.StatusOK:
		return outcome{typ: typeInfo, label: fmt.Sprintf("200 with body %q, not true", clip(body))}
	case a.status == http.StatusInternalServerError && strings.Contains(body, noLeader):
		return outcome{typ: typeFail, label: "refused before apply: " + clip(body)}
	}
	return outcome{typ: typeInfo, label: statusLabel(a.status, a.body)}
}

// classifyRead is the read table. A read changes nothing, so no read outcome
// can hide a write; it is ok only when the answer is KVSGet's not-found (404,
// empty body, with the X-Consul-Index header setMeta writes first), or a 200
// array of exactly one entry for this key whose base64 Value decodes to a
// decimal integer.
func classifyRead(a attempt, k string) outcome {
	if out, done := transport(a); done {
		return out
	}
	switch a.status {
	case http.StatusNotFound:
		if a.header.Get(indexHeader) == "" || strings.TrimSpace(string(a.body)) != "" {
			return outcome{typ: typeInfo, label: "404 without KVSGet's X-Consul-Index header or with a body: " + statusLabel(a.status, a.body)}
		}
		return outcome{typ: typeOK, value: json.RawMessage("null")}
	case http.StatusOK:
	default:
		return outcome{typ: typeInfo, label: statusLabel(a.status, a.body)}
	}
	var entries []struct {
		Key   string
		Value []byte
	}
	if err := json.Unmarshal(a.body, &entries); err != nil {
		return outcome{typ: typeInfo, label: "unparseable body: " + clip(string(a.body))}
	}
	switch {
	case len(entries) != 1:
		return outcome{typ: typeInfo, label: fmt.Sprintf("expected one entry, got %d", len(entries))}
	case entries[0].Key != k:
		return outcome{typ: typeInfo, label: fmt.Sprintf("asked for %q, answered %q", k, clip(entries[0].Key))}
	}
	n, err := strconv.ParseInt(string(entries[0].Value), 10, 64)
	if err != nil {
		return outcome{typ: typeInfo, label: "the value is not an integer: " + clip(fmt.Sprintf("%q", entries[0].Value))}
	}
	return outcome{typ: typeOK, value: json.RawMessage(strconv.FormatInt(n, 10))}
}

func provablyNeverSent(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) || errors.Is(err, os.ErrDeadlineExceeded) {
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
	case errors.Is(err, context.Canceled):
		return "canceled"
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "timeout"
	}
	return clip(err.Error())
}

func statusLabel(status int, raw []byte) string {
	if msg := strings.TrimSpace(string(raw)); msg != "" {
		return fmt.Sprintf("%d %s", status, clip(msg))
	}
	return fmt.Sprintf("%d %s", status, http.StatusText(status))
}

func clip(s string) string {
	if len(s) > 200 {
		return s[:200]
	}
	return s
}
