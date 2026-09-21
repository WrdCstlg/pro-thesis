package main

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// ---------------------------------------------------------------------------
// Orchestration: precondition, per-key search, budget, witness
// ---------------------------------------------------------------------------

// options are the checker's tunables. They arrive from flags or the
// environment; none of them can change a verdict from violated to ok.
type options struct {
	// Wall is the total wall-clock budget for reading the history and searching
	// every key.
	Wall time.Duration
	// MaxStates is the total explored-state ceiling across every key.
	MaxStates int64
	// MaxMemoBytes bounds the memo table. Exceeding it degrades speed, never
	// soundness.
	MaxMemoBytes int64
}

func defaultOptions() options {
	return options{
		Wall:         30 * time.Second,
		MaxStates:    2_000_000,
		MaxMemoBytes: 256 << 20,
	}
}

// report is what the checker concluded.
type report struct {
	Status      schema.OracleStatus
	Witness     schema.Witness
	Explanation string
}

func inconclusive(format string, a ...any) report {
	return report{Status: schema.StatusInconclusive, Explanation: fmt.Sprintf(format, a...)}
}

// keyResult is one key's outcome.
type keyResult struct {
	Key string
	Out outcome
	St  stats
	N   int
}

// check runs the whole checker over a history file.
func check(path string, opt options, start time.Time) report {
	deadline := start.Add(opt.Wall)

	ld, err := loadHistory(path, deadline)
	if err != nil {
		return inconclusive("%s", err.Error())
	}

	bud := &budget{deadline: deadline, maxStates: opt.MaxStates}

	results := make([]keyResult, 0, len(ld.Keys))
	var firstViolated *keyResult
	var firstViolatedOps []*operation

	for i, key := range ld.Keys {
		ops := ld.ByKey[key]
		remaining := len(ld.Keys) - i
		share, keyDeadline, ok := bud.slice(remaining)
		if !ok {
			results = append(results, keyResult{
				Key: key, Out: outcomeExhausted, N: len(ops),
				St: stats{Why: exhaustWall, BestDepth: 0, BestLastWrite: -1},
			})
			continue
		}

		s := newSearcher(ops, share, keyDeadline, opt.MaxMemoBytes)
		out := s.run()
		bud.spend(s.st.States)

		kr := keyResult{Key: key, Out: out, St: s.st, N: len(ops)}
		results = append(results, kr)

		if out == outcomeViolated && firstViolated == nil {
			k := kr
			firstViolated = &k
			firstViolatedOps = ops
			// A witnessed violation on ONE key proves the whole history
			// non-linearizable (the locality theorem again, in the other
			// direction) so there is nothing left to establish and the remaining
			// keys are not searched.
			break
		}
	}

	if firstViolated != nil {
		w, diag := buildWitness(firstViolated.Key, firstViolatedOps, firstViolated.St)
		return report{
			Status:  schema.StatusViolated,
			Witness: w,
			Explanation: fmt.Sprintf(
				"key %q: no linearization exists for the %d operation(s) on this key under a "+
					"last-write-wins register. The search space was exhausted (%d state(s) explored "+
					"over %d step(s)) with every branch rejected; this is a WITNESSED failure, not a "+
					"timeout and not a budget. %s %s",
				firstViolated.Key, firstViolated.N, firstViolated.St.States, firstViolated.St.Steps,
				diag, evidenceLine(ld.C)),
		}
	}

	// D-C. Never ok for a key the search did not finish.
	var unfinished []keyResult
	for _, r := range results {
		if r.Out == outcomeExhausted {
			unfinished = append(unfinished, r)
		}
	}
	if len(unfinished) > 0 {
		return inconclusive("%s %s No conclusion is drawn for the unfinished key(s): an exhausted "+
			"budget is INCONCLUSIVE, never ok.",
			exhaustionLine(unfinished, results, bud), evidenceLine(ld.C))
	}

	var totalStates int64
	for _, r := range results {
		totalStates += r.St.States
	}
	return report{
		Status: schema.StatusOK,
		Explanation: fmt.Sprintf(
			"every one of the %d key(s) in %s admits a linearization of a last-write-wins register "+
				"(%d operation(s) checked, %d state(s) explored, search completed on every key). %s",
			len(ld.Keys), path, len(ld.Ops), totalStates, evidenceLine(ld.C)),
	}
}

