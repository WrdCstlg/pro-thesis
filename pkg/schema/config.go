package schema

import (
	"fmt"
	"regexp"
	"sort"

	"gopkg.in/yaml.v3"
)

// ---------------------------------------------------------------------------
// prothesis.yaml (directive 4.2) plus Addendum A.10's top-level `search:` block.
//
// Every yaml tag below is transcribed from the directive. Nothing here is
// improvised; the one extension is the `search:` section, which Addendum A.10
// specifies in full and which Phase 0 carries as FORMAT CAPACITY ONLY: the
// fields exist and round-trip, no search behaviour is implemented.
// ---------------------------------------------------------------------------

// Driver command placeholders (directive 4.2).
//
// The first three are the directive's own vocabulary. {profile} interpolates
// the profile NAME, not its contents, so a driver cannot learn `clients`,
// `ops` or `mix` through the command template: logged as OQ-012.
const (
	PlaceholderHistoryPath = "{history_path}"
	PlaceholderSeed        = "{seed}"
	PlaceholderProfile     = "{profile}"

	// PlaceholderPlanPath is ADDITIVE. See DECISIONS.md D-021 and OQ-012.
	//
	// Phase 5 shrinks a violation by running ddmin over the OPERATION trace,
	// which requires handing the driver an explicit, reduced list of operations:
	// typically fewer than ten out of twenty thousand. No frozen placeholder
	// can express that: {seed} regenerates the whole pseudo-random stream and
	// {profile} names a profile. Without this, Phase 5's second minimization
	// stage is unimplementable rather than merely awkward.
	//
	// The directive fixes the SCHEMA FIELD NAMES; it nowhere declares the
	// placeholder vocabulary closed, and `driver.cmd` is an opaque template
	// string. Widening it is therefore additive rather than a schema change.
	//
	// Transport is dual: the placeholder is substituted when present, and
	// PlanPathEnv is ALWAYS exported into the driver's environment, so a driver
	// that does not spell {plan_path} in its command line can still find the
	// plan. A driver that supports neither simply ignores both, and op
	// shrinking is reported as not attempted for that target rather than
	// silently producing a wrong minimal repro.
	PlaceholderPlanPath = "{plan_path}"

	// PlanPathEnv is the environment fallback for PlaceholderPlanPath.
	PlanPathEnv = "PROTHESIS_PLAN_PATH"
)

// knownPlaceholders is the complete placeholder vocabulary of driver.cmd.
var knownPlaceholders = map[string]bool{
	PlaceholderHistoryPath: true,
	PlaceholderSeed:        true,
	PlaceholderProfile:     true,
	PlaceholderPlanPath:    true,
}

// placeholderToken matches a placeholder-shaped token: {lower_snake_case}.
//
// Deliberately narrow. A JSON argument (`{"a":1}`), a shell brace expansion
// (`{a,b}`) and a Go template (`{{.X}}`) all fail to match, so none is mistaken
// for a typo'd placeholder.
var placeholderToken = regexp.MustCompile(`\{[a-z][a-z0-9_]*\}`)

// UnknownPlaceholders returns the placeholder-shaped tokens in tmpl that are not
// part of the vocabulary.
//
// This exists because of a real defect found by testing: a typo'd placeholder is
// silently catastrophic. `{histori_path}` is substituted by nothing and rejected
// by nothing, so the driver receives the literal brace string as its history
// path and writes every operation to a file of that name. The canonical history
// is then empty, the phase-marker merge treats it as "the driver never started",
// and the run reports INCONCLUSIVE from no_stuck_op: a verdict that reads like
// an environment flake and points nowhere near the typo. Worse, a Phase 3
// consistency oracle reading an empty history is VACUOUSLY SATISFIED: the run
// goes green because nothing happened.
//
// It scans the TEMPLATE rather than the substituted argv on purpose. A
// substituted value may legitimately contain braces (a path, a JSON blob), and
// checking after substitution would reject those.
func UnknownPlaceholders(tmpl string) []string {
	var out []string
	seen := map[string]bool{}
	for _, tok := range placeholderToken.FindAllString(tmpl, -1) {
		if knownPlaceholders[tok] || seen[tok] {
			continue
		}
		seen[tok] = true
		out = append(out, tok)
	}
	sort.Strings(out)
	return out
}

