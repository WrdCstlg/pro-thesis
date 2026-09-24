package doctor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/WrdCstlg/pro-thesis/internal/probehost"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

func stubValidConfig(t *testing.T, dir string) *schema.Config {
	t.Helper()
	cfg := &schema.Config{
		Version: schema.ConfigVersion,
		Name:    "test-service",
		Harness: schema.HarnessConfig{
			Backend: "compose",
			File:    "docker-compose.yaml",
			Nodes: []schema.NodeConfig{
				{ID: "n1", Service: "s1", RoleHint: "replica", Port: 8080},
			},
			Health: []schema.HealthProbe{
				{Node: "s1:*", Probe: "http://{host}:{port}/healthz", Timeout: schema.Duration(30)},
			},
		},
		Driver: schema.DriverConfig{
			Cmd: "./bin/driver",
			Profiles: map[string]schema.DriverProfile{
				"smoke": {Clients: 1, Ops: 10},
			},
		},
		Profiles: map[string]schema.Profile{
			"smoke": {Budget: schema.Duration(60), DriverProfile: "smoke"},
		},
	}
	return cfg
}

func healthyDockerCheck(version string) DockerChecker {
	return func(ctx context.Context) (string, error) {
		return version, nil
	}
}

func loopbackLookup(host string) ([]string, error) {
	if host == "127.0.0.1" || host == "localhost" {
		return []string{"127.0.0.1"}, nil
	}
	return nil, fmt.Errorf("host %s not found", host)
}

