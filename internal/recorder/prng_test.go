package recorder

import (
	"encoding/hex"
	"math"
	"reflect"
	"testing"
)

// ---------------------------------------------------------------------------
// The bit source is real ChaCha20
// ---------------------------------------------------------------------------

// TestChaChaRFC8439Vectors checks the block function against RFC 8439's own
// published vectors.
//
// This is the only test in the PRNG suite whose expected values come from
// OUTSIDE this repository. Everything else below pins what this implementation
// does; this pins that what it does is the published algorithm. Without it, a
// transposed rotation constant would be deterministic, biased, and invisible:
// every determinism test would still be green while the distribution was skewed.
func TestChaChaRFC8439Vectors(t *testing.T) {
	cases := []struct {
		name    string
		key     [32]byte
		nonce   [12]byte
		counter uint32
		want    string
	}{
		{
			name: "A.1 test vector #1 (zero key, zero nonce, counter 0)",
			want: "76b8e0ada0f13d90405d6ae55386bd28bdd219b8a08ded1aa836efcc8b770dc7" +
				"da41597c5157488d7724e03fb8d84a376a43b8f41518a11cc387b669b2ee6586",
		},
		{
			name:    "A.1 test vector #2 (zero key, zero nonce, counter 1)",
			counter: 1,
			want: "9f07e7be5551387a98ba977c732d080dcb0f29a048e3656912c6533e32ee7aed" +
				"29b721769ce64e43d57133b074d839d531ed1f28510afb45ace10a1f4b794d6f",
		},
		{
			name:    "2.3.2 (key 00..1f, nonce 000000090000004a00000000, counter 1)",
			key:     [32]byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21, 22, 23, 24, 25, 26, 27, 28, 29, 30, 31},
			nonce:   [12]byte{0, 0, 0, 0x09, 0, 0, 0, 0x4a, 0, 0, 0, 0},
			counter: 1,
			want: "10f1e7e4d13b5915500fdd1fa32071c4c7d1f4c733c068030422aa9ac3d46c4e" +
				"d2826446079faa0914c2d705d98b02a2b5129cd1de164eb9cbd083e8a2503c4e",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out [64]byte
			k, n := tc.key, tc.nonce
			chachaBlock(&k, &n, tc.counter, &out)
			if got := hex.EncodeToString(out[:]); got != tc.want {
				t.Fatalf("block mismatch\n got %s\nwant %s", got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// G2: a stream's values are a function of (root seed, path) and nothing else
// ---------------------------------------------------------------------------

// TestNewStreamDoesNotPerturbExistingStreams is the property PHASE0_BUILD_BRIEF
// §3 and D-013 exist for:
//
//	adding a NEW named stream must not change any value produced by an EXISTING
//	stream for the same root seed.
//
// It is what stops a later phase from invalidating every committed regression
// world by registering the streams its own engine needs. Sequential
// child-seeding would fail this test; path-keying passes it structurally.
func TestNewStreamDoesNotPerturbExistingStreams(t *testing.T) {
	const seed = Seed(0x2a)
	const n = 32

	before := drawAllRegistered(seed, n)

	// A later phase registers additional streams and consumes heavily from
	// them, in a different order, before the existing streams are touched.
	saved := RegisteredStreams
	t.Cleanup(func() { RegisteredStreams = saved })
	RegisteredStreams = append(append([]string(nil), saved...),
		"zz.saboteur.mutate", "zz.saboteur.select")

	future := NewStreams(seed)
	for _, d := range []string{"zz.saboteur.select", "zz.saboteur.mutate"} {
		st := future.Get(d)
		for i := 0; i < 10_000; i++ {
			st.Uint64()
		}
	}

	after := drawAllRegistered(seed, n)
	for _, d := range saved {
		if !reflect.DeepEqual(before[d], after[d]) {
			t.Fatalf("stream %q changed after new streams were registered and consumed:\n"+
				"before %v\nafter  %v", d, before[d], after[d])
		}
	}
	// And the new streams must actually produce something, or the test above
	// would pass for the trivial reason that nothing happened.
	if got := NewStreams(seed).Get("zz.saboteur.mutate").Uint64(); got == 0 {
		t.Fatal("new stream produced a zero first value; suspicious enough to check by hand")
	}
}

func drawAllRegistered(seed Seed, n int) map[string][]uint64 {
	out := map[string][]uint64{}
	s := NewStreams(seed)
	for _, d := range RegisteredStreams {
		st := s.Get(d)
		v := make([]uint64, n)
		for i := range v {
			v[i] = st.Uint64()
		}
		out[d] = v
	}
	return out
}

// TestStreamIndependenceUnderConsumption checks the other direction: how much
// anyone draws from stream A cannot move stream B by one bit.
func TestStreamIndependenceUnderConsumption(t *testing.T) {
	const seed = Seed(0xdeadbeefcafe)
	var want []uint64
	for k := 0; k <= 200; k++ {
		s := NewStreams(seed)
		a := s.Get(StreamFaultSchedule)
		for i := 0; i < k; i++ {
			a.Uint64()
		}
		b := s.Get(StreamDriverWorkload)
		got := make([]uint64, 16)
		for i := range got {
			got[i] = b.Uint64()
		}
		if k == 0 {
			want = got
			continue
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("after %d draws from %s, %s produced %v, want %v",
				k, StreamFaultSchedule, StreamDriverWorkload, got, want)
		}
	}
}

// TestPathSeparatorCannotCollide checks the reason the path separator is NUL:
// a separator the segment grammar permits would let two different paths hash
// the same message, and two logically distinct streams would silently share a
// keystream.
func TestPathSeparatorCannotCollide(t *testing.T) {
	const seed = Seed(7)
	a := MustDeriveKey(seed, "a", "b/c")
	b := MustDeriveKey(seed, "a/b", "c")
	if a == b {
		t.Fatal(`Derive("a","b/c") collided with Derive("a/b","c")`)
	}
	// A prefix relationship must not collide either.
	if MustDeriveKey(seed, "ab") == MustDeriveKey(seed, "a", "b") {
		t.Fatal(`Derive("ab") collided with Derive("a","b")`)
	}
	// A segment carrying the separator itself is rejected rather than silently
	// folded into the path.
	if _, err := DeriveKey(seed, "a\x00b"); err == nil {
		t.Fatal("a segment containing NUL must be rejected")
	}
	if _, err := DeriveKey(seed); err != ErrEmptyPath {
		t.Fatalf("empty path: got %v, want %v", err, ErrEmptyPath)
	}
}

// TestDeriveKeyIsPure checks that a key is a function of (seed, path) only.
func TestDeriveKeyIsPure(t *testing.T) {
	for _, seed := range []Seed{0, 1, 42, 1 << 63, Seed(^uint64(0))} {
		k1 := MustDeriveKey(seed, StreamDriverWorkload)
		k2 := MustDeriveKey(seed, StreamDriverWorkload)
		if k1 != k2 {
			t.Fatalf("seed %d: DeriveKey is not deterministic", seed)
		}
		if k1 == MustDeriveKey(seed+1, StreamDriverWorkload) {
			t.Fatalf("seed %d: neighbouring seeds derive the same key", seed)
		}
		if k1 == MustDeriveKey(seed, StreamFaultSchedule) {
			t.Fatalf("seed %d: different domains derive the same key", seed)
		}
		if k1 == RootKey(seed) {
			t.Fatalf("seed %d: a stream key collided with the root key", seed)
		}
	}
}

// ---------------------------------------------------------------------------
// Frozen goldens
// ---------------------------------------------------------------------------

// TestFrozenStreamGoldens pins the exact byte sequence this construction
// produces.
//
// Every archived world depends on these values. If a Go upgrade, a refactor or
// a "harmless" change to the derivation moves any of them, every committed
// regression world silently starts executing a different program: with no
// change to any world's identity, so nothing else would notice. This test is
// the alarm.
func TestFrozenStreamGoldens(t *testing.T) {
	const seed = Seed(0x2a)

	t.Run("root key", func(t *testing.T) {
		const want = "33f113ab45173ae8b05ffbad00ccdfa096c2558dd1ecad6e4a74a3a2860bddc8"
		if got := RootKey(seed).String(); got != want {
			t.Fatalf("root key\n got %s\nwant %s", got, want)
		}
	})

	t.Run("domain keys", func(t *testing.T) {
		want := map[string]string{
			StreamClientSchedule:  "60e588db839603a347cbf5172f9910a911017504d18c3ad55d80a09ab6a8b08a",
			StreamDriverWorkload:  "0c2327e23797be881274d9984b90f428d7e333eef667a1ab045e16ea37d18c01",
			StreamFaultSchedule:   "fc6e948bd3f742f9715798c921f64fabdf2f340cc5f9671abc0905f571883def",
			StreamInjectDelay:     "2dbb2caf0ab25b696f60ecbe6ab7800c9098ac7a9f43adb6743d8d02239e3509",
			StreamTopologyVariant: "e35ad8f33300c444049771d7ff83ee488314d441e2e65c74d7f2b18c07c57aad",
		}
		for d, w := range want {
			if got := MustDeriveKey(seed, d).String(); got != w {
				t.Errorf("key %s\n got %s\nwant %s", d, got, w)
			}
		}
		if len(want) != len(RegisteredStreams) {
			t.Errorf("golden table covers %d domains, %d are registered; "+
				"a new domain needs a golden entry", len(want), len(RegisteredStreams))
		}
	})

	t.Run("uint64", func(t *testing.T) {
		want := map[string][]uint64{
			StreamClientSchedule:  {0x1fc632ffacdf16f0, 0x205e46b7c4183728, 0xf2eefe6f405a29b8, 0x1520ce39861ac250},
			StreamDriverWorkload:  {0x6b80af5a4c7c8256, 0x88075eb28d1c0a5f, 0x5911a93bd33bdf53, 0xdbcd80cf7723383e},
			StreamFaultSchedule:   {0x4de0a8d337bd865b, 0xf1f071e003420a5d, 0x05c68cae5396b1d6, 0xbbf2503eaca845a9},
			StreamInjectDelay:     {0x5512495f25f1ee31, 0x056a27b141d2bade, 0x13ae3788cb0ba1d6, 0xc1b18874b283d059},
			StreamTopologyVariant: {0x493aa3cd6fe920e4, 0x3b6c4e908775d7be, 0xad7af79799b7a50c, 0xfa5e1ee4ec3d5806},
		}
		s := NewStreams(seed)
		for _, d := range RegisteredStreams {
			st := s.Get(d)
			got := make([]uint64, 4)
			for i := range got {
				got[i] = st.Uint64()
			}
			if !reflect.DeepEqual(got, want[d]) {
				t.Errorf("uint64 %s\n got %#v\nwant %#v", d, got, want[d])
			}
		}
	})

	t.Run("uint64n", func(t *testing.T) {
		want := []uint64{419, 531, 347, 858, 985, 726, 501, 90}
		st := NewStream(MustDeriveKey(seed, StreamDriverWorkload))
		got := make([]uint64, len(want))
		for i := range got {
			got[i] = st.Uint64n(1000)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("uint64n(1000)\n got %v\nwant %v", got, want)
		}
	})

	t.Run("float64", func(t *testing.T) {
		// Compared as IEEE-754 bit patterns, never as decimal text: a decimal
		// comparison would hide a one-ULP difference, which is exactly the kind
		// of drift this test exists to catch.
		want := []uint64{
			0x3fdae02bd6931f20, 0x3fe100ebd651a381, 0x3fd6446a4ef4cef6,
			0x3feb79b019eee467, 0x3fef8664b4a0f9db, 0x3fe73f9daa072c29,
		}
		st := NewStream(MustDeriveKey(seed, StreamDriverWorkload))
		got := make([]uint64, len(want))
		for i := range got {
			f := st.Float64()
			if f < 0 || f >= 1 {
				t.Fatalf("Float64 returned %v, outside [0,1)", f)
			}
			got[i] = math.Float64bits(f)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("float64 bits\n got %#v\nwant %#v", got, want)
		}
	})

	t.Run("shuffle", func(t *testing.T) {
		want := []int{12, 15, 7, 2, 11, 6, 8, 1, 5, 13, 3, 10, 9, 0, 14, 4}
		st := NewStream(MustDeriveKey(seed, StreamFaultSchedule))
		got := make([]int, 16)
		for i := range got {
			got[i] = i
		}
		st.Shuffle(len(got), func(i, j int) { got[i], got[j] = got[j], got[i] })
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("shuffle\n got %v\nwant %v", got, want)
		}
	})

	t.Run("weighted ppm", func(t *testing.T) {
		want := []int{0, 0, 2, 0, 1, 2, 1, 2, 2, 0, 1, 1}
		st := NewStream(MustDeriveKey(seed, StreamClientSchedule))
		got := make([]int, len(want))
		for i := range got {
			got[i] = st.WeightedPPM([]uint32{400000, 400000, 200000})
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("weighted ppm\n got %v\nwant %v", got, want)
		}
	})

	t.Run("index sub-stream", func(t *testing.T) {
		const want = uint64(0xe74fb43483124ceb)
		st := NewStream(MustDeriveKey(seed, StreamInjectDelay))
		if got := st.Index(7).Uint64(); got != want {
			t.Fatalf("Index(7).Uint64() = %#016x, want %#016x", got, want)
		}
	})

	t.Run("world seeds", func(t *testing.T) {
		want := []Seed{0x90ed2465d9c2bdbb, 0x0669f700ddc05551}
		for i, w := range want {
			if got := WorldSeed(seed, uint64(i)); got != w {
				t.Errorf("WorldSeed(%d) = %#016x, want %#016x", i, uint64(got), uint64(w))
			}
		}
	})
}

// ---------------------------------------------------------------------------
// Sub-stream addressing
// ---------------------------------------------------------------------------

// TestIndexIsIndependentOfParentConsumption is the property Phase 5's shrinker
// depends on: deleting one element must be invisible to every other element.
func TestIndexIsIndependentOfParentConsumption(t *testing.T) {
	k := MustDeriveKey(1234, StreamDriverWorkload)
	want := make([]uint64, 8)
	for i := range want {
		want[i] = NewStream(k).Index(uint64(i)).Uint64()
	}
	// Consume heavily from the parent and from other indices, then re-read.
	parent := NewStream(k)
	for i := 0; i < 5000; i++ {
		parent.Uint64()
	}
	for i := 100; i < 200; i++ {
		parent.Index(uint64(i)).Uint64()
	}
	for i := range want {
		if got := parent.Index(uint64(i)).Uint64(); got != want[i] {
			t.Fatalf("Index(%d) moved: got %#x want %#x", i, got, want[i])
		}
	}
	// Distinct indices must not alias.
	seen := map[uint64]int{}
	for i := 0; i < 500; i++ {
		v := NewStream(k).Index(uint64(i)).Uint64()
		if j, dup := seen[v]; dup {
			t.Fatalf("Index(%d) and Index(%d) produced the same first value", j, i)
		}
		seen[v] = i
	}
}

// TestSequentialDoesNotAliasIndexZero checks the nonce discriminator byte. With
// a naive nonce = index encoding, sequential consumption of a stream and its
// Index(0) sub-stream would be the same keystream.
func TestSequentialDoesNotAliasIndexZero(t *testing.T) {
	k := MustDeriveKey(99, StreamFaultSchedule)
	if NewStream(k).Uint64() == NewStream(k).Index(0).Uint64() {
		t.Fatal("sequential stream aliases Index(0)")
	}
}

// TestSeek checks that Seek positions the stream exactly where sequential
// consumption would have left it, and that an out-of-range Seek errors rather
// than silently wrapping to a plausible-looking but wrong position.
func TestSeek(t *testing.T) {
	k := MustDeriveKey(5, StreamInjectDelay)
	ref := NewStream(k)
	buf := make([]byte, 200)
	if _, err := ref.Read(buf); err != nil {
		t.Fatal(err)
	}
	want := ref.Uint64()

	s := NewStream(k)
	if err := s.Seek(200); err != nil {
		t.Fatal(err)
	}
	if s.Offset() != 200 {
		t.Fatalf("Offset() = %d, want 200", s.Offset())
	}
	if got := s.Uint64(); got != want {
		t.Fatalf("after Seek(200): got %#x, want %#x", got, want)
	}
	if err := s.Seek(MaxStreamOffset); err == nil {
		t.Fatal("Seek at the capacity limit must fail, not wrap")
	}
	if err := s.Seek(MaxStreamOffset - 1); err != nil {
		t.Fatalf("Seek just below the limit must succeed: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Distribution sanity
// ---------------------------------------------------------------------------

// TestUint64nBoundsAndUniformity checks the Lemire rejection bound.
//
// A wrong bound is the worst class of bug available here: it would be perfectly
// deterministic AND biased, so every reproducibility test in this file would
// stay green while the fault schedules PRO-THESIS generates quietly stopped
// exploring part of their space. The seed is fixed, so this never flakes.
func TestUint64nBoundsAndUniformity(t *testing.T) {
	for _, n := range []uint64{1, 2, 3, 7, 1000, 1 << 32, ^uint64(0)} {
		st := NewStream(MustDeriveKey(11, StreamFaultSchedule))
		for i := 0; i < 2000; i++ {
			if v := st.Uint64n(n); v >= n {
				t.Fatalf("Uint64n(%d) returned %d, out of range", n, v)
			}
		}
	}

	const (
		buckets = 7
		draws   = 70_000
	)
	counts := make([]int, buckets)
	st := NewStream(MustDeriveKey(0x5eed, StreamDriverWorkload))
	for i := 0; i < draws; i++ {
		counts[st.Uint64n(buckets)]++
	}
	exp := float64(draws) / buckets
	var chi float64
	for _, c := range counts {
		d := float64(c) - exp
		chi += d * d / exp
	}
	// df = 6. The 0.9999 critical value is about 27.9; 45 is a deliberately
	// loose ceiling that a real bias of a few percent would still blow past.
	if chi > 45 {
		t.Fatalf("chi-square %.2f over %d buckets, counts %v: distribution is skewed", chi, buckets, counts)
	}

	// Monobit on the raw keystream.
	ones := 0
	raw := make([]byte, 1<<16)
	if _, err := NewStream(MustDeriveKey(0x5eed, StreamClientSchedule)).Read(raw); err != nil {
		t.Fatal(err)
	}
	for _, b := range raw {
		for i := 0; i < 8; i++ {
			ones += int(b>>i) & 1
		}
	}
	total := len(raw) * 8
	dev := math.Abs(float64(ones)/float64(total) - 0.5)
	if dev > 0.01 {
		t.Fatalf("monobit deviation %.4f over %d bits", dev, total)
	}
}

// TestShuffleIsAPermutation checks that Shuffle neither drops nor duplicates.
func TestShuffleIsAPermutation(t *testing.T) {
	st := NewStream(MustDeriveKey(3, StreamFaultSchedule))
	for n := 0; n < 40; n++ {
		v := make([]int, n)
		for i := range v {
			v[i] = i
		}
		st.Shuffle(n, func(i, j int) { v[i], v[j] = v[j], v[i] })
		seen := make([]bool, n)
		for _, x := range v {
			if x < 0 || x >= n || seen[x] {
				t.Fatalf("n=%d: Shuffle produced %v, which is not a permutation", n, v)
			}
			seen[x] = true
		}
	}
}

// TestWeightedPPMRespectsWeights checks that a zero weight is never selected
// and that the observed frequencies track the weights.
func TestWeightedPPMRespectsWeights(t *testing.T) {
	w := []uint32{700_000, 0, 300_000}
	counts := make([]int, len(w))
	st := NewStream(MustDeriveKey(17, StreamClientSchedule))
	const n = 50_000
	for i := 0; i < n; i++ {
		counts[st.WeightedPPM(w)]++
	}
	if counts[1] != 0 {
		t.Fatalf("a zero-weight bucket was selected %d times", counts[1])
	}
	got := float64(counts[0]) / n
	if got < 0.69 || got > 0.71 {
		t.Fatalf("bucket 0 selected %.4f of the time, want ~0.70 (counts %v)", got, counts)
	}
}

// ---------------------------------------------------------------------------
// Registry
// ---------------------------------------------------------------------------

func TestStreamsGetIsMemoized(t *testing.T) {
	s := NewStreams(1)
	a := s.Get(StreamDriverWorkload)
	first := a.Uint64()
	b := s.Get(StreamDriverWorkload)
	if b.Uint64() == first {
		t.Fatal("Get returned a fresh stream instead of the memoized one")
	}
	if a != b {
		t.Fatal("Get returned two different stream objects for one domain")
	}
	audit := s.Audit()
	if len(audit) != 1 || audit[0].Domain != StreamDriverWorkload || audit[0].Draws != 2 {
		t.Fatalf("audit = %+v, want one entry for %s with 2 draws", audit, StreamDriverWorkload)
	}
}

// TestUnregisteredDomainPanics: a typo'd domain string would otherwise produce
// a perfectly deterministic and completely unreproducible stream.
func TestUnregisteredDomainPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("Get on an unregistered domain must panic")
		}
	}()
	NewStreams(1).Get("driver.worklaod")
}

func TestRegisteredStreamsAreSortedAndValid(t *testing.T) {
	for i, d := range RegisteredStreams {
		if !validDomain(d) {
			t.Errorf("domain %q does not match the registered-domain grammar", d)
		}
		if i > 0 && RegisteredStreams[i-1] >= d {
			t.Errorf("RegisteredStreams is not sorted at index %d (%q, %q)", i, RegisteredStreams[i-1], d)
		}
		if !isRegistered(d) {
			t.Errorf("isRegistered(%q) is false", d)
		}
	}
	if isRegistered("nope") {
		t.Error("isRegistered accepted an unregistered domain")
	}
}
