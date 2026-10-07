import { appendFile, writeFile } from "node:fs/promises"
import { Data, Effect, Ref, Result, Schema, Semaphore } from "effect"
import { Clock } from "./clock"
import { HistoryEvent, opIdOf, type HistoryProcess, type OpId, type OpKind } from "./schema"

export type History = ReadonlyArray<HistoryEvent>

export class HistoryError extends Data.TaggedError("HistoryError")<{
  // 1-based line number in the JSONL text, or 0 when the failure is not tied to a line.
  readonly line: number
  readonly reason:
    | "blank-line"
    | "invalid-line"
    | "time-regressed"
    | "duplicate-invoke"
    | "duplicate-completion"
    | "completion-without-invoke"
    | "completion-mismatch"
    | "encode-failed"
  readonly detail: string
}> {}

const lineCodec = Schema.fromJsonString(HistoryEvent)
// Excess properties are an error: a field the schema does not know about is evidence this decoder
// would otherwise drop without saying so.
const decodeLine = Schema.decodeUnknownResult(lineCodec, { onExcessProperty: "error" })
const encodeLine = Schema.encodeResult(lineCodec)

const failAt = (line: number, reason: HistoryError["reason"], detail: string): Result.Result<never, HistoryError> =>
  Result.fail(new HistoryError({ line, reason, detail }))

export const encodeHistoryLine = (event: HistoryEvent): Result.Result<string, HistoryError> =>
  Result.mapError(encodeLine(event), (error) => new HistoryError({ line: 0, reason: "encode-failed", detail: error.message }))

// One event per line, each line terminated by "\n". An empty history is the empty string.
export const encodeHistory = (history: History): Result.Result<string, HistoryError> =>
  Result.map(Result.all(history.map(encodeHistoryLine)), (lines) => lines.map((line) => `${line}\n`).join(""))

type Seen = { readonly invoke: HistoryEvent | undefined; readonly completed: boolean }
type Fold = {
  readonly events: ReadonlyArray<HistoryEvent>
  readonly lastT: number
  readonly seen: Readonly<Record<string, Seen>>
}

const decodeAt = (text: string, line: number): Result.Result<HistoryEvent, HistoryError> =>
  text === ""
    ? failAt(line, "blank-line", "a blank line inside the history")
    : Result.mapError(decodeLine(text), (error) => new HistoryError({ line, reason: "invalid-line", detail: error.message }))

// Decodes one line on its own, with no cross-line rules. The recorder and the tests use it where a
// single event is all there is.
export const decodeHistoryLine = (text: string): Result.Result<HistoryEvent, HistoryError> => decodeAt(text, 0)

// Structural rules, all about what the file says and none about what a run should do:
//  - t never decreases down the file;
//  - an id has at most one invoke and at most one completion (ok, fail or info);
//  - an invoke is the first event of its id;
//  - ok and fail complete an earlier invoke with the same process and f; info may do the same, or
//    stand alone as an event that has no invoke (a recorded fact such as a crash or a fault).
// The decoder cannot know whether an operation took effect; it only refuses files that are not
// shaped like a history, so an oracle never has to guess at one.
const step = (fold: Fold, event: HistoryEvent, line: number): Result.Result<Fold, HistoryError> => {
  if (event.t < fold.lastT) return failAt(line, "time-regressed", `t ${event.t} is before the previous event's t ${fold.lastT}`)
  const prior = fold.seen[event.id]
  const next = (seen: Seen): Result.Result<Fold, HistoryError> =>
    Result.succeed({ events: [...fold.events, event], lastT: event.t, seen: { ...fold.seen, [event.id]: seen } })
  const completes = (): Result.Result<Fold, HistoryError> => {
    if (prior === undefined || prior.invoke === undefined) return failAt(line, "completion-without-invoke", `${event.id} has no earlier invoke`)
    if (prior.completed) return failAt(line, "duplicate-completion", `${event.id} was already completed`)
    if (prior.invoke.f !== event.f || prior.invoke.process !== event.process)
      return failAt(line, "completion-mismatch", `${event.id} completes with a different process or f than its invoke`)
    return next({ invoke: prior.invoke, completed: true })
  }
  switch (event.type) {
    case "invoke":
      return prior === undefined ? next({ invoke: event, completed: false }) : failAt(line, "duplicate-invoke", `${event.id} already has an event`)
    case "ok":
    case "fail":
      return completes()
    case "info":
      return prior === undefined ? next({ invoke: undefined, completed: true }) : prior.invoke === undefined ? failAt(line, "duplicate-completion", `${event.id} was already completed`) : completes()
    default:
      return event.type satisfies never
  }
}