// Failing-first requirement 1: A PE driver refused in a Linux context.
func TestDoctorRefusesPEDriverInLinuxContext(t *testing.T) {
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Write a Windows PE binary (magic bytes 'M', 'Z')
	driverPath := filepath.Join(binDir, "driver")
	if err := os.WriteFile(driverPath, []byte("MZ\x90\x00this is a pe binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := stubValidConfig(t, dir)
	opts := Options{
		TargetGOOS:  "linux",
		DockerCheck: healthyDockerCheck("v2.40.3"),
		LookupHost:  loopbackLookup,
	}

	rep := Diagnose(context.Background(), cfg, dir, opts)
	if rep.ExitCode != schema.ExitConfigError {
		t.Fatalf("exit code = %v (%d), want ExitConfigError (5)", rep.ExitCode, rep.ExitCode)
	}

	var found bool
	for _, c := range rep.Checks {
		if c.Name == "driver" && c.Status == StatusFailed {
			detailLower := strings.ToLower(c.Detail)
			if strings.Contains(detailLower, "windows pe") && strings.Contains(detailLower, "linux") {
				found = true
				break
			}
		}
	}
	if !found {
		t.Fatalf("driver check did not fail with Windows PE / Linux platform mismatch message: %+v", rep.Checks)
	}
}

// Failing-first requirement 2: A missing driver refused.
func TestDoctorRefusesMissingDriver(t *testing.T) {
	dir := t.TempDir()
	cfg := stubValidConfig(t, dir)
	cfg.Driver.Cmd = "./bin/nonexistent_driver"

	opts := Options{
		TargetGOOS:  "linux",
		DockerCheck: healthyDockerCheck("v2.40.3"),
		LookupHost:  loopbackLookup,
	}

	rep := Diagnose(context.Background(), cfg, dir, opts)
	if rep.ExitCode != schema.ExitConfigError {
		t.Fatalf("exit code = %v (%d), want ExitConfigError (5)", rep.ExitCode, rep.ExitCode)
	}

	var found bool
	for _, c := range rep.Checks {
		if c.Name == "driver" && c.Status == StatusFailed {
			if strings.Contains(c.Detail, "not found") || strings.Contains(c.Detail, "does not exist") {
				found = true
				break
			}
		}
	}
	if !found {
		t.Fatalf("driver check did not fail with missing driver error: %+v", rep.Checks)
	}
}

// Failing-first requirement 3: A compose v1 daemon refused.
func TestDoctorRefusesComposeV1(t *testing.T) {
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	driverPath := filepath.Join(binDir, "driver")
	if err := os.WriteFile(driverPath, []byte("\x7fELFstub"), 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := stubValidConfig(t, dir)
	opts := Options{
		TargetGOOS:  "linux",
		DockerCheck: healthyDockerCheck("1.29.2"),
		LookupHost:  loopbackLookup,
	}

	rep := Diagnose(context.Background(), cfg, dir, opts)
	if rep.ExitCode != schema.ExitConfigError {
		t.Fatalf("exit code = %v (%d), want ExitConfigError (5)", rep.ExitCode, rep.ExitCode)
	}

	var found bool
	for _, c := range rep.Checks {
		if c.Name == "docker" && c.Status == StatusFailed {
			if strings.Contains(c.Detail, "v2 required") || strings.Contains(c.Detail, "v1") {
				found = true
				break
			}
		}
	}
	if !found {
		t.Fatalf("docker check did not fail with compose v1 refusal: %+v", rep.Checks)
	}
}

// Requirement: Docker unreachable reports INCONCLUSIVE (exit 2).
func TestDoctorReportsEnvironmentErrorOnUnreachableDocker(t *testing.T) {
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	driverPath := filepath.Join(binDir, "driver")
	if err := os.WriteFile(driverPath, []byte("\x7fELFstub"), 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := stubValidConfig(t, dir)
	opts := Options{
		TargetGOOS: "linux",
		DockerCheck: func(ctx context.Context) (string, error) {
			return "", errors.New("daemon not reachable: connection refused")
		},
		LookupHost: loopbackLookup,
	}

	rep := Diagnose(context.Background(), cfg, dir, opts)
	if rep.ExitCode != schema.ExitInconclusive {
		t.Fatalf("exit code = %v (%d), want ExitInconclusive (2)", rep.ExitCode, rep.ExitCode)
	}

	var found bool
	for _, c := range rep.Checks {
		if c.Name == "docker" && c.Status == StatusInconclusive {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("docker check did not report inconclusive: %+v", rep.Checks)
	}
}

// Requirement: Unresolvable probe host reports INCONCLUSIVE (exit 2).
func TestDoctorRefusesUnresolvableProbeHost(t *testing.T) {
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	driverPath := filepath.Join(binDir, "driver")
	if err := os.WriteFile(driverPath, []byte("\x7fELFstub"), 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := stubValidConfig(t, dir)
	opts := Options{
		TargetGOOS:  "linux",
		DockerCheck: healthyDockerCheck("v2.40.3"),
		LookupHost: func(host string) ([]string, error) {
			return nil, errors.New("no such host")
		},
	}

	rep := Diagnose(context.Background(), cfg, dir, opts)
	if rep.ExitCode != schema.ExitInconclusive {
		t.Fatalf("exit code = %v (%d), want ExitInconclusive (2)", rep.ExitCode, rep.ExitCode)
	}

	var found bool
	for _, c := range rep.Checks {
		if c.Name == "probe_host" && c.Status == StatusInconclusive {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("probe_host check did not report inconclusive: %+v", rep.Checks)
	}
}

// Requirement: Malformed health probe target reports ExitConfigError (exit 5).
func TestDoctorRefusesMalformedHealthProbe(t *testing.T) {
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	driverPath := filepath.Join(binDir, "driver")
	if err := os.WriteFile(driverPath, []byte("\x7fELFstub"), 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := stubValidConfig(t, dir)
	// Health probe targeting a nonexistent service/node
	cfg.Harness.Health[0].Node = "nonexistent:*"

	opts := Options{
		TargetGOOS:  "linux",
		DockerCheck: healthyDockerCheck("v2.40.3"),
		LookupHost:  loopbackLookup,
	}

	rep := Diagnose(context.Background(), cfg, dir, opts)
	if rep.ExitCode != schema.ExitConfigError {
		t.Fatalf("exit code = %v (%d), want ExitConfigError (5)", rep.ExitCode, rep.ExitCode)
	}

	var found bool
	for _, c := range rep.Checks {
		if c.Name == "health_probes" && c.Status == StatusFailed {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("health_probes check did not report failure: %+v", rep.Checks)
	}
}

// Failing-first requirement 4: A fully healthy project returning 0 with all checks listed.
func TestDoctorPassesFullyHealthyProject(t *testing.T) {
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	driverPath := filepath.Join(binDir, "driver")
	if err := os.WriteFile(driverPath, []byte("\x7fELFstub"), 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := stubValidConfig(t, dir)
	opts := Options{
		TargetGOOS:  "linux",
		DockerCheck: healthyDockerCheck("v2.40.3"),
		LookupHost:  loopbackLookup,
	}

	rep := Diagnose(context.Background(), cfg, dir, opts)
	if rep.ExitCode != schema.ExitPass {
		t.Fatalf("exit code = %v (%d), want ExitPass (0); checks: %+v", rep.ExitCode, rep.ExitCode, rep.Checks)
	}

	// Must report all 5 checks, and every one must have StatusOk
	wantChecks := []string{"docker", "probe_host", "driver", "project_dir", "health_probes"}
	if len(rep.Checks) < len(wantChecks) {
		t.Fatalf("got %d checks, want at least %d: %+v", len(rep.Checks), len(wantChecks), rep.Checks)
	}

	checkMap := make(map[string]Check)
	for _, c := range rep.Checks {
		checkMap[c.Name] = c
	}
	for _, name := range wantChecks {
		c, ok := checkMap[name]
		if !ok {
			t.Errorf("missing check %q in report", name)
			continue
		}
		if c.Status != StatusOk {
			t.Errorf("check %q has status %q, want %q (detail: %s)", name, c.Status, StatusOk, c.Detail)
		}
	}
}

func TestDoctorProbeHostReportsSource(t *testing.T) {
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	driverPath := filepath.Join(binDir, "driver")
	if err := os.WriteFile(driverPath, []byte("\x7fELFstub"), 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := stubValidConfig(t, dir)
	opts := Options{
		TargetGOOS:  "linux",
		DockerCheck: healthyDockerCheck("v2.40.3"),
		LookupHost:  loopbackLookup,
	}

	// Default source
	rep := Diagnose(context.Background(), cfg, dir, opts)
	var detail string
	for _, c := range rep.Checks {
		if c.Name == "probe_host" {
			detail = c.Detail
		}
	}
	if !strings.Contains(detail, "default") {
		t.Errorf("probe_host detail %q does not state default source", detail)
	}

	// Env source
	t.Setenv(probehost.EnvVar, "127.0.0.1")
	_ = probehost.SetFromEnv(os.LookupEnv)
	defer func() {
		_ = probehost.Set(probehost.Default)
	}()

	repEnv := Diagnose(context.Background(), cfg, dir, opts)
	for _, c := range repEnv.Checks {
		if c.Name == "probe_host" {
			if !strings.Contains(c.Detail, probehost.EnvVar) {
				t.Errorf("probe_host detail %q does not mention %s", c.Detail, probehost.EnvVar)
			}
		}
	}
}
