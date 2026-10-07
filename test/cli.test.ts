import { afterAll, describe, expect, test } from "bun:test"
import { existsSync } from "node:fs"
import { mkdir, mkdtemp, readdir, readFile, rm, writeFile } from "node:fs/promises"
import { tmpdir } from "node:os"
import { join } from "node:path"
import { Result, Schema } from "effect"
import { LockFile, RunVerdict, type FaultlineConfig } from "../src/schema"

// The CLI as a real subprocess, in a throwaway project directory with its own faultline.json, its
// own .faultline/ and its own TMP, so every file it writes and every workspace it leaves can be
// counted. The driver points at the repository's fixture by absolute path.

const ROOT = join(import.meta.dir, "..")
const CLI = join(ROOT, "src", "cli.ts")
const dirs: Array<string> = []
afterAll(() => Promise.all(dirs.map((dir) => rm(dir, { recursive: true, force: true, maxRetries: 10, retryDelay: 100 }))))

const config = (worlds = 1): FaultlineConfig => ({
  driver: { cmd: [process.execPath, join(ROOT, "fixture", "runtime.ts")], envAllow: ["PATH", "SYSTEMROOT", "TEMP", "TMP", "FIXTURE_DEFECTS"] },
  profiles: { small: { worlds, sessions: 2, turnsPerSession: 3, minProviderTurns: 6, quiesceMs: 1000, faults: [], oracles: [] } },
  oracles: { builtin: ["tool-pairing"], external: [] },
  oracle: { timeoutMs: 1000 },
  faults: { allow: [], deny: [] },
  budget: { wallClockMs: 60000 },
})

// Branded fields are plain strings in JSON; this writes the config exactly as a user would.
const withOracles = (base: FaultlineConfig, oracles: ReadonlyArray<string>): Readonly<Record<string, unknown>> => ({
  ...base,
  profiles: { small: { ...base.profiles["small"], oracles } },
})

type Project = { readonly dir: string; readonly tmp: string }

const project = async (contents: unknown = withOracles(config(), ["tool-pairing"])): Promise<Project> => {
  const dir = await mkdtemp(join(tmpdir(), "fl-cli-"))
  dirs.push(dir)
  const tmp = join(dir, "tmp")
  await mkdir(tmp)
  await writeFile(join(dir, "faultline.json"), typeof contents === "string" ? contents : JSON.stringify(contents, null, 2))
  return { dir, tmp }
}

type Ran = { readonly exit: number; readonly stdout: string; readonly stderr: string }

const cli = async (where: Project, args: ReadonlyArray<string>): Promise<Ran> => {
  const env = { ...process.env, TMP: where.tmp, TEMP: where.tmp, TMPDIR: where.tmp, FIXTURE_DEFECTS: "" }
  const proc = Bun.spawn([process.execPath, CLI, ...args], { cwd: where.dir, env, stdin: "ignore", stdout: "pipe", stderr: "pipe" })
  const [exit, stdout, stderr] = await Promise.all([proc.exited, new Response(proc.stdout).text(), new Response(proc.stderr).text()])
  return { exit, stdout, stderr }
}

const runDirs = async (where: Project): Promise<ReadonlyArray<string>> =>
  existsSync(join(where.dir, ".faultline", "runs")) ? (await readdir(join(where.dir, ".faultline", "runs"))).map((name) => join(where.dir, ".faultline", "runs", name)) : []

const verdictOf = async (runDir: string): Promise<RunVerdict> =>
  Result.getOrThrow(Schema.decodeUnknownResult(Schema.fromJsonString(RunVerdict), { onExcessProperty: "error" })(await readFile(join(runDir, "verdict.json"), "utf8")))

const lockText = async (where: Project): Promise<string | undefined> =>
  existsSync(join(where.dir, ".faultline", "lock")) ? readFile(join(where.dir, ".faultline", "lock"), "utf8") : undefined

const LOCK_KEYS = ["driver", "profiles", "oracles.builtin", "oracles.external", "faults.allow", "faults.deny", "budget"]

