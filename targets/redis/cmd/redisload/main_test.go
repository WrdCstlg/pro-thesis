package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/WrdCstlg/pro-thesis/pkg/schema"
	"github.com/WrdCstlg/pro-thesis/targets/redis/internal/redisapi"
	"github.com/WrdCstlg/pro-thesis/targets/redis/internal/respfake"
)

// The reply shapes below come from the RESP specification
// (https://redis.io/docs/latest/develop/reference/protocol-spec/) and Redis's
// command documentation, not from a live server, so these tests could fail
// before the driver existed:
//
//   - SET key value answers the simple string +OK (SET, "Return information").
//   - GET answers a bulk string, or the null bulk string $-1\r\n when the key
//     does not exist (GET; RESP2 "Null bulk strings").
//   - A replica refuses a write with an error whose first word is READONLY:
//     "-READONLY You can't write against a read only replica." (replication,
//     "Read-only replica"; the text is redis/src/server.c shared.roslaveerr).
//   - INFO <section> answers one bulk string of `key:value` lines; INFO server
//     carries redis_version and run_id, INFO replication carries role.
//   - SENTINEL get-master-addr-by-name answers [ip, port] as two bulk strings,
//     or a null array when the name is not monitored; SENTINEL master answers a
//     flat array of field/value bulk strings including ip, port, runid,
//     flags, quorum, down-after-milliseconds and failover-timeout (Sentinel,
//     "Sentinel API").

const pinnedVersion = "8.10.2"

// node is a fake Redis server. Every node of one world shares one store, so a
// history it produces is linearizable; the tests are about the DRIVER.
type node struct {
	id      string
	ip      string // the container address Sentinel would report
	runID   string
	version string
	role    string // INFO replication role: master or slave
	store   *store

	mu       sync.Mutex
	setReply string // when set, SET answers these raw bytes and stores nothing
	getReply string // when set, GET answers these raw bytes
	srv      *respfake.Server
}

type store struct {
	mu sync.Mutex
	m  map[string]string
}

func (n *node) handle(args []string) string {
	if len(args) == 0 {
		return "-ERR empty command\r\n"
	}
	n.mu.Lock()
	setReply, getReply, version, role := n.setReply, n.getReply, n.version, n.role
	n.mu.Unlock()
	switch strings.ToUpper(args[0]) {
	case "INFO":
		section := ""
		if len(args) > 1 {
			section = strings.ToLower(args[1])
		}
		switch section {
		case "server":
			return respfake.Bulk("# Server\r\nredis_version:" + version + "\r\nredis_mode:standalone\r\nrun_id:" + n.runID + "\r\ntcp_port:6379\r\n")
		case "replication":
			return respfake.Bulk("# Replication\r\nrole:" + role + "\r\nconnected_slaves:0\r\n")
		}
		return "-ERR unknown section\r\n"
	case "SET":
		if setReply != "" {
			return setReply
		}
		if role != "master" {
			return "-READONLY You can't write against a read only replica.\r\n"
		}
		n.store.mu.Lock()
		n.store.m[args[1]] = args[2]
		n.store.mu.Unlock()
		return "+OK\r\n"
	case "GET":
		if getReply != "" {
			return getReply
		}
		n.store.mu.Lock()
		v, ok := n.store.m[args[1]]
		n.store.mu.Unlock()
		if !ok {
			return "$-1\r\n"
		}
		return respfake.Bulk(v)
	}
	return "-ERR unknown command '" + args[0] + "'\r\n"
}

// sentinel is a fake Sentinel naming one node as the primary.
type sentinel struct {
	mu       sync.Mutex
	primary  *node
	version  string
	runIDFor func(*node) string // the runid it reports; nil means the node's own
	quorum   string
	srv      *respfake.Server
}

