package schema

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"sort"
	"time"

	"gopkg.in/yaml.v3"
)

// Compiled-in configuration defaults. Every value here is either the
// directive's own sample value (4.2) or Addendum A.10's stated default.
const (
	DefaultHarnessFile   = "docker-compose.yaml"
	DefaultHealthTimeout = 30 * time.Second
	DefaultSteadyTimeout = 60 * time.Second
	DefaultOraclesDir    = ".prothesis/oracles"
	DefaultArtifactsDir  = ".prothesis/runs"

	DefaultMaxConcurrentFaults = 3
	DefaultMaxFaultsPerWorld   = 24

	// DefaultExplorationConstant is A.10's LITERAL 1.41, not math.Sqrt2.
	DefaultExplorationConstant = 1.41
	DefaultProbeBudgetPct      = 20
	DefaultMaxMCTSDepth        = 4

	DefaultViolationWeight = 100
	DefaultNoveltyWeight   = 10
	DefaultFaultPenalty    = 3
	DefaultDurationPenalty = 0.1

	DefaultSampleIntervalMS   = 500
	DefaultSpiralWindow       = 5
	DefaultReinforceThreshold = 0.0

	// DefaultLLMEndpoint is a local llama.cpp-style server: no auth, no
	// network egress. DefaultLLMTimeoutMS is generous because a thinking
	// model's reasoning precedes its answer inside the same response.
	DefaultLLMEndpoint   = "http://localhost:8080/v1/chat/completions"
	DefaultLLMModel      = "qwen3.8"
	DefaultLLMMaxRetries = 2
	DefaultLLMTimeoutMS  = 120000
)

// DefaultConfig returns the compiled-in defaults.
//
// # Why no map-valued section is pre-seeded
//
// yaml.v3 decodes in place, so a scalar or struct default survives a document
// that does not mention it. Maps behave differently in two ways, and both are
// load-bearing:
//
//   - map ENTRIES do not merge. `profiles: {soak: {worlds: 5}}` decoded over a
//     pre-seeded soak yields a soak profile with an empty budget and an empty
//     driver_profile, silently destroying the rest of the pre-seeded entry.
//   - map KEYS do merge. A user who DELETES `soak` from prothesis.yaml would
//     get a fully populated soak back from the compiled-in defaults, which
//     would make profile deletion invisible, and profile budgets are covered
//     by the Phase 3 oracle lock, so an invisible deletion is a gate-weakening
//     hole rather than a cosmetic surprise.
//
// Decoder.KnownFields(true) does not help: it rejects unknown FIELDS inside a
// struct, not unknown KEYS in a map, so a typo'd profile name decodes with no
// error and silently creates a phantom profile.
//
// So Profiles, Driver.Profiles and every Mix stay nil here, and per-entry
// defaults are applied explicitly after decoding by applyEntryDefaults.
//
// Sequences are left nil for a related reason: `oracles.builtin` and
// `perturber.allow` must hash differently when absent than when fully
// populated, or deleting an oracle from the list would not move the lock
// digest.
func DefaultConfig() Config {
	return Config{
		// Version is deliberately NOT defaulted. It is the schema
		// discriminator; filling it in would make a document that never
		// declared one validate successfully.
		Harness: HarnessConfig{
			Backend:     BackendCompose,
			File:        DefaultHarnessFile,
			SteadyState: SteadyStateProbe{Timeout: Duration(DefaultSteadyTimeout)},
		},
		Perturber: PerturberConfig{
			Budget: PerturberBudget{
				MaxConcurrentFaults: DefaultMaxConcurrentFaults,
				MaxFaultsPerWorld:   DefaultMaxFaultsPerWorld,
			},
		},
		Oracles: OraclesConfig{Dir: DefaultOraclesDir},
		Artifacts: ArtifactsConfig{
			Dir:           DefaultArtifactsDir,
			RetainPassing: RetainN(3),
			RetainFailing: RetainAll(),
		},
		Search: DefaultSearchConfig(),
	}
}

