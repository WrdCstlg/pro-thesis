package cjson

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// cjson is the single definition of canonical form for every byte string this
// system hashes. It was previously exercised only indirectly through pkg/schema
// and internal/recorder, which means a bug in a rejection path (a branch no
// realistic world happens to reach) would surface as a confusing failure two
// packages away, or not at all.
//
// Every test below asserts a REJECTION whose absence would silently corrupt a
// hashed artifact. They are the guards, not the happy path.

type okStruct struct {
	Alpha string `json:"alpha"`
	Zulu  int64  `json:"zulu"`
}

func TestEncodeSortsKeysRegardlessOfDeclarationOrder(t *testing.T) {
	// Declaration order is zulu-then-alpha via a second type; both must encode
	// identically, so reordering a Go struct is a no-op rather than a silent
	// rehash of every committed world.
	type reordered struct {
		Zulu  int64  `json:"zulu"`
		Alpha string `json:"alpha"`
	}
	a, err := EncodeCompact(okStruct{Alpha: "x", Zulu: 7})
	if err != nil {
		t.Fatal(err)
	}
	b, err := EncodeCompact(reordered{Zulu: 7, Alpha: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if string(a) != string(b) {
		t.Fatalf("field declaration order changed the encoding:\n  %s\n  %s", a, b)
	}
	if want := `{"alpha":"x","zulu":7}`; string(a) != want {
		t.Fatalf("got %s, want %s", a, want)
	}
}

func TestEncodeDoesNotHTMLEscape(t *testing.T) {
	// The whole reason this package exists rather than encoding/json: Go
	// escapes < and > by default, which would turn the normative edge target
	// n1<->n2 into a different byte string and change its world hash.
	got, err := EncodeCompact(okStruct{Alpha: "n1<->n2 & more"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "n1<->n2 & more") {
		t.Fatalf("edge target was escaped: %s", got)
	}

	std, _ := json.Marshal(okStruct{Alpha: "n1<->n2 & more"})
	if strings.Contains(string(std), "n1<->n2") {
		t.Skip("encoding/json no longer HTML-escapes; this guard is obsolete")
	}
}

func TestEncodeTrailingNewline(t *testing.T) {
	full, err := Encode(okStruct{Alpha: "a"})
	if err != nil {
		t.Fatal(err)
	}
	compact, err := EncodeCompact(okStruct{Alpha: "a"})
	if err != nil {
		t.Fatal(err)
	}
	if string(full) != string(compact)+"\n" {
		t.Fatalf("Encode must be EncodeCompact plus exactly one LF: %q vs %q", full, compact)
	}
}

// --- rejection paths -------------------------------------------------------

func TestRejectsFloat(t *testing.T) {
	type withFloat struct {
		Ratio float64 `json:"ratio"`
	}
	assertRejected(t, withFloat{Ratio: 0.5}, "float",
		"a bare float would break byte-identity on any toolchain whose shortest-float representation changes")
}

func TestRejectsMap(t *testing.T) {
	type withMap struct {
		M map[string]string `json:"m"`
	}
	assertRejected(t, withMap{M: map[string]string{"a": "b"}}, "map",
		"Go randomizes map iteration order, so a map cannot have a canonical encoding")
}

func TestRejectsInterface(t *testing.T) {
	type withAny struct {
		V any `json:"v"`
	}
	assertRejected(t, withAny{V: int64(1)}, "interface",
		"interface{} decoding turns integers into float64 and loses precision above 2^53")
}

func TestRejectsEmbeddedField(t *testing.T) {
	type Inner struct {
		A string `json:"a"`
	}
	type outer struct {
		Inner
		B string `json:"b"`
	}
	assertRejected(t, outer{}, "embedded", "embedding hides the key order the encoder must fix")
}

func TestRejectsUntaggedExportedField(t *testing.T) {
	type untagged struct {
		A string `json:"a"`
		B string
	}
	assertRejected(t, untagged{}, "json tag", "an untagged field's key would come from the Go name")
}

func TestRejectsBadKeyName(t *testing.T) {
	type badKey struct {
		A string `json:"Alpha"`
	}
	assertRejected(t, badKey{}, "must match",
		"restricting keys to lowercase ASCII makes byte order and UTF-16 order coincide")
}

// The duplicate-tag type is built with reflect.StructOf rather than declared,
// because `go vet` rejects a literal duplicate json tag at compile time, which
// is the very thing under test here.
func TestRejectsDuplicateKeys(t *testing.T) {
	dup := reflect.StructOf([]reflect.StructField{
		{Name: "A", Type: reflect.TypeOf(""), Tag: `json:"a"`},
		{Name: "B", Type: reflect.TypeOf(""), Tag: `json:"a"`},
	})
	v := reflect.New(dup).Elem().Interface()
	assertRejected(t, v, "duplicate", "two fields cannot share one key")
}

func TestRejectsOmitEmptyOnNonPointerNonString(t *testing.T) {
	type badOmit struct {
		N int64 `json:"n,omitempty"`
	}
	assertRejected(t, badOmit{}, "omitempty",
		"omitempty on an integer makes absent and zero indistinguishable, breaking the bijection")
}

// TestRejectsInvalidUTF8 checks a VALUE-level rule, so only the encoder can
// enforce it. Lint walks the type graph and never sees a string's contents:
// asserting Lint catches this would be asserting something impossible.
func TestRejectsInvalidUTF8(t *testing.T) {
	bad := okStruct{Alpha: string([]byte{0xff, 0xfe})}
	if _, err := EncodeCompact(bad); err == nil {
		t.Fatal("encoded invalid UTF-8; replacing bad bytes with U+FFFD would be silent data loss " +
			"and would break injectivity")
	} else if !strings.Contains(err.Error(), "UTF-8") {
		t.Fatalf("error %q does not mention UTF-8", err)
	}
	if err := Lint(bad); err != nil {
		t.Fatalf("Lint rejected a well-typed value (%v); it is a static type check and must not "+
			"depend on runtime contents", err)
	}
}

func TestRejectsJSONMarshaler(t *testing.T) {
	type withMarshaler struct {
		M marshalerType `json:"m"`
	}
	assertRejected(t, withMarshaler{}, "json.Marshaler",
		"the reflective encoder ignores MarshalJSON, so encoder and decoder would disagree")
}

type marshalerType struct{}

func (marshalerType) MarshalJSON() ([]byte, error) { return []byte(`"custom"`), nil }

func assertRejected(t *testing.T, v any, wantSubstr, why string) {
	t.Helper()
	_, encErr := EncodeCompact(v)
	lintErr := Lint(v)
	if encErr == nil && lintErr == nil {
		t.Fatalf("expected rejection containing %q, got none.\nwhy this matters: %s", wantSubstr, why)
	}
	// Lint is the static check and must catch everything the encoder does; the
	// encoder only sees paths a particular value reaches.
	if lintErr == nil {
		t.Errorf("Lint accepted a value the encoder rejected (%v); Lint is the static guard and must not be weaker", encErr)
	}
	got := ""
	if lintErr != nil {
		got = lintErr.Error()
	} else {
		got = encErr.Error()
	}
	if !strings.Contains(got, wantSubstr) {
		t.Fatalf("error %q does not mention %q\nwhy this matters: %s", got, wantSubstr, why)
	}
}

// --- decode framing --------------------------------------------------------

func TestDecodeRejectsFraming(t *testing.T) {
	valid, err := Encode(okStruct{Alpha: "a", Zulu: 1})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name  string
		input []byte
		want  error
	}{
		{"CRLF", []byte(strings.Replace(string(valid), "\n", "\r\n", 1)), ErrCRLF},
		{"BOM", append([]byte{0xEF, 0xBB, 0xBF}, valid...), ErrBOM},
		{"no trailing LF", valid[:len(valid)-1], ErrMissingTrailingLF},
		{"two trailing LF", append(append([]byte{}, valid...), '\n'), ErrMissingTrailingLF},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out okStruct
			err := Unmarshal(tc.input, &out)
			if err == nil {
				t.Fatalf("accepted %s; a canonical document must be rejected for it", tc.name)
			}
			if tc.want != nil && !strings.Contains(err.Error(), tc.want.Error()[:20]) {
				t.Fatalf("got %v, want something like %v", err, tc.want)
			}
		})
	}
}

