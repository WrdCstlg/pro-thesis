package schema

import (
	"fmt"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// The fault grammar
//
//	KIND(TARGET[, PARAMS])@WINDOW
//
//	WINDOW : start_ms..end_ms   milliseconds relative to DRIVE start
//	TARGET : node       n1
//	         wildcard   kv:*
//	         role       role:leader
//	         edge       n1<->n2
//	         quorum     minority(kv) | majority(kv) | any(2, kv)
//	PARAMS : positional or name=value, comma separated
//
// The canonical string is the WIRE FORM. `.thesis` fault schedules and the
// verdict's `surviving_faults` are both arrays of these strings, and the
// directive fixes their spelling directly:
//
//	"proc.pause(role:leader)@8200..15100"
//	"net.partition(minority(kv))@8400..14900"
//
// The AST below exists only in memory. It is deliberately NOT given JSON
// methods: the world file's canonical encoder is reflective and rejects any
// type that implements json.Marshaler, because the two would then disagree
// about the type's representation.
// ---------------------------------------------------------------------------

// TargetKind discriminates the five target shapes.
type TargetKind string

const (
	// TargetNode names one logical node: n1
	TargetNode TargetKind = "node"
	// TargetWildcard names every node of a service: kv:*
	TargetWildcard TargetKind = "wildcard"
	// TargetRole names whichever node currently holds a role: role:leader
	TargetRole TargetKind = "role"
	// TargetEdge names the link between two nodes: n1<->n2
	TargetEdge TargetKind = "edge"
	// TargetQuorum names a quorum-relative subset: minority(kv)
	TargetQuorum TargetKind = "quorum"
)

// QuorumFunc is the function of a quorum target.
type QuorumFunc string

const (
	// QuorumMinority selects floor((n-1)/2) nodes of the scope.
	QuorumMinority QuorumFunc = "minority"
	// QuorumMajority selects floor(n/2)+1 nodes of the scope.
	QuorumMajority QuorumFunc = "majority"
	// QuorumAny selects exactly N nodes of the scope.
	//
	// ADDITIVE. Directive 4.3 lists only minority() and majority(), but a
	// fixed-cardinality selector is needed to express "one node of three"
	// without naming a concrete node, which is what makes a schedule portable
	// across topology variants. See DECISIONS.md.
	QuorumAny QuorumFunc = "any"
)

// Target is the parsed TARGET of a fault.
//
// Exactly one shape's fields are populated; Kind says which.
type Target struct {
	Kind TargetKind

	// Node is the node id when Kind is TargetNode.
	Node string
	// Service is the service name when Kind is TargetWildcard (the "kv" of
	// "kv:*").
	Service string
	// Role is the role name when Kind is TargetRole (the "leader" of
	// "role:leader").
	Role string
	// A and B are the endpoints when Kind is TargetEdge. They are stored in
	// lexicographic order: a partition is bidirectional, so n2<->n1 and
	// n1<->n2 denote the same fault and must hash the same.
	A string
	B string
	// Func, Scope and N describe a quorum target. N is meaningful only for
	// QuorumAny.
	Func  QuorumFunc
	Scope string
	N     int
}

// String renders the target in canonical form.
func (t Target) String() string {
	switch t.Kind {
	case TargetNode:
		return t.Node
	case TargetWildcard:
		return t.Service + ":*"
	case TargetRole:
		return "role:" + t.Role
	case TargetEdge:
		return t.A + "<->" + t.B
	case TargetQuorum:
		if t.Func == QuorumAny {
			return fmt.Sprintf("any(%d, %s)", t.N, t.Scope)
		}
		return string(t.Func) + "(" + t.Scope + ")"
	}
	return ""
}

// IsDynamic reports whether the target binds to concrete nodes only at
// injection time, from live cluster state.
//
// This is why the world file carries a realized schedule alongside the planned
// one: Raft leadership moves at runtime, so a world recording only
// proc.pause(role:leader) does not necessarily replay the same physical fault.
func (t Target) IsDynamic() bool {
	switch t.Kind {
	case TargetRole, TargetQuorum, TargetWildcard:
		return true
	}
	return false
}

// Validate checks a target's internal consistency.
func (t Target) Validate() error {
	switch t.Kind {
	case TargetNode:
		if !validIdent(t.Node) {
			return fmt.Errorf("target: %q is not a node id", t.Node)
		}
	case TargetWildcard:
		if !validIdent(t.Service) {
			return fmt.Errorf("target: %q is not a service name", t.Service)
		}
	case TargetRole:
		if !validIdent(t.Role) {
			return fmt.Errorf("target: %q is not a role name", t.Role)
		}
	case TargetEdge:
		if !validIdent(t.A) || !validIdent(t.B) {
			return fmt.Errorf("target: %q<->%q is not an edge between two node ids", t.A, t.B)
		}
		if t.A == t.B {
			return fmt.Errorf("target: edge %s<->%s connects a node to itself", t.A, t.B)
		}
		if t.A > t.B {
			return fmt.Errorf("target: edge endpoints are not in canonical order (%s<->%s)", t.A, t.B)
		}
	case TargetQuorum:
		if !validIdent(t.Scope) {
			return fmt.Errorf("target: %q is not a quorum scope", t.Scope)
		}
		switch t.Func {
		case QuorumMinority, QuorumMajority:
			if t.N != 0 {
				return fmt.Errorf("target: %s() takes no count", t.Func)
			}
		case QuorumAny:
			if t.N < 1 {
				return fmt.Errorf("target: any(%d, %s): count must be >= 1", t.N, t.Scope)
			}
		default:
			return fmt.Errorf("target: unknown quorum function %q (want minority, majority or any)", t.Func)
		}
	default:
		return fmt.Errorf("target: unknown target kind %q", t.Kind)
	}
	return nil
}

// ParseTarget parses one TARGET.
func ParseTarget(s string) (Target, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Target{}, fmt.Errorf("target: empty")
	}

	// Edge, checked first: "<->" cannot appear in any other shape.
	if i := strings.Index(s, "<->"); i >= 0 {
		a := strings.TrimSpace(s[:i])
		b := strings.TrimSpace(s[i+3:])
		if strings.Contains(b, "<->") {
			return Target{}, fmt.Errorf("target: %q has more than one edge; an edge joins exactly two nodes", s)
		}
		if a > b {
			a, b = b, a
		}
		t := Target{Kind: TargetEdge, A: a, B: b}
		if err := t.Validate(); err != nil {
			return Target{}, err
		}
		return t, nil
	}

	// Quorum: an identifier followed by a parenthesised argument list.
	if open := strings.IndexByte(s, '('); open >= 0 {
		if !strings.HasSuffix(s, ")") {
			return Target{}, fmt.Errorf("target: %q is missing its closing ')'", s)
		}
		fn := QuorumFunc(strings.TrimSpace(s[:open]))
		args, err := splitTopLevel(s[open+1:len(s)-1], ',')
		if err != nil {
			return Target{}, fmt.Errorf("target %q: %w", s, err)
		}
		t := Target{Kind: TargetQuorum, Func: fn}
		switch fn {
		case QuorumMinority, QuorumMajority:
			if len(args) != 1 || args[0] == "" {
				return Target{}, fmt.Errorf("target: %s(...) takes exactly one scope, got %q", fn, s)
			}
			t.Scope = args[0]
		case QuorumAny:
			if len(args) != 2 {
				return Target{}, fmt.Errorf("target: any(...) takes a count and a scope, e.g. any(2, kv), got %q", s)
			}
			n, err := strconv.Atoi(args[0])
			if err != nil {
				return Target{}, fmt.Errorf("target: any(%q, ...): count is not an integer", args[0])
			}
			t.N = n
			t.Scope = args[1]
		default:
			return Target{}, fmt.Errorf("target: unknown quorum function %q in %q "+
				"(want minority, majority or any)", fn, s)
		}
		if err := t.Validate(); err != nil {
			return Target{}, err
		}
		return t, nil
	}

	// Role.
	if rest, ok := strings.CutPrefix(s, "role:"); ok {
		t := Target{Kind: TargetRole, Role: strings.TrimSpace(rest)}
		if err := t.Validate(); err != nil {
			return Target{}, err
		}
		return t, nil
	}

	// Service wildcard.
	if rest, ok := strings.CutSuffix(s, ":*"); ok {
		t := Target{Kind: TargetWildcard, Service: strings.TrimSpace(rest)}
		if err := t.Validate(); err != nil {
			return Target{}, err
		}
		return t, nil
	}

	if strings.ContainsRune(s, ':') {
		return Target{}, fmt.Errorf("target: %q is not a target "+
			"(a colon introduces role:<name> or <service>:*)", s)
	}

	t := Target{Kind: TargetNode, Node: s}
	if err := t.Validate(); err != nil {
		return Target{}, err
	}
	return t, nil
}

