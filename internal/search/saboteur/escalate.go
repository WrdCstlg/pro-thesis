package saboteur

import (
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// ---------------------------------------------------------------------------
// A.5: Tier 2, escalation
//
// Tier 2 is activated only when OBSERVE classifies REINFORCE. It roots an MCTS
// tree at each top-ranked reinforcing signal and spends the escalation budget
// descending them, cheapest rung first.
//
// # Early termination, and the two things A.5 gets wrong about it (OQ-004)
//
// A.5: "If an oracle violation is detected during DRIVE or PERTURB, terminate
// the rollout immediately and backpropagate the full violation reward."
//
// FIRST DEFECT: it is unsound for the classes that matter. Directive §4.1 puts
// oracle evaluation in ASSERT, after HEAL and QUIESCE, and invariant I5 says
// "checking consistency during an active network partition generates false
// positives". Early termination is admissible only for oracle classes that are
// PREFIX-CLOSED: a violation observed on a prefix of the history remains a
// violation on the whole history. That is `crash` and `safety`. `consistency`
// and `convergence` are ASSERT-only, so a rollout hunting a consistency bug
// (the class A.6 rewards most, and the class the fixture's stale read belongs to)
// MUST run to ASSERT. Terminating it early would bias the search away from
// exactly the bugs the tool exists to find.
//
// SECOND DEFECT, and the worse one: "terminate immediately" skips HEAL. §4.3's
// Critical Guarantee requires HEAL to verify that no residual network rules or
// stopped processes persist. A rollout that skips it leaves live iptables rules
// and SIGSTOPped containers behind, poisoning every subsequent rollout, and it
// is triggered, perversely, BY SUCCESS. The more effective the Saboteur, the
// more corrupted its own search becomes. HEAL and TEARDOWN therefore run on
// EVERY path: violation, budget expiry, cancellation and error alike.
//
// Both readings are settled in D-022 and OQ-004 and are implemented here as
// policy, not as hope.
//
// # What early termination actually does in v1, stated plainly
//
// It stops the SEARCH, not the world. internal/oracle evaluates at ASSERT and
// exposes no mid-DRIVE hook, so there is no place from which a world could be
// cut short even for a prefix-closed class. Cutting a world short would also
// save nothing worth having: PERTURB already blocks only until the last
// withdrawal, and HEAL, QUIESCE and TEARDOWN would still have to run in full.
// So `MayTerminateEarly` decides whether a finding licenses ending the search
// immediately, and `TerminationScope` says which of the two things is meant.
// Claiming a within-world truncation this stack cannot perform would be the
// easy lie here.
// ---------------------------------------------------------------------------

// ErrEscalate reports an escalation that cannot be planned.
var ErrEscalate = errors.New("saboteur: escalate")

// PrefixClosedClasses are the oracle classes whose violations survive being
// observed on a prefix of the history.
//
// A crash is a crash: no continuation of the history un-crashes a process. A
// safety-property violation is by definition a bad state reached, and no suffix
// removes it. Every other class is a claim about the history AS A WHOLE:
// linearizability is not prefix-closed in the direction that matters here
// (a prefix may look non-linearizable only because its completing operations
// have not returned), and convergence is explicitly a claim about the end state.
var PrefixClosedClasses = [...]schema.OracleClass{schema.ClassCrash, schema.ClassSafety}

// PrefixClosed reports whether a violation of this class observed mid-run
// remains a violation of the whole run.
func PrefixClosed(c schema.OracleClass) bool {
	for _, x := range PrefixClosedClasses {
		if c == x {
			return true
		}
	}
	return false
}

// TerminationScope says what an early-termination decision authorises.
type TerminationScope string

const (
	// TerminateNothing: the finding does not license stopping anything early.
	TerminateNothing TerminationScope = "none"
	// TerminateSearch: stop proposing further worlds. The world that produced
	// the finding still completes HEAL, QUIESCE, ASSERT and TEARDOWN.
	TerminateSearch TerminationScope = "search"
)

// Termination is an early-termination decision, with its reason.
type Termination struct {
	Scope  TerminationScope
	Reason string
	// Oracle and Class name the finding that produced it, when there is one.
	Oracle string
	Class  schema.OracleClass
	Phase  schema.Phase
}

// Terminates reports whether the search should stop.
func (t Termination) Terminates() bool { return t.Scope == TerminateSearch }

// TerminationInput is one oracle finding, in the terms this decision needs.
type TerminationInput struct {
	Oracle string
	Class  schema.OracleClass
	// Definite is false when the oracle could not answer. An indefinite finding
	// never terminates anything: "could not check" is not "found a bug".
	Definite bool
	// Violated is true only for a definite violation.
	Violated bool
	// ObservedPhase is the lifecycle phase the evidence was seen in.
	ObservedPhase schema.Phase
}

// MayTerminateEarly applies A.5's rule with OQ-004's restriction.
//
// A violation observed in ASSERT always terminates the search: the world ran to
// completion and the finding is final, so there is nothing left to protect.
//
// A violation observed in DRIVE or PERTURB terminates the search only when its
// class is prefix-closed. A consistency finding stamped with a DRIVE phase is
// evidence the engine attributes to a moment in the timeline, not a licence to
// stop observing: the oracle that produced it still ran at ASSERT over the
// whole history, and a search that stopped on the strength of a mid-history
// glimpse would be acting on a claim nobody made.
func MayTerminateEarly(in TerminationInput) Termination {
	if !in.Definite || !in.Violated {
		reason := "no definite violation"
		if !in.Definite {
			reason = "the oracle could not answer, which is not a finding"
		}
		return Termination{Scope: TerminateNothing, Reason: reason, Oracle: in.Oracle, Class: in.Class}
	}
	switch in.ObservedPhase {
	case schema.PhaseDrive, schema.PhasePerturb:
		if !PrefixClosed(in.Class) {
			return Termination{
				Scope: TerminateNothing,
				Reason: fmt.Sprintf("%s is not prefix-closed, so a violation observed in %s is not "+
					"yet a violation of the whole history; the rollout runs to ASSERT (OQ-004)",
					in.Class, in.ObservedPhase),
				Oracle: in.Oracle, Class: in.Class, Phase: in.ObservedPhase,
			}
		}
		return Termination{
			Scope: TerminateSearch,
			Reason: fmt.Sprintf("%s is prefix-closed: a violation observed in %s remains one over the "+
				"whole history", in.Class, in.ObservedPhase),
			Oracle: in.Oracle, Class: in.Class, Phase: in.ObservedPhase,
		}
	default:
		return Termination{
			Scope:  TerminateSearch,
			Reason: "the world completed and an oracle returned a definite violation",
			Oracle: in.Oracle, Class: in.Class, Phase: in.ObservedPhase,
		}
	}
}

// FirstTermination applies MayTerminateEarly across a world's findings and
// returns the first decision that terminates, or the last non-terminating one.
// Findings are considered in oracle-name order so the decision does not depend
// on the order the engine happened to collect them.
func FirstTermination(ins []TerminationInput) Termination {
	sorted := append([]TerminationInput(nil), ins...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Oracle < sorted[j].Oracle })
	last := Termination{Scope: TerminateNothing, Reason: "no oracle reported a definite violation"}
	for _, in := range sorted {
		t := MayTerminateEarly(in)
		if t.Terminates() {
			return t
		}
		last = t
	}
	return last
}

