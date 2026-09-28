# Evidence

Every claim the [README](../README.md) makes, and where its measurement lives. Run directories
under `.prothesis/runs/` are gitignored, so a run id is checkable on the machine that produced it
and in the two committed observation bundles; everything else here is in the tree. Where a
number came from a command, the command is next to it.

---

## The first five minutes, measured

Recorded 2026-09-28 between 00:07 and 00:12 UTC (the evening of 2026-09-27 on the build host's
clock) by [`scripts/demo/first-five-minutes.ps1`](../scripts/demo/first-five-minutes.ps1)
against the fixture, with `bin/thesis` stamped `build: commit b619c28, source date
2026-09-27T21:11:36Z`. Every verdict names `b619c28` without the `+dirty` suffix a build from an
uncommitted tree carries (`internal/buildinfo`), so the binaries were built from that commit.
The tree they ran in was not clean: it held the change that commits these recordings, and
`measurements.json` records `tree_dirty: true`. The script asserts each step's exit code and
stops on the first surprise. The raw file is
[`docs/media/demo/measurements.json`](media/demo/measurements.json), and the tables below were
generated from it, not typed.

**This is the second recording, and the first was replaced.** An earlier pass of the same five
steps, on the same binaries about ninety minutes before, returned the same five exit codes (runs
`r_2026_09_27_b295` FAIL, `r_2026_09_27_7638` PASS, the refusal with exit 4, `r_2026_09_27_9ca0`
INCONCLUSIVE, and replay `r_2026_09_27_af3c` reproducing 3 of 3). It was replaced, before anything
was committed, because its replay step printed the absolute path of the checkout, whose directory
name was not one to publish. This pass ran through a directory junction named `pro-thesis`
pointing at the same checkout. The first pass's recordings were overwritten and are not in the
tree; its run directories remain on the build host.

Build host: AMD Ryzen 9 9950X3D, Windows 11 (build 26200), Docker Desktop running Docker Engine
29.1.3 (`docker version` `Server.Version`) in a VM of 8 CPUs and 30.1 GiB (`docker info` `NCPU` and
`MemTotal`), all recorded in the file.

| Step | Exit (expected) | Wall time | Run verdict | Lock |
|---|---|---:|---|---|
| 01-catch | 1 (1) | 25.603 s | FAIL | bypassed |
| 02-patched | 0 (0) | 111.835 s | PASS | ok |
| 03-cheat | 4 (4) | 0.029 s | none: refused before any run started | |
| 04-narrowed | 2 (2) | 22.338 s | INCONCLUSIVE | bypassed |
| 05-replay | 1 (1) | 66.138 s | none: replay leaves no `verdict.json` in its run directory | |

Wall time is measured by the recorder around the process, from spawn to exit.

Every world those steps ran, phase durations in milliseconds from each world's
`world.thesis` `phase_timings`, bytes summed over the world's directory:

| Step | Run | World | Outcome | BOOT | SEED | DRIVE | HEAL | QUIESCE | Bytes |
|---|---|---|---|---:|---:|---:|---:|---:|---:|
| 01-catch | `r_2026_09_28_2071` | world-0001 | violation | 9,299 | 489 | 8,149 | 3,281 | 2,019 | 2,464,301 |
| 02-patched | `r_2026_09_28_d64c` | world-0001 | pass | 8,618 | 242 | 8,002 | 1,755 | 2,019 | 2,522,065 |
| 02-patched | `r_2026_09_28_d64c` | world-0002 | pass | 8,360 | 159 | 7,904 | 1,771 | 2,018 | 3,158,969 |
| 02-patched | `r_2026_09_28_d64c` | world-0003 | pass | 8,366 | 178 | 7,888 | 1,850 | 2,025 | 3,075,532 |
| 02-patched | `r_2026_09_28_d64c` | world-0004 | pass | 8,408 | 174 | 7,878 | 1,720 | 2,017 | 3,141,589 |
| 02-patched | `r_2026_09_28_d64c` | world-0005 | pass | 8,039 | 156 | 7,949 | 2,177 | 2,022 | 3,189,412 |
| 04-narrowed | `r_2026_09_28_3c56` | world-0001 | pass | 8,477 | 180 | 7,900 | 1,685 | 2,021 | 3,188,329 |
| 05-replay | `r_2026_09_28_4dd7` | world-0001 | violation | 8,343 | 158 | 7,897 | 1,746 | 2,018 | 3,153,551 |
| 05-replay | `r_2026_09_28_4dd7` | world-0002 | violation | 8,341 | 153 | 7,939 | 1,677 | 2,019 | 3,259,116 |
| 05-replay | `r_2026_09_28_4dd7` | world-0003 | violation | 7,788 | 150 | 7,896 | 1,687 | 2,022 | 3,203,763 |

PERTURB runs inside DRIVE and is within 2 ms of it in every row, so it is not repeated. ASSERT
records 0 ms in every world, so this table cannot say what checking cost; that has not been
measured.

