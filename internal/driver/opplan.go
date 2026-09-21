package driver

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
)

// Origin values for Plan.Origin. PlanOp.AtMS is measured from one of them.
const (
	// OriginDrive means the offsets are measured from the DRIVE phase marker
	// the harness wrote into the merged history. This is the anchor that makes
	// an op offset comparable with a fault window, since both are then relative
	// to the same instant.
	OriginDrive = "drive"

	// OriginFirstOp means the history carried no DRIVE marker, so offsets are
	// measured from the earliest operation's own invoke. A plan with this
	// origin still paces correctly RELATIVE to itself; what it cannot promise
	// is that op N lands inside a fault window measured from DRIVE.
	OriginFirstOp = "first_op"
)

// PlanOp is one operation of an explicit operation trace.
//
// It carries exactly what is needed to re-issue the operation and to recognise
// it afterwards, and nothing that would be a lie on replay.
//
// # Why op ids are carried rather than regenerated
//
// The verdict's witness names OP IDS (directive 4.6: `"witness": {"op_ids":
// [8891, 8903], "key": "k/42"}`). A shrunk repro whose replayed history
// renumbers its operations cannot be cross-checked against that witness at all,
// so "did this candidate reproduce THE SAME violation" becomes unanswerable,
// and an unanswerable question is answered optimistically by anyone in a hurry.
// ddmin therefore REMOVES operations; it never renumbers them.
type PlanOp struct {
	// OpID is the history op_id, carried verbatim from the original run.
	OpID int64 `json:"op_id"`

	// Process is the logical client. The driver must execute this operation on
	// the same logical process it originally ran on, because a process is a
	// sequential thread with exactly one operation in flight, and that is what
	// makes a history tractable for a linearizability checker. Re-assigning
	// operations between processes changes the concurrency structure, which
	// changes which histories are linearizable: a shrink that changes the bug.
	Process int `json:"process"`

	// F is the operation function, as it appears in the history's `f` member:
	// "read", "write", "txn", "admin" for the reference fixture. The set is NOT
	// closed here. The directive's history schema does not freeze it, and a
	// third-party driver's vocabulary is its own business; this package moves
	// the token and the driver interprets it.
	F string `json:"f"`

	// Key is the operated-on key, when the function has one.
	Key string `json:"key,omitempty"`

	// Value is the history record's `value` member, VERBATIM, as opaque JSON.
	//
	// It is raw rather than typed for the same reason MergeHistory moves raw
	// lines: the shape is the driver's, not this package's. A write's value is
	// an integer for the reference fixture and a transaction's is an array of
	// micro-operations, and a decode-then-re-encode round trip through a type
	// this package invented would silently rewrite both. Raw bytes also carry
	// integers above 2^53 without the float64 corruption an interface{} decode
	// would introduce.
	//
	// It is taken from the INVOKE record, never from the completion: a
	// completion records what the system DID, and replaying that instead of
	// what was ASKED changes the workload. For a transaction the two genuinely
	// differ: the completion carries the executed ops with their read results
	// filled in.
	Value json.RawMessage `json:"value,omitempty"`

	// ReadMode is the consistency mode a read asked for, from the driver's
	// namespaced meta ("lease" or "linearizable" for the reference fixture).
	// Empty means the driver's own default.
	//
	// It is carried because the reference fixture's defect lives on ONE of the
	// two read paths: a lease read is served locally with no quorum round trip
	// and can be stale, a linearizable read takes the read-index path and is
	// correct. Replaying a linearizable read as a lease read, or the reverse,
	// reproduces a different thing.
	ReadMode string `json:"read_mode,omitempty"`

	// Target is the client-plane target this operation was sent to, as one of
	// Plan.Targets. It is a POSITION, not an address: see Plan.Targets.
	Target string `json:"target,omitempty"`

	// AtMS is the operation's offset in milliseconds from Plan.Origin.
	//
	// This is not decoration. ddmin reduces 20,000 operations to a handful, and
	// a handful of operations retire in milliseconds: long before a fault
	// window that opens at @3000ms. A trace replayed as fast as it can be
	// issued therefore reproduces NOTHING, and the pipeline would record a
	// perfect-looking minimal repro that replays 0/3 and poisons every future
	// gate run. A cooperating driver paces against this; one that does not is
	// visible through VerifyReplay rather than assumed.
	AtMS int64 `json:"at_ms,omitempty"`

	// Pinned marks an operation the minimizer must never remove: the violation
	// witness's own op ids.
	//
	// They are the evidence. A candidate that dropped them could still fail,
	// and there would be no way to tell whether it failed for the same reason,
	// because the identity check has nothing left to match on. Subset ENFORCES
	// this rather than trusting a caller to remember it.
	//
	// The driver ignores this member entirely; it is a fact about the trace,
	// not an instruction to the process executing it.
	Pinned bool `json:"pinned,omitempty"`
}

