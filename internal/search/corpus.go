package search

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/WrdCstlg/pro-thesis/internal/recorder"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// ---------------------------------------------------------------------------
// The corpus
//
// An energy-weighted archive of worlds. The base spec's search loop:
//
//	select(corpus)  energy-weighted, favouring cheap + recently novel
//	add             on positive coverage delta
//	cull            dominated worlds
//
// Three properties this implementation is built around:
//
//  1. EVERY ENTRY IS A .thesis WORLD WITH PROVENANCE. WorldMeta.Origin and
//     ParentHash are hash-excluded and omitempty (D-014), reserved in Phase 0
//     for exactly this. Phase 5 shrinks these worlds and I2 requires them
//     self-contained: a corpus that does not serialize is a corpus Phase 5
//     cannot use. Add REFUSES a world with no origin.
//
//  2. SELECTION IS INTEGER-WEIGHTED AND PATH-KEYED. Weights are uint32 and
//     selection goes through recorder.Stream.WeightedPPM, the frozen
//     deterministic weighted selector. No float arithmetic appears in any
//     weight: Go permits fusing floating-point operations, so a float energy
//     could select a different entry on a different architecture and A.8's
//     "same seed, same tree" would be false in a way nobody would ever find.
//
//  3. AN UNOBSERVED WORLD IS NEVER ADMITTED. A world that never reached DRIVE
//     produced a truncated boot log; folding that into coverage teaches the
//     search that failing to boot is novel, and the corpus then fills with
//     worlds that break the harness rather than the target. This is the concrete
//     form of the ranked #1 failure mode (see outcome.go).
// ---------------------------------------------------------------------------

// Entry is one archived world and what executing it established.
type Entry struct {
	// World is the archived world, normalized and stamped with provenance.
	World schema.World
	// Hash is World.Hash().
	Hash string

	// Outcome is what the world established, including its three-valued Signal.
	Outcome Outcome

	// Templates and States are the coverage ids this world hit, sorted. They are
	// what domination and the rare-template index are computed from.
	Templates []string
	States    []string

	// AddedSeq is the number of observed worlds the run had seen when this entry
	// was admitted.
	AddedSeq int
	// LastNovelSeq is the observed-world count at the moment this entry last
	// contributed new coverage. The recency term of the energy decays from it.
	//
	// It counts WORLDS OFFERED, not corpus entries. A counter based on corpus
	// size would move backwards when culling removed entries, so an entry could
	// grow younger, and the recency term would stop being a function of time.
	LastNovelSeq int
	// Selections counts how many times this entry has been chosen as a mutation
	// parent. It does not enter the energy: it is reported so a human can see
	// whether the search is grinding on one lineage.
	Selections int
}

// Origin is the entry's provenance.
func (e *Entry) Origin() string {
	if e.World.Meta == nil {
		return ""
	}
	return e.World.Meta.Origin
}

// ParentHash is the entry's lineage pointer.
func (e *Entry) ParentHash() string {
	if e.World.Meta == nil {
		return ""
	}
	return e.World.Meta.ParentHash
}

// FaultCount is the entry's schedule length.
func (e *Entry) FaultCount() int { return len(e.World.FaultSchedule.Planned) }

// Protected reports whether an entry may never be culled or evicted.
//
// A violating world is evidence and the search exists to produce it. A seeded
// or manual world is the floor of the search: culling the seed corpus would let
// a run drift away from the fault kinds it was configured to exercise, and a
// gate that quietly stopped testing a kind is the anti-gaming failure I6 exists
// to prevent.
func (e *Entry) Protected() bool {
	if e.Outcome.Signal() == SignalViolated {
		return true
	}
	switch e.Origin() {
	case OriginSeeded, OriginManual, OriginShrunk:
		return true
	}
	return false
}

