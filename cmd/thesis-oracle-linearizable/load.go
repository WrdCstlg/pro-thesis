package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"time"

	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// ---------------------------------------------------------------------------
// Loading a history into a checkable operation set
//
// Three things happen here, and the order matters:
//
//  1. records are paired into operations by op_id
//  2. the PER-KEY DECOMPOSITION PRECONDITION is checked (brief D-A). Nothing is
//     partitioned before it passes.
//  3. ok / fail / info are applied (brief D-B)
//
// Every refusal in this file is a *refusal*, reported as INCONCLUSIVE. None of
// them is ever a violation and none of them is ever silently absorbed.
// ---------------------------------------------------------------------------

// refusal is a reason the checker will not draw a conclusion.
type refusal struct{ msg string }

func (e *refusal) Error() string { return e.msg }

func refuse(format string, a ...any) error { return &refusal{msg: fmt.Sprintf(format, a...)} }

// Operation names this checker models. Anything else makes the history
// uninterpretable as a last-write-wins register history.
const (
	opNameRead  = "read"
	opNameWrite = "write"
)

type opKind int

const (
	opRead opKind = iota
	opWrite
)

func (k opKind) String() string {
	if k == opRead {
		return "read"
	}
	return "write"
}

// operation is one checkable single-key register operation.
type operation struct {
	// id is the history op_id, which is what the witness reports.
	id int64
	// process is the logical client, when the history recorded one.
	process    int64
	hasProcess bool

	kind opKind
	key  string
	// value is the value a read RETURNED or a write WROTE.
	value Value

	// invokeNS / returnNS bound the operation's real-time interval.
	invokeNS int64
	returnNS int64
	// openAtStart means no invoke record was found, so the interval is treated
	// as opening before every other event. Widening an interval can only remove
	// constraints, so this can lose a violation but can never invent one.
	openAtStart bool
	// openAtEnd means the operation has no DETERMINATE completion, so its
	// interval extends to infinity. See the note in buildOperations.
	openAtEnd bool
	// maybe marks an indeterminate write: the search must explore both the
	// applied and the not-applied branch.
	maybe bool

	invokeLine, completeLine int
}

// describe renders the operation for an explanation.
func (o *operation) describe() string {
	if o.hasProcess {
		return fmt.Sprintf("op %d (process %d, %s)", o.id, o.process, o.kind)
	}
	return fmt.Sprintf("op %d (%s)", o.id, o.kind)
}

// counts is the honest account of what was kept and what was dropped. It is
// reported in EVERY explanation, including `ok`, because "checked and satisfied"
// and "nothing was checked" must never be confusable.
type counts struct {
	Records     int // operation records read
	Markers     int // phase / note markers skipped
	Operations  int // op_ids seen
	DetReads    int // ok reads with a recorded value: the evidence
	DetWrites   int // ok writes
	MaybeWrites int // info / never-completed writes, explored on both branches
	FailedOps   int // fail records, dropped with their invokes
	InfoReads   int // indeterminate reads, dropped
	ValuelessOK int // ok reads with no recorded value, dropped
	OrphanEnds  int // completions with no invoke, opened at the start
	ClampedEnds int // completions timestamped before their own invoke
}

// loaded is the result of a successful load.
type loaded struct {
	Ops   []*operation
	ByKey map[string][]*operation
	Keys  []string
	C     counts
}

// rawOp is an op_id's invoke and completion records.
type rawOp struct {
	id           int64
	invoke       *schema.HistoryEntry
	invokeLine   int
	complete     *schema.HistoryEntry
	completeLine int
}

// loadHistory reads a JSONL history and returns the checkable operation set, or
// a *refusal explaining why no conclusion can be drawn.
func loadHistory(path string, deadline time.Time) (*loaded, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, refuse("cannot open the history at %s: %v", path, err)
	}
	defer func() { _ = f.Close() }()
	return readHistory(f, path, deadline)
}

