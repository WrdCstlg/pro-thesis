package replay

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/WrdCstlg/pro-thesis/internal/control"
	"github.com/WrdCstlg/pro-thesis/internal/driver"
	"github.com/WrdCstlg/pro-thesis/internal/shrink"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// LoadRecordedPlan finds the driver plan a world recorded and checks it is the
// workload the world names.
//
// A v2 world carries plan_digest, the digest of the workload it drove
// (driver.Plan.WorkloadDigest). Its plan is the plan.json beside a run's
// world.thesis, or <name>.plan.json beside a corpus entry <name>.thesis. A plan
// whose workload digest differs from the world's, or no plan at all, is an
// error: replaying without it would drive a workload regenerated from whatever
// the config says today, which is not the world being replayed.
//
// A world that recorded no digest (every v1 world) returns a nil plan and a
// note: the workload is regenerated from the config, as it always was for such
// a world.
func LoadRecordedPlan(worldPath string, w *schema.World) (*driver.Plan, string, error) {
	plan, _, note, err := loadRecordedPlan(worldPath, w)
	return plan, note, err
}

// loadRecordedPlan is LoadRecordedPlan, also returning the file it read.
func loadRecordedPlan(worldPath string, w *schema.World) (*driver.Plan, string, string, error) {
	if w == nil {
		return nil, "", "", errors.New("replay: LoadRecordedPlan(nil world)")
	}
	if w.PlanDigest == "" {
		return nil, "", "the world records no plan_digest (" + w.Schema + "), so the workload is regenerated " +
			"from the current config and seed", nil
	}
	var tried []string
	for _, p := range recordedPlanPaths(worldPath) {
		tried = append(tried, p)
		raw, err := os.ReadFile(p)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, "", "", fmt.Errorf("replay: read recorded plan %s: %w", p, err)
		}
		plan, err := driver.ParsePlan(raw)
		if err != nil {
			return nil, "", "", fmt.Errorf("replay: recorded plan %s: %w", p, err)
		}
		got, err := plan.WorkloadDigest()
		if err != nil {
			return nil, "", "", fmt.Errorf("replay: digest recorded plan %s: %w", p, err)
		}
		if got != w.PlanDigest {
			return nil, "", "", fmt.Errorf("replay: the plan at %s drives workload %s, but the world's plan_digest "+
				"is %s; it is not the plan this world ran", p, got, w.PlanDigest)
		}
		return plan, p, "drives the recorded plan " + p + " (workload digest matches plan_digest)", nil
	}
	return nil, "", "", fmt.Errorf("replay: the world names its workload (plan_digest %s) but no recorded plan "+
		"was found (looked for %s); replaying without it would drive a different workload",
		w.PlanDigest, strings.Join(tried, ", "))
}

// Where a replayed workload came from.
const (
	PlanSupplied    = "supplied"
	PlanRecorded    = "recorded"
	PlanRegenerated = "regenerated"
)

// PlanChoice is the workload a replay or shrink drives.
type PlanChoice struct {
	// Plan is nil only when the workload is regenerated from the config (a
	// world that recorded no plan_digest, and no plan supplied).
	Plan *driver.Plan
	// Source is PlanSupplied, PlanRecorded or PlanRegenerated.
	Source string
	// Path is the plan file, when there is one.
	Path string
	// Workload is the digest of a supplied plan's workload when it is not the
	// workload the world recorded. The run then replays the world's schedule
	// under a different workload, and says so.
	Workload string
	// Note is one line for a human.
	Note string
}

