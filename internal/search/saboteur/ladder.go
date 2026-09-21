package saboteur

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/WrdCstlg/pro-thesis/internal/perturber"
	"github.com/WrdCstlg/pro-thesis/internal/perturber/faults"
	"github.com/WrdCstlg/pro-thesis/internal/recorder"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// ---------------------------------------------------------------------------
// A.7: the escalation ladder
// ---------------------------------------------------------------------------
//
// The ladder is a PRIOR over the action space: children are expanded in rung
// order, cheapest and highest-signal first, and UCT lifts a higher rung only
// when the lower ones prove uninteresting.
//
// D-015 is why this file is the most carefully written one in the package. At
// Addendum A's stated budgets the tree never revisits a node, so "expand in
// ladder order" IS the search. An action this file declines to generate is an
// experiment the Saboteur will never run, and an action it generates in a shape
// that cannot fire is a world's worth of wall clock (~30 s measured) spent
// proving nothing.

// Rung is one step of A.7's ladder. Lower is cheaper and is tried first.
type Rung int

// The six rungs, verbatim from A.7.
const (
	// RungRecon detects timing-sensitive code paths with small delays.
	RungRecon Rung = iota
	// RungStress pushes retry and timeout logic to its limits.
	RungStress
	// RungGrayFail creates ambiguous liveness with SIGSTOP/SIGCONT: the node
	// looks alive to the orchestrator and dead to its peers. A.7 calls this
	// "the single most productive fault kind in distributed systems testing".
	RungGrayFail
	// RungPartition forces quorum reconfiguration under load.
	RungPartition
	// RungCompound overlaps two faults from rungs 1-3 in time.
	RungCompound
	// RungTemporal attacks lease timers and anything assuming bounded clock
	// drift.
	RungTemporal

	numRungs = iota
)

var rungNames = [numRungs]string{"RECON", "STRESS", "GRAY_FAIL", "PARTITION", "COMPOUND", "TEMPORAL"}

// AllRungs is the ladder in order.
var AllRungs = [numRungs]Rung{RungRecon, RungStress, RungGrayFail, RungPartition, RungCompound, RungTemporal}

func (r Rung) String() string {
	if r < 0 || int(r) >= len(rungNames) {
		return fmt.Sprintf("Rung(%d)", int(r))
	}
	return rungNames[r]
}

// streamSegment is the PRNG path segment for this rung's tie-break stream. It is
// part of the reproducibility contract: renaming one changes every tie-break
// draw for every seed while leaving every world identity unchanged.
func (r Rung) streamSegment() string {
	return strings.ToLower(r.String())
}

// ladderStreamRoot is the first path segment of every ladder stream.
//
// Streams are derived with recorder.MustDeriveKey rather than through
// recorder.Streams.Get because Get's domain registry is a closed list frozen in
// Phase 0, and a stream key is a function of its PATH ALONE (D-013). Deriving at
// a fixed compile-time path is exactly what MustDeriveKey is for (WorldSeed
// already does it) and it means this file cannot perturb a single value drawn
// by any existing stream, so every committed regression world stays valid.
const ladderStreamRoot = "search.ladder"

// LadderStream returns the tie-break stream for one rung under one world seed.
//
// Each rung gets its own path, so the number of draws taken inside one rung can
// never shift another rung's ordering. That independence is the property
// TestRungStreamsAreIndependent pins.
func LadderStream(seed recorder.Seed, r Rung) *recorder.Stream {
	return recorder.NewStream(recorder.MustDeriveKey(seed, ladderStreamRoot, r.streamSegment()))
}

// Action is one candidate expansion: a set of faults to add to the branch.
//
// Rungs 0-3 and 5 produce exactly one fault. Rung 4 produces two, overlapped in
// time.
type Action struct {
	Rung Rung
	// Faults are canonical fault strings, in schedule order. Every one has been
	// through schema.ParseFault and perturber.Compile against the caller's
	// config, allow list, budget, topology and constraints.
	Faults []string
	// Kinds are the fault kinds used, in the same order as Faults.
	Kinds []schema.FaultKind
	// Target is the TARGET token the faults share.
	Target string
	// Note carries an honesty caveat about what the fault physically does, when
	// the grammar's reading and the mechanism's reality differ. Empty when they
	// do not.
	Note string
}

