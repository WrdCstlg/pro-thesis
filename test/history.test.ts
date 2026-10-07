import { describe, expect, test } from "bun:test"
import { Arbitrary, Result } from "effect"
import { decodeHistory, decodeHistoryLine, encodeHistory, encodeHistoryLine, type HistoryError } from "../src/history"
import { HistoryEvent, type OpKind, type OpType } from "../src/schema"
import { opId, sessionId } from "./support/ids"
import { assertProperty } from "./support/property"

type Reason = HistoryError["reason"]

const event = (id: number, type: OpType, t: number, extra: Partial<HistoryEvent> = {}): HistoryEvent => ({
  id: opId(id),
  process: sessionId(1),
  type,
  f: "prompt" satisfies OpKind,
  value: null,
  t,
  ...extra,
})

const lineOf = (e: HistoryEvent): string => Result.getOrThrow(encodeHistoryLine(e))
const textOf = (events: ReadonlyArray<HistoryEvent>): string => events.map((e) => `${lineOf(e)}\n`).join("")

const reasonOf = (text: string): readonly [Reason, number] | "decoded" => {
  const decoded = decodeHistory(text)
  return Result.isFailure(decoded) ? [decoded.failure.reason, decoded.failure.line] : "decoded"
}

describe("decodeHistory: well-formed histories", () => {
  test("an empty text is the empty history", () => {
    expect(decodeHistory("")).toEqual(Result.succeed([]))
  })

  test("a final newline is optional", () => {
    const events = [event(1, "invoke", 0), event(1, "ok", 3)]
    const withNewline = textOf(events)
    expect(decodeHistory(withNewline)).toEqual(Result.succeed(events))
    expect(decodeHistory(withNewline.slice(0, -1))).toEqual(Result.succeed(events))
  })

  test("events at the same t are allowed; t only has to not decrease", () => {
    expect(Result.isSuccess(decodeHistory(textOf([event(1, "invoke", 5), event(2, "invoke", 5), event(2, "ok", 5), event(1, "ok", 5)])))).toBe(true)
  })

  test("an info event may complete an invoke or stand alone as a recorded fact", () => {
    const completes = [event(1, "invoke", 0), event(1, "info", 9)]
    const standalone = [event(2, "info", 0, { process: "harness", f: "crash" })]
    expect(Result.isSuccess(decodeHistory(textOf(completes)))).toBe(true)
    expect(Result.isSuccess(decodeHistory(textOf(standalone)))).toBe(true)
  })

  test("an invoke that is never completed is accepted: the operation is still in flight", () => {
    expect(Result.isSuccess(decodeHistory(textOf([event(1, "invoke", 0)])))).toBe(true)
  })

  test("a value that contains newlines is written on one line and read back unchanged", () => {
    const multiline = event(1, "invoke", 0, { value: { text: "a\nb\r\nc", nested: ["x\ny"] } })
    expect(lineOf(multiline).includes("\n")).toBe(false)
    expect(decodeHistory(textOf([multiline]))).toEqual(Result.succeed([multiline]))
  })
})

