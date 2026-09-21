package faults

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// tc / netem plan construction.
//
// # The shaping stack
//
// Every shaped interface carries exactly one perturber-owned root qdisc:
//
//	tc qdisc add dev eth0 root handle 1: prio bands 16 \
//	   priomap 1 1 1 1 1 1 1 1 1 1 1 1 1 1 1 1
//
// The priomap matters. A default `prio` priomap distributes traffic across
// bands 0, 1 and 2 by TOS, which would send some unfiltered traffic straight
// into the band the classic recipes use for shaping, so a "latency to n2"
// fault would also delay an arbitrary TOS-selected slice of everything else.
// Mapping every TOS value to band 1 puts ALL unfiltered traffic in class 1:2
// and leaves bands 2..15 (classes 1:3..1:16) free for faults to claim.
//
// # Slots are the ownership story
//
// tc has no comment facility, so ownership cannot be tagged. Instead each
// shaping fault claims a SLOT in [3, 16] on each interface it shapes, and the
// slot number fixes every handle it uses:
//
//	class  1:<slot>
//	qdisc  <slot*10>:   attached to parent 1:<slot>
//	pref   <slot>       for the u32 filters selecting into that class
//
// Withdrawal deletes exactly those, and the last slot to leave an interface
// takes the root qdisc with it. The ledger records slot assignments so a
// diagnosis can be done by hand; the AUTHORITY on whether anything survived is
// the BOOT baseline diff, not the ledger, because a ledger cannot see a qdisc
// nobody recorded.
const (
	// ShapeRootHandle is the perturber's root qdisc handle.
	ShapeRootHandle = "1:"
	// ShapeBands is the prio band count. 16 is the qdisc's maximum.
	ShapeBands = 16
	// ShapeDefaultClass is where all unfiltered traffic goes.
	ShapeDefaultClass = "1:2"
	// FirstSlot and LastSlot bound the claimable slot range.
	FirstSlot = 3
	LastSlot  = 16
)

// ReorderEnableDelayMS is the delay netem requires before it will reorder.
//
// This is not a tuning choice, it is a kernel constraint. Measured on this
// host, iproute2 v7.0.0 rejects `netem reorder 25%` outright:
//
//	reordering not possible without specifying some delay
//
// netem implements reordering by sending the selected fraction IMMEDIATELY
// while everything else waits `delay`, so the delay IS the reordering distance
// and a reorder with no delay has no meaning. The frozen grammar's
// net.reorder(pct) carries no delay parameter, so the injector supplies this
// one and records it in the realized ledger rather than hiding it. See the
// package report and OPEN_QUESTIONS.
const ReorderEnableDelayMS = 10

// safeRootQdiscKinds are the qdisc kinds the perturber is willing to displace.
//
// Installing our root over the target's own shaping would destroy a
// configuration we cannot restore: `tc qdisc del ... root` returns the KERNEL
// default, not whatever the operator had. Every kind here is a kernel default
// or an automatic multi-queue wrapper, so deleting our root genuinely restores
// the prior state, which the BOOT baseline then confirms.
var safeRootQdiscKinds = map[string]bool{
	"noqueue":    true,
	"pfifo_fast": true,
	"mq":         true,
	"fq_codel":   true,
	"fq":         true,
	"pfifo":      true,
}

// RootQdiscKind reads the root qdisc kind for dev out of normalized
// `tc qdisc show` lines, e.g. "qdisc noqueue 0: dev eth0 root".
func RootQdiscKind(qdiscs []string, dev string) (string, bool) {
	for _, l := range qdiscs {
		f := strings.Fields(l)
		if len(f) < 5 || f[0] != "qdisc" {
			continue
		}
		var isDev, isRoot bool
		for i, tok := range f {
			if tok == "root" {
				isRoot = true
			}
			if tok == "dev" && i+1 < len(f) && f[i+1] == dev {
				isDev = true
			}
		}
		if isDev && isRoot {
			return f[1], true
		}
	}
	return "", false
}

// CheckRootQdiscSafe reports whether the perturber may take over dev's root.
func CheckRootQdiscSafe(nodeID string, base Baseline, dev string) error {
	kind, ok := RootQdiscKind(base.Qdiscs, dev)
	if !ok {
		// No recorded root qdisc at BOOT. Refusing is the conservative reading:
		// we would be installing over something we never observed.
		return fmt.Errorf("faults: node %s: no root qdisc recorded for %s at BOOT, "+
			"so shaping it cannot be safely withdrawn", nodeID, dev)
	}
	if !safeRootQdiscKinds[kind] {
		return fmt.Errorf("faults: node %s: %s already carries a %q root qdisc that the target owns; "+
			"installing a shaping root over it would destroy a configuration `tc qdisc del root` "+
			"cannot restore", nodeID, dev, kind)
	}
	return nil
}

