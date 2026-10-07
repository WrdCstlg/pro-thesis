import { describe, expect, test } from "bun:test"
import { Result } from "effect"
import { nextBelow, nextU64, seedPrng, type Prng } from "../src/prng"

const drawU64 = (prng: Prng, count: number): ReadonlyArray<bigint> =>
  Array.from({ length: count }).reduce<{ readonly out: ReadonlyArray<bigint>; readonly prng: Prng }>(
    (acc) => {
      const [value, next] = nextU64(acc.prng)
      return { out: [...acc.out, value], prng: next }
    },
    { out: [], prng },
  ).out

const drawBelowMany = (prng: Prng, bound: number, count: number): ReadonlyArray<number> =>
  Array.from({ length: count }).reduce<{ readonly out: ReadonlyArray<number>; readonly prng: Prng }>(
    (acc) =>
      Result.match(nextBelow(acc.prng, bound), {
        onSuccess: ([value, next]) => ({ out: [...acc.out, value], prng: next }),
        onFailure: () => acc,
      }),
    { out: [], prng },
  ).out

describe("prng", () => {
  // Published splitmix64 reference outputs for seed 0. These come from the algorithm's
  // reference implementation, not from this module, so they catch a wrong constant or shift.
  test("matches the splitmix64 reference sequence for seed 0", () => {
    expect(drawU64(seedPrng(0n), 3)).toEqual([0xe220a8397b1dcdafn, 0x6e789e6aa1b965f4n, 0x06c45d188009454fn])
  })

  test("the same seed yields the same sequence", () => {
    expect(drawU64(seedPrng(42n), 1000)).toEqual(drawU64(seedPrng(42n), 1000))
  })

  test("adjacent seeds yield different sequences", () => {
    const a = drawU64(seedPrng(42n), 64)
    const b = drawU64(seedPrng(43n), 64)
    expect(a.filter((value, index) => value === b[index]).length).toBe(0)
  })

  test("seeds are reduced modulo 2^64, so a negative seed is a valid, distinct seed", () => {
    expect(seedPrng(-1n).state).toBe(0xffffffffffffffffn)
    expect(drawU64(seedPrng(-1n), 4)).not.toEqual(drawU64(seedPrng(0n), 4))
  })

  test("nextBelow stays in range and reaches every value of a small bound", () => {
    const values = drawBelowMany(seedPrng(7n), 6, 6000)
    expect(values.length).toBe(6000)
    expect(values.every((value) => Number.isInteger(value) && value >= 0 && value < 6)).toBe(true)
    expect(new Set(values).size).toBe(6)
  })

  test("nextBelow with bound 1 always yields 0", () => {
    expect(new Set(drawBelowMany(seedPrng(9n), 1, 100))).toEqual(new Set([0]))
  })

  test("nextBelow refuses a bound that is zero, negative, fractional or unsafe", () => {
    const refused = [0, -3, 2.5, Number.MAX_SAFE_INTEGER + 2, Number.NaN].map((bound) =>
      Result.isFailure(nextBelow(seedPrng(1n), bound)),
    )
    expect(refused).toEqual([true, true, true, true, true])
  })

  test("the generator is pure: drawing from a state does not change that state", () => {
    const start = seedPrng(5n)
    const first = nextU64(start)
    const again = nextU64(start)
    expect(again[0]).toBe(first[0])
    expect(start.state).toBe(5n)
  })
})
