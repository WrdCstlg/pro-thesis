package corpus

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// The corpus is the one artifact in this system that outlives a run and is
// consulted forever after: `thesis regress` replays every world in it on every
// gate invocation. Two failures are therefore expensive in opposite directions.
//
// Committing a world that does NOT reproduce poisons every future gate with a
// permanent red that nobody can fix, because the world is evidence of nothing.
// Refusing a world that DOES reproduce loses the regression entirely. The gate
// between those two outcomes is Confirmation.Passed, so it is tested first and
// hardest.

func confirmed(k int) Confirmation {
	return Confirmation{Attempts: k, Reproduced: k}
}

func TestConfirmationPassesOnlyOnUnanimousReproduction(t *testing.T) {
	cases := []struct {
		name string
		c    Confirmation
		want bool
	}{
		{"3/3 clean", Confirmation{Attempts: 3, Reproduced: 3}, true},
		{"1/1 clean", Confirmation{Attempts: 1, Reproduced: 1}, true},

		// Never ran. A world nobody replayed is not evidence, and the zero
		// value of a struct must not read as a pass: this is the case a
		// caller reaches by forgetting to run the gate at all.
		{"zero value", Confirmation{}, false},
		{"0 attempts but claims reproductions", Confirmation{Attempts: 0, Reproduced: 3}, false},

		// Partial reproduction. Under Tier B this is the common honest answer,
		// and it is NOT a pass: a world that reproduces 2 times in 3 makes
		// `thesis regress` flaky, which trains a team to ignore it.
		{"2/3", Confirmation{Attempts: 3, Reproduced: 2}, false},
		{"0/3", Confirmation{Attempts: 3, Reproduced: 0}, false},

		// An inconclusive replay is not a reproduction and not a
		// non-reproduction. Folding it either way manufactures evidence in one
		// direction or flakiness in the other, so any inconclusive fails.
		{"3/3 but one inconclusive", Confirmation{Attempts: 3, Reproduced: 3, Inconclusive: 1}, false},
		{"2/3 with an inconclusive", Confirmation{Attempts: 3, Reproduced: 2, Inconclusive: 1}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.c.Passed(); got != tc.want {
				t.Fatalf("Passed() = %v, want %v for %+v\n"+
					"A false pass commits a world that proves nothing and reds every future "+
					"gate run; a false refusal silently drops a real regression.", got, tc.want, tc.c)
			}
		})
	}
}

// The refusal message is read by a human deciding whether a regression was lost
// or correctly rejected, so it must name WHICH of the three refusal shapes
// occurred rather than saying only "not confirmed".
func TestErrNotConfirmedDistinguishesTheThreeRefusals(t *testing.T) {
	cases := []struct {
		name string
		c    Confirmation
		want string
	}{
		{"never ran", Confirmation{}, "never ran"},
		{"never reproduced", Confirmation{Attempts: 3, Reproduced: 0}, "0/3"},
		{"partial", Confirmation{Attempts: 3, Reproduced: 2}, "2/3"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := &ErrNotConfirmed{Confirmation: tc.c}
			msg := err.Error()
			if !strings.Contains(msg, tc.want) {
				t.Fatalf("the refusal does not say %q, so a reader cannot tell which refusal it was:\n%s",
					tc.want, msg)
			}
		})
	}

	// The three messages must differ from one another, or naming the shape
	// achieves nothing.
	seen := map[string]string{}
	for _, tc := range cases {
		m := (&ErrNotConfirmed{Confirmation: tc.c}).Error()
		if prev, dup := seen[m]; dup {
			t.Fatalf("%q and %q produce the identical message", prev, tc.name)
		}
		seen[m] = tc.name
	}
}

// --- the store ------------------------------------------------------------

