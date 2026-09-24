// Package probehost holds the address host-side probes are sent to.
//
// D-010 measured that a container IP is not routable from a Windows host under
// Docker Desktop's Linux engine, so a published port on loopback is the only
// way in, and `127.0.0.1` was compiled in as a constant. That holds for the
// harness running ON the host. It is false the moment the harness runs INSIDE a
// container: there `127.0.0.1` is the container's own loopback, the target's
// published ports are on a different network namespace, and every health probe
// fails for a reason that has nothing to do with the system under test.
//
// So the address is now resolved once at startup from PROTHESIS_PROBE_HOST and
// defaults to what D-010 decided. The default is unchanged, which is the point:
// no existing project moves, and a containerized run names the address it can
// actually reach (`host.docker.internal` under Docker Desktop, the bridge
// gateway on Linux).
//
// It is an environment variable rather than a configuration key because it
// describes where the harness is standing, not what is being tested. Two
// operators running the same committed project from different places must reach
// the same verdict, and a key in prothesis.yaml would let one of them change
// the gate by moving house.
//
// The resolved value lives in a package variable, installed once, for the same
// reason harness.dockerBin does: threading it through would touch every probe
// call site in two packages to carry a value that cannot change during a run.
package probehost

import (
	"fmt"
	"strings"
)

const (
	// Default is D-010's measured address, and what every project that says
	// nothing continues to use.
	Default = "127.0.0.1"

	// EnvVar names the address host-side probes are sent to.
	EnvVar = "PROTHESIS_PROBE_HOST"
)

// host is the installed value. It is only ever written by Set, which validates
// first, so a refused value never lands here.
var host = Default

// Host returns the address host-side probes are sent to.
func Host() string { return host }

// Set validates v and installs it. On error nothing is installed, so a caller
// that ignores the error keeps the previous address rather than an unusable
// one.
func Set(v string) error {
	trimmed := strings.TrimSpace(v)
	if err := Validate(trimmed); err != nil {
		return err
	}
	host = trimmed
	return nil
}

// SetFromEnv installs the address named by EnvVar. An absent variable is not an
// error and leaves the default in place; a present but unusable one is refused
// loudly, because silently falling back to loopback inside a container would
// turn every world INCONCLUSIVE with no statement of why.
func SetFromEnv(lookup func(string) (string, bool)) error {
	raw, ok := lookup(EnvVar)
	if !ok {
		return nil
	}
	if err := Set(raw); err != nil {
		return fmt.Errorf("%s: %w", EnvVar, err)
	}
	return nil
}

// Validate reports whether v can be the host half of a probe URL.
//
// A probe template is expanded by plain substitution into
// `http://{host}:{port}/...` (harness.ExpandProbe), so v has to be a bare host
// that can sit in front of a colon and a port. Each rejection below is a value
// that would otherwise produce a URL that fails for a reason with nothing to do
// with the system under test.
func Validate(v string) error {
	v = strings.TrimSpace(v)
	switch {
	case v == "":
		return fmt.Errorf("names no address; leave it unset to use the default %s", Default)
	case strings.Contains(v, "://"):
		return fmt.Errorf("%q is a URL, not a host. The probe template supplies its own "+
			"scheme and port, so this is the host alone, such as %q", v, "host.docker.internal")
	case v == "0.0.0.0" || v == "::" || v == "[::]":
		return fmt.Errorf("%q is a bind address, not a destination. `docker compose port` "+
			"reports it as the BIND address and only its port number is usable; name the "+
			"address this process can actually reach the published port on (D-010)", v)
	case strings.HasPrefix(v, "["):
		if !strings.HasSuffix(v, "]") {
			return fmt.Errorf("%q opens a bracketed IPv6 literal and does not close it", v)
		}
		return nil
	case strings.Count(v, ":") == 1:
		return fmt.Errorf("%q carries its own port. The probe template supplies the port "+
			"from the topology, so this is the host alone", v)
	case strings.Contains(v, ":"):
		return fmt.Errorf("%q looks like an IPv6 literal and must be bracketed, as %q, "+
			"because the probe template joins host and port with a colon", v, "["+v+"]")
	}
	return nil
}
