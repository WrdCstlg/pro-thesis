package oracle

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"time"

	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// ---------------------------------------------------------------------------
// Input: everything an oracle is allowed to look at
// ---------------------------------------------------------------------------

// Input is the evidence collected for one world execution.
//
// It is READ-ONLY for oracles. The engine hands the same pointer to every
// oracle in a phase, so a mutation would silently change what the next oracle
// sees; nothing in this package writes through it.
//
// Input is the in-process counterpart of prothesis.oracle_input/v1: that
// document carries PATHS for an external process to open, while a built-in
// receives the already-collected values. Whoever fills this in is also
// responsible for writing the corresponding OracleInput document, so the two
// views of a run never disagree: see Input.OracleInput.
type Input struct {
	// RunID is the run this world belongs to.
	RunID string
	// WorldPath is the .thesis file for the executed world.
	WorldPath string
	// HistoryPath is the merged canonical history.jsonl.
	HistoryPath string
	// FinalStatePath and TelemetryPath are carried so the OracleInput document
	// can be emitted from the same value. Built-ins do not read them.
	FinalStatePath string
	TelemetryPath  string

	// ArtifactDir is the world's artifact directory: the run bundle this
	// evaluation writes into.
	//
	// ADDITIVE, and NOT part of prothesis.oracle_input/v1: it is where the
	// harness puts things ABOUT an oracle, not something an oracle is given.
	// Phase 3's external oracle runner captures each process's stderr under
	// <ArtifactDir>/oracles/, because an oracle that fails needs to be
	// debuggable and its diagnostics are otherwise thrown away with the pipe.
	// Empty means "capture in memory only", which is what a unit test wants.
	ArtifactDir string

	// Phases are the MEASURED lifecycle windows, in milliseconds relative to
	// DRIVE start. Windows before DRIVE are negative. They may overlap: PERTURB
	// is contained within DRIVE.
	Phases schema.PhaseTimings

	// DriveOriginWallNS is the wall-clock Unix epoch nanosecond value of DRIVE
	// start: the anchor that converts a history record's t_ns into the
	// millisecond frame everything else uses (DECISIONS.md D-011, OQ-008).
	//
	// Zero means "not recorded". An oracle that needs the conversion must then
	// return INCONCLUSIVE rather than guess an origin.
	DriveOriginWallNS int64

	// Nodes is one entry per logical node in harness.nodes.
	Nodes []NodeObservation

	// Logs are the collected log streams. A stream may be node-scoped or not.
	Logs []LogStream

	// Probes are health probe observations, in the order they were taken.
	Probes []ProbeObservation

	// HealthProbed names the nodes for which harness.health DECLARES a probe.
	// A node absent from it has no definition of "available" (nobody wrote
	// one) so availability_after_heal reports it as not judged rather than as
	// unprobed (INCONCLUSIVE) or as unavailable. Nil means "not recorded", and
	// every node is then treated as declared, the pre-D-066 behaviour.
	HealthProbed []string

	// History is the parsed history log. Nil means it was not collected, which
	// is an inconclusive input for every history-based oracle, not an empty one.
	History *History

	// PlannedFaults are the fault windows the Perturber scheduled for this
	// world. Phase 1 injects nothing, so this is empty and every unexpected
	// process exit is unexcused. Phase 2 fills it from the REALIZED schedule:
	// the one that records which concrete node a fault bound to (D-012).
	PlannedFaults []FaultWindow
}

// Window returns the measured window for a phase.
func (in *Input) Window(p schema.Phase) (schema.PhaseWindow, bool) {
	if in == nil {
		return schema.PhaseWindow{}, false
	}
	return in.Phases.Lookup(p)
}

// Node returns the observation for a logical node id.
func (in *Input) Node(id string) (NodeObservation, bool) {
	for _, n := range in.Nodes {
		if n.NodeID == id {
			return n, true
		}
	}
	return NodeObservation{}, false
}

// MSFromEpochNS converts a history record's Unix epoch nanosecond timestamp
// into milliseconds relative to DRIVE start.
//
// The second return is false when no DRIVE origin was recorded. There is no
// defensible fallback: without the anchor a history timestamp cannot be placed
// on the phase timeline at all, which is precisely the situation I5 requires an
// oracle to report rather than paper over.
func (in *Input) MSFromEpochNS(tNS int64) (int64, bool) {
	if in == nil || in.DriveOriginWallNS == 0 {
		return 0, false
	}
	return floorDivMS(tNS - in.DriveOriginWallNS), true
}

