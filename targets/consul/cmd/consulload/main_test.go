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
	"net/url"
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

// The response shapes below are Consul v2.0.4's, read from its source at tag
// v2.0.4 rather than from a live node, so these tests could fail before the
// driver existed:
//
//   - agent/kvs_endpoint.go KVSPut (lines 186-294): a plain PUT answers the
//     JSON literal `true` once KVS.Apply returns (290-291); a request with no
//     body answers 200 with a JSON STRING, "Request has no content-length & no
//     body" (246-247), which is not a success.
//   - agent/kvs_endpoint.go KVSGet (84-137): setMeta, then 404 with an empty
//     body when there is no entry (110-115); otherwise a JSON array of
//     structs.DirEntry (agent/structs/structs.go 2795-2804), whose Value is a
//     []byte and so base64 in JSON.
//   - agent/http.go wrap/handleErr (514-563): an RPC error is a text/plain body
//     carrying err.Error(), HTTP 500 unless it is a permission, rate-limit or
//     HTTPError case. setMeta (841-853) sets X-Consul-Index,
//     X-Consul-KnownLeader and X-Consul-LastContact always, and
//     X-Consul-Effective-Consistency only when the query meta names a level
//     (setConsistency, 819-823); the fake never sends it. parseConsistency
//     (986-1041) reads ?stale and ?consistent by presence.
//   - agent/agent_endpoint.go AgentSelf (72-156): {"Config":{...,"Version":
//     "2.0.4",...},...}; Version is VersionWithMetadata (agent/config/
//     runtime.go 1840-1846), with no leading "v".
//   - agent/structs/errors.go:12: ErrNoLeader is "No cluster leader".

const pinnedVersion = "2.0.4"

// kvStore is the register state behind one or more fake nodes.
type kvStore struct {
	mu sync.Mutex
	m  map[string][]byte
}

func newStore() *kvStore { return &kvStore{m: map[string][]byte{}} }

// reqLog is what a fake node saw of one request.
type reqLog struct {
	method   string
	path     string
	rawQuery string
}

// fakeConsul speaks the slice of Consul's HTTP API the driver uses, with the
// shapes listed at the top of this file. It serialises every request, so any
// history it produces is linearizable; the tests are about the DRIVER.
type fakeConsul struct {
	mu    sync.Mutex
	store *kvStore

	version string // Config.Version in /v1/agent/self; "" omits it

	putBody   string        // when set, PUTs answer 200 with this body
	putStatus int           // when set, PUTs answer this status with putErr as the body
	putErr    string        //
	getBody   string        // when set, GETs answer 200 with this body
	bare404   bool          // an absent key answers 404 without Consul's meta headers
	stall     time.Duration // PUTs sleep this long before answering
	hangup    bool          // PUTs are read, then the connection is dropped

	requests int
	kv       int
	applied  int
	log      []reqLog
}

func newFake(st *kvStore) *fakeConsul {
	return &fakeConsul{store: st, version: pinnedVersion}
}

