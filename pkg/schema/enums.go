package schema

import (
	"encoding/json"
	"fmt"

	"gopkg.in/yaml.v3"
)

// ---------------------------------------------------------------------------
// Decoding policy for closed enums
//
// Closed enums decode STRICTLY: an unrecognised value is an error, never a
// silent coercion. An unrecognised history `type` treated as "info" would make
// consistency checking unsound, and an unrecognised oracle status treated as
// "ok" would be a gate-weakening hole.
//
// The empty string is a separate case. prothesis.verdict/v1 is a TOTAL contract
// with no omitempty, so a violation that is not phase-scoped, or that came from
// a third-party oracle that omitted `class`, emits `"phase":""` / `"class":""`.
// If the strict decoders rejected "", a verdict PRO-THESIS emits could not be
// read back by PRO-THESIS. So enums that appear in the verdict accept "" as
// "absent"; Valid() is false for it, and each Validate decides whether absent is
// legal in that position.
//
// HistoryType, Backend, BuiltinOracle, SearchStrategy and EscalationLadder do
// NOT accept "": in every position where they appear, an empty value is
// definitionally wrong, and an absent key never reaches the decoder at all.
// ---------------------------------------------------------------------------

// strictEnumJSON decodes a JSON string and checks membership.
func strictEnumJSON(b []byte, kind string, allowEmpty bool, ok func(string) bool) (string, error) {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return "", fmt.Errorf("schema: %s must be a string: %w", kind, err)
	}
	if s == "" && allowEmpty {
		return "", nil
	}
	if !ok(s) {
		return "", fmt.Errorf("schema: unknown %s %q", kind, s)
	}
	return s, nil
}

// strictEnumYAML decodes a YAML !!str scalar and checks membership.
func strictEnumYAML(n *yaml.Node, kind string, allowEmpty bool, ok func(string) bool) (string, error) {
	s, err := stringScalar(n, kind)
	if err != nil {
		return "", err
	}
	if s == "" && allowEmpty {
		return "", nil
	}
	if !ok(s) {
		return "", fmt.Errorf("%s: unknown value %q", kind, s)
	}
	return s, nil
}

// ---------------------------------------------------------------------------
// VerdictResult: the `verdict` field of prothesis.verdict/v1
// ---------------------------------------------------------------------------

// VerdictResult is the `verdict` field of prothesis.verdict/v1.
//
// The directive exhibits only "FAIL" (4.6) but names all six outcomes in the
// exit-code table (4.7); the six spellings here are those names. CONFIG_ERROR
// as a verdict document value is unusual (a config error means no run
// happened) but the bijection with the exit code is what makes `thesis gate
// --json` total, so it is defined rather than left as a hole.
type VerdictResult string

const (
	VerdictPass            VerdictResult = "PASS"
	VerdictFail            VerdictResult = "FAIL"
	VerdictInconclusive    VerdictResult = "INCONCLUSIVE"
	VerdictBudgetExhausted VerdictResult = "BUDGET_EXHAUSTED"
	VerdictOracleDrift     VerdictResult = "ORACLE_DRIFT"
	VerdictConfigError     VerdictResult = "CONFIG_ERROR"
)

// AllVerdictResults is every defined verdict, in exit-code order.
var AllVerdictResults = [...]VerdictResult{
	VerdictPass, VerdictFail, VerdictInconclusive,
	VerdictBudgetExhausted, VerdictOracleDrift, VerdictConfigError,
}

func (v VerdictResult) Valid() bool {
	for _, x := range AllVerdictResults {
		if v == x {
			return true
		}
	}
	return false
}

// ExitCode is the bijection back to the CLI exit code space. It returns -1 for
// an undefined verdict.
func (v VerdictResult) ExitCode() ExitCode {
	switch v {
	case VerdictPass:
		return ExitPass
	case VerdictFail:
		return ExitFail
	case VerdictInconclusive:
		return ExitInconclusive
	case VerdictBudgetExhausted:
		return ExitBudgetExhausted
	case VerdictOracleDrift:
		return ExitOracleDrift
	case VerdictConfigError:
		return ExitConfigError
	}
	return -1
}

func (v *VerdictResult) UnmarshalJSON(b []byte) error {
	s, err := strictEnumJSON(b, "verdict", true, func(x string) bool { return VerdictResult(x).Valid() })
	if err != nil {
		return err
	}
	*v = VerdictResult(s)
	return nil
}

// ---------------------------------------------------------------------------
// OracleClass: 8 values
// ---------------------------------------------------------------------------

// OracleClass is the `class` field of prothesis.oracle_output/v1 and of a
// verdict violation.
type OracleClass string

