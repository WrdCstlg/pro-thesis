// Package schema defines the frozen, normative wire contracts of PRO-THESIS.
//
// # Normativity
//
// Field names in this package are NORMATIVE. They are transcribed from the
// PRO-THESIS build directive (not published in this repository; D-069) and
// must not be renamed, retyped, or given new meaning. Fields marked ADDITIVE are
// extensions: they add keys where the directive names none for something that
// must exist. They never alter or remove a specified key. Every ADDITIVE field
// carries a DECISIONS.md entry.
//
// # Dependencies
//
// This package imports only the Go standard library and gopkg.in/yaml.v3.
// yaml.v3 is required by the configuration types: prothesis.yaml carries two
// int-or-string unions (retain_passing / retain_failing) and duration scalars,
// and the only correct way to decode those is UnmarshalYAML(*yaml.Node) with a
// switch on the resolved node tag. The obsolete func-based unmarshaler cannot
// do it: yaml.v3 will happily decode the integer scalar 3 into a Go string, so
// a "try string first" probe silently succeeds and the directive's own
// `retain_passing: 3` would be rejected as a CONFIG_ERROR. See DECISIONS.md
// D-007.
//
// The verdict, world, history and oracle contracts are JSON, so a third party
// consuming prothesis.verdict/v1 needs no YAML parser to do it.
//
// # Emission rule
//
// Never call encoding/json.Marshal directly on the types in this package. Use
// MarshalWorld, WriteVerdict, MarshalOracleInput, CanonicalMarshal, or the
// package-internal marshalNoEscape. Go's default encoder HTML-escapes '<' and
// '>', which turns the edge target n1<->n2 into n1<->n2; a different
// byte string, hence a different world hash and an unreadable verdict.
//
// # Two canonical forms, and which is which
//
// The world is hashed, so its bytes are a contract and it is encoded by
// internal/recorder/cjson: a reflective encoder over a fully typed graph with no
// map, no float and no interface. MarshalWorld and World.Hash both go through
// it, so a world's file and a world's identity are two views of one byte string.
//
// The verdict, history and oracle documents are NOT hashed and legitimately
// carry oracle-authored JSON this package declares no type for, which cjson
// forbids by design. They use CanonicalMarshal: encoding/json with HTML
// escaping off, plus a key sort. The two differ on inputs neither a world nor a
// verdict should contain (encoding/json escapes U+2028 and U+2029
// unconditionally), which is precisely why the hashed path does not use it.
//
// # Phase discipline
//
// This is a Phase 0 deliverable. It contains types, enums, parsers, canonical
// serializers and Validate(). It contains no I/O policy, no subprocess
// management, no Docker, no oracle, no fault injector, no coverage counter, no
// search loop and no lock manifest. Later phases add consumers, not new schema
// fields.
package schema

const (
	// ConfigVersion is the required value of prothesis.yaml `version`.
	ConfigVersion = "prothesis/v1"

	// WorldSchema identifies a .thesis world file.
	//
	// ADDITIVE: the directive freezes the world tuple (invariant I2) but names
	// no schema id for the file that carries it. Without one there is no way to
	// version-gate the format or to tell a .thesis from arbitrary JSON.
	WorldSchema = "prothesis.world/v1"

	// VerdictSchema is the `schema` value of prothesis.verdict/v1 (directive 4.6).
	VerdictSchema = "prothesis.verdict/v1"

	// OracleInputSchema is the `schema` value of the oracle stdin document
	// (directive 4.5).
	OracleInputSchema = "prothesis.oracle_input/v1"

	// OracleOutputSchema is the `schema` value of the oracle stdout document
	// (directive 4.5).
	OracleOutputSchema = "prothesis.oracle_output/v1"
)

// ToolName is the binary name. `prothesis` is an accepted alias.
const ToolName = "thesis"
