// This file carries the PROCESS family (proc.pause, proc.kill, proc.restart,
// proc.slow) together with the small amount of surface the process, clock and
// I/O families share.
//
// Two properties govern everything in these three files and are not negotiable:
//
//  1. EVERY fault implements Withdraw; Withdraw is IDEMPOTENT, safe after a
//     partial or failed Inject, driven by OBSERVED state rather than in-process
//     bookkeeping, and never bails on the first target. A leaked SIGSTOP or a
//     leaked CPU quota poisons every subsequent world, and the leak is triggered
//     by exactly the paths that already went wrong once.
//  2. A fault that cannot be injected in this environment returns an error
//     wrapping ErrUnsupported, which the control plane maps to exit 2
//     INCONCLUSIVE. It NEVER silently succeeds. A world whose faults never fired
//     reporting PASS is the worst output this tool can produce.
package faults

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/WrdCstlg/pro-thesis/internal/oracle"
	"github.com/WrdCstlg/pro-thesis/internal/perturber"
	"github.com/WrdCstlg/pro-thesis/internal/recorder"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// ===========================================================================
// SHARED SURFACE OF THE PROCESS / CLOCK / I/O FAMILIES
//
// The net.* family and the NET_ADMIN sidecar are written by a second author
// into their own files in this package. Everything below is either exported
// with a name those files do not use, or unexported with a deliberately
// distinctive `dk` prefix, so the two file sets coexist without a collision.
// ===========================================================================

// ErrUnsupported is the sentinel every "this cannot be done here" error wraps.
//
// The control plane must map it to schema.ExitInconclusive (exit 2): the config
// was valid and the system was fine, the ENVIRONMENT could not carry out the
// fault. It must never be folded into a pass.
var ErrUnsupported = errors.New("perturber: fault is not injectable in this environment")

// Unsupportedf builds an ErrUnsupported-wrapping error naming the kind and the
// reason. The reason is written for a human who has to fix it.
func Unsupportedf(kind schema.FaultKind, format string, a ...any) error {
	return fmt.Errorf("%s: %s: %w", kind, fmt.Sprintf(format, a...), ErrUnsupported)
}

// IsUnsupported reports whether err came from a kind this environment cannot
// inject, as opposed to a genuine injection failure.
func IsUnsupported(err error) bool { return errors.Is(err, ErrUnsupported) }

// ExitCodeFor maps an injection error onto the normative CLI exit space.
//
// Both arms are exit 2. They are kept distinct because the DIAGNOSIS differs: an
// unsupported kind is a capability gap that recurs on every run until the
// environment changes, while an injection failure may be transient.
func ExitCodeFor(err error) schema.ExitCode {
	if err == nil {
		return schema.ExitPass
	}
	return schema.ExitInconclusive
}

// Target is one concrete, ALREADY-RESOLVED node a fault acts on.
//
// Resolution of the grammar's dynamic target forms (role:leader, minority(kv),
// kv:*) happens above this package, at injection time, from live cluster state;
// that is the whole reason the world file splits planned from realized (D-012).
// A primitive here is handed the answer, never the question.
type Target struct {
	// NodeID is the logical node id from prothesis.yaml.
	NodeID string
	// ContainerID is the container backing the node RIGHT NOW. A compose
	// recreate changes it, so callers re-read it from Primitive.Targets().
	ContainerID string
	// ComposeService is the PHYSICAL compose service, never the logical group.
	ComposeService string
	// Group is the logical service group the grammar targets.
	Group string
	// ClientPort is the client port INSIDE the container.
	ClientPort int64
}

// TargetsFromNodes projects the resolver's node set onto this package's target
// shape, rejecting a node with no container rather than injecting into nothing.
func TargetsFromNodes(nodes []perturber.Node) ([]Target, error) {
	out := make([]Target, 0, len(nodes))
	for _, n := range nodes {
		if n.ContainerID == "" {
			return nil, fmt.Errorf("faults: node %s has no bound container; a fault against nothing "+
				"that reported success would make a world look perturbed when it was not", n.ID)
		}
		port := n.ContainerPort
		if port <= 0 {
			port = int64(schema.DefaultClientPort)
		}
		out = append(out, Target{
			NodeID:         n.ID,
			ContainerID:    n.ContainerID,
			ComposeService: n.ComposeService,
			Group:          n.Service,
			ClientPort:     port,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].NodeID < out[j].NodeID })
	return out, nil
}

// Env is the execution environment the process, clock and I/O families need.
type Env struct {
	// RunID is half the ownership tag `thesis:<run_id>:<fault_id>` D-026
	// requires, so residue can be found by owner rather than by reconstructing
	// what was applied.
	RunID string
	// DockerBin overrides the docker executable. Empty means "docker".
	DockerBin string
	// ProjectDir is the directory compose is invoked from.
	ProjectDir string
	// Topology is the `up` -> `down` handoff record. Required only by the clock
	// family, which must recreate a container through compose.
	Topology *recorder.Topology
	// WorkDir is where generated compose fragments are written. Empty means the
	// OS temp directory.
	WorkDir string
	// HelperImage is the image used by the one fault that needs a privileged
	// helper container (fd.exhaust). Empty means DefaultHelperImage.
	HelperImage string
	// Exec runs a command and returns its stdout. Nil means os/exec. It is the
	// seam unit tests substitute so the pure logic of every fault (argument
	// construction, parsing, withdrawal ordering, residual classification) is
	// testable without Docker.
	Exec func(ctx context.Context, dir, name string, args []string) (string, error)
}

func (e Env) docker() string {
	if e.DockerBin != "" {
		return e.DockerBin
	}
	return "docker"
}

// Tag is the ownership tag for one fault: `thesis:<run_id>:<fault_id>`.
//
// It is the same spelling perturber.OwnershipTag produces, and it is duplicated
// here only so a primitive can name its own residue without importing the
// executor's request types into every call site.
func (e Env) Tag(faultID string) string { return "thesis:" + e.RunID + ":" + faultID }

// Record is one PHYSICAL event a fault performed, for the realized schedule.
//
// Times are WALL-CLOCK nanoseconds because a primitive has no business knowing
// the virtual clock's origin; the caller converts through recorder.Timeline,
// which is the only thing that does.
//
// A fault may produce MORE records than the schedule has faults. A clock fault
// produces three (restart, skew, restart) because the kernel fixes a time
// namespace's offset at creation, so a clock fault is only implementable as a
// restart into a skewed namespace (OQ-020). Recording the restarts is what keeps
// the world file and the causal timeline honest about what physically happened,
// and it is also what keeps no_crash from reporting the perturber's own restart
// as a crash.
type Record struct {
	// Kind is the kind ACTUALLY performed, which is not always Spec().Kind.
	Kind schema.FaultKind
	// Planned is the canonical string of the planned fault this came from.
	Planned string
	// Resolved is the canonical string with the target bound to concrete nodes.
	Resolved string
	// Nodes are the concrete node ids the event hit.
	Nodes []string
	// StartWallNS and EndWallNS bound the event.
	StartWallNS int64
	EndWallNS   int64
	// Open reports that the fault was injected and never withdrawn, so EndWallNS
	// is the injection instant rather than a real end.
	//
	// It is an explicit flag rather than an EndWallNS == StartWallNS convention,
	// and the difference is not cosmetic: Windows' wall clock has a coarse tick,
	// so an injection and its withdrawal can land on the SAME nanosecond value.
	// A convention would then read a properly closed window as still open, and
	// the realized schedule would misreport every fast fault.
	Open bool
}

