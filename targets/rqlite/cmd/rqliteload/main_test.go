package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// The response shapes below are rqlite v10.5.3's, read from its source rather
// than from a live node, so these tests could fail before the driver existed:
//
//   - http/service.go: every response carries X-RQLITE-VERSION (addBuildVersion,
//     called first in ServeHTTP). A body is {"results":[...],"error":"..."}
//     (type Response); a failed Execute or Query that is not a leader error is
//     HTTP 200 with a top-level "error" and "results":[] (DBResults marshals an
//     empty slice). With ?redirect a node that is not the leader answers 301
//     with Location <leader api_addr><path>?<raw query> (DoRedirect,
//     FormRedirect), and store.Execute returned ErrNotLeader before applying
//     anything (store/store.go, Execute).
//   - command/encoding/json.go: an execute result is
//     {"last_insert_id":N,"rows_affected":N,"error":"..."}, a query result is
//     {"columns":[...],"types":[...],"values":[[...]],"error":"..."}, every
//     member omitempty, so an empty result set has no "values".
//   - http/query_params.go: ?level is read by name, and an unknown name falls
//     through to the default level, which is why the driver must refuse one.

const pinnedVersion = "v10.5.3"

// kvStore is the register state behind one or more fake nodes.
type kvStore struct {
	mu sync.Mutex
	m  map[string]int64
}

func newStore() *kvStore { return &kvStore{m: map[string]int64{}} }

// reqLog is what a fake node saw of one request.
type reqLog struct {
	path     string
	rawQuery string
	sql      string
}

// fakeRqlite speaks the slice of rqlite's HTTP API the driver uses, with the
// shapes listed at the top of this file. It serialises every request, so any
// history it produces is linearizable; the tests are about the DRIVER.
type fakeRqlite struct {
	mu    sync.Mutex
	store *kvStore

	version     string // X-RQLITE-VERSION; "" sends none
	switchAfter int    // after this many requests, answer altVersion instead
	altVersion  string

	redirectTo string        // when set, INSERTs answer 301 to http://<redirectTo>
	execBody   string        // when set, INSERTs answer 200 with this body
	queryBody  string        // when set, SELECTs answer 200 with this body
	status     int           // when set, INSERTs answer this status
	stall      time.Duration // INSERTs sleep this long before answering
	hangup     bool          // INSERTs are read, then the connection is dropped

	requests int
	applied  int
	log      []reqLog
}

func newFake(st *kvStore) *fakeRqlite {
	return &fakeRqlite{store: st, version: pinnedVersion}
}

// statement decodes rqlite's request body: an array of statements, each a
// string or [sql, params...].
func statement(body []byte) (string, []any) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var stmts []any
	if err := dec.Decode(&stmts); err != nil || len(stmts) == 0 {
		return "", nil
	}
	switch s := stmts[0].(type) {
	case string:
		return s, nil
	case []any:
		if len(s) == 0 {
			return "", nil
		}
		sql, _ := s[0].(string)
		return sql, s[1:]
	}
	return "", nil
}

