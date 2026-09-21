package control

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The merge must be BY RAW LINE. Decoding into schema.HistoryEntry and
// re-encoding would drop every field the struct does not model: the fixture
// attaches a namespaced `meta` object to each operation carrying the node,
// term, commit_index, read_mode and served_by that Phase 3's consistency oracle
// needs to attribute a stale read. Losing it would not fail loudly; the
// consistency check would simply become unable to explain itself.
func TestMergePreservesUnmodelledFields(t *testing.T) {
	dir := t.TempDir()
	hist := filepath.Join(dir, "history.jsonl")
	phases := filepath.Join(dir, "phases.jsonl")

	write(t, hist, `{"t_ns":300,"process":3,"type":"invoke","f":"read","key":"k/42","op_id":7,"meta":{"served_by":"kv-n2","read_mode":"lease","term":4,"commit_index":91}}
{"t_ns":500,"process":3,"type":"ok","f":"read","key":"k/42","value":7,"op_id":7,"meta":{"served_by":"kv-n2","latency_us":1204}}`)
	write(t, phases, `{"t_ns":100,"type":"info","event":"phase","phase":"DRIVE"}
{"t_ns":400,"type":"info","event":"phase","phase":"HEAL"}`)

	if err := MergePhaseMarkers(hist, phases); err != nil {
		t.Fatalf("merge: %v", err)
	}
	lines := readAll(t, hist)

	if len(lines) != 4 {
		t.Fatalf("got %d lines, want 4 (2 ops + 2 markers): %q", len(lines), lines)
	}

	// Every original op must still carry its full meta object, byte for byte.
	var metas int
	for _, l := range lines {
		if !strings.Contains(l, `"op_id"`) {
			continue
		}
		metas++
		var m struct {
			Meta map[string]any `json:"meta"`
		}
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("op line is not valid JSON after merge: %v\n%s", err, l)
		}
		if len(m.Meta) == 0 {
			t.Fatalf("an op lost its meta object in the merge; Phase 3 loses stale-read "+
				"attribution and the loss is silent:\n%s", l)
		}
	}
	if metas != 2 {
		t.Fatalf("expected 2 op records after merge, got %d", metas)
	}
	if !strings.Contains(lines[1], `"served_by":"kv-n2"`) || !strings.Contains(lines[1], `"term":4`) {
		t.Fatalf("meta contents were rewritten rather than passed through:\n%s", lines[1])
	}

	// Ordered by t_ns: DRIVE(100), invoke(300), HEAL(400), ok(500).
	wantOrder := []string{"DRIVE", `"op_id":7,"meta"`, "HEAL", `"type":"ok"`}
	for i, want := range wantOrder {
		if !strings.Contains(lines[i], want) {
			t.Fatalf("line %d is not in t_ns order; want it to contain %q, got:\n%s", i, want, lines[i])
		}
	}
}

// Re-running ASSERT must not duplicate markers.
func TestMergeIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	hist := filepath.Join(dir, "history.jsonl")
	phases := filepath.Join(dir, "phases.jsonl")
	write(t, hist, `{"t_ns":300,"process":1,"type":"invoke","f":"read","op_id":1}`)
	write(t, phases, `{"t_ns":100,"type":"info","event":"phase","phase":"DRIVE"}`)

	if err := MergePhaseMarkers(hist, phases); err != nil {
		t.Fatal(err)
	}
	first := readAll(t, hist)
	if err := MergePhaseMarkers(hist, phases); err != nil {
		t.Fatal(err)
	}
	second := readAll(t, hist)

	if len(first) != len(second) {
		t.Fatalf("merge is not idempotent: %d lines then %d", len(first), len(second))
	}
	if strings.Join(first, "\n") != strings.Join(second, "\n") {
		t.Fatal("a second merge changed the history")
	}
}

// A missing phases log is not an error: the driver may never have started.
func TestMergeToleratesMissingInputs(t *testing.T) {
	dir := t.TempDir()
	hist := filepath.Join(dir, "history.jsonl")
	write(t, hist, `{"t_ns":1,"process":1,"type":"invoke","f":"read","op_id":1}`)

	if err := MergePhaseMarkers(hist, filepath.Join(dir, "absent.jsonl")); err != nil {
		t.Fatalf("missing phases log should be tolerated: %v", err)
	}
	if err := MergePhaseMarkers(filepath.Join(dir, "absent.jsonl"), hist); err != nil {
		t.Fatalf("missing history should be tolerated: %v", err)
	}
}

// A line the ordering decoder cannot parse must survive the merge. Dropping it
// would be silent data loss in the artifact a violation is reconstructed from.
func TestMergeKeepsUnparseableLines(t *testing.T) {
	dir := t.TempDir()
	hist := filepath.Join(dir, "history.jsonl")
	phases := filepath.Join(dir, "phases.jsonl")
	write(t, hist, "{\"t_ns\":300,\"type\":\"invoke\",\"op_id\":1}\nthis is not json\n{\"t_ns\":500,\"type\":\"ok\",\"op_id\":1}")
	write(t, phases, `{"t_ns":100,"type":"info","event":"phase","phase":"DRIVE"}`)

	if err := MergePhaseMarkers(hist, phases); err != nil {
		t.Fatal(err)
	}
	lines := readAll(t, hist)
	var found bool
	for _, l := range lines {
		if l == "this is not json" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the unparseable line was dropped; the merge must not lose data:\n%q", lines)
	}
	if len(lines) != 4 {
		t.Fatalf("got %d lines, want 4", len(lines))
	}
}

// final_state.json is named by the frozen oracle contract. It was previously
// never written, so every external oracle in Phase 3 that opened
// final_state_path would have failed at the open() call.
func TestFinalStateIsWrittenAndWellFormed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "final_state.json")

	if err := writeFinalState(path, nil, nil); err != nil {
		t.Fatalf("writeFinalState with no topology must still produce a document: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fs FinalState
	if err := json.Unmarshal(b, &fs); err != nil {
		t.Fatalf("final_state.json is not valid JSON: %v", err)
	}
	if fs.Schema != finalStateSchema {
		t.Fatalf("schema = %q, want %q", fs.Schema, finalStateSchema)
	}
	// Empty collections must serialise as [] rather than null: the document is
	// read by third-party oracles, and null-versus-[] is the classic source of
	// a crash in a consumer written in another language.
	if !strings.Contains(string(b), `"nodes": []`) {
		t.Fatalf("empty nodes must render as [], not null:\n%s", b)
	}
	if !strings.Contains(string(b), `"phases": []`) {
		t.Fatalf("empty phases must render as [], not null:\n%s", b)
	}
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readAll(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimRight(string(b), "\n"), "\n")
}
