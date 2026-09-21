// Command loadgen is the fixture's workload driver. It emits the normative
// PRO-THESIS history log (JSONL) and nothing else.
//
// Normative invocation (the frozen driver.cmd template):
//
//	./bin/loadgen --history {history_path} --seed {seed} --profile {profile}
//
// It writes ONLY operation records. Phase markers are the harness's business
// (two writers appending to one file across a bind mount gives torn lines, and
// one torn line makes the whole history unparseable).
//
// # Two modes
//
// PROFILE MODE is the default and is unchanged: the profile name resolves to
// clients/ops/keys/mix and every operation is drawn from the seeded PRNG.
//
// PLAN MODE is entered when {plan_path} (or its PROTHESIS_PLAN_PATH fallback,
// DECISIONS.md D-021) names a plan CARRYING AN OPERATION TRACE. The generator
// then draws nothing: it executes the operations it was given, in order, on the
// logical clients they were recorded on, with their recorded op ids, keys,
// values, read modes and offsets. That is what Phase 5's `ddmin` over the
// operation trace needs, and no frozen placeholder can express it: {seed}
// regenerates the whole stream and {profile} passes only a name (OQ-012).
//
// What plan mode does NOT change is how an operation is EXECUTED or RECORDED.
// Every operation still goes through the same doRead/doWrite/doTxn/doAdmin path,
// the same retry rule, the same history.Classify. The ok/fail/info soundness
// property is therefore structural rather than re-argued: there is only one
// execution path and plan mode chooses its arguments.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"prothesis.dev/kvfixture/internal/client"
	"prothesis.dev/kvfixture/internal/history"
	"prothesis.dev/kvfixture/internal/kv"
	"prothesis.dev/kvfixture/internal/plan"
	"prothesis.dev/kvfixture/internal/workload"
)

// Exit codes mirror the normative CLI contract, so a supervisor can tell a bad
// configuration (5) from a driver that could not run (2).
const (
	exitOK          = 0
	exitInconclusiv = 2
	exitConfig      = 5
)

func main() {
	// -version is handled before run(): a driver under the harness is never
	// passed this flag, so only a human asking by hand sees the stamp.
	for _, a := range os.Args[1:] {
		if a == "-version" || a == "--version" {
			fmt.Printf("loadgen build: commit %s dirty=%s source_date=%s\n",
				stamp(buildCommit), stamp(buildDirty), stamp(buildSourceDate))
			os.Exit(0)
		}
	}
	code, err := run(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "loadgen: %v\n", err)
	}
	os.Exit(code)
}

// Build provenance, stamped by scripts/build.ps1 via -ldflags -X. The fixture
// is a separate module and cannot import the harness's internal/buildinfo, so
// it carries its own three vars with the same contract: empty means unstamped,
// never an error. buildSourceDate is the commit's date, not a build clock, for
// the reason D-072 gives.
var (
	buildCommit     string
	buildDirty      string
	buildSourceDate string
)

func stamp(s string) string {
	if s == "" {
		return "unstamped"
	}
	return s
}

type options struct {
	historyPath string
	seed        uint64
	profileName string
	targets     []string

	clients  int
	ops      int
	keys     int
	duration time.Duration

	readTimeout  time.Duration
	writeTimeout time.Duration
	txnTimeout   time.Duration
	connTimeout  time.Duration
	thinkMaxMS   int
	leaseReadPPT int

	stdinControl bool
	quiet        bool

	// planPath is {plan_path} / $PROTHESIS_PLAN_PATH.
	planPath string
	// planPace issues each planned operation at its recorded at_ms offset.
	//
	// Off by default. A shrink candidate is run to find out WHETHER it still
	// reproduces, and faithful pacing makes that answer take as long as the
	// original world did, which is the cost stage 2 exists to avoid. Pacing is
	// for reproducing a timing-sensitive anomaly, not for the search.
	planPace bool
}

