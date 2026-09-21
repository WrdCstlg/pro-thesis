package faults

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/WrdCstlg/pro-thesis/internal/perturber"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// ---------------------------------------------------------------------------
// The CLOCK family: clock.skew(ms) and clock.jump(ms).
//
// # Mechanism, and the two things it cannot do
//
// D-024 settles the mechanism on MEASURED evidence: libfaketime does not work on
// Go binaries; the runtime reads CLOCK_MONOTONIC and CLOCK_REALTIME through the
// vDSO, so LD_PRELOAD never sees the call, and a C control in the same
// experiment DID shift, proving libfaketime itself was working. Linux time
// namespaces do work, on an unmodified image, without --privileged:
//
//	--cap-add SYS_ADMIN --cap-add SYS_TIME
//	entrypoint: unshare --time --monotonic=<s> --boottime=<s> --fork --pid --mount-proc <argv...>
//
// Two constraints are BINDING, not advisory, and neither may be softened in a
// message, a comment or a verdict:
//
//   - OQ-019: a time namespace moves CLOCK_MONOTONIC and CLOCK_BOOTTIME ONLY.
//     THE WALL CLOCK IS NOT AFFECTED. Logic keyed on CLOCK_REALTIME (calendar
//     TTLs, certificate expiry, wall timestamps compared between nodes) cannot
//     be attacked this way, and this package must never imply it can. The KV
//     fixture's read lease is a monotonic deadline, so the case the fixture
//     exists to demonstrate is reachable. That is the whole of the claim.
//
//   - OQ-020: the kernel accepts writes to timens_offsets only BEFORE any
//     process exists in the namespace, so the offset is fixed at namespace
//     creation and cannot be injected into a running process. A clock fault is
//     therefore implementable only as a RESTART INTO A SKEWED NAMESPACE. That is
//     a different physical event from what `@WINDOW` implies, so this fault
//     records THREE entries (the restart in, the skew, and the restart out)
//     and the causal timeline shows all three. It does not pretend a clock
//     shifted underneath a live process.
//
// # Two further limits, measured on this machine while building this file
//
//   - util-linux `unshare` parses --monotonic and --boottime as WHOLE SECONDS.
//     `--monotonic=1.5` is rejected outright ("failed to parse monotonic
//     offset"). A millisecond parameter therefore has second granularity here.
//     An offset that truncates to zero would be a fault that never fires, so it
//     is refused; a non-zero truncation is applied and the APPLIED value (not
//     the requested one) is what the realized record carries.
//
//   - BusyBox's `unshare`, which every alpine-based image ships INCLUDING this
//     project's own kv fixture image, DOES NOT IMPLEMENT --time: it answers
//     "unshare: unrecognized option: time". D-024's command therefore does not
//     run on an arbitrary image. The image is probed before anything is
//     recreated, and one without a --time-capable unshare produces an
//     ErrUnsupported naming the exact remedy rather than a container that will
//     not boot.
// ---------------------------------------------------------------------------

// DefaultClockWaitTimeout bounds how long a recreated container is given to come
// back and pass its healthcheck.
const DefaultClockWaitTimeout = 90 * time.Second

// clockOverlayPrefix names the generated compose fragment.
const clockOverlayPrefix = "prothesis-clock-"

// ClockShift implements clock.skew and clock.jump.
//
// Both kinds use one mechanism because the kernel offers one: the offset is
// constant for the life of the namespace. The distinction the grammar draws
// between them survives in what is RECORDED (skew is durative and its record
// spans the window, jump is instantaneous and its record is stamped at the step)
// not in a physical difference that does not exist.
type ClockShift struct {
	base

	requestedMS int64
	appliedSec  int64

	// WaitTimeout bounds the post-recreate health wait. Zero means
	// DefaultClockWaitTimeout.
	WaitTimeout time.Duration

	ovMu        sync.Mutex
	overlayPath string
}