func (f *fakeConsul) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)

	f.mu.Lock()
	f.requests++
	if strings.HasPrefix(r.URL.Path, "/v1/kv/") {
		f.kv++
	}
	f.log = append(f.log, reqLog{method: r.Method, path: r.URL.Path, rawQuery: r.URL.RawQuery})
	ver, putBody, putStatus, putErr, getBody, bare404, stall, hangup :=
		f.version, f.putBody, f.putStatus, f.putErr, f.getBody, f.bare404, f.stall, f.hangup
	f.mu.Unlock()

	switch {
	case r.URL.Path == "/v1/agent/self" && r.Method == http.MethodGet:
		cfg := map[string]any{"Datacenter": "dc1", "PrimaryDatacenter": "dc1", "NodeName": "consul-n1",
			"NodeID": "6f1c2a2e-0000-0000-0000-000000000001", "Revision": "a3eb721c", "Server": true,
			"BuildDate": "2026-09-09T17:52:35Z", "BindAddr": "172.20.0.2"}
		if ver != "" {
			cfg["Version"] = ver
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"Config": cfg, "DebugConfig": map[string]any{},
			"Stats": map[string]any{}, "Meta": map[string]any{}})
	case strings.HasPrefix(r.URL.Path, "/v1/kv/") && r.Method == http.MethodPut:
		key := strings.TrimPrefix(r.URL.Path, "/v1/kv/")
		if stall > 0 {
			time.Sleep(stall)
		}
		if hangup {
			if hj, ok := w.(http.Hijacker); ok {
				if conn, _, err := hj.Hijack(); err == nil {
					_ = conn.Close()
				}
			}
			return
		}
		if putStatus != 0 {
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(putStatus)
			_, _ = io.WriteString(w, putErr)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if putBody != "" {
			_, _ = io.WriteString(w, putBody)
			return
		}
		f.store.mu.Lock()
		f.store.m[key] = append([]byte(nil), body...)
		f.store.mu.Unlock()
		f.mu.Lock()
		f.applied++
		f.mu.Unlock()
		_, _ = io.WriteString(w, "true")
	case strings.HasPrefix(r.URL.Path, "/v1/kv/") && r.Method == http.MethodGet:
		key := strings.TrimPrefix(r.URL.Path, "/v1/kv/")
		f.store.mu.Lock()
		v, ok := f.store.m[key]
		f.store.mu.Unlock()
		if !(bare404 && !ok) {
			w.Header().Set("X-Consul-Index", "17")
			w.Header().Set("X-Consul-KnownLeader", "true")
			w.Header().Set("X-Consul-LastContact", "0")
		}
		if getBody != "" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, getBody)
			return
		}
		if !ok {
			if bare404 {
				http.NotFound(w, r)
				return
			}
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]map[string]any{{"LockIndex": 0, "Key": key, "Flags": 0,
			"Value": v, "CreateIndex": 12, "ModifyIndex": 17}})
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeConsul) snapshot() (requests, kv, applied int, log []reqLog) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests, f.kv, f.applied, append([]reqLog(nil), f.log...)
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
		"--expect-version", pinnedVersion, "--keys", "4", "--timeout", "2s", "--setup-timeout", "3s"}
}

// line is one history record as the driver writes it, meta included.
type line struct {
	schema.HistoryEntry
	Meta struct {
		Target      string `json:"target"`
		ReadMode    string `json:"read_mode"`
		KnownLeader string `json:"known_leader"`
		LastContact string `json:"last_contact"`
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

// runOK runs the driver against one fake node and returns its history.
func runOK(t *testing.T, fk *fakeConsul, clients, ops int, mix map[string]int64, extra ...string) []line {
	t.Helper()
	srv := httptest.NewServer(fk)
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	hist := filepath.Join(dir, "history.jsonl")
	plan := writePlan(t, dir, "p", clients, ops, mix)
	args := append(baseArgs(hist, plan, "consul-n1="+srv.URL), extra...)
	code, err := run(args, strings.NewReader(""), io.Discard)
	if err != nil || code != exitOK {
		t.Fatalf("run: code=%d err=%v", code, err)
	}
	return readHistory(t, hist)
}

// ---------------------------------------------------------------------------
// Classification and contract tests
// ---------------------------------------------------------------------------

func TestAWriteAnsweredFalseIsNotOk(t *testing.T) {
	cases := map[string]string{
		// KVSPut returns `out` only for CAS, lock and unlock; for a plain set
		// it answers true. A false is therefore not this API's success.
		"false": "false",
		// kvs_endpoint.go:246-247: HTTP 200 with a JSON string.
		"a JSON string": `"Request has no content-length & no body"`,
		// An empty 200 says nothing either.
		"an empty body": " ",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			fk := newFake(newStore())
			fk.putBody = body
			writes := completions(t, runOK(t, fk, 2, 12, map[string]int64{"write": 1000000}), "write")
			if len(writes) == 0 {
				t.Fatal("no write was recorded")
			}
			for _, w := range writes {
				if w.Type == schema.HistoryOK {
					t.Fatalf("op_id %d: HTTP 200 with body %q was recorded ok", *w.OpID, body)
				}
				if w.Type != schema.HistoryInfo {
					t.Fatalf("op_id %d: HTTP 200 with body %q was recorded %s; the request was sent and may have applied, want info",
						*w.OpID, body, w.Type)
				}
			}
		})
	}
}

