package control

import (
	"context"
	"testing"
	"time"

	"github.com/WrdCstlg/pro-thesis/internal/recorder"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// What a clean run's wall-clock expiry means depends on what the profile asked
// for. An open-ended profile (worlds: -1) is DEFINED by running until the budget
// expires, so expiry is how it completes; it used to exit 3 every time, so a
// soak could never pass. A finite profile that ran out of time before its
// planned worlds were done did not perform the gate.
func TestExpiryOutcomeOpenEndedVersusFinite(t *testing.T) {
	cases := []struct {
		name     string
		budget   Budget
		finished int
		want     Outcome
	}{
		{"open-ended, one world finished, default minimum", Budget{Wall: time.Minute, Worlds: -1}, 1, OutcomePass},
		{"open-ended, minimum met", Budget{Wall: time.Minute, Worlds: -1, MinWorlds: 3}, 3, OutcomePass},
		{"open-ended, below its minimum", Budget{Wall: time.Minute, Worlds: -1, MinWorlds: 3}, 2, OutcomeBudgetExhausted},
		{"open-ended, nothing finished", Budget{Wall: time.Minute, Worlds: -1}, 0, OutcomeBudgetExhausted},
		{"finite, stopped before its planned worlds", Budget{Wall: time.Minute, Worlds: 5}, 2, OutcomeBudgetExhausted},
		{"finite, every planned world finished", Budget{Wall: time.Minute, Worlds: 2}, 2, OutcomePass},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clock := recorder.NewSimClock(time.Now())
			tr := NewBudgetTracker(tc.budget, clock)
			for i := 0; i < tc.finished; i++ {
				tr.NoteWorldStarted()
				tr.NoteWorldFinished()
			}
			clock.Advance(2 * time.Minute)
			if !tr.WallExpired() {
				t.Fatal("setup: the wall budget did not expire")
			}
			if got := ExpiryOutcome(tr); got != tc.want {
				t.Fatalf("ExpiryOutcome = %q, want %q", got, tc.want)
			}
		})
	}
}

// A world entered but cut short by expiry consumed time and judged nothing; it
// must not count toward an open-ended profile's minimum.
func TestAWorldCutShortDoesNotCountTowardTheMinimum(t *testing.T) {
	clock := recorder.NewSimClock(time.Now())
	tr := NewBudgetTracker(Budget{Wall: time.Minute, Worlds: -1, MinWorlds: 2}, clock)
	tr.NoteWorldStarted()
	tr.NoteWorldFinished()
	tr.NoteWorldStarted() // entered, never finished
	clock.Advance(2 * time.Minute)
	if got := ExpiryOutcome(tr); got != OutcomeBudgetExhausted {
		t.Fatalf("ExpiryOutcome = %q with 1 of a required 2 worlds finished, want %q", got, OutcomeBudgetExhausted)
	}
}

func TestBudgetForProfileCarriesTheMinimum(t *testing.T) {
	unbounded, two, three := -1, 2, 3
	cases := []struct {
		name string
		prof schema.Profile
		want Budget
	}{
		{"open-ended without a minimum", schema.Profile{Budget: schema.Duration(time.Hour), Worlds: &unbounded},
			Budget{Wall: time.Hour, Worlds: -1, MinWorlds: 1}},
		{"open-ended with a minimum", schema.Profile{Budget: schema.Duration(time.Hour), Worlds: &unbounded, MinWorlds: &three},
			Budget{Wall: time.Hour, Worlds: -1, MinWorlds: 3}},
		{"worlds omitted is open-ended", schema.Profile{Budget: schema.Duration(time.Hour)},
			Budget{Wall: time.Hour, Worlds: -1, MinWorlds: 1}},
		{"finite", schema.Profile{Budget: schema.Duration(time.Hour), Worlds: &two},
			Budget{Wall: time.Hour, Worlds: 2}},
	}
	for _, tc := range cases {
		if got := BudgetForProfile(tc.prof); got != tc.want {
			t.Fatalf("%s: BudgetForProfile = %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

// End to end: an open-ended profile whose short wall budget expires during a
// clean world exits 0, and the same profile requiring more worlds than it had
// time for exits 3.
func TestAnOpenEndedRunPassesWhenItsBudgetExpiresCleanly(t *testing.T) {
	run := func(minWorlds *int) (schema.Verdict, schema.ExitCode) {
		cfg := perturbTestConfig(t)
		unbounded := -1
		cfg.Profiles["smoke"] = schema.Profile{
			// Short enough that it expires during the first world (a stub world
			// takes about two seconds here), long enough that the first starts.
			Budget: schema.Duration(200 * time.Millisecond), Worlds: &unbounded,
			MinWorlds: minWorlds, DriverProfile: "smoke",
		}
		r, _ := newPerturbTestRunner(t, cfg, nil)
		v, exit, err := r.Run(context.Background())
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		return v, exit
	}
	v, exit := run(nil)
	if exit != schema.ExitPass {
		t.Fatalf("open-ended run, budget expired after a clean world: exit %d (%s), want 0", exit, v.Verdict)
	}
	if v.Budget.WorldsPlanned != schema.WorldsUnbounded || v.Budget.WorldsRun < 1 {
		t.Fatalf("budget = %+v, want worlds_planned -1 and at least one world run", v.Budget)
	}
	many := 50
	if _, exit := run(&many); exit != schema.ExitBudgetExhausted {
		t.Fatalf("open-ended run that finished fewer than min_worlds: exit %d, want 3", exit)
	}
}
