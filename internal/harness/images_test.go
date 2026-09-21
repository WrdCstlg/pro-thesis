package harness

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/WrdCstlg/pro-thesis/internal/recorder"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

func TestParseInspectImagesDedupesSharedImage(t *testing.T) {
	out := `[
	  {"Id":"aaa111","Image":"sha256:deadbeef","Config":{"Image":"prothesis/kvfixture:buggy"}},
	  {"Id":"bbb222","Image":"sha256:deadbeef","Config":{"Image":"prothesis/kvfixture:buggy"}},
	  {"Id":"ccc333","Image":"sha256:deadbeef","Config":{"Image":"prothesis/kvfixture:buggy"}}
	]`
	nodes := []recorder.NodeBinding{
		{ID: "kv-n1", Service: "kv-n1", ContainerID: "aaa111"},
		{ID: "kv-n2", Service: "kv-n2", ContainerID: "bbb222"},
		{ID: "kv-n3", Service: "kv-n3", ContainerID: "ccc333"},
	}
	cfg := []schema.NodeConfig{
		{ID: "kv-n1", Service: "kv"},
		{ID: "kv-n2", Service: "kv"},
		{ID: "kv-n3", Service: "kv"},
	}
	images, err := parseInspectImages(out, nodes, cfg)
	if err != nil {
		t.Fatalf("parseInspectImages: %v", err)
	}
	if len(images) != 1 {
		t.Fatalf("three nodes on one image should dedupe to one entry, got %d: %+v", len(images), images)
	}
	want := schema.SUTImage{Service: "kv", Image: "prothesis/kvfixture:buggy", Digest: "sha256:deadbeef"}
	if images[0] != want {
		t.Errorf("images[0] = %+v, want %+v", images[0], want)
	}
}

func TestParseInspectImagesMixedFleetKeepsBoth(t *testing.T) {
	// The case sut.images exists for: one patched node in a buggy fleet.
	out := `[
	  {"Id":"aaa111","Image":"sha256:fixed","Config":{"Image":"prothesis/kvfixture:kvfixed"}},
	  {"Id":"bbb222","Image":"sha256:buggy","Config":{"Image":"prothesis/kvfixture:buggy"}}
	]`
	nodes := []recorder.NodeBinding{
		{ID: "kv-n1", Service: "kv-n1", ContainerID: "aaa111"},
		{ID: "kv-n2", Service: "kv-n2", ContainerID: "bbb222"},
	}
	cfg := []schema.NodeConfig{{ID: "kv-n1", Service: "kv"}, {ID: "kv-n2", Service: "kv"}}
	images, err := parseInspectImages(out, nodes, cfg)
	if err != nil {
		t.Fatalf("parseInspectImages: %v", err)
	}
	if len(images) != 2 {
		t.Fatalf("mixed fleet must keep both digests, got %+v", images)
	}
}

func TestParseInspectImagesNDJSON(t *testing.T) {
	out := "{\"Id\":\"aaa111\",\"Image\":\"sha256:x\",\"Config\":{\"Image\":\"img:1\"}}\n" +
		"{\"Id\":\"bbb222\",\"Image\":\"sha256:x\",\"Config\":{\"Image\":\"img:1\"}}\n"
	nodes := []recorder.NodeBinding{
		{ID: "n1", Service: "n1", ContainerID: "aaa111"},
		{ID: "n2", Service: "n2", ContainerID: "bbb222"},
	}
	cfg := []schema.NodeConfig{{ID: "n1", Service: "kv"}, {ID: "n2", Service: "kv"}}
	images, err := parseInspectImages(out, nodes, cfg)
	if err != nil {
		t.Fatalf("parseInspectImages: %v", err)
	}
	if len(images) != 1 || images[0].Digest != "sha256:x" {
		t.Fatalf("ndjson form misparsed: %+v", images)
	}
}

func TestParseInspectImagesServiceFallsBackToBinding(t *testing.T) {
	out := `[{"Id":"aaa111","Image":"sha256:x","Config":{"Image":"img:1"}}]`
	nodes := []recorder.NodeBinding{{ID: "n1", Service: "n1", ContainerID: "aaa111"}}
	images, err := parseInspectImages(out, nodes, nil)
	if err != nil {
		t.Fatalf("parseInspectImages: %v", err)
	}
	// No cfg mapping: fall back to the binding's own service name.
	if len(images) != 1 || images[0].Service != "n1" {
		t.Errorf("images = %+v, want one entry with service %q", images, "n1")
	}
}

