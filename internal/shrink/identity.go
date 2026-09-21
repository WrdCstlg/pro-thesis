package shrink

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// ---------------------------------------------------------------------------
// Violation identity: RULE 1
//
// "An accepted reduction must reproduce the SAME violation, not any violation."
//
// This file defines what "the same" means, because ddmin cannot be sound without
// an answer. Too loose and a reduction that switched bugs is accepted, and the
// minimal repro reproduces something else. Too strict and every legitimate
// reduction is rejected, and the shrink silently degrades to "no reduction
// possible", which looks like a correct negative result and is not.
// ---------------------------------------------------------------------------

// Identity is the fingerprint of one violation.
//
// It is built from the fields the frozen schemas already carry: the oracle's own
// name, its class, and its witness. Nothing here is invented normative surface:
// Identity never leaves this package, and what reaches the verdict is only
// schema.Shrink and schema.MinimalRepro.
type Identity struct {
	// Oracle is the oracle's own name, e.g. "linearizable.kv".
	Oracle string
	// Class is the oracle class, e.g. consistency.
	Class schema.OracleClass
	// Severity is carried for reporting only. It is DERIVED from Class
	// (schema.DefaultSeverity), so matching on it would be matching on Class
	// twice.
	Severity schema.Severity
	// Key is the witness key when the finding is key-scoped, "" otherwise.
	Key string
	// OpIDs are the witness op ids. Carried for reporting and for the strictest
	// match level; see Strictness for why they are NOT part of the default.
	OpIDs []int64
}

// IdentityOf builds an Identity from a verdict violation.
func IdentityOf(v schema.Violation) Identity {
	return Identity{
		Oracle:   v.Oracle,
		Class:    v.Class,
		Severity: v.Severity,
		Key:      v.Witness.Key,
		OpIDs:    append([]int64(nil), v.Witness.OpIDs...),
	}
}

// IdentityOfOutput builds an Identity from an oracle's own output document.
// Severity is derived exactly as schema.OracleOutput.ToViolation derives it, so
// an Identity taken from an output and one taken from the violation it becomes
// are equal.
func IdentityOfOutput(out schema.OracleOutput) Identity {
	return Identity{
		Oracle:   out.Oracle,
		Class:    out.Class,
		Severity: schema.DefaultSeverity(out.Class),
		Key:      out.Witness.Key,
		OpIDs:    append([]int64(nil), out.Witness.OpIDs...),
	}
}

// Zero reports whether the identity names no oracle at all, which is not a
// violation and can never be shrunk toward.
func (id Identity) Zero() bool { return id.Oracle == "" }

// String renders the identity for logs and reports.
func (id Identity) String() string {
	var b strings.Builder
	b.WriteString(id.Oracle)
	if id.Class != "" {
		b.WriteString(" (")
		b.WriteString(string(id.Class))
		b.WriteString(")")
	}
	if id.Key != "" {
		b.WriteString(" key=")
		b.WriteString(id.Key)
	}
	if len(id.OpIDs) > 0 {
		b.WriteString(" ops=[")
		for i, op := range id.OpIDs {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(strconv.FormatInt(op, 10))
		}
		b.WriteByte(']')
	}
	return b.String()
}

// key is a stable de-duplication key for the divergence set.
func (id Identity) key() string {
	return id.Oracle + "\x00" + string(id.Class) + "\x00" + id.Key
}

// ---------------------------------------------------------------------------
// Strictness
// ---------------------------------------------------------------------------

// Strictness is how much of the original violation a candidate must reproduce
// before its reduction is accepted.
type Strictness int