// Config is a decoded prothesis.yaml.
//
// Decode it with DecodeConfig, which starts from DefaultConfig and decodes in
// place so unset scalars keep their defaults. Do not construct it with a bare
// struct literal outside tests.
type Config struct {
	Version   string          `yaml:"version"`
	Name      string          `yaml:"name"`
	Harness   HarnessConfig   `yaml:"harness"`
	Driver    DriverConfig    `yaml:"driver"`
	Perturber PerturberConfig `yaml:"perturber"`
	Oracles   OraclesConfig   `yaml:"oracles"`

	// Profiles is the top-level run profile map: `profiles.smoke`, `.gate`,
	// `.soak`. It is NEVER pre-seeded with defaults: see DefaultConfig.
	Profiles map[string]Profile `yaml:"profiles"`

	Artifacts ArtifactsConfig `yaml:"artifacts"`

	// Search is Addendum A.10's top-level block.
	//
	// It is a MAPPING, and it is a different key from the BOOLEAN
	// `profiles.<name>.search`. YAML resolves them by path, so they do not
	// collide, but the two-level naming is a trap for anyone reading the file:
	// the profile boolean gates WHETHER search engages for that profile; this
	// block configures HOW it behaves once engaged. Logged as OQ-005.
	Search SearchConfig `yaml:"search"`
}

// ---------------------------------------------------------------------------
// harness
// ---------------------------------------------------------------------------

// HarnessConfig is the `harness:` section.
type HarnessConfig struct {
	Backend Backend `yaml:"backend"`
	// File is the topology definition the backend consumes: a compose file for
	// the compose backend.
	File string `yaml:"file"`

	// PrefixEnv is the environment variable the topology file uses to namespace
	// per-worker resource names, e.g. `KV_PREFIX` for a compose file spelling
	// `container_name: "${KV_PREFIX:-prothesis}-kv-n1"`.
	//
	// ADDITIVE, and the companion to NodeConfig.PortEnv: without it two parallel
	// workers ask for the same container_name and the second fails to create.
	// Optional: a topology file that does not pin container names needs nothing
	// here, because compose already namespaces by project.
	PrefixEnv string `yaml:"prefix_env,omitempty"`

	Nodes       []NodeConfig     `yaml:"nodes"`
	Health      []HealthProbe    `yaml:"health"`
	SteadyState SteadyStateProbe `yaml:"steady_state"`

	// RoleProbe is `harness.role_probe`, OPTIONAL: a host-side program, exec'd
	// like steady_state.probe with the same PROTHESIS_* environment, that
	// prints one JSON object per line ({"node","role","term"} or
	// {"node","error"}) so `role:` targets can bind on a system that has no
	// /status document (OQ-063 item 4, D-066). Absent, roles are read from each
	// node's /status as before. Covered by .prothesis/lock: a swapped probe
	// changes which node `role:leader` hits.
	RoleProbe SteadyStateProbe `yaml:"role_probe,omitempty"`
}

