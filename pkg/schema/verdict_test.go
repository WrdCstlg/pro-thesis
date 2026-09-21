package schema

import (
	"bytes"
	"encoding/json"
	"sort"
	"strings"
	"testing"
)

// TestGoldenVerdictRoundTrip decodes the directive's 4.6 verdict, re-emits it,
// and compares the CANONICAL forms byte for byte.
//
// Equality, not containment: this package deliberately adds no field to the
// verdict and drops none, so a superset or a subset is a regression. If a
// future phase genuinely needs an additive verdict field, this test is where
// that decision becomes a visible, reviewed edit.
func TestGoldenVerdictRoundTrip(t *testing.T) {
	raw := readTestdata(t, "verdict.golden.json")

	v, err := UnmarshalVerdict(raw)
	if err != nil {
		t.Fatalf("UnmarshalVerdict: %v", err)
	}
	if err := v.Validate(); err != nil {
		t.Fatalf("the directive's own verdict does not validate: %v", err)
	}

	out, err := MarshalVerdict(v)
	if err != nil {
		t.Fatalf("MarshalVerdict: %v", err)
	}
	gotCanon, err := Canonicalize(out)
	if err != nil {
		t.Fatalf("canonicalize re-emission: %v", err)
	}
	wantCanon, err := Canonicalize(raw)
	if err != nil {
		t.Fatalf("canonicalize golden: %v", err)
	}
	if !bytes.Equal(gotCanon, wantCanon) {
		t.Errorf("verdict re-emission differs from the directive's sample.\n got: %s\nwant: %s",
			gotCanon, wantCanon)
	}

	// Spot checks on the values a consumer branches on.
	if v.RunID != "r_2026_09_03_a41f" || v.Profile != "gate" || v.Verdict != VerdictFail {
		t.Errorf("verdict header = %+v", *v)
	}
	if v.ExitCode() != ExitFail {
		t.Errorf("ExitCode = %d, want 1", v.ExitCode())
	}
	if v.Budget != (Budget{WallS: 600, UsedS: 412, WorldsPlanned: 30, WorldsRun: 22}) {
		t.Errorf("budget = %+v", v.Budget)
	}
	if len(v.Violations) != 1 {
		t.Fatalf("violations = %d, want 1", len(v.Violations))
	}
	vi := v.Violations[0]
	if vi.ID != "v1" || vi.Oracle != "linearizable.kv" || vi.Class != ClassConsistency {
		t.Errorf("violation = %+v", vi)
	}
	// The only class/severity pair either document exhibits is
	// consistency -> high. A table that re-graded it would break here.
	if vi.Severity != SeverityHigh {
		t.Errorf("severity = %q, want high (directive 4.6)", vi.Severity)
	}
	if got := DefaultSeverity(ClassConsistency); got != SeverityHigh {
		t.Errorf("DefaultSeverity(consistency) = %q, want high", got)
	}
	if vi.Phase != PhaseDrive || vi.FirstSeenMS != 14320 {
		t.Errorf("violation phase/first_seen = %q/%d", vi.Phase, vi.FirstSeenMS)
	}
	if len(vi.Witness.OpIDs) != 2 || vi.Witness.OpIDs[0] != 8891 || vi.Witness.Key != "k/42" {
		t.Errorf("witness = %+v", vi.Witness)
	}
	if vi.MinimalRepro == nil || vi.MinimalRepro.Reproduced != "3/3" {
		t.Errorf("minimal_repro = %+v", vi.MinimalRepro)
	}
	if !vi.Shrink.Attempted || vi.Shrink.FaultsBefore != 14 || vi.Shrink.OpsAfter != 6 {
		t.Errorf("shrink = %+v", vi.Shrink)
	}
	if len(vi.Shrink.SurvivingFaults) != 2 {
		t.Fatalf("surviving_faults = %v", vi.Shrink.SurvivingFaults)
	}
	for _, f := range vi.Shrink.SurvivingFaults {
		if _, err := ParseFault(f); err != nil {
			t.Errorf("surviving fault %q does not parse: %v", f, err)
		}
	}
	if len(vi.CausalTimeline) != 4 || vi.CausalTimeline[2].Node != "n1" {
		t.Errorf("causal_timeline = %+v", vi.CausalTimeline)
	}
	if vi.CausalTimeline[0].Node != "" {
		t.Errorf("causal_timeline[0].node = %q, want absent", vi.CausalTimeline[0].Node)
	}
	if vi.Suspect == nil || vi.Suspect.Confidence != 0.42 || len(vi.Suspect.Files) != 2 {
		t.Errorf("suspect = %+v", vi.Suspect)
	}
	if v.Coverage != (Coverage{NewTemplates: 12, CumTemplates: 4102, NewStates: 6, CumStates: 18933}) {
		t.Errorf("coverage = %+v", v.Coverage)
	}
	if v.CoverageDeltaVsBaseline == nil || v.CoverageDeltaVsBaseline.Templates != -3 {
		t.Errorf("coverage_delta_vs_baseline = %+v", v.CoverageDeltaVsBaseline)
	}
	if v.OracleLock.Status != LockOK || !strings.HasPrefix(v.OracleLock.ManifestSHA, HashPrefix) {
		t.Errorf("oracle_lock = %+v", v.OracleLock)
	}
	if v.Artifacts != ".prothesis/runs/r_2026_09_03_a41f/" {
		t.Errorf("artifacts = %q", v.Artifacts)
	}
}

