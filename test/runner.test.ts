import { afterAll, describe, expect, test } from "bun:test"
import { mkdtemp, readdir, readFile, rm } from "node:fs/promises"
import { tmpdir } from "node:os"
import { join } from "node:path"
import { Effect, Layer, Result, Schema } from "effect"
import { digestOf } from "../src/canonical"
import { ClockLive } from "../src/clock"
import { decodeHistory } from "../src/history"
import { toolPairing } from "../src/oracle/builtin/tool-pairing"
import { ProviderStubLive } from "../src/provider/stub"
import { driverEnvOf, judgeHistory, ProcessControl, ProcessControlLive, runWorld, workspaceLive, type WorldPlan, type WorldResult } from "../src/runner"
import { builtinOracleName, Seed, WorldFile, worldIdOf, type Profile } from "../src/schema"

const ROOT = join(import.meta.dir, "..")
const dirs: Array<string> = []
afterAll(() => Promise.all(dirs.map((dir) => rm(dir, { recursive: true, force: true, maxRetries: 10, retryDelay: 100 }))))

const freshDir = async (prefix: string): Promise<string> => {
  const dir = await mkdtemp(join(tmpdir(), prefix))
  dirs.push(dir)
  return dir
}

const profile: Profile = { worlds: 1, sessions: 2, turnsPerSession: 3, minProviderTurns: 6, quiesceMs: 1000, faults: [], oracles: [builtinOracleName("tool-pairing")] }
const fixtureDriver = { cmd: [process.execPath, "fixture/runtime.ts"] as const, envAllow: ["PATH", "SYSTEMROOT", "TEMP", "TMP", "FIXTURE_DEFECTS"] }

const isAlive = (pid: number): boolean => Result.isSuccess(Effect.runSync(Effect.result(Effect.try({ try: () => process.kill(pid, 0), catch: () => "gone" }))))

type Ran = { readonly result: WorldResult; readonly base: string; readonly dir: string; readonly pids: ReadonlyArray<number> }

// Runs one world on the live layers. The workspace base is a fresh directory so that leftovers can
// be counted, and ProcessControl is the live implementation with each spawned pid noted on the way
// through, so the test can ask the OS afterwards whether that process still exists.
const runOne = async (overrides: Partial<WorldPlan> = {}): Promise<Ran> => {
  const base = await freshDir("fl-ws-")
  const runDir = await freshDir("fl-run-")
  const dir = join(runDir, "worlds", "world-1")
  const pids: Array<number> = []
  const noting = Layer.effect(
    ProcessControl,
    Effect.map(
      Effect.provide(
        Effect.gen(function* () {
          return yield* ProcessControl
        }),
        ProcessControlLive,
      ),
      (live) => ({
        spawn: (request: Parameters<typeof live.spawn>[0]) => Effect.tap(live.spawn(request), (spawned) => Effect.sync(() => pids.push(spawned.pid))),
      }),
    ),
  )
  const plan: WorldPlan = {
    world: worldIdOf(1),
    dir,
    seed: Result.getOrThrow(Schema.decodeUnknownResult(Seed)("12345")),
    profileName: "test",
    profile,
    driver: fixtureDriver,
    projectDir: ROOT,
    inheritedEnv: process.env,
    oracles: [{ name: builtinOracleName("tool-pairing"), judge: toolPairing }],
    ...overrides,
  }
  const result = await Effect.runPromise(runWorld(plan).pipe(Effect.provide(Layer.mergeAll(ClockLive, workspaceLive(base), noting, ProviderStubLive))))
  return { result, base, dir, pids }
}

describe("runWorld against the clean reference fixture", () => {
  test("reaches ASSERT, tool-pairing says Ok, and nothing is left behind: no workspace, no live SUT", async () => {
    const ran = await runOne()
    expect(ran.result.problems).toEqual([])
    expect(ran.result.outcome.reachedAssert).toBe(true)
    expect(ran.result.outcome.guards).toEqual([])
    expect(ran.result.outcome.verdicts.map((verdict) => verdict._tag)).toEqual(["Ok"])
    expect(await readdir(ran.base)).toEqual([])
    expect(ran.pids).toHaveLength(1)
    expect(ran.pids.filter(isAlive)).toEqual([])
  })

  test("the history on disk decodes and shows 2 x 3 provider turns, 2 x 2 tool calls and the prompts completed ok", async () => {
    const ran = await runOne()
    const history = Result.getOrThrow(decodeHistory(await readFile(join(ran.dir, "history.jsonl"), "utf8")))
    const count = (f: string, type: string) => history.filter((event) => event.f === f && event.type === type).length
    expect([count("provider-turn", "invoke"), count("provider-turn", "ok")]).toEqual([6, 6])
    expect([count("tool-call", "invoke"), count("tool-call", "ok")]).toEqual([4, 4])
    expect([count("prompt", "invoke"), count("prompt", "ok")]).toEqual([2, 2])
    // turn k carries k results: 0 + 1 + 2 per session
    expect(count("tool-result", "invoke")).toBe(6)
  })

  test("world.json decodes and its digest is the SHA-256 of the canonical JSON of every other field", async () => {
    const ran = await runOne()
    const world = Result.getOrThrow(Schema.decodeUnknownResult(Schema.fromJsonString(WorldFile), { onExcessProperty: "error" })(await readFile(join(ran.dir, "world.json"), "utf8")))
    const { digest, ...fields } = world
    expect(String(digest)).toBe(String(Result.getOrThrow(digestOf(fields))))
    expect(world.seed).toBe(Result.getOrThrow(Schema.decodeUnknownResult(Seed)("12345")))
    expect(world.sutCommand).toEqual([...fixtureDriver.cmd])
    expect([world.planned, world.realized]).toEqual([[], []])
  })

  test("a stub that saw fewer requests than minProviderTurns adds provider-not-exercised", async () => {
    const ran = await runOne({ profile: { ...profile, minProviderTurns: 7 } })
    expect(ran.result.outcome.reachedAssert).toBe(true)
    expect(ran.result.outcome.guards).toEqual(["provider-not-exercised"])
  })

  test("a profile with no oracles reaches ASSERT with no verdicts, which aggregation turns into no-oracle-judged", async () => {
    const ran = await runOne({ oracles: [] })
    expect(ran.result.outcome.reachedAssert).toBe(true)
    expect(ran.result.outcome.verdicts).toEqual([])
  })
})

