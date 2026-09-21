package shrink

import (
	"context"
	"strings"
	"testing"

	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// ---------------------------------------------------------------------------
// RULE 1: an accepted reduction must reproduce the SAME violation
// ---------------------------------------------------------------------------

func TestMatchDecidesWhatCountsAsTheSameViolation(t *testing.T) {
	cases := []struct {
		name  string
		level Strictness
		cand  Identity
		want  bool
		// reason is a substring the refusal must name, so the report says
		// something a human can act on rather than "rejected".
		reason string
	}{
		{
			name:  "the same finding matches at every level",
			level: MatchWitnessOps,
			cand:  origID,
			want:  true,
		},
		{
			name:   "a DIFFERENT oracle is never a reduction",
			level:  MatchOracleClass,
			cand:   aDifferentBug,
			want:   false,
			reason: "oracle",
		},
		{
			name:  "the same oracle at a different class is not the same finding",
			level: MatchOracleClass,
			cand: Identity{Oracle: origID.Oracle, Class: schema.ClassSafety,
				Severity: schema.SeverityMedium, Key: origID.Key},
			want:   false,
			reason: "class",
		},
		{
			name:  "a violation on a different key is a different finding",
			level: MatchWitnessKey,
			cand: Identity{Oracle: origID.Oracle, Class: origID.Class,
				Severity: origID.Severity, Key: "k/91"},
			want:   false,
			reason: "witness key",
		},
		{
			name:  "the same defect with DIFFERENT op ids still matches (OQ-034)",
			level: MatchWitnessKey,
			cand:  sameBugDifferentOps,
			want:  true,
		},
		{
			name:   "the same defect with different op ids is REFUSED at MatchWitnessOps",
			level:  MatchWitnessOps,
			cand:   sameBugDifferentOps,
			want:   false,
			reason: "op id",
		},
		{
			name:  "a witness with no key degrades to oracle+class, never below it",
			level: MatchWitnessKey,
			cand:  Identity{Oracle: origID.Oracle, Class: origID.Class, Severity: origID.Severity},
			want:  true,
		},
		{
			name:  "a candidate on a different key is still refused when the ORIGINAL has a key",
			level: MatchWitnessKey,
			cand: Identity{Oracle: origID.Oracle, Class: origID.Class,
				Severity: origID.Severity, Key: "k/7"},
			want:   false,
			reason: "witness key",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, why := tc.level.Match(origID, tc.cand)
			if got != tc.want {
				t.Fatalf("Match(%s) = %v (%s), want %v", tc.level, got, why, tc.want)
			}
			if !tc.want && !strings.Contains(why, tc.reason) {
				t.Fatalf("refusal reason %q does not name %q", why, tc.reason)
			}
		})
	}
}

func TestMatchRefusesAnOriginalWithNoOracle(t *testing.T) {
	ok, why := MatchWitnessKey.Match(Identity{}, origID)
	if ok {
		t.Fatalf("an empty original matched %s; there is nothing to shrink toward", origID)
	}
	if !strings.Contains(why, "no oracle") {
		t.Fatalf("reason = %q", why)
	}
}

// TestADifferentBugIsNeverAcceptedAsAReduction is the ranked #1 failure mode,
// exercised end to end through the prober rather than through Match alone.
//
// The candidate FAILS (the world is not clean, an oracle reported a violation)
// and it is still rejected, because the violation is not the one being shrunk
// toward. Recording it as a successful reduction would produce a "minimal repro"
// that reproduces something else, and an agent reading it would fix the wrong
// thing.
func TestADifferentBugIsNeverAcceptedAsAReduction(t *testing.T) {
	fake := newFake(func(c Candidate, nth int) Attempt { return differentBug() })
	led := newLedger(Budget{Worlds: -1, Wall: -1}, nil)
	p := newProber(fake, origID, Policy{}.withDefaults(), led, nil)

	d, stop := p.test(context.Background(), Candidate{Faults: faultList(3)})
	if stop != StopComplete {
		t.Fatalf("stop = %q", stop)
	}
	if d.accepted() {
		t.Fatalf("a candidate that failed a DIFFERENT oracle was accepted as a reduction")
	}
	if d.Signal != SignalDifferent {
		t.Fatalf("signal = %s, want %s: a different violation must be distinguishable "+
			"from a clean world", d.Signal, SignalDifferent)
	}
	if !strings.Contains(d.Reason, "DIFFERENT") {
		t.Fatalf("reason = %q; it must say a different violation fired", d.Reason)
	}
	div := p.divergences()
	if len(div) != 1 || div[0].Oracle != aDifferentBug.Oracle {
		t.Fatalf("divergences = %v, want the different oracle recorded for the report", div)
	}
}

