package recorder

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math/bits"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"
)

// Deterministic seed streams.
//
// The requirement this file exists to satisfy (PHASE0_BUILD_BRIEF §3, D-013):
//
//	Adding a NEW named stream must not change any value produced by an EXISTING
//	stream for the same root seed.
//
// Sequential child-seeding (child_i = f(parent, i)) violates it, and the
// consequence is not cosmetic: every committed regression world would be
// invalidated the moment a later phase registers another stream. So a stream's
// key is HMAC-SHA256 over its PATH ALONE, keyed by the root seed. Independence
// is then structural (a property of the derivation) rather than a discipline
// someone has to remember.
//
// Nothing here uses math/rand. Neither math/rand nor math/rand/v2 guarantees a
// stable VALUE sequence across Go versions (Shuffle's implementation has already
// changed once), and every archived world depends on that sequence. The bit
// source is ChaCha20 (RFC 8439) and every extraction primitive is written out
// below, so the whole construction is reproducible from published standards in
// any language.

// ---------------------------------------------------------------------------
// Keys and paths
// ---------------------------------------------------------------------------

// Seed is a root seed: 64 bits.
type Seed uint64

// StreamKey is a 32-byte ChaCha20 key: one node of the seed tree.
type StreamKey [32]byte

// String renders the key as 64 lowercase hex digits.
func (k StreamKey) String() string {
	const hexdigits = "0123456789abcdef"
	b := make([]byte, 0, 64)
	for _, c := range k {
		b = append(b, hexdigits[c>>4], hexdigits[c&0xf])
	}
	return string(b)
}

const (
	// streamDomainTag domain-separates this construction from anything else
	// that might ever be HMAC'd with the same root seed.
	streamDomainTag = "prothesis/v1\x00"

	// streamPathSep separates path segments.
	//
	// It is NUL because NUL is the one byte the segment grammar forbids
	// (checkPathSegment). That is what makes the path-to-bytes mapping
	// injective: without a forbidden separator, Derive("a", "b/c") and
	// Derive("a/b", "c") would hash the same message and two distinct streams
	// would silently share a keystream.
	streamPathSep = byte(0x00)

	// maxPathSegment bounds a single segment.
	maxPathSegment = 128
)

// ErrEmptyPath is returned when a derivation path has no segments.
var ErrEmptyPath = errors.New("recorder: stream path needs at least one segment")

func checkPathSegment(seg string) error {
	if seg == "" {
		return errors.New("recorder: empty stream path segment")
	}
	if len(seg) > maxPathSegment {
		return fmt.Errorf("recorder: stream path segment %q is %d bytes, limit %d", seg, len(seg), maxPathSegment)
	}
	if strings.IndexByte(seg, streamPathSep) >= 0 {
		return fmt.Errorf("recorder: stream path segment %q contains the path separator (NUL)", seg)
	}
	if !utf8.ValidString(seg) {
		return fmt.Errorf("recorder: stream path segment %q is not valid UTF-8", seg)
	}
	return nil
}

func seedKeyBytes(s Seed) []byte {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(s))
	return b[:]
}

// RootKey is the seed tree's root: HMAC-SHA256(be64(seed), "prothesis/v1\x00root").
//
// It is not itself a stream key for any named domain; it exists so that a run
// root seed has a canonical 32-byte expansion.
func RootKey(s Seed) StreamKey {
	m := hmac.New(sha256.New, seedKeyBytes(s))
	m.Write([]byte(streamDomainTag + "root"))
	var out StreamKey
	copy(out[:], m.Sum(nil))
	return out
}

// DeriveKey returns the key for the stream at path, under root seed s:
//
//	HMAC-SHA256(be64(s), "prothesis/v1\x00stream" || 0x00 || seg0 || 0x00 || seg1 ...)
//
// The key is a function of (s, path) and of nothing else. In particular it does
// not depend on which other streams exist, on the order in which streams were
// created, or on how many values anyone drew from any of them.
func DeriveKey(s Seed, path ...string) (StreamKey, error) {
	if len(path) == 0 {
		return StreamKey{}, ErrEmptyPath
	}
	m := hmac.New(sha256.New, seedKeyBytes(s))
	m.Write([]byte(streamDomainTag + "stream"))
	for _, seg := range path {
		if err := checkPathSegment(seg); err != nil {
			return StreamKey{}, err
		}
		m.Write([]byte{streamPathSep})
		m.Write([]byte(seg))
	}
	var out StreamKey
	copy(out[:], m.Sum(nil))
	return out, nil
}

