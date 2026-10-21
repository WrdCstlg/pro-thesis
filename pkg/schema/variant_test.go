package schema

import (
	"strings"
	"testing"
)

// A profile's `variant` is the value harness.variant_env is set to. Without a
// variant_env nothing would carry it to the build, and the world would record a
// variant that did not run.
func TestAVariantIsValidatedAgainstTheHarness(t *testing.T) {
	cases := []struct {
		name, harness, profile, wantErr string
	}{
		{"pinned", "harness:\n  variant_env: KV_VARIANT\n", "{ budget: 1m, driver_profile: x, variant: kvfixed }", ""},
		{"no variant_env", "", "{ budget: 1m, driver_profile: x, variant: kvfixed }", "variant_env"},
		{"not a token", "harness:\n  variant_env: KV_VARIANT\n", "{ budget: 1m, driver_profile: x, variant: \"kv fixed\" }", "variant"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := DecodeConfig([]byte(tc.harness + "profiles:\n  a: " + tc.profile + "\n"))
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			err = cfg.ValidateProfiles()
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("rejected a valid variant: %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("want an error naming %q, got %v", tc.wantErr, err)
			}
		})
	}
}
