import { existsSync } from "node:fs"
import { mkdir, readFile, rename, writeFile } from "node:fs/promises"
import { tmpdir } from "node:os"
import { join } from "node:path"
import { Data, Effect, Layer, Option, Result, Schema } from "effect"
import { Clock, ClockLive } from "./clock"
import { checkLock, lockEntriesOf, recordLock } from "./lock"
import { toolPairing } from "./oracle/builtin/tool-pairing"
import { nextU64, seedPrng } from "./prng"
import { ProviderStubLive } from "./provider/stub"
import { ProcessControlLive, runWorld, workspaceLive, type BuiltinOracle } from "./runner"
import { parseFaults } from "./schedule"
import {
  builtinOracleName,
  FaultlineConfig,
  LockFile,
  LOCK_KEYS,
  RunId,
  RunVerdict,
  Seed,
  Sha256,
  worldIdOf,
  type BuiltinOracleName,
  type ExitCode,
  type LockEntries,
  type LockKey,
  type OracleVerdict,
  type Profile,
} from "./schema"
import { aggregate } from "./verdict"

// The command line. The only module that writes to stdout or stderr, and the only one that turns
// an outcome into a process exit code.
//
// Built so far: `run --profile <p> [--seed <n>]` and `lock --reason "<text>"` (D-022). The other
// flags and subcommands in the specification are refused with 64 until the milestone that builds
// them; a refusal is not a verdict.

class CliError extends Data.TaggedError("CliError")<{ readonly exit: 2 | 4 | 64; readonly detail: string }> {}

const usage = (detail: string): CliError => new CliError({ exit: 64, detail })
const harness = (detail: string): CliError => new CliError({ exit: 2, detail })

const io = <A>(make: (detail: string) => CliError, detail: string, run: () => PromiseLike<A>): Effect.Effect<A, CliError> =>
  Effect.tryPromise({ try: run, catch: (error) => make(`${detail}: ${String(error)}`) })

const write = (stream: NodeJS.WriteStream, text: string): Effect.Effect<void> =>
  Effect.callback<void>((resume) => {
    stream.write(text, () => resume(Effect.void))
  })

// ---- Arguments --------------------------------------------------------------------------------

type Command =
  | { readonly _tag: "Run"; readonly profile: string; readonly seed: string | undefined }
  | { readonly _tag: "Lock"; readonly reason: string }

const NOT_BUILT: Readonly<Record<string, string>> = {
  "--worlds": "M5",
  "--budget": "M5",
  "--fault": "M3",
}

const flagsOf = (
  tokens: ReadonlyArray<string>,
  allowed: ReadonlyArray<string>,
  found: Readonly<Record<string, string>>,
): Result.Result<Readonly<Record<string, string>>, CliError> => {
  const [flag, value, ...rest] = tokens
  if (flag === undefined) return Result.succeed(found)
  const later = NOT_BUILT[flag]
  if (later !== undefined) return Result.fail(usage(`${flag} is not built in this milestone (arrives in ${later})`))
  if (!allowed.includes(flag)) return Result.fail(usage(`unknown argument ${flag}`))
  if (value === undefined) return Result.fail(usage(`${flag} needs a value`))
  if (found[flag] !== undefined) return Result.fail(usage(`${flag} given twice`))
  return flagsOf(rest, allowed, { ...found, [flag]: value })
}

