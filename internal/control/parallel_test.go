package control

import (
	"context"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// netTCPAddr is net.TCPAddr under a local name, so the occupied-port test reads
// as one idea rather than as a type assertion.
type netTCPAddr = net.TCPAddr

func listenOnAny(t *testing.T) (net.Listener, error) {
	t.Helper()
	return net.Listen("tcp", "127.0.0.1:0")
}

// freePortBand returns the base of a band of `span` consecutive host ports that
// are all bindable right now.
//
// Tests must not use DefaultWorkerPortBase. A test that did was order-dependent
// and flaky: it passed inside the package (where earlier tests had shifted the
// band) and failed in isolation, because Windows leaves TIME_WAIT sockets on
// 19000 after any parallel test that actually drove traffic, and a TIME_WAIT
// socket defeats bind() without SO_REUSEADDR. A test that fails for a reason
// unrelated to what it asserts is worse than no test: it trains a reader to
// ignore the failure.
//
// Binding and immediately closing is safe here precisely because these
// listeners never ACCEPT anything (TIME_WAIT is a property of a connection
// that was established and closed, not of a listening socket) so the band is
// genuinely re-bindable on return.
//
// Note this deliberately keeps the port preflight ENABLED in the caller. The
// alternative fix, VerifyPortsFree=false, would make the test pass by disabling
// a guard; this one makes it pass by not colliding with it.
func freePortBand(t *testing.T, span int) int {
	t.Helper()
	if span < 1 {
		span = 1
	}
	for attempt := 0; attempt < 32; attempt++ {
		probe, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Skipf("could not bind a loopback port to find a free band: %v", err)
		}
		base := probe.Addr().(*netTCPAddr).Port
		_ = probe.Close()
		if base+span > 65535 {
			continue
		}

		held := make([]net.Listener, 0, span)
		ok := true
		for i := 0; i < span; i++ {
			ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", base+i))
			if err != nil {
				ok = false
				break
			}
			held = append(held, ln)
		}
		for _, ln := range held {
			_ = ln.Close()
		}
		if ok {
			return base
		}
	}
	t.Skip("could not find a free band of consecutive loopback ports")
	return 0
}

func joinRunWorld(r *Runner, ordinal int) string {
	return filepath.Join(r.runDir, WorldDirName(ordinal))
}

func parallelConfig(t *testing.T) *schema.Config {
	t.Helper()
	cfg := drainWorldConfig(t)
	cfg.Harness.Nodes = []schema.NodeConfig{
		{ID: "kv-n1", Service: "kv", ComposeService: "kv-n1"},
		{ID: "kv-n2", Service: "kv", ComposeService: "kv-n2"},
		{ID: "kv-n3", Service: "kv", ComposeService: "kv-n3"},
	}
	w := 8
	cfg.Profiles["smoke"] = schema.Profile{
		Budget:        schema.Duration(5 * time.Minute),
		Worlds:        &w,
		DriverProfile: "smoke",
	}
	return cfg
}

