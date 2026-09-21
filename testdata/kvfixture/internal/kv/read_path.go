package kv

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"time"

	"prothesis.dev/kvfixture/internal/raft"
)

// ReadResponse is the body of a successful read.
type ReadResponse struct {
	Key         string `json:"key"`
	Found       bool   `json:"found"`
	Value       *int64 `json:"value"`
	Applied     bool   `json:"applied"`
	ServedBy    string `json:"served_by"`
	Term        uint64 `json:"term"`
	CommitIndex uint64 `json:"commit_index"`
	ReadMode    string `json:"read_mode"`
	OpID        int64  `json:"op_id,omitempty"`
	WriteOpID   int64  `json:"write_op_id,omitempty"`
	WriteIndex  uint64 `json:"write_index,omitempty"`
	WriteTerm   uint64 `json:"write_term,omitempty"`
}

// handleRead serves GET /kv/{key}.
//
// The default consistency mode is "lease". That is deliberate: the defect must
// be reachable by an ordinary client that asks for nothing special. A client
// that explicitly asks for consistency=linearizable takes the read-index path,
// which is correct, and the difference between the two answers is exactly what
// a differential oracle needs.
func (s *Server) handleRead(w http.ResponseWriter, r *http.Request) {
	key := keyFromPath(r.URL.Path)
	if key == "" {
		s.writeErr(w, http.StatusBadRequest, apiError{Code: "BAD_KEY", Applied: notApplied()})
		return
	}
	mode := r.URL.Query().Get("consistency")
	if mode == "" {
		mode = "lease"
	}

	atomic.AddInt64(&s.inflight, 1)
	defer atomic.AddInt64(&s.inflight, -1)

	v := s.node.View()

	if mode == "lease" {
		if v.Role == raft.RoleLeader && v.LeaseHeld {
			// ---- THE BUG'S OBSERVABLE SURFACE -------------------------------
			// No quorum round trip. If this node was displaced while its lease
			// was still nominally valid -- because the lease outlives the
			// election timeout by 8.3x -- the value below is stale and the
			// client is never told.
			atomic.AddUint64(&s.staleServed, 1)
			s.writeJSON(w, http.StatusOK, s.localRead(key, v, "lease"))
			return
		}
		if v.Role != raft.RoleLeader {
			s.writeErr(w, http.StatusServiceUnavailable, apiError{
				Code: "NOT_LEADER", Applied: notApplied(), LeaderHint: v.LeaderID,
			})
			return
		}
		// Leader, but the lease has lapsed: fall through to the correct path.
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.opts.ApplyWait)
	defer cancel()

	idx, err := s.node.ReadIndex(ctx)
	if err != nil {
		switch {
		case errors.Is(err, raft.ErrNotLeader):
			s.writeErr(w, http.StatusServiceUnavailable, apiError{
				Code: "NOT_LEADER", Applied: notApplied(), LeaderHint: v.LeaderID,
			})
		case errors.Is(err, raft.ErrNoQuorum):
			s.writeErr(w, http.StatusServiceUnavailable, apiError{
				Code: "NO_QUORUM", Applied: notApplied(),
			})
		default:
			// Indeterminate: no applied:false, so the client records `info`.
			s.writeErr(w, http.StatusGatewayTimeout, apiError{
				Code: "READ_TIMEOUT", Message: err.Error(),
			})
		}
		return
	}
	if err := s.node.WaitApplied(ctx, idx); err != nil {
		s.writeErr(w, http.StatusGatewayTimeout, apiError{Code: "APPLY_TIMEOUT", Message: err.Error()})
		return
	}
	s.writeJSON(w, http.StatusOK, s.localRead(key, s.node.View(), "linearizable"))
}

func (s *Server) localRead(key string, v raft.View, mode string) ReadResponse {
	resp := ReadResponse{
		Key:         key,
		Applied:     true,
		ServedBy:    s.opts.NodeID,
		Term:        v.Term,
		CommitIndex: v.CommitIndex,
		ReadMode:    mode,
	}
	if cur, ok := s.sm.Get(key); ok {
		val := cur.Value
		resp.Found = true
		resp.Value = &val
		resp.WriteOpID = cur.OpID
		resp.WriteIndex = cur.Index
		resp.WriteTerm = cur.Term
	}
	return resp
}

// LeaseWindowRemaining reports how long this node believes its read lease still
// runs. Zero when no lease is held.
func (s *Server) LeaseWindowRemaining() time.Duration {
	return s.node.View().LeaseRemaining
}
