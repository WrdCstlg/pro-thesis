// Command provebug demonstrates the fixture's stale-read anomaly end to end
// WITHOUT any part of PRO-THESIS being involved, and writes the golden
// artifacts a consistency oracle can be developed against with zero Docker.
//
// Why this binary exists: a PASS from PRO-THESIS is uninformative unless we
// know independently that the bug is there and what its signature looks like.
//
// Two modes:
//
//	--mode inproc   (default) three real Raft nodes in this process, real HTTP
//	                client planes on loopback, peer plane cut in memory.
//	                Deterministic, ~8s, no Docker.
//	--mode docker   the real compose cluster; peer-plane partition via
//	                `docker network disconnect`, proc.pause via `docker pause`.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"prothesis.dev/kvfixture/internal/client"
	"prothesis.dev/kvfixture/internal/history"
	"prothesis.dev/kvfixture/internal/kv"
	"prothesis.dev/kvfixture/internal/raft"
)

var logWriter io.Writer = os.Stderr

const (
	exitProven       = 0
	exitNotProven    = 1
	exitInconclusive = 2
)

func main() {
	code := run()
	os.Exit(code)
}

type config struct {
	mode        string
	seed        uint64
	targets     string
	containers  string
	peerNet     string
	outDir      string
	scenario    string
	key         string
	valueBefore int64
	valueAfter  int64

	pauseDelay time.Duration
	pauseDur   time.Duration
	readWindow time.Duration
	readEvery  time.Duration
	bootWait   time.Duration

	expectClean bool
	verbose     bool
	keepGolden  bool
}

func run() int {
	var c config
	fs := flag.NewFlagSet("provebug", flag.ContinueOnError)
	fs.StringVar(&c.mode, "mode", "inproc", "inproc | docker")
	seed := fs.Uint64("seed", 20260906, "world seed for the in-process cluster")
	fs.StringVar(&c.targets, "targets", "", "docker mode: comma-separated id=clientURL pairs")
	fs.StringVar(&c.containers, "containers", "", "docker mode: comma-separated id=containerName pairs")
	fs.StringVar(&c.peerNet, "peer-network", "prothesis-kvpeer", "docker mode: peer-plane network name")
	fs.StringVar(&c.outDir, "out", "golden", "directory for history.jsonl, witness.json and telemetry.json")
	fs.StringVar(&c.scenario, "scenario", "partition", "partition | pause  (pause implies partition)")
	fs.StringVar(&c.key, "key", "k/42", "key to exercise")
	fs.Int64Var(&c.valueBefore, "value-before", 7, "value committed before the fault")
	fs.Int64Var(&c.valueAfter, "value-after", 9, "value committed by the new leader")
	fs.DurationVar(&c.pauseDelay, "pause-delay", 200*time.Millisecond, "delay from partition to proc.pause")
	fs.DurationVar(&c.pauseDur, "pause", 2800*time.Millisecond, "proc.pause duration (must be < the lease)")
	fs.DurationVar(&c.readWindow, "read-window", 4500*time.Millisecond, "how long to poll the displaced leader for stale reads")
	fs.DurationVar(&c.readEvery, "read-every", 25*time.Millisecond, "stale-read poll interval")
	fs.DurationVar(&c.bootWait, "boot-wait", 30*time.Second, "how long to wait for a healthy cluster")
	fs.BoolVar(&c.expectClean, "expect-clean", false, "negative control: require NO anomaly (for the kvfixed build)")
	fs.BoolVar(&c.verbose, "v", false, "verbose")
	fs.BoolVar(&c.keepGolden, "write-golden", true, "write the golden artifacts")
	if err := fs.Parse(os.Args[1:]); err != nil {
		return exitInconclusive
	}
	c.seed = *seed

	p := &prover{cfg: c, hc: client.New(2*time.Second, 32)}
	code, err := p.execute()
	if err != nil {
		fmt.Fprintf(os.Stderr, "provebug: %v\n", err)
	}
	return code
}

// ------------------------------------------------------------------ the prover

