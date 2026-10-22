package engine

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// A recorded leak never goes away: the run's search record keeps its network
// counts, and recorded artifacts are not edited. Without a way to say "this
// leak was swept", the acceptance leak check stays red on a corpus that holds
// one forever (OQ-044), whether or not the leaked project still exists. A run
// may therefore be acknowledged as swept, but only in a committed list that
// carries the evidence, and only while Docker confirms, at check time, that
// nothing of the run's projects is left.

func fiveBC6() recordedLeak {
	return recordedLeak{Run: "r_2026_09_08_5bc6", Before: 24, After: 22,
		Projects: []string{"thesis-r_2026_09_08_5bc6-w00", "thesis-r_2026_09_08_5bc6-w02"}}
}

func fiveBC6Ack() sweptLeak {
	return sweptLeak{Run: "r_2026_09_08_5bc6", LeakedNetworks: 2, ConfirmedAbsent: "2026-10-21",
		Evidence: "no container or network of the run's projects listed", Ledger: "OQ-044"}
}

func TestASweptLeakIsHonouredOnlyWhileItsProjectIsGone(t *testing.T) {
	var asked [][]string
	gone := func(p []string) ([]string, error) { asked = append(asked, p); return nil, nil }
	present := func([]string) ([]string, error) {
		return []string{"network thesis-r_2026_09_08_5bc6-w02_default"}, nil
	}
	unreachable := func([]string) ([]string, error) { return nil, errors.New("docker daemon is not reachable") }
	acked := map[string]sweptLeak{"r_2026_09_08_5bc6": fiveBC6Ack()}
	miscounted := fiveBC6Ack()
	miscounted.LeakedNetworks = 1

	cases := []struct {
		name     string
		leaks    []recordedLeak
		acks     map[string]sweptLeak
		probe    residueProbe
		wantFail string // "" means honoured, no failure
	}{
		{"a leak nobody acknowledged", []recordedLeak{fiveBC6()}, nil, gone, "2 leaked"},
		{"acknowledged and confirmed gone", []recordedLeak{fiveBC6()}, acked, gone, ""},
		{"acknowledged but still present", []recordedLeak{fiveBC6()}, acked, present, "still holds"},
		{"acknowledged, daemon cannot be asked", []recordedLeak{fiveBC6()}, acked, unreachable, "cannot confirm"},
		{"acknowledged with a different count", []recordedLeak{fiveBC6()},
			map[string]sweptLeak{"r_2026_09_08_5bc6": miscounted}, gone, "acknowledges 1"},
		{"an acknowledgement that matches no recorded leak", nil, acked, gone, "matches no recorded leak"},
		{"a leak that records no project", []recordedLeak{{Run: "r_2026_09_08_5bc6", Before: 24, After: 22}},
			acked, gone, "records no compose project"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			asked = nil
			failures, honoured := judgeLeaks(c.leaks, c.acks, c.probe)
			if c.wantFail == "" {
				if len(failures) != 0 || len(honoured) != 1 {
					t.Fatalf("failures %q, honoured %q; want the acknowledgement honoured", failures, honoured)
				}
				if want := [][]string{fiveBC6().Projects}; !reflect.DeepEqual(asked, want) {
					t.Fatalf("the probe was asked about %v, want the run's recorded projects %v", asked, want)
				}
				return
			}
			if len(failures) != 1 || !strings.Contains(failures[0], c.wantFail) || len(honoured) != 0 {
				t.Fatalf("failures %q, honoured %q; want one failure mentioning %q", failures, honoured, c.wantFail)
			}
		})
	}
}

func TestTheSweptLeakListIsStrict(t *testing.T) {
	write := func(s string) string {
		t.Helper()
		p := filepath.Join(t.TempDir(), "swept-leaks.yaml")
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	good := `runs:
  - run: r_2026_09_08_5bc6
    leaked_networks: 2
    confirmed_absent: "2026-10-21"
    evidence: no container or network of the run's projects listed
    ledger: OQ-044
`
	acks, err := loadSweptLeaks(write(good))
	if err != nil || !reflect.DeepEqual(acks, map[string]sweptLeak{"r_2026_09_08_5bc6": fiveBC6Ack()}) {
		t.Fatalf("a valid list: %+v, err %v", acks, err)
	}

	for name, doc := range map[string]string{
		"an unknown key":       strings.Replace(good, "ledger:", "ledgr:", 1),
		"no evidence":          strings.Replace(good, "evidence: no container or network of the run's projects listed", "evidence: \"\"", 1),
		"no confirmation date": strings.Replace(good, `confirmed_absent: "2026-10-21"`, `confirmed_absent: "last week"`, 1),
		"no leaked count":      strings.Replace(good, "leaked_networks: 2", "leaked_networks: 0", 1),
		"a malformed run id":   strings.Replace(good, "run: r_2026_09_08_5bc6", "run: 5bc6", 1),
		"a malformed ledger":   strings.Replace(good, "ledger: OQ-044", "ledger: see the ledger", 1),
		"the same run twice":   good + strings.TrimPrefix(good, "runs:\n"),
	} {
		if _, err := loadSweptLeaks(write(doc)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}

	// The committed list itself loads, and a missing list is no list.
	if _, err := loadSweptLeaks(sweptLeaksPath(t)); err != nil {
		t.Fatalf("the committed list: %v", err)
	}
	if acks, err := loadSweptLeaks(filepath.Join(t.TempDir(), "absent.yaml")); err != nil || len(acks) != 0 {
		t.Fatalf("a missing list: %+v, err %v; want an empty one", acks, err)
	}
}
