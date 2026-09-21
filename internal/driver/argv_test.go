package driver

import (
	"fmt"
	"math"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// frozenTemplate is the directive's own driver.cmd, verbatim (4.2 / OQ-012).
const frozenTemplate = "./bin/loadgen --history {history_path} --seed {seed} --profile {profile}"

// goodSub is a substitution that passes Validate, so a test can vary one field
// at a time without every case failing validation for an unrelated reason.
func goodSub() Substitution {
	return Substitution{
		HistoryPath: "/runs/r1/w1/history.jsonl",
		Seed:        12345,
		Profile:     "steady",
		PlanPath:    "/runs/r1/w1/plan.json",
	}
}

// The three frozen placeholders must all substitute. A template placeholder
// that silently survives is handed to the driver as a literal brace string,
// and the driver then runs against a file nobody wrote.
func TestResolveArgvSubstitutesAllFrozenPlaceholders(t *testing.T) {
	sub := goodSub()
	argv, err := ResolveArgv(frozenTemplate, sub)
	if err != nil {
		t.Fatalf("ResolveArgv on the directive's own frozen template failed: %v", err)
	}
	want := []string{
		"./bin/loadgen",
		"--history", "/runs/r1/w1/history.jsonl",
		"--seed", "12345",
		"--profile", "steady",
	}
	if !reflect.DeepEqual(argv, want) {
		t.Fatalf("the frozen template did not resolve to the frozen argv.\n got: %#v\nwant: %#v", argv, want)
	}
	if left := UnresolvedPlaceholders(argv); len(left) != 0 {
		t.Fatalf("placeholders %v survived substitution of the frozen template", left)
	}
}

// {seed} must carry the UNSIGNED DECIMAL seed, because that is what
// `thesis run --seed N` accepts. A driver handed "0000000000003039" and parsing
// it as decimal runs seed 3039 instead of 12345; a different world, silently,
// which defeats I2 replay: the verdict would name a seed that does not
// reproduce what was actually run.
func TestResolveArgvSeedIsUnsignedDecimal(t *testing.T) {
	cases := []uint64{
		0,
		1,
		12345,
		1 << 32,
		1 << 63,
		math.MaxUint64,
	}
	for _, seed := range cases {
		t.Run(strconv.FormatUint(seed, 10), func(t *testing.T) {
			sub := goodSub()
			sub.Seed = seed
			argv, err := ResolveArgv(frozenTemplate, sub)
			if err != nil {
				t.Fatalf("ResolveArgv: %v", err)
			}
			got := argv[4] // --seed VALUE

			want := strconv.FormatUint(seed, 10)
			if got != want {
				t.Fatalf("seed %d substituted as %q, want the unsigned decimal %q; "+
					"any other rendering means `thesis run --seed %s` reruns a DIFFERENT world",
					seed, got, want, want)
			}
			if hex := fmt.Sprintf("%016x", seed); got == hex && seed > 9 {
				t.Fatalf("seed substituted as hex %q; a driver parsing that as decimal "+
					"silently runs a different workload", got)
			}
			if signed := strconv.FormatInt(int64(seed), 10); got == signed && signed != want {
				t.Fatalf("seed substituted as the SIGNED rendering %q; seeds above 2^63 "+
					"would reach the driver negative and I2 replay would be unreachable", got)
			}
		})
	}
}

// {plan_path} is ADDITIVE (D-021). It must substitute when a template spells
// it, because it is the only transport that can carry a resolved profile or a
// Phase 5 reduced op list to the driver.
func TestResolveArgvSubstitutesAdditivePlanPath(t *testing.T) {
	tmpl := frozenTemplate + " --plan {plan_path}"
	sub := goodSub()
	argv, err := ResolveArgv(tmpl, sub)
	if err != nil {
		t.Fatalf("ResolveArgv: %v", err)
	}
	if got := argv[len(argv)-1]; got != sub.PlanPath {
		t.Fatalf("{plan_path} resolved to %q, want %q; without it Phase 5 op shrinking "+
			"has no way to hand the driver a reduced op list (D-021)", got, sub.PlanPath)
	}
	if left := UnresolvedPlaceholders(argv); len(left) != 0 {
		t.Fatalf("placeholders %v survived: %#v", left, argv)
	}
}

// A template that does NOT spell {plan_path} stays valid. D-021's whole point is
// dual transport: an uncooperative driver ignores both the placeholder and the
// environment variable, and op shrinking is reported as not attempted rather
// than the run being rejected.
func TestResolveArgvTemplateWithoutPlanPathIsValid(t *testing.T) {
	argv, err := ResolveArgv(frozenTemplate, goodSub())
	if err != nil {
		t.Fatalf("the frozen template names no {plan_path} and must still resolve: %v", err)
	}
	for _, a := range argv {
		if strings.Contains(a, "plan.json") {
			t.Fatalf("a plan path was injected into a template that never asked for one: %#v", argv)
		}
	}
}

// This repo's own directory is `C:\AI Projects\Pro-synthesis`. A substituted
// value containing a space must remain ONE argv element. Substituting before
// splitting would hand the driver `C:\AI` as its history path and
// `Projects\Pro-synthesis\...\history.jsonl` as a stray argument: the driver
// would write its ops somewhere nobody reads, the harness would find no
// history, and the merge silently no-ops.
func TestResolveArgvValueWithSpaceStaysOneArgument(t *testing.T) {
	const spaced = `C:\AI Projects\Pro-synthesis\.prothesis\runs\r1\w1\history.jsonl`
	const spacedPlan = `C:\AI Projects\Pro-synthesis\.prothesis\runs\r1\w1\plan.json`

	sub := Substitution{
		HistoryPath: spaced,
		Seed:        7,
		Profile:     "two words",
		PlanPath:    spacedPlan,
	}
	argv, err := ResolveArgv(frozenTemplate+" --plan {plan_path}", sub)
	if err != nil {
		t.Fatalf("ResolveArgv: %v", err)
	}
	want := []string{
		"./bin/loadgen",
		"--history", spaced,
		"--seed", "7",
		"--profile", "two words",
		"--plan", spacedPlan,
	}
	if !reflect.DeepEqual(argv, want) {
		t.Fatalf("a value containing a space changed the argument COUNT or was truncated.\n"+
			" got (%d args): %#v\nwant (%d args): %#v", len(argv), argv, len(want), want)
	}
}

// The same ordering property, pushed harder: a substituted value containing
// quote characters must not be re-parsed. If substitution happened before
// splitting, the quote below would open a group and swallow the rest of the
// command line, or fail as an unterminated quote.
func TestResolveArgvDoesNotReparseSubstitutedValues(t *testing.T) {
	sub := goodSub()
	sub.HistoryPath = `/runs/a "b' c/history.jsonl`
	argv, err := ResolveArgv(frozenTemplate, sub)
	if err != nil {
		t.Fatalf("a substituted value containing quotes must not be re-parsed, got: %v", err)
	}
	if len(argv) != 7 {
		t.Fatalf("got %d args, want 7; a substituted value changed the argument count: %#v",
			len(argv), argv)
	}
	if argv[2] != sub.HistoryPath {
		t.Fatalf("history path was rewritten by re-parsing: got %q want %q", argv[2], sub.HistoryPath)
	}
}

func TestSubstitutionValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Substitution)
		wantErr string // substring; "" means the substitution must be accepted
	}{
		{"complete", func(*Substitution) {}, ""},
		{"seed zero is legal", func(s *Substitution) { s.Seed = 0 }, ""},
		{"no history path", func(s *Substitution) { s.HistoryPath = "" }, "history_path"},
		{"no profile", func(s *Substitution) { s.Profile = "" }, "profile"},
		{"no plan path", func(s *Substitution) { s.PlanPath = "" }, "plan_path"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sub := goodSub()
			tc.mutate(&sub)
			err := sub.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("a complete substitution was rejected: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("an incomplete substitution was accepted; the driver would be "+
					"invoked with a missing %s and the run would be about a workload nobody asked for",
					tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error does not name the offending field %q: %v", tc.wantErr, err)
			}
		})
	}
}

