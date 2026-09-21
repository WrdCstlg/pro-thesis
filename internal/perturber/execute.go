package perturber

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/WrdCstlg/pro-thesis/internal/recorder"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// ---------------------------------------------------------------------------
// Execution
//
// The executor walks the compiled event list on the VIRTUAL clock during
// PERTURB, which overlaps DRIVE. It is a SINGLE goroutine by construction: one
// sequencing loop, sleeping to each event's deadline in turn. Firing a goroutine
// per event would destroy the very property the compiled total order exists to
// give (that inject ordering is a function of the schedule and not of the Go
// scheduler) and would race teardown, because a fault whose injection lands
// after HEAL has begun is a leak nothing withdraws.
// ---------------------------------------------------------------------------

// DefaultWithdrawTimeout bounds one withdrawal on the cancellation path.
const DefaultWithdrawTimeout = 15 * time.Second

// ErrResidual reports fault state that survived HEAL.
//
// It is a HARNESS ERROR (exit 2), never an oracle violation. The distinction is
// not bookkeeping: a residual fault means the run could not be CONDUCTED
// properly, so it has no verdict to give about the system under test, and the
// residue will poison every world that follows it on this host.
var ErrResidual = errors.New("perturber: residual fault state survived HEAL")

// ErrUnverified reports that HEAL could not establish that nothing was left
// behind. Unverified is not clean.
var ErrUnverified = errors.New("perturber: HEAL could not verify zero residual fault state")

// ResidualError carries what HEAL found still in place.
type ResidualError struct {
	Residues []Residue
}

func (e *ResidualError) Error() string {
	parts := make([]string, 0, len(e.Residues))
	for _, r := range e.Residues {
		parts = append(parts, r.String())
	}
	return fmt.Sprintf("%v: %d residue(s): %s", ErrResidual, len(e.Residues), strings.Join(parts, "; "))
}

func (e *ResidualError) Unwrap() error { return ErrResidual }

// Injection is the full record of one fault's life: what it bound to, when it
// landed, and whether each half of it worked.
//
// It is richer than schema.RealizedFault on purpose. The world file records what
// WAS injected; this records what was ATTEMPTED, including the failures, because
// "the target resolved to nothing" and "the fault ran and did nothing" have to
// be distinguishable after the fact.
type Injection struct {
	FaultID string
	Fault   PlannedFault

	// Resolution is the binding used, including the live role observation when
	// one was taken.
	Resolution Resolution
	// Nodes is Resolution.Nodes, flattened for convenience.
	Nodes []Node
	// Seed is the deterministic seed handed to the injector (D-025).
	Seed uint64

	// InjectedAtMS is the virtual millisecond at which injection was attempted.
	InjectedAtMS int64
	// InjectOK reports whether the injector accepted it.
	InjectOK  bool
	InjectErr error

	// Withdrawn reports whether withdrawal was attempted at all.
	Withdrawn     bool
	WithdrawnAtMS int64
	WithdrawOK    bool
	WithdrawErr   error
	// WithdrawnAtHeal reports whether the last withdrawal attempt was HEAL
	// sweeping up rather than the schedule reaching the window end.
	WithdrawnAtHeal bool
}

// Realized projects a successful injection onto the world file's shape.
//
// # Why Resolved is sometimes just the planned string
//
// schema.RealizedFault.Resolved must itself parse as a fault, and the frozen
// TARGET grammar offers node, wildcard, role, edge and quorum forms: none of
// which can name an ARBITRARY set of nodes. So a resolution to a single node is
// pinned (`proc.pause(role:leader)` becomes `proc.pause(kv-n2)`, which is the
// case that matters, because leadership moves and a role target is the one form
// that does not replay from the seed) and an edge is already concrete, while a
// multi-node wildcard or quorum keeps its planned target and lets Nodes carry
// the binding. Nodes is the ground truth in every case; see the open question
// recorded against this package.
//
// EndMS is the LAST WITHDRAWAL ATTEMPT, which for a withdrawal that failed is
// an upper bound rather than the instant the fault stopped acting. That case is
// already a harness error (HEAL reports the residue and the world has no
// verdict to give) and the exact truth, including WithdrawOK, stays available
// in Executor.Injections.
func (i Injection) Realized() schema.RealizedFault {
	end := i.InjectedAtMS
	if i.Withdrawn {
		end = i.WithdrawnAtMS
	}
	if end < i.InjectedAtMS {
		end = i.InjectedAtMS
	}
	ids := nodeIDs(i.Nodes)
	if ids == nil {
		ids = []string{}
	}
	return schema.RealizedFault{
		Fault:    i.Fault.Canonical,
		Resolved: resolvedFaultString(i.Fault.Spec, i.Nodes),
		Nodes:    ids,
		StartMS:  i.InjectedAtMS,
		EndMS:    end,
	}
}

