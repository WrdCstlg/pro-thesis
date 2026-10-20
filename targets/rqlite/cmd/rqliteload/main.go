// Command rqliteload is the Synthesis workload driver for the rqlite target.
//
// It is written the way a third party would write it (D-065): the Go standard
// library only, rqlite's plain HTTP API, and the history format of
// docs/TARGETS.md section 5 spelled out here rather than imported from the
// harness. Single-key register operations on one table:
//
//	write  INSERT OR REPLACE INTO kv(k,v) VALUES(?,?)   POST /db/execute?redirect
//	read   SELECT v FROM kv WHERE k=?                   POST /db/query?level=<L>
//
// # Where every parameter comes from
//
// The command line (driver.cmd, lock-covered) and the plan file named by
// --plan, which the harness writes from driver.profiles (lock-covered for every
// profile a run profile names, D-090). The read level is part of the mix entry's
// name: read_linearizable, read_strong, read_weak or read_none. No workload
// parameter is read from the environment and nothing falls back to a built-in
// default: a missing plan, an empty mix, a zero client or op count and an
// unknown read level are refused before the history file exists. The one
// environment variable read is the topology the harness exports
// (PROTHESIS_TARGETS), and only when --targets is not given.
//
// # The soundness rules
//
//   - A completion is `fail` only with positive evidence that the operation
//     did not take effect: the request was never sent (connection refused, no
//     such host), or a write was answered 301 by a node that is not the leader.
//     With ?redirect, rqlite's store.Execute returns ErrNotLeader BEFORE it
//     applies anything, and the HTTP layer turns that into the 301 (rqlite
//     v10.5.3 store/store.go Execute, http/service.go execute/DoRedirect).
//   - Everything else that is not a fully parsed success is `info`: a timeout,
//     a reset after the request was written, any other status, and an HTTP 200
//     that carries a top-level `error` or a per-statement `error`.
//   - Writes go to the believed leader only and carry ?redirect, so a follower
//     never forwards one: rqlite retries a forwarded request at least once and
//     can apply a write twice (issue 1628). A 301 is never followed: its
//     Location names a container host name, which is mapped back to a node by
//     label, and that node becomes the believed leader for the next write. A
//     duplicate apply that still happens is the target's behaviour, recorded
//     as it is.
//   - Every response's X-RQLITE-VERSION must equal --expect-version. A
//     mismatch is recorded on the operation that saw it (info), stops the run,
//     and ends it non-zero.
//   - The read level sent is recorded in every read record's meta.read_mode.
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
	"net/url"
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

	versionHeader  = "X-RQLITE-VERSION"
	servedByHeader = "X-RQLITE-SERVED-BY"

	mixWrite   = "write"
	readPrefix = "read_"

	typeInvoke = "invoke"
	typeOK     = "ok"
	typeFail   = "fail"
	typeInfo   = "info"

	createTable = "CREATE TABLE IF NOT EXISTS kv (k TEXT NOT NULL PRIMARY KEY, v INTEGER NOT NULL)"
	writeSQL    = "INSERT OR REPLACE INTO kv(k,v) VALUES(?,?)"
	readSQL     = "SELECT v FROM kv WHERE k=?"
)

// readLevels are the levels this driver will send, spelled as rqlite's
// ?level parameter spells them. rqlite maps any other spelling to its default
// level without saying so, so anything else is refused at start-up.
var readLevels = []string{"linearizable", "strong", "weak", "none"}

