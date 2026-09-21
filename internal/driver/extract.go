package driver

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

// History record types this package needs to tell apart. They are the
// directive's own section 4.4 vocabulary; nothing here invents a name.
const (
	histInvoke = "invoke"
	histOK     = "ok"
	histFail   = "fail"
	histInfo   = "info"
)

// ExtractOptions steers ExtractOps.
type ExtractOptions struct {
	// Pinned is the violation witness's op ids. They are marked
	// PlanOp.Pinned, and Extraction.Plan refuses to build a trace that cannot
	// carry all of them unless AllowMissingPinned is set.
	Pinned []int64

	// AllowMissingPinned downgrades "the history does not contain a witness op"
	// from an error to a report.
	//
	// The default is to REFUSE, because the caller's next move is to run ddmin
	// against the resulting trace and ask "does this candidate still reproduce
	// the same violation": a question that has no answer once the evidence is
	// gone. Set this only when the caller intends to report op shrinking as NOT
	// ATTEMPTED (D-021) rather than to shrink.
	AllowMissingPinned bool
}

// OpOutcome is what the ORIGINAL history said happened to an operation.
//
// It is not part of the plan: a plan is a request, and re-issuing an operation
// does not re-issue its result. It is reported so a caller can see the shape of
// the evidence it is about to shrink: in particular whether a pinned witness
// operation was INDETERMINATE.
type OpOutcome struct {
	OpID int64
	// Type is "ok", "fail", "info", or "" when the history recorded no
	// completion at all: an operation still in flight when observation ended.
	Type string
}

// Extraction is the result of reading an operation trace out of a history.
type Extraction struct {
	// Ops is the trace, ascending by op id.
	//
	// Op id order IS the original per-process issue order: a process is a
	// sequential thread that takes a fresh, monotonically increasing op id per
	// operation and holds one at a time. Sorting by op id is therefore not a
	// convenience: it is the canonical form, and it is what makes two
	// extractions of the same history identical whatever order the lines
	// arrived in.
	Ops []PlanOp

	// InvokeOrder is the op ids in the order the operations were actually
	// ISSUED, by invoke timestamp. It normally equals Ops's order; where it
	// does not, the driver did not issue the trace in the order it was given,
	// which VerifyReplay is looking for.
	InvokeOrder []int64
	// Targets is the sorted distinct set of targets the operations addressed.
	Targets []string
	// Origin and OriginNS are what Ops[i].AtMS is measured from.
	Origin   string
	OriginNS int64

	// Lines, OpRecords and PhaseMarkers describe what was read.
	Lines        int
	OpRecords    int
	PhaseMarkers int
	// TruncatedTail is set when the input's final line had no newline and did
	// not parse: the signature of a driver killed mid-write. It is tolerated
	// (and reported) rather than fatal, because a driver stopped at QUIESCE is
	// the normal case, not a defect.
	TruncatedTail bool
	// CompletionsWithoutInvoke counts operations that have a completion record
	// but no invoke. They are NOT extractable: a completion records what the
	// system did, and for a transaction that genuinely differs from what was
	// asked, so replaying one would replay a different operation.
	CompletionsWithoutInvoke int
	// ClampedOffsets counts operations whose invoke preceded the origin. Their
	// AtMS is clamped to zero rather than made negative.
	ClampedOffsets int

	// Outcomes is what the original history recorded for each extracted
	// operation, ascending by op id.
	Outcomes []OpOutcome

	// MissingPinned are witness op ids with no INVOKE record in this history:
	// wholly absent, or present only as a completion. They cannot be replayed
	// and cannot be pinned, so a trace built from this history cannot be
	// checked against the original violation.
	MissingPinned []int64
	// AllowedMissing mirrors ExtractOptions.AllowMissingPinned, so Plan can
	// honour it without the caller passing the options twice. It is exported
	// because a caller that builds an Extraction by hand (a test, or a future
	// extractor for a driver with a different history shape) must be able to
	// set it.
	AllowedMissing bool

	// IndeterminatePinned are witness op ids that ARE extractable but whose
	// original outcome was `info` or had no completion record at all.
	//
	// This is OQ-034 arriving in the shrink pipeline. The linearizability
	// checker's witness is a POINTER built from the deepest partial
	// linearization, and it may name a superseding write whose interval extends
	// to infinity. Such an operation is real evidence and must still be pinned;
	// what it is not is a fact with a definite outcome, so a caller comparing
	// two runs' values for it will find nothing to compare. Reported so the
	// caller can say so rather than discover it as a flake.
	IndeterminatePinned []int64

	outcome map[int64]string
}

