package replay

import (
	"strings"
	"testing"

	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// worldWith builds a normalized world with the given planned and realized
// schedule. Realized nil means "never executed", which is a DIFFERENT statement
// from an empty list and the tests below depend on the difference.
func worldWith(planned []string, realized []schema.RealizedFault) *schema.World {
	w := schema.NewWorld(4242, "default", "linear")
	w.FaultSchedule.Planned = planned
	w.FaultSchedule.Realized = realized
	n := w.Normalized()
	return &n
}

// THE headline rule of this phase's replay half (D-012).
//
// A planned `role:leader` binds to whichever node holds the role AT INJECTION
// TIME. Raft leadership moves, so re-resolving it on replay can address a
// different node and quietly test a different world. The realized record names
// the concrete node, and that is what must be executed.
func TestAPlannedRoleTargetReplaysAgainstTheRecordedNode(t *testing.T) {
	w := worldWith(
		[]string{"proc.pause(role:leader)@8300..11000"},
		[]schema.RealizedFault{{
			Fault:    "proc.pause(role:leader)@8300..11000",
			Resolved: "proc.pause(kv-n2)@8300..11000",
			Nodes:    []string{"kv-n2"},
			StartMS:  8302,
			EndMS:    11004,
		}},
	)

	c, err := ChooseSchedule(w)
	if err != nil {
		t.Fatalf("ChooseSchedule: %v", err)
	}
	if c.Source != FromRealized {
		t.Fatalf("source = %q, want %q", c.Source, FromRealized)
	}
	if len(c.Faults) != 1 {
		t.Fatalf("faults = %v, want exactly one", c.Faults)
	}
	if got := c.Faults[0]; got != "proc.pause(kv-n2)@8300..11000" {
		t.Fatalf("replayed fault is %q, want the RECORDED node kv-n2.\n"+
			"Replaying the planned role:leader would re-resolve against live cluster state and "+
			"can address a different node (D-012).", got)
	}
	for _, f := range c.Faults {
		if strings.Contains(f, "role:") {
			t.Fatalf("replayed schedule still contains a dynamic role target: %q", f)
		}
	}
	if !c.Pinned() {
		t.Fatalf("schedule reports unpinned faults %v, but every target is a concrete node", c.Unpinned)
	}

	// The substitution must be VISIBLE. A reader who is told only "1 fault from
	// realized" cannot tell that a role target was rebound.
	var sawRebind bool
	for _, n := range c.Notes {
		if strings.Contains(n, "role:leader") && strings.Contains(n, "kv-n2") {
			sawRebind = true
		}
	}
	if !sawRebind {
		t.Fatalf("notes do not record the role:leader -> kv-n2 rebinding: %v", c.Notes)
	}
}

// The mutation guard for the test above: if ChooseSchedule ever preferred the
// planned half, this is what it would look like, and the test above would catch
// it. This one pins the OTHER direction: that the planned half is used when,
// and only when, there is no realized half.
func TestANeverExecutedWorldFallsBackToPlannedAndSaysSo(t *testing.T) {
	w := worldWith([]string{"proc.pause(role:leader)@8300..11000"}, nil)

	c, err := ChooseSchedule(w)
	if err != nil {
		t.Fatalf("ChooseSchedule: %v", err)
	}
	if c.Source != FromPlanned {
		t.Fatalf("source = %q, want %q", c.Source, FromPlanned)
	}
	if len(c.Faults) != 1 || c.Faults[0] != "proc.pause(role:leader)@8300..11000" {
		t.Fatalf("faults = %v, want the planned schedule verbatim", c.Faults)
	}
	if c.Pinned() {
		t.Fatalf("a role: target is dynamic and must be reported as unpinned; Unpinned = %v", c.Unpinned)
	}
	if !strings.Contains(strings.Join(c.Notes, " "), "never been executed") {
		t.Fatalf("notes must say the world has never been executed, so a reader knows the fidelity "+
			"claim is weaker; got %v", c.Notes)
	}
}

// `realized: []` is EXECUTED-AND-INJECTED-NOTHING, which is not the same as
// never executed. Replaying the plan here would replay a world that never ran.
func TestAnExecutedWorldThatInjectedNothingReplaysNothing(t *testing.T) {
	w := worldWith(
		[]string{"net.partition(kv-n1)@3000..5000"},
		[]schema.RealizedFault{},
	)

	c, err := ChooseSchedule(w)
	if err != nil {
		t.Fatalf("ChooseSchedule: %v", err)
	}
	if c.Source != FromRealized {
		t.Fatalf("source = %q, want %q", c.Source, FromRealized)
	}
	if len(c.Faults) != 0 {
		t.Fatalf("faults = %v, want none: the world executed and injected nothing", c.Faults)
	}
	joined := strings.Join(c.Notes, " ")
	if !strings.Contains(joined, "EMPTY") || !strings.Contains(joined, "injected nothing") {
		t.Fatalf("the discrepancy between a 1-fault plan and an empty realized schedule must be "+
			"stated loudly; notes = %v", c.Notes)
	}
}

// A quorum or multi-node wildcard cannot be written back as a fault string (the
// frozen TARGET grammar has no form for an arbitrary node set) so the realized
// record keeps the planned target and only Nodes carries the binding. That is
// not an error, but it IS the honest explanation for a k/n below k/k, and it
// must be reported rather than silently replayed as if it were pinned.
func TestAnUnpinnableRealizedTargetIsReportedAsUnpinned(t *testing.T) {
	w := worldWith(
		[]string{"net.partition(minority(kv))@8400..10900"},
		[]schema.RealizedFault{{
			Fault:    "net.partition(minority(kv))@8400..10900",
			Resolved: "net.partition(minority(kv))@8400..10900",
			Nodes:    []string{"kv-n1"},
			StartMS:  8402,
			EndMS:    10903,
		}},
	)

	c, err := ChooseSchedule(w)
	if err != nil {
		t.Fatalf("ChooseSchedule: %v", err)
	}
	if c.Pinned() {
		t.Fatalf("minority(kv) is a dynamic target and must be reported as unpinned")
	}
	if len(c.Unpinned) != 1 || c.Unpinned[0] != "net.partition(minority(kv))@8400..10900" {
		t.Fatalf("Unpinned = %v, want the quorum fault", c.Unpinned)
	}
	if !strings.Contains(strings.Join(c.Notes, " "), "re-resolve") {
		t.Fatalf("the note must say the target will re-resolve on replay; got %v", c.Notes)
	}
}

// The replay writes its own world file, and schema.World.Validate refuses a
// non-canonical `planned` entry. A schedule that went in as written and came out
// as canonical would produce a world the recorder refuses to store: at
// TEARDOWN, after the work was done.
func TestTheChosenScheduleIsCanonicalAndSorted(t *testing.T) {
	w := worldWith(nil, []schema.RealizedFault{
		{Fault: "proc.pause(kv-n3)@9000..9500", Resolved: "proc.pause( kv-n3 )@9000..9500",
			Nodes: []string{"kv-n3"}, StartMS: 9001, EndMS: 9502},
		{Fault: "net.partition(kv-n1)@3000..5000", Resolved: "net.partition(kv-n1)@3000..5000",
			Nodes: []string{"kv-n1"}, StartMS: 3001, EndMS: 5002},
	})

	c, err := ChooseSchedule(w)
	if err != nil {
		t.Fatalf("ChooseSchedule: %v", err)
	}
	want := []string{"net.partition(kv-n1)@3000..5000", "proc.pause(kv-n3)@9000..9500"}
	if len(c.Faults) != len(want) {
		t.Fatalf("faults = %v, want %v", c.Faults, want)
	}
	for i := range want {
		if c.Faults[i] != want[i] {
			t.Fatalf("faults[%d] = %q, want %q (sorted by window, canonical form)", i, c.Faults[i], want[i])
		}
	}

	// And prove the point: a world built from this schedule must validate.
	nw := schema.NewWorld(1, "default", "linear")
	nw.FaultSchedule.Planned = c.Faults
	nw.FaultSchedule.Realized = []schema.RealizedFault{}
	nn := nw.Normalized()
	if err := nn.Validate(); err != nil {
		t.Fatalf("a world carrying the chosen schedule does not validate: %v", err)
	}
}

// A realized entry whose `resolved` will not parse is refused BEFORE anything
// boots, naming the field. Discovering it at PERTURB would cost a boot and a
// bridge network to learn what a parse answers.
func TestAnUnparseableRealizedFaultIsRefusedUpFront(t *testing.T) {
	w := &schema.World{
		Schema:          schema.WorldSchema,
		Seed:            1,
		TopologyVariant: "default",
		DriverProfile:   "linear",
		FaultSchedule: schema.FaultSchedule{
			Planned: []string{},
			Realized: []schema.RealizedFault{{
				Fault: "proc.pause(kv-n1)@1..2", Resolved: "this is not a fault",
				Nodes: []string{"kv-n1"},
			}},
		},
		PhaseTimings: schema.PhaseTimings{},
		SUT:          schema.SUT{Images: []schema.SUTImage{}},
	}
	if _, err := ChooseSchedule(w); err == nil {
		t.Fatal("ChooseSchedule accepted an unparseable realized fault")
	}
}
