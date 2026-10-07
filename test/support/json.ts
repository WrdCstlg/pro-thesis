import { Result, Schema } from "effect"

export type Json = Schema.Json

export const isRecord = (value: Json): value is { readonly [key: string]: Json } =>
  typeof value === "object" && value !== null && !Array.isArray(value)

// Reverses the key order of every object. Integer-like keys keep ascending order in JavaScript
// whatever the insertion order, so some objects cannot be reordered; callers rely only on the
// reordering never changing the value.
export const reverseKeys = (value: Json): Json =>
  Array.isArray(value)
    ? value.map(reverseKeys)
    : isRecord(value)
      ? Object.fromEntries(Object.entries(value).reverse().map(([key, item]) => [key, reverseKeys(item)]))
      : value

// Clones a value through JSON so a test can reorder keys and decode again without JSON.parse.
export const jsonOf = (value: unknown): Json =>
  Result.getOrThrow(Schema.decodeUnknownResult(Schema.fromJsonString(Schema.Json))(JSON.stringify(value)))