func run(args []string) (int, error) {
	// The pacing anchor, captured as early as this process can capture
	// anything. The harness writes the DRIVE phase marker and then execs the
	// driver, so this instant is the closest thing the driver has to DRIVE
	// start: which is what a plan's at_ms offsets, and a fault schedule's
	// windows, are both measured from.
	t0 := time.Now()

	fs := flag.NewFlagSet("loadgen", flag.ContinueOnError)
	var o options
	fs.StringVar(&o.historyPath, "history", "", "path to the history JSONL to write (normative)")
	seed := fs.Uint64("seed", 0, "world seed (normative)")
	fs.StringVar(&o.profileName, "profile", "", "driver profile NAME (normative)")
	// The default targets come from PROTHESIS_TARGETS when the harness set it.
	//
	// The frozen driver.cmd template carries {history_path}, {seed} and
	// {profile} and nothing that can differ per world, so a harness running
	// several worlds at once has no way to tell each driver which cluster is
	// its own. Without this, every concurrent world's driver addresses
	// 127.0.0.1:18081; one of them talks to somebody else's cluster and the
	// rest talk to nothing, and a world that drove nothing still produces a
	// history that looks clean. An explicit --targets still wins.
	//
	// Same dual-transport shape as --stdin-control below.
	defaultTargets := "127.0.0.1:18081,127.0.0.1:18082,127.0.0.1:18083"
	if t := strings.TrimSpace(os.Getenv("PROTHESIS_TARGETS")); t != "" {
		defaultTargets = t
	}
	targets := fs.String("targets", defaultTargets,
		"comma-separated client-plane targets (host:port or full URL); "+
			"defaults to $PROTHESIS_TARGETS when set")
	fs.IntVar(&o.clients, "clients", 0, "override the profile's client count")
	fs.IntVar(&o.ops, "ops", 0, "override the profile's operation budget")
	fs.IntVar(&o.keys, "keys", 0, "override the profile's key space")
	fs.DurationVar(&o.duration, "duration", 0, "stop after this long (0 = run the op budget)")
	fs.DurationVar(&o.readTimeout, "read-timeout", 500*time.Millisecond, "per-read deadline")
	fs.DurationVar(&o.writeTimeout, "write-timeout", time.Second, "per-write deadline")
	fs.DurationVar(&o.txnTimeout, "txn-timeout", 1500*time.Millisecond, "per-transaction deadline")
	fs.DurationVar(&o.connTimeout, "connect-timeout", 200*time.Millisecond, "dial deadline")
	fs.IntVar(&o.thinkMaxMS, "think-max-ms", 2, "maximum per-client think time")
	fs.IntVar(&o.leaseReadPPT, "lease-read-ppt", 900,
		"parts per thousand of reads issued with the default (lease) consistency")
	fs.BoolVar(&o.stdinControl, "stdin-control", os.Getenv("PROTHESIS_STDIN_CONTROL") == "1",
		`stop on stdin EOF or a {"cmd":"stop"} line`)
	fs.BoolVar(&o.quiet, "quiet", false, "suppress the run banner on stderr")
	// PLAN MODE. The dual transport is D-021: {plan_path} in a driver.cmd that
	// spells it, $PROTHESIS_PLAN_PATH for one that does not. An explicit flag
	// wins, exactly as --targets does over $PROTHESIS_TARGETS.
	fs.StringVar(&o.planPath, "plan", os.Getenv(plan.EnvPath),
		"driver plan to execute; defaults to $"+plan.EnvPath)
	fs.BoolVar(&o.planPace, "plan-pace", os.Getenv("PROTHESIS_PLAN_PACE") == "1",
		"issue each planned operation at its recorded at_ms offset")
	if err := fs.Parse(args); err != nil {
		return exitConfig, err
	}
	o.seed = *seed

	if o.historyPath == "" {
		return exitConfig, errors.New("--history is required")
	}
	if o.profileName == "" {
		return exitConfig, errors.New("--profile is required")
	}
	for _, t := range strings.Split(*targets, ",") {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		if !strings.HasPrefix(t, "http://") && !strings.HasPrefix(t, "https://") {
			t = "http://" + t
		}
		o.targets = append(o.targets, strings.TrimRight(t, "/"))
	}
	if len(o.targets) == 0 {
		return exitConfig, errors.New("--targets resolved to nothing")
	}

	prof, err := workload.Resolve(o.profileName, os.Getenv)
	if err != nil {
		return exitConfig, err
	}
	if o.clients > 0 {
		prof.Clients, prof.Source = o.clients, "flags"
	}
	if o.ops > 0 {
		prof.Ops, prof.Source = o.ops, "flags"
	}
	if o.keys > 0 {
		prof.Keys, prof.Source = o.keys, "flags"
	}
	if err := prof.Validate(); err != nil {
		return exitConfig, err
	}

	// The plan is loaded BEFORE the history writer, so a plan this driver cannot
	// replay exits 5 without having created a file. A zero-length history is
	// indistinguishable from a run that drove nothing and passed.
	//
	// HasOperations is the discriminator, never the path's existence: the
	// harness has exported PROTHESIS_PLAN_PATH pointing at a PROFILE plan on
	// every run since Phase 1, and treating that as "a trace to replay" would
	// execute nothing and write a history a checker calls clean. See
	// internal/plan's package doc.
	var pl *plan.Plan
	if o.planPath != "" {
		loaded, err := plan.Load(o.planPath)
		if err != nil {
			return exitConfig, fmt.Errorf("plan: %w", err)
		}
		if loaded.HasOperations() {
			if n, have := len(loaded.Targets), len(o.targets); n > have {
				return exitConfig, fmt.Errorf(
					"plan: the trace was recorded against %d target(s) and this driver has %d. "+
						"Op.Target is a POSITION, not an address, so there is no honest mapping; "+
						"remapping anyway would drive a different cluster than the trace names",
					n, have)
			}
			pl = loaded
		}
	}

	w, err := history.NewWriter(o.historyPath)
	if err != nil {
		return exitInconclusiv, err
	}

	if !o.quiet {
		if pl != nil {
			fmt.Fprintf(os.Stderr, "loadgen: PLAN MODE seed=%d plan=%s operations=%d "+
				"processes=%d pace=%v history=%s\n",
				o.seed, o.planPath, len(pl.Operations), len(pl.Processes()), o.planPace, o.historyPath)
		} else {
			fmt.Fprintf(os.Stderr, "loadgen: seed=%d profile=%s targets=%v history=%s\n",
				o.seed, prof, o.targets, o.historyPath)
		}
	}

	g := &generator{
		opts:    o,
		prof:    prof,
		w:       w,
		c:       client.New(o.connTimeout, prof.Clients*2),
		stop:    make(chan struct{}),
		started: time.Now(),
		t0:      t0,
		plan:    pl,
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		select {
		case <-sigCh:
			g.requestStop()
		case <-g.stop:
		}
	}()
	if o.stdinControl {
		go g.watchStdin()
	}
	if o.duration > 0 {
		go func() {
			t := time.NewTimer(o.duration)
			defer t.Stop()
			select {
			case <-t.C:
				g.requestStop()
			case <-g.stop:
			}
		}()
	}

	g.discover()
	g.runClients()

	// The buffered tail of a history is not optional: losing it reads
	// downstream as a truncated run rather than as a driver bug.
	if err := w.Close(); err != nil {
		return exitInconclusiv, fmt.Errorf("closing history: %w", err)
	}
	if !o.quiet {
		fmt.Fprintf(os.Stderr, "loadgen: done records=%d ok=%d fail=%d info=%d elapsed=%s\n",
			w.Count(), g.nOK.Load(), g.nFail.Load(), g.nInfo.Load(),
			time.Since(g.started).Round(time.Millisecond))
	}
	return exitOK, nil
}

