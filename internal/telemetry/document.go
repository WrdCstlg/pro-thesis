package telemetry

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// Document is the complete telemetry artifact: what
// `prothesis.oracle_input/v1`'s `telemetry_path` points at.
//
// It exists ALONGSIDE the JSONL rather than instead of it. The JSONL is for a
// live consumer; this is for an external oracle, which is a one-shot process
// that reads a whole file and exits, and which needs the Coverage census that no
// single sample can express.
type Document struct {
	Schema string `json:"schema"`
	RunID  string `json:"run_id"`
	// World is the world ordinal within the run, or null for run-level
	// telemetry collected outside any world (`thesis up`, for example).
	World *int64 `json:"world"`

	// IntervalMS is the sampling period that was configured. An oracle
	// reasoning about "3 consecutive samples" needs to know what a sample is
	// worth in wall time.
	IntervalMS int64 `json:"interval_ms"`

	// DriveOriginWallNS is the run's wall anchor: Unix epoch nanoseconds at
	// DRIVE start. It is the conversion constant between a sample's t_ns and
	// the start_ms/end_ms frame of the oracle contract's `phases` array. Null
	// when telemetry was collected without a DRIVE phase.
	DriveOriginWallNS *int64 `json:"drive_origin_wall_ns"`

	// Phases is the measured lifecycle window list, copied from the run so an
	// oracle reading only this file can phase-scope its own analysis.
	Phases schema.PhaseTimings `json:"phases"`

	FirstTNS    int64 `json:"first_t_ns"`
	LastTNS     int64 `json:"last_t_ns"`
	SampleCount int64 `json:"sample_count"`

	Nodes []NodeSeries `json:"nodes"`

	// Coverage is the per-metric census: what was observed, what never was, and
	// why. This is what lets an oracle return INCONCLUSIVE with a diagnosis
	// instead of a shrug.
	Coverage Coverage `json:"coverage"`

	// Errors are non-fatal collection problems recorded during the run.
	Errors []string `json:"errors"`
}

// NodeSeries is one node's ordered samples.
type NodeSeries struct {
	Node      string `json:"node"`
	Container string `json:"container,omitempty"`
	// Samples are in observation order, which is Seq order.
	Samples []Sample `json:"samples"`
	// Gaps counts missing sequence numbers: evidence that a round was dropped
	// rather than that the metric was flat.
	Gaps int64 `json:"gaps"`
}

// Coverage is the per-metric observation census.
type Coverage struct {
	// Present lists metric paths observed at least once on at least one node.
	Present []string `json:"present"`
	// Absent lists metric paths NEVER observed on at least one node, with the
	// nodes and the reason last recorded.
	Absent []AbsentMetric `json:"absent"`
}

// AbsentMetric is one metric that was never observed on some node.
type AbsentMetric struct {
	Metric string   `json:"metric"`
	Nodes  []string `json:"nodes"`
	Reason string   `json:"reason"`
}

// Observed reports whether metric was observed at least once anywhere.
func (c Coverage) Observed(metric string) bool {
	for _, m := range c.Present {
		if m == metric {
			return true
		}
	}
	return false
}

// Missing returns the absence record for metric, if any.
func (c Coverage) Missing(metric string) (AbsentMetric, bool) {
	for _, a := range c.Absent {
		if a.Metric == metric {
			return a, true
		}
	}
	return AbsentMetric{}, false
}

// DocumentMeta is everything about a document that the sample stream does not
// itself carry.
type DocumentMeta struct {
	RunID             string
	World             *int64
	IntervalMS        int64
	DriveOriginWallNS *int64
	Phases            schema.PhaseTimings
	Errors            []string
}

