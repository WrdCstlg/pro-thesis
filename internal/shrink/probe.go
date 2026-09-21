package shrink

import (
	"context"
	"fmt"
	"sort"
	"sync"
)

// ---------------------------------------------------------------------------
// RULE 2: the confirmation asymmetry, applied to ACCEPTANCE
//
// "Under Tier B a candidate that genuinely still reproduces can fail once by
// chance, and ddmin would then keep a fault that does not matter."
//
// The prober is where that is answered, and the answer is asymmetric because the
// evidence is:
//
//	ONE reproduction is a PROOF. The candidate demonstrably still fails; no
//	number of further executions can unprove it. AcceptTrials = 1.
//
//	ONE non-reproduction is nearly no evidence at all. Under a probabilistic
//	replay the same candidate may reproduce on the next attempt. RejectTrials = 2.
//
//	An UNKNOWN execution is neither. A world whose topology failed to boot says
//	nothing about whether a fault was necessary, and counting it as a
//	non-reproduction is precisely how ddmin keeps the wrong faults.
//
// So the retries are spent on the rejection branch, which is where a wrong
// answer costs something, and the acceptance branch (the branch a successful
// shrink takes over and over) stays at one world.
// ---------------------------------------------------------------------------

// decision is the prober's terminal judgement of one candidate.
type decision struct {
	// Signal is the judgement. SignalUnknown means it could not be judged, which
	// is NOT a rejection.
	Signal Signal
	// Trials is how many worlds were executed to reach it.
	Trials int
	// Reproduced is how many of those reproduced the original violation.
	Reproduced int
	// Reason is why, for the report.
	Reason string
	// WorldPath is the last .thesis the executor named for this candidate.
	WorldPath string
	// Truncated is true when the budget cut the evaluation short, so the
	// judgement is provisional and must not be cached or trusted as a rejection.
	Truncated bool
}

// reproduced reports whether the candidate may be accepted as a reduction.
func (d decision) accepted() bool { return d.Signal == SignalReproduced }

// divergence is one non-matching violation and WHERE it was seen.
//
// Provenance is the whole of D-063's bound. A divergence seen on a REDUCED
// candidate may raise the question of witness-key drift; only one seen on an
// execution of the UNREDUCED world may answer it. The two are stored together so
// the report can still list every divergent violation, and kept distinct so the
// calibration decision can consult only the kind that is evidence about the
// target rather than about a reduction.
type divergence struct {
	id Identity
	// unreduced is true when the world that produced this identity was the one
	// with nothing removed: stage 0's executions, or a re-probe that a reduced
	// candidate's drift triggered.
	unreduced bool
}

// prober evaluates candidates under the confirmation policy and the budget.
type prober struct {
	exec  Executor
	batch BatchExecutor
	orig  Identity
	pol   Policy
	led   *ledger
	log   func(string)

	mu        sync.Mutex
	cache     map[string]decision
	divergent map[string]divergence
	attempts  int

	// unreduced is the world with NOTHING removed, kept so that a drift a reduced
	// candidate raises can be checked against it. Nil when the caller skipped the
	// baseline: and then no calibration ever happens, because the run never
	// executes the one world whose behaviour is evidence about the target.
	unreduced *Candidate
	// reprobes counts re-executions of the unreduced world made to answer a
	// drift question, against Policy.CalibrationProbes.
	reprobes int

	// mayCalibrate is set when the identity level was a DEFAULT rather than a
	// caller's instruction, and cleared before the confirmation gate. calibNote
	// records the one calibration a run may make.
	mayCalibrate bool
	calibNote    string
}

// calibrationNote is the single sentence every calibration is reported by. It
// exists once so the stage note, the Result field and the CLI cannot drift apart
// and describe the same demotion three different ways.
func calibrationNote(orig, drift Identity) string {
	return fmt.Sprintf(
		"the same defect reproduced as %s (%s) on witness key %q, not the target's %q, so the key "+
			"is not stable for this target; identity matching was calibrated down from %s to %s "+
			"(the floor) for the rest of the run",
		drift.Oracle, drift.Class, drift.Key, orig.Key, MatchWitnessKey, MatchOracleClass)
}