// jsonEsc renders r as a JSON \uXXXX escape, built from a raw backslash byte so
// no literal escape sequence appears in this source file.
func jsonEsc(r rune) string {
	return string(rune(92)) + fmt.Sprintf("u%04x", r)
}

func TestUnmarshalCanonicalRejectsNonCanonical(t *testing.T) {
	cases := map[string]string{
		"unsorted keys":  `{"zulu":1,"alpha":"a"}` + "\n",
		"inserted space": `{"alpha": "a","zulu":1}` + "\n",
		"duplicate key":  `{"alpha":"a","alpha":"b","zulu":1}` + "\n",
		// The ESCAPED form, built with an explicit backslash. A raw '<' is
		// canonical here (that is the whole point of disabling HTML escaping)
		// so asserting on a raw '<' would test the opposite of the intended
		// rule. What must be rejected is encoding/json's default output, which
		// escapes it.
		"HTML-escaped char": `{"alpha":"` + jsonEsc('<') + `","zulu":1}` + "\n",
		// A non-minimally-escaped 'a' decodes fine and re-encodes as a bare 'a',
		// so the bytes differ from their canonical form.
		"non-minimal escape": `{"alpha":"` + jsonEsc('a') + `","zulu":1}` + "\n",
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			var out okStruct
			if err := UnmarshalCanonical([]byte(in), &out); err == nil {
				t.Fatalf("accepted non-canonical input (%s); byte-identity is a runtime invariant, "+
					"so this must be rejected at the point of use", name)
			}
		})
	}
}