func TestCreateIsIdempotentAndNeverClobbersAReadme(t *testing.T) {
	c := OpenDir(filepath.Join(t.TempDir(), RegressionsDirName))

	if c.Exists() {
		t.Fatal("a corpus reported itself present before it was created")
	}
	if err := c.Create(); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !c.Exists() {
		t.Fatal("Create did not produce a corpus")
	}

	readme := filepath.Join(c.Dir(), ReadmeFileName)
	// A hand-edited README is a human's note about why these worlds exist. The
	// corpus is append-only in spirit as well as in mechanism, so a second
	// Create must not silently overwrite it.
	const edited = "# Regression corpus\n\nHand-edited: these three worlds are the lease bug.\n"
	if err := os.WriteFile(readme, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := c.Create(); err != nil {
		t.Fatalf("second Create: %v", err)
	}
	got, err := os.ReadFile(readme)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != edited {
		t.Fatalf("Create clobbered a hand-edited README:\n%s", got)
	}
}

func TestAddRefusesAnUnconfirmedWorld(t *testing.T) {
	c := OpenDir(filepath.Join(t.TempDir(), RegressionsDirName))
	if err := c.Create(); err != nil {
		t.Fatal(err)
	}
	w := executedWorld(t)

	for _, conf := range []Confirmation{
		{},
		{Attempts: 3, Reproduced: 0},
		{Attempts: 3, Reproduced: 2},
		{Attempts: 3, Reproduced: 3, Inconclusive: 1},
	} {
		name, err := c.Add(w, conf)
		if err == nil {
			t.Fatalf("Add accepted %+v and wrote %q. A world that does not reproduce k/k makes "+
				"every future `thesis regress` report on nothing.", conf, name)
		}
		var nc *ErrNotConfirmed
		if !errors.As(err, &nc) {
			t.Fatalf("Add(%+v) refused with %T, want *ErrNotConfirmed so a caller can tell a "+
				"confirmation refusal from an I/O failure", conf, err)
		}
	}

	entries, err := c.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("a refused world reached the corpus anyway: %d entr(ies)", len(entries))
	}
}

func TestAddAcceptsAConfirmedWorldAndItRoundTrips(t *testing.T) {
	c := OpenDir(filepath.Join(t.TempDir(), RegressionsDirName))
	if err := c.Create(); err != nil {
		t.Fatal(err)
	}
	w := executedWorld(t)

	// Add returns the committed world's PATH, not its bare name.
	path, err := c.Add(w, confirmed(3))
	if err != nil {
		t.Fatalf("Add refused a 3/3 world: %v", err)
	}
	name := filepath.Base(path)
	if !strings.HasPrefix(name, "w_") || !strings.HasSuffix(name, ".thesis") {
		t.Fatalf("committed as %q, want w_<id>.thesis", name)
	}
	if !filepath.IsAbs(path) {
		t.Fatalf("Add returned %q; a relative path would resolve against the caller's working "+
			"directory, which is not the project root for `thesis regress`", path)
	}

	entries, err := c.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	e := entries[0]
	if !e.OK() {
		t.Fatalf("the committed world does not read back: %v", e.Err)
	}
	// The point of the corpus is that a world replays LATER, so the bytes must
	// survive the round trip through the canonical codec.
	if e.World.Seed != w.Seed {
		t.Fatalf("seed changed across the corpus: %d -> %d", w.Seed, e.World.Seed)
	}
	if len(e.World.FaultSchedule.Realized) != len(w.FaultSchedule.Realized) {
		t.Fatalf("the realized schedule changed across the corpus: %d -> %d faults",
			len(w.FaultSchedule.Realized), len(e.World.FaultSchedule.Realized))
	}

	// LF only. A CRLF rewrite on a Windows checkout would break the world hash
	// of every committed regression at once, in somebody else's clone.
	raw, err := os.ReadFile(e.Path)
	if err != nil {
		t.Fatal(err)
	}
	if idx := strings.IndexByte(string(raw), '\r'); idx >= 0 {
		t.Fatalf("a committed world carries a CR at byte %d; .gitattributes exists to prevent "+
			"exactly this and the corpus must not write one itself", idx)
	}
}

