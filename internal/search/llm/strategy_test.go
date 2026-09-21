package llm

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/WrdCstlg/pro-thesis/internal/search"
	"github.com/WrdCstlg/pro-thesis/internal/search/saboteur"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// A valid proposal becomes a validated world: canonicalized planned schedule,
// per-ordinal seed, honest provenance, no realized faults carried from nowhere.
func TestProposeReturnsAValidatedWorld(t *testing.T) {
	srv := newScriptedServer(t, []string{chatCompletion(t, proposalJSON(t, "net.partition(kv-n1)@4000..9000"))})
	opts := testOptions(t, srv.URL)
	s, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	w, err := s.Propose(context.Background(), 0)
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	want, err := schema.CanonicalFault("net.partition(kv-n1)@4000..9000")
	if err != nil {
		t.Fatalf("CanonicalFault: %v", err)
	}
	if len(w.FaultSchedule.Planned) != 1 || w.FaultSchedule.Planned[0] != want {
		t.Errorf("planned = %v, want [%s] (canonicalized, nothing else)", w.FaultSchedule.Planned, want)
	}
	if w.Seed != saboteur.WorldSeedFor(opts.Params.Streams.Seed(), 0) {
		t.Errorf("seed = %d, want %d: the strategy and the executor must derive the same world",
			w.Seed, saboteur.WorldSeedFor(opts.Params.Streams.Seed(), 0))
	}
	if w.Meta == nil || w.Meta.Origin != search.OriginLLM {
		t.Errorf("meta.origin = %+v, want %q", w.Meta, search.OriginLLM)
	}
	if w.FaultSchedule.Realized != nil {
		t.Errorf("realized = %v, want nil: an unexecuted world has no realized schedule", w.FaultSchedule.Realized)
	}
	if srv.callCount() != 1 {
		t.Errorf("the model was called %d times for one valid proposal, want 1", srv.callCount())
	}
	if s.Name() != schema.StrategyLLM {
		t.Errorf("Name() = %q, want %q", s.Name(), schema.StrategyLLM)
	}
}

// A wire-format error is fed back VERBATIM. Paraphrasing the validator's
// words would put a second interpretation of the grammar between the model
// and the code that owns the grammar.
func TestARejectedProposalComesBackInTheNextPromptVerbatim(t *testing.T) {
	bad := "net.latency(kv-n1)@soon..later"
	_, parseErr := schema.ParseFault(bad)
	if parseErr == nil {
		t.Fatalf("the fixture fault %q parsed; the test needs one that does not", bad)
	}

	srv := newScriptedServer(t, []string{
		chatCompletion(t, proposalJSON(t, bad)),
		chatCompletion(t, proposalJSON(t, "net.partition(kv-n1)@4000..9000")),
	})
	s, err := New(testOptions(t, srv.URL))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := s.Propose(context.Background(), 0); err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if srv.callCount() != 2 {
		t.Fatalf("callCount = %d, want 2 (one rejection, one retry)", srv.callCount())
	}
	next := srv.prompt(1)
	if !strings.Contains(next, parseErr.Error()) {
		t.Errorf("the retry prompt does not carry the parser's error verbatim.\nerror: %s\nprompt:\n%s", parseErr, next)
	}
	if !strings.Contains(next, bad) {
		t.Errorf("the retry prompt does not quote the rejected schedule %q", bad)
	}
}

// A schedule that PARSES but violates the perturber policy (clock.skew is on
// the fixture's deny list) is rejected by the same Compile the executor runs,
// and retried, never handed to a world.
func TestAPolicyDeniedKindIsRejectedAndRetried(t *testing.T) {
	denied := "clock.skew(kv-n1, 100)@4000..9000"
	canonical, err := schema.CanonicalFault(denied)
	if err != nil {
		t.Fatalf("the fixture fault must PARSE so the policy is what rejects it: %v", err)
	}
	opts := testOptions(t, "")
	valErr := opts.Params.Validator.Validate([]string{canonical})
	if valErr == nil {
		t.Fatalf("the fixture policy accepted %s; the test needs one it denies", denied)
	}

	srv := newScriptedServer(t, []string{
		chatCompletion(t, proposalJSON(t, denied)),
		chatCompletion(t, proposalJSON(t, "net.partition(kv-n1)@4000..9000")),
	})
	opts.LLM.Endpoint = srv.URL
	opts.Dir = t.TempDir()
	s, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	w, err := s.Propose(context.Background(), 0)
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	for _, f := range w.FaultSchedule.Planned {
		if strings.HasPrefix(f, "clock.skew") {
			t.Errorf("a denied kind reached a world: %v", w.FaultSchedule.Planned)
		}
	}
	if srv.callCount() != 2 {
		t.Fatalf("callCount = %d, want 2", srv.callCount())
	}
	if next := srv.prompt(1); !strings.Contains(next, valErr.Error()) {
		t.Errorf("the retry prompt does not carry the validator's error verbatim.\nerror: %s\nprompt:\n%s", valErr, next)
	}
}

