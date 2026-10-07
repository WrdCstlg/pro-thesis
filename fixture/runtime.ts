import { Database } from "bun:sqlite"
import { appendFile, rename, writeFile } from "node:fs/promises"
import { join } from "node:path"
import { Data, Effect, Result, Schema } from "effect"
import { AppendLineArgs, ChatChunk, ChatMessage, FixtureDefect, PromptBody, type ChatToolCall } from "../src/schema"

// The reference agent runtime faultline is tested against. It is a system under test, not part of
// the harness: nothing here is evidence, and faultline reads none of its state. Durable state lives
// in one SQLite file: the inbox, each session's conversation, and tool results keyed by tool-call id.

const Env = Schema.Struct({
  PROVIDER_BASE_URL: Schema.String,
  FAULTLINE_WORKSPACE: Schema.String,
  FAULTLINE_STATE: Schema.String,
  FAULTLINE_PORT_FILE: Schema.String,
  FIXTURE_DEFECTS: Schema.optionalKey(Schema.String),
})
type Env = typeof Env.Type

// Defects this build contains. A defect that is requested but not built is refused at boot:
// running the clean code under a defect's name would let a defect row pass for the wrong reason.
const BUILT_DEFECTS: ReadonlyArray<FixtureDefect> = []
const MAX_TURNS = 64

class FixtureError extends Data.TaggedError("FixtureError")<{
  readonly reason: "boot" | "bad-request" | "conflict" | "provider" | "stream" | "tool" | "storage"
  readonly detail: string
}> {}

const TOOLS = [
  {
    type: "function",
    function: {
      name: "append_line",
      description: "Append one line to a file in the workspace.",
      parameters: { type: "object", properties: { path: { type: "string" }, line: { type: "string" } }, required: ["path", "line"] },
    },
  },
]

const decodeMessage = Schema.decodeUnknownResult(Schema.fromJsonString(ChatMessage))
const encodeMessage = Schema.encodeResult(Schema.fromJsonString(ChatMessage))
const decodeChunk = Schema.decodeUnknownResult(Schema.fromJsonString(ChatChunk))
const decodeArgs = Schema.decodeUnknownResult(Schema.fromJsonString(AppendLineArgs))
const decodePrompt = Schema.decodeUnknownResult(Schema.fromJsonString(PromptBody))

const storage = <A>(detail: string, run: () => A): Effect.Effect<A, FixtureError> =>
  Effect.try({ try: run, catch: (error) => new FixtureError({ reason: "storage", detail: `${detail}: ${String(error)}` }) })

const openDatabase = (path: string): Effect.Effect<Database, FixtureError> =>
  storage("open", () => {
    const db = new Database(path, { create: true, strict: true })
    db.run("PRAGMA journal_mode = WAL")
    db.run("CREATE TABLE IF NOT EXISTS inbox (session TEXT NOT NULL, message_id TEXT NOT NULL, text TEXT NOT NULL, PRIMARY KEY (session, message_id))")
    db.run("CREATE TABLE IF NOT EXISTS messages (session TEXT NOT NULL, seq INTEGER NOT NULL, body TEXT NOT NULL, PRIMARY KEY (session, seq))")
    db.run("CREATE TABLE IF NOT EXISTS tool_results (tool_call_id TEXT PRIMARY KEY, session TEXT NOT NULL, content TEXT NOT NULL)")
    return db
  })

const appendMessage = (db: Database, session: string, message: ChatMessage): Effect.Effect<void, FixtureError> =>
  Effect.flatMap(
    Effect.mapError(Effect.fromResult(encodeMessage(message)), (error) => new FixtureError({ reason: "storage", detail: error.message })),
    (body) =>
      storage("append message", () => {
        db.query("INSERT INTO messages (session, seq, body) SELECT $session, COALESCE(MAX(seq), -1) + 1, $body FROM messages WHERE session = $session").run({ session, body })
      }),
  )

const loadMessages = (db: Database, session: string): Effect.Effect<ReadonlyArray<ChatMessage>, FixtureError> =>
  Effect.flatMap(
    storage("load messages", () => db.query<{ body: string }, { session: string }>("SELECT body FROM messages WHERE session = $session ORDER BY seq").all({ session })),
    (rows) => Effect.mapError(Effect.fromResult(Result.all(rows.map((row) => decodeMessage(row.body)))), (error) => new FixtureError({ reason: "storage", detail: error.message })),
  )

// ---- Admission ----------------------------------------------------------------------------------
// An exact retry (same session, id and text) is the same message and is not processed again; the
// same id with different text is a conflict and is refused.

type Admission = "admitted" | "duplicate"

