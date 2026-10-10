package control

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// OQ-080: a failure to write verdict.json is discarded
func TestWriteVerdictFailureReturnsInconclusiveAndError(t *testing.T) {
	r, _ := newPerturbTestRunner(t, perturbTestConfig(t), nil)
	r.oracleEngine = noFindings{}
	
	// Create runDir early
	if err := os.MkdirAll(r.runDir, 0o755); err != nil {
		t.Fatalf("setup mkdir: %v", err)
	}
	
	// To force WriteVerdictFile to fail, create a DIRECTORY with the same name.
	verdictPath := filepath.Join(r.runDir, VerdictFileName)
	if err := os.Mkdir(verdictPath, 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}

	_, exit, err := r.Run(context.Background())
	if err == nil {
		t.Errorf("Run() expected error when writing verdict.json fails, got nil")
	}
	if exit != schema.ExitInconclusive {
		t.Errorf("Run() expected exit %d (Inconclusive) when writing verdict fails, got %d", schema.ExitInconclusive, exit)
	}
}
