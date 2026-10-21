package main

import (
	"testing"

	"github.com/WrdCstlg/pro-thesis/internal/replay"
	"github.com/WrdCstlg/pro-thesis/internal/shrink"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// A shrink's ddmin result used to reach no verdict: minimal_repro was null and
// shrink.attempted false in every verdict.json, and shrink.Result.Apply had no
// caller outside its tests. A shrink now writes a verdict in its own bundle
// whose violation carries the reduction and the minimal reproduction.
func TestAShrinkVerdictCarriesTheMinimalRepro(t *testing.T) {
	res := &shrink.Result{
		Attempted: true, Complete: true,
		FaultsBefore: 3, FaultsAfter: 1, OpsBefore: 500, OpsAfter: 12,
		SurvivingFaults: []string{"net.partition(kv-n1)@3000..7500"},
		Confirmation:    shrink.Confirmation{K: 3, Trials: 3, Reproduced: 3},
		WorldPath:       ".prothesis/runs/r_2026_10_21_0002/world-0009/world.thesis",
	}
	original := shrink.Identity{Oracle: "linearizable.kv", Class: schema.ClassConsistency, Key: "k/0"}
	meta := replay.VerdictMeta{RunID: "r_2026_10_21_0002", Profile: "linear", Commit: "abc1234",
		Artifacts: ".prothesis/runs/r_2026_10_21_0002/"}

	v := shrinkVerdict(res, original, meta, ".prothesis/regressions/w_ab12.thesis")
	if err := v.Validate(); err != nil {
		t.Fatalf("the shrink verdict does not validate: %v", err)
	}
	if v.Verdict != schema.VerdictFail || len(v.Violations) != 1 {
		t.Fatalf("verdict %s with %d violation(s), want FAIL with the reproduced one", v.Verdict, len(v.Violations))
	}
	vi := v.Violations[0]
	if !vi.Shrink.Attempted || vi.Shrink.FaultsAfter != 1 || vi.Shrink.OpsAfter != 12 {
		t.Fatalf("shrink = %+v, want the measured reduction", vi.Shrink)
	}
	if vi.MinimalRepro == nil || vi.MinimalRepro.World != ".prothesis/regressions/w_ab12.thesis" || vi.MinimalRepro.Reproduced != "3/3" {
		t.Fatalf("minimal_repro = %+v, want the committed world reproduced 3/3", vi.MinimalRepro)
	}

	// Nothing reproduced: no violation is claimed and nothing points at a repro.
	none := &shrink.Result{Attempted: true, Stop: shrink.StopBaselineNotReproduced}
	nv := shrinkVerdict(none, original, meta, "")
	if nv.Verdict != schema.VerdictInconclusive || len(nv.Violations) != 0 {
		t.Fatalf("a shrink whose violation never reproduced: verdict %s, %d violation(s)", nv.Verdict, len(nv.Violations))
	}
}