describe("decodeHistory: refusals, each with its reason and line number", () => {
  const ok = (id: number, t: number) => lineOf(event(id, "ok", t))
  const invoke = (id: number, t: number) => lineOf(event(id, "invoke", t))

  test("a blank line inside the history", () => {
    expect(reasonOf(`${invoke(1, 0)}\n\n${ok(1, 1)}\n`)).toEqual(["blank-line", 2])
    expect(reasonOf("\n")).toEqual(["blank-line", 1])
  })

  test("a truncated last line, as left by a crash mid-write, is an error and not a shorter history", () => {
    const full = `${invoke(1, 0)}\n${ok(1, 1)}\n`
    expect(reasonOf(full.slice(0, -10))).toEqual(["invalid-line", 2])
  })

  test("a line that is not an event: wrong shape, unknown f, bad id, negative t, extra field", () => {
    const base = JSON.stringify({ id: "op-1", process: "session-1", type: "invoke", f: "prompt", value: null, t: 0 })
    expect(reasonOf(`${base}\n`)).toBe("decoded")
    const bad = [
      "not json",
      "[]",
      base.replace('"prompt"', '"teleport"'),
      base.replace('"op-1"', '"op-x"'),
      base.replace('"t":0', '"t":-1'),
      base.replace('"session-1"', '"somebody"'),
      base.replace("}", ',"extra":1}'),
    ]
    bad.forEach((line) => expect(reasonOf(`${line}\n`)).toEqual(["invalid-line", 1]))
  })

  test("t decreasing down the file", () => {
    expect(reasonOf(`${invoke(1, 5)}\n${invoke(2, 4)}\n`)).toEqual(["time-regressed", 2])
  })

  test("two invokes with one id, and an invoke after an info that stood alone", () => {
    expect(reasonOf(`${invoke(1, 0)}\n${invoke(1, 1)}\n`)).toEqual(["duplicate-invoke", 2])
    expect(reasonOf(`${lineOf(event(1, "info", 0))}\n${invoke(1, 1)}\n`)).toEqual(["duplicate-invoke", 2])
  })

  test("an ok or fail with no earlier invoke", () => {
    expect(reasonOf(`${ok(1, 0)}\n`)).toEqual(["completion-without-invoke", 1])
    expect(reasonOf(`${lineOf(event(1, "fail", 0))}\n`)).toEqual(["completion-without-invoke", 1])
    expect(reasonOf(`${lineOf(event(1, "info", 0))}\n${ok(1, 1)}\n`)).toEqual(["completion-without-invoke", 2])
  })

  test("a second completion of the same id, whatever its type", () => {
    expect(reasonOf(`${invoke(1, 0)}\n${ok(1, 1)}\n${ok(1, 2)}\n`)).toEqual(["duplicate-completion", 3])
    expect(reasonOf(`${invoke(1, 0)}\n${ok(1, 1)}\n${lineOf(event(1, "info", 2))}\n`)).toEqual(["duplicate-completion", 3])
    expect(reasonOf(`${invoke(1, 0)}\n${lineOf(event(1, "fail", 1))}\n${ok(1, 2)}\n`)).toEqual(["duplicate-completion", 3])
    expect(reasonOf(`${lineOf(event(1, "info", 0))}\n${lineOf(event(1, "info", 1))}\n`)).toEqual(["duplicate-completion", 2])
  })

  test("a completion whose process or f differs from its invoke", () => {
    expect(reasonOf(`${invoke(1, 0)}\n${lineOf(event(1, "ok", 1, { f: "resume" }))}\n`)).toEqual(["completion-mismatch", 2])
    expect(reasonOf(`${invoke(1, 0)}\n${lineOf(event(1, "ok", 1, { process: "stub" }))}\n`)).toEqual(["completion-mismatch", 2])
    expect(reasonOf(`${invoke(1, 0)}\n${lineOf(event(1, "info", 1, { f: "crash" }))}\n`)).toEqual(["completion-mismatch", 2])
  })

  test("the line number is the 1-based position of the first offending line, not of a later one", () => {
    expect(reasonOf(`${invoke(1, 0)}\n${ok(2, 1)}\n${ok(1, 2)}\n${ok(1, 3)}\n`)).toEqual(["completion-without-invoke", 2])
  })
})

describe("history encoding", () => {
  test("encodeHistory writes one newline-terminated line per event and the empty history as the empty string", () => {
    expect(encodeHistory([])).toEqual(Result.succeed(""))
    const events = [event(1, "invoke", 0), event(1, "ok", 2), event(2, "info", 2, { process: "harness", f: "crash" })]
    const text = Result.getOrThrow(encodeHistory(events))
    expect(text.split("\n")).toHaveLength(events.length + 1)
    expect(text.endsWith("\n")).toBe(true)
    expect(decodeHistory(text)).toEqual(Result.succeed(events))
  })

  test("encoding is stable: decode(encode(e)) encodes to the same line, for every event the schema admits", async () => {
    await assertProperty("line idempotence", Arbitrary.schema(HistoryEvent), (candidate) => {
      const first = encodeHistoryLine(candidate)
      const decoded = Result.flatMap(first, decodeHistoryLine)
      const second = Result.flatMap(decoded, encodeHistoryLine)
      return Result.isSuccess(first) && Result.isSuccess(second) && second.success === first.success
    })
  })
})
