package search

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/WrdCstlg/pro-thesis/internal/recorder"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// corpusWorld builds a distinct, valid world with the given schedule.
func corpusWorld(t *testing.T, seed uint64, origin, parent string, planned ...string) schema.World {
	t.Helper()
	w := testBaseWorld()
	w.Seed = seed
	w.FaultSchedule.Planned = append([]string(nil), planned...)
	w.Meta = &schema.WorldMeta{Origin: origin, ParentHash: parent}
	w = w.Normalized()
	if err := w.Validate(); err != nil {
		t.Fatalf("built an invalid world: %v", err)
	}
	return w
}

func observedOutcome(durationMS int64, findings ...Finding) Outcome {
	return Outcome{Observed: true, DurationMS: durationMS, Findings: findings}
}

// ---------------------------------------------------------------------------
// Admission
// ---------------------------------------------------------------------------

// TestAnUnobservedWorldIsNeverAdmitted is the corpus half of the anti-noise
// rule. A world that died in BOOT produced a truncated boot log; folding that
// into coverage would teach the search that failing to boot is novel, and the
// corpus would fill with worlds that break the harness rather than the target.
func TestAnUnobservedWorldIsNeverAdmitted(t *testing.T) {
	c := NewCorpus(DefaultEnergyParams())
	w := corpusWorld(t, 1, OriginSeeded, "", "proc.pause(kv-n1)@3000..4000")
	out := Outcome{Observed: false, Err: "compose up failed"}

	res, err := c.Add(w, coverageOf("level=info event=boot node=kv-n1"), out)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if res.Added {
		t.Fatal("an unobserved world was admitted to the corpus")
	}
	if c.Len() != 0 {
		t.Fatalf("corpus has %d entries after refusing the world", c.Len())
	}
	if c.Global().Templates.Len() != 0 {
		t.Fatal("an unobserved world's log templates were folded into global coverage")
	}
	if c.Worlds() != 0 {
		t.Fatalf("an unobserved world counted toward the rare-template denominator (%d)", c.Worlds())
	}
	if !strings.Contains(res.Reason, "never observed") {
		t.Fatalf("Reason = %q, want it to say the world was never observed", res.Reason)
	}
}

func TestAWorldWithNoProvenanceIsRefused(t *testing.T) {
	c := NewCorpus(DefaultEnergyParams())
	w := testBaseWorld()
	w.FaultSchedule.Planned = []string{"proc.pause(kv-n1)@3000..4000"}
	w = w.Normalized() // no Meta at all

	_, err := c.Add(w, coverageOf("x"), observedOutcome(1000, okFinding("no_crash", schema.ClassCrash)))
	if err == nil {
		t.Fatal("a world with no meta.origin was accepted; Phase 5 could not audit it")
	}
	if !strings.Contains(err.Error(), "origin") {
		t.Fatalf("error = %v, want it to name the missing origin", err)
	}
}

func TestAdmissionRequiresNewCoverageOrAViolation(t *testing.T) {
	c := NewCorpus(DefaultEnergyParams())
	lines := []string{
		"level=info event=boot node=kv-n1 term=1",
		"level=info event=elected node=kv-n1 term=2",
	}

	first := corpusWorld(t, 1, OriginSeeded, "", "proc.pause(kv-n1)@3000..4000")
	res, err := c.Add(first, coverageOf(lines...), observedOutcome(30000, okFinding("no_crash", schema.ClassCrash)))
	if err != nil || !res.Added {
		t.Fatalf("first world: added=%v err=%v", res.Added, err)
	}

	// Same coverage, no violation: nothing new to learn.
	second := corpusWorld(t, 2, OriginSeeded, "", "proc.pause(kv-n2)@3000..4000")
	res, err = c.Add(second, coverageOf(lines...), observedOutcome(30000, okFinding("no_crash", schema.ClassCrash)))
	if err != nil {
		t.Fatalf("second world: %v", err)
	}
	if res.Added {
		t.Fatal("a world with no new coverage and no violation was admitted")
	}

	// Same coverage, but it found something. A violating world is the artifact
	// the tool exists to produce and Phase 5 shrinks it.
	third := corpusWorld(t, 3, OriginSeeded, "", "proc.pause(kv-n3)@3000..4000")
	res, err = c.Add(third, coverageOf(lines...),
		observedOutcome(30000, violatedFinding("linearizable.kv", schema.ClassConsistency, schema.SeverityHigh)))
	if err != nil {
		t.Fatalf("third world: %v", err)
	}
	if !res.Added {
		t.Fatalf("a violating world with no new coverage was refused: %s", res.Reason)
	}
}

