// Package kv is the replicated key-value state machine and the client-plane
// HTTP server that fronts it.
package kv

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
)

// Command is the replicated command encoding. It travels inside a Raft entry.
type Command struct {
	Op     string  `json:"op"` // "w" | "cas" | "txn" | "noop"
	Key    string  `json:"key,omitempty"`
	Value  int64   `json:"value,omitempty"`
	Expect *int64  `json:"expect,omitempty"`
	OpID   int64   `json:"op_id,omitempty"`
	Ops    []TxnOp `json:"ops,omitempty"`
}

// TxnOp is one micro-operation inside a transaction, encoded on the wire in the
// Elle read/write-register convention as ["r","k/3",null] or ["w","k/7",42].
type TxnOp struct {
	F     string // "r" or "w"
	Key   string
	Value *int64
}

// MarshalJSON encodes a TxnOp as a three-element array.
func (t TxnOp) MarshalJSON() ([]byte, error) {
	if t.Value == nil {
		return json.Marshal([]any{t.F, t.Key, nil})
	}
	return json.Marshal([]any{t.F, t.Key, *t.Value})
}

// UnmarshalJSON decodes a three-element array into a TxnOp.
func (t *TxnOp) UnmarshalJSON(b []byte) error {
	var raw []json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	if len(raw) != 3 {
		return fmt.Errorf("kv: txn op must have 3 elements, got %d", len(raw))
	}
	if err := json.Unmarshal(raw[0], &t.F); err != nil {
		return err
	}
	if err := json.Unmarshal(raw[1], &t.Key); err != nil {
		return err
	}
	if string(raw[2]) == "null" {
		t.Value = nil
		return nil
	}
	var v int64
	if err := json.Unmarshal(raw[2], &v); err != nil {
		return err
	}
	t.Value = &v
	return nil
}

// Value is one applied key, carrying the provenance that lets a consistency
// oracle attribute a stale read to the exact write that produced it.
type Value struct {
	Value int64  `json:"value"`
	OpID  int64  `json:"op_id"`
	Index uint64 `json:"index"`
	Term  uint64 `json:"term"`
}

// WriteResult is the apply-time outcome of a plain write.
type WriteResult struct {
	Index uint64
	Term  uint64
}

// CASResult is the apply-time outcome of a compare-and-swap. A CAS that runs
// and legitimately does not match is a SUCCESSFUL operation with a negative
// result, not a failure: OK is false but the operation definitely executed.
type CASResult struct {
	OK    bool
	Found bool
	Have  int64
}

// TxnResult carries a transaction back with its reads filled in.
type TxnResult struct {
	Ops []TxnOp
}

// ErrBadCommand is returned when an entry cannot be decoded. It is applied as a
// no-op so that one malformed entry cannot wedge the apply loop forever.
var ErrBadCommand = errors.New("kv: malformed command")

// StateMachine is the applied state. Every mutation happens inside Apply, in
// log order, so the map is a pure function of the committed prefix.
type StateMachine struct {
	mu      sync.RWMutex
	m       map[string]Value
	applied uint64
	ops     uint64
}

// NewStateMachine returns an empty state machine.
func NewStateMachine() *StateMachine {
	return &StateMachine{m: make(map[string]Value)}
}

// Apply implements raft.Applier.
func (s *StateMachine) Apply(index, term uint64, data json.RawMessage) any {
	var cmd Command
	if err := json.Unmarshal(data, &cmd); err != nil {
		s.mu.Lock()
		s.applied = index
		s.mu.Unlock()
		return ErrBadCommand
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.applied = index
	s.ops++

	switch cmd.Op {
	case "w":
		s.m[cmd.Key] = Value{Value: cmd.Value, OpID: cmd.OpID, Index: index, Term: term}
		return WriteResult{Index: index, Term: term}

	case "cas":
		cur, found := s.m[cmd.Key]
		if cmd.Expect == nil {
			// CAS against absence.
			if found {
				return CASResult{OK: false, Found: true, Have: cur.Value}
			}
			s.m[cmd.Key] = Value{Value: cmd.Value, OpID: cmd.OpID, Index: index, Term: term}
			return CASResult{OK: true}
		}
		if !found || cur.Value != *cmd.Expect {
			return CASResult{OK: false, Found: found, Have: cur.Value}
		}
		s.m[cmd.Key] = Value{Value: cmd.Value, OpID: cmd.OpID, Index: index, Term: term}
		return CASResult{OK: true, Found: true, Have: cur.Value}

	case "txn":
		// The whole transaction is one log entry applied under one lock, so it
		// is atomic by construction. There is no partial-apply path and hence
		// no second consistency defect hiding here.
		out := make([]TxnOp, len(cmd.Ops))
		for i, op := range cmd.Ops {
			switch op.F {
			case "r":
				if cur, ok := s.m[op.Key]; ok {
					v := cur.Value
					out[i] = TxnOp{F: "r", Key: op.Key, Value: &v}
				} else {
					out[i] = TxnOp{F: "r", Key: op.Key}
				}
			case "w":
				if op.Value != nil {
					s.m[op.Key] = Value{Value: *op.Value, OpID: cmd.OpID, Index: index, Term: term}
				}
				out[i] = op
			default:
				out[i] = op
			}
		}
		return TxnResult{Ops: out}

	case "noop":
		return WriteResult{Index: index, Term: term}

	default:
		return ErrBadCommand
	}
}

// Get returns the applied value for a key.
func (s *StateMachine) Get(key string) (Value, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.m[key]
	return v, ok
}

// Dump returns the whole applied state, for final_state.json.
func (s *StateMachine) Dump() map[string]Value {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]Value, len(s.m))
	for k, v := range s.m {
		out[k] = v
	}
	return out
}

// Keys returns the applied key set in sorted order.
func (s *StateMachine) Keys() []string {
	s.mu.RLock()
	out := make([]string, 0, len(s.m))
	for k := range s.m {
		out = append(out, k)
	}
	s.mu.RUnlock()
	sort.Strings(out)
	return out
}

// Stats reports the applied index and total applied command count.
func (s *StateMachine) Stats() (applied, ops uint64) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.applied, s.ops
}