func (s *sentinel) handle(args []string) string {
	s.mu.Lock()
	p, version, runIDFor, quorum := s.primary, s.version, s.runIDFor, s.quorum
	s.mu.Unlock()
	switch strings.ToUpper(args[0]) {
	case "INFO":
		return respfake.Bulk("# Server\r\nredis_version:" + version + "\r\nredis_mode:sentinel\r\nrun_id:5e000000000000000000000000000000000000ff\r\n")
	case "SENTINEL":
		if len(args) < 3 || args[2] != "mymaster" {
			return "*-1\r\n"
		}
		rid := p.runID
		if runIDFor != nil {
			rid = runIDFor(p)
		}
		switch strings.ToLower(args[1]) {
		case "get-master-addr-by-name":
			return respfake.Fields(p.ip, "6379")
		case "master":
			return respfake.Fields("name", "mymaster", "ip", p.ip, "port", "6379", "runid", rid,
				"flags", "master", "quorum", quorum, "down-after-milliseconds", "1000",
				"failover-timeout", "3000", "parallel-syncs", "1", "config-epoch", "0",
				"num-slaves", "2", "num-other-sentinels", "2")
		}
	}
	return "-ERR unknown command\r\n"
}

type world struct {
	nodes     []*node
	sentinels []*sentinel
}

// newWorld starts three fake nodes and three fake Sentinels that agree the
// node at index primary is the primary.
func newWorld(t *testing.T, primary int) *world {
	t.Helper()
	st := &store{m: map[string]string{}}
	w := &world{}
	for i := 0; i < 3; i++ {
		n := &node{id: fmt.Sprintf("redis-n%d", i+1), ip: fmt.Sprintf("172.20.0.%d", i+2),
			runID: fmt.Sprintf("%040d", i+1), version: pinnedVersion, role: "slave", store: st}
		if i == primary {
			n.role = "master"
		}
		srv, err := respfake.Start(n.handle)
		if err != nil {
			t.Fatal(err)
		}
		n.srv = srv
		t.Cleanup(srv.Close)
		w.nodes = append(w.nodes, n)
	}
	for i := 0; i < 3; i++ {
		s := &sentinel{primary: w.nodes[primary], version: pinnedVersion, quorum: "2"}
		srv, err := respfake.Start(s.handle)
		if err != nil {
			t.Fatal(err)
		}
		s.srv = srv
		t.Cleanup(srv.Close)
		w.sentinels = append(w.sentinels, s)
	}
	return w
}

func (w *world) nodeFlag() string {
	var parts []string
	for _, n := range w.nodes {
		parts = append(parts, n.id+"="+n.srv.Addr())
	}
	return strings.Join(parts, ",")
}

func (w *world) sentinelFlag() string {
	var parts []string
	for i, s := range w.sentinels {
		parts = append(parts, w.nodes[i].id+"="+s.srv.Addr())
	}
	return strings.Join(parts, ",")
}

func (w *world) args(hist, plan, primary string) []string {
	return []string{"--history", hist, "--seed", "7", "--plan", plan,
		"--nodes", w.nodeFlag(), "--sentinels", w.sentinelFlag(), "--master-name", "mymaster",
		"--expect-version", pinnedVersion, "--expect-primary", primary, "--expect-quorum", "2",
		"--expect-down-after", "1000ms", "--expect-failover-timeout", "3000ms",
		"--keys", "4", "--timeout", "2s", "--setup-timeout", "2s"}
}

