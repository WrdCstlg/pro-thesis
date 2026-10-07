import { mkdir, rm } from "node:fs/promises"
import { dirname, join } from "node:path"
import { Data, Effect, Option, Result, Schema } from "effect"

// The gate is the only judge of a milestone. It is frozen by GATE.lock after M0, so every
// behavior a later acceptance row needs (config mutation, interruption, tampering, process
// accounting) is already here. It reads faultline's outputs through its own minimal schemas on
// purpose: a judge that imported the harness's schemas would inherit the harness's mistakes.
// It writes its report through process.stdout.write and never through console, which the scan
// bans in scripts/ as everywhere else outside src/cli.ts.

const ROOT = process.cwd()
const MILESTONES = ["M0", "M1", "M2", "M3", "M4", "M5"] as const
const LOCK_KEYS = ["driver", "profiles", "oracles.builtin", "oracles.external", "faults.allow", "faults.deny", "budget"] as const
const SCAN_DIRS = ["src", "fixture", "scripts"] as const
const ROW_TIMEOUT_MS = 900_000
const LEFTOVER_ATTEMPTS = 3
const LEFTOVER_SPACING_MS = 1_000
const GATE_DIR = join(ROOT, ".gate")
const CONFIG_PATH = join(ROOT, "faultline.json")
const CONFIG_BACKUP = join(GATE_DIR, "faultline.json.orig")
const LOCK_PATH = join(ROOT, ".faultline", "lock")
const RUNS_GLOB = ".faultline/runs/*/verdict.json"
const WORKSPACE_PREFIX = "faultline-"

class GateIoError extends Data.TaggedError("GateIoError")<{ readonly op: string; readonly cause: unknown }> {}
class GateDecodeError extends Data.TaggedError("GateDecodeError")<{ readonly file: string; readonly detail: string }> {}
type GateError = GateIoError | GateDecodeError

const describeError = (error: GateError): string =>
  error._tag === "GateIoError" ? `${error.op}: ${String(error.cause)}` : `${error.file}: ${error.detail}`

// ---- schemas the gate reads -------------------------------------------------------------------

const Milestone = Schema.Literals(MILESTONES)
type Milestone = typeof Milestone.Type
const LockStatus = Schema.Literals(["ok", "bypassed", "drift"])
const Sha256Hex = Schema.String.pipe(Schema.check(Schema.isPattern(/^[0-9a-f]{64}$/)))

const ExtraAssert = Schema.Union([
  Schema.Struct({ kind: Schema.Literal("inconclusiveReason"), reason: Schema.String, oracle: Schema.optionalKey(Schema.String) }),
  Schema.Struct({ kind: Schema.Literal("lockStatus"), status: LockStatus }),
  Schema.Struct({ kind: Schema.Literal("reportNamesOnly"), keys: Schema.Array(Schema.Literals(LOCK_KEYS)) }),
  Schema.Struct({ kind: Schema.Literal("bumpProfileWorlds"), profile: Schema.String, delta: Schema.Number }),
  Schema.Struct({ kind: Schema.Literal("noWorldStarted") }),
  Schema.Struct({ kind: Schema.Literal("lockFileUnchanged") }),
  Schema.Struct({ kind: Schema.Literal("interruptAfterMs"), ms: Schema.Number, signal: Schema.Literals(["SIGINT", "SIGTERM"]) }),
  Schema.Struct({ kind: Schema.Literal("workspaceExistsAtInterrupt") }),
  Schema.Struct({ kind: Schema.Literal("stdoutMatches"), pattern: Schema.String, record: Schema.String }),
  Schema.Struct({ kind: Schema.Literal("stdoutExcludes"), text: Schema.String }),
  Schema.Struct({ kind: Schema.Literal("tamperWorldSeedDigit"), from: Schema.String }),
  Schema.Struct({ kind: Schema.Literal("oracleProcessesGone"), min: Schema.Number }),
])
type ExtraAssert = typeof ExtraAssert.Type

const Row = Schema.Struct({
  id: Schema.String.pipe(Schema.check(Schema.isPattern(/^A\d{2}$/))),
  due: Milestone,
  args: Schema.Array(Schema.String),
  env: Schema.Record(Schema.String, Schema.String),
  expectedExit: Schema.Literals([0, 1, 2, 3, 4, 64]),
  expectedOracle: Schema.optionalKey(Schema.String),
  extraAssert: Schema.optionalKey(Schema.Array(ExtraAssert)),
})
type Row = typeof Row.Type
const Acceptance = Schema.Array(Row)

const GateLock = Schema.Struct({ "acceptance.json": Sha256Hex, "scripts/gate.ts": Sha256Hex })

const GateVerdict = Schema.Struct({
  exitCode: Schema.Number,
  lockStatus: LockStatus,
  worlds: Schema.Array(
    Schema.Struct({
      world: Schema.String,
      verdicts: Schema.Array(Schema.Struct({ _tag: Schema.Literals(["Ok", "Violation", "Inconclusive"]), oracle: Schema.String })),
    }),
  ),
  violations: Schema.Array(
    Schema.Struct({ world: Schema.String, oracle: Schema.String, witness: Schema.NonEmptyArray(Schema.String), explanation: Schema.String }),
  ),
  inconclusive: Schema.Array(
    Schema.Struct({ reason: Schema.String, world: Schema.optionalKey(Schema.String), oracle: Schema.optionalKey(Schema.String) }),
  ),
})
type GateVerdict = typeof GateVerdict.Type

const ConfigProfiles = Schema.Struct({ profiles: Schema.Record(Schema.String, Schema.Struct({ worlds: Schema.Number })) })
const WindowsProcesses = Schema.Array(Schema.Struct({ ProcessId: Schema.Number, ParentProcessId: Schema.Number }))

