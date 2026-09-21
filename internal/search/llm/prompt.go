package llm

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/WrdCstlg/pro-thesis/internal/search"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// ---------------------------------------------------------------------------
// The prompt
//
// The shape is ported from the prompt builder in the author's distadv project
// (not in this repository; D-075): system description, grammar, campaign
// feedback, strict output
// requirements. The grammar section is NOT ported: distadv's 7-primitive DSL
// maps onto nothing in PRO-THESIS's frozen 17-kind registry, so the primer
// here is built PROGRAMMATICALLY from search.Space and the schema registry.
// A hand-copied grammar string would drift from the parser the first time the
// registry moved; this one cannot, because it reads the same declarations
// schema.ParseFault enforces.
// ---------------------------------------------------------------------------

// Feedback is what the last executed world taught the strategy.
type Feedback struct {
	// Seen is false before the first Observe: the first prompt says so rather
	// than coaching from a zero Outcome, which would read as an unknown world
	// that never ran.
	Seen    bool
	Outcome search.Outcome
}

// proposal is the JSON envelope the model answers with. The faults inside are
// PRO-THESIS's own wire format; the envelope exists so extraction can
// tolerate the prose a chat model wraps around its answer.
type proposal struct {
	Schedule  []string `json:"schedule"`
	Reasoning string   `json:"reasoning"`
}

// BuildPrompt renders the user prompt for one Propose call.
func BuildPrompt(p search.Params, fb Feedback) string {
	var b strings.Builder

	b.WriteString("You are the adversary in a fault-injection campaign against a distributed system.\n")
	b.WriteString("Your task: propose ONE fault schedule that maximizes the probability of exposing a ")
	b.WriteString("correctness violation (stale read, lost write, split brain, unlinearizable history).\n")
	b.WriteString("Reason about timing windows and quorums; do not guess randomly.\n\n")

	b.WriteString("== SYSTEM UNDER TEST ==\n")
	fmt.Fprintf(&b, "topology variant: %s\ndriver profile: %s\n",
		p.Base.TopologyVariant, p.Base.DriverProfile)
	fmt.Fprintf(&b, "fault window policy (milliseconds from the start of the workload phase): "+
		"a fault may start no earlier than %d and must end no later than %d; a durative fault's "+
		"length must be within [%d, %d]\n",
		p.Space.Window.EarliestMS, p.Space.Window.LatestMS,
		p.Space.Window.MinDurationMS, p.Space.Window.MaxDurationMS)
	fmt.Fprintf(&b, "a schedule carries at most %d fault(s)\n", p.Space.MaxFaults)

	b.WriteString("\n== FAULT SCHEDULE GRAMMAR ==\n")
	b.WriteString(grammarPrimer(p))

	b.WriteString("\n== CAMPAIGN FEEDBACK ==\n")
	b.WriteString(coaching(fb))

	b.WriteString("\n== OUTPUT REQUIREMENTS (STRICT) ==\n")
	b.WriteString(`Answer with exactly one JSON object and no other text:
  {
    "schedule": ["KIND(target[, params])@start_ms..end_ms", ...],
    "reasoning": "<1-4 sentences of protocol-level reasoning, citing timing values>"
  }
Rules:
  - every element of "schedule" must be a fault in the grammar above, and nothing else;
  - never mix reasoning, labels or prose into a schedule element;
  - an empty schedule is a refusal and is treated as one.
`)
	return b.String()
}

// grammarPrimer renders the permitted slice of the frozen fault grammar from
// the enumerated space: kinds with their parameter contracts, the targets
// legal for each, and the magnitude ladder.
func grammarPrimer(p search.Params) string {
	var b strings.Builder
	b.WriteString("one fault: KIND(TARGET[, PARAMS])@start_ms..end_ms\n")
	b.WriteString("TARGET is a node id, role:leader, role:follower, an edge n1<->n2, " +
		"minority(service), majority(service), any(k, service), or service:* for a whole group.\n")
	b.WriteString("a durative fault is active for the whole window; a non-durative fault fires once " +
		"at start_ms (the window still bounds when its effect is expected).\n\n")
	b.WriteString("permitted kinds (the perturber policy refuses everything else):\n")
	for _, k := range p.Space.Kinds {
		decl, ok := schema.LookupFaultKind(k)
		if !ok {
			continue
		}
		targets := p.Space.Targets[k]
		if len(targets) == 0 {
			continue
		}
		fmt.Fprintf(&b, "  - %s(target", k)
		for _, pd := range decl.Params {
			if pd.Default != "" {
				fmt.Fprintf(&b, ", %s=%s", pd.Name, pd.Default)
			} else {
				fmt.Fprintf(&b, ", %s", pd.Name)
			}
		}
		b.WriteString(")")
		traits := []string{}
		if decl.Durative {
			traits = append(traits, "durative")
		} else {
			traits = append(traits, "fires once at start")
		}
		if decl.AllowEdge {
			traits = append(traits, "edge targets allowed")
		}
		fmt.Fprintf(&b, " [%s] — %s\n", strings.Join(traits, ", "), decl.Doc)
		for _, pd := range decl.Params {
			fmt.Fprintf(&b, "      %s: %s (%s)\n", pd.Name, paramTypeName(pd.Type), pd.Unit)
		}
		names := make([]string, 0, len(targets))
		for _, t := range targets {
			names = append(names, t.String())
		}
		fmt.Fprintf(&b, "      targets: %s\n", strings.Join(names, ", "))
		rungs := p.Space.Rungs(k)
		if len(rungs) == 1 && len(rungs[0]) == 0 {
			b.WriteString("      magnitudes: (no parameters)\n")
		} else {
			rs := make([]string, 0, len(rungs))
			for _, r := range rungs {
				rs = append(rs, "("+strings.Join(r, ", ")+")")
			}
			fmt.Fprintf(&b, "      magnitudes, ascending in aggression: %s\n", strings.Join(rs, ", "))
		}
	}
	return b.String()
}

