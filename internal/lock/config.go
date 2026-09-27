package lock

import (
	"bytes"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// CoveredPaths are the prothesis.yaml paths the lock digests, in sorted order.
//
// The Phase 3 brief fixes the minimum: `perturber.allow`, `perturber.deny`,
// `perturber.budget`, every profile's `budget` and `worlds`, and the top-level
// `search:` block. Three of the entries below go beyond it, each for a stated
// reason:
//
//   - `profiles` in whole rather than only budget and worlds. Covering the two
//     named keys would leave `driver_profile` open, and swapping
//     `profiles.gate.driver_profile: gate` for `smoke` exchanges a 20,000-op
//     workload for a 500-op one: a one-token gate weakening that touches no
//     oracle. Covering the block is a superset of the requirement, so nothing
//     required is lost.
//   - `oracles.builtin`, because deleting `no_stuck_op` from the list must not
//     be invisible. pkg/schema already refuses to default this list for
//     exactly this reason.
//   - `oracles.dir`, because repointing it is how you swap the whole oracle
//     set in one line.
//   - `driver.cmd`, because the driver writes the history every consistency
//     oracle judges. Pinning it to a smaller profile, or adding `--ops 10`,
//     hid the planted defect with the digest unmoved (OQ-076, D-090).
//
// `driver.profiles` is covered BY REFERENCE, not here: ProjectConfig digests
// `driver.profiles.<name>` for every name a run profile's `driver_profile`
// names, and no other. A driver profile no run profile uses is not the gate's
// workload, and covering it would fire exit 4 on routine edits (OQ-026, D-090).
var CoveredPaths = []string{
	"driver.cmd",
	// D-066. harness.health is the URL availability_after_heal judges a node
	// at (convergenceProbes), so repointing it at an always-200 path is a gate
	// weakening; harness.role_probe decides which node `role:leader` hits.
	"harness.health",
	"harness.role_probe",
	"oracles.builtin",
	"oracles.dir",
	"perturber.allow",
	"perturber.budget",
	"perturber.deny",
	"profiles",
	"search",
}

// maxProjectionDepth bounds recursion. YAML anchors can be nested arbitrarily
// and a hand-written document can be far deeper than any real config; a bound
// turns a pathological file into a named error rather than a stack overflow in
// the process that is supposed to be checking it.
const maxProjectionDepth = 64

// ProjectConfig flattens the covered subset of a RAW prothesis.yaml into sorted
// leaf entries.
//
// It takes BYTES, not a decoded *schema.Config, and that signature is the
// mechanism behind D-F: a compiled-in default is unreachable from here, so it
// cannot enter the digest preimage. A key the user did not write produces no
// entry at all: absence is represented by absence, never by the value a
// default would have supplied.
//
// It does not validate. An invalid config is the config decoder's business
// (exit 5); the lock's business is to notice change, and it must be able to
// digest a document it would not itself accept.
func ProjectConfig(data []byte) ([]ConfigEntry, error) {
	out := []ConfigEntry{}
	if len(bytes.TrimSpace(data)) == 0 {
		return out, nil
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("lock: parse prothesis.yaml: %w", err)
	}
	root := resolveAlias(&doc, 0)
	if root == nil {
		return out, nil
	}
	if root.Kind == yaml.DocumentNode {
		if len(root.Content) == 0 {
			return out, nil
		}
		root = resolveAlias(root.Content[0], 0)
	}
	if root == nil || root.Kind != yaml.MappingNode {
		// A non-mapping document carries none of the covered paths. Returning
		// an empty projection is right: there is nothing covered to digest, and
		// the config decoder will reject the document on its own terms.
		return out, nil
	}

	for _, p := range CoveredPaths {
		n := lookup(root, strings.Split(p, "."), 0)
		if n == nil {
			continue
		}
		if err := flatten(n, p, &out, 0); err != nil {
			return nil, err
		}
	}
	for _, name := range referencedDriverProfiles(root) {
		n := lookup(root, []string{"driver", "profiles", name}, 0)
		if n == nil {
			// A run profile naming a driver profile that does not exist is the
			// config decoder's to reject (exit 5). The reference itself is already
			// covered under `profiles`, so absence stays absence here.
			continue
		}
		if err := flatten(n, joinPath(DriverProfilesPath, name), &out, 0); err != nil {
			return nil, err
		}
	}
	sortEntries(out)
	return out, nil
}

// DriverProfilesPath is the config section covered by reference.
const DriverProfilesPath = "driver.profiles"

// CoveredByReference describes, for the lock file's `covers` list, the entries
// ProjectConfig digests by following a reference rather than a fixed path.
const CoveredByReference = "driver.profiles.<name>, for every <name> a profiles.*.driver_profile names"

// referencedDriverProfiles returns the driver profile names that some run
// profile's `driver_profile` names, sorted and without duplicates, so the
// projection is a total order whatever order the profiles were written in.
func referencedDriverProfiles(root *yaml.Node) []string {
	profiles := lookup(root, []string{"profiles"}, 0)
	if profiles == nil || profiles.Kind != yaml.MappingNode {
		return nil
	}
	seen := map[string]bool{}
	for i := 0; i+1 < len(profiles.Content); i += 2 {
		ref := lookup(profiles.Content[i+1], []string{"driver_profile"}, 1)
		if ref == nil || ref.Kind != yaml.ScalarNode || ref.Tag == "!!null" || ref.Value == "" {
			continue
		}
		seen[ref.Value] = true
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func sortEntries(e []ConfigEntry) {
	sort.Slice(e, func(i, j int) bool {
		if e[i].Path != e[j].Path {
			return e[i].Path < e[j].Path
		}
		if e[i].Kind != e[j].Kind {
			return e[i].Kind < e[j].Kind
		}
		return e[i].Value < e[j].Value
	})
}

// lookup walks a mapping by key segments, resolving aliases as it goes.
// It returns nil when any segment is absent, which is how "the user did not
// write this" reaches the projection.
func lookup(n *yaml.Node, segs []string, depth int) *yaml.Node {
	if depth > maxProjectionDepth {
		return nil
	}
	n = resolveAlias(n, depth)
	if n == nil || len(segs) == 0 {
		return n
	}
	if n.Kind != yaml.MappingNode {
		return nil
	}
	// LAST match wins. A duplicate mapping key is rejected outright by
	// schema.DecodeConfig, so such a document can never be run; but where one
	// is tolerated at all, YAML's convention is that the last binding is the
	// effective one, and the lock must not report the value a decoder would
	// have discarded.
	var found *yaml.Node
	for i := 0; i+1 < len(n.Content); i += 2 {
		k := resolveAlias(n.Content[i], depth)
		if k == nil || k.Value != segs[0] {
			continue
		}
		found = n.Content[i+1]
	}
	if found == nil {
		return nil
	}
	return lookup(found, segs[1:], depth+1)
}

// flatten turns a YAML subtree into leaf entries under path.
//
// Sequences of scalars are flattened as a MULTISET: every element becomes an
// entry sharing the parent path, and the entries are sorted. `perturber.allow`
// and `perturber.deny` are sets of fault kinds, and reordering one is
// semantically nothing: firing exit 4 on a reorder would be a spurious lock
// failure, which is the third way this phase fails. REMOVING an element is
// still caught, because the multiset shrinks. A sequence containing anything
// other than scalars falls back to indexed paths, where order does matter.
func flatten(n *yaml.Node, path string, out *[]ConfigEntry, depth int) error {
	if depth > maxProjectionDepth {
		return fmt.Errorf("lock: prothesis.yaml nests deeper than %d at %s", maxProjectionDepth, path)
	}
	n = resolveAlias(n, depth)
	if n == nil {
		return nil
	}
	switch n.Kind {
	case yaml.ScalarNode:
		if n.Tag == "!!null" {
			*out = append(*out, ConfigEntry{Path: path, Kind: KindNull})
			return nil
		}
		*out = append(*out, ConfigEntry{Path: path, Kind: KindScalar, Value: n.Value})
		return nil

	case yaml.MappingNode:
		if len(n.Content) == 0 {
			*out = append(*out, ConfigEntry{Path: path, Kind: KindEmptyMap})
			return nil
		}
		for i := 0; i+1 < len(n.Content); i += 2 {
			k := resolveAlias(n.Content[i], depth)
			if k == nil {
				continue
			}
			if err := flatten(n.Content[i+1], joinPath(path, k.Value), out, depth+1); err != nil {
				return err
			}
		}
		return nil

	case yaml.SequenceNode:
		if len(n.Content) == 0 {
			*out = append(*out, ConfigEntry{Path: path, Kind: KindEmptySeq})
			return nil
		}
		allScalar := true
		for _, c := range n.Content {
			if r := resolveAlias(c, depth); r == nil || r.Kind != yaml.ScalarNode {
				allScalar = false
				break
			}
		}
		if allScalar {
			for _, c := range n.Content {
				r := resolveAlias(c, depth)
				if r.Tag == "!!null" {
					*out = append(*out, ConfigEntry{Path: path, Kind: KindNull})
					continue
				}
				*out = append(*out, ConfigEntry{Path: path, Kind: KindScalar, Value: r.Value})
			}
			return nil
		}
		for i, c := range n.Content {
			if err := flatten(c, path+"["+strconv.Itoa(i)+"]", out, depth+1); err != nil {
				return err
			}
		}
		return nil

	case yaml.DocumentNode:
		if len(n.Content) == 0 {
			return nil
		}
		return flatten(n.Content[0], path, out, depth+1)

	default:
		return fmt.Errorf("lock: %s: unsupported YAML node kind %d", path, n.Kind)
	}
}

// resolveAlias follows an anchor reference. The depth bound is what stops a
// self-referential alias from looping forever.
func resolveAlias(n *yaml.Node, depth int) *yaml.Node {
	for i := 0; n != nil && n.Kind == yaml.AliasNode; i++ {
		if depth+i > maxProjectionDepth {
			return nil
		}
		n = n.Alias
	}
	return n
}

// joinPath renders a dotted path, quoting a key that is not a plain identifier
// so that a key containing a dot cannot be confused with nesting.
func joinPath(base, key string) string {
	if !plainKey(key) {
		key = strconv.Quote(key)
	}
	if base == "" {
		return key
	}
	return base + "." + key
}

func plainKey(k string) bool {
	if k == "" {
		return false
	}
	for i := 0; i < len(k); i++ {
		c := k[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '_' || c == '-':
		default:
			return false
		}
	}
	return true
}
