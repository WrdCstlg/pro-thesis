package integration

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// fixtureFile reads a file from testdata/kvfixture, which lives two directories
// up from this package.
//
// It fails rather than skips when the file is missing. A skip here would be a
// silent hole: these are the only assertions in the tree that the fixture module
// and the schema module still agree, and a vanished fixture is precisely the
// event they exist to report.
func fixtureFile(t *testing.T, rel string) []byte {
	t.Helper()
	path := filepath.Join("..", "..", "testdata", "kvfixture", filepath.FromSlash(rel))
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the fixture's %s: %v\n"+
			"This test asserts that the kvfixture module and pkg/schema still agree on the wire "+
			"contracts they each declare independently. If the fixture moved, move this test with it.", rel, err)
	}
	return b
}

// TestFixtureConfigDecodesAndValidates is the `thesis up` path in miniature.
//
// testdata/kvfixture/prothesis.yaml is the config the Phase 0 definition of done
// runs against. If pkg/schema cannot decode it, `thesis up` cannot start, and
// the failure would otherwise appear as a CONFIG_ERROR against the project's own
// reference configuration.
//
// Both validators are asserted deliberately. ValidateForTopology is what up/down
// call in Phase 0; Validate is the full check the later phases call. The fixture
// config declares a driver, a perturber budget and three run profiles, so it
// must satisfy both: a config that only passes the weaker check would be a
// fixture that cannot be driven.
func TestFixtureConfigDecodesAndValidates(t *testing.T) {
	cfg, err := schema.DecodeConfig(fixtureFile(t, "prothesis.yaml"))
	if err != nil {
		t.Fatalf("pkg/schema rejected the fixture's own prothesis.yaml: %v", err)
	}

	if err := cfg.ValidateForTopology(); err != nil {
		t.Fatalf("ValidateForTopology (the check `thesis up`/`down` run): %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate (the full check): %v", err)
	}

	if cfg.Harness.Backend != schema.BackendCompose {
		t.Errorf("harness.backend = %q, want %q", cfg.Harness.Backend, schema.BackendCompose)
	}
	if got := len(cfg.Harness.Nodes); got != 3 {
		t.Fatalf("harness.nodes has %d entries, want 3 (one compose service per node, brief D-F)", got)
	}

	// D-F: container IPs are not routable from a Windows host, so every node
	// needs its own published client port and the health probe must be
	// host-side. A probe that names a container hostname would pass review and
	// then time out on this machine.
	//
	// Every node must also actually be covered by a probe. Phase 0 definition of
	// done (a) is that `thesis up` boots the fixture cleanly, and "cleanly" is
	// only observable through the health probes: an unprobed node would let up
	// report success over a node that never joined.
	probed := map[string]bool{}
	for _, h := range cfg.Harness.Health {
		if !bytes.Contains([]byte(h.Probe), []byte("{host}")) || !bytes.Contains([]byte(h.Probe), []byte("{port}")) {
			t.Errorf("health probe %q does not use {host}/{port}; container IPs are unreachable from the host (brief D-F)", h.Probe)
		}
		tgt, err := schema.ParseTarget(h.Node)
		if err != nil {
			t.Errorf("health target %q does not parse: %v", h.Node, err)
			continue
		}
		switch tgt.Kind {
		case schema.TargetNode:
			if _, ok := cfg.Node(tgt.Node); !ok {
				t.Errorf("health target %q names no node in harness.nodes", h.Node)
				continue
			}
			probed[tgt.Node] = true

		case schema.TargetWildcard:
			// `kv:*` resolves over the LOGICAL group, which is what OQ-016's
			// resolution made possible. A wildcard matching nothing is the
			// failure mode that let the fixture health-check zero nodes, so it
			// is an error here rather than a silent skip.
			matched := cfg.NodesOfService(tgt.Service)
			if len(matched) == 0 {
				t.Errorf("health target %q matches no node; a probe that matches nothing passes "+
					"vacuously and `thesis up` would report a cluster healthy that never formed", h.Node)
				continue
			}
			for _, n := range matched {
				probed[n.ID] = true
			}

		default:
			t.Errorf("health target %q uses selector kind %v, which Phase 0 cannot resolve", h.Node, tgt.Kind)
		}
	}
	for _, n := range cfg.Harness.Nodes {
		if !probed[n.ID] {
			t.Errorf("node %q has no health probe; `thesis up` would report success without ever checking it", n.ID)
		}
	}

	// OQ-016, settled. See DECISIONS.md D-020.
	//
	// The canary that used to stand here has fired and been retired. It asserted
	// that no node declared `service: kv`, because the wildcard `kv:*` could not
	// resolve while the logical group and the compose service were one field.
	//
	// The resolution splits them: `service` is the LOGICAL group the frozen
	// grammar targets, `compose_service` is the physical service that publishes
	// a port. So the directive's §4.2 sample is now expressible verbatim, and
	// what must be asserted is the opposite of what the canary asserted: that
	// the wildcard covers the whole group.
	//
	// The rejected alternative is still worth naming, because it stays tempting:
	// reinterpreting the token before the colon as a prefix glob so `kv:*`
	// matches `kv-n1` would avoid the new field, but it silently rewrites the
	// frozen grammar and with it `minority(kv)`, `majority(kv)` and the
	// constraint string "never partition more than minority of kv".
	const group = "kv"
	var inGroup, distinctCompose = 0, map[string]bool{}
	for _, n := range cfg.Harness.Nodes {
		if n.Service == group {
			inGroup++
			distinctCompose[n.EffectiveComposeService()] = true
		}
	}
	if inGroup != 3 {
		t.Errorf("%d nodes declare service %q, want 3; minority(%s) and majority(%s) resolve over "+
			"this group and Phase 2's definition of done depends on it", inGroup, group, group, group)
	}
	if len(distinctCompose) != inGroup {
		t.Errorf("the %d nodes in group %q map onto only %d compose services; two logical nodes "+
			"sharing one container cannot be partitioned from each other",
			inGroup, group, len(distinctCompose))
	}
	// minority(kv) over 3 nodes is 1, majority is 2. If the group ever has fewer
	// than 3 members, minority(kv) is 0 nodes and a partition fault becomes a
	// silent no-op that still reports as injected.
	if inGroup >= 3 && (inGroup-1)/2 < 1 {
		t.Errorf("minority(%s) resolves to 0 nodes at group size %d", group, inGroup)
	}

	// Every run profile must name a driver profile that exists, or the run
	// aborts after boot rather than before it.
	for _, name := range cfg.ProfileNames() {
		p, _ := cfg.Profile(name)
		if _, ok := cfg.DriverProfileByName(p.DriverProfile); !ok {
			t.Errorf("profile %q names driver_profile %q, which driver.profiles does not define", name, p.DriverProfile)
		}
	}

	// The driver command is the frozen §4.2 template. The fixture's loadgen is
	// invoked through it verbatim, so the placeholders it consumes are part of
	// the contract between the two modules.
	for _, ph := range []string{"{history_path}", "{seed}", "{profile}"} {
		if !bytes.Contains([]byte(cfg.Driver.Cmd), []byte(ph)) {
			t.Errorf("driver.cmd %q is missing placeholder %s", cfg.Driver.Cmd, ph)
		}
	}
}

