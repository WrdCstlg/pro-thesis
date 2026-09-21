package driver

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A small, realistic merged history: the harness's phase markers interleaved
// with the driver's op records, exactly as MergeHistory produces them (D-011).
//
// DRIVE opens at t_ns 1_000_000_000. Offsets below are therefore 100ms, 140ms,
// 250ms and 900ms.
const sampleHistory = `{"t_ns":1000000000,"type":"info","event":"phase","phase":"DRIVE"}
{"t_ns":1100000000,"process":3,"type":"invoke","f":"write","key":"k/0","value":3000012,"op_id":11402,"meta":{"target":"http://127.0.0.1:18082","seq":1}}
{"t_ns":1120000000,"process":3,"type":"ok","f":"write","key":"k/0","value":3000012,"op_id":11402,"meta":{"target":"http://127.0.0.1:18082","node":"kv-n2","term":65,"seq":2}}
{"t_ns":1140000000,"process":7,"type":"invoke","f":"read","key":"k/0","op_id":11430,"meta":{"target":"http://127.0.0.1:18082","read_mode":"lease","seq":3}}
{"t_ns":1150000000,"process":7,"type":"ok","f":"read","key":"k/0","value":3000012,"op_id":11430,"meta":{"target":"http://127.0.0.1:18082","read_mode":"lease","served_by":"kv-n2","seq":4}}
{"t_ns":1250000000,"process":11,"type":"invoke","f":"txn","value":[["r","k/3",null],["w","k/0",11000044]],"op_id":11580,"meta":{"target":"http://127.0.0.1:18083","seq":5}}
{"t_ns":1260000000,"process":11,"type":"info","f":"txn","value":[["r","k/3",7],["w","k/0",11000044]],"error":"timeout","op_id":11580,"meta":{"target":"http://127.0.0.1:18083","seq":6}}
{"t_ns":1900000000,"process":7,"type":"invoke","f":"read","key":"k/0","op_id":11999,"meta":{"target":"http://127.0.0.1:18081","read_mode":"linearizable","seq":7}}
{"t_ns":2000000000,"type":"info","event":"phase","phase":"HEAL"}
`

func extractSample(t *testing.T, opts ExtractOptions) *Extraction {
	t.Helper()
	e, err := ExtractOps(strings.NewReader(sampleHistory), opts)
	if err != nil {
		t.Fatalf("ExtractOps: %v", err)
	}
	return e
}

// TestExtractionBuildsTheTraceFromInvokeRecords.
func TestExtractionBuildsTheTraceFromInvokeRecords(t *testing.T) {
	e := extractSample(t, ExtractOptions{})
	if diff := cmpInt64(planOpIDs(e.Ops), []int64{11402, 11430, 11580, 11999}); diff != "" {
		t.Fatalf("op ids: %s", diff)
	}
	want := []PlanOp{
		{OpID: 11402, Process: 3, F: "write", Key: "k/0", Target: "http://127.0.0.1:18082", AtMS: 100},
		{OpID: 11430, Process: 7, F: "read", Key: "k/0", ReadMode: "lease",
			Target: "http://127.0.0.1:18082", AtMS: 140},
		{OpID: 11580, Process: 11, F: "txn", Target: "http://127.0.0.1:18083", AtMS: 250},
		{OpID: 11999, Process: 7, F: "read", Key: "k/0", ReadMode: "linearizable",
			Target: "http://127.0.0.1:18081", AtMS: 900},
	}
	for i, w := range want {
		g := e.Ops[i]
		if g.OpID != w.OpID || g.Process != w.Process || g.F != w.F || g.Key != w.Key ||
			g.ReadMode != w.ReadMode || g.Target != w.Target || g.AtMS != w.AtMS {
			t.Errorf("ops[%d]:\n got %+v\nwant %+v", i, g, w)
		}
	}
	if got := string(e.Ops[0].Value); got != "3000012" {
		t.Errorf("write value = %s, want 3000012", got)
	}
	if diff := cmpStrings(e.Targets, []string{
		"http://127.0.0.1:18081", "http://127.0.0.1:18082", "http://127.0.0.1:18083",
	}); diff != "" {
		t.Fatalf("targets: %s", diff)
	}
	if e.PhaseMarkers != 2 {
		t.Errorf("phase markers = %d, want 2", e.PhaseMarkers)
	}
	if e.OpRecords != 7 {
		t.Errorf("op records = %d, want 7", e.OpRecords)
	}
}