func (f *fakeRqlite) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	sql, params := statement(body)

	f.mu.Lock()
	f.requests++
	ver := f.version
	if f.switchAfter > 0 && f.requests > f.switchAfter {
		ver = f.altVersion
	}
	f.log = append(f.log, reqLog{path: r.URL.Path, rawQuery: r.URL.RawQuery, sql: sql})
	redirectTo, execBody, queryBody, status, stall, hangup := f.redirectTo, f.execBody, f.queryBody, f.status, f.stall, f.hangup
	f.mu.Unlock()

	if ver != "" {
		w.Header().Set("X-RQLITE-VERSION", ver)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	switch r.URL.Path {
	case "/db/execute":
		if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(sql)), "CREATE") {
			_, _ = io.WriteString(w, `{"results":[{}]}`)
			return
		}
		if redirectTo != "" {
			w.Header().Set("Location", "http://"+redirectTo+r.URL.Path+"?"+r.URL.RawQuery)
			w.WriteHeader(http.StatusMovedPermanently)
			return
		}
		if stall > 0 {
			time.Sleep(stall)
		}
		if hangup {
			hj, ok := w.(http.Hijacker)
			if ok {
				conn, _, err := hj.Hijack()
				if err == nil {
					_ = conn.Close()
				}
			}
			return
		}
		if status != 0 {
			http.Error(w, "leader not found", status)
			return
		}
		if execBody != "" {
			_, _ = io.WriteString(w, execBody)
			return
		}
		if len(params) != 2 {
			_, _ = io.WriteString(w, `{"results":[{"error":"wrong number of parameters"}]}`)
			return
		}
		k, _ := params[0].(string)
		n, _ := params[1].(json.Number)
		v, err := n.Int64()
		if err != nil {
			_, _ = io.WriteString(w, `{"results":[{"error":"value is not an integer"}]}`)
			return
		}
		f.store.mu.Lock()
		f.store.m[k] = v
		f.store.mu.Unlock()
		f.mu.Lock()
		f.applied++
		f.mu.Unlock()
		_, _ = io.WriteString(w, `{"results":[{"last_insert_id":1,"rows_affected":1}]}`)
	case "/db/query":
		if queryBody != "" {
			_, _ = io.WriteString(w, queryBody)
			return
		}
		k := ""
		if len(params) > 0 {
			k, _ = params[0].(string)
		}
		f.store.mu.Lock()
		v, ok := f.store.m[k]
		f.store.mu.Unlock()
		if !ok {
			_, _ = io.WriteString(w, `{"results":[{"columns":["v"],"types":["integer"]}]}`)
			return
		}
		fmt.Fprintf(w, `{"results":[{"columns":["v"],"types":["integer"],"values":[[%d]]}]}`, v)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakeRqlite) snapshot() (requests, applied int, log []reqLog) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests, f.applied, append([]reqLog(nil), f.log...)
}

// writePlan writes a PROFILE plan (prothesis.driver_plan/v1) the way the
// harness does: clients, ops and a mix of weight_ppm entries.
func writePlan(t *testing.T, dir, profile string, clients, ops int, mix map[string]int64) string {
	t.Helper()
	type entry struct {
		Op        string `json:"op"`
		WeightPPM int64  `json:"weight_ppm"`
	}
	var m []entry
	for op, w := range mix {
		m = append(m, entry{op, w})
	}
	doc := map[string]any{"schema": "prothesis.driver_plan/v1", "profile": profile, "seed": 1,
		"history_path": "x", "clients": clients, "ops": ops, "mix": m}
	b, _ := json.Marshal(doc)
	p := filepath.Join(dir, profile+"-"+strconv.Itoa(len(b))+".json")
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func baseArgs(hist, plan, targets string) []string {
	return []string{"--history", hist, "--seed", "7", "--plan", plan, "--targets", targets,
		"--expect-version", pinnedVersion, "--keys", "4", "--timeout", "2s"}
}

// line is one history record as the driver writes it, meta included.
type line struct {
	schema.HistoryEntry
	Meta struct {
		Target     string `json:"target"`
		ReadMode   string `json:"read_mode"`
		RedirectTo string `json:"redirect_to"`
	} `json:"meta"`
}

// readHistory decodes every line through the PUBLIC schema reader and
// validates it, exactly as the harness does, and also keeps the meta.
func readHistory(t *testing.T, path string) []line {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read history: %v", err)
	}
	hr := schema.NewHistoryReader(bytes.NewReader(raw))
	for {
		e, err := hr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("history line %d: %v", hr.LineNo(), err)
		}
		if err := e.Validate(); err != nil {
			t.Fatalf("history line %d is not a valid record: %v", hr.LineNo(), err)
		}
	}
	var out []line
	for _, l := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if l == "" {
			continue
		}
		var r line
		if err := json.Unmarshal([]byte(l), &r); err != nil {
			t.Fatalf("history line %q: %v", l, err)
		}
		out = append(out, r)
	}
	return out
}

