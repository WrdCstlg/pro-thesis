package raft

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

// Transport carries the two peer RPCs. Two implementations exist: HTTPTransport
// for the real cluster, and an in-process transport in internal/netsim for
// tests and for the bug demonstration.
type Transport interface {
	AppendEntries(ctx context.Context, peer string, req AppendReq) (AppendResp, error)
	RequestVote(ctx context.Context, peer string, req VoteReq) (VoteResp, error)
	Close() error
}

// Peer-plane routes. These are served on a separate listener from the client
// plane so that a peer-plane partition leaves the node fully reachable by
// clients and by health probes -- which is what makes the injected defect
// observable at all. A fault that also cut the client plane would model a dead
// node, not a gray failure.
const (
	PathAppend = "/raft/append"
	PathVote   = "/raft/vote"
)

// HTTPTransport dials peers over HTTP/JSON.
type HTTPTransport struct {
	self    string
	addrs   map[string]string // peer ID -> base URL
	client  *http.Client
	appends uint64
	votes   uint64
}

// NewHTTPTransport builds a transport for the given peer base URLs
// (for example {"kv-n2": "http://n2-peer:9090"}).
func NewHTTPTransport(self string, addrs map[string]string, rpcTimeout time.Duration) *HTTPTransport {
	t := &HTTPTransport{
		self:  self,
		addrs: make(map[string]string, len(addrs)),
	}
	for k, v := range addrs {
		t.addrs[k] = strings.TrimRight(v, "/")
	}
	t.client = &http.Client{
		Timeout: 0, // the per-call context governs
		Transport: &http.Transport{
			Proxy: nil,
			DialContext: (&net.Dialer{
				Timeout:   rpcTimeout,
				KeepAlive: 15 * time.Second,
			}).DialContext,
			MaxIdleConns:          16,
			MaxIdleConnsPerHost:   4,
			MaxConnsPerHost:       8,
			IdleConnTimeout:       30 * time.Second,
			ResponseHeaderTimeout: rpcTimeout,
			DisableCompression:    true,
		},
	}
	return t
}

// PeerIDs returns the configured peer identities, sorted.
func (t *HTTPTransport) PeerIDs() []string {
	out := make([]string, 0, len(t.addrs))
	for k := range t.addrs {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Stats returns RPC counters for GET /status.
func (t *HTTPTransport) Stats() (appends, votes uint64) {
	return atomic.LoadUint64(&t.appends), atomic.LoadUint64(&t.votes)
}

// AppendEntries implements Transport.
func (t *HTTPTransport) AppendEntries(ctx context.Context, peer string, req AppendReq) (AppendResp, error) {
	atomic.AddUint64(&t.appends, 1)
	var resp AppendResp
	err := t.call(ctx, peer, PathAppend, req, &resp)
	return resp, err
}

// RequestVote implements Transport.
func (t *HTTPTransport) RequestVote(ctx context.Context, peer string, req VoteReq) (VoteResp, error) {
	atomic.AddUint64(&t.votes, 1)
	var resp VoteResp
	err := t.call(ctx, peer, PathVote, req, &resp)
	return resp, err
}

// Close implements Transport.
func (t *HTTPTransport) Close() error {
	if tr, ok := t.client.Transport.(*http.Transport); ok {
		tr.CloseIdleConnections()
	}
	return nil
}

func (t *HTTPTransport) call(ctx context.Context, peer, path string, in, out any) error {
	base, ok := t.addrs[peer]
	if !ok {
		return fmt.Errorf("raft: unknown peer %q", peer)
	}
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, base+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	res, err := t.client.Do(httpReq)
	if err != nil {
		return err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 1<<16))
		res.Body.Close()
	}()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("raft: peer %s returned %d", peer, res.StatusCode)
	}
	return json.NewDecoder(res.Body).Decode(out)
}

// PeerHandler returns the peer-plane HTTP handler for a node.
func PeerHandler(r *Raft) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(PathAppend, func(w http.ResponseWriter, req *http.Request) {
		var in AppendReq
		if err := decodeJSON(w, req, &in); err != nil {
			return
		}
		writeJSON(w, r.HandleAppend(in))
	})
	mux.HandleFunc(PathVote, func(w http.ResponseWriter, req *http.Request) {
		var in VoteReq
		if err := decodeJSON(w, req, &in); err != nil {
			return
		}
		writeJSON(w, r.HandleVote(in))
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"ok": true, "plane": "peer", "node": r.ID()})
	})
	return mux
}

func decodeJSON(w http.ResponseWriter, req *http.Request, out any) error {
	if req.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return fmt.Errorf("method")
	}
	dec := json.NewDecoder(io.LimitReader(req.Body, 32<<20))
	if err := dec.Decode(out); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return err
	}
	return nil
}

func writeJSON(w http.ResponseWriter, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		http.Error(w, "encode", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
}