// NewClockFault builds the primitive for one clock.* kind.
//
// It refuses at CONSTRUCTION only what is knowable without touching Docker: an
// unknown kind, a missing parameter, and an offset that cannot be expressed.
// Everything environmental (whether the image has a usable unshare, whether a
// compose project is addressable) is checked in Inject, where the failure is an
// ErrUnsupported the control plane reports as INCONCLUSIVE.
func NewClockFault(req PrimitiveRequest) (Primitive, error) {
	if err := req.validate(); err != nil {
		return nil, err
	}
	switch req.Spec.Kind {
	case schema.FaultClockSkew, schema.FaultClockJump:
	default:
		return nil, fmt.Errorf("faults: %q is not a clock-family kind", req.Spec.Kind)
	}

	ms, err := dkIntParam(req.Spec, "ms")
	if err != nil {
		return nil, err
	}
	if ms == 0 {
		return nil, fmt.Errorf("%s: ms=0 is not a clock fault", req.Spec.Kind)
	}
	sec := ms / 1000 // Go's integer division truncates toward zero
	if sec == 0 {
		return nil, Unsupportedf(req.Spec.Kind,
			"an offset of %dms truncates to 0 whole seconds, and util-linux `unshare` accepts only "+
				"whole seconds for --monotonic/--boottime (measured: `--monotonic=1.5` is rejected). "+
				"Injecting a zero offset would be a fault that never fires, so it is refused rather "+
				"than silently applied; use an offset of at least 1000ms", ms)
	}

	return &ClockShift{base: newBase(req), requestedMS: ms, appliedSec: sec}, nil
}

// AppliedOffsetMS is the offset actually installed, which may differ from the
// requested one because the mechanism has second granularity. It is what the
// realized record reports.
func (f *ClockShift) AppliedOffsetMS() int64 { return f.appliedSec * 1000 }

// RequestedOffsetMS is what the schedule asked for.
func (f *ClockShift) RequestedOffsetMS() int64 { return f.requestedMS }

// Inject implements Primitive.
func (f *ClockShift) Inject(ctx context.Context) error {
	if err := f.checkEnv(); err != nil {
		return err
	}
	start := dkNowNS()

	var (
		errs []error
		hit  []string
	)
	for _, t := range f.Targets() {
		if err := f.injectOne(ctx, t); err != nil {
			errs = append(errs, err)
			continue
		}
		f.mark(t.NodeID, true)
		hit = append(hit, t.NodeID)
	}

	if len(hit) > 0 {
		f.setInjected(true)
		// THREE records, in the order the events happened. OQ-020 requires the
		// restart to be visible: the node went away and came back skewed, and a
		// world file showing only the skew would misdescribe what happened,
		// and would leave the restart's process exit unexcused by no_crash.
		f.note(schema.FaultProcRestart, hit, start, dkNowNS())
		f.noteOpenResolved(f.spec.Kind, hit, f.appliedResolved(hit), start)
	}
	return errors.Join(errs...)
}

