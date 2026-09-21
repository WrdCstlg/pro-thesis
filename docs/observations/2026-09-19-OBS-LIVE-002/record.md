# OBS-LIVE-002: Observation record

Date: 2026-09-19
Observer: Claude (external; recorded the execution, did not influence it)
Run: `r_2026_09_19_151f`
Predecessor: [OBS-LIVE-001](../2026-09-17-OBS-LIVE-001/record.md)

**There is no `design.md` for this observation, and that absence is deliberate.**
OBS-LIVE-001's design document was a pre-registration written before its run.
This run was executed first and written up afterwards. Producing a design
document now would manufacture the appearance of a prediction that was never
made, which is the failure mode this project exists to catch. The scenario and
the criteria are stated below, labelled as what they are: post-hoc.

## Verdict of the observation

**PRO-THESIS functioned end to end, and this bundle records something no
artifact on this machine had ever recorded: a DRIVE window that spans its
PERTURB.**

The harness booted a 3-node Raft cluster, partitioned the leader's peer plane
at t+3s, healed at t+7.5s, recorded a normative history, and the external
`linearizable.kv` oracle proved by exhausted search (not by timeout) that the
history is not linearizable. The process exit code was **measured as 1**, in
the launching shell, on the line after the call.

## Why this bundle exists

D-071 changed how the lifecycle records DRIVE. Before it, `runWorld` opened
PERTURB with `Advance`, which closes the open primary phase, so DRIVE ended at
the instant PERTURB began. The fix is pinned by a mutation-verified unit test
(`TestPerturbNestsInsideDriveInTheWorldFile`), but a unit test asserts about a
`PhaseLog`, not about the world files this harness ships. Measured across every
`.thesis` file on this machine before this run: **777 worlds carried a DRIVE
window and 0 of them spanned their PERTURB.** D-071's own text says post-fix
worlds would; nothing demonstrated it.

This run is that demonstration.

## Setup evidence

Binary hashes, provenance readback, oracle-lock state and SUT image digests are
in [`evidence/BUILD_INFO.txt`](evidence/BUILD_INFO.txt). In summary: all three
binaries stamped `commit 2724b78`, `dirty=1`, `source_date 2026-09-18T01:01:01Z`;
the checker's digest verified reproducible by a second build; the oracle lock
verified `ok` before the run and is recorded `ok` with no `executables_moved` in
the verdict the run produced.

## Execution

From `testdata/kvfixture/`:

```
..\..\bin\thesis.exe run --profile linear --seed 1 --fault 'net.partition(role:leader)@3000..7500'
```

The exit code was captured by calling the binary directly in the launching
PowerShell session and reading `$LASTEXITCODE` on the next line. OBS-LIVE-001's
run 1 could not report its exit code because it ran the pipeline inside a
separate `powershell -Command` child, whose own exit code is 1 whatever the
harness returned.

| | Value | Source |
|---|---|---|
| Run ID | `r_2026_09_19_151f` | run directory |
| World seed | 2337997644773862495 | `world.thesis` |
| Verdict | FAIL | `verdict.json` |
| **Process exit code** | **1, measured** | launching shell, in-session |
| Commit recorded | `2724b78+dirty` | `verdict.json`: the BINARY's stamp (D-070) |
| Wall time | 29 s / 600 s budget | `verdict.json` |
| Worlds | 1 run / 5 planned | `verdict.json`: the runner stops at the first violation |
| Oracle lock | `ok`, `executables_moved` absent | `verdict.json` |
| History records | 9,571 (0 unparseable, 7 phase markers) | `history.jsonl`, recounted |
| Operations | 4,782: 4,701 ok / 16 fail / 65 info | `history.jsonl`, recounted |
| Operation records | 9,564 (4,734 read + 4,830 write) | `history.jsonl`, recounted |
| Built-in oracles | 6, all `ok` | `result.json` |
| Violating oracle | `linearizable.kv` (consistency, high) | `verdict.json` |
| Oracle proof | exhausted search, 2,869 states / 10,435 steps, 356 of 577 ops placed on key `k/0` | `verdict.json` |
| Witness | write 4000170 (op 2805) → write 13000190 (op 2924) → read returns **4000170** (op 2962) | `verdict.json` |
| `sut.images` | `sha256:f833e5d8…` for `prothesis/kvfixture:buggy`, service `kv` | `world.thesis` |
| Drain | completed, 13 ms | `result.json` |
| Docker residue | 0 containers, 0 networks | `docker ps -a`, `docker network ls` after teardown |

