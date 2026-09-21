package lock

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/WrdCstlg/pro-thesis/internal/recorder"
)

// FileName is the lock file inside the project state directory.
const FileName = "lock"

// FileSchema identifies the lock document.
//
// ADDITIVE, like ManifestSchema: the directive names the PATH `.prothesis/lock`
// and the verdict field `oracle_lock.manifest_sha`, and fixes no document
// schema for the file itself. Logged as OQ-027.
const FileSchema = "prothesis.lock/v1"

// ErrNoLock is returned when the project has no lock file.
//
// It is a distinct sentinel because "absent" and "mismatched" call for
// different actions and must never be collapsed: a mismatch is drift a human
// must adjudicate, an absence is a project that has not been locked yet.
var ErrNoLock = errors.New("lock: no .prothesis/lock in this project")

// Path returns the lock file path for a project root.
func Path(projectDir string) string {
	return filepath.Join(recorder.StateDir(projectDir), FileName)
}

// File is the on-disk `.prothesis/lock` document.
//
// It is written as indented JSON rather than canonical compact JSON on purpose:
// the whole design intent is that a lock bump is a REVIEWABLE COMMIT, and a
// one-line file produces a diff no reviewer can read. The digest is computed
// over the canonical encoding of Manifest alone, so the presentation of this
// file has no effect on it.
//
// Reason, LockedAt, ToolVersion, BuiltinOptionsFingerprint and Limitations are
// all OUTSIDE the digest. That is deliberate for each:
//
//   - Reason and LockedAt change on every bump by construction; digesting them
//     would make the digest a function of when it was written.
//   - ToolVersion must not be hashed, or every upgrade of `thesis` becomes an
//     exit-4 incident on every downstream project (see cmd/thesis/version.go
//     and OQ-015.2).
//   - BuiltinOptionsFingerprint covers COMPILED-IN thresholds reachable from no
//     config key. It is recorded so a reviewer can see it moved, and excluded
//     so that a threshold adjustment in a release is a warning rather than
//     drift. See DECISIONS.md D-035.
type File struct {
	Schema string `json:"schema"`
	// ManifestSHA is the digest of Manifest, "sha256:"-prefixed. It is the
	// value the verdict's oracle_lock.manifest_sha carries.
	ManifestSHA string `json:"manifest_sha"`
	// Reason is the MANDATORY human justification for this bump.
	Reason string `json:"reason"`
	// LockedAt is RFC 3339 UTC.
	LockedAt string `json:"locked_at"`
	// ToolVersion is the thesis build that wrote the file. Informational.
	ToolVersion string `json:"tool_version"`
	// BuiltinOptionsFingerprint is internal/oracle.Options.Fingerprint at write
	// time. Informational; outside the digest.
	BuiltinOptionsFingerprint string `json:"builtin_options_fingerprint,omitempty"`
	// Executables records what each external oracle's cmd resolved to, and what
	// that file hashed to, at write time. Informational; OUTSIDE the digest, for
	// the reason given on ExecutableFingerprint. See D-060 and OQ-057.
	//
	// Omitted when empty so that a project with no external oracles, and a lock
	// written before this block existed, both stay byte-identical.
	Executables []ExecutableFingerprint `json:"executables,omitempty"`
	// Covers names the prothesis.yaml paths the manifest digests, so a reader
	// of the file can see the scope without reading this package.
	Covers []string `json:"covers"`
	// Limitations states, in the artifact itself, what this lock does NOT
	// guarantee. It is emitted rather than assumed because a reader who trusts
	// the lock for more than it provides is worse off than one with no lock.
	Limitations []string `json:"limitations"`
	// Manifest is the full projection, kept so that `verify` can say exactly
	// what moved instead of only that something did.
	Manifest Manifest `json:"manifest"`
}

