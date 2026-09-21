// Command etcdload is the PRO-THESIS workload driver for the etcd target.
//
// It is a third-party driver in every sense that matters: it talks to an
// upstream etcd over its gRPC-gateway JSON API with nothing but net/http, and
// it writes the history in prothesis.history/v1 using the PUBLIC schema
// package: the one thing a driver author is expected to depend on. It
// imports nothing from the harness's internal packages.
//
// The contract it honours, all of it visible in prothesis.yaml or in the
// environment the harness exports:
//
//   - `driver.cmd` placeholders: --history {history_path}, --seed {seed},
//     --profile {profile}.
//   - PROTHESIS_TARGETS: the client-plane addresses of this world's nodes, in
//     prothesis.yaml node order. Under parallel execution every world has its
//     own ports, so the driver never hard-codes any.
//   - PROTHESIS_PLAN_PATH: the resolved driver plan (D-021). A PROFILE plan
//     carries clients/ops/mix and the driver generates its workload from them
//     and from the seed. An OPERATION plan carries the explicit trace and the
//     driver executes exactly that (op ids verbatim, per-process order kept,
//     targets mapped position-for-position) which is what makes stage-2
//     shrinking sound (D-056).
//   - PROTHESIS_STDIN_CONTROL=1: stdin is a control channel; {"cmd":"stop"} or
//     EOF means stop ISSUING and finish what is in flight (OQ-033).
//
// # The soundness rule
//
// A completion is `fail` only with positive evidence that the operation never
// reached the server. Everything ambiguous (a timeout, a reset after the
// request was written, a 5xx without a definite refusal) is `info`. A paused
// member's kernel completes the TCP handshake and buffers the request; the
// client times out with no idea whether the write will apply on resume. Record
// that as `fail` and every consistency verdict built on the history is
// unsound while still looking correct. This table is the fixture's
// (testdata/kvfixture/internal/history/classify.go), restated here because the
// property it protects belongs to every driver, not to that one.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
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
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

const (
	exitOK     = 0
	exitRun    = 1
	exitConfig = 2

	envTargets = "PROTHESIS_TARGETS"
	envPlan    = "PROTHESIS_PLAN_PATH"
	envStdin   = "PROTHESIS_STDIN_CONTROL"

	planSchema = "prothesis.driver_plan/v1"

	// Mix vocabulary. `read` and `write` are what the checker recognises;
	// `read_serializable` is this driver's own name for a serializable read,
	// RECORDED as f:"read" with meta.read_mode:"serializable" so the checker
	// holds it to the same standard as any other read.
	mixRead             = "read"
	mixReadSerializable = "read_serializable"
	mixWrite            = "write"

	readLinearizable = "linearizable"
	readSerializable = "serializable"

	defaultTargets = "127.0.0.1:12379,127.0.0.1:12380,127.0.0.1:12381"
)

func main() {
	code, err := run(os.Args[1:], os.Stdin, os.Stderr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "etcdload: %v\n", err)
	}
	os.Exit(code)
}

type options struct {
	historyPath  string
	seed         uint64
	profile      string
	targets      string
	planPath     string
	stdinControl bool
	clients      int
	ops          int
	keys         int
	duration     time.Duration
	timeout      time.Duration
}

