package faults

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/WrdCstlg/pro-thesis/internal/perturber"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// ---------------------------------------------------------------------------
// The I/O and RESOURCE families: io.latency, io.error, io.fill, mem.pressure,
// fd.exhaust.
//
// This is the family where honesty costs the most, so the rule is stated up
// front. A kind that cannot be injected on this platform returns an error
// wrapping ErrUnsupported, which the control plane reports as exit 2
// INCONCLUSIVE. It is NEVER a silent no-op and never a pass: a world whose
// faults did not fire, reporting PASS, is a false "we tested that" about a
// system nobody perturbed.
//
// Measured on this machine (Docker Desktop 29.1.3, Linux engine, cgroup v2)
// and what follows from it:
//
//	io.fill       IMPLEMENTED. `docker exec` + df/dd inside the target. Needs a
//	              shell in the image; probed, and refused with a specific
//	              message when absent.
//	fd.exhaust    IMPLEMENTED. A helper container sharing the target's PID
//	              namespace lowers RLIMIT_NOFILE on the target's PID 1 with
//	              prlimit(2). Measured end to end: 1048576 -> 64 -> restored,
//	              observed from inside the target.
//	mem.pressure  PARTIAL, and refused where it cannot be withdrawn — it works
//	              only on a container that ALREADY declares a memory limit.
//	io.latency    NOT IMPLEMENTABLE HERE.
//	io.error      NOT IMPLEMENTABLE HERE.
//
// PlatformCapability carries the reasons for the last two, in the words a person
// who has to work around them needs.
// ---------------------------------------------------------------------------

// DefaultHelperImage is the image used by the one fault that needs a privileged
// helper container. It must contain util-linux's prlimit; BusyBox does not have
// prlimit, so an alpine image is not a substitute.
const DefaultHelperImage = "debian:stable-slim"

// DefaultFillDir is where io.fill places its ballast. /tmp is on the container's
// writable layer in every image this project targets, so filling it fills the
// filesystem the process actually writes to, without touching a data volume
// whose contents a later oracle may read.
const DefaultFillDir = "/tmp"

// fillPrefix names io.fill's ballast files. It carries the run and fault ids for
// the same reason an injected iptables rule carries a comment (D-026): residual
// verification must match on OWNERSHIP, because a target may legitimately have
// files of its own in the same directory.
const fillPrefix = ".thesis-fill-"

// DefaultFDHeadroom is how many descriptors above the target's CURRENT open
// count fd.exhaust leaves.
//
// `fd.exhaust` declares no parameters in the frozen registry, so the magnitude
// has to come from somewhere. Deriving it from the live open-descriptor count
// makes the fault mean the same thing ("you are one handful of sockets from
// EMFILE") on a process holding 12 descriptors and on one holding 12,000,
// which a fixed absolute limit would not.
const DefaultFDHeadroom = 8

// dockerMinMemoryBytes is the smallest memory limit the daemon accepts.
const dockerMinMemoryBytes int64 = 6 * 1024 * 1024

// PlatformCapability reports whether a kind can be injected on this platform AT
// ALL, without touching Docker.
//
// It answers only the statically decidable question, and it exists so a schedule
// can be rejected before a world is booted and driven. A kind that passes here
// may still be refused at injection time for an environmental reason (an image
// without util-linux for clock.*, a container with no memory limit for
// mem.pressure) and those refusals also wrap ErrUnsupported.
func PlatformCapability(kind schema.FaultKind) error {
	switch kind {
	case schema.FaultIOLatency:
		return Unsupportedf(kind,
			"delaying filesystem operations needs a shim between the process and its storage — a FUSE "+
				"layer or a device-mapper target — and neither can be inserted under a RUNNING "+
				"container's overlay2 mount. Docker Desktop's block devices belong to the LinuxKit VM, "+
				"`docker update` cannot change device throttles after create, and the cgroup io "+
				"controller expresses bandwidth and IOPS but not latency. Exclude io.latency from "+
				"`perturber.allow`, or model the delay with net.latency where the storage is remote")
	case schema.FaultIOError:
		return Unsupportedf(kind,
			"failing a fraction of filesystem operations needs dm-flakey or a FUSE error-injection "+
				"layer beneath the target's filesystem, which means owning the storage stack before "+
				"the container starts. Nothing reachable from the Docker CLI can fail a proportion of "+
				"a running container's file operations. Exclude io.error from `perturber.allow`")
	}
	return nil
}