describe("usage errors exit 64 and write nothing", () => {
  const cases: ReadonlyArray<ReadonlyArray<string>> = [
    [],
    ["frobnicate"],
    ["run"],
    ["run", "--profile"],
    ["run", "--profile", "small", "--profile", "small"],
    ["run", "--profile", "small", "--colour", "red"],
    ["run", "--profile", "small", "--worlds", "1"],
    ["run", "--profile", "small", "--budget", "1"],
    ["run", "--profile", "small", "--fault", "stream.malformed()@turn=1"],
    ["run", "--profile", "small", "--seed", "-1"],
    ["run", "--profile", "small", "--seed", "18446744073709551616"],
    ["run", "--profile", "nope"],
    ["lock"],
    ["lock", "--reason", ""],
    ["lock", "--reason", "   "],
    ["lock", "verify"],
    ["replay", "x", "-k", "1", "--expect", "tool-pairing"],
    ["explain", "x"],
  ]
  cases.forEach((args) => {
    test(`faultline ${args.join(" ") || "(no arguments)"}`, async () => {
      const where = await project()
      const ran = await cli(where, args)
      expect({ exit: ran.exit, stdout: ran.stdout }).toEqual({ exit: 64, stdout: "" })
      expect(ran.stderr).not.toBe("")
      expect(await runDirs(where)).toEqual([])
      expect(await lockText(where)).toBeUndefined()
    })
  })

  test("a config with a key the schema does not know is refused (OQ-008)", async () => {
    const where = await project({ ...config(), profile: {} })
    expect((await cli(where, ["run", "--profile", "small"])).exit).toBe(64)
  })

  test("a config that is not JSON, and a missing config, are refused", async () => {
    const broken = await project("{ not json")
    expect((await cli(broken, ["run", "--profile", "small"])).exit).toBe(64)
    const missing = await project()
    await rm(join(missing.dir, "faultline.json"))
    expect((await cli(missing, ["lock", "--reason", "x"])).exit).toBe(64)
  })

  test("a profile oracle that is not declared, or declared but not built, is refused before anything runs", async () => {
    const undeclared = await project(withOracles(config(), ["edit-integrity"]))
    expect((await cli(undeclared, ["run", "--profile", "small"])).exit).toBe(64)
    const unbuilt = await project({ ...(withOracles(config(), ["edit-integrity"])), oracles: { builtin: ["tool-pairing", "edit-integrity"], external: [] } })
    const ran = await cli(unbuilt, ["run", "--profile", "small"])
    expect(ran.exit).toBe(64)
    expect(ran.stderr).toContain("not built")
    expect(await runDirs(unbuilt)).toEqual([])
  })

  test("a profile that plans faults is refused in this build, and a malformed fault entry too", async () => {
    const planned = await project({ ...config(), profiles: { small: { ...config().profiles["small"], oracles: ["tool-pairing"], faults: ["stream.malformed()@turn=1"] } } })
    expect((await cli(planned, ["run", "--profile", "small"])).exit).toBe(64)
    const malformed = await project({ ...config(), profiles: { small: { ...config().profiles["small"], oracles: ["tool-pairing"], faults: ["stream.nope()@turn=1"] } } })
    expect((await cli(malformed, ["run", "--profile", "small"])).exit).toBe(64)
  })
})

describe("the lock is decided before anything is spawned", () => {
  test("no lock file: exit 4, every key named, a drift verdict and no world started", async () => {
    const where = await project()
    const ran = await cli(where, ["run", "--profile", "small"])
    expect(ran.exit).toBe(4)
    expect(LOCK_KEYS.every((key) => ran.stderr.includes(key))).toBe(true)
    const runs = await runDirs(where)
    expect(runs).toHaveLength(1)
    const verdict = await verdictOf(runs[0] ?? "")
    expect([verdict.lockStatus, verdict.exitCode, verdict.worlds.length]).toEqual(["drift", 4, 0])
    expect(existsSync(join(runs[0] ?? "", "worlds"))).toBe(false)
    expect(await readdir(where.tmp)).toEqual([])
  })

  test("after a lock, a changed profile is drift naming profiles and no other key", async () => {
    const where = await project()
    expect((await cli(where, ["lock", "--reason", "first lock"])).exit).toBe(0)
    await writeFile(join(where.dir, "faultline.json"), JSON.stringify({ ...(withOracles(config(), ["tool-pairing"])), profiles: { small: { ...config().profiles["small"], oracles: ["tool-pairing"], sessions: 3 } } }))
    const ran = await cli(where, ["run", "--profile", "small"])
    expect(ran.exit).toBe(4)
    expect(LOCK_KEYS.filter((key) => `${ran.stdout}\n${ran.stderr}`.includes(key))).toEqual(["profiles"])
  })

  test("a lock file that does not decode is drift for run, and lock refuses to overwrite it", async () => {
    const where = await project()
    await mkdir(join(where.dir, ".faultline"))
    await writeFile(join(where.dir, ".faultline", "lock"), "{ corrupt")
    expect((await cli(where, ["run", "--profile", "small"])).exit).toBe(4)
    expect((await cli(where, ["lock", "--reason", "try to paper over it"])).exit).toBe(4)
    expect(await lockText(where)).toBe("{ corrupt")
  })

  test("lock records every key with the reason on first lock, and nothing on a re-lock with no change", async () => {
    const where = await project()
    expect((await cli(where, ["lock", "--reason", "first lock"])).exit).toBe(0)
    const first = Result.getOrThrow(Schema.decodeUnknownResult(Schema.fromJsonString(LockFile))((await lockText(where)) ?? ""))
    expect(first.history.map((entry) => [entry.key, entry.from, entry.reason])).toEqual(LOCK_KEYS.map((key) => [key, null, "first lock"]))
    expect((await cli(where, ["lock", "--reason", "again"])).exit).toBe(0)
    const second = Result.getOrThrow(Schema.decodeUnknownResult(Schema.fromJsonString(LockFile))((await lockText(where)) ?? ""))
    expect(second).toEqual(first)
  })
})