// BuildDocument assembles a Document from a JSONL stream.
//
// The document is always materialised from the JSONL, never from a second
// in-memory copy of the samples. One source of truth means the tailable stream
// and the oracle's input cannot disagree about what was observed, and a
// disagreement between them would be undetectable and would falsify every
// oracle that read the wrong one.
func BuildDocument(r io.Reader, meta DocumentMeta) (*Document, error) {
	doc := &Document{
		Schema:            DocumentSchema,
		RunID:             meta.RunID,
		World:             meta.World,
		IntervalMS:        meta.IntervalMS,
		DriveOriginWallNS: meta.DriveOriginWallNS,
		Phases:            meta.Phases,
		Errors:            append([]string{}, meta.Errors...),
		Nodes:             []NodeSeries{},
	}
	if doc.IntervalMS == 0 {
		doc.IntervalMS = DefaultIntervalMS
	}
	if doc.Phases == nil {
		doc.Phases = schema.PhaseTimings{}
	}

	byNode := map[string]*NodeSeries{}
	var order []string
	present := map[string]bool{}
	absentNodes := map[string]map[string]bool{}
	absentReason := map[string]string{}

	br := bufio.NewReader(r)
	lineNo := 0
	for {
		line, err := readLine(br)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("telemetry: read sample stream: %w", err)
		}
		lineNo++
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		s, err := ParseSample(line)
		if err != nil {
			// A malformed telemetry line is recorded, not swallowed and not
			// fatal. Losing one sample must not cost the other 99% of the
			// series, and silently dropping it would let a truncated stream
			// look like a complete one.
			doc.Errors = append(doc.Errors, fmt.Sprintf("line %d: %v", lineNo, err))
			continue
		}

		ns, ok := byNode[s.Node]
		if !ok {
			ns = &NodeSeries{Node: s.Node, Container: s.Container, Samples: []Sample{}}
			byNode[s.Node] = ns
			order = append(order, s.Node)
		}
		if ns.Container == "" {
			ns.Container = s.Container
		}
		ns.Samples = append(ns.Samples, *s)

		for _, m := range observedMetrics(s) {
			present[m] = true
		}
		for _, a := range s.Absent {
			if absentNodes[a.Metric] == nil {
				absentNodes[a.Metric] = map[string]bool{}
			}
			absentNodes[a.Metric][s.Node] = true
			absentReason[a.Metric] = a.Reason
		}

		doc.SampleCount++
		if doc.SampleCount == 1 || s.TNS < doc.FirstTNS {
			doc.FirstTNS = s.TNS
		}
		if s.TNS > doc.LastTNS {
			doc.LastTNS = s.TNS
		}
	}

	sort.Strings(order)
	for _, node := range order {
		ns := byNode[node]
		sort.SliceStable(ns.Samples, func(i, j int) bool { return ns.Samples[i].Seq < ns.Samples[j].Seq })
		ns.Gaps = countGaps(ns.Samples)
		doc.Nodes = append(doc.Nodes, *ns)
	}

	doc.Coverage = buildCoverage(present, absentNodes, absentReason)
	return doc, nil
}

// countGaps returns how many sequence numbers are missing from a node's series.
func countGaps(samples []Sample) int64 {
	if len(samples) < 2 {
		return 0
	}
	var gaps int64
	for i := 1; i < len(samples); i++ {
		d := samples[i].Seq - samples[i-1].Seq
		if d > 1 {
			gaps += d - 1
		}
	}
	return gaps
}

// buildCoverage assembles the census.
//
// A metric appears under Absent when it was absent on ANY node, even if another
// node reported it. That asymmetry is deliberate and fail-closed: an oracle
// comparing three nodes cannot soundly conclude anything from two of them, so
// "observed somewhere" must not read as "observed".
func buildCoverage(present map[string]bool, absentNodes map[string]map[string]bool, reason map[string]string) Coverage {
	cov := Coverage{Present: []string{}, Absent: []AbsentMetric{}}
	for m := range present {
		cov.Present = append(cov.Present, m)
	}
	sort.Strings(cov.Present)

	metrics := make([]string, 0, len(absentNodes))
	for m := range absentNodes {
		metrics = append(metrics, m)
	}
	sort.Strings(metrics)
	for _, m := range metrics {
		nodes := make([]string, 0, len(absentNodes[m]))
		for n := range absentNodes[m] {
			nodes = append(nodes, n)
		}
		sort.Strings(nodes)
		cov.Absent = append(cov.Absent, AbsentMetric{Metric: m, Nodes: nodes, Reason: reason[m]})
	}
	return cov
}

