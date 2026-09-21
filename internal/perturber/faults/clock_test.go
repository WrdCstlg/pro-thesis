package faults

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/WrdCstlg/pro-thesis/internal/recorder"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// ---------------------------------------------------------------------------
// The two binding constraints, asserted rather than commented.
// ---------------------------------------------------------------------------

// util-linux `unshare` parses --monotonic as WHOLE SECONDS: `--monotonic=1.5` is
// rejected outright. An offset that truncates to zero would therefore be a fault
// that never fires, and a fault that never fires reporting success is the worst
// outcome this tool can produce. It must be refused, not rounded.
func TestClockRefusesAnOffsetThatTruncatesToZero(t *testing.T) {
	for _, ms := range []int64{1, 400, -999} {
		spec := mustFault(t, fmtSpec("clock.skew(kv-n1, ms=%d)@8200..15100", ms))
		_, err := NewClockFault(PrimitiveRequest{
			Env: Env{RunID: "r"}, Spec: spec, FaultID: "f1",
			Targets: []Target{{NodeID: "kv-n1", ContainerID: "c1", ComposeService: "kv-n1"}},
		})
		if err == nil {
			t.Fatalf("ms=%d must be refused: it truncates to a 0s namespace offset", ms)
		}
		if !IsUnsupported(err) {
			t.Fatalf("ms=%d: want ErrUnsupported (exit 2 INCONCLUSIVE), got %v", ms, err)
		}
	}
}

