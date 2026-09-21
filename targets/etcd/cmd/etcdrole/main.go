// Command etcdrole is the etcd target's `harness.role_probe`: it tells the
// harness which member currently leads, so `role:leader` and `role:follower`
// targets can bind on a system that has no /status document.
//
// It runs on the host under the PROTHESIS_* environment the harness exports
// and prints one JSON object per member, in node order:
//
//	{"node":"etcd-n1","role":"follower","term":7}
//	{"node":"etcd-n2","role":"leader","term":7}
//	{"node":"etcd-n3","error":"reports no leader"}
//
// A member is the leader iff its own member id is the id it reports as
// leader; a follower iff it reports some other non-zero id; and an error line
// (never a guessed role) when it reports no leader or cannot be reached.
// The harness treats an error line as "unobserved", which is what a member
// cut from its peers is: it may still be printing the leader it last knew.
// The term is raftTerm, and the harness prefers the highest term when two
// members disagree, the standard Raft tie-break.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"time"

	"github.com/WrdCstlg/pro-thesis/targets/etcd/internal/etcdapi"
)

type line struct {
	Node  string `json:"node"`
	Role  string `json:"role,omitempty"`
	Term  uint64 `json:"term,omitempty"`
	Error string `json:"error,omitempty"`
}

func main() {
	targets := flag.String("targets", "", "comma-separated host:port list; defaults to the PROTHESIS_* environment")
	timeout := flag.Duration("timeout", 2*time.Second, "per-member request timeout")
	flag.Parse()
	urls, err := etcdapi.ResolveTargets(*targets)
	if err != nil {
		fmt.Fprintf(os.Stderr, "etcdrole: %v\n", err)
		os.Exit(1)
	}
	client := &http.Client{Timeout: *timeout}
	if err := run(client, urls, *timeout, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "etcdrole: %v\n", err)
		os.Exit(1)
	}
}

// run observes every member and writes the lines. Per-member failures are
// error LINES, not a failed run: the harness needs to know which members
// could not be asked, and one unreachable member must not hide the others.
func run(client *http.Client, urls map[string]string, timeout time.Duration, w io.Writer) error {
	nodes := make([]string, 0, len(urls))
	for n := range urls {
		nodes = append(nodes, n)
	}
	sort.Strings(nodes)
	enc := json.NewEncoder(w)
	for _, node := range nodes {
		if err := enc.Encode(observe(client, node, urls[node], timeout)); err != nil {
			return err
		}
	}
	return nil
}

func observe(client *http.Client, node, base string, timeout time.Duration) line {
	st, err := etcdapi.MaintenanceStatus(client, base, timeout)
	if err != nil {
		return line{Node: node, Error: err.Error()}
	}
	term, _ := strconv.ParseUint(st.RaftTerm, 10, 64)
	switch {
	case st.Leader == "" || st.Leader == "0":
		return line{Node: node, Error: "reports no leader"}
	case st.Leader == st.Header.MemberID:
		return line{Node: node, Role: "leader", Term: term}
	default:
		return line{Node: node, Role: "follower", Term: term}
	}
}
