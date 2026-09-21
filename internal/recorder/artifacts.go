package recorder

import (
	"bufio"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/WrdCstlg/pro-thesis/internal/recorder/cjson"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// The `.prothesis/` artifact tree.
//
//	.prothesis/
//	├── lock                                 (Phase 3)
//	├── oracles/                             (Phase 3)
//	├── regressions/                         (Phase 5, committed to the repo)
//	└── runs/
//	    ├── CURRENT                          run id of the live topology, if any
//	    └── r_2026_09_03_a41f/
//	        ├── RUNNING                      sentinel; absent => clean teardown
//	        ├── MANIFEST.json                sha256 + size of every file
//	        ├── clock.json                   anchor, calibration, drift
//	        ├── topology.json                the up -> down handoff
//	        └── worlds/
//	            └── 000000/
//	                ├── world.thesis
//	                ├── world_hash           one line, for grep and scripts
//	                └── logs/
//
// World directories are keyed by a six-digit ORDINAL rather than by world hash,
// because a later phase's confirmation gate executes the same world k times, so
// the hash is not unique within a run.

const (
	// StateDirName is the per-project state directory.
	StateDirName = ".prothesis"
	// RunsDirName holds one directory per run.
	RunsDirName = "runs"
	// RunningSentinel marks a run directory as live. Its absence means the run
	// was torn down cleanly.
	RunningSentinel = "RUNNING"
	// CurrentPointer names the file holding the run id of the currently live
	// topology. It lives inside runs/ so the existing .gitignore covers it.
	CurrentPointer = "CURRENT"
	// ManifestFileName is the per-run content manifest.
	ManifestFileName = "MANIFEST.json"
	// TopologyFileName is the up -> down handoff record.
	TopologyFileName = "topology.json"
	// ClockFileName holds the run's clock anchor and health.
	ClockFileName = "clock.json"
	// WorldsDirName holds one directory per executed world.
	WorldsDirName = "worlds"
	// WorldFileBase is the world file inside a world directory.
	WorldFileBase = "world" + schema.WorldFileExt
	// WorldHashFileName is a one-line, greppable copy of the world's identity.
	WorldHashFileName = "world_hash"
)

// ADDITIVE schema identifiers. The directive names no schema id for these
// files; without one there is no way to version-gate the format.
const (
	ManifestSchema = "prothesis.manifest/v1"
	TopologySchema = "prothesis.topology/v1"
)

// StateDir returns the .prothesis directory for a project root.
func StateDir(projectDir string) string { return filepath.Join(projectDir, StateDirName) }

// RunsDir returns the runs directory for a project root.
func RunsDir(projectDir string) string { return filepath.Join(StateDir(projectDir), RunsDirName) }

// ---------------------------------------------------------------------------
// run_id
// ---------------------------------------------------------------------------

// RunIDLen is the length of a run id: r_2026_09_03_a41f is seventeen
// characters. It carries no colon, so it is a legal path component on every
// platform.
const RunIDLen = len("r_2026_09_03_a41f")

// ValidRunID reports whether s has the normative run id shape.
//
// It delegates to pkg/schema, which owns identifier shapes. Allocation stays
// here, because it is inseparable from creating the directory.
func ValidRunID(s string) bool { return schema.ValidRunID(s) }

// allocRunIDAttempts bounds the retry on directory collision.
const allocRunIDAttempts = 64

// AllocRunID creates a fresh run directory under runsDir and returns its id.
//
// The date is UTC, not local: local dates make CI runs sort incoherently across
// time zones and produce two "2026_09_03" folders around midnight. The four hex
// characters come from crypto/rand rather than the seeded stream, because a run
// id identifies a RUN and two runs of the same world are two runs.
//
// Uniqueness is not a probabilistic claim: os.Mkdir fails atomically if the
// directory exists, so a collision is detected exactly and retried with fresh
// entropy. That keeps the normative four-hex shape while removing the birthday
// collision sixteen bits would otherwise give a long-lived daemon.
func AllocRunID(runsDir string, wallUTC time.Time) (runID, dir string, err error) {
	if err := os.MkdirAll(runsDir, 0o755); err != nil {
		return "", "", fmt.Errorf("recorder: create runs dir %s: %w", runsDir, err)
	}
	y, m, d := wallUTC.UTC().Date()
	prefix := fmt.Sprintf("r_%04d_%02d_%02d_", y, int(m), d)
	var buf [2]byte
	for i := 0; i < allocRunIDAttempts; i++ {
		if _, err := rand.Read(buf[:]); err != nil {
			return "", "", fmt.Errorf("recorder: read entropy for run id: %w", err)
		}
		id := prefix + hex.EncodeToString(buf[:])
		p := filepath.Join(runsDir, id)
		err := os.Mkdir(p, 0o755)
		if err == nil {
			return id, p, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return "", "", fmt.Errorf("recorder: create run dir %s: %w", p, err)
		}
	}
	return "", "", fmt.Errorf("recorder: could not allocate a free run id under %s after %d attempts",
		runsDir, allocRunIDAttempts)
}

// ---------------------------------------------------------------------------
// Path safety
// ---------------------------------------------------------------------------

var safeNamePattern = regexp.MustCompile(`^[A-Za-z0-9_.\-]{1,64}$`)

// windowsReserved are device names that NTFS resolves specially. Creating a
// file called "con.log" fails silently rather than producing a debuggable
// error, so node ids that reach a log filename are checked up front.
var windowsReserved = map[string]bool{
	"con": true, "prn": true, "aux": true, "nul": true,
	"com1": true, "com2": true, "com3": true, "com4": true, "com5": true,
	"com6": true, "com7": true, "com8": true, "com9": true,
	"lpt1": true, "lpt2": true, "lpt3": true, "lpt4": true, "lpt5": true,
	"lpt6": true, "lpt7": true, "lpt8": true, "lpt9": true,
}

// SafeName validates an identifier for use as a path component on any platform
// this tool supports, Windows included.
func SafeName(s string) error {
	if !safeNamePattern.MatchString(s) {
		return fmt.Errorf("recorder: %q is not a safe path component "+
			"(want 1-64 characters from A-Z a-z 0-9 _ . -)", s)
	}
	if strings.HasSuffix(s, ".") || strings.HasSuffix(s, " ") {
		return fmt.Errorf("recorder: %q ends with a dot or space, which Windows silently strips", s)
	}
	base := strings.ToLower(s)
	if i := strings.IndexByte(base, '.'); i >= 0 {
		base = base[:i]
	}
	if windowsReserved[base] {
		return fmt.Errorf("recorder: %q is a reserved Windows device name", s)
	}
	return nil
}

// checkRel validates a bundle-relative path: forward slashes only, no absolute
// paths, no "..", and every component individually safe.
func checkRel(rel string) error {
	if rel == "" {
		return errors.New("recorder: empty relative path")
	}
	if strings.ContainsRune(rel, '\\') {
		return fmt.Errorf("recorder: relative path %q must use forward slashes", rel)
	}
	if path.IsAbs(rel) || filepath.IsAbs(rel) {
		return fmt.Errorf("recorder: relative path %q must not be absolute", rel)
	}
	for _, seg := range strings.Split(rel, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return fmt.Errorf("recorder: relative path %q has an illegal segment %q", rel, seg)
		}
		if err := SafeName(seg); err != nil {
			return fmt.Errorf("recorder: relative path %q: %w", rel, err)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Bundle
// ---------------------------------------------------------------------------

// Bundle owns one run directory. Every artifact write goes through it, so
// atomicity, path sanitation and the manifest cannot drift apart.
//
// A Bundle obtained from WorldDir shares the run's state; only the root bundle
// may be closed.
type Bundle struct {
	runDir string
	runID  string
	sub    string // "" for the run root, else a forward-slash relative path
	st     *bundleState
}

type bundleState struct {
	mu        sync.Mutex
	closed    bool
	appenders []*Appender
}

// OpenBundle allocates a new run directory under runsDir and marks it RUNNING.
func OpenBundle(runsDir string, wallUTC time.Time) (*Bundle, error) {
	runID, dir, err := AllocRunID(runsDir, wallUTC)
	if err != nil {
		return nil, err
	}
	b := &Bundle{runDir: dir, runID: runID, st: &bundleState{}}
	if err := os.WriteFile(filepath.Join(dir, RunningSentinel), []byte(runID+"\n"), 0o644); err != nil {
		return nil, fmt.Errorf("recorder: write %s: %w", RunningSentinel, err)
	}
	return b, nil
}

// OpenBundleAt reopens an existing run directory.
//
// This is what makes `thesis down` possible at all: `up` and `down` are
// separate OS processes, so `down` cannot inherit anything in memory. It
// reattaches to the directory `up` left behind and reads the persisted
// topology from it.
func OpenBundleAt(runsDir, runID string) (*Bundle, error) {
	if !ValidRunID(runID) {
		return nil, fmt.Errorf("recorder: %q is not a valid run id (want %s)", runID, schema.RunIDShape)
	}
	dir := filepath.Join(runsDir, runID)
	fi, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("recorder: open run %s: %w", runID, err)
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("recorder: %s is not a directory", dir)
	}
	return &Bundle{runDir: dir, runID: runID, st: &bundleState{}}, nil
}

// RunID returns the run id.
func (b *Bundle) RunID() string { return b.runID }

// Dir returns the absolute-or-as-given directory this bundle writes into.
func (b *Bundle) Dir() string {
	if b.sub == "" {
		return b.runDir
	}
	return filepath.Join(b.runDir, filepath.FromSlash(b.sub))
}

// RunDir returns the run root directory, even for a sub-bundle.
func (b *Bundle) RunDir() string { return b.runDir }

// Path resolves a bundle-relative path.
func (b *Bundle) Path(rel string) (string, error) {
	if err := checkRel(rel); err != nil {
		return "", err
	}
	return filepath.Join(b.Dir(), filepath.FromSlash(rel)), nil
}

// WriteJSON canonically encodes v and atomically replaces rel.
func (b *Bundle) WriteJSON(rel string, v any) error {
	data, err := cjson.Encode(v)
	if err != nil {
		return fmt.Errorf("recorder: encode %s: %w", rel, err)
	}
	return b.WriteFile(rel, data)
}

// WriteFile atomically replaces rel with data.
func (b *Bundle) WriteFile(rel string, data []byte) error {
	p, err := b.Path(rel)
	if err != nil {
		return err
	}
	return WriteFileAtomic(p, data)
}

// ReadFile reads rel.
func (b *Bundle) ReadFile(rel string) ([]byte, error) {
	p, err := b.Path(rel)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(p)
}

// WorldDir allocates worlds/<6-digit ordinal>/ and returns a sub-bundle sharing
// this run's state.
func (b *Bundle) WorldDir(ordinal int) (*Bundle, error) {
	if ordinal < 0 || ordinal > 999999 {
		return nil, fmt.Errorf("recorder: world ordinal %d is outside [0, 999999]", ordinal)
	}
	sub := fmt.Sprintf("%s/%06d", WorldsDirName, ordinal)
	dir := filepath.Join(b.runDir, filepath.FromSlash(sub))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("recorder: create %s: %w", dir, err)
	}
	return &Bundle{runDir: b.runDir, runID: b.runID, sub: sub, st: b.st}, nil
}

// StoreWorld writes world.thesis plus the greppable world_hash sidecar.
func (b *Bundle) StoreWorld(w *schema.World) error {
	p, err := b.Path(WorldFileBase)
	if err != nil {
		return err
	}
	if err := StoreWorld(p, w); err != nil {
		return err
	}
	h, err := HashWorld(w)
	if err != nil {
		return err
	}
	return b.WriteFile(WorldHashFileName, []byte(h+"\n"))
}

// Appender returns a crash-safe append-only writer for rel.
//
// The returned Appender is safe for concurrent use; a JSONL file with
// interleaved partial lines is unparseable, and one torn line makes an entire
// history log useless.
func (b *Bundle) Appender(rel string) (*Appender, error) {
	p, err := b.Path(rel)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return nil, fmt.Errorf("recorder: create %s: %w", filepath.Dir(p), err)
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("recorder: open %s: %w", p, err)
	}
	a := &Appender{f: f, w: bufio.NewWriter(f), path: p}
	b.st.mu.Lock()
	b.st.appenders = append(b.st.appenders, a)
	b.st.mu.Unlock()
	return a, nil
}