// A non-zero truncation is applied, and the APPLIED value (not the requested
// one) is what the realized record carries. A record naming the request would
// make the world file a record of an intention rather than of an event.
func TestClockRecordsTheAppliedOffsetNotTheRequestedOne(t *testing.T) {
	p, err := NewClockFault(PrimitiveRequest{
		Env:     Env{RunID: "r"},
		Spec:    mustFault(t, "clock.skew(kv-n2, ms=5500)@8200..15100"),
		FaultID: "f1",
		Targets: []Target{{NodeID: "kv-n2", ContainerID: "c2", ComposeService: "kv-n2"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	cs := p.(*ClockShift)
	if cs.RequestedOffsetMS() != 5500 {
		t.Fatalf("requested = %d", cs.RequestedOffsetMS())
	}
	if cs.AppliedOffsetMS() != 5000 {
		t.Fatalf("applied = %d, want 5000 (whole seconds only)", cs.AppliedOffsetMS())
	}
	got := cs.appliedResolved([]string{"kv-n2"})
	if got != "clock.skew(kv-n2, ms=5000)@8200..15100" {
		t.Fatalf("resolved = %q; it must carry the APPLIED offset", got)
	}
	if _, err := schema.ParseFault(got); err != nil {
		t.Fatalf("a resolved string must parse back through the frozen grammar: %v", err)
	}
}

func TestClockRefusesZero(t *testing.T) {
	_, err := NewClockFault(PrimitiveRequest{
		Env: Env{RunID: "r"}, Spec: mustFault(t, "clock.jump(kv-n1, ms=0)@8200..8200"),
		FaultID: "f1", Targets: []Target{{NodeID: "kv-n1", ContainerID: "c1"}},
	})
	if err == nil {
		t.Fatal("ms=0 is not a clock fault")
	}
}

// D-024 names CLOCK_MONOTONIC and CLOCK_BOOTTIME as the clocks a time namespace
// moves. Shifting one without the other produces a clock pair no real machine
// exhibits, which is a false-positive generator for anything that cross-checks
// them.
func TestClockUnshareArgvSetsBothClocksAndPreservesArgv(t *testing.T) {
	argv := clockUnshareArgv(3, []string{"/kv", "serve"})
	want := []string{
		"unshare", "--time", "--monotonic=3", "--boottime=3",
		"--fork", "--pid", "--mount-proc", "/kv", "serve",
	}
	if strings.Join(argv, " ") != strings.Join(want, " ") {
		t.Fatalf("argv = %v\nwant %v", argv, want)
	}
	if !clockArgvIsSkewed(argv) {
		t.Fatal("the entrypoint this package installs must be recognisable as its own, or HEAL " +
			"cannot tell a skewed container from an ordinary one")
	}
}

func TestClockArgvIsSkewedIgnoresUnrelatedEntrypoints(t *testing.T) {
	cases := [][]string{
		nil,
		{"/kv", "serve"},
		{"/bin/sh", "-c", "unshare --time"},   // the word appears, but not as argv[0]
		{"unshare", "--pid", "--fork", "/kv"}, // an unshare that is not ours
	}
	for _, argv := range cases {
		if clockArgvIsSkewed(argv) {
			t.Fatalf("%v must not be read as a PRO-THESIS time namespace", argv)
		}
	}
	if !clockArgvIsSkewed([]string{"/usr/bin/unshare", "--time", "--monotonic=1", "/kv"}) {
		t.Fatal("an absolute path to unshare is still ours")
	}
}

// A target's argv can contain spaces, colons and quotes. Emitting it as a JSON
// array (a valid YAML flow sequence) is what keeps a fault injector from
// becoming an injection bug.
func TestClockOverlayQuotesArgvSafely(t *testing.T) {
	body, err := clockOverlayYAML("kv-n1", "thesis:r1:f1", 3,
		[]string{"/bin/app", "--flag", `a "quoted" value`, "k: v"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, `"a \"quoted\" value"`) {
		t.Fatalf("argv was not escaped:\n%s", body)
	}
	if !strings.Contains(body, `"k: v"`) {
		t.Fatalf("a value containing a colon was not quoted:\n%s", body)
	}
	if !strings.Contains(body, `cap_add: ["SYS_ADMIN", "SYS_TIME"]`) {
		t.Fatalf("both capabilities are required; either alone fails with EPERM:\n%s", body)
	}
	if !strings.Contains(body, "thesis:r1:f1") {
		t.Fatalf("the fragment must name its owner:\n%s", body)
	}
	if !strings.Contains(body, "  kv-n1:\n") {
		t.Fatalf("the fragment must scope to one service:\n%s", body)
	}
}

// The project name and base file set must match what `up` used, or compose
// resolves a DIFFERENT project and recreates nothing while reporting success.
func TestClockComposeArgsMatchTheProjectUpCreated(t *testing.T) {
	got := strings.Join(clockComposeArgs("thesis-kvfixture-abcd", "docker-compose.yaml",
		"/run/overlay.yaml", "/tmp/clock.yaml", "kv-n1"), " ")
	want := "compose --project-name thesis-kvfixture-abcd " +
		"--file docker-compose.yaml --file /run/overlay.yaml --file /tmp/clock.yaml " +
		"up --detach --force-recreate --no-deps kv-n1"
	if got != want {
		t.Fatalf("got  %q\nwant %q", got, want)
	}

	// Withdrawal is the same invocation WITHOUT the clock fragment: dropping the
	// override is what restores the original entrypoint, so nothing has to be
	// remembered about what the entrypoint was.
	got = strings.Join(clockComposeArgs("p", "compose.yaml", "", "", "svc"), " ")
	want = "compose --project-name p --file compose.yaml up --detach --force-recreate --no-deps svc"
	if got != want {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
}

func TestParseTimensOffset(t *testing.T) {
	out := "monotonic        3600         0\nboottime         3600         0\n"
	v, ok := parseTimensOffset(out)
	if !ok || v != 3600 {
		t.Fatalf("got %d, %v", v, ok)
	}
	if _, ok := parseTimensOffset("boottime 1 0\n"); ok {
		t.Fatal("a file with no monotonic row must report ok=false rather than a default")
	}
}

// OQ-020: a clock fault is only implementable as a restart into a skewed
// namespace, so it must record BOTH. A world file showing only the skew would
// misdescribe what happened, and no_crash would have no window excusing the
// restart's process exit.
func TestClockInjectionWithoutAComposeProjectIsRefused(t *testing.T) {
	p, err := NewClockFault(PrimitiveRequest{
		Env:     Env{RunID: "r"}, // no Topology
		Spec:    mustFault(t, "clock.skew(kv-n1, ms=3000)@8200..15100"),
		FaultID: "f1",
		Targets: []Target{{NodeID: "kv-n1", ContainerID: "c1", ComposeService: "kv-n1"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	err = p.Inject(context.Background())
	if err == nil || !IsUnsupported(err) {
		t.Fatalf("without the compose handoff a clock fault cannot restart anything; want "+
			"ErrUnsupported, got %v", err)
	}
	if len(p.Records()) != 0 {
		t.Fatal("a fault that did not fire must record nothing")
	}
}

// The three-record shape, asserted against the record builder directly so it is
// pinned without a live daemon.
func TestClockRecordsRestartAndSkewTogether(t *testing.T) {
	p, err := NewClockFault(PrimitiveRequest{
		Env:     Env{RunID: "r"},
		Spec:    mustFault(t, "clock.skew(kv-n2, ms=3000)@8200..15100"),
		FaultID: "f1",
		Targets: []Target{{NodeID: "kv-n2", ContainerID: "c2", ComposeService: "kv-n2"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	cs := p.(*ClockShift)
	cs.note(schema.FaultProcRestart, []string{"kv-n2"}, 100, 200)
	cs.noteOpenResolved(schema.FaultClockSkew, []string{"kv-n2"}, cs.appliedResolved([]string{"kv-n2"}), 100)
	cs.closeWindow(schema.FaultClockSkew, 900)
	cs.note(schema.FaultProcRestart, []string{"kv-n2"}, 900, 1000)

	recs := cs.Records()
	if len(recs) != 3 {
		t.Fatalf("want restart + skew + restart, got %d records", len(recs))
	}
	if recs[0].Kind != schema.FaultProcRestart || recs[2].Kind != schema.FaultProcRestart {
		t.Fatalf("the two restarts OQ-020 forces must both be recorded: %+v", recs)
	}
	if recs[1].Kind != schema.FaultClockSkew || recs[1].EndWallNS != 900 || recs[1].Open {
		t.Fatalf("the skew's window was not closed: %+v", recs[1])
	}

	tl := &recorder.Timeline{}
	if err := tl.StartDriveAt(recorder.RTime(0), 0); err != nil {
		t.Fatal(err)
	}
	windows := OracleFaultWindows(recs, tl)
	if len(windows) != 3 {
		t.Fatalf("all three events must reach no_crash's planned windows, got %d", len(windows))
	}
	restarts := 0
	for _, w := range windows {
		if w.Kind == string(schema.FaultProcRestart) {
			restarts++
			if !w.Names("kv-n2") {
				t.Fatalf("a window that names no concrete node excuses nothing: %+v", w)
			}
		}
	}
	if restarts != 2 {
		t.Fatalf("want 2 restart windows excusing the two container recreates, got %d", restarts)
	}
}

func fmtSpec(format string, a ...any) string {
	return fmt.Sprintf(format, a...)
}
