package recorder

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/WrdCstlg/pro-thesis/internal/recorder/cjson"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// ---------------------------------------------------------------------------
// A hand-built world whose canonical bytes exercise the awkward cases
// ---------------------------------------------------------------------------

// goldenWorld is deliberately built around an EDGE target, `n1<->n2`. Go's
// encoding/json escapes '<' and '>' by default; if that ever leaks into the
// `.thesis` encoder, every world hash in a committed corpus changes at once.
// Having a literal '<' in the golden bytes makes that a test failure rather
// than a discovery.
func goldenWorld(t *testing.T) schema.World {
	t.Helper()
	w := schema.NewWorld(0x2a, "kv3", "gate")
	w.FaultSchedule.Planned = canonicalFaults(t,
		"proc.pause(role:leader)@8200..11000",
		"net.partition(minority(kv))@8400..10900",
		"net.loss(n1<->n2, pct=5)@500..1500",
	)
	w.PhaseTimings = schema.PhaseTimings{
		{Phase: schema.PhaseDrive, StartMS: 0, EndMS: 42000},
		{Phase: schema.PhaseHeal, StartMS: 42000, EndMS: 45000},
		{Phase: schema.PhaseQuiesce, StartMS: 45000, EndMS: 50000},
		{Phase: schema.PhaseAssert, StartMS: 50000, EndMS: 55000},
	}
	w.SUT.Images = []schema.SUTImage{
		{Service: "kv", Image: "kvfixture:buggy", Digest: "sha256:" + strings.Repeat("ab", 32)},
	}
	w.Meta = &schema.WorldMeta{Origin: "shrink", ParentHash: "sha256:" + strings.Repeat("cd", 32)}
	out := w.Normalized()
	if err := out.Validate(); err != nil {
		t.Fatalf("golden world does not validate: %v", err)
	}
	return out
}

// canonicalFaults runs each candidate through the schema's own canonicalizer,
// so this test file can never drift from the fault grammar.
func canonicalFaults(t *testing.T, ss ...string) []string {
	t.Helper()
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		c, err := schema.CanonicalFault(s)
		if err != nil {
			t.Fatalf("candidate fault %q does not parse: %v", s, err)
		}
		out = append(out, c)
	}
	return schema.SortFaultStrings(out)
}

func mustEncode(t *testing.T, w *schema.World) []byte {
	t.Helper()
	b, err := EncodeWorld(w)
	if err != nil {
		t.Fatalf("EncodeWorld: %v", err)
	}
	return b
}

// ---------------------------------------------------------------------------
// The type graph itself
// ---------------------------------------------------------------------------

// TestWorldTypeGraphIsCanonicalizable is the guard against the field somebody
// adds later.
//
// Every byte-identity guarantee rests on `.thesis` containing no float, no map
// and no interface{}. A `Confidence float64` or a `Labels map[string]string`
// added to the world in a later phase would compile, pass casual review, and
// silently make world identity platform-dependent. Lint walks the whole type
// graph statically, so it catches a field no test value happens to populate.
func TestWorldTypeGraphIsCanonicalizable(t *testing.T) {
	types := []struct {
		name string
		v    any
	}{
		{"schema.World", schema.World{}},
		{"schema.FaultSchedule", schema.FaultSchedule{}},
		{"schema.RealizedFault", schema.RealizedFault{}},
		{"schema.SUT", schema.SUT{}},
		{"schema.PhaseTimings", schema.PhaseTimings{}},
		{"recorder.Topology", Topology{}},
		{"recorder.Manifest", Manifest{}},
		{"recorder.ClockHealth", ClockHealth{}},
		{"recorder.StreamAudit", StreamAudit{}},
	}
	for _, tc := range types {
		if err := cjson.Lint(tc.v); err != nil {
			t.Errorf("%s is not canonically encodable: %v", tc.name, err)
		}
	}
}

// ---------------------------------------------------------------------------
// Definition of done (b): byte-identity, measured against bytes read from disk
// ---------------------------------------------------------------------------

