// Package llm is the `llm` search strategy: an LLM-proposed adversarial
// fault-schedule stream, served by a local OpenAI-compatible model endpoint.
//
// The retry-with-rejection loop, the three-valued feedback coaching, the
// temperature-0 transport and the rejection prompt are ported from the
// author's distadv project, which is not in this repository (D-075); the
// grammar is ours.
// Proposals are written in PRO-THESIS's own fault wire format and validated
// by the same code path as human-authored schedules (schema.ParseFaults for
// the wire format, then search.Validator, which IS internal/perturber's
// Compile) so the strategy can never emit a world the executor would refuse.
//
// Two guarantees are structural rather than aspirational:
//
//   - Temperature is always sent as 0. There is no knob (D-075).
//   - Every attempt is appended to llm-proposals.jsonl in the run directory,
//     because the proposal stream is not seed-reproducible (OQ-069).
//
// The strategy carries per-ordinal feedback state, so it requires a
// single-worker engine; engine.New enforces that.
package llm

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/WrdCstlg/pro-thesis/internal/search"
	"github.com/WrdCstlg/pro-thesis/internal/search/saboteur"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// Options is everything the llm strategy is built from.
type Options struct {
	// Params is the shared strategy construction: config, space, validator,
	// corpus, streams, base world.
	Params search.Params
	// LLM is search.llm, already defaulted and validated by the schema.
	LLM schema.LLMConfig
	// Dir is the run's artifact directory; the recording lands here. It is
	// mandatory: a strategy whose audit trail is optional will one day run
	// without one.
	Dir string
	// Log receives a line per rejection and per transport failure. Nil
	// discards.
	Log func(format string, args ...any)
}

// Strategy implements search.Strategy over an LLM proposal endpoint.
type Strategy struct {
	p      search.Params
	cfg    schema.LLMConfig
	client *client
	rec    *auditLog
	logf   func(format string, args ...any)

	mu sync.Mutex
	fb Feedback
}

var _ search.Strategy = (*Strategy)(nil)

// New builds the strategy. An underspecified endpoint config is refused here,
// at construction, rather than at the first Propose thirty seconds into a
// run: the schema validator says the same things about a decoded config, and
// this repeats them for a hand-built one.
func New(o Options) (*Strategy, error) {
	if err := o.Params.Validate(); err != nil {
		return nil, err
	}
	switch {
	case o.LLM.Endpoint == "":
		return nil, fmt.Errorf("llm: search.llm.endpoint is empty; the strategy has nothing to ask")
	case o.LLM.MaxRetries < 0:
		return nil, fmt.Errorf("llm: search.llm.max_retries must not be negative, got %d", o.LLM.MaxRetries)
	case o.LLM.TimeoutMS < 1000:
		return nil, fmt.Errorf("llm: search.llm.timeout_ms must be at least 1000, got %d", o.LLM.TimeoutMS)
	case o.Dir == "":
		return nil, fmt.Errorf("llm: Options.Dir is empty; the audit trail (llm-proposals.jsonl) is not optional")
	}
	apiKey := ""
	if o.LLM.APIKeyEnv != "" {
		apiKey = os.Getenv(o.LLM.APIKeyEnv)
	}
	logf := o.Log
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Strategy{
		p:      o.Params,
		cfg:    o.LLM,
		client: newClient(o.LLM.Endpoint, o.LLM.Model, apiKey, time.Duration(o.LLM.TimeoutMS)*time.Millisecond),
		rec:    newAuditLog(o.Dir),
		logf:   logf,
	}, nil
}

// Name implements search.Strategy.
func (s *Strategy) Name() schema.SearchStrategy { return schema.StrategyLLM }