// String renders the action for a log line or a tree node label.
func (a Action) String() string {
	return a.Rung.String() + ": " + strings.Join(a.Faults, " + ")
}

// Skip records an action the ladder did NOT generate, and why.
//
// A rung whose kind is outside perturber.allow, or whose mechanism this platform
// cannot deliver, yields NOTHING rather than an error: a search must not die
// mid-flight because rung 5 is unavailable. But it must not go quiet either: a
// silently empty rung looks exactly like a rung that was tried and found
// nothing, and those are different facts.
type Skip struct {
	Rung   Rung
	What   string
	Reason string
}

// LadderPlan is one expansion's worth of candidates.
type LadderPlan struct {
	// Actions are in ladder order: rung 0 first, and within a rung in the
	// deterministic order described on LadderOptions.Seed.
	Actions []Action
	// Skipped explains every action that was not generated.
	Skipped []Skip
}

// ActionsFor returns the candidates for one rung, preserving order.
func (p LadderPlan) ActionsFor(r Rung) []Action {
	out := make([]Action, 0, len(p.Actions))
	for _, a := range p.Actions {
		if a.Rung == r {
			out = append(out, a)
		}
	}
	return out
}

// SkipsFor returns the skips recorded for one rung.
func (p LadderPlan) SkipsFor(r Rung) []Skip {
	out := make([]Skip, 0, len(p.Skipped))
	for _, s := range p.Skipped {
		if s.Rung == r {
			out = append(out, s)
		}
	}
	return out
}

// LadderTiming is the window arithmetic. Every default is a measured number from
// this project rather than a guess.
type LadderTiming struct {
	// ReconMS is a rung-0 window: long enough to cross a few heartbeats, short
	// enough to stay cheap.
	ReconMS int64
	// StressMS is a rung-1 window.
	StressMS int64
	// PauseMS is the gray-failure window. It MUST stay under LeaseMS: a pause
	// that outlives the lease lets the deadline expire while the process is
	// stopped, so the resumed ex-leader correctly refuses the read and the
	// anomaly cannot occur (D-009, D-019, OQ-010). 2700 is the measured value
	// that reproduces on the fixture.
	PauseMS int64
	// PartitionMS is the rung-3 window, and the OUTER window of a compound
	// action. D-031: it must OUTLAST the pause it contains.
	PartitionMS int64
	// CompoundLeadInMS is how long the outer fault runs before the inner one
	// starts. D-031's proven schedule uses 100 ms.
	CompoundLeadInMS int64
	// CompoundTailMinMS is the minimum time the outer fault must survive the
	// inner one. It is what makes "the partition outlasts the pause" a
	// structural property of every compound action rather than a coincidence of
	// two defaults.
	CompoundTailMinMS int64
}

// DefaultLeaseMS is the read-lease length the fixture implements (D-009).
const DefaultLeaseMS int64 = 5000

// DefaultTiming returns the measured window arithmetic.
//
// With AtMS = 8200 the compound action is byte-for-byte D-031's proven schedule:
//
//	net.partition(role:leader)@8200..13500
//	proc.pause(role:leader)@8300..11000
func DefaultTiming() LadderTiming {
	return LadderTiming{
		ReconMS:           1500,
		StressMS:          2000,
		PauseMS:           2700,
		PartitionMS:       5300,
		CompoundLeadInMS:  100,
		CompoundTailMinMS: 500,
	}
}

