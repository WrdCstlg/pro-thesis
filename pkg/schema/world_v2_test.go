package schema

import (
	"strings"
	"testing"
)

// World identity v2 (D-109) hashes the world's INPUTS: seed, driver profile,
// planned schedule, topology variant, the images it ran on and its driver
// plan. A v1 identity also hashed measured phase timings and the realized
// schedule (live-resolved targets, measured windows), so re-executing the same
// world produced a different hash every time and a world could not be named by
// its content.

func v2World() World {
	w := NewWorld(424242, "kvfixed", "gate")
	w.FaultSchedule.Planned = []string{"net.partition(role:leader)@3000..7500"}
	w.SUT = SUT{Images: []SUTImage{{Service: "kv", Image: "kvfixture:fixed", Digest: "sha256:aa"}}}
	w.PlanDigest = "sha256:bb"
	return w.Normalized()
}

func TestWorldV2HashIgnoresWhatWasMeasured(t *testing.T) {
	a := v2World()
	a.PhaseTimings = PhaseTimings{{Phase: PhaseBoot, StartMS: -9000, EndMS: -4000}}
	a.FaultSchedule.Realized = []RealizedFault{{
		Fault: "net.partition(role:leader)@3000..7500", Resolved: "net.partition(kv-n1)@3000..7500",
		Nodes: []string{"kv-n1"}, StartMS: 3004, EndMS: 7511,
	}}
	b := v2World()
	b.PhaseTimings = PhaseTimings{{Phase: PhaseBoot, StartMS: -9400, EndMS: -4100}}
	b.FaultSchedule.Realized = []RealizedFault{{
		Fault: "net.partition(role:leader)@3000..7500", Resolved: "net.partition(kv-n3)@3000..7500",
		Nodes: []string{"kv-n3"}, StartMS: 3009, EndMS: 7502,
	}}
	ha, err := a.Hash()
	if err != nil {
		t.Fatal(err)
	}
	hb, err := b.Hash()
	if err != nil {
		t.Fatal(err)
	}
	if ha != hb {
		t.Fatalf("two executions of one world hash differently (%s vs %s): timings and the realized "+
			"schedule are measurements, not inputs", ha, hb)
	}
}

func TestWorldV2HashCoversEveryInput(t *testing.T) {
	base, err := func() (string, error) { w := v2World(); return w.Hash() }()
	if err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*World){
		"seed":           func(w *World) { w.Seed++ },
		"driver_profile": func(w *World) { w.DriverProfile = "smoke" },
		"planned":        func(w *World) { w.FaultSchedule.Planned = []string{"proc.pause(kv-n1)@3000..7500"} },
		"variant":        func(w *World) { w.TopologyVariant = "buggy" },
		"image":          func(w *World) { w.SUT.Images[0].Digest = "sha256:cc" },
		"plan":           func(w *World) { w.PlanDigest = "sha256:dd" },
	} {
		w := v2World()
		mutate(&w)
		h, err := w.Hash()
		if err != nil {
			t.Fatal(err)
		}
		if h == base {
			t.Errorf("changing %s did not change the world's identity", name)
		}
	}
}

// goldenWorldV2Bytes freezes the v2 encoding, as goldenWorldBytes freezes v1.
const goldenWorldV2Bytes = `{"driver_profile":"gate",` +
	`"fault_schedule":{"planned":["net.partition(role:leader)@3000..7500"],"realized":null},` +
	`"phase_timings":[],` +
	`"plan_digest":"sha256:bb",` +
	`"schema":"prothesis.world/v2",` +
	`"seed":424242,` +
	`"sut":{"images":[{"digest":"sha256:aa","image":"kvfixture:fixed","service":"kv"}]},` +
	`"topology_variant":"kvfixed"}` + "\n"

func TestWorldV2GoldenEncoding(t *testing.T) {
	w := v2World()
	if err := w.Validate(); err != nil {
		t.Fatalf("v2 golden world does not validate: %v", err)
	}
	got, err := MarshalWorld(&w)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != goldenWorldV2Bytes {
		t.Fatalf("v2 canonical encoding changed.\n got: %s\nwant: %s", got, goldenWorldV2Bytes)
	}
	back, err := UnmarshalWorld(got)
	if err != nil {
		t.Fatalf("UnmarshalWorld(v2): %v", err)
	}
	if back.Schema != WorldSchema || back.PlanDigest != "sha256:bb" {
		t.Fatalf("v2 did not round-trip: %+v", back)
	}
}

func TestNewWorldsAreV2AndOldWorldsStayV1(t *testing.T) {
	if w := NewWorld(1, "default", "smoke"); w.Schema != WorldSchema || WorldSchema != "prothesis.world/v2" {
		t.Fatalf("NewWorld schema = %q, current = %q, want prothesis.world/v2", w.Schema, WorldSchema)
	}
	old := goldenWorld()
	if old.Schema != WorldSchemaV1 {
		t.Fatalf("a v1 world was restamped %q by Normalized", old.Schema)
	}
}

func TestAV1WorldCannotCarryAPlanDigest(t *testing.T) {
	w := goldenWorld()
	w.PlanDigest = "sha256:bb"
	err := w.Validate()
	if err == nil || !strings.Contains(err.Error(), "plan_digest") {
		t.Fatalf("a v1 world with a plan_digest validated (err %v); v1 identity cannot see it", err)
	}
}
