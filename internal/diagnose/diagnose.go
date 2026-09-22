// Package diagnose attributes every recorded non-terminal outcome in a run
// corpus to a catalogued known problem. It answers the question "do we know
// WHY every piece of unjudged evidence was not judged" without changing a
// single recorded byte: the corpus is read-only to this package.
//
// The refusal rule (OQ-072): an item that matches no registry entry is
// UNATTRIBUTED, and unattributed items make the diagnostic fail loudly.
// "Nothing to attribute" and "we failed to attribute it" must never look
// alike. Registry order is precedence order: the first entry whose predicates
// all match wins, so specific entries precede fallbacks and there is no
// catch-all - a catch-all would make unattributed silence impossible to see.
package diagnose

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// Item is one thing that needed attribution but matched nothing.
type Item struct {
	Level  string // world | bundle
	Bundle string // run directory name
	Detail string // what was seen, for the human reader
}

// Count is one known problem's attribution tally.
type Count struct {
	ID             string
	Title          string
	Classification string
	N              int
}

// Report is the full attribution pass.
type Report struct {
	Bundles      int     // run directories examined (excluding .trash)
	Worlds       int     // world-* directories examined
	Counts       []Count // per known problem with at least one hit, sorted by id
	Unattributed []Item  // empty means the corpus is fully attributed
}

// Diagnose walks runsDir and attributes every non-terminal world outcome and
// every non-terminal or absent run verdict to a registry entry.
func Diagnose(runsDir string, reg *schema.KnownProblems) (*Report, error) {
	if err := reg.Validate(); err != nil {
		return nil, err
	}
	worldKPs, bundleKPs := splitByLevel(reg)

	entries, err := os.ReadDir(runsDir)
	if err != nil {
		return nil, fmt.Errorf("reading runs dir: %w", err)
	}

	rep := &Report{}
	counts := map[string]int{}

	for _, e := range entries {
		if !e.IsDir() || e.Name() == ".trash" || !strings.HasPrefix(e.Name(), "r_") {
			continue
		}
		rep.Bundles++
		bf, worlds, err := readBundle(filepath.Join(runsDir, e.Name()))
		if err != nil {
			rep.Unattributed = append(rep.Unattributed, Item{
				Level: "bundle", Bundle: e.Name(),
				Detail: "unreadable bundle: " + err.Error(),
			})
			continue
		}

		// World level: every recorded outcome that is not pass|violation must
		// attribute, and so must every world dir with no result.json at all.
		for _, w := range worlds {
			rep.Worlds++
			if w.hasResult && (w.outcome == "pass" || w.outcome == "violation") {
				continue
			}
			id := matchWorld(worldKPs, w)
			if id == "" {
				rep.Unattributed = append(rep.Unattributed, Item{
					Level: "world", Bundle: e.Name(),
					Detail: fmt.Sprintf("%s: outcome=%q hasResult=%v with no matching known problem",
						w.name, w.outcome, w.hasResult),
				})
				continue
			}
			counts[id]++
		}

		// Bundle level: terminal verdicts need no attribution.
		if bf.verdict == "PASS" || bf.verdict == "FAIL" {
			continue
		}
		id := matchBundle(bundleKPs, bf)
		if id == "" {
			rep.Unattributed = append(rep.Unattributed, Item{
				Level: "bundle", Bundle: e.Name(),
				Detail: fmt.Sprintf("verdict=%q narrowed=%v zeroWorlds=%v missingResults=%v interrupted=%v unrecorded=%v",
					bf.verdict, bf.narrowed, bf.zeroWorlds, bf.missingResults, bf.interrupted, bf.unrecorded),
			})
			continue
		}
		counts[id]++
	}

	for _, p := range reg.Problems {
		if counts[p.ID] == 0 {
			continue
		}
		rep.Counts = append(rep.Counts, Count{
			ID: p.ID, Title: p.Title, Classification: p.Classification, N: counts[p.ID],
		})
	}
	sort.Slice(rep.Counts, func(i, j int) bool { return rep.Counts[i].ID < rep.Counts[j].ID })
	sort.Slice(rep.Unattributed, func(i, j int) bool {
		if rep.Unattributed[i].Bundle != rep.Unattributed[j].Bundle {
			return rep.Unattributed[i].Bundle < rep.Unattributed[j].Bundle
		}
		return rep.Unattributed[i].Detail < rep.Unattributed[j].Detail
	})
	return rep, nil
}

func splitByLevel(reg *schema.KnownProblems) (world, bundle []schema.KnownProblem) {
	for _, p := range reg.Problems {
		if p.Level == schema.KPLevelWorld {
			world = append(world, p)
		} else {
			bundle = append(bundle, p)
		}
	}
	return world, bundle
}

