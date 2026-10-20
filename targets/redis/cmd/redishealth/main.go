// Command redishealth is the Redis target's health shim: a small HTTP server
// that answers the harness's HTTP health probe for a node that speaks only
// RESP.
//
// It exists because of a contract gap: harness.health is an HTTP probe
// (docs/TARGETS.md, D-010) and Redis has no HTTP interface (README, "Contract
// gaps"). It runs inside Linux, one per node, in a container from the same
// pinned redis image with this binary mounted read-only and the entrypoint
// overridden, sharing the node's network namespace (docker-compose.yaml
// `network_mode: service:<node>`). It talks to the node over 127.0.0.1, which a
// partition of the node does not cut, so it reports whether the node's process
// answers, not whether the cluster can reach it.
//
// GET /health, on every request and with nothing cached:
//
//	PING                  must answer +PONG
//	INFO replication      must answer a bulk string naming role:master or role:slave
//
// 200 with {"role":..., "master_link_status":..., "connected_slaves":...} when
// both hold; 503 with {"error":...} otherwise, including a dial error, a reply
// that does not come within --timeout, an error reply such as -LOADING, and a
// malformed reply. A replica whose link to the primary is down still answers
// reads (replica-serve-stale-data defaults to yes), so it counts as
// answering; whether replication is established is the steady-state probe's
// question, not this one.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/WrdCstlg/pro-thesis/targets/redis/internal/redisapi"
)

func main() {
	listen := flag.String("listen", ":8080", "address to serve HTTP on")
	redis := flag.String("redis", "127.0.0.1:6379", "the node's RESP address, inside the shared network namespace")
	timeout := flag.Duration("timeout", time.Second, "per-check timeout, dial included")
	flag.Parse()
	srv := &http.Server{
		Addr:              *listen,
		Handler:           handler(*redis, *timeout),
		ReadHeaderTimeout: 5 * time.Second,
	}
	fmt.Fprintf(os.Stderr, "redishealth: serving %s for %s (timeout %s)\n", *listen, *redis, *timeout)
	log.Fatal(srv.ListenAndServe())
}

// handler answers every path the same way: the harness's probe template names
// the path, and this shim has one question to answer.
func handler(addr string, timeout time.Duration) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		status, body := check(addr, timeout)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(body)
	})
}

func check(addr string, timeout time.Duration) (int, []byte) {
	fail := func(format string, args ...any) (int, []byte) {
		b, _ := json.Marshal(map[string]string{"error": fmt.Sprintf(format, args...)})
		return http.StatusServiceUnavailable, b
	}
	c, err := redisapi.Dial(addr, timeout)
	if err != nil {
		return fail("dial %s: %v", addr, err)
	}
	defer c.Close()
	rep, err := c.Do("PING")
	if err != nil {
		return fail("PING: %v", err)
	}
	if rep.Kind != redisapi.SimpleString || rep.Str != "PONG" {
		return fail("PING answered %s, not +PONG", rep)
	}
	repl, err := redisapi.Info(c, "replication")
	if err != nil {
		return fail("%v", err)
	}
	role := repl["role"]
	if role != "master" && role != "slave" {
		return fail("INFO replication names role %q", role)
	}
	b, _ := json.Marshal(map[string]string{
		"role":               role,
		"master_link_status": repl["master_link_status"],
		"connected_slaves":   repl["connected_slaves"],
	})
	return http.StatusOK, b
}
