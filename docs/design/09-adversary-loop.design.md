# 09-adversary-loop.design

Status: DESIGN, 2026-09-20. Nothing here is implemented except where a ledger
entry is cited. The seven rulings at the end were made by the author on
2026-09-20 and are recorded in D-076; everything else is the agent's proposal
and is reviewable.

## Summary

A continuous campaign in which a **worker** model (local by default) proposes
fault schedules world after world, and a **meta layer** above it studies what
the campaign has and has not found and widens it. The meta layer is two roles
with two different models: a **builder** that writes proposals, and an
**arbiter** (the most capable coding model the user has) that judges them
before a human does. Everything any of the three produces reaches the system
under test only through the same validators a human-authored schedule goes
through. The meta layer widens the campaign two ways: by proposing
**hypothesis families** the worker instantiates, and by proposing **planted
defects**; new bugs written
into the reference fixture as build variants, each with a pre-registered claim
about which fault should make which oracle fire. A planted defect the campaign
fails to detect is the most valuable output this design has: it is a measured
gap in the harness, found without waiting for a real system to fall into it.

No model can write to the harness, the oracles, the lock or any lock-covered
value. Every **high-risk item** stops for a human, who is shown a full
diagnosis (the review packet) and whose approval is bound to the packet's
hash. Any model, cloud or local, can fill either role.

The worker already exists: `search.strategy: llm` (D-075, with OQ-069's
accepted risk that a proposal stream is not seed-reproducible). This design is
the loop around it, the layer above it, and the gate between both and anything
dangerous.

## Terms

| Term | Meaning |
|---|---|
| **worker** | The model that proposes one fault schedule per world. D-075's strategy. Local by default. |
| **meta layer** | Everything above the worker: the builder and the arbiter. Where this document says "the meta", it means that layer. |
| **builder** | The model that reads campaign results and WRITES: hypothesis families and planted-defect proposals. By the author's ruling, the second most capable model available. No default. |
| **arbiter** | The model that JUDGES what the builder wrote: refuses duplicates and incoherent proposals, writes the diagnosis in a review packet, adjudicates disputed outcomes, proposes budgets. The most capable coding model available. It can reject; it can never approve. No default. |
| **campaign** | A bounded, resumable run of many searches under one budget, with a ledger. `thesis campaign`. |
| **library** | Three append-only records: the regression corpus (exists), `hypotheses.jsonl`, `defects.jsonl`. |
| **variant** | A build of the fixture selected by `KV_VARIANT`. Today: `buggy`, `kvfixed`. |
| **high-risk item** | Anything in the register below marked GATED. It does not proceed without an approval record. |
| **review packet** | The full diagnosis of one high-risk item, content-addressed, rendered for a human. |

## Decisions (12)

### D1: Three layers, one write surface each

**Choice.** The worker writes *schedules*. The meta writes *hypothesis
families* and *defect proposals*. The campaign writes *run artifacts and the
library*. Nothing else. The harness (`cmd/`, `internal/`, `pkg/`), every oracle
definition, `.prothesis/lock` and every lock-covered key of `prothesis.yaml`
are outside all three surfaces.

**Rationale.** The repository's threat model is an agent that would rather
weaken a gate than fix a defect (SECURITY.md). Putting two more models in the
loop multiplies that adversary. The defence that scales is not vigilance, it
is a write surface small enough to check mechanically: a schedule is refused
or accepted by `perturber.Compile`; a hypothesis is data rendered into a
prompt; a defect proposal is inert text until a human approves it.

**Enforced by.** A campaign-end check that `git diff --quiet` holds over the
protected paths and that both locks still verify; a failing-first test that
plants a write into a protected path from inside a fake provider and requires
the campaign to stop with exit 4. A breach is `ORACLE_DRIFT`, not a warning.

**Rejected.** Letting the meta layer propose harness changes directly (new
oracles, new fault kinds). That is the most tempting reading of "improve the
system" and the one that ends with a model grading its own homework. A gap the
campaign finds becomes a drafted `OPEN_QUESTIONS.md` entry with its evidence;
closing it is engineering work done under the ordinary doctrine.

### D2: Any model: three transports, three roles

**Choice.** A provider is one of three transports, and each of the three roles
(worker, builder, arbiter (D12)) binds to one:

