package oracle

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// NoStuckOp is the `no_stuck_op` built-in: no operation remains outstanding past
// HEAL + timeout (exceeding SLO ceiling).
//
// schema.BuiltinOracle declares it valid in HEAL, QUIESCE, and ASSERT.
//
// Inconclusive paths:
//   - No history collected, or history is unreliable (malformed / truncated)
//   - Empty history, or no operations invoked
//   - No DRIVE origin to convert nanosecond timestamps to the phase timeline
//   - Invokes cannot be paired (missing both op_id and process)
//   - In-flight operations when observation ended before SLO window elapsed
type NoStuckOp struct {
	builtinDecl
	opts NoStuckOpOptions
}

// NewNoStuckOp builds the oracle.
func NewNoStuckOp(opts NoStuckOpOptions) *NoStuckOp {
	return &NoStuckOp{
		builtinDecl: builtinDecl{id: schema.BuiltinNoStuckOp},
		opts:        opts.withDefaults(),
	}
}

type activeOp struct {
	key         string
	opID        *int64
	proc        *int64
	f           string
	invokeMS    int64
	invokeEntry schema.HistoryEntry
}

type completedOp struct {
	activeOp
	compMS    int64
	compEntry schema.HistoryEntry
}

type stuckFinding struct {
	opID       string
	f          string
	invokeMS   int64
	compMS     int64 // -1 if never completed
	durationMS int64
	reason     string
}