// LadderOptions is the input to Generate.
type LadderOptions struct {
	// Config supplies perturber.allow / deny / budget / constraints and is
	// required: an action generated without them could name a kind the run is
	// forbidden to inject.
	Config *schema.Config
	// Topology is what node targets are drawn from and what static targets are
	// checked against. Required.
	Topology *perturber.Topology
	// Seed is the RUN seed, matching internal/search's convention that search
	// decisions derive from the run seed while world-internal randomness derives
	// from a world's own seed. Tie-break draws derive from it BY PATH, so the
	// ladder's prior is identical at every expansion in a run, which is what a
	// prior should be, and adding a rung cannot perturb any existing stream.
	Seed recorder.Seed
	// AtMS is the virtual millisecond at which generated windows open: A.5's
	// "applied at the current virtual clock time".
	AtMS int64
	// Prefix is the branch's already-committed faults. Candidates are compiled
	// together WITH it, so budget and max-concurrency are checked in context
	// rather than per-action.
	Prefix []string
	// KindSupported reports whether this platform can actually deliver a kind.
	// nil means faults.PlatformCapability, which is the honest default: the
	// fixture's own allow list contains io.latency, which is in the frozen
	// registry and is NOT implementable on Docker Desktop (D-033b, OQ-021).
	// Generating it would burn a world to reach ErrUnsupported mid-DRIVE.
	KindSupported func(schema.FaultKind) error
	// LeaseMS is the system-under-test's read-lease or equivalent deadline. The
	// gray-failure and compound rungs keep their pause strictly under it. Zero
	// means DefaultLeaseMS.
	LeaseMS int64
	// Timing is the window arithmetic. The zero value means DefaultTiming.
	Timing LadderTiming
}

func (o *LadderOptions) fill() {
	if o.KindSupported == nil {
		o.KindSupported = faults.PlatformCapability
	}
	if o.LeaseMS == 0 {
		o.LeaseMS = DefaultLeaseMS
	}
	if o.Timing == (LadderTiming{}) {
		o.Timing = DefaultTiming()
	}
}

// ErrLadder reports a ladder configuration that cannot generate anything sound.
var ErrLadder = errors.New("saboteur: escalation ladder")

// Generate produces the candidate actions for one node expansion, in ladder
// order.
//
// Every returned fault string has been parsed by pkg/schema and accepted by
// perturber.Compile against the caller's allow list, deny list, budget,
// constraints and topology: together with Prefix. A candidate that fails any of
// those is recorded in LadderPlan.Skipped and omitted; it is never an error,
// because a search that aborts when rung 5 is denied is worse than one that
// climbs the five rungs it has.
func Generate(opts LadderOptions) (LadderPlan, error) {
	if opts.Config == nil {
		return LadderPlan{}, fmt.Errorf("%w: needs a config; allow, deny, budget and constraints "+
			"all come from it and an action generated without them may name a kind the run is "+
			"forbidden to inject", ErrLadder)
	}
	if opts.Topology == nil {
		return LadderPlan{}, fmt.Errorf("%w: needs a topology; node targets are drawn from it and "+
			"a target that matches nothing injects nothing while still reporting as perturbed", ErrLadder)
	}
	if opts.AtMS < 0 {
		return LadderPlan{}, fmt.Errorf("%w: AtMS is %d; a window cannot open before the virtual "+
			"clock exists", ErrLadder, opts.AtMS)
	}
	opts.fill()
	if opts.Timing.PauseMS >= opts.LeaseMS {
		// Refused rather than clamped. A pause at or beyond the lease is the
		// exact defect D-019 found in the directive's own reference schedule:
		// CLOCK_MONOTONIC advances under SIGSTOP, so the lease expires during
		// the pause and the resumed ex-leader correctly refuses the stale read.
		// The rung would still generate perfectly legal faults; they simply
		// could never produce the anomaly the rung exists to produce.
		return LadderPlan{}, fmt.Errorf("%w: PauseMS is %d against a lease of %d; a gray failure "+
			"that outlives the lease lets the deadline expire while the process is stopped, so the "+
			"resumed node correctly refuses the stale read and the rung cannot fire (D-019, OQ-010)",
			ErrLadder, opts.Timing.PauseMS, opts.LeaseMS)
	}

	g := &generator{opts: opts}
	g.recon()
	g.stress()
	g.grayFail()
	g.partition()
	g.compound()
	g.temporal()
	return LadderPlan{Actions: g.actions, Skipped: g.skipped}, nil
}

// ---------------------------------------------------------------------------
// generation
// ---------------------------------------------------------------------------

type generator struct {
	opts    LadderOptions
	actions []Action
	skipped []Skip
	// targetOrder memoizes each rung's target order. A rung draws from its
	// stream ONCE, so the order a kind sees does not depend on how many other
	// kinds in the same rung happened to be permitted: otherwise removing
	// net.loss from perturber.allow would silently reorder io.latency's targets.
	targetOrder [numRungs][]string
}

