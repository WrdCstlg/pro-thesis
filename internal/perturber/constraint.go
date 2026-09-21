package perturber

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// ---------------------------------------------------------------------------
// perturber.constraints
//
// Phase 0 stored these VERBATIM and validated nothing about them, and recorded
// the reason that could not stand: "a silently ignored safety constraint would
// let the search partition a majority and manufacture a false violation"
// (pkg/schema/config.go, PerturberConfig.Constraints). This file is the typed
// grammar that note asks for, and it FAILS CLOSED: a constraint that does not
// parse is refused, never ignored.
//
// The grammar covers exactly the two sentence shapes the directive itself
// writes, in section 4.2's sample config and in the scaffolded prothesis.yaml:
//
//	never partition more than <minority|majority|N> of <service>
//	<node> must be reachable during <PHASE>
//
// It is deliberately not an English parser. Widening it is an additive change;
// silently accepting a sentence nobody checks is not.
// ---------------------------------------------------------------------------

// ErrUnparseableConstraint reports a perturber.constraints entry outside the
// supported grammar.
var ErrUnparseableConstraint = errors.New("perturber: unparseable safety constraint")

// ErrConstraintViolated reports a schedule that a safety constraint refuses.
var ErrConstraintViolated = errors.New("perturber: fault schedule violates a safety constraint")

// ConstraintKind discriminates the two supported forms.
type ConstraintKind string

const (
	// ConstraintPartitionLimit is "never partition more than X of S".
	ConstraintPartitionLimit ConstraintKind = "partition_limit"
	// ConstraintReachable is "N must be reachable during PHASE".
	ConstraintReachable ConstraintKind = "reachable"
)

// Constraint is one parsed safety constraint.
type Constraint struct {
	Kind ConstraintKind
	// Source is the entry verbatim, so every diagnostic can quote what the
	// author actually wrote.
	Source string

	// Service and Limit/Count describe a partition limit. Limit is empty when
	// the author wrote an explicit count.
	Service string
	Limit   schema.QuorumFunc
	Count   int

	// Node and Phase describe a reachability constraint.
	Node  string
	Phase schema.Phase
}

// String renders the constraint back into its canonical sentence.
func (c Constraint) String() string {
	switch c.Kind {
	case ConstraintPartitionLimit:
		size := string(c.Limit)
		if size == "" {
			size = strconv.Itoa(c.Count)
		}
		return fmt.Sprintf("never partition more than %s of %s", size, c.Service)
	case ConstraintReachable:
		return fmt.Sprintf("%s must be reachable during %s", c.Node, c.Phase)
	}
	return c.Source
}

// AllowedPartitionSize is how many nodes of a group this constraint permits to
// be partitioned away, given the group's size.
func (c Constraint) AllowedPartitionSize(groupSize int) (int, error) {
	if c.Kind != ConstraintPartitionLimit {
		return 0, fmt.Errorf("perturber: %q is not a partition limit", c.Source)
	}
	if c.Limit == "" {
		return c.Count, nil
	}
	return QuorumSize(c.Limit, groupSize, 0)
}

const constraintGrammarHelp = "supported forms are " +
	"`never partition more than <minority|majority|N> of <service>` and " +
	"`<node> must be reachable during <PHASE>`"