// RealizedFault converts a record into the world file's realized entry.
//
// A record that predates DRIVE, or a timeline with no origin, yields ok=false
// rather than a silently wrong offset: a fault misplaced at t+0 would make the
// causal timeline lie about ordering, which is the one thing it exists to
// establish.
func (r Record) RealizedFault(tl *recorder.Timeline) (schema.RealizedFault, bool) {
	if tl == nil {
		return schema.RealizedFault{}, false
	}
	start, err := tl.VMSFromEpochNS(r.StartWallNS)
	if err != nil {
		return schema.RealizedFault{}, false
	}
	end, err := tl.VMSFromEpochNS(r.EndWallNS)
	if err != nil {
		return schema.RealizedFault{}, false
	}
	return schema.RealizedFault{
		Fault:    r.Planned,
		Resolved: r.Resolved,
		Nodes:    append([]string(nil), r.Nodes...),
		StartMS:  start,
		EndMS:    end,
	}, true
}

// OracleFaultWindows converts realized records into the windows the no_crash
// oracle reads as oracle.Input.PlannedFaults.
//
// THIS IS THE SEAM THAT KEEPS A DELIBERATE KILL FROM READING AS A CRASH.
// no_crash reports any process exit outside a planned fault window as a
// violation, and it matches on CONCRETE NODE IDS: an empty node list excuses
// nothing, by design, so a fault whose binding was not recorded cannot silence a
// real crash. A proc.kill, a proc.restart, or either of the restarts a clock
// fault entails, that fails to reach this list is reported as a crash the
// perturber itself caused.
//
// The conversion is from the REALIZED records, never from the planned schedule:
// role:leader binds at injection time and the planned string does not say to
// what.
func OracleFaultWindows(recs []Record, tl *recorder.Timeline) []oracle.FaultWindow {
	out := make([]oracle.FaultWindow, 0, len(recs))
	for _, r := range recs {
		rf, ok := r.RealizedFault(tl)
		if !ok {
			continue
		}
		out = append(out, oracle.FaultWindow{
			Kind:    string(r.Kind),
			Target:  r.Resolved,
			Nodes:   append([]string(nil), r.Nodes...),
			StartMS: rf.StartMS,
			EndMS:   rf.EndMS,
		})
	}
	return out
}

// Primitive is one injected, withdrawable disturbance of the process, clock or
// I/O families.
//
// It is named Primitive rather than Fault because the net.* family in this
// package already owns the name Fault for its own concrete type. The two are
// separate mechanisms behind one perturber.Injector contract.
type Primitive interface {
	// Kind is the fault kind this implements.
	Kind() schema.FaultKind
	// Spec is the parsed planned fault.
	Spec() schema.FaultSpec
	// FaultID is the per-fault ownership id.
	FaultID() string
	// Targets are the concrete nodes, re-read after any recreate.
	Targets() []Target
	// Inject applies the fault. An error wrapping ErrUnsupported means this
	// environment cannot carry the kind out at all.
	Inject(ctx context.Context) error
	// Withdraw removes it. It is idempotent, decides what to undo from OBSERVED
	// state rather than from what this process remembers doing, and attempts
	// EVERY target before returning an aggregate error.
	Withdraw(ctx context.Context) error
	// Active reports whether the fault is currently applied.
	Active() bool
	// Records are the physical events performed so far.
	Records() []Record
}

// PrimitiveRequest is the input to every primitive constructor in these three
// families.
type PrimitiveRequest struct {
	Env     Env
	Spec    schema.FaultSpec
	FaultID string
	Targets []Target
}

func (r PrimitiveRequest) validate() error {
	if r.FaultID == "" {
		return fmt.Errorf("%s: a fault id is required; it is half the ownership tag that "+
			"withdrawal and residual verification match on", r.Spec.Kind)
	}
	if len(r.Targets) == 0 {
		return fmt.Errorf("%s: no concrete targets; %s resolved to nothing, and injecting nothing "+
			"while reporting success is how a world with no faults reports PASS",
			r.Spec.Kind, r.Spec.Target)
	}
	for i, t := range r.Targets {
		if t.NodeID == "" {
			return fmt.Errorf("%s: target %d has no node id", r.Spec.Kind, i)
		}
		if t.ContainerID == "" {
			return fmt.Errorf("%s: target %s has no container id", r.Spec.Kind, t.NodeID)
		}
	}
	return nil
}

// WithdrawPrimitives withdraws every primitive, in reverse injection order,
// attempting all of them and returning an aggregate error.
//
// Reverse order matters: a clock fault recreates a container, so withdrawing it
// before an unrelated pause on the same node would try to unpause a container
// that no longer exists. Attempting ALL of them matters more: HEAL exists to
// clean up after a partial failure, so a HEAL that stops at the first error is
// the one thing it must not be.
func WithdrawPrimitives(ctx context.Context, ps []Primitive) error {
	var errs []error
	for i := len(ps) - 1; i >= 0; i-- {
		if ps[i] == nil {
			continue
		}
		if err := ps[i].Withdraw(ctx); err != nil {
			errs = append(errs, fmt.Errorf("withdraw %s: %w", ps[i].Spec(), err))
		}
	}
	return errors.Join(errs...)
}

// NewProcessFault builds the primitive for one proc.* kind.
func NewProcessFault(req PrimitiveRequest) (Primitive, error) {
	if err := req.validate(); err != nil {
		return nil, err
	}
	switch req.Spec.Kind {
	case schema.FaultProcPause:
		return &ProcPause{base: newBase(req)}, nil
	case schema.FaultProcKill:
		sig, err := dkSignalArg(req.Spec)
		if err != nil {
			return nil, err
		}
		return &ProcKill{base: newBase(req), signal: sig}, nil
	case schema.FaultProcRestart:
		return &ProcRestart{base: newBase(req)}, nil
	case schema.FaultProcSlow:
		pct, err := dkFloatParam(req.Spec, "cpu_pct")
		if err != nil {
			return nil, err
		}
		if pct <= 0 {
			return nil, fmt.Errorf("%s: cpu_pct=%g is not a throttle, it is a stop; use proc.pause "+
				"for that", req.Spec.Kind, pct)
		}
		return &ProcSlow{base: newBase(req), cpuPct: pct, baseline: map[string]dkCPUState{}}, nil
	}
	return nil, fmt.Errorf("faults: %q is not a process-family kind", req.Spec.Kind)
}

// ---------------------------------------------------------------------------
// base: shared bookkeeping
// ---------------------------------------------------------------------------

