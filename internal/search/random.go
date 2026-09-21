package search

import (
	"context"
	"fmt"

	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// ---------------------------------------------------------------------------
// The uniform-random baseline
//
// This is not filler. D-029 pre-registers the Phase 4 statistical claim as a
// ONE-SIDED FISHER'S EXACT TEST on the count of trials that find a violation
// within budget, with direction, alpha (0.05), per-arm world budget and fixture
// difficulty fixed IN ADVANCE, because a benchmark whose test is chosen after
// seeing the numbers is not evidence. The Saboteur is one arm of that test and
// this is the other. A strawman here would make Phase 4's definition of done
// meaningless in the most flattering possible direction.
//
// So the baseline is built to be FAIR, and fairness here is structural rather
// than aspirational. It shares, by construction rather than by convention:
//
//	the fault space          Params.Space          — same kinds, targets, rungs
//	the fault budget         Space.MaxFaults       — same ceiling, both arms
//	the window policy        Space.Window          — same reachable schedules
//	the validator            Params.Validator      — same definition of legal
//	the world cost           it executes real worlds, exactly like the Saboteur
//	the coverage extractor   Observe()             — same signal, same buckets
//	the corpus               Params.Corpus         — same archive
//
// What it does NOT share is the only thing under test: how it chooses. It
// chooses uniformly, it reads no telemetry, it consults no coverage, and its
// Observe is a deliberate no-op. That is what "uniform random fault injection"
// means, and pretending otherwise in either direction would corrupt the
// comparison.
//
// One thing it DOES do that a naive baseline would not: it still adds its
// worlds to the shared corpus. That is not adaptation (it never selects from
// the corpus) it is so that the two arms produce comparable coverage records
// and a comparable archive for Phase 5.
// ---------------------------------------------------------------------------

// Random is the uniform-random strategy.
type Random struct {
	p Params
	// proposed counts worlds handed out, for diagnostics only. It is not a
	// budget: the runner owns the budget.
	proposed int
	// seeds are the seed worlds, handed out before random generation begins.
	//
	// Seeding is part of the baseline on purpose: the base spec's own search
	// loop seeds one world per allowed fault kind plus a no-fault control, and
	// an arm denied its seed corpus would be a weaker baseline than the spec
	// describes. Both arms start from the same floor.
	seeds []schema.World
	// observed counts Observe calls, so a test can prove the strategy really is
	// ignoring them rather than merely appearing to.
	observed int
}

// NewRandom builds the baseline.
func NewRandom(p Params) (*Random, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	seeds, err := SeedWorlds(p)
	if err != nil {
		return nil, err
	}
	return &Random{p: p, seeds: seeds}, nil
}

// NewRandomWithoutSeeds builds the baseline with no seed corpus.
//
// It exists for a caller that has already seeded the corpus from another source:
// a replayed regression set, say. It is not the default, because a baseline
// that skipped the seed sweep would not be the baseline the base spec describes.
func NewRandomWithoutSeeds(p Params) (*Random, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return &Random{p: p}, nil
}

// Name implements Strategy.
func (r *Random) Name() schema.SearchStrategy { return schema.StrategyRandom }

// Propose implements Strategy.
//
// The first len(seeds) ordinals hand out the seed worlds; after that every
// world is drawn uniformly. The generation stream is keyed by the ORDINAL, so
// world 17 is generated identically whether the run reached it in seventeen
// steps or in seventeen steps and three retries, which is what A.8's
// reproducibility requirement actually demands.
func (r *Random) Propose(ctx context.Context, ordinal int) (schema.World, error) {
	if err := ctx.Err(); err != nil {
		return schema.World{}, err
	}
	if ordinal < 0 {
		return schema.World{}, fmt.Errorf("search: negative world ordinal %d", ordinal)
	}
	r.proposed++

	if ordinal < len(r.seeds) {
		return r.seeds[ordinal], nil
	}

	st := r.p.Streams.World(PathRandomWorld, ordinal)
	planned, err := RandomSchedule(r.p.Space, r.p.Validator, st)
	if err != nil {
		return schema.World{}, err
	}
	w := newWorld(r.p.Base, OriginSeeded, planned, st.Uint64())
	// A uniformly generated world has no parent, so `mutated` would be a lie
	// and `seeded` is what it is: a world the strategy constructed rather than
	// derived. Provenance has to be honest for Phase 5 to reason about lineage.
	if r.p.Validator != nil {
		if err := r.p.Validator.ValidateWorld(&w); err != nil {
			return schema.World{}, fmt.Errorf("search: random world failed validation after "+
				"its schedule passed: %w", err)
		}
	}
	return w, nil
}

// Observe implements Strategy and does nothing.
//
// This is the definition of the baseline, not an omission. A "random" strategy
// that quietly reweighted itself from coverage would be a coverage-guided
// fuzzer, and D-029's comparison would then be measuring the Saboteur against a
// second adaptive searcher while calling it uniform random.
func (r *Random) Observe(ctx context.Context, out Outcome) error {
	r.observed++
	return nil
}

// Proposed is how many worlds the strategy has handed out.
func (r *Random) Proposed() int { return r.proposed }

// Observations is how many outcomes were reported to it. It exists so a test
// can assert that the number is non-zero while the strategy's behaviour is
// unchanged: the honest form of "this strategy ignores feedback".
func (r *Random) Observations() int { return r.observed }

// Seeds returns the seed worlds, in the order Propose hands them out.
func (r *Random) Seeds() []schema.World {
	out := make([]schema.World, len(r.seeds))
	copy(out, r.seeds)
	return out
}

var _ Strategy = (*Random)(nil)
