package oracle

import (
	"fmt"
	"os"
	"path/filepath"
)

// ---------------------------------------------------------------------------
// `thesis init` scaffolding for oracles.dir
// ---------------------------------------------------------------------------
//
// An empty oracles directory teaches a new project nothing, and the format is
// additive (OQ-026) so there is nowhere else to learn it from. The scaffold
// therefore ships the reference definition (`linearizable.kv`, the consistency
// checker this tool exists to run) as a complete, valid file.
//
// It ships it as `linearizable.kv.yaml.example`, NOT as a live definition, and
// that is the deliberate part.
//
// A live definition names an executable. On a project that has just run
// `thesis init` that executable does not exist yet, so every single run would
// report an external oracle that could not be started: INCONCLUSIVE, exit 2,
// forever, on a project that has not done anything wrong. The right answer to
// "the oracle you configured is not on disk" is exit 2; the wrong thing is to
// configure one the user never asked for and then report it.
//
// Renaming the file is the activation step, it is one command, and it is
// exactly the kind of change .prothesis/lock is meant to see: adding an oracle
// moves the lock digest and needs `thesis oracles lock --reason "..."` in a
// separate reviewed commit. See DECISIONS.md D-039.

// ExampleDefinitionFileName is the scaffolded example, deliberately carrying an
// extension Discover does not treat as a definition.
const ExampleDefinitionFileName = "linearizable.kv.yaml.example"

// ReadmeFileName documents the format beside the definitions it describes,
// where somebody editing one will actually see it.
const ReadmeFileName = "README.md"

// Scaffold writes the oracle-directory scaffold into dir, creating the
// directory if needed.
//
// It NEVER overwrites. Clobbering a file in this directory would silently move
// the lock digest, and a scaffold that can rewrite a reviewed oracle is a
// gate-weakening tool. The returned slice names the files actually written.
func Scaffold(dir string) ([]string, error) {
	if dir == "" {
		return nil, fmt.Errorf("oracle: scaffold: no directory")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("oracle: scaffold %s: %w", dir, err)
	}
	var written []string
	for _, f := range []struct{ name, body string }{
		{ExampleDefinitionFileName, exampleDefinition},
		{ReadmeFileName, oraclesReadme},
	} {
		path := filepath.Join(dir, f.name)
		if _, err := os.Stat(path); err == nil {
			continue
		}
		if err := os.WriteFile(path, []byte(f.body), 0o644); err != nil {
			return written, fmt.Errorf("oracle: scaffold %s: %w", path, err)
		}
		written = append(written, path)
	}
	return written, nil
}

// exampleDefinition is a COMPLETE, VALID definition. It is parsed by
// TestScaffoldedExampleIsAValidDefinition, so the scaffold can never drift into
// something the tool would reject.
const exampleDefinition = `# EXAMPLE ORACLE DEFINITION — NOT ACTIVE
#
# This file is inert: ` + "`thesis`" + ` discovers only *.yaml and *.yml in this
# directory, and this one ends in .example. It is here so a new project has a
# working definition to copy rather than a blank directory and a format to guess.
#
# To activate it:
#   1. build the checker so that ` + "`cmd`" + ` below actually resolves
#   2. rename this file to        linearizable.kv.yaml
#   3. thesis oracles lock --reason "add the linearizable.kv consistency oracle"
#
# Step 3 is not optional: adding an oracle changes .prothesis/lock, and a lock
# bump is meant to be a separate, human-reviewed commit.

version: prothesis.oracle_def/v1

# The oracle's identity. It appears verbatim in the verdict's violations[].oracle
# and is what .prothesis/lock addresses. If the executable prints an "oracle"
# field of its own, it must print this same string, or the finding is rejected as
# INCONCLUSIVE — a mismatch means the wrong binary is wired up.
name: linearizable.kv

# One of: crash, consistency, liveness, convergence,
#         resource, safety, differential, metamorphic
#
# Declared HERE and never taken from the oracle's output, because severity is
# derived from the class. An oracle that could choose its own class could grade
# its own finding down.
class: consistency

# Invariant I5: the lifecycle phases in which EVALUATING this oracle is
# meaningful. The ENGINE enforces this; the oracle is never even asked outside
# these phases. Checking consistency during an active partition is a
# false-positive factory, so a consistency oracle is ASSERT-only.
#
# This is NOT where a finding may be OBSERVED. An ASSERT-only oracle routinely
# reports evidence that lies in DRIVE — that is exactly what a stale read looks
# like — and the two fields are never checked against each other.
valid_phases: [ASSERT]

# The executable and its arguments. Relative programs resolve against the
# directory prothesis.yaml lives in, which is also the working directory.
#
# No placeholders: an oracle receives prothesis.oracle_input/v1 on STDIN and
# writes prothesis.oracle_output/v1 on STDOUT, exiting 0 (ok), 1 (violated) or
# 2 (inconclusive). Anything in braces here would be passed through literally.
cmd: "./bin/linearizable-kv"

# Bounds ONE evaluation. On expiry the oracle's whole process tree is killed and
# the finding is INCONCLUSIVE — never ok. Required, because an oracle with no
# timeout can hang a run forever, and a run that never returns has no verdict.
timeout: 120s
`