**Where the bytes go.** Over the four run directories those steps wrote (30,362,888 B, of which
the ten world directories hold 30,356,627 B), grouped by file name with `Get-ChildItem -Recurse
-File | Group-Object Name`: `history.jsonl` 86.5%, `telemetry.json` 8.2%, `telemetry.jsonl` 4.6%,
everything else under 1%. World by world, `history.jsonl` is 80.4% to 87.9% of the bytes.

**Build cache.** `docker system df` reported the build cache at 11.14 GB before the first step
and 11.14 GB after the last, across all ten worlds (D-078).

**The corpus nothing prunes.** After this recording the fixture's run directory on the build
host held 204 run bundles in 2,957,378,716 B (2.75 GiB); 160 carry a `verdict.json` (FAIL 84,
INCONCLUSIVE 49, PASS 26, BUDGET_EXHAUSTED 1). The retention policy in `prothesis.yaml` is
parsed, validated and enforced by nothing (OQ-070).

---

## The witness in the README's diagram

[`docs/media/witness.svg`](media/witness.svg) is drawn by
[`scripts/demo/witness-svg.mjs`](../scripts/demo/witness-svg.mjs) from the `verdict.json` of
`r_2026_09_28_2071` and nothing else: operation times from its causal timeline, operation ids
from its witness, and the explanation quoted verbatim. The checker's full explanation, as it
wrote it:

> key "k/0": no linearization exists for the 542 operation(s) on this key under a
> last-write-wins register. The search space was exhausted (1578 state(s) explored over 5055
> step(s)) with every branch rejected; this is a WITNESSED failure, not a timeout and not a
> budget. The deepest partial linearization the search reached placed 291 of 542 operation(s) on
> this key. At that point the register held 14000167, written by op 2480, and op 2493 (process 13,
> read) returned 15000136. That value was written by op 2331, which every linearization places
> before the value the register held. The witness op_ids point at the failure; the proof is the
> exhausted search over the whole key, not those operations alone. Evidence: 2199 determinate
> read(s), 2182 determinate write(s), 58 indeterminate write(s) explored on both the applied and
> the not-applied branch; 15 failed operation(s) dropped; 8 indeterminate read(s) dropped.

In this witness the write that held the register, op 2480, timed out (`info`, t+4755..5756 ms),
and the read, op 2493 at t+5012..5013 ms, falls inside it. Real-time order alone therefore cannot
show the read stale, and the diagram does not claim it does. Its one sentence of its own, that
the checker "tried it both applied and not applied; neither gives this history a valid order",
combines two things the explanation says: every indeterminate write was explored on both
branches, and no linearization exists. The script prints it only when the first of those is in
the text. The first recording's witness was the other kind: its overwriting write
completed 6 ms before the stale read began, and for that case the script prints the gap instead.

---

## What a run leaves behind

Every world writes the same files. The
[OBS-LIVE-002 bundle](observations/2026-09-19-OBS-LIVE-002/evidence/r_2026_09_19_151f/) is one
world committed verbatim, so each line below can be checked against a real file.

| File | What it is |
|---|---|
| `verdict.json` | `prothesis.verdict/v1`: run id, profile, the build's commit stamp, the verdict, budget use, `violations[]` with witnesses, coverage, `oracle_lock {status, manifest_sha}`, artifact paths. |
| `world-NNNN/world.thesis` | `prothesis.world/v1`, canonical JSON, content-addressed. Seed, topology variant, driver profile, the fault schedule as **planned** and as **realized** (which node `role:leader` bound to, and the millisecond at which injection actually landed), phase timings, and `sut.images`: the resolved image ID, not the tag. The loader re-encodes and byte-compares on every read, so an edited world is rejected rather than silently re-hashed. |
| `world-NNNN/history.jsonl` | One JSON object per line, Jepsen-shaped: `invoke` / `ok` / `fail` / `info` per operation, with `info` meaning "may or may not have taken effect", plus the phase markers. |
| `world-NNNN/phases.jsonl`, `plan.json`, `oracle_input.json`, `result.json`, `final_state.json`, `logs/` | Phase transitions with nanosecond timestamps; the driver's plan; exactly what the external oracle was handed; the per-world outcome; the post-QUIESCE state; every node's log. |

The witness from that bundle, as the oracle wrote it into `verdict.json`:

> key "k/0": no linearization exists for the 577 operation(s) on this key
> under a last-write-wins register. The search space was exhausted (2869
> state(s) explored over 10435 step(s)) with every branch rejected; this is a
> WITNESSED failure, not a timeout and not a budget. The deepest partial
> linearization the search reached placed 356 of 577 operation(s) on this key.
> At that point the register held 13000190, written by op 2924, and op 2962
> (process 10, read) returned 4000170. That value was written by op 2805,
> which every linearization places before the value the register held.

An independent observer's script
([`crosscheck.py`](observations/2026-09-17-OBS-LIVE-001/crosscheck.py)) reconstructs the witness
from the raw history without the oracle, and counts hard real-time stale reads by its own
criterion: 72 and 46 in OBS-LIVE-001's two runs, 50 in OBS-LIVE-002.

