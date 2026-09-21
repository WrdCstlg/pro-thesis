package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/WrdCstlg/pro-thesis/internal/oracle"
	"github.com/WrdCstlg/pro-thesis/internal/recorder"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

func cmdInit(_ context.Context, g globals, args []string) schema.ExitCode {
	var force bool
	for _, a := range args {
		switch a {
		case "--force":
			force = true
		default:
			errorf("init: unknown flag %q", a)
			return schema.ExitConfigError
		}
	}

	dir, err := filepath.Abs(filepath.Dir(g.configPath))
	if err != nil {
		errorf("init: %v", err)
		return schema.ExitConfigError
	}
	cfgPath := filepath.Join(dir, filepath.Base(g.configPath))

	// Refuse to clobber. Scaffolding over an existing config would be a silent
	// way to reset a perturber allow-list or a profile budget, which the
	// anti-gaming rules treat as a lock change.
	if _, err := os.Stat(cfgPath); err == nil && !force {
		errorf("init: %s already exists (pass --force to overwrite)", cfgPath)
		return schema.ExitConfigError
	}

	if err := os.WriteFile(cfgPath, []byte(scaffoldConfig), 0o644); err != nil {
		errorf("init: write %s: %v", cfgPath, err)
		return schema.ExitInconclusive
	}

	// The state directory layout. oracles/ and regressions/ are committed
	// artifacts, not scratch: the lock manifest covers them and the regression
	// corpus is append-only. runs/ is the only transient one.
	for _, sub := range []string{"oracles", "regressions", "runs"} {
		if err := os.MkdirAll(filepath.Join(recorder.StateDir(dir), sub), 0o755); err != nil {
			errorf("init: %v", err)
			return schema.ExitInconclusive
		}
	}

	// Scaffold the oracles directory rather than leaving it empty. The
	// definition format is additive, so an empty directory leaves a new project
	// with nothing to copy and no format to read. The reference definition is
	// written INERT (a .example extension Discover does not pick up) so that a
	// project that has not built a checker yet is not handed an oracle it never
	// asked for, reporting exit 2 on every run. See DECISIONS.md D-036.
	oraclesDir := filepath.Join(recorder.StateDir(dir), "oracles")
	written, err := oracle.Scaffold(oraclesDir)
	if err != nil {
		errorf("init: %v", err)
		return schema.ExitInconclusive
	}
	for _, w := range written {
		infof(g, "wrote %s", w)
	}

	// .gitattributes is not optional and not cosmetic. `.thesis` worlds and the
	// lock are content-addressed over their exact bytes; on a Windows checkout
	// with core.autocrlf=true git would rewrite every LF, the canonical decoder
	// would reject the file, and every world_hash in the committed regression
	// corpus would break at once: surfacing in someone else's clone, long
	// after the mistake.
	ga := filepath.Join(dir, ".gitattributes")
	if _, err := os.Stat(ga); os.IsNotExist(err) {
		if err := os.WriteFile(ga, []byte(scaffoldGitattributes), 0o644); err != nil {
			errorf("init: write %s: %v", ga, err)
			return schema.ExitInconclusive
		}
		infof(g, "wrote .gitattributes")
	}

	// Verify the scaffold satisfies the validator it will be read by. A
	// scaffold that its own tool rejects is worse than none.
	if data, err := os.ReadFile(cfgPath); err == nil {
		if cfg, err := schema.DecodeConfig(data); err != nil {
			errorf("init: the scaffolded config does not decode: %v", err)
			return schema.ExitConfigError
		} else if err := cfg.ValidateForTopology(); err != nil {
			errorf("init: the scaffolded config does not validate: %v", err)
			return schema.ExitConfigError
		}
	}

	infof(g, "wrote %s", cfgPath)
	infof(g, "created %s/{oracles,regressions,runs}", recorder.StateDir(dir))
	infof(g, "")
	infof(g, "Next: point harness.file at your compose file, list your nodes, then run `thesis up`.")
	return schema.ExitPass
}

var _ = fmt.Sprintf

const scaffoldGitattributes = `# Line endings are load-bearing here, not cosmetic.
#
# .thesis worlds and .prothesis/lock are content-addressed over their exact
# bytes. Without this file, a Windows checkout with core.autocrlf=true rewrites
# every LF and every world_hash in the regression corpus breaks at once.
#
# Do NOT use the "binary" attribute: it implies -diff, which would hide lock
# and regression-corpus changes behind "Binary files differ" and defeat the
# human review the anti-gaming rules depend on.

*.thesis        text eol=lf
*.jsonl         text eol=lf
.prothesis/lock text eol=lf
*.sh            text eol=lf
`

// scaffoldConfig is the prothesis.yaml `thesis init` writes.
//
// It follows the directive's section 4.2 sample, with two deliberate
// departures, both commented in place: one compose service per node (container
// IPs are not routable from a Windows host, so each node needs its own
// published port), and explicit node health targets rather than the `kv:*`
// wildcard (see OPEN_QUESTIONS.md OQ-016).
const scaffoldConfig = `version: prothesis/v1
name: my-service

harness:
  backend: compose
  file: docker-compose.yaml

  # Logical nodes. Fault targets in the grammar address these ids, not compose
  # service names. One service per node: container IPs are not routable from a
  # Windows host, so every node needs its own published port for health probes
  # and for the driver to reach it.
  nodes:
    - { id: n1, service: n1, role_hint: replica }
    - { id: n2, service: n2, role_hint: replica }
    - { id: n3, service: n3, role_hint: replica }

  # {host} is always 127.0.0.1 and {port} is the node's published host port.
  health:
    - node: "n1"
      probe: "http://{host}:{port}/healthz"
      timeout: 30s
    - node: "n2"
      probe: "http://{host}:{port}/healthz"
      timeout: 30s
    - node: "n3"
      probe: "http://{host}:{port}/healthz"
      timeout: 30s

  # Steady state is a cluster-wide condition that no single URL can express.
  # Run it inside the cluster; that also avoids depending on a host shell.
  steady_state:
    probe: "docker compose exec -T n1 /steady"
    timeout: 60s

driver:
  cmd: "./bin/loadgen --history {history_path} --seed {seed} --profile {profile}"
  profiles:
    smoke: { clients: 4,  ops: 500,    mix: { read: 0.5, write: 0.5 } }
    gate:  { clients: 16, ops: 20000,  mix: { read: 0.4, write: 0.4, txn: 0.2 } }
    soak:  { clients: 64, ops: 500000, mix: { read: 0.3, write: 0.4, txn: 0.2, admin: 0.1 } }

perturber:
  budget:
    max_concurrent_faults: 3
    max_faults_per_world: 24
  # The fault space. This list is covered by .prothesis/lock: removing a kind,
  # narrowing a budget, or shortening a profile budget is a lock change and
  # needs "thesis oracles lock --reason ..." in a separate, reviewed commit.
  allow:
    - net.partition
    - net.latency
    - net.loss
    - proc.kill
    - proc.pause
    - clock.skew
    - io.latency
  deny:
    - io.fill
  constraints:
    - "never partition more than minority of kv"

oracles:
  dir: .prothesis/oracles
  builtin:
    - no_crash
    - no_panic_log
    - no_unbounded_queue
    - resource_return_to_baseline
    - availability_after_heal
    - no_stuck_op

profiles:
  smoke: { budget: 90s, worlds: 3,  driver_profile: smoke }
  gate:  { budget: 10m, worlds: 30, driver_profile: gate }
  soak:  { budget: 8h,  worlds: -1, driver_profile: soak, search: true }

artifacts:
  dir: .prothesis/runs
  retain_passing: 3
  retain_failing: all
`