// injectOne recreates one node's container inside a skewed time namespace.
func (f *ClockShift) injectOne(ctx context.Context, t Target) error {
	if t.ComposeService == "" {
		return fmt.Errorf("%s: node %s has no compose service, so its container cannot be recreated",
			f.spec.Kind, t.NodeID)
	}
	st, err := dkInspect(ctx, f.env, t.ContainerID)
	if err != nil {
		return fmt.Errorf("%s: inspect %s: %w", t.NodeID, dkShortID(t.ContainerID), err)
	}
	if st.Config.Image == "" {
		return fmt.Errorf("%s: cannot determine the image backing %s", f.spec.Kind, t.NodeID)
	}
	if err := ClockCapability(ctx, f.env, st.Config.Image); err != nil {
		return fmt.Errorf("node %s: %w", t.NodeID, err)
	}

	argv := append(append([]string(nil), st.Config.Entrypoint...), st.Config.Cmd...)
	if len(argv) == 0 {
		return Unsupportedf(f.spec.Kind,
			"image %s declares neither an ENTRYPOINT nor a CMD, so there is no argv to re-launch "+
				"inside a time namespace", st.Config.Image)
	}

	overlay, err := f.writeOverlay(t.ComposeService, argv)
	if err != nil {
		return fmt.Errorf("%s: %w", t.NodeID, err)
	}
	if err := f.recreate(ctx, t, overlay); err != nil {
		return fmt.Errorf("node %s: %w", t.NodeID, err)
	}

	// Verify the skew is really in force.
	//
	// The structural check is decisive on its own, because `unshare --time
	// --monotonic=N` FAILS CLOSED: if the kernel refuses the namespace or the
	// offset, the process never execs and the container exits at once. A
	// container that is RUNNING with our unshare argv as its entrypoint could
	// not have got there without the offset having been installed.
	id, err := ResolveContainerByNode(ctx, f.env, t.NodeID)
	if err != nil {
		return fmt.Errorf("node %s: after recreate: %w", t.NodeID, err)
	}
	st2, err := dkInspect(ctx, f.env, id)
	if err != nil {
		return fmt.Errorf("node %s: after recreate: %w", t.NodeID, err)
	}
	if !clockArgvIsSkewed(st2.Config.Entrypoint) {
		return fmt.Errorf("node %s: the recreated container is not running under a time namespace "+
			"(entrypoint %v)", t.NodeID, st2.Config.Entrypoint)
	}
	if !st2.State.Running {
		return fmt.Errorf("node %s: the container recreated inside a time namespace is %s; "+
			"`unshare --time` fails closed, so the offset was rejected", t.NodeID, st2.State.Status)
	}

	// Direct confirmation when the image can read its own /proc. Optional (a
	// scratch image has no `cat`, and the structural check already establishes
	// the fact) but a MISMATCH is fatal.
	if off, ok := f.readOffset(ctx, id); ok && off != f.appliedSec {
		return fmt.Errorf("node %s: the time namespace reports a monotonic offset of %ds, not the "+
			"%ds that was asked for", t.NodeID, off, f.appliedSec)
	}
	return nil
}

// Withdraw implements Primitive.
//
// Withdrawal is a SECOND restart: the container is recreated without the
// entrypoint override, which drops it out of the time namespace. There is no
// cheaper undo: a namespace's offset cannot be changed once a process exists in
// it, which is the same kernel rule that forces the restart on the way in.
//
// It decides from OBSERVED state: any target whose live entrypoint is one this
// package installed is restored, whether or not this object injected it.
func (f *ClockShift) Withdraw(ctx context.Context) error {
	end := dkNowNS()
	var (
		errs     []error
		restored []string
	)

	for _, t := range f.Targets() {
		id := t.ContainerID
		if live, err := ResolveContainerByNode(ctx, f.env, t.NodeID); err == nil && live != "" {
			id = live
		}
		st, err := dkInspect(ctx, f.env, id)
		if err != nil {
			if f.isApplied(t.NodeID) {
				errs = append(errs, fmt.Errorf("%s: a skewed container cannot be inspected, so it "+
					"cannot be shown to have been restored: %w", t.NodeID, err))
			}
			continue
		}
		if !clockArgvIsSkewed(st.Config.Entrypoint) {
			f.mark(t.NodeID, false)
			continue
		}
		if err := f.checkEnv(); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", t.NodeID, err))
			continue
		}
		if err := f.recreate(ctx, t, ""); err != nil {
			errs = append(errs, fmt.Errorf("%s: restoring the original entrypoint failed, so the "+
				"node is still running on a skewed clock: %w", t.NodeID, err))
			continue
		}
		newID, err := ResolveContainerByNode(ctx, f.env, t.NodeID)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: after restore: %w", t.NodeID, err))
			continue
		}
		stAfter, err := dkInspect(ctx, f.env, newID)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: after restore: %w", t.NodeID, err))
			continue
		}
		if clockArgvIsSkewed(stAfter.Config.Entrypoint) {
			errs = append(errs, fmt.Errorf("%s: the container is STILL running inside a time "+
				"namespace after withdrawal (entrypoint %v)", t.NodeID, stAfter.Config.Entrypoint))
			continue
		}
		f.mark(t.NodeID, false)
		restored = append(restored, t.NodeID)
	}

	f.ovMu.Lock()
	if f.overlayPath != "" {
		// A leftover fragment is not a fault leak, so failing to remove it never
		// fails the withdrawal.
		_ = os.Remove(f.overlayPath)
		f.overlayPath = ""
	}
	f.ovMu.Unlock()

	if len(restored) > 0 {
		f.closeWindow(f.spec.Kind, end)
		f.note(schema.FaultProcRestart, restored, end, dkNowNS())
	}
	f.setInjected(false)
	return errors.Join(errs...)
}