// writePlan writes a PROFILE plan (prothesis.driver_plan/v1) the way the
// harness does.
func writePlan(t *testing.T, dir, profile string, clients, ops int, mix map[string]int64) string {
	t.Helper()
	type entry struct {
		Op        string `json:"op"`
		WeightPPM int64  `json:"weight_ppm"`
	}
	var m []entry
	for op, w := range mix {
		m = append(m, entry{op, w})
	}
	doc := map[string]any{"schema": "prothesis.driver_plan/v1", "profile": profile, "seed": 1,
		"history_path": "x", "clients": clients, "ops": ops, "mix": m}
	b, _ := json.Marshal(doc)
	p := filepath.Join(dir, profile+"-"+strconv.Itoa(len(b))+".json")
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// line is one history record as the driver writes it, meta included.
type line struct {
	schema.HistoryEntry
	Meta struct {
		Target   string          `json:"target"`
		Primary  string          `json:"primary"`
		Sentinel string          `json:"sentinel"`
		Reply    string          `json:"reply"`
		Topology json.RawMessage `json:"topology"`
	} `json:"meta"`
}

// readHistory decodes every line through the PUBLIC schema reader and
// validates it, exactly as the harness does, and also keeps the meta.
func readHistory(t *testing.T, path string) []line {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read history: %v", err)
	}
	hr := schema.NewHistoryReader(bytes.NewReader(raw))
	for {
		e, err := hr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("history line %d: %v", hr.LineNo(), err)
		}
		if err := e.Validate(); err != nil {
			t.Fatalf("history line %d is not a valid record: %v", hr.LineNo(), err)
		}
	}
	var out []line
	for _, l := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if l == "" {
			continue
		}
		var r line
		if err := json.Unmarshal([]byte(l), &r); err != nil {
			t.Fatalf("history line %q: %v", l, err)
		}
		out = append(out, r)
	}
	return out
}

// pairs asserts every op_id has exactly one invoke and one completion with the
// same f and key, and that every record names the node served and the primary
// believed, then returns the pairs.
func pairs(t *testing.T, hist []line) map[int64][2]*line {
	t.Helper()
	byID := map[int64][2]*line{}
	for i := range hist {
		e := &hist[i]
		if e.OpID == nil {
			t.Fatalf("record %d carries no op_id", i)
		}
		if e.Meta.Target == "" || e.Meta.Primary == "" {
			t.Fatalf("op_id %d (%s): meta.target %q, meta.primary %q; every record must name both",
				*e.OpID, e.Type, e.Meta.Target, e.Meta.Primary)
		}
		p := byID[*e.OpID]
		if e.Type == schema.HistoryInvoke {
			if p[0] != nil {
				t.Fatalf("op_id %d has two invokes", *e.OpID)
			}
			p[0] = e
		} else {
			if p[1] != nil {
				t.Fatalf("op_id %d has two completions", *e.OpID)
			}
			p[1] = e
		}
		byID[*e.OpID] = p
	}
	for id, p := range byID {
		if p[0] == nil || p[1] == nil {
			t.Fatalf("op_id %d is not a complete invoke/completion pair", id)
		}
		if p[0].F != p[1].F || *p[0].Key != *p[1].Key {
			t.Fatalf("op_id %d: invoke %s %s, completion %s %s", id, p[0].F, *p[0].Key, p[1].F, *p[1].Key)
		}
	}
	return byID
}

func completions(t *testing.T, hist []line, f string) []*line {
	t.Helper()
	var out []*line
	for _, p := range pairs(t, hist) {
		if p[1].F == f {
			out = append(out, p[1])
		}
	}
	return out
}

func runOK(t *testing.T, args []string) {
	t.Helper()
	code, err := run(args, strings.NewReader(""), io.Discard)
	if err != nil || code != exitOK {
		t.Fatalf("run: code=%d err=%v", code, err)
	}
}

// ---------------------------------------------------------------------------
// Classification and contract tests
// ---------------------------------------------------------------------------

func TestASimpleStringOkIsOk(t *testing.T) {
	w := newWorld(t, 0)
	dir := t.TempDir()
	hist := filepath.Join(dir, "history.jsonl")
	runOK(t, w.args(hist, writePlan(t, dir, "w", 2, 12, map[string]int64{"write": 1000000}), "redis-n1"))
	byID := pairs(t, readHistory(t, hist))
	if len(byID) != 12 {
		t.Fatalf("%d operations recorded, the plan says 12", len(byID))
	}
	for _, p := range byID {
		if p[1].Type != schema.HistoryOK {
			t.Fatalf("op_id %d: SET answered +OK was recorded %s (%q)", *p[1].OpID, p[1].Type, p[1].Error)
		}
		if string(p[1].Value) != string(p[0].Value) {
			t.Fatalf("op_id %d: ok value %s, invoked with %s", *p[1].OpID, p[1].Value, p[0].Value)
		}
	}
	if n := w.nodes[0].srv.Count("SET"); n != 12 {
		t.Fatalf("the primary received %d SET(s); the history holds 12 ok writes", n)
	}
}