// pairs asserts every op_id has exactly one invoke and one completion with the
// same f and key, and returns them.
func pairs(t *testing.T, hist []line) map[int64][2]*line {
	t.Helper()
	byID := map[int64][2]*line{}
	for i := range hist {
		e := &hist[i]
		if e.OpID == nil {
			t.Fatalf("record %d carries no op_id", i)
		}
		p := byID[*e.OpID]
		if e.Type == schema.HistoryInvoke {
			if p[0] != nil {
				t.Fatalf("op_id %d has two invokes", *e.OpID)
			}
			p[0] = e
		} else {
			if p[1] != nil {
				t.Fatalf("op_id %d has two completions", *e.OpID)
			}
			p[1] = e
		}
		byID[*e.OpID] = p
	}
	for id, p := range byID {
		if p[0] == nil || p[1] == nil {
			t.Fatalf("op_id %d is not a complete invoke/completion pair", id)
		}
		if p[0].F != p[1].F || *p[0].Key != *p[1].Key {
			t.Fatalf("op_id %d: invoke %s %s, completion %s %s", id, p[0].F, *p[0].Key, p[1].F, *p[1].Key)
		}
	}
	return byID
}

func completions(t *testing.T, hist []line, f string) []*line {
	t.Helper()
	var out []*line
	for _, p := range pairs(t, hist) {
		if p[1].F == f {
			out = append(out, p[1])
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Classification and contract tests
// ---------------------------------------------------------------------------

func TestAWriteWithAnErrorBodyIsNotOk(t *testing.T) {
	cases := map[string]struct{ body, want string }{
		// http/service.go execute(): a non-leader error from the proxy is
		// written as resp.Error under HTTP 200.
		"top-level error": {`{"results":[],"error":"leadership lost while committing log"}`, "leadership lost while committing log"},
		// command/encoding/json.go Result.Error: one statement failed.
		"per-statement error": {`{"results":[{"error":"UNIQUE constraint failed: kv.k"}]}`, "UNIQUE constraint failed"},
		// No results at all is not a success either.
		"no results": {`{"results":[]}`, "result"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			fk := newFake(newStore())
			fk.execBody = c.body
			srv := httptest.NewServer(fk)
			defer srv.Close()
			dir := t.TempDir()
			hist := filepath.Join(dir, "history.jsonl")
			plan := writePlan(t, dir, "w", 2, 12, map[string]int64{"write": 1000000})
			code, err := run(baseArgs(hist, plan, "rq-n1="+srv.URL), strings.NewReader(""), io.Discard)
			if err != nil || code != exitOK {
				t.Fatalf("run: code=%d err=%v", code, err)
			}
			writes := completions(t, readHistory(t, hist), "write")
			if len(writes) == 0 {
				t.Fatal("no write was recorded")
			}
			for _, w := range writes {
				if w.Type == schema.HistoryOK {
					t.Fatalf("op_id %d: HTTP 200 with %s was recorded ok", *w.OpID, c.body)
				}
				if !strings.Contains(w.Error, c.want) {
					t.Fatalf("op_id %d: error %q does not carry rqlite's %q", *w.OpID, w.Error, c.want)
				}
			}
		})
	}
}

func TestAWriteUnknownAfterSendIsInfo(t *testing.T) {
	type knob func(*fakeRqlite)
	cases := map[string]knob{
		// The leader accepted the request and has not answered within the
		// client's timeout: it may still commit.
		"timeout after send": func(f *fakeRqlite) { f.stall = 1500 * time.Millisecond },
		// The request was written and the connection died before an answer.
		"connection dropped after send": func(f *fakeRqlite) { f.hangup = true },
		// A status that is not 200 or the leader redirect says nothing about
		// whether the statement was applied.
		"503 after send": func(f *fakeRqlite) { f.status = http.StatusServiceUnavailable },
		"500 after send": func(f *fakeRqlite) { f.status = http.StatusInternalServerError },
		// HTTP 200 whose error does not say the statement was refused.
		"200 with an error": func(f *fakeRqlite) {
			f.execBody = `{"results":[],"error":"leadership lost while committing log"}`
		},
	}
	for name, set := range cases {
		t.Run(name, func(t *testing.T) {
			fk := newFake(newStore())
			set(fk)
			srv := httptest.NewServer(fk)
			defer srv.Close()
			dir := t.TempDir()
			hist := filepath.Join(dir, "history.jsonl")
			plan := writePlan(t, dir, "w", 2, 4, map[string]int64{"write": 1000000})
			args := append(baseArgs(hist, plan, "rq-n1="+srv.URL), "--timeout", "500ms")
			code, err := run(args, strings.NewReader(""), io.Discard)
			if err != nil || code != exitOK {
				t.Fatalf("run: code=%d err=%v", code, err)
			}
			writes := completions(t, readHistory(t, hist), "write")
			if len(writes) == 0 {
				t.Fatal("no write was recorded")
			}
			for _, w := range writes {
				if w.Type != schema.HistoryInfo {
					t.Fatalf("op_id %d: a write whose outcome is unknown after it was sent was recorded %s (%q), want info",
						*w.OpID, w.Type, w.Error)
				}
			}
		})
	}
}

func TestAWriteRefusedBeforeSendIsFail(t *testing.T) {
	// A real dial to a port nothing listens on: the error net/http returns on
	// this host, not one constructed by hand.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := "http://" + ln.Addr().String()
	_ = ln.Close()
	att := send(newHTTPClient(), dead+"/db/execute?redirect", []byte(`[["SELECT 1"]]`), time.Second)
	if att.err == nil {
		t.Fatalf("expected a dial error against %s", dead)
	}
	out := classifyWrite(att, pinnedVersion, nil)
	if out.typ != string(schema.HistoryFail) {
		t.Fatalf("connection refused before any byte was sent: got %s (%q), want fail", out.typ, out.label)
	}

	// The same rule over the error shapes a refusal can take.
	refused := &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}
	noHost := &net.DNSError{Err: "no such host", Name: "rq-n9", IsNotFound: true}
	for name, a := range map[string]attempt{
		"dial refused": {err: refused},
		"no such host": {err: noHost},
	} {
		if got := classifyWrite(a, pinnedVersion, nil); got.typ != string(schema.HistoryFail) {
			t.Fatalf("%s: got %s, want fail", name, got.typ)
		}
	}
	// And the boundary: once the request was written, the same error is info.
	if got := classifyWrite(attempt{wroteRequest: true, err: errors.New("connection reset by peer")}, pinnedVersion, nil); got.typ != string(schema.HistoryInfo) {
		t.Fatalf("an error after the request was written: got %s, want info", got.typ)
	}
}

func TestAReadWithAStatementErrorIsNotOk(t *testing.T) {
	cases := map[string]struct{ body, want string }{
		"per-statement error": {`{"results":[{"error":"no such table: kv"}]}`, "no such table: kv"},
		"top-level error":     {`{"results":[],"error":"not leader"}`, "not leader"},
		"two rows":            {`{"results":[{"columns":["v"],"types":["integer"],"values":[[1],[2]]}]}`, "row"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			fk := newFake(newStore())
			fk.queryBody = c.body
			srv := httptest.NewServer(fk)
			defer srv.Close()
			dir := t.TempDir()
			hist := filepath.Join(dir, "history.jsonl")
			plan := writePlan(t, dir, "r", 2, 20, map[string]int64{"read_linearizable": 1000000})
			code, err := run(baseArgs(hist, plan, "rq-n1="+srv.URL), strings.NewReader(""), io.Discard)
			if err != nil || code != exitOK {
				t.Fatalf("run: code=%d err=%v", code, err)
			}
			reads := completions(t, readHistory(t, hist), "read")
			if len(reads) == 0 {
				t.Fatal("no read was recorded")
			}
			for _, r := range reads {
				if r.Type == schema.HistoryOK {
					t.Fatalf("op_id %d: a read answered %s was recorded ok with value %s", *r.OpID, c.body, r.Value)
				}
				if !strings.Contains(r.Error, c.want) {
					t.Fatalf("op_id %d: error %q does not carry %q", *r.OpID, r.Error, c.want)
				}
			}
		})
	}
}