// ---------------------------------------------------------------------------
// FaultSpec
// ---------------------------------------------------------------------------

// Param is one bound fault parameter.
//
// Value is the EXACT source text. It is never re-derived from a parsed number:
// 0.05 must not re-render as 0.05000000000000000277, because that would change
// the canonical fault string and therefore the hash of every world containing
// it.
type Param struct {
	Name  string
	Value string
}

// FaultSpec is a parsed fault. Its canonical string form is the wire form.
type FaultSpec struct {
	Kind   FaultKind
	Target Target
	// Params is every declared parameter of the kind, in declared order, with
	// defaults materialized. Parsing normalizes into this shape, so a
	// FaultSpec produced by ParseFault is always complete.
	Params []Param
	// StartMS and EndMS are milliseconds relative to DRIVE start. For a
	// durative kind the fault is active over [StartMS, EndMS) and withdrawn at
	// EndMS. For an instantaneous kind it fires at StartMS and [StartMS, EndMS]
	// is the window in which the resulting disturbance is expected.
	StartMS int64
	EndMS   int64
}

// Param returns the bound value of the named parameter.
func (f FaultSpec) Param(name string) (string, bool) {
	for _, p := range f.Params {
		if p.Name == name {
			return p.Value, true
		}
	}
	return "", false
}

