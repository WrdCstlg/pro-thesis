package schema

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// ---------------------------------------------------------------------------
// History log (directive 4.4): append-only JSONL, Jepsen/Elle compatible.
//
//	{"t_ns":...,"process":3,"type":"invoke","f":"write","key":"k/42","value":7,"op_id":8891}
//	{"t_ns":...,"process":3,"type":"ok","f":"write","key":"k/42","value":7,"op_id":8891}
//	{"t_ns":...,"process":5,"type":"invoke","f":"read","key":"k/42","op_id":8903}
//	{"t_ns":...,"process":5,"type":"fail","f":"read","key":"k/42","error":"timeout","op_id":8903}
//	{"t_ns":...,"type":"info","event":"phase","phase":"HEAL"}
//
// One flat struct covers both record shapes, because `type` does not
// discriminate them: invoke/ok/fail/info share the operation shape, and "info"
// is OVERLOADED; it is both the indeterminate operation outcome (which
// consistency checking depends on) and the type of a phase marker. Only
// `event` separates them. See RecordKind.
// ---------------------------------------------------------------------------

// HistoryEntry is one line of the history log.
//
// Fields whose zero value is a legitimate observation are pointers, so
// "absent" and "zero" stay distinguishable: process 0, op_id 0 and the empty
// key are all real values. Fields whose empty value is definitionally invalid
// are plain strings with omitempty.
//
// Struct order is the emission order and matches directive 4.4's first record
// exactly.
type HistoryEntry struct {
	// TNS is the timestamp in UNIX EPOCH NANOSECONDS.
	//
	// The field name is normative and says nanoseconds. Directive 4.4's
	// example VALUES (1725300000000000) are Unix epoch MICROSECONDS for
	// September 2024 (epoch nanoseconds for that date are ~1.7e18, three
	// orders of magnitude larger) so the example values are a specification
	// error and the field name governs. Drivers emit time.Now().UnixNano().
	//
	// Run-relative nanoseconds were rejected: the driver is an external,
	// unmodified process that cannot know the harness's virtual-clock origin,
	// so it can only emit wall time. The harness records the DRIVE-start wall
	// clock once, which makes the conversion to prothesis.oracle_input/v1's
	// start_ms/end_ms exact. See DECISIONS.md D-011 and OQ-008.
	TNS int64 `json:"t_ns"`

	// Process is the logical client id. A pointer because process 0 is real.
	Process *int64 `json:"process,omitempty"`

	Type HistoryType `json:"type"`

	// F is the operation name ("read", "write", "txn"). Required on an
	// operation record.
	F string `json:"f,omitempty"`

	// Key is the operated-on key. A pointer because the empty key is real.
	Key *string `json:"key,omitempty"`

	// Value is the operation's value, kept as raw JSON.
	//
	// RawMessage rather than any: it distinguishes absent from null from 0
	// from false, and it never lossily round-trips a large integer through
	// float64.
	Value json.RawMessage `json:"value,omitempty"`

	// OpID pairs an invoke with its completion. A pointer because op_id 0 is
	// real, and because the directive never marks it mandatory; Jepsen
	// histories use :process and :index.
	OpID *int64 `json:"op_id,omitempty"`

	// Error is the failure detail on a fail or info completion.
	Error string `json:"error,omitempty"`

	// Event, when non-empty, makes this a MARKER record rather than an
	// operation record. See EventPhase, EventFault, EventNote.
	Event string `json:"event,omitempty"`

	// Phase is the lifecycle phase named by a "phase" marker.
	Phase Phase `json:"phase,omitempty"`
}

// HistoryRecordKind is the union discriminator.
type HistoryRecordKind string

const (
	// RecordOperation is a driver-emitted operation record.
	RecordOperation HistoryRecordKind = "operation"
	// RecordMarker is a harness-emitted marker record.
	RecordMarker HistoryRecordKind = "marker"
)

// RecordKind reports which half of the union this entry belongs to.
//
// The discriminator is `event`, not `type`: a record carrying a non-empty
// event is a marker; a record without one is an operation. This is what lets
// type "info" keep BOTH of its normative meanings.
func (e *HistoryEntry) RecordKind() HistoryRecordKind {
	if e.Event != "" {
		return RecordMarker
	}
	return RecordOperation
}

// Validate enforces the operation/marker union.
//
// It is deliberately not called by HistoryReader.Next: a history is written by
// an external, user-authored load generator, and one malformed line must not
// abort a 500,000-line read. The caller decides the policy.
func (e *HistoryEntry) Validate() error {
	var errs ValidationErrors
	if !e.Type.Valid() {
		errs.Add("type", "unknown history type %q (want one of %v)", e.Type, AllHistoryTypes)
	}
	if e.RecordKind() == RecordMarker {
		if e.Type != HistoryInfo {
			errs.Add("type", "a marker record (event %q) must have type %q, got %q",
				e.Event, HistoryInfo, e.Type)
		}
		if e.F != "" {
			errs.Add("f", "a marker record carries no operation name, got %q", e.F)
		}
		// op_id and value are the two fields that make a record EVIDENCE. A
		// consumer discriminates on `event` alone, so a record carrying both an
		// event and an op_id is not merely malformed: it is an operation that
		// will be silently dropped by every marker-skipping reader, deleting a
		// completion from the history a verdict rests on. Rejecting it here is
		// what makes that deletion impossible rather than unlikely. See OQ-058.
		if e.OpID != nil {
			errs.Add("op_id", "a marker record carries no op_id, got %d "+
				"(a record with both an event and an op_id is an operation that "+
				"would be dropped as a marker)", *e.OpID)
		}
		if len(e.Value) > 0 {
			errs.Add("value", "a marker record carries no value, got %s", e.Value)
		}
		if e.Event == EventPhase && !e.Phase.Valid() {
			errs.Add("phase", "a %q marker must name a lifecycle phase, got %q (want one of %v)",
				EventPhase, e.Phase, AllPhases)
		}
	} else {
		if e.F == "" {
			errs.Add("f", "an operation record needs an operation name "+
				"(add \"event\" to make this a marker record)")
		}
		if e.Phase != "" {
			errs.Add("phase", "phase belongs to a marker record; this record has no \"event\"")
		}
	}
	return errs.OrNil()
}

