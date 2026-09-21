package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/WrdCstlg/pro-thesis/internal/shrink"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

func TestParseStrictness(t *testing.T) {
	cases := []struct {
		input string
		want  shrink.Strictness
		err   bool
	}{
		{"class", shrink.MatchOracleClass, false},
		{"oracle_class", shrink.MatchOracleClass, false},
		{"key", shrink.MatchWitnessKey, false},
		{"witness_key", shrink.MatchWitnessKey, false},
		{"ops", shrink.MatchWitnessOps, false},
		{"witness_ops", shrink.MatchWitnessOps, false},
		{"CLASS", shrink.MatchOracleClass, false},
		{"KEY", shrink.MatchWitnessKey, false},
		{"invalid", shrink.MatchUnset, true},
		{"", shrink.MatchUnset, true},
	}

	for _, tc := range cases {
		got, err := parseStrictness(tc.input)
		if tc.err && err == nil {
			t.Errorf("parseStrictness(%q) succeeded, want error", tc.input)
		}
		if !tc.err && err != nil {
			t.Errorf("parseStrictness(%q) failed: %v", tc.input, err)
		}
		if !tc.err && got != tc.want {
			t.Errorf("parseStrictness(%q) = %v, want %v", tc.input, got, tc.want)
		}
	}
}

