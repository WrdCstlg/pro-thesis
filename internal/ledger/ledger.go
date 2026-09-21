// Package ledger reads the headings of the project's two ledgers,
// DECISIONS.md and OPEN_QUESTIONS.md, and states the invariants a citation of
// an entry depends on.
//
// Both ledgers are append-only and both are cited from code comments, commit
// messages and each other, so an entry id is an address. Two properties make
// an address safe to follow:
//
//   - exactly one DEFINING heading per id (`## OQ-021: title`), and
//   - every FOLLOW-UP heading (`## OQ-052 UPDATE (D-062): …`,
//     `## OQ-057 MITIGATED, NOT RESOLVED (D-060): …`) attached to an id that
//     is already defined above it.
//
// Before D-067 neither held: two OQ ids and three D ids each named two
// unrelated entries (OQ-051), and two Phase 1 code comments cited "OQ-021" and
// "OQ-022" on a day the ledger ended at OQ-018. Those were promises of entries
// that were never written, which the next day's unrelated OQ-021 and OQ-022
// then silently answered. A duplicate announces itself; a pointer to the wrong
// entry does not, which is why both are checked mechanically rather than by
// re-reading.
package ledger

import (
	"bufio"
	"fmt"
	"io"
	"regexp"
	"strings"
)

// Kind says whether a heading introduces an entry or adds a section to one.
type Kind int

const (
	// Defining is `## <ID>: title`, the entry itself.
	Defining Kind = iota
	// FollowUp is `## <ID> <STATUS …>: title`, a later section about an entry
	// defined above it, carrying a status word.
	FollowUp
)

func (k Kind) String() string {
	if k == FollowUp {
		return "follow-up"
	}
	return "defining"
}

// Heading is one `## ` entry heading.
type Heading struct {
	ID   string
	Line int
	Kind Kind
	Text string
}

// headingRE matches an entry heading. The suffix letter is D-067's split of a
// collided id: the second topic that had shared a number keeps its position
// and becomes `<ID>b`.
var headingRE = regexp.MustCompile(`^## ((?:OQ|D)-\d{3}[a-z]?)(.*)$`)

// followUpRE is the status vocabulary a follow-up heading must carry before its
// colon. It is deliberately closed: a heading reusing an id with some other
// qualifier is more likely a collision than a follow-up, and is reported.
var followUpRE = regexp.MustCompile(`\b(RESOLVED|UPDATE|UPDATED|MITIGATED)\b`)

// RefRE matches a citation of a ledger entry in any text.
var RefRE = regexp.MustCompile(`\b(?:OQ|D)-\d{3}[a-z]?\b`)

// Parse reads every entry heading in a ledger. A `## ` line that names an id
// but is neither `<ID>: …` nor a follow-up with a status word is an error,
// because it is an address nobody can classify.
//
// The separator is the FIRST colon after the id. A title may contain colons of
// its own, and a status qualifier never does.
func Parse(r io.Reader) ([]Heading, error) {
	var (
		out  []Heading
		errs []string
		line int
	)
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 256*1024), 4*1024*1024)
	for sc.Scan() {
		line++
		m := headingRE.FindStringSubmatch(strings.TrimRight(sc.Text(), "\r"))
		if m == nil {
			continue
		}
		id, rest := m[1], m[2]
		h := Heading{ID: id, Line: line, Text: strings.TrimSpace(sc.Text())}
		sep := strings.Index(rest, ":")
		qualifier := rest
		if sep >= 0 {
			qualifier = rest[:sep]
		}
		switch {
		case strings.TrimSpace(qualifier) == "" && sep >= 0:
			h.Kind = Defining
		case followUpRE.MatchString(qualifier):
			h.Kind = FollowUp
		default:
			errs = append(errs, fmt.Sprintf("line %d: heading %q is neither `## %s: title` nor a follow-up "+
				"carrying RESOLVED, UPDATE(D) or MITIGATED", line, h.Text, id))
			continue
		}
		out = append(out, h)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(errs) > 0 {
		return out, fmt.Errorf("ledger: %s", strings.Join(errs, "; "))
	}
	return out, nil
}

// Check reports every violation of the two invariants, in line order: a second
// defining heading for an id, and a follow-up whose id is not defined above it.
func Check(headings []Heading) []string {
	var problems []string
	defined := map[string]int{}
	for _, h := range headings {
		switch h.Kind {
		case Defining:
			if first, dup := defined[h.ID]; dup {
				problems = append(problems, fmt.Sprintf("%s is defined twice: line %d and line %d", h.ID, first, h.Line))
				continue
			}
			defined[h.ID] = h.Line
		case FollowUp:
			if _, ok := defined[h.ID]; !ok {
				problems = append(problems, fmt.Sprintf("line %d: follow-up %q attaches to %s, which no heading above it defines",
					h.Line, h.Text, h.ID))
			}
		}
	}
	return problems
}

// IDs returns the set of ids a ledger defines.
func IDs(headings []Heading) map[string]bool {
	out := map[string]bool{}
	for _, h := range headings {
		if h.Kind == Defining {
			out[h.ID] = true
		}
	}
	return out
}
