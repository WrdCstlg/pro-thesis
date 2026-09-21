package schema

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestPointerHelpersKeepZeroDistinguishable is the point of the helpers: a
// pointer to zero must emit the field, and an absent field must not.
func TestPointerHelpersKeepZeroDistinguishable(t *testing.T) {
	e := HistoryEntry{
		TNS: 1, Type: HistoryOK, F: "read",
		Process: Int64(0), OpID: Int64(0), Key: Str(""),
		Value: json.RawMessage("0"),
	}
	line, err := e.MarshalLine()
	if err != nil {
		t.Fatalf("MarshalLine: %v", err)
	}
	for _, want := range []string{`"process":0`, `"op_id":0`, `"key":""`, `"value":0`} {
		if !strings.Contains(string(line), want) {
			t.Errorf("missing %s in %s", want, line)
		}
	}

	p := Profile{Budget: Duration(1), DriverProfile: "x", Worlds: Worlds(WorldsUnbounded)}
	if p.Worlds == nil || *p.Worlds != -1 {
		t.Errorf("Worlds(WorldsUnbounded) = %v", p.Worlds)
	}
	if *Int(7) != 7 || *Uint64(9) != 9 {
		t.Error("Int/Uint64 helpers are wrong")
	}
}
