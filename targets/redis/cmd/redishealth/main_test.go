package main

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/WrdCstlg/pro-thesis/targets/redis/internal/respfake"
)

// Reply shapes from Redis's documentation, not from a live server: PING
// answers +PONG; INFO replication answers one bulk string carrying role:master
// or role:slave; a server still loading its dataset answers most commands with
// -LOADING.

func healthy(args []string) string {
	switch strings.ToUpper(args[0]) {
	case "PING":
		return "+PONG\r\n"
	case "INFO":
		return respfake.Bulk("# Replication\r\nrole:slave\r\nmaster_link_status:up\r\n")
	}
	return "-ERR unknown command\r\n"
}

func get(t *testing.T, redisAddr string) (int, string) {
	t.Helper()
	srv := httptest.NewServer(handler(redisAddr, 300*time.Millisecond))
	defer srv.Close()
	client := &http.Client{Timeout: 5 * time.Second}
	start := time.Now()
	resp, err := client.Get(srv.URL + "/health")
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("the shim took %s to answer; it must answer within its own timeout", took)
	}
	return resp.StatusCode, string(b)
}

func TestTheHealthShimFailsWhenTheNodeDoesNotAnswer(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := ln.Addr().String()
	_ = ln.Close()

	cases := map[string]func([]string) string{
		"accepts and never answers": func([]string) string { return respfake.Silent },
		"closes after the command":  func([]string) string { return respfake.Hangup },
		"still loading": func(args []string) string {
			return "-LOADING Redis is loading the dataset in memory\r\n"
		},
		"PING answered with something else": func(args []string) string {
			if strings.EqualFold(args[0], "PING") {
				return "+PING\r\n"
			}
			return healthy(args)
		},
		"INFO names no role": func(args []string) string {
			if strings.EqualFold(args[0], "INFO") {
				return respfake.Bulk("# Replication\r\nconnected_slaves:0\r\n")
			}
			return healthy(args)
		},
	}
	t.Run("nothing listens", func(t *testing.T) {
		if code, body := get(t, dead); code/100 == 2 {
			t.Fatalf("no server at %s: the shim answered %d %s", dead, code, body)
		}
	})
	for name, h := range cases {
		t.Run(name, func(t *testing.T) {
			fake, err := respfake.Start(h)
			if err != nil {
				t.Fatal(err)
			}
			defer fake.Close()
			if code, body := get(t, fake.Addr()); code/100 == 2 {
				t.Fatalf("the shim answered %d %s", code, body)
			}
		})
	}

	// And the boundary: a node that answers PING and INFO is healthy.
	fake, err := respfake.Start(healthy)
	if err != nil {
		t.Fatal(err)
	}
	defer fake.Close()
	code, body := get(t, fake.Addr())
	if code != http.StatusOK || !strings.Contains(body, `"role":"slave"`) {
		t.Fatalf("a node answering +PONG and role:slave: got %d %s, want 200 naming the role", code, body)
	}
}
