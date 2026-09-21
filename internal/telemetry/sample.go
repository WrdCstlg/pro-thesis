package telemetry

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// ADDITIVE schema identifiers. The directive names `telemetry_path` but no
// schema id for what it points at; without one there is no way to version-gate
// the format an oracle parses.
const (
	// DocumentSchema is the id of the complete document telemetry_path names.
	DocumentSchema = "prothesis.telemetry/v1"
	// SampleSchema is the id carried on each JSONL line, so a tailing consumer
	// can version-check without reading a header it may have missed.
	SampleSchema = "prothesis.telemetry_sample/v1"
)

// Artifact file names inside a run or world bundle.
const (
	// JSONLName is the append-only live stream.
	JSONLName = "telemetry.jsonl"
	// DocumentName is the materialised document telemetry_path points at.
	DocumentName = "telemetry.json"
)

// Metric path names used by Absence and by Document.Coverage.
//
// They are dotted paths into Sample, so a message like
// "resource_return_to_baseline is INCONCLUSIVE: memory.rss_bytes was never
// observed on kv-n2" names something a human can go and look at.
const (
	MetricProcess       = "process"
	MetricMemory        = "memory"
	MetricMemoryRSS     = "memory.rss_bytes"
	MetricCPU           = "cpu"
	MetricCPUMilliPct   = "cpu.milli_pct"
	MetricTasks         = "tasks"
	MetricTasksOpenFDs  = "tasks.open_fds"
	MetricProbe         = "probe"
	MetricStatus        = "status"
	MetricStatusQueue   = "status.queue_depth"
	MetricStatusRoutine = "status.goroutines"
)

// Sample is one node observed at one instant: one line of telemetry.jsonl.
//
// Every metric group is a pointer and every metric inside it is a pointer.
// nil means NOT OBSERVED. See the package comment: a zero substituted for an
// absent metric turns `resource_return_to_baseline` into a rubber stamp.
type Sample struct {
	Schema string `json:"schema"`
	// Seq is the per-node sample ordinal, from 0. It makes a gap visible: a
	// consumer that sees 41 then 43 knows a sample was dropped rather than
	// silently interpolating across it.
	Seq int64 `json:"seq"`
	// Node is the logical node id from prothesis.yaml.
	Node string `json:"node"`
	// Container is the container the metrics were read from, when known.
	Container string `json:"container,omitempty"`

	// TNS is UNIX EPOCH NANOSECONDS, the same frame as the history log's t_ns
	// (PHASE0_BUILD_BRIEF D-C). This is what lets an oracle line a telemetry
	// sample up against an operation without a second conversion constant.
	TNS int64 `json:"t_ns"`
	// RTNS is the recorder's monotonic reading. Unaffected by NTP steps, so it
	// is the correct basis for a rate.
	RTNS int64 `json:"rt_ns"`
	// VMS is milliseconds relative to the DRIVE origin: the frame
	// prothesis.oracle_input/v1's `phases` array uses. It is NULL before DRIVE
	// starts, because virtual time does not exist then (recorder.ErrNoOrigin);
	// emitting 0 would place every BOOT sample at the DRIVE origin.
	VMS *int64 `json:"v_ms"`
	// Phase is the lifecycle phase in force when the sample was taken, or ""
	// when the collector was given no phase source.
	Phase schema.Phase `json:"phase"`

	// CollectUS is how long gathering this sample took, in microseconds. A
	// sample that took longer than the interval is evidence the host, not the
	// target, is the thing under load.
	CollectUS int64 `json:"collect_us"`

	Process *ProcessMetrics `json:"process"`
	Memory  *MemoryMetrics  `json:"memory"`
	CPU     *CPUMetrics     `json:"cpu"`
	Tasks   *TaskMetrics    `json:"tasks"`
	Probe   *ProbeMetrics   `json:"probe"`
	Status  *StatusMetrics  `json:"status"`

	// Absent records why a metric group or metric is missing from THIS sample.
	// It is the difference between an oracle reporting "inconclusive" and an
	// oracle reporting "inconclusive: the container was paused for the whole
	// QUIESCE window".
	Absent []Absence `json:"absent"`
}

// Absence is one metric that could not be observed, and why.
type Absence struct {
	// Metric is a dotted path into Sample, e.g. "memory.rss_bytes".
	Metric string `json:"metric"`
	// Reason is a human-readable cause, e.g. "container is paused".
	Reason string `json:"reason"`
}

// MarkAbsent records a metric as unobserved. Duplicate metric paths are
// collapsed, keeping the first reason.
func (s *Sample) MarkAbsent(metric, format string, a ...any) {
	for _, x := range s.Absent {
		if x.Metric == metric {
			return
		}
	}
	s.Absent = append(s.Absent, Absence{Metric: metric, Reason: fmt.Sprintf(format, a...)})
}

