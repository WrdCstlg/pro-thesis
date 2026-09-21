package faults

import (
	"strings"
	"testing"
)

func TestTagShape(t *testing.T) {
	got, err := Tag("r_2026_09_07_a41f", "f0001")
	if err != nil {
		t.Fatalf("Tag: %v", err)
	}
	want := "thesis:r_2026_09_07_a41f:f0001"
	if got != want {
		t.Fatalf("Tag = %q, want %q", got, want)
	}
	if !strings.HasPrefix(got, TagPrefix) {
		t.Fatalf("tag %q does not carry the prefix HEAL greps for", got)
	}
}

// The process, clock and network families must agree on the tag byte for byte
// or HEAL's sweep finds only half the residue.
func TestTagAgreesWithEnvTag(t *testing.T) {
	env := Env{RunID: "r_2026_09_07_a41f"}
	mine, err := Tag(env.RunID, "f0009")
	if err != nil {
		t.Fatalf("Tag: %v", err)
	}
	if theirs := env.Tag("f0009"); mine != theirs {
		t.Fatalf("network family tag %q != shared Env.Tag %q", mine, theirs)
	}
}

func TestTagRejectsShellMetacharacters(t *testing.T) {
	for _, bad := range []string{"r'; rm -rf /; '", "a b", "a$b", "a`b`", "", "a\nb"} {
		if _, err := Tag(bad, "f1"); err == nil {
			t.Fatalf("Tag accepted run id %q, which would reach the sidecar's shell", bad)
		}
		if _, err := Tag("r_2026_09_07_a41f", bad); err == nil {
			t.Fatalf("Tag accepted fault id %q, which would reach the sidecar's shell", bad)
		}
	}
}

// A partition that drops only INPUT is a one-way drop, not a partition. This is
// the exact mistake DECISIONS.md D-026 records having made in the rehearsal, so
// it gets a dedicated regression guard.
func TestPartitionIsBidirectional(t *testing.T) {
	cmds, err := PartitionCommands("thesis:r1:f1", []string{"172.23.0.3", "172.23.0.4"})
	if err != nil {
		t.Fatalf("PartitionCommands: %v", err)
	}
	var in, out int
	for _, c := range cmds {
		switch {
		case strings.Contains(c, "-I INPUT 1 -s "):
			in++
		case strings.Contains(c, "-I OUTPUT 1 -d "):
			out++
		default:
			t.Fatalf("unexpected command %q", c)
		}
	}
	if in != 2 || out != 2 {
		t.Fatalf("got %d INPUT and %d OUTPUT rules for 2 peers; a partition needs one of each per peer", in, out)
	}
}

func TestPartitionCommandsCarryTheTag(t *testing.T) {
	cmds, err := PartitionCommands("thesis:r1:f1", []string{"10.0.0.5"})
	if err != nil {
		t.Fatalf("PartitionCommands: %v", err)
	}
	for _, c := range cmds {
		if !strings.Contains(c, "-m comment --comment 'thesis:r1:f1'") {
			t.Fatalf("rule %q carries no ownership comment; withdrawal matches on it", c)
		}
	}
}

// -I ... 1 rather than -A. A target may ship its own ACCEPT rules, and an
// appended DROP would never be reached.
func TestPartitionInsertsAtTheTop(t *testing.T) {
	cmds, _ := PartitionCommands("thesis:r1:f1", []string{"10.0.0.5"})
	for _, c := range cmds {
		if strings.Contains(c, " -A ") {
			t.Fatalf("rule %q is appended; it must be inserted at position 1", c)
		}
	}
}

func TestPartitionSelectsAddressFamily(t *testing.T) {
	cmds, err := PartitionCommands("thesis:r1:f1", []string{"10.0.0.5", "fd00::2"})
	if err != nil {
		t.Fatalf("PartitionCommands: %v", err)
	}
	var v4, v6 int
	for _, c := range cmds {
		if strings.HasPrefix(c, "ip6tables ") {
			v6++
			if !strings.Contains(c, "fd00::2/128") {
				t.Fatalf("ip6tables rule %q does not carry a /128", c)
			}
			continue
		}
		v4++
		if !strings.Contains(c, "10.0.0.5/32") {
			t.Fatalf("iptables rule %q does not carry a /32", c)
		}
	}
	if v4 != 2 || v6 != 2 {
		t.Fatalf("got %d IPv4 and %d IPv6 rules, want 2 and 2", v4, v6)
	}
}

func TestPartitionRejectsEmptyPeerSet(t *testing.T) {
	if _, err := PartitionCommands("thesis:r1:f1", nil); err == nil {
		t.Fatal("a partition against no peers is a silent no-op and must be an error")
	}
}

