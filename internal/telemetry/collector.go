package telemetry

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/WrdCstlg/pro-thesis/internal/harness"
	"github.com/WrdCstlg/pro-thesis/internal/recorder"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// DefaultIntervalMS is the sampling interval in milliseconds.
//
// 500 ms is Addendum A.4's figure for container/process metrics and A.10's
// default for `observe.sample_interval_ms`. A.3's "every 200ms" is the
// probe-specific trajectory rate for the Phase 4 sweep, not this knob; the two
// are reconciled in OPEN_QUESTIONS.md OQ-006.2.
const DefaultIntervalMS = 500

// Target is one node to sample.
type Target struct {
	// Node is the logical node id from prothesis.yaml.
	Node string
	// Container is the container backing it, from the topology record.
	Container string
	// ProbeURL is the node's expanded health probe, or "" when it declares
	// none. Build it with harness.ExpandProbe so a node is sampled against
	// exactly the address WaitHealthy probed at BOOT.
	ProbeURL string
	// StatusURL is the target's own status endpoint, or "" when it has none.
	// A target with no status endpoint is fully supported; the metrics only it
	// can supply are then ABSENT, and an oracle needing one returns
	// INCONCLUSIVE.
	StatusURL string
}

// Options configures a Collector.
type Options struct {
	// Targets is the node set to sample. Required.
	Targets []Target
	// Sink receives every sample. Required.
	Sink Sink

	// Interval is the sampling period. Zero means DefaultIntervalMS.
	Interval time.Duration
	// CollectTimeout bounds one whole sampling round. Zero means four fifths of
	// Interval, so a stuck round is abandoned before the next one is due rather
	// than queueing behind it.
	CollectTimeout time.Duration

	// Docker is the engine interface. Zero means a CLI with default settings.
	Docker Docker
	// HTTPClient is used for probes and status. Zero means a client with
	// DefaultHTTPTimeout.
	HTTPClient *http.Client

	// Clock supplies the monotonic and wall readings on every sample. Zero
	// means a RealClock owned by the collector and closed with it.
	Clock recorder.Clock
	// Timeline converts a monotonic reading to virtual milliseconds. Zero, or
	// a timeline whose DRIVE origin has not been stamped, leaves Sample.VMS
	// null: which is correct, not degraded: virtual time does not exist before
	// DRIVE.
	Timeline *recorder.Timeline
	// PhaseFn reports the lifecycle phase in force. Zero leaves Sample.Phase
	// empty.
	PhaseFn func() schema.Phase

	// EnableDockerStats turns on the `docker stats` fallback.
	//
	// OFF by default, and that is a measurement rather than a preference: on the
	// build machine `docker stats --no-stream` cost 993 ms, roughly twice the
	// default interval, and it cannot produce a page-cache-free memory figure.
	// It is worth enabling only for a target with no POSIX shell, where the
	// choice is between a degraded CPU and memory-usage signal and none at all.
	EnableDockerStats bool

	// Retain keeps every sample in memory as well as writing it to the Sink.
	// Off by default: an 8-hour soak at 500 ms over three nodes is ~173,000
	// samples, and the document is materialised by reading the JSONL back
	// instead.
	Retain bool

	// OnError receives non-fatal collection errors. A collection error never
	// stops the loop: it becomes an absent metric with a recorded reason.
	OnError func(error)
}

// Sink receives samples as they are produced.
type Sink interface {
	Append(s *Sample) error
}

// Collector samples a set of targets on an interval.
//
// It is safe for concurrent use. Start runs the loop until its context ends or
// Stop is called; SampleRound takes one round synchronously, which is what the
// tests and the ASSERT-time final sample use.
type Collector struct {
	opts     Options
	interval time.Duration
	timeout  time.Duration
	docker   Docker
	client   *http.Client
	clock    recorder.Clock
	ownClock bool

	mu       sync.Mutex
	seq      map[string]int64
	prev     map[string]*Sample
	retained []Sample
	rounds   int64
	started  bool
	stopped  bool
	done     chan struct{}
	stopOnce sync.Once
	cancel   context.CancelFunc
	firstTNS int64
	lastTNS  int64
}

// New builds a Collector.
func New(opts Options) (*Collector, error) {
	if len(opts.Targets) == 0 {
		return nil, errors.New("telemetry: no targets to sample")
	}
	if opts.Sink == nil {
		return nil, errors.New("telemetry: a sink is required")
	}
	seen := map[string]bool{}
	for i, t := range opts.Targets {
		if t.Node == "" {
			return nil, fmt.Errorf("telemetry: targets[%d] has no node id", i)
		}
		if seen[t.Node] {
			return nil, fmt.Errorf("telemetry: duplicate target node %q", t.Node)
		}
		seen[t.Node] = true
	}

	c := &Collector{
		opts:     opts,
		interval: opts.Interval,
		timeout:  opts.CollectTimeout,
		docker:   opts.Docker,
		client:   opts.HTTPClient,
		clock:    opts.Clock,
		seq:      map[string]int64{},
		prev:     map[string]*Sample{},
		done:     make(chan struct{}),
	}
	if c.interval <= 0 {
		c.interval = DefaultIntervalMS * time.Millisecond
	}
	if c.timeout <= 0 {
		c.timeout = c.interval * 4 / 5
	}
	if c.timeout <= 0 {
		c.timeout = c.interval
	}
	if c.docker == nil {
		c.docker = &CLI{}
	}
	if c.client == nil {
		c.client = &http.Client{Timeout: DefaultHTTPTimeout}
	}
	if c.clock == nil {
		// SkipCalibration and a disabled drift watcher: the collector's clock is
		// a timestamp source, and the run's own clock (which does calibrate and
		// does watch drift) is what the harness passes in production.
		c.clock = recorder.NewRealClock(recorder.RealClockOptions{
			SkipCalibration: true,
			DriftInterval:   -1,
		})
		c.ownClock = true
	}
	return c, nil
}