// AbsentReason returns the recorded reason for a metric, if any.
func (s *Sample) AbsentReason(metric string) (string, bool) {
	for _, x := range s.Absent {
		if x.Metric == metric {
			return x.Reason, true
		}
	}
	return "", false
}

// RSS returns the honest resident-set figure and whether it was observed.
//
// Callers must use this rather than reading MemoryMetrics.RSSBytes through a nil
// check of their own, because it is the one place that refuses to fall back to
// CurrentBytes, DockerUsageBytes or the target's self-reported figure. Every one
// of those is a different quantity, and substituting any of them manufactures
// false `resource_return_to_baseline` violations.
func (s *Sample) RSS() (int64, bool) {
	if s == nil || s.Memory == nil || s.Memory.RSSBytes == nil {
		return 0, false
	}
	return *s.Memory.RSSBytes, true
}

// QueueDepth returns the target-reported queue depth and whether it was
// observed. Queue depth is not observable from an unmodified process, so this
// is nil for any target that does not report it, and `no_unbounded_queue` must
// then return INCONCLUSIVE.
func (s *Sample) QueueDepth() (int64, bool) {
	if s == nil || s.Status == nil || s.Status.QueueDepth == nil {
		return 0, false
	}
	return *s.Status.QueueDepth, true
}

// Goroutines returns the target-reported goroutine count and whether it was
// observed. Same caveat as QueueDepth.
func (s *Sample) Goroutines() (int64, bool) {
	if s == nil || s.Status == nil || s.Status.Goroutines == nil {
		return 0, false
	}
	return *s.Status.Goroutines, true
}

// ---------------------------------------------------------------------------
// process
// ---------------------------------------------------------------------------

// ProcessMetrics is the container's process state, from `docker inspect`.
//
// This is the evidence `no_crash` runs on: a container that exited outside a
// planned fault window, or that the kernel OOM-killed, shows up here.
type ProcessMetrics struct {
	// Status is docker's own word: created, running, paused, restarting,
	// removing, exited or dead. Empty when inspect did not answer.
	Status string `json:"status"`

	Running    *bool `json:"running"`
	Paused     *bool `json:"paused"`
	Restarting *bool `json:"restarting"`
	// OOMKilled is the kernel's verdict, not an inference from an exit code.
	OOMKilled *bool `json:"oom_killed"`
	Dead      *bool `json:"dead"`

	// PID is the container's main process id in the engine's namespace. Zero is
	// a real value for a stopped container, hence the pointer.
	PID *int64 `json:"pid"`
	// ExitCode is meaningful only once Running is false. Exit code 0 from a
	// container that was never supposed to exit is still a crash, which is why
	// this is a pointer and not folded into Status.
	ExitCode *int64 `json:"exit_code"`
	// Error is docker's error string for the container, if any.
	Error string `json:"error,omitempty"`

	// StartedAtNS and FinishedAtNS are Unix epoch nanoseconds, converted from
	// docker's RFC3339 strings. Nil when docker reported the zero time.
	StartedAtNS  *int64 `json:"started_at_ns"`
	FinishedAtNS *int64 `json:"finished_at_ns"`
	// RestartCount is how many times the engine restarted the container. The kv
	// fixture sets `restart: "no"` precisely so this stays 0 and cannot mask a
	// crash.
	RestartCount *int64 `json:"restart_count"`
}

// IsUp reports whether the container was observed running and not paused.
func (p *ProcessMetrics) IsUp() bool {
	return p != nil && p.Running != nil && *p.Running && (p.Paused == nil || !*p.Paused)
}

// ---------------------------------------------------------------------------
// memory
// ---------------------------------------------------------------------------

// MemorySource names where RSSBytes came from. It is recorded on every sample
// so a reader never has to guess which quantity they are looking at.
type MemorySource string

const (
	// SourceCgroupV2Anon is /sys/fs/cgroup/memory.stat `anon`: true anonymous
	// memory, page cache excluded. The only honest RSS on cgroup v2.
	SourceCgroupV2Anon MemorySource = "cgroup_v2_anon"
	// SourceCgroupV1RSS is /sys/fs/cgroup/memory/memory.stat `rss`: the cgroup
	// v1 equivalent, likewise page-cache-free.
	SourceCgroupV1RSS MemorySource = "cgroup_v1_rss"
	// SourceNone means no true anon figure was obtainable. RSSBytes is then nil.
	SourceNone MemorySource = ""
)