func newProber(exec Executor, orig Identity, pol Policy, led *ledger, log func(string)) *prober {
	p := &prober{
		exec:      exec,
		orig:      orig,
		pol:       pol,
		led:       led,
		log:       log,
		cache:     map[string]decision{},
		divergent: map[string]divergence{},
	}
	if b, ok := exec.(BatchExecutor); ok {
		p.batch = b
	}
	return p
}

// parallelism is how many candidates may be offered to the executor at once.
// Without a BatchExecutor the answer is always one, whatever the policy asked
// for: pretending otherwise would report a concurrency that never happened.
func (p *prober) parallelism() int {
	if p.batch == nil {
		return 1
	}
	return p.pol.Parallelism
}

func (p *prober) logf(format string, args ...any) {
	if p.log == nil {
		return
	}
	p.log(fmt.Sprintf(format, args...))
}

// divergences returns the distinct non-matching violations seen so far, sorted.
// They are what makes a switched bug visible in the report instead of vanishing
// into a rejection count. Provenance is not reported here: a divergence is a
// divergence in the report whichever world produced it.
func (p *prober) divergences() []Identity {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]Identity, 0, len(p.divergent))
	for _, d := range p.divergent {
		out = append(out, d.id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].key() < out[j].key() })
	return out
}

// noteDivergence records non-matching identities with their provenance.
//
// fromUnreduced says the world that produced them had nothing removed. An
// identity first seen on a reduced candidate and later on the unreduced world is
// UPGRADED, never the reverse: unreduced evidence is the stronger kind, and a
// later weaker sighting must not erase it.
func (p *prober) noteDivergence(ids []Identity, fromUnreduced bool) {
	if len(ids) == 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, id := range ids {
		k := id.key()
		if prev, seen := p.divergent[k]; seen {
			if fromUnreduced && !prev.unreduced {
				p.divergent[k] = divergence{id: prev.id, unreduced: true}
			}
			continue
		}
		p.divergent[k] = divergence{id: id, unreduced: fromUnreduced}
	}
}

// relaxTo lowers the identity-matching level and undoes every judgement that was
// made under the stricter one.
//
// Both halves are required. Leaving the memo table behind would let candidates
// rejected under the old rule stay rejected forever (the cache is keyed by
// candidate, not by the rule the candidate was judged under) and leaving the
// divergence set behind would report violations as "a different bug" in the very
// report that explains they are not.
//
// It only ever widens: a HIGHER Strictness demands more, so relaxing moves the
// level DOWN and a request to move it up is ignored rather than honoured. Its
// callers are in-package: stage 0's baseline, and maybeCalibrate, which under
// D-063 acts only on evidence from an execution of the unreduced world.
func (p *prober) relaxTo(s Strictness) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if s >= p.pol.Strictness {
		return
	}
	p.pol.Strictness = s
	p.cache = map[string]decision{}
	for k, d := range p.divergent {
		if ok, _ := s.Match(p.orig, d.id); ok {
			delete(p.divergent, k)
		}
	}
}

// strictness is the level currently in force, which is the requested one until a
// calibration lowers it.
func (p *prober) strictness() Strictness {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.pol.Strictness
}

// setUnreduced tells the prober which candidate is the world with nothing
// removed. Only executions of THIS candidate may answer a drift question.
func (p *prober) setUnreduced(c Candidate) {
	p.mu.Lock()
	defer p.mu.Unlock()
	cc := c
	p.unreduced = &cc
}

// isUnreduced reports whether c is the unreduced world, so a divergence it
// produced is recorded with the provenance that lets it calibrate.
func (p *prober) isUnreduced(c Candidate) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.unreduced != nil && p.unreduced.key() == c.key()
}

// driftFrom reports an identity that matches the original on oracle AND class
// but carries a different witness key, drawn only from divergences with the
// given provenance, or nil when none was seen.
func (p *prober) driftFrom(unreduced bool) *Identity {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.orig.Key == "" {
		return nil
	}
	for _, d := range p.divergent {
		if d.unreduced != unreduced {
			continue
		}
		if d.id.Key == "" || d.id.Key == p.orig.Key {
			continue
		}
		if ok, _ := MatchOracleClass.Match(p.orig, d.id); ok {
			got := d.id
			return &got
		}
	}
	return nil
}