// DefaultSearchConfig returns Addendum A.10's stated defaults.
func DefaultSearchConfig() SearchConfig {
	return SearchConfig{
		Strategy:            StrategySaboteur,
		ExplorationConstant: DefaultExplorationConstant,
		ProbeBudgetPct:      DefaultProbeBudgetPct,
		MaxMCTSDepth:        DefaultMaxMCTSDepth,
		EscalationLadder:    LadderDefault,
		Utility: UtilityConfig{
			ViolationWeight: DefaultViolationWeight,
			NoveltyWeight:   DefaultNoveltyWeight,
			FaultPenalty:    DefaultFaultPenalty,
			DurationPenalty: DefaultDurationPenalty,
		},
		Observe: ObserveConfig{
			SampleIntervalMS:   DefaultSampleIntervalMS,
			SpiralWindow:       DefaultSpiralWindow,
			ReinforceThreshold: DefaultReinforceThreshold,
		},
		LLM: DefaultLLMConfig(),
	}
}

// DefaultLLMConfig returns the llm strategy's endpoint defaults.
func DefaultLLMConfig() LLMConfig {
	return LLMConfig{
		Endpoint:   DefaultLLMEndpoint,
		Model:      DefaultLLMModel,
		MaxRetries: DefaultLLMMaxRetries,
		TimeoutMS:  DefaultLLMTimeoutMS,
	}
}

// DecodeConfig decodes prothesis.yaml bytes over DefaultConfig.
//
// Unknown fields are rejected (Decoder.KnownFields): a misspelled key that
// silently does nothing is a way to appear to have tightened a budget while
// changing nothing. It does NOT validate; call Validate, or one of the
// section validators, afterwards.
func DecodeConfig(data []byte) (*Config, error) {
	cfg := DefaultConfig()
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		if errors.Is(err, io.EOF) {
			// An empty document leaves the defaults in place; Validate will
			// report the missing version rather than this being a parse error.
			cfg.applyEntryDefaults()
			return &cfg, nil
		}
		return nil, ValidationErrors{{Path: "prothesis.yaml", Msg: err.Error()}}
	}
	// A second YAML document in the same file would be silently ignored.
	var extra yaml.Node
	if err := dec.Decode(&extra); err == nil {
		return nil, ValidationErrors{{
			Path: "prothesis.yaml",
			Msg:  "more than one YAML document; prothesis.yaml holds exactly one",
		}}
	}
	cfg.applyEntryDefaults()
	return &cfg, nil
}

// applyEntryDefaults fills in defaults that cannot live in DefaultConfig
// because they belong to elements of a sequence or a map.
//
// Only sequence elements get defaults. Map entries (driver profiles, run
// profiles) deliberately get none: a compiled-in default there would be
// indistinguishable from a value the user wrote, and both maps are
// lock-covered.
func (c *Config) applyEntryDefaults() {
	for i := range c.Harness.Health {
		if c.Harness.Health[i].Timeout == 0 {
			c.Harness.Health[i].Timeout = Duration(DefaultHealthTimeout)
		}
	}
}

// ---------------------------------------------------------------------------
// validation
//
// Validation is split by section so a command validates only what it uses.
// `thesis up` and `thesis down` need version, name and harness; requiring a
// populated driver, perturber and oracle section for them would make Phase 0's
// definition of done depend on three subsystems that do not exist yet.
// ---------------------------------------------------------------------------

// Validate runs every section validator plus the cross-section checks. Use it
// for commands that execute a whole run.
func (c *Config) Validate() error {
	var errs ValidationErrors
	errs.Merge("", c.ValidateMeta())
	errs.Merge("", c.ValidateHarness())
	errs.Merge("", c.ValidateDriver())
	errs.Merge("", c.ValidatePerturber())
	errs.Merge("", c.ValidateOracles())
	errs.Merge("", c.ValidateProfiles())
	errs.Merge("", c.ValidateArtifacts())
	errs.Merge("", c.ValidateSearch())
	errs.Merge("", c.validateCrossReferences())
	return errs.OrNil()
}

// ValidateForTopology is the subset `thesis init`, `up` and `down` require.
func (c *Config) ValidateForTopology() error {
	var errs ValidationErrors
	errs.Merge("", c.ValidateMeta())
	errs.Merge("", c.ValidateHarness())
	return errs.OrNil()
}

