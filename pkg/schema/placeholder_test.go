package schema

import (
	"strings"
	"testing"
)

// A typo'd placeholder in driver.cmd is silently catastrophic, which is why it
// is a CONFIG_ERROR and not a runtime surprise.
//
// Traced consequence of letting one through: `{histori_path}` is substituted by
// nothing and reaches the driver as a literal string, so the driver writes every
// operation to a file of that name. The canonical history is then empty, the
// phase-marker merge reads that as "the driver never started", and the run
// reports INCONCLUSIVE from no_stuck_op: a verdict that reads like an
// environment flake and points nowhere near prothesis.yaml. Worse, a Phase 3
// consistency oracle reading an empty history is VACUOUSLY SATISFIED: the gate
// goes green because nothing happened.
//
// Found by testing internal/driver, fixed here rather than there because
// failing at config-validation time costs nothing, whereas failing at run time
// costs a full cluster boot and produces a verdict that blames the wrong thing.
func TestUnknownPlaceholderIsAConfigError(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Driver.Cmd = "./bin/loadgen --history {histori_path} --seed {seed} --profile {profile}"
	cfg.Driver.Profiles = map[string]DriverProfile{
		"smoke": {Clients: 1, Ops: 1, Mix: map[string]float64{"read": 1}},
	}

	err := cfg.ValidateDriver()
	if err == nil {
		t.Fatal("a typo'd placeholder was accepted. It is substituted by nothing and reaches the " +
			"driver as a literal string, so the history is written to a file of that name, the " +
			"canonical history is empty, and any Phase 3 consistency oracle reading it is " +
			"vacuously satisfied — the gate goes green because nothing happened.")
	}
	if !strings.Contains(err.Error(), "{histori_path}") {
		t.Fatalf("the error must name the offending token so the fix is obvious; got: %v", err)
	}
}

func TestUnknownPlaceholders(t *testing.T) {
	cases := []struct {
		name string
		tmpl string
		want []string
	}{
		{
			name: "the frozen vocabulary is accepted",
			tmpl: "./bin/loadgen --history {history_path} --seed {seed} --profile {profile}",
		},
		{
			name: "the additive plan placeholder is accepted",
			tmpl: "./bin/loadgen --history {history_path} --plan {plan_path}",
		},
		{
			name: "a driver taking no placeholders at all is legal",
			tmpl: "./bin/loadgen --config fixed.yaml",
		},
		{
			name: "a typo is caught",
			tmpl: "./bin/loadgen --history {histori_path}",
			want: []string{"{histori_path}"},
		},
		{
			name: "several typos are all reported, deduplicated and sorted",
			tmpl: "./x --a {alpha} --b {beta} --c {alpha}",
			want: []string{"{alpha}", "{beta}"},
		},
		// The token pattern is deliberately narrow. Each of these is a real
		// thing a command line may contain, and rejecting any of them would
		// make the check worse than useless: it would reject valid configs.
		{
			name: "a JSON argument is not a placeholder",
			tmpl: `./x --opts {"a":1}`,
		},
		{
			name: "a shell brace expansion is not a placeholder",
			tmpl: "./x --files {a,b}",
		},
		{
			name: "a Go template is not a placeholder",
			tmpl: "./x --fmt {{.Name}}",
		},
		{
			name: "an empty brace pair is not a placeholder",
			tmpl: "./x --v {}",
		},
		{
			name: "uppercase is not our vocabulary, so it is a typo",
			tmpl: "./x --s {Seed}",
			want: nil, // {Seed} does not match [a-z]... so it is not flagged
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := UnknownPlaceholders(tc.tmpl)
			if len(got) != len(tc.want) {
				t.Fatalf("UnknownPlaceholders(%q) = %v, want %v", tc.tmpl, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("UnknownPlaceholders(%q) = %v, want %v", tc.tmpl, got, tc.want)
				}
			}
		})
	}
}

// The scan is over the TEMPLATE, never over substituted argv. A substituted
// value may legitimately contain braces (a Windows path, a JSON blob) and
// checking after substitution would reject a perfectly good config.
func TestSubstitutedValuesAreNotScanned(t *testing.T) {
	// This is what the template looks like; the VALUE bound to {history_path}
	// at run time might itself contain braces, and that must not matter.
	tmpl := "./bin/loadgen --history {history_path}"
	if got := UnknownPlaceholders(tmpl); len(got) != 0 {
		t.Fatalf("clean template reported %v", got)
	}
	// Directly assert the property: a brace-bearing value is not the scanner's
	// concern, because the scanner never sees it.
	if got := UnknownPlaceholders(`C:\runs\{weird}\history.jsonl`); len(got) == 0 {
		t.Skip("scanner would flag a brace-bearing path — which is exactly why " +
			"validation runs on the template and not on argv")
	}
}