// resolvedFaultString binds a fault's target to concrete nodes as far as the
// frozen grammar allows.
func resolvedFaultString(spec schema.FaultSpec, nodes []Node) string {
	if len(nodes) == 1 && spec.Target.Kind != schema.TargetEdge {
		pinned := spec
		pinned.Target = schema.Target{Kind: schema.TargetNode, Node: nodes[0].ID}
		return pinned.String()
	}
	return spec.String()
}

// Result is one PERTURB phase's outcome.
type Result struct {
	// Injections is every attempt, in the order attempted.
	Injections []Injection
	// Realized is the successful injections in world-file shape.
	Realized []schema.RealizedFault
	// Canceled reports that the schedule was cut short by context cancellation
	// rather than run to its end.
	Canceled bool
}

// HealReport is what HEAL did and found.
type HealReport struct {
	// Withdrawn names the faults HEAL had to withdraw itself: normally none,
	// because the schedule withdraws at each window end. A non-empty list after
	// an uncancelled run means a withdrawal failed earlier and was retried.
	Withdrawn []string
	// Residues is what survived. Any entry is a harness error.
	Residues []Residue
	// Verified reports that EVERY registered injector completed its residual
	// check. False means the check could not be carried out, which is not the
	// same as clean and is never treated as clean.
	Verified bool
}

// ExecutorOptions configures an executor.
type ExecutorOptions struct {
	// RunID is half of the ownership tag every injected rule carries. Required:
	// an untagged rule cannot be withdrawn by tag or verified by prefix (D-026).
	RunID string
	// Schedule is the compiled event list. Required.
	Schedule *Schedule
	// Topology is the live view. Required.
	Topology *Topology
	// Resolver binds targets. Optional; defaults to a resolver with no role
	// observer, which makes a `role:` target an error rather than a guess.
	Resolver *Resolver
	// Registry supplies the mechanisms. Required for a non-empty schedule.
	Registry *Registry
	// Clock and Timeline are the recorder's, so PERTURB shares one time frame
	// with the phase markers, the history and the telemetry.
	Clock    recorder.Clock
	Timeline *recorder.Timeline
	// Streams is the world's PRNG registry. Optional; built from Seed when nil.
	Streams *recorder.Streams
	// Seed is the world seed, used only when Streams is nil.
	Seed recorder.Seed
	// WithdrawTimeout bounds each withdrawal. Zero means
	// DefaultWithdrawTimeout.
	WithdrawTimeout time.Duration
	// Log receives progress lines. Optional.
	Log func(format string, args ...any)
}

// Executor drives a compiled schedule.
type Executor struct {
	runID    string
	sched    *Schedule
	top      *Topology
	resolver *Resolver
	reg      *Registry
	clock    recorder.Clock
	timeline *recorder.Timeline
	streams  *recorder.Streams
	wto      time.Duration
	log      func(string, ...any)

	mu         sync.Mutex
	injections []*Injection
	byFault    map[string]*Injection
	activeIDs  []string // fault ids still active, in injection order
	ran        bool
}

