// Package doctor implements `thesis doctor`: a pre-flight readiness check
// for the PRO-THESIS harness, container environment, and target configuration.
//
// It reports every check it ran, including the ones that passed, so a reader
// can tell "nothing was wrong" from "nothing was looked at".
package doctor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/WrdCstlg/pro-thesis/internal/driver"
	"github.com/WrdCstlg/pro-thesis/internal/oracle"
	"github.com/WrdCstlg/pro-thesis/internal/probehost"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// CheckStatus indicates whether an individual check passed, failed, or was inconclusive.
type CheckStatus string

const (
	StatusOk           CheckStatus = "ok"
	StatusFailed       CheckStatus = "failed"
	StatusInconclusive CheckStatus = "inconclusive"
)

// Check represents the outcome of one pre-flight check.
type Check struct {
	Name   string      `json:"name"`
	Status CheckStatus `json:"status"`
	Detail string      `json:"detail"`
}

// Report holds the complete list of checks and the resulting normative exit code.
type Report struct {
	Checks   []Check         `json:"checks"`
	ExitCode schema.ExitCode `json:"exit_code"`
}

// DockerChecker checks daemon accessibility and returns compose version string.
type DockerChecker func(ctx context.Context) (composeVersion string, err error)

// HostResolver resolves a hostname to IP addresses.
type HostResolver func(host string) ([]string, error)

// Options configures the doctor run.
type Options struct {
	// TargetGOOS overrides runtime.GOOS for platform binary checks.
	TargetGOOS string
	// DockerCheck overrides the docker daemon and compose check.
	DockerCheck DockerChecker
	// LookupHost overrides DNS / host resolution.
	LookupHost HostResolver
}

// DefaultDockerCheck queries the live docker CLI and docker compose.
func DefaultDockerCheck(ctx context.Context) (string, error) {
	// 1. Check daemon accessibility
	cmdInfo := exec.CommandContext(ctx, "docker", "info", "--format", "{{.ServerVersion}}")
	var infoStderr bytes.Buffer
	cmdInfo.Stderr = &infoStderr
	if err := cmdInfo.Run(); err != nil {
		return "", fmt.Errorf("docker daemon is not reachable: %w (%s)", err, strings.TrimSpace(infoStderr.String()))
	}

	// 2. Check docker compose version
	cmdCompose := exec.CommandContext(ctx, "docker", "compose", "version", "--format", "json")
	var compStdout, compStderr bytes.Buffer
	cmdCompose.Stdout = &compStdout
	cmdCompose.Stderr = &compStderr
	if err := cmdCompose.Run(); err != nil {
		// Fallback to plain version
		cmdPlain := exec.CommandContext(ctx, "docker", "compose", "version")
		out, errPlain := cmdPlain.Output()
		if errPlain != nil {
			return "", fmt.Errorf("docker compose is not available: %w (%s)", err, strings.TrimSpace(compStderr.String()))
		}
		return strings.TrimSpace(string(out)), nil
	}

	var parsed struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(compStdout.Bytes(), &parsed); err == nil && parsed.Version != "" {
		return parsed.Version, nil
	}
	return strings.TrimSpace(compStdout.String()), nil
}