// Close flushes every appender, writes MANIFEST.json, and removes the RUNNING
// sentinel. Only the run root bundle may be closed. It is idempotent.
func (b *Bundle) Close() error {
	if b.sub != "" {
		return errors.New("recorder: only the run root bundle may be closed")
	}
	b.st.mu.Lock()
	if b.st.closed {
		b.st.mu.Unlock()
		return nil
	}
	b.st.closed = true
	apps := append([]*Appender(nil), b.st.appenders...)
	b.st.mu.Unlock()

	var firstErr error
	for _, a := range apps {
		if err := a.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if err := b.WriteManifest(); err != nil && firstErr == nil {
		firstErr = err
	}
	if err := os.Remove(filepath.Join(b.runDir, RunningSentinel)); err != nil &&
		!errors.Is(err, fs.ErrNotExist) && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

// ---------------------------------------------------------------------------
// Manifest
// ---------------------------------------------------------------------------

// ManifestEntry is one file's content address.
type ManifestEntry struct {
	Path   string `json:"path"`
	Sha256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// Manifest is the per-run content manifest.
//
// BundleSHA is computed over the canonical encoding of the sorted file list,
// never over a compressed archive: compress/flate's output is not guaranteed
// stable across Go versions, so hashing compressed bytes would make bundle
// identity depend on the toolchain.
type Manifest struct {
	BundleSha string          `json:"bundle_sha"`
	Files     []ManifestEntry `json:"files"`
	RunID     string          `json:"run_id"`
	Schema    string          `json:"schema"`
}

// BuildManifest walks the run directory and content-addresses every file.
//
// Walking rather than recording writes is deliberate: log files are written by
// the driver and by container log collection, not through this API, and a
// manifest that silently omits them would content-address only the half of the
// bundle we happen to control.
func (b *Bundle) BuildManifest() (*Manifest, error) {
	var files []ManifestEntry
	err := filepath.WalkDir(b.runDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(b.runDir, p)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		if rel == ManifestFileName || rel == RunningSentinel || strings.HasSuffix(rel, ".tmp") {
			return nil
		}
		sum, size, herr := hashFile(p)
		if herr != nil {
			return herr
		}
		files = append(files, ManifestEntry{Path: rel, Sha256: sum, Size: size})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("recorder: build manifest for %s: %w", b.runDir, err)
	}
	if files == nil {
		files = []ManifestEntry{}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })

	pre, err := cjson.EncodeCompact(files)
	if err != nil {
		return nil, fmt.Errorf("recorder: encode manifest preimage: %w", err)
	}
	sum := sha256.Sum256(pre)
	return &Manifest{
		BundleSha: schema.HashPrefix + hex.EncodeToString(sum[:]),
		Files:     files,
		RunID:     b.runID,
		Schema:    ManifestSchema,
	}, nil
}

// WriteManifest builds and writes MANIFEST.json.
func (b *Bundle) WriteManifest() error {
	m, err := b.BuildManifest()
	if err != nil {
		return err
	}
	data, err := cjson.Encode(*m)
	if err != nil {
		return fmt.Errorf("recorder: encode manifest: %w", err)
	}
	return WriteFileAtomic(filepath.Join(b.runDir, ManifestFileName), data)
}

func hashFile(p string) (string, int64, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return schema.HashPrefix + hex.EncodeToString(h.Sum(nil)), n, nil
}

// ---------------------------------------------------------------------------
// Appender
// ---------------------------------------------------------------------------

// Appender is a mutex-guarded, buffered, append-only writer for a JSONL
// artifact.
type Appender struct {
	mu     sync.Mutex
	f      *os.File
	w      *bufio.Writer
	path   string
	closed bool
}

// Path returns the file being appended to.
func (a *Appender) Path() string { return a.path }

// AppendJSON canonically encodes v as one line.
func (a *Appender) AppendJSON(v any) error {
	line, err := cjson.EncodeCompact(v)
	if err != nil {
		return fmt.Errorf("recorder: encode line for %s: %w", a.path, err)
	}
	return a.AppendLine(line)
}

// AppendLine appends line plus a single LF. line must contain no LF or CR of
// its own.
func (a *Appender) AppendLine(line []byte) error {
	for _, c := range line {
		if c == '\n' || c == '\r' {
			return fmt.Errorf("recorder: %s: a JSONL record must not contain CR or LF", a.path)
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return fmt.Errorf("recorder: %s: appender is closed", a.path)
	}
	if _, err := a.w.Write(line); err != nil {
		return err
	}
	return a.w.WriteByte('\n')
}

// Flush pushes buffered bytes to the OS.
func (a *Appender) Flush() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return nil
	}
	return a.w.Flush()
}

// Sync flushes and fsyncs.
func (a *Appender) Sync() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return nil
	}
	if err := a.w.Flush(); err != nil {
		return err
	}
	return a.f.Sync()
}