// -------------------------------------------------------------- the generator

type generator struct {
	opts    options
	prof    workload.Profile
	w       *history.Writer
	c       *client.Client
	started time.Time

	// t0 is the pacing anchor: the earliest instant this process could observe,
	// captured before flag parsing. A plan's at_ms offsets are measured from the
	// harness's DRIVE marker, and this is the closest thing the driver has to it.
	t0 time.Time
	// plan is the trace to replay, or nil for profile mode.
	plan *plan.Plan

	// byNode is written once, before any client goroutine starts, and read
	// concurrently thereafter.
	byNode map[string]string

	stopOnce sync.Once
	stop     chan struct{}

	nextOpID atomic.Int64
	issued   atomic.Int64
	nOK      atomic.Int64
	nFail    atomic.Int64
	nInfo    atomic.Int64
}

func (g *generator) requestStop() { g.stopOnce.Do(func() { close(g.stop) }) }

func (g *generator) stopped() bool {
	select {
	case <-g.stop:
		return true
	default:
		return false
	}
}

func (g *generator) watchStdin() {
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		var msg struct {
			Cmd string `json:"cmd"`
		}
		if err := json.Unmarshal(sc.Bytes(), &msg); err == nil && msg.Cmd == "stop" {
			break
		}
	}
	g.requestStop()
}

