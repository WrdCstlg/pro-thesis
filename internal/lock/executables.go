package lock

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/WrdCstlg/pro-thesis/internal/driver"
	"github.com/WrdCstlg/pro-thesis/internal/oracle"
)

// ExecutableFingerprint records what an external oracle's `cmd` RESOLVED to,
// and what that file hashed to, at lock time.
//
// It is recorded OUTSIDE the manifest digest, for the same reason
// builtin_options_fingerprint is: including it would make an ordinary `go build`
// of the checker produce ORACLE_DRIFT, and the correct response would become
// "re-lock without reading the diff", which trains exactly the reflex the lock
// exists to prevent. That objection is why OQ-057 was documented rather than
// closed, and it is answered by reporting rather than by refusing.
//
// What this gives up, stated plainly: a swapped checker is still POSSIBLE.
// Nothing short of signing makes it impossible. What it removes is the SILENCE:
// the property that made OQ-057 exploitable rather than merely conceivable.
type ExecutableFingerprint struct {
	// Oracle is the definition's `name`, which is also the string the verdict's
	// violations[].oracle carries.
	Oracle string `json:"oracle"`
	// Cmd is the definition's `cmd` verbatim, so a reader can see what was
	// written as well as what it became.
	Cmd string `json:"cmd"`
	// Resolved is the path the program resolved to, slash-separated and
	// project-relative when it lies inside the project.
	//
	// It is recorded because resolution is not obvious: a bare program name can
	// come from the project directory, from a .exe sibling, or from PATH, and
	// "which file did you hash" is the first question a reader has.
	Resolved string `json:"resolved"`
	// SHA256 is the program's digest, "sha256:"-prefixed. Empty when the
	// program could not be read, in which case Unresolved says why.
	SHA256 string `json:"sha256,omitempty"`
	// Size is the program's size in bytes.
	Size int64 `json:"size,omitempty"`
	// Unresolved is why no hash was taken. A PATH lookup, a missing binary and
	// an interpreter are all legitimate and all produce a fingerprint with no
	// digest rather than an error: an oracle that cannot be fingerprinted must
	// not prevent locking, it must be VISIBLE as not fingerprinted.
	Unresolved string `json:"unresolved,omitempty"`
}

// FingerprintExecutables hashes the resolved program of every external oracle
// declared under oraclesDir.
//
// It parses the definitions with the SAME parser the engine uses and resolves
// argv[0] with the SAME resolver the engine calls, because a fingerprint taken
// of a different file than the one that will execute is worse than no
// fingerprint at all: it is a false assurance. The duplication hazard is not
// hypothetical: D-059 records a gate that stayed broken for two phases because
// one of two hand-copied implementations was fixed and the other was not.
//
// Only the PROGRAM is hashed. Arguments, interpreters, shared libraries and
// anything the program loads at run time are not, and that is stated in the
// lock's own limitations rather than implied away.
func FingerprintExecutables(projectDir, oraclesDir string) ([]ExecutableFingerprint, error) {
	dir := oraclesDir
	if dir != "" && !filepath.IsAbs(dir) {
		dir = filepath.Join(projectDir, dir)
	}
	disc, err := oracle.Discover(dir)
	if err != nil {
		return nil, fmt.Errorf("lock: discover oracle definitions in %s: %w", dir, err)
	}
	if disc == nil {
		return nil, nil
	}

	out := make([]ExecutableFingerprint, 0, len(disc.Definitions))
	for _, d := range disc.Definitions {
		out = append(out, fingerprintOne(projectDir, d))
	}
	// Discover already returns definitions sorted by name. Re-sorting states the
	// requirement locally rather than inheriting it: this block is committed and
	// diffed, and an ordering that moved would produce a spurious change on
	// every re-lock, which is how a reviewer learns to skim it.
	sort.Slice(out, func(i, j int) bool { return out[i].Oracle < out[j].Oracle })
	return out, nil
}

func fingerprintOne(projectDir string, d oracle.Definition) ExecutableFingerprint {
	fp := ExecutableFingerprint{Oracle: d.Name, Cmd: d.Cmd}

	argv, err := driver.SplitCommand(d.Cmd)
	if err != nil || len(argv) == 0 || argv[0] == "" {
		fp.Unresolved = "the cmd could not be split into an argv"
		return fp
	}

	resolved := driver.ResolveProgram(argv[0], projectDir)
	fp.Resolved = relativeToProject(projectDir, resolved)

	info, err := os.Stat(resolved)
	switch {
	case err != nil:
		// Not an error. `cmd: python check.py` resolves through PATH at run
		// time and is a legitimate configuration; so is locking a project
		// before its checker has been built. Both are recorded as
		// un-fingerprinted, which is a fact a reader can act on.
		fp.Unresolved = fmt.Sprintf("no file at %s (a PATH lookup or an unbuilt checker "+
			"resolves at run time and cannot be fingerprinted here)", fp.Resolved)
		return fp
	case info.IsDir():
		fp.Unresolved = fmt.Sprintf("%s is a directory", fp.Resolved)
		return fp
	}

	sum, size, err := hashFile(resolved)
	if err != nil {
		fp.Unresolved = fmt.Sprintf("could not read %s: %v", fp.Resolved, err)
		return fp
	}
	fp.SHA256, fp.Size = sum, size
	return fp
}