const decodeText =
  <A>(file: string, decode: (input: unknown) => Result.Result<A, Schema.SchemaError>) =>
  (text: string): Effect.Effect<A, GateDecodeError> =>
    Result.match(decode(text), {
      onFailure: (error) => Effect.fail(new GateDecodeError({ file, detail: String(error) })),
      onSuccess: (value) => Effect.succeed(value),
    })

// ---- io primitives ----------------------------------------------------------------------------

const io = <A>(op: string, run: () => PromiseLike<A>): Effect.Effect<A, GateIoError> =>
  Effect.tryPromise({ try: run, catch: (cause) => new GateIoError({ op, cause }) })

const readText = (path: string) => io(`read ${path}`, () => Bun.file(path).text())
const writeText = (path: string, text: string) => io(`write ${path}`, () => Bun.write(path, text))
const fileExists = (path: string) => io(`stat ${path}`, () => Bun.file(path).exists())
const removePath = (path: string) => io(`remove ${path}`, () => rm(path, { recursive: true, force: true }))
const makeDir = (path: string) => io(`mkdir ${path}`, () => mkdir(path, { recursive: true }))
const globIn = (cwd: string, pattern: string, onlyFiles: boolean) =>
  io(`glob ${pattern} in ${cwd}`, () => Array.fromAsync(new Bun.Glob(pattern).scan({ cwd, onlyFiles, dot: true })))
const emit = (text: string) => Effect.sync(() => process.stdout.write(text))

const sha256Hex = (bytes: Uint8Array | string): string => new Bun.CryptoHasher("sha256").update(bytes).digest("hex")

const fileDigest = (path: string) =>
  Effect.gen(function* () {
    const present = yield* fileExists(path)
    if (!present) return "absent"
    const bytes = yield* io(`read ${path}`, () => Bun.file(path).bytes())
    return sha256Hex(bytes)
  })

type Captured = { readonly exit: number; readonly stdout: string; readonly stderr: string }

const capture = (cmd: ReadonlyArray<string>): Effect.Effect<Captured, GateIoError> =>
  Effect.gen(function* () {
    const proc = yield* Effect.try({
      try: () => Bun.spawn([...cmd], { cwd: ROOT, stdin: "ignore", stdout: "pipe", stderr: "pipe" }),
      catch: (cause) => new GateIoError({ op: `spawn ${cmd.join(" ")}`, cause }),
    })
    const [exit, stdout, stderr] = yield* Effect.all(
      [io("wait", () => proc.exited), io("stdout", () => new Response(proc.stdout).text()), io("stderr", () => new Response(proc.stderr).text())],
      { concurrency: "unbounded" },
    )
    return { exit, stdout, stderr }
  })

// ---- process accounting -----------------------------------------------------------------------
// Windows keeps a dead parent's pid in ParentProcessId, so descendants of the exited CLI are still
// found by walking parent links. POSIX re-parents orphans, so there the gate also counts any new
// process in its own process group. A process that both calls setsid and is orphaned escapes the
// POSIX check; that limit is recorded in DECISIONS.md.

type ProcEntry = { readonly pid: number; readonly ppid: number; readonly pgid: number }

const POSIX_PS_LINE = /^\s*(\d+)\s+(\d+)\s+(\d+)\s*$/

const windowsSnapshot = Effect.gen(function* () {
  const out = yield* capture([
    "powershell",
    "-NoProfile",
    "-Command",
    "Get-CimInstance Win32_Process | Select-Object ProcessId,ParentProcessId | ConvertTo-Json -Compress",
  ])
  if (out.exit !== 0) return yield* Effect.fail(new GateIoError({ op: "list processes", cause: out.stderr }))
  const rows = yield* decodeText("Win32_Process listing", Schema.decodeUnknownResult(Schema.fromJsonString(WindowsProcesses)))(out.stdout)
  return rows.map((row): ProcEntry => ({ pid: row.ProcessId, ppid: row.ParentProcessId, pgid: -1 }))
})

const posixSnapshot = Effect.gen(function* () {
  const out = yield* capture(["ps", "-A", "-o", "pid=,ppid=,pgid="])
  if (out.exit !== 0) return yield* Effect.fail(new GateIoError({ op: "list processes", cause: out.stderr }))
  const lines = out.stdout.split("\n").filter((line) => line.trim() !== "")
  const parsed = lines.map((line) => POSIX_PS_LINE.exec(line))
  if (parsed.some((match) => match === null)) {
    return yield* Effect.fail(new GateDecodeError({ file: "ps listing", detail: "unparseable line" }))
  }
  return parsed
    .filter((match): match is RegExpExecArray => match !== null)
    .map((match): ProcEntry => ({ pid: Number(match[1]), ppid: Number(match[2]), pgid: Number(match[3]) }))
})

const snapshotProcesses: Effect.Effect<ReadonlyArray<ProcEntry>, GateError> =
  process.platform === "win32" ? windowsSnapshot : posixSnapshot

const descendantsOf = (roots: ReadonlySet<number>, procs: ReadonlyArray<ProcEntry>): ReadonlySet<number> => {
  const next = procs.filter((p) => roots.has(p.ppid) && !roots.has(p.pid)).map((p) => p.pid)
  return next.length === 0 ? roots : descendantsOf(new Set([...roots, ...next]), procs)
}

const leftoversOf = (cliPid: number, before: ReadonlySet<number>, procs: ReadonlyArray<ProcEntry>): ReadonlyArray<number> => {
  const gatePgid = procs.find((p) => p.pid === process.pid)?.pgid ?? -1
  const viaParents = [...descendantsOf(new Set([cliPid]), procs)].filter((pid) => pid !== cliPid)
  const viaGroup = procs
    .filter((p) => gatePgid !== -1 && p.pgid === gatePgid && p.pid !== process.pid && p.ppid !== process.pid)
    .map((p) => p.pid)
  return [...new Set([...viaParents, ...viaGroup])].filter((pid) => !before.has(pid)).sort((a, b) => a - b)
}

