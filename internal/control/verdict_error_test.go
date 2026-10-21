package control

import (
	"errors"
	"strings"
	"testing"

	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// An INCONCLUSIVE verdict used to carry `error` only for a recovered panic, so
// `thesis diagnose` met unjudged runs with no recorded reason. Every
// INCONCLUSIVE verdict now says why.
func TestEveryInconclusiveVerdictSaysWhy(t *testing.T) {
	for _, o := range []Outcome{OutcomeInconclusive, OutcomeHarnessError, OutcomeCanceled, Outcome("unheard-of")} {
		v := BuildVerdict(VerdictInput{RunID: "r_2026_10_21_0004", Profile: "linear", Outcome: o})
		if v.Verdict != schema.VerdictInconclusive || v.Error == "" {
			t.Errorf("outcome %q: verdict %s with error %q; want INCONCLUSIVE with a cause", o, v.Verdict, v.Error)
		}
	}
	if v := BuildVerdict(VerdictInput{Outcome: OutcomePass}); v.Error != "" {
		t.Errorf("a PASS carries an error: %q", v.Error)
	}

	narrowed := BuildVerdict(VerdictInput{Outcome: OutcomePass, Budget: schema.Budget{Narrowed: true}})
	if narrowed.Verdict != schema.VerdictInconclusive || !strings.Contains(narrowed.Error, "narrowed") {
		t.Errorf("a clean narrowed run: %s, error %q; want the narrowing named", narrowed.Verdict, narrowed.Error)
	}

	given := BuildVerdict(VerdictInput{Outcome: OutcomeHarnessError, Error: "world 2: harness_error: compose up: exit 1"})
	if given.Error != "world 2: harness_error: compose up: exit 1" {
		t.Errorf("the caller's cause was replaced: %q", given.Error)
	}
}

func TestTheRunCauseNamesTheWorldAndTheOracle(t *testing.T) {
	worlds := []WorldOutcome{
		{Ordinal: 1, Outcome: OutcomePass},
		{Ordinal: 2, Outcome: OutcomeInconclusive, Findings: []OracleResult{
			{Output: schema.OracleOutput{Oracle: "no_crash", Status: schema.StatusOK}},
			{Output: schema.OracleOutput{Oracle: "linearizable.kv", Status: schema.StatusInconclusive,
				Explanation: "history truncated at op 1180"}},
		}},
	}
	got := RunCause(OutcomeInconclusive, worlds)
	if !strings.Contains(got, "world 2") || !strings.Contains(got, "linearizable.kv") || !strings.Contains(got, "truncated") {
		t.Fatalf("cause %q does not name world 2's inconclusive oracle and its reason", got)
	}

	worlds[1].Findings[1] = OracleResult{Output: schema.OracleOutput{Oracle: "linearizable.kv"}, Err: errors.New("checker exited 137")}
	if got := RunCause(OutcomeInconclusive, worlds); !strings.Contains(got, "could not be evaluated: checker exited 137") {
		t.Fatalf("cause %q does not carry the oracle's execution error", got)
	}

	worlds[1].Err = errors.New("driver exited 5")
	if got := RunCause(OutcomeInconclusive, worlds); !strings.Contains(got, "driver exited 5") {
		t.Fatalf("cause %q does not carry the world's own error first", got)
	}

	if got := RunCause(OutcomeCanceled, nil); !strings.Contains(got, "no world ran") {
		t.Fatalf("a run interrupted before any world: %q", got)
	}
}
