// Package workload resolves a driver profile NAME into concrete workload
// parameters.
//
// This exists because of a real gap in the frozen contract (OQ-012). The
// normative driver command template is
//
//	./bin/loadgen --history {history_path} --seed {seed} --profile {profile}
//
// and {profile} carries only the profile's NAME. The rich profile definition in
// prothesis.yaml -- clients, ops, mix -- has no path to the driver through that
// template. Three consequences, handled explicitly rather than papered over:
//
//  1. The driver must resolve the parameters itself. It does so from the table
//     below, which is documented in the fixture README so the mapping is
//     auditable.
//  2. A harness that CAN pass the resolved profile should, and this package
//     accepts it through PROTHESIS_* environment variables, which need no
//     change to the frozen template. Environment values win over the table.
//  3. An unknown profile name with no environment override is a CONFIG error
//     (exit 5). It must never fall back to a built-in default, because a
//     silently-defaulted profile is a gate that nobody can see has been
//     weakened.
package workload

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
)

// Mix is the operation mix. The weights are integers in parts-per-thousand so
// no float ever has to round-trip through a config or a world file.
type Mix struct {
	Read  int
	Write int
	Txn   int
	Admin int
}

// Total returns the sum of the weights.
func (m Mix) Total() int { return m.Read + m.Write + m.Txn + m.Admin }

// Profile is a resolved workload.
type Profile struct {
	Name    string
	Clients int
	Ops     int
	Keys    int
	Mix     Mix
	Source  string // "builtin" or "env" or "flags"
}

// builtin is the profile-name to parameters table.
//
// Key space matters more than it looks: a linearizability checker's ability to
// witness a stale read depends on key density, and the frozen profile schema
// has nowhere to express it. Few keys and many ops maximises conflict density
// and therefore checker power.
var builtin = map[string]Profile{
	"smoke": {Name: "smoke", Clients: 4, Ops: 500, Keys: 8, Mix: Mix{Read: 500, Write: 500}},
	"gate":  {Name: "gate", Clients: 16, Ops: 20000, Keys: 64, Mix: Mix{Read: 400, Write: 400, Txn: 200}},
	"soak":  {Name: "soak", Clients: 64, Ops: 500000, Keys: 256, Mix: Mix{Read: 300, Write: 400, Txn: 200, Admin: 100}},

	// linear is the profile a per-key linearizability checker can soundly
	// consume. Three properties, each load-bearing:
	//
	//  1. SINGLE-KEY ONLY. No txn and no admin weight, because the fixture's
	//     txn reads one key and writes a DIFFERENT one, which breaks the
	//     precondition of Herlihy & Wing's locality theorem. A checker handed
	//     a multi-key operation must refuse (PHASE3_BUILD_BRIEF D-A), so
	//     `gate` and `soak` are unusable for consistency checking BY DESIGN.
	//
	//  2. LONG ENOUGH TO SPAN THE FAULT WINDOW. `smoke` is single-key but
	//     retires 500 ops in ~700 ms and is finished long before a fault at
	//     @8200ms is injected, so it can only ever produce a clean history.
	//     The op budget below is deliberately far larger than the number of
	//     ops a world actually retires: the driver is stopped at QUIESCE, so
	//     an over-large budget costs nothing, while an under-large one ends
	//     DRIVE before the anomaly can be observed. Read/write operations are
	//     substantially cheaper than `gate`'s transactions, so matching
	//     `gate`'s 20,000 would finish BEFORE the window rather than across
	//     it: see DECISIONS D-040.
	//
	//  3. FEW KEYS. Conflict density, and therefore the checker's power to
	//     witness a stale read, is a function of operations per key. Eight
	//     keys over sixteen clients keeps per-key concurrency near two, which
	//     is what keeps the Wing & Gong search cheap while still putting
	//     thousands of operations on every key.
	"linear": {Name: "linear", Clients: 16, Ops: 60000, Keys: 8, Mix: Mix{Read: 500, Write: 500}},
}

