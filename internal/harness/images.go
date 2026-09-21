package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/WrdCstlg/pro-thesis/internal/recorder"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// imageResolveTimeout bounds one `docker inspect` made for provenance. A
// variable so a test can shorten it.
//
// WHY A BOUND OF ITS OWN (OQ-067, D-073). ResolveImages is fail-soft in its
// ERROR (a failure costs the world its provenance and never its execution)
// but until this bound it was handed the world's own context, so in its TIME it
// was fail-soft only up to the whole wall budget. Measured on the build host
// after a disk-full event corrupted the Docker content store: `docker inspect`
// on a NONEXISTENT container took 32 s to return a 500, three such calls ran a
// 60 s profile budget out, and two passing tests failed BUDGET_EXHAUSTED for a
// metadata lookup the workload never made. Provenance is not worth the budget.
// Five seconds is generous for one inspect against a healthy daemon (measured
// 44–57 ms over five samples on the build host, two orders of magnitude inside
// the bound) and exceeding it must look exactly like any other resolution
// failure: [] plus a warning that names the cause.
var imageResolveTimeout = 5 * time.Second

// inspectRun is the docker invocation ResolveImages makes. A variable so a test
// can stand in a daemon that never answers: the failure OQ-067 measured, which
// no real process on the test host can be asked to reproduce on demand.
var inspectRun = run

// ResolveImages resolves every bound node's container to the image it is
// actually running, closing OQ-055: a world file that cannot name the build
// it reproduced against leaves a future green `thesis regress` ambiguous
// between "the defect is fixed" and "this ran against a different build".
//
// The answer is taken from `docker inspect` on the bound containers (one
// call, N args, the same pattern the harness already relies on) because the
// container's `.Image` is the resolved image ID, a content address no tag
// drift can fake. A tag (`kvfixture:buggy`) is a name; the ID is the thing.
//
// The logical service is reported (the fault grammar's group, e.g. "kv"), not
// the per-node compose service: three nodes sharing one image produce ONE
// entry, and a world running a mixed fleet (one node patched, two buggy)
// produces one entry per distinct (service, image, digest), which is exactly
// the fact a regression needs.
//
// The call is bounded by imageResolveTimeout regardless of what ctx allows, and
// the error names which side ended it: our bound ("did not answer within") or
// the caller's own context ("aborted by the caller's context"). Without that,
// both arrive as docker's wrap of a killed child ("signal: killed") and the
// world's warning would blame the daemon for a deadline the harness set.
func ResolveImages(ctx context.Context, nodes []recorder.NodeBinding, cfg []schema.NodeConfig) ([]schema.SUTImage, error) {
	ids := make([]string, 0, len(nodes))
	for _, n := range nodes {
		if n.ContainerID != "" {
			ids = append(ids, n.ContainerID)
		}
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("no bound containers to inspect")
	}
	args := append([]string{"inspect"}, ids...)

	bctx, cancel := context.WithTimeout(ctx, imageResolveTimeout)
	defer cancel()
	out, err := inspectRun(bctx, "", args...)
	if err != nil {
		// Our bound fired, not the caller's: the parent is still live.
		if errors.Is(bctx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
			return nil, fmt.Errorf("docker inspect did not answer within %s: %w", imageResolveTimeout, err)
		}
		// The caller's context ended it. Say so: docker's own wrap of a killed
		// child reads like a daemon failure, and the world's warning would
		// otherwise blame the daemon for a deadline the harness set.
		// %w on the context error, not on docker's: a caller can then classify
		// the abort with errors.Is, which docker's exec wrap would never allow.
		if cerr := ctx.Err(); cerr != nil {
			return nil, fmt.Errorf("docker inspect aborted by the caller's context (%w): %v", cerr, err)
		}
		return nil, err
	}
	return parseInspectImages(out, nodes, cfg)
}

// inspectImageDoc is the subset of `docker inspect` ResolveImages reads.
// Unknown fields are ignored, per the harness's CLI-containment rule: the
// inspect schema is old and stable, but never parsed more deeply than needed.
type inspectImageDoc struct {
	ID     string `json:"Id"`
	Image  string `json:"Image"` // resolved image ID, "sha256:..."
	Config struct {
		Image string `json:"Image"` // the reference as written, e.g. "prothesis/kvfixture:buggy"
	} `json:"Config"`
}

// parseInspectImages folds inspect output into deduplicated SUT image
// entries. Pure, so the mapping is testable without a daemon.
func parseInspectImages(out string, nodes []recorder.NodeBinding, cfg []schema.NodeConfig) ([]schema.SUTImage, error) {
	trimmed := strings.TrimSpace(out)
	if trimmed == "" {
		return nil, fmt.Errorf("docker inspect printed nothing")
	}
	var docs []inspectImageDoc
	if strings.HasPrefix(trimmed, "[") {
		if err := json.Unmarshal([]byte(trimmed), &docs); err != nil {
			return nil, fmt.Errorf("parse `docker inspect` array: %w", err)
		}
	} else {
		for _, line := range strings.Split(trimmed, "\n") {
			if line = strings.TrimSpace(line); line == "" {
				continue
			}
			var d inspectImageDoc
			if err := json.Unmarshal([]byte(line), &d); err != nil {
				return nil, fmt.Errorf("parse `docker inspect` line: %w", err)
			}
			docs = append(docs, d)
		}
	}

	logical := map[string]string{}
	for _, n := range cfg {
		logical[n.ID] = n.Service
	}

	seen := map[schema.SUTImage]bool{}
	var images []schema.SUTImage
	for _, n := range nodes {
		if n.ContainerID == "" {
			continue
		}
		var match *inspectImageDoc
		for i := range docs {
			// compose ps reports the full ID on current compose, but match by
			// prefix either way: a short ID is a prefix of the long one.
			if strings.HasPrefix(docs[i].ID, n.ContainerID) || strings.HasPrefix(n.ContainerID, docs[i].ID) {
				match = &docs[i]
				break
			}
		}
		if match == nil {
			return nil, fmt.Errorf("container %s (node %s) absent from docker inspect output", n.ContainerID, n.ID)
		}
		svc := logical[n.ID]
		if svc == "" {
			svc = n.Service
		}
		img := schema.SUTImage{Service: svc, Image: match.Config.Image, Digest: match.Image}
		if !seen[img] {
			seen[img] = true
			images = append(images, img)
		}
	}
	return images, nil
}
