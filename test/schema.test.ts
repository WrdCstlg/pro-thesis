import { describe, expect, test } from "bun:test"
import { Result, Schema } from "effect"
import {
  AppendLineArgs,
  builtinOracleName,
  BuiltinOracleName,
  ExitCode,
  Fault,
  FAULT_KINDS,
  HistoryEvent,
  InconclusiveReason,
  MessageId,
  OpId,
  opIdOf,
  OracleName,
  OracleVerdict,
  PortAnnouncement,
  RunId,
  RunVerdict,
  Seed,
  SessionId,
  sessionIdOf,
  Sha256,
  ToolCallId,
  ToolResultValue,
  WorldId,
  worldIdOf,
} from "../src/schema"

const accepts = <A, I>(schema: Schema.Codec<A, I>, input: unknown): boolean =>
  Result.isSuccess(Schema.decodeUnknownResult(schema, { onExcessProperty: "error" })(input))

const HEX64 = "a".repeat(64)

describe("brand patterns", () => {
  const table: ReadonlyArray<{
    readonly name: string
    readonly accepts: (input: unknown) => boolean
    readonly good: ReadonlyArray<string>
    readonly bad: ReadonlyArray<unknown>
  }> = [
    { name: "OpId", accepts: (input) => accepts(OpId, input), good: ["op-1", "op-0", "op-123456"], bad: ["op-", "op-1a", "OP-1", "op--1", " op-1", "op-1\n", "1", "", 1, null] },
    { name: "SessionId", accepts: (input) => accepts(SessionId, input), good: ["session-1", "session-42"], bad: ["session-", "session-x", "sessions-1", "session-1 ", "", undefined] },
    { name: "WorldId", accepts: (input) => accepts(WorldId, input), good: ["world-1", "world-100"], bad: ["world-", "world-a", "worlds-1", "world-1\n", ""] },
    { name: "RunId", accepts: (input) => accepts(RunId, input), good: ["run-1", "run-2026-10-06-ab12", "run-a"], bad: ["run-", "run-A", "run-a_b", "runs-1", "run-a b", ""] },
    { name: "Sha256", accepts: (input) => accepts(Sha256, input), good: [HEX64, "0123456789abcdef".repeat(4)], bad: ["a".repeat(63), "a".repeat(65), "A".repeat(64), "g".repeat(64), `${HEX64}\n`, ""] },
    { name: "ToolCallId", accepts: (input) => accepts(ToolCallId, input), good: ["call_0", "call_deadbeef"], bad: ["call_", "call-0", "call_G", "CALL_0", "call_0 ", ""] },
    { name: "MessageId", accepts: (input) => accepts(MessageId, input), good: ["m", "msg-session-1-0"], bad: [""] },
    { name: "OracleName", accepts: (input) => accepts(OracleName, input), good: ["tool-pairing", "a", "9x", "x-y-z"], bad: ["", "-a", "A", "a_b", "a b", "a.b"] },
  ]

  table.forEach((row) => {
    test(`${row.name} accepts its good examples and rejects its bad ones`, () => {
      expect(row.good.map((input) => [input, row.accepts(input)])).toEqual(row.good.map((input) => [input, true]))
      expect(row.bad.map((input) => [input, row.accepts(input)])).toEqual(row.bad.map((input) => [input, false]))
    })
  })

  test("a string that is accepted by one id kind is not thereby accepted by another", () => {
    expect(accepts(SessionId, "world-1")).toBe(false)
    expect(accepts(WorldId, "op-1")).toBe(false)
    expect(accepts(OpId, "session-1")).toBe(false)
    expect(accepts(ToolCallId, HEX64)).toBe(false)
  })
})

describe("Seed", () => {
  test("accepts canonical decimal strings from 0 up to 2^64 - 1", () => {
    const good = ["0", "1", "20261006", "9007199254740993", "18446744073709551615"]
    expect(good.map((input) => [input, accepts(Seed, input)])).toEqual(good.map((input) => [input, true]))
  })

  test("rejects 2^64, leading zeros, signs, fractions, exponents, hex, whitespace, the empty string and non-strings", () => {
    const bad: ReadonlyArray<unknown> = ["18446744073709551616", "00", "01", "+1", "-1", "1.0", "1e3", "0x10", " 1", "1 ", "", 1, 1n, null]
    expect(bad.map((input) => [String(input), accepts(Seed, input)])).toEqual(bad.map((input) => [String(input), false]))
  })
})

