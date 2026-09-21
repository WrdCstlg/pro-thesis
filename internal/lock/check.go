package lock

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// GateOptions are the inputs to the enforcement point.
type GateOptions struct {
	// ProjectDir is the directory holding prothesis.yaml and .prothesis/.
	ProjectDir string
	// ConfigPath is the prothesis.yaml to project. Empty means
	// <ProjectDir>/prothesis.yaml.
	ConfigPath string
	// ConfigBytes overrides reading ConfigPath, for tests and for a caller that
	// already has the bytes. It must be the RAW user document, never a
	// re-serialisation of a decoded Config, which would carry defaults.
	ConfigBytes []byte
	// BuiltinOptionsFingerprint is internal/oracle.Options.Fingerprint().
	// Compared for a WARNING only; see Report.OptionsMoved and D-035.
	BuiltinOptionsFingerprint string
}

// Report is the result of one drift check.
type Report struct {
	// Status is the verdict's oracle_lock.status: ok, mismatch or absent.
	Status schema.LockStatus
	// ManifestSHA is the RECOMPUTED digest. It is what the verdict carries in
	// every case, including a mismatch: reporting the stale locked digest on a
	// drifted run would misdescribe what actually ran.
	ManifestSHA string
	// LockedSHA is the digest recorded in .prothesis/lock, or "" when absent.
	LockedSHA string
	// LockPath is where the lock was looked for.
	LockPath string
	// Manifest is the recomputed manifest.
	Manifest Manifest
	// Changes is what moved, empty unless Status is mismatch.
	Changes []Change
	// Reason and LockedAt come from the lock file, so a drift message can say
	// what the last bump claimed to be for.
	Reason   string
	LockedAt string
	// OraclesDir is the directory that was walked, as resolved.
	OraclesDir string
	// OptionsMoved reports that the compiled-in built-in oracle thresholds
	// differ from the ones recorded at lock time. It is a WARNING and NEVER
	// drift: those thresholds are reachable from no config key, so treating a
	// release that adjusts one as ORACLE_DRIFT would halt every downstream
	// project on an upgrade; the failure D-F exists to prevent. See D-035.
	OptionsMoved          bool
	OptionsFingerprintWas string
	OptionsFingerprintNow string

	// ExecutablesNow are the resolved-program fingerprints measured during this
	// check, so a caller writing a lock does not have to measure them twice.
	ExecutablesNow []ExecutableFingerprint
	// ExecutablesMoved names external oracles whose resolved program no longer
	// matches the lock. Like OptionsMoved it is a WARNING and NEVER drift, and
	// for the same shape of reason: hashing a binary into the digest would make
	// an ordinary rebuild exit 4 on every downstream project, and the correct
	// response would become re-locking without reading the diff.
	//
	// Unlike OptionsMoved it also reaches the VERDICT, because this is the
	// program that DECIDED the verdict. A green result whose checker changed has
	// to carry that fact or the result is not interpretable. See OQ-057, D-060.
	ExecutablesMoved []ExecutableChange

	// InternallyInconsistent reports that the lock file's own recorded manifest
	// does not hash to its own recorded digest, which only hand-editing
	// produces. Status is LockMismatch when it is set.
	InternallyInconsistent   bool
	InconsistencyExplanation string
}

