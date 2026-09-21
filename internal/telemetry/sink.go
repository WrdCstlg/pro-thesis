package telemetry

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// WriterSink appends samples as JSONL to any writer.
//
// It is mutex-guarded because the collector samples targets concurrently and a
// JSONL file with interleaved partial lines is unparseable: one torn line makes
// the whole stream useless, which is the same reasoning behind
// recorder.Appender.
type WriterSink struct {
	mu     sync.Mutex
	w      *bufio.Writer
	closer func() error
	closed bool
	n      int64
}

// NewWriterSink wraps w. flushEvery forces a flush after that many lines; zero
// flushes on every line.
//
// Flushing eagerly is the default on purpose: the JSONL is specified as
// TAILABLE, and a live consumer cannot read what is sitting in a buffer. The
// cost is one write syscall per sample, which at three nodes every 500 ms is
// six per second.
func NewWriterSink(w interface {
	Write([]byte) (int, error)
}) *WriterSink {
	return &WriterSink{w: bufio.NewWriter(w)}
}

// NewFileSink opens path for append and returns a sink that owns it.
func NewFileSink(path string) (*WriterSink, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("telemetry: create %s: %w", filepath.Dir(path), err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("telemetry: open %s: %w", path, err)
	}
	s := NewWriterSink(f)
	s.closer = func() error {
		if err := f.Sync(); err != nil {
			_ = f.Close()
			return err
		}
		return f.Close()
	}
	return s, nil
}

// Append implements Sink.
func (s *WriterSink) Append(sample *Sample) error {
	line, err := sample.MarshalLine()
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("telemetry: sink is closed")
	}
	if _, err := s.w.Write(line); err != nil {
		return err
	}
	if err := s.w.WriteByte('\n'); err != nil {
		return err
	}
	s.n++
	return s.w.Flush()
}

// Count returns the number of samples appended.
func (s *WriterSink) Count() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.n
}

// Flush pushes buffered bytes to the underlying writer.
func (s *WriterSink) Flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	return s.w.Flush()
}

// Close flushes and releases the underlying file, if this sink owns one.
// Idempotent.
func (s *WriterSink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	err := s.w.Flush()
	if s.closer != nil {
		if cerr := s.closer(); err == nil {
			err = cerr
		}
	}
	return err
}

// MemorySink collects samples in memory. For tests and for short-lived
// in-process consumers.
type MemorySink struct {
	mu      sync.Mutex
	Samples []Sample
}

// Append implements Sink.
func (m *MemorySink) Append(s *Sample) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Samples = append(m.Samples, *s)
	return nil
}

// All returns a copy of the collected samples.
func (m *MemorySink) All() []Sample {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Sample(nil), m.Samples...)
}

// MultiSink fans a sample out to several sinks, so the live JSONL and an
// in-process classifier can share one collector.
type MultiSink []Sink

// Append implements Sink. It appends to every sink and returns the first error,
// rather than stopping at it: a failing in-process consumer must not stop the
// durable artifact from being written.
func (m MultiSink) Append(s *Sample) error {
	var first error
	for _, sink := range m {
		if err := sink.Append(s); err != nil && first == nil {
			first = err
		}
	}
	return first
}
