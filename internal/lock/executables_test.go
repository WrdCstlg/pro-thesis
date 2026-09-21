package lock

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// OQ-057 measured the whole exploit: replacing bin/linearizable-kv with nine
// lines that print `"status":"ok"` left `thesis oracles verify` reporting OK and
// a world known to reproduce coming back PASS; with no drift, no warning, and
// nothing unusual in the artifact. The lock governs what PRO-THESIS READS; it
// never governed what PRO-THESIS EXECUTES.
//
// D-060 does not close that. Hashing a binary INTO the digest would make an
// ordinary `go build` exit 4 and turn re-locking into a reflex, which is why
// OQ-057 was documented rather than fixed. What these tests pin is the weaker,
// achievable property: the swap is no longer SILENT.

// oracleDef is a minimal but valid prothesis.oracle_def/v1 naming cmd.
func oracleDef(name, cmd string) string {
	return "version: prothesis.oracle_def/v1\n" +
		"name: " + name + "\n" +
		"class: consistency\n" +
		"valid_phases: [ASSERT]\n" +
		"cmd: \"" + cmd + "\"\n" +
		"timeout: 60s\n"
}

// writeProgram puts a file at a project-relative path and returns its dir path.
func writeProgram(t *testing.T, dir, rel, body string) string {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestFingerprintHashesTheProgramThatWillActuallyRun(t *testing.T) {
	dir := newProject(t, baseConfig, map[string]string{
		"lin.kv.yaml": oracleDef("linearizable.kv", "./bin/checker"),
	})
	writeProgram(t, dir, "bin/checker", "#!/bin/sh\nexit 0\n")

	fps, err := FingerprintExecutables(dir, ".prothesis/oracles")
	if err != nil {
		t.Fatalf("FingerprintExecutables: %v", err)
	}
	if len(fps) != 1 {
		t.Fatalf("fingerprinted %d program(s), want 1", len(fps))
	}
	got := fps[0]
	if got.Oracle != "linearizable.kv" {
		t.Errorf("oracle = %q", got.Oracle)
	}
	if !strings.HasPrefix(got.SHA256, schema.HashPrefix) {
		t.Errorf("sha256 = %q, want a %s-prefixed digest", got.SHA256, schema.HashPrefix)
	}
	if got.Size == 0 {
		t.Error("size = 0")
	}
	// The resolved path is recorded project-relative and slash-separated: a lock
	// carrying a developer's home directory is not reviewable in a clone.
	if got.Resolved != "bin/checker" {
		t.Errorf("resolved = %q, want %q", got.Resolved, "bin/checker")
	}
	if got.Unresolved != "" {
		t.Errorf("a readable program reported %q", got.Unresolved)
	}
}

// A checker that is not on disk is a fact to RECORD, not an error. Locking a
// project before building its checker, and `cmd: python check.py` resolving
// through PATH at run time, are both legitimate, and both have to stay
// visible as un-fingerprinted rather than silently producing a fingerprint of
// nothing.
func TestAnUnbuiltCheckerIsRecordedNotRefused(t *testing.T) {
	dir := newProject(t, baseConfig, map[string]string{
		"lin.kv.yaml": oracleDef("linearizable.kv", "./bin/never-built"),
	})

	fps, err := FingerprintExecutables(dir, ".prothesis/oracles")
	if err != nil {
		t.Fatalf("an unbuilt checker made fingerprinting fail: %v", err)
	}
	if len(fps) != 1 {
		t.Fatalf("fingerprinted %d, want 1", len(fps))
	}
	if fps[0].SHA256 != "" {
		t.Error("a program that does not exist produced a digest")
	}
	if fps[0].Unresolved == "" {
		t.Error("a program that could not be hashed does not say why")
	}
}

func TestDiffExecutablesReportsTheChangesThatMatter(t *testing.T) {
	was := []ExecutableFingerprint{
		{Oracle: "linearizable.kv", Resolved: "bin/checker", SHA256: "sha256:aaa", Size: 10},
	}

	t.Run("unchanged is silent", func(t *testing.T) {
		if got := DiffExecutables(was, was); len(got) != 0 {
			t.Fatalf("an unchanged program reported %d change(s): %+v", len(got), got)
		}
	})

	t.Run("a swapped program is reported", func(t *testing.T) {
		now := []ExecutableFingerprint{
			{Oracle: "linearizable.kv", Resolved: "bin/checker", SHA256: "sha256:bbb", Size: 9},
		}
		got := DiffExecutables(was, now)
		if len(got) != 1 {
			t.Fatalf("a swapped checker reported %d change(s); this is the OQ-057 case and it "+
				"must never be silent", len(got))
		}
		if got[0].Was != "sha256:aaa" || got[0].Now != "sha256:bbb" {
			t.Errorf("change = %+v, want both digests", got[0])
		}
	})

	t.Run("a program that has gone missing is reported", func(t *testing.T) {
		now := []ExecutableFingerprint{
			{Oracle: "linearizable.kv", Resolved: "bin/checker", Unresolved: "no file"},
		}
		got := DiffExecutables(was, now)
		if len(got) != 1 {
			t.Fatal("the program that decides every verdict disappeared and nothing said so")
		}
	})

	t.Run("an oracle that is no longer declared is reported", func(t *testing.T) {
		got := DiffExecutables(was, nil)
		if len(got) != 1 {
			t.Fatal("an oracle vanished from the definitions and nothing said so")
		}
	})

	// The upgrade path. A lock written before this block existed carries no
	// fingerprints; warning about that on every project's first run after the
	// upgrade is noise, and noise is how a warning gets ignored.
	t.Run("a lock predating the block is not a change", func(t *testing.T) {
		now := []ExecutableFingerprint{
			{Oracle: "linearizable.kv", Resolved: "bin/checker", SHA256: "sha256:bbb"},
		}
		if got := DiffExecutables(nil, now); len(got) != 0 {
			t.Fatalf("a pre-D-060 lock produced %d spurious warning(s)", len(got))
		}
	})
}

// The load-bearing property of the whole design: recording a program's hash must
// NOT move the manifest digest. If it did, every `go build` of a checker would
// produce ORACLE_DRIFT, the correct response would become "re-lock without
// reading the diff", and the lock would train the exact reflex it exists to
// prevent: which is why OQ-057 was left open rather than closed this way.
func TestFingerprintingDoesNotMoveTheDigest(t *testing.T) {
	dir := newProject(t, baseConfig, map[string]string{
		"lin.kv.yaml": oracleDef("linearizable.kv", "./bin/checker"),
	})
	prog := writeProgram(t, dir, "bin/checker", "original\n")
	before := digestOf(t, dir)

	// Rebuild the checker into something completely different.
	if err := os.WriteFile(prog, []byte("rebuilt, quite different\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	after := digestOf(t, dir)

	if before != after {
		t.Fatalf("rebuilding a checker moved the manifest digest (%s -> %s). Every go build "+
			"would now exit 4 and re-locking would become a reflex", before, after)
	}

	// And the fingerprint DID move, so the warning has something to report.
	fps, err := FingerprintExecutables(dir, ".prothesis/oracles")
	if err != nil {
		t.Fatal(err)
	}
	if len(fps) != 1 || fps[0].SHA256 == "" {
		t.Fatalf("the rebuilt checker was not fingerprinted: %+v", fps)
	}
}

// End to end through Gate and Write: lock a project, swap the program, and
// require the next check to report it as a WARNING while the status stays ok.
func TestASwappedProgramIsWarnedAboutAndTheStatusStaysOK(t *testing.T) {
	dir := newProject(t, baseConfig, map[string]string{
		"lin.kv.yaml": oracleDef("linearizable.kv", "./bin/checker"),
	})
	prog := writeProgram(t, dir, "bin/checker", "the real checker\n")

	rep, err := Gate(GateOptions{ProjectDir: dir})
	if err != nil {
		t.Fatalf("Gate: %v", err)
	}
	if len(rep.ExecutablesNow) != 1 || rep.ExecutablesNow[0].SHA256 == "" {
		t.Fatalf("the gate did not fingerprint the program: %+v", rep.ExecutablesNow)
	}
	if _, err := Write(WriteOptions{
		ProjectDir: dir, Manifest: rep.Manifest, Reason: "baseline",
		Executables: rep.ExecutablesNow,
	}); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Clean re-check: ok, and nothing to warn about.
	clean, err := Gate(GateOptions{ProjectDir: dir})
	if err != nil {
		t.Fatalf("Gate: %v", err)
	}
	if clean.Status != schema.LockOK {
		t.Fatalf("status = %q, want ok", clean.Status)
	}
	if len(clean.ExecutablesMoved) != 0 {
		t.Fatalf("an untouched checker warned: %+v", clean.ExecutablesMoved)
	}
	if len(clean.OracleLock().ExecutablesMoved) != 0 {
		t.Error("a clean verdict carried an executables_moved entry")
	}

	// The OQ-057 swap.
	if err := os.WriteFile(prog, []byte("a program that always prints ok\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	swapped, err := Gate(GateOptions{ProjectDir: dir})
	if err != nil {
		t.Fatalf("Gate: %v", err)
	}

	// The status MUST stay ok. The digest covers definitions and no definition
	// moved; claiming drift here would be the fix that makes every rebuild exit
	// 4, which D-060 explicitly declines to do.
	if swapped.Status != schema.LockOK {
		t.Errorf("status = %q, want ok: a swapped program is a warning, not drift", swapped.Status)
	}
	if swapped.EnforceExitCode() != schema.ExitPass {
		t.Errorf("a swapped program blocked the run with exit %d; D-060 reports, it does not refuse",
			swapped.EnforceExitCode())
	}

	// ...and it must be impossible to miss.
	if len(swapped.ExecutablesMoved) != 1 {
		t.Fatalf("the swap produced %d warning(s), want 1. This is OQ-057 measured: before "+
			"D-060 it produced none at all", len(swapped.ExecutablesMoved))
	}
	if got := swapped.OracleLock().ExecutablesMoved; len(got) != 1 || got[0] != "linearizable.kv" {
		t.Errorf("the verdict does not name the changed checker: %v", got)
	}
	diag := swapped.Diagnosis()
	if !strings.Contains(diag, "WARNING") || !strings.Contains(diag, "linearizable.kv") {
		t.Errorf("the diagnosis does not warn about the swap:\n%s", diag)
	}
	// The warning has to appear on an OK report specifically: a swapped
	// checker's whole signature is that everything else looks normal.
	if !strings.Contains(diag, "oracle lock OK") {
		t.Errorf("the warning was not attached to an OK diagnosis:\n%s", diag)
	}
}
