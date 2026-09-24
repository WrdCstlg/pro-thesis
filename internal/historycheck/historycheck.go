// Package historycheck validates a history log against the contract a driver
// must satisfy, offline, without Docker and without a project.
//
// It exists because the feedback loop for a new target was a whole world: boot
// a cluster, drive it, and learn from an oracle's refusal that the history was
// malformed all along. Every rule here is one the linearizable checker already
// refuses on (cmd/thesis-oracle-linearizable/load.go), moved to where an
// integrator can hit it in a second against a file.
//
// What it cannot do is the part that matters most. Whether a timeout was
// recorded as "info" rather than "fail" is a claim about what the target
// actually did, and no reading of the file can settle it. This package checks
// the shape of a history; only the driver's author can make it true.
package historycheck

import (
	"errors"
	"fmt"
	"io"
	"sort"

	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// Schema names this report's format.
const Schema = "prothesis.history_check/v1"

// Severity decides what a finding does to the exit code.
type Severity string

const (
	// SeverityViolation is a proven breach of the contract, with the offending
	// line as its witness. Any violation makes the file FAIL.
	SeverityViolation Severity = "violation"
	// SeverityInconclusive means the history cannot be judged: there was
	// nothing to check, or what is there is incomplete. It is not a pass, and
	// it is not a proven breach either. Keeping the two apart is the whole
	// point of having an INCONCLUSIVE code at all.
	SeverityInconclusive Severity = "inconclusive"
	// SeverityWarning is a modelling smell that no rule in this system
	// forbids. It is printed and never changes the exit code.
	SeverityWarning Severity = "warning"
)

// Finding codes. They are stable strings rather than positions in a list, so a
// CI grep for one keeps working when another is added.
const (
	CodeMalformedJSON           = "malformed_json"
	CodeInvalidRecord           = "invalid_record"
	CodeNoOpID                  = "no_op_id"
	CodeDuplicateInvoke         = "duplicate_invoke"
	CodeDuplicateCompletion     = "duplicate_completion"
	CodeCompletionWithoutInvoke = "completion_without_invoke"
	CodeUnclosedInvoke          = "unclosed_invoke"
	CodeNoOperations            = "no_operations"
	CodeTimestampNotNanoseconds = "timestamp_not_nanoseconds"
	CodeNegativeInterval        = "negative_interval"
	CodeProcessConcurrent       = "process_concurrent_ops"
)

// The plausible range for a Unix epoch nanosecond timestamp. The low bound is
// 2001-09-09 and the high bound is 2096-10-02: wide enough that no real run
// trips it, narrow enough to catch the two mistakes that actually happen.
// Directive 4.4's own example values were epoch MICROSECONDS (1725300000000000
// is 2024 in microseconds, 1970 in nanoseconds), and a driver that emits
// run-relative nanoseconds lands near zero. Both are silent: they produce a
// history that checks fine and phase windows that line up with nothing.
const (
	minEpochNS int64 = 1_000_000_000_000_000_000
	maxEpochNS int64 = 4_000_000_000_000_000_000
)

// Finding is one thing wrong with the history.
type Finding struct {
	Code     string   `json:"code"`
	Severity Severity `json:"severity"`
	// Line is the 1-based line the finding is about. An operation left open is
	// reported at its invoke, which is the line the author has to change.
	Line    int    `json:"line,omitempty"`
	OpID    *int64 `json:"op_id,omitempty"`
	Message string `json:"message"`
}

// TypeCount is one record type and how many times it appeared. A sorted slice
// rather than a map, so two runs over one file print identical output.
type TypeCount struct {
	Type string `json:"type"`
	N    int    `json:"n"`
}

// Report is everything the check measured, whether or not it found anything.
//
// The counts are not decoration. A reader has to be able to tell a history that
// was checked and found clean from one that was never really looked at, and the
// only way to do that from the outside is to print what was counted.
type Report struct {
	Schema string `json:"schema"`
	Path   string `json:"path"`

	Lines   int `json:"lines"`
	Records int `json:"records"` // operation records
	Markers int `json:"markers"` // harness-written phase and fault markers
	// Operations is the number of distinct op_ids, so a paired invoke and
	// completion count once.
	Operations    int `json:"operations"`
	Indeterminate int `json:"indeterminate"` // "info" completions
	Unclosed      int `json:"unclosed"`      // invokes with no completion
	Processes     int `json:"processes"`
	Keys          int `json:"keys"`

	FirstTNS int64 `json:"first_t_ns"`
	LastTNS  int64 `json:"last_t_ns"`

	Types    []TypeCount `json:"types"`
	Findings []Finding   `json:"findings"`
}

// Violations counts findings that prove a breach of the contract.
func (r *Report) Violations() int { return r.count(SeverityViolation) }

// Warnings counts findings that no rule forbids.
func (r *Report) Warnings() int { return r.count(SeverityWarning) }

func (r *Report) count(s Severity) int {
	n := 0
	for _, f := range r.Findings {
		if f.Severity == s {
			n++
		}
	}
	return n
}

// ExitCode maps the report onto the normative codes.
//
// A proven violation outranks "nothing to check": if the file contains both a
// witnessed breach and no complete operation, the breach is the more useful
// thing to report and the report body still carries both.
func (r *Report) ExitCode() schema.ExitCode {
	if r.Violations() > 0 {
		return schema.ExitFail
	}
	if r.count(SeverityInconclusive) > 0 {
		return schema.ExitInconclusive
	}
	return schema.ExitPass
}

// openOp is an invoke waiting for its completion.
type openOp struct {
	line    int
	tNS     int64
	process *int64
}

// Check reads a history and reports everything wrong with it.
//
// It returns an error only when the stream itself fails. A malformed line is a
// finding, not an error: the reader is built to survive one, and a validator
// that stopped at the first bad line would report one defect in a file with
// fifty.
func Check(r io.Reader, path string) (*Report, error) {
	rep := &Report{Schema: Schema, Path: path}

	hr := schema.NewHistoryReader(r)
	types := map[string]int{}
	processes := map[int64]bool{}
	keys := map[string]bool{}
	// invoked and completed are keyed by op_id and never deleted: pairing has
	// to see a duplicate that arrives long after the operation closed.
	invoked := map[int64]*openOp{}
	completed := map[int64]int{}
	inFlight := map[int64]int64{} // process -> op_id currently open
	var order []int64             // op_ids in first-seen order, for deterministic EOF reporting
	first, last := int64(0), int64(0)
	seenTime := false

	for {
		e, err := hr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		var lineErr *schema.HistoryLineError
		if errors.As(err, &lineErr) {
			rep.Lines++
			rep.add(Finding{
				Code: CodeMalformedJSON, Severity: SeverityViolation, Line: lineErr.Line,
				Message: "line is not valid JSON: " + lineErr.Err.Error(),
			})
			continue
		}
		if err != nil {
			return nil, err
		}

		line := hr.LineNo()
		rep.Lines++
		types[string(e.Type)]++
		if !seenTime {
			first, last, seenTime = e.TNS, e.TNS, true
		}
		if e.TNS < first {
			first = e.TNS
		}
		if e.TNS > last {
			last = e.TNS
		}

		// The union rule first: a record we cannot classify is not evidence of
		// anything, so nothing downstream should try to interpret it.
		if verr := e.Validate(); verr != nil {
			rep.add(Finding{
				Code: CodeInvalidRecord, Severity: SeverityViolation, Line: line,
				Message: verr.Error(),
			})
			continue
		}

		if e.TNS < minEpochNS || e.TNS > maxEpochNS {
			rep.add(Finding{
				Code: CodeTimestampNotNanoseconds, Severity: SeverityViolation, Line: line,
				Message: "t_ns is not a plausible Unix epoch nanosecond timestamp. " +
					"The field name is normative and says nanoseconds; emit time.Now().UnixNano(). " +
					"Microseconds and run-relative times both land here",
			})
		}

		if e.RecordKind() == schema.RecordMarker {
			rep.Markers++
			continue
		}
		rep.Records++
		if e.Process != nil {
			processes[*e.Process] = true
		}
		if e.Key != nil {
			keys[*e.Key] = true
		}
		if e.Type == schema.HistoryInfo {
			rep.Indeterminate++
		}

		if e.OpID == nil {
			rep.add(Finding{
				Code: CodeNoOpID, Severity: SeverityViolation, Line: line,
				Message: "operation record carries no op_id. The checker pairs invokes with " +
					"completions by op_id and reports op_ids as its witness; it will not guess a " +
					"pairing from the process field",
			})
			continue
		}
		id := *e.OpID

		switch e.Type {
		case schema.HistoryInvoke:
			if prior, dup := invoked[id]; dup {
				rep.add(Finding{
					Code: CodeDuplicateInvoke, Severity: SeverityViolation, Line: line, OpID: &id,
					Message: fmt.Sprintf("op_id %d was already invoked on line %d; an op_id that names two "+
						"operations makes every interval in the history ambiguous", id, prior.line),
				})
				continue
			}
			if e.Process != nil {
				if open, busy := inFlight[*e.Process]; busy {
					rep.add(Finding{
						Code: CodeProcessConcurrent, Severity: SeverityWarning, Line: line, OpID: &id,
						Message: fmt.Sprintf("process %d invoked op_id %d while op_id %d was still in "+
							"flight. A process is a logical client, so this history describes more "+
							"concurrency than its process count admits. The checker pairs by op_id "+
							"and does not refuse this", *e.Process, id, open),
					})
				} else {
					inFlight[*e.Process] = id
				}
			}
			invoked[id] = &openOp{line: line, tNS: e.TNS, process: e.Process}
			order = append(order, id)

		case schema.HistoryOK, schema.HistoryFail, schema.HistoryInfo:
			inv, ok := invoked[id]
			if !ok {
				rep.add(Finding{
					Code: CodeCompletionWithoutInvoke, Severity: SeverityViolation, Line: line, OpID: &id,
					Message: fmt.Sprintf("op_id %d completes but was never invoked; an operation with no "+
						"invoke has no interval and cannot be placed in any order", id),
				})
				continue
			}
			if prior, dup := completed[id]; dup {
				rep.add(Finding{
					Code: CodeDuplicateCompletion, Severity: SeverityViolation, Line: line, OpID: &id,
					Message: fmt.Sprintf("op_id %d was already completed on line %d", id, prior),
				})
				continue
			}
			completed[id] = line
			if e.TNS < inv.tNS {
				rep.add(Finding{
					Code: CodeNegativeInterval, Severity: SeverityViolation, Line: line, OpID: &id,
					Message: fmt.Sprintf("op_id %d completes at t_ns %d, before it was invoked at %d on "+
						"line %d; no linearization can place an interval that ends before it begins",
						id, e.TNS, inv.tNS, inv.line),
				})
			}
			if inv.process != nil && inFlight[*inv.process] == id {
				delete(inFlight, *inv.process)
			}
		}
	}

	for _, id := range order {
		if _, done := completed[id]; done {
			continue
		}
		inv := invoked[id]
		rep.Unclosed++
		// INCONCLUSIVE, not a violation. An operation with no completion does
		// not prove the file is wrong; it proves the history stops before the
		// answer, which is exactly what no_stuck_op refuses over. Measured on
		// the two committed corpora: of 985 recorded histories, the 61 with an
		// unclosed operation include 50 whose world never wrote a result.json
		// at all, and NONE that was judged pass. Calling those FAIL would
		// collapse a refusal into a verdict.
		rep.add(Finding{
			Code: CodeUnclosedInvoke, Severity: SeverityInconclusive, Line: inv.line, OpID: &id,
			Message: fmt.Sprintf("op_id %d was invoked and never completed. If the driver was asked to "+
				"drain, it did not finish this operation before exiting; if the world was cut "+
				"short, the history is incomplete. Either way no oracle can judge it: this is the "+
				"shape no_stuck_op refuses", id),
		})
	}

	if rep.Records == 0 {
		rep.add(Finding{
			Code: CodeNoOperations, Severity: SeverityInconclusive,
			Message: fmt.Sprintf("no operation records (%d marker record(s), %d line(s)). An empty "+
				"history is INCONCLUSIVE, never ok: a checker handed nothing has checked nothing",
				rep.Markers, rep.Lines),
		})
	}

	rep.Operations = len(order) + distinctOrphans(completed, invoked)
	rep.Processes = len(processes)
	rep.Keys = len(keys)
	rep.FirstTNS, rep.LastTNS = first, last
	rep.Types = sortedTypes(types)
	sort.SliceStable(rep.Findings, func(i, j int) bool { return rep.Findings[i].Line < rep.Findings[j].Line })
	return rep, nil
}

// distinctOrphans counts completions whose op_id was never invoked, so
// Operations reports every operation the file mentions rather than only the
// well-formed ones.
func distinctOrphans(completed map[int64]int, invoked map[int64]*openOp) int {
	n := 0
	for id := range completed {
		if _, ok := invoked[id]; !ok {
			n++
		}
	}
	return n
}

func sortedTypes(m map[string]int) []TypeCount {
	out := make([]TypeCount, 0, len(m))
	for k, v := range m {
		out = append(out, TypeCount{Type: k, N: v})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Type < out[j].Type })
	return out
}

func (r *Report) add(f Finding) { r.Findings = append(r.Findings, f) }
