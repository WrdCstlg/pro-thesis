import { describe, expect, test } from "bun:test"
import { Arbitrary, Schema } from "effect"
import { LockStatus, OracleVerdict, WorldId, type ExitCode } from "../src/schema"
import { aggregate, type RunFacts, type WorldGuard, type WorldOutcome } from "../src/verdict"
import { oracleName, opId, worldId } from "./support/ids"
import { assertProperty, sampleOf } from "./support/property"

type Violation = Extract<OracleVerdict, { readonly _tag: "Violation" }>
type Inconclusive = Extract<OracleVerdict, { readonly _tag: "Inconclusive" }>
type Ok = Extract<OracleVerdict, { readonly _tag: "Ok" }>

const verdictArbitrary = Arbitrary.schema(OracleVerdict)
// Built from the union's own members so every draw is usable. A filter over the union made the
// first run report "Property exhausted after 116 run(s) and 3001 discard(s)" instead of running.
const [okMember, violationMember, inconclusiveMember] = OracleVerdict.members
const okArbitrary = Arbitrary.schema(okMember)
const violationArbitrary = Arbitrary.schema(violationMember)
const inconclusiveArbitrary = Arbitrary.schema(inconclusiveMember)

const GUARDS: ReadonlyArray<WorldGuard> = ["fault-missed-load", "provider-not-exercised"]

const aViolation: Violation = { _tag: "Violation", oracle: oracleName("edit-integrity"), witness: [opId(7)], explanation: "marker appears twice" }
const anInconclusive: Inconclusive = { _tag: "Inconclusive", oracle: oracleName("quiescence"), reason: "missing-probe" }
const anOk: Ok = { _tag: "Ok", oracle: oracleName("tool-pairing") }

const world = (index: number, verdicts: ReadonlyArray<OracleVerdict>, extra: Partial<WorldOutcome> = {}): WorldOutcome => ({
  world: worldId(index),
  reachedAssert: true,
  verdicts,
  guards: [],
  ...extra,
})

const passing = (worlds: number): RunFacts => ({
  lock: "ok",
  narrowed: false,
  interrupted: false,
  budgetExhausted: false,
  plannedWorlds: worlds,
  worlds: Array.from({ length: worlds }, (_, index) => world(index + 1, [anOk])),
})

const exitOf = (facts: RunFacts): ExitCode => aggregate(facts).exitCode

describe("aggregate: the exit-code table, one row at a time", () => {
  test("a locked, complete run in which every oracle returned Ok is 0", () => {
    expect(exitOf(passing(3))).toBe(0)
  })

  test("lock drift is 4 even when a violation and an exhausted budget are present, and nothing else is reported", () => {
    const facts: RunFacts = { ...passing(2), lock: "drift", budgetExhausted: true, worlds: [world(1, [aViolation])] }
    expect(aggregate(facts)).toEqual({ exitCode: 4, worlds: [], violations: [], inconclusive: [] })
  })

  test("a violation is 1 and outranks an exhausted budget and inconclusive worlds", () => {
    const facts: RunFacts = { ...passing(3), budgetExhausted: true, worlds: [world(1, [aViolation]), world(2, [anInconclusive])] }
    const outcome = aggregate(facts)
    expect(outcome.exitCode).toBe(1)
    expect(outcome.violations).toEqual([{ world: worldId(1), oracle: aViolation.oracle, witness: aViolation.witness, explanation: aViolation.explanation }])
    expect(outcome.inconclusive.map((entry) => entry.reason)).toContain("missing-probe")
  })

  test("a violation stands in a narrowed run and in an interrupted run (FAIL cannot be hidden)", () => {
    expect(exitOf({ ...passing(3), narrowed: true, worlds: [world(1, [aViolation])] })).toBe(1)
    expect(exitOf({ ...passing(3), interrupted: true, lock: "bypassed", worlds: [world(1, [aViolation])] })).toBe(1)
  })

  test("an exhausted budget with no violation is 3, even when worlds are also inconclusive", () => {
    expect(exitOf({ ...passing(3), budgetExhausted: true, worlds: [world(1, [anOk])] })).toBe(3)
    expect(exitOf({ ...passing(2), budgetExhausted: true, worlds: [world(1, [anInconclusive]), world(2, [anOk])] })).toBe(3)
  })

  test("a single Inconclusive among passing oracles makes the run 2", () => {
    expect(exitOf({ ...passing(2), worlds: [world(1, [anOk, anOk, anInconclusive]), world(2, [anOk])] })).toBe(2)
  })

  test("each inconclusive condition in the table is reported with its reason and gives 2", () => {
    const reasons = (facts: RunFacts) => aggregate(facts).inconclusive.map((entry) => entry.reason)
    expect(reasons({ ...passing(1), worlds: [world(1, [anOk], { reachedAssert: false })] })).toEqual(["world-not-asserted"])
    expect(reasons({ ...passing(1), worlds: [world(1, [])] })).toEqual(["no-oracle-judged"])
    expect(reasons({ ...passing(1), worlds: [world(1, [anOk], { guards: ["fault-missed-load"] })] })).toEqual(["fault-missed-load"])
    expect(reasons({ ...passing(1), worlds: [world(1, [anOk], { guards: ["provider-not-exercised"] })] })).toEqual(["provider-not-exercised"])
    expect(reasons({ ...passing(1), narrowed: true })).toEqual(["narrowed-run"])
    expect(reasons({ ...passing(1), lock: "bypassed" })).toEqual(["narrowed-run"])
    expect(reasons({ ...passing(1), interrupted: true })).toEqual(["interrupted"])
    expect(reasons({ ...passing(2), worlds: [world(1, [anOk])] })).toEqual(["world-not-asserted"])
    const exits = [
      { ...passing(1), worlds: [world(1, [anOk], { reachedAssert: false })] },
      { ...passing(1), worlds: [world(1, [])] },
      { ...passing(1), worlds: [world(1, [anOk], { guards: ["fault-missed-load"] })] },
      { ...passing(1), narrowed: true },
      { ...passing(1), lock: "bypassed" as const },
      { ...passing(1), interrupted: true },
      { ...passing(2), worlds: [world(1, [anOk])] },
    ].map(exitOf)
    expect(exits).toEqual([2, 2, 2, 2, 2, 2, 2])
  })

  test("an Inconclusive verdict names its oracle and world in the report", () => {
    expect(aggregate({ ...passing(1), worlds: [world(1, [anInconclusive])] }).inconclusive).toEqual([
      { reason: "missing-probe", world: worldId(1), oracle: anInconclusive.oracle },
    ])
  })

  test("nothing ran is never 0: no worlds planned, no worlds run, or a plan that is not a positive integer", () => {
    expect(exitOf({ ...passing(1), plannedWorlds: 0, worlds: [] })).toBe(2)
    expect(exitOf({ ...passing(1), plannedWorlds: 1, worlds: [] })).toBe(2)
    expect(exitOf({ ...passing(1), plannedWorlds: 0 })).toBe(2)
    expect(exitOf({ ...passing(1), plannedWorlds: -1 })).toBe(2)
    expect(exitOf({ ...passing(1), plannedWorlds: Number.NaN })).toBe(2)
    expect(exitOf({ ...passing(1), plannedWorlds: 1.5 })).toBe(2)
  })
})

