import { describe, expect, test } from "bun:test"
import { Arbitrary, Result, Schema } from "effect"
import { decodeHistory, encodeHistory, type History } from "../src/history"
import { toolPairing } from "../src/oracle/builtin/tool-pairing"
import type { HistoryEvent, OpId, OracleVerdict } from "../src/schema"
import { opId, oracleName } from "./support/ids"
import { isRecord } from "./support/json"
import { assertProperty } from "./support/property"
import { referenceHistory } from "./support/reference-model"

const context = { workspace: { files: {} } }

const judge = (history: History): OracleVerdict => toolPairing(history, context)

const field = (event: HistoryEvent, key: string): unknown => (isRecord(event.value) ? event.value[key] : undefined)

const invokes = (history: History, f: HistoryEvent["f"]): ReadonlyArray<HistoryEvent> => history.filter((event) => event.type === "invoke" && event.f === f)

const witnessOf = (verdict: OracleVerdict): ReadonlyArray<OpId> => (verdict._tag === "Violation" ? verdict.witness : [])

const nextId = (history: History): OpId => opId(Math.max(0, ...history.map((event) => Number(event.id.slice(3)))) + 1)

// Fails the test when a lookup the case depends on finds nothing.
const defined = <A>(value: A | undefined): A => Result.getOrThrow(value === undefined ? Result.fail("a lookup the test depends on found nothing") : Result.succeed(value))

// A mutation must leave a history the decoder accepts; otherwise the oracle would be judging a file
// no recorder could have written, and an Inconclusive would hide behind it.
const decodable = (history: History): boolean => Result.isSuccess(Result.flatMap(encodeHistory(history), decodeHistory))

// ---- Seeded mutations -------------------------------------------------------------------------

const dropOp = (history: History, id: OpId): History => history.filter((event) => event.id !== id)

// Re-sends one tool result: a second op with the same value right after the original completes.
const duplicateOp = (history: History, id: OpId): { readonly history: History; readonly added: OpId } => {
  const added = nextId(history)
  const okIndex = history.findIndex((event) => event.id === id && event.type !== "invoke")
  const invoke = history.find((event) => event.id === id && event.type === "invoke")
  const ok = history[okIndex]
  if (invoke === undefined || ok === undefined) return { history, added }
  return {
    history: [...history.slice(0, okIndex + 1), { ...invoke, id: added, t: ok.t }, { ...ok, id: added }, ...history.slice(okIndex + 1)],
    added,
  }
}

const reportForeign = (history: History, id: OpId): History =>
  history.map((event) => (event.id === id && isRecord(event.value) ? { ...event, value: { ...event.value, reportedId: "call_0000000000000000" } } : event))

const asInfo = (history: History, id: OpId): History =>
  history.map((event) => (event.id === id && event.type === "ok" ? { ...event, type: "info", value: { cancelled: true } } : event))

// The other branch of an info tool-call: the SUT never received it, so no later turn echoes or
// reports it. For the last call of a session that means the final turn and its results are gone.
const withoutLaterTurn = (history: History, call: HistoryEvent): History => {
  const session = field(call, "session")
  const k = field(call, "k")
  const later = invokes(history, "provider-turn").filter((turn) => field(turn, "session") === session && typeof k === "number" && field(turn, "k") === k + 1)
  const laterIds = new Set<string>(later.map((turn) => turn.id))
  const reports = invokes(history, "tool-result").filter((result) => laterIds.has(String(field(result, "turnOp"))))
  const gone = new Set<string>([...laterIds, ...reports.map((result) => result.id)])
  return history.filter((event) => !gone.has(event.id))
}

const pickFrom = <A>(items: ReadonlyArray<A>, pick: number): A | undefined => items[pick % Math.max(1, items.length)]

// ---- Unit cases -------------------------------------------------------------------------------

