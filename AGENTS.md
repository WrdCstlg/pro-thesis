# Instructions for coding agents

You are working on **PRO-THESIS**, a fault-injection harness that produces verdicts about
distributed systems. Its entire value is that the verdicts are true. A false PASS is the worst
thing this repository can produce: worse than a crash, worse than no answer at all.

Read this file before your first edit. Most rules below exist because something specific went
wrong; the reason is given so you can tell when the rule applies.

---

## 1. Engineering doctrine: not negotiable

**Every fix ships with a failing-first test.** Write the test, watch it fail, then fix. Then
*mutate*: revert the fix and confirm the test fails again. Paste that failure output into your
report. A test that passes with the bug present is worse than no test, because it certifies the
bug. Two tests in this repo were replaced for exactly that: one was `if len(x) == 2 && (...)`,
which passed silently whenever the length changed; another tested an API that was never broken
while the defect sat in its caller.

**Never assert what you did not measure.** Give the command next to the number. "Two builds are
byte-identical" is a claim; `scripts/build.ps1 -Verify` printing two equal digests is a
measurement. Ledger prose, code comments and commit messages are all held to this. Entries have
had to be rewritten for saying "rebuilt from unchanged source" when the source had changed.

**The refusal surface is the quality metric.** A tool that cannot answer must say INCONCLUSIVE,
loudly. Silence is the defect. When you add a capture, a check or a gate, the first question is
"how does a reader tell *nothing happened* from *we failed to observe it*?"

**Determinism.** `.thesis` world files are content-addressed canonical JSON. Anything reaching
those bytes must be deterministic: no maps, no floats, no wall clocks, and every sort must be a
*total* order. A comparator that left two entries tied made the same world hash two different
ways depending on the order containers happened to bind.

**Exit codes are normative.** `0` PASS · `1` FAIL · `2` INCONCLUSIVE · `3` BUDGET_EXHAUSTED ·
`4` ORACLE_DRIFT · `5` CONFIG_ERROR. Never invent a code and never collapse two.

**The ledgers are the memory.** `DECISIONS.md` is append-only: supersede an entry by adding a
new one that cites it, never by editing the old text. `OPEN_QUESTIONS.md` records *measured*
weaknesses. Every `D-0xx` / `OQ-0xx` citation must resolve; a test enforces it
(`internal/ledger`). Next free ids: **D-0xx (085)**, **OQ-0xx (074)**, spelled that way on
purpose. The citation test matches `\b(?:OQ|D)-\d{3}[a-z]?\b` in every `.go`, `.md`, `.yaml`, `.yml`,
`.ps1` and `.py` file, so writing a not-yet-allocated id in its real form anywhere fails the suite
until the heading exists. Add the heading in the same change, or do not write the id.

