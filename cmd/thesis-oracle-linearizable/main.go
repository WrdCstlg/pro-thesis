// Command thesis-oracle-linearizable is PRO-THESIS's linearizability checker
// for the KV register model, packaged as an EXTERNAL oracle (Phase 3 brief D-E).
//
// It reads prothesis.oracle_input/v1 on stdin, writes prothesis.oracle_output/v1
// on stdout, and exits 0 (ok), 1 (violated) or 2 (inconclusive). It declares
// itself as oracle "linearizable.kv", class "consistency", valid in ASSERT.
// Phase validity is ENFORCED BY THE ENGINE, not here: this program declares
// where it is meaningful and the engine refuses to evaluate it elsewhere (I5).
//
// # What it decides, and what it refuses to decide
//
// The three ways this component can fail, ranked by the brief, are a false
// positive, a silent vacuous pass, and a spurious lock failure. Everything below
// is arranged against the first two.
//
//	violated      only ever from a WITNESSED linearization failure: a search
//	              that exhausted the space for one key and found nothing. Never
//	              from a timeout, never from a budget, never from a parse error.
//	inconclusive  every refusal. An empty history, a multi-key operation, an
//	              unparseable record, a missing op_id, an exhausted budget, a
//	              history with no determinate reads.
//	ok            only when every key's search ran to completion and found a
//	              linearization, and there was real evidence to check.
//
// # The four soundness rules that earn the verdict
//
//  1. PER-KEY DECOMPOSITION IS CHECKED BEFORE IT IS USED (load.go).
//     Herlihy & Wing's locality theorem makes per-key checking exact, but only
//     for histories in which every operation touches exactly one key. The
//     fixture's `txn` reads one key and writes a different one, so the gate and
//     soak profiles violate the precondition. A multi-key operation is refused
//     by name; the history is never partitioned regardless.
//
//  2. ok / fail / info ARE HONOURED EXACTLY (load.go).
//     fail is dropped with its invoke. An indeterminate read is dropped. An
//     indeterminate WRITE is explored on both branches, because treating it as
//     definitely applied constrains the model more than reality permits, and a
//     more-constrained model rejecting a history does not prove the real one
//     does. An indeterminate operation's interval also extends to infinity: a
//     request that timed out may still apply afterwards, and bounding it at its
//     own timeout is the same false-positive generator in a different disguise.
//
//  3. THE SEARCH IS BUDGETED AND EXHAUSTION IS INCONCLUSIVE (check.go).
//     Wall clock and explored-state ceiling, shared out per key so one
//     pathological key cannot starve the rest. A key that did not finish is
//     never reported ok.
//
//  4. THE REGISTER STARTS UNDETERMINED (search.go).
//     Assuming an initial value of null would report a violation for any history
//     whose first read observes a value written before the recorded window
//     began.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"

	"github.com/WrdCstlg/pro-thesis/internal/buildinfo"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// The oracle's own declaration. These three values are its identity in the
// verdict, in .prothesis/lock and in the engine's phase-validity check.
const (
	oracleName  = "linearizable.kv"
	oracleClass = schema.ClassConsistency
)

func oracleValidPhases() []schema.Phase { return []schema.Phase{schema.PhaseAssert} }

// Environment fallbacks, for a runner that passes no arguments.
const (
	envWall      = "PROTHESIS_ORACLE_WALL"
	envMaxStates = "PROTHESIS_ORACLE_MAX_STATES"
	envMemoBytes = "PROTHESIS_ORACLE_MAX_MEMO_BYTES"
)

func main() {
	// -version is handled here, BEFORE run(): stdout is the oracle protocol
	// channel, so only a human invoking the flag by hand may ever see this.
	for _, a := range os.Args[1:] {
		if a == "-version" || a == "--version" {
			fmt.Printf("thesis-oracle-linearizable build: commit %s dirty=%s source_date=%s\n",
				stamp(buildinfo.Commit), stamp(buildinfo.Dirty), stamp(buildinfo.SourceDate))
			os.Exit(0)
		}
	}
	os.Exit(int(run(os.Stdin, os.Stdout, os.Stderr, os.Args[1:], time.Now())))
}

func stamp(s string) string {
	if s == "" {
		return "unstamped"
	}
	return s
}

