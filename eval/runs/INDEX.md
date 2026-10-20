# Run index

Every run the evaluation suite made, in order. The artifacts stay in the target's
`.prothesis/runs/<run id>/` (or, once archived, `.prothesis/archive/<run id>.tar.zst` with its
`.sha256.json`). "Volumes after" is the count of the target's compose-labelled volumes left after
the run; anything other than 0 excludes the run's worlds from the precision and reproducibility
figures.

| Arm | Phase | Run id | Commit | Exit (shell) | Worlds | Volumes after | Archived |
|---|---|---|---|---|---|---|---|

Operator probes (not arms):

| Purpose | Run id | Commit | Exit (shell) | Worlds | Volumes after |
|---|---|---|---|---|---|
| storage-leak workaround check: launched from the project directory | `r_2026_10_20_53a0` (testdata/kvfixture) | `afd338a` | 2 (narrowed by design) | 1 | 0 |
