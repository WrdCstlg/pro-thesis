package schema

import (
	"strings"
	"testing"
)

// TestFrozenFaultStrings pins the two fault strings the directive writes out in
// full (4.6 surviving_faults). They must parse, and they must print back
// byte-identically: a canonical printer that reformatted either one would
// change the hash of every world that contains it.
func TestFrozenFaultStrings(t *testing.T) {
	for _, s := range []string{
		"proc.pause(role:leader)@8200..15100",
		"net.partition(minority(kv))@8400..14900",
	} {
		f, err := ParseFault(s)
		if err != nil {
			t.Fatalf("ParseFault(%q): %v", s, err)
		}
		if got := f.String(); got != s {
			t.Errorf("round trip of the directive's own string:\n got %q\nwant %q", got, s)
		}
	}
}

// TestTargetFormsRoundTrip covers every target shape in the grammar.
func TestTargetFormsRoundTrip(t *testing.T) {
	cases := []struct {
		in      string
		want    string // canonical form; "" means identical to in
		kind    TargetKind
		dynamic bool
	}{
		{in: "net.partition(n1)@0..100", kind: TargetNode},
		{in: "net.partition(kv:*)@0..100", kind: TargetWildcard, dynamic: true},
		{in: "net.partition(role:leader)@0..100", kind: TargetRole, dynamic: true},
		{in: "net.partition(n1<->n2)@0..100", kind: TargetEdge},
		{in: "net.partition(n2<->n1)@0..100", want: "net.partition(n1<->n2)@0..100", kind: TargetEdge},
		{in: "net.partition(minority(kv))@0..100", kind: TargetQuorum, dynamic: true},
		{in: "net.partition(majority(kv))@0..100", kind: TargetQuorum, dynamic: true},
		{in: "net.partition(any(2, kv))@0..100", kind: TargetQuorum, dynamic: true},
		{in: "net.partition( any( 2 , kv ) )@0..100", want: "net.partition(any(2, kv))@0..100",
			kind: TargetQuorum, dynamic: true},
		{in: "proc.pause(pg)@-500..0", kind: TargetNode},
	}
	for _, c := range cases {
		want := c.want
		if want == "" {
			want = c.in
		}
		f, err := ParseFault(c.in)
		if err != nil {
			t.Errorf("ParseFault(%q): %v", c.in, err)
			continue
		}
		if got := f.String(); got != want {
			t.Errorf("ParseFault(%q).String() = %q, want %q", c.in, got, want)
			continue
		}
		if f.Target.Kind != c.kind {
			t.Errorf("%q: target kind = %q, want %q", c.in, f.Target.Kind, c.kind)
		}
		if f.Target.IsDynamic() != c.dynamic {
			t.Errorf("%q: IsDynamic = %v, want %v", c.in, f.Target.IsDynamic(), c.dynamic)
		}
		// print -> parse -> print is stable.
		again, err := ParseFault(want)
		if err != nil {
			t.Errorf("re-parsing the canonical form %q: %v", want, err)
			continue
		}
		if got := again.String(); got != want {
			t.Errorf("canonical form is not a fixed point: %q -> %q", want, got)
		}
	}
}

// TestAllSeventeenKindsRoundTrip walks the frozen registry, so adding, removing
// or renaming a kind, a parameter or a default is caught here.
func TestAllSeventeenKindsRoundTrip(t *testing.T) {
	if len(AllFaultKinds) != 17 {
		t.Fatalf("registry has %d kinds, want the 17 v1 kinds", len(AllFaultKinds))
	}
	// Every kind name is family.verb, and every family is represented.
	families := map[FaultFamily]int{}
	for _, k := range AllFaultKinds {
		decl, ok := LookupFaultKind(k)
		if !ok {
			t.Fatalf("%q is listed but not registered", k)
		}
		if !strings.HasPrefix(string(k), string(decl.Family)+".") {
			t.Errorf("%q does not begin with its family %q", k, decl.Family)
		}
		families[decl.Family]++

		// Build a legal fault of this kind with every parameter supplied.
		args := []string{"n1"}
		for _, p := range decl.Params {
			args = append(args, p.Name+"="+sampleParamValue(p))
		}
		src := string(k) + "(" + strings.Join(args, ", ") + ")@100..200"
		f, err := ParseFault(src)
		if err != nil {
			t.Errorf("ParseFault(%q): %v", src, err)
			continue
		}
		if got := f.String(); got != src {
			t.Errorf("%q round-tripped to %q", src, got)
		}
		if len(f.Params) != len(decl.Params) {
			t.Errorf("%s: canonical form carries %d params, declared %d",
				k, len(f.Params), len(decl.Params))
		}
	}
	for _, fam := range []FaultFamily{FamilyNet, FamilyProc, FamilyClock, FamilyIO, FamilyMem, FamilyFD} {
		if families[fam] == 0 {
			t.Errorf("no kind in family %q", fam)
		}
	}
}

func sampleParamValue(p ParamDecl) string {
	switch p.Type {
	case ParamSignal:
		return "SIGTERM"
	case ParamRate:
		return "0.05"
	case ParamPercent:
		return "25"
	case ParamInt:
		return "-250"
	default:
		return "40"
	}
}