// candidate is one fault before validation.
type candidate struct {
	kind    schema.FaultKind
	target  string
	params  string // rendered ", k=v" suffix, or ""
	startMS int64
	endMS   int64
}

func (c candidate) text() string {
	return fmt.Sprintf("%s(%s%s)@%d..%d", c.kind, c.target, c.params, c.startMS, c.endMS)
}

// emit validates a candidate action and either records it or records why not.
func (g *generator) emit(r Rung, note string, cands ...candidate) {
	if len(cands) == 0 {
		return
	}
	texts := make([]string, 0, len(cands))
	kinds := make([]schema.FaultKind, 0, len(cands))
	for _, c := range cands {
		canon, err := schema.CanonicalFault(c.text())
		if err != nil {
			g.skip(r, c.text(), fmt.Sprintf("does not parse: %v", err))
			return
		}
		texts = append(texts, canon)
		kinds = append(kinds, c.kind)
	}

	// Compile WITH the branch prefix. Budget and max-concurrency are properties
	// of the whole schedule, not of one action, so checking the action alone
	// would let the search build a branch that only fails on the world that
	// executes it.
	planned := make([]string, 0, len(g.opts.Prefix)+len(texts))
	planned = append(planned, g.opts.Prefix...)
	planned = append(planned, texts...)
	if _, err := perturber.Compile(perturber.CompileOptions{
		Config:   g.opts.Config,
		Topology: g.opts.Topology,
		Planned:  planned,
	}); err != nil {
		g.skip(r, strings.Join(texts, " + "), err.Error())
		return
	}

	g.actions = append(g.actions, Action{
		Rung:   r,
		Faults: texts,
		Kinds:  kinds,
		Target: cands[0].target,
		Note:   note,
	})
}

func (g *generator) skip(r Rung, what, reason string) {
	g.skipped = append(g.skipped, Skip{Rung: r, What: what, Reason: reason})
}

// usable reports whether a kind may be generated at all, and records the reason
// when it may not. Two gates, in this order:
//
//  1. perturber.allow / deny. Checked here as well as in Compile so the skip
//     reason names the config rather than a compile failure, and so no string
//     is built for a kind the run may not use.
//  2. platform capability. The frozen registry declares 17 kinds; this host
//     cannot deliver all of them (D-033b). Generating one anyway costs a whole
//     world to reach ErrUnsupported mid-DRIVE.
func (g *generator) usable(r Rung, k schema.FaultKind) bool {
	permitted := false
	for _, x := range g.opts.Config.Perturber.EffectiveFaultKinds() {
		if x == k {
			permitted = true
			break
		}
	}
	if !permitted {
		g.skip(r, string(k), "not permitted by perturber.allow / perturber.deny")
		return false
	}
	if err := g.opts.KindSupported(k); err != nil {
		g.skip(r, string(k), err.Error())
		return false
	}
	return true
}

// ---------------------------------------------------------------------------
// targets
// ---------------------------------------------------------------------------

// roleLeader is the target every rung tries first.
//
// It is not a tie to be broken: for a consensus system the leader is where the
// signal is, and D-031 proved on this fixture that targeting anything else makes
// the canonical bug unreachable. A role target binds to a concrete node at
// INJECTION time from live state (D-012), so nothing here can verify it
// statically: which is exactly why it must be the schedule's own words rather
// than a node id resolved now and stale by the time the fault fires.
const roleLeader = "role:leader"

// targets returns the target tokens for a rung, highest-signal first.
//
// role:leader is pinned first. The node ids that follow are genuinely equal
// prior, so their order is drawn from this rung's path-keyed stream: a fixed
// alphabetical order would make every seed in a 10-trial benchmark try kv-n1
// first, turning a tie-break into a systematic bias.
func (g *generator) targets(r Rung) []string {
	if r >= 0 && int(r) < len(g.targetOrder) && g.targetOrder[r] != nil {
		return g.targetOrder[r]
	}
	nodes := g.opts.Topology.Nodes()
	ids := make([]string, 0, len(nodes))
	for _, n := range nodes {
		ids = append(ids, n.ID)
	}
	// Topology.Nodes() is already ID-sorted; sort again so the shuffle's input
	// is a documented total order rather than an inherited one.
	sort.Strings(ids)
	st := LadderStream(g.opts.Seed, r)
	st.Shuffle(len(ids), func(i, j int) { ids[i], ids[j] = ids[j], ids[i] })
	out := append([]string{roleLeader}, ids...)
	if r >= 0 && int(r) < len(g.targetOrder) {
		g.targetOrder[r] = out
	}
	return out
}

