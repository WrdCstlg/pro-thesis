package schema

import (
	"strings"
	"testing"
)

// `min_worlds` is the open-ended profile's completion requirement: a run that
// stops at its wall budget with at least that many clean worlds finished is a
// PASS. It means nothing for a finite profile, whose planned count already is
// the requirement, and a minimum below one would let a run that judged nothing
// pass.
func TestMinWorldsIsValidatedAgainstTheWorldCount(t *testing.T) {
	cases := []struct {
		name    string
		profile string
		wantErr string
	}{
		{"open-ended with a minimum", "{ budget: 1h, driver_profile: x, worlds: -1, min_worlds: 3 }", ""},
		{"worlds omitted is open-ended", "{ budget: 1h, driver_profile: x, min_worlds: 2 }", ""},
		{"zero", "{ budget: 1h, driver_profile: x, worlds: -1, min_worlds: 0 }", "min_worlds"},
		{"negative", "{ budget: 1h, driver_profile: x, worlds: -1, min_worlds: -2 }", "min_worlds"},
		{"finite profile", "{ budget: 1h, driver_profile: x, worlds: 5, min_worlds: 2 }", "min_worlds"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := DecodeConfig([]byte("profiles:\n  a: " + tc.profile + "\n"))
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			err = cfg.ValidateProfiles()
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("rejected a valid profile: %v", err)
			case tc.wantErr != "" && err == nil:
				t.Fatal("accepted an invalid min_worlds")
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Fatalf("the error does not name %q: %v", tc.wantErr, err)
			}
		})
	}
}
