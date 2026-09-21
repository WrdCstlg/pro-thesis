package control

import (
	"context"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/WrdCstlg/pro-thesis/internal/driver"
	"github.com/WrdCstlg/pro-thesis/internal/harness"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// ---------------------------------------------------------------------------
// PARALLEL WORLD EXECUTION
//
// Phase 4's budget does not close serially. Measured on this machine: a world
// costs ~30s wall, A.3's probe sweep is 22 worlds, and the escalation budget
// buys ~16 serial rollouts against a root branching factor near 249; the ratio
// at which UCT is provably equivalent to ladder-ordered enumeration and the tree
// is decoration (D-015, OQ-013). Eight concurrent compose projects give 3.8x for
// +3-5% timing distortion, which raises the rollout count to ~62 and is the
// point at which the tree starts to earn its place (D-022).
//
// # The constraint that shapes everything here
//
// This host has a HARD CEILING of about 24 free Docker bridge networks, and the
// KV fixture creates TWO per compose project. So concurrency is not bounded by
// CPU or by memory; it is bounded by an address pool, and the bound is low
// enough to hit. Worse, a LEAKED project holds its pair permanently: a search
// that leaks one world in twelve does not degrade, it dies, with an error about
// address pools that reads like a Docker bug.
//
// Three things follow, and they are the design:
//
//  1. The cap is COMPUTED from the pool, checked BEFORE anything boots, and
//     refuses rather than truncates. A silently reduced concurrency would make
//     the measured 3.8x an unreproducible claim.
//  2. A worker slot is released only when Backend.Down VERIFIED its own work.
//     A slot whose teardown failed is RETIRED (permanently withdrawn) because
//     its networks are gone for the rest of this engine's life and pretending
//     otherwise just moves the failure later.
//  3. When retirement empties the pool, the executor FAILS CLOSED naming the
//     leaked projects and the `docker network rm` that would recover them.
//
// # What makes two worlds independent
//
// Each world gets its own compose PROJECT NAME, its own generated overlay, its
// own band of PUBLISHED HOST PORTS, its own artifact directory, and its own
// seed. The seed is derived from (run seed, ordinal) through the recorder's
// path-keyed derivation, so a world behaves identically whether it is executed
// first or last (D-013, WorldSeedFor). Results are returned sorted by ordinal
// regardless of the order they finished in.
//
// # How a world gets its own host ports
//
// Not by editing the system under test. Compose interpolates `${VAR}` in the
// target's own compose file, and the fixture already parameterises exactly the
// two things that collide: `${KV_PORT_N1:-18081}` for each published port and
// `${KV_PREFIX:-prothesis}` for the network and container names. The harness
// therefore supplies VALUES, per slot, through UpRequest.Env.
//
// Which variable names to set is a property of the target's compose file, not
// something this package can know, so it is a seam: ParallelOptions.WorkerEnv.
// DefaultWorkerEnv emits generic PROTHESIS_* names; ComposeVarEnv maps a slot
// onto a project's own names. For the KV fixture that mapping is
//
//	KV_PREFIX  -> slot.Prefix                (distinct networks and containers)
//	KV_PORT_N1 -> slot.Ports["kv-n1"]        (base + 0)
//	KV_PORT_N2 -> slot.Ports["kv-n2"]        (base + 1)
//	KV_PORT_N3 -> slot.Ports["kv-n3"]        (base + 2)
//
// and it is spelled out in ComposeVarEnv's own documentation and pinned by a
// test, so it cannot rot into a comment nobody checks.
// ---------------------------------------------------------------------------

// Defaults for the network budget, measured on the build machine rather than
// chosen. See harness.BridgeNetworkPool and DECISIONS.md D-017 / OQ-013.
const (
	// DefaultFreeBridgeNetworks is how many bridge networks are typically free
	// on this host at rest: a ceiling of 30 minus the ~6 held by the default
	// bridge and Docker Desktop's own networks.
	DefaultFreeBridgeNetworks = 24

	// DefaultNetworksPerWorld is how many networks one world's compose project
	// creates. TWO for the KV fixture, whose client plane and peer plane are
	// separate networks on purpose: a fault that cuts only the peer plane is
	// what produces the gray failure the fixture exists to demonstrate.
	DefaultNetworksPerWorld = 2

	// DefaultWorkerPortBase is the first host port a worker slot may publish.
	// It sits above the fixture's own 18081-18083 defaults so a parallel run
	// cannot collide with a hand-started `thesis up`.
	DefaultWorkerPortBase = 19000

	// DefaultPortsPerWorker is how many consecutive host ports one slot owns.
	// Generous on purpose: the band is what makes slots non-colliding by
	// construction, and ports are not the scarce resource here.
	DefaultPortsPerWorker = 16
)

// ErrWorkerPoolExhausted is returned when every worker slot has been retired
// because its compose project could not be verified torn down.
//
// It is a distinct, wrapped error so a search loop can tell "this host has run
// out of networks and needs a human" from any other failure. It is NOT
// retryable: the networks are gone until someone removes them.
var ErrWorkerPoolExhausted = errors.New("control: every parallel worker slot has been retired")

// TargetsEnv tells a driver where THIS world's nodes are reachable: a
// comma-separated list of `127.0.0.1:<host port>` in the config's node order.
//
// ADDITIVE, and exported on the same terms as schema.PlanPathEnv (D-021) and
// driver.StdinControlEnv: a driver that does not know the name ignores it, and
// one that does needs no change to `driver.cmd`. It exists because a frozen
// command template cannot carry a value that differs per world, and because a
// driver addressing the wrong cluster is worse than one that fails: it reports
// on a system nobody perturbed.
//
// The fixture's loadgen reads it as the default for --targets, so an explicit
// --targets still wins.
const TargetsEnv = "PROTHESIS_TARGETS"

// NetworkBudget is the Docker bridge-network pool available to a parallel run,
// and how much of it one world consumes.
type NetworkBudget struct {
	// Available is how many bridge networks may still be created. Zero means
	// DefaultFreeBridgeNetworks.
	Available int
	// PerWorld is how many networks a single world's compose project creates.
	// Zero means DefaultNetworksPerWorld.
	PerWorld int
	// Probe, when set, MEASURES the free pool instead of trusting Available.
	// harness.FreeBridgeNetworks is the implementation for the compose backend.
	//
	// A probe that fails does not stop the run: the configured Available is
	// used and the fallback is announced. A probe that SUCCEEDS and reports too
	// few networks does stop it, because that is the exhaustion this exists to
	// catch before it becomes a confusing Docker error mid-search.
	Probe func(context.Context) (int, error)
}

func (b NetworkBudget) available() int {
	if b.Available <= 0 {
		return DefaultFreeBridgeNetworks
	}
	return b.Available
}

func (b NetworkBudget) perWorld() int {
	if b.PerWorld <= 0 {
		return DefaultNetworksPerWorld
	}
	return b.PerWorld
}

// MaxWorkers is how many worlds may run at once inside this budget.
func (b NetworkBudget) MaxWorkers() int { return b.available() / b.perWorld() }

// Explain renders the arithmetic, so a refusal says why rather than only that.
func (b NetworkBudget) Explain() string {
	return fmt.Sprintf("%d free bridge network(s) / %d per world = %d concurrent world(s)",
		b.available(), b.perWorld(), b.MaxWorkers())
}

// WorkerSlot is one concurrent execution lane: a compose project name, a name
// prefix, and a band of host ports that no other lane uses.
//
// It is a VALUE and is safe to copy. Nothing in it is shared with another slot,
// which is the point.
type WorkerSlot struct {
	// Index is the slot number, from 0.
	Index int
	// Project is the compose project name for this lane's worlds.
	Project string
	// Prefix is a short, DNS-safe token for names the target's compose file
	// parameterises (container names, network names).
	Prefix string
	// PortBase is the first host port this lane owns.
	PortBase int
	// Ports maps each declared node id to the host port this lane publishes it
	// on, in the config's own node order.
	Ports map[string]int
	// Nodes is the declared node ids in the config's own order, so anything
	// built from Ports has a stable order a map cannot give it.
	Nodes []string
	// PortSpan is how many consecutive ports the lane owns from PortBase.
	PortSpan int
}

// Targets renders the lane's nodes as `127.0.0.1:<port>` in config order.
//
// Loopback, because container IPs are not routable from a Windows host (D-010),
// so a published port is reachable at 127.0.0.1 and nowhere else.
//
// This is the DRIVER's half of per-world isolation, and it is easy to forget.
// Giving each compose project its own published ports only stops the two
// clusters from colliding; a driver whose targets are baked in then addresses
// whichever cluster owns the default port, or, when no world owns it, nothing
// at all, and the world reports on a system it never touched. That is the
// vacuous pass this project is ranked against, arrived at from a new direction.
func (s WorkerSlot) Targets() []string {
	out := make([]string, 0, len(s.Nodes))
	for _, id := range s.Nodes {
		if p, ok := s.Ports[id]; ok && p > 0 {
			out = append(out, "127.0.0.1:"+strconv.Itoa(p))
		}
	}
	return out
}

// SortedPorts returns the lane's ports ordered by node id, so anything derived
// from them is stable.
func (s WorkerSlot) SortedPorts() []int {
	ids := make([]string, 0, len(s.Ports))
	for id := range s.Ports {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]int, 0, len(ids))
	for _, id := range ids {
		out = append(out, s.Ports[id])
	}
	return out
}

