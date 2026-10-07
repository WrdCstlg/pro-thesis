import { mkdir, mkdtemp, readdir, readFile, rm, writeFile } from "node:fs/promises"
import { join } from "node:path"
import { Context, Data, Effect, Layer, Option, Result, Schema, Scope } from "effect"
import { digestOf } from "./canonical"
import { Clock } from "./clock"
import { decodeHistory, makeRecorder, type History, type Recorder } from "./history"
import { promptTextOf, ProviderStub, scriptDigestOf, type ScriptParams } from "./provider/stub"
import {
  MessageId,
  PortAnnouncement,
  sessionIdOf,
  WorldFile,
  type FaultlineConfig,
  type OracleContext,
  type OracleName,
  type OracleVerdict,
  type Profile,
  type Seed,
  type SessionId,
  type WorkspaceSnapshot,
  type WorldId,
} from "./schema"
import type { WorldGuard, WorldOutcome } from "./verdict"

// One world: a fresh workspace, a recorder, the provider stub, one SUT process, the load, then
// ASSERT. Every resource is acquired in one Scope, so the SUT is killed, the stub stopped and the
// workspace removed on success, failure, timeout and interruption alike, in reverse order of
// acquisition (the SUT dies before its workspace is removed, which Windows needs to unlock files).

export class RunnerError extends Data.TaggedError("RunnerError")<{
  readonly stage: "workspace" | "spawn" | "boot" | "load" | "assert" | "world-file"
  readonly detail: string
}> {}

const io = <A>(stage: RunnerError["stage"], detail: string, run: (signal: AbortSignal) => PromiseLike<A>): Effect.Effect<A, RunnerError> =>
  Effect.tryPromise({ try: run, catch: (error) => new RunnerError({ stage, detail: `${detail}: ${String(error)}` }) })

// ---- Services ---------------------------------------------------------------------------------

export type WorkspaceDirs = {
  readonly root: string
  // What the SUT's tools may change and what ASSERT snapshots. Nothing else is put here.
  readonly files: string
  readonly state: string
  readonly portFile: string
}

export class Workspace extends Context.Service<Workspace, { readonly acquire: Effect.Effect<WorkspaceDirs, RunnerError, Scope.Scope> }>()(
  "faultline/Workspace",
) {}

// Directories are named faultline-* under `base`; the gate looks for that prefix in TMP after every
// row. Removal retries because Windows can hold a file for a moment after its process has exited.
export const workspaceLive = (base: string): Layer.Layer<Workspace> =>
  Layer.succeed(Workspace, {
    acquire: Effect.gen(function* () {
      const root = yield* Effect.acquireRelease(io("workspace", `create a workspace under ${base}`, () => mkdtemp(join(base, "faultline-"))), (dir) =>
        Effect.asVoid(Effect.result(io("workspace", `remove ${dir}`, () => rm(dir, { recursive: true, force: true, maxRetries: 20, retryDelay: 100 })))),
      )
      const files = join(root, "files")
      yield* io("workspace", `create ${files}`, () => mkdir(files))
      return { root, files, state: join(root, "state.sqlite"), portFile: join(root, "port.json") }
    }),
  })

export type SpawnRequest = {
  readonly cmd: readonly [string, ...Array<string>]
  readonly cwd: string
  readonly env: Readonly<Record<string, string>>
  readonly stdoutPath: string
  readonly stderrPath: string
}

export type Spawned = {
  readonly pid: number
  // undefined while the process runs; a description of how it ended once it has.
  readonly ended: Effect.Effect<string | undefined>
}

export class ProcessControl extends Context.Service<ProcessControl, { readonly spawn: (request: SpawnRequest) => Effect.Effect<Spawned, RunnerError, Scope.Scope> }>()(
  "faultline/ProcessControl",
) {}

// Release waits for the exit after kill(9) (TerminateProcess on Windows), bounded so that a process
// the OS will not reap cannot hang teardown; the gate's leftover check still sees such a process.
const KILL_WAIT_MS = 5_000