const (
	// MatchUnset is the ZERO VALUE, and it is deliberately not a match level.
	//
	// If the weakest level were the zero value, every caller who left Policy
	// alone would silently get the weakest identity test in the package, and
	// RULE 1's failure mode would be reachable by not typing anything.
	// Policy.withDefaults promotes MatchUnset to MatchWitnessKey; Policy.Validate
	// refuses an explicit MatchOracle. Together those mean the floor is reached
	// only by asking for it and being told no.
	MatchUnset Strictness = iota

	// MatchOracle requires only that the same oracle fired.
	//
	// It is BELOW the directive's floor ("at minimum oracle name and class must
	// match") and exists so the floor is expressible in the type rather than
	// only in prose.
	MatchOracle

	// MatchOracleClass is the directive's floor: the same oracle AND the same
	// class.
	MatchOracleClass

	// MatchWitnessKey is the DEFAULT. Oracle, class, and, when the original
	// violation and the candidate BOTH carry a witness key, the same key.
	//
	// Why the key is in and the op ids are out, which is the load-bearing
	// judgement of this file:
	//
	//   - The key MAY be the discriminator that catches a switched bug, on a
	//     target whose key is stable. Whether this target's is stable is not
	//     assumed here: stage 0 measures it, and the whole run keeps measuring
	//     it, and the level is calibrated down to MatchOracleClass when the
	//     measurement says the key is a scheduling artifact (D-054).
	//
	//     That correction was earned. This comment used to argue for the key by
	//     asserting, in one sentence, that "a stale read on k/0 and a stale read
	//     on k/7 are the same defect" and, in the next, that a violation which
	//     moved to another key "is a different finding and must not be accepted".
	//     Those disagree, and only the second was implemented. The fixture then
	//     settled it: 27 recorded violations of the ONE injected defect land on
	//     three keys, a single run has produced four, and holding the stricter
	//     rule anyway reduced 14 faults to 1 and then threw the answer away at a
	//     1/3 confirmation. See OQ-047.
	//
	//   - Op ids are NOT stable across executions of the same defect, and this
	//     is MEASURED in this repository rather than assumed. OQ-034 records two
	//     definition-of-done runs of the SAME defect under the SAME schedule:
	//
	//         run 8400  witness {key: k/0, op_ids: [11430, 11597, 11602]}
	//         run d731  witness {key: k/0, op_ids: [11402, 11580, 11578]}
	//
	//     Same key, entirely different op ids. Requiring op-id equality would
	//     have rejected a genuine reproduction of the identical bug; a false
	//     rejection on every candidate, which does not merely lose reductions:
	//     it makes the whole shrink report "nothing could be removed" while
	//     spending the full budget proving nothing.
	//
	//   - Stage 2 makes it worse still. ddmin over the operation trace DELETES
	//     operations on purpose, so the ids the original witness names are
	//     exactly the ones that stop existing. Op-id equality would make op
	//     shrinking structurally unable to accept a single reduction.
	//
	//   - The "both carry a key" condition matters. A crash oracle's witness has
	//     no key, and demanding one would reject every non-key-scoped violation
	//     outright. When either side has no key the level degrades to
	//     MatchOracleClass FOR THAT COMPARISON, which is still the directive's
	//     floor and is never weaker than it.
	MatchWitnessKey

	// MatchWitnessOps additionally requires the candidate's witness op ids to be
	// a superset of the original's.
	//
	// Correct only for a Tier A (deterministic) driver, where the same seed
	// issues the same operations with the same ids. It is offered because such a
	// driver is the whole point of Tier A, and refused as a default because the
	// fixture is Tier B and the measurement above says what that costs.
	MatchWitnessOps
)

// String renders the level.
func (s Strictness) String() string {
	switch s {
	case MatchUnset:
		return "unset"
	case MatchOracle:
		return "oracle"
	case MatchOracleClass:
		return "oracle+class"
	case MatchWitnessKey:
		return "oracle+class+witness_key"
	case MatchWitnessOps:
		return "oracle+class+witness_key+op_ids"
	}
	return fmt.Sprintf("Strictness(%d)", int(s))
}

// Valid reports whether s is one of the defined levels, MatchUnset included:
// unset is legal on a Policy nobody filled in, and withDefaults resolves it.
func (s Strictness) Valid() bool { return s >= MatchUnset && s <= MatchWitnessOps }

// Match reports whether cand reproduces orig at this strictness, and returns the
// reason when it does not.
//
// The reason is not decoration. A shrink that quietly rejects every candidate
// because the witness key moved is indistinguishable, in the verdict, from a
// world whose faults were all essential, and the two demand opposite responses
// from whoever reads it.
func (s Strictness) Match(orig, cand Identity) (bool, string) {
	if orig.Zero() {
		return false, "the original violation names no oracle"
	}
	if cand.Oracle != orig.Oracle {
		return false, fmt.Sprintf("oracle %q, want %q", cand.Oracle, orig.Oracle)
	}
	if s >= MatchOracleClass && cand.Class != orig.Class {
		return false, fmt.Sprintf("class %q, want %q", cand.Class, orig.Class)
	}
	if s >= MatchWitnessKey && orig.Key != "" && cand.Key != "" && cand.Key != orig.Key {
		return false, fmt.Sprintf("witness key %q, want %q", cand.Key, orig.Key)
	}
	if s >= MatchWitnessOps {
		if missing := missingOps(orig.OpIDs, cand.OpIDs); len(missing) > 0 {
			return false, fmt.Sprintf("witness is missing op id(s) %v", missing)
		}
	}
	return true, ""
}

// missingOps returns the elements of want that have is does not contain.
func missingOps(want, have []int64) []int64 {
	if len(want) == 0 {
		return nil
	}
	set := make(map[int64]struct{}, len(have))
	for _, op := range have {
		set[op] = struct{}{}
	}
	var missing []int64
	for _, op := range want {
		if _, ok := set[op]; !ok {
			missing = append(missing, op)
		}
	}
	sort.Slice(missing, func(i, j int) bool { return missing[i] < missing[j] })
	return missing
}