// MustDeriveKey is DeriveKey for paths fixed in source. A malformed compile-time
// constant path is a programming error, not a runtime condition.
func MustDeriveKey(s Seed, path ...string) StreamKey {
	k, err := DeriveKey(s, path...)
	if err != nil {
		panic(err)
	}
	return k
}

// WorldSeed derives the seed of world number ordinal within a run rooted at run.
//
// A `.thesis` file must be self-contained (invariant I2), so this value is
// written into the world and a replay needs nothing else. It is path-derived
// like everything else, so registering new streams later cannot perturb it.
func WorldSeed(run Seed, ordinal uint64) Seed {
	k := MustDeriveKey(run, "world.seed", fmt.Sprintf("%d", ordinal))
	return Seed(binary.BigEndian.Uint64(k[:8]))
}

// ---------------------------------------------------------------------------
// ChaCha20 (RFC 8439)
// ---------------------------------------------------------------------------

// chachaConstants is the ASCII string "expand 32-byte k" as four little-endian
// words (RFC 8439 §2.3).
var chachaConstants = [4]uint32{0x61707865, 0x3320646e, 0x79622d32, 0x6b206574}

// chachaBlock computes one 64-byte ChaCha20 block: 20 rounds (10 double
// rounds), state serialized little-endian. Verified against the RFC 8439 §2.3.2
// and §2.4.2 test vectors in prng_test.go.
func chachaBlock(key *[32]byte, nonce *[12]byte, counter uint32, out *[64]byte) {
	var s [16]uint32
	s[0], s[1], s[2], s[3] = chachaConstants[0], chachaConstants[1], chachaConstants[2], chachaConstants[3]
	for i := 0; i < 8; i++ {
		s[4+i] = binary.LittleEndian.Uint32(key[i*4 : i*4+4])
	}
	s[12] = counter
	for i := 0; i < 3; i++ {
		s[13+i] = binary.LittleEndian.Uint32(nonce[i*4 : i*4+4])
	}

	x := s
	for i := 0; i < 10; i++ {
		// column rounds
		qr(&x[0], &x[4], &x[8], &x[12])
		qr(&x[1], &x[5], &x[9], &x[13])
		qr(&x[2], &x[6], &x[10], &x[14])
		qr(&x[3], &x[7], &x[11], &x[15])
		// diagonal rounds
		qr(&x[0], &x[5], &x[10], &x[15])
		qr(&x[1], &x[6], &x[11], &x[12])
		qr(&x[2], &x[7], &x[8], &x[13])
		qr(&x[3], &x[4], &x[9], &x[14])
	}
	for i := 0; i < 16; i++ {
		binary.LittleEndian.PutUint32(out[i*4:i*4+4], x[i]+s[i])
	}
}

func qr(a, b, c, d *uint32) {
	*a += *b
	*d ^= *a
	*d = bits.RotateLeft32(*d, 16)
	*c += *d
	*b ^= *c
	*b = bits.RotateLeft32(*b, 12)
	*a += *b
	*d ^= *a
	*d = bits.RotateLeft32(*d, 8)
	*c += *d
	*b ^= *c
	*b = bits.RotateLeft32(*b, 7)
}

// ---------------------------------------------------------------------------
// Stream
// ---------------------------------------------------------------------------

// MaxStreamOffset is the addressable keystream length of one (key, nonce) pair:
// 2^32 blocks of 64 bytes.
const MaxStreamOffset = uint64(1) << 38

// Nonce layout. A stream's 12-byte ChaCha nonce is:
//
//	byte  0    : 0x00 for the sequential stream, 0x01 for an indexed sub-stream
//	bytes 1..3 : zero
//	bytes 4..11: the sub-stream index, BIG-ENDIAN uint64 (zero when sequential)
//
// The discriminator byte is what stops Index(0) from colliding with sequential
// consumption of the parent. The byte order is stated explicitly because the
// reason for hand-rolling ChaCha20 at all is cross-language reproduction, and an
// unspecified nonce encoding would defeat it.
const (
	nonceKindSequential = byte(0x00)
	nonceKindIndexed    = byte(0x01)
)

func makeNonce(kind byte, index uint64) [12]byte {
	var n [12]byte
	n[0] = kind
	binary.BigEndian.PutUint64(n[4:12], index)
	return n
}

// Stream is a seekable deterministic bit source.
//
// ChaCha20 is used because it is frozen forever by RFC 8439 (so it is bit-exact
// in any language), and because it is COUNTER-ADDRESSABLE: block N is computable
// without generating blocks 0..N-1, which is what makes Seek and Index O(1).
//
// A Stream is not safe for concurrent use.
type Stream struct {
	key   StreamKey
	nonce [12]byte
	block uint32
	buf   [64]byte
	off   int
	valid bool

	draws uint64
}