// TestDefaultsAreMaterialized: an omitted parameter is written back into the
// canonical string, so an archived world cannot change meaning when a default
// changes in a later release.
func TestDefaultsAreMaterialized(t *testing.T) {
	f, err := ParseFault("proc.kill(n1)@100..200")
	if err != nil {
		t.Fatalf("ParseFault: %v", err)
	}
	const want = "proc.kill(n1, signal=SIGKILL)@100..200"
	if got := f.String(); got != want {
		t.Errorf("got %q, want %q", got, want)
	}

	f, err = ParseFault("net.latency(n1, 40)@0..100")
	if err != nil {
		t.Fatalf("ParseFault: %v", err)
	}
	if got, want := f.String(), "net.latency(n1, mean=40, jitter=0)@0..100"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestParamBindingFormsAgree: positional and named binding must produce the
// same canonical string, or the same fault would hash two ways.
func TestParamBindingFormsAgree(t *testing.T) {
	a, err := ParseFault("net.latency(n1, 40, 5)@0..100")
	if err != nil {
		t.Fatalf("positional: %v", err)
	}
	b, err := ParseFault("net.latency(n1, jitter=5, mean=40)@0..100")
	if err != nil {
		t.Fatalf("named out of order: %v", err)
	}
	if a.String() != b.String() {
		t.Errorf("positional %q != named %q", a.String(), b.String())
	}
}

// TestParamValueTextIsPreservedVerbatim: a parameter's source text must never
// be re-derived from a parsed number, or 0.05 could re-render differently and
// change a world hash.
func TestParamValueTextIsPreservedVerbatim(t *testing.T) {
	for _, v := range []string{"0.05", "0.050", "0.5", "1"} {
		src := "io.error(n1, rate=" + v + ")@0..10"
		f, err := ParseFault(src)
		if err != nil {
			t.Fatalf("ParseFault(%q): %v", src, err)
		}
		got, _ := f.Param("rate")
		if got != v {
			t.Errorf("rate stored as %q, want the source text %q", got, v)
		}
		if f.String() != src {
			t.Errorf("%q re-printed as %q", src, f.String())
		}
	}
}

func TestParseFaultErrors(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"proc.pause(n1)", "missing @"},
		{"proc.pause(n1)@100", "window"},
		{"proc.pause(n1)@200..100", "ends before"},
		{"proc.nope(n1)@0..1", "unknown kind"},
		{"proc.pause()@0..1", "needs a target"},
		{"proc.pause(n1@0..1", "KIND(TARGET"},
		{"proc.pause(n1<->n2)@0..1", "edge target"},
		{"net.partition(quorum(kv))@0..1", "unknown quorum function"},
		{"net.partition(any(kv))@0..1", "count and a scope"},
		{"net.partition(any(0, kv))@0..1", "count must be"},
		{"net.partition(n1<->n1)@0..1", "connects a node to itself"},
		{"net.loss(n1)@0..1", "required"},
		{"net.loss(n1, pct=140)@0..1", "outside [0, 100]"},
		{"io.error(n1, rate=2)@0..1", "outside [0, 1]"},
		{"proc.pause(n1, foo=1)@0..1", "no parameters"},
		{"net.latency(n1, jitter=5, 40)@0..1", "follows a named one"},
		{"net.latency(n1, mean=1, mean=2)@0..1", "given twice"},
		{"proc.pause(role:)@0..1", "role name"},
		{"proc.pause(1bad)@0..1", "not a node id"},
	}
	for _, c := range cases {
		_, err := ParseFault(c.in)
		if err == nil {
			t.Errorf("ParseFault(%q) succeeded, want an error mentioning %q", c.in, c.want)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("ParseFault(%q) error = %v, want it to mention %q", c.in, err, c.want)
		}
	}
}

// TestEdgeOnlyForNetworkKinds: an edge target names a link, and only the
// network family acts on a link.
func TestEdgeOnlyForNetworkKinds(t *testing.T) {
	for _, k := range AllFaultKinds {
		decl, _ := LookupFaultKind(k)
		args := []string{"n1<->n2"}
		for _, p := range decl.Params {
			args = append(args, p.Name+"="+sampleParamValue(p))
		}
		src := string(k) + "(" + strings.Join(args, ", ") + ")@0..1"
		_, err := ParseFault(src)
		if decl.Family == FamilyNet && err != nil {
			t.Errorf("%q rejected an edge target: %v", k, err)
		}
		if decl.Family != FamilyNet && err == nil {
			t.Errorf("%q accepted an edge target; only the network family acts on a link", k)
		}
	}
}

// TestDurativeSplit pins which kinds fire at their window start rather than
// being active over it.
func TestDurativeSplit(t *testing.T) {
	instantaneous := map[FaultKind]bool{
		FaultProcKill: true, FaultProcRestart: true, FaultClockJump: true,
	}
	for _, k := range AllFaultKinds {
		if got, want := k.Durative(), !instantaneous[k]; got != want {
			t.Errorf("%q Durative = %v, want %v", k, got, want)
		}
	}
}

// TestSortFaultStringsDoesNotMutate: hashing a candidate world must never
// reorder the schedule it was derived from. The sort is idempotent, so a
// mutating implementation would only surface as a heisenbug much later.
func TestSortFaultStringsDoesNotMutate(t *testing.T) {
	in := []string{
		"net.partition(minority(kv))@8400..14900",
		"proc.pause(role:leader)@8200..15100",
	}
	before := append([]string(nil), in...)
	out := SortFaultStrings(in)

	for i := range in {
		if in[i] != before[i] {
			t.Fatalf("SortFaultStrings mutated its input at %d: %q -> %q", i, before[i], in[i])
		}
	}
	if out[0] != "proc.pause(role:leader)@8200..15100" {
		t.Errorf("sorted order = %q, want the 8200 window first", out)
	}
}

func TestCanonicalFault(t *testing.T) {
	got, err := CanonicalFault("net.partition( n2<->n1 )@10..20")
	if err != nil {
		t.Fatalf("CanonicalFault: %v", err)
	}
	if want := "net.partition(n1<->n2)@10..20"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