// NewIOFault builds the primitive for one io.* / mem.* / fd.* kind.
func NewIOFault(req PrimitiveRequest) (Primitive, error) {
	if err := req.validate(); err != nil {
		return nil, err
	}
	switch req.Spec.Kind {
	case schema.FaultIOLatency, schema.FaultIOError:
		return &IOUnsupported{base: newBase(req), why: PlatformCapability(req.Spec.Kind)}, nil

	case schema.FaultIOFill:
		pct, err := dkFloatParam(req.Spec, "pct")
		if err != nil {
			return nil, err
		}
		if pct <= 0 {
			return nil, fmt.Errorf("%s: pct=%g occupies nothing", req.Spec.Kind, pct)
		}
		return &IOFill{base: newBase(req), pct: pct, Dir: DefaultFillDir}, nil

	case schema.FaultMemPressure:
		pct, err := dkFloatParam(req.Spec, "pct")
		if err != nil {
			return nil, err
		}
		if pct <= 0 || pct >= 100 {
			return nil, fmt.Errorf("%s: pct=%g must be in (0, 100); 0 applies no pressure and 100 "+
				"leaves the target no memory at all, which is a kill and not a pressure fault",
				req.Spec.Kind, pct)
		}
		return &MemPressure{base: newBase(req), pct: pct, baseline: map[string]dkMemState{}}, nil

	case schema.FaultFDExhaust:
		return &FDExhaust{base: newBase(req), baseline: map[string]dkFDState{}}, nil
	}
	return nil, fmt.Errorf("faults: %q is not an io/mem/fd-family kind", req.Spec.Kind)
}

// ---------------------------------------------------------------------------
// io.latency and io.error: refused, loudly
// ---------------------------------------------------------------------------

// IOUnsupported stands in for a kind this platform cannot inject.
//
// It exists rather than a nil so that a schedule containing io.latency is still
// a well-formed schedule with a fault object in it, and so the refusal arrives
// at INJECT, where the control plane is already handling errors and can report
// INCONCLUSIVE with the reason attached.
//
// It never reports itself as active and it produces no records, so nothing
// downstream can mistake it for a disturbance that occurred.
type IOUnsupported struct {
	base
	why error
}

// Inject implements Primitive. It always fails, wrapping ErrUnsupported.
func (f *IOUnsupported) Inject(_ context.Context) error { return f.why }

// Withdraw implements Primitive. Nothing was applied, so there is nothing to
// undo.
func (f *IOUnsupported) Withdraw(_ context.Context) error { return nil }

// ---------------------------------------------------------------------------
// io.fill
// ---------------------------------------------------------------------------

// IOFill is `io.fill(pct)`: occupy a percentage of the target's filesystem,
// releasing it on withdraw.
//
// It writes one ballast file per target, named with the run and fault ids, and
// removes it on withdraw. Naming the file after its owner is what lets HEAL tell
// PRO-THESIS's residue apart from files the target legitimately created: the
// same rule D-026 imposes on iptables rules, for the same reason.
type IOFill struct {
	base
	pct float64

	// Dir is the directory whose filesystem is filled. Empty means
	// DefaultFillDir.
	Dir string
}

func (f *IOFill) dir() string {
	if f.Dir != "" {
		return f.Dir
	}
	return DefaultFillDir
}

// BallastPath is the file this fault writes. It is derived from the run and
// fault ids alone, so a withdrawal in a process that did not perform the
// injection addresses exactly the same file.
func (f *IOFill) BallastPath() string {
	return f.dir() + "/" + fillPrefix + f.env.RunID + "-" + f.faultID
}

// Inject implements Primitive.
func (f *IOFill) Inject(ctx context.Context) error {
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
		f.noteOpen(schema.FaultIOFill, hit, start)
	}
	return errors.Join(errs...)
}

func (f *IOFill) injectOne(ctx context.Context, t Target) error {
	if err := dkRequireFillTools(ctx, f.env, t); err != nil {
		return err
	}
	out, err := dkRun(ctx, f.env, "exec", t.ContainerID, "df", "-kP", f.dir())
	if err != nil {
		return fmt.Errorf("%s: reading the filesystem size of %s: %w", t.NodeID, f.dir(), err)
	}
	totalKB, usedKB, availKB, err := parseDF(out)
	if err != nil {
		return fmt.Errorf("%s: %w", t.NodeID, err)
	}

	addMB, err := fillBallastMB(f.pct, totalKB, usedKB, availKB)
	if err != nil {
		return fmt.Errorf("node %s: %w", t.NodeID, err)
	}

	path := f.BallastPath()
	// `dd` exiting non-zero on ENOSPC is expected when the filesystem fills
	// before the requested count, so the `ls` is what decides whether ballast
	// actually landed. Without it a full disk would look like a fault that
	// failed, and a fault that never wrote a byte would look like one that
	// worked.
	script := fmt.Sprintf("dd if=/dev/zero of=%s bs=1048576 count=%d 2>/dev/null; ls -l %s >/dev/null",
		dkShellQuote(path), addMB, dkShellQuote(path))
	if _, err := dkRun(ctx, f.env, "exec", t.ContainerID, "sh", "-c", script); err != nil {
		return fmt.Errorf("%s: writing ballast to %s: %w", t.NodeID, path, err)
	}
	return nil
}