// TestTheReadModeIsCarried.
//
// The reference fixture's defect lives on ONE of the two read paths: a lease
// read is served locally with no quorum round trip and can be stale, a
// linearizable read takes the read-index path and is correct. A trace that
// forgot which was asked for would replay a different operation and reproduce a
// different thing, or nothing.
func TestTheReadModeIsCarried(t *testing.T) {
	e := extractSample(t, ExtractOptions{})
	if e.Ops[1].ReadMode != "lease" {
		t.Errorf("op 11430 read_mode = %q, want lease", e.Ops[1].ReadMode)
	}
	if e.Ops[3].ReadMode != "linearizable" {
		t.Errorf("op 11999 read_mode = %q, want linearizable", e.Ops[3].ReadMode)
	}
}

// TestTheValueComesFromTheInvokeNotTheCompletion.
//
// A completion records what the system DID. For a transaction the two genuinely
// differ (the completion carries the executed ops with their read results
// filled in) so replaying a completion replays a different operation.
func TestTheValueComesFromTheInvokeNotTheCompletion(t *testing.T) {
	e := extractSample(t, ExtractOptions{})
	got := string(e.Ops[2].Value)
	if strings.Contains(got, "7") && !strings.Contains(got, "null") {
		t.Fatalf("the txn value came from the completion (read result filled in): %s", got)
	}
	if !strings.Contains(got, `["r","k/3",null]`) {
		t.Fatalf("txn value = %s, want the invoke's request shape", got)
	}
}

// TestOffsetsAreMeasuredFromTheDriveMarker.
//
// This is what makes an operation's offset comparable with a fault window: both
// are relative to DRIVE. Without it, a trace ddmin reduced to six operations
// retires in milliseconds and can never overlap a fault at @3000ms, so the
// "minimal repro" replays 0/3 and poisons every future gate run.
func TestOffsetsAreMeasuredFromTheDriveMarker(t *testing.T) {
	e := extractSample(t, ExtractOptions{})
	if e.Origin != OriginDrive {
		t.Fatalf("origin = %q, want %q", e.Origin, OriginDrive)
	}
	if e.OriginNS != 1000000000 {
		t.Fatalf("origin_ns = %d, want 1000000000", e.OriginNS)
	}
	if e.Ops[0].AtMS != 100 {
		t.Fatalf("first op at_ms = %d, want 100", e.Ops[0].AtMS)
	}
}

// TestWithoutADriveMarkerOffsetsFallBackToTheFirstOperation.
func TestWithoutADriveMarkerOffsetsFallBackToTheFirstOperation(t *testing.T) {
	h := `{"t_ns":5000000000,"process":0,"type":"invoke","f":"read","key":"k/1","op_id":1}
{"t_ns":5250000000,"process":0,"type":"invoke","f":"read","key":"k/1","op_id":2}
`
	e, err := ExtractOps(strings.NewReader(h), ExtractOptions{})
	if err != nil {
		t.Fatalf("ExtractOps: %v", err)
	}
	if e.Origin != OriginFirstOp {
		t.Fatalf("origin = %q, want %q", e.Origin, OriginFirstOp)
	}
	if e.Ops[0].AtMS != 0 || e.Ops[1].AtMS != 250 {
		t.Fatalf("offsets = %d, %d; want 0, 250", e.Ops[0].AtMS, e.Ops[1].AtMS)
	}
}

// TestAnOperationBeforeTheOriginIsClampedNotNegative.
func TestAnOperationBeforeTheOriginIsClampedNotNegative(t *testing.T) {
	h := `{"t_ns":1000000000,"type":"info","event":"phase","phase":"DRIVE"}
{"t_ns":999000000,"process":0,"type":"invoke","f":"read","key":"k/1","op_id":1}
`
	e, err := ExtractOps(strings.NewReader(h), ExtractOptions{})
	if err != nil {
		t.Fatalf("ExtractOps: %v", err)
	}
	if e.Ops[0].AtMS != 0 {
		t.Fatalf("at_ms = %d, want 0", e.Ops[0].AtMS)
	}
	if e.ClampedOffsets != 1 {
		t.Errorf("ClampedOffsets = %d, want 1; a clamp must be reported, not silent", e.ClampedOffsets)
	}
}

// TestAMalformedLineIsAnError.
//
// Skipping malformed lines silently is how the very operation a witness names
// disappears from a trace.
func TestAMalformedLineIsAnError(t *testing.T) {
	h := `{"t_ns":1,"process":0,"type":"invoke","f":"read","key":"k/1","op_id":1}
this is not json
{"t_ns":2,"process":0,"type":"ok","f":"read","key":"k/1","op_id":1}
`
	_, err := ExtractOps(strings.NewReader(h), ExtractOptions{})
	if err == nil {
		t.Fatal("a torn line was skipped silently")
	}
	if !strings.Contains(err.Error(), "line 2") {
		t.Errorf("error does not name the line: %v", err)
	}
}