type prover struct {
	cfg     config
	hc      *client.Client
	ctl     controller
	records []history.Record
	tel     []telemetrySample
	nextOp  int64
	t0      time.Time
}

type telemetrySample struct {
	TNS    int64     `json:"t_ns"`
	Marker string    `json:"marker"`
	Status kv.Status `json:"status"`
}

func (p *prover) logf(format string, args ...any) {
	fmt.Fprintf(logWriter, "provebug: "+format+"\n", args...)
}

func (p *prover) opID() int64 { p.nextOp++; return 90000 + p.nextOp }

func (p *prover) execute() (int, error) {
	var logger *log.Logger
	if p.cfg.verbose {
		logger = log.New(os.Stderr, "node ", log.LstdFlags|log.Lmicroseconds)
	}

	switch p.cfg.mode {
	case "inproc":
		if p.cfg.scenario == "pause" {
			return exitInconclusive, errors.New("--scenario pause requires --mode docker: a goroutine cannot be SIGSTOPped")
		}
		ctl, err := newInprocController(p.cfg.seed, logger)
		if err != nil {
			return exitInconclusive, err
		}
		p.ctl = ctl
	case "docker":
		nodes, err := parseDockerNodes(p.cfg.targets, p.cfg.containers)
		if err != nil {
			return exitInconclusive, err
		}
		ctl, err := newDockerController(nodes, p.cfg.peerNet, p.cfg.verbose)
		if err != nil {
			return exitInconclusive, err
		}
		p.ctl = ctl
	default:
		return exitInconclusive, fmt.Errorf("unknown --mode %q", p.cfg.mode)
	}
	defer p.ctl.Close()

	p.t0 = time.Now()
	result, err := p.scenario()
	if err != nil {
		p.writeArtifacts(result)
		return exitInconclusive, err
	}
	p.writeArtifacts(result)

	p.report(result)

	if p.cfg.expectClean {
		if result.StaleReads > 0 {
			return exitNotProven, fmt.Errorf("NEGATIVE CONTROL FAILED: %d stale reads on a build that must not serve any", result.StaleReads)
		}
		if !result.Converged {
			return exitNotProven, errors.New("NEGATIVE CONTROL FAILED: cluster did not converge after heal")
		}
		return exitProven, nil
	}
	if result.StaleReads == 0 {
		return exitNotProven, errors.New("NO STALE READ OBSERVED: the anomaly this fixture exists to exhibit did not occur")
	}
	if !result.Converged {
		return exitNotProven, errors.New("stale read observed but the cluster did not converge; that is a different, worse bug")
	}
	return exitProven, nil
}

type result struct {
	Mode            string  `json:"mode"`
	Variant         string  `json:"variant"`
	Scenario        string  `json:"scenario"`
	LeaseMS         int64   `json:"lease_ms"`
	ElectionMinMS   int64   `json:"election_min_ms"`
	OldLeader       string  `json:"old_leader"`
	OldTerm         uint64  `json:"old_term"`
	NewLeader       string  `json:"new_leader"`
	NewTerm         uint64  `json:"new_term"`
	WriteOpIDBefore int64   `json:"write_op_id_before"`
	WriteOpIDAfter  int64   `json:"write_op_id_after"`
	StaleOpIDs      []int64 `json:"stale_op_ids"`
	StaleReads      int     `json:"stale_reads"`
	TotalReads      int     `json:"total_reads"`
	InfoReads       int     `json:"info_reads"`
	FailReads       int     `json:"fail_reads"`

	PartitionAtMS   int64 `json:"partition_at_ms"`
	PausedAtMS      int64 `json:"paused_at_ms"`
	ResumedAtMS     int64 `json:"resumed_at_ms"`
	ElectionAtMS    int64 `json:"new_leader_elected_at_ms"`
	NewWriteAtMS    int64 `json:"new_value_committed_at_ms"`
	FirstStaleMS    int64 `json:"first_stale_read_at_ms"`
	LastStaleMS     int64 `json:"last_stale_read_at_ms"`
	StaleWindowMS   int64 `json:"stale_window_ms"`
	LeaseRemainMS   int64 `json:"lease_remaining_ms_at_first_stale"`
	Converged       bool  `json:"converged"`
	ConvergedAtMS   int64 `json:"converged_at_ms"`
	ResidualFaults  int   `json:"residual_faults_after_heal"`
	ConvergedValue  int64 `json:"converged_value"`
	ExpectedNoStale bool  `json:"expected_no_stale"`
}

