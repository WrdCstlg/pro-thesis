package raft

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"sync"
	"time"
)

// ProposeResult is delivered once per accepted proposal.
//
// Err == nil means the command was committed and applied at Index/Term.
// Err == ErrOverwritten means it definitely did NOT execute.
// Any other error, and a caller that gives up waiting, must be reported to the
// client as indeterminate.
type ProposeResult struct {
	Index  uint64
	Term   uint64
	Result any
	Err    error
}

// Proposal is a handle on an in-flight command.
type Proposal struct {
	Index uint64
	Term  uint64
	ch    chan ProposeResult
}

// Done returns the single-shot completion channel.
func (p *Proposal) Done() <-chan ProposeResult { return p.ch }

// Raft is one node.
type Raft struct {
	cfg    Config
	peers  []string // sorted once; never ranged as a map, so broadcast order is deterministic
	tr     Transport
	app    Applier
	st     Storage
	logger *log.Logger

	mu sync.Mutex

	// Persistent state, fsynced before any RPC reply that depends on it.
	currentTerm uint64
	votedFor    string
	rlog        *Log

	// Volatile state on all servers.
	role        Role
	leaderID    string
	commitIndex uint64
	lastApplied uint64

	// Volatile state on leaders.
	nextIndex   map[string]uint64
	matchIndex  map[string]uint64
	lastAckTime map[string]time.Time
	// lastAckSend is the SEND time of the most recent AppendEntries this peer
	// acknowledged. ReadIndex needs it: an ack only confirms leadership as of
	// the moment its request left, so counting an ack whose request was already
	// in flight before the read index was captured would weaken the linearizable
	// read path. That path is meant to be correct.
	lastAckSend map[string]time.Time
	inflight    map[string]bool
	votes       map[string]bool

	// Deadlines. Every one of these is a CLOCK_MONOTONIC reading; nothing in
	// this implementation counts scheduler ticks, so a frozen process gains no
	// artificial credit anywhere.
	leaseDeadline     time.Time
	electionDeadline  time.Time
	heartbeatDeadline time.Time
	lastElectionReset time.Time
	lastDrawnTimeout  time.Duration

	waiters map[uint64]*Proposal
	rng     *rng

	ticks   uint64
	started time.Time
	closed  bool

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	loopWG sync.WaitGroup
}