func TestAnUnknownReadLevelIsRefusedAtStart(t *testing.T) {
	for _, op := range []string{"read_bogus", "read", "read_auto", "read_Linearizable"} {
		t.Run(op, func(t *testing.T) {
			fk := newFake(newStore())
			srv := httptest.NewServer(fk)
			defer srv.Close()
			dir := t.TempDir()
			hist := filepath.Join(dir, "history.jsonl")
			plan := writePlan(t, dir, "x", 2, 20, map[string]int64{op: 500000, "write": 500000})
			code, err := run(baseArgs(hist, plan, "rq-n1="+srv.URL), strings.NewReader(""), io.Discard)
			if code != exitConfig || err == nil {
				t.Fatalf("mix entry %q: code=%d err=%v; want a config refusal", op, code, err)
			}
			if _, statErr := os.Stat(hist); statErr == nil {
				t.Fatalf("mix entry %q: a refused start must not leave a history behind", op)
			}
			if n, _, _ := fk.snapshot(); n != 0 {
				t.Fatalf("mix entry %q: the driver sent %d request(s) before refusing", op, n)
			}
		})
	}
}

func TestTheReadLevelSentIsRecorded(t *testing.T) {
	for _, level := range []string{"linearizable", "strong", "weak", "none"} {
		t.Run(level, func(t *testing.T) {
			fk := newFake(newStore())
			srv := httptest.NewServer(fk)
			defer srv.Close()
			dir := t.TempDir()
			hist := filepath.Join(dir, "history.jsonl")
			plan := writePlan(t, dir, level, 3, 60, map[string]int64{"read_" + level: 500000, "write": 500000})
			code, err := run(baseArgs(hist, plan, "rq-n1="+srv.URL), strings.NewReader(""), io.Discard)
			if err != nil || code != exitOK {
				t.Fatalf("run: code=%d err=%v", code, err)
			}
			h := readHistory(t, hist)
			var readRecords int
			for _, r := range h {
				if r.F != "read" {
					continue
				}
				readRecords++
				if r.Meta.ReadMode != level {
					t.Fatalf("op_id %d (%s): meta.read_mode %q, want %q", *r.OpID, r.Type, r.Meta.ReadMode, level)
				}
			}
			_, _, log := fk.snapshot()
			var queries int
			for _, l := range log {
				if l.path != "/db/query" {
					continue
				}
				queries++
				q, err := parseQuery(l.rawQuery)
				if err != nil {
					t.Fatal(err)
				}
				if got := q["level"]; got != level {
					t.Fatalf("the node was asked level %q (query %q); the history says %q", got, l.rawQuery, level)
				}
			}
			if queries == 0 || readRecords != 2*queries {
				t.Fatalf("%d query request(s) reached the node; the history holds %d read record(s), want two per request",
					queries, readRecords)
			}
		})
	}
}