// Names lists the built-in profile names, sorted.
func Names() []string {
	out := make([]string, 0, len(builtin))
	for k := range builtin {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ErrUnknownProfile is returned for a name with no built-in entry and no
// environment override. Callers must exit 5 (CONFIG_ERROR).
type ErrUnknownProfile struct{ Name string }

func (e *ErrUnknownProfile) Error() string {
	return fmt.Sprintf("unknown driver profile %q; known profiles: %s (set PROTHESIS_CLIENTS/PROTHESIS_OPS/PROTHESIS_MIX_* to supply one explicitly)",
		e.Name, strings.Join(Names(), ", "))
}

// Resolve turns a profile name into parameters. env is normally os.Environ
// lookups; pass a non-nil map in tests.
func Resolve(name string, env func(string) string) (Profile, error) {
	if env == nil {
		env = os.Getenv
	}
	p, known := builtin[name]
	if known {
		p.Source = "builtin"
	} else {
		p = Profile{Name: name, Keys: 64, Source: "env"}
	}

	overrode := false
	if v := env("PROTHESIS_CLIENTS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return Profile{}, fmt.Errorf("PROTHESIS_CLIENTS must be a positive integer, got %q", v)
		}
		p.Clients, overrode = n, true
	}
	if v := env("PROTHESIS_OPS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return Profile{}, fmt.Errorf("PROTHESIS_OPS must be a positive integer, got %q", v)
		}
		p.Ops, overrode = n, true
	}
	if v := env("PROTHESIS_KEYS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return Profile{}, fmt.Errorf("PROTHESIS_KEYS must be a positive integer, got %q", v)
		}
		p.Keys, overrode = n, true
	}
	mixSet := false
	for key, dst := range map[string]*int{
		"PROTHESIS_MIX_READ":  &p.Mix.Read,
		"PROTHESIS_MIX_WRITE": &p.Mix.Write,
		"PROTHESIS_MIX_TXN":   &p.Mix.Txn,
		"PROTHESIS_MIX_ADMIN": &p.Mix.Admin,
	} {
		v := env(key)
		if v == "" {
			continue
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return Profile{}, fmt.Errorf("%s must be a non-negative integer (parts per thousand), got %q", key, v)
		}
		if !mixSet {
			p.Mix = Mix{}
			mixSet = true
		}
		*dst = n
		overrode = true
	}
	if overrode {
		p.Source = "env"
	}

	if !known && !overrode {
		return Profile{}, &ErrUnknownProfile{Name: name}
	}
	if err := p.Validate(); err != nil {
		return Profile{}, err
	}
	return p, nil
}

// Validate rejects a profile that cannot generate work.
func (p Profile) Validate() error {
	if p.Clients <= 0 {
		return fmt.Errorf("profile %q: clients must be positive, got %d", p.Name, p.Clients)
	}
	if p.Ops <= 0 {
		return fmt.Errorf("profile %q: ops must be positive, got %d", p.Name, p.Ops)
	}
	if p.Keys <= 0 {
		return fmt.Errorf("profile %q: keys must be positive, got %d", p.Name, p.Keys)
	}
	if p.Mix.Total() <= 0 {
		return fmt.Errorf("profile %q: operation mix is empty", p.Name)
	}
	return nil
}

// Pick maps a uniform draw in [0, Mix.Total()) to an operation class.
func (p Profile) Pick(draw int) string {
	m := p.Mix
	if draw < m.Read {
		return "read"
	}
	draw -= m.Read
	if draw < m.Write {
		return "write"
	}
	draw -= m.Write
	if draw < m.Txn {
		return "txn"
	}
	return "admin"
}

// String renders the profile for the run banner.
func (p Profile) String() string {
	return fmt.Sprintf("%s(clients=%d ops=%d keys=%d mix=r%d/w%d/t%d/a%d source=%s)",
		p.Name, p.Clients, p.Ops, p.Keys, p.Mix.Read, p.Mix.Write, p.Mix.Txn, p.Mix.Admin, p.Source)
}
