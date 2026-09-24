package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

const twoPairedOps = `{"t_ns":1789442762673427900,"process":0,"type":"invoke","f":"write","key":"k/1","op_id":1}
{"t_ns":1789442762673427901,"process":0,"type":"ok","f":"write","key":"k/1","op_id":1}
`

func writeHistory(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// A CI step points this at a glob of several hundred histories. Measured while
// sweeping the committed corpora: one file was momentarily unreadable, and
// because the first open error returned immediately, the reports already
// computed for 984 other files were discarded and the operator learned nothing
// about any of them. An unreadable path must stay loud without erasing the
// evidence that was gathered either side of it.
func TestVerifyPathsKeepsGoingPastAnUnreadablePath(t *testing.T) {
	dir := t.TempDir()
	good := writeHistory(t, dir, "good.jsonl", twoPairedOps)
	missing := filepath.Join(dir, "absent.jsonl")
	alsoGood := writeHistory(t, dir, "also-good.jsonl", twoPairedOps)

	reports, bad := verifyPaths(context.Background(), []string{good, missing, alsoGood})

	if len(reports) != 2 {
		t.Fatalf("got %d report(s), want 2: the readable files either side of the bad one "+
			"must still be reported", len(reports))
	}
	if len(bad) != 1 {
		t.Fatalf("got %d unreadable path(s), want 1", len(bad))
	}
	if bad[0].Path != missing {
		t.Errorf("unreadable path = %q, want %q", bad[0].Path, missing)
	}
	if bad[0].Err == nil {
		t.Error("an unreadable path must carry the error that explains why")
	}
}

// Still loud: a path that cannot be read is a usage error, and no count of
// clean files makes it a pass.
func TestHistoryVerifyFailsWhenAPathCannotBeRead(t *testing.T) {
	dir := t.TempDir()
	good := writeHistory(t, dir, "good.jsonl", twoPairedOps)
	missing := filepath.Join(dir, "absent.jsonl")

	g := globals{quiet: true}
	code := cmdHistory(context.Background(), g, []string{"verify", good, missing})
	if code != schema.ExitConfigError {
		t.Fatalf("exit %d, want %d (CONFIG_ERROR): an unreadable path is not a pass",
			code, schema.ExitConfigError)
	}
}

func TestHistoryVerifyPassesACleanFile(t *testing.T) {
	dir := t.TempDir()
	good := writeHistory(t, dir, "good.jsonl", twoPairedOps)

	g := globals{quiet: true}
	if code := cmdHistory(context.Background(), g, []string{"verify", good}); code != schema.ExitPass {
		t.Fatalf("exit %d, want %d (PASS)", code, schema.ExitPass)
	}
}

// The subcommand is required: `thesis history somefile.jsonl` must not be read
// as a verify, because a future subcommand would then change what it means.
func TestHistoryRequiresASubcommand(t *testing.T) {
	g := globals{quiet: true}
	if code := cmdHistory(context.Background(), g, nil); code != schema.ExitConfigError {
		t.Fatalf("exit %d, want %d for no arguments", code, schema.ExitConfigError)
	}
	if code := cmdHistory(context.Background(), g, []string{"history.jsonl"}); code != schema.ExitConfigError {
		t.Fatalf("exit %d, want %d for a missing subcommand", code, schema.ExitConfigError)
	}
	if code := cmdHistory(context.Background(), g, []string{"verify"}); code != schema.ExitConfigError {
		t.Fatalf("exit %d, want %d for verify with no path", code, schema.ExitConfigError)
	}
}

// Several files reduce to one code, and a witnessed breach outranks a history
// that merely stops early.
func TestHistoryVerifyReducesManyFilesToTheWorstCode(t *testing.T) {
	dir := t.TempDir()
	clean := writeHistory(t, dir, "clean.jsonl", twoPairedOps)
	stops := writeHistory(t, dir, "stops.jsonl",
		`{"t_ns":1789442762673427900,"process":0,"type":"invoke","f":"write","key":"k/1","op_id":1}`+"\n")
	broken := writeHistory(t, dir, "broken.jsonl",
		`{"t_ns":1789442762673427900,"process":0,"type":"ok","f":"write","key":"k/1","op_id":7}`+"\n")

	g := globals{quiet: true}
	if code := cmdHistory(context.Background(), g, []string{"verify", clean, stops}); code != schema.ExitInconclusive {
		t.Fatalf("exit %d, want %d (INCONCLUSIVE) for a clean file plus one that stops early",
			code, schema.ExitInconclusive)
	}
	if code := cmdHistory(context.Background(), g, []string{"verify", clean, stops, broken}); code != schema.ExitFail {
		t.Fatalf("exit %d, want %d (FAIL): a witnessed breach outranks an incomplete history",
			code, schema.ExitFail)
	}
}
