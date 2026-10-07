import { expect } from "bun:test"
import { Arbitrary, Effect } from "effect"

// Runs a property with a fixed seed so a failure reproduces, and refuses a pass that ran no cases:
// a property that generates nothing is the vacuous PASS this project exists to prevent.
export const assertProperty = async <A>(
  name: string,
  arbitrary: Arbitrary.Arbitrary<A>,
  property: (value: A) => boolean,
  runs = 300,
): Promise<void> => {
  const result = await Effect.runPromise(Arbitrary.checkEffect(arbitrary, property, { runs, seed: 20261006 }))
  expect({ name, failure: Arbitrary.formatCheckFailure(result) }).toEqual({ name, failure: undefined })
  expect(result._tag).toBe("Passed")
  expect(result._tag === "Passed" ? result.runs : 0).toBeGreaterThanOrEqual(runs)
}

export const sampleOf = <A>(arbitrary: Arbitrary.Arbitrary<A>, count: number): Promise<ReadonlyArray<A>> =>
  Effect.runPromise(Arbitrary.sampleEffect(arbitrary, { count, seed: 20261006 }))
