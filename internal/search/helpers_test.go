package search

import (
	"testing"

	"github.com/WrdCstlg/pro-thesis/internal/perturber"
	"github.com/WrdCstlg/pro-thesis/internal/recorder"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// testConfig mirrors testdata/kvfixture/prothesis.yaml's perturber and driver
// blocks, including the safety constraint. Mirroring the real fixture matters:
// a generator tested only against a permissive config would never exercise the
// constraint-rejection path, which is precisely where "generate then validate"
// earns its keep.
func testConfig() *schema.Config {
	return &schema.Config{
		Version: schema.ConfigVersion,
		Name:    "kvfixture",
		Perturber: schema.PerturberConfig{
			Budget: schema.PerturberBudget{MaxConcurrentFaults: 3, MaxFaultsPerWorld: 24},
			Allow: []schema.FaultKind{
				schema.FaultNetPartition, schema.FaultNetLatency, schema.FaultNetLoss,
				schema.FaultProcKill, schema.FaultProcPause, schema.FaultIOLatency,
			},
			Deny:        []schema.FaultKind{schema.FaultIOFill, schema.FaultClockSkew},
			Constraints: []string{"never partition more than minority of kv"},
		},
		Driver: schema.DriverConfig{
			Cmd: "./bin/loadgen --history {history_path} --seed {seed} --profile {profile}",
			Profiles: map[string]schema.DriverProfile{
				"smoke":  {Clients: 4, Ops: 500, Mix: map[string]float64{"read": 0.5, "write": 0.5}},
				"gate":   {Clients: 16, Ops: 20000, Mix: map[string]float64{"read": 0.4, "write": 0.4, "txn": 0.2}},
				"linear": {Clients: 16, Ops: 60000, Mix: map[string]float64{"read": 0.5, "write": 0.5}},
			},
		},
	}
}

// testTopology is the fixture's three-node kv cluster.
func testTopology(t *testing.T) *perturber.Topology {
	t.Helper()
	top, err := perturber.NewTopologyFromNodes([]perturber.Node{
		{ID: "kv-n1", Service: "kv", ComposeService: "kv-n1", ContainerID: "c1", HostPort: 18081, ContainerPort: 8080},
		{ID: "kv-n2", Service: "kv", ComposeService: "kv-n2", ContainerID: "c2", HostPort: 18082, ContainerPort: 8080},
		{ID: "kv-n3", Service: "kv", ComposeService: "kv-n3", ContainerID: "c3", HostPort: 18083, ContainerPort: 8080},
	})
	if err != nil {
		t.Fatalf("build topology: %v", err)
	}
	return top
}

// testBaseWorld is the world template a search derives from.
func testBaseWorld() schema.World {
	w := schema.NewWorld(0, "default", "linear")
	w.SUT = schema.SUT{Images: []schema.SUTImage{
		{Service: "kv", Image: "kvfixture:buggy", Digest: "sha256:" + "ab"},
	}}
	return w
}

// testParams assembles a complete strategy Params against the fixture shape.
func testParams(t *testing.T, seed uint64) Params {
	t.Helper()
	cfg := testConfig()
	top := testTopology(t)
	sp, err := NewSpace(cfg, top, DefaultWindowPolicy())
	if err != nil {
		t.Fatalf("NewSpace: %v", err)
	}
	v, err := NewValidator(cfg, top, nil)
	if err != nil {
		t.Fatalf("NewValidator: %v", err)
	}
	return Params{
		Config:    cfg,
		Space:     sp,
		Validator: v,
		Corpus:    NewCorpus(DefaultEnergyParams()),
		Streams:   NewStreams(recorder.Seed(seed)),
		Base:      testBaseWorld(),
	}
}

// okFinding is a definite passing oracle result.
func okFinding(name string, class schema.OracleClass) Finding {
	return Finding{Oracle: name, Class: class, Severity: schema.SeverityMedium, Status: schema.StatusOK}
}

// inconclusiveFinding is the OQ-033 shape: an oracle that refused to answer.
func inconclusiveFinding(name string, class schema.OracleClass) Finding {
	return Finding{Oracle: name, Class: class, Severity: schema.SeverityMedium, Status: schema.StatusInconclusive}
}

// violatedFinding is a definite violation.
func violatedFinding(name string, class schema.OracleClass, sev schema.Severity) Finding {
	return Finding{Oracle: name, Class: class, Severity: sev, Status: schema.StatusViolated}
}

// coverageOf builds a per-world coverage set from explicit log lines, so a test
// controls the coverage delta exactly.
func coverageOf(lines ...string) *Coverage {
	c := NewCoverage()
	for _, ln := range lines {
		c.Templates.Add("kv-n1", ln)
	}
	return c
}