// HasOperations reports whether this is an operation plan rather than a profile
// plan.
//
// This is the discriminator that makes D-021's dual transport safe. The driver
// supervisor ALWAYS exports PROTHESIS_PLAN_PATH, so a driver that took the mere
// existence of the variable as "replay a trace" would find a profile plan, read
// zero operations, and drive nothing, while still producing a history that
// looks clean. The trace's PRESENCE is the signal, never the path's.
func (p *Plan) HasOperations() bool { return len(p.Operations) > 0 }

// OpIDs returns every op id in the trace, in plan order.
func (p *Plan) OpIDs() []int64 {
	out := make([]int64, len(p.Operations))
	for i, op := range p.Operations {
		out[i] = op.OpID
	}
	return out
}

// PinnedOpIDs returns the op ids the minimizer must keep.
func (p *Plan) PinnedOpIDs() []int64 {
	out := []int64{}
	for _, op := range p.Operations {
		if op.Pinned {
			out = append(out, op.OpID)
		}
	}
	return out
}

// RemovableOpIDs returns the op ids ddmin is allowed to remove: every operation
// that is not pinned, in plan order.
//
// This is the input to delta debugging. Handing ddmin the full op id list and
// asking it to remember the pins would put the guarantee in the algorithm; it
// belongs in the data.
func (p *Plan) RemovableOpIDs() []int64 {
	out := []int64{}
	for _, op := range p.Operations {
		if !op.Pinned {
			out = append(out, op.OpID)
		}
	}
	return out
}

// Pin marks the named op ids as pinned.
//
// An op id the plan does not contain is an ERROR, never a silent no-op. A
// witness naming an operation the trace lacks means the trace was built from a
// different history, or the operation was never extractable (OQ-034: a witness
// may name an indeterminate write that has no `ok` record, and it may name one
// with no completion record at all). Either way the candidate cannot be checked
// for "the same bug", and pretending the pin succeeded is how that becomes
// invisible.
func (p *Plan) Pin(ids []int64) error {
	idx := make(map[int64]int, len(p.Operations))
	for i, op := range p.Operations {
		idx[op.OpID] = i
	}
	var missing []int64
	for _, id := range ids {
		i, ok := idx[id]
		if !ok {
			missing = append(missing, id)
			continue
		}
		p.Operations[i].Pinned = true
	}
	if len(missing) > 0 {
		sort.Slice(missing, func(a, b int) bool { return missing[a] < missing[b] })
		return fmt.Errorf("driver: cannot pin op_id(s) %v: the plan has no such operation "+
			"(the witness names evidence this trace does not contain, so no candidate "+
			"built from it can be checked against the original violation)", missing)
	}
	return nil
}

