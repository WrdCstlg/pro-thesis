package main

import (
	"hash/fnv"
	"sort"
	"time"
)

// ---------------------------------------------------------------------------
// D-D. Wing & Gong with Lowe's optimisations
//
// Per key, over a last-write-wins register:
//
//   - events are sorted by t_ns and threaded onto a doubly linked list, so
//     lifting an operation out of the list and putting it back is O(1) and
//     backtracking costs nothing
//   - depth-first search over which pending operation linearizes next
//   - MEMOISATION on (register value, bitset of linearized ops). Without it the
//     search is exponential on histories it should handle trivially
//   - a read may linearize only if the register currently holds what it returned
//   - a write always may, and sets the value
//   - an INDETERMINATE write branches: applied, then not-applied
//
// The one place this departs from the textbook register model is the INITIAL
// STATE. Fixing it at null would report a violation for any history whose first
// read observes a value written before the recorded window began: a false
// positive produced by the checker's own assumption. So the register starts
// UNDETERMINED and the first read to linearize against an undetermined register
// establishes the value. That is strictly less constrained than assuming null,
// so it can lose a violation but can never invent one.
// ---------------------------------------------------------------------------

// outcome is what a per-key search concluded.
type outcome int

const (
	// outcomeLinearizable: a linearization was found. Sound and complete for
	// this key.
	outcomeLinearizable outcome = iota
	// outcomeViolated: the search space was exhausted with no linearization.
	// This is a WITNESSED failure, never a timeout.
	outcomeViolated
	// outcomeExhausted: the budget ran out. Nothing is concluded.
	outcomeExhausted
)

// exhaustion names which ceiling was hit.
type exhaustion string

const (
	exhaustNone   exhaustion = ""
	exhaustWall   exhaustion = "the wall-clock budget"
	exhaustStates exhaustion = "the explored-state ceiling"
)

// stats is the search's own account of what it did. Everything reported to a
// human comes from here.
type stats struct {
	States int64
	Steps  int64
	// BestDepth is the most operations placed in any linearization prefix the
	// search reached.
	BestDepth int
	// BestState is the register value at that deepest point.
	BestState Value
	// BestLinearized is the bitset of operations placed at that point.
	BestLinearized []uint64
	// BestLastWrite is the index of the write that established BestState, or -1
	// when no write did (the value was established by the first read).
	BestLastWrite int
	// Why names the exhausted ceiling, when the search did not finish.
	Why exhaustion
}

// node is one event on the linked list. A call node and its return node point at
// each other through match.
type node struct {
	prev, next *node
	match      *node
	isReturn   bool
	idx        int // index of the operation in the key's slice
	op         *operation
}

func (n *node) lift() {
	n.prev.next = n.next
	n.next.prev = n.prev
	m := n.match
	m.prev.next = m.next
	m.next.prev = m.prev
}

func (n *node) unlift() {
	m := n.match
	m.prev.next = m
	m.next.prev = m
	n.prev.next = n
	n.next.prev = n
}

// frame records a taken choice so backtracking can undo it and try the next
// alternative.
type frame struct {
	call          *node
	prevState     Value
	alt           int
	prevLastWrite int
}

// searcher runs the DFS for one key.
type searcher struct {
	ops   []*operation
	head  *node
	tail  *node
	words int

	memo *memo

	state     Value
	lastWrite int
	linear    []uint64
	depth     int
	stack     []frame

	limitStates int64
	deadline    time.Time

	st stats
}