// ChoosePlan resolves the workload a replay or shrink drives. A supplied plan
// (--plan) is driven exactly; without one, the world's recorded plan
// (LoadRecordedPlan). A supplied plan that cannot be used is the caller's
// argument error; a recorded plan that cannot be found is the world's.
func ChoosePlan(worldPath string, w *schema.World, supplied string) (PlanChoice, error) {
	if w == nil {
		return PlanChoice{}, errors.New("replay: ChoosePlan(nil world)")
	}
	if supplied == "" {
		plan, path, note, err := loadRecordedPlan(worldPath, w)
		if err != nil {
			return PlanChoice{}, err
		}
		c := PlanChoice{Plan: plan, Source: PlanRegenerated, Path: path, Note: note}
		if plan != nil {
			c.Source = PlanRecorded
		}
		return c, nil
	}
	plan, err := driver.LoadSuppliedPlan(supplied, w.DriverProfile, w.Seed)
	if err != nil {
		return PlanChoice{}, &SuppliedPlanError{Err: err}
	}
	digest, err := plan.WorkloadDigest()
	if err != nil {
		return PlanChoice{}, &SuppliedPlanError{Err: err}
	}
	c := PlanChoice{Plan: plan, Source: PlanSupplied, Path: supplied,
		Note: fmt.Sprintf("drives the supplied plan %s exactly (%d operation(s), workload %s)",
			supplied, len(plan.Operations), digest)}
	if digest != w.PlanDigest {
		c.Workload = digest
		recorded := "none"
		if w.PlanDigest != "" {
			recorded = w.PlanDigest
		}
		c.Note += fmt.Sprintf("; the world recorded workload %s, so this is the world's schedule under "+
			"the supplied workload, not the recorded world", recorded)
	}
	return c, nil
}

// SuppliedPlanError is a --plan that cannot be driven: an argument error.
type SuppliedPlanError struct{ Err error }

func (e *SuppliedPlanError) Error() string { return e.Err.Error() }
func (e *SuppliedPlanError) Unwrap() error { return e.Err }

func recordedPlanPaths(worldPath string) []string {
	dir := filepath.Dir(worldPath)
	base := filepath.Base(worldPath)
	if base == "world.thesis" {
		return []string{filepath.Join(dir, driver.PlanFileName)}
	}
	return []string{
		filepath.Join(dir, strings.TrimSuffix(base, schema.WorldFileExt)+".plan.json"),
		filepath.Join(dir, driver.PlanFileName),
	}
}

// VerdictMeta is what a replay verdict carries that the report does not know.
type VerdictMeta struct {
	RunID      string
	Profile    string
	Commit     string
	Artifacts  string
	OracleLock schema.OracleLock
}

// Verdict renders the report as a prothesis.verdict/v1 document with a trials
// record, so `thesis replay -k` leaves a machine-readable result in its run
// bundle as `thesis run` does. Its verdict agrees with the report's exit code.
func (r Report) Verdict(m VerdictMeta) schema.Verdict {
	v := schema.Verdict{
		Schema:     schema.VerdictSchema,
		RunID:      m.RunID,
		Profile:    m.Profile,
		Commit:     m.Commit,
		Violations: []schema.Violation{},
		OracleLock: m.OracleLock,
		Artifacts:  m.Artifacts,
		Budget: schema.Budget{
			UsedS:         int64(r.Elapsed.Round(time.Second).Seconds()),
			WorldsPlanned: r.Planned,
			WorldsRun:     len(r.Attempts),
		},
	}
	switch r.ExitCode() {
	case schema.ExitFail:
		v.Verdict = schema.VerdictFail
	case schema.ExitBudgetExhausted:
		v.Verdict = schema.VerdictBudgetExhausted
	case schema.ExitPass:
		v.Verdict = schema.VerdictPass
	default:
		v.Verdict = schema.VerdictInconclusive
	}
	t := &schema.Trials{World: r.WorldHash, Workload: r.Workload}
	n := 1
	for _, a := range r.Attempts {
		t.Attempts++
		switch a.Signal {
		case SignalReproduced:
			t.Reproduced++
			for _, id := range a.Found {
				if a.isDivergent(id) {
					continue
				}
				v.Violations = append(v.Violations, schema.Violation{
					ID:          control.ViolationID(n),
					Oracle:      id.Oracle,
					Class:       id.Class,
					Explanation: fmt.Sprintf("reproduced in attempt %d of %d", a.N, r.Planned),
					Witness:     schema.Witness{Key: id.Key},
				})
				n++
			}
		case SignalNotReproduced:
			t.NotReproduced++
		case SignalDifferent:
			t.Different++
		default:
			t.Inconclusive++
		}
	}
	v.Trials = t
	v.Normalize()
	return v
}

// isDivergent reports whether id is one of this attempt's divergent
// violations, compared as Report.Divergences compares them.
func (a Attempt) isDivergent(id shrink.Identity) bool {
	for _, d := range a.Divergent {
		if d.Oracle == id.Oracle && d.Class == id.Class && d.Key == id.Key {
			return true
		}
	}
	return false
}
