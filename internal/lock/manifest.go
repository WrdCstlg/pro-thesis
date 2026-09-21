package lock

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/WrdCstlg/pro-thesis/internal/recorder/cjson"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// ManifestSchema identifies the manifest projection.
//
// It is carried INSIDE the digest preimage so that a future change to what the
// manifest covers cannot be mistaken for a change to what the user wrote: the
// version moves, the digest moves, and `verify` says which.
//
// ADDITIVE. The directive names `.prothesis/lock` and the verdict's
// `oracle_lock.manifest_sha` but fixes no internal schema for either, so this
// identifier is chosen to match the `prothesis.*` family. Logged as OQ-027.
const ManifestSchema = "prothesis.lock_manifest/v1"

// Manifest is the digested projection: what the lock actually covers.
//
// Every field is derived from bytes the USER wrote: the contents of files
// under oracles.dir, and the covered subset of prothesis.yaml with defaults NOT
// applied. Nothing compiled in reaches it. See the package doc, D-F and
// OQ-015.2.
//
// It is cjson-encodable by construction: no maps, no interfaces, no floats.
// Floats in particular are excluded on purpose: `search.exploration_constant`
// is carried as the raw scalar TEXT the user wrote, because Go's
// shortest-float representation carries no cross-version stability guarantee
// and a toolchain upgrade that reformatted 1.41 would be a spurious exit 4.
type Manifest struct {
	Schema string `json:"schema"`
	// Oracles is every file under oracles.dir, sorted by slash path.
	Oracles []OracleFile `json:"oracles"`
	// Config is the covered configuration leaves, sorted, exactly as written.
	// A key the user did not write has NO entry here.
	Config []ConfigEntry `json:"config"`
}

// OracleFile is one file under oracles.dir.
type OracleFile struct {
	// Path is relative to oracles.dir, slash-separated, so a lock written on
	// Windows verifies on Linux.
	Path string `json:"path"`
	// SHA256 is "sha256:"-prefixed over the file's exact bytes.
	SHA256 string `json:"sha256"`
	// Size is the byte length. Redundant with SHA256 for detection, kept
	// because a human reading a lock diff can use it.
	Size int64 `json:"size"`
}

// ConfigEntry is one covered configuration leaf.
//
// Kind separates the cases a bare string cannot: an explicit YAML null, an
// empty sequence and an empty mapping are all distinct from the string "".
type ConfigEntry struct {
	// Path is a dotted path such as "perturber.budget.max_faults_per_world".
	// A key that is not a plain identifier is quoted.
	Path string `json:"path"`
	// Kind is one of KindScalar, KindNull, KindEmptySeq, KindEmptyMap.
	Kind string `json:"kind"`
	// Value is the raw YAML scalar text, verbatim, with quoting removed by the
	// parser. Empty for every non-scalar kind.
	//
	// Raw text rather than a decoded value is deliberate. Decoding would drag
	// in the schema's typed parsers and, with them, its defaults; and a float
	// re-rendered by Go is not stable across toolchains. The cost is that
	// rewriting `90s` as `1m30s` moves the digest, which is a real edit to a
	// lock-covered field, and the diagnosis names it in exactly those words.
	Value string `json:"value"`
}

// Entry kinds.
const (
	KindScalar   = "scalar"
	KindNull     = "null"
	KindEmptySeq = "empty_seq"
	KindEmptyMap = "empty_map"
)

// Digest returns the "sha256:"-prefixed digest over the manifest's canonical
// encoding.
//
// cjson is used rather than encoding/json because this tree has exactly one
// definition of canonical bytes for anything that is hashed, and a second one
// would be a latent split-brain: two encoders that agree today and disagree
// after someone adds a field.
func (m Manifest) Digest() (string, error) {
	b, err := cjson.EncodeCompact(m)
	if err != nil {
		return "", fmt.Errorf("lock: encode manifest: %w", err)
	}
	sum := sha256.Sum256(b)
	return schema.HashPrefix + hex.EncodeToString(sum[:]), nil
}