const admit = (db: Database, session: string, body: PromptBody): Effect.Effect<Admission, FixtureError> =>
  Effect.flatMap(
    storage("admit", () =>
      db.transaction((): Admission | "conflict" => {
        const existing = db.query<{ text: string }, { session: string; id: string }>("SELECT text FROM inbox WHERE session = $session AND message_id = $id").get({ session, id: body.messageId })
        if (existing !== null) return existing.text === body.text ? "duplicate" : "conflict"
        db.query("INSERT INTO inbox (session, message_id, text) VALUES ($session, $id, $text)").run({ session, id: body.messageId, text: body.text })
        return "admitted"
      })(),
    ),
    (outcome) => (outcome === "conflict" ? Effect.fail(new FixtureError({ reason: "conflict", detail: `message ${body.messageId} was admitted with different text` })) : Effect.succeed(outcome)),
  )

// ---- The provider stream ------------------------------------------------------------------------

type Assembled = { readonly content: string; readonly calls: ReadonlyArray<ChatToolCall>; readonly finish: "tool_calls" | "stop" | null; readonly done: boolean }

const withCall = (calls: ReadonlyArray<ChatToolCall>, index: number, update: (call: ChatToolCall) => ChatToolCall): ReadonlyArray<ChatToolCall> =>
  calls.length > index
    ? calls.map((call, at) => (at === index ? update(call) : call))
    : [...calls, update({ id: "", type: "function", function: { name: "", arguments: "" } })]

const assemble = (text: string): Result.Result<Assembled, FixtureError> =>
  text
    .split("\n\n")
    .filter((frame) => frame !== "")
    .reduce<Result.Result<Assembled, FixtureError>>(
      (acc, frame) =>
        Result.flatMap(acc, (state) => {
          if (state.done) return Result.fail(new FixtureError({ reason: "stream", detail: "frame after [DONE]" }))
          if (!frame.startsWith("data: ")) return Result.fail(new FixtureError({ reason: "stream", detail: `not an SSE data frame: ${frame.slice(0, 40)}` }))
          const payload = frame.slice("data: ".length)
          if (payload === "[DONE]") return Result.succeed({ ...state, done: true })
          return Result.mapBoth(decodeChunk(payload), {
            onFailure: (error) => new FixtureError({ reason: "stream", detail: error.message }),
            onSuccess: (chunk) => {
              const choice = chunk.choices[0]
              if (choice === undefined) return state
              return {
                content: state.content + (choice.delta.content ?? ""),
                calls: (choice.delta.tool_calls ?? []).reduce(
                  (calls, part) =>
                    withCall(calls, part.index, (call) => ({
                      id: part.id ?? call.id,
                      type: "function",
                      function: { name: call.function.name + (part.function?.name ?? ""), arguments: call.function.arguments + (part.function?.arguments ?? "") },
                    })),
                  state.calls,
                ),
                finish: choice.finish_reason ?? state.finish,
                done: false,
              }
            },
          })
        }),
      Result.succeed({ content: "", calls: [], finish: null, done: false }),
    )

const requestTurn = (env: Env, messages: ReadonlyArray<ChatMessage>): Effect.Effect<Assembled, FixtureError> =>
  Effect.gen(function* () {
    const response = yield* Effect.tryPromise({
      try: (signal) =>
        fetch(`${env.PROVIDER_BASE_URL}/v1/chat/completions`, {
          method: "POST",
          headers: { "content-type": "application/json" },
          body: JSON.stringify({ model: "faultline-stub", stream: true, messages, tools: TOOLS }),
          signal,
        }),
      catch: (error) => new FixtureError({ reason: "provider", detail: String(error) }),
    })
    if (!response.ok) return yield* Effect.fail(new FixtureError({ reason: "provider", detail: `provider answered ${response.status}` }))
    const text = yield* Effect.tryPromise({ try: () => response.text(), catch: (error) => new FixtureError({ reason: "stream", detail: String(error) }) })
    const assembled = yield* Effect.fromResult(assemble(text))
    if (!assembled.done || assembled.finish === null) return yield* Effect.fail(new FixtureError({ reason: "stream", detail: "stream ended without finish_reason and [DONE]" }))
    return assembled
  })

// ---- Tools ---------------------------------------------------------------------------------------

const runTool = (env: Env, db: Database, session: string, call: ChatToolCall): Effect.Effect<string, FixtureError> =>
  Effect.gen(function* () {
    const stored = yield* storage("look up result", () =>
      db.query<{ content: string }, { id: string }>("SELECT content FROM tool_results WHERE tool_call_id = $id").get({ id: call.id }),
    )
    if (stored !== null) return stored.content
    if (call.function.name !== "append_line") return yield* Effect.fail(new FixtureError({ reason: "tool", detail: `unknown tool ${call.function.name}` }))
    const args = yield* Effect.mapError(Effect.fromResult(decodeArgs(call.function.arguments)), (error) => new FixtureError({ reason: "tool", detail: error.message }))
    yield* Effect.tryPromise({
      try: () => appendFile(join(env.FAULTLINE_WORKSPACE, args.path), `${args.line}\n`),
      catch: (error) => new FixtureError({ reason: "tool", detail: String(error) }),
    })
    yield* storage("store result", () => {
      db.query("INSERT INTO tool_results (tool_call_id, session, content) VALUES ($id, $session, 'ok')").run({ id: call.id, session })
    })
    return "ok"
  })