// evidenceLine states what the checker actually had to work with. It is emitted
// on ok as well as on failure, because "checked and satisfied" and "nothing was
// checked" must never look alike in a verdict.
func evidenceLine(c counts) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Evidence: %d determinate read(s), %d determinate write(s), "+
		"%d indeterminate write(s) explored on both the applied and the not-applied branch",
		c.DetReads, c.DetWrites, c.MaybeWrites)
	extra := make([]string, 0, 5)
	if c.FailedOps > 0 {
		extra = append(extra, fmt.Sprintf("%d failed operation(s) dropped", c.FailedOps))
	}
	if c.InfoReads > 0 {
		extra = append(extra, fmt.Sprintf("%d indeterminate read(s) dropped", c.InfoReads))
	}
	if c.ValuelessOK > 0 {
		extra = append(extra, fmt.Sprintf("%d ok read(s) with no recorded value dropped", c.ValuelessOK))
	}
	if c.OrphanEnds > 0 {
		extra = append(extra, fmt.Sprintf("%d completion(s) had no invoke record and were opened at "+
			"the start of the history", c.OrphanEnds))
	}
	if c.ClampedEnds > 0 {
		extra = append(extra, fmt.Sprintf("%d completion(s) were timestamped before their own invoke "+
			"and had their interval widened", c.ClampedEnds))
	}
	if len(extra) > 0 {
		b.WriteString("; ")
		b.WriteString(strings.Join(extra, "; "))
	}
	b.WriteString(".")
	return b.String()
}

