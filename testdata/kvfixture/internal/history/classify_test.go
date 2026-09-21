package history

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"syscall"
	"testing"
)

func boolPtr(b bool) *bool { return &b }

// TestClassifyTable has one case per row of the ok/fail/info decision table.
// This is the fixture's soundness lynchpin: a single mis-categorised timeout
// makes every consistency verdict unsound while still looking correct.
func TestClassifyTable(t *testing.T) {
	dialRefused := &net.OpError{
		Op:  "dial",
		Net: "tcp",
		Err: syscall.ECONNREFUSED,
	}
	dnsErr := &net.DNSError{Err: "no such host", Name: "kv-n9", IsNotFound: true}
	timeoutErr := &net.OpError{Op: "read", Net: "tcp", Err: os.ErrDeadlineExceeded}
	resetAfterWrite := &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}

	cases := []struct {
		name string
		in   Attempt
		want string
		why  string
	}{
		{
			name: "200 with a parseable body",
			in:   Attempt{WroteRequest: true, GotResponse: true, Status: 200, BodyParsed: true},
			want: TypeOK,
			why:  "the server confirmed the operation",
		},
		{
			name: "200 CAS mismatch is applied and therefore ok",
			in:   Attempt{WroteRequest: true, GotResponse: true, Status: 200, BodyParsed: true, Applied: boolPtr(true)},
			want: TypeOK,
			why:  "a definite CAS failure is a successful operation with a negative result",
		},
		{
			name: "200 whose body we cannot parse",
			in:   Attempt{WroteRequest: true, GotResponse: true, Status: 200, BodyParsed: false},
			want: TypeInfo,
			why:  "it ran, but we do not know what it did",
		},
		{
			name: "connection refused before any byte was written",
			in:   Attempt{WroteRequest: false, Err: fmt.Errorf("Post %q: %w", "http://x/", dialRefused)},
			want: TypeFail,
			why:  "the request provably never reached the server process",
		},
		{
			name: "DNS failure",
			in:   Attempt{WroteRequest: false, Err: fmt.Errorf("Get %q: %w", "http://x/", dnsErr)},
			want: TypeFail,
			why:  "never resolved, so never reached the application",
		},
		{
			name: "503 NOT_LEADER with applied:false",
			in:   Attempt{WroteRequest: true, GotResponse: true, Status: 503, Code: "NOT_LEADER", Applied: boolPtr(false)},
			want: TypeFail,
			why:  "the server declined before touching the state machine",
		},
		{
			name: "409 NOT_COMMITTED with applied:false",
			in:   Attempt{WroteRequest: true, GotResponse: true, Status: 409, Code: "NOT_COMMITTED", Applied: boolPtr(false)},
			want: TypeFail,
			why:  "the entry was overwritten by a later term and can never commit",
		},
		{
			name: "429 admission rejection with applied:false",
			in:   Attempt{WroteRequest: true, GotResponse: true, Status: 429, Code: "OVERLOAD", Applied: boolPtr(false)},
			want: TypeFail,
			why:  "rejected at admission",
		},
		{
			name: "deadline exceeded before response headers",
			in:   Attempt{WroteRequest: true, GotResponse: false, Err: context.DeadlineExceeded},
			want: TypeInfo,
			why:  "THE FROZEN-NODE CASE: the kernel buffered the request and it may still be applied",
		},
		{
			name: "deadline exceeded with no request byte written",
			in:   Attempt{WroteRequest: false, GotResponse: false, Err: context.DeadlineExceeded},
			want: TypeInfo,
			why:  "a dial timeout is not proof the request was never delivered; only a refusal is",
		},
		{
			name: "deadline exceeded while reading the body",
			in:   Attempt{WroteRequest: true, GotResponse: true, Status: 200, Err: timeoutErr},
			want: TypeInfo,
			why:  "applied, response lost",
		},
		{
			name: "connection reset after the request was written",
			in:   Attempt{WroteRequest: true, GotResponse: false, Err: fmt.Errorf("Post: %w", resetAfterWrite)},
			want: TypeInfo,
			why:  "the server may have read a complete request",
		},
		{
			name: "500 without applied:false",
			in:   Attempt{WroteRequest: true, GotResponse: true, Status: 500, Code: "PROPOSE_FAILED"},
			want: TypeInfo,
			why:  "absence of evidence is not evidence of absence",
		},
		{
			name: "504 APPLY_TIMEOUT without applied:false",
			in:   Attempt{WroteRequest: true, GotResponse: true, Status: 504, Code: "APPLY_TIMEOUT"},
			want: TypeInfo,
			why:  "the command may still commit after the server stopped waiting",
		},
		{
			name: "502 from an intermediary",
			in:   Attempt{WroteRequest: true, GotResponse: true, Status: 502},
			want: TypeInfo,
			why:  "unknown",
		},
		{
			name: "headers never arrived and no error was reported",
			in:   Attempt{WroteRequest: true, GotResponse: false},
			want: TypeInfo,
			why:  "nothing observed, nothing may be concluded",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Classify(tc.in)
			if got.Type != tc.want {
				t.Fatalf("Classify(%s) = %q, want %q\n  reason: %s", tc.name, got.Type, tc.want, tc.why)
			}
			if tc.want != TypeOK && got.Error == "" {
				t.Fatalf("non-ok outcome must carry an error label")
			}
		})
	}
}

// A timeout must NEVER be classified fail, whatever else is true about it.
// This is stated separately because it is the specific mistake that silently
// invalidates a linearizability checker.
func TestTimeoutIsNeverFail(t *testing.T) {
	for _, err := range []error{
		context.DeadlineExceeded,
		os.ErrDeadlineExceeded,
		fmt.Errorf("wrapped: %w", context.DeadlineExceeded),
		&net.OpError{Op: "dial", Net: "tcp", Err: os.ErrDeadlineExceeded},
	} {
		for _, wrote := range []bool{true, false} {
			out := Classify(Attempt{WroteRequest: wrote, Err: err})
			if out.Type == TypeFail {
				t.Fatalf("Classify(timeout %v, wrote=%v) = fail; timeouts are always indeterminate", err, wrote)
			}
		}
	}
}

func TestErrorLabels(t *testing.T) {
	if got := errorLabel(context.DeadlineExceeded); got != "timeout" {
		t.Fatalf("errorLabel(DeadlineExceeded) = %q", got)
	}
	if got := errorLabel(errors.New("boom")); got != "boom" {
		t.Fatalf("errorLabel(boom) = %q", got)
	}
	if got := failLabel("", http.StatusServiceUnavailable); got != "Service Unavailable" {
		t.Fatalf("failLabel = %q", got)
	}
}
