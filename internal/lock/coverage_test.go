package lock

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The settings that decide whether a fault produces a violation used to sit
// outside the digest: the compose file (election timeouts, Raft multipliers,
// Sentinel quorum), the node list, the steady-state probe and the perturber's
// constraints. Two runs could report the same lock digest while running
// different systems. Each now moves the digest.
func TestTheNewlyCoveredKeysMoveTheDigest(t *testing.T) {
	base := digestOfConfig(t, baseConfig)
	cases := map[string]func(string) string{
		"harness.file": func(c string) string {
			return strings.Replace(c, "file: docker-compose.yaml", "file: other-compose.yaml", 1)
		},
		"harness.nodes": func(c string) string {
			return strings.Replace(c, "role_hint: replica", "role_hint: leader", 1)
		},
		"harness.steady_state": func(c string) string {
			return strings.Replace(c, "  health:\n", "  steady_state:\n    probe: ./bin/steady\n    timeout: 20s\n  health:\n", 1)
		},
		"harness.variant_env": func(c string) string {
			return strings.Replace(c, "  backend: compose\n", "  backend: compose\n  variant_env: KV_VARIANT\n", 1)
		},
		"perturber.constraints": func(c string) string {
			return strings.Replace(c, "minority of kv", "majority of kv", 1)
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			changed := mutate(baseConfig)
			if changed == baseConfig {
				t.Fatal("setup: the mutation did not apply")
			}
			if got := digestOfConfig(t, changed); got == base {
				t.Fatalf("editing %s left the digest unchanged", name)
			}
		})
	}
}

func TestTheComposeFileContentIsDigested(t *testing.T) {
	dir := newProject(t, baseConfig, nil)
	noCompose := digestOf(t, dir)

	compose := filepath.Join(dir, "docker-compose.yaml")
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(compose, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("services:\n  kv-n1:\n    image: kv:1\n    environment: { ELECTION_MS: \"600\" }\n")
	withCompose := digestOf(t, dir)
	if withCompose == noCompose {
		t.Fatal("adding the compose file the config names left the digest unchanged")
	}

	raw, _ := os.ReadFile(filepath.Join(dir, "prothesis.yaml"))
	before, err := Build(dir, raw, ".prothesis/oracles")
	if err != nil {
		t.Fatal(err)
	}
	write("services:\n  kv-n1:\n    image: kv:1\n    environment: { ELECTION_MS: \"60000\" }\n")
	if got := digestOf(t, dir); got == withCompose {
		t.Fatal("editing the compose file (an election timeout) left the digest unchanged")
	}
	after, err := Build(dir, raw, ".prothesis/oracles")
	if err != nil {
		t.Fatal(err)
	}
	changes := Diff(before, after)
	if len(changes) != 1 || !strings.Contains(changes[0].String(), "docker-compose.yaml") {
		t.Fatalf("the diagnosis does not name the compose file: %v", changes)
	}
}

// A lock written before the manifest carried files must still hash to its own
// recorded digest when read back, or every existing lock would be reported as
// hand-edited the moment this version reads it.
func TestALockWrittenBeforeFilesExistedStillChecksItself(t *testing.T) {
	old := Manifest{
		Schema:  "prothesis.lock_manifest/v1",
		Oracles: []OracleFile{{Path: "a.yaml", SHA256: "sha256:00", Size: 3}},
		Config:  []ConfigEntry{{Path: "driver.cmd", Kind: KindScalar, Value: "./bin/x"}},
	}
	want, err := old.Digest()
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), `"topology"`) {
		t.Fatalf("a manifest with no topology file serializes a topology key: %s", b)
	}
	var back Manifest
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if got, err := back.Digest(); err != nil || got != want {
		t.Fatalf("round-tripped digest %s (err %v), want %s", got, err, want)
	}
}
