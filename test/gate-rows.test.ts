import { afterAll, beforeAll, describe, expect, test } from "bun:test"
import { mkdir, rm } from "node:fs/promises"
import { join } from "node:path"
import { Effect, Result } from "effect"

// These tests run the real, frozen scripts/gate.ts against a throwaway project whose fake CLI
// misbehaves in one known way per acceptance row. They exist because the gate's row checks are the
// only thing standing between a CLI that exits 1 for the wrong reason and a green row, and a
// detector that is never shown to fire is not evidence of anything (rule I, DECISIONS D-014/D-015).
// FAULTLINE_GATE_SOURCE lets a mutation run point the fixture at a deliberately broken gate copy
// without touching the frozen file.

const REPO = join(import.meta.dir, "..")
const GATE_SOURCE = process.env["FAULTLINE_GATE_SOURCE"] ?? join(REPO, "scripts", "gate.ts")
const FIXTURE = join(REPO, ".gate", `fixture-${process.pid}`)
const sha256 = (text: string): string => new Bun.CryptoHasher("sha256").update(text).digest("hex")

const CLI_SOURCE = `import { mkdirSync, writeFileSync } from "node:fs"
import { tmpdir } from "node:os"
import { join } from "node:path"

const profile = process.argv[4] ?? ""
const okWorld = [{ world: "w1", verdicts: [{ _tag: "Ok", oracle: "tool-pairing" }] }]
const finish = (processExit: number, verdictExit: number, worlds: ReadonlyArray<object>): never => {
  const dir = join(process.cwd(), ".faultline", "runs", "run-" + profile)
  mkdirSync(dir, { recursive: true })
  writeFileSync(join(dir, "verdict.json"), JSON.stringify({ exitCode: verdictExit, lockStatus: "ok", worlds, violations: [], inconclusive: [] }))
  return process.exit(processExit)
}
if (profile === "clean") finish(0, 0, okWorld)
if (profile === "no-verdict") process.exit(1)
if (profile === "mismatch") finish(1, 0, okWorld)
if (profile === "vacuous") finish(0, 0, [{ world: "w1", verdicts: [] }])
if (profile === "leak-process") {
  const child = Bun.spawn([process.execPath, "-e", "setInterval(() => {}, 1000)"], { stdio: ["ignore", "ignore", "ignore"], detached: true })
  child.unref()
  writeFileSync(join(process.cwd(), "leak.pid"), String(child.pid))
  finish(0, 0, okWorld)
}
if (profile === "leak-workspace") {
  mkdirSync(join(tmpdir(), "faultline-leak"), { recursive: true })
  finish(0, 0, okWorld)
}
process.exit(64)
`

const row = (id: string, profile: string, expectedExit: number) => ({
  id,
  due: "M0",
  args: ["run", "--profile", profile],
  env: {},
  expectedExit,
})

const ROWS = [
  row("A01", "clean", 0),
  row("A02", "no-verdict", 1),
  row("A03", "mismatch", 1),
  row("A04", "vacuous", 0),
  row("A05", "leak-process", 0),
  row("A06", "leak-workspace", 0),
]

const PROFILES = Object.fromEntries(["clean", "vacuous", "leak-process", "leak-workspace"].map((name) => [name, { worlds: 1 }]))

const isAlive = (pid: number): boolean =>
  Result.isSuccess(Effect.runSync(Effect.result(Effect.try({ try: () => process.kill(pid, 0), catch: (cause) => cause }))))

const forceKill = (pid: number): boolean =>
  Result.isSuccess(Effect.runSync(Effect.result(Effect.try({ try: () => process.kill(pid, "SIGKILL"), catch: (cause) => cause }))))

const output = { text: "", leakedPid: 0 }

const statusOf = (id: string): string | undefined =>
  new RegExp(`^${id} \\|.*\\| (PASS|FAIL|PENDING)\\s*$`, "m").exec(output.text)?.[1]

const detailOf = (id: string): string =>
  new RegExp(`^${id} \\((?:PASS|FAIL|PENDING)\\):\\n((?:  .*(?:\\n|$))*)`, "m").exec(output.text)?.[1] ?? ""

