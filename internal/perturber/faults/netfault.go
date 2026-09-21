package faults

import (
	"context"
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
// Seeding
// ---------------------------------------------------------------------------

// SeedStreamPath is the derivation path for netem seeds.
//
// It is path-keyed through recorder.DeriveKey rather than drawn sequentially
// from a registry stream, which is what makes it safe to add: a stream's key is
// a function of its path alone, so introducing this domain cannot change a
// single value any existing domain produces for the same root seed (D-013).
// The consequence is that the seeds do not appear in recorder.Streams.Audit();
// they are recorded per fault in the realized ledger instead, which is the
// artifact a replay actually reads.
const SeedStreamPath = "perturber.net.netem"

// Seeder derives the netem seed for one (fault, node, interface).
type Seeder func(faultID, nodeID, iface string) uint64

// NewSeeder returns the default path-keyed seeder for a world seed.
func NewSeeder(worldSeed uint64) Seeder {
	return func(faultID, nodeID, iface string) uint64 {
		k, err := recorder.DeriveKey(recorder.Seed(worldSeed), SeedStreamPath, faultID, nodeID, iface)
		if err != nil {
			// The path segments are validated identifiers, so this cannot
			// happen; degrade to a seed-derived value rather than panicking
			// inside an injection.
			return worldSeed | 1
		}
		v := binary.BigEndian.Uint64(k[:8])
		if v == 0 {
			// netem treats a missing seed as "choose randomly". Zero is
			// accepted and stored, but it reads like an absent seed in a
			// `tc qdisc show` dump, so it is avoided for legibility.
			v = 1
		}
		return v
	}
}

// ---------------------------------------------------------------------------
// Injector
// ---------------------------------------------------------------------------

// Options configures an Injector.
type Options struct {
	// RunID supplies half the ownership tag `thesis:<run_id>:<fault_id>` that
	// withdrawal and residual verification match on (D-026).
	RunID string
	// DockerBin overrides the docker executable. Empty means "docker".
	DockerBin string
	// WorldSeed is what netem seeds derive from. It is the WORLD's seed, not
	// the run's: a world file must be self-contained, so its randomness cannot
	// depend on its ordinal within some run (D-025).
	WorldSeed uint64
	// Nodes is the whole topology, not just the nodes a fault acts on: the far
	// side of a partition is the complement, so the injector has to know every
	// node to compute it.
	Nodes []NetNode
	// Baselines is the BOOT snapshot. Required before injecting: without it
	// there is nothing to judge residue against, and shaping cannot check that
	// it is safe to take over an interface's root qdisc.
	Baselines *BaselineSet
	// Runner executes docker. Nil means ExecRunner.
	Runner CommandRunner
	// Image is the sidecar image. Empty means SidecarImage.
	Image string
	// SidecarTimeout bounds one sidecar invocation.
	SidecarTimeout time.Duration
	// Seeder overrides netem seed derivation.
	Seeder Seeder
	// Now overrides the wall clock, for tests.
	Now func() int64
}

// Injector injects and withdraws the net.* family.
//
// It is safe for concurrent use. Faults on different nodes inject in parallel;
// faults touching the same node serialize, because slot allocation and the root
// qdisc refcount on one interface are shared state and a lost update there
// leaves an orphaned qdisc that only the baseline diff would ever notice.
type Injector struct {
	docker    string
	runID     string
	worldSeed uint64
	targets   []NetNode
	byID      map[string]NetNode
	byCID     map[string]string
	sidecar   *Sidecar
	cmdRunner CommandRunner
	seeder    Seeder
	now       func() int64

	mu        sync.Mutex
	baselines *BaselineSet
	nodeLocks map[string]*sync.Mutex
	slots     map[string]map[int]string
	ledger    []Realized
}

// NewInjector builds an Injector.
func NewInjector(opts Options) (*Injector, error) {
	if err := checkToken("run id", opts.RunID); err != nil {
		return nil, err
	}
	if len(opts.Nodes) == 0 {
		return nil, errors.New("faults: the network injector needs the whole topology, " +
			"because the far side of a partition is the complement of the target")
	}
	byID := make(map[string]NetNode, len(opts.Nodes))
	byCID := make(map[string]string, len(opts.Nodes))
	for _, t := range opts.Nodes {
		if err := checkToken("node id", t.NodeID); err != nil {
			return nil, err
		}
		if err := checkContainer(t.ContainerID); err != nil {
			return nil, fmt.Errorf("faults: node %s: %w", t.NodeID, err)
		}
		if _, dup := byID[t.NodeID]; dup {
			return nil, fmt.Errorf("faults: duplicate node id %q", t.NodeID)
		}
		byID[t.NodeID] = t
		byCID[t.ContainerID] = t.NodeID
	}

	worldSeed := opts.WorldSeed
	seeder := opts.Seeder
	if seeder == nil {
		seeder = NewSeeder(worldSeed)
	}
	now := opts.Now
	if now == nil {
		now = nowNS
	}

	in := &Injector{
		docker:    opts.DockerBin,
		runID:     opts.RunID,
		worldSeed: worldSeed,
		targets:   append([]NetNode(nil), opts.Nodes...),
		byID:      byID,
		byCID:     byCID,
		baselines: opts.Baselines,
		cmdRunner: opts.Runner,
		seeder:    seeder,
		now:       now,
		nodeLocks: map[string]*sync.Mutex{},
		slots:     map[string]map[int]string{},
	}
	in.sidecar = &Sidecar{
		Image:     opts.Image,
		DockerBin: opts.DockerBin,
		RunID:     opts.RunID,
		Runner:    opts.Runner,
		Timeout:   opts.SidecarTimeout,
	}
	sort.Slice(in.targets, func(i, j int) bool { return in.targets[i].NodeID < in.targets[j].NodeID })
	return in, nil
}

// Sidecar exposes the underlying sidecar runner, so any other family can reuse
// the mechanism without re-deriving it.
func (in *Injector) Sidecar() *Sidecar { return in.sidecar }

// WorldSeed reports the seed netem seeds are derived from.
func (in *Injector) WorldSeed() uint64 { return in.worldSeed }

func (in *Injector) runner() CommandRunner {
	if in.cmdRunner == nil {
		return ExecRunner{}
	}
	return in.cmdRunner
}

func (in *Injector) dockerBin() string {
	if in.docker == "" {
		return "docker"
	}
	return in.docker
}

func (in *Injector) nodeIDForContainer(cid string) string {
	if id, ok := in.byCID[cid]; ok {
		return id
	}
	return cid
}

func (in *Injector) lockNodes(ids []string) func() {
	sorted := append([]string(nil), ids...)
	sort.Strings(sorted)
	in.mu.Lock()
	locks := make([]*sync.Mutex, 0, len(sorted))
	for _, id := range sorted {
		l, ok := in.nodeLocks[id]
		if !ok {
			l = &sync.Mutex{}
			in.nodeLocks[id] = l
		}
		locks = append(locks, l)
	}
	in.mu.Unlock()
	// Acquired in id order, which is a total order over all callers, so two
	// faults touching overlapping node sets cannot deadlock against each other.
	for _, l := range locks {
		l.Lock()
	}
	return func() {
		for i := len(locks) - 1; i >= 0; i-- {
			locks[i].Unlock()
		}
	}
}

func slotKey(nodeID, iface string) string { return nodeID + "\x00" + iface }

// claimSlot reserves the lowest free shaping slot on an interface and reports
// whether the caller must also install the root qdisc.
func (in *Injector) claimSlot(nodeID, iface, faultID string) (slot int, installRoot bool, err error) {
	in.mu.Lock()
	defer in.mu.Unlock()
	k := slotKey(nodeID, iface)
	m := in.slots[k]
	if m == nil {
		m = map[int]string{}
		in.slots[k] = m
	}
	for s := FirstSlot; s <= LastSlot; s++ {
		if _, taken := m[s]; !taken {
			installRoot = len(m) == 0
			m[s] = faultID
			return s, installRoot, nil
		}
	}
	return 0, false, fmt.Errorf("faults: node %s interface %s already carries %d shaping faults, "+
		"which is the prio qdisc's band limit", nodeID, iface, LastSlot-FirstSlot+1)
}

// releaseSlot frees a slot and reports whether the root qdisc should now go.
func (in *Injector) releaseSlot(nodeID, iface string, slot int) (removeRoot bool) {
	in.mu.Lock()
	defer in.mu.Unlock()
	k := slotKey(nodeID, iface)
	m := in.slots[k]
	if m == nil {
		return false
	}
	if _, held := m[slot]; !held {
		return false
	}
	delete(m, slot)
	if len(m) == 0 {
		delete(in.slots, k)
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// BOOT baseline
// ---------------------------------------------------------------------------

// CaptureBaselines snapshots every node's network state. Call it in BOOT,
// before any fault exists.
//
// Failing to capture a node is fatal rather than skipped. A node with no
// baseline can still be injected into, but its residue can never be judged, and
// a run that cannot verify its own cleanup silently poisons every world after
// it (OQ-004).
func (in *Injector) CaptureBaselines(ctx context.Context) (*BaselineSet, error) {
	if err := in.sidecar.EnsureImage(ctx); err != nil {
		return nil, err
	}
	set := &BaselineSet{Schema: BaselineSchema, RunID: in.runID}
	for _, t := range in.targets {
		res, err := in.sidecar.Exec(ctx, t.ContainerID, captureScript)
		if err != nil {
			return nil, fmt.Errorf("faults: baseline for node %s: %w", t.NodeID, err)
		}
		c, err := parseCapture(res.Stdout)
		if err != nil {
			return nil, fmt.Errorf("faults: baseline for node %s: %w", t.NodeID, err)
		}
		set.Nodes = append(set.Nodes, Baseline{
			NodeID:             t.NodeID,
			ContainerID:        t.ContainerID,
			CapturedWallUnixNS: in.now(),
			Ifaces:             c.Ifaces,
			Qdiscs:             c.Qdiscs,
			IPTables:           c.IPTables,
			IP6Tables:          c.IP6Tables,
		})
	}
	sort.Slice(set.Nodes, func(i, j int) bool { return set.Nodes[i].NodeID < set.Nodes[j].NodeID })
	in.mu.Lock()
	in.baselines = set
	in.mu.Unlock()
	return set, nil
}

// Baselines returns the snapshot in force.
func (in *Injector) Baselines() *BaselineSet {
	in.mu.Lock()
	defer in.mu.Unlock()
	return in.baselines
}

// SetBaselines installs a snapshot read back from the run bundle, so `thesis
// down` can verify residue in a process that never ran BOOT.
func (in *Injector) SetBaselines(set *BaselineSet) {
	in.mu.Lock()
	in.baselines = set
	in.mu.Unlock()
}

// ---------------------------------------------------------------------------
// NetFault
// ---------------------------------------------------------------------------

// NetRequest describes one network fault to inject.
type NetRequest struct {
	// FaultID identifies this fault occurrence within the run. It becomes half
	// the ownership tag, so it must be unique within the run.
	FaultID string
	// Spec is the parsed fault. pkg/schema owns the grammar; this package
	// consumes it.
	Spec schema.FaultSpec
	// Resolution binds a dynamic target to concrete nodes. Leave it zero for a
	// node, wildcard or edge target and ResolveStatic will do it; supply it for
	// role: and quorum targets, which need live cluster state.
	Resolution Resolution
}

type faultState int

const (
	statePlanned faultState = iota
	stateInjected
	stateWithdrawn
)

// SeedRecord is one netem seed, recorded so a replay installs the same one.
type SeedRecord struct {
	NodeID string `json:"node_id"`
	Iface  string `json:"iface"`
	Slot   int    `json:"slot"`
	Seed   uint64 `json:"seed"`
}

// Realized is the ledger entry for one injected network fault.
//
// It is richer than schema.RealizedFault on purpose. The world file records
// what a REPLAY needs; this records what a HUMAN needs in order to understand
// what physically happened: the exact commands, the seeds, and every place the
// mechanism had to deviate from the literal reading of the grammar.
type Realized struct {
	FaultID     string       `json:"fault_id"`
	Planned     string       `json:"planned"`
	Resolved    string       `json:"resolved"`
	Nodes       []string     `json:"nodes"`
	Peers       []string     `json:"peers"`
	Tag         string       `json:"tag"`
	StartWallNS int64        `json:"start_wall_unix_ns"`
	EndWallNS   int64        `json:"end_wall_unix_ns"`
	Seeds       []SeedRecord `json:"seeds,omitempty"`
	Commands    []string     `json:"commands"`
	Withdrawal  []string     `json:"withdrawal"`
	Notes       []string     `json:"notes,omitempty"`
}

// LedgerFile is the bundle-relative path the realized ledger is stored at.
const LedgerFile = "perturber/net_realized.json"

// nodePlan is one node's share of a fault.
type nodePlan struct {
	nodeID    string
	container string
	inject    []string
	withdraw  []string
	slots     []slotRef
	seeds     []SeedRecord
}

type slotRef struct {
	iface string
	slot  int
	// rootInstalled records that this plan is the one that put the root qdisc
	// on the interface. It is informational: whether the root goes on
	// withdrawal is decided by the live refcount, not by who installed it,
	// because faults withdraw in an order nobody controls.
	rootInstalled bool
}

// NetFault is one injected, withdrawable network fault. It satisfies Fault.
type NetFault struct {
	in    *Injector
	id    string
	tag   string
	spec  schema.FaultSpec
	res   Resolution
	peers []string

	mu          sync.Mutex
	state       faultState
	plans       []nodePlan
	seeds       []SeedRecord
	notes       []string
	injectedNS  int64
	withdrawnNS int64
}

// Kind is the fault kind this injects.
func (f *NetFault) Kind() schema.FaultKind { return f.spec.Kind }

// Spec is the parsed planned fault.
func (f *NetFault) Spec() schema.FaultSpec { return f.spec }

// FaultID is the per-fault ownership id.
func (f *NetFault) FaultID() string { return f.id }

// Tag returns the ownership tag carried by every rule this fault installs.
func (f *NetFault) Tag() string { return f.tag }

// Nodes are the concrete nodes the fault acts on.
func (f *NetFault) Nodes() []NetNode {
	out := make([]NetNode, 0, len(f.res.Selected))
	for _, id := range f.res.Selected {
		out = append(out, f.in.byID[id])
	}
	return out
}

// Peers reports the nodes on the far side of the cut.
func (f *NetFault) Peers() []string { return append([]string(nil), f.peers...) }

// Active reports whether the fault is currently applied.
func (f *NetFault) Active() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state == stateInjected
}

// Window reports the wall-clock nanoseconds at which injection and withdrawal
// actually happened. ok is false until the fault has been injected.
func (f *NetFault) Window() (startNS, endNS int64, ok bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.state == statePlanned || f.injectedNS == 0 {
		return 0, 0, false
	}
	end := f.withdrawnNS
	if end == 0 {
		end = f.injectedNS
	}
	return f.injectedNS, end, true
}

// RealizedFault renders the world file's `fault_schedule.realized` entry.
//
// The timeline supplies the DRIVE origin. A fault injected before DRIVE, or a
// timeline with no origin, yields ok=false rather than a plausible-looking
// zero: a fault misplaced at t+0 would make the causal timeline lie about
// ordering, which is the one thing it exists to establish (D-011).
func (f *NetFault) RealizedFault(tl *recorder.Timeline) (schema.RealizedFault, bool) {
	startNS, endNS, ok := f.Window()
	if !ok || tl == nil {
		return schema.RealizedFault{}, false
	}
	start, err := tl.VMSFromEpochNS(startNS)
	if err != nil {
		return schema.RealizedFault{}, false
	}
	end, err := tl.VMSFromEpochNS(endNS)
	if err != nil {
		return schema.RealizedFault{}, false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return schema.RealizedFault{
		Fault:    f.spec.String(),
		Resolved: f.resolvedStringLocked(),
		Nodes:    append([]string(nil), f.res.Selected...),
		StartMS:  start,
		EndMS:    end,
	}, true
}

// ErrNotNetworkFault is returned for a kind this injector does not serve.
//
// It is a sentinel rather than a plain error so a scheduler can tell "this
// family cannot carry the fault out" apart from "the injection failed". The
// two must never be conflated: the first is a routing decision, the second is a
// world that did not get the fault it was supposed to get.
var ErrNotNetworkFault = errors.New("faults: not a net.* fault kind")

// NewNetFault validates a request and prepares an injectable fault. It performs
// no I/O.
func (in *Injector) NewNetFault(req NetRequest) (*NetFault, error) {
	if !Supports(req.Spec.Kind) {
		return nil, fmt.Errorf("%w: %s; the network injector serves %s",
			ErrNotNetworkFault, req.Spec.Kind, strings.Join(supportedKindNames(), ", "))
	}
	if err := req.Spec.Validate(); err != nil {
		return nil, err
	}
	tag, err := Tag(in.runID, req.FaultID)
	if err != nil {
		return nil, err
	}

	res := req.Resolution
	if len(res.Selected) == 0 {
		res, err = ResolveStatic(req.Spec, in.targets)
		if err != nil {
			return nil, err
		}
	}
	for _, id := range res.Selected {
		if _, ok := in.byID[id]; !ok {
			return nil, fmt.Errorf("faults: %s resolved to node %q, which the topology does not contain",
				req.Spec.Kind, id)
		}
	}
	peers := res.Peers
	if len(peers) == 0 {
		peers = complement(in.targets, res.Selected)
	}
	for _, id := range peers {
		if _, ok := in.byID[id]; !ok {
			return nil, fmt.Errorf("faults: %s names peer %q, which the topology does not contain",
				req.Spec.Kind, id)
		}
	}
	if len(peers) == 0 {
		return nil, fmt.Errorf("faults: %s(%s) resolves to every node in the topology, "+
			"so there is nothing on the other side of the cut; it would be a silent no-op",
			req.Spec.Kind, req.Spec.Target)
	}
	if in.Baselines() == nil {
		return nil, errors.New("faults: no BOOT baseline; capture one before injecting, " +
			"or HEAL has nothing to judge residue against (DECISIONS.md D-026)")
	}

	sel := append([]string(nil), res.Selected...)
	sort.Strings(sel)
	return &NetFault{
		in:    in,
		id:    req.FaultID,
		tag:   tag,
		spec:  req.Spec,
		res:   Resolution{Selected: sel, Peers: res.Peers},
		peers: peers,
	}, nil
}

func supportedKindNames() []string {
	return []string{
		string(schema.FaultNetPartition), string(schema.FaultNetLatency),
		string(schema.FaultNetLoss), string(schema.FaultNetReorder),
		string(schema.FaultNetDuplicate), string(schema.FaultNetBandwidth),
	}
}

// Inject applies the fault.
//
// It is all-or-nothing. A partition applied to two of three nodes is not a
// weaker fault, it is a DIFFERENT one (a three-way split where a two-way was
// asked for) so a partial failure withdraws what did land and reports the
// failure rather than continuing against a topology nobody described.
func (f *NetFault) Inject(ctx context.Context) error {
	f.mu.Lock()
	if f.state != statePlanned {
		f.mu.Unlock()
		return fmt.Errorf("faults: fault %s has already been injected", f.id)
	}
	f.mu.Unlock()

	in := f.in
	unlock := in.lockNodes(f.res.Selected)
	defer unlock()

	if err := in.sidecar.EnsureImage(ctx); err != nil {
		return err
	}

	// Addresses are read for the PEERS only: the selected nodes' own addresses
	// are never needed, because rules and filters name the far end.
	peerCIDs := make([]string, 0, len(f.peers))
	for _, id := range f.peers {
		peerCIDs = append(peerCIDs, in.byID[id].ContainerID)
	}
	addrs, err := in.inspectAddrs(ctx, peerCIDs)
	if err != nil {
		return err
	}
	var peerAddrs []string
	for _, id := range f.peers {
		na, ok := addrs[id]
		if !ok || len(na.Addrs) == 0 {
			return fmt.Errorf("faults: peer %s has no container addresses, "+
				"so %s(%s) would not affect it", id, f.spec.Kind, f.spec.Target)
		}
		peerAddrs = append(peerAddrs, na.Addrs...)
	}
	sort.Strings(peerAddrs)

	plans := make([]nodePlan, 0, len(f.res.Selected))
	var notes []string
	var seeds []SeedRecord
	rollback := func() {
		for _, p := range plans {
			for _, s := range p.slots {
				in.releaseSlot(p.nodeID, s.iface, s.slot)
			}
		}
	}

	for _, nodeID := range f.res.Selected {
		var (
			p    nodePlan
			ns   []string
			perr error
		)
		if f.spec.Kind == schema.FaultNetPartition {
			p, perr = f.planPartition(nodeID, peerAddrs)
		} else {
			p, ns, perr = f.planShaping(nodeID, peerAddrs)
		}
		if perr != nil {
			rollback()
			return perr
		}
		notes = append(notes, ns...)
		seeds = append(seeds, p.seeds...)
		plans = append(plans, p)
	}

	// Execute in parallel across nodes. A fault window is a virtual-clock
	// interval the schedule promised to honour, and serialising three ~250 ms
	// sidecars would put half a second of skew inside it.
	errs := make([]error, len(plans))
	var wg sync.WaitGroup
	for i := range plans {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := in.sidecar.Exec(ctx, plans[i].container, script(true, plans[i].inject))
			errs[i] = err
		}(i)
	}
	wg.Wait()

	var firstErr error
	for i, err := range errs {
		if err != nil && firstErr == nil {
			firstErr = fmt.Errorf("faults: inject %s on node %s: %w",
				f.spec.Kind, plans[i].nodeID, err)
		}
	}

	f.mu.Lock()
	f.plans = plans
	f.seeds = seeds
	f.notes = append(f.notes, notes...)
	f.state = stateInjected
	f.injectedNS = in.now()
	f.mu.Unlock()

	if firstErr != nil {
		// Withdraw everything, including the nodes that succeeded. Use the
		// caller's context if it is still alive and a detached one if it is
		// not, because the commonest cause of a partial injection is exactly a
		// context that just expired.
		wctx := ctx
		if ctx.Err() != nil {
			var cancel context.CancelFunc
			wctx, cancel = context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
		}
		// withdrawHeld, not Withdraw: this goroutine already holds the node
		// locks, and Go mutexes are not reentrant. Calling the public method
		// here deadlocks the rollback, which is the one path that must never
		// hang, because it is what stops a half-applied partition from
		// surviving the world.
		if werr := f.withdrawHeld(wctx); werr != nil {
			return fmt.Errorf("%w; and rolling it back failed: %v", firstErr, werr)
		}
		return firstErr
	}
	return nil
}

func (f *NetFault) planPartition(nodeID string, peerAddrs []string) (nodePlan, error) {
	cmds, err := PartitionCommands(f.tag, peerAddrs)
	if err != nil {
		return nodePlan{}, fmt.Errorf("faults: node %s: %w", nodeID, err)
	}
	wcmds, err := WithdrawIPTablesCommands(f.tag)
	if err != nil {
		return nodePlan{}, err
	}
	return nodePlan{
		nodeID:    nodeID,
		container: f.in.byID[nodeID].ContainerID,
		inject:    cmds,
		withdraw:  wcmds,
	}, nil
}

func (f *NetFault) planShaping(nodeID string, peerAddrs []string) (nodePlan, []string, error) {
	in := f.in
	base, ok := in.Baselines().Node(nodeID)
	if !ok {
		return nodePlan{}, nil, fmt.Errorf("faults: node %s has no BOOT baseline, "+
			"so shaping it could not be verified clean afterwards", nodeID)
	}

	byIface := map[string][]string{}
	var notes []string
	for _, a := range peerAddrs {
		iface, ok := ifaceForAddr(base, a)
		if !ok {
			notes = append(notes, fmt.Sprintf(
				"node %s: peer address %s matches no interface prefix recorded at BOOT; not shaped",
				nodeID, a))
			continue
		}
		byIface[iface] = append(byIface[iface], a)
	}
	if len(byIface) == 0 {
		return nodePlan{}, nil, fmt.Errorf("faults: node %s: none of the %d peer address(es) fall inside "+
			"an interface prefix recorded at BOOT, so %s(%s) would shape no traffic at all",
			nodeID, len(peerAddrs), f.spec.Kind, f.spec.Target)
	}

	ifaces := make([]string, 0, len(byIface))
	for i := range byIface {
		ifaces = append(ifaces, i)
	}
	sort.Strings(ifaces)

	p := nodePlan{nodeID: nodeID, container: in.byID[nodeID].ContainerID}
	release := func() {
		for _, s := range p.slots {
			in.releaseSlot(nodeID, s.iface, s.slot)
		}
	}
	for _, iface := range ifaces {
		if err := CheckRootQdiscSafe(nodeID, base, iface); err != nil {
			release()
			return nodePlan{}, nil, err
		}
		slot, installRoot, err := in.claimSlot(nodeID, iface, f.id)
		if err != nil {
			release()
			return nodePlan{}, nil, err
		}
		p.slots = append(p.slots, slotRef{iface: iface, slot: slot, rootInstalled: installRoot})

		seed := in.seeder(f.id, nodeID, iface)
		args, err := NetemArgs(f.spec, seed)
		if err != nil {
			release()
			return nodePlan{}, nil, err
		}
		if installRoot {
			root, err := ShapeRootCommand(iface)
			if err != nil {
				release()
				return nodePlan{}, nil, err
			}
			p.inject = append(p.inject, root)
		}
		slotCmds, err := ShapeSlotCommands(iface, slot, args, byIface[iface])
		if err != nil {
			release()
			return nodePlan{}, nil, err
		}
		p.inject = append(p.inject, slotCmds...)
		p.seeds = append(p.seeds, SeedRecord{NodeID: nodeID, Iface: iface, Slot: slot, Seed: seed})
		if f.spec.Kind == schema.FaultNetReorder {
			notes = append(notes, fmt.Sprintf(
				"node %s %s: netem refuses `reorder` without a delay, so a %d ms enabling delay was added; "+
					"the canonical fault string does not carry it",
				nodeID, iface, ReorderEnableDelayMS))
		}
	}
	return p, notes, nil
}

// Withdraw removes everything the fault installed and verifies that it did.
//
// It is idempotent and safe after a failed injection: withdrawal is expressed
// entirely in terms of the ownership tag and the claimed slots, both of which
// exist whether or not the corresponding rule was ever installed. It attempts
// EVERY node before returning, because HEAL exists to clean up after a partial
// failure and a HEAL that stops at the first error is the one that leaves the
// mess.
func (f *NetFault) Withdraw(ctx context.Context) error {
	unlock := f.in.lockNodes(f.res.Selected)
	defer unlock()
	return f.withdrawHeld(ctx)
}

// withdrawHeld is Withdraw for a caller that already holds this fault's node
// locks. Inject's rollback path is such a caller.
func (f *NetFault) withdrawHeld(ctx context.Context) error {
	f.mu.Lock()
	if f.state == stateWithdrawn {
		f.mu.Unlock()
		return nil
	}
	plans := f.plans
	if plans == nil {
		// Injection never built a plan. There is still a tag to sweep for: an
		// injection script can install some rules before failing.
		for _, nodeID := range f.res.Selected {
			w, err := WithdrawIPTablesCommands(f.tag)
			if err != nil {
				f.mu.Unlock()
				return err
			}
			plans = append(plans, nodePlan{
				nodeID:    nodeID,
				container: f.in.byID[nodeID].ContainerID,
				withdraw:  w,
			})
		}
	}
	f.state = stateWithdrawn
	f.mu.Unlock()

	in := f.in
	errs := make([]error, len(plans))
	ran := make([][]string, len(plans))
	var wg sync.WaitGroup
	for i := range plans {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ran[i], errs[i] = f.withdrawNode(ctx, plans[i])
		}(i)
	}
	wg.Wait()

	f.mu.Lock()
	// The ledger records the commands that ACTUALLY ran, not the ones planned:
	// a shaping fault's slot deletions are only known once the refcount says
	// whether the root goes with them, and a ledger that guessed would be
	// worse than none when someone is reading it to work out what happened.
	for i := range plans {
		if i < len(f.plans) {
			f.plans[i].withdraw = ran[i]
		}
	}
	f.withdrawnNS = in.now()
	f.mu.Unlock()
	in.record(f)

	var msgs []string
	for _, err := range errs {
		if err != nil {
			msgs = append(msgs, err.Error())
		}
	}
	if len(msgs) > 0 {
		return fmt.Errorf("faults: withdraw %s: %s", f.spec.Kind, strings.Join(msgs, "; "))
	}
	return nil
}

func (f *NetFault) withdrawNode(ctx context.Context, p nodePlan) ([]string, error) {
	in := f.in
	cmds := append([]string(nil), p.withdraw...)
	if len(cmds) == 0 {
		w, err := WithdrawIPTablesCommands(f.tag)
		if err != nil {
			return nil, err
		}
		cmds = w
	}
	for _, s := range p.slots {
		del, err := ShapeSlotDeleteCommands(s.iface, s.slot)
		if err != nil {
			return cmds, err
		}
		cmds = append(cmds, del...)
		if in.releaseSlot(p.nodeID, s.iface, s.slot) {
			root, err := ShapeRootDeleteCommand(s.iface)
			if err != nil {
				return cmds, err
			}
			cmds = append(cmds, root+" || true")
		}
	}
	// This fault's own tag, never the shared prefix: it deleted by f.tag, so it
	// may only be held to f.tag. See CountTaggedCommand.
	cmds = append(cmds, CountTaggedCommand(f.tag), "echo '#QDISC'", "tc qdisc show", "echo '#END'")

	// No `set -e`: every deletion must be attempted even after one fails, or a
	// partial withdrawal becomes an unrecoverable one.
	res, err := in.sidecar.Exec(ctx, p.container, script(false, cmds))
	if err != nil {
		if isNotRunning(err.Error()) {
			// The container is gone and its network namespace went with it.
			// Nothing of ours can have survived, so this is a clean withdrawal
			// rather than a failed one.
			return cmds, nil
		}
		return cmds, fmt.Errorf("node %s: %w", p.nodeID, err)
	}
	return cmds, verifyWithdrawal(p, res.Stdout)
}

// verifyWithdrawal reads the self-check appended to every withdrawal script.
//
// Withdrawal verifies itself rather than trusting that its deletes landed,
// because the alternative is discovering the leak at HEAL, one phase later,
// with no idea which fault left it.
func verifyWithdrawal(p nodePlan, out string) error {
	tagged, qdiscs, err := parseWithdrawOutput(out)
	if err != nil {
		return fmt.Errorf("node %s: %w", p.nodeID, err)
	}
	var problems []string
	if tagged > 0 {
		problems = append(problems, fmt.Sprintf("%d tagged iptables rule(s) survived", tagged))
	}
	for _, s := range p.slots {
		h := slotHandle(s.slot)
		for _, q := range qdiscs {
			if strings.Contains(q, " "+h+" ") && strings.Contains(q, "dev "+s.iface) {
				problems = append(problems, fmt.Sprintf("qdisc %s on %s survived", h, s.iface))
				break
			}
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("node %s: %s", p.nodeID, strings.Join(problems, ", "))
	}
	return nil
}

func parseWithdrawOutput(out string) (tagged int, qdiscs []string, err error) {
	section := ""
	sawTagged := false
	for _, raw := range strings.Split(out, "\n") {
		line := strings.TrimSpace(strings.TrimRight(raw, "\r"))
		switch line {
		case "#TAGGED", "#QDISC", "#END":
			section = line
			continue
		}
		if line == "" {
			continue
		}
		switch section {
		case "#TAGGED":
			n, convErr := strconv.Atoi(line)
			if convErr != nil {
				return 0, nil, fmt.Errorf("withdrawal self-check printed %q where a rule count was expected", line)
			}
			tagged = n
			sawTagged = true
		case "#QDISC":
			if n := normalizeQdiscLine(line); n != "" {
				qdiscs = append(qdiscs, n)
			}
		}
	}
	if !sawTagged {
		return 0, nil, errors.New("withdrawal self-check produced no rule count")
	}
	if section != "#END" {
		return 0, nil, errors.New("withdrawal self-check output is truncated")
	}
	return tagged, qdiscs, nil
}

// record appends the fault to the run ledger.
func (in *Injector) record(f *NetFault) {
	f.mu.Lock()
	var cmds, wcmds []string
	for _, p := range f.plans {
		for _, c := range p.inject {
			cmds = append(cmds, p.nodeID+": "+c)
		}
		for _, c := range p.withdraw {
			wcmds = append(wcmds, p.nodeID+": "+c)
		}
	}
	r := Realized{
		FaultID:     f.id,
		Planned:     f.spec.String(),
		Resolved:    f.resolvedStringLocked(),
		Nodes:       append([]string(nil), f.res.Selected...),
		Peers:       append([]string(nil), f.peers...),
		Tag:         f.tag,
		StartWallNS: f.injectedNS,
		EndWallNS:   f.withdrawnNS,
		Seeds:       append([]SeedRecord(nil), f.seeds...),
		Commands:    cmds,
		Withdrawal:  wcmds,
		Notes:       append([]string(nil), f.notes...),
	}
	f.mu.Unlock()

	in.mu.Lock()
	in.ledger = append(in.ledger, r)
	in.mu.Unlock()
}

// resolvedStringLocked renders the fault with its target bound, where the
// grammar can express the binding. The caller holds f.mu.
//
// A single-node resolution becomes a node target, which is exactly what
// `fault_schedule.realized` wants: net.partition(minority(kv)) realized as
// net.partition(kv-n2). A multi-node resolution has no grammar form (the
// frozen TARGET vocabulary has no set literal) so the planned string is kept
// and the concrete nodes travel in the `nodes` list beside it.
func (f *NetFault) resolvedStringLocked() string {
	if len(f.res.Selected) != 1 || f.spec.Target.Kind == schema.TargetNode {
		return f.spec.String()
	}
	bound := f.spec
	bound.Target = schema.Target{Kind: schema.TargetNode, Node: f.res.Selected[0]}
	if err := bound.Validate(); err != nil {
		return f.spec.String()
	}
	return bound.String()
}

// Ledger returns every network fault withdrawn so far, in withdrawal order.
func (in *Injector) Ledger() []Realized {
	in.mu.Lock()
	defer in.mu.Unlock()
	return append([]Realized(nil), in.ledger...)
}

// EncodeLedger renders the ledger as deterministic JSON for the run bundle.
func (in *Injector) EncodeLedger() ([]byte, error) {
	type doc struct {
		Schema string     `json:"schema"`
		RunID  string     `json:"run_id"`
		Faults []Realized `json:"faults"`
	}
	l := in.Ledger()
	if l == nil {
		l = []Realized{}
	}
	return encodeJSON(doc{Schema: LedgerSchema, RunID: in.runID, Faults: l})
}

// LedgerSchema versions the realized ledger. ADDITIVE.
const LedgerSchema = "prothesis.net_realized/v1"
