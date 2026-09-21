package harness

import "github.com/WrdCstlg/pro-thesis/pkg/schema"

// ProbeTemplates maps every node that `harness.health` covers to the probe
// template that covers it; the first declaration naming a node wins.
//
// A node no entry selects is ABSENT from the map. It has no probe, and a
// caller wanting one must not invent it: "/healthz" is the reference fixture's
// path, not a convention, and the first third-party target (targets/etcd,
// whose endpoint is /health) answered an invented one with 404 on every node
// (OQ-063).
//
// Two consumers resolve through here (the telemetry sampler and the post-HEAL
// convergence probe) so the URL availability_after_heal judges a node at is
// the URL telemetry sampled it at. BOOT's WaitHealthy is deliberately NOT one
// of them: it waits on EVERY entry that names a node, which is stricter, and a
// node two entries cover must answer both before a world starts. For the
// single-entry case every ordinary config is in, the three agree byte for
// byte.
func ProbeTemplates(cfg *schema.Config) (map[string]string, error) {
	out := map[string]string{}
	for _, hp := range cfg.Harness.Health {
		nodes, err := ResolveProbeTargets(cfg, hp.Node)
		if err != nil {
			return nil, err
		}
		for _, n := range nodes {
			if _, taken := out[n]; !taken {
				out[n] = hp.Probe
			}
		}
	}
	return out, nil
}