const parseArgs = (argv: ReadonlyArray<string>): Result.Result<Command, CliError> => {
  const [command, ...rest] = argv
  if (command === "run")
    return Result.flatMap(flagsOf(rest, ["--profile", "--seed"], {}), (flags) => {
      const profile = flags["--profile"]
      return profile === undefined ? Result.fail(usage("run needs --profile <name>")) : Result.succeed({ _tag: "Run", profile, seed: flags["--seed"] })
    })
  if (command === "lock" && rest[0] === "verify") return Result.fail(usage("lock verify is not built in this milestone (arrives in M5)"))
  if (command === "lock")
    return Result.flatMap(flagsOf(rest, ["--reason"], {}), (flags) => {
      const reason = flags["--reason"]
      return reason === undefined || !/\S/.test(reason) ? Result.fail(usage("lock needs a non-empty --reason")) : Result.succeed({ _tag: "Lock", reason })
    })
  if (command === "replay" || command === "explain") return Result.fail(usage(`${command} is not built in this milestone (arrives in M5)`))
  return Result.fail(usage("usage: faultline run --profile <name> [--seed <n>] | faultline lock --reason <text>"))
}

// ---- Config and lock --------------------------------------------------------------------------

// Excess keys are an error (OQ-008): a misspelt key would otherwise be dropped without a word, and
// a profile could lose a field the author believes is set.
const decodeConfig = Schema.decodeUnknownResult(Schema.fromJsonString(FaultlineConfig), { onExcessProperty: "error" })
const decodeLock = Schema.decodeUnknownResult(Schema.fromJsonString(LockFile), { onExcessProperty: "error" })
const decodeSeed = Schema.decodeUnknownResult(Seed)
const decodeRunId = Schema.decodeUnknownResult(RunId)
const decodeSha256 = Schema.decodeUnknownResult(Sha256)
const decodeVersion = Schema.decodeUnknownResult(Schema.fromJsonString(Schema.Struct({ version: Schema.String })))

const loadConfig = (cwd: string): Effect.Effect<FaultlineConfig, CliError> =>
  Effect.flatMap(io(usage, "read faultline.json", () => readFile(join(cwd, "faultline.json"), "utf8")), (text) =>
    Effect.mapError(Effect.fromResult(decodeConfig(text)), (error) => usage(`faultline.json: ${error.message}`)),
  )

// The digest of each external oracle's executable file, cmd[0] read relative to the project.
const executableDigests = (config: FaultlineConfig, cwd: string): Effect.Effect<Readonly<Record<string, Sha256>>, CliError> =>
  Effect.map(
    Effect.forEach(config.oracles.external, (oracle) =>
      Effect.flatMap(io(usage, `read the executable of external oracle ${oracle.name}`, () => readFile(join(cwd, oracle.cmd[0]))), (bytes) =>
        Effect.mapError(
          Effect.fromResult(decodeSha256(new Bun.CryptoHasher("sha256").update(bytes).digest("hex"))),
          (error) => harness(`digest of ${oracle.name}: ${error.message}`),
        ).pipe(Effect.map((digest) => [oracle.name, digest] as const)),
      ),
    ),
    (pairs) => Object.fromEntries(pairs),
  )

const deriveEntries = (config: FaultlineConfig, cwd: string): Effect.Effect<LockEntries, CliError> =>
  Effect.flatMap(executableDigests(config, cwd), (executables) =>
    Effect.mapError(Effect.fromResult(lockEntriesOf(config, executables)), (error) => usage(`cannot derive the lock: ${error.detail}`)),
  )

type LockState = { readonly _tag: "Missing" } | { readonly _tag: "Corrupt"; readonly detail: string } | { readonly _tag: "Present"; readonly lock: LockFile }

const lockPath = (cwd: string): string => join(cwd, ".faultline", "lock")

const readLock = (cwd: string): Effect.Effect<LockState, CliError> =>
  existsSync(lockPath(cwd))
    ? Effect.map(io(harness, "read .faultline/lock", () => readFile(lockPath(cwd), "utf8")), (text) =>
        Result.match(decodeLock(text), {
          onFailure: (error): LockState => ({ _tag: "Corrupt", detail: error.message }),
          onSuccess: (lock): LockState => ({ _tag: "Present", lock }),
        }),
      )
    : Effect.succeed({ _tag: "Missing" })