// Evaluate implements Oracle.
func (o *NoStuckOp) Evaluate(_ context.Context, phase schema.Phase, in *Input) (Result, error) {
	if in == nil || in.History == nil {
		return Inconclusive("no history was collected, so operations could not be checked for stuck state"), nil
	}
	if !in.History.Reliable() {
		return Inconclusive(in.History.Unreliable()), nil
	}
	if len(in.History.Entries) == 0 {
		return Inconclusive("the history is empty, so no operations could be evaluated"), nil
	}
	if in.DriveOriginWallNS == 0 {
		return Inconclusive("no DRIVE origin was recorded, so history timestamps cannot be placed on the phase timeline"), nil
	}

	heal, hasHeal := in.Window(schema.PhaseHeal)
	if !hasHeal {
		quiesce, hasQuiesce := in.Window(schema.PhaseQuiesce)
		if !hasQuiesce {
			return Inconclusive("the run recorded no HEAL or QUIESCE window, so post-fault completion cannot be evaluated"), nil
		}
		heal = schema.PhaseWindow{Phase: schema.PhaseHeal, StartMS: quiesce.StartMS, EndMS: quiesce.StartMS}
	}
	healEndMS := heal.EndMS
	sloCeilingMS := o.opts.SLOCeiling.Milliseconds()

	obsEndMS, hasObs := in.ObservationEndMS()
	if !hasObs {
		obsEndMS = healEndMS
	}

	active := make(map[string]activeOp)
	var completed []completedOp
	totalInvokes := 0

	for _, e := range in.History.Entries {
		if e.RecordKind() != schema.RecordOperation {
			continue
		}
		tMS, conv := in.MSFromEpochNS(e.TNS)
		if !conv {
			return Inconclusive("failed to convert timestamp for history record: no DRIVE origin"), nil
		}

		key, ok := opKey(e)
		if !ok {
			return Inconclusive("history contains operations with neither op_id nor process, so invokes cannot be paired"), nil
		}

		switch e.Type {
		case schema.HistoryInvoke:
			active[key] = activeOp{
				key:         key,
				opID:        e.OpID,
				proc:        e.Process,
				f:           e.F,
				invokeMS:    tMS,
				invokeEntry: e,
			}
			totalInvokes++

		case schema.HistoryOK, schema.HistoryFail, schema.HistoryInfo:
			inv, found := active[key]
			if !found {
				return Inconclusive("history contains completion for %s with no prior invoke (at t+%dms)", key, tMS), nil
			}
			delete(active, key)
			completed = append(completed, completedOp{
				activeOp:  inv,
				compMS:    tMS,
				compEntry: e,
			})
		}
	}

	if totalInvokes == 0 {
		return Inconclusive("no operations were invoked in the history to evaluate"), nil
	}

	var stuck []stuckFinding

	// 1. Check uncompleted operations
	for _, inv := range active {
		stuckThreshold := healEndMS + sloCeilingMS
		if inv.invokeMS+sloCeilingMS > stuckThreshold {
			stuckThreshold = inv.invokeMS + sloCeilingMS
		}
		if obsEndMS >= stuckThreshold {
			dur := obsEndMS - inv.invokeMS
			stuck = append(stuck, stuckFinding{
				opID:       formatOpID(inv.opID, inv.proc),
				f:          inv.f,
				invokeMS:   inv.invokeMS,
				compMS:     -1,
				durationMS: dur,
				reason: fmt.Sprintf("uncompleted after %dms (still open at t+%dms, exceeding %dms SLO ceiling past HEAL)",
					dur, obsEndMS, sloCeilingMS),
			})
		}
	}

	// 2. Check completed operations that exceeded SLO after HEAL
	for _, comp := range completed {
		if comp.compMS > healEndMS {
			durationAfterHeal := comp.compMS - healEndMS
			if comp.invokeMS > healEndMS {
				durationAfterHeal = comp.compMS - comp.invokeMS
			}
			totalDuration := comp.compMS - comp.invokeMS
			if durationAfterHeal > sloCeilingMS || (comp.compMS > healEndMS+sloCeilingMS && totalDuration > sloCeilingMS) {
				stuck = append(stuck, stuckFinding{
					opID:       formatOpID(comp.opID, comp.proc),
					f:          comp.f,
					invokeMS:   comp.invokeMS,
					compMS:     comp.compMS,
					durationMS: totalDuration,
					reason: fmt.Sprintf("took %dms to complete at t+%dms (%dms after HEAL, exceeding %dms SLO ceiling)",
						totalDuration, comp.compMS, durationAfterHeal, sloCeilingMS),
				})
			}
		}
	}

	if len(stuck) > 0 {
		sort.Slice(stuck, func(i, j int) bool {
			if stuck[i].invokeMS != stuck[j].invokeMS {
				return stuck[i].invokeMS < stuck[j].invokeMS
			}
			return stuck[i].opID < stuck[j].opID
		})

		firstSeen := stuck[0].invokeMS + sloCeilingMS
		if firstSeen < healEndMS+sloCeilingMS {
			firstSeen = healEndMS + sloCeilingMS
		}

		details := make([]string, 0, len(stuck))
		witnessOps := make([]map[string]any, 0, len(stuck))
		for i, s := range stuck {
			details = append(details, fmt.Sprintf("%s(%s): %s", s.opID, s.f, s.reason))
			if i < MaxWitnessSamples {
				m := map[string]any{
					"op_id":       s.opID,
					"f":           s.f,
					"invoke_ms":   s.invokeMS,
					"duration_ms": s.durationMS,
					"reason":      s.reason,
				}
				if s.compMS >= 0 {
					m["completed_ms"] = s.compMS
				}
				witnessOps = append(witnessOps, m)
			}
		}

		w := NewWitness().
			Set("stuck_count", len(stuck)).
			Set("slo_ceiling_ms", sloCeilingMS).
			Set("heal_end_ms", healEndMS).
			Set("stuck_ops", witnessOps).
			Text("detail", strings.Join(details, "; "))

		plural := "operation"
		if len(stuck) > 1 {
			plural = "operations"
		}
		return Violated(firstSeen, w.Build(),
			"%d %s remained stuck past HEAL + SLO ceiling (%dms): %s",
			len(stuck), plural, sloCeilingMS, strings.Join(details, "; ")), nil
	}

	if len(active) > 0 {
		return Inconclusive("%d operation(s) were still in flight when observation ended at t+%dms before "+
			"the %dms SLO ceiling elapsed", len(active), obsEndMS, sloCeilingMS), nil
	}

	return OK("all %d operation(s) completed within SLO ceiling (%dms) after HEAL",
		len(completed), sloCeilingMS), nil
}

func opKey(e schema.HistoryEntry) (string, bool) {
	if e.OpID != nil {
		return fmt.Sprintf("op:%d", *e.OpID), true
	}
	if e.Process != nil {
		return fmt.Sprintf("proc:%d", *e.Process), true
	}
	return "", false
}

func formatOpID(opID *int64, proc *int64) string {
	switch {
	case opID != nil && proc != nil:
		return fmt.Sprintf("op#%d(proc%d)", *opID, *proc)
	case opID != nil:
		return fmt.Sprintf("op#%d", *opID)
	case proc != nil:
		return fmt.Sprintf("proc#%d", *proc)
	default:
		return "unknown"
	}
}