// TestCoverageIsScoredBeforeItIsMerged: merging first would make every delta
// zero and the corpus would never admit anything.
func TestCoverageIsScoredBeforeItIsMerged(t *testing.T) {
	c := NewCorpus(DefaultEnergyParams())
	w := corpusWorld(t, 1, OriginSeeded, "", "proc.pause(kv-n1)@3000..4000")
	res, err := c.Add(w, coverageOf("a=1", "b=2", "c=3"),
		observedOutcome(30000, okFinding("no_crash", schema.ClassCrash)))
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if res.Delta.NewTemplates != 3 {
		t.Fatalf("first world's delta = %d templates, want 3", res.Delta.NewTemplates)
	}
	if c.Global().Templates.Len() != 3 {
		t.Fatalf("global coverage = %d, want 3 after the merge", c.Global().Templates.Len())
	}
}

// ---------------------------------------------------------------------------
// Energy
// ---------------------------------------------------------------------------

func TestEnergyFavoursCheapRecentAndParsimonious(t *testing.T) {
	c := NewCorpus(DefaultEnergyParams())

	base := &Entry{Hash: "h1", Outcome: observedOutcome(30000, okFinding("no_crash", schema.ClassCrash))}
	base.World = corpusWorld(t, 1, OriginSeeded, "", "proc.pause(kv-n1)@3000..4000")

	slow := *base
	slow.Hash = "h2"
	slow.Outcome.DurationMS = 120000

	many := *base
	many.Hash = "h3"
	many.World = corpusWorld(t, 2, OriginSeeded, "",
		"proc.pause(kv-n1)@3000..4000", "net.latency(kv-n2, mean=50, jitter=10)@3000..4000",
		"net.loss(kv-n3, pct=5)@3000..4000", "proc.kill(kv-n1, signal=SIGKILL)@6000..6500")

	stale := *base
	stale.Hash = "h4"
	stale.LastNovelSeq = -20 // twenty worlds ago

	unknown := *base
	unknown.Hash = "h5"
	unknown.Outcome.Findings = []Finding{inconclusiveFinding("no_stuck_op", schema.ClassLiveness)}

	b := c.Energy(base)
	for _, tc := range []struct {
		name string
		e    *Entry
	}{
		{"a slower world", &slow},
		{"a world with more faults", &many},
		{"a world that has produced nothing new for twenty worlds", &stale},
		{"a world whose oracles could not answer", &unknown},
	} {
		if got := c.Energy(tc.e); got >= b {
			t.Errorf("%s has energy %d, not below the reference %d", tc.name, got, b)
		}
	}
}

// TestEnergyIsNeverZero: a zero-weight entry can never be selected, and a
// corpus whose weights all reached zero makes WeightedPPM panic on a zero
// total: an ordinary long run would become a crash.
func TestEnergyIsNeverZero(t *testing.T) {
	c := NewCorpus(DefaultEnergyParams())
	e := &Entry{
		Hash:         "h",
		World:        corpusWorld(t, 1, OriginSeeded, "", "proc.pause(kv-n1)@3000..4000"),
		Outcome:      Outcome{Observed: true, DurationMS: 1 << 40},
		LastNovelSeq: -1_000_000,
	}
	if got := c.Energy(e); got < 1 {
		t.Fatalf("energy = %d; a corpus entry must always stay selectable", got)
	}
}

// TestEnergyIsIntegerAndArchitectureStable: the value must be a pure function
// of the entry, computed the same way every time. Go permits fusing
// floating-point operations, so a float energy could select a different entry
// on a different architecture and A.8's "same seed, same tree" would quietly
// become false.
func TestEnergyIsDeterministic(t *testing.T) {
	c := NewCorpus(DefaultEnergyParams())
	e := &Entry{
		Hash:    "h",
		World:   corpusWorld(t, 1, OriginSeeded, "", "proc.pause(kv-n1)@3000..4000"),
		Outcome: observedOutcome(45000, okFinding("no_crash", schema.ClassCrash)),
	}
	first := c.Energy(e)
	for i := 0; i < 64; i++ {
		if got := c.Energy(e); got != first {
			t.Fatalf("energy is not a pure function: %d then %d", first, got)
		}
	}
}

