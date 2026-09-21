package schema

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/WrdCstlg/pro-thesis/internal/recorder/cjson"
)

// goldenWorldBytes is the frozen canonical encoding of goldenWorld().
//
// It is written out by hand rather than generated, so that any change to the
// canonicalizer, the field set, the fault printer or the normalizer fails here
// loudly instead of quietly re-baselining itself.
const goldenWorldBytes = `{"driver_profile":"gate",` +
	`"fault_schedule":{"planned":[` +
	`"net.partition(n1<->n2)@1000..2000",` +
	`"proc.pause(role:leader)@8200..11000",` +
	`"net.partition(minority(kv))@8400..10900"` +
	`],"realized":null},` +
	`"meta":{"origin":"test-fixture"},` +
	`"phase_timings":[` +
	`{"end_ms":-4000,"phase":"BOOT","start_ms":-9000},` +
	`{"end_ms":0,"phase":"SEED","start_ms":-4000},` +
	`{"end_ms":42000,"phase":"DRIVE","start_ms":0},` +
	`{"end_ms":15000,"phase":"PERTURB","start_ms":8000},` +
	`{"end_ms":45000,"phase":"HEAL","start_ms":42000},` +
	`{"end_ms":50000,"phase":"QUIESCE","start_ms":45000},` +
	`{"end_ms":55000,"phase":"ASSERT","start_ms":50000},` +
	`{"end_ms":57000,"phase":"TEARDOWN","start_ms":55000}],` +
	`"schema":"prothesis.world/v1",` +
	`"seed":424242,` +
	`"sut":{"images":[{` +
	`"digest":"sha256:5f2b0e0f8f2b9c0d1e3a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f90",` +
	`"image":"kvfixture:buggy","service":"kv"}]},` +
	`"topology_variant":"default"}` + "\n"

// goldenWorldHash pins the content address. Changing the canonical encoding in
// any way (even a way that still round-trips) invalidates every archived
// world, so it must never change silently.
const goldenWorldHash = "sha256:db30f2be72f0dec6697a41daaaa06c2a594fadf450cf903f601d4dc86732d1ae"

func goldenWorld() World {
	w := NewWorld(424242, "default", "gate")
	// Deliberately out of canonical order: Normalized must sort them.
	w.FaultSchedule.Planned = []string{
		"proc.pause(role:leader)@8200..11000",
		"net.partition(minority(kv))@8400..10900",
		"net.partition(n1<->n2)@1000..2000",
	}
	w.PhaseTimings = PhaseTimings{
		{Phase: PhaseBoot, StartMS: -9000, EndMS: -4000},
		{Phase: PhaseSeed, StartMS: -4000, EndMS: 0},
		{Phase: PhaseDrive, StartMS: 0, EndMS: 42000},
		{Phase: PhasePerturb, StartMS: 8000, EndMS: 15000},
		{Phase: PhaseHeal, StartMS: 42000, EndMS: 45000},
		{Phase: PhaseQuiesce, StartMS: 45000, EndMS: 50000},
		{Phase: PhaseAssert, StartMS: 50000, EndMS: 55000},
		{Phase: PhaseTeardown, StartMS: 55000, EndMS: 57000},
	}
	w.SUT = SUT{Images: []SUTImage{{
		Service: "kv",
		Image:   "kvfixture:buggy",
		Digest:  "sha256:5f2b0e0f8f2b9c0d1e3a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f90",
	}}}
	w.Meta = &WorldMeta{Origin: "test-fixture"}
	return w.Normalized()
}

// TestWorldGoldenEncoding is the frozen-format test.
func TestWorldGoldenEncoding(t *testing.T) {
	w := goldenWorld()
	if err := w.Validate(); err != nil {
		t.Fatalf("golden world does not validate: %v", err)
	}
	got, err := MarshalWorld(&w)
	if err != nil {
		t.Fatalf("MarshalWorld: %v", err)
	}
	if string(got) != goldenWorldBytes {
		t.Errorf("canonical encoding changed.\n got: %s\nwant: %s", got, goldenWorldBytes)
	}
	h, err := w.Hash()
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if h != goldenWorldHash {
		t.Errorf("world hash changed: got %s, want %s", h, goldenWorldHash)
	}
	short, err := w.ShortID()
	if err != nil {
		t.Fatalf("ShortID: %v", err)
	}
	if want := goldenWorldHash[len(HashPrefix) : len(HashPrefix)+ShortIDLen]; short != want {
		t.Errorf("ShortID = %q, want %q", short, want)
	}
}

