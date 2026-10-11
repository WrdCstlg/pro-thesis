package control

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// VerdictInput is everything prothesis.verdict/v1 needs that is not derivable
// from the violations themselves.
type VerdictInput struct {
	RunID   string
	Profile string
	// Commit identifies the build that produced the verdict: the harness
	// binary's build stamp when present (D-070), else the working tree's HEAD;
	// empty when neither exists, never fabricated.
	Commit string
	// Outcome is the folded run outcome.
	Outcome Outcome
	// Budget is the measured budget snapshot.
	Budget schema.Budget
	// Violations are the findings, already carrying ids and causal timelines.
	Violations []schema.Violation
	// Artifacts is the run bundle path, as directive 4.6 renders it.
	Artifacts string
	// OracleLock is the drift check made before the run started, verbatim.
	//
	// The ZERO VALUE normalizes to "absent", not "ok". That default is
	// load-bearing: a caller that never checked must not be able to produce a
	// verdict claiming a clean drift check, and an agent that deleted
	// .prothesis/lock must not thereby clear it.
	OracleLock schema.OracleLock
}

// BuildVerdict assembles prothesis.verdict/v1.
//
// # What is deliberately empty, and why saying so matters
//
//	coverage                    zeros. Coverage signals are a Phase 4
//	                            deliverable. Zeros are the truth: nothing was
//	                            measured, so nothing was found.
//
//	coverage_delta_vs_baseline  null. There is no baseline commit to compare
//	                            against, and a fabricated {templates: 0} would
//	                            read as "no code paths lost", which is a claim
//	                            nobody made.
//
//	oracle_lock                 whatever the caller's drift check found, and
//	                            "absent" when it made none. Phase 3 wires the
//	                            real check in: the CLI runs internal/lock.Gate
//	                            BEFORE booting anything and passes the result
//	                            here, because a drifted gate must not start a
//	                            cluster (D-I). Manufacturing "ok" from a
//	                            zero value would be the exact gate-weakening
//	                            invariant I6 exists to prevent: an agent that
//	                            deleted .prothesis/lock would get a clean drift
//	                            check from a tool that never checked. The zero
//	                            value therefore normalizes to "absent" — there
//	                            is no lock, so drift is NOT enforced.
//
// The verdict field ORACLE_DRIFT (exit 4) is unreachable from here. See
// Outcome.VerdictResult.
func BuildVerdict(in VerdictInput) schema.Verdict {
	violations := in.Violations
	if violations == nil {
		violations = []schema.Violation{}
	}

	outcome := in.Outcome
	lock := in.OracleLock

	// A NARROWED run (one whose budget the command line cut below the
	// profile's) carries two invariants, applied here so that every command
	// building a verdict gets them rather than each remembering separately.
	//
	// 1. It may not report PASS. PASS means "all oracles satisfied across all
	//    worlds", and the worlds it quantifies over are the ones the profile
	//    specifies. A run cut to a fraction of them that came back clean has
	//    not falsified anything; it declined to look. Exit 2's published agent
	//    action (retry once, then escalate) is the correct instruction.
	//
	//    Only PASS is downgraded. A violation found under a narrowed budget is
	//    still a violation and still exits 1, which is what keeps `--worlds 1`
	//    useful while iterating on a fix. Budget exhaustion is already exit 3.
	//
	//    This also closes the world-budget hole: exhausting `--worlds` with no
	//    violation was indistinguishable from a clean run, because the runner
	//    consulted only WallExpired. A profile's OWN world count running out
	//    cleanly remains a PASS: that is the gate the profile asked for.
	//
	// 2. It may not report `oracle_lock: ok`. The digest still matches the
	//    file; it is argv that moved a lock-covered value, so "ok" would be a
	//    true sentence functioning as a false one. An ABSENT lock stays absent:
	//    nothing was locked, so nothing was bypassed, and absent already fails
	//    `thesis oracles verify`.
	//
	// See D-059 and OQ-059.
	if in.Budget.Narrowed {
		if outcome == OutcomePass {
			outcome = OutcomeInconclusive
		}
		if lock.Status == schema.LockOK {
			lock.Status = schema.LockBypassed
		}
	}

	v := schema.Verdict{
		Schema:     schema.VerdictSchema,
		RunID:      in.RunID,
		Profile:    in.Profile,
		Commit:     in.Commit,
		Verdict:    outcome.VerdictResult(),
		Budget:     in.Budget,
		Violations: violations,

		Coverage:                schema.Coverage{},
		CoverageDeltaVsBaseline: nil,

		OracleLock: lock,
		Artifacts:  in.Artifacts,
	}
	v.Normalize()
	return v
}