func (p *prover) ms(t time.Time) int64 { return t.Sub(p.t0).Milliseconds() }

func (p *prover) scenario() (*result, error) {
	res := &result{
		Mode:            p.cfg.mode,
		Variant:         raft.Variant,
		Scenario:        p.cfg.scenario,
		LeaseMS:         raft.LeaseDuration.Milliseconds(),
		ElectionMinMS:   raft.DefaultConfig("x", nil, 0).ElectionMin.Milliseconds(),
		ExpectedNoStale: p.cfg.expectClean,
	}

	nodes := p.ctl.Nodes()

	// 1. Wait for a healthy cluster with exactly one leader.
	leader, term0, err := p.waitLeader(p.cfg.bootWait)
	if err != nil {
		return res, err
	}
	res.OldLeader, res.OldTerm = leader, term0
	p.logf("step 1: cluster healthy, leader=%s term=%d", leader, term0)
	p.sample("boot", leader)

	// 2. Commit a value on the leader and confirm every node applied it.
	w1 := p.opID()
	idx, err := p.write(leader, p.cfg.key, p.cfg.valueBefore, w1)
	if err != nil {
		return res, fmt.Errorf("step 2: initial write: %w", err)
	}
	res.WriteOpIDBefore = w1
	if err := p.waitAllApplied(idx, 10*time.Second); err != nil {
		return res, fmt.Errorf("step 2: replication: %w", err)
	}
	p.logf("step 2: %s=%d committed at index %d and applied on all nodes", p.cfg.key, p.cfg.valueBefore, idx)

	// 3. The leader must actually hold its lease before we start.
	st, err := p.status(leader)
	if err != nil {
		return res, err
	}
	if !st.Lease.Held {
		return res, fmt.Errorf("step 3: leader %s does not hold a lease; nothing to demonstrate", leader)
	}
	p.logf("step 3: %s holds a lease with %dms remaining (lease duration %dms, election min %dms)",
		leader, st.Lease.RemainingMS, res.LeaseMS, res.ElectionMinMS)

	// 4. Cut the leader's PEER plane. Client plane untouched.
	partitionAt := time.Now()
	if err := p.ctl.CutPeers(leader); err != nil {
		return res, fmt.Errorf("step 4: partition: %w", err)
	}
	res.PartitionAtMS = p.ms(partitionAt)
	p.logf("step 4: peer plane of %s cut at t+%dms", leader, res.PartitionAtMS)

	// 5. Optionally freeze the leader for a window SHORTER than its lease.
	//    A pause longer than the lease cannot produce the anomaly: the monotonic
	//    deadline keeps running while the process is stopped, so the lease has
	//    already expired by the time it wakes. That is why the specification's
	//    own 6.9s reference pause against a 5s lease cannot trigger this bug.
	if p.cfg.scenario == "pause" {
		if !p.ctl.SupportsPause() {
			return res, errors.New("step 5: this controller cannot pause a node")
		}
		if p.cfg.pauseDur >= raft.LeaseDuration {
			// Not an error: this is the measurement that shows the reference
			// schedule's 6.9s pause cannot work. Run it and report the result.
			p.logf("step 5: WARNING --pause %s is NOT shorter than the %s lease. "+
				"CLOCK_MONOTONIC keeps advancing while the process is frozen, so the lease "+
				"expires during the pause and no stale read should be observed.",
				p.cfg.pauseDur, raft.LeaseDuration)
		}
		time.Sleep(p.cfg.pauseDelay)
		if err := p.ctl.Pause(leader); err != nil {
			return res, fmt.Errorf("step 5: pause: %w", err)
		}
		res.PausedAtMS = p.ms(time.Now())
		p.logf("step 5: %s paused at t+%dms for %s", leader, res.PausedAtMS, p.cfg.pauseDur)
	}

	// 6. The majority elects a new leader.
	survivors := []string{}
	for _, n := range nodes {
		if n.ID != leader {
			survivors = append(survivors, n.ID)
		}
	}
	newLeader, term1, err := p.waitLeaderAmong(survivors, term0+1, 10*time.Second)
	if err != nil {
		return res, fmt.Errorf("step 6: %w", err)
	}
	res.NewLeader, res.NewTerm = newLeader, term1
	res.ElectionAtMS = p.ms(time.Now())
	p.logf("step 6: %s elected leader in term %d at t+%dms (%dms after the partition)",
		newLeader, term1, res.ElectionAtMS, res.ElectionAtMS-res.PartitionAtMS)

	// 7. The new leader commits a new value. From this instant the old leader's
	//    local state is provably stale.
	w2 := p.opID()
	idx2, err := p.writeRetry(newLeader, p.cfg.key, p.cfg.valueAfter, w2, 5*time.Second)
	if err != nil {
		return res, fmt.Errorf("step 7: write on the new leader: %w", err)
	}
	res.WriteOpIDAfter = w2
	res.NewWriteAtMS = p.ms(time.Now())
	p.logf("step 7: %s=%d committed by %s at index %d, t+%dms", p.cfg.key, p.cfg.valueAfter, newLeader, idx2, res.NewWriteAtMS)
	p.sample("after_new_write", newLeader)

	// 8. If paused, resume BEFORE the lease would have expired.
	if p.cfg.scenario == "pause" {
		resumeAt := partitionAt.Add(p.cfg.pauseDelay + p.cfg.pauseDur)
		if d := time.Until(resumeAt); d > 0 {
			time.Sleep(d)
		}
		if err := p.ctl.Unpause(leader); err != nil {
			return res, fmt.Errorf("step 8: unpause: %w", err)
		}
		res.ResumedAtMS = p.ms(time.Now())
		p.logf("step 8: %s resumed at t+%dms, still partitioned", leader, res.ResumedAtMS)
	}

	// 9. Poll the displaced leader with an ORDINARY read (default consistency).
	windowEnd := time.Now().Add(p.cfg.readWindow)
	var firstStale, lastStale time.Time
	for time.Now().Before(windowEnd) {
		opID := p.opID()
		val, mode, term, outcome := p.read(leader, p.cfg.key, opID)
		res.TotalReads++
		switch outcome {
		case history.TypeOK:
			if val != nil && *val == p.cfg.valueBefore {
				res.StaleReads++
				res.StaleOpIDs = append(res.StaleOpIDs, opID)
				if firstStale.IsZero() {
					firstStale = time.Now()
					res.FirstStaleMS = p.ms(firstStale)
					if st, err := p.status(leader); err == nil {
						res.LeaseRemainMS = st.Lease.RemainingMS
						p.sample("first_stale_read", leader)
					}
					p.logf("step 9: STALE READ at t+%dms -- %s returned %d (read_mode=%s, term=%d) while term %d had committed %d",
						res.FirstStaleMS, leader, *val, mode, term, term1, p.cfg.valueAfter)
				}
				lastStale = time.Now()
				res.LastStaleMS = p.ms(lastStale)
			}
		case history.TypeInfo:
			res.InfoReads++
		case history.TypeFail:
			res.FailReads++
		}
		time.Sleep(p.cfg.readEvery)
	}
	if res.StaleReads > 0 {
		res.StaleWindowMS = res.LastStaleMS - res.FirstStaleMS
		p.logf("step 9: %d stale reads over a %dms window (first t+%dms, last t+%dms); %d info, %d fail",
			res.StaleReads, res.StaleWindowMS, res.FirstStaleMS, res.LastStaleMS, res.InfoReads, res.FailReads)
	} else {
		p.logf("step 9: no stale read observed in %s (%d reads: %d info, %d fail)",
			p.cfg.readWindow, res.TotalReads, res.InfoReads, res.FailReads)
	}
	p.sample("end_of_read_window", leader)

	// 10. Heal, and require the cluster to converge. This is what proves the
	//     anomaly is a STALE READ and not a permanent split brain: the system is
	//     otherwise healthy and the defect is exactly the injected one.
	if err := p.ctl.HealPeers(leader); err != nil {
		return res, fmt.Errorf("step 10: heal: %w", err)
	}
	p.logf("step 10: peer plane restored at t+%dms", p.ms(time.Now()))

	if v, at, err := p.waitConverged(term1, 20*time.Second); err == nil {
		res.Converged = true
		res.ConvergedValue = v
		res.ConvergedAtMS = p.ms(at)
		p.logf("step 11: converged at t+%dms: all nodes agree term=%d leader=%s %s=%d",
			res.ConvergedAtMS, term1, newLeader, p.cfg.key, v)
	} else {
		p.logf("step 11: CONVERGENCE FAILED: %v", err)
	}

	if n, err := p.ctl.ResidualFaults(); err == nil {
		res.ResidualFaults = n
		if n != 0 {
			p.logf("step 11: WARNING %d residual faults after heal", n)
		}
	}
	p.sample("after_heal", newLeader)
	return res, nil
}