// Diagnose evaluates all 5 doctor checks against the project configuration and host environment.
func Diagnose(ctx context.Context, cfg *schema.Config, projectDir string, opts Options) *Report {
	rep := &Report{
		Checks:   make([]Check, 0, 5),
		ExitCode: schema.ExitPass,
	}

	targetOS := opts.TargetGOOS
	if targetOS == "" {
		targetOS = runtime.GOOS
	}

	dockerFn := opts.DockerCheck
	if dockerFn == nil {
		dockerFn = DefaultDockerCheck
	}

	lookupFn := opts.LookupHost
	if lookupFn == nil {
		lookupFn = net.LookupHost
	}

	// 1. Docker reachable, and docker compose version reports v2
	checkDocker(ctx, dockerFn, rep)

	// 2. Probe host is reachable
	checkProbeHost(lookupFn, rep)

	// 3. Driver command and oracles exist, are executable, and match target platform
	checkDriverAndOracles(cfg, projectDir, targetOS, rep)

	// 4. Project directory is writable
	checkProjectDir(projectDir, rep)

	// 5. Health probe targets well formed and probed nodes declare a port
	checkHealthProbes(cfg, rep)

	// Determine overall exit code: ExitConfigError (5) outranks ExitInconclusive (2).
	var hasConfigError, hasInconclusive bool
	for _, c := range rep.Checks {
		switch c.Status {
		case StatusFailed:
			hasConfigError = true
		case StatusInconclusive:
			hasInconclusive = true
		}
	}

	if hasConfigError {
		rep.ExitCode = schema.ExitConfigError
	} else if hasInconclusive {
		rep.ExitCode = schema.ExitInconclusive
	} else {
		rep.ExitCode = schema.ExitPass
	}

	return rep
}

func checkDocker(ctx context.Context, checker DockerChecker, rep *Report) {
	ver, err := checker(ctx)
	if err != nil {
		rep.Checks = append(rep.Checks, Check{
			Name:   "docker",
			Status: StatusInconclusive,
			Detail: fmt.Sprintf("docker daemon not reachable: %v", err),
		})
		return
	}

	vTrim := strings.TrimPrefix(strings.TrimSpace(ver), "v")
	// If it indicates compose v1 (e.g. 1.x or "docker-compose version 1.")
	if strings.HasPrefix(vTrim, "1.") || strings.Contains(ver, "version 1.") {
		rep.Checks = append(rep.Checks, Check{
			Name:   "docker",
			Status: StatusFailed,
			Detail: fmt.Sprintf("docker compose version is %s: compose v2 required (v1 not supported)", ver),
		})
		return
	}

	rep.Checks = append(rep.Checks, Check{
		Name:   "docker",
		Status: StatusOk,
		Detail: fmt.Sprintf("reachable, compose %s", ver),
	})
}

func checkProbeHost(lookup HostResolver, rep *Report) {
	h := probehost.Host()
	source := "default"
	if _, ok := os.LookupEnv(probehost.EnvVar); ok {
		source = probehost.EnvVar
	}

	addrs, err := lookup(h)
	if err != nil {
		rep.Checks = append(rep.Checks, Check{
			Name:   "probe_host",
			Status: StatusInconclusive,
			Detail: fmt.Sprintf("probe host %q does not resolve: %v (source: %s)", h, err, source),
		})
		return
	}

	rep.Checks = append(rep.Checks, Check{
		Name:   "probe_host",
		Status: StatusOk,
		Detail: fmt.Sprintf("%s resolves to %s (source: %s)", h, strings.Join(addrs, ", "), source),
	})
}

