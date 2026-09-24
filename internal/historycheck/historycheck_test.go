package historycheck

import (
	"strings"
	"testing"

	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// A history that satisfies the contract, used as the base every negative case
// mutates by exactly one line. Two paired operations on two processes, wrapped
// in the phase markers the harness writes.
const goodHistory = `{"t_ns":1789442762673427900,"type":"info","event":"phase","phase":"DRIVE"}
{"t_ns":1789442762673427901,"process":0,"type":"invoke","f":"write","key":"k/1","value":7,"op_id":1}
{"t_ns":1789442762673427902,"process":1,"type":"invoke","f":"read","key":"k/1","op_id":2}
{"t_ns":1789442762673427903,"process":0,"type":"ok","f":"write","key":"k/1","value":7,"op_id":1}
{"t_ns":1789442762673427904,"process":1,"type":"info","f":"read","key":"k/1","op_id":2,"error":"timeout"}
{"t_ns":1789442762673427905,"type":"info","event":"phase","phase":"HEAL"}
`

func check(t *testing.T, body string) *Report {
	t.Helper()
	rep, err := Check(strings.NewReader(body), "history.jsonl")
	if err != nil {
		t.Fatalf("Check returned a transport error on readable input: %v", err)
	}
	return rep
}

// codes returns the finding codes in report order, so a test can assert the
// exact set rather than "at least one finding", which would pass for the wrong
// reason.
func codes(rep *Report) []string {
	out := make([]string, 0, len(rep.Findings))
	for _, f := range rep.Findings {
		out = append(out, f.Code)
	}
	return out
}

func wantCodes(t *testing.T, rep *Report, want ...string) {
	t.Helper()
	got := codes(rep)
	if len(got) != len(want) {
		t.Fatalf("finding codes = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("finding codes = %v, want %v", got, want)
		}
	}
}

func TestVerifyAcceptsAWellFormedHistory(t *testing.T) {
	rep := check(t, goodHistory)
	if len(rep.Findings) != 0 {
		t.Fatalf("a conforming history produced findings: %v", rep.Findings)
	}
	if got := rep.ExitCode(); got != schema.ExitPass {
		t.Fatalf("exit code = %d, want %d (PASS)", got, schema.ExitPass)
	}
}

// The counts are the refusal surface: they are what lets a reader tell a
// history that was checked and found clean from one that was never looked at.
func TestReportCountsAreMeasuredNotAssumed(t *testing.T) {
	rep := check(t, goodHistory)
	if rep.Lines != 6 {
		t.Errorf("Lines = %d, want 6", rep.Lines)
	}
	if rep.Operations != 2 {
		t.Errorf("Operations = %d, want 2 (paired invokes count once)", rep.Operations)
	}
	if rep.Records != 4 {
		t.Errorf("Records = %d, want 4 operation records", rep.Records)
	}
	if rep.Markers != 2 {
		t.Errorf("Markers = %d, want 2", rep.Markers)
	}
	if rep.Indeterminate != 1 {
		t.Errorf("Indeterminate = %d, want 1 (the info completion)", rep.Indeterminate)
	}
	if rep.Processes != 2 {
		t.Errorf("Processes = %d, want 2", rep.Processes)
	}
	if rep.Keys != 1 {
		t.Errorf("Keys = %d, want 1", rep.Keys)
	}
	if rep.FirstTNS != 1789442762673427900 || rep.LastTNS != 1789442762673427905 {
		t.Errorf("span = [%d,%d], want [1789442762673427900,1789442762673427905]", rep.FirstTNS, rep.LastTNS)
	}
	want := []TypeCount{
		{Type: "info", N: 3}, // one indeterminate completion plus two markers
		{Type: "invoke", N: 2},
		{Type: "ok", N: 1},
	}
	if len(rep.Types) != len(want) {
		t.Fatalf("Types = %v, want %v", rep.Types, want)
	}
	for i := range want {
		if rep.Types[i] != want[i] {
			t.Fatalf("Types = %v, want %v (sorted by type name for determinism)", rep.Types, want)
		}
	}
}

func TestVerifyRejectsAnOperationWithoutAnOpID(t *testing.T) {
	body := strings.Replace(goodHistory,
		`"key":"k/1","value":7,"op_id":1}`+"\n"+`{"t_ns":1789442762673427902`,
		`"key":"k/1","value":7}`+"\n"+`{"t_ns":1789442762673427902`, 1)
	rep := check(t, body)
	wantCodes(t, rep, CodeNoOpID, CodeCompletionWithoutInvoke)
	if got := rep.ExitCode(); got != schema.ExitFail {
		t.Fatalf("exit code = %d, want %d (FAIL)", got, schema.ExitFail)
	}
}

func TestVerifyRejectsTwoInvokesForOneOpID(t *testing.T) {
	body := goodHistory +
		`{"t_ns":1789442762673427906,"process":0,"type":"invoke","f":"write","key":"k/2","op_id":1}` + "\n"
	rep := check(t, body)
	// One bad line, one finding: the second invoke is reported and not tracked,
	// so it does not also surface as an operation left open at EOF.
	wantCodes(t, rep, CodeDuplicateInvoke)
	if rep.Findings[0].Line != 7 {
		t.Errorf("finding line = %d, want 7 (the second invoke, not the first)", rep.Findings[0].Line)
	}
	if rep.Findings[0].OpID == nil || *rep.Findings[0].OpID != 1 {
		t.Errorf("finding must name the op_id it is about, got %v", rep.Findings[0].OpID)
	}
}

func TestVerifyRejectsTwoCompletionsForOneOpID(t *testing.T) {
	body := goodHistory +
		`{"t_ns":1789442762673427906,"process":0,"type":"ok","f":"write","key":"k/1","op_id":1}` + "\n"
	rep := check(t, body)
	wantCodes(t, rep, CodeDuplicateCompletion)
}

func TestVerifyRejectsACompletionWithNoInvoke(t *testing.T) {
	body := goodHistory +
		`{"t_ns":1789442762673427906,"process":0,"type":"ok","f":"write","key":"k/9","op_id":99}` + "\n"
	rep := check(t, body)
	wantCodes(t, rep, CodeCompletionWithoutInvoke)
}

// An invoke still open at EOF is what a driver that ignored the drain leaves
// behind, and it is the shape `no_stuck_op` refuses to judge.
//
// It is INCONCLUSIVE and not FAIL, because the history stops before the answer
// rather than proving one. Measured over the two committed corpora: of the 61
// recorded histories carrying an unclosed operation, 50 belong to a world that
// never wrote a result.json and none belongs to a world judged pass.
func TestVerifyReportsAnInvokeLeftOpenAsInconclusive(t *testing.T) {
	body := goodHistory +
		`{"t_ns":1789442762673427906,"process":0,"type":"invoke","f":"write","key":"k/3","op_id":3}` + "\n"
	rep := check(t, body)
	wantCodes(t, rep, CodeUnclosedInvoke)
	if rep.Unclosed != 1 {
		t.Errorf("Unclosed = %d, want 1", rep.Unclosed)
	}
	if rep.Findings[0].OpID == nil || *rep.Findings[0].OpID != 3 {
		t.Errorf("the finding must name the op that was left open, got %v", rep.Findings[0].OpID)
	}
	if got := rep.ExitCode(); got != schema.ExitInconclusive {
		t.Fatalf("exit code = %d, want %d (INCONCLUSIVE): an incomplete history is a refusal, "+
			"not a verdict", got, schema.ExitInconclusive)
	}
	if rep.Violations() != 0 {
		t.Fatalf("Violations = %d, want 0: an unclosed operation proves nothing about the file",
			rep.Violations())
	}
}

// A proven breach still outranks an incomplete history, so a file with both
// reports FAIL and keeps both findings.
func TestAWitnessedBreachOutranksAnIncompleteHistory(t *testing.T) {
	body := goodHistory +
		`{"t_ns":1789442762673427906,"process":0,"type":"invoke","f":"write","key":"k/3","op_id":3}` + "\n" +
		`{"t_ns":1789442762673427907,"process":0,"type":"ok","f":"write","key":"k/9","op_id":99}` + "\n"
	rep := check(t, body)
	wantCodes(t, rep, CodeUnclosedInvoke, CodeCompletionWithoutInvoke)
	if got := rep.ExitCode(); got != schema.ExitFail {
		t.Fatalf("exit code = %d, want %d (FAIL)", got, schema.ExitFail)
	}
}

// The union rule: a record carrying both an event and an op_id is an operation
// that every marker-skipping reader silently drops (OQ-058).
func TestVerifyRejectsAMarkerCarryingAnOpID(t *testing.T) {
	body := goodHistory +
		`{"t_ns":1789442762673427906,"type":"info","event":"phase","phase":"QUIESCE","op_id":4}` + "\n"
	rep := check(t, body)
	wantCodes(t, rep, CodeInvalidRecord)
}

// The field name is normative and says nanoseconds. The directive's own
// example values were microseconds, so this is the mistake an integrator is
// most likely to inherit from the spec.
func TestVerifyRejectsATimestampThatIsNotEpochNanoseconds(t *testing.T) {
	body := `{"t_ns":1725300000000000,"process":0,"type":"invoke","f":"write","key":"k/1","op_id":1}
{"t_ns":1725300000000001,"process":0,"type":"ok","f":"write","key":"k/1","op_id":1}
`
	rep := check(t, body)
	wantCodes(t, rep, CodeTimestampNotNanoseconds, CodeTimestampNotNanoseconds)
	if got := rep.ExitCode(); got != schema.ExitFail {
		t.Fatalf("exit code = %d, want %d (FAIL)", got, schema.ExitFail)
	}
}

// An operation whose completion precedes its invoke has a negative interval,
// which no linearization can place.
func TestVerifyRejectsACompletionEarlierThanItsInvoke(t *testing.T) {
	body := `{"t_ns":1789442762673427905,"process":0,"type":"invoke","f":"write","key":"k/1","op_id":1}
{"t_ns":1789442762673427900,"process":0,"type":"ok","f":"write","key":"k/1","op_id":1}
`
	rep := check(t, body)
	wantCodes(t, rep, CodeNegativeInterval)
}

// A checker handed nothing has checked nothing: that is INCONCLUSIVE, never a
// pass.
func TestVerifyReportsEmptyHistoryAsInconclusive(t *testing.T) {
	rep := check(t, `{"t_ns":1789442762673427900,"type":"info","event":"phase","phase":"DRIVE"}`+"\n")
	wantCodes(t, rep, CodeNoOperations)
	if got := rep.ExitCode(); got != schema.ExitInconclusive {
		t.Fatalf("exit code = %d, want %d (INCONCLUSIVE)", got, schema.ExitInconclusive)
	}
}

func TestVerifyReportsATotallyEmptyFileAsInconclusive(t *testing.T) {
	rep := check(t, "")
	wantCodes(t, rep, CodeNoOperations)
	if got := rep.ExitCode(); got != schema.ExitInconclusive {
		t.Fatalf("exit code = %d, want %d (INCONCLUSIVE)", got, schema.ExitInconclusive)
	}
}

func TestVerifyRejectsMalformedJSON(t *testing.T) {
	body := goodHistory + "{not json\n"
	rep := check(t, body)
	wantCodes(t, rep, CodeMalformedJSON)
	if rep.Findings[0].Line != 7 {
		t.Errorf("finding line = %d, want 7", rep.Findings[0].Line)
	}
}

// One malformed line must not abort the read: the reader is explicitly built to
// survive it, and a validator that stopped at the first bad line would report
// one defect in a file with fifty.
func TestVerifyKeepsGoingAfterTheFirstMalformedLine(t *testing.T) {
	body := "{not json\n" + goodHistory + "{also not json\n"
	rep := check(t, body)
	wantCodes(t, rep, CodeMalformedJSON, CodeMalformedJSON)
	if rep.Findings[0].Line == rep.Findings[1].Line {
		t.Fatalf("both findings report line %d; the second bad line was not reached",
			rep.Findings[0].Line)
	}
	if rep.Records != 4 {
		t.Errorf("Records = %d, want 4: the good records between the bad lines still count",
			rep.Records)
	}
}

// The checker pairs by op_id and does not require this, so it is a warning. It
// is reported because a process is a logical client: two operations in flight
// on one means the history describes more concurrency than its process count
// admits, which is a modelling error in the driver.
func TestVerifyWarnsWhenOneProcessHasTwoOperationsInFlight(t *testing.T) {
	body := `{"t_ns":1789442762673427900,"process":0,"type":"invoke","f":"write","key":"k/1","op_id":1}
{"t_ns":1789442762673427901,"process":0,"type":"invoke","f":"write","key":"k/2","op_id":2}
{"t_ns":1789442762673427902,"process":0,"type":"ok","f":"write","key":"k/1","op_id":1}
{"t_ns":1789442762673427903,"process":0,"type":"ok","f":"write","key":"k/2","op_id":2}
`
	rep := check(t, body)
	wantCodes(t, rep, CodeProcessConcurrent)
	if rep.Findings[0].Severity != SeverityWarning {
		t.Fatalf("severity = %q, want %q", rep.Findings[0].Severity, SeverityWarning)
	}
	if got := rep.ExitCode(); got != schema.ExitPass {
		t.Fatalf("exit code = %d, want %d: a warning must not fail the file", got, schema.ExitPass)
	}
	if rep.Warnings() != 1 || rep.Violations() != 0 {
		t.Fatalf("warnings = %d, violations = %d, want 1 and 0", rep.Warnings(), rep.Violations())
	}
}

// Findings are reported in line order regardless of which check produced them,
// so two runs over the same file print the same thing.
func TestFindingsAreOrderedByLine(t *testing.T) {
	body := goodHistory +
		`{"t_ns":1789442762673427906,"process":0,"type":"ok","f":"write","key":"k/9","op_id":99}` + "\n" +
		"{not json\n"
	rep := check(t, body)
	wantCodes(t, rep, CodeCompletionWithoutInvoke, CodeMalformedJSON)
	for i := 1; i < len(rep.Findings); i++ {
		if rep.Findings[i-1].Line > rep.Findings[i].Line {
			t.Fatalf("findings are not in line order: %v", rep.Findings)
		}
	}
}
