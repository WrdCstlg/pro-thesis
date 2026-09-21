package schema

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
)

// TestGoldenHistoryRoundTrip pins every field name and shape in directive 4.4.
//
// Comparison is on the CANONICAL (key-sorted) form, because the directive's own
// five lines are not internally consistent about member order: line 2 writes
// op_id last while line 4 writes error before op_id. Object member order is not
// semantic, so canonical equality is the honest assertion. Lines whose member
// order matches this package's emission order are additionally compared byte
// for byte.
func TestGoldenHistoryRoundTrip(t *testing.T) {
	raw := readTestdata(t, "history.golden.jsonl")
	lines := splitJSONL(raw)
	if len(lines) != 5 {
		t.Fatalf("golden history has %d lines, want 5", len(lines))
	}

	entries, err := ReadAllHistory(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("ReadAllHistory: %v", err)
	}
	if len(entries) != 5 {
		t.Fatalf("read %d entries, want 5", len(entries))
	}

	for i, e := range entries {
		if err := e.Validate(); err != nil {
			t.Errorf("line %d does not validate: %v", i+1, err)
		}
		out, err := e.MarshalLine()
		if err != nil {
			t.Fatalf("line %d: MarshalLine: %v", i+1, err)
		}
		wantCanon, err := Canonicalize(lines[i])
		if err != nil {
			t.Fatalf("line %d: canonicalize golden: %v", i+1, err)
		}
		gotCanon, err := Canonicalize(out)
		if err != nil {
			t.Fatalf("line %d: canonicalize re-emission: %v", i+1, err)
		}
		if !bytes.Equal(gotCanon, wantCanon) {
			t.Errorf("line %d re-emitted differently:\n got: %s\nwant: %s", i+1, gotCanon, wantCanon)
		}
	}

	// Lines 1, 2, 3 and 5 are written in this package's emission order, so they
	// must come back byte for byte.
	for _, i := range []int{0, 1, 2, 4} {
		out, _ := entries[i].MarshalLine()
		if !bytes.Equal(out, lines[i]) {
			t.Errorf("line %d is not byte-identical:\n got: %s\nwant: %s", i+1, out, lines[i])
		}
	}

	// Field-by-field spot checks on the shapes the union depends on.
	e0 := entries[0]
	if e0.TNS != 1725300000000000 || e0.Type != HistoryInvoke || e0.F != "write" {
		t.Errorf("entries[0] = %+v", e0)
	}
	if e0.Process == nil || *e0.Process != 3 {
		t.Errorf("entries[0].process = %v, want 3", e0.Process)
	}
	if e0.Key == nil || *e0.Key != "k/42" {
		t.Errorf("entries[0].key = %v, want k/42", e0.Key)
	}
	if string(e0.Value) != "7" {
		t.Errorf("entries[0].value = %q, want raw 7", e0.Value)
	}
	if e0.OpID == nil || *e0.OpID != 8891 {
		t.Errorf("entries[0].op_id = %v, want 8891", e0.OpID)
	}
	if entries[3].Error != "timeout" || entries[3].Type != HistoryFail {
		t.Errorf("entries[3] = %+v", entries[3])
	}

	marker := entries[4]
	if marker.RecordKind() != RecordMarker {
		t.Errorf("the phase line is a %s, want a marker", marker.RecordKind())
	}
	if marker.Event != EventPhase || marker.Phase != PhaseHeal {
		t.Errorf("marker = %+v", marker)
	}
}

func splitJSONL(b []byte) [][]byte {
	var out [][]byte
	for _, l := range bytes.Split(b, []byte("\n")) {
		l = bytes.TrimSuffix(l, []byte("\r"))
		if len(bytes.TrimSpace(l)) == 0 {
			continue
		}
		out = append(out, l)
	}
	return out
}

