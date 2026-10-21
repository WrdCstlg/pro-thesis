package corpus

import (
	"os"
	"strings"
	"testing"

	"github.com/WrdCstlg/pro-thesis/internal/driver"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// A shrink reduces the workload as well as the schedule (op-level ddmin), and a
// v2 world names its workload by digest. A corpus entry without the plan it was
// confirmed with could not be replayed as itself, so the plan is committed
// beside it, and a plan that is not the world's workload is refused.

func shrunkPlan() *driver.Plan {
	return &driver.Plan{Schema: driver.PlanSchema, Profile: "linear", Seed: 7,
		HistoryPath: `C:\Users\someone\run\world-0003\history.jsonl`, Targets: []string{"127.0.0.1:18081"},
		Operations: []driver.PlanOp{{OpID: 4, Process: 0, F: "write", Key: "k/0"}}}
}

func confirmedWorld(t *testing.T, plan *driver.Plan) *schema.World {
	t.Helper()
	w := schema.NewWorld(7, "default", "linear")
	w.FaultSchedule.Planned = []string{"net.partition(kv-n1)@3000..7500"}
	w.FaultSchedule.Realized = []schema.RealizedFault{}
	if plan != nil {
		d, err := plan.WorkloadDigest()
		if err != nil {
			t.Fatal(err)
		}
		w.PlanDigest = d
	}
	w = w.Normalized()
	return &w
}

func planBytes(t *testing.T, p *driver.Plan) []byte {
	t.Helper()
	b, err := p.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestTheShrunkPlanIsCommittedBesideItsWorld(t *testing.T) {
	c := OpenDir(t.TempDir())
	plan := shrunkPlan()
	path, err := c.AddWithPlan(confirmedWorld(t, plan), confirmed(3), planBytes(t, plan))
	if err != nil {
		t.Fatalf("AddWithPlan: %v", err)
	}
	pp := strings.TrimSuffix(path, schema.WorldFileExt) + PlanFileSuffix
	raw, err := os.ReadFile(pp)
	if err != nil {
		t.Fatalf("no plan beside the entry: %v", err)
	}
	if strings.Contains(string(raw), `someone`) || strings.Contains(string(raw), "18081") {
		t.Fatalf("the committed plan carries the shrink's local paths or addresses:\n%s", raw)
	}
	got, err := driver.ParsePlan(raw)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := plan.WorkloadDigest()
	if d, _ := got.WorkloadDigest(); d != want {
		t.Fatalf("the committed plan drives %s, the world names %s", d, want)
	}
	// The plan file is not mistaken for a world.
	entries, err := c.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || !entries[0].OK() {
		t.Fatalf("List = %+v, want the one world and no corrupt entry for its plan", entries)
	}
}

func TestAWorldThatNamesItsWorkloadNeedsItsPlan(t *testing.T) {
	c := OpenDir(t.TempDir())
	if _, err := c.Add(confirmedWorld(t, shrunkPlan()), confirmed(3)); err == nil {
		t.Fatal("a world with a plan_digest was committed without its plan")
	}
	other := shrunkPlan()
	other.Operations[0].Key = "k/1" // a valid plan, a different workload
	if _, err := c.AddWithPlan(confirmedWorld(t, shrunkPlan()), confirmed(3), planBytes(t, other)); err == nil ||
		!strings.Contains(err.Error(), "plan_digest") {
		t.Fatalf("a plan that is not the world's workload was accepted (err %v)", err)
	}
	// A world that names no workload is committed as before.
	if _, err := c.Add(confirmedWorld(t, nil), confirmed(3)); err != nil {
		t.Fatalf("Add of a world without a plan_digest: %v", err)
	}
}