// exhaustionLine says how far the search got.
func exhaustionLine(unfinished, all []keyResult, b *budget) string {
	done := len(all) - len(unfinished)
	first := unfinished[0]
	why := string(first.St.Why)
	if why == "" {
		why = "the budget already consumed by earlier keys"
	}
	names := make([]string, 0, len(unfinished))
	for _, r := range unfinished {
		names = append(names, fmt.Sprintf("%q", r.Key))
	}
	if len(names) > 6 {
		names = append(names[:6], fmt.Sprintf("and %d more", len(names)-6))
	}
	return fmt.Sprintf(
		"the budget was exhausted: %d of %d key(s) were searched to completion; %d key(s) were not "+
			"(%s). The first unfinished key %q carries %d operation(s); %s stopped it after %d "+
			"state(s), with the deepest partial linearization placing %d of them. %d state(s) were "+
			"spent in total.",
		done, len(all), len(unfinished), strings.Join(names, ", "),
		first.Key, first.N, why, first.St.States, maxInt(first.St.BestDepth, 0), b.spent)
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// ---------------------------------------------------------------------------
// The witness
// ---------------------------------------------------------------------------

// buildWitness turns the deepest partial linearization into the frozen witness
// shape plus an explanation a coding agent can act on.
//
// The PROOF of non-linearizability is the exhausted search over the whole key,
// not the operations named here: there is no small certificate for
// non-membership, which is why the problem is NP-complete. What the witness does
// is POINT at the failure: the read that could not be placed, the write that
// established the value blocking it, and the earlier write whose value the read
// returned. That triple is exactly the stale-read shape. The explanation says
// which claim is which, so nobody mistakes a pointer for a proof.
func buildWitness(key string, ops []*operation, st stats) (schema.Witness, string) {
	order := make([]int, len(ops))
	for i := range ops {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return earlier(ops[order[a]], ops[order[b]]) })

	var focus *operation
	for _, i := range order {
		if testBit(st.BestLinearized, i) {
			continue
		}
		op := ops[i]
		if op.kind != opRead {
			continue
		}
		if st.BestState.Known() && st.BestState.Equal(op.value) {
			continue
		}
		focus = op
		break
	}

	var stateWriter *operation
	if st.BestLastWrite >= 0 && st.BestLastWrite < len(ops) {
		stateWriter = ops[st.BestLastWrite]
	}

	var staleSource *operation
	if focus != nil {
		for _, i := range order {
			if !testBit(st.BestLinearized, i) {
				continue
			}
			op := ops[i]
			if op.kind == opWrite && op.value.Equal(focus.value) {
				staleSource = op // keep the latest such write
			}
		}
	}

	picked := make([]*operation, 0, 3)
	seen := make(map[int64]bool, 3)
	for _, op := range []*operation{staleSource, stateWriter, focus} {
		if op == nil || seen[op.id] {
			continue
		}
		seen[op.id] = true
		picked = append(picked, op)
	}

	if len(picked) == 0 {
		// No read was identifiably blocked. Fall back to the operations that
		// could not be placed, capped, so the witness still points somewhere.
		for _, i := range order {
			if testBit(st.BestLinearized, i) {
				continue
			}
			picked = append(picked, ops[i])
			if len(picked) == 16 {
				break
			}
		}
	}

	sort.SliceStable(picked, func(a, b int) bool { return earlier(picked[a], picked[b]) })
	ids := make([]int64, 0, len(picked))
	for _, op := range picked {
		ids = append(ids, op.id)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "The deepest partial linearization the search reached placed %d of %d "+
		"operation(s) on this key.", maxInt(st.BestDepth, 0), len(ops))
	if focus == nil {
		fmt.Fprintf(&b, " No single blocked read isolates the failure; the witness names the "+
			"operation(s) that could not be placed.")
		return schema.Witness{OpIDs: ids, Key: key}, b.String()
	}

	fmt.Fprintf(&b, " At that point the register held %s", st.BestState.String())
	if stateWriter != nil {
		fmt.Fprintf(&b, ", written by op %d", stateWriter.id)
	}
	fmt.Fprintf(&b, ", and %s returned %s.", focus.describe(), focus.value.String())
	if staleSource != nil {
		fmt.Fprintf(&b, " That value was written by op %d, which every linearization places before "+
			"the value the register held.", staleSource.id)
	} else {
		fmt.Fprintf(&b, " No write already placed at that point had written that value.")
	}
	b.WriteString(" The witness op_ids point at the failure; the proof is the exhausted search over " +
		"the whole key, not those operations alone.")

	return schema.Witness{OpIDs: ids, Key: key}, b.String()
}

// earlier orders operations by the start of their real-time interval, then by
// op_id, so every witness and every explanation is deterministic.
func earlier(a, b *operation) bool {
	if a.openAtStart != b.openAtStart {
		return a.openAtStart
	}
	if a.invokeNS != b.invokeNS {
		return a.invokeNS < b.invokeNS
	}
	return a.id < b.id
}

// ---------------------------------------------------------------------------
// D-C. The budget
// ---------------------------------------------------------------------------

// budget is the shared wall-clock and explored-state allowance.
//
// Each key is given an equal share of what remains, so one pathological key
// cannot starve the rest and turn an otherwise complete check into a blanket
// INCONCLUSIVE. Unused share rolls over, because the share is recomputed from
// what has actually been spent.
type budget struct {
	deadline  time.Time
	maxStates int64
	spent     int64
}

func (b *budget) slice(remainingKeys int) (states int64, deadline time.Time, ok bool) {
	if remainingKeys < 1 {
		remainingKeys = 1
	}
	left := b.maxStates - b.spent
	if left <= 0 {
		return 0, time.Time{}, false
	}
	wallLeft := time.Until(b.deadline)
	if wallLeft <= 0 {
		return 0, time.Time{}, false
	}
	share := left / int64(remainingKeys)
	if share < 1 {
		share = 1
	}
	d := time.Now().Add(wallLeft / time.Duration(remainingKeys))
	if d.After(b.deadline) {
		d = b.deadline
	}
	return share, d, true
}

func (b *budget) spend(n int64) { b.spent += n }