func newParallelRunner(t *testing.T, backend *stubBackend, drv Driver, opts ParallelOptions) (*Runner, *ParallelRunner) {
	t.Helper()
	r, err := NewRunner(RunnerOptions{
		Config:        parallelConfig(t),
		ProjectDir:    t.TempDir(),
		RunID:         "r_2026_09_08_test",
		Seed:          9001,
		Backend:       backend,
		Driver:        drv,
		Telemetry:     stubTelemetry{},
		OracleEngine:  stubOracles{},
		SteadyState:   noSteadyState{},
		LogCollector:  stubLogs{},
		ImageResolver: stubImages{},
		Quiet:         true,
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	pr, err := r.Parallel(opts)
	if err != nil {
		t.Fatalf("Parallel: %v", err)
	}
	return r, pr
}

func requestsFor(n int) []WorldRequest {
	reqs := make([]WorldRequest, 0, n)
	for i := 1; i <= n; i++ {
		reqs = append(reqs, WorldRequest{Ordinal: i, Label: fmt.Sprintf("w%d", i)})
	}
	return reqs
}

// ---------------------------------------------------------------------------
// The network ceiling: fail closed, never truncate
// ---------------------------------------------------------------------------

// The hard constraint of this host. ~24 free bridge networks, two per fixture
// project. Asking for more must be REFUSED with the arithmetic, before anything
// boots: not silently reduced, which would make "8 projects give 3.8x" an
// unreproducible claim and would move the failure to whichever world happened to
// exhaust the pool.
func TestConcurrencyBeyondTheNetworkPoolIsRefusedBeforeAnythingBoots(t *testing.T) {
	backend := &stubBackend{}
	r, err := NewRunner(RunnerOptions{
		Config:        parallelConfig(t),
		ProjectDir:    t.TempDir(),
		Backend:       backend,
		Driver:        finishedDriver(),
		Telemetry:     stubTelemetry{},
		OracleEngine:  stubOracles{},
		SteadyState:   noSteadyState{},
		LogCollector:  stubLogs{},
		ImageResolver: stubImages{},
		Quiet:         true,
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	// 24 free / 2 per world = 12. Thirteen must not be allowed.
	_, err = r.Parallel(ParallelOptions{
		Workers:  13,
		Networks: NetworkBudget{Available: 24, PerWorld: 2},
	})
	if err == nil {
		t.Fatalf("13 concurrent worlds were accepted against a 12-world network budget")
	}
	for _, want := range []string{"13", "26", "24", "12"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the refusal does not show the arithmetic (missing %q): %v", want, err)
		}
	}
	if len(backend.projects()) != 0 {
		t.Fatalf("the refusal booted %d project(s); it must fail before anything exists",
			len(backend.projects()))
	}

	// The boundary is allowed, so the check bounds rather than being timid.
	if _, err := r.Parallel(ParallelOptions{
		Workers:  12,
		Networks: NetworkBudget{Available: 24, PerWorld: 2},
	}); err != nil {
		t.Fatalf("12 concurrent worlds must fit in a 12-world budget: %v", err)
	}
}

func TestANetworkPoolWithNoRoomForOneWorldIsRefused(t *testing.T) {
	b := NetworkBudget{Available: 1, PerWorld: 2}
	if b.MaxWorkers() != 0 {
		t.Fatalf("MaxWorkers = %d, want 0", b.MaxWorkers())
	}
	err := ParallelOptions{Workers: 1, Networks: b}.Validate()
	if err == nil || !strings.Contains(err.Error(), "no room") {
		t.Fatalf("a pool too small for one world was accepted: %v", err)
	}
}

// The defaults must encode the MEASURED host, not a round number.
func TestTheDefaultNetworkBudgetMatchesTheMeasuredHost(t *testing.T) {
	var b NetworkBudget
	if got := b.MaxWorkers(); got != 12 {
		t.Fatalf("the default budget allows %d concurrent worlds; 24 free / 2 per world = 12", got)
	}
	if DefaultNetworksPerWorld != 2 {
		t.Fatalf("DefaultNetworksPerWorld = %d; the KV fixture creates two networks per project "+
			"(client plane and peer plane) and the split is what makes a gray failure possible",
			DefaultNetworksPerWorld)
	}
	// D-022's measured 4-way concurrency must fit with headroom.
	if err := (ParallelOptions{Workers: 4}).Validate(); err != nil {
		t.Fatalf("the measured 4-way concurrency does not fit the default budget: %v", err)
	}
}

// A MEASURED pool that is too small stops the run; a probe that cannot answer
// does not, because a daemon hiccup must not stop a search.
func TestAMeasuredPoolTooSmallIsFatalButAnUnreadableOneIsNot(t *testing.T) {
	t.Run("measured too small is fatal", func(t *testing.T) {
		backend := &stubBackend{}
		_, pr := newParallelRunner(t, backend, finishedDriver(), ParallelOptions{
			Workers: 4,
			Networks: NetworkBudget{
				Probe: func(context.Context) (int, error) { return 3, nil },
			},
		})
		_, err := pr.Run(context.Background(), requestsFor(4))
		if err == nil || !strings.Contains(err.Error(), "room for 3") {
			t.Fatalf("a measured shortfall did not stop the run: %v", err)
		}
		if len(backend.projects()) != 0 {
			t.Fatalf("the run booted %d project(s) despite the shortfall", len(backend.projects()))
		}
	})

	t.Run("unreadable falls back and says so", func(t *testing.T) {
		backend := &stubBackend{}
		var errOut strings.Builder
		r, err := NewRunner(RunnerOptions{
			Config:        parallelConfig(t),
			ProjectDir:    t.TempDir(),
			Backend:       backend,
			Driver:        finishedDriver(),
			Telemetry:     stubTelemetry{},
			OracleEngine:  stubOracles{},
			SteadyState:   noSteadyState{},
			LogCollector:  stubLogs{},
			ImageResolver: stubImages{},
			Stderr:        &errOut,
			Quiet:         true,
		})
		if err != nil {
			t.Fatalf("NewRunner: %v", err)
		}
		// A band found at run time, not DefaultWorkerPortBase: what is under
		// test here is the network-budget probe's fallback, and colliding with
		// a TIME_WAIT socket on the shared default made this fail for a reason
		// that has nothing to do with that. See freePortBand.
		pr, err := r.Parallel(ParallelOptions{
			Workers:  2,
			PortBase: freePortBand(t, 2*DefaultPortsPerWorker),
			Networks: NetworkBudget{
				Probe: func(context.Context) (int, error) { return 0, errors.New("daemon busy") },
			},
		})
		if err != nil {
			t.Fatalf("Parallel: %v", err)
		}
		out, err := pr.Run(context.Background(), requestsFor(2))
		if err != nil {
			t.Fatalf("an unreadable probe stopped the run: %v", err)
		}
		if len(out) != 2 {
			t.Fatalf("got %d outcomes, want 2", len(out))
		}
		if !strings.Contains(errOut.String(), "could not measure") {
			t.Fatalf("the fallback was silent:\n%s", errOut.String())
		}
	})
}

// ---------------------------------------------------------------------------
// Isolation
// ---------------------------------------------------------------------------

// Two concurrent worlds sharing a project name would have the second `up` adopt
// the first's containers and the first `down` destroy the second's. Ports,
// overlays and artifact directories are the same argument.
func TestEveryWorkerGetsItsOwnProjectPortsAndOverlay(t *testing.T) {
	backend := &stubBackend{}
	r, pr := newParallelRunner(t, backend, finishedDriver(), ParallelOptions{
		Workers: 4,
		// The ports are not really bound by the stub backend, so the freeness
		// probe would be checking this machine rather than the code.
		VerifyPortsFree: boolPtr(false),
	})

	slots := pr.Slots()
	projects := map[string]bool{}
	prefixes := map[string]bool{}
	ports := map[int]int{}
	for _, s := range slots {
		if projects[s.Project] {
			t.Fatalf("two slots share the compose project %q", s.Project)
		}
		projects[s.Project] = true
		if prefixes[s.Prefix] {
			t.Fatalf("two slots share the name prefix %q", s.Prefix)
		}
		prefixes[s.Prefix] = true
		if len(s.Ports) != 3 {
			t.Fatalf("slot %d allocated %d port(s) for a 3-node topology", s.Index, len(s.Ports))
		}
		for node, p := range s.Ports {
			if prev, dup := ports[p]; dup {
				t.Fatalf("slot %d node %s and slot %d publish the same host port %d",
					s.Index, node, prev, p)
			}
			ports[p] = s.Index
		}
	}

	out, err := pr.Run(context.Background(), requestsFor(4))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(out) != 4 {
		t.Fatalf("got %d outcomes, want 4", len(out))
	}

	backend.mu.Lock()
	upProjects := append([]string(nil), backend.upProjects...)
	upOverlays := append([]string(nil), backend.upOverlays...)
	backend.mu.Unlock()

	seenProject := map[string]bool{}
	for _, p := range upProjects {
		if seenProject[p] {
			t.Fatalf("compose project %q was brought up twice concurrently", p)
		}
		seenProject[p] = true
	}
	seenOverlay := map[string]bool{}
	for _, o := range upOverlays {
		if o == "" {
			t.Fatalf("a world booted with no explicit overlay path; concurrent worlds writing " +
				"one overlay file cross-wire each other's teardown")
		}
		if seenOverlay[o] {
			t.Fatalf("two worlds wrote the same overlay file %q", o)
		}
		seenOverlay[o] = true
	}

	// Artifact directories are per-ordinal and must not collide either.
	for _, o := range out {
		want := joinRunWorld(r, o.Ordinal)
		if o.Paths.Dir != want {
			t.Fatalf("world %d wrote to %s, want %s", o.Ordinal, o.Paths.Dir, want)
		}
	}
}

// The compose file's own variables are how a world gets its own published
// ports. This pins the KV fixture's mapping so it cannot rot into a comment.
func TestComposeVarEnvMapsTheFixtureVariables(t *testing.T) {
	slot := WorkerSlot{
		Index:    2,
		Project:  "thesis-run-w02",
		Prefix:   "run-w02",
		PortBase: 19032,
		Ports:    map[string]int{"kv-n1": 19032, "kv-n2": 19033, "kv-n3": 19034},
	}
	envFn := ComposeVarEnv("KV_PREFIX", map[string]string{
		"kv-n1": "KV_PORT_N1",
		"kv-n2": "KV_PORT_N2",
		"kv-n3": "KV_PORT_N3",
	})
	env := envFn(slot)
	want := map[string]string{
		"KV_PREFIX":  "run-w02",
		"KV_PORT_N1": "19032",
		"KV_PORT_N2": "19033",
		"KV_PORT_N3": "19034",
		// The generic names ride along, so a compose file that spells them needs
		// no mapping at all.
		"PROTHESIS_PORT_KV_N1": "19032",
		"PROTHESIS_PREFIX":     "run-w02",
		"PROTHESIS_WORKER":     "2",
	}
	got := envMap(env)
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("%s = %q, want %q (full env: %v)", k, got[k], v, env)
		}
	}

	// A node with no allocated port must be OMITTED, not exported empty:
	// `${KV_PORT_N9:-18089}` with KV_PORT_N9="" publishes port "" and compose
	// fails to parse, whereas an unset variable falls back correctly.
	envFn2 := ComposeVarEnv("", map[string]string{"kv-n9": "KV_PORT_N9"})
	for _, kv := range envFn2(slot) {
		if strings.HasPrefix(kv, "KV_PORT_N9=") {
			t.Fatalf("an unallocated node exported %q; compose would fail to parse the port", kv)
		}
	}
}

func envMap(env []string) map[string]string {
	out := map[string]string{}
	for _, kv := range env {
		if i := strings.IndexByte(kv, '='); i > 0 {
			out[kv[:i]] = kv[i+1:]
		}
	}
	return out
}

// The environment must actually reach compose AND the driver, or the driver
// connects to another world's cluster.
func TestTheWorkerEnvironmentReachesBothComposeAndTheDriver(t *testing.T) {
	backend := &stubBackend{}
	drv := finishedDriver()
	_, pr := newParallelRunner(t, backend, drv, ParallelOptions{
		Workers:         2,
		VerifyPortsFree: boolPtr(false),
		WorkerEnv: ComposeVarEnv("KV_PREFIX", map[string]string{
			"kv-n1": "KV_PORT_N1", "kv-n2": "KV_PORT_N2", "kv-n3": "KV_PORT_N3",
		}),
	})
	if _, err := pr.Run(context.Background(), requestsFor(2)); err != nil {
		t.Fatalf("Run: %v", err)
	}

	backend.mu.Lock()
	envs := append([][]string(nil), backend.upEnvs...)
	backend.mu.Unlock()
	if len(envs) != 2 {
		t.Fatalf("got %d compose environments, want 2", len(envs))
	}
	seen := map[string]bool{}
	for _, e := range envs {
		m := envMap(e)
		if m["KV_PORT_N1"] == "" {
			t.Fatalf("compose was invoked without KV_PORT_N1; both worlds would publish 18081")
		}
		if seen[m["KV_PORT_N1"]] {
			t.Fatalf("two worlds asked compose for the same KV_PORT_N1 %q", m["KV_PORT_N1"])
		}
		seen[m["KV_PORT_N1"]] = true
	}

	for _, req := range drv.requests() {
		if envMap(req.Env)["KV_PORT_N1"] == "" {
			t.Fatalf("the driver was launched without the world's port environment; it would " +
				"connect to whichever cluster owns the default port")
		}
	}
}

// ---------------------------------------------------------------------------
// Determinism
// ---------------------------------------------------------------------------

// Worlds complete out of order (a probe world is much cheaper than a compound
// one) so a search that ranked in completion order would produce a different
// answer on a differently loaded machine, from the same seed.
func TestOutcomesComeBackInOrdinalOrderWhateverTheCompletionOrder(t *testing.T) {
	backend := &stubBackend{}
	// Reverse the ordinals AND give the workers real concurrency, so completion
	// order cannot accidentally match request order.
	reqs := []WorldRequest{{Ordinal: 5}, {Ordinal: 2}, {Ordinal: 4}, {Ordinal: 1}, {Ordinal: 3}}
	_, pr := newParallelRunner(t, backend, finishedDriver(), ParallelOptions{
		Workers: 4, VerifyPortsFree: boolPtr(false),
	})
	out, err := pr.Run(context.Background(), reqs)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := make([]int, 0, len(out))
	for _, o := range out {
		got = append(got, o.Ordinal)
	}
	if !sort.IntsAreSorted(got) {
		t.Fatalf("outcomes came back as %v; a search ranking in this order would not be "+
			"reproducible from a seed", got)
	}
	if len(got) != 5 {
		t.Fatalf("got %d outcomes for 5 requests", len(got))
	}
}

// A world's seed must be a function of its ordinal alone. If it depended on
// scheduling, the same seed would explore a different tree on a busier machine,
// which A.8 forbids outright.
func TestAWorldSeedDependsOnItsOrdinalAndNothingElse(t *testing.T) {
	const runSeed = 424242
	first := WorldSeedFor(runSeed, 7)
	for i := 0; i < 50; i++ {
		if got := WorldSeedFor(runSeed, 7); got != first {
			t.Fatalf("WorldSeedFor is not a function: %d then %d", first, got)
		}
	}
	seen := map[uint64]int{}
	for ord := 1; ord <= 64; ord++ {
		s := WorldSeedFor(runSeed, ord)
		if prev, dup := seen[s]; dup {
			t.Fatalf("worlds %d and %d derive the same seed %d", prev, ord, s)
		}
		seen[s] = ord
	}
	if WorldSeedFor(runSeed, 3) == WorldSeedFor(runSeed+1, 3) {
		t.Fatalf("the run seed does not reach the world seed")
	}
}

// Two runs of the same request set, at different concurrency, must produce the
// same worlds. Concurrency is a scheduling decision, not a semantic one.
func TestTheSameRequestsGiveTheSameWorldsAtAnyConcurrency(t *testing.T) {
	collect := func(workers int) []uint64 {
		backend := &stubBackend{}
		_, pr := newParallelRunner(t, backend, finishedDriver(), ParallelOptions{
			Workers: workers, VerifyPortsFree: boolPtr(false),
		})
		out, err := pr.Run(context.Background(), requestsFor(6))
		if err != nil {
			t.Fatalf("Run(workers=%d): %v", workers, err)
		}
		seeds := make([]uint64, 0, len(out))
		for _, o := range out {
			seeds = append(seeds, o.Seed)
		}
		return seeds
	}
	serial, wide := collect(1), collect(4)
	if len(serial) != len(wide) {
		t.Fatalf("different world counts: %d vs %d", len(serial), len(wide))
	}
	for i := range serial {
		if serial[i] != wide[i] {
			t.Fatalf("world %d got seed %d serially and %d at 4-way concurrency",
				i+1, serial[i], wide[i])
		}
	}
}

// Two worlds sharing an ordinal would share a seed and overwrite each other's
// artifacts. Refuse rather than let the second silently win.
func TestDuplicateOrdinalsAreRefused(t *testing.T) {
	backend := &stubBackend{}
	_, pr := newParallelRunner(t, backend, finishedDriver(), ParallelOptions{
		Workers: 2, VerifyPortsFree: boolPtr(false),
	})
	_, err := pr.Run(context.Background(), []WorldRequest{{Ordinal: 1}, {Ordinal: 1}})
	if err == nil || !strings.Contains(err.Error(), "twice") {
		t.Fatalf("duplicate ordinals were accepted: %v", err)
	}
	if len(backend.projects()) != 0 {
		t.Fatalf("the refusal booted a project")
	}
}

// ---------------------------------------------------------------------------
// Leaks
// ---------------------------------------------------------------------------

// A leaked compose project holds two bridge networks for the rest of this
// engine's life. Its slot must never be handed out again, and when every slot
// has leaked the executor must fail closed naming them, not boot a project on
// top of a live one and report a Docker error nobody can attribute.
func TestALeakedProjectRetiresItsSlotAndEventuallyFailsClosed(t *testing.T) {
	backend := &stubBackend{
		downErr: errors.New("harness: project thesis-x still has 2 network(s) after down"),
	}
	var errOut strings.Builder
	r, err := NewRunner(RunnerOptions{
		Config:        parallelConfig(t),
		ProjectDir:    t.TempDir(),
		RunID:         "r_leak",
		Backend:       backend,
		Driver:        finishedDriver(),
		Telemetry:     stubTelemetry{},
		OracleEngine:  stubOracles{},
		SteadyState:   noSteadyState{},
		LogCollector:  stubLogs{},
		ImageResolver: stubImages{},
		Stderr:        &errOut,
		Quiet:         true,
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	pr, err := r.Parallel(ParallelOptions{Workers: 2, VerifyPortsFree: boolPtr(false)})
	if err != nil {
		t.Fatalf("Parallel: %v", err)
	}

	out, runErr := pr.Run(context.Background(), requestsFor(6))
	if runErr == nil {
		t.Fatalf("six worlds all leaking their project ran to completion; the pool must fail closed")
	}
	if !errors.Is(runErr, ErrWorkerPoolExhausted) {
		t.Fatalf("the failure was not ErrWorkerPoolExhausted: %v", runErr)
	}
	for _, want := range []string{"docker network", "slot 0", "slot 1"} {
		if !strings.Contains(runErr.Error(), want) {
			t.Fatalf("the exhaustion message is not actionable (missing %q): %v", want, runErr)
		}
	}

	// Exactly the two slots ran; the rest were refused rather than attempted.
	booted := len(backend.projects())
	if booted != 2 {
		t.Fatalf("%d project(s) were booted; only the two slots may run before the pool is "+
			"exhausted, or every later world burns a boot to fail the same way", booted)
	}

	// The leak must be visible per world, not only in the batch error.
	leaks := 0
	for _, o := range out {
		if o.TeardownErr != nil {
			leaks++
			if o.Outcome.Rank() < OutcomeHarnessError.Rank() {
				t.Fatalf("world %d leaked its project but reported %s", o.Ordinal, o.Outcome)
			}
		}
	}
	if leaks != 2 {
		t.Fatalf("%d world(s) recorded a teardown failure, want 2", leaks)
	}
}

// A verified teardown must return its slot, or a long search would starve on a
// pool that is not actually exhausted.
func TestAVerifiedTeardownReturnsItsSlot(t *testing.T) {
	backend := &stubBackend{}
	_, pr := newParallelRunner(t, backend, finishedDriver(), ParallelOptions{
		Workers: 2, VerifyPortsFree: boolPtr(false),
	})
	out, err := pr.Run(context.Background(), requestsFor(9))
	if err != nil {
		t.Fatalf("nine worlds through two clean slots failed: %v", err)
	}
	if len(out) != 9 {
		t.Fatalf("got %d outcomes, want 9", len(out))
	}
	for _, o := range out {
		if o.TeardownErr != nil {
			t.Fatalf("world %d reported a teardown failure it did not have: %v", o.Ordinal, o.TeardownErr)
		}
	}
	if got := len(backend.projects()); got != 9 {
		t.Fatalf("%d project(s) booted for 9 worlds", got)
	}
}

// TEARDOWN runs on every path, including the one where BOOT itself failed.
func TestEveryWorldIsTornDownEvenWhenBootFails(t *testing.T) {
	backend := &stubBackend{}
	_, pr := newParallelRunner(t, backend, finishedDriver(), ParallelOptions{
		Workers: 3, VerifyPortsFree: boolPtr(false),
	})
	out, err := pr.Run(context.Background(), requestsFor(3))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	backend.mu.Lock()
	downs := len(backend.downProject)
	backend.mu.Unlock()
	if downs != len(out) {
		t.Fatalf("%d world(s) ran but Down was called %d time(s); a world that skips teardown "+
			"holds two bridge networks permanently", len(out), downs)
	}
}

// A world whose BOOT failed still gets a VERIFICATION sweep, addressed at the
// project it would have created.
//
// Backend.Up rolls itself back when it fails partway, but its rollback is a Down
// whose error it discards, so a project that survived a failed boot would be
// invisible, and on this host an invisible project is two bridge networks gone.
func TestAWorldThatNeverBootedIsStillSweptByProjectName(t *testing.T) {
	backend := &stubBackend{upErr: errors.New("compose up: simulated boot failure")}
	_, pr := newParallelRunner(t, backend, finishedDriver(), ParallelOptions{
		Workers: 2, VerifyPortsFree: boolPtr(false),
	})
	out, err := pr.Run(context.Background(), requestsFor(2))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	backend.mu.Lock()
	downs := append([]string(nil), backend.downProject...)
	backend.mu.Unlock()

	if len(downs) != 2 {
		t.Fatalf("Down was called %d time(s) for two failed boots; a world that created "+
			"nothing is only KNOWN to have created nothing once somebody looks", len(downs))
	}
	slots := pr.Slots()
	want := map[string]bool{slots[0].Project: true, slots[1].Project: true}
	for _, p := range downs {
		if !want[p] {
			t.Fatalf("the sweep addressed project %q, which is not either slot's (%v); it "+
				"would verify the wrong thing", p, want)
		}
	}
	for _, o := range out {
		if o.Outcome != OutcomeHarnessError {
			t.Fatalf("world %d reported %s for a failed boot", o.Ordinal, o.Outcome)
		}
		if o.TeardownErr != nil {
			t.Fatalf("world %d reported a teardown failure for a clean sweep: %v",
				o.Ordinal, o.TeardownErr)
		}
	}
}

// ---------------------------------------------------------------------------
// Ports
// ---------------------------------------------------------------------------

// An occupied port must be reported as an occupied port, before a world burns a
// boot to discover it as "Bind for 127.0.0.1:19001 failed" from inside a
// concurrent batch where it is not obvious which world said it.
func TestAnOccupiedHostPortIsRefusedWithItsOwnMessage(t *testing.T) {
	ln, err := listenOnAny(t)
	if err != nil {
		t.Skipf("could not bind a loopback port to occupy: %v", err)
	}
	defer func() { _ = ln.Close() }()
	port := ln.Addr().(*netTCPAddr).Port

	backend := &stubBackend{}
	_, pr := newParallelRunner(t, backend, finishedDriver(), ParallelOptions{
		Workers:  1,
		PortBase: port,
	})
	_, err = pr.Run(context.Background(), requestsFor(1))
	if err == nil {
		t.Fatalf("a slot whose first port was already bound was accepted")
	}
	if !strings.Contains(err.Error(), "already in use") {
		t.Fatalf("the refusal does not name the cause: %v", err)
	}
	if !strings.Contains(err.Error(), "thesis down") {
		t.Fatalf("the refusal does not say what to do about it: %v", err)
	}
	if len(backend.projects()) != 0 {
		t.Fatalf("the refusal booted a project")
	}
}

func TestPortBandsDoNotOverlap(t *testing.T) {
	backend := &stubBackend{}
	_, pr := newParallelRunner(t, backend, finishedDriver(), ParallelOptions{
		Workers: 8, PortsPerWorker: 4, PortBase: 20000, VerifyPortsFree: boolPtr(false),
	})
	prev := -1
	for _, s := range pr.Slots() {
		if s.PortBase <= prev {
			t.Fatalf("slot %d starts at %d, inside the previous band ending at %d",
				s.Index, s.PortBase, prev)
		}
		prev = s.PortBase + s.PortSpan - 1
	}
}

func boolPtr(b bool) *bool { return &b }