export const ProcessControlLive = Layer.succeed(ProcessControl, {
  spawn: (request) =>
    Effect.map(
      Effect.acquireRelease(
        Effect.try({
          try: () =>
            Bun.spawn([...request.cmd], {
              cwd: request.cwd,
              env: { ...request.env },
              stdin: "ignore",
              stdout: Bun.file(request.stdoutPath),
              stderr: Bun.file(request.stderrPath),
            }),
          catch: (error) => new RunnerError({ stage: "spawn", detail: `${request.cmd.join(" ")}: ${String(error)}` }),
        }),
        (proc) =>
          Effect.asVoid(
            Effect.andThen(
              Effect.sync(() => (proc.exitCode === null && proc.signalCode === null ? proc.kill(9) : undefined)),
              Effect.timeoutOption(Effect.promise(() => proc.exited), KILL_WAIT_MS),
            ),
          ),
      ),
      (proc): Spawned => ({
        pid: proc.pid,
        ended: Effect.sync(() => (proc.exitCode !== null ? `exit ${proc.exitCode}` : proc.signalCode !== null ? `signal ${proc.signalCode}` : undefined)),
      }),
    ),
})

// ---- The driver environment (D-023) -----------------------------------------------------------

// Only allow-listed variables reach the SUT, plus the four launch variables faultline owns. Names
// are matched without regard to case because Windows spells PATH as `Path`.
export const driverEnvOf = (
  allow: ReadonlyArray<string>,
  inherited: Readonly<Record<string, string | undefined>>,
  launch: Readonly<Record<string, string>>,
): Record<string, string> => {
  const wanted = new Set(allow.map((name) => name.toUpperCase()))
  return {
    ...Object.fromEntries(Object.entries(inherited).filter((entry): entry is [string, string] => entry[1] !== undefined && wanted.has(entry[0].toUpperCase()))),
    ...launch,
  }
}

// ---- Boot -------------------------------------------------------------------------------------

const BOOT_TIMEOUT_MS = 15_000
const POLL_MS = 25
const PROMPT_TIMEOUT_MS = 60_000

const decodePort = Schema.decodeUnknownResult(Schema.fromJsonString(PortAnnouncement))
const decodeMessageId = Schema.decodeUnknownResult(MessageId)

const failIfEnded = (sut: Spawned, waitingFor: string): Effect.Effect<void, RunnerError> =>
  Effect.flatMap(sut.ended, (ended) =>
    ended === undefined ? Effect.void : Effect.fail(new RunnerError({ stage: "boot", detail: `the SUT ended (${ended}) while faultline waited for ${waitingFor}` })),
  )

const pollPort = (portFile: string, sut: Spawned): Effect.Effect<number, RunnerError> =>
  Effect.gen(function* () {
    yield* failIfEnded(sut, "its port file")
    const text = yield* Effect.result(io("boot", `read ${portFile}`, () => readFile(portFile, "utf8")))
    const announced = Result.isSuccess(text) ? Result.getOrUndefined(decodePort(text.success)) : undefined
    if (announced !== undefined) return announced.port
    yield* Effect.sleep(POLL_MS)
    return yield* pollPort(portFile, sut)
  })

const pollHealth = (port: number, sut: Spawned): Effect.Effect<void, RunnerError> =>
  Effect.gen(function* () {
    yield* failIfEnded(sut, "/health")
    const status = yield* Effect.result(
      io("boot", "GET /health", (signal) => fetch(`http://127.0.0.1:${port}/health`, { signal }).then((response) => response.text().then(() => response.status))),
    )
    if (Result.isSuccess(status) && status.success === 200) return
    yield* Effect.sleep(POLL_MS)
    return yield* pollHealth(port, sut)
  })

const boot = (portFile: string, sut: Spawned): Effect.Effect<number, RunnerError> =>
  Effect.flatMap(
    Effect.timeoutOption(
      Effect.flatMap(pollPort(portFile, sut), (port) => Effect.as(pollHealth(port, sut), port)),
      BOOT_TIMEOUT_MS,
    ),
    (port) =>
      Option.match(port, {
        onNone: () => Effect.fail(new RunnerError({ stage: "boot", detail: `no port file and healthy /health within ${BOOT_TIMEOUT_MS} ms` })),
        onSome: Effect.succeed,
      }),
  )

// ---- Load -------------------------------------------------------------------------------------

