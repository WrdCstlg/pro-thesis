package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os/exec"
	"strings"
	"time"

	"prothesis.dev/kvfixture/internal/kv"
	"prothesis.dev/kvfixture/internal/netsim"
	"prothesis.dev/kvfixture/internal/raft"
)

// nodeRef is one cluster member as the prover sees it.
type nodeRef struct {
	ID        string
	ClientURL string
	Container string
}

// controller is the fault surface the demonstration needs. Two implementations:
// an in-process one (deterministic, no Docker, no ports to leak) and a
// docker-backed one that drives the real compose cluster.
//
// This is demonstration scaffolding for the fixture, NOT the PRO-THESIS fault
// injector. Phase 2 builds that, against unmodified systems, with a NET_ADMIN
// sidecar.
type controller interface {
	Nodes() []nodeRef
	// CutPeers isolates a node on the PEER plane only. Its client plane stays
	// reachable, which is what makes the resulting failure a gray one and the
	// stale read observable at all.
	CutPeers(id string) error
	HealPeers(id string) error
	SupportsPause() bool
	Pause(id string) error
	Unpause(id string) error
	ResidualFaults() (int, error)
	Close()
}

// ------------------------------------------------------------------ in-process

type inprocController struct {
	net    *netsim.Net
	nodes  []nodeRef
	rafts  map[string]*raft.Raft
	srvs   []*http.Server
	lns    []net.Listener
	cutIDs map[string]bool
}

func newInprocController(seed uint64, logger *log.Logger) (*inprocController, error) {
	ids := []string{"kv-n1", "kv-n2", "kv-n3"}
	c := &inprocController{
		net:    netsim.NewNet(time.Millisecond),
		rafts:  map[string]*raft.Raft{},
		cutIDs: map[string]bool{},
	}
	for _, id := range ids {
		peers := make([]string, 0, 2)
		for _, o := range ids {
			if o != id {
				peers = append(peers, o)
			}
		}
		cfg := raft.DefaultConfig(id, peers, seed)
		sm := kv.NewStateMachine()
		r, err := raft.New(cfg, c.net.Transport(id), sm, raft.NewMemStorage(), logger)
		if err != nil {
			c.Close()
			return nil, err
		}
		srv := kv.NewServer(kv.Options{NodeID: id, ApplyWait: 2 * time.Second, Logger: logger}, r, sm)
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			c.Close()
			return nil, fmt.Errorf("listen: %w", err)
		}
		hs := &http.Server{Handler: srv.Handler(), ReadHeaderTimeout: 5 * time.Second}
		go func() { _ = hs.Serve(ln) }()

		c.rafts[id] = r
		c.srvs = append(c.srvs, hs)
		c.lns = append(c.lns, ln)
		c.nodes = append(c.nodes, nodeRef{ID: id, ClientURL: "http://" + ln.Addr().String()})
		c.net.Register(id, r)
	}
	for _, id := range ids {
		c.rafts[id].Start()
	}
	return c, nil
}

func (c *inprocController) Nodes() []nodeRef { return c.nodes }

func (c *inprocController) CutPeers(id string) error {
	c.net.Isolate(id)
	c.cutIDs[id] = true
	return nil
}

func (c *inprocController) HealPeers(id string) error {
	c.net.Rejoin(id)
	delete(c.cutIDs, id)
	return nil
}

func (c *inprocController) SupportsPause() bool { return false }

// Pause is unimplementable in-process: a goroutine cannot be SIGSTOPped, and
// faking it by gating the raft loop while leaving the HTTP handlers running
// would model something that does not exist. The docker mode is the authority
// on proc.pause.
func (c *inprocController) Pause(string) error {
	return errors.New("proc.pause is not available in-process; run with --mode docker")
}

func (c *inprocController) Unpause(string) error { return c.Pause("") }

func (c *inprocController) ResidualFaults() (int, error) { return c.net.CutCount(), nil }

