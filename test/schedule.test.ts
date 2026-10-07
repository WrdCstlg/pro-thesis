import { describe, expect, test } from "bun:test"
import { Arbitrary, Result, Schema } from "effect"
import { formatFault, parseFault, parseFaults, type ScheduleParseError } from "../src/schedule"
import { FAULT_KINDS, ScheduledFault, type Fault } from "../src/schema"
import { assertProperty, sampleOf } from "./support/property"

type Reason = ScheduleParseError["reason"]

const accepted: ReadonlyArray<readonly [string, ScheduledFault]> = [
  ["stream.disconnect(afterChunks=2)@t=999999", { fault: { _tag: "stream.disconnect", afterChunks: 2 }, trigger: { _tag: "AtTime", ms: 999999 } }],
  ["stream.stall(ms=500)@turn=3", { fault: { _tag: "stream.stall", ms: 500 }, trigger: { _tag: "AtTurn", turn: 3 } }],
  ["stream.malformed@turn=1", { fault: { _tag: "stream.malformed" }, trigger: { _tag: "AtTurn", turn: 1 } }],
  ["stream.http(status=429,retryAfter=2)@t=0", { fault: { _tag: "stream.http", status: 429, retryAfter: 2 }, trigger: { _tag: "AtTime", ms: 0 } }],
  ["stream.http(status=500)@turn=2", { fault: { _tag: "stream.http", status: 500 }, trigger: { _tag: "AtTurn", turn: 2 } }],
  ["runtime.crash(restartAfter=250)@t=1000", { fault: { _tag: "runtime.crash", restartAfter: 250 }, trigger: { _tag: "AtTime", ms: 1000 } }],
  ["client.duplicate@turn=4", { fault: { _tag: "client.duplicate" }, trigger: { _tag: "AtTurn", turn: 4 } }],
  ["client.conflict@t=10", { fault: { _tag: "client.conflict" }, trigger: { _tag: "AtTime", ms: 10 } }],
  ["client.interrupt@turn=5", { fault: { _tag: "client.interrupt" }, trigger: { _tag: "AtTurn", turn: 5 } }],
]

const rejected: ReadonlyArray<readonly [string, Reason]> = [
  ["", "malformed"],
  ["stream.malformed", "malformed"],
  ["stream.malformed@turn=1@t=1", "malformed"],
  ["stream.explode(x=1)@t=1", "unknown-kind"],
  [" stream.malformed@turn=1", "unknown-kind"],
  ["Stream.Malformed@turn=1", "unknown-kind"],
  ["@turn=1", "unknown-kind"],
  ["stream.disconnect(afterChunks=2,foo=1)@t=1", "invalid-params"],
  ["stream.disconnect@t=1", "invalid-params"],
  ["stream.disconnect(afterChunks=0)@t=1", "invalid-params"],
  ["stream.disconnect(afterChunks=two)@t=1", "invalid-params"],
  ["stream.disconnect(afterChunks=-1)@t=1", "invalid-params"],
  ["stream.disconnect(afterChunks=02)@t=1", "invalid-params"],
  ["stream.disconnect(afterChunks=1.5)@t=1", "invalid-params"],
  ["stream.disconnect(afterChunks=1e3)@t=1", "invalid-params"],
  ["stream.disconnect(afterChunks=1000000000000000)@t=1", "invalid-params"],
  ["stream.disconnect(afterChunks=99999999999999999999)@t=1", "invalid-params"],
  ["stream.http(status=404)@t=1", "invalid-params"],
  ["stream.http(status=429,retryAfter=-1)@t=1", "invalid-params"],
  ["stream.malformed(x=1)@t=1", "invalid-params"],
  ["stream.malformed(_tag=1)@t=1", "invalid-params"],
  ["runtime.crash@t=1", "invalid-params"],
  ["stream.disconnect(afterChunks=2,afterChunks=3)@t=1", "malformed"],
  ["stream.disconnect(afterChunks)@t=1", "malformed"],
  ["stream.disconnect(=2)@t=1", "malformed"],
  ["stream.disconnect(afterChunks=)@t=1", "malformed"],
  ["stream.disconnect(afterChunks=2@t=1", "malformed"],
  ["stream.disconnect(afterChunks=2,)@t=1", "malformed"],
  ["stream.disconnect((afterChunks=2))@t=1", "malformed"],
  ["stream.malformed@", "invalid-trigger"],
  ["stream.malformed@x=1", "invalid-trigger"],
  ["stream.malformed@t=", "invalid-trigger"],
  ["stream.malformed@t=-1", "invalid-trigger"],
  ["stream.malformed@turn=0", "invalid-trigger"],
  ["stream.malformed@t", "invalid-trigger"],
  ["stream.malformed@t=1,turn=2", "invalid-trigger"],
  ["stream.malformed@T=1", "invalid-trigger"],
]

