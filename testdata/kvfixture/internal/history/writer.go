package history

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
)

// Writer appends records to a JSONL file. LF only, no BOM, one trailing
// newline per record. The mutex serialises appends so a line can never be torn;
// it deliberately does NOT set t_ns, which the caller captures at the true
// event instant.
type Writer struct {
	mu  sync.Mutex
	f   *os.File
	bw  *bufio.Writer
	seq uint64
	n   uint64
}

// NewWriter creates or truncates path and returns a Writer.
func NewWriter(path string) (*Writer, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return nil, fmt.Errorf("history: open %s: %w", path, err)
	}
	return &Writer{f: f, bw: bufio.NewWriterSize(f, 256*1024)}, nil
}

// NextSeq returns a monotonic append sequence number for meta.seq.
func (w *Writer) NextSeq() uint64 { return atomic.AddUint64(&w.seq, 1) }

// Append writes one record.
func (w *Writer) Append(rec Record) error {
	if rec.Meta != nil && rec.Meta.Seq == 0 {
		rec.Meta.Seq = w.NextSeq()
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("history: marshal: %w", err)
	}
	if bytes.ContainsAny(b, "\r\n") {
		return fmt.Errorf("history: record contains a raw newline")
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, err := w.bw.Write(b); err != nil {
		return err
	}
	if err := w.bw.WriteByte('\n'); err != nil {
		return err
	}
	w.n++
	if w.n%256 == 0 {
		return w.bw.Flush()
	}
	return nil
}

// Count returns the number of records written.
func (w *Writer) Count() uint64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.n
}

// Close flushes, fsyncs and closes. It must be called before the process exits
// or the buffered tail of the history is lost, which reads downstream as a
// truncated run rather than as a driver bug.
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return nil
	}
	if err := w.bw.Flush(); err != nil {
		w.f.Close()
		w.f = nil
		return err
	}
	if err := w.f.Sync(); err != nil {
		w.f.Close()
		w.f = nil
		return err
	}
	err := w.f.Close()
	w.f = nil
	return err
}