// MemoryMetrics is one node's memory at one instant.
//
// Read the field comments before using any of them interchangeably. Four
// different quantities are carried here on purpose, because conflating them is
// exactly how a resource oracle starts lying.
type MemoryMetrics struct {
	// Source says which cgroup field RSSBytes came from, or "" when RSSBytes is
	// nil.
	Source MemorySource `json:"source"`

	// RSSBytes is TRUE anonymous memory: cgroup v2 `anon` or cgroup v1 `rss`.
	// Nil when no such figure was obtainable, never substituted from any other
	// field on this struct. This is the only field a resource oracle may treat
	// as RSS.
	RSSBytes *int64 `json:"rss_bytes"`

	// PageCacheBytes is cgroup v2 `file`: page cache charged to this cgroup. It
	// is here as a DIAGNOSTIC, so a reader can see that a rising CurrentBytes is
	// cache rather than a leak.
	PageCacheBytes *int64 `json:"page_cache_bytes"`

	// CurrentBytes is `memory.current`: total charged memory, page cache
	// INCLUDED. Do not compare it against a baseline: it does not return to one
	// under a write workload, and doing so is the systematic false-positive this
	// format exists to prevent.
	CurrentBytes *int64 `json:"current_bytes"`

	// DockerUsageBytes is docker stats' MemUsage. Measured on this machine to be
	// `memory.current - inactive_file`: it removes inactive page cache only, so
	// the active file cache a write workload just created is still inside it.
	// Present only when the docker-stats fallback ran.
	DockerUsageBytes *int64 `json:"docker_usage_bytes"`

	// PID1RSSBytes is /proc/1/status VmRSS: the resident set of PID 1 ALONE. For
	// a single-process container it corroborates RSSBytes; for a container
	// running several processes it is strictly less than the cgroup total, so it
	// is never used as RSSBytes.
	PID1RSSBytes *int64 `json:"pid1_rss_bytes"`

	SwapBytes   *int64 `json:"swap_bytes"`
	ShmemBytes  *int64 `json:"shmem_bytes"`
	KernelBytes *int64 `json:"kernel_bytes"`
}

// ---------------------------------------------------------------------------
// cpu
// ---------------------------------------------------------------------------

// CPUMetrics is one node's CPU at one instant.
//
// The cumulative counters are the raw observation; the derived fields need two
// samples and are nil on the first, because a rate computed from one point is a
// fabrication.
type CPUMetrics struct {
	// UsageUS is cgroup `cpu.stat` usage_usec: cumulative CPU time since the
	// cgroup was created.
	UsageUS  *int64 `json:"usage_us"`
	UserUS   *int64 `json:"user_us"`
	SystemUS *int64 `json:"system_us"`
	// ThrottledUS and NrThrottled are cpu.stat's throttling counters. A node
	// that is being CPU-throttled looks unresponsive for reasons that have
	// nothing to do with the fault under test, so this is worth recording.
	ThrottledUS *int64 `json:"throttled_us"`
	NrThrottled *int64 `json:"nr_throttled"`

	// DeltaUS is UsageUS minus the previous sample's, and IntervalNS is the
	// monotonic gap between the two. Both nil on a node's first sample.
	DeltaUS    *int64 `json:"delta_us"`
	IntervalNS *int64 `json:"interval_ns"`
	// MilliPct is CPU percent times 1000, so 100000 means one core fully
	// saturated and 800000 means eight. Integer rather than float: see the
	// package comment on encoding.
	MilliPct *int64 `json:"milli_pct"`

	// DockerMilliPct is docker stats' CPUPerc, likewise times 1000. Present only
	// when the docker-stats fallback ran. It is a different estimator from
	// MilliPct (docker samples the counter itself), so it is carried separately.
	DockerMilliPct *int64 `json:"docker_milli_pct"`
}

// ---------------------------------------------------------------------------
// tasks
// ---------------------------------------------------------------------------

// TaskMetrics counts processes, threads and descriptors.
//
// The directive's `resource_return_to_baseline` names "RSS, FDs, goroutines".
// FDs are here; goroutines are NOT, because a goroutine count cannot be observed
// from outside an arbitrary process: it lives under StatusMetrics, present only
// for a target that reports it.
type TaskMetrics struct {
	// PIDsCurrent is cgroup `pids.current`: processes plus threads in the
	// cgroup. This is the whole-container figure.
	PIDsCurrent *int64 `json:"pids_current"`
	// Threads is /proc/1/status Threads: PID 1's thread count only.
	Threads *int64 `json:"threads"`
	// OpenFDs counts /proc/1/fd: PID 1's open descriptors only. For a
	// single-process container that is the container's fd count; for a
	// multi-process one it is a lower bound, and the field name says whose it
	// is rather than pretending otherwise.
	OpenFDs *int64 `json:"open_fds"`
}

// ---------------------------------------------------------------------------
// probe
// ---------------------------------------------------------------------------

