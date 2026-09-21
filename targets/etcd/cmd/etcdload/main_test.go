package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// fakeEtcd is the smallest server that speaks the two gRPC-gateway routes the
// driver uses. It is single-threaded under a mutex, so every history it
// produces is trivially linearizable; the tests here are about the DRIVER's
// obligations, not etcd's.
type fakeEtcd struct {
	mu    sync.Mutex
	store map[string]string
	rev   int64
	// fail, when set, makes every request answer this status with an etcd
	// style error body.
	fail int
}

func newFakeEtcd() *fakeEtcd { return &fakeEtcd{store: map[string]string{}} }

func (f *fakeEtcd) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != 0 {
		w.WriteHeader(f.fail)
		_, _ = io.WriteString(w, `{"code":14,"message":"etcdserver: request timed out"}`)
		return
	}
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	key, _ := base64.StdEncoding.DecodeString(body["key"].(string))
	hdr := func() map[string]string {
		return map[string]string{"member_id": "42", "revision": itoa(f.rev), "raft_term": "2"}
	}
	switch r.URL.Path {
	case "/v3/kv/put":
		val, _ := base64.StdEncoding.DecodeString(body["value"].(string))
		f.rev++
		f.store[string(key)] = string(val)
		_ = json.NewEncoder(w).Encode(map[string]any{"header": hdr()})
	case "/v3/kv/range":
		resp := map[string]any{"header": hdr(), "count": "0"}
		if v, ok := f.store[string(key)]; ok {
			resp["kvs"] = []map[string]string{{"value": base64.StdEncoding.EncodeToString([]byte(v))}}
			resp["count"] = "1"
		}
		_ = json.NewEncoder(w).Encode(resp)
	default:
		http.NotFound(w, r)
	}
}

func itoa(n int64) string { return json.Number(strings.TrimSpace(string(mustJSON(n)))).String() }

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }

// readHistory decodes every line of a history through the PUBLIC reader and
// validates each record, which is exactly what the harness does.
func readHistory(t *testing.T, path string) []schema.HistoryEntry {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open history: %v", err)
	}
	defer f.Close()
	hr := schema.NewHistoryReader(f)
	var out []schema.HistoryEntry
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
		out = append(out, *e)
	}
	return out
}