// ResolveArgv must reject an incomplete substitution BEFORE it produces an
// argv, so a partially-filled command line can never reach exec.
func TestResolveArgvRejectsIncompleteSubstitution(t *testing.T) {
	sub := goodSub()
	sub.PlanPath = ""
	argv, err := ResolveArgv(frozenTemplate, sub)
	if err == nil {
		t.Fatalf("an empty plan path was accepted; %s is ALWAYS exported (D-021) and the "+
			"driver would open \"\": argv=%#v", schema.PlanPathEnv, argv)
	}
	if argv != nil {
		t.Fatalf("a rejected substitution still produced an argv: %#v", argv)
	}
}

func TestResolveArgvRejectsUnusableTemplates(t *testing.T) {
	tests := []struct {
		name string
		tmpl string
		want string
	}{
		{"empty", "", "empty"},
		{"whitespace only", "   \t\n ", "empty"},
		{"empty program name", `'' --history {history_path}`, "empty program name"},
		{"unterminated quote", `./bin/loadgen --history "{history_path}`, "unterminated"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			argv, err := ResolveArgv(tc.tmpl, goodSub())
			if err == nil {
				t.Fatalf("driver.cmd %q was accepted and produced %#v; an unusable command "+
					"template must fail loudly, not run the wrong program", tc.tmpl, argv)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not explain the problem (want it to mention %q)", err, tc.want)
			}
		})
	}
}

