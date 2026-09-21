package perturber

import (
	"errors"
	"strings"
	"testing"

	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

func TestParseConstraintTheTwoDirectiveForms(t *testing.T) {
	// Both of these appear verbatim in the directive's section 4.2 sample and in
	// the config `thesis init` scaffolds. If either stopped parsing, every
	// scaffolded project would fail closed on its first run.
	c, err := ParseConstraint("never partition more than minority of kv")
	if err != nil {
		t.Fatalf("ParseConstraint: %v", err)
	}
	if c.Kind != ConstraintPartitionLimit || c.Service != "kv" || c.Limit != schema.QuorumMinority {
		t.Errorf("parsed = %+v", c)
	}
	if n, err := c.AllowedPartitionSize(3); err != nil || n != 1 {
		t.Errorf("minority of 3 = %d (%v), want 1", n, err)
	}

	c, err = ParseConstraint("pg must be reachable during SEED")
	if err != nil {
		t.Fatalf("ParseConstraint: %v", err)
	}
	if c.Kind != ConstraintReachable || c.Node != "pg" || c.Phase != schema.PhaseSeed {
		t.Errorf("parsed = %+v", c)
	}
}

func TestParseConstraintVariants(t *testing.T) {
	for _, s := range []string{
		"never partition more than majority of kv",
		"never partition more than 2 of kv",
		"Never partition more than MINORITY of kv.",
		"kv-n1 must be reachable during drive",
	} {
		if _, err := ParseConstraint(s); err != nil {
			t.Errorf("ParseConstraint(%q): %v", s, err)
		}
	}
}

// Fail closed. pkg/schema recorded the reason: "a silently ignored safety
// constraint would let the search partition a majority and manufacture a false
// violation".
func TestParseConstraintFailsClosed(t *testing.T) {
	for _, s := range []string{
		"",
		"never partition the cluster",
		"never partition more than most of kv",
		"kv must be nice during DRIVE",
		"pg must be reachable during LUNCH",
		"never partition more than minority of kv extra words",
	} {
		if _, err := ParseConstraint(s); !errors.Is(err, ErrUnparseableConstraint) {
			t.Errorf("ParseConstraint(%q) error = %v, want ErrUnparseableConstraint", s, err)
		}
	}
}

func TestParseConstraintsReportsEveryFailure(t *testing.T) {
	_, err := ParseConstraints([]string{
		"never partition more than minority of kv",
		"gibberish one",
		"gibberish two",
	})
	if err == nil {
		t.Fatal("expected errors")
	}
	if !strings.Contains(err.Error(), "constraints[1]") || !strings.Contains(err.Error(), "constraints[2]") {
		t.Errorf("both bad entries should be named: %v", err)
	}
}

func mustConstraints(t *testing.T, ss ...string) []Constraint {
	t.Helper()
	cs, err := ParseConstraints(ss)
	if err != nil {
		t.Fatalf("ParseConstraints: %v", err)
	}
	return cs
}

func mustSpec(t *testing.T, s string) schema.FaultSpec {
	t.Helper()
	spec, err := schema.ParseFault(s)
	if err != nil {
		t.Fatalf("ParseFault(%q): %v", s, err)
	}
	return spec
}

func TestPartitionLimitRefusesTooLargeASet(t *testing.T) {
	top := kvTopology(t)
	cs := mustConstraints(t, "never partition more than minority of kv")

	one := top.Service("kv")[:1]
	if err := CheckResolved(cs, mustSpec(t, "net.partition(minority(kv))@1..2"), one, top); err != nil {
		t.Errorf("partitioning 1 of 3 kv nodes is the minority and must be allowed: %v", err)
	}

	two := top.Service("kv")[:2]
	err := CheckResolved(cs, mustSpec(t, "net.partition(majority(kv))@1..2"), two, top)
	if !errors.Is(err, ErrConstraintViolated) {
		t.Fatalf("partitioning 2 of 3: error = %v, want ErrConstraintViolated", err)
	}
	if !strings.Contains(err.Error(), "allows at most 1") {
		t.Errorf("the error must state the limit: %v", err)
	}
}

// A 100% loss is a partition in everything but spelling. A constraint that
// refused net.partition while permitting net.loss(kv:*, 100) would be trivially
// circumventable.
func TestPartitionLimitCoversTotalPacketLoss(t *testing.T) {
	top := kvTopology(t)
	cs := mustConstraints(t, "never partition more than minority of kv")
	two := top.Service("kv")[:2]

	if err := CheckResolved(cs, mustSpec(t, "net.loss(majority(kv), pct=100)@1..2"), two, top); !errors.Is(err, ErrConstraintViolated) {
		t.Errorf("net.loss at 100%% must count as a partition: %v", err)
	}
	if err := CheckResolved(cs, mustSpec(t, "net.loss(majority(kv), pct=30)@1..2"), two, top); err != nil {
		t.Errorf("net.loss at 30%% is degradation, not a partition: %v", err)
	}
}

// Cutting one link isolates no node as a group: both endpoints still reach the
// cluster through the third. Counting the endpoints instead would refuse a legal
// and useful fault.
func TestPartitionLimitTreatsAnEdgeAsIsolatingNobody(t *testing.T) {
	top := kvTopology(t)
	cs := mustConstraints(t, "never partition more than minority of kv")
	pair := []Node{top.Nodes()[0], top.Nodes()[1]}
	if err := CheckResolved(cs, mustSpec(t, "net.partition(kv-n1<->kv-n2)@1..2"), pair, top); err != nil {
		t.Errorf("an edge cut partitions no group: %v", err)
	}
}

func TestPartitionLimitIgnoresOtherServices(t *testing.T) {
	top := kvTopology(t)
	cs := mustConstraints(t, "never partition more than minority of kv")
	pg, _ := top.Node("pg")
	if err := CheckResolved(cs, mustSpec(t, "net.partition(pg)@1..2"), []Node{pg}, top); err != nil {
		t.Errorf("a constraint about kv must not bind postgres: %v", err)
	}
}

func TestPartitionLimitNamingAnAbsentServiceIsAnError(t *testing.T) {
	top := kvTopology(t)
	cs := mustConstraints(t, "never partition more than minority of redis")
	one := top.Service("kv")[:1]
	if err := CheckResolved(cs, mustSpec(t, "net.partition(minority(kv))@1..2"), one, top); err == nil {
		t.Fatal("a constraint naming a service no node declares must be an error, not a no-op")
	}
}

func TestReachabilityConstraint(t *testing.T) {
	top := kvTopology(t)
	n1, _ := top.Node("kv-n1")
	n2, _ := top.Node("kv-n2")

	during := mustConstraints(t, "kv-n1 must be reachable during DRIVE")
	if err := CheckResolved(during, mustSpec(t, "proc.pause(kv-n1)@1..2"), []Node{n1}, top); !errors.Is(err, ErrConstraintViolated) {
		t.Errorf("pausing a node required during DRIVE must be refused: %v", err)
	}
	if err := CheckResolved(during, mustSpec(t, "proc.pause(kv-n2)@1..2"), []Node{n2}, top); err != nil {
		t.Errorf("the constraint names kv-n1 only: %v", err)
	}
	// Latency degrades a node; it does not remove it.
	if err := CheckResolved(during, mustSpec(t, "net.latency(kv-n1, mean=40)@1..2"), []Node{n1}, top); err != nil {
		t.Errorf("latency does not make a node unreachable: %v", err)
	}

	// BOOT and SEED precede DRIVE, and every fault window is DRIVE-relative, so
	// PERTURB cannot reach them. Such a constraint is parsed and recorded, and it
	// is trivially satisfied rather than pretended to be checked.
	seed := mustConstraints(t, "kv-n1 must be reachable during SEED")
	if err := CheckResolved(seed, mustSpec(t, "proc.pause(kv-n1)@1..2"), []Node{n1}, top); err != nil {
		t.Errorf("no PERTURB fault can reach SEED: %v", err)
	}

	// From HEAL onward, every durative fault has been withdrawn, so only the
	// removal that was never withdrawable survives.
	after := mustConstraints(t, "kv-n1 must be reachable during ASSERT")
	if err := CheckResolved(after, mustSpec(t, "proc.pause(kv-n1)@1..2"), []Node{n1}, top); err != nil {
		t.Errorf("a pause is withdrawn at HEAL and cannot reach ASSERT: %v", err)
	}
	if err := CheckResolved(after, mustSpec(t, "proc.kill(kv-n1)@1..2"), []Node{n1}, top); !errors.Is(err, ErrConstraintViolated) {
		t.Errorf("a kill is not withdrawable and does reach ASSERT: %v", err)
	}
}

// The compile-time pass may only decide what it can decide. A quorum target's
// SIZE is known from the topology, so a partition limit is checkable; its node
// IDENTITIES are not chosen until the fault's PRNG sub-stream is drawn, so a
// reachability constraint has to wait.
func TestCompileTimeChecksOnlyWhatIsKnown(t *testing.T) {
	top := kvTopology(t)
	limit := mustConstraints(t, "never partition more than minority of kv")
	reach := mustConstraints(t, "kv-n1 must be reachable during DRIVE")
	two := top.Service("kv")[:2]
	spec := mustSpec(t, "net.partition(majority(kv))@1..2")

	if err := CheckCompileTime(limit, spec, two, false, top); !errors.Is(err, ErrConstraintViolated) {
		t.Errorf("a size-based constraint is decidable without identities: %v", err)
	}
	if err := CheckCompileTime(reach, spec, two, false, top); err != nil {
		t.Errorf("an identity-based constraint must be deferred, not guessed: %v", err)
	}
	if err := CheckCompileTime(reach, spec, two, true, top); !errors.Is(err, ErrConstraintViolated) {
		t.Errorf("with exact identities it must be checked: %v", err)
	}
}

func TestCompileEnforcesConstraints(t *testing.T) {
	cfg := testConfig(t, nil, nil)
	cfg.Perturber.Constraints = []string{"never partition more than minority of kv"}
	if _, err := compileWith(t, cfg, []string{"net.partition(majority(kv))@100..200"}); !errors.Is(err, ErrConstraintViolated) {
		t.Fatalf("Compile must enforce the partition limit: %v", err)
	}
	if _, err := compileWith(t, cfg, []string{"net.partition(minority(kv))@100..200"}); err != nil {
		t.Errorf("a minority partition is exactly what the constraint permits: %v", err)
	}
	// kv:* is all three, which is more than the minority.
	if _, err := compileWith(t, cfg, []string{"net.partition(kv:*)@100..200"}); !errors.Is(err, ErrConstraintViolated) {
		t.Errorf("partitioning the whole group must be refused: %v", err)
	}
}