const settleLeftovers = (
  cliPid: number,
  before: ReadonlySet<number>,
  attemptsLeft: number,
): Effect.Effect<{ readonly leftovers: ReadonlyArray<number>; readonly after: ReadonlyArray<ProcEntry> }, GateError> =>
  Effect.gen(function* () {
    const after = yield* snapshotProcesses
    const leftovers = leftoversOf(cliPid, before, after)
    if (leftovers.length === 0 || attemptsLeft <= 1) return { leftovers, after }
    yield* Effect.sleep(LEFTOVER_SPACING_MS)
    return yield* settleLeftovers(cliPid, before, attemptsLeft - 1)
  })

// Leftovers are killed so one leaking row cannot poison the rows after it; they are still reported.
const killQuietly = (pid: number) =>
  Effect.result(Effect.try({ try: () => process.kill(pid, "SIGKILL"), catch: (cause) => new GateIoError({ op: `kill ${pid}`, cause }) }))

// ---- banned-pattern scan ----------------------------------------------------------------------
// Code patterns run on source with comments and string literals blanked. Marker words (TO-DO and
// friends, ts-directives) run on the raw source, because they live in comments and strings. The
// marker patterns are assembled from fragments so this file does not match itself.

const STRIPPABLE = new RegExp(
  "\\/\\/[^\\n]*|\\/\\*[\\s\\S]*?\\*\\/|\"(?:\\\\.|[^\"\\\\\\n])*\"|'(?:\\\\.|[^'\\\\\\n])*'|`(?:\\\\[\\s\\S]|[^`\\\\])*`",
  "g",
)

export const stripCommentsAndStrings = (source: string): string =>
  source.replace(STRIPPABLE, (token) =>
    token.startsWith("/") ? token.replace(/[^\n]/g, "") : `""${token.replace(/[^\n]/g, "")}`,
  )

type BannedPattern = { readonly label: string; readonly pattern: RegExp; readonly exempt: string; readonly raw: boolean }

const code = (label: string, source: string, exempt: string): BannedPattern => ({ label, pattern: new RegExp(source), exempt, raw: false })
const marker = (label: string, source: string, flags: string): BannedPattern => ({ label, pattern: new RegExp(source, flags), exempt: "", raw: true })

const BANNED: ReadonlyArray<BannedPattern> = [
  code("try {", "\\btry\\s*\\{", ""),
  code("catch (", "\\bcatch\\s*\\(", ""),
  code("throw", "\\bthrow\\b", ""),
  code("JSON.parse(", "\\bJSON\\.parse\\s*\\(", ""),
  code("Math.random(", "\\bMath\\.random\\s*\\(", ""),
  code(": any", ":\\s*any\\b", ""),
  code("<any>", "<\\s*any\\s*>", ""),
  code("as any", "\\bas\\s+any\\b", ""),
  code("as unknown as", "\\bas\\s+unknown\\s+as\\b", ""),
  code("as <Identifier>", "\\bas\\s+(?!const\\b)[A-Za-z_$][\\w$]*", ""),
  code("non-null assertion", "[\\w)\\]]!(?=[.)\\],;])", ""),
  code("else", "\\belse\\b", ""),
  code("let", "\\blet\\s", ""),
  code("import * as", "\\bimport\\s*\\*\\s*as\\b", ""),
  code("export default", "\\bexport\\s+default\\b", ""),
  code("Date.now(", "\\bDate\\.now\\s*\\(", "src/clock.ts"),
  code("performance.now(", "\\bperformance\\.now\\s*\\(", "src/clock.ts"),
  code("new Date(", "\\bnew\\s+Date\\s*\\(", "src/clock.ts"),
  code("console.", "\\bconsole\\.", "src/cli.ts"),
  marker("ts-ignore", "@ts-" + "ignore", ""),
  marker("ts-expect-error", "@ts-" + "expect-error", ""),
  marker("ts-nocheck", "@ts-" + "nocheck", ""),
  marker("TO" + "DO", "\\bTO" + "DO\\b", ""),
  marker("FIX" + "ME", "\\bFIX" + "ME\\b", ""),
  marker("X" + "XX", "\\bX" + "XX\\b", ""),
  marker("not " + "implemented", "not\\s+" + "implemented", "i"),
  marker("place" + "holder", "place" + "holder", "i"),
  marker("stub" + "()", "\\bstub" + "\\(\\)", ""),
]

type Finding = { readonly file: string; readonly line: number; readonly label: string }

export const findBanned = (file: string, source: string): ReadonlyArray<Finding> => {
  const rawLines = source.split("\n")
  const strippedLines = stripCommentsAndStrings(source).split("\n")
  return BANNED.filter((banned) => banned.exempt !== file).flatMap((banned) =>
    (banned.raw ? rawLines : strippedLines).flatMap((text, index) =>
      banned.pattern.test(text) ? [{ file, line: index + 1, label: banned.label }] : [],
    ),
  )
}

const scanStep = Effect.gen(function* () {
  const files = (yield* Effect.forEach(SCAN_DIRS, (dir) => globIn(ROOT, `${dir}/**/*.ts`, true)))
    .flat()
    .map((path) => path.split("\\").join("/"))
    .sort()
  const findings = (yield* Effect.forEach(files, (file) => Effect.map(readText(join(ROOT, file)), (source) => findBanned(file, source)))).flat()
  return { files, findings }
})

// ---- acceptance rows --------------------------------------------------------------------------

