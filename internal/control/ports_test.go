package control

import (
	"strings"
	"testing"

	"github.com/WrdCstlg/pro-thesis/internal/recorder"
)

// A world whose driver is told one set of ports while compose publishes another
// does not fail loudly: it drives a full workload against nothing, records a
// history in which every operation failed, and hands the oracle nothing to
// check. The oracle then says INCONCLUSIVE, which is correct and useless, and a
// search reading that concludes its fault told it nothing.
//
// MEASURED: `thesis search` on the reference fixture without --worker-env
// produced 78,018 records over 39,009 op ids, all 39,009 failed, zero checkable
// (run r_2026_09_09_23c5). See OQ-056. Every Phase 4 result came from a script that passed
// the flag.
func TestAWorldRefusesToDriveAgainstPortsNobodyPublished(t *testing.T) {
	// The slot allocated 19000-19002; compose ignored the band and published the
	// file's defaults, which is exactly what omitting --worker-env produces.
	want := map[string]int64{"kv-n1": 19000, "kv-n2": 19001, "kv-n3": 19002}
	published := []recorder.NodeBinding{
		{ID: "kv-n1", HostPort: 18081},
		{ID: "kv-n2", HostPort: 18082},
		{ID: "kv-n3", HostPort: 18083},
	}

	err := portsAgree(want, published)
	if err == nil {
		t.Fatal("a world whose driver targets 19000-19002 while compose published 18081-18083 " +
			"was allowed to run; every operation would fail and the history would be " +
			"unjudgeable, which reads as INCONCLUSIVE rather than as the misconfiguration it is")
	}
	// The message has to name the remedy. "ports disagree" sends a user hunting
	// through compose; --worker-env is the actual fix and it is not guessable.
	for _, want := range []string{"--worker-env", "kv-n1", "19000", "18081"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}
}

func TestPortAgreementDoesNotFireWhenItShouldNot(t *testing.T) {
	published := []recorder.NodeBinding{
		{ID: "kv-n1", HostPort: 19000},
		{ID: "kv-n2", HostPort: 19001},
	}

	// Agreement.
	if err := portsAgree(map[string]int64{"kv-n1": 19000, "kv-n2": 19001}, published); err != nil {
		t.Errorf("matching ports were rejected: %v", err)
	}

	// A serial world expects nothing: it publishes whatever the compose file says
	// and tells the driver the same, so there is no second opinion to disagree
	// with. Failing here would break every non-parallel run.
	if err := portsAgree(nil, published); err != nil {
		t.Errorf("a serial world (no expected band) was rejected: %v", err)
	}
	if err := portsAgree(map[string]int64{}, published); err != nil {
		t.Errorf("an empty expected band was rejected: %v", err)
	}

	// A node the slot allocated no port for is not evidence of anything. Not
	// every declared node has to publish one, and a health probe already reports
	// the ones that needed a port and did not get it.
	if err := portsAgree(map[string]int64{"kv-n1": 19000, "kv-n9": 0}, published); err != nil {
		t.Errorf("a node with no allocated port was treated as a mismatch: %v", err)
	}

	// A node in the band that the topology never bound cannot disagree with
	// itself; the missing binding is the harness's complaint to make, not this
	// check's.
	if err := portsAgree(map[string]int64{"kv-n1": 19000, "kv-n3": 19002}, published); err != nil {
		t.Errorf("a node absent from the topology was treated as a mismatch: %v", err)
	}
}