// New constructs a node and restores any durable state. It does not start the
// event loop; call Start.
func New(cfg Config, tr Transport, app Applier, st Storage, logger *log.Logger) (*Raft, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if tr == nil || app == nil || st == nil {
		return nil, fmt.Errorf("raft: transport, applier and storage are all required")
	}
	if logger == nil {
		logger = log.New(discard{}, "", 0)
	}
	peers := append([]string(nil), cfg.Peers...)
	sort.Strings(peers)

	r := &Raft{
		cfg:         cfg,
		peers:       peers,
		tr:          tr,
		app:         app,
		st:          st,
		logger:      logger,
		rlog:        NewLog(),
		role:        RoleFollower,
		nextIndex:   make(map[string]uint64, len(peers)),
		matchIndex:  make(map[string]uint64, len(peers)),
		lastAckTime: make(map[string]time.Time, len(peers)),
		lastAckSend: make(map[string]time.Time, len(peers)),
		inflight:    make(map[string]bool, len(peers)),
		votes:       make(map[string]bool, len(peers)+1),
		waiters:     make(map[uint64]*Proposal),
		rng:         newRNG(DeriveNodeSeed(cfg.Seed, cfg.ID)),
		started:     time.Now(),
	}
	term, votedFor, err := st.LoadState()
	if err != nil {
		return nil, err
	}
	entries, err := st.LoadEntries()
	if err != nil {
		return nil, err
	}
	r.currentTerm, r.votedFor = term, votedFor
	r.rlog.Restore(entries)
	r.ctx, r.cancel = context.WithCancel(context.Background())
	r.resetElectionDeadline(time.Now())
	return r, nil
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

// ID returns this node's identity.
func (r *Raft) ID() string { return r.cfg.ID }

// Start launches the event loop.
func (r *Raft) Start() {
	r.loopWG.Add(1)
	go r.run()
}

// Stop halts the event loop, cancels outstanding RPCs, fails every waiting
// proposal as indeterminate, and closes storage.
func (r *Raft) Stop() {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.closed = true
	for idx, p := range r.waiters {
		delete(r.waiters, idx)
		p.ch <- ProposeResult{Index: idx, Err: ErrStopped}
	}
	r.mu.Unlock()

	r.cancel()
	r.loopWG.Wait()
	r.wg.Wait()
	_ = r.st.Close()
}

func (r *Raft) quorum() int { return (len(r.peers)+1)/2 + 1 }

func (r *Raft) run() {
	defer r.loopWG.Done()
	t := time.NewTicker(r.cfg.TickInterval)
	defer t.Stop()
	for {
		select {
		case <-r.ctx.Done():
			return
		case <-t.C:
			r.mu.Lock()
			r.ticks++
			r.onTick(time.Now())
			r.mu.Unlock()
		}
	}
}

// onTick runs with r.mu held. It is a pure scheduler: every decision compares
// monotonic deadlines, so the number of ticks actually delivered is irrelevant
// to correctness. A future rewrite of this loop cannot silently delete the
// injected defect, which lives in a constant, not in the loop's cadence.
func (r *Raft) onTick(now time.Time) {
	if r.closed {
		return
	}
	switch r.role {
	case RoleLeader:
		if !now.Before(r.heartbeatDeadline) {
			r.heartbeatDeadline = now.Add(r.cfg.HeartbeatPeriod)
			for _, p := range r.peers {
				r.sendAppendLocked(p)
			}
		}
	default:
		if !now.Before(r.electionDeadline) {
			r.becomeCandidate(now)
		}
	}
	r.applyCommitted()
}

func (r *Raft) resetElectionDeadline(now time.Time) {
	lo := int64(r.cfg.ElectionMin)
	hi := int64(r.cfg.ElectionMax)
	d := time.Duration(r.rng.between(lo, hi))
	r.lastDrawnTimeout = d
	r.lastElectionReset = now
	r.electionDeadline = now.Add(d)
}

func (r *Raft) persistStateLocked() {
	if err := r.st.SaveState(r.currentTerm, r.votedFor); err != nil {
		r.logger.Printf("level=error event=persist_state err=%q", err)
	}
}

func (r *Raft) becomeFollower(term uint64, leaderID string, now time.Time) {
	changed := term != r.currentTerm
	if changed {
		r.currentTerm = term
		r.votedFor = ""
	}
	if r.role != RoleFollower {
		r.logger.Printf("level=info event=role_change node=%s role=follower term=%d leader=%q", r.cfg.ID, term, leaderID)
	}
	r.role = RoleFollower
	r.leaderID = leaderID
	// A node that is no longer leader holds no lease. leaseHeld already
	// requires RoleLeader; this is belt and braces so GET /status cannot show a
	// stale remaining-lease figure after a step-down.
	r.leaseDeadline = time.Time{}
	r.resetElectionDeadline(now)
	if changed {
		r.persistStateLocked()
	}
}

func (r *Raft) becomeCandidate(now time.Time) {
	r.currentTerm++
	r.votedFor = r.cfg.ID
	r.persistStateLocked()
	r.role = RoleCandidate
	r.leaderID = ""
	r.leaseDeadline = time.Time{}
	r.votes = map[string]bool{r.cfg.ID: true}
	r.resetElectionDeadline(now)
	r.logger.Printf("level=info event=election_start node=%s term=%d last_index=%d last_term=%d",
		r.cfg.ID, r.currentTerm, r.rlog.LastIndex(), r.rlog.LastTerm())

	req := VoteReq{
		Term:         r.currentTerm,
		CandidateID:  r.cfg.ID,
		LastLogIndex: r.rlog.LastIndex(),
		LastLogTerm:  r.rlog.LastTerm(),
	}
	for _, p := range r.peers {
		r.sendVoteLocked(p, req)
	}
	if len(r.votes) >= r.quorum() {
		r.becomeLeader(now)
	}
}

func (r *Raft) becomeLeader(now time.Time) {
	r.role = RoleLeader
	r.leaderID = r.cfg.ID
	// A brand new leader has NOT earned a lease. It must complete a heartbeat
	// round with a majority before it may serve a local read.
	r.leaseDeadline = time.Time{}
	for _, p := range r.peers {
		r.nextIndex[p] = r.rlog.LastIndex() + 1
		r.matchIndex[p] = 0
		r.lastAckTime[p] = time.Time{}
		r.lastAckSend[p] = time.Time{}
	}
	// No-op entry on election (section 5.4.2): without it a new leader can
	// never safely commit entries carried over from a previous term.
	e := Entry{Index: r.rlog.LastIndex() + 1, Term: r.currentTerm}
	r.rlog.Append(e)
	if err := r.st.AppendEntries([]Entry{e}); err != nil {
		r.logger.Printf("level=error event=append_noop err=%q", err)
		r.rlog.TruncateFrom(e.Index)
		r.becomeFollower(r.currentTerm, "", now)
		return
	}
	r.logger.Printf("level=info event=elected node=%s term=%d noop_index=%d", r.cfg.ID, r.currentTerm, e.Index)
	r.heartbeatDeadline = now.Add(r.cfg.HeartbeatPeriod)
	for _, p := range r.peers {
		r.sendAppendLocked(p)
	}
	r.maybeAdvanceCommit()
}

func (r *Raft) sendAppendLocked(peer string) {
	if r.closed || r.role != RoleLeader || r.inflight[peer] {
		return
	}
	ni := r.nextIndex[peer]
	if ni < 1 {
		ni = 1
	}
	prevIndex := ni - 1
	prevTerm, ok := r.rlog.TermAt(prevIndex)
	if !ok {
		// nextIndex ran past the end of our log; restart from the beginning.
		ni, prevIndex, prevTerm = 1, 0, 0
		r.nextIndex[peer] = 1
	}
	req := AppendReq{
		Term:         r.currentTerm,
		LeaderID:     r.cfg.ID,
		PrevLogIndex: prevIndex,
		PrevLogTerm:  prevTerm,
		Entries:      r.rlog.From(ni, r.cfg.MaxEntriesPerRPC),
		LeaderCommit: r.commitIndex,
	}
	reqTerm := r.currentTerm
	sentAt := time.Now()
	r.inflight[peer] = true
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		ctx, cancel := context.WithTimeout(r.ctx, r.cfg.RPCTimeout)
		resp, err := r.tr.AppendEntries(ctx, peer, req)
		cancel()
		r.mu.Lock()
		r.inflight[peer] = false
		if err == nil {
			r.onAppendResp(peer, reqTerm, sentAt, resp, time.Now())
		}
		r.mu.Unlock()
	}()
}

