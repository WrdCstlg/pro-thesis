import { Result, Schema } from "effect"
import { MessageId, OpId, OracleName, SessionId, Sha256, ToolCallId, WorldId } from "../../src/schema"

// Test-only constructors. The ids are branded, so a test cannot pass a raw string where an id is
// expected; getOrThrow is acceptable here because a malformed literal is a bug in the test itself.
export const opId = (n: number): OpId => Result.getOrThrow(Schema.decodeUnknownResult(OpId)(`op-${n}`))
export const sessionId = (n: number): SessionId => Result.getOrThrow(Schema.decodeUnknownResult(SessionId)(`session-${n}`))
export const worldId = (n: number): WorldId => Result.getOrThrow(Schema.decodeUnknownResult(WorldId)(`world-${n}`))
export const oracleName = (name: string): OracleName => Result.getOrThrow(Schema.decodeUnknownResult(OracleName)(name))
export const messageId = (id: string): MessageId => Result.getOrThrow(Schema.decodeUnknownResult(MessageId)(id))
export const toolCallId = (hex: string): ToolCallId => Result.getOrThrow(Schema.decodeUnknownResult(ToolCallId)(`call_${hex}`))
export const sha256 = (hex: string): Sha256 => Result.getOrThrow(Schema.decodeUnknownResult(Sha256)(hex))