type RowResult = {
  readonly row: Row
  readonly actual: string
  readonly failures: ReadonlyArray<string>
  readonly notes: ReadonlyArray<string>
  readonly runDir: Option.Option<string>
}

const isKind =
  <K extends ExtraAssert["kind"]>(kind: K) =>
  (candidate: ExtraAssert): candidate is Extract<ExtraAssert, { readonly kind: K }> =>
    candidate.kind === kind

const isJsonObject = (value: unknown): value is { readonly [key: string]: unknown } =>
  typeof value === "object" && value !== null && !Array.isArray(value)

const bumpWorlds = (config: unknown, profile: string, delta: number): Option.Option<unknown> => {
  if (!isJsonObject(config)) return Option.none()
  const profiles = config["profiles"]
  if (!isJsonObject(profiles)) return Option.none()
  const target = profiles[profile]
  if (!isJsonObject(target)) return Option.none()
  const worlds = target["worlds"]
  if (typeof worlds !== "number") return Option.none()
  return Option.some({ ...config, profiles: { ...profiles, [profile]: { ...target, worlds: worlds + delta } } })
}

// One byte changes and the JSON stays valid, so replay must reject on the digest, not on parsing.
const SEED_FIELD = /"seed"\s*:\s*"?(\d+)/

const tamperSeedDigit = (text: string): Option.Option<string> => {
  const match = SEED_FIELD.exec(text)
  if (match === null) return Option.none()
  const at = match.index + match[0].length - 1
  const digit = text.charAt(at)
  return Option.some(`${text.slice(0, at)}${digit === "9" ? "8" : String(Number(digit) + 1)}${text.slice(at + 1)}`)
}

// The config mutation is restored by the scope finalizer. A backup file covers the case where the
// gate itself is killed before the finalizer runs: the next gate run restores it first.
const mutateConfig = (bump: Extract<ExtraAssert, { readonly kind: "bumpProfileWorlds" }>) =>
  Effect.acquireRelease(
    Effect.gen(function* () {
      const original = yield* readText(CONFIG_PATH)
      const parsed = yield* decodeText(CONFIG_PATH, Schema.decodeUnknownResult(Schema.fromJsonString(Schema.Unknown)))(original)
      const updated = yield* Option.match(bumpWorlds(parsed, bump.profile, bump.delta), {
        onNone: () => Effect.fail(new GateDecodeError({ file: CONFIG_PATH, detail: `profiles.${bump.profile}.worlds is not a number` })),
        onSome: (value) => Effect.succeed(value),
      })
      yield* makeDir(GATE_DIR)
      yield* writeText(CONFIG_BACKUP, original)
      yield* writeText(CONFIG_PATH, `${JSON.stringify(updated, null, 2)}\n`)
      return original
    }),
    (original) =>
      Effect.result(Effect.andThen(writeText(CONFIG_PATH, original), removePath(CONFIG_BACKUP))).pipe(
        Effect.flatMap((restored) =>
          Result.isFailure(restored)
            ? Effect.sync(() => process.stderr.write(`gate: could not restore faultline.json: ${describeError(restored.failure)}\n`))
            : Effect.void,
        ),
      ),
  )

const recoverConfigBackup = Effect.gen(function* () {
  const present = yield* fileExists(CONFIG_BACKUP)
  if (!present) return Option.none<string>()
  yield* writeText(CONFIG_PATH, yield* readText(CONFIG_BACKUP))
  yield* removePath(CONFIG_BACKUP)
  return Option.some("restored faultline.json from .gate/faultline.json.orig left by an interrupted gate run")
})

const listVerdictFiles = Effect.map(globIn(ROOT, RUNS_GLOB, true), (paths) => paths.map((path) => join(ROOT, path)).sort())

const readVerdict = (runDir: string) =>
  Effect.flatMap(readText(join(runDir, "verdict.json")), decodeText(join(runDir, "verdict.json"), Schema.decodeUnknownResult(Schema.fromJsonString(GateVerdict))))

// Total on purpose: an unreadable config yields none, which expectedShape reports as a failure of
// the row rather than aborting the row and hiding the CLI's own exit code.
const plannedWorlds = (profile: string) =>
  Effect.map(
    Effect.result(
      Effect.flatMap(readText(CONFIG_PATH), decodeText(CONFIG_PATH, Schema.decodeUnknownResult(Schema.fromJsonString(ConfigProfiles)))),
    ),
    (outcome) => (Result.isSuccess(outcome) ? Option.fromNullishOr(outcome.success.profiles[profile]?.worlds) : Option.none<number>()),
  )

const workspaceEntries = (tmpDir: string) => globIn(tmpDir, `${WORKSPACE_PREFIX}*`, false)

const requireRunDir = (prior: ReadonlyMap<string, RowResult>, from: string) =>
  Option.match(Option.flatMap(Option.fromNullishOr(prior.get(from)), (result) => result.runDir), {
    onNone: () => Effect.fail(new GateDecodeError({ file: from, detail: `row ${from} produced no run directory` })),
    onSome: (runDir) => Effect.succeed(runDir),
  })

const violatingWorldFile = (prior: ReadonlyMap<string, RowResult>, from: string) =>
  Effect.gen(function* () {
    const runDir = yield* requireRunDir(prior, from)
    const verdict = yield* readVerdict(runDir)
    const expected = prior.get(from)?.row.expectedOracle
    const violation = verdict.violations.find((candidate) => candidate.oracle === expected)
    if (violation === undefined) {
      return yield* Effect.fail(new GateDecodeError({ file: runDir, detail: `no ${String(expected)} violation in ${from}'s verdict` }))
    }
    return join(runDir, "worlds", violation.world, "world.json")
  })

