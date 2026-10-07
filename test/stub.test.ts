import { afterAll, describe, expect, test } from "bun:test"
import { mkdtemp, readFile, rm } from "node:fs/promises"
import { tmpdir } from "node:os"
import { join } from "node:path"
import { Effect, Layer, Result, Schema } from "effect"
import { ClockLive } from "../src/clock"
import { decodeHistory, makeRecorder } from "../src/history"
import { markerOf, promptTextOf, ProviderStub, ProviderStubLive, scriptDigestOf, toolCallIdOf, turnOf, type ScriptParams, type StubHandle } from "../src/provider/stub"
import { ProviderTurnValue, ToolCallValue, ToolResultValue, type HistoryEvent } from "../src/schema"
import { sessionId } from "./support/ids"

const dirs: Array<string> = []
afterAll(() => Promise.all(dirs.map((dir) => rm(dir, { recursive: true, force: true }))))

const script: ScriptParams = { seed: 20261006n, turnsPerSession: 3 }
const s1 = sessionId(1)

const freshDir = async (): Promise<string> => {
  const dir = await mkdtemp(join(tmpdir(), "fl-stub-"))
  dirs.push(dir)
  return dir
}

// Runs `body` against a live stub on an ephemeral port with a real recorder file, then closes the
// scope (stopping the server) and hands back what the body returned plus the history on disk.
const withStub = async <A>(body: (stub: StubHandle) => Promise<A>): Promise<{ readonly value: A; readonly history: string; readonly baseUrl: string }> => {
  const path = join(await freshDir(), "history.jsonl")
  return Effect.runPromise(
    Effect.scoped(
      Effect.gen(function* () {
        const provider = yield* ProviderStub
        const recorder = yield* makeRecorder(path)
        const stub = yield* provider.serve(script, recorder)
        const value = yield* Effect.promise(() => body(stub))
        return { value, baseUrl: stub.baseUrl }
      }),
    ).pipe(
      Effect.provide(Layer.mergeAll(ClockLive, ProviderStubLive)),
      Effect.flatMap((out) => Effect.map(Effect.promise(() => readFile(path, "utf8")), (history) => ({ ...out, history }))),
    ),
  )
}

const post = (baseUrl: string, body: unknown): Promise<{ readonly status: number; readonly text: string }> =>
  fetch(`${baseUrl}/v1/chat/completions`, { method: "POST", headers: { "content-type": "application/json" }, body: JSON.stringify(body) }).then((response) =>
    response.text().then((text) => ({ status: response.status, text })),
  )

const userMessage = { role: "user", content: promptTextOf(s1, script.seed) }

const afterTurn = (k: number) => {
  const id = toolCallIdOf(script.seed, s1, k) ?? "missing"
  return [
    { role: "assistant", content: null, tool_calls: [{ id, type: "function", function: { name: "append_line", arguments: JSON.stringify({ path: "log.txt", line: markerOf(s1, k) }) } }] },
    { role: "tool", tool_call_id: id, content: "ok" },
  ]
}

const framesOf = (k: number): string => {
  const turn = turnOf(script, s1, k)
  return turn._tag === "OutOfScript" ? "out of script" : turn.frames.join("")
}

const decoded = (text: string): ReadonlyArray<HistoryEvent> => Result.getOrThrow(decodeHistory(text))

const invokesOf = (history: ReadonlyArray<HistoryEvent>, f: HistoryEvent["f"]) => history.filter((event) => event.type === "invoke" && event.f === f)

describe("the provider script", () => {
  test("turns before the last call append_line once with the session's marker; the last turn stops; later turns are outside the script", () => {
    const first = turnOf(script, s1, 0)
    const last = turnOf(script, s1, 2)
    expect(first._tag).toBe("ToolCall")
    expect(first._tag === "ToolCall" ? first.marker : "").toBe("session-1:op-0")
    expect(first._tag === "ToolCall" ? first.frames.at(-1) : "").toBe("data: [DONE]\n\n")
    expect(last._tag).toBe("Stop")
    expect(turnOf(script, s1, 3)._tag).toBe("OutOfScript")
    expect(turnOf(script, s1, -1)._tag).toBe("OutOfScript")
  })

  test("tool-call ids depend on seed, session and turn and on nothing else", () => {
    expect(toolCallIdOf(1n, s1, 0)).toBe(toolCallIdOf(1n, s1, 0))
    const variants = [toolCallIdOf(1n, s1, 0), toolCallIdOf(2n, s1, 0), toolCallIdOf(1n, sessionId(2), 0), toolCallIdOf(1n, s1, 1)]
    expect(new Set(variants).size).toBe(4)
  })

  test("the script digest moves with the seed and the turn count and is stable otherwise", () => {
    const digest = (params: ScriptParams) => Result.getOrThrow(scriptDigestOf(params, [s1, sessionId(2)]))
    expect(digest(script)).toBe(digest({ ...script }))
    expect(digest({ ...script, seed: script.seed + 1n })).not.toBe(digest(script))
    expect(digest({ ...script, turnsPerSession: 4 })).not.toBe(digest(script))
  })
})

