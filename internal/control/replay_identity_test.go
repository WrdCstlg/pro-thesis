package control

import (
	"context"
	"testing"

	"github.com/WrdCstlg/pro-thesis/internal/driver"
	"github.com/WrdCstlg/pro-thesis/internal/recorder"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// A replay asks for a world by identity, and the identity includes the images
// it ran on and the workload it drove (D-109). Booted on another image, the
// world is not that world: it can neither reproduce nor clear it.

func replayRunner(t *testing.T, images ImageResolver) *Runner {
	t.Helper()
	r, err := NewRunner(RunnerOptions{
		Config:     perturbTestConfig(t),
		ProjectDir: t.TempDir(),
		Backend: &stubBackend{nodes: []recorder.NodeBinding{
			{ID: "kv-n1", ContainerID: "c1", HostPort: 18081, ContainerPort: 8080, Service: "kv-n1"},
			{ID: "kv-n2", ContainerID: "c2", HostPort: 18082, ContainerPort: 8080, Service: "kv-n2"},
			{ID: "kv-n3", ContainerID: "c3", HostPort: 18083, ContainerPort: 8080, Service: "kv-n3"},
		}},
		Driver:        finishedDriver(),
		Telemetry:     stubTelemetry{},
		OracleEngine:  stubOracles{},
		LogCollector:  stubLogs{},
		ImageResolver: images,
		Quiet:         true,
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	return r
}

func TestAReplayIsInconclusiveOnAnyOtherImage(t *testing.T) {
	recorded := []schema.SUTImage{{Service: "kv", Image: "kv:fixed", Digest: "sha256:aaaa"}}
	cases := []struct {
		name    string
		running ImageResolver
		want    Outcome
	}{
		{"the recorded image", &recordingImages{fleet: recorded}, OutcomePass},
		{"another digest", &recordingImages{fleet: []schema.SUTImage{{Service: "kv", Image: "kv:fixed", Digest: "sha256:bbbb"}}}, OutcomeInconclusive},
		{"an extra image", &recordingImages{fleet: append(append([]schema.SUTImage(nil), recorded...),
			schema.SUTImage{Service: "kv", Image: "kv:fixed", Digest: "sha256:cccc"})}, OutcomeInconclusive},
		{"images not resolvable", stubImages{}, OutcomeInconclusive},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := replayRunner(t, tc.running)
			out := r.RunWorld(context.Background(), WorldRequest{Ordinal: 1, ExpectImages: recorded})
			if out.Outcome != tc.want {
				t.Fatalf("outcome %q, want %q", out.Outcome, tc.want)
			}
		})
	}
}

// A replay hands down the recorded plan and the world drives it as recorded:
// the replayed world's plan_digest is the recorded one.
func TestAReplayDrivesTheRecordedPlan(t *testing.T) {
	r := replayRunner(t, stubImages{})
	seed := uint64(4242)
	recorded := &driver.Plan{
		Schema: driver.PlanSchema, Profile: "smoke", Seed: seed, Clients: 1, Ops: 7,
		Mix: []driver.MixEntry{{Op: "read", WeightPPM: 1000000}},
	}
	want, err := recorded.WorkloadDigest()
	if err != nil {
		t.Fatal(err)
	}
	out := r.RunWorld(context.Background(), WorldRequest{Ordinal: 1, Seed: &seed, Plan: recorded})
	w, err := recorder.LoadWorld(out.Paths.World)
	if err != nil {
		t.Fatalf("load world: %v", err)
	}
	if w.PlanDigest != want {
		t.Fatalf("the replayed world drove workload %s, want the recorded %s", w.PlanDigest, want)
	}
}