// readHistory is loadHistory's testable core.
func readHistory(r io.Reader, path string, deadline time.Time) (*loaded, error) {
	hr := schema.NewHistoryReader(r)
	raw := make(map[int64]*rawOp, 1024)
	order := make([]int64, 0, 1024)
	var c counts

	for n := 0; ; n++ {
		if n%4096 == 0 && !deadline.IsZero() && time.Now().After(deadline) {
			return nil, refuse("the wall-clock budget expired after reading %d line(s) of %s; "+
				"no conclusion is drawn from a partly-read history", hr.LineNo(), path)
		}
		e, err := hr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			var le *schema.HistoryLineError
			if errors.As(err, &le) {
				// A line that did not decode may be the completion record for an
				// operation. Continuing would check a history that is not the one
				// the system produced.
				return nil, refuse("line %d of %s did not decode (%v); a dropped line may be an "+
					"operation's completion record, so the history cannot be checked as read",
					le.Line, path, le.Err)
			}
			return nil, refuse("reading %s stopped at line %d: %v", path, hr.LineNo(), err)
		}

		// The union is enforced BEFORE the record is classified, because the
		// discriminator is not self-checking: RecordKind reads `event` alone, so
		// an OPERATION record carrying a stray event is classified as a marker
		// and skipped. That deletion is silent and it moves the verdict in both
		// directions: hiding the completion of a stale read turns a violation
		// into `ok`, and hiding one half of a transaction leaves a remainder
		// that admits no linearization and is reported as a WITNESSED violation
		// of a history that never happened.
		//
		// HistoryEntry.Validate is the union's own rule and was written for
		// exactly this; the read path simply did not call it. Refusing here is
		// the only outcome that is honest in both directions: a record we cannot
		// classify is not evidence of a violation and is not evidence of its
		// absence. See OQ-058.
		if verr := e.Validate(); verr != nil {
			return nil, refuse("line %d of %s is not a valid history record: %v. A record carrying "+
				"an \"event\" field is a MARKER and is skipped, so an operation record that also "+
				"carries one is deleted from the history without trace — which can hide a real "+
				"violation or manufacture a false one. This checker refuses the history rather "+
				"than checking a version of it the system never produced",
				hr.LineNo(), path, verr)
		}

		if e.RecordKind() == schema.RecordMarker {
			c.Markers++
			continue
		}
		c.Records++

		if e.OpID == nil {
			return nil, refuse("line %d of %s (type %q, f %q) carries no op_id. This checker pairs "+
				"invokes with completions by op_id and reports op_ids as its witness; it will not "+
				"guess a pairing from the process field",
				hr.LineNo(), path, e.Type, e.F)
		}
		id := *e.OpID
		ro := raw[id]
		if ro == nil {
			ro = &rawOp{id: id}
			raw[id] = ro
			order = append(order, id)
		}
		cp := *e
		switch e.Type {
		case schema.HistoryInvoke:
			if ro.invoke != nil {
				return nil, refuse("op_id %d has two invoke records (lines %d and %d of %s); "+
					"an op_id that names two operations makes every interval in the history ambiguous",
					id, ro.invokeLine, hr.LineNo(), path)
			}
			ro.invoke, ro.invokeLine = &cp, hr.LineNo()
		case schema.HistoryOK, schema.HistoryFail, schema.HistoryInfo:
			if ro.complete != nil {
				return nil, refuse("op_id %d has two completion records (lines %d and %d of %s)",
					id, ro.completeLine, hr.LineNo(), path)
			}
			ro.complete, ro.completeLine = &cp, hr.LineNo()
		default:
			return nil, refuse("line %d of %s has unknown record type %q (want one of %v)",
				hr.LineNo(), path, e.Type, schema.AllHistoryTypes)
		}
	}

	c.Operations = len(order)

	// Guard 2 from the brief's failure ranking: a checker handed nothing has
	// checked nothing.
	if c.Records == 0 {
		return nil, refuse("%s contains no operation records (%d marker record(s) only). "+
			"An empty history is INCONCLUSIVE, never ok: a checker handed nothing has checked nothing",
			path, c.Markers)
	}

	if err := checkPrecondition(raw, order); err != nil {
		return nil, err
	}
	return buildOperations(raw, order, path, c)
}

