package corpus

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/WrdCstlg/pro-thesis/internal/recorder"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// RegressionsDirName is the corpus directory inside `.prothesis/`.
//
// The directive names the path in its own worked example
// (`.prothesis/regressions/w_a41f.thesis`, section 4.6) and `.gitignore` already
// carries the note that it is NOT ignored. Nothing here is invented.
const RegressionsDirName = "regressions"

// ReadmeFileName is the corpus's own README, written when the directory is
// created.
//
// It exists at the one place somebody would stand while deciding to delete a
// file. The rule it states (removal is a lock change) is D-004's, not this
// package's, and List ignores it: only `w_xxxx.thesis` names are corpus entries.
const ReadmeFileName = "README.md"

// Dir returns the corpus directory for a project root.
func Dir(projectDir string) string {
	return filepath.Join(recorder.StateDir(projectDir), RegressionsDirName)
}

// ---------------------------------------------------------------------------
// The confirmation gate
// ---------------------------------------------------------------------------

// Confirmation is the measured result of the k/k confirmation gate.
//
// It is a plain value rather than a reference into internal/shrink so that the
// corpus can be exercised (and the gate pinned by a test) without the
// minimization pipeline. The pipeline produces one of these; so does
// `thesis replay`.
type Confirmation struct {
	// Attempts is k: how many confirmation replays were EXECUTED. An attempt
	// that could not be conducted at all (a dead daemon, a world that never
	// reached DRIVE) is not one of these; see Inconclusive.
	Attempts int
	// Reproduced is how many of those attempts reproduced THE SAME violation,
	// judged against the original violation's identity and not against "did
	// something fail".
	Reproduced int
	// Inconclusive is how many replays could not be judged either way. They are
	// counted separately and never silently folded into Reproduced or into its
	// complement: rounding an unevaluable replay toward "reproduced" manufactures
	// evidence, and rounding it toward "did not" manufactures flakiness.
	Inconclusive int
}

// String renders the gate's answer as the verdict's own "k/n" spelling.
func (c Confirmation) String() string {
	s := fmt.Sprintf("%d/%d", c.Reproduced, c.Attempts)
	if c.Inconclusive > 0 {
		s += fmt.Sprintf(" (%d inconclusive)", c.Inconclusive)
	}
	return s
}

// Passed reports whether the gate was met: at least one attempt, every attempt
// reproduced, and none was inconclusive.
//
// The "none inconclusive" clause is not pedantry. An inconclusive replay is a
// replay nobody measured, and k/k over a set that includes one is a claim about
// fewer executions than it names.
func (c Confirmation) Passed() bool {
	return c.Attempts > 0 && c.Reproduced == c.Attempts && c.Inconclusive == 0
}

// ErrNotConfirmed reports a world refused entry to the corpus because it did not
// reproduce k/k.
type ErrNotConfirmed struct {
	Confirmation Confirmation
}

func (e *ErrNotConfirmed) Error() string {
	switch {
	case e.Confirmation.Attempts == 0:
		return "corpus: refusing to commit a world whose confirmation gate never ran. " +
			"A world nobody replayed is not evidence, and every future `thesis regress` " +
			"would spend a world on it to learn nothing"
	case e.Confirmation.Reproduced == 0:
		return fmt.Sprintf("corpus: refusing to commit a world that reproduced %s. "+
			"A world that never reproduces makes `thesis regress` report PASS whatever the "+
			"system under test does, which is a green gate that checked nothing",
			e.Confirmation)
	default:
		return fmt.Sprintf("corpus: refusing to commit a world that reproduced %s rather than %d/%d. "+
			"`thesis regress` reports PASS when a corpus world does not reproduce, so a flaky "+
			"entry manufactures a FALSE GREEN at that rate. Shrink further, or confirm at a k "+
			"you are willing to publish — the measured number is reported either way and is "+
			"never rounded up",
			e.Confirmation, e.Confirmation.Attempts, e.Confirmation.Attempts)
	}
}

// ---------------------------------------------------------------------------
// The corpus
// ---------------------------------------------------------------------------

// Corpus is the append-only regression store at `.prothesis/regressions/`.
//
// Note what is absent: there is no Remove, no Replace, no Prune and no Truncate.
// Deleting a regression is a lock change and therefore a human's commit, not an
// API call.
type Corpus struct {
	dir string
}