// Build assembles the manifest for a project.
//
// configBytes is the RAW prothesis.yaml, undecoded. oraclesDir is the resolved
// directory to walk; it may be absolute or relative to projectDir, and it may
// not exist (a project with no external oracles is legitimate, and produces an
// empty file list rather than an error).
//
// It deliberately does NOT take a *schema.Config. Accepting one would put the
// compiled-in defaults within reach, and the whole point of D-F is that they
// are not.
func Build(projectDir string, configBytes []byte, oraclesDir string) (Manifest, error) {
	entries, err := ProjectConfig(configBytes)
	if err != nil {
		return Manifest{}, err
	}
	files, err := HashOracleDir(projectDir, oraclesDir)
	if err != nil {
		return Manifest{}, err
	}
	return Manifest{
		Schema:  ManifestSchema,
		Oracles: files,
		Config:  entries,
	}, nil
}

// HashOracleDir content-hashes every regular file under dir, sorted by slash
// path relative to dir.
//
// A missing directory yields an empty list and no error: a project may have no
// external oracles at all, and the lock still covers its config. A directory
// that exists but cannot be read IS an error: silently returning an empty list
// there would let an unreadable oracles.dir look identical to an empty one,
// which is a vacuous pass in the mechanism whose entire job is to notice
// removal.
func HashOracleDir(projectDir, dir string) ([]OracleFile, error) {
	if dir == "" {
		return []OracleFile{}, nil
	}
	abs := dir
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(projectDir, dir)
	}
	info, err := os.Stat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return []OracleFile{}, nil
		}
		return nil, fmt.Errorf("lock: stat oracles dir %s: %w", abs, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("lock: oracles dir %s is not a directory", abs)
	}

	out := []OracleFile{}
	walkErr := filepath.WalkDir(abs, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		// Regular files only. A symlink, socket or device under oracles.dir
		// cannot be content-hashed meaningfully, and following a symlink would
		// let a link swap change what runs without changing the digest.
		if !d.Type().IsRegular() {
			return fmt.Errorf("lock: %s is not a regular file (mode %s); "+
				"the lock cannot content-hash it, and a symlink swap would change what runs "+
				"without moving the digest", p, d.Type())
		}
		rel, rerr := filepath.Rel(abs, p)
		if rerr != nil {
			return rerr
		}
		sum, size, herr := hashFile(p)
		if herr != nil {
			return herr
		}
		out = append(out, OracleFile{
			Path:   filepath.ToSlash(rel),
			SHA256: sum,
			Size:   size,
		})
		return nil
	})
	if walkErr != nil {
		return nil, fmt.Errorf("lock: walk oracles dir %s: %w", abs, walkErr)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

func hashFile(p string) (string, int64, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", 0, fmt.Errorf("lock: open %s: %w", p, err)
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, fmt.Errorf("lock: read %s: %w", p, err)
	}
	return schema.HashPrefix + hex.EncodeToString(h.Sum(nil)), n, nil
}

// OracleDirFromConfig extracts `oracles.dir` as the user wrote it, falling back
// to schema.DefaultOraclesDir when the key is absent.
//
// The FALLBACK is used only to decide which directory to WALK. It never reaches
// the manifest: `oracles.dir` appears in Config only when the user wrote it, so
// a future change to DefaultOraclesDir moves no digest by itself. It would move
// the file list if the two directories held different files, which is honest:
// different oracles would then actually be in force.
func OracleDirFromConfig(configBytes []byte) (string, error) {
	entries, err := ProjectConfig(configBytes)
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		if e.Path == "oracles.dir" && e.Kind == KindScalar && strings.TrimSpace(e.Value) != "" {
			return e.Value, nil
		}
	}
	return schema.DefaultOraclesDir, nil
}