const (
	ClassCrash        OracleClass = "crash"
	ClassConsistency  OracleClass = "consistency"
	ClassLiveness     OracleClass = "liveness"
	ClassConvergence  OracleClass = "convergence"
	ClassResource     OracleClass = "resource"
	ClassSafety       OracleClass = "safety"
	ClassDifferential OracleClass = "differential"
	ClassMetamorphic  OracleClass = "metamorphic"
)

// AllOracleClasses is the complete closed set.
var AllOracleClasses = [...]OracleClass{
	ClassCrash, ClassConsistency, ClassLiveness, ClassConvergence,
	ClassResource, ClassSafety, ClassDifferential, ClassMetamorphic,
}

func (c OracleClass) Valid() bool {
	for _, x := range AllOracleClasses {
		if c == x {
			return true
		}
	}
	return false
}

func (c *OracleClass) UnmarshalJSON(b []byte) error {
	s, err := strictEnumJSON(b, "oracle class", true, func(x string) bool { return OracleClass(x).Valid() })
	if err != nil {
		return err
	}
	*c = OracleClass(s)
	return nil
}

func (c *OracleClass) UnmarshalYAML(n *yaml.Node) error {
	s, err := strictEnumYAML(n, "oracle class", true, func(x string) bool { return OracleClass(x).Valid() })
	if err != nil {
		return err
	}
	*c = OracleClass(s)
	return nil
}

// ---------------------------------------------------------------------------
// OracleStatus: 3 values
// ---------------------------------------------------------------------------

// OracleStatus is the `status` field of prothesis.oracle_output/v1.
type OracleStatus string

const (
	StatusOK           OracleStatus = "ok"
	StatusViolated     OracleStatus = "violated"
	StatusInconclusive OracleStatus = "inconclusive"
)

// AllOracleStatuses is the complete closed set.
var AllOracleStatuses = [...]OracleStatus{StatusOK, StatusViolated, StatusInconclusive}

func (s OracleStatus) Valid() bool {
	return s == StatusOK || s == StatusViolated || s == StatusInconclusive
}

// rank orders statuses for fail-closed reconciliation: ok < inconclusive <
// violated. An absent status ranks with inconclusive, never with ok.
func (s OracleStatus) rank() int {
	switch s {
	case StatusViolated:
		return 2
	case StatusInconclusive:
		return 1
	case StatusOK:
		return 0
	}
	return 1 // unknown or absent: never the weakest reading
}

// Worse returns the more severe of two statuses. Used to reconcile an external
// oracle's process exit code with the status on its stdout: PRO-THESIS never
// adopts the weaker reading of a disagreement (invariant I6).
func (s OracleStatus) Worse(o OracleStatus) OracleStatus {
	if o.rank() > s.rank() {
		return o
	}
	return s
}

func (s *OracleStatus) UnmarshalJSON(b []byte) error {
	v, err := strictEnumJSON(b, "oracle status", true, func(x string) bool { return OracleStatus(x).Valid() })
	if err != nil {
		return err
	}
	*s = OracleStatus(v)
	return nil
}

// ---------------------------------------------------------------------------
// Severity: the verdict's `severity` field
// ---------------------------------------------------------------------------

// Severity is the verdict-level grade of a violation.
//
// prothesis.oracle_output/v1 has NO severity field and prothesis.yaml has no
// per-oracle severity setting, so severity is assigned by the engine, never
// reported by the oracle. That is deliberate: letting an oracle grade its own
// finding is a gate-weakening vector under invariant I6.
type Severity string

const (
	SeverityLow    Severity = "low"
	SeverityMedium Severity = "medium"
	SeverityHigh   Severity = "high"
	// SeverityCritical is defined so the ladder can be extended without a
	// breaking change, but the v1 table (DefaultSeverity) never returns it: the
	// only severity value the directive exhibits pairs `"class":"consistency"`
	// with `"severity":"high"`, and re-grading the directive's own worked
	// example would put PRO-THESIS at odds with any CI policy written against
	// it.
	SeverityCritical Severity = "critical"
)

// AllSeverities is the complete closed set, weakest first.
var AllSeverities = [...]Severity{SeverityLow, SeverityMedium, SeverityHigh, SeverityCritical}

func (s Severity) Valid() bool {
	for _, x := range AllSeverities {
		if s == x {
			return true
		}
	}
	return false
}

// Rank orders severities, weakest first. An undefined severity ranks -1.
func (s Severity) Rank() int {
	for i, x := range AllSeverities {
		if s == x {
			return i
		}
	}
	return -1
}

// DefaultSeverity is the fixed class-to-severity table.
//
// consistency -> high is normative: directive 4.6 is the only place either
// document pairs a class with a severity, and it pairs consistency with high.
// Everything except resource follows it; an unknown or absent class grades high
// rather than low, because failing closed means never grading a finding down on
// incomplete information.
func DefaultSeverity(c OracleClass) Severity {
	if c == ClassResource {
		return SeverityMedium
	}
	return SeverityHigh
}

