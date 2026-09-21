package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// The chat transport
//
// Ported from the author's distadv project (not in this repository; D-075)
// with one deliberate deviation: the port carries a REAL timeout (distadv's client has none, and
// a wedged model server would hold a world budget open forever). The stdlib
// idiom matches internal/telemetry/http.go.
//
// The served model is a "thinking" model: its reasoning lands in
// reasoning_content and the ANSWER in choices[0].message.content. Only
// content is read; the reasoning channel is never parsed as a schedule.
// ---------------------------------------------------------------------------

// maxResponseBytes caps how much of an API response body is read.
const maxResponseBytes = 8 << 20 // 8 MiB

// systemPrompt pins the output contract. Ported from distadv.
const systemPrompt = "You are an adversarial fault-schedule designer for distributed-systems consistency testing. " +
	"You always answer with exactly one JSON object and never with any other text."

// chatMessage is one message in a chat-completions request.
type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// chatRequest is the request body for a chat-completions call.
type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Temperature float64       `json:"temperature"`
}

// chatResponse is the subset of the chat-completions response we consume.
// reasoning_content is deliberately absent: the answer is content, and a
// struct that carried the reasoning channel would invite reading it.
type chatResponse struct {
	Choices []struct {
		Message chatMessage `json:"message"`
	} `json:"choices"`
}

// client is one OpenAI-compatible chat-completions endpoint.
type client struct {
	endpoint string
	model    string
	apiKey   string
	http     *http.Client
}

// newClient builds a client. Temperature is not a parameter: it is always
// sent as 0, because a sampled proposal stream is not reproducible (D-075).
func newClient(endpoint, model, apiKey string, timeout time.Duration) *client {
	return &client{
		endpoint: endpoint,
		model:    model,
		apiKey:   apiKey,
		http:     &http.Client{Timeout: timeout},
	}
}

// chat performs one chat-completions round trip and returns the assistant
// message content. Transport-level problems (network, status, shape) are
// returned as errors; cancellation and the timeout are both honoured.
func (c *client) chat(ctx context.Context, userPrompt string) (string, error) {
	payload, err := json.Marshal(chatRequest{
		Model: c.model,
		Messages: []chatMessage{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: userPrompt},
		},
		Temperature: 0,
	})
	if err != nil {
		return "", fmt.Errorf("llm: encoding request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(payload))
	if err != nil {
		return "", fmt.Errorf("llm: building request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("llm: http request: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return "", fmt.Errorf("llm: reading response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("llm: api returned status %d: %s", resp.StatusCode, truncateForError(string(body)))
	}
	var cr chatResponse
	if err := json.Unmarshal(body, &cr); err != nil {
		return "", fmt.Errorf("llm: decoding response: %w", err)
	}
	if len(cr.Choices) == 0 {
		return "", errors.New("llm: api response contained no choices")
	}
	content := strings.TrimSpace(cr.Choices[0].Message.Content)
	if content == "" {
		return "", errors.New("llm: api response contained an empty message " +
			"(a thinking model's reasoning is not the answer; content is)")
	}
	return content, nil
}

// truncateForError shortens s for inclusion in an error message. Ported from
// distadv.
func truncateForError(s string) string {
	const max = 200
	if len(s) <= max {
		return s
	}
	return s[:max] + "...(truncated)"
}
