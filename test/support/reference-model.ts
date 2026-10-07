import { Result, Schema } from "effect"
import { digestOf } from "../../src/canonical"
import { nextBelow, seedPrng, type Prng } from "../../src/prng"
import type { HistoryEvent, OpId, OpKind, OpType } from "../../src/schema"
import { opId, sessionId } from "./ids"

// A correct run of the tool loop, recorded the way the stub and the runner record it (D-024), with
// sessions interleaved by the seeded PRNG. The oracle-soundness properties start from it.
//
// Turn k of a session: the stub records the provider-turn invoke with every tool-call id the
// request echoes (calls 0..k-1), then one tool-result per role "tool" message in the request
// (again calls 0..k-1, each linked to this turn's op), then for k < T-1 the tool-call it issues,
// then the completions. Values that link to another op name it, and names become op ids only after
// interleaving, because ids are assigned in order of first appearance.

export type ReferenceParams = { readonly seed: bigint; readonly sessions: number; readonly turnsPerSession: number }

type IdOf = (op: string) => OpId

type Template = {
  readonly op: string
  readonly process: HistoryEvent["process"]
  readonly type: OpType
  readonly f: OpKind
  readonly value: (idOf: IdOf) => Schema.Json
}
type Step = ReadonlyArray<Template>

const hex = (value: unknown, length: number): string => Result.getOrThrow(digestOf(value)).slice(0, length)

const fixed = (value: Schema.Json) => (): Schema.Json => value

const sessionSteps = (seed: bigint, index: number, turns: number): ReadonlyArray<Step> => {
  const session = sessionId(index)
  const at = (name: string) => `${session}/${name}`
  const callOf = (k: number) => `call_${hex({ seed: seed.toString(), session, k }, 16)}`
  const promptValue = { session, messageId: `msg-${session}-0`, text: `prompt for ${session}` }
  const prompt: Step = [{ op: at("prompt"), process: session, type: "invoke", f: "prompt", value: fixed(promptValue) }]
  const turnSteps = Array.from({ length: turns }, (_, k): ReadonlyArray<Step> => {
    const turnOp = at(`turn-${k}`)
    const prior = Array.from({ length: k }, (_, j) => callOf(j))
    const begin: Step = [
      { op: turnOp, process: "stub", type: "invoke", f: "provider-turn", value: fixed({ session, k, turnKey: hex({ session, k }, 64), echoed: prior }) },
    ]
    const results = prior.map((reportedId, j): Step => {
      const value = (idOf: IdOf): Schema.Json => ({ session, reportedId, turnOp: idOf(turnOp) })
      return [
        { op: at(`turn-${k}/result-${j}`), process: "stub", type: "invoke", f: "tool-result", value },
        { op: at(`turn-${k}/result-${j}`), process: "stub", type: "ok", f: "tool-result", value },
      ]
    })
    const issue: ReadonlyArray<Step> =
      k < turns - 1
        ? [
            [
              {
                op: at(`call-${k}`),
                process: "stub",
                type: "invoke",
                f: "tool-call",
                value: (idOf) => ({ session, k, toolCallId: callOf(k), marker: `${session}:op-${k}`, turnOp: idOf(turnOp) }),
              },
            ],
            [{ op: at(`call-${k}`), process: "stub", type: "ok", f: "tool-call", value: fixed({ frames: 5 }) }],
          ]
        : []
    const end: Step = [{ op: turnOp, process: "stub", type: "ok", f: "provider-turn", value: fixed({ finish: k < turns - 1 ? "tool_calls" : "stop" }) }]
    return [begin, ...results, ...issue, end]
  }).flat()
  const answered: Step = [{ op: at("prompt"), process: session, type: "ok", f: "prompt", value: fixed({ status: 200 }) }]
  return [prompt, ...turnSteps, answered]
}

// Picks a session that still has steps left, uniformly, until none do. A step is atomic, so the
// events of one session keep their order while other sessions' steps fall between them.
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
  const idOf: IdOf = (op) => opId(ops.indexOf(op) + 1)
  // Each event is 0 to 4 ms after the one before it, so t is non-decreasing and has ties.
  const timed = templates.reduce<{ readonly t: number; readonly prng: Prng; readonly events: ReadonlyArray<HistoryEvent> }>(
    (acc, template) => {
      const [gap, next] = Result.getOrThrow(nextBelow(acc.prng, 5))
      const t = acc.t + gap
      return {
        t,
        prng: next,
        events: [...acc.events, { id: idOf(template.op), process: template.process, type: template.type, f: template.f, value: template.value(idOf), t }],
      }
    },
    { t: 0, prng: seedPrng(params.seed ^ 0x5eedn), events: [] },
  )
  return timed.events
}
