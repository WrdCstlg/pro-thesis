// Package redisapi is the slice of Redis's wire protocol and Sentinel surface
// the target's programs share (cmd/redisload, cmd/redisrole, cmd/redissteady,
// cmd/redishealth), so they cannot disagree about what a reply is or which
// node a Sentinel means. Standard library only.
//
// The protocol is RESP2 as the Redis protocol specification defines it
// (https://redis.io/docs/latest/develop/reference/protocol-spec/): every
// command is an array of bulk strings, and a reply is one of
//
//	+<text>\r\n                 simple string
//	-<text>\r\n                 error
//	:<int>\r\n                  integer
//	$<n>\r\n<n bytes>\r\n       bulk string; $-1\r\n is the null bulk string
//	*<n>\r\n<n replies>         array; *-1\r\n is the null array
//
// No HELLO is sent, so a server stays on RESP2 and a RESP3 type byte is a
// malformed reply here, never a guess. The parser is strict: a missing CRLF, a
// bulk string whose length does not match, or an unknown type byte is
// ErrMalformed, and a caller must treat the connection as out of step.
//
// # Mapping Sentinel's addresses to node ids
//
// Sentinel names a primary by the address it reaches it at, which is a
// container address on the compose network (172.x). The host programs cannot
// route to it and the harness knows nodes only by id. The mapping used here is
// the server's run_id: `SENTINEL master <name>` reports the primary's `runid`
// alongside its `ip` and `port`, and every node reports its own `run_id` in
// `INFO server`, asked over the node's published host port. A run_id is a
// random 40-hex-character identity Redis draws at every process start, so two
// nodes cannot share one and a restarted node gets a new one. A Sentinel answer
// whose runid is empty (Sentinel has not yet read the node's INFO) or matches
// no node is refused, never matched by address or by guess. No announce-ip is
// configured anywhere: replication and Sentinel traffic stay on the compose
// network, where a partition can reach them.
package redisapi

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"
)

// Kind is a RESP2 reply's type byte.
type Kind byte

// The five RESP2 reply types.
const (
	SimpleString Kind = '+'
	Error        Kind = '-'
	Integer      Kind = ':'
	BulkString   Kind = '$'
	Array        Kind = '*'
)

// Reply is one parsed RESP2 reply.
type Reply struct {
	Kind  Kind
	Str   string // simple string, error text, or bulk string contents
	Int   int64  // integer
	Nil   bool   // $-1 (null bulk string) or *-1 (null array)
	Elems []Reply
}

// String renders a reply for a history label, clipped.
func (r Reply) String() string {
	switch r.Kind {
	case SimpleString:
		return "+" + clip(r.Str)
	case Error:
		return "-" + clip(r.Str)
	case Integer:
		return ":" + strconv.FormatInt(r.Int, 10)
	case BulkString:
		if r.Nil {
			return "$-1"
		}
		return "$" + clip(strconv.Quote(r.Str))
	case Array:
		if r.Nil {
			return "*-1"
		}
		return fmt.Sprintf("*%d", len(r.Elems))
	}
	return fmt.Sprintf("kind %q", byte(r.Kind))
}

// ErrMalformed is a reply that is not RESP2.
var ErrMalformed = errors.New("malformed RESP reply")

const (
	maxLine  = 64 << 10
	maxBulk  = 16 << 20
	maxElems = 1 << 20
	maxDepth = 8
)

// ReadReply reads exactly one reply. io.EOF means the connection closed before
// the first byte of a reply; a reply cut short is ErrMalformed wrapping
// io.ErrUnexpectedEOF.
func ReadReply(r *bufio.Reader) (Reply, error) {
	return readReply(r, 0)
}

