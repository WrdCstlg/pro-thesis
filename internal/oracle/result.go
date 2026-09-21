package oracle

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// Result is what one oracle concluded about one world.
//
// It carries exactly the fields prothesis.oracle_output/v1 carries, minus the
// three an oracle does not choose for itself: `oracle`, `class` and
// `valid_phases` come from the Oracle's own declaration, so an implementation
// cannot report a finding under a class it never declared.
//
// There is deliberately no severity field here, for the same reason
// schema.OracleOutput has none: severity is assigned by the engine from the
// class (schema.DefaultSeverity), because letting an oracle grade its own
// finding down is a gate-weakening vector under invariant I6.
type Result struct {
	// Status is ok, violated or inconclusive.
	Status schema.OracleStatus

	// Phase is the lifecycle phase in which the finding was OBSERVED: a
	// different thing from the oracle's valid_phases, which says where
	// evaluating it is meaningful.
	//
	// Leave it empty and the engine derives it from FirstSeenMS against the
	// measured phase timings, which is how directive 4.6's own example carries
	// "phase": "DRIVE" on a violation found during ASSERT.
	Phase schema.Phase

	// FirstSeenMS is when the evidence first appears, in milliseconds relative
	// to DRIVE start.
	FirstSeenMS int64

	// Witness is the evidence. A violation without one is a claim without
	// proof, so every built-in attaches something a human can check.
	Witness schema.Witness

	// Explanation says what happened, in one sentence, to a reader who was not
	// there. It is mandatory on a violation and on an inconclusive result: "I
	// could not check" is only useful with the reason attached.
	Explanation string
}

// OK reports that the oracle checked and found nothing wrong.
func OK(format string, a ...any) Result {
	return Result{Status: schema.StatusOK, Explanation: fmt.Sprintf(format, a...)}
}

// Violated reports a finding. firstSeenMS places it on the timeline and w is
// the evidence.
func Violated(firstSeenMS int64, w schema.Witness, format string, a ...any) Result {
	return Result{
		Status:      schema.StatusViolated,
		FirstSeenMS: firstSeenMS,
		Witness:     w,
		Explanation: fmt.Sprintf(format, a...),
	}
}

// Inconclusive reports that the oracle could NOT check.
//
// This is a first-class outcome, not a soft failure. It maps to exit code 2 and
// never to 0, because reporting PASS over a property that was never evaluated
// is the most dangerous thing this tool can do.
func Inconclusive(format string, a ...any) Result {
	return Result{Status: schema.StatusInconclusive, Explanation: fmt.Sprintf(format, a...)}
}

// inPhase pins the phase the finding was observed in, suppressing the engine's
// derivation from FirstSeenMS.
//
// An oracle uses it when its evidence carries no usable timestamp: a log line,
// or a process exit whose time nothing recorded. Letting FirstSeenMS default to
// zero would otherwise place the finding at DRIVE start, which is a fabricated
// position on the timeline, and a fabricated position is worse than none: it is
// the field a human reads first when reconstructing what happened.
func (r Result) inPhase(p schema.Phase) Result {
	r.Phase = p
	return r
}

// IsViolation reports whether the result is a finding.
func (r Result) IsViolation() bool { return r.Status == schema.StatusViolated }

// IsInconclusive reports whether the oracle could not check.
func (r Result) IsInconclusive() bool { return r.Status == schema.StatusInconclusive }

// ResultFromOracleOutput converts an external oracle's stdout document into a
// Result.
//
// This is the Phase 3 seam: a process-backed Oracle runs the executable, hands
// the bytes and exit code to schema.DecodeOracleOutput, which already
// reconciles the two channels fail-closed, and converts the result here. The
// engine then treats it exactly like a built-in.
func ResultFromOracleOutput(out *schema.OracleOutput, observed schema.Phase, firstSeenMS int64) Result {
	if out == nil {
		return Inconclusive("the oracle produced no output document")
	}
	return Result{
		Status:      out.Status,
		Phase:       observed,
		FirstSeenMS: firstSeenMS,
		Witness:     out.Witness,
		Explanation: out.Explanation,
	}
}

// ---------------------------------------------------------------------------
// witness construction
// ---------------------------------------------------------------------------

// MaxWitnessTextBytes bounds a single text value in a witness.
//
// A panic witness quotes a log line, and a log line can be a megabyte of
// serialized state. The verdict is read by humans and by agents with a context
// budget, so quoted text is cut, and the cut is recorded in the witness as
// "<field>_truncated": true rather than hidden.
const MaxWitnessTextBytes = 512

// WitnessBuilder accumulates evidence into a schema.Witness.
//
// The witness's typed members (op_ids, key) are modelled by the schema; every
// other member goes into Extra verbatim, which is how an oracle-specific fact
// like a queue depth series or a container exit code survives into the verdict.
type WitnessBuilder struct {
	opIDs []int64
	key   string
	extra map[string]json.RawMessage
}

// NewWitness starts an empty witness.
func NewWitness() *WitnessBuilder { return &WitnessBuilder{} }

// OpID appends a history op_id.
func (b *WitnessBuilder) OpID(id int64) *WitnessBuilder {
	b.opIDs = append(b.opIDs, id)
	return b
}

// OpIDs appends several history op_ids.
func (b *WitnessBuilder) OpIDs(ids ...int64) *WitnessBuilder {
	b.opIDs = append(b.opIDs, ids...)
	return b
}

// Key sets the operated-on key.
func (b *WitnessBuilder) Key(k string) *WitnessBuilder {
	b.key = k
	return b
}

// Set records an arbitrary member. Values that cannot be encoded are recorded
// as an error string rather than dropped: silently losing evidence would defeat
// the purpose of the witness.
func (b *WitnessBuilder) Set(name string, v any) *WitnessBuilder {
	if b.extra == nil {
		b.extra = map[string]json.RawMessage{}
	}
	raw, err := encodeJSON(v)
	if err != nil {
		raw, _ = encodeJSON(fmt.Sprintf("<unencodable %T: %v>", v, err))
	}
	b.extra[name] = raw
	return b
}

// Text records a string member, truncating it to MaxWitnessTextBytes and
// setting "<name>_truncated": true when it does.
func (b *WitnessBuilder) Text(name, s string) *WitnessBuilder {
	cut, truncated := truncateText(s, MaxWitnessTextBytes)
	b.Set(name, cut)
	if truncated {
		b.Set(name+"_truncated", true)
	}
	return b
}

// Build returns the witness.
func (b *WitnessBuilder) Build() schema.Witness {
	w := schema.Witness{Key: b.key, Extra: b.extra}
	if len(b.opIDs) > 0 {
		ids := append([]int64(nil), b.opIDs...)
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		w.OpIDs = ids
	}
	return w
}

// encodeJSON marshals with HTML escaping off, matching pkg/schema's emitters:
// Go escapes '<' and '>' by default, which would rewrite an edge target such as
// n1<->n2 inside a witness.
func encodeJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// truncateText cuts s to at most n bytes on a valid UTF-8 boundary.
func truncateText(s string, n int) (string, bool) {
	if len(s) <= n {
		return s, false
	}
	cut := strings.ToValidUTF8(s[:n], "")
	return cut, true
}
