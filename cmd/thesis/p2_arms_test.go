package main

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/WrdCstlg/pro-thesis/internal/lock"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
	"gopkg.in/yaml.v3"
)

// A P2 arm is 10 runs of one un-narrowed world each, seeds 1 to 10 (D-112).
// That holds only if `thesis run --arm <id>` resolves the arm to a profile
// that exists in its target's prothesis.yaml, plans exactly one world, carries
// the manifest's standard fault, and is covered by the project's current lock.
// `--worlds` can only narrow a profile and a clean narrowed run is never a
// PASS, so a row pointing at a multi-world profile describes runs that cannot
// be made as registered; a row pointing at a missing profile describes none.

// p2Row is the part of an eval/manifest.yaml arm this check reads.
type p2Row struct {
	ID           string         `yaml:"id"`
	Tier         int            `yaml:"tier"`
	Target       string         `yaml:"target"`
	Profile      string         `yaml:"profile"`
	Expected     *int           `yaml:"expected"`
	TargetOracle string         `yaml:"target_oracle"`
	N            map[string]any `yaml:"n"`
	Seeds        map[string]any `yaml:"seeds"`
	Status       string         `yaml:"status"`
}

// wantP2Arms is the P2 population: the Tier-1 arms less the deferred RQ-04.
var wantP2Arms = []string{"CO-01", "CO-02", "CO-03", "ET-01", "HC-01", "HC-02", "RE-01", "RQ-01", "RQ-02", "RQ-03"}

func TestEveryP2ArmResolvesToALockedOneWorldProfile(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(root, "eval", "manifest.yaml")
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		StandardFault string  `yaml:"standard_fault"`
		Arms          []p2Row `yaml:"arms"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("%s: %v", manifestPath, err)
	}
	if doc.StandardFault == "" {
		t.Fatal("the manifest names no standard_fault")
	}

	var got []string
	sawDeferred := false
	for _, a := range doc.Arms {
		if a.ID == "RQ-04" {
			sawDeferred = true
			_, inN := a.N["P2"]
			_, inSeeds := a.Seeds["P2"]
			if a.Status != "deferred" || inN || inSeeds {
				t.Errorf("RQ-04: status %q, P2 in n %v, P2 in seeds %v; want deferred and absent from P2",
					a.Status, inN, inSeeds)
			}
			continue
		}
		if _, ok := a.N["P2"]; !ok || a.Tier != 1 {
			continue
		}
		got = append(got, a.ID)
		a := a
		t.Run(a.ID, func(t *testing.T) { checkP2Arm(t, root, manifestPath, doc.StandardFault, a) })
	}
	if !sawDeferred {
		t.Error("the manifest has no RQ-04 row")
	}
	sort.Strings(got)
	if !reflect.DeepEqual(got, wantP2Arms) {
		t.Errorf("P2 arms %v, want %v", got, wantP2Arms)
	}
}

func checkP2Arm(t *testing.T, root, manifestPath, standardFault string, a p2Row) {
	if n, _ := a.N["P2"].(int); n != 10 {
		t.Errorf("n.P2 = %v, want 10 runs", a.N["P2"])
	}
	if s, _ := a.Seeds["P2"].(string); s != "1-10" {
		t.Errorf("seeds.P2 = %v, want run seeds \"1-10\"", a.Seeds["P2"])
	}

	// Resolve the arm exactly as `thesis run --arm` does.
	projectDir := filepath.Join(root, filepath.FromSlash(a.Target))
	var profile string
	cal, code := runCalibration(projectDir, a.ID, manifestPath, "", "", &profile, false)
	if code != schema.ExitPass || cal == nil {
		t.Fatalf("thesis run --arm %s does not resolve (exit %d)", a.ID, code)
	}
	if profile != a.Profile {
		t.Fatalf("--arm resolved profile %q, the row names %q", profile, a.Profile)
	}
	if a.Expected != nil && *a.Expected == int(schema.ExitFail) && cal.TargetOracle != "linearizable.kv" {
		t.Errorf("an expected-FAIL arm resolves target oracle %q, want linearizable.kv", cal.TargetOracle)
	}

	cfgBytes, err := os.ReadFile(filepath.Join(projectDir, "prothesis.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := schema.DecodeConfig(cfgBytes)
	if err != nil {
		t.Fatal(err)
	}
	p, ok := cfg.Profiles[profile]
	if !ok {
		t.Fatalf("profile %q is not in %s/prothesis.yaml", profile, a.Target)
	}
	if p.Worlds == nil || *p.Worlds != 1 {
		planned := "no world count"
		if p.Worlds != nil {
			planned = strconv.Itoa(*p.Worlds) + " world(s)"
		}
		t.Errorf("profile %q plans %s, want exactly 1 world", profile, planned)
	}
	if !reflect.DeepEqual(p.Faults, []string{standardFault}) {
		t.Errorf("profile %q faults %q, want exactly the standard fault %q", profile, p.Faults, standardFault)
	}

	// Only the world count differs from the source profile (and, for the
	// kvfixed arm, the pinned variant).
	srcName, wantVariant := strings.TrimPrefix(profile, "p2-"), ""
	if srcName == "linear-kvfixed" {
		srcName, wantVariant = "linear", "kvfixed"
	}
	src, ok := cfg.Profiles[srcName]
	switch {
	case !strings.HasPrefix(profile, "p2-") || !ok:
		t.Errorf("profile %q does not name a source profile (p2-<source>)", profile)
	case src.Budget != p.Budget || src.DriverProfile != p.DriverProfile || !reflect.DeepEqual(src.Faults, p.Faults):
		t.Errorf("profile %q differs from its source %q beyond the world count", profile, srcName)
	case p.Variant != wantVariant:
		t.Errorf("profile %q variant %q, want %q", profile, p.Variant, wantVariant)
	}

	// Covered by the project's current lock: the recomputed digest is the
	// locked one, and the locked manifest carries the profile's world count.
	rep, err := lock.Gate(lock.GateOptions{ProjectDir: projectDir, ConfigBytes: cfgBytes})
	if err != nil {
		t.Fatalf("lock: %v", err)
	}
	if rep.LockedSHA == "" || rep.ManifestSHA != rep.LockedSHA {
		t.Errorf("%s's lock is not current for this config (locked %s, now %s)", a.Target, rep.LockedSHA, rep.ManifestSHA)
	}
	covered := false
	for _, e := range rep.Manifest.Config {
		if (e.Path == "profiles."+profile+".worlds" || e.Path == `profiles."`+profile+`".worlds`) && e.Value == "1" {
			covered = true
		}
	}
	if !covered {
		t.Errorf("the lock manifest carries no profiles.%s.worlds = 1", profile)
	}
}