// ValidateMeta checks the schema discriminator and the project name.
func (c *Config) ValidateMeta() error {
	var errs ValidationErrors
	switch c.Version {
	case "":
		errs.Add("version", "missing (write %q)", ConfigVersion)
	case ConfigVersion:
	default:
		errs.Add("version", "is %q, want %q", c.Version, ConfigVersion)
	}
	if c.Name == "" {
		errs.Add("name", "missing (the project name, used in run ids and container labels)")
	}
	return errs.OrNil()
}

// ValidateHarness checks the topology section.
func (c *Config) ValidateHarness() error {
	var errs ValidationErrors
	h := &c.Harness

	if h.Backend == "" {
		errs.Add("harness.backend", "missing (want one of %v)", AllBackends)
	} else if !h.Backend.Valid() {
		errs.Add("harness.backend", "unknown backend %q (want one of %v)", h.Backend, AllBackends)
	}
	if h.File == "" {
		errs.Add("harness.file", "missing (the topology file the %s backend reads)", h.Backend)
	}

	if len(h.Nodes) == 0 {
		errs.Add("harness.nodes", "at least one node is required")
	}
	seen := map[string]int{}
	for i, n := range h.Nodes {
		p := indexPath("harness.nodes", i)
		switch {
		case n.ID == "":
			errs.Add(p+".id", "missing (the logical node id used by fault targets)")
		case !validIdent(n.ID):
			errs.Add(p+".id", "%q is not a node id (letters, digits, _ - . starting with a letter or _)", n.ID)
		default:
			if j, dup := seen[n.ID]; dup {
				errs.Add(p+".id", "duplicate node id %q (already used at harness.nodes[%d])", n.ID, j)
			}
			seen[n.ID] = i
		}
		if n.Service == "" {
			errs.Add(p+".service", "missing (the logical group the fault grammar targets, e.g. \"kv\")")
		} else if !validIdent(n.Service) {
			errs.Add(p+".service", "%q is not a service name", n.Service)
		}
		if n.ComposeService != "" && !validIdent(n.ComposeService) {
			errs.Add(p+".compose_service", "%q is not a service name", n.ComposeService)
		}
	}

	// Two nodes may share a logical Service (that is the whole point of
	// `minority(kv)`) and two nodes may also share a compose service by
	// DEFAULT, which is exactly the directive's own section 4.2 sample: three
	// nodes with `service: kv` and no compose_service, describing one scaled
	// service. That sample must stay valid, so the check below applies only to
	// EXPLICITLY set compose_service values.
	//
	// An explicit collision is different: the author named a physical service
	// twice, which cannot be what they meant, because the backend would bind
	// two logical nodes to one container and a partition between them would be
	// a silent no-op. Whether an implicit collision is resolvable depends on the
	// backend (compose can scale a service, and a future backend may address
	// replicas by ordinal), so it is diagnosed at bind time by the backend that
	// knows, not rejected here by the schema that does not.
	byCompose := map[string]int{}
	for i, n := range h.Nodes {
		if n.ComposeService == "" {
			continue
		}
		if j, dup := byCompose[n.ComposeService]; dup {
			errs.Add(indexPath("harness.nodes", i)+".compose_service",
				"%q is already used by harness.nodes[%d] (%q); two logical nodes cannot share one "+
					"compose service, or a fault between them would be a silent no-op",
				n.ComposeService, j, h.Nodes[j].ID)
		}
		byCompose[n.ComposeService] = i
	}

	for i, hp := range h.Health {
		p := indexPath("harness.health", i)
		if hp.Node == "" {
			errs.Add(p+".node", "missing (a node id or a target expression such as \"kv:*\")")
		} else if _, err := ParseTarget(hp.Node); err != nil {
			errs.Add(p+".node", "%s", err.Error())
		}
		if hp.Probe == "" {
			errs.Add(p+".probe", "missing (a probe URL, e.g. \"http://{host}:{port}/healthz\")")
		}
		if hp.Timeout <= 0 {
			errs.Add(p+".timeout", "must be positive, got %s", hp.Timeout)
		}
	}

	if h.SteadyState.Probe != "" && h.SteadyState.Timeout <= 0 {
		errs.Add("harness.steady_state.timeout", "must be positive, got %s", h.SteadyState.Timeout)
	}
	if h.RoleProbe.Probe != "" && h.RoleProbe.Timeout <= 0 {
		errs.Add("harness.role_probe.timeout", "must be positive, got %s (role resolution runs inside a fault window)", h.RoleProbe.Timeout)
	}
	return errs.OrNil()
}