// TestGoldenWorldRoundTripsThroughDisk is the narrow, readable version of the
// definition-of-done evidence.
//
// It reads the bytes BACK FROM DISK and re-encodes them through the decoder
// that does NOT perform the canonicality comparison. Comparing against
// DecodeWorld's own re-encoding would be circular: DecodeWorld already asserts
// the equality, so the test's own assertion would be unreachable.
func TestGoldenWorldRoundTripsThroughDisk(t *testing.T) {
	w := goldenWorld(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "world.thesis")

	if err := StoreWorld(path, &w); err != nil {
		t.Fatalf("StoreWorld: %v", err)
	}
	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}

	if bytes.IndexByte(onDisk, '\r') >= 0 {
		t.Fatal("the file on disk contains a CR byte")
	}
	if n := bytes.Count(onDisk, []byte{'\n'}); n != 1 || onDisk[len(onDisk)-1] != '\n' {
		t.Fatalf("want exactly one trailing LF, found %d LF bytes", n)
	}
	if !bytes.Contains(onDisk, []byte("n1<->n2")) {
		t.Fatalf("the edge target was escaped on the way to disk; file is:\n%s", onDisk)
	}

	decoded, err := DecodeWorldUnchecked(onDisk)
	if err != nil {
		t.Fatalf("DecodeWorldUnchecked on bytes from disk: %v", err)
	}
	again, err := EncodeWorld(decoded)
	if err != nil {
		t.Fatalf("re-encode: %v", err)
	}
	if !bytes.Equal(onDisk, again) {
		t.Fatalf("byte-identity violated\ndisk: %s\nre:   %s", onDisk, again)
	}
	if !reflect.DeepEqual(w, *decoded) {
		t.Fatalf("semantic round-trip lost information\nwant %+v\ngot  %+v", w, *decoded)
	}

	strict, err := LoadWorld(path)
	if err != nil {
		t.Fatalf("LoadWorld rejected its own output: %v", err)
	}
	hw, err := HashWorld(&w)
	if err != nil {
		t.Fatal(err)
	}
	hs, err := HashWorld(strict)
	if err != nil {
		t.Fatal(err)
	}
	if hw != hs {
		t.Fatalf("hash changed across a round trip: %s -> %s", hw, hs)
	}
}

// TestGeneratedWorldsRoundTripThroughDisk is the property version: many
// generated worlds, each written to and read back from a real file.
//
// The generator is driven by this repository's own PRNG, so a failure is
// itself a seed: invariant I2 applied to the recorder's own test suite.
func TestGeneratedWorldsRoundTripThroughDisk(t *testing.T) {
	dir := t.TempDir()
	gen := NewStream(MustDeriveKey(0xB17E1D, StreamTopologyVariant))

	const n = 300
	hashes := map[string]int{}
	for i := 0; i < n; i++ {
		w := genWorld(t, gen)
		if err := w.Validate(); err != nil {
			t.Fatalf("world %d does not validate: %v", i, err)
		}
		path := filepath.Join(dir, fmt.Sprintf("w%04d.thesis", i))
		if err := StoreWorld(path, &w); err != nil {
			t.Fatalf("world %d: StoreWorld: %v", i, err)
		}
		onDisk, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("world %d: read back: %v", i, err)
		}

		if want := mustEncode(t, &w); !bytes.Equal(onDisk, want) {
			t.Fatalf("world %d: the file on disk differs from the encoding\ndisk %q\nwant %q",
				i, onDisk, want)
		}
		decoded, err := DecodeWorldUnchecked(onDisk)
		if err != nil {
			t.Fatalf("world %d: DecodeWorldUnchecked: %v\nbytes: %s", i, err, onDisk)
		}
		again, err := EncodeWorld(decoded)
		if err != nil {
			t.Fatalf("world %d: re-encode: %v", i, err)
		}
		if !bytes.Equal(onDisk, again) {
			t.Fatalf("world %d: byte-identity violated\ndisk %q\nre   %q", i, onDisk, again)
		}
		if !reflect.DeepEqual(w, *decoded) {
			t.Fatalf("world %d: semantic round-trip lost information", i)
		}
		if _, err := LoadWorld(path); err != nil {
			t.Fatalf("world %d: the strict loader rejected the encoder's own output: %v", i, err)
		}

		h, err := HashWorld(&w)
		if err != nil {
			t.Fatal(err)
		}
		if prev, dup := hashes[h]; dup {
			// Not a failure by itself: the generator may produce the same world
			// twice. But identical hashes must mean identical bytes.
			prevBytes, err := os.ReadFile(filepath.Join(dir, fmt.Sprintf("w%04d.thesis", prev)))
			if err != nil {
				t.Fatal(err)
			}
			if !identicalIdentity(prevBytes, onDisk) {
				t.Fatalf("worlds %d and %d hash alike but differ in their identity fields", prev, i)
			}
		}
		hashes[h] = i
	}
	if len(hashes) < n/2 {
		t.Fatalf("the generator produced only %d distinct worlds out of %d; "+
			"it is not exercising the encoder", len(hashes), n)
	}
}