func main() {
	code, err := run(os.Args[1:], os.Stdin, os.Stderr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "rqliteload: %v\n", err)
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
	fs := flag.NewFlagSet("rqliteload", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&o.historyPath, "history", "", "history file to write (required)")
	fs.Uint64Var(&o.seed, "seed", 1, "world seed")
	fs.StringVar(&o.profile, "profile", "", "driver profile name; when given it must equal the plan's")
	fs.StringVar(&o.planPath, "plan", "", "driver plan to execute (required): the harness's {plan_path}")
	fs.StringVar(&o.targets, "targets", "", "comma-separated [label=]host:port list; default: $"+envTargets+", labelled rq-n1.. by position")
	fs.StringVar(&o.expectVersion, "expect-version", "", "the X-RQLITE-VERSION every response must carry (required)")
	fs.BoolVar(&o.stdinControl, "stdin-control", false, `stop on stdin EOF or a {"cmd":"stop"} line`)
	fs.IntVar(&o.keys, "keys", 8, "key space: k/0 .. k/N-1")
	fs.DurationVar(&o.timeout, "timeout", 2*time.Second, "per-request client timeout; rqlite's own timeouts are set to 3/4 of it")
	fs.DurationVar(&o.setupTimeout, "setup-timeout", 30*time.Second, "how long to keep trying to create the table")
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

	fmt.Fprintf(stderr, "rqliteload: profile=%s seed=%d clients=%d ops=%d mix=%s targets=%d expect-version=%s history=%s\n",
		pl.Profile, o.seed, pl.Clients, pl.Ops, table.describe(), len(targets), o.expectVersion, o.historyPath)

	if err := g.setup(); err != nil {
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
	fmt.Fprintf(stderr, "rqliteload: done ops=%d ok=%d fail=%d info=%d records=%d\n",
		g.issued.Load(), g.nOK.Load(), g.nFail.Load(), g.nInfo.Load(), g.w.count())
	if msg := g.versionMismatch(); msg != "" {
		return exitRun, errors.New(msg)
	}
	return exitOK, nil
}

// ---------------------------------------------------------------------------
// Targets
// ---------------------------------------------------------------------------

// target is one node's HTTP API. Its label is what meta.target carries and
// what a redirect's Location host is mapped back to: the harness node id,
// rq-n<i+1> by position in PROTHESIS_TARGETS (prothesis.yaml's node order),
// or the explicit label=address form.
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
			label = "rq-n" + strconv.Itoa(len(out)+1)
		}
		out = append(out, target{label: label, base: raw})
	}
	if len(out) == 0 {
		return nil, errors.New("no targets: pass --targets or run under the harness, which exports " + envTargets)
	}
	return out, nil
}

