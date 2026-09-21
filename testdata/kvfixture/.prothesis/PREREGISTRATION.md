# Pre-registration: Addendum A.9 item 4

**Written and frozen BEFORE the comparison was run.** A benchmark whose test is
chosen after seeing the numbers is not evidence (D-029).

## The test

- **Design**: two arms, 10 independent trials each, seeds 1..10 shared between
  the arms so each seed is used once per arm.
- **Arms**: `--strategy saboteur` versus `--strategy random`. Both are built
  from the same `search.Params`, so they share the fault space, the magnitude
  ladders, the window policy, the `MaxFaults` ceiling, the validator, the
  corpus, the coverage extractor and the world cost. They differ only in how
  they choose.
- **Per-trial budget, fixed in advance**: `--budget 8m --workers 4` against
  `testdata/kvfixture` profile `search` (driver profile `linear`, image variant
  `buggy`).
- **Primary endpoint**: did the trial produce a **`linearizable.kv` violation**
  (the consistency-class stale read the fixture exists to exhibit) within
  budget? A binary outcome per trial.
- **Statistic**: **one-sided Fisher's exact test** on the 2x2 count table
  (arm x found/not-found).
- **Direction, fixed in advance**: Saboteur > random. A result in the other
  direction is reported as non-significant, not re-tested in the other tail.
- **Alpha**: 0.05.

## Why the count and not the median

D-029. A.9 #4 asks for "median time-to-first-violation <= 50% of uniform
random". That quantity is **right-censored**: a trial that never finds the bug
has no finite time, and a median over censored data is undefined. The count of
trials that found a violation within a fixed budget is the same claim made
testable. At n=10 per arm it has real power: 9/10 vs 2/10 gives p = 0.0027.

**Secondary, reported but not the test**: Mann-Whitney U on time-to-first
`linearizable.kv` violation over the uncensored subset, with the censoring rate
stated. It is descriptive only.

## What I knew when I wrote this, stated so the choice can be judged

I had already run one exploratory Saboteur trial (run `r_2026_09_08_19d9`, seed
424242, 30m budget). From it I knew:

1. **The "any violation" endpoint SATURATES and is therefore useless.** The
   fixture's `perturber.allow` contains `proc.kill`, the compose file sets
   `restart: "no"`, and a killed node never comes back, so
   `availability_after_heal` and `resource_return_to_baseline` violate on
   essentially every `proc.kill` world. Both arms would score 10/10 and the
   test would measure nothing. Choosing the endpoint to avoid a ceiling effect
   is legitimate; concealing that I chose it for that reason would not be.
2. The consistency violation IS reachable (that run found one) so the harder
   endpoint is not guaranteed to floor at 0/10 either.
3. I do **not** know either arm's success rate on the consistency endpoint. The
   exploratory run was a single Saboteur trial at a different budget and a
   different seed, and no `random` trial has been run at all.

## Falsifiability

This test can fail. If the Saboteur finds the consistency violation in no more
trials than uniform random, the p-value will exceed 0.05 and the honest report
is that Addendum A.9 #4 is **not** demonstrated on this fixture at this budget.
That outcome is to be reported with the table and the p-value exactly as
obtained.