// Independent restatement of the contract: PASS needs every condition below at once, and the other
// codes follow the precedence order. It is written from the exit-code table, not from aggregate.
const contractPasses = (facts: RunFacts): boolean =>
  facts.lock === "ok" &&
  !facts.narrowed &&
  !facts.interrupted &&
  !facts.budgetExhausted &&
  facts.plannedWorlds >= 1 &&
  facts.worlds.length >= facts.plannedWorlds &&
  facts.worlds.every(
    (outcome) =>
      outcome.reachedAssert && outcome.guards.length === 0 && outcome.verdicts.length >= 1 && outcome.verdicts.every((verdict) => verdict._tag === "Ok"),
  )

const hasViolation = (facts: RunFacts): boolean => facts.worlds.some((outcome) => outcome.verdicts.some((verdict) => verdict._tag === "Violation"))

const contractExit = (facts: RunFacts): ExitCode =>
  facts.lock === "drift" ? 4 : hasViolation(facts) ? 1 : facts.budgetExhausted ? 3 : contractPasses(facts) ? 0 : 2

const passingArbitrary: Arbitrary.Arbitrary<RunFacts> = Arbitrary.map(
  Arbitrary.array(Arbitrary.array(okArbitrary, { minLength: 1, maxLength: 3 }), { minLength: 1, maxLength: 5 }),
  (verdictLists): RunFacts => ({
    lock: "ok",
    narrowed: false,
    interrupted: false,
    budgetExhausted: false,
    plannedWorlds: verdictLists.length,
    worlds: verdictLists.map((verdicts, index) => world(index + 1, verdicts)),
  }),
)

const perturbations: ReadonlyArray<(facts: RunFacts) => RunFacts> = [
  (facts) => ({ ...facts, lock: "bypassed" }),
  (facts) => ({ ...facts, lock: "drift" }),
  (facts) => ({ ...facts, narrowed: true }),
  (facts) => ({ ...facts, interrupted: true }),
  (facts) => ({ ...facts, budgetExhausted: true }),
  (facts) => ({ ...facts, plannedWorlds: facts.plannedWorlds + 1 }),
  (facts) => ({ ...facts, worlds: facts.worlds.map((outcome) => ({ ...outcome, reachedAssert: false })) }),
  (facts) => ({ ...facts, worlds: facts.worlds.map((outcome, index) => (index === 0 ? { ...outcome, verdicts: [] } : outcome)) }),
  (facts) => ({ ...facts, worlds: facts.worlds.map((outcome, index) => (index === 0 ? { ...outcome, verdicts: [...outcome.verdicts, anInconclusive] } : outcome)) }),
  (facts) => ({ ...facts, worlds: facts.worlds.map((outcome, index) => (index === 0 ? { ...outcome, verdicts: [...outcome.verdicts, aViolation] } : outcome)) }),
  (facts) => ({ ...facts, worlds: facts.worlds.map((outcome, index) => (index === 0 ? { ...outcome, guards: GUARDS } : outcome)) }),
  (facts) => ({ ...facts, worlds: facts.worlds.slice(1) }),
  (facts) => facts,
]

