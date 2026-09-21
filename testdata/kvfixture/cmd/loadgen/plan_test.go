package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"prothesis.dev/kvfixture/internal/plan"
)

// OQ-052. Stage 2 of `thesis shrink` reduces the WORKLOAD by handing the driver
// a shorter operation trace. This driver declared planPath as a struct field and
// never read it, so it drove its usual load whatever it was handed: every
// candidate reproduced, ddmin walked straight to a single operation, and the run
// reported "6761 -> 1 operations" against a workload that never changed.
//
// D-056 made the harness refuse that rather than believe it. These tests are the
// other half: they assert the driver now HONOURS the plan, which is what makes
// the refusal unnecessary rather than merely correct.
//
// Everything here runs against httptest, so plan mode is verified without
// Docker. What is not verified here is that a shrunk trace still REPRODUCES the
// fixture's defect: that needs the real cluster and is recorded as still open.

// fakeCluster is a KV node that answers every client-plane call successfully and
// records what it was asked, so a test can compare the plan against the traffic.
type fakeCluster struct {
	mu   sync.Mutex
	node string
	// seen is every request path, in arrival order.
	seen []string
	// writes maps key -> last value written.
	writes map[string]int64
}

func newFakeCluster(t *testing.T, node string) (*fakeCluster, *httptest.Server) {
	t.Helper()
	f := &fakeCluster{node: node, writes: map[string]int64{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		if r.URL.Path != "/status" {
			f.seen = append(f.seen, r.Method+" "+r.URL.Path)
		}
		f.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/status":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"node": f.node, "role": "leader", "term": 1, "commit_index": 1,
			})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/kv/"):
			key := strings.TrimPrefix(r.URL.Path, "/kv/")
			f.mu.Lock()
			v, ok := f.writes[key]
			f.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": true, "found": ok, "value": v, "served_by": f.node,
				"term": 1, "commit_index": 1, "read_mode": r.URL.Query().Get("consistency"),
			})
		case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/kv/"):
			key := strings.TrimPrefix(r.URL.Path, "/kv/")
			var body struct {
				Value int64 `json:"value"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.mu.Lock()
			f.writes[key] = body.Value
			f.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": true, "applied": true, "served_by": f.node, "term": 1, "index": 1,
			})
		case r.URL.Path == "/txn":
			var body struct {
				Ops json.RawMessage `json:"ops"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.mu.Lock()
			f.seen[len(f.seen)-1] += " " + string(body.Ops)
			f.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": true, "applied": true, "ops": json.RawMessage(body.Ops),
				"served_by": f.node, "term": 1, "index": 1,
			})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": true, "applied": true, "served_by": f.node,
			})
		}
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeCluster) requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.seen...)
}

// writePlan renders a plan document and returns its path.
func writePlan(t *testing.T, p plan.Plan) string {
	t.Helper()
	p.Schema = plan.Schema
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal plan: %v", err)
	}
	path := filepath.Join(t.TempDir(), "plan.json")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatalf("write plan: %v", err)
	}
	return path
}

// readHistory returns the decoded records the driver wrote.
func readHistory(t *testing.T, path string) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read history: %v", err)
	}
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("history line %q: %v", line, err)
		}
		out = append(out, rec)
	}
	return out
}

func intVal(t *testing.T, m map[string]any, k string) int64 {
	t.Helper()
	f, ok := m[k].(float64)
	if !ok {
		t.Fatalf("record has no numeric %q: %v", k, m)
	}
	return int64(f)
}

