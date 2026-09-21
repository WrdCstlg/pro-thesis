package workload

import (
	"errors"
	"testing"
)

func envMap(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestResolveBuiltin(t *testing.T) {
	p, err := Resolve("gate", envMap(nil))
	if err != nil {
		t.Fatalf("resolve gate: %v", err)
	}
	if p.Clients != 16 || p.Ops != 20000 || p.Keys != 64 {
		t.Fatalf("unexpected gate profile: %s", p)
	}
	if p.Source != "builtin" {
		t.Fatalf("source = %q", p.Source)
	}
}

// An unknown profile must be a loud CONFIG error, never a silent default. A
// driver that quietly runs `smoke` when asked for `gate` has weakened the gate
// invisibly.
func TestResolveUnknownProfileIsAnError(t *testing.T) {
	_, err := Resolve("does-not-exist", envMap(nil))
	if err == nil {
		t.Fatal("expected an error for an unknown profile")
	}
	var unknown *ErrUnknownProfile
	if !errors.As(err, &unknown) {
		t.Fatalf("expected ErrUnknownProfile, got %T: %v", err, err)
	}
}

// A harness that CAN pass the resolved profile must win over the built-in
// table, otherwise editing prothesis.yaml changes nothing and the lock covers
// a value that does not govern the run.
func TestEnvironmentOverridesBuiltin(t *testing.T) {
	p, err := Resolve("gate", envMap(map[string]string{
		"PROTHESIS_CLIENTS":   "3",
		"PROTHESIS_OPS":       "77",
		"PROTHESIS_MIX_READ":  "900",
		"PROTHESIS_MIX_WRITE": "100",
	}))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if p.Clients != 3 || p.Ops != 77 {
		t.Fatalf("env override ignored: %s", p)
	}
	if p.Mix.Read != 900 || p.Mix.Write != 100 || p.Mix.Txn != 0 {
		t.Fatalf("mix override should replace the whole mix, got %s", p)
	}
	if p.Source != "env" {
		t.Fatalf("source = %q", p.Source)
	}
}

func TestResolveUnknownNameWithFullEnvIsAccepted(t *testing.T) {
	p, err := Resolve("custom", envMap(map[string]string{
		"PROTHESIS_CLIENTS":  "2",
		"PROTHESIS_OPS":      "10",
		"PROTHESIS_KEYS":     "4",
		"PROTHESIS_MIX_READ": "1000",
	}))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if p.Clients != 2 || p.Ops != 10 || p.Keys != 4 {
		t.Fatalf("unexpected profile: %s", p)
	}
}

func TestPickCoversTheWholeMix(t *testing.T) {
	p := Profile{Mix: Mix{Read: 300, Write: 400, Txn: 200, Admin: 100}}
	counts := map[string]int{}
	for i := 0; i < p.Mix.Total(); i++ {
		counts[p.Pick(i)]++
	}
	want := map[string]int{"read": 300, "write": 400, "txn": 200, "admin": 100}
	for k, v := range want {
		if counts[k] != v {
			t.Fatalf("Pick distribution for %q = %d, want %d", k, counts[k], v)
		}
	}
}

func TestValidateRejectsEmptyMix(t *testing.T) {
	p := Profile{Name: "x", Clients: 1, Ops: 1, Keys: 1}
	if err := p.Validate(); err == nil {
		t.Fatal("expected an error for an empty operation mix")
	}
}

// ---------------------------------------------------------------------------
// The `linear` profile's soundness properties
// ---------------------------------------------------------------------------

// linear exists so a per-key linearizability checker has a history it can
// SOUNDLY consume. Every assertion here guards a property that, if lost, turns
// the consistency oracle into a permanent `inconclusive` -- which is a gate
// that reports green because nothing was checked, the second-ranked failure
// mode in PHASE3_BUILD_BRIEF section 0.
func TestLinearProfileIsSingleKeyOnly(t *testing.T) {
	p, err := Resolve("linear", envMap(nil))
	if err != nil {
		t.Fatalf("resolve linear: %v", err)
	}

	// The fixture's txn reads one key and writes a DIFFERENT, independently
	// drawn key, so ANY txn weight breaks the precondition of the locality
	// theorem and the checker must refuse the whole history (brief D-A).
	if p.Mix.Txn != 0 {
		t.Fatalf("linear has txn weight %d; the fixture's txn is MULTI-KEY, so a per-key "+
			"linearizability checker must refuse every history this profile produces", p.Mix.Txn)
	}
	// admin touches no key at all, and the checker refuses an operation whose
	// `f` it does not model rather than guessing that it is a no-op.
	if p.Mix.Admin != 0 {
		t.Fatalf("linear has admin weight %d; admin names no key and is not modelled by the "+
			"register checker, which refuses rather than assuming it is a no-op", p.Mix.Admin)
	}
	if p.Mix.Read <= 0 {
		t.Fatal("linear has no read weight; a history of writes alone is trivially " +
			"linearizable and is no evidence at all")
	}
	if p.Mix.Write <= 0 {
		t.Fatal("linear has no write weight; with no writes there is nothing for a read to " +
			"be stale about")
	}

	// The op budget must be far above what `smoke` retires. smoke is single-key
	// too, and the ONLY reason it cannot serve is that 500 ops finish in about
	// 700ms -- long before a fault window opening at @8200ms. A budget that
	// drifted back down to smoke's size would leave the profile sound but
	// unable to observe anything.
	if p.Ops <= builtin["smoke"].Ops*10 {
		t.Fatalf("linear ops = %d; too few to keep DRIVE alive across a fault window "+
			"(smoke retires %d in ~700ms)", p.Ops, builtin["smoke"].Ops)
	}
	// Few keys is what gives the checker power: conflict density is a function
	// of operations per key.
	if p.Keys > builtin["gate"].Keys {
		t.Fatalf("linear keys = %d, gate keys = %d; linear exists to CONCENTRATE operations "+
			"onto few keys", p.Keys, builtin["gate"].Keys)
	}
}

// Pick must never return a multi-key class for the linear mix. This is the
// property TestLinearProfileIsSingleKeyOnly asserts, checked through the code
// path loadgen actually uses to choose an operation.
func TestLinearProfileNeverPicksAMultiKeyOperation(t *testing.T) {
	p, err := Resolve("linear", envMap(nil))
	if err != nil {
		t.Fatalf("resolve linear: %v", err)
	}
	for i := 0; i < p.Mix.Total(); i++ {
		switch got := p.Pick(i); got {
		case "read", "write":
		default:
			t.Fatalf("Pick(%d) = %q; linear must emit only single-key operations", i, got)
		}
	}
}