// fillBallastMB decides how much ballast to write. Pure, so the arithmetic and
// every refusal is unit-testable without a container.
func fillBallastMB(pct float64, totalKB, usedKB, availKB int64) (int64, error) {
	if totalKB <= 0 {
		return 0, fmt.Errorf("the filesystem reports %d total blocks", totalKB)
	}
	wantUsedKB := int64(math.Round(pct / 100 * float64(totalKB)))
	addKB := wantUsedKB - usedKB
	if addKB <= 0 {
		return 0, Unsupportedf(schema.FaultIOFill,
			"the filesystem is already %.1f%% full, at or above the requested %g%%; writing nothing "+
				"and reporting success would be a fault that never fired",
			100*float64(usedKB)/float64(totalKB), pct)
	}
	if addKB > availKB {
		addKB = availKB
	}
	addMB := addKB / 1024
	if addMB < 1 {
		return 0, Unsupportedf(schema.FaultIOFill,
			"filling to %g%% needs less than 1MiB of ballast, which is below the granularity of this "+
				"mechanism", pct)
	}
	return addMB, nil
}

// Withdraw implements Primitive.
//
// `rm -f` is idempotent and addresses a path derived from the run and fault ids,
// so it is correct whether or not this object performed the injection.
func (f *IOFill) Withdraw(ctx context.Context) error {
	end := dkNowNS()
	var errs []error
	freed := false
	for _, t := range f.Targets() {
		st, err := dkInspect(ctx, f.env, t.ContainerID)
		if err != nil || !st.State.Running {
			// A container that is gone took its writable layer with it.
			f.mark(t.NodeID, false)
			continue
		}
		script := fmt.Sprintf("rm -f %s", dkShellQuote(f.BallastPath()))
		if _, err := dkRun(ctx, f.env, "exec", t.ContainerID, "sh", "-c", script); err != nil {
			errs = append(errs, fmt.Errorf("%s: removing ballast %s: %w (the space stays occupied "+
				"in every subsequent world)", t.NodeID, f.BallastPath(), err))
			continue
		}
		if f.isApplied(t.NodeID) {
			freed = true
		}
		f.mark(t.NodeID, false)
	}
	if freed {
		f.closeWindow(schema.FaultIOFill, end)
	}
	f.setInjected(false)
	return errors.Join(errs...)
}

// parseDF reads `df -kP` output: total, used and available 1K blocks.
//
// -P forces the POSIX single-line-per-filesystem format, and that is what makes
// this parse safe: without it a long device name wraps onto its own line and
// every field offset moves.
func parseDF(out string) (totalKB, usedKB, availKB int64, err error) {
	lines := dkLines(out)
	if len(lines) < 2 {
		return 0, 0, 0, fmt.Errorf("df produced no filesystem row: %q", strings.TrimSpace(out))
	}
	row := lines[len(lines)-1]
	fields := strings.Fields(row)
	if len(fields) < 4 {
		return 0, 0, 0, fmt.Errorf("cannot parse df row %q", row)
	}
	// Filesystem  1024-blocks  Used  Available  Capacity  Mounted-on
	vals := make([]int64, 3)
	for i, s := range fields[1:4] {
		v, perr := strconv.ParseInt(s, 10, 64)
		if perr != nil {
			return 0, 0, 0, fmt.Errorf("cannot parse df row %q: %q is not a block count", row, s)
		}
		vals[i] = v
	}
	if vals[0] <= 0 {
		return 0, 0, 0, fmt.Errorf("df reports a filesystem of %d blocks", vals[0])
	}
	return vals[0], vals[1], vals[2], nil
}

// ---------------------------------------------------------------------------
// mem.pressure
// ---------------------------------------------------------------------------

type dkMemState struct {
	Memory     int64
	MemorySwap int64
	Known      bool
}

// MemPressure is `mem.pressure(pct)`.
//
// # What it does, precisely
//
// The registry describes the kind as "hold a percentage of the target's memory
// limit resident". Holding memory resident requires running an allocator inside
// the target's own cgroup, which the Docker CLI cannot arrange for a container
// that is already running. What IS reachable is the equivalent condition from
// the other side: shrink the limit so that pct of the ORIGINAL limit is no
// longer available. The process meets the same wall (reclaim pressure, then the
// OOM killer) and withdrawal restores the exact original byte value.
//
// That is a mechanism deviation from the registry's wording, and it is recorded
// as such rather than hidden: here, in the realized schedule, and in
// OPEN_QUESTIONS.md.
//
// # Why it refuses a container with no memory limit
//
// Measured on this host: `docker update --memory 0` is a NO-OP (the daemon
// reads 0 as "no change") and `docker update --memory -1` is rejected by the
// CLI as an invalid size. So once a limit is placed on a container that started
// unlimited, nothing in the Docker CLI can make it unlimited again. Injecting
// under those conditions would produce a fault that cannot be withdrawn, which
// violates the one rule this package has no discretion over. It is refused, with
// ErrUnsupported, rather than injected and leaked.
//
// The rejected alternative was to write `max` into the container's memory.max
// from a privileged helper. It would restore the kernel's view while leaving
// docker's HostConfig.Memory saying otherwise, so the residual check would
// false-fail and the next `docker update` would silently re-apply the old limit.
// A withdrawal that leaves two sources of truth disagreeing is not a withdrawal.
type MemPressure struct {
	base
	pct float64

	blMu     sync.Mutex
	baseline map[string]dkMemState
}

