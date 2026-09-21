package llm

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// ---------------------------------------------------------------------------
// The audit trail
//
// An LLM-proposed schedule stream is not seed-reproducible (OQ-069): --seed
// pins the world seeds and the executor, but the proposal text comes from an
// external model server. This file is therefore the only record of WHY a
// world carried the schedule it did, and it records every attempt: the
// rejected ones included, because a rejection that left no trace is a retry
// nobody can account for.
//
// It is deliberately NOT a frozen contract: no `schema:` line, no version.
// It sits beside search.json in the run directory and is written for a human
// auditing a run.
// ---------------------------------------------------------------------------

// ProposalsFileName is the recording's file name within the run directory.
const ProposalsFileName = "llm-proposals.jsonl"

// proposalRecord is one proposal attempt, accepted or not.
type proposalRecord struct {
	Ordinal  int      `json:"ordinal"`
	Attempt  int      `json:"attempt"` // 1-based model call number within this ordinal
	Prompt   string   `json:"prompt"`
	Response string   `json:"response"`
	Schedule []string `json:"schedule"`
	// ValidatorError carries the rejection cause (parse, policy or transport)
	// verbatim. Empty exactly when Accepted.
	ValidatorError string `json:"validator_error"`
	Accepted       bool   `json:"accepted"`
}

// auditLog appends proposal records to the run directory.
type auditLog struct {
	path string
	mu   sync.Mutex
}

func newAuditLog(dir string) *auditLog {
	return &auditLog{path: filepath.Join(dir, ProposalsFileName)}
}

// append writes one line. A write failure is returned to the caller, which
// logs it loudly; it does not fail the proposal: the recording must not
// change what the search executes, only account for it.
func (r *auditLog) append(rec proposalRecord) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if rec.Schedule == nil {
		rec.Schedule = []string{}
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(r.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(b, '\n'))
	return err
}
