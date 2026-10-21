package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/WrdCstlg/pro-thesis/internal/control"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// `thesis report` and `thesis gate`: two-sided calibration of the gate itself
// (D-109). Both read the evaluation manifest (eval/manifest.yaml)
// and every verdict.json under each arm's target that names an arm, classify
// each run against its arm's pole, and write calibration_report.json and
// calibration_report.md. `report` stops there; `gate` also fails on any escape
// and on any threshold breach.

const reportUsage = `usage: thesis report [flags]

Calibrate the gate against the evaluation manifest's arms: classify every run
made for an arm (TRUE_PASS, TRUE_REJECT, ESCAPE, FALSE_REJECTION, INCONCLUSIVE),
and report sensitivity, specificity, the false-rejection rate and the PPV of a
FAIL at several base rates. Writes calibration_report.json and .md.

Flags:
  --manifest PATH    the evaluation manifest (default: eval/manifest.yaml, found
                     by walking up from the current directory)
  --runs DIR         a runs directory to read (repeatable; default: each arm
                     target's .prothesis/runs under the manifest's repository)
  --out DIR          where to write the report (default: <manifest dir>/calibration)
  --base-rate F      a base rate to report PPV at (repeatable; default
                     0.20, 0.10, 0.042, 0.01)

Exit codes: 0 written; 2 no arm has a decided run; 5 bad flags or manifest.
`

const gateUsage = `usage: thesis gate [flags]

thesis report, then pass or fail the calibration: exit 1 on any ESCAPE (a
known defect that passed) and on any breached threshold.

Flags: the flags of thesis report, and
  --min-specificity F  fail when specificity is below F
  --min-ppv F          fail when the PPV of a FAIL at --ppv-base-rate is below F
  --ppv-base-rate F    the base rate --min-ppv is judged at (default 0.042)

Exit codes: 0 calibrated within its thresholds; 1 an escape or a breach; 2
nothing decided, or a threshold whose rate is unmeasured; 5 bad flags or manifest.
`

type calibrationFlags struct {
	manifest       string
	runs           []string
	out            string
	baseRates      []float64
	minSpecificity *float64
	minPPV         *float64
	ppvBaseRate    float64
}

func parseCalibrationFlags(verb string, args []string, gate bool) (calibrationFlags, bool) {
	f := calibrationFlags{ppvBaseRate: 0.042}
	value := func(i *int, a, name string) (string, bool) {
		if v, ok := strings.CutPrefix(a, name+"="); ok {
			return v, true
		}
		if *i+1 >= len(args) {
			errorf("%s: %s needs a value", verb, name)
			return "", false
		}
		*i++
		return args[*i], true
	}
	float := func(name, v string) (float64, bool) {
		x, err := strconv.ParseFloat(v, 64)
		if err != nil || x < 0 || x > 1 {
			errorf("%s: %s %q is not a number between 0 and 1", verb, name, v)
			return 0, false
		}
		return x, true
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		name := a
		if j := strings.IndexByte(a, '='); j > 0 {
			name = a[:j]
		}
		switch name {
		case "-h", "--help":
			if gate {
				fmt.Print(gateUsage)
			} else {
				fmt.Print(reportUsage)
			}
			return f, false
		case "--manifest", "--runs", "--out", "--base-rate", "--min-specificity", "--min-ppv", "--ppv-base-rate":
			if !gate && (name == "--min-specificity" || name == "--min-ppv" || name == "--ppv-base-rate") {
				errorf("%s: %s is a flag of thesis gate", verb, name)
				return f, false
			}
			v, ok := value(&i, a, name)
			if !ok {
				return f, false
			}
			switch name {
			case "--manifest":
				f.manifest = v
			case "--runs":
				f.runs = append(f.runs, v)
			case "--out":
				f.out = v
			default:
				x, ok := float(name, v)
				if !ok {
					return f, false
				}
				switch name {
				case "--base-rate":
					f.baseRates = append(f.baseRates, x)
				case "--min-specificity":
					f.minSpecificity = &x
				case "--min-ppv":
					f.minPPV = &x
				case "--ppv-base-rate":
					f.ppvBaseRate = x
				}
			}
		default:
			errorf("%s: unknown flag %q", verb, a)
			return f, false
		}
	}
	if len(f.baseRates) == 0 {
		f.baseRates = append([]float64(nil), schema.DefaultBaseRates...)
	}
	return f, true
}