func (r *Raft) sendVoteLocked(peer string, req VoteReq) {
	if r.closed {
		return
	}
	reqTerm := req.Term
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		ctx, cancel := context.WithTimeout(r.ctx, r.cfg.RPCTimeout)
		resp, err := r.tr.RequestVote(ctx, peer, req)
		cancel()
		r.mu.Lock()
		if err == nil {
			r.onVoteResp(peer, reqTerm, resp, time.Now())
		}
		r.mu.Unlock()
	}()
}

// onAppendResp runs with r.mu held.
//
// The three guards below are not decoration. Without the role guard a node that
// has stepped down still advances a commit index; without the request-term
// guard a response to an RPC from an older term is processed as current; and
// without the floor on nextIndex the decrement underflows uint64. Each of those
// is an independent safety bug, and this fixture is required to have exactly
// one.
func (r *Raft) onAppendResp(peer string, reqTerm uint64, sentAt time.Time, resp AppendResp, now time.Time) {
	if resp.Term > r.currentTerm {
		r.becomeFollower(resp.Term, "", now)
		return
	}
	if r.role != RoleLeader || reqTerm != r.currentTerm || resp.Term != r.currentTerm {
		return // stale response: dropped
	}
	if !resp.Success {
		next := r.nextIndex[peer]
		switch {
		case resp.ConflictIndex > 0 && resp.ConflictIndex < next:
			r.nextIndex[peer] = resp.ConflictIndex
		case next > 1:
			r.nextIndex[peer] = next - 1
		default:
			r.nextIndex[peer] = 1
		}
		r.sendAppendLocked(peer)
		return
	}

	r.lastAckTime[peer] = now
	if sentAt.After(r.lastAckSend[peer]) {
		r.lastAckSend[peer] = sentAt
	}
	if resp.MatchIndex > r.matchIndex[peer] {
		r.matchIndex[peer] = resp.MatchIndex
	}
	if r.matchIndex[peer]+1 > r.nextIndex[peer] {
		r.nextIndex[peer] = r.matchIndex[peer] + 1
	}
	r.maybeRenewLease(now)
	r.maybeAdvanceCommit()
	// Keep streaming if the follower is still behind.
	if r.rlog.LastIndex() >= r.nextIndex[peer] {
		r.sendAppendLocked(peer)
	}
}