func (c *inprocController) Close() {
	for _, r := range c.rafts {
		r.Stop()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	for _, s := range c.srvs {
		_ = s.Shutdown(ctx)
	}
	for _, l := range c.lns {
		_ = l.Close()
	}
}

// ---------------------------------------------------------------------- docker

type dockerController struct {
	nodes   []nodeRef
	peerNet string
	aliasOf func(string) string
	paused  map[string]bool
	cut     map[string]bool
	verbose bool
}

func newDockerController(nodes []nodeRef, peerNet string, verbose bool) (*dockerController, error) {
	if err := dockerAvailable(); err != nil {
		return nil, err
	}
	return &dockerController{
		nodes:   nodes,
		peerNet: peerNet,
		aliasOf: func(id string) string { return strings.TrimPrefix(id, "kv-") + "-peer" },
		paused:  map[string]bool{},
		cut:     map[string]bool{},
		verbose: verbose,
	}, nil
}

func dockerAvailable() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "version", "--format", "{{.Server.Version}}").CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker daemon unreachable: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (c *dockerController) run(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", args...)
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if c.verbose {
		fmt.Fprintf(logWriter, "docker %s -> %q (err=%v)\n", strings.Join(args, " "), text, err)
	}
	if err != nil {
		return text, fmt.Errorf("docker %s: %v: %s", strings.Join(args, " "), err, text)
	}
	return text, nil
}

func (c *dockerController) container(id string) (string, error) {
	for _, n := range c.nodes {
		if n.ID == id {
			if n.Container == "" {
				return "", fmt.Errorf("no container name known for node %s", id)
			}
			return n.Container, nil
		}
	}
	return "", fmt.Errorf("unknown node %s", id)
}

func (c *dockerController) Nodes() []nodeRef { return c.nodes }

// CutPeers detaches the container from the peer network. The client network is
// untouched, so the published port and every health probe keep working.
func (c *dockerController) CutPeers(id string) error {
	name, err := c.container(id)
	if err != nil {
		return err
	}
	if _, err := c.run("network", "disconnect", c.peerNet, name); err != nil {
		return err
	}
	c.cut[id] = true
	return nil
}

func (c *dockerController) HealPeers(id string) error {
	name, err := c.container(id)
	if err != nil {
		return err
	}
	if _, err := c.run("network", "connect", "--alias", c.aliasOf(id), c.peerNet, name); err != nil {
		return err
	}
	delete(c.cut, id)
	return nil
}

func (c *dockerController) SupportsPause() bool { return true }

func (c *dockerController) Pause(id string) error {
	name, err := c.container(id)
	if err != nil {
		return err
	}
	if _, err := c.run("pause", name); err != nil {
		return err
	}
	c.paused[id] = true
	return nil
}

func (c *dockerController) Unpause(id string) error {
	name, err := c.container(id)
	if err != nil {
		return err
	}
	if _, err := c.run("unpause", name); err != nil {
		return err
	}
	delete(c.paused, id)
	return nil
}

// ResidualFaults counts what HEAL must find at zero: paused containers and
// containers still detached from the peer network.
func (c *dockerController) ResidualFaults() (int, error) {
	n := 0
	for _, node := range c.nodes {
		if node.Container == "" {
			continue
		}
		state, err := c.run("inspect", "-f", "{{.State.Status}}", node.Container)
		if err != nil {
			return 0, err
		}
		if state == "paused" {
			n++
		}
		nets, err := c.run("inspect", "-f", "{{range $k,$v := .NetworkSettings.Networks}}{{$k}} {{end}}", node.Container)
		if err != nil {
			return 0, err
		}
		if !strings.Contains(nets, c.peerNet) {
			n++
		}
	}
	return n, nil
}

// Close undoes every fault it applied. `docker compose down` on a paused
// container is a known hazard, so unpausing first is not optional.
func (c *dockerController) Close() {
	for id := range c.paused {
		_ = c.Unpause(id)
	}
	for id := range c.cut {
		_ = c.HealPeers(id)
	}
}
