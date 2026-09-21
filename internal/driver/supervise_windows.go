//go:build windows

package driver

import (
	"fmt"
	"os/exec"
	"strconv"
	"syscall"
	"time"
)

func setProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= syscall.CREATE_NEW_PROCESS_GROUP
}

func terminateTree(cmd *exec.Cmd, _ time.Duration) error {
	if cmd == nil || cmd.Process == nil || cmd.Process.Pid <= 0 {
		return nil
	}
	pid := cmd.Process.Pid
	// On Windows, taskkill /F /T /PID kills the process and all of its child processes.
	killCmd := exec.Command("taskkill", "/F", "/T", "/PID", strconv.Itoa(pid))
	if out, err := killCmd.CombinedOutput(); err != nil {
		if cmd.ProcessState != nil && cmd.ProcessState.Exited() {
			return nil
		}
		// Also try Process.Kill as a fallback
		_ = cmd.Process.Kill()
		return fmt.Errorf("taskkill /PID %d: %w: %s", pid, err, string(out))
	}
	return nil
}

func signalOf(_ error) (bool, string) {
	// Windows does not have POSIX signals.
	return false, ""
}
