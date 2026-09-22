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
// id for a non-terminal outcome that matched a registry entry; Unattributed
// marks a non-terminal outcome that matched nothing; a terminal outcome
// (pass|violation) carries neither. Exactly one of {terminal, KP != "",
// Unattributed} holds per world.
type WorldFact struct {
	Name         string
	HasResult    bool
	Outcome      string
	Legacy       bool
	Oracles      []OracleFact
	ExitCode     int
	HasTelemetry bool
	DurationMs   float64 // from search.json when recorded, else 0
	KP           string
	Unattributed bool
}

// BundleFact is one run directory's scan result, with the same attribution
// tri-state as WorldFact plus the run's verdict and profile ("" when no
// verdict.json exists).
type BundleFact struct {
	Name         string
	Verdict      string
	Profile      string
	KP           string
	Unattributed bool
	Worlds       []WorldFact
}

// Scan walks runsDir exactly as Diagnose does and returns every bundle's
// facts with per-item attribution. Bundles come back in directory order
// (sorted), worlds in world-dir order (sorted): the output order is
// deterministic given the corpus.
func Scan(runsDir string, reg *schema.KnownProblems) ([]BundleFact, error) {
	if err := reg.Validate(); err != nil {
		return nil, err
	}
	worldKPs, bundleKPs := splitByLevel(reg)

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
		b.Verdict = bf.verdict
		b.Profile = bf.profile

		for _, w := range worlds {
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
			if w.hasResult && (w.outcome == "pass" || w.outcome == "violation") {
				b.Worlds = append(b.Worlds, wf)
				continue
			}
			if id := matchWorld(worldKPs, w); id != "" {
				wf.KP = id
			} else {
				wf.Unattributed = true
			}
			b.Worlds = append(b.Worlds, wf)
		}

		if bf.verdict != "PASS" && bf.verdict != "FAIL" {
			if id := matchBundle(bundleKPs, bf); id != "" {
				b.KP = id
			} else {
				b.Unattributed = true
			}
		}
		out = append(out, b)
	}
	return out, nil
}
