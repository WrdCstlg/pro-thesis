// Command redissteady is the steady-state probe for the Redis target.
//
// It runs on the HOST, with the node and Sentinel addresses on its command
// line (harness.steady_state.probe), not from the PROTHESIS_* environment the
// harness exports (internal/control/steady.go): on this target that
// environment carries the health shims' ports (README, "Contract gaps").
//
// Steady means, in one observation:
//
//   - every node answers INFO, exactly one reports role:master, and every
//     Sentinel that answers names that node (redisapi.Agree, the role probe's
//     rule);
//   - every node and every Sentinel answered;
//   - the primary reports connected_slaves equal to the other nodes, each
//     slaveN line with state=online, and every replica reports
//     master_link_status:up;
//   - every Sentinel reports flags exactly "master" (no s_down or o_down),
//     num-slaves equal to the replicas and num-other-sentinels equal to the
//     other Sentinels, so each knows the whole topology.
//
// Exit 0 when that holds before the deadline, 1 with the last reason
// otherwise.
package main

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/WrdCstlg/pro-thesis/targets/redis/internal/redisapi"
)

func main() {
	var (
		timeout       = flag.Duration("timeout", 55*time.Second, "give up after this long")
		reqTO         = flag.Duration("request-timeout", 2*time.Second, "per-server timeout, dial included")
		interval      = flag.Duration("interval", 500*time.Millisecond, "time between attempts")
		nodesFlag     = flag.String("nodes", "", "node RESP addresses, id=host:port,... (required)")
		sentinelsFlag = flag.String("sentinels", "", "Sentinel addresses, <co-located node id>=host:port,... (required)")
		name          = flag.String("master-name", "mymaster", "the name Sentinel monitors the primary under")
	)
	flag.Parse()

	nodes, err := redisapi.ParseTargets(*nodesFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "redissteady: --nodes: %v\n", err)
		os.Exit(1)
	}
	sentinels, err := redisapi.ParseTargets(*sentinelsFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "redissteady: --sentinels: %v\n", err)
		os.Exit(1)
	}
	deadline := time.Now().Add(*timeout)
	var last string
	for {
		primary, quorum, reason := check(nodes, sentinels, *name, *reqTO)
		if reason == "" {
			fmt.Printf("redissteady: steady across %d node(s), primary %s, %d Sentinel(s) agree, quorum %s\n",
				len(nodes), primary, len(sentinels), quorum)
			os.Exit(0)
		}
		last = reason
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(*interval)
	}
	fmt.Fprintf(os.Stderr, "redissteady: not steady within %s: %s\n", *timeout, last)
	os.Exit(1)
}

// check returns the agreed primary, the quorum and "" when steady, else the
// first reason it is not.
func check(nodes, sentinels []redisapi.Target, name string, reqTO time.Duration) (string, string, string) {
	obs := redisapi.Observe(nodes, sentinels, name, reqTO)
	for _, n := range obs.Nodes {
		if n.Err != nil {
			return "", "", n.ID + ": " + n.Err.Error()
		}
	}
	for _, s := range obs.Sentinels {
		if s.FetchErr != nil {
			return "", "", "the Sentinel at " + s.Sentinel + ": " + s.FetchErr.Error()
		}
	}
	primary, err := redisapi.Agree(obs)
	if err != nil {
		return "", "", err.Error()
	}
	replicas := len(nodes) - 1
	for _, n := range obs.Nodes {
		if n.ID == primary {
			if got := n.Repl["connected_slaves"]; got != strconv.Itoa(replicas) {
				return "", "", fmt.Sprintf("%s reports connected_slaves %q, want %d", n.ID, got, replicas)
			}
			for i := 0; i < replicas; i++ {
				if l := n.Repl["slave"+strconv.Itoa(i)]; !strings.Contains(l, "state=online") {
					return "", "", fmt.Sprintf("%s reports slave%d %q, not online", n.ID, i, l)
				}
			}
			continue
		}
		if got := n.Repl["master_link_status"]; got != "up" {
			return "", "", fmt.Sprintf("%s reports master_link_status %q", n.ID, got)
		}
	}
	quorum := ""
	for _, s := range obs.Sentinels {
		f := s.Master.Fields
		switch {
		case f["flags"] != "master":
			return "", "", fmt.Sprintf("the Sentinel at %s reports the primary's flags %q", s.Sentinel, f["flags"])
		case f["num-slaves"] != strconv.Itoa(replicas):
			return "", "", fmt.Sprintf("the Sentinel at %s knows %s replica(s), want %d", s.Sentinel, f["num-slaves"], replicas)
		case f["num-other-sentinels"] != strconv.Itoa(len(sentinels)-1):
			return "", "", fmt.Sprintf("the Sentinel at %s knows %s other Sentinel(s), want %d", s.Sentinel, f["num-other-sentinels"], len(sentinels)-1)
		}
		quorum = f["quorum"]
	}
	return primary, quorum, ""
}
