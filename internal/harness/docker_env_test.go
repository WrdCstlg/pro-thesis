package harness

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
)

// helperEnvVar marks a re-execution of this test binary as the stand-in for
// `docker`. The stand-in exists because the property under test lives in the
// CHILD's environment, and the only honest way to read a child's environment
// is to be the child.
const helperEnvVar = "PROTHESIS_HARNESS_TEST_HELPER"

// TestHelperProcessPrintsTheAttestationSwitch is not a test. Run as a child by
// childSeesAttestationSwitch it prints the one variable and exits; run by the
// suite it returns at once.
func TestHelperProcessPrintsTheAttestationSwitch(t *testing.T) {
	if os.Getenv(helperEnvVar) != "print-attestation-switch" {
		return
	}
	fmt.Fprintf(os.Stdout, "BUILDX_NO_DEFAULT_ATTESTATIONS=%q\n", os.Getenv("BUILDX_NO_DEFAULT_ATTESTATIONS"))
	os.Exit(0)
}

// childSeesAttestationSwitch runs this test binary through runEnv in docker's
// place and returns what the child saw for BUILDX_NO_DEFAULT_ATTESTATIONS.
func childSeesAttestationSwitch(t *testing.T, extra []string) string {
	t.Helper()
	old := dockerBin
	dockerBin = os.Args[0]
	t.Cleanup(func() { dockerBin = old })
	t.Setenv(helperEnvVar, "print-attestation-switch")

	out, err := runEnv(context.Background(), "", extra, "-test.run=^TestHelperProcessPrintsTheAttestationSwitch$")
	if err != nil {
		t.Fatalf("stand-in docker failed: %v", err)
	}
	line := strings.TrimSpace(out)
	const prefix = "BUILDX_NO_DEFAULT_ATTESTATIONS="
	if !strings.HasPrefix(line, prefix) {
		t.Fatalf("stand-in docker printed %q, want a line starting %q", line, prefix)
	}
	return strings.Trim(strings.TrimPrefix(line, prefix), `"`)
}

// Every docker invocation the harness makes must switch off buildx's DEFAULT
// attestations. Compose attaches a provenance attestation to every image it
// builds, the attestation carries the build's own timestamps, and the
// containerd image store reports the digest of the INDEX that wraps image and
// attestation together, so two fully cached builds of one unchanged Dockerfile
// produce two different image ids. Measured in OQ-068: twenty worlds, twenty
// `sut.images` digests, one program. D-070 records that digest as the identity
// of what a world ran against; an identity that changes when nothing changed is
// not one.
//
// The inherited value is set to "0" first, so this cannot pass because the
// machine running the suite happens to export the switch itself.
//
// Mutation: drop dockerEnv from runEnv and both subtests fail on "0".
func TestEveryDockerInvocationSwitchesOffDefaultBuildAttestations(t *testing.T) {
	t.Run("with no per-request environment (run)", func(t *testing.T) {
		t.Setenv("BUILDX_NO_DEFAULT_ATTESTATIONS", "0")
		if got := childSeesAttestationSwitch(t, nil); got != "1" {
			t.Fatalf("the child saw BUILDX_NO_DEFAULT_ATTESTATIONS=%q, want \"1\": an image built by this "+
				"invocation carries a provenance attestation and its id will not survive a rebuild (OQ-068)", got)
		}
	})
	t.Run("with a per-request environment (runEnv, the compose-up path)", func(t *testing.T) {
		t.Setenv("BUILDX_NO_DEFAULT_ATTESTATIONS", "0")
		if got := childSeesAttestationSwitch(t, []string{"KV_PREFIX=thesis-slot-1"}); got != "1" {
			t.Fatalf("the child saw BUILDX_NO_DEFAULT_ATTESTATIONS=%q, want \"1\": the per-request environment "+
				"is the path `compose up` takes, which is the one that builds", got)
		}
	})
}

// The per-request environment still wins. It is the override channel D-042
// built ("a later entry wins, which is what makes this an override rather
// than a suggestion") and a project that wants attestations on its images can
// say so there, at the documented cost of an unstable `sut.images`.
//
// Mutation: append dockerEnv AFTER extra in runEnv and this fails on "1".
func TestAPerRequestEnvironmentCanStillOverrideTheSwitch(t *testing.T) {
	if got := childSeesAttestationSwitch(t, []string{"BUILDX_NO_DEFAULT_ATTESTATIONS=0"}); got != "0" {
		t.Fatalf("the child saw %q, want \"0\": the harness default must sit BEFORE the per-request "+
			"entries so that a caller's explicit choice is the later, winning one", got)
	}
}
