import { Schema } from "effect"

// Every type that crosses a boundary (config, lock file, world file, history line, verdict file,
// external-oracle stdout) is defined here once; TypeScript types are derived with `typeof X.Type`.
// Only OpId's pattern is given by the specification. The patterns of the other ids are fixed here
// and recorded in D-017, so a change to one is a change to a ledger entry.

export const OpId = Schema.String.pipe(Schema.check(Schema.isPattern(/^op-\d+$/)), Schema.brand("OpId"))
export type OpId = typeof OpId.Type

export const SessionId = Schema.String.pipe(Schema.check(Schema.isPattern(/^session-\d+$/)), Schema.brand("SessionId"))
export type SessionId = typeof SessionId.Type

export const WorldId = Schema.String.pipe(Schema.check(Schema.isPattern(/^world-\d+$/)), Schema.brand("WorldId"))
export type WorldId = typeof WorldId.Type

export const RunId = Schema.String.pipe(Schema.check(Schema.isPattern(/^run-[0-9a-z-]+$/)), Schema.brand("RunId"))
export type RunId = typeof RunId.Type

export const Sha256 = Schema.String.pipe(Schema.check(Schema.isPattern(/^[0-9a-f]{64}$/)), Schema.brand("Sha256"))
export type Sha256 = typeof Sha256.Type

export const ToolCallId = Schema.String.pipe(Schema.check(Schema.isPattern(/^call_[0-9a-f]+$/)), Schema.brand("ToolCallId"))
export type ToolCallId = typeof ToolCallId.Type

export const MessageId = Schema.String.pipe(Schema.check(Schema.isNonEmpty()), Schema.brand("MessageId"))
export type MessageId = typeof MessageId.Type

export const OracleName = Schema.String.pipe(Schema.check(Schema.isPattern(/^[a-z0-9][a-z0-9-]*$/)), Schema.brand("OracleName"))
export type OracleName = typeof OracleName.Type

// The seed is a decimal string because JSON has no 64-bit integer. The filter rejects values above
// 2^64 - 1 so that a seed is never silently truncated by BigInt.asUintN in the PRNG, which would
// make two different seeds produce the same world.
const U64_MAX = 18446744073709551615n
const DECIMAL = /^(0|[1-9][0-9]*)$/
export const Seed = Schema.String.pipe(
  Schema.check(Schema.makeFilter((text: string) => DECIMAL.test(text) && BigInt(text) <= U64_MAX)),
  Schema.brand("Seed"),
)
export type Seed = typeof Seed.Type

// Bounds. MAX_PARAM keeps every integer below 10^15, so formatting it as a decimal and parsing it
// back is exact (see schedule.ts).
const MAX_PARAM = 999_999_999_999_999
const NonNegativeInt = Schema.Int.pipe(Schema.check(Schema.isGreaterThanOrEqualTo(0), Schema.isLessThanOrEqualTo(MAX_PARAM)))
const PositiveInt = Schema.Int.pipe(Schema.check(Schema.isGreaterThanOrEqualTo(1), Schema.isLessThanOrEqualTo(MAX_PARAM)))

export const OpKind = Schema.Literals([
  "prompt",
  "resume",
  "interrupt",
  "provider-turn",
  "tool-call",
  "tool-result",
  "fault",
  "crash",
  "restart",
  "probe",
])
export type OpKind = typeof OpKind.Type

export const OpType = Schema.Literals(["invoke", "ok", "fail", "info"])
export type OpType = typeof OpType.Type

export const HistoryProcess = Schema.Union([SessionId, Schema.Literal("harness"), Schema.Literal("stub")])
export type HistoryProcess = typeof HistoryProcess.Type

// `value` is unknown in the specification. It is JSON here because the history is a JSONL file, so
// a value that is not JSON could not be written and read back.
export const HistoryEvent = Schema.Struct({
  id: OpId,
  process: HistoryProcess,
  type: OpType,
  f: OpKind,
  value: Schema.Json,
  t: Schema.Number.pipe(Schema.check(Schema.isFinite(), Schema.isGreaterThanOrEqualTo(0))),
})
export type HistoryEvent = typeof HistoryEvent.Type