// SetBaseline seeds the memory baseline from the BOOT snapshot, so a withdrawal
// works in a process that did not perform the injection.
func (f *MemPressure) SetBaseline(bl ContainerBaseline) {
	f.blMu.Lock()
	defer f.blMu.Unlock()
	for _, n := range bl.Nodes {
		f.baseline[n.NodeID] = dkMemState{Memory: n.Memory, MemorySwap: n.MemorySwap, Known: true}
	}
}

func (f *MemPressure) getBaseline(nodeID string) (dkMemState, bool) {
	f.blMu.Lock()
	defer f.blMu.Unlock()
	s, ok := f.baseline[nodeID]
	return s, ok && s.Known
}

func (f *MemPressure) putBaseline(nodeID string, s dkMemState) {
	f.blMu.Lock()
	defer f.blMu.Unlock()
	if prev, ok := f.baseline[nodeID]; ok && prev.Known {
		return
	}
	s.Known = true
	f.baseline[nodeID] = s
}

// Inject implements Primitive.
func (f *MemPressure) Inject(ctx context.Context) error {
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
		if st.HostConfig.Memory <= 0 {
			errs = append(errs, Unsupportedf(f.spec.Kind,
				"container %s (%s) declares no memory limit. A limit placed on an unlimited container "+
					"cannot be removed again — `docker update --memory 0` is a no-op and `--memory -1` "+
					"is rejected — so this fault would not be withdrawable. Give the service a "+
					"`mem_limit` in the compose file, or exclude mem.pressure from `perturber.allow`",
				t.NodeID, dkShortID(t.ContainerID)))
			continue
		}
		f.putBaseline(t.NodeID, dkMemState{Memory: st.HostConfig.Memory, MemorySwap: st.HostConfig.MemorySwap})
		bl, _ := f.getBaseline(t.NodeID)

		limit, err := memPressureLimit(f.pct, bl.Memory)
		if err != nil {
			errs = append(errs, fmt.Errorf("node %s: %w", t.NodeID, err))
			continue
		}
		if err := dkSetMemory(ctx, f.env, t.ContainerID, limit, bl.MemorySwap); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", t.NodeID, err))
			continue
		}
		f.mark(t.NodeID, true)
		hit = append(hit, t.NodeID)
	}
	if len(hit) > 0 {
		f.setInjected(true)
		f.noteOpen(schema.FaultMemPressure, hit, start)
	}
	return errors.Join(errs...)
}

// memPressureLimit is the new memory ceiling. Pure, so the arithmetic and the
// minimum-size refusal are testable without Docker.
func memPressureLimit(pct float64, baseline int64) (int64, error) {
	limit := int64(math.Round(float64(baseline) * (100 - pct) / 100))
	if limit < dockerMinMemoryBytes {
		return 0, Unsupportedf(schema.FaultMemPressure,
			"withholding %g%% of a %d-byte limit leaves %d bytes, below docker's %d-byte minimum",
			pct, baseline, limit, dockerMinMemoryBytes)
	}
	return limit, nil
}

// Withdraw implements Primitive. It restores from the baseline whenever the live
// limit differs from it, whether or not this object injected.
func (f *MemPressure) Withdraw(ctx context.Context) error {
	end := dkNowNS()
	var errs []error
	restored := false
	for _, t := range f.Targets() {
		st, err := dkInspect(ctx, f.env, t.ContainerID)
		if err != nil {
			if f.isApplied(t.NodeID) {
				errs = append(errs, fmt.Errorf("%s: a memory-limited container cannot be inspected, "+
					"so its limit cannot be shown to have been restored: %w", t.NodeID, err))
			}
			continue
		}
		bl, known := f.getBaseline(t.NodeID)
		if !known {
			if f.isApplied(t.NodeID) {
				errs = append(errs, fmt.Errorf("%s: no memory baseline was recorded, so the original "+
					"limit cannot be restored; the container still reports %d bytes",
					t.NodeID, st.HostConfig.Memory))
			}
			continue
		}
		if st.HostConfig.Memory == bl.Memory {
			f.mark(t.NodeID, false)
			continue
		}
		if err := dkSetMemory(ctx, f.env, t.ContainerID, bl.Memory, bl.MemorySwap); err != nil {
			errs = append(errs, fmt.Errorf("%s: restoring the memory limit failed, so the pressure is "+
				"still in force: %w", t.NodeID, err))
			continue
		}
		f.mark(t.NodeID, false)
		restored = true
	}
	if restored {
		f.closeWindow(schema.FaultMemPressure, end)
	}
	f.setInjected(false)
	return errors.Join(errs...)
}