| `provider` | Wire format | Covers |
|---|---|---|
| `openai` | chat-completions JSON | llama.cpp, vLLM, Ollama's `/v1`, LM Studio, OpenAI, and the compatibility endpoints most cloud vendors publish |
| `anthropic` | Messages API JSON | Anthropic's API, which is not chat-completions shaped |
| `command` | one JSON request on stdin, one JSON response on stdout, exit 0/1/2 | anything else: a vendor SDK, a gateway, a model behind a queue |

```yaml
models:
  worker:
    provider: openai
    endpoint: http://localhost:8080/v1/chat/completions
    model: qwen3.8                 # passed through verbatim; never rewritten
  builder:                         # writes proposals: the second most capable model you have
    provider: anthropic            # or openai, or command
    model: <the identifier your provider expects>
    api_key_env: ANTHROPIC_API_KEY # the NAME of a variable; the key is never in a file
  arbiter:                         # judges them: the most capable coding model you have
    provider: openai
    endpoint: <your provider's chat-completions URL>
    model: <the identifier your provider expects>
    api_key_env: ARBITER_API_KEY
```

`search.llm` (D-075) becomes shorthand for `models.worker` so existing configs
keep working.

**Rationale.** Two wire formats cover nearly every model that exists; the
third covers the rest without a code change, and it is the idiom this
repository already trusts for the thing that decides the verdict: external
oracles are programs spoken to over a pipe with a timeout and a process-tree
kill. "Compatible with any LLM" is a promise only an adapter seam can keep.

**Carried over from D-075, unchanged.** A real timeout on every call; context
cancellation honoured; the response body capped; only the answer channel
parsed, never a reasoning channel; the key read from the environment and never
recorded; every request and response appended to the run's audit trail.

