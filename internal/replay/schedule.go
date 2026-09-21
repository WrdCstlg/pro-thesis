package replay

import (
	"fmt"
	"strings"

	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// ScheduleSource says which half of `fault_schedule` a replay executed.
type ScheduleSource string

const (
	// FromRealized: the world records what was actually injected, and that is
	// what ran. This is the case invariant I2 depends on.
	FromRealized ScheduleSource = "realized"
	// FromPlanned: the world has never been executed (`realized` is null), so
	// the plan is all there is.
	FromPlanned ScheduleSource = "planned"
)

// ScheduleChoice is the decision ChooseSchedule made, with everything a reader
// needs to judge how faithful the replay can be.
type ScheduleChoice struct {
	// Source names the half that was taken.
	Source ScheduleSource
	// Faults is the canonical schedule to execute.
	Faults []string
	// Unpinned names the faults whose target is still DYNAMIC (a role, a
	// quorum, or a multi-node wildcard) and will therefore re-resolve against
	// live cluster state on replay.
	//
	// It is not an error. The frozen TARGET grammar has no form for "these three
	// specific nodes", so a quorum resolution cannot be written back as a fault
	// string (see perturber's resolvedFaultString). It IS a fidelity warning,
	// and the one that explains a k/n below k/k.
	Unpinned []string
	// Notes are human-readable statements about what was chosen and why. They
	// are printed by `thesis replay` and carried into its report.
	Notes []string
}

// Pinned reports whether every fault addresses a concrete node or edge, i.e.
// whether this replay can address the same nodes the original did.
func (c ScheduleChoice) Pinned() bool { return len(c.Unpinned) == 0 }

// Summary renders the choice in one line.
func (c ScheduleChoice) Summary() string {
	s := fmt.Sprintf("%d fault(s) from fault_schedule.%s", len(c.Faults), c.Source)
	if n := len(c.Unpinned); n > 0 {
		s += fmt.Sprintf(", %d of them still dynamically targeted", n)
	}
	return s
}

// ErrScheduleUnusable reports a world whose recorded schedule cannot be
// executed.
type ErrScheduleUnusable struct {
	Field  string
	Value  string
	Reason string
}

func (e *ErrScheduleUnusable) Error() string {
	return fmt.Sprintf("replay: %s = %q cannot be executed: %s", e.Field, e.Value, e.Reason)
}

// ChooseSchedule decides what a replay of w should actually inject.
//
// It PREFERS THE REALIZED SCHEDULE whenever the world has one, which is the
// whole point (D-012). See the package doc for the three cases and why each is
// answered the way it is.
//
// The returned faults are canonical and sorted exactly as a world file's
// `planned` list is, so the replay's own world file hashes stably and a replay
// of a replay chooses the same schedule again.
func ChooseSchedule(w *schema.World) (ScheduleChoice, error) {
	if w == nil {
		return ScheduleChoice{}, fmt.Errorf("replay: ChooseSchedule(nil)")
	}

	// `realized: null`: never executed. The plan is all there is, and saying so
	// matters: a world assembled by hand or emitted by a search that has not run
	// it carries no evidence about which node a `role:` target bound to.
	if w.FaultSchedule.Realized == nil {
		c := ScheduleChoice{Source: FromPlanned}
		faults, unpinned, err := canonicalize("fault_schedule.planned", w.FaultSchedule.Planned)
		if err != nil {
			return ScheduleChoice{}, err
		}
		c.Faults, c.Unpinned = faults, unpinned
		c.Notes = append(c.Notes,
			"fault_schedule.realized is null: this world has never been executed, so the PLANNED "+
				"schedule is all there is. A dynamic target will bind afresh at injection time.")
		if len(unpinned) > 0 {
			c.Notes = append(c.Notes, unpinnedNote(unpinned))
		}
		return c, nil
	}

	c := ScheduleChoice{Source: FromRealized}

	// `realized: []` with a non-empty plan: the world executed and injected
	// nothing. Replaying the plan here would replay a DIFFERENT world than the
	// one whose verdict is on file, so the empty realized schedule wins and the
	// discrepancy is stated rather than repaired.
	if len(w.FaultSchedule.Realized) == 0 {
		c.Faults = []string{}
		if len(w.FaultSchedule.Planned) > 0 {
			c.Notes = append(c.Notes, fmt.Sprintf(
				"fault_schedule.realized is EMPTY while planned names %d fault(s): this world executed "+
					"and injected nothing. The replay injects nothing too — replaying the plan would "+
					"replay a world that never ran.", len(w.FaultSchedule.Planned)))
		} else {
			c.Notes = append(c.Notes, "this world has no faults; the replay exercises the workload alone.")
		}
		return c, nil
	}

	resolved := make([]string, 0, len(w.FaultSchedule.Realized))
	for i, rf := range w.FaultSchedule.Realized {
		if rf.Resolved == "" {
			return ScheduleChoice{}, &ErrScheduleUnusable{
				Field:  fmt.Sprintf("fault_schedule.realized[%d].resolved", i),
				Value:  "",
				Reason: "empty; the world file records no fault to inject for this entry",
			}
		}
		resolved = append(resolved, rf.Resolved)
	}

	faults, unpinned, err := canonicalize("fault_schedule.realized[].resolved", resolved)
	if err != nil {
		return ScheduleChoice{}, err
	}
	c.Faults, c.Unpinned = faults, unpinned

	c.Notes = append(c.Notes, fmt.Sprintf(
		"replaying fault_schedule.realized: %d fault(s) as they were ACTUALLY injected, against the "+
			"nodes the original run bound them to.", len(faults)))
	if rebound := reboundTargets(w.FaultSchedule.Realized); len(rebound) > 0 {
		c.Notes = append(c.Notes, fmt.Sprintf(
			"%d planned target(s) were dynamic and are replayed against the RECORDED node instead of "+
				"being resolved again: %s", len(rebound), strings.Join(rebound, ", ")))
	}
	if len(unpinned) > 0 {
		c.Notes = append(c.Notes, unpinnedNote(unpinned))
	}
	return c, nil
}

// canonicalize parses every fault string, re-renders it in canonical form and
// sorts the result the way a world file's planned list is sorted.
//
// Re-rendering is not cosmetic. The replay writes its own world file, and
// schema.World.Validate refuses a `planned` entry that is not canonical, so a
// schedule that went in as written and came out as canonical would produce a
// world the recorder refuses to store, at TEARDOWN, after the work was done.
func canonicalize(field string, in []string) (faults, unpinned []string, err error) {
	out := make([]string, 0, len(in))
	for i, s := range in {
		spec, perr := schema.ParseFault(s)
		if perr != nil {
			return nil, nil, &ErrScheduleUnusable{
				Field:  fmt.Sprintf("%s[%d]", field, i),
				Value:  s,
				Reason: perr.Error(),
			}
		}
		canon := spec.String()
		out = append(out, canon)
		if spec.Target.IsDynamic() {
			unpinned = append(unpinned, canon)
		}
	}
	sorted := schema.SortFaultStrings(out)
	if sorted == nil {
		sorted = []string{}
	}
	if unpinned != nil {
		unpinned = schema.SortFaultStrings(unpinned)
	}
	return sorted, unpinned, nil
}

// reboundTargets names the planned faults whose target was dynamic and whose
// realized record pinned it to a concrete node: the D-012 case, spelled out so
// a reader can see the substitution that just happened.
func reboundTargets(realized []schema.RealizedFault) []string {
	var out []string
	for _, rf := range realized {
		planned, err := schema.ParseFault(rf.Fault)
		if err != nil || !planned.Target.IsDynamic() {
			continue
		}
		res, err := schema.ParseFault(rf.Resolved)
		if err != nil || res.Target.IsDynamic() {
			continue
		}
		out = append(out, fmt.Sprintf("%s -> %s", planned.Target.String(), res.Target.String()))
	}
	return out
}

func unpinnedNote(unpinned []string) string {
	return fmt.Sprintf(
		"%d fault(s) still name a DYNAMIC target and will re-resolve against live cluster state on "+
			"replay: %s. The frozen TARGET grammar has no form for an arbitrary node set, so a quorum "+
			"or multi-node wildcard resolution cannot be written back as a fault string. Expect a "+
			"lower reproduction rate for these.",
		len(unpinned), strings.Join(unpinned, ", "))
}