// TestVerdictPresenceRule: a zero verdict must still be safe for an agent to
// iterate. Empty lists are [], and exactly three fields are null.
func TestVerdictPresenceRule(t *testing.T) {
	v := NewVerdict("r_2026_09_03_a41f", "smoke", VerdictPass)
	out, err := MarshalVerdict(&v)
	if err != nil {
		t.Fatalf("MarshalVerdict: %v", err)
	}

	var doc map[string]json.RawMessage
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	wantKeys := []string{
		"artifacts", "budget", "commit", "coverage", "coverage_delta_vs_baseline",
		"oracle_lock", "profile", "run_id", "schema", "verdict", "violations",
	}
	gotKeys := make([]string, 0, len(doc))
	for k := range doc {
		gotKeys = append(gotKeys, k)
	}
	sort.Strings(gotKeys)
	if strings.Join(gotKeys, ",") != strings.Join(wantKeys, ",") {
		t.Errorf("verdict keys = %v, want %v", gotKeys, wantKeys)
	}
	if string(doc["violations"]) != "[]" {
		t.Errorf("violations = %s, want []", doc["violations"])
	}
	nulls := 0
	for k, raw := range doc {
		if string(raw) == "null" {
			nulls++
			if k != "coverage_delta_vs_baseline" {
				t.Errorf("%s is null; only coverage_delta_vs_baseline may be null at the top level", k)
			}
		}
	}
	if nulls != 1 {
		t.Errorf("%d top-level nulls, want exactly 1", nulls)
	}
	if v.OracleLock.Status != LockAbsent {
		t.Errorf("oracle_lock.status = %q; a verdict with no lock must not claim ok", v.OracleLock.Status)
	}
}

