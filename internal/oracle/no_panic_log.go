package oracle

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// MaxLogLineBytes bounds how much of a single log line is matched against.
//
// A crashing process can emit a megabyte on one line. Reading it whole would
// let a target OOM the tool that is watching it for OOMs, so the scanner keeps
// the first MaxLogLineBytes and records that the line was cut. A panic
// announces itself at the START of its line, so the prefix is the part that
// matters.
const MaxLogLineBytes = 64 << 10

// LogPattern is one named crash signature.
//
// Name and Pattern are both carried so that Options.Fingerprint covers the
// pattern SET: deleting `go.panic` from the list silences the oracle without
// touching a line of its code, which is exactly the gate weakening invariant I6
// exists to catch.
type LogPattern struct {
	Name    string `json:"name"`
	Pattern string `json:"pattern"`

	re *regexp.Regexp
}

// NoPanicLogOptions tunes no_panic_log.
type NoPanicLogOptions struct {
	// Patterns REPLACES the default set when non-empty.
	Patterns []LogPattern `json:"patterns"`
	// ExtraPatterns are appended to whichever set is in force. This is the
	// additive knob, and it is the one to reach for: adding a target-specific
	// signature strengthens the oracle, whereas editing Patterns can only
	// narrow it.
	ExtraPatterns []LogPattern `json:"extra_patterns"`
	// Ignore suppresses a matching line. Every entry here is a hole in the
	// oracle; keep them narrow and expect the lock to notice them.
	Ignore []string `json:"ignore"`
}

// DefaultLogPatterns is the compiled-in crash signature set.
//
// The patterns are deliberately not anchored to the start of a line: collected
// logs are usually prefixed by the collector (`kv-n1  | panic: ...`) or by the
// application's own timestamp, and an anchored pattern would miss every one of
// them. They match at a line start OR after whitespace, which keeps `panic: `
// from matching inside a word.
func DefaultLogPatterns() []LogPattern {
	return []LogPattern{
		// Go runtime.
		{Name: "go.panic", Pattern: `(?:^|[\s|])panic: `},
		{Name: "go.fatal", Pattern: `(?:^|[\s|])fatal error: `},
		{Name: "go.goroutine_dump", Pattern: `goroutine \d+ \[running\]:`},

		// Fatal signals, however the runtime spells them.
		{Name: "signal.fatal", Pattern: `signal SIG(SEGV|BUS|ILL|FPE|ABRT)\b`},

		// Out of memory, from the process, the runtime or the kernel.
		{Name: "oom.out_of_memory", Pattern: `(?i)out of memory`},
		{Name: "oom.killed", Pattern: `(?i)(oom[-_ ]?kill(ed|er)?\b|killed process \d+|memory cgroup out of memory)`},
		{Name: "oom.cannot_allocate", Pattern: `(?i)cannot allocate memory`},

		// JVM.
		{Name: "java.exception_in_thread", Pattern: `(?:^|[\s|])Exception in thread "`},
		{Name: "java.stack_frame", Pattern: `(?:^|[\s|])at [\w$.]+\([\w$ .-]*\.java:\d+\)`},
		{Name: "java.caused_by", Pattern: `(?:^|[\s|])Caused by: [\w$.]*(Exception|Error)\b`},

		// Rust and C++.
		{Name: "rust.panic", Pattern: `thread '[^']*' panicked at`},
		{Name: "cpp.terminate", Pattern: `terminate called after throwing an instance of`},
	}
}

// DefaultNoPanicLogOptions returns the defaults.
func DefaultNoPanicLogOptions() NoPanicLogOptions {
	return NoPanicLogOptions{Patterns: DefaultLogPatterns()}
}

func (o NoPanicLogOptions) withDefaults() NoPanicLogOptions {
	if len(o.Patterns) == 0 {
		o.Patterns = DefaultLogPatterns()
	}
	return o
}

