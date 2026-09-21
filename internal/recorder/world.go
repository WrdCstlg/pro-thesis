package recorder

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/WrdCstlg/pro-thesis/internal/recorder/cjson"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// The `.thesis` codec.
//
// pkg/schema owns the world TYPE (schema.World and invariant I2's tuple);
// internal/recorder owns the CODEC and the file I/O, per the Phase 0 package
// split in the build brief. Nothing here invents a field.
//
// Phase 0 definition of done (b) is "a world configuration round-trips through
// `.thesis` serialization byte-identically". Read as "for every parseable JSON
// document" that is mathematically impossible; {"a":1,"b":2} and {"b":2,"a":1}
// denote the same world and no single encoder reproduces both. So the canonical
// form is defined (package cjson), a valid `.thesis` file is defined as exactly
// the output of the canonical encoder, and the loader REJECTS non-canonical
// input rather than silently normalizing it.
//
// Silent normalization would be a hole in invariant I6: a hand-edited
// regression world would rewrite to a different identity and the corpus would
// drift with nobody noticing.

// ErrHashMismatch reports that a world's recomputed identity does not match an
// expected value.
type ErrHashMismatch struct {
	Want string
	Got  string
}

func (e *ErrHashMismatch) Error() string {
	return fmt.Sprintf("recorder: world hash mismatch: want %s, got %s", e.Want, e.Got)
}

// EncodeWorld returns the canonical bytes of w, including the single trailing
// LF.
func EncodeWorld(w *schema.World) ([]byte, error) {
	if w == nil {
		return nil, errors.New("recorder: EncodeWorld(nil)")
	}
	b, err := cjson.Encode(*w)
	if err != nil {
		return nil, fmt.Errorf("recorder: encode world: %w", err)
	}
	return b, nil
}

// DecodeWorld parses canonical `.thesis` bytes.
//
// It fails unless the document satisfies the framing rules (no BOM, no CR,
// exactly one trailing LF, no trailing data), decodes with no unknown fields,
// is bytewise equal to the canonical encoding of what it decoded, and passes
// schema validation.
//
// The byte-comparison is the point. Byte-identity stops being an assertion in a
// test suite and becomes a runtime invariant, checked on every load, for every
// regression world, on every gate invocation. A subtle encoder or decoder bug
// cannot be silent.
func DecodeWorld(b []byte) (*schema.World, error) {
	var w schema.World
	if err := cjson.UnmarshalCanonical(b, &w); err != nil {
		return nil, err
	}
	if err := w.Validate(); err != nil {
		return nil, err
	}
	return &w, nil
}

// DecodeWorldUnchecked parses a `.thesis` document WITHOUT the byte-identity
// gate and WITHOUT schema validation.
//
// It exists for exactly two callers: a repair path that canonicalizes a
// hand-edited world, and the test suite. The tests need it because a round-trip
// test written against DecodeWorld alone is VACUOUS: DecodeWorld internally
// performs the very comparison such a test would assert, so the assertion is
// unreachable unless the encoder is nondeterministic between two consecutive
// calls on one value. It could not detect a decoder that drops a field, a
// decoder that mis-parses a value, or a lossy encode/decode pair: all three
// surface as a canonicality error inside DecodeWorld and hit the test's
// "input was rejected, skip" branch. Real byte-identity evidence has to come
// from decoding WITHOUT the gate and comparing a re-encoding against bytes read
// back from disk.
//
// The framing and unknown-field rules still apply: they are properties of the
// document, not of its canonicality.
func DecodeWorldUnchecked(b []byte) (*schema.World, error) {
	var w schema.World
	if err := cjson.Unmarshal(b, &w); err != nil {
		return nil, err
	}
	return &w, nil
}

// HashWorld returns the world's content address.
//
// It delegates to schema.World.Hash so that the system has exactly ONE
// definition of world identity, owned by the package that owns the type. The
// hash covers a frozen projection equal to invariant I2's five-tuple plus
// `sut`; `schema` and `meta` are excluded, so provenance never changes what a
// world IS and the same world discovered on two commits dedupes.
func HashWorld(w *schema.World) (string, error) {
	if w == nil {
		return "", errors.New("recorder: HashWorld(nil)")
	}
	return w.Hash()
}

// WorldFileName returns the content-addressed file name for w.
//
// It delegates to pkg/schema, which owns identifier shapes, so the tree has one
// naming scheme rather than two. Note that a short id is 4 hex digits: it is a
// filename convenience, NOT an identity, and two different worlds can share one.
// StoreWorldNoClobber is what stops that from silently deleting a committed
// regression.
func WorldFileName(w *schema.World) (string, error) {
	if w == nil {
		return "", errors.New("recorder: WorldFileName(nil)")
	}
	return schema.WorldFilenameFor(w)
}

// ErrWorldFileCollision reports that two DIFFERENT worlds want the same
// content-addressed filename.
//
// A short id is 4 hex digits, so by the birthday bound a collision becomes
// likely at a few hundred files, and the regression corpus is specified as
// append-only and committed to the repository. Silently overwriting one
// regression world with another is functionally the deletion of a regression
// test, which invariant I6 exists to prevent, so it is an error, loudly, at the
// moment of the write.
type ErrWorldFileCollision struct {
	Path     string
	Existing string
	Incoming string
}

