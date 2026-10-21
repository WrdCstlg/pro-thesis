package driver

import (
	"encoding/json"
	"testing"
)

// A world's identity includes the workload its driver ran (D-109). The plan
// file also says where this execution writes its history, which addresses it
// reaches and when its clock started; those differ on every execution of the
// same world and must not move the digest. Everything the driver is asked to
// DO must.
func TestWorkloadDigestCoversTheWorkloadAndNothingElse(t *testing.T) {
	base := func() *Plan {
		return &Plan{
			Schema: PlanSchema, Profile: "linear", Seed: 7, HistoryPath: "/run/a/world-0001/history.jsonl",
			Clients: 4, Ops: 500, Mix: []MixEntry{{Op: "read", WeightPPM: 500000}, {Op: "write", WeightPPM: 500000}},
			Targets: []string{"127.0.0.1:18081"}, OriginNS: 1_700_000_000_000_000_000,
		}
	}
	want, err := base().WorkloadDigest()
	if err != nil {
		t.Fatal(err)
	}

	elsewhere := base()
	elsewhere.HistoryPath = "/run/b/world-0007/history.jsonl"
	elsewhere.Targets = []string{"127.0.0.1:19081"}
	elsewhere.OriginNS = 1_800_000_000_000_000_000
	if got, _ := elsewhere.WorkloadDigest(); got != want {
		t.Fatal("the same workload executed elsewhere and later has a different digest")
	}

	for name, mutate := range map[string]func(*Plan){
		"seed":    func(p *Plan) { p.Seed = 8 },
		"profile": func(p *Plan) { p.Profile = "smoke" },
		"clients": func(p *Plan) { p.Clients = 2 },
		"ops":     func(p *Plan) { p.Ops = 5 },
		"mix":     func(p *Plan) { p.Mix[0].WeightPPM = 900000 },
		"origin":  func(p *Plan) { p.Origin = OriginFirstOp },
		"operations": func(p *Plan) {
			p.Operations = []PlanOp{{OpID: 1, F: "read", Key: "k/0", Value: json.RawMessage("null")}}
		},
	} {
		p := base()
		mutate(p)
		got, err := p.WorkloadDigest()
		if err != nil {
			t.Fatal(err)
		}
		if got == want {
			t.Errorf("changing %s did not change the workload digest", name)
		}
	}
}