// DurationMS is the window length.
func (f FaultSpec) DurationMS() int64 { return f.EndMS - f.StartMS }

// String renders the canonical wire form.
//
// Every declared parameter is emitted, named, in declared order, including
// those that took their default. Materializing defaults is what makes an
// archived world self-contained (invariant I2): changing a default in a later
// release can never silently change what a committed regression replays.
func (f FaultSpec) String() string {
	var b strings.Builder
	b.WriteString(string(f.Kind))
	b.WriteByte('(')
	b.WriteString(f.Target.String())
	for _, p := range f.Params {
		b.WriteString(", ")
		b.WriteString(p.Name)
		b.WriteByte('=')
		b.WriteString(p.Value)
	}
	b.WriteString(")@")
	b.WriteString(strconv.FormatInt(f.StartMS, 10))
	b.WriteString("..")
	b.WriteString(strconv.FormatInt(f.EndMS, 10))
	return b.String()
}

// Validate re-checks a FaultSpec that was built by hand rather than parsed.
func (f FaultSpec) Validate() error {
	decl, ok := LookupFaultKind(f.Kind)
	if !ok {
		return fmt.Errorf("fault: unknown kind %q (want one of %s)", f.Kind, joinFaultKinds(AllFaultKinds))
	}
	if err := f.Target.Validate(); err != nil {
		return fmt.Errorf("%s: %w", f.Kind, err)
	}
	if f.Target.Kind == TargetEdge && !decl.AllowEdge {
		return fmt.Errorf("%s: an edge target (%s) is only meaningful for the network family; "+
			"%s acts on a node", f.Kind, f.Target, f.Kind)
	}
	if len(f.Params) != len(decl.Params) {
		return fmt.Errorf("%s: has %d parameters, the canonical form carries all %d (%s)",
			f.Kind, len(f.Params), len(decl.Params), strings.Join(decl.ParamNames(), ", "))
	}
	for i, p := range f.Params {
		if p.Name != decl.Params[i].Name {
			return fmt.Errorf("%s: parameter %d is %q, want %q (canonical order is %s)",
				f.Kind, i, p.Name, decl.Params[i].Name, strings.Join(decl.ParamNames(), ", "))
		}
		if err := validateParamValue(f.Kind, decl.Params[i], p.Value); err != nil {
			return err
		}
	}
	if f.EndMS < f.StartMS {
		return fmt.Errorf("%s: window %d..%d ends before it starts", f.Kind, f.StartMS, f.EndMS)
	}
	return nil
}

