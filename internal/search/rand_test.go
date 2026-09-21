package search

import (
	"fmt"
	"strings"
	"testing"

	"github.com/WrdCstlg/pro-thesis/internal/recorder"
)

// TestAddingAStreamCannotPerturbAnExistingOne is D-013's property, checked
// where this package relies on it.
//
// The consequence if it failed is specific and severe: the Saboteur will add
// its own streams in internal/search/saboteur, and if that shifted the values
// the random baseline draws, a benchmark run before and after would not be
// comparing the same arm, and every committed regression world would be
// invalidated at the same time.
func TestAddingAStreamCannotPerturbAnExistingOne(t *testing.T) {
	const seed = recorder.Seed(0x1234_5678)

	before := make([]uint64, 8)
	s1 := NewStreams(seed)
	st := s1.Get(PathRandomWorld, "3")
	for i := range before {
		before[i] = st.Uint64()
	}

	// A second registry that touches several other paths first, including one
	// that does not exist today, standing in for a future Saboteur stream.
	s2 := NewStreams(seed)
	for _, p := range [][]string{
		{PathCorpusSelect},
		{PathMutateOp},
		{PathMutateDraw},
		{PathSeedWorlds, "control"},
		{"search.saboteur.probe"},
		{"search.saboteur.mcts", "42"},
	} {
		other := s2.Get(p...)
		for i := 0; i < 100; i++ {
			other.Uint64()
		}
	}
	after := make([]uint64, 8)
	st2 := s2.Get(PathRandomWorld, "3")
	for i := range after {
		after[i] = st2.Uint64()
	}

	for i := range before {
		if before[i] != after[i] {
			t.Fatalf("draw %d moved after other streams were used: %d then %d", i, before[i], after[i])
		}
	}
}

// TestStreamsAreMemoizedSoSequentialDrawsContinue.
func TestStreamsAreMemoizedSoSequentialDrawsContinue(t *testing.T) {
	s := NewStreams(1)
	a := s.Get(PathMutateOp)
	first := a.Uint64()
	b := s.Get(PathMutateOp)
	if a != b {
		t.Fatal("Get returned a different stream for the same path")
	}
	second := b.Uint64()
	if first == second {
		t.Fatal("the memoized stream restarted rather than continuing")
	}
}

// TestDifferentOrdinalsAreIndependentSubStreams.
func TestDifferentOrdinalsAreIndependentSubStreams(t *testing.T) {
	s := NewStreams(99)
	seen := map[uint64]int{}
	for i := 0; i < 50; i++ {
		v := s.World(PathRandomWorld, i).Uint64()
		if prev, ok := seen[v]; ok {
			t.Fatalf("ordinals %d and %d produced the same first value %d", prev, i, v)
		}
		seen[v] = i
	}
}

// TestStreamsPanicOnAnEmptyPath: a typo'd path would otherwise produce a
// perfectly deterministic, completely unreproducible stream.
func TestStreamsPanicOnAnEmptyPath(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("Get with no path did not panic")
		}
	}()
	NewStreams(1).Get()
}

// TestStreamPathsAreDistinctAndStable pins the path constants. Renaming one
// silently changes every search recorded against a seed while leaving every
// world identity unchanged: the worst kind of change, because nothing fails.
func TestStreamPathsAreDistinctAndStable(t *testing.T) {
	want := map[string]string{
		"PathCorpusSelect": "search.corpus.select",
		"PathMutateOp":     "search.mutate.op",
		"PathMutateDraw":   "search.mutate.draw",
		"PathRandomWorld":  "search.random.world",
		"PathSeedWorlds":   "search.seed.world",
	}
	got := map[string]string{
		"PathCorpusSelect": PathCorpusSelect,
		"PathMutateOp":     PathMutateOp,
		"PathMutateDraw":   PathMutateDraw,
		"PathRandomWorld":  PathRandomWorld,
		"PathSeedWorlds":   PathSeedWorlds,
	}
	for name, w := range want {
		if got[name] != w {
			t.Errorf("%s = %q, want %q", name, got[name], w)
		}
	}
	seen := map[string]string{}
	for name, v := range got {
		if other, dup := seen[v]; dup {
			t.Errorf("%s and %s share the path %q; two decisions would share a keystream", name, other, v)
		}
		seen[v] = name
	}
}

func TestStreamAuditIsSortedAndCountsDraws(t *testing.T) {
	s := NewStreams(3)
	s.Get(PathMutateOp).Uint64()
	s.Get(PathCorpusSelect).Uint64()
	s.Get(PathCorpusSelect).Uint64()
	audit := s.Audit()
	if len(audit) != 2 {
		t.Fatalf("audit has %d entries, want 2", len(audit))
	}
	if audit[0].Domain >= audit[1].Domain {
		t.Fatalf("audit is not sorted: %v", audit)
	}
	byPath := map[string]uint64{}
	for _, a := range audit {
		byPath[a.Domain] = a.Draws
	}
	if byPath[PathCorpusSelect] != 2 || byPath[PathMutateOp] != 1 {
		t.Fatalf("draw counts are wrong: %v", byPath)
	}
}

// TestPickIsUniformEnoughToBeABaseline. A generator with a badly skewed pick
// would make "uniform random fault injection" untrue, and D-029's comparison
// would be against something else.
func TestPickIsUniformEnoughToBeABaseline(t *testing.T) {
	st := recorder.NewStream(recorder.MustDeriveKey(1, "test", "pick"))
	items := []string{"a", "b", "c", "d", "e", "f", "g", "h"}
	counts := map[string]int{}
	const draws = 80000
	for i := 0; i < draws; i++ {
		counts[pick(st, items)]++
	}
	expected := draws / len(items)
	for _, it := range items {
		if counts[it] < expected*9/10 || counts[it] > expected*11/10 {
			t.Fatalf("%q drawn %d times, expected about %d: %v", it, counts[it], expected, counts)
		}
	}
}

func TestPickPanicsOnAnEmptySet(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("pick from an empty set did not panic; it would emit an empty fault target")
		}
	}()
	pick(recorder.NewStream(recorder.MustDeriveKey(1, "t")), []string{})
}

func TestPickIndexAgreesWithPick(t *testing.T) {
	items := []string{"x", "y", "z"}
	for seed := uint64(0); seed < 20; seed++ {
		a := recorder.NewStream(recorder.MustDeriveKey(recorder.Seed(seed), "t"))
		b := recorder.NewStream(recorder.MustDeriveKey(recorder.Seed(seed), "t"))
		got := pick(a, items)
		i, alsoGot := pickIndex(b, items)
		if got != alsoGot || items[i] != got {
			t.Fatalf("pick=%q pickIndex=(%d,%q)", got, i, alsoGot)
		}
	}
}

func TestJoinPathIsForDisplayOnly(t *testing.T) {
	// The memo key is a display string; the KEY is derived from the segments,
	// so two different segment lists must not share a derived key even if their
	// joined display forms would collide.
	a, err := recorder.DeriveKey(1, "a/b", "c")
	if err != nil {
		t.Fatalf("DeriveKey: %v", err)
	}
	b, err := recorder.DeriveKey(1, "a", "b/c")
	if err != nil {
		t.Fatalf("DeriveKey: %v", err)
	}
	if a == b {
		t.Fatal("two distinct segment lists derived the same key")
	}
	if got := joinPath([]string{"a", "b", "c"}); got != "a/b/c" {
		t.Fatalf("joinPath = %q", got)
	}
	_ = fmt.Sprint(strings.Count("a/b/c", "/"))
}
