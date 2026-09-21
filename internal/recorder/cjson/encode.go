package cjson

import (
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"sync"
	"unicode/utf8"
)

// Encode returns the canonical encoding of v, including the single trailing LF.
//
// It is total and deterministic: for a given value it returns the same bytes on
// every call, in every process, on every platform, for every Go version. That
// property is the whole point: see the package comment.
//
// Encode never consults json.Marshaler. Types implementing it are rejected by
// Lint precisely because encoding/json (which this package's decoder uses) would
// then disagree with this encoder about their representation.
func Encode(v any) ([]byte, error) {
	buf := make([]byte, 0, 512)
	buf, err := appendValue(buf, reflect.ValueOf(v), "")
	if err != nil {
		return nil, err
	}
	return append(buf, '\n'), nil
}

// EncodeCompact is Encode without the trailing LF. It is used to build the
// preimage of a hash over a projection, and for one line of a JSONL file.
func EncodeCompact(v any) ([]byte, error) {
	buf := make([]byte, 0, 512)
	return appendValue(buf, reflect.ValueOf(v), "")
}

func appendValue(dst []byte, rv reflect.Value, path string) ([]byte, error) {
	if !rv.IsValid() {
		return nil, fmt.Errorf("cjson: %s: cannot encode an invalid (nil) value", pathOf(path))
	}
	t := rv.Type()
	if err := rejectMarshaler(t, path); err != nil {
		return nil, err
	}
	switch rv.Kind() {
	case reflect.Bool:
		if rv.Bool() {
			return append(dst, "true"...), nil
		}
		return append(dst, "false"...), nil

	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.AppendInt(dst, rv.Int(), 10), nil

	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return strconv.AppendUint(dst, rv.Uint(), 10), nil

	case reflect.String:
		return appendString(dst, rv.String(), path)

	case reflect.Pointer:
		if rv.IsNil() {
			return append(dst, "null"...), nil
		}
		return appendValue(dst, rv.Elem(), path)

	case reflect.Slice:
		if rv.IsNil() {
			// A nil slice encodes as null and decodes back to a nil slice; an
			// empty non-nil slice encodes as [] and decodes back to an empty
			// non-nil slice. Both round-trip exactly, and the distinction is
			// load-bearing: fault_schedule.realized is null for a world that has
			// never been executed.
			return append(dst, "null"...), nil
		}
		dst = append(dst, '[')
		for i := 0; i < rv.Len(); i++ {
			if i > 0 {
				dst = append(dst, ',')
			}
			var err error
			dst, err = appendValue(dst, rv.Index(i), fmt.Sprintf("%s[%d]", path, i))
			if err != nil {
				return nil, err
			}
		}
		return append(dst, ']'), nil

	case reflect.Struct:
		plan, err := planFor(t)
		if err != nil {
			return nil, err
		}
		dst = append(dst, '{')
		first := true
		for _, f := range plan {
			fv := rv.Field(f.index)
			if f.omitEmpty && isEmptyForOmit(fv) {
				continue
			}
			if !first {
				dst = append(dst, ',')
			}
			first = false
			dst, err = appendString(dst, f.name, path)
			if err != nil {
				return nil, err
			}
			dst = append(dst, ':')
			dst, err = appendValue(dst, fv, path+"."+f.name)
			if err != nil {
				return nil, err
			}
		}
		return append(dst, '}'), nil

	case reflect.Float32, reflect.Float64:
		return nil, fmt.Errorf("cjson: %s: float%d is forbidden; carry ratios as integer parts-per-million "+
			"(Go's shortest-float representation is not guaranteed stable across versions)", pathOf(path), t.Bits())

	case reflect.Map:
		return nil, fmt.Errorf("cjson: %s: map is forbidden; Go randomizes map iteration order", pathOf(path))

	case reflect.Interface:
		return nil, fmt.Errorf("cjson: %s: interface is forbidden; interface{} decoding turns integers into float64", pathOf(path))

	default:
		return nil, fmt.Errorf("cjson: %s: unsupported kind %s", pathOf(path), rv.Kind())
	}
}

func pathOf(p string) string {
	if p == "" {
		return "<root>"
	}
	return p
}

// isEmptyForOmit implements omitempty. Lint restricts omitempty to pointer and
// string fields, so only those two cases can occur; the others are defensive.
func isEmptyForOmit(rv reflect.Value) bool {
	switch rv.Kind() {
	case reflect.Pointer:
		return rv.IsNil()
	case reflect.String:
		return rv.Len() == 0
	case reflect.Slice:
		return rv.IsNil() || rv.Len() == 0
	case reflect.Bool:
		return !rv.Bool()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return rv.Int() == 0
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return rv.Uint() == 0
	default:
		return false
	}
}