// A missing or unreadable lock binds nothing, so every key counts as moved.
const movedKeys = (state: LockState, derived: LockEntries): ReadonlyArray<LockKey> => {
  if (state._tag !== "Present") return LOCK_KEYS
  const check = checkLock(state.lock, derived)
  return check._tag === "Ok" ? [] : check.moved
}

// ---- Profile validation -----------------------------------------------------------------------

const BUILT_ORACLES: Partial<Record<BuiltinOracleName, BuiltinOracle["judge"]>> = { "tool-pairing": toolPairing }

// Every oracle a profile names must be declared in the config and built in this binary. A name that
// fails either is refused before anything runs; it is never skipped, because a skipped oracle is a
// narrowed run that nobody asked for (OQ-010).
const oraclesOf = (config: FaultlineConfig, profile: Profile): Effect.Effect<ReadonlyArray<BuiltinOracle>, CliError> =>
  Effect.forEach(profile.oracles, (name) => {
    const builtin = config.oracles.builtin.find((declared) => declared === name)
    if (builtin === undefined)
      return config.oracles.external.some((declared) => declared.name === name)
        ? Effect.fail(usage(`external oracle ${name}: the external oracle runner is not built in this milestone (arrives in M4)`))
        : Effect.fail(usage(`oracle ${name} is named by the profile but not declared under oracles`))
    const judge = BUILT_ORACLES[builtin]
    return judge === undefined
      ? Effect.fail(usage(`built-in oracle ${name} is not built in this milestone`))
      : Effect.succeed({ name: builtinOracleName(builtin), judge })
  })

// Fault entries are parsed so a malformed one is a usage error (OQ-009). This build injects no
// faults, so a profile that plans any is refused rather than run without them (OQ-010).
const requireNoFaults = (profile: Profile): Effect.Effect<void, CliError> =>
  Result.match(parseFaults(profile.faults), {
    onFailure: (error) => Effect.fail(usage(`profile fault: ${error.message === "" ? error._tag : error.message}`)),
    onSuccess: (planned) => (planned.length === 0 ? Effect.void : Effect.fail(usage("this profile plans faults; fault injection is not built in this milestone (arrives in M3)"))),
  })

// ---- Run --------------------------------------------------------------------------------------

const sourceRoot = join(import.meta.dir, "..")

const gitOf = (args: ReadonlyArray<string>): Effect.Effect<string | undefined> =>
  Effect.map(
    Effect.result(Effect.try({ try: () => Bun.spawnSync(["git", ...args], { cwd: sourceRoot, stdout: "pipe", stderr: "pipe" }), catch: () => "unavailable" })),
    (outcome) => (Result.isSuccess(outcome) && outcome.success.exitCode === 0 ? outcome.success.stdout.toString() : undefined),
  )

// Read, never invented: each field is the literal "unknown" when it cannot be read.
const provenance = Effect.gen(function* () {
  const packageText = yield* Effect.result(io(harness, "read package.json", () => readFile(join(sourceRoot, "package.json"), "utf8")))
  const version = Result.isSuccess(packageText) ? Result.getOrUndefined(decodeVersion(packageText.success)) : undefined
  const head = yield* gitOf(["rev-parse", "HEAD"])
  const status = yield* gitOf(["status", "--porcelain"])
  return {
    faultlineVersion: version === undefined ? "unknown" : version.version,
    git: { head: head === undefined ? "unknown" : head.trim(), dirty: status === undefined ? ("unknown" as const) : status.trim() !== "" },
    bunVersion: Bun.version,
    platform: process.platform,
  }
})

const writeVerdict = (runDir: string, verdict: RunVerdict): Effect.Effect<string, CliError> =>
  Effect.gen(function* () {
    const encoded = yield* Effect.mapError(Effect.fromResult(Schema.encodeResult(RunVerdict)(verdict)), (error) => harness(`encode verdict.json: ${error.message}`))
    const path = join(runDir, "verdict.json")
    yield* io(harness, `write ${path}`, () => writeFile(path, `${JSON.stringify(encoded, null, 2)}\n`))
    return path
  })

