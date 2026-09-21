package schema

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
)

// ---------------------------------------------------------------------------
// prothesis.verdict/v1 (directive 4.6)
//
// The verdict is consumed by an autonomous agent, so it is a TOTAL contract:
// every defined key is present in every emitted verdict, empty lists are [],
// an empty witness is {}, and absent scalars are "" or 0. Exactly three fields
// may be null, because for each of them a fabricated zero would be a lie:
//
//	minimal_repro               shrinking never produced one
//	suspect                     no blame analysis ran
//	coverage_delta_vs_baseline  there is no baseline to compare against
//
// A fabricated `reproduced: "0/0"` in particular would violate the rule against
// claiming determinism the tool does not have.
//
// EVERY nested object is a named exported type. None is an anonymous inline
// struct, so later phases can compose them (a per-world result type, a report
// renderer) without reopening this file.
// ---------------------------------------------------------------------------

// Verdict is prothesis.verdict/v1.
type Verdict struct {
	Schema string `json:"schema"`
	RunID  string `json:"run_id"`
	// Profile is the run profile name.
	Profile string `json:"profile"`
	// Commit identifies the build that produced this verdict: the harness
	// binary's own build stamp when it carries one (D-070), else the working
	// tree's HEAD at run time.
	Commit string `json:"commit"`
	// Verdict is the outcome, bijective with the process exit code.
	Verdict VerdictResult `json:"verdict"`

	Budget     Budget      `json:"budget"`
	Violations []Violation `json:"violations"`

	Coverage Coverage `json:"coverage"`
	// CoverageDeltaVsBaseline is null when no baseline comparison was made.
	CoverageDeltaVsBaseline *CoverageDelta `json:"coverage_delta_vs_baseline"`

	OracleLock OracleLock `json:"oracle_lock"`
	// Artifacts is the run bundle directory.
	Artifacts string `json:"artifacts"`
}

// Budget is the verdict's `budget` object.
type Budget struct {
	WallS         int64 `json:"wall_s"`
	UsedS         int64 `json:"used_s"`
	WorldsPlanned int   `json:"worlds_planned"`
	WorldsRun     int   `json:"worlds_run"`

	// The three fields below are ADDITIVE and are emitted ONLY when `--worlds`
	// or `--budget` narrowed the profile. A run that took its budget from the
	// profile is byte-identical to one recorded before they existed, which is
	// what keeps the directive's own sample verdict a valid document.
	//
	// They exist because worlds_planned records the EFFECTIVE count, so a run
	// narrowed from 30 to 1 reported "1 run / 1 planned" and the number the
	// profile actually asks for appeared nowhere in the artifact. A reader
	// could only recover it by opening prothesis.yaml and cross-referencing the
	// profile name by hand, and nothing prompted them to. See D-059, OQ-059.

	// Narrowed reports that argv reduced the profile's budget.
	Narrowed bool `json:"narrowed,omitempty"`
	// WorldsRequired is the profile's own world count. WorldsUnbounded (-1)
	// means the profile sets no ceiling.
	WorldsRequired int `json:"worlds_required,omitempty"`
	// WallSRequired is the profile's own wall budget in seconds. Zero means
	// unbounded, exactly as it does for WallS.
	WallSRequired int64 `json:"wall_s_required,omitempty"`
}

// Violation is one entry of `violations`.
type Violation struct {
	ID     string      `json:"id"`
	Oracle string      `json:"oracle"`
	Class  OracleClass `json:"class"`
	// Severity is assigned by the engine, never reported by the oracle:
	// letting an oracle grade its own finding would be a gate-weakening
	// vector. See DefaultSeverity.
	Severity Severity `json:"severity"`
	// Phase is the lifecycle phase in which the violation was OBSERVED. It is
	// a different field from an oracle's valid_phases, which says where
	// evaluating that oracle is meaningful. The empty phase means "not scoped
	// to a phase" and decodes back cleanly.
	Phase       Phase   `json:"phase"`
	FirstSeenMS int64   `json:"first_seen_ms"`
	Explanation string  `json:"explanation"`
	Witness     Witness `json:"witness"`

	// MinimalRepro is null until shrinking produces a committed world.
	MinimalRepro *MinimalRepro `json:"minimal_repro"`
	// Shrink is never null: it carries `attempted`, which exists precisely to
	// express the not-attempted case.
	Shrink Shrink `json:"shrink"`

	CausalTimeline []TimelineEvent `json:"causal_timeline"`
	// Suspect is null when no blame analysis ran. It is better to say nothing
	// than to publish a fabricated confidence number.
	Suspect *Suspect `json:"suspect"`
}