// ---------------------------------------------------------------------------
// Rung 0: RECON
// ---------------------------------------------------------------------------

// A.7: net.latency(50ms) | clock.skew(100ms). Purpose: detect timing-sensitive
// code paths.
func (g *generator) recon() {
	const r = RungRecon

	// clock.skew(100ms) IS NOT GENERATED, and the reason is measured rather
	// than cautious: util-linux `unshare` parses --monotonic as WHOLE SECONDS
	// (`--monotonic=1.5` is rejected outright), so a 100 ms offset truncates
	// toward zero and internal/perturber/faults refuses it as "a fault that
	// never fires" (OQ-023). A.7's rung 0 clock action is unimplementable as
	// written. Recorded here rather than silently rounded up to 1000 ms: a
	// 10x magnitude change is a different experiment, and quietly substituting
	// one is how a ladder stops testing what it claims to test.
	if g.usable(r, schema.FaultClockSkew) {
		g.skip(r, "clock.skew(100ms)",
			"an offset of 100ms truncates to 0 whole seconds and util-linux `unshare` accepts only "+
				"whole seconds for --monotonic, so the fault would never fire (OQ-023). Rung 5 "+
				"carries the implementable clock actions at 3000ms and 5000ms")
	}

	if !g.usable(r, schema.FaultNetLatency) {
		return
	}
	end := g.opts.AtMS + g.opts.Timing.ReconMS
	for _, t := range g.targets(r) {
		g.emit(r, "", candidate{
			kind: schema.FaultNetLatency, target: t,
			params: ", mean=50, jitter=10", startMS: g.opts.AtMS, endMS: end,
		})
	}
}

// ---------------------------------------------------------------------------
// Rung 1: STRESS
// ---------------------------------------------------------------------------

// A.7: net.latency(500ms) | net.loss(5%) | io.latency(200ms). Purpose: push
// retry and timeout logic to its limits.
func (g *generator) stress() {
	const r = RungStress
	end := g.opts.AtMS + g.opts.Timing.StressMS

	type spec struct {
		kind   schema.FaultKind
		params string
	}
	// Registry order, which is frozen, so this list's order does not depend on
	// how it happens to be written here.
	for _, s := range []spec{
		{schema.FaultNetLatency, ", mean=500, jitter=50"},
		{schema.FaultNetLoss, ", pct=5"},
		{schema.FaultIOLatency, ", ms=200"},
	} {
		if !g.usable(r, s.kind) {
			continue
		}
		for _, t := range g.targets(r) {
			g.emit(r, "", candidate{
				kind: s.kind, target: t, params: s.params,
				startMS: g.opts.AtMS, endMS: end,
			})
		}
	}
}

// ---------------------------------------------------------------------------
// Rung 2: GRAY FAIL
// ---------------------------------------------------------------------------

// A.7: proc.pause (SIGSTOP/SIGCONT). Purpose: ambiguous liveness; the node
// looks alive to the orchestrator and dead to its peers.
func (g *generator) grayFail() {
	const r = RungGrayFail
	if !g.usable(r, schema.FaultProcPause) {
		return
	}
	end := g.opts.AtMS + g.opts.Timing.PauseMS
	for _, t := range g.targets(r) {
		g.emit(r, "", candidate{
			kind: schema.FaultProcPause, target: t,
			startMS: g.opts.AtMS, endMS: end,
		})
	}
}

// ---------------------------------------------------------------------------
// Rung 3: PARTITION
// ---------------------------------------------------------------------------

// asymmetricNote records a capability gap rather than pretending A.7's
// preference was honoured.
const asymmetricNote = "symmetric partition: A.7 prefers an asymmetric one, but every TARGET form " +
	"in the frozen grammar (node, wildcard, role, edge, quorum) is symmetric, so an asymmetric " +
	"partition needs a directed target the grammar cannot express (D-028 item 4)"