// NoPanicLog is the `no_panic_log` built-in: no collected log contains a panic,
// a fatal error or an out-of-memory signature.
//
// It is an invariant oracle, valid in every phase, for the same reason no_crash
// is: a panic is a finding whenever it is printed.
type NoPanicLog struct {
	builtinDecl
	patterns []LogPattern
	ignore   []*regexp.Regexp
	opts     NoPanicLogOptions
}

// NewNoPanicLog compiles the pattern set. A pattern that does not compile is an
// error rather than a silently dropped check.
func NewNoPanicLog(opts NoPanicLogOptions) (*NoPanicLog, error) {
	opts = opts.withDefaults()
	o := &NoPanicLog{builtinDecl: builtinDecl{id: schema.BuiltinNoPanicLog}, opts: opts}

	seen := map[string]bool{}
	add := func(p LogPattern) error {
		if p.Name == "" {
			return fmt.Errorf("no_panic_log: pattern %q has no name", p.Pattern)
		}
		if seen[p.Name] {
			return fmt.Errorf("no_panic_log: pattern name %q is used twice", p.Name)
		}
		re, err := regexp.Compile(p.Pattern)
		if err != nil {
			return fmt.Errorf("no_panic_log: pattern %q does not compile: %w", p.Name, err)
		}
		seen[p.Name] = true
		p.re = re
		o.patterns = append(o.patterns, p)
		return nil
	}
	for _, p := range opts.Patterns {
		if err := add(p); err != nil {
			return nil, err
		}
	}
	for _, p := range opts.ExtraPatterns {
		if err := add(p); err != nil {
			return nil, err
		}
	}
	if len(o.patterns) == 0 {
		return nil, errors.New("no_panic_log: the pattern set is empty, so the oracle could never fire")
	}
	for _, ig := range opts.Ignore {
		re, err := regexp.Compile(ig)
		if err != nil {
			return nil, fmt.Errorf("no_panic_log: ignore pattern %q does not compile: %w", ig, err)
		}
		o.ignore = append(o.ignore, re)
	}
	return o, nil
}

// Patterns returns the compiled signature names, for reporting.
func (o *NoPanicLog) Patterns() []string {
	out := make([]string, 0, len(o.patterns))
	for _, p := range o.patterns {
		out = append(out, p.Name)
	}
	return out
}

// logHit is one matching line.
type logHit struct {
	stream    string
	pattern   string
	lineNo    int
	line      string
	truncated bool
}

// Evaluate implements Oracle.
func (o *NoPanicLog) Evaluate(ctx context.Context, phase schema.Phase, in *Input) (Result, error) {
	if in == nil || len(in.Logs) == 0 {
		return Inconclusive("no logs were collected, so no process could be scanned for panics, fatal "+
			"errors or OOM kills; %d signature(s) went unchecked", len(o.patterns)), nil
	}

	var (
		hits     []logHit
		unread   []string
		scanned  int
		lineSeen int
	)

	for _, ls := range in.Logs {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		r, err := ls.Open()
		if err != nil {
			unread = append(unread, fmt.Sprintf("%s (%v)", ls.Label(), err))
			continue
		}
		n, err := o.scan(ls, r, &hits)
		_ = r.Close()
		lineSeen += n
		if err != nil {
			unread = append(unread, fmt.Sprintf("%s (%v)", ls.Label(), err))
			continue
		}
		scanned++
	}

	if len(hits) > 0 {
		return o.violation(phase, hits, unread), nil
	}
	if len(unread) > 0 {
		return Inconclusive("%d of %d log stream(s) could not be read, so they were never scanned: %s",
			len(unread), len(in.Logs), strings.Join(sortedStrings(unread), ", ")), nil
	}
	if scanned == 0 {
		return Inconclusive("no log stream could be scanned"), nil
	}
	return OK("scanned %d line(s) across %d log stream(s) against %d crash signature(s) with no match",
		lineSeen, scanned, len(o.patterns)), nil
}