// Close flushes, syncs and closes. Idempotent.
func (a *Appender) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return nil
	}
	a.closed = true
	err := a.w.Flush()
	if serr := a.f.Sync(); err == nil {
		err = serr
	}
	if cerr := a.f.Close(); err == nil {
		err = cerr
	}
	return err
}

// ---------------------------------------------------------------------------
// The up -> down handoff
// ---------------------------------------------------------------------------

// Topology is everything `thesis down` needs and cannot possibly have in
// memory.
//
// `up` and `down` are separate OS processes. The compose project name, the
// generated overlay file, the host port assignments and the logical
// node -> container bindings all exist only inside the `up` process unless they
// are written down. On this host a leaked compose project permanently consumes
// one of a hard ceiling of about two dozen Docker bridge networks, so losing
// this record is not a cosmetic failure: it is unrecoverable within a session.
//
// ADDITIVE: the directive names no schema for this file.
type Topology struct {
	// Backend is the harness backend that created the topology.
	Backend string `json:"backend"`
	// ComposeFile is the user's compose file, as given in prothesis.yaml.
	ComposeFile string `json:"compose_file"`
	// ComposeProject is the -p project name. This is the single most important
	// field in the file: without it `down` cannot address what `up` created.
	ComposeProject string `json:"compose_project"`
	// CreatedWallUnixNS is when `up` completed.
	CreatedWallUnixNS int64 `json:"created_wall_unix_ns"`
	// Nodes binds each logical node to its container and published port.
	Nodes []NodeBinding `json:"nodes"`
	// OverlayFile is the generated compose overlay `up` wrote, if any. `down`
	// must pass the same file set or compose resolves a different project.
	OverlayFile string `json:"overlay_file"`
	// ProjectDir is the working directory compose was invoked from.
	ProjectDir string `json:"project_dir"`
	// RunID ties the topology back to its run directory.
	RunID string `json:"run_id"`
	// Schema is TopologySchema.
	Schema string `json:"schema"`
}

