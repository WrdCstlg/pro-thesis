package engine

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/WrdCstlg/pro-thesis/internal/control"
	"github.com/WrdCstlg/pro-thesis/internal/search"
	"github.com/WrdCstlg/pro-thesis/internal/search/saboteur"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// SearchRecordName is the search's own artifact, beside verdict.json.
//
// It is NOT part of any frozen contract, and it deliberately carries no
// `schema:` line naming a versioned document: inventing a
// `prothesis.search/v1` envelope would put a new normative artifact into a
// system whose contracts are frozen. It is a run record for a human and for the
// acceptance harness, and it is named as such.
const SearchRecordName = "search.json"

// SearchRecord is what the search did, written so a claim about it can be
// checked against a file rather than against a log line that has scrolled away.
type SearchRecord struct {
	RunID    string                `json:"run_id"`
	Profile  string                `json:"profile"`
	Strategy schema.SearchStrategy `json:"strategy"`
	Seed     uint64                `json:"seed"`
	Workers  int                   `json:"workers"`

	Stopped   string `json:"stopped_because"`
	WorldsRun int    `json:"worlds_run"`

	// FirstViolationOrdinal is 0 when no violation was found. It is the ONLY
	// honest summary of a right-censored search: D-029's pre-registered test is
	// a Fisher's exact on the COUNT of trials that found one, precisely because
	// a trial that found nothing has no finite time to report.
	FirstViolationOrdinal  int      `json:"first_violation_ordinal"`
	FirstViolationSeconds  *float64 `json:"first_violation_seconds"`
	FirstViolationFaults   []string `json:"first_violation_faults"`
	FirstViolationFaultCnt int      `json:"first_violation_fault_count"`

	Coverage schema.Coverage `json:"coverage"`

	// NetworksBefore and NetworksAfter are the leak check. -1 means the probe
	// could not run, which is not the same as "no change".
	NetworksBefore int `json:"free_bridge_networks_before"`
	NetworksAfter  int `json:"free_bridge_networks_after"`

	// Signals is A.3's ranked probe output.
	Signals []SignalRecord `json:"probe_signals"`
	// Trees is the D-015 measurement, per escalation tree.
	Trees []TreeRecord `json:"escalation_trees"`
	// TreeContributed is false when backpropagation never decided anything,
	// i.e. the tree behaved exactly as ladder-ordered enumeration.
	TreeContributed bool `json:"tree_contributed"`

	Worlds []WorldRecord `json:"worlds"`
}

// SignalRecord is one entry of A.3's ranked output.
type SignalRecord struct {
	Kind      schema.FaultKind `json:"kind"`
	Target    string           `json:"target"`
	Node      string           `json:"node"`
	Class     string           `json:"classification"`
	Rank      float64          `json:"rank"`
	Utility   float64          `json:"utility"`
	Evaluable bool             `json:"evaluable"`
	Escalate  bool             `json:"escalate"`
	Reason    string           `json:"reason"`
}

// TreeRecord is one escalation tree's measurement of itself.
type TreeRecord struct {
	Nodes              int  `json:"nodes"`
	Expansions         int  `json:"expansions"`
	ForcedSelections   int  `json:"forced_selections"`
	InformedSelections int  `json:"informed_selections"`
	Pruned             int  `json:"pruned"`
	MaxVisits          int  `json:"max_visits"`
	RevisitedNodes     int  `json:"revisited_nodes"`
	Contributed        bool `json:"backprop_influenced_a_decision"`
}

// WorldRecord is one executed world, as the search read it.
type WorldRecord struct {
	Ordinal      int      `json:"ordinal"`
	Label        string   `json:"label"`
	Seed         uint64   `json:"seed"`
	Faults       []string `json:"faults"`
	Signal       string   `json:"signal"`
	Why          string   `json:"why"`
	Outcome      string   `json:"outcome"`
	DurationMS   int64    `json:"duration_ms"`
	NewTemplates int64    `json:"new_templates"`
	NewStates    int64    `json:"new_states"`
	Project      string   `json:"project"`
	Teardown     string   `json:"teardown_error,omitempty"`
	Drain        string   `json:"drain"`
}