// relativeToProject renders a path project-relative and slash-separated when it
// lies inside the project, and absolute otherwise. A lock file that records a
// developer's home directory is not reviewable in someone else's clone.
func relativeToProject(projectDir, p string) string {
	if projectDir == "" {
		return filepath.ToSlash(p)
	}
	rel, err := filepath.Rel(projectDir, p)
	if err != nil || strings.HasPrefix(rel, "..") {
		return filepath.ToSlash(p)
	}
	return filepath.ToSlash(rel)
}

// ExecutableChange is one oracle whose program no longer matches what the lock
// recorded.
type ExecutableChange struct {
	Oracle string
	// Was and Now are the recorded and current digests. Either may be empty,
	// which is itself informative: "" -> sha means a checker that could not be
	// fingerprinted at lock time can be now, and sha -> "" means the program
	// the verdict depends on has gone missing.
	Was string
	Now string
	// Detail is the human sentence for the warning.
	Detail string
}

// DiffExecutables compares the fingerprints recorded in a lock against the ones
// measured now.
//
// A lock written before this block existed carries no fingerprints at all, and
// that is NOT reported as a change: every project would otherwise emit a
// warning on its first run after the upgrade, which is noise that teaches
// people to ignore the warning. It becomes meaningful at the next re-lock.
func DiffExecutables(locked, now []ExecutableFingerprint) []ExecutableChange {
	if len(locked) == 0 {
		return nil
	}
	byName := make(map[string]ExecutableFingerprint, len(now))
	for _, f := range now {
		byName[f.Oracle] = f
	}

	var out []ExecutableChange
	for _, was := range locked {
		is, present := byName[was.Oracle]
		switch {
		case !present:
			out = append(out, ExecutableChange{
				Oracle: was.Oracle, Was: was.SHA256,
				Detail: "the oracle is no longer declared, so its program was not checked",
			})
		case was.SHA256 == "" && is.SHA256 == "":
			// Neither could be fingerprinted. Nothing moved; the limitation is
			// already recorded in the lock.
		case was.SHA256 == "":
			out = append(out, ExecutableChange{
				Oracle: was.Oracle, Now: is.SHA256,
				Detail: fmt.Sprintf("could not be fingerprinted at lock time (%s) and now "+
					"resolves to %s", was.Unresolved, is.Resolved),
			})
		case is.SHA256 == "":
			out = append(out, ExecutableChange{
				Oracle: was.Oracle, Was: was.SHA256,
				Detail: fmt.Sprintf("the program recorded at %s is no longer readable: %s",
					was.Resolved, is.Unresolved),
			})
		case was.SHA256 != is.SHA256:
			out = append(out, ExecutableChange{
				Oracle: was.Oracle, Was: was.SHA256, Now: is.SHA256,
				Detail: fmt.Sprintf("%s changed (%s -> %s)", is.Resolved,
					shortSum(was.SHA256), shortSum(is.SHA256)),
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Oracle < out[j].Oracle })
	return out
}

func shortSum(s string) string {
	s = strings.TrimPrefix(s, "sha256:")
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

// FormatExecutableChanges renders the operator-facing warning.
//
// It says what a reader has to do, because "the checker changed" with no
// instruction is a line people learn to scroll past. A rebuild is the common
// and legitimate cause; a swap is the one that matters; the text refuses to
// guess which.
func FormatExecutableChanges(changes []ExecutableChange) string {
	if len(changes) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "thesis: WARNING: %d oracle program(s) differ from the lock:\n", len(changes))
	for _, c := range changes {
		fmt.Fprintf(&b, "  %s: %s\n", c.Oracle, c.Detail)
	}
	b.WriteString("  The lock hashes oracle DEFINITIONS; these digests are recorded outside it so a\n" +
		"  rebuild is a warning rather than drift. A rebuilt checker is expected. A checker\n" +
		"  you did not rebuild is the failure OQ-057 describes: the program deciding every\n" +
		"  verdict was replaced. Confirm which, then re-lock with a reason that says so.\n")
	return b.String()
}
