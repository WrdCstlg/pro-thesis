package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
)

// ---------------------------------------------------------------------------
// Register values
//
// A history value arrives as raw JSON. Comparing raw bytes would be a
// FALSE-POSITIVE GENERATOR: a driver that writes 7 and a server that echoes 7.0
// mean the same value, and a byte comparison would call the read unlinearizable
// and report a violation of a system that is behaving correctly. So every value
// is reduced to a canonical text form once, at load time, and compared as text
// thereafter.
// ---------------------------------------------------------------------------

// Value is a register value in canonical textual form.
//
// The zero Value is UNKNOWN, which is the register's state before anything in
// the checked sub-history has determined it. Unknown is not "empty" and not
// "null": see the note on alternatives() in search.go for why the distinction
// closes a false-positive hole.
//
// Value is comparable, which is what lets the memo table key on it directly.
type Value struct {
	known bool
	canon string
}

// unknownValue is the register's initial state.
var unknownValue = Value{}

// knownValue wraps an already-canonical text.
func knownValue(canon string) Value { return Value{known: true, canon: canon} }

// Known reports whether the value has been determined.
func (v Value) Known() bool { return v.known }

// String renders the value for an explanation.
func (v Value) String() string {
	if !v.known {
		return "<undetermined>"
	}
	return v.canon
}

// Equal reports value equality. Two unknowns are equal only to each other.
func (v Value) Equal(o Value) bool { return v == o }

// canonicalValue reduces raw JSON to a canonical Value.
//
// Numbers are normalised so that 7, 7.0 and 7e0 collapse to the same text.
// Objects have their members sorted. Arrays keep their order, because array
// order is semantically significant.
func canonicalValue(raw json.RawMessage) (Value, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return unknownValue, fmt.Errorf("empty JSON value")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return unknownValue, err
	}
	if dec.More() {
		return unknownValue, fmt.Errorf("trailing data after the JSON value")
	}
	var b bytes.Buffer
	if err := writeCanonical(&b, v); err != nil {
		return unknownValue, err
	}
	return knownValue(b.String()), nil
}

func writeCanonical(b *bytes.Buffer, v any) error {
	switch t := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		if t {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case json.Number:
		b.WriteString(canonicalNumber(t))
	case string:
		q, err := json.Marshal(t)
		if err != nil {
			return err
		}
		b.Write(q)
	case []any:
		b.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				b.WriteByte(',')
			}
			if err := writeCanonical(b, e); err != nil {
				return err
			}
		}
		b.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			q, err := json.Marshal(k)
			if err != nil {
				return err
			}
			b.Write(q)
			b.WriteByte(':')
			if err := writeCanonical(b, t[k]); err != nil {
				return err
			}
		}
		b.WriteByte('}')
	default:
		return fmt.Errorf("unsupported JSON value of type %T", v)
	}
	return nil
}

// canonicalNumber normalises a JSON number's text.
//
// Integral values render as decimal integers so that "7" and "7.0" compare
// equal. The residual imprecision (two distinct integers above 2^53 that
// collapse to the same float) makes two DIFFERENT values compare EQUAL, which
// loses a violation rather than manufacturing one. That is the safe direction,
// and it is stated here rather than hidden.
func canonicalNumber(n json.Number) string {
	if i, err := n.Int64(); err == nil {
		return strconv.FormatInt(i, 10)
	}
	if f, err := n.Float64(); err == nil {
		if f == math.Trunc(f) && math.Abs(f) < float64(int64(1)<<53) {
			return strconv.FormatInt(int64(f), 10)
		}
		return strconv.FormatFloat(f, 'g', -1, 64)
	}
	return n.String()
}