// One provider turn, then recurse until the provider stops. Each step is persisted before the next
// request is built, so the conversation in SQLite is the conversation the provider saw.
const converse = (env: Env, db: Database, session: string, turns: number): Effect.Effect<number, FixtureError> =>
  Effect.gen(function* () {
    if (turns >= MAX_TURNS) return yield* Effect.fail(new FixtureError({ reason: "provider", detail: `no stop after ${MAX_TURNS} turns` }))
    const turn = yield* requestTurn(env, yield* loadMessages(db, session))
    if (turn.finish === "stop") {
      yield* appendMessage(db, session, { role: "assistant", content: turn.content })
      return turns + 1
    }
    yield* appendMessage(db, session, { role: "assistant", content: null, tool_calls: turn.calls })
    yield* Effect.forEach(
      turn.calls,
      (call) => Effect.flatMap(runTool(env, db, session, call), (content) => appendMessage(db, session, { role: "tool", tool_call_id: call.id, content })),
      { discard: true },
    )
    return yield* converse(env, db, session, turns + 1)
  })

// ---- HTTP ----------------------------------------------------------------------------------------

const reply = (status: number, body: Record<string, string | number>): Response =>
  new Response(JSON.stringify(body), { status, headers: { "content-type": "application/json" } })

const STATUS: Record<FixtureError["reason"], number> = { boot: 500, "bad-request": 400, conflict: 409, provider: 502, stream: 502, tool: 500, storage: 500 }

const prompt = (env: Env, db: Database, session: string, request: Request): Effect.Effect<Response, FixtureError> =>
  Effect.gen(function* () {
    const text = yield* Effect.tryPromise({ try: () => request.text(), catch: (error) => new FixtureError({ reason: "bad-request", detail: String(error) }) })
    const body = yield* Effect.mapError(Effect.fromResult(decodePrompt(text)), (error) => new FixtureError({ reason: "bad-request", detail: error.message }))
    const admission = yield* admit(db, session, body)
    if (admission === "duplicate") return reply(200, { status: "duplicate" })
    yield* appendMessage(db, session, { role: "user", content: body.text })
    const turns = yield* converse(env, db, session, 0)
    return reply(200, { status: "completed", turns })
  })

const route = (env: Env, db: Database) => (request: Request): Promise<Response> => {
  const url = new URL(request.url)
  const match = /^\/session\/([^/]+)\/prompt$/.exec(url.pathname)
  if (request.method === "GET" && url.pathname === "/health") return Promise.resolve(reply(200, { status: "ok" }))
  if (request.method !== "POST" || match === null || match[1] === undefined) return Promise.resolve(reply(404, { error: "not found" }))
  return Effect.runPromise(
    prompt(env, db, decodeURIComponent(match[1]), request).pipe(
      Effect.catchTag("FixtureError", (error) => Effect.succeed(reply(STATUS[error.reason], { error: error.reason, detail: error.detail }))),
    ),
  )
}

// ---- Boot ----------------------------------------------------------------------------------------

const requestedDefects = (env: Env): Effect.Effect<ReadonlyArray<FixtureDefect>, FixtureError> => {
  const names = (env.FIXTURE_DEFECTS ?? "").split(",").filter((name) => name !== "")
  return Effect.mapError(
    Effect.fromResult(Schema.decodeUnknownResult(Schema.Array(FixtureDefect))(names)),
    (error) => new FixtureError({ reason: "boot", detail: `FIXTURE_DEFECTS: ${error.message}` }),
  )
}

const main = Effect.gen(function* () {
  const env = yield* Effect.mapError(Effect.fromResult(Schema.decodeUnknownResult(Env)(process.env)), (error) => new FixtureError({ reason: "boot", detail: error.message }))
  const unbuilt = (yield* requestedDefects(env)).filter((defect) => !BUILT_DEFECTS.includes(defect))
  if (unbuilt.length > 0) return yield* Effect.fail(new FixtureError({ reason: "boot", detail: `defects requested but not built in this fixture: ${unbuilt.join(",")}` }))
  const db = yield* openDatabase(env.FAULTLINE_STATE)
  const server = yield* Effect.try({
    try: () => Bun.serve({ port: 0, hostname: "127.0.0.1", idleTimeout: 255, fetch: route(env, db) }),
    catch: (error) => new FixtureError({ reason: "boot", detail: String(error) }),
  })
  const staging = `${env.FAULTLINE_PORT_FILE}.partial`
  // Written then renamed, so the harness never reads half a file.
  yield* Effect.tryPromise({
    try: () => writeFile(staging, JSON.stringify({ port: server.port })).then(() => rename(staging, env.FAULTLINE_PORT_FILE)),
    catch: (error) => new FixtureError({ reason: "boot", detail: String(error) }),
  })
})

Effect.runFork(
  main.pipe(
    Effect.catchTag("FixtureError", (error) =>
      Effect.sync(() => {
        process.stderr.write(`fixture: ${error.reason}: ${error.detail}\n`)
        process.exit(3)
      }),
    ),
  ),
)
