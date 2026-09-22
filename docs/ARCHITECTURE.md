# Architecture

Generated from the code by reading it, not from memory of the design docs. Any edge found wrong
is a bug in this document and must be fixed in the same change that finds it. Cross-references:
the refusal census is OQ-072, the attribution layer is D-082, the retention gap is OQ-070.

## 1. Command and package wiring

```mermaid
flowchart TD
    subgraph CLI["cmd/thesis (one binary, hand-rolled dispatch)"]
        V["verbs: init, up, down, run, search, replay, regress, bisect, shrink, oracles, diagnose, cluster, version"]
    end
    subgraph Core["internal/"]
        CTRL["control: runner, world lifecycle, verdicts"]
        SEARCH["search/engine: strategies, guided search"]
        REC["recorder: bundles, MANIFEST, canonical artifacts"]
        HAR["harness: Docker CLI, compose projects"]
        ORA["oracle: engine + built-in invariants"]
        DIAG["diagnose: known-problem attribution"]
        CLU["cluster: two-layer clustering of the unattributed pool"]
        LOCK["lock: oracle tamper manifest"]
        CORP["corpus + shrink: regression worlds"]
    end
    SCHEMA["pkg/schema: config, verdicts, world files, retain, knownproblems"]
    CHECKER["cmd/thesis-oracle-linearizable (external checker binary, fingerprinted by the lock)"]

    V --> CTRL
    V --> SEARCH
    V --> DIAG
    CTRL --> REC
    CTRL --> HAR
    CTRL --> ORA
    SEARCH --> CTRL
    ORA --> CHECKER
    V --> LOCK
    V --> CLU
    CLU --> DIAG
    CTRL --> CORP
    CTRL --> SCHEMA
    DIAG --> SCHEMA
    LOCK --> SCHEMA
```

`pkg/schema/retain.go` (`RetainPolicy.Keeps`) sits inside `SCHEMA` and has **zero callers**: the
retention policy is parsed, validated and defaulted, and enforced by nothing (OQ-070).

`thesis cluster` (extract, discover, taxonomy) reads the corpus only through `diagnose.Scan`, an
additive read-only twin of the diagnose walk, and writes derived reports
(`cluster_features.csv`, `cluster_manifest.json`, `cluster_report.yaml`, `taxonomy_report.yaml`,
all gitignored) under `.prothesis/`. It never writes to the registry and never promotes a
candidate; discover exits 2 when any CONTESTED outcome exists.

## 2. World lifecycle and the six ways a verdict becomes INCONCLUSIVE

```mermaid
flowchart LR
    BOOT --> STEADY_STATE --> DRIVE --> HEAL --> QUIESCE --> ASSERT
    PERTURB -.->|nests inside DRIVE since D-071| DRIVE
    ASSERT --> VJ["verdict.json per run"]
    ASSERT --> RJ["result.json per world"]

    INC1["1. oracle engine error, unrecognised status, or per-oracle refusal"]
    INC2["2. harness error: world dies before ASSERT, no result.json (OQ-071)"]
    INC3["3. cancellation before or during the world"]
    INC4["4. narrowed budget: a PASS is downgraded (D-059)"]
    INC5["5. search completes zero worlds"]
    INC6["6. fail-closed default: undeclared outcome"]
    RJ -.-> INC1
    BOOT -.-> INC2
    DRIVE -.-> INC3
    VJ -.-> INC4
    VJ -.-> INC5
    VJ -.-> INC6
```

Every one of the six is a refusal to judge, and since D-082 every recorded instance must
attribute to a known-problem entry or the diagnostic goes red.

## 3. Evidence and attribution dataflow

```mermaid
flowchart TD
    subgraph Bundle["run bundle (.prothesis/runs/r_YYYY_MM_DD_xxxx/)"]
        VJF["verdict.json"]
        SJF["search.json"]
        W["world-NNNN/: history.jsonl, telemetry, result.json"]
        MF["MANIFEST.json (only via recorder.OpenBundle / Bundle.Close)"]
    end
    REG[".prothesis/known-problems.yaml (committed registry, KP-001..KP-014)"]
    DIAG2["thesis diagnose (read-only)"]
    OK["exit 0: everything attributed"]
    RED["exit 2: UNATTRIBUTED items printed with evidence"]

    VJF --> DIAG2
    W --> DIAG2
    REG --> DIAG2
    DIAG2 --> OK
    DIAG2 --> RED
    RED -.->|investigate, catalogue, same change| REG
```

The loop at the bottom is the recursion: a new kind of refusal attributes to nothing, exits 2,
and is catalogued into the registry with its ledger citation in the same change. What is not yet
built: the census floor that makes corpus shrinkage loud in `internal/search/engine` (prerequisite
for the OQ-070 pruner), and the pruner itself, which when it lands must rename into
`.prothesis/runs/.trash/` and never unlink.
