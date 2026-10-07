import { describe, expect, test } from "bun:test"
import { Arbitrary, Result, Schema } from "effect"
import { canonicalJson, digestOf, sha256Hex } from "../src/canonical"
import { isRecord, reverseKeys, type Json } from "./support/json"
import { assertProperty, sampleOf } from "./support/property"

type Path = ReadonlyArray<string | number>

const digest = (value: unknown): string => Result.getOrThrow(digestOf(value))

const pathsOf = (value: Json): ReadonlyArray<Path> =>
  Array.isArray(value)
    ? [[], ...value.flatMap((item, index) => pathsOf(item).map((path) => [index, ...path]))]
    : isRecord(value)
      ? [[], ...Object.entries(value).flatMap(([key, item]) => pathsOf(item).map((path) => [key, ...path]))]
      : [[]]

const nodesOf = (value: Json): ReadonlyArray<Json> =>
  Array.isArray(value)
    ? [value, ...value.flatMap(nodesOf)]
    : isRecord(value)
      ? [value, ...Object.values(value).flatMap(nodesOf)]
      : [value]

const updateAt = (value: Json, path: Path, change: (node: Json) => Json): Json => {
  const [head, ...rest] = path
  if (head === undefined) return change(value)
  if (Array.isArray(value) && typeof head === "number")
    return value.map((item, index) => (index === head ? updateAt(item, rest, change) : item))
  if (isRecord(value) && typeof head === "string")
    return Object.fromEntries(Object.entries(value).map(([key, item]) => [key, key === head ? updateAt(item, rest, change) : item]))
  return value
}

// Changes one node so that the result is a different JSON value. An added object key is longer than
// every existing key, so it cannot collide with one.
const alter = (node: Json): Json => {
  if (node === null) return false
  if (typeof node === "boolean") return !node
  if (typeof node === "number") return node === 0 ? 1 : 0
  if (typeof node === "string") return `${node}x`
  if (Array.isArray(node)) return [...node, null]
  return { ...node, [`${Object.keys(node).join("|")}!`]: null }
}

const jsonArbitrary = Arbitrary.schema(Schema.Json)
const decodeJsonText = Schema.decodeUnknownResult(Schema.fromJsonString(Schema.Json))

describe("canonicalJson", () => {
  test("sorts keys recursively and writes no whitespace", () => {
    expect(canonicalJson({ b: 1, a: { d: [1, 2], c: null } })).toEqual(Result.succeed('{"a":{"c":null,"d":[1,2]},"b":1}'))
  })

  test("distinguishes values that a careless serializer would merge", () => {
    const vectors: ReadonlyArray<readonly [unknown, unknown]> = [
      [1, "1"],
      [null, "null"],
      [[], {}],
      [{ a: null }, {}],
      [{ a: "b,\"c\":\"d" }, { a: "b", c: "d" }],
      [[1, 2], [12]],
      [{ a: [1] }, { a: 1 }],
      [true, "true"],
    ]
    vectors.forEach(([left, right]) => expect(digest(left)).not.toEqual(digest(right)))
  })

  test("a different insertion order and different whitespace give the same digest", () => {
    const compact = decodeJsonText('{"b":[1,{"y":2,"x":1}],"a":true}')
    const spaced = decodeJsonText('{\n  "a" : true,\n  "b" : [ 1 , { "x" : 1 , "y" : 2 } ]\n}')
    expect(Result.isSuccess(compact) && Result.isSuccess(spaced)).toBe(true)
    expect(digest(Result.getOrThrow(compact))).toEqual(digest(Result.getOrThrow(spaced)))
  })

  test("refuses values that are not JSON instead of dropping them", () => {
    const sparse: Array<number> = []
    sparse[1] = 1
    const refused: ReadonlyArray<unknown> = [
      undefined,
      { a: undefined },
      [undefined],
      Number.NaN,
      Number.POSITIVE_INFINITY,
      1n,
      new Map([["a", 1]]),
      new Set([1]),
      new Date(0),
      Symbol("s"),
      () => 1,
      { [Symbol("k")]: 1 },
      sparse,
      new (class Point { readonly x = 1 })(),
    ]
    refused.forEach((value) => expect(Result.isFailure(canonicalJson(value))).toBe(true))
  })

  test("names the path of the value it refused", () => {
    const refusal = canonicalJson({ outer: [{ ok: 1 }, { bad: Number.NaN }] })
    expect(Result.isFailure(refusal) ? [refusal.failure.reason, refusal.failure.path] : []).toEqual([
      "non-finite-number",
      "$.outer[1].bad",
    ])
  })
})

describe("sha256Hex", () => {
  test("matches the published SHA-256 vector for abc", () => {
    const hex: string = Result.getOrThrow(sha256Hex("abc"))
    expect(hex).toEqual("ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad")
  })

  test("hashes UTF-8 bytes, so two different strings do not share a digest", () => {
    expect(digest("é")).not.toEqual(digest("e"))
    expect(digest("\ud800")).not.toEqual(digest("\ufffd"))
  })
})

describe("canonical digest properties", () => {
  test("the generator is not vacuous: it produces nested objects, arrays and every scalar kind", async () => {
    const sample = await sampleOf(jsonArbitrary, 400)
    const kinds = new Set(sample.flatMap(nodesOf).map((node) => (node === null ? "null" : Array.isArray(node) ? "array" : typeof node)))
    expect(sample.some((value) => isRecord(value) && Object.keys(value).length > 1)).toBe(true)
    expect(sample.some((value) => Array.isArray(value) && value.length > 1)).toBe(true)
    expect(sample.filter((value) => pathsOf(value).length > 3).length).toBeGreaterThan(20)
    expect([...kinds].toSorted()).toEqual(["array", "boolean", "null", "number", "object", "string"])
  })

  test("the digest is invariant under key order and whitespace", async () => {
    await assertProperty("key order and whitespace", jsonArbitrary, (value) => {
      const reordered = reverseKeys(value)
      const reparsed = decodeJsonText(JSON.stringify(value, null, 2))
      return (
        digest(reordered) === digest(value) &&
        Result.isSuccess(reparsed) &&
        digest(reparsed.success) === digest(value)
      )
    })
  })

  test("the canonical text is a fixed point: decoding it and canonicalising again changes nothing", async () => {
    await assertProperty("fixed point", jsonArbitrary, (value) => {
      const text = Result.getOrThrow(canonicalJson(value))
      const decoded = decodeJsonText(text)
      return Result.isSuccess(decoded) && Result.getOrThrow(canonicalJson(decoded.success)) === text
    })
  })

  test("changing any one node always changes the digest", async () => {
    const arbitrary = Arbitrary.all([jsonArbitrary, Arbitrary.schema(Schema.Int)])
    await assertProperty("any change", arbitrary, ([value, choice]) => {
      const paths = pathsOf(value)
      const path = paths[Math.abs(choice) % paths.length] ?? []
      return digest(updateAt(value, path, alter)) !== digest(value)
    })
  })
})
