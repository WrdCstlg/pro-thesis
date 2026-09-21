package driver

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// updateGolden regenerates internal/driver/testdata/golden_plan.json.
//
// The golden exists to pin the WIRE FORMAT across a module boundary. The
// reference fixture is a separate Go module (prothesis.dev/kvfixture) and
// deliberately does not import this one: a driver is an external, unmodified
// process, and making the reference driver depend on the harness's own packages
// would prove nothing about a third-party one. So the format is pinned from both
// sides against the same bytes: this test asserts the ENCODER produces them, and
// testdata/kvfixture/internal/plan asserts its own DECODER reads them. Either
// side drifting fails one of the two.
var updateGolden = flag.Bool("update-golden", false, "rewrite testdata/golden_plan.json")

// goldenPlan is the plan the golden file encodes. It deliberately exercises
// every member shape: a lease read, a linearizable read, an integer-valued
// write, a transaction whose value is an opaque array, an operation with no key
// or value at all, a pinned witness op, and a gap in the op ids left by a
// removal.
func goldenPlan() *Plan {
	return &Plan{
		Schema:      PlanSchema,
		Profile:     "linear",
		Seed:        424242,
		HistoryPath: "/artifacts/history.jsonl",
		Clients:     16,
		Ops:         60000,
		Mix: []MixEntry{
			{Op: "read", WeightPPM: 500000},
			{Op: "write", WeightPPM: 500000},
		},
		Targets: []string{
			"http://127.0.0.1:18081",
			"http://127.0.0.1:18082",
			"http://127.0.0.1:18083",
		},
		Origin:   OriginDrive,
		OriginNS: 1788764420000000000,
		Operations: []PlanOp{
			{
				OpID: 11402, Process: 3, F: "write", Key: "k/0",
				Value: json.RawMessage(`3000012`), Target: "http://127.0.0.1:18082", AtMS: 2971,
			},
			{
				OpID: 11430, Process: 7, F: "read", Key: "k/0",
				ReadMode: "lease", Target: "http://127.0.0.1:18082", AtMS: 3004, Pinned: true,
			},
			{
				OpID: 11578, Process: 7, F: "read", Key: "k/0",
				ReadMode: "linearizable", Target: "http://127.0.0.1:18081", AtMS: 4180, Pinned: true,
			},
			{
				OpID: 11580, Process: 11, F: "txn",
				Value:  json.RawMessage(`[["r","k/3",null],["w","k/0",11000044]]`),
				Target: "http://127.0.0.1:18083", AtMS: 4188, Pinned: true,
			},
			{OpID: 11999, Process: 3, F: "admin", Target: "http://127.0.0.1:18081", AtMS: 4900},
		},
	}
}

const (
	goldenPath = "testdata/golden_plan.json"
	// fixtureGoldenPath is the SAME BYTES, carried in the reference fixture's
	// own module so its independent decoder can be pinned against them without
	// either module importing the other.
	fixtureGoldenPath = "../../testdata/kvfixture/internal/plan/testdata/golden_plan.json"
)

// TestOperationPlanGoldenFile pins the on-disk wire format.
func TestOperationPlanGoldenFile(t *testing.T) {
	got, err := goldenPlan().Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if *updateGolden {
		for _, p := range []string{goldenPath, fixtureGoldenPath} {
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, got, 0o644); err != nil {
				t.Fatal(err)
			}
			t.Logf("wrote %s (%d bytes)", p, len(got))
		}
		return
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden (regenerate with -update-golden): %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("the plan wire format moved. The reference fixture's own decoder is pinned "+
			"against these same bytes, so a change here without the matching change there "+
			"means the harness writes a plan the driver cannot read.\n got:\n%s\nwant:\n%s",
			got, want)
	}
	if bytes.Contains(want, []byte("\r")) {
		t.Error("the golden carries a CR byte: git has translated line endings. " +
			"Check .gitattributes (`*.json text eol=lf`) and `git config core.autocrlf`")
	}

	// The fixture's copy must be the same bytes. If the two drift, one module's
	// test passes while the other's fails (in a DIFFERENT test run, since they
	// are separate modules) and the format silently forks.
	mirror, err := os.ReadFile(fixtureGoldenPath)
	if err != nil {
		t.Fatalf("read the fixture's copy of the golden: %v", err)
	}
	if !bytes.Equal(mirror, want) {
		t.Errorf("%s has drifted from %s; the harness and the reference driver are pinned "+
			"against different bytes", fixtureGoldenPath, goldenPath)
	}
}