// dkSetMemory applies a memory limit, retrying with an explicit swap value when
// the daemon complains that memory-swap is smaller than memory.
func dkSetMemory(ctx context.Context, env Env, containerID string, limit, swap int64) error {
	args := []string{"update", "--memory", strconv.FormatInt(limit, 10)}
	if swap > 0 {
		args = append(args, "--memory-swap", strconv.FormatInt(swap, 10))
	}
	args = append(args, containerID)
	_, err := dkRun(ctx, env, args...)
	if err == nil {
		return nil
	}
	if !strings.Contains(strings.ToLower(err.Error()), "swap") {
		return fmt.Errorf("docker update --memory: %w", err)
	}
	retry := []string{"update",
		"--memory", strconv.FormatInt(limit, 10),
		"--memory-swap", strconv.FormatInt(limit, 10),
		containerID}
	if _, rerr := dkRun(ctx, env, retry...); rerr != nil {
		return fmt.Errorf("docker update --memory (with swap): %w", rerr)
	}
	return nil
}

// ---------------------------------------------------------------------------
// fd.exhaust
// ---------------------------------------------------------------------------

type dkFDState struct {
	// Soft and Hard are the limits as /proc spells them: a decimal count or
	// the word "unlimited".
	Soft string
	Hard string
	Open int64
}

// FDExhaust is `fd.exhaust`: hold the target's file descriptor table near its
// limit.
//
// # Mechanism, measured
//
// A helper container joined to the target's PID namespace lowers RLIMIT_NOFILE
// on the target's PID 1 with prlimit(2):
//
//	docker run --rm --pid container:<target> --cap-add SYS_RESOURCE <helper> \
//	  prlimit --pid 1 --nofile=<n>:<n>
//
// Verified end to end on this host: the target's "Max open files" went 1048576
// -> 64 and back, observed from INSIDE the target, with no change to the
// target's image or configuration. Sharing the PID namespace is what makes PID 1
// in the helper resolve to the target's own PID 1; CAP_SYS_RESOURCE is what
// allows the hard limit to be raised again at withdrawal.
//
// Lowering the limit does not close descriptors that are already open; it makes
// the NEXT open fail with EMFILE, which is exactly how fd exhaustion presents in
// a server: existing connections survive, new ones are refused.
//
// # Three constraints
//
//   - It acts on PID 1 of the target's namespace. A container started with
//     `init: true` has docker-init as PID 1 and the application beneath it, so
//     the limit would land on the wrong process. This project's fixture sets
//     `init: false` deliberately; a target that does not must be excluded.
//   - It needs a helper image containing util-linux's prlimit. BusyBox does not
//     have prlimit, so an alpine image is not a substitute.
//   - The image must already be in the local store. Pulling one at injection
//     time would put a registry round trip inside a timed fault window and make
//     injection depend on outbound network access.
type FDExhaust struct {
	base

	// Headroom is how many descriptors above the current open count to allow.
	// Zero means DefaultFDHeadroom.
	Headroom int64

	blMu     sync.Mutex
	baseline map[string]dkFDState
}

func (f *FDExhaust) helperImage() string {
	if f.env.HelperImage != "" {
		return f.env.HelperImage
	}
	return DefaultHelperImage
}

func (f *FDExhaust) headroom() int64 {
	if f.Headroom > 0 {
		return f.Headroom
	}
	return DefaultFDHeadroom
}

// SetBaseline seeds the descriptor-limit baseline from the BOOT snapshot, so a
// withdrawal works in a process that did not perform the injection.
func (f *FDExhaust) SetBaseline(bl ContainerBaseline) {
	f.blMu.Lock()
	defer f.blMu.Unlock()
	for _, n := range bl.Nodes {
		if n.FDSoft == "" || n.FDHard == "" {
			continue
		}
		f.baseline[n.NodeID] = dkFDState{Soft: n.FDSoft, Hard: n.FDHard}
	}
}

func (f *FDExhaust) getBaseline(nodeID string) (dkFDState, bool) {
	f.blMu.Lock()
	defer f.blMu.Unlock()
	s, ok := f.baseline[nodeID]
	return s, ok && s.Soft != "" && s.Hard != ""
}

func (f *FDExhaust) putBaseline(nodeID string, s dkFDState) {
	f.blMu.Lock()
	defer f.blMu.Unlock()
	if prev, ok := f.baseline[nodeID]; ok && prev.Soft != "" {
		// Keep the BOOT value, but take the live open count, which the BOOT
		// snapshot does not carry.
		prev.Open = s.Open
		f.baseline[nodeID] = prev
		return
	}
	f.baseline[nodeID] = s
}