func TestAWriteUnknownAfterSendIsInfo(t *testing.T) {
	type knob func(*fakeConsul)
	cases := map[string]knob{
		// The node accepted the request and has not answered within the
		// client's timeout: it may still commit.
		"timeout after send": func(f *fakeConsul) { f.stall = 1500 * time.Millisecond },
		// The request was written and the connection died before an answer.
		"connection dropped after send": func(f *fakeConsul) { f.hangup = true },
		// raftApply's future failed after the entry was appended
		// (agent/consul/kvs_endpoint.go:129-132): it may still commit.
		"500 leadership lost": func(f *fakeConsul) {
			f.putStatus, f.putErr = http.StatusInternalServerError, "raft apply failed: leadership lost while committing log"
		},
		// A forward to the leader whose answer was lost.
		"500 forward EOF": func(f *fakeConsul) {
			f.putStatus, f.putErr = http.StatusInternalServerError, "rpc error making call: EOF"
		},
		"503 after send": func(f *fakeConsul) { f.putStatus, f.putErr = http.StatusServiceUnavailable, "retry later" },
	}
	for name, set := range cases {
		t.Run(name, func(t *testing.T) {
			fk := newFake(newStore())
			set(fk)
			writes := completions(t, runOK(t, fk, 2, 4, map[string]int64{"write": 1000000}, "--timeout", "500ms"), "write")
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
	att := send(newHTTPClient(), http.MethodPut, dead+"/v1/kv/k/0", []byte("1"), time.Second)
	if att.err == nil {
		t.Fatalf("expected a dial error against %s", dead)
	}
	if out := classifyWrite(att); out.typ != string(schema.HistoryFail) {
		t.Fatalf("connection refused before any byte was sent: got %s (%q), want fail", out.typ, out.label)
	}

	// The same rule over the error shapes a refusal can take.
	refused := &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}
	noHost := &net.DNSError{Err: "no such host", Name: "consul-n9", IsNotFound: true}
	for name, a := range map[string]attempt{
		"dial refused": {err: refused},
		"no such host": {err: noHost},
	} {
		if got := classifyWrite(a); got.typ != string(schema.HistoryFail) {
			t.Fatalf("%s: got %s, want fail", name, got.typ)
		}
	}

	// Refused before Raft apply: ErrNoLeader is produced only by
	// forwardRequestToLeader before any handler runs (agent/consul/rpc.go
	// 795-799 and 855-858), locally or wrapped by the forwarding hop's pool
	// (agent/pool/pool.go:666).
	for _, body := range []string{"No cluster leader", "rpc error making call: No cluster leader"} {
		a := attempt{wroteRequest: true, gotResponse: true, status: http.StatusInternalServerError, body: []byte(body)}
		if got := classifyWrite(a); got.typ != string(schema.HistoryFail) {
			t.Fatalf("500 %q: got %s (%q), want fail", body, got.typ, got.label)
		}
	}

	// And the boundary: once the request was written, the same transport
	// error is info, and so is a 500 that is not the no-leader refusal.
	if got := classifyWrite(attempt{wroteRequest: true, err: errors.New("connection reset by peer")}); got.typ != string(schema.HistoryInfo) {
		t.Fatalf("an error after the request was written: got %s, want info", got.typ)
	}
	a := attempt{wroteRequest: true, gotResponse: true, status: http.StatusInternalServerError,
		body: []byte("raft apply failed: leadership lost while committing log")}
	if got := classifyWrite(a); got.typ != string(schema.HistoryInfo) {
		t.Fatalf("500 leadership lost: got %s, want info", got.typ)
	}
}

func TestANotFoundReadIsTheAbsentValue(t *testing.T) {
	// Every seed write is refused (no leader), so every key stays absent.
	fk := newFake(newStore())
	fk.putStatus, fk.putErr = http.StatusInternalServerError, "No cluster leader"
	reads := completions(t, runOK(t, fk, 2, 24, map[string]int64{"read_default": 1000000}), "read")
	if len(reads) == 0 {
		t.Fatal("no read was recorded")
	}
	for _, r := range reads {
		if r.Type != schema.HistoryOK || string(r.Value) != "null" {
			t.Fatalf("op_id %d: a 404 from KVSGet was recorded %s value %s (%q), want ok null",
				*r.OpID, r.Type, r.Value, r.Error)
		}
	}

	// A 404 without Consul's meta headers did not come from KVSGet (an
	// unknown route, a proxy): it is not evidence the key is absent.
	fk2 := newFake(newStore())
	fk2.putStatus, fk2.putErr = http.StatusInternalServerError, "No cluster leader"
	fk2.bare404 = true
	for _, r := range completions(t, runOK(t, fk2, 2, 12, map[string]int64{"read_default": 1000000}), "read") {
		if r.Type == schema.HistoryOK {
			t.Fatalf("op_id %d: a bare 404 was recorded ok with value %s", *r.OpID, r.Value)
		}
	}
}

func TestANonIntegerValueIsNotOk(t *testing.T) {
	enc := func(s string) string { return `"` + b64(s) + `"` }
	cases := map[string]string{
		"not a number":  `[{"LockIndex":0,"Key":"k/0","Flags":0,"Value":` + enc("abc") + `,"CreateIndex":3,"ModifyIndex":3}]`,
		"a fraction":    `[{"LockIndex":0,"Key":"k/0","Flags":0,"Value":` + enc("1.5") + `,"CreateIndex":3,"ModifyIndex":3}]`,
		"a null value":  `[{"LockIndex":0,"Key":"k/0","Flags":0,"Value":null,"CreateIndex":3,"ModifyIndex":3}]`,
		"not base64":    `[{"LockIndex":0,"Key":"k/0","Flags":0,"Value":"%%%","CreateIndex":3,"ModifyIndex":3}]`,
		"two entries":   `[{"Key":"k/0","Value":` + enc("1") + `},{"Key":"k/0","Value":` + enc("2") + `}]`,
		"another key":   `[{"LockIndex":0,"Key":"other","Flags":0,"Value":` + enc("1") + `,"CreateIndex":3,"ModifyIndex":3}]`,
		"not an array":  `{"Key":"k/0","Value":` + enc("1") + `}`,
		"an empty list": `[]`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			fk := newFake(newStore())
			fk.getBody = body
			reads := completions(t, runOK(t, fk, 2, 24, map[string]int64{"read_stale": 1000000}, "--keys", "1"), "read")
			if len(reads) == 0 {
				t.Fatal("no read was recorded")
			}
			for _, r := range reads {
				if r.Type == schema.HistoryOK {
					t.Fatalf("op_id %d: a read answered %s was recorded ok with value %s", *r.OpID, body, r.Value)
				}
			}
		})
	}
	// The positive control: the same shape with an integer is ok.
	fk := newFake(newStore())
	fk.getBody = `[{"LockIndex":0,"Key":"k/0","Flags":0,"Value":` + enc("3000017") + `,"CreateIndex":3,"ModifyIndex":3}]`
	for _, r := range completions(t, runOK(t, fk, 1, 6, map[string]int64{"read_stale": 1000000}, "--keys", "1"), "read") {
		if r.Type != schema.HistoryOK || string(r.Value) != "3000017" {
			t.Fatalf("op_id %d: an integer value was recorded %s %s (%q)", *r.OpID, r.Type, r.Value, r.Error)
		}
	}
}

