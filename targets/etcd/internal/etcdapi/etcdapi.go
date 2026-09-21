// Package etcdapi is the small slice of etcd's HTTP surface the target's
// host-side probes need, plus the PROTHESIS_* environment the harness exports
// to them. Both probes (cmd/etcdsteady, cmd/etcdrole) resolve targets and
// read /v3/maintenance/status through here so they cannot disagree about
// either.
package etcdapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// Environment the harness exports to a host-side probe
// (internal/control/steady.go SteadyStateEnv).
const (
	EnvNodes      = "PROTHESIS_NODES"
	EnvHost       = "PROTHESIS_HOST"
	EnvPortPrefix = "PROTHESIS_PORT_"
)

// Status is /v3/maintenance/status, the fields the probes read. The numbers
// are JSON strings on the wire.
type Status struct {
	Header struct {
		MemberID string `json:"member_id"`
	} `json:"header"`
	Leader    string `json:"leader"`
	RaftIndex string `json:"raftIndex"`
	RaftTerm  string `json:"raftTerm"`
}

// ResolveTargets builds the node -> base URL map from an explicit
// comma-separated list, or from the environment the harness exports.
func ResolveTargets(flagValue string) (map[string]string, error) {
	out := map[string]string{}
	if flagValue != "" {
		for i, t := range strings.Split(flagValue, ",") {
			t = strings.TrimSpace(t)
			if t == "" {
				continue
			}
			if !strings.HasPrefix(t, "http://") && !strings.HasPrefix(t, "https://") {
				t = "http://" + t
			}
			out["target"+strconv.Itoa(i)] = t
		}
	} else {
		nodes := strings.Split(os.Getenv(EnvNodes), ",")
		host := os.Getenv(EnvHost)
		if host == "" {
			host = "127.0.0.1"
		}
		for _, n := range nodes {
			n = strings.TrimSpace(n)
			if n == "" {
				continue
			}
			port := os.Getenv(EnvPortPrefix + EnvKey(n))
			if port == "" {
				return nil, fmt.Errorf("no %s%s in the environment for node %q (is this running under thesis?)",
					EnvPortPrefix, EnvKey(n), n)
			}
			out[n] = "http://" + host + ":" + port
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no targets: pass --targets or run under thesis, which exports %s", EnvNodes)
	}
	return out, nil
}

// EnvKey mirrors internal/control/steady.go's envNodeKey: upper-case, with
// anything outside [A-Z0-9] replaced by '_'.
func EnvKey(id string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(id) {
		switch {
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// Health asks /health and returns "" when the member reports healthy, else
// the reason.
func Health(client *http.Client, base string) string {
	resp, err := client.Get(base + "/health")
	if err != nil {
		return "health: " + err.Error()
	}
	defer resp.Body.Close()
	var body struct {
		Health string `json:"health"`
		Reason string `json:"reason"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "health: unparseable body: " + err.Error()
	}
	if body.Health != "true" {
		return "health: " + body.Health + " " + body.Reason
	}
	return ""
}

// MaintenanceStatus asks POST /v3/maintenance/status.
func MaintenanceStatus(client *http.Client, base string, timeout time.Duration) (*Status, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v3/maintenance/status", bytes.NewReader([]byte("{}")))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var st Status
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		return nil, err
	}
	return &st, nil
}