export const BuiltinOracleName = Schema.Literals([
  "tool-pairing",
  "edit-integrity",
  "no-provider-retry-after-crash",
  "exactly-once-admission",
  "quiescence",
])
export type BuiltinOracleName = typeof BuiltinOracleName.Type

export const InconclusiveReason = Schema.Literals([
  "oracle-crashed",
  "oracle-nonzero-exit",
  "oracle-timeout",
  "oracle-malformed-output",
  "missing-probe",
  "unsupported-platform",
  "history-unsplittable",
  "no-oracle-judged",
  "world-not-asserted",
  "fault-missed-load",
  "provider-not-exercised",
  "narrowed-run",
  "interrupted",
])
export type InconclusiveReason = typeof InconclusiveReason.Type

export const OracleVerdict = Schema.Union([
  Schema.TaggedStruct("Ok", { oracle: OracleName }),
  Schema.TaggedStruct("Violation", { oracle: OracleName, witness: Schema.NonEmptyArray(OpId), explanation: Schema.String }),
  Schema.TaggedStruct("Inconclusive", { oracle: OracleName, reason: InconclusiveReason }),
])
export type OracleVerdict = typeof OracleVerdict.Type

export const ExitCode = Schema.Literals([0, 1, 2, 3, 4, 64])
export type ExitCode = typeof ExitCode.Type

export const LockStatus = Schema.Literals(["ok", "bypassed", "drift"])
export type LockStatus = typeof LockStatus.Type

export const WorldVerdicts = Schema.Struct({ world: WorldId, verdicts: Schema.Array(OracleVerdict) })
export type WorldVerdicts = typeof WorldVerdicts.Type

export const ViolationRecord = Schema.Struct({
  world: WorldId,
  oracle: OracleName,
  witness: Schema.NonEmptyArray(OpId),
  explanation: Schema.String,
})
export type ViolationRecord = typeof ViolationRecord.Type

export const InconclusiveRecord = Schema.Struct({
  reason: InconclusiveReason,
  world: Schema.optionalKey(WorldId),
  oracle: Schema.optionalKey(OracleName),
})
export type InconclusiveRecord = typeof InconclusiveRecord.Type

// git.head is the literal "unknown" and git.dirty is the literal "unknown" when they cannot be
// read; the specification forbids inventing either.
export const RunVerdict = Schema.Struct({
  runId: RunId,
  faultlineVersion: Schema.String,
  git: Schema.Struct({ head: Schema.String, dirty: Schema.Union([Schema.Boolean, Schema.Literal("unknown")]) }),
  bunVersion: Schema.String,
  platform: Schema.String,
  lockStatus: LockStatus,
  exitCode: ExitCode,
  worlds: Schema.Array(WorldVerdicts),
  violations: Schema.Array(ViolationRecord),
  inconclusive: Schema.Array(InconclusiveRecord),
})
export type RunVerdict = typeof RunVerdict.Type

export const FAULT_KINDS = [
  "stream.disconnect",
  "stream.stall",
  "stream.malformed",
  "stream.http",
  "runtime.crash",
  "client.duplicate",
  "client.conflict",
  "client.interrupt",
] as const

export const FaultKind = Schema.Literals(FAULT_KINDS)
export type FaultKind = typeof FaultKind.Type

export const Fault = Schema.Union([
  Schema.TaggedStruct("stream.disconnect", { afterChunks: PositiveInt }),
  Schema.TaggedStruct("stream.stall", { ms: PositiveInt }),
  Schema.TaggedStruct("stream.malformed", {}),
  Schema.TaggedStruct("stream.http", { status: Schema.Literals([429, 500]), retryAfter: Schema.optionalKey(NonNegativeInt) }),
  Schema.TaggedStruct("runtime.crash", { restartAfter: NonNegativeInt }),
  Schema.TaggedStruct("client.duplicate", {}),
  Schema.TaggedStruct("client.conflict", {}),
  Schema.TaggedStruct("client.interrupt", {}),
])
export type Fault = typeof Fault.Type