// Outcome returns the original outcome of an op id: "ok", "fail", "info", or ""
// when there was no completion record (or no such operation).
func (e *Extraction) Outcome(opID int64) string { return e.outcome[opID] }

// histLine is the decode target for one history line.
//
// It is deliberately partial and NON-STRICT. The reference fixture attaches a
// namespaced `meta` object carrying node, term, commit_index, read_mode,
// served_by and latency_us, and a third-party driver will attach its own; a
// strict decode would reject a perfectly good history for carrying evidence.
type histLine struct {
	TNS     int64           `json:"t_ns"`
	Process *int            `json:"process"`
	Type    string          `json:"type"`
	F       string          `json:"f"`
	Key     string          `json:"key"`
	Value   json.RawMessage `json:"value"`
	OpID    int64           `json:"op_id"`
	Event   string          `json:"event"`
	Phase   string          `json:"phase"`
	Meta    *histMeta       `json:"meta"`
}

// histMeta reads the two members of the driver's namespaced extension that a
// faithful replay needs.
//
// Reaching into `meta` at all is a considered choice. It is documented as the
// place "nothing soundness-critical may depend on", and nothing here does: an
// absent meta yields a plan with no read_mode and no target, which is still a
// valid plan whose driver falls back to its own defaults. What is gained is
// large: a lease read and a linearizable read take DIFFERENT CODE PATHS in the
// reference fixture, and only one of them can be stale, so a trace that forgets
// which was asked for reproduces a different thing.
type histMeta struct {
	Target   string `json:"target"`
	ReadMode string `json:"read_mode"`
}

// ExtractOpsFromFile reads a history JSONL from disk.
func ExtractOpsFromFile(path string, opts ExtractOptions) (*Extraction, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("driver: open history %s: %w", path, err)
	}
	defer f.Close()
	e, err := ExtractOps(f, opts)
	if err != nil {
		return nil, fmt.Errorf("driver: history %s: %w", path, err)
	}
	return e, nil
}

