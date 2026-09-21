package plan

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// TestGoldenPlanDecodes is the fixture's half of the cross-module format pin.
//
// The bytes in testdata/golden_plan.json are produced by the harness's own
// encoder (internal/driver's TestOperationPlanGoldenFile). Neither module
// imports the other, so this is the only thing standing between "the harness
// writes a plan" and "the driver reads the same plan". If it fails, the format
// has forked and the driver would replay a workload the minimizer is not
// reasoning about.
func TestGoldenPlanDecodes(t *testing.T) {
	data, err := os.ReadFile("testdata/golden_plan.json")
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if bytes.Contains(data, []byte("\r")) {
		t.Fatal("the golden carries a CR byte: git has translated line endings. " +
			"Check .gitattributes (`*.json text eol=lf`)")
	}
	p, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !p.HasOperations() {
		t.Fatal("the golden decoded with no operation trace")
	}
	if p.Profile != "linear" || p.Seed != 424242 {
		t.Errorf("profile/seed = %q/%d", p.Profile, p.Seed)
	}
	if p.Origin != "drive" || p.OriginNS != 1788764420000000000 {
		t.Errorf("origin = %q/%d", p.Origin, p.OriginNS)
	}
	want := []Op{
		{OpID: 11402, Process: 3, F: FWrite, Key: "k/0", Target: "http://127.0.0.1:18082", AtMS: 2971},
		{OpID: 11430, Process: 7, F: FRead, Key: "k/0", ReadMode: ReadLease,
			Target: "http://127.0.0.1:18082", AtMS: 3004, Pinned: true},
		{OpID: 11578, Process: 7, F: FRead, Key: "k/0", ReadMode: ReadLinearizable,
			Target: "http://127.0.0.1:18081", AtMS: 4180, Pinned: true},
		{OpID: 11580, Process: 11, F: FTxn, Target: "http://127.0.0.1:18083", AtMS: 4188, Pinned: true},
		{OpID: 11999, Process: 3, F: FAdmin, Target: "http://127.0.0.1:18081", AtMS: 4900},
	}
	if len(p.Operations) != len(want) {
		t.Fatalf("operations = %d, want %d", len(p.Operations), len(want))
	}
	for i, w := range want {
		g := p.Operations[i]
		if g.OpID != w.OpID || g.Process != w.Process || g.F != w.F || g.Key != w.Key ||
			g.ReadMode != w.ReadMode || g.Target != w.Target || g.AtMS != w.AtMS || g.Pinned != w.Pinned {
			t.Errorf("operations[%d]:\n got %+v\nwant %+v", i, g, w)
		}
	}
	var v int64
	if err := json.Unmarshal(p.Operations[0].Value, &v); err != nil || v != 3000012 {
		t.Errorf("write value = %s (%v), want 3000012", p.Operations[0].Value, err)
	}
	var txn [][]json.RawMessage
	if err := json.Unmarshal(p.Operations[3].Value, &txn); err != nil {
		t.Fatalf("txn value did not decode: %v (%s)", err, p.Operations[3].Value)
	}
	if len(txn) != 2 {
		t.Fatalf("txn has %d micro-operations, want 2", len(txn))
	}
	if diff := cmpStrings(p.Targets, []string{
		"http://127.0.0.1:18081", "http://127.0.0.1:18082", "http://127.0.0.1:18083",
	}); diff != "" {
		t.Fatalf("targets: %s", diff)
	}
}

// TestAProfilePlanIsNotAReplayPlan.
//
// PROTHESIS_PLAN_PATH has pointed at a profile plan on every run since Phase 1.
// A driver keying replay off the variable's existence would find one, read zero
// operations, and report on a system it never touched.
func TestAProfilePlanIsNotAReplayPlan(t *testing.T) {
	data := []byte(`{"schema":"prothesis.driver_plan/v1","profile":"linear","seed":1,` +
		`"history_path":"/h.jsonl","clients":16,"ops":60000,` +
		`"mix":[{"op":"read","weight_ppm":500000}]}`)
	p, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if p.HasOperations() {
		t.Fatal("a profile plan claims to carry a trace")
	}
	if p.Clients != 16 || p.Ops != 60000 {
		t.Errorf("the profile members did not survive: %+v", p)
	}
}

// TestAnExplicitlyEmptyTraceIsRejected.
func TestAnExplicitlyEmptyTraceIsRejected(t *testing.T) {
	data := []byte(`{"schema":"prothesis.driver_plan/v1","profile":"linear","operations":[]}`)
	if _, err := Parse(data); err == nil {
		t.Fatal("an empty trace was accepted; a driver handed one drives nothing")
	}
}