func TestAReadonlyErrorIsFail(t *testing.T) {
	w := newWorld(t, 0)
	w.nodes[0].setReply = "-READONLY You can't write against a read only replica.\r\n"
	dir := t.TempDir()
	hist := filepath.Join(dir, "history.jsonl")
	runOK(t, w.args(hist, writePlan(t, dir, "w", 2, 10, map[string]int64{"write": 1000000}), "redis-n1"))
	writes := completions(t, readHistory(t, hist), "write")
	if len(writes) == 0 {
		t.Fatal("no write was recorded")
	}
	for _, c := range writes {
		if c.Type != schema.HistoryFail {
			t.Fatalf("op_id %d: a write refused with -READONLY was recorded %s, want fail", *c.OpID, c.Type)
		}
		if !strings.Contains(c.Error, "READONLY") {
			t.Fatalf("op_id %d: error %q does not carry the refusal", *c.OpID, c.Error)
		}
	}
}

func TestAnErrorAfterSendIsInfo(t *testing.T) {
	cases := map[string]string{
		// An error that is not READONLY says nothing about whether the write
		// was applied, from this driver's point of view.
		"generic error": "-ERR something went wrong\r\n",
		"loading":       "-LOADING Redis is loading the dataset in memory\r\n",
		"masterdown":    "-MASTERDOWN Link with MASTER is down and replica-serve-stale-data is set to 'no'.\r\n",
		// The command was read and the connection died with no reply.
		"connection dropped after send": respfake.Hangup,
		// The command was read and no reply came within the client timeout.
		"no reply within the timeout": respfake.Silent,
	}
	for name, reply := range cases {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t, 0)
			w.nodes[0].setReply = reply
			dir := t.TempDir()
			hist := filepath.Join(dir, "history.jsonl")
			args := append(w.args(hist, writePlan(t, dir, "w", 2, 4, map[string]int64{"write": 1000000}), "redis-n1"),
				"--timeout", "300ms")
			runOK(t, args)
			writes := completions(t, readHistory(t, hist), "write")
			if len(writes) == 0 {
				t.Fatal("no write was recorded")
			}
			for _, c := range writes {
				if c.Type != schema.HistoryInfo {
					t.Fatalf("op_id %d: a write answered %q after it was sent was recorded %s (%q), want info",
						*c.OpID, reply, c.Type, c.Error)
				}
			}
		})
	}

	// The boundary: a command that provably never left the client is fail.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := ln.Addr().String()
	_ = ln.Close()
	_, derr := redisapi.Dial(dead, time.Second)
	if derr == nil {
		t.Fatalf("expected a dial error against %s", dead)
	}
	if got := classifyWrite(attempt{err: derr}); got.typ != typeFail {
		t.Fatalf("connection refused before any byte was sent: got %s (%q), want fail", got.typ, got.label)
	}
	if got := classifyWrite(attempt{err: &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}}); got.typ != typeFail {
		t.Fatalf("dial refused: got %s, want fail", got.typ)
	}
	if got := classifyWrite(attempt{sent: true, err: errors.New("connection reset by peer")}); got.typ != typeInfo {
		t.Fatalf("an error after the command was sent: got %s, want info", got.typ)
	}
}

func TestANilBulkReplyIsTheAbsentValue(t *testing.T) {
	w := newWorld(t, 0)
	w.nodes[0].getReply = "$-1\r\n"
	dir := t.TempDir()
	hist := filepath.Join(dir, "history.jsonl")
	runOK(t, w.args(hist, writePlan(t, dir, "r", 2, 30, map[string]int64{"read": 1000000}), "redis-n1"))
	reads := completions(t, readHistory(t, hist), "read")
	if len(reads) == 0 {
		t.Fatal("no read was recorded")
	}
	for _, c := range reads {
		if c.Type != schema.HistoryOK || string(c.Value) != "null" {
			t.Fatalf("op_id %d: GET answered $-1 was recorded %s with value %q (%q), want ok with null",
				*c.OpID, c.Type, c.Value, c.Error)
		}
	}

	// The null ARRAY is not GET's absent value: it is not a reply GET sends.
	if got := classifyRead(attempt{sent: true, reply: redisapi.Reply{Kind: redisapi.Array, Nil: true}}); got.typ == typeOK {
		t.Fatalf("GET answered *-1 was classified ok with value %s", got.value)
	}
}

