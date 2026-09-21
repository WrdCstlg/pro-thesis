// Package corpus is the committed regression corpus: `.prothesis/regressions/`.
//
// It is the smallest package in Phase 5 and the one with the sharpest rules,
// because every file it holds is a LOCK-ADJACENT COMMITTED ARTIFACT. D-004 keeps
// `.prothesis/regressions/` out of `.gitignore` deliberately: the corpus is
// specified as append-only, and removing a `.thesis` regression file is a lock
// change (invariant I6, anti-gaming rule 4).
//
// # Four rules, and the failure each one closes
//
// APPEND-ONLY. There is no Remove, no Replace and no Prune in this API, and that
// absence is the enforcement. A world is added under its own content address; a
// DIFFERENT world wanting the same four-hex filename is an error, never a silent
// overwrite, because overwriting one regression with another is functionally the
// deletion of a regression test. Adding the identical world twice is a no-op, so
// callers may be re-run freely.
//
// A WORLD THAT DID NOT REPRODUCE IS REFUSED. Add takes the confirmation gate's
// measured result and requires k/k. A world that never reproduced is dead weight
// that costs ~30s of every future gate run and proves nothing; a world that
// reproduced 2/3 is worse, because `thesis regress` reports PASS when a corpus
// world does not reproduce, so a flaky entry manufactures a FALSE GREEN one run
// in three. Neither is a corpus this project can stand behind, and the honest
// remedies (shrink further, or confirm at a k you are willing to publish) are
// both available and both visible.
//
// BYTE-IDENTICAL AND LF-ONLY. Writes go through internal/recorder, whose codec
// re-encodes and byte-compares on every Load (D-012), so canonicality is a
// runtime invariant here rather than a test assertion. `.gitattributes` already
// carries `*.thesis text eol=lf`; a CR byte would change every world_hash on a
// Windows checkout, and the decoder names that case specifically.
//
// DETERMINISTIC ORDER. List sorts by filename, which is the world's own content
// address. `thesis regress` runs on every gate invocation forever, so the order
// in which it spends a budget must not depend on a directory read.
//
// # What this package deliberately does not do
//
// It does not shrink, does not replay, and does not decide whether a world
// reproduces. It is handed a world and a Confirmation and it enforces the gate.
// internal/shrink produces the first, internal/replay measures the second.
package corpus
