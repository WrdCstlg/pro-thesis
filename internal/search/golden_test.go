package search

import "testing"

// TestNormalisedFixtureLinesAreGolden pins the exact template text for every
// fixture log site.
//
// A golden test here is worth more than the usual: the two failure directions
// (under- and over-normalisation) are each tested by a pairwise claim, but a
// change that shifts WHICH tokens are replaced can satisfy both pairwise tests
// while still degrading the signal; replacing `not_leader` with a placeholder,
// say, would keep the code-path pairs distinct by their event names alone. The
// golden text makes that visible in a diff.
func TestNormalisedFixtureLinesAreGolden(t *testing.T) {
	want := []string{
		`level=info event=boot node=kv-n<NUM> variant=buggy lease_ms=<NUM> tick_ms=<NUM> hb_ms=<NUM> election_ms=<NUM>-<NUM> peers=[kv-n<NUM> kv-n<NUM>] client=:<NUM> peer=:<NUM> data_dir="/data/kv-n<NUM>" seed=<NUM> node_seed=<NUM>`,
		`level=info event=role_change node=kv-n<NUM> role=follower term=<NUM> leader="kv-n<NUM>"`,
		`level=info event=election_start node=kv-n<NUM> term=<NUM> last_index=<NUM> last_term=<NUM>`,
		`level=info event=elected node=kv-n<NUM> term=<NUM> noop_index=<NUM>`,
		`level=warn event=request_error node=kv-n<NUM> status=<NUM> code=not_leader msg="no leader"`,
		`level=error event=persist_state err="disk full"`,
		`level=error event=append_wal err="disk full"`,
		`level=fatal event=truncate_committed node=kv-n<NUM> index=<NUM> commit_index=<NUM> term=<NUM> leader="kv-n<NUM>"`,
		`level=info event=shutdown node=kv-n<NUM> signal=terminated`,
		`level=info event=stopped node=kv-n<NUM>`,
	}
	if len(want) != len(fixtureLines) {
		t.Fatalf("golden list has %d entries, fixtureLines has %d", len(want), len(fixtureLines))
	}
	for i, ln := range fixtureLines {
		if got := NormalizeLine(ln); got != want[i] {
			t.Errorf("line %d normalised unexpectedly\n  raw:  %s\n  got:  %s\n  want: %s", i, ln, got, want[i])
		}
	}
}