describe("tool-pairing", () => {
  const reference = referenceHistory({ seed: 7n, sessions: 4, turnsPerSession: 6 })
  const results = invokes(reference, "tool-result")
  const calls = invokes(reference, "tool-call")

  test("a correct run of the tool loop is Ok", () => {
    expect(results.length).toBe(4 * 15)
    expect(judge(reference)).toEqual({ _tag: "Ok", oracle: oracleName("tool-pairing") })
  })

  test("a dropped tool result is a Violation whose witness names the issuing call and the turn that lost the result", () => {
    const target = defined(results[10])
    const call = defined(calls.find((candidate) => field(candidate, "toolCallId") === field(target, "reportedId")))
    const verdict = judge(dropOp(reference, target.id))
    expect(verdict._tag).toBe("Violation")
    expect(witnessOf(verdict).some((id) => id === field(target, "turnOp"))).toBe(true)
    expect(witnessOf(verdict)).toContain(call.id)
    expect(verdict._tag === "Violation" ? verdict.explanation : "").toContain("reported 0 results")
  })

  test("a duplicated tool result is a Violation whose witness names both results", () => {
    const target = defined(results[3])
    const mutated = duplicateOp(reference, target.id)
    expect(decodable(mutated.history)).toBe(true)
    const verdict = judge(mutated.history)
    expect(verdict._tag).toBe("Violation")
    expect(witnessOf(verdict)).toContain(mutated.added)
    expect(witnessOf(verdict)).toContain(target.id)
  })

  test("a tool result for an id the stub never issued is a Violation whose witness names that result", () => {
    const target = defined(results[0])
    const verdict = judge(reportForeign(reference, target.id))
    expect(verdict._tag).toBe("Violation")
    expect(witnessOf(verdict)[0]).toBe(target.id)
    expect(verdict._tag === "Violation" ? verdict.explanation : "").toContain("never issued")
  })

  test("a tool result for an id the stub issues only later counts as never issued", () => {
    const early = defined(invokes(reference, "provider-turn").find((turn) => field(turn, "k") === 0))
    const laterCall = defined(calls.find((call) => field(call, "k") === 3 && field(call, "session") === field(early, "session")))
    const reportId = nextId(reference)
    const at = reference.findIndex((event) => event.id === early.id)
    const report: HistoryEvent = {
      id: reportId,
      process: "stub",
      type: "invoke",
      f: "tool-result",
      value: { session: String(field(early, "session")), reportedId: String(field(laterCall, "toolCallId")), turnOp: early.id },
      t: early.t,
    }
    const mutated = [...reference.slice(0, at + 1), report, ...reference.slice(at + 1)]
    expect(decodable(mutated)).toBe(true)
    expect(witnessOf(judge(mutated))).toContain(reportId)
  })

  test("a tool result that points at no earlier provider turn makes the history unjudgeable: Inconclusive, never Ok", () => {
    const target = defined(results[5])
    const mutated = reference.map((event) =>
      event.id === target.id && event.type === "invoke" && isRecord(event.value) ? { ...event, value: { ...event.value, turnOp: "op-999999" } } : event,
    )
    expect(judge(mutated)).toEqual({ _tag: "Inconclusive", oracle: oracleName("tool-pairing"), reason: "history-malformed" })
  })

  test("an invoke value with a field the payload schema does not know is Inconclusive, never Ok", () => {
    const target = defined(calls[0])
    const mutated = reference.map((event) => (event.id === target.id && event.type === "invoke" && isRecord(event.value) ? { ...event, value: { ...event.value, extra: 1 } } : event))
    expect(judge(mutated)).toEqual({ _tag: "Inconclusive", oracle: oracleName("tool-pairing"), reason: "history-malformed" })
  })

  test("an info tool-call is Ok both when the SUT received it and when it did not", () => {
    const last = defined(calls.find((call) => field(call, "k") === 4))
    const present = asInfo(reference, last.id)
    const absent = withoutLaterTurn(present, last)
    expect(present.some((event) => event.id === last.id && event.type === "info")).toBe(true)
    expect(absent.length).toBeLessThan(present.length)
    expect(decodable(present)).toBe(true)
    expect(decodable(absent)).toBe(true)
    expect(judge(present)).toEqual({ _tag: "Ok", oracle: oracleName("tool-pairing") })
    expect(judge(absent)).toEqual({ _tag: "Ok", oracle: oracleName("tool-pairing") })
  })
})

// ---- Oracle soundness (property) --------------------------------------------------------------

const Bounded = Schema.Int.pipe(Schema.check(Schema.isGreaterThanOrEqualTo(0), Schema.isLessThanOrEqualTo(1_000_000)))
const caseArbitrary = Arbitrary.schema(
  Schema.Struct({ seed: Bounded, sessions: Schema.Literals([1, 2, 3, 4]), turns: Schema.Literals([2, 3, 4, 5, 6]), pick: Bounded }),
)
type Case = typeof caseArbitrary extends Arbitrary.Arbitrary<infer A> ? A : never

const historyOf = (c: Case): History => referenceHistory({ seed: BigInt(c.seed), sessions: c.sessions, turnsPerSession: c.turns })

describe("tool-pairing soundness", () => {
  test("histories from the correct reference model are Ok", async () => {
    await assertProperty("model Ok", caseArbitrary, (c) => judge(historyOf(c))._tag === "Ok")
  })

  test("one dropped tool result gives a Violation whose witness contains the turn that lost it", async () => {
    await assertProperty("dropped result", caseArbitrary, (c) => {
      const history = historyOf(c)
      const target = pickFrom(invokes(history, "tool-result"), c.pick)
      if (target === undefined) return false
      const mutated = dropOp(history, target.id)
      return decodable(mutated) && witnessOf(judge(mutated)).some((id) => id === field(target, "turnOp"))
    })
  })

  test("one duplicated tool result gives a Violation whose witness contains the duplicate", async () => {
    await assertProperty("duplicated result", caseArbitrary, (c) => {
      const history = historyOf(c)
      const target = pickFrom(invokes(history, "tool-result"), c.pick)
      if (target === undefined) return false
      const mutated = duplicateOp(history, target.id)
      return decodable(mutated.history) && witnessOf(judge(mutated.history)).includes(mutated.added)
    })
  })

  test("one foreign tool result gives a Violation whose witness contains that result", async () => {
    await assertProperty("foreign result", caseArbitrary, (c) => {
      const history = historyOf(c)
      const target = pickFrom(invokes(history, "tool-result"), c.pick)
      if (target === undefined) return false
      const mutated = reportForeign(history, target.id)
      return decodable(mutated) && witnessOf(judge(mutated)).includes(target.id)
    })
  })

  test("an info tool-call gives Ok with the effect present and with it absent", async () => {
    await assertProperty("info both branches", caseArbitrary, (c) => {
      const history = historyOf(c)
      const lastCalls = invokes(history, "tool-call").filter((call) => field(call, "k") === c.turns - 2)
      const target = pickFrom(lastCalls, c.pick)
      if (target === undefined) return false
      const present = asInfo(history, target.id)
      const absent = withoutLaterTurn(present, target)
      return (
        absent.length < present.length &&
        decodable(present) &&
        decodable(absent) &&
        judge(present)._tag === "Ok" &&
        judge(absent)._tag === "Ok"
      )
    })
  })
})
