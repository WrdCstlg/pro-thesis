package driver

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// fixtureConfig mirrors the shape of the directive's own driver.profiles block.
func fixtureConfig() *schema.Config {
	return &schema.Config{
		Driver: schema.DriverConfig{
			Cmd: frozenTemplate,
			Profiles: map[string]schema.DriverProfile{
				"steady": {
					Clients: 16,
					Ops:     20000,
					Mix:     map[string]float64{"read": 0.4, "write": 0.4, "txn": 0.2},
				},
				"burst": {
					Clients: 64,
					Ops:     5000,
					Mix:     map[string]float64{"write": 1.0},
				},
			},
		},
	}
}

func TestNewPlanResolvesTheNamedProfile(t *testing.T) {
	p, err := NewPlan(fixtureConfig(), "steady", "/runs/r1/w1/history.jsonl", 12345)
	if err != nil {
		t.Fatalf("NewPlan: %v", err)
	}
	if p.Schema != PlanSchema {
		t.Fatalf("plan schema = %q, want %q", p.Schema, PlanSchema)
	}
	if p.Profile != "steady" || p.Seed != 12345 || p.HistoryPath != "/runs/r1/w1/history.jsonl" {
		t.Fatalf("plan lost its identity: %+v", p)
	}
	if p.Clients != 16 || p.Ops != 20000 {
		t.Fatalf("clients/ops = %d/%d, want 16/20000; the plan is the ONLY transport that can "+
			"carry them to the driver, because {profile} passes a name (OQ-012)", p.Clients, p.Ops)
	}
	want := []MixEntry{
		{Op: "read", WeightPPM: 400000},
		{Op: "txn", WeightPPM: 200000},
		{Op: "write", WeightPPM: 400000},
	}
	if !reflect.DeepEqual(p.Mix, want) {
		t.Fatalf("mix\n got: %#v\nwant: %#v", p.Mix, want)
	}
}

