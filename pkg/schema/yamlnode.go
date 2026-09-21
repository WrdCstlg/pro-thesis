package schema

import (
	"fmt"
	"sort"

	"gopkg.in/yaml.v3"
)

// YAML resolved scalar tags. Decoding an int-or-string union correctly requires
// looking at the RESOLVED TAG of the node, not at whether a probe decode into a
// Go string happened to succeed: yaml.v3 decodes any scalar node into a string
// when asked, so `retain_passing: 3` would probe-decode as the string "3".
const (
	yamlTagStr   = "!!str"
	yamlTagInt   = "!!int"
	yamlTagFloat = "!!float"
	yamlTagBool  = "!!bool"
	yamlTagNull  = "!!null"
)

// scalarNode returns n resolved through any alias, requiring a scalar.
func scalarNode(n *yaml.Node, kind string) (*yaml.Node, error) {
	n = derefAlias(n)
	if n == nil {
		return nil, fmt.Errorf("%s: missing value", kind)
	}
	if n.Kind != yaml.ScalarNode {
		return nil, fmt.Errorf("%s: want a scalar, got %s at line %d", kind, nodeKindName(n.Kind), n.Line)
	}
	return n, nil
}

// scalarString requires a scalar node and returns its literal text. A YAML null
// yields the empty string.
func scalarString(n *yaml.Node, kind string) (string, error) {
	s, err := scalarNode(n, kind)
	if err != nil {
		return "", err
	}
	if s.Tag == yamlTagNull {
		return "", nil
	}
	return s.Value, nil
}

// stringScalar requires a scalar node whose RESOLVED TAG is !!str. `foo: 3`
// therefore fails here rather than silently becoming "3".
func stringScalar(n *yaml.Node, kind string) (string, error) {
	s, err := scalarNode(n, kind)
	if err != nil {
		return "", err
	}
	if s.Tag != yamlTagStr {
		return "", fmt.Errorf("%s: want a string, got %s value %q at line %d",
			kind, tagName(s.Tag), s.Value, s.Line)
	}
	return s.Value, nil
}

func derefAlias(n *yaml.Node) *yaml.Node {
	for n != nil && n.Kind == yaml.AliasNode {
		n = n.Alias
	}
	return n
}

func nodeKindName(k yaml.Kind) string {
	switch k {
	case yaml.DocumentNode:
		return "a document"
	case yaml.SequenceNode:
		return "a sequence"
	case yaml.MappingNode:
		return "a mapping"
	case yaml.ScalarNode:
		return "a scalar"
	case yaml.AliasNode:
		return "an alias"
	}
	return "an unknown node"
}

func tagName(tag string) string {
	switch tag {
	case yamlTagStr:
		return "a string"
	case yamlTagInt:
		return "an integer"
	case yamlTagFloat:
		return "a float"
	case yamlTagBool:
		return "a boolean"
	case yamlTagNull:
		return "null"
	}
	return tag
}

// sortStable is sort.SliceStable with a typed comparator, so call sites read as
// comparisons over values rather than over indices.
func sortStable[T any](s []T, less func(a, b T) bool) {
	sort.SliceStable(s, func(i, j int) bool { return less(s[i], s[j]) })
}