// ---------------------------------------------------------------------------
// D-A. The per-key decomposition precondition
// ---------------------------------------------------------------------------

// opShape is what the precondition scan learns about one operation.
type opShape struct {
	// Keys are the distinct keys the operation names, in first-seen order.
	Keys []string
	// MicroOps counts the recognised micro-operations in the operation's value
	// list. A non-zero count means the record describes a TRANSACTION, which is
	// what turns "this checker does not model that operation" into "you need a
	// transactional checker".
	MicroOps int
}

// shape returns the keys an operation names and whether it is transactional.
//
// It looks at the top-level `key` AND at any operation list carried in `value`,
// which is how the fixture's `txn` records its read key and its (different)
// write key. A checker that looked only at the top-level field would see a txn
// as key-less rather than as multi-key, and would report the weaker of the two
// refusals.
//
// Two encodings of a micro-operation are recognised:
//
//	["r","k/3",null]          Elle's read/write-register convention, which is
//	["w","k/7",42]            what the fixture emits on the wire
//	{"f":"r","key":"k/3"}     the object form
//
// Anything else contributes no keys, which leaves the operation key-less and
// therefore refused, never silently assigned to one key.
func (ro *rawOp) shape() (opShape, error) {
	var sh opShape
	seen := make(map[string]bool, 4)
	add := func(k string) {
		if !seen[k] {
			seen[k] = true
			sh.Keys = append(sh.Keys, k)
		}
	}

	var top *string
	for _, e := range []*schema.HistoryEntry{ro.invoke, ro.complete} {
		if e == nil || e.Key == nil {
			continue
		}
		if top != nil && *top != *e.Key {
			return sh, refuse("op_id %d names key %q on its invoke and key %q on its completion; "+
				"an operation whose key is ambiguous cannot be placed in any key's sub-history",
				ro.id, *top, *e.Key)
		}
		k := *e.Key
		top = &k
	}
	if top != nil {
		add(*top)
	}

	for _, e := range []*schema.HistoryEntry{ro.invoke, ro.complete} {
		if e == nil || len(e.Value) == 0 {
			continue
		}
		var list []json.RawMessage
		if err := json.Unmarshal(e.Value, &list); err != nil {
			continue // a scalar value: not an operation list
		}
		n := 0
		for _, item := range list {
			if k, ok := microOpKey(item); ok {
				add(k)
				n++
			}
		}
		if n > sh.MicroOps {
			sh.MicroOps = n
		}
	}
	return sh, nil
}

// microOpKey extracts the key from one micro-operation in either encoding.
func microOpKey(item json.RawMessage) (string, bool) {
	var tuple []json.RawMessage
	if err := json.Unmarshal(item, &tuple); err == nil {
		// Elle's convention: [f, key, value].
		if len(tuple) >= 2 {
			var f, k string
			if json.Unmarshal(tuple[0], &f) == nil && json.Unmarshal(tuple[1], &k) == nil {
				return k, true
			}
		}
		return "", false
	}
	var obj struct {
		Key *string `json:"key"`
	}
	if err := json.Unmarshal(item, &obj); err == nil && obj.Key != nil {
		return *obj.Key, true
	}
	return "", false
}

// opName returns the operation name, requiring the invoke and the completion to
// agree.
func (ro *rawOp) opName() (string, error) {
	var name string
	for _, e := range []*schema.HistoryEntry{ro.invoke, ro.complete} {
		if e == nil || e.F == "" {
			continue
		}
		if name != "" && name != e.F {
			return "", refuse("op_id %d is %q on its invoke and %q on its completion", ro.id, name, e.F)
		}
		name = e.F
	}
	if name == "" {
		return "", refuse("op_id %d has no operation name on either record", ro.id)
	}
	return name, nil
}