// pairs asserts the invoke/completion discipline the checker depends on:
// every op_id has exactly one invoke and exactly one completion, the
// completion's f and key match the invoke's, and the completion type is one
// of ok/fail/info.
func pairs(t *testing.T, hist []schema.HistoryEntry) map[int64][2]*schema.HistoryEntry {
	t.Helper()
	byID := map[int64][2]*schema.HistoryEntry{}
	for i := range hist {
		e := &hist[i]
		if e.OpID == nil {
			t.Fatalf("record %d carries no op_id: %+v", i, e)
		}
		p := byID[*e.OpID]
		switch e.Type {
		case schema.HistoryInvoke:
			if p[0] != nil {
				t.Fatalf("op_id %d has two invokes", *e.OpID)
			}
			p[0] = e
		case schema.HistoryOK, schema.HistoryFail, schema.HistoryInfo:
			if p[1] != nil {
				t.Fatalf("op_id %d has two completions", *e.OpID)
			}
			p[1] = e
		default:
			t.Fatalf("op_id %d has unknown type %q", *e.OpID, e.Type)
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

func TestProfileModeWritesAValidPairedHistory(t *testing.T) {
	srv := httptest.NewServer(newFakeEtcd())
	defer srv.Close()
	hist := filepath.Join(t.TempDir(), "history.jsonl")

	code, err := run([]string{"--history", hist, "--seed", "7", "--targets", srv.URL + "," + srv.URL,
		"--clients", "3", "--ops", "120", "--keys", "4"}, strings.NewReader(""), io.Discard)
	if err != nil || code != exitOK {
		t.Fatalf("run: code=%d err=%v", code, err)
	}

	h := readHistory(t, hist)
	byID := pairs(t, h)
	// The op budget is 120 and the seed writes come out of it, so the history
	// holds exactly 120 pairs: the first four being process 0 seeding
	// k/0..k/3 so no read ever has to answer null.
	if got, want := len(byID), 120; got != want {
		t.Fatalf("op pairs: got %d, want %d", got, want)
	}
	for i := 0; i < 4; i++ {
		p, ok := byID[int64(i+1)]
		if !ok || p[0].F != "write" || *p[0].Key != key(i) || *p[0].Process != 0 {
			t.Fatalf("op_id %d should be process 0 seeding %s; got %+v", i+1, key(i), p[0])
		}
	}
	reads, writes, ok := 0, 0, 0
	for _, p := range byID {
		switch p[0].F {
		case "read":
			reads++
			if len(p[0].Value) != 0 {
				t.Fatalf("a read's invoke carries a value: %s", p[0].Value)
			}
		case "write":
			writes++
			if len(p[0].Value) == 0 {
				t.Fatalf("a write's invoke carries no value")
			}
		default:
			t.Fatalf("unexpected f %q", p[0].F)
		}
		if p[1].Type == schema.HistoryOK {
			ok++
			if len(p[1].Value) == 0 {
				t.Fatalf("op_id %d: an ok completion carries no value", *p[1].OpID)
			}
		}
	}
	if reads == 0 || writes == 0 {
		t.Fatalf("mix did not produce both kinds: reads=%d writes=%d", reads, writes)
	}
	if ok != len(byID) {
		t.Fatalf("against a healthy server every op should be ok: %d of %d", ok, len(byID))
	}
}

func TestReadCompletionsCarryTheReadModeTheMixAskedFor(t *testing.T) {
	srv := httptest.NewServer(newFakeEtcd())
	defer srv.Close()
	dir := t.TempDir()
	hist := filepath.Join(dir, "history.jsonl")
	planPath := filepath.Join(dir, "plan.json")
	// A PROFILE plan whose mix is serializable reads only.
	_ = os.WriteFile(planPath, []byte(`{"schema":"prothesis.driver_plan/v1","profile":"stale","seed":1,
		"history_path":"x","clients":2,"ops":40,"mix":[{"op":"read_serializable","weight_ppm":1000000}]}`), 0o644)

	code, err := run([]string{"--history", hist, "--targets", srv.URL, "--plan", planPath, "--keys", "2"},
		strings.NewReader(""), io.Discard)
	if err != nil || code != exitOK {
		t.Fatalf("run: code=%d err=%v", code, err)
	}
	raw, _ := os.ReadFile(hist)
	var serializable, reads int
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var r struct {
			Type string `json:"type"`
			F    string `json:"f"`
			Meta struct {
				ReadMode string `json:"read_mode"`
			} `json:"meta"`
		}
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatal(err)
		}
		if r.F == "read" {
			reads++
			if r.Meta.ReadMode == readSerializable {
				serializable++
			}
		}
	}
	if reads == 0 || serializable != reads {
		t.Fatalf("read records=%d, carrying read_mode=serializable: %d", reads, serializable)
	}
}

func TestOperationModeExecutesTheTraceVerbatim(t *testing.T) {
	srv := httptest.NewServer(newFakeEtcd())
	defer srv.Close()
	dir := t.TempDir()
	hist := filepath.Join(dir, "history.jsonl")
	planPath := filepath.Join(dir, "plan.json")
	_ = os.WriteFile(planPath, []byte(`{"schema":"prothesis.driver_plan/v1","profile":"linear","seed":1,
		"history_path":"x","clients":16,"ops":60000,"mix":[{"op":"read","weight_ppm":500000},{"op":"write","weight_ppm":500000}],
		"operations":[
		  {"op_id":90001,"process":0,"f":"write","key":"k/3","value":7000126,"target":"etcd-n2","at_ms":10},
		  {"op_id":90004,"process":1,"f":"read","key":"k/3","read_mode":"serializable","target":"etcd-n1","at_ms":12},
		  {"op_id":90007,"process":0,"f":"read","key":"k/3","target":"etcd-n2","at_ms":15}
		],
		"targets":["etcd-n1","etcd-n2"]}`), 0o644)

	code, err := run([]string{"--history", hist, "--targets", srv.URL + "," + srv.URL, "--plan", planPath},
		strings.NewReader(""), io.Discard)
	if err != nil || code != exitOK {
		t.Fatalf("run: code=%d err=%v", code, err)
	}
	h := readHistory(t, hist)
	byID := pairs(t, h)
	if len(byID) != 3 {
		t.Fatalf("operation mode must execute exactly the trace: got %d op pairs, want 3", len(byID))
	}
	for _, id := range []int64{90001, 90004, 90007} {
		if _, ok := byID[id]; !ok {
			t.Fatalf("planned op_id %d was not executed", id)
		}
	}
	// The write's value is carried verbatim and the later read on the same
	// key returns it.
	if got := string(byID[90001][0].Value); got != "7000126" {
		t.Fatalf("write value not verbatim: %s", got)
	}
	if got := string(byID[90007][1].Value); got != "7000126" {
		t.Fatalf("read after write returned %s", got)
	}
	// Per-process order is kept: process 0's write precedes its read.
	if byID[90001][1].TNS > byID[90007][0].TNS {
		t.Fatalf("process 0 issued its read before its write completed")
	}
}

func TestOperationModeRefusesATraceItCannotExecute(t *testing.T) {
	dir := t.TempDir()
	cases := map[string]string{
		"empty trace": `{"schema":"prothesis.driver_plan/v1","profile":"p","seed":1,"history_path":"x","operations":[]}`,
		"unknown f":   `{"schema":"prothesis.driver_plan/v1","profile":"p","seed":1,"history_path":"x","operations":[{"op_id":1,"process":0,"f":"txn","key":"k"}]}`,
		"more targets than we have": `{"schema":"prothesis.driver_plan/v1","profile":"p","seed":1,"history_path":"x",
			"operations":[{"op_id":1,"process":0,"f":"read","key":"k","target":"etcd-n3"}],"targets":["etcd-n1","etcd-n2","etcd-n3"]}`,
	}
	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			planPath := filepath.Join(dir, strings.ReplaceAll(name, " ", "_")+".json")
			_ = os.WriteFile(planPath, []byte(doc), 0o644)
			hist := filepath.Join(dir, strings.ReplaceAll(name, " ", "_")+".jsonl")
			code, err := run([]string{"--history", hist, "--targets", "127.0.0.1:1,127.0.0.1:2", "--plan", planPath},
				strings.NewReader(""), io.Discard)
			if code != exitConfig || err == nil {
				t.Fatalf("code=%d err=%v; want a config refusal", code, err)
			}
			if _, statErr := os.Stat(hist); statErr == nil {
				t.Fatalf("a refused plan must not leave a history behind")
			}
		})
	}
}