// Append-only is the mechanism, not merely the policy: there is no exported way
// to delete or mutate an entry, because removing a regression file is a lock
// change (D-004) and an API that offered it would invite the change to be made
// without one.
func TestTheCorpusOffersNoWayToRemoveAWorld(t *testing.T) {
	c := OpenDir(filepath.Join(t.TempDir(), RegressionsDirName))
	if err := c.Create(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Add(executedWorld(t), confirmed(3)); err != nil {
		t.Fatal(err)
	}

	// Adding a SECOND, identical world must not overwrite the first. Two
	// discoveries of the same shape are two pieces of evidence.
	before, err := c.List()
	if err != nil {
		t.Fatal(err)
	}
	w2 := executedWorld(t)
	w2.Seed = w2.Seed + 1
	if _, err := c.Add(w2, confirmed(3)); err != nil {
		t.Fatalf("Add of a second world: %v", err)
	}
	after, err := c.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before)+1 {
		t.Fatalf("committing a second world left %d entries, want %d — an overwrite would "+
			"silently destroy a regression", len(after), len(before)+1)
	}
}

func TestListIsStableAndIgnoresNonWorlds(t *testing.T) {
	c := OpenDir(filepath.Join(t.TempDir(), RegressionsDirName))
	if err := c.Create(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		w := executedWorld(t)
		w.Seed = uint64(1000 + i)
		if _, err := c.Add(w, confirmed(3)); err != nil {
			t.Fatalf("Add %d: %v", i, err)
		}
	}
	// Files that legitimately live beside the worlds. The lock hashes the whole
	// directory, so a README and a note are expected; neither is a world.
	for _, f := range []string{ReadmeFileName, "notes.txt", ".gitkeep"} {
		if err := os.WriteFile(filepath.Join(c.Dir(), f), []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	first, err := c.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 4 {
		t.Fatalf("got %d entries, want 4 — a non-world file was parsed as a world", len(first))
	}
	// `thesis regress` runs this order on every gate invocation, so an order
	// that depended on directory iteration would make a budgeted run cover a
	// different subset each time.
	for i := 0; i < 8; i++ {
		again, err := c.List()
		if err != nil {
			t.Fatal(err)
		}
		for j := range again {
			if again[j].Name != first[j].Name {
				t.Fatalf("List is not stable: entry %d was %q then %q", j, first[j].Name, again[j].Name)
			}
		}
	}
}

// A corrupt file is reported, never skipped. Skipping would make `thesis
// regress` quietly cover less than the corpus while still reporting PASS.
func TestACorruptWorldIsReportedRatherThanSkipped(t *testing.T) {
	c := OpenDir(filepath.Join(t.TempDir(), RegressionsDirName))
	if err := c.Create(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Add(executedWorld(t), confirmed(3)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c.Dir(), "w_dead.thesis"), []byte("{not a world\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	entries, err := c.List()
	if err != nil {
		t.Fatalf("List failed outright on a corrupt member; it should report the member: %v", err)
	}
	var bad int
	for _, e := range entries {
		if !e.OK() {
			bad++
			if e.Err == nil {
				t.Fatal("an entry is not OK but carries no error to explain why")
			}
		}
	}
	if bad != 1 {
		t.Fatalf("got %d unreadable entries, want 1 — a corrupt world was skipped, which would "+
			"make regress silently cover less than the corpus", bad)
	}
}

func TestListOnAnAbsentCorpusIsEmptyNotAnError(t *testing.T) {
	c := OpenDir(filepath.Join(t.TempDir(), RegressionsDirName))
	entries, err := c.List()
	if err != nil {
		t.Fatalf("listing a corpus that has never been created should be empty, not an error: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("got %d entries from an absent corpus", len(entries))
	}
}

// OQ-062 / D-063. The short id in the filename is the content address, not a
// label: a file named w_xxxx.thesis must contain the world whose hash begins
// with xxxx. A world loaded under a mismatched name is a corrupted corpus
// entry and must be reported as an error, never executed cleanly under the
// wrong identity.
func TestListRejectsWorldWhoseFilenameDoesNotMatchContentHash(t *testing.T) {
	c := OpenDir(filepath.Join(t.TempDir(), RegressionsDirName))
	if err := c.Create(); err != nil {
		t.Fatal(err)
	}
	path, err := c.Add(executedWorld(t), confirmed(3))
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	// Place the exact same world bytes under a mismatched short ID.
	mismatchedName := "w_0000.thesis"
	if filepath.Base(path) == mismatchedName {
		mismatchedName = "w_ffff.thesis"
	}
	if err := os.WriteFile(filepath.Join(c.Dir(), mismatchedName), data, 0o644); err != nil {
		t.Fatal(err)
	}

	entries, err := c.List()
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	if entries[0].OK() {
		t.Fatalf("entry %s was accepted as OK despite its filename not matching its content hash (%s)",
			entries[0].Name, entries[0].Hash)
	}
	if entries[0].Err == nil || !strings.Contains(entries[0].Err.Error(), "hash") {
		t.Fatalf("entry error does not explain hash mismatch: %v", entries[0].Err)
	}
}

// OQ-062 / D-063. A file ending in .thesis that fails ParseWorldFilename is an
// invalid regression file, not a benign neighbor like README.md. Silently
// skipping it would let a corrupt or mistyped regression file disappear from
// regress, producing a vacuous pass.
func TestListSurfacesMalformedThesisFilenamesAsErrors(t *testing.T) {
	c := OpenDir(filepath.Join(t.TempDir(), RegressionsDirName))
	if err := c.Create(); err != nil {
		t.Fatal(err)
	}
	// Names that CLAIM to be worlds and fail schema.ParseWorldFilename. The last
	// two are the shapes a suffix test misses: uppercase hex, and the .orig a
	// merge conflict leaves behind; each of which is a committed regression
	// that has silently stopped being one.
	malformed := []string{"w_bad.thesis", "world-0001.thesis", "w_A41F.thesis", "w_e952.thesis.orig"}
	for _, name := range malformed {
		if err := os.WriteFile(filepath.Join(c.Dir(), name), []byte("dummy\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// And legitimate non-world files that MUST still be ignored: the README the
	// corpus writes itself, and a note somebody left.
	for _, f := range []string{ReadmeFileName, "notes.txt"} {
		if err := os.WriteFile(filepath.Join(c.Dir(), f), []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	entries, err := c.List()
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if len(entries) != len(malformed) {
		t.Fatalf("got %d entries, want %d (malformed .thesis files were silently skipped)",
			len(entries), len(malformed))
	}
	for _, e := range entries {
		if e.OK() {
			t.Fatalf("malformed file %s was reported as OK", e.Name)
		}
		if e.Err == nil {
			t.Fatalf("malformed file %s has nil Err", e.Name)
		}
	}
}

// --- helpers ---------------------------------------------------------------

// executedWorld builds a world that has actually been run: it carries a
// REALIZED schedule. Add refuses a world without one, because a confirmation
// attached to a never-executed world describes some other world.
func executedWorld(t *testing.T) *schema.World {
	t.Helper()
	w := &schema.World{
		Schema:          schema.WorldSchema,
		Seed:            424242,
		TopologyVariant: "default",
		DriverProfile:   "linear",
		FaultSchedule: schema.FaultSchedule{
			Planned: []string{"net.partition(role:leader)@3000..5000"},
			Realized: []schema.RealizedFault{{
				Fault:    "net.partition(role:leader)@3000..5000",
				Resolved: "net.partition(kv-n1)@3000..5000",
				Nodes:    []string{"kv-n1"},
				StartMS:  3001,
				EndMS:    5000,
			}},
		},
		PhaseTimings: schema.PhaseTimings{
			{Phase: schema.PhaseDrive, StartMS: 0, EndMS: 1},
			{Phase: schema.PhaseAssert, StartMS: 9000, EndMS: 9000},
		},
		// Required: the world file records WHICH BUILD of the system under test
		// ran (D-012 / OQ-011), because a regression replayed after a patch
		// otherwise silently tests a different program. Empty is written as []
		// rather than null so a third-party reader never has to distinguish
		// "no images resolved" from "field absent".
		SUT: schema.SUT{Images: []schema.SUTImage{{
			Service: "kv-n1",
			Image:   "prothesis/kvfixture:buggy",
			Digest:  "sha256:0000000000000000000000000000000000000000000000000000000000000000",
		}}},
	}
	if err := w.Validate(); err != nil {
		t.Fatalf("the test's own world does not validate, so the test would be measuring its "+
			"fixture rather than the corpus: %v", err)
	}
	return w
}