// A.7: net.partition (asymmetric preferred). Purpose: force quorum
// reconfiguration under load.
func (g *generator) partition() {
	const r = RungPartition
	if !g.usable(r, schema.FaultNetPartition) {
		return
	}
	end := g.opts.AtMS + g.opts.Timing.PartitionMS
	for _, t := range g.targets(r) {
		g.emit(r, asymmetricNote, candidate{
			kind: schema.FaultNetPartition, target: t,
			startMS: g.opts.AtMS, endMS: end,
		})
	}
	// A quorum target is offered ONLY here, never in the compound rung. See
	// compound() for why.
	for _, svc := range g.opts.Topology.Services() {
		t := fmt.Sprintf("%s(%s)", schema.QuorumMinority, svc)
		g.emit(r, asymmetricNote, candidate{
			kind: schema.FaultNetPartition, target: t,
			startMS: g.opts.AtMS, endMS: end,
		})
	}
}

// ---------------------------------------------------------------------------
// Rung 4: COMPOUND
// ---------------------------------------------------------------------------

// compoundNote states the shape every compound action holds to.
const compoundNote = "the outer fault strictly contains and OUTLASTS the inner one, and both name " +
	"the same target: D-031 measured that a displaced leader which can learn it was displaced " +
	"steps down before serving anything, so the containing fault must survive the contained one"

// A.7: overlap two faults from rungs 1-3 in time, "bias toward overlapping
// proc.pause with net.partition; this is the canonical Raft lease bug trigger".
//
// # The shape is not free, and getting it wrong makes the phase's own definition
// # of done unreachable
//
// D-031 established three independent facts about this fixture, each by running
// it rather than by reading:
//
//   - The partition must target THE LEADER. Pausing the leader while
//     partitioning an arbitrary minority removes two of three nodes from the
//     connected majority, so no election completes, nothing is committed in a
//     new term, and there is nothing stale to read. Measured: the term advanced
//     only AFTER the pause was withdrawn, and zero stale reads occurred.
//   - The partition must OUTLAST the pause. The instant the ex-leader
//     unfreezes, the new leader's heartbeat reaches it, it learns of the higher
//     term and steps down before serving anything.
//   - The pause must be SHORTER than the lease (enforced in Generate).
//
// The directive's own pairing (proc.pause(role:leader) with
// net.partition(minority(kv))) violates the first, and its windows violate the
// second and third. This generator makes all three structural: both faults of a
// compound action always name the SAME target, the outer window always strictly
// contains the inner, and no compound action ever uses a quorum target, because
// a quorum target binds to an ARBITRARY minority at injection time, which is
// precisely how the directive's schedule became incapable of firing.
func (g *generator) compound() {
	const r = RungCompound

	// The canonical pair first: this is the "bias" A.7 asks for, expressed as
	// ladder order rather than as a weight.
	pairs := [][2]schema.FaultKind{
		{schema.FaultNetPartition, schema.FaultProcPause},
	}
	// Then the remaining overlaps of a rung 2-3 kind with a rung 1-3 kind.
	for _, outer := range []schema.FaultKind{schema.FaultNetPartition, schema.FaultProcPause} {
		for _, inner := range []schema.FaultKind{
			schema.FaultProcPause, schema.FaultNetLatency, schema.FaultNetLoss, schema.FaultIOLatency,
		} {
			if outer == inner {
				continue
			}
			if outer == schema.FaultNetPartition && inner == schema.FaultProcPause {
				continue // already first
			}
			pairs = append(pairs, [2]schema.FaultKind{outer, inner})
		}
	}

	targets := g.targets(r)
	for _, p := range pairs {
		outerKind, innerKind := p[0], p[1]
		if !g.usable(r, outerKind) || !g.usable(r, innerKind) {
			continue
		}
		outerDur := g.duration(outerKind)
		innerDur := g.duration(innerKind)

		outerStart := g.opts.AtMS
		outerEnd := outerStart + outerDur
		innerStart := outerStart + g.opts.Timing.CompoundLeadInMS
		maxInner := outerEnd - g.opts.Timing.CompoundTailMinMS - innerStart
		if maxInner < innerDur {
			innerDur = maxInner
		}
		if innerDur <= 0 {
			g.skip(r, fmt.Sprintf("%s over %s", outerKind, innerKind),
				fmt.Sprintf("the outer window (%dms) cannot contain a %dms lead-in, a positive "+
					"inner window and a %dms tail; widen Timing rather than emitting a pair whose "+
					"inner fault outlives its container",
					outerDur, g.opts.Timing.CompoundLeadInMS, g.opts.Timing.CompoundTailMinMS))
			continue
		}
		if innerKind == schema.FaultProcPause && innerDur >= g.opts.LeaseMS {
			g.skip(r, fmt.Sprintf("%s over %s", outerKind, innerKind),
				fmt.Sprintf("the contained pause would run %dms against a %dms lease, so the "+
					"deadline expires while the process is stopped and the anomaly cannot occur",
					innerDur, g.opts.LeaseMS))
			continue
		}
		innerEnd := innerStart + innerDur

		for _, t := range targets {
			g.emit(r, compoundNote,
				candidate{kind: outerKind, target: t, params: g.params(outerKind),
					startMS: outerStart, endMS: outerEnd},
				candidate{kind: innerKind, target: t, params: g.params(innerKind),
					startMS: innerStart, endMS: innerEnd},
			)
		}
	}
}

