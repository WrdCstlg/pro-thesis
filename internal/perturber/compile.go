package perturber

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// ---------------------------------------------------------------------------
// Schedule compilation
//
// A []FaultSpec becomes a TOTALLY ORDERED event list against the virtual clock:
// an inject at each window's start and a withdraw at its end, sorted, with ties
// broken deterministically. Everything that can be refused is refused HERE,
// before a container is touched (an unknown kind, a target that matches
// nothing, a budget overrun, a safety constraint, a missing mechanism) because
// a schedule that fails eight seconds into DRIVE has already perturbed the
// system it was about to declare untested.
// ---------------------------------------------------------------------------

// ErrBudgetExceeded reports a schedule that exceeds perturber.budget.
var ErrBudgetExceeded = errors.New("perturber: schedule exceeds perturber.budget")

// ErrKindNotAllowed reports a fault kind outside perturber.allow / perturber.deny.
//
// Narrowing the permitted fault space is an anti-gaming concern: the allow list
// is one of the things the Phase 3 lock covers, precisely so a green gate cannot
// be bought by quietly removing the fault that was failing. A kind outside the
// list is refused rather than skipped, so removing a kind from `allow` while a
// schedule still names it is loud.
var ErrKindNotAllowed = errors.New("perturber: fault kind is not permitted by perturber.allow/deny")

// EventKind discriminates the two things that happen to a fault.
type EventKind uint8

const (
	// EventWithdraw removes a fault at its window end. It sorts BEFORE an inject
	// at the same instant: see Schedule ordering.
	EventWithdraw EventKind = iota
	// EventInject applies a fault at its window start.
	EventInject
)

func (k EventKind) String() string {
	if k == EventInject {
		return "inject"
	}
	return "withdraw"
}

// PlannedFault is one compiled entry of the planned schedule.
type PlannedFault struct {
	// ID is the schedule-local fault id, `f001`. It is half of the ownership tag
	// `thesis:<run_id>:<fault_id>` that every injected rule carries (D-026), so
	// it is short, stable within a world, and free of characters an iptables
	// comment cannot hold.
	ID string
	// Spec is the parsed fault.
	Spec schema.FaultSpec
	// Canonical is Spec.String(): the wire form, with every declared parameter
	// materialized.
	Canonical string
	// Occurrence distinguishes duplicates of one canonical string, so two
	// identical faults draw from different PRNG sub-streams rather than
	// resolving to the same node twice.
	Occurrence int
}

// Event is one scheduled action.
type Event struct {
	// Seq is the position in the total order. It is what makes "inject ordering
	// is deterministic" a checkable claim.
	Seq int
	// Kind is inject or withdraw.
	Kind EventKind
	// AtMS is the virtual-clock millisecond the event is scheduled for,
	// relative to DRIVE start.
	AtMS int64
	// Fault indexes Schedule.Faults.
	Fault int
}

// Schedule is a compiled, totally ordered fault schedule.
type Schedule struct {
	// Faults are the planned faults, canonically ordered by (start, end,
	// string). Fault ids are assigned in this order.
	Faults []PlannedFault
	// Events is the total order the executor walks.
	Events []Event
	// Constraints are the parsed perturber.constraints, carried through so the
	// executor can re-check them once dynamic targets have bound.
	Constraints []Constraint
	// PeakConcurrent is the greatest number of simultaneously active faults the
	// schedule reaches.
	PeakConcurrent int
}

// Len is the number of planned faults.
func (s *Schedule) Len() int {
	if s == nil {
		return 0
	}
	return len(s.Faults)
}

// Empty reports whether there is nothing to do.
func (s *Schedule) Empty() bool { return s.Len() == 0 }

// Planned returns the canonical fault strings in schedule order. This is what
// goes into a world's fault_schedule.planned.
func (s *Schedule) Planned() []string {
	if s == nil {
		return []string{}
	}
	out := make([]string, 0, len(s.Faults))
	for _, f := range s.Faults {
		out = append(out, f.Canonical)
	}
	return out
}

