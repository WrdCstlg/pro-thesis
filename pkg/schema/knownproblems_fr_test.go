package schema

import (
	"strings"
	"testing"
)

// A false_rejection entry explains false rejections only, so it must say which
// ones: an entry that matched every false rejection would explain them all
// away, the catch-all the registry forbids.
func TestAFalseRejectionEntryMustNameWhatItExplains(t *testing.T) {
	yes := true
	entry := func(level string, m KnownProblemMatch) *KnownProblems {
		return &KnownProblems{Schema: KnownProblemsSchema, Problems: []KnownProblem{{
			ID: "KP-001", Title: "t", Classification: KPDefectOpen, Ledger: "OQ-054", Level: level, Match: m,
		}}}
	}
	cases := []struct {
		name  string
		reg   *KnownProblems
		fails string
	}{
		{"world, by oracle", entry(KPLevelWorld, KnownProblemMatch{FalseRejection: &yes, Oracle: "linearizable.kv"}), ""},
		{"world, by reason", entry(KPLevelWorld, KnownProblemMatch{FalseRejection: &yes, ReasonRegex: "drift"}), ""},
		{"world, by outcome only", entry(KPLevelWorld, KnownProblemMatch{FalseRejection: &yes, Outcome: "violation"}),
			"every false rejection"},
		{"world, nothing else", entry(KPLevelWorld, KnownProblemMatch{FalseRejection: &yes}), "no predicate"},
		{"bundle, by arm", entry(KPLevelBundle, KnownProblemMatch{FalseRejection: &yes, Arm: "GP-01"}), ""},
		{"bundle, by verdict only", entry(KPLevelBundle, KnownProblemMatch{FalseRejection: &yes, Verdict: "FAIL"}),
			"names no arm"},
		{"arm on a world entry", entry(KPLevelWorld, KnownProblemMatch{Oracle: "no_crash", Arm: "GP-01"}),
			"bundle-level predicate"},
		{"arm alone on a bundle entry", entry(KPLevelBundle, KnownProblemMatch{Arm: "GP-01"}), ""},
	}
	for _, c := range cases {
		err := c.reg.Validate()
		switch {
		case c.fails == "" && err != nil:
			t.Errorf("%s: rejected: %v", c.name, err)
		case c.fails != "" && (err == nil || !strings.Contains(err.Error(), c.fails)):
			t.Errorf("%s: err %v, want one mentioning %q", c.name, err, c.fails)
		}
	}
}
