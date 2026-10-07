import { Data, Result } from "effect"

// splitmix64 (Steele, Lea, Flood 2014). Chosen over mulberry32 because its 64-bit state lets a
// u64 seed from the CLI be used without truncation. State is threaded explicitly: every call
// returns the value and the next generator, so no hidden mutable state can leak between worlds.

export type Prng = { readonly state: bigint }

export class PrngBoundError extends Data.TaggedError("PrngBoundError")<{ readonly bound: number }> {}

const GOLDEN_GAMMA = 0x9e3779b97f4a7c15n
const TWO_POW_64 = 1n << 64n

export const seedPrng = (seed: bigint): Prng => ({ state: BigInt.asUintN(64, seed) })

export const nextU64 = (prng: Prng): readonly [bigint, Prng] => {
  const state = BigInt.asUintN(64, prng.state + GOLDEN_GAMMA)
  const mixed1 = BigInt.asUintN(64, (state ^ (state >> 30n)) * 0xbf58476d1ce4e5b9n)
  const mixed2 = BigInt.asUintN(64, (mixed1 ^ (mixed1 >> 27n)) * 0x94d049bb133111ebn)
  return [mixed2 ^ (mixed2 >> 31n), { state }]
}

// Uniform integer in [0, bound). Rejection sampling removes modulo bias: draws at or above the
// largest multiple of bound below 2^64 are discarded. Expected draws per call are below 2.
export const nextBelow = (
  prng: Prng,
  bound: number,
): Result.Result<readonly [number, Prng], PrngBoundError> =>
  Number.isSafeInteger(bound) && bound > 0
    ? Result.succeed(drawBelow(prng, BigInt(bound)))
    : Result.fail(new PrngBoundError({ bound }))

const drawBelow = (prng: Prng, bound: bigint): readonly [number, Prng] => {
  const [value, next] = nextU64(prng)
  return value < TWO_POW_64 - (TWO_POW_64 % bound) ? [Number(value % bound), next] : drawBelow(next, bound)
}