// MinimalRepro points at the shrunk world that reproduces a violation.
type MinimalRepro struct {
	World string `json:"world"`
	Cmd   string `json:"cmd"`
	// Reproduced is "k/n": how many confirmation replays reproduced the
	// violation. It never claims more than was measured.
	Reproduced string `json:"reproduced"`
}

// Shrink records the minimization pipeline's result.
type Shrink struct {
	Attempted       bool     `json:"attempted"`
	FaultsBefore    int      `json:"faults_before"`
	FaultsAfter     int      `json:"faults_after"`
	OpsBefore       int      `json:"ops_before"`
	OpsAfter        int      `json:"ops_after"`
	SurvivingFaults []string `json:"surviving_faults"`
}

// TimelineEvent is one row of `causal_timeline`.
type TimelineEvent struct {
	TMS   int64  `json:"t_ms"`
	Event string `json:"event"`
	// Node is omitted on rows that are not node-scoped, matching directive
	// 4.6, which carries it only on the log row.
	Node   string `json:"node,omitempty"`
	Detail string `json:"detail"`
}

// Suspect is the blame analysis for a violation.
type Suspect struct {
	Files      []string `json:"files"`
	Confidence float64  `json:"confidence"`
	Basis      string   `json:"basis"`
}

// Coverage is the run's coverage counters. Phase 0 emits zeros; the signals
// themselves arrive in Phase 4.
type Coverage struct {
	NewTemplates int64 `json:"new_templates"`
	CumTemplates int64 `json:"cum_templates"`
	NewStates    int64 `json:"new_states"`
	CumStates    int64 `json:"cum_states"`
}

// CoverageDelta is `coverage_delta_vs_baseline`.
type CoverageDelta struct {
	Templates int64  `json:"templates"`
	Note      string `json:"note"`
}

// OracleLock is `oracle_lock`.
type OracleLock struct {
	Status LockStatus `json:"status"`
	// ManifestSHA is the computed manifest digest, "sha256:"-prefixed.
	ManifestSHA string `json:"manifest_sha"`
	// ExecutablesMoved names external oracles whose resolved PROGRAM no longer
	// matches the fingerprint recorded at lock time.
	//
	// The status stays `ok`: the digest covers definitions and the definitions
	// did not move, and hashing a binary into the digest would make an ordinary
	// rebuild exit 4. But the program named here is the one that DECIDED this
	// verdict, so a green result has to carry the fact that it changed:
	// otherwise a swapped checker produces a clean PASS and the artifact records
	// nothing unusual, which is exactly what OQ-057 measured.
	//
	// Additive and omitted when empty, so a verdict from a project whose
	// checkers did not move is byte-identical to one written before D-060.
	ExecutablesMoved []string `json:"executables_moved,omitempty"`
}

// ---------------------------------------------------------------------------
// Witness
// ---------------------------------------------------------------------------

// Witness is the evidence an oracle attaches to a finding.
//
// The directive's example carries op_ids and key, but a witness is authored by
// a third-party oracle and its useful content is oracle-specific, so any other
// members are preserved verbatim in Extra rather than dropped. Dropping them
// would throw away the evidence a human needs to confirm the finding.
type Witness struct {
	// OpIDs are the history op_ids that demonstrate the violation.
	OpIDs []int64
	// Key is the operated-on key, when the finding is key-scoped.
	Key string
	// Extra holds every other member of the witness object, verbatim.
	Extra map[string]json.RawMessage
}