// TestPlanModeDrivesExactlyThePlannedOperations is the test OQ-052 needed and
// did not have. A plan of four operations must produce four invocations, not
// the profile's 60,000.
func TestPlanModeDrivesExactlyThePlannedOperations(t *testing.T) {
	fake, srv := newFakeCluster(t, "kv-n1")

	hist := filepath.Join(t.TempDir(), "history.jsonl")
	planPath := writePlan(t, plan.Plan{
		Profile: "linear", Seed: 7, Targets: []string{"http://recorded-host:18081"},
		Operations: []plan.Op{
			{OpID: 4001, Process: 7, F: plan.FWrite, Key: "k/0", Value: json.RawMessage("11"),
				Target: "http://recorded-host:18081"},
			{OpID: 4002, Process: 7, F: plan.FRead, Key: "k/0", ReadMode: plan.ReadLinearizable,
				Target: "http://recorded-host:18081"},
			{OpID: 4003, Process: 11, F: plan.FTxn,
				Value:  json.RawMessage(`[["r","k/1",null],["w","k/2",22]]`),
				Target: "http://recorded-host:18081"},
			{OpID: 4004, Process: 11, F: plan.FAdmin, Target: "http://recorded-host:18081"},
		},
	})

	code, err := run([]string{
		"--history", hist, "--seed", "7", "--profile", "linear",
		"--targets", srv.URL, "--plan", planPath, "--quiet",
	})
	if err != nil || code != exitOK {
		t.Fatalf("run = %d, %v", code, err)
	}

	recs := readHistory(t, hist)
	var invokes []map[string]any
	for _, r := range recs {
		if r["type"] == "invoke" {
			invokes = append(invokes, r)
		}
	}
	if len(invokes) != 4 {
		t.Fatalf("the driver issued %d invocation(s) for a 4-operation plan. This is OQ-052: "+
			"a driver that ignores its plan makes every shrink candidate reproduce and turns "+
			"ddmin into a machine for inventing reductions", len(invokes))
	}

	// Op ids are carried VERBATIM. The verdict's witness names op ids, so a
	// replay that renumbered them could not be cross-checked against the
	// violation it is supposed to reproduce.
	wantIDs := []int64{4001, 4002, 4003, 4004}
	gotIDs := map[int64]bool{}
	for _, in := range invokes {
		gotIDs[intVal(t, in, "op_id")] = true
	}
	for _, id := range wantIDs {
		if !gotIDs[id] {
			t.Errorf("op_id %d is absent; the plan's ids must be carried verbatim, got %v", id, gotIDs)
		}
	}

	// Processes are the RECORDED identities, not 0..n-1. Re-assigning operations
	// between processes changes the concurrency structure and therefore which
	// histories are linearizable.
	procs := map[int64]bool{}
	for _, in := range invokes {
		procs[intVal(t, in, "process")] = true
	}
	if !procs[7] || !procs[11] {
		t.Errorf("processes = %v, want the recorded 7 and 11", procs)
	}

	// And the cluster saw exactly the planned shapes.
	got := fake.requests()
	if len(got) != 4 {
		t.Fatalf("the cluster saw %d request(s): %v", len(got), got)
	}
	joined := strings.Join(got, " | ")
	for _, want := range []string{
		"PUT /kv/k/0", "GET /kv/k/0", "POST /txn", "POST /admin/noop",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("the cluster never saw %q; it saw: %s", want, joined)
		}
	}
	// The transaction replayed its RECORDED op list rather than a regenerated
	// read-one/write-another pair.
	if !strings.Contains(joined, `["r","k/1",null]`) || !strings.Contains(joined, `["w","k/2",22]`) {
		t.Errorf("the txn did not replay its recorded ops: %s", joined)
	}
}

// A plan with no `operations` member is a PROFILE plan, and the harness has
// pointed PROTHESIS_PLAN_PATH at one on every run since Phase 1. Treating its
// presence as "a trace to replay" would execute nothing and still write a
// history a consistency checker calls perfectly clean.
func TestAProfilePlanDoesNotSuppressTheWorkload(t *testing.T) {
	_, srv := newFakeCluster(t, "kv-n1")

	hist := filepath.Join(t.TempDir(), "history.jsonl")
	planPath := writePlan(t, plan.Plan{Profile: "smoke", Seed: 3, Clients: 2, Ops: 6})

	code, err := run([]string{
		"--history", hist, "--seed", "3", "--profile", "smoke",
		"--targets", srv.URL, "--plan", planPath, "--quiet",
		"--clients", "2", "--ops", "6", "--think-max-ms", "0",
	})
	if err != nil || code != exitOK {
		t.Fatalf("run = %d, %v", code, err)
	}

	var invokes int
	for _, r := range readHistory(t, hist) {
		if r["type"] == "invoke" {
			invokes++
		}
	}
	if invokes == 0 {
		t.Fatal("a profile plan suppressed the workload entirely: the driver wrote no " +
			"invocations, which is a history that every consistency oracle calls clean")
	}
}

// The environment fallback is D-021's dual transport: a driver.cmd that does not
// spell {plan_path} must still find the plan.
func TestPlanPathIsReadFromTheEnvironment(t *testing.T) {
	fake, srv := newFakeCluster(t, "kv-n1")

	hist := filepath.Join(t.TempDir(), "history.jsonl")
	planPath := writePlan(t, plan.Plan{
		Profile: "linear", Seed: 1, Targets: []string{"http://recorded:1"},
		Operations: []plan.Op{
			{OpID: 1, Process: 0, F: plan.FWrite, Key: "k/9", Value: json.RawMessage("5"),
				Target: "http://recorded:1"},
		},
	})
	t.Setenv(plan.EnvPath, planPath)

	code, err := run([]string{
		"--history", hist, "--seed", "1", "--profile", "linear",
		"--targets", srv.URL, "--quiet",
	})
	if err != nil || code != exitOK {
		t.Fatalf("run = %d, %v", code, err)
	}
	if got := fake.requests(); len(got) != 1 || !strings.Contains(got[0], "PUT /kv/k/9") {
		t.Fatalf("the environment plan was not executed; cluster saw %v", got)
	}
}

