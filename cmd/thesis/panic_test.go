package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"testing"

	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// OQ-082: an unrecovered panic exits 2, the INCONCLUSIVE code, and writes no verdict
func TestPanicWritesVerdict(t *testing.T) {
	if os.Getenv("BE_CRASHING_THESIS") == "1" {
		os.Args = []string{"thesis", "__panic"}
		main()
		return
	}

	// Make sure verdict.json doesn't exist to start
	_ = os.Remove("verdict.json")
	defer os.Remove("verdict.json")

	cmd := exec.Command(os.Args[0], "-test.run=TestPanicWritesVerdict")
	cmd.Env = append(os.Environ(), "BE_CRASHING_THESIS=1")
	err := cmd.Run()
	
	if err == nil {
		t.Fatalf("expected crash, got nil")
	}

	exitError, ok := err.(*exec.ExitError)
	if !ok || exitError.ExitCode() != int(schema.ExitInconclusive) {
		t.Fatalf("expected exit code %d, got %v", schema.ExitInconclusive, err)
	}

	data, err := os.ReadFile("verdict.json")
	if err != nil {
		t.Fatalf("verdict.json was not written: %v", err)
	}

	var v schema.Verdict
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatalf("verdict.json invalid JSON: %v", err)
	}

	if v.Verdict != schema.VerdictInconclusive {
		t.Errorf("verdict is %q, want %q", v.Verdict, schema.VerdictInconclusive)
	}
	if v.Error != "panic: test panic" {
		t.Errorf("unexpected error text: %q", v.Error)
	}
}