**Rejected.** One SDK per vendor (a dependency per vendor in a tree that has
one dependency). A plugin system (nothing here needs to load code). Defaulting
the builder or the arbiter (a compiled-in cloud default is an egress decision
made on the user's behalf).

### D3: Temperature: pinned for the worker, recorded for the meta layer

**Choice.** The worker keeps D-075's rule: `temperature: 0`, no knob. The
builder and the arbiter each have an optional `temperature`, recorded verbatim
in every audit line.

**Rationale.** The worker's output becomes a world, and the nearer that is to
reproducible the better the evidence. The meta's output never becomes a world
without passing the validator (a hypothesis) or a human (a defect), and its
job is novelty. Reproducibility is not the property that protects it;
auditability is.

### D4: `thesis campaign`: a loop with the verdict contract

**Choice.** A new verb. Each iteration: run `search` with the worker for N
worlds against one variant → on a violation, fault-level `shrink` → confirm
under the k-of-n rule → add to the corpus if it clears the floor → append one
line to `campaign.jsonl`. Every M iterations: residue check, cache prune,
retention sweep, and a meta step. The campaign is resumable from its ledger.

Exit codes are the existing six and nothing else: `1` if any world in the
campaign carried a violation, `0` only if none did and no run was narrowed or
unevaluable, `2` if nothing could be proven, `3` if the budget ended first,
`4` **immediately** on any lock mismatch or protected-path write, `5` on a
configuration error. A campaign never continues past `4` or `5`.

**Refuses to start** on an unstamped binary (D-070, D-072): a campaign is
thousands of verdicts, and on 2026-09-19 a single smoke run on a bare
`go build` reported `commit: 53cf41c` over a twelve-file-dirty tree. It also
takes an exclusive, PID-stamped lock file in `.prothesis/`, because nothing
today gives two `thesis` processes mutual exclusion in one project directory.

**Rejected.** A PowerShell or shell loop around the existing verbs. It would
work by Friday and it would be the one component with no exit-code discipline,
no provenance and no tests.

### D5: The library is three append-only files

| File | Written by | Holds |
|---|---|---|
| `.prothesis/regressions/w_xxxx.thesis` | campaign, through `corpus.Add` | worlds that reproduce, content-addressed (exists) |
| `.prothesis/library/hypotheses.jsonl` | campaign | each hypothesis family, who proposed it, worlds spent, outcomes by class |
| `.prothesis/library/defects.jsonl` | campaign | each planted defect, its pre-registration, DETECTED / NOT DETECTED / UNEVALUABLE, the oracle and fault that caught it, the run ids |

All three are committed, LF-only, never rewritten. A model proposes; only the
campaign appends, and only measured outcomes.

### D6: Hypothesis families are data, not prompts

**Choice.** The builder returns a JSON object: a name, a rationale, the fault
kinds and target selectors it concerns, parameter ranges, a timing relation to
the lifecycle, and what outcome would refute it. The campaign validates the
object against the schema registry; the arbiter then refuses it or lets it
through with a stated reason; and only then is it rendered into the worker's
prompt as a directive. Every schedule the worker then writes is still parsed and
compiled like any other.

**Rationale.** Free text from one model pasted into another's prompt is an
injection channel with extra steps. A typed object can be validated, counted,
de-duplicated and scored; "unique" becomes checkable (a family whose kinds,
targets and ranges match an existing one is refused as a duplicate).

**The first curriculum target is already measured:** 12 of the 17 registered
fault kinds have never been realized in any recorded world.

### D7: Planted defects are build variants with a pre-registration

**Choice.** A defect proposal is a unified diff confined to
`testdata/kvfixture/internal/**`, guarded by a new build tag, plus a
pre-registration: which fault, which oracle, which outcome, and why. On
approval the campaign adds the variant to a committed registry
(`testdata/kvfixture/variants.yaml`), from which the Dockerfile's closed
`KV_VARIANT` case and the compose image tag are generated: the property that
a patched build can never be labelled `:buggy` is kept by construction.

Before a variant is attacked it must pass two controls: it builds, and with no
faults injected the `smoke` profile passes. A variant that fails its own
no-fault control is recorded UNEVALUABLE and never counted as detected.

**Outcome semantics.** DETECTED: a world violated on the pre-registered oracle
(or another; recorded as "detected, mechanism differs", the etcd lesson).
NOT DETECTED after the variant's budget: a capability gap, drafted as an OQ
entry with run ids. The pre-registration is written and hashed before the
first world runs, exactly as `targets/etcd/.prothesis/PREREGISTRATION.md` was.

**Rejected.** Runtime fault flags inside the fixture binary (the fixture's own
README explains why the lease constant is compile-time: a defect that
configuration can turn off is a defect a gate can be talked out of). Letting
variants touch the harness module. Auto-approval for "small" diffs.

### D8: High-risk items stop for a human, who sees the whole diagnosis

**Choice.** `thesis review`: `list`, `show ID`, `approve ID --reason`,
`reject ID --reason`. A pending item is a **review packet** on disk. An
approval is one line in the append-only `.prothesis/reviews.jsonl`: packet id,
the packet's SHA-256, the decision, the reason, the reviewer's git identity,
the time. The campaign proceeds only if an approval line's hash matches the
packet it is about to act on; edit the packet and the approval no longer
applies. Rejections are recorded with the same weight and are fed to the meta
as structured feedback.

**The review packet.** Rendered as Markdown by `thesis review show`:

1. **What is proposed**, in one sentence, and by which role, provider and
   model identifier (as configured, verbatim).
2. **Why**: the evidence that motivated it, with run ids, counts and the
   library lines it rests on. No claim without a pointer.
3. **The exact artifact**: the diff, the config delta or the budget change,
   with its SHA-256. What is approved is these bytes.
4. **Pre-registration** (defects): fault, oracle, expected outcome, mechanism.
5. **Blast radius**: files touched, containers and images built, network
   destinations contacted, data that leaves the machine, disk and spend.
6. **What could go wrong**, specifically, and how it would be noticed.
7. **Reversibility**: how to undo it, and what cannot be undone.
8. **How to verify afterwards**: the command and the expected result.
9. **Static findings**: for a diff: paths outside the allowed tree, new
   imports, `os/exec`, `net`, `unsafe`, file or environment access, `init`
   functions, build-tag correctness. Each listed, none summarized away.
10. **Expiry**: a packet not acted on within its window lapses.

**Who writes the packet.** Items 1, 3, 4 and 9 are produced mechanically by
the campaign: the bytes, their hash, the static findings. Items 2 and 5–8 are
the arbiter's diagnosis of the builder's proposal, written by a different
model from the one that made it, and marked as the arbiter's. The arbiter may
reject a proposal before it ever becomes a packet, with its reason recorded
and fed back to the builder; it cannot approve one. A packet the arbiter
passed still stops for the human, and says so in its first line.

**Rationale.** "Human in the loop" fails in practice when the human is shown a
summary and a button. The packet is built so that approving it without reading
it is at least approving a specific hash with a stated reason in a permanent
record.

### D9: The budget comes from the environment, and a model may not lower a floor

**Choice.** `thesis doctor` (declared by the directive, unimplemented today)
becomes the environment probe: free disk, Docker's disk cap and current use,
free bridge address pools, CPU and memory, whether a local model endpoint
answers and how fast, measured world time and measured bytes per world. It
emits JSON. The budget is then one of:

- **user-specified**: `campaign.environment` in `prothesis.yaml`;
- **derived**: the harness computes a safe envelope from the doctor report
  with fixed formulas (never below a disk floor, never above the Docker cap
  less a margin, prune before the cache reaches a threshold);
- **model-proposed**: the arbiter may propose a budget from the doctor report.
  It is clamped to the derived envelope. A proposal that would exceed the
  envelope is a high-risk item and becomes a review packet.

**Rationale.** The user asked for the budget to follow the deployment
environment, stated up front or evaluated by the model. A model evaluating its
own resource limits is fine as a proposer and unacceptable as the authority;
the harness measures, the formulas are in code with tests, and only a human
raises a ceiling.

### D10: "Improving" is four numbers, measured per campaign

Defects detected over defects planted, by class. Fault kinds ever realized
(today 5 of 17). Oracles that have ever fired. Worlds to first violation per
variant. Reported by `thesis campaign report` from the library files alone, so
the claim "the system got better" is a diff between two reports, not prose.

### D11: Lock coverage grows; existing locks stay valid

`models` and `campaign` join `lock.CoveredPaths`. Because the lock digests the
raw YAML and an unwritten key produces no entry, no existing lock moves until
a project writes one of those blocks, and when it does, switching an endpoint
from localhost to a cloud host moves the digest and needs a recorded reason.
Egress can never be enabled silently.

### D12: Model selection follows the task

**Choice.** Three roles, three tiers, by the author's ruling (D-076, ruling 7):

| Role | Task | Tier |
|---|---|---|
| worker | volume: one schedule per world, thousands of times | whatever is cheap and close; a local model by default |
| builder | construction: hypothesis families, planted-defect diffs | the second most capable model available |
| arbiter | judgment: refusing, diagnosing, adjudicating, budgeting | the most capable coding model available |

The harness cannot measure capability, so the tier is the user's declaration
and the identifiers are the user's strings. What the harness does enforce is
**separation**: the builder and the arbiter may not resolve to the same
provider, endpoint and model. A configuration where they do is refused with
exit 5 unless the lock-covered key `models.arbiter_is_builder: true` is set,
and then every review packet opens by saying the proposal was judged by the
model that wrote it.

**Rationale.** The repository's own history is the argument. Every defect in
its ledgers that mattered was found by a session other than the one that wrote
the code: the fictional shrink, the comments that had stopped being true, the
observer's unsound cross-check, the promotional README, the lock claim in
D-075's first draft. Putting the strongest model on judgment rather than on
construction spends capability where a miss is most expensive (a bad proposal
costs a rejection, a bad judgment costs a false PASS) and it makes the
builder's output something a stronger reader has already tried to break before
it reaches a person.

**Rejected.** One model for everything (no independent reader). The strongest
model as builder with a weaker reviewer (the reviewer becomes a formality).
Letting the arbiter approve low-risk items to save the human's time (the
author's ruling is that every high-risk item gets a human; the arbiter's job
is to make that read shorter and better informed, not to replace it).