// A trace recorded against more nodes than this driver has cannot be mapped
// position-for-position. Remapping anyway would drive a different cluster than
// the trace names, and a world that drove the wrong cluster still writes a
// history that looks clean.
func TestAPlanWithMoreTargetsThanTheDriverHasIsRefused(t *testing.T) {
	_, srv := newFakeCluster(t, "kv-n1")

	hist := filepath.Join(t.TempDir(), "history.jsonl")
	planPath := writePlan(t, plan.Plan{
		Profile: "linear", Seed: 1,
		Targets: []string{"http://a:1", "http://b:2", "http://c:3"},
		Operations: []plan.Op{
			{OpID: 1, Process: 0, F: plan.FWrite, Key: "k/0", Value: json.RawMessage("1"),
				Target: "http://c:3"},
		},
	})

	code, err := run([]string{
		"--history", hist, "--seed", "1", "--profile", "linear",
		"--targets", srv.URL, "--plan", planPath, "--quiet",
	})
	if code != exitConfig || err == nil {
		t.Fatalf("run = %d, %v; want exit %d and an error", code, err, exitConfig)
	}
	if !strings.Contains(err.Error(), "POSITION") {
		t.Errorf("the refusal does not explain why remapping is unsafe: %v", err)
	}
	// And it refused BEFORE writing a history: a zero-length history is
	// indistinguishable from a run that drove nothing and passed.
	if _, statErr := os.Stat(hist); statErr == nil {
		t.Error("the refused run created a history file")
	}
}

// A plan naming an operation this driver cannot perform is refused whole. Running
// the rest would produce a history that silently differs from the trace the
// minimizer is reasoning about.
func TestAnUnexecutablePlanIsRefusedWhole(t *testing.T) {
	_, srv := newFakeCluster(t, "kv-n1")

	hist := filepath.Join(t.TempDir(), "history.jsonl")
	planPath := writePlan(t, plan.Plan{
		Profile: "linear", Seed: 1, Targets: []string{"http://a:1"},
		Operations: []plan.Op{
			{OpID: 1, Process: 0, F: plan.FWrite, Key: "k/0", Value: json.RawMessage("1"), Target: "http://a:1"},
			{OpID: 2, Process: 0, F: "teleport", Key: "k/0", Target: "http://a:1"},
		},
	})

	code, err := run([]string{
		"--history", hist, "--seed", "1", "--profile", "linear",
		"--targets", srv.URL, "--plan", planPath, "--quiet",
	})
	if code != exitConfig || err == nil {
		t.Fatalf("run = %d, %v; want a refusal", code, err)
	}
	if !strings.Contains(err.Error(), "teleport") {
		t.Errorf("the refusal does not name the offending operation: %v", err)
	}
}

// planWasHonoured (D-056) counts `invoke` records and refuses a candidate whose
// history holds more than were planned. This asserts the two halves agree: the
// driver must never exceed its plan, or the harness will correctly refuse a
// reduction that was in fact honoured.
func TestTheDriverNeverExceedsItsPlan(t *testing.T) {
	for _, n := range []int{1, 2, 5, 13} {
		t.Run(fmt.Sprintf("%d_operations", n), func(t *testing.T) {
			_, srv := newFakeCluster(t, "kv-n1")
			hist := filepath.Join(t.TempDir(), "history.jsonl")

			ops := make([]plan.Op, 0, n)
			for i := 0; i < n; i++ {
				ops = append(ops, plan.Op{
					OpID: int64(100 + i), Process: i % 3, F: plan.FWrite,
					Key: fmt.Sprintf("k/%d", i%4), Value: json.RawMessage("1"),
					Target: "http://a:1",
				})
			}
			planPath := writePlan(t, plan.Plan{
				Profile: "linear", Seed: 1, Targets: []string{"http://a:1"}, Operations: ops,
			})

			code, err := run([]string{
				"--history", hist, "--seed", "1", "--profile", "linear",
				"--targets", srv.URL, "--plan", planPath, "--quiet",
			})
			if err != nil || code != exitOK {
				t.Fatalf("run = %d, %v", code, err)
			}

			invokes := 0
			for _, r := range readHistory(t, hist) {
				if r["type"] == "invoke" {
					invokes++
				}
			}
			if invokes > n {
				t.Fatalf("%d invocation(s) for a %d-operation plan; D-056 would refuse this "+
					"candidate and the reduction would be reported as impossible", invokes, n)
			}
		})
	}
}