// checkPrecondition enforces the locality theorem's precondition BEFORE any
// partitioning happens.
//
// Herlihy & Wing (1990): a history is linearizable iff every object's
// subhistory is linearizable. Checking each key independently is therefore
// exact: but only for a history in which every operation belongs to exactly
// one object. A history containing a transaction over two keys does not satisfy
// the hypothesis, and per-key decomposition would silently ACCEPT histories
// that admit no serial order. That is a false PASS in the checker the product is
// sold on, so it is refused outright.
func checkPrecondition(raw map[int64]*rawOp, order []int64) error {
	type offender struct {
		id    int64
		name  string
		shape opShape
		found bool
	}
	var multi, other, keyless offender

	for _, id := range order {
		ro := raw[id]
		name, err := ro.opName()
		if err != nil {
			return err
		}
		sh, err := ro.shape()
		if err != nil {
			return err
		}
		if len(sh.Keys) > 1 && !multi.found {
			multi = offender{id, name, sh, true}
		}
		if name != opNameRead && name != opNameWrite && !other.found {
			other = offender{id, name, sh, true}
		}
		if len(sh.Keys) == 0 && !keyless.found {
			keyless = offender{id, name, sh, true}
		}
	}

	if multi.found {
		return refuse("op_id %d (%q) touches %d keys (%s), so this history does not satisfy the "+
			"precondition of the Herlihy & Wing locality theorem and per-key decomposition is "+
			"UNSOUND for it. This checker will not partition a history it has not proven "+
			"single-key: doing so would silently accept histories that admit no serial order, "+
			"which is a false PASS in the checker the product is sold on. A transactional "+
			"(Elle-style) checker is required for %q operations. The fixture's gate and soak "+
			"driver profiles are 20%% multi-key txn and are affected; a single-key read/write "+
			"profile is checkable.",
			multi.id, multi.name, len(multi.shape.Keys), joinQuoted(multi.shape.Keys), multi.name)
	}
	if other.found {
		msg := fmt.Sprintf("op_id %d has operation name %q, which this checker does not model. It "+
			"models a single-key last-write-wins register with %q and %q operations only; "+
			"interpreting %q as one of them would be a guess about the system's semantics, and a "+
			"wrong guess is a false positive",
			other.id, other.name, opNameRead, opNameWrite, other.name)
		if other.shape.MicroOps > 0 {
			msg += fmt.Sprintf(". It carries %d micro-operation(s) over %s, so it is a TRANSACTION: "+
				"a transactional (Elle-style) checker is required",
				other.shape.MicroOps, pluralKeys(other.shape.Keys))
		}
		return &refusal{msg: msg}
	}
	if keyless.found {
		return refuse("op_id %d (%q) names no key on either its invoke or its completion, so it cannot "+
			"be placed in any key's sub-history", keyless.id, keyless.name)
	}
	return nil
}

func pluralKeys(keys []string) string {
	if len(keys) == 1 {
		return fmt.Sprintf("key %q", keys[0])
	}
	return fmt.Sprintf("%d keys (%s)", len(keys), joinQuoted(keys))
}

func joinQuoted(ss []string) string {
	out := ""
	for i, s := range ss {
		if i > 0 {
			out += ", "
		}
		out += fmt.Sprintf("%q", s)
	}
	return out
}

// ---------------------------------------------------------------------------
// D-B. ok / fail / info
// ---------------------------------------------------------------------------

