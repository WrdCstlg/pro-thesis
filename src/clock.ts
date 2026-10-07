import { Context, Effect, Layer } from "effect"

// The only module that reads time. Everything else asks this service, so a test can see every
// time-dependent decision through one seam and src/ has exactly one place where the banned
// time APIs appear.
export class Clock extends Context.Service<
  Clock,
  {
    // Milliseconds on a monotonic clock with an arbitrary origin. Used for history `t` and for
    // deadlines; never compared across processes.
    readonly monotonicMs: Effect.Effect<number>
    // Wall-clock milliseconds since the epoch. Used only for run ids, default seeds and lock `at`.
    readonly wallMs: Effect.Effect<number>
    readonly isoNow: Effect.Effect<string>
  }
>()("faultline/Clock") {}

export const ClockLive = Layer.succeed(Clock, {
  monotonicMs: Effect.sync(() => performance.now()),
  wallMs: Effect.sync(() => Date.now()),
  isoNow: Effect.sync(() => new Date().toISOString()),
})
