import { Context, Data, Effect, Layer, Ref, Result, Schema, Scope } from "effect"
import { canonicalJson, sha256Hex } from "../canonical"
import type { Recorder, RecorderError } from "../history"
import { ChatRequest, SessionId, ToolCallId, type ChatChunk, type ChatMessage, type OpId, type Sha256 } from "../schema"

// The scripted provider. It implements one subset of OpenAI-compatible
// POST /v1/chat/completions with stream: true: SSE frames `data: {json}\n\n` ending with
// `data: [DONE]\n\n`; choices[0].delta.content, choices[0].delta.tool_calls[] and
// choices[0].finish_reason ("tool_calls" | "stop"). It reads `messages` and `tools` and ignores
// every other request field. Nothing beyond that subset is claimed.

export type ScriptParams = { readonly seed: bigint; readonly turnsPerSession: number }

// ---- The script (pure) ------------------------------------------------------------------------
// Every byte below is a function of (seed, session, k, turnsPerSession). JSON.stringify is
// deterministic here because every object is built with a fixed key order in this file.

const SESSION_TAG = /^\[faultline (session-\d+)\]/

export const promptTextOf = (session: SessionId, seed: bigint): string => `[faultline ${session}] seed ${seed}: append one line per turn`

const hexOf = (text: string): string => new Bun.CryptoHasher("sha256").update(text).digest("hex")

// The pattern of ToolCallId is ^call_[0-9a-f]+$ and a SHA-256 hex prefix always matches it, so the
// decode cannot fail; returning undefined keeps that claim checked rather than assumed, and the
// caller treats undefined as a turn outside the script.
export const toolCallIdOf = (seed: bigint, session: SessionId, k: number): ToolCallId | undefined =>
  Result.getOrUndefined(Schema.decodeUnknownResult(ToolCallId)(`call_${hexOf(`${seed}\u0000${session}\u0000${k}`).slice(0, 16)}`))

export const markerOf = (session: SessionId, k: number): string => `${session}:op-${k}`

const frame = (chunk: ChatChunk): string => `data: ${JSON.stringify(chunk)}\n\n`

const split3 = (text: string): readonly [string, string, string] => {
  const a = Math.floor(text.length / 3)
  const b = Math.floor((2 * text.length) / 3)
  return [text.slice(0, a), text.slice(a, b), text.slice(b)]
}

export type Turn =
  | { readonly _tag: "ToolCall"; readonly toolCallId: ToolCallId; readonly marker: string; readonly frames: ReadonlyArray<string> }
  | { readonly _tag: "Stop"; readonly frames: ReadonlyArray<string> }
  | { readonly _tag: "OutOfScript" }

// Turn k < T-1 calls append_line once; turn T-1 answers with text and stops; anything later is
// outside the script.
export const turnOf = (script: ScriptParams, session: SessionId, k: number): Turn => {
  if (k >= script.turnsPerSession || k < 0) return { _tag: "OutOfScript" }
  if (k === script.turnsPerSession - 1) {
    const [p1, p2, p3] = split3(`${session} finished after ${k} tool calls.`)
    return {
      _tag: "Stop",
      frames: [
        frame({ choices: [{ index: 0, delta: { role: "assistant", content: p1 }, finish_reason: null }] }),
        frame({ choices: [{ index: 0, delta: { content: p2 }, finish_reason: null }] }),
        frame({ choices: [{ index: 0, delta: { content: p3 }, finish_reason: null }] }),
        frame({ choices: [{ index: 0, delta: {}, finish_reason: "stop" }] }),
        "data: [DONE]\n\n",
      ],
    }
  }
  const toolCallId = toolCallIdOf(script.seed, session, k)
  if (toolCallId === undefined) return { _tag: "OutOfScript" }
  const marker = markerOf(session, k)
  const [a1, a2, a3] = split3(JSON.stringify({ path: "log.txt", line: marker }))
  return {
    _tag: "ToolCall",
    toolCallId,
    marker,
    frames: [
      frame({
        choices: [
          { index: 0, delta: { role: "assistant", tool_calls: [{ index: 0, id: toolCallId, type: "function", function: { name: "append_line", arguments: a1 } }] }, finish_reason: null },
        ],
      }),
      frame({ choices: [{ index: 0, delta: { tool_calls: [{ index: 0, function: { arguments: a2 } }] }, finish_reason: null }] }),
      frame({ choices: [{ index: 0, delta: { tool_calls: [{ index: 0, function: { arguments: a3 } }] }, finish_reason: null }] }),
      frame({ choices: [{ index: 0, delta: {}, finish_reason: "tool_calls" }] }),
      "data: [DONE]\n\n",
    ],
  }
}