const indexArbitrary = Arbitrary.schema(Schema.Literals([0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12]))

// Complete passing runs, each with up to two perturbations, so the generator reaches every exit code
// and the properties below are not vacuous (a test below proves that).
const factsArbitrary: Arbitrary.Arbitrary<RunFacts> = Arbitrary.flatMap(passingArbitrary, (base) =>
  Arbitrary.map(Arbitrary.array(indexArbitrary, { maxLength: 2 }), (indexes) =>
    indexes.reduce((facts, index) => (perturbations[index] ?? ((same: RunFacts) => same))(facts), base),
  ),
)

const randomFactsArbitrary: Arbitrary.Arbitrary<RunFacts> = Arbitrary.all({
  lock: Arbitrary.schema(LockStatus),
  narrowed: Arbitrary.schema(Schema.Boolean),
  interrupted: Arbitrary.schema(Schema.Boolean),
  budgetExhausted: Arbitrary.schema(Schema.Boolean),
  plannedWorlds: Arbitrary.schema(Schema.Literals([1, 2, 3, 4])),
  worlds: Arbitrary.array(
    Arbitrary.all({
      world: Arbitrary.schema(WorldId),
      reachedAssert: Arbitrary.schema(Schema.Boolean),
      verdicts: Arbitrary.array(verdictArbitrary, { maxLength: 3 }),
      guards: Arbitrary.array(Arbitrary.schema(Schema.Literals(GUARDS)), { maxLength: 1 }),
    }),
    { maxLength: 4 },
  ),
})

const withoutViolations = (facts: RunFacts): RunFacts => ({
  ...facts,
  worlds: facts.worlds.map((outcome) => ({ ...outcome, verdicts: outcome.verdicts.filter((verdict) => verdict._tag !== "Violation") })),
})

const insertVerdict = (facts: RunFacts, verdict: OracleVerdict, choice: number): RunFacts =>
  facts.worlds.length === 0
    ? { ...facts, worlds: [world(1, [verdict])] }
    : { ...facts, worlds: facts.worlds.map((outcome, index) => (index === choice % facts.worlds.length ? { ...outcome, verdicts: [...outcome.verdicts, verdict] } : outcome)) }

describe("aggregate: properties", () => {
  test("the generators are not vacuous: between them they reach exit codes 0, 1, 2, 3 and 4", async () => {
    const mixed = await sampleOf(factsArbitrary, 600)
    const random = await sampleOf(randomFactsArbitrary, 600)
    expect([...new Set([...mixed, ...random].map(exitOf))].toSorted()).toEqual([0, 1, 2, 3, 4])
    expect(mixed.filter((facts) => exitOf(facts) === 0).length).toBeGreaterThan(20)
    expect(mixed.filter((facts) => exitOf(facts) === 2).length).toBeGreaterThan(20)
  })

  test("the exit code equals the code the contract assigns, for passing runs with perturbations and for random runs", async () => {
    await assertProperty("contract, perturbed", factsArbitrary, (facts) => exitOf(facts) === contractExit(facts))
    await assertProperty("contract, random", randomFactsArbitrary, (facts) => exitOf(facts) === contractExit(facts))
  })

  test("adding a Violation always gives 1, unless the lock drifted, which gives 4", async () => {
    await assertProperty(
      "violation",
      Arbitrary.all([factsArbitrary, violationArbitrary, Arbitrary.schema(Schema.Literals([0, 1, 2, 3, 4]))]),
      ([facts, violation, choice]) => exitOf(insertVerdict(facts, violation, choice)) === (facts.lock === "drift" ? 4 : 1),
    )
  })

  test("adding an Inconclusive to a run with no violations never gives 0", async () => {
    await assertProperty(
      "inconclusive",
      Arbitrary.all([factsArbitrary, inconclusiveArbitrary, Arbitrary.schema(Schema.Literals([0, 1, 2, 3, 4]))]),
      ([facts, inconclusive, choice]) => {
        const exit = exitOf(insertVerdict(withoutViolations(facts), inconclusive, choice))
        return exit === 2 || exit === 3 || exit === 4
      },
    )
  })

  test("removing a world from a passing, locked run gives 2", async () => {
    await assertProperty(
      "remove a world",
      Arbitrary.all([passingArbitrary, Arbitrary.schema(Schema.Literals([0, 1, 2, 3, 4]))]),
      ([facts, choice]) =>
        exitOf(facts) === 0 &&
        exitOf({ ...facts, worlds: facts.worlds.filter((_, index) => index !== choice % facts.worlds.length) }) === 2,
    )
  })
})