// findEvalManifest walks up from dir to the first eval/manifest.yaml.
func findEvalManifest(dir string) string {
	for d := dir; ; {
		p := filepath.Join(d, "eval", "manifest.yaml")
		if _, err := os.Stat(p); err == nil {
			return p
		}
		parent := filepath.Dir(d)
		if parent == d {
			return ""
		}
		d = parent
	}
}

// loadEvalManifest resolves and parses the manifest, returning it with its
// absolute path. An empty path is found by walking up from the working
// directory.
func loadEvalManifest(path string) (*schema.EvalManifest, string, error) {
	if path == "" {
		wd, _ := os.Getwd()
		if path = findEvalManifest(wd); path == "" {
			return nil, "", errors.New("no eval/manifest.yaml here or in any parent directory; pass --manifest")
		}
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, "", err
	}
	raw, err := os.ReadFile(abs)
	if err != nil {
		return nil, "", err
	}
	m, err := schema.ParseEvalManifest(raw)
	if err != nil {
		return nil, "", err
	}
	return m, abs, nil
}

// collectVerdicts reads every <runs>/r_*/verdict.json. With no runs
// directories given, each arm target's .prothesis/runs under the manifest's
// repository (the directory holding eval/) is read.
func collectVerdicts(root string, m *schema.EvalManifest, runsDirs []string) ([]schema.LocatedVerdict, error) {
	if len(runsDirs) == 0 {
		seen := map[string]bool{}
		for _, a := range m.Arms {
			d := filepath.Join(root, filepath.FromSlash(a.Target), ".prothesis", "runs")
			if fi, err := os.Stat(d); err == nil && fi.IsDir() && !seen[d] {
				seen[d] = true
				runsDirs = append(runsDirs, d)
			}
		}
		sort.Strings(runsDirs)
	}
	var out []schema.LocatedVerdict
	for _, d := range runsDirs {
		ents, err := os.ReadDir(d)
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("runs directory %s does not exist", d)
		}
		if err != nil {
			return nil, err
		}
		for _, e := range ents {
			if !e.IsDir() || !strings.HasPrefix(e.Name(), "r_") {
				continue
			}
			p := filepath.Join(d, e.Name(), "verdict.json")
			raw, err := os.ReadFile(p)
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err != nil {
				return nil, err
			}
			var v schema.Verdict
			if err := json.Unmarshal(raw, &v); err != nil {
				return nil, fmt.Errorf("%s: %w", p, err)
			}
			rel, rerr := filepath.Rel(root, p)
			if rerr != nil {
				rel = p
			}
			out = append(out, schema.LocatedVerdict{Path: filepath.ToSlash(rel), Verdict: v})
		}
	}
	return out, nil
}

// runCalibration turns `thesis run`'s --arm / --manifest / --expect /
// --expect-oracle into the run's expectation. With --arm, the manifest arm
// supplies the pole, the target oracle and the default profile, and the run is
// refused when it is not that arm's: another project, another profile, or an
// --expect that contradicts the pole. It sets *profile when the arm supplies it.
func runCalibration(projectDir, arm, manifestPath, expect, expectOracle string, profile *string, profileSet bool) (*control.Calibration, schema.ExitCode) {
	if arm == "" && expect == "" {
		if manifestPath != "" || expectOracle != "" {
			errorf("run: --manifest and --expect-oracle need --arm or --expect")
			return nil, schema.ExitConfigError
		}
		return nil, schema.ExitPass
	}
	if expect != "" && expect != string(schema.VerdictPass) && expect != string(schema.VerdictFail) {
		errorf("run: --expect %q: want PASS or FAIL", expect)
		return nil, schema.ExitConfigError
	}
	cal := &control.Calibration{Expected: schema.VerdictResult(expect), TargetOracle: expectOracle}
	if arm == "" {
		return cal, schema.ExitPass
	}
	if manifestPath == "" {
		manifestPath = findEvalManifest(projectDir)
	}
	m, abs, err := loadEvalManifest(manifestPath)
	if err != nil {
		errorf("run: --arm %s: %v", arm, err)
		return nil, schema.ExitConfigError
	}
	a, ok := m.Arm(arm)
	if !ok {
		errorf("run: %s has no arm %q", abs, arm)
		return nil, schema.ExitConfigError
	}
	exp, ok := a.ExpectedVerdict()
	if !ok {
		errorf("run: arm %s has no PASS/FAIL pole in %s, so a run of it cannot be calibrated", arm, abs)
		return nil, schema.ExitConfigError
	}
	if expect != "" && schema.VerdictResult(expect) != exp {
		errorf("run: --expect %s contradicts arm %s, whose pole is %s", expect, arm, exp)
		return nil, schema.ExitConfigError
	}
	root := filepath.Dir(filepath.Dir(abs))
	if rel, err := filepath.Rel(root, projectDir); err != nil || filepath.ToSlash(rel) != a.Target {
		errorf("run: arm %s targets %s, but this project is %s", arm, a.Target, projectDir)
		return nil, schema.ExitConfigError
	}
	switch {
	case !profileSet && a.Profile != "":
		*profile = a.Profile
	case profileSet && a.Profile != "" && *profile != a.Profile:
		errorf("run: arm %s runs under profile %s, not %s", arm, a.Profile, *profile)
		return nil, schema.ExitConfigError
	}
	cal.Arm, cal.Expected = arm, exp
	if cal.TargetOracle == "" {
		cal.TargetOracle = a.TargetOracle
	}
	return cal, schema.ExitPass
}