// NodeConfig is one entry of `harness.nodes`.
type NodeConfig struct {
	// ID is the logical node id used by fault targets (n1, n2, pg).
	ID string `yaml:"id"`

	// Service is the LOGICAL group this node belongs to, and it is what the
	// fault grammar targets: `kv:*`, `minority(kv)`, `majority(kv)`,
	// `any(2, kv)`, and the constraint vocabulary ("never partition more than
	// minority of kv"). The directive's own section 4.2 sample gives all three
	// kv nodes `service: kv`, which is exactly this reading.
	Service string `yaml:"service"`

	// ComposeService is the PHYSICAL compose service backing this node.
	//
	// ADDITIVE, and it exists to resolve OQ-016. Container IPs are not routable
	// from a Windows host, so every node must publish its own 127.0.0.1 port,
	// which forces one compose service per node (kv-n1, kv-n2, kv-n3). But the
	// frozen grammar resolves `minority(kv)` over nodes whose Service is `kv`.
	// Those two requirements are only compatible if the logical group and the
	// physical service are separate fields.
	//
	// The alternative (reinterpreting `kv:*` as a prefix glob so it matches
	// kv-n1) was rejected: it would silently redefine `minority(kv)`,
	// `majority(kv)` and the frozen constraint strings, changing the meaning of
	// a normative grammar to avoid adding a field.
	//
	// Empty means "same as Service", so the directive's section 4.2 sample and
	// any single-service topology keep working unchanged.
	ComposeService string `yaml:"compose_service,omitempty"`

	// PortEnv is the environment variable this node's PUBLISHED host port is
	// parameterised by in the topology file, e.g. `KV_PORT_N1` for a compose
	// file spelling `ports: ["127.0.0.1:${KV_PORT_N1:-18081}:8080"]`.
	//
	// ADDITIVE, and it exists because the parallel executor cannot otherwise be
	// used correctly by accident. It allocates a per-slot port band and tells the
	// driver about it through PROTHESIS_TARGETS; for the CLUSTER to be published
	// on that band, the band has to reach the topology file, and only the file's
	// author knows what its variables are called. That mapping used to live
	// exclusively in a `--worker-env` flag, so a `thesis search` invocation that
	// forgot it drove every world against ports nothing was listening on and
	// produced histories in which no operation was checkable: an INCONCLUSIVE
	// that reads like a shrug rather than a misconfiguration. See OQ-056.
	//
	// It belongs HERE for the same reason ComposeService does: it is a fact about
	// this project's topology file, not a per-invocation choice. `--worker-env`
	// still wins when given, so a caller can override it without editing config.
	PortEnv string `yaml:"port_env,omitempty"`

	// Port is the node's CLIENT port INSIDE the container, the one a health
	// probe's {port} placeholder ultimately resolves to on the host.
	//
	// ADDITIVE, resolving OQ-017. The frozen health probe template is
	// "http://{host}:{port}/healthz", but nothing in `harness.nodes` says which
	// port a node serves on, so {port} had nothing to bind to and the backend
	// had to assume one. An assumption is fine for the directive's own sample
	// and for the fixture (both use 8080) and wrong for the first real target
	// that does not.
	//
	// Zero means DefaultClientPort. It is the CONTAINER port, not the published
	// host port: the published port is assigned by compose and discovered at
	// bind time, because pinning it in config would make two concurrent runs
	// collide on the same host port.
	Port int `yaml:"port,omitempty"`

	// RoleHint is advisory and system-specific. It is an OPEN string set, not
	// an enum; see the Role* constants for the common spellings.
	RoleHint string `yaml:"role_hint"`
}

// DefaultClientPort is the in-container client port assumed when a node
// declares none. It matches the directive's section 4.2 sample and the fixture.
const DefaultClientPort = 8080

// ClientPort is the in-container client port for this node.
func (n NodeConfig) ClientPort() int {
	if n.Port > 0 {
		return n.Port
	}
	return DefaultClientPort
}

// EffectiveComposeService is the physical service name a backend should
// address for this node. Every backend must use this rather than Service,
// which is the logical group the fault grammar targets.
func (n NodeConfig) EffectiveComposeService() string {
	if n.ComposeService != "" {
		return n.ComposeService
	}
	return n.Service
}

// HealthProbe is one entry of `harness.health`.
//
// Probes run HOST-SIDE against published 127.0.0.1 ports: {host} is 127.0.0.1
// and {port} is the published host port. Container IPs are not routable from a
// Windows host, so probing them is not an option. See DECISIONS.md D-010.
type HealthProbe struct {
	// Node is a target expression in the fault-grammar target vocabulary,
	// typically a wildcard such as "kv:*".
	Node string `yaml:"node"`
	// Probe is a URL template. {host} and {port} are interpolated.
	Probe   string   `yaml:"probe"`
	Timeout Duration `yaml:"timeout"`
}