// NodeBinding is one logical node's resolved identity.
type NodeBinding struct {
	// ContainerID is the container `up` observed for this node. It may be
	// empty if `up` failed before inspection; `down` then falls back to the
	// project label.
	ContainerID string `json:"container_id"`
	// ContainerPort is the client port inside the container.
	ContainerPort int64 `json:"container_port"`
	// HostPort is the published 127.0.0.1 port. Container IPs are not routable
	// from a Windows host, so every logical node needs its own published port
	// and health probes run host-side against it.
	HostPort int64 `json:"host_port"`
	// ID is the logical node id from prothesis.yaml.
	ID string `json:"id"`
	// Service is the compose service backing the node.
	Service string `json:"service"`
}

// Validate checks the handoff record is usable by a later process.
func (t *Topology) Validate() error {
	var errs schema.ValidationErrors
	if t.Schema != TopologySchema {
		errs.Add("schema", "must be %q, got %q", TopologySchema, t.Schema)
	}
	if !ValidRunID(t.RunID) {
		errs.Add("run_id", "%q does not match %s", t.RunID, schema.RunIDShape)
	}
	if t.Backend == "" {
		errs.Add("backend", "must not be empty")
	}
	if t.Backend == string(schema.BackendCompose) && t.ComposeProject == "" {
		errs.Add("compose_project", "must not be empty for the compose backend "+
			"(without it `thesis down` cannot address what `thesis up` created)")
	}
	if t.Nodes == nil {
		errs.Add("nodes", "must be a list, not null")
	}
	seen := map[string]bool{}
	ports := map[int64]string{}
	for i, n := range t.Nodes {
		p := fmt.Sprintf("nodes[%d]", i)
		if err := SafeName(n.ID); err != nil {
			errs.Add(p+".id", "%s", err.Error())
		}
		if seen[n.ID] {
			errs.Add(p+".id", "duplicate node id %q", n.ID)
		}
		seen[n.ID] = true
		if n.Service == "" {
			errs.Add(p+".service", "must not be empty")
		}
		if n.HostPort < 1 || n.HostPort > 65535 {
			errs.Add(p+".host_port", "%d is not a TCP port", n.HostPort)
		} else if prev, dup := ports[n.HostPort]; dup {
			errs.Add(p+".host_port", "port %d is already assigned to node %q", n.HostPort, prev)
		} else {
			ports[n.HostPort] = n.ID
		}
		if n.ContainerPort < 1 || n.ContainerPort > 65535 {
			errs.Add(p+".container_port", "%d is not a TCP port", n.ContainerPort)
		}
	}
	return errs.OrNil()
}