// identicalIdentity compares two encoded worlds ignoring the unhashed fields,
// so a legitimate hash collision between provenance-only variants is not
// reported as a failure.
func identicalIdentity(a, b []byte) bool {
	strip := func(x []byte) map[string]json.RawMessage {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(x, &m); err != nil {
			return nil
		}
		delete(m, "meta")
		delete(m, "schema")
		return m
	}
	return reflect.DeepEqual(strip(a), strip(b))
}

// TestEncodingIsInjectiveOverIdentity checks both directions: worlds that mean
// the same thing must encode the same, and worlds that differ by one
// millisecond must encode and hash differently.
func TestEncodingIsInjectiveOverIdentity(t *testing.T) {
	base := goldenWorld(t)

	// Same value, independently constructed: identical bytes.
	other := goldenWorld(t)
	if !bytes.Equal(mustEncode(t, &base), mustEncode(t, &other)) {
		t.Fatal("two equal worlds encoded differently")
	}

	// One millisecond apart: different bytes AND a different identity.
	shifted := base
	shifted.PhaseTimings = append(schema.PhaseTimings(nil), base.PhaseTimings...)
	shifted.PhaseTimings[0].EndMS++
	if bytes.Equal(mustEncode(t, &base), mustEncode(t, &shifted)) {
		t.Fatal("a one-millisecond difference encoded identically")
	}
	hb, _ := HashWorld(&base)
	hs, _ := HashWorld(&shifted)
	if hb == hs {
		t.Fatal("a one-millisecond difference hashed identically")
	}

	// Provenance differs: different bytes, SAME identity. This is what lets the
	// same world found on two commits dedupe in the corpus.
	reprovenanced := base
	reprovenanced.Meta = &schema.WorldMeta{Origin: "search", ParentHash: ""}
	if bytes.Equal(mustEncode(t, &base), mustEncode(t, &reprovenanced)) {
		t.Fatal("a provenance change did not change the file")
	}
	hr, _ := HashWorld(&reprovenanced)
	if hb != hr {
		t.Fatalf("a provenance change altered the world identity: %s -> %s", hb, hr)
	}

	// An absent meta and an absent meta must agree; the omitempty pointer is a
	// bijection only if that holds.
	noMeta := base
	noMeta.Meta = nil
	if _, err := DecodeWorldUnchecked(mustEncode(t, &noMeta)); err != nil {
		t.Fatalf("a world with no meta does not decode: %v", err)
	}
	round, err := DecodeWorldUnchecked(mustEncode(t, &noMeta))
	if err != nil {
		t.Fatal(err)
	}
	if round.Meta != nil {
		t.Fatal("an omitted meta decoded as a non-nil pointer")
	}
}

// ---------------------------------------------------------------------------
// The strict decoder rejects non-canonical input
// ---------------------------------------------------------------------------