// floorDivMS converts nanoseconds to milliseconds, rounding toward negative
// infinity so that a record 1 ns before DRIVE start lands at -1 ms rather than
// at 0. Go's integer division truncates toward zero, which would put pre-DRIVE
// records inside DRIVE.
func floorDivMS(ns int64) int64 {
	const d = int64(time.Millisecond)
	q, r := ns/d, ns%d
	if r != 0 && ((r < 0) != (d < 0)) {
		q--
	}
	return q
}

// ObservationEndMS is the last moment about which this input carries evidence:
// the later of the final history record and the end of the last measured phase
// window.
//
// Both sources matter. The history alone under-reports (a driver killed with
// operations in flight simply stops writing), and the phase timings alone say
// nothing about whether the driver was still being observed.
func (in *Input) ObservationEndMS() (int64, bool) {
	var end int64
	ok := false
	for _, w := range in.Phases {
		if !ok || w.EndMS > end {
			end, ok = w.EndMS, true
		}
	}
	if in.History != nil {
		for i := len(in.History.Entries) - 1; i >= 0; i-- {
			ms, conv := in.MSFromEpochNS(in.History.Entries[i].TNS)
			if !conv {
				break
			}
			if !ok || ms > end {
				end, ok = ms, true
			}
			break
		}
	}
	return end, ok
}

// PlannedFaultsFor returns the planned fault windows that name a node.
func (in *Input) PlannedFaultsFor(nodeID string) []FaultWindow {
	out := make([]FaultWindow, 0, len(in.PlannedFaults))
	for _, f := range in.PlannedFaults {
		if f.Names(nodeID) {
			out = append(out, f)
		}
	}
	return out
}

// LogsFor returns the log streams scoped to a node, plus every unscoped stream.
func (in *Input) LogsFor(nodeID string) []LogStream {
	out := make([]LogStream, 0, len(in.Logs))
	for _, l := range in.Logs {
		if l.NodeID == nodeID || l.NodeID == "" {
			out = append(out, l)
		}
	}
	return out
}

// OracleInput renders the prothesis.oracle_input/v1 document for this input, so
// a Phase 3 external oracle and a Phase 1 built-in are handed the same run.
func (in *Input) OracleInput() schema.OracleInput {
	return schema.NewOracleInput(in.HistoryPath, in.FinalStatePath, in.TelemetryPath, in.WorldPath, in.Phases)
}

// ---------------------------------------------------------------------------
// node state
// ---------------------------------------------------------------------------

// NodeObservation is what the harness saw of one logical node.
type NodeObservation struct {
	// NodeID is the logical id from harness.nodes.
	NodeID string
	// Service is the logical group (the fault grammar's `kv` in `kv:*`).
	Service string
	// ContainerID is the backing container, when there is one.
	ContainerID string

	// StateObserved reports that the backend actually inspected this node's
	// process.
	//
	// Without it, Exit == nil is ambiguous between "the process never exited"
	// and "nobody looked", and no_crash would report a clean pass over a node it
	// never examined. False here is an inconclusive input, not a healthy one.
	StateObserved bool

	// Running is the process's liveness at collection time. Only meaningful
	// when StateObserved is true.
	Running bool

	// Exit is the process's exit record, or nil if it never exited.
	Exit *ProcessExit

	// RestartCount is how many times the supervisor restarted the process. A
	// non-zero count means the process died even if it is running now.
	RestartCount int

	// Metrics are the telemetry series sampled for this node.
	Metrics []Series

	// Baseline holds explicitly measured pre-DRIVE values. When a metric is
	// absent here, BaselineFor falls back to the last sample at or before DRIVE
	// start.
	Baseline []Metric
}

// ProcessExit records how a process ended.
type ProcessExit struct {
	// Code is the process exit status.
	Code int
	// Signal is the terminating signal name when known ("SIGKILL", "SIGSEGV").
	Signal string
	// OOMKilled reports that the kernel's OOM killer chose this process.
	OOMKilled bool
	// Error is the engine's own error string for the container, when set.
	Error string
	// AtMS is when the process exited, in milliseconds relative to DRIVE start.
	// Nil means the time was not observed, which no planned fault window can
	// excuse, because an exit that cannot be placed on the timeline cannot be
	// shown to fall inside one.
	AtMS *int64
}

// ExitedAt returns the exit time and whether it is known.
func (e *ProcessExit) ExitedAt() (int64, bool) {
	if e == nil || e.AtMS == nil {
		return 0, false
	}
	return *e.AtMS, true
}

