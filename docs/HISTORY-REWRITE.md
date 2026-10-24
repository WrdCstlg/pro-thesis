# History rewrite of 2026-10-23

On 2026-10-23 the history of this repository was rewritten, identically in both public copies
(`WrdCstlg/pro-thesis` and `piercepartners/Pro-synthesis`). It removed material that belongs in a private
repository:

- the development notes, ledgers, plans, reviews and specs that were public until they moved to a
  private companion repository on 2026-10-19;
- the commit-message text that summarised them.

Older commit messages now keep only their subject line.

What did not change:

- the current tree;
- every author and committer date.

Every commit id changed. Four commits disappeared, because they changed only files that were removed.

## Commits cited by their old ids

Files in this tree still cite these commits by the ids they had before the rewrite:

| Old id | New id | Cited in |
|---|---|---|
| `15358c0` | `ca4c9d5` | `scripts/gaming-benchmark.ps1`, `docs/observations/2026-10-12-GAMING-BASELINE/benchmark.json` |
| `5e55347` | `ef579d9` | `eval/runs/INDEX.md` |
| `5ffeb3f` | `d3ffd54` | `README.md`, `docs/EVIDENCE.md` |
| `7f5ee3c` | `630584b` | `scripts/gaming-benchmark.ps1`, `docs/observations/2026-10-12-GAMING-BASELINE/benchmark.json` |
| `7fb03ba` | `da30d61` | `README.md`, `docs/media/03-cheat.svg`, `docs/media/demo/03-cheat.cast` |
| `afd338a` | `04b0114` | `eval/runs/INDEX.md` |
| `b619c28` | `dee8efe` | `docs/EVIDENCE.md`, `docs/media/` (recorded demos and their measurements) |
| `c22e678` | `a588d4d` | `eval/results/P2.md`, `eval/runs/INDEX.md` |

Recorded artifacts (verdicts, locks, demo recordings) are not edited to the new ids. They record what
was true when they were made.

## The P2 pre-registration

The ten P2 predictions in `eval/predictions/` were registered in the commit that was `c22e678` and is
now `a588d4d`. Its contents and dates are unchanged.

Before the rewrite, GitHub Actions ran this repository's CI on `c22e678` twice, at 2026-10-23T04:47:10Z
and 05:02:42Z, both green. P2's first world started at 2026-10-23T12:24:54Z. Those runs are
`https://github.com/WrdCstlg/pro-thesis/actions/runs/39902173769` and `.../39903580215`.