// ---------------------------------------------------------------------------
// A.5's budget allocation
// ---------------------------------------------------------------------------

// BudgetSplit is A.5's partition of the total budget between the two tiers.
//
// "Probe budget: first 20% of worlds (or first 20% of wall-clock budget,
// whichever is reached first). Escalation budget: remaining 80%."
//
// The "whichever is reached first" is implemented literally: probing stops at
// the earlier of the two, so a sweep that turns out to be expensive cannot eat
// the escalation budget and a sweep that is cheap does not sit idle.
type BudgetSplit struct {
	// ProbeWall and ProbeWorlds are the Tier 1 ceilings. ProbeWorlds is -1 when
	// the run has no world ceiling.
	ProbeWall   time.Duration
	ProbeWorlds int
	// TotalWall and TotalWorlds are the run's own ceilings, repeated so a
	// caller can report the split without recomputing it.
	TotalWall   time.Duration
	TotalWorlds int
	// Pct is the percentage actually used.
	Pct int
}

// SplitBudget applies A.10's probe_budget_pct.
func SplitBudget(totalWall time.Duration, totalWorlds, pct int) BudgetSplit {
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	s := BudgetSplit{TotalWall: totalWall, TotalWorlds: totalWorlds, Pct: pct}
	if totalWall > 0 {
		s.ProbeWall = totalWall * time.Duration(pct) / 100
	}
	if totalWorlds > 0 {
		s.ProbeWorlds = totalWorlds * pct / 100
		if s.ProbeWorlds < 1 && pct > 0 {
			// A probe budget that rounds to zero worlds would drop the control
			// world, and with it every comparative criterion in A.3. One world
			// is the floor whenever any probing was asked for.
			s.ProbeWorlds = 1
		}
	} else {
		s.ProbeWorlds = -1
	}
	return s
}