func (e *ErrWorldFileCollision) Error() string {
	return fmt.Sprintf("recorder: %s already holds a different world (%s); refusing to overwrite it with %s. "+
		"Two worlds share a %d-hex short id; rename one explicitly rather than losing a regression",
		e.Path, e.Existing, e.Incoming, schema.ShortIDLen)
}

// StoreWorldNoClobber writes w into dir under its content-addressed filename,
// refusing to replace a different world that already occupies that name.
//
// Writing the same world twice is a no-op and returns no error, so callers may
// be re-run freely.
func StoreWorldNoClobber(dir string, w *schema.World) (string, error) {
	name, err := WorldFileName(w)
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, name)
	incoming, err := HashWorld(w)
	if err != nil {
		return "", err
	}
	switch existing, err := LoadWorld(path); {
	case err == nil:
		have, herr := HashWorld(existing)
		if herr != nil {
			return "", herr
		}
		if have == incoming {
			return path, nil // already stored, byte for byte
		}
		return "", &ErrWorldFileCollision{Path: path, Existing: have, Incoming: incoming}
	case errors.Is(err, fs.ErrNotExist):
		// The normal case: nothing is there.
	default:
		// Something is there and will not load. Refusing is the safe answer:
		// overwriting an unreadable committed world destroys the evidence that
		// it was corrupted.
		return "", fmt.Errorf("recorder: %s exists but does not load; refusing to overwrite it: %w", path, err)
	}
	return path, StoreWorld(path, w)
}

// LoadWorld reads and decodes a `.thesis` file.
func LoadWorld(path string) (*schema.World, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("recorder: load world: %w", err)
	}
	w, err := DecodeWorld(b)
	if err != nil {
		return nil, fmt.Errorf("recorder: load world %s: %w", path, err)
	}
	return w, nil
}

// LoadWorldExpecting is LoadWorld plus an identity check against wantHash. A
// committed regression world that no longer hashes to what the corpus recorded
// is a corpus integrity failure, not a load error, so it is reported
// distinctly.
func LoadWorldExpecting(path, wantHash string) (*schema.World, error) {
	w, err := LoadWorld(path)
	if err != nil {
		return nil, err
	}
	got, err := HashWorld(w)
	if err != nil {
		return nil, err
	}
	if got != wantHash {
		return nil, &ErrHashMismatch{Want: wantHash, Got: got}
	}
	return w, nil
}

// StoreWorld validates, encodes and atomically writes w to path.
func StoreWorld(path string, w *schema.World) error {
	if w == nil {
		return errors.New("recorder: StoreWorld(nil)")
	}
	if err := w.Validate(); err != nil {
		return fmt.Errorf("recorder: refusing to store an invalid world at %s: %w", path, err)
	}
	b, err := EncodeWorld(w)
	if err != nil {
		return err
	}
	return WriteFileAtomic(path, b)
}

// ---------------------------------------------------------------------------
// Atomic file writes
// ---------------------------------------------------------------------------

// atomicWriteBackoff bounds the rename retry; its length is the attempt count.
var atomicWriteBackoff = []time.Duration{
	10 * time.Millisecond,
	25 * time.Millisecond,
	50 * time.Millisecond,
	100 * time.Millisecond,
	200 * time.Millisecond,
}

// WriteFileAtomic writes data to a temporary file in the same directory, fsyncs
// it, and renames it over path: retrying the rename.
//
// The retry is not defensive padding. On Windows, os.Rename is MoveFileEx with
// MOVEFILE_REPLACE_EXISTING, which fails with ERROR_ACCESS_DENIED or
// ERROR_SHARING_VIOLATION whenever the target is open without FILE_SHARE_DELETE:
// Defender scanning the file just written, an editor holding a run artifact
// open, or Docker Desktop touching a bind-mounted log. That is the single most
// common transient failure in an artifacts directory on this platform, and
// without a retry it loses the artifact outright.
//
// Every error is treated as retryable. Retrying a genuinely permanent failure
// costs 385 ms and then reports the real error; not retrying a transient one
// loses data.
//
// On final failure the temporary file is deliberately LEFT IN PLACE and named
// in the error, so the bytes are recoverable rather than discarded.
func WriteFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("recorder: create %s: %w", dir, err)
	}
	f, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("recorder: create temp near %s: %w", path, err)
	}
	tmp := f.Name()
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("recorder: write %s: %w", tmp, err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("recorder: sync %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("recorder: close %s: %w", tmp, err)
	}

	var last error
	for i := 0; i < len(atomicWriteBackoff); i++ {
		if last = os.Rename(tmp, path); last == nil {
			return nil
		}
		time.Sleep(atomicWriteBackoff[i])
	}
	return fmt.Errorf("recorder: rename %s -> %s failed after %d attempts; "+
		"the written bytes are preserved at %s: %w", tmp, path, len(atomicWriteBackoff), tmp, last)
}
