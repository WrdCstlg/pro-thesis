package corpus

import (
	"path/filepath"
	"testing"

	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// The committed regression corpus is evidence recorded under the world format
// of its day. A change to how worlds are identified must leave every committed
// entry loadable under the name it was committed with: a v1 world keeps its v1
// identity (D-109), so its filename short id still matches its content hash.
func TestTheCommittedRegressionCorpusStillResolves(t *testing.T) {
	c := OpenDir(filepath.Join("..", "..", "testdata", "kvfixture", ".prothesis", "regressions"))
	entries, err := c.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("no committed regression worlds found; this test guards them and must not pass vacuously")
	}
	for _, e := range entries {
		if !e.OK() {
			t.Errorf("%s no longer resolves: %v", e.Name, e.Err)
			continue
		}
		if e.World.Schema != schema.WorldSchemaV1 && e.World.Schema != schema.WorldSchema {
			t.Errorf("%s declares schema %q", e.Name, e.World.Schema)
		}
	}
}