// Retries exhausted is a LOUD error. search.ErrExhausted ends a search
// cleanly, and a model that never produced a legal schedule is not "the space
// is enumerated": folding one into the other would report a broken
// integration as a completed search.
func TestExhaustedRetriesFailLoudlyNotErrExhausted(t *testing.T) {
	srv := newScriptedServer(t, []string{
		chatCompletion(t, proposalJSON(t, "bogus.wire")),
		chatCompletion(t, proposalJSON(t, "still.bogus")),
		chatCompletion(t, proposalJSON(t, "never.valid")),
	})
	opts := testOptions(t, srv.URL)
	opts.LLM.MaxRetries = 1
	s, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = s.Propose(context.Background(), 0)
	if err == nil {
		t.Fatal("two invalid proposals with max_retries 1 produced no error")
	}
	if errors.Is(err, search.ErrExhausted) {
		t.Errorf("the error wraps ErrExhausted (%v): a model failure must not read as a completed enumeration", err)
	}
	if srv.callCount() != 2 {
		t.Errorf("callCount = %d, want 2 (1 + max_retries)", srv.callCount())
	}
}

// The three-valued signal maps to three coachings, and unknown is never
// folded into clean: a world the oracle could not judge must not produce the
// coaching that licenses "these faults did not break the system".
func TestObserveMapsTheThreeSignalsToThreeCoachings(t *testing.T) {
	propose := func(t *testing.T, out search.Outcome, observe bool) string {
		t.Helper()
		srv := newScriptedServer(t, []string{chatCompletion(t, proposalJSON(t, "net.partition(kv-n1)@4000..9000"))})
		s, err := New(testOptions(t, srv.URL))
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if observe {
			if err := s.Observe(context.Background(), out); err != nil {
				t.Fatalf("Observe: %v", err)
			}
		}
		if _, err := s.Propose(context.Background(), 1); err != nil {
			t.Fatalf("Propose: %v", err)
		}
		return srv.prompt(0)
	}

	violated := propose(t, search.Outcome{
		Observed: true,
		Faults:   []string{"net.partition(kv-n2)@3000..8000"},
		Findings: []search.Finding{{Oracle: "no_stale_reads", Status: schema.StatusViolated}},
	}, true)
	if !strings.Contains(violated, "no_stale_reads") {
		t.Errorf("the violated coaching does not name the oracle that fired:\n%s", violated)
	}
	if !strings.Contains(violated, "net.partition(kv-n2)@3000..8000") {
		t.Errorf("the violated coaching does not quote the schedule that produced the violation:\n%s", violated)
	}
	if !strings.Contains(violated, "tighten") {
		t.Errorf("the violated coaching does not direct a re-probe/tightening:\n%s", violated)
	}

	clean := propose(t, search.Outcome{
		Observed: true,
		Findings: []search.Finding{{Oracle: "no_stale_reads", Status: schema.StatusOK}},
	}, true)
	if !strings.Contains(clean, "clean does NOT mean correct") {
		t.Errorf("the clean coaching does not warn that clean is not correctness:\n%s", clean)
	}

	unknown := propose(t, search.Outcome{
		Observed: true,
		Findings: []search.Finding{{Oracle: "no_stale_reads", Status: schema.StatusInconclusive}},
	}, true)
	if !strings.Contains(unknown, "milder") {
		t.Errorf("the unknown coaching does not direct a milder schedule:\n%s", unknown)
	}
	if strings.Contains(unknown, "clean does NOT mean correct") {
		t.Errorf("an UNKNOWN world produced the CLEAN coaching — unknown folded into clean:\n%s", unknown)
	}

	first := propose(t, search.Outcome{}, false)
	if !strings.Contains(first, "first") {
		t.Errorf("the first-world prompt does not say it is the first:\n%s", first)
	}
}

