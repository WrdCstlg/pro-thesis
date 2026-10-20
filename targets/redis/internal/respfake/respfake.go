// Package respfake is a scripted RESP2 server for the redis target's tests. A
// handler maps each command to the raw bytes of its reply, so a test states the
// reply shape it means (taken from the RESP specification and Redis's command
// documentation) byte for byte, including shapes no real server sends.
package respfake

import (
	"bufio"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/WrdCstlg/pro-thesis/targets/redis/internal/redisapi"
)

// Special handler results.
const (
	// Hangup closes the connection after the command was read, with no reply.
	Hangup = "\x00hangup"
	// Silent reads the command and never answers; the connection stays open
	// until the server closes.
	Silent = "\x00silent"
)

// Handler returns the raw reply for one command (its arguments as sent).
type Handler func(args []string) string

// Server is one fake endpoint on 127.0.0.1.
type Server struct {
	ln      net.Listener
	handler Handler

	mu    sync.Mutex
	log   [][]string
	conns []net.Conn
	wg    sync.WaitGroup
	done  chan struct{}
}

// Start listens on an ephemeral loopback port.
func Start(h Handler) (*Server, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	s := &Server{ln: ln, handler: h, done: make(chan struct{})}
	s.wg.Add(1)
	go s.accept()
	return s, nil
}

// Addr is host:port.
func (s *Server) Addr() string { return s.ln.Addr().String() }

// Log returns every command received so far, in arrival order.
func (s *Server) Log() [][]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([][]string(nil), s.log...)
}

// Count returns how many received commands have this name (case-insensitive).
func (s *Server) Count(name string) int {
	n := 0
	for _, c := range s.Log() {
		if len(c) > 0 && strings.EqualFold(c[0], name) {
			n++
		}
	}
	return n
}

// Close stops the server and every open connection.
func (s *Server) Close() {
	select {
	case <-s.done:
		return
	default:
	}
	close(s.done)
	_ = s.ln.Close()
	s.mu.Lock()
	for _, c := range s.conns {
		_ = c.Close()
	}
	s.mu.Unlock()
	s.wg.Wait()
}

func (s *Server) accept() {
	defer s.wg.Done()
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		s.conns = append(s.conns, c)
		s.mu.Unlock()
		s.wg.Add(1)
		go s.serve(c)
	}
}

func (s *Server) serve(c net.Conn) {
	defer s.wg.Done()
	defer c.Close()
	r := bufio.NewReader(c)
	for {
		cmd, err := redisapi.ReadReply(r)
		if err != nil || cmd.Kind != redisapi.Array {
			return
		}
		args := make([]string, 0, len(cmd.Elems))
		for _, e := range cmd.Elems {
			args = append(args, e.Str)
		}
		s.mu.Lock()
		s.log = append(s.log, args)
		s.mu.Unlock()
		out := s.handler(args)
		switch out {
		case Hangup:
			return
		case Silent:
			select {
			case <-s.done:
			case <-time.After(time.Minute):
			}
			return
		}
		if _, err := c.Write([]byte(out)); err != nil {
			return
		}
	}
}

// Bulk renders a bulk string reply.
func Bulk(s string) string { return "$" + itoa(len(s)) + "\r\n" + s + "\r\n" }

// Fields renders a flat array of bulk strings, the shape of SENTINEL master.
func Fields(kv ...string) string {
	var b strings.Builder
	b.WriteString("*" + itoa(len(kv)) + "\r\n")
	for _, s := range kv {
		b.WriteString(Bulk(s))
	}
	return b.String()
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var d []byte
	for n > 0 {
		d = append([]byte{byte('0' + n%10)}, d...)
		n /= 10
	}
	return string(d)
}