type base struct {
	env     Env
	spec    schema.FaultSpec
	faultID string

	mu       sync.Mutex
	targets  []Target
	applied  map[string]bool
	recs     []Record
	injected bool
}

func newBase(req PrimitiveRequest) base {
	return base{
		env:     req.Env,
		spec:    req.Spec,
		faultID: req.FaultID,
		targets: append([]Target(nil), req.Targets...),
		applied: map[string]bool{},
	}
}

func (b *base) Kind() schema.FaultKind { return b.spec.Kind }
func (b *base) Spec() schema.FaultSpec { return b.spec }
func (b *base) FaultID() string        { return b.faultID }

func (b *base) Targets() []Target {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]Target(nil), b.targets...)
}

func (b *base) Active() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, on := range b.applied {
		if on {
			return true
		}
	}
	return false
}

func (b *base) Records() []Record {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]Record(nil), b.recs...)
}

// note appends a CLOSED record: an instantaneous event, or one whose window is
// known at the time it is recorded.
func (b *base) note(kind schema.FaultKind, nodes []string, startNS, endNS int64) {
	b.appendRec(kind, nodes, dkResolved(b.spec, kind, nodes), startNS, endNS, false)
}

// noteOpen appends a record whose end is stamped later by closeWindow.
func (b *base) noteOpen(kind schema.FaultKind, nodes []string, startNS int64) {
	b.appendRec(kind, nodes, dkResolved(b.spec, kind, nodes), startNS, startNS, true)
}

// noteOpenResolved is noteOpen with an explicitly supplied resolved string, for
// a fault whose realized parameters differ from its planned ones.
func (b *base) noteOpenResolved(kind schema.FaultKind, nodes []string, resolved string, startNS int64) {
	b.appendRec(kind, nodes, resolved, startNS, startNS, true)
}

func (b *base) appendRec(kind schema.FaultKind, nodes []string, resolved string, startNS, endNS int64, open bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.recs = append(b.recs, Record{
		Kind:        kind,
		Planned:     b.spec.String(),
		Resolved:    resolved,
		Nodes:       append([]string(nil), nodes...),
		StartWallNS: startNS,
		EndWallNS:   endNS,
		Open:        open,
	})
}

// closeWindow stamps the end of the most recent OPEN record of this kind.
//
// endNS is floored at the record's start: a coarse wall clock can report a
// withdrawal marginally earlier than the injection it follows, and a window that
// ends before it begins would be rejected by schema.FaultSpec.Validate and would
// make the causal timeline non-monotonic.
func (b *base) closeWindow(kind schema.FaultKind, endNS int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for i := len(b.recs) - 1; i >= 0; i-- {
		if b.recs[i].Kind == kind && b.recs[i].Open {
			if endNS < b.recs[i].StartWallNS {
				endNS = b.recs[i].StartWallNS
			}
			b.recs[i].EndWallNS = endNS
			b.recs[i].Open = false
			return
		}
	}
}

func (b *base) mark(nodeID string, on bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.applied[nodeID] = on
}

func (b *base) isApplied(nodeID string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.applied[nodeID]
}

func (b *base) setInjected(v bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.injected = v
}

// rebind points a target at the container currently backing its node.
//
// Only the container id moves. The published host port is deliberately NOT
// re-read here: it belongs to recorder.Topology, which the caller owns, and a
// primitive silently holding a second copy of it is how the two drift apart. A
// caller that recreates containers should re-read Targets() and update its own
// binding.
func (b *base) rebind(nodeID, containerID string) {
	if containerID == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for i := range b.targets {
		if b.targets[i].NodeID == nodeID {
			b.targets[i].ContainerID = containerID
		}
	}
}

// dkResolved renders the canonical fault string with the target replaced by the
// concrete nodes, preserving the planned parameters and window.
//
// A kind that differs from the planned kind (the restarts a clock fault entails)
// drops the planned parameters, because clock.skew's `ms` is not a parameter
// proc.restart declares and the result would not parse back.
func dkResolved(spec schema.FaultSpec, kind schema.FaultKind, nodes []string) string {
	var b strings.Builder
	b.WriteString(string(kind))
	b.WriteByte('(')
	b.WriteString(strings.Join(nodes, "+"))
	if kind == spec.Kind {
		for _, p := range spec.Params {
			b.WriteString(", ")
			b.WriteString(p.Name)
			b.WriteByte('=')
			b.WriteString(p.Value)
		}
	}
	b.WriteString(")@")
	b.WriteString(strconv.FormatInt(spec.StartMS, 10))
	b.WriteString("..")
	b.WriteString(strconv.FormatInt(spec.EndMS, 10))
	return b.String()
}

// ---------------------------------------------------------------------------
// proc.pause: SIGSTOP / SIGCONT
// ---------------------------------------------------------------------------

// ProcPause is `proc.pause`: SIGSTOP the whole container at the window start,
// SIGCONT at the window end.
//
// It is the most important kind in the system because it is the cheapest way to
// manufacture a GRAY failure: the container still exists, its ports are still
// published, docker still reports it, and it answers nothing. The node is alive
// to the orchestrator and dead to its peers, which is precisely the condition
// the KV fixture's stale read needs, and a condition a kill cannot produce.
//
// It is also the most dangerous to leak. A PAUSED CONTAINER CANNOT BE STOPPED:
// `compose down` blocks for the full stop timeout and then leaves it, leaking
// the container AND one of this host's ~24 free bridge networks permanently. So
// Withdraw unpauses whatever it FINDS paused rather than whatever this process
// remembers pausing, and VerifyClean re-checks that nothing is left paused
// before HEAL may pass.
type ProcPause struct{ base }

// Inject implements Primitive.
func (f *ProcPause) Inject(ctx context.Context) error {
	start := dkNowNS()
	var (
		errs []error
		hit  []string
	)
	for _, t := range f.Targets() {
		st, err := dkInspect(ctx, f.env, t.ContainerID)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: inspect %s: %w", t.NodeID, dkShortID(t.ContainerID), err))
			continue
		}
		if st.State.Paused {
			// Already paused, and not by us. Pausing again is a no-op, and
			// unpausing at withdrawal would resume something we did not stop.
			errs = append(errs, fmt.Errorf("%s: container %s was ALREADY paused before injection; "+
				"an earlier run leaked a SIGSTOP and this world is not clean",
				t.NodeID, dkShortID(t.ContainerID)))
			continue
		}
		if !st.State.Running {
			errs = append(errs, fmt.Errorf("%s: container %s is %s, so there is no process to stop",
				t.NodeID, dkShortID(t.ContainerID), st.State.Status))
			continue
		}
		if _, err := dkRun(ctx, f.env, "pause", t.ContainerID); err != nil {
			errs = append(errs, fmt.Errorf("%s: docker pause: %w", t.NodeID, err))
			continue
		}
		// Verify rather than assume: `docker pause` exiting 0 is the CLI's
		// opinion, .State.Paused is the daemon's.
		if st2, err := dkInspect(ctx, f.env, t.ContainerID); err != nil || !st2.State.Paused {
			errs = append(errs, fmt.Errorf("%s: docker pause reported success but the container is "+
				"not paused", t.NodeID))
			continue
		}
		f.mark(t.NodeID, true)
		hit = append(hit, t.NodeID)
	}
	if len(hit) > 0 {
		f.setInjected(true)
		// The window stays open until withdrawal stamps it. An open window that
		// defaulted to "forever" would excuse every later process exit.
		f.noteOpen(schema.FaultProcPause, hit, start)
	}
	return errors.Join(errs...)
}