// TestWorldRoundTripThroughDisk is Phase 0 definition of done (b).
//
// It compares against bytes READ BACK FROM DISK rather than against the
// encoder's own output, so a bug that affected both sides equally cannot hide.
func TestWorldRoundTripThroughDisk(t *testing.T) {
	worlds := map[string]World{
		"golden": goldenWorld(),
		"empty":  NewWorld(0, "default", "smoke"),
		"executed": func() World {
			w := goldenWorld()
			w.FaultSchedule.Realized = []RealizedFault{{
				Fault:    "proc.pause(role:leader)@8200..11000",
				Resolved: "proc.pause(n2)@8200..11000",
				Nodes:    []string{"n2"},
				StartMS:  8203,
				EndMS:    11007,
			}}
			return w.Normalized()
		}(),
		"no-meta": func() World {
			w := goldenWorld()
			w.Meta = nil
			return w
		}(),
	}

	dir := t.TempDir()
	for name, w := range worlds {
		t.Run(name, func(t *testing.T) {
			out, err := MarshalWorld(&w)
			if err != nil {
				t.Fatalf("MarshalWorld: %v", err)
			}
			path := filepath.Join(dir, name+WorldFileExt)
			if err := os.WriteFile(path, out, 0o644); err != nil {
				t.Fatalf("write: %v", err)
			}

			onDisk, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			decoded, err := UnmarshalWorld(onDisk)
			if err != nil {
				t.Fatalf("UnmarshalWorld: %v", err)
			}
			reEncoded, err := MarshalWorld(decoded)
			if err != nil {
				t.Fatalf("re-MarshalWorld: %v", err)
			}
			if !bytes.Equal(reEncoded, onDisk) {
				t.Errorf("not byte-identical:\n disk: %s\nagain: %s", onDisk, reEncoded)
			}

			h1, _ := w.Hash()
			h2, err := decoded.Hash()
			if err != nil {
				t.Fatalf("Hash: %v", err)
			}
			if h1 != h2 {
				t.Errorf("hash changed across the round trip: %s -> %s", h1, h2)
			}
		})
	}
}

// TestWorldEdgeTargetSurvivesEncoding is the HTML-escaping trap. Go's default
// JSON encoder writes '<' as <, which would corrupt n1<->n2 and change
// every world hash.
func TestWorldEdgeTargetSurvivesEncoding(t *testing.T) {
	w := goldenWorld()
	b, err := MarshalWorld(&w)
	if err != nil {
		t.Fatalf("MarshalWorld: %v", err)
	}
	if !bytes.Contains(b, []byte("n1<->n2")) {
		t.Errorf("edge target was escaped; encoding is: %s", b)
	}
	if bytes.Contains(b, []byte("\\u003c")) || bytes.Contains(b, []byte("\\u003e")) ||
		bytes.Contains(b, []byte("\\u0026")) {
		t.Errorf("encoding contains HTML escapes: %s", b)
	}
}

// TestWorldHashExcludesMeta: provenance must never change a world's identity,
// which is what lets later phases add provenance fields without invalidating
// the committed corpus.
func TestWorldHashExcludesMeta(t *testing.T) {
	a := goldenWorld()
	b := goldenWorld()
	b.Meta = &WorldMeta{Origin: "search", ParentHash: goldenWorldHash}

	ha, err := a.Hash()
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	hb, err := b.Hash()
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if ha != hb {
		t.Errorf("meta changed the world hash: %s != %s", ha, hb)
	}

	c := goldenWorld()
	c.Meta = nil
	hc, _ := c.Hash()
	if hc != ha {
		t.Errorf("dropping meta changed the world hash: %s != %s", hc, ha)
	}
}

// TestWorldHashCoversEveryIdentityMember: each of I2's five members plus sut
// must move the hash, or two different executions would share an identity.
func TestWorldHashCoversEveryIdentityMember(t *testing.T) {
	base := goldenWorld()
	baseHash, err := base.Hash()
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}

	mutations := map[string]func(*World){
		"seed":             func(w *World) { w.Seed++ },
		"topology_variant": func(w *World) { w.TopologyVariant = "three-node" },
		"driver_profile":   func(w *World) { w.DriverProfile = "smoke" },
		"fault_schedule.planned": func(w *World) {
			w.FaultSchedule.Planned = append(w.FaultSchedule.Planned, "proc.kill(n3, signal=SIGKILL)@1..2")
		},
		"fault_schedule.realized": func(w *World) {
			w.FaultSchedule.Realized = []RealizedFault{}
		},
		"phase_timings": func(w *World) { w.PhaseTimings[2].EndMS = 42001 },
		"sut":           func(w *World) { w.SUT.Images[0].Digest = "sha256:" + strings.Repeat("0", 64) },
	}
	for name, mutate := range mutations {
		w := goldenWorld()
		mutate(&w)
		h, err := w.Hash()
		if err != nil {
			t.Fatalf("%s: Hash: %v", name, err)
		}
		if h == baseHash {
			t.Errorf("changing %s did not change the world hash", name)
		}
	}
}