func (g *generator) runClients() {
	if g.plan != nil {
		g.runPlan()
		return
	}
	var wg sync.WaitGroup
	for i := 0; i < g.prof.Clients; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			g.clientLoop(idx)
		}(i)
	}
	wg.Wait()
}

// runPlan executes a recorded trace instead of generating one.
//
// One goroutine per RECORDED process, each running its own operations in plan
// order. Both halves matter: a process is a sequential thread with one operation
// in flight, so collapsing two processes into one (or spreading one across two)
// changes which interleavings were possible and therefore which histories are
// linearizable. A replay that got that wrong could reproduce an anomaly the
// original run could not have produced, which is worse than not reproducing.
//
// Nothing here is generated. No PRNG is consulted, the profile's client count
// and op budget are ignored, and every op id is the one the original run
// recorded: the verdict's witness names op ids, so a replay that renumbered
// them could not be cross-checked against the violation it is meant to
// reproduce.
func (g *generator) runPlan() {
	var wg sync.WaitGroup
	for pid, ops := range g.plan.ByProcess() {
		wg.Add(1)
		go func(process int, ops []plan.Op) {
			defer wg.Done()
			g.planLoop(process, ops)
		}(pid, ops)
	}
	wg.Wait()
}

func (g *generator) planLoop(process int, ops []plan.Op) {
	for _, op := range ops {
		if g.stopped() {
			return
		}
		if g.opts.planPace {
			if d := time.Until(g.t0.Add(time.Duration(op.AtMS) * time.Millisecond)); d > 0 {
				select {
				case <-time.After(d):
				case <-g.stop:
					return
				}
			}
		}
		g.execPlanned(process, op)
	}
}

// execPlanned dispatches ONE recorded operation onto the same execution path a
// generated one takes. Plan mode chooses arguments; it does not add a second way
// to perform an operation, so the ok/fail/info soundness property is structural
// rather than re-argued.
func (g *generator) execPlanned(process int, op plan.Op) {
	target := g.plannedTarget(op)
	switch op.F {
	case plan.FRead:
		mode := op.ReadMode
		if mode == "" {
			mode = "lease"
		}
		g.doRead(process, op.OpID, target, op.Key, mode)
	case plan.FWrite:
		var v int64
		if err := json.Unmarshal(op.Value, &v); err != nil {
			// Unreachable: Plan.Validate already decoded this. Stopping rather
			// than skipping, because a trace executed in part is not the trace
			// the minimizer is reasoning about.
			fmt.Fprintf(os.Stderr, "loadgen: plan op_id %d: bad write value: %v\n", op.OpID, err)
			g.requestStop()
			return
		}
		g.doWrite(process, op.OpID, target, op.Key, v)
	case plan.FTxn:
		var ops []kv.TxnOp
		if err := json.Unmarshal(op.Value, &ops); err != nil {
			fmt.Fprintf(os.Stderr, "loadgen: plan op_id %d: bad txn ops: %v\n", op.OpID, err)
			g.requestStop()
			return
		}
		g.doTxnOps(process, op.OpID, target, ops)
	case plan.FAdmin:
		g.doAdmin(process, op.OpID, target)
	}
}

