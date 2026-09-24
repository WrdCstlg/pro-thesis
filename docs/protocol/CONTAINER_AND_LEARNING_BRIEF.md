# Build brief: container parity, a recorded results ledger, and a meta-observation layer

You are building on PRO-THESIS, a fault-injection harness whose entire value is that its verdicts
are true. A false PASS is the worst thing this repository can produce: worse than a crash, worse
than no answer at all.

Read `AGENTS.md` before your first edit. It is not background, it is binding, and most of its rules
exist because something specific went wrong. This brief adds to it and never overrides it.

---

## 0. How to work, and where to stop

**Three work packages, in order. Stop after each one.** Report what you measured, then wait for the
author's approval before committing and before starting the next package. Do not run them
concurrently and do not start WP2 because WP1 is "nearly done".

**When anything is ambiguous, stop and ask.** A wrong guess here is not a bug, it is a false
verdict, which is the one failure mode this project is built to prevent. Stopping costs an hour.
Guessing costs the thing the project is for.

**Every change ships with a failing-first test.** Write the test, run it, watch it fail, then fix.
Then mutate: revert the fix, confirm the test fails again, restore, and confirm the source is
byte-identical (compare SHA-256 before and after). Paste the actual failure output into your
report. A test that passes with the bug present certifies the bug.

**Never assert what you did not measure.** Put the command next to the number. "The container runs
search" is a claim; a pasted transcript with an exit code is a measurement.

---

## 1. State when this brief was written (2026-09-24)

Verify this with `git log -1` and `git status --short` rather than trusting it.

| Fact | Value |
|---|---|
| HEAD | `4e38ad2`, working tree clean |
| Full suite | 36 ok, 0 not ok, 42 total (`pwsh -File scripts/run-tests.ps1`) |
| Next free ledger ids | D-0xx **(088)**, OQ-0xx **(075)**, spelled that way on purpose. See section 9. |
| Harness image | `pro-thesis:dev`, built from the root `Dockerfile`, 302 MB, not published anywhere |
| Container proof | `r_2026_09_24_b498`: exit 1 in 23.6 s, one witnessed violation on key `k/0` |
| Two instructive failures | `r_2026_09_24_a253` and `r_2026_09_24_9c90`, both INCONCLUSIVE because the driver was never told where the cluster was. Read D-087 before touching anything in `internal/control`. |

### Which verbs are known to work inside the container

Measured on 2026-09-24 with the socket mounted and the project bind-mounted:

| Verb | Result |
|---|---|
| `run` | exit 1 with a witnessed violation, and exit 2 twice before that. Works. |
| `oracles verify` / `oracles list` | exit 0, correct lock digest. Works. |
| `diagnose` | exit 0, "every non-terminal outcome attributed to a known problem". Works. |
| `history verify` | exit 0. Works. |
| `search`, `replay`, `regress`, `shrink`, `bisect`, `cluster`, `up`, `down`, `init` | **Never run in the container.** Unknown. |

That last row is most of WP1.

---

## 2. WP1: make the container run the full scope

The container currently proves one path. The goal is that every verb either works in the container
or refuses with a message that says why, and that a misconfigured container is refused **before** it
burns a world rather than after.

### 1.1 Implement `thesis doctor` (highest value in this package)

`doctor` is declared and unimplemented (`cmd/thesis/main.go`, the "Declared, not yet implemented"
list). It is worth more in a container than on a host, where a whole class of misconfiguration
costs a world to discover and a second to check.

**Correction, made after this brief was executed.** An earlier version of this section said two
runs lost on 2026-09-24 were the reason, and that the platform check below "would have saved" them.
That was wrong, and it propagated into D-088 before review caught it. Those runs failed because
`PROTHESIS_TARGETS` was unset and the driver fell back to loopback; the binary was a correct Linux
ELF and every check below would have passed. D-087 fixed that structurally. Do not justify a
pre-flight by a failure it would not have caught.

Implement it as a pre-flight that checks, and **reports every check it ran, including the ones that
passed**, so a reader can tell "nothing was wrong" from "nothing was looked at":

1. **Docker reachable**, and `docker compose version` reports v2. The harness shells out to
   `docker compose`; v1 will not do.
