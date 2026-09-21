// Package plan reads the PRO-THESIS driver plan, `prothesis.driver_plan/v1`.
//
// # Why the fixture has its own reader
//
// The plan is a WIRE FORMAT between the harness and an external, unmodified
// driver process. This fixture is a separate Go module and deliberately does not
// import the harness's own packages: a reference driver that shared the
// harness's types would prove nothing about a third-party driver, which is the
// case the format has to work for. So both sides carry their own reader and both
// are pinned against the SAME golden bytes: internal/driver's
// TestOperationPlanGoldenFile encodes them, this package's TestGoldenPlanDecodes
// decodes them. A change on either side without the matching change on the other
// fails one of the two tests.
//
// # The two modes, and why the path is not the discriminator
//
// The harness ALWAYS exports PROTHESIS_PLAN_PATH (DECISIONS.md D-021), and it
// has pointed at a PROFILE plan (clients, ops, mix, no operation trace) on
// every run since Phase 1. A driver that took the variable's existence as "there
// is a trace to replay" would therefore find zero operations, drive nothing, and
// still write a history that a consistency checker calls perfectly clean. That
// is the vacuous pass this project has been bitten by twice.
//
// So HasOperations, not the path, is the discriminator: a plan carrying no
// `operations` member is a profile plan and the driver generates its own
// workload exactly as before.
package plan

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
)

// Schema is the plan document's identifier.
const Schema = "prothesis.driver_plan/v1"

// EnvPath is the environment fallback for the {plan_path} placeholder.
//
// The dual transport is the point (D-021): a driver whose `driver.cmd` does not
// spell {plan_path} still finds the plan, and a driver that knows neither
// ignores both; in which case operation shrinking is reported as NOT ATTEMPTED
// for that target rather than silently producing a wrong minimal repro.
const EnvPath = "PROTHESIS_PLAN_PATH"

// Operation functions this driver understands. The set is NOT part of the
// format; it is this driver's own vocabulary, and an op naming anything else is
// a configuration error rather than something to skip.
const (
	FRead  = "read"
	FWrite = "write"
	FTxn   = "txn"
	FAdmin = "admin"
)

// Read consistency modes.
const (
	ReadLease        = "lease"
	ReadLinearizable = "linearizable"
)

// Plan is a driver plan document.
type Plan struct {
	Schema      string     `json:"schema"`
	Profile     string     `json:"profile"`
	Seed        uint64     `json:"seed"`
	HistoryPath string     `json:"history_path"`
	Clients     int        `json:"clients"`
	Ops         int        `json:"ops"`
	Mix         []MixEntry `json:"mix"`

	// Operations is the explicit trace to replay. Absent means profile mode.
	//
	// When it is present it IS the workload: Clients, Ops and Mix describe the
	// profile the operations were drawn from and are provenance, not
	// instructions.
	Operations []Op `json:"operations"`

	// Targets is the distinct set of client-plane targets the ORIGINAL run
	// addressed, and Op.Target names one of them. It is a POSITION, not an
	// address: see Position.
	Targets []string `json:"targets"`

	// Origin says what Op.AtMS is measured from: "drive" (the harness's DRIVE
	// phase marker) or "first_op".
	Origin string `json:"origin"`
	// OriginNS is the absolute t_ns the offsets were measured from. It is
	// provenance only; a replay's wall clock is its own.
	OriginNS int64 `json:"origin_ns"`
}

// MixEntry is one operation weight, in parts-per-million so the file carries no
// bare float.
type MixEntry struct {
	Op        string `json:"op"`
	WeightPPM int64  `json:"weight_ppm"`
}

// Op is one operation of the trace.
type Op struct {
	// OpID is the history op_id, carried verbatim from the original run. The
	// verdict's witness names op ids, so a replay that renumbered them could
	// not be cross-checked against the violation it is supposed to reproduce.
	OpID int64 `json:"op_id"`
	// Process is the logical client. A process is a sequential thread with one
	// operation in flight; re-assigning operations between processes changes
	// the concurrency structure and therefore which histories are linearizable.
	Process int `json:"process"`
	// F is the operation function.
	F string `json:"f"`
	// Key is the operated-on key.
	Key string `json:"key"`
	// Value is the request value, verbatim from the original invoke record: an
	// integer for a write, the Elle-convention op array for a transaction.
	Value json.RawMessage `json:"value"`
	// ReadMode is the consistency a read asked for. Empty means the default.
	ReadMode string `json:"read_mode"`
	// Target names one of Plan.Targets.
	Target string `json:"target"`
	// AtMS is the offset from Plan.Origin at which the operation was issued.
	AtMS int64 `json:"at_ms"`
	// Pinned marks violation-witness evidence. THIS DRIVER IGNORES IT: it is a
	// fact about the trace for the minimizer that built it, not an instruction
	// to the process executing it.
	Pinned bool `json:"pinned"`
}

// Parse decodes a plan document.
//
// Decoding is tolerant of unknown members (a plan written by a newer harness
// must still be readable by this driver, which is what makes the format
// additive) and strict about everything it does understand.
func Parse(data []byte) (*Plan, error) {
	var p Plan
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("decode plan: %w", err)
	}
	if p.Schema != Schema {
		return nil, fmt.Errorf("plan schema is %q, want %q", p.Schema, Schema)
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return &p, nil
}

// Load reads and decodes a plan file.
func Load(path string) (*Plan, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	p, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return p, nil
}