// decided is how many runs landed on a pole either way.
func decided(c schema.CalibrationCounts) int {
	return c.TruePass + c.TrueReject + c.Escape + c.FalseRejection
}

func cmdReport(_ context.Context, g globals, args []string) schema.ExitCode {
	f, ok := parseCalibrationFlags("report", args, false)
	if !ok {
		return schema.ExitConfigError
	}
	rep, outDir, code := calibrate("report", f)
	if code != schema.ExitPass {
		return code
	}
	if err := writeCalibrationReport(g, rep, outDir); err != nil {
		errorf("report: %v", err)
		return schema.ExitInconclusive
	}
	if decided(rep.Aggregate) == 0 {
		return schema.ExitInconclusive
	}
	return schema.ExitPass
}

func cmdGate(_ context.Context, g globals, args []string) schema.ExitCode {
	f, ok := parseCalibrationFlags("gate", args, true)
	if !ok {
		return schema.ExitConfigError
	}
	rep, outDir, code := calibrate("gate", f)
	if code != schema.ExitPass {
		return code
	}
	th := &schema.GateThresholds{MinSpecificity: f.minSpecificity, MinPPV: f.minPPV}
	if f.minPPV != nil {
		th.PPVBaseRate = f.ppvBaseRate
	}
	rep.Thresholds = th

	// breach: a measured failure; unmeasured: a threshold that could not be
	// judged because its rate has no denominator. A breach outranks it.
	breach, unmeasured := false, false
	if rep.Aggregate.Escape > 0 {
		breach = true
		rep.Breaches = append(rep.Breaches, fmt.Sprintf("%d escape(s): a known defect passed the gate", rep.Aggregate.Escape))
	}
	if f.minSpecificity != nil {
		switch sp := rep.Rates.Specificity; {
		case sp == nil:
			unmeasured = true
			rep.Breaches = append(rep.Breaches, "min_specificity: specificity is unmeasured (no decided gold-positive run)")
		case *sp < *f.minSpecificity:
			breach = true
			rep.Breaches = append(rep.Breaches, fmt.Sprintf("specificity %.4f is below the floor %.4f", *sp, *f.minSpecificity))
		}
	}
	if f.minPPV != nil {
		p := schema.PPVByBaseRate(rep.Rates, []float64{f.ppvBaseRate})[0]
		switch {
		case p.PPV == nil:
			unmeasured = true
			rep.Breaches = append(rep.Breaches, fmt.Sprintf("min_ppv: the PPV at base rate %.3f is unmeasured", f.ppvBaseRate))
		case *p.PPV < *f.minPPV:
			breach = true
			rep.Breaches = append(rep.Breaches, fmt.Sprintf("PPV %.4f at base rate %.3f is below the floor %.4f",
				*p.PPV, f.ppvBaseRate, *f.minPPV))
		}
	}
	if err := writeCalibrationReport(g, rep, outDir); err != nil {
		errorf("gate: %v", err)
		return schema.ExitInconclusive
	}
	switch {
	case breach:
		return schema.ExitFail
	case decided(rep.Aggregate) == 0 || unmeasured:
		return schema.ExitInconclusive
	default:
		return schema.ExitPass
	}
}

