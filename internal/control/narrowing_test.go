package control

import (
	"testing"
	"time"

	"github.com/WrdCstlg/pro-thesis/internal/recorder"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// OQ-059. `--worlds 1` against a profile locked at 30 ran a thirtieth of the
// committed gate, exited 0, and reported `oracle_lock: ok` with a budget
// reading "1 run / 1 planned": the profile's own requirement appeared nowhere
// in the artifact. D-033 permitted this on the reasoning that narrowing "can
// only make the gate harder to pass", which is inverted for a search: fewer
// worlds is fewer chances to find the defect.
//
// The flags survive. What the tests below pin is that a narrowed run is
// RECORDED, may not report PASS, and may not report an unqualified lock.

func TestApplyCLIOverridesOnlyNarrows(t *testing.T) {
	profile := Budget{Wall: 10 * time.Minute, Worlds: 30}

	cases := map[string]struct {
		cliWall     time.Duration
		cliWorlds   int
		wantWall    time.Duration
		wantWorlds  int
		wantNarrow  bool
		explanation string
	}{
		"nothing supplied": {
			0, 0, 10 * time.Minute, 30, false,
			"a run that took the profile's budget must be indistinguishable from one recorded " +
				"before these fields existed",
		},
		"worlds narrowed": {
			0, 1, 10 * time.Minute, 1, true, "",
		},
		"wall narrowed": {
			time.Minute, 0, time.Minute, 30, true, "",
		},
		"both narrowed": {
			time.Minute, 1, time.Minute, 1, true, "",
		},
		"worlds widened is refused": {
			0, 100, 10 * time.Minute, 30, false,
			"widening from argv would let an agent grant itself a bigger budget without " +
				"touching the locked file",
		},
		"wall widened is refused": {
			time.Hour, 0, 10 * time.Minute, 30, false, "",
		},
		"equal values are not a narrowing": {
			10 * time.Minute, 30, 10 * time.Minute, 30, false,
			"asking for exactly what the profile specifies weakens nothing",
		},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got := ApplyCLIOverrides(profile, c.cliWall, c.cliWorlds)
			if got.Wall != c.wantWall || got.Worlds != c.wantWorlds {
				t.Errorf("effective budget = wall %v / worlds %d, want %v / %d",
					got.Wall, got.Worlds, c.wantWall, c.wantWorlds)
			}
			if got.Narrowed != c.wantNarrow {
				t.Errorf("Narrowed = %v, want %v. %s", got.Narrowed, c.wantNarrow, c.explanation)
			}
			// The profile's own numbers survive regardless.
			if got.RequiredWall != profile.Wall || got.RequiredWorlds != profile.Worlds {
				t.Errorf("the profile's requirement was lost: wall %v, worlds %d",
					got.RequiredWall, got.RequiredWorlds)
			}
		})
	}
}

// An unbounded profile is the case most easily got wrong: capping a soak run at
// 5 worlds IS a narrowing, even though 5 > -1 numerically.
func TestCappingAnUnboundedProfileIsANarrowing(t *testing.T) {
	unbounded := Budget{Wall: 0, Worlds: schema.WorldsUnbounded}

	got := ApplyCLIOverrides(unbounded, 0, 5)
	if !got.Narrowed {
		t.Fatal("capping an unbounded world count at 5 was not recorded as a narrowing; " +
			"a soak profile cut to five worlds has not performed the gate it specifies")
	}
	if got.Worlds != 5 {
		t.Errorf("Worlds = %d, want 5", got.Worlds)
	}

	got = ApplyCLIOverrides(unbounded, time.Minute, 0)
	if !got.Narrowed {
		t.Fatal("capping an unbounded wall budget at one minute was not recorded as a narrowing")
	}
}

func TestBudgetSnapshotCarriesTheProfilesRequirement(t *testing.T) {
	clock := recorder.NewSimClock(time.Now())

	t.Run("not narrowed emits nothing additive", func(t *testing.T) {
		b := ApplyCLIOverrides(Budget{Wall: 10 * time.Minute, Worlds: 30}, 0, 0)
		got := NewBudgetTracker(b, clock).Snapshot()
		if got.Narrowed || got.WorldsRequired != 0 || got.WallSRequired != 0 {
			t.Fatalf("an un-narrowed run emitted the additive fields (%+v); the directive's own "+
				"sample verdict must stay a valid document", got)
		}
		if got.WorldsPlanned != 30 {
			t.Errorf("worlds_planned = %d, want 30", got.WorldsPlanned)
		}
	})

	t.Run("narrowed records both numbers", func(t *testing.T) {
		b := ApplyCLIOverrides(Budget{Wall: 10 * time.Minute, Worlds: 30}, 0, 1)
		got := NewBudgetTracker(b, clock).Snapshot()
		if !got.Narrowed {
			t.Fatal("the snapshot does not say the budget was narrowed")
		}
		if got.WorldsPlanned != 1 {
			t.Errorf("worlds_planned = %d, want 1 (what actually ran)", got.WorldsPlanned)
		}
		if got.WorldsRequired != 30 {
			t.Errorf("worlds_required = %d, want 30. Without it the verdict reads "+
				"\"1 run / 1 planned\" and the profile's requirement is unrecoverable "+
				"from the artifact", got.WorldsRequired)
		}
		if got.WallSRequired != 600 {
			t.Errorf("wall_s_required = %d, want 600", got.WallSRequired)
		}
	})

	t.Run("an unbounded requirement is spelled, not zeroed", func(t *testing.T) {
		b := ApplyCLIOverrides(Budget{Wall: 0, Worlds: schema.WorldsUnbounded}, 0, 5)
		got := NewBudgetTracker(b, clock).Snapshot()
		if got.WorldsRequired != schema.WorldsUnbounded {
			t.Errorf("worlds_required = %d, want %d (unbounded); omitting it would read as "+
				"\"the profile asked for nothing\"", got.WorldsRequired, schema.WorldsUnbounded)
		}
	})
}