// HasOperations reports whether this plan carries a trace to replay.
func (p *Plan) HasOperations() bool { return len(p.Operations) > 0 }

// Validate checks the trace's structure.
//
// It says nothing about the profile members: those have been written since
// Phase 1 and rejecting a plan this driver has always accepted would be a
// regression dressed as rigour.
func (p *Plan) Validate() error {
	if p.Operations == nil {
		return nil
	}
	if len(p.Operations) == 0 {
		return fmt.Errorf("the plan carries an EMPTY operation trace; " +
			"absent means \"generate from the profile\", empty would mean \"execute nothing\", " +
			"and a driver that executes nothing still writes a history a checker calls clean")
	}
	targets := map[string]bool{}
	for _, t := range p.Targets {
		targets[t] = true
	}
	var prev int64
	for i, op := range p.Operations {
		switch {
		case op.OpID <= 0:
			return fmt.Errorf("operations[%d]: op_id %d is not positive", i, op.OpID)
		case i > 0 && op.OpID <= prev:
			return fmt.Errorf("operations[%d]: op_id %d does not follow %d; the trace must be "+
				"strictly ascending, which is the original issue order", i, op.OpID, prev)
		}
		prev = op.OpID
		if op.Process < 0 {
			return fmt.Errorf("operations[%d] (op_id %d): process %d is negative", i, op.OpID, op.Process)
		}
		if op.AtMS < 0 {
			return fmt.Errorf("operations[%d] (op_id %d): at_ms %d is negative", i, op.OpID, op.AtMS)
		}
		switch op.F {
		case FRead:
			switch op.ReadMode {
			case "", ReadLease, ReadLinearizable:
			default:
				return fmt.Errorf("operations[%d] (op_id %d): read_mode %q is not %q or %q",
					i, op.OpID, op.ReadMode, ReadLease, ReadLinearizable)
			}
			if op.Key == "" {
				return fmt.Errorf("operations[%d] (op_id %d): a read needs a key", i, op.OpID)
			}
		case FWrite:
			if op.Key == "" {
				return fmt.Errorf("operations[%d] (op_id %d): a write needs a key", i, op.OpID)
			}
			var v int64
			if err := json.Unmarshal(op.Value, &v); err != nil {
				return fmt.Errorf("operations[%d] (op_id %d): a write needs an integer value, got %s",
					i, op.OpID, valueForError(op.Value))
			}
		case FTxn:
			if len(op.Value) == 0 {
				return fmt.Errorf("operations[%d] (op_id %d): a txn needs its operation list", i, op.OpID)
			}
		case FAdmin:
		case "":
			return fmt.Errorf("operations[%d] (op_id %d): no f; there is nothing to execute", i, op.OpID)
		default:
			// Never skip. An operation this driver cannot execute is a plan
			// this driver cannot replay, and running the rest would produce a
			// history that silently differs from the trace the minimizer is
			// reasoning about.
			return fmt.Errorf("operations[%d] (op_id %d): f %q is not one of %s/%s/%s/%s; "+
				"this driver cannot replay this plan and will not replay part of it",
				i, op.OpID, op.F, FRead, FWrite, FTxn, FAdmin)
		}
		if op.Target != "" && !targets[op.Target] {
			return fmt.Errorf("operations[%d] (op_id %d): target %q is not in the plan's target list, "+
				"so it has no position to map onto", i, op.OpID, op.Target)
		}
	}
	return nil
}

func valueForError(v json.RawMessage) string {
	if len(v) == 0 {
		return "nothing"
	}
	if len(v) > 40 {
		return string(v[:40]) + "..."
	}
	return string(v)
}

// Position returns the index of a recorded target in Plan.Targets, or -1.
//
// A target is a POSITION, not an address. Under parallel execution every world
// publishes a different host port (D-043), so an absolute URL recorded in world
// 3 addresses somebody else's cluster on replay, or nothing at all, and a world
// that drove nothing still writes a history that looks clean. The driver
// therefore maps position-for-position onto its OWN target list.
//
// The correspondence is positional because both lists are sorted and both
// enumerate the same cluster's nodes in the same order: PROTHESIS_TARGETS is
// emitted in config node order with ascending ports, which for `host:port`
// strings of equal length is also lexicographic order.
func (p *Plan) Position(recorded string) int {
	for i, t := range p.Targets {
		if t == recorded {
			return i
		}
	}
	return -1
}

// Processes returns the distinct logical clients the trace uses, ascending.
//
// The COUNT is what makes a shrunk trace cheap to run; the IDENTITIES are what
// keep its history comparable with the original, so a trace reduced to two
// processes still reports them as, say, 7 and 11 rather than 0 and 1.
func (p *Plan) Processes() []int {
	seen := map[int]bool{}
	out := []int{}
	for _, op := range p.Operations {
		if !seen[op.Process] {
			seen[op.Process] = true
			out = append(out, op.Process)
		}
	}
	sort.Ints(out)
	return out
}

// ByProcess groups the trace by logical client, each group in plan order.
func (p *Plan) ByProcess() map[int][]Op {
	out := map[int][]Op{}
	for _, op := range p.Operations {
		out[op.Process] = append(out[op.Process], op)
	}
	return out
}

// SpanMS is the offset of the last operation: how long a faithfully paced
// replay runs before its final operation is issued.
func (p *Plan) SpanMS() int64 {
	var max int64
	for _, op := range p.Operations {
		if op.AtMS > max {
			max = op.AtMS
		}
	}
	return max
}