// ParseConstraint parses one perturber.constraints entry.
func ParseConstraint(s string) (Constraint, error) {
	raw := strings.TrimSpace(s)
	trimmed := strings.TrimRight(raw, ".")
	toks := strings.Fields(trimmed)
	if len(toks) == 0 {
		return Constraint{}, fmt.Errorf("%w: empty constraint (%s)", ErrUnparseableConstraint, constraintGrammarHelp)
	}

	// never partition more than <size> of <service>
	if len(toks) == 7 &&
		eqFold(toks[0], "never") && eqFold(toks[1], "partition") &&
		eqFold(toks[2], "more") && eqFold(toks[3], "than") && eqFold(toks[5], "of") {
		c := Constraint{Kind: ConstraintPartitionLimit, Source: raw, Service: toks[6]}
		switch {
		case eqFold(toks[4], string(schema.QuorumMinority)):
			c.Limit = schema.QuorumMinority
		case eqFold(toks[4], string(schema.QuorumMajority)):
			c.Limit = schema.QuorumMajority
		default:
			n, err := strconv.Atoi(toks[4])
			if err != nil || n < 0 {
				return Constraint{}, fmt.Errorf("%w: %q: %q is not minority, majority or a non-negative count",
					ErrUnparseableConstraint, raw, toks[4])
			}
			c.Count = n
		}
		return c, nil
	}

	// <node> must be reachable during <PHASE>
	if len(toks) == 6 &&
		eqFold(toks[1], "must") && eqFold(toks[2], "be") &&
		eqFold(toks[3], "reachable") && eqFold(toks[4], "during") {
		p, err := schema.ParsePhase(strings.ToUpper(toks[5]))
		if err != nil {
			return Constraint{}, fmt.Errorf("%w: %q: %s", ErrUnparseableConstraint, raw, err.Error())
		}
		return Constraint{Kind: ConstraintReachable, Source: raw, Node: toks[0], Phase: p}, nil
	}

	return Constraint{}, fmt.Errorf("%w: %q (%s)", ErrUnparseableConstraint, raw, constraintGrammarHelp)
}