// SteadyStateProbe is `harness.steady_state`.
//
// The probe is EXEC'd, not run through a shell. The directive's sample value is
// a .sh path, which cannot run on a Windows host (Git Bash is non-functional on
// the build machine); the compose backend executes it inside a helper container
// instead. See DECISIONS.md D-010 and OQ-007.5.
type SteadyStateProbe struct {
	Probe   string   `yaml:"probe"`
	Timeout Duration `yaml:"timeout"`
}

// ---------------------------------------------------------------------------
// driver
// ---------------------------------------------------------------------------

// DriverConfig is the `driver:` section.
type DriverConfig struct {
	// Cmd is the workload generator command template. See the Placeholder*
	// constants.
	Cmd string `yaml:"cmd"`
	// Profiles is the named workload map. NEVER pre-seeded: see DefaultConfig.
	Profiles map[string]DriverProfile `yaml:"profiles"`
}

// DriverProfile is one entry of `driver.profiles`.
//
// There is deliberately no unknown-key escape hatch here. A custom
// UnmarshalYAML on this type would disable Decoder.KnownFields for its subtree
// (the option does not propagate into a custom unmarshaler), which would let a
// typo inside a driver profile decode silently.
type DriverProfile struct {
	Clients int `yaml:"clients"`
	Ops     int `yaml:"ops"`
	// Mix is the operation mix. The directive's samples sum to 1.0, but nothing
	// declares it a probability distribution, so relative weights are accepted
	// and the sum is NOT validated. NEVER pre-seeded: see DefaultConfig.
	Mix map[string]float64 `yaml:"mix"`
}

// ---------------------------------------------------------------------------
// perturber
// ---------------------------------------------------------------------------

// PerturberConfig is the `perturber:` section.
type PerturberConfig struct {
	Budget PerturberBudget `yaml:"budget"`
	Allow  []FaultKind     `yaml:"allow"`
	Deny   []FaultKind     `yaml:"deny"`
	// Constraints are natural-language safety constraints on the fault search,
	// e.g. "never partition more than minority of kv".
	//
	// Phase 0 stores them VERBATIM and validates nothing about them: no grammar
	// is defined anywhere, and no deterministic parser can enforce an English
	// sentence. Phase 2 must introduce a typed grammar and reject an
	// unparseable constraint rather than ignore it: a silently ignored safety
	// constraint would let the search partition a majority and manufacture a
	// false violation. Logged as an open question in 01-schema.design.md.
	Constraints []string `yaml:"constraints"`
}

// PerturberBudget is `perturber.budget`.
type PerturberBudget struct {
	MaxConcurrentFaults int `yaml:"max_concurrent_faults"`
	MaxFaultsPerWorld   int `yaml:"max_faults_per_world"`
}

// EffectiveFaultKinds returns the kinds permitted by allow minus deny, in
// registry order. An empty Allow means every registered kind is permitted.
func (p PerturberConfig) EffectiveFaultKinds() []FaultKind {
	denied := make(map[FaultKind]bool, len(p.Deny))
	for _, k := range p.Deny {
		denied[k] = true
	}
	allowed := make(map[FaultKind]bool, len(p.Allow))
	for _, k := range p.Allow {
		allowed[k] = true
	}
	out := make([]FaultKind, 0, len(AllFaultKinds))
	for _, k := range AllFaultKinds {
		if denied[k] {
			continue
		}
		if len(p.Allow) > 0 && !allowed[k] {
			continue
		}
		out = append(out, k)
	}
	return out
}

// ---------------------------------------------------------------------------
// oracles
// ---------------------------------------------------------------------------

// OraclesConfig is the `oracles:` section.
type OraclesConfig struct {
	Dir string `yaml:"dir"`
	// Builtin is the list of built-in oracles in force.
	//
	// It is NOT defaulted to the full set. An absent list must hash differently
	// from a full list, or deleting `no_stuck_op` would be invisible to the
	// Phase 3 lock, which is exactly the gate-weakening invariant I6 exists to
	// catch.
	Builtin []BuiltinOracle `yaml:"builtin"`
}