func checkDriverAndOracles(cfg *schema.Config, projectDir, targetOS string, rep *Report) {
	if cfg == nil {
		rep.Checks = append(rep.Checks, Check{
			Name:   "driver",
			Status: StatusFailed,
			Detail: "no configuration loaded",
		})
		return
	}

	// 1. Driver check
	if cfg.Driver.Cmd == "" {
		rep.Checks = append(rep.Checks, Check{
			Name:   "driver",
			Status: StatusFailed,
			Detail: "driver.cmd is empty",
		})
	} else {
		tokens, err := driver.SplitCommand(cfg.Driver.Cmd)
		if err != nil || len(tokens) == 0 {
			rep.Checks = append(rep.Checks, Check{
				Name:   "driver",
				Status: StatusFailed,
				Detail: fmt.Sprintf("cannot parse driver.cmd %q: %v", cfg.Driver.Cmd, err),
			})
		} else {
			prog := tokens[0]
			resolved := resolveForPlatform(prog, projectDir, targetOS)
			if detail, ok := inspectBinary(resolved, targetOS); !ok {
				rep.Checks = append(rep.Checks, Check{
					Name:   "driver",
					Status: StatusFailed,
					Detail: fmt.Sprintf("driver %s: %s", prog, detail),
				})
			} else {
				rep.Checks = append(rep.Checks, Check{
					Name:   "driver",
					Status: StatusOk,
					Detail: fmt.Sprintf("%s is %s", prog, detail),
				})
			}
		}
	}

	// 2. Oracles check under oracles.dir
	oraclesDir := oracle.OraclesDir(cfg, projectDir)
	if oraclesDir != "" {
		if fi, err := os.Stat(oraclesDir); err == nil && fi.IsDir() {
			disc, err := oracle.Discover(oraclesDir)
			if err != nil {
				rep.Checks = append(rep.Checks, Check{
					Name:   "oracles",
					Status: StatusFailed,
					Detail: fmt.Sprintf("discover oracles in %s: %v", oraclesDir, err),
				})
				return
			}
			for _, def := range disc.Definitions {
				tokens, err := driver.SplitCommand(def.Cmd)
				if err != nil || len(tokens) == 0 {
					rep.Checks = append(rep.Checks, Check{
						Name:   "oracles",
						Status: StatusFailed,
						Detail: fmt.Sprintf("oracle %q cmd %q: %v", def.Name, def.Cmd, err),
					})
					return
				}
				prog := tokens[0]
				resolved := resolveForPlatform(prog, projectDir, targetOS)
				if detail, ok := inspectBinary(resolved, targetOS); !ok {
					rep.Checks = append(rep.Checks, Check{
						Name:   "oracles",
						Status: StatusFailed,
						Detail: fmt.Sprintf("oracle %q (%s): %s", def.Name, prog, detail),
					})
					return
				}
			}
			rep.Checks = append(rep.Checks, Check{
				Name:   "oracles",
				Status: StatusOk,
				Detail: fmt.Sprintf("%d external oracle executable(s) verified", len(disc.Definitions)),
			})
		}
	}
}

func resolveForPlatform(prog, projectDir, targetOS string) string {
	resolved := driver.ResolveProgram(prog, projectDir)
	if targetOS == "windows" && !strings.HasSuffix(strings.ToLower(resolved), ".exe") {
		if fi, err := os.Stat(resolved + ".exe"); err == nil && !fi.IsDir() {
			return resolved + ".exe"
		}
	}
	if targetOS == "linux" && strings.HasSuffix(strings.ToLower(resolved), ".exe") {
		stripped := strings.TrimSuffix(resolved, ".exe")
		if fi, err := os.Stat(stripped); err == nil && !fi.IsDir() {
			return stripped
		}
	}
	return resolved
}

func inspectBinary(path, targetOS string) (string, bool) {
	fi, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Sprintf("binary does not exist at %s", path), false
		}
		return fmt.Sprintf("cannot access binary at %s: %v", path, err), false
	}
	if fi.IsDir() {
		return fmt.Sprintf("%s is a directory, not an executable", path), false
	}

	// Read magic bytes
	f, err := os.Open(path)
	if err != nil {
		return fmt.Sprintf("cannot open %s: %v", path, err), false
	}
	defer f.Close()

	var magic [4]byte
	n, _ := f.Read(magic[:])
	buf := magic[:n]

	isPE := bytes.HasPrefix(buf, []byte("MZ"))
	isELF := bytes.HasPrefix(buf, []byte("\x7fELF"))
	isScript := bytes.HasPrefix(buf, []byte("#!"))

	switch targetOS {
	case "linux":
		if isPE {
			return fmt.Sprintf("binary %s is a Windows PE executable (MZ); running on Linux requires a Linux ELF binary", path), false
		}
		if runtime.GOOS == "linux" && fi.Mode()&0111 == 0 && !isScript {
			// On Linux regular binary without executable bits
			return fmt.Sprintf("binary %s lacks executable permissions (chmod +x required)", path), false
		}
		if isELF {
			return "linux/amd64 ELF executable", true
		}
		if isScript {
			return "script executable", true
		}
		return "executable", true

	case "windows":
		if isELF {
			return fmt.Sprintf("binary %s is a Linux ELF executable; running on Windows requires a Windows PE binary", path), false
		}
		if isPE {
			return "Windows PE executable", true
		}
		if isScript || strings.HasSuffix(strings.ToLower(path), ".bat") || strings.HasSuffix(strings.ToLower(path), ".cmd") || strings.HasSuffix(strings.ToLower(path), ".ps1") {
			return "script executable", true
		}
		return "executable", true

	default:
		return "executable", true
	}
}