// ParseConstraints parses every entry, reporting all failures at once.
func ParseConstraints(ss []string) ([]Constraint, error) {
	out := make([]Constraint, 0, len(ss))
	var errs []error
	for i, s := range ss {
		c, err := ParseConstraint(s)
		if err != nil {
			errs = append(errs, fmt.Errorf("perturber.constraints[%d]: %w", i, err))
			continue
		}
		out = append(out, c)
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// enforcement
// ---------------------------------------------------------------------------

// isolatingKinds are the kinds that remove a node from the cluster's view.
//
// net.partition is the obvious one. proc.pause is here because a SIGSTOPped
// node is unreachable to its peers while remaining alive to the orchestrator,
// which is precisely the gray failure the directive calls top priority, and
// proc.kill and proc.restart because a dead process answers nothing.
func isolatingKind(k schema.FaultKind) bool {
	switch k {
	case schema.FaultNetPartition, schema.FaultProcPause, schema.FaultProcKill, schema.FaultProcRestart:
		return true
	}
	return false
}

// partitions reports whether a fault cuts its targets off from the rest of the
// cluster as a GROUP.
//
// net.loss at 100% is included because it is a partition in everything but
// spelling, and a constraint that refused net.partition while permitting
// net.loss(kv:*, 100) would be trivially circumventable, which is exactly the
// kind of hole invariant I6 exists to close.
func partitions(spec schema.FaultSpec) bool {
	switch spec.Kind {
	case schema.FaultNetPartition:
		return true
	case schema.FaultNetLoss:
		if v, ok := spec.Param("pct"); ok {
			if f, err := strconv.ParseFloat(v, 64); err == nil && f >= 100 {
				return true
			}
		}
	}
	return false
}

// checkPartitionLimit enforces a partition limit against a resolved node set.
//
// An EDGE target counts as isolating ZERO nodes. Cutting the link n1<->n2 in a
// three-node cluster leaves both endpoints reachable through n3, so no node is
// partitioned away as a group; the constraint's subject is the size of the
// isolated SET, and an edge does not create one. Counting the two endpoints
// instead would refuse `net.partition(n1<->n2)` under "never partition more than
// minority of kv", which is not what the sentence says.
func (c Constraint) checkPartitionLimit(spec schema.FaultSpec, nodes []Node, top *Topology) error {
	if c.Kind != ConstraintPartitionLimit || !partitions(spec) {
		return nil
	}
	group := top.Service(c.Service)
	if len(group) == 0 {
		return fmt.Errorf("perturber: constraint %q names service %q, which no node declares (have: %s)",
			c.Source, c.Service, servicesText(top))
	}
	allowed, err := c.AllowedPartitionSize(len(group))
	if err != nil {
		return err
	}
	isolated := 0
	if spec.Target.Kind != schema.TargetEdge {
		for _, n := range nodes {
			if n.Service == c.Service {
				isolated++
			}
		}
	}
	if isolated > allowed {
		return fmt.Errorf("%w: %s would partition %d of %d %s nodes (%s), but the constraint %q allows at most %d",
			ErrConstraintViolated, spec.String(), isolated, len(group), c.Service,
			strings.Join(nodeIDs(nodes), ", "), c.Source, allowed)
	}
	return nil
}

// checkReachable enforces a reachability constraint against a resolved node set.
//
// # What is and is not enforceable, stated rather than implied
//
// Fault windows are milliseconds relative to DRIVE start, and PERTURB is
// contained in DRIVE, so no fault this package schedules can reach BOOT or SEED.
// A constraint naming either (the directive's own "pg must be reachable during
// SEED" is one) is therefore parsed, recorded, and TRIVIALLY SATISFIED. It is
// not silently dropped, and it is not claimed to have been checked against
// something it could never fail.
//
// From HEAL onwards every durative fault has been withdrawn, so the only fault
// that can still make a node unreachable there is one that was never
// withdrawable in the first place: proc.kill.
func (c Constraint) checkReachable(spec schema.FaultSpec, nodes []Node) error {
	if c.Kind != ConstraintReachable {
		return nil
	}
	hit := false
	for _, n := range nodes {
		if n.ID == c.Node {
			hit = true
			break
		}
	}
	if !hit {
		return nil
	}
	switch {
	case c.Phase.Ordinal() < schema.PhaseDrive.Ordinal():
		// Unreachable by construction: see the doc comment.
		return nil
	case c.Phase.Ordinal() <= schema.PhasePerturb.Ordinal():
		if !isolatingKind(spec.Kind) && !partitions(spec) {
			return nil
		}
	default:
		// HEAL and later: withdrawal has already happened, so only a
		// non-withdrawable removal survives.
		if spec.Kind != schema.FaultProcKill {
			return nil
		}
	}
	return fmt.Errorf("%w: %s makes node %q unreachable, but the constraint %q requires it during %s",
		ErrConstraintViolated, spec.String(), c.Node, c.Source, c.Phase)
}

// CheckResolved applies every constraint to a fault whose target has been bound
// to concrete nodes. It is the exact check, and it runs at injection time.
func CheckResolved(cs []Constraint, spec schema.FaultSpec, nodes []Node, top *Topology) error {
	for _, c := range cs {
		if err := c.checkPartitionLimit(spec, nodes, top); err != nil {
			return err
		}
		if err := c.checkReachable(spec, nodes); err != nil {
			return err
		}
	}
	return nil
}

// CheckCompileTime applies the constraints that can be decided before a target
// binds to concrete nodes.
//
// A partition limit is a question about the SIZE of the isolated set, and a
// quorum target's size is known from the topology alone, so it is checked here,
// with `nodes` standing in for the count. A reachability constraint is a
// question about IDENTITY, so it is checked here only when the identities are
// already exact (node, wildcard and edge targets) and deferred to CheckResolved
// otherwise.
//
// Nothing is guessed. Assuming the worst for `role:leader` would refuse
// `net.partition(role:leader)` under "never partition more than minority of kv"
// on a three-node cluster, which is a legal and useful fault, and refusing a
// legal fault teaches an author to delete the constraint.
func CheckCompileTime(cs []Constraint, spec schema.FaultSpec, nodes []Node, exactIdentities bool, top *Topology) error {
	for _, c := range cs {
		if err := c.checkPartitionLimit(spec, nodes, top); err != nil {
			return err
		}
		if exactIdentities {
			if err := c.checkReachable(spec, nodes); err != nil {
				return err
			}
		}
	}
	return nil
}

func eqFold(a, b string) bool { return strings.EqualFold(a, b) }
