package schema

import (
	"fmt"
	"strings"
	"time"
)

// Identifier shapes, transcribed from the directive's worked examples:
//
//	run id          r_2026_09_03_a41f   (4.6)
//	world filename  w_a41f.thesis       (4.6, minimal_repro.world)
const (
	// RunIDPrefix begins every run id.
	RunIDPrefix = "r_"
	// WorldIDPrefix begins every world filename.
	WorldIDPrefix = "w_"
	// WorldFileExt is the world file extension. It is covered by
	// .gitattributes as `text eol=lf`.
	WorldFileExt = ".thesis"
	// ShortIDLen is the number of hex digits in a short id.
	ShortIDLen = 4
	// RunIDShape and WorldFileShape are used in error messages.
	RunIDShape     = "r_YYYY_MM_DD_xxxx"
	WorldFileShape = "w_xxxx.thesis"

	runIDDateLayout = "2006_01_02"
)

// ValidShortID reports whether s is exactly ShortIDLen lowercase hex digits.
func ValidShortID(s string) bool {
	if len(s) != ShortIDLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') {
			continue
		}
		return false
	}
	return true
}

// ShortIDFromHash reduces a content address to its short id: the first
// ShortIDLen hex digits after the algorithm prefix.
//
// The short id is a display and filename convenience, never an identity. Two
// worlds sharing a short id are distinguished by their full hash, which is what
// the corpus compares.
func ShortIDFromHash(hash string) (string, error) {
	h := strings.TrimPrefix(hash, HashPrefix)
	if len(h) < ShortIDLen {
		return "", fmt.Errorf("short id: %q is too short to reduce (want at least %d hex digits after %q)",
			hash, ShortIDLen, HashPrefix)
	}
	s := strings.ToLower(h[:ShortIDLen])
	if !ValidShortID(s) {
		return "", fmt.Errorf("short id: %q does not begin with %d hex digits", hash, ShortIDLen)
	}
	return s, nil
}

// FormatRunID builds a run id from a timestamp and a short id.
//
// The date is taken in UTC so two machines in different zones never disagree
// about which day a run belongs to.
func FormatRunID(t time.Time, shortID string) (string, error) {
	if !ValidShortID(shortID) {
		return "", fmt.Errorf("run id: %q is not a short id (want %d lowercase hex digits)",
			shortID, ShortIDLen)
	}
	return RunIDPrefix + t.UTC().Format(runIDDateLayout) + "_" + shortID, nil
}

// RunIDFromHash builds a run id whose short id is derived from a content
// address.
func RunIDFromHash(t time.Time, hash string) (string, error) {
	s, err := ShortIDFromHash(hash)
	if err != nil {
		return "", err
	}
	return FormatRunID(t, s)
}

// ParseRunID splits a run id into its date (UTC midnight) and short id.
func ParseRunID(s string) (time.Time, string, error) {
	bad := func() (time.Time, string, error) {
		return time.Time{}, "", fmt.Errorf("run id: %q is not a run id (want %s)", s, RunIDShape)
	}
	rest, ok := strings.CutPrefix(s, RunIDPrefix)
	if !ok {
		return bad()
	}
	i := strings.LastIndexByte(rest, '_')
	if i < 0 {
		return bad()
	}
	dateTxt, shortID := rest[:i], rest[i+1:]
	if !ValidShortID(shortID) {
		return bad()
	}
	d, err := time.ParseInLocation(runIDDateLayout, dateTxt, time.UTC)
	if err != nil {
		return bad()
	}
	return d, shortID, nil
}

// ValidRunID reports whether s parses as a run id.
func ValidRunID(s string) bool {
	_, _, err := ParseRunID(s)
	return err == nil
}

// RunIDShortID returns just the short id of a run id.
func RunIDShortID(s string) (string, error) {
	_, id, err := ParseRunID(s)
	return id, err
}

// WorldFilename returns the file name for a world with the given short id.
func WorldFilename(shortID string) (string, error) {
	if !ValidShortID(shortID) {
		return "", fmt.Errorf("world filename: %q is not a short id (want %d lowercase hex digits)",
			shortID, ShortIDLen)
	}
	return WorldIDPrefix + shortID + WorldFileExt, nil
}

// WorldFilenameFor returns the file name a world should be stored under: its
// own content address, reduced to a short id.
func WorldFilenameFor(w *World) (string, error) {
	s, err := w.ShortID()
	if err != nil {
		return "", err
	}
	return WorldFilename(s)
}

// ParseWorldFilename extracts the short id from a world file name. It accepts
// a bare name, not a path: path handling is a caller's policy.
func ParseWorldFilename(name string) (string, error) {
	bad := func() (string, error) {
		return "", fmt.Errorf("world filename: %q is not a world file name (want %s)", name, WorldFileShape)
	}
	rest, ok := strings.CutPrefix(name, WorldIDPrefix)
	if !ok {
		return bad()
	}
	rest, ok = strings.CutSuffix(rest, WorldFileExt)
	if !ok {
		return bad()
	}
	if !ValidShortID(rest) {
		return bad()
	}
	return rest, nil
}