const prepareTamperedWorld = (prior: ReadonlyMap<string, RowResult>, from: string) =>
  Effect.gen(function* () {
    const source = yield* violatingWorldFile(prior, from)
    const tampered = yield* Option.match(tamperSeedDigit(yield* readText(source)), {
      onNone: () => Effect.fail(new GateDecodeError({ file: source, detail: "no \"seed\" field to tamper" })),
      onSome: (text) => Effect.succeed(text),
    })
    const target = join(GATE_DIR, "tamper", "world.json")
    yield* makeDir(dirname(target))
    yield* writeText(target, tampered)
    return target
  })

const ARG_REFERENCE = /^\{(A\d{2})\.(world|verdict|tampered)\}$/

const resolveArg = (arg: string, prior: ReadonlyMap<string, RowResult>, tampered: Option.Option<string>) => {
  const match = ARG_REFERENCE.exec(arg)
  if (match === null) return Effect.succeed(arg)
  const from = match[1] ?? ""
  const field = match[2] ?? ""
  if (field === "verdict") return Effect.map(requireRunDir(prior, from), (runDir) => join(runDir, "verdict.json"))
  if (field === "world") return violatingWorldFile(prior, from)
  return Option.match(tampered, {
    onNone: () => Effect.fail(new GateDecodeError({ file: arg, detail: "no tamperWorldSeedDigit setup for this row" })),
    onSome: (path) => Effect.succeed(path),
  })
}

const rowEnv = (row: Row, tmpDir: string): Record<string, string> => {
  // An inherited FIXTURE_DEFECTS would silently turn a clean row into a defect row.
  const scrubbed = new Set(["FIXTURE_DEFECTS", "TMP", "TEMP", "TMPDIR"])
  const inherited = Object.entries(process.env).filter(
    (entry): entry is [string, string] => entry[1] !== undefined && !scrubbed.has(entry[0].toUpperCase()),
  )
  return { ...Object.fromEntries(inherited), TMP: tmpDir, TEMP: tmpDir, TMPDIR: tmpDir, ...row.env }
}

type CliRun = {
  readonly pid: number
  readonly completed: Option.Option<readonly [Captured, Option.Option<boolean>]>
}

const runCli = (
  args: ReadonlyArray<string>,
  env: Record<string, string>,
  interrupt: Option.Option<Extract<ExtraAssert, { readonly kind: "interruptAfterMs" }>>,
  tmpDir: string,
) =>
  Effect.gen(function* () {
    const proc = yield* Effect.acquireRelease(
      Effect.try({
        try: () => Bun.spawn([process.execPath, "src/cli.ts", ...args], { cwd: ROOT, env, stdin: "ignore", stdout: "pipe", stderr: "pipe" }),
        catch: (cause) => new GateIoError({ op: "spawn faultline", cause }),
      }),
      (child) => Effect.sync(() => (child.exitCode === null && child.signalCode === null ? child.kill(9) : undefined)),
    )
    const collect = Effect.map(
      Effect.all([io("wait", () => proc.exited), io("stdout", () => new Response(proc.stdout).text()), io("stderr", () => new Response(proc.stderr).text())], {
        concurrency: "unbounded",
      }),
      ([exit, stdout, stderr]): Captured => ({ exit, stdout, stderr }),
    )
    const signaller = Option.match(interrupt, {
      onNone: () => Effect.succeed(Option.none<boolean>()),
      onSome: (setup) =>
        Effect.gen(function* () {
          yield* Effect.sleep(setup.ms)
          const workspaces = yield* workspaceEntries(tmpDir)
          yield* Effect.sync(() => proc.kill(setup.signal))
          return Option.some(workspaces.length > 0)
        }),
    })
    const completed = yield* Effect.timeoutOption(Effect.all([collect, signaller], { concurrency: "unbounded" }), ROW_TIMEOUT_MS)
    return { pid: proc.pid, completed } satisfies CliRun
  })

const LOCK_KEY_NAMED = (key: string) => new RegExp(`(^|[^A-Za-z0-9_.])${key.split(".").join("\\.")}(?![A-Za-z0-9_])`, "m")

const verdictConsistency = (row: Row, exit: number, verdict: GateVerdict, planned: Option.Option<number>): ReadonlyArray<string> => {
  const exitMatches = verdict.exitCode === exit ? [] : [`verdict.json exitCode ${verdict.exitCode} differs from process exit ${exit}`]
  return [...exitMatches, ...expectedShape(row, verdict, planned)]
}

const expectedShape = (row: Row, verdict: GateVerdict, planned: Option.Option<number>): ReadonlyArray<string> => {
  const expected = row.expectedExit
  switch (expected) {
    case 0:
      return [
        ...(verdict.lockStatus === "ok" ? [] : [`lockStatus ${verdict.lockStatus} on a PASS`]),
        ...Option.match(planned, {
          onNone: () => ["profile's planned world count not found in faultline.json"],
          onSome: (count) => (verdict.worlds.length === count ? [] : [`${verdict.worlds.length} worlds judged, ${count} planned`]),
        }),
        ...verdict.worlds.filter((world) => world.verdicts.length === 0).map((world) => `world ${world.world} has no oracle verdict on a PASS`),
        ...verdict.worlds.flatMap((world) =>
          world.verdicts.filter((v) => v._tag !== "Ok").map((v) => `world ${world.world}: ${v.oracle} returned ${v._tag} on a PASS`),
        ),
        ...(verdict.violations.length === 0 ? [] : ["violations present on a PASS"]),
        ...(verdict.inconclusive.length === 0 ? [] : ["inconclusive entries present on a PASS"]),
      ]
    case 1:
      return [
        ...(verdict.violations.length > 0 ? [] : ["exit 1 without any recorded violation"]),
        ...(row.expectedOracle === undefined || verdict.violations.some((v) => v.oracle === row.expectedOracle)
          ? []
          : [`no violation from ${row.expectedOracle}`]),
      ]
    case 2:
      return [
        ...(verdict.violations.length === 0 ? [] : ["violations present on an INCONCLUSIVE"]),
        ...(verdict.inconclusive.length > 0 ? [] : ["exit 2 without any recorded inconclusive reason"]),
      ]
    case 3:
      return verdict.violations.length === 0 ? [] : ["violations present on a BUDGET_EXHAUSTED"]
    case 4:
      return verdict.lockStatus === "drift" ? [] : [`lockStatus ${verdict.lockStatus} on a LOCK_DRIFT`]
    case 64:
      return []
  }
  return expected satisfies never
}

