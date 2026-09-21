package kv

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync/atomic"
	"time"

	"prothesis.dev/kvfixture/internal/raft"
)

// WriteResponse is the body of a successful write.
type WriteResponse struct {
	Key      string `json:"key"`
	Value    int64  `json:"value"`
	Applied  bool   `json:"applied"`
	Index    uint64 `json:"index"`
	Term     uint64 `json:"term"`
	OpID     int64  `json:"op_id,omitempty"`
	ServedBy string `json:"served_by"`
}

// CASResponse is the body of a completed compare-and-swap. A CAS that ran and
// did not match returns 200 with ok:false and applied:true -- the operation
// definitely executed, it just did not swap.
type CASResponse struct {
	Key      string `json:"key"`
	OK       bool   `json:"ok"`
	Applied  bool   `json:"applied"`
	Have     *int64 `json:"have"`
	Value    int64  `json:"value"`
	Index    uint64 `json:"index"`
	Term     uint64 `json:"term"`
	OpID     int64  `json:"op_id,omitempty"`
	ServedBy string `json:"served_by"`
}

// TxnResponse is the body of a completed transaction.
type TxnResponse struct {
	Ops      []TxnOp `json:"ops"`
	Applied  bool    `json:"applied"`
	Index    uint64  `json:"index"`
	Term     uint64  `json:"term"`
	OpID     int64   `json:"op_id,omitempty"`
	ServedBy string  `json:"served_by"`
}

type writeBody struct {
	Value int64 `json:"value"`
	OpID  int64 `json:"op_id"`
}

type casBody struct {
	Expect *int64 `json:"expect"`
	Value  int64  `json:"value"`
	OpID   int64  `json:"op_id"`
}

type txnBody struct {
	OpID int64   `json:"op_id"`
	Ops  []TxnOp `json:"ops"`
}

func decodeBody(w http.ResponseWriter, r *http.Request, out any) bool {
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	if err := dec.Decode(out); err != nil {
		b, _ := json.Marshal(apiError{Code: "BAD_BODY", Applied: notApplied(), Message: err.Error()})
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write(b)
		return false
	}
	return true
}

func (s *Server) handleWrite(w http.ResponseWriter, r *http.Request) {
	key := keyFromPath(r.URL.Path)
	if key == "" {
		s.writeErr(w, http.StatusBadRequest, apiError{Code: "BAD_KEY", Applied: notApplied()})
		return
	}
	var body writeBody
	if !decodeBody(w, r, &body) {
		return
	}
	s.propose(w, r, Command{Op: "w", Key: key, Value: body.Value, OpID: body.OpID},
		func(res any, idx, term uint64) any {
			return WriteResponse{
				Key: key, Value: body.Value, Applied: true,
				Index: idx, Term: term, OpID: body.OpID, ServedBy: s.opts.NodeID,
			}
		})
}

func (s *Server) handleCAS(w http.ResponseWriter, r *http.Request) {
	key := keyFromPath(r.URL.Path)
	if key == "" {
		s.writeErr(w, http.StatusBadRequest, apiError{Code: "BAD_KEY", Applied: notApplied()})
		return
	}
	var body casBody
	if !decodeBody(w, r, &body) {
		return
	}
	s.propose(w, r, Command{Op: "cas", Key: key, Value: body.Value, Expect: body.Expect, OpID: body.OpID},
		func(res any, idx, term uint64) any {
			out := CASResponse{
				Key: key, Applied: true, Value: body.Value,
				Index: idx, Term: term, OpID: body.OpID, ServedBy: s.opts.NodeID,
			}
			if cr, ok := res.(CASResult); ok {
				out.OK = cr.OK
				if cr.Found {
					have := cr.Have
					out.Have = &have
				}
			}
			return out
		})
}

func (s *Server) handleTxn(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body txnBody
	if !decodeBody(w, r, &body) {
		return
	}
	if len(body.Ops) == 0 {
		s.writeErr(w, http.StatusBadRequest, apiError{Code: "EMPTY_TXN", Applied: notApplied()})
		return
	}
	s.propose(w, r, Command{Op: "txn", OpID: body.OpID, Ops: body.Ops},
		func(res any, idx, term uint64) any {
			out := TxnResponse{Applied: true, Index: idx, Term: term, OpID: body.OpID, ServedBy: s.opts.NodeID}
			if tr, ok := res.(TxnResult); ok {
				out.Ops = tr.Ops
			} else {
				out.Ops = body.Ops
			}
			return out
		})
}

// propose is the single admission path for every mutating operation.
//
// The error mapping is the server half of the ok/fail/info contract:
//
//	ErrNotLeader    -> 503 applied:false   the command never entered any log
//	ErrOverwritten  -> 409 applied:false   the entry was replaced by a later term
//	apply-wait deadline -> 504, NO applied flag, because the command may still commit
//
// The absence of applied:false is not an oversight in the timeout branch: it is
// the whole point. A driver that reads that response and records `fail` makes
// every consistency verdict built on the history unsound.
func (s *Server) propose(w http.ResponseWriter, r *http.Request, cmd Command, ok func(res any, idx, term uint64) any) {
	atomic.AddInt64(&s.inflight, 1)
	defer atomic.AddInt64(&s.inflight, -1)

	data, err := json.Marshal(cmd)
	if err != nil {
		s.writeErr(w, http.StatusBadRequest, apiError{Code: "BAD_COMMAND", Applied: notApplied(), Message: err.Error()})
		return
	}

	v := s.node.View()
	p, err := s.node.Propose(data)
	if err != nil {
		switch {
		case errors.Is(err, raft.ErrNotLeader):
			s.writeErr(w, http.StatusServiceUnavailable, apiError{
				Code: "NOT_LEADER", Applied: notApplied(), LeaderHint: v.LeaderID,
			})
		case errors.Is(err, raft.ErrStopped):
			// Shutting down mid-admission: indeterminate.
			s.writeErr(w, http.StatusServiceUnavailable, apiError{Code: "SHUTTING_DOWN"})
		default:
			s.writeErr(w, http.StatusInternalServerError, apiError{Code: "PROPOSE_FAILED", Message: err.Error()})
		}
		return
	}

	timer := time.NewTimer(s.opts.ApplyWait)
	defer timer.Stop()

	select {
	case res := <-p.Done():
		if res.Err != nil {
			if errors.Is(res.Err, raft.ErrOverwritten) {
				s.writeErr(w, http.StatusConflict, apiError{Code: "NOT_COMMITTED", Applied: notApplied()})
				return
			}
			// Anything else (including ErrStopped) is indeterminate.
			s.writeErr(w, http.StatusServiceUnavailable, apiError{Code: "INDETERMINATE", Message: res.Err.Error()})
			return
		}
		if e, isErr := res.Result.(error); isErr {
			s.writeErr(w, http.StatusInternalServerError, apiError{Code: "APPLY_ERROR", Message: e.Error()})
			return
		}
		s.writeJSON(w, http.StatusOK, ok(res.Result, res.Index, res.Term))

	case <-timer.C:
		s.node.Forget(p)
		s.writeErr(w, http.StatusGatewayTimeout, apiError{Code: "APPLY_TIMEOUT"})

	case <-r.Context().Done():
		s.node.Forget(p)
		s.writeErr(w, http.StatusGatewayTimeout, apiError{Code: "CLIENT_GONE"})
	}
}