func parseQuery(raw string) (map[string]string, error) {
	out := map[string]string{}
	for _, kv := range strings.Split(raw, "&") {
		if kv == "" {
			continue
		}
		k, v, _ := strings.Cut(kv, "=")
		out[k] = v
	}
	return out, nil
}

func TestAVersionHeaderMismatchEndsTheRun(t *testing.T) {
	t.Run("from the first response", func(t *testing.T) {
		fk := newFake(newStore())
		fk.version = "v10.5.2"
		srv := httptest.NewServer(fk)
		defer srv.Close()
		dir := t.TempDir()
		hist := filepath.Join(dir, "history.jsonl")
		plan := writePlan(t, dir, "v", 2, 20, map[string]int64{"read_strong": 500000, "write": 500000})
		code, err := run(baseArgs(hist, plan, "rq-n1="+srv.URL), strings.NewReader(""), io.Discard)
		if code == exitOK || err == nil {
			t.Fatalf("code=%d err=%v; a node reporting v10.5.2 against --expect-version %s must end the run non-zero",
				code, err, pinnedVersion)
		}
		if !strings.Contains(err.Error(), "v10.5.2") || !strings.Contains(err.Error(), pinnedVersion) {
			t.Fatalf("the error must name both versions: %v", err)
		}
	})
	t.Run("no header at all", func(t *testing.T) {
		fk := newFake(newStore())
		fk.version = ""
		srv := httptest.NewServer(fk)
		defer srv.Close()
		dir := t.TempDir()
		hist := filepath.Join(dir, "history.jsonl")
		plan := writePlan(t, dir, "v", 2, 20, map[string]int64{"read_strong": 500000, "write": 500000})
		code, err := run(baseArgs(hist, plan, "rq-n1="+srv.URL), strings.NewReader(""), io.Discard)
		if code == exitOK || err == nil {
			t.Fatalf("code=%d err=%v; a response with no version header must end the run non-zero", code, err)
		}
	})
	t.Run("mid-run", func(t *testing.T) {
		fk := newFake(newStore())
		fk.switchAfter = 25
		fk.altVersion = "v10.6.0"
		srv := httptest.NewServer(fk)
		defer srv.Close()
		dir := t.TempDir()
		hist := filepath.Join(dir, "history.jsonl")
		plan := writePlan(t, dir, "v", 2, 400, map[string]int64{"read_strong": 500000, "write": 500000})
		code, err := run(baseArgs(hist, plan, "rq-n1="+srv.URL), strings.NewReader(""), io.Discard)
		if code == exitOK || err == nil {
			t.Fatalf("code=%d err=%v; the version changing mid-run must end the run non-zero", code, err)
		}
		h := readHistory(t, hist)
		byID := pairs(t, h) // nothing left in flight
		if len(byID) >= 400 {
			t.Fatalf("the driver ran its whole budget (%d ops) after the version changed", len(byID))
		}
		var flagged int
		for _, p := range byID {
			if strings.Contains(p[1].Error, "v10.6.0") {
				flagged++
				if p[1].Type == schema.HistoryOK {
					t.Fatalf("op_id %d: an answer from the wrong version was recorded ok", *p[1].OpID)
				}
			}
		}
		if flagged == 0 {
			t.Fatal("no completion says which version answered")
		}
	})
}