// Gate is THE enforcement point (D-I).
//
// `thesis run` calls it before booting anything (there is no point starting a
// cluster for a run whose verdict is already void) and Phase 6's `thesis gate`
// wraps this same function rather than reimplementing it.
//
// It returns an error only for conditions under which drift cannot be
// DETERMINED: an unreadable config, an unreadable oracles directory, a corrupt
// lock file. Those map to exit 2 (retry once, then escalate), never to exit 0.
// A determinable outcome (ok, mismatch or absent) is always a Report with a
// nil error.
func Gate(opts GateOptions) (*Report, error) {
	projectDir := opts.ProjectDir
	if projectDir == "" {
		projectDir = "."
	}
	abs, err := filepath.Abs(projectDir)
	if err != nil {
		return nil, fmt.Errorf("lock: resolve project dir %s: %w", projectDir, err)
	}
	projectDir = abs

	data := opts.ConfigBytes
	if data == nil {
		p := opts.ConfigPath
		if p == "" {
			p = filepath.Join(projectDir, "prothesis.yaml")
		}
		data, err = os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("lock: read %s: %w", p, err)
		}
	}

	oraclesDir, err := OracleDirFromConfig(data)
	if err != nil {
		return nil, err
	}
	manifest, err := Build(projectDir, data, oraclesDir)
	if err != nil {
		return nil, err
	}
	sum, err := manifest.Digest()
	if err != nil {
		return nil, err
	}

	rep := &Report{
		ManifestSHA: sum,
		LockPath:    Path(projectDir),
		Manifest:    manifest,
		OraclesDir:  oraclesDir,
	}

	// Measured unconditionally, including when the lock is absent, so that
	// `thesis oracles lock` can record what it saw without a second walk of the
	// same definitions. A failure here is NOT fatal to the gate: an oracle whose
	// program cannot be fingerprinted must not prevent a drift check from being
	// determinable, because the drift check is about the DEFINITIONS and is
	// answerable without it.
	if fps, ferr := FingerprintExecutables(projectDir, oraclesDir); ferr == nil {
		rep.ExecutablesNow = fps
	}

	locked, err := Read(projectDir)
	if err != nil {
		if errors.Is(err, ErrNoLock) {
			// Absent is NOT ok. It is also not drift: a project that has never
			// been locked must still be runnable, and refusing to run one would
			// be a spurious exit 4 on first contact.
			rep.Status = schema.LockAbsent
			return rep, nil
		}
		return nil, err
	}

	rep.LockedSHA = locked.ManifestSHA
	rep.Reason = locked.Reason
	rep.LockedAt = locked.LockedAt
	rep.OptionsFingerprintWas = locked.BuiltinOptionsFingerprint
	rep.OptionsFingerprintNow = opts.BuiltinOptionsFingerprint
	rep.ExecutablesMoved = DiffExecutables(locked.Executables, rep.ExecutablesNow)
	rep.OptionsMoved = locked.BuiltinOptionsFingerprint != "" &&
		opts.BuiltinOptionsFingerprint != "" &&
		locked.BuiltinOptionsFingerprint != opts.BuiltinOptionsFingerprint

	// A lock file whose recorded manifest does not hash to its recorded digest
	// has been hand-edited. That is the loudest available evidence of tampering
	// and it is reported as a mismatch, not smoothed over: trusting the
	// recorded digest over the recorded manifest would let an agent paste in a
	// digest it computed from a config it then changed back.
	if selfSum, serr := locked.Manifest.Digest(); serr == nil && selfSum != locked.ManifestSHA {
		rep.Status = schema.LockMismatch
		rep.InternallyInconsistent = true
		rep.InconsistencyExplanation = fmt.Sprintf(
			"the lock file's recorded manifest hashes to %s but the file claims %s; "+
				"the file has been edited by hand", selfSum, locked.ManifestSHA)
		rep.Changes = Diff(locked.Manifest, manifest)
		return rep, nil
	}

	if locked.ManifestSHA == sum {
		rep.Status = schema.LockOK
		return rep, nil
	}

	rep.Status = schema.LockMismatch
	rep.Changes = Diff(locked.Manifest, manifest)
	return rep, nil
}

// EnforceExitCode is the code `thesis run` returns for this report.
//
// Only a MISMATCH blocks. An absent lock does not: the drift check has never
// been baselined for this project, which is reported honestly in the verdict as
// "absent" and warned about on stderr, but it is not evidence that anything was
// weakened.
func (r *Report) EnforceExitCode() schema.ExitCode {
	if r.Status == schema.LockMismatch {
		return schema.ExitOracleDrift
	}
	return schema.ExitPass
}

// VerifyExitCode is the code `thesis oracles verify` returns.
//
// An ABSENT lock fails here where it does not fail a run. `verify` is an
// explicit request to check the gate against its baseline; answering "there is
// no baseline" with exit 0 would be a vacuous pass in the one command whose
// entire job is to notice that the baseline moved, and it is a pass an agent
// could manufacture by deleting one file. Exit 4 is the right code because the
// remedy is a human-authored `thesis oracles lock --reason`, which is precisely
// what an agent must not do for itself.
func (r *Report) VerifyExitCode() schema.ExitCode {
	switch r.Status {
	case schema.LockOK:
		return schema.ExitPass
	default:
		return schema.ExitOracleDrift
	}
}