// Describe renders the exit for an explanation string.
func (e *ProcessExit) Describe() string {
	if e == nil {
		return "no exit record"
	}
	s := fmt.Sprintf("exit code %d", e.Code)
	if e.Signal != "" {
		s += " (" + e.Signal + ")"
	}
	if e.OOMKilled {
		s += ", OOM-killed"
	}
	if e.Error != "" {
		s += ", engine error: " + e.Error
	}
	return s
}

// ---------------------------------------------------------------------------
// telemetry
// ---------------------------------------------------------------------------

// Telemetry metric names.
//
// Values are INTEGERS throughout. Every quantity here is naturally integral
// (bytes, counts, depths) and integer comparison keeps a threshold decision
// from depending on float formatting.
const (
	// MetricQueueDepth is the depth of the node's primary work queue.
	MetricQueueDepth = "queue_depth"

	// MetricRSSBytes is the process's ANONYMOUS resident set size in bytes.
	//
	// The distinction is load-bearing. A collector that reports a cgroup's
	// memory.current, or any other page-cache-inclusive figure, measures file
	// cache that grows with log volume and is reclaimed on demand, so
	// resource_return_to_baseline would fire on a healthy system that merely
	// wrote a lot of logs. Report anonymous RSS (the fixture's /status
	// rss_bytes, read from /proc/self/statm) or subtract inactive_file.
	MetricRSSBytes = "rss_bytes"

	// MetricFDCount is the number of open file descriptors.
	MetricFDCount = "fd_count"
	// MetricGoroutines is the goroutine count of a Go process.
	MetricGoroutines = "goroutines"
	// MetricThreads is the OS thread count, for non-Go processes.
	MetricThreads = "threads"
)

// Sample is one telemetry point.
type Sample struct {
	// TMS is the sample time in milliseconds relative to DRIVE start. Samples
	// taken during BOOT or SEED are negative.
	TMS int64
	// Value is the measured value.
	Value int64
}

// Series is one metric's samples for one node, in time order.
type Series struct {
	Metric string
	Points []Sample
}

// Metric is a single named value, used for explicitly measured baselines.
type Metric struct {
	Name  string
	Value int64
}

// Series returns the named series for a node.
func (n NodeObservation) Series(metric string) (Series, bool) {
	for _, s := range n.Metrics {
		if s.Metric == metric {
			return s, true
		}
	}
	return Series{}, false
}

// HasMetric reports whether the node carries any sample for a metric.
func (n NodeObservation) HasMetric(metric string) bool {
	s, ok := n.Series(metric)
	return ok && len(s.Points) > 0
}

// BaselineFor returns the pre-DRIVE value of a metric.
//
// An explicitly measured baseline wins. Otherwise the last sample at or before
// DRIVE start is used, because DRIVE start is the virtual clock origin and
// everything at or before it was taken while the system was still undisturbed.
func (n NodeObservation) BaselineFor(metric string) (int64, bool) {
	for _, m := range n.Baseline {
		if m.Name == metric {
			return m.Value, true
		}
	}
	s, ok := n.Series(metric)
	if !ok {
		return 0, false
	}
	return s.LastAtOrBefore(0)
}

// InWindow returns the samples whose time falls in [w.StartMS, w.EndMS).
func (s Series) InWindow(w schema.PhaseWindow) []Sample {
	out := make([]Sample, 0, len(s.Points))
	for _, p := range s.Points {
		if w.Contains(p.TMS) {
			out = append(out, p)
		}
	}
	return out
}

// From returns the samples at or after ms.
func (s Series) From(ms int64) []Sample {
	out := make([]Sample, 0, len(s.Points))
	for _, p := range s.Points {
		if p.TMS >= ms {
			out = append(out, p)
		}
	}
	return out
}

// LastAtOrBefore returns the value of the latest sample at or before ms.
func (s Series) LastAtOrBefore(ms int64) (int64, bool) {
	var (
		best Sample
		ok   bool
	)
	for _, p := range s.Points {
		if p.TMS > ms {
			continue
		}
		if !ok || p.TMS >= best.TMS {
			best, ok = p, true
		}
	}
	return best.Value, ok
}

// LastFrom returns the latest sample at or after ms.
func (s Series) LastFrom(ms int64) (Sample, bool) {
	var (
		best Sample
		ok   bool
	)
	for _, p := range s.Points {
		if p.TMS < ms {
			continue
		}
		if !ok || p.TMS >= best.TMS {
			best, ok = p, true
		}
	}
	return best, ok
}