// TestATruncatedTailIsToleratedAndReported.
//
// A driver killed at QUIESCE legitimately leaves a partial last line, and
// failing the whole shrink on the normal case would be its own defect.
func TestATruncatedTailIsToleratedAndReported(t *testing.T) {
	h := `{"t_ns":1,"process":0,"type":"invoke","f":"read","key":"k/1","op_id":1}
{"t_ns":2,"process":0,"type":"invoke","f":"read","ke`
	e, err := ExtractOps(strings.NewReader(h), ExtractOptions{})
	if err != nil {
		t.Fatalf("a truncated tail was fatal: %v", err)
	}
	if !e.TruncatedTail {
		t.Error("the truncated tail was not reported")
	}
	if len(e.Ops) != 1 {
		t.Fatalf("ops = %d, want 1", len(e.Ops))
	}
}

// TestTwoInvokesForOneOpIDIsAnError.
//
// Two possible executions behind one operation makes the history unsound in
// exactly the way a mis-classified timeout does, and a trace built from it
// would replay only one of them.
func TestTwoInvokesForOneOpIDIsAnError(t *testing.T) {
	h := `{"t_ns":1,"process":0,"type":"invoke","f":"read","key":"k/1","op_id":1}
{"t_ns":2,"process":0,"type":"invoke","f":"read","key":"k/2","op_id":1}
`
	if _, err := ExtractOps(strings.NewReader(h), ExtractOptions{}); err == nil {
		t.Fatal("a duplicate invoke was accepted")
	}
}

// TestAWitnessOpWithNoOkRecordIsStillPinned is OQ-034 arriving in the pipeline.
//
// The linearizability checker's witness is a POINTER built from the deepest
// partial linearization, and it may name a superseding write whose interval
// extends to infinity: an `info` record with no definite outcome. Such an
// operation is still evidence and must still be pinned; what must not happen is
// silently dropping it because it never returned.
func TestAWitnessOpWithNoOkRecordIsStillPinned(t *testing.T) {
	e := extractSample(t, ExtractOptions{Pinned: []int64{11430, 11580, 11999}})
	if diff := cmpInt64(e.MissingPinned, nil); diff != "" && len(e.MissingPinned) != 0 {
		t.Fatalf("MissingPinned = %v, want none", e.MissingPinned)
	}
	if diff := cmpInt64(e.IndeterminatePinned, []int64{11580, 11999}); diff != "" {
		t.Fatalf("IndeterminatePinned: %s "+
			"(11580 completed `info`; 11999 has no completion record at all)", diff)
	}
	p, err := e.Plan(nil)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if diff := cmpInt64(p.PinnedOpIDs(), []int64{11430, 11580, 11999}); diff != "" {
		t.Fatalf("pinned in the plan: %s", diff)
	}
	if diff := cmpInt64(p.RemovableOpIDs(), []int64{11402}); diff != "" {
		t.Fatalf("removable: %s", diff)
	}
	if e.Outcome(11580) != "info" {
		t.Errorf("Outcome(11580) = %q, want info", e.Outcome(11580))
	}
	if e.Outcome(11999) != "" {
		t.Errorf("Outcome(11999) = %q, want \"\" (no completion record)", e.Outcome(11999))
	}
}

// TestAWitnessOpTheHistoryDoesNotContainRefusesToBuildAPlan.
//
// A trace missing its own evidence cannot answer "is this the same violation",
// and a minimizer that proceeds anyway produces a minimal repro for a bug nobody
// asked about. D-021 already names the correct outcome: operation shrinking is
// reported NOT ATTEMPTED.
func TestAWitnessOpTheHistoryDoesNotContainRefusesToBuildAPlan(t *testing.T) {
	e := extractSample(t, ExtractOptions{Pinned: []int64{11430, 999999}})
	if diff := cmpInt64(e.MissingPinned, []int64{999999}); diff != "" {
		t.Fatalf("MissingPinned: %s", diff)
	}
	if _, err := e.Plan(nil); err == nil {
		t.Fatal("a plan was built without the witness evidence it will be judged against")
	}

	allowed := extractSample(t, ExtractOptions{Pinned: []int64{11430, 999999}, AllowMissingPinned: true})
	p, err := allowed.Plan(nil)
	if err != nil {
		t.Fatalf("AllowMissingPinned did not permit the extraction: %v", err)
	}
	if diff := cmpInt64(p.PinnedOpIDs(), []int64{11430}); diff != "" {
		t.Fatalf("pins: %s", diff)
	}
}