// Subset returns a plan carrying only the named operations.
//
// Relative order, client assignment, op ids, offsets and Targets are all
// preserved exactly; only the membership changes. `keep` need not be sorted:
// the result is always in the parent's order, so the same set always gives the
// same plan whatever order ddmin generated it in.
//
// It REFUSES three things, each because the silent version corrupts a shrink:
//
//   - an op id the parent does not contain (a minimizer bug that would
//     otherwise quietly produce a smaller trace than it thought it had;
//   - dropping a PINNED op) the witness evidence, without which "is this the
//     same violation" cannot be answered;
//   - an empty result; a driver handed nothing drives nothing, and a world
//     that drove nothing still writes a history a checker calls clean.
func (p *Plan) Subset(keep []int64) (*Plan, error) {
	if !p.HasOperations() {
		return nil, fmt.Errorf("driver: Subset on a profile plan: it carries no operation trace")
	}
	wanted := make(map[int64]bool, len(keep))
	for _, id := range keep {
		wanted[id] = true
	}
	have := make(map[int64]bool, len(p.Operations))
	for _, op := range p.Operations {
		have[op.OpID] = true
	}
	var unknown []int64
	for _, id := range keep {
		if !have[id] {
			unknown = append(unknown, id)
		}
	}
	if len(unknown) > 0 {
		sort.Slice(unknown, func(a, b int) bool { return unknown[a] < unknown[b] })
		return nil, fmt.Errorf("driver: Subset asked to keep op_id(s) %v that are not in the plan", unknown)
	}

	out := p.Clone()
	out.Operations = make([]PlanOp, 0, len(keep))
	var dropped []int64
	for _, op := range p.Operations {
		if wanted[op.OpID] {
			out.Operations = append(out.Operations, cloneOp(op))
			continue
		}
		if op.Pinned {
			dropped = append(dropped, op.OpID)
		}
	}
	if len(dropped) > 0 {
		return nil, fmt.Errorf("driver: Subset would drop pinned op_id(s) %v: they are the "+
			"violation witness, and a candidate without them cannot be checked against the "+
			"original violation identity", dropped)
	}
	if len(out.Operations) == 0 {
		return nil, fmt.Errorf("driver: Subset produced an empty operation trace; " +
			"a driver handed no operations drives nothing and reports on a system it never touched")
	}
	return out, nil
}

// Without returns a plan with the named operations removed. It is Subset's
// complement and carries the same refusals.
func (p *Plan) Without(drop []int64) (*Plan, error) {
	remove := make(map[int64]bool, len(drop))
	for _, id := range drop {
		remove[id] = true
	}
	keep := make([]int64, 0, len(p.Operations))
	for _, op := range p.Operations {
		if !remove[op.OpID] {
			keep = append(keep, op.OpID)
		}
	}
	return p.Subset(keep)
}

// Clone returns a deep copy. Every slice is copied, including the opaque value
// bytes, so a candidate can never mutate the plan it was derived from, which
// under a parallel minimizer would be a data race between candidates and,
// worse, a silent corruption of the baseline the whole shrink is judged against.
func (p *Plan) Clone() *Plan {
	out := *p
	if p.Mix != nil {
		out.Mix = append([]MixEntry(nil), p.Mix...)
	}
	if p.Targets != nil {
		out.Targets = append([]string(nil), p.Targets...)
	}
	if p.Operations != nil {
		out.Operations = make([]PlanOp, len(p.Operations))
		for i, op := range p.Operations {
			out.Operations[i] = cloneOp(op)
		}
	}
	return &out
}

func cloneOp(op PlanOp) PlanOp {
	if op.Value != nil {
		op.Value = append(json.RawMessage(nil), op.Value...)
	}
	return op
}

