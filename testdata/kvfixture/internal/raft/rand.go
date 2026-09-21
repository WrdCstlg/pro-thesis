package raft

// Deterministic value extraction, written in-repo.
//
// math/rand and math/rand/v2 do not guarantee a stable value sequence across Go
// versions, and the fixture's election-timeout randomisation is part of what a
// PRO-THESIS world claims to reproduce. A three-line splitmix64 that will
// produce the same bytes in ten years is worth more here than any library.

const (
	smGamma = 0x9E3779B97F4A7C15
	smMixA  = 0xBF58476D1CE4E5B9
	smMixB  = 0x94D049BB133111EB
)

// Splitmix64 is the one-shot finaliser. It is a bijection on uint64, so
// distinct inputs always produce distinct outputs.
func Splitmix64(x uint64) uint64 {
	z := x + smGamma
	z = (z ^ (z >> 30)) * smMixA
	z = (z ^ (z >> 27)) * smMixB
	return z ^ (z >> 31)
}

// FNV64a is the 64-bit FNV-1a hash of s.
func FNV64a(s string) uint64 {
	const (
		offset = 1469598103934665603
		prime  = 1099511628211
	)
	h := uint64(offset)
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= prime
	}
	return h
}

// DeriveNodeSeed mixes the world seed with the node identity.
//
// This exists because the obvious compose formulation
//
//	KV_SEED: "${KV_SEED:-1}"  /  "${KV_SEED:-2}"  /  "${KV_SEED:-3}"
//
// collapses to ONE seed for all three nodes the moment anything in the
// environment sets KV_SEED -- which is exactly what a world-varying harness
// does. All three nodes would then draw an identical election timeout, split
// the vote on every round, and the cluster could fail to elect a leader at all.
// Every node therefore receives the SAME KV_SEED and derives its own stream
// here, so correctness never depends on compose interpolation defaults.
func DeriveNodeSeed(worldSeed uint64, nodeID string) uint64 {
	return Splitmix64(worldSeed ^ FNV64a(nodeID))
}

// rng is a splitmix64 sequence.
type rng struct{ state uint64 }

func newRNG(seed uint64) *rng { return &rng{state: seed} }

func (r *rng) next() uint64 {
	r.state += smGamma
	z := r.state
	z = (z ^ (z >> 30)) * smMixA
	z = (z ^ (z >> 27)) * smMixB
	return z ^ (z >> 31)
}

// between returns a value in [lo, hi]. Unbiased enough for timeout jitter; the
// modulo bias at these magnitudes is below one part in 2^50.
func (r *rng) between(lo, hi int64) int64 {
	if hi <= lo {
		return lo
	}
	span := uint64(hi-lo) + 1
	return lo + int64(r.next()%span)
}