// TestAnOperationPlanRoundTripsByteIdentically is the format's central claim.
//
// A plan that does not survive write -> read -> write is a plan whose meaning
// depends on who last touched it, and ddmin derives thousands of candidates from
// one parent.
func TestAnOperationPlanRoundTripsByteIdentically(t *testing.T) {
	orig := goldenPlan()
	first, err := orig.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	back, err := ParsePlan(first)
	if err != nil {
		t.Fatalf("ParsePlan: %v", err)
	}
	second, err := back.Marshal()
	if err != nil {
		t.Fatalf("re-Marshal: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Fatalf("the plan did not round trip byte-identically\nfirst:\n%s\nsecond:\n%s", first, second)
	}
	if len(back.Operations) != len(orig.Operations) {
		t.Fatalf("operations: got %d, want %d", len(back.Operations), len(orig.Operations))
	}
	for i, want := range orig.Operations {
		got := back.Operations[i]
		if got.OpID != want.OpID || got.Process != want.Process || got.F != want.F ||
			got.Key != want.Key || got.ReadMode != want.ReadMode || got.Target != want.Target ||
			got.AtMS != want.AtMS || got.Pinned != want.Pinned {
			t.Errorf("operations[%d]:\n got %+v\nwant %+v", i, got, want)
		}
		if !jsonEqual(got.Value, want.Value) {
			t.Errorf("operations[%d].value: got %s, want %s", i, got.Value, want.Value)
		}
	}
}

// TestAlsoRoundTripsThroughDisk exercises the same claim through WritePlan,
// which is the path the harness actually uses.
func TestAlsoRoundTripsThroughDisk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "w1", PlanFileName)
	if err := WritePlan(path, goldenPlan()); err != nil {
		t.Fatalf("WritePlan: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	back, err := ParsePlan(data)
	if err != nil {
		t.Fatalf("ParsePlan: %v", err)
	}
	again, err := back.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, again) {
		t.Fatalf("disk round trip is not byte-identical\non disk:\n%s\nre-encoded:\n%s", data, again)
	}
}

// TestAProfilePlanIsUnchangedByTheAdditiveMembers.
//
// Every plan written since Phase 1 is a profile plan. If adding the operation
// members moved their bytes, every existing artifact and every existing test
// would have to be regenerated, and the format would not be additive, whatever
// the comment said.
func TestAProfilePlanIsUnchangedByTheAdditiveMembers(t *testing.T) {
	p := &Plan{
		Schema: PlanSchema, Profile: "gate", Seed: 7, HistoryPath: "/h.jsonl",
		Clients: 16, Ops: 20000,
		Mix: []MixEntry{{Op: "read", WeightPPM: 400000}},
	}
	b, err := p.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	for _, key := range []string{"operations", "targets", "origin", "origin_ns"} {
		if bytes.Contains(b, []byte(`"`+key+`"`)) {
			t.Errorf("a profile plan emitted %q; the Phase 5 members must be absent, not null:\n%s", key, b)
		}
	}
	if p.HasOperations() {
		t.Error("HasOperations is true on a profile plan; the driver would enter replay mode " +
			"with an empty trace and drive nothing")
	}
}

// TestHasOperationsIsTheModeDiscriminatorNotThePathsExistence.
//
// D-021 ALWAYS exports PROTHESIS_PLAN_PATH. A driver keying replay off the
// variable's existence would find a profile plan at that path on every run this
// project has ever done, read zero operations, and report on a system it never
// touched.
func TestHasOperationsIsTheModeDiscriminatorNotThePathsExistence(t *testing.T) {
	profile := &Plan{Schema: PlanSchema, Profile: "gate", Mix: []MixEntry{}}
	if profile.HasOperations() {
		t.Fatal("a profile plan claims to carry a trace")
	}
	if !goldenPlan().HasOperations() {
		t.Fatal("an operation plan does not claim to carry a trace")
	}
}

