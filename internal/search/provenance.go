package search

import (
	"errors"
	"fmt"

	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// ---------------------------------------------------------------------------
// Provenance
//
// pkg/schema.WorldMeta reserved `origin` and `parent_hash` in Phase 0 (D-014)
// for exactly this phase. Both are HASH-EXCLUDED and `omitempty`, so stamping
// provenance on a world never changes what the world IS and never invalidates a
// committed regression.
//
// Populating them is not bookkeeping. Phase 5 shrinks corpus worlds and
// invariant I2 requires the result to be self-contained; a corpus entry that
// cannot say where it came from is one a human cannot audit and one `thesis
// bisect` cannot reason about. And a corpus that does not serialize is a corpus
// Phase 5 cannot use at all.
// ---------------------------------------------------------------------------

// Origins. WorldMeta.Origin is an open string, so these are the values THIS
// package writes rather than a closed schema enum.
const (
	// OriginSeeded is an initial world the search constructed before any
	// observation: one per allowed fault kind, plus the no-fault control.
	OriginSeeded = "seeded"
	// OriginMutated is a world produced by applying a mutation operator to a
	// corpus parent.
	OriginMutated = "mutated"
	// OriginSaboteurProbe is a Tier 1 probe world. Written by
	// internal/search/saboteur; declared here because the corpus has to be able
	// to recognise it.
	OriginSaboteurProbe = "saboteur-probe"
	// OriginSaboteurMCTS is a Tier 2 rollout world.
	OriginSaboteurMCTS = "saboteur-mcts"
	// OriginShrunk is a Phase 5 product.
	OriginShrunk = "shrunk"
	// OriginManual is a hand-authored world: a committed regression, or one a
	// human wrote to reproduce something.
	OriginManual = "manual"
	// OriginLLM is a world whose schedule was proposed by the llm strategy's
	// model endpoint and validated locally (D-075).
	OriginLLM = "llm"
)

// AllOrigins is every origin this tree writes, sorted. It is used to validate a
// corpus entry rather than to constrain the schema field.
var AllOrigins = []string{
	OriginLLM, OriginManual, OriginMutated, OriginSaboteurMCTS, OriginSaboteurProbe,
	OriginSeeded, OriginShrunk,
}

// KnownOrigin reports whether o is one this tree writes. An unknown origin is
// not an error (the field is an open string and a future phase may add one)
// but the corpus refuses an EMPTY one.
func KnownOrigin(o string) bool {
	for _, x := range AllOrigins {
		if x == o {
			return true
		}
	}
	return false
}

// ErrNoOrigin reports a world offered to the corpus with no provenance.
var ErrNoOrigin = errors.New("search: world has no meta.origin; a corpus entry that cannot say " +
	"where it came from is one Phase 5 cannot audit")

// Stamp returns a copy of w carrying the given provenance, normalized.
//
// It works on a COPY. Stamping the caller's world in place would mutate a
// corpus parent while a mutation operator is deriving a child from it, and
// because Normalized() sorts the planned schedule the damage would be silent
// and would surface much later as a world that no longer matches its own hash.
func Stamp(w schema.World, origin, parentHash string) schema.World {
	out := w
	meta := schema.WorldMeta{Origin: origin, ParentHash: parentHash}
	if w.Meta != nil {
		// Preserve anything already set that this call does not name.
		meta = *w.Meta
		if origin != "" {
			meta.Origin = origin
		}
		if parentHash != "" {
			meta.ParentHash = parentHash
		}
	}
	out.Meta = &meta
	return out.Normalized()
}

// Derive returns a child of parent: the same world shape with a new planned
// schedule, stamped with the given origin and with parent_hash set to the
// parent's own hash.
//
// The parent hash is computed from the parent EXACTLY AS IT STANDS, before any
// normalization of the child, so the recorded lineage points at a world that
// really exists on disk under that hash.
func Derive(parent schema.World, origin string, mutate func(*schema.World)) (schema.World, error) {
	ph, err := parent.Hash()
	if err != nil {
		return schema.World{}, fmt.Errorf("search: hash parent world: %w", err)
	}
	child := parent
	// Deep-copy the mutable parts so a mutator cannot reach back into the
	// parent's slices.
	child.FaultSchedule.Planned = append([]string(nil), parent.FaultSchedule.Planned...)
	// A derived world has never been executed: carrying the parent's realized
	// schedule or measured phase timings forward would be a claim about a run
	// that never happened.
	child.FaultSchedule.Realized = nil
	child.PhaseTimings = schema.PhaseTimings{}
	child.Meta = nil

	if mutate != nil {
		mutate(&child)
	}
	return Stamp(child, origin, ph), nil
}

// ValidateEntryWorld checks the invariants the corpus requires of every entry.
func ValidateEntryWorld(w *schema.World) error {
	if w == nil {
		return errors.New("search: nil world")
	}
	if err := w.Validate(); err != nil {
		return err
	}
	if w.Meta == nil || w.Meta.Origin == "" {
		return ErrNoOrigin
	}
	return nil
}
