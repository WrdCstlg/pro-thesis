package search

import (
	"strings"
	"testing"

	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// TestACleanWorldIsDistinguishableFromAnUnevaluableOne is the ranked #1 failure
// mode expressed as an assertion. If these two produced the same signal, every
// ranking the search computes downstream would be built on noise.
func TestACleanWorldIsDistinguishableFromAnUnevaluableOne(t *testing.T) {
	clean := Outcome{
		Observed: true,
		Findings: []Finding{
			okFinding("no_crash", schema.ClassCrash),
			okFinding("no_stuck_op", schema.ClassLiveness),
			okFinding("linearizable.kv", schema.ClassConsistency),
		},
	}
	// The measured OQ-033 shape: linearizable.kv answered, no_stuck_op could not.
	unevaluable := Outcome{
		Observed: true,
		Findings: []Finding{
			okFinding("no_crash", schema.ClassCrash),
			inconclusiveFinding("no_stuck_op", schema.ClassLiveness),
			okFinding("linearizable.kv", schema.ClassConsistency),
		},
	}
	if clean.Signal() != SignalClean {
		t.Fatalf("a world whose every oracle returned ok has signal %q, want %q", clean.Signal(), SignalClean)
	}
	if unevaluable.Signal() != SignalUnknown {
		t.Fatalf("a world with an inconclusive oracle has signal %q, want %q — "+
			"treating it as clean would teach the search that the schedule was survivable when nothing was established",
			unevaluable.Signal(), SignalUnknown)
	}
	if !strings.Contains(unevaluable.Why(), "no_stuck_op") {
		t.Fatalf("Why() must name the oracle that could not answer, got %q", unevaluable.Why())
	}
}

func TestAViolationOutranksAnInconclusiveOracle(t *testing.T) {
	// This is the measured `linear` FAIL: linearizable.kv violated while
	// no_stuck_op was inconclusive (OQ-033). The run exits 1, and the search
	// must agree.
	o := Outcome{
		Observed: true,
		Findings: []Finding{
			violatedFinding("linearizable.kv", schema.ClassConsistency, schema.SeverityHigh),
			inconclusiveFinding("no_stuck_op", schema.ClassLiveness),
		},
	}
	if o.Signal() != SignalViolated {
		t.Fatalf("signal = %q, want %q", o.Signal(), SignalViolated)
	}
	if len(o.Violations()) != 1 {
		t.Fatalf("Violations() returned %d, want 1", len(o.Violations()))
	}
	if len(o.Indefinite()) != 1 {
		t.Fatalf("Indefinite() returned %d, want 1 (the inconclusive oracle is still reported)", len(o.Indefinite()))
	}
}

func TestZeroOraclesIsNeverClean(t *testing.T) {
	o := Outcome{Observed: true}
	if o.Signal() != SignalUnknown {
		t.Fatalf("a world with no oracle findings has signal %q; zero oracles is the vacuous pass", o.Signal())
	}
	if !strings.Contains(o.Why(), "nothing was checked") {
		t.Fatalf("Why() = %q, want it to say nothing was checked", o.Why())
	}
}

func TestAnUnobservedWorldIsNeverClean(t *testing.T) {
	o := Outcome{
		Observed: false,
		Findings: []Finding{okFinding("no_crash", schema.ClassCrash)},
	}
	if o.Signal() != SignalUnknown {
		t.Fatalf("a world that never reached DRIVE has signal %q, want %q", o.Signal(), SignalUnknown)
	}
}

func TestAHarnessErrorIsNeverClean(t *testing.T) {
	o := Outcome{
		Observed: true,
		Findings: []Finding{okFinding("no_crash", schema.ClassCrash)},
		Err:      "compose up failed: no free bridge networks",
	}
	if o.Signal() != SignalUnknown {
		t.Fatalf("a world that errored has signal %q, want %q", o.Signal(), SignalUnknown)
	}
}

// TestAnOracleThatCouldNotRunIsNotOK: an oracle whose process failed carries
// Err. Its Status field may still hold a zero-ish value, and a Definite() that
// ignored Err would read a failed oracle as a passing one.
func TestAnOracleThatCouldNotRunIsNotOK(t *testing.T) {
	f := Finding{Oracle: "linearizable.kv", Class: schema.ClassConsistency,
		Status: schema.StatusOK, Err: "exec: \"./bin/linearizable-kv\": file does not exist"}
	if f.Definite() {
		t.Fatal("an oracle that could not be executed reported a definite answer")
	}
	o := Outcome{Observed: true, Findings: []Finding{f}}
	if o.Signal() != SignalUnknown {
		t.Fatalf("signal = %q, want %q", o.Signal(), SignalUnknown)
	}
}

func TestSignalRankOrdersViolatedAboveCleanAboveUnknown(t *testing.T) {
	if !(SignalViolated.Rank() > SignalClean.Rank() && SignalClean.Rank() > SignalUnknown.Rank()) {
		t.Fatalf("rank order is wrong: violated=%d clean=%d unknown=%d",
			SignalViolated.Rank(), SignalClean.Rank(), SignalUnknown.Rank())
	}
}