// plannedTarget maps a recorded target onto this driver's own list BY POSITION.
//
// An absolute URL recorded in one world addresses a different cluster on replay:
// under parallel execution every world publishes its own host port (D-043), so
// replaying the recorded address would drive somebody else's cluster or nothing
// at all, and a world that drove nothing still writes a history that looks
// clean. run() has already refused a plan with more positions than this driver
// has targets, so the index is in range.
func (g *generator) plannedTarget(op plan.Op) string {
	if op.Target == "" {
		return g.opts.targets[0]
	}
	if i := g.plan.Position(op.Target); i >= 0 && i < len(g.opts.targets) {
		return g.opts.targets[i]
	}
	return g.opts.targets[0]
}

// splitmix64 stream, written in-repo so a client's op sequence is reproducible
// across Go versions.
type stream struct{ s uint64 }

func (r *stream) next() uint64 {
	r.s += 0x9E3779B97F4A7C15
	z := r.s
	z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
	z = (z ^ (z >> 27)) * 0x94D049BB133111EB
	return z ^ (z >> 31)
}

func (r *stream) intn(n int) int {
	if n <= 1 {
		return 0
	}
	return int(r.next() % uint64(n))
}

func (g *generator) clientLoop(idx int) {
	// Each client is a Jepsen-style logical process with exactly ONE operation
	// in flight. Strict per-client sequentiality is what makes a history
	// tractable for a linearizability checker.
	rng := &stream{s: g.opts.seed ^ (uint64(idx+1) * 0x9E3779B97F4A7C15)}
	seq := int64(0)

	for {
		if g.stopped() {
			return
		}
		if g.issued.Add(1) > int64(g.prof.Ops) {
			return
		}
		seq++
		target := g.opts.targets[rng.intn(len(g.opts.targets))]
		key := fmt.Sprintf("k/%d", rng.intn(g.prof.Keys))
		class := g.prof.Pick(rng.intn(g.prof.Mix.Total()))
		opID := g.nextOpID.Add(1)

		switch class {
		case "read":
			mode := "lease"
			if rng.intn(1000) >= g.opts.leaseReadPPT {
				mode = "linearizable"
			}
			g.doRead(idx, opID, target, key, mode)
		case "write":
			// Globally unique values: a read returning V identifies exactly
			// which write produced it, so every stale read is attributable.
			value := int64(idx)*1_000_000 + seq
			g.doWrite(idx, opID, target, key, value)
		case "txn":
			readKey := fmt.Sprintf("k/%d", rng.intn(g.prof.Keys))
			value := int64(idx)*1_000_000 + seq
			g.doTxn(idx, opID, target, readKey, key, value)
		default:
			g.doAdmin(idx, opID, target)
		}

		if g.opts.thinkMaxMS > 0 {
			time.Sleep(time.Duration(rng.intn(g.opts.thinkMaxMS+1)) * time.Millisecond)
		}
	}
}

func (g *generator) append(rec history.Record) {
	if err := g.w.Append(rec); err != nil {
		fmt.Fprintf(os.Stderr, "loadgen: history append failed: %v\n", err)
		g.requestStop()
	}
}

func (g *generator) count(kind string) {
	switch kind {
	case history.TypeOK:
		g.nOK.Add(1)
	case history.TypeFail:
		g.nFail.Add(1)
	default:
		g.nInfo.Add(1)
	}
}

// maxAttempts bounds leader-hint following.
const maxAttempts = 3

// targetOf maps a node identity to its client-plane URL, learned at startup.
func (g *generator) targetOf(nodeID string) (string, bool) {
	if nodeID == "" {
		return "", false
	}
	t, ok := g.byNode[nodeID]
	return t, ok
}

