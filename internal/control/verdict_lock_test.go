package control

import (
	"testing"

	"github.com/WrdCstlg/pro-thesis/pkg/schema"
)

// TestVerdictCarriesTheLockHonestly pins both halves of the oracle_lock
// contract: a caller's real drift check reaches the verdict verbatim, and a
// caller that made none gets "absent" rather than a manufactured "ok".
func TestVerdictCarriesTheLockHonestly(t *testing.T) {
	t.Run("checked", func(t *testing.T) {
		v := BuildVerdict(VerdictInput{
			RunID:   "r_test",
			Profile: "smoke",
			Outcome: OutcomePass,
			OracleLock: schema.OracleLock{
				Status:      schema.LockOK,
				ManifestSHA: "sha256:" + "ab",
			},
		})
		if v.OracleLock.Status != schema.LockOK {
			t.Fatalf("oracle_lock.status = %q, want ok", v.OracleLock.Status)
		}
		if v.OracleLock.ManifestSHA != "sha256:ab" {
			t.Fatalf("oracle_lock.manifest_sha = %q", v.OracleLock.ManifestSHA)
		}
	})

	t.Run("unchecked_is_absent_not_ok", func(t *testing.T) {
		v := BuildVerdict(VerdictInput{RunID: "r_test", Profile: "smoke", Outcome: OutcomePass})
		if v.OracleLock.Status != schema.LockAbsent {
			t.Fatalf("a caller that made no drift check produced oracle_lock.status = %q; "+
				"it must be %q. Anything else lets an agent clear the drift check by deleting "+
				"one file.", v.OracleLock.Status, schema.LockAbsent)
		}
		if v.OracleLock.ManifestSHA != "" {
			t.Fatalf("an absent lock reported manifest_sha %q; there is no manifest to report",
				v.OracleLock.ManifestSHA)
		}
	})

	t.Run("mismatch_survives_normalization", func(t *testing.T) {
		v := BuildVerdict(VerdictInput{
			RunID: "r_test", Profile: "smoke", Outcome: OutcomePass,
			OracleLock: schema.OracleLock{Status: schema.LockMismatch, ManifestSHA: "sha256:cd"},
		})
		if v.OracleLock.Status != schema.LockMismatch {
			t.Fatalf("a mismatch was normalized away to %q", v.OracleLock.Status)
		}
	})
}