// NewViolation converts a violated oracle result into a verdict violation.
//
// Severity is assigned from the class by schema.DefaultSeverity and is never
// read from the oracle: prothesis.oracle_output/v1 has no severity field, and
// adding one would let an oracle grade its own finding down.
//
// minimal_repro is NULL and shrink.attempted is FALSE. Both are Phase 5. The
// distinction between them is the point: `shrink` is never null because it
// carries `attempted`, which exists precisely to express the not-attempted
// case, while a `minimal_repro` object would have to carry a `reproduced: "k/n"`
// string, and there is no k and no n. A fabricated "0/0" would be a claim about
// determinism this tool has not measured.
//
// suspect is NULL. The verdict schema wants suspected source files with a
// confidence score, derived from "log-template locality + blame over shrunk
// timeline window". Log-template coverage is Phase 4 and the shrunk window is
// Phase 5, so neither input exists; a confidence number produced without them
// would be a number with no derivation. Logged as OQ-019.
func NewViolation(id string, res OracleResult, timeline []schema.TimelineEvent) schema.Violation {
	v := res.Output.ToViolation(id, res.ObservedPhase, res.FirstSeenMS)
	if timeline == nil {
		timeline = []schema.TimelineEvent{}
	}
	v.CausalTimeline = timeline
	v.MinimalRepro = nil
	v.Shrink = schema.Shrink{Attempted: false, SurvivingFaults: []string{}}
	v.Suspect = nil
	return v
}

// ViolationID renders the nth violation id in directive 4.6's spelling.
func ViolationID(n int) string { return fmt.Sprintf("v%d", n) }

// WriteVerdictFile writes the verdict as indented JSON with a trailing newline.
//
// It validates first. A verdict that fails its own schema is a bug in this
// package, and writing it anyway would put a malformed document in front of the
// agent that has to act on it.
func WriteVerdictFile(path string, v *schema.Verdict) error {
	if err := v.Validate(); err != nil {
		return fmt.Errorf("control: refusing to write a verdict that fails validation: %w", err)
	}
	b, err := schema.MarshalVerdict(v)
	if err != nil {
		return fmt.Errorf("control: encode verdict: %w", err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		return fmt.Errorf("control: write %s: %w", path, err)
	}
	return nil
}

// gitCommitTimeout bounds the revision lookup. git is not on the critical path
// of anything; a hung invocation must not hold a run open.
const gitCommitTimeout = 5 * time.Second

// GitCommit returns the short revision of the working tree at dir, or "".
//
// It NEVER returns an error. The verdict's `commit` field is metadata: a run in
// a directory that is not a git repository, or on a machine with no git, is a
// perfectly valid run, and failing it over a missing changelog entry would be
// absurd. What is not acceptable is inventing a value, so the failure mode is
// the empty string.
func GitCommit(ctx context.Context, dir string) string {
	gctx, cancel := context.WithTimeout(ctx, gitCommitTimeout)
	defer cancel()

	cmd := exec.CommandContext(gctx, "git", "-C", dir, "rev-parse", "--short", "HEAD")
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return ""
	}
	rev := strings.TrimSpace(out.String())
	for _, c := range rev {
		if (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') {
			continue
		}
		// Not a hex revision: an abbreviated ref, a detached-head message, or
		// locale-translated output. Report nothing rather than something wrong.
		return ""
	}
	return rev
}