beforeAll(async () => {
  await rm(FIXTURE, { recursive: true, force: true })
  await mkdir(join(FIXTURE, "scripts"), { recursive: true })
  await mkdir(join(FIXTURE, "src"), { recursive: true })
  const gateText = await Bun.file(GATE_SOURCE).text()
  const acceptanceText = JSON.stringify(ROWS, null, 2)
  await Promise.all([
    Bun.write(join(FIXTURE, "scripts", "gate.ts"), gateText),
    Bun.write(join(FIXTURE, "acceptance.json"), acceptanceText),
    Bun.write(join(FIXTURE, "src", "cli.ts"), CLI_SOURCE),
    Bun.write(join(FIXTURE, "MILESTONE"), "M0\n"),
    Bun.write(join(FIXTURE, "faultline.json"), JSON.stringify({ profiles: PROFILES })),
    Bun.write(join(FIXTURE, "package.json"), JSON.stringify({ name: "gate-fixture", private: true, type: "module", scripts: { typecheck: "bun --version" } })),
    Bun.write(join(FIXTURE, "GATE.lock"), JSON.stringify({ "acceptance.json": sha256(acceptanceText), "scripts/gate.ts": sha256(gateText) })),
  ])
  const proc = Bun.spawn([process.execPath, "scripts/gate.ts"], { cwd: FIXTURE, stdin: "ignore", stdout: "pipe", stderr: "pipe" })
  const [stdout, stderr] = await Promise.all([new Response(proc.stdout).text(), new Response(proc.stderr).text(), proc.exited])
  output.text = `${stdout}\n${stderr}`
  const pidText = await Bun.file(join(FIXTURE, "leak.pid")).text()
  output.leakedPid = Number(pidText.trim())
}, 240_000)

afterAll(async () => {
  if (Number.isInteger(output.leakedPid) && output.leakedPid > 0 && isAlive(output.leakedPid)) forceKill(output.leakedPid)
  await rm(FIXTURE, { recursive: true, force: true })
})

describe("gate row checks, run against a misbehaving fake CLI", () => {
  test("liveness helper: this process is alive, a nonexistent pid is not", () => {
    expect(isAlive(process.pid)).toBe(true)
    expect(isAlive(2_000_000_000)).toBe(false)
  })

  test("the fixture gate ran to its acceptance table", () => {
    expect(output.text).toContain("row | due | expected | actual | status")
  })

  test("A01 control: a CLI that behaves is the only row that passes", () => {
    expect([statusOf("A01"), statusOf("A02"), statusOf("A03"), statusOf("A04"), statusOf("A05"), statusOf("A06")]).toEqual([
      "PASS",
      "FAIL",
      "FAIL",
      "FAIL",
      "FAIL",
      "FAIL",
    ])
  })

  test("A02: an exit code that matches by coincidence, with no verdict.json, fails the row", () => {
    expect(detailOf("A02")).toContain("expected exactly one new verdict.json, found 0")
    expect(detailOf("A02")).not.toContain("exit 1, expected")
  })

  test("A03: a verdict.json whose exitCode disagrees with the process exit fails the row", () => {
    expect(detailOf("A03")).toContain("verdict.json exitCode 0 differs from process exit 1")
    expect(detailOf("A03")).not.toContain("exit 1, expected")
  })

  test("A04: a PASS in which a world was judged by no oracle fails the row", () => {
    expect(detailOf("A04")).toContain("world w1 has no oracle verdict on a PASS")
    expect(detailOf("A04")).not.toContain("exit 0, expected")
  })

  test("A05: a surviving process fails the row, and the gate kills it", () => {
    expect(output.leakedPid).toBeGreaterThan(0)
    expect(detailOf("A05")).toContain("leftover processes after exit (killed by the gate)")
    expect(detailOf("A05")).toContain(String(output.leakedPid))
    expect(isAlive(output.leakedPid)).toBe(false)
  })

  test("A06: a workspace left in TMP fails the row", () => {
    expect(detailOf("A06")).toContain("workspaces left in TMP: faultline-leak")
  })
})