// NewExecutor validates the wiring and returns an executor.
//
// It refuses a schedule whose kinds have no mechanism. That check belongs here
// rather than at the first injection: discovering mid-DRIVE that net.partition
// has no injector means the world has already been half-perturbed, and reporting
// it as executed would be a claim about a system nobody tested.
func NewExecutor(opts ExecutorOptions) (*Executor, error) {
	if opts.Schedule == nil {
		return nil, errors.New("perturber: NewExecutor needs a compiled schedule")
	}
	if opts.Topology == nil {
		return nil, errors.New("perturber: NewExecutor needs a topology")
	}
	if opts.Clock == nil {
		return nil, errors.New("perturber: NewExecutor needs a clock")
	}
	if opts.Timeline == nil {
		return nil, errors.New("perturber: NewExecutor needs a timeline")
	}
	if !opts.Timeline.Started() {
		// PERTURB is contained in DRIVE and the virtual origin IS DRIVE start,
		// so an executor built before DRIVE has no frame to schedule against.
		return nil, fmt.Errorf("perturber: PERTURB scheduled before DRIVE: %w", recorder.ErrNoOrigin)
	}
	if !opts.Schedule.Empty() && strings.TrimSpace(opts.RunID) == "" {
		return nil, errors.New("perturber: NewExecutor needs a run id; it is half of the " +
			"`thesis:<run_id>:<fault_id>` ownership tag that makes withdrawal and residual " +
			"verification match on ownership rather than on a reconstructed rule spec")
	}

	res := opts.Resolver
	if res == nil {
		var err error
		res, err = NewResolver(opts.Topology, nil)
		if err != nil {
			return nil, err
		}
	}

	reg := opts.Registry
	if !opts.Schedule.Empty() {
		if reg == nil {
			return nil, fmt.Errorf("%w: the schedule has %d fault(s) but no injector registry was supplied",
				ErrNoInjector, opts.Schedule.Len())
		}
		if err := checkScheduleInjectable(reg, opts.Schedule); err != nil {
			return nil, err
		}
	}

	streams := opts.Streams
	if streams == nil {
		streams = recorder.NewStreams(opts.Seed)
	}

	wto := opts.WithdrawTimeout
	if wto <= 0 {
		wto = DefaultWithdrawTimeout
	}

	return &Executor{
		runID:    opts.RunID,
		sched:    opts.Schedule,
		top:      opts.Topology,
		resolver: res,
		reg:      reg,
		clock:    opts.Clock,
		timeline: opts.Timeline,
		streams:  streams,
		wto:      wto,
		log:      opts.Log,
		byFault:  map[string]*Injection{},
	}, nil
}