// newSearcher threads the key's operations onto the event list.
//
// The sort is (rank, t_ns, invoke-before-return, op index):
//
//   - rank 0 are the interval-opening events of operations whose invoke record
//     was missing; they sort before everything
//   - rank 2 are the interval-closing events of INDETERMINATE operations, whose
//     interval extends to infinity; they sort after everything
//   - within rank 1, an invoke sorts before a return at the SAME timestamp. That
//     turns a zero-duration coincidence into concurrency rather than into a
//     real-time ordering, which removes constraints and so cannot invent a
//     violation. It never places a return before its own invoke.
func newSearcher(ops []*operation, limitStates int64, deadline time.Time, maxMemoBytes int64) *searcher {
	type ev struct {
		rank     int
		ts       int64
		isReturn bool
		idx      int
	}
	evs := make([]ev, 0, 2*len(ops))
	for i, op := range ops {
		inv := ev{rank: 1, ts: op.invokeNS, isReturn: false, idx: i}
		if op.openAtStart {
			inv.rank = 0
		}
		ret := ev{rank: 1, ts: op.returnNS, isReturn: true, idx: i}
		if op.openAtEnd {
			ret.rank = 2
		}
		evs = append(evs, inv, ret)
	}
	sort.SliceStable(evs, func(a, b int) bool {
		x, y := evs[a], evs[b]
		if x.rank != y.rank {
			return x.rank < y.rank
		}
		if x.rank == 1 && x.ts != y.ts {
			return x.ts < y.ts
		}
		if x.isReturn != y.isReturn {
			return !x.isReturn
		}
		return x.idx < y.idx
	})

	head := &node{}
	tail := &node{}
	head.next, tail.prev = tail, head

	calls := make([]*node, len(ops))
	rets := make([]*node, len(ops))
	cur := head
	for _, e := range evs {
		n := &node{isReturn: e.isReturn, idx: e.idx, op: ops[e.idx]}
		if e.isReturn {
			rets[e.idx] = n
		} else {
			calls[e.idx] = n
		}
		n.prev, n.next = cur, tail
		cur.next, tail.prev = n, n
		cur = n
	}
	for i := range ops {
		calls[i].match, rets[i].match = rets[i], calls[i]
	}

	words := (len(ops) + 63) / 64
	return &searcher{
		ops:         ops,
		head:        head,
		tail:        tail,
		words:       words,
		memo:        newMemo(words, maxMemoBytes),
		state:       unknownValue,
		lastWrite:   -1,
		linear:      make([]uint64, words),
		limitStates: limitStates,
		deadline:    deadline,
		st:          stats{BestDepth: -1, BestLastWrite: -1},
	}
}

// alternatives is the register model: the successor states an operation may
// produce from the current state, in the order the search tries them.
//
// An empty result means the operation cannot linearize here.
func alternatives(state Value, op *operation) []Value {
	if op.kind == opRead {
		if !state.Known() {
			// The register's value has not been determined yet, so this read
			// determines it. See the note at the top of this file.
			return []Value{op.value}
		}
		if state.Equal(op.value) {
			return []Value{state}
		}
		return nil
	}
	if !op.maybe {
		return []Value{op.value}
	}
	// D-B: an indeterminate write MUST branch. Treating it as definitely applied
	// over-constrains the model and is a false-positive generator; treating it as
	// definitely not applied is equally wrong in the other direction.
	if state.Equal(op.value) {
		return []Value{op.value} // both branches coincide
	}
	return []Value{op.value, state}
}

// applied reports whether the chosen alternative left the register holding the
// operation's own written value. It is used only for the diagnostic that names
// which write established the blocking state.
func applied(chosen Value, op *operation) bool {
	return op.kind == opWrite && chosen.Known() && chosen.Equal(op.value)
}

// run performs the search.
func (s *searcher) run() outcome {
	s.noteBest()

	entry := s.head.next
	for {
		s.st.Steps++
		// Completion is checked BEFORE the budget, so a search that finishes on
		// its last permitted state reports its result rather than an exhaustion.
		if s.head.next == s.tail {
			return outcomeLinearizable
		}
		// Checked on the first step as well as every 1024th: a deadline that has
		// already passed must stop the search before it starts, not after it has
		// spent a thousand steps it had no budget for.
		if (s.st.Steps == 1 || s.st.Steps&1023 == 0) &&
			!s.deadline.IsZero() && time.Now().After(s.deadline) {
			s.st.Why = exhaustWall
			return outcomeExhausted
		}
		if s.st.States >= s.limitStates {
			s.st.Why = exhaustStates
			return outcomeExhausted
		}

		if entry != s.tail && !entry.isReturn {
			if s.tryFrom(entry, 0) {
				entry = s.head.next
				continue
			}
			entry = entry.next
			continue
		}

		// Either the event list ran out before every operation was placed, or the
		// head is the RETURN of an operation that was never linearized, which is
		// impossible, since an operation must linearize before it returns. Undo
		// the last choice.
		if len(s.stack) == 0 {
			return outcomeViolated
		}
		f := s.stack[len(s.stack)-1]
		s.stack = s.stack[:len(s.stack)-1]
		f.call.unlift()
		s.state = f.prevState
		s.lastWrite = f.prevLastWrite
		clearBit(s.linear, f.call.idx)
		s.depth--

		if s.tryFrom(f.call, f.alt+1) {
			entry = s.head.next
			continue
		}
		entry = f.call.next
	}
}

