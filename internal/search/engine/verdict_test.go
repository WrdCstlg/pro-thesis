package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/WrdCstlg/pro-thesis/pkg/schema"
	"gopkg.in/yaml.v3"
)

// ---------------------------------------------------------------------------
// A.9 item 5: every verdict a search produced must be well-formed
//
// "thesis search --budget 30m completes without panic and produces well-formed
// prothesis.verdict/v1 output."
//
// control.WriteVerdictFile validates before writing, so a verdict.json on disk
// has already passed once. That is not the same as checking it: a validator that
// silently accepted everything would satisfy both. This walks every recorded run
// bundle in the reference fixture and re-parses and re-validates what is
// actually there, independently of the code that wrote it.
//
// It SKIPS when no run bundle exists, which is the normal state of a clean
// checkout: the acceptance runs are not committed. A skip says "nothing to
// check here", never "checked and fine". corpusBundles makes that the ONLY
// skip: while the corpus exists, the committed CENSUS.json floor (see
// census_test.go) turns a prune that shrinks it into a failure, so these
// checks cannot silently stop having anything to check.
// ---------------------------------------------------------------------------

func runsDir(t *testing.T) string {
	t.Helper()
	// internal/search/engine -> repo root
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	return filepath.Join(root, "testdata", "kvfixture", ".prothesis", "runs")
}

func TestEveryRecordedVerdictIsWellFormed(t *testing.T) {
	dir := runsDir(t)
	ents := corpusBundles(t)
	checked := 0
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		path := filepath.Join(dir, e.Name(), "verdict.json")
		data, err := os.ReadFile(path)
		if err != nil {
			continue // a run that was interrupted before ASSERT has none
		}
		v, err := schema.UnmarshalVerdict(data)
		if err != nil {
			t.Errorf("%s does not parse as prothesis.verdict/v1: %v", path, err)
			continue
		}
		if err := v.Validate(); err != nil {
			t.Errorf("%s fails its own schema: %v", path, err)
			continue
		}
		if v.Schema != schema.VerdictSchema {
			t.Errorf("%s declares schema %q, want %q", path, v.Schema, schema.VerdictSchema)
		}
		if v.RunID == "" {
			t.Errorf("%s has no run_id", path)
		}
		// A violation that names no oracle is unactionable: the agent reading
		// this verdict has nothing to look up.
		for _, viol := range v.Violations {
			if viol.Oracle == "" || viol.ID == "" {
				t.Errorf("%s: violation %+v has no id or no oracle", path, viol)
			}
		}
		checked++
	}
	if checked == 0 {
		t.Skip("no verdict.json in any run bundle")
	}
	t.Logf("validated %d recorded verdict(s)", checked)
}

// The search's own record must be parseable too, and it must not claim a
// time-to-first-violation for a trial that found none: that is the
// right-censoring D-029 builds its whole test around.
func TestEveryRecordedSearchRecordIsConsistent(t *testing.T) {
	dir := runsDir(t)
	ents := corpusBundles(t)
	checked := 0
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		path := filepath.Join(dir, e.Name(), SearchRecordName)
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var rec SearchRecord
		if err := json.Unmarshal(data, &rec); err != nil {
			t.Errorf("%s does not parse: %v", path, err)
			continue
		}
		if rec.FirstViolationOrdinal == 0 && rec.FirstViolationSeconds != nil {
			t.Errorf("%s reports a time-to-first-violation for a run that found none; that "+
				"quantity is CENSORED and must be absent, not zero", path)
		}
		if rec.FirstViolationOrdinal > 0 && rec.FirstViolationSeconds == nil {
			t.Errorf("%s found a violation but recorded no time for it", path)
		}
		checked++
	}
	if checked == 0 {
		t.Skip("no search.json in any run bundle")
	}
	t.Logf("validated %d recorded search record(s)", checked)
}

// ---------------------------------------------------------------------------
// The bridge-network leak check
//
// This asserts a property of the ENVIRONMENT a run executed in, not of the code
// under test, so it is gated behind PROTHESIS_ACCEPTANCE rather than run as an
// ordinary unit test. A daemon that becomes unreachable mid-teardown leaks a
// project through no fault of this package (OQ-044), and a suite that went red
// on every developer machine that ever had a flaky Docker would train people to
// ignore it, which is worse than not having it.
//
// It is NOT weakened. It is the same assertion, run where its failure means
// something actionable: the acceptance procedure runs it explicitly, and a
// failure names the run, the direction and the magnitude so the leaked project
// can be swept.
//
//	PROTHESIS_ACCEPTANCE=1 go test ./internal/search/engine/ -run LeakedABridgeNetwork -v
//
// Recovery for a run it names:
//
//	docker compose -p <project> down -v --remove-orphans
//
// The run's record keeps its counts forever, so a swept run is acknowledged in
// testdata/kvfixture/.prothesis/swept-leaks.yaml with the evidence of the
// sweep, and the check then asks Docker whether anything of the run's projects
// is left (D-111). A listed run whose project survives still fails.
// ---------------------------------------------------------------------------

