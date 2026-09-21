package driver

import (
	"os/exec"
	"time"
)

// ---------------------------------------------------------------------------
// Process-tree handling, exported for the Phase 3 external oracle runner
// ---------------------------------------------------------------------------
//
// An external oracle is the same shape of problem as a driver: a third-party
// executable that may spawn children, may ignore a polite signal, and must be
// gone before the harness moves on. Phase 3 reuses the mechanism this package
// already measured on both platforms rather than growing a second one, because
// a second one would be the copy that rots, and the symptom of a tree kill
// that misses a grandchild is an orphaned process holding a port open, which
// surfaces one world later as flakiness.
//
// These are THIN wrappers. The platform implementations stay in
// supervise_posix.go and supervise_windows.go and remain the single source of
// truth: SIGTERM to the process group, a grace window, then SIGKILL on POSIX;
// `taskkill /F /T` on Windows.

// SetProcessGroup prepares cmd so that TerminateTree can address the whole tree
// it spawns. It must be called BEFORE cmd.Start; calling it afterwards has no
// effect and TerminateTree would then reach only the direct child.
func SetProcessGroup(cmd *exec.Cmd) { setProcessGroup(cmd) }

// TerminateTree kills cmd and every process it spawned, giving the tree grace
// to exit on its own first where the platform supports it. A grace of zero
// means DefaultGrace.
//
// It is safe to call with a nil command, an unstarted command, or a process
// that has already exited, because every teardown path calls it and any of
// those states is reachable there.
func TerminateTree(cmd *exec.Cmd, grace time.Duration) error {
	return terminateTree(cmd, grace)
}

// ResolveProgram resolves a command's program name against a working
// directory, supplying a .exe suffix where the host needs one.
//
// It exists so a definition written portably (`./bin/checker`) runs on this
// Windows host without the config having to spell an extension that would then
// be wrong everywhere else.
func ResolveProgram(prog, dir string) string { return resolveProgram(prog, dir) }
