package schema

import (
	"fmt"
	"regexp"
	"sort"

	"gopkg.in/yaml.v3"
)

// KnownProblemsSchema is the schema identifier for .prothesis/known-problems.yaml.
const KnownProblemsSchema = "prothesis.knownproblems/v1"

// Known-problem classifications. The classification says what kind of cause the
// entry is; it never changes a recorded verdict.
const (
	KPAcceptedRefusal = "accepted-refusal" // an honest refusal to judge; counted, never "fixed"
	KPByDesign        = "by-design"        // a documented, intended behaviour (e.g. D-059)
	KPDefectFixed     = "defect-fixed"     // a harness defect with a landed fix
	KPDefectOpen      = "defect-open"      // a harness defect without a landed fix
	KPOperational     = "operational"      // operator/environment events (interrupts, aborts)
)

// Known-problem levels.
const (
	KPLevelWorld  = "world"
	KPLevelBundle = "bundle"
)

var kpClassifications = map[string]bool{
	KPAcceptedRefusal: true,
	KPByDesign:        true,
	KPDefectFixed:     true,
	KPDefectOpen:      true,
	KPOperational:     true,
}

var kpIDPattern = regexp.MustCompile(`^KP-\d{3}$`)
var kpLedgerPattern = regexp.MustCompile(`^(?:OQ|D)-\d{3}[a-z]?$`)

// KnownProblemMatch is a conjunctive matcher: every field that is set must
// match the fact set of the item under diagnosis. World-level entries may set
// Outcome, Oracle, ReasonRegex, Legacy and NoResult; bundle-level entries may
// set Verdict, Narrowed, ZeroWorlds, MissingResults, Interrupted and
// Unrecorded. The validator rejects cross-level fields and entries that set
// no predicate at all. Registry order is precedence order: the first entry
// whose predicates all match wins, so specific entries precede fallbacks.
type KnownProblemMatch struct {
	// world level
	Outcome     string `yaml:"outcome,omitempty"`
	Oracle      string `yaml:"oracle,omitempty"`
	ReasonRegex string `yaml:"reason_regex,omitempty"`
	Legacy      *bool  `yaml:"legacy,omitempty"`    // result.json predates the oracles[] schema
	NoResult    *bool  `yaml:"no_result,omitempty"` // the world died before result.json was written

	// bundle level
	Verdict        string `yaml:"verdict,omitempty"`
	Narrowed       *bool  `yaml:"narrowed,omitempty"`
	ZeroWorlds     *bool  `yaml:"zero_worlds,omitempty"`
	MissingResults *bool  `yaml:"missing_results,omitempty"` // a world dir without result.json
	Interrupted    *bool  `yaml:"interrupted,omitempty"`     // no verdict.json, at least one result.json
	Unrecorded     *bool  `yaml:"unrecorded,omitempty"`      // no verdict.json, no result.json anywhere
}

// KnownProblem is one catalogued cause. Ledger must cite an OQ- or D- entry;
// the ledger citation test enforces that it resolves.
type KnownProblem struct {
	ID             string            `yaml:"id"`
	Title          string            `yaml:"title"`
	Classification string            `yaml:"classification"`
	Ledger         string            `yaml:"ledger"`
	Level          string            `yaml:"level"`
	Match          KnownProblemMatch `yaml:"match"`
}

// KnownProblems is the registry document.
type KnownProblems struct {
	Schema   string         `yaml:"schema"`
	Problems []KnownProblem `yaml:"problems"`
}

// DecodeKnownProblems parses a known-problems registry.
func DecodeKnownProblems(data []byte) (*KnownProblems, error) {
	var kp KnownProblems
	if err := yaml.Unmarshal(data, &kp); err != nil {
		return nil, fmt.Errorf("known-problems registry: %w", err)
	}
	return &kp, nil
}

// Validate checks the registry for structural soundness. It does not check
// that ledger citations resolve; internal/ledger's citation test does that
// for the committed file.
func (kp *KnownProblems) Validate() error {
	var errs ValidationErrors
	if kp.Schema != KnownProblemsSchema {
		errs.Add("schema", "must be %q", KnownProblemsSchema)
	}
	seen := map[string]bool{}
	for i, p := range kp.Problems {
		path := indexPath("problems", i)
		if !kpIDPattern.MatchString(p.ID) {
			errs.Add(path+".id", "must look like KP-001, got %q", p.ID)
		}
		if seen[p.ID] {
			errs.Add(path+".id", "duplicate id %s", p.ID)
		}
		seen[p.ID] = true
		if p.Title == "" {
			errs.Add(path+".title", "required")
		}
		if !kpClassifications[p.Classification] {
			errs.Add(path+".classification", "unknown classification %q", p.Classification)
		}
		if !kpLedgerPattern.MatchString(p.Ledger) {
			errs.Add(path+".ledger", "must cite an OQ- or D- ledger entry, got %q", p.Ledger)
		}
		m := p.Match
		switch p.Level {
		case KPLevelWorld:
			if m.Verdict != "" || m.Narrowed != nil || m.ZeroWorlds != nil ||
				m.MissingResults != nil || m.Interrupted != nil || m.Unrecorded != nil {
				errs.Add(path+".match", "world-level entry sets a bundle-level predicate")
			}
			if m.Outcome == "" && m.Oracle == "" && m.ReasonRegex == "" && m.Legacy == nil && m.NoResult == nil {
				errs.Add(path+".match", "world-level entry sets no predicate; it would match everything")
			}
		case KPLevelBundle:
			if m.Outcome != "" || m.Oracle != "" || m.ReasonRegex != "" || m.Legacy != nil || m.NoResult != nil {
				errs.Add(path+".match", "bundle-level entry sets a world-level predicate")
			}
			if m.Verdict == "" && m.Narrowed == nil && m.ZeroWorlds == nil &&
				m.MissingResults == nil && m.Interrupted == nil && m.Unrecorded == nil {
				errs.Add(path+".match", "bundle-level entry sets no predicate; it would match everything")
			}
		default:
			errs.Add(path+".level", "must be %q or %q, got %q", KPLevelWorld, KPLevelBundle, p.Level)
		}
		if m.ReasonRegex != "" {
			if _, err := regexp.Compile(m.ReasonRegex); err != nil {
				errs.Add(path+".match.reason_regex", "does not compile: %s", err)
			}
		}
	}
	if len(errs) > 0 {
		sort.Slice(errs, func(i, j int) bool { return errs[i].Path < errs[j].Path })
		return errs
	}
	return nil
}