## The high-risk register: full diagnosis

GATED means a review packet and an approval record. STRUCTURAL means prevented
by construction and verified by test, with any breach ending the campaign.

| # | Item | Class |
|---|---|---|
| R1 | Model-written code compiled into the fixture | GATED, always |
| R2 | Prompts and results leaving the machine for a cloud provider | GATED on enable and on any endpoint change |
| R3 | A model weakening a gate | STRUCTURAL → exit 4 |
| R4 | Prompt injection through the system under test's own output | STRUCTURAL + GATED downstream |
| R5 | Flaky worlds entering the regression corpus | RULE (k-of-n floor); below-floor entry GATED |
| R6 | Disk, cache, network-pool and volume exhaustion | ENVELOPE; raising it GATED |
| R7 | Runaway cloud spend | ENVELOPE; raising it GATED |
| R8 | Residual faults poisoning later worlds | STRUCTURAL → stop |
| R9 | Evidence dilution across non-comparable strata | RULE |
| R10 | Verdicts from unstamped or dirty binaries | STRUCTURAL → refuse to start |
| R11 | Two processes, or two agents, in one project directory | STRUCTURAL → refuse to start |
| R12 | A model drafting ledger entries | GATED |

**R1: Model-written code in the fixture.** *Mechanism:* the builder returns a
diff; the arbiter diagnoses it; on the human's approval it is compiled inside
the fixture's image build and run in three containers. *What goes wrong:* the diff reaches outside
`internal/**` and alters the driver, the history writer or the health probe,
so the "defect" is really a change to what the harness observes; it adds an
import that opens a socket or reads the environment; it plants a bug that is
live even with no fault, so every world fails and "detected" means nothing;
its build tag is wrong, so the defect ships in `:buggy` and silently changes
the reference result every committed verdict rests on. *Containment:* the
diff is applied only under a new tag; path, import and tag checks are static
findings in the packet; the no-fault control must pass; the fixture containers
have no NET_ADMIN and no published peer plane; the compile happens in the
image build, not on the host; `buggy` and `kvfixed` are rebuilt and their CI
verdicts re-asserted after any variant lands. *Residual:* a subtle defect that
interacts with the planted lease bug. Variants are therefore built on
`kvfixed`, not on `buggy`, so exactly one defect is live.

