// Command rqlitesteady is the steady-state probe for the rqlite target.
//
// It runs on the HOST, under the PROTHESIS_* environment the harness exports
// (internal/control/steady.go): PROTHESIS_NODES, PROTHESIS_HOST and each
// node's PROTHESIS_PORT_<NODE>.
//
// Steady means, on every node at once:
//
//   - GET /readyz?timeout=<t> answers 200 (the node knows a leader it can
//     contact, its store is ready, and a local SELECT 1 works);
//   - GET /nodes?ver=2 from every node names the SAME leader, mapped from its
//     api_addr to a node id, and every node answered.
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

	"github.com/WrdCstlg/pro-thesis/targets/rqlite/internal/rqliteapi"
)

func main() {
	var (
		timeout  = flag.Duration("timeout", 55*time.Second, "give up after this long")
		reqTO    = flag.Duration("request-timeout", 2*time.Second, "per-request timeout, also sent to rqlite as ?timeout=")
		interval = flag.Duration("interval", 500*time.Millisecond, "time between attempts")
		targets  = flag.String("targets", "", "comma-separated id=host:port list; defaults to the PROTHESIS_* environment")
	)
	flag.Parse()

	urls, err := rqliteapi.ResolveTargets(*targets)
	if err != nil {
		fmt.Fprintf(os.Stderr, "rqlitesteady: %v\n", err)
		os.Exit(1)
	}
	client := &http.Client{Timeout: *reqTO + time.Second}
	deadline := time.Now().Add(*timeout)
	var last string
	for {
		leader, reason := check(client, urls, *reqTO)
		if reason == "" {
			fmt.Printf("rqlitesteady: steady across %d node(s), leader %s\n", len(urls), leader)
			os.Exit(0)
		}
		last = reason
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(*interval)
	}
	fmt.Fprintf(os.Stderr, "rqlitesteady: not steady within %s: %s\n", *timeout, last)
	os.Exit(1)
}

// check returns the agreed leader and "" when the cluster is steady, else the
// first reason it is not.
func check(client *http.Client, urls map[string]string, reqTO time.Duration) (string, string) {
	for _, id := range rqliteapi.SortedIDs(urls) {
		if reason := rqliteapi.Readyz(client, urls[id], reqTO); reason != "" {
			return "", id + ": " + reason
		}
	}
	views := rqliteapi.Observe(client, urls, reqTO)
	var unasked []string
	for _, v := range views {
		if v.FetchErr != nil {
			unasked = append(unasked, v.Observer+": "+v.FetchErr.Error())
		}
	}
	if len(unasked) > 0 {
		return "", "nodes: " + strings.Join(unasked, "; ")
	}
	leader, err := rqliteapi.Agree(views)
	if err != nil {
		return "", err.Error()
	}
	return leader, ""
}