// TestACompletionWithoutAnInvokeIsNotExtractable.
func TestACompletionWithoutAnInvokeIsNotExtractable(t *testing.T) {
	h := `{"t_ns":1,"process":0,"type":"ok","f":"read","key":"k/1","value":5,"op_id":77}
`
	e, err := ExtractOps(strings.NewReader(h), ExtractOptions{Pinned: []int64{77}})
	if err != nil {
		t.Fatalf("ExtractOps: %v", err)
	}
	if len(e.Ops) != 0 {
		t.Fatalf("a completion-only operation was reconstructed as a request: %+v", e.Ops)
	}
	if e.CompletionsWithoutInvoke != 1 {
		t.Errorf("CompletionsWithoutInvoke = %d, want 1", e.CompletionsWithoutInvoke)
	}
	if diff := cmpInt64(e.MissingPinned, []int64{77}); diff != "" {
		t.Fatalf("MissingPinned: %s", diff)
	}
}

// TestThePlanInheritsTheProfilePlansIdentity.
func TestThePlanInheritsTheProfilePlansIdentity(t *testing.T) {
	base := &Plan{
		Schema: PlanSchema, Profile: "linear", Seed: 424242,
		HistoryPath: "/h.jsonl", Clients: 16, Ops: 60000,
		Mix: []MixEntry{{Op: "read", WeightPPM: 500000}},
	}
	e := extractSample(t, ExtractOptions{})
	p, err := e.Plan(base)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if p.Profile != "linear" || p.Seed != 424242 || p.HistoryPath != "/h.jsonl" ||
		p.Clients != 16 || p.Ops != 60000 || len(p.Mix) != 1 {
		t.Errorf("the profile identity did not survive extraction: %+v", p)
	}
	if base.HasOperations() {
		t.Error("Plan mutated its base")
	}
	if _, err := p.Marshal(); err != nil {
		t.Fatalf("the extracted plan does not encode: %v", err)
	}
}

// ------------------------------------------------------------ VerifyReplay

