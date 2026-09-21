package oracle

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// AvailabilityAfterHeal is the `availability_after_heal` built-in: every logical
// node answers health probes successfully after faults have been withdrawn.
//
// Invariant I5 declares this oracle valid only in QUIESCE and ASSERT. During
// DRIVE and PERTURB, an unavailable node is the intended consequence of fault
// injection; checking availability while a partition or SIGSTOP is active is a
// false-positive factory.
//
// Post-HEAL convergence window:
//   - If QUIESCE is present in the run, QUIESCE is the convergence grace window
//     by definition (directive 4.1 step 6), so [quiesce.StartMS, quiesce.EndMS] is used.
//   - If QUIESCE is absent, [heal.EndMS, heal.EndMS + opts.ConvergenceWindow.Milliseconds()]
//     is used.
//
// An oracle whose required evidence is absent returns INCONCLUSIVE:
//   - No node state collected
//   - No HEAL and no QUIESCE window
//   - No probe attempted in the convergence window
//   - Probes not attempted for all nodes
type AvailabilityAfterHeal struct {
	builtinDecl
	opts AvailabilityOptions
}

// NewAvailabilityAfterHeal builds the oracle.
func NewAvailabilityAfterHeal(opts AvailabilityOptions) *AvailabilityAfterHeal {
	return &AvailabilityAfterHeal{
		builtinDecl: builtinDecl{id: schema.BuiltinAvailabilityAfterHeal},
		opts:        opts.withDefaults(),
	}
}

// Evaluate implements Oracle.
func (o *AvailabilityAfterHeal) Evaluate(_ context.Context, phase schema.Phase, in *Input) (Result, error) {
	if in == nil || len(in.Nodes) == 0 {
		return Inconclusive("no node state was collected, so no availability could be evaluated"), nil
	}

	var winStart, winEnd int64
	quiesce, hasQuiesce := in.Window(schema.PhaseQuiesce)
	heal, hasHeal := in.Window(schema.PhaseHeal)

	switch {
	case hasQuiesce && quiesce.DurationMS() > 0:
		winStart = quiesce.StartMS
		winEnd = quiesce.EndMS
	case hasHeal:
		winStart = heal.EndMS
		winEnd = heal.EndMS + o.opts.ConvergenceWindow.Milliseconds()
	case hasQuiesce:
		winStart = quiesce.StartMS
		winEnd = quiesce.StartMS + o.opts.ConvergenceWindow.Milliseconds()
	default:
		return Inconclusive("the run recorded neither a HEAL nor a QUIESCE window, so there is no post-fault " +
			"interval in which availability could be evaluated"), nil
	}

	if winEnd <= winStart {
		return Inconclusive("the post-HEAL convergence window (%dms..%dms) has zero or negative duration",
			winStart, winEnd), nil
	}

	// Filter probe observations falling in [winStart, winEnd].
	type nodeProbeState struct {
		probed     bool
		successful bool
		attempts   []ProbeObservation
	}

	byNode := make(map[string]*nodeProbeState, len(in.Nodes))
	for _, n := range in.Nodes {
		byNode[n.NodeID] = &nodeProbeState{}
	}

	// A node harness.health declares no probe for has no definition of
	// "available": nobody wrote one. It is NOT JUDGED (neither unprobed
	// (which would make the world INCONCLUSIVE over a config omission) nor
	// unavailable) and the OK text names it so the verdict does not read as
	// covering it. A nil HealthProbed is a caller that did not record the
	// declaration, and every node is then judged as before (D-066).
	var declared map[string]bool
	if in.HealthProbed != nil {
		declared = make(map[string]bool, len(in.HealthProbed))
		for _, id := range in.HealthProbed {
			declared[id] = true
		}
		judged := 0
		for _, n := range in.Nodes {
			if declared[n.NodeID] {
				judged++
			}
		}
		if judged == 0 {
			return Inconclusive("harness.health declares a probe for none of the %d node(s), so availability "+
				"after HEAL has no definition to be judged against", len(in.Nodes)), nil
		}
	}

	var windowProbes []ProbeObservation
	for _, p := range in.Probes {
		if p.TMS >= winStart && p.TMS <= winEnd {
			windowProbes = append(windowProbes, p)
			if np, ok := byNode[p.NodeID]; ok {
				np.probed = true
				np.attempts = append(np.attempts, p)
				if p.OK {
					np.successful = true
				}
			}
		}
	}

	if len(windowProbes) == 0 {
		return Inconclusive("no probe was attempted in the convergence window (t+%dms..t+%dms)",
			winStart, winEnd), nil
	}

	var (
		unavailable []string
		unprobed    []string
		notJudged   []string
		details     []string
		firstFailed int64
		hasFailed   bool
	)

	for _, n := range in.Nodes {
		if declared != nil && !declared[n.NodeID] {
			notJudged = append(notJudged, n.NodeID)
			continue
		}
		np := byNode[n.NodeID]
		if !np.probed {
			unprobed = append(unprobed, n.NodeID)
			continue
		}
		if !np.successful {
			unavailable = append(unavailable, n.NodeID)
			lastErr := "unknown error"
			for _, att := range np.attempts {
				if !att.OK {
					if !hasFailed || att.TMS < firstFailed {
						firstFailed = att.TMS
						hasFailed = true
					}
					if att.Err != "" {
						lastErr = att.Err
					}
				}
			}
			details = append(details, fmt.Sprintf("%s failed all %d probe(s) in window (%s)",
				n.NodeID, len(np.attempts), lastErr))
		}
	}

	if len(unavailable) > 0 {
		sort.Strings(unavailable)
		sort.Strings(details)
		if !hasFailed {
			firstFailed = winStart
		}

		w := NewWitness().
			Set("failed_nodes", unavailable).
			Set("convergence_window_ms", []int64{winStart, winEnd}).
			Text("detail", strings.Join(details, "; "))

		for _, nodeID := range unavailable {
			np := byNode[nodeID]
			probeWitness := make([]map[string]any, 0, len(np.attempts))
			for _, att := range np.attempts {
				m := map[string]any{
					"t_ms":       att.TMS,
					"target":     att.Target,
					"ok":         att.OK,
					"latency_ms": att.LatencyMS,
				}
				if att.Err != "" {
					m["err"] = att.Err
				}
				probeWitness = append(probeWitness, m)
			}
			w.Set("probes_"+nodeID, probeWitness)
		}

		plural := "node"
		if len(unavailable) > 1 {
			plural = "nodes"
		}
		return Violated(firstFailed, w.Build(),
			"%d %s failed to become available after HEAL within the %dms convergence window: %s",
			len(unavailable), plural, winEnd-winStart, strings.Join(details, "; ")), nil
	}

	if len(unprobed) > 0 {
		sort.Strings(unprobed)
		return Inconclusive("%d node(s) were never probed during the post-HEAL convergence window: %s",
			len(unprobed), strings.Join(unprobed, ", ")), nil
	}

	judged := len(in.Nodes) - len(notJudged)
	if len(notJudged) > 0 {
		sort.Strings(notJudged)
		return OK("all %d node(s) with a declared health probe answered it after HEAL (within %dms convergence "+
			"window); not judged, no health entry declared: %s",
			judged, winEnd-winStart, strings.Join(notJudged, ", ")), nil
	}
	return OK("all %d node(s) answered health probes successfully after HEAL (within %dms convergence window)",
		judged, winEnd-winStart), nil
}
