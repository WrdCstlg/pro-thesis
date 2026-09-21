package schema

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// ---------------------------------------------------------------------------
// The external oracle contract (directive 4.5)
//
// An oracle is an executable. It reads prothesis.oracle_input/v1 on stdin,
// writes prothesis.oracle_output/v1 on stdout, and exits 0 (OK), 1 (VIOLATED)
// or 2 (INCONCLUSIVE).
// ---------------------------------------------------------------------------

// OracleExitCode is the oracle process exit space.
//
// It is a DIFFERENT space from the CLI's ExitCode: 1 means VIOLATED here and
// FAIL there.
type OracleExitCode int

const (
	OracleExitOK           OracleExitCode = 0
	OracleExitViolated     OracleExitCode = 1
	OracleExitInconclusive OracleExitCode = 2
)

// Valid reports whether c is one of the three defined codes.
func (c OracleExitCode) Valid() bool {
	return c == OracleExitOK || c == OracleExitViolated || c == OracleExitInconclusive
}

// Status maps an exit code to a status. Any code outside 0..2 (an oracle that
// panicked, was OOM-killed, or exited 127 because it is not executable) maps
// to inconclusive, never to ok.
func (c OracleExitCode) Status() OracleStatus {
	switch c {
	case OracleExitOK:
		return StatusOK
	case OracleExitViolated:
		return StatusViolated
	default:
		return StatusInconclusive
	}
}

// OracleInput is prothesis.oracle_input/v1, the document written to an
// oracle's stdin.
type OracleInput struct {
	Schema      string `json:"schema"`
	HistoryPath string `json:"history_path"`
	// FinalStatePath and TelemetryPath name artifacts whose own schemas are
	// Phase 1 deliverables. Phase 0 carries the paths, not their contents.
	FinalStatePath string `json:"final_state_path"`
	TelemetryPath  string `json:"telemetry_path"`
	WorldPath      string `json:"world_path"`
	// Phases are MEASURED lifecycle windows in milliseconds relative to DRIVE
	// start, which is why DRIVE.start_ms is 0 in the directive's example. They
	// are what makes invariant I5 phase-aware assertion evaluable: an oracle
	// converts a history record's epoch t_ns into this frame using the DRIVE
	// origin the harness recorded.
	Phases PhaseTimings `json:"phases"`
}

// NewOracleInput returns an input document with the schema stamped.
func NewOracleInput(historyPath, finalStatePath, telemetryPath, worldPath string, phases PhaseTimings) OracleInput {
	if phases == nil {
		phases = PhaseTimings{}
	}
	return OracleInput{
		Schema:         OracleInputSchema,
		HistoryPath:    historyPath,
		FinalStatePath: finalStatePath,
		TelemetryPath:  telemetryPath,
		WorldPath:      worldPath,
		Phases:         phases,
	}
}

// Validate checks the input document.
func (in *OracleInput) Validate() error {
	var errs ValidationErrors
	if in.Schema != OracleInputSchema {
		errs.Add("schema", "is %q, want %q", in.Schema, OracleInputSchema)
	}
	if in.HistoryPath == "" {
		errs.Add("history_path", "missing")
	}
	if in.WorldPath == "" {
		errs.Add("world_path", "missing")
	}
	if in.Phases == nil {
		errs.Add("phases", "is null; write []")
	} else {
		errs.Merge("phases", in.Phases.Validate())
	}
	return errs.OrNil()
}

// MarshalOracleInput encodes the stdin document, indented, with a trailing
// newline. Oracles are third-party programs and some are shell scripts reading
// with `jq`; a readable document costs nothing at this size.
func MarshalOracleInput(in *OracleInput) ([]byte, error) {
	in.Schema = OracleInputSchema
	b, err := marshalNoEscape(in)
	if err != nil {
		return nil, fmt.Errorf("oracle_input: encode: %w", err)
	}
	return indentJSON(b)
}

// UnmarshalOracleInput decodes the stdin document. It is strict about unknown
// fields: this document is produced by PRO-THESIS itself, so an unknown key
// means a version mismatch the oracle should report rather than ignore.
func UnmarshalOracleInput(data []byte) (*OracleInput, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var in OracleInput
	if err := dec.Decode(&in); err != nil {
		return nil, fmt.Errorf("oracle_input: decode: %w", err)
	}
	if in.Schema != OracleInputSchema {
		return nil, fmt.Errorf("oracle_input: schema is %q, want %q", in.Schema, OracleInputSchema)
	}
	return &in, nil
}

// OracleOutput is prothesis.oracle_output/v1, the document an oracle writes to
// stdout.
type OracleOutput struct {
	Schema string `json:"schema"`
	// Oracle is the oracle's own name, e.g. "linearizable.kv".
	Oracle string      `json:"oracle"`
	Class  OracleClass `json:"class"`
	// ValidPhases is the oracle's invariant I5 declaration: the lifecycle
	// phases in which evaluating it is meaningful. It is a different field
	// from a violation's `phase`, which says where a finding was OBSERVED.
	ValidPhases []Phase      `json:"valid_phases"`
	Status      OracleStatus `json:"status"`
	Witness     Witness      `json:"witness"`
	Explanation string       `json:"explanation"`
}

// MarshalOracleOutput encodes the stdout document, indented, with a trailing
// newline.
func MarshalOracleOutput(out *OracleOutput) ([]byte, error) {
	out.Schema = OracleOutputSchema
	if out.ValidPhases == nil {
		out.ValidPhases = []Phase{}
	}
	b, err := marshalNoEscape(out)
	if err != nil {
		return nil, fmt.Errorf("oracle_output: encode: %w", err)
	}
	return indentJSON(b)
}

