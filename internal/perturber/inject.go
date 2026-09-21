package perturber

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// ---------------------------------------------------------------------------
// The contract between this package and internal/perturber/faults
// ---------------------------------------------------------------------------

// Injector is one fault family's mechanism.
//
// The three methods are ONE contract, not three optional capabilities.
// Directive 4.3's Critical Guarantee is that "every fault must implement a safe
// withdraw() mechanism" and that "HEAL must verify that no residual network
// rules or stopped processes persist". Making VerifyClean part of the interface
// rather than an optional extra is deliberate: an injector that could be
// registered without one would let a family be added whose residue nobody ever
// checks, and the failure would surface as a mysteriously poisoned world several
// rollouts later.
//
// Implementations live in internal/perturber/faults. This package never
// constructs one; it is handed a Registry.
type Injector interface {
	// Kinds reports the fault kinds this injector serves. Registering two
	// injectors for one kind is a programming error, not a fallback chain.
	Kinds() []schema.FaultKind

	// Inject applies the fault. It must be atomic in effect: on error, it must
	// leave nothing behind, because the executor treats an injection error as a
	// failure of the whole schedule and will not call Withdraw for a fault that
	// never reported success.
	//
	// A partition MUST be bidirectional (D-026): a single INPUT drop is a
	// one-way drop, and the cluster does not see a symmetric partition.
	Inject(ctx context.Context, req InjectRequest) error

	// Withdraw removes the fault. It must be IDEMPOTENT (the executor withdraws
	// on the normal path, again on cancellation, and again at HEAL) and it must
	// match on the OWNERSHIP TAG rather than reconstruct the rule spec, because
	// a reconstructed spec fails after a partial removal and then HEAL cannot
	// clean up after its own partial failure (D-026).
	//
	// For an instantaneous kind (proc.kill, proc.restart, clock.jump) there is
	// nothing to remove and Withdraw is a no-op that returns nil. It is still
	// called, so the executor's bookkeeping has one shape.
	Withdraw(ctx context.Context, req WithdrawRequest) error

	// VerifyClean reports every residue still attributable to this run.
	//
	// It must compare against the snapshot taken at BOOT, never against an
	// assumed-empty table: under the requirement to work on unmodified systems a
	// target may legitimately ship its own iptables rules, and an assumed-empty
	// check is then a false-failure generator (D-026, demonstrated).
	//
	// Returning an empty slice and a nil error is the ONLY way to report clean.
	// An error means verification could not be carried out, which HEAL treats as
	// unverified: not as clean.
	VerifyClean(ctx context.Context, req VerifyRequest) ([]Residue, error)
}

// InjectRequest is one concrete injection.
type InjectRequest struct {
	// RunID and FaultID are the ownership tag. Every rule an injector installs
	// carries `thesis:<run_id>:<fault_id>` (as an iptables comment, a qdisc
	// handle, a container label or whatever the mechanism affords) so that
	// withdrawal and residual verification both match on ownership rather than
	// on a reconstructed rule spec (D-026).
	RunID   string
	FaultID string

	// Spec is the planned fault, parameters and window included.
	Spec schema.FaultSpec

	// Nodes is the WHOLE resolved set, ID-sorted, and it is a set rather than a
	// list of independent victims. net.partition over {n1, n2} isolates that
	// pair as a group: n1 and n2 still reach each other, and only traffic to the
	// complement is dropped. Injecting per node instead would additionally cut
	// n1<->n2, which is a different fault.
	Nodes []Node

	// Peers is every node NOT in Nodes. A partition needs it to know what to cut
	// the target set off FROM, and computing it here means one definition of
	// "the rest of the cluster" rather than one per family.
	Peers []Node

	// Seed is this fault's deterministic seed, drawn from the recorder's
	// path-keyed PRNG. netem accepts and faithfully stores an explicit seed on
	// this kernel (measured: asked 11111, reports 11111), so net.loss,
	// net.reorder and net.duplicate MUST pass it or their per-packet decisions
	// are unreproducible and invariant I2 is lost for three kinds (D-025).
	Seed uint64

	// AtMS is the virtual-clock millisecond at which the executor reached this
	// injection. It need not equal Spec.StartMS: what the world records is when
	// the injection actually landed.
	AtMS int64

	// Ownership is the fully formed tag, `thesis:<run_id>:<fault_id>`.
	Ownership string
}

// WithdrawRequest is the counterpart of InjectRequest.
//
// It carries the same identity and the same node set so that an injector can be
// stateless: withdrawal never depends on in-process bookkeeping that a crashed
// or restarted harness would have lost.
type WithdrawRequest struct {
	RunID     string
	FaultID   string
	Spec      schema.FaultSpec
	Nodes     []Node
	Peers     []Node
	AtMS      int64
	Ownership string
	// AtHeal reports whether this withdrawal is HEAL sweeping up rather than the
	// schedule reaching a window's end. It is informational; behaviour must not
	// differ, because a withdrawal that only works on the happy path is not a
	// withdrawal.
	AtHeal bool
}

// VerifyRequest is HEAL's residual check.
type VerifyRequest struct {
	// RunID scopes the check to this run's ownership tag.
	RunID string
	// Nodes is every node in the topology, not merely the ones this world
	// targeted: a residue from an earlier world is exactly what this check
	// exists to catch.
	Nodes []Node
	// FaultIDs are the ids injected in this world, so a verifier can name the
	// fault a residue came from.
	FaultIDs []string
	// OwnershipPrefix is `thesis:<run_id>:`. HEAL greps for it.
	OwnershipPrefix string
}

