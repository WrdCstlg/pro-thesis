package harness

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/WrdCstlg/pro-thesis/internal/probehost"
	"github.com/WrdCstlg/pro-thesis/internal/recorder"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// ProbeHost is the address every health probe is sent to.
//
// Never 0.0.0.0. Container IPs are NOT routable from a Windows host under
// Docker Desktop's Linux engine; verified directly: a container at 172.17.0.2
// is unreachable from PowerShell. So a host-side probe has exactly one way in,
// the published port. `docker compose port` reports the BIND address (0.0.0.0),
// which is not a destination; only its port number is used. See D-010.
//
// It defaults to loopback, which is what D-010 measured and what every existing
// project keeps. It stopped being a constant when the harness itself had to run
// inside a container, where loopback is the container's own and the target's
// published ports are somewhere else entirely (D-087).
func ProbeHost() string { return probehost.Host() }

// WaitHealthy blocks until every declared health probe passes, or the probe's
// timeout expires.
//
// A health failure is an ENVIRONMENT failure (exit 2, INCONCLUSIVE), not a
// config error: the config was valid, the system did not come up.
func WaitHealthy(ctx context.Context, cfg *schema.Config, top *recorder.Topology) error {
	if len(cfg.Harness.Health) == 0 {
		return nil
	}
	byNode := map[string]recorder.NodeBinding{}
	for _, n := range top.Nodes {
		byNode[n.ID] = n
	}

	for _, hp := range cfg.Harness.Health {
		targets, err := ResolveProbeTargets(cfg, hp.Node)
		if err != nil {
			return err
		}
		if len(targets) == 0 {
			// A probe that matches nothing must be an error, never a silent
			// pass. The fixture hit exactly this: a single `kv:*` entry
			// resolved to zero nodes, so a literal reading health-checked
			// NOTHING and `up` would have reported success over a cluster that
			// never formed. See OPEN_QUESTIONS.md OQ-016.
			return fmt.Errorf("harness: health probe target %q matches no node in harness.nodes; "+
				"a probe that matches nothing would pass vacuously", hp.Node)
		}
		for _, nodeID := range targets {
			nb, ok := byNode[nodeID]
			if !ok {
				return fmt.Errorf("harness: health probe targets node %q, which the topology does not bind", nodeID)
			}
			if nb.HostPort == 0 {
				return fmt.Errorf("harness: node %q publishes no host port, so probe %q cannot reach it "+
					"(container IPs are not routable from this host; publish the client port to 127.0.0.1)",
					nodeID, hp.Probe)
			}
			url := ExpandProbe(hp.Probe, ProbeHost(), nb.HostPort)
			if err := pollHTTP(ctx, url, hp.Timeout.Std()); err != nil {
				return fmt.Errorf("harness: node %q failed its health probe: %w", nodeID, err)
			}
		}
	}
	return nil
}

// ExpandProbe substitutes {host} and {port} in a probe template.
//
// Exported because internal/telemetry samples the SAME probe on the SAME
// expansion every interval. Two copies of this two-line function would be two
// definitions of what a probe URL is, and they would drift the moment a third
// placeholder is added.
func ExpandProbe(tmpl, host string, port int64) string {
	r := strings.NewReplacer("{host}", host, "{port}", strconv.FormatInt(port, 10))
	return r.Replace(tmpl)
}

// ResolveProbeTargets resolves a health probe's node selector to logical node
// ids.
//
// Phase 0 supports the plain node id and the `service:*` wildcard. Roles,
// edges and quorum forms are Phase 2 concerns and are rejected here rather than
// silently matching nothing.
//
// Exported for the same reason as ExpandProbe: telemetry must attribute a probe
// to exactly the nodes WaitHealthy attributes it to, or a node would be probed
// at BOOT and then silently dropped from the sampled series.
func ResolveProbeTargets(cfg *schema.Config, sel string) ([]string, error) {
	sel = strings.TrimSpace(sel)
	if sel == "" {
		return nil, fmt.Errorf("harness: empty health probe target")
	}
	if strings.HasSuffix(sel, ":*") {
		service := strings.TrimSuffix(sel, ":*")
		var out []string
		for _, n := range cfg.NodesOfService(service) {
			out = append(out, n.ID)
		}
		return out, nil
	}
	if strings.ContainsAny(sel, "(<:") {
		return nil, fmt.Errorf("harness: health probe target %q uses a selector form "+
			"(role, edge or quorum) that Phase 0 does not resolve; use a node id or `service:*`", sel)
	}
	if _, ok := cfg.Node(sel); !ok {
		return nil, nil
	}
	return []string{sel}, nil
}

// pollHTTP polls url until it answers 2xx or the timeout expires.
func pollHTTP(ctx context.Context, url string, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 3 * time.Second}

	var last error
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return fmt.Errorf("%s: %w", url, err)
		}
		resp, err := client.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				return nil
			}
			last = fmt.Errorf("%s: HTTP %d", url, resp.StatusCode)
		} else {
			last = fmt.Errorf("%s: %w", url, err)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("not healthy within %s: %w", timeout, last)
		}
		// Fixed 250ms interval. Deliberately NOT jittered from the recorder's
		// PRNG: BOOT precedes the virtual clock's origin (which is DRIVE start),
		// so there is no clock to schedule against, and making boot timing part
		// of the seed would put host-dependent behaviour inside the world.
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}
