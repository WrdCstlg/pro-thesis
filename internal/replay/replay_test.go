package replay

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/WrdCstlg/pro-thesis/internal/control"
	"github.com/WrdCstlg/pro-thesis/internal/shrink"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// ---------------------------------------------------------------------------
// A scripted executor.
//
// A world costs ~30 s wall on this host, so the k/n arithmetic, the divergence
// rule and the budget are pinned WITHOUT Docker. That is the whole reason
// Executor is an interface.
// ---------------------------------------------------------------------------

type scriptedExec struct {
	steps []Execution
	seen  []ExecRequest
}

func (s *scriptedExec) Execute(_ context.Context, req ExecRequest) Execution {
	s.seen = append(s.seen, req)
	if len(s.seen) > len(s.steps) {
		return Execution{Outcome: control.OutcomeHarnessError}
	}
	return s.steps[len(s.seen)-1]
}

func violated(oracle string, class schema.OracleClass, key string) control.OracleResult {
	return control.OracleResult{
		Output: schema.OracleOutput{
			Schema:  schema.OracleOutputSchema,
			Oracle:  oracle,
			Class:   class,
			Status:  schema.StatusViolated,
			Witness: schema.Witness{Key: key},
		},
	}
}

func satisfied(oracle string, class schema.OracleClass) control.OracleResult {
	return control.OracleResult{
		Output: schema.OracleOutput{
			Schema: schema.OracleOutputSchema,
			Oracle: oracle,
			Class:  class,
			Status: schema.StatusOK,
		},
	}
}

func clean() Execution {
	return Execution{
		Outcome:  control.OutcomePass,
		Findings: []control.OracleResult{satisfied("no_crash", schema.ClassCrash)},
	}
}

func reproduces(key string) Execution {
	return Execution{
		Outcome: control.OutcomeViolation,
		Findings: []control.OracleResult{
			satisfied("no_crash", schema.ClassCrash),
			violated("linearizable.kv", schema.ClassConsistency, key),
		},
	}
}

func testWorld() *schema.World {
	return worldWith(
		[]string{"net.partition(kv-n1)@3000..5000"},
		[]schema.RealizedFault{{
			Fault: "net.partition(kv-n1)@3000..5000", Resolved: "net.partition(kv-n1)@3000..5000",
			Nodes: []string{"kv-n1"}, StartMS: 3001, EndMS: 5002,
		}},
	)
}

// ---------------------------------------------------------------------------