// TestAForeignDocumentIsRejected.
func TestAForeignDocumentIsRejected(t *testing.T) {
	for _, tc := range []struct {
		name string
		data string
	}{
		{"wrong schema", `{"schema":"prothesis.verdict/v1"}`},
		{"no schema", `{"profile":"linear"}`},
		{"not json", `nope`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Parse([]byte(tc.data)); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}

// TestValidateRefusesRatherThanSkippingAnUnknownFunction.
//
// Skipping is the dangerous option: the driver would replay PART of the trace
// and write a history that silently differs from the one the minimizer is
// reasoning about, so the candidate's result would be attributed to the wrong
// workload.
func TestValidateRefusesRatherThanSkippingAnUnknownFunction(t *testing.T) {
	p := &Plan{Schema: Schema, Operations: []Op{{OpID: 1, F: "teleport", Key: "k/0"}}}
	err := p.Validate()
	if err == nil {
		t.Fatal("an unexecutable operation was accepted")
	}
	if !strings.Contains(err.Error(), "will not replay part of it") {
		t.Errorf("the error does not say why refusing beats skipping: %v", err)
	}
}

// TestValidateRejectsAMalformedOperation.
func TestValidateRejectsAMalformedOperation(t *testing.T) {
	for _, tc := range []struct {
		name string
		op   Op
	}{
		{"write with no value", Op{OpID: 1, F: FWrite, Key: "k/0"}},
		{"write with a string value", Op{OpID: 1, F: FWrite, Key: "k/0", Value: json.RawMessage(`"7"`)}},
		{"write with no key", Op{OpID: 1, F: FWrite, Value: json.RawMessage(`7`)}},
		{"read with no key", Op{OpID: 1, F: FRead}},
		{"read with an unknown mode", Op{OpID: 1, F: FRead, Key: "k/0", ReadMode: "eventual"}},
		{"txn with no ops", Op{OpID: 1, F: FTxn}},
		{"no f", Op{OpID: 1, Key: "k/0"}},
		{"zero op id", Op{OpID: 0, F: FAdmin}},
		{"negative process", Op{OpID: 1, F: FAdmin, Process: -1}},
		{"negative offset", Op{OpID: 1, F: FAdmin, AtMS: -1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &Plan{Schema: Schema, Operations: []Op{tc.op}}
			if err := p.Validate(); err == nil {
				t.Fatalf("accepted %+v", tc.op)
			}
		})
	}
}

// TestValidateRejectsANonAscendingTrace.
func TestValidateRejectsANonAscendingTrace(t *testing.T) {
	p := &Plan{Schema: Schema, Operations: []Op{
		{OpID: 5, F: FAdmin}, {OpID: 4, F: FAdmin},
	}}
	if err := p.Validate(); err == nil {
		t.Fatal("a descending trace was accepted")
	}
}

// TestValidateRejectsAnUnlistedTarget.
func TestValidateRejectsAnUnlistedTarget(t *testing.T) {
	p := &Plan{Schema: Schema, Targets: []string{"http://a"},
		Operations: []Op{{OpID: 1, F: FAdmin, Target: "http://b"}}}
	if err := p.Validate(); err == nil {
		t.Fatal("an op naming an unlisted target was accepted; the driver would silently " +
			"pick some other node")
	}
}

// TestPositionIsAnIndexNotAnAddress.
//
// Under parallel execution every world publishes different host ports, so a URL
// recorded in one world addresses somebody else's cluster in another, or
// nothing at all, and a world that drove nothing still writes a clean-looking
// history.
func TestPositionIsAnIndexNotAnAddress(t *testing.T) {
	p := &Plan{Targets: []string{"http://127.0.0.1:18081", "http://127.0.0.1:18082"}}
	if got := p.Position("http://127.0.0.1:18082"); got != 1 {
		t.Errorf("Position = %d, want 1", got)
	}
	if got := p.Position("http://127.0.0.1:19999"); got != -1 {
		t.Errorf("Position of an unknown target = %d, want -1", got)
	}
}

// TestProcessIdentitiesSurviveAReduction.
func TestProcessIdentitiesSurviveAReduction(t *testing.T) {
	p := &Plan{Operations: []Op{
		{OpID: 1, Process: 11, F: FAdmin},
		{OpID: 2, Process: 7, F: FAdmin},
		{OpID: 3, Process: 11, F: FAdmin},
	}}
	if diff := cmpInts(p.Processes(), []int{7, 11}); diff != "" {
		t.Fatalf("processes: %s", diff)
	}
	groups := p.ByProcess()
	if len(groups[11]) != 2 || groups[11][0].OpID != 1 || groups[11][1].OpID != 3 {
		t.Errorf("process 11's group is not in plan order: %+v", groups[11])
	}
	if p.SpanMS() != 0 {
		t.Errorf("SpanMS = %d, want 0", p.SpanMS())
	}
}

func cmpStrings(got, want []string) string {
	if len(got) != len(want) {
		return "length " + itoa(len(got)) + " != " + itoa(len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			return "at " + itoa(i) + ": " + got[i] + " != " + want[i]
		}
	}
	return ""
}

func cmpInts(got, want []int) string {
	if len(got) != len(want) {
		return "length " + itoa(len(got)) + " != " + itoa(len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			return "at " + itoa(i) + ": " + itoa(got[i]) + " != " + itoa(want[i])
		}
	}
	return ""
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		return "-" + string(b)
	}
	return string(b)
}