// The whole point of the plan file: it must survive a trip through the disk
// unchanged. A driver reads this file to learn the workload; any field that
// does not round-trip is a field the driver silently defaults instead.
func TestPlanRoundTripsThroughDisk(t *testing.T) {
	for _, profile := range []string{"steady", "burst"} {
		t.Run(profile, func(t *testing.T) {
			orig, err := NewPlan(fixtureConfig(), profile, `C:\AI Projects\Pro-synthesis\h.jsonl`, 1<<63)
			if err != nil {
				t.Fatalf("NewPlan: %v", err)
			}
			path := filepath.Join(t.TempDir(), "nested", "deeper", PlanFileName)
			if err := WritePlan(path, orig); err != nil {
				t.Fatalf("WritePlan: %v", err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read back: %v", err)
			}
			got, err := ParsePlan(data)
			if err != nil {
				t.Fatalf("ParsePlan: %v", err)
			}
			if !reflect.DeepEqual(got, orig) {
				t.Fatalf("the plan did not survive WritePlan -> ParsePlan; the driver would run a "+
					"different workload from the one the verdict names.\n got: %+v\nwant: %+v", got, orig)
			}
		})
	}
}

// ppm exists because the world file forbids bare floats (D-012): Go's
// shortest-float representation carries no cross-version stability guarantee,
// so a bare 0.4 could re-encode differently on a toolchain upgrade and move
// every hash that covers it. The conversion must therefore be exact for the
// weights the directive's own samples use.
func TestMixWeightsConvertToPPMWithoutDrift(t *testing.T) {
	tests := []struct {
		weight float64
		want   int64
	}{
		{0.4, 400000},
		{0.2, 200000},
		{0.1, 100000},
		{0.35, 350000},
		{0.125, 125000},
		{0.5, 500000},
		{1.0, 1000000},
		{0.0, 0},
		{2.5, 2500000},   // relative weights are legal; the mix need not sum to 1
		{0.000001, 1},    // one part per million
		{0.0000004, 0},   // rounds down
		{0.0000006, 1},   // rounds up
		{-0.4, -400000},  // half away from zero, in both directions
		{-0.0000006, -1}, // ...including the rounding
	}
	for _, tc := range tests {
		got := ppm(tc.weight)
		if got != tc.want {
			t.Fatalf("ppm(%v) = %d, want %d; a drifting conversion changes the plan bytes "+
				"for an unchanged config", tc.weight, got, tc.want)
		}
	}

	// And the value a driver reads back must be the weight the user wrote.
	for _, w := range []float64{0.4, 0.2, 0.1, 0.35, 0.125, 0.5, 1.0, 2.5} {
		if back := float64(ppm(w)) / 1e6; back != w {
			t.Fatalf("weight %v round-tripped as %v through parts-per-million; the driver "+
				"would run a different mix from the one prothesis.yaml declares", w, back)
		}
	}
}

// A float weight must reach the file as an integer, never as a bare float.
func TestPlanFileCarriesNoBareFloat(t *testing.T) {
	p, err := NewPlan(fixtureConfig(), "steady", "/h.jsonl", 1)
	if err != nil {
		t.Fatalf("NewPlan: %v", err)
	}
	data, err := p.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if bytes.Contains(data, []byte("0.4")) || bytes.Contains(data, []byte("0.2")) {
		t.Fatalf("the plan carries a bare float; D-012 forbids it because Go's shortest-float "+
			"rendering has no cross-version stability guarantee:\n%s", data)
	}
	if !bytes.Contains(data, []byte(`"weight_ppm": 400000`)) {
		t.Fatalf("read: 0.4 did not reach the file as 400000 parts-per-million:\n%s", data)
	}
}

// Mix ordering must come from sorting, not from Go's map iteration. Go
// randomizes map order per range, so an unsorted implementation produces a
// different plan file on every run for an identical config, and anything that
// later hashes the plan would see a change nobody made.
func TestMixOrderIsDeterministicNotMapOrder(t *testing.T) {
	cfg := &schema.Config{
		Driver: schema.DriverConfig{
			Profiles: map[string]schema.DriverProfile{
				"wide": {Clients: 1, Ops: 1, Mix: map[string]float64{
					"read": 0.3, "write": 0.2, "txn": 0.2, "scan": 0.1, "cas": 0.1, "delete": 0.1,
				}},
			},
		},
	}
	first, err := NewPlan(cfg, "wide", "/h.jsonl", 1)
	if err != nil {
		t.Fatalf("NewPlan: %v", err)
	}
	firstBytes, err := first.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	if !sort.SliceIsSorted(first.Mix, func(i, j int) bool { return first.Mix[i].Op < first.Mix[j].Op }) {
		t.Fatalf("mix is not sorted by op: %#v", first.Mix)
	}

	for i := 0; i < 200; i++ {
		p, err := NewPlan(cfg, "wide", "/h.jsonl", 1)
		if err != nil {
			t.Fatalf("NewPlan (iteration %d): %v", i, err)
		}
		b, err := p.Marshal()
		if err != nil {
			t.Fatalf("Marshal (iteration %d): %v", i, err)
		}
		if !bytes.Equal(b, firstBytes) {
			t.Fatalf("the plan file changed between two identical calls (iteration %d); "+
				"Go map order is randomized, so the mix must be sorted\nfirst: %s\n got: %s",
				i, firstBytes, b)
		}
	}
}

// An unknown profile must be an error, not an empty plan. Running the driver
// with no workload and then reporting PASS is the worst possible response to a
// typo'd driver_profile.
func TestNewPlanRejectsAnUnknownProfile(t *testing.T) {
	cfg := fixtureConfig()
	p, err := NewPlan(cfg, "no-such-profile", "/h.jsonl", 1)
	if err == nil {
		t.Fatalf("an unknown profile produced a plan instead of an error: %+v", p)
	}
	if p != nil {
		t.Fatalf("an unknown profile produced a plan alongside its error: %+v", p)
	}
	if !strings.Contains(err.Error(), "no-such-profile") {
		t.Fatalf("error does not name the missing profile: %v", err)
	}
	// The declared-profile list is rendered through sortedKeys, so the message is
	// stable. An error message whose text changes run to run (Go randomizes map
	// order) cannot be matched in a test, quoted in a bug report, or diffed in CI.
	for i := 0; i < 50; i++ {
		_, err2 := NewPlan(cfg, "no-such-profile", "/h.jsonl", 1)
		if err2 == nil {
			t.Fatalf("iteration %d accepted the unknown profile", i)
		}
		if err2.Error() != err.Error() {
			t.Fatalf("the error message is not stable across calls; the declared-profile list "+
				"is coming from map order:\n%v\n%v", err, err2)
		}
	}
	if !strings.Contains(err.Error(), "[burst steady]") {
		t.Fatalf("declared profiles are not listed in sorted order: %v", err)
	}
}

// A nil config is legal: it yields a plan carrying only what the run itself
// knows. The driver then keeps its own defaults for clients/ops/mix.
func TestNewPlanWithNilConfig(t *testing.T) {
	p, err := NewPlan(nil, "steady", "/h.jsonl", 9)
	if err != nil {
		t.Fatalf("NewPlan(nil): %v", err)
	}
	if p.Schema != PlanSchema || p.Profile != "steady" || p.Seed != 9 {
		t.Fatalf("plan lost its identity: %+v", p)
	}
	if p.Mix == nil {
		t.Fatalf("mix is nil; it must encode as [] so a driver reading the file with jq does " +
			"not have to special-case null")
	}
	if p.Clients != 0 || p.Ops != 0 {
		t.Fatalf("clients/ops = %d/%d, want 0/0 meaning 'the driver keeps its own default'",
			p.Clients, p.Ops)
	}
}

func TestMarshalEmitsAnEmptyMixAsAnArray(t *testing.T) {
	p := &Plan{Profile: "x", Seed: 1, HistoryPath: "/h.jsonl"} // Schema and Mix deliberately unset
	data, err := p.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if bytes.Contains(data, []byte("null")) {
		t.Fatalf("the plan carries a null; a driver reading it with jq would have to "+
			"special-case it:\n%s", data)
	}
	got, err := ParsePlan(data)
	if err != nil {
		t.Fatalf("Marshal produced a plan its own parser rejects: %v\n%s", err, data)
	}
	if got.Schema != PlanSchema {
		t.Fatalf("Marshal did not stamp the schema id, so ParsePlan would reject the file "+
			"it just wrote: %q", got.Schema)
	}
	if got.Mix == nil || len(got.Mix) != 0 {
		t.Fatalf("mix = %#v, want an empty array", got.Mix)
	}
}

// HTML escaping is off, so an op name containing < or > survives verbatim.
// Go escapes those by default, which would change the bytes of a file that
// later phases may hash.
func TestMarshalDoesNotHTMLEscape(t *testing.T) {
	p := &Plan{
		Profile: "x", Seed: 1, HistoryPath: "/h.jsonl",
		Mix: []MixEntry{{Op: "n1<->n2", WeightPPM: 1000000}},
	}
	data, err := p.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !bytes.Contains(data, []byte("n1<->n2")) {
		t.Fatalf("< and > were HTML-escaped, changing the bytes of the plan file:\n%s", data)
	}
}

func TestParsePlanRejectsAForeignFile(t *testing.T) {
	tests := []struct {
		name string
		data string
		want string
	}{
		{"wrong schema id", `{"schema":"prothesis.world/v1","profile":"x"}`, "schema"},
		{"missing schema id", `{"profile":"x"}`, "schema"},
		{"not json", `clients: 16`, "decode"},
		{"truncated", `{"schema":"prothesis.driver_plan/v1",`, "decode"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p, err := ParsePlan([]byte(tc.data))
			if err == nil {
				t.Fatalf("a foreign file was accepted as a driver plan: %+v", p)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

// WritePlan is atomic because the driver reads the file as it starts: a driver
// that opened a half-written plan would run a workload nobody asked for. The
// observable consequences are that the parent directory is created and that no
// temporary file is left where a driver could find it.
func TestWritePlanCreatesParentsAndLeavesNoTemporary(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "runs", "r1", "w1", PlanFileName)
	p, err := NewPlan(fixtureConfig(), "steady", "/h.jsonl", 1)
	if err != nil {
		t.Fatalf("NewPlan: %v", err)
	}
	if err := WritePlan(path, p); err != nil {
		t.Fatalf("WritePlan: %v", err)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if len(names) != 1 || names[0] != PlanFileName {
		t.Fatalf("world directory holds %v, want only %q; a leftover temporary is a file a "+
			"driver could open mid-write", names, PlanFileName)
	}

	// Overwriting must replace the file, not append to it or leave the old plan.
	p2, err := NewPlan(fixtureConfig(), "burst", "/h.jsonl", 2)
	if err != nil {
		t.Fatalf("NewPlan: %v", err)
	}
	if err := WritePlan(path, p2); err != nil {
		t.Fatalf("WritePlan (overwrite): %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	got, err := ParsePlan(data)
	if err != nil {
		t.Fatalf("ParsePlan after overwrite: %v\n%s", err, data)
	}
	if got.Profile != "burst" || got.Seed != 2 {
		t.Fatalf("overwrite left the previous plan in place: %+v", got)
	}
	if n := bytes.Count(data, []byte(`"schema"`)); n != 1 {
		t.Fatalf("the file contains %d plans; WritePlan appended instead of replacing:\n%s", n, data)
	}
}

// The plan is read by drivers that may be shell scripts using jq, so it must be
// ordinary indented JSON terminated by a newline.
func TestPlanFileIsReadableJSON(t *testing.T) {
	p, err := NewPlan(fixtureConfig(), "steady", "/h.jsonl", 1)
	if err != nil {
		t.Fatalf("NewPlan: %v", err)
	}
	data, err := p.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !bytes.HasSuffix(data, []byte("\n")) {
		t.Fatalf("plan does not end with a newline: %q", data)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatalf("plan is not valid JSON: %v\n%s", err, data)
	}
	for _, k := range []string{"schema", "profile", "seed", "history_path", "clients", "ops", "mix"} {
		if _, ok := fields[k]; !ok {
			t.Fatalf("plan is missing the %q field a driver reads:\n%s", k, data)
		}
	}
}