// ---------------------------------------------------------------------------
// profiles
// ---------------------------------------------------------------------------

// WorldsUnbounded is the `worlds` value meaning "run until the budget expires"
// (directive 4.2: `soak: { budget: 8h, worlds: -1, ... }`).
const WorldsUnbounded = -1

// Profile is one entry of the top-level `profiles:` map.
type Profile struct {
	Budget Duration `yaml:"budget"`
	// Worlds is a pointer so that "key absent" and "explicitly zero" stay
	// distinguishable. Profile budgets are lock-covered, so substituting a
	// default for an explicitly written value there must never happen
	// silently.
	//
	//	nil               inherit the CLI / caller default
	//	WorldsUnbounded   run until the budget expires
	//	n > 0             run exactly n worlds
	//	0                 rejected: meaningless, and better reported than guessed
	Worlds *int `yaml:"worlds"`
	// DriverProfile names an entry of driver.profiles. A top-level profile and
	// a driver profile may share a name (the directive's own sample uses
	// smoke/gate/soak for both) and they are resolved in different maps.
	DriverProfile string `yaml:"driver_profile"`
	// Search gates whether the guided search engine engages for this profile.
	// It is a BOOLEAN and is a different key from the top-level `search:`
	// mapping. See Config.Search and OQ-005.
	Search bool `yaml:"search"`
}

// ---------------------------------------------------------------------------
// artifacts
// ---------------------------------------------------------------------------

// ArtifactsConfig is the `artifacts:` section.
type ArtifactsConfig struct {
	Dir           string       `yaml:"dir"`
	RetainPassing RetainPolicy `yaml:"retain_passing"`
	RetainFailing RetainPolicy `yaml:"retain_failing"`
}

// ---------------------------------------------------------------------------
// search (Addendum A.10)
// ---------------------------------------------------------------------------

// SearchStrategy is `search.strategy`.
type SearchStrategy string

const (
	StrategySaboteur SearchStrategy = "saboteur"
	StrategyRandom   SearchStrategy = "random"
	StrategyHybrid   SearchStrategy = "hybrid"
	// StrategyLLM is an LLM-proposed adversarial schedule stream, served by a
	// local OpenAI-compatible endpoint (search.llm). Proposals are written in
	// this schema's own fault wire format and validated by the same code path
	// as human-authored schedules. See internal/search/llm and D-075.
	StrategyLLM SearchStrategy = "llm"
)

// AllSearchStrategies is the complete closed set.
var AllSearchStrategies = [...]SearchStrategy{StrategySaboteur, StrategyRandom, StrategyHybrid, StrategyLLM}

func (s SearchStrategy) Valid() bool {
	for _, x := range AllSearchStrategies {
		if s == x {
			return true
		}
	}
	return false
}

func (s *SearchStrategy) UnmarshalYAML(n *yaml.Node) error {
	v, err := strictEnumYAML(n, "search.strategy", false, func(x string) bool { return SearchStrategy(x).Valid() })
	if err != nil {
		return fmt.Errorf("%w (want one of %v)", err, AllSearchStrategies)
	}
	*s = SearchStrategy(v)
	return nil
}

// EscalationLadder is `search.escalation_ladder`.
type EscalationLadder string

const (
	// LadderDefault uses the compiled-in escalation ladder.
	LadderDefault EscalationLadder = "default"
	// LadderCustom reads .prothesis/ladder.yaml.
	LadderCustom EscalationLadder = "custom"
)

// AllEscalationLadders is the complete closed set.
var AllEscalationLadders = [...]EscalationLadder{LadderDefault, LadderCustom}

func (l EscalationLadder) Valid() bool { return l == LadderDefault || l == LadderCustom }

func (l *EscalationLadder) UnmarshalYAML(n *yaml.Node) error {
	v, err := strictEnumYAML(n, "search.escalation_ladder", false,
		func(x string) bool { return EscalationLadder(x).Valid() })
	if err != nil {
		return fmt.Errorf("%w (want one of %v)", err, AllEscalationLadders)
	}
	*l = EscalationLadder(v)
	return nil
}