// DefaultWorkerEnv is the generic environment a slot exports.
//
// The names are PROTHESIS_-namespaced so they cannot collide with anything that
// is not ours, and a target compose file that spells them gets per-world ports
// with no further configuration. A target that uses its own names (the KV
// fixture uses KV_PORT_N1) needs ComposeVarEnv instead: that is a fact about
// the target, and guessing at it here would be magic that fails silently.
func DefaultWorkerEnv(slot WorkerSlot) []string {
	env := []string{
		"PROTHESIS_WORKER=" + strconv.Itoa(slot.Index),
		"PROTHESIS_PROJECT=" + slot.Project,
		"PROTHESIS_PREFIX=" + slot.Prefix,
		"PROTHESIS_PORT_BASE=" + strconv.Itoa(slot.PortBase),
		// TargetsEnv is the driver's half. See WorkerSlot.Targets.
		TargetsEnv + "=" + strings.Join(slot.Targets(), ","),
	}
	ids := make([]string, 0, len(slot.Ports))
	for id := range slot.Ports {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		env = append(env, "PROTHESIS_PORT_"+envSafe(id)+"="+strconv.Itoa(slot.Ports[id]))
	}
	return env
}

// ComposeVarEnv maps a slot onto the variable names a particular project's
// compose file already uses, in ADDITION to the generic ones.
//
// prefixVar receives WorkerSlot.Prefix; portVars maps a node id from
// prothesis.yaml to the compose variable that carries its published host port.
// Either may be empty.
//
// For testdata/kvfixture, whose docker-compose.yaml already parameterises both:
//
//	ComposeVarEnv("KV_PREFIX", map[string]string{
//	    "kv-n1": "KV_PORT_N1",
//	    "kv-n2": "KV_PORT_N2",
//	    "kv-n3": "KV_PORT_N3",
//	})
//
// A node id in portVars that the slot did not allocate a port for is SKIPPED
// rather than exported empty: `${KV_PORT_N1:-18081}` with KV_PORT_N1 set to the
// empty string publishes port "" and compose fails with a parse error, whereas
// an unset variable correctly falls back to the file's own default.
func ComposeVarEnv(prefixVar string, portVars map[string]string) func(WorkerSlot) []string {
	// Copied so a caller mutating its map afterwards cannot change what a
	// running search injects.
	vars := make(map[string]string, len(portVars))
	for k, v := range portVars {
		vars[k] = v
	}
	return func(slot WorkerSlot) []string {
		env := DefaultWorkerEnv(slot)
		if prefixVar != "" {
			env = append(env, prefixVar+"="+slot.Prefix)
		}
		ids := make([]string, 0, len(vars))
		for id := range vars {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			port, ok := slot.Ports[id]
			if !ok || port <= 0 {
				continue
			}
			env = append(env, vars[id]+"="+strconv.Itoa(port))
		}
		return env
	}
}

