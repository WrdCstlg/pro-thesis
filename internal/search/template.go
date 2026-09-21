package search

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"sort"
	"strings"
)

// ---------------------------------------------------------------------------
// Log-template coverage
//
// The base Phase 4 spec is explicit that this signal has the highest
// signal-to-integration-cost ratio in the system: normalise each log line by
// stripping variable fields, hash the template, count distinct. Zero build
// changes, any language, works on an unmodified target.
//
// NORMALISATION QUALITY IS SIGNAL QUALITY, in both directions:
//
//   - UNDER-normalising makes every line unique. Coverage then rises
//     monotonically with log volume and measures nothing. The specific trap is
//     an unbounded counter: `term=65` and `term=66` are the same code path.
//   - OVER-normalising collapses distinct code paths. The specific trap is
//     collapsing whole quoted strings or whole key=value pairs: in a structured
//     log the WORDS are the code path (`event=elected` vs `event=role_change`,
//     `code=not_leader` vs `code=lease_expired`) and only the NUMBERS are
//     variable.
//
// The rule this file follows, and the reason it is the right rule: replace a
// token by its SHAPE, never by its position, and never replace anything that is
// not numeric, temporal, or an address. A word is always kept.
// ---------------------------------------------------------------------------

// Placeholder tokens. Every one of them is DIGIT-FREE on purpose: the numeric
// rule runs last and has no word boundary at its left edge (it must, so that
// `kv-n1` normalises to `kv-n<NUM>`), so a placeholder containing a digit would
// be partially rewritten by a later rule.
const (
	PlaceholderTimestamp = "<TS>"
	PlaceholderUUID      = "<UUID>"
	PlaceholderIP        = "<IP>"
	PlaceholderDuration  = "<DUR>"
	PlaceholderSize      = "<SIZE>"
	PlaceholderHex       = "<HEX>"
	PlaceholderNumber    = "<NUM>"
	// PlaceholderTruncated marks a line cut at MaxLineBytes.
	PlaceholderTruncated = "<TRUNC>"
)

// MaxLineBytes bounds one line before normalisation.
//
// Without a bound, a panic stack or a dumped request body makes every line its
// own template and the coverage number becomes a log-volume number. With one, a
// pathological line is truncated and marked. The cost is real and is stated
// rather than hidden: two distinct lines that agree on their first MaxLineBytes
// collapse into one template. At 4 KiB that costs nothing on structured logs
// and bounds the damage on unstructured ones.
const MaxLineBytes = 4096

