package schema

import (
	"fmt"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// FaultKind is the KIND half of the fault grammar `KIND(TARGET[, PARAMS])@WINDOW`.
//
// The seventeen v1 kinds are frozen by directive 4.3 ("Required Fault Families
// (Phase 2)") plus `io.fill`, which the directive names in the sample config's
// `perturber.deny` list (4.2) and nowhere else.
type FaultKind string

// The seventeen v1 fault kinds.
const (
	// Network: tc/netem and iptables in the target's network namespace.
	FaultNetPartition FaultKind = "net.partition"
	FaultNetLatency   FaultKind = "net.latency"
	FaultNetLoss      FaultKind = "net.loss"
	FaultNetReorder   FaultKind = "net.reorder"
	FaultNetDuplicate FaultKind = "net.duplicate"
	FaultNetBandwidth FaultKind = "net.bandwidth"

	// Process: signals and cgroup CPU quota.
	FaultProcKill    FaultKind = "proc.kill"
	FaultProcPause   FaultKind = "proc.pause"
	FaultProcRestart FaultKind = "proc.restart"
	FaultProcSlow    FaultKind = "proc.slow"

	// Clock: libfaketime or a time namespace.
	FaultClockSkew FaultKind = "clock.skew"
	FaultClockJump FaultKind = "clock.jump"

	// I/O.
	FaultIOLatency FaultKind = "io.latency"
	FaultIOError   FaultKind = "io.error"
	FaultIOFill    FaultKind = "io.fill"

	// Resource.
	FaultMemPressure FaultKind = "mem.pressure"
	FaultFDExhaust   FaultKind = "fd.exhaust"
)

// FaultFamily groups kinds by the mechanism that injects them. The family is
// the part of the kind name before the dot.
type FaultFamily string

const (
	FamilyNet   FaultFamily = "net"
	FamilyProc  FaultFamily = "proc"
	FamilyClock FaultFamily = "clock"
	FamilyIO    FaultFamily = "io"
	FamilyMem   FaultFamily = "mem"
	FamilyFD    FaultFamily = "fd"
)

// ParamType is the value domain of a fault parameter. It is used to VALIDATE a
// parameter's source text; the text itself is what is stored and re-emitted, so
// no number is ever reformatted and no world hash can drift because Go changed
// its shortest-float representation.
type ParamType int

const (
	// ParamInt is any decimal integer, possibly negative (clock.skew may run
	// a clock backwards).
	ParamInt ParamType = iota
	// ParamNonNegInt is a decimal integer >= 0.
	ParamNonNegInt
	// ParamPercent is a number in [0, 100], integer or decimal.
	ParamPercent
	// ParamRate is a number in [0, 1], integer or decimal.
	ParamRate
	// ParamSignal is a POSIX signal name (SIGKILL, SIGTERM, ...) or number.
	ParamSignal
)

func (t ParamType) String() string {
	switch t {
	case ParamInt:
		return "an integer"
	case ParamNonNegInt:
		return "a non-negative integer"
	case ParamPercent:
		return "a percentage in [0, 100]"
	case ParamRate:
		return "a rate in [0, 1]"
	case ParamSignal:
		return "a signal name such as SIGKILL, or a signal number"
	}
	return "a value"
}

// ParamDecl declares one parameter of a fault kind.
type ParamDecl struct {
	// Name is the parameter's normative name, taken from directive 4.3's
	// parenthesised parameter lists (net.latency(mean,jitter), net.loss(pct),
	// proc.kill(signal), proc.slow(cpu_pct), clock.skew(ms), io.error(rate),
	// mem.pressure(pct), net.bandwidth(bps), io.latency(ms)).
	Name string
	// Type is the domain the value must lie in.
	Type ParamType
	// Default is the source text used when the parameter is omitted. An empty
	// Default makes the parameter REQUIRED.
	Default string
	// Unit documents what the number means. Values carry no unit suffix, so
	// this is the only place the unit is written down.
	Unit string
}

// Required reports whether the parameter must be supplied explicitly.
func (p ParamDecl) Required() bool { return p.Default == "" }

// KindDecl is the frozen registry entry for one fault kind.
type KindDecl struct {
	Kind   FaultKind
	Family FaultFamily
	Params []ParamDecl
	// Durative distinguishes faults that are ACTIVE over [start, end) and
	// withdrawn at end from faults that FIRE AT start.
	//
	// For an instantaneous kind, [start, end] is the tolerated-disturbance
	// window: the built-in oracle no_crash is specified as "no process exit
	// outside planned fault windows", which is only meaningful if a kill's
	// window bounds the period in which the resulting exit is expected.
	Durative bool
	// AllowEdge reports whether an edge target (n1<->n2) is meaningful. Only
	// the network family acts on a link; pausing an edge is not a thing.
	AllowEdge bool
	// Doc is a one-line description used by `thesis` help output.
	Doc string
}

// ParamNames returns the declared parameter names in order.
func (d KindDecl) ParamNames() []string {
	out := make([]string, len(d.Params))
	for i, p := range d.Params {
		out[i] = p.Name
	}
	return out
}

// faultRegistry is the frozen v1 kind table. Changing a kind name, a parameter
// name, a parameter order or a default changes the canonical string of every
// fault that uses it, and therefore the world_hash of every archived world that
// contains one.
var faultRegistry = []KindDecl{
	{
		Kind: FaultNetPartition, Family: FamilyNet, Durative: true, AllowEdge: true,
		Doc: "drop traffic between the target and the rest of the cluster, in both directions",
	},
	{
		Kind: FaultNetLatency, Family: FamilyNet, Durative: true, AllowEdge: true,
		Params: []ParamDecl{
			{Name: "mean", Type: ParamNonNegInt, Unit: "milliseconds"},
			{Name: "jitter", Type: ParamNonNegInt, Default: "0", Unit: "milliseconds"},
		},
		Doc: "delay packets by mean +/- jitter",
	},
	{
		Kind: FaultNetLoss, Family: FamilyNet, Durative: true, AllowEdge: true,
		Params: []ParamDecl{{Name: "pct", Type: ParamPercent, Unit: "percent of packets"}},
		Doc:    "drop a percentage of packets",
	},
	{
		Kind: FaultNetReorder, Family: FamilyNet, Durative: true, AllowEdge: true,
		Params: []ParamDecl{{Name: "pct", Type: ParamPercent, Unit: "percent of packets"}},
		Doc:    "reorder a percentage of packets",
	},
	{
		Kind: FaultNetDuplicate, Family: FamilyNet, Durative: true, AllowEdge: true,
		Params: []ParamDecl{{Name: "pct", Type: ParamPercent, Unit: "percent of packets"}},
		Doc:    "duplicate a percentage of packets",
	},
	{
		Kind: FaultNetBandwidth, Family: FamilyNet, Durative: true, AllowEdge: true,
		Params: []ParamDecl{{Name: "bps", Type: ParamNonNegInt, Unit: "bits per second"}},
		Doc:    "cap egress bandwidth",
	},
	{
		Kind: FaultProcKill, Family: FamilyProc, Durative: false,
		Params: []ParamDecl{{Name: "signal", Type: ParamSignal, Default: "SIGKILL", Unit: "POSIX signal"}},
		Doc:    "deliver a signal to the target process once, at the window start",
	},
	{
		Kind: FaultProcPause, Family: FamilyProc, Durative: true,
		Doc: "SIGSTOP at the window start, SIGCONT at the window end (gray failure)",
	},
	{
		Kind: FaultProcRestart, Family: FamilyProc, Durative: false,
		Doc: "stop and start the target once, at the window start",
	},
	{
		Kind: FaultProcSlow, Family: FamilyProc, Durative: true,
		Params: []ParamDecl{{Name: "cpu_pct", Type: ParamPercent, Unit: "percent of one CPU"}},
		Doc:    "throttle the target to a fraction of its CPU quota",
	},
	{
		Kind: FaultClockSkew, Family: FamilyClock, Durative: true,
		Params: []ParamDecl{{Name: "ms", Type: ParamInt, Unit: "milliseconds of offset"}},
		Doc:    "hold the target's clock at a constant offset for the window",
	},
	{
		Kind: FaultClockJump, Family: FamilyClock, Durative: false,
		Params: []ParamDecl{{Name: "ms", Type: ParamInt, Unit: "milliseconds of jump"}},
		Doc:    "step the target's clock once, at the window start",
	},
	{
		Kind: FaultIOLatency, Family: FamilyIO, Durative: true,
		Params: []ParamDecl{{Name: "ms", Type: ParamNonNegInt, Unit: "milliseconds per operation"}},
		Doc:    "delay filesystem operations",
	},
	{
		Kind: FaultIOError, Family: FamilyIO, Durative: true,
		Params: []ParamDecl{{Name: "rate", Type: ParamRate, Unit: "fraction of operations"}},
		Doc:    "fail a fraction of filesystem operations",
	},
	{
		Kind: FaultIOFill, Family: FamilyIO, Durative: true,
		// ADDITIVE parameter name. The directive names `io.fill` only in the
		// sample config's deny list and gives it no parameter list; a disk-fill
		// fault is meaningless without a magnitude, so `pct` is declared here
		// and marked. See DECISIONS.md.
		Params: []ParamDecl{{Name: "pct", Type: ParamPercent, Unit: "percent of the filesystem to occupy"}},
		Doc:    "occupy a percentage of the target's filesystem, releasing it on withdraw",
	},
	{
		Kind: FaultMemPressure, Family: FamilyMem, Durative: true,
		Params: []ParamDecl{{Name: "pct", Type: ParamPercent, Unit: "percent of the memory limit"}},
		Doc:    "hold a percentage of the target's memory limit resident",
	},
	{
		Kind: FaultFDExhaust, Family: FamilyFD, Durative: true,
		Doc: "hold the target's file descriptor table near its limit",
	},
}

var faultByKind = func() map[FaultKind]KindDecl {
	m := make(map[FaultKind]KindDecl, len(faultRegistry))
	for _, d := range faultRegistry {
		m[d.Kind] = d
	}
	return m
}()

// AllFaultKinds is every registered kind, in registry order (network, process,
// clock, io, resource).
var AllFaultKinds = func() []FaultKind {
	out := make([]FaultKind, len(faultRegistry))
	for i, d := range faultRegistry {
		out[i] = d.Kind
	}
	return out
}()

// LookupFaultKind returns the registry entry for k.
func LookupFaultKind(k FaultKind) (KindDecl, bool) {
	d, ok := faultByKind[k]
	return d, ok
}

// Valid reports whether k is one of the seventeen registered kinds.
func (k FaultKind) Valid() bool {
	_, ok := faultByKind[k]
	return ok
}

// Family returns the injection family of k, or "" if k is unregistered.
func (k FaultKind) Family() FaultFamily {
	if d, ok := faultByKind[k]; ok {
		return d.Family
	}
	return ""
}

// Durative reports whether k is active over its window rather than firing at
// its start. It is false for an unregistered kind.
func (k FaultKind) Durative() bool {
	if d, ok := faultByKind[k]; ok {
		return d.Durative
	}
	return false
}

func (k FaultKind) String() string { return string(k) }

// UnmarshalYAML decodes `perturber.allow` / `perturber.deny` entries strictly.
//
// A misspelled kind that silently matched nothing would be a way to appear to
// have widened the fault space while changing nothing, so an unregistered kind
// is a CONFIG_ERROR rather than an ignored line.
func (k *FaultKind) UnmarshalYAML(n *yaml.Node) error {
	s, err := stringScalar(n, "fault kind")
	if err != nil {
		return err
	}
	if !FaultKind(s).Valid() {
		return fmt.Errorf("fault kind: unknown kind %q at line %d (want one of %s)",
			s, n.Line, joinFaultKinds(AllFaultKinds))
	}
	*k = FaultKind(s)
	return nil
}

func joinFaultKinds(ks []FaultKind) string {
	parts := make([]string, len(ks))
	for i, k := range ks {
		parts[i] = string(k)
	}
	return strings.Join(parts, ", ")
}

// ---------------------------------------------------------------------------
// parameter value validation
// ---------------------------------------------------------------------------

// validateParamValue checks a parameter's SOURCE TEXT against its declared
// domain without converting it. The text is what gets re-emitted.
func validateParamValue(kind FaultKind, p ParamDecl, text string) error {
	bad := func(why string) error {
		return fmt.Errorf("%s: parameter %s=%q: %s (want %s%s)",
			kind, p.Name, text, why, p.Type, unitSuffix(p))
	}
	if text == "" {
		return bad("empty value")
	}
	switch p.Type {
	case ParamInt:
		if _, err := strconv.ParseInt(text, 10, 64); err != nil {
			return bad("not a decimal integer")
		}
	case ParamNonNegInt:
		v, err := strconv.ParseInt(text, 10, 64)
		if err != nil {
			return bad("not a decimal integer")
		}
		if v < 0 {
			return bad("negative")
		}
	case ParamPercent:
		v, err := strconv.ParseFloat(text, 64)
		if err != nil {
			return bad("not a number")
		}
		if v < 0 || v > 100 {
			return bad("outside [0, 100]")
		}
	case ParamRate:
		v, err := strconv.ParseFloat(text, 64)
		if err != nil {
			return bad("not a number")
		}
		if v < 0 || v > 1 {
			return bad("outside [0, 1]")
		}
	case ParamSignal:
		if !validSignalText(text) {
			return bad("not a signal")
		}
	}
	return nil
}

func unitSuffix(p ParamDecl) string {
	if p.Unit == "" {
		return ""
	}
	return ", in " + p.Unit
}

// validSignalText accepts a SIGxxx name or a positive signal number. The set of
// deliverable signals is a backend and host question, not a schema one, so the
// schema checks only the shape.
func validSignalText(s string) bool {
	if n, err := strconv.Atoi(s); err == nil {
		return n > 0 && n < 128
	}
	if !strings.HasPrefix(s, "SIG") || len(s) < 4 {
		return false
	}
	for i := 3; i < len(s); i++ {
		c := s[i]
		if (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			continue
		}
		return false
	}
	return true
}
