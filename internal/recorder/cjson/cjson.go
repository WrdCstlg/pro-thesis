// Package cjson implements the canonical JSON profile that PRO-THESIS `.thesis`
// world files are written in.
//
// The profile is fixed by PHASE0_BUILD_BRIEF.md §2 and every rule below is
// mandatory, because Phase 0 definition of done (b) ("a world configuration
// round-trips through `.thesis` serialization byte-identically") is exactly the
// statement that this encoder is a bijection on the language its decoder accepts.
//
//  1. Object keys are sorted ascending by byte order. Struct fields are sorted
//     too, so reordering a Go struct declaration is a no-op rather than a silent
//     rehash of every committed world.
//  2. No insignificant whitespace anywhere, and exactly one trailing LF.
//  3. LF only. A CR byte anywhere is a specific, named error (git has translated
//     the file); see ErrCRLF.
//  4. HTML escaping is off. `<`, `>` and `&` are emitted literally. Go's
//     encoding/json escapes them by default, which would corrupt the edge target
//     `n1<->n2` and change every world hash.
//  5. No bare floats. float32/float64 are rejected by Lint and by the encoder;
//     ratios are carried as integer parts-per-million.
//  6. No map and no interface{} in the type graph. Map iteration order is
//     randomized and interface{} decoding turns integers into float64.
//  7. The decoder is strict: unknown fields are rejected (DisallowUnknownFields),
//     as is trailing data.
//  8. UnmarshalCanonical re-encodes and byte-compares, so byte-identity is a
//     runtime invariant rather than only a test assertion.
//
// Nothing in this package is specific to worlds. The world TYPE lives in
// pkg/schema, which owns it; this package owns the canonical FORM. Both
// pkg/schema (MarshalWorld, World.Hash) and internal/recorder (EncodeWorld,
// DecodeWorld, the artifact bundle) encode through here, so the tree has exactly
// one definition of canonical bytes for anything whose bytes are hashed.
//
// pkg/schema importing an internal/ package is legal within this module and
// deliberate: one encoder is a correctness requirement, two is a latent
// split-brain. Relocating this package to internal/cjson would make the layering
// read better and is a mechanical, behaviour-free change; it is recorded as an
// open question rather than done mid-integration.
package cjson

import (
	"errors"
	"fmt"
)

// ErrCRLF is returned when input contains a CR byte. This is called out
// separately from a generic syntax error because on Windows it has exactly one
// cause and exactly one fix.
var ErrCRLF = errors.New("cjson: CR byte in input: git has translated line endings. " +
	"Check .gitattributes (`*.thesis text eol=lf`) and `git config core.autocrlf`")

// ErrBOM is returned when input starts with a UTF-8 byte-order mark.
var ErrBOM = errors.New("cjson: input starts with a UTF-8 BOM; canonical documents have none")

// ErrTrailingData is returned when input carries anything after the top-level value
// other than the single trailing LF.
var ErrTrailingData = errors.New("cjson: trailing data after the top-level value")

// ErrMissingTrailingLF is returned when a canonical document does not end with
// exactly one LF.
var ErrMissingTrailingLF = errors.New("cjson: canonical documents end with exactly one LF")

// NonCanonicalError reports that a document parsed, but its bytes are not the
// canonical encoding of the value it denotes.
//
// This is the error that makes rule 8 real. It fires for unsorted keys, inserted
// whitespace, duplicate keys, HTML-escaped characters, `-0`, `1.0` where an
// integer was meant, non-minimal string escapes, a missing field, and anything
// else that decodes but does not re-encode to the same bytes.
type NonCanonicalError struct {
	// Offset is the byte offset of the first difference between the input and
	// its canonical re-encoding, or -1 when the inputs differ only in length.
	Offset int
	// Want is a short excerpt of the canonical encoding at Offset.
	Want string
	// Got is a short excerpt of the input at Offset.
	Got string
}

func (e *NonCanonicalError) Error() string {
	return fmt.Sprintf("cjson: input is not canonical at byte %d: canonical form has %q, input has %q "+
		"(the file must be byte-identical to its canonical encoding)", e.Offset, e.Want, e.Got)
}

// FirstDiff returns the byte offset of the first difference between a and b, or
// -1 if one is a prefix of the other and they differ only in length, or len(a)
// if they are equal.
func FirstDiff(a, b []byte) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	if len(a) == len(b) {
		return len(a)
	}
	return n
}

func excerpt(b []byte, off, n int) string {
	if off < 0 || off >= len(b) {
		return ""
	}
	end := off + n
	if end > len(b) {
		end = len(b)
	}
	return string(b[off:end])
}