// reducedDrift is the QUESTION: a reduced candidate fired the target's oracle
// and class on another key. It is a prompt to measure, and never evidence:
// nothing about a reduced world says whether the key is part of the target's
// identity or an artifact of what the reduction left in flight.
func (p *prober) reducedDrift() *Identity { return p.driftFrom(false) }

// answerDriftQuestion is D-063's bound.
//
// When a REDUCED candidate has shown the target's oracle and class on another
// key, and calibration is still open, it re-executes the UNREDUCED world once
// (at most Policy.CalibrationProbes times per run) so that the evidence which
// decides the level is a fact about the target rather than something a
// reduction manufactured. Only that execution's divergences carry unreduced
// provenance, and only those can satisfy keyDrift.
//
// The attack this closes: a candidate that DROPS the essential fault while a
// second mechanism fires the same oracle on another key. Before the bound, its
// divergence relaxed the level in its own round, it was accepted, and stage 4
// certified the wrong bug k/k. Now it can cost the run a re-probe, and if the
// unreduced world holds its key the candidate stays rejected. If the unreduced
// world drifts, the relaxation is one stage 0 would have made had its two draws
// been luckier, which is the whole point.
//
// A budget stop during the re-probe leaves the level strict. That is the safe
// direction; the stop is returned so the caller can report it.
func (p *prober) answerDriftQuestion(ctx context.Context) StopReason {
	p.mu.Lock()
	limit := p.pol.CalibrationProbes
	open := p.mayCalibrate && p.calibNote == "" && p.orig.Key != "" &&
		p.pol.Strictness == MatchWitnessKey && p.unreduced != nil &&
		p.reprobes < limit
	unreduced := p.unreduced
	p.mu.Unlock()
	if !open || p.reducedDrift() == nil || p.keyDrift() != nil {
		return StopComplete
	}
	p.mu.Lock()
	p.reprobes++
	n := p.reprobes
	p.mu.Unlock()
	p.logf("a reduced candidate raised witness-key drift; re-executing the unreduced world to "+
		"answer it (%d of at most %d)", n, limit)
	_, stop := p.probeOnce(ctx, *unreduced)
	return stop
}

// policy returns the policy currently in force. Only Strictness ever changes
// (calibration); the trial counts are fixed for the life of the run.
func (p *prober) policy() Policy {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.pol
}

// allowCalibration says whether this run's identity level may still be calibrated
// against measured witness-key drift. See maybeCalibrate.
func (p *prober) allowCalibration(ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.mayCalibrate = ok
}

// calibration returns the note describing a calibration that happened, or "".
func (p *prober) calibration() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calibNote
}

// recordCalibration stores the note for a calibration the caller performed, so a
// run has exactly one place a reader looks for it whichever stage triggered it.
// The FIRST note wins: the level moves once, so the reason it moved is the reason
// it moved the first time.
func (p *prober) recordCalibration(note string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.calibNote == "" {
		p.calibNote = note
	}
}

