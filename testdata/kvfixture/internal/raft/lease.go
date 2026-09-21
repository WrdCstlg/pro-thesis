//go:build !kvfixed

package raft

import "time"

// LeaseDuration is THE INJECTED DEFECT.
//
// This is the ONLY deliberate defect in the fixture. Everything else in
// internal/raft and internal/kv is meant to be correct; if PRO-THESIS finds a
// safety violation that is not explained by this constant, that is a bug in the
// fixture and must be fixed, not celebrated.
//
// The leader grants itself a read lease so that it can answer reads from local
// state with no quorum round trip. The lease is refreshed whenever a majority
// of peers acknowledge a heartbeat -- which is the CORRECT renewal rule -- and
// the deadline is a genuine CLOCK_MONOTONIC reading, so it keeps running while
// the process is frozen. None of that is the mistake.
//
// The mistake is the number.
//
// A lease read is safe only while it is impossible for anyone else to have
// become leader since the lease was granted. That requires
//
//	LeaseDuration + maxClockDrift < ElectionTimeoutMin
//
// Here it is 5000ms against an ElectionTimeoutMin of 600ms: violated by 8.3x.
// The author reasoned "the spec says a five-second lease, and five seconds is
// nice and long, so reads stay fast even if a heartbeat round is slow." That
// reasoning never mentions the election timeout, which is exactly the term it
// needed to mention.
//
// Two consequences, both required by spec section 5:
//
//  1. net.partition(minority) -- an isolated leader keeps serving local reads
//     for the remaining lease, which outlives its own replacement's election by
//     roughly 4 seconds. It cannot learn about the new term, so the window is
//     deterministic, not racy.
//
//  2. proc.pause -- a frozen leader that wakes up BEFORE the deadline it was
//     granted still believes it holds the lease, and serves a read from a state
//     machine missing every write committed in the new term. Note that the
//     monotonic clock keeps advancing during the freeze, so the pause must be
//     SHORTER than the lease for this to fire. That is the honest behaviour of
//     the mechanism spec section 5 describes, and it is why the reference
//     schedule's 6.9s pause cannot trigger this bug (logged as OQ-010).
//
// THE FIX is one line: see lease_fixed.go (build tag `kvfixed`), which sets the
// lease to 300ms, comfortably inside ElectionTimeoutMin.
const LeaseDuration = 5 * time.Second

// Variant identifies which lease constant was compiled in. It is derived from
// the build tag rather than from configuration, so an image mislabelled at
// build time still reports the truth at GET /status.
const Variant = "buggy"
