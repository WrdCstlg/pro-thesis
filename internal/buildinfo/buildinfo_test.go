package buildinfo

import "testing"

func TestVerdictCommitPrefersStamp(t *testing.T) {
	defer func() { Commit = "" }()

	Commit = ""
	if got := VerdictCommit("abc1234"); got != "abc1234" {
		t.Errorf("unstamped binary: VerdictCommit = %q, want fallback %q", got, "abc1234")
	}

	Commit = "4a3e10c"
	if got := VerdictCommit("abc1234"); got != "4a3e10c" {
		t.Errorf("stamped binary: VerdictCommit = %q, want build stamp %q", got, "4a3e10c")
	}
}

// The +dirty suffix is the artifact's only way to say the tree was uncommitted
// when the binary was built, and nothing exercised it: the suite builds test
// binaries without -X, so Dirty is "" and the branch never ran. Deleting it
// would have broken no test.
//
// Both vars are restored, not just Commit: a leaked Dirty="1" would make
// TestVerdictCommitPrefersStamp fail depending on test order.
func TestVerdictCommitMarksADirtyBuild(t *testing.T) {
	defer func() { Commit, Dirty = "", "" }()

	Commit, Dirty = "4a3e10c", "1"
	if got := VerdictCommit("ignored"); got != "4a3e10c+dirty" {
		t.Errorf("dirty build: VerdictCommit = %q, want %q", got, "4a3e10c+dirty")
	}

	Dirty = "0"
	if got := VerdictCommit("ignored"); got != "4a3e10c" {
		t.Errorf("clean build: VerdictCommit = %q, want the bare revision %q", got, "4a3e10c")
	}

	// An unstamped binary cannot know whether the tree was dirty, so the
	// fallback is reported bare rather than decorated with a guess.
	Commit, Dirty = "", "1"
	if got := VerdictCommit("abc1234"); got != "abc1234" {
		t.Errorf("unstamped binary: VerdictCommit = %q, want the undecorated fallback %q", got, "abc1234")
	}
}

func TestStamped(t *testing.T) {
	defer func() { Commit = "" }()
	Commit = ""
	if Stamped() {
		t.Error("Stamped() = true with no commit")
	}
	Commit = "4a3e10c"
	if !Stamped() {
		t.Error("Stamped() = false with a commit")
	}
}