func TestParseInspectImagesMissingContainerIsAnError(t *testing.T) {
	out := `[{"Id":"aaa111","Image":"sha256:x","Config":{"Image":"img:1"}}]`
	nodes := []recorder.NodeBinding{
		{ID: "n1", Service: "n1", ContainerID: "aaa111"},
		{ID: "n2", Service: "n2", ContainerID: "zzz999"},
	}
	if _, err := parseInspectImages(out, nodes, nil); err == nil {
		t.Fatal("a bound container absent from inspect output must not silently drop its image")
	}
}

// A daemon that never answers must cost the world its provenance, not its
// budget. Before D-073 the call was bounded only by the caller's context, so a
// stand-in daemon that blocks until told to stop kept ResolveImages waiting for
// as long as the caller allowed, and the caller was the world, whose allowance
// was its whole wall budget. That is the failure OQ-067 measured: 32 s per
// inspect, three inspects, one 60 s budget, BUDGET_EXHAUSTED.
//
// The stand-in blocks on ctx.Done, so the ONLY way this returns is the bound
// inside ResolveImages. Mutation: replace that WithTimeout with a plain
// WithCancel (the bound gone, nothing else changed) and this fails on the
// two-second select below rather than hanging, because the caller's context
// here has no deadline at all.
func TestResolveImagesGivesUpOnADaemonThatNeverAnswers(t *testing.T) {
	origRun, origTimeout := inspectRun, imageResolveTimeout
	t.Cleanup(func() { inspectRun, imageResolveTimeout = origRun, origTimeout })

	imageResolveTimeout = 50 * time.Millisecond
	inspectRun = func(ctx context.Context, _ string, _ ...string) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	}

	nodes := []recorder.NodeBinding{{ID: "kv-n1", Service: "kv-n1", ContainerID: "c1"}}
	type result struct {
		images []schema.SUTImage
		err    error
	}
	done := make(chan result, 1)
	go func() {
		imgs, err := ResolveImages(context.Background(), nodes, nil)
		done <- result{imgs, err}
	}()

	select {
	case r := <-done:
		if r.err == nil {
			t.Fatalf("a daemon that never answered produced images %+v and no error", r.images)
		}
		if !strings.Contains(r.err.Error(), "did not answer within") {
			t.Errorf("the error must name the bound so the world's warning says why provenance is missing; got: %v", r.err)
		}
		if r.images != nil {
			t.Errorf("images = %+v, want nil so sut.images records []", r.images)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ResolveImages has not returned: it is waiting on the daemon, bounded by nothing but a caller that set no deadline")
	}
}

// The bound is the contract D-073, OQ-067 and AGENTS.md all state as "five
// seconds". Nothing else pins the number (every other test here shortens it)
// so a silent change to five minutes would otherwise pass the suite.
func TestImageResolveBoundIsFiveSeconds(t *testing.T) {
	if imageResolveTimeout != 5*time.Second {
		t.Fatalf("imageResolveTimeout = %s, want 5s: the ledger documents five seconds and this is the only test that reads the default", imageResolveTimeout)
	}
}

// The bound must be OURS, not the caller's, and the error must say which. A
// caller whose own deadline has already passed gets that deadline's error back
// unadorned: blaming a five-second inspect bound for a budget that ran out for
// other reasons would put a false explanation in the run's warning.
//
// An EXPIRED DEADLINE, not a cancellation, on purpose: a cancelled parent makes
// bctx report Canceled and never enters the timeout branch, so it could not
// tell whether the guard exists. An inherited DeadlineExceeded looks exactly
// like ours from bctx alone; only the parent's own error tells them apart.
// Mutation: drop the `ctx.Err() == nil` guard in ResolveImages and this fails.
func TestResolveImagesReportsTheCallersDeadlineAsTheCallers(t *testing.T) {
	origRun := inspectRun
	t.Cleanup(func() { inspectRun = origRun })
	inspectRun = func(ctx context.Context, _ string, _ ...string) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	}

	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	_, err := ResolveImages(ctx, []recorder.NodeBinding{{ID: "n1", ContainerID: "c1"}}, nil)
	if err == nil {
		t.Fatal("a caller whose deadline had passed must not get images")
	}
	if strings.Contains(err.Error(), "did not answer within") {
		t.Errorf("the caller's own deadline expired, but the error blames the inspect bound: %v", err)
	}
	// And it must say whose deadline it was: docker's own wrap of a killed child
	// is "signal: killed", which reads like a daemon failure. Mutation: drop the
	// caller's-context branch in ResolveImages and this fails.
	if !strings.Contains(err.Error(), "aborted by the caller's context") {
		t.Errorf("the error does not attribute the abort to the caller's context: %v", err)
	}
	// A positive check, so this cannot pass on an unrelated error: the caller's
	// own deadline must come back classifiable, not only described.
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("want the caller's own deadline back through errors.Is, got: %v", err)
	}
}