**Documentation rides with the revision (author's rule, 2026-09-22).** Whenever the author
revises the system, the documentation that describes it is revised in the SAME change: the
README (including its authorship section and ledger ranges), this file's state and id counters,
and `docs/ARCHITECTURE.md` when wiring moved. A revision whose documentation lands later is
incomplete work, and the README records what the author built versus what agents built under his
specifications; keep that attribution true.

---

## 2. Properties of the development host

These are measured properties of the machine this was developed on, not folklore.

**Use PowerShell.** On this host the Bash tool fails with cygwin fork errors.

**Never run `go test ./...` directly.** An Application Control policy blocks execution of
freshly-compiled test binaries from temp directories. Use:

```powershell
pwsh -File scripts/run-tests.ps1                    # both modules, 32 packages
pwsh -File scripts/run-tests.ps1 -Match pkg/schema  # one package
pwsh -File scripts/run-tests.ps1 -Run TestFoo       # one test
```

The wrapper compiles each package to `bin/`, runs it from there, retries a refusal three times,
and falls back to an *unstripped* build when the policy rejects a stripped one; measured: one
package was refused nine times stripped and ran first try unstripped. It covers **both** modules
(the root module and `testdata/kvfixture`).

**A hook blocks deletion commands.** Anything containing `Remove-Item`, `rm <path>`, or the bare
PowerShell aliases `rd` / `ri` is refused, including a variable innocently named `$rd`. Delete
with `[IO.File]::Delete($p)` or `[IO.Directory]::Delete($p, $true)` in a command by itself.

**Disk discipline.** A multi-agent run once filled the C: drive because every
sub-agent compiled probe binaries into scratch (1.25 GB of `.exe`). Writes then failed *mid-file*
and silently corrupted the working tree: a `var (` line vanished from a `.go` file, the last line
of `.gitignore` vanished, and six files were left in a state matching no commit. If you fan out
agents, forbid them from compiling. Check free space before and after any large operation.

**Docker** is capped at 50 GB. Compose rebuilds the kv image before **every** world
(`pull_policy: build`; the variant lock depends on it). Until D-078 that cost gigabytes of build
cache per world and a new image id every time: the run corpus sat inside the build context, and
buildx's default provenance attestation made even a fully cached rebuild report a new index digest.
Both are fixed (`.prothesis/` is in the fixture's `.dockerignore`, and the harness sets
`BUILDX_NO_DEFAULT_ATTESTATIONS=1` on every docker call) so a rebuild is under a second and
`sut.images` is stable while the build cache lives. It is NOT stable across a cold cache (OQ-068,
still open on that half): after `docker builder prune`, expect one new digest. If you invoke docker
yourself outside `internal/harness`, you do not get the switch. A sick daemon once took 32 s to answer
`docker inspect` on a nonexistent container, which spent whole world budgets and turned two
passing tests red: see OQ-067. Since D-073 that call is bounded to five seconds (as hard as the
killed child's exit) and unit worlds
reach it only through the `ImageResolver` seam, so a sick daemon can cost a live world its
provenance and nothing else. If you construct a `control.Runner` that boots a stub-backend world
in a unit test, name `ImageResolver: stubImages{}`; the Docker-backed fixture tests and
`TestNewRunnerResolvesImagesFromTheDaemonByDefault` deliberately do not.

---

## 3. Commands that matter

```powershell
# Build release binaries WITH provenance. Never use bare `go build` for these.
pwsh -File scripts/build.ps1
pwsh -File scripts/build.ps1 -Verify   # builds twice, fails if the bytes differ

# Verify the oracle lock (run from the fixture directory)
cd testdata/kvfixture; ..\..\bin\thesis.exe oracles verify

# Attribute every recorded non-terminal outcome to the known-problem registry
cd testdata/kvfixture; ..\..\bin\thesis.exe diagnose   # exit 2 = something is UNATTRIBUTED

# Read a binary's provenance
.\bin\thesis.exe version
.\testdata\kvfixture\bin\linearizable-kv.exe -version
```

`scripts/build.ps1` stamps commit, dirty flag and **source date** (the commit's committer date,
never a wall clock) and builds with `-trimpath -buildvcs=false`. Both flags are load-bearing:
`linearizable-kv.exe` is fingerprinted by SHA-256 in `.prothesis/lock`, and any per-build
variation makes every ordinary rebuild look like a swapped checker. See D-072.

**If you change anything that compiles into the checker, its digest moves.** Re-lock with a reason
that names *why* it moved:

```powershell
..\..\bin\thesis.exe oracles lock --reason "what changed in the source, or that only the stamp moved"
```

A reason that says "unchanged source" when source changed is a false entry in the one record that
exists to catch tampering; D-072 records two such entries and why they happened.

---

## 4. State when this file was last updated

Do not trust this section over `git log -1` and `git status --short`; it is a snapshot, and other
agents commit here. At the first public commit: full suite 32 packages passing on the build host,
build, vet and gofmt clean on both modules, `oracles verify` exit 0 in both projects, measured
2026-09-21. The public history begins at that commit. Commit ids that appear inside build stamps,
verdicts, evidence bundles and ledger entries written before it refer to the development history
and do not resolve in this repository. The next stamped build will move the checker's fingerprint,
because the stamp is the commit id; re-lock with a reason that says only the stamp moved, after
proving it the usual way (D-072, D-078). Commit with the repository-local `user.email`, which is the
GitHub noreply address; the author's personal address must not enter the history. Anything on top
of the first public commit is described by `git status`, not by this file.

---

## 5. The work in flight: L1b, L1c, L3

Three features are designed but **not implemented**. Everything below is measured: reproduce any
figure you intend to rely on.

### Measured ground truth

| Fact | Where it bites |
|---|---|
| `testdata/kvfixture/.prothesis/runs` holds **156 bundles, 2,481,611,442 B**, gitignored, existing nowhere else | No second copy. Deletion is final. |
| 118 have `verdict.json`: FAIL 51, INCONCLUSIVE 43, PASS 23, BUDGET_EXHAUSTED 1. **38 have none** | The policy names only *passing* and *failing*: over half the population is unaddressed |
| Bytes: `history.jsonl` **74.1%**, `telemetry.json` 13.9%, `telemetry.jsonl` 10.9%, all else **<1%** | 98.9% of the space is three bulk files |
| Each world's `result.json` already carries `{world, seed, outcome}` | L3 needs an *aggregator*, not new per-world persistence |
| `MANIFEST.json` content-addresses **every** file (`internal/recorder/artifacts.go:414`), but only **8 of 156** bundles have one and **nothing reads it** | File-level pruning is possible, but it must rewrite the manifest and preserve the original `bundle_sha`, or the bundle's own integrity record becomes false |
| `retain_passing: 3` is the scaffolded default (`pkg/schema/config_validate.go:88`). Simulated: deleting whole directories under the literal policy frees **104.6 MiB = 4.42%** and pins 2,262 MiB forever | Whole-directory deletion is **nearly a no-op** on this corpus. PASS is the only class the policy deletes and it is the smallest, at 5.6% of bytes |
| `RetainPolicy.Keeps` (`pkg/schema/retain.go:63`) has **zero callers anywhere** | The policy is parsed, validated, defaulted, and enforced by nothing |
| The RUNNING sentinel is written only by `recorder.OpenBundle` (reached solely from `cmd/thesis/up.go:55`) and the `--keep-up` path (`internal/control/runworld.go:147`) | `thesis run` and `search` bundles, 118 of 156, **never get one**. Zero sentinels on disk is not evidence of clean shutdown; it means a pruner has no liveness signal for them |
| 774 `result.json` files record per-world outcomes: pass 285, violation 205, inconclusive 284 | The per-world trial frame is **490 trials vs 74 run-level; 6.6×**. The unit of observation decides the sample size |
| The entire on-disk corpus predates D-071 | No existing run may be pooled with new ones; the gate must stratify or refuse |
| `verdict.json`'s key is **`verdict`**, not `outcome` (`outcome` is the per-world key in `result.json`) | A corpus scan keyed on the wrong one returns empty for all 118 bundles and looks like a clean tree |
| `internal/recorder/artifacts.go:38` documents `worlds/<6-digit>/`, but disk holds **930 `world-*` dirs and zero `worlds/`** (`internal/control/runner.go:567`) | Code written to the documented layout matches nothing |

### Landmines

- **`internal/search/engine/verdict_test.go`** scans that same corpus and calls `t.Skipf` when it
  is empty. A pruner that shrinks it converts PASS into SKIP with no suite failure. Whatever you
  build must make that impossible, or make it loud.
- **`r_2026_09_17_5805` and `r_2026_09_17_66d5`** are cited by a committed observation bundle.
  Never delete them.
- **Nothing gives mutual exclusion** between two `thesis` processes in one project directory, and
  per the row above, the bundles a pruner would most want to judge carry no liveness marker at all.
  A pruner needs a stated concurrency contract; "no sentinel" must not be read as "safe to delete".
- **`.trash` is normative.** The project's own committed spec requires a pruner to *rename* a run
  directory into `.prothesis/runs/.trash/` rather than unlink it, with a separate explicit sweep to
  reclaim. Note the spec covers whole directories only: if you strip files from *inside* a bundle,
  that mitigation does not cover you and you must extend it rather than skip it.
- **Deleting files inside a bundle falsifies its `MANIFEST.json`.** Rewrite the manifest and
  preserve the original `bundle_sha`, or the bundle's own integrity record becomes false.
- **37% of recorded verdicts are INCONCLUSIVE or BUDGET_EXHAUSTED**, not Bernoulli trials. L3's
  trial definition must exclude them, or the statistic is over a population that mixes "the defect
  did not appear" with "we never got to look".

### Decide before writing code

L3's trial definition and retention floor must be settled *first*: whichever feature lands first
silently fixes the other's parameters. Shipping L1b as-configured would cap L3's PASS-side sample
at 3 by accident.

---

## 6. Commits and pushes

- **Development is private; publishing is deliberate.** In the author's working copy, `origin` is
  the private development repository and routine pushes go there only. The public repository
  (`WrdCstlg/pro-thesis`, remote `public`) receives `git push public master` only when the author
  says to publish. Its `master` refuses force-pushes and deletion, so nothing published can be
  rewritten. Nothing that names the private repository belongs in a tracked file.
- **State the destination before every commit and push**, and get explicit approval. Measure
  visibility with `gh repo view <owner>/<repo> --json isPrivate` rather than assuming it, and
  verify each remote with `git ls-remote` after pushing.
- Cite the ledger ids you touched in the commit subject, and keep the `Co-Authored-By` trailer.
  Do not strip trailers or otherwise hide agent authorship.
- Never force-push a published branch without explicit instruction.

---

## 7. Other agents may be editing this tree right now

More than one agent system has worked in this tree at the same time, and one has overwritten
another's uncommitted work. Before editing: `git status --short` and check file mtimes. If a file changed
since you read it, stop and diff it rather than overwriting. If the working tree contains changes
you did not make, say so instead of committing them blind.

---

## 8. Do not

- Delete anything under `.prothesis/runs/` without an explicit, specific instruction.
- Edit a recorded artifact (`verdict.json`, `history.jsonl`, `result.json`) without disclosing the
  edit in the bundle. Silent post-hoc edits destroy the only thing evidence is for. If you must
  redact, say so in the record and keep the original hash.
- Claim a test passes without running it, or a build reproduces without building it twice.
- "Fix" a failing test by weakening its assertion.

---

## 9. Which model does what

The author's ruling of 2026-09-20 (D-076, ruling 7): **model selection follows the task.**

- **Building** (writing code, tests and ledger drafts) is done by the second most capable model
  available to the session.
- **Arbitrating** (review, adjudicating a disagreement between sessions or models, the last read
  before something reaches the author) is done by the most capable coding model available.
- **A session does not arbitrate its own work.** If you built it, someone else judges it; say in
  your report who that was, or that nobody has yet.

No model is named here on purpose: the ranking changes, and the rule should not. Record the model
that did the work in the commit's `Co-Authored-By` trailer, exactly as the tool reports it.
