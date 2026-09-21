package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// goldenHistory is the committed witness history from the fixture's provebug
// run: a single key, a definite write of 7, a definite write of 9 that returns
// before the read is invoked, and a read of 7 served by the displaced leader
// under its stale lease.
const goldenHistory = "../../testdata/kvfixture/golden/history.jsonl"

// ---------------------------------------------------------------------------
// The definition of done: the fixture lease bug, flagged with witness op ids
// ---------------------------------------------------------------------------

func TestGoldenFixtureHistoryIsAConsistencyViolation(t *testing.T) {
	if _, err := os.Stat(goldenHistory); err != nil {
		t.Skipf("golden history not present: %v", err)
	}
	got := check(goldenHistory, testOptions(), time.Now())
	wantStatus(t, got, schema.StatusViolated)

	if got.Witness.Key != "k/42" {
		t.Fatalf("witness key = %q, want %q", got.Witness.Key, "k/42")
	}
	// op 90001 wrote 7; op 90002 committed 9 and returned before op 90003 was
	// invoked; op 90003 read 7 from the displaced leader.
	wantOpIDs(t, got, 90001, 90002, 90003)
	wantMentions(t, got,
		"no linearization exists",
		"WITNESSED failure, not a",
		"the register held 9",
		"written by op 90002",
		"returned 7",
		"written by op 90001",
	)

	// Every op id in the witness must exist in the history, so the witness can
	// actually be looked up by whoever reads the verdict.
	ld, err := loadHistory(goldenHistory, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatalf("load golden history: %v", err)
	}
	present := make(map[int64]bool, len(ld.Ops))
	for _, op := range ld.Ops {
		present[op.id] = true
	}
	for _, id := range got.Witness.OpIDs {
		if !present[id] {
			t.Fatalf("witness names op_id %d, which is not in the history", id)
		}
	}
	if ld.C.DetReads == 0 {
		t.Fatal("the golden history was read as having no determinate reads")
	}
}

// The golden history's own committed witness must agree with ours on the key
// and must contain every op id we report, so the two accounts of the same bug do
// not contradict each other.
func TestGoldenWitnessAgreesWithTheCommittedOne(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "kvfixture", "golden", "witness.json"))
	if err != nil {
		t.Skipf("committed witness not present: %v", err)
	}
	committed, err := schema.ParseOracleOutput(raw)
	if err != nil {
		t.Fatalf("parse committed witness: %v", err)
	}
	if committed.Oracle != oracleName || committed.Class != oracleClass {
		t.Fatalf("committed witness names oracle %q class %q; this oracle is %q/%q",
			committed.Oracle, committed.Class, oracleName, oracleClass)
	}

	got := check(goldenHistory, testOptions(), time.Now())
	wantStatus(t, got, schema.StatusViolated)
	if got.Witness.Key != committed.Witness.Key {
		t.Fatalf("witness key = %q, committed = %q", got.Witness.Key, committed.Witness.Key)
	}
	have := make(map[int64]bool, len(committed.Witness.OpIDs))
	for _, id := range committed.Witness.OpIDs {
		have[id] = true
	}
	for _, id := range got.Witness.OpIDs {
		if !have[id] {
			t.Fatalf("op_id %d is in our witness but not in the committed one %v",
				id, committed.Witness.OpIDs)
		}
	}
}

// ---------------------------------------------------------------------------
// The external oracle contract (directive 4.5)
// ---------------------------------------------------------------------------

func oracleInputFor(t *testing.T, historyPath string) []byte {
	t.Helper()
	in := schema.NewOracleInput(historyPath, "final_state.json", "telemetry.json", "world.thesis",
		schema.PhaseTimings{
			{Phase: schema.PhaseDrive, StartMS: 0, EndMS: 42000},
			{Phase: schema.PhaseHeal, StartMS: 42000, EndMS: 45000},
			{Phase: schema.PhaseQuiesce, StartMS: 45000, EndMS: 50000},
			{Phase: schema.PhaseAssert, StartMS: 50000, EndMS: 55000},
		})
	b, err := schema.MarshalOracleInput(&in)
	if err != nil {
		t.Fatalf("marshal oracle input: %v", err)
	}
	return b
}

// invoke runs the program end to end over stdin and returns the decoded output
// and the exit code.
func invoke(t *testing.T, stdin []byte, args ...string) (*schema.OracleOutput, schema.OracleExitCode, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(bytes.NewReader(stdin), &stdout, &stderr, args, time.Now())
	out, err := schema.ParseOracleOutput(stdout.Bytes())
	if err != nil {
		t.Fatalf("the oracle's stdout is not %s: %v\nstdout: %s\nstderr: %s",
			schema.OracleOutputSchema, err, stdout.String(), stderr.String())
	}
	return out, code, stderr.String()
}