// EnergyParams tunes the selection weight.
//
// Every field is an integer. See the file header for why.
type EnergyParams struct {
	// Unit is the full weight of an ideal entry.
	Unit uint64
	// CostRefMS is the world duration at and below which no cost penalty
	// applies. Set from the measured per-world cost (~30s wall on this host), so
	// an ordinary world is not penalised and only an unusually slow one is.
	CostRefMS int64
	// FaultRef damps the parsimony term: weight scales as FaultRef/(FaultRef+n).
	FaultRef int
	// RecencyRef damps the novelty-recency term: weight scales as
	// RecencyRef/(RecencyRef+age), where age is how many worlds have been added
	// since this entry last produced new coverage.
	RecencyRef int
	// RarePctPerHit is the percentage bonus per rare template this entry hits.
	RareBonusPct int
	// RareBonusCapPct caps the total rare bonus.
	RareBonusCapPct int
	// UnknownPenaltyPct reduces the weight of an entry whose signal is unknown.
	//
	// It is a penalty and not an exclusion. An unevaluable world's COVERAGE is
	// real (those log lines were genuinely emitted) so it is still worth
	// mutating from. What is not real is any claim that its faults were
	// survivable, and nothing in the energy makes such a claim.
	UnknownPenaltyPct int
}

// DefaultEnergyParams is the compiled-in tuning.
func DefaultEnergyParams() EnergyParams {
	return EnergyParams{
		Unit:              1_000_000,
		CostRefMS:         30_000,
		FaultRef:          4,
		RecencyRef:        8,
		RareBonusPct:      25,
		RareBonusCapPct:   200,
		UnknownPenaltyPct: 40,
	}
}

// Corpus is the archive.
type Corpus struct {
	params EnergyParams
	// entries are held in HASH order, so every derived slice, every weight
	// vector and every selection is independent of insertion order.
	entries []*Entry
	byHash  map[string]*Entry

	// global is the run's cumulative coverage.
	global *Coverage
	// templateWorlds counts how many admitted worlds hit each template id. It is
	// the rare-event index.
	templateWorlds map[string]int
	// worlds counts every OBSERVED world offered, admitted or not. It is the
	// denominator of "hit in <1% of worlds" and the clock the recency term
	// decays against.
	worlds int

	// rareCache memoizes the rare-template set for the current value of worlds.
	rareCache       map[string]bool
	rareCacheWorlds int

	// maxEntries bounds the archive. Zero means DefaultMaxEntries.
	maxEntries int
}

// DefaultMaxEntries bounds the corpus.
//
// An 8-hour soak profile runs worlds until its budget expires, and an unbounded
// corpus would make every selection linear in a set that never stops growing.
// Eviction is by lowest energy among unprotected entries, so what is lost is
// what the search had already stopped choosing.
const DefaultMaxEntries = 512

// NewCorpus returns an empty corpus.
func NewCorpus(params EnergyParams) *Corpus {
	if params.Unit == 0 {
		params = DefaultEnergyParams()
	}
	return &Corpus{
		params:         params,
		byHash:         map[string]*Entry{},
		global:         NewCoverage(),
		templateWorlds: map[string]int{},
		maxEntries:     DefaultMaxEntries,
	}
}

// SetMaxEntries bounds the archive. A value below 1 restores the default.
func (c *Corpus) SetMaxEntries(n int) {
	if n < 1 {
		n = DefaultMaxEntries
	}
	c.maxEntries = n
}

// Len is the number of archived entries.
func (c *Corpus) Len() int { return len(c.entries) }

// Worlds is the number of observed worlds offered to the corpus.
func (c *Corpus) Worlds() int { return c.worlds }

// Global is the run's cumulative coverage.
func (c *Corpus) Global() *Coverage { return c.global }

// Entries returns the archive in hash order.
func (c *Corpus) Entries() []*Entry {
	out := make([]*Entry, len(c.entries))
	copy(out, c.entries)
	return out
}

// Get returns the entry with this world hash.
func (c *Corpus) Get(hash string) (*Entry, bool) {
	e, ok := c.byHash[hash]
	return e, ok
}

// AddResult says what Add did and why, so a search log can explain itself.
type AddResult struct {
	Added bool
	// Entry is the admitted entry, or the existing one on a duplicate.
	Entry *Entry
	// Delta is the coverage this world contributed to the global set.
	Delta Delta
	// Reason explains a refusal, or names the admission criterion.
	Reason string
	// Culled is how many entries this admission made redundant.
	Culled int
}

