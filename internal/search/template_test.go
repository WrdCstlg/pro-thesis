package search

import (
	"fmt"
	"strings"
	"testing"
)

// Real lines from the kv fixture, in the exact format its logger emits
// (testdata/kvfixture/internal/raft/raft.go, internal/kv/server.go and
// cmd/kv/main.go). They are copied rather than invented so that a change to the
// fixture's log format shows up here as a failing normalisation test rather
// than as a silently degraded coverage signal.
var fixtureLines = []string{
	`level=info event=boot node=kv-n1 variant=buggy lease_ms=5000 tick_ms=50 hb_ms=150 election_ms=600-1200 peers=[kv-n2 kv-n3] client=:8080 peer=:9090 data_dir="/data/kv-n1" seed=42 node_seed=7`,
	`level=info event=role_change node=kv-n2 role=follower term=65 leader="kv-n1"`,
	`level=info event=election_start node=kv-n1 term=66 last_index=11402 last_term=65`,
	`level=info event=elected node=kv-n1 term=66 noop_index=11403`,
	`level=warn event=request_error node=kv-n2 status=503 code=not_leader msg="no leader"`,
	`level=error event=persist_state err="disk full"`,
	`level=error event=append_wal err="disk full"`,
	`level=fatal event=truncate_committed node=kv-n3 index=9001 commit_index=9000 term=66 leader="kv-n1"`,
	`level=info event=shutdown node=kv-n1 signal=terminated`,
	`level=info event=stopped node=kv-n1`,
}

// ---------------------------------------------------------------------------
// UNDER-normalisation: variable fields must collapse
// ---------------------------------------------------------------------------

func TestNormalisationCollapsesVariableFields(t *testing.T) {
	cases := []struct {
		name string
		a, b string
	}{
		{
			"the term counter",
			`level=info event=role_change node=kv-n2 role=follower term=65 leader="kv-n1"`,
			`level=info event=role_change node=kv-n2 role=follower term=66 leader="kv-n1"`,
		},
		{
			"the node index",
			`level=info event=role_change node=kv-n1 role=follower term=65 leader="kv-n1"`,
			`level=info event=role_change node=kv-n3 role=follower term=65 leader="kv-n1"`,
		},
		{
			"a log index that crosses a digit boundary",
			`level=info event=elected node=kv-n1 term=66 noop_index=9999999`,
			`level=info event=elected node=kv-n1 term=66 noop_index=10000000`,
		},
		{
			"a duration",
			`level=info event=heartbeat took=12ms`,
			`level=info event=heartbeat took=1.5s`,
		},
		{
			"a compound duration",
			`level=info event=uptime for=1h2m3.5s`,
			`level=info event=uptime for=900ms`,
		},
		{
			"a peer address",
			`level=warn event=dial_failed peer=172.18.0.4:9090`,
			`level=warn event=dial_failed peer=172.18.0.7:9090`,
		},
		{
			"a wall-clock timestamp inside the message",
			`level=info event=lease_granted until=2026-09-08T12:00:01.250Z`,
			`level=info event=lease_granted until=2026-09-08T12:00:09.998Z`,
		},
		{
			"a request uuid",
			`level=info event=request id=7f3e9c1a-4b2d-4e88-9a11-0c5f2b7d6e30`,
			`level=info event=request id=00000000-1111-2222-3333-444444444444`,
		},
		{
			"a pointer in a panic trace",
			`goroutine 42 [running]: main.serve(0xc000123456)`,
			`goroutine 9 [running]: main.serve(0xc000abcdef)`,
		},
		{
			"a docker --timestamps prefix",
			`2026-09-08T11:59:59.123456789Z level=info event=stopped node=kv-n1`,
			`2026-09-08T12:00:03.000000001Z level=info event=stopped node=kv-n1`,
		},
		{
			"a Go log package date/time prefix",
			`2026/09/08 11:59:59.123456 level=info event=stopped node=kv-n1`,
			`2026/09/08 12:00:03.000001 level=info event=stopped node=kv-n1`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			na, nb := NormalizeLine(c.a), NormalizeLine(c.b)
			if na != nb {
				t.Fatalf("under-normalised: these are the same code path but produced two templates\n  a: %s\n  -> %s\n  b: %s\n  -> %s",
					c.a, na, c.b, nb)
			}
		})
	}
}

