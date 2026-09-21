package driver

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// Substitution is the set of values that fill the `driver.cmd` placeholders.
type Substitution struct {
	// HistoryPath fills {history_path}. The driver writes ONLY op records here;
	// the harness writes phase markers elsewhere and MergeHistory joins them.
	HistoryPath string
	// Seed fills {seed}. It is emitted in UNSIGNED DECIMAL, matching
	// `thesis run --seed N`, so a seed pasted from a verdict into a command line
	// and a seed substituted into driver.cmd are the same characters.
	Seed uint64
	// Profile fills {profile}. It is the profile NAME, not its contents:
	// the frozen template has no way to carry `clients`, `ops` or `mix`, which
	// is what PlanPath exists to fix. See OQ-012.
	Profile string
	// PlanPath fills {plan_path} and is exported as PROTHESIS_PLAN_PATH.
	//
	// ADDITIVE, reserved by DECISIONS.md D-021. It names a file carrying the
	// resolved driver profile today and a reduced operation list in Phase 5.
	// Required: the environment variable is always exported, and exporting an
	// empty path would have a driver open "" and fail confusingly.
	PlanPath string
}

// Validate checks the substitution set.
func (s Substitution) Validate() error {
	var errs schema.ValidationErrors
	if s.HistoryPath == "" {
		errs.Add("history_path", "must not be empty; the driver has nowhere to write its history")
	}
	if s.Profile == "" {
		errs.Add("profile", "must not be empty; %s substitutes the profile name", schema.PlaceholderProfile)
	}
	if s.PlanPath == "" {
		errs.Add("plan_path", "must not be empty: %s is ALWAYS exported (DECISIONS.md D-021), "+
			"and a driver handed an empty path would open \"\". Call WritePlan to produce one",
			schema.PlanPathEnv)
	}
	return errs.OrNil()
}

// replacer builds the placeholder substitution.
//
// Note that {plan_path} is included even though the environment variable is the
// primary transport: a driver may spell either, and a driver supporting neither
// ignores both. That dual transport is the point of D-021: op shrinking is then
// reported as NOT ATTEMPTED for an uncooperative driver, rather than silently
// producing a wrong minimal repro.
func (s Substitution) replacer() *strings.Replacer {
	return strings.NewReplacer(
		schema.PlaceholderHistoryPath, s.HistoryPath,
		schema.PlaceholderSeed, strconv.FormatUint(s.Seed, 10),
		schema.PlaceholderProfile, s.Profile,
		schema.PlaceholderPlanPath, s.PlanPath,
	)
}

// ResolveArgv splits a `driver.cmd` template into an argv and substitutes the
// placeholders.
//
// ORDER IS LOAD-BEARING: split first, substitute second. A substituted value may
// contain spaces (the build machine's own project directory is
// `C:\AI Projects\Pro-synthesis`) and substituting first would split that value
// across argument boundaries, handing the driver `C:\AI` as its history path and
// `Projects\Pro-synthesis\...` as a stray argument. Splitting first makes a
// placeholder's expansion incapable of changing the argument count.
func ResolveArgv(tmpl string, sub Substitution) ([]string, error) {
	if err := sub.Validate(); err != nil {
		return nil, err
	}
	tokens, err := SplitCommand(tmpl)
	if err != nil {
		return nil, err
	}
	if len(tokens) == 0 {
		return nil, fmt.Errorf("driver: driver.cmd is empty")
	}
	r := sub.replacer()
	argv := make([]string, len(tokens))
	for i, t := range tokens {
		argv[i] = r.Replace(t)
	}
	if argv[0] == "" {
		return nil, fmt.Errorf("driver: driver.cmd has an empty program name")
	}
	return argv, nil
}

// UnresolvedPlaceholders returns any known placeholder still present in argv.
//
// Used to catch a template that spells a placeholder this build does not know:
// a driver receiving a literal "{plan_path}" as an argument would open a file
// with that name and fail in a way that points nowhere near the config.
func UnresolvedPlaceholders(argv []string) []string {
	known := []string{
		schema.PlaceholderHistoryPath,
		schema.PlaceholderSeed,
		schema.PlaceholderProfile,
		schema.PlaceholderPlanPath,
	}
	var out []string
	for _, p := range known {
		for _, a := range argv {
			if strings.Contains(a, p) {
				out = append(out, p)
				break
			}
		}
	}
	return out
}

// SplitCommand splits a command template into arguments, respecting quotes.
//
// The grammar is deliberately small, and each restriction has a reason:
//
//   - Unquoted runs of non-space characters are one argument.
//   - Single quotes group verbatim. Nothing inside them is special.
//   - Double quotes group verbatim. Nothing inside them is special either.
//   - Adjacent runs concatenate: --history"/a b/h.jsonl" is ONE argument.
//   - ” and "" produce a real empty argument, because a driver may legitimately
//     want one and dropping it would change the argument positions after it.
//   - An unterminated quote is an ERROR. Silently closing it at end of input is
//     how a typo in prothesis.yaml becomes a driver invoked with the wrong
//     arguments and a verdict about a workload nobody ran.
//
// BACKSLASH IS NOT AN ESCAPE, anywhere.
//
// That is a deliberate departure from POSIX shell splitting, and the reason is
// this host: `driver.cmd` routinely contains Windows paths, and treating
// backslash as an escape would turn `C:\bin\loadgen` into `C:binloadgen`;
// silently, and with no error to point at. A double quote inside an argument is
// written by wrapping it in single quotes, which costs nothing and cannot
// mangle a path.
//
// Tabs, carriage returns and newlines separate arguments as spaces do, so a
// template written across several YAML lines behaves as one command.
func SplitCommand(s string) ([]string, error) {
	var (
		args    []string
		cur     strings.Builder
		started bool // cur holds an argument, even if it is empty
		quote   byte // 0, '\'' or '"'
		openAt  int
	)
	flush := func() {
		if started {
			args = append(args, cur.String())
			cur.Reset()
			started = false
		}
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if quote != 0 {
			if c == quote {
				quote = 0
				continue
			}
			cur.WriteByte(c)
			continue
		}
		switch c {
		case ' ', '\t', '\n', '\r', '\v', '\f':
			flush()
		case '\'', '"':
			quote = c
			openAt = i
			started = true
		default:
			cur.WriteByte(c)
			started = true
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("driver: unterminated %c quote opened at byte %d in driver.cmd: %s",
			quote, openAt, s)
	}
	flush()
	return args, nil
}