var (
	// ANSI SGR sequences. A target that colours its output by log level would
	// otherwise produce a different template for the same line depending on the
	// terminal state it inherited.
	reANSI = regexp.MustCompile("\x1b\\[[0-9;]*[A-Za-z]")

	// A leading RFC3339 stamp, as `docker logs --timestamps` emits. Phase 1's
	// collector already strips this into LogLine.Text, so this is defence
	// against a caller that passes a raw line, not a reimplementation of it.
	reLeadingRFC3339 = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}[Tt ]\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:[Zz]|[+-]\d{2}:?\d{2})?\s+`)

	// A leading Go `log` package date/time prefix, in every flag combination
	// the stdlib emits (date only, time only, both, with or without
	// microseconds).
	reLeadingGoLog = regexp.MustCompile(`^(?:\d{4}/\d{2}/\d{2}\s+)?(?:\d{2}:\d{2}:\d{2}(?:\.\d+)?\s+)?`)

	// Order below is load-bearing. Each rule consumes text that a later, more
	// general rule would otherwise mangle: a UUID contains hex runs, a duration
	// contains digits, an IP contains dots and digits.
	reUUID = regexp.MustCompile(`\b[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}\b`)

	reISOTimestamp = regexp.MustCompile(`\b\d{4}-\d{2}-\d{2}[Tt ]\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:[Zz]|[+-]\d{2}:?\d{2})?`)
	reClockTime    = regexp.MustCompile(`\b\d{1,2}:\d{2}:\d{2}(?:\.\d+)?\b`)

	reIPv4 = regexp.MustCompile(`\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}(?::\d{1,5})?\b`)
	// IPv6 runs after the clock rule so that `12:34:56` is a time, not an
	// address. Requires at least three groups, so a `host:port` pair is not
	// swallowed.
	reIPv6 = regexp.MustCompile(`\b(?:[0-9a-fA-F]{1,4}:){3,7}[0-9a-fA-F]{1,4}\b`)

	// Go-style durations, including compound forms: 500ms, 1.5s, 2m30s,
	// 1h2m3.5s, 900µs.
	reDuration = regexp.MustCompile(`\b\d+(?:\.\d+)?(?:ns|µs|us|ms|s|m|h)(?:\d+(?:\.\d+)?(?:ns|µs|us|ms|s|m|h))*\b`)

	reSize = regexp.MustCompile(`(?i)\b\d+(?:\.\d+)?\s?[kmgtp]i?b\b`)

	// A hex run: 0x-prefixed, or eight or more hex digits. The
	// ReplaceAllStringFunc below rejects an all-decimal match; see hexOrKeep.
	reHex = regexp.MustCompile(`\b(?:0[xX][0-9a-fA-F]+|[0-9a-fA-F]{8,})\b`)

	// The numeric rule. NO left word boundary, deliberately: `kv-n1` must
	// normalise to `kv-n<NUM>` so that three nodes of one service share a
	// template, and `\b` between `n` and `1` does not exist. No sign either:
	// `-1000` normalises to `-<NUM>`, which keeps a negative clock offset
	// visibly different from a positive one.
	reNumber = regexp.MustCompile(`\d+(?:\.\d+)?`)

	reSpaceRun = regexp.MustCompile(`[ \t]+`)
)

// hexOrKeep decides whether a reHex match is really a hex identifier.
//
// An all-decimal run is NOT: `commit_index=11430000` is a counter, and letting
// the hex rule take it would mean the same code path produced one template
// below 10,000,000 and another above it. That is the digit-boundary bug: an
// under-normalisation that appears only after a system has been running a
// while, which is the worst time to discover a coverage signal is broken. Such
// a match is returned unchanged so the numeric rule handles it uniformly.
//
// The residual, stated rather than hidden: a hex id that happens to contain no
// letter (about 2.3% of random 8-digit hex ids) is normalised as a number
// rather than as hex. Both are placeholders and both are length-independent, so
// the template still collapses across values of the same shape.
func hexOrKeep(m string) string {
	if len(m) > 1 && (m[1] == 'x' || m[1] == 'X') {
		return PlaceholderHex
	}
	for i := 0; i < len(m); i++ {
		switch {
		case m[i] >= 'a' && m[i] <= 'f', m[i] >= 'A' && m[i] <= 'F':
			return PlaceholderHex
		}
	}
	return m
}

// NormalizeLine reduces one log line to its template.
//
// It is a pure function of the line: no state, no configuration, no clock. That
// is what makes a template id comparable between two worlds, between two runs,
// and between two machines.
func NormalizeLine(line string) string {
	s := line
	if len(s) > MaxLineBytes {
		s = s[:MaxLineBytes] + " " + PlaceholderTruncated
	}
	s = reANSI.ReplaceAllString(s, "")
	s = strings.TrimSpace(s)

	// Leading stamps, in the order they nest: a docker RFC3339 prefix can sit
	// in front of a target's own Go log prefix.
	s = reLeadingRFC3339.ReplaceAllString(s, "")
	s = reLeadingGoLog.ReplaceAllString(s, "")

	s = reUUID.ReplaceAllString(s, PlaceholderUUID)
	s = reISOTimestamp.ReplaceAllString(s, PlaceholderTimestamp)
	s = reClockTime.ReplaceAllString(s, PlaceholderTimestamp)
	s = reIPv4.ReplaceAllString(s, PlaceholderIP)
	s = reIPv6.ReplaceAllString(s, PlaceholderIP)
	s = reDuration.ReplaceAllString(s, PlaceholderDuration)
	s = reSize.ReplaceAllString(s, PlaceholderSize)
	s = reHex.ReplaceAllStringFunc(s, hexOrKeep)
	s = reNumber.ReplaceAllString(s, PlaceholderNumber)

	s = reSpaceRun.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

// TemplateID is the content address of a normalised template: the first 12 hex
// digits of SHA-256 over the template text, prefixed so it is greppable.
//
// The ID exists for display and for the rare-template index. Nothing in this
// package keys a map by it (TemplateSet keys by the template TEXT) so a
// truncation collision cannot silently merge two templates.
func TemplateID(norm string) string {
	sum := sha256.Sum256([]byte(norm))
	return "t_" + hex.EncodeToString(sum[:6])
}

// TemplateStat is one distinct template and what has been seen of it.
type TemplateStat struct {
	// ID is the content address.
	ID string
	// Template is the normalised text.
	Template string
	// Count is how many lines reduced to this template.
	Count int64
	// Nodes are the logical node ids that emitted it, sorted and deduplicated.
	// A template seen on one node only is a different fact from one seen on all
	// three, and the Saboteur's ranking may want to know which.
	Nodes []string
	// Example is the first raw line that produced this template, kept so a
	// human reading a coverage report can see what the template stands for.
	Example string
}

// TemplateSet is a set of distinct log templates.
//
// It is keyed by template TEXT, not by hash: a coverage counter that could
// silently merge two code paths because two hashes collided would be a coverage
// counter nobody should trust.
type TemplateSet struct {
	byText map[string]*TemplateStat
	// lines is the total number of lines offered, including duplicates and
	// including blank lines that were skipped. It is what makes "1 template
	// from 40,000 lines" distinguishable from "1 template from 1 line".
	lines int64
}

// NewTemplateSet returns an empty set.
func NewTemplateSet() *TemplateSet {
	return &TemplateSet{byText: map[string]*TemplateStat{}}
}

// Add normalises one line and records it, returning the template id and whether
// it was new to this set.
//
// A blank line is ignored and reported as (\"\", false): a blank line is not
// evidence of a code path.
func (s *TemplateSet) Add(node, line string) (string, bool) {
	if s.byText == nil {
		s.byText = map[string]*TemplateStat{}
	}
	s.lines++
	norm := NormalizeLine(line)
	if norm == "" {
		return "", false
	}
	st, ok := s.byText[norm]
	if !ok {
		st = &TemplateStat{ID: TemplateID(norm), Template: norm, Example: line}
		s.byText[norm] = st
	}
	st.Count++
	if node != "" {
		st.Nodes = insertSorted(st.Nodes, node)
	}
	return st.ID, !ok
}

// AddLines records every line of one node.
func (s *TemplateSet) AddLines(node string, lines []string) (newCount int64) {
	for _, ln := range lines {
		if _, isNew := s.Add(node, ln); isNew {
			newCount++
		}
	}
	return newCount
}

// Len is the number of distinct templates.
func (s *TemplateSet) Len() int { return len(s.byText) }

// Lines is the number of lines offered, including duplicates.
func (s *TemplateSet) Lines() int64 { return s.lines }

// IDs returns every template id, sorted. Sorted because a search that iterated
// a Go map would make its own decisions depend on map iteration order, which is
// randomized: and A.8 requires the same seed to expand the same tree.
func (s *TemplateSet) IDs() []string {
	out := make([]string, 0, len(s.byText))
	for _, st := range s.byText {
		out = append(out, st.ID)
	}
	sort.Strings(out)
	return out
}

// Has reports whether the set contains a template with this id.
func (s *TemplateSet) Has(id string) bool {
	for _, st := range s.byText {
		if st.ID == id {
			return true
		}
	}
	return false
}

// Stats returns every template, ordered by id.
func (s *TemplateSet) Stats() []TemplateStat {
	out := make([]TemplateStat, 0, len(s.byText))
	for _, st := range s.byText {
		c := *st
		c.Nodes = append([]string(nil), st.Nodes...)
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Merge folds other into s and returns how many of other's templates were new
// to s. This is the per-world-into-global step of the coverage loop.
func (s *TemplateSet) Merge(other *TemplateSet) int64 {
	if other == nil {
		return 0
	}
	if s.byText == nil {
		s.byText = map[string]*TemplateStat{}
	}
	// Iterate in id order so that Example and Nodes accumulate deterministically
	// regardless of the order the templates were first seen in.
	keys := make([]string, 0, len(other.byText))
	for k := range other.byText {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var added int64
	for _, k := range keys {
		o := other.byText[k]
		st, ok := s.byText[k]
		if !ok {
			cp := *o
			cp.Nodes = append([]string(nil), o.Nodes...)
			s.byText[k] = &cp
			added++
			continue
		}
		st.Count += o.Count
		for _, n := range o.Nodes {
			st.Nodes = insertSorted(st.Nodes, n)
		}
	}
	s.lines += other.lines
	return added
}

// NewAgainst reports how many of s's templates are absent from base, without
// modifying either set. It is the delta a world is scored on before the world's
// coverage is folded into the global set.
func (s *TemplateSet) NewAgainst(base *TemplateSet) int64 {
	if base == nil {
		return int64(len(s.byText))
	}
	var n int64
	for k := range s.byText {
		if _, ok := base.byText[k]; !ok {
			n++
		}
	}
	return n
}

// insertSorted inserts v into a sorted slice, keeping it sorted and unique.
func insertSorted(ss []string, v string) []string {
	i := sort.SearchStrings(ss, v)
	if i < len(ss) && ss[i] == v {
		return ss
	}
	ss = append(ss, "")
	copy(ss[i+1:], ss[i:])
	ss[i] = v
	return ss
}
