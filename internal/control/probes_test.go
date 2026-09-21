package control

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/WrdCstlg/pro-thesis/internal/oracle"
)

// OQ-063. availability_after_heal must see every attempt in the convergence
// window: the direct probes the runner took AND the telemetry collector's
// samples at the same declared URL. Before D-066 the telemetry samples were
// dropped whenever a direct probe existed, so a node that answered four
// in-window telemetry probes and failed the one direct probe at QUIESCE+0 ms
// was reported as never available. Mutation: returning req.Probes when it is
// non-empty fails this test.
func TestReadProbesMergesDirectAndTelemetryAttempts(t *testing.T) {
	dir := t.TempDir()
	tel := filepath.Join(dir, "telemetry.jsonl")
	lines := "" +
		// before DRIVE: no virtual time, must be skipped
		`{"schema":"prothesis.telemetry_sample/v1","seq":0,"node":"n1","t_ns":1,"v_ms":null,"probe":{"url":"http://127.0.0.1:1/health","ok":true,"status_code":200,"latency_us":100}}` + "\n" +
		// in the window: two answers for n1, one for n2
		`{"schema":"prothesis.telemetry_sample/v1","seq":1,"node":"n1","t_ns":2,"v_ms":2157,"probe":{"url":"http://127.0.0.1:1/health","ok":true,"status_code":200,"latency_us":1500}}` + "\n" +
		`{"schema":"prothesis.telemetry_sample/v1","seq":2,"node":"n1","t_ns":3,"v_ms":2657,"probe":{"url":"http://127.0.0.1:1/health","ok":true,"status_code":200,"latency_us":1200}}` + "\n" +
		`{"schema":"prothesis.telemetry_sample/v1","seq":3,"node":"n2","t_ns":4,"v_ms":2657,"probe":{"url":"http://127.0.0.1:2/health","ok":false,"status_code":503,"latency_us":900,"error":"HTTP 503"}}` + "\n" +
		// a sample with no probe at all
		`{"schema":"prothesis.telemetry_sample/v1","seq":4,"node":"n2","t_ns":5,"v_ms":3157,"probe":null}` + "\n"
	if err := os.WriteFile(tel, []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}
	req := EvalRequest{
		Probes: []oracle.ProbeObservation{
			{NodeID: "n1", Target: "http://127.0.0.1:1/health", TMS: 1814, OK: false, Err: "HTTP status 503"},
			{NodeID: "n2", Target: "http://127.0.0.1:2/health", TMS: 1815, OK: false, Err: "HTTP status 503"},
		},
	}
	req.Paths.Telemetry = tel

	got := readProbes(req)
	if len(got) != 5 {
		t.Fatalf("want 2 direct + 3 telemetry attempts, got %d: %+v", len(got), got)
	}
	if got[0].TMS != 1814 || got[1].TMS != 1815 {
		t.Fatalf("direct probes must come first, unchanged: %+v", got[:2])
	}
	var n1OK int
	for _, p := range got[2:] {
		if p.NodeID == "n1" && p.OK {
			n1OK++
		}
	}
	if n1OK != 2 {
		t.Fatalf("n1's two in-window telemetry answers were not carried: %+v", got[2:])
	}
	if got[4].NodeID != "n2" || got[4].OK || got[4].Err != "HTTP 503" || got[4].LatencyMS != 0 {
		t.Fatalf("telemetry failure must keep its error and ms latency: %+v", got[4])
	}
}

func TestReadProbesWithoutTelemetryReturnsTheDirectProbesOnly(t *testing.T) {
	req := EvalRequest{Probes: []oracle.ProbeObservation{{NodeID: "n1", TMS: 1, OK: true}}}
	req.Paths.Telemetry = filepath.Join(t.TempDir(), "missing.jsonl")
	if got := readProbes(req); len(got) != 1 {
		t.Fatalf("got %+v", got)
	}
}