// Inject implements Primitive.
func (f *FDExhaust) Inject(ctx context.Context) error {
	start := dkNowNS()
	image := f.helperImage()
	if !dkImageExists(ctx, f.env, image) {
		return Unsupportedf(f.spec.Kind,
			"the helper image %q is not present in the local image store, and fd.exhaust needs "+
				"util-linux's prlimit (BusyBox does not have it). Run `docker pull %s` once, or point "+
				"Env.HelperImage at an image that does contain prlimit. Pulling it here would put a "+
				"registry round trip inside the fault window", image, image)
	}

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
		if !st.State.Running || st.State.Paused {
			errs = append(errs, fmt.Errorf("%s: container %s is %s; a PID namespace can only be "+
				"joined while the target is running", t.NodeID, dkShortID(t.ContainerID), st.State.Status))
			continue
		}

		live, err := readFDState(ctx, f.env, image, t.ContainerID)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: reading the current descriptor limits: %w", t.NodeID, err))
			continue
		}
		f.putBaseline(t.NodeID, live)

		limit, err := fdTargetLimit(live, f.headroom())
		if err != nil {
			errs = append(errs, fmt.Errorf("node %s: %w", t.NodeID, err))
			continue
		}
		if _, err := dkRun(ctx, f.env, fdPrlimitArgs(image, t.ContainerID,
			strconv.FormatInt(limit, 10), strconv.FormatInt(limit, 10))...); err != nil {
			errs = append(errs, fmt.Errorf("%s: prlimit --nofile=%d: %w", t.NodeID, limit, err))
			continue
		}
		// Verify rather than assume: prlimit exiting 0 in a helper container is
		// not evidence that the target's rlimit moved.
		if now, err := readFDState(ctx, f.env, image, t.ContainerID); err == nil {
			if got, ok := dkParseLimit(now.Soft); !ok || got != limit {
				errs = append(errs, fmt.Errorf("%s: prlimit reported success but the soft limit is "+
					"%q, not %d", t.NodeID, now.Soft, limit))
				continue
			}
		}
		f.mark(t.NodeID, true)
		hit = append(hit, t.NodeID)
	}
	if len(hit) > 0 {
		f.setInjected(true)
		f.noteOpen(schema.FaultFDExhaust, hit, start)
	}
	return errors.Join(errs...)
}

// fdTargetLimit picks the constrained limit. Pure, so the "this would not
// constrain anything" refusal is testable without Docker.
func fdTargetLimit(live dkFDState, headroom int64) (int64, error) {
	limit := live.Open + headroom
	if soft, ok := dkParseLimit(live.Soft); ok && limit >= soft {
		return 0, Unsupportedf(schema.FaultFDExhaust,
			"%d descriptors are already open against a soft limit of %d, so lowering the limit to %d "+
				"would not constrain anything; raising the fault's headroom is the wrong fix, the "+
				"target simply has more slack than this mechanism can take away",
			live.Open, soft, limit)
	}
	if limit < 1 {
		return 0, Unsupportedf(schema.FaultFDExhaust,
			"a computed limit of %d descriptors is not a limit", limit)
	}
	return limit, nil
}

// fdPrlimitArgs builds the helper invocation. Pure, so the argument shape is
// pinned by a test rather than by a successful run on one machine.
func fdPrlimitArgs(image, containerID, soft, hard string) []string {
	return []string{
		"run", "--rm",
		"--pid", "container:" + containerID,
		"--cap-add", "SYS_RESOURCE",
		image,
		"prlimit", "--pid", "1", fmt.Sprintf("--nofile=%s:%s", soft, hard),
	}
}

// Withdraw implements Primitive.
func (f *FDExhaust) Withdraw(ctx context.Context) error {
	end := dkNowNS()
	image := f.helperImage()
	var errs []error
	restored := false

	for _, t := range f.Targets() {
		st, err := dkInspect(ctx, f.env, t.ContainerID)
		if err != nil || !st.State.Running {
			// RLIMIT_NOFILE is per-process. The process it applied to is gone,
			// and the limit went with it.
			f.mark(t.NodeID, false)
			continue
		}
		bl, known := f.getBaseline(t.NodeID)
		if !known {
			if f.isApplied(t.NodeID) {
				errs = append(errs, fmt.Errorf("%s: no descriptor-limit baseline was recorded, so the "+
					"original limit cannot be restored", t.NodeID))
			}
			continue
		}
		if !dkImageExists(ctx, f.env, image) {
			errs = append(errs, fmt.Errorf("%s: helper image %q is gone, so the descriptor limit "+
				"cannot be restored and the target is still constrained", t.NodeID, image))
			continue
		}
		if live, err := readFDState(ctx, f.env, image, t.ContainerID); err == nil &&
			live.Soft == bl.Soft && live.Hard == bl.Hard {
			f.mark(t.NodeID, false)
			continue
		}
		if _, err := dkRun(ctx, f.env, fdPrlimitArgs(image, t.ContainerID, bl.Soft, bl.Hard)...); err != nil {
			errs = append(errs, fmt.Errorf("%s: restoring --nofile=%s:%s failed, so the descriptor "+
				"limit is still constrained: %w", t.NodeID, bl.Soft, bl.Hard, err))
			continue
		}
		f.mark(t.NodeID, false)
		restored = true
	}
	if restored {
		f.closeWindow(schema.FaultFDExhaust, end)
	}
	f.setInjected(false)
	return errors.Join(errs...)
}