export const Trigger = Schema.Union([
  Schema.TaggedStruct("AtTime", { ms: NonNegativeInt }),
  Schema.TaggedStruct("AtTurn", { turn: PositiveInt }),
])
export type Trigger = typeof Trigger.Type

export const ScheduledFault = Schema.Struct({ fault: Fault, trigger: Trigger })
export type ScheduledFault = typeof ScheduledFault.Type

export const RealizedFault = Schema.Struct({
  fault: Fault,
  tMs: Schema.Number.pipe(Schema.check(Schema.isFinite(), Schema.isGreaterThanOrEqualTo(0))),
  turn: Schema.NullOr(PositiveInt),
})
export type RealizedFault = typeof RealizedFault.Type

export const WorldFile = Schema.Struct({
  seed: Seed,
  profile: Schema.String,
  planned: Schema.Array(ScheduledFault),
  realized: Schema.Array(RealizedFault),
  providerScriptDigest: Sha256,
  sutCommand: Schema.NonEmptyArray(Schema.String),
  digest: Sha256,
})
export type WorldFile = typeof WorldFile.Type

export const Profile = Schema.Struct({
  worlds: PositiveInt,
  sessions: PositiveInt,
  turnsPerSession: PositiveInt,
  minProviderTurns: NonNegativeInt,
  quiesceMs: PositiveInt,
  faults: Schema.Array(Schema.String),
  oracles: Schema.Array(OracleName),
})
export type Profile = typeof Profile.Type

export const ExternalOracle = Schema.Struct({ name: OracleName, cmd: Schema.NonEmptyArray(Schema.String) })
export type ExternalOracle = typeof ExternalOracle.Type

export const FaultlineConfig = Schema.Struct({
  driver: Schema.Struct({ cmd: Schema.NonEmptyArray(Schema.String), envAllow: Schema.Array(Schema.String) }),
  profiles: Schema.Record(Schema.String, Profile),
  oracles: Schema.Struct({ builtin: Schema.Array(BuiltinOracleName), external: Schema.Array(ExternalOracle) }),
  oracle: Schema.Struct({ timeoutMs: PositiveInt }),
  faults: Schema.Struct({ allow: Schema.Array(FaultKind), deny: Schema.Array(FaultKind) }),
  budget: Schema.Struct({ wallClockMs: PositiveInt }),
})
export type FaultlineConfig = typeof FaultlineConfig.Type

export const LOCK_KEYS = ["driver", "profiles", "oracles.builtin", "oracles.external", "faults.allow", "faults.deny", "budget"] as const

export const LockKey = Schema.Literals(LOCK_KEYS)
export type LockKey = typeof LockKey.Type

export const LockEntries = Schema.Struct({
  driver: Sha256,
  profiles: Sha256,
  "oracles.builtin": Sha256,
  "oracles.external": Sha256,
  "faults.allow": Sha256,
  "faults.deny": Sha256,
  budget: Sha256,
})
export type LockEntries = typeof LockEntries.Type

// `from` is null when the key had no entry before (the first lock).
export const LockHistoryEntry = Schema.Struct({
  at: Schema.String,
  key: LockKey,
  from: Schema.NullOr(Sha256),
  to: Sha256,
  reason: Schema.String.pipe(Schema.check(Schema.isPattern(/\S/))),
})
export type LockHistoryEntry = typeof LockHistoryEntry.Type

export const LockFile = Schema.Struct({
  version: Schema.Literal(1),
  entries: LockEntries,
  history: Schema.Array(LockHistoryEntry),
})
export type LockFile = typeof LockFile.Type
