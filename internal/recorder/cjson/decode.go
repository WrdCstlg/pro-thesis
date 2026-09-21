package cjson

import (
	"bytes"
	"encoding"
	"encoding/json"
	"fmt"
	"reflect"
	"sync"
)

// Unmarshal decodes a canonical document into v, enforcing rules 3, 7 and the
// framing rules, but NOT rule 8. Callers that need the byte-identity guarantee
// want UnmarshalCanonical.
//
// v must be a non-nil pointer.
func Unmarshal(data []byte, v any) error {
	if err := checkFraming(data); err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	// UseNumber keeps any interface{} that slipped past Lint from silently
	// becoming a float64. The encoder rejects interface{} outright, so this is a
	// belt-and-braces measure for callers that decode into a type Lint has not
	// seen.
	dec.UseNumber()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("cjson: decode: %w", err)
	}
	// Anything after the top-level value (beyond the single trailing LF that
	// checkFraming already accounted for) is an error.
	var rest json.RawMessage
	if err := dec.Decode(&rest); err == nil {
		return ErrTrailingData
	}
	return nil
}

// UnmarshalCanonical decodes data into v and then proves that data is exactly
// the canonical encoding of what it decoded.
//
// This is rule 8, and it is what makes byte-identity a runtime invariant rather
// than a property some test happens to check. Every Load path in the tree goes
// through here: a non-canonical `.thesis` file is rejected at the point of use,
// so a corrupted or hand-edited world can never silently participate in a run.
func UnmarshalCanonical(data []byte, v any) error {
	if err := Unmarshal(data, v); err != nil {
		return err
	}
	// Re-encode from the decoded value. reflect.ValueOf(v).Elem() drops the
	// caller's pointer so the encoder sees the value itself.
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Pointer || rv.IsNil() {
		return fmt.Errorf("cjson: UnmarshalCanonical needs a non-nil pointer, got %T", v)
	}
	got, err := Encode(rv.Elem().Interface())
	if err != nil {
		return fmt.Errorf("cjson: re-encode for canonicality check: %w", err)
	}
	if bytes.Equal(got, data) {
		return nil
	}
	off := FirstDiff(data, got)
	return &NonCanonicalError{
		Offset: off,
		Want:   excerpt(got, off, 24),
		Got:    excerpt(data, off, 24),
	}
}

// checkFraming enforces the byte-level rules that must hold before the document
// is even handed to a JSON parser.
func checkFraming(data []byte) error {
	if len(data) == 0 {
		return fmt.Errorf("cjson: empty document")
	}
	if bytes.HasPrefix(data, []byte{0xEF, 0xBB, 0xBF}) {
		return ErrBOM
	}
	// Rule 3. A CR anywhere means git translated the file; this has exactly one
	// cause and one fix on Windows, so it gets its own error rather than
	// surfacing as an opaque syntax failure three layers down.
	if bytes.IndexByte(data, '\r') >= 0 {
		return ErrCRLF
	}
	// Rule 2: exactly one trailing LF, and no other LF anywhere (the encoder
	// emits no insignificant whitespace, so an interior newline is by
	// construction non-canonical).
	if data[len(data)-1] != '\n' {
		return ErrMissingTrailingLF
	}
	body := data[:len(data)-1]
	if bytes.IndexByte(body, '\n') >= 0 {
		return ErrMissingTrailingLF
	}
	return nil
}

// ---------------------------------------------------------------------------
// type-graph linting
// ---------------------------------------------------------------------------

var (
	jsonMarshalerType = reflect.TypeOf((*json.Marshaler)(nil)).Elem()
	textMarshalerType = reflect.TypeOf((*encoding.TextMarshaler)(nil)).Elem()
)

var marshalerCache sync.Map // reflect.Type -> error

// rejectMarshaler fails for any type whose JSON representation this package's
// encoder and encoding/json would disagree about.
//
// Encode is reflective and never consults json.Marshaler or
// encoding.TextMarshaler, but the decoder is encoding/json, which does. A type
// implementing either would therefore encode one way and decode another, and
// UnmarshalCanonical would report a spurious NonCanonicalError, or worse, a
// genuine asymmetry would slip through.
//
// json.Unmarshaler is deliberately ALLOWED and is how the schema enums work:
// they validate strictly on the way in while encoding as their underlying
// string on the way out, which is exactly consistent with this encoder. Any
// laxity or normalization inside such an UnmarshalJSON is caught by rule 8.
func rejectMarshaler(t reflect.Type, path string) error {
	if v, ok := marshalerCache.Load(t); ok {
		if v == nil {
			return nil
		}
		return withPath(v.(error), path)
	}
	err := checkMarshaler(t)
	if err == nil {
		marshalerCache.Store(t, nil)
		return nil
	}
	marshalerCache.Store(t, err)
	return withPath(err, path)
}

func checkMarshaler(t reflect.Type) error {
	pt := reflect.PointerTo(t)
	switch {
	case t.Implements(jsonMarshalerType) || (t.Kind() != reflect.Pointer && pt.Implements(jsonMarshalerType)):
		return fmt.Errorf("type %s implements json.Marshaler; canonical encoding is reflective and would "+
			"disagree with encoding/json about its representation", t)
	case t.Implements(textMarshalerType) || (t.Kind() != reflect.Pointer && pt.Implements(textMarshalerType)):
		return fmt.Errorf("type %s implements encoding.TextMarshaler; canonical encoding is reflective and "+
			"would disagree with encoding/json about its representation", t)
	}
	return nil
}

func withPath(err error, path string) error {
	return fmt.Errorf("cjson: %s: %w", pathOf(path), err)
}

// Lint walks the type graph of v and reports every violation of the canonical
// profile, without needing a value that exercises each branch.
//
// Encode enforces the same rules, but only on the paths a particular value
// happens to reach: a nil pointer to a struct containing a float, or an empty
// slice of maps, would encode cleanly and then fail years later on the one
// world that populated it. Lint is the static check, and every schema type that
// participates in a `.thesis` file has a test asserting Lint passes on it.
func Lint(v any) error {
	return lintType(reflect.TypeOf(v), "", map[reflect.Type]bool{})
}

func lintType(t reflect.Type, path string, seen map[reflect.Type]bool) error {
	if t == nil {
		return fmt.Errorf("cjson: %s: nil type", pathOf(path))
	}
	if seen[t] {
		return nil // recursive type; already being checked up-stack
	}
	seen[t] = true
	defer delete(seen, t)

	if err := rejectMarshaler(t, path); err != nil {
		return err
	}

	switch t.Kind() {
	case reflect.Bool, reflect.String,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return nil

	case reflect.Pointer, reflect.Slice:
		return lintType(t.Elem(), path+"[]", seen)

	case reflect.Struct:
		plan, err := planFor(t)
		if err != nil {
			return err
		}
		for _, f := range plan {
			if err := lintType(t.Field(f.index).Type, path+"."+f.name, seen); err != nil {
				return err
			}
		}
		return nil

	case reflect.Float32, reflect.Float64:
		return fmt.Errorf("cjson: %s: float%d is forbidden; carry ratios as integer parts-per-million",
			pathOf(path), t.Bits())

	case reflect.Map:
		return fmt.Errorf("cjson: %s: map is forbidden; Go randomizes map iteration order", pathOf(path))

	case reflect.Interface:
		return fmt.Errorf("cjson: %s: interface is forbidden; interface{} decoding turns integers into float64",
			pathOf(path))

	default:
		return fmt.Errorf("cjson: %s: unsupported kind %s", pathOf(path), t.Kind())
	}
}