// buildOperations applies the soundness rules for ok, fail and info.
//
//	ok    — definitely happened, with that response
//	fail  — definitely did NOT happen; dropped along with its invoke
//	info  — indeterminate
//	          read  → the value returned is unknown, so it constrains nothing → dropped
//	          write → may or may not have applied → kept, and BRANCHED on
//
// An invoke with no completion record at all is treated exactly as info.
//
// One further rule, which the directive does not spell out but which closes a
// real false-positive hole: an INDETERMINATE operation's interval is NOT bounded
// by its recorded completion timestamp. A request that timed out at t may still
// be executing on the server and may apply long after t. Constraining such a
// write to linearize before its own timeout constrains the model more than
// reality permits, and a more-constrained model rejecting a history does not
// prove the real one does. So an indeterminate write's interval extends to
// infinity, exactly as Jepsen/Knossos treat :info.
func buildOperations(raw map[int64]*rawOp, order []int64, path string, c counts) (*loaded, error) {
	ops := make([]*operation, 0, len(order))

	for _, id := range order {
		ro := raw[id]
		name, err := ro.opName()
		if err != nil {
			return nil, err
		}
		sh, err := ro.shape()
		if err != nil {
			return nil, err
		}
		key := sh.Keys[0] // checkPrecondition proved there is exactly one

		if ro.complete != nil && ro.complete.Type == schema.HistoryFail {
			c.FailedOps++
			continue
		}
		determinate := ro.complete != nil && ro.complete.Type == schema.HistoryOK

		op := &operation{key: key, id: id, invokeLine: ro.invokeLine, completeLine: ro.completeLine}
		if ro.invoke != nil && ro.invoke.Process != nil {
			op.process, op.hasProcess = *ro.invoke.Process, true
		} else if ro.complete != nil && ro.complete.Process != nil {
			op.process, op.hasProcess = *ro.complete.Process, true
		}

		switch name {
		case opNameRead:
			if !determinate {
				// The value read is unknown, so the read constrains nothing.
				// Dropping a read is always sound: reads do not change the
				// register, so any linearization of the full history restricted
				// to the remaining operations is still a linearization.
				c.InfoReads++
				continue
			}
			if len(ro.complete.Value) == 0 {
				c.ValuelessOK++
				continue
			}
			v, err := canonicalValue(ro.complete.Value)
			if err != nil {
				return nil, refuse("op_id %d (line %d of %s) returned a value this checker could not "+
					"parse: %v", id, ro.completeLine, path, err)
			}
			op.kind, op.value = opRead, v
			c.DetReads++

		case opNameWrite:
			rawVal := json.RawMessage(nil)
			if ro.complete != nil && len(ro.complete.Value) > 0 {
				rawVal = ro.complete.Value
			} else if ro.invoke != nil && len(ro.invoke.Value) > 0 {
				rawVal = ro.invoke.Value
			}
			if len(rawVal) == 0 {
				return nil, refuse("op_id %d is a write with no value on either record in %s; a write "+
					"whose value is unrecorded cannot be modelled", id, path)
			}
			v, err := canonicalValue(rawVal)
			if err != nil {
				return nil, refuse("op_id %d (%s) carries a value this checker could not parse: %v",
					id, path, err)
			}
			op.kind, op.value = opWrite, v
			if determinate {
				c.DetWrites++
			} else {
				op.maybe = true
				c.MaybeWrites++
			}

		default:
			// checkPrecondition already rejected these.
			return nil, refuse("op_id %d has unmodelled operation name %q", id, name)
		}

		if ro.invoke != nil {
			op.invokeNS = ro.invoke.TNS
		} else {
			op.openAtStart = true
			c.OrphanEnds++
		}
		if determinate {
			op.returnNS = ro.complete.TNS
			if !op.openAtStart && op.returnNS < op.invokeNS {
				// Widening the interval is the safe direction; shrinking it below
				// the invoke would invent a real-time ordering that never held.
				op.returnNS = op.invokeNS
				c.ClampedEnds++
			}
		} else {
			op.openAtEnd = true
		}

		ops = append(ops, op)
	}

	if len(ops) == 0 {
		return nil, refuse("%s carries %d operation record(s) over %d op_id(s), but none of them is "+
			"checkable: %d failed, %d were indeterminate reads and %d were ok reads with no recorded "+
			"value. A checker with nothing to check reports INCONCLUSIVE, never ok",
			path, c.Records, c.Operations, c.FailedOps, c.InfoReads, c.ValuelessOK)
	}
	if c.DetReads == 0 {
		return nil, refuse("%s carries %d checkable operation(s) but NOT ONE determinate read. A "+
			"register history without reads is trivially linearizable — every write linearizes — so "+
			"reporting ok would assert a property no evidence supports",
			path, len(ops))
	}

	byKey := make(map[string][]*operation, 8)
	for _, op := range ops {
		byKey[op.key] = append(byKey[op.key], op)
	}
	keys := make([]string, 0, len(byKey))
	for k := range byKey {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	return &loaded{Ops: ops, ByKey: byKey, Keys: keys, C: c}, nil
}