func (p *prover) report(r *result) {
	fmt.Fprintf(logWriter, `
=========================== provebug summary ===========================
 build variant          %s   (lease %dms, election min %dms)
 mode / scenario        %s / %s
 displaced leader       %s (term %d)
 new leader             %s (term %d)
 partition at           t+%dms
 pause window           t+%dms .. t+%dms
 new leader elected     t+%dms
 new value committed    t+%dms
 first stale read       t+%dms   (lease believed to have %dms left)
 last stale read        t+%dms
 stale-read window      %dms over %d reads (%d stale, %d info, %d fail)
 converged              %v at t+%dms with %s=%d
 residual faults        %d
========================================================================
`,
		r.Variant, r.LeaseMS, r.ElectionMinMS,
		r.Mode, r.Scenario,
		r.OldLeader, r.OldTerm,
		r.NewLeader, r.NewTerm,
		r.PartitionAtMS,
		r.PausedAtMS, r.ResumedAtMS,
		r.ElectionAtMS,
		r.NewWriteAtMS,
		r.FirstStaleMS, r.LeaseRemainMS,
		r.LastStaleMS,
		r.StaleWindowMS, r.TotalReads, r.StaleReads, r.InfoReads, r.FailReads,
		r.Converged, r.ConvergedAtMS, p.cfg.key, r.ConvergedValue,
		r.ResidualFaults)
}