// TestValidateRejectsAnExplicitlyEmptyTrace.
func TestValidateRejectsAnExplicitlyEmptyTrace(t *testing.T) {
	data := []byte(`{"schema":"prothesis.driver_plan/v1","profile":"linear","seed":1,` +
		`"history_path":"/h","clients":1,"ops":1,"mix":[],"operations":[]}`)
	_, err := ParsePlan(data)
	if err == nil {
		t.Fatal("an empty operation trace was accepted; a driver handed one drives nothing " +
			"while still writing a history a checker calls clean")
	}
	if !strings.Contains(err.Error(), "EMPTY operation trace") {
		t.Errorf("error does not name the problem: %v", err)
	}
}

// TestValidateRejectsATraceThatIsNotAscending.
//
// Strictly ascending op ids are what make "ddmin removes operations, it does not
// renumber them" checkable, and they are the original per-process issue order.
func TestValidateRejectsATraceThatIsNotAscending(t *testing.T) {
	for _, tc := range []struct {
		name string
		ids  []int64
	}{
		{"descending", []int64{5, 4}},
		{"duplicate", []int64{5, 5}},
		{"zero", []int64{0}},
		{"negative", []int64{-1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &Plan{Schema: PlanSchema, Mix: []MixEntry{}}
			for _, id := range tc.ids {
				p.Operations = append(p.Operations, PlanOp{OpID: id, Process: 0, F: "read"})
			}
			if err := p.Validate(); err == nil {
				t.Fatalf("Validate accepted op ids %v", tc.ids)
			}
		})
	}
}

// TestValidateRejectsAnOpNamingATargetThePlanDoesNotCarry.
func TestValidateRejectsAnOpNamingATargetThePlanDoesNotCarry(t *testing.T) {
	p := &Plan{
		Schema: PlanSchema, Mix: []MixEntry{},
		Targets:    []string{"http://a"},
		Operations: []PlanOp{{OpID: 1, F: "read", Target: "http://b"}},
	}
	if err := p.Validate(); err == nil {
		t.Fatal("an op naming an unlisted target was accepted; the driver would have no " +
			"position to map it onto and would silently pick some other node")
	}
}

// TestValidateRejectsAnUnparseableValue.
func TestValidateRejectsAnUnparseableValue(t *testing.T) {
	p := &Plan{
		Schema: PlanSchema, Mix: []MixEntry{},
		Operations: []PlanOp{{OpID: 1, F: "write", Key: "k", Value: json.RawMessage(`{`)}},
	}
	if err := p.Validate(); err == nil {
		t.Fatal("a value that is not JSON was accepted")
	}
}

// TestSubsetPreservesOrderClientAssignmentAndOpIDs.
func TestSubsetPreservesOrderClientAssignmentAndOpIDs(t *testing.T) {
	parent := goldenPlan()
	// keep is deliberately out of order: the result must still be in the
	// parent's order, so the same set always gives the same candidate whatever
	// order ddmin generated it in.
	got, err := parent.Subset([]int64{11580, 11402, 11430, 11578})
	if err != nil {
		t.Fatalf("Subset: %v", err)
	}
	wantIDs := []int64{11402, 11430, 11578, 11580}
	if diff := cmpInt64(got.OpIDs(), wantIDs); diff != "" {
		t.Fatalf("op ids: %s", diff)
	}
	byID := map[int64]PlanOp{}
	for _, op := range parent.Operations {
		byID[op.OpID] = op
	}
	for _, op := range got.Operations {
		want := byID[op.OpID]
		if op.Process != want.Process {
			t.Errorf("op %d moved from process %d to %d; re-assigning operations between "+
				"processes changes the concurrency structure and therefore which histories "+
				"are linearizable", op.OpID, want.Process, op.Process)
		}
		if op.F != want.F || op.Key != want.Key || op.ReadMode != want.ReadMode ||
			op.Target != want.Target || op.AtMS != want.AtMS {
			t.Errorf("op %d changed:\n got %+v\nwant %+v", op.OpID, op, want)
		}
	}
	if len(parent.Operations) != 5 {
		t.Errorf("Subset mutated its parent: %d operations remain", len(parent.Operations))
	}
}