## The phase windows: what this bundle is for

```
BOOT     -15321..-152 ms
SEED       -152..0
DRIVE         0..7859      <-- spans
PERTURB       1..7859      <-- contained
HEAL       7859..9149
QUIESCE    9149..11166
ASSERT    11166..11166
```

`DRIVE` is 7,859 ms wide and contains `PERTURB` at both edges. Compare the same
scenario, same seed, two days earlier:

| | OBS-LIVE-001 run 1 (pre-fix) | OBS-LIVE-002 (post-fix) |
|---|---|---|
| DRIVE | `0..0`: zero width | `0..7859` |
| PERTURB | `0..7941`: beside DRIVE | `1..7859`: inside DRIVE |
| DRIVE spans PERTURB | no | **yes** |

The pre-fix shape was not cosmetic. `PhaseTimings.At` returns every window
covering a timestamp and the oracle engine takes the lowest lifecycle ordinal,
so a finding carrying `first_seen_ms` was attributed to PERTURB while DRIVE did
not span it, and is attributed to DRIVE now that it does. Neither bundle
exhibits the flip in its own violation, because the external oracle supplies no
`first_seen_ms` and the engine pins those to ASSERT, but every future finding
that does carry one is affected, and **no verdict field records which semantics
produced it.** That is why D-071 forbids pooling pre-fix and post-fix runs in
one statistical sample, and why this bundle is the first member of the post-fix
stratum.

## Seed determinism, re-confirmed independently

`--seed 1` produced world seed **2337997644773862495**: byte-identical to
OBS-LIVE-001 run 1 two days earlier, on rebuilt binaries, after a Docker store
reset, against a freshly built image. OBS-LIVE-001's revision-1 incidental
finding that `--seed` "did not pin the world seed" was retracted in revision 2;
this is a second, independent confirmation of the retraction.

## Same seed, different race

The seed pins the *plan*. It does not pin the *interleaving*, and this pair of
runs demonstrates the distinction better than either could alone:

| | OBS-LIVE-001 run 1 | OBS-LIVE-002 |
|---|---|---|
| World seed | 2337997644773862495 | identical |
| Fault | `net.partition(role:leader)@3000..7500` | identical |
| Operations | 5,406 | 4,782 |
| History records | 10,819 | 9,571 |
| BOOT duration | 22,751 ms | 15,321 ms |
| Leader partitioned | kv-n2 | kv-n2 |
| Leader elected at term 2 | **kv-n1** | **kv-n3** |
| Witness op_ids | 2320 / 2447 / 2470 | 2805 / 2924 / 2962 |
| C3 real-time margin | 456.73 ms | 44.90 ms |

The elected node differs, the operation count differs by 12%, and the witness is
a different triple. The workload is issued over a wall-clock window, so a faster
BOOT yields fewer operations inside the same virtual window; the election is a
race between three timers. This is the mechanism behind OQ-054's *measured
reproduction rate* rather than a claim of determinism, and it is why
OBS-LIVE-001's design text had to be corrected from "the defect window is
deterministic" to a rate.

Election evidence is in the node logs, not inferred: `kv-n2` logs
`event=elected term=1`, then `kv-n3` logs `event=election_start term=2` and
`event=elected term=2 noop_index=1438`, and `kv-n2` logs
`role_change role=follower term=2 leader="kv-n3"`.

## Independent cross-check

Run with the predecessor bundle's script, unmodified, against this bundle's
evidence:

```
python ../2026-09-17-OBS-LIVE-001/crosscheck.py evidence/r_2026_09_19_151f
```

It is referenced rather than copied, deliberately: two copies of a criterion
that is itself the subject of a correction would drift apart, and the criterion
matters more than the convenience.

- **C1**: every witness op_id (2805, 2924, 2962) present with both invoke and
  completion records. OK.
- **C2**: the real-time stale-read scan, all three criteria printed side by
  side over 2,366 completed reads:

  | criterion | flagged |
  |---|---|
  | v1: naive "newest completed write" | 985 |
  | v2: the audit's first correction, still unsound | 60 |
  | **v3: sound** | **50** |

  16 reads returned a value no completed write produced and were skipped rather
  than counted. On OBS-LIVE-001 run 1 the same script reports 1,161 /: / 72.
  The v1-to-v3 ratio is ~20× on both runs: an independent reconfirmation, on
  data that did not exist when the audit was performed, that the original
  "1,161 hard stale reads" figure was an artifact of the criterion.
- **C3**: the oracle's witness reconstructs from raw records under the sound
  criterion, with a 44.90 ms margin between the newer write's completion and the
  stale read's invocation. OK.

