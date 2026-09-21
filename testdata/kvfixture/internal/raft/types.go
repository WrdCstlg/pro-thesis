// Package raft implements the subset of the Raft consensus algorithm that the
// PRO-THESIS KV fixture needs: leader election, log replication and commit
// advance (Ongaro & Ousterhout, Figure 2), minus snapshots and minus membership
// change.
//
// The package carries EXACTLY ONE deliberate defect, and it is not in this file.
// See lease.go.
package raft

import (
	"encoding/json"
	"errors"
	"time"
)

// Role is a Raft server state.
type Role int32

// The three Raft server states.
const (
	RoleFollower Role = iota
	RoleCandidate
	RoleLeader
)

func (r Role) String() string {
	switch r {
	case RoleFollower:
		return "follower"
	case RoleCandidate:
		return "candidate"
	case RoleLeader:
		return "leader"
	default:
		return "unknown"
	}
}

// Entry is a single replicated log entry. Data == nil marks the no-op entry a
// leader appends on election so it can commit entries from prior terms safely
// (Figure 8 / section 5.4.2).
type Entry struct {
	Index uint64          `json:"i"`
	Term  uint64          `json:"t"`
	Data  json.RawMessage `json:"d,omitempty"`
}

// Applier is the replicated state machine. Apply is invoked exactly once per
// log index, in index order, while the Raft mutex is held; implementations must
// not call back into Raft.
type Applier interface {
	Apply(index, term uint64, data json.RawMessage) any
}

// AppendReq is the AppendEntries RPC request.
type AppendReq struct {
	Term         uint64  `json:"term"`
	LeaderID     string  `json:"leader_id"`
	PrevLogIndex uint64  `json:"prev_log_index"`
	PrevLogTerm  uint64  `json:"prev_log_term"`
	Entries      []Entry `json:"entries,omitempty"`
	LeaderCommit uint64  `json:"leader_commit"`
}

// AppendResp is the AppendEntries RPC response. ConflictIndex is the standard
// fast-backtrack optimisation; it is advisory and never affects safety.
type AppendResp struct {
	Term          uint64 `json:"term"`
	Success       bool   `json:"success"`
	MatchIndex    uint64 `json:"match_index"`
	ConflictIndex uint64 `json:"conflict_index,omitempty"`
}

// VoteReq is the RequestVote RPC request.
type VoteReq struct {
	Term         uint64 `json:"term"`
	CandidateID  string `json:"candidate_id"`
	LastLogIndex uint64 `json:"last_log_index"`
	LastLogTerm  uint64 `json:"last_log_term"`
}

// VoteResp is the RequestVote RPC response.
type VoteResp struct {
	Term    uint64 `json:"term"`
	Granted bool   `json:"granted"`
}

// Errors returned by the public API. They are deliberately distinguishable:
// ErrNotLeader and ErrOverwritten are positive evidence that a proposal did NOT
// execute, and the HTTP layer turns them into responses carrying
// "applied": false. Anything else must be reported to the client as
// indeterminate.
var (
	// ErrNotLeader means this node was not the leader when the proposal was
	// admitted. The command was never appended to any log.
	ErrNotLeader = errors.New("raft: not leader")
	// ErrOverwritten means the entry this proposal occupied was replaced by a
	// different entry from a later term, so it can never commit at that index.
	ErrOverwritten = errors.New("raft: proposal overwritten by a later term")
	// ErrNoQuorum means a linearizable read could not confirm leadership with a
	// majority within the RPC deadline.
	ErrNoQuorum = errors.New("raft: no quorum")
	// ErrStopped means the node is shutting down.
	ErrStopped = errors.New("raft: stopped")
)

// Config is the static configuration of one Raft node.
//
// Note what is NOT here: the read-lease duration. It is a compile-time constant
// in lease.go / lease_fixed.go and is deliberately not reachable from the
// environment, so the injected defect cannot be turned off by configuration.
type Config struct {
	ID    string
	Peers []string // peer IDs, self excluded
	Seed  uint64   // world seed; the per-node stream is derived from it

	TickInterval     time.Duration // scheduler poll interval
	HeartbeatPeriod  time.Duration
	ElectionMin      time.Duration
	ElectionMax      time.Duration
	RPCTimeout       time.Duration
	MaxEntriesPerRPC int
}

// DefaultConfig returns the fixture's measured timing budget.
//
//	heartbeat 100ms       3 heartbeats fit inside ElectionMin with 2x margin
//	election  600..900ms   6 missed heartbeats; immune to VM scheduling jitter
//	lease     5000ms       spec-mandated, and 8.3x larger than ElectionMin,
//	                       which is precisely the injected defect
func DefaultConfig(id string, peers []string, seed uint64) Config {
	return Config{
		ID:               id,
		Peers:            append([]string(nil), peers...),
		Seed:             seed,
		TickInterval:     25 * time.Millisecond,
		HeartbeatPeriod:  100 * time.Millisecond,
		ElectionMin:      600 * time.Millisecond,
		ElectionMax:      900 * time.Millisecond,
		RPCTimeout:       250 * time.Millisecond,
		MaxEntriesPerRPC: 256,
	}
}

func (c Config) validate() error {
	if c.ID == "" {
		return errors.New("raft: Config.ID is empty")
	}
	if c.TickInterval <= 0 || c.HeartbeatPeriod <= 0 || c.RPCTimeout <= 0 {
		return errors.New("raft: non-positive interval in Config")
	}
	if c.ElectionMin <= 0 || c.ElectionMax < c.ElectionMin {
		return errors.New("raft: invalid election timeout range")
	}
	if c.HeartbeatPeriod*3 > c.ElectionMin {
		return errors.New("raft: 3*HeartbeatPeriod must fit inside ElectionMin")
	}
	if c.MaxEntriesPerRPC <= 0 {
		return errors.New("raft: MaxEntriesPerRPC must be positive")
	}
	for _, p := range c.Peers {
		if p == c.ID {
			return errors.New("raft: Config.Peers contains self")
		}
	}
	return nil
}

// View is an immutable snapshot of the node's externally observable state. It
// is recomputed under the mutex on every call: nothing about role, term or
// lease is ever served from a cached publication, so the only consistency
// defect in this tree is the lease duration itself.
type View struct {
	ID             string
	Role           Role
	Term           uint64
	LeaderID       string
	CommitIndex    uint64
	LastApplied    uint64
	LastLogIndex   uint64
	LeaseHeld      bool
	LeaseRemaining time.Duration
	Ticks          uint64
	Uptime         time.Duration
	PendingApply   uint64 // commitIndex - lastApplied
	Waiters        int    // in-flight proposals awaiting apply
}
