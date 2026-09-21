package schema

import (
	"bytes"
	"strings"
	"testing"
)

// TestGoldenOracleInput pins directive 4.5's stdin document.
func TestGoldenOracleInput(t *testing.T) {
	raw := readTestdata(t, "oracle_input.golden.json")
	in, err := UnmarshalOracleInput(raw)
	if err != nil {
		t.Fatalf("UnmarshalOracleInput: %v", err)
	}
	if in.HistoryPath != "/path/to/history.jsonl" ||
		in.FinalStatePath != "/path/to/final_state.json" ||
		in.TelemetryPath != "/path/to/telemetry.json" ||
		in.WorldPath != "/path/to/world.thesis" {
		t.Errorf("oracle input paths = %+v", *in)
	}
	if len(in.Phases) != 4 {
		t.Fatalf("phases = %d, want 4", len(in.Phases))
	}
	drive, ok := in.Phases.Lookup(PhaseDrive)
	if !ok || drive.StartMS != 0 || drive.EndMS != 42000 {
		t.Errorf("DRIVE window = %+v", drive)
	}
	if err := in.Validate(); err != nil {
		t.Errorf("the directive's own oracle input does not validate: %v", err)
	}

	out, err := MarshalOracleInput(in)
	if err != nil {
		t.Fatalf("MarshalOracleInput: %v", err)
	}
	gotCanon, err := Canonicalize(out)
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}
	wantCanon, err := Canonicalize(raw)
	if err != nil {
		t.Fatalf("canonicalize golden: %v", err)
	}
	if !bytes.Equal(gotCanon, wantCanon) {
		t.Errorf("oracle input re-emission differs.\n got: %s\nwant: %s", gotCanon, wantCanon)
	}

	// Phase lookup is what makes invariant I5 evaluable, so exercise it.
	if got := in.Phases.At(43000); len(got) != 1 || got[0] != PhaseHeal {
		t.Errorf("At(43000) = %v, want [HEAL]", got)
	}
	if got := in.Phases.At(60000); len(got) != 0 {
		t.Errorf("At(60000) = %v, want none", got)
	}
}

// TestGoldenOracleOutput pins directive 4.5's stdout document.
func TestGoldenOracleOutput(t *testing.T) {
	raw := readTestdata(t, "oracle_output.golden.json")
	out, err := ParseOracleOutput(raw)
	if err != nil {
		t.Fatalf("ParseOracleOutput: %v", err)
	}
	if out.Oracle != "linearizable.kv" || out.Class != ClassConsistency || out.Status != StatusViolated {
		t.Errorf("oracle output = %+v", *out)
	}
	if len(out.ValidPhases) != 1 || out.ValidPhases[0] != PhaseAssert {
		t.Errorf("valid_phases = %v, want [ASSERT]", out.ValidPhases)
	}
	if !out.ValidIn(PhaseAssert) || out.ValidIn(PhaseDrive) {
		t.Error("ValidIn disagrees with valid_phases")
	}
	if len(out.Witness.OpIDs) != 2 || out.Witness.OpIDs[1] != 8903 || out.Witness.Key != "k/42" {
		t.Errorf("witness = %+v", out.Witness)
	}
	if out.Explanation != "process 5 read stale value under lease violation" {
		t.Errorf("explanation = %q", out.Explanation)
	}
	if err := out.Validate(); err != nil {
		t.Errorf("the directive's own oracle output does not validate: %v", err)
	}

	emitted, err := MarshalOracleOutput(out)
	if err != nil {
		t.Fatalf("MarshalOracleOutput: %v", err)
	}
	gotCanon, _ := Canonicalize(emitted)
	wantCanon, _ := Canonicalize(raw)
	if !bytes.Equal(gotCanon, wantCanon) {
		t.Errorf("oracle output re-emission differs.\n got: %s\nwant: %s", gotCanon, wantCanon)
	}
}

