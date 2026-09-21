package driver

import (
	"errors"
	"fmt"
	"io"
	"os"
)

// ---------------------------------------------------------------------------
// DRAIN: asking a driver to stop ISSUING without killing what it is doing
//
// The problem this exists to solve is OQ-033. QUIESCE used to be a single act:
// kill the driver. A profile long enough to span a fault window is ALWAYS killed
// with operations in flight (that is what D-040 designed `linear` to do) and
// `no_stuck_op` then sees invokes with no completion and correctly refuses to
// decide, because "the harness stopped the driver" and "the operation was
// wedged" are indistinguishable from the history alone.
//
// The oracle is right. The lifecycle was wrong: it never asked. A driver that
// can be told "issue nothing new, finish what you hold" turns those in-flight
// operations into completed ones, which the oracle can then judge on the
// evidence: pass them if they retired inside the SLO ceiling, VIOLATE them if
// they did not. Both answers are earned; neither is manufactured.
//
// The channel is the driver's stdin, because that is the one input every process
// has and it needs no cooperation from the command template. Two signals are
// sent, in this order, and a driver honouring EITHER drains:
//
//	{"cmd":"stop"}\n   an explicit request, for a driver that keeps stdin open
//	EOF                the pipe closes, for a driver that only watches for it
//
// The fixture's loadgen implements exactly this pair behind --stdin-control, and
// its clients stop issuing at the top of their loop while the operation already
// in flight runs to completion, which is the semantics required, not merely a
// convenient approximation of it.
// ---------------------------------------------------------------------------

// DrainCommand is the line written to a cooperating driver's stdin to ask it to
// stop issuing new operations while completing the ones it already has.
//
// ADDITIVE. The directive fixes the history log, the exit codes and the command
// TEMPLATE, but says nothing about a control channel to a running driver. This
// wire form is chosen to match the fixture loadgen's existing `--stdin-control`
// reader rather than inventing a second dialect. Logged as OQ-035.
const DrainCommand = `{"cmd":"stop"}`

// StdinControlEnv tells a driver that its stdin is a PRO-THESIS control channel
// and that it should honour DrainCommand and EOF.
//
// ADDITIVE, and exported on exactly the same terms as schema.PlanPathEnv
// (DECISIONS.md D-021): a driver that does not know the name ignores it, and a
// driver that does needs no change to its command line. It is namespaced so it
// cannot collide with anything that is not ours.
const StdinControlEnv = "PROTHESIS_STDIN_CONTROL"

// ErrNoControlChannel reports that this driver was launched WITHOUT a control
// channel, so it could not be asked to drain.
//
// It is a distinct error rather than a silent no-op because "the driver refused
// to drain" and "nobody asked it to" are different facts about a run, and the
// verdict has to be able to tell them apart.
var ErrNoControlChannel = errors.New("driver: this driver was started without a stdin control channel")

// Drain asks the driver to stop issuing new operations and complete the ones it
// has: it writes DrainCommand and then closes stdin, so a driver honouring
// either signal responds.
//
// It does NOT wait. Waiting is the caller's business, because only the caller
// knows the deadline it is prepared to give and what to do when the deadline
// passes. Drain is idempotent, and it is not an error to drain a driver that has
// already exited: that is the outcome drain was asking for.
//
// A driver that ignores both signals keeps running, and Drain has no way to tell
// that from one that is finishing up. The DEADLINE is the discriminator, and it
// belongs to the caller.
func (s *Supervisor) Drain() error {
	s.mu.Lock()
	w, closed, live := s.stdin, s.stdinClosed, s.live
	if w == nil {
		s.mu.Unlock()
		return ErrNoControlChannel
	}
	if closed || !live {
		// Already asked, or already gone. Both mean there is nothing left to do
		// and neither is a failure.
		s.mu.Unlock()
		return nil
	}
	s.stdinClosed = true
	s.mu.Unlock()

	_, writeErr := io.WriteString(w, DrainCommand+"\n")
	closeErr := w.Close()

	// The close is the load-bearing half: it delivers EOF, which the fixture's
	// loadgen and any well-behaved stdin-driven process both treat as "stop".
	// A write that failed against a pipe whose reader has already gone away is
	// not a drain failure: the reader going away IS the driver exiting.
	if closeErr == nil || errors.Is(closeErr, os.ErrClosed) {
		return nil
	}
	if writeErr != nil {
		return fmt.Errorf("driver: drain: writing %s failed (%v) and closing stdin failed: %w",
			DrainCommand, writeErr, closeErr)
	}
	return fmt.Errorf("driver: drain: closing stdin: %w", closeErr)
}

// HasControlChannel reports whether this driver was launched with a stdin
// control channel.
//
// It says a channel was PROVIDED, never that the driver honours it. Nothing on
// this side of the pipe can know that, and claiming otherwise would put an
// unearned sentence in the verdict.
func (s *Supervisor) HasControlChannel() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stdin != nil
}