type worldScan struct {
	name      string
	hasResult bool
	outcome   string
	legacy    bool // result.json predates the oracles[] schema
	reasons   []oracleReason
}

type oracleReason struct {
	oracle string
	reason string
}

func matchWorld(kps []schema.KnownProblem, w worldScan) string {
	for _, p := range kps {
		m := p.Match
		if m.NoResult != nil && *m.NoResult != !w.hasResult {
			continue
		}
		if m.Legacy != nil && *m.Legacy != w.legacy {
			continue
		}
		if m.Outcome != "" && m.Outcome != w.outcome {
			continue
		}
		if m.Oracle == "" && m.ReasonRegex == "" {
			return p.ID // outcome/legacy/no_result-only entry
		}
		for _, o := range w.reasons {
			if m.Oracle != "" && m.Oracle != o.oracle {
				continue
			}
			if m.ReasonRegex != "" && !regexp.MustCompile(m.ReasonRegex).MatchString(o.reason) {
				continue // compiled already in Validate
			}
			return p.ID
		}
	}
	return ""
}

type bundleFacts struct {
	verdict        string // "" when no verdict.json
	narrowed       bool
	zeroWorlds     bool
	missingResults bool // at least one world dir without result.json
	interrupted    bool // no verdict.json, at least one result.json
	unrecorded     bool // no verdict.json, no result.json anywhere
}

func matchBundle(kps []schema.KnownProblem, bf bundleFacts) string {
	for _, p := range kps {
		m := p.Match
		if m.Verdict != "" && m.Verdict != bf.verdict {
			continue
		}
		if m.Narrowed != nil && *m.Narrowed != bf.narrowed {
			continue
		}
		if m.ZeroWorlds != nil && *m.ZeroWorlds != bf.zeroWorlds {
			continue
		}
		if m.MissingResults != nil && *m.MissingResults != bf.missingResults {
			continue
		}
		if m.Interrupted != nil && *m.Interrupted != bf.interrupted {
			continue
		}
		if m.Unrecorded != nil && *m.Unrecorded != bf.unrecorded {
			continue
		}
		return p.ID
	}
	return ""
}

// readBundle reads a run directory's verdict.json and world-*/result.json
// files into fact sets. It never writes.
func readBundle(dir string) (bundleFacts, []worldScan, error) {
	var bf bundleFacts

	entries, err := os.ReadDir(dir)
	if err != nil {
		return bf, nil, err
	}
	var worldDirs []string
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), "world-") {
			worldDirs = append(worldDirs, e.Name())
		}
	}
	sort.Strings(worldDirs)
	bf.zeroWorlds = len(worldDirs) == 0

	vpath := filepath.Join(dir, "verdict.json")
	if fileExists(vpath) {
		data, err := os.ReadFile(vpath)
		if err != nil {
			return bf, nil, err
		}
		var v struct {
			Verdict string `json:"verdict"`
			Budget  struct {
				Narrowed bool `json:"narrowed"`
			} `json:"budget"`
		}
		if err := json.Unmarshal(data, &v); err != nil {
			return bf, nil, fmt.Errorf("verdict.json: %w", err)
		}
		bf.verdict = v.Verdict
		bf.narrowed = v.Budget.Narrowed
	}

	hasResults := false
	var worlds []worldScan
	for _, w := range worldDirs {
		ws := worldScan{name: w}
		rpath := filepath.Join(dir, w, "result.json")
		if fileExists(rpath) {
			data, err := os.ReadFile(rpath)
			if err != nil {
				return bf, nil, err
			}
			var r struct {
				Outcome string `json:"outcome"`
				Oracles []struct {
					Oracle      string `json:"oracle"`
					Status      string `json:"status"`
					Explanation string `json:"explanation"`
					Error       string `json:"error"`
				} `json:"oracles"`
			}
			if err := json.Unmarshal(data, &r); err != nil {
				return bf, nil, fmt.Errorf("%s/result.json: %w", w, err)
			}
			ws.hasResult = true
			ws.outcome = r.Outcome
			ws.legacy = r.Oracles == nil
			for _, o := range r.Oracles {
				reason := o.Explanation
				if o.Error != "" {
					reason += " " + o.Error
				}
				ws.reasons = append(ws.reasons, oracleReason{oracle: o.Oracle, reason: reason})
			}
			hasResults = true
		} else {
			bf.missingResults = true
		}
		worlds = append(worlds, ws)
	}
	bf.interrupted = bf.verdict == "" && hasResults
	bf.unrecorded = bf.verdict == "" && !hasResults
	return bf, worlds, nil
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}
