package schema

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/WrdCstlg/pro-thesis/internal/recorder/cjson"
)

// ---------------------------------------------------------------------------
// The .thesis world file
//
// Invariant I2: "Every execution is defined by a serializable world tuple:
// (seed, topology_variant, driver_profile, fault_schedule, phase_timings). Any
// violation must serialize to a self-contained .thesis file that reproduces the
// issue."
//
// The file's canonical encoding rules are mandatory and each closes a specific
// failure:
//
//  1. Object keys sorted by byte order.
//  2. No insignificant whitespace; exactly one trailing LF.
//  3. LF only. A CR means git translated the file (see .gitattributes).
//  4. HTML escaping OFF. Go escapes '<' and '>' by default, which would corrupt
//     the edge target n1<->n2 and change every world hash.
//  5. No bare floats anywhere. Go's shortest-float representation carries no
//     cross-version stability guarantee; ratios are integer parts-per-million.
//  6. No map and no interface{} in the type graph. Map iteration order is
//     randomized and interface{} decoding turns integers into float64.
//  7. Strict decoding: unknown fields are rejected.
//  8. UnmarshalWorld re-encodes and byte-compares on every call, so
//     byte-identity is a RUNTIME invariant, not merely a test assertion.
//
// Rules 5 and 6 are why no type in this graph is a map, a float or an
// interface, and why FaultSpec is kept out of it: the schedule is carried as
// canonical fault STRINGS, which is also the form the verdict's
// surviving_faults uses.
// ---------------------------------------------------------------------------

// ErrWorldCRLF reports CR bytes in a .thesis file. On Windows this has exactly
// one cause and one fix, so it is named separately from a syntax error.
var ErrWorldCRLF = errors.New("world: CR byte in .thesis file: git has translated line endings. " +
	"Check .gitattributes (`*.thesis text eol=lf`) and `git config core.autocrlf`")

// ErrWorldTrailingData reports content after the top-level object.
var ErrWorldTrailingData = errors.New("world: trailing data after the world object")

// NonCanonicalWorldError reports that a .thesis file parsed but its bytes are
// not the canonical encoding of the world it denotes.
//
// This is rule 8. It fires for unsorted keys, inserted whitespace, a missing
// trailing newline, HTML-escaped characters, and anything else that decodes but
// does not re-encode to the same bytes.
type NonCanonicalWorldError struct {
	// Offset is the byte offset of the first difference.
	Offset int
	// Want is a short excerpt of the canonical encoding at Offset.
	Want string
	// Got is a short excerpt of the file at Offset.
	Got string
}

func (e *NonCanonicalWorldError) Error() string {
	return fmt.Sprintf("world: file is not canonical at byte %d: canonical form has %q, file has %q "+
		"(a .thesis file must be byte-identical to its canonical encoding)", e.Offset, e.Want, e.Got)
}

// World is a .thesis file.
//
// Field order in this declaration is irrelevant: the canonical encoder sorts
// keys, so reordering the struct is a no-op rather than a silent rehash of
// every committed world.
type World struct {
	// Schema is always WorldSchema.
	//
	// ADDITIVE: the directive freezes the world tuple but names no schema id
	// for the file that carries it, so there is otherwise no way to
	// version-gate the format or to tell a .thesis from arbitrary JSON.
	Schema string `json:"schema"`

	// Seed is the root PRNG seed. It is a JSON number and that is exact: the
	// type graph is fully typed, so encoding/json marshals and unmarshals
	// uint64 losslessly. The float64 precision trap applies only to
	// interface{} decoding, which rule 6 forbids.
	//
	// Zero is a legal seed. Nothing reserves it, and forbidding it would make
	// a World{} literal invalid in every test for a reason unrelated to what
	// the test checks.
	Seed uint64 `json:"seed"`

	// TopologyVariant names the topology this world ran against.
	TopologyVariant string `json:"topology_variant"`

	// DriverProfile names an entry of driver.profiles in prothesis.yaml.
	DriverProfile string `json:"driver_profile"`

	FaultSchedule FaultSchedule `json:"fault_schedule"`

	// PhaseTimings is the measured lifecycle window list, in milliseconds
	// relative to DRIVE start.
	PhaseTimings PhaseTimings `json:"phase_timings"`

	// SUT identifies the build that ran.
	//
	// ADDITIVE. I2's five-tuple omits the identity of the system under test,
	// which makes three things unsound: a regression world replayed after a
	// patch silently tests a different program, `thesis bisect` cannot be
	// correct, and `reproduced: "3/3"` is a claim about an unknown binary.
	// Included in the hash. Logged as OQ-011.
	SUT SUT `json:"sut"`

	// Meta is provenance. It is EXCLUDED from the world hash and omitted when
	// empty, so recording where a world came from never changes its identity,
	// and so later phases can add provenance fields without invalidating a
	// single committed world.
	Meta *WorldMeta `json:"meta,omitempty"`
}