func (r *Raft) onVoteResp(peer string, reqTerm uint64, resp VoteResp, now time.Time) {
	if resp.Term > r.currentTerm {
		r.becomeFollower(resp.Term, "", now)
		return
	}
	if r.role != RoleCandidate || reqTerm != r.currentTerm || resp.Term != r.currentTerm {
		return
	}
	if !resp.Granted {
		return
	}
	r.votes[peer] = true
	if len(r.votes) >= r.quorum() {
		r.becomeLeader(now)
	}
}

// maybeRenewLease refreshes the read lease when a majority of peers have
// acknowledged a heartbeat recently. The renewal RULE is correct; only
// LeaseDuration is wrong. See lease.go.
func (r *Raft) maybeRenewLease(now time.Time) {
	if r.role != RoleLeader {
		return
	}
	window := 3 * r.cfg.HeartbeatPeriod
	acks := 1 // self
	for _, p := range r.peers {
		if t := r.lastAckTime[p]; !t.IsZero() && now.Sub(t) <= window {
			acks++
		}
	}
	if acks >= r.quorum() {
		r.leaseDeadline = now.Add(LeaseDuration)
	}
}

// leaseHeld reports whether this leader may answer a read from local state with
// no quorum round trip. It is recomputed on every call, never cached and never
// published: a node that has stepped down cannot serve a lease read even for a
// microsecond.
func (r *Raft) leaseHeld(now time.Time) bool {
	return r.role == RoleLeader && !r.leaseDeadline.IsZero() && now.Before(r.leaseDeadline)
}

// maybeAdvanceCommit implements the Figure 8 restriction: a leader counts
// replicas only for entries from its OWN term. Omitting the term check is the
// single most commonly botched rule in Raft and produces a second, independent
// safety bug.
func (r *Raft) maybeAdvanceCommit() {
	if r.role != RoleLeader {
		return
	}
	for n := r.rlog.LastIndex(); n > r.commitIndex; n-- {
		t, ok := r.rlog.TermAt(n)
		if !ok {
			continue
		}
		if t != r.currentTerm {
			break
		}
		count := 1
		for _, p := range r.peers {
			if r.matchIndex[p] >= n {
				count++
			}
		}
		if count >= r.quorum() {
			r.commitIndex = n
			r.applyCommitted()
			return
		}
	}
}

func (r *Raft) applyCommitted() {
	for r.lastApplied < r.commitIndex {
		idx := r.lastApplied + 1
		e, ok := r.rlog.At(idx)
		if !ok {
			return
		}
		var res any
		if len(e.Data) > 0 {
			res = r.app.Apply(e.Index, e.Term, e.Data)
		}
		r.lastApplied = idx
		if p, ok := r.waiters[idx]; ok {
			delete(r.waiters, idx)
			if p.Term == e.Term {
				p.ch <- ProposeResult{Index: idx, Term: e.Term, Result: res}
			} else {
				p.ch <- ProposeResult{Index: idx, Term: e.Term, Err: ErrOverwritten}
			}
		}
	}
}

func (r *Raft) failWaiters(indices []uint64) {
	for _, idx := range indices {
		if p, ok := r.waiters[idx]; ok {
			delete(r.waiters, idx)
			p.ch <- ProposeResult{Index: idx, Err: ErrOverwritten}
		}
	}
}

// ---------------------------------------------------------------- RPC servers