// run is main's testable core. It ALWAYS writes a valid
// prothesis.oracle_output/v1 to stdout, and its exit code always agrees with the
// status in that document.
func run(stdin io.Reader, stdout, stderr io.Writer, args []string, start time.Time) schema.OracleExitCode {
	opt, err := parseOptions(args, stderr)
	if err != nil {
		return emit(stdout, stderr, inconclusive(
			"the oracle could not parse its own arguments: %v", err))
	}

	raw, err := io.ReadAll(stdin)
	if err != nil {
		return emit(stdout, stderr, inconclusive(
			"reading %s from stdin failed: %v", schema.OracleInputSchema, err))
	}
	in, err := schema.UnmarshalOracleInput(raw)
	if err != nil {
		return emit(stdout, stderr, inconclusive(
			"the input document on stdin is not usable: %v", err))
	}

	// A malformed document is refused, but a document that merely fails a field
	// this oracle does not read (world_path, say) must not cost a run its
	// consistency check. The validation error is carried into the explanation
	// instead, so it is visible rather than swallowed.
	var note string
	if err := in.Validate(); err != nil {
		note = fmt.Sprintf(" (note: the input document did not validate: %v)", err)
	}
	if in.HistoryPath == "" {
		return emit(stdout, stderr, inconclusive(
			"the input document names no history_path, so there is nothing to check%s", note))
	}

	rep := check(in.HistoryPath, opt, start)
	if note != "" {
		rep.Explanation += note
	}
	return emit(stdout, stderr, rep)
}

// emit writes the output document and returns the matching exit code.
//
// The exit code is derived from the status in the document that was written, so
// the two channels cannot disagree: a disagreement is an oracle defect the
// engine would have to treat as inconclusive.
func emit(stdout, stderr io.Writer, rep report) schema.OracleExitCode {
	out := &schema.OracleOutput{
		Schema:      schema.OracleOutputSchema,
		Oracle:      oracleName,
		Class:       oracleClass,
		ValidPhases: oracleValidPhases(),
		Status:      rep.Status,
		Witness:     rep.Witness,
		Explanation: rep.Explanation,
	}
	if out.Status == schema.StatusViolated && out.Explanation == "" {
		out.Explanation = "a linearization failure was witnessed but not described; " +
			"this is a defect in " + oracleName
	}
	if err := out.Validate(); err != nil {
		// Refuse to emit a document this oracle's own schema rejects.
		fmt.Fprintf(stderr, "%s: refusing to emit an invalid output document: %v\n", oracleName, err)
		out = &schema.OracleOutput{
			Schema:      schema.OracleOutputSchema,
			Oracle:      oracleName,
			Class:       oracleClass,
			ValidPhases: oracleValidPhases(),
			Status:      schema.StatusInconclusive,
			Explanation: fmt.Sprintf("%s built an output document that did not validate: %v", oracleName, err),
		}
	}

	b, err := schema.MarshalOracleOutput(out)
	if err != nil {
		fmt.Fprintf(stderr, "%s: encoding the output document failed: %v\n", oracleName, err)
		return schema.OracleExitInconclusive
	}
	if _, err := stdout.Write(b); err != nil {
		fmt.Fprintf(stderr, "%s: writing the output document failed: %v\n", oracleName, err)
		return schema.OracleExitInconclusive
	}

	switch out.Status {
	case schema.StatusOK:
		return schema.OracleExitOK
	case schema.StatusViolated:
		return schema.OracleExitViolated
	default:
		return schema.OracleExitInconclusive
	}
}

// parseOptions reads the budget from the environment, then lets flags override.
func parseOptions(args []string, stderr io.Writer) (options, error) {
	opt := defaultOptions()

	if s := os.Getenv(envWall); s != "" {
		d, err := time.ParseDuration(s)
		if err != nil {
			return opt, fmt.Errorf("%s=%q: %w", envWall, s, err)
		}
		opt.Wall = d
	}
	if s := os.Getenv(envMaxStates); s != "" {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return opt, fmt.Errorf("%s=%q: %w", envMaxStates, s, err)
		}
		opt.MaxStates = n
	}
	if s := os.Getenv(envMemoBytes); s != "" {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return opt, fmt.Errorf("%s=%q: %w", envMemoBytes, s, err)
		}
		opt.MaxMemoBytes = n
	}

	fs := flag.NewFlagSet("thesis-oracle-linearizable", flag.ContinueOnError)
	fs.SetOutput(stderr)
	wall := fs.Duration("wall", opt.Wall,
		"total wall-clock budget; exhausting it is INCONCLUSIVE, never ok")
	states := fs.Int64("max-states", opt.MaxStates,
		"total explored-state ceiling across every key; exhausting it is INCONCLUSIVE, never ok")
	memo := fs.Int64("max-memo-bytes", opt.MaxMemoBytes,
		"memoisation table ceiling; exceeding it costs speed, never soundness")
	if err := fs.Parse(args); err != nil {
		return opt, err
	}
	if fs.NArg() > 0 {
		return opt, fmt.Errorf("unexpected argument %q; this oracle reads its input from stdin", fs.Arg(0))
	}
	opt.Wall, opt.MaxStates, opt.MaxMemoBytes = *wall, *states, *memo

	if opt.Wall <= 0 {
		return opt, fmt.Errorf("-wall must be positive, got %s", opt.Wall)
	}
	if opt.MaxStates < 1 {
		return opt, fmt.Errorf("-max-states must be at least 1, got %d", opt.MaxStates)
	}
	if opt.MaxMemoBytes < 1 {
		return opt, fmt.Errorf("-max-memo-bytes must be at least 1, got %d", opt.MaxMemoBytes)
	}
	return opt, nil
}