// ValidateDriver checks the workload section.
//
// It deliberately does NOT require driver.cmd to contain any particular
// placeholder. Nothing in the directive says it must, and a driver that reads
// its history path from the environment is legitimate.
func (c *Config) ValidateDriver() error {
	var errs ValidationErrors
	if c.Driver.Cmd == "" {
		errs.Add("driver.cmd", "missing (the workload generator command template)")
	}
	// A typo'd placeholder is a CONFIG_ERROR, caught here rather than at run
	// time, because exit 5 is exactly "a human fixes this by editing a file" and
	// because failing now costs nothing while failing later costs a full cluster
	// boot and produces a verdict that points at the wrong thing entirely.
	//
	// Note this validates the VOCABULARY, not the presence of any particular
	// placeholder: a driver that takes none of them is still legal, which is why
	// this is not a required-substring check.
	for _, unknown := range UnknownPlaceholders(c.Driver.Cmd) {
		errs.Add("driver.cmd", "unknown placeholder %s (known: %s, %s, %s, %s); "+
			"an unrecognised placeholder is substituted by nothing and reaches the driver as a "+
			"literal string, so the history would be written to a file of that name and the "+
			"canonical history would be empty",
			unknown, PlaceholderHistoryPath, PlaceholderPlanPath, PlaceholderProfile, PlaceholderSeed)
	}
	for _, name := range sortedKeys(c.Driver.Profiles) {
		p := keyPath("driver.profiles", name)
		dp := c.Driver.Profiles[name]
		if dp.Clients < 1 {
			errs.Add(p+".clients", "must be at least 1, got %d", dp.Clients)
		}
		if dp.Ops < 1 {
			errs.Add(p+".ops", "must be at least 1, got %d", dp.Ops)
		}
		// The mix is NOT required to sum to 1.0. Relative weights are a common
		// load-generator convention and nothing in the directive declares the
		// mix a probability distribution.
		positive := false
		for _, k := range sortedFloatKeys(dp.Mix) {
			if dp.Mix[k] < 0 {
				errs.Add(keyPath(p+".mix", k), "negative weight %v", dp.Mix[k])
			}
			if dp.Mix[k] > 0 {
				positive = true
			}
		}
		if len(dp.Mix) > 0 && !positive {
			errs.Add(p+".mix", "every weight is zero, so the profile would issue no operations")
		}
	}
	return errs.OrNil()
}

// ValidatePerturber checks the fault-space section.
func (c *Config) ValidatePerturber() error {
	var errs ValidationErrors
	b := c.Perturber.Budget
	if b.MaxConcurrentFaults < 1 {
		errs.Add("perturber.budget.max_concurrent_faults", "must be at least 1, got %d", b.MaxConcurrentFaults)
	}
	if b.MaxFaultsPerWorld < 1 {
		errs.Add("perturber.budget.max_faults_per_world", "must be at least 1, got %d", b.MaxFaultsPerWorld)
	}
	if b.MaxConcurrentFaults > b.MaxFaultsPerWorld && b.MaxFaultsPerWorld >= 1 {
		errs.Add("perturber.budget.max_concurrent_faults",
			"%d exceeds max_faults_per_world (%d), so the concurrency cap can never be reached",
			b.MaxConcurrentFaults, b.MaxFaultsPerWorld)
	}
	if len(c.Perturber.EffectiveFaultKinds()) == 0 {
		errs.Add("perturber.deny", "denies every allowed fault kind, so no fault can ever be injected")
	}
	return errs.OrNil()
}

