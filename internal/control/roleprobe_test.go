package control

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/WrdCstlg/pro-thesis/internal/perturber"
	"github.com/WrdCstlg/pro-thesis/internal/recorder"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

func etcdNodes() []perturber.Node {
	return []perturber.Node{
		{ID: "etcd-n1", Service: "etcd", HostPort: 12379},
		{ID: "etcd-n2", Service: "etcd", HostPort: 12380},
		{ID: "etcd-n3", Service: "etcd", HostPort: 12381},
	}
}

func etcdTopology() *recorder.Topology {
	return &recorder.Topology{Nodes: []recorder.NodeBinding{
		{ID: "etcd-n1", HostPort: 12379},
		{ID: "etcd-n2", HostPort: 12380},
		{ID: "etcd-n3", HostPort: 12381},
	}}
}

// The probe's output binds roles; a node it does not name is unobserved, not
// a follower; and the probe runs in the project directory with the same
// PROTHESIS_* environment the steady-state probe gets.
func TestExecRoleObserverBindsRolesFromTheProbeOutput(t *testing.T) {
	var gotArgv []string
	var gotDir string
	var gotEnv []string
	o := &ExecRoleObserver{
		Probe:      "./bin/etcdrole --timeout 2s",
		Timeout:    5 * time.Second,
		ProjectDir: "C:/proj/etcd",
		Topology:   etcdTopology(),
		Exec: func(_ context.Context, argv []string, dir string, env []string) ([]byte, error) {
			gotArgv, gotDir, gotEnv = argv, dir, env
			return []byte("{\"node\":\"etcd-n2\",\"role\":\"Leader\",\"term\":7}\n" +
				"{\"node\":\"etcd-n1\",\"role\":\"follower\",\"term\":7}\n" +
				"\n" +
				"{\"node\":\"etcd-n9\",\"role\":\"follower\",\"term\":7}\n"), nil
		},
	}
	obs, err := o.ObserveRoles(context.Background(), etcdNodes())
	if err != nil {
		t.Fatal(err)
	}
	if len(obs) != 3 {
		t.Fatalf("want one observation per node, got %d", len(obs))
	}
	if obs[0].NodeID != "etcd-n1" || obs[0].Role != "follower" || obs[0].Term != 7 || obs[0].Err != nil {
		t.Fatalf("n1: %+v", obs[0])
	}
	if obs[1].NodeID != "etcd-n2" || obs[1].Role != "leader" || obs[1].Term != 7 || obs[1].Err != nil {
		t.Fatalf("n2: role must be lower-cased and bound: %+v", obs[1])
	}
	if obs[2].NodeID != "etcd-n3" || obs[2].Role != "" || obs[2].Err == nil {
		t.Fatalf("n3 was not reported and must be UNOBSERVED, not a follower: %+v", obs[2])
	}
	if strings.Join(gotArgv, " ") != "./bin/etcdrole --timeout 2s" {
		t.Fatalf("argv: %v", gotArgv)
	}
	if gotDir != "C:/proj/etcd" {
		t.Fatalf("dir: %q", gotDir)
	}
	var sawNodes, sawPort bool
	for _, e := range gotEnv {
		if e == "PROTHESIS_NODES=etcd-n1,etcd-n2,etcd-n3" {
			sawNodes = true
		}
		if e == "PROTHESIS_PORT_ETCD_N2=12380" {
			sawPort = true
		}
	}
	if !sawNodes || !sawPort {
		t.Fatalf("the probe did not receive the topology environment: nodes=%v port=%v", sawNodes, sawPort)
	}
}

func TestExecRoleObserverErrorLinesAreUnobservedNodes(t *testing.T) {
	o := &ExecRoleObserver{
		Probe: "./bin/etcdrole", Topology: etcdTopology(),
		Exec: func(context.Context, []string, string, []string) ([]byte, error) {
			return []byte("{\"node\":\"etcd-n1\",\"role\":\"leader\",\"term\":3}\n" +
				"{\"node\":\"etcd-n2\",\"error\":\"reports no leader\"}\n" +
				"{\"node\":\"etcd-n3\",\"role\":\"\"}\n"), nil
		},
	}
	obs, err := o.ObserveRoles(context.Background(), etcdNodes())
	if err != nil {
		t.Fatal(err)
	}
	if obs[1].Err == nil || obs[1].Err.Error() != "reports no leader" || obs[1].Role != "" {
		t.Fatalf("n2: %+v", obs[1])
	}
	if obs[2].Err == nil || obs[2].Role != "" {
		t.Fatalf("n3 reported an empty role and must be unobserved: %+v", obs[2])
	}
}

func TestExecRoleObserverFailsClosedOnExitErrorOrGarbage(t *testing.T) {
	failing := &ExecRoleObserver{Probe: "./bin/etcdrole", Topology: etcdTopology(),
		Exec: func(context.Context, []string, string, []string) ([]byte, error) {
			return nil, errors.New("exit status 1: boom")
		}}
	if _, err := failing.ObserveRoles(context.Background(), etcdNodes()); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("a failing probe must be an observation error carrying its stderr, got %v", err)
	}
	garbage := &ExecRoleObserver{Probe: "./bin/etcdrole", Topology: etcdTopology(),
		Exec: func(context.Context, []string, string, []string) ([]byte, error) {
			return []byte("leader: etcd-n1\n"), nil
		}}
	if _, err := garbage.ObserveRoles(context.Background(), etcdNodes()); err == nil {
		t.Fatal("unparseable output must be an error, not zero observations")
	}
	nameless := &ExecRoleObserver{Probe: "./bin/etcdrole", Topology: etcdTopology(),
		Exec: func(context.Context, []string, string, []string) ([]byte, error) {
			return []byte("{\"role\":\"leader\"}\n"), nil
		}}
	if _, err := nameless.ObserveRoles(context.Background(), etcdNodes()); err == nil {
		t.Fatal("a role record naming no node must be an error")
	}
}

// D-066. A project that declares harness.role_probe resolves `role:` targets
// through it; one that does not keeps the fixture-shaped /status reader.
// Mutation: making the observer unconditionally HTTPRoleObserver fails the
// first assertion.
func TestRunnerUsesTheConfiguredRoleProbe(t *testing.T) {
	cfg := &schema.Config{}
	cfg.Harness.RoleProbe = schema.SteadyStateProbe{Probe: "./bin/etcdrole", Timeout: schema.Duration(10 * time.Second)}
	r := &Runner{cfg: cfg}
	r.opts.ProjectDir = "C:/proj/etcd"
	obs, ok := r.roleObserver(etcdTopology()).(*ExecRoleObserver)
	if !ok {
		t.Fatalf("a declared role_probe must select ExecRoleObserver, got %T", r.roleObserver(etcdTopology()))
	}
	if obs.Probe != "./bin/etcdrole" || obs.Timeout != 10*time.Second || obs.ProjectDir != "C:/proj/etcd" || obs.Topology == nil {
		t.Fatalf("observer not wired from config: %+v", obs)
	}

	plain := &Runner{cfg: &schema.Config{}}
	if _, ok := plain.roleObserver(etcdTopology()).(*perturber.HTTPRoleObserver); !ok {
		t.Fatalf("without role_probe the /status reader must remain the default, got %T", plain.roleObserver(etcdTopology()))
	}
}