// ExtractOps reads an operation trace out of a history JSONL.
//
// The trace is built from INVOKE records only. Completions contribute their
// outcome and nothing else.
//
// A malformed line is a HARD ERROR, with one exception: a final line with no
// terminating newline is treated as a truncated tail and reported. The
// asymmetry is deliberate. Skipping malformed lines silently is how the very
// operation a witness names disappears from a trace, and D-011 already records
// that one torn line makes a history unparseable, but a driver killed at
// QUIESCE legitimately leaves a partial last line, and failing the whole shrink
// on the normal case would be its own defect.
func ExtractOps(r io.Reader, opts ExtractOptions) (*Extraction, error) {
	e := &Extraction{
		Ops:            []PlanOp{},
		InvokeOrder:    []int64{},
		Targets:        []string{},
		Outcomes:       []OpOutcome{},
		outcome:        map[int64]string{},
		Origin:         OriginFirstOp,
		AllowedMissing: opts.AllowMissingPinned,
	}

	type pending struct {
		op  PlanOp
		tns int64
	}
	invokes := map[int64]*pending{}
	seenAny := map[int64]bool{}
	targets := map[string]bool{}

	var (
		driveNS    int64
		haveDrive  bool
		firstOpNS  int64
		haveFirst  bool
		dupInvokes []int64
	)

	br := bufio.NewReaderSize(r, 256*1024)
	lineNo := 0
	for {
		raw, readErr := br.ReadString('\n')
		if readErr != nil && readErr != io.EOF {
			return nil, fmt.Errorf("driver: read history at line %d: %w", lineNo+1, readErr)
		}
		eof := readErr == io.EOF
		terminated := strings.HasSuffix(raw, "\n")
		line := strings.TrimRight(raw, "\r\n")

		if line != "" {
			lineNo++
			e.Lines++

			var h histLine
			if err := json.Unmarshal([]byte(line), &h); err != nil {
				if eof && !terminated {
					// The final line has no newline: the writer was killed
					// mid-write, which is the normal end of a driver stopped
					// at QUIESCE rather than a defect.
					e.TruncatedTail = true
					e.Lines--
					break
				}
				return nil, fmt.Errorf("driver: history line %d is not JSON: %w", lineNo, err)
			}

			switch {
			case h.Event != "" || (h.Type == "" && h.Phase != ""):
				e.PhaseMarkers++
				if !haveDrive && h.Phase == "DRIVE" {
					driveNS, haveDrive = h.TNS, true
				}
			case h.OpID == 0:
				// A record with no op_id cannot be addressed by a witness and
				// cannot be replayed. Read, counted, carried no further.
			default:
				e.OpRecords++
				seenAny[h.OpID] = true
				switch h.Type {
				case histInvoke:
					if _, dup := invokes[h.OpID]; dup {
						dupInvokes = append(dupInvokes, h.OpID)
						break
					}
					op := PlanOp{OpID: h.OpID, F: h.F, Key: h.Key}
					if h.Process != nil {
						op.Process = *h.Process
					}
					if len(h.Value) > 0 && string(h.Value) != "null" {
						op.Value = append(json.RawMessage(nil), h.Value...)
					}
					if h.Meta != nil {
						op.ReadMode = h.Meta.ReadMode
						op.Target = h.Meta.Target
						if op.Target != "" {
							targets[op.Target] = true
						}
					}
					invokes[h.OpID] = &pending{op: op, tns: h.TNS}
					if !haveFirst || h.TNS < firstOpNS {
						firstOpNS, haveFirst = h.TNS, true
					}
				case histOK, histFail, histInfo:
					// Last completion wins. A well-formed history has exactly
					// one; a driver that emitted two has a defect, and the
					// later record is the more conservative reading of what it
					// finally observed.
					e.outcome[h.OpID] = h.Type
				}
			}
		}

		if eof {
			break
		}
	}
	if len(dupInvokes) > 0 {
		sort.Slice(dupInvokes, func(a, b int) bool { return dupInvokes[a] < dupInvokes[b] })
		return nil, fmt.Errorf("driver: history has more than one invoke record for op_id(s) %v; "+
			"two possible executions behind one operation makes the history unsound and a "+
			"trace built from it would replay only one of them", dupInvokes)
	}

	// The origin. A DRIVE phase marker is preferred because a fault window is
	// measured from the same instant, so an op offset and a fault offset become
	// directly comparable, which is the whole reason a shrunk trace can still
	// overlap the window that produced the anomaly.
	if haveDrive {
		e.Origin, e.OriginNS = OriginDrive, driveNS
	} else if haveFirst {
		e.Origin, e.OriginNS = OriginFirstOp, firstOpNS
	}

	ids := make([]int64, 0, len(invokes))
	for id := range invokes {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(a, b int) bool { return ids[a] < ids[b] })

	// Issue order, by invoke timestamp, tie-broken by op id. It is derived
	// from t_ns rather than from the line order because the driver's own
	// history is explicitly NOT guaranteed to be sorted (timestamps are taken
	// at the true event instant, not when the writer wins its mutex), and
	// MergeHistory then reorders it by t_ns anyway. Reading the order off the
	// timestamps is correct under both layouts.
	e.InvokeOrder = append(e.InvokeOrder, ids...)
	sort.SliceStable(e.InvokeOrder, func(a, b int) bool {
		x, y := invokes[e.InvokeOrder[a]], invokes[e.InvokeOrder[b]]
		if x.tns != y.tns {
			return x.tns < y.tns
		}
		return e.InvokeOrder[a] < e.InvokeOrder[b]
	})

	for _, id := range ids {
		p := invokes[id]
		off := (p.tns - e.OriginNS) / 1_000_000
		if off < 0 {
			off = 0
			e.ClampedOffsets++
		}
		op := p.op
		op.AtMS = off
		e.Ops = append(e.Ops, op)
		e.Outcomes = append(e.Outcomes, OpOutcome{OpID: id, Type: e.outcome[id]})
	}
	for id := range seenAny {
		if _, ok := invokes[id]; !ok {
			e.CompletionsWithoutInvoke++
		}
	}
	for t := range targets {
		e.Targets = append(e.Targets, t)
	}
	sort.Strings(e.Targets)

	// Pins, and the two honest ways they can be unsatisfiable.
	pinned := map[int64]bool{}
	for _, id := range opts.Pinned {
		if pinned[id] {
			continue
		}
		pinned[id] = true
		if _, ok := invokes[id]; !ok {
			e.MissingPinned = append(e.MissingPinned, id)
			continue
		}
		if t := e.outcome[id]; t != histOK {
			e.IndeterminatePinned = append(e.IndeterminatePinned, id)
		}
	}
	sort.Slice(e.MissingPinned, func(a, b int) bool { return e.MissingPinned[a] < e.MissingPinned[b] })
	sort.Slice(e.IndeterminatePinned, func(a, b int) bool {
		return e.IndeterminatePinned[a] < e.IndeterminatePinned[b]
	})
	for i := range e.Ops {
		if pinned[e.Ops[i].OpID] {
			e.Ops[i].Pinned = true
		}
	}
	return e, nil
}

