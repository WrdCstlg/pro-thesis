package driver

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// PlanSchema is the id of the file {plan_path} names.
//
// ADDITIVE. The directive names no schema for it, because it names no
// {plan_path} either: both are reserved by DECISIONS.md D-021 to close the gap
// OQ-012 identified.
const PlanSchema = "prothesis.driver_plan/v1"

// PlanFileName is the conventional name of the plan inside a world bundle.
const PlanFileName = "plan.json"

// Plan is the resolved driver profile, written to {plan_path} and pointed at by
// PROTHESIS_PLAN_PATH.
//
// It exists because `driver.profiles` never reaches the driver through the
// frozen command template: {profile} interpolates the profile NAME, so a driver
// has no way to learn `clients: 16, ops: 20000, mix: {read: 0.4, ...}` unless it
// independently parses prothesis.yaml, which a portable third-party driver
// cannot be assumed to do. See OPEN_QUESTIONS.md OQ-012 and DECISIONS.md D-021.
//
// # Two modes, one file
//
// PROFILE MODE: `operations` absent. This is the Phase 1 shape: the plan
// describes the workload's PARAMETERS and the driver generates operations from
// them and from {seed}. Every plan written before Phase 5 is of this kind and
// its bytes are unchanged by the additive members below.
//
// OPERATION MODE: `operations` present. The plan carries the EXPLICIT operation
// trace the driver must execute, in order, with the recorded client assignment
// and the recorded op ids. This is what Phase 5's second minimization stage
// needs and what no frozen placeholder can express: {seed} regenerates the whole
// pseudo-random stream and {profile} passes only a name (OQ-012, D-021).
//
// A driver that ignores the plan entirely is fully supported; op shrinking is
// then reported as NOT ATTEMPTED for that target rather than producing a wrong
// minimal repro. VerifyReplay is how the caller tells the two apart from the
// outside, without trusting the driver's word for it.
//
// # Is the plan hashed?
//
// NO. The plan is not part of `world_hash` and is deliberately NOT encoded
// through internal/recorder/cjson. Three reasons, in order of weight:
//
//  1. It is a wire format for a FOREIGN process. A third-party driver may read
//     it with `jq` or a shell, so the indented, human-legible encoding is the
//     feature; cjson's whitespace-free canonical form would be a readability
//     regression on the one file whose whole purpose is to be consumed by
//     something that is not this program.
//  2. Re-encoding it through cjson would move the bytes of every plan Phase 1
//     already writes, for no gain.
//  3. `value` is carried as opaque raw JSON (see PlanOp.Value), which cjson's
//     type-graph rules deliberately forbid.
//
// The plan is nonetheless CANONICAL BY CONSTRUCTION (fixed field order, no
// maps, no bare floats, LF only, exactly one trailing newline) so Marshal is a
// bijection on the language ParsePlan accepts, pinned by
// TestAnOperationPlanRoundTripsByteIdentically. Fingerprint gives a stable
// identity for anyone who needs one (ddmin candidate de-duplication); it is not
// a world hash and must never be used as one.
type Plan struct {
	Schema string `json:"schema"`
	// Profile is the driver profile name, the same value {profile} carries.
	Profile string `json:"profile"`
	// Seed is the world seed, in the same unsigned decimal form {seed} carries.
	Seed uint64 `json:"seed"`
	// HistoryPath is where the driver must write its op records. Repeated here
	// so a driver reading only the plan has everything it needs.
	HistoryPath string `json:"history_path"`

	// Clients and Ops are the resolved profile's values. Zero means the profile
	// did not set them; the driver keeps its own default.
	Clients int `json:"clients"`
	Ops     int `json:"ops"`

	// Mix is the operation mix, as a sorted list rather than a map so the file
	// is byte-stable across runs. The directive's samples sum to 1.0, but
	// nothing declares it a probability distribution, so relative weights are
	// carried through unvalidated: exactly as pkg/schema does.
	Mix []MixEntry `json:"mix"`

	// ------------------------------------------------------------------
	// ADDITIVE, Phase 5. Everything below is absent from a profile plan, so
	// a plan written before this existed encodes to the same bytes it did.
	// New members are appended, never interleaved, for exactly that reason:
	// encoding/json emits struct fields in declaration order.
	// ------------------------------------------------------------------

	// Operations is the explicit operation trace, ascending by op id.
	//
	// Absent (nil) means PROFILE MODE: there is no trace and the driver
	// generates its own workload. Present means OPERATION MODE. An EMPTY but
	// present list is rejected by Validate rather than encoded, because
	// "execute nothing" is a driver that drives nothing while still writing a
	// history a checker will happily call clean: the vacuous pass this
	// project has been bitten by twice.
	//
	// When it is present, Clients, Ops and Mix describe the profile the
	// operations were DRAWN FROM. They are provenance, not instructions: the
	// trace is the workload.
	Operations []PlanOp `json:"operations,omitempty"`

	// Targets is the sorted, distinct set of client-plane targets the original
	// operations addressed, and PlanOp.Target names one of them.
	//
	// It is carried, rather than each op carrying an absolute URL the driver
	// uses verbatim, because a target is WORLD-LOCAL: under parallel execution
	// every world publishes a different host port (D-043), so an absolute URL
	// recorded in world 3 addresses somebody else's cluster on replay. The
	// driver maps position-for-position onto its OWN target list instead.
	//
	// Subsetting NEVER recomputes this list. Recomputing it after ddmin
	// dropped every op that addressed one node would renumber the positions
	// and silently re-point the surviving operations at different nodes,
	// which is a shrink that changes the bug, the ranked #1 failure of this
	// phase, arriving through a tidy-up.
	Targets []string `json:"targets,omitempty"`

	// Origin says what PlanOp.AtMS is measured from: OriginDrive when the
	// history carried a DRIVE phase marker, OriginFirstOp otherwise.
	Origin string `json:"origin,omitempty"`

	// OriginNS is the absolute t_ns the offsets were measured from, kept for
	// provenance so a human can line a plan up against the original history.
	// It is never used to schedule anything: a replay's wall clock is its own.
	OriginNS int64 `json:"origin_ns,omitempty"`
}

