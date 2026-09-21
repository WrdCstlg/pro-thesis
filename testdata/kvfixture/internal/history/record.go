// Package history writes the normative PRO-THESIS history log (JSONL) and
// classifies operation outcomes as ok / fail / info.
//
// The classifier in classify.go is the single most soundness-critical function
// in the fixture. One mis-categorised timeout makes every consistency verdict
// built on the history untrustworthy while still looking like it works.
package history

import "encoding/json"

// Type values, per the base directive section 4.4.
const (
	// TypeInvoke opens an operation's real-time interval.
	TypeInvoke = "invoke"
	// TypeOK means definite success.
	TypeOK = "ok"
	// TypeFail means the operation definitely did not happen.
	TypeFail = "fail"
	// TypeInfo means the state is indeterminate.
	TypeInfo = "info"
)

// Record is one line of the history log.
//
// The top-level field names and semantics are normative and are reproduced
// exactly: t_ns, process, type, f, key, value, error, op_id (plus event/phase
// for the harness's phase markers). Everything the fixture wants to add lives
// in the namespaced `meta` object, so no normative name or meaning is altered.
//
// t_ns is Unix EPOCH NANOSECONDS. The field name is normative and says
// nanoseconds; the directive's example values are epoch microseconds and are
// simply wrong (OQ-008). It is captured at the true event instant -- immediately
// before the request is written for an invoke, immediately after the response is
// read for a completion -- and NOT at the moment the writer wins its mutex. A
// linearizability checker's entire input is the real-time interval of each
// operation; timestamping at flush time would widen those intervals under
// contention and silently hide real violations.
//
// Consequence, stated plainly: the file is NOT guaranteed to be sorted by t_ns.
// Consumers must sort. meta.seq preserves append order.
type Record struct {
	TNS     int64            `json:"t_ns"`
	Process *int             `json:"process,omitempty"`
	Type    string           `json:"type"`
	F       string           `json:"f,omitempty"`
	Key     string           `json:"key,omitempty"`
	Value   *json.RawMessage `json:"value,omitempty"`
	Error   string           `json:"error,omitempty"`
	OpID    int64            `json:"op_id,omitempty"`
	Event   string           `json:"event,omitempty"`
	Phase   string           `json:"phase,omitempty"`
	Meta    *Meta            `json:"meta,omitempty"`
}

// Meta is the namespaced extension. Consistency checkers ignore it; oracles
// that build causal timelines and attribute stale reads read it. Nothing
// soundness-critical may depend on a field in here.
type Meta struct {
	Node      string `json:"node,omitempty"`
	Target    string `json:"target,omitempty"`
	Term      uint64 `json:"term,omitempty"`
	Commit    uint64 `json:"commit_index,omitempty"`
	ReadMode  string `json:"read_mode,omitempty"`
	ServedBy  string `json:"served_by,omitempty"`
	WriteOpID int64  `json:"write_op_id,omitempty"`
	Index     uint64 `json:"index,omitempty"`
	LatencyUS int64  `json:"latency_us,omitempty"`
	Attempt   int    `json:"attempt,omitempty"`
	Status    int    `json:"status,omitempty"`
	Code      string `json:"code,omitempty"`
	Seq       uint64 `json:"seq,omitempty"`
}

// RawInt64 wraps an integer as a history value.
func RawInt64(v int64) *json.RawMessage {
	b, _ := json.Marshal(v)
	rm := json.RawMessage(b)
	return &rm
}

// RawNull is an explicit JSON null, used when a read legitimately found
// nothing. It is distinct from omitting `value` entirely.
func RawNull() *json.RawMessage {
	rm := json.RawMessage("null")
	return &rm
}

// RawAny encodes an arbitrary value (used for the transaction op list).
func RawAny(v any) (*json.RawMessage, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	rm := json.RawMessage(b)
	return &rm, nil
}
