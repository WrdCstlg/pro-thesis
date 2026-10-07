import { Data, Result, Schema } from "effect"
import { Fault, FaultKind, Trigger, type ScheduledFault } from "./schema"

export class ScheduleParseError extends Data.TaggedError("ScheduleParseError")<{
  readonly input: string
  readonly reason: "malformed" | "unknown-kind" | "invalid-params" | "invalid-trigger"
  readonly detail: string
}> {}

const fail = (input: string, reason: ScheduleParseError["reason"], detail: string): Result.Result<never, ScheduleParseError> =>
  Result.fail(new ScheduleParseError({ input, reason, detail }))

// Canonical decimal only: no sign, no leading zero, no exponent, no whitespace. That keeps
// format(parse(x)) a function of the value and rules out two spellings of one schedule.
const INTEGER = /^(0|[1-9][0-9]*)$/
const decimal = (text: string): number | undefined =>
  INTEGER.test(text) && Number.isSafeInteger(Number(text)) ? Number(text) : undefined

// Splits "a=1,b=2" into entries. A key without a value, an empty key, a repeated key and a value
// that is not a canonical decimal are all refused here, before Schema sees the params.
const entriesOf = (input: string, text: string): Result.Result<ReadonlyArray<readonly [string, number]>, ScheduleParseError> => {
  if (text === "") return Result.succeed([])
  const pairs = text.split(",").map((pair) => pair.split("="))
  const keys = pairs.map((pair) => pair[0])
  if (pairs.some((pair) => pair.length !== 2 || pair[0] === "" || pair[1] === ""))
    return fail(input, "malformed", `params must be key=value pairs separated by commas, got "${text}"`)
  if (new Set(keys).size !== keys.length) return fail(input, "malformed", `a param is repeated in "${text}"`)
  return Result.all(
    pairs.map(([key, value]) => {
      const parsed = decimal(value ?? "")
      return key === undefined || value === undefined || parsed === undefined
        ? fail(input, "invalid-params", `param ${key ?? ""}=${value ?? ""} is not a canonical non-negative decimal integer`)
        : Result.succeed([key, parsed] as const)
    }),
  )
}

const faultOf = (input: string, head: string): Result.Result<Fault, ScheduleParseError> => {
  const open = head.indexOf("(")
  const kind = open === -1 ? head : head.slice(0, open)
  const argText = open === -1 ? "" : head.slice(open)
  if (open !== -1 && !argText.endsWith(")")) return fail(input, "malformed", `unclosed "(" in "${head}"`)
  const inner = open === -1 ? "" : argText.slice(1, -1)
  if (inner.includes("(") || inner.includes(")")) return fail(input, "malformed", `nested parentheses in "${head}"`)
  if (!Schema.is(FaultKind)(kind)) return fail(input, "unknown-kind", `"${kind}" is not a fault kind`)
  return Result.flatMap(entriesOf(input, inner), (entries) =>
    entries.some(([key]) => key === "_tag")
      ? fail(input, "invalid-params", "_tag is not a param")
      : Result.mapError(
          Schema.decodeUnknownResult(Fault, { onExcessProperty: "error" })({ ...Object.fromEntries(entries), _tag: kind }),
          (error) => new ScheduleParseError({ input, reason: "invalid-params", detail: error.message }),
        ),
  )
}

const triggerOf = (input: string, text: string): Result.Result<Trigger, ScheduleParseError> => {
  const parts = text.split("=")
  const value = parts[1] === undefined ? undefined : decimal(parts[1])
  if (parts.length !== 2 || value === undefined) return fail(input, "invalid-trigger", `trigger must be t=<ms> or turn=<n>, got "${text}"`)
  if (parts[0] !== "t" && parts[0] !== "turn") return fail(input, "invalid-trigger", `unknown trigger "${parts[0] ?? ""}", expected t or turn`)
  return Result.mapError(
    Schema.decodeUnknownResult(Trigger, { onExcessProperty: "error" })(
      parts[0] === "t" ? { _tag: "AtTime", ms: value } : { _tag: "AtTurn", turn: value },
    ),
    (error) => new ScheduleParseError({ input, reason: "invalid-trigger", detail: error.message }),
  )
}

// <kind>(<k>=<v>,...)@<trigger>. Parentheses are optional for a kind with no params. There is no
// trimming: " stream.malformed@turn=1" is an unknown kind, not a convenience.
export const parseFault = (input: string): Result.Result<ScheduledFault, ScheduleParseError> => {
  const parts = input.split("@")
  const head = parts[0]
  const trigger = parts[1]
  if (parts.length !== 2 || head === undefined || trigger === undefined)
    return fail(input, "malformed", "expected exactly one @ between the fault and its trigger")
  return Result.flatMap(faultOf(input, head), (fault) => Result.map(triggerOf(input, trigger), (when) => ({ fault, trigger: when })))
}

export const parseFaults = (inputs: ReadonlyArray<string>): Result.Result<ReadonlyArray<ScheduledFault>, ScheduleParseError> =>
  Result.all(inputs.map(parseFault))

const paramsOf = (fault: Fault): ReadonlyArray<string> => {
  switch (fault._tag) {
    case "stream.disconnect":
      return [`afterChunks=${fault.afterChunks}`]
    case "stream.stall":
      return [`ms=${fault.ms}`]
    case "stream.http":
      return [`status=${fault.status}`, ...(fault.retryAfter === undefined ? [] : [`retryAfter=${fault.retryAfter}`])]
    case "runtime.crash":
      return [`restartAfter=${fault.restartAfter}`]
    case "stream.malformed":
    case "client.duplicate":
    case "client.conflict":
    case "client.interrupt":
      return []
    default:
      return fault satisfies never
  }
}

export const formatFault = (scheduled: ScheduledFault): string => {
  const params = paramsOf(scheduled.fault)
  const when = scheduled.trigger._tag === "AtTime" ? `t=${scheduled.trigger.ms}` : `turn=${scheduled.trigger.turn}`
  return `${scheduled.fault._tag}${params.length === 0 ? "" : `(${params.join(",")})`}@${when}`
}
