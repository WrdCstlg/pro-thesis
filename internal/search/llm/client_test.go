package llm

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The request shape is the contract with the model server: the OpenAI
// chat-completions path the operator configured, temperature pinned to 0, and
// no Authorization header unless the operator named an env var.
func TestClientSendsTemperatureZeroAndNoAuthByDefault(t *testing.T) {
	srv := newScriptedServer(t, []string{chatCompletion(t, "pong")})
	c := newClient(srv.URL+"/v1/chat/completions", "qwen3.8", "", 5*time.Second)

	content, err := c.chat(context.Background(), "ping")
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if content != "pong" {
		t.Errorf("content = %q, want %q", content, "pong")
	}
	seen := srv.received()
	if len(seen) != 1 {
		t.Fatalf("server saw %d requests, want 1", len(seen))
	}
	req := seen[0]
	if req.Path != "/v1/chat/completions" {
		t.Errorf("path = %q, want /v1/chat/completions: the configured endpoint's path is the contract", req.Path)
	}
	if req.Body.Model != "qwen3.8" {
		t.Errorf("model = %q, want qwen3.8", req.Body.Model)
	}
	if req.Body.Temperature != 0 {
		t.Errorf("temperature = %v, want 0: there is no temperature knob; reproducibility is not optional (D-075)", req.Body.Temperature)
	}
	if req.Authorization != "" {
		t.Errorf("Authorization = %q, want empty when api_key_env is unset", req.Authorization)
	}
	if len(req.Body.Messages) != 2 || req.Body.Messages[0].Role != "system" || req.Body.Messages[1].Role != "user" {
		t.Errorf("messages = %+v, want [system, user]", req.Body.Messages)
	}
	if req.Body.Messages[1].Content != "ping" {
		t.Errorf("user content = %q, want ping", req.Body.Messages[1].Content)
	}
}

func TestClientSendsBearerWhenAnAPIKeyIsConfigured(t *testing.T) {
	srv := newScriptedServer(t, []string{chatCompletion(t, "pong")})
	c := newClient(srv.URL, "qwen3.8", "sekret", 5*time.Second)
	if _, err := c.chat(context.Background(), "ping"); err != nil {
		t.Fatalf("chat: %v", err)
	}
	if got := srv.received()[0].Authorization; got != "Bearer sekret" {
		t.Errorf("Authorization = %q, want %q", got, "Bearer sekret")
	}
}

// hungServer never answers until released; the release runs before Close so
// teardown never waits on it.
func hungServer(t *testing.T) *httptest.Server {
	t.Helper()
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		case <-time.After(10 * time.Second):
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })
	return srv
}

// A hung server must cost the configured timeout and no more. distadv's
// client carried no timeout at all; that defect is not ported.
func TestClientTimesOutOnAHungServer(t *testing.T) {
	srv := hungServer(t)

	c := newClient(srv.URL, "qwen3.8", "", 50*time.Millisecond)
	start := time.Now()
	_, err := c.chat(context.Background(), "ping")
	if err == nil {
		t.Fatal("a hung server produced no error")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("the hung server held the call for %s; the timeout is the contract", elapsed)
	}
}

func TestClientHonoursCancellation(t *testing.T) {
	srv := hungServer(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := newClient(srv.URL, "qwen3.8", "", 30*time.Second)
	start := time.Now()
	_, err := c.chat(ctx, "ping")
	if err == nil {
		t.Fatal("a cancelled context produced no error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want it to wrap context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("cancellation took %s", elapsed)
	}
}

// The served model is a thinking model: the reasoning lands in
// reasoning_content and the ANSWER in content. Reading the wrong field would
// feed the model's private monologue to the fault parser.
func TestClientReadsContentNotReasoning(t *testing.T) {
	srv := newScriptedServer(t, []string{chatCompletion(t, "the answer")})
	c := newClient(srv.URL, "qwen3.8", "", 5*time.Second)
	content, err := c.chat(context.Background(), "ping")
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if content != "the answer" {
		t.Errorf("content = %q, want %q (reasoning_content must never be read as the answer)", content, "the answer")
	}
	if strings.Contains(content, "reasoning") {
		t.Errorf("content %q carries the reasoning channel", content)
	}
}
