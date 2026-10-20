# Onboarding log (axis A7)

The honest cost of bringing each target to the P1 definition of done: `thesis doctor` green, one
world reaching ASSERT with a complete bundle (INCONCLUSIVE or better), no verdict arm run. Time
includes everything; operator interventions are listed.

## rqlite 10.5.3

| | |
|---|---|
| Image | `rqlite/rqlite:10.5.3@sha256:de385382ef6d93a4b4b831eac4d419cfa35fdb1515621a4ad8ba8b5a5beeb6b5` (MIT) |
| Design before the build | about ten minutes, not timed precisely |
| Build to done | 2026-10-19 23:18:28 to 23:48:16 -04:00: **29 min 48 s** |
| Review | to 23:50:38 -04:00 (tests re-run, classification logic read, gaps verified) |
| Configuration | `docker-compose.yaml` 120 lines (72 non-comment), `prothesis.yaml` 91 (44), oracle definition 35 (6, byte-identical to etcd's) |
| Go | driver 967 lines, role probe 82, steady-state probe 85, shared package 279; tests 775 (driver) and 167 (role probe) |
| Tests | 36 pass, 0 fail (`scripts/run-tests.ps1 -Match targets/rqlite`); every test failed before its code existed and again under a targeted mutation |
| Lock | manifest `sha256:3395e4cf...a261`, 60 configuration values; fingerprints the driver and the checker |
| Smoke world | `r_2026_10_20_95ab`: PASS, exit 0; 500 operations (241 determinate reads, 259 writes), every read at `linearizable`, every response `v10.5.3`; image identity equal to the pin; 0 volumes, containers or networks left |
| Not exercised live | the write redirect path (the leader never changed in the smoke world) and the role probe |

Contract gaps found: OQ-091 (driver profile fields), OQ-092 (no node ids in `PROTHESIS_TARGETS`),
OQ-093 (image outside the lock), OQ-094 (probe programs not fingerprinted), OQ-095 (term
tie-break), OQ-096 (steady-state success not recorded), OQ-088 (unstamped target programs), and
OQ-087 (worked around by launching from the project directory). Each is also noted in
`docs/TARGETS.md` section 11.

Operator interventions: the worktree had no harness binary, so `scripts/build.ps1` ran first and
overwrote the fixture's locked checker and driver, which were restored from the locked bytes and
verified; three one-off `docker run` calls to read `rqlited -version` and `-h`; one retried commit
(a quoting error; nothing had been committed). No restarts, no manual cleanup, no retried worlds.