// WriteTopology validates and atomically writes topology.json into the run
// directory.
func (b *Bundle) WriteTopology(t *Topology) error {
	if t.RunID == "" {
		t.RunID = b.runID
	}
	if t.Schema == "" {
		t.Schema = TopologySchema
	}
	if err := t.Validate(); err != nil {
		return err
	}
	data, err := cjson.Encode(*t)
	if err != nil {
		return fmt.Errorf("recorder: encode topology: %w", err)
	}
	return WriteFileAtomic(filepath.Join(b.runDir, TopologyFileName), data)
}

// ReadTopology reads and validates the handoff record from a run directory.
func ReadTopology(runDir string) (*Topology, error) {
	p := filepath.Join(runDir, TopologyFileName)
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, fmt.Errorf("recorder: read topology: %w", err)
	}
	var t Topology
	if err := cjson.UnmarshalCanonical(data, &t); err != nil {
		return nil, fmt.Errorf("recorder: decode %s: %w", p, err)
	}
	if err := t.Validate(); err != nil {
		return nil, fmt.Errorf("recorder: %s: %w", p, err)
	}
	return &t, nil
}

// ReadTopologyForRun is ReadTopology addressed by run id.
func ReadTopologyForRun(runsDir, runID string) (*Topology, error) {
	if !ValidRunID(runID) {
		return nil, fmt.Errorf("recorder: %q is not a valid run id", runID)
	}
	return ReadTopology(filepath.Join(runsDir, runID))
}