The oracle's per-key exhausted search and the observer's criterion agree the
history is not linearizable, by independent routes.

## Function criteria (stated post-hoc)

- **F1 lifecycle**: BOOT→SEED→DRIVE→PERTURB→HEAL→QUIESCE→ASSERT all recorded,
  TEARDOWN marker present, no harness-level error, zero container residue. And,
  for the first time, DRIVE spans PERTURB. **PASS**
- **F2 normative history**: 9,571 records, 0 unparseable, every invoke carrying
  exactly one completion. **PASS**
- **F3 oracle verdicts**: 6 built-in oracles `ok`, external `linearizable.kv`
  `violated` with a witness; `oracle_input.json` evidences the input half of the
  pipe protocol. **PASS, with the same gap OBS-LIVE-001 had**: the oracle's
  stdout is still not persisted, so the OUTPUT half is evidenced only by the
  parsed result. L1c is the open work.
- **F4 exit-code contract**: process exit **1** on a FAIL verdict, measured in
  the launching shell rather than inferred. **PASS**, and unlike OBS-LIVE-001
  this is a measurement rather than a reconstruction.
- **F5 independent reproducibility**: C1 and C3 reconstruct the oracle's
  witness from raw history; C2 v3 finds 50 hard stale reads by an independent
  criterion. **PASS**

## Notes and corrections

1. **Three files in this bundle were edited after the fact, and here is exactly
   how.** Seven absolute-path values carrying the build host's repository root
   (`C:\AI Projects\Pro-synthesis\…`) were rewritten to repository-relative form
   before committing: `verdict.json` (`witness.oracle_definition`,
   `witness.stderr_path` (2 values), `world-0001/oracle_input.json`
   (`history_path`, `final_state_path`, `telemetry_path`, `world_path`) 4), and
   `world-0001/plan.json` (`history_path`; 1). The edit removed one prefix
   substring and changed nothing else; 0 absolute paths remain. **Every other
   file in this bundle is byte-identical to the live run directory**, verified by
   SHA-256 at copy time, including `history.jsonl`, from which every count and
   both witnesses above are recomputed. The harness still emits absolute paths,
   so re-running this scenario reproduces them: this is a redaction in the
   bundle, not a fix in the tool.
2. **The absolute-path leak is live, and this run is fresh proof of it.** The
   run produced `witness.stderr_path` and `oracle_definition` as absolute host
   paths on 2026-09-19, after OBS-LIVE-001 note 9 documented the same class of
   leak. L1c's design proposes emitting them run-relative
   (`world-0001/oracles/linearizable.kv.stderr.log`), which would end the class
   rather than redacting each instance by hand.
3. **`witness.stderr_path` points at a file this bundle does not contain.** The
   capture is 0 bytes (the oracle writes its document on stdout, per the pipe
   protocol) and 0-byte captures are omitted here as they were in OBS-LIVE-001.
   A reader following that path finds nothing, which is the correct amount of
   information and an uncomfortable way to convey it.
4. **Not included:** `telemetry.json` (230 KB), `telemetry.jsonl` (133 KB),
   `prothesis-overlay.yaml`, and the 0-byte oracle stderr capture. The node logs
   under `logs/` ARE included, because the leader-election claim above is
   provable only from them. Note that `telemetry.jsonl` is an input the external
   oracle is given (`oracle_input.json.telemetry_path`), so re-running the
   checker against this bundle alone is not possible: the same limitation
   OBS-LIVE-001 carries.
5. **The live run directory is gitignored** (`**/.prothesis/runs/`) and exists
   only on this machine. This bundle is the durable copy. Nothing prunes run
   directories today, so the original survives by accident rather than by policy:
   see OQ-067's neighbours and the L1b work.

## Artifacts

- This directory: `record.md` (this file), `evidence/BUILD_INFO.txt`, and
  `evidence/r_2026_09_19_151f/`: `verdict.json` plus
  `world-0001/{history.jsonl, phases.json, phases.jsonl, result.json,
  oracle_input.json, plan.json, final_state.json, world.thesis, logs/kv-n*.log}`.
  Total 2,147,994 bytes.
- Cross-check script: [`../2026-09-17-OBS-LIVE-001/crosscheck.py`](../2026-09-17-OBS-LIVE-001/crosscheck.py),
  referenced rather than duplicated.
- Live run directory (gitignored, unprotected):
  `testdata/kvfixture/.prothesis/runs/r_2026_09_19_151f/`.