// Each world's seed is the next splitmix64 output after the run seed, so a run seed fixes every
// world seed.
const worldSeedsOf = (runSeed: Seed, count: number): ReadonlyArray<string> =>
  Array.from({ length: count }).reduce<{ readonly prng: ReturnType<typeof seedPrng>; readonly seeds: ReadonlyArray<string> }>(
    (acc) => {
      const [value, next] = nextU64(acc.prng)
      return { prng: next, seeds: [...acc.seeds, value.toString()] }
    },
    { prng: seedPrng(BigInt(runSeed)), seeds: [] },
  ).seeds

const exitName: Readonly<Record<ExitCode, string>> = { 0: "PASS", 1: "FAIL", 2: "INCONCLUSIVE", 3: "BUDGET_EXHAUSTED", 4: "LOCK_DRIFT", 64: "USAGE" }

const verdictLine = (verdict: OracleVerdict): string => {
  switch (verdict._tag) {
    case "Ok":
      return `${verdict.oracle}: Ok`
    case "Violation":
      return `${verdict.oracle}: Violation [${verdict.witness.join(", ")}] ${verdict.explanation}`
    case "Inconclusive":
      return `${verdict.oracle}: Inconclusive (${verdict.reason})`
    default:
      return verdict satisfies never
  }
}

const run = (command: Extract<Command, { readonly _tag: "Run" }>, cwd: string): Effect.Effect<ExitCode, CliError, Clock> =>
  Effect.gen(function* () {
    const clock = yield* Clock
    const config = yield* loadConfig(cwd)
    const profile = config.profiles[command.profile]
    if (profile === undefined) return yield* Effect.fail(usage(`no profile ${command.profile} in faultline.json`))
    const runSeed = yield* Effect.mapError(Effect.fromResult(decodeSeed(command.seed ?? String(yield* clock.wallMs))), () =>
      usage(`--seed must be a decimal integer from 0 to 2^64 - 1, got ${command.seed ?? ""}`),
    )
    const oracles = yield* oraclesOf(config, profile)
    yield* requireNoFaults(profile)
    const derived = yield* deriveEntries(config, cwd)
    const lock = yield* readLock(cwd)
    const wall = yield* clock.wallMs
    const runId = yield* Effect.mapError(Effect.fromResult(decodeRunId(`run-${wall.toString(36)}-${process.pid.toString(36)}`)), (error) => harness(`run id: ${error.message}`))
    const runDir = join(cwd, ".faultline", "runs", runId)
    yield* io(harness, `create ${runDir}`, () => mkdir(runDir, { recursive: true }))
    const facts = yield* provenance
    const moved = movedKeys(lock, derived)
    // Lock drift is decided here, before any process is spawned or any port opened. The report
    // names the moved keys and no other lock key.
    if (moved.length > 0) {
      const path = yield* writeVerdict(runDir, { runId, ...facts, lockStatus: "drift", exitCode: 4, worlds: [], violations: [], inconclusive: [] })
      yield* write(process.stderr, `LOCK_DRIFT: moved without a recorded reason: ${moved.join(", ")}\n`)
      yield* write(process.stdout, `faultline: exit 4 (LOCK_DRIFT); verdict ${path}\n`)
      return 4
    }
    const seeds = worldSeedsOf(runSeed, profile.worlds)
    const results = yield* Effect.forEach(
      seeds,
      (seed, index) =>
        Effect.flatMap(Effect.fromResult(decodeSeed(seed)), (worldSeed) =>
          runWorld({
            world: worldIdOf(index + 1),
            dir: join(runDir, "worlds", worldIdOf(index + 1)),
            seed: worldSeed,
            profileName: command.profile,
            profile,
            driver: config.driver,
            projectDir: cwd,
            inheritedEnv: process.env,
            oracles,
          }),
        ).pipe(Effect.mapError((error) => harness(`world seed: ${error.message}`))),
      { concurrency: 1 },
    ).pipe(Effect.provide(Layer.mergeAll(workspaceLive(tmpdir()), ProcessControlLive, ProviderStubLive)))
    const outcome = aggregate({
      lock: "ok",
      narrowed: false,
      interrupted: false,
      budgetExhausted: false,
      plannedWorlds: profile.worlds,
      worlds: results.map((result) => result.outcome),
    })
    const path = yield* writeVerdict(runDir, { runId, ...facts, lockStatus: "ok", ...outcome })
    const lines = [
      ...results.flatMap((result) => [
        `${result.outcome.world}: ${result.outcome.reachedAssert ? "reached ASSERT" : "did not reach ASSERT"}`,
        ...result.outcome.verdicts.map((verdict) => `  ${verdictLine(verdict)}`),
        ...result.problems.map((problem) => `  problem: ${problem}`),
      ]),
      ...outcome.inconclusive.map((entry) => `inconclusive: ${entry.reason}${entry.world === undefined ? "" : ` (${entry.world})`}`),
      `faultline: exit ${outcome.exitCode} (${exitName[outcome.exitCode]}); seed ${runSeed}; verdict ${path}`,
    ]
    yield* write(process.stdout, `${lines.join("\n")}\n`)
    return outcome.exitCode
  })