// SearchConfig is Addendum A.10's top-level `search:` block.
//
// Every field is optional and defaulted. Phase 0 carries the format only; no
// Saboteur behaviour exists before Phase 4 (DECISIONS.md D-006, D-014).
type SearchConfig struct {
	Strategy SearchStrategy `yaml:"strategy"`
	// ExplorationConstant is UCT's C.
	//
	// It is a FLOAT here and that is correct: prothesis.yaml is YAML and is not
	// the .thesis world file, which is the only document that forbids floats.
	//
	// The default is the LITERAL 1.41 that A.10 writes, not math.Sqrt2. The two
	// differ in the 3rd decimal place, and every UCT comparison downstream is a
	// float comparison, so substituting sqrt(2) would silently diverge replays
	// from any run recorded against the documented default.
	ExplorationConstant float64 `yaml:"exploration_constant"`
	// ProbeBudgetPct is the percentage of the budget spent on Tier 1 probes.
	ProbeBudgetPct int `yaml:"probe_budget_pct"`
	// MaxMCTSDepth is the maximum additional faults per MCTS branch.
	MaxMCTSDepth     int              `yaml:"max_mcts_depth"`
	EscalationLadder EscalationLadder `yaml:"escalation_ladder"`
	Utility          UtilityConfig    `yaml:"utility"`
	Observe          ObserveConfig    `yaml:"observe"`
	// LLM is search.llm: the OpenAI-compatible proposal endpoint consulted
	// when strategy is `llm`. Its defaults are inert under every other
	// strategy and are validated only when llm is selected.
	LLM LLMConfig `yaml:"llm"`
}

// LLMConfig is `search.llm`: the endpoint that proposes fault schedules for
// the llm strategy.
//
// There is deliberately NO temperature knob: the request always pins
// temperature to 0, because a sampled proposal stream is not reproducible and
// a knob that defaults to "reproducible" but can be turned is a knob that
// will be left turned. The residual nondeterminism (server version, hardware)
// that temp 0 does not close is recorded in OQ-069, and the audit trail is
// llm-proposals.jsonl in the run directory.
type LLMConfig struct {
	// Endpoint is the chat-completions URL.
	Endpoint string `yaml:"endpoint"`
	// Model is the model identifier sent in the request.
	Model string `yaml:"model"`
	// APIKeyEnv names an environment variable whose value is sent as a Bearer
	// token. Empty sends no Authorization header: the default local server
	// authenticates nothing.
	APIKeyEnv string `yaml:"api_key_env"`
	// MaxRetries is how many times a rejected proposal is retried, with the
	// validator's error fed back verbatim into the next prompt.
	MaxRetries int `yaml:"max_retries"`
	// TimeoutMS bounds one model call.
	TimeoutMS int `yaml:"timeout_ms"`
}

// UtilityConfig is `search.utility`.
type UtilityConfig struct {
	ViolationWeight int `yaml:"violation_weight"`
	NoveltyWeight   int `yaml:"novelty_weight"`
	FaultPenalty    int `yaml:"fault_penalty"`
	// DurationPenalty is the cost per second of execution. A.10 writes 0.1.
	DurationPenalty float64 `yaml:"duration_penalty"`
}

// ObserveConfig is `search.observe`.
type ObserveConfig struct {
	// SampleIntervalMS is the telemetry sampling interval.
	//
	// A.4 and A.10 both say 500 ms for general telemetry; A.3's "every 200ms"
	// is the probe-specific trajectory rate and is not this knob.
	SampleIntervalMS int `yaml:"sample_interval_ms"`
	// SpiralWindow is the number of samples in the trend regression window.
	SpiralWindow int `yaml:"spiral_window"`
	// ReinforceThreshold is the minimum positive trend slope that counts as
	// REINFORCE.
	ReinforceThreshold float64 `yaml:"reinforce_threshold"`
}