// OracleLock renders the report as the verdict's `oracle_lock` object.
//
// ManifestSHA is always the RECOMPUTED digest, including on a mismatch: the
// verdict describes the run that happened, and the run happened against the
// current manifest, not the locked one.
func (r *Report) OracleLock() schema.OracleLock {
	out := schema.OracleLock{Status: r.Status, ManifestSHA: r.ManifestSHA}
	for _, c := range r.ExecutablesMoved {
		out.ExecutablesMoved = append(out.ExecutablesMoved, c.Oracle)
	}
	return out
}

// Diagnosis is the message a human is escalated to.
//
// "Lock mismatch" on its own forces the reader to diff by hand, so this names
// the file, the budget or the allow-list entry that moved, with old and new
// values, and states the two admissible responses.
func (r *Report) Diagnosis() string {
	var b strings.Builder
	switch r.Status {
	case schema.LockOK:
		fmt.Fprintf(&b, "oracle lock OK: %s\n", r.ManifestSHA)
		fmt.Fprintf(&b, "  %d oracle file(s) under %s, %d covered config value(s)\n",
			len(r.Manifest.Oracles), r.OraclesDir, len(r.Manifest.Config))
		if r.Reason != "" {
			fmt.Fprintf(&b, "  locked %s: %s\n", r.LockedAt, r.Reason)
		}

	case schema.LockAbsent:
		fmt.Fprintf(&b, "ORACLE LOCK ABSENT — drift is NOT being enforced for this project.\n")
		fmt.Fprintf(&b, "  no lock at %s\n", r.LockPath)
		fmt.Fprintf(&b, "  computed manifest: %s\n", r.ManifestSHA)
		fmt.Fprintf(&b, "  %d oracle file(s) under %s, %d covered config value(s)\n",
			len(r.Manifest.Oracles), r.OraclesDir, len(r.Manifest.Config))
		fmt.Fprintf(&b, "  Baseline it with a HUMAN-authored reason:\n")
		fmt.Fprintf(&b, "      thesis oracles lock --reason \"initial baseline\"\n")

	case schema.LockMismatch:
		fmt.Fprintf(&b, "ORACLE DRIFT — the gate has moved since it was last locked.\n")
		fmt.Fprintf(&b, "  locked   %s\n", r.LockedSHA)
		fmt.Fprintf(&b, "  computed %s\n", r.ManifestSHA)
		if r.LockedAt != "" {
			fmt.Fprintf(&b, "  last locked %s: %s\n", r.LockedAt, r.Reason)
		}
		if r.InternallyInconsistent {
			fmt.Fprintf(&b, "  LOCK FILE INTEGRITY: %s\n", r.InconsistencyExplanation)
		}
		if len(r.Changes) == 0 {
			fmt.Fprintf(&b, "  The digest differs but no field-level difference was found. "+
				"That means the manifest FORMAT changed, not your configuration; "+
				"this is a thesis-version issue, not a gate weakening.\n")
		} else {
			fmt.Fprintf(&b, "  What moved (%d):\n", len(r.Changes))
			b.WriteString(indent(FormatChanges(r.Changes), "  "))
		}
		fmt.Fprintf(&b, "  Exit 4 is never auto-resolved. Either revert the change above, or, "+
			"if it is intended, record it in a reviewable commit:\n")
		fmt.Fprintf(&b, "      thesis oracles lock --reason \"...why this gate change is correct...\"\n")
	}

	if r.OptionsMoved {
		fmt.Fprintf(&b, "  WARNING (not drift): built-in oracle thresholds changed since this lock "+
			"was written: %s -> %s. They are compiled in and reachable from no config key, so they "+
			"are recorded but NOT digested (D-035). Review them if this gate result surprises you.\n",
			r.OptionsFingerprintWas, r.OptionsFingerprintNow)
	}
	// Printed for EVERY status, including ok. A swapped checker's whole
	// signature is that everything else looks normal, so a warning that only
	// appeared alongside other bad news would appear exactly when it was not
	// needed. See OQ-057, D-060.
	if len(r.ExecutablesMoved) > 0 {
		b.WriteString(indent(FormatExecutableChanges(r.ExecutablesMoved), "  "))
	}
	return b.String()
}

func indent(s, prefix string) string {
	if s == "" {
		return ""
	}
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i := range lines {
		lines[i] = prefix + lines[i]
	}
	return strings.Join(lines, "\n") + "\n"
}