// ---------------------------------------------------------------------------
// probes
// ---------------------------------------------------------------------------

// ProbeObservation is one health probe attempt.
type ProbeObservation struct {
	// NodeID is the node probed.
	NodeID string
	// Target is the expanded probe URL, for the witness.
	Target string
	// TMS is when the probe was attempted, relative to DRIVE start.
	TMS int64
	// OK reports whether the probe answered successfully.
	OK bool
	// LatencyMS is how long the probe took.
	LatencyMS int64
	// Err is the failure detail when OK is false.
	Err string
}

// ---------------------------------------------------------------------------
// planned fault windows
// ---------------------------------------------------------------------------

// FaultWindow is one scheduled fault, reduced to what an oracle needs: when it
// was active and which concrete nodes it hit.
//
// Nodes is the REALIZED binding, not the target expression. `role:leader` and
// `minority(kv)` bind to concrete nodes at injection time from live cluster
// state, so an oracle cannot resolve them after the fact (D-012).
type FaultWindow struct {
	// Kind is the fault kind, e.g. "proc.kill".
	Kind string
	// Target is the original target expression, for explanations.
	Target string
	// Nodes are the concrete node ids the fault was injected against.
	//
	// An EMPTY list excuses nothing. A window whose binding was not recorded
	// cannot show that any particular node's exit was planned, and treating
	// "unknown" as "planned" would let an unrecorded fault silence every crash
	// in its time range.
	Nodes []string
	// StartMS and EndMS bound the fault, relative to DRIVE start.
	StartMS int64
	EndMS   int64
}

// Names reports whether the window records this node as a concrete target.
func (f FaultWindow) Names(nodeID string) bool {
	for _, n := range f.Nodes {
		if n == nodeID {
			return true
		}
	}
	return false
}

// Covers reports whether the window excuses an event on a node at ms.
// graceMS widens the window's end for a process that takes time to die.
func (f FaultWindow) Covers(nodeID string, ms, graceMS int64) bool {
	if !f.Names(nodeID) {
		return false
	}
	return ms >= f.StartMS && ms <= f.EndMS+graceMS
}

// String renders the window in the fault grammar's shape.
func (f FaultWindow) String() string {
	t := f.Target
	if t == "" {
		t = "?"
	}
	return fmt.Sprintf("%s(%s)@%d..%d", f.Kind, t, f.StartMS, f.EndMS)
}

// ---------------------------------------------------------------------------
// logs
// ---------------------------------------------------------------------------

// LogStream is one collected log.
//
// Bytes wins when set, so a test can hand-build a stream without touching the
// filesystem and a collector can hand over an in-memory capture. Otherwise Path
// is opened lazily: a log can be hundreds of megabytes, and no_panic_log
// streams it rather than reading it whole.
type LogStream struct {
	// NodeID scopes the stream to a node. Empty means unscoped (the driver's
	// own log, for instance).
	NodeID string
	// Name distinguishes several streams for one node ("stdout", "kv.log").
	Name string
	// Path is the file to read when Bytes is nil.
	Path string
	// Bytes is an in-memory stream.
	Bytes []byte
}

// ErrNoLogSource is returned by Open for a stream with neither bytes nor path.
var ErrNoLogSource = errors.New("oracle: log stream has neither bytes nor a path")

// Open returns a reader over the stream. The caller closes it.
func (l LogStream) Open() (io.ReadCloser, error) {
	if l.Bytes != nil {
		return io.NopCloser(bytes.NewReader(l.Bytes)), nil
	}
	if l.Path == "" {
		return nil, fmt.Errorf("%w (label %q)", ErrNoLogSource, l.Label())
	}
	f, err := os.Open(l.Path)
	if err != nil {
		return nil, err
	}
	return f, nil
}

// Label names the stream in an explanation or witness.
func (l LogStream) Label() string {
	switch {
	case l.NodeID != "" && l.Name != "":
		return l.NodeID + "/" + l.Name
	case l.NodeID != "":
		return l.NodeID
	case l.Name != "":
		return l.Name
	case l.Path != "":
		return l.Path
	default:
		return "(unnamed log)"
	}
}

// ---------------------------------------------------------------------------
// history
// ---------------------------------------------------------------------------

