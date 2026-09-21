// Package buildinfo carries the build provenance stamped into the binary by
// -ldflags -X at compile time (see scripts/build.ps1).
//
// WHY THIS EXISTS (OQ-055's sibling gap): verdict.json historically recorded
// the working tree's HEAD at run time. That says where the run happened, not
// what the binary IS: a thesis.exe built yesterday from a dirty tree still
// reports today's HEAD. For a tool whose artifacts exist to be re-verified,
// the build identity must travel INSIDE the binary.
//
// The vars are strings, empty when unstamped (a plain `go build` or
// `go test`). Nothing here may fail: provenance is metadata, and an unstamped
// binary running a valid world must still run. The failure mode is the empty
// string, never an error: same contract as control.GitCommit.
package buildinfo

var (
	// Commit is the git revision the binary was built from, e.g. "4a3e10c".
	Commit string
	// Dirty is "1" when the working tree had uncommitted changes at build
	// time. A dirty stamp means Commit names a tree the binary only
	// approximately corresponds to.
	Dirty string
	// SourceDate is Commit's committer date, RFC3339 UTC. It is the date of the
	// SOURCE, never the moment the compiler ran: linearizable-kv.exe is
	// fingerprinted by SHA-256 in .prothesis/lock, and a wall-clock stamp made
	// every rebuild of identical source look like a swapped checker (D-072).
	// Taking it from the commit keeps two builds of one tree byte-identical
	// while still printing a date, which a reader can check against
	// `git show -s --format=%cI <commit>` after normalising it to UTC.
	SourceDate string
)

// Stamped reports whether the binary carries build provenance at all.
func Stamped() bool { return Commit != "" }

// VerdictCommit returns the revision a verdict should record: the binary's
// own build stamp when present, else the fallback (the working tree's HEAD,
// which is all an unstamped binary can know). A dirty build appends "+dirty"
// to the commit string so the artifact correctly expresses uncommitted state.
func VerdictCommit(fallback string) string {
	if Commit != "" {
		if Dirty == "1" {
			return Commit + "+dirty"
		}
		return Commit
	}
	return fallback
}