// ---------------------------------------------------------------------------
// Current-run pointer
// ---------------------------------------------------------------------------

// ErrNoCurrentRun is returned when no topology is recorded as live.
var ErrNoCurrentRun = errors.New("recorder: no current run recorded " +
	"(nothing to tear down; `thesis up` writes " + RunsDirName + "/" + CurrentPointer + ")")

func currentPointerPath(runsDir string) string { return filepath.Join(runsDir, CurrentPointer) }

// WriteCurrentRun records runID as the live topology, so a later `thesis down`
// invoked with no arguments knows what to tear down.
func WriteCurrentRun(runsDir, runID string) error {
	if !ValidRunID(runID) {
		return fmt.Errorf("recorder: %q is not a valid run id", runID)
	}
	if err := os.MkdirAll(runsDir, 0o755); err != nil {
		return fmt.Errorf("recorder: create %s: %w", runsDir, err)
	}
	return WriteFileAtomic(currentPointerPath(runsDir), []byte(runID+"\n"))
}

// ReadCurrentRun returns the recorded live run id.
func ReadCurrentRun(runsDir string) (string, error) {
	data, err := os.ReadFile(currentPointerPath(runsDir))
	if errors.Is(err, fs.ErrNotExist) {
		return "", ErrNoCurrentRun
	}
	if err != nil {
		return "", fmt.Errorf("recorder: read current run: %w", err)
	}
	id := strings.TrimRight(string(data), "\r\n")
	if !ValidRunID(id) {
		return "", fmt.Errorf("recorder: %s contains %q, which is not a run id",
			currentPointerPath(runsDir), id)
	}
	return id, nil
}

// ClearCurrentRun removes the pointer. It is idempotent, because `down` must be
// safe to run twice.
func ClearCurrentRun(runsDir string) error {
	err := os.Remove(currentPointerPath(runsDir))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("recorder: clear current run: %w", err)
	}
	return nil
}