// TestNonCanonicalInputIsRejected is the mutation suite.
//
// For the mutations that are semantically LOSSLESS (reordered keys, inserted
// whitespace, a duplicated key, an HTML-escaped '<') the test does more than
// check that an error came back. It also asserts that the unchecked decoder
// read exactly the same world, and that re-encoding that world reproduces the
// original golden bytes. That proves the rejection came from the canonicality
// gate rather than from an unrelated parse failure, and it proves the decoder
// is not quietly disagreeing about the value.
func TestNonCanonicalInputIsRejected(t *testing.T) {
	w := goldenWorld(t)
	golden := mustEncode(t, &w)

	lossless := []struct {
		name string
		make func([]byte) []byte
	}{
		{"unsorted object keys", reverseTopLevelKeys},
		{"inserted whitespace", func(b []byte) []byte {
			return append([]byte("{ "), b[1:]...)
		}},
		{"duplicate key", func(b []byte) []byte {
			// Last one wins in encoding/json, so the decoded world is unchanged
			// while the bytes carry a member that is not in the canonical form.
			return append([]byte(`{"seed":999999,`), b[1:]...)
		}},
		{"html-escaped '<'", func(b []byte) []byte {
			// This is what encoding/json emits by default, and what would
			// corrupt the edge target n1<->n2 if it ever reached the `.thesis`
			// encoder.
			return bytes.Replace(b, []byte("n1<"), []byte("n1"+jsonEsc("003c")), 1)
		}},
		{"non-minimal escape", func(b []byte) []byte {
			return bytes.Replace(b, []byte(`"kv3"`), []byte(`"`+jsonEsc("006b")+`v3"`), 1)
		}},
	}
	for _, tc := range lossless {
		t.Run(tc.name, func(t *testing.T) {
			mutated := tc.make(append([]byte(nil), golden...))
			if bytes.Equal(mutated, golden) {
				t.Fatal("the mutation did not change the bytes; the test proves nothing")
			}
			if _, err := DecodeWorld(mutated); err == nil {
				t.Fatal("the strict loader accepted non-canonical bytes")
			}
			got, err := DecodeWorldUnchecked(mutated)
			if err != nil {
				t.Fatalf("the mutation was supposed to be semantically lossless, "+
					"but the unchecked decoder rejected it: %v", err)
			}
			back, err := EncodeWorld(got)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(back, golden) {
				t.Fatalf("the decoder read a different world from a lossless mutation\n"+
					"want %s\ngot  %s", golden, back)
			}
		})
	}

	framing := []struct {
		name    string
		make    func([]byte) []byte
		wantErr error
	}{
		{"CRLF", func(b []byte) []byte {
			return bytes.ReplaceAll(b, []byte("\n"), []byte("\r\n"))
		}, cjson.ErrCRLF},
		{"UTF-8 BOM", func(b []byte) []byte {
			return append([]byte{0xEF, 0xBB, 0xBF}, b...)
		}, cjson.ErrBOM},
		{"missing trailing LF", func(b []byte) []byte {
			return b[:len(b)-1]
		}, cjson.ErrMissingTrailingLF},
		{"extra trailing LF", func(b []byte) []byte {
			return append(b, '\n')
		}, cjson.ErrMissingTrailingLF},
	}
	for _, tc := range framing {
		t.Run(tc.name, func(t *testing.T) {
			mutated := tc.make(append([]byte(nil), golden...))
			_, err := DecodeWorld(mutated)
			if err == nil {
				t.Fatal("the strict loader accepted a badly framed document")
			}
			if !strings.Contains(err.Error(), tc.wantErr.Error()) {
				t.Fatalf("want an error naming %v, got %v", tc.wantErr, err)
			}
		})
	}

	t.Run("unknown field", func(t *testing.T) {
		mutated := append([]byte(`{"bogus":1,`), golden[1:]...)
		if _, err := DecodeWorldUnchecked(mutated); err == nil {
			t.Fatal("an unknown field was accepted")
		}
	})

	t.Run("trailing data", func(t *testing.T) {
		mutated := append(append([]byte(nil), golden[:len(golden)-1]...), []byte("{}\n")...)
		if _, err := DecodeWorldUnchecked(mutated); err == nil {
			t.Fatal("trailing data was accepted")
		}
	})
}

// jsonEsc builds a JSON \uXXXX escape sequence without a backslash literal, so
// the mutation text in this file cannot be silently rewritten by an editor or a
// line-ending filter.
func jsonEsc(hex4 string) string { return string(rune(0x5C)) + "u" + hex4 }