// ---- Lock -------------------------------------------------------------------------------------

const lock = (command: Extract<Command, { readonly _tag: "Lock" }>, cwd: string): Effect.Effect<ExitCode, CliError, Clock> =>
  Effect.gen(function* () {
    const clock = yield* Clock
    const config = yield* loadConfig(cwd)
    const derived = yield* deriveEntries(config, cwd)
    const state = yield* readLock(cwd)
    // An unreadable lock is not overwritten: its history is the record of every reason ever given.
    if (state._tag === "Corrupt") return yield* Effect.fail(new CliError({ exit: 4, detail: `.faultline/lock does not decode, refusing to replace it: ${state.detail}` }))
    const previous = state._tag === "Present" ? Option.some(state.lock) : Option.none<LockFile>()
    const at = yield* clock.isoNow
    const next = yield* Effect.mapError(Effect.fromResult(recordLock({ previous, derived, reason: command.reason, at })), () => usage("lock needs a non-empty --reason"))
    const encoded = yield* Effect.mapError(Effect.fromResult(Schema.encodeResult(LockFile)(next)), (error) => harness(`encode lock: ${error.message}`))
    const path = lockPath(cwd)
    const staging = `${path}.partial`
    yield* io(harness, "create .faultline", () => mkdir(join(cwd, ".faultline"), { recursive: true }))
    // Written then renamed, so a crash mid-write cannot leave half a lock.
    yield* io(harness, `write ${path}`, () => writeFile(staging, `${JSON.stringify(encoded, null, 2)}\n`).then(() => rename(staging, path)))
    const added = next.history.length - (state._tag === "Present" ? state.lock.history.length : 0)
    yield* write(process.stdout, `faultline: lock written, ${added} key(s) moved\n`)
    return 0 as const
  })

// ---- Entry ------------------------------------------------------------------------------------

const program = (argv: ReadonlyArray<string>, cwd: string): Effect.Effect<ExitCode, never, Clock> =>
  Effect.gen(function* () {
    const command = yield* Effect.fromResult(parseArgs(argv))
    switch (command._tag) {
      case "Run":
        return yield* run(command, cwd)
      case "Lock":
        return yield* lock(command, cwd)
      default:
        return command satisfies never
    }
  }).pipe(Effect.catchTag("CliError", (error) => Effect.as(write(process.stderr, `faultline: ${error.detail}\n`), error.exit)))

Effect.runPromise(Effect.provide(program(process.argv.slice(2), process.cwd()), ClockLive)).then((code) => process.exit(code))
