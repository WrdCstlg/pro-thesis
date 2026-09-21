package faults

import (
	"fmt"
	"net"
	"regexp"
	"strings"
)

// Everything this package interpolates into a shell script that runs inside the
// sidecar is validated first.
//
// The script reaches `sh -c` as one argv element, so there is no host shell in
// the path on any platform, but the sidecar's own shell will happily obey a
// metacharacter that arrives inside a node id or a parameter value. Validating
// at the boundary is cheaper and more legible than quoting at every use site,
// and it fails loudly instead of producing a mis-scoped rule.
//
// Generated scripts additionally contain NO double quotes, only single ones.
// That is a Windows-host concern: os/exec escapes an argument containing a
// double quote for CreateProcess, docker.exe unescapes it with C-runtime rules,
// and the two agree, but only for the cases both implement identically. Using
// one quoting character removes the question.

var (
	// runIDPattern matches the normative run id shape r_YYYY_MM_DD_hhhh. The
	// authority on run ids is pkg/schema; this is a shell-safety check, not a
	// second definition.
	tokenPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.\-]{0,63}$`)

	// ifacePattern matches a Linux interface name: IFNAMSIZ is 16 including the
	// NUL, so 15 characters.
	ifacePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.\-]{0,14}$`)

	// numberPattern matches an unsigned decimal, with an optional fractional
	// part. Fault parameter values are stored as SOURCE TEXT and re-emitted
	// verbatim (pkg/schema Param), so "30" must reach netem as "30" and never
	// as "30.000000".
	numberPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)(\.[0-9]+)?$`)

	// containerPattern matches a container id or name.
	containerPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.\-]{0,127}$`)
)

func checkToken(what, s string) error {
	if !tokenPattern.MatchString(s) {
		return fmt.Errorf("faults: %s %q is not a safe identifier "+
			"(want 1-64 characters from A-Z a-z 0-9 _ . -)", what, s)
	}
	return nil
}

func checkIface(s string) error {
	if !ifacePattern.MatchString(s) {
		return fmt.Errorf("faults: %q is not an interface name", s)
	}
	return nil
}

func checkNumber(what, s string) error {
	if !numberPattern.MatchString(s) {
		return fmt.Errorf("faults: %s %q is not an unsigned decimal number", what, s)
	}
	return nil
}

func checkContainer(s string) error {
	if !containerPattern.MatchString(s) {
		return fmt.Errorf("faults: %q is not a container id or name", s)
	}
	return nil
}

// Tag builds the ownership tag carried by every injected iptables rule.
//
// The `thesis:` prefix is what HEAL greps for, so it is a hard prefix and not a
// formatting choice: a rule that does not carry it is invisible to residual
// verification and will outlive the run.
func Tag(runID, faultID string) (string, error) {
	if err := checkToken("run id", runID); err != nil {
		return "", err
	}
	if err := checkToken("fault id", faultID); err != nil {
		return "", err
	}
	return TagPrefix + runID + ":" + faultID, nil
}

// TagPrefix is the ownership marker HEAL scans for. Every rule this package
// installs carries it.
const TagPrefix = "thesis:"

// MustTag is Tag for values fixed in source or already validated.
func MustTag(runID, faultID string) string {
	t, err := Tag(runID, faultID)
	if err != nil {
		panic(err)
	}
	return t
}

// checkAddr validates an address that will be interpolated into an iptables or
// tc argument, and reports whether it is IPv6.
func checkAddr(s string) (isV6 bool, err error) {
	ip := net.ParseIP(s)
	if ip == nil {
		return false, fmt.Errorf("faults: %q is not an IP address", s)
	}
	if ip.To4() != nil {
		return false, nil
	}
	return true, nil
}

// script joins commands into one `sh -c` program.
//
// `set -e` is deliberately NOT applied globally: an injection script wants to
// abort on the first failure, but a withdrawal script must attempt every
// deletion even after one fails, or a partial withdrawal becomes an
// unrecoverable one. Each builder says which it wants.
func script(abortOnError bool, cmds []string) string {
	var b strings.Builder
	if abortOnError {
		b.WriteString("set -e\n")
	}
	for _, c := range cmds {
		b.WriteString(c)
		b.WriteByte('\n')
	}
	return b.String()
}
