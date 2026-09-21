package schema

import (
	"encoding/json"
	"fmt"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration is a configuration duration scalar: a Go duration string such as
// "90s", "10m" or "8h" (directive 4.2 uses all three).
//
// # Why a custom type rather than time.Duration
//
// yaml.v3 decodes a plain integer into a time.Duration as a NANOSECOND count,
// so `budget: 90` would silently mean 90ns rather than 90 seconds. A budget is
// lock-covered configuration (invariant I6), so a silent unit guess there is a
// gate-weakening vector, not a convenience. This type rejects a bare number and
// says what to write instead.
//
// # Why UnmarshalYAML takes a *yaml.Node
//
// The decision is forced. yaml.v3 will decode ANY scalar node into a Go string
// when asked, so the "probe with a string first" trick used for int-or-string
// unions reports success for `budget: 90` with the value "90", and the
// diagnostic below would never fire. The resolved node tag is the only reliable
// discriminator. See DECISIONS.md D-007 and PHASE0_BUILD_BRIEF.md D-A.
type Duration time.Duration

// Std returns the standard library value.
func (d Duration) Std() time.Duration { return time.Duration(d) }

// Seconds returns the duration in seconds.
func (d Duration) Seconds() float64 { return time.Duration(d).Seconds() }

// Milliseconds returns the duration in whole milliseconds.
func (d Duration) Milliseconds() int64 { return time.Duration(d).Milliseconds() }

// IsZero reports whether the duration is exactly zero, which is how an absent
// key is represented.
func (d Duration) IsZero() bool { return d == 0 }

// String renders the duration the way time.Duration does, so "90s" re-emits as
// "1m30s". The tool never rewrites a user's prothesis.yaml, so the
// normalization is visible only in effective-config dumps, where the normalized
// semantic value is exactly what should be shown.
func (d Duration) String() string { return time.Duration(d).String() }

// ParseDuration parses a Go duration string. A bare number is rejected: there
// is no defensible unit to guess.
func ParseDuration(s string) (Duration, error) {
	if s == "" {
		return 0, fmt.Errorf("duration: empty value (write a Go duration such as \"90s\", \"10m\" or \"8h\")")
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("duration: %q is not a duration (write a unit, e.g. %q for %s seconds): %w",
			s, s+"s", s, err)
	}
	return Duration(v), nil
}

// UnmarshalYAML decodes a duration by switching on the RESOLVED node tag.
func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	s, err := scalarNode(n, "duration")
	if err != nil {
		return err
	}
	switch s.Tag {
	case yamlTagStr:
		v, err := ParseDuration(s.Value)
		if err != nil {
			return fmt.Errorf("%w (at line %d)", err, s.Line)
		}
		*d = v
		return nil
	case yamlTagInt, yamlTagFloat:
		return fmt.Errorf("duration: %s needs a unit at line %d; write %q if you meant seconds "+
			"(a bare number would be read as nanoseconds)", s.Value, s.Line, s.Value+"s")
	case yamlTagNull:
		return fmt.Errorf("duration: null at line %d; remove the key to keep the default, "+
			"or write a duration such as \"90s\"", s.Line)
	default:
		return fmt.Errorf("duration: want a duration string such as \"90s\", got %s at line %d",
			tagName(s.Tag), s.Line)
	}
}

// MarshalYAML emits the duration as a string so a dumped effective config
// round-trips through UnmarshalYAML.
func (d Duration) MarshalYAML() (any, error) { return d.String(), nil }

// UnmarshalJSON accepts a duration string. A JSON number is rejected for the
// same reason a YAML integer is.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("duration: must be a string such as \"90s\", got %s", string(b))
	}
	v, err := ParseDuration(s)
	if err != nil {
		return err
	}
	*d = v
	return nil
}

// MarshalJSON emits the duration as a string.
func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(d.String()) }