func envSafe(s string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(s) {
		switch {
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// ParallelOptions configures concurrent world execution.
type ParallelOptions struct {
	// Workers is how many worlds run at once. Zero means one.
	Workers int
	// Networks is the bridge-network budget the worker count is checked against.
	Networks NetworkBudget
	// PortBase and PortsPerWorker define each slot's band of host ports. Zero
	// means the defaults.
	PortBase       int
	PortsPerWorker int
	// WorkerEnv maps a slot onto the environment its compose project needs. Nil
	// means DefaultWorkerEnv.
	WorkerEnv func(WorkerSlot) []string
	// VerifyPortsFree probes each slot's ports before the run and refuses if any
	// is already bound. Default true.
	//
	// It exists so an occupied port is reported as an occupied port. Left to
	// compose it surfaces as "Bind for 127.0.0.1:19001 failed: port is already
	// allocated" from inside a world that has already burned a boot, and under
	// concurrency it is not obvious which world said it.
	VerifyPortsFree *bool
}

func (o ParallelOptions) workers() int {
	if o.Workers <= 0 {
		return 1
	}
	return o.Workers
}

func (o ParallelOptions) portBase() int {
	if o.PortBase <= 0 {
		return DefaultWorkerPortBase
	}
	return o.PortBase
}

func (o ParallelOptions) portsPerWorker() int {
	if o.PortsPerWorker <= 0 {
		return DefaultPortsPerWorker
	}
	return o.PortsPerWorker
}

func (o ParallelOptions) verifyPorts() bool {
	if o.VerifyPortsFree == nil {
		return true
	}
	return *o.VerifyPortsFree
}

// Validate refuses a configuration that cannot work, BEFORE anything boots.
//
// The network check is the load-bearing one and it fails CLOSED: asking for more
// concurrency than the pool supports is refused with the arithmetic, not
// silently reduced. Silently reducing it would make "8 projects give 3.8x" an
// unreproducible claim, and would move the real failure to whichever world
// happened to be the one that exhausted the pool.
func (o ParallelOptions) Validate() error {
	w := o.workers()
	maxW := o.Networks.MaxWorkers()
	if maxW < 1 {
		return fmt.Errorf("control: the Docker bridge-network pool has no room for even one world (%s); "+
			"free some with `docker network prune` or raise NetworkBudget.Available if this host's "+
			"default-address-pools were widened", o.Networks.Explain())
	}
	if w > maxW {
		return fmt.Errorf("control: %d concurrent worlds need %d bridge networks but only %s; "+
			"lower Workers to %d or free networks with `docker network prune`",
			w, w*o.Networks.perWorld(), o.Networks.Explain(), maxW)
	}
	if o.portsPerWorker() < 1 {
		return fmt.Errorf("control: PortsPerWorker must be positive")
	}
	last := o.portBase() + w*o.portsPerWorker() - 1
	if last > 65535 {
		return fmt.Errorf("control: %d workers from port %d would need port %d, above 65535",
			w, o.portBase(), last)
	}
	return nil
}

// WorldRequest names one world to execute.
type WorldRequest struct {
	// Ordinal is the world's 1-based identity. It fixes the seed and the
	// artifact directory, so the same ordinal is the same world however it was
	// scheduled. Requests must carry distinct ordinals.
	Ordinal int
	// Faults is this world's PLANNED schedule, in the canonical fault grammar.
	// Nil means the runner's own (a plain `thesis run --fault ...`).
	Faults []string
	// Label is a free-form tag carried through to the outcome, so a search can
	// say which ladder rung or tree node a world came from without keeping a
	// side table.
	Label string

	// Seed PINS this world's seed instead of deriving it from (run seed,
	// ordinal). Nil keeps WorldSeedFor, which is what every search world uses.
	//
	// ADDITIVE, and it exists for exactly one caller: REPLAY. A `.thesis` file
	// records the world's own seed (invariant I2 puts it first in the tuple), and
	// WorldSeedFor is an HMAC over the run seed and the ordinal (D-013/D-044),
	// so there is no run seed that regenerates a recorded world seed, and no
	// ordinal either. Without this field `thesis replay` would execute the
	// recorded FAULT SCHEDULE under a DIFFERENT seed, which drives a different
	// workload through the same faults and is not the world the file names.
	//
	// It is deliberately a POINTER. Zero is a legal seed (schema.World says so),
	// so a plain uint64 could not distinguish "pin zero" from "derive".
	Seed *uint64

	// Plan overrides the driver plan for this world. When non-nil, its explicit
	// operations are executed instead of profile generation. Used by Phase 5
	// shrinking for workload reduction.
	Plan *driver.Plan
}

// seedFor resolves this request's world seed: the pinned one when given, the
// ordinal-derived one otherwise.
func (r WorldRequest) seedFor(runSeed uint64) uint64 {
	if r.Seed != nil {
		return *r.Seed
	}
	return WorldSeedFor(runSeed, r.Ordinal)
}

// WorldOutcome is one world's result, in the shape a search ranks worlds by.
type WorldOutcome struct {
	Ordinal int
	Seed    uint64
	Label   string
	// Slot is the worker lane that executed it, or -1 for a serial world.
	Slot int
	// Project is the compose project it ran in.
	Project string
	// HostPorts is the published host port each node was OBSERVED on. It is
	// what proves two concurrent worlds were isolated rather than merely asked
	// to be, and it is how Phase 4's OBSERVE reaches a node.
	HostPorts map[string]int64
	Outcome   Outcome
	// Findings is EVERY oracle's result, not only the violations; the same rule
	// result.json follows (D-034), and for the same reason: a utility function
	// that cannot tell "checked and satisfied" from "never ran" is scoring noise.
	Findings []OracleResult
	Phases   schema.PhaseTimings
	Paths    WorldPaths
	Planned  []string
	Realized []schema.RealizedFault
	// Drain is what QUIESCE's drain did. A search reading Outcome==pass is
	// entitled to know whether the operations behind it were allowed to retire.
	Drain DrainReport
	// Duration is the world's wall-clock cost, which A.6's utility function
	// charges for.
	Duration time.Duration
	// Err is the world's own execution error, if any. It never carries the
	// outcome: that is Outcome's job.
	Err error
	// TeardownErr is non-nil when this world's compose project could not be
	// verified destroyed. Its slot has been retired.
	TeardownErr error

	// KeptUp reports that the topology was deliberately left standing and the
	// up -> down handoff persisted, so `thesis down` can still remove it. Only
	// Runner.RunWorldKeepUp can produce it; the parallel executor never does.
	KeptUp bool
}

// Violated reports whether any oracle in this world reported a violation.
func (w WorldOutcome) Violated() bool {
	for _, f := range w.Findings {
		if f.Violated() {
			return true
		}
	}
	return false
}

// ParallelRunner executes worlds concurrently across isolated compose projects.
type ParallelRunner struct {
	runner *Runner
	opts   ParallelOptions
	slots  []WorkerSlot
	pool   *slotPool
}

// Parallel prepares concurrent execution for this runner.
//
// It validates against the network budget and allocates every slot's ports
// UP FRONT, so a configuration that cannot work is refused before a single
// container exists.
func (r *Runner) Parallel(opts ParallelOptions) (*ParallelRunner, error) {
	if err := opts.Validate(); err != nil {
		return nil, err
	}
	slots := make([]WorkerSlot, 0, opts.workers())
	for i := 0; i < opts.workers(); i++ {
		slots = append(slots, r.newSlot(opts, i))
	}
	return &ParallelRunner{
		runner: r,
		opts:   opts,
		slots:  slots,
		pool:   newSlotPool(len(slots)),
	}, nil
}

// Slots returns the allocated lanes, for reporting and for tests.
func (p *ParallelRunner) Slots() []WorkerSlot {
	return append([]WorkerSlot(nil), p.slots...)
}

func (r *Runner) newSlot(opts ParallelOptions, i int) WorkerSlot {
	base := opts.portBase() + i*opts.portsPerWorker()
	slot := WorkerSlot{
		Index:    i,
		Prefix:   fmt.Sprintf("%s-w%02d", shortRunToken(r.runID), i),
		PortBase: base,
		PortSpan: opts.portsPerWorker(),
		Ports:    map[string]int{},
	}
	// The project name follows D-022's `thesis-<run_id>-w<N>` shape, sanitized
	// the way compose requires.
	slot.Project = sanitizeComposeName(fmt.Sprintf("thesis-%s-w%02d", r.runID, i))
	for j, n := range r.cfg.Harness.Nodes {
		if j >= opts.portsPerWorker() {
			// More nodes than the band has room for. Refusing here would be
			// wrong (a topology can legitimately declare nodes that publish no
			// port) but silently reusing another slot's port would not be, so
			// the extras simply get none and the compose file's own default
			// applies. Validate's PortsPerWorker check is where a caller is
			// told to widen the band.
			break
		}
		slot.Ports[n.ID] = base + j
		slot.Nodes = append(slot.Nodes, n.ID)
	}
	return slot
}

// shortRunToken reduces a run id to a compact DNS-safe token, for names that go
// into container and network names and therefore into DNS labels.
func shortRunToken(runID string) string {
	s := sanitizeComposeName(runID)
	if len(s) > 12 {
		s = s[len(s)-12:]
	}
	s = strings.Trim(s, "-_")
	if s == "" {
		s = "run"
	}
	return s
}

func sanitizeComposeName(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-_")
	if out == "" {
		out = "thesis"
	}
	if len(out) > 48 {
		out = strings.Trim(out[:48], "-_")
	}
	return out
}

// Run executes every request, at most Workers at a time, and returns the
// outcomes SORTED BY ORDINAL.
//
// Deterministic ordering is not cosmetic. Worlds complete out of order (a
// probe world is much cheaper than a compound-fault one) and a search that
// ranked or reported in completion order would produce a different answer on a
// machine with a different load, from the same seed. Sorting on the way out
// makes the schedule an implementation detail (A.8's determinism requirement).
func (p *ParallelRunner) Run(ctx context.Context, reqs []WorldRequest) ([]WorldOutcome, error) {
	if len(reqs) == 0 {
		return nil, nil
	}
	if err := p.checkOrdinals(reqs); err != nil {
		return nil, err
	}
	if err := p.checkNetworkBudget(ctx); err != nil {
		return nil, err
	}
	if p.opts.verifyPorts() {
		if err := p.checkPortsFree(); err != nil {
			return nil, err
		}
	}

	out := make([]WorldOutcome, len(reqs))
	var wg sync.WaitGroup
	var firstFatal error
	var fatalOnce sync.Once

	for i, req := range reqs {
		if ctx.Err() != nil {
			out[i] = WorldOutcome{
				Ordinal: req.Ordinal, Label: req.Label, Slot: -1,
				Outcome: OutcomeCanceled, Err: ctx.Err(),
			}
			continue
		}

		slotIdx, err := p.pool.acquire(ctx)
		if err != nil {
			// Pool exhaustion is FATAL to the batch, not to one world. Every
			// remaining world would fail the same way, and each attempt costs a
			// boot. Say so once, name the leaked projects, and stop.
			fatalOnce.Do(func() { firstFatal = err })
			out[i] = WorldOutcome{
				Ordinal: req.Ordinal, Label: req.Label, Slot: -1,
				Outcome: OutcomeHarnessError, Err: err,
			}
			continue
		}

		wg.Add(1)
		go func(idx int, req WorldRequest, slotIdx int) {
			defer wg.Done()
			res := p.runOne(ctx, req, p.slots[slotIdx])
			// Release ONLY on a verified teardown. A slot whose project survived
			// is retired: its networks are gone for good, and handing it to the
			// next world would boot a project on top of a live one.
			if res.TeardownErr != nil {
				p.pool.retire(slotIdx, res.Project, res.TeardownErr)
			} else {
				p.pool.release(slotIdx)
			}
			out[idx] = res
		}(i, req, slotIdx)
	}
	wg.Wait()

	sort.SliceStable(out, func(i, j int) bool { return out[i].Ordinal < out[j].Ordinal })
	return out, firstFatal
}

func (p *ParallelRunner) checkOrdinals(reqs []WorldRequest) error {
	seen := map[int]bool{}
	for _, r := range reqs {
		if r.Ordinal < 1 {
			return fmt.Errorf("control: world ordinal %d is not 1-based; the ordinal fixes the "+
				"world's seed and artifact directory and cannot be zero", r.Ordinal)
		}
		if seen[r.Ordinal] {
			return fmt.Errorf("control: world ordinal %d appears twice; two worlds sharing an "+
				"ordinal would share a seed and overwrite each other's artifacts", r.Ordinal)
		}
		seen[r.Ordinal] = true
	}
	return nil
}

// checkNetworkBudget measures the pool when a probe is configured.
//
// A probe FAILURE falls back to the configured budget and says so: refusing to
// run because `docker network ls` did not answer would turn a transient daemon
// hiccup into a stopped search. A probe SUCCESS that reports too few networks is
// fatal, which is the whole reason to measure.
func (p *ParallelRunner) checkNetworkBudget(ctx context.Context) error {
	probe := p.opts.Networks.Probe
	if probe == nil {
		return nil
	}
	free, err := probe(ctx)
	if err != nil {
		fmt.Fprintf(p.runner.stderr,
			"thesis: could not measure the bridge-network pool (%v); assuming the configured %s\n",
			err, p.opts.Networks.Explain())
		return nil
	}
	need := p.opts.workers() * p.opts.Networks.perWorld()
	if free < need {
		return fmt.Errorf("control: %d concurrent worlds need %d bridge networks and this engine "+
			"has room for %d; lower Workers to %d, or free networks with `docker network prune`",
			p.opts.workers(), need, free, free/p.opts.Networks.perWorld())
	}
	return nil
}

// checkPortsFree refuses to start when a slot's published ports are taken.
func (p *ParallelRunner) checkPortsFree() error {
	for _, s := range p.slots {
		ids := make([]string, 0, len(s.Ports))
		for id := range s.Ports {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			port := s.Ports[id]
			if err := portFree(port); err != nil {
				return fmt.Errorf("control: worker slot %d wants host port %d for node %q, "+
					"and it is already in use (%v); a previous run may still be up — try "+
					"`thesis down` — or move the band with ParallelOptions.PortBase",
					s.Index, port, id, err)
			}
		}
	}
	return nil
}

// portFree reports whether 127.0.0.1:port can be bound right now.
//
// Loopback only, because that is where the fixture publishes and where the
// health probes connect: container IPs are not routable from a Windows host
// (D-010), so 127.0.0.1 is the only address a published port is reachable on.
func portFree(port int) error {
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return err
	}
	return ln.Close()
}

// runOne executes a single request in a slot.
func (p *ParallelRunner) runOne(ctx context.Context, req WorldRequest, slot WorkerSlot) WorldOutcome {
	r := p.runner
	faults := req.Faults
	if faults == nil {
		faults = r.faults
	}

	envFn := p.opts.WorkerEnv
	if envFn == nil {
		envFn = DefaultWorkerEnv
	}

	expect := make(map[string]int64, len(slot.Ports))
	for id, port := range slot.Ports {
		expect[id] = int64(port)
	}

	spec := worldSpec{
		ordinal: req.Ordinal,
		seed:    req.seedFor(r.seed),
		paths:   NewWorldPaths(filepath.Join(r.runDir, WorldDirName(req.Ordinal))),
		faults:  append([]string(nil), faults...),
		plan:    req.Plan,
		env:     envFn(slot),
		project: slot.Project,
		overlay: filepath.Join(r.runDir, WorldDirName(req.Ordinal), harness.OverlayName),
		slot:    slot.Index,
		// What the driver will be told, so BOOT can check the harness actually
		// published it. WorkerEnv decides whether the compose file learns about
		// this band; if it does not, the two disagree and every operation fails.
		expectPorts: expect,
	}

	if !r.quiet {
		fmt.Fprintf(r.stderr, "thesis: world %d on slot %d (project %s, seed %d)\n",
			spec.ordinal, slot.Index, slot.Project, spec.seed)
	}

	start := time.Now()
	wres, err := r.runWorld(ctx, spec)
	elapsed := time.Since(start)

	return WorldOutcome{
		Ordinal:     req.Ordinal,
		Seed:        spec.seed,
		Label:       req.Label,
		Slot:        slot.Index,
		Project:     slot.Project,
		HostPorts:   wres.hostPorts,
		Outcome:     wres.outcome,
		Findings:    wres.findings,
		Phases:      wres.phases,
		Paths:       wres.paths,
		Planned:     wres.planned,
		Realized:    wres.realized,
		Drain:       wres.drain,
		Duration:    elapsed,
		Err:         err,
		TeardownErr: wres.teardownErr,
	}
}

// ---------------------------------------------------------------------------
// slot pool
// ---------------------------------------------------------------------------

type retirement struct {
	slot    int
	project string
	err     error
}

// slotPool hands out worker lanes and refuses to hand back a leaked one.
type slotPool struct {
	mu       sync.Mutex
	cond     *sync.Cond
	free     []int
	retired  []retirement
	total    int
	inflight int
}

func newSlotPool(n int) *slotPool {
	p := &slotPool{total: n}
	p.cond = sync.NewCond(&p.mu)
	for i := 0; i < n; i++ {
		p.free = append(p.free, i)
	}
	return p
}

// acquire blocks until a lane is free, or fails closed when none can ever be.
func (p *slotPool) acquire(ctx context.Context) (int, error) {
	// A waiter must be woken when the context is cancelled; sync.Cond has no
	// select, so a watchdog broadcasts on cancellation.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			p.cond.Broadcast()
		case <-stop:
		}
	}()

	p.mu.Lock()
	defer p.mu.Unlock()
	for {
		if len(p.free) > 0 {
			i := p.free[0]
			p.free = p.free[1:]
			p.inflight++
			return i, nil
		}
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		// No free lane and nothing running that could free one: every lane has
		// been retired. This is the fail-closed path.
		if p.inflight == 0 {
			return 0, p.exhaustedLocked()
		}
		p.cond.Wait()
	}
}