// Limitations is the honest-limitation text embedded in every lock file.
//
// The first entry is the verdict-signing concern (v3) and is the one that
// matters: hashing a definition is not hashing an executable.
var Limitations = []string{
	"The DIGEST does not cover the EXECUTABLE an oracle names. Files under oracles.dir are " +
		"covered byte for byte; the binary, interpreter, shared library, container image or " +
		"PATH entry an oracle resolves at run time is NOT hashed into manifest_sha, so " +
		"swapping a checker does not produce ORACLE_DRIFT and never will: hashing it into " +
		"the digest would make an ordinary rebuild exit 4 and turn re-locking into a reflex. " +
		"Since D-060 the `executables` block above records each resolved program's SHA-256 " +
		"OUTSIDE the digest, and `verify` and every run report a moved or missing one as a " +
		"WARNING that also lands in the verdict. That removes the SILENCE OQ-057 measured; " +
		"it does not make a swap impossible, and nothing short of signing would. Only the " +
		"program is fingerprinted — not its arguments, its interpreter, or anything it loads.",
	"File modes are not covered. The executable bit does not survive a Windows checkout, so " +
		"hashing it would fire a spurious ORACLE_DRIFT on a cross-platform clone.",
	"Built-in oracle thresholds are compiled in and reachable from no config key. " +
		"builtin_options_fingerprint records them for review but is OUTSIDE the digest: " +
		"including it would turn any release that adjusts a default into exit 4 on every " +
		"downstream project at once. `thesis oracles verify` reports a moved fingerprint as a " +
		"warning, never as drift.",
	"driver.profiles (clients / ops / mix) is not covered. Shrinking a workload there is a " +
		"gate weakening this lock does not catch. Logged as OQ-026.",
	"The digest covers the USER-SUPPLIED prothesis.yaml with defaults NOT applied. A key you " +
		"did not write contributes nothing, so upgrading thesis cannot move this digest by " +
		"changing a compiled-in default.",
}

// Read loads the lock file for a project.
//
// A missing file returns ErrNoLock. A malformed file is an ERROR, never an
// absent lock: treating unparseable bytes as "no lock" would let a single
// corrupting write silently clear the drift check.
func Read(projectDir string) (*File, error) {
	p := Path(projectDir)
	data, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w (looked in %s)", ErrNoLock, p)
		}
		return nil, fmt.Errorf("lock: read %s: %w", p, err)
	}
	var f File
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("lock: %s is not a readable lock file: %w "+
			"(this is NOT treated as an absent lock: a corrupt lock must not clear the drift check)", p, err)
	}
	if f.Schema != FileSchema {
		return nil, fmt.Errorf("lock: %s declares schema %q, want %q", p, f.Schema, FileSchema)
	}
	if f.ManifestSHA == "" {
		return nil, fmt.Errorf("lock: %s carries no manifest_sha", p)
	}
	return &f, nil
}

// WriteOptions are the inputs to a lock bump.
type WriteOptions struct {
	ProjectDir string
	Manifest   Manifest
	// Reason is MANDATORY. A bump with no stated reason is indistinguishable
	// from an agent quietly re-baselining a gate it could not pass, which is
	// the exact behaviour invariant I6 exists to make visible.
	Reason string
	// ToolVersion is recorded, never hashed.
	ToolVersion string
	// BuiltinOptionsFingerprint is recorded, never hashed.
	BuiltinOptionsFingerprint string
	// Executables are the resolved-program fingerprints. Recorded, never hashed.
	Executables []ExecutableFingerprint
	// Now overrides the timestamp, for tests.
	Now time.Time
}

// ErrNoReason is returned when a lock bump carries no justification.
var ErrNoReason = errors.New("lock: --reason is mandatory: a lock bump with no stated reason is " +
	"indistinguishable from re-baselining a gate that could not be passed")

// Write writes (or refreshes) the project's lock file and returns the document
// it wrote.
func Write(opts WriteOptions) (*File, error) {
	if strings.TrimSpace(opts.Reason) == "" {
		return nil, ErrNoReason
	}
	sum, err := opts.Manifest.Digest()
	if err != nil {
		return nil, err
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	f := &File{
		Schema:                    FileSchema,
		ManifestSHA:               sum,
		Reason:                    strings.TrimSpace(opts.Reason),
		LockedAt:                  now.UTC().Format(time.RFC3339),
		ToolVersion:               opts.ToolVersion,
		BuiltinOptionsFingerprint: opts.BuiltinOptionsFingerprint,
		Executables:               append([]ExecutableFingerprint(nil), opts.Executables...),
		Covers:                    append([]string(nil), CoveredPaths...),
		Limitations:               append([]string(nil), Limitations...),
		Manifest:                  opts.Manifest,
	}

	dir := recorder.StateDir(opts.ProjectDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("lock: create %s: %w", dir, err)
	}
	buf := &bytes.Buffer{}
	enc := json.NewEncoder(buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(f); err != nil {
		return nil, fmt.Errorf("lock: encode lock file: %w", err)
	}
	p := Path(opts.ProjectDir)
	if err := os.WriteFile(p, buf.Bytes(), 0o644); err != nil {
		return nil, fmt.Errorf("lock: write %s: %w", p, err)
	}
	return f, nil
}