// maybeCalibrate lowers the identity level if an execution of the UNREDUCED
// world has shown this target's witness key moving between executions.
//
// Stage 0 asks the question deliberately, with two executions of the unreduced
// world. That is the cheapest place to ask it, and it is not sufficient, which
// was measured rather than reasoned. Run r_2026_09_09_9d07 drew k/0 twice,
// concluded the key was stable, and then watched the SAME defect land on k/1,
// k/2, k/3 and k/4 over the following 60 worlds. k/0 holds roughly three
// quarters of the time on this fixture, so two draws agree more often than not
// while proving nothing.
//
// So the whole run may ASK. Any reduced candidate that fires this target's
// oracle AND class on a different key is a prompt: stage 0's question, raised
// again for free by a world that was being spent anyway. But only the unreduced
// world may ANSWER (D-063): answerDriftQuestion re-executes it, and this
// function consults only the divergences that such executions produced. The
// distinction is not pedantry. A reduced candidate that has dropped the
// essential fault, with a second mechanism firing the same oracle elsewhere, is
// indistinguishable from legitimate drift AT THE CANDIDATE, and perfectly
// distinguishable at the unreduced world, whose behaviour no reduction controls.
//
// The guardrails, restated with the bound:
//
//   - Only a DEFAULT level is ever calibrated (mayCalibrate).
//   - Only down to MatchOracleClass, the directive's floor, one step, one way.
//   - The evidence must match on ORACLE and CLASS. A candidate that fired a
//     different oracle or a different class is a divergence and stays one, so a
//     genuinely switched bug cannot trigger this.
//   - The evidence must come from the UNREDUCED world. A reduced candidate can
//     trigger at most Policy.CalibrationProbes re-executions of it, and cannot
//     move the level itself.
//   - Not during confirmation. The pipeline freezes calibration before stage 4,
//     because a gate that relaxes itself while being applied is
//     indistinguishable from a gate being gamed: however good its reasons. The
//     level confirmation runs at is the level the rest of the run settled on,
//     decided before the gate opened.
func (p *prober) maybeCalibrate() {
	p.mu.Lock()
	if !p.mayCalibrate || p.calibNote != "" || p.orig.Key == "" ||
		p.pol.Strictness != MatchWitnessKey {
		p.mu.Unlock()
		return
	}
	p.mu.Unlock()

	drift := p.keyDrift()
	if drift == nil {
		return
	}
	note := calibrationNote(p.orig, *drift)
	p.relaxTo(MatchOracleClass)
	p.recordCalibration(note)
	p.logf("%s", note)
}

// keyDrift reports an identity that matches the original on oracle AND class but
// carries a different witness key, drawn ONLY from executions of the unreduced
// world, or nil when none was seen.
//
// That restriction is what makes it a measurement rather than a report: the world
// that produced the identity had nothing removed, so nothing a reduction did
// could have moved the key. Divergences from reduced candidates are consulted by
// reducedDrift, and only to decide whether to ask this question again.
func (p *prober) keyDrift() *Identity { return p.driftFrom(true) }