// checkScheduleInjectable is the executor-side half of the same gate Compile
// applies, and it must ask the same two questions.
//
// Both exist because the two run at different times: Compile may be handed no
// registry at all (injectors are rebuilt per world, since container ids change
// every world; D-032), so the executor is the first point on the live path where
// a registry is guaranteed. A check on only one of them is a check a real run can
// route around.
func checkScheduleInjectable(reg *Registry, sched *Schedule) error {
	var errs []error
	for _, k := range sched.Kinds() {
		if err := kindInjectable(reg, k); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// kindInjectable answers both questions the two gates share: is there a mechanism
// for this kind, and can this host deliver it?
func kindInjectable(reg *Registry, k schema.FaultKind) error {
	inj, err := reg.For(k)
	if err != nil {
		// No mechanism at all subsumes "no mechanism on this host": reporting both
		// would name the same kind twice with two different remedies.
		return err
	}
	if pc, ok := inj.(PlatformCapable); ok {
		return pc.Capability(k)
	}
	return nil
}

// Schedule returns the compiled schedule.
func (e *Executor) Schedule() *Schedule { return e.sched }

// Run walks the event list on the virtual clock.
//
// It returns when the last event has been processed, or earlier on cancellation
// or on an error it cannot continue past. In every case it leaves nothing
// injected that it can withdraw: cancellation and injection failure both run
// withdrawal of the whole active set before returning, on a context detached
// from the cancelled one. HEAL runs afterwards regardless and sweeps again.
func (e *Executor) Run(ctx context.Context) (Result, error) {
	e.mu.Lock()
	if e.ran {
		e.mu.Unlock()
		return Result{}, errors.New("perturber: Run called twice on one executor")
	}
	e.ran = true
	e.mu.Unlock()

	if e.sched.Empty() {
		return e.snapshot(false), nil
	}

	var errs []error
	for _, ev := range e.sched.Events {
		at, err := e.timeline.R(recorder.VMS(ev.AtMS))
		if err != nil {
			errs = append(errs, fmt.Errorf("perturber: event %d at t+%dms: %w", ev.Seq, ev.AtMS, err))
			errs = append(errs, e.withdrawAllErr(ctx, false))
			return e.snapshot(false), errors.Join(errs...)
		}
		if _, err := e.clock.SleepUntilR(ctx, at); err != nil {
			// Cancelled, or the clock was retired underneath us. Either way the
			// schedule stops here and everything still injected comes out.
			e.logf("perturber: schedule interrupted at t+%dms: %v", ev.AtMS, err)
			errs = append(errs, err, e.withdrawAllErr(ctx, false))
			return e.snapshot(true), errors.Join(errs...)
		}

		switch ev.Kind {
		case EventInject:
			if err := e.inject(ctx, ev); err != nil {
				// An injection that failed means the schedule was not conducted
				// as written. Continuing would produce a world that reports more
				// perturbation than it received, so it stops here: after taking
				// out everything already applied.
				errs = append(errs, err, e.withdrawAllErr(ctx, false))
				return e.snapshot(false), errors.Join(errs...)
			}
		case EventWithdraw:
			if err := e.withdrawFault(ctx, ev.Fault, false); err != nil {
				// One stuck withdrawal must not strand the others: the loop
				// carries on, the fault stays marked active, and HEAL retries it.
				errs = append(errs, err)
			}
		}
	}
	return e.snapshot(false), errors.Join(errs...)
}

// Heal withdraws whatever is still active and then VERIFIES that nothing is
// left behind.
//
// It is idempotent and is safe to call after a cancelled Run, after a failed
// Run, and after a Run that never happened. Directive 4.3 makes the verification
// half mandatory ("HEAL must verify that no residual network rules or stopped
// processes persist"), and the check compares against the injectors' own BOOT
// baseline rather than an assumed-empty table: a target may legitimately ship
// its own firewall rules, and an assumed-empty check is then a false-failure
// generator (D-026).
func (e *Executor) Heal(ctx context.Context) (HealReport, error) {
	var (
		rep  HealReport
		errs []error
	)

	before := e.activeFaultIDs()
	if err := e.withdrawAllErr(ctx, true); err != nil {
		errs = append(errs, err)
	}
	rep.Withdrawn = before

	residues, verified, verr := e.verifyClean(ctx)
	rep.Residues = residues
	rep.Verified = verified
	if verr != nil {
		errs = append(errs, fmt.Errorf("%w: %v", ErrUnverified, verr))
	}
	if len(residues) > 0 {
		errs = append(errs, &ResidualError{Residues: residues})
	}
	if !verified && e.injectedAny() {
		errs = append(errs, fmt.Errorf("%w: %d fault(s) were injected in this world but no injector "+
			"completed a residual check; unverified is not clean", ErrUnverified, e.injectedCount()))
	}
	return rep, errors.Join(errs...)
}

// Realized returns the world file's fault_schedule.realized, as it stands now.
//
// Call it AFTER Heal: a fault withdrawn by HEAL has its end_ms filled in only
// once the withdrawal has happened.
func (e *Executor) Realized() []schema.RealizedFault {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]schema.RealizedFault, 0, len(e.injections))
	for _, in := range e.injections {
		if !in.InjectOK {
			continue
		}
		out = append(out, in.Realized())
	}
	return out
}

// Injections returns every attempt, successes and failures alike.
func (e *Executor) Injections() []Injection {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]Injection, 0, len(e.injections))
	for _, in := range e.injections {
		out = append(out, *in)
	}
	return out
}

// ---------------------------------------------------------------------------
// internals
// ---------------------------------------------------------------------------

func (e *Executor) inject(ctx context.Context, ev Event) error {
	f := e.sched.Faults[ev.Fault]
	rec := &Injection{FaultID: f.ID, Fault: f}

	fail := func(err error) error {
		rec.InjectErr = err
		rec.InjectedAtMS = e.nowMS()
		e.record(rec)
		return err
	}

	res, err := e.resolver.Resolve(ctx, f.Spec.Target, e.substream(f, "target"))
	if err != nil {
		// A target that resolves to nothing is an error, never a no-op. See
		// ErrNoMatch.
		return fail(fmt.Errorf("perturber: %s [%s]: %w", f.Canonical, f.ID, err))
	}
	rec.Resolution = res
	rec.Nodes = res.Nodes

	if err := CheckResolved(e.sched.Constraints, f.Spec, res.Nodes, e.top); err != nil {
		return fail(fmt.Errorf("perturber: %s [%s]: %w", f.Canonical, f.ID, err))
	}

	inj, err := e.reg.For(f.Spec.Kind)
	if err != nil {
		return fail(fmt.Errorf("perturber: %s [%s]: %w", f.Canonical, f.ID, err))
	}

	rec.Seed = e.substream(f, "seed").Uint64()
	rec.InjectedAtMS = e.nowMS()

	req := InjectRequest{
		RunID:     e.runID,
		FaultID:   f.ID,
		Spec:      f.Spec,
		Nodes:     res.Nodes,
		Peers:     e.peersOf(res.Nodes),
		Seed:      rec.Seed,
		AtMS:      rec.InjectedAtMS,
		Ownership: OwnershipTag(e.runID, f.ID),
	}
	if err := inj.Inject(ctx, req); err != nil {
		return fail(fmt.Errorf("perturber: inject %s [%s] on %s: %w",
			f.Canonical, f.ID, strings.Join(res.IDs(), ","), err))
	}

	rec.InjectOK = true
	e.record(rec)
	e.markActive(rec)
	e.logf("perturber: t+%dms inject %s [%s] -> %s", rec.InjectedAtMS, f.Canonical, f.ID,
		strings.Join(res.IDs(), ","))
	return nil
}