// sweptLeak is one entry of the committed list of recorded leaks whose
// projects have since been removed.
type sweptLeak struct {
	Run             string `yaml:"run"`
	LeakedNetworks  int    `yaml:"leaked_networks"`
	ConfirmedAbsent string `yaml:"confirmed_absent"`
	Evidence        string `yaml:"evidence"`
	Ledger          string `yaml:"ledger"`
}

// recordedLeak is a run whose search record shows fewer free bridge networks
// after it than before, with the compose projects its worlds used.
type recordedLeak struct {
	Run           string
	Before, After int
	Projects      []string
}

// residueProbe lists what Docker still holds for the given compose projects.
type residueProbe func(projects []string) ([]string, error)

// sweptLeaksPath is the committed list, beside the corpus it describes.
func sweptLeaksPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(filepath.Dir(runsDir(t)), "swept-leaks.yaml")
}

var (
	runIDPattern  = regexp.MustCompile(`^r_\d{4}_\d{2}_\d{2}_[0-9a-f]{4}$`)
	ledgerPattern = regexp.MustCompile(`^(?:OQ|D)-\d{3}[a-z]?$`)
)

// loadSweptLeaks reads the committed list strictly: an unknown key, an entry
// without its evidence, count, date or ledger citation, or a run listed twice
// is an error, because an acknowledgement nobody can check is how a leak
// would be waved through. A missing list is an empty one.
func loadSweptLeaks(path string) (map[string]sweptLeak, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]sweptLeak{}, nil
	}
	if err != nil {
		return nil, err
	}
	var doc struct {
		Runs []sweptLeak `yaml:"runs"`
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	out := map[string]sweptLeak{}
	for i, s := range doc.Runs {
		at := fmt.Sprintf("%s: runs[%d]", path, i)
		switch {
		case !runIDPattern.MatchString(s.Run):
			return nil, fmt.Errorf("%s: run %q is not a run id", at, s.Run)
		case s.LeakedNetworks < 1:
			return nil, fmt.Errorf("%s: leaked_networks must name the leak acknowledged (>= 1)", at)
		case s.Evidence == "":
			return nil, fmt.Errorf("%s: an acknowledgement without evidence", at)
		case !ledgerPattern.MatchString(s.Ledger):
			return nil, fmt.Errorf("%s: ledger %q is not an OQ- or D- id", at, s.Ledger)
		}
		if _, err := time.Parse("2006-01-02", s.ConfirmedAbsent); err != nil {
			return nil, fmt.Errorf("%s: confirmed_absent %q is not a date (YYYY-MM-DD)", at, s.ConfirmedAbsent)
		}
		if _, dup := out[s.Run]; dup {
			return nil, fmt.Errorf("%s: run %s is listed twice", at, s.Run)
		}
		out[s.Run] = s
	}
	return out, nil
}

// judgeLeaks decides each recorded leak. A leak fails unless the list
// acknowledges exactly that leak AND the probe confirms, now, that nothing of
// the run's recorded projects remains. An acknowledgement that matches no
// recorded leak fails too: a stale entry is a record nobody checked.
func judgeLeaks(leaks []recordedLeak, acks map[string]sweptLeak, probe residueProbe) (failures, honoured []string) {
	matched := map[string]bool{}
	for _, l := range leaks {
		n := l.Before - l.After
		ack, ok := acks[l.Run]
		if !ok {
			failures = append(failures, fmt.Sprintf("%s: %d free bridge networks before the run and only %d after — %d leaked. "+
				"On this host a leaked project holds them until swept, and the pool is about 24 "+
				"(D-017, OQ-044). Sweep with: docker compose -p <project> down -v --remove-orphans, "+
				"then acknowledge the run, with the evidence, in swept-leaks.yaml",
				l.Run, l.Before, l.After, n))
			continue
		}
		matched[l.Run] = true
		switch {
		case ack.LeakedNetworks != n:
			failures = append(failures, fmt.Sprintf("%s: the list acknowledges %d leaked network(s), the run recorded %d",
				l.Run, ack.LeakedNetworks, n))
		case len(l.Projects) == 0:
			failures = append(failures, fmt.Sprintf("%s: acknowledged as swept, but the run records no compose project, "+
				"so the sweep cannot be confirmed", l.Run))
		default:
			residue, err := probe(l.Projects)
			switch {
			case err != nil:
				failures = append(failures, fmt.Sprintf("%s: acknowledged as swept, but Docker cannot confirm it: %v", l.Run, err))
			case len(residue) > 0:
				failures = append(failures, fmt.Sprintf("%s: acknowledged as swept, but Docker still holds %s",
					l.Run, strings.Join(residue, ", ")))
			default:
				honoured = append(honoured, fmt.Sprintf("%s: %d leaked network(s), acknowledged as swept (%s, %s) and "+
					"confirmed gone now: no container or network of %s", l.Run, n, ack.ConfirmedAbsent, ack.Ledger,
					strings.Join(l.Projects, ", ")))
			}
		}
	}
	runs := make([]string, 0, len(acks))
	for r := range acks {
		runs = append(runs, r)
	}
	sort.Strings(runs)
	for _, r := range runs {
		if !matched[r] {
			failures = append(failures, fmt.Sprintf("%s: swept-leaks.yaml acknowledges it, but it matches no recorded leak", r))
		}
	}
	return failures, honoured
}

