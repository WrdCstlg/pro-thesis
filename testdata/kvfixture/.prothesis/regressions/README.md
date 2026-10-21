# Regression corpus

Every `w_xxxx.thesis` file here is a world that PRO-THESIS shrank from a real
violation and then CONFIRMED reproduces. `thesis regress` replays all of them on
every gate invocation.

**This directory is append-only, and it is committed on purpose.**
`.gitignore` excludes `.prothesis/runs/` but deliberately does not exclude this
directory (D-004). Removing a `.thesis` file from it is a LOCK CHANGE
under invariant I6 and anti-gaming rule 4: it deletes a regression test. If a
regression is genuinely obsolete, that is a human-reviewed commit that says so,
not a cleanup.

Two rules the tooling enforces so you do not have to:

- a world that did not reproduce k/k is REFUSED entry, because
  `thesis regress` reports a corpus world that does not reproduce as
  NON_REPRODUCING (exit 2), and a flaky entry turns every gate run it misses
  into an unjudged one at exactly its flake rate;
- a DIFFERENT world wanting a filename already taken is an error, never a silent
  overwrite. Filenames are the first four hex digits of the world hash, so a
  collision is possible; losing a regression to one is not.

A world that names its workload (`plan_digest`) is stored with that workload
beside it as `w_xxxx.plan.json`, and `thesis regress` replays that exact
operation sequence. The plan's digest must match the world's, so a plan file
cannot be swapped without the world refusing it.

The files are LF-only and byte-identical to their canonical encoding. Do not
hand-edit them: the loader re-encodes and byte-compares on every read, so an
edited world is rejected rather than silently re-hashed.
