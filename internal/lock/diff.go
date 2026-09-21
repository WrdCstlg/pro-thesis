package lock

import (
	"fmt"
	"sort"
	"strings"
)

// ChangeKind classifies one manifest movement.
type ChangeKind string

const (
	ChangeOracleAdded    ChangeKind = "oracle_file_added"
	ChangeOracleRemoved  ChangeKind = "oracle_file_removed"
	ChangeOracleModified ChangeKind = "oracle_file_modified"
	ChangeConfigAdded    ChangeKind = "config_added"
	ChangeConfigRemoved  ChangeKind = "config_removed"
	ChangeConfigChanged  ChangeKind = "config_changed"
	ChangeSchema         ChangeKind = "manifest_schema_changed"
)

// Change is one difference between the locked manifest and the recomputed one.
//
// It exists because "lock mismatch" on its own is a dead end. The message that
// fires here is the message a human is escalated to; it has to say which file,
// which budget, which allow-list entry, and what the old and new values were,
// or the first thing that human does is diff by hand.
type Change struct {
	Kind ChangeKind
	// Subject is the oracle file path or the config path that moved.
	Subject string
	// Was is the locked value ("" when the subject was absent).
	Was string
	// Now is the recomputed value ("" when the subject is now absent).
	Now string
}

// String renders one change as the line a human reads.
func (c Change) String() string {
	switch c.Kind {
	case ChangeOracleAdded:
		return fmt.Sprintf("oracle file ADDED:    %s (%s)", c.Subject, c.Now)
	case ChangeOracleRemoved:
		return fmt.Sprintf("oracle file REMOVED:  %s (was %s)", c.Subject, c.Was)
	case ChangeOracleModified:
		return fmt.Sprintf("oracle file EDITED:   %s\n      was %s\n      now %s", c.Subject, c.Was, c.Now)
	case ChangeConfigAdded:
		return fmt.Sprintf("config ADDED:         %s = %s", c.Subject, c.Now)
	case ChangeConfigRemoved:
		return fmt.Sprintf("config REMOVED:       %s (was %s)", c.Subject, c.Was)
	case ChangeConfigChanged:
		return fmt.Sprintf("config CHANGED:       %s: %s -> %s", c.Subject, c.Was, c.Now)
	case ChangeSchema:
		return fmt.Sprintf("manifest schema:      %s -> %s (the LOCK FORMAT changed, not your config; "+
			"re-lock with a reason naming the thesis version)", c.Was, c.Now)
	default:
		return fmt.Sprintf("%s: %s: %s -> %s", c.Kind, c.Subject, c.Was, c.Now)
	}
}

// Diff reports every movement from was (the locked manifest) to now (the
// recomputed one), in a stable order: schema first, then oracle files by path,
// then config by path.
func Diff(was, now Manifest) []Change {
	var out []Change
	if was.Schema != now.Schema {
		out = append(out, Change{Kind: ChangeSchema, Subject: "schema", Was: was.Schema, Now: now.Schema})
	}
	out = append(out, diffOracles(was.Oracles, now.Oracles)...)
	out = append(out, diffConfig(was.Config, now.Config)...)
	return out
}

func diffOracles(was, now []OracleFile) []Change {
	oldByPath := make(map[string]OracleFile, len(was))
	for _, f := range was {
		oldByPath[f.Path] = f
	}
	newByPath := make(map[string]OracleFile, len(now))
	for _, f := range now {
		newByPath[f.Path] = f
	}
	paths := unionKeys(oldByPath, newByPath)

	var out []Change
	for _, p := range paths {
		o, hadOld := oldByPath[p]
		n, hasNew := newByPath[p]
		switch {
		case !hadOld:
			out = append(out, Change{Kind: ChangeOracleAdded, Subject: p, Now: describeFile(n)})
		case !hasNew:
			out = append(out, Change{Kind: ChangeOracleRemoved, Subject: p, Was: describeFile(o)})
		case o.SHA256 != n.SHA256 || o.Size != n.Size:
			out = append(out, Change{Kind: ChangeOracleModified, Subject: p,
				Was: describeFile(o), Now: describeFile(n)})
		}
	}
	return out
}