// SHA-256 over every frame of every turn of every session, in session then turn order. It changes
// whenever any byte the stub could send changes.
export const scriptDigestOf = (script: ScriptParams, sessions: ReadonlyArray<SessionId>): Result.Result<Sha256, unknown> =>
  sha256Hex(
    sessions
      .flatMap((session) =>
        Array.from({ length: script.turnsPerSession }, (_, k) => {
          const turn = turnOf(script, session, k)
          return turn._tag === "OutOfScript" ? `out-of-script:${session}:${k}\n` : turn.frames.join("")
        }),
      )
      .join(""),
  )

// ---- The server (effectful) -------------------------------------------------------------------

export class StubError extends Data.TaggedError("StubError")<{ readonly reason: "listen-failed"; readonly detail: string }> {}

export type StubHandle = {
  readonly baseUrl: string
  // Every POST to /v1/chat/completions, decodable or not.
  readonly requests: Effect.Effect<number>
  // Things the stub could not record or could not script. Any entry means the history is not a
  // full account of what crossed the provider boundary.
  readonly problems: Effect.Effect<ReadonlyArray<string>>
}

export class ProviderStub extends Context.Service<
  ProviderStub,
  { readonly serve: (script: ScriptParams, recorder: Recorder) => Effect.Effect<StubHandle, StubError, Scope.Scope> }
>()("faultline/ProviderStub") {}

const RawRequest = Schema.fromJsonString(Schema.Struct({ messages: Schema.Array(Schema.Json) }))
const decodeRaw = Schema.decodeUnknownResult(RawRequest)
const decodeTyped = Schema.decodeUnknownResult(Schema.fromJsonString(ChatRequest))
const decodeSession = Schema.decodeUnknownResult(SessionId)

const json = (status: number, body: Schema.Json): Response =>
  new Response(JSON.stringify(body), { status, headers: { "content-type": "application/json" } })

const sessionOf = (messages: ReadonlyArray<ChatMessage>): SessionId | undefined => {
  const first = messages.find((message) => message.role === "user")
  const tag = first === undefined || first.role !== "user" ? null : SESSION_TAG.exec(first.content)
  return tag === null ? undefined : Result.getOrUndefined(decodeSession(tag[1]))
}

const handleChat = (
  script: ScriptParams,
  recorder: Recorder,
  problems: Ref.Ref<ReadonlyArray<string>>,
  text: string,
): Effect.Effect<Response, RecorderError> =>
  Effect.gen(function* () {
    const raw = decodeRaw(text)
    const typed = decodeTyped(text)
    if (Result.isFailure(raw) || Result.isFailure(typed)) {
      yield* Ref.update(problems, (all) => [...all, "a request body this stub does not read"])
      return json(400, { error: "request is not a chat completion request this stub reads" })
    }
    const messages = typed.success.messages
    const session = sessionOf(messages)
    const turnKey = Result.getOrUndefined(Result.flatMap(canonicalJson(raw.success.messages), sha256Hex))
    if (session === undefined || turnKey === undefined) {
      yield* Ref.update(problems, (all) => [...all, "a request whose session could not be identified"])
      return json(400, { error: "no [faultline session-N] tag in the first user message" })
    }
    const k = messages.filter((message) => message.role === "assistant").length
    const echoed = messages.flatMap((message) => (message.role === "assistant" ? (message.tool_calls ?? []).map((call) => call.id) : []))
    const turnOp = yield* recorder.invoke("stub", "provider-turn", { session, k, turnKey, echoed: [...echoed] })
    yield* Effect.forEach(
      messages.flatMap((message) => (message.role === "tool" ? [message.tool_call_id] : [])),
      (reportedId) =>
        Effect.flatMap(recorder.invoke("stub", "tool-result", { session, reportedId, turnOp }), (id) =>
          recorder.complete(id, "stub", "ok", "tool-result", { session, reportedId, turnOp }),
        ),
      { discard: true },
    )
    const turn = turnOf(script, session, k)
    if (turn._tag === "OutOfScript") {
      yield* recorder.complete(turnOp, "stub", "fail", "provider-turn", { status: 400 })
      yield* Ref.update(problems, (all) => [...all, `${session} asked for turn ${k}, past the script's ${script.turnsPerSession} turns`])
      return json(400, { error: `turn ${k} is outside the script` })
    }
    const callOp: OpId | undefined =
      turn._tag === "ToolCall" ? yield* recorder.invoke("stub", "tool-call", { session, k, toolCallId: turn.toolCallId, marker: turn.marker, turnOp }) : undefined
    return sseResponse(turn.frames, recorder, problems, turnOp, callOp, turn._tag === "ToolCall" ? "tool_calls" : "stop")
  })