// discover learns which node answers on which target, so a NOT_LEADER response
// can be redirected.
func (g *generator) discover() {
	g.byNode = map[string]string{}
	for _, t := range g.opts.targets {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		var st kv.Status
		res := g.c.Do(ctx, http.MethodGet, t+"/status", nil, &st)
		cancel()
		if res.Attempt.Err == nil && res.Attempt.Status == http.StatusOK && st.Node != "" {
			g.byNode[st.Node] = t
		}
	}
}

// perform executes one LOGICAL operation, following the server's leader hint
// after a definite non-execution.
//
// The retry rule is a soundness rule, not a convenience. Retrying is admissible
// only when the previous attempt carried applied:false -- positive evidence the
// command never reached the state machine. An INDETERMINATE outcome must never
// be retried under the same op_id: that would put two possible executions
// behind one operation and make the history unsound in exactly the way a
// mis-classified timeout does.
func (g *generator) perform(timeout time.Duration, start string,
	call func(ctx context.Context, target string) client.Result,
) (client.Result, history.Outcome, string, int) {
	target := start
	for attempt := 1; ; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		res := call(ctx, target)
		cancel()
		outcome := history.Classify(res.Attempt)
		if outcome.Type == history.TypeFail && res.Code == "NOT_LEADER" && attempt < maxAttempts {
			if next, ok := g.targetOf(res.LeaderHint); ok && next != target {
				target = next
				continue
			}
		}
		return res, outcome, target, attempt
	}
}

func (g *generator) doRead(process int, opID int64, target, key, mode string) {
	suffix := "/kv/" + key
	if mode == "linearizable" {
		suffix += "?consistency=linearizable"
	}
	g.append(history.Record{
		TNS: time.Now().UnixNano(), Process: &process, Type: history.TypeInvoke,
		F: "read", Key: key, OpID: opID,
		Meta: &history.Meta{Target: target, ReadMode: mode, Seq: g.w.NextSeq()},
	})

	var out kv.ReadResponse
	res, outcome, served, attempts := g.perform(g.opts.readTimeout, target,
		func(ctx context.Context, t string) client.Result {
			out = kv.ReadResponse{}
			return g.c.Do(ctx, http.MethodGet, t+suffix, nil, &out)
		})

	rec := history.Record{
		TNS: res.EndNS, Process: &process, Type: outcome.Type,
		F: "read", Key: key, Error: outcome.Error, OpID: opID,
		Meta: &history.Meta{
			Target: served, ReadMode: mode, LatencyUS: res.Latency.Microseconds(),
			Status: res.Attempt.Status, Code: res.Code, Attempt: attempts, Seq: g.w.NextSeq(),
		},
	}
	if outcome.Type == history.TypeOK {
		if out.Found && out.Value != nil {
			rec.Value = history.RawInt64(*out.Value)
		} else {
			rec.Value = history.RawNull()
		}
		rec.Meta.Node = out.ServedBy
		rec.Meta.ServedBy = out.ServedBy
		rec.Meta.Term = out.Term
		rec.Meta.Commit = out.CommitIndex
		rec.Meta.ReadMode = out.ReadMode
		rec.Meta.WriteOpID = out.WriteOpID
		rec.Meta.Index = out.WriteIndex
	}
	g.append(rec)
	g.count(outcome.Type)
}