// TestATailOfOneCodePathIsOneTemplate is the aggregate form of the same claim.
// A thousand heartbeat lines are one code path; a coverage signal that reported
// a thousand templates would be reporting log volume.
func TestATailOfOneCodePathIsOneTemplate(t *testing.T) {
	ts := NewTemplateSet()
	for i := 0; i < 1000; i++ {
		ts.Add("kv-n1", fmt.Sprintf(
			`level=info event=append_entries node=kv-n%d term=%d prev_index=%d entries=%d took=%dms`,
			(i%3)+1, 60+i/50, 11000+i, i%4, 3+i%9))
	}
	if got := ts.Len(); got != 1 {
		t.Fatalf("1000 lines of one code path produced %d templates, want 1:\n%s",
			got, renderTemplates(ts))
	}
	if got := ts.Lines(); got != 1000 {
		t.Fatalf("Lines() = %d, want 1000 (the set must still know how much it saw)", got)
	}
}

// ---------------------------------------------------------------------------
// OVER-normalisation: distinct code paths must stay distinct
// ---------------------------------------------------------------------------

func TestNormalisationKeepsDistinctCodePathsDistinct(t *testing.T) {
	cases := []struct {
		name string
		a, b string
	}{
		{
			"two different events",
			`level=info event=election_start node=kv-n1 term=66 last_index=11402 last_term=65`,
			`level=info event=elected node=kv-n1 term=66 noop_index=11403`,
		},
		{
			"two different severities",
			`level=info event=persist_state err="disk full"`,
			`level=error event=persist_state err="disk full"`,
		},
		{
			"two different error sites with the same message",
			`level=error event=persist_state err="disk full"`,
			`level=error event=append_wal err="disk full"`,
		},
		{
			"two different roles",
			`level=info event=role_change node=kv-n2 role=follower term=65 leader="kv-n1"`,
			`level=info event=role_change node=kv-n2 role=leader term=65 leader="kv-n1"`,
		},
		{
			"two different refusal codes behind the same status",
			`level=warn event=request_error node=kv-n2 status=503 code=not_leader msg="no leader"`,
			`level=warn event=request_error node=kv-n2 status=503 code=lease_expired msg="no leader"`,
		},
		{
			"two different quoted messages",
			`level=warn event=step_down node=kv-n2 msg="higher term seen"`,
			`level=warn event=step_down node=kv-n2 msg="lease expired"`,
		},
		{
			"a negative clock offset is not a positive one",
			`level=info event=clock_offset ms=-1000`,
			`level=info event=clock_offset ms=1000`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			na, nb := NormalizeLine(c.a), NormalizeLine(c.b)
			if na == nb {
				t.Fatalf("over-normalised: two distinct code paths collapsed into one template\n  a: %s\n  b: %s\n  both -> %s",
					c.a, c.b, na)
			}
		})
	}
}

// TestEveryFixtureLineIsItsOwnTemplate proves the normaliser does not fold the
// fixture's ten distinct log sites into fewer. It is the aggregate
// over-normalisation guard, and it fails loudly if a future rule (collapsing
// quoted strings, say, or whole key=value pairs) is added without noticing what
// it costs.
func TestEveryFixtureLineIsItsOwnTemplate(t *testing.T) {
	ts := NewTemplateSet()
	for _, ln := range fixtureLines {
		ts.Add("kv-n1", ln)
	}
	if got, want := ts.Len(), len(fixtureLines); got != want {
		t.Fatalf("the fixture's %d distinct log sites produced %d templates:\n%s",
			want, got, renderTemplates(ts))
	}
}

// TestNormalisationIsIdempotent: normalising a template must be a fixed point,
// or the coverage counter would depend on how many times a line was processed.
func TestNormalisationIsIdempotent(t *testing.T) {
	for _, ln := range fixtureLines {
		once := NormalizeLine(ln)
		twice := NormalizeLine(once)
		if once != twice {
			t.Fatalf("not idempotent:\n  raw:   %s\n  once:  %s\n  twice: %s", ln, once, twice)
		}
	}
}

