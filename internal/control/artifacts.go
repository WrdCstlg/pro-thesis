package control

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/WrdCstlg/pro-thesis/internal/recorder"
	"github.com/WrdCstlg/pro-thesis/internal/telemetry"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// materializeOracleArtifacts writes every file prothesis.oracle_input/v1 names,
// so the document is never a promise the run cannot keep.
//
// This runs at ASSERT, before any oracle is handed the input document.
func (r *Runner) materializeOracleArtifacts(paths WorldPaths, top *recorder.Topology, timings schema.PhaseTimings, tl *recorder.Timeline) error {
	var firstErr error
	note := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}

	// telemetry.json: the complete document.
	//
	// telemetry.jsonl stays: it is append-only and tailable, which is what a
	// live consumer (Phase 4's Saboteur) needs. But `telemetry_path` is read by
	// third-party oracles that must not have to implement a JSONL reader, and
	// the directive's own example names a .json document. Writing both satisfies
	// each reading rather than forcing a choice.
	meta := telemetry.DocumentMeta{RunID: r.runID, Phases: timings}
	if tl != nil {
		if origin, err := tl.DriveOriginWallNS(); err == nil {
			meta.DriveOriginWallNS = &origin
		}
	}
	if _, err := telemetry.MaterializeDocument(paths.Telemetry, TelemetryDocPath(paths), meta); err != nil {
		note(fmt.Errorf("materialise telemetry document: %w", err))
	}

	// final_state.json: named by the frozen contract, previously never written.
	if err := writeFinalState(paths.FinalState, top, timings); err != nil {
		note(fmt.Errorf("write final state: %w", err))
	}
	return firstErr
}

// TelemetryDocPath is the complete telemetry document beside the JSONL stream.
func TelemetryDocPath(p WorldPaths) string {
	return filepath.Join(p.Dir, telemetry.DocumentName)
}

// FinalState is the artifact prothesis.oracle_input/v1's final_state_path names.
//
// The directive fixes the FIELD NAME `final_state_path` but never specifies the
// document's shape, so this is a Phase 0/1 choice recorded in DECISIONS.md
// rather than a transcription. It is deliberately thin: the end-of-run
// observable state of each node, enough for a convergence oracle to ask "did
// the cluster agree in the end", and nothing that duplicates the history log or
// the telemetry document.
type FinalState struct {
	Schema string `json:"schema"`
	RunID  string `json:"run_id"`
	// Nodes is the per-node terminal state, sorted by node id so the document
	// is stable across runs.
	Nodes []FinalNodeState `json:"nodes"`
	// Phases is the measured lifecycle, repeated here so the artifact is
	// self-describing when read on its own.
	Phases schema.PhaseTimings `json:"phases"`
}

// FinalNodeState is one node's terminal observable state.
type FinalNodeState struct {
	ID      string `json:"id"`
	Service string `json:"service"`
	// Reachable records whether the node answered at teardown time. An
	// unreachable node is a fact, not an error: a world that killed a node ends
	// with it unreachable and a convergence oracle needs to know.
	Reachable bool `json:"reachable"`
	// Status is the node's own /status payload verbatim when it exposes one,
	// null otherwise. It is carried as raw JSON rather than a typed struct
	// because it is the TARGET's vocabulary, not ours: typing it here would
	// make the artifact fixture-specific and break the portability the oracle
	// library depends on.
	Status json.RawMessage `json:"status"`
}

const finalStateSchema = "prothesis.final_state/v1"