// Explain renders the split for a log line.
func (s BudgetSplit) Explain() string {
	worlds := "unbounded"
	if s.ProbeWorlds >= 0 {
		worlds = fmt.Sprintf("%d of %d worlds", s.ProbeWorlds, s.TotalWorlds)
	}
	wall := "unbounded"
	if s.ProbeWall > 0 {
		wall = fmt.Sprintf("%s of %s", s.ProbeWall.Round(time.Second), s.TotalWall.Round(time.Second))
	}
	return fmt.Sprintf("Tier 1 probe budget: %d%% = %s / %s, whichever is reached first", s.Pct, wall, worlds)
}

// ---------------------------------------------------------------------------
// the escalation itself
// ---------------------------------------------------------------------------

// DefaultMaxTrees bounds how many reinforcing signals get their own tree.
//
// A.5 says the escalation budget is "allocated to MCTS trees rooted at the
// top-ranked REINFORCE signals" without saying how many. Three is chosen
// because the escalation budget buys tens of rollouts, not hundreds: spreading
// it over every reinforcing probe would give each tree too few rollouts to
// reach its compound rung, which is where A.7 says the bugs are.
const DefaultMaxTrees = 3

// EscalateOptions builds an Escalation.
type EscalateOptions struct {
	Tree TreeOptions
	// Signals is A.3's ranked output. Only reinforcing, evaluable entries are
	// escalated; A.5 says Tier 2 is "activated only when OBSERVE classifies
	// REINFORCE".
	Signals []ProbeSignal
	// MaxTrees bounds the number of roots. Zero means DefaultMaxTrees.
	MaxTrees int
	// RootAction builds the committed prefix for a signal's tree. nil means
	// LadderRootFor, which re-issues the reinforcing (kind, target) at the
	// ladder's own magnitude.
	RootAction func(ProbeSignal, TreeOptions) ([]string, error)
}

// Escalation is Tier 2's state: one tree per top-ranked reinforcing signal,
// visited round-robin so no single tree can consume the whole budget while a
// differently-rooted branch goes untried.
type Escalation struct {
	trees   []*Tree
	roots   []ProbeSignal
	next    int
	skipped []Skip
}