// Withdraw implements Primitive.
func (f *ProcPause) Withdraw(ctx context.Context) error {
	end := dkNowNS()
	var errs []error
	unpaused := false
	for _, t := range f.Targets() {
		st, err := dkInspect(ctx, f.env, t.ContainerID)
		if err != nil {
			// A container that no longer exists cannot be paused. But one we
			// DID pause and can no longer inspect is a genuine problem: it
			// cannot be shown to have been resumed.
			if f.isApplied(t.NodeID) {
				errs = append(errs, fmt.Errorf("%s: paused container %s cannot be inspected, so it "+
					"cannot be shown to have been resumed: %w",
					t.NodeID, dkShortID(t.ContainerID), err))
			}
			continue
		}
		if !st.State.Paused {
			f.mark(t.NodeID, false)
			continue
		}
		if _, err := dkRun(ctx, f.env, "unpause", t.ContainerID); err != nil {
			errs = append(errs, fmt.Errorf("%s: docker unpause: %w (a paused container cannot be "+
				"stopped, so this leaks the container and its network)", t.NodeID, err))
			continue
		}
		if st2, err := dkInspect(ctx, f.env, t.ContainerID); err != nil || st2.State.Paused {
			errs = append(errs, fmt.Errorf("%s: docker unpause reported success but the container is "+
				"still paused", t.NodeID))
			continue
		}
		f.mark(t.NodeID, false)
		unpaused = true
	}
	if unpaused {
		f.closeWindow(schema.FaultProcPause, end)
	}
	f.setInjected(false)
	return errors.Join(errs...)
}

// ---------------------------------------------------------------------------
// proc.kill: one signal, once
// ---------------------------------------------------------------------------

// ProcKill is `proc.kill(signal)`: deliver a signal once, at the window start.
//
// The kind is INSTANTANEOUS but its window is not decorative. no_crash reports
// any process exit OUTSIDE a planned fault window as a violation, so the window
// is exactly the period in which the resulting exit is expected. The record this
// fault emits therefore spans the full planned window and names the concrete
// node. Fail to route that record into oracle.Input.PlannedFaults and the
// perturber reports its own deliberate kill as a crash it discovered.
type ProcKill struct {
	base
	signal string
}

// Signal is the signal as it will be handed to `docker kill --signal`.
func (f *ProcKill) Signal() string { return f.signal }

// Inject implements Primitive.
func (f *ProcKill) Inject(ctx context.Context) error {
	start := dkNowNS()
	var (
		errs []error
		hit  []string
	)
	for _, t := range f.Targets() {
		st, err := dkInspect(ctx, f.env, t.ContainerID)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: inspect %s: %w", t.NodeID, dkShortID(t.ContainerID), err))
			continue
		}
		if st.State.Paused {
			// A SIGSTOPped process queues signals but acts on none of them
			// until it is resumed. SIGKILL still lands, but the exit would be
			// observed at an unrelated time, so the causal ordering would lie.
			errs = append(errs, fmt.Errorf("%s: container %s is paused; signal delivery to a "+
				"SIGSTOPped process is not observable until it is resumed",
				t.NodeID, dkShortID(t.ContainerID)))
			continue
		}
		if !st.State.Running {
			errs = append(errs, fmt.Errorf("%s: container %s is %s, so there is no process to signal",
				t.NodeID, dkShortID(t.ContainerID), st.State.Status))
			continue
		}
		if _, err := dkRun(ctx, f.env, "kill", "--signal", f.signal, t.ContainerID); err != nil {
			errs = append(errs, fmt.Errorf("%s: docker kill --signal %s: %w", t.NodeID, f.signal, err))
			continue
		}
		hit = append(hit, t.NodeID)
	}
	if len(hit) > 0 {
		f.setInjected(true)
		f.note(schema.FaultProcKill, hit, start, start+f.spec.DurationMS()*int64(time.Millisecond))
	}
	return errors.Join(errs...)
}

// Withdraw implements Primitive.
//
// A delivered signal cannot be un-delivered, so there is nothing to undo. The
// method still exists, still returns nil, and is still called on every path:
// "every fault implements withdraw" with an exception is how residue starts
// getting left behind.
func (f *ProcKill) Withdraw(_ context.Context) error {
	f.setInjected(false)
	return nil
}

// ---------------------------------------------------------------------------
// proc.restart: stop then start
// ---------------------------------------------------------------------------

// ProcRestart is `proc.restart`: stop and start the target once.
//
// `docker stop` + `docker start` acts on the SAME container, so the id survives.
// The binding is nevertheless re-resolved from the node LABEL afterwards,
// because the clock family recreates containers through compose and every later
// fault addresses a node by label rather than by a remembered id. Exercising the
// rebinding path on the fault that does not need it is what keeps it working for
// the fault that does.
//
// It deliberately does not use `docker restart`, which collapses stop and start
// into one call whose failure mode is ambiguous. A container that fails to come
// back must be reported as a harness failure, not as a restart that "worked".
type ProcRestart struct {
	base
	// StopTimeout is the grace period handed to `docker stop`. Zero means
	// docker's default.
	StopTimeout time.Duration
}

// Inject implements Primitive.
func (f *ProcRestart) Inject(ctx context.Context) error {
	start := dkNowNS()
	var (
		errs []error
		hit  []string
	)
	for _, t := range f.Targets() {
		st, err := dkInspect(ctx, f.env, t.ContainerID)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: inspect %s: %w", t.NodeID, dkShortID(t.ContainerID), err))
			continue
		}
		if st.State.Paused {
			// A paused container cannot be stopped: the stop blocks for the
			// full grace period and then leaves it running.
			if _, err := dkRun(ctx, f.env, "unpause", t.ContainerID); err != nil {
				errs = append(errs, fmt.Errorf("%s: container is paused and cannot be unpaused, so "+
					"it cannot be stopped: %w", t.NodeID, err))
				continue
			}
		}
		stopArgs := []string{"stop"}
		if f.StopTimeout > 0 {
			stopArgs = append(stopArgs, "--timeout", strconv.Itoa(int(f.StopTimeout.Seconds())))
		}
		stopArgs = append(stopArgs, t.ContainerID)
		if _, err := dkRun(ctx, f.env, stopArgs...); err != nil {
			errs = append(errs, fmt.Errorf("%s: docker stop: %w", t.NodeID, err))
			continue
		}
		if _, err := dkRun(ctx, f.env, "start", t.ContainerID); err != nil {
			errs = append(errs, fmt.Errorf("%s: docker start after stop: %w (the node is now DOWN, "+
				"which is a harness failure and not a finding)", t.NodeID, err))
			continue
		}
		if st2, err := dkInspect(ctx, f.env, t.ContainerID); err != nil || !st2.State.Running {
			errs = append(errs, fmt.Errorf("%s: the container did not come back up after restart",
				t.NodeID))
			continue
		}
		if id, err := ResolveContainerByNode(ctx, f.env, t.NodeID); err == nil && id != "" {
			f.rebind(t.NodeID, id)
		}
		hit = append(hit, t.NodeID)
	}
	if len(hit) > 0 {
		f.setInjected(true)
		f.note(schema.FaultProcRestart, hit, start, start+f.spec.DurationMS()*int64(time.Millisecond))
	}
	return errors.Join(errs...)
}