type Observed = {
  readonly exit: number
  readonly output: string
  readonly stdout: string
  readonly verdict: Option.Option<GateVerdict>
  readonly worldFiles: ReadonlyArray<string>
  readonly pidsFromFiles: ReadonlyArray<number>
  readonly alive: ReadonlySet<number>
  readonly lockBefore: string
  readonly lockAfter: string
  readonly workspaceAtInterrupt: Option.Option<boolean>
}

const checkExtra = (extra: ExtraAssert, observed: Observed): { readonly failures: ReadonlyArray<string>; readonly notes: ReadonlyArray<string> } => {
  const fail = (message: string) => ({ failures: [message], notes: [] })
  const pass = { failures: [], notes: [] }
  const kind = extra.kind
  switch (kind) {
    case "inconclusiveReason":
      return Option.match(observed.verdict, {
        onNone: () => fail(`no verdict.json to find reason ${extra.reason}`),
        onSome: (verdict) =>
          verdict.inconclusive.some((entry) => entry.reason === extra.reason && (extra.oracle === undefined || entry.oracle === extra.oracle))
            ? pass
            : fail(`no inconclusive entry with reason ${extra.reason}${extra.oracle === undefined ? "" : ` from ${extra.oracle}`}`),
      })
    case "lockStatus":
      return Option.match(observed.verdict, {
        onNone: () => fail("no verdict.json to read lockStatus from"),
        onSome: (verdict) => (verdict.lockStatus === extra.status ? pass : fail(`lockStatus ${verdict.lockStatus}, expected ${extra.status}`)),
      })
    case "reportNamesOnly": {
      const named = LOCK_KEYS.filter((key) => LOCK_KEY_NAMED(key).test(observed.output))
      const wanted = [...extra.keys].sort()
      return named.join(",") === LOCK_KEYS.filter((key) => wanted.includes(key)).join(",")
        ? pass
        : fail(`report names [${named.join(", ")}], expected exactly [${wanted.join(", ")}]`)
    }
    case "noWorldStarted":
      return observed.worldFiles.length === 0 ? pass : fail(`${observed.worldFiles.length} world.json files written`)
    case "lockFileUnchanged":
      return observed.lockBefore === observed.lockAfter ? pass : fail(".faultline/lock changed")
    case "workspaceExistsAtInterrupt":
      return Option.match(observed.workspaceAtInterrupt, {
        onNone: () => fail("the interrupt was never sent"),
        onSome: (existed) => (existed ? pass : fail(`no ${WORKSPACE_PREFIX}* workspace in TMP when the interrupt was sent (positive control failed)`)),
      })
    case "stdoutMatches": {
      const match = new RegExp(extra.pattern, "m").exec(observed.stdout)
      return match === null ? fail(`stdout does not match /${extra.pattern}/`) : { failures: [], notes: [`${extra.record}=${match[1] ?? match[0]}`] }
    }
    case "stdoutExcludes":
      return observed.stdout.includes(extra.text) ? fail(`stdout contains "${extra.text}"`) : pass
    case "oracleProcessesGone": {
      const alive = observed.pidsFromFiles.filter((pid) => observed.alive.has(pid))
      return observed.pidsFromFiles.length < extra.min
        ? fail(`${observed.pidsFromFiles.length} oracle pid files found, at least ${extra.min} required`)
        : alive.length === 0
          ? pass
          : fail(`oracle processes still alive: ${alive.join(", ")}`)
    }
    case "bumpProfileWorlds":
    case "interruptAfterMs":
    case "tamperWorldSeedDigit":
      return pass
  }
  return kind satisfies never
}