2. **The probe host is reachable.** `probehost.Host()` is where host-side probes and the driver's
   targets both go. Check that it resolves, and say which value is in force and where it came from
   (default, or `PROTHESIS_PROBE_HOST`).
3. **The driver command exists, is executable, and is the right platform.** Read the first bytes
   of the resolved driver binary: `MZ` is a
   Windows PE, `\x7fELF` is Linux. Running in a Linux container with a PE driver must be a refusal
   naming the mismatch, not a generic "not found". Do the same for every oracle executable named
   under `oracles.dir`.
4. **The project directory is writable** by this process, because `.prothesis/runs` is written
   through the mount.
5. **Every declared node's health probe target is well formed** and every node that is probed
   declares a port.

Exit codes are normative and you may not invent one: `0` ready, `2` a check could not be performed,
`5` a check failed and the configuration is wrong. Read `pkg/schema/exit.go`.

Failing-first tests must include: a PE driver refused in a Linux context, a missing driver refused,
a compose v1 daemon refused, and a fully healthy project returning 0 with all checks listed.

### 1.2 Close OQ-074: the parallel-lane port pre-flight

Read OQ-074 in full first. `internal/control/parallel.go` checks whether a host port is free by
binding `127.0.0.1:<port>` locally. Inside a container that is the container's namespace, so a port
busy on the host binds cleanly and the check reports it free.

Two acceptable fixes, in order of preference:

1. **Ask the daemon.** The daemon knows which host ports are published. Derive the answer from it
   rather than from the local netstack. This works from anywhere and is the real fix.
2. **Refuse to answer.** When `probehost.Host()` is not loopback, skip the bind and say in the
   output that lane collision detection is unavailable and why. A check that cannot answer must say
   so; silently reporting "free" is the defect.

Do not do both. If you take option 2, say in your report that you took the weaker one and why.

The failing-first test for this cannot use a real container. Inject the seam (a function variable,
in the style of `harness.dockerBin` or `control.ImageResolver`) and test the decision, not the
socket.

### 1.3 Establish what every other verb does in the container

For each of `search`, `replay`, `regress`, `shrink`, `bisect`, `cluster`, `up`, `down`, `init`: run
it in the container against `testdata/kvfixture`, record the command and the exit code verbatim,
and classify the result as works / fails-with-a-clear-message / fails-silently.

Anything that fails silently is the finding. Fix it if the fix is small and obviously correct;
otherwise record an OQ with the measured evidence and move on. `bisect` walks git revisions and git
is present in the image (`/usr/bin/git`, measured), but `bisect` has never had a recorded live run
anywhere, so a failure there is expected and is not yours to fix in this package.

**Do not** try to make parallel `search` work in the container before 1.2 lands.

---

## 3. WP2: a recorded results ledger, and evidence you can verify

Today a run's evidence is a directory under `.prothesis/runs/`, which is gitignored and exists
nowhere else. There are 197 such bundles locally and no durable, reviewable record of what any of
them concluded. That is the gap this package closes.

### 2.1 `thesis evidence verify`: make the manifest mean something

`internal/recorder/artifacts.go` content-addresses every file in a bundle into `MANIFEST.json`.
**Nothing reads it**, and only a minority of bundles have one. An integrity record nobody checks is
decoration.

Build `thesis evidence verify <bundle>`: recompute every file's digest, compare against the
manifest, and report additions, removals and modifications separately. Preserve and report the
original `bundle_sha`.

Then measure how many of the bundles on disk actually carry a manifest and say so in your report. If
the answer is "a minority", the follow-up question is why `recorder.OpenBundle` is reached from only
some paths; investigate and record what you find, but **do not** rewrite the recorder in this
package.

Exit codes: `0` the bundle matches its manifest, `1` it does not (that is a witnessed breach, with
the offending paths as the witness), `2` there is no manifest to check against, `5` the bundle path
is unreadable.

### 2.2 The results ledger

Add an append-only, committed record of what every run concluded:
`.prothesis/RESULTS.jsonl`, one canonical JSON object per line.

Each line carries, at minimum: `run_id`, the run's own recorded start time (from the bundle, never a
wall clock read at append time), `tool_version` and the build commit, `sut.images`, `profile`, the
canonical fault schedule string, `verdict`, per-world outcome counts, per-oracle status counts,
`bundle_sha` when there is one, and the known-problem ids `diagnose` attributed.

