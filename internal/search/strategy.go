package search

import (
	"context"
	"errors"
	"fmt"

	"github.com/WrdCstlg/pro-thesis/internal/recorder"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// ---------------------------------------------------------------------------
// The strategy interface
//
// Two strategies implement it: `random` (random.go, in this package) and
// `saboteur` (internal/search/saboteur, another agent's deliverable). The
// interface is deliberately narrow (propose a world, observe what it did) so
// that the two arms of D-029's pre-registered benchmark differ ONLY in how they
// choose, and share the world cost, the fault space, the validator, the corpus
// and the coverage extractor.
//
// D-028 item 7 governs which strategy runs at all: the profile's `search`
// boolean decides WHETHER a search runs and the top-level `search:` block
// decides WHICH. `thesis gate` never runs MCTS.
// ---------------------------------------------------------------------------

// ErrExhausted reports that a strategy has nothing further to propose. It is
// not a failure: a search that has enumerated its space should stop rather than
// spend the rest of the budget re-running worlds.
var ErrExhausted = errors.New("search: the strategy has no further worlds to propose")

// Params is everything a strategy is built from.
//
// It is shared by construction between the strategies, which is what makes the
// benchmark a comparison of strategies rather than of budgets.
type Params struct {
	// Config is the resolved prothesis.yaml.
	Config *schema.Config
	// Space is the enumerated action space.
	Space *Space
	// Validator refuses any schedule the executor would refuse.
	Validator *Validator
	// Corpus is the shared archive. A strategy may read and add to it.
	Corpus *Corpus
	// Streams is the run's path-keyed randomness.
	Streams *Streams
	// Base is the world template: topology variant, driver profile, sut. Its
	// Seed is the base from which each world's own seed is derived.
	Base schema.World
}

// Validate checks that a Params is usable.
func (p Params) Validate() error {
	switch {
	case p.Config == nil:
		return errors.New("search: Params needs a config")
	case p.Space == nil:
		return errors.New("search: Params needs a space")
	case p.Corpus == nil:
		return errors.New("search: Params needs a corpus")
	case p.Streams == nil:
		return errors.New("search: Params needs a stream registry")
	case p.Base.TopologyVariant == "":
		return errors.New("search: Params.Base needs a topology_variant")
	case p.Base.DriverProfile == "":
		return errors.New("search: Params.Base needs a driver_profile")
	}
	return nil
}

// Strategy chooses what to execute next.
type Strategy interface {
	// Name is the strategy's `search.strategy` name.
	Name() schema.SearchStrategy

	// Propose returns the world to execute at this ordinal.
	//
	// The world it returns is already validated against the perturber, already
	// normalized, and already carries provenance. A strategy that returned an
	// unvalidated world would push the failure into the middle of a
	// thirty-second execution.
	//
	// It returns ErrExhausted when it has nothing left.
	Propose(ctx context.Context, ordinal int) (schema.World, error)

	// Observe records what executing a proposed world established. A strategy
	// that ignores Observe is a valid strategy: the random baseline does
	// exactly that, and saying so is the point of the baseline.
	Observe(ctx context.Context, out Outcome) error
}

// SeedWorlds builds the initial corpus: one world per permitted fault kind,
// plus the NO-FAULT CONTROL.
//
// The control is not optional. A.3's probe sweep drops it and every
// DAMPEN/REINFORCE criterion is comparative ("returned to baseline", "exceeded
// 2x baseline", "got worse after the fault ended") so without a control the
// classifier has no baseline and every criterion is unevaluable (D-028 item 6).
// The same argument applies to coverage: without a no-fault world, the templates
// a healthy system emits are indistinguishable from the ones a fault caused, and
// every seeded world's delta is inflated by the boot sequence.
//
// The control is FIRST in the returned slice, so a run that exhausts its budget
// early has still measured it.
func SeedWorlds(p Params) ([]schema.World, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	out := make([]schema.World, 0, len(p.Space.Kinds)+1)

	control := newWorld(p.Base, OriginSeeded, nil, p.Streams.Get(PathSeedWorlds, "control").Uint64())
	if p.Validator != nil {
		if err := p.Validator.ValidateWorld(&control); err != nil {
			return nil, err
		}
	}
	out = append(out, control)

	for _, kind := range p.Space.Kinds {
		if len(p.Space.Targets[kind]) == 0 {
			continue
		}
		w, err := seedWorldFor(p, kind, p.Streams.Get(PathSeedWorlds, string(kind)))
		if err != nil {
			// A kind with no schedulable target is skipped. That is a config
			// fact (the fixture's constraints refuse majority partitions, for
			// instance) not a search failure. It becomes a failure only when
			// EVERY kind fails, which is the check below.
			continue
		}
		out = append(out, w)
	}
	if len(out) == 1 {
		return nil, fmt.Errorf("%w: no permitted fault kind produced a schedulable seed world",
			ErrNoValidCandidate)
	}
	return out, nil
}

// newWorld builds an unexecuted world from the base template.
//
// It clears realized faults and phase timings: those are measurements of a run,
// and carrying a template's forward would be a claim about an execution that
// never happened.
func newWorld(base schema.World, origin string, planned []string, seed uint64) schema.World {
	w := base
	w.Seed = seed
	if planned == nil {
		planned = []string{}
	}
	w.FaultSchedule.Planned = append([]string(nil), planned...)
	w.FaultSchedule.Realized = nil
	w.PhaseTimings = schema.PhaseTimings{}
	w.Meta = &schema.WorldMeta{Origin: origin}
	return w.Normalized()
}

// seedWorldFor builds one single-fault world for a named kind.
func seedWorldFor(p Params, kind schema.FaultKind, st *recorder.Stream) (schema.World, error) {
	// Deliberately not RandomFault: a seed world must exercise a NAMED kind, so
	// the kind is fixed and only the target, magnitude and window are drawn.
	targets := p.Space.Targets[kind]
	rungs := p.Space.Rungs(kind)
	for attempt := 0; attempt < GenerateAttempts; attempt++ {
		t := pick(st, targets)
		params := pick(st, rungs)
		start, end := randomWindow(p.Space.Window, kind.Durative(), st)
		f, err := BuildFault(kind, t, params, start, end)
		if err != nil {
			continue
		}
		w := newWorld(p.Base, OriginSeeded, []string{f.String()}, st.Uint64())
		if p.Validator != nil {
			if err := p.Validator.ValidateWorld(&w); err != nil {
				continue
			}
		}
		return w, nil
	}
	return schema.World{}, fmt.Errorf("%w: %s", ErrNoValidCandidate, kind)
}
