package lock

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// A changed checker or driver program used to be a warning everywhere (D-060),
// because a rebuild changed the bytes and CI builds its own binaries. The lock
// now records the platform its fingerprints were taken on. On THAT platform a
// changed program is drift (exit 4); on any other it stays a warning, since a
// Windows fingerprint can never match a Linux build.

const otherPlatform = "test-os/test-arch"

func lockedProject(t *testing.T, programs map[string]string) (dir string, paths map[string]string) {
	t.Helper()
	cfg := strings.Replace(baseConfig, "./bin/loadgen --history", "./bin/driver --history", 1)
	dir = newProject(t, cfg, map[string]string{
		"lin.kv.yaml": oracleDef("linearizable.kv", "./bin/checker"),
	})
	paths = map[string]string{}
	for rel, body := range programs {
		paths[rel] = writeProgram(t, dir, rel, body)
	}
	rep, err := Gate(GateOptions{ProjectDir: dir})
	if err != nil {
		t.Fatalf("Gate: %v", err)
	}
	if _, err := Write(WriteOptions{
		ProjectDir: dir, Manifest: rep.Manifest, Reason: "baseline", Executables: rep.ExecutablesNow,
	}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	return dir, paths
}

func TestAChangedProgramIsDriftOnTheLockingPlatform(t *testing.T) {
	for _, tc := range []struct{ name, rel, subject string }{
		{"checker", "bin/checker", "linearizable.kv"},
		{"driver", "bin/driver", "driver.cmd"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, progs := lockedProject(t, map[string]string{
				"bin/checker": "the real checker\n", "bin/driver": "the real driver\n",
			})
			f, err := Read(dir)
			if err != nil {
				t.Fatalf("Read: %v", err)
			}
			if f.Platform != CurrentPlatform() {
				t.Fatalf("the lock records platform %q, want %q", f.Platform, CurrentPlatform())
			}
			clean, err := Gate(GateOptions{ProjectDir: dir})
			if err != nil || clean.Status != schema.LockOK {
				t.Fatalf("untouched project: status %q, err %v", clean.Status, err)
			}

			if err := os.WriteFile(progs[tc.rel], []byte("a program that always prints ok\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			rep, err := Gate(GateOptions{ProjectDir: dir})
			if err != nil {
				t.Fatalf("Gate: %v", err)
			}
			if rep.Status != schema.LockMismatch || !rep.ExecutablesFatal {
				t.Fatalf("status %q, fatal %v: a changed %s on the locking platform must be drift",
					rep.Status, rep.ExecutablesFatal, tc.name)
			}
			if rep.EnforceExitCode() != schema.ExitOracleDrift || rep.VerifyExitCode() != schema.ExitOracleDrift {
				t.Fatalf("run exit %d, verify exit %d, want 4 and 4", rep.EnforceExitCode(), rep.VerifyExitCode())
			}
			diag := rep.Diagnosis()
			for _, want := range []string{tc.subject, CurrentPlatform()} {
				if !strings.Contains(diag, want) {
					t.Fatalf("the diagnosis does not name %q:\n%s", want, diag)
				}
			}
			if strings.Contains(diag, "manifest FORMAT changed") {
				t.Fatalf("the diagnosis blames the lock format for a changed program:\n%s", diag)
			}
		})
	}
}

func TestAChangedProgramIsAWarningOnAnotherPlatform(t *testing.T) {
	dir, progs := lockedProject(t, map[string]string{"bin/checker": "the real checker\n", "bin/driver": "d\n"})
	if err := os.WriteFile(progs["bin/checker"], []byte("rebuilt elsewhere\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	rep, err := Gate(GateOptions{ProjectDir: dir, Platform: otherPlatform})
	if err != nil {
		t.Fatalf("Gate: %v", err)
	}
	if rep.Status != schema.LockOK || rep.ExecutablesFatal || rep.EnforceExitCode() != schema.ExitPass {
		t.Fatalf("status %q, fatal %v, exit %d: a fingerprint from another platform cannot be compared, "+
			"so it may only warn", rep.Status, rep.ExecutablesFatal, rep.EnforceExitCode())
	}
	if len(rep.ExecutablesMoved) != 1 {
		t.Fatalf("the warning is missing: %+v", rep.ExecutablesMoved)
	}
}

// A lock written before the platform was recorded cannot say where its
// fingerprints came from, so a difference stays a warning until it is
// re-locked.
func TestALockWithoutAPlatformOnlyWarns(t *testing.T) {
	dir, progs := lockedProject(t, map[string]string{"bin/checker": "the real checker\n", "bin/driver": "d\n"})
	raw, err := os.ReadFile(Path(dir))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	delete(doc, "platform")
	out, _ := json.MarshalIndent(doc, "", "  ")
	if err := os.WriteFile(Path(dir), out, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(progs["bin/checker"], []byte("changed\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	rep, err := Gate(GateOptions{ProjectDir: dir})
	if err != nil {
		t.Fatalf("Gate: %v", err)
	}
	if rep.Status != schema.LockOK || rep.ExecutablesFatal {
		t.Fatalf("status %q, fatal %v: a lock with no recorded platform must only warn", rep.Status, rep.ExecutablesFatal)
	}
}
