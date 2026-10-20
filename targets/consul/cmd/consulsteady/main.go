// Command consulsteady is the steady-state probe for the Consul target.
//
// It runs on the HOST, under the PROTHESIS_* environment the harness exports
// (internal/control/steady.go): PROTHESIS_NODES, PROTHESIS_HOST and each
// node's PROTHESIS_PORT_<NODE>.
//
// Steady means, on every node at once:
//
//   - GET /v1/operator/raft/configuration?stale answers (the node's own view,
//     served locally);
//   - every node names the SAME leader, mapped from its Consul node name to a
//     node id;
//   - every node's configuration lists exactly the world's nodes, each once,
//     each a voter (three voters for this target).
//
// Exit 0 when that holds before the deadline, 1 with the last reason
// otherwise.
package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/WrdCstlg/pro-thesis/targets/consul/internal/consulapi"
)

func main() {
	var (
		timeout  = flag.Duration("timeout", 55*time.Second, "give up after this long")
		reqTO    = flag.Duration("request-timeout", 2*time.Second, "per-request timeout")
		interval = flag.Duration("interval", 500*time.Millisecond, "time between attempts")
		targets  = flag.String("targets", "", "comma-separated id=host:port list; defaults to the PROTHESIS_* environment")
	)
	flag.Parse()

	urls, err := consulapi.ResolveTargets(*targets)
	if err != nil {
		fmt.Fprintf(os.Stderr, "consulsteady: %v\n", err)
		os.Exit(1)
	}
	client := &http.Client{Timeout: *reqTO + time.Second}
	deadline := time.Now().Add(*timeout)
	var last string
	for {
		leader, reason := check(client, urls, *reqTO)
		if reason == "" {
			fmt.Printf("consulsteady: steady across %d node(s), %d voters, leader %s\n", len(urls), len(urls), leader)
			os.Exit(0)
		}
		last = reason
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(*interval)
	}
	fmt.Fprintf(os.Stderr, "consulsteady: not steady within %s: %s\n", *timeout, last)
	os.Exit(1)
}

// check returns the agreed leader and "" when the cluster is steady, else the
// first reason it is not.
func check(client *http.Client, urls map[string]string, reqTO time.Duration) (string, string) {
	ids := consulapi.SortedIDs(urls)
	views := consulapi.Observe(client, urls, reqTO)
	var unasked []string
	for _, v := range views {
		if v.FetchErr != nil {
			unasked = append(unasked, v.Observer+": "+v.FetchErr.Error())
		}
	}
	if len(unasked) > 0 {
		return "", "nodes: " + strings.Join(unasked, "; ")
	}
	leader, err := consulapi.Agree(views)
	if err != nil {
		return "", err.Error()
	}
	for _, v := range views {
		seen := map[string]bool{}
		for _, s := range v.Servers {
			id, err := consulapi.NodeIDForName(s.Node, ids)
			if err != nil {
				return "", v.Observer + ": " + err.Error()
			}
			if seen[id] {
				return "", fmt.Sprintf("%s lists %s twice", v.Observer, id)
			}
			seen[id] = true
			if !s.Voter {
				return "", fmt.Sprintf("%s lists %s as a non-voter", v.Observer, id)
			}
		}
		if len(seen) != len(ids) {
			return "", fmt.Sprintf("%s lists %d voter(s), want %d", v.Observer, len(seen), len(ids))
		}
	}
	return leader, ""
}