// duration is the default window length for a kind inside a compound action.
func (g *generator) duration(k schema.FaultKind) int64 {
	switch k {
	case schema.FaultNetPartition:
		return g.opts.Timing.PartitionMS
	case schema.FaultProcPause:
		return g.opts.Timing.PauseMS
	default:
		return g.opts.Timing.StressMS
	}
}

// params is the rendered parameter suffix for a kind used in a compound action.
func (g *generator) params(k schema.FaultKind) string {
	switch k {
	case schema.FaultNetLatency:
		return ", mean=500, jitter=50"
	case schema.FaultNetLoss:
		return ", pct=5"
	case schema.FaultIOLatency:
		return ", ms=200"
	default:
		return ""
	}
}

// ---------------------------------------------------------------------------
// Rung 5: TEMPORAL
// ---------------------------------------------------------------------------

// clockNote is attached to every clock action. Both halves are measured facts
// from D-024, not caveats added for safety.
const clockNote = "monotonic and boottime ONLY — a Linux time namespace does not move the wall " +
	"clock, so logic keyed on CLOCK_REALTIME (calendar TTLs, certificate expiry) is out of reach " +
	"(OQ-019); and the offset is fixed when the namespace is created, so this is a RESTART into a " +
	"skewed namespace rather than a clock shifted under a running process (OQ-020)"

// A.7: clock.jump(5000ms) | clock.skew(3000ms). Purpose: attack lease timers,
// TTLs, and any logic assuming monotonic or bounded clock drift.
//
// Both magnitudes are whole seconds, which is the only granularity the mechanism
// has (OQ-023). Nothing sub-second is generated anywhere in this file.
func (g *generator) temporal() {
	const r = RungTemporal
	type spec struct {
		kind schema.FaultKind
		ms   int64
	}
	for _, s := range []spec{
		{schema.FaultClockJump, 5000},
		{schema.FaultClockSkew, 3000},
	} {
		if !g.usable(r, s.kind) {
			continue
		}
		if s.ms%1000 != 0 {
			// Unreachable with the constants above; kept because the invariant
			// is about the MECHANISM, and a future edit to the magnitudes must
			// not be able to slip a sub-second offset past it.
			g.skip(r, fmt.Sprintf("%s(%dms)", s.kind, s.ms),
				"util-linux `unshare` accepts whole seconds only; a sub-second offset truncates "+
					"and the fault never fires (OQ-023)")
			continue
		}
		// The window borrows the partition length because a clock fault's
		// window bounds a RESTART pair (OQ-020): the node comes back skewed at
		// the start and is restarted back to true time at the end. It must
		// therefore be long enough for the node to rejoin and for a lease to be
		// exercised under the skew, which is the same span rung 3 needs for an
		// election to complete.
		end := g.opts.AtMS + g.opts.Timing.PartitionMS
		for _, t := range g.targets(r) {
			g.emit(r, clockNote, candidate{
				kind: s.kind, target: t,
				params:  fmt.Sprintf(", ms=%d", s.ms),
				startMS: g.opts.AtMS, endMS: end,
			})
		}
	}
}
