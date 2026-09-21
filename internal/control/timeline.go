package control

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/WrdCstlg/pro-thesis/internal/recorder"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// Bounds on an assembled causal timeline.
//
// A timeline is read by a human or pasted into an agent's context window, so it
// is a SUMMARY, not an archive. The full evidence stays in the bundle: the
// history, the telemetry and the per-node logs are all on disk and all named by
// the verdict's `artifacts` path.
const (
	// MaxTimelineRows caps the assembled timeline.
	MaxTimelineRows = 64
	// MaxTimelineLogRows caps how many log lines may be quoted.
	MaxTimelineLogRows = 12
	// TimelineLogWindowMS is how far from the finding a log line may be and
	// still be considered relevant.
	TimelineLogWindowMS = 5000
	// maxTimelineDetail truncates one row's detail text.
	maxTimelineDetail = 240
)

// interestingLogRE selects log lines worth quoting in a causal timeline.
//
// This is a HEURISTIC and is labelled as one. It is not a substitute for the
// log-template coverage of Phase 4, and it must never be treated as evidence
// that a line is or is not relevant: it decides only what gets QUOTED. The
// complete log for every node is written to the bundle either way, so a line
// this pattern misses is still one grep away. Logged as OQ-065 (promised as
// "OQ-022" in Phase 1 and never written; see D-067).
var interestingLogRE = regexp.MustCompile(`(?i)` + strings.Join([]string{
	`panic`,
	`fatal`,
	`\boom\b`,
	`out of memory`,
	`segmentation fault`,
	`goroutine \d+ \[`,
	`exit status`,
	`\bterm \d+`,
	`leader`,
	`elected`,
	`election`,
	`partition`,
	`unreachable`,
	`connection refused`,
	`timeout`,
	`timed out`,
	`\berror\b`,
	`unhealthy`,
	`lease`,
}, "|"))

// TimelineSource is the per-world evidence a causal timeline is assembled from.
//
// Directive 4.6's example draws rows from exactly three places (faults, node
// logs and operations) plus the phase structure that makes them legible. This
// type is those four, gathered once per world so that N violations do not
// re-read a 500,000-line history N times.
type TimelineSource struct {
	// Phases are the measured lifecycle windows.
	Phases schema.PhaseTimings
	// Realized is what the Perturber actually injected. Empty in Phase 1; the
	// assembler reads it anyway so Phase 2 adds faults without touching this
	// file.
	Realized []schema.RealizedFault
	// Ops maps an op_id to the history records carrying it.
	Ops map[int64][]schema.HistoryEntry
	// Logs are the collected node logs.
	Logs []CollectedLog
}

// LoadWitnessOps reads the history log and returns the records whose op_id is
// in ids.
//
// It tolerates malformed lines rather than aborting: the history is written by
// an external, user-authored load generator, and one bad line out of half a
// million must not cost the timeline the other 499,999. A read failure IS
// returned, because that means the artifact is missing, which is a different
// and reportable fact.
func LoadWitnessOps(historyPath string, ids []int64) (map[int64][]schema.HistoryEntry, error) {
	if len(ids) == 0 {
		return map[int64][]schema.HistoryEntry{}, nil
	}
	want := make(map[int64]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	f, err := os.Open(historyPath)
	if err != nil {
		return nil, fmt.Errorf("control: read history for causal timeline: %w", err)
	}
	defer f.Close()

	out := make(map[int64][]schema.HistoryEntry, len(ids))
	hr := schema.NewHistoryReader(f)
	for {
		e, err := hr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			var le *schema.HistoryLineError
			if errors.As(err, &le) {
				continue
			}
			return out, fmt.Errorf("control: read history for causal timeline: %w", err)
		}
		if e.OpID == nil || !want[*e.OpID] {
			continue
		}
		out[*e.OpID] = append(out[*e.OpID], *e)
	}
	return out, nil
}