describe("Fault", () => {
  test("decodes one example of each listed kind, and the union has exactly as many members as the list", () => {
    const examples: ReadonlyArray<{ readonly _tag: string; readonly [param: string]: unknown }> = [
      { _tag: "stream.disconnect", afterChunks: 1 },
      { _tag: "stream.stall", ms: 1 },
      { _tag: "stream.malformed" },
      { _tag: "stream.http", status: 429 },
      { _tag: "runtime.crash", restartAfter: 0 },
      { _tag: "client.duplicate" },
      { _tag: "client.conflict" },
      { _tag: "client.interrupt" },
    ]
    expect(FAULT_KINDS).toHaveLength(8)
    expect(examples.map((example) => example._tag)).toEqual([...FAULT_KINDS])
    expect(examples.every((example) => accepts(Fault, example))).toBe(true)
    expect(Fault.members).toHaveLength(FAULT_KINDS.length)
  })

  test("rejects a kind that is not listed, a missing tag, and a parameter the kind does not take", () => {
    expect(accepts(Fault, { _tag: "stream.reset" })).toBe(false)
    expect(accepts(Fault, { _tag: "runtime.crash" })).toBe(false)
    expect(accepts(Fault, {})).toBe(false)
    expect(accepts(Fault, { _tag: "stream.malformed", ms: 1 })).toBe(false)
    expect(accepts(Fault, { _tag: "stream.http", status: 404 })).toBe(false)
  })

  test("enforces the parameter bounds of each kind", () => {
    expect(accepts(Fault, { _tag: "stream.disconnect", afterChunks: 1 })).toBe(true)
    expect(accepts(Fault, { _tag: "stream.disconnect", afterChunks: 0 })).toBe(false)
    expect(accepts(Fault, { _tag: "stream.stall", ms: 0 })).toBe(false)
    expect(accepts(Fault, { _tag: "stream.stall", ms: 1.5 })).toBe(false)
    expect(accepts(Fault, { _tag: "runtime.crash", restartAfter: 0 })).toBe(true)
    expect(accepts(Fault, { _tag: "runtime.crash", restartAfter: -1 })).toBe(false)
    expect(accepts(Fault, { _tag: "stream.http", status: 429, retryAfter: 0 })).toBe(true)
    expect(accepts(Fault, { _tag: "stream.http", status: 500 })).toBe(true)
  })
})

describe("OracleVerdict", () => {
  const oracle = "tool-pairing"

  test("a Violation needs a non-empty witness of op ids", () => {
    expect(accepts(OracleVerdict, { _tag: "Violation", oracle, witness: ["op-1"], explanation: "x" })).toBe(true)
    expect(accepts(OracleVerdict, { _tag: "Violation", oracle, witness: [], explanation: "x" })).toBe(false)
    expect(accepts(OracleVerdict, { _tag: "Violation", oracle, explanation: "x" })).toBe(false)
    expect(accepts(OracleVerdict, { _tag: "Violation", oracle, witness: ["not-an-op"], explanation: "x" })).toBe(false)
  })

  test("an Inconclusive needs one of the listed reasons", () => {
    expect(accepts(OracleVerdict, { _tag: "Inconclusive", oracle, reason: "oracle-timeout" })).toBe(true)
    expect(accepts(OracleVerdict, { _tag: "Inconclusive", oracle, reason: "because" })).toBe(false)
    expect(accepts(OracleVerdict, { _tag: "Inconclusive", oracle })).toBe(false)
  })

  test("an Ok names its oracle and carries nothing else", () => {
    expect(accepts(OracleVerdict, { _tag: "Ok", oracle })).toBe(true)
    expect(accepts(OracleVerdict, { _tag: "Ok" })).toBe(false)
    expect(accepts(OracleVerdict, { _tag: "Ok", oracle, witness: ["op-1"] })).toBe(false)
    expect(accepts(OracleVerdict, { _tag: "Pass", oracle })).toBe(false)
  })

  // Thirteen from M1 plus history-malformed, added in M2 for an oracle handed a history it cannot
  // decode (D-024). Pinned by name so that a renamed or dropped reason fails here.
  test("the listed inconclusive reasons are exactly the fourteen named", () => {
    const named = [
      "oracle-crashed",
      "oracle-nonzero-exit",
      "oracle-timeout",
      "oracle-malformed-output",
      "missing-probe",
      "unsupported-platform",
      "history-unsplittable",
      "no-oracle-judged",
      "world-not-asserted",
      "fault-missed-load",
      "provider-not-exercised",
      "narrowed-run",
      "interrupted",
      "history-malformed",
    ]
    expect(InconclusiveReason.literals.map(String).sort()).toEqual(named.sort())
    expect(new Set(InconclusiveReason.literals).size).toBe(14)
  })
})

describe("nominal id constructors", () => {
  test("their output decodes under the real schemas for indexes from 1 up", () => {
    const indexes = [1, 2, 9, 10, 99, 123456]
    expect(indexes.every((n) => accepts(OpId, opIdOf(n)) && accepts(SessionId, sessionIdOf(n)) && accepts(WorldId, worldIdOf(n)))).toBe(true)
    expect([opIdOf(7), sessionIdOf(7), worldIdOf(7)].map(String)).toEqual(["op-7", "session-7", "world-7"])
    expect(BuiltinOracleName.literals.every((name) => accepts(OracleName, builtinOracleName(name)))).toBe(true)
  })

  test("an index that is not a positive safe integer is clamped to 1 and stays inside the pattern", () => {
    const odd = [0, -1, 1.5, Number.NaN, Number.POSITIVE_INFINITY, 2 ** 60]
    expect(odd.map(opIdOf).map(String)).toEqual(odd.map(() => "op-1"))
    expect(odd.every((n) => accepts(OpId, opIdOf(n)) && accepts(SessionId, sessionIdOf(n)) && accepts(WorldId, worldIdOf(n)))).toBe(true)
  })
})

