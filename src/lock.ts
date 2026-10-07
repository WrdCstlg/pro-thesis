import { Data, Option, Result } from "effect"
import { digestOf } from "./canonical"
import { LOCK_KEYS, type FaultlineConfig, type LockEntries, type LockFile, type LockKey, type Sha256 } from "./schema"

export class LockDerivationError extends Data.TaggedError("LockDerivationError")<{
  readonly reason: "duplicate-external-oracle" | "missing-executable-digest" | "uncanonical"
  readonly detail: string
}> {}

export class LockRecordError extends Data.TaggedError("LockRecordError")<{ readonly reason: "empty-reason" }> {}

export type LockCheck =
  | { readonly _tag: "Ok" }
  | { readonly _tag: "Drift"; readonly moved: ReadonlyArray<LockKey> }

const compareNames = (a: string, b: string): number => (a < b ? -1 : a > b ? 1 : 0)

// What the lock binds for `oracles.external`: each oracle's name, its command, and the digest of its
// executable file. The list is sorted by name, so reordering the declarations does not move the lock;
// names are unique, so the sort is a total order.
const externalOracles = (
  config: FaultlineConfig,
  executables: Readonly<Record<string, Sha256>>,
): Result.Result<ReadonlyArray<{ readonly name: string; readonly cmd: ReadonlyArray<string>; readonly executable: Sha256 }>, LockDerivationError> => {
  const names = config.oracles.external.map((oracle) => oracle.name)
  if (new Set(names).size !== names.length)
    return Result.fail(new LockDerivationError({ reason: "duplicate-external-oracle", detail: `external oracle names must be unique: ${names.join(", ")}` }))
  return Result.map(
    Result.all(
      config.oracles.external.map((oracle) => {
        const executable = executables[oracle.name]
        return executable === undefined
          ? Result.fail(new LockDerivationError({ reason: "missing-executable-digest", detail: `no digest of the executable of external oracle ${oracle.name}` }))
          : Result.succeed({ name: oracle.name, cmd: oracle.cmd, executable })
      }),
    ),
    (entries) => entries.toSorted((a, b) => compareNames(a.name, b.name)),
  )
}

const digestKey = (key: LockKey, value: unknown): Result.Result<Sha256, LockDerivationError> =>
  Result.mapError(digestOf(value), (error) => new LockDerivationError({ reason: "uncanonical", detail: `${key}: ${error.reason} at ${error.path}` }))

// The seven locked keys and nothing else. `oracle.timeoutMs`, which the specification names but does
// not list among the locked keys, is deliberately not digested (OQ-007).
// `executables` maps an external oracle's name to the digest of its executable file; reading that
// file is the shell's job, this function only binds the digest it is given.
export const lockEntriesOf = (
  config: FaultlineConfig,
  executables: Readonly<Record<string, Sha256>>,
): Result.Result<LockEntries, LockDerivationError> =>
  Result.gen(function* () {
    const external = yield* externalOracles(config, executables)
    return {
      driver: yield* digestKey("driver", config.driver),
      profiles: yield* digestKey("profiles", config.profiles),
      "oracles.builtin": yield* digestKey("oracles.builtin", config.oracles.builtin),
      "oracles.external": yield* digestKey("oracles.external", external),
      "faults.allow": yield* digestKey("faults.allow", config.faults.allow),
      "faults.deny": yield* digestKey("faults.deny", config.faults.deny),
      budget: yield* digestKey("budget", config.budget),
    }
  })

// Keys come back in LOCK_KEYS order, so the report of what moved is itself deterministic.
export const checkLock = (lock: LockFile, derived: LockEntries): LockCheck => {
  const moved = LOCK_KEYS.filter((key) => lock.entries[key] !== derived[key])
  return moved.length === 0 ? { _tag: "Ok" } : { _tag: "Drift", moved }
}

// The only function that produces a new lock. History is append-only: the previous history is a
// prefix of the new one, and a key whose digest did not change adds no entry. Locking with nothing
// moved returns the previous file unchanged, and the reason is not recorded because nothing moved.
// `at` comes from the injected clock; this function reads no time.
export const recordLock = (input: {
  readonly previous: Option.Option<LockFile>
  readonly derived: LockEntries
  readonly reason: string
  readonly at: string
}): Result.Result<LockFile, LockRecordError> => {
  if (!/\S/.test(input.reason)) return Result.fail(new LockRecordError({ reason: "empty-reason" }))
  const before = Option.getOrUndefined(input.previous)
  const moved = LOCK_KEYS.flatMap((key) => {
    const from = before === undefined ? null : before.entries[key]
    return from === input.derived[key] ? [] : [{ at: input.at, key, from, to: input.derived[key], reason: input.reason }]
  })
  return Result.succeed({ version: 1, entries: input.derived, history: [...(before === undefined ? [] : before.history), ...moved] })
}