// TestFixtureGoldenHistoryParsesThroughSchema.
//
// The fixture writes history with its own hand-declared Record type, because it
// is a separate stdlib-only module and cannot import pkg/schema. Nothing but
// this test checks that the two declarations still describe the same bytes.
//
// The consequence of drift is specific and bad: pkg/schema's reader tolerates a
// malformed line by design (an external load generator must not be able to abort
// a 500,000-line read), so a field-level mismatch does not crash; it silently
// drops records, and a consistency oracle then returns PASS on a history with
// holes in it.
func TestFixtureGoldenHistoryParsesThroughSchema(t *testing.T) {
	raw := fixtureFile(t, "golden/history.jsonl")

	hr := schema.NewHistoryReader(bytes.NewReader(raw))
	var (
		entries   []schema.HistoryEntry
		lines     int
		invokes   = map[int64]bool{}
		completed = map[int64]bool{}
		metaSeen  bool
	)
	for {
		e, err := hr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("line %d of the fixture's golden history did not parse: %v", hr.LineNo(), err)
		}
		lines++

		// Validate is not called by Next on purpose. Calling it here is the
		// whole point: the fixture's emitter must produce records the schema
		// considers well-formed, not merely records that json.Unmarshal accepts.
		if err := e.Validate(); err != nil {
			t.Fatalf("line %d is not a valid history record: %v\n  %s", hr.LineNo(), err, hr.Raw())
		}

		// The fixture carries its extensions in a namespaced `meta` object that
		// schema.HistoryEntry does not model. That is by design (meta must not
		// be soundness-bearing) but it means a merge that re-emits parsed
		// entries DROPS it. Recording the fact here so the Phase 1 recorder
		// merge is written against reality: merge by copying raw lines, not by
		// re-encoding HistoryEntry.
		if bytes.Contains(hr.Raw(), []byte(`"meta":`)) {
			metaSeen = true
			round, err := e.MarshalLine()
			if err != nil {
				t.Fatalf("line %d: MarshalLine: %v", hr.LineNo(), err)
			}
			if bytes.Contains(round, []byte(`"meta":`)) {
				t.Errorf("line %d: schema.HistoryEntry unexpectedly preserved `meta`; "+
					"if it now models meta, the merge comment above is stale", hr.LineNo())
			}
		}

		if e.RecordKind() == schema.RecordOperation {
			if e.OpID == nil {
				t.Fatalf("line %d: operation record has no op_id; nothing can pair it with its completion\n  %s",
					hr.LineNo(), hr.Raw())
			}
			switch e.Type {
			case schema.HistoryInvoke:
				if invokes[*e.OpID] {
					t.Errorf("line %d: op_id %d invoked twice", hr.LineNo(), *e.OpID)
				}
				invokes[*e.OpID] = true
			case schema.HistoryOK, schema.HistoryFail, schema.HistoryInfo:
				if !invokes[*e.OpID] {
					t.Errorf("line %d: op_id %d completes without an invoke", hr.LineNo(), *e.OpID)
				}
				completed[*e.OpID] = true
			}
		}
		entries = append(entries, *e)
	}

	if lines == 0 {
		t.Fatal("the fixture's golden history is empty; it is supposed to contain a reproduced violation")
	}
	if !metaSeen {
		t.Error("no record carried `meta`; the fixture is documented to emit it, so either the fixture " +
			"changed or this test is reading the wrong file")
	}
	for id := range invokes {
		if !completed[id] {
			t.Errorf("op_id %d was invoked and never completed; a checker cannot bound its interval", id)
		}
	}

	// The golden history is documented as sorted by t_ns (provebug sorts it),
	// which is what lets an oracle stream it. The live loadgen output is NOT
	// sorted (it timestamps at the true event instant, not under the writer
	// mutex) so this asserts a property of the GOLDEN artifact only.
	for i := 1; i < len(entries); i++ {
		if entries[i].TNS < entries[i-1].TNS {
			t.Fatalf("golden history is not sorted by t_ns at record %d (%d < %d)",
				i+1, entries[i].TNS, entries[i-1].TNS)
		}
	}

	// D-C: t_ns is Unix EPOCH nanoseconds. The directive's example values are
	// epoch MICROSECONDS and are wrong; if the fixture had copied them, every
	// epoch->start_ms conversion in Phase 3 would be off by 1000x and the
	// phase-aware assertion of invariant I5 would silently compare against
	// nothing. 1e18 ns is 2001-09-09; 1e19 is year 2286.
	const minEpochNS = int64(1e18)
	const maxEpochNS = int64(9e18)
	if entries[0].TNS < minEpochNS || entries[0].TNS > maxEpochNS {
		t.Errorf("first t_ns is %d, which is not plausible Unix EPOCH nanoseconds "+
			"(brief D-C: the field name governs, not the directive's example values)", entries[0].TNS)
	}
}