// The world's OWN seed is what runs. WorldSeedFor is an HMAC over (run seed,
// ordinal) that cannot be inverted, so without a pinned seed a replay would
// execute the recorded fault schedule under a different workload: the same
// faults, a different world.
func TestTheReplayExecutesTheWorldsOwnSeed(t *testing.T) {
	ex := &scriptedExec{steps: []Execution{clean(), clean(), clean()}}
	w := testWorld()

	if _, err := Run(context.Background(), Options{World: w, Exec: ex}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(ex.seen) != DefaultAttempts {
		t.Fatalf("ran %d attempts, want the default %d", len(ex.seen), DefaultAttempts)
	}
	for i, req := range ex.seen {
		if req.Seed != w.Seed {
			t.Fatalf("attempt %d executed seed %d, want the world's own %d", i+1, req.Seed, w.Seed)
		}
		if len(req.Faults) != 1 || req.Faults[0] != "net.partition(kv-n1)@3000..5000" {
			t.Fatalf("attempt %d executed %v, want the realized schedule", i+1, req.Faults)
		}
	}
	// Each attempt gets its own artifact directory, so k confirmations leave k
	// inspectable worlds rather than overwriting one.
	for i := 1; i < len(ex.seen); i++ {
		if ex.seen[i].Ordinal == ex.seen[i-1].Ordinal {
			t.Fatalf("attempts %d and %d share ordinal %d; each needs its own world directory",
				i, i+1, ex.seen[i].Ordinal)
		}
	}
}

// k/n is REPORTED, never rounded. The spec concedes Tier B replay is
// probabilistic and requires the honest number.
func TestTheConfirmationIsReportedNotRounded(t *testing.T) {
	ex := &scriptedExec{steps: []Execution{reproduces("k/0"), clean(), reproduces("k/0")}}

	rep, err := Run(context.Background(), Options{World: testWorld(), Exec: ex})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := rep.Confirmation.String(); got != "2/3" {
		t.Fatalf("Confirmation = %q, want %q — 2/3 must not be rounded to 3/3", got, "2/3")
	}
	if rep.Confirmation.Passed() {
		t.Fatal("a 2/3 confirmation must not report Passed(); the corpus gate depends on it")
	}
	if rep.ExitCode() != schema.ExitFail {
		t.Fatalf("exit = %d, want %d: the world reproduced, which is the actionable answer",
			rep.ExitCode(), schema.ExitFail)
	}
}

// An execution nobody could judge is neither a reproduction nor a
// non-reproduction. Folding it into either manufactures evidence in one
// direction or flakiness in the other.
func TestAnUnjudgeableExecutionIsCountedSeparately(t *testing.T) {
	ex := &scriptedExec{steps: []Execution{
		reproduces("k/0"),
		{Outcome: control.OutcomeHarnessError},
		{Outcome: control.OutcomeInconclusive, Findings: []control.OracleResult{
			{Output: schema.OracleOutput{Oracle: "no_stuck_op", Class: schema.ClassLiveness,
				Status: schema.StatusInconclusive}},
		}},
	}}

	rep, err := Run(context.Background(), Options{World: testWorld(), Exec: ex})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Confirmation.Reproduced != 1 || rep.Confirmation.Attempts != 3 || rep.Confirmation.Inconclusive != 2 {
		t.Fatalf("Confirmation = %+v, want 1 reproduced of 3 with 2 inconclusive", rep.Confirmation)
	}
	if !strings.Contains(rep.Confirmation.String(), "inconclusive") {
		t.Fatalf("the printed confirmation %q hides the inconclusive replays", rep.Confirmation)
	}
	if rep.Confirmation.Passed() {
		t.Fatal("a confirmation containing an unjudgeable replay is not k/k")
	}
	if rep.Attempts[1].Signal != SignalUnknown || rep.Attempts[2].Signal != SignalUnknown {
		t.Fatalf("signals = %q, %q; both must be %q",
			rep.Attempts[1].Signal, rep.Attempts[2].Signal, SignalUnknown)
	}
}

// THE RANKED #1 FAILURE MODE, on the replay side. A world that fails a DIFFERENT
// oracle has not reproduced anything, and reporting it as a reproduction would
// send an agent to fix the wrong thing.
func TestADifferentViolationIsNotAReproduction(t *testing.T) {
	ex := &scriptedExec{steps: []Execution{{
		Outcome: control.OutcomeViolation,
		Findings: []control.OracleResult{
			violated("availability_after_heal", schema.ClassLiveness, ""),
		},
	}}}

	rep, err := Run(context.Background(), Options{
		World:      testWorld(),
		Exec:       ex,
		Attempts:   1,
		Expect:     shrink.Identity{Oracle: "linearizable.kv", Class: schema.ClassConsistency, Key: "k/0"},
		Strictness: shrink.MatchWitnessKey,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Confirmation.Reproduced != 0 {
		t.Fatalf("a violation from a different oracle was counted as a reproduction: %+v", rep.Confirmation)
	}
	if rep.Attempts[0].Signal != SignalDifferent {
		t.Fatalf("signal = %q, want %q", rep.Attempts[0].Signal, SignalDifferent)
	}
	d := rep.Divergences()
	if len(d) != 1 || d[0].Oracle != "availability_after_heal" {
		t.Fatalf("divergences = %v, want the availability violation named", d)
	}
	if rep.ExitCode() != schema.ExitPass {
		t.Fatalf("exit = %d, want %d: the violation being replayed did not reproduce",
			rep.ExitCode(), schema.ExitPass)
	}
}

// The mirror of the test above: the SAME oracle on a DIFFERENT key is the same
// defect, not a different one. OQ-034 measures op ids moving between two runs of
// one defect; the witness KEY is the discriminator that survives, and only when
// both sides carry one.
func TestTheSameOracleOnADifferentKeyIsStillNotTheSameViolation(t *testing.T) {
	ex := &scriptedExec{steps: []Execution{reproduces("k/7")}}

	rep, err := Run(context.Background(), Options{
		World:      testWorld(),
		Exec:       ex,
		Attempts:   1,
		Expect:     shrink.Identity{Oracle: "linearizable.kv", Class: schema.ClassConsistency, Key: "k/0"},
		Strictness: shrink.MatchWitnessKey,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Attempts[0].Signal != SignalDifferent {
		t.Fatalf("signal = %q, want %q at MatchWitnessKey", rep.Attempts[0].Signal, SignalDifferent)
	}

	// And at the directive's floor, where no key is compared, it IS a match.
	ex2 := &scriptedExec{steps: []Execution{reproduces("k/7")}}
	rep2, err := Run(context.Background(), Options{
		World:      testWorld(),
		Exec:       ex2,
		Attempts:   1,
		Expect:     shrink.Identity{Oracle: "linearizable.kv", Class: schema.ClassConsistency, Key: "k/0"},
		Strictness: shrink.MatchOracleClass,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep2.Attempts[0].Signal != SignalReproduced {
		t.Fatalf("signal = %q at MatchOracleClass, want %q", rep2.Attempts[0].Signal, SignalReproduced)
	}
}

// With no expected identity, which is `thesis regress`'s rule, any violation
// counts, and the rule is stated in the report rather than implied.
func TestWithNoExpectedIdentityAnyViolationCounts(t *testing.T) {
	ex := &scriptedExec{steps: []Execution{{
		Outcome:  control.OutcomeViolation,
		Findings: []control.OracleResult{violated("no_crash", schema.ClassCrash, "")},
	}}}

	rep, err := Run(context.Background(), Options{World: testWorld(), Exec: ex, Attempts: 1})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Confirmation.Reproduced != 1 {
		t.Fatalf("Confirmation = %+v, want the violation counted", rep.Confirmation)
	}
	if !strings.Contains(rep.MatchRule, "any oracle violation") {
		t.Fatalf("MatchRule = %q, want it to state that any violation counted", rep.MatchRule)
	}
}

// A world nobody checked is not a clean negative. An oracle set that produces
// nothing is the vacuous pass this project refuses to emit.
func TestAWorldWithNoOracleResultsIsUnknownNotClean(t *testing.T) {
	ex := &scriptedExec{steps: []Execution{{Outcome: control.OutcomePass}}}

	rep, err := Run(context.Background(), Options{World: testWorld(), Exec: ex, Attempts: 1})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Attempts[0].Signal != SignalUnknown {
		t.Fatalf("signal = %q, want %q: no oracle produced a result, so nothing was checked",
			rep.Attempts[0].Signal, SignalUnknown)
	}
	if rep.ExitCode() != schema.ExitInconclusive {
		t.Fatalf("exit = %d, want %d", rep.ExitCode(), schema.ExitInconclusive)
	}
}

// The budget stops the replay BEFORE an attempt starts, never during one, and
// what did not run is reported.
func TestTheBudgetStopsBetweenAttemptsAndIsReported(t *testing.T) {
	now := time.Unix(0, 0)
	ex := &scriptedExec{steps: []Execution{clean(), clean(), clean()}}

	rep, err := Run(context.Background(), Options{
		World:    testWorld(),
		Exec:     ex,
		Attempts: 3,
		Budget:   45 * time.Second,
		Now: func() time.Time {
			// Each call advances 30 s, the measured cost of a world. The first
			// attempt starts at t=0, the second is checked at t=30s (inside the
			// 45 s budget), the third at t>=60s (outside).
			t := now
			now = now.Add(30 * time.Second)
			return t
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(ex.seen) == 3 {
		t.Fatal("the budget did not stop the replay; all three attempts ran")
	}
	if !rep.BudgetExpired {
		t.Fatal("BudgetExpired is false after the budget stopped an attempt")
	}
	if rep.Planned != 3 {
		t.Fatalf("Planned = %d, want 3: the report must say what was ASKED for", rep.Planned)
	}
	if rep.Confirmation.Attempts != len(ex.seen) {
		t.Fatalf("Confirmation counts %d attempts but %d ran", rep.Confirmation.Attempts, len(ex.seen))
	}
	if rep.ExitCode() != schema.ExitBudgetExhausted {
		t.Fatalf("exit = %d, want %d", rep.ExitCode(), schema.ExitBudgetExhausted)
	}
}

// --keep-up applies to the LAST attempt only. Keeping every attempt up would
// leave k topologies behind, and CURRENT can name only one.
func TestKeepUpAppliesToTheLastAttemptOnly(t *testing.T) {
	ex := &scriptedExec{steps: []Execution{clean(), clean(), clean()}}

	if _, err := Run(context.Background(), Options{
		World: testWorld(), Exec: ex, Attempts: 3, KeepUp: true,
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for i, req := range ex.seen {
		want := i == len(ex.seen)-1
		if req.KeepUp != want {
			t.Fatalf("attempt %d KeepUp = %v, want %v", i+1, req.KeepUp, want)
		}
	}
}

// A replay that did not reproduce is a FACT about the world, reported as exit 0
// with the measured 0/k next to it, not an error, and not a claim that the bug
// is gone.
func TestANonReproductionIsAFactNotAnError(t *testing.T) {
	ex := &scriptedExec{steps: []Execution{clean(), clean(), clean()}}

	rep, err := Run(context.Background(), Options{World: testWorld(), Exec: ex})
	if err != nil {
		t.Fatalf("a non-reproduction must not be an error: %v", err)
	}
	if rep.ExitCode() != schema.ExitPass {
		t.Fatalf("exit = %d, want %d", rep.ExitCode(), schema.ExitPass)
	}
	if got := rep.Confirmation.String(); got != "0/3" {
		t.Fatalf("Confirmation = %q, want %q", got, "0/3")
	}
	for _, a := range rep.Attempts {
		if a.Signal != SignalNotReproduced {
			t.Fatalf("attempt %d signal = %q, want %q", a.N, a.Signal, SignalNotReproduced)
		}
	}
}

func TestRunRefusesWithoutAWorldOrAnExecutor(t *testing.T) {
	if _, err := Run(context.Background(), Options{Exec: &scriptedExec{}}); err == nil {
		t.Fatal("Run accepted a nil world")
	}
	if _, err := Run(context.Background(), Options{World: testWorld()}); err == nil {
		t.Fatal("Run accepted a nil executor")
	}
}
