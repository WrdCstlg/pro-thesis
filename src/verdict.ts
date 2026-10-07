import type {
  ExitCode,
  InconclusiveReason,
  InconclusiveRecord,
  LockStatus,
  OracleName,
  OracleVerdict,
  ViolationRecord,
  WorldId,
  WorldVerdicts,
} from "./schema"

// Guards the runner evaluates per world from facts it owns (the stub's realized fault times, the
// harness's load window, the stub's request count). The other vacuity guards are derived here.
export type WorldGuard = Extract<InconclusiveReason, "fault-missed-load" | "provider-not-exercised">

export type WorldOutcome = {
  readonly world: WorldId
  readonly reachedAssert: boolean
  readonly verdicts: ReadonlyArray<OracleVerdict>
  readonly guards: ReadonlyArray<WorldGuard>
}

export type RunFacts = {
  readonly lock: LockStatus
  readonly narrowed: boolean
  readonly interrupted: boolean
  readonly budgetExhausted: boolean
  readonly plannedWorlds: number
  readonly worlds: ReadonlyArray<WorldOutcome>
}

export type RunOutcome = {
  readonly exitCode: ExitCode
  readonly worlds: ReadonlyArray<WorldVerdicts>
  readonly violations: ReadonlyArray<ViolationRecord>
  readonly inconclusive: ReadonlyArray<InconclusiveRecord>
}

const record = (
  reason: InconclusiveReason,
  where: { readonly world?: WorldId; readonly oracle?: OracleName } = {},
): InconclusiveRecord => ({ reason, ...where })

const violationsOf = (outcome: WorldOutcome): ReadonlyArray<ViolationRecord> =>
  outcome.verdicts.flatMap((verdict) =>
    verdict._tag === "Violation"
      ? [{ world: outcome.world, oracle: verdict.oracle, witness: verdict.witness, explanation: verdict.explanation }]
      : [],
  )

// A world that never reached ASSERT is not also reported as unjudged: no oracle could have run.
const inconclusiveOf = (outcome: WorldOutcome): ReadonlyArray<InconclusiveRecord> => [
  ...(outcome.reachedAssert ? [] : [record("world-not-asserted", { world: outcome.world })]),
  ...(outcome.reachedAssert && outcome.verdicts.length === 0 ? [record("no-oracle-judged", { world: outcome.world })] : []),
  ...outcome.verdicts.flatMap((verdict) =>
    verdict._tag === "Inconclusive" ? [record(verdict.reason, { world: outcome.world, oracle: verdict.oracle })] : [],
  ),
  ...outcome.guards.map((guard) => record(guard, { world: outcome.world })),
]

// Worlds the plan called for that have no outcome. A plan that is not a positive safe integer, or an
// empty run, counts as one missing world so that "nothing ran" can never aggregate to PASS.
const missingWorlds = (facts: RunFacts): number =>
  Number.isSafeInteger(facts.plannedWorlds) && facts.plannedWorlds >= 1
    ? Math.max(facts.plannedWorlds - facts.worlds.length, facts.worlds.length === 0 ? 1 : 0)
    : 1

// Run-level precedence, in the order the exit-code contract states it:
// lock drift 4, any violation 1, budget exhausted 3, any inconclusive condition 2, otherwise 0.
// Exit 0 is reachable only when `inconclusive` is empty, so every way of not having checked
// something has to be an entry in that list. A bypassed lock is a narrowed run by definition.
export const aggregate = (facts: RunFacts): RunOutcome => {
  if (facts.lock === "drift") return { exitCode: 4, worlds: [], violations: [], inconclusive: [] }
  const violations = facts.worlds.flatMap(violationsOf)
  const inconclusive = [
    ...facts.worlds.flatMap(inconclusiveOf),
    ...Array.from({ length: missingWorlds(facts) }, () => record("world-not-asserted")),
    ...(facts.narrowed || facts.lock === "bypassed" ? [record("narrowed-run")] : []),
    ...(facts.interrupted ? [record("interrupted")] : []),
  ]
  return {
    exitCode: violations.length > 0 ? 1 : facts.budgetExhausted ? 3 : inconclusive.length > 0 ? 2 : 0,
    worlds: facts.worlds.map((outcome) => ({ world: outcome.world, verdicts: outcome.verdicts })),
    violations,
    inconclusive,
  }
}
