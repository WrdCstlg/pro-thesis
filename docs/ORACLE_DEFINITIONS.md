# `prothesis.oracle_def/v1`: the external oracle definition file

Reference for the files in `oracles.dir` (`.prothesis/oracles` by default).

**Status: ADDITIVE.** The directive freezes `prothesis.yaml`, `prothesis.verdict/v1`,
`prothesis.oracle_input/v1`, `prothesis.oracle_output/v1`, the history JSONL, the exit codes and the
fault grammar. It defines no definition-file format, so this one is designed here. Rationale in
`DECISIONS.md` D-039; the gap it fills, and the alternative that was rejected, in
`OPEN_QUESTIONS.md` OQ-029. It resolves OQ-028.

The same reference is scaffolded into each project as `.prothesis/oracles/README.md`, where the
person editing a definition will actually see it. This copy exists for the repo.

---

## The file

One oracle per file. Files ending in `.yaml` or `.yml` are definitions; everything else in the
directory is ignored and reported.

```yaml
version: prothesis.oracle_def/v1
name: linearizable.kv
class: consistency
valid_phases: [ASSERT]
cmd: "./bin/linearizable-kv"
timeout: 120s
```

| key | required | meaning |
|---|---|---|
| `version` | yes | always `prothesis.oracle_def/v1` |
| `name` | yes | the oracle's identity: `violations[].oracle` in the verdict, and what `.prothesis/lock` addresses. Unique across the directory, and it may not collide with a built-in |
| `class` | yes | one of the eight `schema.OracleClass` values: `crash`, `consistency`, `liveness`, `convergence`, `resource`, `safety`, `differential`, `metamorphic` |
| `valid_phases` | yes | invariant I5: the lifecycle phases in which **evaluating** the oracle is meaningful. Non-empty |
| `cmd` | yes | the executable and its arguments, split quote-aware exactly as `driver.cmd` is. A relative program resolves against the directory `prothesis.yaml` lives in, which is also the working directory. No placeholders |
| `timeout` | yes | wall-clock bound on one evaluation (`120s`, `2m`). Positive, at most 1h |

### Why each of those is strict

* **Unknown keys are rejected.** `valid_phase` for `valid_phases` would otherwise take its zero
  value, leaving the oracle valid in no phase: an oracle that can never fire, sitting in the
  directory looking like coverage.
* **`version` is required**, because `KnownFields` catches a key that was *added* in a later format
  but not one that was *removed*: a v1 reader would decode the missing field to its zero value.
* **There is no `enabled:` flag.** Turning an oracle off without deleting it is a gate-weakening
  move that reads as one character in a diff. Delete the file instead; that is unmissable.
* **`cmd` may not contain `{history_path}`, `{seed}`, `{profile}` or `{plan_path}`.** Nothing is
  substituted into an oracle's command line (everything arrives on stdin) so the oracle would be
  handed that text literally and would open a file called `{history_path}`.
* **A definition that does not parse fails the run.** Skipping it would mean an oracle silently
  vanished and the gate went green over a property nobody checked.

### Discovery order

Definitions register **sorted by oracle name**, after the built-ins, which register in the
directive's canonical order. Sorting by name rather than by filename means renaming a file cannot
renumber a verdict's `v1`, `v2`, … violations.

---

## The executable contract (directive 4.5)

An oracle is a separate process, and may be written in any language with any dependencies: that is
the reason the contract exists, and it is what keeps `thesis` itself a single static binary with one
third-party dependency (D-002, D-007).

* **stdin**: `prothesis.oracle_input/v1`: `history_path`, `final_state_path`, `telemetry_path`,
  `world_path`, and the **measured** `phases` in milliseconds relative to DRIVE start.
* **stdout**: `prothesis.oracle_output/v1`. Echoing `oracle`, `class` and `valid_phases` is
  optional; if present they must match the definition.
* **exit code**: `0` ok, `1` violated, `2` inconclusive.

