import { describe, expect, test } from "bun:test"
import { Arbitrary, Option, Result, Schema } from "effect"
import { checkLock, lockEntriesOf, recordLock } from "../src/lock"
import { FaultlineConfig, LOCK_KEYS, LockFile, Profile, type LockEntries, type LockKey, type Sha256 } from "../src/schema"
import { sha256 } from "./support/ids"
import { jsonOf, reverseKeys } from "./support/json"
import { assertProperty, sampleOf } from "./support/property"

const decodeConfig = Schema.decodeUnknownResult(FaultlineConfig, { onExcessProperty: "error" })

const config: FaultlineConfig = Result.getOrThrow(
  decodeConfig({
    driver: { cmd: ["bun", "fixture/runtime.ts"], envAllow: ["PATH", "FIXTURE_DEFECTS"] },
    profiles: {
      baseline: { worlds: 3, sessions: 4, turnsPerSession: 6, minProviderTurns: 24, quiesceMs: 2000, faults: [], oracles: ["tool-pairing"] },
      disconnect: {
        worlds: 5,
        sessions: 4,
        turnsPerSession: 6,
        minProviderTurns: 24,
        quiesceMs: 2000,
        faults: ["stream.disconnect(afterChunks=2)@turn=2"],
        oracles: ["tool-pairing", "edit-integrity"],
      },
    },
    oracles: {
      builtin: ["tool-pairing", "edit-integrity"],
      external: [
        { name: "slow", cmd: ["bun", "oracles/slow.ts"] },
        { name: "fast", cmd: ["bun", "oracles/fast.ts"] },
      ],
    },
    oracle: { timeoutMs: 5000 },
    faults: { allow: ["stream.disconnect", "runtime.crash"], deny: ["client.conflict"] },
    budget: { wallClockMs: 600000 },
  }),
)

const executables: Readonly<Record<string, Sha256>> = { slow: sha256("a".repeat(64)), fast: sha256("b".repeat(64)) }
const AT = "2026-10-06T23:00:00.000Z"

const anotherProfile = Result.getOrThrow(
  Schema.decodeUnknownResult(Profile)({ worlds: 2, sessions: 1, turnsPerSession: 2, minProviderTurns: 1, quiesceMs: 100, faults: [], oracles: ["quiescence"] }),
)

const entriesOf = (cfg: FaultlineConfig, exes: Readonly<Record<string, Sha256>> = executables): LockEntries => Result.getOrThrow(lockEntriesOf(cfg, exes))
const lockOf = (entries: LockEntries): LockFile => Result.getOrThrow(recordLock({ previous: Option.none(), derived: entries, reason: "initial lock", at: AT }))
const movedBy = (cfg: FaultlineConfig, exes: Readonly<Record<string, Sha256>> = executables): ReadonlyArray<LockKey> => {
  const verdict = checkLock(lockOf(entriesOf(config)), entriesOf(cfg, exes))
  return verdict._tag === "Ok" ? [] : verdict.moved
}

const profilesWith = (change: (profile: FaultlineConfig["profiles"][string]) => FaultlineConfig["profiles"][string]): FaultlineConfig["profiles"] =>
  Object.fromEntries(Object.entries(config.profiles).map(([name, profile]) => [name, name === "disconnect" ? change(profile) : profile]))