// NewStream returns the sequential stream for key k.
func NewStream(k StreamKey) *Stream {
	return &Stream{key: k, nonce: makeNonce(nonceKindSequential, 0)}
}

// Index returns an independent sub-stream of s addressed by i.
//
// This is the primitive that makes shrinking converge. A delta-debugging pass
// that removes operation #7 from a 20,000-operation workload must not disturb
// the randomness of operations #8..#20000, or it would be minimizing against a
// moving target. Anything a shrinker may delete therefore draws from
// Index(ordinal), never sequentially.
//
// The sub-stream shares s's key; deriving it costs no hashing.
func (s *Stream) Index(i uint64) *Stream {
	return &Stream{key: s.key, nonce: makeNonce(nonceKindIndexed, i)}
}

// Seek positions the stream at byte offset n of its keystream.
func (s *Stream) Seek(n uint64) error {
	if n >= MaxStreamOffset {
		return fmt.Errorf("recorder: Seek(%d) exceeds the addressable keystream length %d "+
			"(2^32 blocks x 64 bytes)", n, MaxStreamOffset)
	}
	s.block = uint32(n / 64)
	s.off = int(n % 64)
	s.valid = false
	return nil
}

// Offset returns the current byte offset into the keystream.
func (s *Stream) Offset() uint64 { return uint64(s.block)*64 + uint64(s.off) }

// Draws returns the number of primitive draws taken from this stream. It feeds
// the per-run stream audit.
func (s *Stream) Draws() uint64 { return s.draws }

func (s *Stream) fill() {
	chachaBlock((*[32]byte)(&s.key), &s.nonce, s.block, &s.buf)
	s.valid = true
}

// Read fills p with keystream bytes. It never returns an error and always fills
// p completely, except that it panics rather than silently wrapping if the
// stream would run past MaxStreamOffset.
func (s *Stream) Read(p []byte) (int, error) {
	n := 0
	for n < len(p) {
		if s.off == 64 {
			if s.block == ^uint32(0) {
				panic("recorder: stream keystream exhausted (2^38 bytes)")
			}
			s.block++
			s.off = 0
			s.valid = false
		}
		if !s.valid {
			s.fill()
		}
		c := copy(p[n:], s.buf[s.off:])
		s.off += c
		n += c
	}
	return n, nil
}

// Uint64 returns the next 8 keystream bytes interpreted LITTLE-ENDIAN.
//
// Little-endian because the ChaCha state serialization is little-endian; stated
// explicitly so a Python or Rust reimplementation matches byte for byte.
func (s *Stream) Uint64() uint64 {
	var b [8]byte
	//nolint:errcheck // Read never fails.
	_, _ = s.Read(b[:])
	s.draws++
	return binary.LittleEndian.Uint64(b[:])
}

// Uint64n returns a uniform value in [0, n) using Lemire's multiply-shift with
// full rejection: no modulo bias.
//
// Written out here rather than delegated to math/rand, whose helpers carry no
// cross-version stability guarantee. bits.Mul64 is a stdlib widening multiply
// whose semantics are fixed by the language.
func (s *Stream) Uint64n(n uint64) uint64 {
	if n == 0 {
		panic("recorder: Uint64n(0)")
	}
	x := s.Uint64()
	hi, lo := bits.Mul64(x, n)
	if lo < n {
		t := (-n) % n // == 2^64 mod n
		for lo < t {
			x = s.Uint64()
			hi, lo = bits.Mul64(x, n)
		}
	}
	return hi
}

// Float64 returns a uniform value in [0,1) with exactly 53 bits of entropy.
//
// Both steps are EXACT in IEEE-754 binary64 on every conforming platform: the
// integer is at most 2^53-1 (exactly representable) and the multiplier is a
// power of two. No libm call is involved, so the result is bit-identical across
// Go versions, architectures and operating systems.
func (s *Stream) Float64() float64 {
	return float64(s.Uint64()>>11) * (1.0 / (1 << 53))
}

// Shuffle permutes n elements using DESCENDING Fisher-Yates, calling swap.
//
// The direction is part of the frozen contract: ascending and descending
// Fisher-Yates consume the same number of values and produce different
// permutations.
func (s *Stream) Shuffle(n int, swap func(i, j int)) {
	if n < 0 {
		panic("recorder: Shuffle with negative n")
	}
	for i := n - 1; i > 0; i-- {
		j := int(s.Uint64n(uint64(i) + 1))
		swap(i, j)
	}
}

