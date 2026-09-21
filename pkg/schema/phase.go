package schema

import (
	"encoding/json"
	"fmt"

	"gopkg.in/yaml.v3"
)

// Phase is one of the eight virtual-clock lifecycle phases (directive 4.1).
//
// Phases ADVANCE linearly, but their time WINDOWS may overlap: PERTURB is
// contained within DRIVE ("overlaps DRIVE", directive 4.1 step 4). Callers must
// not assume phase_timings partitions the timeline.
//
// The empty Phase is a representable value meaning "not scoped to a phase". It
// is accepted on decode so that a verdict PRO-THESIS emits can be read back by
// PRO-THESIS: prothesis.verdict/v1 is a total contract with no omitempty, so a
// violation that is not phase-scoped emits `"phase":""`. Valid() is false for
// the empty phase; each Validate decides whether "unscoped" is legal there.
type Phase string

const (
	PhaseBoot     Phase = "BOOT"
	PhaseSeed     Phase = "SEED"
	PhaseDrive    Phase = "DRIVE"
	PhasePerturb  Phase = "PERTURB"
	PhaseHeal     Phase = "HEAL"
	PhaseQuiesce  Phase = "QUIESCE"
	PhaseAssert   Phase = "ASSERT"
	PhaseTeardown Phase = "TEARDOWN"
)

// AllPhases is in lifecycle order; the index is the phase ordinal.
var AllPhases = [...]Phase{
	PhaseBoot, PhaseSeed, PhaseDrive, PhasePerturb,
	PhaseHeal, PhaseQuiesce, PhaseAssert, PhaseTeardown,
}

// Valid reports whether p is one of the eight defined phases. The empty phase
// is not valid; use IsUnscoped to test for it.
func (p Phase) Valid() bool {
	for _, q := range AllPhases {
		if p == q {
			return true
		}
	}
	return false
}

// IsUnscoped reports whether p is the empty "not scoped to a phase" value.
func (p Phase) IsUnscoped() bool { return p == "" }

// Ordinal returns the lifecycle index of p, or -1 if p is not a defined phase.
func (p Phase) Ordinal() int {
	for i, q := range AllPhases {
		if p == q {
			return i
		}
	}
	return -1
}

// ParsePhase accepts exactly the eight normative spellings. It is deliberately
// case-sensitive: the wire form is uppercase, and silently coercing "drive" to
// DRIVE would hide a producer emitting a non-normative document.
func ParsePhase(s string) (Phase, error) {
	p := Phase(s)
	if !p.Valid() {
		return "", fmt.Errorf("schema: unknown phase %q (want one of %v)", s, AllPhases)
	}
	return p, nil
}

// UnmarshalJSON accepts the eight defined phases and the empty string
// (meaning "unscoped"). Anything else is rejected: an unrecognised phase
// silently treated as a known one would corrupt I5 phase-aware assertion.
func (p *Phase) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("schema: phase must be a string: %w", err)
	}
	if s == "" {
		*p = ""
		return nil
	}
	q, err := ParsePhase(s)
	if err != nil {
		return err
	}
	*p = q
	return nil
}

// UnmarshalYAML mirrors UnmarshalJSON for configuration documents.
func (p *Phase) UnmarshalYAML(n *yaml.Node) error {
	s, err := scalarString(n, "phase")
	if err != nil {
		return err
	}
	if s == "" {
		*p = ""
		return nil
	}
	q, err := ParsePhase(s)
	if err != nil {
		return err
	}
	*p = q
	return nil
}

// PhaseWindow is one entry of `phases` (prothesis.oracle_input/v1) and of
// `phase_timings` (the .thesis world file).
//
// Times are MILLISECONDS relative to DRIVE start, which is the virtual clock
// origin: directive 4.5 shows `{"phase":"DRIVE","start_ms":0,...}`. Windows
// before DRIVE therefore have negative bounds.
type PhaseWindow struct {
	Phase   Phase `json:"phase"    yaml:"phase"`
	StartMS int64 `json:"start_ms" yaml:"start_ms"`
	EndMS   int64 `json:"end_ms"   yaml:"end_ms"`
}

// Contains reports whether ms falls in [StartMS, EndMS).
func (w PhaseWindow) Contains(ms int64) bool { return ms >= w.StartMS && ms < w.EndMS }

// DurationMS is the window length in milliseconds.
func (w PhaseWindow) DurationMS() int64 { return w.EndMS - w.StartMS }

// Validate checks one window.
func (w PhaseWindow) Validate() error {
	if !w.Phase.Valid() {
		return fmt.Errorf("unknown phase %q (want one of %v)", w.Phase, AllPhases)
	}
	if w.EndMS < w.StartMS {
		return fmt.Errorf("phase %s: end_ms %d precedes start_ms %d", w.Phase, w.EndMS, w.StartMS)
	}
	return nil
}

// PhaseTimings is the ordered list of phase windows for one run. It is one of
// the five members of invariant I2's world tuple.
type PhaseTimings []PhaseWindow

// Lookup returns the window for phase p.
func (t PhaseTimings) Lookup(p Phase) (PhaseWindow, bool) {
	for _, w := range t {
		if w.Phase == p {
			return w, true
		}
	}
	return PhaseWindow{}, false
}

// At returns every phase whose window covers ms. More than one may match:
// PERTURB is contained within DRIVE.
func (t PhaseTimings) At(ms int64) []Phase {
	out := make([]Phase, 0, 2)
	for _, w := range t {
		if w.Contains(ms) {
			out = append(out, w.Phase)
		}
	}
	return out
}

// Normalize returns a fresh slice, never nil, in lifecycle order. It does not
// mutate the receiver: hashing a world must not reorder the caller's data.
func (t PhaseTimings) Normalize() PhaseTimings {
	out := make(PhaseTimings, len(t))
	copy(out, t)
	sortStable(out, func(a, b PhaseWindow) bool {
		ao, bo := a.Phase.Ordinal(), b.Phase.Ordinal()
		if ao != bo {
			return ao < bo
		}
		if a.StartMS != b.StartMS {
			return a.StartMS < b.StartMS
		}
		return a.EndMS < b.EndMS
	})
	return out
}

// Validate checks every window, rejects duplicates, and enforces the clock
// origin: when DRIVE is present its start_ms must be 0, because every other
// millisecond value in the system is expressed relative to it.
func (t PhaseTimings) Validate() error {
	var errs ValidationErrors
	seen := map[Phase]bool{}
	for i, w := range t {
		if err := w.Validate(); err != nil {
			errs.Add(indexPath("phase_timings", i), "%s", err.Error())
			continue
		}
		if seen[w.Phase] {
			errs.Add(indexPath("phase_timings", i), "duplicate phase %s", w.Phase)
		}
		seen[w.Phase] = true
	}
	if d, ok := t.Lookup(PhaseDrive); ok && d.StartMS != 0 {
		errs.Add("phase_timings.DRIVE.start_ms",
			"must be 0 (DRIVE start is the virtual clock origin), got %d", d.StartMS)
	}
	return errs.OrNil()
}