// ---------------------------------------------------------------------------
// mechanism
// ---------------------------------------------------------------------------

// ClockCapability reports whether an image can host a skewed time namespace.
//
// The probe is `unshare --version`: util-linux prints "unshare from util-linux
// 2.xx" and BusyBox prints its usage and exits non-zero. It is a decisive
// one-container check costing about as much as `docker run true`.
//
// It exists because D-024's command silently assumes util-linux is present, and
// on this project's own kv fixture image (alpine:3.20, hence BusyBox) it is
// not. Discovering that at injection time, as a container that will not boot,
// would look like a bug in the system under test.
func ClockCapability(ctx context.Context, env Env, image string) error {
	out, err := dkRun(ctx, env, "run", "--rm", "--entrypoint", "unshare", image, "--version")
	if err != nil || !strings.Contains(strings.ToLower(out), "util-linux") {
		return Unsupportedf(schema.FaultClockSkew,
			"image %s has no util-linux `unshare`. Linux time namespaces are the only mechanism that "+
				"works here — libfaketime cannot skew a Go binary (D-024) — and BusyBox's unshare, "+
				"which every alpine image ships, does not implement --time. Add util-linux to the "+
				"image (alpine: `RUN apk add --no-cache util-linux-misc`; debian: already present), "+
				"or exclude clock.* from `perturber.allow`", image)
	}
	return nil
}

// checkEnv verifies the compose handoff this fault needs before touching
// anything.
func (f *ClockShift) checkEnv() error {
	top := f.env.Topology
	if top == nil || top.ComposeProject == "" || top.ComposeFile == "" {
		return Unsupportedf(f.spec.Kind,
			"a clock fault is a restart into a skewed time namespace (OQ-020), which needs the "+
				"compose project handoff `thesis up` wrote; none was supplied")
	}
	return nil
}

// clockUnshareArgv builds the entrypoint that enters the skewed namespace.
//
// --monotonic and --boottime are BOTH set, to the same value. D-024 names both
// clocks as the ones a time namespace moves, and shifting one without the other
// produces a clock pair no real machine exhibits: a false-positive generator
// for any system that cross-checks them.
func clockUnshareArgv(offsetSec int64, argv []string) []string {
	off := strconv.FormatInt(offsetSec, 10)
	out := []string{
		"unshare",
		"--time",
		"--monotonic=" + off,
		"--boottime=" + off,
		"--fork",
		"--pid",
		"--mount-proc",
	}
	return append(out, argv...)
}

// clockArgvIsSkewed reports whether an entrypoint is one this package installed.
func clockArgvIsSkewed(argv []string) bool {
	if len(argv) == 0 {
		return false
	}
	if argv[0] != "unshare" && !strings.HasSuffix(argv[0], "/unshare") {
		return false
	}
	for _, a := range argv[1:] {
		if a == "--time" {
			return true
		}
	}
	return false
}

// clockOverlayYAML renders the compose fragment carrying the entrypoint override
// and the two capabilities.
//
// The argv is emitted as a JSON array, which is a valid YAML flow sequence with
// correctly escaped double-quoted scalars. A target's argv can contain spaces,
// colons and quotes, and hand-rolled YAML quoting is how a fault injector turns
// into an injection bug.
func clockOverlayYAML(service, owner string, offsetSec int64, argv []string) (string, error) {
	entry, err := json.Marshal(clockUnshareArgv(offsetSec, argv))
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("# Generated by the PRO-THESIS perturber. Do not edit; removed at HEAL.\n")
	b.WriteString("#\n")
	b.WriteString("# A time namespace's offset is fixed when the namespace is created, so a clock\n")
	b.WriteString("# fault cannot be injected into a running process (OQ-020). This fragment is how\n")
	b.WriteString("# the target is restarted INTO an already-skewed namespace.\n")
	fmt.Fprintf(&b, "# owner: %s\n", owner)
	b.WriteString("services:\n")
	fmt.Fprintf(&b, "  %s:\n", service)
	fmt.Fprintf(&b, "    entrypoint: %s\n", entry)
	b.WriteString("    cap_add: [\"SYS_ADMIN\", \"SYS_TIME\"]\n")
	return b.String(), nil
}