func (g *generator) doWrite(process int, opID int64, target, key string, value int64) {
	g.append(history.Record{
		TNS: time.Now().UnixNano(), Process: &process, Type: history.TypeInvoke,
		F: "write", Key: key, Value: history.RawInt64(value), OpID: opID,
		Meta: &history.Meta{Target: target, Seq: g.w.NextSeq()},
	})

	var out kv.WriteResponse
	res, outcome, served, attempts := g.perform(g.opts.writeTimeout, target,
		func(ctx context.Context, t string) client.Result {
			out = kv.WriteResponse{}
			return g.c.Do(ctx, http.MethodPut, t+"/kv/"+key,
				map[string]any{"value": value, "op_id": opID}, &out)
		})

	rec := history.Record{
		TNS: res.EndNS, Process: &process, Type: outcome.Type,
		F: "write", Key: key, Value: history.RawInt64(value), Error: outcome.Error, OpID: opID,
		Meta: &history.Meta{
			Target: served, LatencyUS: res.Latency.Microseconds(),
			Status: res.Attempt.Status, Code: res.Code, Attempt: attempts, Seq: g.w.NextSeq(),
		},
	}
	if outcome.Type == history.TypeOK {
		rec.Meta.Node = out.ServedBy
		rec.Meta.Term = out.Term
		rec.Meta.Index = out.Index
	}
	g.append(rec)
	g.count(outcome.Type)
}

// doTxn is the GENERATED form: read one key, write another.
//
// It builds the op list and hands it to doTxnOps, which is the only place a
// transaction is actually performed. Plan mode replays a recorded op list
// through the same function rather than reconstructing these two arguments from
// it: a trace can carry any transaction shape, and decomposing it back into
// readKey/writeKey would quietly rewrite the ones that do not fit.
func (g *generator) doTxn(process int, opID int64, target, readKey, writeKey string, value int64) {
	g.doTxnOps(process, opID, target, []kv.TxnOp{
		{F: "r", Key: readKey},
		{F: "w", Key: writeKey, Value: &value},
	})
}

func (g *generator) doTxnOps(process int, opID int64, target string, ops []kv.TxnOp) {
	invokeVal, err := history.RawAny(ops)
	if err != nil {
		return
	}
	g.append(history.Record{
		TNS: time.Now().UnixNano(), Process: &process, Type: history.TypeInvoke,
		F: "txn", Value: invokeVal, OpID: opID,
		Meta: &history.Meta{Target: target, Seq: g.w.NextSeq()},
	})

	var out kv.TxnResponse
	res, outcome, served, attempts := g.perform(g.opts.txnTimeout, target,
		func(ctx context.Context, t string) client.Result {
			out = kv.TxnResponse{}
			return g.c.Do(ctx, http.MethodPost, t+"/txn",
				map[string]any{"op_id": opID, "ops": ops}, &out)
		})

	rec := history.Record{
		TNS: res.EndNS, Process: &process, Type: outcome.Type,
		F: "txn", Value: invokeVal, Error: outcome.Error, OpID: opID,
		Meta: &history.Meta{
			Target: served, LatencyUS: res.Latency.Microseconds(),
			Status: res.Attempt.Status, Code: res.Code, Attempt: attempts, Seq: g.w.NextSeq(),
		},
	}
	if outcome.Type == history.TypeOK {
		if done, err := history.RawAny(out.Ops); err == nil {
			rec.Value = done
		}
		rec.Meta.Node = out.ServedBy
		rec.Meta.Term = out.Term
		rec.Meta.Index = out.Index
	}
	g.append(rec)
	g.count(outcome.Type)
}

func (g *generator) doAdmin(process int, opID int64, target string) {
	g.append(history.Record{
		TNS: time.Now().UnixNano(), Process: &process, Type: history.TypeInvoke,
		F: "admin", OpID: opID,
		Meta: &history.Meta{Target: target, Seq: g.w.NextSeq()},
	})

	res, outcome, served, attempts := g.perform(g.opts.writeTimeout, target,
		func(ctx context.Context, t string) client.Result {
			return g.c.Do(ctx, http.MethodPost, t+"/admin/noop", map[string]any{"op_id": opID}, nil)
		})

	g.append(history.Record{
		TNS: res.EndNS, Process: &process, Type: outcome.Type,
		F: "admin", Error: outcome.Error, OpID: opID,
		Meta: &history.Meta{
			Target: served, LatencyUS: res.Latency.Microseconds(),
			Status: res.Attempt.Status, Code: res.Code, Attempt: attempts, Seq: g.w.NextSeq(),
		},
	})
	g.count(outcome.Type)
}