func writeFinalState(path string, top *recorder.Topology, timings schema.PhaseTimings) error {
	fs := FinalState{
		Schema: finalStateSchema,
		Phases: timings,
	}
	if top != nil {
		fs.RunID = top.RunID
		for _, n := range top.Nodes {
			fs.Nodes = append(fs.Nodes, FinalNodeState{
				ID:      n.ID,
				Service: n.Service,
				// Phase 1 records reachability from the last health observation
				// the runner made; a richer probe belongs with the convergence
				// oracles in Phase 3.
				Reachable: n.HostPort != 0,
				Status:    nil,
			})
		}
	}
	sort.Slice(fs.Nodes, func(i, j int) bool { return fs.Nodes[i].ID < fs.Nodes[j].ID })
	if fs.Nodes == nil {
		fs.Nodes = []FinalNodeState{}
	}
	if fs.Phases == nil {
		fs.Phases = []schema.PhaseWindow{}
	}

	b, err := json.MarshalIndent(fs, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

// MergePhaseMarkers merges the harness's phase markers into the canonical
// history, ordered by t_ns.
//
// Directive 4.4's sample shows a phase marker inline in the history stream:
//
//	{"t_ns":1725300002000000,"type":"info","event":"phase","phase":"HEAL"}
//
// so the merged file is what the normative format describes and what
// oracle_input.history_path must name.
//
// The merge is BY RAW LINE. Decoding into schema.HistoryEntry and re-encoding
// would silently drop any field the struct does not model: the fixture
// attaches a namespaced `meta` object to every operation (node, term,
// commit_index, read_mode, served_by, latency_us) that carries the stale-read
// attribution Phase 3's consistency oracle needs. schema.HistoryEntry is used
// only to READ t_ns for ordering.
//
// It is idempotent: a history that already contains its phase markers is left
// unchanged, so re-running ASSERT cannot duplicate them.
func MergePhaseMarkers(historyPath, phasesPath string) error {
	markers, err := readLines(phasesPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // no markers to merge
		}
		return err
	}
	if len(markers) == 0 {
		return nil
	}
	histLines, err := readLines(historyPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // no history (e.g. the driver never started)
		}
		return err
	}

	// Idempotence: if any phase marker is already present, assume the merge ran.
	for _, l := range histLines {
		if e, err := decodeT(l); err == nil && e.Event == "phase" {
			return nil
		}
	}

	type row struct {
		tns   int64
		line  []byte
		order int // stable tiebreak: history before markers at equal t_ns
	}
	rows := make([]row, 0, len(histLines)+len(markers))
	for i, l := range histLines {
		e, err := decodeT(l)
		if err != nil {
			// A line we cannot parse still belongs in the output: dropping it
			// would be data loss. Anchor it to the previous timestamp.
			var prev int64
			if len(rows) > 0 {
				prev = rows[len(rows)-1].tns
			}
			rows = append(rows, row{tns: prev, line: l, order: i})
			continue
		}
		rows = append(rows, row{tns: e.TNS, line: l, order: i})
	}
	for i, l := range markers {
		e, err := decodeT(l)
		if err != nil {
			continue
		}
		rows = append(rows, row{tns: e.TNS, line: l, order: len(histLines) + i})
	}

	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].tns != rows[j].tns {
			return rows[i].tns < rows[j].tns
		}
		return rows[i].order < rows[j].order
	})

	tmp := historyPath + ".merge"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	for _, r := range rows {
		if _, err := w.Write(r.line); err != nil {
			_ = f.Close()
			return err
		}
		if _, err := w.Write([]byte("\n")); err != nil {
			_ = f.Close()
			return err
		}
	}
	if err := w.Flush(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, historyPath)
}

// tOnly reads just the ordering fields, leaving every other member untouched.
type tOnly struct {
	TNS   int64  `json:"t_ns"`
	Event string `json:"event"`
}

func decodeT(line []byte) (tOnly, error) {
	var e tOnly
	err := json.Unmarshal(line, &e)
	return e, err
}

func readLines(path string) ([][]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var out [][]byte
	sc := bufio.NewScanner(f)
	// History lines carry a full operation plus its meta object; the default
	// 64KB token limit is not generous enough for a wide record.
	sc.Buffer(make([]byte, 0, 256*1024), 8*1024*1024)
	for sc.Scan() {
		b := sc.Bytes()
		if len(b) == 0 {
			continue
		}
		cp := make([]byte, len(b))
		copy(cp, b)
		out = append(out, cp)
	}
	return out, sc.Err()
}