describe("the provider stub over HTTP", () => {
  test("the same request gets the same bytes, and those bytes are the script's frames", async () => {
    const out = await withStub(async (stub) => [await post(stub.baseUrl, { messages: [userMessage] }), await post(stub.baseUrl, { messages: [userMessage], model: "x" })])
    const [a, b] = out.value
    expect(a?.status).toBe(200)
    expect(a?.text).toBe(framesOf(0))
    expect(b?.text).toBe(a?.text)
  })

  test("k is the number of assistant messages in the request, and each role tool message is recorded as a result of that turn", async () => {
    const out = await withStub(async (stub) => post(stub.baseUrl, { messages: [userMessage, ...afterTurn(0), ...afterTurn(1)] }))
    expect(out.value.text).toBe(framesOf(2))
    const history = decoded(out.history)
    const turns = invokesOf(history, "provider-turn")
    const results = invokesOf(history, "tool-result")
    expect(turns).toHaveLength(1)
    const turn = Result.getOrThrow(Schema.decodeUnknownResult(ProviderTurnValue)(turns[0]?.value))
    expect(turn.k).toBe(2)
    expect(turn.echoed).toEqual([toolCallIdOf(script.seed, s1, 0), toolCallIdOf(script.seed, s1, 1)].map(String))
    const turnOp = String(turns[0]?.id)
    expect(
      results.map((event) => {
        const value = Result.getOrThrow(Schema.decodeUnknownResult(ToolResultValue)(event.value))
        return [value.session, value.reportedId, value.turnOp].map(String)
      }),
    ).toEqual([
      [s1, toolCallIdOf(script.seed, s1, 0), turnOp].map(String),
      [s1, toolCallIdOf(script.seed, s1, 1), turnOp].map(String),
    ])
    expect(invokesOf(history, "tool-call")).toHaveLength(0)
    expect(history.filter((event) => event.id === turns[0]?.id).map((event) => event.type)).toEqual(["invoke", "ok"])
  })

  test("a tool-call turn records the issued call, linked to its turn, and completes both once the stream is consumed", async () => {
    const out = await withStub(async (stub) => post(stub.baseUrl, { messages: [userMessage] }))
    const history = decoded(out.history)
    const call = invokesOf(history, "tool-call")[0]
    const value = Result.getOrThrow(Schema.decodeUnknownResult(ToolCallValue, { onExcessProperty: "error" })(call?.value))
    expect(String(value.toolCallId)).toBe(String(toolCallIdOf(script.seed, s1, 0)))
    expect(value.marker).toBe("session-1:op-0")
    expect(String(value.turnOp)).toBe(String(invokesOf(history, "provider-turn")[0]?.id))
    expect(history.map((event) => `${event.f}:${event.type}`)).toEqual(["provider-turn:invoke", "tool-call:invoke", "tool-call:ok", "provider-turn:ok"])
  })

  test("the completions are on disk by the time the client has read the end of the stream", async () => {
    const path = join(await freshDir(), "history.jsonl")
    const seen = await Effect.runPromise(
      Effect.scoped(
        Effect.gen(function* () {
          const provider = yield* ProviderStub
          const recorder = yield* makeRecorder(path)
          const stub = yield* provider.serve(script, recorder)
          // Read the file the moment the body ends, while the server is still up.
          return yield* Effect.promise(() => post(stub.baseUrl, { messages: [userMessage] }).then(() => readFile(path, "utf8")))
        }),
      ).pipe(Effect.provide(Layer.mergeAll(ClockLive, ProviderStubLive))),
    )
    expect(decoded(seen).map((event) => `${event.f}:${event.type}`)).toEqual(["provider-turn:invoke", "tool-call:invoke", "tool-call:ok", "provider-turn:ok"])
  })

  test("a request with no session tag is refused with 400, counted, and reported as a problem", async () => {
    const out = await withStub(async (stub) => {
      const response = await post(stub.baseUrl, { messages: [{ role: "user", content: "no tag here" }] })
      return { response, requests: await Effect.runPromise(stub.requests), problems: await Effect.runPromise(stub.problems) }
    })
    expect(out.value.response.status).toBe(400)
    expect(out.value.requests).toBe(1)
    expect(out.value.problems).toHaveLength(1)
    expect(out.history).toBe("")
  })

  test("a turn past the script is refused with 400, recorded as a failed provider turn, and reported as a problem", async () => {
    const out = await withStub(async (stub) => {
      const response = await post(stub.baseUrl, { messages: [userMessage, ...afterTurn(0), ...afterTurn(1), { role: "assistant", content: "done" }] })
      return { response, problems: await Effect.runPromise(stub.problems) }
    })
    expect(out.value.response.status).toBe(400)
    expect(out.value.problems).toHaveLength(1)
    expect(decoded(out.history).filter((event) => event.f === "provider-turn").map((event) => event.type)).toEqual(["invoke", "fail"])
  })

  test("a body that is not a chat request this stub reads is refused with 400", async () => {
    const out = await withStub(async (stub) => [await post(stub.baseUrl, { prompt: "x" }), await post(stub.baseUrl, { messages: [{ role: "robot", content: "x" }] })])
    expect(out.value.map((response) => response.status)).toEqual([400, 400])
  })

  test("a refused body and a request to any other route are not in the history, so each is reported as a problem", async () => {
    const out = await withStub(async (stub) => {
      const unreadable = await post(stub.baseUrl, { prompt: "x" })
      const elsewhere = await fetch(`${stub.baseUrl}/v1/embeddings`, { method: "POST", body: "{}" }).then((response) => response.text().then(() => response.status))
      const problems = await Effect.runPromise(stub.problems)
      return { statuses: [unreadable.status, elsewhere], problems }
    })
    expect(out.value.statuses).toEqual([400, 404])
    expect(out.value.problems).toHaveLength(2)
    expect(out.value.problems.join("\n")).toContain("/v1/embeddings")
    expect(out.history).toBe("")
  })

  test("the port is closed once the scope ends", async () => {
    const out = await withStub(async () => undefined)
    const after = await fetch(`${out.baseUrl}/v1/chat/completions`, { method: "POST", body: "{}" }).then(
      () => "answered",
      () => "refused",
    )
    expect(after).toBe("refused")
  })
})