// withdrawFault withdraws one fault if it is still active.
//
// A fault whose withdrawal FAILS stays marked active, so HEAL tries again. That
// is the whole point of running withdrawal three times over: a leaked iptables
// rule or a SIGSTOPped container poisons every subsequent world on this host.
func (e *Executor) withdrawFault(ctx context.Context, faultIdx int, atHeal bool) error {
	f := e.sched.Faults[faultIdx]
	return e.withdrawByID(ctx, f.ID, atHeal)
}

func (e *Executor) withdrawByID(ctx context.Context, faultID string, atHeal bool) error {
	e.mu.Lock()
	rec, active := e.byFault[faultID]
	if active {
		active = containsString(e.activeIDs, faultID)
	}
	e.mu.Unlock()
	if !active || rec == nil {
		// Never injected, or already out. Both are fine: withdrawal is
		// idempotent by contract.
		return nil
	}

	inj, err := e.reg.For(rec.Fault.Spec.Kind)
	if err != nil {
		return fmt.Errorf("perturber: withdraw %s [%s]: %w", rec.Fault.Canonical, rec.FaultID, err)
	}

	wctx, cancel := e.withdrawContext(ctx)
	defer cancel()

	req := WithdrawRequest{
		RunID:     e.runID,
		FaultID:   rec.FaultID,
		Spec:      rec.Fault.Spec,
		Nodes:     rec.Nodes,
		Peers:     e.peersOf(rec.Nodes),
		AtMS:      e.nowMS(),
		Ownership: OwnershipTag(e.runID, rec.FaultID),
		AtHeal:    atHeal,
	}
	werr := inj.Withdraw(wctx, req)

	e.mu.Lock()
	rec.Withdrawn = true
	rec.WithdrawnAtMS = req.AtMS
	rec.WithdrawOK = werr == nil
	rec.WithdrawErr = werr
	rec.WithdrawnAtHeal = atHeal
	if werr == nil {
		e.activeIDs = removeString(e.activeIDs, faultID)
	}
	e.mu.Unlock()

	if werr != nil {
		return fmt.Errorf("perturber: withdraw %s [%s] on %s: %w",
			rec.Fault.Canonical, rec.FaultID, strings.Join(nodeIDs(rec.Nodes), ","), werr)
	}
	e.logf("perturber: t+%dms withdraw %s [%s]", req.AtMS, rec.Fault.Canonical, rec.FaultID)
	return nil
}

// withdrawAllErr withdraws every still-active fault, newest first.
//
// LIFO because faults compose: a netem qdisc installed on top of a partition
// comes off before the partition does, in the reverse order it went on.
func (e *Executor) withdrawAllErr(ctx context.Context, atHeal bool) error {
	ids := e.activeFaultIDs()
	var errs []error
	for i := len(ids) - 1; i >= 0; i-- {
		if err := e.withdrawByID(ctx, ids[i], atHeal); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// verifyClean runs every registered injector's residual check.
//
// Every injector is asked, not only the ones this world used: a residue from an
// earlier world on the same host is exactly what this check exists to catch, and
// asking only the families that fired would make the check blind to precisely
// the leak that matters.
func (e *Executor) verifyClean(ctx context.Context) ([]Residue, bool, error) {
	injectors := e.reg.Injectors()
	if len(injectors) == 0 {
		return nil, false, nil
	}
	req := VerifyRequest{
		RunID:           e.runID,
		Nodes:           e.top.Nodes(),
		FaultIDs:        e.injectedFaultIDs(),
		OwnershipPrefix: OwnershipPrefix(e.runID),
	}
	var (
		all  []Residue
		errs []error
	)
	for _, inj := range injectors {
		res, err := inj.VerifyClean(ctx, req)
		if err != nil {
			errs = append(errs, fmt.Errorf("%T: %w", inj, err))
			continue
		}
		all = append(all, res...)
	}
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].NodeID != all[j].NodeID {
			return all[i].NodeID < all[j].NodeID
		}
		if all[i].Mechanism != all[j].Mechanism {
			return all[i].Mechanism < all[j].Mechanism
		}
		return all[i].Detail < all[j].Detail
	})
	if len(errs) > 0 {
		return all, false, errors.Join(errs...)
	}
	return all, true, nil
}

