package search

import (
	"fmt"
	"sort"
	"strconv"
	"sync"

	"github.com/WrdCstlg/pro-thesis/internal/recorder"
)

// ---------------------------------------------------------------------------
// Search randomness
//
// Addendum A.8: "All Saboteur decisions (fault selection, tree expansion) must
// be deterministic given the same PRNG seed."
//
// Every draw in this package comes from a recorder Stream keyed by an explicit
// PATH. That is not decoration: D-013 makes a stream's key a function of its
// path ALONE, so adding a new named stream cannot perturb an existing one. Two
// consequences the search depends on:
//
//  1. Adding the Saboteur's own streams later cannot change a single value the
//     random baseline draws, so a benchmark run before and after that change is
//     still comparing the same arm.
//  2. The k-th world's generation stream is Path(..., "world", "k"), so world 17
//     is generated identically whether the run reached it after 16 worlds or
//     after 16 worlds and a retry. A search whose world N depended on how many
//     values worlds 1..N-1 happened to consume would not replay.
//
// recorder.Streams is deliberately NOT reused: it is the per-WORLD registry,
// built from a world's own seed, and its registered-domain list is a Phase 0
// frozen table. Search decisions are per-RUN and are derived from the RUN seed,
// so they get their own registry here rather than an entry appended to a table
// that governs world-internal randomness.
// ---------------------------------------------------------------------------

// Stream path roots. They are part of the reproducibility contract: renaming
// one silently changes every search's behaviour while leaving every world
// identity unchanged.
const (
	// PathCorpusSelect draws the energy-weighted corpus pick.
	PathCorpusSelect = "search.corpus.select"
	// PathMutateOp chooses which mutation operator to apply.
	PathMutateOp = "search.mutate.op"
	// PathMutateDraw draws a mutation operator's own parameters.
	PathMutateDraw = "search.mutate.draw"
	// PathRandomWorld generates one world for the uniform-random baseline.
	PathRandomWorld = "search.random.world"
	// PathSeedWorlds generates the initial seed corpus.
	PathSeedWorlds = "search.seed.world"
)

// Streams is the per-RUN search stream registry.
//
// It memoizes by full path so that sequential consumption of one path continues
// where it left off, exactly like recorder.Streams does for a world.
type Streams struct {
	seed recorder.Seed
	mu   sync.Mutex
	used map[string]*recorder.Stream
	// order preserves first-use order for the audit, so the audit does not
	// depend on map iteration.
	order []string
}

// NewStreams builds the registry from a run's root seed.
func NewStreams(runSeed recorder.Seed) *Streams {
	return &Streams{seed: runSeed, used: map[string]*recorder.Stream{}}
}

// Seed returns the run seed the registry was built from.
func (s *Streams) Seed() recorder.Seed { return s.seed }

// Get returns the memoized stream for a path.
//
// It PANICS on an empty or malformed path, matching recorder.Streams.Get's
// rule and for the same reason: a typo'd path would silently produce a fresh,
// perfectly deterministic, completely unreproducible stream that no other build
// would recreate.
func (s *Streams) Get(path ...string) *recorder.Stream {
	if len(path) == 0 {
		panic("search: Streams.Get with no path")
	}
	key := joinPath(path)
	s.mu.Lock()
	defer s.mu.Unlock()
	if st, ok := s.used[key]; ok {
		return st
	}
	k, err := recorder.DeriveKey(s.seed, path...)
	if err != nil {
		panic(fmt.Sprintf("search: derive stream %q: %v", key, err))
	}
	st := recorder.NewStream(k)
	s.used[key] = st
	s.order = append(s.order, key)
	return st
}

// World returns the stream for one ordinal under a path root. It is the
// primitive that makes world N's generation independent of worlds 1..N-1.
func (s *Streams) World(root string, ordinal int) *recorder.Stream {
	return s.Get(root, strconv.Itoa(ordinal))
}

// Audit reports draw counts per path, sorted by path.
func (s *Streams) Audit() []recorder.StreamAudit {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]recorder.StreamAudit, 0, len(s.used))
	for _, k := range s.order {
		out = append(out, recorder.StreamAudit{Domain: k, Draws: s.used[k].Draws()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Domain < out[j].Domain })
	return out
}

// joinPath renders a path for display and for the memo key. The separator is
// '/' rather than the NUL the derivation uses, because this string is only ever
// shown to a human: the KEY is derived from the segments, not from this.
func joinPath(path []string) string {
	out := ""
	for i, p := range path {
		if i > 0 {
			out += "/"
		}
		out += p
	}
	return out
}

// pick returns a uniformly chosen element of ss, using the frozen Uint64n.
// It panics on an empty slice: an empty choice set is a construction error the
// caller must have prevented, and returning a zero value would emit an empty
// fault target.
func pick[T any](st *recorder.Stream, ss []T) T {
	if len(ss) == 0 {
		panic("search: pick from an empty set")
	}
	return ss[st.Uint64n(uint64(len(ss)))]
}

// pickIndex is pick for callers that need the index too.
func pickIndex[T any](st *recorder.Stream, ss []T) (int, T) {
	if len(ss) == 0 {
		panic("search: pick from an empty set")
	}
	i := int(st.Uint64n(uint64(len(ss))))
	return i, ss[i]
}
