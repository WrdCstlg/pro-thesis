import { Result, Schema } from "effect"
import type { History } from "../../history"
import {
  builtinOracleName,
  ProviderTurnValue,
  ToolCallValue,
  ToolResultValue,
  type HistoryEvent,
  type OpId,
  type OracleContext,
  type OracleVerdict,
} from "../../schema"

const ORACLE = builtinOracleName("tool-pairing")

type At<A> = { readonly index: number; readonly op: OpId; readonly value: A }

type Invokes = {
  readonly calls: ReadonlyArray<At<ToolCallValue>>
  readonly turns: ReadonlyArray<At<ProviderTurnValue>>
  readonly results: ReadonlyArray<At<ToolResultValue>>
}

const invokesOf = <A, I>(history: History, f: HistoryEvent["f"], schema: Schema.Codec<A, I>): Result.Result<ReadonlyArray<At<A>>, OpId> => {
  const decode = Schema.decodeUnknownResult(schema, { onExcessProperty: "error" })
  return Result.all(
    history.flatMap((event, index) =>
      event.type === "invoke" && event.f === f
        ? [Result.mapBoth(decode(event.value), { onFailure: () => event.id, onSuccess: (value) => ({ index, op: event.id, value }) })]
        : [],
    ),
  )
}

// Every value this oracle reads must decode, and every tool result must point at a provider turn
// that exists. A history that fails either is not something this oracle can judge, so it says
// Inconclusive instead of reading around the hole.
const decodeInvokes = (history: History): Result.Result<Invokes, OpId> =>
  Result.gen(function* () {
    const calls = yield* invokesOf(history, "tool-call", ToolCallValue)
    const turns = yield* invokesOf(history, "provider-turn", ProviderTurnValue)
    const results = yield* invokesOf(history, "tool-result", ToolResultValue)
    const orphan = results.find((result) => !turns.some((turn) => turn.op === result.value.turnOp && turn.index < result.index))
    if (orphan !== undefined) return yield* Result.fail(orphan.op)
    return { calls, turns, results }
  })

type Finding = { readonly witness: readonly [OpId, ...Array<OpId>]; readonly explanation: string }

// For one provider request: every id the stub issued earlier and the SUT echoed back in an
// assistant message must appear exactly once among that request's tool results, and no tool result
// may carry an id the stub never issued before the request.
const findingsOf = (invokes: Invokes, turn: At<ProviderTurnValue>): ReadonlyArray<Finding> => {
  const issuedBefore = invokes.calls.filter((call) => call.index < turn.index)
  const here = invokes.results.filter((result) => result.value.turnOp === turn.op)
  const foreign = here.flatMap((result): ReadonlyArray<Finding> =>
    issuedBefore.some((call) => call.value.toolCallId === result.value.reportedId)
      ? []
      : [
          {
            witness: [result.op, turn.op],
            explanation: `${turn.value.session} turn ${turn.value.k} reported a tool result for ${JSON.stringify(result.value.reportedId)}, which the stub never issued`,
          },
        ],
  )
  const unpaired = [...new Set(turn.value.echoed)].flatMap((echoedId): ReadonlyArray<Finding> => {
    const call = issuedBefore.find((candidate) => candidate.value.toolCallId === echoedId)
    if (call === undefined) return []
    const matching = here.filter((result) => result.value.reportedId === echoedId)
    return matching.length === 1
      ? []
      : [
          {
            witness: [call.op, turn.op, ...matching.map((result) => result.op)],
            explanation: `${turn.value.session} turn ${turn.value.k} echoed tool call ${echoedId} but reported ${matching.length} results for it, expected exactly 1`,
          },
        ]
  })
  return [...foreign, ...unpaired]
}

// Pure and synchronous. Reads only invoke values, which the stub records when a request arrives and
// which are therefore certain; an info completion changes none of them, so both branches of every
// info op give the same verdict and the oracle never has to split the history.
export const toolPairing = (history: History, _context: OracleContext): OracleVerdict =>
  Result.match(decodeInvokes(history), {
    onFailure: (): OracleVerdict => ({ _tag: "Inconclusive", oracle: ORACLE, reason: "history-malformed" }),
    onSuccess: (invokes): OracleVerdict => {
      const first = invokes.turns.flatMap((turn) => findingsOf(invokes, turn))[0]
      return first === undefined
        ? { _tag: "Ok", oracle: ORACLE }
        : { _tag: "Violation", oracle: ORACLE, witness: first.witness, explanation: first.explanation }
    },
  })