// HandleAppend processes an AppendEntries RPC.
func (r *Raft) HandleAppend(req AppendReq) AppendResp {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()

	if req.Term < r.currentTerm {
		return AppendResp{Term: r.currentTerm, Success: false}
	}
	if req.Term > r.currentTerm || r.role != RoleFollower {
		r.becomeFollower(req.Term, req.LeaderID, now)
	}
	r.leaderID = req.LeaderID
	r.resetElectionDeadline(now)

	if req.PrevLogIndex > 0 {
		term, ok := r.rlog.TermAt(req.PrevLogIndex)
		if !ok {
			return AppendResp{Term: r.currentTerm, Success: false, ConflictIndex: r.rlog.LastIndex() + 1}
		}
		if term != req.PrevLogTerm {
			ci := r.rlog.FirstIndexOfTerm(term)
			if ci == 0 {
				ci = req.PrevLogIndex
			}
			return AppendResp{Term: r.currentTerm, Success: false, ConflictIndex: ci}
		}
	}

	// Append, truncating only on a genuine term conflict. Entries already
	// present with a matching term are left alone, so a duplicated or reordered
	// AppendEntries is idempotent.
	var toAppend []Entry
	for i, e := range req.Entries {
		idx := req.PrevLogIndex + uint64(i) + 1
		if have, ok := r.rlog.TermAt(idx); ok {
			if have == e.Term {
				continue
			}
			// Safety canary. Under a correct implementation this is
			// unreachable: Log Matching guarantees a committed entry is never
			// overwritten. If it ever fires, the fixture has acquired a SECOND
			// safety defect and every downstream verdict about "the known bug"
			// is suspect, so say so loudly rather than truncating.
			if idx <= r.commitIndex {
				r.logger.Printf("level=fatal event=truncate_committed node=%s index=%d commit_index=%d term=%d leader=%q",
					r.cfg.ID, idx, r.commitIndex, r.currentTerm, req.LeaderID)
				return AppendResp{Term: r.currentTerm, Success: false}
			}
			if err := r.st.TruncateFrom(idx); err != nil {
				r.logger.Printf("level=error event=truncate err=%q", err)
				return AppendResp{Term: r.currentTerm, Success: false}
			}
			r.failWaiters(r.rlog.TruncateFrom(idx))
		}
		toAppend = req.Entries[i:]
		break
	}
	if len(toAppend) > 0 {
		normalized := make([]Entry, len(toAppend))
		for i := range toAppend {
			normalized[i] = toAppend[i]
			normalized[i].Index = req.PrevLogIndex + uint64(i) + 1
		}
		// Recompute the append offset: entries before toAppend[0] were skipped
		// as already-present matches.
		start := normalized[0].Index
		if start != r.rlog.LastIndex()+1 {
			// Should be unreachable: the loop above either matched or truncated.
			r.logger.Printf("level=error event=append_gap start=%d last=%d", start, r.rlog.LastIndex())
			return AppendResp{Term: r.currentTerm, Success: false}
		}
		if err := r.st.AppendEntries(normalized); err != nil {
			r.logger.Printf("level=error event=append_wal err=%q", err)
			return AppendResp{Term: r.currentTerm, Success: false}
		}
		r.rlog.Append(normalized...)
	}

	match := req.PrevLogIndex + uint64(len(req.Entries))
	if req.LeaderCommit > r.commitIndex {
		r.commitIndex = min64(req.LeaderCommit, match)
		r.applyCommitted()
	}
	return AppendResp{Term: r.currentTerm, Success: true, MatchIndex: match}
}

// HandleVote processes a RequestVote RPC.
func (r *Raft) HandleVote(req VoteReq) VoteResp {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()

	if req.Term < r.currentTerm {
		return VoteResp{Term: r.currentTerm, Granted: false}
	}
	if req.Term > r.currentTerm {
		r.becomeFollower(req.Term, "", now)
	}
	granted := (r.votedFor == "" || r.votedFor == req.CandidateID) &&
		r.rlog.UpToDate(req.LastLogIndex, req.LastLogTerm)
	if granted {
		r.votedFor = req.CandidateID
		r.persistStateLocked()
		r.resetElectionDeadline(now)
	}
	return VoteResp{Term: r.currentTerm, Granted: granted}
}

// ------------------------------------------------------------- client surface

