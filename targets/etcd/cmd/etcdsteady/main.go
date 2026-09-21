// Command etcdsteady is the steady-state probe for the etcd target.
//
// It runs on the HOST, because the etcd image ships no shell and the
// fixture's `docker compose exec ... /kv steady` shape is therefore
// unavailable. The harness exports the topology into the probe's environment
// (internal/control/steady.go): PROTHESIS_NODES is the comma-separated node
// list, PROTHESIS_HOST the address to probe, and PROTHESIS_PORT_<NODE> each
// node's published client port, so the probe needs no compose access at all.
//
// Steady means, on every node at once:
//
//   - /health answers "true";
//   - /v3/maintenance/status names a leader, the SAME leader everywhere;
//   - raftTerm is the same everywhere;
//   - raftIndex is within a small band across nodes (a lagging follower is
//     not steady);
//   - raftIndex > 0.
//
// Exit 0 when that holds before the deadline, 1 with the last reason
// otherwise. A steady-state failure is the failure most likely to be the
// system under test's own, so the reason is printed rather than swallowed.
package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/WrdCstlg/pro-thesis/targets/etcd/internal/etcdapi"
)

func main() {
	var (
		timeout  = flag.Duration("timeout", 55*time.Second, "give up after this long")
		interval = flag.Duration("interval", 500*time.Millisecond, "time between attempts")
		indexGap = flag.Int64("max-index-gap", 5, "largest raftIndex spread across nodes still called steady")
		targets  = flag.String("targets", "", "comma-separated host:port list; defaults to the PROTHESIS_* environment")
	)
	flag.Parse()

	urls, err := etcdapi.ResolveTargets(*targets)
	if err != nil {
		fmt.Fprintf(os.Stderr, "etcdsteady: %v\n", err)
		os.Exit(1)
	}

	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(*timeout)
	var last string
	for {
		reason := check(client, urls, *indexGap)
		if reason == "" {
			fmt.Printf("etcdsteady: steady across %d node(s)\n", len(urls))
			os.Exit(0)
		}
		last = reason
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(*interval)
	}
	fmt.Fprintf(os.Stderr, "etcdsteady: not steady within %s: %s\n", *timeout, last)
	os.Exit(1)
}

// check returns "" when the cluster is steady, else the first reason it is
// not.
func check(client *http.Client, urls map[string]string, maxGap int64) string {
	var (
		leader   string
		term     string
		minIndex int64 = -1
		maxIndex int64 = -1
	)
	for node, base := range urls {
		if reason := etcdapi.Health(client, base); reason != "" {
			return node + ": " + reason
		}
		st, err := etcdapi.MaintenanceStatus(client, base, 2*time.Second)
		if err != nil {
			return node + ": status: " + err.Error()
		}
		if st.Leader == "" || st.Leader == "0" {
			return node + ": reports no leader"
		}
		if leader == "" {
			leader = st.Leader
		} else if st.Leader != leader {
			return node + ": leader " + st.Leader + " disagrees with " + leader
		}
		if term == "" {
			term = st.RaftTerm
		} else if st.RaftTerm != term {
			return node + ": term " + st.RaftTerm + " disagrees with " + term
		}
		idx, err := strconv.ParseInt(st.RaftIndex, 10, 64)
		if err != nil {
			return node + ": raftIndex " + st.RaftIndex + " is not a number"
		}
		if idx <= 0 {
			return node + ": raftIndex is 0"
		}
		if minIndex < 0 || idx < minIndex {
			minIndex = idx
		}
		if idx > maxIndex {
			maxIndex = idx
		}
	}
	if maxIndex-minIndex > maxGap {
		return fmt.Sprintf("raftIndex spread %d exceeds %d (a follower is lagging)", maxIndex-minIndex, maxGap)
	}
	return ""
}