// MixEntry is one operation weight.
type MixEntry struct {
	Op string `json:"op"`
	// Weight is parts-per-million of the whole mix, so the file carries no bare
	// float. `read: 0.4` becomes 400000. Integer because a plan may end up
	// hashed by a later phase, and Go's shortest-float representation carries no
	// cross-version stability guarantee.
	WeightPPM int64 `json:"weight_ppm"`
}

// NewPlan resolves a driver profile into a Plan.
//
// An unknown profile name is an error rather than an empty plan: `driver_profile
// : gate` naming a profile that does not exist is a config mistake, and running
// the driver with no workload while reporting PASS would be the worst possible
// response to it.
func NewPlan(cfg *schema.Config, profile, historyPath string, seed uint64) (*Plan, error) {
	p := &Plan{
		Schema:      PlanSchema,
		Profile:     profile,
		Seed:        seed,
		HistoryPath: historyPath,
		Mix:         []MixEntry{},
	}
	if cfg == nil {
		return p, nil
	}
	dp, ok := cfg.Driver.Profiles[profile]
	if !ok {
		return nil, fmt.Errorf("driver: driver.profiles has no entry %q (declared profiles: %v)",
			profile, sortedKeys(cfg.Driver.Profiles))
	}
	p.Clients = dp.Clients
	p.Ops = dp.Ops
	for op, w := range dp.Mix {
		p.Mix = append(p.Mix, MixEntry{Op: op, WeightPPM: ppm(w)})
	}
	sort.Slice(p.Mix, func(i, j int) bool { return p.Mix[i].Op < p.Mix[j].Op })
	return p, nil
}

// ppm converts a mix weight to parts-per-million, rounding half away from zero.
func ppm(w float64) int64 {
	v := w * 1e6
	if v >= 0 {
		v += 0.5
	} else {
		v -= 0.5
	}
	return int64(v)
}

func sortedKeys(m map[string]schema.DriverProfile) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Marshal encodes the plan, indented, with a trailing newline. A driver may be
// a shell script reading it with `jq`; readability costs nothing at this size.
func (p *Plan) Marshal() ([]byte, error) {
	p.Schema = PlanSchema
	if p.Mix == nil {
		p.Mix = []MixEntry{}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(p); err != nil {
		return nil, fmt.Errorf("driver: encode plan: %w", err)
	}
	return buf.Bytes(), nil
}

// ParsePlan decodes a plan file.
//
// Decoding is TOLERANT of unknown members (a plan written by a newer build
// must still be readable by an older driver, which is the whole point of an
// additive format) and STRICT about the operation trace: a malformed trace is
// rejected here rather than becoming a workload nobody asked for. See
// Plan.Validate.
func ParsePlan(data []byte) (*Plan, error) {
	var p Plan
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("driver: decode plan: %w", err)
	}
	if p.Schema != PlanSchema {
		return nil, fmt.Errorf("driver: plan schema is %q, want %q", p.Schema, PlanSchema)
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return &p, nil
}

// WritePlan writes the plan atomically and returns its path.
//
// Atomic because the driver reads this file as it starts: a driver that opened
// a half-written plan would run a workload nobody asked for, and the verdict
// would be about that workload.
func WritePlan(path string, p *Plan) error {
	if err := p.Validate(); err != nil {
		return err
	}
	data, err := p.Marshal()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("driver: create %s: %w", filepath.Dir(path), err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("driver: write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("driver: rename %s -> %s: %w", tmp, path, err)
	}
	return nil
}