// Interval returns the effective sampling period.
func (c *Collector) Interval() time.Duration { return c.interval }

// Start begins sampling in the background and returns immediately.
//
// The first round runs at once rather than after one interval, so a short phase
// still produces at least one sample. Sampling continues until ctx ends or Stop
// is called.
func (c *Collector) Start(ctx context.Context) error {
	c.mu.Lock()
	if c.started {
		c.mu.Unlock()
		return errors.New("telemetry: collector is already started")
	}
	c.started = true
	ctx, c.cancel = context.WithCancel(ctx)
	c.mu.Unlock()

	go c.loop(ctx)
	return nil
}

func (c *Collector) loop(ctx context.Context) {
	defer close(c.done)
	t := time.NewTicker(c.interval)
	defer t.Stop()
	for {
		c.SampleRound(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Stop ends the loop and waits for the in-flight round to finish. Idempotent.
func (c *Collector) Stop() {
	c.stopOnce.Do(func() {
		c.mu.Lock()
		started := c.started
		cancel := c.cancel
		c.stopped = true
		c.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		if started {
			<-c.done
		}
		if c.ownClock {
			_ = c.clock.Close()
		}
	})
}

// Rounds returns how many sampling rounds have completed.
func (c *Collector) Rounds() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.rounds
}

// Retained returns the retained samples, or nil when Options.Retain was false.
func (c *Collector) Retained() []Sample {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Sample(nil), c.retained...)
}

// Window returns the first and last sample timestamps observed, in Unix epoch
// nanoseconds, and whether any sample was taken.
func (c *Collector) Window() (first, last int64, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.rounds == 0 {
		return 0, 0, false
	}
	return c.firstTNS, c.lastTNS, true
}

// SampleRound takes one synchronous sample of every target.
//
// It never returns an error. A docker call that fails, a container that has
// vanished, a probe that times out: each becomes an ABSENT metric with a
// recorded reason on the affected sample, and the round still produces a sample
// for every target. A collector that stopped on the first failure would stop
// exactly when the system under test started misbehaving, which is when its
// output matters most.
func (c *Collector) SampleRound(ctx context.Context) []*Sample {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	now := c.clock.NowR()
	tNS := c.clock.NowWall().UnixNano()
	startWall := time.Now()

	var vms *int64
	if c.opts.Timeline != nil {
		if v, err := c.opts.Timeline.V(now); err == nil {
			vms = i64(v.MS())
		}
	}
	phase := schema.Phase("")
	if c.opts.PhaseFn != nil {
		phase = c.opts.PhaseFn()
	}

	containers := make([]string, 0, len(c.opts.Targets))
	for _, t := range c.opts.Targets {
		if t.Container != "" {
			containers = append(containers, t.Container)
		}
	}

	states, stateErr := c.docker.InspectStates(ctx, containers)
	if stateErr != nil {
		c.reportError(fmt.Errorf("telemetry: docker inspect: %w", stateErr))
	}

	var stats map[string]DockerStats
	if c.opts.EnableDockerStats {
		var err error
		stats, err = c.docker.Stats(ctx, containers)
		if err != nil {
			c.reportError(fmt.Errorf("telemetry: docker stats: %w", err))
		}
	}

	samples := make([]*Sample, len(c.opts.Targets))
	var wg sync.WaitGroup
	for i, t := range c.opts.Targets {
		wg.Add(1)
		go func(i int, t Target) {
			defer wg.Done()
			s := &Sample{
				Schema:    SampleSchema,
				Node:      t.Node,
				Container: t.Container,
				TNS:       tNS,
				RTNS:      int64(now),
				Phase:     phase,
			}
			if vms != nil {
				s.VMS = i64(*vms)
			}
			c.sampleTarget(ctx, s, t, states, stats, stateErr)
			samples[i] = s
		}(i, t)
	}
	wg.Wait()

	collectUS := time.Since(startWall).Microseconds()

	c.mu.Lock()
	for _, s := range samples {
		s.CollectUS = collectUS
		s.Seq = c.seq[s.Node]
		c.seq[s.Node]++
		deriveCPURate(s, c.prev[s.Node])
		snapshot := *s
		c.prev[s.Node] = &snapshot
		if c.opts.Retain {
			c.retained = append(c.retained, *s)
		}
	}
	if c.rounds == 0 {
		c.firstTNS = tNS
	}
	c.lastTNS = tNS
	c.rounds++
	c.mu.Unlock()

	for _, s := range samples {
		if err := c.opts.Sink.Append(s); err != nil {
			c.reportError(fmt.Errorf("telemetry: append sample for %s: %w", s.Node, err))
		}
	}
	return samples
}

// sampleTarget gathers one node's metrics.
func (c *Collector) sampleTarget(ctx context.Context, s *Sample, t Target,
	states map[string]ContainerState, stats map[string]DockerStats, stateErr error) {

	var state ContainerState
	haveState := false
	if t.Container == "" {
		s.MarkAbsent(MetricProcess, "the topology binds no container for this node")
	} else if st, ok := states[t.Container]; ok {
		applyState(s, st)
		state, haveState = st, true
	} else if stateErr != nil {
		s.MarkAbsent(MetricProcess, "docker inspect failed: %v", stateErr)
	} else {
		s.MarkAbsent(MetricProcess, "docker knows no container %q; it has been removed", t.Container)
	}

	// Read the cgroup only when the container can answer.
	switch {
	case t.Container == "":
		c.markContainerMetricsAbsent(s, "the topology binds no container for this node")
	case haveState && state.Paused:
		// A paused container refuses exec outright: the daemon replies
		// "Container is paused, unpause the container before exec". This is not
		// a collection failure: it is a true statement about the world, and
		// recording it as such is what lets an oracle distinguish "we could not
		// measure" from "the process was frozen by a fault".
		c.markContainerMetricsAbsent(s, "container is paused, so its cgroup cannot be read")
	case haveState && !state.Running:
		c.markContainerMetricsAbsent(s, "container is not running (status %q)", state.Status)
	default:
		out, err := c.docker.ExecScript(ctx, t.Container, readScript)
		if err != nil {
			c.markContainerMetricsAbsent(s, "%s", describeExecFailure(err))
		} else {
			r := parseSections(out)
			applyMemory(s, r)
			applyCPU(s, r)
			applyTasks(s, r)
		}
	}

	if row, ok := stats[t.Container]; ok {
		applyDockerStats(s, row)
	}

	collectProbe(ctx, c.client, s, t.ProbeURL)
	collectStatus(ctx, c.client, s, t.StatusURL)
}

// markContainerMetricsAbsent records the same reason against every metric the
// in-container read would have supplied.
func (c *Collector) markContainerMetricsAbsent(s *Sample, format string, a ...any) {
	reason := fmt.Sprintf(format, a...)
	s.MarkAbsent(MetricMemory, "%s", reason)
	s.MarkAbsent(MetricMemoryRSS, "%s", reason)
	s.MarkAbsent(MetricCPU, "%s", reason)
	s.MarkAbsent(MetricCPUMilliPct, "%s", reason)
	s.MarkAbsent(MetricTasks, "%s", reason)
	s.MarkAbsent(MetricTasksOpenFDs, "%s", reason)
}

func (c *Collector) reportError(err error) {
	if c.opts.OnError != nil {
		c.opts.OnError(err)
	}
}

// ---------------------------------------------------------------------------
// building targets from the harness's own view of the world
// ---------------------------------------------------------------------------

// StatusPath is the conventional path of a target's own observability endpoint.
//
// ADDITIVE and advisory. prothesis.yaml has no field naming a status endpoint,
// so TargetsFromTopology probes this path on the same published port as the
// health probe. A target that serves nothing there simply has those metrics
// absent: it is never an error, and the tool still runs against a target that
// has never heard of PRO-THESIS.
const StatusPath = "/status"

// TargetsFromTopology derives the sampling set from a validated config and the
// topology `thesis up` persisted.
//
// Probe URLs come from harness.ExpandProbe and harness.ResolveProbeTargets:
// the SAME functions WaitHealthy uses. Re-implementing either here would let
// BOOT and DRIVE disagree about which address a node lives at, and the
// disagreement would show up as a node that was healthy at boot and
// unreachable ever after.
func TargetsFromTopology(cfg *schema.Config, top *recorder.Topology) ([]Target, error) {
	if cfg == nil || top == nil {
		return nil, errors.New("telemetry: TargetsFromTopology needs a config and a topology")
	}
	probeByNode, err := harness.ProbeTemplates(cfg)
	if err != nil {
		return nil, err
	}

	out := make([]Target, 0, len(top.Nodes))
	for _, nb := range top.Nodes {
		t := Target{Node: nb.ID, Container: nb.ContainerID}
		if tmpl, ok := probeByNode[nb.ID]; ok && nb.HostPort > 0 {
			t.ProbeURL = harness.ExpandProbe(tmpl, harness.ProbeHost(), nb.HostPort)
		}
		if nb.HostPort > 0 {
			t.StatusURL = harness.ExpandProbe("http://{host}:{port}"+StatusPath, harness.ProbeHost(), nb.HostPort)
		}
		out = append(out, t)
	}
	if len(out) == 0 {
		return nil, errors.New("telemetry: the topology binds no nodes to sample")
	}
	return out, nil
}