func TestAMalformedReplyIsNotOk(t *testing.T) {
	setCases := map[string]string{
		"a different simple string": "+OKAY\r\n",
		"lower case":                "+ok\r\n",
		"OK as a bulk string":       "$2\r\nOK\r\n",
		"an integer":                ":1\r\n",
		"no type byte":              "OK\r\n",
		"LF without CR":             "+OK\n",
		"an array":                  "*1\r\n+OK\r\n",
	}
	for name, reply := range setCases {
		t.Run("SET "+name, func(t *testing.T) {
			w := newWorld(t, 0)
			w.nodes[0].setReply = reply
			dir := t.TempDir()
			hist := filepath.Join(dir, "history.jsonl")
			args := append(w.args(hist, writePlan(t, dir, "w", 1, 3, map[string]int64{"write": 1000000}), "redis-n1"),
				"--timeout", "300ms")
			runOK(t, args)
			for _, c := range completions(t, readHistory(t, hist), "write") {
				if c.Type == schema.HistoryOK {
					t.Fatalf("op_id %d: SET answered %q was recorded ok", *c.OpID, reply)
				}
			}
		})
	}
	getCases := map[string]string{
		"bulk length mismatch": "$3\r\nab\r\n",
		"a simple string":      "+5\r\n",
		"an integer":           ":5\r\n",
		"not an integer value": "$3\r\nabc\r\n",
		"an unknown type":      "_\r\n",
	}
	for name, reply := range getCases {
		t.Run("GET "+name, func(t *testing.T) {
			w := newWorld(t, 0)
			w.nodes[0].getReply = reply
			dir := t.TempDir()
			hist := filepath.Join(dir, "history.jsonl")
			args := append(w.args(hist, writePlan(t, dir, "r", 1, 12, map[string]int64{"read": 1000000}), "redis-n1"),
				"--timeout", "300ms")
			runOK(t, args)
			reads := completions(t, readHistory(t, hist), "read")
			if len(reads) == 0 {
				t.Fatal("no read was recorded")
			}
			for _, c := range reads {
				if c.Type == schema.HistoryOK {
					t.Fatalf("op_id %d: GET answered %q was recorded ok with value %s", *c.OpID, reply, c.Value)
				}
			}
		})
	}
}