// ProbeMetrics is the result of one health probe, with its latency.
//
// The probe URL is expanded by harness.ExpandProbe: the same function
// WaitHealthy uses, so a node is probed at BOOT and sampled during DRIVE against
// exactly the same address.
type ProbeMetrics struct {
	URL string `json:"url"`
	// OK is true only for a 2xx response. A probe that was not attempted is a
	// nil *ProbeMetrics, not an OK:false one.
	OK bool `json:"ok"`
	// StatusCode is nil when the request never produced a response.
	StatusCode *int64 `json:"status_code"`
	// LatencyUS is round-trip time in microseconds, recorded for a failed probe
	// too: how long a node took to refuse is itself a signal.
	LatencyUS *int64 `json:"latency_us"`
	Error     string `json:"error,omitempty"`
}

// ---------------------------------------------------------------------------
// status
// ---------------------------------------------------------------------------

// StatusMetrics is the target's own /status document, both verbatim and
// extracted.
//
// Raw is preserved for the same reason the history merge moves raw lines: the
// target knows things about itself this struct does not model, and a decode
// followed by a re-encode would silently destroy them. The extracted fields are
// a convenience over a WELL-KNOWN vocabulary; a target that spells a field
// differently loses only the convenience, never the data.
type StatusMetrics struct {
	URL        string `json:"url"`
	OK         bool   `json:"ok"`
	StatusCode *int64 `json:"status_code"`
	LatencyUS  *int64 `json:"latency_us"`
	Error      string `json:"error,omitempty"`

	// Raw is the response body, verbatim, when it parsed as a JSON object.
	Raw json.RawMessage `json:"raw,omitempty"`

	// Extracted well-known fields. Each is nil unless the document carried that
	// key with a value of the right type.
	Role        *string `json:"role"`
	Term        *int64  `json:"term"`
	LeaderID    *string `json:"leader_id"`
	CommitIndex *int64  `json:"commit_index"`

	// QueueDepth and Goroutines are the two metrics the directive's built-in
	// oracles name that are NOT observable from outside a process. They are here
	// and only here. An oracle that needs one and finds nil returns INCONCLUSIVE.
	QueueDepth *int64 `json:"queue_depth"`
	Goroutines *int64 `json:"goroutines"`

	ClientConnections *int64 `json:"client_connections"`
	PeerConnections   *int64 `json:"peer_connections"`
	UptimeMS          *int64 `json:"uptime_ms"`

	// ReportedRSSBytes is the target's OWN rss_bytes claim.
	//
	// It is deliberately not promoted into MemoryMetrics.RSSBytes. A target's
	// self-report is only as honest as the target: the kv fixture computes it
	// from runtime.MemStats.Sys, which is address space obtained from the OS and
	// does not fall when memory is freed. Trusting it would make
	// `resource_return_to_baseline` fail on every Go target that ever allocated.
	// See OPEN_QUESTIONS.md OQ-020.
	ReportedRSSBytes *int64 `json:"reported_rss_bytes"`

	Lease *LeaseStatus `json:"lease"`
}

// LeaseStatus is the well-known `lease` sub-object: a read lease the node
// believes it holds. For the kv fixture this is the smoking gun: a node still
// claiming a lease after it has been displaced is the injected defect.
type LeaseStatus struct {
	Held        *bool  `json:"held"`
	RemainingMS *int64 `json:"remaining_ms"`
	DurationMS  *int64 `json:"duration_ms"`
}

// ---------------------------------------------------------------------------
// encoding
// ---------------------------------------------------------------------------

// MarshalLine encodes a sample as one JSONL line, without the newline.
//
// HTML escaping is off, matching every other emitter in the tree: Go escapes
// '<' and '>' by default, which would rewrite a node id or an error string
// containing them.
func (s *Sample) MarshalLine() ([]byte, error) {
	if s.Schema == "" {
		s.Schema = SampleSchema
	}
	return marshalNoEscape(s)
}

// ParseSample decodes one JSONL line.
//
// Unknown fields are ALLOWED. The line may have been written by a newer build
// than the one reading it: a tailing consumer must degrade to the fields it
// understands rather than refusing the stream. A wrong schema id is still an
// error, because that is a format change rather than an addition.
func ParseSample(line []byte) (*Sample, error) {
	var s Sample
	if err := json.Unmarshal(line, &s); err != nil {
		return nil, fmt.Errorf("telemetry: decode sample: %w", err)
	}
	if s.Schema != SampleSchema {
		return nil, fmt.Errorf("telemetry: sample schema is %q, want %q", s.Schema, SampleSchema)
	}
	return &s, nil
}

func marshalNoEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// i64 returns a pointer to v. Local rather than schema.Int64 so the telemetry
// types have no reason to import the config package.
func i64(v int64) *int64 { return &v }

func boolp(v bool) *bool { return &v }

func strp(v string) *string { return &v }