// appendString writes a canonical JSON string.
//
// Only `"`, `\` and U+0000..U+001F are escaped. Everything else (including `<`,
// `>` and `&`) is emitted as raw UTF-8. Invalid UTF-8 is an error, never
// silently replaced with U+FFFD: substitution is data loss and would break
// injectivity.
func appendString(dst []byte, s string, path string) ([]byte, error) {
	if !utf8.ValidString(s) {
		return nil, fmt.Errorf("cjson: %s: string is not valid UTF-8", pathOf(path))
	}
	dst = append(dst, '"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"':
			dst = append(dst, '\\', '"')
		case c == '\\':
			dst = append(dst, '\\', '\\')
		case c == '\b':
			dst = append(dst, '\\', 'b')
		case c == '\f':
			dst = append(dst, '\\', 'f')
		case c == '\n':
			dst = append(dst, '\\', 'n')
		case c == '\r':
			dst = append(dst, '\\', 'r')
		case c == '\t':
			dst = append(dst, '\\', 't')
		case c < 0x20:
			const hexdigits = "0123456789abcdef"
			dst = append(dst, '\\', 'u', '0', '0', hexdigits[c>>4], hexdigits[c&0xf])
		default:
			dst = append(dst, c)
		}
	}
	return append(dst, '"'), nil
}

// ---------------------------------------------------------------------------
// struct field plans
// ---------------------------------------------------------------------------

type fieldPlan struct {
	name      string
	index     int
	omitEmpty bool
}

var planCache sync.Map // reflect.Type -> planEntry

type planEntry struct {
	plan []fieldPlan
	err  error
}

// planFor returns the sorted encoding plan for a struct type, memoized.
func planFor(t reflect.Type) ([]fieldPlan, error) {
	if v, ok := planCache.Load(t); ok {
		e := v.(planEntry)
		return e.plan, e.err
	}
	plan, err := buildPlan(t)
	planCache.Store(t, planEntry{plan: plan, err: err})
	return plan, err
}

func buildPlan(t reflect.Type) ([]fieldPlan, error) {
	var plan []fieldPlan
	seen := map[string]bool{}
	for i := 0; i < t.NumField(); i++ {
		sf := t.Field(i)
		if sf.Anonymous {
			return nil, fmt.Errorf("cjson: %s: embedded field %s is forbidden; declare it explicitly with a json tag",
				t, sf.Name)
		}
		if sf.PkgPath != "" { // unexported
			continue
		}
		tag, ok := sf.Tag.Lookup("json")
		if !ok {
			return nil, fmt.Errorf("cjson: %s.%s: every exported field needs a json tag", t, sf.Name)
		}
		name, opts := splitTag(tag)
		if name == "-" && opts == "" {
			continue
		}
		if !validKey(name) {
			return nil, fmt.Errorf("cjson: %s.%s: json name %q must match ^[a-z][a-z0-9_]*$", t, sf.Name, name)
		}
		if seen[name] {
			return nil, fmt.Errorf("cjson: %s: duplicate json name %q", t, name)
		}
		seen[name] = true

		f := fieldPlan{name: name, index: i}
		for _, o := range splitOpts(opts) {
			switch o {
			case "omitempty":
				f.omitEmpty = true
			case "":
			default:
				return nil, fmt.Errorf("cjson: %s.%s: unsupported json tag option %q", t, sf.Name, o)
			}
		}
		if f.omitEmpty {
			k := sf.Type.Kind()
			if k != reflect.Pointer && k != reflect.String {
				return nil, fmt.Errorf("cjson: %s.%s: omitempty is only allowed on pointer and string fields "+
					"(absent-versus-zero must stay a bijection)", t, sf.Name)
			}
		}
		plan = append(plan, f)
	}
	sort.Slice(plan, func(i, j int) bool { return plan[i].name < plan[j].name })
	return plan, nil
}

func splitTag(tag string) (name, opts string) {
	for i := 0; i < len(tag); i++ {
		if tag[i] == ',' {
			return tag[:i], tag[i+1:]
		}
	}
	return tag, ""
}

func splitOpts(opts string) []string {
	if opts == "" {
		return nil
	}
	var out []string
	start := 0
	for i := 0; i <= len(opts); i++ {
		if i == len(opts) || opts[i] == ',' {
			out = append(out, opts[start:i])
			start = i + 1
		}
	}
	return out
}

// validKey reports whether k matches ^[a-z][a-z0-9_]*$.
//
// Restricting keys to lowercase ASCII identifiers makes byte order and UTF-16
// code-unit order coincide, so the sort is JCS-compatible without importing
// anything and without a Unicode normalization question.
func validKey(k string) bool {
	if k == "" || k[0] < 'a' || k[0] > 'z' {
		return false
	}
	for i := 1; i < len(k); i++ {
		c := k[i]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_' {
			continue
		}
		return false
	}
	return true
}