func run(args []string, stdin io.Reader, stderr io.Writer) (int, error) {
	var o options
	fs := flag.NewFlagSet("etcdload", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&o.historyPath, "history", "", "history file to write (required)")
	fs.Uint64Var(&o.seed, "seed", 1, "world seed")
	fs.StringVar(&o.profile, "profile", "", "driver profile NAME (informational; the plan carries the parameters)")
	dt := defaultTargets
	if t := strings.TrimSpace(os.Getenv(envTargets)); t != "" {
		dt = t
	}
	fs.StringVar(&o.targets, "targets", dt, "comma-separated client-plane targets (host:port or URL); defaults to $"+envTargets)
	fs.StringVar(&o.planPath, "plan", os.Getenv(envPlan), "driver plan to execute; defaults to $"+envPlan)
	fs.BoolVar(&o.stdinControl, "stdin-control", os.Getenv(envStdin) == "1", `stop on stdin EOF or a {"cmd":"stop"} line`)
	fs.IntVar(&o.clients, "clients", 0, "override the plan's client count")
	fs.IntVar(&o.ops, "ops", 0, "override the plan's operation budget")
	fs.IntVar(&o.keys, "keys", 8, "key space: k/0 .. k/N-1")
	fs.DurationVar(&o.duration, "duration", 0, "stop after this long (0 = run the op budget)")
	fs.DurationVar(&o.timeout, "timeout", 2*time.Second, "per-request timeout")
	if err := fs.Parse(args); err != nil {
		return exitConfig, err
	}
	if o.historyPath == "" {
		return exitConfig, errors.New("--history is required")
	}
	if o.keys <= 0 {
		return exitConfig, errors.New("--keys must be positive")
	}

	targets, err := parseTargets(o.targets)
	if err != nil {
		return exitConfig, err
	}

	// The plan is loaded BEFORE the history file is created: a plan this
	// driver cannot execute must not leave behind a history a checker would
	// call clean.
	var pl *plan
	if o.planPath != "" {
		pl, err = loadPlan(o.planPath)
		if err != nil {
			return exitConfig, fmt.Errorf("plan: %w", err)
		}
		if pl.operationMode() && len(pl.Targets) > len(targets) {
			return exitConfig, fmt.Errorf("plan: the trace was recorded against %d target(s) and this driver has %d",
				len(pl.Targets), len(targets))
		}
	}

	clients, ops, mix := 4, 500, []mixEntry{{op: mixRead, weight: 500000}, {op: mixWrite, weight: 500000}}
	if pl != nil {
		if pl.Clients > 0 {
			clients = pl.Clients
		}
		if pl.Ops > 0 {
			ops = pl.Ops
		}
		if len(pl.Mix) > 0 {
			mix = mix[:0]
			for _, m := range pl.Mix {
				mix = append(mix, mixEntry{op: m.Op, weight: m.WeightPPM})
			}
		}
	}
	if o.clients > 0 {
		clients = o.clients
	}
	if o.ops > 0 {
		ops = o.ops
	}
	table, err := buildMix(mix)
	if err != nil {
		return exitConfig, err
	}

	f, err := os.Create(o.historyPath)
	if err != nil {
		return exitRun, fmt.Errorf("history: %w", err)
	}
	hw := newHistWriter(f)

	g := &generator{
		o:       o,
		targets: targets,
		w:       hw,
		http: &http.Client{Transport: &http.Transport{
			MaxIdleConnsPerHost: 64,
			IdleConnTimeout:     30 * time.Second,
		}},
		mix:     table,
		clients: clients,
		stop:    make(chan struct{}),
		plan:    pl,
	}
	g.remaining.Store(int64(ops))

	if pl != nil && pl.operationMode() {
		fmt.Fprintf(stderr, "etcdload: OPERATION MODE seed=%d plan=%s operations=%d processes=%d history=%s\n",
			o.seed, o.planPath, len(pl.Operations), len(pl.processes()), o.historyPath)
	} else {
		fmt.Fprintf(stderr, "etcdload: PROFILE MODE seed=%d clients=%d ops=%d mix=%s targets=%d history=%s\n",
			o.seed, clients, ops, describeMix(mix), len(targets), o.historyPath)
	}

	if o.stdinControl {
		go g.watchStdin(stdin)
	}
	if o.duration > 0 {
		t := time.AfterFunc(o.duration, g.requestStop)
		defer t.Stop()
	}

	if pl != nil && pl.operationMode() {
		g.runPlan()
	} else {
		g.runClients()
	}

	if err := hw.close(); err != nil {
		return exitRun, fmt.Errorf("history: %w", err)
	}
	fmt.Fprintf(stderr, "etcdload: done ops=%d ok=%d fail=%d info=%d records=%d\n",
		g.issued.Load(), g.nOK.Load(), g.nFail.Load(), g.nInfo.Load(), hw.count())
	return exitOK, nil
}

// ---------------------------------------------------------------------------
// Targets
// ---------------------------------------------------------------------------