// tryFrom attempts the alternatives of call starting at index from, taking the
// first one the memo has not already explored. It reports whether it advanced.
func (s *searcher) tryFrom(call *node, from int) bool {
	alts := alternatives(s.state, call.op)
	for i := from; i < len(alts); i++ {
		next := alts[i]
		setBit(s.linear, call.idx)
		if s.memo.insert(next, s.linear) {
			s.stack = append(s.stack, frame{
				call:          call,
				prevState:     s.state,
				alt:           i,
				prevLastWrite: s.lastWrite,
			})
			s.state = next
			if applied(next, call.op) {
				s.lastWrite = call.idx
			}
			s.depth++
			call.lift()
			s.st.States++
			s.noteBest()
			return true
		}
		clearBit(s.linear, call.idx)
	}
	clearBit(s.linear, call.idx)
	return false
}

// noteBest snapshots the deepest partial linearization reached. It is the
// evidence the witness is built from.
func (s *searcher) noteBest() {
	if s.depth <= s.st.BestDepth {
		return
	}
	s.st.BestDepth = s.depth
	s.st.BestState = s.state
	if len(s.st.BestLinearized) != len(s.linear) {
		s.st.BestLinearized = make([]uint64, len(s.linear))
	}
	copy(s.st.BestLinearized, s.linear)
	s.st.BestLastWrite = s.lastWrite
}

// ---------------------------------------------------------------------------
// bitset helpers
// ---------------------------------------------------------------------------

func setBit(bits []uint64, i int)   { bits[i>>6] |= 1 << uint(i&63) }
func clearBit(bits []uint64, i int) { bits[i>>6] &^= 1 << uint(i&63) }
func testBit(bits []uint64, i int) bool {
	if i>>6 >= len(bits) {
		return false
	}
	return bits[i>>6]&(1<<uint(i&63)) != 0
}

func equalBits(a, b []uint64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// memoisation
// ---------------------------------------------------------------------------

type memoEntry struct {
	state Value
	bits  []uint64
}

// memo remembers the (register value, linearized-set) pairs already explored.
//
// It is bounded, because a bitset copy per state over a long history would
// otherwise consume unbounded memory. When the bound is reached the table stops
// RECORDING but keeps reporting "not seen", so a branch is never pruned on the
// strength of an exploration that did not happen: pruning an unexplored branch
// could turn a linearizable history into a reported violation, which is the one
// outcome this program must never produce.
type memo struct {
	m          map[uint64][]memoEntry
	words      int
	entries    int
	maxEntries int
}

func newMemo(words int, maxBytes int64) *memo {
	per := int64(words*8) + 64
	max := maxBytes / per
	if max < 1024 {
		max = 1024
	}
	if max > 1<<30 { // stays inside int on every platform Go supports
		max = 1 << 30
	}
	return &memo{m: make(map[uint64][]memoEntry, 1024), words: words, maxEntries: int(max)}
}

// insert reports whether the pair is new and should therefore be explored.
func (mm *memo) insert(state Value, bits []uint64) bool {
	h := hashState(state, bits)
	bucket := mm.m[h]
	for i := range bucket {
		if bucket[i].state == state && equalBits(bucket[i].bits, bits) {
			return false
		}
	}
	if mm.entries >= mm.maxEntries {
		return true
	}
	cp := make([]uint64, len(bits))
	copy(cp, bits)
	mm.m[h] = append(bucket, memoEntry{state: state, bits: cp})
	mm.entries++
	return true
}

func hashState(state Value, bits []uint64) uint64 {
	h := fnv.New64a()
	if state.Known() {
		_, _ = h.Write([]byte{1})
		_, _ = h.Write([]byte(state.canon))
	} else {
		_, _ = h.Write([]byte{0})
	}
	var buf [8]byte
	for _, w := range bits {
		for i := 0; i < 8; i++ {
			buf[i] = byte(w >> uint(8*i))
		}
		_, _ = h.Write(buf[:])
	}
	return h.Sum64()
}