func TestANarrowedRunCannotReportPass(t *testing.T) {
	narrowed := schema.Budget{
		WallS: 600, WorldsPlanned: 1, WorldsRun: 1,
		Narrowed: true, WorldsRequired: 30, WallSRequired: 600,
	}
	full := schema.Budget{WallS: 600, WorldsPlanned: 30, WorldsRun: 30}
	okLock := schema.OracleLock{Status: schema.LockOK, ManifestSHA: "sha256:ab"}

	t.Run("clean narrowed run is inconclusive, not pass", func(t *testing.T) {
		v := BuildVerdict(VerdictInput{
			RunID: "r_test", Profile: "gate", Outcome: OutcomePass,
			Budget: narrowed, OracleLock: okLock,
		})
		if v.Verdict == schema.VerdictPass {
			t.Fatal("a run cut from 30 worlds to 1 found nothing and reported PASS. PASS " +
				"quantifies over the worlds the profile specifies; this run declined to look")
		}
		if v.Verdict != schema.VerdictInconclusive {
			t.Fatalf("verdict = %q, want INCONCLUSIVE", v.Verdict)
		}
		if v.ExitCode() != schema.ExitInconclusive {
			t.Fatalf("exit = %d, want 2", v.ExitCode())
		}
	})

	t.Run("a violation under a narrowed budget still exits 1", func(t *testing.T) {
		v := BuildVerdict(VerdictInput{
			RunID: "r_test", Profile: "gate", Outcome: OutcomeViolation,
			Budget: narrowed, OracleLock: okLock,
		})
		if v.ExitCode() != schema.ExitFail {
			t.Fatalf("exit = %d, want 1. A violation found in one world is still a violation, "+
				"and downgrading it would make --worlds 1 useless while fixing a bug",
				v.ExitCode())
		}
	})

	t.Run("an un-narrowed clean run still passes", func(t *testing.T) {
		v := BuildVerdict(VerdictInput{
			RunID: "r_test", Profile: "gate", Outcome: OutcomePass,
			Budget: full, OracleLock: okLock,
		})
		if v.Verdict != schema.VerdictPass {
			t.Fatalf("verdict = %q, want PASS: the profile's own world count running out "+
				"cleanly is the gate the profile asked for", v.Verdict)
		}
	})
}

func TestANarrowedRunCannotReportAnUnqualifiedLock(t *testing.T) {
	narrowed := schema.Budget{WorldsPlanned: 1, WorldsRun: 1, Narrowed: true, WorldsRequired: 30}

	t.Run("ok becomes bypassed", func(t *testing.T) {
		v := BuildVerdict(VerdictInput{
			RunID: "r_test", Profile: "gate", Outcome: OutcomeViolation, Budget: narrowed,
			OracleLock: schema.OracleLock{Status: schema.LockOK, ManifestSHA: "sha256:ab"},
		})
		if v.OracleLock.Status == schema.LockOK {
			t.Fatal("a run that overrode a lock-covered value from argv reported " +
				"oracle_lock: ok. The digest matched the file; it is argv that moved the " +
				"gate, and \"ok\" here is a true sentence functioning as a false one")
		}
		if v.OracleLock.Status != schema.LockBypassed {
			t.Fatalf("oracle_lock.status = %q, want %q", v.OracleLock.Status, schema.LockBypassed)
		}
		if v.OracleLock.ManifestSHA != "sha256:ab" {
			t.Errorf("the digest was lost: %q", v.OracleLock.ManifestSHA)
		}
	})

	t.Run("absent stays absent", func(t *testing.T) {
		v := BuildVerdict(VerdictInput{
			RunID: "r_test", Profile: "gate", Outcome: OutcomeViolation, Budget: narrowed,
			OracleLock: schema.OracleLock{Status: schema.LockAbsent},
		})
		if v.OracleLock.Status != schema.LockAbsent {
			t.Fatalf("oracle_lock.status = %q, want absent: nothing was locked, so nothing "+
				"was bypassed, and absent already fails `thesis oracles verify`",
				v.OracleLock.Status)
		}
	})

	t.Run("an un-narrowed run keeps ok", func(t *testing.T) {
		v := BuildVerdict(VerdictInput{
			RunID: "r_test", Profile: "gate", Outcome: OutcomePass,
			Budget:     schema.Budget{WorldsPlanned: 30, WorldsRun: 30},
			OracleLock: schema.OracleLock{Status: schema.LockOK, ManifestSHA: "sha256:ab"},
		})
		if v.OracleLock.Status != schema.LockOK {
			t.Fatalf("oracle_lock.status = %q, want ok", v.OracleLock.Status)
		}
	})
}