// Open returns the corpus for a project root WITHOUT creating it.
//
// A project that has never shrunk anything has no corpus directory, and that is
// a legitimate state that `thesis regress` reports as "there are no
// regressions". Creating it as a side effect of reading would turn a question
// into a write.
func Open(projectDir string) *Corpus { return &Corpus{dir: Dir(projectDir)} }

// OpenDir returns the corpus rooted at an explicit directory. Tests use it; so
// does anyone relocating the corpus deliberately.
func OpenDir(dir string) *Corpus { return &Corpus{dir: dir} }

// Dir is the corpus directory.
func (c *Corpus) Dir() string { return c.dir }

// Exists reports whether the corpus directory is present.
func (c *Corpus) Exists() bool {
	fi, err := os.Stat(c.dir)
	return err == nil && fi.IsDir()
}

// Create makes the corpus directory and writes its README if either is missing.
// It never touches an existing README.
func (c *Corpus) Create() error {
	if err := os.MkdirAll(c.dir, 0o755); err != nil {
		return fmt.Errorf("corpus: create %s: %w", c.dir, err)
	}
	p := filepath.Join(c.dir, ReadmeFileName)
	if _, err := os.Stat(p); err == nil {
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("corpus: stat %s: %w", p, err)
	}
	return os.WriteFile(p, []byte(readme), 0o644)
}

// Entry is one committed regression world.
type Entry struct {
	// Name is the file name, `w_xxxx.thesis`.
	Name string
	// Path is the absolute file path.
	Path string
	// ShortID is the four hex digits in the name.
	ShortID string
	// World is the decoded world. It is never nil in a List result: a file that
	// does not decode is reported through Err instead.
	World *schema.World
	// Hash is the world's content address.
	Hash string
	// Err is set when the file could not be read or did not decode. Such an
	// entry is still LISTED, because a corrupt regression is a fact about the
	// corpus that a gate must report rather than skip.
	Err error
}

// OK reports whether this entry decoded.
func (e Entry) OK() bool { return e.Err == nil && e.World != nil }

// List returns every corpus entry in DETERMINISTIC order: sorted by file name,
// which is the world's own content address.
//
// A missing directory is not an error. It returns no entries, and the caller is
// expected to say "there are no regressions" rather than "0 regressions passed":
// those are different facts and only one of them is evidence.
func (c *Corpus) List() ([]Entry, error) {
	ents, err := os.ReadDir(c.dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("corpus: read %s: %w", c.dir, err)
	}

	var names []string
	for _, de := range ents {
		if de.IsDir() {
			continue
		}
		// A README, a note, or an editor's backup file legitimately lives here
		// and is not a world. But a name that CLAIMS to be a world (the `w_`
		// prefix, or `.thesis` anywhere in it in any case) must parse as one.
		// Silently skipping w_bad.thesis, w_A41F.thesis or w_e952.thesis.orig
		// would let a mistyped, merge-damaged or corrupt regression disappear
		// from regress, converting a real failure into a vacuous pass (OQ-062).
		// A suffix test alone missed the last two of those.
		name := de.Name()
		if strings.HasPrefix(name, schema.WorldIDPrefix) ||
			strings.Contains(strings.ToLower(name), schema.WorldFileExt) {
			names = append(names, name)
		}
	}
	sort.Strings(names)

	out := make([]Entry, 0, len(names))
	for _, name := range names {
		p := filepath.Join(c.dir, name)
		shortID, err := schema.ParseWorldFilename(name)
		if err != nil {
			out = append(out, Entry{
				Name: name,
				Path: p,
				Err:  fmt.Errorf("corpus: invalid regression filename %q: %w", name, err),
			})
			continue
		}
		e := Entry{Name: name, Path: p, ShortID: shortID}
		w, err := recorder.LoadWorld(p)
		if err != nil {
			e.Err = err
			out = append(out, e)
			continue
		}
		h, err := recorder.HashWorld(w)
		if err != nil {
			e.Err = err
			out = append(out, e)
			continue
		}
		// The short id in the filename is the content address (D-012 / D-063).
		// A world whose content hash does not match its filename is corrupt
		// and must not be accepted or executed under the wrong identity.
		wantShort, err := schema.ShortIDFromHash(h)
		if err != nil {
			e.Err = fmt.Errorf("corpus: %s content hash %s: %w", name, h, err)
			out = append(out, e)
			continue
		}
		if wantShort != shortID {
			e.Err = fmt.Errorf("corpus: %s content hash %s does not match filename short-id %s (want %s)",
				name, h, shortID, wantShort)
			out = append(out, e)
			continue
		}
		e.World = w
		e.Hash = h
		out = append(out, e)
	}
	return out, nil
}