const runRowUnsafe = (row: Row, prior: ReadonlyMap<string, RowResult>) =>
  Effect.gen(function* () {
    const extras = row.extraAssert ?? []
    const tmpDir = join(GATE_DIR, "tmp", row.id)
    yield* removePath(tmpDir)
    yield* makeDir(tmpDir)
    const tamper = Option.fromNullishOr(extras.find(isKind("tamperWorldSeedDigit")))
    const tampered = yield* Option.match(tamper, {
      onNone: () => Effect.succeed(Option.none<string>()),
      onSome: (setup) => Effect.map(prepareTamperedWorld(prior, setup.from), Option.some),
    })
    const args = yield* Effect.forEach(row.args, (arg) => resolveArg(arg, prior, tampered))
    const lockBefore = yield* fileDigest(LOCK_PATH)
    const before = new Set((yield* snapshotProcesses).map((p) => p.pid))
    const verdictsBefore = yield* listVerdictFiles
    const bump = Option.fromNullishOr(extras.find(isKind("bumpProfileWorlds")))
    const interrupt = Option.fromNullishOr(extras.find(isKind("interruptAfterMs")))
    const cli = yield* Effect.scoped(
      Effect.gen(function* () {
        yield* Option.match(bump, { onNone: () => Effect.void, onSome: (setup) => Effect.asVoid(mutateConfig(setup)) })
        return yield* runCli(args, rowEnv(row, tmpDir), interrupt, tmpDir)
      }),
    )
    const settled = yield* settleLeftovers(cli.pid, before, LEFTOVER_ATTEMPTS)
    yield* Effect.forEach(settled.leftovers, killQuietly)
    const workspacesAfter = yield* workspaceEntries(tmpDir)
    const lockAfter = yield* fileDigest(LOCK_PATH)
    const verdictsAfter = yield* listVerdictFiles
    const fresh = verdictsAfter.filter((path) => !verdictsBefore.includes(path))
    const runDir = fresh.length === 1 ? Option.fromNullishOr(fresh[0]).pipe(Option.map(dirname)) : Option.none<string>()
    const hygiene = [
      ...(settled.leftovers.length === 0 ? [] : [`leftover processes after exit (killed by the gate): ${settled.leftovers.join(", ")}`]),
      ...(workspacesAfter.length === 0 ? [] : [`workspaces left in TMP: ${workspacesAfter.join(", ")}`]),
    ]
    return yield* Option.match(cli.completed, {
      onNone: () =>
        Effect.succeed<RowResult>({ row, actual: "gate-timeout", failures: [`no exit within ${ROW_TIMEOUT_MS} ms`, ...hygiene], notes: [], runDir }),
      onSome: ([captured, workspaceAtInterrupt]) =>
        Effect.gen(function* () {
          const isRun = row.args[0] === "run"
          const verdictRequired = isRun && row.expectedExit !== 64
          const verdict = yield* Option.match(runDir, {
            onNone: () => Effect.succeed(Option.none<GateVerdict>()),
            onSome: (dir) => Effect.map(readVerdict(dir), Option.some),
          })
          const profileIndex = row.args.indexOf("--profile")
          const profile = profileIndex === -1 ? undefined : row.args[profileIndex + 1]
          const planned = row.expectedExit === 0 && profile !== undefined ? yield* plannedWorlds(profile) : Option.none<number>()
          const worldFiles = yield* Option.match(runDir, {
            onNone: () => Effect.succeed<ReadonlyArray<string>>([]),
            onSome: (dir) => globIn(dir, "worlds/*/world.json", true),
          })
          const pidFiles = yield* Option.match(runDir, {
            onNone: () => Effect.succeed<ReadonlyArray<string>>([]),
            onSome: (dir) => Effect.map(globIn(dir, "**/*.pid", true), (paths) => paths.map((path) => join(dir, path))),
          })
          const pidsFromFiles = (yield* Effect.forEach(pidFiles, readText)).map((text) => Number(text.trim())).filter(Number.isInteger)
          const observed: Observed = {
            exit: captured.exit,
            output: `${captured.stdout}\n${captured.stderr}`,
            stdout: captured.stdout,
            verdict,
            worldFiles,
            pidsFromFiles,
            alive: new Set(settled.after.map((p) => p.pid)),
            lockBefore,
            lockAfter,
            workspaceAtInterrupt,
          }
          const checks = extras.map((extra) => checkExtra(extra, observed))
          const failures = [
            ...(captured.exit === row.expectedExit ? [] : [`exit ${captured.exit}, expected ${row.expectedExit}`]),
            ...(verdictRequired && fresh.length !== 1 ? [`expected exactly one new verdict.json, found ${fresh.length}`] : []),
            ...(verdictRequired
              ? Option.match(verdict, { onNone: () => [], onSome: (v) => verdictConsistency(row, captured.exit, v, planned) })
              : []),
            ...checks.flatMap((check) => check.failures),
            ...hygiene,
          ]
          return { row, actual: `exit=${captured.exit}`, failures, notes: checks.flatMap((check) => check.notes), runDir }
        }),
    })
  })

const runRow = (row: Row, prior: ReadonlyMap<string, RowResult>): Effect.Effect<RowResult> =>
  Effect.map(Effect.result(runRowUnsafe(row, prior)), (outcome) =>
    Result.isSuccess(outcome)
      ? outcome.success
      : { row, actual: "gate-error", failures: [`gate could not execute the row: ${describeError(outcome.failure)}`], notes: [], runDir: Option.none() },
  )

// ---- steps and report -------------------------------------------------------------------------

type StepResult = { readonly name: string; readonly ok: boolean; readonly detail: string; readonly tail: ReadonlyArray<string> }

const tailOf = (text: string, count: number): ReadonlyArray<string> => text.split("\n").filter((line) => line.trim() !== "").slice(-count)

const commandStep = (name: string, cmd: ReadonlyArray<string>) =>
  Effect.map(Effect.result(capture(cmd)), (outcome): StepResult =>
    Result.isSuccess(outcome)
      ? {
          name,
          ok: outcome.success.exit === 0,
          detail: `exit ${outcome.success.exit}`,
          tail: outcome.success.exit === 0 ? [] : tailOf(`${outcome.success.stdout}\n${outcome.success.stderr}`, 40),
        }
      : { name, ok: false, detail: describeError(outcome.failure), tail: [] },
  )

const gateLockStep = Effect.gen(function* () {
  const acceptanceHash = sha256Hex(yield* io("read acceptance.json", () => Bun.file(join(ROOT, "acceptance.json")).bytes()))
  const gateHash = sha256Hex(yield* io("read scripts/gate.ts", () => Bun.file(join(ROOT, "scripts", "gate.ts")).bytes()))
  const locked = yield* decodeText("GATE.lock", Schema.decodeUnknownResult(Schema.fromJsonString(GateLock)))(yield* readText(join(ROOT, "GATE.lock")))
  const ok = locked["acceptance.json"] === acceptanceHash && locked["scripts/gate.ts"] === gateHash
  return {
    name: "gate-lock",
    ok,
    detail: `acceptance.json=${acceptanceHash} gate.ts=${gateHash}`,
    tail: ok ? [] : [`GATE.lock holds acceptance.json=${locked["acceptance.json"]} gate.ts=${locked["scripts/gate.ts"]}`],
  } satisfies StepResult
})