// ------------------------------------------------------------------- HTTP ops

func (p *prover) urlOf(id string) (string, error) {
	for _, n := range p.ctl.Nodes() {
		if n.ID == id {
			return n.ClientURL, nil
		}
	}
	return "", fmt.Errorf("unknown node %q", id)
}

func (p *prover) status(id string) (kv.Status, error) {
	u, err := p.urlOf(id)
	if err != nil {
		return kv.Status{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var st kv.Status
	res := p.hc.Do(ctx, http.MethodGet, u+"/status", nil, &st)
	if res.Attempt.Err != nil {
		return kv.Status{}, res.Attempt.Err
	}
	if res.Attempt.Status != http.StatusOK {
		return kv.Status{}, fmt.Errorf("%s /status returned %d", id, res.Attempt.Status)
	}
	return st, nil
}

func (p *prover) sample(marker, id string) {
	for _, n := range p.ctl.Nodes() {
		st, err := p.status(n.ID)
		if err != nil {
			st = kv.Status{Node: n.ID, Role: "unreachable"}
		}
		p.tel = append(p.tel, telemetrySample{TNS: time.Now().UnixNano(), Marker: marker, Status: st})
	}
}

func (p *prover) waitLeader(timeout time.Duration) (string, uint64, error) {
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		leaders := []string{}
		terms := map[uint64]bool{}
		ok := true
		for _, n := range p.ctl.Nodes() {
			st, err := p.status(n.ID)
			if err != nil {
				ok = false
				last = fmt.Sprintf("%s unreachable: %v", n.ID, err)
				break
			}
			terms[st.Term] = true
			if st.Role == "leader" {
				leaders = append(leaders, st.Node)
			}
			if st.CommitIndex == 0 {
				ok = false
				last = fmt.Sprintf("%s commit_index=0", n.ID)
			}
		}
		if ok && len(leaders) == 1 && len(terms) == 1 {
			st, err := p.status(leaders[0])
			if err == nil {
				return leaders[0], st.Term, nil
			}
		}
		if ok && len(leaders) != 1 {
			last = fmt.Sprintf("found %d leaders", len(leaders))
		}
		time.Sleep(100 * time.Millisecond)
	}
	return "", 0, fmt.Errorf("no healthy single-leader cluster within %s (%s)", timeout, last)
}

func (p *prover) waitLeaderAmong(ids []string, minTerm uint64, timeout time.Duration) (string, uint64, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, id := range ids {
			st, err := p.status(id)
			if err != nil {
				continue
			}
			if st.Role == "leader" && st.Term >= minTerm {
				return id, st.Term, nil
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return "", 0, fmt.Errorf("no leader with term >= %d among %v within %s", minTerm, ids, timeout)
}

func (p *prover) waitAllApplied(index uint64, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		ok := true
		for _, n := range p.ctl.Nodes() {
			st, err := p.status(n.ID)
			if err != nil || st.LastApplied < index {
				ok = false
			}
		}
		if ok {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("not every node applied index %d within %s", index, timeout)
}

func (p *prover) waitConverged(term uint64, timeout time.Duration) (int64, time.Time, error) {
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		values := map[int64]int{}
		terms := map[uint64]int{}
		ok := true
		for _, n := range p.ctl.Nodes() {
			st, err := p.status(n.ID)
			if err != nil {
				ok, last = false, fmt.Sprintf("%s unreachable", n.ID)
				break
			}
			terms[st.Term]++
			if st.Term < term {
				ok, last = false, fmt.Sprintf("%s still on term %d", n.ID, st.Term)
			}
			v, err := p.dumpValue(n.ID, p.cfg.key)
			if err != nil {
				ok, last = false, fmt.Sprintf("%s dump failed: %v", n.ID, err)
				break
			}
			values[v]++
		}
		if ok && len(values) == 1 && len(terms) == 1 {
			for v := range values {
				return v, time.Now(), nil
			}
		}
		if ok && len(values) != 1 {
			last = fmt.Sprintf("values still disagree: %v", values)
		}
		time.Sleep(100 * time.Millisecond)
	}
	return 0, time.Time{}, fmt.Errorf("no convergence within %s (%s)", timeout, last)
}

func (p *prover) dumpValue(id, key string) (int64, error) {
	u, err := p.urlOf(id)
	if err != nil {
		return 0, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var out struct {
		State map[string]kv.Value `json:"state"`
	}
	res := p.hc.Do(ctx, http.MethodGet, u+"/dump", nil, &out)
	if res.Attempt.Err != nil {
		return 0, res.Attempt.Err
	}
	v, ok := out.State[key]
	if !ok {
		return -1, nil
	}
	return v.Value, nil
}

func (p *prover) write(id, key string, value, opID int64) (uint64, error) {
	u, err := p.urlOf(id)
	if err != nil {
		return 0, err
	}
	proc := 0
	p.records = append(p.records, history.Record{
		TNS: time.Now().UnixNano(), Process: &proc, Type: history.TypeInvoke,
		F: "write", Key: key, Value: history.RawInt64(value), OpID: opID,
		Meta: &history.Meta{Target: id},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var out kv.WriteResponse
	res := p.hc.Do(ctx, http.MethodPut, u+"/kv/"+key, map[string]any{"value": value, "op_id": opID}, &out)
	outcome := history.Classify(res.Attempt)
	rec := history.Record{
		TNS: res.EndNS, Process: &proc, Type: outcome.Type,
		F: "write", Key: key, Value: history.RawInt64(value), Error: outcome.Error, OpID: opID,
		Meta: &history.Meta{Target: id, Status: res.Attempt.Status, Code: res.Code,
			LatencyUS: res.Latency.Microseconds()},
	}
	if outcome.Type == history.TypeOK {
		rec.Meta.Node = out.ServedBy
		rec.Meta.Term = out.Term
		rec.Meta.Index = out.Index
	}
	p.records = append(p.records, rec)
	if outcome.Type != history.TypeOK {
		return 0, fmt.Errorf("write to %s classified %s (%s)", id, outcome.Type, outcome.Error)
	}
	return out.Index, nil
}

func (p *prover) writeRetry(id, key string, value, opID int64, timeout time.Duration) (uint64, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		idx, err := p.write(id, key, value, opID)
		if err == nil {
			return idx, nil
		}
		lastErr = err
		time.Sleep(50 * time.Millisecond)
	}
	return 0, lastErr
}

func (p *prover) read(id, key string, opID int64) (*int64, string, uint64, string) {
	u, err := p.urlOf(id)
	if err != nil {
		return nil, "", 0, history.TypeInfo
	}
	proc := 1
	p.records = append(p.records, history.Record{
		TNS: time.Now().UnixNano(), Process: &proc, Type: history.TypeInvoke,
		F: "read", Key: key, OpID: opID,
		Meta: &history.Meta{Target: id, ReadMode: "lease"},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 800*time.Millisecond)
	defer cancel()
	var out kv.ReadResponse
	res := p.hc.Do(ctx, http.MethodGet, u+"/kv/"+key, nil, &out)
	outcome := history.Classify(res.Attempt)
	rec := history.Record{
		TNS: res.EndNS, Process: &proc, Type: outcome.Type,
		F: "read", Key: key, Error: outcome.Error, OpID: opID,
		Meta: &history.Meta{Target: id, Status: res.Attempt.Status, Code: res.Code,
			LatencyUS: res.Latency.Microseconds(), ReadMode: "lease"},
	}
	var val *int64
	if outcome.Type == history.TypeOK {
		if out.Found && out.Value != nil {
			v := *out.Value
			val = &v
			rec.Value = history.RawInt64(v)
		} else {
			rec.Value = history.RawNull()
		}
		rec.Meta.Node = out.ServedBy
		rec.Meta.ServedBy = out.ServedBy
		rec.Meta.Term = out.Term
		rec.Meta.Commit = out.CommitIndex
		rec.Meta.ReadMode = out.ReadMode
		rec.Meta.WriteOpID = out.WriteOpID
	}
	p.records = append(p.records, rec)
	return val, out.ReadMode, out.Term, outcome.Type
}

// ------------------------------------------------------------------ artifacts

// oracleOutput is prothesis.oracle_output/v1. The `schema` and `valid_phases`
// fields are normative and are present: this file is the golden target Phase 3's
// oracle is written against, so an omission here would propagate into the real
// oracle's output.
type oracleOutput struct {
	Schema      string   `json:"schema"`
	Oracle      string   `json:"oracle"`
	Class       string   `json:"class"`
	ValidPhases []string `json:"valid_phases"`
	Status      string   `json:"status"`
	Witness     witness  `json:"witness"`
	Explanation string   `json:"explanation"`
}

type witness struct {
	OpIDs []int64 `json:"op_ids"`
	Key   string  `json:"key"`
}

func (p *prover) writeArtifacts(r *result) {
	if !p.cfg.keepGolden || r == nil {
		return
	}
	if err := os.MkdirAll(p.cfg.outDir, 0o755); err != nil {
		p.logf("cannot create %s: %v", p.cfg.outDir, err)
		return
	}

	// history.jsonl -- sorted by t_ns, because a history's real-time intervals
	// are its entire meaning and the records were timestamped at the true event
	// instant rather than at append time.
	recs := append([]history.Record(nil), p.records...)
	sort.SliceStable(recs, func(i, j int) bool { return recs[i].TNS < recs[j].TNS })
	hp := filepath.Join(p.cfg.outDir, "history.jsonl")
	w, err := history.NewWriter(hp)
	if err != nil {
		p.logf("cannot write history: %v", err)
	} else {
		for _, rec := range recs {
			if err := w.Append(rec); err != nil {
				p.logf("history append: %v", err)
				break
			}
		}
		if err := w.Close(); err != nil {
			p.logf("history close: %v", err)
		} else {
			p.logf("wrote %s (%d records)", hp, len(recs))
		}
	}

	status := "ok"
	explanation := fmt.Sprintf(
		"no lease read returned a stale value; the %dms lease expires inside the %dms minimum election timeout",
		r.LeaseMS, r.ElectionMinMS)
	ops := []int64{}
	if r.StaleReads > 0 {
		status = "violated"
		ops = append([]int64{r.WriteOpIDBefore, r.WriteOpIDAfter}, r.StaleOpIDs...)
		if len(ops) > 12 {
			ops = ops[:12]
		}
		explanation = fmt.Sprintf(
			"process 1 read %s=%d from %s under a leader read lease at t+%dms, %dms after op_id %d committed %s=%d in term %d on %s. "+
				"The lease is %dms against a %dms minimum election timeout, so the displaced leader kept answering local reads for %dms after it was replaced.",
			p.cfg.key, p.cfg.valueBefore, r.OldLeader, r.FirstStaleMS,
			r.FirstStaleMS-r.NewWriteAtMS, r.WriteOpIDAfter, p.cfg.key, p.cfg.valueAfter, r.NewTerm, r.NewLeader,
			r.LeaseMS, r.ElectionMinMS, r.StaleWindowMS)
	}
	out := oracleOutput{
		Schema:      "prothesis.oracle_output/v1",
		Oracle:      "linearizable.kv",
		Class:       "consistency",
		ValidPhases: []string{"ASSERT"},
		Status:      status,
		Witness:     witness{OpIDs: ops, Key: p.cfg.key},
		Explanation: explanation,
	}
	writeJSONFile(p, filepath.Join(p.cfg.outDir, "witness.json"), out)
	writeJSONFile(p, filepath.Join(p.cfg.outDir, "telemetry.json"), map[string]any{
		"samples": p.tel,
		"summary": r,
	})
}

func writeJSONFile(p *prover, path string, v any) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		p.logf("marshal %s: %v", path, err)
		return
	}
	b = append(b, '\n')
	if err := os.WriteFile(path, b, 0o644); err != nil {
		p.logf("write %s: %v", path, err)
		return
	}
	p.logf("wrote %s", path)
}

// --------------------------------------------------------------------- parsing

func parseDockerNodes(targets, containers string) ([]nodeRef, error) {
	if targets == "" {
		return nil, errors.New("--targets is required in docker mode, e.g. kv-n1=http://127.0.0.1:18081,...")
	}
	byID := map[string]*nodeRef{}
	order := []string{}
	for _, part := range strings.Split(targets, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		id, url, ok := strings.Cut(part, "=")
		if !ok {
			return nil, fmt.Errorf("malformed --targets entry %q, want ID=URL", part)
		}
		if !strings.HasPrefix(url, "http") {
			url = "http://" + url
		}
		byID[id] = &nodeRef{ID: id, ClientURL: strings.TrimRight(url, "/")}
		order = append(order, id)
	}
	for _, part := range strings.Split(containers, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		id, name, ok := strings.Cut(part, "=")
		if !ok {
			return nil, fmt.Errorf("malformed --containers entry %q, want ID=NAME", part)
		}
		n, ok := byID[id]
		if !ok {
			return nil, fmt.Errorf("--containers names %q which is not in --targets", id)
		}
		n.Container = name
	}
	out := make([]nodeRef, 0, len(order))
	for _, id := range order {
		n := byID[id]
		if n.Container == "" {
			n.Container = "prothesis-" + id
		}
		out = append(out, *n)
	}
	return out, nil
}