// WeightedPPM selects an index given integer weights (conventionally parts per
// million). Integer arithmetic only.
//
// This closes a real determinism trap. A config mix like
// {read: 0.4, write: 0.4, txn: 0.2} arrives as a Go map, whose iteration order
// Go randomizes, and float addition is not associative, so a cumulative sum
// would select different buckets at boundaries from run to run. Weights reach
// this function already canonicalized to sorted-key order and to integers.
func (s *Stream) WeightedPPM(w []uint32) int {
	if len(w) == 0 {
		panic("recorder: WeightedPPM with no weights")
	}
	var total uint64
	for _, x := range w {
		total += uint64(x)
	}
	if total == 0 {
		panic("recorder: WeightedPPM with zero total weight")
	}
	r := s.Uint64n(total)
	var acc uint64
	for i, x := range w {
		acc += uint64(x)
		if r < acc {
			return i
		}
	}
	return len(w) - 1 // unreachable: r < total == acc
}

// ---------------------------------------------------------------------------
// Domain registry
// ---------------------------------------------------------------------------

// Registered stream domains.
//
// These strings are part of the reproducibility contract, not implementation
// detail: renaming one silently changes every world's behaviour while leaving
// every world identity unchanged. They are frozen here in Phase 0 so that later
// phases add streams rather than rename them.
const (
	StreamFaultSchedule   = "fault.schedule"
	StreamDriverWorkload  = "driver.workload"
	StreamClientSchedule  = "client.schedule"
	StreamInjectDelay     = "inject.delay"
	StreamTopologyVariant = "topology.variant"
)

// RegisteredStreams is the sorted set of domains a Streams registry will serve.
//
// Later phases append; they never rename or remove. Because a stream's key is a
// function of its path alone, appending here cannot change a single value drawn
// from any domain already in the list, which is the property that keeps
// committed regression worlds valid across phases.
var RegisteredStreams = []string{
	StreamClientSchedule,
	StreamDriverWorkload,
	StreamFaultSchedule,
	StreamInjectDelay,
	StreamTopologyVariant,
}

// domainGrammar: registered domains are lowercase dotted identifiers. Enforced
// so a domain can never contain the path separator and so the table stays
// greppable.
func validDomain(d string) bool {
	if d == "" || len(d) > 64 || d[0] < 'a' || d[0] > 'z' {
		return false
	}
	for i := 1; i < len(d); i++ {
		c := d[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '_', c == '.':
		default:
			return false
		}
	}
	return d[len(d)-1] != '.'
}

func init() {
	seen := map[string]bool{}
	for i, d := range RegisteredStreams {
		if !validDomain(d) {
			panic("recorder: invalid registered stream domain " + d)
		}
		if seen[d] {
			panic("recorder: duplicate registered stream domain " + d)
		}
		seen[d] = true
		if i > 0 && RegisteredStreams[i-1] >= d {
			panic("recorder: RegisteredStreams must be sorted and unique")
		}
	}
}

// StreamAudit is the per-domain draw count written to the run's stream audit.
type StreamAudit struct {
	Domain string `json:"domain"`
	Draws  uint64 `json:"draws"`
}

// Streams is the per-WORLD stream registry: per world, not per run, because a
// `.thesis` file must be self-contained and a world's randomness must never
// depend on its ordinal within some run.
type Streams struct {
	seed Seed
	mu   sync.Mutex
	used map[string]*Stream
}

// NewStreams builds the registry for one world from that world's own seed.
func NewStreams(worldSeed Seed) *Streams {
	return &Streams{seed: worldSeed, used: map[string]*Stream{}}
}

// Seed returns the world seed this registry was built from.
func (s *Streams) Seed() Seed { return s.seed }

// Get returns the memoized stream for a registered domain.
//
// It panics on an unregistered domain. A typo'd domain string would otherwise
// silently produce a fresh, perfectly deterministic, and completely
// unreproducible stream that no other build of the tool would ever recreate.
func (s *Streams) Get(domain string) *Stream {
	if !isRegistered(domain) {
		panic(fmt.Sprintf("recorder: unregistered stream domain %q (registered: %v)", domain, RegisteredStreams))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if st, ok := s.used[domain]; ok {
		return st
	}
	st := NewStream(MustDeriveKey(s.seed, domain))
	s.used[domain] = st
	return st
}

func isRegistered(d string) bool {
	i := sort.SearchStrings(RegisteredStreams, d)
	return i < len(RegisteredStreams) && RegisteredStreams[i] == d
}

// Audit reports draw counts per domain, sorted by domain, for every stream that
// was actually used.
func (s *Streams) Audit() []StreamAudit {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]StreamAudit, 0, len(s.used))
	for d, st := range s.used {
		out = append(out, StreamAudit{Domain: d, Draws: st.Draws()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Domain < out[j].Domain })
	return out
}