// TestFixtureWitnessIsAConformantOracleOutput.
//
// golden/witness.json is the artifact PRO-THESIS must independently reproduce in
// Phase 3: it is the fixture's own, dependency-free proof that the anomaly is
// real. If it is not a conformant prothesis.oracle_output/v1 document, the Phase
// 3 comparison has nothing well-formed to compare against and a PASS from
// PRO-THESIS would be uninformative, which is the exact failure the fixture
// exists to rule out.
func TestFixtureWitnessIsAConformantOracleOutput(t *testing.T) {
	raw := fixtureFile(t, "golden/witness.json")

	out, err := schema.ParseOracleOutput(raw)
	if err != nil {
		t.Fatalf("the fixture's golden witness is not a parseable oracle output: %v", err)
	}
	if err := out.Validate(); err != nil {
		t.Fatalf("the fixture's golden witness is not a VALID oracle output: %v", err)
	}

	if out.Status != schema.StatusViolated {
		t.Errorf("witness status = %q, want %q; a witness that does not assert a violation proves nothing",
			out.Status, schema.StatusViolated)
	}
	if !out.ValidIn(schema.PhaseAssert) {
		t.Errorf("witness valid_phases = %v, which does not include %s; a consistency claim is only "+
			"meaningful in the phase where the assertion runs", out.ValidPhases, schema.PhaseAssert)
	}
	if out.Explanation == "" {
		t.Error("witness carries no explanation; a violation a human cannot read is not a finding")
	}

	// The witness must name the operations it accuses, and those op_ids must
	// actually occur in the golden history. A witness pointing at operations
	// that are not in the history is the single easiest way for a reproduction
	// claim to be quietly false.
	if len(out.Witness.OpIDs) == 0 {
		t.Fatal("witness names no op_ids")
	}
	if out.Witness.Key == "" {
		t.Error("witness names no key; the fixture's anomaly is key-scoped and the key is what makes it checkable")
	}

	inHistory := map[int64]bool{}
	hr := schema.NewHistoryReader(bytes.NewReader(fixtureFile(t, "golden/history.jsonl")))
	for {
		e, err := hr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("golden history line %d: %v", hr.LineNo(), err)
		}
		if e.OpID != nil {
			inHistory[*e.OpID] = true
		}
	}
	var missing []int64
	for _, id := range out.Witness.OpIDs {
		if !inHistory[id] {
			missing = append(missing, id)
		}
	}
	sort.Slice(missing, func(i, j int) bool { return missing[i] < missing[j] })
	if len(missing) > 0 {
		t.Errorf("the witness accuses op_ids %v, which do not appear in golden/history.jsonl; "+
			"the two golden artifacts describe different runs", missing)
	}
}
