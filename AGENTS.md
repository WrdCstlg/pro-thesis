# Instructions for coding agents

You are working on **PRO-THESIS**, a fault-injection harness that produces verdicts about
distributed systems. Its entire value is that the verdicts are true. A false PASS is the worst
thing this repository can produce: worse than a crash, worse than no answer at all.

Read this file before your first edit. Most rules below exist because something specific went
wrong; the reason is given so you can tell when the rule applies.

---

## 0. Before any work: read the decisions (D-101)

The author's global rule (Global Rules section 10, at the end of this file), applied here. Before
your first edit, and before you propose a plan:

1. Read this file.
2. List the headings of `DECISIONS.md` and `OPEN_QUESTIONS.md` (`Select-String '^## (D|OQ)-'`).
   Read in full every entry added since the last commit you know of; a new session reads at least
   the ten most recent decisions. Read in full every entry cited by the files you will touch.
3. Read `docs/design/10-hardening.plan.md`. It is the work in flight, and D-097 records the
   rulings it rests on.
4. Run `git status --short`, `git log --oneline -10`, `git worktree list` and
   `git ls-remote origin master`. Sessions of other models work in their own worktrees at the same
   time (section 7).
5. Name the entries you read, by id, in your first report.

Until you have done this you may read files and run offline checks, but make no edit, commit or
push. When you make or receive a decision, record it in `DECISIONS.md` in the same change that acts
on it: a decision that exists only in a chat is lost to the next session. Every entry carries an
`APPROVED BY:` line naming the human instruction, or `nobody, pending` when no human approved it;
agent confidence is not approval. Two sessions that skipped this step on 2026-10-12 recorded opposite
answers from the author to the same question, under the same id (D-100; see D-101).

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
(`internal/ledger`). Next free ids: **D-0xx (104)**, **OQ-0xx (087)**, spelled that way on
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
pwsh -File scripts/run-tests.ps1                    # both modules; PACKAGES and TESTS lines (D-098)
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

The corpus figures in the table are a snapshot, not the current state. The corpus has grown
since: re-measured 2026-09-27, `testdata/kvfixture/.prothesis/runs` held 204 run bundles in
2,957,378,716 B, 160 of them with `verdict.json` (FAIL 84, INCONCLUSIVE 49, PASS 26,
BUDGET_EXHAUSTED 1). Re-derive any row before you rely on it.

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
  Never delete them. **`r_2026_10_12_5bb0` and `r_2026_10_12_332c`** are the evidence of OQ-085,
  and **the nineteen bundles D-099 cites** are the gaming benchmark's baseline; the same applies.
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

