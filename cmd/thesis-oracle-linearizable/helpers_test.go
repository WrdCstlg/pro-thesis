package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// ---------------------------------------------------------------------------
// History construction helpers
//
// Histories are built as raw JSONL rather than through schema.HistoryWriter so a
// test can write a record the writer would reject (a missing op_id, a torn
// line) which is exactly the input this checker has to refuse safely.
// ---------------------------------------------------------------------------

type rec struct {
	t     int64
	proc  int64
	noPID bool
	typ   schema.HistoryType
	f     string
	key   string
	noKey bool
	val   string // raw JSON; "" means the field is absent
	id    int64
	noID  bool
	errS  string
	// ev sets the `event` field, which is the marker/operation discriminator.
	// Setting it on a record that also carries operation fields is malformed by
	// construction (the shape OQ-058 is about) and no writer would emit it,
	// which is why it can only be built here.
	ev string
}

func (r rec) entry() schema.HistoryEntry {
	e := schema.HistoryEntry{TNS: r.t, Type: r.typ, F: r.f, Error: r.errS, Event: r.ev}
	if !r.noPID {
		p := r.proc
		e.Process = &p
	}
	if !r.noKey {
		k := r.key
		e.Key = &k
	}
	if r.val != "" {
		e.Value = json.RawMessage(r.val)
	}
	if !r.noID {
		id := r.id
		e.OpID = &id
	}
	return e
}

// invokeOK is one complete operation: an invoke and an ok completion.
func op(id, proc int64, f, key string, t0, t1 int64, invokeVal, doneVal string, typ schema.HistoryType) []rec {
	return []rec{
		{t: t0, proc: proc, typ: schema.HistoryInvoke, f: f, key: key, val: invokeVal, id: id},
		{t: t1, proc: proc, typ: typ, f: f, key: key, val: doneVal, id: id},
	}
}

func wOK(id int64, key string, t0, t1 int64, v string) []rec {
	return op(id, 0, "write", key, t0, t1, v, v, schema.HistoryOK)
}

func wInfo(id int64, key string, t0, t1 int64, v string) []rec {
	return op(id, 0, "write", key, t0, t1, v, v, schema.HistoryInfo)
}

func wFail(id int64, key string, t0, t1 int64, v string) []rec {
	return op(id, 0, "write", key, t0, t1, v, v, schema.HistoryFail)
}

// wPending is an invoke with no completion record at all.
func wPending(id int64, key string, t0 int64, v string) []rec {
	return []rec{{t: t0, proc: 0, typ: schema.HistoryInvoke, f: "write", key: key, val: v, id: id}}
}

func rOK(id int64, key string, t0, t1 int64, v string) []rec {
	return op(id, 1, "read", key, t0, t1, "", v, schema.HistoryOK)
}

// tag sets `event` on every record in a group. This is how a real history
// acquires the OQ-058 shape: a driver that labels its own operations, or a
// second writer whose marker fields land on an operation line.
func tag(ev string, recs []rec) []rec {
	out := make([]rec, len(recs))
	copy(out, recs)
	for i := range out {
		out[i].ev = ev
	}
	return out
}

func cat(groups ...[]rec) []rec {
	out := make([]rec, 0, 16)
	for _, g := range groups {
		out = append(out, g...)
	}
	return out
}

// writeHistory renders records to a temp JSONL file and returns its path.
func writeHistory(t *testing.T, recs []rec, extraLines ...string) string {
	t.Helper()
	var b strings.Builder
	for _, r := range recs {
		e := r.entry()
		line, err := e.MarshalLine()
		if err != nil {
			t.Fatalf("marshal record %+v: %v", r, err)
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	for _, l := range extraLines {
		b.WriteString(l)
		b.WriteByte('\n')
	}
	path := filepath.Join(t.TempDir(), "history.jsonl")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatalf("write history: %v", err)
	}
	return path
}

// testOptions is a generous budget, so a test that reports inconclusive is
// reporting a real refusal and not a starved search.
func testOptions() options {
	return options{Wall: 20 * time.Second, MaxStates: 2_000_000, MaxMemoBytes: 64 << 20}
}

func run1(t *testing.T, recs []rec, extra ...string) report {
	t.Helper()
	return check(writeHistory(t, recs, extra...), testOptions(), time.Now())
}

func wantStatus(t *testing.T, got report, want schema.OracleStatus) {
	t.Helper()
	if got.Status != want {
		t.Fatalf("status = %q, want %q\nexplanation: %s", got.Status, want, got.Explanation)
	}
}

func wantOpIDs(t *testing.T, got report, want ...int64) {
	t.Helper()
	if len(got.Witness.OpIDs) != len(want) {
		t.Fatalf("witness op_ids = %v, want %v\nexplanation: %s",
			got.Witness.OpIDs, want, got.Explanation)
	}
	for i := range want {
		if got.Witness.OpIDs[i] != want[i] {
			t.Fatalf("witness op_ids = %v, want %v\nexplanation: %s",
				got.Witness.OpIDs, want, got.Explanation)
		}
	}
}

func wantMentions(t *testing.T, got report, needles ...string) {
	t.Helper()
	for _, n := range needles {
		if !strings.Contains(got.Explanation, n) {
			t.Fatalf("explanation does not mention %q\nexplanation: %s", n, got.Explanation)
		}
	}
}

// seq builds a long single-key history of alternating writes and reads that is
// linearizable, used to force budget exhaustion.
func seq(key string, n int) []rec {
	out := make([]rec, 0, 2*n)
	var t int64
	for i := 0; i < n; i++ {
		id := int64(1000 + i)
		v := fmt.Sprintf("%d", i)
		out = append(out, wOK(id, key, t, t+1, v)...)
		t += 2
		out = append(out, rOK(id+500, key, t, t+1, v)...)
		t += 2
	}
	return out
}