Rules that are not negotiable:

- **Deterministic.** No maps in the serialized output, no floats, no wall clocks, and every sort a
  total order. Two appends of the same bundle must produce identical bytes. This repository has
  already been bitten by a comparator that left two entries tied.
- **Append-only.** Re-running against a bundle already recorded is a no-op, not a duplicate and not
  an edit. Make that a test.
- **It is a record, not a verdict.** Appending must never change an exit code.

Surface it as a verb (`thesis record` or an additive flag on `diagnose`; choose, and say why in the
ledger entry). Read `internal/diagnose/` first, because it already walks the corpus read-only and
you should reuse that walk rather than write a second one that can drift from it.

### 2.3 Wire it into CI

The CI workflow (`.github/workflows/ci.yml`) already asserts exit codes through
`scripts/ci-run.sh WANT_EXIT`. Add a step that appends each live run to the ledger and one that runs
`thesis diagnose`, which already exits 2 when anything is unattributed.

---

## 4. WP3: the meta-observation layer

The ask is that learning is captured and fed back into the next cycle. Most of the machinery already
exists and is not connected:

- `thesis diagnose` attributes every refusal to `.prothesis/known-problems.yaml` (D-082).
- `thesis cluster` discovers candidate failure categories in the unattributed pool (D-084) and
  promotes nothing.
- `internal/search/engine/census_test.go` holds the corpus to a committed floor (D-083).

**Do not build a new parallel system.** Connect these.

### 3.1 Make a known problem carry its evidence

Extend `known-problems.yaml` so each entry records how often it has been observed and when it was
last seen, derived from the results ledger rather than hand-maintained. A KP entry that says "seen
41 times, last on 2026-09-24" is auditable; prose is not.

The derived fields must be regenerable from `RESULTS.jsonl` alone. If regenerating produces
different numbers than the file holds, that is a finding and the tool should say so rather than
silently rewriting.

### 3.2 A cycle report

Add a report that answers, between two points in the ledger: what verdict distribution changed, what
refusal classes are new, which known problems moved in frequency, and **how many times each distinct
fault schedule has reproduced a violation**.

That last one is the valuable output. The confirmation gate is currently k of k (OQ-054), so a
defect that reproduces 7 times in 9 cannot enter the regression corpus, and nothing currently counts
those 7. Counting them is a prerequisite for ever fixing OQ-054, and it is the concrete form of
"learning informs the next iteration".

The report is **advisory**. It promotes nothing into the regression corpus and changes no verdict. A
human still decides. `thesis cluster` already works this way; follow it.

### 3.3 Close the loop in CI

One CI step per cycle that produces the report and fails when a new refusal class is unattributed.
Nothing else in CI may depend on the report's content.

---

## 5. Out of scope. Do not do these.

- **Publishing anything.** Not the image to a registry, not the repository, not a tag. Distribution
  is the author's decision and is not in this brief.
- **The OQ-070 retention pruner.** It needs the `.trash` rename contract and a concurrency story.
  Separate work.
- **Changing the k-of-k gate (OQ-054).** WP3 counts reproductions; it does not change the gate.
  Changing it needs a ruling from the author.
- **The em dash cleanup.** 199 of them across 62 tracked files, cosmetic, and it would bury the
  diff of real work. Separate change.
- **Re-locking the oracle lock**, unless you changed something that compiles into the checker. If
  you did, re-lock with a reason naming exactly what moved, and never write "unchanged source" when
  source changed. Read D-072 and D-060 first.

---

## 6. Host facts that will waste your day if you ignore them

These are measured properties of this machine, not preferences.

- **Use PowerShell. The Bash tool is dead here** and fails with cygwin fork errors.
- **Never run `go test ./...`.** An Application Control policy blocks freshly-compiled test binaries.
  Use `pwsh -File scripts/run-tests.ps1`, which covers both modules and retries a refusal. Narrow
  with `-Match pkg/schema` or `-Run TestFoo`.
- The same policy can block a **stripped release binary**. On 2026-09-24 `bin/thesis.exe` was
  refused while a plain `go build` binary ran fine. If a binary will not start, rebuild it
  unstripped before concluding anything about your code.
