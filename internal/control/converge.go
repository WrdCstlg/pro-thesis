package control

import (
	"fmt"
	"net/http"
	"time"

	"github.com/WrdCstlg/pro-thesis/internal/harness"
	"github.com/WrdCstlg/pro-thesis/internal/oracle"
	"github.com/WrdCstlg/pro-thesis/internal/recorder"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// convergenceProbes takes one health probe per node during QUIESCE, evidence
// availability_after_heal judges.
//
// Each node is probed at the URL ITS OWN `harness.health` entry declares (the
// same template telemetry samples) expanded against the port compose actually
// published. Before this existed the path was the literal "/healthz": the
// reference fixture's path, which the first third-party target (targets/etcd,
// whose endpoint is /health) answered with 404 on every node, so the oracle
// reported all three members "failed to become available after HEAL" on a
// cluster that was fine. The probe the oracle judged was never the probe the
// config declared.
//
// Three rules, each pinned by a test:
//
//   - A node no health entry covers gets NO observation here rather than an
//     invented URL. The oracle is told which nodes have a declared probe
//     (oracle.Input.HealthProbed, healthProbedNodes) and reports the others as
//     not judged rather than as unprobed (OQ-063, D-066).
//   - Any 2xx answer is available, the rule WaitHealthy and telemetry already
//     apply; "== 200" here would call a 204 unavailable that BOOT accepted.
//   - Every observation is stamped by `now` immediately before its own GET.
//     With a 3 s client timeout the third node's attempt can be seconds after
//     the first, and a causal timeline that put them at one instant would
//     misplace it.
func convergenceProbes(cfg *schema.Config, top *recorder.Topology, now func() int64, client *http.Client) ([]oracle.ProbeObservation, error) {
	templates, err := harness.ProbeTemplates(cfg)
	if err != nil {
		return nil, err
	}
	var probes []oracle.ProbeObservation
	for _, n := range cfg.Harness.Nodes {
		tmpl, declared := templates[n.ID]
		if !declared {
			continue
		}
		var port int64
		if nb, ok := findNodeBinding(top, n.ID); ok {
			port = nb.HostPort
		}
		probeURL := harness.ExpandProbe(tmpl, harness.ProbeHost, port)
		tms := now()
		start := time.Now()
		resp, pErr := client.Get(probeURL)
		lat := time.Since(start).Milliseconds()
		ok := pErr == nil && resp != nil && resp.StatusCode/100 == 2
		errStr := ""
		if pErr != nil {
			errStr = pErr.Error()
		} else if resp != nil && resp.StatusCode/100 != 2 {
			errStr = fmt.Sprintf("HTTP status %d", resp.StatusCode)
		}
		if resp != nil {
			_ = resp.Body.Close()
		}
		probes = append(probes, oracle.ProbeObservation{
			NodeID:    n.ID,
			Target:    probeURL,
			TMS:       tms,
			OK:        ok,
			LatencyMS: lat,
			Err:       errStr,
		})
	}
	return probes, nil
}

// healthProbedNodes lists, in harness.nodes order, the nodes harness.health
// declares a probe for: what oracle.Input.HealthProbed carries. Nil when the
// config is absent or its selectors do not resolve, which the oracle reads as
// "not recorded" and judges every node as before.
func healthProbedNodes(cfg *schema.Config) []string {
	if cfg == nil {
		return nil
	}
	templates, err := harness.ProbeTemplates(cfg)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(templates))
	for _, n := range cfg.Harness.Nodes {
		if _, ok := templates[n.ID]; ok {
			out = append(out, n.ID)
		}
	}
	return out
}