// reverseTopLevelKeys re-emits a canonical document with its top-level members
// in descending key order. Every value is copied verbatim, so the document is
// semantically identical and differs from the canonical form only in ordering.
func reverseTopLevelKeys(b []byte) []byte {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return b
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(keys)))
	var out bytes.Buffer
	out.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			out.WriteByte(',')
		}
		fmt.Fprintf(&out, "%q:", k)
		out.Write(m[k])
	}
	out.WriteString("}\n")
	return out.Bytes()
}

// TestSingleByteMutationsAreDetected sweeps every byte of the golden document.
//
// The assertion is deliberately NOT "the loader returned an error". An error is
// the trivially satisfied outcome, and it is also the WRONG expectation: some
// single-byte mutations (a digit inside a number) produce a perfectly canonical
// encoding of a DIFFERENT world, which the strict loader is right to accept.
//
// The real property is about bytes the decoder might IGNORE. If a byte is
// ignored, mutating it yields the same world from different bytes. So: whenever
// the unchecked decoder succeeds, the world it produced must differ from the
// golden world; and whenever it produces the same world, the bytes must have
// been rejected.
func TestSingleByteMutationsAreDetected(t *testing.T) {
	w := goldenWorld(t)
	golden := mustEncode(t, &w)
	want, err := DecodeWorldUnchecked(golden)
	if err != nil {
		t.Fatal(err)
	}

	replacements := []byte{'0', '9', 'x', '"'}
	decoded, rejected := 0, 0
	for i := 0; i < len(golden); i++ {
		for _, r := range replacements {
			if golden[i] == r {
				continue
			}
			mutated := append([]byte(nil), golden...)
			mutated[i] = r

			got, err := DecodeWorldUnchecked(mutated)
			if err != nil {
				rejected++
				continue // rejected outright; nothing was ignored
			}
			decoded++
			if reflect.DeepEqual(*want, *got) {
				t.Fatalf("offset %d -> %q: the decoder produced the golden world from different bytes, "+
					"so it ignored that byte\nbytes: %s", i, r, mutated)
			}
			// A mutation that still decodes must re-encode to itself or be
			// reported as non-canonical; it may never round-trip to the golden
			// bytes.
			back, err := EncodeWorld(got)
			if err != nil {
				continue
			}
			if bytes.Equal(back, golden) {
				t.Fatalf("offset %d -> %q: mutated bytes re-encoded to the golden document", i, r)
			}
		}
	}
	if decoded == 0 || rejected == 0 {
		t.Fatalf("the sweep is one-sided: %d mutations decoded, %d were rejected", decoded, rejected)
	}
}

// FuzzWorldCanonicalForm explores the language the decoder accepts.
//
// The property checked is idempotence plus semantic round-trip through the
// UNCHECKED decoder, which is not implied by anything the decoder does
// internally: a decoder that dropped a field, or an encoder that emitted a
// field the decoder could not read back, fails here.
func FuzzWorldCanonicalForm(f *testing.F) {
	w := schema.NewWorld(0x2a, "kv3", "gate")
	w.PhaseTimings = schema.PhaseTimings{{Phase: schema.PhaseDrive, StartMS: 0, EndMS: 1}}
	seed, err := EncodeWorld(&w)
	if err != nil {
		f.Fatal(err)
	}
	f.Add(seed)
	f.Add([]byte("{}\n"))
	f.Add([]byte(`{"seed":0}` + "\n"))

	f.Fuzz(func(t *testing.T, b []byte) {
		first, err := DecodeWorldUnchecked(b)
		if err != nil {
			return
		}
		enc1, err := EncodeWorld(first)
		if err != nil {
			// The only way encoding can fail after a successful decode is a
			// string encoding/json accepted that cjson refuses; that is a real
			// asymmetry and worth surfacing.
			t.Fatalf("a decoded world failed to encode: %v", err)
		}
		second, err := DecodeWorldUnchecked(enc1)
		if err != nil {
			t.Fatalf("the canonical encoding does not re-parse: %v\nbytes: %q", err, enc1)
		}
		enc2, err := EncodeWorld(second)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(enc1, enc2) {
			t.Fatalf("encoding is not idempotent\nfirst  %q\nsecond %q", enc1, enc2)
		}
		if !reflect.DeepEqual(*first, *second) {
			t.Fatalf("the canonical encoding lost information\nwant %+v\ngot  %+v", *first, *second)
		}
		if err := cjson.UnmarshalCanonical(enc1, new(schema.World)); err != nil {
			t.Fatalf("the encoder produced bytes its own strict decoder rejects: %v", err)
		}
	})
}

