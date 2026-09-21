package control

import (
	"context"

	"github.com/WrdCstlg/pro-thesis/internal/harness"
	"github.com/WrdCstlg/pro-thesis/internal/recorder"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// dockerImageResolver is the production ImageResolver: harness.ResolveImages,
// which asks the daemon. It is the default NewRunner installs when the caller
// supplies none, so `thesis run` records provenance without being told to, and
// a test that wants no daemon has to say so: see stubImages in the tests.
//
// The time bound lives in harness with the call it bounds (D-073); this type
// adds nothing to it.
type dockerImageResolver struct{}

func (dockerImageResolver) Resolve(ctx context.Context, nodes []recorder.NodeBinding, cfg []schema.NodeConfig) ([]schema.SUTImage, error) {
	return harness.ResolveImages(ctx, nodes, cfg)
}
