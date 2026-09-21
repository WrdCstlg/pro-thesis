package schema

import (
	"encoding/json"
	"fmt"
	"strconv"

	"gopkg.in/yaml.v3"
)

// RetainAllLiteral is the only string spelling accepted for an unbounded
// retention policy. Directive 4.2: `retain_failing: all`.
const RetainAllLiteral = "all"

// RetainPolicy is `artifacts.retain_passing` / `artifacts.retain_failing`.
//
// The directive writes one of each form:
//
//	retain_passing: 3
//	retain_failing: all
//
// Nothing makes that asymmetry normative (a user will write
// `retain_failing: 10`) so one polymorphic type serves both keys. A negative
// integer is a synonym for "all", which matches the `worlds: -1` convention
// already in the config and keeps one sentinel rule rather than two.
//
// Decoding switches on the RESOLVED YAML tag. The obsolete "probe with a
// string first" technique cannot work here: yaml.v3 decodes the integer scalar
// 3 into the Go string "3" without error, so the directive's own
// `retain_passing: 3` would be rejected as a CONFIG_ERROR. See DECISIONS.md
// D-007.
type RetainPolicy struct {
	// Unlimited is true for "all" (or any negative integer): keep every
	// artifact bundle.
	Unlimited bool
	// N is the number of bundles to keep when Unlimited is false. Zero means
	// keep none.
	N int
}

// RetainAll returns the unbounded policy.
func RetainAll() RetainPolicy { return RetainPolicy{Unlimited: true} }

// RetainN returns the policy that keeps the n most recent bundles. A negative n
// is normalized to the unbounded policy.
func RetainN(n int) RetainPolicy {
	if n < 0 {
		return RetainAll()
	}
	return RetainPolicy{N: n}
}

// Limit reports the retention count and whether it is unbounded. When
// unlimited is true the count is meaningless.
func (r RetainPolicy) Limit() (n int, unlimited bool) {
	if r.Unlimited {
		return 0, true
	}
	return r.N, false
}

// Keeps reports whether the i'th most recent bundle (0 = newest) is retained.
func (r RetainPolicy) Keeps(i int) bool {
	if r.Unlimited {
		return true
	}
	return i >= 0 && i < r.N
}

// String renders the policy in its config spelling.
func (r RetainPolicy) String() string {
	if r.Unlimited {
		return RetainAllLiteral
	}
	return strconv.Itoa(r.N)
}

// Validate rejects a bounded policy with a negative count. RetainN normalizes
// negatives, so this only fires on a hand-built struct literal.
func (r RetainPolicy) Validate() error {
	if !r.Unlimited && r.N < 0 {
		return fmt.Errorf("retention: negative count %d (use %q for unbounded)", r.N, RetainAllLiteral)
	}
	return nil
}

// UnmarshalYAML decodes either a non-negative integer or the string "all".
func (r *RetainPolicy) UnmarshalYAML(n *yaml.Node) error {
	s, err := scalarNode(n, "retention")
	if err != nil {
		return err
	}
	switch s.Tag {
	case yamlTagInt:
		v, err := strconv.Atoi(s.Value)
		if err != nil {
			return fmt.Errorf("retention: %q is not an integer at line %d", s.Value, s.Line)
		}
		*r = RetainN(v)
		return nil
	case yamlTagStr:
		if s.Value != RetainAllLiteral {
			return fmt.Errorf("retention: %q at line %d is not a retention policy "+
				"(write a non-negative integer, or %q for unbounded)", s.Value, s.Line, RetainAllLiteral)
		}
		*r = RetainAll()
		return nil
	default:
		return fmt.Errorf("retention: want a non-negative integer or %q, got %s at line %d",
			RetainAllLiteral, tagName(s.Tag), s.Line)
	}
}

// MarshalYAML emits "all" or the integer, so a dumped effective config
// round-trips.
func (r RetainPolicy) MarshalYAML() (any, error) {
	if r.Unlimited {
		return RetainAllLiteral, nil
	}
	return r.N, nil
}

// UnmarshalJSON mirrors UnmarshalYAML for JSON documents.
func (r *RetainPolicy) UnmarshalJSON(b []byte) error {
	var n int
	if err := json.Unmarshal(b, &n); err == nil {
		*r = RetainN(n)
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("retention: want a non-negative integer or %q, got %s", RetainAllLiteral, string(b))
	}
	if s != RetainAllLiteral {
		return fmt.Errorf("retention: %q is not a retention policy (write a non-negative integer or %q)",
			s, RetainAllLiteral)
	}
	*r = RetainAll()
	return nil
}

// MarshalJSON emits "all" or the integer.
func (r RetainPolicy) MarshalJSON() ([]byte, error) {
	if r.Unlimited {
		return json.Marshal(RetainAllLiteral)
	}
	return json.Marshal(r.N)
}