func (p *prober) cached(c Candidate) (decision, bool) {
	if p.pol.DisableCache {
		return decision{}, false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	d, ok := p.cache[c.key()]
	return d, ok
}

func (p *prober) memo(c Candidate, d decision) {
	if p.pol.DisableCache || d.Truncated {
		// A budget-truncated judgement is provisional. Caching it would let a
		// single unlucky moment near the ceiling harden into a permanent
		// rejection for the rest of the pipeline.
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cache[c.key()] = d
}

// worldsRun reports how many worlds the prober has executed.
func (p *prober) worldsRun() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.attempts
}

// ---------------------------------------------------------------------------
// execution
// ---------------------------------------------------------------------------

// execute runs one round: at most one execution of each given candidate.
//
// It charges the ledger FIRST and honours a partial grant by running a prefix of
// the slice, in index order. That is what keeps the answer deterministic at the
// budget ceiling: which candidates got to run is a function of their position,
// not of which goroutine won a race.
//
// The returned slice is as long as the number of candidates actually executed.
func (p *prober) execute(ctx context.Context, cs []Candidate) ([]Attempt, StopReason) {
	if len(cs) == 0 {
		return nil, StopComplete
	}
	if err := ctx.Err(); err != nil {
		return nil, StopCanceled
	}
	granted, reason := p.led.acquire(len(cs))
	if granted <= 0 {
		return nil, reason
	}
	cs = cs[:granted]

	p.mu.Lock()
	p.attempts += len(cs)
	p.mu.Unlock()

	out := make([]Attempt, 0, len(cs))
	width := p.parallelism()
	for start := 0; start < len(cs); start += width {
		end := start + width
		if end > len(cs) {
			end = len(cs)
		}
		chunk := cs[start:end]
		var (
			got []Attempt
			err error
		)
		if p.batch != nil && len(chunk) > 1 {
			got, err = p.batch.ExecuteBatch(ctx, chunk)
			if err == nil && len(got) != len(chunk) {
				err = fmt.Errorf("shrink: ExecuteBatch returned %d attempt(s) for %d candidate(s)",
					len(got), len(chunk))
			}
		} else {
			got = make([]Attempt, 0, len(chunk))
			for _, c := range chunk {
				var a Attempt
				a, err = p.exec.Execute(ctx, c)
				if err != nil {
					break
				}
				got = append(got, a)
			}
		}
		if err != nil {
			p.logf("executor failed: %v", err)
			if ctx.Err() != nil {
				return out, StopCanceled
			}
			return out, StopExecutorError
		}
		out = append(out, got...)
		if ctx.Err() != nil {
			return out, StopCanceled
		}
	}
	return out, reason
}

// ---------------------------------------------------------------------------
// judgement
// ---------------------------------------------------------------------------

// tally accumulates one candidate's evidence across rounds.
type tally struct {
	reproduced int
	misses     int
	unknowns   int
	trials     int
	sig        Signal
	reason     string
	world      string
	done       bool
}

func (t *tally) observe(sig Signal, reason string, world string, pol Policy) {
	t.trials++
	if world != "" {
		t.world = world
	}
	switch sig {
	case SignalReproduced:
		t.reproduced++
		if t.reproduced >= pol.AcceptTrials {
			t.sig, t.reason, t.done = SignalReproduced, "", true
		}
	case SignalUnknown:
		t.unknowns++
		t.reason = reason
		if t.unknowns > pol.UnknownRetries {
			// Abandoned, not rejected. The distinction is the whole point: an
			// unjudgeable candidate is not evidence that its removed elements
			// mattered.
			t.sig, t.done = SignalUnknown, true
		}
	default: // SignalDifferent, SignalClean
		t.misses++
		// A DIFFERENT violation is the more informative of the two misses, so it
		// wins the reported signal (RULE 1's report side).
		if sig == SignalDifferent || t.sig != SignalDifferent {
			t.sig = sig
			t.reason = reason
		}
		if t.misses >= pol.RejectTrials {
			t.done = true
		}
	}
}

func (t tally) decision(truncated bool) decision {
	return decision{
		Signal:     t.sig,
		Trials:     t.trials,
		Reproduced: t.reproduced,
		Reason:     t.reason,
		WorldPath:  t.world,
		Truncated:  truncated,
	}
}

// firstReproducing tests the candidates and returns the index of the one whose
// reduction is accepted, or -1.
//
// The candidates are tested in ROUNDS (one execution of each still-undecided
// candidate, in index order, batched at Policy.Parallelism) and the accepted
// answer is the LOWEST-INDEX candidate accepted in the EARLIEST round.
//
// Two consequences, both deliberate:
//
//   - The answer does not depend on which execution finished first, so the same
//     inputs give the same shrink at any parallelism (D-044's guarantee, applied
//     to candidates instead of world seeds).
//   - Round-based ordering is also the cheaper schedule. Spending trial 1 on
//     every candidate before trial 2 on any of them finds a candidate that
//     reproduces immediately, rather than paying RejectTrials worlds to reject
//     candidate 1 first.
func (p *prober) firstReproducing(ctx context.Context, cs []Candidate) (int, []decision, StopReason) {
	decisions := make([]decision, len(cs))
	tallies := make([]tally, len(cs))
	pending := make([]int, 0, len(cs))

	for i, c := range cs {
		if d, ok := p.cached(c); ok {
			decisions[i] = d
			if d.accepted() {
				p.logf("candidate %d (%s): reproduced [cached]", i, c)
				return i, decisions, StopComplete
			}
			continue
		}
		pending = append(pending, i)
	}

	maxRounds := p.pol.AcceptTrials + p.pol.RejectTrials + p.pol.UnknownRetries
	stop := StopComplete
	for round := 0; round < maxRounds && len(pending) > 0; round++ {
		batch := make([]Candidate, 0, len(pending))
		for _, i := range pending {
			batch = append(batch, cs[i])
		}
		attempts, reason := p.execute(ctx, batch)
		// Two passes, deliberately. The first gathers this round's evidence and
		// may ASK the drift question; the second JUDGES at whatever level
		// survived. One pass would mean the round that raises the question is the
		// last round still rejected for it, and on a schedule ddmin does not
		// revisit, that rejection is permanent.
		//
		// What sits between the passes is D-063's bound. A divergence from a
		// reduced candidate is recorded with reduced provenance, which keyDrift
		// ignores; answerDriftQuestion then re-executes the UNREDUCED world, whose
		// divergences alone can calibrate. A candidate in this batch can prompt a
		// measurement; it cannot substitute for one.
		//
		// noteDivergence dedupes, and relaxTo prunes the entries a calibration
		// just reclassified, so running it twice adds nothing and loses nothing.
		for j, a := range attempts {
			if sig, ids, _ := a.Classify(p.orig, p.strictness()); sig == SignalDifferent {
				p.noteDivergence(ids, p.isUnreduced(batch[j]))
			}
		}
		if rs := p.answerDriftQuestion(ctx); rs != StopComplete && reason == StopComplete {
			// The re-probe ran out of budget. This round's attempts already cost
			// their worlds and are judged at the level that survived (the strict
			// one) and the stop propagates so the caller can say why it ended.
			reason = rs
		}
		p.maybeCalibrate()

		for j, a := range attempts {
			i := pending[j]
			sig, ids, why := a.Classify(p.orig, p.strictness())
			if sig == SignalDifferent {
				p.noteDivergence(ids, p.isUnreduced(batch[j]))
			}
			tallies[i].observe(sig, why, a.WorldPath, p.policy())
		}
		if reason != StopComplete {
			stop = reason
		}
		// Settle every candidate that reached a terminal state THIS round, then
		// answer with the lowest index that was accepted.
		accepted := -1
		next := pending[:0:0]
		for _, i := range pending {
			t := tallies[i]
			if t.done {
				d := t.decision(false)
				decisions[i] = d
				p.memo(cs[i], d)
				if d.accepted() && accepted < 0 {
					accepted = i
				}
				continue
			}
			// Not settled. If the budget cut this round short it never ran, so
			// it stays provisional.
			decisions[i] = t.decision(true)
			next = append(next, i)
		}
		if accepted >= 0 {
			p.logf("candidate %d (%s): reproduced after %d world(s)",
				accepted, cs[accepted], tallies[accepted].trials)
			return accepted, decisions, StopComplete
		}
		pending = next
		if stop != StopComplete {
			break
		}
	}

	// Anything still pending exhausted its rounds without settling. Record it as
	// what it is rather than as a rejection.
	for _, i := range pending {
		d := tallies[i].decision(stop != StopComplete)
		if d.Signal == SignalReproduced {
			// Reproduced but short of AcceptTrials. Not accepted, and not a
			// rejection either.
			d.Signal = SignalUnknown
			d.Reason = fmt.Sprintf("reproduced %d/%d time(s), short of the %d required to accept",
				d.Reproduced, d.Trials, p.pol.AcceptTrials)
		}
		decisions[i] = d
		p.memo(cs[i], d)
	}
	return -1, decisions, stop
}

// probeOnce executes a candidate EXACTLY once and returns what it observed,
// bypassing both the memo table and the accept/reject retry logic.
//
// It exists for stage 0's identity experiment, which is not a judgement and must
// not be answered from either. The memo would return the baseline's own cached
// verdict without running anything (the candidate is byte-identical) and the
// retry logic would keep going until it got the answer it wanted, which is the
// opposite of what an experiment asking "does this repeat?" needs.
//
// The attempt still costs a world and is still charged to the ledger.
func (p *prober) probeOnce(ctx context.Context, c Candidate) ([]Identity, StopReason) {
	attempts, stop := p.execute(ctx, []Candidate{c})
	if len(attempts) == 0 {
		return nil, stop
	}
	sig, ids, _ := attempts[0].Classify(p.orig, p.strictness())
	if sig == SignalDifferent {
		// Stage 0 and answerDriftQuestion both probe the UNREDUCED world, so
		// this is normally unreduced provenance, but it is decided by the
		// candidate, not by the caller, so a future caller probing something
		// else cannot mislabel its evidence.
		p.noteDivergence(ids, p.isUnreduced(c))
	}
	return ids, stop
}

// test judges a single candidate under the same policy.
func (p *prober) test(ctx context.Context, c Candidate) (decision, StopReason) {
	idx, decisions, stop := p.firstReproducing(ctx, []Candidate{c})
	d := decisions[0]
	if idx == 0 {
		d.Signal = SignalReproduced
	}
	return d, stop
}