// BuildCausalTimeline assembles the `causal_timeline` of one violation.
//
// Every row's t_ms is in the SAME frame as the phase windows: milliseconds
// relative to DRIVE start. Rows from BOOT and SEED therefore carry negative
// offsets, which is correct and is exactly what schema.PhaseWindow documents.
func BuildCausalTimeline(src TimelineSource, res OracleResult, tl *recorder.Timeline) []schema.TimelineEvent {
	var pinned, filler []schema.TimelineEvent

	// 1. Phase structure. Pinned: without it, an offset is a number with no
	//    meaning, and I5 phase-awareness is the whole reason those offsets are
	//    interesting.
	for _, w := range src.Phases {
		pinned = append(pinned, schema.TimelineEvent{
			TMS:    w.StartMS,
			Event:  schema.TimelinePhase,
			Detail: fmt.Sprintf("%s begins (window %d..%d ms)", w.Phase, w.StartMS, w.EndMS),
		})
	}

	// 2. The finding itself. Pinned.
	//
	// findingMS is the finding's coordinate, and it is used for BOTH the row
	// and the log lines gathered around it below. Placing the row at one offset
	// and the surrounding log excerpt at another would produce a timeline whose
	// two halves disagree about when the finding happened.
	findingMS := oracleRowMS(src.Phases, res)
	pinned = append(pinned, schema.TimelineEvent{
		TMS:    findingMS,
		Event:  schema.TimelineOracle,
		Detail: truncateDetail(oracleDetail(res)),
	})

	// 3. Faults, as actually injected. Pinned: a fault is never noise.
	for _, r := range src.Realized {
		nodes := strings.Join(r.Nodes, ",")
		if nodes == "" {
			nodes = "unresolved"
		}
		pinned = append(pinned, schema.TimelineEvent{
			TMS:    r.StartMS,
			Event:  schema.TimelineFault,
			Detail: truncateDetail(fmt.Sprintf("inject %s -> %s", r.Fault, nodes)),
		})
		pinned = append(pinned, schema.TimelineEvent{
			TMS:    r.EndMS,
			Event:  schema.TimelineFault,
			Detail: truncateDetail(fmt.Sprintf("withdraw %s -> %s", r.Fault, nodes)),
		})
	}

	// 4. The witness operations. Pinned: they ARE the evidence the oracle
	//    named, and dropping one would make the witness unverifiable.
	for _, id := range res.Output.Witness.OpIDs {
		for _, e := range src.Ops[id] {
			ms, ok := virtualMS(tl, e.TNS)
			if !ok {
				continue
			}
			pinned = append(pinned, schema.TimelineEvent{
				TMS:    ms,
				Event:  schema.TimelineOp,
				Detail: truncateDetail(opDetail(e)),
			})
		}
	}

	// 5. Log lines near the finding. Filler: heuristic, and capped.
	filler = append(filler, selectLogRows(src.Logs, findingMS)...)

	out := pinned
	room := MaxTimelineRows - len(out)
	if room > 0 {
		if len(filler) > room {
			filler = filler[:room]
		}
		out = append(out, filler...)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].TMS < out[j].TMS })
	if out == nil {
		out = []schema.TimelineEvent{}
	}
	return out
}

// selectLogRows picks the interesting log lines nearest the finding.
func selectLogRows(logs []CollectedLog, firstSeenMS int64) []schema.TimelineEvent {
	type cand struct {
		ev   schema.TimelineEvent
		dist int64
	}
	var cands []cand
	for _, cl := range logs {
		for _, ln := range cl.Lines {
			if !ln.HasVirtual || !interestingLogRE.MatchString(ln.Text) {
				continue
			}
			d := ln.TMS - firstSeenMS
			if d < 0 {
				d = -d
			}
			if d > TimelineLogWindowMS {
				continue
			}
			cands = append(cands, cand{
				ev: schema.TimelineEvent{
					TMS:    ln.TMS,
					Event:  schema.TimelineLog,
					Node:   cl.Node,
					Detail: truncateDetail(strings.TrimSpace(ln.Text)),
				},
				dist: d,
			})
		}
	}
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].dist < cands[j].dist })
	if len(cands) > MaxTimelineLogRows {
		cands = cands[:MaxTimelineLogRows]
	}
	out := make([]schema.TimelineEvent, 0, len(cands))
	for _, c := range cands {
		out = append(out, c.ev)
	}
	return out
}