func (f *ClockShift) writeOverlay(service string, argv []string) (string, error) {
	body, err := clockOverlayYAML(service, f.env.Tag(f.faultID), f.appliedSec, argv)
	if err != nil {
		return "", err
	}
	dir := f.env.WorkDir
	if dir == "" {
		dir = os.TempDir()
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, clockOverlayPrefix+f.faultID+".yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		return "", err
	}
	f.ovMu.Lock()
	f.overlayPath = path
	f.ovMu.Unlock()
	return path, nil
}

// composeArgs builds the `compose up --force-recreate` invocation for one
// service, with the clock fragment when clockOverlay is non-empty.
//
// The project name and the base file set must match what `up` used, or compose
// resolves a DIFFERENT project and recreates nothing while reporting success.
func clockComposeArgs(project, composeFile, overlayFile, clockOverlay, service string) []string {
	args := []string{"compose", "--project-name", project, "--file", composeFile}
	if overlayFile != "" {
		args = append(args, "--file", overlayFile)
	}
	if clockOverlay != "" {
		args = append(args, "--file", clockOverlay)
	}
	return append(args, "up", "--detach", "--force-recreate", "--no-deps", service)
}

func (f *ClockShift) recreate(ctx context.Context, t Target, clockOverlay string) error {
	top := f.env.Topology
	overlay := ""
	if top.OverlayFile != "" {
		if _, err := os.Stat(top.OverlayFile); err == nil {
			overlay = top.OverlayFile
		}
	}
	args := clockComposeArgs(top.ComposeProject, top.ComposeFile, overlay, clockOverlay, t.ComposeService)
	if _, err := dkRun(ctx, f.env, args...); err != nil {
		return fmt.Errorf("compose up --force-recreate %s: %w", t.ComposeService, err)
	}
	return f.waitReady(ctx, t)
}