// Residue is one piece of fault state that outlived HEAL.
type Residue struct {
	// NodeID is where it was found.
	NodeID string
	// Mechanism names the primitive, e.g. "iptables", "tc-qdisc", "sigstop",
	// "cgroup-cpu-quota", "time-namespace". It is an open string: the set of
	// mechanisms is a property of the families, not of this package.
	Mechanism string
	// FaultID is the owning fault when the tag identified one, else "".
	FaultID string
	// Detail is the verbatim evidence (the rule line, the qdisc, the container
	// state) so a report names what to remove rather than that something was
	// wrong.
	Detail string
}

func (r Residue) String() string {
	id := r.FaultID
	if id == "" {
		id = "unattributed"
	}
	return fmt.Sprintf("%s on %s (%s): %s", r.Mechanism, r.NodeID, id, r.Detail)
}

// OwnershipTag is the per-fault ownership tag: `thesis:<run_id>:<fault_id>`.
//
// D-026 fixes this spelling. Every injected rule carries it, withdrawal matches
// on it, and HEAL greps for its prefix.
func OwnershipTag(runID, faultID string) string {
	return "thesis:" + runID + ":" + faultID
}

// OwnershipPrefix is the run-scoped prefix HEAL greps for.
func OwnershipPrefix(runID string) string { return "thesis:" + runID + ":" }

// ---------------------------------------------------------------------------
// Registry
// ---------------------------------------------------------------------------

// ErrNoInjector reports a fault kind with no registered mechanism.
//
// This is a hard failure, and it is the same anti-gaming rule as the zero-node
// target: a schedule naming a kind nobody can inject must be refused, because
// running it would report a world as perturbed when nothing happened to it.
var ErrNoInjector = errors.New("perturber: no injector is registered for this fault kind")

// PlatformCapable is implemented by an injector that can tell, WITHOUT touching
// Docker, that a kind it otherwise serves cannot be delivered on this host.
//
// It is an optional interface rather than a method on Injector because most
// mechanisms have nothing to say: tc, iptables and docker kill work wherever a
// container does. The io.* family does not (delaying or failing filesystem
// operations needs a shim beneath a running container's overlay2 mount) and it
// knows that statically.
//
// The interface lives HERE and is implemented THERE because internal/perturber
// cannot import internal/perturber/faults: faults imports perturber, so the
// dependency only runs one way. Inverting it through an interface is what lets
// the compile-time check consult a table only the faults package can hold.
type PlatformCapable interface {
	// Capability returns nil when k can be injected on this host, and the reason
	// it cannot otherwise. The reason must name a remedy: it is what a user sees
	// instead of a wasted world.
	Capability(k schema.FaultKind) error
}

// Registry maps a fault kind to the injector that serves it.
type Registry struct {
	injectors []Injector
	byKind    map[schema.FaultKind]int
}

// NewRegistry indexes injectors by kind.
//
// An empty registry is legal (a world with no faults needs no mechanism) but
// NewExecutor then refuses any schedule that has events.
func NewRegistry(injectors ...Injector) (*Registry, error) {
	r := &Registry{byKind: map[schema.FaultKind]int{}}
	for _, inj := range injectors {
		if inj == nil {
			return nil, errors.New("perturber: NewRegistry given a nil injector")
		}
		idx := len(r.injectors)
		r.injectors = append(r.injectors, inj)
		kinds := inj.Kinds()
		if len(kinds) == 0 {
			return nil, fmt.Errorf("perturber: injector %T declares no fault kinds", inj)
		}
		for _, k := range kinds {
			if !k.Valid() {
				return nil, fmt.Errorf("perturber: injector %T declares unknown fault kind %q", inj, k)
			}
			if prev, dup := r.byKind[k]; dup {
				return nil, fmt.Errorf("perturber: fault kind %s is served by both %T and %T; "+
					"one kind has exactly one mechanism", k, r.injectors[prev], inj)
			}
			r.byKind[k] = idx
		}
	}
	return r, nil
}

// For returns the injector serving k.
func (r *Registry) For(k schema.FaultKind) (Injector, error) {
	if r == nil {
		return nil, fmt.Errorf("%w: %s (no registry was supplied)", ErrNoInjector, k)
	}
	i, ok := r.byKind[k]
	if !ok {
		return nil, fmt.Errorf("%w: %s (registered: %s)", ErrNoInjector, k, joinKinds(r.Kinds()))
	}
	return r.injectors[i], nil
}

// Injectors returns the registered injectors in registration order, each once.
func (r *Registry) Injectors() []Injector {
	if r == nil {
		return nil
	}
	out := make([]Injector, len(r.injectors))
	copy(out, r.injectors)
	return out
}

// Kinds lists every served kind, sorted.
func (r *Registry) Kinds() []schema.FaultKind {
	if r == nil {
		return nil
	}
	out := make([]schema.FaultKind, 0, len(r.byKind))
	for k := range r.byKind {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func joinKinds(ks []schema.FaultKind) string {
	if len(ks) == 0 {
		return "none"
	}
	parts := make([]string, len(ks))
	for i, k := range ks {
		parts[i] = string(k)
	}
	return strings.Join(parts, ", ")
}