func TestWritesAreSentToTheLeaderOnly(t *testing.T) {
	st := newStore()
	follower := newFake(st)
	// The Location names the leader's advertised address, a container host
	// name the driver cannot resolve. Following it would dial rq-n2:4001.
	follower.redirectTo = "rq-n2:4001"
	leader := newFake(st)
	fsrv := httptest.NewServer(follower)
	defer fsrv.Close()
	lsrv := httptest.NewServer(leader)
	defer lsrv.Close()

	dir := t.TempDir()
	hist := filepath.Join(dir, "history.jsonl")
	plan := writePlan(t, dir, "l", 3, 60, map[string]int64{"write": 700000, "read_linearizable": 300000})
	code, err := run(baseArgs(hist, plan, "rq-n1="+fsrv.URL+",rq-n2="+lsrv.URL), strings.NewReader(""), io.Discard)
	if err != nil || code != exitOK {
		t.Fatalf("run: code=%d err=%v", code, err)
	}

	_, fApplied, fLog := follower.snapshot()
	_, lApplied, lLog := leader.snapshot()
	for _, l := range append(fLog, lLog...) {
		if l.path != "/db/execute" {
			continue
		}
		q, _ := parseQuery(l.rawQuery)
		if _, ok := q["redirect"]; !ok {
			t.Fatalf("an execute request was sent without ?redirect (query %q): a follower would forward it, "+
				"and rqlite retries a forwarded write", l.rawQuery)
		}
	}
	if fApplied != 0 {
		t.Fatalf("the follower applied %d write(s)", fApplied)
	}
	writes := completions(t, readHistory(t, hist), "write")
	var ok, toFollower int
	for _, w := range writes {
		// A write that reached the follower was refused there, with positive
		// evidence, and the 301 was mapped to rq-n2 rather than followed. A
		// followed redirect shows up here as anything else: a dial or lookup
		// error, or a timeout, recorded against rq-n1.
		if w.Meta.Target == "rq-n1" {
			toFollower++
			if w.Type != schema.HistoryFail || w.Meta.RedirectTo != "rq-n2" {
				t.Fatalf("op_id %d: a write sent to the follower: got %s %q redirect_to %q, want fail mapped to rq-n2",
					*w.OpID, w.Type, w.Error, w.Meta.RedirectTo)
			}
		}
		if w.Type == schema.HistoryOK {
			ok++
			if w.Meta.Target != "rq-n2" {
				t.Fatalf("op_id %d: an ok write was served by %q, not the leader rq-n2", *w.OpID, w.Meta.Target)
			}
		}
		if strings.Contains(w.Error, "no such host") || strings.Contains(w.Error, "rq-n2:4001") && w.Type == schema.HistoryInfo {
			t.Fatalf("op_id %d: the driver followed the redirect: %q", *w.OpID, w.Error)
		}
	}
	if toFollower == 0 {
		t.Fatal("no write reached the follower, so the redirect path was not exercised")
	}
	if ok == 0 || ok != lApplied {
		t.Fatalf("ok writes %d, writes the leader applied %d: the redirect was not mapped back to rq-n2", ok, lApplied)
	}

	// A redirect to a node this world does not have is refused, not followed.
	stray := newFake(newStore())
	stray.redirectTo = "10.9.9.9:4001"
	ssrv := httptest.NewServer(stray)
	defer ssrv.Close()
	hist2 := filepath.Join(dir, "history2.jsonl")
	plan2 := writePlan(t, dir, "s", 1, 5, map[string]int64{"write": 1000000})
	code, err = run(baseArgs(hist2, plan2, "rq-n1="+ssrv.URL), strings.NewReader(""), io.Discard)
	if err != nil || code != exitOK {
		t.Fatalf("run: code=%d err=%v", code, err)
	}
	for _, w := range completions(t, readHistory(t, hist2), "write") {
		if w.Type != schema.HistoryFail || !strings.Contains(w.Error, "10.9.9.9:4001") {
			t.Fatalf("op_id %d: a 301 to an unknown node: got %s %q, want fail naming the address", *w.OpID, w.Type, w.Error)
		}
	}
}

