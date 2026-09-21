# Security

## This tool executes code from the configuration file, by design

`driver.cmd`, `harness.steady_state.probe` and every external oracle's `cmd:` are
run as your user, with your privileges. Cloning a repository and running
`thesis run` executes **that repository's** choice of binaries. PRO-THESIS also
runs privileged sidecar containers joined to another container's network
namespace, executes `iptables` and `tc` inside them, kills and pauses processes
in containers, and creates and destroys Docker containers, networks and volumes.

`.prothesis/lock` is an **integrity** mechanism, not a sandbox. It tells a
reviewer that the gate moved. It does not confine anything.

**Do not run PRO-THESIS against a repository you would not run `make` in.**

The "Security model" section of the [README](README.md) is the short statement
of the same thing; the weaknesses that are open today are listed below.

## Status

This is research software at `0.1.0-phase0`, under an Apache-2.0 licence whose
Section 7 disclaimer applies in full. It has no release process, no versioned
support window, and no security-update channel. Nothing here should be read as a
commitment to patch on a schedule.

## Reporting a vulnerability

Open a GitHub issue. There is no
private disclosure channel, and given the threat model above (a tool whose
documented behaviour is to execute arbitrary code you point it at), most findings
are not usefully secret. If you believe you have found something that genuinely
should not be public before a fix exists, say so in an issue without the detail
and a private channel will be arranged.

## What is already public, and is not a report

The project publishes its own defects rather than fixing them quietly, so the
following are known and documented. An issue restating one of them is welcome as
a discussion, but it is not a disclosure:

| | |
|---|---|
| [`OPEN_QUESTIONS.md`](OPEN_QUESTIONS.md) | Every known defect and unresolved question, including working write-ups of gate bypasses |
| OQ-057 / D-060 | A swapped oracle executable is reported, not refused; the exit code is unchanged |
| OQ-059 / D-059 | `--worlds` / `--budget` narrow a lock-covered profile; now recorded, and a narrowed clean run cannot report PASS |
| OQ-061 / D-061 | Program resolution preferred the project directory over `PATH`; closed |
| D-069 | The build directive was removed from the repository *and its history*; history rewriting is not deletion, and the entry says what that does and does not achieve |

## Threat model, stated plainly

PRO-THESIS is designed for the case where the **system under test**, and
potentially the harness's own configuration, was written by an autonomous coding
agent that may weaken a gate rather than fix a defect. The lock, the exit-code
contract and the fail-closed oracles exist for that adversary.

It is **not** designed to defend the host against a malicious `prothesis.yaml`.
A configuration you did not write is code you did not read.