// Withdraw implements Primitive. A completed restart leaves nothing applied.
func (f *ProcRestart) Withdraw(_ context.Context) error {
	f.setInjected(false)
	return nil
}

// ---------------------------------------------------------------------------
// proc.slow: CPU throttling
// ---------------------------------------------------------------------------

// ProcSlow is `proc.slow(cpu_pct)`: throttle the target to cpu_pct percent of
// ONE CPU.
//
// # Which docker knob, and why not the obvious one
//
// `docker update --cpus` is the obvious route and it does throttle, but IT
// CANNOT BE UNDONE, which disqualifies it as the default. Measured on this host:
//
//	docker update --cpus 0.25 c   ->  NanoCpus 250000000, cpu.max "25000 100000"
//	docker update --cpus 0    c   ->  NanoCpus STILL 250000000, cpu.max unchanged
//
// The daemon reads 0 as "no change", so a container that started unlimited can
// never be returned to unlimited through `--cpus`. That is a permanent throttle
// leaked into every subsequent world on that container.
//
// `--cpu-quota -1` DOES clear it; verified by reading /sys/fs/cgroup/cpu.max
// inside the container afterwards: "max 100000". It is only usable when NanoCpus
// is unset, because the daemon then refuses "CPU Period cannot be updated as
// NanoCPUs has already been set". So:
//
//   - NanoCpus == 0 (the normal case, and the fixture's): set --cpu-period and
//     --cpu-quota; withdraw to the recorded baseline, or to --cpu-quota -1 when
//     there was no baseline quota.
//   - NanoCpus != 0 (the compose file declared `cpus:`): use --cpus, and
//     withdraw by restoring the exact recorded NanoCpus, which the daemon does
//     honour because it is non-zero.
//
// Both branches have an exact inverse and both are verifiable through `docker
// inspect`, which is what the process injector's VerifyClean checks.
//
// One residue is accepted and stated rather than hidden: on the quota branch,
// withdrawing with `--cpu-quota -1` leaves HostConfig.CpuPeriod at the value we
// set where the baseline had 0. A period with no quota does not throttle (the
// cgroup reads "max 100000") so the residual check compares the quota and
// NanoCpus, not the period.
type ProcSlow struct {
	base
	cpuPct   float64
	baseline map[string]dkCPUState
	blMu     sync.Mutex
}

type dkCPUState struct {
	NanoCPUs  int64
	CPUQuota  int64
	CPUPeriod int64
	Known     bool
}

// defaultCPUPeriod is the kernel's default CFS period, in microseconds.
const defaultCPUPeriod int64 = 100000

// minCPUQuota is the smallest CFS quota the kernel accepts, in microseconds.
const minCPUQuota int64 = 1000

// SetBaseline seeds the CPU baseline from the BOOT snapshot, so a withdrawal
// works even in a process that did not perform the injection.
//
// The perturber.Injector contract requires withdrawal not to depend on
// in-process bookkeeping a crashed harness would have lost. The BOOT baseline is
// the artifact that replaces it, and it is the same artifact D-026 already
// requires HEAL to judge residue against.
func (f *ProcSlow) SetBaseline(bl ContainerBaseline) {
	f.blMu.Lock()
	defer f.blMu.Unlock()
	for _, n := range bl.Nodes {
		f.baseline[n.NodeID] = dkCPUState{
			NanoCPUs:  n.NanoCPUs,
			CPUQuota:  n.CPUQuota,
			CPUPeriod: n.CPUPeriod,
			Known:     true,
		}
	}
}

func (f *ProcSlow) getBaseline(nodeID string) (dkCPUState, bool) {
	f.blMu.Lock()
	defer f.blMu.Unlock()
	s, ok := f.baseline[nodeID]
	return s, ok && s.Known
}

func (f *ProcSlow) putBaseline(nodeID string, s dkCPUState) {
	f.blMu.Lock()
	defer f.blMu.Unlock()
	if prev, ok := f.baseline[nodeID]; ok && prev.Known {
		return // the BOOT snapshot wins over anything observed mid-run
	}
	s.Known = true
	f.baseline[nodeID] = s
}

// Inject implements Primitive.
func (f *ProcSlow) Inject(ctx context.Context) error {
	start := dkNowNS()
	var (
		errs []error
		hit  []string
	)
	for _, t := range f.Targets() {
		st, err := dkInspect(ctx, f.env, t.ContainerID)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: inspect %s: %w", t.NodeID, dkShortID(t.ContainerID), err))
			continue
		}
		f.putBaseline(t.NodeID, dkCPUState{
			NanoCPUs:  st.HostConfig.NanoCPUs,
			CPUQuota:  st.HostConfig.CPUQuota,
			CPUPeriod: st.HostConfig.CPUPeriod,
		})
		bl, _ := f.getBaseline(t.NodeID)

		args, err := f.injectArgs(bl, t.ContainerID)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if _, err := dkRun(ctx, f.env, args...); err != nil {
			errs = append(errs, fmt.Errorf("%s: docker update (cpu): %w", t.NodeID, err))
			continue
		}
		if st2, err := dkInspect(ctx, f.env, t.ContainerID); err == nil {
			if !dkQuotaActive(st2.HostConfig.CPUQuota) && st2.HostConfig.NanoCPUs == bl.NanoCPUs {
				errs = append(errs, fmt.Errorf("%s: docker update reported success but no CPU limit "+
					"is recorded on the container", t.NodeID))
				continue
			}
		}
		f.mark(t.NodeID, true)
		hit = append(hit, t.NodeID)
	}
	if len(hit) > 0 {
		f.setInjected(true)
		f.noteOpen(schema.FaultProcSlow, hit, start)
	}
	return errors.Join(errs...)
}

// injectArgs builds the `docker update` invocation for one target. It is pure,
// so the branch choice is unit-testable without Docker.
func (f *ProcSlow) injectArgs(bl dkCPUState, containerID string) ([]string, error) {
	if bl.NanoCPUs != 0 {
		return []string{"update", "--cpus",
			strconv.FormatFloat(f.cpuPct/100, 'f', -1, 64), containerID}, nil
	}
	period := bl.CPUPeriod
	if period <= 0 {
		period = defaultCPUPeriod
	}
	quota := int64(math.Round(f.cpuPct / 100 * float64(period)))
	if quota < minCPUQuota {
		return nil, Unsupportedf(f.spec.Kind,
			"cpu_pct=%g over a %dus period is a %dus quota, below the kernel minimum of %dus",
			f.cpuPct, period, quota, minCPUQuota)
	}
	return []string{"update",
		"--cpu-period", strconv.FormatInt(period, 10),
		"--cpu-quota", strconv.FormatInt(quota, 10),
		containerID}, nil
}

