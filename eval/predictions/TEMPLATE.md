# Prediction: <ARM-ID>

Registered: <YYYY-MM-DD>, committed before the arm's first world. Frozen after that commit; an
amendment is a new dated section, and any change made after seeing results turns the arm into a
hypothesis, not evidence.

## Arm

Target, control type, variant, profile, fault schedule, seeds and N, as in `eval/manifest.yaml`.

## Expected verdict

The exit code, and for a multi-world run how many worlds are expected to land where.

## Expected mechanism

What the system and the harness are expected to do, step by step, that produces that verdict.

## Expected violation class

For a FAIL: the shape of the witness (for example a stale read served by a deposed leader). For a
PASS: why no violation is expected.

## Fault kinds expected to realize

Each fault in the schedule, and how its realization will be checked from the world file.

## INCONCLUSIVE risks

What could make a world INCONCLUSIVE, and which known-problem attribution would apply.

## What would falsify this

The evidence that would show the prediction wrong, stated before any world runs, for the verdict and
for the mechanism separately.