// ValidateOracles checks the oracle section.
func (c *Config) ValidateOracles() error {
	var errs ValidationErrors
	if c.Oracles.Dir == "" {
		errs.Add("oracles.dir", "missing (where external oracle executables live)")
	}
	seen := map[BuiltinOracle]int{}
	for i, o := range c.Oracles.Builtin {
		if j, dup := seen[o]; dup {
			errs.Add(indexPath("oracles.builtin", i), "duplicate entry %q (already listed at index %d)", o, j)
		}
		seen[o] = i
	}
	return errs.OrNil()
}

// ValidateProfiles checks the top-level run profiles.
func (c *Config) ValidateProfiles() error {
	var errs ValidationErrors
	if len(c.Profiles) == 0 {
		errs.Add("profiles", "at least one run profile is required")
	}
	for _, name := range sortedKeys(c.Profiles) {
		p := keyPath("profiles", name)
		pr := c.Profiles[name]
		if pr.Budget <= 0 {
			errs.Add(p+".budget", "must be positive, got %s", pr.Budget)
		}
		if pr.DriverProfile == "" {
			errs.Add(p+".driver_profile", "missing (names an entry of driver.profiles)")
		}
		if pr.Worlds != nil {
			switch w := *pr.Worlds; {
			case w == 0:
				errs.Add(p+".worlds", "0 runs no worlds; write %d for unbounded, or omit the key to inherit",
					WorldsUnbounded)
			case w < WorldsUnbounded:
				errs.Add(p+".worlds", "%d is not a world count (write a positive number or %d for unbounded)",
					w, WorldsUnbounded)
			}
		}
	}
	return errs.OrNil()
}

// ValidateArtifacts checks the artifact retention section.
func (c *Config) ValidateArtifacts() error {
	var errs ValidationErrors
	if c.Artifacts.Dir == "" {
		errs.Add("artifacts.dir", "missing (where run bundles are written)")
	}
	errs.Merge("artifacts.retain_passing", c.Artifacts.RetainPassing.Validate())
	errs.Merge("artifacts.retain_failing", c.Artifacts.RetainFailing.Validate())
	return errs.OrNil()
}

// ValidateSearch checks Addendum A.10's block.
func (c *Config) ValidateSearch() error {
	var errs ValidationErrors
	s := &c.Search
	if !s.Strategy.Valid() {
		errs.Add("search.strategy", "unknown strategy %q (want one of %v)", s.Strategy, AllSearchStrategies)
	}
	if !s.EscalationLadder.Valid() {
		errs.Add("search.escalation_ladder", "unknown ladder %q (want one of %v)",
			s.EscalationLadder, AllEscalationLadders)
	}
	if s.ExplorationConstant < 0 {
		errs.Add("search.exploration_constant", "must not be negative, got %v", s.ExplorationConstant)
	}
	if s.ProbeBudgetPct < 0 || s.ProbeBudgetPct > 100 {
		errs.Add("search.probe_budget_pct", "must be a percentage in [0, 100], got %d", s.ProbeBudgetPct)
	}
	if s.MaxMCTSDepth < 1 {
		errs.Add("search.max_mcts_depth", "must be at least 1, got %d", s.MaxMCTSDepth)
	}
	if s.Observe.SampleIntervalMS < 1 {
		errs.Add("search.observe.sample_interval_ms", "must be at least 1, got %d", s.Observe.SampleIntervalMS)
	}
	if s.Observe.SpiralWindow < 2 {
		errs.Add("search.observe.spiral_window",
			"must be at least 2; a trend regression needs two samples, got %d", s.Observe.SpiralWindow)
	}
	if s.Utility.ViolationWeight < 0 {
		errs.Add("search.utility.violation_weight", "must not be negative, got %d", s.Utility.ViolationWeight)
	}
	if s.Utility.NoveltyWeight < 0 {
		errs.Add("search.utility.novelty_weight", "must not be negative, got %d", s.Utility.NoveltyWeight)
	}
	if s.Utility.FaultPenalty < 0 {
		errs.Add("search.utility.fault_penalty", "must not be negative, got %d", s.Utility.FaultPenalty)
	}
	if s.Utility.DurationPenalty < 0 {
		errs.Add("search.utility.duration_penalty", "must not be negative, got %v", s.Utility.DurationPenalty)
	}
	// The llm block is validated only when llm is the selected strategy: under
	// any other strategy its defaults are inert, and refusing them would gate
	// a saboteur run on a service it never calls.
	if s.Strategy == StrategyLLM {
		if s.LLM.Endpoint == "" {
			errs.Add("search.llm.endpoint", "is required when search.strategy is %q", StrategyLLM)
		}
		if s.LLM.MaxRetries < 0 {
			errs.Add("search.llm.max_retries", "must not be negative, got %d", s.LLM.MaxRetries)
		}
		if s.LLM.TimeoutMS < 1000 {
			errs.Add("search.llm.timeout_ms", "must be at least 1000, got %d; a model call that "+
				"cannot wait one second will never return a proposal", s.LLM.TimeoutMS)
		}
	}
	return errs.OrNil()
}

