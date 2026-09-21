package search

import (
	"github.com/WrdCstlg/pro-thesis/internal/telemetry"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// ---------------------------------------------------------------------------
// Combined coverage
//
// Two signals, one accumulator.
//
// BRANCH COVERAGE IS DELIBERATELY ABSENT, and this is the note recording that
// rather than an omission. The base spec ranks it last and says why: it needs
// build cooperation (a recompile with -cover, a JaCoCo agent, an LLVM profile
// runtime) that an arbitrary system under test may not give, and PRO-THESIS's
// stated premise is that it works on UNMODIFIED systems. Log-template coverage
// costs zero build changes in any language; that is precisely why the spec
// calls it the highest signal-to-integration-cost ratio in the system.
//
// It remains AVAILABLE-IF-INSTRUMENTED: a target that already emits a coverage
// profile could be folded in as a third set alongside Templates and States
// without changing Delta, Corpus, or any strategy, because nothing in this
// package reads a coverage set except through NewAgainst and Merge. Nothing is
// half-attempted here: there is no stub, no flag and no partial parser to
// mislead a reader into thinking the signal exists.
// ---------------------------------------------------------------------------

// Coverage is a template set and a state set observed together.
//
// A per-world Coverage is measured, scored against the run's global Coverage,
// and then merged into it. Scoring BEFORE merging is not a detail: merging
// first would make every world's delta zero.
type Coverage struct {
	Templates *TemplateSet
	States    *StateSet
}

// NewCoverage returns an empty accumulator.
func NewCoverage() *Coverage {
	return &Coverage{Templates: NewTemplateSet(), States: NewStateSet()}
}

// Delta is how much a world added.
type Delta struct {
	NewTemplates int64
	NewStates    int64
}

// Positive reports whether the world contributed anything at all. The corpus
// admits on a positive delta.
func (d Delta) Positive() bool { return d.NewTemplates > 0 || d.NewStates > 0 }

// Total is the combined count, used only for ordering two deltas.
func (d Delta) Total() int64 { return d.NewTemplates + d.NewStates }

// Against returns c's delta relative to base, without modifying either.
func (c *Coverage) Against(base *Coverage) Delta {
	if c == nil {
		return Delta{}
	}
	var bt *TemplateSet
	var bs *StateSet
	if base != nil {
		bt, bs = base.Templates, base.States
	}
	return Delta{
		NewTemplates: c.Templates.NewAgainst(bt),
		NewStates:    c.States.NewAgainst(bs),
	}
}

// Merge folds other into c and returns what was new.
func (c *Coverage) Merge(other *Coverage) Delta {
	if c == nil || other == nil {
		return Delta{}
	}
	if c.Templates == nil {
		c.Templates = NewTemplateSet()
	}
	if c.States == nil {
		c.States = NewStateSet()
	}
	return Delta{
		NewTemplates: c.Templates.Merge(other.Templates),
		NewStates:    c.States.Merge(other.States),
	}
}

// Counts renders the cumulative figures into the frozen verdict shape.
//
// NewTemplates and NewStates on the returned value are the caller's to fill
// from the world's Delta: schema.Coverage carries both the cumulative and the
// per-run new counts, and only the caller knows which run it is reporting.
func (c *Coverage) Counts() schema.Coverage {
	if c == nil {
		return schema.Coverage{}
	}
	return schema.Coverage{
		CumTemplates: int64(c.Templates.Len()),
		CumStates:    int64(c.States.Len()),
	}
}

// ---------------------------------------------------------------------------
// Building a world's coverage
// ---------------------------------------------------------------------------

// WorldObservation is everything a single executed world offers the coverage
// extractor.
//
// It is a plain struct of primitives rather than a control-package type, so
// internal/search does not import internal/control: control is what will drive
// a search, and the dependency would be a cycle. The caller in control fills
// Logs from the LogCollector it already ran and Telemetry from the document the
// collector already materialised. Nothing is re-collected here.
type WorldObservation struct {
	// Logs is one entry per node: the node's logical id and its collected log
	// lines, already stripped of the docker timestamp prefix by Phase 1's
	// ParseTimestampedLog.
	Logs []NodeLog
	// Telemetry is the world's materialised telemetry document, or nil when the
	// world produced none.
	Telemetry *telemetry.Document
	// Buckets is the state-abstraction bucketing. The zero value means
	// DefaultBuckets.
	Buckets Buckets
}

// NodeLog is one node's collected log text.
type NodeLog struct {
	Node  string
	Lines []string
}

// Observe builds a world's coverage from what the world produced.
//
// It returns the per-world Coverage. It does NOT merge into any global set and
// does not decide whether the world was evaluable: that is Outcome's job, and
// keeping the two apart is what stops an unevaluable world's coverage from
// being silently laundered into evidence.
func Observe(obs WorldObservation) *Coverage {
	cov := NewCoverage()
	for _, nl := range obs.Logs {
		cov.Templates.AddLines(nl.Node, nl.Lines)
	}
	// The zero Buckets value means DefaultBuckets; StatesFromDocument fills any
	// dimension left empty, so a partially-specified policy cannot silently
	// leave one component unbucketed.
	for _, g := range StatesFromDocument(obs.Telemetry, obs.Buckets) {
		cov.States.Add(g)
	}
	return cov
}