// Add offers an executed world to the corpus.
//
// Admission rules, in order:
//
//  1. The world must validate and must carry meta.origin. A corpus entry that
//     cannot say where it came from is one Phase 5 cannot audit.
//  2. The world must have been OBSERVED. An unobserved world's logs are a
//     truncated boot, not evidence about the system.
//  3. A duplicate hash updates the existing entry's outcome and coverage
//     bookkeeping and is not re-admitted.
//  4. Otherwise it is admitted if it contributed new coverage, OR if its signal
//     is violated. A violating world is admitted even with zero coverage delta:
//     it is the artifact the whole tool exists to produce and Phase 5 shrinks
//     it.
//
// The world's coverage is scored against the global set BEFORE being merged
// into it. Merging first would make every delta zero.
func (c *Corpus) Add(w schema.World, cov *Coverage, out Outcome) (AddResult, error) {
	norm := w.Normalized()
	if err := ValidateEntryWorld(&norm); err != nil {
		return AddResult{Reason: "invalid world"}, err
	}
	hash, err := norm.Hash()
	if err != nil {
		return AddResult{}, fmt.Errorf("search: hash world: %w", err)
	}
	out.WorldHash = hash
	out.Faults = append([]string(nil), norm.FaultSchedule.Planned...)

	if !out.Observed {
		return AddResult{
			Reason: "not admitted: the world was never observed, so its coverage is not evidence " +
				"about the system (" + out.Why() + ")",
		}, nil
	}

	c.worlds++
	delta := cov.Against(c.global)
	out.Coverage = delta

	templates := idsOf(cov, true)
	states := idsOf(cov, false)
	out.Templates = templates
	out.States = states

	// Fold into the global sets and the rare-template index. This happens for
	// every observed world, admitted or not: coverage is a fact about the run,
	// and an unadmitted world's templates still make a template less rare.
	c.global.Merge(cov)
	for _, id := range templates {
		c.templateWorlds[id]++
	}

	if existing, ok := c.byHash[hash]; ok {
		existing.Outcome = out
		existing.Templates = templates
		existing.States = states
		if delta.Positive() {
			existing.LastNovelSeq = c.worlds
		}
		return AddResult{Entry: existing, Delta: delta, Reason: "already in the corpus"}, nil
	}

	violated := out.Signal() == SignalViolated
	if !delta.Positive() && !violated {
		return AddResult{
			Delta:  delta,
			Reason: "not admitted: no new coverage and no violation",
		}, nil
	}

	e := &Entry{
		World:        norm,
		Hash:         hash,
		Outcome:      out,
		Templates:    templates,
		States:       states,
		AddedSeq:     c.worlds,
		LastNovelSeq: c.worlds,
	}
	c.insert(e)

	reason := "admitted: new coverage"
	if violated && !delta.Positive() {
		reason = "admitted: a violation, with no new coverage"
	} else if violated {
		reason = "admitted: a violation, with new coverage"
	}

	culled := c.Cull()
	c.evictToLimit()
	return AddResult{Added: true, Entry: e, Delta: delta, Reason: reason, Culled: culled}, nil
}

// insert places e in hash order.
func (c *Corpus) insert(e *Entry) {
	i := sort.Search(len(c.entries), func(i int) bool { return c.entries[i].Hash >= e.Hash })
	c.entries = append(c.entries, nil)
	copy(c.entries[i+1:], c.entries[i:])
	c.entries[i] = e
	c.byHash[e.Hash] = e
}

// idsOf projects a coverage set to sorted ids.
func idsOf(cov *Coverage, templates bool) []string {
	if cov == nil {
		return []string{}
	}
	if templates {
		if cov.Templates == nil {
			return []string{}
		}
		return cov.Templates.IDs()
	}
	if cov.States == nil {
		return []string{}
	}
	return cov.States.IDs()
}

// ---------------------------------------------------------------------------
// Energy and selection
// ---------------------------------------------------------------------------

