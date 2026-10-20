package ledger

import (
	"bufio"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func mustParse(t *testing.T, s string) []Heading {
	t.Helper()
	hs, err := Parse(strings.NewReader(s))
	if err != nil {
		t.Fatal(err)
	}
	return hs
}

func TestParseClassifiesDefiningAndFollowUpHeadings(t *testing.T) {
	hs := mustParse(t, strings.Join([]string{
		"# OPEN QUESTIONS",
		"## OQ-052: the fixture driver's plan mode is a doc comment",
		"text mentioning OQ-052 is not a heading",
		"## OQ-052 UPDATE (D-062): the fixture driver now executes its plan",
		"## OQ-057: the oracle lock does not cover the program",
		"## OQ-057 MITIGATED, NOT RESOLVED (D-060): the swap is still possible",
		"## OQ-022b RESOLVED: QUIESCE now outlasts the SLO ceiling",
		"## Not an entry",
	}, "\n"))
	want := []struct {
		id   string
		kind Kind
		line int
	}{
		{"OQ-052", Defining, 2}, {"OQ-052", FollowUp, 4},
		{"OQ-057", Defining, 5}, {"OQ-057", FollowUp, 6},
		{"OQ-022b", FollowUp, 7},
	}
	if len(hs) != len(want) {
		t.Fatalf("got %d headings, want %d: %+v", len(hs), len(want), hs)
	}
	for i, w := range want {
		if hs[i].ID != w.id || hs[i].Kind != w.kind || hs[i].Line != w.line {
			t.Fatalf("heading %d: got %s %s line %d, want %s %s line %d", i, hs[i].ID, hs[i].Kind, hs[i].Line, w.id, w.kind, w.line)
		}
	}
}

// The collision OQ-051 recorded: two unrelated entries under one number.
// Mutation: dropping the duplicate branch in Check fails this test.
func TestCheckReportsADuplicatedDefiningHeading(t *testing.T) {
	hs := mustParse(t, "## D-031: The Phase 2 acceptance schedule\n\n## D-031: `proc.slow` throttles through the CFS quota\n")
	problems := Check(hs)
	if len(problems) != 1 || !strings.Contains(problems[0], "D-031 is defined twice: line 1 and line 3") {
		t.Fatalf("got %v", problems)
	}
}

func TestCheckReportsAFollowUpWithNothingToFollow(t *testing.T) {
	hs := mustParse(t, "## OQ-022 RESOLVED: QUIESCE now outlasts the SLO ceiling\n## OQ-022: mem.pressure cannot be withdrawn\n")
	problems := Check(hs)
	if len(problems) != 1 || !strings.Contains(problems[0], "no heading above it defines") {
		t.Fatalf("a follow-up that precedes its entry must be reported, got %v", problems)
	}
}

func TestParseRefusesAHeadingItCannotClassify(t *testing.T) {
	if _, err := Parse(strings.NewReader("## OQ-021 (the second one): io.latency\n")); err == nil {
		t.Fatal("a qualifier that is not a status word must be refused: it is how the old OQ-022 collision was spelled")
	}
}

// writeLedgers creates both ledger files, empty, in dir.
func writeLedgers(t *testing.T, dir string, names ...string) {
	t.Helper()
	if len(names) == 0 {
		names = []string{"DECISIONS.md", "OPEN_QUESTIONS.md"}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func envWith(value string) func(string) string {
	return func(k string) string {
		if k == PrivateEnv {
			return value
		}
		return ""
	}
}

// The ledgers live in <root>/.internal/ (D-105). Present, they are what Locate
// returns, and the private declaration never hides ledgers that are there: a
// checkout that can see them always runs the checks.
//
// Mutation: drop PrivateDir from the joined path and Locate looks at the
// repository root, so the paths differ and this test fails.
func TestLocateFindsTheLedgersUnderInternal(t *testing.T) {
	root := t.TempDir()
	writeLedgers(t, filepath.Join(root, ".internal"))
	want := Paths{
		Decisions:     filepath.Join(root, ".internal", "DECISIONS.md"),
		OpenQuestions: filepath.Join(root, ".internal", "OPEN_QUESTIONS.md"),
	}
	for _, env := range []string{"", "1"} {
		got, err := Locate(root, envWith(env))
		if err != nil {
			t.Fatalf("%s=%q: ledgers present under .internal/, got error %v", PrivateEnv, env, err)
		}
		if got != want {
			t.Fatalf("%s=%q: got %+v, want %+v", PrivateEnv, env, got, want)
		}
	}
}

// Absent ledgers with nothing declaring them private is a failure, and the
// message says where they belong and how to attach them. A stale copy at the
// old public path does not count as attached, and only the exact value "1"
// is a declaration.
//
// Mutation: accept any non-empty value as the declaration and the "0" and
// "true" rows come back as ErrPrivate.
func TestLocateRefusesAbsentLedgersThatNobodyDeclaredPrivate(t *testing.T) {
	root := t.TempDir()
	writeLedgers(t, root)
	for _, env := range []string{"", "0", "true"} {
		_, err := Locate(root, envWith(env))
		if err == nil {
			t.Fatalf("%s=%q: no ledgers under .internal/ and no declaration, got no error", PrivateEnv, env)
		}
		if errors.Is(err, ErrPrivate) {
			t.Fatalf("%s=%q: undeclared absence reported as the declared-private outcome: %v", PrivateEnv, env, err)
		}
		for _, want := range []string{".internal", "git clone", PrivateEnv + "=1"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("%s=%q: the error must name %q, got: %v", PrivateEnv, env, want, err)
			}
		}
	}
}

// Absent ledgers in a checkout that declares them private (public CI) is the
// declared outcome, ErrPrivate, which the ledger tests turn into a skip.
//
// Mutation: never return ErrPrivate and this fails.
func TestLocateReportsDeclaredPrivateLedgers(t *testing.T) {
	_, err := Locate(t.TempDir(), envWith("1"))
	if !errors.Is(err, ErrPrivate) {
		t.Fatalf("no ledgers and %s=1: got %v, want ErrPrivate", PrivateEnv, err)
	}
	const reason = "ledgers are private (D-105); the citation check runs where .internal/ is attached"
	if err.Error() != reason {
		t.Fatalf("skip reason:\n got  %q\n want %q", err.Error(), reason)
	}
}

// A .internal/ holding one ledger but not the other is a broken attachment,
// not a private checkout, and the declaration does not excuse it.
//
// Mutation: let the declaration cover a partial absence and this fails.
func TestLocateRefusesAHalfAttachedInternal(t *testing.T) {
	root := t.TempDir()
	writeLedgers(t, filepath.Join(root, ".internal"), "DECISIONS.md")
	_, err := Locate(root, envWith("1"))
	if err == nil || errors.Is(err, ErrPrivate) {
		t.Fatalf("OPEN_QUESTIONS.md missing beside DECISIONS.md: got %v, want a failure", err)
	}
	if !strings.Contains(err.Error(), "OPEN_QUESTIONS.md") {
		t.Fatalf("the error must name the missing ledger, got: %v", err)
	}
}

// repoRoot walks up from the test's directory to the directory holding go.mod.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the test directory")
		}
		dir = parent
	}
}