func b64(s string) string {
	b, _ := json.Marshal([]byte(s))
	return strings.Trim(string(b), `"`)
}

func TestAnUnknownReadModeIsRefusedAtStart(t *testing.T) {
	for _, op := range []string{"read_bogus", "read", "read_leader", "read_Stale", "read_linearizable", "read_"} {
		t.Run(op, func(t *testing.T) {
			fk := newFake(newStore())
			srv := httptest.NewServer(fk)
			defer srv.Close()
			dir := t.TempDir()
			hist := filepath.Join(dir, "history.jsonl")
			plan := writePlan(t, dir, "x", 2, 20, map[string]int64{op: 500000, "write": 500000})
			code, err := run(baseArgs(hist, plan, "consul-n1="+srv.URL), strings.NewReader(""), io.Discard)
			if code != exitConfig || err == nil {
				t.Fatalf("mix entry %q: code=%d err=%v; want a config refusal", op, code, err)
			}
			if _, statErr := os.Stat(hist); statErr == nil {
				t.Fatalf("mix entry %q: a refused start must not leave a history behind", op)
			}
			if n, _, _, _ := fk.snapshot(); n != 0 {
				t.Fatalf("mix entry %q: the driver sent %d request(s) before refusing", op, n)
			}
		})
	}
}