func TestTheDriverReadsNoWorkloadFromTheEnvironment(t *testing.T) {
	fk := newFake(newStore())
	srv := httptest.NewServer(fk)
	defer srv.Close()
	dir := t.TempDir()

	// A hostile environment: every name a driver might plausibly read a
	// workload from, pointing at a different workload.
	hostile := writePlan(t, dir, "none", 1, 3, map[string]int64{"read_none": 1000000})
	for k, v := range map[string]string{
		"PROTHESIS_PLAN_PATH":      hostile,
		"PROTHESIS_PROFILE":        "none",
		"PROTHESIS_OPS":            "3",
		"PROTHESIS_CLIENTS":        "1",
		"PROTHESIS_KEYS":           "1",
		"PROTHESIS_STDIN_CONTROL":  "1",
		"PROTHESIS_EXPECT_VERSION": "v0.0.0",
		"RQLITE_LEVEL":             "none",
	} {
		t.Setenv(k, v)
	}

	// 1. With an explicit plan, the workload is the plan's, whatever the
	// environment says.
	hist := filepath.Join(dir, "history.jsonl")
	plan := writePlan(t, dir, "strong", 2, 40, map[string]int64{"read_strong": 500000, "write": 500000})
	code, err := run(baseArgs(hist, plan, "rq-n1="+srv.URL), strings.NewReader(""), io.Discard)
	if err != nil || code != exitOK {
		t.Fatalf("run: code=%d err=%v", code, err)
	}
	byID := pairs(t, readHistory(t, hist))
	if len(byID) != 40 {
		t.Fatalf("ran %d operations; the plan says 40", len(byID))
	}
	for _, p := range byID {
		if p[0].F == "read" && p[0].Meta.ReadMode != "strong" {
			t.Fatalf("op_id %d read at %q; the plan says strong", *p[0].OpID, p[0].Meta.ReadMode)
		}
	}

	// 2. With no --plan, the driver refuses rather than pick one up from
	// PROTHESIS_PLAN_PATH.
	hist2 := filepath.Join(dir, "history2.jsonl")
	args := []string{"--history", hist2, "--seed", "7", "--targets", "rq-n1=" + srv.URL, "--expect-version", pinnedVersion}
	code, err = run(args, strings.NewReader(""), io.Discard)
	if code != exitConfig || err == nil {
		t.Fatalf("no --plan: code=%d err=%v; want a config refusal", code, err)
	}
	if _, statErr := os.Stat(hist2); statErr == nil {
		t.Fatal("no --plan: a refused start must not leave a history behind")
	}

	// 3. The source reads exactly one variable, the topology the harness
	// exports, and nothing else.
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`os\.(Getenv|LookupEnv|Environ|ExpandEnv)\(([^)]*)\)`)
	for _, m := range re.FindAllStringSubmatch(string(src), -1) {
		if m[1] != "Getenv" || strings.TrimSpace(m[2]) != "envTargets" {
			t.Fatalf("main.go reads the environment through %s: only os.Getenv(envTargets) is allowed", m[0])
		}
	}
}