// TestTheSameBugWithShiftedOpIdsIsAccepted is the other half of RULE 1: too
// strict is also a defect. OQ-034 measured two runs of the same defect under the
// same schedule producing entirely different witness op ids, so a policy that
// demanded op-id equality would reject every genuine reproduction and report
// "nothing could be removed" after spending the whole budget.
func TestTheSameBugWithShiftedOpIdsIsAccepted(t *testing.T) {
	fake := newFake(func(c Candidate, nth int) Attempt { return reproducedAs(sameBugDifferentOps) })
	led := newLedger(Budget{Worlds: -1, Wall: -1}, nil)
	p := newProber(fake, origID, Policy{}.withDefaults(), led, nil)

	d, _ := p.test(context.Background(), Candidate{Faults: faultList(3)})
	if !d.accepted() {
		t.Fatalf("the same defect with shifted op ids was rejected (%s: %s); "+
			"see OQ-034 for the measurement that says it must not be", d.Signal, d.Reason)
	}
	if d.Trials != 1 {
		t.Fatalf("trials = %d, want 1: a single reproduction is a proof", d.Trials)
	}
}

func TestIdentityOfAViolationAndOfItsOutputAgree(t *testing.T) {
	out := schema.OracleOutput{
		Schema:      schema.OracleOutputSchema,
		Oracle:      "linearizable.kv",
		Class:       schema.ClassConsistency,
		Status:      schema.StatusViolated,
		Explanation: "no linearization exists for the 445 operation(s) on this key",
		Witness:     schema.Witness{Key: "k/0", OpIDs: []int64{11430, 11597, 11602}},
	}
	v := out.ToViolation("v1", schema.PhaseAssert, 11084)

	a := IdentityOfOutput(out)
	b := IdentityOf(v)
	if a.Oracle != b.Oracle || a.Class != b.Class || a.Severity != b.Severity || a.Key != b.Key {
		t.Fatalf("IdentityOfOutput = %+v, IdentityOf(violation) = %+v; they must agree, or a "+
			"shrink judged against one would silently reject the other", a, b)
	}
	if ok, why := MatchWitnessOps.Match(a, b); !ok {
		t.Fatalf("the two identities do not match at the strictest level: %s", why)
	}
}

func TestPolicyRefusesAMatchLevelBelowTheDirectivesFloor(t *testing.T) {
	err := Policy{Strictness: MatchOracle}.Validate()
	if err == nil {
		t.Fatal("Policy{Strictness: MatchOracle}.Validate() = nil; oracle name alone is below " +
			"the directive's floor of oracle AND class")
	}
	if !strings.Contains(err.Error(), "MatchOracleClass") {
		t.Fatalf("error %q does not name the remedy", err)
	}
	// And the ZERO policy must not land on it, because the zero value of the
	// Strictness type IS MatchOracle.
	if got := (Policy{}).withDefaults().Strictness; got != MatchWitnessKey {
		t.Fatalf("the zero Policy resolves to %s, want %s", got, MatchWitnessKey)
	}
}

func TestPolicyRefusesAnInvertedConfirmationAsymmetry(t *testing.T) {
	err := Policy{AcceptTrials: 3, RejectTrials: 1}.Validate()
	if err == nil {
		t.Fatal("Validate() = nil for AcceptTrials > RejectTrials; that inverts RULE 2")
	}
	if !strings.Contains(err.Error(), "asymmetry") {
		t.Fatalf("error %q does not explain the asymmetry", err)
	}
}