// calibrate loads the manifest and verdicts and builds the report.
func calibrate(verb string, f calibrationFlags) (schema.CalibrationReport, string, schema.ExitCode) {
	m, manifestPath, err := loadEvalManifest(f.manifest)
	if err != nil {
		errorf("%s: %v", verb, err)
		return schema.CalibrationReport{}, "", schema.ExitConfigError
	}
	root := filepath.Dir(filepath.Dir(manifestPath))
	verdicts, err := collectVerdicts(root, m, f.runs)
	if err != nil {
		errorf("%s: %v", verb, err)
		return schema.CalibrationReport{}, "", schema.ExitConfigError
	}
	rel, rerr := filepath.Rel(root, manifestPath)
	if rerr != nil {
		rel = manifestPath
	}
	rep := schema.BuildCalibrationReport(filepath.ToSlash(rel), m, verdicts, f.baseRates)
	out := f.out
	if out == "" {
		out = filepath.Join(filepath.Dir(manifestPath), "calibration")
	}
	return rep, out, schema.ExitPass
}

func writeCalibrationReport(g globals, rep schema.CalibrationReport, outDir string) error {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if err := os.WriteFile(filepath.Join(outDir, "calibration_report.json"), b, 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(outDir, "calibration_report.md"), []byte(renderCalibrationMarkdown(rep)), 0o644); err != nil {
		return err
	}
	switch {
	case g.json:
		fmt.Print(string(b))
	case !g.quiet:
		fmt.Print(renderCalibrationMarkdown(rep))
		fmt.Printf("\nwritten: %s\n", filepath.Join(outDir, "calibration_report.{json,md}"))
	}
	return nil
}

func rate(p *float64) string {
	if p == nil {
		return "unmeasured"
	}
	return strconv.FormatFloat(*p, 'f', 4, 64)
}

func renderCalibrationMarkdown(rep schema.CalibrationReport) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Gate calibration\n\nManifest: `%s`\n\n", rep.Manifest)
	b.WriteString("| Arm | Pole | Expected | Runs | TRUE_PASS | TRUE_REJECT | ESCAPE | FALSE_REJECTION | INCONCLUSIVE | Sensitivity | Specificity |\n")
	b.WriteString("|---|---|---|---|---|---|---|---|---|---|---|\n")
	for _, a := range rep.Arms {
		c := a.Counts
		fmt.Fprintf(&b, "| %s | %s | %s | %d | %d | %d | %d | %d | %d | %s | %s |\n",
			a.Arm, a.ControlType, a.Expected, c.Runs, c.TruePass, c.TrueReject, c.Escape, c.FalseRejection,
			c.Undecided, rate(a.Rates.Sensitivity), rate(a.Rates.Specificity))
	}
	c := rep.Aggregate
	fmt.Fprintf(&b, "| **all** | | | %d | %d | %d | %d | %d | %d | %s | %s |\n\n",
		c.Runs, c.TruePass, c.TrueReject, c.Escape, c.FalseRejection, c.Undecided,
		rate(rep.Rates.Sensitivity), rate(rep.Rates.Specificity))
	fmt.Fprintf(&b, "False-rejection rate: %s\n\n", rate(rep.Rates.FalseRejectionRate))
	b.WriteString("| Base rate | PPV of a FAIL |\n|---|---|\n")
	for _, p := range rep.PPV {
		fmt.Fprintf(&b, "| %s | %s |\n", strconv.FormatFloat(p.BaseRate, 'f', -1, 64), rate(p.PPV))
	}
	if len(rep.Uncalibrated) > 0 {
		b.WriteString("\nNot calibrated:\n\n")
		for _, u := range rep.Uncalibrated {
			fmt.Fprintf(&b, "- %s\n", u)
		}
	}
	if len(rep.Unmatched) > 0 {
		b.WriteString("\nVerdicts naming an arm the manifest does not have:\n\n")
		for _, u := range rep.Unmatched {
			fmt.Fprintf(&b, "- `%s`\n", u)
		}
	}
	if len(rep.Breaches) > 0 {
		b.WriteString("\nGate breaches:\n\n")
		for _, br := range rep.Breaches {
			fmt.Fprintf(&b, "- %s\n", br)
		}
	}
	return b.String()
}
