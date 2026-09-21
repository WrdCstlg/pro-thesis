package raft

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// Storage is the durable half of Raft's persistent state. Figure 2 requires
// currentTerm, votedFor and log[] to survive a restart; a node that forgets its
// term or its vote can double-vote and violate Election Safety, which would be
// a second safety bug independent of the injected one.
type Storage interface {
	LoadState() (term uint64, votedFor string, err error)
	SaveState(term uint64, votedFor string) error
	LoadEntries() ([]Entry, error)
	AppendEntries(entries []Entry) error
	TruncateFrom(index uint64) error
	Close() error
}

// MemStorage is a non-durable Storage for tests and for in-process runs.
type MemStorage struct {
	mu       sync.Mutex
	term     uint64
	votedFor string
	entries  []Entry
}

// NewMemStorage returns an empty in-memory store.
func NewMemStorage() *MemStorage { return &MemStorage{} }

// LoadState implements Storage.
func (m *MemStorage) LoadState() (uint64, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.term, m.votedFor, nil
}

// SaveState implements Storage.
func (m *MemStorage) SaveState(term uint64, votedFor string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.term, m.votedFor = term, votedFor
	return nil
}

// LoadEntries implements Storage.
func (m *MemStorage) LoadEntries() ([]Entry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Entry, len(m.entries))
	copy(out, m.entries)
	return out, nil
}

// AppendEntries implements Storage.
func (m *MemStorage) AppendEntries(entries []Entry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.entries = append(m.entries, entries...)
	return nil
}

// TruncateFrom implements Storage.
func (m *MemStorage) TruncateFrom(index uint64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if index == 0 || index > uint64(len(m.entries)) {
		return nil
	}
	m.entries = m.entries[:index-1]
	return nil
}

// Close implements Storage.
func (m *MemStorage) Close() error { return nil }

type persistedState struct {
	Term     uint64 `json:"term"`
	VotedFor string `json:"voted_for"`
}

// FileStorage keeps term/vote in state.json (write-temp, fsync, rename) and the
// log in an append-only, fsynced wal.jsonl.
type FileStorage struct {
	mu      sync.Mutex
	dir     string
	walPath string
	stPath  string
	wal     *os.File
	w       *bufio.Writer
	count   uint64
}

// NewFileStorage opens (creating if necessary) a durable store under dir.
func NewFileStorage(dir string) (*FileStorage, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("raft: create data dir: %w", err)
	}
	fs := &FileStorage{
		dir:     dir,
		walPath: filepath.Join(dir, "wal.jsonl"),
		stPath:  filepath.Join(dir, "state.json"),
	}
	f, err := os.OpenFile(fs.walPath, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("raft: open wal: %w", err)
	}
	fs.wal = f
	fs.w = bufio.NewWriterSize(f, 64*1024)
	return fs, nil
}

// LoadState implements Storage.
func (f *FileStorage) LoadState() (uint64, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, err := os.ReadFile(f.stPath)
	if os.IsNotExist(err) {
		return 0, "", nil
	}
	if err != nil {
		return 0, "", fmt.Errorf("raft: read state: %w", err)
	}
	var ps persistedState
	if err := json.Unmarshal(b, &ps); err != nil {
		return 0, "", fmt.Errorf("raft: parse state: %w", err)
	}
	return ps.Term, ps.VotedFor, nil
}

// SaveState implements Storage. It is fsynced before it returns, because the
// caller replies to an RPC immediately afterwards.
func (f *FileStorage) SaveState(term uint64, votedFor string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, err := json.Marshal(persistedState{Term: term, VotedFor: votedFor})
	if err != nil {
		return err
	}
	tmp := f.stPath + ".tmp"
	fh, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("raft: open state tmp: %w", err)
	}
	if _, err := fh.Write(append(b, '\n')); err != nil {
		fh.Close()
		return err
	}
	if err := fh.Sync(); err != nil {
		fh.Close()
		return err
	}
	if err := fh.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, f.stPath)
}

// LoadEntries implements Storage.
func (f *FileStorage) LoadEntries() ([]Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fh, err := os.Open(f.walPath)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("raft: open wal: %w", err)
	}
	defer fh.Close()

	var out []Entry
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e Entry
		if err := json.Unmarshal(line, &e); err != nil {
			// A torn tail is the expected outcome of a crash mid-fsync. Stop at
			// the last intact record rather than refusing to boot.
			break
		}
		if e.Index != uint64(len(out))+1 {
			break
		}
		out = append(out, e)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("raft: scan wal: %w", err)
	}
	f.count = uint64(len(out))
	return out, nil
}

// AppendEntries implements Storage.
func (f *FileStorage) AppendEntries(entries []Entry) error {
	if len(entries) == 0 {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, e := range entries {
		b, err := json.Marshal(e)
		if err != nil {
			return err
		}
		if _, err := f.w.Write(append(b, '\n')); err != nil {
			return err
		}
		f.count = e.Index
	}
	if err := f.w.Flush(); err != nil {
		return err
	}
	return f.wal.Sync()
}

// TruncateFrom implements Storage by rewriting the WAL. Conflicting-suffix
// truncation is rare (it requires a lost election with uncommitted entries) and
// the fixture's logs are bounded, so a rewrite is simpler and more obviously
// correct than an in-place tombstone scheme.
func (f *FileStorage) TruncateFrom(index uint64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if index == 0 {
		return nil
	}
	if err := f.w.Flush(); err != nil {
		return err
	}
	fh, err := os.Open(f.walPath)
	if err != nil {
		return fmt.Errorf("raft: open wal for truncate: %w", err)
	}
	var kept []Entry
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		if len(sc.Bytes()) == 0 {
			continue
		}
		var e Entry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			break
		}
		if e.Index >= index {
			break
		}
		kept = append(kept, e)
	}
	fh.Close()

	tmp := f.walPath + ".tmp"
	nf, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	bw := bufio.NewWriterSize(nf, 64*1024)
	for _, e := range kept {
		b, err := json.Marshal(e)
		if err != nil {
			nf.Close()
			return err
		}
		if _, err := bw.Write(append(b, '\n')); err != nil {
			nf.Close()
			return err
		}
	}
	if err := bw.Flush(); err != nil {
		nf.Close()
		return err
	}
	if err := nf.Sync(); err != nil {
		nf.Close()
		return err
	}
	if err := nf.Close(); err != nil {
		return err
	}
	if err := f.wal.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, f.walPath); err != nil {
		return err
	}
	reopened, err := os.OpenFile(f.walPath, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	f.wal = reopened
	f.w = bufio.NewWriterSize(reopened, 64*1024)
	f.count = index - 1
	return nil
}

// Close implements Storage.
func (f *FileStorage) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.wal == nil {
		return nil
	}
	if err := f.w.Flush(); err != nil {
		return err
	}
	err := f.wal.Close()
	f.wal = nil
	return err
}