// History is a parsed history log plus an honest account of how completely it
// was read.
//
// The completeness fields are not bookkeeping. A single dropped line may be the
// completion record for an operation, so an oracle that pairs invokes with
// completions must report INCONCLUSIVE over an unreliable history rather than
// report the unpaired invoke as a stuck operation.
type History struct {
	// Path is where the history came from.
	Path string
	// Entries are the decoded records, in file order.
	Entries []schema.HistoryEntry
	// MalformedLines counts lines that did not decode.
	MalformedLines int
	// InvalidRecords counts lines that DECODED but violate the
	// operation/marker union: most importantly an operation record carrying an
	// `event`, which every marker-skipping reader silently discards.
	//
	// Counted separately from MalformedLines because the two say different
	// things to a reader of the explanation: one is a torn file, the other is a
	// driver emitting records whose kind cannot be determined. See OQ-058.
	InvalidRecords int
	// FirstInvalidReason is the first union violation seen, so the explanation
	// can name the actual problem rather than only its count.
	FirstInvalidReason string
	// Truncated reports that reading stopped before the end of the stream.
	Truncated bool
	// Err is why reading stopped, when it stopped early.
	Err string
}

// Reliable reports whether every line was read and decoded. A history that is
// not reliable is inconclusive evidence, not weak evidence.
func (h *History) Reliable() bool {
	return h != nil && h.MalformedLines == 0 && h.InvalidRecords == 0 &&
		!h.Truncated && h.Err == ""
}

// Unreliable renders why a history cannot be trusted, for an explanation.
func (h *History) Unreliable() string {
	switch {
	case h == nil:
		return "no history was collected"
	case h.Err != "":
		return fmt.Sprintf("reading %s stopped early: %s", h.describePath(), h.Err)
	case h.Truncated:
		return fmt.Sprintf("%s was truncated", h.describePath())
	case h.MalformedLines > 0:
		return fmt.Sprintf("%s has %d line(s) that did not decode; a dropped line may be the completion "+
			"record for an operation", h.describePath(), h.MalformedLines)
	case h.InvalidRecords > 0:
		return fmt.Sprintf("%s has %d record(s) whose kind cannot be determined (%s); a record "+
			"carrying an \"event\" is skipped as a marker, so an operation record that also "+
			"carries one is dropped without trace and the evidence it held is gone",
			h.describePath(), h.InvalidRecords, h.FirstInvalidReason)
	default:
		return ""
	}
}

func (h *History) describePath() string {
	if h == nil || h.Path == "" {
		return "the history"
	}
	return h.Path
}

// ReadHistory decodes a JSONL history from r.
//
// It TOLERATES malformed lines (a five-hundred-thousand-line history must not
// be thrown away over one torn line) and records how many it saw, so the
// oracles can decide. It never silently drops evidence.
func ReadHistory(r io.Reader, path string) *History {
	h := &History{Path: path, Entries: make([]schema.HistoryEntry, 0, 256)}
	hr := schema.NewHistoryReader(r)
	for {
		e, err := hr.Next()
		if errors.Is(err, io.EOF) {
			return h
		}
		if err != nil {
			var le *schema.HistoryLineError
			if errors.As(err, &le) {
				h.MalformedLines++
				continue
			}
			h.Truncated = true
			h.Err = err.Error()
			return h
		}
		// A record that decodes but breaks the union is NOT appended. Its kind
		// is undecidable, so any consumer has to guess, and the guess every
		// marker-skipping reader makes is "marker", which deletes an operation
		// silently. Counting it instead makes the history unreliable, which is
		// what turns a guess into an honest INCONCLUSIVE. See OQ-058.
		if verr := e.Validate(); verr != nil {
			h.InvalidRecords++
			if h.FirstInvalidReason == "" {
				h.FirstInvalidReason = verr.Error()
			}
			continue
		}
		h.Entries = append(h.Entries, *e)
	}
}

// ReadHistoryFile reads a history from disk. A missing or unreadable file
// yields a History carrying the error rather than an error return, so the
// caller can hand it to the engine and let the oracles report INCONCLUSIVE with
// the reason attached.
func ReadHistoryFile(path string) *History {
	f, err := os.Open(path)
	if err != nil {
		return &History{Path: path, Truncated: true, Err: err.Error()}
	}
	defer func() { _ = f.Close() }()
	return ReadHistory(f, path)
}

// ---------------------------------------------------------------------------
// shared helpers
// ---------------------------------------------------------------------------

// sortedStrings returns a sorted copy, so explanations and witnesses are
// deterministic no matter what order the collector filled Input in.
func sortedStrings(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}