const reasonOf = (input: string): Reason | "accepted" => {
  const result = parseFault(input)
  return Result.isFailure(result) ? result.failure.reason : "accepted"
}

describe("parseFault", () => {
  test("accepts every example in the grammar and produces the exact value", () => {
    accepted.forEach(([input, expected]) => expect(parseFault(input)).toEqual(Result.succeed(expected)))
  })

  test("a fault with no params parses the same with and without empty parentheses", () => {
    expect(parseFault("stream.malformed()@turn=1")).toEqual(parseFault("stream.malformed@turn=1"))
  })

  test("a retryAfter that was not given is absent from the value, not undefined", () => {
    const parsed = parseFault("stream.http(status=500)@turn=2")
    expect(Result.isSuccess(parsed) ? Object.keys(parsed.success.fault) : []).toEqual(["_tag", "status"])
  })

  test("every fault kind the schema lists has an accepted example, and no example uses another kind", () => {
    const kinds = accepted.flatMap(([input]) => {
      const parsed = parseFault(input)
      return Result.isSuccess(parsed) ? [parsed.success.fault._tag] : []
    })
    expect([...new Set(kinds)].toSorted()).toEqual([...FAULT_KINDS].toSorted())
  })

  test("rejects each malformed input with the reason that names its defect", () => {
    expect(rejected.map(([input]) => [input, reasonOf(input)])).toEqual(rejected.map(([input, reason]) => [input, reason]))
  })

  test("the error carries the input and a non-empty detail", () => {
    const failure = parseFault("stream.disconnect(afterChunks=0)@t=1")
    expect(Result.isFailure(failure) ? [failure.failure.input, failure.failure.detail.length > 0] : []).toEqual(["stream.disconnect(afterChunks=0)@t=1", true])
  })

  test("parseFaults keeps the order and reports the first bad schedule", () => {
    expect(parseFaults(accepted.map(([input]) => input))).toEqual(Result.succeed(accepted.map(([, expected]) => expected)))
    const failure = parseFaults(["client.duplicate@turn=1", "bogus@t=1", "stream.malformed"])
    expect(Result.isFailure(failure) ? [failure.failure.input, failure.failure.reason] : []).toEqual(["bogus@t=1", "unknown-kind"])
    expect(parseFaults([])).toEqual(Result.succeed([]))
  })
})

describe("formatFault", () => {
  test("writes the canonical spelling of every accepted example that is already canonical", () => {
    accepted.forEach(([input, value]) => expect(formatFault(value)).toEqual(input))
  })
})

const scheduledArbitrary = Arbitrary.schema(ScheduledFault)

describe("schedule properties", () => {
  test("the generator is not vacuous: it reaches every fault kind, both triggers and both http forms", async () => {
    const sample = await sampleOf(scheduledArbitrary, 800)
    const kinds = new Set(sample.map((scheduled) => scheduled.fault._tag))
    const http = sample.flatMap((scheduled): ReadonlyArray<Fault> => (scheduled.fault._tag === "stream.http" ? [scheduled.fault] : []))
    expect([...kinds].toSorted()).toEqual([...FAULT_KINDS].toSorted())
    expect(new Set(sample.map((scheduled) => scheduled.trigger._tag)).size).toBe(2)
    expect(http.some((fault) => fault._tag === "stream.http" && fault.retryAfter !== undefined)).toBe(true)
    expect(http.some((fault) => fault._tag === "stream.http" && fault.retryAfter === undefined)).toBe(true)
  })

  test("parse(format(x)) is x for every schedule the schema admits", async () => {
    await assertProperty("round trip", scheduledArbitrary, (scheduled) => {
      const parsed = parseFault(formatFault(scheduled))
      return Result.isSuccess(parsed) && Bun.deepEquals(parsed.success, scheduled, true)
    })
  })

  test("deleting any one character of a valid schedule either fails or yields a schedule that round-trips", async () => {
    await assertProperty(
      "mutated input",
      Arbitrary.all([scheduledArbitrary, Arbitrary.schema(Schema.Int)]),
      ([scheduled, choice]) => {
        const text = formatFault(scheduled)
        const at = Math.abs(choice) % text.length
        const parsed = parseFault(text.slice(0, at) + text.slice(at + 1))
        if (Result.isFailure(parsed)) return true
        const again = parseFault(formatFault(parsed.success))
        return Result.isSuccess(again) && Bun.deepEquals(again.success, parsed.success, true)
      },
    )
  })

  test("parseFault is total: any string gives a value or a typed error, never an exception", async () => {
    await assertProperty("total", Arbitrary.schema(Schema.String), (text) => {
      const parsed = parseFault(text)
      return Result.isSuccess(parsed) || Result.isFailure(parsed)
    })
  })
})