// withdrawArgs builds the `docker update` invocation that restores one target.
// Pure, for the same reason.
func withdrawCPUArgs(bl dkCPUState, containerID string) []string {
	switch {
	case bl.NanoCPUs != 0:
		return []string{"update", "--cpus",
			strconv.FormatFloat(float64(bl.NanoCPUs)/1e9, 'f', -1, 64), containerID}
	case bl.CPUQuota > 0:
		period := bl.CPUPeriod
		if period <= 0 {
			period = defaultCPUPeriod
		}
		return []string{"update",
			"--cpu-period", strconv.FormatInt(period, 10),
			"--cpu-quota", strconv.FormatInt(bl.CPUQuota, 10), containerID}
	default:
		// -1, never 0: the daemon reads 0 as "no change" and the throttle
		// would survive the withdrawal.
		return []string{"update", "--cpu-quota", "-1", containerID}
	}
}

// Withdraw implements Primitive.
//
// It decides from OBSERVED state, not from what this process remembers doing: a
// container whose live CPU limit already matches its baseline needs nothing,
// and one whose limit differs is restored whether or not this object injected
// it. That is what lets HEAL clean up after a crashed run.
func (f *ProcSlow) Withdraw(ctx context.Context) error {
	end := dkNowNS()
	var errs []error
	restored := false
	for _, t := range f.Targets() {
		st, err := dkInspect(ctx, f.env, t.ContainerID)
		if err != nil {
			if f.isApplied(t.NodeID) {
				errs = append(errs, fmt.Errorf("%s: throttled container %s cannot be inspected, so "+
					"the quota cannot be shown to have been restored: %w",
					t.NodeID, dkShortID(t.ContainerID), err))
			}
			continue
		}
		bl, known := f.getBaseline(t.NodeID)
		if !known {
			// No baseline at all. The only safe assumption is that the target
			// was unthrottled, because that is what an unmodified container is;
			// but say so, because it is an assumption and not a record.
			if !dkQuotaActive(st.HostConfig.CPUQuota) && st.HostConfig.NanoCPUs == 0 {
				f.mark(t.NodeID, false)
				continue
			}
			errs = append(errs, fmt.Errorf("%s: no CPU baseline was recorded, so the original quota "+
				"cannot be restored; the container still carries quota=%d nanocpus=%d",
				t.NodeID, st.HostConfig.CPUQuota, st.HostConfig.NanoCPUs))
			continue
		}
		if st.HostConfig.NanoCPUs == bl.NanoCPUs && !dkQuotaActive(st.HostConfig.CPUQuota) {
			f.mark(t.NodeID, false)
			continue
		}
		if _, err := dkRun(ctx, f.env, withdrawCPUArgs(bl, t.ContainerID)...); err != nil {
			errs = append(errs, fmt.Errorf("%s: restoring the CPU quota failed, so the throttle is "+
				"still in force: %w", t.NodeID, err))
			continue
		}
		f.mark(t.NodeID, false)
		restored = true
	}
	if restored {
		f.closeWindow(schema.FaultProcSlow, end)
	}
	f.setInjected(false)
	return errors.Join(errs...)
}

// ---------------------------------------------------------------------------
// BOOT baseline
// ---------------------------------------------------------------------------

// ContainerNodeBaseline is one node's container configuration at BOOT.
type ContainerNodeBaseline struct {
	NodeID      string `json:"node_id"`
	ContainerID string `json:"container_id"`
	Paused      bool   `json:"paused"`
	NanoCPUs    int64  `json:"nano_cpus"`
	CPUQuota    int64  `json:"cpu_quota"`
	CPUPeriod   int64  `json:"cpu_period"`
	Memory      int64  `json:"memory"`
	MemorySwap  int64  `json:"memory_swap"`
	// Entrypoint is the argv the container was created with, so a clock
	// withdrawal can prove the original one came back.
	Entrypoint []string `json:"entrypoint,omitempty"`
	// FDSoft and FDHard are PID 1's descriptor limits as /proc spells them,
	// captured only when a helper could read them. Empty means "not captured",
	// which is different from "unlimited".
	FDSoft string `json:"fd_soft,omitempty"`
	FDHard string `json:"fd_hard,omitempty"`
}

// ContainerBaseline is the BOOT-time snapshot the process and I/O families
// compare against.
//
// It compares against an OBSERVED baseline rather than an assumed-clean one for
// the same reason D-026 requires it of iptables: a target may legitimately ship
// its own CPU or memory limits, and an assumed-default check is either
// unimplementable or a false-failure generator on exactly the unmodified systems
// this tool claims to support.
//
// It is a serializable value, not in-process bookkeeping, because the
// perturber.Injector contract requires withdrawal to survive a harness that
// crashed and restarted.
type ContainerBaseline struct {
	Schema string                  `json:"schema"`
	RunID  string                  `json:"run_id"`
	Nodes  []ContainerNodeBaseline `json:"nodes"`
}

// ContainerBaselineSchema versions the snapshot artifact. ADDITIVE: the
// directive names no schema for it, and a baseline that cannot be version-gated
// cannot be read back by a later release.
const ContainerBaselineSchema = "prothesis.container_baseline/v1"

// ContainerBaselineFile is the bundle-relative path the snapshot is stored at.
const ContainerBaselineFile = "perturber/container_baseline.json"

// Node returns the baseline for a node id.
func (b ContainerBaseline) Node(nodeID string) (ContainerNodeBaseline, bool) {
	for _, n := range b.Nodes {
		if n.NodeID == nodeID {
			return n, true
		}
	}
	return ContainerNodeBaseline{}, false
}

// SnapshotContainerBaseline records every target's container configuration.
// Call it at BOOT, before any fault is injected.
//
// helperImage may be empty; when it is present and pullable the descriptor
// limits are captured too, which is what makes fd.exhaust withdrawable from a
// process that did not inject it. Failing to capture them is not an error: it
// narrows what can be restored, and the failure is reported at withdrawal where
// it is actionable.
func SnapshotContainerBaseline(ctx context.Context, env Env, targets []Target, helperImage string) (ContainerBaseline, error) {
	out := ContainerBaseline{Schema: ContainerBaselineSchema, RunID: env.RunID}
	var errs []error
	for _, t := range targets {
		st, err := dkInspect(ctx, env, t.ContainerID)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", t.NodeID, err))
			continue
		}
		n := ContainerNodeBaseline{
			NodeID:      t.NodeID,
			ContainerID: t.ContainerID,
			Paused:      st.State.Paused,
			NanoCPUs:    st.HostConfig.NanoCPUs,
			CPUQuota:    st.HostConfig.CPUQuota,
			CPUPeriod:   st.HostConfig.CPUPeriod,
			Memory:      st.HostConfig.Memory,
			MemorySwap:  st.HostConfig.MemorySwap,
			Entrypoint:  append([]string(nil), st.Config.Entrypoint...),
		}
		if helperImage != "" && st.State.Running && dkImageExists(ctx, env, helperImage) {
			if fd, err := readFDState(ctx, env, helperImage, t.ContainerID); err == nil {
				n.FDSoft, n.FDHard = fd.Soft, fd.Hard
			}
		}
		out.Nodes = append(out.Nodes, n)
	}
	sort.Slice(out.Nodes, func(i, j int) bool { return out.Nodes[i].NodeID < out.Nodes[j].NodeID })
	return out, errors.Join(errs...)
}