// FaultSchedule is the world's `fault_schedule`.
//
// # Why the schedule is split
//
// This split is required by the BASE specification, independent of any search
// engine. role:leader, minority(kv) and kv:* bind to concrete nodes at
// injection time from live cluster state, and Raft leadership moves at runtime.
// A world that records only proc.pause(role:leader) does not necessarily
// reproduce, because on replay a different node may be leader. Realized records
// what was actually injected, against which concrete nodes, at which
// virtual-clock time; `thesis replay` executes it when present.
type FaultSchedule struct {
	// Planned is the schedule as authored or searched: canonical fault strings
	// in the KIND(TARGET[, PARAMS])@start..end grammar.
	Planned []string `json:"planned"`
	// Realized is what actually happened. It is null for a world that has
	// never been executed, which is a different statement from an empty list
	// (executed, and nothing was injected).
	Realized []RealizedFault `json:"realized"`
}

// RealizedFault records one actually-injected fault.
//
// ADDITIVE in full: the brief fixes `fault_schedule.realized` as a list but
// names no element shape.
type RealizedFault struct {
	// Fault is the planned fault's canonical string, so a realized entry can
	// be matched back to the plan it came from.
	Fault string `json:"fault"`
	// Resolved is the canonical string with the target bound to a concrete
	// node, e.g. proc.pause(n2)@8200..11000 for a planned role:leader.
	Resolved string `json:"resolved"`
	// Nodes are the concrete node ids the target resolved to, in the order the
	// resolver produced them.
	Nodes []string `json:"nodes"`
	// StartMS and EndMS are the virtual-clock milliseconds at which injection
	// and withdrawal actually happened, which need not equal the planned
	// window.
	StartMS int64 `json:"start_ms"`
	EndMS   int64 `json:"end_ms"`
}

// SUT identifies the system under test by resolved image digest.
type SUT struct {
	Images []SUTImage `json:"images"`
}

// SUTImage is one resolved image.
type SUTImage struct {
	// Service is the harness service name the image backs.
	Service string `json:"service"`
	// Image is the reference as written, e.g. "kvfixture:buggy".
	Image string `json:"image"`
	// Digest is the resolved content address, e.g. "sha256:...".
	Digest string `json:"digest"`
}

// WorldMeta is unhashed provenance.
type WorldMeta struct {
	// Origin says how the world came to exist: "init", "search", "shrink",
	// "replay". It is an open string.
	Origin string `json:"origin,omitempty"`
	// ParentHash is the world hash this world was derived from, if any.
	ParentHash string `json:"parent_hash,omitempty"`
}

// NewWorld returns a normalized, unexecuted world.
func NewWorld(seed uint64, topologyVariant, driverProfile string) World {
	return World{
		Schema:          WorldSchema,
		Seed:            seed,
		TopologyVariant: topologyVariant,
		DriverProfile:   driverProfile,
		FaultSchedule:   FaultSchedule{Planned: []string{}},
		PhaseTimings:    PhaseTimings{},
		SUT:             SUT{Images: []SUTImage{}},
	}
}