func TestWritesGoToThePrimarySentinelNames(t *testing.T) {
	// Sentinel names redis-n2 by its container address, which no target flag
	// carries; only the runid says which node that is. The replicas refuse
	// writes, as replicas do.
	w := newWorld(t, 1)
	dir := t.TempDir()
	hist := filepath.Join(dir, "history.jsonl")
	runOK(t, w.args(hist, writePlan(t, dir, "m", 3, 60, map[string]int64{"write": 600000, "read": 400000}), "redis-n2"))

	for _, i := range []int{0, 2} {
		if n := w.nodes[i].srv.Count("SET"); n != 0 {
			t.Fatalf("%s is a replica and received %d SET(s)", w.nodes[i].id, n)
		}
		if n := w.nodes[i].srv.Count("GET"); n != 0 {
			t.Fatalf("%s is a replica and received %d GET(s); reads go to the primary", w.nodes[i].id, n)
		}
	}
	byID := pairs(t, readHistory(t, hist))
	var ok int
	for _, p := range byID {
		for _, r := range p {
			if r.Meta.Target != "redis-n2" || r.Meta.Primary != "redis-n2" {
				t.Fatalf("op_id %d (%s): served by %q, believed primary %q; Sentinel names redis-n2",
					*r.OpID, r.Type, r.Meta.Target, r.Meta.Primary)
			}
		}
		if p[1].Type == schema.HistoryOK && p[1].F == "write" {
			ok++
		}
	}
	if ok == 0 || ok != w.nodes[1].srv.Count("SET") {
		t.Fatalf("ok writes %d, SETs the primary received %d", ok, w.nodes[1].srv.Count("SET"))
	}

	// A runid that is no node of this world is refused at start, never
	// matched by address.
	stray := newWorld(t, 1)
	for _, s := range stray.sentinels {
		s.runIDFor = func(*node) string { return strings.Repeat("e", 40) }
	}
	hist2 := filepath.Join(dir, "history2.jsonl")
	code, err := run(stray.args(hist2, writePlan(t, dir, "s", 1, 5, map[string]int64{"write": 1000000}), "redis-n2"),
		strings.NewReader(""), io.Discard)
	if code == exitOK || err == nil || !strings.Contains(err.Error(), strings.Repeat("e", 40)) {
		t.Fatalf("a primary runid no node reports: code=%d err=%v; want a refusal naming the runid", code, err)
	}
	if _, statErr := os.Stat(hist2); statErr == nil {
		t.Fatal("a refused start must not leave a history behind")
	}
}

func TestAVersionMismatchEndsTheRun(t *testing.T) {
	for name, set := range map[string]func(*world){
		"a node":     func(w *world) { w.nodes[2].version = "8.0.0" },
		"a Sentinel": func(w *world) { w.sentinels[1].version = "8.0.0" },
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t, 0)
			set(w)
			dir := t.TempDir()
			hist := filepath.Join(dir, "history.jsonl")
			code, err := run(w.args(hist, writePlan(t, dir, "v", 2, 20, map[string]int64{"write": 500000, "read": 500000}), "redis-n1"),
				strings.NewReader(""), io.Discard)
			if code == exitOK || err == nil {
				t.Fatalf("code=%d err=%v; a server reporting 8.0.0 against --expect-version %s must end the run non-zero",
					code, err, pinnedVersion)
			}
			if !strings.Contains(err.Error(), "8.0.0") || !strings.Contains(err.Error(), pinnedVersion) {
				t.Fatalf("the error must name both versions: %v", err)
			}
			if _, statErr := os.Stat(hist); statErr == nil {
				t.Fatal("a refused start must not leave a history behind")
			}
			for _, n := range w.nodes {
				if c := n.srv.Count("SET"); c != 0 {
					t.Fatalf("%s received %d SET(s) before the version was checked", n.id, c)
				}
			}
		})
	}
}

func TestTheDriverReadsNoWorkloadFromTheEnvironment(t *testing.T) {
	w := newWorld(t, 0)
	dir := t.TempDir()

	// A hostile environment: every name a driver might plausibly read a
	// workload or a topology from, pointing somewhere else.
	hostile := writePlan(t, dir, "hostile", 1, 3, map[string]int64{"read": 1000000})
	for k, v := range map[string]string{
		"PROTHESIS_PLAN_PATH":      hostile,
		"PROTHESIS_PROFILE":        "hostile",
		"PROTHESIS_OPS":            "3",
		"PROTHESIS_CLIENTS":        "1",
		"PROTHESIS_KEYS":           "1",
		"PROTHESIS_STDIN_CONTROL":  "1",
		"PROTHESIS_EXPECT_VERSION": "0.0.0",
		"PROTHESIS_TARGETS":        "127.0.0.1:1,127.0.0.1:2,127.0.0.1:3",
		"REDIS_MASTER_NAME":        "other",
	} {
		t.Setenv(k, v)
	}

	// 1. With an explicit plan, the workload is the plan's.
	hist := filepath.Join(dir, "history.jsonl")
	runOK(t, w.args(hist, writePlan(t, dir, "mixed", 2, 40, map[string]int64{"read": 500000, "write": 500000}), "redis-n1"))
	byID := pairs(t, readHistory(t, hist))
	if len(byID) != 40 {
		t.Fatalf("ran %d operations; the plan says 40", len(byID))
	}

	// 2. With no --plan, the driver refuses rather than pick one up from
	// PROTHESIS_PLAN_PATH.
	hist2 := filepath.Join(dir, "history2.jsonl")
	var noPlan []string
	args := w.args(hist2, "x", "redis-n1")
	for i := 0; i < len(args); i++ {
		if args[i] == "--plan" {
			i++
			continue
		}
		noPlan = append(noPlan, args[i])
	}
	code, err := run(noPlan, strings.NewReader(""), io.Discard)
	if code != exitConfig || err == nil {
		t.Fatalf("no --plan: code=%d err=%v; want a config refusal", code, err)
	}
	if _, statErr := os.Stat(hist2); statErr == nil {
		t.Fatal("no --plan: a refused start must not leave a history behind")
	}

	// 3. The source reads no environment variable at all: PROTHESIS_TARGETS
	// carries the health shims' ports on this target, not RESP addresses.
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`os\.(Getenv|LookupEnv|Environ|ExpandEnv)\(`)
	if m := re.FindString(string(src)); m != "" {
		t.Fatalf("main.go reads the environment through %s", m)
	}
}