// TestVerdictEmitDecodeRoundTripsUnscopedFields.
//
// The total contract emits "phase":"" and "class":"" for a violation that is
// not phase-scoped or came from an oracle that omitted its class. If the strict
// enum decoders rejected the empty string, a verdict PRO-THESIS emits could not
// be read back by PRO-THESIS, which would break `thesis report` and every
// corpus reader.
func TestVerdictEmitDecodeRoundTripsUnscopedFields(t *testing.T) {
	v := NewVerdict("r_2026_09_03_a41f", "gate", VerdictFail)
	v.Violations = []Violation{{
		ID:          "v1",
		Oracle:      "third.party",
		Explanation: "something happened",
	}}
	out, err := MarshalVerdict(&v)
	if err != nil {
		t.Fatalf("MarshalVerdict: %v", err)
	}
	if !bytes.Contains(out, []byte(`"phase": ""`)) {
		t.Errorf("expected an explicit empty phase in:\n%s", out)
	}
	back, err := UnmarshalVerdict(out)
	if err != nil {
		t.Fatalf("a verdict this package emitted could not be read back: %v", err)
	}
	if len(back.Violations) != 1 {
		t.Fatalf("violations = %d", len(back.Violations))
	}
	got := back.Violations[0]
	if got.Phase != "" || got.Class != "" {
		t.Errorf("unscoped fields changed: phase=%q class=%q", got.Phase, got.Class)
	}
	// Normalize must have graded the finding rather than leaving it empty, and
	// must have graded it high rather than low on incomplete information.
	if got.Severity != SeverityHigh {
		t.Errorf("severity = %q, want high", got.Severity)
	}
	if got.Shrink.SurvivingFaults == nil || got.CausalTimeline == nil {
		t.Errorf("normalize left a null list: %+v", got)
	}
	if got.MinimalRepro != nil || got.Suspect != nil {
		t.Error("minimal_repro and suspect must stay null when nothing produced them")
	}
}

// TestVerdictSurvivingFaultsNotHTMLEscaped: an edge target inside the verdict
// must survive as written, or the string a human is told to replay is wrong.
func TestVerdictSurvivingFaultsNotHTMLEscaped(t *testing.T) {
	v := NewVerdict("r_2026_09_03_a41f", "gate", VerdictFail)
	v.Violations = []Violation{{
		ID: "v1", Oracle: "no_crash", Class: ClassCrash,
		Shrink: Shrink{Attempted: true, SurvivingFaults: []string{"net.partition(n1<->n2)@100..200"}},
	}}
	out, err := MarshalVerdict(&v)
	if err != nil {
		t.Fatalf("MarshalVerdict: %v", err)
	}
	if !bytes.Contains(out, []byte("n1<->n2")) {
		t.Errorf("edge target was escaped:\n%s", out)
	}
}

// TestWitnessPreservesOracleAuthoredMembers: a witness is authored by a third
// party and its useful content is oracle-specific.
func TestWitnessPreservesOracleAuthoredMembers(t *testing.T) {
	const src = `{"op_ids":[1,2],"key":"k/42","term":5,"nodes":["n1","n2"]}`
	var w Witness
	if err := json.Unmarshal([]byte(src), &w); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(w.OpIDs) != 2 || w.Key != "k/42" {
		t.Errorf("witness = %+v", w)
	}
	if string(w.Extra["term"]) != "5" || string(w.Extra["nodes"]) != `["n1","n2"]` {
		t.Errorf("extra members lost: %v", w.Extra)
	}
	out, err := json.Marshal(w)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	gotCanon, _ := Canonicalize(out)
	wantCanon, _ := Canonicalize([]byte(src))
	if !bytes.Equal(gotCanon, wantCanon) {
		t.Errorf("witness round trip:\n got: %s\nwant: %s", gotCanon, wantCanon)
	}

	var empty Witness
	b, err := json.Marshal(empty)
	if err != nil {
		t.Fatalf("encode empty: %v", err)
	}
	if string(b) != "{}" {
		t.Errorf("empty witness = %s, want {}", b)
	}
	if !empty.IsEmpty() {
		t.Error("IsEmpty = false on a zero witness")
	}

	if err := json.Unmarshal([]byte(`{"op_ids":["not an id"]}`), &empty); err == nil {
		t.Error("a witness with non-integer op_ids was accepted")
	}
}