func readReply(r *bufio.Reader, depth int) (Reply, error) {
	if depth > maxDepth {
		return Reply{}, fmt.Errorf("%w: nested deeper than %d", ErrMalformed, maxDepth)
	}
	line, err := readLine(r, depth == 0)
	if err != nil {
		return Reply{}, err
	}
	if len(line) == 0 {
		return Reply{}, fmt.Errorf("%w: empty line", ErrMalformed)
	}
	body := line[1:]
	switch Kind(line[0]) {
	case SimpleString, Error:
		return Reply{Kind: Kind(line[0]), Str: body}, nil
	case Integer:
		n, err := strconv.ParseInt(body, 10, 64)
		if err != nil {
			return Reply{}, fmt.Errorf("%w: integer %q", ErrMalformed, body)
		}
		return Reply{Kind: Integer, Int: n}, nil
	case BulkString:
		n, err := strconv.Atoi(body)
		if err != nil || n < -1 || n > maxBulk {
			return Reply{}, fmt.Errorf("%w: bulk length %q", ErrMalformed, body)
		}
		if n == -1 {
			return Reply{Kind: BulkString, Nil: true}, nil
		}
		buf := make([]byte, n+2)
		if _, err := io.ReadFull(r, buf); err != nil {
			return Reply{}, fmt.Errorf("%w: bulk string of %d bytes cut short: %v", ErrMalformed, n, unexpected(err))
		}
		if buf[n] != '\r' || buf[n+1] != '\n' {
			return Reply{}, fmt.Errorf("%w: bulk string of %d bytes is not followed by CRLF", ErrMalformed, n)
		}
		return Reply{Kind: BulkString, Str: string(buf[:n])}, nil
	case Array:
		n, err := strconv.Atoi(body)
		if err != nil || n < -1 || n > maxElems {
			return Reply{}, fmt.Errorf("%w: array length %q", ErrMalformed, body)
		}
		if n == -1 {
			return Reply{Kind: Array, Nil: true}, nil
		}
		out := Reply{Kind: Array, Elems: make([]Reply, 0, n)}
		for i := 0; i < n; i++ {
			e, err := readReply(r, depth+1)
			if err != nil {
				if errors.Is(err, io.EOF) {
					err = fmt.Errorf("%w: array cut short: %v", ErrMalformed, io.ErrUnexpectedEOF)
				}
				return Reply{}, err
			}
			out.Elems = append(out.Elems, e)
		}
		return out, nil
	}
	return Reply{}, fmt.Errorf("%w: unknown type byte %q", ErrMalformed, line[0])
}

// readLine reads one CRLF-terminated line without its terminator. A bare LF,
// a line longer than maxLine, or a connection closed mid-line is ErrMalformed;
// a connection closed before the first byte is io.EOF when first is true.
func readLine(r *bufio.Reader, first bool) (string, error) {
	var b strings.Builder
	for {
		c, err := r.ReadByte()
		if err != nil {
			if errors.Is(err, io.EOF) && b.Len() == 0 && first {
				return "", io.EOF
			}
			if errors.Is(err, io.EOF) {
				return "", fmt.Errorf("%w: line cut short: %v", ErrMalformed, io.ErrUnexpectedEOF)
			}
			return "", err
		}
		if c == '\n' {
			return "", fmt.Errorf("%w: LF without CR", ErrMalformed)
		}
		if c == '\r' {
			n, err := r.ReadByte()
			if err != nil {
				if errors.Is(err, io.EOF) {
					return "", fmt.Errorf("%w: CR at end of input: %v", ErrMalformed, io.ErrUnexpectedEOF)
				}
				return "", err
			}
			if n != '\n' {
				return "", fmt.Errorf("%w: CR not followed by LF", ErrMalformed)
			}
			return b.String(), nil
		}
		if b.Len() >= maxLine {
			return "", fmt.Errorf("%w: line longer than %d bytes", ErrMalformed, maxLine)
		}
		b.WriteByte(c)
	}
}

func unexpected(err error) error {
	if errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}
	return err
}

// EncodeCommand renders a command as the array of bulk strings RESP2 requires.
func EncodeCommand(args ...string) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "*%d\r\n", len(args))
	for _, a := range args {
		fmt.Fprintf(&b, "$%d\r\n%s\r\n", len(a), a)
	}
	return []byte(b.String())
}

// Conn is one connection to a Redis server or Sentinel. It is not safe for
// concurrent use.
type Conn struct {
	c       net.Conn
	r       *bufio.Reader
	timeout time.Duration
}

// Dial connects with a timeout that also bounds every later round trip.
func Dial(addr string, timeout time.Duration) (*Conn, error) {
	c, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return nil, err
	}
	return &Conn{c: c, r: bufio.NewReader(c), timeout: timeout}, nil
}

// Close closes the connection.
func (c *Conn) Close() error { return c.c.Close() }

