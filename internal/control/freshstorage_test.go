package control

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/WrdCstlg/pro-thesis/internal/recorder"
)

// A world records the fresh-storage check its BOOT made (OQ-087) in
// result.json, beside its compose project. The check lives on the topology,
// which no world file keeps; without this record nobody reading a world later
// can tell whether it booted on clean storage, cleaned storage or an unchecked
// project, and H6b cannot claim an empty initial state.
//
// Mutation: drop the storage entry from the runner's result document and this
// fails.
func TestAWorldRecordsItsFreshStorageCheckInResultJSON(t *testing.T) {
	leftover := "stub-proj_n1data"
	var errOut strings.Builder
	r, err := NewRunner(RunnerOptions{
		Config:     drainWorldConfig(t),
		ProjectDir: t.TempDir(),
		Backend: &stubBackend{storage: &recorder.StorageCheck{
			Found: []string{leftover}, Removed: []string{leftover}, Fresh: true,
		}},
		Driver:        &stubDriver{drainable: true},
		Telemetry:     stubTelemetry{},
		OracleEngine:  stubOracles{},
		SteadyState:   noSteadyState{},
		LogCollector:  stubLogs{},
		ImageResolver: stubImages{},
		DrainDeadline: 2 * time.Second,
		Stderr:        &errOut,
		Quiet:         true,
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	if _, _, err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	res := readResultDoc(t, r, 1)
	st, _ := res["storage"].(map[string]any)
	if st == nil {
		t.Fatalf("result.json has no storage record, so the world's fresh-storage check is lost:\n%v", res)
	}
	found, _ := st["found"].([]any)
	removed, _ := st["removed"].([]any)
	if st["fresh"] != true || len(found) != 1 || found[0] != leftover || len(removed) != 1 || removed[0] != leftover {
		t.Fatalf("result.json storage = %v, want fresh, found and removed [%s]", st, leftover)
	}
}