export const decodeHistory = (text: string): Result.Result<History, HistoryError> => {
  const lines = text.split("\n")
  const body = lines.at(-1) === "" ? lines.slice(0, -1) : lines
  return Result.map(
    body.reduce<Result.Result<Fold, HistoryError>>(
      (acc, line, index) => Result.flatMap(acc, (fold) => Result.flatMap(decodeAt(line, index + 1), (event) => step(fold, event, index + 1))),
      Result.succeed({ events: [], lastT: 0, seen: {} }),
    ),
    (fold) => fold.events,
  )
}

// ---- Recorder (effectful) --------------------------------------------------------------------

export class RecorderError extends Data.TaggedError("RecorderError")<{
  readonly reason: "write-failed" | "encode-failed" | "window-already-open"
  readonly detail: string
}> {}

export type Recorder = {
  readonly path: string
  // Moves the origin of `t` to now. Only allowed before the first event, so no recorded t is ever
  // re-based.
  readonly openWindow: Effect.Effect<void, RecorderError>
  readonly invoke: (process: HistoryProcess, f: OpKind, value: Schema.Json) => Effect.Effect<OpId, RecorderError>
  readonly complete: (id: OpId, process: HistoryProcess, type: "ok" | "fail" | "info", f: OpKind, value: Schema.Json) => Effect.Effect<void, RecorderError>
  readonly events: Effect.Effect<ReadonlyArray<HistoryEvent>>
}

type RecorderState = { readonly origin: number; readonly lastT: number; readonly nextId: number; readonly events: ReadonlyArray<HistoryEvent> }

// Appends one line per event to `path` (created empty here). Every append takes the single permit,
// reads the clock inside it, and writes before releasing, so the order of lines in the file is the
// order of ids, and t never decreases down the file even when fibers race. t is whole milliseconds
// since the window opened, clamped to the previous event's t.
export const makeRecorder = (path: string): Effect.Effect<Recorder, RecorderError, Clock> =>
  Effect.gen(function* () {
    const clock = yield* Clock
    const origin = yield* clock.monotonicMs
    const state = yield* Ref.make<RecorderState>({ origin, lastT: 0, nextId: 1, events: [] })
    const permit = yield* Semaphore.make(1)
    yield* Effect.tryPromise({
      try: () => writeFile(path, "", { flag: "wx" }),
      catch: (error) => new RecorderError({ reason: "write-failed", detail: `${path}: ${String(error)}` }),
    })

    const append = (build: (id: OpId) => Omit<HistoryEvent, "t">, reuse: OpId | undefined): Effect.Effect<OpId, RecorderError> =>
      Semaphore.withPermits(permit, 1)(
        Effect.gen(function* () {
          const now = yield* clock.monotonicMs
          const current = yield* Ref.get(state)
          const id = reuse ?? opIdOf(current.nextId)
          const t = Math.max(current.lastT, Math.floor(now - current.origin))
          const event: HistoryEvent = { ...build(id), t }
          const line = yield* Effect.fromResult(encodeHistoryLine(event))
          yield* Effect.tryPromise({
            try: () => appendFile(path, `${line}\n`),
            catch: (error) => new RecorderError({ reason: "write-failed", detail: `${path}: ${String(error)}` }),
          })
          yield* Ref.set(state, { origin: current.origin, lastT: t, nextId: reuse === undefined ? current.nextId + 1 : current.nextId, events: [...current.events, event] })
          return id
        }),
      ).pipe(Effect.mapError((error) => (error._tag === "HistoryError" ? new RecorderError({ reason: "encode-failed", detail: error.detail }) : error)))

    return {
      path,
      openWindow: Effect.gen(function* () {
        const now = yield* clock.monotonicMs
        const current = yield* Ref.get(state)
        if (current.events.length > 0) return yield* Effect.fail(new RecorderError({ reason: "window-already-open", detail: `${current.events.length} events already recorded` }))
        yield* Ref.set(state, { origin: now, lastT: 0, nextId: 1, events: [] })
      }),
      invoke: (process, f, value) => append((id) => ({ id, process, type: "invoke", f, value }), undefined),
      complete: (id, process, type, f, value) => Effect.asVoid(append(() => ({ id, process, type, f, value }), id)),
      events: Effect.map(Ref.get(state), (current) => current.events),
    }
  })