// NewEscalation builds the trees.
//
// It returns ErrNoReinforcingSignal when nothing reinforced, which is not a
// failure: A.5 says that case falls back to the base spec's stochastic corpus
// mutation, and "the Saboteur is an accelerant, not a replacement for baseline
// coverage".
func NewEscalation(opts EscalateOptions) (*Escalation, error) {
	max := opts.MaxTrees
	if max <= 0 {
		max = DefaultMaxTrees
	}
	roots := TopReinforcing(opts.Signals, max)
	if len(roots) == 0 {
		return nil, ErrNoReinforcingSignal
	}
	build := opts.RootAction
	if build == nil {
		build = LadderRootFor
	}
	e := &Escalation{}
	for _, sig := range roots {
		prefix, err := build(sig, opts.Tree)
		if err != nil {
			e.skipped = append(e.skipped, Skip{
				Rung:   RungGrayFail,
				What:   fmt.Sprintf("%s(%s)", sig.Kind, sig.Target),
				Reason: err.Error(),
			})
			continue
		}
		to := opts.Tree
		to.Root = prefix
		to.PriorTarget = sig.Target
		t, err := NewTree(to)
		if err != nil {
			return nil, err
		}
		e.trees = append(e.trees, t)
		e.roots = append(e.roots, sig)
	}
	if len(e.trees) == 0 {
		return nil, fmt.Errorf("%w: every reinforcing signal produced an unusable tree root", ErrEscalate)
	}
	return e, nil
}

// ErrNoReinforcingSignal reports that Tier 1 found nothing to escalate.
var ErrNoReinforcingSignal = errors.New("saboteur: no evaluable probe reinforced, so Tier 2 has no root")

// Trees returns the escalation's trees, in root order.
func (e *Escalation) Trees() []*Tree { return append([]*Tree(nil), e.trees...) }

// Roots returns the signals the trees are rooted at.
func (e *Escalation) Roots() []ProbeSignal { return append([]ProbeSignal(nil), e.roots...) }

// Skipped returns the roots that could not be built.
func (e *Escalation) Skipped() []Skip { return append([]Skip(nil), e.skipped...) }

// Next selects the next rollout, round-robin across trees.
//
// A tree that reports itself exhausted is retired rather than retried; when
// every tree is exhausted it returns ErrExhaustedTree, which the caller treats
// as "fall back to corpus mutation" rather than as an error.
func (e *Escalation) Next() (*Tree, *Node, error) {
	for tried := 0; tried < len(e.trees); tried++ {
		if len(e.trees) == 0 {
			break
		}
		i := e.next % len(e.trees)
		t := e.trees[i]
		n, err := t.Select()
		if errors.Is(err, ErrExhaustedTree) {
			e.trees = append(e.trees[:i:i], e.trees[i+1:]...)
			e.roots = append(e.roots[:i:i], e.roots[i+1:]...)
			if len(e.trees) == 0 {
				break
			}
			e.next = i % len(e.trees)
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		e.next = (i + 1) % len(e.trees)
		return t, n, nil
	}
	return nil, nil, ErrExhaustedTree
}

// LadderRootFor re-issues a reinforcing (kind, target) at the escalation
// ladder's own magnitude, as the tree's committed prefix.
//
// The probe that produced the signal ran at MINIMAL magnitude and a 2000 ms
// window, because that is what makes a probe cheap. Rooting the tree at that
// exact fault would carry the probe's deliberately weak magnitude into every
// branch below it, and A.5's whole purpose is to convert a detected spiral into
// a violation. So the ladder's magnitude for the kind is used instead, at the
// tree's own AtMS, and the target is carried over verbatim: the target is what
// the probe MEASURED, the magnitude is what Tier 2 chooses.
func LadderRootFor(sig ProbeSignal, opts TreeOptions) ([]string, error) {
	opts.fill()
	dur := opts.Timing.StressMS
	params := ""
	switch sig.Kind {
	case schema.FaultNetPartition:
		dur = opts.Timing.PartitionMS
	case schema.FaultProcPause:
		dur = opts.Timing.PauseMS
	case schema.FaultNetLatency:
		params = ", mean=500, jitter=50"
	case schema.FaultNetLoss:
		params = ", pct=5"
	case schema.FaultIOLatency:
		params = ", ms=200"
	}
	text := fmt.Sprintf("%s(%s%s)@%d..%d", sig.Kind, sig.Target, params, opts.AtMS, opts.AtMS+dur)
	canon, err := schema.CanonicalFault(text)
	if err != nil {
		return nil, fmt.Errorf("%w: root %q: %w", ErrEscalate, text, err)
	}
	return []string{canon}, nil
}