// Plan returns base with the extracted trace attached.
//
// base is not modified; the result is a deep copy carrying the same profile,
// seed and history path. Passing nil yields a bare operation plan, which is what
// a caller shrinking a world that was driven by a foreign profile wants.
//
// It REFUSES when a witness op id had no invoke record, unless
// ExtractOptions.AllowMissingPinned was set. The refusal is the point: a trace
// missing its own evidence cannot answer "is this the same violation", and a
// minimizer that proceeds anyway produces a minimal repro for a bug nobody
// asked about.
func (e *Extraction) Plan(base *Plan) (*Plan, error) {
	if !e.AllowedMissing && len(e.MissingPinned) > 0 {
		return nil, fmt.Errorf("driver: the history has no invoke record for witness op_id(s) %v. "+
			"They are the evidence a shrunk candidate is checked against, so operation shrinking "+
			"must be reported as NOT ATTEMPTED (DECISIONS.md D-021) rather than run without them",
			e.MissingPinned)
	}
	var p *Plan
	if base != nil {
		p = base.Clone()
	} else {
		p = &Plan{Schema: PlanSchema, Mix: []MixEntry{}}
	}
	p.Operations = make([]PlanOp, len(e.Ops))
	for i, op := range e.Ops {
		p.Operations[i] = cloneOp(op)
	}
	p.Targets = append([]string(nil), e.Targets...)
	p.Origin = e.Origin
	p.OriginNS = e.OriginNS
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return p, nil
}

// VerifyReplay cross-checks a replayed history against the plan that produced
// it.
//
// This is how D-021's "a driver supporting neither ignores both, and op
// shrinking is then reported as NOT ATTEMPTED" becomes an OBSERVATION rather
// than an assumption. The driver is an external, unmodified process; it cannot
// be asked whether it honoured the plan, and a driver that ignored it produces a
// perfectly well-formed history of a completely different workload. The only
// evidence available from outside is whether the op ids in the history are the
// op ids in the plan.
func VerifyReplay(p *Plan, historyPath string) (*ReplayCheck, error) {
	if !p.HasOperations() {
		return nil, fmt.Errorf("driver: VerifyReplay on a profile plan: there is no trace to verify against")
	}
	e, err := ExtractOpsFromFile(historyPath, ExtractOptions{AllowMissingPinned: true})
	if err != nil {
		return nil, err
	}
	return p.compare(e), nil
}

// ReplayCheck is the result of VerifyReplay.
type ReplayCheck struct {
	// Planned and Observed are the operation counts.
	Planned  int
	Observed int
	// Matched is how many planned op ids appear in the replay.
	Matched int
	// Missing are planned op ids the replay never issued. This is NOT by itself
	// evidence that the driver ignored the plan: QUIESCE stops the driver
	// (D-042), so a truncated tail of the trace is the normal outcome of a
	// world whose budget ran out before the trace did.
	Missing []int64
	// Extra are op ids the replay issued that the plan never named. These ARE
	// evidence: a driver executing the trace cannot invent operations, so even
	// one of these means the driver generated its own workload and every
	// comparison with the original is void.
	Extra []int64
	// OutOfOrder names processes whose operations did not appear in the plan's
	// order. A logical process is sequential; reordering it changes the
	// concurrency structure and therefore which histories are linearizable.
	OutOfOrder []int
	// PinnedMissing are witness op ids the replay never issued. A candidate
	// with these is UNCHECKABLE (neither "still reproduces" nor "no longer
	// reproduces") and must be treated as an inconclusive execution, never as
	// a rejection.
	PinnedMissing []int64
	// Honoured reports that the driver executed this plan rather than
	// generating its own workload.
	Honoured bool
	// Checkable reports that the replay can be compared with the original: the
	// plan was honoured AND every pinned operation was issued.
	Checkable bool
}