// The prompt op is completed from what faultline's own client saw: ok on a 2xx, fail on any other
// status, info when no answer arrived (timeout or transport error), because the SUT may or may not
// have taken the prompt in.
const promptSession = (recorder: Recorder, port: number, session: SessionId, seed: bigint): Effect.Effect<void, RunnerError | Schema.SchemaError> =>
  Effect.gen(function* () {
    const messageId = yield* Effect.fromResult(decodeMessageId(`msg-${session}-0`))
    const text = promptTextOf(session, seed)
    const op = yield* Effect.mapError(recorder.invoke(session, "prompt", { session, messageId, text }), (error) => new RunnerError({ stage: "load", detail: error.detail }))
    const answer = yield* Effect.timeoutOption(
      Effect.result(
        io("load", `POST /session/${session}/prompt`, (signal) =>
          fetch(`http://127.0.0.1:${port}/session/${session}/prompt`, {
            method: "POST",
            headers: { "content-type": "application/json" },
            body: JSON.stringify({ messageId, text }),
            signal,
          }).then((response) => response.text().then(() => response.status)),
        ),
      ),
      PROMPT_TIMEOUT_MS,
    )
    const completion = Option.match(answer, {
      onNone: () => recorder.complete(op, session, "info", "prompt", { timeoutMs: PROMPT_TIMEOUT_MS }),
      onSome: (outcome) =>
        Result.isFailure(outcome)
          ? recorder.complete(op, session, "info", "prompt", { error: outcome.failure.detail })
          : recorder.complete(op, session, outcome.success >= 200 && outcome.success < 300 ? "ok" : "fail", "prompt", { status: outcome.success }),
    })
    yield* Effect.mapError(completion, (error) => new RunnerError({ stage: "load", detail: error.detail }))
  })

// ---- ASSERT -----------------------------------------------------------------------------------

export type BuiltinOracle = {
  readonly name: OracleName
  readonly judge: (history: History, context: OracleContext) => OracleVerdict
}

const compareNames = (a: string, b: string): number => (a < b ? -1 : a > b ? 1 : 0)

// Anything in the workspace other than a regular file fails the snapshot: an oracle must never be
// handed a partial view of the workspace without being told.
const snapshotOf = (dir: string): Effect.Effect<WorkspaceSnapshot, RunnerError> =>
  Effect.gen(function* () {
    const entries = yield* io("assert", `list ${dir}`, () => readdir(dir, { withFileTypes: true }))
    const odd = entries.filter((entry) => !entry.isFile()).map((entry) => entry.name)
    if (odd.length > 0) return yield* Effect.fail(new RunnerError({ stage: "assert", detail: `the workspace holds entries that are not files: ${odd.join(", ")}` }))
    const names = entries.map((entry) => entry.name).toSorted(compareNames)
    const contents = yield* Effect.forEach(names, (name) => Effect.map(io("assert", `read ${name}`, () => readFile(join(dir, name), "utf8")), (text) => [name, text] as const))
    return { files: Object.fromEntries(contents) }
  })

// The history is judged from the file on disk, decoded with the same rules any reader would use. A
// file that does not decode is not judged by anyone: every oracle reports history-malformed.
export const judgeHistory = (oracles: ReadonlyArray<BuiltinOracle>, historyText: string, workspace: WorkspaceSnapshot): ReadonlyArray<OracleVerdict> =>
  Result.match(decodeHistory(historyText), {
    onFailure: () => oracles.map((oracle): OracleVerdict => ({ _tag: "Inconclusive", oracle: oracle.name, reason: "history-malformed" })),
    onSuccess: (history) => oracles.map((oracle) => oracle.judge(history, { workspace })),
  })

// ---- The world --------------------------------------------------------------------------------

export type WorldPlan = {
  readonly world: WorldId
  // <run>/worlds/<world>; created here.
  readonly dir: string
  readonly seed: Seed
  readonly profileName: string
  readonly profile: Profile
  readonly driver: FaultlineConfig["driver"]
  readonly projectDir: string
  readonly inheritedEnv: Readonly<Record<string, string | undefined>>
  readonly oracles: ReadonlyArray<BuiltinOracle>
}

export type WorldResult = {
  readonly outcome: WorldOutcome
  // Why the world did not reach ASSERT, or what the stub could not account for. Empty on a world
  // that was judged.
  readonly problems: ReadonlyArray<string>
}

// Every error a world can fail with carries either `detail` (faultline's own tagged errors) or a
// schema message.
const describe = (error: { readonly _tag: string; readonly message: string; readonly detail?: string }): string =>
  `${error._tag}: ${error.detail ?? error.message}`