// Energy is the entry's selection weight.
//
// It favours worlds that are FAST, RECENTLY NOVEL and use FEW FAULTS, and lifts
// worlds that hit rare templates. The computation is integer-only and the order
// of operations is fixed, so the value is reproducible on any architecture.
//
// It NEVER returns zero. A zero-weight entry can never be selected, and a
// corpus whose weights all reached zero would make WeightedPPM panic with a
// zero total: turning an ordinary long run into a crash. The floor is 1.
func (c *Corpus) Energy(e *Entry) uint32 {
	p := c.params
	if p.Unit == 0 {
		p = DefaultEnergyParams()
	}
	v := p.Unit

	// Cost: cheap is better, no bonus for being faster than the reference.
	dur := e.Outcome.DurationMS
	if dur > p.CostRefMS && p.CostRefMS > 0 {
		v = v * uint64(p.CostRefMS) / uint64(dur)
	}

	// Parsimony: few faults is better.
	if p.FaultRef > 0 {
		v = v * uint64(p.FaultRef) / uint64(p.FaultRef+e.FaultCount())
	}

	// Recency of novelty: an entry that has produced nothing new for many worlds
	// decays.
	if p.RecencyRef > 0 {
		age := c.worlds - e.LastNovelSeq
		if age < 0 {
			age = 0
		}
		v = v * uint64(p.RecencyRef) / uint64(p.RecencyRef+age)
	}

	// Rare-event bias.
	if bonus := c.rareBonusPct(e); bonus > 0 {
		v = v * uint64(100+bonus) / 100
	}

	// An unevaluable world is worth mutating from but is not worth as much as
	// one that actually told us something.
	if e.Outcome.Signal() == SignalUnknown && p.UnknownPenaltyPct > 0 {
		pct := p.UnknownPenaltyPct
		if pct > 100 {
			pct = 100
		}
		v = v * uint64(100-pct) / 100
	}

	if v < 1 {
		return 1
	}
	const maxWeight = uint64(1) << 31
	if v > maxWeight {
		v = maxWeight
	}
	return uint32(v)
}

// rareSet is the memoized rare-template set.
//
// Memoized because Energy is called once per entry per selection, and
// recomputing a sorted rare set inside each call made selection quadratic in
// the corpus size for no gain: the set can only change when a world is
// offered, which is exactly what the cache key tracks.
func (c *Corpus) rareSet() map[string]bool {
	if c.rareCache != nil && c.rareCacheWorlds == c.worlds {
		return c.rareCache
	}
	rare := c.RareTemplates(DefaultRareFraction)
	set := make(map[string]bool, len(rare))
	for _, id := range rare {
		set[id] = true
	}
	c.rareCache = set
	c.rareCacheWorlds = c.worlds
	return set
}

// rareBonusPct is the entry's rare-template bonus, capped.
func (c *Corpus) rareBonusPct(e *Entry) int {
	if c.params.RareBonusPct <= 0 {
		return 0
	}
	set := c.rareSet()
	if len(set) == 0 {
		return 0
	}
	hits := 0
	for _, id := range e.Templates {
		if set[id] {
			hits++
		}
	}
	bonus := hits * c.params.RareBonusPct
	if capPct := c.params.RareBonusCapPct; capPct > 0 && bonus > capPct {
		bonus = capPct
	}
	return bonus
}

// DefaultRareFraction is the "<1% of worlds" threshold the base spec names.
const DefaultRareFraction = 0.01

// MinWorldsForRarity is the number of observed worlds below which nothing is
// called rare.
//
// One hundred, and the number follows from the spec's own fraction rather than
// from taste: "hit in <1% of worlds" is a claim about a count, and below a
// hundred worlds "<1%" means "hit by fewer than one world", which nothing
// satisfies. Declaring anything rare before that point would be arithmetic on a
// denominator too small to carry the claim, and a bias that applies to
// everything is not a bias.
const MinWorldsForRarity = 100

// minRareThreshold is the floor on the rare-template count threshold.
//
// TWO, and this is a deliberate departure from the literal reading, recorded
// rather than slipped in. Taken literally, "hit by strictly fewer than
// 0.01 x worlds" is satisfied by nothing at all until 200 worlds: at 150 worlds
// the threshold floors to 1, and no observed template is hit by fewer than one
// world. Phase 4's measured rollout budget is around 62 worlds at four-way
// parallelism (OQ-013), so the literal rule would make the rare-event bias
// permanently inert in exactly the runs it was written for.
//
// A floor of two means "hit by exactly one world" counts as rare from
// MinWorldsForRarity upward (which at 100 worlds IS one per cent, the spec's
// own number at the first scale where it is expressible) and from 200 worlds
// onward the literal fraction takes over unchanged. The rule is therefore
// stricter or equal to the spec everywhere the spec is meaningful, and defined
// where the spec is degenerate.
const minRareThreshold = 2