// The stream is pulled one frame at a time (highWaterMark 0), so `ok` is recorded only after the
// HTTP writer has asked for, and been given, every frame, and the stream is closed only after `ok`
// is on disk: a SUT that has seen the end of the stream cannot act before the record exists. A
// cancelled stream records `info`: the SUT may or may not have received the tool call.
const sseResponse = (
  frames: ReadonlyArray<string>,
  recorder: Recorder,
  problems: Ref.Ref<ReadonlyArray<string>>,
  turnOp: OpId,
  callOp: OpId | undefined,
  finish: "tool_calls" | "stop",
): Response => {
  const encoder = new TextEncoder()
  const iterator = frames[Symbol.iterator]()
  const settle = (type: "ok" | "info", value: Schema.Json): Promise<void> =>
    Effect.runPromise(
      Effect.gen(function* () {
        if (callOp !== undefined) yield* recorder.complete(callOp, "stub", type, "tool-call", value)
        yield* recorder.complete(turnOp, "stub", type, "provider-turn", type === "ok" ? { finish } : value)
      }).pipe(Effect.catchTag("RecorderError", (error) => Ref.update(problems, (all) => [...all, `could not record a stream outcome: ${error.detail}`]))),
    )
  const body = new ReadableStream<Uint8Array>(
    {
      pull: (controller) => {
        const next = iterator.next()
        if (next.done === true) return settle("ok", { frames: frames.length }).then(() => controller.close())
        controller.enqueue(encoder.encode(next.value))
        return undefined
      },
      cancel: () => settle("info", { cancelled: true }),
    },
    { highWaterMark: 0 },
  )
  return new Response(body, { status: 200, headers: { "content-type": "text/event-stream", "cache-control": "no-cache" } })
}

const serve = (script: ScriptParams, recorder: Recorder): Effect.Effect<StubHandle, StubError, Scope.Scope> =>
  Effect.gen(function* () {
    const requests = yield* Ref.make(0)
    const problems = yield* Ref.make<ReadonlyArray<string>>([])
    const respond = (request: Request): Promise<Response> =>
      Effect.runPromise(
        Effect.gen(function* () {
          const route = `${request.method} ${new URL(request.url).pathname}`
          // Traffic the stub refuses without recording is still traffic across the provider boundary;
          // it is reported so that the world is not judged on a history that omits it (DEF-003).
          if (route !== "POST /v1/chat/completions") return yield* Effect.as(Ref.update(problems, (all) => [...all, `${route}: not a route this stub serves`]), json(404, { error: "not found" }))
          yield* Ref.update(requests, (n) => n + 1)
          const text = yield* Effect.promise(() => request.text())
          return yield* handleChat(script, recorder, problems, text)
        }).pipe(
          Effect.catchTag("RecorderError", (error) =>
            Effect.as(Ref.update(problems, (all) => [...all, `could not record a request: ${error.detail}`]), json(500, { error: "stub could not record" })),
          ),
        ),
      )
    const server = yield* Effect.acquireRelease(
      Effect.try({
        try: () => Bun.serve({ port: 0, hostname: "127.0.0.1", fetch: respond }),
        catch: (error) => new StubError({ reason: "listen-failed", detail: String(error) }),
      }),
      (listening) => Effect.promise(() => listening.stop(true)),
    )
    if (server.port === undefined) return yield* Effect.fail(new StubError({ reason: "listen-failed", detail: "Bun.serve reported no port" }))
    return {
      baseUrl: `http://127.0.0.1:${server.port}`,
      requests: Ref.get(requests),
      problems: Ref.get(problems),
    }
  })

export const ProviderStubLive = Layer.succeed(ProviderStub, { serve })