- **Both remotes are public since 2026-10-12.** `origin` (`piercepartners/Pro-synthesis`) was the
  private development repository until the author published it on 2026-10-12; `public`
  (`WrdCstlg/pro-thesis`) has always been public. Every push to either is a publication: before
  any push, confirm the range contains no secrets, regulated material, client-identifying
  material or unfiled IP. Both `master`s refuse force-pushes and deletion, administrators
  included (`origin`'s since D-102), so nothing published on either can be rewritten.
- **State the destination before every commit and push**, and get explicit approval. Measure
  visibility with `gh repo view <owner>/<repo> --json isPrivate` rather than assuming it, and
  verify each remote with `git ls-remote` after pushing.
- Cite the ledger ids you touched in the commit subject. Since D-100 (2026-10-12) commits carry
  NO `Co-Authored-By` trailer, per the owner's global rule; name the model in the commit body's
  prose instead. Never strip existing trailers from history or otherwise hide agent authorship.
- Never force-push a published branch without explicit instruction.

---

## 7. Other agents may be editing this tree right now

More than one agent system has worked in this tree at the same time, and one has overwritten
another's uncommitted work. Before editing: `git status --short` and check file mtimes. If a file changed
since you read it, stop and diff it rather than overwriting. If the working tree contains changes
you did not make, say so instead of committing them blind.

**Work in your own worktree (D-097).** Each agent session creates its own `git worktree` on its own
branch, cut from `master`, and commits there; only the author merges to `master`. Put the worktree
outside a temporary directory, or the host's Application Control policy refuses the test binaries
(section 2). On 2026-10-10 two sessions shared one working tree, and a revert, a new fix and
formatting edits ended up interleaved in it, partly staged.

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
that did the work in the commit body's prose, exactly as the tool reports it; the
`Co-Authored-By` trailer that used to carry this was discontinued by D-100 (2026-10-12).

<!-- GLOBAL-RULES v3 PUBLIC-SAFE BEGIN (2026-10-09). The public form of the portfolio standard v3 (2026-10-07), for repositories that are published. Section 10 added 2026-10-13 (D-101). -->
## Global Rules (portfolio standard v3)

> Project-specific rules elsewhere in this file add to these and may be stricter. If one contradicts a rule below, stop and ask. Never pick one silently.

### 1. Environment
- Windows, PowerShell 7. Use PowerShell syntax for all commands. (Claude Code's Bash tool fails here with cygwin fork errors.)
- Model names are immutable. Never edit, "correct", or update a model name: your knowledge of models is always outdated.

### 2. Anchor the session before acting
- If the workspace is a single repo, work there. If it contains several repos (a parent folder holding many projects) and the user
  has not named one, ask. Never infer the project from open editor tabs.
- You have no memory of earlier conversations. On "continue" / "pick up where we left off", first read the repo's
  AGENTS.md, BACKLOG.md (or equivalent), and recent git log/status; state what you found; then proceed.
- Stay in the named repo unless the user moves you or the task is explicitly cross-repo.

### 3. Evidence standard: SHOWN, NOT CLAIMED
- Every claim about state, results, or behavior must rest on tool output from this session (a command run or a file read).
  Never describe a command you did not run or a result you did not see.
- In reports, label anything not verified this session as unverified or inherited.
- After every file write, read it back before calling it done.
- A failing check is a finding. Never weaken a test, threshold, or gate to make it pass.
- If an earlier claim of yours proves wrong, correct it explicitly before continuing.

### 4. Integrity: no fabricated work
- An honest "not done", "blocked", or "could not verify" is a successful outcome. A fabricated or inflated result is the one unrecoverable failure.
- Never make a check pass by: skipping, xfail-ing, deleting, or disabling tests; narrowing collection (-k, --deselect, ignore paths);
  hardcoding expected outputs or special-casing test inputs; mocking the unit under test; swallowing exceptions;
  writing assertions that cannot fail; lowering coverage or thresholds; or bypassing hooks or CI (--no-verify, SKIP=).
- Report every change to tests, fixtures, thresholds, or CI config as its own line item with the reason. Never fold it into "fixed".
- Every reported result states the exact command, exit code, and pass/fail/skip counts as printed. Never report counts you did not see.
- Acceptance and refusal tests are fixed in the spec before implementation. Changing them afterward needs user approval.
- Never edit `.git/hooks`, CI workflows, or hook configs to get past a failure.

### 5. Boundaries
- "Read-only" / "probe" / "prepare" / "don't execute": reading files and running offline local checks is allowed;
  changing tracked files, git state, remotes, installed packages, or spending money is not.
- STOP means stop. At a stated phase boundary, report and wait.
- Live or paid runs (API calls, model evals) need a spend cap approved by the user before launch.

### 6. Irreversible actions
Git and remote (commit, push, merge, branch deletion, visibility change):
- Before acting, state: repo, remote URL, account, branch, visibility (check with `gh repo view --json visibility`), and exactly what is included.
- Making a repo public, force-pushing, or rewriting history requires explicit confirmation in the same turn.
  If the repo is already public, no visibility confirmation is needed, but every push to it is publication: first confirm the diff
  contains no secrets, regulated material, client-identifying material, or unfiled IP. Force-push and history rewrite stay confirmation-only.
- Before any repo is made public, scan the full git history (not just HEAD) for secrets, credentials, regulated material,
  client-identifying information, and unfiled IP. Report findings and wait for the user's go-ahead.
- Fast-forward merges only.
- Commit trailers: follow the repo's AGENTS.md. If it says nothing, ask before the first commit in that repo.
- Never commit, push, or send to an external service any secret, credential, or regulated material.
- A terse "push" / "commit" / "run it" gets the same checks as a detailed request.
Local: deleting or overwriting user files outside `scratch/` requires confirmation. Name the files first.

### 7. Engineering discipline
- Fix the blocker. If you cannot (it needs the user, hardware, or access), say so plainly, name what is needed,
  and do not present lower-priority work as progress on it.
- For systems defined by their connections (APIs, pipelines, Docker stacks), write the integration test first; unit tests fill gaps.
- Specify and test what a module refuses before its happy path. A refusal or verification test counts only after
  you have seen it fail against a deliberately broken implementation.
- Priority: verification > refusal > observability > features.
- Be honest about novelty: novel research vs applied engineering vs standard implementation.
- Infrastructure (Docker, CI, .env, migrations) is code: edit → read back → validate → test.

### 8. Adversarial Persona Loop: mandatory for every new feature
- New features only (new capability, endpoint, module, or user-facing behavior), not bug fixes, refactors, docs, or tests. If unsure, ask.
- Before writing feature code, read and follow the author's Adversarial Persona Loop protocol, which is kept outside this repository.
  If it is not available to the session, say so and stop. Do not run the loop from memory.
- If the repo has no persona instantiation yet, draft one and get the user's approval before the first cycle.
- Write ENGINEER's spec in the repo's existing spec/design/ADR location (e.g. openspec/specs, docs/design, docs/adr, DECISIONS.md);
  if none exists, use `docs/specs/YYYY-MM-DD-<feature>.md`. Specs are dated, append-only records: supersede, never rewrite.
- The spec names its acceptance and refusal tests. Code is written only from that spec.
- Ask before committing a spec to a public repo or one holding unfiled IP or regulated material.

### 9. Hygiene
- Throwaway scripts and outputs go outside the repo or in a gitignored `scratch/`, never the repo root.
  Verification worth keeping is promoted into the test suite.
- Remove worktrees you created with `git worktree remove`, and their branches with `git branch -d` (never `-D`) once merged.
  Ask before removing anything unmerged.

### 10. Decisions are recorded, and every session reads them first (adopted 2026-10-12)
- Every decision that binds later work (a ruling the user gives, a design choice, a change of process, an alternative
  rejected) is recorded in the repo's decision record in the same change that acts on it: `DECISIONS.md` if the repo has
  one, otherwise its ADR or spec location, otherwise a new `DECISIONS.md`. A decision that exists only in a chat is not
  recorded. Supersede an entry with a new one that cites it; never edit an old one. Each entry names who approved it
  (`APPROVED BY:` and the human instruction, or `nobody, pending`); agent confidence is not approval.
- Before its first edit, and before proposing a plan, every session of every model and tool reads: the repo's AGENTS.md;
  the headings of every entry in the decision record; in full, the entries added since the last commit it knows of (a new
  session: at least the ten most recent) and every entry the files it will touch cite; then `git status`, `git log` and
  `git worktree list`, because another session may be mid-change.
- The session's first report names the entries it read, by id. Until it has done this, a session may read and run
  offline checks, but makes no edit, commit or push.
<!-- GLOBAL-RULES v3 PUBLIC-SAFE END -->
