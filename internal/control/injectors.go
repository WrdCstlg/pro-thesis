package control

import (
	"context"
	"fmt"

	"github.com/WrdCstlg/pro-thesis/internal/perturber"
	"github.com/WrdCstlg/pro-thesis/internal/perturber/faults"
	"github.com/WrdCstlg/pro-thesis/internal/recorder"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// ---------------------------------------------------------------------------
// Injector assembly
//
// The fault families each know how to apply themselves; this file is the only
// place that knows they all exist and how to hand each one what it needs.
//
// It runs at BOOT rather than at PERTURB for a reason that is not stylistic:
// every family judges "clean" against a snapshot taken BEFORE any fault
// existed (D-026). Capturing that snapshot after a fault has been injected
// would bake the fault into the definition of clean, and HEAL would then verify
// nothing at all.
// ---------------------------------------------------------------------------

// InjectorSet is the assembled fault mechanisms plus the baselines HEAL judges
// residue against.
type InjectorSet struct {
	Injectors []perturber.Injector
	// NetBaselines is the per-node iptables/tc snapshot.
	NetBaselines *faults.BaselineSet
	// ContainerBaseline is the per-node cpu/memory/fd snapshot.
	ContainerBaseline faults.ContainerBaseline
}

// BuildInjectors assembles every fault family against a live topology and
// captures the BOOT baselines they verify against.
//
// A family whose mechanism is unavailable on this host is still REGISTERED. It
// then fails loudly at injection time with an error wrapping
// faults.ErrUnsupported, which the control plane reports as exit 2
// INCONCLUSIVE. That is deliberate and it is the whole point: the alternative
// (omitting the family so its kinds resolve to "no injector") is also loud, but
// a family that silently no-opped would let a world whose faults never fired
// report PASS, which is the most dangerous verdict this tool can produce.
func BuildInjectors(
	ctx context.Context,
	cfg *schema.Config,
	top *recorder.Topology,
	runID string,
	projectDir string,
	worldSeed uint64,
) (*InjectorSet, error) {
	if cfg == nil || top == nil {
		return nil, fmt.Errorf("control: BuildInjectors needs a config and a topology")
	}

	netNodes, targets := faultTopology(cfg, top)
	if len(netNodes) == 0 {
		return nil, fmt.Errorf("control: the topology binds no nodes, so no fault has anywhere to land")
	}

	env := faults.Env{
		RunID:      runID,
		ProjectDir: projectDir,
		Topology:   top,
	}

	// --- network family -----------------------------------------------------
	//
	// The injector is constructed first and its baselines captured through it,
	// because capture runs the same sidecar mechanism injection does: if the
	// sidecar cannot run, we learn it here at BOOT rather than mid-window.
	netInj, err := faults.NewInjector(faults.Options{
		RunID:     runID,
		WorldSeed: worldSeed,
		Nodes:     netNodes,
	})
	if err != nil {
		return nil, fmt.Errorf("control: build the network injector: %w", err)
	}
	netBase, err := netInj.CaptureBaselines(ctx)
	if err != nil {
		return nil, fmt.Errorf("control: capture the network baseline at BOOT: %w", err)
	}
	netInj.SetBaselines(netBase)

	// --- container-state families -------------------------------------------
	//
	// proc.slow, mem.pressure and fd.exhaust all mutate container limits, and
	// all restore them from this snapshot. An unreadable snapshot is not fatal
	// to the run: the families that need it refuse at injection time with
	// ErrUnsupported naming what was missing, which is a better failure than
	// refusing to boot a world whose schedule may not use them at all.
	containerBase, cbErr := faults.SnapshotContainerBaseline(ctx, env, targets, faults.DefaultHelperImage)

	set := &InjectorSet{
		Injectors: []perturber.Injector{
			faults.NewNetInjector(netInj),
			faults.NewProcessInjector(env, containerBase),
			faults.NewClockInjector(env),
			faults.NewIOInjector(env, containerBase),
		},
		NetBaselines:      netBase,
		ContainerBaseline: containerBase,
	}
	if cbErr != nil {
		// Reported, not swallowed, and not fatal. The caller logs it.
		return set, fmt.Errorf("control: container baseline incomplete (proc.slow, mem.pressure and "+
			"fd.exhaust will refuse rather than run unverifiably): %w", cbErr)
	}
	return set, nil
}

// faultTopology projects the harness topology into the two shapes the fault
// families take.
//
// Group is the LOGICAL service from prothesis.yaml (the `kv` of `minority(kv)`)
// while ComposeService is the physical one. Under one-service-per-node those
// differ, and conflating them is exactly the defect D-020 exists to prevent.
func faultTopology(cfg *schema.Config, top *recorder.Topology) ([]faults.NetNode, []faults.Target) {
	byID := map[string]schema.NodeConfig{}
	for _, n := range cfg.Harness.Nodes {
		byID[n.ID] = n
	}

	var nets []faults.NetNode
	var targets []faults.Target
	for _, b := range top.Nodes {
		nc, ok := byID[b.ID]
		if !ok {
			// Bound by the harness but absent from the config: it cannot be
			// named by a fault target, so it is not a fault surface. It is
			// still part of the topology for a partition's complement, so it
			// keeps its physical identity and gets no group.
			nets = append(nets, faults.NetNode{
				NodeID:         b.ID,
				ContainerID:    b.ContainerID,
				ComposeService: b.Service,
				HostPort:       b.HostPort,
			})
			continue
		}
		nets = append(nets, faults.NetNode{
			NodeID:         b.ID,
			ContainerID:    b.ContainerID,
			ComposeService: nc.EffectiveComposeService(),
			Group:          nc.Service,
			HostPort:       b.HostPort,
		})
		targets = append(targets, faults.Target{
			NodeID:         b.ID,
			ContainerID:    b.ContainerID,
			ComposeService: nc.EffectiveComposeService(),
			Group:          nc.Service,
			ClientPort:     int64(nc.ClientPort()),
		})
	}
	return nets, targets
}