func (e *Engine) writeSearchRecord(res Result) {
	rec := SearchRecord{
		RunID:                 e.runner.RunID(),
		Profile:               e.runner.Profile(),
		Strategy:              res.Strategy,
		Seed:                  e.runner.Seed(),
		Workers:               e.workers(),
		Stopped:               res.StoppedBecause,
		WorldsRun:             len(res.Worlds),
		FirstViolationOrdinal: res.FirstViolationOrdinal,
		FirstViolationFaults:  res.FirstViolationFaults,
		Coverage:              res.Coverage,
		NetworksBefore:        res.NetworksBefore,
		NetworksAfter:         res.NetworksAfter,
		TreeContributed:       res.TreeContributed(),
	}
	if rec.FirstViolationFaults == nil {
		rec.FirstViolationFaults = []string{}
	}
	rec.FirstViolationFaultCnt = len(rec.FirstViolationFaults)
	if res.FirstViolationOrdinal > 0 {
		s := res.TimeToFirstViolation.Seconds()
		rec.FirstViolationSeconds = &s
	}
	for _, sig := range res.Signals {
		rec.Signals = append(rec.Signals, SignalRecord{
			Kind: sig.Kind, Target: sig.Target, Node: sig.Node,
			Class: sig.Class.String(), Rank: sig.Rank, Utility: sig.Utility,
			Evaluable: sig.Evaluable, Escalate: sig.Escalate, Reason: sig.Reason,
		})
	}
	if rec.Signals == nil {
		rec.Signals = []SignalRecord{}
	}
	for _, t := range res.TreeStats {
		rec.Trees = append(rec.Trees, TreeRecord{
			Nodes: t.Nodes, Expansions: t.Expansions,
			ForcedSelections: t.ForcedSelections, InformedSelections: t.InformedSelections,
			Pruned: t.Pruned, MaxVisits: t.MaxVisits, RevisitedNodes: t.RevisitedNodes,
			Contributed: t.TreeContributed(),
		})
	}
	if rec.Trees == nil {
		rec.Trees = []TreeRecord{}
	}

	bySignal := map[int]search.Outcome{}
	for _, o := range res.Outcomes {
		bySignal[o.Ordinal] = o
	}
	for _, w := range res.Worlds {
		out := bySignal[w.Ordinal]
		wr := WorldRecord{
			Ordinal: w.Ordinal, Label: w.Label, Seed: w.Seed,
			Faults:       w.Planned,
			Signal:       string(out.Signal()),
			Why:          out.Why(),
			Outcome:      string(w.Outcome),
			DurationMS:   w.Duration.Milliseconds(),
			NewTemplates: out.Coverage.NewTemplates,
			NewStates:    out.Coverage.NewStates,
			Project:      w.Project,
			Drain:        string(w.Drain.Outcome),
		}
		if wr.Faults == nil {
			wr.Faults = []string{}
		}
		if w.TeardownErr != nil {
			wr.Teardown = w.TeardownErr.Error()
		}
		rec.Worlds = append(rec.Worlds, wr)
	}
	if rec.Worlds == nil {
		rec.Worlds = []WorldRecord{}
	}

	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(e.runner.RunDir(), SearchRecordName), append(b, '\n'), 0o644)
}

func (e *Engine) workers() int {
	if e.opts.Workers > 0 {
		return e.opts.Workers
	}
	return DefaultWorkers
}

// hasDrive reports whether a world reached DRIVE, which is the line between "a
// world that observed the system" and "a world that failed to boot".
func hasDrive(t schema.PhaseTimings) bool {
	w, ok := t.Lookup(schema.PhaseDrive)
	return ok && w.EndMS >= w.StartMS
}

// explainBudget renders a budget for a log line.
func explainBudget(b control.Budget) string {
	wall := "unbounded"
	if !b.WallUnbounded() {
		wall = b.Wall.Round(time.Second).String()
	}
	worlds := "unbounded"
	if !b.WorldsUnbounded() {
		worlds = fmt.Sprintf("%d", b.Worlds)
	}
	return fmt.Sprintf("budget=%s worlds=%s", wall, worlds)
}

// ExplainSignals renders A.3's ranked output for a human.
func ExplainSignals(sigs []saboteur.ProbeSignal, n int) string {
	if len(sigs) == 0 {
		return "  (no probe produced a classifiable signal)\n"
	}
	out := ""
	for i, s := range sigs {
		if n > 0 && i >= n {
			out += fmt.Sprintf("  ... and %d more\n", len(sigs)-i)
			break
		}
		note := ""
		if !s.Evaluable {
			note = "  [UNEVALUABLE — not evidence about the system]"
		}
		out += fmt.Sprintf("  %2d. %-14s %-12s node=%-8s %-22s rank=%6.1f%s\n",
			i+1, s.Kind, s.Target, s.Node, s.Class, s.Rank, note)
	}
	return out
}