// IsIndeterminate reports whether the record says the operation's outcome is
// unknown. Reporting an indeterminate outcome as ok or fail makes every
// consistency oracle unsound, which is why "info" exists.
func (e *HistoryEntry) IsIndeterminate() bool {
	return e.RecordKind() == RecordOperation && e.Type == HistoryInfo
}

// PhaseMarker builds the marker record the harness writes at a lifecycle
// transition.
func PhaseMarker(tNS int64, p Phase) HistoryEntry {
	return HistoryEntry{TNS: tNS, Type: HistoryInfo, Event: EventPhase, Phase: p}
}

// MarshalLine returns the entry's JSONL encoding without the newline. HTML
// escaping is off, as everywhere in this package.
func (e *HistoryEntry) MarshalLine() ([]byte, error) { return marshalNoEscape(e) }

// ---------------------------------------------------------------------------
// streaming writer
// ---------------------------------------------------------------------------

// HistoryWriter appends validated entries to a JSONL stream.
type HistoryWriter struct {
	w *bufio.Writer
	n int64
}

// NewHistoryWriter wraps w.
func NewHistoryWriter(w io.Writer) *HistoryWriter {
	return &HistoryWriter{w: bufio.NewWriter(w)}
}

// Write validates and appends one entry.
func (hw *HistoryWriter) Write(e *HistoryEntry) error {
	if err := e.Validate(); err != nil {
		return fmt.Errorf("history: refusing to write record %d: %w", hw.n+1, err)
	}
	b, err := e.MarshalLine()
	if err != nil {
		return fmt.Errorf("history: encode record %d: %w", hw.n+1, err)
	}
	if _, err := hw.w.Write(b); err != nil {
		return err
	}
	if err := hw.w.WriteByte('\n'); err != nil {
		return err
	}
	hw.n++
	return nil
}

// Count is the number of records written.
func (hw *HistoryWriter) Count() int64 { return hw.n }

// Flush pushes buffered records to the underlying writer.
func (hw *HistoryWriter) Flush() error { return hw.w.Flush() }

// ---------------------------------------------------------------------------
// streaming reader
// ---------------------------------------------------------------------------

// HistoryLineError reports a line that could not be decoded. It carries the
// raw line so a caller can report or quarantine it.
type HistoryLineError struct {
	Line int
	Raw  string
	Err  error
}

func (e *HistoryLineError) Error() string {
	return fmt.Sprintf("history line %d: %v (line was: %s)", e.Line, e.Err, e.Raw)
}

func (e *HistoryLineError) Unwrap() error { return e.Err }

// HistoryReader streams a JSONL history.
//
// It uses an unbounded line reader rather than bufio.Scanner: a single record
// carrying a large value would exceed Scanner's default 64 KiB token limit and
// truncate the history silently.
type HistoryReader struct {
	r    *bufio.Reader
	line int
	raw  []byte
}

// NewHistoryReader wraps r.
func NewHistoryReader(r io.Reader) *HistoryReader {
	return &HistoryReader{r: bufio.NewReader(r)}
}

// LineNo is the 1-based number of the line most recently returned.
func (hr *HistoryReader) LineNo() int { return hr.line }

// Raw returns the raw bytes of the line most recently returned, so a caller
// that needs byte fidelity (or that wants fields this struct does not model)
// can keep them. The slice is only valid until the next call to Next.
func (hr *HistoryReader) Raw() []byte { return hr.raw }

// Next decodes the next record.
//
// It returns io.EOF when the stream is exhausted. On a malformed line it
// returns a *HistoryLineError and REMAINS USABLE: the caller may keep reading
// and decide how many bad lines are tolerable.
func (hr *HistoryReader) Next() (*HistoryEntry, error) {
	for {
		raw, err := hr.readLine()
		if err != nil {
			return nil, err
		}
		hr.line++
		hr.raw = raw
		if len(bytes.TrimSpace(raw)) == 0 {
			continue
		}
		var e HistoryEntry
		if err := json.Unmarshal(raw, &e); err != nil {
			return nil, &HistoryLineError{Line: hr.line, Raw: string(raw), Err: err}
		}
		return &e, nil
	}
}

// readLine returns one line without its terminator, or io.EOF.
func (hr *HistoryReader) readLine() ([]byte, error) {
	b, err := hr.r.ReadBytes('\n')
	if len(b) == 0 {
		if err == nil {
			err = io.EOF
		}
		return nil, err
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	// Trim the terminator, and a CR before it: a history written inside a
	// container and read across a Windows bind mount may arrive CRLF-framed.
	// This is input tolerance on a log, not a relaxed assertion: the .thesis
	// world file, whose bytes are hashed, rejects CR outright.
	b = bytes.TrimSuffix(b, []byte{'\n'})
	b = bytes.TrimSuffix(b, []byte{'\r'})
	return b, nil
}

// ReadAllHistory reads every record, stopping at the first malformed line.
// Use HistoryReader directly when a bad line should be tolerated.
func ReadAllHistory(r io.Reader) ([]HistoryEntry, error) {
	hr := NewHistoryReader(r)
	out := make([]HistoryEntry, 0, 64)
	for {
		e, err := hr.Next()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return out, err
		}
		out = append(out, *e)
	}
}