func (p *slotPool) release(i int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.inflight--
	p.free = append(p.free, i)
	sort.Ints(p.free)
	p.cond.Broadcast()
}

// retire permanently withdraws a lane whose compose project could not be
// verified destroyed.
func (p *slotPool) retire(i int, project string, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.inflight--
	p.retired = append(p.retired, retirement{slot: i, project: project, err: err})
	p.cond.Broadcast()
}

// Retired reports the lanes withdrawn so far, for a report.
func (p *slotPool) Retired() []retirement {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]retirement(nil), p.retired...)
}

// exhaustedLocked is the fail-closed message.
//
// It says "could not be VERIFIED destroyed" rather than "leaked", and the
// distinction is not hedging. Backend.Down returns an error both when it found
// residue it could not remove AND when it could not look: a dead daemon, most
// often the same outage that made the world fail in the first place. Asserting a
// leak in the second case would send an operator hunting for networks that do
// not exist. Either way the slot is unusable: booting a project on top of a
// state nobody could inspect is exactly the confusing mid-search Docker error
// this refusal exists to replace.
func (p *slotPool) exhaustedLocked() error {
	var b strings.Builder
	fmt.Fprintf(&b, "%v: none of the %d slot(s) could be VERIFIED torn down, so up to %d "+
		"bridge network(s) may still be held",
		ErrWorkerPoolExhausted, p.total, len(p.retired)*DefaultNetworksPerWorld)
	for _, r := range p.retired {
		fmt.Fprintf(&b, "\n  slot %d: project %s: %v", r.slot, r.project, r.err)
	}
	b.WriteString("\n  recover with: docker network ls --filter label=com.docker.compose.project " +
		"and `docker network rm` the survivors, or `docker network prune`")
	return fmt.Errorf("%s: %w", b.String(), ErrWorkerPoolExhausted)
}