// TestRecorderAndSchemaCodecsAgree asserts that the two `.thesis` entry points
// (recorder.EncodeWorld/DecodeWorld and schema.MarshalWorld/UnmarshalWorld) are
// the same function over a realistic corpus, and that each accepts the other's
// output.
//
// The tree briefly had two independent canonical encoders and they provably
// disagreed; schema.MarshalWorld now delegates to cjson, so exactly one
// definition of canonical form exists and this test is a regression guard
// against re-splitting it rather than a bug hunt. It is deliberately kept:
// nothing in the type system stops a future edit from reintroducing a second
// encoder, and this is where that would surface.
//
// The corpus here is deliberately realistic (every string that can appear in a
// world is an identifier, a fault-grammar string, an image reference or a
// content address) and includes the edge target `n1<->n2`, which is where two
// encoders part company first, because encoding/json escapes '<' by default.
// TestCodecsAgreeOnEveryStringByte covers the inputs a realistic corpus misses.
func TestRecorderAndSchemaCodecsAgree(t *testing.T) {
	check := func(t *testing.T, w *schema.World) {
		t.Helper()
		mine, err := EncodeWorld(w)
		if err != nil {
			t.Fatalf("recorder encoder: %v", err)
		}
		theirs, err := schema.MarshalWorld(w)
		if err != nil {
			t.Fatalf("schema encoder: %v", err)
		}
		if !bytes.Equal(mine, theirs) {
			off := cjson.FirstDiff(mine, theirs)
			t.Fatalf("the two canonical encoders disagree at byte %d\nrecorder %s\nschema   %s",
				off, mine, theirs)
		}
		// And each must accept the other's output.
		if _, err := DecodeWorld(theirs); err != nil {
			t.Fatalf("the recorder rejected the schema encoder's output: %v", err)
		}
		if _, err := schema.UnmarshalWorld(mine); err != nil {
			t.Fatalf("the schema decoder rejected the recorder's output: %v", err)
		}
	}

	t.Run("golden", func(t *testing.T) {
		w := goldenWorld(t)
		check(t, &w)
	})
	t.Run("generated", func(t *testing.T) {
		gen := NewStream(MustDeriveKey(0xC0DEC, StreamTopologyVariant))
		for i := 0; i < 200; i++ {
			w := genWorld(t, gen)
			check(t, &w)
		}
	})
}

// ---------------------------------------------------------------------------
// Store/Load behaviour
// ---------------------------------------------------------------------------

func TestStoreWorldRefusesInvalidWorlds(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.thesis")

	var empty schema.World
	if err := StoreWorld(path, &empty); err == nil {
		t.Fatal("StoreWorld accepted a world with no schema id")
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("StoreWorld created a file for a world it rejected")
	}

	w := goldenWorld(t)
	w.FaultSchedule.Planned = nil // null, not []
	if err := StoreWorld(path, &w); err == nil {
		t.Fatal("StoreWorld accepted a null planned schedule")
	}
}

func TestLoadWorldExpectingDetectsDrift(t *testing.T) {
	w := goldenWorld(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "w.thesis")
	if err := StoreWorld(path, &w); err != nil {
		t.Fatal(err)
	}
	h, err := HashWorld(&w)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := LoadWorldExpecting(path, h); err != nil {
		t.Fatalf("LoadWorldExpecting rejected a matching hash: %v", err)
	}
	_, err = LoadWorldExpecting(path, schema.HashPrefix+strings.Repeat("00", 32))
	var mismatch *ErrHashMismatch
	if !errors.As(err, &mismatch) {
		t.Fatalf("want an ErrHashMismatch, got %v", err)
	}
	if mismatch.Got != h {
		t.Fatalf("the mismatch error reports %q, want the real hash %q", mismatch.Got, h)
	}
}