func TestTheReadModeSentIsRecorded(t *testing.T) {
	for _, mode := range []string{"default", "stale", "consistent"} {
		t.Run(mode, func(t *testing.T) {
			fk := newFake(newStore())
			h := runOK(t, fk, 3, 60, map[string]int64{"read_" + mode: 500000, "write": 500000})
			var readRecords int
			for _, r := range h {
				if r.F != "read" {
					continue
				}
				readRecords++
				if r.Meta.ReadMode != mode {
					t.Fatalf("op_id %d (%s): meta.read_mode %q, want %q", *r.OpID, r.Type, r.Meta.ReadMode, mode)
				}
				if r.Type == schema.HistoryOK && (r.Meta.KnownLeader != "true" || r.Meta.LastContact != "0") {
					t.Fatalf("op_id %d: known_leader %q last_contact %q; the node sent true and 0",
						*r.OpID, r.Meta.KnownLeader, r.Meta.LastContact)
				}
			}
			_, _, _, log := fk.snapshot()
			var gets int
			for _, l := range log {
				if l.method != http.MethodGet || !strings.HasPrefix(l.path, "/v1/kv/") {
					continue
				}
				gets++
				q, err := url.ParseQuery(l.rawQuery)
				if err != nil {
					t.Fatal(err)
				}
				_, stale := q["stale"]
				_, consistent := q["consistent"]
				sent := "default"
				switch {
				case stale && consistent:
					sent = "both"
				case stale:
					sent = "stale"
				case consistent:
					sent = "consistent"
				}
				if sent != mode || len(q) > 1 || (mode == "default" && len(q) != 0) {
					t.Fatalf("the node was asked %q (query %q); the history says %q", sent, l.rawQuery, mode)
				}
			}
			if gets == 0 || readRecords != 2*gets {
				t.Fatalf("%d GET request(s) reached the node; the history holds %d read record(s), want two per request",
					gets, readRecords)
			}
		})
	}
}

func TestAVersionMismatchEndsTheRun(t *testing.T) {
	try := func(t *testing.T, targets string, fakes ...*fakeConsul) (int, error, string) {
		t.Helper()
		dir := t.TempDir()
		hist := filepath.Join(dir, "history.jsonl")
		plan := writePlan(t, dir, "v", 2, 20, map[string]int64{"read_consistent": 500000, "write": 500000})
		code, err := run(baseArgs(hist, plan, targets), strings.NewReader(""), io.Discard)
		if _, statErr := os.Stat(hist); statErr == nil {
			t.Fatal("a run refused over the server version must not leave a history behind")
		}
		for _, f := range fakes {
			if _, kv, _, _ := f.snapshot(); kv != 0 {
				t.Fatalf("the driver sent %d KV request(s) before the version check refused", kv)
			}
		}
		msg := ""
		if err != nil {
			msg = err.Error()
		}
		return code, err, msg
	}
	t.Run("another release", func(t *testing.T) {
		fk := newFake(newStore())
		fk.version = "2.0.3"
		srv := httptest.NewServer(fk)
		defer srv.Close()
		code, err, msg := try(t, "consul-n1="+srv.URL, fk)
		if code == exitOK || err == nil {
			t.Fatalf("code=%d err=%v; a node reporting 2.0.3 against --expect-version %s must end the run non-zero", code, err, pinnedVersion)
		}
		if !strings.Contains(msg, "2.0.3") || !strings.Contains(msg, pinnedVersion) {
			t.Fatalf("the error must name both versions: %v", err)
		}
	})
	t.Run("one node of three", func(t *testing.T) {
		st := newStore()
		a, b, c := newFake(st), newFake(st), newFake(st)
		c.version = "2.1.0-rc2"
		sa, sb, sc := httptest.NewServer(a), httptest.NewServer(b), httptest.NewServer(c)
		defer sa.Close()
		defer sb.Close()
		defer sc.Close()
		code, err, msg := try(t, fmt.Sprintf("consul-n1=%s,consul-n2=%s,consul-n3=%s", sa.URL, sb.URL, sc.URL), a, b, c)
		if code == exitOK || err == nil || !strings.Contains(msg, "consul-n3") {
			t.Fatalf("code=%d err=%v; the odd node out must end the run, by name", code, err)
		}
	})
	t.Run("no version at all", func(t *testing.T) {
		fk := newFake(newStore())
		fk.version = ""
		srv := httptest.NewServer(fk)
		defer srv.Close()
		if code, err, _ := try(t, "consul-n1="+srv.URL, fk); code == exitOK || err == nil {
			t.Fatalf("code=%d err=%v; an agent/self with no Config.Version must end the run non-zero", code, err)
		}
	})
}