// TestWorldRejectsNonCanonicalInput is rule 8 as a runtime invariant.
func TestWorldRejectsNonCanonicalInput(t *testing.T) {
	w := goldenWorld()
	canonical, err := MarshalWorld(&w)
	if err != nil {
		t.Fatalf("MarshalWorld: %v", err)
	}

	cases := map[string][]byte{
		"crlf":                bytes.ReplaceAll(canonical, []byte("\n"), []byte("\r\n")),
		"no trailing newline": bytes.TrimSuffix(canonical, []byte("\n")),
		"pretty printed": func() []byte {
			var out bytes.Buffer
			out.Write(bytes.ReplaceAll(canonical, []byte(`","`), []byte(`", "`)))
			return out.Bytes()
		}(),
		"unknown field": bytes.Replace(canonical,
			[]byte(`"seed":424242`), []byte(`"seed":424242,"tree_id":"x"`), 1),
		"unsorted keys": []byte(`{"seed":1,"driver_profile":"gate","fault_schedule":{"planned":[],` +
			`"realized":null},"phase_timings":[],"schema":"prothesis.world/v1",` +
			`"sut":{"images":[]},"topology_variant":"default"}` + "\n"),
		"trailing data": append(append([]byte{}, canonical...), []byte("{}\n")...),
		// Go's default encoder would have written the edge target this way.
		"html escaped": bytes.Replace(canonical,
			[]byte("n1<->n2"), []byte("n1\\u003c-\\u003en2"), 1),
		"empty": nil,
	}
	for name, data := range cases {
		if _, err := UnmarshalWorld(data); err == nil {
			t.Errorf("%s: accepted a non-canonical .thesis file", name)
		}
	}

	// And the canonical form itself still loads.
	if _, err := UnmarshalWorld(canonical); err != nil {
		t.Errorf("canonical bytes rejected: %v", err)
	}
}

func TestWorldRejectsWrongSchema(t *testing.T) {
	w := goldenWorld()
	w.Schema = "prothesis.world/v2"
	b, err := CanonicalMarshal(&w)
	if err != nil {
		t.Fatalf("CanonicalMarshal: %v", err)
	}
	if _, err := UnmarshalWorld(append(b, '\n')); err == nil {
		t.Error("a world declaring an unknown schema id was accepted")
	}
}

func TestWorldValidateCatchesRealMistakes(t *testing.T) {
	cases := map[string]func(*World){
		"null planned":         func(w *World) { w.FaultSchedule.Planned = nil },
		"unparseable fault":    func(w *World) { w.FaultSchedule.Planned[0] = "net.nope(n1)@0..1" },
		"non-canonical fault":  func(w *World) { w.FaultSchedule.Planned[0] = "proc.kill(n1)@0..1" },
		"empty driver_profile": func(w *World) { w.DriverProfile = "" },
		"null phase_timings":   func(w *World) { w.PhaseTimings = nil },
		"duplicate phase": func(w *World) {
			w.PhaseTimings = append(w.PhaseTimings, PhaseWindow{Phase: PhaseDrive, StartMS: 0, EndMS: 1})
		},
		"drive not at origin": func(w *World) { w.PhaseTimings[2].StartMS = 5 },
		"null sut images":     func(w *World) { w.SUT.Images = nil },
	}
	for name, mutate := range cases {
		w := goldenWorld()
		mutate(&w)
		if err := w.Validate(); err == nil {
			t.Errorf("%s: Validate accepted it", name)
		}
	}
}

// TestWorldTypeGraphIsCanonicalJSONSafe is the static check that the world
// carries no float, no map, no interface and no embedded field: the four
// things that would make the encoding non-deterministic or lossy. Encode only
// catches these on paths a particular value happens to reach.
func TestWorldTypeGraphIsCanonicalJSONSafe(t *testing.T) {
	if err := cjson.Lint(World{}); err != nil {
		t.Errorf("cjson.Lint(World): %v", err)
	}
	for _, v := range []any{FaultSchedule{}, RealizedFault{}, SUT{}, SUTImage{}, WorldMeta{}, PhaseWindow{}} {
		if err := cjson.Lint(v); err != nil {
			t.Errorf("cjson.Lint(%T): %v", v, err)
		}
	}
}

