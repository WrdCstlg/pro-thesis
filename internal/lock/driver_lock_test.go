package lock

import (
	"os"
	"strings"
	"testing"

	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// ---------------------------------------------------------------------------
// The load generator is inside the lock (OQ-026, OQ-076, D-090)
//
// The driver writes the history every consistency oracle judges. A lock that
// covers the oracles and not the driver covers the judge and not the evidence.
// Measured on 2026-09-25 (OQ-076): pinning `driver.cmd` to `--profile smoke`
// left `thesis oracles verify` at exit 0, and linearizable.kv then returned ok
// in all 4 worlds that booted, against the planted defect it otherwise catches.
// ---------------------------------------------------------------------------

func TestADriverCommandEditMovesTheDigest(t *testing.T) {
	pinned := strings.Replace(baseConfig, "--profile {profile}", "--profile smoke", 1)
	if pinned == baseConfig {
		t.Fatal("fixture does not contain the driver's --profile placeholder")
	}
	if digestOfConfig(t, baseConfig) == digestOfConfig(t, pinned) {
		t.Fatal("pinning driver.cmd to one profile did not move the digest; the command that " +
			"writes the history every oracle judges must be covered (OQ-076)")
	}
}

// A driver profile that a run profile names is the workload the gate runs, so
// shrinking it weakens the gate as surely as shortening its budget (OQ-026).
func TestShrinkingAReferencedDriverProfileMovesTheDigest(t *testing.T) {
	for _, tc := range []struct{ name, from, to string }{
		{"ops", "ops: 500", "ops: 5"},
		{"clients", "clients: 4", "clients: 1"},
		{"mix", "mix: { read: 0.5, write: 0.5 }", "mix: { read: 1.0 }"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed := strings.Replace(baseConfig, tc.from, tc.to, 1)
			if changed == baseConfig {
				t.Fatalf("fixture does not contain %q", tc.from)
			}
			if digestOfConfig(t, baseConfig) == digestOfConfig(t, changed) {
				t.Fatalf("editing the %s of a driver profile a run profile names did not move "+
					"the digest (OQ-026)", tc.name)
			}
		})
	}
}

// A driver profile that no run profile names is not the gate's workload.
// Editing it must not fire exit 4: that is the stream of spurious lock bumps
// OQ-026 declined to create. The reference is what decides, so the same edit
// must move the digest as soon as a run profile names that driver profile.
func TestOnlyDriverProfilesARunProfileNamesAreCovered(t *testing.T) {
	const smoke = "    smoke: { clients: 4, ops: 500, mix: { read: 0.5, write: 0.5 } }\n"
	withScratch := strings.Replace(baseConfig, smoke,
		smoke+"    scratch: { clients: 2, ops: 50, mix: { read: 1.0 } }\n", 1)
	if withScratch == baseConfig {
		t.Fatal("fixture has no driver profile to anchor on")
	}
	shrink := func(cfg string) string {
		out := strings.Replace(cfg, "scratch: { clients: 2, ops: 50,", "scratch: { clients: 2, ops: 5,", 1)
		if out == cfg {
			t.Fatal("fixture does not contain the scratch profile")
		}
		return out
	}

	if digestOfConfig(t, withScratch) != digestOfConfig(t, shrink(withScratch)) {
		t.Fatal("editing a driver profile that no run profile names moved the digest; that is " +
			"the spurious exit 4 on routine workload edits that OQ-026 warned about")
	}

	named := strings.Replace(withScratch,
		"gate: { budget: 10m, worlds: 30, driver_profile: gate }",
		"gate: { budget: 10m, worlds: 30, driver_profile: scratch }", 1)
	if named == withScratch {
		t.Fatal("fixture has no gate profile to repoint")
	}
	if digestOfConfig(t, named) == digestOfConfig(t, shrink(named)) {
		t.Fatal("once a run profile names the scratch driver profile, shrinking it must move the digest")
	}
}

// The driver's PROGRAM is fingerprinted the way an oracle's is (D-060): outside
// the digest, so a rebuild is a warning and not drift, and never in silence.
func TestTheDriverProgramIsFingerprintedAndASwapIsWarnedAbout(t *testing.T) {
	dir := newProject(t, baseConfig, map[string]string{
		"lin.kv.yaml": oracleDef("linearizable.kv", "./bin/checker"),
	})
	writeProgram(t, dir, "bin/checker", "the real checker\n")
	loadgen := writeProgram(t, dir, "bin/loadgen", "the real load generator\n")

	rep, err := Gate(GateOptions{ProjectDir: dir})
	if err != nil {
		t.Fatalf("Gate: %v", err)
	}
	var drv *ExecutableFingerprint
	for i := range rep.ExecutablesNow {
		if rep.ExecutablesNow[i].Oracle == "driver.cmd" {
			drv = &rep.ExecutablesNow[i]
		}
	}
	if drv == nil || drv.SHA256 == "" {
		t.Fatalf("the load generator was not fingerprinted: %+v", rep.ExecutablesNow)
	}
	f, err := Write(WriteOptions{
		ProjectDir: dir, Manifest: rep.Manifest, Reason: "baseline",
		Executables: rep.ExecutablesNow,
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if !strings.Contains(strings.Join(f.Covers, "\n"), "driver.cmd") {
		t.Errorf("the lock file's covers list does not name driver.cmd: %v", f.Covers)
	}

	clean, err := Gate(GateOptions{ProjectDir: dir})
	if err != nil {
		t.Fatalf("Gate: %v", err)
	}
	if clean.Status != schema.LockOK || len(clean.ExecutablesMoved) != 0 {
		t.Fatalf("an untouched project did not check clean: status %q, moved %+v",
			clean.Status, clean.ExecutablesMoved)
	}

	if err := os.WriteFile(loadgen, []byte("a driver that issues one read and exits\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	swapped, err := Gate(GateOptions{ProjectDir: dir})
	if err != nil {
		t.Fatalf("Gate: %v", err)
	}
	// A rebuilt driver binary is expected on every build, so it warns and does
	// not block, exactly as a rebuilt checker does (D-060).
	if swapped.Status != schema.LockOK || swapped.EnforceExitCode() != schema.ExitPass {
		t.Errorf("a swapped driver program changed the lock status to %q (exit %d); it must "+
			"warn, not refuse", swapped.Status, swapped.EnforceExitCode())
	}
	if got := swapped.OracleLock().ExecutablesMoved; len(got) != 1 || got[0] != "driver.cmd" {
		t.Fatalf("the verdict does not name the swapped driver program: %v", got)
	}
	if diag := swapped.Diagnosis(); !strings.Contains(diag, "WARNING") || !strings.Contains(diag, "driver.cmd") {
		t.Errorf("the diagnosis does not warn about the swapped driver:\n%s", diag)
	}
}