// TestOracleStatusReconciliationIsFailClosed is the invariant I6 test: given an
// oracle's two independent result channels, PRO-THESIS never adopts the weaker
// reading of a disagreement.
func TestOracleStatusReconciliationIsFailClosed(t *testing.T) {
	body := func(status string) []byte {
		return []byte(`{"schema":"prothesis.oracle_output/v1","oracle":"x","class":"crash",` +
			`"valid_phases":["ASSERT"],"status":"` + status + `","explanation":"e"}`)
	}

	cases := []struct {
		name   string
		stdout []byte
		exit   int
		want   OracleStatus
	}{
		{"agree ok", body("ok"), 0, StatusOK},
		{"agree violated", body("violated"), 1, StatusViolated},
		{"agree inconclusive", body("inconclusive"), 2, StatusInconclusive},

		{"prints ok, exits violated", body("ok"), 1, StatusViolated},
		{"prints violated, exits ok", body("violated"), 0, StatusViolated},
		{"prints ok, exits inconclusive", body("ok"), 2, StatusInconclusive},
		{"prints inconclusive, exits ok", body("inconclusive"), 0, StatusInconclusive},
		{"prints violated, exits inconclusive", body("violated"), 2, StatusViolated},

		{"empty stdout, exits ok", nil, 0, StatusInconclusive},
		{"garbage stdout, exits ok", []byte("boom\n"), 0, StatusInconclusive},
		{"wrong schema, exits ok", []byte(`{"schema":"crucible.oracle_output/v1"}`), 0, StatusInconclusive},
		{"malformed witness, exits ok",
			[]byte(`{"schema":"prothesis.oracle_output/v1","status":"ok","witness":{"op_ids":["a"]}}`),
			0, StatusInconclusive},
		{"unknown status, exits ok",
			[]byte(`{"schema":"prothesis.oracle_output/v1","status":"probably_fine"}`),
			0, StatusInconclusive},
		{"no status at all, exits ok",
			[]byte(`{"schema":"prothesis.oracle_output/v1","oracle":"x"}`), 0, StatusInconclusive},

		{"crashed with 137", body("ok"), 137, StatusInconclusive},
		{"exits 3", body("ok"), 3, StatusInconclusive},
	}
	for _, c := range cases {
		got := DecodeOracleOutput(c.stdout, c.exit)
		if got.Status != c.want {
			t.Errorf("%s: status = %q, want %q (explanation: %s)", c.name, got.Status, c.want, got.Explanation)
		}
		if got.Schema != OracleOutputSchema {
			t.Errorf("%s: schema = %q", c.name, got.Schema)
		}
		if got.ValidPhases == nil {
			t.Errorf("%s: valid_phases is null, want []", c.name)
		}
	}

	// A disagreement must leave a trace, or the reason vanishes from the run.
	got := DecodeOracleOutput(body("ok"), 1)
	if !strings.Contains(got.Explanation, "disagrees") {
		t.Errorf("disagreement not recorded: %q", got.Explanation)
	}
}

func TestOracleExitCodeSpace(t *testing.T) {
	if OracleExitOK.Status() != StatusOK ||
		OracleExitViolated.Status() != StatusViolated ||
		OracleExitInconclusive.Status() != StatusInconclusive {
		t.Error("oracle exit code mapping is wrong")
	}
	if OracleExitCode(7).Valid() {
		t.Error("7 is not an oracle exit code")
	}
	if OracleExitCode(7).Status() != StatusInconclusive {
		t.Error("an out-of-contract exit code must not map to ok")
	}
	// The two exit spaces overlap numerically but not semantically: 1 means
	// VIOLATED for an oracle process and FAIL for the CLI, and 2 means
	// INCONCLUSIVE in both but for different reasons. The types are distinct so
	// the compiler stops a caller conflating them.
	if OracleExitInconclusive.Status() == StatusOK {
		t.Error("oracle exit 2 must never read as ok")
	}
}