func TestWorldFileNameIsContentAddressed(t *testing.T) {
	w := goldenWorld(t)
	name, err := WorldFileName(&w)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(name, schema.WorldIDPrefix) || !strings.HasSuffix(name, schema.WorldFileExt) {
		t.Fatalf("unexpected world file name %q", name)
	}
	if err := SafeName(name); err != nil {
		t.Fatalf("world file name is not a safe path component: %v", err)
	}
	h, err := HashWorld(&w)
	if err != nil {
		t.Fatal(err)
	}
	short := strings.TrimSuffix(strings.TrimPrefix(name, schema.WorldIDPrefix), schema.WorldFileExt)
	if want := strings.TrimPrefix(h, schema.HashPrefix)[:schema.ShortIDLen]; short != want {
		t.Fatalf("file name carries short id %q, want %q (it must address the content)", short, want)
	}
}

// TestStoreWorldNoClobberRefusesToLoseARegression covers the consequence of a
// 4-hex short id: two different worlds can want the same filename, and in the
// committed, append-only regression corpus an overwrite is the deletion of a
// regression test.
func TestStoreWorldNoClobberRefusesToLoseARegression(t *testing.T) {
	dir := t.TempDir()
	w := goldenWorld(t)

	path, err := StoreWorldNoClobber(dir, &w)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the world was not written: %v", err)
	}

	// Storing the same world again is a no-op, not an error.
	again, err := StoreWorldNoClobber(dir, &w)
	if err != nil {
		t.Fatalf("re-storing an identical world failed: %v", err)
	}
	if again != path {
		t.Fatalf("path changed on re-store: %q -> %q", path, again)
	}

	// A DIFFERENT world forced into the same filename must be refused rather
	// than silently replacing the first.
	other := goldenWorld(t)
	other.Seed++
	forced := filepath.Join(dir, filepath.Base(path))
	if err := StoreWorld(forced+".probe", &other); err != nil {
		t.Fatal(err)
	}
	// Simulate the short-id collision by writing `other` under `w`'s name.
	collided := t.TempDir()
	if err := StoreWorld(filepath.Join(collided, filepath.Base(path)), &other); err != nil {
		t.Fatal(err)
	}
	_, err = StoreWorldNoClobber(collided, &w)
	var collision *ErrWorldFileCollision
	if !errors.As(err, &collision) {
		t.Fatalf("want an ErrWorldFileCollision, got %v", err)
	}
	if collision.Existing == collision.Incoming {
		t.Fatal("the collision error reports one hash twice")
	}

	// A file that exists but does not load is never overwritten either: doing so
	// would destroy the evidence that a committed world was corrupted.
	corrupt := t.TempDir()
	if err := os.WriteFile(filepath.Join(corrupt, filepath.Base(path)), []byte("not json\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := StoreWorldNoClobber(corrupt, &w); err == nil {
		t.Fatal("an unreadable world file was overwritten")
	}
}

// TestWriteFileAtomicReplaces checks that a second write fully replaces the
// first, leaving no temporary files behind.
func TestWriteFileAtomicReplaces(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "sub", "a.json")
	if err := WriteFileAtomic(p, []byte("first-and-longer\n")); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileAtomic(p, []byte("second\n")); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "second\n" {
		t.Fatalf("content is %q, want %q", got, "second\n")
	}
	entries, err := os.ReadDir(filepath.Dir(p))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("a temporary file was left behind: %s", e.Name())
		}
	}
}

// ---------------------------------------------------------------------------
// Generator
// ---------------------------------------------------------------------------

var (
	genTopologies = []string{"kv3", "kv5", "kv3-pg", "kv3_variant.b"}
	genProfiles   = []string{"smoke", "gate", "soak"}
	genFaults     = []string{
		"proc.pause(role:leader)@8200..11000",
		"proc.pause(kv:*)@1000..2000",
		"net.partition(minority(kv))@8400..10900",
		"net.partition(majority(kv))@100..200",
		"net.partition(n1<->n2)@500..1500",
		"net.latency(kv:*, mean=150, jitter=20)@1000..2000",
		"net.loss(n1<->n2, pct=5)@500..1500",
		"clock.skew(n2, ms=-400)@2000..6000",
		"proc.slow(n3, cpu_pct=30)@0..1",
	}
	genOrigins = []string{"init", "search", "shrink", "replay"}
	genImages  = []string{"kvfixture:buggy", "kvfixture:kvfixed", "postgres:16-alpine"}
)

