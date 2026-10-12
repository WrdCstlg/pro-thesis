# SPEC: 2026-10-12 H1 Put the Fault Schedule Under the Lock

> **Adversarial Persona Loop Record & Technical Specification**
> Item H1 of `docs/design/10-hardening.plan.md` | D-097 Ruling 1 | OQ-085

---

## 1. Adversarial Persona Loop (Cycle 1)

### BUILDER
**Senan.** Building PRO-THESIS, a fault-injection harness whose exit code is its product.
**Invariant:** Exit 0 is emitted only for a full-size, locked, judged run in which the load
overlapped the injected faults the gate specifies.
**Proposal:** Move the fault schedule into the lock-covered run profile (`profiles.<name>.faults`).
`thesis run --profile X` injects it by default. An argv schedule that differs from the profile,
or an explicit `--no-faults` flag, marks the budget narrowed (D-059), so the run cannot report PASS.

### USER
**Staff Engineer on Storage Team.**
"I am responsible for a Raft-backed cluster. We just had a customer hit a lost write after a partition.
When our CI gate reports green on our release branch, I need to know the network partition actually
occurred while the load was running against the cluster.
If the harness lets a test pass with zero faults injected because somebody left off `--fault` in a script
or swapped the profile, our release ships 'fixed', the customer loses data again, and our chaos gate
is completely discredited. I do not care about your lock hashes; I care that the partition was fired."

### VETO
**Platform Security Architect & Compliance Auditor (SOC 2 CC8.1, NIST PW.8).**
"Authority: SOC 2 CC8.1 (change authorization and segregation of duties); NIST PW.8 (executable verification).
The lock exists precisely to enforce segregation of duties: an engineer submitting code cannot silently
lower the bar of the verification gate without a reviewable change to the lock digest.
As an attacker aiming to produce a false PASS (per Appendix B):
1. **Attack B1 bypass:** If I cannot omit `--fault` on CLI, what if I define an empty `faults: []` in a profile?
   If a profile with no faults can report PASS, an attacker simply defines or selects a profile with no faults.
2. **Attack B7 bypass:** What if I edit `prothesis.yaml` to change or remove faults without re-locking?
   `profiles` is in `CoveredPaths`. If `profiles.<name>.faults` is not digested into the lock, drift will not fire!
3. **CLI tampering:** If I pass `--no-faults` or a different fault on argv, it must be impossible for that run
   to emit exit 0.
4. **Lock synchronization:** Both committed locks (`.prothesis/lock` and `testdata/kvfixture/.prothesis/lock`)
   must be updated. If they are not re-locked with the exact key and reason, CI and verification break with exit 4.
I will veto any implementation where an argv modification of faults can exit 0, or where an empty schedule on a
faulted profile passes."

### ENGINEER
**Senan.** Owns `internal/control`, `pkg/schema`, `cmd/thesis/run.go`, and `.prothesis/lock`.
"Reading the loop:
1. **BUILDER's invariant** holds: the fault schedule must be under the lock, and modifying it from argv narrows the run.
2. **USER's demand** holds: running a profile without CLI flags must inject the locked schedule faithfully.
3. **VETO's attack vectors** are closed:
   - `profiles.<name>.faults` is decoded and validated at config load (`schema.ParseFault`).
   - `internal/lock` already digests the entire `profiles` mapping; adding `faults` automatically extends the lock coverage.
   - CLI adds `--no-faults`. If passed, faults are cleared and `Budget.Narrowed = true`.
   - If CLI `--fault` is passed: if it canonically matches `profile.Faults`, the run is un-narrowed. If it differs in any way
     (different kind, target, timing, or missing faults), `Budget.Narrowed = true` (D-059).
   - A narrowed clean run reports INCONCLUSIVE (exit 2) and `oracle_lock: bypassed`. Exit 0 is impossible.
   - Both committed locks (`.prothesis/lock` and `testdata/kvfixture/.prothesis/lock`) will be re-locked with the reason:
     `profiles.<name>.faults puts the gate's fault schedule under the lock (item H1, D-097 ruling 1)`.

---

## 2. Technical Specification

### 2.1 Schema (`pkg/schema/config.go`)
Extend `Profile`:
```go
type Profile struct {
    Budget        Duration `yaml:"budget"`
    Worlds        *int     `yaml:"worlds"`
    DriverProfile string   `yaml:"driver_profile"`
    Search        bool     `yaml:"search"`
    Faults        []string `yaml:"faults,omitempty"`
}
```
In `Config.Validate()`:
Each fault in `Profile.Faults` is validated via `schema.ParseFault`.

### 2.2 CLI & Narrowing (`cmd/thesis/run.go`)
- Support `--no-faults`.
- When `--fault` is not provided:
  - If `--no-faults` is set: `faults = nil`, `narrowed = (len(profCfg.Faults) > 0)`.
  - If `--no-faults` is NOT set: `faults = profCfg.Faults`.
- When `--fault` IS provided:
  - Compare canonical strings of argv `faults` against `profCfg.Faults`.
  - If slices are not equal: mark `narrowed = true`.
- Pass `faults` and `narrowed` into runner options / budget.

### 2.3 Acceptance Tests
1. `TestRunInjectsTheProfileFaultSchedule`: Running with no `--fault` injects the profile's schedule.
2. `TestAnArgvScheduleEqualToTheProfileIsNotNarrowed`: Passing `--fault` identical to profile is not narrowed.

### 2.4 Refusal Tests
1. `TestAnArgvScheduleThatDiffersFromTheProfileCannotPass`: Passing a different `--fault` marks narrowed, exit 2.
2. `TestNoFaultsOnAProfileWithFaultsCannotPass`: Passing `--no-faults` on a profile with faults marks narrowed, exit 2.
3. `TestEditingTheProfileScheduleWithoutRelockIsDrift`: Changing `profiles.<name>.faults` without re-locking produces ORACLE_DRIFT (exit 4).

### 2.5 State Carryforward
- **Built:** H1 profile fault schedule locking and CLI narrowing.
- **Invariants Preserved:** D-059 (narrowed runs cannot pass); I6 (lock covers profile configuration).
- **Invariants Introduced:** A run profile's fault schedule is normative and locked; argv changes are narrow-only.
- **Next Stated Priority:** Phase 1 item H3 (detect history truncation by declaration).