func TestTheDriverReadsNoWorkloadFromTheEnvironment(t *testing.T) {
	fk := newFake(newStore())
	srv := httptest.NewServer(fk)
	defer srv.Close()
	dir := t.TempDir()

	// A hostile environment: every name a driver might plausibly read a
	// workload from, pointing at a different workload, including the Consul
	// client library's own variables.
	hostile := writePlan(t, dir, "none", 1, 3, map[string]int64{"read_stale": 1000000})
	for k, v := range map[string]string{
		"PROTHESIS_PLAN_PATH":      hostile,
		"PROTHESIS_PROFILE":        "none",
		"PROTHESIS_OPS":            "3",
		"PROTHESIS_CLIENTS":        "1",
		"PROTHESIS_KEYS":           "1",
		"PROTHESIS_STDIN_CONTROL":  "1",
		"PROTHESIS_EXPECT_VERSION": "0.0.0",
		"CONSUL_HTTP_ADDR":         "127.0.0.1:1",
		"CONSUL_CONSISTENCY":       "stale",
	} {
		t.Setenv(k, v)
	}

	// 1. With an explicit plan, the workload is the plan's, whatever the
	// environment says.
	hist := filepath.Join(dir, "history.jsonl")
	plan := writePlan(t, dir, "consistent", 2, 40, map[string]int64{"read_consistent": 500000, "write": 500000})
	code, err := run(baseArgs(hist, plan, "consul-n1="+srv.URL), strings.NewReader(""), io.Discard)
	if err != nil || code != exitOK {
		t.Fatalf("run: code=%d err=%v", code, err)
	}
	byID := pairs(t, readHistory(t, hist))
	if len(byID) != 40 {
		t.Fatalf("ran %d operations; the plan says 40", len(byID))
	}
	for _, p := range byID {
		if p[0].F == "read" && p[0].Meta.ReadMode != "consistent" {
			t.Fatalf("op_id %d read in mode %q; the plan says consistent", *p[0].OpID, p[0].Meta.ReadMode)
		}
	}

	// 2. With no --plan, the driver refuses rather than pick one up from
	// PROTHESIS_PLAN_PATH.
	hist2 := filepath.Join(dir, "history2.jsonl")
	args := []string{"--history", hist2, "--seed", "7", "--targets", "consul-n1=" + srv.URL, "--expect-version", pinnedVersion}
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
	matches := re.FindAllStringSubmatch(string(src), -1)
	if len(matches) == 0 {
		t.Fatal("main.go reads no environment variable at all; the topology must come from " + "PROTHESIS_TARGETS when --targets is absent")
	}
	for _, m := range matches {
		if m[1] != "Getenv" || strings.TrimSpace(m[2]) != "envTargets" {
			t.Fatalf("main.go reads the environment through %s: only os.Getenv(envTargets) is allowed", m[0])
		}
	}
}
