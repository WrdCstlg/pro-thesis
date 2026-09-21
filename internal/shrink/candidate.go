package shrink

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// The execution seam
//
// Everything expensive happens behind this interface. That is what lets the
// ddmin algorithm, the confirmation asymmetry, the budget and the window
// narrowing all be pinned by tests that never start a container, and it keeps
// the Docker bridge-network ceiling (D-043: ~24 networks, 2 per fixture project)
// under the control of the executor, which is the only component that knows how
// many it may consume.
// ---------------------------------------------------------------------------

// Candidate is one reduced world to execute.
type Candidate struct {
	// Faults is the candidate's PLANNED schedule as canonical fault strings, in
	// the KIND(TARGET[, PARAMS])@start..end grammar. An empty slice is a
	// meaningful candidate: "does the workload alone reproduce this?".
	Faults []string

	// Ops names the op ids to KEEP, drawn from the violating world's history, or
	// nil for "drive the whole workload".
	//
	// The contract with the driver, which is the {plan_path} seam D-021 reserved
	// to close OQ-012: the driver regenerates its workload from the world seed
	// exactly as it did originally and issues only these ops, in this order. Ids
	// are enough because the workload is a function of the seed; the plan does
	// not have to restate what each op was.
	//
	// A driver that honours neither {plan_path} nor PROTHESIS_PLAN_PATH cannot
	// serve a non-nil Ops. That is not silently degraded: Options.OpPlan is the
	// switch, and op shrinking is REPORTED as not attempted rather than skipped
	// invisibly.
	Ops []int64
}

// key is the memoization key for a candidate. Faults are joined in the order
// given; the fault stage always builds them in the original schedule's order, so
// two structurally identical candidates share a key.
func (c Candidate) key() string {
	var b strings.Builder
	b.WriteString("f:")
	for i, f := range c.Faults {
		if i > 0 {
			b.WriteByte('\x1f')
		}
		b.WriteString(f)
	}
	if c.Ops != nil {
		b.WriteString("\x1eo:")
		for i, op := range c.Ops {
			if i > 0 {
				b.WriteByte('\x1f')
			}
			b.WriteString(strconv.FormatInt(op, 10))
		}
	}
	return b.String()
}

// String renders a candidate compactly for logs.
func (c Candidate) String() string {
	ops := "all ops"
	if c.Ops != nil {
		ops = fmt.Sprintf("%d op(s)", len(c.Ops))
	}
	return fmt.Sprintf("%d fault(s), %s", len(c.Faults), ops)
}

// Attempt is one execution of one candidate, as the executor observed it.
type Attempt struct {
	// Reached reports whether the world actually ran far enough for its oracles
	// to mean anything. False is UNKNOWN, never "did not reproduce": a world
	// whose topology failed to boot is not evidence that a fault was
	// unnecessary, and treating it as one is how ddmin keeps the wrong faults.
	Reached bool

	// Observed is every violation the world produced, not only the matching one.
	// The non-matching ones are what make a switched bug visible in the report
	// instead of invisible in a rejection count.
	Observed []Identity

	// WorldPath is the .thesis file this attempt executed, when the executor
	// wrote one. It is what Result.MinimalRepro names.
	WorldPath string

	// Duration is the wall cost, for the budget report.
	Duration time.Duration

	// Err is a harness or executor failure. A non-nil Err is UNKNOWN regardless
	// of Observed.
	Err error

	// Note is free text the executor wants carried into the report.
	Note string
}

// Signal is the four-valued judgement of one attempt. It is deliberately not a
// bool: "did not reproduce" and "could not tell" demand different responses, and
// collapsing them is how a shrink learns from noise.
type Signal int

const (
	// SignalUnknown: the execution could not be judged. Not evidence either way.
	SignalUnknown Signal = iota
	// SignalReproduced: an observed violation matched the original identity.
	SignalReproduced
	// SignalDifferent: the world failed, but with a violation that is NOT the
	// original. RULE 1: this is a rejection, and a reportable divergence.
	SignalDifferent
	// SignalClean: the world ran and no oracle reported a violation.
	SignalClean
)

// String renders the signal.
func (s Signal) String() string {
	switch s {
	case SignalUnknown:
		return "unknown"
	case SignalReproduced:
		return "reproduced"
	case SignalDifferent:
		return "different_violation"
	case SignalClean:
		return "clean"
	}
	return fmt.Sprintf("Signal(%d)", int(s))
}

// Classify judges one attempt against the original violation.
//
// It returns the signal, the matching identity when there is one (and otherwise
// the divergent identities), and a human reason.
func (a Attempt) Classify(orig Identity, s Strictness) (Signal, []Identity, string) {
	if a.Err != nil {
		return SignalUnknown, nil, "execution failed: " + a.Err.Error()
	}
	if !a.Reached {
		return SignalUnknown, nil, "the world did not reach a state its oracles could judge"
	}
	var diverged []Identity
	var reasons []string
	for _, got := range a.Observed {
		ok, why := s.Match(orig, got)
		if ok {
			return SignalReproduced, []Identity{got}, ""
		}
		diverged = append(diverged, got)
		reasons = append(reasons, fmt.Sprintf("%s: %s", got.Oracle, why))
	}
	if len(diverged) == 0 {
		return SignalClean, nil, "no oracle reported a violation"
	}
	return SignalDifferent, diverged, "a DIFFERENT violation fired (" + strings.Join(reasons, "; ") + ")"
}

// Executor runs one candidate world and reports what happened.
//
// It is the caller's job to satisfy the contract that makes shrinking sound:
// the candidate must be executed against the SAME system under test, the SAME
// topology variant and the SAME world seed as the violating world, differing
// only in the fault schedule and the op plan. A candidate executed against a
// different build is not a reduction of anything (OQ-011 is why the world file
// carries `sut` at all).
type Executor interface {
	Execute(ctx context.Context, c Candidate) (Attempt, error)
}

// BatchExecutor runs several INDEPENDENT candidates at once.
//
// ddmin's subsets within one round are independent by construction, which is why
// they can be batched at all. An executor that implements this is expected to be
// internal/control's ParallelRunner (D-043), which is also what enforces the
// bridge-network budget: this package never decides how many worlds may exist at
// once, it only says how many it would like.
//
// The returned slice must be the same length as cs and in the same order. A
// per-candidate failure belongs in that candidate's Attempt.Err; the returned
// error is for a failure of the batch itself.
type BatchExecutor interface {
	Executor
	ExecuteBatch(ctx context.Context, cs []Candidate) ([]Attempt, error)
}

// ExecutorFunc adapts a function to Executor.
type ExecutorFunc func(ctx context.Context, c Candidate) (Attempt, error)

// Execute implements Executor.
func (f ExecutorFunc) Execute(ctx context.Context, c Candidate) (Attempt, error) { return f(ctx, c) }