func TestResolveOriginalViolationFromVerdict(t *testing.T) {
	dir := t.TempDir()
	runDir := filepath.Join(dir, "r_test")
	worldDir := filepath.Join(runDir, "world-0001")
	if err := os.MkdirAll(worldDir, 0o755); err != nil {
		t.Fatal(err)
	}

	worldPath := filepath.Join(worldDir, "world.thesis")
	if err := os.WriteFile(worldPath, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	verdictDoc := schema.Verdict{
		Schema:  schema.VerdictSchema,
		RunID:   "r_test",
		Verdict: schema.VerdictFail,
		Violations: []schema.Violation{
			{
				ID:       "v1",
				Oracle:   "linearizable.kv",
				Class:    schema.ClassConsistency,
				Severity: schema.SeverityHigh,
				Witness: schema.Witness{
					Key:   "k/42",
					OpIDs: []int64{101, 102},
				},
			},
		},
	}
	vBytes, _ := json.Marshal(verdictDoc)
	if err := os.WriteFile(filepath.Join(runDir, "verdict.json"), vBytes, 0o644); err != nil {
		t.Fatal(err)
	}

	id, err := resolveOriginalViolation(worldPath, "")
	if err != nil {
		t.Fatalf("resolveOriginalViolation: %v", err)
	}
	if id.Oracle != "linearizable.kv" {
		t.Errorf("Oracle = %q, want linearizable.kv", id.Oracle)
	}
	if id.Class != schema.ClassConsistency {
		t.Errorf("Class = %q, want consistency", id.Class)
	}
	if id.Key != "k/42" {
		t.Errorf("Key = %q, want k/42", id.Key)
	}
	if len(id.OpIDs) != 2 || id.OpIDs[0] != 101 || id.OpIDs[1] != 102 {
		t.Errorf("OpIDs = %v, want [101, 102]", id.OpIDs)
	}
}

func TestResolveOriginalViolationFallbackExpect(t *testing.T) {
	dir := t.TempDir()
	worldPath := filepath.Join(dir, "world.thesis")
	if err := os.WriteFile(worldPath, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	id, err := resolveOriginalViolation(worldPath, "no_crash")
	if err != nil {
		t.Fatalf("resolveOriginalViolation fallback: %v", err)
	}
	if id.Oracle != "no_crash" || id.Class != schema.ClassCrash {
		t.Errorf("unexpected identity: %+v", id)
	}

	_, err = resolveOriginalViolation(worldPath, "")
	if err == nil {
		t.Fatal("resolveOriginalViolation without expect or artifacts succeeded, want error")
	}
}

func TestCmdShrinkArgumentRefusals(t *testing.T) {
	ctx := context.Background()
	g := globals{}

	// No world argument -> ExitConfigError
	if code := cmdShrink(ctx, g, nil); code != schema.ExitConfigError {
		t.Errorf("cmdShrink(nil) = %v, want ExitConfigError (5)", code)
	}

	// Help flag -> ExitPass
	if code := cmdShrink(ctx, g, []string{"--help"}); code != schema.ExitPass {
		t.Errorf("cmdShrink(--help) = %v, want ExitPass (0)", code)
	}

	// Unknown flag -> ExitConfigError
	if code := cmdShrink(ctx, g, []string{"w_foo.thesis", "--unknown"}); code != schema.ExitConfigError {
		t.Errorf("cmdShrink(--unknown) = %v, want ExitConfigError (5)", code)
	}

	// Invalid confirm-k -> ExitConfigError
	if code := cmdShrink(ctx, g, []string{"w_foo.thesis", "-k", "0"}); code != schema.ExitConfigError {
		t.Errorf("cmdShrink(-k 0) = %v, want ExitConfigError (5)", code)
	}

	// Invalid budget -> ExitConfigError
	if code := cmdShrink(ctx, g, []string{"w_foo.thesis", "--budget", "bad"}); code != schema.ExitConfigError {
		t.Errorf("cmdShrink(--budget bad) = %v, want ExitConfigError (5)", code)
	}
}

// Stage 2 shortens the driver's operation plan and watches whether the violation
// survives. That inference is only valid if the driver actually EXECUTED the
// plan: and a driver that ignores it makes every candidate reproduce, so ddmin
// reduces in a straight line and reports a minimal repro naming operations that
// were never the reason for anything.
//
// MEASURED, not hypothetical: run r_2026_09_09_2904 handed the fixture's loadgen
// a plan of 1 operation, loadgen ignored it (its plan mode is a doc comment and
// an unread struct field; OQ-052), the history recorded 6,626 invocations, and
// the shrink reported "operations 6761 -> 1". The confirmation gate caught it at
// 0/3, which is the gate doing its job over a layer beneath it that had failed.
func TestAnIgnoredOperationPlanIsCaughtRatherThanBelieved(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, lines ...string) string {
		p := filepath.Join(dir, name)
		body := ""
		for _, l := range lines {
			body += l + "\n"
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}

	// A driver that ignored a 2-op plan and drove four operations of its own.
	ignored := write("ignored.jsonl",
		`{"t_ns":1,"type":"info","event":"phase","phase":"DRIVE"}`,
		`{"t_ns":2,"type":"invoke","op_id":1}`,
		`{"t_ns":3,"type":"ok","op_id":1}`,
		`{"t_ns":4,"type":"invoke","op_id":2}`,
		`{"t_ns":5,"type":"ok","op_id":2}`,
		`{"t_ns":6,"type":"invoke","op_id":3}`,
		`{"t_ns":7,"type":"fail","op_id":3}`,
		`{"t_ns":8,"type":"invoke","op_id":4}`,
	)
	err := planWasHonoured(ignored, 2)
	if err == nil {
		t.Fatal("a history with 4 invocations passed a 2-operation plan; stage 2 would go on to " +
			"report a reduction it never actually tested")
	}
	for _, want := range []string{"ignored its operation plan", "--skip-ops", "plan_path"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q, so a user is told something is wrong and "+
				"not what to do: %v", want, err)
		}
	}

	// Honoured exactly.
	if err := planWasHonoured(ignored, 4); err != nil {
		t.Errorf("a history with exactly as many invocations as planned was rejected: %v", err)
	}

	// FEWER invocations than planned is not disobedience. The driver is stopped
	// at QUIESCE, so a world that ends before the plan runs out is the normal
	// case: treating it as a violation would refuse every short world.
	if err := planWasHonoured(ignored, 10); err != nil {
		t.Errorf("a world that retired fewer operations than planned was treated as ignoring "+
			"its plan; the driver is stopped at QUIESCE and this is the normal case: %v", err)
	}

	// Completions, failures and phase markers must not be counted as invocations:
	// the plan bounds what may be INVOKED, and counting every record would
	// fire on a fully compliant driver.
	if err := planWasHonoured(ignored, 4); err != nil {
		t.Errorf("non-invoke records were counted against the plan: %v", err)
	}

	// A missing history is a different failure, already reported by the run.
	// Inventing a second complaint here would bury it.
	if err := planWasHonoured(filepath.Join(dir, "absent.jsonl"), 1); err != nil {
		t.Errorf("a missing history was reported as an ignored plan: %v", err)
	}

	// A corrupt line must not silently zero the count and turn the check vacuous.
	garbled := write("garbled.jsonl",
		`{"t_ns":1,"type":"invoke","op_id":1}`,
		`not json at all`,
		`{"t_ns":2,"type":"invoke","op_id":2}`,
		`{"t_ns":3,"type":"invoke","op_id":3}`,
	)
	if err := planWasHonoured(garbled, 1); err == nil {
		t.Error("unparseable lines made the check pass a history with 3 invocations against a " +
			"1-operation plan")
	}
}
