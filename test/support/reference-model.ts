import { Result, Schema } from "effect"
import { digestOf } from "../../src/canonical"
import { nextBelow, seedPrng, type Prng } from "../../src/prng"
import { MessageId, Sha256, SessionId, ToolCallId, type HistoryEvent, type OpKind, type OpType } from "../../src/schema"
import { opId, sessionId } from "./ids"

// A correct run of the tool loop, as the stub would record it, with sessions interleaved by the
// seeded PRNG. It exists so the oracle-soundness properties of M2 to M4 have a model to start from.
//
// The payload shapes below are PROVISIONAL (D-018): the specification types `value` as unknown and
// the oracles that read it do not exist yet. They move to schema.ts, with a ledger entry, when the
// first oracle needs them.
const Turn = Schema.Int.pipe(Schema.check(Schema.isGreaterThanOrEqualTo(0)))
export const PromptValue = Schema.Struct({ session: SessionId, messageId: MessageId, text: Schema.String })
export const ProviderTurnValue = Schema.Struct({ session: SessionId, k: Turn, turnKey: Sha256 })
export const ToolCallValue = Schema.Struct({ session: SessionId, k: Turn, toolCallId: ToolCallId, marker: Schema.String })
export const ToolResultValue = Schema.Struct({ session: SessionId, toolCallId: ToolCallId })

export type ReferenceParams = { readonly seed: bigint; readonly sessions: number; readonly turnsPerSession: number }

type Template = {
  readonly op: string
  readonly process: HistoryEvent["process"]
  readonly type: OpType
  readonly f: OpKind
  readonly value: Schema.Json
}
type Step = ReadonlyArray<Template>

const hex = (value: unknown, length: number): string => Result.getOrThrow(digestOf(value)).slice(0, length)

const pair = (op: string, process: HistoryEvent["process"], f: OpKind, invoke: Schema.Json, complete: Schema.Json): Step => [
  { op, process, type: "invoke", f, value: invoke },
  { op, process, type: "ok", f, value: complete },
]

const sessionSteps = (seed: bigint, index: number, turns: number): ReadonlyArray<Step> => {
  const session = sessionId(index)
  const at = (name: string) => `${session}/${name}`
  const callOf = (k: number) => `call_${hex({ seed: seed.toString(), session, k }, 16)}`
  const promptValue = { session, messageId: `msg-${session}-0`, text: `prompt for ${session}` }
  const turnSteps = Array.from({ length: turns }, (_, k): ReadonlyArray<Step> => {
    const turn = { session, k, turnKey: hex({ session, k }, 64) }
    const begin: Step = [
      { op: at(`turn-${k}`), process: "stub", type: "invoke", f: "provider-turn", value: turn },
      ...(k === 0 ? [] : pair(at(`result-${k - 1}`), "stub", "tool-result", { session, toolCallId: callOf(k - 1) }, { session, toolCallId: callOf(k - 1) })),
    ]
    const issue: ReadonlyArray<Step> =
      k < turns - 1
        ? [pair(at(`call-${k}`), "stub", "tool-call", { session, k, toolCallId: callOf(k), marker: `${session}:op-${k}` }, { session, k, toolCallId: callOf(k), marker: `${session}:op-${k}` })]
        : []
    const end: Step = [{ op: at(`turn-${k}`), process: "stub", type: "ok", f: "provider-turn", value: turn }]
    return [begin, ...issue, end]
  }).flat()
  return [pair(at("prompt"), session, "prompt", promptValue, promptValue), ...turnSteps]
}

// Picks a session that still has steps left, uniformly, until none do. A step is atomic, so the
// two halves of one operation can be separated by another session's steps but never reordered.
const interleave = (prng: Prng, queues: ReadonlyArray<ReadonlyArray<Step>>, out: ReadonlyArray<Template>): ReadonlyArray<Template> => {
  const live = queues.flatMap((queue, index) => (queue.length > 0 ? [index] : []))
  if (live.length === 0) return out
  const [pick, next] = Result.getOrThrow(nextBelow(prng, live.length))
  const chosen = live[pick] ?? 0
  return interleave(
    next,
    queues.map((queue, index) => (index === chosen ? queue.slice(1) : queue)),
    [...out, ...(queues[chosen]?.[0] ?? [])],
  )
}

export const referenceHistory = (params: ReferenceParams): ReadonlyArray<HistoryEvent> => {
  const prng = seedPrng(params.seed)
  const queues = Array.from({ length: params.sessions }, (_, index) => sessionSteps(params.seed, index + 1, params.turnsPerSession))
  const templates = interleave(prng, queues, [])
  const ops = Array.from(new Set(templates.map((template) => template.op)))
  // Each event is 0 to 4 ms after the one before it, so t is non-decreasing and has ties.
  const timed = templates.reduce<{ readonly t: number; readonly prng: Prng; readonly events: ReadonlyArray<HistoryEvent> }>(
    (acc, template) => {
      const [gap, next] = Result.getOrThrow(nextBelow(acc.prng, 5))
      const t = acc.t + gap
      return {
        t,
        prng: next,
        events: [...acc.events, { id: opId(ops.indexOf(template.op) + 1), process: template.process, type: template.type, f: template.f, value: template.value, t }],
      }
    },
    { t: 0, prng: seedPrng(params.seed ^ 0x5eedn), events: [] },
  )
  return timed.events
}