// ---------------------------------------------------------------------------
// perturber.Injector adapter for the process family
// ---------------------------------------------------------------------------

// ProcessInjector implements perturber.Injector for proc.pause, proc.kill,
// proc.restart and proc.slow.
type ProcessInjector struct {
	env      Env
	baseline ContainerBaseline

	mu   sync.Mutex
	live map[string]Primitive
	recs []Record
}

// NewProcessInjector builds the process family's injector.
//
// baseline is the BOOT snapshot. It may be zero, but a proc.slow withdrawal then
// has nothing to restore TO and will refuse rather than guess: see
// ProcSlow.Withdraw.
func NewProcessInjector(env Env, baseline ContainerBaseline) *ProcessInjector {
	return &ProcessInjector{env: env, baseline: baseline, live: map[string]Primitive{}}
}

// Kinds implements perturber.Injector.
func (in *ProcessInjector) Kinds() []schema.FaultKind {
	return []schema.FaultKind{
		schema.FaultProcPause,
		schema.FaultProcKill,
		schema.FaultProcRestart,
		schema.FaultProcSlow,
	}
}

// Records returns every physical event this injector performed, for the world's
// realized schedule and the causal timeline.
func (in *ProcessInjector) Records() []Record {
	in.mu.Lock()
	defer in.mu.Unlock()
	out := append([]Record(nil), in.recs...)
	for _, p := range in.live {
		out = append(out, p.Records()...)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].StartWallNS < out[j].StartWallNS })
	return out
}

// Inject implements perturber.Injector.
//
// It is atomic in effect: a partial injection is rolled back before the error is
// returned, because the executor treats an injection error as a failure of the
// whole schedule and will not call Withdraw for a fault that never reported
// success.
func (in *ProcessInjector) Inject(ctx context.Context, req perturber.InjectRequest) error {
	p, err := in.build(req.RunID, req.FaultID, req.Spec, req.Nodes)
	if err != nil {
		return err
	}
	if err := p.Inject(ctx); err != nil {
		// Roll back whatever landed. Ignoring the rollback's own error here
		// would hide a leak behind an injection failure, so it is joined on.
		if wErr := p.Withdraw(ctx); wErr != nil {
			return errors.Join(err, fmt.Errorf("rollback after a failed injection also failed: %w", wErr))
		}
		return err
	}
	in.mu.Lock()
	in.live[req.FaultID] = p
	in.mu.Unlock()
	return nil
}

// Withdraw implements perturber.Injector. It is idempotent, and it works from a
// fresh primitive when this process did not perform the injection.
func (in *ProcessInjector) Withdraw(ctx context.Context, req perturber.WithdrawRequest) error {
	in.mu.Lock()
	p, ok := in.live[req.FaultID]
	in.mu.Unlock()
	if !ok {
		var err error
		p, err = in.build(req.RunID, req.FaultID, req.Spec, req.Nodes)
		if err != nil {
			return err
		}
	}
	err := p.Withdraw(ctx)
	in.mu.Lock()
	in.recs = append(in.recs, p.Records()...)
	delete(in.live, req.FaultID)
	in.mu.Unlock()
	return err
}

// VerifyClean implements perturber.Injector.
//
// Returning an empty slice with a nil error is the ONLY way it reports clean. A
// node it could not inspect is reported as a residue of unknown content, never
// skipped: unverified is not clean.
func (in *ProcessInjector) VerifyClean(ctx context.Context, req perturber.VerifyRequest) ([]perturber.Residue, error) {
	targets, err := TargetsFromNodes(req.Nodes)
	if err != nil {
		return nil, err
	}
	var out []perturber.Residue
	for _, t := range targets {
		st, err := dkInspect(ctx, in.env, t.ContainerID)
		if err != nil {
			// A container that is genuinely gone holds no process residue: the
			// SIGSTOP died with the process and the cgroup went with it.
			if live, lerr := ResolveContainerByNode(ctx, in.env, t.NodeID); lerr != nil || live == "" {
				continue
			}
			out = append(out, perturber.Residue{
				NodeID:    t.NodeID,
				Mechanism: "inspect",
				Detail: fmt.Sprintf("container %s could not be inspected, so it was NOT verified "+
					"clean: %v", dkShortID(t.ContainerID), err),
			})
			continue
		}
		bl, known := in.baseline.Node(t.NodeID)
		if st.State.Paused && !bl.Paused {
			out = append(out, perturber.Residue{
				NodeID:    t.NodeID,
				Mechanism: "sigstop",
				Detail: fmt.Sprintf("container %s is still PAUSED; a paused container cannot be "+
					"stopped, so teardown will block for its full timeout and then leak the "+
					"container and one bridge network", dkShortID(t.ContainerID)),
			})
		}
		if known {
			if st.HostConfig.NanoCPUs != bl.NanoCPUs {
				out = append(out, perturber.Residue{
					NodeID:    t.NodeID,
					Mechanism: "cgroup-cpu-quota",
					Detail: fmt.Sprintf("NanoCpus is %d, baseline %d",
						st.HostConfig.NanoCPUs, bl.NanoCPUs),
				})
			}
			if dkQuotaActive(st.HostConfig.CPUQuota) && st.HostConfig.CPUQuota != bl.CPUQuota {
				out = append(out, perturber.Residue{
					NodeID:    t.NodeID,
					Mechanism: "cgroup-cpu-quota",
					Detail: fmt.Sprintf("CFS quota is %dus over a %dus period, baseline %dus",
						st.HostConfig.CPUQuota, st.HostConfig.CPUPeriod, bl.CPUQuota),
				})
			}
		} else if st.State.Paused || dkQuotaActive(st.HostConfig.CPUQuota) {
			out = append(out, perturber.Residue{
				NodeID:    t.NodeID,
				Mechanism: "cgroup-cpu-quota",
				Detail: fmt.Sprintf("no BOOT baseline was captured for this node, and it currently "+
					"reports paused=%v quota=%dus; nothing here can be shown to be pre-existing",
					st.State.Paused, st.HostConfig.CPUQuota),
			})
		}
	}
	return out, nil
}

