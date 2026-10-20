// Package consulapi is the slice of Consul's HTTP surface the target's
// host-side probes share (cmd/consulrole, cmd/consulsteady), plus the
// PROTHESIS_* environment the harness exports to them, so the two probes
// cannot disagree about either. Standard library only.
//
// Shapes are Consul v2.0.4's (agent/operator_endpoint.go
// OperatorRaftConfiguration, agent/consul/operator_raft_endpoint.go
// RaftGetConfiguration, agent/structs/operator.go RaftServer).
package consulapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
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

// ResolveTargets builds the node id -> base URL map from an explicit
// comma-separated id=host:port list, or from the environment the harness
// exports. Node ids matter: a server's Consul node name is mapped back to one.
func ResolveTargets(flagValue string) (map[string]string, error) {
	out := map[string]string{}
	if flagValue != "" {
		for _, t := range strings.Split(flagValue, ",") {
			t = strings.TrimSpace(t)
			if t == "" {
				continue
			}
			id, addr, ok := strings.Cut(t, "=")
			if !ok || id == "" || addr == "" {
				return nil, fmt.Errorf("target %q: want id=host:port", t)
			}
			if !strings.HasPrefix(addr, "http://") && !strings.HasPrefix(addr, "https://") {
				addr = "http://" + addr
			}
			out[id] = strings.TrimRight(addr, "/")
		}
	} else {
		host := os.Getenv(EnvHost)
		if host == "" {
			host = "127.0.0.1"
		}
		for _, n := range strings.Split(os.Getenv(EnvNodes), ",") {
			n = strings.TrimSpace(n)
			if n == "" {
				continue
			}
			port := os.Getenv(EnvPortPrefix + EnvKey(n))
			if port == "" {
				return nil, fmt.Errorf("no %s%s in the environment for node %q (is this running under the harness?)",
					EnvPortPrefix, EnvKey(n), n)
			}
			out[n] = "http://" + host + ":" + port
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no targets: pass --targets or run under the harness, which exports %s", EnvNodes)
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

// SortedIDs returns the map's node ids in order.
func SortedIDs(urls map[string]string) []string {
	ids := make([]string, 0, len(urls))
	for id := range urls {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// Server is one entry of GET /v1/operator/raft/configuration.
type Server struct {
	ID              string
	Node            string
	Address         string
	Leader          bool
	ProtocolVersion string
	Voter           bool
	LastIndex       uint64
}

// FetchConfiguration asks one node for ITS OWN view of the Raft
// configuration. ?stale is what makes it this node's view: without it the
// request is forwarded to the leader (agent/consul/rpc.go forwardRPC), every
// node answers with the leader's view, and a disagreement is hidden. A node
// that has never heard from a leader forwards even a stale read, and with no
// leader holds it for up to rpc_hold_timeout (7 s); the context timeout bounds
// that.
func FetchConfiguration(client *http.Client, base string, timeout time.Duration) ([]Server, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v1/operator/raft/configuration?stale", nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("HTTP %d %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var doc struct {
		Servers *[]Server
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return nil, fmt.Errorf("unparseable raft configuration body: %w", err)
	}
	if doc.Servers == nil {
		return nil, fmt.Errorf("raft configuration body has no \"Servers\" list")
	}
	return *doc.Servers, nil
}

// NodeIDForName maps a server's Consul node name to a harness node id. It
// must be one of the ids, which holds because the compose file starts each
// server with -node=<its service name> and the service names are the node
// ids. "(unknown)" (Serf does not know the address) maps to nothing.
func NodeIDForName(name string, ids []string) (string, error) {
	for _, id := range ids {
		if name == id {
			return id, nil
		}
	}
	return "", fmt.Errorf("server node name %q names no node of this world (%s)", name, strings.Join(ids, ", "))
}

// View is what one node said about who leads.
type View struct {
	Observer string
	Leader   string // node id, or "" when this node names no leader
	Servers  []Server
	// FetchErr: the node could not be asked. It is unobserved, not wrong.
	FetchErr error
	// ContentErr: the node answered something that cannot be one view (two
	// leaders in one list, a leader with an unknown name). It refuses the
	// whole observation.
	ContentErr error
}

// Observe asks every node, in id order.
func Observe(client *http.Client, urls map[string]string, timeout time.Duration) []View {
	ids := SortedIDs(urls)
	views := make([]View, 0, len(ids))
	for _, id := range ids {
		v := View{Observer: id}
		servers, err := FetchConfiguration(client, urls[id], timeout)
		if err != nil {
			v.FetchErr = err
			views = append(views, v)
			continue
		}
		v.Servers = servers
		var leaders []string
		for _, s := range servers {
			if !s.Leader {
				continue
			}
			lid, err := NodeIDForName(s.Node, ids)
			if err != nil {
				v.ContentErr = fmt.Errorf("%s reports leader %s: %w", id, s.Address, err)
				break
			}
			leaders = append(leaders, lid)
		}
		if v.ContentErr == nil {
			switch len(leaders) {
			case 0:
			case 1:
				v.Leader = leaders[0]
			default:
				v.ContentErr = fmt.Errorf("%s lists %d leaders: %s", id, len(leaders), strings.Join(leaders, ", "))
			}
		}
		views = append(views, v)
	}
	return views
}

// Agree returns the one leader every node that answered names, or a refusal:
// no node answered, an answer could not be read as a view, the nodes name
// different leaders, or none names one. The configuration carries no Raft
// term, so two claims cannot be ranked; the only safe answer to a
// disagreement is none.
func Agree(views []View) (string, error) {
	var answered []View
	for _, v := range views {
		if v.ContentErr != nil {
			return "", v.ContentErr
		}
		if v.FetchErr == nil {
			answered = append(answered, v)
		}
	}
	if len(answered) == 0 {
		var why []string
		for _, v := range views {
			why = append(why, v.Observer+": "+v.FetchErr.Error())
		}
		return "", fmt.Errorf("no node answered /v1/operator/raft/configuration: %s", strings.Join(why, "; "))
	}
	leader := answered[0].Leader
	for _, v := range answered[1:] {
		if v.Leader != leader {
			return "", fmt.Errorf("the nodes disagree about the leader: %s", describe(answered))
		}
	}
	if leader == "" {
		return "", fmt.Errorf("no node names a leader: %s", describe(answered))
	}
	return leader, nil
}

func describe(views []View) string {
	parts := make([]string, 0, len(views))
	for _, v := range views {
		l := v.Leader
		if l == "" {
			l = "none"
		}
		parts = append(parts, v.Observer+" says "+l)
	}
	return strings.Join(parts, "; ")
}
