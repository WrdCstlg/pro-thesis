# Media

Every image the README shows, how it was made, and how to make it again. None is a mock-up.

| File | What it is | Made by |
|---|---|---|
| `01-catch.svg` … `05-replay.svg` | The five steps of the README's first five minutes, played back from the recordings in [`demo/`](demo/) | [`scripts/demo/render-cast-svg.mjs`](../../scripts/demo/render-cast-svg.mjs) |
| `witness.svg` | The catch from step 1, drawn from that run's `verdict.json` | [`scripts/demo/witness-svg.mjs`](../../scripts/demo/witness-svg.mjs) |
| `demo/*.cast` | The recordings: [asciicast v2](https://docs.asciinema.org/manual/asciicast/v2/), one per step, with the measured timestamps | [`scripts/demo/record.mjs`](../../scripts/demo/record.mjs) |
| `demo/measurements.json` | Host, build, exit codes, wall time, per-world phase timings and bytes, and the Docker build cache before and after | [`scripts/demo/first-five-minutes.ps1`](../../scripts/demo/first-five-minutes.ps1) |

## What is real, and what playback changes

The recordings hold every byte the commands wrote, in order, with the time it arrived. Each
recording's header has a `prothesis` block that records what actually ran: the argv, the
working directory, the environment overrides and the exit code. Commands are shown in POSIX form;
the recordings were made on Windows, and the header says so. These are the second recording of
the five steps; the first was replaced before anything was committed, and
[`docs/EVIDENCE.md`](../EVIDENCE.md#the-first-five-minutes-measured) says why and what it measured.

Playback changes three things, and each SVG's `<desc>` names the ones it used:

- pauses longer than one second are shortened to one second, so step 1's 25.6-second world
  reaches its verdict 5.0 seconds into playback;
- output that arrives all at once but is taller than the screen is shown a screen at a time,
  five seconds per screen, instead of scrolling past in one frame;
- lines longer than 100 columns wrap.

Colour is added, too: any colour in a recording is dropped, and every colour on screen is the
renderer's reading of the line's text (a FAIL verdict red, an exit code by its meaning, fault and
phase lines in their own hues). The recordings here carry none.

A reader whose system asks for reduced motion sees the final screen, without animation.

`witness.svg` draws only what `verdict.json` contains: times from the causal timeline, operation
ids from the witness, and the checker's explanation quoted verbatim. It adds one sentence of its
own. When the overwriting write completed, that sentence is the gap between two recorded times;
when that write timed out, as in the current image, it combines two things the explanation says.
[`docs/EVIDENCE.md`](../EVIDENCE.md#the-witness-in-the-readmes-diagram) says which, and how.

## Making them again

```powershell
# Re-record against a live Docker daemon: runs the five steps, rewrites everything here.
pwsh -File scripts/demo/first-five-minutes.ps1

# Redraw the SVGs from the recordings already here. Needs Node, not Docker.
pwsh -File scripts/demo/first-five-minutes.ps1 -RenderOnly
```

`-RenderOnly` redraws `witness.svg` only where the catch run's `verdict.json` is still on disk
(run directories are gitignored); elsewhere it keeps the committed one and prints a warning.

To look at one moment of a recording as a still, for example 14 seconds into step 1's playback:

```powershell
node scripts/demo/render-cast-svg.mjs docs/media/demo/01-catch.cast still.svg --at 14
```
