package history

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"strings"
	"syscall"
)

// Attempt is everything the client observed about one HTTP round trip. It is
// deliberately explicit rather than derived, so the classifier is a pure
// function that a table-driven test can drive directly.
type Attempt struct {
	// WroteRequest is true if any byte of the request reached the wire. It comes
	// from httptrace.ClientTrace.WroteRequest, not from a guess.
	WroteRequest bool
	// GotResponse is true if response headers were received.
	GotResponse bool
	// Err is the transport-level error, if any.
	Err error
	// Status is the HTTP status code, valid only when GotResponse is true.
	Status int
	// BodyParsed is true if the response body decoded into a known shape.
	BodyParsed bool
	// Applied is the server's `applied` field, nil when absent.
	Applied *bool
	// Code is the server's error code, empty when absent.
	Code string
}

// Outcome is the classification result.
type Outcome struct {
	Type  string // TypeOK | TypeFail | TypeInfo
	Error string // populated for fail and info
}

// Classify implements the ok/fail/info decision table.
//
// The governing rule, from the directive's section 4.4 and the determinism
// addendum: `fail` requires POSITIVE EVIDENCE OF NON-EXECUTION. Everything
// ambiguous is `info`.
//
//	200 with a parseable body                       -> ok
//	CAS that ran and did not match (applied:true)   -> ok    (definite negative result)
//	dial refused / DNS failure / no route,
//	  with no request byte on the wire              -> fail  (never reached the process)
//	4xx/5xx carrying applied:false                  -> fail  (declined before the state machine)
//	any timeout, before or after headers            -> INFO
//	connection reset after the request was written  -> INFO
//	any 5xx WITHOUT applied:false                   -> INFO
//	anything else                                   -> INFO
//
// The frozen-node case is precisely why the timeout rows are info. A paused
// node's KERNEL completes the TCP handshake and buffers the request; the client
// times out having no idea whether the write will be applied on resume. If that
// were recorded as `fail` and the request were later applied, every consistency
// verdict built on the history would be unsound -- and would still look correct.
func Classify(a Attempt) Outcome {
	if a.Err != nil {
		if !a.WroteRequest && provablyNeverSent(a.Err) {
			return Outcome{Type: TypeFail, Error: errorLabel(a.Err)}
		}
		return Outcome{Type: TypeInfo, Error: errorLabel(a.Err)}
	}

	if !a.GotResponse {
		return Outcome{Type: TypeInfo, Error: "no-response"}
	}

	if a.Status == http.StatusOK {
		if !a.BodyParsed {
			// A 200 whose body we could not understand tells us the operation
			// ran, but not what it did. Not ok; not fail.
			return Outcome{Type: TypeInfo, Error: "unparseable-body"}
		}
		return Outcome{Type: TypeOK}
	}

	if a.Applied != nil && !*a.Applied {
		return Outcome{Type: TypeFail, Error: failLabel(a.Code, a.Status)}
	}

	// Absence of evidence is not evidence of absence.
	return Outcome{Type: TypeInfo, Error: failLabel(a.Code, a.Status)}
}

// provablyNeverSent reports whether the error proves the request never reached
// the server process.
func provablyNeverSent(err error) bool {
	if err == nil {
		return false
	}
	// A deadline is never proof of anything.
	if errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, context.Canceled) ||
		errors.Is(err, os.ErrDeadlineExceeded) {
		return false
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		if opErr.Timeout() {
			return false
		}
		if opErr.Op == "dial" {
			return true
		}
		return false
	}
	// Windows surfaces a refused connection through a syscall.Errno that does
	// not always match syscall.ECONNREFUSED by value; fall back to the text of
	// the wrapped error, but only for the dial phase.
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "no such host") ||
		strings.Contains(msg, "actively refused") {
		return true
	}
	return false
}

func errorLabel(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, os.ErrDeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "timeout"
	}
	msg := err.Error()
	if len(msg) > 200 {
		msg = msg[:200]
	}
	return msg
}

func failLabel(code string, status int) string {
	if code != "" {
		return code
	}
	return http.StatusText(status)
}
