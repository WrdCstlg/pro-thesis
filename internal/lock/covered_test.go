package lock

import (
	"strings"
	"testing"
)

// D-066. harness.health is what availability_after_heal judges a node at, and
// harness.role_probe decides which node `role:leader` hits, so editing either
// must move the digest: an agentic loop that cannot pass the availability
// oracle must not be able to repoint the probe at an always-200 path without
// ORACLE_DRIFT. Mutation: removing either path from CoveredPaths fails the
// matching case.
func TestCoveredHarnessProbeEditsMoveTheDigest(t *testing.T) {
	base := digestOfConfig(t, baseConfig)
	for _, tc := range []struct{ name, from, to string }{
		{"health_probe_path", `probe: "http://{host}:{port}/healthz"`, `probe: "http://{host}:{port}/always-200"`},
		{"health_selector", `node: "kv:*"`, `node: "n1"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed := strings.Replace(baseConfig, tc.from, tc.to, 1)
			if changed == baseConfig {
				t.Fatalf("fixture does not contain %q", tc.from)
			}
			if digestOfConfig(t, changed) == base {
				t.Fatalf("editing %s did not move the digest; harness.health must be covered", tc.name)
			}
		})
	}
	t.Run("role_probe_added", func(t *testing.T) {
		withRole := strings.Replace(baseConfig, "  health:", "  role_probe: { probe: \"./bin/role\", timeout: 10s }\n  health:", 1)
		if withRole == baseConfig {
			t.Fatal("fixture has no health block to anchor on")
		}
		if digestOfConfig(t, withRole) == base {
			t.Fatal("adding harness.role_probe did not move the digest; it must be covered")
		}
	})
}
