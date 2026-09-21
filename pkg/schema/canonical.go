package schema

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
)

// marshalNoEscape marshals v as JSON with HTML escaping disabled and without
// the trailing newline json.Encoder appends.
//
// Every emitter in this package routes through it. Go's default encoder
// escapes '<', '>' and '&', which would rewrite the edge target n1<->n2 as
// n1<->n2: a different byte string, a different world hash, and an
// unreadable `surviving_faults` entry in a verdict.
func marshalNoEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// Canonicalize rewrites valid JSON into PRO-THESIS canonical form:
//
//   - object members sorted by key in byte order (every schema key is ASCII)
//   - no insignificant whitespace
//   - scalar tokens (numbers, strings, true/false/null) copied VERBATIM
//
// Copying scalars verbatim is the point: no number is ever re-derived, so a
// uint64 above 2^53 keeps every digit and no float is reformatted. This is
// RFC 8785 restricted to ASCII keys and to input this package produced.
func Canonicalize(raw []byte) ([]byte, error) {
	var out bytes.Buffer
	if err := canon(&out, raw); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func canon(dst *bytes.Buffer, raw json.RawMessage) error {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 {
		return errors.New("schema: empty JSON value")
	}
	switch t[0] {
	case '{':
		var m map[string]json.RawMessage
		if err := json.Unmarshal(t, &m); err != nil {
			return err
		}
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		dst.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				dst.WriteByte(',')
			}
			kb, err := marshalNoEscape(k)
			if err != nil {
				return err
			}
			dst.Write(kb)
			dst.WriteByte(':')
			if err := canon(dst, m[k]); err != nil {
				return err
			}
		}
		dst.WriteByte('}')
	case '[':
		var a []json.RawMessage
		if err := json.Unmarshal(t, &a); err != nil {
			return err
		}
		dst.WriteByte('[')
		for i := range a {
			if i > 0 {
				dst.WriteByte(',')
			}
			if err := canon(dst, a[i]); err != nil {
				return err
			}
		}
		dst.WriteByte(']')
	default:
		if !json.Valid(t) {
			return fmt.Errorf("schema: invalid JSON scalar %q", string(t))
		}
		dst.Write(t)
	}
	return nil
}

// CanonicalMarshal marshals v with HTML escaping off, then canonicalizes.
func CanonicalMarshal(v any) ([]byte, error) {
	b, err := marshalNoEscape(v)
	if err != nil {
		return nil, err
	}
	return Canonicalize(b)
}

// HashPrefix is the algorithm prefix on every content address this package
// produces, matching the directive's `"manifest_sha": "sha256:..."`.
const HashPrefix = "sha256:"

// HashCanonical returns "sha256:"+hex over the canonical encoding of v. This is
// the content-address function for worlds.
func HashCanonical(v any) (string, error) {
	c, err := CanonicalMarshal(v)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(c)
	return HashPrefix + hex.EncodeToString(sum[:]), nil
}

// HashBytes returns "sha256:"+hex over b exactly as given.
func HashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return HashPrefix + hex.EncodeToString(sum[:])
}

// indentJSON re-indents compact JSON with two spaces and a single trailing
// newline. Used for artifacts humans read and diff (the verdict); the .thesis
// world file is emitted compact and canonical instead, because its byte
// identity is a hashed contract.
func indentJSON(b []byte) ([]byte, error) {
	var out bytes.Buffer
	if err := json.Indent(&out, b, "", "  "); err != nil {
		return nil, err
	}
	out.WriteByte('\n')
	return out.Bytes(), nil
}
