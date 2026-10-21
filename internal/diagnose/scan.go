// scan.go exposes the corpus walk as structured facts for read-only
// consumers (today: internal/cluster). It is ADDITIVE to this package: the
// walk, the matchers and Diagnose's report are untouched, and the
// attribution ids Scan reports come from the same matchWorld/matchBundle
// calls Diagnose makes, so the two can never disagree. The corpus stays
// read-only here exactly as it is for Diagnose.
package diagnose

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// OracleFact is one oracle's recorded judgement on a world.
type OracleFact struct {
	Oracle      string
	Status      string
	Explanation string
	Error       string
}

// WorldFact is one world directory's scan result. KP holds the attribution
// id for an outcome that needed attribution and matched a registry entry;
// Unattributed marks one that matched nothing; a terminal outcome
// (pass|violation) carries neither, unless it is a violating world of a false
// rejection (FalseRejection), which needs attribution like an unjudged one.
// Exactly one of {terminal, KP != "", Unattributed} holds per world.
type WorldFact struct {
	Name           string
	HasResult      bool
	Outcome        string
	Legacy         bool
	Oracles        []OracleFact
	ExitCode       int
	HasTelemetry   bool
	DurationMs     float64 // from search.json when recorded, else 0
	KP             string
	Unattributed   bool
	FalseRejection bool
}

// BundleFact is one run directory's scan result, with the same attribution
// tri-state as WorldFact plus the run's verdict and profile ("" when no
// verdict.json exists). FalseRejection marks a run expected to PASS that came
// back FAIL; its KP/Unattributed are set only when it has no violating world
// to carry the false rejection. FalseRejectionUnexplained marks such a run
// with an item nothing explains.
type BundleFact struct {
	Name                      string
	Verdict                   string
	Profile                   string
	Arm                       string
	Expected                  string
	KP                        string
	Unattributed              bool
	FalseRejection            bool
	FalseRejectionUnexplained bool
	Worlds                    []WorldFact
}

// Scan walks runsDir exactly as Diagnose does and returns every bundle's
// facts with per-item attribution. Bundles come back in directory order
// (sorted), worlds in world-dir order (sorted): the output order is
// deterministic given the corpus.
func Scan(runsDir string, reg *schema.KnownProblems) ([]BundleFact, error) {
	return ScanWith(runsDir, reg, Options{})
}

// FalseRejectionRates is DiagnoseWith's per-arm false-rejection tally, over
// scanned bundles.
func FalseRejectionRates(bundles []BundleFact) []FalseRejectionArm {
	runs := make([]frRun, 0, len(bundles))
	for _, b := range bundles {
		runs = append(runs, newFRRun(b.Arm, b.Expected, b.Verdict, b.FalseRejectionUnexplained))
	}
	return tallyFalseRejections(runs)
}

// ScanWith is Scan with Options.
func ScanWith(runsDir string, reg *schema.KnownProblems, opts Options) ([]BundleFact, error) {
	if err := reg.Validate(); err != nil {
		return nil, err
	}
	worldKPs, bundleKPs := splitByLevel(reg)
	recs := opts.records()

	entries, err := os.ReadDir(runsDir)
	if err != nil {
		return nil, fmt.Errorf("reading runs dir: %w", err)
	}

	var out []BundleFact
	for _, e := range entries {
		if !e.IsDir() || e.Name() == ".trash" || !strings.HasPrefix(e.Name(), "r_") {
			continue
		}
		bf, worlds, err := readBundle(filepath.Join(runsDir, e.Name()))
		b := BundleFact{Name: e.Name()}
		if err != nil {
			// Same refusal Diagnose reports: an unreadable bundle is
			// unattributed, not silently skipped.
			b.Unattributed = true
			out = append(out, b)
			continue
		}
		bf.applyCalibration(recs[e.Name()])
		av := attribute(bf, worlds, worldKPs, bundleKPs)
		b.Verdict = bf.verdict
		b.Profile = bf.profile
		b.Arm = bf.arm
		b.Expected = bf.expected
		b.FalseRejection = bf.falseRejection
		b.FalseRejectionUnexplained = av.unexplainedFR

		for i, w := range worlds {
			wf := WorldFact{
				Name:         w.name,
				HasResult:    w.hasResult,
				Outcome:      w.outcome,
				Legacy:       w.legacy,
				ExitCode:     w.exitCode,
				HasTelemetry: w.hasTelemetry,
				DurationMs:   w.durationMs,
			}
			for _, o := range w.reasons {
				wf.Oracles = append(wf.Oracles, OracleFact{
					Oracle: o.oracle, Status: o.status,
					Explanation: o.explanation, Error: o.errorText,
				})
			}
			wa := av.worlds[i]
			wf.FalseRejection = wa.falseRejection
			if !wa.terminal {
				wf.KP = wa.kp
				wf.Unattributed = wa.kp == ""
			}
			b.Worlds = append(b.Worlds, wf)
		}

		if !av.terminal {
			b.KP = av.kp
			b.Unattributed = av.kp == ""
		}
		out = append(out, b)
	}
	return out, nil
}
