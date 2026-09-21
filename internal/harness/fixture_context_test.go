package harness

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The reference fixture's build context must not contain its own run corpus.
//
// The fixture's Dockerfile is `COPY . .` and compose builds the image before
// every world (`pull_policy: build`, which the variant lock depends on). Every
// run writes a directory under `.prothesis/runs/`, so with the corpus inside
// the context every build's COPY layer misses the cache: measured in OQ-068,
// 2.38 GB of a 2.40 GB context was run output, one new file in it cost +5.4 GB
// of build cache for one build, and a 50 GB Docker cap is nine worlds away.
// Nothing under `.prothesis/` is needed to compile the fixture.
//
// Mutation: delete the `.prothesis/` line from the fixture's .dockerignore and
// this fails.
func TestTheFixtureBuildContextExcludesItsOwnRunCorpus(t *testing.T) {
	path := filepath.Join("..", "..", "testdata", "kvfixture", ".dockerignore")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "!") {
			continue
		}
		// .dockerignore patterns are rooted at the context; `**/` and a
		// trailing `/` or `/**` are spellings of the same directory exclusion.
		p := strings.TrimPrefix(line, "/")
		p = strings.TrimPrefix(p, "**/")
		p = strings.TrimSuffix(p, "/**")
		p = strings.TrimSuffix(p, "/")
		if p == ".prothesis" {
			return
		}
	}
	t.Fatalf("%s does not exclude .prothesis/: every run's artifacts enter the next build's context, "+
		"the COPY layer misses the cache on every world, and the build cache grows by gigabytes per "+
		"world (OQ-068)", path)
}
