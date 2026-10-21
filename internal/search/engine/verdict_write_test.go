package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/WrdCstlg/pro-thesis/internal/control"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// A search whose verdict.json cannot be written has no verdict anyone else can
// read, so it cannot exit with the verdict's code. `thesis run` already exits 2
// with the error in this case; the search discarded the error and exited 0.
func TestASearchVerdictThatCannotBeWrittenIsExitTwo(t *testing.T) {
	r := newEngineRunner(t, engineTestConfig(t, true))
	e, err := New(Options{Runner: r, Quiet: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// A directory where the file must go makes the write fail.
	if err := os.MkdirAll(filepath.Join(r.RunDir(), control.VerdictFileName), 0o755); err != nil {
		t.Fatalf("plant directory: %v", err)
	}
	res, err := e.finish(context.Background(), Result{
		Worlds: []control.WorldOutcome{{Ordinal: 1, Outcome: control.OutcomePass}},
	}, "test", nil)
	if err == nil {
		t.Fatal("finish reported no error although verdict.json could not be written")
	}
	if res.ExitCode != schema.ExitInconclusive {
		t.Fatalf("exit = %d, want 2: a clean search whose verdict was never written must not exit 0", res.ExitCode)
	}
}
