// Command redisrole is the Redis target's harness.role_probe: it tells the
// harness which node is currently primary, so role:leader and role:follower
// targets can bind on a system that has no fixture-shaped /status document.
//
// It runs on the host. It asks every node INFO server and INFO replication,
// and every Sentinel `SENTINEL get-master-addr-by-name` and `SENTINEL master`,
// maps each Sentinel's answer to a node by runid (internal/redisapi explains
// why), and prints one JSON object per node, in node order:
//
//	{"node":"redis-n1","role":"leader"}
//	{"node":"redis-n2","role":"follower"}
//	{"node":"redis-n3","error":"dial tcp 127.0.0.1:16381: connect: connection refused"}
//
// The primary is "leader" and a replica "follower", the vocabulary the fault
// grammar's role: targets use. A node that cannot be asked is an error line
// (the harness treats it as unobserved), never a guessed role. It REFUSES,
// exiting 1 with the reason and printing no role line, when two nodes report
// role:master, when the Sentinels that answered name different primaries, when
// the primary they name is not the one node reporting role:master, when no
// Sentinel answered, or when a Sentinel names a runid no node reports. A split
// produces exactly these disagreements, and a fault sent to the wrong node
// would be misattributed.
//
// The addresses come from the command line (harness.role_probe, lock-covered),
// not from the PROTHESIS_* environment: on this target that environment
// carries the health shims' ports (README, "Contract gaps").
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/WrdCstlg/pro-thesis/targets/redis/internal/redisapi"
)

type line struct {
	Node  string `json:"node"`
	Role  string `json:"role,omitempty"`
	Error string `json:"error,omitempty"`
}

func main() {
	nodesFlag := flag.String("nodes", "", "node RESP addresses, id=host:port,... (required)")
	sentinelsFlag := flag.String("sentinels", "", "Sentinel addresses, <co-located node id>=host:port,... (required)")
	name := flag.String("master-name", "mymaster", "the name Sentinel monitors the primary under")
	timeout := flag.Duration("timeout", 2*time.Second, "per-server timeout, dial included")
	flag.Parse()
	nodes, err := redisapi.ParseTargets(*nodesFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "redisrole: --nodes: %v\n", err)
		os.Exit(1)
	}
	sentinels, err := redisapi.ParseTargets(*sentinelsFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "redisrole: --sentinels: %v\n", err)
		os.Exit(1)
	}
	if err := run(nodes, sentinels, *name, *timeout, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "redisrole: refusing: %v\n", err)
		os.Exit(1)
	}
}

// run observes every node and Sentinel, decides, and only then writes: a
// refusal prints nothing on stdout.
func run(nodes, sentinels []redisapi.Target, name string, timeout time.Duration, w io.Writer) error {
	obs := redisapi.Observe(nodes, sentinels, name, timeout)
	primary, err := redisapi.Agree(obs)
	if err != nil {
		return err
	}
	lines := make([]line, 0, len(obs.Nodes))
	for _, n := range obs.Nodes {
		switch {
		case n.Err != nil:
			lines = append(lines, line{Node: n.ID, Error: n.Err.Error()})
		case n.ID == primary:
			lines = append(lines, line{Node: n.ID, Role: "leader"})
		default:
			lines = append(lines, line{Node: n.ID, Role: "follower"})
		}
	}
	enc := json.NewEncoder(w)
	for _, l := range lines {
		if err := enc.Encode(l); err != nil {
			return err
		}
	}
	return nil
}