// Kinds lists the distinct kinds the schedule uses, sorted.
func (s *Schedule) Kinds() []schema.FaultKind {
	if s == nil {
		return nil
	}
	seen := map[schema.FaultKind]bool{}
	out := make([]schema.FaultKind, 0, len(s.Faults))
	for _, f := range s.Faults {
		if seen[f.Spec.Kind] {
			continue
		}
		seen[f.Spec.Kind] = true
		out = append(out, f.Spec.Kind)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// LastEventMS is the virtual millisecond of the final event, or 0 when empty.
// It is what bounds PERTURB.
func (s *Schedule) LastEventMS() int64 {
	if s == nil || len(s.Events) == 0 {
		return 0
	}
	return s.Events[len(s.Events)-1].AtMS
}

// CompileOptions is the input to Compile.
type CompileOptions struct {
	// Config supplies perturber.budget, perturber.allow/deny and
	// perturber.constraints. Required.
	Config *schema.Config
	// Topology is what static targets resolve against. Required: a schedule
	// cannot be checked for zero-match targets without one, and skipping the
	// check is the failure mode this package exists to prevent.
	Topology *Topology
	// Planned is the schedule as authored: canonical or human-written fault
	// strings.
	Planned []string
	// Registry, when non-nil, is checked so that a kind with no mechanism is
	// refused at compile time rather than discovered mid-DRIVE.
	Registry *Registry
}

// Compile turns authored fault strings into an ordered event list.
func Compile(opts CompileOptions) (*Schedule, error) {
	if opts.Config == nil {
		return nil, errors.New("perturber: Compile needs a config")
	}
	if opts.Topology == nil {
		return nil, errors.New("perturber: Compile needs a topology; a schedule whose targets " +
			"are never checked against one can resolve to nothing and report as injected")
	}

	// Constraints are parsed even for an empty schedule, so a typo in
	// perturber.constraints is reported by the first run rather than by the
	// first run that happens to inject something.
	constraints, err := ParseConstraints(opts.Config.Perturber.Constraints)
	if err != nil {
		return nil, err
	}

	sched := &Schedule{Constraints: constraints, Faults: []PlannedFault{}, Events: []Event{}}
	if len(opts.Planned) == 0 {
		return sched, nil
	}

	budget := opts.Config.Perturber.Budget
	if budget.MaxFaultsPerWorld < 1 || budget.MaxConcurrentFaults < 1 {
		return nil, fmt.Errorf("perturber: perturber.budget is max_concurrent_faults=%d "+
			"max_faults_per_world=%d; both must be at least 1 (Config.Validate enforces this — "+
			"a zero budget must fail closed, not mean `unlimited`)",
			budget.MaxConcurrentFaults, budget.MaxFaultsPerWorld)
	}

	specs, err := parsePlanned(opts.Planned)
	if err != nil {
		return nil, err
	}

	if len(specs) > budget.MaxFaultsPerWorld {
		return nil, fmt.Errorf("%w: %d faults, but perturber.budget.max_faults_per_world is %d",
			ErrBudgetExceeded, len(specs), budget.MaxFaultsPerWorld)
	}

	if err := checkKindsPermitted(opts.Config, specs); err != nil {
		return nil, err
	}
	if opts.Registry != nil {
		if err := checkKindsInjectable(opts.Registry, specs); err != nil {
			return nil, err
		}
	}

	// Canonical ordering: (start_ms, end_ms, canonical string). Fault ids are
	// assigned in this order, so a schedule's ids do not depend on the order the
	// author happened to write it in, and two runs of the same world tag their
	// iptables rules identically.
	sort.SliceStable(specs, func(i, j int) bool {
		if specs[i].StartMS != specs[j].StartMS {
			return specs[i].StartMS < specs[j].StartMS
		}
		if specs[i].EndMS != specs[j].EndMS {
			return specs[i].EndMS < specs[j].EndMS
		}
		return specs[i].String() < specs[j].String()
	})

	occurrences := map[string]int{}
	for i, spec := range specs {
		canon := spec.String()
		pf := PlannedFault{
			ID:         faultID(i),
			Spec:       spec,
			Canonical:  canon,
			Occurrence: occurrences[canon],
		}
		occurrences[canon]++
		sched.Faults = append(sched.Faults, pf)
	}

	// Static target resolution. A target that matches no node is refused here.
	if err := checkTargets(sched, opts.Topology, constraints); err != nil {
		return nil, err
	}

	sched.Events = buildEvents(sched.Faults)
	peak, err := checkConcurrency(sched, budget.MaxConcurrentFaults)
	if err != nil {
		return nil, err
	}
	sched.PeakConcurrent = peak
	return sched, nil
}

// faultID renders the schedule-local fault id.
func faultID(i int) string { return fmt.Sprintf("f%03d", i+1) }

// parsePlanned parses every entry, reporting all failures at once, and refuses a
// window that starts before the virtual clock exists.
func parsePlanned(planned []string) ([]schema.FaultSpec, error) {
	specs := make([]schema.FaultSpec, 0, len(planned))
	var errs []error
	for i, s := range planned {
		spec, err := schema.ParseFault(s)
		if err != nil {
			errs = append(errs, fmt.Errorf("fault_schedule.planned[%d]: %w", i, err))
			continue
		}
		if spec.StartMS < 0 {
			// The virtual clock's origin IS DRIVE start, so a negative offset
			// names an instant at which virtual time does not exist. PERTURB is
			// contained in DRIVE and cannot reach backwards into SEED.
			errs = append(errs, fmt.Errorf("fault_schedule.planned[%d]: %s: window starts at %dms, "+
				"but fault windows are relative to DRIVE start and virtual time does not exist before it",
				i, spec.String(), spec.StartMS))
			continue
		}
		specs = append(specs, spec)
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return specs, nil
}

// checkKindsPermitted enforces perturber.allow and perturber.deny.
func checkKindsPermitted(cfg *schema.Config, specs []schema.FaultSpec) error {
	permitted := map[schema.FaultKind]bool{}
	for _, k := range cfg.Perturber.EffectiveFaultKinds() {
		permitted[k] = true
	}
	denied := map[schema.FaultKind]bool{}
	for _, k := range cfg.Perturber.Deny {
		denied[k] = true
	}
	allowListed := map[schema.FaultKind]bool{}
	for _, k := range cfg.Perturber.Allow {
		allowListed[k] = true
	}

	var errs []error
	reported := map[schema.FaultKind]bool{}
	for _, spec := range specs {
		if permitted[spec.Kind] || reported[spec.Kind] {
			continue
		}
		reported[spec.Kind] = true
		switch {
		case denied[spec.Kind]:
			errs = append(errs, fmt.Errorf("%w: %s is in perturber.deny", ErrKindNotAllowed, spec.Kind))
		case len(cfg.Perturber.Allow) > 0 && !allowListed[spec.Kind]:
			errs = append(errs, fmt.Errorf("%w: %s is not in perturber.allow (allowed: %s)",
				ErrKindNotAllowed, spec.Kind, joinKinds(cfg.Perturber.EffectiveFaultKinds())))
		default:
			errs = append(errs, fmt.Errorf("%w: %s", ErrKindNotAllowed, spec.Kind))
		}
	}
	return errors.Join(errs...)
}

// checkKindsInjectable refuses a schedule naming a kind no injector serves, or
// one this PLATFORM cannot deliver.
//
// Same rule as the zero-node target: a fault with no mechanism would be a
// silent no-op, and a world reported as perturbed when nothing happened to it is
// how a run reports PASS over an untested system.
//
// The two checks answer different questions and both belong here. A registry
// lookup asks "does this build have an injector for this kind?"; PlatformCapability
// asks "can this HOST deliver it at all?", and its own documentation says it
// exists "so a schedule can be rejected before a world is booted and driven",
// which only happens if something calls it on the compile path. Nothing did.
//
// D-049 wired the same table into `internal/search/engine` so the Saboteur and
// the random baseline draw from a space with the undeliverable kinds removed.
// That covered the SEARCH path and left every other one: a hand-authored
// schedule, `thesis run --fault`, and a shrink candidate all reached PERTURB and
// failed there. Measured on run r_2026_09_09_42db, a 14-fault world naming
// io.latency spent a compose project, a bridge network and ~39 seconds to learn
// what this table lookup knows, and returned INCONCLUSIVE. See OQ-049.
func checkKindsInjectable(reg *Registry, specs []schema.FaultSpec) error {
	var errs []error
	reported := map[schema.FaultKind]bool{}
	for _, spec := range specs {
		if reported[spec.Kind] {
			continue
		}
		reported[spec.Kind] = true
		if err := kindInjectable(reg, spec.Kind); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// checkTargets resolves every statically resolvable target and applies the
// constraints that can be decided before dynamic binding.
func checkTargets(sched *Schedule, top *Topology, constraints []Constraint) error {
	res, err := NewResolver(top, nil)
	if err != nil {
		return err
	}
	var errs []error
	for _, f := range sched.Faults {
		nodes, exact, err := staticNodes(res, top, f.Spec.Target)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", f.Canonical, err))
			continue
		}
		if nodes == nil {
			// A role: target. It binds only against live state, so the executor
			// checks it, and errors, at injection time.
			continue
		}
		if err := CheckCompileTime(constraints, f.Spec, nodes, exact, top); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// staticNodes resolves a target as far as the topology alone allows.
//
// It returns (nil, false, nil) for a `role:` target: nothing about it is known
// before injection, and inventing an upper bound would refuse legal schedules.
// The second result reports whether the node IDENTITIES are exact; for a quorum
// target the COUNT is exact but the identities are not chosen until the fault's
// PRNG sub-stream is drawn from at injection time.
func staticNodes(res *Resolver, top *Topology, t schema.Target) ([]Node, bool, error) {
	switch t.Kind {
	case schema.TargetRole:
		return nil, false, nil
	case schema.TargetQuorum:
		group := top.Service(t.Scope)
		if len(group) == 0 {
			return nil, false, noMatch(t, "no node has service %q (have: %s)", t.Scope, servicesText(top))
		}
		k, err := QuorumSize(t.Func, len(group), t.N)
		if err != nil {
			return nil, false, resolveFailed(t, err, "%s", err.Error())
		}
		if k <= 0 {
			return nil, false, noMatch(t, "%s of a %d-node group is %d nodes", t.Func, len(group), k)
		}
		if k > len(group) {
			return nil, false, noMatch(t, "needs %d nodes but service %q has only %d", k, t.Scope, len(group))
		}
		// Identities are not chosen here: every node of the group carries the
		// same service, so the prefix is an exact stand-in for the COUNT and for
		// nothing else.
		return group[:k:k], false, nil
	default:
		r, err := res.ResolveStatic(t, nil)
		if err != nil {
			return nil, false, err
		}
		return r.Nodes, true, nil
	}
}

// buildEvents produces the total order.
//
// # Ordering, and the one line that needs defending
//
// Events sort by (at_ms, kind, fault index), and WITHDRAW SORTS BEFORE INJECT at
// the same instant. Fault windows are half-open (a fault is active over
// [start, end) and withdrawn at end) so a schedule that hands one fault off to
// another at a single millisecond must not transiently hold both. Ordering
// inject first would make such a handoff count as two concurrent faults and
// could refuse a schedule that never exceeds the budget for any positive
// duration.
//
// The fault index is the final tiebreak, and it is stable because ids were
// assigned in canonical (start, end, string) order.
func buildEvents(faults []PlannedFault) []Event {
	events := make([]Event, 0, 2*len(faults))
	for i, f := range faults {
		events = append(events,
			Event{Kind: EventInject, AtMS: f.Spec.StartMS, Fault: i},
			// Every fault gets a withdraw event, INCLUDING an instantaneous kind
			// such as proc.kill, whose Withdraw is a no-op. Uniform bookkeeping is
			// what lets HEAL have exactly one notion of "still active", and a
			// no-op withdraw that still asserts no residue is cheaper than a
			// second code path.
			Event{Kind: EventWithdraw, AtMS: f.Spec.EndMS, Fault: i},
		)
	}
	sort.SliceStable(events, func(i, j int) bool {
		a, b := events[i], events[j]
		if a.AtMS != b.AtMS {
			return a.AtMS < b.AtMS
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind // EventWithdraw == 0 sorts first
		}
		return a.Fault < b.Fault
	})
	for i := range events {
		events[i].Seq = i
	}
	return events
}

// checkConcurrency walks the event list and enforces
// perturber.budget.max_concurrent_faults.
//
// It runs on the COMPILED order rather than on interval arithmetic, so what is
// checked is exactly what the executor will do.
func checkConcurrency(sched *Schedule, maxConcurrent int) (int, error) {
	active := map[int]bool{}
	peak := 0
	for _, ev := range sched.Events {
		if ev.Kind == EventWithdraw {
			delete(active, ev.Fault)
			continue
		}
		active[ev.Fault] = true
		if len(active) > peak {
			peak = len(active)
		}
		if len(active) > maxConcurrent {
			ids := make([]string, 0, len(active))
			for i := range active {
				ids = append(ids, sched.Faults[i].Canonical)
			}
			sort.Strings(ids)
			return 0, fmt.Errorf("%w: %d faults are active at t+%dms (%s), "+
				"but perturber.budget.max_concurrent_faults is %d",
				ErrBudgetExceeded, len(active), ev.AtMS, strings.Join(ids, "; "), maxConcurrent)
		}
	}
	return peak, nil
}
