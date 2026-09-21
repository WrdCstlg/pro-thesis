package control

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/WrdCstlg/pro-thesis/internal/recorder"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

type bufSink struct{ bytes.Buffer }

func (s *bufSink) AppendLine(line []byte) error {
	s.Write(line)
	return nil
}

func TestPhaseLogNestUnnest(t *testing.T) {
	var out bufSink
	clock := recorder.NewSimClock(time.Now())
	tl := &recorder.Timeline{}
	pl, err := NewPhaseLog(clock, tl, &out)
	if err != nil {
		t.Fatalf("NewPhaseLog: %v", err)
	}

	if err := pl.Advance(schema.PhaseBoot); err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if err := pl.Advance(schema.PhaseDrive); err != nil {
		t.Fatalf("Advance: %v", err)
	}

	// Nest into Perturb
	if err := pl.Nest(schema.PhasePerturb); err != nil {
		t.Fatalf("Nest: %v", err)
	}
	if pl.Current() != schema.PhasePerturb {
		t.Errorf("pl.Current() = %s, want %s", pl.Current(), schema.PhasePerturb)
	}

	// Unnest back to Drive
	if err := pl.Unnest(); err != nil {
		t.Fatalf("Unnest: %v", err)
	}
	if pl.Current() != schema.PhaseDrive {
		t.Errorf("pl.Current() = %s, want %s", pl.Current(), schema.PhaseDrive)
	}

	str := out.String()
	if !strings.Contains(str, `"phase":"BOOT"`) || !strings.Contains(str, `"phase":"PERTURB"`) {
		t.Errorf("Missing expected phases in log: %s", str)
	}
}

// TestPerturbNestsInsideDriveInTheWorldFile pins the CALL SITE, not the API.
//
// PhaseLog.Nest always behaved correctly: TestPhaseLogNestUnnest above passes
// with or without the defect, because the defect was never in PhaseLog. runWorld
// reached for Advance, which CLOSES the open primary phase, so DRIVE ended the
// instant PERTURB began. Worlds written that way record a DRIVE window that stops
// where PERTURB starts instead of spanning it, while the driver in fact runs
// through PERTURB and on into HEAL; a reader of phases.json alone would conclude
// no traffic was driven. It also moves attribution: schema.PhaseTimings.At returns
// every covering window and the oracle engine takes the lowest lifecycle ordinal,
// so a finding timestamped mid-PERTURB is labelled PERTURB when DRIVE does not
// span it and DRIVE when it does.
//
// Reverting runner.go to Advance(PhasePerturb) must fail this test; verified by
// mutation, which reported "DRIVE is 0..0, a zero-width window, while PERTURB ran
// 0..60".
func TestPerturbNestsInsideDriveInTheWorldFile(t *testing.T) {
	cfg := perturbTestConfig(t)
	r, _ := newPerturbTestRunner(t, cfg, []string{"proc.pause(kv-n1)@10..60"}, newControlFakeInjector())

	if _, _, err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(r.runDir, "world-0001", "world.thesis"))
	if err != nil {
		t.Fatalf("read world file: %v", err)
	}
	w, err := schema.UnmarshalWorld(data)
	if err != nil {
		t.Fatalf("world file is not canonical: %v", err)
	}

	drive, ok := w.PhaseTimings.Lookup(schema.PhaseDrive)
	if !ok {
		t.Fatalf("the world file records no DRIVE window: %+v", w.PhaseTimings)
	}
	perturb, ok := w.PhaseTimings.Lookup(schema.PhasePerturb)
	if !ok {
		t.Fatalf("the world file records no PERTURB window: %+v", w.PhaseTimings)
	}

	if drive.DurationMS() <= 0 {
		t.Errorf("DRIVE is %d..%d, a zero-width window, while PERTURB ran %d..%d: "+
			"the driver ran but the world file says the drive phase had no extent",
			drive.StartMS, drive.EndMS, perturb.StartMS, perturb.EndMS)
	}
	if perturb.StartMS < drive.StartMS || perturb.EndMS > drive.EndMS {
		t.Errorf("PERTURB %d..%d is not contained in DRIVE %d..%d, but directive 4.1 step 4 "+
			"and pkg/schema/phase.go both say PERTURB overlaps DRIVE",
			perturb.StartMS, perturb.EndMS, drive.StartMS, drive.EndMS)
	}
}