// TestPlaceholdersCarryNoDigits pins the invariant the ordering depends on. The
// numeric rule has no left word boundary (it must, so `kv-n1` collapses), so a
// placeholder containing a digit would be partially rewritten by a later rule
// and the normaliser would stop being idempotent.
func TestPlaceholdersCarryNoDigits(t *testing.T) {
	for _, p := range []string{
		PlaceholderTimestamp, PlaceholderUUID, PlaceholderIP, PlaceholderDuration,
		PlaceholderSize, PlaceholderHex, PlaceholderNumber, PlaceholderTruncated,
	} {
		if strings.ContainsAny(p, "0123456789") {
			t.Fatalf("placeholder %q contains a digit; the numeric rule would rewrite it", p)
		}
	}
}

// TestABlankLineIsNotEvidence: a blank or whitespace-only line is not a code
// path, and counting it would let a chatty formatter inflate coverage.
func TestABlankLineIsNotEvidence(t *testing.T) {
	ts := NewTemplateSet()
	for _, ln := range []string{"", "   ", "\t", "\x1b[0m"} {
		if id, isNew := ts.Add("kv-n1", ln); isNew || id != "" {
			t.Fatalf("blank line %q was counted as template %q", ln, id)
		}
	}
	if ts.Len() != 0 {
		t.Fatalf("blank lines produced %d templates", ts.Len())
	}
}

// TestALongLineIsTruncatedNotDropped: an unbounded line must not be discarded
// (it is still evidence) and must not be allowed to make every line unique.
func TestALongLineIsTruncatedNotDropped(t *testing.T) {
	long := "level=error event=panic stack=" + strings.Repeat("a", MaxLineBytes*2)
	got := NormalizeLine(long)
	if got == "" {
		t.Fatal("a long line was dropped entirely")
	}
	if !strings.Contains(got, PlaceholderTruncated) {
		t.Fatalf("a line over %d bytes was not marked truncated: %.80s...", MaxLineBytes, got)
	}
	if len(got) > MaxLineBytes+64 {
		t.Fatalf("truncation did not bound the template: %d bytes", len(got))
	}
}

// ---------------------------------------------------------------------------
// Set behaviour
// ---------------------------------------------------------------------------

func TestTemplateSetTracksNodesAndMergesDeterministically(t *testing.T) {
	a := NewTemplateSet()
	a.Add("kv-n1", fixtureLines[1])
	a.Add("kv-n3", fixtureLines[1])
	a.Add("kv-n1", fixtureLines[2])

	b := NewTemplateSet()
	b.Add("kv-n2", fixtureLines[1])
	b.Add("kv-n2", fixtureLines[4])

	// Score before merging: the delta must be measured against the set as it
	// stood, or every world would report zero new templates.
	if got := b.NewAgainst(a); got != 1 {
		t.Fatalf("b adds %d templates to a, want 1", got)
	}
	if got := a.Merge(b); got != 1 {
		t.Fatalf("Merge reported %d new, want 1", got)
	}
	if a.Len() != 3 {
		t.Fatalf("merged set has %d templates, want 3", a.Len())
	}

	var roleChange *TemplateStat
	for _, s := range a.Stats() {
		if strings.Contains(s.Template, "event=role_change") {
			st := s
			roleChange = &st
		}
	}
	if roleChange == nil {
		t.Fatal("the role_change template disappeared from the merged set")
	}
	want := []string{"kv-n1", "kv-n2", "kv-n3"}
	if strings.Join(roleChange.Nodes, ",") != strings.Join(want, ",") {
		t.Fatalf("nodes = %v, want %v (sorted and deduplicated)", roleChange.Nodes, want)
	}
}

func TestTemplateIDsAreSortedAndStable(t *testing.T) {
	build := func() *TemplateSet {
		ts := NewTemplateSet()
		for _, ln := range fixtureLines {
			ts.Add("kv-n1", ln)
		}
		return ts
	}
	first := strings.Join(build().IDs(), ",")
	for i := 0; i < 8; i++ {
		if got := strings.Join(build().IDs(), ","); got != first {
			t.Fatalf("template ids are not stable across builds:\n  %s\n  %s", first, got)
		}
	}
	ids := build().IDs()
	for i := 1; i < len(ids); i++ {
		if ids[i-1] >= ids[i] {
			t.Fatalf("ids are not sorted: %v", ids)
		}
	}
}

func renderTemplates(ts *TemplateSet) string {
	var b strings.Builder
	for _, s := range ts.Stats() {
		fmt.Fprintf(&b, "  %s x%d  %s\n", s.ID, s.Count, s.Template)
	}
	return b.String()
}