// paramTypeName renders a parameter type for the prompt.
func paramTypeName(t schema.ParamType) string {
	switch t {
	case schema.ParamNonNegInt:
		return "non-negative integer"
	case schema.ParamInt:
		return "integer"
	case schema.ParamPercent:
		return "percent, 0..100"
	case schema.ParamRate:
		return "fraction, 0..1"
	case schema.ParamSignal:
		return "POSIX signal name"
	default:
		return fmt.Sprintf("type %d", int(t))
	}
}

// coaching renders the feedback branch. Ported in spirit from distadv's
// BuildPrompt feedback switch: the signal is THREE-valued and unknown is
// never folded into clean; a world the oracle could not judge is not
// evidence that the faults were harmless (OQ-033 is why that rule exists).
func coaching(fb Feedback) string {
	if !fb.Seen {
		return "This is the first world of the campaign. Choose a schedule that probes the most " +
			"suspicious timing window in the system description above.\n"
	}
	out := fb.Outcome
	var b strings.Builder
	fmt.Fprintf(&b, "worlds observed so far: the last was ordinal %d\n", out.Ordinal)
	if len(out.Faults) > 0 {
		fmt.Fprintf(&b, "last schedule: %s\n", strings.Join(out.Faults, ", "))
	}
	fmt.Fprintf(&b, "last outcome: %s (%s)\n", out.Signal(), out.Why())
	switch out.Signal() {
	case search.SignalViolated:
		b.WriteString("The oracle PROVED a violation in the last world. The witnesses:\n")
		for _, v := range out.Violations() {
			fmt.Fprintf(&b, "  - %s (class %s, severity %s, phase %s)\n",
				v.Oracle, v.Class, v.Severity, v.Phase)
		}
		b.WriteString("Prioritize re-probing or tightening around this witness: vary the fault duration, " +
			"start time, or target to confirm the violation and sharpen the boundary of the violated class.\n")
	case search.SignalClean:
		b.WriteString("No violation was observed in the last world. NOTE: clean does NOT mean correct — " +
			"it means this one world did not expose one. Move to a different timing window, fault kind, " +
			"or target; do not repeat the schedule that was just cleared.\n")
	default:
		b.WriteString("The last world was incomplete or unevaluable — the oracle could not judge it, " +
			"which is NOT a weak pass. Propose a milder schedule — shorter faults, fewer faults, or a " +
			"later start — so the world reaches an evaluable state.\n")
	}
	return b.String()
}

// rejectionPrompt appends a rejection notice to the base prompt so the model
// can correct its previous answer. Ported from distadv/strategy.go:404.
// schedule holds the rejected fault strings (nil when the response could not
// be parsed at all); reason is the rejection cause, carried VERBATIM; the
// parser and the perturber's compiler own the grammar's wording, and
// paraphrasing them would put a second interpretation between the model and
// the code that decides what is legal.
func rejectionPrompt(base string, attempt int, schedule []string, rawResponse string, reason error) string {
	var b strings.Builder
	b.WriteString(base)
	fmt.Fprintf(&b, "\n== REJECTED ANSWER (attempt %d) ==\n", attempt)
	if len(schedule) > 0 {
		fmt.Fprintf(&b, "Your previous proposal was rejected by the schedule validator:\n  schedule: %s\n  error: %s\n",
			strings.Join(schedule, ", "), reason)
	} else {
		fmt.Fprintf(&b, "Your previous response could not be parsed as a proposal JSON object:\n  response: %s\n  error: %s\n",
			rawResponse, reason)
	}
	b.WriteString("Return a corrected JSON proposal that satisfies every grammar and policy constraint. " +
		"Output JSON only — no other text.\n")
	return b.String()
}

// proposalFromContent extracts a proposal from the model's answer. Ported
// from distadv: it tolerates surrounding prose and markdown code fences, but
// the extracted object must decode into the envelope.
func proposalFromContent(content string) (proposal, error) {
	for _, cand := range extractJSONObjectCandidates(content) {
		var p proposal
		if err := json.Unmarshal([]byte(cand), &p); err == nil {
			return p, nil
		}
	}
	return proposal{}, errors.New("llm: the model's response did not contain a decodable proposal JSON object")
}

// extractJSONObjectCandidates returns candidate JSON object substrings: the
// whole trimmed content, the content with code fences stripped, and the
// outermost {...} span. Ported from distadv.
func extractJSONObjectCandidates(content string) []string {
	c := strings.TrimSpace(content)
	if c == "" {
		return nil
	}
	out := []string{c}
	if s := stripCodeFences(c); s != "" && s != c {
		out = append(out, s)
	}
	if i := strings.IndexByte(c, '{'); i >= 0 {
		if j := strings.LastIndexByte(c, '}'); j > i {
			out = append(out, c[i:j+1])
		}
	}
	return out
}

// stripCodeFences removes ``` / ```json fences around a payload. Ported from
// distadv.
func stripCodeFences(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "```") {
		return s
	}
	if idx := strings.IndexByte(s, '\n'); idx >= 0 {
		s = s[idx+1:]
	} else {
		s = strings.TrimSuffix(s, "```")
		return strings.TrimSpace(s)
	}
	s = strings.TrimSuffix(strings.TrimSpace(s), "```")
	return strings.TrimSpace(s)
}