**R2: Cloud egress.** *What leaves:* topology (node ids, roles), the fault
grammar, the policy, outcome summaries, and for the meta layer campaign
statistics and library lines. *What must never leave:* raw `history.jsonl`,
node logs, environment variables, file paths outside the project, API keys.
*Containment:* prompts are built from typed summaries, not from files; the
audit trail records every byte sent, so "what did we send" has an answer;
keys by variable name only; `models` is lock-covered (D11). *Gate:* enabling a
non-loopback endpoint for either role, and any later change to it, produces a
packet listing the destination host, the fields that will be sent, and a
sample request. *Residual:* the provider's own retention policy, which is the
user's contract with the provider and is named in the packet as unverifiable
from here.

**R3: A model weakening a gate.** *Mechanism:* there is none by design; no
role has a write path to a protected file. *What goes wrong anyway:* a bug in
the campaign, or a `command` provider (which is a program on the host, run as
the user) writing where it should not. *Containment:* the protected-path diff
and both lock verifications at every meta step and at campaign end; breach is
exit 4 and the campaign does not resume without a human. The `command`
provider is code the user chose to run and is called out as such in R2's
packet. *Residual:* OQ-057 is unchanged; a swapped oracle executable is still
a warning, not exit 4. A campaign should run with the strict mode the
decision matrix recommends once it exists.

