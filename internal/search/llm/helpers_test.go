package llm

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/WrdCstlg/pro-thesis/internal/perturber"
	"github.com/WrdCstlg/pro-thesis/internal/recorder"
	"github.com/WrdCstlg/pro-thesis/internal/search"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// testConfig mirrors internal/search's helpers_test.go fixture: the kvfixture
// perturber policy, including its deny list, so the policy-rejection path is
// exercised by a kind the grammar accepts and the policy refuses.
func testConfig() *schema.Config {
	return &schema.Config{
		Version: schema.ConfigVersion,
		Name:    "kvfixture",
		Perturber: schema.PerturberConfig{
			Budget: schema.PerturberBudget{MaxConcurrentFaults: 3, MaxFaultsPerWorld: 24},
			Allow: []schema.FaultKind{
				schema.FaultNetPartition, schema.FaultNetLatency, schema.FaultNetLoss,
				schema.FaultProcKill, schema.FaultProcPause, schema.FaultIOLatency,
			},
			Deny:        []schema.FaultKind{schema.FaultIOFill, schema.FaultClockSkew},
			Constraints: []string{"never partition more than minority of kv"},
		},
		Driver: schema.DriverConfig{
			Cmd: "./bin/loadgen --history {history_path} --seed {seed} --profile {profile}",
			Profiles: map[string]schema.DriverProfile{
				"linear": {Clients: 16, Ops: 60000, Mix: map[string]float64{"read": 0.5, "write": 0.5}},
			},
		},
	}
}

// testTopology is the fixture's three-node kv cluster.
func testTopology(t *testing.T) *perturber.Topology {
	t.Helper()
	top, err := perturber.NewTopologyFromNodes([]perturber.Node{
		{ID: "kv-n1", Service: "kv", ComposeService: "kv-n1", ContainerID: "c1", HostPort: 18081, ContainerPort: 8080},
		{ID: "kv-n2", Service: "kv", ComposeService: "kv-n2", ContainerID: "c2", HostPort: 18082, ContainerPort: 8080},
		{ID: "kv-n3", Service: "kv", ComposeService: "kv-n3", ContainerID: "c3", HostPort: 18083, ContainerPort: 8080},
	})
	if err != nil {
		t.Fatalf("build topology: %v", err)
	}
	return top
}

// testParams assembles a complete strategy Params against the fixture shape.
func testParams(t *testing.T, seed uint64) search.Params {
	t.Helper()
	cfg := testConfig()
	top := testTopology(t)
	sp, err := search.NewSpace(cfg, top, search.DefaultWindowPolicy())
	if err != nil {
		t.Fatalf("NewSpace: %v", err)
	}
	v, err := search.NewValidator(cfg, top, nil)
	if err != nil {
		t.Fatalf("NewValidator: %v", err)
	}
	return search.Params{
		Config:    cfg,
		Space:     sp,
		Validator: v,
		Corpus:    search.NewCorpus(search.DefaultEnergyParams()),
		Streams:   search.NewStreams(recorder.Seed(seed)),
		Base:      schema.NewWorld(0, "default", "linear"),
	}
}

// testOptions builds Options against a scripted server's URL and a temp
// recording directory.
func testOptions(t *testing.T, endpoint string) Options {
	t.Helper()
	return Options{
		Params: testParams(t, 42),
		LLM: schema.LLMConfig{
			Endpoint:   endpoint,
			Model:      "qwen3.8",
			MaxRetries: 2,
			TimeoutMS:  30000,
		},
		Dir: t.TempDir(),
	}
}

// capturedRequest is what the scripted server saw, verbatim enough to assert
// on the wire contract.
type capturedRequest struct {
	Path          string
	Authorization string
	Body          chatRequest
}

// scriptedServer is a fake chat-completions API, ported from distadv's
// strategy_test.go: it replies with scripts by 1-based call number (repeating
// the last) and records every received request.
type scriptedServer struct {
	*httptest.Server
	calls atomic.Int32

	mu   sync.Mutex
	seen []capturedRequest
}

func newScriptedServer(t *testing.T, scripts []string) *scriptedServer {
	t.Helper()
	ss := &scriptedServer{}
	ss.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(ss.calls.Add(1))
		var req chatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err == nil {
			ss.mu.Lock()
			ss.seen = append(ss.seen, capturedRequest{
				Path:          r.URL.Path,
				Authorization: r.Header.Get("Authorization"),
				Body:          req,
			})
			ss.mu.Unlock()
		}
		idx := n - 1
		if idx >= len(scripts) {
			idx = len(scripts) - 1
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(scripts[idx]))
	}))
	t.Cleanup(ss.Close)
	return ss
}

func (s *scriptedServer) callCount() int { return int(s.calls.Load()) }

func (s *scriptedServer) received() []capturedRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]capturedRequest(nil), s.seen...)
}

// lastPrompt returns the user prompt of the n-th (0-based) recorded call.
func (s *scriptedServer) prompt(n int) string {
	seen := s.received()
	if n >= len(seen) || len(seen[n].Body.Messages) == 0 {
		return ""
	}
	return seen[n].Body.Messages[len(seen[n].Body.Messages)-1].Content
}

// chatCompletion encodes a chat-completions response whose single choice
// carries content, with the thinking model's reasoning_content alongside it.
func chatCompletion(t *testing.T, content string) string {
	t.Helper()
	body := map[string]any{
		"choices": []map[string]any{
			{"message": map[string]string{
				"role":              "assistant",
				"content":           content,
				"reasoning_content": "the model's private reasoning, never the answer",
			}},
		},
	}
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal fake response: %v", err)
	}
	return string(b)
}

// proposalJSON encodes a proposal envelope as the content of a chat message.
func proposalJSON(t *testing.T, schedule ...string) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"schedule":  schedule,
		"reasoning": "test reasoning",
	})
	if err != nil {
		t.Fatalf("marshal proposal: %v", err)
	}
	return string(b)
}