// ShapeRootCommand installs the perturber's root qdisc on dev.
func ShapeRootCommand(dev string) (string, error) {
	if err := checkIface(dev); err != nil {
		return "", err
	}
	priomap := strings.TrimSpace(strings.Repeat("1 ", ShapeBands))
	return fmt.Sprintf("tc qdisc add dev %s root handle %s prio bands %d priomap %s",
		dev, ShapeRootHandle, ShapeBands, priomap), nil
}

// ShapeRootDeleteCommand removes the perturber's root qdisc from dev.
func ShapeRootDeleteCommand(dev string) (string, error) {
	if err := checkIface(dev); err != nil {
		return "", err
	}
	return fmt.Sprintf("tc qdisc del dev %s root handle %s", dev, ShapeRootHandle), nil
}

// slotHandle is the netem handle for a slot: 30:, 40:, ... 160:.
func slotHandle(slot int) string { return strconv.Itoa(slot*10) + ":" }

// slotClass is the prio class for a slot: 1:3 ... 1:16.
func slotClass(slot int) string { return "1:" + strconv.Itoa(slot) }

// ShapeSlotCommands installs one fault's netem qdisc and the u32 filters that
// steer the affected peers' traffic into it.
//
// Only traffic addressed to a peer is shaped. Everything else (the health
// probe from the host, the client workload, the orchestrator's own inspection)
// stays in class 1:2 and is untouched. That is what makes a shaping fault a
// GRAY failure rather than a dead node, and the KV fixture's stale read is only
// observable in the gray case.
func ShapeSlotCommands(dev string, slot int, netemArgs []string, dsts []string) ([]string, error) {
	if err := checkIface(dev); err != nil {
		return nil, err
	}
	if slot < FirstSlot || slot > LastSlot {
		return nil, fmt.Errorf("faults: shaping slot %d is outside [%d, %d]", slot, FirstSlot, LastSlot)
	}
	if len(netemArgs) == 0 {
		return nil, fmt.Errorf("faults: shaping slot %d has no netem parameters", slot)
	}
	if len(dsts) == 0 {
		return nil, fmt.Errorf("faults: shaping on %s selects no destination address; "+
			"a fault that matches no traffic is a silent no-op", dev)
	}
	sorted := append([]string(nil), dsts...)
	sort.Strings(sorted)

	cmds := []string{
		fmt.Sprintf("tc qdisc add dev %s parent %s handle %s netem %s",
			dev, slotClass(slot), slotHandle(slot), strings.Join(netemArgs, " ")),
	}
	for _, d := range sorted {
		v6, err := checkAddr(d)
		if err != nil {
			return nil, err
		}
		if v6 {
			// Deliberately not implemented rather than silently skipped. A
			// filter set that omits a reachable peer under-injects, and an
			// under-injected fault produces a PASS on a world whose fault never
			// fully fired: the most dangerous output this tool can produce.
			return nil, fmt.Errorf("faults: shaping toward the IPv6 peer address %s is not implemented; "+
				"refusing rather than shaping only the IPv4 half of the path", d)
		}
		cmds = append(cmds, fmt.Sprintf(
			"tc filter add dev %s protocol ip parent %s0 prio %d u32 match ip dst %s/32 flowid %s",
			dev, ShapeRootHandle, slot, d, slotClass(slot)))
	}
	return cmds, nil
}

// ShapeSlotDeleteCommands removes one fault's filters and netem qdisc.
//
// Filters go first. Deleting the qdisc while filters still point at its class
// leaves the filters live against a class with no qdisc, which silently drops
// nothing but leaves state behind that the baseline diff cannot see, because
// `tc qdisc show` does not list filters.
func ShapeSlotDeleteCommands(dev string, slot int) ([]string, error) {
	if err := checkIface(dev); err != nil {
		return nil, err
	}
	if slot < FirstSlot || slot > LastSlot {
		return nil, fmt.Errorf("faults: shaping slot %d is outside [%d, %d]", slot, FirstSlot, LastSlot)
	}
	return []string{
		fmt.Sprintf("tc filter del dev %s parent %s0 protocol ip prio %d || true",
			dev, ShapeRootHandle, slot),
		fmt.Sprintf("tc qdisc del dev %s parent %s handle %s || true",
			dev, slotClass(slot), slotHandle(slot)),
	}, nil
}