func writeHistory(t *testing.T, lines string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "history.jsonl")
	if err := os.WriteFile(path, []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestVerifyReplayAcceptsAnHonouredPlan.
func TestVerifyReplayAcceptsAnHonouredPlan(t *testing.T) {
	e := extractSample(t, ExtractOptions{Pinned: []int64{11430}})
	p, err := e.Plan(nil)
	if err != nil {
		t.Fatal(err)
	}
	c, err := VerifyReplay(p, writeHistory(t, sampleHistory))
	if err != nil {
		t.Fatalf("VerifyReplay: %v", err)
	}
	if !c.Honoured || !c.Checkable {
		t.Fatalf("an identical replay was not accepted: %+v (%s)", c, c.Reason())
	}
	if c.Matched != 4 || len(c.Missing) != 0 || len(c.Extra) != 0 {
		t.Errorf("check = %+v", c)
	}
}

// TestVerifyReplayDetectsADriverThatIgnoredThePlan.
//
// This is what turns D-021's "op shrinking is reported as NOT ATTEMPTED" into an
// OBSERVATION. A driver that ignored {plan_path} produces a perfectly
// well-formed history of a completely different workload, and the only evidence
// available from outside is whether the op ids match.
func TestVerifyReplayDetectsADriverThatIgnoredThePlan(t *testing.T) {
	e := extractSample(t, ExtractOptions{})
	p, err := e.Plan(nil)
	if err != nil {
		t.Fatal(err)
	}
	own := `{"t_ns":1000000000,"type":"info","event":"phase","phase":"DRIVE"}
{"t_ns":1100000000,"process":0,"type":"invoke","f":"read","key":"k/1","op_id":1}
{"t_ns":1200000000,"process":0,"type":"ok","f":"read","key":"k/1","value":1,"op_id":1}
`
	c, err := VerifyReplay(p, writeHistory(t, own))
	if err != nil {
		t.Fatalf("VerifyReplay: %v", err)
	}
	if c.Honoured {
		t.Fatal("a driver that generated its own workload was reported as honouring the plan")
	}
	if len(c.Extra) != 1 || c.Extra[0] != 1 {
		t.Errorf("Extra = %v, want [1]", c.Extra)
	}
	if !strings.Contains(c.Reason(), "NOT ATTEMPTED") && !strings.Contains(c.Reason(), "own workload") {
		t.Errorf("Reason does not explain the finding: %s", c.Reason())
	}
}

// TestVerifyReplayTreatsATruncatedReplayAsHonoured.
//
// QUIESCE stops the driver (D-042), so a trace whose tail was never reached is
// the normal outcome of a world whose budget ran out, not evidence that the
// driver ignored the plan.
func TestVerifyReplayTreatsATruncatedReplayAsHonoured(t *testing.T) {
	e := extractSample(t, ExtractOptions{})
	p, err := e.Plan(nil)
	if err != nil {
		t.Fatal(err)
	}
	short := `{"t_ns":1000000000,"type":"info","event":"phase","phase":"DRIVE"}
{"t_ns":1100000000,"process":3,"type":"invoke","f":"write","key":"k/0","value":3000012,"op_id":11402}
{"t_ns":1120000000,"process":3,"type":"ok","f":"write","key":"k/0","value":3000012,"op_id":11402}
`
	c, err := VerifyReplay(p, writeHistory(t, short))
	if err != nil {
		t.Fatalf("VerifyReplay: %v", err)
	}
	if !c.Honoured {
		t.Fatalf("a truncated but faithful replay was rejected: %s", c.Reason())
	}
	if len(c.Missing) != 3 {
		t.Errorf("Missing = %v, want 3 entries", c.Missing)
	}
}

// TestVerifyReplayReportsAMissingPinnedOpAsUncheckable.
//
// Neither "still reproduces" nor "no longer reproduces": the evidence was never
// issued, so the candidate is an inconclusive execution. Counting it as a
// rejection would keep an operation that does not matter: the flaky-shrink
// failure mode, arriving from the ops stage instead of the fault stage.
func TestVerifyReplayReportsAMissingPinnedOpAsUncheckable(t *testing.T) {
	e := extractSample(t, ExtractOptions{Pinned: []int64{11999}})
	p, err := e.Plan(nil)
	if err != nil {
		t.Fatal(err)
	}
	short := `{"t_ns":1000000000,"type":"info","event":"phase","phase":"DRIVE"}
{"t_ns":1100000000,"process":3,"type":"invoke","f":"write","key":"k/0","value":3000012,"op_id":11402}
`
	c, err := VerifyReplay(p, writeHistory(t, short))
	if err != nil {
		t.Fatalf("VerifyReplay: %v", err)
	}
	if !c.Honoured {
		t.Fatalf("expected honoured: %s", c.Reason())
	}
	if c.Checkable {
		t.Fatal("a replay that never issued the witness operation was reported as checkable")
	}
	if diff := cmpInt64(c.PinnedMissing, []int64{11999}); diff != "" {
		t.Fatalf("PinnedMissing: %s", diff)
	}
	if !strings.Contains(c.Reason(), "UNCHECKABLE") {
		t.Errorf("Reason does not say the candidate is uncheckable: %s", c.Reason())
	}
}

// TestVerifyReplayDetectsPerProcessReordering.
//
// A logical process is a sequential thread with one operation in flight;
// reordering it changes the concurrency structure and therefore which histories
// are linearizable.
func TestVerifyReplayDetectsPerProcessReordering(t *testing.T) {
	e := extractSample(t, ExtractOptions{})
	p, err := e.Plan(nil)
	if err != nil {
		t.Fatal(err)
	}
	// Process 7 issues 11999 before 11430, the reverse of the plan.
	swapped := `{"t_ns":1000000000,"type":"info","event":"phase","phase":"DRIVE"}
{"t_ns":1100000000,"process":7,"type":"invoke","f":"read","key":"k/0","op_id":11999}
{"t_ns":1200000000,"process":7,"type":"invoke","f":"read","key":"k/0","op_id":11430}
`
	c, err := VerifyReplay(p, writeHistory(t, swapped))
	if err != nil {
		t.Fatalf("VerifyReplay: %v", err)
	}
	if c.Honoured {
		t.Fatal("out-of-order execution within one process was accepted")
	}
	if diff := cmpInts(c.OutOfOrder, []int{7}); diff != "" {
		t.Fatalf("OutOfOrder: %s", diff)
	}
}

// TestVerifyReplayRefusesAProfilePlan.
func TestVerifyReplayRefusesAProfilePlan(t *testing.T) {
	p := &Plan{Schema: PlanSchema, Profile: "gate", Mix: []MixEntry{}}
	if _, err := VerifyReplay(p, writeHistory(t, sampleHistory)); err == nil {
		t.Fatal("VerifyReplay accepted a plan with no trace to verify against")
	}
}

func planOpIDs(ops []PlanOp) []int64 {
	out := make([]int64, len(ops))
	for i, op := range ops {
		out[i] = op.OpID
	}
	return out
}