// observedMetrics lists the metric paths a sample actually carries.
//
// A metric group that exists but whose key field is nil does NOT count as
// observed: a MemoryMetrics with only DockerUsageBytes set has not observed
// memory.rss_bytes, and saying otherwise would let a resource oracle believe it
// has a baseline when it has only a page-cache-inclusive number.
func observedMetrics(s *Sample) []string {
	var out []string
	if s.Process != nil {
		out = append(out, MetricProcess)
	}
	if s.Memory != nil {
		out = append(out, MetricMemory)
		if s.Memory.RSSBytes != nil {
			out = append(out, MetricMemoryRSS)
		}
	}
	if s.CPU != nil {
		out = append(out, MetricCPU)
		if s.CPU.MilliPct != nil {
			out = append(out, MetricCPUMilliPct)
		}
	}
	if s.Tasks != nil {
		out = append(out, MetricTasks)
		if s.Tasks.OpenFDs != nil {
			out = append(out, MetricTasksOpenFDs)
		}
	}
	if s.Probe != nil {
		out = append(out, MetricProbe)
	}
	if s.Status != nil {
		out = append(out, MetricStatus)
		if s.Status.QueueDepth != nil {
			out = append(out, MetricStatusQueue)
		}
		if s.Status.Goroutines != nil {
			out = append(out, MetricStatusRoutine)
		}
	}
	return out
}

// readLine returns one line without its terminator, or io.EOF.
//
// An unbounded reader rather than bufio.Scanner: a sample carrying a large
// target /status document would exceed Scanner's 64 KiB token limit and
// truncate the stream silently. The same reasoning as schema.HistoryReader.
func readLine(r *bufio.Reader) ([]byte, error) {
	b, err := r.ReadBytes('\n')
	if len(b) == 0 {
		if err == nil {
			err = io.EOF
		}
		return nil, err
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	b = bytes.TrimSuffix(b, []byte{'\n'})
	b = bytes.TrimSuffix(b, []byte{'\r'})
	return b, nil
}

// Marshal encodes the document, indented, with a trailing newline.
//
// Indented because an external oracle may be a shell script reading it with
// `jq`, and because a human diffing two telemetry documents is a normal thing to
// do. HTML escaping is off, as everywhere in this tree.
func (d *Document) Marshal() ([]byte, error) {
	d.Schema = DocumentSchema
	if d.Nodes == nil {
		d.Nodes = []NodeSeries{}
	}
	if d.Errors == nil {
		d.Errors = []string{}
	}
	if d.Phases == nil {
		d.Phases = schema.PhaseTimings{}
	}
	b, err := marshalNoEscape(d)
	if err != nil {
		return nil, fmt.Errorf("telemetry: encode document: %w", err)
	}
	var out bytes.Buffer
	if err := json.Indent(&out, b, "", "  "); err != nil {
		return nil, fmt.Errorf("telemetry: indent document: %w", err)
	}
	out.WriteByte('\n')
	return out.Bytes(), nil
}

// ParseDocument decodes a telemetry document.
func ParseDocument(data []byte) (*Document, error) {
	var d Document
	if err := json.Unmarshal(data, &d); err != nil {
		return nil, fmt.Errorf("telemetry: decode document: %w", err)
	}
	if d.Schema != DocumentSchema {
		return nil, fmt.Errorf("telemetry: document schema is %q, want %q", d.Schema, DocumentSchema)
	}
	return &d, nil
}

// MaterializeDocument reads jsonlPath and writes the document to outPath.
//
// This is the ASSERT-time step: `telemetry_path` in the oracle input names
// outPath. The write is atomic, because an oracle that reads a half-written
// document would report INCONCLUSIVE for a reason that has nothing to do with
// the system under test.
func MaterializeDocument(jsonlPath, outPath string, meta DocumentMeta) (*Document, error) {
	f, err := os.Open(jsonlPath)
	if err != nil {
		return nil, fmt.Errorf("telemetry: open %s: %w", jsonlPath, err)
	}
	defer f.Close()

	doc, err := BuildDocument(f, meta)
	if err != nil {
		return nil, err
	}
	data, err := doc.Marshal()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		return nil, fmt.Errorf("telemetry: create %s: %w", filepath.Dir(outPath), err)
	}
	tmp := outPath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return nil, fmt.Errorf("telemetry: write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, outPath); err != nil {
		_ = os.Remove(tmp)
		return nil, fmt.Errorf("telemetry: rename %s -> %s: %w", tmp, outPath, err)
	}
	return doc, nil
}