// TestSubsetRefusesToDropAPinnedOp is the RULE 1 guard in the data.
//
// A candidate without the witness cannot be checked against the original
// violation identity, so "did this reproduce the same bug" becomes unanswerable,
// and an unanswerable question gets an optimistic answer.
func TestSubsetRefusesToDropAPinnedOp(t *testing.T) {
	parent := goldenPlan()
	_, err := parent.Subset([]int64{11402, 11430, 11999})
	if err == nil {
		t.Fatal("Subset dropped a pinned witness op")
	}
	if !strings.Contains(err.Error(), "11578") || !strings.Contains(err.Error(), "11580") {
		t.Errorf("error does not name the dropped pins: %v", err)
	}
	if _, err := parent.Without(parent.PinnedOpIDs()); err == nil {
		t.Fatal("Without dropped the pinned ops")
	}
}

// TestSubsetRefusesAnUnknownOpID: a minimizer bug must not silently produce a
// smaller trace than it thought it had.
func TestSubsetRefusesAnUnknownOpID(t *testing.T) {
	if _, err := goldenPlan().Subset([]int64{11402, 999999}); err == nil {
		t.Fatal("Subset accepted an op id the parent does not contain")
	}
}

// TestSubsetRefusesAnEmptyResult.
func TestSubsetRefusesAnEmptyResult(t *testing.T) {
	p := goldenPlan()
	for i := range p.Operations {
		p.Operations[i].Pinned = false
	}
	if _, err := p.Subset(nil); err == nil {
		t.Fatal("Subset produced an empty trace; a driver handed no operations drives nothing")
	}
}

// TestSubsetKeepsTheParentsTargetListSoPositionsDoNotMove.
//
// Recomputing the distinct targets after a reduction would renumber the
// positions and silently re-point the surviving operations at different nodes.
// That is a shrink that changes the bug, arriving through a tidy-up.
func TestSubsetKeepsTheParentsTargetListSoPositionsDoNotMove(t *testing.T) {
	parent := goldenPlan()
	// Every surviving op addresses 18082 only.
	got, err := parent.Subset([]int64{11402, 11430, 11578, 11580})
	if err != nil {
		t.Fatalf("Subset: %v", err)
	}
	if diff := cmpStrings(got.Targets, parent.Targets); diff != "" {
		t.Fatalf("the target list moved under subsetting: %s", diff)
	}
}

// TestWithoutIsSubsetsComplement.
func TestWithoutIsSubsetsComplement(t *testing.T) {
	parent := goldenPlan()
	got, err := parent.Without([]int64{11999})
	if err != nil {
		t.Fatalf("Without: %v", err)
	}
	if diff := cmpInt64(got.OpIDs(), []int64{11402, 11430, 11578, 11580}); diff != "" {
		t.Fatalf("%s", diff)
	}
}

// TestRemovableOpIDsExcludesThePins.
func TestRemovableOpIDsExcludesThePins(t *testing.T) {
	p := goldenPlan()
	if diff := cmpInt64(p.RemovableOpIDs(), []int64{11402, 11999}); diff != "" {
		t.Fatalf("removable: %s", diff)
	}
	if diff := cmpInt64(p.PinnedOpIDs(), []int64{11430, 11578, 11580}); diff != "" {
		t.Fatalf("pinned: %s", diff)
	}
}

// TestPinRefusesAnOpTheTraceDoesNotContain.
func TestPinRefusesAnOpTheTraceDoesNotContain(t *testing.T) {
	p := goldenPlan()
	err := p.Pin([]int64{11402, 424242})
	if err == nil {
		t.Fatal("Pin silently ignored an op id the trace does not contain")
	}
	if !strings.Contains(err.Error(), "424242") {
		t.Errorf("error does not name the missing op: %v", err)
	}
}

// TestCloneDoesNotShareValueBytes: a candidate must never be able to mutate the
// baseline the whole shrink is judged against.
func TestCloneDoesNotShareValueBytes(t *testing.T) {
	parent := goldenPlan()
	clone := parent.Clone()
	clone.Operations[0].Value[0] = 'X'
	clone.Targets[0] = "http://changed"
	clone.Operations[0].Process = 99
	if string(parent.Operations[0].Value) != "3000012" {
		t.Errorf("the clone shares its value bytes with the parent: %s", parent.Operations[0].Value)
	}
	if parent.Targets[0] != "http://127.0.0.1:18081" {
		t.Errorf("the clone shares its target slice with the parent: %v", parent.Targets)
	}
	if parent.Operations[0].Process != 3 {
		t.Errorf("the clone shares its operation slice with the parent")
	}
}