// Reason renders the check as one line for a human or a log.
func (c *ReplayCheck) Reason() string {
	switch {
	case !c.Honoured && c.Matched == 0:
		return fmt.Sprintf("the driver issued %d operation(s), none of them from the plan: "+
			"it does not consume %s or %s, so operation shrinking is NOT ATTEMPTED",
			c.Observed, "{plan_path}", "PROTHESIS_PLAN_PATH")
	case !c.Honoured && len(c.Extra) > 0:
		return fmt.Sprintf("the driver issued %d operation(s) the plan never named (first: %v): "+
			"it generated its own workload, so this replay cannot be compared with the original",
			len(c.Extra), firstN(c.Extra, 3))
	case !c.Honoured && len(c.OutOfOrder) > 0:
		return fmt.Sprintf("process(es) %v executed out of plan order: a logical process is "+
			"sequential, and reordering it changes which histories are linearizable", c.OutOfOrder)
	case !c.Checkable:
		return fmt.Sprintf("the plan was honoured but pinned op_id(s) %v were never issued: "+
			"the witness evidence is absent, so this candidate is UNCHECKABLE rather than failing",
			c.PinnedMissing)
	case len(c.Missing) > 0:
		return fmt.Sprintf("the plan was honoured: %d of %d operation(s) issued, %d not reached "+
			"before the driver was stopped", c.Matched, c.Planned, len(c.Missing))
	default:
		return fmt.Sprintf("the plan was honoured in full: %d of %d operation(s) issued",
			c.Matched, c.Planned)
	}
}

func (p *Plan) compare(e *Extraction) *ReplayCheck {
	planned := make(map[int64]PlanOp, len(p.Operations))
	order := make(map[int64]int, len(p.Operations))
	for i, op := range p.Operations {
		planned[op.OpID] = op
		order[op.OpID] = i
	}
	seen := make(map[int64]bool, len(e.Ops))
	c := &ReplayCheck{
		Planned:  len(p.Operations),
		Observed: len(e.Ops),
		Missing:  []int64{},
		Extra:    []int64{},
	}
	byID := make(map[int64]PlanOp, len(e.Ops))
	for _, op := range e.Ops {
		byID[op.OpID] = op
	}
	lastIdx := map[int]int{}
	bad := map[int]bool{}
	// Walk ISSUE order, not op-id order: the question is whether the driver
	// executed the trace in the order it was given, and op-id order is the
	// order it was given, so comparing it with itself proves nothing.
	for _, id := range e.InvokeOrder {
		got, ok := byID[id]
		if !ok {
			continue
		}
		if _, ok := planned[got.OpID]; !ok {
			c.Extra = append(c.Extra, got.OpID)
			continue
		}
		seen[got.OpID] = true
		c.Matched++
		idx := order[got.OpID]
		if prev, ok := lastIdx[got.Process]; ok && idx < prev {
			bad[got.Process] = true
		}
		lastIdx[got.Process] = idx
	}
	for _, op := range p.Operations {
		if !seen[op.OpID] {
			c.Missing = append(c.Missing, op.OpID)
			if op.Pinned {
				c.PinnedMissing = append(c.PinnedMissing, op.OpID)
			}
		}
	}
	for pr := range bad {
		c.OutOfOrder = append(c.OutOfOrder, pr)
	}
	sort.Ints(c.OutOfOrder)
	c.Honoured = c.Matched > 0 && len(c.Extra) == 0 && len(c.OutOfOrder) == 0
	c.Checkable = c.Honoured && len(c.PinnedMissing) == 0
	return c
}

func firstN(ids []int64, n int) []int64 {
	if len(ids) <= n {
		return ids
	}
	return ids[:n]
}
