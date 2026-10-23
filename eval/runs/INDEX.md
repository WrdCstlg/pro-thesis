# Run index

Every run the evaluation suite made, in order. The artifacts stay in the target's
`.prothesis/runs/<run id>/` (or, once archived, `.prothesis/archive/<run id>.tar.zst` with its
`.sha256.json`). "Volumes after" is the count of the target's compose-labelled volumes left after
the run; anything other than 0 excludes the run's worlds from the precision and reproducibility
figures.

Run bundles are not committed. For every run cited below, `manifests/<run id>.sha256.json` records
the SHA-256 of each file in its bundle, so a bundle found on disk later can be checked against what
was cited. The P1 bundles were built in separate worktrees and copied into this tree's
`targets/<t>/.prothesis/runs/`, verified file by file, before those worktrees could be removed.

| Arm | Phase | Run id | Commit | Exit (shell) | Worlds | Volumes after | Archived |
|---|---|---|---|---|---|---|---|
| rqlite onboarding smoke (no arm) | P1 | `r_2026_10_20_95ab` (targets/rqlite) | `5e55347` (harness build) | 0 | 1 | 0 | no |
| Consul onboarding smoke (no arm) | P1 | `r_2026_10_20_9237` (targets/consul) | `5e55347` (harness build) | 0 | 1 | 0 | no |
| Redis onboarding smoke (no arm) | P1 | `r_2026_10_20_c22a` (targets/redis) | `5e55347` (harness build) | 0 | 1 | 0 | no |
| HC-01 seed 1 (excluded) | P2 | `r_2026_10_23_5eb3` (testdata/kvfixture) | `c22e678` | 0 | 1 | 0 | no |
| HC-01 seed 2 | P2 | `r_2026_10_23_63b7` (testdata/kvfixture) | `c22e678` | 0 | 1 | 0 | no |
| HC-01 seed 3 | P2 | `r_2026_10_23_497b` (testdata/kvfixture) | `c22e678` | 0 | 1 | 0 | no |
| HC-01 seed 4 | P2 | `r_2026_10_23_359f` (testdata/kvfixture) | `c22e678` | 0 | 1 | 0 | no |
| HC-01 seed 5 | P2 | `r_2026_10_23_83bb` (testdata/kvfixture) | `c22e678` | 0 | 1 | 0 | no |
| HC-01 seed 6 | P2 | `r_2026_10_23_e3f1` (testdata/kvfixture) | `c22e678` | 0 | 1 | 0 | no |
| HC-01 seed 7 | P2 | `r_2026_10_23_5946` (testdata/kvfixture) | `c22e678` | 0 | 1 | 0 | no |
| HC-01 seed 8 | P2 | `r_2026_10_23_592e` (testdata/kvfixture) | `c22e678` | 0 | 1 | 0 | no |
| HC-01 seed 9 | P2 | `r_2026_10_23_cf0d` (testdata/kvfixture) | `c22e678` | 0 | 1 | 0 | no |
| HC-01 seed 10 | P2 | `r_2026_10_23_b18d` (testdata/kvfixture) | `c22e678` | 0 | 1 | 0 | no |
| HC-02 seed 1 | P2 | `r_2026_10_23_8cf8` (testdata/kvfixture) | `c22e678` | 0 | 1 | 0 | no |
| HC-02 seed 2 | P2 | `r_2026_10_23_1d7e` (testdata/kvfixture) | `c22e678` | 0 | 1 | 0 | no |
| HC-02 seed 3 | P2 | `r_2026_10_23_fce7` (testdata/kvfixture) | `c22e678` | 0 | 1 | 0 | no |
| HC-02 seed 4 | P2 | `r_2026_10_23_150b` (testdata/kvfixture) | `c22e678` | 0 | 1 | 0 | no |
| HC-02 seed 5 | P2 | `r_2026_10_23_17ea` (testdata/kvfixture) | `c22e678` | 0 | 1 | 0 | no |
| HC-02 seed 6 | P2 | `r_2026_10_23_875f` (testdata/kvfixture) | `c22e678` | 0 | 1 | 0 | no |
| HC-02 seed 7 | P2 | `r_2026_10_23_38cf` (testdata/kvfixture) | `c22e678` | 0 | 1 | 0 | no |
| HC-02 seed 8 | P2 | `r_2026_10_23_cf51` (testdata/kvfixture) | `c22e678` | 0 | 1 | 0 | no |
| HC-02 seed 9 | P2 | `r_2026_10_23_863e` (testdata/kvfixture) | `c22e678` | 0 | 1 | 0 | no |
| HC-02 seed 10 | P2 | `r_2026_10_23_1210` (testdata/kvfixture) | `c22e678` | 0 | 1 | 0 | no |
| ET-01 seed 1 | P2 | `r_2026_10_23_8c90` (targets/etcd) | `c22e678` | 0 | 1 | 0 | no |
| ET-01 seed 2 | P2 | `r_2026_10_23_806f` (targets/etcd) | `c22e678` | 0 | 1 | 0 | no |
| ET-01 seed 3 | P2 | `r_2026_10_23_65f5` (targets/etcd) | `c22e678` | 0 | 1 | 0 | no |
| ET-01 seed 4 | P2 | `r_2026_10_23_4ba6` (targets/etcd) | `c22e678` | 0 | 1 | 0 | no |
| ET-01 seed 5 | P2 | `r_2026_10_23_bbbc` (targets/etcd) | `c22e678` | 0 | 1 | 0 | no |
| ET-01 seed 6 | P2 | `r_2026_10_23_6de4` (targets/etcd) | `c22e678` | 0 | 1 | 0 | no |
| ET-01 seed 7 | P2 | `r_2026_10_23_c204` (targets/etcd) | `c22e678` | 0 | 1 | 0 | no |
| ET-01 seed 8 | P2 | `r_2026_10_23_be0a` (targets/etcd) | `c22e678` | 0 | 1 | 0 | no |
| ET-01 seed 9 | P2 | `r_2026_10_23_8079` (targets/etcd) | `c22e678` | 0 | 1 | 0 | no |
| ET-01 seed 10 | P2 | `r_2026_10_23_3524` (targets/etcd) | `c22e678` | 0 | 1 | 0 | no |
| RQ-01 seed 1 | P2 | `r_2026_10_23_2db8` (targets/rqlite) | `c22e678` | 0 | 1 | 0 | no |
| RQ-01 seed 2 | P2 | `r_2026_10_23_e151` (targets/rqlite) | `c22e678` | 0 | 1 | 0 | no |
| RQ-01 seed 3 | P2 | `r_2026_10_23_b9a4` (targets/rqlite) | `c22e678` | 0 | 1 | 0 | no |
| RQ-01 seed 4 | P2 | `r_2026_10_23_996e` (targets/rqlite) | `c22e678` | 2 | 1 | 0 | no |
| RQ-01 seed 5 | P2 | `r_2026_10_23_e7c7` (targets/rqlite) | `c22e678` | 0 | 1 | 0 | no |
| RQ-01 seed 6 | P2 | `r_2026_10_23_165c` (targets/rqlite) | `c22e678` | 0 | 1 | 0 | no |
| RQ-01 seed 7 | P2 | `r_2026_10_23_b815` (targets/rqlite) | `c22e678` | 0 | 1 | 0 | no |
| RQ-01 seed 8 | P2 | `r_2026_10_23_c30e` (targets/rqlite) | `c22e678` | 0 | 1 | 0 | no |
| RQ-01 seed 9 | P2 | `r_2026_10_23_148f` (targets/rqlite) | `c22e678` | 0 | 1 | 0 | no |
| RQ-01 seed 10 | P2 | `r_2026_10_23_6f51` (targets/rqlite) | `c22e678` | 0 | 1 | 0 | no |
| RQ-02 seed 1 | P2 | `r_2026_10_23_a46c` (targets/rqlite) | `c22e678` | 1 | 1 | 0 | no |
| RQ-02 seed 2 | P2 | `r_2026_10_23_04b8` (targets/rqlite) | `c22e678` | 1 | 1 | 0 | no |
| RQ-02 seed 3 | P2 | `r_2026_10_23_fc4a` (targets/rqlite) | `c22e678` | 1 | 1 | 0 | no |
| RQ-02 seed 4 | P2 | `r_2026_10_23_8b22` (targets/rqlite) | `c22e678` | 1 | 1 | 0 | no |
| RQ-02 seed 5 | P2 | `r_2026_10_23_cd14` (targets/rqlite) | `c22e678` | 1 | 1 | 0 | no |
| RQ-02 seed 6 | P2 | `r_2026_10_23_3f30` (targets/rqlite) | `c22e678` | 1 | 1 | 0 | no |
| RQ-02 seed 7 | P2 | `r_2026_10_23_d734` (targets/rqlite) | `c22e678` | 1 | 1 | 0 | no |
| RQ-02 seed 8 | P2 | `r_2026_10_23_d9d0` (targets/rqlite) | `c22e678` | 2 | 1 | 0 | no |
| RQ-02 seed 9 | P2 | `r_2026_10_23_03e3` (targets/rqlite) | `c22e678` | 1 | 1 | 0 | no |
| RQ-02 seed 10 | P2 | `r_2026_10_23_9ed5` (targets/rqlite) | `c22e678` | 1 | 1 | 0 | no |
| RQ-03 seed 1 | P2 | `r_2026_10_23_a2ff` (targets/rqlite) | `c22e678` | 0 | 1 | 0 | no |
| RQ-03 seed 2 | P2 | `r_2026_10_23_32b5` (targets/rqlite) | `c22e678` | 0 | 1 | 0 | no |
| RQ-03 seed 3 | P2 | `r_2026_10_23_9ff6` (targets/rqlite) | `c22e678` | 0 | 1 | 0 | no |
| RQ-03 seed 4 | P2 | `r_2026_10_23_07ca` (targets/rqlite) | `c22e678` | 0 | 1 | 0 | no |
| RQ-03 seed 5 | P2 | `r_2026_10_23_9952` (targets/rqlite) | `c22e678` | 0 | 1 | 0 | no |
| RQ-03 seed 6 | P2 | `r_2026_10_23_9662` (targets/rqlite) | `c22e678` | 0 | 1 | 0 | no |
| RQ-03 seed 7 | P2 | `r_2026_10_23_9e8b` (targets/rqlite) | `c22e678` | 0 | 1 | 0 | no |
| RQ-03 seed 8 | P2 | `r_2026_10_23_a856` (targets/rqlite) | `c22e678` | 0 | 1 | 0 | no |
| RQ-03 seed 9 | P2 | `r_2026_10_23_e948` (targets/rqlite) | `c22e678` | 0 | 1 | 0 | no |
| RQ-03 seed 10 | P2 | `r_2026_10_23_f47c` (targets/rqlite) | `c22e678` | 0 | 1 | 0 | no |
| CO-01 seed 1 | P2 | `r_2026_10_23_227d` (targets/consul) | `c22e678` | 1 | 1 | 0 | no |
| CO-01 seed 2 | P2 | `r_2026_10_23_983e` (targets/consul) | `c22e678` | 1 | 1 | 0 | no |
| CO-01 seed 3 | P2 | `r_2026_10_23_1782` (targets/consul) | `c22e678` | 2 | 1 | 0 | no |
| CO-01 seed 4 | P2 | `r_2026_10_23_6f9b` (targets/consul) | `c22e678` | 2 | 1 | 0 | no |
| CO-01 seed 5 | P2 | `r_2026_10_23_cbe4` (targets/consul) | `c22e678` | 2 | 1 | 0 | no |
| CO-01 seed 6 | P2 | `r_2026_10_23_c12c` (targets/consul) | `c22e678` | 1 | 1 | 0 | no |
| CO-01 seed 7 | P2 | `r_2026_10_23_ff33` (targets/consul) | `c22e678` | 1 | 1 | 0 | no |
| CO-01 seed 8 | P2 | `r_2026_10_23_c928` (targets/consul) | `c22e678` | 1 | 1 | 0 | no |
| CO-01 seed 9 | P2 | `r_2026_10_23_52f2` (targets/consul) | `c22e678` | 2 | 1 | 0 | no |
| CO-01 seed 10 | P2 | `r_2026_10_23_b2ba` (targets/consul) | `c22e678` | 2 | 1 | 0 | no |
| CO-02 seed 1 | P2 | `r_2026_10_23_e156` (targets/consul) | `c22e678` | 0 | 1 | 0 | no |
| CO-02 seed 2 | P2 | `r_2026_10_23_12ac` (targets/consul) | `c22e678` | 0 | 1 | 0 | no |
| CO-02 seed 3 | P2 | `r_2026_10_23_5644` (targets/consul) | `c22e678` | 0 | 1 | 0 | no |
| CO-02 seed 4 | P2 | `r_2026_10_23_36dd` (targets/consul) | `c22e678` | 0 | 1 | 0 | no |
| CO-02 seed 5 | P2 | `r_2026_10_23_7f53` (targets/consul) | `c22e678` | 0 | 1 | 0 | no |
| CO-02 seed 6 | P2 | `r_2026_10_23_0571` (targets/consul) | `c22e678` | 0 | 1 | 0 | no |
| CO-02 seed 7 | P2 | `r_2026_10_23_ff67` (targets/consul) | `c22e678` | 0 | 1 | 0 | no |
| CO-02 seed 8 | P2 | `r_2026_10_23_c352` (targets/consul) | `c22e678` | 0 | 1 | 0 | no |
| CO-02 seed 9 | P2 | `r_2026_10_23_9629` (targets/consul) | `c22e678` | 0 | 1 | 0 | no |
| CO-02 seed 10 | P2 | `r_2026_10_23_d6e7` (targets/consul) | `c22e678` | 0 | 1 | 0 | no |
| CO-03 seed 1 | P2 | `r_2026_10_23_ff27` (targets/consul) | `c22e678` | 0 | 1 | 0 | no |
| CO-03 seed 2 | P2 | `r_2026_10_23_33ed` (targets/consul) | `c22e678` | 0 | 1 | 0 | no |
| CO-03 seed 3 | P2 | `r_2026_10_23_e3a5` (targets/consul) | `c22e678` | 0 | 1 | 0 | no |
| CO-03 seed 4 | P2 | `r_2026_10_23_8d05` (targets/consul) | `c22e678` | 2 | 1 | 0 | no |
| CO-03 seed 5 | P2 | `r_2026_10_23_5f08` (targets/consul) | `c22e678` | 0 | 1 | 0 | no |
| CO-03 seed 6 | P2 | `r_2026_10_23_4aaa` (targets/consul) | `c22e678` | 0 | 1 | 0 | no |
| CO-03 seed 7 | P2 | `r_2026_10_23_782c` (targets/consul) | `c22e678` | 2 | 1 | 0 | no |
| CO-03 seed 8 | P2 | `r_2026_10_23_e4af` (targets/consul) | `c22e678` | 2 | 1 | 0 | no |
| CO-03 seed 9 | P2 | `r_2026_10_23_27fa` (targets/consul) | `c22e678` | 0 | 1 | 0 | no |
| CO-03 seed 10 | P2 | `r_2026_10_23_9495` (targets/consul) | `c22e678` | 0 | 1 | 0 | no |
| RE-01 seed 1 | P2 | `r_2026_10_23_1db9` (targets/redis) | `c22e678` | 1 | 1 | 0 | no |
| RE-01 seed 2 | P2 | `r_2026_10_23_e893` (targets/redis) | `c22e678` | 1 | 1 | 0 | no |
| RE-01 seed 3 | P2 | `r_2026_10_23_ff65` (targets/redis) | `c22e678` | 1 | 1 | 0 | no |
| RE-01 seed 4 | P2 | `r_2026_10_23_a28f` (targets/redis) | `c22e678` | 1 | 1 | 0 | no |
| RE-01 seed 5 | P2 | `r_2026_10_23_b035` (targets/redis) | `c22e678` | 1 | 1 | 0 | no |
| RE-01 seed 6 | P2 | `r_2026_10_23_3e76` (targets/redis) | `c22e678` | 1 | 1 | 0 | no |
| RE-01 seed 7 | P2 | `r_2026_10_23_a4f0` (targets/redis) | `c22e678` | 1 | 1 | 0 | no |
| RE-01 seed 8 | P2 | `r_2026_10_23_8eef` (targets/redis) | `c22e678` | 1 | 1 | 0 | no |
| RE-01 seed 9 | P2 | `r_2026_10_23_b5f2` (targets/redis) | `c22e678` | 1 | 1 | 0 | no |
| RE-01 seed 10 | P2 | `r_2026_10_23_aa53` (targets/redis) | `c22e678` | 1 | 1 | 0 | no |

P2 rows are in launch order: one run per seed, each launched with `thesis run --arm <id> --seed <s>` from the arm's target directory by a binary built at `c22e678`. "Volumes after" counts the volumes of the run's own compose project. HC-01 seed 1 is excluded from every count: its world booted on three volumes its own compose project had left behind earlier (OQ-087). Scored results: `../results/P2.md`.

Operator probes (not arms):

| Purpose | Run id | Commit | Exit (shell) | Worlds | Volumes after |
|---|---|---|---|---|---|
| storage-leak workaround check: launched from the project directory | `r_2026_10_20_53a0` (testdata/kvfixture) | `afd338a` | 2 (narrowed by design) | 1 | 0 |