// ParseFault parses one canonical (or human-written) fault string.
func ParseFault(s string) (FaultSpec, error) {
	orig := s
	s = strings.TrimSpace(s)
	if s == "" {
		return FaultSpec{}, fmt.Errorf("fault: empty string (want KIND(TARGET[, PARAMS])@start_ms..end_ms)")
	}

	at := strings.LastIndexByte(s, '@')
	if at < 0 {
		return FaultSpec{}, fmt.Errorf("fault %q: missing @start_ms..end_ms window", orig)
	}
	spec := strings.TrimSpace(s[:at])
	window := strings.TrimSpace(s[at+1:])

	start, end, err := parseWindow(window)
	if err != nil {
		return FaultSpec{}, fmt.Errorf("fault %q: %w", orig, err)
	}

	open := strings.IndexByte(spec, '(')
	if open < 0 || !strings.HasSuffix(spec, ")") {
		return FaultSpec{}, fmt.Errorf("fault %q: want KIND(TARGET[, PARAMS])@start..end", orig)
	}
	kind := FaultKind(strings.TrimSpace(spec[:open]))
	decl, ok := LookupFaultKind(kind)
	if !ok {
		return FaultSpec{}, fmt.Errorf("fault %q: unknown kind %q (want one of %s)",
			orig, kind, joinFaultKinds(AllFaultKinds))
	}

	args, err := splitTopLevel(spec[open+1:len(spec)-1], ',')
	if err != nil {
		return FaultSpec{}, fmt.Errorf("fault %q: %w", orig, err)
	}
	if len(args) == 0 || args[0] == "" {
		return FaultSpec{}, fmt.Errorf("fault %q: %s needs a target, e.g. %s(n1)@%d..%d",
			orig, kind, kind, start, end)
	}

	target, err := ParseTarget(args[0])
	if err != nil {
		return FaultSpec{}, fmt.Errorf("fault %q: %w", orig, err)
	}

	params, err := bindParams(kind, decl, args[1:])
	if err != nil {
		return FaultSpec{}, fmt.Errorf("fault %q: %w", orig, err)
	}

	f := FaultSpec{Kind: kind, Target: target, Params: params, StartMS: start, EndMS: end}
	if err := f.Validate(); err != nil {
		return FaultSpec{}, fmt.Errorf("fault %q: %w", orig, err)
	}
	return f, nil
}

// ParseFaults parses a whole schedule, reporting every bad entry at once.
func ParseFaults(ss []string) ([]FaultSpec, error) {
	out := make([]FaultSpec, 0, len(ss))
	var errs ValidationErrors
	for i, s := range ss {
		f, err := ParseFault(s)
		if err != nil {
			errs.Add(indexPath("fault_schedule.planned", i), "%s", err.Error())
			continue
		}
		out = append(out, f)
	}
	if err := errs.OrNil(); err != nil {
		return nil, err
	}
	return out, nil
}

// CanonicalFault parses s and returns its canonical spelling. It is the
// normalization used before a fault string enters a world file.
func CanonicalFault(s string) (string, error) {
	f, err := ParseFault(s)
	if err != nil {
		return "", err
	}
	return f.String(), nil
}

// parseWindow parses "start..end".
func parseWindow(w string) (start, end int64, err error) {
	i := strings.Index(w, "..")
	if i < 0 {
		return 0, 0, fmt.Errorf("window %q: want start_ms..end_ms", w)
	}
	sTxt := strings.TrimSpace(w[:i])
	eTxt := strings.TrimSpace(w[i+2:])
	start, err = strconv.ParseInt(sTxt, 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("window %q: start_ms %q is not an integer", w, sTxt)
	}
	end, err = strconv.ParseInt(eTxt, 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("window %q: end_ms %q is not an integer", w, eTxt)
	}
	if end < start {
		return 0, 0, fmt.Errorf("window %q: ends before it starts", w)
	}
	return start, end, nil
}

