package control

import (
	"path/filepath"
	"strings"

	"github.com/WrdCstlg/pro-thesis/internal/recorder"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// Artifact file names inside a world directory, and the verdict inside a run
// directory.
//
// ADDITIVE: the directive names none of these files. It names the SCHEMAS they
// carry (prothesis.verdict/v1, the history JSONL, prothesis.oracle_input/v1)
// and refers to their locations only through `artifacts` and through the
// oracle input's *_path fields, which are supplied at runtime. Without agreed
// names, two subsystems writing into the same world directory would collide.
const (
	// HistoryFileName is the driver's operation log. ONLY the driver writes it
	// (PHASE0_BUILD_BRIEF D-D).
	HistoryFileName = "history.jsonl"

	// PhasesLogName is the harness's phase-marker log. ONLY the control plane
	// writes it. The recorder merges it with the history at TEARDOWN.
	PhasesLogName = "phases.jsonl"

	// PhasesFileName carries the measured schema.PhaseTimings.
	//
	// It exists because the marker stream cannot express nesting: PERTURB
	// overlaps DRIVE (directive 4.1 step 4), so a reader of phases.jsonl alone
	// would conclude DRIVE ended when PERTURB began. This file is the
	// AUTHORITATIVE record of the windows, and it is what the oracle input and
	// the world file are built from. Logged as OQ-020.
	PhasesFileName = "phases.json"

	// TelemetryFileName is the sampled-observation artifact.
	TelemetryFileName = "telemetry.jsonl"

	// FinalStateFileName is prothesis.oracle_input/v1's final_state_path.
	FinalStateFileName = "final_state.json"

	// OracleInputFileName is the prothesis.oracle_input/v1 document, written
	// for debuggability so a human can rerun an oracle by hand.
	OracleInputFileName = "oracle_input.json"

	// ResultFileName is the per-world execution record.
	ResultFileName = "result.json"

	// LogsDirName holds the collected per-node logs.
	LogsDirName = "logs"

	// VerdictFileName is prothesis.verdict/v1 in the RUN directory.
	VerdictFileName = "verdict.json"
)

// WorldPaths are one world's artifact locations, as absolute filesystem paths.
type WorldPaths struct {
	// Dir is the world directory.
	Dir string
	// World is the .thesis file.
	World string
	// History is the driver's operation log.
	History string
	// PhasesLog is the phase-marker log.
	PhasesLog string
	// Phases is the measured phase windows.
	Phases string
	// Telemetry is the sampled-observation artifact.
	Telemetry string
	// FinalState is the final-state artifact.
	FinalState string
	// OracleInput is the oracle stdin document.
	OracleInput string
	// Logs is the per-node log directory.
	Logs string
	// Result is the per-world execution record.
	Result string
}

// NewWorldPaths derives the artifact layout for a world directory.
func NewWorldPaths(dir string) WorldPaths {
	j := func(name string) string { return filepath.Join(dir, name) }
	return WorldPaths{
		Dir:         dir,
		World:       j(recorder.WorldFileBase),
		History:     j(HistoryFileName),
		PhasesLog:   j(PhasesLogName),
		Phases:      j(PhasesFileName),
		Telemetry:   j(TelemetryFileName),
		FinalState:  j(FinalStateFileName),
		OracleInput: j(OracleInputFileName),
		Logs:        j(LogsDirName),
		Result:      j(ResultFileName),
	}
}

// OracleInputDoc builds prothesis.oracle_input/v1 for these paths and windows.
//
// Every path is emitted even when the file does not exist. That is deliberate
// and is the honest behaviour: the document's contract is "here is where each
// artifact lives", and an oracle that needs telemetry must fail on a missing
// file (reporting INCONCLUSIVE) rather than silently evaluating against
// nothing and reporting ok.
func (p WorldPaths) OracleInputDoc(phases schema.PhaseTimings) schema.OracleInput {
	return schema.NewOracleInput(p.History, p.FinalState, p.Telemetry, p.World, phases)
}

// NodeLogPath is where a node's collected log is written.
func (p WorldPaths) NodeLogPath(nodeID string) string {
	return filepath.Join(p.Logs, sanitizeLogName(nodeID)+".log")
}

// sanitizeLogName reduces a node id to something recorder.SafeName accepts, so
// a node called `con` or `n1/2` cannot produce an unopenable file on Windows.
func sanitizeLogName(id string) string {
	var b strings.Builder
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := b.String()
	if out == "" {
		out = "node"
	}
	if err := recorder.SafeName(out); err != nil {
		// A reserved device name, a trailing dot, or an over-long id. Prefixing
		// removes every one of those cases without losing the original.
		out = "node_" + out
	}
	if len(out) > 60 {
		out = out[:60]
	}
	return out
}

// RelativeArtifacts renders an artifact directory the way directive 4.6's
// `artifacts` field shows it: repo-relative, forward slashes, trailing slash.
//
// It falls back to the absolute path when dir is not under projectDir, because
// a wrong relative path is worse than a verbose absolute one: an agent that
// cannot find the bundle cannot act on the verdict.
func RelativeArtifacts(projectDir, dir string) string {
	out := dir
	if rel, err := filepath.Rel(projectDir, dir); err == nil && !strings.HasPrefix(rel, "..") {
		out = rel
	}
	out = filepath.ToSlash(out)
	if !strings.HasSuffix(out, "/") {
		out += "/"
	}
	return out
}
