# PHASE 3 BUILD BRIEF — external oracles, consistency checking, and the lock

Authoritative for Phase 3. Where a design doc disagrees with this, this wins.

Phase 3 deliverables (directive §6) and nothing else:
- external oracle process runner honouring the stdin/stdout JSON contract
- a linearizability checker for the KV register model
- phase-validity enforcement (invariant oracles throughout; convergence oracles only in `ASSERT`)
- `.prothesis/lock` tamper detection, `thesis oracles lock --reason "..."`, `thesis oracles verify`

Definition of done:
- **(a)** the fixture lease bug is flagged as a `linearizable.kv` consistency violation with
  concrete witness op ids
- **(b)** an unauthorized edit to an oracle or a budget exits **4** (`ORACLE_DRIFT`)

---

## 0. The three ways this phase can fail, ranked

Phase 3 is the phase where a mistake is *worse than no feature*. Rank them and design against them:

1. **A false positive.** A checker that reports `violated` on a correct system destroys the
   product. The whole value proposition is "when it says FAIL, something is genuinely wrong".
   Every soundness decision below resolves toward "rather report inconclusive than risk a false
   positive".
2. **A silent vacuous pass.** A checker that exhausts its budget, or is handed an empty history,
   and answers `ok`. This class has already bitten this project twice — a health probe matching
   zero nodes, and a typo'd placeholder producing an empty history — and a consistency oracle
   reading an empty history is the worst instance yet, because the gate goes green precisely
   because nothing happened.
3. **A spurious lock failure.** Exit 4 is the one code the agent loop must NEVER auto-resolve.
   A lock that fires on a `thesis` upgrade halts every downstream project pending human sign-off.

---

## 1. The consistency checker

### D-A. Per-key decomposition is sound, and its precondition MUST be checked

Herlihy & Wing (1990), the **locality theorem**: a history is linearizable if and only if every
object's subhistory is linearizable. So checking each key independently is not an approximation —
it is exact — and it is what makes the problem tractable: a measured gate world has ~29,000
operations over 64 keys, which is hopeless globally and ~450 operations per key at very low
per-key concurrency (16 clients over 64 keys), which is trivial.

**The precondition is that every operation touches exactly one key.** The fixture's `txn` breaks
it: `doTxn(idx, opID, target, readKey, key, value)` reads one key and writes a *different*,
independently drawn key. Measured mixes:

| profile | mix | keys per op |
|---|---|---|
| `smoke` | read 0.5 / write 0.5 | single-key |
| `gate` | read 0.4 / write 0.4 / **txn 0.2** | **multi-key** |
| `soak` | read 0.3 / write 0.4 / **txn 0.2** / admin 0.1 | **multi-key** |

**Therefore:** the checker validates the precondition first. If any operation touches more than
one key, it returns **`inconclusive`**, naming the offending op id and saying that per-key
decomposition is unsound for this history and a transactional checker (Elle-style) is required.
It must NEVER partition a history it has not proven single-key — that would silently accept
histories admitting no serial order, which is a false PASS in the checker whose correctness the
product is sold on.

### D-B. `ok` / `fail` / `info` — the soundness rules

The directive fixes the meanings; the checker's job is to honour them exactly.

- **`ok`** — definitely happened, with that response. A normal completed operation.
- **`fail`** — definitely did NOT happen. **Drop it**, and drop its invoke. It constrains nothing.
- **`info`** — indeterminate: it may have taken effect, or may not.
  - an **`info` read** returns an unknown value, so it constrains nothing → **drop it**.
  - an **`info` write** may or may not have applied → the search must explore **both branches**.

The last rule is the one that matters. Treating an indeterminate write as *definitely applied*
constrains the model MORE than reality permits, and a more-constrained model rejecting a history
does not prove the real one does — that is a **false-positive generator**. Treating it as
definitely *not* applied is equally wrong in the other direction. It must branch.

An operation invoked with no completion record at all (the driver was killed mid-flight) is
treated exactly as `info`.

### D-C. Exhaustion is `inconclusive`, never `ok`

Linearizability checking is NP-complete in general, and the branching from `info` writes is
exactly what makes it blow up — which is unfortunate, because faults are what *produce* `info`
records, so the histories most worth checking are the hardest ones.

The checker takes an explicit budget (wall clock and explored-state count). On exhaustion it
returns `inconclusive` and reports how far it got. It must never report `ok` for a key it did not
finish, and must never report `violated` on a partial search unless the violation is *witnessed* —
a concrete linearization failure, not a timeout.

**An empty history is `inconclusive`, not `ok`.** A checker handed nothing has checked nothing.

### D-D. Algorithm: Wing & Gong with Lowe's optimisations

Per key, over a last-write-wins register:

- events sorted by `t_ns`; an operation is `(invoke_t, response_t, type, value)`
- depth-first search over which pending operation linearizes next, with the standard
  linked-list lift/unlift so backtracking is O(1)