describe("a locked run", () => {
  test("the clean fixture passes with one verdict per planned world, a lock status of ok and no workspace left in TMP", async () => {
    const where = await project(withOracles(config(2), ["tool-pairing"]))
    expect((await cli(where, ["lock", "--reason", "first lock"])).exit).toBe(0)
    const ran = await cli(where, ["run", "--profile", "small", "--seed", "7"])
    expect(ran.exit).toBe(0)
    const runs = await runDirs(where)
    expect(runs).toHaveLength(1)
    const verdict = await verdictOf(runs[0] ?? "")
    expect([verdict.lockStatus, verdict.exitCode]).toEqual(["ok", 0])
    expect(verdict.worlds.map((world) => `${world.world}:${world.verdicts.map((v) => v._tag).join(",")}`)).toEqual(["world-1:Ok", "world-2:Ok"])

    expect([verdict.violations, verdict.inconclusive]).toEqual([[], []])
    expect(verdict.bunVersion).toBe(Bun.version)
    expect(await readdir(join(runs[0] ?? "", "worlds"))).toEqual(["world-1", "world-2"])
    expect(await readdir(where.tmp)).toEqual([])
  }, 60_000)

  test("the same seed gives the same world seeds and provider script digests", async () => {
    const where = await project()
    expect((await cli(where, ["lock", "--reason", "first lock"])).exit).toBe(0)
    expect((await cli(where, ["run", "--profile", "small", "--seed", "99"])).exit).toBe(0)
    expect((await cli(where, ["run", "--profile", "small", "--seed", "99"])).exit).toBe(0)
    const worlds = await Promise.all((await runDirs(where)).map((dir) => readFile(join(dir, "worlds", "world-1", "world.json"), "utf8")))
    expect(worlds).toHaveLength(2)
    expect(worlds[0]).toBe(worlds[1] ?? "")
  }, 60_000)

  test("a profile with an empty oracle list is INCONCLUSIVE with no-oracle-judged, never PASS", async () => {
    const where = await project(withOracles(config(), []))
    expect((await cli(where, ["lock", "--reason", "first lock"])).exit).toBe(0)
    const ran = await cli(where, ["run", "--profile", "small"])
    expect(ran.exit).toBe(2)
    const verdict = await verdictOf((await runDirs(where))[0] ?? "")
    expect(verdict.inconclusive.map((entry) => entry.reason)).toEqual(["no-oracle-judged"])
  }, 60_000)

  test("a SUT that cannot boot is INCONCLUSIVE with world-not-asserted, never PASS", async () => {
    const where = await project({ ...(withOracles(config(), ["tool-pairing"])), driver: { cmd: [process.execPath, "-e", "process.exit(9)"], envAllow: [] } })
    expect((await cli(where, ["lock", "--reason", "first lock"])).exit).toBe(0)
    const ran = await cli(where, ["run", "--profile", "small"])
    expect(ran.exit).toBe(2)
    const verdict = await verdictOf((await runDirs(where))[0] ?? "")
    expect(verdict.inconclusive.map((entry) => entry.reason)).toEqual(["world-not-asserted"])
    expect(ran.stdout).toContain("exit 9")
  }, 60_000)
})