// RareTemplates returns the template ids hit by fewer than frac of the observed
// worlds, sorted.
func (c *Corpus) RareTemplates(frac float64) []string {
	if c.worlds < MinWorldsForRarity || frac <= 0 {
		return nil
	}
	// Integer threshold: a template is rare when it was hit by strictly fewer
	// worlds than this. The one floating-point operation is here, on a count of
	// worlds rather than on a selection weight; its result is an int and every
	// comparison below is integer, so no float reaches a selection decision.
	threshold := int(frac * float64(c.worlds))
	if threshold < minRareThreshold {
		threshold = minRareThreshold
	}
	out := make([]string, 0, 8)
	for id, n := range c.templateWorlds {
		if n < threshold {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// ErrEmptyCorpus reports a selection from an empty corpus.
var ErrEmptyCorpus = errors.New("search: the corpus is empty")

// Select draws an entry, weighted by energy.
//
// The draw is recorder.Stream.WeightedPPM over the hash-ordered entry list, so
// the same seed selects the same entry from the same corpus regardless of the
// order worlds were admitted in or how Go decided to iterate a map.
func (c *Corpus) Select(st *recorder.Stream) (*Entry, error) {
	if len(c.entries) == 0 {
		return nil, ErrEmptyCorpus
	}
	weights := make([]uint32, len(c.entries))
	for i, e := range c.entries {
		weights[i] = c.Energy(e)
	}
	i := st.WeightedPPM(weights)
	c.entries[i].Selections++
	return c.entries[i], nil
}

// ---------------------------------------------------------------------------
// Culling
// ---------------------------------------------------------------------------

// Dominates reports whether a makes b redundant.
//
// Pareto domination on the four axes the search actually trades between:
// coverage (both sets), cost, fault count, and signal strength. No weights are
// involved, so culling does not depend on the utility function, which lives in
// internal/search/saboteur and which a strategy may legitimately configure
// differently.
//
// It is strict: a must be at least as good on EVERY axis and different on at
// least the coverage or the cost. Two identical entries do not dominate each
// other, or culling would delete both.
func Dominates(a, b *Entry) bool {
	if a == nil || b == nil || a.Hash == b.Hash {
		return false
	}
	if !covers(a.Templates, b.Templates) || !covers(a.States, b.States) {
		return false
	}
	if a.FaultCount() > b.FaultCount() {
		return false
	}
	if a.Outcome.DurationMS > b.Outcome.DurationMS {
		return false
	}
	if a.Outcome.Signal().Rank() < b.Outcome.Signal().Rank() {
		return false
	}
	// Something must be strictly better, or a and b are interchangeable.
	return len(a.Templates) > len(b.Templates) ||
		len(a.States) > len(b.States) ||
		a.FaultCount() < b.FaultCount() ||
		a.Outcome.DurationMS < b.Outcome.DurationMS ||
		a.Outcome.Signal().Rank() > b.Outcome.Signal().Rank()
}

// covers reports whether the sorted set a contains every element of the sorted
// set b.
func covers(a, b []string) bool {
	i := 0
	for _, x := range b {
		for i < len(a) && a[i] < x {
			i++
		}
		if i >= len(a) || a[i] != x {
			return false
		}
	}
	return true
}

// Cull removes dominated entries and returns how many were removed.
//
// A protected entry is never culled: see Entry.Protected.
func (c *Corpus) Cull() int {
	if len(c.entries) < 2 {
		return 0
	}
	drop := make(map[string]bool)
	for _, b := range c.entries {
		if b.Protected() {
			continue
		}
		for _, a := range c.entries {
			if drop[a.Hash] {
				// Do not let an already-condemned entry condemn another: that
				// would make the result depend on iteration order.
				continue
			}
			if Dominates(a, b) {
				drop[b.Hash] = true
				break
			}
		}
	}
	if len(drop) == 0 {
		return 0
	}
	keep := c.entries[:0]
	for _, e := range c.entries {
		if drop[e.Hash] {
			delete(c.byHash, e.Hash)
			continue
		}
		keep = append(keep, e)
	}
	c.entries = keep
	return len(drop)
}

// evictToLimit trims the corpus to maxEntries by dropping the lowest-energy
// unprotected entries, breaking ties by hash so eviction is deterministic.
func (c *Corpus) evictToLimit() {
	limit := c.maxEntries
	if limit < 1 {
		limit = DefaultMaxEntries
	}
	if len(c.entries) <= limit {
		return
	}
	type cand struct {
		e      *Entry
		energy uint32
	}
	cands := make([]cand, 0, len(c.entries))
	for _, e := range c.entries {
		if e.Protected() {
			continue
		}
		cands = append(cands, cand{e: e, energy: c.Energy(e)})
	}
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].energy != cands[j].energy {
			return cands[i].energy < cands[j].energy
		}
		return cands[i].e.Hash < cands[j].e.Hash
	})
	need := len(c.entries) - limit
	drop := make(map[string]bool, need)
	for i := 0; i < len(cands) && len(drop) < need; i++ {
		drop[cands[i].e.Hash] = true
	}
	if len(drop) == 0 {
		return
	}
	keep := c.entries[:0]
	for _, e := range c.entries {
		if drop[e.Hash] {
			delete(c.byHash, e.Hash)
			continue
		}
		keep = append(keep, e)
	}
	c.entries = keep
}