const notAsserted = (world: WorldId, problems: ReadonlyArray<string>): WorldResult => ({
  outcome: { world, reachedAssert: false, verdicts: [], guards: [] },
  problems,
})

const liveWorld = (plan: WorldPlan, script: ScriptParams, sessions: ReadonlyArray<SessionId>) =>
  Effect.gen(function* () {
    const workspace = yield* Workspace
    const processes = yield* ProcessControl
    const provider = yield* ProviderStub
    const dirs = yield* workspace.acquire
    const recorder = yield* makeRecorder(join(plan.dir, "history.jsonl"))
    const stub = yield* provider.serve(script, recorder)
    const sut = yield* processes.spawn({
      cmd: plan.driver.cmd,
      cwd: plan.projectDir,
      env: driverEnvOf(plan.driver.envAllow, plan.inheritedEnv, {
        PROVIDER_BASE_URL: stub.baseUrl,
        FAULTLINE_WORKSPACE: dirs.files,
        FAULTLINE_STATE: dirs.state,
        FAULTLINE_PORT_FILE: dirs.portFile,
      }),
      stdoutPath: join(plan.dir, "sut.stdout.log"),
      stderrPath: join(plan.dir, "sut.stderr.log"),
    })
    const port = yield* boot(dirs.portFile, sut)
    yield* recorder.openWindow
    yield* Effect.forEach(sessions, (session) => promptSession(recorder, port, session, script.seed), { concurrency: "unbounded", discard: true })
    // ASSERT. The stub's own account comes first: if it could not record or script a request, the
    // history is not a full account of the provider boundary and the world is not judged.
    const problems = yield* stub.problems
    const requests = yield* stub.requests
    const snapshot = yield* snapshotOf(dirs.files)
    const historyText = yield* io("assert", `read ${recorder.path}`, () => readFile(recorder.path, "utf8"))
    if (problems.length > 0) return notAsserted(plan.world, problems)
    const guards: ReadonlyArray<WorldGuard> = requests < plan.profile.minProviderTurns ? ["provider-not-exercised"] : []
    return { outcome: { world: plan.world, reachedAssert: true, verdicts: judgeHistory(plan.oracles, historyText, snapshot), guards }, problems: [] } satisfies WorldResult
  })

const worldFileText = (plan: WorldPlan, script: ScriptParams, sessions: ReadonlyArray<SessionId>): Effect.Effect<string, RunnerError> =>
  Effect.mapError(
    Effect.fromResult(
      Result.gen(function* () {
        const providerScriptDigest = yield* scriptDigestOf(script, sessions)
        const fields = { seed: plan.seed, profile: plan.profileName, planned: [], realized: [], providerScriptDigest, sutCommand: plan.driver.cmd }
        const digest = yield* digestOf(fields)
        const encoded = yield* Schema.encodeResult(WorldFile)({ ...fields, digest })
        return `${JSON.stringify(encoded, null, 2)}\n`
      }),
    ),
    (error) => new RunnerError({ stage: "world-file", detail: String(error) }),
  )

// Never fails: a world that cannot be set up, booted, loaded or asserted is an outcome with
// reachedAssert false, which aggregation turns into world-not-asserted. world.json is written for
// every world that got a directory, whatever happened inside it.
export const runWorld = (plan: WorldPlan): Effect.Effect<WorldResult, never, Clock | Workspace | ProcessControl | ProviderStub> =>
  Effect.gen(function* () {
    const script: ScriptParams = { seed: BigInt(plan.seed), turnsPerSession: plan.profile.turnsPerSession }
    const sessions = Array.from({ length: plan.profile.sessions }, (_, index) => sessionIdOf(index + 1))
    const made = yield* Effect.result(io("world-file", `create ${plan.dir}`, () => mkdir(plan.dir, { recursive: true })))
    if (Result.isFailure(made)) return notAsserted(plan.world, [made.failure.detail])
    const attempt = yield* Effect.result(Effect.scoped(liveWorld(plan, script, sessions)))
    const result = Result.isSuccess(attempt) ? attempt.success : notAsserted(plan.world, [describe(attempt.failure)])
    const written = yield* Effect.result(Effect.flatMap(worldFileText(plan, script, sessions), (text) => io("world-file", "write world.json", () => writeFile(join(plan.dir, "world.json"), text))))
    return Result.isSuccess(written) ? result : notAsserted(plan.world, [...result.problems, written.failure.detail])
  })
