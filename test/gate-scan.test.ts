import { describe, expect, test } from "bun:test"
import { findBanned, stripCommentsAndStrings } from "../scripts/gate"

// The scan is the gate's cheapest defence, and a scanner that matches nothing passes everything.
// Each banned pattern gets a positive sample (must be found) and the stripper gets negative samples
// (banned text inside comments and strings must not be found).

const labelsIn = (file: string, source: string): ReadonlyArray<string> => findBanned(file, source).map((finding) => finding.label)

describe("banned-pattern scan", () => {
  const positives: ReadonlyArray<readonly [string, string]> = [
    ["try {", "try { run() }"],
    ["catch (", "} catch (e) {}"],
    ["throw", "throw new Error()"],
    ["JSON.parse(", "const v = JSON.parse(text)"],
    ["Math.random(", "const r = Math.random()"],
    [": any", "const f = (x: any) => x"],
    ["<any>", "const xs: Array<any> = []"],
    ["as any", "const y = x as any"],
    ["as unknown as", "const y = x as unknown as Foo"],
    ["as <Identifier>", "const y = x as Foo"],
    ["non-null assertion", "const y = x!.field"],
    ["non-null assertion", "call(x!)"],
    ["else", "if (a) { b() } else { c() }"],
    ["let", "let counter = 0"],
    ["import * as", "import * as fs from 'node:fs'"],
    ["export default", "export default thing"],
    ["Date.now(", "const t = Date.now()"],
    ["performance.now(", "const t = performance.now()"],
    ["new Date(", "const d = new Date()"],
    ["console.", "console.log(1)"],
  ]

  test.each(positives)("finds %s", (label, source) => {
    expect(labelsIn("src/sample.ts", source)).toContain(label)
  })

  test("finds marker words in comments and strings, where they actually occur", () => {
    const source = ["// TO" + "DO later", "const m = 'FIX" + "ME'", "/* X" + "XX */", "// @ts-" + "ignore", "// @ts-" + "expect-error", "// @ts-" + "nocheck", "const s = 'not " + "implemented'", "// place" + "holder", "stub" + "()"].join("\n")
    expect(new Set(labelsIn("src/sample.ts", source))).toEqual(
      new Set(["TO" + "DO", "FIX" + "ME", "X" + "XX", "ts-ignore", "ts-expect-error", "ts-nocheck", "not " + "implemented", "place" + "holder", "stub" + "()"]),
    )
  })

  test("does not flag code-pattern text inside comments or string literals", () => {
    const source = [
      "// try { } catch (e) { throw e } else let x",
      "/* console.log(JSON.parse(x)) as any */",
      "const a = \"try { throw } else let\"",
      "const b = 'Math.random() as Foo'",
      "const c = `Date.now() ${1} new Date()`",
    ].join("\n")
    expect(labelsIn("src/sample.ts", source)).toEqual([])
  })

  test("allows as const, optional chaining, inequality and negation", () => {
    const source = ["const xs = [1, 2] as const", "const v = a?.b", "const n = a !== b", "const m = !ready", "if (!(a && b)) {}"].join("\n")
    expect(labelsIn("src/sample.ts", source)).toEqual([])
  })

  test("exempts clock reads only in src/clock.ts and console only in src/cli.ts", () => {
    expect(labelsIn("src/clock.ts", "const t = Date.now()")).toEqual([])
    expect(labelsIn("src/runner.ts", "const t = Date.now()")).toEqual(["Date.now("])
    expect(labelsIn("src/cli.ts", "console.log(1)")).toEqual([])
    expect(labelsIn("scripts/gate.ts", "console.log(1)")).toEqual(["console."])
  })

  test("stripping preserves line numbers", () => {
    const source = "/* one\ntwo */\nconst a = 1\nlet b = 2"
    expect(stripCommentsAndStrings(source).split("\n").length).toBe(4)
    expect(findBanned("src/sample.ts", source)).toEqual([{ file: "src/sample.ts", line: 4, label: "let" }])
  })

  test("the gate script itself is clean", async () => {
    const source = await Bun.file(`${import.meta.dir}/../scripts/gate.ts`).text()
    expect(findBanned("scripts/gate.ts", source)).toEqual([])
  })
})