func TestUnmarshalCanonicalAcceptsItsOwnOutput(t *testing.T) {
	want := okStruct{Alpha: "n1<->n2", Zulu: -3}
	b, err := Encode(want)
	if err != nil {
		t.Fatal(err)
	}
	var got okStruct
	if err := UnmarshalCanonical(b, &got); err != nil {
		t.Fatalf("the encoder produced output its own decoder rejects: %v", err)
	}
	if got != want {
		t.Fatalf("round trip changed the value: %+v -> %+v", want, got)
	}
}

func TestUnknownFieldRejected(t *testing.T) {
	var out okStruct
	if err := Unmarshal([]byte(`{"alpha":"a","extra":1,"zulu":1}`+"\n"), &out); err == nil {
		t.Fatal("accepted an unknown field; the decoder must be strict or a typo silently vanishes")
	}
}

func TestNilSliceAndEmptySliceDiffer(t *testing.T) {
	// Load-bearing: fault_schedule.realized is null for a world that has never
	// been executed and [] for one that executed with no faults. Collapsing
	// them would erase the distinction between "not run" and "ran cleanly".
	type withSlice struct {
		S []string `json:"s"`
	}
	nilEnc, err := EncodeCompact(withSlice{S: nil})
	if err != nil {
		t.Fatal(err)
	}
	emptyEnc, err := EncodeCompact(withSlice{S: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	if string(nilEnc) == string(emptyEnc) {
		t.Fatalf("nil and empty slice encode identically (%s); null vs [] must stay distinguishable", nilEnc)
	}
	if string(nilEnc) != `{"s":null}` {
		t.Fatalf("nil slice encoded as %s, want {\"s\":null}", nilEnc)
	}
	if string(emptyEnc) != `{"s":[]}` {
		t.Fatalf("empty slice encoded as %s, want {\"s\":[]}", emptyEnc)
	}
}
