package kv

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"runtime"
	"strings"
	"sync/atomic"
	"time"

	"prothesis.dev/kvfixture/internal/raft"
)

// Options configure the client-plane server.
type Options struct {
	NodeID    string
	RoleHint  string
	ApplyWait time.Duration
	Logger    *log.Logger
}

// Server is the client plane: everything a workload generator, a health probe
// or a fault-target resolver talks to.
type Server struct {
	opts Options
	node Node
	sm   *StateMachine
	log  *log.Logger

	clientConns int64
	peerConns   int64
	inflight    int64
	staleServed uint64
	started     time.Time
}

// Node is the subset of *raft.Raft the server needs. Declared as an interface
// so the read and write paths can be exercised without a cluster.
type Node interface {
	ID() string
	View() raft.View
	Propose(json.RawMessage) (*raft.Proposal, error)
	Forget(*raft.Proposal)
	ReadIndex(context.Context) (uint64, error)
	WaitApplied(context.Context, uint64) error
}

// NewServer builds a client-plane server.
func NewServer(opts Options, r Node, sm *StateMachine) *Server {
	if opts.ApplyWait <= 0 {
		opts.ApplyWait = 2 * time.Second
	}
	lg := opts.Logger
	if lg == nil {
		lg = log.New(io.Discard, "", 0)
	}
	return &Server{opts: opts, node: r, sm: sm, log: lg, started: time.Now()}
}

// TrackClientConn is wired to the client listener's http.Server.ConnState.
func (s *Server) TrackClientConn(delta int64) { atomic.AddInt64(&s.clientConns, delta) }

// TrackPeerConn is wired to the peer listener's http.Server.ConnState. The peer
// count is the one that actually moves under a partition, so it is the useful
// coverage dimension.
func (s *Server) TrackPeerConn(delta int64) { atomic.AddInt64(&s.peerConns, delta) }

// ConnStateHook returns an http.Server.ConnState function updating a counter.
func ConnStateHook(track func(int64)) func(net.Conn, http.ConnState) {
	return func(_ net.Conn, st http.ConnState) {
		switch st {
		case http.StateNew:
			track(1)
		case http.StateHijacked, http.StateClosed:
			track(-1)
		}
	}
}

// ---------------------------------------------------------------- error shape

type apiError struct {
	Code string `json:"code"`
	// Applied is the client's ONLY positive evidence that an operation did not
	// execute. It is present and false on every error produced BEFORE the
	// command reached the state machine, and absent on every indeterminate
	// outcome. Getting this wrong makes every consistency verdict unsound.
	Applied    *bool  `json:"applied,omitempty"`
	LeaderHint string `json:"leader_hint,omitempty"`
	Message    string `json:"message,omitempty"`
}

func notApplied() *bool { b := false; return &b }

func (s *Server) writeErr(w http.ResponseWriter, status int, e apiError) {
	// 5xx responses are logged so `no_panic_log` and log-template coverage have
	// something real to read. 4xx and 503 NOT_LEADER are ordinary steady-state
	// traffic on a three-node cluster and would drown the log.
	if status >= 500 && e.Code != "NOT_LEADER" {
		s.log.Printf("level=warn event=request_error node=%s status=%d code=%s msg=%q",
			s.opts.NodeID, status, e.Code, e.Message)
	}
	b, _ := json.Marshal(e)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

func (s *Server) writeJSON(w http.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		s.writeErr(w, http.StatusInternalServerError, apiError{Code: "ENCODE", Message: err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

// ------------------------------------------------------------------- handlers

// Handler returns the client-plane routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealthz)
	mux.HandleFunc("/readyz", s.handleReadyz)
	mux.HandleFunc("/status", s.handleStatus)
	mux.HandleFunc("/dump", s.handleDump)
	mux.HandleFunc("/kv/", s.handleKV)
	mux.HandleFunc("/txn", s.handleTxn)
	mux.HandleFunc("/admin/noop", s.handleNoop)
	return mux
}

// HealthResponse is the /healthz body.
type HealthResponse struct {
	OK      bool   `json:"ok"`
	Node    string `json:"node"`
	Variant string `json:"variant"`
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	s.writeJSON(w, http.StatusOK, HealthResponse{OK: true, Node: s.opts.NodeID, Variant: raft.Variant})
}

func (s *Server) handleReadyz(w http.ResponseWriter, _ *http.Request) {
	v := s.node.View()
	ready := v.LeaderID != "" && v.CommitIndex > 0
	if !ready {
		s.writeErr(w, http.StatusServiceUnavailable, apiError{Code: "NOT_READY", Applied: notApplied()})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "node": s.opts.NodeID})
}

// Status is the observability tuple. Phase 2 resolves role:leader from it and
// Phase 4 builds state-abstraction tuples from it.
type Status struct {
	Node        string `json:"node"`
	Variant     string `json:"variant"`
	RoleHint    string `json:"role_hint,omitempty"`
	Role        string `json:"role"`
	Term        uint64 `json:"term"`
	LeaderID    string `json:"leader_id"`
	CommitIndex uint64 `json:"commit_index"`
	LastApplied uint64 `json:"last_applied"`
	LastLogIdx  uint64 `json:"last_log_index"`

	QueueDepth   int64 `json:"queue_depth"`
	PendingApply int64 `json:"pending_apply"`
	Waiters      int   `json:"waiters"`

	ClientConnections int64 `json:"client_connections"`
	PeerConnections   int64 `json:"peer_connections"`

	Lease StatusLease `json:"lease"`
	Clock StatusClock `json:"clock"`

	Goroutines  int    `json:"goroutines"`
	RSSBytes    uint64 `json:"rss_bytes"`
	UptimeMS    int64  `json:"uptime_ms"`
	StaleServed uint64 `json:"lease_reads_served"`
}

// StatusLease reports the read lease. remaining_ms is what the node BELIEVES it
// still holds; comparing it against the cluster's real term is the smoking gun.
type StatusLease struct {
	Held        bool  `json:"held"`
	RemainingMS int64 `json:"remaining_ms"`
	DurationMS  int64 `json:"duration_ms"`
}

// StatusClock reports both time bases.
type StatusClock struct {
	MonotonicMS int64 `json:"monotonic_ms"`
	WallUnixNS  int64 `json:"wall_unix_ns"`
	Ticks       uint64
}

// MarshalJSON keeps the ticks field snake_cased alongside the rest.
func (c StatusClock) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]any{
		"monotonic_ms": c.MonotonicMS,
		"wall_unix_ns": c.WallUnixNS,
		"ticks":        c.Ticks,
	})
}