func (in *ProcessInjector) build(runID, faultID string, spec schema.FaultSpec, nodes []perturber.Node) (Primitive, error) {
	targets, err := TargetsFromNodes(nodes)
	if err != nil {
		return nil, err
	}
	env := in.env
	if runID != "" {
		env.RunID = runID
	}
	p, err := NewProcessFault(PrimitiveRequest{
		Env: env, Spec: spec, FaultID: faultID, Targets: targets,
	})
	if err != nil {
		return nil, err
	}
	if slow, ok := p.(*ProcSlow); ok {
		slow.SetBaseline(in.baseline)
	}
	return p, nil
}

// ---------------------------------------------------------------------------
// docker plumbing
//
// Deliberately duplicated in miniature rather than shared with internal/harness:
// that package's runner is unexported, and importing the harness from the
// perturber would invert the dependency. The `dk` prefix keeps these clear of
// the sidecar runner a second author writes into this same package.
// ---------------------------------------------------------------------------

// dkInspectDoc is the slice of `docker inspect` these three families read.
type dkInspectDoc struct {
	ID    string `json:"Id"`
	Name  string `json:"Name"`
	State struct {
		Status   string `json:"Status"`
		Running  bool   `json:"Running"`
		Paused   bool   `json:"Paused"`
		ExitCode int    `json:"ExitCode"`
		Health   *struct {
			Status string `json:"Status"`
		} `json:"Health"`
	} `json:"State"`
	Config struct {
		Image      string            `json:"Image"`
		Entrypoint []string          `json:"Entrypoint"`
		Cmd        []string          `json:"Cmd"`
		Labels     map[string]string `json:"Labels"`
	} `json:"Config"`
	HostConfig struct {
		NanoCPUs   int64 `json:"NanoCpus"`
		CPUQuota   int64 `json:"CpuQuota"`
		CPUPeriod  int64 `json:"CpuPeriod"`
		Memory     int64 `json:"Memory"`
		MemorySwap int64 `json:"MemorySwap"`
	} `json:"HostConfig"`
}

// dkHealthy reports whether the container declares a healthcheck and passes it.
func (d dkInspectDoc) dkHealthy() (declared, healthy bool) {
	if d.State.Health == nil {
		return false, false
	}
	return true, d.State.Health.Status == "healthy"
}

// dkRun executes the docker CLI and returns stdout. Stderr is folded into the
// error, because a bare exit status is useless for diagnosis.
func dkRun(ctx context.Context, env Env, args ...string) (string, error) {
	if env.Exec != nil {
		return env.Exec(ctx, env.ProjectDir, env.docker(), args)
	}
	cmd := exec.CommandContext(ctx, env.docker(), args...)
	cmd.Dir = env.ProjectDir
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("docker %s: %w: %s",
			strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// dkInspect reads one container's state.
func dkInspect(ctx context.Context, env Env, ref string) (dkInspectDoc, error) {
	out, err := dkRun(ctx, env, "inspect", "--format", "{{json .}}", ref)
	if err != nil {
		return dkInspectDoc{}, err
	}
	line := strings.TrimSpace(out)
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = strings.TrimSpace(line[:i])
	}
	if line == "" {
		return dkInspectDoc{}, fmt.Errorf("docker inspect %s: no output", ref)
	}
	var doc dkInspectDoc
	if err := json.Unmarshal([]byte(line), &doc); err != nil {
		return dkInspectDoc{}, fmt.Errorf("docker inspect %s: %w", ref, err)
	}
	return doc, nil
}

// dkImageExists reports whether an image is present in the local store.
func dkImageExists(ctx context.Context, env Env, image string) bool {
	_, err := dkRun(ctx, env, "image", "inspect", "--format", "{{.Id}}", image)
	return err == nil
}

// ResolveContainerByNode finds the container currently backing a logical node,
// by the label the harness overlay stamps.
//
// Addressing by LABEL rather than by a remembered container id is what makes a
// fault sequence survive a restart or a compose recreate: the id changes, the
// label does not.
func ResolveContainerByNode(ctx context.Context, env Env, nodeID string) (string, error) {
	args := []string{"ps", "--all", "--quiet", "--filter", "label=io.prothesis.node=" + nodeID}
	if env.Topology != nil && env.Topology.ComposeProject != "" {
		args = append(args, "--filter", "label=com.docker.compose.project="+env.Topology.ComposeProject)
	}
	out, err := dkRun(ctx, env, args...)
	if err != nil {
		return "", err
	}
	ids := dkLines(out)
	switch len(ids) {
	case 0:
		return "", fmt.Errorf("no container carries label io.prothesis.node=%s", nodeID)
	case 1:
		return ids[0], nil
	default:
		return "", fmt.Errorf("%d containers carry label io.prothesis.node=%s; a stale container "+
			"from an earlier run was not swept", len(ids), nodeID)
	}
}

func dkLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

func dkShortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

func dkNowNS() int64 { return time.Now().UnixNano() }

// dkQuotaActive reports whether a CpuQuota value throttles anything. Docker
// records "no quota" as either 0 (never set) or -1 (explicitly cleared).
func dkQuotaActive(q int64) bool { return q > 0 }

// dkFloatParam reads a numeric fault parameter from its SOURCE TEXT.
func dkFloatParam(spec schema.FaultSpec, name string) (float64, error) {
	txt, ok := spec.Param(name)
	if !ok {
		return 0, fmt.Errorf("%s: parameter %s is missing", spec.Kind, name)
	}
	v, err := strconv.ParseFloat(txt, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: parameter %s=%q is not a number", spec.Kind, name, txt)
	}
	return v, nil
}

// dkIntParam reads an integer fault parameter from its SOURCE TEXT.
func dkIntParam(spec schema.FaultSpec, name string) (int64, error) {
	txt, ok := spec.Param(name)
	if !ok {
		return 0, fmt.Errorf("%s: parameter %s is missing", spec.Kind, name)
	}
	v, err := strconv.ParseInt(txt, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: parameter %s=%q is not an integer", spec.Kind, name, txt)
	}
	return v, nil
}

// dkSignalArg normalizes proc.kill's signal parameter into the spelling the
// docker CLI accepts.
//
// pkg/schema validates the SHAPE (SIGxxx, or a number in 1..127) and
// deliberately leaves deliverability to the backend, which is here. Docker
// accepts both the SIG-prefixed name and a bare number, so the source text
// passes through unchanged and only the unusable cases are rejected.
func dkSignalArg(spec schema.FaultSpec) (string, error) {
	txt, ok := spec.Param("signal")
	if !ok || txt == "" {
		return "", fmt.Errorf("%s: parameter signal is missing", spec.Kind)
	}
	if n, err := strconv.Atoi(txt); err == nil {
		if n <= 0 || n >= 128 {
			return "", fmt.Errorf("%s: signal %d is outside the deliverable range 1..127", spec.Kind, n)
		}
		return txt, nil
	}
	if !strings.HasPrefix(txt, "SIG") {
		return "", fmt.Errorf("%s: signal %q is neither a SIGxxx name nor a number", spec.Kind, txt)
	}
	return txt, nil
}