// oraclesReadme is the format reference, kept beside the definitions.
const oraclesReadme = `# ` + "`.prothesis/oracles`" + `

Oracle definitions. One YAML file per oracle, one oracle per file.

Everything in this directory is content-hashed into ` + "`.prothesis/lock`" + `.
Changing, adding or removing anything here moves the digest, and a run whose
digest does not match the lock exits **4** (` + "`ORACLE_DRIFT`" + `) — the one
exit code an agent loop must never resolve on its own. Re-baseline it
deliberately:

    thesis oracles lock --reason "why this changed"

## The definition format — ` + "`prothesis.oracle_def/v1`" + `

    version: prothesis.oracle_def/v1
    name: linearizable.kv
    class: consistency
    valid_phases: [ASSERT]
    cmd: "./bin/linearizable-kv"
    timeout: 120s

| key | required | meaning |
|---|---|---|
| ` + "`version`" + ` | yes | always ` + "`prothesis.oracle_def/v1`" + ` |
| ` + "`name`" + ` | yes | the identity in the verdict and in the lock; unique across the directory |
| ` + "`class`" + ` | yes | one of ` + "`crash`" + `, ` + "`consistency`" + `, ` + "`liveness`" + `, ` + "`convergence`" + `, ` + "`resource`" + `, ` + "`safety`" + `, ` + "`differential`" + `, ` + "`metamorphic`" + ` |
| ` + "`valid_phases`" + ` | yes | lifecycle phases in which **evaluating** the oracle is meaningful (invariant I5) |
| ` + "`cmd`" + ` | yes | the executable and its arguments; relative to the project directory |
| ` + "`timeout`" + ` | yes | wall-clock bound on one evaluation, e.g. ` + "`120s`" + ` |

Unknown keys are an error, not a default. A file that does not parse fails the
run rather than being skipped: an oracle that silently disappears would let a
gate go green over a property nobody checked.

Files whose names do not end in ` + "`.yaml`" + ` or ` + "`.yml`" + ` are ignored,
so a README, a checker's source, or a committed helper script can live here too.

## The executable contract (directive 4.5)

An oracle is a separate process. It may be written in any language and may carry
its own dependencies — that is the reason the contract exists.

* **stdin** — ` + "`prothesis.oracle_input/v1`" + `: the paths to this world's
  history, final state, telemetry and ` + "`.thesis`" + ` file, plus the
  **measured** phase windows in milliseconds relative to DRIVE start.
* **stdout** — ` + "`prothesis.oracle_output/v1`" + `: ` + "`schema`" + `,
  ` + "`status`" + ` (` + "`ok`" + ` / ` + "`violated`" + ` / ` + "`inconclusive`" + `),
  ` + "`witness`" + ` and ` + "`explanation`" + `. Echoing ` + "`oracle`" + `,
  ` + "`class`" + ` and ` + "`valid_phases`" + ` is optional; if you do echo them
  they must match this file.
* **exit code** — ` + "`0`" + ` ok, ` + "`1`" + ` violated, ` + "`2`" + ` inconclusive.

### The rules that decide whether your oracle is believed

1. **The exit code and the reported status must agree.** They are two
   independent channels and a disagreement is a defect in the oracle, so
   *neither* reading is adopted: the finding becomes ` + "`inconclusive`" + ` and
   says which channel said what.
2. **Anything that is not a clean answer is ` + "`inconclusive`" + `, never
   ` + "`ok`" + `.** A crash, a hang that hits the timeout, an unparseable
   document, an empty stdout, an exit code outside 0/1/2, or more than 1 MiB of
   stdout all land there. Exit 2 means "retry once, then escalate to a human".
3. **Report ` + "`inconclusive`" + ` when you could not check.** A checker handed
   an empty history has checked nothing; one that exhausted its budget has
   checked part of something. Saying ` + "`ok`" + ` there is the single most
   damaging thing an oracle can do, because the gate goes green precisely
   because nothing happened.
4. **Only report ` + "`violated`" + ` with a witness.** A false positive destroys
   the value of every other verdict this tool emits.

### Placing a finding on the timeline

` + "`prothesis.oracle_output/v1`" + ` carries no timestamp, so two **optional**
witness members are read (additive — see ` + "`OPEN_QUESTIONS.md`" + ` OQ-030):

    "witness": {
      "op_ids": [90002, 90117],
      "key": "k/42",
      "first_seen_ms": 11084,
      "phase": "DRIVE"
    }

* ` + "`first_seen_ms`" + ` — milliseconds relative to DRIVE start. The engine
  derives the observed phase from it against the measured windows.
* ` + "`phase`" + ` — pins the observed phase directly, for evidence with no
  usable timestamp.

Both describe where the **evidence** lies. That is
` + "`violations[].phase`" + `, which is a *different field* from this file's
` + "`valid_phases`" + ` and is routinely outside it: a stale read found by an
ASSERT-only consistency oracle is observed in DRIVE. Nothing checks one against
the other.

### stderr

Captured into the run bundle at
` + "`<run>/world-NNNN/oracles/<name>.stderr.log`" + ` and excerpted into the
explanation when the finding is not ` + "`ok`" + `. Write diagnostics there
freely; stdout is the contract channel and must carry only the document.
`