// readFDState reads PID 1's descriptor limits and current open count from a
// helper container sharing the target's PID namespace.
//
// It reads from the HELPER rather than from the target so the measurement does
// not depend on the target image having a shell: the same reason the injection
// uses a helper.
func readFDState(ctx context.Context, env Env, image, containerID string) (dkFDState, error) {
	out, err := dkRun(ctx, env,
		"run", "--rm", "--pid", "container:"+containerID, image,
		"sh", "-c", "grep -i '^Max open files' /proc/1/limits; echo \"OPENFDS $(ls -1 /proc/1/fd | wc -l)\"")
	if err != nil {
		return dkFDState{}, err
	}
	return parseFDState(out)
}

// parseFDState parses the helper's output.
func parseFDState(out string) (dkFDState, error) {
	var st dkFDState
	for _, line := range dkLines(out) {
		fields := strings.Fields(line)
		switch {
		case len(fields) >= 5 && strings.EqualFold(fields[0], "Max") &&
			strings.EqualFold(fields[1], "open") && strings.EqualFold(fields[2], "files"):
			st.Soft, st.Hard = fields[3], fields[4]
		case len(fields) == 2 && fields[0] == "OPENFDS":
			n, err := strconv.ParseInt(fields[1], 10, 64)
			if err != nil {
				return dkFDState{}, fmt.Errorf("cannot parse open descriptor count %q", fields[1])
			}
			st.Open = n
		}
	}
	if st.Soft == "" || st.Hard == "" {
		return dkFDState{}, fmt.Errorf("no 'Max open files' row in %q", strings.TrimSpace(out))
	}
	return st, nil
}