// TestInfoIsOverloaded is the reason `event`, not `type`, is the union
// discriminator. Both records below have type "info" and both are normative:
// one is an indeterminate operation outcome, the other a phase marker.
func TestInfoIsOverloaded(t *testing.T) {
	pid := int64(5)
	opID := int64(8903)
	indeterminate := HistoryEntry{
		TNS: 1, Process: &pid, Type: HistoryInfo, F: "write",
		Error: "timeout", OpID: &opID,
	}
	if indeterminate.RecordKind() != RecordOperation {
		t.Error("an info record with no event is an operation record")
	}
	if !indeterminate.IsIndeterminate() {
		t.Error("IsIndeterminate = false on an info operation outcome")
	}
	if err := indeterminate.Validate(); err != nil {
		t.Errorf("indeterminate op record rejected: %v", err)
	}

	marker := PhaseMarker(2, PhaseHeal)
	if marker.RecordKind() != RecordMarker {
		t.Error("a record carrying an event is a marker record")
	}
	if marker.IsIndeterminate() {
		t.Error("a marker is not an indeterminate operation")
	}
	if err := marker.Validate(); err != nil {
		t.Errorf("phase marker rejected: %v", err)
	}
}

func TestHistoryUnionValidation(t *testing.T) {
	pid := int64(0)
	cases := map[string]HistoryEntry{
		"marker with the wrong type": {TNS: 1, Type: HistoryOK, Event: EventPhase, Phase: PhaseHeal},
		"phase marker with no phase": {TNS: 1, Type: HistoryInfo, Event: EventPhase},
		"marker carrying an op name": {TNS: 1, Type: HistoryInfo, Event: EventNote, F: "read"},
		"op record with no f":        {TNS: 1, Type: HistoryOK, Process: &pid},
		"op record carrying a phase": {TNS: 1, Type: HistoryOK, F: "read", Phase: PhaseHeal},
		"unknown type":               {TNS: 1, Type: HistoryType("done"), F: "read"},
		// OQ-058. op_id and value are what make a record evidence. A reader
		// discriminates on `event` alone and skips markers, so a record carrying
		// both an event and an op_id is an operation that vanishes without being
		// counted: which hides a real violation or manufactures a false one.
		"marker carrying an op_id": {TNS: 1, Type: HistoryInfo, Event: EventNote, OpID: &pid},
		"marker carrying a value": {TNS: 1, Type: HistoryInfo, Event: EventNote,
			Value: json.RawMessage(`7`)},
	}
	for name, e := range cases {
		if err := e.Validate(); err == nil {
			t.Errorf("%s: Validate accepted it", name)
		}
	}
}

// TestHistoryAbsentVersusZero: process 0, op_id 0, the empty key and the values
// 0, false and null must all stay distinguishable from "the field was absent".
func TestHistoryAbsentVersusZero(t *testing.T) {
	zero := int64(0)
	empty := ""
	e := HistoryEntry{
		TNS: 1, Type: HistoryOK, F: "read",
		Process: &zero, OpID: &zero, Key: &empty, Value: json.RawMessage("false"),
	}
	line, err := e.MarshalLine()
	if err != nil {
		t.Fatalf("MarshalLine: %v", err)
	}
	for _, want := range []string{`"process":0`, `"op_id":0`, `"key":""`, `"value":false`} {
		if !strings.Contains(string(line), want) {
			t.Errorf("emitted line lost %s: %s", want, line)
		}
	}

	var back HistoryEntry
	if err := json.Unmarshal(line, &back); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if back.Process == nil || *back.Process != 0 {
		t.Errorf("process 0 came back as %v", back.Process)
	}
	if back.Key == nil || *back.Key != "" {
		t.Errorf("empty key came back as %v", back.Key)
	}
	if string(back.Value) != "false" {
		t.Errorf("value came back as %q", back.Value)
	}

	absent := HistoryEntry{TNS: 1, Type: HistoryOK, F: "read"}
	line, _ = absent.MarshalLine()
	if strings.Contains(string(line), "process") || strings.Contains(string(line), "op_id") ||
		strings.Contains(string(line), "key") || strings.Contains(string(line), "value") {
		t.Errorf("absent fields were emitted: %s", line)
	}
}