// TestToViolationAssignsSeverityFromClass: the oracle output schema has no
// severity field, so severity is the engine's to assign and an oracle can never
// grade its own finding down.
func TestToViolationAssignsSeverityFromClass(t *testing.T) {
	out := &OracleOutput{
		Schema: OracleOutputSchema, Oracle: "linearizable.kv", Class: ClassConsistency,
		Status: StatusViolated, Explanation: "stale read",
		Witness: Witness{OpIDs: []int64{8891, 8903}, Key: "k/42"},
	}
	v := out.ToViolation("v1", PhaseDrive, 14320)
	if v.Severity != SeverityHigh {
		t.Errorf("severity = %q, want high (directive 4.6's only worked example)", v.Severity)
	}
	if v.ID != "v1" || v.Oracle != "linearizable.kv" || v.Class != ClassConsistency {
		t.Errorf("violation = %+v", v)
	}
	if v.Phase != PhaseDrive || v.FirstSeenMS != 14320 {
		t.Errorf("violation phase/first_seen = %q/%d", v.Phase, v.FirstSeenMS)
	}
	if v.Witness.Key != "k/42" || len(v.Witness.OpIDs) != 2 {
		t.Errorf("witness not carried: %+v", v.Witness)
	}
	if v.Shrink.Attempted || v.Shrink.SurvivingFaults == nil {
		t.Errorf("shrink = %+v, want a not-attempted object with an empty list", v.Shrink)
	}
	if v.MinimalRepro != nil || v.Suspect != nil {
		t.Error("minimal_repro and suspect must be null before anything produces them")
	}

	if got := DefaultSeverity(ClassResource); got != SeverityMedium {
		t.Errorf("DefaultSeverity(resource) = %q, want medium", got)
	}
	if got := DefaultSeverity(OracleClass("")); got != SeverityHigh {
		t.Errorf("DefaultSeverity(absent class) = %q; failing closed means not grading down", got)
	}
}

func TestOracleOutputValidateCatchesRealMistakes(t *testing.T) {
	base := func() OracleOutput {
		return OracleOutput{
			Schema: OracleOutputSchema, Oracle: "x", Class: ClassCrash,
			Status: StatusOK, ValidPhases: []Phase{PhaseAssert},
		}
	}
	cases := map[string]func(*OracleOutput){
		"missing oracle name":     func(o *OracleOutput) { o.Oracle = "" },
		"unknown class":           func(o *OracleOutput) { o.Class = OracleClass("vibes") },
		"unknown phase":           func(o *OracleOutput) { o.ValidPhases = []Phase{Phase("LATER")} },
		"violated with no reason": func(o *OracleOutput) { o.Status = StatusViolated },
		"wrong schema":            func(o *OracleOutput) { o.Schema = "x/v1" },
		"unknown status":          func(o *OracleOutput) { o.Status = OracleStatus("meh") },
	}
	for name, mutate := range cases {
		o := base()
		mutate(&o)
		if err := o.Validate(); err == nil {
			t.Errorf("%s: Validate accepted it", name)
		}
	}
	o := base()
	if err := o.Validate(); err != nil {
		t.Errorf("a well-formed oracle output was rejected: %v", err)
	}
}

func TestBuiltinOracleDeclarations(t *testing.T) {
	if len(AllBuiltinOracles) != 6 {
		t.Fatalf("%d built-in oracles, want the 6 the directive names", len(AllBuiltinOracles))
	}
	for _, o := range AllBuiltinOracles {
		if !o.Class().Valid() {
			t.Errorf("%q has no class", o)
		}
		phases := o.ValidPhases()
		if len(phases) == 0 {
			t.Errorf("%q declares no valid phases; invariant I5 requires every oracle to", o)
		}
		for _, p := range phases {
			if !p.Valid() {
				t.Errorf("%q declares unknown phase %q", o, p)
			}
		}
	}
	// The post-recovery oracles must not be evaluated while a fault is still
	// injected; that is precisely the false positive I5 exists to prevent.
	for _, o := range []BuiltinOracle{
		BuiltinNoUnboundedQueue, BuiltinResourceReturnToBaseline, BuiltinAvailabilityAfterHeal,
	} {
		for _, p := range o.ValidPhases() {
			if p == PhaseDrive || p == PhasePerturb {
				t.Errorf("%q claims to be valid in %s", o, p)
			}
		}
	}
}
