package harness

import (
	"strings"
	"testing"

	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// composeArgs flag placement is load-bearing and got it wrong once already.
func TestComposeArgsPutProgressBeforeSubcommand(t *testing.T) {
	got := composeArgs("proj", []string{"a.yaml", "b.yaml"}, "up", "--detach")
	want := []string{"compose", "--project-name", "proj", "--file", "a.yaml", "--file", "b.yaml", "up", "--detach"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("got %v\nwant %v", got, want)
	}
	// The project name and file set must precede the subcommand: --progress and
	// --project-name are ROOT-only flags on `docker compose`, and compose
	// resolves a DIFFERENT project if the file set differs between up and down.
	subIdx := indexOf(got, "up")
	for _, rootFlag := range []string{"--project-name", "--file"} {
		if i := indexOf(got, rootFlag); i > subIdx {
			t.Fatalf("%s appears after the subcommand; it is a root-only flag", rootFlag)
		}
	}
}

func indexOf(ss []string, s string) int {
	for i, v := range ss {
		if v == s {
			return i
		}
	}
	return -1
}

// `docker compose port` reports the BIND address, which on this host is
// 0.0.0.0. That is not a destination: only the port number is usable, and
// every probe is addressed to 127.0.0.1.
func TestParsePublishedPort(t *testing.T) {
	cases := map[string]struct {
		in   string
		want int64
		bad  bool
	}{
		"windows 0.0.0.0":  {in: "0.0.0.0:18081\n", want: 18081},
		"loopback":         {in: "127.0.0.1:18082\n", want: 18082},
		"ipv6":             {in: "[::]:18083\n", want: 18083},
		"two lines":        {in: "0.0.0.0:18081\n[::]:18081\n", want: 18081},
		"empty":            {in: "", bad: true},
		"no colon":         {in: "18081\n", bad: true},
		"not a number":     {in: "0.0.0.0:http\n", bad: true},
		"out of range":     {in: "0.0.0.0:99999\n", bad: true},
		"zero is not real": {in: "0.0.0.0:0\n", bad: true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := parsePublishedPort(tc.in)
			if tc.bad {
				if err == nil {
					t.Fatalf("accepted %q -> %d; a bad port must not silently become a probe target", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("%q: %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("%q -> %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

// The CLI's JSON output is an unversioned contract: some versions emit an
// array, others one object per line. Both must parse or `up` breaks on a
// Docker upgrade nobody controls.
func TestParsePSHandlesBothShapes(t *testing.T) {
	array := `[{"ID":"abc","Name":"p-n1","Service":"n1","State":"running","Health":"healthy"}]`
	lines := `{"ID":"abc","Name":"p-n1","Service":"n1","State":"running","Health":"healthy"}
{"ID":"def","Name":"p-n2","Service":"n2","State":"running","Health":"healthy"}`

	a, err := parsePS(array)
	if err != nil || len(a) != 1 || a[0].Service != "n1" {
		t.Fatalf("array shape: %v %+v", err, a)
	}
	l, err := parsePS(lines)
	if err != nil || len(l) != 2 || l[1].Service != "n2" {
		t.Fatalf("line shape: %v %+v", err, l)
	}
	if e, err := parsePS("   \n"); err != nil || len(e) != 0 {
		t.Fatalf("empty: %v %+v", err, e)
	}
}

// The project name must be stable across processes, because `up` and `down` are
// separate OS invocations and compose addresses a project by name.
func TestProjectNameIsStableAndScoped(t *testing.T) {
	cfg := &schema.Config{Name: "my service!"}
	cfg.Harness.File = "docker-compose.yaml"

	a := ProjectName(cfg, "C:\\proj")
	b := ProjectName(cfg, "C:\\proj")
	if a != b {
		t.Fatalf("project name is not stable: %s vs %s", a, b)
	}
	if c := ProjectName(cfg, "C:\\other"); c == a {
		t.Fatal("two checkouts of the same config produced the same project name; they would collide")
	}
	if !strings.HasPrefix(a, "thesis-") {
		t.Fatalf("project name %q must start with thesis- so the reaper can find leaked projects", a)
	}
	for _, r := range a {
		valid := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_'
		if !valid {
			t.Fatalf("project name %q contains %q, which compose rejects", a, r)
		}
	}
}

func TestExpandProbe(t *testing.T) {
	got := ExpandProbe("http://{host}:{port}/healthz", "127.0.0.1", 18081)
	if want := "http://127.0.0.1:18081/healthz"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// A health probe matching nothing must be an error, never a vacuous pass. The
// fixture hit exactly this: one `kv:*` entry resolved to zero nodes, so a
// literal reading health-checked NOTHING and `up` would have reported success
// over a cluster that never formed. See OPEN_QUESTIONS.md OQ-016.
func TestResolveProbeTargets(t *testing.T) {
	cfg := &schema.Config{}
	cfg.Harness.Nodes = []schema.NodeConfig{
		{ID: "kv-n1", Service: "kv-n1"},
		{ID: "kv-n2", Service: "kv-n2"},
		{ID: "pg", Service: "postgres"},
	}

	if got, err := ResolveProbeTargets(cfg, "kv-n1"); err != nil || len(got) != 1 || got[0] != "kv-n1" {
		t.Fatalf("node id: %v %v", got, err)
	}
	if got, err := ResolveProbeTargets(cfg, "kv-n1:*"); err != nil || len(got) != 1 {
		t.Fatalf("service wildcard: %v %v", got, err)
	}
	// A wildcard over a service no node declares resolves to nothing. That is
	// not an error here: WaitHealthy turns it into one, with the message that
	// names the probe.
	if got, err := ResolveProbeTargets(cfg, "kv:*"); err != nil || len(got) != 0 {
		t.Fatalf("unmatched wildcard should resolve empty, got %v %v", got, err)
	}
	if got, err := ResolveProbeTargets(cfg, "nope"); err != nil || got != nil {
		t.Fatalf("unknown node should resolve empty, got %v %v", got, err)
	}
	// Phase 2 selector forms must be rejected loudly rather than matching
	// nothing and passing vacuously.
	for _, sel := range []string{"role:leader", "minority(kv)", "n1<->n2"} {
		if _, err := ResolveProbeTargets(cfg, sel); err == nil {
			t.Fatalf("selector %q was accepted; Phase 0 cannot resolve it and must say so", sel)
		}
	}
	if _, err := ResolveProbeTargets(cfg, "  "); err == nil {
		t.Fatal("empty selector accepted")
	}
}

// The directive names `process` as a v1 backend, but every fault it would need
// is a Linux primitive. Refusing is correct; degrading to "injects nothing"
// would report PASS on a world whose faults never fired.
func TestProcessBackendRefusesOnWindows(t *testing.T) {
	cfg := &schema.Config{}
	cfg.Harness.Backend = schema.BackendProcess
	if _, err := New(cfg); err == nil {
		t.Fatal("process backend was accepted; it cannot inject proc.pause or net.partition on this host")
	}

	cfg.Harness.Backend = schema.BackendCompose
	b, err := New(cfg)
	if err != nil {
		t.Fatalf("compose backend rejected: %v", err)
	}
	if b.Name() != schema.BackendCompose {
		t.Fatalf("got backend %q", b.Name())
	}

	for _, unsupported := range []schema.Backend{schema.BackendK8s, schema.BackendSim} {
		cfg.Harness.Backend = unsupported
		if _, err := New(cfg); err == nil {
			t.Fatalf("backend %q accepted; it is post-v1", unsupported)
		}
	}
}