func wantContract(t *testing.T, out *schema.OracleOutput) {
	t.Helper()
	if out.Schema != schema.OracleOutputSchema {
		t.Fatalf("schema = %q", out.Schema)
	}
	if out.Oracle != oracleName {
		t.Fatalf("oracle = %q, want %q", out.Oracle, oracleName)
	}
	if out.Class != schema.ClassConsistency {
		t.Fatalf("class = %q, want %q", out.Class, schema.ClassConsistency)
	}
	if len(out.ValidPhases) != 1 || out.ValidPhases[0] != schema.PhaseAssert {
		t.Fatalf("valid_phases = %v, want [ASSERT]", out.ValidPhases)
	}
	if err := out.Validate(); err != nil {
		t.Fatalf("the oracle emitted a document its own schema rejects: %v", err)
	}
}

// exitFor is the exit code the contract requires for a status.
func exitFor(s schema.OracleStatus) schema.OracleExitCode {
	switch s {
	case schema.StatusOK:
		return schema.OracleExitOK
	case schema.StatusViolated:
		return schema.OracleExitViolated
	default:
		return schema.OracleExitInconclusive
	}
}

func TestContractOnAViolatedHistory(t *testing.T) {
	path := writeHistory(t, cat(
		wOK(1, "k/42", 0, 1, "7"),
		wOK(2, "k/42", 2, 3, "9"),
		rOK(3, "k/42", 4, 5, "7"),
	))
	out, code, _ := invoke(t, oracleInputFor(t, path))
	wantContract(t, out)
	if out.Status != schema.StatusViolated {
		t.Fatalf("status = %q, want violated: %s", out.Status, out.Explanation)
	}
	if code != schema.OracleExitViolated {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if out.Witness.Key != "k/42" || len(out.Witness.OpIDs) == 0 {
		t.Fatalf("witness = %+v", out.Witness)
	}
}

func TestContractOnALinearizableHistory(t *testing.T) {
	path := writeHistory(t, cat(wOK(1, "k", 0, 1, "7"), rOK(2, "k", 2, 3, "7")))
	out, code, _ := invoke(t, oracleInputFor(t, path))
	wantContract(t, out)
	if out.Status != schema.StatusOK {
		t.Fatalf("status = %q, want ok: %s", out.Status, out.Explanation)
	}
	if code != schema.OracleExitOK {
		t.Fatalf("exit code = %d, want 0", code)
	}
}

func TestContractOnARefusal(t *testing.T) {
	path := writeHistory(t, nil)
	out, code, _ := invoke(t, oracleInputFor(t, path))
	wantContract(t, out)
	if out.Status != schema.StatusInconclusive {
		t.Fatalf("status = %q, want inconclusive", out.Status)
	}
	if code != schema.OracleExitInconclusive {
		t.Fatalf("exit code = %d, want 2", code)
	}
}

// The exit code and the reported status must agree. A disagreement is an oracle
// defect the engine has to treat as inconclusive, so it must never happen here.
func TestExitCodeAlwaysAgreesWithStatus(t *testing.T) {
	viol := writeHistory(t, cat(
		wOK(1, "k", 0, 1, "1"), wOK(2, "k", 2, 3, "2"), rOK(3, "k", 4, 5, "1")))
	fine := writeHistory(t, cat(wOK(1, "k", 0, 1, "1"), rOK(2, "k", 2, 3, "1")))
	empty := writeHistory(t, nil)

	cases := []struct {
		name  string
		stdin []byte
		args  []string
	}{
		{"violated", oracleInputFor(t, viol), nil},
		{"ok", oracleInputFor(t, fine), nil},
		{"empty history", oracleInputFor(t, empty), nil},
		{"missing history", oracleInputFor(t, filepath.Join(t.TempDir(), "nope.jsonl")), nil},
		{"garbage stdin", []byte("not json at all"), nil},
		{"empty stdin", nil, nil},
		{"wrong schema", []byte(`{"schema":"crucible.oracle_input/v1"}`), nil},
		{"bad flag", oracleInputFor(t, fine), []string{"-max-states", "0"}},
		{"stray argument", oracleInputFor(t, fine), []string{"/some/path"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, code, _ := invoke(t, tc.stdin, tc.args...)
			wantContract(t, out)
			if code != exitFor(out.Status) {
				t.Fatalf("status %q but exit code %d", out.Status, code)
			}
			// The engine reconciles the two channels fail-closed. If they agree,
			// reconciliation must be a no-op, which is the proof they agree.
			dec := schema.DecodeOracleOutput(mustMarshal(t, out), int(code))
			if dec.Status != out.Status {
				t.Fatalf("the engine reconciled %q to %q: %s", out.Status, dec.Status, dec.Explanation)
			}
			if code == schema.OracleExitOK && out.Status != schema.StatusOK {
				t.Fatalf("exited 0 without reporting ok (%q)", out.Status)
			}
		})
	}
}

func mustMarshal(t *testing.T, out *schema.OracleOutput) []byte {
	t.Helper()
	b, err := schema.MarshalOracleOutput(out)
	if err != nil {
		t.Fatalf("marshal output: %v", err)
	}
	return b
}

// A refusal must carry its reason. An INCONCLUSIVE with no explanation is
// unactionable: the exit-code contract says retry once then escalate to a human,
// and a human cannot escalate a verdict that does not say why.
func TestEveryRefusalExplainsItself(t *testing.T) {
	cases := [][]byte{
		nil,
		[]byte("garbage"),
		oracleInputFor(t, writeHistory(t, nil)),
		oracleInputFor(t, filepath.Join(t.TempDir(), "absent.jsonl")),
	}
	for i, stdin := range cases {
		out, _, _ := invoke(t, stdin)
		if out.Status != schema.StatusInconclusive {
			t.Fatalf("case %d: status = %q", i, out.Status)
		}
		if strings.TrimSpace(out.Explanation) == "" {
			t.Fatalf("case %d: an inconclusive finding with no explanation", i)
		}
	}
}

// The witness must survive a round trip through the frozen envelope, in exactly
// the shape the directive fixes: {"op_ids": [...], "key": "..."} and nothing
// invented alongside it.
func TestWitnessRoundTripsInTheFrozenShape(t *testing.T) {
	path := writeHistory(t, cat(
		wOK(1, "k/42", 0, 1, "7"), wOK(2, "k/42", 2, 3, "9"), rOK(3, "k/42", 4, 5, "7")))
	out, _, _ := invoke(t, oracleInputFor(t, path))
	b := mustMarshal(t, out)

	var doc struct {
		Witness map[string]json.RawMessage `json:"witness"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("decode emitted document: %v", err)
	}
	if len(doc.Witness) != 2 {
		t.Fatalf("witness has %d member(s) %v; the frozen shape is op_ids and key",
			len(doc.Witness), doc.Witness)
	}
	if _, ok := doc.Witness["op_ids"]; !ok {
		t.Fatal("witness has no op_ids")
	}
	if _, ok := doc.Witness["key"]; !ok {
		t.Fatal("witness has no key")
	}
}

// A violated finding converts into a verdict violation with the normative
// severity for its class.
func TestViolationConvertsIntoTheVerdictShape(t *testing.T) {
	path := writeHistory(t, cat(
		wOK(1, "k", 0, 1, "1"), wOK(2, "k", 2, 3, "2"), rOK(3, "k", 4, 5, "1")))
	out, _, _ := invoke(t, oracleInputFor(t, path))
	v := out.ToViolation("v1", schema.PhaseAssert, 14320)
	if v.Oracle != oracleName || v.Class != schema.ClassConsistency {
		t.Fatalf("violation = %+v", v)
	}
	if v.Severity != schema.SeverityHigh {
		t.Fatalf("severity = %q, want high", v.Severity)
	}
	if v.Witness.Key != "k" || len(v.Witness.OpIDs) == 0 {
		t.Fatalf("witness = %+v", v.Witness)
	}
}

// The environment carries the budget when a runner passes no arguments.
func TestBudgetComesFromTheEnvironment(t *testing.T) {
	t.Setenv(envMaxStates, "5")
	t.Setenv(envWall, "9s")
	t.Setenv(envMemoBytes, "1048576")
	opt, err := parseOptions(nil, os.Stderr)
	if err != nil {
		t.Fatalf("parseOptions: %v", err)
	}
	if opt.MaxStates != 5 || opt.Wall != 9*time.Second || opt.MaxMemoBytes != 1<<20 {
		t.Fatalf("options = %+v", opt)
	}
	// Flags win over the environment.
	opt, err = parseOptions([]string{"-max-states", "77"}, os.Stderr)
	if err != nil {
		t.Fatalf("parseOptions: %v", err)
	}
	if opt.MaxStates != 77 {
		t.Fatalf("max-states = %d, want 77", opt.MaxStates)
	}

	t.Setenv(envMaxStates, "not-a-number")
	if _, err := parseOptions(nil, os.Stderr); err == nil {
		t.Fatal("a malformed budget in the environment must be an error, not a silent default")
	}
}

// A budget so small the oracle cannot finish must reach the caller as exit 2.
func TestStarvedBudgetReachesTheCallerAsInconclusive(t *testing.T) {
	path := writeHistory(t, seq("k", 40))
	out, code, _ := invoke(t, oracleInputFor(t, path), "-max-states", "3")
	wantContract(t, out)
	if out.Status != schema.StatusInconclusive {
		t.Fatalf("status = %q, want inconclusive: %s", out.Status, out.Explanation)
	}
	if code != schema.OracleExitInconclusive {
		t.Fatalf("exit code = %d, want 2", code)
	}
}