// ---------------------------------------------------------------------------
// Serialization
// ---------------------------------------------------------------------------

// CorpusExt is the extension of an archived corpus world.
const CorpusExt = ".thesis"

// Save writes every entry to dir as a content-addressed .thesis file.
//
// What is archived is the WORLD and its PROVENANCE: precisely what Phase 5
// needs to shrink an entry and what invariant I2 requires to be self-contained.
// Run-local statistics (energy, selection counts, coverage ids) are NOT
// archived: they are properties of one run's execution, and writing them beside
// a world would invite a later phase to read a stale energy as if it were a
// fact about the world.
//
// It uses recorder.StoreWorldNoClobber, so two different worlds that share a
// 4-hex short id are an error rather than a silent deletion.
func (c *Corpus) Save(dir string) ([]string, error) {
	if dir == "" {
		return nil, errors.New("search: Corpus.Save needs a directory")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("search: create corpus dir %s: %w", dir, err)
	}
	paths := make([]string, 0, len(c.entries))
	for _, e := range c.entries {
		w := e.World
		p, err := recorder.StoreWorldNoClobber(dir, &w)
		if err != nil {
			return paths, fmt.Errorf("search: store corpus world %s: %w", e.Hash, err)
		}
		paths = append(paths, p)
	}
	sort.Strings(paths)
	return paths, nil
}

// LoadCorpusWorlds reads every .thesis file in dir.
//
// Each is decoded through recorder.LoadWorld, which re-encodes and byte-compares
// on every call, so a hand-edited or CRLF-translated corpus world is refused
// here rather than silently participating in a search.
//
// Worlds are returned in filename order and every one is required to carry
// provenance: a corpus file with no origin is a file somebody dropped in the
// directory, and treating it as a seed would let an unreviewed world into the
// search.
func LoadCorpusWorlds(dir string) ([]schema.World, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("search: read corpus dir %s: %w", dir, err)
	}
	names := make([]string, 0, len(ents))
	for _, de := range ents {
		if de.IsDir() || !strings.HasSuffix(de.Name(), CorpusExt) {
			continue
		}
		names = append(names, de.Name())
	}
	sort.Strings(names)

	out := make([]schema.World, 0, len(names))
	for _, n := range names {
		p := filepath.Join(dir, n)
		w, err := recorder.LoadWorld(p)
		if err != nil {
			return nil, err
		}
		if err := ValidateEntryWorld(w); err != nil {
			return nil, fmt.Errorf("search: corpus world %s: %w", p, err)
		}
		out = append(out, *w)
	}
	return out, nil
}
