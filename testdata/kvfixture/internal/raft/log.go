package raft

// Log is the in-memory replicated log. Index 1 is the first real entry; index 0
// is the empty sentinel with term 0. There is no compaction: without an
// InstallSnapshot RPC, compaction would break catch-up for a lagging follower,
// which would be a SECOND safety bug in a fixture whose entire value rests on
// having exactly one.
type Log struct {
	entries []Entry // entries[i] has Index == uint64(i)+1
}

// NewLog returns an empty log.
func NewLog() *Log { return &Log{} }

// Restore rebuilds a log from a durable record. It panics on a non-contiguous
// sequence, because silently accepting one would corrupt every index-based
// invariant in the algorithm.
func (l *Log) Restore(entries []Entry) {
	for i, e := range entries {
		if e.Index != uint64(i)+1 {
			panic("raft: non-contiguous log restored from storage")
		}
	}
	l.entries = append(l.entries[:0], entries...)
}

// LastIndex returns the index of the final entry, or 0 for an empty log.
func (l *Log) LastIndex() uint64 { return uint64(len(l.entries)) }

// LastTerm returns the term of the final entry, or 0 for an empty log.
func (l *Log) LastTerm() uint64 {
	if len(l.entries) == 0 {
		return 0
	}
	return l.entries[len(l.entries)-1].Term
}

// TermAt returns the term of the entry at index, and whether it exists.
// Index 0 always exists with term 0.
func (l *Log) TermAt(index uint64) (uint64, bool) {
	if index == 0 {
		return 0, true
	}
	if index > uint64(len(l.entries)) {
		return 0, false
	}
	return l.entries[index-1].Term, true
}

// At returns the entry at index, and whether it exists.
func (l *Log) At(index uint64) (Entry, bool) {
	if index == 0 || index > uint64(len(l.entries)) {
		return Entry{}, false
	}
	return l.entries[index-1], true
}

// From returns up to max entries starting at index, as a fresh slice.
func (l *Log) From(index uint64, max int) []Entry {
	if index == 0 {
		index = 1
	}
	if index > uint64(len(l.entries)) {
		return nil
	}
	tail := l.entries[index-1:]
	if max > 0 && len(tail) > max {
		tail = tail[:max]
	}
	out := make([]Entry, len(tail))
	copy(out, tail)
	return out
}

// Append adds entries to the end of the log. Indices must continue the
// sequence; a violation is a programming error, not a runtime condition.
func (l *Log) Append(entries ...Entry) {
	for _, e := range entries {
		if e.Index != uint64(len(l.entries))+1 {
			panic("raft: out-of-order append")
		}
		l.entries = append(l.entries, e)
	}
}

// TruncateFrom deletes index and everything after it. It returns the indices
// that were removed so their proposers can be told, definitively, that their
// command did not execute.
func (l *Log) TruncateFrom(index uint64) []uint64 {
	if index == 0 || index > uint64(len(l.entries)) {
		return nil
	}
	removed := make([]uint64, 0, uint64(len(l.entries))-index+1)
	for i := index; i <= uint64(len(l.entries)); i++ {
		removed = append(removed, i)
	}
	l.entries = l.entries[:index-1]
	return removed
}

// FirstIndexOfTerm returns the lowest index whose entry has the given term, or
// 0 if the term does not appear. Used only for the ConflictIndex fast-backtrack
// hint.
func (l *Log) FirstIndexOfTerm(term uint64) uint64 {
	for i := range l.entries {
		if l.entries[i].Term == term {
			return l.entries[i].Index
		}
	}
	return 0
}

// UpToDate implements the election restriction of section 5.4.1: a candidate's
// log must be at least as up to date as the voter's.
func (l *Log) UpToDate(lastIndex, lastTerm uint64) bool {
	myTerm := l.LastTerm()
	if lastTerm != myTerm {
		return lastTerm > myTerm
	}
	return lastIndex >= l.LastIndex()
}

// Entries returns a copy of the whole log.
func (l *Log) Entries() []Entry {
	out := make([]Entry, len(l.entries))
	copy(out, l.entries)
	return out
}
