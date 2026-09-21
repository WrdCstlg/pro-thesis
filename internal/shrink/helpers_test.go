package shrink

import (
	"context"
	"sync"
	"time"

	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// ---------------------------------------------------------------------------
// Test doubles
//
// Every test in this package runs against a PURE predicate, never a container.
// That is the point of the Executor seam: the ddmin algorithm, the confirmation
// asymmetry, the budget and the window narrowing are all pinned here, and a
// change to the harness cannot silently change any of them.
// ---------------------------------------------------------------------------

// origID is the violation every test shrinks toward: the fixture's own finding,
// with the witness from OQ-034's run 8400.
var origID = Identity{
	Oracle:   "linearizable.kv",
	Class:    schema.ClassConsistency,
	Severity: schema.SeverityHigh,
	Key:      "k/0",
	OpIDs:    []int64{11430, 11597, 11602},
}

// sameBugDifferentOps is the SAME defect observed on a second execution: same
// oracle, same class, same key, entirely different op ids. Measured, not
// invented: OQ-034 records both witnesses from two definition-of-done runs.
var sameBugDifferentOps = Identity{
	Oracle:   "linearizable.kv",
	Class:    schema.ClassConsistency,
	Severity: schema.SeverityHigh,
	Key:      "k/0",
	OpIDs:    []int64{11402, 11580, 11578},
}

// sameBugDifferentKey is the SAME defect observed on a key the original witness
// never named: same oracle, same class, different key.
//
// Measured, not invented. 27 recorded linearizable.kv violations of the ONE
// injected fixture defect land on three keys (k/0 x20, k/1 x5, k/2 x2) and two
// runs produced two different keys within a single run. The witness key is a
// scheduling artifact for this target, not part of its identity. See OQ-047.
var sameBugDifferentKey = Identity{
	Oracle:   "linearizable.kv",
	Class:    schema.ClassConsistency,
	Severity: schema.SeverityHigh,
	Key:      "k/2",
	OpIDs:    []int64{3325, 3395, 3416},
}

// aDifferentBug is another oracle entirely. Accepting a reduction because THIS
// fired is the ranked #1 failure mode.
var aDifferentBug = Identity{
	Oracle:   "no_crash",
	Class:    schema.ClassCrash,
	Severity: schema.SeverityHigh,
}

func reproduced() Attempt {
	return Attempt{Reached: true, Observed: []Identity{origID}, WorldPath: "w_shrunk.thesis"}
}

func reproducedAs(id Identity) Attempt {
	return Attempt{Reached: true, Observed: []Identity{id}, WorldPath: "w_shrunk.thesis"}
}

func clean() Attempt {
	return Attempt{Reached: true, WorldPath: "w_shrunk.thesis"}
}

func differentBug() Attempt {
	return Attempt{Reached: true, Observed: []Identity{aDifferentBug}, WorldPath: "w_shrunk.thesis"}
}

func unjudgeable() Attempt {
	return Attempt{Reached: false}
}

// fakeExecutor answers from a pure function of (candidate, how many times this
// candidate has been executed before).
type fakeExecutor struct {
	mu       sync.Mutex
	seen     map[string]int
	log      []Candidate
	inflight int
	peak     int
	delay    time.Duration
	respond  func(c Candidate, nth int) Attempt
	fail     error

	// onCandidate runs before each response, so a test can change what the
	// executor does when the pipeline reaches a particular candidate.
	onCandidate func(c Candidate)
}

func newFake(respond func(c Candidate, nth int) Attempt) *fakeExecutor {
	return &fakeExecutor{seen: map[string]int{}, respond: respond}
}

func (f *fakeExecutor) Execute(ctx context.Context, c Candidate) (Attempt, error) {
	if f.fail != nil {
		return Attempt{}, f.fail
	}
	f.mu.Lock()
	f.seen[c.key()]++
	nth := f.seen[c.key()]
	f.log = append(f.log, c)
	f.inflight++
	if f.inflight > f.peak {
		f.peak = f.inflight
	}
	f.mu.Unlock()

	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	if f.onCandidate != nil {
		f.onCandidate(c)
	}
	a := f.respond(c, nth)

	f.mu.Lock()
	f.inflight--
	f.mu.Unlock()
	return a, nil
}

func (f *fakeExecutor) total() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.log)
}

func (f *fakeExecutor) peakInflight() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.peak
}

// batchExecutor adds real concurrency, so the parallel path is exercised rather
// than merely configured.
type batchExecutor struct{ *fakeExecutor }

func (b batchExecutor) ExecuteBatch(ctx context.Context, cs []Candidate) ([]Attempt, error) {
	out := make([]Attempt, len(cs))
	errs := make([]error, len(cs))
	var wg sync.WaitGroup
	for i := range cs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			out[i], errs[i] = b.Execute(ctx, cs[i])
		}(i)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// predicate helpers
// ---------------------------------------------------------------------------

func hasFault(c Candidate, want string) bool {
	for _, f := range c.Faults {
		if f == want {
			return true
		}
	}
	return false
}

// hasOp treats a nil Ops as "the whole workload", which is what a nil Ops means
// to a real executor.
func hasOp(c Candidate, all []int64, want int64) bool {
	if c.Ops == nil {
		for _, op := range all {
			if op == want {
				return true
			}
		}
		return false
	}
	for _, op := range c.Ops {
		if op == want {
			return true
		}
	}
	return false
}

// stepClock advances by step every time it is read, so a wall budget can be
// exhausted deterministically without sleeping.
type stepClock struct {
	mu   sync.Mutex
	t    time.Time
	step time.Duration
}

func newStepClock(step time.Duration) *stepClock {
	return &stepClock{t: time.Unix(1_700_000_000, 0), step: step}
}

func (c *stepClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.t
	c.t = c.t.Add(c.step)
	return out
}

// faultList builds n distinct, parseable faults on distinct windows.
func faultList(n int) []string {
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		start := int64(1000 + i*1000)
		f := schema.FaultSpec{
			Kind:    schema.FaultProcPause,
			Target:  schema.Target{Kind: schema.TargetNode, Node: "kv-n1"},
			StartMS: start,
			EndMS:   start + 500,
		}
		spec, err := schema.ParseFault(f.String())
		if err != nil {
			panic(err)
		}
		out = append(out, spec.String())
	}
	return out
}