// Send writes one command. Any error leaves it unknown how much reached the
// server.
func (c *Conn) Send(args ...string) error {
	if err := c.c.SetDeadline(time.Now().Add(c.timeout)); err != nil {
		return err
	}
	_, err := c.c.Write(EncodeCommand(args...))
	return err
}

// Receive reads one reply under the deadline Send set.
func (c *Conn) Receive() (Reply, error) { return ReadReply(c.r) }

// Do is Send then Receive.
func (c *Conn) Do(args ...string) (Reply, error) {
	if err := c.Send(args...); err != nil {
		return Reply{}, err
	}
	return c.Receive()
}

// ---------------------------------------------------------------------------
// Targets
// ---------------------------------------------------------------------------

// Target is one labelled address: a node's RESP port, or the Sentinel
// co-located with that node, in which case ID is the node's id.
type Target struct {
	ID   string
	Addr string
}

// ParseTargets reads an ordered, comma-separated id=host:port list. Every entry
// needs both halves and ids must be distinct.
func ParseTargets(s string) ([]Target, error) {
	var out []Target
	seen := map[string]bool{}
	for _, raw := range strings.Split(s, ",") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		id, addr, ok := strings.Cut(raw, "=")
		id, addr = strings.TrimSpace(id), strings.TrimSpace(addr)
		if !ok || id == "" || addr == "" {
			return nil, fmt.Errorf("target %q: want id=host:port", raw)
		}
		if _, _, err := net.SplitHostPort(addr); err != nil {
			return nil, fmt.Errorf("target %q: %v", raw, err)
		}
		if seen[id] {
			return nil, fmt.Errorf("target id %q appears twice", id)
		}
		seen[id] = true
		out = append(out, Target{ID: id, Addr: addr})
	}
	if len(out) == 0 {
		return nil, errors.New("no targets")
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// INFO and Sentinel replies
// ---------------------------------------------------------------------------

// ParseInfo reads INFO's text: `key:value` lines, `# Section` headers and blank
// lines skipped.
func ParseInfo(s string) map[string]string {
	out := map[string]string{}
	for _, l := range strings.Split(s, "\n") {
		l = strings.TrimRight(l, "\r")
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		if k, v, ok := strings.Cut(l, ":"); ok {
			out[k] = v
		}
	}
	return out
}

// Info asks INFO <section> and parses the bulk-string answer.
func Info(c *Conn, section string) (map[string]string, error) {
	rep, err := c.Do("INFO", section)
	if err != nil {
		return nil, err
	}
	if rep.Kind == Error {
		return nil, fmt.Errorf("INFO %s: -%s", section, rep.Str)
	}
	if rep.Kind != BulkString || rep.Nil {
		return nil, fmt.Errorf("INFO %s: expected a bulk string, got %s", section, rep)
	}
	return ParseInfo(rep.Str), nil
}

// FieldMap reads a flat array of alternating field names and values, the shape
// of `SENTINEL master <name>` and of each element of `SENTINEL replicas <name>`.
func FieldMap(r Reply) (map[string]string, error) {
	if r.Kind != Array || r.Nil {
		return nil, fmt.Errorf("expected a field array, got %s", r)
	}
	if len(r.Elems)%2 != 0 {
		return nil, fmt.Errorf("field array has an odd number (%d) of elements", len(r.Elems))
	}
	out := make(map[string]string, len(r.Elems)/2)
	for i := 0; i < len(r.Elems); i += 2 {
		k, v := r.Elems[i], r.Elems[i+1]
		if k.Kind != BulkString || k.Nil || v.Kind != BulkString || v.Nil {
			return nil, fmt.Errorf("field array element %d is %s/%s, not two bulk strings", i, k, v)
		}
		out[k.Str] = v.Str
	}
	return out, nil
}

// SentinelMaster is what one Sentinel says about the primary it monitors.
type SentinelMaster struct {
	Addr   string            // host:port Sentinel reports (a container address)
	RunID  string            // the primary's run_id as Sentinel last read it
	Fields map[string]string // every field of SENTINEL master <name>
}

// AskSentinel asks `SENTINEL get-master-addr-by-name`, the call a client uses
// to find the primary, and then `SENTINEL master` for the same name, whose
// runid is what maps the address to a node. The two answers must name the same
// address, or the primary moved between them and the answer is refused.
func AskSentinel(c *Conn, name string) (SentinelMaster, error) {
	rep, err := c.Do("SENTINEL", "get-master-addr-by-name", name)
	if err != nil {
		return SentinelMaster{}, err
	}
	if rep.Kind == Error {
		return SentinelMaster{}, fmt.Errorf("SENTINEL get-master-addr-by-name: -%s", rep.Str)
	}
	if rep.Kind != Array || rep.Nil {
		return SentinelMaster{}, fmt.Errorf("SENTINEL get-master-addr-by-name %s: no such primary (%s)", name, rep)
	}
	if len(rep.Elems) != 2 || rep.Elems[0].Kind != BulkString || rep.Elems[1].Kind != BulkString ||
		rep.Elems[0].Nil || rep.Elems[1].Nil {
		return SentinelMaster{}, fmt.Errorf("SENTINEL get-master-addr-by-name: expected [ip, port], got %s", rep)
	}
	addr := net.JoinHostPort(rep.Elems[0].Str, rep.Elems[1].Str)

	rep, err = c.Do("SENTINEL", "master", name)
	if err != nil {
		return SentinelMaster{}, err
	}
	if rep.Kind == Error {
		return SentinelMaster{}, fmt.Errorf("SENTINEL master: -%s", rep.Str)
	}
	fields, err := FieldMap(rep)
	if err != nil {
		return SentinelMaster{}, fmt.Errorf("SENTINEL master: %w", err)
	}
	if got := net.JoinHostPort(fields["ip"], fields["port"]); got != addr {
		return SentinelMaster{}, fmt.Errorf("the primary moved between two questions: get-master-addr-by-name said %s, SENTINEL master says %s", addr, got)
	}
	return SentinelMaster{Addr: addr, RunID: fields["runid"], Fields: fields}, nil
}

// NodeForRunID maps a run_id to a node id through the table the nodes
// themselves reported (run_id -> node id). Empty or unknown is an error.
func NodeForRunID(runID string, byRunID map[string]string) (string, error) {
	if runID == "" {
		return "", errors.New("Sentinel reports no runid for the primary yet")
	}
	id, ok := byRunID[runID]
	if !ok {
		return "", fmt.Errorf("runid %s is no node of this world", runID)
	}
	return id, nil
}

// NodeInfo is what one node says about itself.
type NodeInfo struct {
	ID      string
	RunID   string
	Version string
	Role    string            // "master" or "slave", as INFO replication spells it
	Repl    map[string]string // all of INFO replication
	Err     error             // the node could not be asked
}

// AskNode asks one node INFO server and INFO replication on a fresh
// connection.
func AskNode(t Target, timeout time.Duration) NodeInfo {
	ni := NodeInfo{ID: t.ID}
	c, err := Dial(t.Addr, timeout)
	if err != nil {
		ni.Err = err
		return ni
	}
	defer c.Close()
	srv, err := Info(c, "server")
	if err != nil {
		ni.Err = err
		return ni
	}
	repl, err := Info(c, "replication")
	if err != nil {
		ni.Err = err
		return ni
	}
	ni.RunID, ni.Version, ni.Role, ni.Repl = srv["run_id"], srv["redis_version"], repl["role"], repl
	if ni.RunID == "" || ni.Role == "" {
		ni.Err = fmt.Errorf("INFO names no run_id (%q) or no role (%q)", ni.RunID, ni.Role)
	}
	return ni
}

// RunIDTable builds run_id -> node id from nodes that answered. Two nodes
// reporting one run_id is refused: the mapping would be a guess.
func RunIDTable(nodes []NodeInfo) (map[string]string, error) {
	out := map[string]string{}
	for _, n := range nodes {
		if n.Err != nil || n.RunID == "" {
			continue
		}
		if other, dup := out[n.RunID]; dup {
			return nil, fmt.Errorf("%s and %s report the same run_id %s", other, n.ID, n.RunID)
		}
		out[n.RunID] = n.ID
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// One observation of the cluster, shared by the role and steady-state probes
// ---------------------------------------------------------------------------

// SentinelView is what one Sentinel said about the primary.
type SentinelView struct {
	Sentinel string // the node id it is co-located with (its --sentinels label)
	Primary  string // node id the reported runid maps to
	Master   SentinelMaster
	// FetchErr: the Sentinel could not be asked. It is unobserved, not wrong.
	FetchErr error
	// ContentErr: it answered something that cannot be one view (a runid no
	// node reports, or none yet). It refuses the whole observation.
	ContentErr error
}

// Observation is every node's and every Sentinel's answer, asked once each.
type Observation struct {
	Nodes     []NodeInfo
	Sentinels []SentinelView
	TableErr  error // two nodes reported one run_id
}

// Observe asks every node INFO server and INFO replication, then every
// Sentinel which primary it monitors, mapping each answer to a node by runid.
func Observe(nodes, sentinels []Target, name string, timeout time.Duration) Observation {
	var obs Observation
	for _, n := range nodes {
		obs.Nodes = append(obs.Nodes, AskNode(n, timeout))
	}
	table, err := RunIDTable(obs.Nodes)
	if err != nil {
		obs.TableErr = err
	}
	for _, s := range sentinels {
		v := SentinelView{Sentinel: s.ID}
		c, err := Dial(s.Addr, timeout)
		if err != nil {
			v.FetchErr = err
			obs.Sentinels = append(obs.Sentinels, v)
			continue
		}
		m, err := AskSentinel(c, name)
		_ = c.Close()
		if err != nil {
			v.FetchErr = err
			obs.Sentinels = append(obs.Sentinels, v)
			continue
		}
		v.Master = m
		if id, err := NodeForRunID(m.RunID, table); err != nil {
			v.ContentErr = fmt.Errorf("the Sentinel at %s names primary %s: %w", s.ID, m.Addr, err)
		} else {
			v.Primary = id
		}
		obs.Sentinels = append(obs.Sentinels, v)
	}
	return obs
}

// Agree returns the one primary that every answering Sentinel names and that
// is the one node reporting role:master, or a refusal. Sentinel's view carries
// a config epoch but a node's INFO does not, so two claims of being primary
// cannot be ranked; the only safe answer to any disagreement is none.
func Agree(obs Observation) (string, error) {
	if obs.TableErr != nil {
		return "", obs.TableErr
	}
	var views []SentinelView
	for _, v := range obs.Sentinels {
		if v.ContentErr != nil {
			return "", v.ContentErr
		}
		if v.FetchErr == nil {
			views = append(views, v)
		}
	}
	if len(views) == 0 {
		var why []string
		for _, v := range obs.Sentinels {
			why = append(why, v.Sentinel+": "+v.FetchErr.Error())
		}
		return "", fmt.Errorf("no Sentinel answered: %s", strings.Join(why, "; "))
	}
	primary := views[0].Primary
	for _, v := range views[1:] {
		if v.Primary != primary {
			return "", fmt.Errorf("the Sentinels disagree about the primary: %s", describeViews(views))
		}
	}
	var masters []string
	answered := map[string]bool{}
	for _, n := range obs.Nodes {
		if n.Err != nil {
			continue
		}
		answered[n.ID] = true
		if n.Role == "master" {
			masters = append(masters, n.ID)
		}
	}
	switch {
	case len(masters) > 1:
		return "", fmt.Errorf("%d nodes report role:master (%s); the Sentinels name %s", len(masters), strings.Join(masters, ", "), primary)
	case len(masters) == 1 && masters[0] != primary:
		return "", fmt.Errorf("%s reports role:master but the Sentinels name %s", masters[0], primary)
	case len(masters) == 0 && answered[primary]:
		return "", fmt.Errorf("the Sentinels name %s, which reports role:%s; no node reports role:master", primary, roleOf(obs, primary))
	case len(masters) == 0:
		return "", fmt.Errorf("the Sentinels name %s, which did not answer; no node that answered reports role:master", primary)
	}
	return primary, nil
}

func roleOf(obs Observation, id string) string {
	for _, n := range obs.Nodes {
		if n.ID == id {
			return n.Role
		}
	}
	return ""
}

func describeViews(views []SentinelView) string {
	parts := make([]string, 0, len(views))
	for _, v := range views {
		parts = append(parts, "the Sentinel at "+v.Sentinel+" says "+v.Primary)
	}
	return strings.Join(parts, "; ")
}

func clip(s string) string {
	if len(s) > 200 {
		return s[:200]
	}
	return s
}