// withdrawContext bounds a withdrawal, and detaches it from a context that has
// already been cancelled.
//
// Withdrawal on the cancellation path MUST NOT inherit the cancelled context:
// the whole reason it runs there is that the caller went away, and a withdrawal
// that returns ctx.Canceled without touching anything is how a leaked rule
// happens.
func (e *Executor) withdrawContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil || ctx.Err() != nil {
		return context.WithTimeout(context.Background(), e.wto)
	}
	return context.WithTimeout(ctx, e.wto)
}

func (e *Executor) record(rec *Injection) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.injections = append(e.injections, rec)
	e.byFault[rec.FaultID] = rec
}

func (e *Executor) markActive(rec *Injection) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !containsString(e.activeIDs, rec.FaultID) {
		e.activeIDs = append(e.activeIDs, rec.FaultID)
	}
}

func (e *Executor) activeFaultIDs() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]string, len(e.activeIDs))
	copy(out, e.activeIDs)
	return out
}

func (e *Executor) injectedFaultIDs() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]string, 0, len(e.injections))
	for _, in := range e.injections {
		if in.InjectOK {
			out = append(out, in.FaultID)
		}
	}
	return out
}

func (e *Executor) injectedCount() int { return len(e.injectedFaultIDs()) }

func (e *Executor) injectedAny() bool { return e.injectedCount() > 0 }

func (e *Executor) snapshot(canceled bool) Result {
	return Result{
		Injections: e.Injections(),
		Realized:   e.Realized(),
		Canceled:   canceled,
	}
}

// nowMS is the current virtual-clock millisecond. NewExecutor has already
// established that the origin exists, so the error branch is unreachable; it
// reports 0 rather than panicking because a bad timestamp must not be able to
// abort a withdrawal.
func (e *Executor) nowMS() int64 {
	v, err := e.timeline.V(e.clock.NowR())
	if err != nil {
		return 0
	}
	return v.MS()
}

// peersOf returns every topology node outside the target set.
func (e *Executor) peersOf(nodes []Node) []Node {
	in := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		in[n.ID] = true
	}
	all := e.top.Nodes()
	out := make([]Node, 0, len(all))
	for _, n := range all {
		if !in[n.ID] {
			out = append(out, n)
		}
	}
	return out
}

// substream returns this fault's own PRNG sub-stream for a given purpose.
//
// The index is a hash of the fault's canonical string, its occurrence among
// identical strings, and the purpose, NOT its position in the schedule. That is
// what makes Phase 5 shrinking converge: deleting one fault from a schedule must
// not change the randomness of any other, or ddmin would be minimizing against a
// moving target (see recorder.Stream.Index). Distinct purposes get distinct
// sub-streams, so adding a third draw later cannot perturb either of the first
// two.
func (e *Executor) substream(f PlannedFault, purpose string) *recorder.Stream {
	base := e.streams.Get(recorder.StreamFaultSchedule)
	return base.Index(substreamIndex(f.Canonical, f.Occurrence, purpose))
}

func substreamIndex(canonical string, occurrence int, purpose string) uint64 {
	h := sha256.New()
	h.Write([]byte(canonical))
	h.Write([]byte{0})
	h.Write([]byte(strconv.Itoa(occurrence)))
	h.Write([]byte{0})
	h.Write([]byte(purpose))
	return binary.BigEndian.Uint64(h.Sum(nil)[:8])
}

func (e *Executor) logf(format string, args ...any) {
	if e.log == nil {
		return
	}
	e.log(format, args...)
}

func containsString(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

func removeString(ss []string, s string) []string {
	out := ss[:0]
	for _, x := range ss {
		if x == s {
			continue
		}
		out = append(out, x)
	}
	return out
}