---

## What has been measured

| Claim | Evidence |
|---|---|
| The planted defect is found | Fixture `FAIL`, exit `1`, asserted on every CI push. A sample of twenty fresh worlds (`--seed 1` to `20`) on clean, stamped binaries: 20 of 20 `FAIL`, exit `1`; the one-sided 95% lower bound on the per-world rate from twenty distinct plans is 0.86 (OQ-054). |
| The patched build is not accused | `-tags kvfixed` fixture `PASS`, exit `0`, on every CI push; adversarial probe A1; five of five worlds in the recording above. |
| The checker discriminates on a system nobody here wrote | etcd v3.5.17: linearizable reads `PASS` 2/2 worlds (`r_2026_09_15_8c77`), serializable reads `FAIL` on `linearizable.kv` (`r_2026_09_15_9a29`), both as pre-registered and both re-asserted in CI. Re-derived from etcd's `header.revision`: 313 revision-stale reads in the failing arm, 0 in the passing one ([`targets/etcd/README.md`](../targets/etcd/README.md)). |
| A violation is a proof, not a timeout | The oracle reports the exhausted search: 2,869 states over 10,435 steps for the 577 operations on `k/0`, witness ops 2805 / 2924 / 2962 (OBS-LIVE-002). The witness reconstructs from raw records without the oracle (`crosscheck.py`, criteria C1 to C3, reported in both records). |
| The witness cites evidence that exists | Adversarial probe A6. |
| Oracles fail closed | `TestAWorldNoOracleJudgedIsInconclusiveNotPass` in [`internal/control/vacuous_pass_test.go`](../internal/control/vacuous_pass_test.go); `TestExternalOracleReportsInconclusive`, `TestExternalOracleThatHangsIsKilledAndIsInconclusive`, `TestAnExternalInconclusiveTakesTheRunToExitTwo`, `TestExternalOracleUnderACancelledContextIsInconclusiveNotViolated` in [`internal/oracle/external_test.go`](../internal/oracle/external_test.go); `TestAvailabilityWithNoDeclaredProbeIsInconclusive`. |
| The gate is locked | Probe A4; `TestHandEditedLockIsMismatch` and the rest of [`internal/lock/lock_test.go`](../internal/lock/lock_test.go); [`internal/lock/driver_lock_test.go`](../internal/lock/driver_lock_test.go) for the driver (D-090); CI's `oracles verify` step; step 03 of the recording above. |
| A narrowed run cannot pass | D-059; step 04 of the recording above. |
| Worlds are content-addressed and cannot be hand-edited | Probe A5 round-trips every stored world byte for byte; `TestUnmarshalCanonicalRejectsNonCanonical`, `TestUnmarshalCanonicalAcceptsItsOwnOutput` ([`internal/recorder/cjson`](../internal/recorder/cjson/)); `TestStoreWorldNoClobberRefusesToLoseARegression`. |
| HEAL restores the network and the processes | Probe A3 pairs the nastiest faults and checks the residue; CI's `residue` step lists any container or network left behind. |
| `role:leader` binds through a probe the target declares, at injection time | Resolved to `kv-n2` in all twenty sample worlds, recorded in each `world.thesis` as `realized`; on etcd, `r_2026_09_15_d939` and `r_2026_09_15_fa3e` (D-066). |
| DRIVE spans PERTURB, so the fault lands under load | All 25 worlds checked on the current lifecycle record it: the twenty-world sample, OBS-LIVE-002, and four worlds recorded for OQ-068 (D-071); and all ten worlds of the recording above. |
| The verdict names the build, and the world names the image | `verdict.commit` is the stamp compiled into the binary (D-070); `scripts/build.ps1 -Verify` proves two builds of one commit byte-identical (D-072); `sut.images` in OBS-LIVE-002's `world.thesis` is `sha256:f833e5d8…`, resolved from `docker inspect` at BOOT. |
| Image identity is stable across rebuilds | Three consecutive worlds on one build cache record one `sut.images` digest; the harness switches off buildx's default attestations on every docker call, and the fixture's build context excludes its own run corpus (D-078, OQ-068). |
| A sick Docker daemon costs a world its provenance, not its budget | Image resolution is bounded at five seconds behind a seam, pinned by six tests, each proven by mutation (D-073). The ASSERT-time container inspect is bounded the same way (D-089). |
| The suite passes on a second machine | CI runs on every push to `master` (this repository's Actions tab). On the first public commit, 2026-09-21: Go 1.27.1 with the Docker-backed tests and the race detector, 1,566 pass · 3 skip · 0 fail; Go 1.22.12, 1,564 pass · 5 skip · 0 fail; live job green with all five exit codes asserted (1, 0, 0, 0, 1, each as wanted). |
| Offline history checking finds nothing malformed in the recorded corpus | `thesis history verify` over both committed corpora: 985 recorded histories and 9,549,942 operation records in 23.5 s; 924 clean, and not one malformed line, duplicate `op_id` or negative interval (D-086). |