// Normalized returns a copy in canonical shape:
//
//   - schema stamped
//   - nil lists that must be present become empty lists (JSON null and []
//     encode to different bytes, so a no-fault world could otherwise hash two
//     ways depending on how it was constructed)
//   - planned faults sorted by (start_ms, end_ms, string), so a world's hash
//     does not depend on the order in which a search discovered its faults
//   - an empty Meta is dropped, so provenance-free worlds have one shape
//
// It does not mutate the receiver: hashing a candidate mid-mutation must never
// reorder the corpus entry it was derived from.
func (w World) Normalized() World {
	out := w
	out.Schema = WorldSchema

	out.FaultSchedule.Planned = SortFaultStrings(w.FaultSchedule.Planned)
	if out.FaultSchedule.Planned == nil {
		out.FaultSchedule.Planned = []string{}
	}
	if w.FaultSchedule.Realized != nil {
		realized := make([]RealizedFault, len(w.FaultSchedule.Realized))
		copy(realized, w.FaultSchedule.Realized)
		for i := range realized {
			if realized[i].Nodes == nil {
				realized[i].Nodes = []string{}
			}
		}
		out.FaultSchedule.Realized = realized
	}

	out.PhaseTimings = w.PhaseTimings.Normalize()

	images := make([]SUTImage, len(w.SUT.Images))
	copy(images, w.SUT.Images)
	sortStable(images, func(a, b SUTImage) bool {
		if a.Service != b.Service {
			return a.Service < b.Service
		}
		if a.Image != b.Image {
			return a.Image < b.Image
		}
		// (service, image) can tie with different digests: a tag drifting
		// between node pulls. Order by digest too, or canonical bytes would
		// depend on the order the nodes happened to bind in.
		return a.Digest < b.Digest
	})
	out.SUT.Images = images

	if w.Meta != nil && *w.Meta == (WorldMeta{}) {
		out.Meta = nil
	} else if w.Meta != nil {
		m := *w.Meta
		out.Meta = &m
	}
	return out
}

// worldIdentity is the hashed projection: exactly invariant I2's five-tuple
// plus sut. The schema id and meta are excluded, so stamping provenance on a
// world never changes what the world IS.
type worldIdentity struct {
	DriverProfile   string        `json:"driver_profile"`
	FaultSchedule   FaultSchedule `json:"fault_schedule"`
	PhaseTimings    PhaseTimings  `json:"phase_timings"`
	Seed            uint64        `json:"seed"`
	SUT             SUT           `json:"sut"`
	TopologyVariant string        `json:"topology_variant"`
}

func (w *World) identity() worldIdentity {
	return worldIdentity{
		DriverProfile:   w.DriverProfile,
		FaultSchedule:   w.FaultSchedule,
		PhaseTimings:    w.PhaseTimings,
		Seed:            w.Seed,
		SUT:             w.SUT,
		TopologyVariant: w.TopologyVariant,
	}
}

// Hash returns "sha256:"+hex over the canonical encoding of the world
// identity, exactly as the receiver stands.
//
// It does NOT normalize first. That is deliberate: a hand-reordered or
// hand-edited world must hash differently from the sealed one, or editing a
// committed regression would be invisible.
//
// The preimage comes from the same encoder MarshalWorld uses (see there), so a
// world's identity and its file are two views of one byte string rather than
// two independently-derived ones that happen to agree.
func (w *World) Hash() (string, error) {
	b, err := cjson.EncodeCompact(w.identity())
	if err != nil {
		return "", fmt.Errorf("world: hash preimage: %w", err)
	}
	sum := sha256.Sum256(b)
	return HashPrefix + hex.EncodeToString(sum[:]), nil
}

// ShortID returns the first ShortIDLen hex digits of the world hash. It is the
// id used in world filenames: w_a41f.thesis.
func (w *World) ShortID() (string, error) {
	h, err := w.Hash()
	if err != nil {
		return "", err
	}
	return ShortIDFromHash(h)
}

// Validate checks a world's structural invariants. It does not compare against
// a config: whether driver_profile resolves is a question for the run, not for
// the file.
func (w *World) Validate() error {
	var errs ValidationErrors
	if w.Schema != WorldSchema {
		errs.Add("schema", "is %q, want %q", w.Schema, WorldSchema)
	}
	if w.TopologyVariant == "" {
		errs.Add("topology_variant", "missing (names the topology this world ran against)")
	}
	if w.DriverProfile == "" {
		errs.Add("driver_profile", "missing (names an entry of driver.profiles)")
	}
	if w.FaultSchedule.Planned == nil {
		errs.Add("fault_schedule.planned", "is null; write [] for a world with no faults")
	}
	for i, s := range w.FaultSchedule.Planned {
		f, err := ParseFault(s)
		if err != nil {
			errs.Add(indexPath("fault_schedule.planned", i), "%s", err.Error())
			continue
		}
		if c := f.String(); c != s {
			errs.Add(indexPath("fault_schedule.planned", i),
				"%q is not in canonical form; write %q", s, c)
		}
	}
	for i, r := range w.FaultSchedule.Realized {
		p := indexPath("fault_schedule.realized", i)
		if r.Fault == "" {
			errs.Add(p+".fault", "missing (the planned fault this entry realizes)")
		} else if _, err := ParseFault(r.Fault); err != nil {
			errs.Add(p+".fault", "%s", err.Error())
		}
		if r.Resolved == "" {
			errs.Add(p+".resolved", "missing (the fault as actually injected)")
		} else if _, err := ParseFault(r.Resolved); err != nil {
			errs.Add(p+".resolved", "%s", err.Error())
		}
		if r.Nodes == nil {
			errs.Add(p+".nodes", "is null; write [] if the target resolved to no node")
		}
		if r.EndMS < r.StartMS {
			errs.Add(p, "realized window %d..%d ends before it starts", r.StartMS, r.EndMS)
		}
	}
	if w.PhaseTimings == nil {
		errs.Add("phase_timings", "is null; write [] for a world that has not been executed")
	} else {
		errs.Merge("phase_timings", w.PhaseTimings.Validate())
	}
	if w.SUT.Images == nil {
		errs.Add("sut.images", "is null; write [] if no image digests were resolved")
	}
	for i, img := range w.SUT.Images {
		p := indexPath("sut.images", i)
		if img.Service == "" {
			errs.Add(p+".service", "missing")
		}
		if img.Image == "" {
			errs.Add(p+".image", "missing")
		}
	}
	return errs.OrNil()
}