// Propose appends a command to the leader's log and returns a handle that
// resolves when the command is applied, or definitively fails.
func (r *Raft) Propose(data json.RawMessage) (*Proposal, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, ErrStopped
	}
	if r.role != RoleLeader {
		return nil, ErrNotLeader
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("raft: empty command")
	}
	e := Entry{Index: r.rlog.LastIndex() + 1, Term: r.currentTerm, Data: data}
	r.rlog.Append(e)
	if err := r.st.AppendEntries([]Entry{e}); err != nil {
		r.rlog.TruncateFrom(e.Index)
		return nil, fmt.Errorf("raft: persist proposal: %w", err)
	}
	p := &Proposal{Index: e.Index, Term: e.Term, ch: make(chan ProposeResult, 1)}
	r.waiters[e.Index] = p
	for _, peer := range r.peers {
		r.sendAppendLocked(peer)
	}
	r.maybeAdvanceCommit()
	return p, nil
}

// Forget drops a proposal's waiter. The caller must call it after giving up, or
// the map grows without bound. Dropping the waiter does NOT cancel the command:
// it may still commit, which is exactly why such an outcome is indeterminate.
func (r *Raft) Forget(p *Proposal) {
	if p == nil {
		return
	}
	r.mu.Lock()
	if cur, ok := r.waiters[p.Index]; ok && cur == p {
		delete(r.waiters, p.Index)
	}
	r.mu.Unlock()
}

// ReadIndex implements the correct linearizable read path: capture the commit
// index, confirm leadership with a live majority, then wait for the state
// machine to catch up. This is the path the lease read bypasses.
func (r *Raft) ReadIndex(ctx context.Context) (uint64, error) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return 0, ErrStopped
	}
	if r.role != RoleLeader {
		r.mu.Unlock()
		return 0, ErrNotLeader
	}
	term := r.currentTerm
	idx := r.commitIndex
	// A leader may not answer a read until it has committed an entry in its own
	// term; before that its commit index may lag a previous leader's.
	if t, ok := r.rlog.TermAt(idx); !ok || t != term {
		r.mu.Unlock()
		return 0, ErrNoQuorum
	}
	start := time.Now()
	r.heartbeatDeadline = start
	for _, p := range r.peers {
		r.sendAppendLocked(p)
	}
	r.mu.Unlock()

	deadline := start.Add(2 * r.cfg.RPCTimeout)
	tick := time.NewTicker(2 * time.Millisecond)
	defer tick.Stop()
	for {
		r.mu.Lock()
		if r.role != RoleLeader || r.currentTerm != term {
			r.mu.Unlock()
			return 0, ErrNotLeader
		}
		acks := 1
		for _, p := range r.peers {
			if r.lastAckSend[p].After(start) {
				acks++
			}
		}
		ok := acks >= r.quorum()
		r.mu.Unlock()
		if ok {
			return idx, nil
		}
		if time.Now().After(deadline) {
			return 0, ErrNoQuorum
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-tick.C:
		}
	}
}

// WaitApplied blocks until the state machine has applied through index.
func (r *Raft) WaitApplied(ctx context.Context, index uint64) error {
	tick := time.NewTicker(2 * time.Millisecond)
	defer tick.Stop()
	for {
		r.mu.Lock()
		done := r.lastApplied >= index
		r.mu.Unlock()
		if done {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}

// View returns a consistent snapshot of externally observable state.
func (r *Raft) View() View {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	held := r.leaseHeld(now)
	var remaining time.Duration
	if held {
		remaining = r.leaseDeadline.Sub(now)
	}
	return View{
		ID:             r.cfg.ID,
		Role:           r.role,
		Term:           r.currentTerm,
		LeaderID:       r.leaderID,
		CommitIndex:    r.commitIndex,
		LastApplied:    r.lastApplied,
		LastLogIndex:   r.rlog.LastIndex(),
		LeaseHeld:      held,
		LeaseRemaining: remaining,
		Ticks:          r.ticks,
		Uptime:         now.Sub(r.started),
		PendingApply:   r.commitIndex - r.lastApplied,
		Waiters:        len(r.waiters),
	}
}

// LeaseHeld reports whether a local (non-quorum) read would be served right
// now. Exported for tests and for the bug demonstration.
func (r *Raft) LeaseHeld() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.leaseHeld(time.Now())
}

// LogEntries returns a copy of the whole replicated log, for test assertions.
func (r *Raft) LogEntries() []Entry {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.rlog.Entries()
}

// DrawnElectionTimeout returns the randomized timeout currently in force.
func (r *Raft) DrawnElectionTimeout() time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastDrawnTimeout
}

func min64(a, b uint64) uint64 {
	if a < b {
		return a
	}
	return b
}