// genWorld builds an arbitrary valid world from a deterministic stream.
func genWorld(t *testing.T, s *Stream) schema.World {
	t.Helper()
	w := schema.NewWorld(
		s.Uint64(),
		genTopologies[s.Uint64n(uint64(len(genTopologies)))],
		genProfiles[s.Uint64n(uint64(len(genProfiles)))],
	)

	nf := int(s.Uint64n(6))
	picked := make([]string, 0, nf)
	used := map[string]bool{}
	for i := 0; i < nf; i++ {
		f := genFaults[s.Uint64n(uint64(len(genFaults)))]
		if used[f] {
			continue
		}
		used[f] = true
		picked = append(picked, f)
	}
	w.FaultSchedule.Planned = canonicalFaults(t, picked...)

	// realized: nil half the time (never executed), otherwise a possibly-empty
	// list. Both shapes must round-trip, and they mean different things.
	if s.Uint64n(2) == 1 {
		realized := make([]schema.RealizedFault, 0, len(w.FaultSchedule.Planned))
		for i, f := range w.FaultSchedule.Planned {
			if s.Uint64n(4) == 0 {
				continue
			}
			nodes := []string{}
			for j := 0; j <= int(s.Uint64n(3)); j++ {
				nodes = append(nodes, fmt.Sprintf("n%d", j+1))
			}
			start := int64(s.Uint64n(20000))
			realized = append(realized, schema.RealizedFault{
				Fault:    f,
				Resolved: canonicalFaults(t, fmt.Sprintf("proc.pause(n%d)@%d..%d", i%3+1, start, start+int64(s.Uint64n(5000))))[0],
				Nodes:    nodes,
				StartMS:  start,
				EndMS:    start + int64(s.Uint64n(5000)),
			})
		}
		w.FaultSchedule.Realized = realized
	}

	drive := int64(s.Uint64n(60000)) + 1
	heal := int64(s.Uint64n(5000))
	quiesce := int64(s.Uint64n(5000))
	pt := schema.PhaseTimings{
		{Phase: schema.PhaseDrive, StartMS: 0, EndMS: drive},
		{Phase: schema.PhaseHeal, StartMS: drive, EndMS: drive + heal},
		{Phase: schema.PhaseQuiesce, StartMS: drive + heal, EndMS: drive + heal + quiesce},
	}
	if s.Uint64n(2) == 1 {
		// BOOT and SEED sit BEFORE the origin, so their bounds are negative.
		boot := int64(s.Uint64n(40000)) + 1
		pt = append(schema.PhaseTimings{
			{Phase: schema.PhaseBoot, StartMS: -boot, EndMS: -boot / 2},
			{Phase: schema.PhaseSeed, StartMS: -boot / 2, EndMS: 0},
		}, pt...)
	}
	w.PhaseTimings = pt

	ni := int(s.Uint64n(4))
	images := make([]schema.SUTImage, 0, ni)
	for i := 0; i < ni; i++ {
		images = append(images, schema.SUTImage{
			Service: fmt.Sprintf("svc%d", i),
			Image:   genImages[s.Uint64n(uint64(len(genImages)))],
			Digest:  schema.HashPrefix + fmt.Sprintf("%064x", s.Uint64()),
		})
	}
	w.SUT.Images = images

	switch s.Uint64n(3) {
	case 0:
		// no meta
	case 1:
		w.Meta = &schema.WorldMeta{Origin: genOrigins[s.Uint64n(uint64(len(genOrigins)))]}
	case 2:
		w.Meta = &schema.WorldMeta{
			Origin:     genOrigins[s.Uint64n(uint64(len(genOrigins)))],
			ParentHash: schema.HashPrefix + fmt.Sprintf("%064x", s.Uint64()),
		}
	}
	return w.Normalized()
}
