package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/WrdCstlg/pro-thesis/internal/corpus"
	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// OQ-062 / D-063. A regression corpus with a member that cannot be replayed is
// refused outright, before a run bundle exists. Running the intact members and
// reporting on them would be a verdict about a corpus the command has just
// proved it does not fully hold, and before D-063 a misnamed file was not even
// counted, so a corpus of nothing but misnamed files was "empty" and exit 0.
func TestRegressRefusesACorpusWithAMemberItCannotReplay(t *testing.T) {
	g := cliProject(t)
	dir := projectDirOf(g)

	regDir := corpus.Dir(dir)
	if err := os.MkdirAll(regDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Claims to be a world; is not one. Before the fix this file was invisible.
	if err := os.WriteFile(filepath.Join(regDir, "w_bad.thesis"), []byte("dummy\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	code := cmdRegress(context.Background(), g, nil)
	if code == schema.ExitPass {
		t.Fatal("a corpus whose only member cannot be replayed produced exit 0: a vacuous pass " +
			"over a regression suite that has silently stopped existing")
	}
	if code != schema.ExitInconclusive {
		t.Fatalf("exit %d, want %d (INCONCLUSIVE): an unreadable corpus is not evidence in either "+
			"direction", code, schema.ExitInconclusive)
	}

	// Refused means REFUSED: no run bundle may exist claiming something ran.
	runs := filepath.Join(dir, ".prothesis", "runs")
	if ents, err := os.ReadDir(runs); err == nil && len(ents) != 0 {
		names := make([]string, 0, len(ents))
		for _, e := range ents {
			names = append(names, e.Name())
		}
		t.Fatalf("a refused regress still allocated a run bundle: %v", names)
	}
}
