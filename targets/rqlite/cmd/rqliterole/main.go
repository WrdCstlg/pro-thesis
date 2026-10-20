// Command rqliterole is the rqlite target's harness.role_probe: it tells the
// harness which node currently leads, so role:leader and role:follower
// targets can bind on a system that has no fixture-shaped /status document.
//
// It runs on the host under the PROTHESIS_* environment the harness exports,
// asks every node GET /nodes?ver=2 with an explicit timeout, maps each
// reported leader's api_addr to a harness node id, and prints one JSON object
// per node, in node order:
//
//	{"node":"rq-n1","role":"follower"}
//	{"node":"rq-n2","role":"leader"}
//	{"node":"rq-n3","error":"Get \"http://127.0.0.1:14003/nodes?...\": ..."}
//
// A node that cannot be asked is an error line (the harness treats it as
// unobserved), never a guessed role. It REFUSES, exiting 1 with the reason and
// printing no role line, when the nodes that answered disagree about the
// leader, when none names one, or when an answer cannot be read as one view:
// /nodes carries no Raft term, so two claims of leadership cannot be ranked,
// and a fault sent to the wrong node would be misattributed.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/WrdCstlg/pro-thesis/targets/rqlite/internal/rqliteapi"
)

type line struct {
	Node  string `json:"node"`
	Role  string `json:"role,omitempty"`
	Error string `json:"error,omitempty"`
}

func main() {
	targets := flag.String("targets", "", "comma-separated id=host:port list; defaults to the PROTHESIS_* environment")
	timeout := flag.Duration("timeout", 2*time.Second, "per-node request timeout, also sent as /nodes?timeout=")
	flag.Parse()
	urls, err := rqliteapi.ResolveTargets(*targets)
	if err != nil {
		fmt.Fprintf(os.Stderr, "rqliterole: %v\n", err)
		os.Exit(1)
	}
	client := &http.Client{Timeout: *timeout + time.Second}
	if err := run(client, urls, *timeout, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "rqliterole: refusing: %v\n", err)
		os.Exit(1)
	}
}

// run observes every node, decides, and only then writes: a refusal prints
// nothing on stdout.
func run(client *http.Client, urls map[string]string, timeout time.Duration, w io.Writer) error {
	views := rqliteapi.Observe(client, urls, timeout)
	leader, err := rqliteapi.Agree(views)
	if err != nil {
		return err
	}
	lines := make([]line, 0, len(views))
	for _, v := range views {
		switch {
		case v.FetchErr != nil:
			lines = append(lines, line{Node: v.Observer, Error: v.FetchErr.Error()})
		case v.Observer == leader:
			lines = append(lines, line{Node: v.Observer, Role: "leader"})
		default:
			lines = append(lines, line{Node: v.Observer, Role: "follower"})
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