// ---------------------------------------------------------------------------
// Selection determinism
// ---------------------------------------------------------------------------

// buildSelectionCorpus fills a corpus with five distinguishable worlds.
func buildSelectionCorpus(t *testing.T, reverse bool) *Corpus {
	t.Helper()
	c := NewCorpus(DefaultEnergyParams())
	type spec struct {
		seed  uint64
		lines []string
		dur   int64
	}
	specs := []spec{
		{1, []string{"a=1"}, 20000},
		{2, []string{"b=1"}, 30000},
		{3, []string{"c=1"}, 40000},
		{4, []string{"d=1"}, 50000},
		{5, []string{"e=1"}, 60000},
	}
	if reverse {
		for i, j := 0, len(specs)-1; i < j; i, j = i+1, j-1 {
			specs[i], specs[j] = specs[j], specs[i]
		}
	}
	for _, s := range specs {
		w := corpusWorld(t, s.seed, OriginSeeded, "",
			fmt.Sprintf("proc.pause(kv-n%d)@3000..4000", (s.seed%3)+1))
		if _, err := c.Add(w, coverageOf(s.lines...),
			observedOutcome(s.dur, okFinding("no_crash", schema.ClassCrash))); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	return c
}

func selectionSequence(t *testing.T, c *Corpus, seed uint64, n int) []string {
	t.Helper()
	st := NewStreams(recorder.Seed(seed)).Get(PathCorpusSelect)
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		e, err := c.Select(st)
		if err != nil {
			t.Fatalf("Select: %v", err)
		}
		out = append(out, e.Hash)
	}
	return out
}

// TestSelectionIsDeterministicUnderAFixedSeed is A.8's requirement made
// checkable: the same seed against the same corpus must draw the same sequence,
// every time, in every process.
//
// The repetition is not padding. Go randomizes map iteration order per range
// statement, so a selection that leaked map order would agree with itself on
// some runs and not others; forty draws repeated eight times makes that visible
// rather than flaky-in-CI.
func TestSelectionIsDeterministicUnderAFixedSeed(t *testing.T) {
	want := selectionSequence(t, buildSelectionCorpus(t, false), 0xDEADBEEF, 40)
	for i := 0; i < 8; i++ {
		got := selectionSequence(t, buildSelectionCorpus(t, false), 0xDEADBEEF, 40)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("selection is not reproducible on repeat %d:\n  %v\n  %v", i, want, got)
		}
	}
	other := selectionSequence(t, buildSelectionCorpus(t, false), 0xFEEDFACE, 40)
	if strings.Join(other, ",") == strings.Join(want, ",") {
		t.Fatal("two different seeds produced the same selection sequence; the seed is not reaching the draw")
	}
}

// TestTheWeightVectorIsAlignedByHashNotByInsertionOrder.
//
// Selection is a WeightedPPM draw over c.entries, so the draw is only
// reproducible if position i means the same entry in every process. The corpus
// therefore keeps entries in hash order. Recency legitimately depends on WHEN a
// world arrived (that is what "recently novel" means) but the ORDER of the
// vector must not.
func TestTheWeightVectorIsAlignedByHashNotByInsertionOrder(t *testing.T) {
	forward := buildSelectionCorpus(t, false).Entries()
	backward := buildSelectionCorpus(t, true).Entries()
	if len(forward) != len(backward) {
		t.Fatalf("corpus sizes differ: %d vs %d", len(forward), len(backward))
	}
	for i := range forward {
		if forward[i].Hash != backward[i].Hash {
			t.Fatalf("entry %d is %s forward and %s backward; the weight vector is not hash-aligned",
				i, forward[i].Hash, backward[i].Hash)
		}
		if i > 0 && forward[i-1].Hash >= forward[i].Hash {
			t.Fatalf("entries are not hash-sorted at %d: %s then %s", i, forward[i-1].Hash, forward[i].Hash)
		}
	}
}