// oracleRowMS is where the oracle's own row sits on the causal timeline.
//
// The obvious answer, res.FirstSeenMS, is wrong in one case that the fixture's
// definition-of-done run hits head on. FirstSeenMS is a plain int64, so an
// oracle that supplied NO timing is indistinguishable, at this layer, from one
// that placed its evidence at exactly DRIVE start -- both arrive as 0. Emitting
// the row at 0 for the first kind puts the finding at the head of the timeline,
// before every fault, when the evidence actually lies seconds later.
//
// internal/oracle already refuses to make the matching mistake about the PHASE:
// Result.inPhase pins the observed phase precisely so the engine does not derive
// one "from a number the oracle never gave", and its comment says a fabricated
// position "is worse than none: it is the field a human reads first". This
// function is that same rule applied to the coordinate rather than the label.
//
// The discriminator needs no new field. When a finding carries no timing, the
// process runner pins the phase it was EVALUATED in, and that phase's window
// does not contain 0 -- ASSERT begins long after DRIVE start. When a finding
// genuinely sits at DRIVE start, the engine DERIVES the phase from the offset,
// so the observed phase is the one whose window contains 0. So:
//
//	FirstSeenMS is coherent with ObservedPhase  ->  use it, unchanged
//	it is not, and it is the zero value         ->  the oracle gave no timing;
//	                                                use the observed phase's start
//
// The fallback is a true statement rather than a guess: the finding was reached
// during that phase. The alternative -- dropping the row -- would remove the one
// entry naming the violation from the violation's own timeline.
func oracleRowMS(phases []schema.PhaseWindow, res OracleResult) int64 {
	if res.FirstSeenMS != 0 || res.ObservedPhase.IsUnscoped() {
		return res.FirstSeenMS
	}
	for _, w := range phases {
		if w.Phase != res.ObservedPhase {
			continue
		}
		if w.Contains(0) {
			// The oracle's evidence really is at DRIVE start, or the observed
			// phase spans it. 0 is the honest coordinate.
			return 0
		}
		return w.StartMS
	}
	// The observed phase has no measured window, so there is nothing better to
	// say than what the oracle gave us.
	return res.FirstSeenMS
}

// oracleDetail renders the oracle row.
func oracleDetail(res OracleResult) string {
	name := res.Output.Oracle
	if name == "" {
		name = "oracle"
	}
	class := string(res.Output.Class)
	if class == "" {
		class = "unclassified"
	}
	expl := res.Output.Explanation
	if expl == "" {
		expl = "no explanation supplied"
	}
	where := ""
	if !res.ObservedPhase.IsUnscoped() {
		where = fmt.Sprintf(" in %s", res.ObservedPhase)
	}
	return fmt.Sprintf("%s (%s) reported a violation%s: %s", name, class, where, expl)
}

// opDetail renders one history record the way directive 4.6's example does:
// "client 3 read k/42 -> 7, never written in term 5".
func opDetail(e schema.HistoryEntry) string {
	var b strings.Builder
	if e.Process != nil {
		fmt.Fprintf(&b, "process %d ", *e.Process)
	}
	if e.F != "" {
		b.WriteString(e.F)
	} else {
		b.WriteString("op")
	}
	if e.Key != nil {
		fmt.Fprintf(&b, " %s", *e.Key)
	}
	if len(e.Value) > 0 {
		fmt.Fprintf(&b, " -> %s", compactJSON(e.Value))
	}
	fmt.Fprintf(&b, " [%s", e.Type)
	if e.OpID != nil {
		fmt.Fprintf(&b, " op_id=%d", *e.OpID)
	}
	if e.Error != "" {
		fmt.Fprintf(&b, " error=%s", e.Error)
	}
	b.WriteString("]")
	return b.String()
}

// compactJSON renders a raw JSON value on one line, so a multi-line value
// cannot break the timeline's row-per-event shape.
func compactJSON(raw json.RawMessage) string {
	s := strings.Join(strings.Fields(string(raw)), " ")
	if s == "" {
		return "null"
	}
	return s
}

// virtualMS converts an epoch timestamp to the virtual frame.
func virtualMS(tl *recorder.Timeline, tNS int64) (int64, bool) {
	if tl == nil {
		return 0, false
	}
	ms, err := tl.VMSFromEpochNS(tNS)
	if err != nil {
		return 0, false
	}
	return ms, true
}

// truncateDetail bounds one row's detail text at a rune boundary.
func truncateDetail(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	if len(s) <= maxTimelineDetail {
		return s
	}
	r := []rune(s)
	if len(r) <= maxTimelineDetail {
		return s
	}
	return string(r[:maxTimelineDetail]) + "…"
}