// Witness member names the struct models directly.
const (
	witnessOpIDs = "op_ids"
	witnessKey   = "key"
)

// IsEmpty reports whether the witness carries nothing.
func (w Witness) IsEmpty() bool { return len(w.OpIDs) == 0 && w.Key == "" && len(w.Extra) == 0 }

// MarshalJSON emits only the members that are present, so an empty witness is
// {} rather than a shape full of nulls.
func (w Witness) MarshalJSON() ([]byte, error) {
	m := make(map[string]json.RawMessage, len(w.Extra)+2)
	for k, v := range w.Extra {
		if k == witnessOpIDs || k == witnessKey {
			continue // the typed fields win
		}
		m[k] = v
	}
	if w.OpIDs != nil {
		b, err := marshalNoEscape(w.OpIDs)
		if err != nil {
			return nil, err
		}
		m[witnessOpIDs] = b
	}
	if w.Key != "" {
		b, err := marshalNoEscape(w.Key)
		if err != nil {
			return nil, err
		}
		m[witnessKey] = b
	}
	return marshalNoEscape(m)
}

// UnmarshalJSON decodes strictly: a witness whose op_ids are not integers is a
// malformed oracle output, and DecodeOracleOutput turns that into
// INCONCLUSIVE rather than letting it pass as OK.
func (w *Witness) UnmarshalJSON(b []byte) error {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return fmt.Errorf("witness: must be an object: %w", err)
	}
	*w = Witness{}
	for k, v := range m {
		switch k {
		case witnessOpIDs:
			if err := json.Unmarshal(v, &w.OpIDs); err != nil {
				return fmt.Errorf("witness.op_ids: must be an array of integers: %w", err)
			}
		case witnessKey:
			if err := json.Unmarshal(v, &w.Key); err != nil {
				return fmt.Errorf("witness.key: must be a string: %w", err)
			}
		default:
			if w.Extra == nil {
				w.Extra = make(map[string]json.RawMessage, len(m))
			}
			w.Extra[k] = v
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// normalization and emission
// ---------------------------------------------------------------------------

// NewVerdict returns a normalized verdict for a run.
func NewVerdict(runID, profile string, result VerdictResult) Verdict {
	v := Verdict{
		Schema:  VerdictSchema,
		RunID:   runID,
		Profile: profile,
		Verdict: result,
	}
	v.Normalize()
	return v
}

// Normalize enforces the presence rule in place. WriteVerdict and
// MarshalVerdict both call it, so it is the single choke point.
//
// It never downgrades information: a severity that is already set is kept, and
// the three nullable fields are left null when they are null.
func (v *Verdict) Normalize() {
	v.Schema = VerdictSchema
	if v.Violations == nil {
		v.Violations = []Violation{}
	}
	for i := range v.Violations {
		v.Violations[i].normalize()
	}
	sort.SliceStable(v.Violations, func(i, j int) bool { return v.Violations[i].ID < v.Violations[j].ID })
	if v.OracleLock.Status == "" {
		// Absent is NOT equivalent to ok: an agent that deletes .prothesis/lock
		// must not thereby clear the drift check.
		v.OracleLock.Status = LockAbsent
	}
}

func (vi *Violation) normalize() {
	if vi.Severity == "" {
		vi.Severity = DefaultSeverity(vi.Class)
	}
	if vi.CausalTimeline == nil {
		vi.CausalTimeline = []TimelineEvent{}
	}
	if vi.Shrink.SurvivingFaults == nil {
		vi.Shrink.SurvivingFaults = []string{}
	}
	if vi.Suspect != nil && vi.Suspect.Files == nil {
		vi.Suspect.Files = []string{}
	}
}

// Validate checks the verdict's internal consistency. It is stricter than
// decoding, which stays lenient so a verdict from a newer tool version can
// still be read.
func (v *Verdict) Validate() error {
	var errs ValidationErrors
	if v.Schema != VerdictSchema {
		errs.Add("schema", "is %q, want %q", v.Schema, VerdictSchema)
	}
	if v.RunID == "" {
		errs.Add("run_id", "missing")
	} else if !ValidRunID(v.RunID) {
		errs.Add("run_id", "%q is not a run id (want %s)", v.RunID, RunIDShape)
	}
	if !v.Verdict.Valid() {
		errs.Add("verdict", "unknown verdict %q (want one of %v)", v.Verdict, AllVerdictResults)
	}
	if v.Violations == nil {
		errs.Add("violations", "is null; an empty verdict carries []")
	}
	seen := map[string]int{}
	for i, vi := range v.Violations {
		p := indexPath("violations", i)
		if vi.ID == "" {
			errs.Add(p+".id", "missing")
		} else if j, dup := seen[vi.ID]; dup {
			errs.Add(p+".id", "duplicate id %q (already used at violations[%d])", vi.ID, j)
		} else {
			seen[vi.ID] = i
		}
		if vi.Oracle == "" {
			errs.Add(p+".oracle", "missing")
		}
		if vi.Class != "" && !vi.Class.Valid() {
			errs.Add(p+".class", "unknown class %q (want one of %v)", vi.Class, AllOracleClasses)
		}
		if !vi.Severity.Valid() {
			errs.Add(p+".severity", "unknown severity %q (want one of %v)", vi.Severity, AllSeverities)
		}
		if vi.Phase != "" && !vi.Phase.Valid() {
			errs.Add(p+".phase", "unknown phase %q (want one of %v, or \"\" for unscoped)",
				vi.Phase, AllPhases)
		}
		if vi.Shrink.SurvivingFaults == nil {
			errs.Add(p+".shrink.surviving_faults", "is null; write []")
		}
		for j, f := range vi.Shrink.SurvivingFaults {
			if _, err := ParseFault(f); err != nil {
				errs.Add(indexPath(p+".shrink.surviving_faults", j), "%s", err.Error())
			}
		}
		if vi.CausalTimeline == nil {
			errs.Add(p+".causal_timeline", "is null; write []")
		}
		if vi.Suspect != nil && (vi.Suspect.Confidence < 0 || vi.Suspect.Confidence > 1) {
			errs.Add(p+".suspect.confidence", "must be in [0, 1], got %v", vi.Suspect.Confidence)
		}
	}
	if !v.OracleLock.Status.Valid() {
		errs.Add("oracle_lock.status", "unknown status %q (want one of %v)",
			v.OracleLock.Status, AllLockStatuses)
	}
	if v.Budget.UsedS < 0 || v.Budget.WallS < 0 {
		errs.Add("budget", "wall_s and used_s must not be negative")
	}
	return errs.OrNil()
}

// ExitCode is the process exit code this verdict encodes.
func (v *Verdict) ExitCode() ExitCode { return v.Verdict.ExitCode() }

// MarshalVerdict normalizes and returns the indented JSON a human reads, with
// a single trailing newline. HTML escaping is off, so the edge target n1<->n2
// survives inside surviving_faults.
func MarshalVerdict(v *Verdict) ([]byte, error) {
	v.Normalize()
	b, err := marshalNoEscape(v)
	if err != nil {
		return nil, fmt.Errorf("verdict: encode: %w", err)
	}
	return indentJSON(b)
}

// WriteVerdict is the single emission choke point.
func WriteVerdict(w io.Writer, v *Verdict) error {
	b, err := MarshalVerdict(v)
	if err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}

// UnmarshalVerdict decodes a verdict.
//
// It is deliberately lenient about unknown fields: a verdict emitted by a newer
// tool version must still be readable by `thesis report` and by any archived
// corpus reader. The strictness that matters is in prothesis.yaml and in the
// world file, both of which are gate surfaces; a verdict is an output.
func UnmarshalVerdict(data []byte) (*Verdict, error) {
	var v Verdict
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, fmt.Errorf("verdict: decode: %w", err)
	}
	if v.Schema != VerdictSchema {
		return nil, fmt.Errorf("verdict: schema is %q, want %q", v.Schema, VerdictSchema)
	}
	return &v, nil
}