describe("runWorld when the world cannot reach ASSERT", () => {
  test("a driver that exits at once: not asserted, the exit is named, world.json still written, nothing left behind", async () => {
    const ran = await runOne({ driver: { cmd: [process.execPath, "-e", "process.exit(7)"], envAllow: ["PATH", "SYSTEMROOT"] } })
    expect(ran.result.outcome.reachedAssert).toBe(false)
    expect(ran.result.outcome.verdicts).toEqual([])
    expect(ran.result.problems.join("\n")).toContain("exit 7")
    expect(await readdir(ran.dir)).toContain("world.json")
    expect(await readdir(ran.base)).toEqual([])
    expect(ran.pids.filter(isAlive)).toEqual([])
  })

  test("FIXTURE_DEFECTS=d1 is refused by this fixture at boot, so the world is not asserted rather than run clean under a defect's name", async () => {
    const ran = await runOne({ inheritedEnv: { ...process.env, FIXTURE_DEFECTS: "d1" } })
    expect(ran.result.outcome.reachedAssert).toBe(false)
    expect(ran.result.problems.join("\n")).toContain("exit 3")
  })

  test("a driver that never announces a port is killed after the boot timeout and the world is not asserted", async () => {
    const ran = await runOne({ driver: { cmd: [process.execPath, "-e", "setInterval(() => {}, 1000)"], envAllow: ["PATH", "SYSTEMROOT"] } })
    expect(ran.result.outcome.reachedAssert).toBe(false)
    expect(ran.result.problems.join("\n")).toContain("within 15000 ms")
    expect(ran.pids).toHaveLength(1)
    expect(ran.pids.filter(isAlive)).toEqual([])
    expect(await readdir(ran.base)).toEqual([])
  }, 30_000)

  test("a driver command that cannot be spawned is not asserted", async () => {
    const ran = await runOne({ driver: { cmd: [join(ROOT, "no-such-executable-here")], envAllow: [] } })
    expect(ran.result.outcome.reachedAssert).toBe(false)
    expect(ran.result.problems).toHaveLength(1)
  })

  // Boots, answers /health, and on each prompt sends the provider one request with no session tag,
  // which the stub refuses and cannot attribute. The history is then not a full account of the
  // provider boundary, so the world must not be judged, even though tool-pairing would say Ok.
  test("a request the stub could not attribute leaves the world not asserted, with the stub's problem named", async () => {
    const stray = [
      "const server = Bun.serve({ port: 0, hostname: '127.0.0.1', fetch: (request) => new URL(request.url).pathname === '/health'",
      "  ? new Response('ok')",
      "  : fetch(process.env.PROVIDER_BASE_URL + '/v1/chat/completions', { method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify({ messages: [{ role: 'user', content: 'no tag' }] }) })",
      "      .then((answer) => answer.text()).then(() => new Response('{}')) })",
      "Bun.write(process.env.FAULTLINE_PORT_FILE, JSON.stringify({ port: server.port }))",
    ].join("\n")
    const ran = await runOne({ driver: { cmd: [process.execPath, "-e", stray], envAllow: ["PATH", "SYSTEMROOT"] } })
    expect(ran.result.outcome.reachedAssert).toBe(false)
    expect(ran.result.outcome.verdicts).toEqual([])
    expect(ran.result.problems.join("\n")).toContain("session could not be identified")
    expect(ran.pids.filter(isAlive)).toEqual([])
    expect(await readdir(ran.base)).toEqual([])
  })
})

describe("driverEnvOf", () => {
  test("passes only allow-listed names, matched without regard to case, and the launch variables win", () => {
    const env = driverEnvOf(["PATH", "FIXTURE_DEFECTS"], { Path: "p", SECRET: "s", fixture_defects: "d1", PROVIDER_BASE_URL: "inherited", UNSET: undefined }, { PROVIDER_BASE_URL: "http://x" })
    expect(env).toEqual({ Path: "p", fixture_defects: "d1", PROVIDER_BASE_URL: "http://x" })
  })

  test("an allow-list entry spelled in lower or mixed case matches the inherited name in any case", () => {
    expect(driverEnvOf(["path", "Temp"], { PATH: "p", TEMP: "t", OTHER: "o" }, {})).toEqual({ PATH: "p", TEMP: "t" })
  })
})

describe("judgeHistory", () => {
  test("a history file that does not decode gives every oracle history-malformed, never Ok", () => {
    const oracles = [{ name: builtinOracleName("tool-pairing"), judge: toolPairing }]
    const verdicts = judgeHistory(oracles, "not json\n", { files: {} })
    expect(verdicts).toEqual([{ _tag: "Inconclusive", oracle: builtinOracleName("tool-pairing"), reason: "history-malformed" }])
  })

  test("an empty history file decodes to no events and is judged", () => {
    const verdicts = judgeHistory([{ name: builtinOracleName("tool-pairing"), judge: toolPairing }], "", { files: {} })
    expect(verdicts.map((verdict) => verdict._tag)).toEqual(["Ok"])
  })
})