// target is one client-plane address. Label is what meta.target carries and
// what an operation plan's Targets list is built from: the node's POSITION in
// PROTHESIS_TARGETS, spelled etcd-n<i+1> to match prothesis.yaml's node ids.
// Labels rather than addresses, because addresses are world-local (D-043) and
// a replay maps position-for-position.
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
			label = "etcd-n" + strconv.Itoa(len(out)+1)
		}
		out = append(out, target{label: label, base: raw})
	}
	if len(out) == 0 {
		return nil, errors.New("no targets: pass --targets or run under thesis, which exports " + envTargets)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// The plan (prothesis.driver_plan/v1), read tolerantly
// ---------------------------------------------------------------------------

type plan struct {
	Schema  string `json:"schema"`
	Profile string `json:"profile"`
	Clients int    `json:"clients"`
	Ops     int    `json:"ops"`
	Mix     []struct {
		Op        string `json:"op"`
		WeightPPM int64  `json:"weight_ppm"`
	} `json:"mix"`
	// Operations present (even if it decodes to a slice) means OPERATION MODE.
	// The discriminator is presence, not the path: the harness always exports
	// PROTHESIS_PLAN_PATH, and it has pointed at a profile plan on every
	// ordinary run.
	Operations []planOp `json:"operations"`
	Targets    []string `json:"targets"`
}

type planOp struct {
	OpID     int64           `json:"op_id"`
	Process  int             `json:"process"`
	F        string          `json:"f"`
	Key      string          `json:"key"`
	Value    json.RawMessage `json:"value"`
	ReadMode string          `json:"read_mode"`
	Target   string          `json:"target"`
	AtMS     int64           `json:"at_ms"`
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
	// An EMPTY but present trace is refused: "execute nothing" is a driver
	// that drives nothing while still writing a history a checker calls clean.
	if bytes.Contains(data, []byte(`"operations"`)) && len(p.Operations) == 0 {
		return nil, fmt.Errorf("%s: carries an empty operation trace; refusing to drive nothing", path)
	}
	for i, op := range p.Operations {
		switch op.F {
		case mixRead, mixWrite:
		default:
			return nil, fmt.Errorf("%s: operations[%d] (op_id %d): unknown f %q; this driver knows %q and %q",
				path, i, op.OpID, op.F, mixRead, mixWrite)
		}
		switch op.ReadMode {
		case "", readLinearizable, readSerializable:
		default:
			return nil, fmt.Errorf("%s: operations[%d] (op_id %d): unknown read_mode %q", path, i, op.OpID, op.ReadMode)
		}
		if op.Target != "" && indexOf(p.Targets, op.Target) < 0 {
			return nil, fmt.Errorf("%s: operations[%d] (op_id %d): target %q is not in the plan's targets list",
				path, i, op.OpID, op.Target)
		}
	}
	return &p, nil
}

func (p *plan) operationMode() bool { return len(p.Operations) > 0 }

// processes returns the distinct process ids in the trace, ascending.
func (p *plan) processes() []int {
	seen := map[int]bool{}
	var out []int
	for _, op := range p.Operations {
		if !seen[op.Process] {
			seen[op.Process] = true
			out = append(out, op.Process)
		}
	}
	sort.Ints(out)
	return out
}

func indexOf(list []string, s string) int {
	for i, v := range list {
		if v == s {
			return i
		}
	}
	return -1
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

func buildMix(entries []mixEntry) (*mixTable, error) {
	t := &mixTable{}
	for _, e := range entries {
		switch e.op {
		case mixRead, mixReadSerializable, mixWrite:
		default:
			return nil, fmt.Errorf("mix: unknown op %q; this driver knows %q, %q and %q",
				e.op, mixRead, mixReadSerializable, mixWrite)
		}
		if e.weight < 0 {
			return nil, fmt.Errorf("mix: %s has negative weight %d", e.op, e.weight)
		}
		if e.weight == 0 {
			continue
		}
		t.total += e.weight
		t.entries = append(t.entries, e)
		t.cum = append(t.cum, t.total)
	}
	if t.total == 0 {
		return nil, errors.New("mix: every weight is zero; there is no workload to drive")
	}
	return t, nil
}

func (t *mixTable) pick(r *rand.Rand) string {
	x := r.Int63n(t.total)
	for i, c := range t.cum {
		if x < c {
			return t.entries[i].op
		}
	}
	return t.entries[len(t.entries)-1].op
}

func describeMix(entries []mixEntry) string {
	parts := make([]string, 0, len(entries))
	for _, e := range entries {
		parts = append(parts, fmt.Sprintf("%s=%d", e.op, e.weight))
	}
	return strings.Join(parts, ",")
}

// ---------------------------------------------------------------------------
// History records
// ---------------------------------------------------------------------------

// record is the public schema's entry plus this driver's namespaced `meta`.
// Embedding flattens the schema fields, so the line is a valid
// prothesis.history/v1 record that happens to carry one extra member, which
// every reader on the harness side tolerates and two of them (target,
// read_mode) read back when they build an operation plan.
type record struct {
	schema.HistoryEntry
	Meta *meta `json:"meta,omitempty"`
}

type meta struct {
	Target    string `json:"target,omitempty"`
	ReadMode  string `json:"read_mode,omitempty"`
	Node      string `json:"node,omitempty"` // etcd member id that answered
	Revision  int64  `json:"revision,omitempty"`
	RaftTerm  int64  `json:"raft_term,omitempty"`
	LatencyUS int64  `json:"latency_us,omitempty"`
	Status    int    `json:"status,omitempty"`
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

// write validates the record against the public schema before it goes to disk,
// then flushes: a driver the harness has to kill must not lose the completions
// it already has.
func (h *histWriter) write(r *record) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.err != nil {
		return
	}
	if err := r.HistoryEntry.Validate(); err != nil {
		h.err = fmt.Errorf("refusing to write an invalid record (op_id %v): %w", derefInt(r.OpID), err)
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

func derefInt(p *int64) int64 {
	if p == nil {
		return -1
	}
	return *p
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
	plan    *plan

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

// watchStdin honours the drain protocol: {"cmd":"stop"} or EOF, whichever
// comes first, asks the clients to stop at the top of their loop. The
// operation in flight runs to completion.
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

// runClients is PROFILE MODE: every key is seeded once by process 0, then
// each process draws from the mix until the shared op budget is spent, the
// duration elapses, or a drain is requested.
func (g *generator) runClients() {
	// Seed the key space so a read never has to answer "null": a null read
	// before any write is legitimate register state, but a checker that does
	// not model an initial value would either drop it or misjudge it, and a
	// seeded space makes the question moot. The seed writes are ordinary
	// operations, recorded like any other.
	for k := 0; k < g.o.keys; k++ {
		if g.stopped() || g.remaining.Add(-1) < 0 {
			break
		}
		g.doWrite(0, g.nextOpID.Add(1), g.targets[k%len(g.targets)], key(k), int64(1000000+k))
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
		if g.stopped() {
			return
		}
		if g.remaining.Add(-1) < 0 {
			return
		}
		op := g.mix.pick(r)
		k := key(r.Intn(g.o.keys))
		t := g.targets[r.Intn(len(g.targets))]
		id := g.nextOpID.Add(1)
		switch op {
		case mixRead:
			g.doRead(process, id, t, k, readLinearizable)
		case mixReadSerializable:
			g.doRead(process, id, t, k, readSerializable)
		case mixWrite:
			seq++
			// Unique across processes for seq < 1e6, and readable by a human
			// looking at a witness: 3000017 is process 3's 17th write.
			g.doWrite(process, id, t, k, int64(process+1)*1000000+seq)
		}
	}
}

// runPlan is OPERATION MODE: one goroutine per recorded process, each
// executing its operations in trace order with the recorded op ids. Nothing is
// generated, nothing is renumbered, and a stop request ends each process at
// its next operation boundary.
func (g *generator) runPlan() {
	byProcess := map[int][]planOp{}
	for _, op := range g.plan.Operations {
		byProcess[op.Process] = append(byProcess[op.Process], op)
	}
	targetOf := func(name string) target {
		if name == "" {
			return g.targets[0]
		}
		return g.targets[indexOf(g.plan.Targets, name)]
	}
	var wg sync.WaitGroup
	for process, ops := range byProcess {
		wg.Add(1)
		go func(process int, ops []planOp) {
			defer wg.Done()
			for _, op := range ops {
				if g.stopped() {
					return
				}
				t := targetOf(op.Target)
				switch op.F {
				case mixRead:
					mode := op.ReadMode
					if mode == "" {
						mode = readLinearizable
					}
					g.doRead(process, op.OpID, t, op.Key, mode)
				case mixWrite:
					v, err := strconv.ParseInt(strings.TrimSpace(string(op.Value)), 10, 64)
					if err != nil {
						// Validate already constrained f; a non-integer write
						// value is a trace this driver cannot execute faithfully.
						// Stopping is the honest response: a history missing
						// this op is caught by D-056's count, a history with a
						// substituted value would not be.
						fmt.Fprintf(os.Stderr, "etcdload: plan op_id %d: write value %s is not an integer; stopping\n",
							op.OpID, string(op.Value))
						g.requestStop()
						return
					}
					g.doWrite(process, op.OpID, t, op.Key, v)
				}
			}
		}(process, ops)
	}
	wg.Wait()
}

// ---------------------------------------------------------------------------
// Operations
// ---------------------------------------------------------------------------

func (g *generator) doWrite(process int, opID int64, t target, k string, v int64) {
	g.issued.Add(1)
	p64 := int64(process)
	val := json.RawMessage(strconv.FormatInt(v, 10))
	body, _ := json.Marshal(map[string]string{
		"key":   base64.StdEncoding.EncodeToString([]byte(k)),
		"value": base64.StdEncoding.EncodeToString([]byte(strconv.FormatInt(v, 10))),
	})
	g.w.write(&record{
		HistoryEntry: schema.HistoryEntry{TNS: time.Now().UnixNano(), Process: &p64, Type: schema.HistoryInvoke,
			F: mixWrite, Key: &k, Value: val, OpID: &opID},
		Meta: &meta{Target: t.label},
	})
	start := time.Now()
	att, raw := g.do(t.base+"/v3/kv/put", body)
	var hdr responseHeader
	att.bodyParsed = att.status == http.StatusOK && json.Unmarshal(raw, &hdr) == nil
	out := classify(att, raw)
	m := &meta{Target: t.label, LatencyUS: time.Since(start).Microseconds(), Status: att.status}
	hdr.fill(m)
	comp := record{
		HistoryEntry: schema.HistoryEntry{TNS: time.Now().UnixNano(), Process: &p64, Type: out.typ,
			F: mixWrite, Key: &k, OpID: &opID, Error: out.label},
		Meta: m,
	}
	if out.typ == schema.HistoryOK {
		comp.Value = val
	}
	g.count(out.typ)
	g.w.write(&comp)
}

func (g *generator) doRead(process int, opID int64, t target, k string, mode string) {
	g.issued.Add(1)
	p64 := int64(process)
	body, _ := json.Marshal(map[string]any{
		"key":          base64.StdEncoding.EncodeToString([]byte(k)),
		"serializable": mode == readSerializable,
	})
	g.w.write(&record{
		HistoryEntry: schema.HistoryEntry{TNS: time.Now().UnixNano(), Process: &p64, Type: schema.HistoryInvoke,
			F: mixRead, Key: &k, OpID: &opID},
		Meta: &meta{Target: t.label, ReadMode: mode},
	})
	start := time.Now()
	att, raw := g.do(t.base+"/v3/kv/range", body)
	var resp rangeResponse
	att.bodyParsed = att.status == http.StatusOK && json.Unmarshal(raw, &resp) == nil
	out := classify(att, raw)
	m := &meta{Target: t.label, ReadMode: mode, LatencyUS: time.Since(start).Microseconds(), Status: att.status}
	resp.fill(m)
	comp := record{
		HistoryEntry: schema.HistoryEntry{TNS: time.Now().UnixNano(), Process: &p64, Type: out.typ,
			F: mixRead, Key: &k, OpID: &opID, Error: out.label},
		Meta: m,
	}
	if out.typ == schema.HistoryOK {
		comp.Value = resp.value()
	}
	g.count(out.typ)
	g.w.write(&comp)
}

func (g *generator) count(t schema.HistoryType) {
	switch t {
	case schema.HistoryOK:
		g.nOK.Add(1)
	case schema.HistoryFail:
		g.nFail.Add(1)
	default:
		g.nInfo.Add(1)
	}
}

// responseHeader is the etcd response header every KV reply carries. The
// numbers are JSON STRINGS on the wire (gRPC-gateway encodes int64 that way).
type responseHeader struct {
	Header struct {
		MemberID string `json:"member_id"`
		Revision string `json:"revision"`
		RaftTerm string `json:"raft_term"`
	} `json:"header"`
}

func (h *responseHeader) fill(m *meta) {
	m.Node = h.Header.MemberID
	m.Revision, _ = strconv.ParseInt(h.Header.Revision, 10, 64)
	m.RaftTerm, _ = strconv.ParseInt(h.Header.RaftTerm, 10, 64)
}

type rangeResponse struct {
	responseHeader
	KVs []struct {
		Value string `json:"value"`
	} `json:"kvs"`
}

// value is the read's result as the history carries it: the stored integer,
// the stored bytes as a JSON string if they are not an integer, or null for
// an absent key.
func (r *rangeResponse) value() json.RawMessage {
	if len(r.KVs) == 0 {
		return json.RawMessage("null")
	}
	b, err := base64.StdEncoding.DecodeString(r.KVs[0].Value)
	if err != nil {
		return json.RawMessage("null")
	}
	if _, err := strconv.ParseInt(string(b), 10, 64); err == nil {
		return json.RawMessage(b)
	}
	s, _ := json.Marshal(string(b))
	return json.RawMessage(s)
}

// ---------------------------------------------------------------------------
// HTTP with the evidence the classifier needs
// ---------------------------------------------------------------------------

// attempt is everything the client observed about one round trip, explicit
// rather than derived, so classify is a pure function a table test can drive.
type attempt struct {
	wroteRequest bool
	gotResponse  bool
	err          error
	status       int
	bodyParsed   bool
}

func (g *generator) do(url string, body []byte) (attempt, []byte) {
	var att attempt
	ctx, cancel := context.WithTimeout(context.Background(), g.o.timeout)
	defer cancel()
	trace := &httptrace.ClientTrace{
		WroteRequest:         func(i httptrace.WroteRequestInfo) { att.wroteRequest = i.Err == nil },
		GotFirstResponseByte: func() { att.gotResponse = true },
	}
	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, trace), http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		att.err = err
		return att, nil
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := g.http.Do(req)
	if err != nil {
		att.err = err
		return att, nil
	}
	defer resp.Body.Close()
	att.gotResponse = true
	att.status = resp.StatusCode
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		att.err = err
		return att, nil
	}
	return att, raw
}

// ---------------------------------------------------------------------------
// Classification: fail needs proof
// ---------------------------------------------------------------------------

type outcome struct {
	typ   schema.HistoryType
	label string
}

// classify implements the ok/fail/info table:
//
//	200 with a parseable body                          -> ok
//	dial refused / DNS failure, no request byte sent   -> fail  (never reached the process)
//	any timeout, before or after headers               -> info
//	reset after the request was written                -> info
//	200 with a body we could not parse                 -> info  (it ran; we do not know what it did)
//	any other status                                   -> info  (etcd carries no applied:false)
func classify(a attempt, raw []byte) outcome {
	if a.err != nil {
		if !a.wroteRequest && provablyNeverSent(a.err) {
			return outcome{schema.HistoryFail, errorLabel(a.err)}
		}
		return outcome{schema.HistoryInfo, errorLabel(a.err)}
	}
	if !a.gotResponse {
		return outcome{schema.HistoryInfo, "no-response"}
	}
	if a.status == http.StatusOK {
		if !a.bodyParsed {
			return outcome{schema.HistoryInfo, "unparseable-body"}
		}
		return outcome{schema.HistoryOK, ""}
	}
	return outcome{schema.HistoryInfo, statusLabel(a.status, raw)}
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
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "no such host") ||
		strings.Contains(msg, "actively refused")
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
	msg := err.Error()
	if len(msg) > 200 {
		msg = msg[:200]
	}
	return msg
}

// statusLabel prefers the gRPC-gateway's message ("etcdserver: request timed
// out", "etcdserver: leader changed") to the bare status text.
func statusLabel(status int, raw []byte) string {
	var e struct {
		Message string `json:"message"`
		Error   string `json:"error"`
	}
	if json.Unmarshal(raw, &e) == nil {
		msg := e.Message
		if msg == "" {
			msg = e.Error
		}
		if msg != "" {
			if len(msg) > 200 {
				msg = msg[:200]
			}
			return fmt.Sprintf("%d %s", status, msg)
		}
	}
	return fmt.Sprintf("%d %s", status, http.StatusText(status))
}
