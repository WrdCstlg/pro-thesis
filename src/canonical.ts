import { Data, Result, Schema } from "effect"
import { Sha256 } from "./schema"

export class CanonicalError extends Data.TaggedError("CanonicalError")<{
  readonly path: string
  readonly reason: "non-finite-number" | "unsupported-value" | "digest-malformed"
}> {}

// Code-unit order, which is what the default string `<` gives. Keys inside one object are unique,
// so this comparator is a total order and the sorted key sequence cannot depend on insertion order.
const compareKeys = (a: string, b: string): number => (a < b ? -1 : a > b ? 1 : 0)

const unsupported = (path: string): Result.Result<string, CanonicalError> =>
  Result.fail(new CanonicalError({ path, reason: "unsupported-value" }))

// Only JSON values are canonicalised. Anything else (undefined, bigint, functions, symbols, Map,
// Set, Date, class instances, symbol-keyed objects, sparse-array holes) is refused rather than
// dropped: JSON.stringify would silently turn a Map into {} and two different configs would then
// share a digest.
const render = (value: unknown, path: string): Result.Result<string, CanonicalError> => {
  if (value === null) return Result.succeed("null")
  if (typeof value === "boolean") return Result.succeed(value ? "true" : "false")
  if (typeof value === "string") return Result.succeed(JSON.stringify(value))
  if (typeof value === "number")
    return Number.isFinite(value)
      ? Result.succeed(JSON.stringify(value))
      : Result.fail(new CanonicalError({ path, reason: "non-finite-number" }))
  if (Array.isArray(value))
    return Result.map(
      Result.all(Array.from(value, (item: unknown, index) => render(item, `${path}[${index}]`))),
      (parts) => `[${parts.join(",")}]`,
    )
  if (typeof value !== "object") return unsupported(path)
  const prototype = Object.getPrototypeOf(value)
  if ((prototype !== Object.prototype && prototype !== null) || Object.getOwnPropertySymbols(value).length > 0) return unsupported(path)
  return Result.map(
    Result.all(
      Object.entries(value)
        .toSorted(([a], [b]) => compareKeys(a, b))
        .map(([key, item]) => Result.map(render(item, `${path}.${key}`), (rendered) => `${JSON.stringify(key)}:${rendered}`)),
    ),
    (members) => `{${members.join(",")}}`,
  )
}

// JSON with keys sorted recursively and no insignificant whitespace. UTF-8 happens at digest time.
export const canonicalJson = (value: unknown): Result.Result<string, CanonicalError> => render(value, "$")

// Bun.CryptoHasher hashes a string as UTF-8. JSON.stringify escapes lone surrogates, so the canonical
// text is well-formed and no two distinct strings collapse to the same bytes.
export const sha256Hex = (text: string): Result.Result<Sha256, CanonicalError> =>
  Result.mapError(
    Schema.decodeUnknownResult(Sha256)(new Bun.CryptoHasher("sha256").update(text).digest("hex")),
    () => new CanonicalError({ path: "$", reason: "digest-malformed" }),
  )

export const digestOf = (value: unknown): Result.Result<Sha256, CanonicalError> => Result.flatMap(canonicalJson(value), sha256Hex)
