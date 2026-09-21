package schema

import (
	"testing"
	"time"
)

// TestRunIDMatchesTheDirectiveExample pins the shape the directive writes out:
// "run_id": "r_2026_09_03_a41f".
func TestRunIDMatchesTheDirectiveExample(t *testing.T) {
	const want = "r_2026_09_03_a41f"
	got, err := FormatRunID(time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC), "a41f")
	if err != nil {
		t.Fatalf("FormatRunID: %v", err)
	}
	if got != want {
		t.Errorf("FormatRunID = %q, want %q", got, want)
	}
	if !ValidRunID(want) {
		t.Errorf("ValidRunID(%q) = false", want)
	}

	d, short, err := ParseRunID(want)
	if err != nil {
		t.Fatalf("ParseRunID: %v", err)
	}
	if short != "a41f" {
		t.Errorf("short id = %q", short)
	}
	if d.Year() != 2026 || d.Month() != time.September || d.Day() != 3 {
		t.Errorf("date = %s", d)
	}

	// The date is taken in UTC so two machines never disagree about which day a
	// run belongs to.
	late := time.Date(2026, 9, 3, 23, 30, 0, 0, time.FixedZone("ahead", 3*3600))
	got, err = FormatRunID(late, "a41f")
	if err != nil {
		t.Fatalf("FormatRunID: %v", err)
	}
	if got != "r_2026_09_03_a41f" {
		t.Errorf("UTC normalization: got %q", got)
	}
}

func TestRunIDRejectsMalformed(t *testing.T) {
	for _, bad := range []string{
		"", "r_2026_09_03", "2026_09_03_a41f", "r_2026_9_3_a41f",
		"r_2026_09_03_A41F", "r_2026_09_03_a41", "r_2026_09_03_a41fg",
		"r_2026_13_03_a41f", "r_2026_09_03_zzzz",
	} {
		if ValidRunID(bad) {
			t.Errorf("ValidRunID(%q) = true", bad)
		}
	}
	if _, err := FormatRunID(time.Now(), "A41F"); err == nil {
		t.Error("FormatRunID accepted an uppercase short id")
	}
}

func TestShortIDFromHash(t *testing.T) {
	const h = "sha256:a41fb0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b8"
	got, err := ShortIDFromHash(h)
	if err != nil {
		t.Fatalf("ShortIDFromHash: %v", err)
	}
	if got != "a41f" {
		t.Errorf("got %q, want a41f", got)
	}
	// The prefix is optional; a bare hex digest works too.
	got, err = ShortIDFromHash("a41fb0c4")
	if err != nil || got != "a41f" {
		t.Errorf("bare digest: got %q, err %v", got, err)
	}
	for _, bad := range []string{"", "sha256:", "sha256:ab", "sha256:zzzz"} {
		if _, err := ShortIDFromHash(bad); err == nil {
			t.Errorf("ShortIDFromHash(%q) succeeded", bad)
		}
	}
}

func TestWorldFilename(t *testing.T) {
	got, err := WorldFilename("a41f")
	if err != nil {
		t.Fatalf("WorldFilename: %v", err)
	}
	if got != "w_a41f.thesis" {
		t.Errorf("WorldFilename = %q, want w_a41f.thesis (directive 4.6)", got)
	}
	if _, err := WorldFilename("nope!"); err == nil {
		t.Error("WorldFilename accepted a non-hex short id")
	}
}

func TestRunIDFromHash(t *testing.T) {
	got, err := RunIDFromHash(
		time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC),
		"sha256:a41fb0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b8")
	if err != nil {
		t.Fatalf("RunIDFromHash: %v", err)
	}
	if got != "r_2026_09_03_a41f" {
		t.Errorf("got %q", got)
	}
}