// waitReady blocks until the recreated container is running and, if it declares
// a healthcheck, healthy.
//
// A recreate that is not waited on is worse than no fault at all: the next fault
// in the schedule would address a container that is still booting, and the world
// would record a disturbance the system never actually experienced.
func (f *ClockShift) waitReady(ctx context.Context, t Target) error {
	timeout := f.WaitTimeout
	if timeout <= 0 {
		timeout = DefaultClockWaitTimeout
	}
	deadline := time.Now().Add(timeout)
	last := "not yet observed"
	for {
		if id, err := ResolveContainerByNode(ctx, f.env, t.NodeID); err == nil {
			st, ierr := dkInspect(ctx, f.env, id)
			switch {
			case ierr != nil:
				last = ierr.Error()
			default:
				declared, healthy := st.dkHealthy()
				if st.State.Running && (!declared || healthy) {
					f.rebind(t.NodeID, id)
					return nil
				}
				last = st.State.Status
				if declared && st.State.Health != nil {
					last += "/" + st.State.Health.Status
				}
			}
		} else {
			last = err.Error()
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("node %s did not become ready within %s after being recreated "+
				"(last observed: %s)", t.NodeID, timeout, last)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// readOffset reads the installed monotonic offset from inside the container. It
// returns ok=false when the image cannot run the read, which is not an error.
func (f *ClockShift) readOffset(ctx context.Context, containerID string) (int64, bool) {
	out, err := dkRun(ctx, f.env, "exec", containerID, "cat", "/proc/1/timens_offsets")
	if err != nil {
		return 0, false
	}
	return parseTimensOffset(out)
}

// parseTimensOffset reads the monotonic seconds out of /proc/<pid>/timens_offsets.
func parseTimensOffset(out string) (int64, bool) {
	for _, line := range dkLines(out) {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "monotonic" {
			v, err := strconv.ParseInt(fields[1], 10, 64)
			if err != nil {
				return 0, false
			}
			return v, true
		}
	}
	return 0, false
}

// appliedResolved renders the realized fault string with the APPLIED offset, not
// the requested one.
//
// If the two differ, the record says what actually happened. A realized entry
// carrying the requested value would make the world file a record of an
// intention rather than of an event, which is exactly what the planned/realized
// split exists to prevent.
func (f *ClockShift) appliedResolved(nodes []string) string {
	return fmt.Sprintf("%s(%s, ms=%d)@%d..%d",
		f.spec.Kind, strings.Join(nodes, "+"), f.AppliedOffsetMS(), f.spec.StartMS, f.spec.EndMS)
}

// ---------------------------------------------------------------------------
// perturber.Injector adapter for the clock family
// ---------------------------------------------------------------------------

// ClockInjector implements perturber.Injector for clock.skew and clock.jump.
type ClockInjector struct {
	env Env

	mu   sync.Mutex
	live map[string]Primitive
	recs []Record
}

// NewClockInjector builds the clock family's injector.
func NewClockInjector(env Env) *ClockInjector {
	return &ClockInjector{env: env, live: map[string]Primitive{}}
}

// Kinds implements perturber.Injector.
func (in *ClockInjector) Kinds() []schema.FaultKind {
	return []schema.FaultKind{schema.FaultClockSkew, schema.FaultClockJump}
}

// Records returns every physical event this injector performed.
//
// A clock fault contributes THREE records per injection, not one: the restart
// in, the skew, and the restart out. A caller that folds only the schedule's own
// planned faults into the realized list will silently drop the restarts, and
// no_crash will then have no window excusing the process exits they caused.
func (in *ClockInjector) Records() []Record {
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
func (in *ClockInjector) Inject(ctx context.Context, req perturber.InjectRequest) error {
	p, err := in.build(req.RunID, req.FaultID, req.Spec, req.Nodes)
	if err != nil {
		return err
	}
	if err := p.Inject(ctx); err != nil {
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

// Withdraw implements perturber.Injector.
func (in *ClockInjector) Withdraw(ctx context.Context, req perturber.WithdrawRequest) error {
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

// VerifyClean implements perturber.Injector: no container is still running
// inside a time namespace this package created.
//
// A node left skewed is not a cosmetic leak. Every subsequent world on that
// container would run against a monotonic clock offset by an amount nothing
// recorded, and the resulting lease and election-timer behaviour would be
// attributed to the system under test.
func (in *ClockInjector) VerifyClean(ctx context.Context, req perturber.VerifyRequest) ([]perturber.Residue, error) {
	targets, err := TargetsFromNodes(req.Nodes)
	if err != nil {
		return nil, err
	}
	var out []perturber.Residue
	for _, t := range targets {
		id := t.ContainerID
		if live, lerr := ResolveContainerByNode(ctx, in.env, t.NodeID); lerr == nil && live != "" {
			id = live
		}
		st, ierr := dkInspect(ctx, in.env, id)
		if ierr != nil {
			// A container that is gone carries no namespace: the namespace died
			// with its last process.
			continue
		}
		if clockArgvIsSkewed(st.Config.Entrypoint) {
			out = append(out, perturber.Residue{
				NodeID:    t.NodeID,
				Mechanism: "time-namespace",
				Detail: fmt.Sprintf("container %s is still running inside a PRO-THESIS time "+
					"namespace: entrypoint %v", dkShortID(id), st.Config.Entrypoint),
			})
		}
	}
	return out, nil
}

func (in *ClockInjector) build(runID, faultID string, spec schema.FaultSpec, nodes []perturber.Node) (Primitive, error) {
	targets, err := TargetsFromNodes(nodes)
	if err != nil {
		return nil, err
	}
	env := in.env
	if runID != "" {
		env.RunID = runID
	}
	return NewClockFault(PrimitiveRequest{Env: env, Spec: spec, FaultID: faultID, Targets: targets})
}