func (s *Server) handleStatus(w http.ResponseWriter, _ *http.Request) {
	s.writeJSON(w, http.StatusOK, s.Status())
}

// Status assembles the observability tuple.
func (s *Server) Status() Status {
	v := s.node.View()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return Status{
		Node:              s.opts.NodeID,
		Variant:           raft.Variant,
		RoleHint:          s.opts.RoleHint,
		Role:              v.Role.String(),
		Term:              v.Term,
		LeaderID:          v.LeaderID,
		CommitIndex:       v.CommitIndex,
		LastApplied:       v.LastApplied,
		LastLogIdx:        v.LastLogIndex,
		QueueDepth:        atomic.LoadInt64(&s.inflight),
		PendingApply:      int64(v.PendingApply),
		Waiters:           v.Waiters,
		ClientConnections: atomic.LoadInt64(&s.clientConns),
		PeerConnections:   atomic.LoadInt64(&s.peerConns),
		Lease: StatusLease{
			Held:        v.LeaseHeld,
			RemainingMS: v.LeaseRemaining.Milliseconds(),
			DurationMS:  raft.LeaseDuration.Milliseconds(),
		},
		Clock: StatusClock{
			MonotonicMS: v.Uptime.Milliseconds(),
			WallUnixNS:  time.Now().UnixNano(),
			Ticks:       v.Ticks,
		},
		Goroutines:  runtime.NumGoroutine(),
		RSSBytes:    ms.Sys,
		UptimeMS:    time.Since(s.started).Milliseconds(),
		StaleServed: atomic.LoadUint64(&s.staleServed),
	}
}

func (s *Server) handleDump(w http.ResponseWriter, _ *http.Request) {
	v := s.node.View()
	s.writeJSON(w, http.StatusOK, map[string]any{
		"node":         s.opts.NodeID,
		"term":         v.Term,
		"role":         v.Role.String(),
		"commit_index": v.CommitIndex,
		"last_applied": v.LastApplied,
		"state":        s.sm.Dump(),
	})
}

func (s *Server) handleNoop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		OpID int64 `json:"op_id"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body)
	s.propose(w, r, Command{Op: "noop", OpID: body.OpID}, func(res any, idx, term uint64) any {
		return map[string]any{"applied": true, "index": idx, "term": term, "op_id": body.OpID}
	})
}

func keyFromPath(p string) string {
	rest := strings.TrimPrefix(p, "/kv/")
	rest = strings.TrimSuffix(rest, "/cas")
	return rest
}

func (s *Server) handleKV(w http.ResponseWriter, r *http.Request) {
	if strings.HasSuffix(r.URL.Path, "/cas") && r.Method == http.MethodPost {
		s.handleCAS(w, r)
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.handleRead(w, r)
	case http.MethodPut, http.MethodPost:
		s.handleWrite(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}