// Add commits a shrunk world to the corpus, refusing everything that would put a
// world in it that should not be there.
//
// The order of the checks is the order of their costs. The confirmation gate is
// first because it is the one that decides whether this world belongs in a
// committed artifact at all; everything after it is about writing the file
// correctly.
//
// It returns the entry's path. Adding the identical world twice is a no-op and
// returns the existing path with no error, so a re-run of a shrink that already
// committed its answer does not fail.
func (c *Corpus) Add(w *schema.World, conf Confirmation) (string, error) {
	if w == nil {
		return "", errors.New("corpus: Add(nil)")
	}
	if !conf.Passed() {
		return "", &ErrNotConfirmed{Confirmation: conf}
	}
	if err := w.Validate(); err != nil {
		return "", fmt.Errorf("corpus: refusing to commit a world that fails validation: %w", err)
	}
	// A world with no realized schedule has never been executed, so nothing
	// measured it and the confirmation it arrived with describes some other
	// world. This cannot happen through the replay path (the confirmation is
	// produced by executing this very world) and refusing it is what keeps that
	// true when a future caller assembles one by hand.
	if w.FaultSchedule.Realized == nil {
		return "", errors.New("corpus: refusing to commit a world whose fault_schedule.realized is null. " +
			"Null means the world has never been executed, so no confirmation can be about it " +
			"(an executed world that injected nothing records [], which is a different statement)")
	}

	if err := c.Create(); err != nil {
		return "", err
	}

	// StoreWorldNoClobber is the append-only enforcement, and it lives in the
	// recorder rather than here because it is the same rule for every content-
	// addressed world file: an identical world is a no-op, a DIFFERENT world
	// under the same four-hex name is an error naming both hashes, and a file
	// that exists but will not load is refused rather than overwritten, because
	// overwriting an unreadable committed world destroys the evidence that it was
	// corrupted.
	path, err := recorder.StoreWorldNoClobber(c.dir, w)
	if err != nil {
		return "", err
	}

	// Prove what was written, from the bytes on disk rather than from the value
	// in memory. LoadWorld re-encodes and byte-compares (D-012 rule 8), so this
	// also proves the file is canonical; the CR scan proves it is LF-only, which
	// is what stops a Windows checkout from breaking every world_hash in the
	// corpus at once.
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("corpus: read back %s: %w", path, err)
	}
	if bytes.IndexByte(data, '\r') >= 0 {
		return "", fmt.Errorf("corpus: %s contains a CR byte; a `.thesis` file must be LF-only "+
			"(check .gitattributes `*.thesis text eol=lf`)", path)
	}
	if _, err := recorder.LoadWorld(path); err != nil {
		return "", fmt.Errorf("corpus: %s did not survive a round trip: %w", path, err)
	}
	return path, nil
}

const readme = `# Regression corpus

Every ` + "`w_xxxx.thesis`" + ` file here is a world that PRO-THESIS shrank from a real
violation and then CONFIRMED reproduces. ` + "`thesis regress`" + ` replays all of them on
every gate invocation.

**This directory is append-only, and it is committed on purpose.**
` + "`.gitignore`" + ` excludes ` + "`.prothesis/runs/`" + ` but deliberately does not exclude this
directory (DECISIONS.md D-004). Removing a ` + "`.thesis`" + ` file from it is a LOCK CHANGE
under invariant I6 and anti-gaming rule 4: it deletes a regression test. If a
regression is genuinely obsolete, that is a human-reviewed commit that says so,
not a cleanup.

Two rules the tooling enforces so you do not have to:

- a world that did not reproduce k/k is REFUSED entry, because
  ` + "`thesis regress`" + ` reports PASS when a corpus world does not reproduce, and a
  flaky entry manufactures a false green at exactly its flake rate;
- a DIFFERENT world wanting a filename already taken is an error, never a silent
  overwrite. Filenames are the first four hex digits of the world hash, so a
  collision is possible; losing a regression to one is not.

The files are LF-only and byte-identical to their canonical encoding. Do not
hand-edit them: the loader re-encodes and byte-compares on every read, so an
edited world is rejected rather than silently re-hashed.
`
