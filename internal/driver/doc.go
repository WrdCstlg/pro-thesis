// Package driver supervises the external workload generator named by
// `driver.cmd` in prothesis.yaml, and merges what it writes into the canonical
// history.
//
// # The driver is an external, unmodified process
//
// Everything in this package follows from that. PRO-THESIS hands the driver a
// command line and a couple of paths, and gets back a JSONL history and an exit
// status. It cannot assume the driver parses prothesis.yaml, understands the
// virtual clock, or cleans up after itself.
//
// # No host shell, ever
//
// `driver.cmd` is a template string, not a shell command. It is split here,
// respecting quotes, and exec'd directly.
//
// Running it through a shell was never an option on this host: Git Bash is
// broken on the build machine (`dofork ... 0xC0000142`), and cmd.exe's quoting
// rules differ from POSIX in ways that would silently change which arguments a
// driver receives. Direct exec also removes a whole class of injection: a
// history path containing a semicolon is an argument, not a second command.
//
// Splitting happens BEFORE substitution, which is the one ordering that
// survives a path with a space in it. The build machine's own project directory
// is `C:\AI Projects\Pro-synthesis`; substituting first and splitting after
// would turn `--history {history_path}` into three arguments and hand the driver
// `C:\AI` as its history file.
//
// # The driver writes op records; the harness writes phase markers
//
// PHASE0_BUILD_BRIEF D-D and DECISIONS.md D-011. Directive 4.4 shows phase
// markers interleaved with operation records in one stream, but 4.2 hands
// `{history_path}` to an external process. Two writers appending to one file
// across the Docker Desktop VM / Windows bind-mount boundary produce torn lines,
// and ONE torn line makes the entire history unparseable: surfacing as a
// spurious INCONCLUSIVE that reads like an environment flake rather than a bug.
//
// So there are two files and one merge:
//
//	{history_path}   op records, written by the driver
//	phases.jsonl     phase markers, written by the harness
//	history.jsonl    the merge, which oracle_input.history_path names
//
// MergeHistory moves RAW LINES ordered by t_ns. It decodes each line ONLY far
// enough to read t_ns for ordering, and writes the original bytes back out. It
// never decodes-then-re-encodes, because schema.HistoryEntry does not model
// everything a driver may emit: the kv fixture attaches a namespaced `meta`
// object to every operation carrying node, term, commit_index, read_mode,
// served_by and latency_us, and a decode/re-encode round trip would silently
// delete all of it; destroying exactly the evidence Phase 3's stale-read
// attribution is built on.
//
// # Killing the tree, not the child
//
// A driver spawns clients. If the supervisor kills only its direct child, an
// orphaned loadgen keeps its connections open, keeps writing to the history
// file, and corrupts the NEXT world: a failure that shows up one world later
// than its cause and looks like flakiness. Cancel and timeout therefore kill the
// whole process tree: `taskkill /F /T` on Windows, a signal to the process group
// on Unix.
//
// # Scope
//
// Phase 1. This package starts, supervises, stops and merges. It schedules no
// faults (Phase 2) and reduces no operation traces (Phase 5), though it does
// write the plan file that Phase 5's reduced trace will later travel in, because
// that same file is how a resolved driver profile reaches the driver today
// (OPEN_QUESTIONS.md OQ-012).
package driver