// TestFingerprintTracksTheTrace, and is not a world hash.
func TestFingerprintTracksTheTrace(t *testing.T) {
	a, err := goldenPlan().Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	same, err := goldenPlan().Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	if a != same {
		t.Fatalf("the same plan fingerprinted differently:\n%s\n%s", a, same)
	}
	reduced, err := goldenPlan().Without([]int64{11999})
	if err != nil {
		t.Fatal(err)
	}
	b, err := reduced.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("a reduced trace fingerprinted the same as its parent, so a minimizer " +
			"de-duplicating on this would skip real candidates")
	}
	if !strings.HasPrefix(a, "sha256:") {
		t.Errorf("fingerprint is not sha256-prefixed: %s", a)
	}
}

// TestTheOperationTraceDeclaresNoFloat.
//
// The same rule cjson enforces for world files, applied here at the type level:
// Go's shortest-float representation carries no cross-version stability
// guarantee, so a bare float would make a plan's bytes a function of the
// toolchain that wrote them, and a minimizer de-duplicating candidates by
// Fingerprint would then see two different plans where there is one.
//
// The opaque `value` member is exempt BY TYPE: it is raw bytes moved verbatim
// from the driver's own history, and this package never decodes or re-encodes
// it, so no rounding can happen here whatever the driver put in it.
func TestTheOperationTraceDeclaresNoFloat(t *testing.T) {
	rt := reflect.TypeOf(PlanOp{})
	for i := 0; i < rt.NumField(); i++ {
		switch rt.Field(i).Type.Kind() {
		case reflect.Float32, reflect.Float64:
			t.Errorf("PlanOp.%s is a float; ratios are carried as integers", rt.Field(i).Name)
		}
	}
	for _, name := range []string{"Operations", "Targets", "Origin", "OriginNS"} {
		f, ok := reflect.TypeOf(Plan{}).FieldByName(name)
		if !ok {
			t.Fatalf("Plan has no field %s", name)
		}
		k := f.Type.Kind()
		if k == reflect.Float32 || k == reflect.Float64 {
			t.Errorf("Plan.%s is a float", name)
		}
	}
}

// TestSpanMSAndProcesses report what a minimizer needs to reason about cost.
func TestSpanMSAndProcesses(t *testing.T) {
	p := goldenPlan()
	if got := p.SpanMS(); got != 4900 {
		t.Errorf("SpanMS = %d, want 4900", got)
	}
	if diff := cmpInts(p.Processes(), []int{3, 7, 11}); diff != "" {
		t.Fatalf("processes: %s", diff)
	}
	reduced, err := p.Subset([]int64{11430, 11578, 11580})
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmpInts(reduced.Processes(), []int{7, 11}); diff != "" {
		t.Fatalf("after reduction: %s", diff)
	}
}

// ---------------------------------------------------------------- helpers

func jsonEqual(a, b json.RawMessage) bool {
	if len(a) == 0 || len(b) == 0 {
		return len(a) == len(b)
	}
	var ca, cb bytes.Buffer
	if err := json.Compact(&ca, a); err != nil {
		return false
	}
	if err := json.Compact(&cb, b); err != nil {
		return false
	}
	return ca.String() == cb.String()
}

func cmpInt64(got, want []int64) string {
	if len(got) != len(want) {
		return fmt.Sprintf("got %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			return fmt.Sprintf("got %v, want %v", got, want)
		}
	}
	return ""
}

func cmpInts(got, want []int) string {
	if len(got) != len(want) {
		return fmt.Sprintf("got %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			return fmt.Sprintf("got %v, want %v", got, want)
		}
	}
	return ""
}

func cmpStrings(got, want []string) string {
	if len(got) != len(want) {
		return fmt.Sprintf("got %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			return fmt.Sprintf("got %v, want %v", got, want)
		}
	}
	return ""
}