// ParseOracleOutput decodes an oracle's stdout strictly.
//
// Unknown fields are ALLOWED: the document is authored by a third party, and
// rejecting an oracle that reports extra detail would punish exactly the
// oracles worth having. What is not allowed is a wrong schema id or a
// malformed known field.
func ParseOracleOutput(data []byte) (*OracleOutput, error) {
	var out OracleOutput
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("oracle_output: decode: %w", err)
	}
	if out.Schema != OracleOutputSchema {
		return nil, fmt.Errorf("oracle_output: schema is %q, want %q", out.Schema, OracleOutputSchema)
	}
	if out.Status != "" && !out.Status.Valid() {
		return nil, fmt.Errorf("oracle_output: unknown status %q (want one of %v)",
			out.Status, AllOracleStatuses)
	}
	return &out, nil
}

// DecodeOracleOutput reconciles an oracle's two independent result channels
// (its process exit code and the status on its stdout) and is TOTAL: it always
// returns a usable output.
//
// The reconciliation rule is fail-closed (invariant I6): the result is the
// WORSE of the two readings, ordered ok < inconclusive < violated. PRO-THESIS
// never adopts the weaker reading of a disagreement, because in an agentic loop
// the weaker reading is exactly the one an adversary would arrange:
//
//   - an oracle that prints "violated" and then crashes would otherwise pass
//   - an oracle that prints "ok" and exits 1 would otherwise pass
//
// A parse failure, a wrong schema id, an empty stdout or an exit code outside
// 0..2 all map to inconclusive (which is exit 2, the code the agent-loop
// contract already handles as retry-once-then-escalate) and never to ok. The
// disagreement is recorded in Explanation so the reason survives into the
// verdict.
func DecodeOracleOutput(stdout []byte, exitCode int) *OracleOutput {
	fromExit := OracleExitCode(exitCode).Status()
	notes := make([]string, 0, 2)
	if !OracleExitCode(exitCode).Valid() {
		notes = append(notes, fmt.Sprintf("oracle exited %d, which is outside the 0/1/2 contract", exitCode))
	}

	out, err := ParseOracleOutput(stdout)
	if err != nil {
		if len(bytes.TrimSpace(stdout)) == 0 {
			notes = append(notes, "oracle wrote nothing to stdout")
		} else {
			notes = append(notes, err.Error())
		}
		return &OracleOutput{
			Schema:      OracleOutputSchema,
			Status:      StatusInconclusive.Worse(fromExit),
			ValidPhases: []Phase{},
			Explanation: strings.Join(notes, "; "),
		}
	}

	fromStdout := out.Status
	if fromStdout == "" {
		notes = append(notes, "oracle output carried no status")
		fromStdout = StatusInconclusive
	}
	reconciled := fromStdout.Worse(fromExit)
	if fromStdout != fromExit {
		notes = append(notes, fmt.Sprintf(
			"oracle status %q disagrees with exit code %d (%q); taking the stricter reading %q",
			fromStdout, exitCode, fromExit, reconciled))
	}
	out.Status = reconciled
	if out.ValidPhases == nil {
		out.ValidPhases = []Phase{}
	}
	if len(notes) > 0 {
		if out.Explanation == "" {
			out.Explanation = strings.Join(notes, "; ")
		} else {
			out.Explanation = out.Explanation + " [" + strings.Join(notes, "; ") + "]"
		}
	}
	return out
}

// Validate checks an oracle's stdout document.
func (out *OracleOutput) Validate() error {
	var errs ValidationErrors
	if out.Schema != OracleOutputSchema {
		errs.Add("schema", "is %q, want %q", out.Schema, OracleOutputSchema)
	}
	if out.Oracle == "" {
		errs.Add("oracle", "missing (the oracle's own name)")
	}
	if !out.Class.Valid() {
		errs.Add("class", "unknown class %q (want one of %v)", out.Class, AllOracleClasses)
	}
	if !out.Status.Valid() {
		errs.Add("status", "unknown status %q (want one of %v)", out.Status, AllOracleStatuses)
	}
	for i, p := range out.ValidPhases {
		if !p.Valid() {
			errs.Add(indexPath("valid_phases", i), "unknown phase %q (want one of %v)", p, AllPhases)
		}
	}
	if out.Status == StatusViolated && out.Explanation == "" {
		errs.Add("explanation", "a violated finding must explain itself")
	}
	return errs.OrNil()
}

// ValidIn reports whether the oracle declares itself meaningful in phase p. An
// oracle that declares no phases is treated as valid nowhere, so a missing
// declaration cannot silently widen where a finding counts.
func (out *OracleOutput) ValidIn(p Phase) bool {
	for _, q := range out.ValidPhases {
		if q == p {
			return true
		}
	}
	return false
}

// ToViolation converts a violated oracle output into a verdict violation.
//
// Severity is assigned here from the class, never read from the oracle: the
// oracle output schema has no severity field, and adding one would let an
// oracle grade its own finding down.
//
// The caller supplies id, the phase in which the finding was observed, and the
// first-seen offset; everything else comes from the oracle.
func (out *OracleOutput) ToViolation(id string, observed Phase, firstSeenMS int64) Violation {
	v := Violation{
		ID:          id,
		Oracle:      out.Oracle,
		Class:       out.Class,
		Severity:    DefaultSeverity(out.Class),
		Phase:       observed,
		FirstSeenMS: firstSeenMS,
		Explanation: out.Explanation,
		Witness:     out.Witness,
		Shrink:      Shrink{SurvivingFaults: []string{}},
	}
	v.normalize()
	return v
}