const statusOf = (result: RowResult, milestone: Milestone): "PASS" | "FAIL" | "PENDING" => {
  const due = MILESTONES.indexOf(result.row.due) <= MILESTONES.indexOf(milestone)
  if (result.failures.length === 0) return "PASS"
  return due ? "FAIL" : "PENDING"
}

const expectedLabel = (row: Row): string => {
  const reason = row.extraAssert?.find(isKind("inconclusiveReason"))?.reason
  return [String(row.expectedExit), row.expectedOracle, reason].filter((part): part is string => part !== undefined).join(" ")
}

const table = (header: ReadonlyArray<string>, rows: ReadonlyArray<ReadonlyArray<string>>): string => {
  const widths = header.map((cell, column) => Math.max(cell.length, ...rows.map((row) => (row[column] ?? "").length)))
  const line = (cells: ReadonlyArray<string>) => cells.map((cell, column) => cell.padEnd(widths[column] ?? 0)).join(" | ").trimEnd()
  return [line(header), widths.map((width) => "-".repeat(width)).join("-|-"), ...rows.map(line)].join("\n")
}

const runRowsInOrder = (rows: ReadonlyArray<Row>) =>
  Effect.reduce(rows, () => new Map<string, RowResult>(), (done, row) =>
    Effect.map(runRow(row, done), (result) => new Map([...done, [row.id, result]])),
  )

const main = Effect.gen(function* () {
  const recovered = yield* recoverConfigBackup
  const milestone = yield* Effect.flatMap(readText(join(ROOT, "MILESTONE")), (text) =>
    decodeText("MILESTONE", Schema.decodeUnknownResult(Milestone))(text.trim()),
  )
  yield* emit(`faultline gate: milestone ${milestone}\n`)
  yield* Option.match(recovered, { onNone: () => Effect.void, onSome: (note) => emit(`gate: ${note}\n`) })

  const typecheck = yield* commandStep("typecheck", [process.execPath, "run", "typecheck"])
  const tests = yield* commandStep("test", [process.execPath, "test"])
  const scan = yield* Effect.map(Effect.result(scanStep), (outcome): StepResult =>
    Result.isSuccess(outcome)
      ? {
          name: "scan",
          ok: outcome.success.findings.length === 0,
          detail: `${outcome.success.files.length} files, ${outcome.success.findings.length} matches`,
          tail: outcome.success.findings.map((finding) => `${finding.file}:${finding.line}: ${finding.label}`),
        }
      : { name: "scan", ok: false, detail: describeError(outcome.failure), tail: [] },
  )
  const gateLock = yield* Effect.map(Effect.result(gateLockStep), (outcome): StepResult =>
    Result.isSuccess(outcome) ? outcome.success : { name: "gate-lock", ok: false, detail: describeError(outcome.failure), tail: [] },
  )
  const steps = [typecheck, tests, scan, gateLock]
  yield* emit(`\n${table(["step", "status", "detail"], steps.map((step) => [step.name, step.ok ? "PASS" : "FAIL", step.detail]))}\n`)
  yield* Effect.forEach(steps.filter((step) => step.tail.length > 0), (step) =>
    emit(`\n${step.name} output (tail):\n${step.tail.map((line) => `  ${line}`).join("\n")}\n`),
  )

  const rows = yield* Effect.flatMap(readText(join(ROOT, "acceptance.json")), decodeText("acceptance.json", Schema.decodeUnknownResult(Schema.fromJsonString(Acceptance))))
  const results = [...(yield* runRowsInOrder(rows)).values()]
  const statuses = results.map((result) => ({ result, status: statusOf(result, milestone) }))
  yield* emit(
    `\n${table(
      ["row", "due", "expected", "actual", "status"],
      statuses.map((entry) => [entry.result.row.id, entry.result.row.due, expectedLabel(entry.result.row), entry.result.actual, entry.status]),
    )}\n`,
  )
  const detailed = statuses.filter((entry) => entry.result.failures.length > 0 || entry.result.notes.length > 0)
  yield* Effect.forEach(detailed, (entry) =>
    emit(
      `\n${entry.result.row.id} (${entry.status}):\n${[...entry.result.failures.map((f) => `  - ${f}`), ...entry.result.notes.map((n) => `  note: ${n}`)].join("\n")}\n`,
    ),
  )

  const due = statuses.filter((entry) => MILESTONES.indexOf(entry.result.row.due) <= MILESTONES.indexOf(milestone))
  const duePassed = due.filter((entry) => entry.status === "PASS")
  const pending = statuses.filter((entry) => entry.status === "PENDING").map((entry) => entry.result.row.id)
  const stepsOk = steps.every((step) => step.ok)
  const exitCode = stepsOk && duePassed.length === due.length ? 0 : 1
  yield* emit(
    `\nsummary: steps ${steps.filter((step) => step.ok).length}/${steps.length}, due rows ${duePassed.length}/${due.length} passed, pending: ${pending.length === 0 ? "none" : pending.join(",")}\ngate exit ${exitCode}\n`,
  )
  return exitCode
})

const program = Effect.flatMap(Effect.result(main), (outcome) =>
  Result.isSuccess(outcome) ? Effect.succeed(outcome.success) : Effect.as(emit(`gate: cannot run: ${describeError(outcome.failure)}\ngate exit 1\n`), 1),
)

if (import.meta.main) {
  Effect.runPromise(program).then((code) => process.exit(code))
}