// TestRecencyDecaysWithWorldsOfferedNotCorpusSize.
//
// The recency clock must be monotone. A clock based on corpus SIZE moves
// backwards when culling removes entries, so an entry would grow younger and
// the "recently novel" term would stop being a function of time.
func TestRecencyDecaysWithWorldsOfferedNotCorpusSize(t *testing.T) {
	c := NewCorpus(DefaultEnergyParams())
	first := corpusWorld(t, 1, OriginSeeded, "", "proc.pause(kv-n1)@3000..4000")
	if _, err := c.Add(first, coverageOf("a=1"),
		observedOutcome(30000, okFinding("no_crash", schema.ClassCrash))); err != nil {
		t.Fatalf("Add: %v", err)
	}
	e, ok := c.Get(mustHash(t, first))
	if !ok {
		t.Fatal("the first world was not admitted")
	}
	before := c.Energy(e)

	// Offer thirty worlds that add nothing. None is admitted, so the corpus size
	// never moves; only the world count does.
	for i := 0; i < 30; i++ {
		w := corpusWorld(t, uint64(100+i), OriginSeeded, "",
			fmt.Sprintf("proc.pause(kv-n%d)@%d..%d", (i%3)+1, 3000+i, 4000+i))
		if _, err := c.Add(w, coverageOf("a=1"),
			observedOutcome(30000, okFinding("no_crash", schema.ClassCrash))); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	after := c.Energy(e)
	if after >= before {
		t.Fatalf("energy did not decay across 30 worlds with no novelty: %d then %d", before, after)
	}
	if c.Len() != 1 {
		t.Fatalf("corpus grew to %d entries; the test's premise is broken", c.Len())
	}
}

func mustHash(t *testing.T, w schema.World) string {
	t.Helper()
	h, err := w.Hash()
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	return h
}

func TestSelectFromAnEmptyCorpusIsAnError(t *testing.T) {
	c := NewCorpus(DefaultEnergyParams())
	st := NewStreams(recorder.Seed(1)).Get(PathCorpusSelect)
	if _, err := c.Select(st); err == nil {
		t.Fatal("Select from an empty corpus returned no error")
	}
}

// ---------------------------------------------------------------------------
// Domination and culling
// ---------------------------------------------------------------------------

func TestDominationRequiresBeingAtLeastAsGoodOnEveryAxis(t *testing.T) {
	mk := func(hash string, templates []string, faults int, dur int64, sig Signal) *Entry {
		planned := make([]string, 0, faults)
		for i := 0; i < faults; i++ {
			planned = append(planned, fmt.Sprintf("net.latency(kv-n1, mean=%d, jitter=0)@%d..%d",
				50*(i+1), 3000+i*100, 4000+i*100))
		}
		e := &Entry{Hash: hash, Templates: templates, World: corpusWorld(t, uint64(faults+1), OriginMutated, "", planned...)}
		e.Outcome = Outcome{Observed: true, DurationMS: dur}
		switch sig {
		case SignalClean:
			e.Outcome.Findings = []Finding{okFinding("no_crash", schema.ClassCrash)}
		case SignalViolated:
			e.Outcome.Findings = []Finding{violatedFinding("no_crash", schema.ClassCrash, schema.SeverityHigh)}
		}
		return e
	}

	big := mk("a", []string{"t1", "t2", "t3"}, 1, 20000, SignalClean)
	small := mk("b", []string{"t1", "t2"}, 1, 20000, SignalClean)
	if !Dominates(big, small) {
		t.Fatal("a strictly larger template set with equal cost did not dominate")
	}
	if Dominates(small, big) {
		t.Fatal("a strictly smaller template set dominated a larger one")
	}
	if Dominates(big, big) {
		t.Fatal("an entry dominated itself; culling would delete it")
	}

	cheaperButFewer := mk("c", []string{"t1"}, 1, 5000, SignalClean)
	if Dominates(cheaperButFewer, big) {
		t.Fatal("a cheaper world with less coverage dominated a more covering one")
	}

	// An unknown world can never dominate a clean one: it establishes less.
	unknown := mk("d", []string{"t1", "t2", "t3"}, 1, 10000, SignalUnknown)
	if Dominates(unknown, small) {
		t.Fatal("a world whose oracles could not answer dominated one that reported clean")
	}
}

func TestCullNeverRemovesAViolationOrASeed(t *testing.T) {
	c := NewCorpus(DefaultEnergyParams())

	// A rich, cheap, single-fault clean world that dominates almost anything.
	rich := corpusWorld(t, 1, OriginMutated, "", "net.latency(kv-n1, mean=50, jitter=10)@3000..4000")
	if _, err := c.Add(rich, coverageOf("a=1", "b=1", "c=1"),
		observedOutcome(5000, okFinding("no_crash", schema.ClassCrash))); err != nil {
		t.Fatalf("Add rich: %v", err)
	}

	// A poor, slow, many-fault world that found something.
	poor := corpusWorld(t, 2, OriginMutated, "",
		"net.partition(role:leader)@8200..13500", "proc.pause(role:leader)@8300..11000")
	if _, err := c.Add(poor, coverageOf("a=1"),
		observedOutcome(60000, violatedFinding("linearizable.kv", schema.ClassConsistency, schema.SeverityHigh))); err != nil {
		t.Fatalf("Add poor: %v", err)
	}

	// A poor, slow seed world with nothing to recommend it except its origin.
	seed := corpusWorld(t, 3, OriginSeeded, "", "proc.kill(kv-n1, signal=SIGKILL)@9000..9500")
	if _, err := c.Add(seed, coverageOf("a=1", "z=1"),
		observedOutcome(90000, okFinding("no_crash", schema.ClassCrash))); err != nil {
		t.Fatalf("Add seed: %v", err)
	}

	c.Cull()

	poorHash, _ := poor.Hash()
	seedHash, _ := seed.Hash()
	if _, ok := c.Get(poorHash); !ok {
		t.Fatal("culling removed a violating world; that is the artifact the tool exists to produce")
	}
	if _, ok := c.Get(seedHash); !ok {
		t.Fatal("culling removed a seeded world; the seed sweep is the floor of the search")
	}
}

func TestCullRemovesADominatedWorld(t *testing.T) {
	c := NewCorpus(DefaultEnergyParams())

	weak := corpusWorld(t, 1, OriginMutated, "",
		"net.latency(kv-n1, mean=50, jitter=10)@3000..4000",
		"net.loss(kv-n2, pct=5)@5000..6000")
	if _, err := c.Add(weak, coverageOf("a=1"),
		observedOutcome(60000, okFinding("no_crash", schema.ClassCrash))); err != nil {
		t.Fatalf("Add weak: %v", err)
	}
	strong := corpusWorld(t, 2, OriginMutated, "", "net.latency(kv-n1, mean=50, jitter=10)@3000..4000")
	if _, err := c.Add(strong, coverageOf("a=1", "b=1"),
		observedOutcome(10000, okFinding("no_crash", schema.ClassCrash))); err != nil {
		t.Fatalf("Add strong: %v", err)
	}

	weakHash, _ := weak.Hash()
	if _, ok := c.Get(weakHash); ok {
		t.Fatal("a strictly dominated world survived culling")
	}
	if c.Len() != 1 {
		t.Fatalf("corpus has %d entries, want 1", c.Len())
	}
}

// ---------------------------------------------------------------------------
// Rare-event bias
// ---------------------------------------------------------------------------

func TestRareTemplatesNeedEnoughWorldsToBeMeaningful(t *testing.T) {
	c := NewCorpus(DefaultEnergyParams())
	for i := 0; i < MinWorldsForRarity-1; i++ {
		w := corpusWorld(t, uint64(i+1), OriginSeeded, "",
			fmt.Sprintf("net.latency(kv-n1, mean=50, jitter=0)@%d..%d", 3000+i, 4000+i))
		if _, err := c.Add(w, coverageOf(fmt.Sprintf("unique=%d", i)),
			observedOutcome(30000, okFinding("no_crash", schema.ClassCrash))); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	if got := c.RareTemplates(DefaultRareFraction); len(got) != 0 {
		t.Fatalf("with %d worlds every template was called rare (%d of them); "+
			"a bias that applies to everything is not a bias", c.Worlds(), len(got))
	}
	if c.Worlds() != MinWorldsForRarity-1 {
		t.Fatalf("the test offered %d worlds, expected %d", c.Worlds(), MinWorldsForRarity-1)
	}
}

// TestRareTemplatesAreNotInertAtPhase4Budgets.
//
// The literal "<1% of worlds" rule is satisfied by nothing until 200 worlds,
// and Phase 4's measured rollout budget is around 62 (OQ-013). minRareThreshold
// is the documented floor that keeps the bias live from MinWorldsForRarity
// upward; this pins that it actually is live there, so the departure cannot be
// silently reverted into a permanently inert feature.
func TestRareTemplatesAreNotInertAtPhase4Budgets(t *testing.T) {
	c := NewCorpus(DefaultEnergyParams())
	common := "level=info event=boot node=kv-n1"
	for i := 0; i < MinWorldsForRarity; i++ {
		lines := []string{common}
		if i == 3 {
			lines = append(lines, "level=fatal event=truncate_committed node=kv-n1 index=1")
		}
		w := corpusWorld(t, uint64(i+1), OriginSeeded, "",
			fmt.Sprintf("net.latency(kv-n1, mean=50, jitter=0)@%d..%d", 3000+i, 4000+i))
		if _, err := c.Add(w, coverageOf(lines...),
			observedOutcome(30000, okFinding("no_crash", schema.ClassCrash))); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	rare := c.RareTemplates(DefaultRareFraction)
	if len(rare) != 1 {
		t.Fatalf("at %d worlds the rare-event bias found %d rare templates, want 1; "+
			"a bias that never fires is not a bias", c.Worlds(), len(rare))
	}
}

func TestRareTemplatesFindTheOnesFewWorldsHit(t *testing.T) {
	c := NewCorpus(DefaultEnergyParams())
	common := "level=info event=boot node=kv-n1"
	// 200 worlds all emit the common line; exactly one also emits a rare one.
	for i := 0; i < 200; i++ {
		lines := []string{common}
		if i == 7 {
			lines = append(lines, "level=fatal event=truncate_committed node=kv-n1 index=1")
		}
		w := corpusWorld(t, uint64(i+1), OriginSeeded, "",
			fmt.Sprintf("net.latency(kv-n1, mean=50, jitter=0)@%d..%d", 3000+i, 4000+i))
		if _, err := c.Add(w, coverageOf(lines...),
			observedOutcome(30000, okFinding("no_crash", schema.ClassCrash))); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	rare := c.RareTemplates(DefaultRareFraction)
	if len(rare) != 1 {
		t.Fatalf("want exactly one rare template, got %d: %v", len(rare), rare)
	}
	wantID := TemplateID(NormalizeLine("level=fatal event=truncate_committed node=kv-n1 index=1"))
	if rare[0] != wantID {
		t.Fatalf("rare template = %s, want %s", rare[0], wantID)
	}
	if commonID := TemplateID(NormalizeLine(common)); rare[0] == commonID {
		t.Fatal("the template every world hit was called rare")
	}
}

// ---------------------------------------------------------------------------
// Serialization: Phase 5's requirement
// ---------------------------------------------------------------------------

// TestProvenanceRoundTripsThroughTheThesisCodec is the claim that makes the
// corpus usable by Phase 5: origin and parent_hash must survive a write and a
// read, and stamping them must NOT change the world's identity (they are
// hash-excluded by D-014).
func TestProvenanceRoundTripsThroughTheThesisCodec(t *testing.T) {
	parent := corpusWorld(t, 1, OriginSeeded, "", "proc.pause(kv-n1)@3000..4000")
	parentHash, err := parent.Hash()
	if err != nil {
		t.Fatalf("hash parent: %v", err)
	}

	child, err := Derive(parent, OriginMutated, func(w *schema.World) {
		w.FaultSchedule.Planned = []string{
			"net.partition(role:leader)@8200..13500",
			"proc.pause(role:leader)@8300..11000",
		}
	})
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}
	if child.Meta == nil || child.Meta.Origin != OriginMutated || child.Meta.ParentHash != parentHash {
		t.Fatalf("child provenance = %+v, want origin=%s parent=%s", child.Meta, OriginMutated, parentHash)
	}

	c := NewCorpus(DefaultEnergyParams())
	for _, w := range []schema.World{parent, child} {
		if _, err := c.Add(w, coverageOf("a="+w.FaultSchedule.Planned[0]),
			observedOutcome(30000, okFinding("no_crash", schema.ClassCrash))); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}

	dir := t.TempDir()
	paths, err := c.Save(dir)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if len(paths) != c.Len() {
		t.Fatalf("Save wrote %d files for %d entries", len(paths), c.Len())
	}
	for _, p := range paths {
		if filepath.Ext(p) != CorpusExt {
			t.Fatalf("corpus file %s does not carry %s", p, CorpusExt)
		}
	}

	loaded, err := LoadCorpusWorlds(dir)
	if err != nil {
		t.Fatalf("LoadCorpusWorlds: %v", err)
	}
	if len(loaded) != c.Len() {
		t.Fatalf("loaded %d worlds, saved %d", len(loaded), c.Len())
	}

	var gotChild *schema.World
	for i := range loaded {
		if loaded[i].Meta != nil && loaded[i].Meta.Origin == OriginMutated {
			gotChild = &loaded[i]
		}
	}
	if gotChild == nil {
		t.Fatal("the mutated world's origin did not survive the round trip")
	}
	if gotChild.Meta.ParentHash != parentHash {
		t.Fatalf("parent_hash = %q after the round trip, want %q", gotChild.Meta.ParentHash, parentHash)
	}

	// Provenance is hash-excluded: stamping it must not change identity.
	stripped := *gotChild
	stripped.Meta = nil
	stripped = stripped.Normalized()
	withMeta, err := gotChild.Hash()
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	without, err := stripped.Hash()
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if withMeta != without {
		t.Fatalf("provenance changed the world hash (%s vs %s); every committed regression would be invalidated",
			withMeta, without)
	}
}

// TestACorpusFileWithoutProvenanceIsRefusedOnLoad: a .thesis file somebody
// dropped into the corpus directory must not be silently adopted as a seed.
func TestACorpusFileWithoutProvenanceIsRefusedOnLoad(t *testing.T) {
	dir := t.TempDir()
	w := testBaseWorld()
	w.FaultSchedule.Planned = []string{"proc.pause(kv-n1)@3000..4000"}
	w = w.Normalized()
	if _, err := recorder.StoreWorldNoClobber(dir, &w); err != nil {
		t.Fatalf("store: %v", err)
	}
	if _, err := LoadCorpusWorlds(dir); err == nil {
		t.Fatal("a corpus world with no provenance was loaded")
	}
}

// TestStampDoesNotMutateItsInput: mutating a corpus parent while deriving a
// child from it would silently reorder the parent's schedule, and because
// Normalized's sort is idempotent the damage would surface much later.
func TestStampDoesNotMutateItsInput(t *testing.T) {
	parent := corpusWorld(t, 1, OriginSeeded, "",
		"proc.pause(kv-n1)@9000..9500", "net.latency(kv-n2, mean=50, jitter=0)@3000..4000")
	before, err := parent.Hash()
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	_ = Stamp(parent, OriginMutated, "sha256:deadbeef")
	child, err := Derive(parent, OriginMutated, func(w *schema.World) {
		w.FaultSchedule.Planned = append(w.FaultSchedule.Planned, "net.loss(kv-n3, pct=5)@5000..6000")
	})
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}
	after, err := parent.Hash()
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if before != after {
		t.Fatalf("the parent world was mutated: %s -> %s", before, after)
	}
	if len(child.FaultSchedule.Planned) != 3 {
		t.Fatalf("child has %d faults, want 3", len(child.FaultSchedule.Planned))
	}
	if len(parent.FaultSchedule.Planned) != 2 {
		t.Fatalf("parent has %d faults, want 2", len(parent.FaultSchedule.Planned))
	}
}

// TestADerivedWorldCarriesNoMeasurementsFromItsParent: realized faults and
// phase timings are measurements of a run, and carrying them forward would be a
// claim about an execution that never happened.
func TestADerivedWorldCarriesNoMeasurementsFromItsParent(t *testing.T) {
	parent := corpusWorld(t, 1, OriginSeeded, "", "proc.pause(kv-n1)@3000..4000")
	parent.FaultSchedule.Realized = []schema.RealizedFault{{
		Fault: "proc.pause(kv-n1)@3000..4000", Resolved: "proc.pause(kv-n1)@3000..4000",
		Nodes: []string{"kv-n1"}, StartMS: 3002, EndMS: 4004,
	}}
	child, err := Derive(parent, OriginMutated, nil)
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}
	if child.FaultSchedule.Realized != nil {
		t.Fatalf("the child inherited a realized schedule: %+v", child.FaultSchedule.Realized)
	}
	if len(child.PhaseTimings) != 0 {
		t.Fatalf("the child inherited phase timings: %+v", child.PhaseTimings)
	}
}
