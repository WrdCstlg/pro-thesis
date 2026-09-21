package ledger

import (
	"bufio"
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

func ledgerHeadings(t *testing.T, root, name string) []Heading {
	t.Helper()
	f, err := os.Open(filepath.Join(root, name))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	hs, err := Parse(f)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return hs
}

// D-067. The committed ledgers hold one defining heading per id and every
// follow-up attaches to an entry above it.
func TestTheCommittedLedgersHaveUniqueAddresses(t *testing.T) {
	root := repoRoot(t)
	for _, name := range []string{"DECISIONS.md", "OPEN_QUESTIONS.md"} {
		if problems := Check(ledgerHeadings(t, root, name)); len(problems) > 0 {
			t.Errorf("%s:\n  %s", name, strings.Join(problems, "\n  "))
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
	root := repoRoot(t)
	oq := IDs(ledgerHeadings(t, root, "OPEN_QUESTIONS.md"))
	d := IDs(ledgerHeadings(t, root, "DECISIONS.md"))

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