// TestWorldEncoderAgreesWithCJSON.
//
// MarshalWorld delegates to internal/recorder/cjson, so this now asserts that
// the delegation is intact and total: that no world reaches a second encoding
// path. It was previously the guard on two independently-written encoders, and
// it did not catch that they disagreed on U+2028/U+2029, because a corpus of
// realistic worlds cannot: see TestCodecsAgreeOnEveryStringByte in
// internal/recorder for the input classes that separate them.
func TestWorldEncoderAgreesWithCJSON(t *testing.T) {
	worlds := []World{
		goldenWorld(),
		NewWorld(0, "default", "smoke"),
		func() World {
			w := goldenWorld()
			w.Meta = nil
			w.FaultSchedule.Realized = []RealizedFault{{
				Fault:    "proc.pause(role:leader)@8200..11000",
				Resolved: "proc.pause(n2)@8200..11000",
				Nodes:    []string{"n2"}, StartMS: 8203, EndMS: 11007,
			}}
			return w.Normalized()
		}(),
		func() World {
			w := NewWorld(^uint64(0), "max-seed", "gate")
			return w.Normalized()
		}(),
	}
	for i, w := range worlds {
		mine, err := MarshalWorld(&w)
		if err != nil {
			t.Fatalf("world %d: MarshalWorld: %v", i, err)
		}
		theirs, err := cjson.Encode(w)
		if err != nil {
			t.Fatalf("world %d: cjson.Encode: %v", i, err)
		}
		if !bytes.Equal(mine, theirs) {
			t.Errorf("world %d: encoders disagree\n schema: %s\n  cjson: %s", i, mine, theirs)
		}
	}
}

func TestWorldFilenameHelpers(t *testing.T) {
	w := goldenWorld()
	name, err := WorldFilenameFor(&w)
	if err != nil {
		t.Fatalf("WorldFilenameFor: %v", err)
	}
	short, err := ParseWorldFilename(name)
	if err != nil {
		t.Fatalf("ParseWorldFilename(%q): %v", name, err)
	}
	if got, _ := w.ShortID(); got != short {
		t.Errorf("filename %q carries %q, want %q", name, short, got)
	}
	for _, bad := range []string{"w_a41f.json", "a41f.thesis", "w_XYZ1.thesis", "w_a41.thesis", "world.thesis"} {
		if _, err := ParseWorldFilename(bad); err == nil {
			t.Errorf("ParseWorldFilename(%q) succeeded", bad)
		}
	}
}

// Two nodes can report the same (service, image) with DIFFERENT digests: a tag
// that drifted between pulls. Ordering on (service, image) alone leaves those two
// entries tied, so their order in the canonical bytes is whatever order the nodes
// happened to bind in. A world is content-addressed, so that is not cosmetic: the
// same world would hash two ways, and every consumer that identifies a world by
// its hash (the corpus filename binding, replay, shrink identity) would see two
// worlds where there is one.
//
// The assertion is the hash, not the slice order: slice order is the mechanism, a
// stable content address is the property. An earlier version of this test guarded
// its comparison with `if len(images) == 2 && ...`, which passed silently in the
// one case worth catching: a Normalized() that dropped an entry.
func TestWorldNormalizedOrdersTiedImagesByDigest(t *testing.T) {
	bbbb := SUTImage{Service: "kv", Image: "prothesis/kvfixture:buggy", Digest: "sha256:bbbb"}
	aaaa := SUTImage{Service: "kv", Image: "prothesis/kvfixture:buggy", Digest: "sha256:aaaa"}

	newWorldWith := func(images ...SUTImage) *World {
		w := NewWorld(1, "default", "smoke")
		w.SUT.Images = images
		return &w
	}

	ascending := newWorldWith(aaaa, bbbb).Normalized()
	descending := newWorldWith(bbbb, aaaa).Normalized()

	if len(ascending.SUT.Images) != 2 || len(descending.SUT.Images) != 2 {
		t.Fatalf("Normalized() changed the image count: %d and %d, want 2 and 2 "+
			"(entries differing only by digest are distinct and must not be deduplicated)",
			len(ascending.SUT.Images), len(descending.SUT.Images))
	}

	for _, tc := range []struct {
		name string
		got  World
	}{{"ascending input", ascending}, {"descending input", descending}} {
		if d := tc.got.SUT.Images[0].Digest; d != "sha256:aaaa" {
			t.Errorf("%s: images[0].Digest = %q, want the lower digest %q", tc.name, d, "sha256:aaaa")
		}
		if d := tc.got.SUT.Images[1].Digest; d != "sha256:bbbb" {
			t.Errorf("%s: images[1].Digest = %q, want the higher digest %q", tc.name, d, "sha256:bbbb")
		}
	}

	ascHash, err := ascending.Hash()
	if err != nil {
		t.Fatalf("Hash() of the ascending world: %v", err)
	}
	descHash, err := descending.Hash()
	if err != nil {
		t.Fatalf("Hash() of the descending world: %v", err)
	}
	if ascHash != descHash {
		t.Errorf("the same world hashed two ways depending on the order the images arrived in:\n"+
			"  ascending  = %s\n  descending = %s\n"+
			"a content address that depends on node bind order is not a content address", ascHash, descHash)
	}
}