// Propose implements search.Strategy.
//
// Flow, ported from distadv/strategy.go:158-246: build the prompt, call the
// model, extract the proposal, validate it against the wire format and the
// perturber policy. A rejected proposal is retried with the rejection reason
// VERBATIM in the next prompt, up to max_retries times. A transport-level
// failure returns immediately: there is no proposal to reject. When every
// attempt is rejected the error is LOUD and is never search.ErrExhausted:
// ErrExhausted ends a search cleanly as "the space is enumerated", and a
// model that never produced a legal schedule is a broken integration, not a
// completed enumeration.
func (s *Strategy) Propose(ctx context.Context, ordinal int) (schema.World, error) {
	if err := ctx.Err(); err != nil {
		return schema.World{}, err
	}
	if ordinal < 0 {
		return schema.World{}, fmt.Errorf("search: llm: negative world ordinal %d", ordinal)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	base := BuildPrompt(s.p, s.fb)
	prompt := base
	var lastErr error

	for attempt := 1; ; attempt++ {
		if attempt > 1+s.cfg.MaxRetries {
			return schema.World{}, fmt.Errorf("llm: no valid proposal for ordinal %d after %d model "+
				"call(s); last rejection: %w", ordinal, attempt-1, lastErr)
		}
		if err := ctx.Err(); err != nil {
			return schema.World{}, err
		}

		content, err := s.client.chat(ctx, prompt)
		if err != nil {
			// Transport-level failure: there is no proposal to reject, so
			// retrying is not meaningful. Fail fast, but still record the
			// attempt: a model call that died leaves a gap in the audit
			// trail otherwise.
			s.record(proposalRecord{
				Ordinal: ordinal, Attempt: attempt, Prompt: prompt,
				ValidatorError: err.Error(),
			})
			return schema.World{}, fmt.Errorf("llm: model call failed: %w", err)
		}

		prop, perr := proposalFromContent(content)
		if perr != nil {
			lastErr = perr
			s.record(proposalRecord{
				Ordinal: ordinal, Attempt: attempt, Prompt: prompt, Response: content,
				ValidatorError: perr.Error(),
			})
			s.logf("thesis: search: llm: ordinal %d attempt %d rejected: %v", ordinal, attempt, perr)
			prompt = rejectionPrompt(base, attempt, nil, truncateForError(content), perr)
			continue
		}

		canonical, verr := s.validateSchedule(prop.Schedule)
		if verr != nil {
			lastErr = verr
			s.record(proposalRecord{
				Ordinal: ordinal, Attempt: attempt, Prompt: prompt, Response: content,
				Schedule: prop.Schedule, ValidatorError: verr.Error(),
			})
			s.logf("thesis: search: llm: ordinal %d attempt %d rejected: %v", ordinal, attempt, verr)
			prompt = rejectionPrompt(base, attempt, prop.Schedule, "", verr)
			continue
		}

		w, werr := s.world(canonical, ordinal)
		if werr != nil {
			lastErr = werr
			s.record(proposalRecord{
				Ordinal: ordinal, Attempt: attempt, Prompt: prompt, Response: content,
				Schedule: canonical, ValidatorError: werr.Error(),
			})
			s.logf("thesis: search: llm: ordinal %d attempt %d failed world validation: %v", ordinal, attempt, werr)
			prompt = rejectionPrompt(base, attempt, canonical, "", werr)
			continue
		}

		s.record(proposalRecord{
			Ordinal: ordinal, Attempt: attempt, Prompt: prompt, Response: content,
			Schedule: canonical, Accepted: true,
		})
		return w, nil
	}
}

// Observe implements search.Strategy: it stores the outcome, which the next
// prompt's coaching branch reads. The signal is three-valued and unknown is
// never folded into clean: see coaching in prompt.go.
func (s *Strategy) Observe(ctx context.Context, out search.Outcome) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fb = Feedback{Seen: true, Outcome: out}
	return nil
}

// validateSchedule is the rejection predicate: wire format first (the same
// parser human-authored schedules go through, canonicalizing as it reads),
// then the schedule budget, then the perturber's own compiler.
func (s *Strategy) validateSchedule(raw []string) ([]string, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("the proposal carried an empty schedule")
	}
	if len(raw) > s.p.Space.MaxFaults {
		return nil, fmt.Errorf("the proposal carried %d faults; the per-world ceiling is %d",
			len(raw), s.p.Space.MaxFaults)
	}
	specs, err := schema.ParseFaults(raw)
	if err != nil {
		return nil, err
	}
	canonical := make([]string, 0, len(specs))
	for _, f := range specs {
		canonical = append(canonical, f.String())
	}
	if v := s.p.Validator; v != nil {
		if err := v.Validate(canonical); err != nil {
			return nil, err
		}
	}
	return canonical, nil
}

// world builds the proposed world with provenance and a per-ordinal seed,
// following the saboteur's template: the SAME seed derivation the executor
// uses, realized faults and phase timings cleared (they are measurements of a
// run that has not happened), and a final ValidateWorld gate before the world
// leaves the strategy.
func (s *Strategy) world(planned []string, ordinal int) (schema.World, error) {
	w := s.p.Base
	w.Seed = saboteur.WorldSeedFor(s.p.Streams.Seed(), ordinal)
	w.FaultSchedule.Planned = append([]string(nil), planned...)
	w.FaultSchedule.Realized = nil
	w.PhaseTimings = schema.PhaseTimings{}
	w.Meta = &schema.WorldMeta{Origin: search.OriginLLM}
	w = w.Normalized()
	if v := s.p.Validator; v != nil {
		if err := v.ValidateWorld(&w); err != nil {
			return schema.World{}, fmt.Errorf("llm: proposed world failed validation: %w", err)
		}
	}
	return w, nil
}

// record appends one attempt to the audit trail. A recording failure is loud
// on stderr and does not change what the search executes.
func (s *Strategy) record(rec proposalRecord) {
	if err := s.rec.append(rec); err != nil {
		s.logf("thesis: search: llm: WARNING: could not record proposal attempt: %v "+
			"(the audit trail %s is incomplete for this run)", err, ProposalsFileName)
	}
}