describe("lockEntriesOf", () => {
  test("derives one 64-hex digest for each of the seven locked keys and no others", () => {
    const entries = entriesOf(config)
    expect(Object.keys(entries).toSorted()).toEqual([...LOCK_KEYS].toSorted())
    LOCK_KEYS.forEach((key) => expect(entries[key]).toMatch(/^[0-9a-f]{64}$/))
    expect(new Set(LOCK_KEYS.map((key) => entries[key])).size).toBe(LOCK_KEYS.length)
  })

  test("is the same when derived twice", () => {
    expect(entriesOf(config)).toEqual(entriesOf(config))
  })

  test("a change to any locked value moves exactly that key", () => {
    const edits: ReadonlyArray<readonly [string, LockKey, FaultlineConfig, Readonly<Record<string, Sha256>>?]> = [
      ["driver argument", "driver", { ...config, driver: { ...config.driver, cmd: [...config.driver.cmd, "--verbose"] } }],
      ["driver env allowlist", "driver", { ...config, driver: { ...config.driver, envAllow: [...config.driver.envAllow, "HOME"] } }],
      ["profile world count", "profiles", { ...config, profiles: profilesWith((profile) => ({ ...profile, worlds: profile.worlds + 1 })) }],
      ["profile fault schedule", "profiles", { ...config, profiles: profilesWith((profile) => ({ ...profile, faults: ["stream.disconnect(afterChunks=3)@turn=2"] })) }],
      ["profile oracle list", "profiles", { ...config, profiles: profilesWith((profile) => ({ ...profile, oracles: [] })) }],
      ["a new profile", "profiles", { ...config, profiles: { ...config.profiles, extra: anotherProfile } }],
      ["builtin oracle added", "oracles.builtin", { ...config, oracles: { ...config.oracles, builtin: [...config.oracles.builtin, "quiescence"] } }],
      ["builtin oracle removed", "oracles.builtin", { ...config, oracles: { ...config.oracles, builtin: ["tool-pairing"] } }],
      ["external command", "oracles.external", { ...config, oracles: { ...config.oracles, external: config.oracles.external.map((oracle) => (oracle.name === "slow" ? { ...oracle, cmd: ["bun", "oracles/slow2.ts"] } : oracle)) } }],
      ["external executable digest", "oracles.external", config, { ...executables, fast: sha256("c".repeat(64)) }],
      ["external oracle removed", "oracles.external", { ...config, oracles: { ...config.oracles, external: config.oracles.external.slice(1) } }, executables],
      ["faults.allow", "faults.allow", { ...config, faults: { ...config.faults, allow: [...config.faults.allow, "client.duplicate"] } }],
      ["faults.deny", "faults.deny", { ...config, faults: { ...config.faults, deny: [] } }],
      ["budget", "budget", { ...config, budget: { wallClockMs: config.budget.wallClockMs + 1 } }],
    ]
    expect(edits.map(([name, , edited, exes]) => [name, movedBy(edited, exes)])).toEqual(edits.map(([name, key]) => [name, [key]]))
  })

  test("oracle.timeoutMs is not among the locked keys, so changing it moves nothing (OQ-007)", () => {
    expect(movedBy({ ...config, oracle: { timeoutMs: config.oracle.timeoutMs * 10 } })).toEqual([])
  })

  test("declaration order of external oracles and key order inside the config do not move the lock", () => {
    const swapped: FaultlineConfig = { ...config, oracles: { ...config.oracles, external: config.oracles.external.toReversed() } }
    expect(entriesOf(swapped)).toEqual(entriesOf(config))
    const reordered = decodeConfig(reverseKeys(jsonOf(config)))
    expect(Result.isSuccess(reordered) ? entriesOf(reordered.success) : undefined).toEqual(entriesOf(config))
  })

  test("refuses duplicate external oracle names and an external oracle with no executable digest", () => {
    const duplicated: FaultlineConfig = { ...config, oracles: { ...config.oracles, external: [...config.oracles.external, ...config.oracles.external] } }
    const duplicate = lockEntriesOf(duplicated, executables)
    const missing = lockEntriesOf(config, { slow: sha256("a".repeat(64)) })
    expect(Result.isFailure(duplicate) ? duplicate.failure.reason : undefined).toBe("duplicate-external-oracle")
    expect(Result.isFailure(missing) ? missing.failure.reason : undefined).toBe("missing-executable-digest")
  })
})

describe("checkLock", () => {
  test("is Ok when every entry matches", () => {
    expect(checkLock(lockOf(entriesOf(config)), entriesOf(config))).toEqual({ _tag: "Ok" })
  })

  test("reports exactly the keys that moved, in LOCK_KEYS order, whatever order they were changed in", () => {
    const edited: FaultlineConfig = {
      ...config,
      budget: { wallClockMs: 1 },
      driver: { ...config.driver, cmd: ["bun", "other.ts"] },
      faults: { ...config.faults, deny: [] },
    }
    expect(checkLock(lockOf(entriesOf(config)), entriesOf(edited))).toEqual({ _tag: "Drift", moved: ["driver", "faults.deny", "budget"] })
  })
})