func TestDrainOnStdinStopsIssuingAndCompletesInFlight(t *testing.T) {
	srv := httptest.NewServer(newFakeEtcd())
	defer srv.Close()
	hist := filepath.Join(t.TempDir(), "history.jsonl")
	pr, pw := io.Pipe()
	done := make(chan struct{})
	var code int
	var err error
	go func() {
		defer close(done)
		// A budget far larger than the test would tolerate: only the drain
		// can end this run.
		code, err = run([]string{"--history", hist, "--targets", srv.URL, "--clients", "2", "--ops", "10000000",
			"--stdin-control"}, pr, io.Discard)
	}()
	// Let some operations happen, then ask for a stop.
	for {
		if st, e := os.Stat(hist); e == nil && st.Size() > 2000 {
			break
		}
	}
	_, _ = io.WriteString(pw, "{\"cmd\":\"stop\"}\n")
	<-done
	if err != nil || code != exitOK {
		t.Fatalf("run: code=%d err=%v", code, err)
	}
	pairs(t, readHistory(t, hist)) // every invoke has its completion: nothing was left in flight
}

func TestClassifyRequiresProofOfNonExecution(t *testing.T) {
	refused := &net.OpError{Op: "dial", Err: errors.New("connectex: No connection could be made because the target machine actively refused it.")}
	timeout := context.DeadlineExceeded
	reset := &net.OpError{Op: "read", Err: errors.New("wsarecv: An existing connection was forcibly closed by the remote host.")}

	cases := []struct {
		name string
		att  attempt
		raw  string
		want schema.HistoryType
	}{
		{"200 parsed is ok", attempt{gotResponse: true, status: 200, bodyParsed: true}, "", schema.HistoryOK},
		{"200 unparsed is info", attempt{gotResponse: true, status: 200}, "garbage", schema.HistoryInfo},
		{"refused before any byte is fail", attempt{err: refused}, "", schema.HistoryFail},
		{"timeout before headers is info", attempt{wroteRequest: true, err: timeout}, "", schema.HistoryInfo},
		{"timeout before the request was written is STILL info", attempt{err: timeout}, "", schema.HistoryInfo},
		{"reset after the request was written is info", attempt{wroteRequest: true, err: reset}, "", schema.HistoryInfo},
		{"503 from etcd is info", attempt{gotResponse: true, status: 503}, `{"code":14,"message":"etcdserver: request timed out"}`, schema.HistoryInfo},
		{"400 is info too: etcd carries no applied:false", attempt{gotResponse: true, status: 400}, `{"code":3,"message":"etcdserver: key is not provided"}`, schema.HistoryInfo},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := classify(c.att, []byte(c.raw))
			if got.typ != c.want {
				t.Fatalf("got %s (%q), want %s", got.typ, got.label, c.want)
			}
		})
	}
}

func TestEveryFailureAgainstADeadServerIsInfoOrFailNeverOK(t *testing.T) {
	// A server that answers 503 to everything: nothing may be recorded ok.
	fe := newFakeEtcd()
	fe.fail = 503
	srv := httptest.NewServer(fe)
	defer srv.Close()
	hist := filepath.Join(t.TempDir(), "history.jsonl")
	code, err := run([]string{"--history", hist, "--targets", srv.URL, "--clients", "2", "--ops", "30", "--keys", "2"},
		strings.NewReader(""), io.Discard)
	if err != nil || code != exitOK {
		t.Fatalf("run: code=%d err=%v", code, err)
	}
	for _, p := range pairs(t, readHistory(t, hist)) {
		if p[1].Type == schema.HistoryOK {
			t.Fatalf("op_id %d recorded ok against a server that refused everything", *p[1].OpID)
		}
		if p[1].Error == "" {
			t.Fatalf("op_id %d: a non-ok completion must say why", *p[1].OpID)
		}
	}
}

func TestParseTargetsLabelsByPositionUnlessNamed(t *testing.T) {
	got, err := parseTargets("127.0.0.1:12379, http://127.0.0.1:12380/, n9=127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	want := []target{{"etcd-n1", "http://127.0.0.1:12379"}, {"etcd-n2", "http://127.0.0.1:12380"}, {"n9", "http://127.0.0.1:1"}}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("target %d: got %+v want %+v", i, got[i], want[i])
		}
	}
	if _, err := parseTargets(" , "); err == nil {
		t.Fatal("an empty list must be refused")
	}
}