// dkParseLimit converts an rlimit as /proc spells it into a number. "unlimited"
// has no numeric value and returns ok=false.
func dkParseLimit(s string) (int64, bool) {
	if strings.EqualFold(s, "unlimited") {
		return 0, false
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// ---------------------------------------------------------------------------
// perturber.Injector adapter for the I/O and resource families
// ---------------------------------------------------------------------------

// IOInjector implements perturber.Injector for io.latency, io.error, io.fill,
// mem.pressure and fd.exhaust.
//
// It claims io.latency and io.error DELIBERATELY, even though it cannot inject
// them. Leaving them unclaimed would make the executor report "no injector is
// registered": true, but it says nothing about why or what to do. Claiming them
// and failing with the measured reason turns a dead end into an instruction.
// Callers that want the schedule rejected before a world is booted should
// pre-flight every kind through PlatformCapability.
type IOInjector struct {
	env      Env
	baseline ContainerBaseline

	mu   sync.Mutex
	live map[string]Primitive
	recs []Record
}

// NewIOInjector builds the I/O family's injector. baseline is the BOOT snapshot;
// mem.pressure and fd.exhaust withdraw against it.
func NewIOInjector(env Env, baseline ContainerBaseline) *IOInjector {
	return &IOInjector{env: env, baseline: baseline, live: map[string]Primitive{}}
}

// Kinds implements perturber.Injector.
func (in *IOInjector) Kinds() []schema.FaultKind {
	return []schema.FaultKind{
		schema.FaultIOLatency,
		schema.FaultIOError,
		schema.FaultIOFill,
		schema.FaultMemPressure,
		schema.FaultFDExhaust,
	}
}

// Capability implements perturber.PlatformCapable.
//
// Two of the five kinds this injector claims cannot be delivered on this host at
// all, and it knows so without touching Docker. Saying it HERE is what lets a
// schedule naming io.latency be refused at compile time rather than eight seconds
// into DRIVE, having already spent a compose project and a bridge network to
// learn what PlatformCapability knows for free. D-049 wired the same table into
// the search's action space; this covers every other path into the executor.
func (in *IOInjector) Capability(k schema.FaultKind) error {
	return PlatformCapability(k)
}

// Records returns every physical event this injector performed.
func (in *IOInjector) Records() []Record {
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
func (in *IOInjector) Inject(ctx context.Context, req perturber.InjectRequest) error {
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
func (in *IOInjector) Withdraw(ctx context.Context, req perturber.WithdrawRequest) error {
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

// VerifyClean implements perturber.Injector: no ballast file this run wrote is
// still on any target's filesystem, no memory limit differs from the BOOT
// baseline, and no descriptor limit is still below it.
//
// The ballast scan matches on the OWNERSHIP PREFIX, never on an assumed-empty
// directory: a target may legitimately keep files in /tmp, and a check demanding
// an empty directory would false-fail on exactly the unmodified systems this
// tool claims to support (D-026).
func (in *IOInjector) VerifyClean(ctx context.Context, req perturber.VerifyRequest) ([]perturber.Residue, error) {
	targets, err := TargetsFromNodes(req.Nodes)
	if err != nil {
		return nil, err
	}
	// req.OwnershipPrefix is deliberately unused. The other two mechanisms in
	// this family carry no text an ownership tag could be written into (a
	// cgroup limit and an rlimit are numbers) so their ownership story is the
	// BOOT baseline, and io.fill's ownership is already encoded in the ballast
	// file's NAME, which embeds the same run id the prefix does.
	image := in.env.HelperImage
	if image == "" {
		image = DefaultHelperImage
	}
	helperPresent := dkImageExists(ctx, in.env, image)

	var out []perturber.Residue
	for _, t := range targets {
		id := t.ContainerID
		if live, lerr := ResolveContainerByNode(ctx, in.env, t.NodeID); lerr == nil && live != "" {
			id = live
		}
		st, ierr := dkInspect(ctx, in.env, id)
		if ierr != nil || !st.State.Running {
			// A container that is gone took its writable layer, its cgroup and
			// its process rlimits with it.
			continue
		}

		// Ballast files.
		scan := fmt.Sprintf("ls -1 %s/%s* 2>/dev/null || true", DefaultFillDir, fillPrefix)
		if ls, lerr := dkRun(ctx, in.env, "exec", id, "sh", "-c", scan); lerr == nil {
			for _, leftover := range dkLines(ls) {
				out = append(out, perturber.Residue{
					NodeID:    t.NodeID,
					Mechanism: "disk-ballast",
					FaultID:   fillFaultIDOf(leftover, in.env.RunID),
					Detail:    fmt.Sprintf("ballast file %s still occupies the filesystem", leftover),
				})
			}
		}
		// A target with no shell could never have hosted io.fill, so a failed
		// scan is not evidence of anything.

		bl, known := in.baseline.Node(t.NodeID)
		if known && st.HostConfig.Memory != bl.Memory {
			out = append(out, perturber.Residue{
				NodeID:    t.NodeID,
				Mechanism: "cgroup-memory",
				Detail: fmt.Sprintf("memory limit is %d bytes, baseline %d",
					st.HostConfig.Memory, bl.Memory),
			})
		}
		if known && bl.FDSoft != "" && helperPresent {
			if live, rerr := readFDState(ctx, in.env, image, id); rerr == nil {
				if live.Soft != bl.FDSoft || live.Hard != bl.FDHard {
					out = append(out, perturber.Residue{
						NodeID:    t.NodeID,
						Mechanism: "rlimit-nofile",
						Detail: fmt.Sprintf("PID 1's descriptor limit is %s:%s, baseline %s:%s",
							live.Soft, live.Hard, bl.FDSoft, bl.FDHard),
					})
				}
			}
		}
	}
	return out, nil
}

// fillFaultIDOf recovers the owning fault id from a ballast file name, so a
// residue report names the fault to look at rather than only the file.
func fillFaultIDOf(path, runID string) string {
	i := strings.LastIndex(path, fillPrefix)
	if i < 0 {
		return ""
	}
	rest := path[i+len(fillPrefix):]
	if runID != "" {
		if trimmed, ok := strings.CutPrefix(rest, runID+"-"); ok {
			return trimmed
		}
		return ""
	}
	if j := strings.IndexByte(rest, '-'); j >= 0 {
		return rest[j+1:]
	}
	return rest
}

func (in *IOInjector) build(runID, faultID string, spec schema.FaultSpec, nodes []perturber.Node) (Primitive, error) {
	targets, err := TargetsFromNodes(nodes)
	if err != nil {
		return nil, err
	}
	env := in.env
	if runID != "" {
		env.RunID = runID
	}
	p, err := NewIOFault(PrimitiveRequest{Env: env, Spec: spec, FaultID: faultID, Targets: targets})
	if err != nil {
		return nil, err
	}
	switch v := p.(type) {
	case *MemPressure:
		v.SetBaseline(in.baseline)
	case *FDExhaust:
		v.SetBaseline(in.baseline)
	}
	return p, nil
}

// ---------------------------------------------------------------------------
// small helpers
// ---------------------------------------------------------------------------

// dkRequireFillTools refuses early when the target image cannot run the commands
// io.fill needs, so the failure names the missing tool rather than surfacing as
// an opaque exec error.
func dkRequireFillTools(ctx context.Context, env Env, t Target) error {
	if _, err := dkRun(ctx, env, "exec", t.ContainerID, "sh", "-c",
		"command -v dd >/dev/null && command -v df >/dev/null"); err != nil {
		return Unsupportedf(schema.FaultIOFill,
			"node %s's image has no `sh`, `dd` and `df`, which is how a filesystem is filled from "+
				"outside without modifying the target. A distroless or scratch image cannot host this "+
				"fault; exclude io.fill from `perturber.allow` for it", t.NodeID)
	}
	return nil
}

// dkShellQuote single-quotes a path for `sh -c`.
//
// Every path here is composed from a run id, a fault id and a configured
// directory, none of which is attacker-controlled, but a fault injector that
// interpolates unquoted strings into a shell is one bad config away from being a
// command-injection bug, and not doing that costs four lines.
func dkShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
