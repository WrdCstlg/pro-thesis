package oracle

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// NoCrash is the `no_crash` built-in: no process exits outside a planned fault
// window.
//
// It is an INVARIANT oracle (schema.BuiltinOracle declares it valid in all
// eight phases) because a process that dies is a finding whenever it happens,
// and unlike a consistency claim it cannot be manufactured by an active
// partition.
//
// In Phase 1 the perturber injects nothing, so Input.PlannedFaults is empty and
// EVERY process exit is unexcused. That is not a temporary simplification: it
// is the Phase 1 definition of done, which requires that manually killing a
// container produce a no_crash violation.
type NoCrash struct {
	builtinDecl
	opts NoCrashOptions
}

// NewNoCrash builds the oracle.
func NewNoCrash(opts NoCrashOptions) *NoCrash {
	return &NoCrash{builtinDecl: builtinDecl{id: schema.BuiltinNoCrash}, opts: opts.withDefaults()}
}

// crashFinding is one node's problem.
type crashFinding struct {
	node   string
	detail string
	atMS   int64
	atOK   bool
}

// Evaluate implements Oracle.
func (o *NoCrash) Evaluate(_ context.Context, phase schema.Phase, in *Input) (Result, error) {
	if in == nil || len(in.Nodes) == 0 {
		return Inconclusive("no node state was collected, so no process could be checked for an " +
			"unplanned exit; a run whose processes were never inspected has not been shown to be crash-free"), nil
	}

	var (
		crashes   []crashFinding
		unchecked []string
		unknown   []string
	)

	for _, n := range in.Nodes {
		if !n.StateObserved {
			unchecked = append(unchecked, n.NodeID)
			continue
		}
		planned := in.PlannedFaultsFor(n.NodeID)

		if n.Exit != nil {
			at, atOK := n.Exit.ExitedAt()
			excused := ""
			if atOK {
				for _, w := range planned {
					if w.Covers(n.NodeID, at, o.opts.FaultWindowGraceMS) {
						excused = w.String()
						break
					}
				}
			}
			if excused == "" {
				when := "at an unrecorded time"
				if atOK {
					when = fmt.Sprintf("at t+%dms", at)
				}
				crashes = append(crashes, crashFinding{
					node:   n.NodeID,
					detail: fmt.Sprintf("%s %s (%s)", n.NodeID, when, n.Exit.Describe()),
					atMS:   at,
					atOK:   atOK,
				})
				continue
			}
		}

		if n.RestartCount > 0 {
			if len(planned) > 0 {
				// The process died and came back, and a fault was scheduled
				// against this node, but a restart count carries no per-restart
				// timestamps, so nothing here can show the death fell inside the
				// window. Saying "planned" would excuse a real crash; saying
				// "crash" would report the perturber's own proc.restart. Neither
				// is knowable from this evidence, which is what inconclusive is
				// for.
				unknown = append(unknown, fmt.Sprintf("%s restarted %d time(s) while %d fault(s) were "+
					"scheduled against it, and restart times were not recorded",
					n.NodeID, n.RestartCount, len(planned)))
				continue
			}
			crashes = append(crashes, crashFinding{
				node:   n.NodeID,
				detail: fmt.Sprintf("%s restarted %d time(s) with no fault scheduled against it", n.NodeID, n.RestartCount),
			})
			continue
		}

		if n.Exit == nil && !n.Running {
			crashes = append(crashes, crashFinding{
				node: n.NodeID,
				detail: fmt.Sprintf("%s is not running and no exit record was collected, so it stopped "+
					"for a reason nothing observed", n.NodeID),
			})
		}
	}

	if len(crashes) > 0 {
		return o.violation(in, phase, crashes), nil
	}
	if len(unchecked) > 0 {
		return Inconclusive("node state was not collected for %s, so those processes were never checked "+
			"for an unplanned exit", strings.Join(sortedStrings(unchecked), ", ")), nil
	}
	if len(unknown) > 0 {
		return Inconclusive("%s", strings.Join(sortedStrings(unknown), "; ")), nil
	}

	return OK("all %d node process(es) ran to the end of the world without an unplanned exit", len(in.Nodes)), nil
}

func (o *NoCrash) violation(in *Input, phase schema.Phase, crashes []crashFinding) Result {
	sort.Slice(crashes, func(i, j int) bool {
		switch {
		case crashes[i].atOK != crashes[j].atOK:
			return crashes[i].atOK // timed findings first, so first_seen_ms is real
		case crashes[i].atOK && crashes[i].atMS != crashes[j].atMS:
			return crashes[i].atMS < crashes[j].atMS
		default:
			return crashes[i].node < crashes[j].node
		}
	})

	firstSeen := int64(0)
	timed := false
	for _, c := range crashes {
		if c.atOK {
			firstSeen, timed = c.atMS, true
			break
		}
	}

	nodes := make([]string, 0, len(crashes))
	details := make([]string, 0, len(crashes))
	for _, c := range crashes {
		nodes = append(nodes, c.node)
		details = append(details, c.detail)
	}

	w := NewWitness().
		Set("nodes", nodes).
		Set("planned_fault_windows", plannedWindowStrings(in)).
		Text("detail", strings.Join(details, "; "))
	for _, c := range crashes {
		if n, ok := in.Node(c.node); ok && n.Exit != nil {
			w.Set("exit_"+c.node, exitWitness(n))
		}
	}

	plural := "process"
	if len(crashes) > 1 {
		plural = "processes"
	}
	res := Violated(firstSeen, w.Build(),
		"%d %s exited outside any planned fault window: %s",
		len(crashes), plural, strings.Join(details, "; "))
	if !timed {
		// Nothing recorded when any of these processes died, so the finding has
		// no position on the timeline. Report the phase the check ran in rather
		// than let first_seen_ms 0 place it at DRIVE start.
		res = res.inPhase(phase)
	}
	return res
}

// exitWitness renders one node's exit for the witness.
func exitWitness(n NodeObservation) map[string]any {
	m := map[string]any{
		"exit_code":     n.Exit.Code,
		"oom_killed":    n.Exit.OOMKilled,
		"restart_count": n.RestartCount,
		"running":       n.Running,
	}
	if n.Exit.Signal != "" {
		m["signal"] = n.Exit.Signal
	}
	if n.Exit.Error != "" {
		m["error"] = n.Exit.Error
	}
	if at, ok := n.Exit.ExitedAt(); ok {
		m["exited_at_ms"] = at
	}
	return m
}

// plannedWindowStrings renders the planned schedule for the witness, so a
// reader can see what the oracle was willing to excuse.
func plannedWindowStrings(in *Input) []string {
	out := make([]string, 0, len(in.PlannedFaults))
	for _, f := range in.PlannedFaults {
		out = append(out, f.String())
	}
	return out
}