- **A hook blocks deletion commands.** `Remove-Item`, `rm <path>`, and the aliases `rd` / `ri` are
  refused. Delete with `[IO.File]::Delete($p)` in a command by itself.
- **Watch the disk.** A multi-agent run once filled the C: drive with probe binaries and corrupted
  the working tree mid-write. Check free space before and after anything large. Docker is capped at
  50 GB and was at roughly 9 GB of images on 2026-09-24.
- **PowerShell will bite you.** `-like '??*'` matches everything because `?` is a wildcard. A
  `[IO.File]::ReadAllText` that throws inside a loop leaves the previous file's contents in the
  variable, and the next write then duplicates the wrong file. That happened on 2026-09-24 and
  created a stray `internal/control/resolve.go` that broke the build. Prefer one file per command.

---

## 7. Docker facts specific to this work

- The harness sets `BUILDX_NO_DEFAULT_ATTESTATIONS=1` on every docker call (D-078). **If you invoke
  docker yourself, you do not get it**, and a rebuild will report a new image digest. Set it.
- Compose rebuilds the fixture image before every world; that is deliberate and the variant lock
  depends on it.
- The container needs the socket mounted and the project bind-mounted. The driver and the oracles
  are the target's executables, are not in the image, and must be `linux/amd64`.
- Expect a WARNING that the checker binary differs from the lock. That is documented and expected:
  the lock records the digest built on the author's machine and `bin/` is gitignored.

---

## 8. What a finished work package looks like

1. Every new behaviour has a test that was red before the code existed, with the failure output
   pasted into your report.
2. Every fix has a mutation: reverted, observed red, restored, source SHA-256 identical.
3. `pwsh -File scripts/run-tests.ps1` passes in full, with the package count stated. It was 36 ok,
   0 not ok, 42 total before you started; it must not go down.
4. `gofmt -l` and `go vet` clean on both modules.
5. `thesis oracles verify` exit 0 in both `testdata/kvfixture` and `targets/etcd`.
6. Documentation rides with the revision (D-081): the README, `AGENTS.md` (including the id
   counters), `docs/ARCHITECTURE.md` when wiring moved, and `docs/TARGETS.md` when the integration
   contract changed. A revision whose documentation lands later is incomplete work.
7. A `DECISIONS.md` entry per package, and an `OPEN_QUESTIONS.md` entry for every weakness you
   measured and did not fix.
8. You state who arbitrated your work, or that nobody has. A session does not arbitrate its own
   work.

---

## 9. The ledger trap that will fail the suite

`internal/ledger` enforces that every `D-nnn` and `OQ-nnn` citation resolves to a heading, and it
scans **every** `.go`, `.md`, `.yaml`, `.yml`, `.ps1` and `.py` file.

So **writing the next id in its real form anywhere fails the whole suite** until the heading exists.
While drafting, spell it the way `AGENTS.md` does: `D-0xx (088)`, not the real form. Write the real
id only in the same change that adds its heading.

Entries are append-only. Supersede an entry by adding a new one that cites it. Never edit the old
text. If you must correct something you just wrote, append a correction paragraph that says what was
wrong, as D-086 does.

---

## 10. Hard prohibitions

- Do not delete anything under `.prothesis/runs/`. The corpus is 2.6 GB, gitignored, and exists
  nowhere else. `r_2026_09_17_5805` and `r_2026_09_17_66d5` are cited by a committed observation
  bundle. The three runs from 2026-09-24 named in section 1 are evidence for D-087.
- Do not "fix" a failing test by weakening its assertion.
- Do not claim a test passes without running it, or that a build reproduces without building twice.
- Do not invent an exit code or collapse two.
- Do not edit a recorded artifact (`verdict.json`, `history.jsonl`, `result.json`) without
  disclosing the edit in the bundle.
- Do not `git add -A`. Stage explicit paths. There are untracked directories in this tree that must
  never be committed.
- Do not commit or push without stating the destination and getting explicit approval. There is more
  than one remote and they are not equivalent. Never name a private repository in a tracked file.
- Do not strip the `Co-Authored-By` trailer or otherwise hide agent authorship. Record the model
  that did the work exactly as the tool reports it.
- Other agents may be editing this tree. Run `git status --short` before editing, and if a file
  changed since you read it, diff it rather than overwriting.
