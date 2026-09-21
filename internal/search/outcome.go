package search

import (
	"fmt"
	"sort"
	"strings"

	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// ---------------------------------------------------------------------------
// What a world told us, and what it did NOT tell us
//
// This file exists because of the ranked #1 failure mode for Phase 4: A SEARCH
// THAT LEARNS FROM NOISE. If a clean world is indistinguishable from an
// unevaluable one, the utility signal is corrupt and every ranking downstream
// is meaningless.
//
// That is not a hypothetical here. OQ-033 measured it: on the `linear` profile
// no_stuck_op is structurally INCONCLUSIVE, because the harness stops the
// driver at QUIESCE with operations in flight and the oracle cannot tell a
// wedged operation from one that was simply not yet due. Three of the four
// measured `linear` worlds could not report PASS. A search reading "no
// violation" off those worlds would conclude the schedules were harmless. They
// were not evaluated.
//
// So:
//
//   - Signal is THREE-valued. There is no bool anywhere in this file.
//   - Clean requires that every configured oracle returned a definite answer.
//     One inconclusive oracle makes the whole world Unknown, because a verdict
//     is only as strong as its weakest evaluated claim.
//   - Observed records whether the world reached DRIVE at all. A world that
//     died in BOOT produced a truncated boot log, and folding that into
//     coverage would teach the search that failing to boot is novel.
// ---------------------------------------------------------------------------

// Signal is what a world's oracles established. It is deliberately not a bool.
type Signal string

const (
	// SignalViolated means at least one oracle returned a definite violation.
	SignalViolated Signal = "violated"
	// SignalClean means every configured oracle ran and every one returned ok.
	// It is the ONLY value that licenses the inference "these faults did not
	// break the system".
	SignalClean Signal = "clean"
	// SignalUnknown means the world produced no usable verdict: an oracle was
	// inconclusive or errored, no oracle ran, or the world never got far enough
	// to be checked. It is NOT a weak form of clean.
	SignalUnknown Signal = "unknown"
)

// Rank orders the three values for "is this at least as informative as that".
// Violated outranks clean outranks unknown, matching the run's own outcome
// ordering (a violated world's exit 1 outranks an inconclusive world's exit 2).
func (s Signal) Rank() int {
	switch s {
	case SignalViolated:
		return 2
	case SignalClean:
		return 1
	default:
		return 0
	}
}

// Finding is one oracle's answer for one world.
//
// Class and Severity are carried because Addendum A.6's payoff function reads
// both. Severity is assigned by the ENGINE and never by the oracle (D-028 item
// 5): letting an oracle grade its own finding would let it grade a finding
// down, and the Saboteur would then stop chasing it.
type Finding struct {
	Oracle   string
	Class    schema.OracleClass
	Severity schema.Severity
	Status   schema.OracleStatus
	// Phase is the lifecycle phase the violation was OBSERVED in. It is a
	// different field from the oracle's valid_phases (OQ-004, OQ-030) and the
	// two are never checked against each other here.
	Phase schema.Phase
	// FirstSeenMS is the DRIVE-relative offset of the evidence, when the oracle
	// supplied one.
	FirstSeenMS int64
	// Err is set when the oracle could not be run at all. An oracle that failed
	// to execute is inconclusive, never ok.
	Err string
}

// Definite reports whether this finding carries a usable claim.
func (f Finding) Definite() bool {
	return f.Err == "" && (f.Status == schema.StatusOK || f.Status == schema.StatusViolated)
}

// Outcome is the result of executing one world, in the terms a search reasons
// in.
//
// It carries every input Addendum A.6's utility function reads (violation
// class, severity, coverage delta, fault count, duration) without computing
// that function, which belongs to internal/search/saboteur.
type Outcome struct {
	// WorldHash identifies the world that produced it.
	WorldHash string
	// Faults are the world's planned faults, canonical.
	Faults []string
	// Ordinal is the world's position in the run.
	Ordinal int

	// Observed reports that the world actually reached DRIVE and produced
	// observations. A world that failed in BOOT, or whose schedule was refused,
	// is NOT observed and its logs are not evidence about the system.
	Observed bool
	// Findings is one entry per oracle the engine considered, including the ones
	// that returned ok. "checked and satisfied" and "never ran" are different
	// facts and only one of them supports a clean signal (D-034).
	Findings []Finding

	// Coverage is what this world contributed, measured against the run's global
	// coverage BEFORE this world was merged into it.
	Coverage Delta
	// Templates and States are the ids this world hit, sorted. They are what the
	// corpus computes domination and rare-event bias from.
	Templates []string
	States    []string

	// DurationMS is the world's wall cost, which is both the parsimony penalty's
	// second term and the corpus energy's cost term.
	DurationMS int64
	// Err is the harness-level failure, if any. It is not an oracle finding: a
	// world that could not run is not a world whose system passed.
	Err string
}

// Signal derives the three-valued answer.
//
// The rules, in order, and each closes a specific way a search learns from
// noise:
//
//  1. A world that did not run, or did not reach DRIVE, is Unknown. Its logs
//     are a truncated boot and its oracles were never given a history.
//  2. A world with a definite violation is Violated. This is checked BEFORE the
//     inconclusive test, matching the run's own outcome ordering: a violation
//     found alongside an inconclusive oracle is still a violation.
//  3. A world with no findings at all is Unknown, never Clean. Zero oracles is
//     the vacuous pass this project has already been bitten by twice.
//  4. A world with any indefinite finding is Unknown. This is the OQ-033 case
//     and it is the whole reason this type exists.
//  5. Otherwise Clean.
func (o Outcome) Signal() Signal {
	if o.Err != "" || !o.Observed {
		return SignalUnknown
	}
	for _, f := range o.Findings {
		if f.Definite() && f.Status == schema.StatusViolated {
			return SignalViolated
		}
	}
	if len(o.Findings) == 0 {
		return SignalUnknown
	}
	for _, f := range o.Findings {
		if !f.Definite() {
			return SignalUnknown
		}
	}
	return SignalClean
}

// Violations returns the definite violations, ordered by oracle name.
func (o Outcome) Violations() []Finding {
	out := make([]Finding, 0, len(o.Findings))
	for _, f := range o.Findings {
		if f.Definite() && f.Status == schema.StatusViolated {
			out = append(out, f)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Oracle < out[j].Oracle })
	return out
}

// Indefinite returns the oracles that could not answer, ordered by name. It is
// what a search reports when it declines to draw a conclusion, so a human can
// see WHY the world taught it nothing.
func (o Outcome) Indefinite() []Finding {
	out := make([]Finding, 0, len(o.Findings))
	for _, f := range o.Findings {
		if !f.Definite() {
			out = append(out, f)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Oracle < out[j].Oracle })
	return out
}

// FaultCount is the parsimony term's first input.
func (o Outcome) FaultCount() int { return len(o.Faults) }

// DurationSeconds is the parsimony term's second input.
func (o Outcome) DurationSeconds() float64 { return float64(o.DurationMS) / 1000.0 }

// Why explains, in one line, what this world established. It is written for a
// human reading a search log and is deliberately explicit about the unknown
// case, which is the one a reader will otherwise mistake for a pass.
func (o Outcome) Why() string {
	switch s := o.Signal(); s {
	case SignalViolated:
		names := make([]string, 0, 4)
		for _, f := range o.Violations() {
			names = append(names, f.Oracle)
		}
		return fmt.Sprintf("violated: %s", strings.Join(names, ", "))
	case SignalClean:
		return fmt.Sprintf("clean: all %d oracle(s) returned a definite ok", len(o.Findings))
	default:
		if o.Err != "" {
			return "unknown: the world did not complete: " + o.Err
		}
		if !o.Observed {
			return "unknown: the world never reached DRIVE, so nothing was observed"
		}
		if len(o.Findings) == 0 {
			return "unknown: no oracle was evaluated, so nothing was checked"
		}
		names := make([]string, 0, 4)
		for _, f := range o.Indefinite() {
			names = append(names, f.Oracle)
		}
		return fmt.Sprintf("unknown: %s could not answer, so a clean result cannot be claimed",
			strings.Join(names, ", "))
	}
}