// ---------------------------------------------------------------------------
// netem parameters
// ---------------------------------------------------------------------------

// NetemArgs renders the netem parameter list for a shaping fault.
//
// EVERY netem qdisc is seeded, including net.bandwidth, whose behaviour is not
// itself stochastic. Measured on this host, netem stores an explicit seed
// faithfully (asked 11111, reported 11111) and invents a fresh random one when
// none is given: `rate 1000000` came back as `rate 1Mbit seed
// 16330850384067447348`. Passing a seed unconditionally means the realized
// world records the same value that replaying it will install (D-025).
//
// Parameter values are the SOURCE TEXT from pkg/schema, re-emitted verbatim. A
// value that round-tripped through a float would change the canonical fault
// string and therefore the hash of every world containing it.
func NetemArgs(spec schema.FaultSpec, seed uint64) ([]string, error) {
	get := func(name string) (string, error) {
		v, ok := spec.Param(name)
		if !ok {
			return "", fmt.Errorf("faults: %s has no %s parameter", spec.Kind, name)
		}
		if err := checkNumber(string(spec.Kind)+"."+name, v); err != nil {
			return "", err
		}
		return v, nil
	}
	nonZero := func(name, v string) error {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return fmt.Errorf("faults: %s: %s=%q is not a number", spec.Kind, name, v)
		}
		if f == 0 {
			return fmt.Errorf("faults: %s(%s): %s=0 injects nothing; "+
				"a fault with no observable effect is a silent no-op, not a fault",
				spec.Kind, spec.Target, name)
		}
		return nil
	}

	var args []string
	switch spec.Kind {
	case schema.FaultNetLatency:
		mean, err := get("mean")
		if err != nil {
			return nil, err
		}
		jitter, err := get("jitter")
		if err != nil {
			return nil, err
		}
		if mean == "0" && jitter == "0" {
			return nil, fmt.Errorf("faults: %s(%s): mean=0 and jitter=0 injects nothing",
				spec.Kind, spec.Target)
		}
		args = append(args, "delay", mean+"ms")
		if jitter != "0" {
			// netem's default distribution is uniform. It is left at the
			// default deliberately: `distribution normal` would need a
			// kernel-supplied distribution table whose presence varies by
			// image, and a fault that works on one host and not another is
			// worse than a simpler one that always works.
			args = append(args, jitter+"ms")
		}

	case schema.FaultNetLoss:
		pct, err := get("pct")
		if err != nil {
			return nil, err
		}
		if err := nonZero("pct", pct); err != nil {
			return nil, err
		}
		args = append(args, "loss", pct+"%")

	case schema.FaultNetDuplicate:
		pct, err := get("pct")
		if err != nil {
			return nil, err
		}
		if err := nonZero("pct", pct); err != nil {
			return nil, err
		}
		args = append(args, "duplicate", pct+"%")

	case schema.FaultNetReorder:
		pct, err := get("pct")
		if err != nil {
			return nil, err
		}
		if err := nonZero("pct", pct); err != nil {
			return nil, err
		}
		// The enabling delay is mandatory; see ReorderEnableDelayMS.
		args = append(args, "delay", strconv.Itoa(ReorderEnableDelayMS)+"ms", "reorder", pct+"%")

	case schema.FaultNetBandwidth:
		bps, err := get("bps")
		if err != nil {
			return nil, err
		}
		if err := nonZero("bps", bps); err != nil {
			return nil, err
		}
		// netem `rate` rather than tbf. tbf needs `burst` and `latency`
		// parameters that have no defensible default (guess burst too small
		// and the shaper cannot reach the requested rate at all, guess it too
		// large and the cap does not bite) while netem takes the rate alone
		// and composes with the delay/loss parameters in the same qdisc, which
		// keeps one mechanism for the whole family.
		args = append(args, "rate", bps)

	default:
		return nil, fmt.Errorf("faults: %s is not a netem-shaped fault kind", spec.Kind)
	}

	args = append(args, "seed", strconv.FormatUint(seed, 10))
	return args, nil
}

// IsShaping reports whether a kind is injected through netem rather than
// iptables.
func IsShaping(k schema.FaultKind) bool {
	switch k {
	case schema.FaultNetLatency, schema.FaultNetLoss, schema.FaultNetReorder,
		schema.FaultNetDuplicate, schema.FaultNetBandwidth:
		return true
	}
	return false
}

// Supports reports whether this package injects the given fault kind.
func Supports(k schema.FaultKind) bool {
	return k == schema.FaultNetPartition || IsShaping(k)
}