// validateCrossReferences checks names that resolve into another section.
func (c *Config) validateCrossReferences() error {
	var errs ValidationErrors
	for _, name := range sortedKeys(c.Profiles) {
		dp := c.Profiles[name].DriverProfile
		if dp == "" {
			continue // already reported by ValidateProfiles
		}
		if _, ok := c.Driver.Profiles[dp]; !ok {
			errs.Add(keyPath("profiles", name)+".driver_profile",
				"%q does not name an entry of driver.profiles (have: %v)", dp, sortedKeys(c.Driver.Profiles))
		}
	}
	for i, hp := range c.Harness.Health {
		t, err := ParseTarget(hp.Node)
		if err != nil {
			continue // already reported by ValidateHarness
		}
		if !c.targetResolves(t) {
			errs.Add(indexPath("harness.health", i)+".node",
				"%q matches no node in harness.nodes", hp.Node)
		}
	}
	return errs.OrNil()
}

// targetResolves reports whether a static target names something in
// harness.nodes. Dynamic targets (role, quorum) are resolved at injection time
// against live cluster state and cannot be checked here.
func (c *Config) targetResolves(t Target) bool {
	switch t.Kind {
	case TargetNode:
		_, ok := c.Node(t.Node)
		return ok
	case TargetWildcard:
		for _, n := range c.Harness.Nodes {
			if n.Service == t.Service {
				return true
			}
		}
		return false
	case TargetEdge:
		_, a := c.Node(t.A)
		_, b := c.Node(t.B)
		return a && b
	case TargetQuorum:
		for _, n := range c.Harness.Nodes {
			if n.Service == t.Scope {
				return true
			}
		}
		return false
	}
	return true
}

// ---------------------------------------------------------------------------
// lookups
// ---------------------------------------------------------------------------

// Node returns the node with the given logical id.
func (c *Config) Node(id string) (NodeConfig, bool) {
	for _, n := range c.Harness.Nodes {
		if n.ID == id {
			return n, true
		}
	}
	return NodeConfig{}, false
}

// NodesOfService returns every node backed by the named service, in
// declaration order.
func (c *Config) NodesOfService(service string) []NodeConfig {
	out := make([]NodeConfig, 0, len(c.Harness.Nodes))
	for _, n := range c.Harness.Nodes {
		if n.Service == service {
			out = append(out, n)
		}
	}
	return out
}

// Profile returns the named run profile.
func (c *Config) Profile(name string) (Profile, bool) {
	p, ok := c.Profiles[name]
	return p, ok
}

// DriverProfileByName returns the named driver profile.
func (c *Config) DriverProfileByName(name string) (DriverProfile, bool) {
	p, ok := c.Driver.Profiles[name]
	return p, ok
}

// ProfileNames returns the run profile names in sorted order.
func (c *Config) ProfileNames() []string { return sortedKeys(c.Profiles) }

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedFloatKeys(m map[string]float64) []string { return sortedKeys(m) }

// String renders a short human summary; it is not a config dump.
func (c *Config) String() string {
	return fmt.Sprintf("prothesis config %q (%s backend, %d nodes, %d profiles)",
		c.Name, c.Harness.Backend, len(c.Harness.Nodes), len(c.Profiles))
}