// targetForLocation maps a 301's Location (rqlite's leader api_addr plus the
// path) to a target by its host name, which is the node's -http-adv-addr and
// therefore its compose service name. -1 when no target has that label.
func targetForLocation(loc string, targets []target) int {
	u, err := url.Parse(loc)
	if err != nil || u.Host == "" {
		return -1
	}
	host := u.Hostname()
	for i, t := range targets {
		if t.label == host {
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
	op     string // mixWrite or "read"
	level  string // the read level, for a read
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
			level := strings.TrimPrefix(m.Op, readPrefix)
			if !knownLevel(level) {
				return nil, fmt.Errorf("mix: %q names read level %q; this driver sends only %v, because rqlite treats an unknown level as weak without saying so",
					m.Op, level, readLevels)
			}
			e.op, e.level = "read", level
		default:
			return nil, fmt.Errorf("mix: unknown op %q; this driver knows %q and %s<level> with level one of %v",
				m.Op, mixWrite, readPrefix, readLevels)
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

func knownLevel(l string) bool {
	for _, k := range readLevels {
		if l == k {
			return true
		}
	}
	return false
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

// meta is this driver's namespaced evidence. read_mode is the ?level value
// sent, on every read record; the same key the fixture and etcd drivers use.
type meta struct {
	Target     string `json:"target,omitempty"`
	ReadMode   string `json:"read_mode,omitempty"`
	Status     int    `json:"status,omitempty"`
	Version    string `json:"version,omitempty"`
	ServedBy   string `json:"served_by,omitempty"`
	RedirectTo string `json:"redirect_to,omitempty"`
	LatencyUS  int64  `json:"latency_us,omitempty"`
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

	leader atomic.Int64 // index of the believed leader

	stop      chan struct{}
	stopOnce  sync.Once
	remaining atomic.Int64
	nextOpID  atomic.Int64
	issued    atomic.Int64
	nOK       atomic.Int64
	nFail     atomic.Int64
	nInfo     atomic.Int64

	mismatchMu sync.Mutex
	mismatch   string
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

func (g *generator) noteMismatch(msg string) {
	g.mismatchMu.Lock()
	if g.mismatch == "" {
		g.mismatch = msg
	}
	g.mismatchMu.Unlock()
	g.requestStop()
}

func (g *generator) versionMismatch() string {
	g.mismatchMu.Lock()
	defer g.mismatchMu.Unlock()
	return g.mismatch
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

// serverTimeout is what rqlite is told to give up after: shorter than the
// client's, so the node answers before the client stops listening.
func (g *generator) serverTimeout() string {
	return (g.o.timeout * 3 / 4).String()
}

// setup creates the table on the leader, learning the leader from 301s on
// the way. It is not an operation and is not recorded in the history.
func (g *generator) setup() error {
	body := mustStatements([]any{createTable})
	deadline := time.Now().Add(g.o.setupTimeout)
	last := "no attempt"
	for {
		idx := int(g.leader.Load())
		t := g.targets[idx]
		att := send(g.http, t.base+"/db/execute?redirect&timeout="+g.serverTimeout(), body, g.o.timeout)
		out := classifyExecute(att, g.o.expectVersion, g.targets, false)
		if out.versionMismatch {
			return errors.New(out.label)
		}
		switch {
		case out.typ == typeOK:
			return nil
		case out.redirect >= 0:
			g.leader.Store(int64(out.redirect))
			last = fmt.Sprintf("%s: %s", t.label, out.label)
		default:
			g.leader.Store(int64((idx + 1) % len(g.targets)))
			last = fmt.Sprintf("%s: %s", t.label, out.label)
		}
		if time.Now().After(deadline) || g.stopped() {
			return fmt.Errorf("could not create the table within %s: last answer %s", g.o.setupTimeout, last)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// runClients seeds every key once from process 0 (value = the key's index,
// below every client value), then runs the clients until the op budget is
// spent, a drain is requested, or a version mismatch stops the run.
func (g *generator) runClients() {
	for k := 0; k < g.o.keys; k++ {
		if g.stopped() || g.remaining.Add(-1) < 0 {
			break
		}
		g.doWrite(0, g.nextOpID.Add(1), key(k), int64(k))
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
		id := g.nextOpID.Add(1)
		if e.op == mixWrite {
			seq++
			// Unique per process for seq < 1e6, and above every seed value:
			// 3000017 is process 2's 17th write.
			g.doWrite(process, id, k, int64(process+1)*1000000+seq)
			continue
		}
		g.doRead(process, id, g.targets[r.Intn(len(g.targets))], k, e.level)
	}
}

func mustStatements(stmts ...[]any) []byte {
	b, err := json.Marshal(stmts)
	if err != nil {
		panic(err)
	}
	return b
}

func (g *generator) doWrite(process int, opID int64, k string, v int64) {
	g.issued.Add(1)
	p64 := int64(process)
	val := json.RawMessage(strconv.FormatInt(v, 10))
	idx := int(g.leader.Load())
	t := g.targets[idx]
	g.w.write(&record{TNS: time.Now().UnixNano(), Process: &p64, Type: typeInvoke, F: mixWrite,
		Key: &k, Value: val, OpID: &opID, Meta: &meta{Target: t.label}})

	start := time.Now()
	att := send(g.http, t.base+"/db/execute?redirect&timeout="+g.serverTimeout(),
		mustStatements([]any{writeSQL, k, v}), g.o.timeout)
	out := classifyExecute(att, g.o.expectVersion, g.targets, true)
	m := responseMeta(att, t.label, start)
	if out.redirect >= 0 {
		m.RedirectTo = g.targets[out.redirect].label
		g.leader.CompareAndSwap(int64(idx), int64(out.redirect))
	} else if out.typ != typeOK {
		// The believed leader did not take it: try the next node next time,
		// so a 301 from a live follower can name the new leader.
		g.leader.CompareAndSwap(int64(idx), int64((idx+1)%len(g.targets)))
	}
	if out.versionMismatch {
		g.noteMismatch(out.label)
	}
	comp := record{TNS: time.Now().UnixNano(), Process: &p64, Type: out.typ, F: mixWrite,
		Key: &k, OpID: &opID, Error: out.label, Meta: m}
	if out.typ == typeOK {
		comp.Value = val
	}
	g.count(out.typ)
	g.w.write(&comp)
}

func (g *generator) doRead(process int, opID int64, t target, k string, level string) {
	g.issued.Add(1)
	p64 := int64(process)
	g.w.write(&record{TNS: time.Now().UnixNano(), Process: &p64, Type: typeInvoke, F: "read",
		Key: &k, OpID: &opID, Meta: &meta{Target: t.label, ReadMode: level}})

	q := "level=" + level + "&timeout=" + g.serverTimeout()
	if level == "linearizable" {
		// rqlite's own default for this wait is 10 s (http/service.go
		// defaultLinearTimeout); stated explicitly.
		q += "&linearizable_timeout=" + g.serverTimeout()
	}
	start := time.Now()
	att := send(g.http, t.base+"/db/query?"+q, mustStatements([]any{readSQL, k}), g.o.timeout)
	out := classifyRead(att, g.o.expectVersion)
	m := responseMeta(att, t.label, start)
	m.ReadMode = level
	if out.versionMismatch {
		g.noteMismatch(out.label)
	}
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
		m.Version = a.header.Get(versionHeader)
		m.ServedBy = a.header.Get(servedByHeader)
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

// newHTTPClient never follows a redirect: a 301's Location names a container
// host name the host cannot resolve, and following it is not how a write
// reaches the leader here.
func newHTTPClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{MaxIdleConnsPerHost: 64, IdleConnTimeout: 30 * time.Second},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func send(client *http.Client, u string, body []byte, timeout time.Duration) attempt {
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
	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, trace), http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		att.err = err
		return att
	}
	req.Header.Set("Content-Type", "application/json")
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
	typ             string
	label           string
	value           json.RawMessage // a read's value, when typ is ok
	redirect        int             // target index a 301 named, or -1
	versionMismatch bool
}

// transport classifies an attempt that produced no usable response, and
// checks the version header on one that did. done is false when the caller
// must go on to read the body.
func transport(a attempt, want string) (outcome, bool) {
	if a.err != nil {
		if !a.wroteRequest && provablyNeverSent(a.err) {
			return outcome{typ: typeFail, label: errorLabel(a.err), redirect: -1}, true
		}
		return outcome{typ: typeInfo, label: errorLabel(a.err), redirect: -1}, true
	}
	if !a.gotResponse {
		return outcome{typ: typeInfo, label: "no-response", redirect: -1}, true
	}
	if got := a.header.Get(versionHeader); got != want {
		if got == "" {
			got = "(no " + versionHeader + " header)"
		}
		return outcome{typ: typeInfo, redirect: -1, versionMismatch: true,
			label: fmt.Sprintf("version mismatch: the node answered %s, --expect-version is %s", got, want)}, true
	}
	return outcome{redirect: -1}, false
}

// classifyWrite is the write table:
//
//	no request byte sent (refused, no such host)       -> fail
//	any other transport error, or no response          -> info
//	X-RQLITE-VERSION not --expect-version              -> info, and the run ends
//	301 from a node that is not the leader             -> fail (store.Execute refused it first)
//	200 with one result, no error, rows_affected 1     -> ok
//	200 with any error, or any other shape             -> info
//	any other status                                   -> info
func classifyWrite(a attempt, want string, targets []target) outcome {
	return classifyExecute(a, want, targets, true)
}

func classifyExecute(a attempt, want string, targets []target, isWrite bool) outcome {
	if out, done := transport(a, want); done {
		return out
	}
	if a.status == http.StatusMovedPermanently {
		loc := a.header.Get("Location")
		idx := targetForLocation(loc, targets)
		if idx < 0 {
			return outcome{typ: typeFail, redirect: -1,
				label: fmt.Sprintf("not leader; the redirect names %q, which is no node of this world", loc)}
		}
		return outcome{typ: typeFail, redirect: idx,
			label: fmt.Sprintf("not leader; the leader is %s", targets[idx].label)}
	}
	if a.status != http.StatusOK {
		return outcome{typ: typeInfo, redirect: -1, label: statusLabel(a.status, a.body)}
	}
	var resp struct {
		Results []struct {
			RowsAffected int64  `json:"rows_affected"`
			Error        string `json:"error"`
		} `json:"results"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(a.body, &resp); err != nil {
		return outcome{typ: typeInfo, redirect: -1, label: "unparseable body: " + clip(string(a.body))}
	}
	switch {
	case resp.Error != "":
		return outcome{typ: typeInfo, redirect: -1, label: "rqlite error: " + clip(resp.Error)}
	case len(resp.Results) != 1:
		return outcome{typ: typeInfo, redirect: -1, label: fmt.Sprintf("expected 1 result, got %d", len(resp.Results))}
	case resp.Results[0].Error != "":
		return outcome{typ: typeInfo, redirect: -1, label: "statement error: " + clip(resp.Results[0].Error)}
	case isWrite && resp.Results[0].RowsAffected != 1:
		return outcome{typ: typeInfo, redirect: -1, label: fmt.Sprintf("rows_affected %d, want 1", resp.Results[0].RowsAffected)}
	}
	return outcome{typ: typeOK, redirect: -1}
}

// classifyRead is the read table. A read changes nothing, so no read outcome
// can hide a write; it is ok only when the body parses to one result with no
// error and at most one row of one integer (or no row: the key is absent).
func classifyRead(a attempt, want string) outcome {
	if out, done := transport(a, want); done {
		return out
	}
	if a.status != http.StatusOK {
		return outcome{typ: typeInfo, redirect: -1, label: statusLabel(a.status, a.body)}
	}
	var resp struct {
		Results []struct {
			Values [][]json.RawMessage `json:"values"`
			Error  string              `json:"error"`
		} `json:"results"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(a.body, &resp); err != nil {
		return outcome{typ: typeInfo, redirect: -1, label: "unparseable body: " + clip(string(a.body))}
	}
	switch {
	case resp.Error != "":
		return outcome{typ: typeInfo, redirect: -1, label: "rqlite error: " + clip(resp.Error)}
	case len(resp.Results) != 1:
		return outcome{typ: typeInfo, redirect: -1, label: fmt.Sprintf("expected 1 result, got %d", len(resp.Results))}
	case resp.Results[0].Error != "":
		return outcome{typ: typeInfo, redirect: -1, label: "statement error: " + clip(resp.Results[0].Error)}
	}
	rows := resp.Results[0].Values
	switch {
	case len(rows) == 0:
		return outcome{typ: typeOK, redirect: -1, value: json.RawMessage("null")}
	case len(rows) > 1:
		return outcome{typ: typeInfo, redirect: -1, label: fmt.Sprintf("expected at most one row, got %d", len(rows))}
	case len(rows[0]) != 1:
		return outcome{typ: typeInfo, redirect: -1, label: fmt.Sprintf("expected one column, got %d", len(rows[0]))}
	}
	v := bytes.TrimSpace(rows[0][0])
	if _, err := strconv.ParseInt(string(v), 10, 64); err != nil {
		return outcome{typ: typeInfo, redirect: -1, label: "the value is not an integer: " + clip(string(v))}
	}
	return outcome{typ: typeOK, redirect: -1, value: json.RawMessage(v)}
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