// bindParams binds positional and named arguments against the declared
// parameter list and materializes defaults.
//
// Positional arguments bind in declared order and must precede every named
// argument, because "net.latency(n1, jitter=5, 40)" has no defensible reading.
func bindParams(kind FaultKind, decl KindDecl, args []string) ([]Param, error) {
	bound := make([]string, len(decl.Params))
	set := make([]bool, len(decl.Params))

	seenNamed := false
	for i, a := range args {
		if a == "" {
			return nil, fmt.Errorf("%s: empty parameter at position %d", kind, i+1)
		}
		name, value, isNamed := strings.Cut(a, "=")
		name = strings.TrimSpace(name)
		if isNamed {
			seenNamed = true
			value = strings.TrimSpace(value)
			idx := paramIndex(decl, name)
			if idx < 0 {
				return nil, fmt.Errorf("%s: unknown parameter %q (want %s)",
					kind, name, paramListText(decl))
			}
			if set[idx] {
				return nil, fmt.Errorf("%s: parameter %s given twice", kind, name)
			}
			bound[idx] = value
			set[idx] = true
			continue
		}
		if seenNamed {
			return nil, fmt.Errorf("%s: positional parameter %q follows a named one; "+
				"put positional parameters first", kind, a)
		}
		if i >= len(decl.Params) {
			return nil, fmt.Errorf("%s: takes %s, got %d parameter(s)",
				kind, paramListText(decl), len(args))
		}
		if set[i] {
			return nil, fmt.Errorf("%s: parameter %s given twice", kind, decl.Params[i].Name)
		}
		bound[i] = strings.TrimSpace(a)
		set[i] = true
	}

	out := make([]Param, 0, len(decl.Params))
	for i, p := range decl.Params {
		if !set[i] {
			if p.Required() {
				return nil, fmt.Errorf("%s: parameter %s is required (%s%s)",
					kind, p.Name, p.Type, unitSuffix(p))
			}
			bound[i] = p.Default
		}
		if err := validateParamValue(kind, p, bound[i]); err != nil {
			return nil, err
		}
		out = append(out, Param{Name: p.Name, Value: bound[i]})
	}
	return out, nil
}

func paramIndex(decl KindDecl, name string) int {
	for i, p := range decl.Params {
		if p.Name == name {
			return i
		}
	}
	return -1
}

func paramListText(decl KindDecl) string {
	if len(decl.Params) == 0 {
		return "no parameters"
	}
	return "(" + strings.Join(decl.ParamNames(), ", ") + ")"
}

// splitTopLevel splits s on sep, ignoring separators nested inside parentheses.
// This is what lets "minority(kv)" and "any(2, kv)" appear as a single target
// argument alongside the fault's own comma-separated parameters.
func splitTopLevel(s string, sep byte) ([]string, error) {
	out := make([]string, 0, 4)
	depth := 0
	start := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth < 0 {
				return nil, fmt.Errorf("unbalanced ')' at offset %d", i)
			}
		case sep:
			if depth == 0 {
				out = append(out, strings.TrimSpace(s[start:i]))
				start = i + 1
			}
		}
	}
	if depth != 0 {
		return nil, fmt.Errorf("unbalanced '('")
	}
	return append(out, strings.TrimSpace(s[start:])), nil
}

// validIdent reports whether s is a legal node id, service name, role name or
// quorum scope: an ASCII letter or underscore followed by letters, digits,
// underscores, hyphens or dots.
func validIdent(s string) bool {
	if s == "" {
		return false
	}
	c := s[0]
	if !(c >= 'a' && c <= 'z') && !(c >= 'A' && c <= 'Z') && c != '_' {
		return false
	}
	for i := 1; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '_', c == '-', c == '.':
		default:
			return false
		}
	}
	return true
}

// SortFaultStrings returns a fresh slice of canonical fault strings ordered by
// (start_ms, end_ms, string).
//
// It does NOT mutate the input. Hashing a world must not reorder the caller's
// schedule: a search loop that hashes a candidate mid-mutation would otherwise
// silently reorder the corpus entry the candidate was derived from, and because
// the sort is idempotent the damage would only surface much later.
func SortFaultStrings(ss []string) []string {
	out := make([]string, len(ss))
	copy(out, ss)
	keys := make(map[string][2]int64, len(out))
	for _, s := range out {
		if _, done := keys[s]; done {
			continue
		}
		if f, err := ParseFault(s); err == nil {
			keys[s] = [2]int64{f.StartMS, f.EndMS}
		} else {
			// An unparseable entry sorts last but keeps a stable position
			// relative to its peers; Validate reports it separately.
			keys[s] = [2]int64{1<<62 - 1, 1<<62 - 1}
		}
	}
	sortStable(out, func(a, b string) bool {
		ka, kb := keys[a], keys[b]
		if ka[0] != kb[0] {
			return ka[0] < kb[0]
		}
		if ka[1] != kb[1] {
			return ka[1] < kb[1]
		}
		return a < b
	})
	return out
}