func describeFile(f OracleFile) string {
	return fmt.Sprintf("%s, %d bytes", f.SHA256, f.Size)
}

func unionKeys(a, b map[string]OracleFile) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(a)+len(b))
	for k := range a {
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	for k := range b {
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// diffConfig compares the covered configuration leaves.
//
// Entries sharing a path form a multiset (that is how `perturber.allow` is
// projected), so the comparison is per-path and multiset-aware. The common case
// (one value on each side) is rendered as the plain "old -> new" a reader
// wants; a genuine set edit is rendered as the elements added and removed.
func diffConfig(was, now []ConfigEntry) []Change {
	oldByPath := groupEntries(was)
	newByPath := groupEntries(now)

	seen := map[string]bool{}
	paths := make([]string, 0, len(oldByPath)+len(newByPath))
	for k := range oldByPath {
		if !seen[k] {
			seen[k] = true
			paths = append(paths, k)
		}
	}
	for k := range newByPath {
		if !seen[k] {
			seen[k] = true
			paths = append(paths, k)
		}
	}
	sort.Strings(paths)

	var out []Change
	for _, p := range paths {
		o := oldByPath[p]
		n := newByPath[p]
		if equalValueSlices(o, n) {
			continue
		}
		switch {
		case len(o) == 0:
			out = append(out, Change{Kind: ChangeConfigAdded, Subject: p, Now: renderValues(n)})
		case len(n) == 0:
			out = append(out, Change{Kind: ChangeConfigRemoved, Subject: p, Was: renderValues(o)})
		case len(o) == 1 && len(n) == 1:
			out = append(out, Change{Kind: ChangeConfigChanged, Subject: p, Was: o[0], Now: n[0]})
		default:
			added, removed := multisetDelta(o, n)
			var parts []string
			if len(removed) > 0 {
				parts = append(parts, "removed "+strings.Join(removed, ", "))
			}
			if len(added) > 0 {
				parts = append(parts, "added "+strings.Join(added, ", "))
			}
			out = append(out, Change{Kind: ChangeConfigChanged, Subject: p,
				Was: renderValues(o), Now: renderValues(n) + " [" + strings.Join(parts, "; ") + "]"})
		}
	}
	return out
}

// groupEntries indexes rendered leaf values by path. Values are pre-sorted by
// ProjectConfig, so the slices compare elementwise.
func groupEntries(es []ConfigEntry) map[string][]string {
	m := map[string][]string{}
	for _, e := range es {
		m[e.Path] = append(m[e.Path], renderEntry(e))
	}
	for k := range m {
		sort.Strings(m[k])
	}
	return m
}

// renderEntry renders one leaf for a human and for comparison. The non-scalar
// kinds are spelled out so that an explicit `null`, an empty list and an empty
// mapping are not all shown as nothing.
func renderEntry(e ConfigEntry) string {
	switch e.Kind {
	case KindNull:
		return "null"
	case KindEmptySeq:
		return "[]"
	case KindEmptyMap:
		return "{}"
	default:
		return e.Value
	}
}

func renderValues(vs []string) string {
	if len(vs) == 1 {
		return vs[0]
	}
	return "[" + strings.Join(vs, ", ") + "]"
}

func equalValueSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// multisetDelta returns what b gained and what b lost relative to a.
func multisetDelta(a, b []string) (added, removed []string) {
	count := map[string]int{}
	for _, v := range a {
		count[v]++
	}
	for _, v := range b {
		count[v]--
	}
	keys := make([]string, 0, len(count))
	for k := range count {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		switch {
		case count[k] > 0:
			for i := 0; i < count[k]; i++ {
				removed = append(removed, k)
			}
		case count[k] < 0:
			for i := 0; i < -count[k]; i++ {
				added = append(added, k)
			}
		}
	}
	return added, removed
}

// FormatChanges renders a change list as the indented block `verify` prints.
func FormatChanges(cs []Change) string {
	if len(cs) == 0 {
		return ""
	}
	var b strings.Builder
	for _, c := range cs {
		b.WriteString("  - ")
		b.WriteString(c.String())
		b.WriteString("\n")
	}
	return b.String()
}