// dockerResidue lists every container and network Docker holds for the given
// compose projects, by the compose label and, for networks, by the
// "<project>_" name prefix compose gives them. Any failure to ask is an error,
// never an empty answer.
func dockerResidue(projects []string) ([]string, error) {
	run := func(args ...string) ([]string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, "docker", args...).Output()
		if err != nil {
			return nil, fmt.Errorf("docker %s: %w", strings.Join(args, " "), err)
		}
		return strings.Fields(string(out)), nil
	}
	allNetworks, err := run("network", "ls", "--format", "{{.Name}}")
	if err != nil {
		return nil, err
	}
	var residue []string
	for _, p := range projects {
		label := "label=com.docker.compose.project=" + p
		containers, err := run("ps", "-a", "--filter", label, "--format", "{{.Names}}")
		if err != nil {
			return nil, err
		}
		for _, c := range containers {
			residue = append(residue, "container "+c)
		}
		labelled, err := run("network", "ls", "--filter", label, "--format", "{{.Name}}")
		if err != nil {
			return nil, err
		}
		seen := map[string]bool{}
		for _, n := range labelled {
			seen[n] = true
		}
		for _, n := range allNetworks {
			if strings.HasPrefix(n, p+"_") {
				seen[n] = true
			}
		}
		names := make([]string, 0, len(seen))
		for n := range seen {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			residue = append(residue, "network "+n)
		}
	}
	return residue, nil
}

func TestNoRecordedRunLeakedABridgeNetwork(t *testing.T) {
	if os.Getenv("PROTHESIS_ACCEPTANCE") == "" {
		t.Skip("set PROTHESIS_ACCEPTANCE=1 to check recorded runs for bridge-network leaks")
	}
	acks, err := loadSweptLeaks(sweptLeaksPath(t))
	if err != nil {
		t.Fatalf("the swept-leak list: %v", err)
	}
	dir := runsDir(t)
	ents := corpusBundles(t)
	checked := 0
	var leaks []recordedLeak
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		path := filepath.Join(dir, e.Name(), SearchRecordName)
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var rec SearchRecord
		if err := json.Unmarshal(data, &rec); err != nil {
			continue
		}
		checked++
		// -1 means the probe could not run, which is NOT the same as no change.
		if rec.NetworksBefore < 0 || rec.NetworksAfter < 0 {
			t.Logf("%s: the free-network probe did not run; this run's teardown is unverified",
				e.Name())
			continue
		}
		if rec.NetworksAfter < rec.NetworksBefore {
			l := recordedLeak{Run: e.Name(), Before: rec.NetworksBefore, After: rec.NetworksAfter}
			seen := map[string]bool{}
			for _, w := range rec.Worlds {
				if w.Project != "" && !seen[w.Project] {
					seen[w.Project] = true
					l.Projects = append(l.Projects, w.Project)
				}
			}
			sort.Strings(l.Projects)
			leaks = append(leaks, l)
		}
	}
	if checked == 0 {
		t.Skip("no search.json in any run bundle")
	}
	// A recorded leak never leaves the record. It passes only when the
	// committed list acknowledges it as swept AND Docker confirms, now, that
	// nothing of the run's projects remains (D-111).
	failures, honoured := judgeLeaks(leaks, acks, dockerResidue)
	for _, h := range honoured {
		t.Log(h)
	}
	for _, f := range failures {
		t.Error(f)
	}
	t.Logf("checked %d recorded run(s); %d leaked, %d of them acknowledged as swept and confirmed gone",
		checked, len(leaks), len(honoured))
}