// -------------------------------------------------------------------------
// UnresolvedPlaceholders
// -------------------------------------------------------------------------

func TestUnresolvedPlaceholdersDetectsEachKnownPlaceholder(t *testing.T) {
	known := []string{
		schema.PlaceholderHistoryPath,
		schema.PlaceholderSeed,
		schema.PlaceholderProfile,
		schema.PlaceholderPlanPath,
	}
	for _, ph := range known {
		t.Run(ph, func(t *testing.T) {
			argv := []string{"./bin/loadgen", "--history", "/x/h.jsonl", "--flag", ph}
			got := UnresolvedPlaceholders(argv)
			if len(got) != 1 || got[0] != ph {
				t.Fatalf("a literal %s reaching the driver was not reported (got %v); the driver "+
					"would open a file with that name and fail nowhere near prothesis.yaml", ph, got)
			}
		})
	}
}

func TestUnresolvedPlaceholdersReportsEveryOffender(t *testing.T) {
	argv := []string{
		"./bin/loadgen",
		schema.PlaceholderHistoryPath,
		schema.PlaceholderProfile,
		"--seed", "1",
	}
	got := UnresolvedPlaceholders(argv)
	if len(got) != 2 {
		t.Fatalf("got %v, want both %s and %s reported in one pass so the user fixes the "+
			"whole template at once", got, schema.PlaceholderHistoryPath, schema.PlaceholderProfile)
	}
}

func TestUnresolvedPlaceholdersAcceptsAFullyResolvedArgv(t *testing.T) {
	argv, err := ResolveArgv(frozenTemplate+" --plan {plan_path}", goodSub())
	if err != nil {
		t.Fatalf("ResolveArgv: %v", err)
	}
	if got := UnresolvedPlaceholders(argv); got != nil {
		t.Fatalf("a fully resolved argv was reported as unresolved: %v (argv %#v)", got, argv)
	}
}

// A placeholder can survive substitution by arriving INSIDE a substituted
// value: strings.Replacer does not rescan its own replacements. A profile
// literally named "{seed}" is absurd, but the guard must hold, because the same
// path is how any brace-bearing value reaches the driver.
func TestUnresolvedPlaceholdersCatchesAPlaceholderCarriedInByAValue(t *testing.T) {
	sub := goodSub()
	sub.Profile = schema.PlaceholderSeed
	argv, err := ResolveArgv(frozenTemplate, sub)
	if err != nil {
		t.Fatalf("ResolveArgv: %v", err)
	}
	got := UnresolvedPlaceholders(argv)
	if len(got) == 0 {
		t.Fatalf("a literal %s reached the driver through a substituted value and was not "+
			"reported: %#v", schema.PlaceholderSeed, argv)
	}
}

// -------------------------------------------------------------------------
// SplitCommand
// -------------------------------------------------------------------------