// MarshalWorld returns the canonical .thesis bytes, including the single
// trailing LF.
//
// # Why this does not use CanonicalMarshal
//
// CanonicalMarshal is encoding/json plus a key sort. That is the right tool for
// the verdict, the history and the oracle documents, whose payloads legitimately
// carry oracle-authored JSON this package never declared a type for. It is the
// WRONG tool for the world, and using it here gave the tree two canonical forms:
//
//	encoding/json escapes U+2028 and U+2029 unconditionally — SetEscapeHTML(false)
//	does not disable it — while the reflective encoder does not. A world whose
//	topology_variant contained either character encoded one way through
//	recorder.StoreWorld and another through MarshalWorld, hashed to two different
//	identities, and produced a file that UnmarshalWorld rejected as non-canonical
//	immediately after the recorder wrote it.
//
// Byte-identity (Phase 0 definition of done (b)) is not a property two encoders
// can jointly hold; it is a property of one encoder. The world is a fully typed
// graph with no map, no float and no interface (exactly cjson's domain) so the
// world path delegates, and cjson is the single definition of canonical form for
// anything whose bytes are hashed. TestCodecsAgreeOnEveryStringByte in
// internal/recorder is the standing guard.
func MarshalWorld(w *World) ([]byte, error) {
	if w == nil {
		return nil, errors.New("world: MarshalWorld(nil)")
	}
	b, err := cjson.Encode(*w)
	if err != nil {
		return nil, fmt.Errorf("world: encode: %w", err)
	}
	return b, nil
}

// UnmarshalWorld decodes a .thesis file and PROVES the bytes are exactly the
// canonical encoding of what they decoded to (rule 8).
//
// A non-canonical, CRLF-translated or hand-edited world is therefore rejected
// at the point of use rather than silently participating in a run.
func UnmarshalWorld(data []byte) (*World, error) {
	if len(data) == 0 {
		return nil, errors.New("world: empty file")
	}
	if bytes.IndexByte(data, '\r') >= 0 {
		return nil, ErrWorldCRLF
	}

	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var w World
	if err := dec.Decode(&w); err != nil {
		return nil, fmt.Errorf("world: decode: %w", err)
	}
	var rest json.RawMessage
	if err := dec.Decode(&rest); err == nil {
		return nil, ErrWorldTrailingData
	} else if !errors.Is(err, io.EOF) {
		return nil, ErrWorldTrailingData
	}

	if w.Schema != WorldSchema {
		return nil, fmt.Errorf("world: schema is %q, want %q", w.Schema, WorldSchema)
	}

	got, err := MarshalWorld(&w)
	if err != nil {
		return nil, fmt.Errorf("world: re-encode for canonicality check: %w", err)
	}
	if !bytes.Equal(got, data) {
		off := firstDiff(data, got)
		return nil, &NonCanonicalWorldError{
			Offset: off,
			Want:   byteExcerpt(got, off, 24),
			Got:    byteExcerpt(data, off, 24),
		}
	}
	return &w, nil
}

// firstDiff returns the byte offset of the first difference between a and b,
// or the length of the shorter one when they differ only in length.
func firstDiff(a, b []byte) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}

func byteExcerpt(b []byte, off, n int) string {
	if off < 0 || off >= len(b) {
		return ""
	}
	end := off + n
	if end > len(b) {
		end = len(b)
	}
	return string(b[off:end])
}