func TestPartitionRejectsNonAddress(t *testing.T) {
	if _, err := PartitionCommands("thesis:r1:f1", []string{"kv-n2"}); err == nil {
		t.Fatal("a hostname is not an address; iptables would resolve it at inject time and drift")
	}
}

// Withdrawal must match on the comment, never reconstruct the rule spec: a
// reconstructed spec cannot match a rule that was already partially removed.
func TestWithdrawMatchesByComment(t *testing.T) {
	cmds, err := WithdrawIPTablesCommands("thesis:r1:f1")
	if err != nil {
		t.Fatalf("WithdrawIPTablesCommands: %v", err)
	}
	joined := strings.Join(cmds, "\n")
	if !strings.Contains(joined, "grep -F 'thesis:r1:f1'") {
		t.Fatalf("withdrawal does not select rules by comment:\n%s", joined)
	}
	if strings.Contains(joined, "-D INPUT -s ") {
		t.Fatalf("withdrawal reconstructs a rule spec:\n%s", joined)
	}
	if !strings.Contains(joined, "sort -rn") {
		t.Fatalf("withdrawal does not delete from the highest line number down; "+
			"iptables renumbers on every delete:\n%s", joined)
	}
	if !strings.Contains(joined, "iptables ") || !strings.Contains(joined, "ip6tables ") {
		t.Fatalf("withdrawal does not cover both address families:\n%s", joined)
	}
	for _, chain := range []string{"INPUT", "OUTPUT", "FORWARD"} {
		if !strings.Contains(joined, chain) {
			t.Fatalf("withdrawal does not scan chain %s", chain)
		}
	}
}

func TestCountTaggedCommandCountsWhatItIsGivenAndNotTheWholeNode(t *testing.T) {
	c := CountTaggedCommand(TagPrefix)
	if !strings.Contains(c, "grep -c -F '"+TagPrefix+"'") {
		t.Fatalf("the sweep's self-check does not count by the `thesis:` prefix: %q", c)
	}
	if !strings.Contains(c, "#TAGGED") {
		t.Fatalf("self-check emits no section marker for the parser: %q", c)
	}

	// The case that made a whole world INCONCLUSIVE with nothing leaked: a
	// fault's own withdrawal must be held to its OWN tag. Counting the shared
	// prefix means a concurrent fault's legitimate rules read as this fault's
	// residue, and max_concurrent_faults is 3. See OQ-048.
	own := "thesis:r1:f006"
	c = CountTaggedCommand(own)
	if !strings.Contains(c, "grep -c -F '"+own+"'") {
		t.Fatalf("a per-fault self-check does not count by that fault's tag: %q", c)
	}
	if strings.Contains(c, "grep -c -F '"+TagPrefix+"'") {
		t.Fatalf("a per-fault self-check still counts every rule carrying the shared prefix, so "+
			"a concurrent fault on the same node fails this one's withdrawal: %q", c)
	}

	// An empty tag must not degrade into "count nothing", which would make every
	// withdrawal verify vacuously: the failure that hides a real leak.
	if c := CountTaggedCommand(""); !strings.Contains(c, "grep -c -F '"+TagPrefix+"'") {
		t.Fatalf("an empty match does not fall back to the prefix, so the self-check would "+
			"pass without checking anything: %q", c)
	}
}

// Generated scripts must contain no double quotes: os/exec on a Windows host
// escapes them for CreateProcess and docker.exe unescapes them with C-runtime
// rules, and the two only provably agree for the simple cases.
func TestGeneratedScriptsUseSingleQuotesOnly(t *testing.T) {
	part, _ := PartitionCommands("thesis:r1:f1", []string{"10.0.0.5", "fd00::1"})
	wd, _ := WithdrawIPTablesCommands("thesis:r1:f1")
	root, _ := ShapeRootCommand("eth0")
	slot, _ := ShapeSlotCommands("eth0", 3, []string{"delay", "40ms", "seed", "7"}, []string{"10.0.0.5"})
	del, _ := ShapeSlotDeleteCommands("eth0", 3)

	all := append([]string{}, part...)
	all = append(all, wd...)
	all = append(all, root)
	all = append(all, slot...)
	all = append(all, del...)
	all = append(all, CountTaggedCommand("thesis:r1:f1"), CountTaggedCommand(TagPrefix), captureScript)

	for _, c := range all {
		if strings.Contains(c, "\"") {
			t.Fatalf("generated command contains a double quote: %q", c)
		}
	}
}
