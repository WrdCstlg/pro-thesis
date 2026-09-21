package oracle

import (
	"fmt"
	"time"

	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// ---------------------------------------------------------------------------
// Built-in oracle tuning
//
// Every value here is a THRESHOLD, and a threshold is the cheapest way to
// weaken a gate without touching a line of oracle code: raise the SLO ceiling
// and no operation is ever stuck; widen the resource tolerance and no leak is
// ever detected; delete a pattern and no panic is ever seen. Invariant I6 exists
// because in an agentic loop that is exactly what gets tried.
//
// So: the defaults are compiled in, none of them is reachable from
// prothesis.yaml today (the directive defines no config field for any of them;
// OQ-019), and Options.Fingerprint gives Phase 3's lock manifest a single value
// to cover.
// ---------------------------------------------------------------------------

// Defaults. Each is justified where it is used.
const (
	// DefaultStuckOpSLO is the ceiling no_stuck_op measures against.
	//
	// The directive names no config field for it (Phase 1 deliverable 6 says
	// only "exceeding SLO ceiling"), so this is a documented default rather
	// than an invented config key: see OPEN_QUESTIONS.md OQ-019.
	//
	// Five seconds is chosen against the reference workload rather than picked
	// from the air: the fixture's load generator uses per-operation deadlines of
	// 500 ms (read), 1 s (write) and 1.5 s (transaction), so an operation still
	// outstanding five seconds after the system healed has outlived the client's
	// own patience by more than 3x. A driver that honours directive 4.4 records
	// such an operation as `info` when it gives up; one that does not leaves an
	// invoke with no completion, which is exactly what this oracle reports.
	DefaultStuckOpSLO = 5 * time.Second

	// DefaultConvergenceWindow is how long after HEAL availability_after_heal
	// waits for health probes to answer, when the run carries no QUIESCE window
	// to use instead.
	//
	// Thirty seconds is the directive's own health probe timeout in the section
	// 4.2 sample config, which is the only convergence-scale number either
	// document states.
	DefaultConvergenceWindow = 30 * time.Second

	// DefaultResourceTolerancePct is the +N% band resource_return_to_baseline
	// allows around the pre-DRIVE baseline.
	//
	// Deliberately generous. This is a smoke-level check running against a
	// handful of samples from a process that has just been through fault
	// injection; a tight bound would produce noise, and a resource oracle that
	// cries wolf is one an operator learns to ignore, which is worse than not
	// having it.
	DefaultResourceTolerancePct = 50

	// DefaultQueueMinSamples is the number of QUIESCE samples no_unbounded_queue
	// needs before it will call a rise monotonic. Addendum A.3 uses the same
	// figure for its "monotonically increased for >=3 consecutive samples"
	// criterion. Fewer samples than this is inconclusive, not a pass.
	DefaultQueueMinSamples = 3
)

// Default absolute slack per metric for resource_return_to_baseline.
//
// A percentage alone is wrong at small magnitudes: 50% of a 4-goroutine
// baseline is 2, so a healthy process that keeps one extra worker alive would
// be reported as leaking. A finding must exceed BOTH the percentage and the
// absolute slack.
const (
	DefaultRSSSlackBytes   = 64 << 20 // 64 MiB
	DefaultFDSlack         = 32
	DefaultGoroutineSlack  = 32
	DefaultThreadSlack     = 32
	defaultUnknownMetricSl = 0
)

// Options carries every built-in's tuning.
type Options struct {
	NoCrash                  NoCrashOptions          `json:"no_crash"`
	NoPanicLog               NoPanicLogOptions       `json:"no_panic_log"`
	NoUnboundedQueue         NoUnboundedQueueOptions `json:"no_unbounded_queue"`
	ResourceReturnToBaseline ResourceOptions         `json:"resource_return_to_baseline"`
	AvailabilityAfterHeal    AvailabilityOptions     `json:"availability_after_heal"`
	NoStuckOp                NoStuckOpOptions        `json:"no_stuck_op"`
}

// DefaultOptions returns the compiled-in defaults.
func DefaultOptions() Options {
	return Options{
		NoCrash:                  DefaultNoCrashOptions(),
		NoPanicLog:               DefaultNoPanicLogOptions(),
		NoUnboundedQueue:         DefaultNoUnboundedQueueOptions(),
		ResourceReturnToBaseline: DefaultResourceOptions(),
		AvailabilityAfterHeal:    DefaultAvailabilityOptions(),
		NoStuckOp:                DefaultNoStuckOpOptions(),
	}
}

// Fingerprint is a "sha256:"-prefixed digest over the canonical encoding of the
// effective options.
//
// It is NOT the lock manifest: that is Phase 3 and lives in internal/lock. It
// is the value that manifest must include, so that widening a tolerance is a
// visible, human-reviewed change rather than an invisible one. Computed over
// the DEFAULTED options, because that is what actually ran.
func (o Options) Fingerprint() (string, error) {
	return schema.HashCanonical(o.withDefaults())
}

// withDefaults fills every zero value.
func (o Options) withDefaults() Options {
	o.NoCrash = o.NoCrash.withDefaults()
	o.NoPanicLog = o.NoPanicLog.withDefaults()
	o.NoUnboundedQueue = o.NoUnboundedQueue.withDefaults()
	o.ResourceReturnToBaseline = o.ResourceReturnToBaseline.withDefaults()
	o.AvailabilityAfterHeal = o.AvailabilityAfterHeal.withDefaults()
	o.NoStuckOp = o.NoStuckOp.withDefaults()
	return o
}

// ---------------------------------------------------------------------------
// per-oracle options
// ---------------------------------------------------------------------------

// NoCrashOptions tunes no_crash.
type NoCrashOptions struct {
	// FaultWindowGraceMS widens a planned fault window's end when deciding
	// whether a process exit was expected.
	//
	// It defaults to ZERO and should stay there without evidence. Every
	// millisecond of grace is a millisecond in which a genuine crash is excused
	// as planned, so this is the most direct gate-weakening knob in the package.
	// Phase 2 may need a small value for processes that take time to die under
	// SIGKILL; that change belongs in the lock.
	FaultWindowGraceMS int64 `json:"fault_window_grace_ms"`
}

// DefaultNoCrashOptions returns the defaults.
func DefaultNoCrashOptions() NoCrashOptions { return NoCrashOptions{FaultWindowGraceMS: 0} }

func (o NoCrashOptions) withDefaults() NoCrashOptions {
	if o.FaultWindowGraceMS < 0 {
		o.FaultWindowGraceMS = 0
	}
	return o
}

// NoUnboundedQueueOptions tunes no_unbounded_queue.
type NoUnboundedQueueOptions struct {
	// Metrics are the queue-depth metric names to check, in priority order.
	// The first one a node carries is used.
	Metrics []string `json:"metrics"`
	// MinSamples is the minimum number of samples inside QUIESCE required
	// before a verdict is possible. Below it the result is INCONCLUSIVE.
	MinSamples int `json:"min_samples"`
}

// DefaultNoUnboundedQueueOptions returns the defaults.
func DefaultNoUnboundedQueueOptions() NoUnboundedQueueOptions {
	return NoUnboundedQueueOptions{
		Metrics:    []string{MetricQueueDepth},
		MinSamples: DefaultQueueMinSamples,
	}
}

func (o NoUnboundedQueueOptions) withDefaults() NoUnboundedQueueOptions {
	if len(o.Metrics) == 0 {
		o.Metrics = []string{MetricQueueDepth}
	}
	if o.MinSamples <= 0 {
		o.MinSamples = DefaultQueueMinSamples
	}
	return o
}

// ResourceOptions tunes resource_return_to_baseline.
type ResourceOptions struct {
	// Metrics are the resource metrics to check. A node carrying none of them
	// is inconclusive, never a pass.
	Metrics []string `json:"metrics"`
	// TolerancePct is the +N% band around the baseline.
	TolerancePct int `json:"tolerance_pct"`
	// Slack is the absolute per-metric allowance. A finding must exceed both
	// this and TolerancePct.
	Slack map[string]int64 `json:"slack"`
}

// DefaultResourceOptions returns the defaults.
func DefaultResourceOptions() ResourceOptions {
	return ResourceOptions{
		Metrics:      []string{MetricRSSBytes, MetricFDCount, MetricGoroutines, MetricThreads},
		TolerancePct: DefaultResourceTolerancePct,
		Slack: map[string]int64{
			MetricRSSBytes:   DefaultRSSSlackBytes,
			MetricFDCount:    DefaultFDSlack,
			MetricGoroutines: DefaultGoroutineSlack,
			MetricThreads:    DefaultThreadSlack,
		},
	}
}

func (o ResourceOptions) withDefaults() ResourceOptions {
	d := DefaultResourceOptions()
	if len(o.Metrics) == 0 {
		o.Metrics = d.Metrics
	}
	if o.TolerancePct <= 0 {
		o.TolerancePct = d.TolerancePct
	}
	if o.Slack == nil {
		o.Slack = d.Slack
	}
	return o
}

// slackFor returns the absolute allowance for a metric.
func (o ResourceOptions) slackFor(metric string) int64 {
	if v, ok := o.Slack[metric]; ok {
		return v
	}
	return defaultUnknownMetricSl
}

// AvailabilityOptions tunes availability_after_heal.
type AvailabilityOptions struct {
	// ConvergenceWindow is how long after HEAL a node has to answer, used only
	// when the run carries no QUIESCE window. QUIESCE is the convergence grace
	// window by definition (directive 4.1 step 6), so it wins when present.
	ConvergenceWindow schema.Duration `json:"convergence_window"`
}

// DefaultAvailabilityOptions returns the defaults.
func DefaultAvailabilityOptions() AvailabilityOptions {
	return AvailabilityOptions{ConvergenceWindow: schema.Duration(DefaultConvergenceWindow)}
}

func (o AvailabilityOptions) withDefaults() AvailabilityOptions {
	if o.ConvergenceWindow <= 0 {
		o.ConvergenceWindow = schema.Duration(DefaultConvergenceWindow)
	}
	return o
}

// NoStuckOpOptions tunes no_stuck_op.
type NoStuckOpOptions struct {
	// SLOCeiling is the longest an operation may remain outstanding after HEAL
	// before it is reported as stuck. See DefaultStuckOpSLO and OQ-019.
	SLOCeiling schema.Duration `json:"slo_ceiling"`
}

// DefaultNoStuckOpOptions returns the defaults.
func DefaultNoStuckOpOptions() NoStuckOpOptions {
	return NoStuckOpOptions{SLOCeiling: schema.Duration(DefaultStuckOpSLO)}
}

func (o NoStuckOpOptions) withDefaults() NoStuckOpOptions {
	if o.SLOCeiling <= 0 {
		o.SLOCeiling = schema.Duration(DefaultStuckOpSLO)
	}
	return o
}

// ---------------------------------------------------------------------------
// construction
// ---------------------------------------------------------------------------

// NewBuiltin constructs one built-in by its normative name.
func NewBuiltin(name schema.BuiltinOracle, opts Options) (Oracle, error) {
	opts = opts.withDefaults()
	switch name {
	case schema.BuiltinNoCrash:
		return NewNoCrash(opts.NoCrash), nil
	case schema.BuiltinNoPanicLog:
		return NewNoPanicLog(opts.NoPanicLog)
	case schema.BuiltinNoUnboundedQueue:
		return NewNoUnboundedQueue(opts.NoUnboundedQueue), nil
	case schema.BuiltinResourceReturnToBaseline:
		return NewResourceReturnToBaseline(opts.ResourceReturnToBaseline), nil
	case schema.BuiltinAvailabilityAfterHeal:
		return NewAvailabilityAfterHeal(opts.AvailabilityAfterHeal), nil
	case schema.BuiltinNoStuckOp:
		return NewNoStuckOp(opts.NoStuckOp), nil
	default:
		return nil, fmt.Errorf("oracle: %q is not a built-in oracle (want one of %v)",
			name, schema.AllBuiltinOracles)
	}
}

// NewBuiltins constructs every built-in, in the directive's order.
func NewBuiltins(opts Options) ([]Oracle, error) {
	return newBuiltinList(schema.AllBuiltinOracles[:], opts)
}

// NewBuiltinsFromConfig constructs exactly the built-ins `oracles.builtin`
// lists, in the directive's canonical order rather than the file's order.
//
// It builds NOTHING that is not listed. The list is not defaulted to the full
// set anywhere in the tree, precisely so that deleting `no_stuck_op` from
// prothesis.yaml is a visible change that the Phase 3 lock will catch.
func NewBuiltinsFromConfig(cfg *schema.Config, opts Options) ([]Oracle, error) {
	if cfg == nil {
		return nil, fmt.Errorf("oracle: no configuration")
	}
	want := map[schema.BuiltinOracle]bool{}
	for _, b := range cfg.Oracles.Builtin {
		want[b] = true
	}
	ordered := make([]schema.BuiltinOracle, 0, len(want))
	for _, b := range schema.AllBuiltinOracles {
		if want[b] {
			ordered = append(ordered, b)
		}
	}
	if len(ordered) != len(want) {
		// Unreachable through DecodeConfig, which rejects unknown names, but a
		// hand-built Config could carry one and silently dropping it would mean
		// running fewer oracles than the config asked for.
		for b := range want {
			if !b.Valid() {
				return nil, fmt.Errorf("oracle: %q is not a built-in oracle (want one of %v)",
					b, schema.AllBuiltinOracles)
			}
		}
	}
	return newBuiltinList(ordered, opts)
}

func newBuiltinList(names []schema.BuiltinOracle, opts Options) ([]Oracle, error) {
	opts = opts.withDefaults()
	out := make([]Oracle, 0, len(names))
	for _, n := range names {
		o, err := NewBuiltin(n, opts)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, nil
}

// NewEngineWithBuiltins returns an engine carrying the built-ins named by the
// configuration.
func NewEngineWithBuiltins(cfg *schema.Config, opts Options) (*Engine, error) {
	os, err := NewBuiltinsFromConfig(cfg, opts)
	if err != nil {
		return nil, err
	}
	e := NewEngine()
	if err := e.RegisterAll(os); err != nil {
		return nil, err
	}
	return e, nil
}

// ---------------------------------------------------------------------------
// shared declaration helper
// ---------------------------------------------------------------------------

// builtinDecl carries the frozen Phase 0 declaration for a built-in.
//
// Class and ValidPhases are NOT restated here. They are read from
// schema.BuiltinOracle, which froze them in Phase 0; restating them in Phase 1
// would create two sources of truth for invariant I5's phase declaration, and
// the copy that drifts is always the one nobody is looking at.
type builtinDecl struct {
	id schema.BuiltinOracle
}

func (d builtinDecl) Name() string                { return string(d.id) }
func (d builtinDecl) Class() schema.OracleClass   { return d.id.Class() }
func (d builtinDecl) ValidPhases() []schema.Phase { return d.id.ValidPhases() }
