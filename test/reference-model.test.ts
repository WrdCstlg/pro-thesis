import { describe, expect, test } from "bun:test"
import { Result, Schema } from "effect"
import { decodeHistory, encodeHistory } from "../src/history"
import type { HistoryEvent } from "../src/schema"
import { isRecord, type Json } from "./support/json"
import { opId } from "./support/ids"
import { PromptValue, ProviderTurnValue, ToolCallValue, ToolResultValue, referenceHistory, type ReferenceParams } from "./support/reference-model"

const params = (seed: bigint, sessions = 4, turnsPerSession = 6): ReferenceParams => ({ seed, sessions, turnsPerSession })

const fieldOf = (value: Json, key: string): Json | undefined => (isRecord(value) ? value[key] : undefined)

const valueSessionOf = (event: HistoryEvent): string | undefined => {
  const session = fieldOf(event.value, "session")
  return typeof session === "string" ? session : undefined
}

const ofKind = (history: ReadonlyArray<HistoryEvent>, f: HistoryEvent["f"], type: HistoryEvent["type"]) =>
  history.filter((event) => event.f === f && event.type === type)

const SEEDS: ReadonlyArray<bigint> = Array.from({ length: 25 }, (_, index) => BigInt(index) * 7919n + 1n)

describe("referenceHistory", () => {
  test("decodes under the decoder's own rules and survives encode then decode unchanged, for 25 seeds", () => {
    SEEDS.forEach((seed) => {
      const history = referenceHistory(params(seed))
      const text = Result.getOrThrow(encodeHistory(history))
      expect(decodeHistory(text)).toEqual(Result.succeed(history))
    })
  })

  test("the same seed gives the same history and different seeds give different interleavings", () => {
    expect(referenceHistory(params(42n))).toEqual(referenceHistory(params(42n)))
    const distinct = new Set(SEEDS.map((seed) => JSON.stringify(referenceHistory(params(seed)).map((event) => [event.id, event.t]))))
    expect(distinct.size).toBeGreaterThan(20)
  })

  test("each session has one prompt, T provider turns, T-1 tool calls and T-1 tool results, every operation invoked then completed", () => {
    const sessions = 3
    const turns = 5
    const history = referenceHistory(params(9n, sessions, turns))
    expect(ofKind(history, "prompt", "invoke")).toHaveLength(sessions)
    expect(ofKind(history, "provider-turn", "invoke")).toHaveLength(sessions * turns)
    expect(ofKind(history, "tool-call", "invoke")).toHaveLength(sessions * (turns - 1))
    expect(ofKind(history, "tool-result", "invoke")).toHaveLength(sessions * (turns - 1))
    expect(history).toHaveLength(2 * sessions * (1 + turns + 2 * (turns - 1)))
    const perId = Object.values(Object.groupBy(history, (event) => event.id)).map((events) => (events ?? []).map((event) => event.type))
    expect(perId.every((types) => types.length === 2 && types[0] === "invoke" && types[1] === "ok")).toBe(true)
  })

  test("op ids are 1..N in order of first appearance, with no gaps", () => {
    const history = referenceHistory(params(5n))
    const firstSeen = Array.from(new Set(history.map((event) => event.id)))
    expect(firstSeen).toEqual(firstSeen.map((_, index) => opId(index + 1)))
  })

  test("every value decodes with the payload schema for its operation kind", () => {
    const history = referenceHistory(params(11n))
    const schemas = { prompt: PromptValue, "provider-turn": ProviderTurnValue, "tool-call": ToolCallValue, "tool-result": ToolResultValue } as const
    const results = history.map((event) =>
      event.f === "prompt" || event.f === "provider-turn" || event.f === "tool-call" || event.f === "tool-result"
        ? Result.isSuccess(Schema.decodeUnknownResult(schemas[event.f], { onExcessProperty: "error" })(event.value))
        : false,
    )
    expect(results.every((decoded) => decoded)).toBe(true)
  })

  test("provider turns of a session run k = 0..T-1 in order, and each tool call is reported back after it was issued", () => {
    const turns = 6
    const history = referenceHistory(params(13n, 4, turns))
    const sessions = Array.from(new Set(history.flatMap((event) => valueSessionOf(event) ?? [])))
    expect(sessions).toHaveLength(4)
    sessions.forEach((session) => {
      const mine = history.filter((event) => valueSessionOf(event) === session)
      const ks = mine.flatMap((event) => (event.f === "provider-turn" && event.type === "invoke" ? [fieldOf(event.value, "k")] : []))
      expect(ks).toEqual(Array.from({ length: turns }, (_, k) => k))
      const calls = mine.filter((event) => event.f === "tool-call" && event.type === "ok")
      const results = mine.filter((event) => event.f === "tool-result" && event.type === "ok")
      expect(calls).toHaveLength(turns - 1)
      calls.forEach((call) => {
        const callId = fieldOf(call.value, "toolCallId")
        const reported = results.filter((result) => fieldOf(result.value, "toolCallId") === callId)
        expect(reported).toHaveLength(1)
        expect((reported[0]?.t ?? -1) >= call.t).toBe(true)
        expect(history.indexOf(reported[0] ?? call)).toBeGreaterThan(history.indexOf(call))
      })
    })
  })

  test("sessions really interleave: in most seeds, events of different sessions alternate", () => {
    const alternating = SEEDS.filter((seed) => {
      const sessionsInOrder = referenceHistory(params(seed)).map(valueSessionOf)
      return sessionsInOrder.some((session, index) => index > 0 && session !== sessionsInOrder[index - 1])
    })
    expect(alternating.length).toBe(SEEDS.length)
    const history = referenceHistory(params(1n))
    const first = history[0]
    expect(first).toBeDefined()
    const firstSession = first === undefined ? undefined : valueSessionOf(first)
    expect(firstSession).toBeDefined()
    expect(history.slice(0, 20).some((event) => valueSessionOf(event) !== firstSession)).toBe(true)
  })
})