func TestVerdictValidateCatchesRealMistakes(t *testing.T) {
	base := func() Verdict {
		v := NewVerdict("r_2026_09_03_a41f", "gate", VerdictFail)
		v.Violations = []Violation{{
			ID: "v1", Oracle: "no_crash", Class: ClassCrash, Severity: SeverityHigh,
			Shrink: Shrink{SurvivingFaults: []string{}}, CausalTimeline: []TimelineEvent{},
		}}
		return v
	}
	cases := map[string]func(*Verdict){
		"bad run id":          func(v *Verdict) { v.RunID = "run-1" },
		"unknown verdict":     func(v *Verdict) { v.Verdict = VerdictResult("MAYBE") },
		"duplicate ids":       func(v *Verdict) { v.Violations = append(v.Violations, v.Violations[0]) },
		"missing oracle":      func(v *Verdict) { v.Violations[0].Oracle = "" },
		"unknown class":       func(v *Verdict) { v.Violations[0].Class = OracleClass("vibes") },
		"unparseable fault":   func(v *Verdict) { v.Violations[0].Shrink.SurvivingFaults = []string{"nope"} },
		"confidence over one": func(v *Verdict) { v.Violations[0].Suspect = &Suspect{Confidence: 1.5} },
		"unknown lock status": func(v *Verdict) { v.OracleLock.Status = LockStatus("fine") },
	}
	for name, mutate := range cases {
		v := base()
		mutate(&v)
		if err := v.Validate(); err == nil {
			t.Errorf("%s: Validate accepted it", name)
		}
	}
	v := base()
	if err := v.Validate(); err != nil {
		t.Errorf("a well-formed verdict was rejected: %v", err)
	}
}

// D-059 added `bypassed` to the lock status set and three additive budget
// fields. Both are read by consumers out of verdict.json, so both have to
// survive the artifact boundary: a status the decoder rejects would turn every
// narrowed run into an unreadable verdict.
func TestNarrowedBudgetAndBypassedLockSurviveTheArtifact(t *testing.T) {
	for _, s := range AllLockStatuses {
		v := NewVerdict("r_2026_09_03_a41f", "gate", VerdictInconclusive)
		v.Violations = []Violation{}
		v.OracleLock.Status = s
		if s != LockAbsent {
			v.OracleLock.ManifestSHA = "sha256:" + "ab"
		}
		v.Budget = Budget{
			WallS: 600, UsedS: 12, WorldsPlanned: 1, WorldsRun: 1,
			Narrowed: true, WorldsRequired: 30, WallSRequired: 600,
		}
		if err := v.Validate(); err != nil {
			t.Fatalf("lock status %q: a valid verdict was rejected: %v", s, err)
		}
		raw, err := MarshalVerdict(&v)
		if err != nil {
			t.Fatalf("lock status %q: marshal: %v", s, err)
		}
		back, err := UnmarshalVerdict(raw)
		if err != nil {
			t.Fatalf("lock status %q: the decoder refused a verdict it had just written: %v", s, err)
		}
		if back.OracleLock.Status != s {
			t.Errorf("lock status round-tripped %q -> %q", s, back.OracleLock.Status)
		}
		if back.Budget != v.Budget {
			t.Errorf("budget round-tripped %+v -> %+v", v.Budget, back.Budget)
		}
	}
}

// The additive fields must be ABSENT from an un-narrowed verdict, which is what
// keeps the directive's own sample a valid document and every world hash still.
func TestAnUnNarrowedVerdictEmitsNoAdditiveBudgetFields(t *testing.T) {
	v := NewVerdict("r_2026_09_03_a41f", "gate", VerdictPass)
	v.Violations = []Violation{}
	v.Budget = Budget{WallS: 600, UsedS: 412, WorldsPlanned: 30, WorldsRun: 30}

	raw, err := MarshalVerdict(&v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, field := range []string{"narrowed", "worlds_required", "wall_s_required"} {
		if bytes.Contains(raw, []byte(`"`+field+`"`)) {
			t.Errorf("an un-narrowed verdict emitted %q; a run that took the profile's budget "+
				"must be byte-identical to one recorded before D-059", field)
		}
	}
}

func TestExitCodeVerdictBijection(t *testing.T) {
	for _, c := range AllExitCodes {
		if got := c.Verdict().ExitCode(); got != c {
			t.Errorf("exit %d -> %q -> exit %d", c, c.Verdict(), got)
		}
		if c.AgentAction() == "" {
			t.Errorf("exit %d has no agent action", c)
		}
	}
	for _, r := range AllVerdictResults {
		if got := r.ExitCode().Verdict(); got != r {
			t.Errorf("verdict %q -> exit %d -> verdict %q", r, r.ExitCode(), got)
		}
	}
}
