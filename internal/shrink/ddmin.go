package shrink

import "context"

// ---------------------------------------------------------------------------
// ddmin
//
// Zeller & Hildebrandt's delta debugging minimization, over the INDICES of an
// element list. It knows nothing about faults, operations, worlds or Docker: it
// asks a SubsetTest which of several candidate subsets still reproduces, and
// narrows.
//
// Keeping it index-pure is what lets the algorithm be pinned by tests against a
// synthetic oracle (a pure function deciding whether a subset reproduces) so
// its correctness is established without a container, and a later change to the
// execution machinery cannot silently change the algorithm.
// ---------------------------------------------------------------------------

// SubsetTest evaluates a batch of candidate subsets.
//
// It returns the index within `subsets` of the one whose reduction is ACCEPTED,
// or -1 when none is, plus a StopReason when the budget or the caller cut the
// evaluation short.
//
// A batch rather than a single subset, for two reasons that are the same reason:
// the subsets in one ddmin round are independent, so they can be executed
// concurrently against internal/control's parallel executor (D-043); and the
// implementation gets to define "accepted" as the LOWEST-INDEX subset that
// reproduced, which keeps the answer independent of completion order.
//
// A test that returns a non-empty StopReason must be assumed to have judged
// nothing further; ddmin stops immediately and reports the set it had reached.
type SubsetTest func(ctx context.Context, subsets [][]int) (int, StopReason)

// DDMin returns a 1-minimal subset of 0..n-1 that still reproduces, together
// with the reason it stopped early if it did.
//
// 1-minimal means: removing any single remaining element makes it stop
// reproducing. That is the contract delta debugging gives on an arbitrary
// (non-monotone) predicate, and it is the contract this returns, not "the
// globally smallest reproducing subset", which is not computable without
// exponential search.
//
// On an early stop the set returned is the smallest one confirmed so far, which
// is what makes a PARTIAL shrink honest: it reproduced, it is smaller than the
// input, and the caller is told the search did not finish.
func DDMin(ctx context.Context, n int, test SubsetTest) ([]int, StopReason) {
	c := make([]int, n)
	for i := range c {
		c[i] = i
	}
	if n == 0 {
		return c, StopComplete
	}

	gran := 2
	for len(c) >= 2 {
		if err := ctx.Err(); err != nil {
			return c, StopCanceled
		}
		if gran > len(c) {
			gran = len(c)
		}
		parts := partition(c, gran)

		// Reduce to a subset.
		i, stop := test(ctx, parts)
		if stop != StopComplete {
			return c, stop
		}
		if i >= 0 {
			c = parts[i]
			gran = 2
			continue
		}

		// Reduce to a complement.
		//
		// Skipped at granularity 2, where the two complements ARE the two parts
		// in the other order and have just been tested. That halves the cost of
		// the most common round in the search, which at ~30s a world is not a
		// micro-optimisation.
		if gran > 2 {
			comps := complements(c, parts)
			i, stop = test(ctx, comps)
			if stop != StopComplete {
				return c, stop
			}
			if i >= 0 {
				c = comps[i]
				if gran--; gran < 2 {
					gran = 2
				}
				continue
			}
		}

		if gran >= len(c) {
			// Granularity has reached the element count, so the last round
			// tested every single-element removal. Nothing further can come out:
			// c is 1-minimal.
			break
		}
		gran *= 2
		if gran > len(c) {
			gran = len(c)
		}
	}

	// One element left. The loop above never tries removing it, because
	// partitioning a singleton into two parts yields the singleton itself and
	// the empty set, and testing the singleton is the identity.
	//
	// The empty set is worth its one world. "Does this still reproduce with NO
	// elements at all" is the single largest reduction available, and it is the
	// question that stops a bug the workload alone produces from being reported
	// as caused by the last surviving fault. D-053 measured this fixture down to
	// ONE fault; whether it needs even that is a question with a cheap answer.
	if len(c) == 1 {
		i, stop := test(ctx, [][]int{{}})
		if stop != StopComplete {
			return c, stop
		}
		if i == 0 {
			return []int{}, StopComplete
		}
	}
	return c, StopComplete
}

// partition splits c into n contiguous, near-equal parts. Contiguity matters for
// a fault schedule: adjacent faults are adjacent in TIME, so a contiguous
// partition tends to separate causally related faults from unrelated ones and
// converges faster than an interleaved one would.
func partition(c []int, n int) [][]int {
	if n < 1 {
		n = 1
	}
	if n > len(c) {
		n = len(c)
	}
	out := make([][]int, 0, n)
	start := 0
	for i := 0; i < n; i++ {
		end := (i + 1) * len(c) / n
		if end < start {
			end = start
		}
		part := make([]int, end-start)
		copy(part, c[start:end])
		out = append(out, part)
		start = end
	}
	return out
}

// complements returns c minus each part, in the same order as the parts.
func complements(c []int, parts [][]int) [][]int {
	out := make([][]int, 0, len(parts))
	for _, p := range parts {
		drop := make(map[int]struct{}, len(p))
		for _, e := range p {
			drop[e] = struct{}{}
		}
		comp := make([]int, 0, len(c)-len(p))
		for _, e := range c {
			if _, gone := drop[e]; !gone {
				comp = append(comp, e)
			}
		}
		out = append(out, comp)
	}
	return out
}