// TestHistoryLargeIntegerValueSurvives: `value` is raw JSON precisely so a
// value above 2^53 is not mangled by a float64 round trip.
func TestHistoryLargeIntegerValueSurvives(t *testing.T) {
	const big = "9007199254740993" // 2^53 + 1
	e := HistoryEntry{TNS: 1, Type: HistoryOK, F: "write", Value: json.RawMessage(big)}
	line, err := e.MarshalLine()
	if err != nil {
		t.Fatalf("MarshalLine: %v", err)
	}
	var back HistoryEntry
	if err := json.Unmarshal(line, &back); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if string(back.Value) != big {
		t.Errorf("value = %q, want %q", back.Value, big)
	}
}

// TestHistoryReaderSurvivesABadLine: a history is written by an external,
// user-authored load generator. One malformed line must not abort a
// 500,000-line read, and it must be reportable.
func TestHistoryReaderSurvivesABadLine(t *testing.T) {
	const doc = `{"t_ns":1,"type":"ok","f":"read"}
{"t_ns":2,"type":"ok","f":  BROKEN
{"t_ns":3,"type":"ok","f":"read"}

{"t_ns":4,"type":"info","event":"phase","phase":"HEAL"}
`
	r := NewHistoryReader(strings.NewReader(doc))
	var good, bad int
	for {
		e, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			var le *HistoryLineError
			if !errors.As(err, &le) {
				t.Fatalf("unexpected error: %v", err)
			}
			if le.Line != 2 {
				t.Errorf("bad line reported as %d, want 2", le.Line)
			}
			if !strings.Contains(le.Raw, "BROKEN") {
				t.Errorf("raw line not carried: %q", le.Raw)
			}
			bad++
			continue
		}
		_ = e
		good++
	}
	if good != 3 || bad != 1 {
		t.Errorf("read %d good and %d bad lines, want 3 and 1", good, bad)
	}
}

// TestHistoryReaderHandlesCRLFAndLongLines.
func TestHistoryReaderHandlesCRLFAndLongLines(t *testing.T) {
	long := strings.Repeat("x", 200*1024)
	doc := "{\"t_ns\":1,\"type\":\"ok\",\"f\":\"read\",\"value\":\"" + long + "\"}\r\n" +
		"{\"t_ns\":2,\"type\":\"info\",\"event\":\"phase\",\"phase\":\"HEAL\"}\r\n"
	entries, err := ReadAllHistory(strings.NewReader(doc))
	if err != nil {
		t.Fatalf("ReadAllHistory: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("read %d entries, want 2", len(entries))
	}
	if len(entries[0].Value) != len(long)+2 {
		t.Errorf("long value truncated: %d bytes", len(entries[0].Value))
	}
}

func TestHistoryWriterRefusesInvalidRecords(t *testing.T) {
	var buf bytes.Buffer
	w := NewHistoryWriter(&buf)
	if err := w.Write(&HistoryEntry{TNS: 1, Type: HistoryOK}); err == nil {
		t.Error("wrote an operation record with no operation name")
	}
	if err := w.Write(&HistoryEntry{TNS: 1, Type: HistoryOK, F: "read"}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	m := PhaseMarker(2, PhaseDrive)
	if err := w.Write(&m); err != nil {
		t.Fatalf("Write marker: %v", err)
	}
	if err := w.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if w.Count() != 2 {
		t.Errorf("Count = %d, want 2", w.Count())
	}
	entries, err := ReadAllHistory(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("ReadAllHistory: %v", err)
	}
	if len(entries) != 2 {
		t.Errorf("wrote %d readable entries, want 2", len(entries))
	}
}
