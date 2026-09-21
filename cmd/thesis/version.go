package main

// version is the tool version.
//
// It is deliberately NOT part of any hash. Digesting the running binary's
// version into the oracle lock would turn every upgrade of `thesis` into an
// exit-4 ORACLE_DRIFT incident across every downstream project, and exit 4 is
// the one code the agent loop must never auto-resolve. See OPEN_QUESTIONS.md
// OQ-015.
const version = "0.1.0-phase0"