// Every attempt lands in llm-proposals.jsonl, accepted or not. That file is
// the audit trail for a proposal stream that is not seed-reproducible
// (OQ-069); an attempt that left no line is an attempt nobody can review.
func TestEveryAttemptIsRecorded(t *testing.T) {
	first := chatCompletion(t, proposalJSON(t, "bogus.wire"))
	second := chatCompletion(t, proposalJSON(t, "net.partition(kv-n1)@4000..9000"))
	srv := newScriptedServer(t, []string{first, second})
	opts := testOptions(t, srv.URL)
	s, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := s.Propose(context.Background(), 3); err != nil {
		t.Fatalf("Propose: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(opts.Dir, ProposalsFileName))
	if err != nil {
		t.Fatalf("no proposals file: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("%d lines recorded, want 2 (one per attempt):\n%s", len(lines), data)
	}
	type record struct {
		Ordinal        int      `json:"ordinal"`
		Attempt        int      `json:"attempt"`
		Prompt         string   `json:"prompt"`
		Response       string   `json:"response"`
		Schedule       []string `json:"schedule"`
		ValidatorError string   `json:"validator_error"`
		Accepted       bool     `json:"accepted"`
	}
	var recs [2]record
	for i, ln := range lines {
		if err := json.Unmarshal([]byte(ln), &recs[i]); err != nil {
			t.Fatalf("line %d does not decode: %v", i, err)
		}
	}
	if recs[0].Ordinal != 3 || recs[1].Ordinal != 3 {
		t.Errorf("ordinals = %d, %d, want 3, 3", recs[0].Ordinal, recs[1].Ordinal)
	}
	if recs[0].Accepted {
		t.Error("the rejected attempt is recorded as accepted")
	}
	if recs[0].ValidatorError == "" {
		t.Error("the rejected attempt records no validator_error")
	}
	if recs[0].Prompt == "" || recs[0].Response == "" {
		t.Error("the rejected attempt did not record its prompt and response")
	}
	if !recs[1].Accepted {
		t.Error("the accepted attempt is recorded as rejected")
	}
	if recs[1].ValidatorError != "" {
		t.Errorf("the accepted attempt carries validator_error %q", recs[1].ValidatorError)
	}
	if len(recs[1].Schedule) != 1 || recs[1].Schedule[0] != "net.partition(kv-n1)@4000..9000" {
		t.Errorf("the accepted attempt's schedule = %v", recs[1].Schedule)
	}
}

// A negative ordinal is refused before the model is called, matching the
// other strategies.
func TestANegativeOrdinalIsRefused(t *testing.T) {
	srv := newScriptedServer(t, []string{chatCompletion(t, proposalJSON(t, "net.partition(kv-n1)@4000..9000"))})
	s, err := New(testOptions(t, srv.URL))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := s.Propose(context.Background(), -1); err == nil {
		t.Fatal("a negative ordinal was accepted")
	}
	if srv.callCount() != 0 {
		t.Errorf("the model was called %d times for an ordinal that must be refused locally", srv.callCount())
	}
}

// Construction refuses a config it cannot honor rather than failing at the
// first Propose, thirty seconds into a run.
func TestNewRefusesAnUnderspecifiedConfig(t *testing.T) {
	base := testOptions(t, "http://localhost:1/v1/chat/completions")

	bad := *(&base)
	bad.LLM.Endpoint = ""
	if _, err := New(bad); err == nil {
		t.Error("an empty endpoint was accepted")
	}

	bad = *(&base)
	bad.LLM.TimeoutMS = 10
	if _, err := New(bad); err == nil {
		t.Error("a sub-second timeout was accepted")
	}

	bad = *(&base)
	bad.LLM.MaxRetries = -1
	if _, err := New(bad); err == nil {
		t.Error("negative max_retries was accepted")
	}

	bad = *(&base)
	bad.Dir = ""
	if _, err := New(bad); err == nil {
		t.Error("an empty recording dir was accepted: the audit trail is not optional")
	}
}