### What makes a finding believable

1. **The exit code and the reported status must AGREE.** They are two independent channels. A
   disagreement is a defect in the oracle, so *neither* reading is adopted: the finding is
   `inconclusive` and names what each channel said. This is stricter than
   `schema.DecodeOracleOutput`'s "take the worse of the two": see D-038 for why (taking the worse
   would manufacture a `violated` out of an oracle bug, and a false positive is the ranked #1 way
   this phase fails).
2. **Nothing that is not a clean answer becomes `ok`.** All of these are `inconclusive`:
   * the executable could not be started
   * it crashed, or exited with a code outside `0/1/2`
   * it hit its timeout (its whole **process tree** is killed)
   * the run was cancelled
   * stdout was empty, unparseable, carried the wrong schema id, or carried no status
   * stdout exceeded 1 MiB
   * its own `oracle` / `class` / `valid_phases` contradicted the definition
3. **Report `inconclusive` when you could not check.** A checker handed an empty history has checked
   nothing; one that exhausted its budget has checked part of something. Reporting `ok` there is the
   single most damaging thing an oracle can do, because the gate goes green *precisely because*
   nothing happened.
4. **Only report `violated` with a witness.** A false positive destroys the value of every other
   verdict the tool emits.

### Phase validity (I5)

The **engine** filters, not the oracle. An oracle is never asked to evaluate outside its declared
`valid_phases`; asking one anyway is an engine bug and fails loudly with a `*PhaseError`.

`valid_phases` (this file) and `violations[].phase` (the verdict) are **different fields with
different meanings**, and the frozen examples invite conflating them (OQ-004). Nothing checks one
against the other. A consistency oracle valid only in `ASSERT` routinely reports evidence lying in
`DRIVE`: that is exactly what the fixture's stale read is (D-031), and directive 4.6's own example
carries `"phase": "DRIVE"` for that shape.

What *is* checked is the oracle's own **declaration**: if the output's `valid_phases` is present and
differs from this file's, the finding is `inconclusive` whatever its status. An executable that
disagrees with the declaration it was registered under is either the wrong binary or a drifted one,
so neither its violations nor its passes can be believed.

### Placing a finding on the timeline

`prothesis.oracle_output/v1` carries no timestamp, so two **optional** witness members are read
(additive, OQ-030):

```json
"witness": {
  "op_ids": [90002, 90117],
  "key": "k/42",
  "first_seen_ms": 11084,
  "phase": "DRIVE"
}
```

* `first_seen_ms`: milliseconds relative to DRIVE start; the engine derives the observed phase from
  it against the measured windows.
* `phase`: pins the observed phase directly, for evidence with no usable timestamp.

Omit both and the finding is pinned to the phase it was evaluated in, which is honest. It is *not*
placed at DRIVE start by a zero the oracle never supplied: a fabricated position on the timeline is
worse than none, because it is the field a human reads first.

### stderr

Captured into the run bundle at `<run>/world-NNNN/oracles/<name>.stderr.log`, excerpted into the
explanation whenever the finding is not `ok`, and named in the witness as `stderr_path`. Write
diagnostics freely; stdout is the contract channel and must carry only the document.

The capture is bounded at 4 MiB and records its own truncation. A capture that cannot be written
never changes a finding: it makes it harder to debug, not wrong.

---

## The lock

Every file in `oracles.dir` is content-hashed into `.prothesis/lock`. Adding, changing or removing
one moves the digest, and a run whose digest does not match exits **4** (`ORACLE_DRIFT`): the one
code an agent loop must never resolve for itself. Re-baseline deliberately, in its own reviewed
commit:

    thesis oracles lock --reason "add the linearizable.kv consistency oracle"

**The honest limitation:** hashing a definition does not hash the executable it names. A definition
pointing at `./bin/linearizable-kv` is covered; the binary is not. That is the verdict-signing
concern (v3), and it is stated here rather than implied away.
