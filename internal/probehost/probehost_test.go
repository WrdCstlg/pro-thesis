package probehost

import "testing"

// restore puts the package back to its default so one test cannot leak into
// the next through the installed value.
func restore(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		if err := Set(Default); err != nil {
			t.Fatalf("restoring the default failed: %v", err)
		}
	})
}

func absent(string) (string, bool) { return "", false }

func present(v string) func(string) (string, bool) {
	return func(string) (string, bool) { return v, true }
}

func TestTheDefaultIsLoopbackWhenTheVariableIsAbsent(t *testing.T) {
	restore(t)
	if err := SetFromEnv(absent); err != nil {
		t.Fatalf("an absent variable must not be an error: %v", err)
	}
	if got := Host(); got != Default {
		t.Fatalf("Host() = %q, want %q", got, Default)
	}
	if Default != "127.0.0.1" {
		t.Fatalf("the default moved to %q; D-010 measured that a published port is "+
			"reachable on loopback and nowhere else", Default)
	}
}

func TestSetFromEnvInstallsTheValue(t *testing.T) {
	restore(t)
	if err := SetFromEnv(present("host.docker.internal")); err != nil {
		t.Fatalf("SetFromEnv: %v", err)
	}
	if got := Host(); got != "host.docker.internal" {
		t.Fatalf("Host() = %q, want %q", got, "host.docker.internal")
	}
}

func TestSurroundingWhitespaceIsTrimmed(t *testing.T) {
	restore(t)
	if err := SetFromEnv(present("  172.17.0.1\t")); err != nil {
		t.Fatalf("SetFromEnv: %v", err)
	}
	if got := Host(); got != "172.17.0.1" {
		t.Fatalf("Host() = %q, want %q", got, "172.17.0.1")
	}
}

// Every rejection below is a value that would otherwise produce a probe URL
// that fails for a reason with nothing to do with the system under test, which
// would be recorded as an environment failure and read as the target's fault.
func TestValuesThatCannotBeAProbeDestinationAreRefused(t *testing.T) {
	cases := []struct {
		name, value, wantSubstring string
	}{
		{"empty", "", "names no address"},
		{"whitespace only", "   ", "names no address"},
		{"the wildcard bind address", "0.0.0.0", "bind address"},
		{"the IPv6 wildcard", "::", "bind address"},
		{"a URL rather than a host", "http://host.docker.internal", "is a URL, not a host"},
		{"a host carrying a port", "host.docker.internal:8080", "carries its own port"},
		{"an unbracketed IPv6 literal", "fd00::1", "bracketed"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := Validate(c.value)
			if err == nil {
				t.Fatalf("Validate(%q) = nil, want an error", c.value)
			}
			if !contains(err.Error(), c.wantSubstring) {
				t.Fatalf("Validate(%q) error = %q, want it to mention %q",
					c.value, err.Error(), c.wantSubstring)
			}
		})
	}
}

// The probe template is plain substitution into `http://{host}:{port}/...`, so
// an IPv6 literal only works when the caller brackets it.
func TestABracketedIPv6LiteralIsAccepted(t *testing.T) {
	restore(t)
	if err := SetFromEnv(present("[fd00::1]")); err != nil {
		t.Fatalf("a bracketed IPv6 literal must be accepted: %v", err)
	}
	if got := Host(); got != "[fd00::1]" {
		t.Fatalf("Host() = %q, want %q", got, "[fd00::1]")
	}
}

// A refused value must not be installed: leaving a half-applied setting behind
// would make the probe destination depend on which error the operator ignored.
func TestARefusedValueLeavesTheInstalledHostUnchanged(t *testing.T) {
	restore(t)
	if err := SetFromEnv(present("host.docker.internal")); err != nil {
		t.Fatal(err)
	}
	if err := SetFromEnv(present("0.0.0.0")); err == nil {
		t.Fatal("0.0.0.0 was accepted")
	}
	if got := Host(); got != "host.docker.internal" {
		t.Fatalf("Host() = %q, want the previously installed %q", got, "host.docker.internal")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