// attachedLedgers returns the repository root and the ledgers under its
// .internal/. A checkout that declared them private skips with Locate's
// reason; any other absence fails with Locate's instructions.
func attachedLedgers(t *testing.T) (string, Paths) {
	t.Helper()
	root := repoRoot(t)
	p, err := Locate(root, os.Getenv)
	if errors.Is(err, ErrPrivate) {
		t.Skip(err.Error())
	}
	if err != nil {
		t.Fatal(err)
	}
	return root, p
}

func ledgerHeadings(t *testing.T, path string) []Heading {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	hs, err := Parse(f)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return hs
}

// D-067. The committed ledgers hold one defining heading per id and every
// follow-up attaches to an entry above it.
func TestTheCommittedLedgersHaveUniqueAddresses(t *testing.T) {
	_, p := attachedLedgers(t)
	for _, path := range []string{p.Decisions, p.OpenQuestions} {
		if problems := Check(ledgerHeadings(t, path)); len(problems) > 0 {
			t.Errorf("%s:\n  %s", path, strings.Join(problems, "\n  "))
		}
	}
}

// D-067. Every citation of a ledger entry anywhere in the source tree (Go,
// Markdown, YAML, PowerShell) names an entry that exists. This is the check
// that would have caught the two Phase 1 comments promising "OQ-021" and
// "OQ-022" on a day the ledger ended at OQ-018. Historical documents under
// docs/protocol and docs/design are snapshots of what was true when they were
// written and are exempt; their citations are not rewritten.
func TestEveryLedgerCitationInTheTreeResolves(t *testing.T) {
	root, p := attachedLedgers(t)
	oq := IDs(ledgerHeadings(t, p.OpenQuestions))
	d := IDs(ledgerHeadings(t, p.Decisions))

	// The walk starts at the repository root, so it covers .internal/ too:
	// the private material's own citations must resolve as well.

	skipDir := map[string]bool{".git": true, "bin": true, "runs": true, "tmp": true, "corpus": true, "node_modules": true}
	exempt := []string{filepath.Join("docs", "protocol"), filepath.Join("docs", "design")}
	exts := map[string]bool{".go": true, ".md": true, ".yaml": true, ".yml": true, ".ps1": true, ".py": true}

	var dangling []string
	err := filepath.WalkDir(root, func(path string, e os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		if e.IsDir() {
			if skipDir[e.Name()] {
				return filepath.SkipDir
			}
			for _, x := range exempt {
				if rel == x {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if !exts[filepath.Ext(path)] {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 256*1024), 4*1024*1024)
		line := 0
		for sc.Scan() {
			line++
			for _, ref := range RefRE.FindAllString(sc.Text(), -1) {
				known := d
				if strings.HasPrefix(ref, "OQ-") {
					known = oq
				}
				if !known[ref] {
					dangling = append(dangling, rel+":"+itoa(line)+": "+ref)
				}
			}
		}
		return sc.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(dangling) > 0 {
		sort.Strings(dangling)
		t.Fatalf("%d citation(s) name a ledger entry that does not exist:\n  %s", len(dangling), strings.Join(dangling, "\n  "))
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// Ledger headings separate the id from the title with a colon: `## D-075: title`
// for an entry, `## OQ-052 UPDATE (D-062): title` for a follow-up. Both ledgers
// are written that way, so a parser that looked for any other separator would
// refuse every heading in them.
//
// Mutation: make Parse look for the em dash instead of the colon, and every
// heading here is refused as unclassifiable.
func TestParseReadsColonSeparatedHeadings(t *testing.T) {
	hs := mustParse(t, strings.Join([]string{
		"## D-075: search.strategy llm, a model-proposed schedule stream",
		"## OQ-052: the fixture driver's plan mode is a doc comment",
		"## OQ-052 UPDATE (D-062): the fixture driver now executes its plan",
		"## OQ-057 MITIGATED, NOT RESOLVED (D-060): the swap is still possible",
	}, "\n"))
	want := []struct {
		id   string
		kind Kind
		line int
	}{
		{"D-075", Defining, 1}, {"OQ-052", Defining, 2},
		{"OQ-052", FollowUp, 3}, {"OQ-057", FollowUp, 4},
	}
	if len(hs) != len(want) {
		t.Fatalf("got %d headings, want %d: %+v", len(hs), len(want), hs)
	}
	for i, w := range want {
		if hs[i].ID != w.id || hs[i].Kind != w.kind || hs[i].Line != w.line {
			t.Fatalf("heading %d: got %s %s line %d, want %s %s line %d", i, hs[i].ID, hs[i].Kind, hs[i].Line, w.id, w.kind, w.line)
		}
	}
}