describe("recordLock", () => {
  const entries = entriesOf(config)

  test("the first lock records every key as moving from nothing, with the reason and the time given", () => {
    const first = Result.getOrThrow(recordLock({ previous: Option.none(), derived: entries, reason: "initial lock", at: AT }))
    expect(first.version).toBe(1)
    expect(first.entries).toEqual(entries)
    expect(first.history.map((entry) => [entry.key, entry.from, entry.to, entry.reason, entry.at])).toEqual(
      LOCK_KEYS.map((key) => [key, null, entries[key], "initial lock", AT]),
    )
  })

  test("a later lock appends one entry per moved key, from the old digest to the new, and leaves earlier history untouched", () => {
    const first = lockOf(entries)
    const moved = entriesOf({ ...config, budget: { wallClockMs: 1 } })
    const second = Result.getOrThrow(recordLock({ previous: Option.some(first), derived: moved, reason: "shorter budget", at: "2026-10-07T00:00:00.000Z" }))
    expect(second.history.slice(0, first.history.length)).toEqual([...first.history])
    expect(second.history.slice(first.history.length)).toEqual([
      { at: "2026-10-07T00:00:00.000Z", key: "budget", from: entries.budget, to: moved.budget, reason: "shorter budget" },
    ])
    expect(second.entries).toEqual(moved)
  })

  test("locking when nothing moved returns the previous file unchanged", () => {
    const first = lockOf(entries)
    expect(Result.getOrThrow(recordLock({ previous: Option.some(first), derived: entries, reason: "no change", at: "2026-10-07T00:00:00.000Z" }))).toEqual(first)
  })

  test("an empty or whitespace-only reason is refused", () => {
    const reasons = ["", " ", "\t\n  "].map((reason) => {
      const refused = recordLock({ previous: Option.none(), derived: entries, reason, at: AT })
      return Result.isFailure(refused) ? refused.failure.reason : "accepted"
    })
    expect(reasons).toEqual(["empty-reason", "empty-reason", "empty-reason"])
  })

  test("the file it produces decodes with the lock-file schema, and a reason of whitespace would not", () => {
    const decoded = Schema.decodeUnknownResult(LockFile, { onExcessProperty: "error" })(lockOf(entries))
    expect(Result.isSuccess(decoded)).toBe(true)
    const forged = { ...lockOf(entries), history: [{ at: AT, key: "budget", from: null, to: entries.budget, reason: "  " }] }
    expect(Result.isFailure(Schema.decodeUnknownResult(LockFile)(forged))).toBe(true)
  })
})

describe("lock properties", () => {
  const configArbitrary = Arbitrary.schema(FaultlineConfig)
  const digestsFor = (cfg: FaultlineConfig): Readonly<Record<string, Sha256>> =>
    Object.fromEntries(cfg.oracles.external.map((oracle) => [oracle.name, sha256("d".repeat(64))]))

  test("the generated configs are varied enough to mean something", async () => {
    const sample = await sampleOf(configArbitrary, 300)
    const derivable = sample.filter((cfg) => Result.isSuccess(lockEntriesOf(cfg, digestsFor(cfg))))
    expect(derivable.length).toBeGreaterThan(sample.length / 2)
    expect(sample.some((cfg) => Object.keys(cfg.profiles).length > 1)).toBe(true)
    expect(sample.some((cfg) => cfg.oracles.external.length > 1)).toBe(true)
  })

  test("the entries do not depend on key order anywhere in the config", async () => {
    await assertProperty("key order", configArbitrary, (cfg) => {
      const reordered = decodeConfig(reverseKeys(jsonOf(cfg)))
      return Result.isSuccess(reordered) && Bun.deepEquals(lockEntriesOf(reordered.success, digestsFor(cfg)), lockEntriesOf(cfg, digestsFor(cfg)), true)
    })
  })
})