// The topology the gate's verdict depends on is
// checked against the lock-covered command line at start and recorded on the
// first record of the history.
func TestTheTopologyIsCheckedAndRecorded(t *testing.T) {
	w := newWorld(t, 0)
	dir := t.TempDir()
	hist := filepath.Join(dir, "history.jsonl")
	runOK(t, w.args(hist, writePlan(t, dir, "t", 1, 6, map[string]int64{"write": 1000000}), "redis-n1"))
	h := readHistory(t, hist)
	if len(h) == 0 || len(h[0].Meta.Topology) == 0 {
		t.Fatal("the first record carries no meta.topology")
	}
	var topo struct {
		MasterName        string `json:"master_name"`
		ConfiguredPrimary string `json:"configured_primary"`
		Quorum            int    `json:"quorum"`
		DownAfterMS       int64  `json:"down_after_ms"`
		FailoverTimeoutMS int64  `json:"failover_timeout_ms"`
		Sentinels         []struct {
			Node    string `json:"node"`
			Primary string `json:"primary"`
		} `json:"sentinels"`
		Nodes []struct {
			ID   string `json:"id"`
			Role string `json:"role"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal(h[0].Meta.Topology, &topo); err != nil {
		t.Fatalf("meta.topology: %v", err)
	}
	if topo.MasterName != "mymaster" || topo.ConfiguredPrimary != "redis-n1" || topo.Quorum != 2 ||
		topo.DownAfterMS != 1000 || topo.FailoverTimeoutMS != 3000 || len(topo.Sentinels) != 3 || len(topo.Nodes) != 3 {
		t.Fatalf("meta.topology is incomplete: %s", h[0].Meta.Topology)
	}
	for _, s := range topo.Sentinels {
		if s.Primary != "redis-n1" {
			t.Fatalf("Sentinel at %s recorded primary %q", s.Node, s.Primary)
		}
	}

	// A topology that is not the one the command line states is refused.
	for name, c := range map[string]struct {
		set     func(*world)
		primary string
	}{
		"another primary": {func(*world) {}, "redis-n2"},
		"another quorum":  {func(w *world) { w.sentinels[0].quorum = "1" }, "redis-n1"},
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t, 0)
			c.set(w)
			hist := filepath.Join(t.TempDir(), "history.jsonl")
			code, err := run(w.args(hist, writePlan(t, dir, "x", 1, 3, map[string]int64{"write": 1000000}), c.primary),
				strings.NewReader(""), io.Discard)
			if code == exitOK || err == nil {
				t.Fatalf("code=%d err=%v; a topology unlike the stated one must be refused", code, err)
			}
			if _, statErr := os.Stat(hist); statErr == nil {
				t.Fatal("a refused start must not leave a history behind")
			}
		})
	}
}