// Fingerprint is a stable identity for a plan's bytes, "sha256:"-prefixed.
//
// It exists so a minimizer can de-duplicate candidates it has already executed:
// at ~30s a world, re-running an identical trace is a measurable fraction of the
// budget D-029 caps.
//
// IT IS NOT A WORLD HASH and must never be used as one. `world_hash` covers the
// I2 world tuple; a plan is a derived artifact of the driver profile plus a
// reduction, and pkg/schema deliberately excludes it. Nothing in this tree feeds
// this value into a digest that a lock, a verdict or a `.thesis` file carries.
func (p *Plan) Fingerprint() (string, error) {
	b, err := p.Marshal()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// Validate checks the operation trace.
//
// It deliberately says NOTHING about the profile members. Those have been
// written by Phase 1 since before this existed and adding rejections to a path
// that already works is how a format change becomes an incident. Everything
// below is a rule the ADDITIVE members must satisfy for a reduction to be sound.
func (p *Plan) Validate() error {
	switch p.Origin {
	case "", OriginDrive, OriginFirstOp:
	default:
		return fmt.Errorf("driver: plan origin is %q, want %q or %q", p.Origin, OriginDrive, OriginFirstOp)
	}
	if p.Operations == nil {
		if len(p.Targets) > 0 {
			return fmt.Errorf("driver: plan carries %d target(s) but no operation trace; "+
				"targets are positions for operations to name and mean nothing without them",
				len(p.Targets))
		}
		return nil
	}
	if len(p.Operations) == 0 {
		return fmt.Errorf("driver: plan carries an EMPTY operation trace. " +
			"Absent means \"generate from the profile\"; empty would mean \"execute nothing\", " +
			"and a driver that executes nothing still writes a history a checker calls clean")
	}

	targets := make(map[string]bool, len(p.Targets))
	for i, t := range p.Targets {
		if t == "" {
			return fmt.Errorf("driver: plan targets[%d] is empty", i)
		}
		if targets[t] {
			return fmt.Errorf("driver: plan targets[%d] %q is a duplicate; positions must be distinct", i, t)
		}
		targets[t] = true
	}

	var prev int64
	for i, op := range p.Operations {
		switch {
		case op.OpID <= 0:
			return fmt.Errorf("driver: plan operations[%d] has op_id %d; op ids are positive", i, op.OpID)
		case i > 0 && op.OpID <= prev:
			// Strictly increasing is what makes "ddmin removes operations, it
			// does not renumber them" a checkable property rather than a
			// promise, and it is also the original per-process issue order: a
			// process takes a fresh op id per operation and holds one at a time.
			return fmt.Errorf("driver: plan operations[%d] op_id %d does not follow %d; "+
				"the trace must be strictly ascending by op id, which is the original issue order",
				i, op.OpID, prev)
		}
		prev = op.OpID
		if op.Process < 0 {
			return fmt.Errorf("driver: plan operations[%d] (op_id %d) has process %d; processes are non-negative",
				i, op.OpID, op.Process)
		}
		if op.F == "" {
			return fmt.Errorf("driver: plan operations[%d] (op_id %d) has no f; "+
				"the driver would not know what to execute", i, op.OpID)
		}
		if op.Value != nil && !json.Valid(op.Value) {
			return fmt.Errorf("driver: plan operations[%d] (op_id %d) has a value that is not JSON", i, op.OpID)
		}
		if op.AtMS < 0 {
			return fmt.Errorf("driver: plan operations[%d] (op_id %d) has at_ms %d; offsets are non-negative",
				i, op.OpID, op.AtMS)
		}
		if op.Target != "" && !targets[op.Target] {
			return fmt.Errorf("driver: plan operations[%d] (op_id %d) names target %q, which is not in "+
				"the plan's target list; the driver has no position to map it onto", i, op.OpID, op.Target)
		}
	}
	return nil
}

// Processes returns the distinct logical clients the trace uses, ascending.
//
// A minimizer that removed every operation of one process leaves the others
// untouched: the surviving processes keep their identities, so the history's
// `process` values still line up with the original. The COUNT changes, which is
// what makes a shrunk trace cheap to run; the IDENTITIES do not, which is what
// keeps it comparable.
func (p *Plan) Processes() []int {
	seen := map[int]bool{}
	out := []int{}
	for _, op := range p.Operations {
		if !seen[op.Process] {
			seen[op.Process] = true
			out = append(out, op.Process)
		}
	}
	sort.Ints(out)
	return out
}

// SpanMS is the offset of the last operation, i.e. how long a faithful replay
// of this trace takes before its final operation is issued.
//
// A minimizer wants this: it is the difference between a candidate that can
// still overlap a fault window and one that cannot.
func (p *Plan) SpanMS() int64 {
	var max int64
	for _, op := range p.Operations {
		if op.AtMS > max {
			max = op.AtMS
		}
	}
	return max
}
