# The protocol the agent was run under

These are the documents an AI coding agent was given before it wrote the code
in this repository. They are kept verbatim, because the process is part of what
the repository demonstrates, and because a reader who wants to check whether a
rule in the ledgers was actually a rule can find it here.

| File | What it is |
|---|---|
| [`SABOTEUR_INJECTION.md`](SABOTEUR_INJECTION.md) | Addendum A: the guided-search engine spec that superseded the base directive's Phase 4. Its central claim was later measured and found untested (OQ-060). |
| [`PHASE0_BUILD_BRIEF.md`](PHASE0_BUILD_BRIEF.md) | The Phase 0 brief: the world file format, the canonical JSON profile, the clock frame. |
| [`PHASE3_BUILD_BRIEF.md`](PHASE3_BUILD_BRIEF.md) | The Phase 3 brief: the external linearizability checker and the single-key soundness rule (D-A). |

**Two documents are deliberately not published here:** the base build directive
that every ledger entry calls "the directive", and the prompt that asked the
agent to close Phase 4 with evidence. Both were removed from this repository
and from its history on 2026-09-15 at the author's decision (D-069). Entries
written before that date still cite them by their old filenames, and the design
documents under [`docs/design/`](../design/) are snapshots that do the same,
kept as written apart from punctuation; nothing in the tree reproduces their
text.

The rules the directive imposed on the agent are the ones the ledgers keep
citing, and they are stated here so a reader can hold the work to them: never
weaken a gate to make it pass; record every non-obvious choice in
`DECISIONS.md`; log every requirement that cannot be met as written in
`OPEN_QUESTIONS.md` rather than quietly relaxing it; ship a compiling binary at
the end of every phase; never claim a number that was not measured. The
exit-code contract, the schema identifiers and the phase lifecycle the directive
fixed are documented in the main [README](../../README.md), and every place the
implementation departed from the directive is an entry in one of the two
ledgers.
