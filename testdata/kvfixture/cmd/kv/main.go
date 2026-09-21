// Command kv is the fixture node binary.
//
//	kv serve                     run a node
//	kv healthcheck --url URL     liveness probe (so the image needs no curl)
//	kv steady --targets A,B,C    cluster-wide steady-state probe
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"prothesis.dev/kvfixture/internal/client"
	"prothesis.dev/kvfixture/internal/kv"
	"prothesis.dev/kvfixture/internal/raft"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = runServe(os.Args[2:])
	case "healthcheck":
		err = runHealthcheck(os.Args[2:])
	case "steady":
		err = runSteady(os.Args[2:])
	case "version":
		fmt.Printf("kvfixture variant=%s lease_ms=%d\n", raft.Variant, raft.LeaseDuration.Milliseconds())
		return
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "kv: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `usage:
  kv serve
  kv healthcheck --url http://127.0.0.1:8080/healthz
  kv steady --targets http://kv-n1:8080,http://kv-n2:8080,http://kv-n3:8080 [--timeout 60s]
  kv version
`)
}

// ------------------------------------------------------------------ env config

func envStr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envMS(key string, def time.Duration) (time.Duration, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer number of milliseconds, got %q", key, v)
	}
	return time.Duration(n) * time.Millisecond, nil
}

func envU64(key string, def uint64) (uint64, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.ParseUint(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s must be an unsigned integer, got %q", key, v)
	}
	return n, nil
}

// parsePeers parses "kv-n2=http://n2-peer:9090,kv-n3=http://n3-peer:9090".
func parsePeers(s string) (map[string]string, error) {
	out := map[string]string{}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		id, url, ok := strings.Cut(part, "=")
		if !ok || id == "" || url == "" {
			return nil, fmt.Errorf("malformed KV_PEERS entry %q, want ID=URL", part)
		}
		out[strings.TrimSpace(id)] = strings.TrimSpace(url)
	}
	if len(out) == 0 {
		return nil, errors.New("KV_PEERS is empty")
	}
	return out, nil
}

// ----------------------------------------------------------------------- serve

func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}

	nodeID := envStr("KV_NODE_ID", "")
	if nodeID == "" {
		return errors.New("KV_NODE_ID is required")
	}
	peers, err := parsePeers(envStr("KV_PEERS", ""))
	if err != nil {
		return err
	}
	seed, err := envU64("KV_SEED", 1)
	if err != nil {
		return err
	}
	tick, err := envMS("KV_TICK_MS", 25*time.Millisecond)
	if err != nil {
		return err
	}
	hb, err := envMS("KV_HEARTBEAT_MS", 100*time.Millisecond)
	if err != nil {
		return err
	}
	emin, err := envMS("KV_ELECTION_MIN_MS", 600*time.Millisecond)
	if err != nil {
		return err
	}
	emax, err := envMS("KV_ELECTION_MAX_MS", 900*time.Millisecond)
	if err != nil {
		return err
	}
	rpc, err := envMS("KV_RPC_TIMEOUT_MS", 250*time.Millisecond)
	if err != nil {
		return err
	}
	applyWait, err := envMS("KV_APPLY_WAIT_MS", 2000*time.Millisecond)
	if err != nil {
		return err
	}

	clientAddr := envStr("KV_CLIENT_ADDR", "0.0.0.0:8080")
	peerAddr := envStr("KV_PEER_ADDR", "0.0.0.0:9090")
	dataDir := envStr("KV_DATA_DIR", "")
	roleHint := envStr("KV_ROLE_HINT", "replica")

	logger := log.New(os.Stdout, "", log.LstdFlags|log.Lmicroseconds|log.LUTC)

	peerIDs := make([]string, 0, len(peers))
	for id := range peers {
		peerIDs = append(peerIDs, id)
	}
	sort.Strings(peerIDs)

	cfg := raft.Config{
		ID:               nodeID,
		Peers:            peerIDs,
		Seed:             seed,
		TickInterval:     tick,
		HeartbeatPeriod:  hb,
		ElectionMin:      emin,
		ElectionMax:      emax,
		RPCTimeout:       rpc,
		MaxEntriesPerRPC: 256,
	}

	var store raft.Storage
	if dataDir == "" {
		store = raft.NewMemStorage()
	} else {
		fstore, err := raft.NewFileStorage(dataDir)
		if err != nil {
			return err
		}
		store = fstore
	}

	sm := kv.NewStateMachine()
	tr := raft.NewHTTPTransport(nodeID, peers, rpc)
	node, err := raft.New(cfg, tr, sm, store, logger)
	if err != nil {
		return err
	}

	srv := kv.NewServer(kv.Options{
		NodeID:    nodeID,
		RoleHint:  roleHint,
		ApplyWait: applyWait,
		Logger:    logger,
	}, node, sm)

	logger.Printf("level=info event=boot node=%s variant=%s lease_ms=%d tick_ms=%d hb_ms=%d election_ms=%d-%d peers=%v client=%s peer=%s data_dir=%q seed=%d node_seed=%d",
		nodeID, raft.Variant, raft.LeaseDuration.Milliseconds(),
		tick.Milliseconds(), hb.Milliseconds(), emin.Milliseconds(), emax.Milliseconds(),
		peerIDs, clientAddr, peerAddr, dataDir, seed, raft.DeriveNodeSeed(seed, nodeID))

	clientSrv := &http.Server{
		Addr:              clientAddr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ConnState:         kv.ConnStateHook(srv.TrackClientConn),
		ErrorLog:          log.New(os.Stderr, "clientsrv ", log.LstdFlags),
	}
	peerSrv := &http.Server{
		Addr:              peerAddr,
		Handler:           raft.PeerHandler(node),
		ReadHeaderTimeout: 5 * time.Second,
		ConnState:         kv.ConnStateHook(srv.TrackPeerConn),
		ErrorLog:          log.New(os.Stderr, "peersrv ", log.LstdFlags),
	}

	clientLn, err := net.Listen("tcp", clientAddr)
	if err != nil {
		return fmt.Errorf("listen client plane: %w", err)
	}
	peerLn, err := net.Listen("tcp", peerAddr)
	if err != nil {
		clientLn.Close()
		return fmt.Errorf("listen peer plane: %w", err)
	}

	errCh := make(chan error, 2)
	go func() { errCh <- clientSrv.Serve(clientLn) }()
	go func() { errCh <- peerSrv.Serve(peerLn) }()

	node.Start()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

	select {
	case sig := <-sigCh:
		logger.Printf("level=info event=shutdown node=%s signal=%v", nodeID, sig)
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Printf("level=error event=serve_failed node=%s err=%q", nodeID, err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = clientSrv.Shutdown(ctx)
	_ = peerSrv.Shutdown(ctx)
	node.Stop()
	_ = tr.Close()
	logger.Printf("level=info event=stopped node=%s", nodeID)
	return nil
}

// ----------------------------------------------------------------- healthcheck

func runHealthcheck(args []string) error {
	fs := flag.NewFlagSet("healthcheck", flag.ContinueOnError)
	url := fs.String("url", "http://127.0.0.1:8080/healthz", "health endpoint")
	timeout := fs.Duration("timeout", 2*time.Second, "request timeout")
	if err := fs.Parse(args); err != nil {
		return err
	}
	c := client.New(*timeout, 2)
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	var out kv.HealthResponse
	res := c.Do(ctx, http.MethodGet, *url, nil, &out)
	if res.Attempt.Err != nil {
		return res.Attempt.Err
	}
	if res.Attempt.Status != http.StatusOK || !out.OK {
		return fmt.Errorf("unhealthy: status=%d body=%s", res.Attempt.Status, string(res.Body))
	}
	return nil
}

// ---------------------------------------------------------------------- steady

func runSteady(args []string) error {
	fs := flag.NewFlagSet("steady", flag.ContinueOnError)
	targets := fs.String("targets", "", "comma-separated client-plane base URLs")
	timeout := fs.Duration("timeout", 60*time.Second, "overall deadline")
	interval := fs.Duration("interval", 250*time.Millisecond, "poll interval")
	verbose := fs.Bool("v", false, "print each failed check")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *targets == "" {
		return errors.New("--targets is required")
	}
	urls := strings.Split(*targets, ",")
	for i := range urls {
		urls[i] = strings.TrimRight(strings.TrimSpace(urls[i]), "/")
	}

	c := client.New(2*time.Second, len(urls)+2)
	deadline := time.Now().Add(*timeout)
	var last string
	for time.Now().Before(deadline) {
		ok, reason, statuses := steadyOnce(c, urls)
		if ok {
			b, _ := json.Marshal(statuses)
			fmt.Printf("steady: ok %s\n", b)
			return nil
		}
		last = reason
		if *verbose {
			fmt.Fprintf(os.Stderr, "steady: not yet: %s\n", reason)
		}
		time.Sleep(*interval)
	}
	return fmt.Errorf("steady state not reached within %s: %s", *timeout, last)
}

// steadyOnce is the real steady-state condition: every node reachable, EXACTLY
// one leader, unanimous agreement on term and leader identity, and a non-zero
// commit index. No single-URL probe can express that.
func steadyOnce(c *client.Client, urls []string) (bool, string, []kv.Status) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	statuses := make([]kv.Status, 0, len(urls))
	for _, u := range urls {
		var st kv.Status
		res := c.Do(ctx, http.MethodGet, u+"/status", nil, &st)
		if res.Attempt.Err != nil {
			return false, fmt.Sprintf("%s unreachable: %v", u, res.Attempt.Err), nil
		}
		if res.Attempt.Status != http.StatusOK {
			return false, fmt.Sprintf("%s status=%d", u, res.Attempt.Status), nil
		}
		statuses = append(statuses, st)
	}

	leaders := 0
	var term uint64
	var leaderID string
	for i, st := range statuses {
		if st.Role == "leader" {
			leaders++
			leaderID = st.Node
		}
		if i == 0 {
			term = st.Term
		} else if st.Term != term {
			return false, fmt.Sprintf("term disagreement: %d != %d", st.Term, term), nil
		}
		if st.CommitIndex == 0 {
			return false, fmt.Sprintf("%s commit_index=0", st.Node), nil
		}
	}
	if leaders != 1 {
		return false, fmt.Sprintf("expected exactly one leader, found %d", leaders), nil
	}
	for _, st := range statuses {
		if st.LeaderID != leaderID {
			return false, fmt.Sprintf("%s reports leader %q, expected %q", st.Node, st.LeaderID, leaderID), nil
		}
	}
	return true, "", statuses
}