func (s *Severity) UnmarshalJSON(b []byte) error {
	v, err := strictEnumJSON(b, "severity", true, func(x string) bool { return Severity(x).Valid() })
	if err != nil {
		return err
	}
	*s = Severity(v)
	return nil
}

// ---------------------------------------------------------------------------
// HistoryType: 4 values
// ---------------------------------------------------------------------------

// HistoryType is the `type` field of a history log record (directive 4.4).
//
// The three completion outcomes are load-bearing for consistency checking:
//
//	ok   - the operation definitely happened
//	fail - the operation definitely did NOT happen
//	info - the outcome is INDETERMINATE
//
// A driver that cannot distinguish MUST emit info. Reporting an indeterminate
// outcome as ok or fail makes every consistency oracle unsound.
type HistoryType string

const (
	HistoryInvoke HistoryType = "invoke"
	HistoryOK     HistoryType = "ok"
	HistoryFail   HistoryType = "fail"
	HistoryInfo   HistoryType = "info"
)

// AllHistoryTypes is the complete closed set.
var AllHistoryTypes = [...]HistoryType{HistoryInvoke, HistoryOK, HistoryFail, HistoryInfo}

func (t HistoryType) Valid() bool {
	switch t {
	case HistoryInvoke, HistoryOK, HistoryFail, HistoryInfo:
		return true
	}
	return false
}

// UnmarshalJSON rejects the empty string as well as unknown values: every
// history record has a type, and a record without one is uninterpretable.
func (t *HistoryType) UnmarshalJSON(b []byte) error {
	v, err := strictEnumJSON(b, "history type", false, func(x string) bool { return HistoryType(x).Valid() })
	if err != nil {
		return err
	}
	*t = HistoryType(v)
	return nil
}

// ---------------------------------------------------------------------------
// LockStatus: the verdict's oracle_lock.status
// ---------------------------------------------------------------------------

// LockStatus is `oracle_lock.status` in prothesis.verdict/v1.
//
// The directive shows only "ok". The other two are required for the value set
// to be total: the first run, before `thesis oracles lock` has ever been
// invoked, has no lock to compare against and must not report "ok".
type LockStatus string

const (
	// LockOK: the computed manifest matches .prothesis/lock.
	LockOK LockStatus = "ok"
	// LockMismatch: drift. Exit 4, refuse to run.
	LockMismatch LockStatus = "mismatch"
	// LockAbsent: no lock file exists yet. NOT equivalent to ok: an agent that
	// deletes .prothesis/lock would otherwise clear the drift check entirely.
	LockAbsent LockStatus = "absent"
	// LockBypassed: the digest MATCHED, and then a lock-covered value was
	// overridden from the command line.
	//
	// NOT equivalent to ok, and the distinction is the whole point. The digest
	// is an honest statement about a FILE and says nothing about argv, so
	// `--worlds 1` against a profile locked at 30 leaves the lock reporting a
	// true "ok" while the gate that actually ran was a thirtieth of the
	// committed one. A status of ok on such a run is a true sentence that
	// functions as a false one. See D-059 and OQ-059.
	LockBypassed LockStatus = "bypassed"
)

// AllLockStatuses is the complete closed set.
var AllLockStatuses = [...]LockStatus{LockOK, LockMismatch, LockAbsent, LockBypassed}

func (s LockStatus) Valid() bool {
	return s == LockOK || s == LockMismatch || s == LockAbsent || s == LockBypassed
}

func (s *LockStatus) UnmarshalJSON(b []byte) error {
	v, err := strictEnumJSON(b, "oracle lock status", true, func(x string) bool { return LockStatus(x).Valid() })
	if err != nil {
		return err
	}
	*s = LockStatus(v)
	return nil
}

// ---------------------------------------------------------------------------
// Backend: harness.backend
// ---------------------------------------------------------------------------

// Backend is `harness.backend`. The directive's own comment enumerates four:
// `compose | process | k8s | sim`.
type Backend string

const (
	BackendCompose Backend = "compose"
	BackendProcess Backend = "process"
	BackendK8s     Backend = "k8s"
	BackendSim     Backend = "sim"
)

// AllBackends is the complete closed set.
var AllBackends = [...]Backend{BackendCompose, BackendProcess, BackendK8s, BackendSim}

func (b Backend) Valid() bool {
	for _, x := range AllBackends {
		if b == x {
			return true
		}
	}
	return false
}

// ImplementedV1 reports whether a backend has an implementation in v1. k8s and
// sim are legal spellings that parse successfully; selecting one is reported by
// Validate as "not implemented in v1" rather than as an unknown value, so the
// enum stays stable and the message is honest about why it does not run.
func (b Backend) ImplementedV1() bool {
	return b == BackendCompose || b == BackendProcess
}

