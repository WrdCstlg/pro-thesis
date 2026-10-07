import { Data, Result, Schema } from "effect"
import { HistoryEvent } from "./schema"

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