**R4: Injection through the SUT.** *Mechanism:* a system under test controls
its own logs and response bodies. If any of that text reaches a prompt, the
SUT is talking to the model. *What goes wrong:* a log line reading like an
instruction steers the worker toward harmless schedules, or the meta toward a
"defect" with a payload. *Containment:* no raw SUT text in any prompt; only
harness-computed values (verdict class, oracle name, counts, timings, the
witness's operation ids and integers). Strings that originate in the SUT are
never interpolated. Everything downstream of the meta is still validated or
gated, so a successful injection buys a refused schedule or a packet a human
reads. *Test:* a fixture whose logs contain an instruction; the prompt bytes
are asserted not to contain it.

**R5: Flaky corpus entries.** *Mechanism:* OQ-054; the fixture's own defect
reproduces about 7 times in 9. A k/k gate refuses it; a loose gate admits
worlds that make `thesis regress` green two runs in three. *Containment:* the
k-of-n rule with the measured rate stored in the entry and a floor below which
entry is refused; `regress` judges each entry against its own recorded rate.
An entry below the floor is never added automatically; if the evidence is
worth keeping it becomes a packet. *Depends on:* the k-of-n decision, which is
therefore on the critical path.

**R6: Resource exhaustion.** *Measured:* +5.4 GB of build cache per world
with a changed context, against a 50 GB Docker cap (OQ-068); about 2 MB of
history per world, so roughly 5 GB a day at thirty seconds a world; Docker
bridge address pools exhausted after about two dozen concurrent networks;
285 stale volumes left by one phase of search runs (OQ-043); a full C: drive
on this host once corrupted six tracked files mid-write. *Containment:* the
OQ-068 fix is a precondition; the envelope of D9; prune and retention sweeps
inside the loop; a hard disk floor below which the campaign stops with exit 3
rather than writing a truncated artifact. *A local model shares the machine:*
the doctor report measures world time with the model loaded, not idle.

**R7: Cloud spend.** *Containment:* per-campaign ceilings on calls and on
tokens per role, counted from provider responses, enforced before each call;
exceeding one is exit 3. Raising a ceiling is a packet that states the spend
so far and the projected spend.

**R8: Residual faults.** *Measured:* a `proc.kill` paired with a durative
network fault leaves an unwithdrawable world, 13 of 40 in one run (OQ-042).
Over thousands of worlds a leaked rule poisons everything after it and every
later verdict is about the leak. *Containment:* the pairing is refused at plan
time until it is fixed; the residue baseline check (adversarial probe A3's
logic) runs every M worlds; any residue stops the campaign and marks every
world since the last clean check UNEVALUABLE in the ledger.

**R9: Evidence dilution.** *Mechanism:* an llm-proposed stream is not
seed-reproducible (OQ-069); pre- and post-D-071 worlds are different
populations; variants are different systems. *Rule:* the unit of evidence is
the recorded world; every statistic is stratified by variant, strategy and
lifecycle version; no report pools across strata, and a report that would is
refused rather than footnoted.

**R10: Unstamped binaries.** Refuse to start; print the `build.ps1` command.

**R11: Concurrent actors.** *Measured:* two agent sessions collided in this
tree on 2026-09-17 and one deleted a stamp field the other's build script was
writing. *Containment:* the campaign's lock file with PID and liveness check;
a dirty protected path at start is a refusal, not a warning.

**R12: Model-drafted ledger entries.** The ledgers are the project's memory
and every entry is held to "never assert what you did not measure". A gap the
campaign finds is drafted with its run ids into a packet; it enters
`OPEN_QUESTIONS.md` only when approved, and the entry says who drafted it.

## Stages and acceptance

Each stage ships failing-first tests proven by mutation, a ledger entry, and,
where it makes a claim about behaviour, a measurement with its command.

| Stage | Delivers | Accepted when |
|---|---|---|
| S0 | Preconditions: the D-075 commit with its seven corrections; the README's fault-kind count corrected (17, not 10); the OQ-068 fix; retention floors; the k-of-n gate | each has its own entry and measurement |
| S1 | `models:` with the three transports and the three roles; `search.llm` as shorthand for the worker | one scripted-server test per transport; a hung `command` provider is killed at its timeout; the key never appears in any artifact; a builder and arbiter that resolve to the same model are refused with exit 5 |
| S2 | `thesis doctor` and the derived envelope | the report is JSON with every value measured; the formulas have table tests; a model proposal above the envelope is clamped and produces a packet |
| S3 | `thesis campaign` and `campaign.jsonl` | resumes after a kill; refuses an unstamped binary; stops on exit 4; a fake provider that writes a protected path ends the campaign with 4 |
| S4 | `thesis review`, the arbiter's diagnosis and the approval record | an edited packet invalidates its approval; nothing GATED proceeds without a matching line; the arbiter can reject and has no code path to approve; a rejection, the arbiter's or the human's, reaches the builder as structured feedback |
| S5 | Meta hypothesis families | a duplicate family is refused; the first live campaign realizes at least one of the twelve never-realized kinds, recorded with its residue check |
| S6 | Variants, the registry, meta-proposed defects | the no-fault control gates every variant; one approved defect is run to a recorded DETECTED or NOT DETECTED with its pre-registration hashed first |
| S7 | `thesis campaign report` | two reports diff to the four numbers of D10 and nothing else |

## What this does not do

It does not make the harness deterministic. It does not let any model change
what a verdict means. It does not claim the worker is better than the ladder:
that is a measurement S5 can make and until it does the README lists the llm
strategy under "not yet proven". It does not test systems other than the
fixture with planted defects: a variant of etcd is a fork of etcd, and out of
scope.

## Rulings

Made by the author, 2026-09-20: (1) the meta layer produces both hypothesis
families and planted defects; (2) the system is compatible with any model,
cloud or local, with Qwen as the local default on the build host; (3) every
high-risk item requires human feedback and is presented with its full
diagnosis; (4) the loop is a `thesis campaign` verb, not a script; (5) the
budget follows the deployment environment, specified up front or evaluated by
the model; (6) the order of work is S0 as listed, then S1 onward; (7) model
selection follows the task; building by the second most capable model, the
arbiter the most capable coding model (D12). All seven are recorded in D-076.

Still the author's to make, when reached: the disk floor and margin constants
of D9; the k-of-n floor of R5; the packet expiry window of D8; whether a
campaign may run against a target other than the fixture before S6 is done.