describe("M2 boundary payloads", () => {
  test("append_line paths are bare file names", () => {
    expect(accepts(AppendLineArgs, { path: "log.txt", line: "session-1:op-0" })).toBe(true)
    expect(["../log.txt", "a/b", "a\\b", "", "C:log"].map((path) => accepts(AppendLineArgs, { path, line: "x" }))).toEqual([false, false, false, false, false])
  })

  test("a tool result keeps the SUT's reported id as untrusted text and needs its turn link", () => {
    expect(accepts(ToolResultValue, { session: "session-1", reportedId: "not even an id", turnOp: "op-3" })).toBe(true)
    expect(accepts(ToolResultValue, { session: "session-1", reportedId: "call_1" })).toBe(false)
  })

  test("a port announcement is one port in 1..65535", () => {
    expect([1, 65535].map((port) => accepts(PortAnnouncement, { port }))).toEqual([true, true])
    expect([0, 65536, 1.5, "80"].map((port) => accepts(PortAnnouncement, { port }))).toEqual([false, false, false, false])
  })
})

describe("ExitCode", () => {
  test("accepts the normative codes and nothing else", () => {
    const allowed = [0, 1, 2, 3, 4, 64]
    const probe = Array.from({ length: 130 }, (_, code) => code)
    expect(probe.filter((code) => accepts(ExitCode, code))).toEqual(allowed)
    expect(accepts(ExitCode, "0")).toBe(false)
    expect(accepts(ExitCode, -1)).toBe(false)
    expect(accepts(ExitCode, 2.5)).toBe(false)
  })
})

describe("HistoryEvent", () => {
  const event = { id: "op-1", process: "stub", type: "invoke", f: "probe", value: null, t: 0 }

  test("accepts a well-formed event and rejects a bad process, kind, type, time or extra field", () => {
    expect(accepts(HistoryEvent, event)).toBe(true)
    expect(accepts(HistoryEvent, { ...event, process: "nobody" })).toBe(false)
    expect(accepts(HistoryEvent, { ...event, f: "nap" })).toBe(false)
    expect(accepts(HistoryEvent, { ...event, type: "maybe" })).toBe(false)
    expect(accepts(HistoryEvent, { ...event, t: -1 })).toBe(false)
    expect(accepts(HistoryEvent, { ...event, t: Number.POSITIVE_INFINITY })).toBe(false)
    expect(accepts(HistoryEvent, { ...event, extra: 1 })).toBe(false)
  })
})

describe("RunVerdict", () => {
  const verdict = {
    runId: "run-1",
    faultlineVersion: "0.0.0",
    git: { head: "unknown", dirty: "unknown" },
    bunVersion: "1.3.0",
    platform: "win32",
    lockStatus: "ok",
    exitCode: 1,
    worlds: [{ world: "world-1", verdicts: [{ _tag: "Violation", oracle: "tool-pairing", witness: ["op-2"], explanation: "x" }] }],
    violations: [{ world: "world-1", oracle: "tool-pairing", witness: ["op-2"], explanation: "x" }],
    inconclusive: [{ reason: "narrowed-run" }, { reason: "oracle-timeout", world: "world-1", oracle: "tool-pairing" }],
  }

  test("decodes a sample run verdict", () => {
    expect(accepts(RunVerdict, verdict)).toBe(true)
  })

  test("git.dirty is a boolean or the literal unknown, and nothing else", () => {
    expect(accepts(RunVerdict, { ...verdict, git: { head: "abc", dirty: true } })).toBe(true)
    expect(accepts(RunVerdict, { ...verdict, git: { head: "abc", dirty: "maybe" } })).toBe(false)
  })

  test("refuses an exit code outside the contract, an unknown lock status and a violation with no witness", () => {
    expect(accepts(RunVerdict, { ...verdict, exitCode: 5 })).toBe(false)
    expect(accepts(RunVerdict, { ...verdict, lockStatus: "fine" })).toBe(false)
    expect(accepts(RunVerdict, { ...verdict, violations: [{ world: "world-1", oracle: "tool-pairing", witness: [], explanation: "x" }] })).toBe(false)
  })

  test("refuses a verdict file with a missing required field or an unknown one", () => {
    const { exitCode: _omitted, ...withoutExit } = verdict
    expect(accepts(RunVerdict, withoutExit)).toBe(false)
    expect(accepts(RunVerdict, { ...verdict, note: "hand edited" })).toBe(false)
  })
})