func checkProjectDir(projectDir string, rep *Report) {
	if projectDir == "" {
		projectDir = "."
	}
	tmp, err := os.CreateTemp(projectDir, ".thesis_doctor_probe_*")
	if err != nil {
		rep.Checks = append(rep.Checks, Check{
			Name:   "project_dir",
			Status: StatusFailed,
			Detail: fmt.Sprintf("project directory %s is not writable: %v", projectDir, err),
		})
		return
	}
	tmpPath := tmp.Name()
	_ = tmp.Close()
	_ = os.Remove(tmpPath)

	rep.Checks = append(rep.Checks, Check{
		Name:   "project_dir",
		Status: StatusOk,
		Detail: fmt.Sprintf("%s is writable", projectDir),
	})
}

func checkHealthProbes(cfg *schema.Config, rep *Report) {
	if cfg == nil {
		return
	}

	if len(cfg.Harness.Health) == 0 {
		rep.Checks = append(rep.Checks, Check{
			Name:   "health_probes",
			Status: StatusOk,
			Detail: "no health probes declared",
		})
		return
	}

	probedNodes := make(map[string]bool)

	for i, hp := range cfg.Harness.Health {
		if strings.TrimSpace(hp.Probe) == "" {
			rep.Checks = append(rep.Checks, Check{
				Name:   "health_probes",
				Status: StatusFailed,
				Detail: fmt.Sprintf("harness.health[%d]: probe template is empty", i),
			})
			return
		}

		t, err := schema.ParseTarget(hp.Node)
		if err != nil {
			rep.Checks = append(rep.Checks, Check{
				Name:   "health_probes",
				Status: StatusFailed,
				Detail: fmt.Sprintf("harness.health[%d].node %q: invalid target expression: %v", i, hp.Node, err),
			})
			return
		}

		var matched []schema.NodeConfig
		switch t.Kind {
		case schema.TargetNode:
			if n, ok := cfg.Node(t.Node); ok {
				matched = append(matched, n)
			}
		case schema.TargetWildcard:
			matched = cfg.NodesOfService(t.Service)
		}

		if len(matched) == 0 {
			rep.Checks = append(rep.Checks, Check{
				Name:   "health_probes",
				Status: StatusFailed,
				Detail: fmt.Sprintf("harness.health[%d].node %q matches no node in harness.nodes", i, hp.Node),
			})
			return
		}

		for _, n := range matched {
			probedNodes[n.ID] = true
		}
	}

	// Verify all probed nodes declare a port
	for nodeID := range probedNodes {
		n, ok := cfg.Node(nodeID)
		if !ok {
			continue
		}
		port := n.ClientPort()
		if port <= 0 || port > 65535 {
			rep.Checks = append(rep.Checks, Check{
				Name:   "health_probes",
				Status: StatusFailed,
				Detail: fmt.Sprintf("node %q is probed but has invalid client port %d", nodeID, port),
			})
			return
		}
	}

	rep.Checks = append(rep.Checks, Check{
		Name:   "health_probes",
		Status: StatusOk,
		Detail: fmt.Sprintf("%d health probe(s) well formed, %d node(s) probed with valid port", len(cfg.Harness.Health), len(probedNodes)),
	})
}
