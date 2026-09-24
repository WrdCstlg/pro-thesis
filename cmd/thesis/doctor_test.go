package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/WrdCstlg/pro-thesis/internal/doctor"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

func TestDoctorHelpReturnsPass(t *testing.T) {
	code := cmdDoctor(context.Background(), globals{}, []string{"--help"})
	if code != schema.ExitPass {
		t.Fatalf("cmdDoctor --help exit code = %v (%d), want ExitPass (0)", code, code)
	}
}

func TestDoctorMissingConfigReturnsConfigError(t *testing.T) {
	code := cmdDoctor(context.Background(), globals{configPath: filepath.Join(t.TempDir(), "nonexistent.yaml")}, nil)
	if code != schema.ExitConfigError {
		t.Fatalf("cmdDoctor missing config exit code = %v (%d), want ExitConfigError (5)", code, code)
	}
}

func TestDoctorJSONOutput(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "prothesis.yaml")
	cfgContent := `version: prothesis/v1
name: test-service
harness:
  backend: compose
  file: docker-compose.yaml
  nodes:
    - { id: n1, service: s1, role_hint: replica, port: 8080 }
driver:
  cmd: "./bin/driver"
`
	if err := os.WriteFile(cfgPath, []byte(cfgContent), 0o644); err != nil {
		t.Fatal(err)
	}

	// Capture stdout
	oldStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w

	code := cmdDoctor(context.Background(), globals{configPath: cfgPath, json: true}, nil)

	_ = w.Close()
	os.Stdout = oldStdout

	var out bytes.Buffer
	_, _ = io.Copy(&out, r)
	_ = r.Close()

	if code != schema.ExitPass && code != schema.ExitConfigError && code != schema.ExitInconclusive {
		t.Fatalf("unexpected exit code: %v", code)
	}

	var rep doctor.Report
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		t.Fatalf("json.Unmarshal failed: %v\nOutput was: %s", err, out.String())
	}
	if len(rep.Checks) == 0 {
		t.Fatalf("expected checks in JSON output, got 0")
	}
}