func (b *Backend) UnmarshalYAML(n *yaml.Node) error {
	s, err := strictEnumYAML(n, "harness.backend", false, func(x string) bool { return Backend(x).Valid() })
	if err != nil {
		return fmt.Errorf("%w (want one of %v)", err, AllBackends)
	}
	*b = Backend(s)
	return nil
}

// ---------------------------------------------------------------------------
// BuiltinOracle: 6 values
// ---------------------------------------------------------------------------

// BuiltinOracle is one entry of `oracles.builtin`. Exactly six are named, in
// both the sample config (4.2) and the Phase 1 deliverables (6): the set below
// is the intersection, and no seventh is named anywhere.
//
// The implementations are Phase 1. This enum exists in Phase 0 only so the
// config can be validated and so the names cannot drift.
type BuiltinOracle string

const (
	BuiltinNoCrash                  BuiltinOracle = "no_crash"
	BuiltinNoPanicLog               BuiltinOracle = "no_panic_log"
	BuiltinNoUnboundedQueue         BuiltinOracle = "no_unbounded_queue"
	BuiltinResourceReturnToBaseline BuiltinOracle = "resource_return_to_baseline"
	BuiltinAvailabilityAfterHeal    BuiltinOracle = "availability_after_heal"
	BuiltinNoStuckOp                BuiltinOracle = "no_stuck_op"
)

// AllBuiltinOracles is the complete closed set, in the directive's order.
var AllBuiltinOracles = [...]BuiltinOracle{
	BuiltinNoCrash, BuiltinNoPanicLog, BuiltinNoUnboundedQueue,
	BuiltinResourceReturnToBaseline, BuiltinAvailabilityAfterHeal, BuiltinNoStuckOp,
}

func (o BuiltinOracle) Valid() bool {
	for _, x := range AllBuiltinOracles {
		if o == x {
			return true
		}
	}
	return false
}

// Class is the oracle class each built-in reports its findings under.
func (o BuiltinOracle) Class() OracleClass {
	switch o {
	case BuiltinNoCrash, BuiltinNoPanicLog:
		return ClassCrash
	case BuiltinNoUnboundedQueue, BuiltinResourceReturnToBaseline:
		return ClassResource
	case BuiltinAvailabilityAfterHeal, BuiltinNoStuckOp:
		return ClassLiveness
	}
	return ""
}

// ValidPhases is the invariant I5 phase-validity declaration for each built-in:
// the lifecycle phases in which evaluating it is meaningful.
//
// The three post-recovery oracles are restricted because evaluating them while
// a fault is still injected manufactures false positives, which is exactly what
// I5 exists to prevent. The two crash oracles are invariants and hold
// throughout.
func (o BuiltinOracle) ValidPhases() []Phase {
	switch o {
	case BuiltinNoUnboundedQueue, BuiltinResourceReturnToBaseline, BuiltinAvailabilityAfterHeal:
		return []Phase{PhaseQuiesce, PhaseAssert}
	case BuiltinNoStuckOp:
		return []Phase{PhaseHeal, PhaseQuiesce, PhaseAssert}
	case BuiltinNoCrash, BuiltinNoPanicLog:
		return append([]Phase(nil), AllPhases[:]...)
	}
	return nil
}

func (o *BuiltinOracle) UnmarshalYAML(n *yaml.Node) error {
	s, err := strictEnumYAML(n, "oracles.builtin", false, func(x string) bool { return BuiltinOracle(x).Valid() })
	if err != nil {
		return fmt.Errorf("%w (want one of %v)", err, AllBuiltinOracles)
	}
	*o = BuiltinOracle(s)
	return nil
}

// ---------------------------------------------------------------------------
// Open string sets: constants, not closed enums
// ---------------------------------------------------------------------------

// Role hints for harness.nodes[].role_hint. ADVISORY and system-specific: this
// is an OPEN set, not an enum. Closing it would foreclose third-party role
// vocabularies for no safety gain.
const (
	RoleLeader   = "leader"
	RoleFollower = "follower"
	RoleReplica  = "replica"
	RoleStorage  = "storage"
	RoleProxy    = "proxy"
	RoleClient   = "client"
)

// History marker `event` values. OPEN: drivers and the harness may emit others.
const (
	// EventPhase marks a lifecycle transition:
	// {"t_ns":...,"type":"info","event":"phase","phase":"HEAL"}
	EventPhase = "phase"
	// EventFault marks a fault injection or withdrawal.
	EventFault = "fault"
	// EventNote is free narrative.
	EventNote = "note"
)

// causal_timeline[].event values. OPEN: the timeline is presentational.
const (
	TimelineFault  = "fault"
	TimelineLog    = "log"
	TimelineOp     = "op"
	TimelinePhase  = "phase"
	TimelineOracle = "oracle"
)
