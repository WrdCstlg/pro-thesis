//go:build kvfixed

package raft

import "time"

// THE FIX (build tag `kvfixed`).
//
// Lease safety condition:
//
//	LeaseDuration + maxClockDrift < ElectionTimeoutMin
//	           300ms +      ~100ms <             600ms   OK
//
// A leader that loses contact with a majority now stops serving local reads
// 300ms later -- before any peer can possibly have won an election -- and falls
// back to the read-index path, which requires a live quorum and fails loudly
// with applied:false if it cannot get one.
//
// This is the target state for Phase 5's `thesis regress` and Phase 6's
// agentic repair. PRO-THESIS never reads this build tag; it observes the
// resolved image digest and the `variant` field of GET /status.
const LeaseDuration = 300 * time.Millisecond

// Variant identifies which lease constant was compiled in.
const Variant = "kvfixed"