- **memoise on `(model state, bitset of linearized ops)`** — this is what makes it fast in
  practice; without it the search is exponential on histories it should handle easily
- a read may linearize only if the register's current value equals what it returned
- a write always may, and sets the value

The witness on failure must be **actionable**: the op ids that could not be linearized, the key,
the value read, and the value the register necessarily held. The verdict's `witness` field is what
a coding agent reads, and `{"op_ids": [...], "key": "..."}` is the frozen shape.

### D-E. The checker is an EXTERNAL oracle

Per the directive, and for a reason beyond compliance: an external oracle is a separate process,
so it may carry its own dependencies without touching the `thesis` static binary. That keeps
D-002 intact while leaving the door open to wrapping Elle or Porcupine later for the
transactional case.

---

## 2. The external oracle runner

- discover oracles under `oracles.dir`
- write `prothesis.oracle_input/v1` to stdin, read `prothesis.oracle_output/v1` from stdout
- exit codes: `0` ok, `1` violated, `2` inconclusive
- **exit code and reported `status` must agree.** If they disagree, that is an oracle defect →
  treat as `inconclusive` and say so. Never trust one over the other silently.
- **a crashed, hung, or unparseable oracle is `inconclusive`, never `ok`.** Enforce a timeout;
  kill the process tree on expiry (the driver supervisor already solved this — reuse the
  approach, do not reinvent it).
- capture stderr into the run bundle: an oracle that fails needs to be debuggable.
- **phase validity (I5).** The engine, not the oracle, does the filtering. An oracle declares
  `valid_phases`; the engine refuses to *evaluate* it outside them. An oracle that reports a
  violation observed in a phase it did not declare valid is an oracle defect → `inconclusive`.

---

## 3. `.prothesis/lock`

### D-F. Digest the USER-SUPPLIED config, with defaults NOT filled in

If the digest covers the resolved config, then shipping a release that changes any compiled-in
default moves the digest on every downstream project at once, and exit 4 is the code the agent
loop must never auto-resolve. So: parse the user's `prothesis.yaml`, re-serialise it canonically
**without** applying defaults, and digest that.

### D-G. What the lock covers

- every file in `oracles.dir` (content hash, sorted by path)
- `perturber.allow` and `perturber.deny` — removing a fault kind is a lock change
- `perturber.budget` — narrowing `max_faults_per_world` or `max_concurrent_faults` is a lock change
- every profile's `budget` and `worlds` — shortening a budget is a lock change
- the top-level `search:` block — otherwise it is the easiest gate-weakening vector in the
  system, since `probe_budget_pct`, `max_mcts_depth` and every utility weight are reachable
  without touching an oracle (D-028 item 8)

**Honest limitation to document, not hide:** hashing an oracle *definition* does not hash the
executable it names. A definition pointing at `/usr/local/bin/checker` is covered; the binary is
not. That is the verdict-signing concern (v3), and the lock file must say so rather than implying
a guarantee it does not provide.

### D-H. Commands

- `thesis oracles list` — what is registered, with class and valid phases
- `thesis oracles verify` — recompute and compare; exit 4 on mismatch, naming exactly what moved
- `thesis oracles lock --reason "..."` — write/refresh the manifest, recording the reason. The
  reason is mandatory: the lock bump is meant to be a reviewable commit, and a bump with no stated
  reason is indistinguishable from an agent quietly re-baselining a gate it could not pass.

### D-I. Enforcement point

Phase 3's DoD names `thesis gate`, which is a **Phase 6** deliverable (this was logged as a spec
ordering conflict). Resolution: the lock check lives in a function `run` calls, and `gate` wraps
the same function in Phase 6. `thesis run` exits 4 on drift, which satisfies the DoD's intent
without implementing Phase 6 early.

---

## 4. Getting the DoD's history

The fixture's `gate` driver profile is 20% multi-key txns, so it is NOT soundly checkable by a
per-key checker — by design, per D-A. `smoke` is single-key but only 500 ops and finishes in
~700 ms, long before any fault window.

Add a driver profile that is **single-key and long enough to span the fault window**: same
read/write mix as smoke, ~20,000 ops, few keys (few keys maximises conflict density and therefore
checker power). It must be added to BOTH the fixture's `prothesis.yaml` and loadgen's built-in
table, because `{profile}` passes only a name and loadgen resolves the parameters itself (OQ-012).

The schedule that reproduces the anomaly is D-031's, not the directive's:

```
net.partition(role:leader)@8200..13500
proc.pause(role:leader)@8300..11000
```

---

## 5. Rules

1. Never weaken an assertion to make something pass. A wrong requirement is an
   `OPEN_QUESTIONS.md` entry and stops there.
2. `go build ./...`, `go vet ./...`, `gofmt -l .` clean; tests able to fail.
3. Do not invent normative field names. Additive fields marked `// ADDITIVE` plus an OQ entry.
4. Phase 3 only. No coverage, no search, no shrinking, no MCP.