// scan reads one stream and appends every matching line.
func (o *NoPanicLog) scan(ls LogStream, r io.Reader, hits *[]logHit) (int, error) {
	lineNo := 0
	err := eachLine(r, MaxLogLineBytes, func(line []byte, truncated bool) bool {
		lineNo++
		if o.ignored(line) {
			return true
		}
		for _, p := range o.patterns {
			if p.re.Match(line) {
				*hits = append(*hits, logHit{
					stream:    ls.Label(),
					pattern:   p.Name,
					lineNo:    lineNo,
					line:      string(line),
					truncated: truncated,
				})
				// One hit per line is enough evidence; a Go panic matches
				// several signatures at once and repeating the same line would
				// bury the witness.
				return true
			}
		}
		return true
	})
	return lineNo, err
}

func (o *NoPanicLog) ignored(line []byte) bool {
	for _, re := range o.ignore {
		if re.Match(line) {
			return true
		}
	}
	return false
}

func (o *NoPanicLog) violation(phase schema.Phase, hits []logHit, unread []string) Result {
	first := hits[0]
	names := map[string]int{}
	streams := map[string]bool{}
	for _, h := range hits {
		names[h.pattern]++
		streams[h.stream] = true
	}
	patterns := make([]string, 0, len(names))
	for n, c := range names {
		patterns = append(patterns, fmt.Sprintf("%s x%d", n, c))
	}
	streamList := make([]string, 0, len(streams))
	for s := range streams {
		streamList = append(streamList, s)
	}

	w := NewWitness().
		Set("log", first.stream).
		Set("line_no", first.lineNo).
		Set("pattern", first.pattern).
		Set("match_count", len(hits)).
		Set("patterns_matched", sortedStrings(patterns)).
		Set("streams", sortedStrings(streamList))
	w.Text("line", first.line)
	if first.truncated {
		w.Set("line_clipped_at_bytes", MaxLogLineBytes)
	}
	if len(unread) > 0 {
		w.Set("unreadable_streams", sortedStrings(unread))
	}

	// The witness quotes the matching line verbatim, because the whole value of
	// this oracle is that a human can confirm the finding without re-running
	// anything.
	quoted, _ := truncateText(strings.TrimSpace(first.line), MaxWitnessTextBytes)

	// A collected log line carries no virtual-clock position (the collector
	// hands over text, not timestamps) so first_seen_ms stays 0 and the phase
	// is pinned to the one the scan ran in. Inventing a millisecond offset here
	// would put a fabricated moment in the causal timeline.
	return Violated(0, w.Build(),
		"%d log line(s) matched a crash signature; first was %s line %d matching %q: %s",
		len(hits), first.stream, first.lineNo, first.pattern, quoted).inPhase(phase)
}

// ---------------------------------------------------------------------------
// bounded line scanning
// ---------------------------------------------------------------------------

// eachLine calls fn for every line in r, keeping at most maxLine bytes of each.
//
// It exists because neither obvious alternative is safe here. bufio.Scanner
// fails the whole stream on a line over its token limit: silently ending the
// scan, which would turn a giant panic dump into a clean pass. ReadBytes('\n')
// is unbounded and lets the target dictate the tool's memory use.
//
// The slice handed to fn aliases an internal buffer and is only valid for the
// duration of the call.
func eachLine(r io.Reader, maxLine int, fn func(line []byte, truncated bool) bool) error {
	br := bufio.NewReaderSize(r, 64*1024)
	var (
		buf       []byte
		truncated bool
	)
	flush := func() bool {
		line := bytes.TrimSuffix(buf, []byte("\r"))
		keep := fn(line, truncated)
		buf = buf[:0]
		truncated = false
		return keep
	}
	for {
		chunk, err := br.ReadSlice('\n')
		if len(chunk) > 0 {
			c := chunk
			if err == nil && c[len(c)-1] == '\n' {
				c = c[:len(c)-1]
			}
			if room := maxLine - len(buf); room > 0 {
				if len(c) > room {
					buf = append(buf, c[:room]...)
					truncated = true
				} else {
					buf = append(buf, c...)
				}
			} else if len(c) > 0 {
				truncated = true
			}
		}
		switch {
		case err == nil:
			if !flush() {
				return nil
			}
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF):
			if len(buf) > 0 || truncated {
				flush()
			}
			return nil
		default:
			return err
		}
	}
}