func TestSplitCommand(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
		why  string
	}{
		{
			name: "frozen template",
			in:   frozenTemplate,
			want: []string{"./bin/loadgen", "--history", "{history_path}",
				"--seed", "{seed}", "--profile", "{profile}"},
			why: "the directive's own driver.cmd must split into exactly its seven tokens",
		},
		{
			name: "runs of whitespace collapse",
			in:   "  a\t\tb \r\n c  ",
			want: []string{"a", "b", "c"},
			why:  "a template written across several YAML lines must behave as one command",
		},
		{
			name: "double-quoted argument containing spaces",
			in:   `./bin/loadgen --history "C:\AI Projects\Pro-synthesis\h.jsonl"`,
			want: []string{"./bin/loadgen", "--history", `C:\AI Projects\Pro-synthesis\h.jsonl`},
			why:  "this repo's own path has a space in it; splitting it would run the wrong program or write the wrong file",
		},
		{
			name: "single-quoted argument containing spaces",
			in:   `prog 'a b c' d`,
			want: []string{"prog", "a b c", "d"},
			why:  "single quotes group verbatim",
		},
		{
			name: "empty double-quoted argument is a real argument",
			in:   `prog "" tail`,
			want: []string{"prog", "", "tail"},
			why:  "dropping an empty argument shifts every argument after it, so the driver reads the wrong flag values",
		},
		{
			name: "empty single-quoted argument is a real argument",
			in:   `prog '' tail`,
			want: []string{"prog", "", "tail"},
			why:  "dropping an empty argument shifts every argument after it",
		},
		{
			name: "adjacent runs concatenate",
			in:   `prog --history"/a b/h.jsonl"`,
			want: []string{"prog", "--history/a b/h.jsonl"},
			why:  "adjacent quoted and unquoted runs are one argument, as documented",
		},
		{
			name: "quote resumes into the same argument",
			in:   `prog "a b"c"d e"`,
			want: []string{"prog", "a bcd e"},
			why:  "quoting is grouping, not a token boundary",
		},
		{
			name: "single quotes are literal inside double quotes",
			in:   `prog "it's here"`,
			want: []string{"prog", "it's here"},
			why:  "nothing is special inside a quoted group, so an apostrophe cannot open a group",
		},
		{
			name: "double quotes are literal inside single quotes",
			in:   `prog 'say "hi"'`,
			want: []string{"prog", `say "hi"`},
			why:  "single quotes are the documented way to write a double quote in an argument",
		},
		{
			name: "windows path with backslashes is not mangled",
			in:   `C:\bin\loadgen.exe --history C:\runs\r1\history.jsonl --tab \there`,
			want: []string{`C:\bin\loadgen.exe`, "--history", `C:\runs\r1\history.jsonl`, "--tab", `\there`},
			why:  "backslash is NOT an escape; treating it as one turns C:\\bin\\loadgen into C:binloadgen, silently",
		},
		{
			name: "backslashes survive inside quotes too",
			in:   `"C:\AI Projects\bin\loadgen.exe" "C:\runs\a b\h.jsonl"`,
			want: []string{`C:\AI Projects\bin\loadgen.exe`, `C:\runs\a b\h.jsonl`},
			why:  "a quoted Windows path must come out byte-identical",
		},
		{
			name: "a trailing backslash does not swallow the closing quote",
			in:   `prog "C:\runs\dir\" tail`,
			want: []string{"prog", `C:\runs\dir\`, "tail"},
			why:  "if backslash escaped the quote, the rest of the command line would be swallowed into one argument",
		},
		{
			name: "shell metacharacters are ordinary text",
			in:   `prog --history /a;rm -rf /b --x $HOME --y $(id) --z |cat`,
			want: []string{"prog", "--history", "/a;rm", "-rf", "/b", "--x", "$HOME",
				"--y", "$(id)", "--z", "|cat"},
			why: "the command never goes through a host shell, so a semicolon is an argument and not a second command",
		},
		{
			name: "empty input",
			in:   "",
			want: nil,
			why:  "an empty driver.cmd yields no arguments; ResolveArgv rejects it by name",
		},
		{
			name: "whitespace only",
			in:   " \t\n ",
			want: nil,
			why:  "whitespace is not an argument",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := SplitCommand(tc.in)
			if err != nil {
				t.Fatalf("SplitCommand(%q) failed: %v\nwhy this matters: %s", tc.in, err, tc.why)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("SplitCommand(%q)\n got: %#v\nwant: %#v\nwhy this matters: %s",
					tc.in, got, tc.want, tc.why)
			}
		})
	}
}

// An unterminated quote must be an ERROR. Silently closing it at end of input
// is how one typo in prothesis.yaml becomes a driver invoked with the wrong
// arguments and a verdict about a workload nobody ran.
func TestSplitCommandRejectsUnbalancedQuotes(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string // the quote character the error must name
	}{
		{"unterminated double quote", `./bin/loadgen --history "/a b/h.jsonl`, `"`},
		{"unterminated single quote", `./bin/loadgen --history '/a b/h.jsonl`, `'`},
		{"lone double quote", `prog "`, `"`},
		{"mismatched pair", `prog "abc'`, `"`},
		{"quote opened after a complete argument", `prog --ok "tail`, `"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := SplitCommand(tc.in)
			if err == nil {
				t.Fatalf("SplitCommand(%q) silently produced %#v; an unbalanced quote must "+
					"fail loudly rather than truncate the command and run the wrong workload",
					tc.in, got)
			}
			if got != nil {
				t.Fatalf("SplitCommand returned a partial argv alongside its error: %#v", got)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not name the offending %s quote, so the user cannot "+
					"find it in prothesis.yaml", err, tc.want)
			}
			if !strings.Contains(err.Error(), "unterminated") {
				t.Fatalf("error %q does not say the quote is unterminated", err)
			}
		})
	}
}

// Balanced quotes anywhere in the string must never trip the unbalanced check.
func TestSplitCommandAcceptsBalancedQuotesAtAnyPosition(t *testing.T) {
	for _, in := range []string{
		`""`,
		`''`,
		`"" ""`,
		`a"" b''`,
		`"a" 'b' "c"`,
	} {
		if _, err := SplitCommand(in); err != nil {
			t.Fatalf("SplitCommand(%q) rejected balanced quotes: %v", in, err)
		}
	}
}
